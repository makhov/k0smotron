//go:build bench

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
	"flag"
	"fmt"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

var (
	perfOps                    = flag.Int("bench.perf-ops", 500, "configmap operations per phase (write + read) per backend")
	perfConcurrency            = flag.Int("bench.perf-concurrency", 10, "concurrent workers for load phases")
	perfWarmup                 = flag.Int("bench.perf-warmup", 50, "warmup operations discarded from measurements")
	perfHCPQPS                 = flag.Float64("bench.hcp-client-qps", 2000, "client-go QPS limit for the HCP API client")
	perfHCPBurst               = flag.Int("bench.hcp-client-burst", 4000, "client-go burst limit for the HCP API client")
	perfResourceSampleInterval = flag.Duration("bench.resource-sample-interval", 2*time.Second, "interval for sampling HCP pod resource usage during perf load")
	perfHCPCPULimit            = flag.String("bench.hcp-cpu-limit", "", "CPU request+limit for HCP control-plane pods, empty=unlimited")
	perfHCPMemoryLimit         = flag.String("bench.hcp-memory-limit", "", "memory request+limit for HCP control-plane pods, empty=unlimited")
	perfRequestTimeout         = flag.Duration("bench.request-timeout", 30*time.Second, "timeout for one HCP client operation during perf load")
	perfPhaseTimeout           = flag.Duration("bench.phase-timeout", 5*time.Minute, "maximum duration for each perf load phase")
	perfProfile                = flag.String("bench.perf-profile", "create-list", "perf profile to run: create-list, watch-churn, or all")
	perfWatchers               = flag.Int("bench.watchers", 25, "number of concurrent watchers for the watch-churn perf profile")
	perfReportPath             = flag.String("bench.perf-report", "bench-perf-results.csv", "CSV output for storage performance results")
)

// TestStoragePerformance creates one single-replica HCP per enabled storage backend,
// drives write (ConfigMap create) and read (ConfigMap list) load against each
// HCP's API server, and records latency percentiles + throughput.
func TestStoragePerformance(t *testing.T) {
	reporter, err := NewPerfCSVReporter(*perfReportPath)
	if err != nil {
		t.Fatalf("create perf reporter: %v", err)
	}
	defer reporter.Close()

	runStoragePerformanceSuite(t, reporter, 1, "")
}

// TestStoragePerformanceHA repeats the storage benchmark with 3 control-plane
// replicas. SQLite is excluded because it is intentionally single-node.
func TestStoragePerformanceHA(t *testing.T) {
	reporter, err := NewPerfCSVReporter(*perfReportPath)
	if err != nil {
		t.Fatalf("create perf reporter: %v", err)
	}
	defer reporter.Close()

	runStoragePerformanceSuite(t, reporter, 3, "-ha")
}

func runStoragePerformanceSuite(t *testing.T, reporter *PerfCSVReporter, replicas int32, nameSuffix string) {
	filter := parseFilter(*storageFilter)

	for _, sc := range perfStorageConfigs(*k0sVersion) {
		sc := sc
		if len(filter) > 0 && !filter[sc.StorageName] {
			continue
		}
		if !sc.Enabled {
			t.Logf("skipping %q: required env var not set", sc.StorageName)
			continue
		}
		if replicas > 1 && sc.StorageName == "kine-sqlite" {
			t.Logf("skipping %q in HA perf: sqlite backend is single-node", sc.StorageName)
			continue
		}

		resultStorageName := sc.StorageName + nameSuffix
		for _, profile := range selectedPerfProfiles(*perfProfile) {
			profile := profile
			t.Run(resultStorageName+"/"+profile, func(t *testing.T) {
				runStoragePerformanceCase(t, reporter, sc, replicas, resultStorageName, profile)
			})
		}
	}
}

func selectedPerfProfiles(profile string) []string {
	switch strings.TrimSpace(profile) {
	case "", "create-list":
		return []string{"create-list"}
	case "watch-churn":
		return []string{"watch-churn"}
	case "all":
		return []string{"create-list", "watch-churn"}
	default:
		return []string{profile}
	}
}

func runStoragePerformanceCase(t *testing.T, reporter *PerfCSVReporter, sc storageEntry, replicas int32, resultStorageName, profile string) {
	t.Helper()

	ctx := context.Background()

	resources, err := hcpResources(*perfHCPCPULimit, *perfHCPMemoryLimit)
	if err != nil {
		t.Fatalf("parse HCP resource limits: %v", err)
	}

	safeName := strings.ReplaceAll(resultStorageName+"-"+profile, "_", "-")
	clusterName := "perf-" + safeName
	mgmtNS := "bench-perf-" + safeName

	nodeAddrs, err := hcpNodeAddresses(ctx, globalKC)
	if err != nil {
		t.Fatalf("pick node address: %v", err)
	}
	nodeAddr := nodeAddrs[0]
	t.Logf("[%s] using externalAddress %s:%d for HCP apiserver", resultStorageName, nodeAddr, hcpAPINodePort)

	if err := ensureNamespace(ctx, globalKC, mgmtNS); err != nil {
		t.Fatalf("ensure namespace: %v", err)
	}
	defer func() {
		_ = globalKC.CoreV1().Namespaces().Delete(
			context.Background(), mgmtNS, metav1.DeleteOptions{})
	}()

	cfg, err := configurePerfScenario(ctx, sc, clusterName, mgmtNS, nodeAddrs, replicas, *k0sVersion)
	if err != nil {
		t.Fatalf("configure scenario: %v", err)
	}
	cfg.Resources = resources

	t.Logf("[%s] creating HCP (NodePort, replicas=%d, cpuLimit=%q, memoryLimit=%q)", resultStorageName, replicas, *perfHCPCPULimit, *perfHCPMemoryLimit)
	if err := createCluster(ctx, globalKC, clusterName, mgmtNS, cfg); err != nil {
		t.Fatalf("create cluster: %v", err)
	}
	defer func() {
		_ = deleteCluster(context.Background(), globalKC, clusterName, mgmtNS)
	}()

	readyCtx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	if err := waitClusterReady(readyCtx, globalKC, clusterName, mgmtNS); err != nil {
		t.Fatalf("wait ready: %v", err)
	}
	t.Logf("[%s] HCP ready", resultStorageName)

	t.Logf("[%s] using node %s:%d for HCP apiserver", resultStorageName, nodeAddr, hcpAPINodePort)

	t.Logf("[%s] HCP client limits: qps=%.0f burst=%d", resultStorageName, *perfHCPQPS, *perfHCPBurst)
	hcpKC, err := buildHCPClient(ctx, globalKC, clusterName, mgmtNS, nodeAddr, float32(*perfHCPQPS), *perfHCPBurst)
	if err != nil {
		t.Fatalf("build HCP client: %v", err)
	}
	if err := waitHCPReachable(ctx, hcpKC); err != nil {
		t.Fatalf("HCP unreachable: %v", err)
	}

	const loadNS = "load-test"

	stopResourceSampler := startPerfPodResourceSampler(t, ctx, mgmtNS, *perfResourceSampleInterval)
	result := runPerfProfile(t, ctx, hcpKC, loadNS, resultStorageName, profile)

	resourceSummary := stopResourceSampler()

	result.Timestamp = time.Now().UTC()
	result.StorageName = resultStorageName
	result.Profile = profile
	result.Concurrency = *perfConcurrency
	result.Ops = *perfOps
	result.CPULimit = *perfHCPCPULimit
	result.MemoryLimit = *perfHCPMemoryLimit
	result.HCPResourceSamples = resourceSummary.Samples
	result.HCPP50MemMi = resourceSummary.P50MemMi
	result.HCPP95MemMi = resourceSummary.P95MemMi
	result.HCPMaxMemMi = resourceSummary.MaxMemMi
	result.HCPP50CPUm = resourceSummary.P50CPUm
	result.HCPP95CPUm = resourceSummary.P95CPUm
	result.HCPMaxCPUm = resourceSummary.MaxCPUm

	t.Logf("[%s] HCP pod resources during load samples=%d p50=%dMi/%dm p95=%dMi/%dm max=%dMi/%dm",
		resultStorageName, resourceSummary.Samples, resourceSummary.P50MemMi, resourceSummary.P50CPUm, resourceSummary.P95MemMi, resourceSummary.P95CPUm, resourceSummary.MaxMemMi, resourceSummary.MaxCPUm)

	if err := reporter.Append(result); err != nil {
		t.Logf("warning: write perf result: %v", err)
	}

	t.Logf("[%s] completed replicas=%d", fmt.Sprintf("%s", resultStorageName), replicas)
}

func runPerfProfile(t *testing.T, ctx context.Context, hcpKC *kubernetes.Clientset, loadNS, resultStorageName, profile string) PerfResult {
	t.Helper()
	switch profile {
	case "create-list":
		return runCreateListProfile(t, ctx, hcpKC, loadNS, resultStorageName)
	case "watch-churn":
		return runWatchChurnProfile(t, ctx, hcpKC, loadNS, resultStorageName)
	default:
		t.Fatalf("unknown perf profile %q", profile)
		return PerfResult{}
	}
}

func runCreateListProfile(t *testing.T, ctx context.Context, hcpKC *kubernetes.Clientset, loadNS, resultStorageName string) PerfResult {
	t.Helper()
	t.Logf("[%s] write load: %d ops, concurrency %d, warmup %d",
		resultStorageName, *perfOps, *perfConcurrency, *perfWarmup)
	writeStart := time.Now()
	writeCtx, writeCancel := context.WithTimeout(ctx, *perfPhaseTimeout)
	writeResult := runWriteLoad(writeCtx, hcpKC, loadNS, *perfOps, *perfConcurrency, *perfWarmup, *perfRequestTimeout)
	writeCancel()
	writeElapsed := time.Since(writeStart)
	if writeResult.FirstErr != nil {
		t.Logf("[%s] write load first error: %v", resultStorageName, writeResult.FirstErr)
	}

	t.Logf("[%s] read load: %d ops, concurrency %d, warmup %d",
		resultStorageName, *perfOps, *perfConcurrency, *perfWarmup)
	readStart := time.Now()
	readCtx, readCancel := context.WithTimeout(ctx, *perfPhaseTimeout)
	readResult := runReadLoad(readCtx, hcpKC, loadNS, *perfOps, *perfConcurrency, *perfWarmup, *perfRequestTimeout)
	readCancel()
	readElapsed := time.Since(readStart)
	if readResult.FirstErr != nil {
		t.Logf("[%s] read load first error: %v", resultStorageName, readResult.FirstErr)
	}

	wp50, wp95, wp99, _ := percentiles(writeResult.Durations)
	rp50, rp95, rp99, _ := percentiles(readResult.Durations)
	t.Logf("[%s] write p50=%s p95=%s p99=%s  %.1f ops/s",
		resultStorageName, wp50, wp95, wp99, opsThroughput(writeResult.Successes, writeElapsed))
	t.Logf("[%s] write success=%d errors=%d errorRate=%.2f%%",
		resultStorageName, writeResult.Successes, writeResult.Errors, errorRate(writeResult.Successes, writeResult.Errors)*100)
	t.Logf("[%s] read  p50=%s p95=%s p99=%s  %.1f ops/s",
		resultStorageName, rp50, rp95, rp99, opsThroughput(readResult.Successes, readElapsed))
	t.Logf("[%s] read success=%d errors=%d errorRate=%.2f%%",
		resultStorageName, readResult.Successes, readResult.Errors, errorRate(readResult.Successes, readResult.Errors)*100)

	return PerfResult{
		WriteP50:        wp50,
		WriteP95:        wp95,
		WriteP99:        wp99,
		WriteThroughput: opsThroughput(writeResult.Successes, writeElapsed),
		WriteSuccesses:  writeResult.Successes,
		WriteErrors:     writeResult.Errors,
		WriteErrorRate:  errorRate(writeResult.Successes, writeResult.Errors),
		ReadP50:         rp50,
		ReadP95:         rp95,
		ReadP99:         rp99,
		ReadThroughput:  opsThroughput(readResult.Successes, readElapsed),
		ReadSuccesses:   readResult.Successes,
		ReadErrors:      readResult.Errors,
		ReadErrorRate:   errorRate(readResult.Successes, readResult.Errors),
	}
}

func runWatchChurnProfile(t *testing.T, ctx context.Context, hcpKC *kubernetes.Clientset, loadNS, resultStorageName string) PerfResult {
	t.Helper()
	t.Logf("[%s] watch-churn load: %d lifecycles, concurrency %d, warmup %d, watchers %d",
		resultStorageName, *perfOps, *perfConcurrency, *perfWarmup, *perfWatchers)
	start := time.Now()
	phaseCtx, cancel := context.WithTimeout(ctx, *perfPhaseTimeout)
	watchResult := runWatchChurnLoad(phaseCtx, hcpKC, loadNS, *perfOps, *perfConcurrency, *perfWarmup, *perfWatchers, *perfRequestTimeout)
	cancel()
	elapsed := time.Since(start)
	if watchResult.Churn.FirstErr != nil {
		t.Logf("[%s] watch-churn first error: %v", resultStorageName, watchResult.Churn.FirstErr)
	}

	cp50, cp95, cp99, _ := percentiles(watchResult.Churn.Durations)
	lp50, lp95, lp99, lmax := percentiles(watchResult.WatchLags)
	t.Logf("[%s] churn p50=%s p95=%s p99=%s %.1f lifecycles/s success=%d errors=%d errorRate=%.2f%%",
		resultStorageName, cp50, cp95, cp99, opsThroughput(watchResult.Churn.Successes, elapsed), watchResult.Churn.Successes, watchResult.Churn.Errors, errorRate(watchResult.Churn.Successes, watchResult.Churn.Errors)*100)
	t.Logf("[%s] watch events=%d errors=%d eventRate=%.1f/s lag p50=%s p95=%s p99=%s max=%s",
		resultStorageName, watchResult.WatchEvents, watchResult.WatchErrors, opsThroughput(watchResult.WatchEvents, elapsed), lp50, lp95, lp99, lmax)

	return PerfResult{
		WriteP50:        cp50,
		WriteP95:        cp95,
		WriteP99:        cp99,
		WriteThroughput: opsThroughput(watchResult.Churn.Successes, elapsed),
		WriteSuccesses:  watchResult.Churn.Successes,
		WriteErrors:     watchResult.Churn.Errors,
		WriteErrorRate:  errorRate(watchResult.Churn.Successes, watchResult.Churn.Errors),
		Watchers:        watchResult.Watchers,
		WatchEvents:     watchResult.WatchEvents,
		WatchErrors:     watchResult.WatchErrors,
		WatchEventRate:  opsThroughput(watchResult.WatchEvents, elapsed),
		WatchLagP50:     lp50,
		WatchLagP95:     lp95,
		WatchLagP99:     lp99,
		WatchLagMax:     lmax,
	}
}

func errorRate(successes, errors int) float64 {
	total := successes + errors
	if total == 0 {
		return 0
	}
	return float64(errors) / float64(total)
}

type perfResourceSummary struct {
	Samples  int
	P50MemMi int64
	P95MemMi int64
	MaxMemMi int64
	P50CPUm  int64
	P95CPUm  int64
	MaxCPUm  int64
}

type perfResourceSample struct {
	memMi int64
	cpuM  int64
}

func startPerfPodResourceSampler(t *testing.T, ctx context.Context, namespace string, interval time.Duration) func() perfResourceSummary {
	t.Helper()

	mc, err := newMetricsClient(globalRC)
	if err != nil {
		t.Logf("warning: cannot build metrics client, skipping HCP resource metrics: %v", err)
		return func() perfResourceSummary { return perfResourceSummary{} }
	}
	if interval <= 0 {
		interval = 2 * time.Second
	}

	sampleCtx, cancel := context.WithCancel(ctx)
	done := make(chan perfResourceSummary, 1)
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		samples := make([]perfResourceSample, 0, 16)
		collect := func() {
			podSamples, err := collectHCPMetrics(sampleCtx, mc, globalKC, namespace)
			if err != nil {
				t.Logf("warning: HCP resource metrics unavailable: %v", err)
				return
			}
			var totalCPU, totalMem int64
			for _, sample := range podSamples {
				totalCPU += sample.CPUMillis
				totalMem += sample.MemoryMiB
			}
			samples = append(samples, perfResourceSample{memMi: totalMem, cpuM: totalCPU})
		}

		collect()
		for {
			select {
			case <-sampleCtx.Done():
				done <- summarizePerfResourceSamples(samples)
				return
			case <-ticker.C:
				collect()
			}
		}
	}()

	return func() perfResourceSummary {
		cancel()
		return <-done
	}
}

func summarizePerfResourceSamples(samples []perfResourceSample) perfResourceSummary {
	if len(samples) == 0 {
		return perfResourceSummary{}
	}

	cpuVals := make([]int64, 0, len(samples))
	memVals := make([]int64, 0, len(samples))
	var maxCPU, maxMem int64
	for _, sample := range samples {
		cpuVals = append(cpuVals, sample.cpuM)
		memVals = append(memVals, sample.memMi)
		if sample.cpuM > maxCPU {
			maxCPU = sample.cpuM
		}
		if sample.memMi > maxMem {
			maxMem = sample.memMi
		}
	}
	p50CPU, p95CPU := int64Percentiles(cpuVals)
	p50Mem, p95Mem := int64Percentiles(memVals)
	return perfResourceSummary{
		Samples:  len(samples),
		P50MemMi: p50Mem,
		P95MemMi: p95Mem,
		MaxMemMi: maxMem,
		P50CPUm:  p50CPU,
		P95CPUm:  p95CPU,
		MaxCPUm:  maxCPU,
	}
}
