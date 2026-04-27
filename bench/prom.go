/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// promClient queries the kube-prometheus-stack Prometheus via apiserver proxy.
// The bench harness runs from a laptop with kubeconfig pointing at the mgmt
// apiserver — direct Prom access would require port-forwarding, but proxy
// works out of the box.
type promClient struct {
	kc          *kubernetes.Clientset
	servicePath string // /api/v1/namespaces/<ns>/services/<name>:<port>/proxy
}

// newPromClient builds a client targeting kube-prometheus-stack's Prom Service.
// Default svc name "prom-kube-prometheus-stack-prometheus" matches the helm
// release "prom" used by `make monitoring`.
func newPromClient(rc *rest.Config) (*promClient, error) {
	kc, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, fmt.Errorf("build prom kubernetes client: %w", err)
	}
	return &promClient{
		kc:          kc,
		servicePath: "/api/v1/namespaces/monitoring/services/prom-kube-prometheus-stack-prometheus:9090/proxy",
	}, nil
}

// promResult is the minimal subset of the Prom query response we read.
type promResult struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Value  []any             `json:"value"` // [<unix_ts>, "<value>"]
		} `json:"result"`
	} `json:"data"`
	ErrorType string `json:"errorType,omitempty"`
	Error     string `json:"error,omitempty"`
}

// queryInstant runs a Prom instant query at time t. Returns the first scalar
// value from the result vector, or 0 if the result is empty (treated as "no
// data" rather than an error — most queries are rate()-windowed so an empty
// vector means the metric simply wasn't reported).
func (p *promClient) queryInstant(ctx context.Context, q string, t time.Time) (float64, error) {
	v := url.Values{}
	v.Set("query", q)
	v.Set("time", strconv.FormatInt(t.Unix(), 10))

	raw, err := p.kc.RESTClient().Get().
		AbsPath(p.servicePath + "/api/v1/query").
		RequestURI(p.servicePath + "/api/v1/query?" + v.Encode()).
		DoRaw(ctx)
	if err != nil {
		return 0, fmt.Errorf("prom query %q: %w", q, err)
	}

	var resp promResult
	if err := json.Unmarshal(raw, &resp); err != nil {
		return 0, fmt.Errorf("decode prom response: %w", err)
	}
	if resp.Status != "success" {
		return 0, fmt.Errorf("prom query %q failed: %s: %s", q, resp.ErrorType, resp.Error)
	}
	if len(resp.Data.Result) == 0 {
		return 0, nil
	}
	if len(resp.Data.Result[0].Value) < 2 {
		return 0, nil
	}
	s, ok := resp.Data.Result[0].Value[1].(string)
	if !ok {
		return 0, fmt.Errorf("prom value not a string: %v", resp.Data.Result[0].Value[1])
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		// Prom returns "NaN" for queries with no data points in the window.
		return 0, nil
	}
	return f, nil
}

// promScenarioMetrics are the snapshot-aggregated Prom metrics for one scenario,
// computed at scenario end with rate-windows that fit inside the scenario's
// runtime so values reset per scenario rather than accumulating.
type promScenarioMetrics struct {
	// Mgmt apiserver
	MgmtAPIServerQPS         float64
	MgmtAPIServerP99LatencyS float64

	// Mgmt etcd
	MgmtEtcdFsyncP99S           float64
	MgmtEtcdCommitP99S          float64
	MgmtEtcdLeaderChangesPerSec float64

	// Operator (k0smotron controller-manager)
	OperatorReconcileP95S         float64
	OperatorReconcileErrorsPerSec float64
	OperatorWorkqueueDepthMax     float64

	// Worker node pressure (max across the m5.4xlarge worker pool)
	WorkerCPUMaxPct float64
	WorkerMemMaxPct float64

	// External DB (postgres / mysql) — populated when a postgres/mysql exporter is scraped
	DBPostgresActiveConn       float64
	DBPostgresXactCommitPerSec float64
	DBPostgresDeadlocksPerSec  float64
	DBMysqlThreadsConnected    float64
	DBMysqlInnodbRowLockTimeMs float64
	DBMysqlSlowQueriesPerSec   float64
}

// collectPromMetrics runs a fixed set of PromQL instant queries at queryAt
// (typically scenario end), with rate windows sized to cover the scenario.
// runWindow is rounded up to a Prom-friendly value.
func collectPromMetrics(ctx context.Context, pc *promClient, queryAt time.Time, runWindow time.Duration) (promScenarioMetrics, error) {
	if runWindow < 30*time.Second {
		runWindow = 30 * time.Second
	}
	// Round up to the nearest minute; min "2m" so rate() has at least 2 samples.
	w := (runWindow + time.Minute - 1).Truncate(time.Minute)
	if w < 2*time.Minute {
		w = 2 * time.Minute
	}
	win := fmt.Sprintf("%dm", int(w/time.Minute))

	queries := map[string]string{
		"apiserver_qps":             fmt.Sprintf(`sum(rate(apiserver_request_total[%s]))`, win),
		"apiserver_p99":             fmt.Sprintf(`histogram_quantile(0.99, sum by (le) (rate(apiserver_request_duration_seconds_bucket[%s])))`, win),
		"etcd_fsync_p99":            fmt.Sprintf(`histogram_quantile(0.99, sum by (le) (rate(etcd_disk_wal_fsync_duration_seconds_bucket[%s])))`, win),
		"etcd_commit_p99":           fmt.Sprintf(`histogram_quantile(0.99, sum by (le) (rate(etcd_disk_backend_commit_duration_seconds_bucket[%s])))`, win),
		"etcd_leader_changes":       fmt.Sprintf(`sum(rate(etcd_server_leader_changes_seen_total[%s]))`, win),
		"operator_reconcile_p95":    fmt.Sprintf(`histogram_quantile(0.95, sum by (le) (rate(controller_runtime_reconcile_time_seconds_bucket[%s])))`, win),
		"operator_reconcile_errors": fmt.Sprintf(`sum(rate(controller_runtime_reconcile_total{result="error"}[%s]))`, win),
		"operator_workqueue_max":    fmt.Sprintf(`max(max_over_time(workqueue_depth[%s]))`, win),
		"worker_cpu_max":            fmt.Sprintf(`max(max_over_time((1 - rate(node_cpu_seconds_total{mode="idle",job="node-exporter"}[2m]))[%s:30s]))`, win),
		"worker_mem_max":            fmt.Sprintf(`max(max_over_time((1 - node_memory_MemAvailable_bytes/node_memory_MemTotal_bytes)[%s:30s]))`, win),

		// Instant gauges wrapped in max_over_time so we capture peak across the
		// scenario window, not the value at query time (which decays as load
		// drops at the end of provision/steady-state).
		"pg_active_conn":     fmt.Sprintf(`max_over_time(sum(pg_stat_activity_count{datname="bench",state="active"})[%s:15s])`, win),
		"pg_xact_commit":     fmt.Sprintf(`sum(rate(pg_stat_database_xact_commit{datname="bench"}[%s]))`, win),
		"pg_deadlocks":       fmt.Sprintf(`sum(rate(pg_stat_database_deadlocks{datname="bench"}[%s]))`, win),
		"mysql_threads":      fmt.Sprintf(`max_over_time(mysql_global_status_threads_connected[%s:15s])`, win),
		"mysql_row_lock_ms":  fmt.Sprintf(`max_over_time((mysql_global_status_innodb_row_lock_time / 1000)[%s:15s])`, win),
		"mysql_slow_queries": fmt.Sprintf(`rate(mysql_global_status_slow_queries[%s])`, win),
	}

	results := make(map[string]float64, len(queries))
	for name, q := range queries {
		v, err := pc.queryInstant(ctx, q, queryAt)
		if err != nil {
			// One bad query shouldn't poison the whole scenario row — log and continue.
			results[name] = 0
			continue
		}
		results[name] = v
	}

	return promScenarioMetrics{
		MgmtAPIServerQPS:              results["apiserver_qps"],
		MgmtAPIServerP99LatencyS:      results["apiserver_p99"],
		MgmtEtcdFsyncP99S:             results["etcd_fsync_p99"],
		MgmtEtcdCommitP99S:            results["etcd_commit_p99"],
		MgmtEtcdLeaderChangesPerSec:   results["etcd_leader_changes"],
		OperatorReconcileP95S:         results["operator_reconcile_p95"],
		OperatorReconcileErrorsPerSec: results["operator_reconcile_errors"],
		OperatorWorkqueueDepthMax:     results["operator_workqueue_max"],
		WorkerCPUMaxPct:               results["worker_cpu_max"] * 100,
		WorkerMemMaxPct:               results["worker_mem_max"] * 100,
		DBPostgresActiveConn:          results["pg_active_conn"],
		DBPostgresXactCommitPerSec:    results["pg_xact_commit"],
		DBPostgresDeadlocksPerSec:     results["pg_deadlocks"],
		DBMysqlThreadsConnected:       results["mysql_threads"],
		DBMysqlInnodbRowLockTimeMs:    results["mysql_row_lock_ms"],
		DBMysqlSlowQueriesPerSec:      results["mysql_slow_queries"],
	}, nil
}
