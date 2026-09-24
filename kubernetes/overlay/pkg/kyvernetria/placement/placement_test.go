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

package placement

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	appslisters "k8s.io/client-go/listers/apps/v1"
	"k8s.io/client-go/tools/cache"

	"k8s.io/kubernetes/pkg/kyvernetria"
)

func controllerRef(apiVersion, kind, name string) []metav1.OwnerReference {
	yes := true
	return []metav1.OwnerReference{{APIVersion: apiVersion, Kind: kind, Name: name, Controller: &yes}}
}

func TestHolderFor(t *testing.T) {
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	_ = indexer.Add(&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "api-7f9", OwnerReferences: controllerRef("apps/v1", "Deployment", "api")}})
	_ = indexer.Add(&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "rollout-1", OwnerReferences: controllerRef("argoproj.io/v1alpha1", "Rollout", "r")}})
	_ = indexer.Add(&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "bare"}})
	lister := appslisters.NewReplicaSetLister(indexer)

	for _, tc := range []struct {
		refs []metav1.OwnerReference
		want Holder
		ok   bool
	}{
		{controllerRef("apps/v1", "ReplicaSet", "api-7f9"), Holder{"Deployment", "ns", "api"}, true},
		{controllerRef("apps/v1", "ReplicaSet", "bare"), Holder{"ReplicaSet", "ns", "bare"}, true},
		{controllerRef("apps/v1", "ReplicaSet", "rollout-1"), Holder{"ReplicaSet", "ns", "rollout-1"}, true},
		{controllerRef("apps/v1", "StatefulSet", "db"), Holder{"StatefulSet", "ns", "db"}, true},
		{controllerRef("apps.kruise.io/v1beta1", "StatefulSet", "db"), Holder{}, false},
		{controllerRef("apps/v1", "DaemonSet", "agent"), Holder{}, false},
		{nil, Holder{}, false},
	} {
		pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "p", OwnerReferences: tc.refs}}
		got, ok := HolderFor(pod, lister)
		if got != tc.want || ok != tc.ok {
			t.Errorf("HolderFor(%v) = %v, %v; want %v, %v", tc.refs, got, ok, tc.want, tc.ok)
		}
	}
	if h, ok := ParseHolder(Holder{"Deployment", "ns", "api"}.String()); !ok || h != (Holder{"Deployment", "ns", "api"}) {
		t.Errorf("holder key round trip: %v %v", h, ok)
	}
}

func TestOrdinal(t *testing.T) {
	for _, tc := range []struct {
		pod  *v1.Pod
		want int
		ok   bool
	}{
		{&v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "db-2"}}, 2, true},
		{&v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "db-x", Labels: map[string]string{"apps.kubernetes.io/pod-index": "7"}}}, 7, true},
		{&v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "other-1"}}, 0, false},
		{&v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "db-a"}}, 0, false},
	} {
		got, ok := Ordinal(tc.pod, "db")
		if got != tc.want || ok != tc.ok {
			t.Errorf("Ordinal(%s) = %d, %v; want %d, %v", tc.pod.Name, got, ok, tc.want, tc.ok)
		}
	}
}

func on(nodes ...string) map[string][]int {
	out := map[string][]int{}
	for _, n := range nodes {
		out[n] = append(out[n], -1)
	}
	return out
}

func TestRecallNodes(t *testing.T) {
	if got := Recall(Deployment, "", on("b", "a")); got != "a,b" {
		t.Errorf("first memory = %q", got)
	}
	if got := Recall(Deployment, "c,a,b", on("a", "b")); got != "c,a,b" {
		t.Errorf("unchanged placement rewrote memory: %q", got)
	}
	if got := Recall(Deployment, "a,b", on("a", "d")); got != "d,a,b" {
		t.Errorf("new home not remembered first: %q", got)
	}
	if got := Recall(Deployment, "a,b", nil); got != "a,b" {
		t.Errorf("memory lost when the pods are gone: %q", got)
	}

	// Growing past the cap fades the oldest homes, never a current one.
	var prev string
	for i := 0; i < 40; i++ {
		prev = Recall(Deployment, prev, on(fmt.Sprintf("n%02d", i)))
	}
	if got := strings.Split(prev, ","); len(got) != kyvernetria.MaxRememberedNodes || got[0] != "n39" {
		t.Errorf("memory not capped or not most recent first: %v", got)
	}

	// Exactly at the cap with every node current: stable, no churn.
	var many []string
	for i := 0; i < kyvernetria.MaxRememberedNodes; i++ {
		many = append(many, fmt.Sprintf("m%02d", i))
	}
	full := Recall(Deployment, prev, on(many...))
	if again := Recall(Deployment, full, on(many...)); again != full {
		t.Errorf("recomputing changed the memory:\n%s\n%s", full, again)
	}
	if !sets.New(strings.Split(full, ",")...).HasAll(many...) {
		t.Errorf("a current home was evicted: %s", full)
	}

	// Spread over more nodes than the cap: forget instead of churning.
	if got := Recall(Deployment, full, on(append(many, "extra")...)); got != "" {
		t.Errorf("too spread to remember, got %q", got)
	}
}

func TestRecallOrdinals(t *testing.T) {
	running := map[string][]int{"a": {0}, "b": {1}}
	got := Recall(StatefulSet, "", running)
	if got != "0=a,1=b" {
		t.Fatalf("got %q", got)
	}
	if again := Recall(StatefulSet, got, running); again != got {
		t.Errorf("stable placement rewrote memory: %q", again)
	}
	// db-1 is gone: its home is kept for its return; db-0 moved.
	if moved := Recall(StatefulSet, got, map[string][]int{"c": {0}}); moved != "0=c,1=b" {
		t.Errorf("got %q", moved)
	}
	// An old node-list value is migrated.
	if migrated := Recall(StatefulSet, "a,b", running); migrated != "0=a,1=b" {
		t.Errorf("got %q", migrated)
	}
	if m := DecodeOrdinals(map[string]string{kyvernetria.RememberedNodesAnnotation: "0=a,junk,2=c"}); !reflect.DeepEqual(m, map[int]string{0: "a", 2: "c"}) {
		t.Errorf("decode = %v", m)
	}
}

func TestScore(t *testing.T) {
	nodes := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l"}
	for node, want := range map[string]int64{"a": 100, "b": 95, "k": 50, "l": 50, "zz": 0} {
		if got := Score(nodes, node); got != want {
			t.Errorf("Score(%q) = %d, want %d", node, got, want)
		}
	}
	if got := Scores([]string{"a", "b", "c"}, sets.New("a")); !reflect.DeepEqual(got, map[string]int64{"b": 95, "c": 90}) {
		t.Errorf("busy node scored: %v", got)
	}
}
