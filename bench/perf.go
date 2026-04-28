/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package bench

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	watchapi "k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// HCP API server is exposed on the worker NodePort range. We pin APIPort
// to 30443 in the Cluster spec so every HCP is reachable on the same port.
const hcpAPINodePort = 30443

const (
	watchLagAnnotation = "bench.k0smotron.io/write-ns"
	watchRunIDLabel    = "bench.k0smotron.io/run-id"
)

type loadResult struct {
	Durations []time.Duration
	Successes int
	Errors    int
	FirstErr  error
}

type resultWithError struct {
	dur time.Duration
	err error
}

type watchChurnResult struct {
	Churn           loadResult
	ChurnByOp       map[string]opStat // create / update1 / update2 / delete
	Watchers        int
	WatchEvents     int
	WatchErrors     int
	WatchReconnects int
	WatchFirstErr   string
	WatchLags       []time.Duration
}

// opStat aggregates per-operation success/error counts and a sample of the
// first error seen — used by watch-churn to localise which step of the
// create/update/update/delete lifecycle is failing.
type opStat struct {
	Successes int
	Errors    int
	FirstErr  string
}

// churnLifecycleOps lists the lifecycle operations in order. The slice order
// is also the report order so changing it changes log/CSV ordering.
var churnLifecycleOps = []string{"create", "update1", "update2", "delete"}

// lifecycleErr tags a returned error with the lifecycle step that produced it.
type lifecycleErr struct {
	op  string
	err error
}

func (e *lifecycleErr) Error() string {
	if e == nil || e.err == nil {
		return ""
	}
	return e.op + ": " + e.err.Error()
}

func (e *lifecycleErr) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

// pickHCPNodeAddress returns a node address reachable from the test runner.
// Terraform knows the EC2 public addresses even when Kubernetes node status
// only reports private InternalIP addresses.
func pickHCPNodeAddress(ctx context.Context, kc *kubernetes.Clientset) (string, error) {
	addrs, err := hcpNodeAddresses(ctx, kc)
	if err != nil {
		return "", err
	}
	return addrs[0], nil
}

func hcpNodeAddresses(ctx context.Context, kc *kubernetes.Clientset) ([]string, error) {
	if addrs := csvValues(os.Getenv("BENCH_WORKER_EXTERNAL_ADDRESSES")); len(addrs) > 0 {
		return addrs, nil
	}

	nodes, err := kc.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	var externalAddrs, internalIPs []string
	for _, n := range nodes.Items {
		if !isNodeReady(n) {
			continue
		}
		for _, addr := range n.Status.Addresses {
			switch addr.Type {
			case corev1.NodeExternalIP, corev1.NodeExternalDNS:
				externalAddrs = append(externalAddrs, addr.Address)
			case corev1.NodeInternalIP:
				internalIPs = append(internalIPs, addr.Address)
			}
		}
	}
	if externalAddrs = uniqueNonEmpty(externalAddrs); len(externalAddrs) > 0 {
		return externalAddrs, nil
	}
	if internalIPs = uniqueNonEmpty(internalIPs); len(internalIPs) > 0 {
		return internalIPs, nil
	}
	return nil, fmt.Errorf("no Ready node with external/internal address found")
}

func csvValues(raw string) []string {
	var values []string
	for _, part := range strings.Split(raw, ",") {
		if value := strings.TrimSpace(part); value != "" {
			values = append(values, value)
		}
	}
	return uniqueNonEmpty(values)
}

func uniqueNonEmpty(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	var out []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func isNodeReady(n corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// buildHCPClient constructs a kubernetes client for the HCP apiserver that
// talks directly to a worker node's NodePort (no port-forward). The perf HCP
// adds the worker addresses to spec.api.sans, so the kubeconfig CA remains
// valid after replacing the host. QPS/Burst are cranked up so the test — not
// the client — is the load bottleneck.
func buildHCPClient(ctx context.Context, mgmtKC *kubernetes.Clientset, clusterName, ns, nodeAddr string, qps float32, burst int) (*kubernetes.Clientset, error) {
	secretName := fmt.Sprintf("%s-kubeconfig", clusterName)
	var kubeconfigBytes []byte
	deadline := time.Now().Add(2 * time.Minute)
	for {
		sec, err := mgmtKC.CoreV1().Secrets(ns).Get(ctx, secretName, metav1.GetOptions{})
		if err == nil {
			kubeconfigBytes = sec.Data["value"]
			if len(kubeconfigBytes) > 0 {
				break
			}
		} else if !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("get kubeconfig secret: %w", err)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for kubeconfig secret %s/%s", ns, secretName)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}

	rc, err := clientcmd.RESTConfigFromKubeConfig(kubeconfigBytes)
	if err != nil {
		return nil, fmt.Errorf("parse HCP kubeconfig: %w", err)
	}

	rc.Host = fmt.Sprintf("https://%s:%d", nodeAddr, hcpAPINodePort)
	rc.QPS = qps
	rc.Burst = burst

	return kubernetes.NewForConfig(rc)
}

// waitHCPReachable polls /readyz on the HCP apiserver until it responds,
// so load measurements don't include first-hit cold start.
func waitHCPReachable(ctx context.Context, hcpKC *kubernetes.Clientset) error {
	deadline := time.Now().Add(3 * time.Minute)
	for {
		_, err := hcpKC.Discovery().ServerVersion()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("HCP apiserver not reachable via NodePort: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// runWriteLoad creates (warmup + ops) ConfigMaps with the given concurrency.
// Warmup operations are discarded. Returns per-create durations for the
// measured ops. Created objects are deleted asynchronously.
func runWriteLoad(ctx context.Context, hcpKC *kubernetes.Clientset, namespace string, ops, concurrency, warmup int, requestTimeout time.Duration) loadResult {
	if err := ensureHCPNamespace(ctx, hcpKC, namespace); err != nil {
		return loadResult{Errors: ops, FirstErr: err}
	}

	total := warmup + ops
	results := make([]resultWithError, total)
	sem := make(chan struct{}, concurrency)

	eg, egCtx := errgroup.WithContext(ctx)
	for i := range total {
		i := i
		eg.Go(func() error {
			sem <- struct{}{}
			defer func() { <-sem }()

			name := fmt.Sprintf("load-%06d", i)
			reqCtx, cancel := context.WithTimeout(egCtx, requestTimeout)
			defer cancel()
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Data:       map[string]string{"index": fmt.Sprintf("%d", i)},
			}

			start := time.Now()
			_, createErr := hcpKC.CoreV1().ConfigMaps(namespace).Create(reqCtx, cm, metav1.CreateOptions{})
			if createErr != nil && apierrors.IsAlreadyExists(createErr) {
				createErr = nil
			}
			results[i] = resultWithError{dur: time.Since(start), err: createErr}

			// async cleanup — don't block the measurement
			go func() {
				_ = hcpKC.CoreV1().ConfigMaps(namespace).Delete(
					context.Background(), name, metav1.DeleteOptions{})
			}()
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return loadResult{Errors: ops, FirstErr: err}
	}

	return measuredLoadResult(results, warmup)
}

func measuredLoadResult(results []resultWithError, warmup int) loadResult {
	var out loadResult
	for _, r := range results[warmup:] {
		if r.err == nil {
			out.Successes++
			out.Durations = append(out.Durations, r.dur)
			continue
		}
		out.Errors++
		if out.FirstErr == nil {
			out.FirstErr = r.err
		}
	}
	return out
}

// runReadLoad performs (warmup + ops) ConfigMap List calls with the given
// concurrency. Returns per-List durations for the measured ops.
func runReadLoad(ctx context.Context, hcpKC *kubernetes.Clientset, namespace string, ops, concurrency, warmup int, requestTimeout time.Duration) loadResult {
	total := warmup + ops
	results := make([]resultWithError, total)
	sem := make(chan struct{}, concurrency)

	eg, egCtx := errgroup.WithContext(ctx)
	for i := 0; i < total; i++ {
		i := i
		eg.Go(func() error {
			sem <- struct{}{}
			defer func() { <-sem }()

			reqCtx, cancel := context.WithTimeout(egCtx, requestTimeout)
			defer cancel()
			start := time.Now()
			_, listErr := hcpKC.CoreV1().ConfigMaps(namespace).List(reqCtx, metav1.ListOptions{})
			results[i] = resultWithError{dur: time.Since(start), err: listErr}
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return loadResult{Errors: ops, FirstErr: err}
	}
	return measuredLoadResult(results, warmup)
}

func runWatchChurnLoad(ctx context.Context, hcpKC *kubernetes.Clientset, namespace string, ops, concurrency, warmup, watchers int, requestTimeout, watchDrain time.Duration) watchChurnResult {
	if err := ensureHCPNamespace(ctx, hcpKC, namespace); err != nil {
		return watchChurnResult{
			Churn:    loadResult{Errors: ops, FirstErr: err},
			Watchers: watchers,
		}
	}
	if watchers < 1 {
		watchers = 1
	}
	runID := fmt.Sprintf("%d", time.Now().UnixNano())
	watcherStart := time.Now()
	labelSelector := fmt.Sprintf("%s=%s", watchRunIDLabel, runID)

	watchCtx, stopWatchers := context.WithCancel(ctx)
	defer stopWatchers()

	var collector watchLagCollector
	ready := make(chan struct{}, watchers)
	var wg sync.WaitGroup
	for i := 0; i < watchers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runConfigMapWatcher(watchCtx, hcpKC, namespace, labelSelector, watcherStart, ready, &collector)
		}()
	}
	for i := 0; i < watchers; i++ {
		select {
		case <-ready:
		case <-ctx.Done():
			stopWatchers()
			wg.Wait()
			return watchChurnResult{
				Churn:           loadResult{Errors: ops, FirstErr: ctx.Err()},
				Watchers:        watchers,
				WatchEvents:     collector.events(),
				WatchErrors:     collector.errors(),
				WatchReconnects: collector.reconnects(),
				WatchFirstErr:   collector.firstError(),
				WatchLags:       collector.lags(),
			}
		}
	}

	churn, byOp := runConfigMapChurn(ctx, hcpKC, namespace, runID, ops, concurrency, warmup, requestTimeout)
	waitWatchDrain(ctx, &collector, expectedMeasuredWatchEvents(byOp, watchers), watchDrain)
	stopWatchers()
	wg.Wait()

	return watchChurnResult{
		Churn:           churn,
		ChurnByOp:       byOp,
		Watchers:        watchers,
		WatchEvents:     collector.events(),
		WatchErrors:     collector.errors(),
		WatchReconnects: collector.reconnects(),
		WatchFirstErr:   collector.firstError(),
		WatchLags:       collector.lags(),
	}
}

func expectedMeasuredWatchEvents(byOp map[string]opStat, watchers int) int {
	if watchers < 1 {
		return 0
	}
	var successfulOps int
	for _, op := range churnLifecycleOps {
		successfulOps += byOp[op].Successes
	}
	return successfulOps * watchers
}

func waitWatchDrain(ctx context.Context, collector *watchLagCollector, expectedEvents int, maxWait time.Duration) {
	if expectedEvents <= 0 || maxWait <= 0 {
		return
	}
	timer := time.NewTimer(maxWait)
	defer timer.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for collector.events() < expectedEvents {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			return
		case <-ticker.C:
		}
	}
}

type watchLagCollector struct {
	mu             sync.Mutex
	lagSamples     []time.Duration
	eventCount     int
	errorCount     int
	reconnectCount int
	firstErr       string
}

func (c *watchLagCollector) addEvent(lag time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.eventCount++
	if lag >= 0 {
		c.lagSamples = append(c.lagSamples, lag)
	}
}

func (c *watchLagCollector) addError(err string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.errorCount++
	if c.firstErr == "" {
		c.firstErr = err
	}
}

func (c *watchLagCollector) addReconnect() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reconnectCount++
}

func (c *watchLagCollector) events() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.eventCount
}

func (c *watchLagCollector) errors() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.errorCount
}

func (c *watchLagCollector) reconnects() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reconnectCount
}

func (c *watchLagCollector) firstError() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.firstErr
}

func (c *watchLagCollector) lags() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]time.Duration, len(c.lagSamples))
	copy(out, c.lagSamples)
	return out
}

func runConfigMapWatcher(ctx context.Context, hcpKC *kubernetes.Clientset, namespace, labelSelector string, watcherStart time.Time, ready chan<- struct{}, collector *watchLagCollector) {
	var resourceVersion string
	readySent := false
	for {
		if ctx.Err() != nil {
			return
		}

		watcher, err := hcpKC.CoreV1().ConfigMaps(namespace).Watch(ctx, metav1.ListOptions{
			LabelSelector:   labelSelector,
			ResourceVersion: resourceVersion,
		})
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			collector.addError(err.Error())
			time.Sleep(100 * time.Millisecond)
			continue
		}

		if !readySent {
			select {
			case ready <- struct{}{}:
			default:
			}
			readySent = true
		}

		resourceVersion = consumeConfigMapWatch(ctx, watcher, resourceVersion, watcherStart, collector)
		watcher.Stop()
		if ctx.Err() == nil {
			collector.addReconnect()
		}
	}
}

func consumeConfigMapWatch(ctx context.Context, watcher watchapi.Interface, resourceVersion string, watcherStart time.Time, collector *watchLagCollector) string {
	for {
		select {
		case <-ctx.Done():
			return resourceVersion
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return resourceVersion
			}
			if event.Type == watchapi.Error {
				if ctx.Err() != nil {
					return resourceVersion
				}
				collector.addError(watchErrorString(event))
				return resourceVersion
			}
			if rv := watchResourceVersion(event); rv != "" {
				resourceVersion = rv
			}
			recordWatchEvent(event, watcherStart, collector)
		}
	}
}

func watchResourceVersion(event watchapi.Event) string {
	accessor, err := apiMeta.Accessor(event.Object)
	if err != nil {
		return ""
	}
	return accessor.GetResourceVersion()
}

func recordWatchEvent(event watchapi.Event, watcherStart time.Time, collector *watchLagCollector) {
	if event.Type == watchapi.Error {
		collector.addError(watchErrorString(event))
		return
	}
	if event.Type != watchapi.Added && event.Type != watchapi.Modified && event.Type != watchapi.Deleted {
		return
	}
	cm, ok := event.Object.(*corev1.ConfigMap)
	if !ok || cm == nil {
		return
	}
	raw := cm.Annotations[watchLagAnnotation]
	if raw == "" {
		return
	}
	writeNS, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return
	}
	writeTime := time.Unix(0, writeNS)
	if writeTime.Before(watcherStart) {
		return
	}
	collector.addEvent(time.Since(writeTime))
}

func watchErrorString(event watchapi.Event) string {
	if err := apierrors.FromObject(event.Object); err != nil {
		return err.Error()
	}
	if status, ok := event.Object.(*metav1.Status); ok && status.Message != "" {
		return status.Message
	}
	return fmt.Sprintf("watch error event object %T", event.Object)
}

func runConfigMapChurn(ctx context.Context, hcpKC *kubernetes.Clientset, namespace, runID string, ops, concurrency, warmup int, requestTimeout time.Duration) (loadResult, map[string]opStat) {
	total := warmup + ops
	results := make([]resultWithError, total)
	opResults := make([][]string, total) // per-lifecycle: list of ops that succeeded; last entry is the failed op when err != nil
	sem := make(chan struct{}, concurrency)

	eg, egCtx := errgroup.WithContext(ctx)
	for i := 0; i < total; i++ {
		i := i
		eg.Go(func() error {
			sem <- struct{}{}
			defer func() { <-sem }()

			start := time.Now()
			completed, err := churnConfigMap(egCtx, hcpKC, namespace, runID, i, requestTimeout)
			results[i] = resultWithError{dur: time.Since(start), err: err}
			opResults[i] = completed
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return loadResult{Errors: ops, FirstErr: err}, nil
	}
	return measuredLoadResult(results, warmup), aggregateOpStats(results, opResults, warmup)
}

// aggregateOpStats walks the measured (post-warmup) lifecycles and computes
// per-operation Successes / Errors / FirstErr. completed[i] holds, in order,
// the names of operations that ran in lifecycle i; the last element is the
// failing op when results[i].err != nil. Successful prior steps in a failed
// lifecycle still count as op-level successes.
func aggregateOpStats(results []resultWithError, completed [][]string, warmup int) map[string]opStat {
	out := map[string]opStat{}
	for _, op := range churnLifecycleOps {
		out[op] = opStat{}
	}
	for i := warmup; i < len(results); i++ {
		ops := completed[i]
		failingOp := ""
		if results[i].err != nil && len(ops) > 0 {
			failingOp = ops[len(ops)-1]
		}
		for _, op := range ops {
			s := out[op]
			if op == failingOp {
				s.Errors++
				if s.FirstErr == "" {
					s.FirstErr = results[i].err.Error()
				}
			} else {
				s.Successes++
			}
			out[op] = s
		}
	}
	return out
}

// churnConfigMap runs one create/update/update/delete lifecycle. Returns the
// names of operations that ran (in order); when err != nil the final entry is
// the operation that produced the error.
func churnConfigMap(ctx context.Context, hcpKC *kubernetes.Clientset, namespace, runID string, index int, requestTimeout time.Duration) ([]string, error) {
	name := fmt.Sprintf("watch-churn-%s-%06d", runID, index)
	completed := make([]string, 0, 4)

	reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Labels:      map[string]string{watchRunIDLabel: runID},
			Annotations: map[string]string{watchLagAnnotation: fmt.Sprintf("%d", time.Now().UnixNano())},
		},
		Data: map[string]string{"index": fmt.Sprintf("%d", index), "phase": "create"},
	}
	completed = append(completed, "create")
	if _, err := hcpKC.CoreV1().ConfigMaps(namespace).Create(reqCtx, cm, metav1.CreateOptions{}); err != nil {
		return completed, &lifecycleErr{op: "create", err: err}
	}

	reqCtx, cancel = context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	cm.Annotations[watchLagAnnotation] = fmt.Sprintf("%d", time.Now().UnixNano())
	cm.Data["phase"] = "update"
	completed = append(completed, "update1")
	updated, err := hcpKC.CoreV1().ConfigMaps(namespace).Update(reqCtx, cm, metav1.UpdateOptions{})
	if err != nil {
		return completed, &lifecycleErr{op: "update1", err: err}
	}

	reqCtx, cancel = context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	if updated.Annotations == nil {
		updated.Annotations = map[string]string{}
	}
	updated.Annotations[watchLagAnnotation] = fmt.Sprintf("%d", time.Now().UnixNano())
	completed = append(completed, "update2")
	if _, err = hcpKC.CoreV1().ConfigMaps(namespace).Update(reqCtx, updated, metav1.UpdateOptions{}); err != nil {
		return completed, &lifecycleErr{op: "update2", err: err}
	}

	reqCtx, cancel = context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	completed = append(completed, "delete")
	if err := hcpKC.CoreV1().ConfigMaps(namespace).Delete(reqCtx, name, metav1.DeleteOptions{}); err != nil {
		return completed, &lifecycleErr{op: "delete", err: err}
	}
	return completed, nil
}

// ensureHCPNamespace creates the namespace inside the HCP cluster if absent.
func ensureHCPNamespace(ctx context.Context, hcpKC *kubernetes.Clientset, name string) error {
	_, err := hcpKC.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create HCP namespace %q: %w", name, err)
	}
	return nil
}

// opsThroughput returns operations per second.
func opsThroughput(ops int, elapsed time.Duration) float64 {
	if elapsed <= 0 {
		return 0
	}
	return float64(ops) / elapsed.Seconds()
}
