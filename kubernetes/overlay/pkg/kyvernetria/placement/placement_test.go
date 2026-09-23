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
	"reflect"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	appslisters "k8s.io/client-go/listers/apps/v1"
)

func controllerRef(kind, name string) []metav1.OwnerReference {
	yes := true
	return []metav1.OwnerReference{{Kind: kind, Name: name, Controller: &yes}}
}

func TestHolderFor(t *testing.T) {
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	_ = indexer.Add(&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "api-7f9", OwnerReferences: controllerRef("Deployment", "api")}})
	_ = indexer.Add(&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "bare"}})
	lister := appslisters.NewReplicaSetLister(indexer)

	for _, tc := range []struct {
		refs []metav1.OwnerReference
		want Holder
		ok   bool
	}{
		{controllerRef("ReplicaSet", "api-7f9"), Holder{"Deployment", "ns", "api"}, true},
		{controllerRef("ReplicaSet", "bare"), Holder{"ReplicaSet", "ns", "bare"}, true},
		{controllerRef("StatefulSet", "db"), Holder{"StatefulSet", "ns", "db"}, true},
		{controllerRef("DaemonSet", "agent"), Holder{}, false},
		{nil, Holder{}, false},
	} {
		pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "p", OwnerReferences: tc.refs}}
		got, ok := HolderFor(pod, lister)
		if got != tc.want || ok != tc.ok {
			t.Errorf("HolderFor(%v) = %v, %v; want %v, %v", tc.refs, got, ok, tc.want, tc.ok)
		}
	}
}

func TestRemember(t *testing.T) {
	nodes, changed := Remember(nil, "a")
	if !changed || !reflect.DeepEqual(nodes, []string{"a"}) {
		t.Fatalf("got %v %v", nodes, changed)
	}
	nodes, changed = Remember([]string{"a", "b"}, "a")
	if changed || !reflect.DeepEqual(nodes, []string{"a", "b"}) {
		t.Fatalf("remembering the current home changed memory: %v", nodes)
	}
	nodes, changed = Remember([]string{"a", "b", "c"}, "c")
	if changed || !reflect.DeepEqual(nodes, []string{"a", "b", "c"}) {
		t.Fatalf("an already remembered node was reordered: %v", nodes)
	}
	nodes, _ = Remember([]string{"a", "b"}, "c")
	if !reflect.DeepEqual(nodes, []string{"c", "a", "b"}) {
		t.Fatalf("got %v", nodes)
	}
	var many []string
	for i := 0; i < 40; i++ {
		many, _ = Remember(many, string(rune('A'+i)))
	}
	if len(many) != 16 {
		t.Fatalf("memory not capped: %d", len(many))
	}
	if Encode(Decode(map[string]string{"kyvernetria.io/remembered-nodes": "x,y"})) != "x,y" {
		t.Fatal("encode/decode round trip failed")
	}
}

func TestScore(t *testing.T) {
	nodes := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l"}
	for node, want := range map[string]int64{"a": 100, "b": 95, "k": 50, "l": 50, "zz": 0} {
		if got := Score(nodes, node); got != want {
			t.Errorf("Score(%q) = %d, want %d", node, got, want)
		}
	}
}
