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
	"encoding/csv"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// csvHeader lists column names in snake_case matching the fields of RunResult.
var csvHeader = []string{
	"timestamp",
	"storage_name",
	"cluster_count",
	"parallelism",
	"run_id",
	"provision_p50_s",
	"provision_p95_s",
	"provision_p99_s",
	"provision_max_s",
	"hcp_p50_mem_mi",
	"hcp_p95_mem_mi",
	"hcp_total_mem_mi",
	"hcp_p50_cpu_m",
	"hcp_total_cpu_m",
	"storage_p50_mem_mi",
	"storage_p95_mem_mi",
	"storage_total_mem_mi",
	"storage_p50_cpu_m",
	"storage_total_cpu_m",
	"operator_mem_mi",
	"operator_cpu_m",
	"churn_recovery_p50_s",
	"churn_recovery_p95_s",
	"mgmt_apiserver_qps",
	"mgmt_apiserver_p99_s",
	"mgmt_etcd_fsync_p99_s",
	"mgmt_etcd_commit_p99_s",
	"mgmt_etcd_leader_changes_per_s",
	"op_reconcile_p95_s",
	"op_reconcile_err_per_s",
	"op_workqueue_depth_max",
	"worker_cpu_max_pct",
	"worker_mem_max_pct",
	"storage_internals_json",
}

// CSVReporter writes RunResult records to a CSV file.
// All public methods are safe for concurrent use.
type CSVReporter struct {
	mu     sync.Mutex
	f      *os.File
	writer *csv.Writer
}

// NewCSVReporter opens (or creates) a CSV file at path, writes the header row,
// and returns a ready-to-use reporter.
func NewCSVReporter(path string) (*CSVReporter, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open report file %q: %w", path, err)
	}

	r := &CSVReporter{
		f:      f,
		writer: csv.NewWriter(f),
	}

	// Write header only when the file is new (zero size).
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("stat report file: %w", err)
	}
	if fi.Size() == 0 {
		if err := r.writer.Write(csvHeader); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("write CSV header: %w", err)
		}
		r.writer.Flush()
		if err := r.writer.Error(); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("flush CSV header: %w", err)
		}
	}

	return r, nil
}

// Append writes one RunResult row to the CSV file.
func (r *CSVReporter) Append(res RunResult) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	row := []string{
		res.Timestamp.Format(time.RFC3339),
		res.StorageName,
		fmt.Sprintf("%d", res.ClusterCount),
		fmt.Sprintf("%d", res.Parallelism),
		fmt.Sprintf("%d", res.RunID),
		fmtSeconds(res.ProvisionP50),
		fmtSeconds(res.ProvisionP95),
		fmtSeconds(res.ProvisionP99),
		fmtSeconds(res.ProvisionMax),
		fmt.Sprintf("%d", res.HCPP50MemMi),
		fmt.Sprintf("%d", res.HCPP95MemMi),
		fmt.Sprintf("%d", res.HCPTotalMemMi),
		fmt.Sprintf("%d", res.HCPP50CPUm),
		fmt.Sprintf("%d", res.HCPTotalCPUm),
		fmt.Sprintf("%d", res.StorageP50MemMi),
		fmt.Sprintf("%d", res.StorageP95MemMi),
		fmt.Sprintf("%d", res.StorageTotalMemMi),
		fmt.Sprintf("%d", res.StorageP50CPUm),
		fmt.Sprintf("%d", res.StorageTotalCPUm),
		fmt.Sprintf("%d", res.OperatorMemMi),
		fmt.Sprintf("%d", res.OperatorCPUm),
		fmtSeconds(res.ChurnRecoveryP50),
		fmtSeconds(res.ChurnRecoveryP95),
		fmtFloat(res.MgmtAPIServerQPS),
		fmtFloat(res.MgmtAPIServerP99LatencyS),
		fmtFloat(res.MgmtEtcdFsyncP99S),
		fmtFloat(res.MgmtEtcdCommitP99S),
		fmtFloat(res.MgmtEtcdLeaderChangesPerSec),
		fmtFloat(res.OperatorReconcileP95S),
		fmtFloat(res.OperatorReconcileErrorsPerSec),
		fmtFloat(res.OperatorWorkqueueDepthMax),
		fmtFloat(res.WorkerCPUMaxPct),
		fmtFloat(res.WorkerMemMaxPct),
		res.StorageInternalsJSON,
	}

	if err := r.writer.Write(row); err != nil {
		return fmt.Errorf("write CSV row: %w", err)
	}
	r.writer.Flush()
	return r.writer.Error()
}

// Close flushes any buffered data and closes the underlying file.
func (r *CSVReporter) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.writer.Flush()
	if err := r.writer.Error(); err != nil {
		_ = r.f.Close()
		return err
	}
	return r.f.Close()
}

// fmtSeconds formats a duration as a decimal seconds string (e.g. "12.345").
func fmtSeconds(d time.Duration) string {
	return fmt.Sprintf("%.3f", d.Seconds())
}

// fmtFloat formats a Prom-derived float with 3 decimal places.
func fmtFloat(f float64) string {
	return fmt.Sprintf("%.3f", f)
}

// ── Storage performance reporter ─────────────────────────────────────────────

var perfCSVHeader = []string{
	"timestamp",
	"storage_name",
	"profile",
	"concurrency",
	"ops",
	"run_id",
	"hcp_cpu_limit",
	"hcp_memory_limit",
	"write_p50_s",
	"write_p95_s",
	"write_p99_s",
	"write_throughput_ops",
	"write_successes",
	"write_errors",
	"write_error_rate",
	"read_p50_s",
	"read_p95_s",
	"read_p99_s",
	"read_throughput_ops",
	"read_successes",
	"read_errors",
	"read_error_rate",
	"create_errors",
	"create_first_err",
	"update1_errors",
	"update1_first_err",
	"update2_errors",
	"update2_first_err",
	"delete_errors",
	"delete_first_err",
	"watchers",
	"watch_events",
	"watch_errors",
	"watch_reconnects",
	"watch_first_err",
	"watch_event_rate",
	"watch_lag_p50_s",
	"watch_lag_p95_s",
	"watch_lag_p99_s",
	"watch_lag_max_s",
	"hcp_resource_samples",
	"hcp_p50_mem_mi",
	"hcp_p95_mem_mi",
	"hcp_max_mem_mi",
	"hcp_p50_cpu_m",
	"hcp_p95_cpu_m",
	"hcp_max_cpu_m",
}

// PerfCSVReporter writes PerfResult records to a CSV file.
type PerfCSVReporter struct {
	mu     sync.Mutex
	f      *os.File
	writer *csv.Writer
}

// NewPerfCSVReporter opens (or creates) path and writes the header row.
func NewPerfCSVReporter(path string) (*PerfCSVReporter, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open perf report %q: %w", path, err)
	}

	r := &PerfCSVReporter{f: f, writer: csv.NewWriter(f)}

	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("stat perf report: %w", err)
	}
	if fi.Size() > 0 {
		header, err := csv.NewReader(f).Read()
		if err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("read perf CSV header: %w", err)
		}
		if strings.Join(header, ",") != strings.Join(perfCSVHeader, ",") {
			_ = f.Close()
			return nil, fmt.Errorf("perf report %q has an old or incompatible header; use a fresh -bench.perf-report path or remove the old file", path)
		}
	}

	if fi.Size() == 0 {
		if err := r.writer.Write(perfCSVHeader); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("write perf CSV header: %w", err)
		}
		r.writer.Flush()
		if err := r.writer.Error(); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("flush perf CSV header: %w", err)
		}
	}

	return r, nil
}

// Append writes one PerfResult row.
func (r *PerfCSVReporter) Append(res PerfResult) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	row := []string{
		res.Timestamp.Format(time.RFC3339),
		res.StorageName,
		res.Profile,
		fmt.Sprintf("%d", res.Concurrency),
		fmt.Sprintf("%d", res.Ops),
		fmt.Sprintf("%d", res.RunID),
		res.CPULimit,
		res.MemoryLimit,
		fmtSeconds(res.WriteP50),
		fmtSeconds(res.WriteP95),
		fmtSeconds(res.WriteP99),
		fmt.Sprintf("%.2f", res.WriteThroughput),
		fmt.Sprintf("%d", res.WriteSuccesses),
		fmt.Sprintf("%d", res.WriteErrors),
		fmt.Sprintf("%.4f", res.WriteErrorRate),
		fmtSeconds(res.ReadP50),
		fmtSeconds(res.ReadP95),
		fmtSeconds(res.ReadP99),
		fmt.Sprintf("%.2f", res.ReadThroughput),
		fmt.Sprintf("%d", res.ReadSuccesses),
		fmt.Sprintf("%d", res.ReadErrors),
		fmt.Sprintf("%.4f", res.ReadErrorRate),
		fmt.Sprintf("%d", res.CreateErrors),
		res.CreateFirstErr,
		fmt.Sprintf("%d", res.Update1Errors),
		res.Update1FirstErr,
		fmt.Sprintf("%d", res.Update2Errors),
		res.Update2FirstErr,
		fmt.Sprintf("%d", res.DeleteErrors),
		res.DeleteFirstErr,
		fmt.Sprintf("%d", res.Watchers),
		fmt.Sprintf("%d", res.WatchEvents),
		fmt.Sprintf("%d", res.WatchErrors),
		fmt.Sprintf("%d", res.WatchReconnects),
		res.WatchFirstErr,
		fmt.Sprintf("%.2f", res.WatchEventRate),
		fmtSeconds(res.WatchLagP50),
		fmtSeconds(res.WatchLagP95),
		fmtSeconds(res.WatchLagP99),
		fmtSeconds(res.WatchLagMax),
		fmt.Sprintf("%d", res.HCPResourceSamples),
		fmt.Sprintf("%d", res.HCPP50MemMi),
		fmt.Sprintf("%d", res.HCPP95MemMi),
		fmt.Sprintf("%d", res.HCPMaxMemMi),
		fmt.Sprintf("%d", res.HCPP50CPUm),
		fmt.Sprintf("%d", res.HCPP95CPUm),
		fmt.Sprintf("%d", res.HCPMaxCPUm),
	}

	if err := r.writer.Write(row); err != nil {
		return fmt.Errorf("write perf CSV row: %w", err)
	}
	r.writer.Flush()
	return r.writer.Error()
}

// Close flushes and closes the underlying file.
func (r *PerfCSVReporter) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.writer.Flush()
	if err := r.writer.Error(); err != nil {
		_ = r.f.Close()
		return err
	}
	return r.f.Close()
}
