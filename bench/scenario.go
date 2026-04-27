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
	"os"
	"time"

	km "github.com/k0sproject/k0smotron/api/k0smotron.io/v1beta2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// ScenarioConfig holds the parameters for a single benchmark scenario.
type ScenarioConfig struct {
	StorageName string
	// Storage sub-fields (mirror km.StorageSpec components for JSON serialisation).
	StorageType km.StorageType
	StorageKine km.KineSpec
	StorageEtcd km.EtcdSpec
	StorageNATS km.NATSSpec

	// ServiceType selects how the HCP apiserver is exposed. Empty = ClusterIP.
	// Perf tests set NodePort so the load generator can hit the apiserver
	// directly via a worker node IP — port-forward serializes all traffic
	// through a single SPDY stream and caps throughput.
	ServiceType corev1.ServiceType
	// ExternalAddress is written to spec.externalAddress for NodePort/LoadBalancer
	// HCPs so generated child-cluster kubeconfigs point at a reachable endpoint.
	ExternalAddress string
	// APISANs are merged into k0sConfig.spec.api.sans for the child API server
	// certificate. Perf NodePort tests include all worker external addresses.
	APISANs   []string
	K0sConfig map[string]any
	Image     string
	Patches   []km.ComponentPatch
	Resources corev1.ResourceRequirements

	HCPReplicas  int32
	ClusterCount int
	Parallelism  int
	K0sVersion   string
	Namespace    string

	// StorageNamespace is the namespace where the external storage backend (postgres/mysql)
	// runs as a pod. Empty for in-namespace backends (etcd, kine-sqlite, kine-nats).
	// When set, the metrics sampler also polls this namespace and reports db_* columns.
	StorageNamespace string
}

// RunResult holds all measured outcomes from one scenario run.
type RunResult struct {
	Timestamp    time.Time
	StorageName  string
	ClusterCount int
	Parallelism  int

	// Provisioning latency percentiles across all clusters in the scenario.
	ProvisionP50 time.Duration
	ProvisionP95 time.Duration
	ProvisionP99 time.Duration
	ProvisionMax time.Duration

	// Peak resource usage aggregated across all HCP pods (kmc-* without -etcd-).
	// For kine variants, the kine process runs inside the HCP "controller" container
	// and is naturally folded into these numbers — there is no separate kine pod.
	HCPP50MemMi   int64 // per-pod median peak memory in MiB
	HCPP95MemMi   int64 // per-pod p95 peak memory in MiB
	HCPTotalMemMi int64 // sum of per-pod peaks
	HCPP50CPUm    int64 // per-pod median peak CPU in millicores
	HCPTotalCPUm  int64 // sum of per-pod peaks

	// Etcd backend pods (kmc-*-etcd-N), present only when StorageType=etcd.
	// Same shape as HCP fields. Zero for kine-* scenarios.
	EtcdP50MemMi   int64
	EtcdP95MemMi   int64
	EtcdTotalMemMi int64
	EtcdP50CPUm    int64
	EtcdTotalCPUm  int64

	// External DB pods (postgres / mysql) sampled from cfg.StorageNamespace.
	// Zero for backends that don't use an external DB.
	DBP50MemMi   int64
	DBP95MemMi   int64
	DBTotalMemMi int64
	DBP50CPUm    int64
	DBTotalCPUm  int64

	// Operator peak resource usage across the run.
	OperatorMemMi int64
	OperatorCPUm  int64

	// Churn recovery latency percentiles.
	ChurnRecoveryP50 time.Duration
	ChurnRecoveryP95 time.Duration
}

// PerfResult holds API latency and throughput measurements for one storage backend.
// Written by TestStoragePerformance to bench-perf-results.csv.
type PerfResult struct {
	Timestamp   time.Time
	StorageName string
	Profile     string
	Concurrency int
	Ops         int
	CPULimit    string
	MemoryLimit string

	// ConfigMap create latency (write path → storage backend write)
	WriteP50        time.Duration
	WriteP95        time.Duration
	WriteP99        time.Duration
	WriteThroughput float64 // ops/sec
	WriteSuccesses  int
	WriteErrors     int
	WriteErrorRate  float64

	// watch-churn per-lifecycle-step error counts and first sampled error.
	// Always 0 / "" for the create-list profile.
	CreateErrors    int
	CreateFirstErr  string
	Update1Errors   int
	Update1FirstErr string
	Update2Errors   int
	Update2FirstErr string
	DeleteErrors    int
	DeleteFirstErr  string

	// ConfigMap list latency (read path → storage backend read)
	ReadP50        time.Duration
	ReadP95        time.Duration
	ReadP99        time.Duration
	ReadThroughput float64 // ops/sec
	ReadSuccesses  int
	ReadErrors     int
	ReadErrorRate  float64

	Watchers        int
	WatchEvents     int
	WatchErrors     int
	WatchReconnects int
	WatchFirstErr   string
	WatchEventRate  float64
	WatchLagP50     time.Duration
	WatchLagP95     time.Duration
	WatchLagP99     time.Duration
	WatchLagMax     time.Duration

	// Management-cluster pod resource usage sampled while the perf load runs.
	HCPResourceSamples int
	HCPP50MemMi        int64
	HCPP95MemMi        int64
	HCPMaxMemMi        int64
	HCPP50CPUm         int64
	HCPP95CPUm         int64
	HCPMaxCPUm         int64
}

// storageEntry bundles a ScenarioConfig partial with an Enabled flag.
type storageEntry struct {
	StorageName      string
	Enabled          bool
	StorageType      km.StorageType
	StorageKine      km.KineSpec
	StorageEtcd      km.EtcdSpec
	StorageNATS      km.NATSSpec
	StorageNamespace string // populated for kine-postgres / kine-mysql; empty otherwise.
}

func scaleFastProbePatches() []km.ComponentPatch {
	return []km.ComponentPatch{{
		Target: km.PatchTarget{
			Kind:      "StatefulSet",
			Component: "control-plane",
		},
		Patch: km.PatchSpec{
			Type: km.StrategicMergePatchType,
			Content: `spec:
  template:
    spec:
      containers:
        - name: controller
          readinessProbe:
            initialDelaySeconds: 0
            periodSeconds: 2
            timeoutSeconds: 5
            failureThreshold: 300
          livenessProbe:
            initialDelaySeconds: 0
            periodSeconds: 2
            timeoutSeconds: 5
            failureThreshold: 120
`,
		},
	}}
}

func hcpResources(cpuLimit, memoryLimit string) (corev1.ResourceRequirements, error) {
	resources := corev1.ResourceRequirements{}
	if cpuLimit != "" {
		q, err := resource.ParseQuantity(cpuLimit)
		if err != nil {
			return resources, err
		}
		resources.Requests = ensureResourceList(resources.Requests)
		resources.Limits = ensureResourceList(resources.Limits)
		resources.Requests[corev1.ResourceCPU] = q
		resources.Limits[corev1.ResourceCPU] = q
	}
	if memoryLimit != "" {
		q, err := resource.ParseQuantity(memoryLimit)
		if err != nil {
			return resources, err
		}
		resources.Requests = ensureResourceList(resources.Requests)
		resources.Limits = ensureResourceList(resources.Limits)
		resources.Requests[corev1.ResourceMemory] = q
		resources.Limits[corev1.ResourceMemory] = q
	}
	return resources, nil
}

func ensureResourceList(list corev1.ResourceList) corev1.ResourceList {
	if list == nil {
		return corev1.ResourceList{}
	}
	return list
}

// storageConfigs returns the full list of storage configurations.
// Postgres + MySQL run as pods in the bench-storage namespace, reachable via
// in-cluster Service DNS — see bench/infra/manifests/storage/.
func storageConfigs(k0sVersion string) []storageEntry {
	_ = k0sVersion // reserved for future version-specific config adjustments

	return []storageEntry{
		{
			StorageName: "etcd",
			Enabled:     true,
			StorageType: km.StorageTypeEtcd,
			StorageEtcd: km.EtcdSpec{
				Image: km.DefaultEtcdImage,
			},
		},
		{
			StorageName: "kine-postgres",
			Enabled:     true,
			StorageType: km.StorageTypeKine,
			StorageKine: km.KineSpec{
				DataSourceURL: "postgres://bench:benchpass@postgres.bench-storage.svc:5432/bench",
			},
			StorageNamespace: "bench-storage",
		},
		{
			StorageName: "kine-mysql",
			Enabled:     true,
			StorageType: km.StorageTypeKine,
			StorageKine: km.KineSpec{
				DataSourceURL: "mysql://bench:benchpass@tcp(mysql.bench-storage.svc:3306)/bench",
			},
			StorageNamespace: "bench-storage",
		},
		{
			StorageName: "kine-sqlite",
			Enabled:     true,
			StorageType: km.StorageTypeKine,
			StorageKine: km.KineSpec{
				DataSourceURL: "sqlite:///data/kine.db",
			},
		},
		{
			StorageName: "kine-nats-embedded",
			Enabled:     true,
			StorageType: km.StorageTypeNATS,
		},
		{
			StorageName: "kine-t4",
			Enabled:     os.Getenv("BENCH_T4_EMBEDDED_ENABLED") != "0",
			StorageType: km.StorageTypeKine,
		},
	}
}
