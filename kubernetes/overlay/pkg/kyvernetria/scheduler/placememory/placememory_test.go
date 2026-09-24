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

package placememory

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	appslisters "k8s.io/client-go/listers/apps/v1"
	"k8s.io/client-go/tools/cache"
	fwk "k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework"

	"k8s.io/kubernetes/pkg/kyvernetria"
)

func owned(kind, name string) []metav1.OwnerReference {
	yes := true
	return []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: kind, Name: name, UID: types.UID("uid-" + name), Controller: &yes}}
}

func pod(name, kind, owner string) *v1.Pod {
	return &v1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: name, UID: types.UID(name), OwnerReferences: owned(kind, owner)}}
}

func nodeInfo(name string, pods ...*v1.Pod) fwk.NodeInfo {
	ni := framework.NewNodeInfo(pods...)
	ni.SetNode(&v1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}})
	return ni
}

func newPlugin(annotation string) *PlaceMemory {
	rs := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	_ = rs.Add(&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "api-new", OwnerReferences: owned("Deployment", "api")}})
	_ = rs.Add(&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "api-old", OwnerReferences: owned("Deployment", "api")}})
	_ = rs.Add(&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "web-1", OwnerReferences: owned("Deployment", "web")}})
	deploys := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	sts := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	a := map[string]string{kyvernetria.RememberedNodesAnnotation: annotation}
	_ = deploys.Add(&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "api", Annotations: a}})
	_ = sts.Add(&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "db", Annotations: a}})
	return &PlaceMemory{
		replicaSets:  appslisters.NewReplicaSetLister(rs),
		deployments:  appslisters.NewDeploymentLister(deploys),
		statefulSets: appslisters.NewStatefulSetLister(sts),
	}
}

func scoreAll(t *testing.T, pl *PlaceMemory, p *v1.Pod, nodes []fwk.NodeInfo) (map[string]int64, bool) {
	t.Helper()
	state := framework.NewCycleState()
	if status := pl.PreScore(context.Background(), state, p, nodes); status.IsSkip() {
		return nil, true
	} else if !status.IsSuccess() {
		t.Fatalf("PreScore: %v", status)
	}
	out := map[string]int64{}
	for _, ni := range nodes {
		s, status := pl.Score(context.Background(), state, p, ni)
		if !status.IsSuccess() {
			t.Fatalf("Score: %v", status)
		}
		out[ni.Node().Name] = s
	}
	return out, false
}

func TestDeploymentReplacementReturnsHomeWithoutPilingUp(t *testing.T) {
	pl := newPlugin("a,b,c")
	nodes := []fwk.NodeInfo{
		nodeInfo("a", pod("api-new-1", "ReplicaSet", "api-new")), // a sibling runs here
		nodeInfo("b", pod("api-old-1", "ReplicaSet", "api-old")), // a sibling from the previous rollout
		nodeInfo("c", pod("web-1-1", "ReplicaSet", "web-1")),     // another workload only
		nodeInfo("d"),
	}
	got, skipped := scoreAll(t, pl, pod("api-new-2", "ReplicaSet", "api-new"), nodes)
	if skipped {
		t.Fatal("PreScore skipped a remembered workload")
	}
	if got["a"] != 0 || got["b"] != 0 {
		t.Errorf("nodes with running replicas got points: %v", got)
	}
	if got["c"] != 90 || got["d"] != 0 {
		t.Errorf("scores = %v, want the empty remembered home c to win", got)
	}

	leaving := pod("api-new-1", "ReplicaSet", "api-new")
	now := metav1.Now()
	leaving.DeletionTimestamp = &now
	got, _ = scoreAll(t, pl, pod("api-new-3", "ReplicaSet", "api-new"), []fwk.NodeInfo{nodeInfo("a", leaving)})
	if got["a"] != 100 {
		t.Errorf("the home of a terminating replica was treated as occupied: %v", got)
	}
}

func TestStatefulSetScoresOnlyItsOwnOrdinal(t *testing.T) {
	pl := newPlugin("0=a,1=b")
	nodes := []fwk.NodeInfo{nodeInfo("a"), nodeInfo("b"), nodeInfo("c")}
	got, _ := scoreAll(t, pl, pod("db-1", "StatefulSet", "db"), nodes)
	if got["b"] != 100 || got["a"] != 0 || got["c"] != 0 {
		t.Errorf("scores = %v, want only db-1's node b", got)
	}
	if _, skipped := scoreAll(t, pl, pod("db-2", "StatefulSet", "db"), nodes); !skipped {
		t.Error("an ordinal with no memory was not skipped")
	}
}

func TestSkipWithoutMemory(t *testing.T) {
	nodes := []fwk.NodeInfo{nodeInfo("a")}
	if _, skipped := scoreAll(t, newPlugin(""), pod("api-new-1", "ReplicaSet", "api-new"), nodes); !skipped {
		t.Error("a workload without memory was not skipped")
	}
	if _, skipped := scoreAll(t, newPlugin("a"), &v1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "bare"}}, nodes); !skipped {
		t.Error("a bare pod was not skipped")
	}
	// Every remembered node is occupied by a replica: nothing to add.
	if _, skipped := scoreAll(t, newPlugin("a"), pod("api-new-2", "ReplicaSet", "api-new"),
		[]fwk.NodeInfo{nodeInfo("a", pod("api-new-1", "ReplicaSet", "api-new"))}); !skipped {
		t.Error("PreScore did not skip when every home is occupied")
	}
}
