/*
Copyright 2023.

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

package k0smotronio

import (
	"github.com/stretchr/testify/assert"
	"strings"
	"testing"

	km "github.com/k0sproject/k0smotron/api/k0smotron.io/v1beta1"
)

func TestEtcd_calculateDesiredReplicas(t *testing.T) {
	var tests = []struct {
		cluster *km.Cluster
		want    int32
	}{
		{cluster: &km.Cluster{}, want: 1},
		{cluster: &km.Cluster{Spec: km.ClusterSpec{Replicas: 1}}, want: 1},
		{cluster: &km.Cluster{Spec: km.ClusterSpec{Replicas: 2}}, want: 3},
		{cluster: &km.Cluster{Spec: km.ClusterSpec{Replicas: 3}}, want: 3},
		{cluster: &km.Cluster{Spec: km.ClusterSpec{Replicas: 4}}, want: 5},
		{cluster: &km.Cluster{Spec: km.ClusterSpec{Replicas: 5}}, want: 5},
	}

	for _, tc := range tests {
		t.Run("", func(t *testing.T) {
			got := calculateDesiredReplicas(tc.cluster)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestEtcd_generateEtcdStatefulSet(t *testing.T) {
	var tests = []struct {
		cluster *km.Cluster
		want    []string
	}{
		{
			cluster: &km.Cluster{},
			want: []string{
				"--auto-compaction-mode=periodic",
				"--auto-compaction-retention=5m",
				"--snapshot-count=10000",
			}},
		{
			cluster: &km.Cluster{Spec: km.ClusterSpec{Etcd: km.EtcdSpec{Args: []string{
				"--auto-compaction-mode=periodic",
			}}}},
			want: []string{
				"--auto-compaction-mode=periodic",
				"--auto-compaction-retention=5m",
				"--snapshot-count=10000",
			}},
		{
			cluster: &km.Cluster{Spec: km.ClusterSpec{Etcd: km.EtcdSpec{Args: []string{
				"--auto-compaction-mode=periodic",
				"--auto-compaction-retention=2h",
				"--snapshot-count=50000",
			}}}},
			want: []string{
				"--auto-compaction-mode=periodic",
				"--auto-compaction-retention=2h",
				"--snapshot-count=50000",
			}},
	}

	for _, tc := range tests {
		t.Run("", func(t *testing.T) {
			r := new(ClusterReconciler)
			sts := r.generateEtcdStatefulSet(tc.cluster, nil, 1)
			for _, w := range tc.want {
				assert.True(t, strings.Contains(sts.Spec.Template.Spec.Containers[0].Args[1], w))
			}
		})
	}
}
