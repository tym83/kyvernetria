/*
Copyright 2026 The Kyvernetria Authors.

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

package relationships

import (
	"fmt"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
)

// BenchmarkGraph is a busy namespace: 500 services, 500 workloads with 20
// texts each (env values and args), a few of which address services.
func BenchmarkGraph(b *testing.B) {
	var services []*v1.Service
	for i := 0; i < 500; i++ {
		name := fmt.Sprintf("svc-%d", i)
		services = append(services, svc("bench", name, map[string]string{"app": name}))
	}
	var deploys []*appsv1.Deployment
	for i := 0; i < 500; i++ {
		env := map[string]string{}
		for j := 0; j < 18; j++ {
			env[fmt.Sprintf("SETTING_%d", j)] = fmt.Sprintf("value-%d-%d --mode=fast worker", i, j)
		}
		env["UPSTREAM_URL"] = fmt.Sprintf("http://svc-%d:8080/v1", (i+1)%500)
		deploys = append(deploys, deploy("bench", fmt.Sprintf("svc-%d", i), map[string]string{"app": fmt.Sprintf("svc-%d", i)},
			env, fmt.Sprintf("--peer=svc-%d.bench.svc.cluster.local:9000", (i+2)%500)))
	}
	workloads := Workloads(deploys, nil, nil)
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		if got := len(Graph(services, workloads)); got != 1500 {
			b.Fatalf("got %d relationships, want 1500", got)
		}
	}
}
