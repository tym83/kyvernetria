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

package worry

import (
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
)

func pod(node, cpu string) *v1.Pod {
	return &v1.Pod{
		Spec: v1.PodSpec{NodeName: node, Containers: []v1.Container{{
			Resources: v1.ResourceRequirements{Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse(cpu)}},
		}}},
		Status: v1.PodStatus{Phase: v1.PodRunning},
	}
}

func TestRequestedAndPercent(t *testing.T) {
	pods := []*v1.Pod{pod("a", "500m"), pod("a", "1"), pod("b", "100m"), pod("", "4")}
	done := pod("a", "8")
	done.Status.Phase = v1.PodSucceeded
	req := Requested(append(pods, done))
	if got := Percent(req["a"][v1.ResourceCPU], resource.MustParse("2")); got != 75 {
		t.Errorf("node a at %d%%, want 75%%", got)
	}
	if got := Percent(req["b"][v1.ResourceCPU], resource.MustParse("2")); got != 5 {
		t.Errorf("node b at %d%%, want 5%%", got)
	}
	if got := Percent(resource.MustParse("1"), resource.Quantity{}); got != 0 {
		t.Errorf("zero allocatable gave %d%%", got)
	}
}

func TestFeelHasHysteresis(t *testing.T) {
	rec := record.NewFakeRecorder(10)
	c := &Controller{recorder: rec, worried: map[string]bool{}}
	node := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-2"}}

	for _, pct := range []int64{50, 72, 90, 68, 71, 60, 80} {
		c.feel(node, v1.ResourceCPU, pct)
	}
	close(rec.Events)
	var events []string
	for e := range rec.Events {
		events = append(events, e)
	}
	// 72 worries, 90/68/71 stay worried (68 is within hysteresis), 60 relieves, 80 worries again.
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3: %v", len(events), events)
	}
	if !strings.Contains(events[0], "Worried") || !strings.Contains(events[0], "72%") ||
		!strings.Contains(events[1], "Relieved") || !strings.Contains(events[2], "Worried") {
		t.Errorf("unexpected events: %v", events)
	}
}
