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
	"context"
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"

	"k8s.io/kubernetes/pkg/kyvernetria"
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
	c := &Controller{recorder: rec}
	node := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-2"}}
	state := sets.New[v1.ResourceName]()

	for _, pct := range []int64{50, 72, 90, 68, 71, 60, 80} {
		c.feel(node, v1.ResourceCPU, pct, state)
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

func busyNode(name, annotation string) *v1.Node {
	n := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status:     v1.NodeStatus{Allocatable: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")}},
	}
	if annotation != "" {
		n.Annotations = map[string]string{kyvernetria.WorriedAnnotation: annotation}
	}
	return n
}

func newController(t *testing.T, rec record.EventRecorder, nodes []*v1.Node, pods []*v1.Pod) (*Controller, *fake.Clientset, informers.SharedInformerFactory) {
	t.Helper()
	var objs []runtime.Object
	for _, n := range nodes {
		objs = append(objs, n)
	}
	client := fake.NewSimpleClientset(objs...)
	f := informers.NewSharedInformerFactory(client, 0)
	for _, n := range nodes {
		_ = f.Core().V1().Nodes().Informer().GetStore().Add(n)
	}
	for _, p := range pods {
		_ = f.Core().V1().Pods().Informer().GetStore().Add(p)
	}
	client.ClearActions()
	return New(client, f.Core().V1().Nodes(), f.Core().V1().Pods(), rec), client, f
}

func TestWorryIsPersistedAndRestored(t *testing.T) {
	rec := record.NewFakeRecorder(10)
	nodes := []*v1.Node{busyNode("worker-1", "")}
	pods := []*v1.Pod{pod("worker-1", "900m")}
	c, client, _ := newController(t, rec, nodes, pods)
	if err := c.check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(rec.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(rec.Events))
	}
	updated, err := client.CoreV1().Nodes().Get(context.Background(), "worker-1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := updated.Annotations[kyvernetria.WorriedAnnotation]; got != "cpu" {
		t.Fatalf("worry not persisted: %q", got)
	}

	// A new leader starts from the node's annotation: no second Worried event.
	rec2 := record.NewFakeRecorder(10)
	c2, client2, _ := newController(t, rec2, []*v1.Node{updated}, pods)
	if err := c2.check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(rec2.Events) != 0 {
		t.Errorf("a new leader repeated the worry: %v", <-rec2.Events)
	}
	for _, a := range client2.Actions() {
		if a.GetVerb() == "patch" {
			t.Errorf("an unchanged worry was written again: %v", a)
		}
	}
}

func TestReliefClearsAnnotation(t *testing.T) {
	rec := record.NewFakeRecorder(10)
	c, client, _ := newController(t, rec, []*v1.Node{busyNode("worker-1", "cpu")}, []*v1.Pod{pod("worker-1", "100m")})
	if err := c.check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e := <-rec.Events; !strings.Contains(e, "Relieved") {
		t.Errorf("unexpected event %q", e)
	}
	updated, _ := client.CoreV1().Nodes().Get(context.Background(), "worker-1", metav1.GetOptions{})
	if _, ok := updated.Annotations[kyvernetria.WorriedAnnotation]; ok {
		t.Errorf("annotation left behind: %v", updated.Annotations)
	}
}

func TestDeletedNodesAreForgotten(t *testing.T) {
	rec := record.NewFakeRecorder(10)
	c, _, f := newController(t, rec, []*v1.Node{busyNode("worker-1", ""), busyNode("worker-2", "")}, nil)
	if err := c.check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(c.worried) != 2 {
		t.Fatalf("tracking %d nodes", len(c.worried))
	}
	_ = f.Core().V1().Nodes().Informer().GetStore().Delete(busyNode("worker-2", ""))
	if err := c.check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.worried["worker-2"]; ok || len(c.worried) != 1 {
		t.Errorf("deleted node still tracked: %v", c.worried)
	}
}
