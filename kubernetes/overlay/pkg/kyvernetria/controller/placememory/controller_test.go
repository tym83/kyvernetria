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
	"encoding/json"
	"fmt"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"k8s.io/kubernetes/pkg/kyvernetria"
	"k8s.io/kubernetes/pkg/kyvernetria/placement"
)

func ownedBy(kind, name string) []metav1.OwnerReference {
	yes := true
	return []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: kind, Name: name, UID: types.UID("uid-" + name), Controller: &yes}}
}

func runningPod(name, owner, node string) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: name, OwnerReferences: ownedBy(owner, ownerName(owner, name))},
		Spec:       v1.PodSpec{NodeName: node},
		Status:     v1.PodStatus{Phase: v1.PodRunning},
	}
}

func ownerName(kind, _ string) string {
	if kind == "ReplicaSet" {
		return "api-7f9"
	}
	return "db"
}

type fixture struct {
	client *fake.Clientset
	c      *Controller
}

func newFixture(t *testing.T, annotation string, pods ...*v1.Pod) *fixture {
	t.Helper()
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "api", UID: "uid-api", ResourceVersion: "7"}}
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "db", UID: "uid-db", ResourceVersion: "7"}}
	if annotation != "" {
		d.Annotations = map[string]string{kyvernetria.RememberedNodesAnnotation: annotation}
		sts.Annotations = map[string]string{kyvernetria.RememberedNodesAnnotation: annotation}
	}
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "api-7f9", UID: "uid-api-7f9", OwnerReferences: ownedBy("Deployment", "api")}}
	objs := []runtime.Object{d, sts, rs}
	client := fake.NewSimpleClientset(objs...)
	f := informers.NewSharedInformerFactory(client, 0)
	c, err := New(client, f.Core().V1().Pods(), f.Apps().V1().ReplicaSets(), f.Apps().V1().Deployments(), f.Apps().V1().StatefulSets())
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Apps().V1().Deployments().Informer().GetStore().Add(d)
	_ = f.Apps().V1().StatefulSets().Informer().GetStore().Add(sts)
	_ = f.Apps().V1().ReplicaSets().Informer().GetStore().Add(rs)
	for _, p := range pods {
		_ = f.Core().V1().Pods().Informer().GetStore().Add(p)
	}
	client.ClearActions()
	return &fixture{client: client, c: c}
}

// patches returns the annotation value and resourceVersion of each patch.
func (f *fixture) patches(t *testing.T) [][2]interface{} {
	t.Helper()
	var out [][2]interface{}
	for _, a := range f.client.Actions() {
		p, ok := a.(clienttesting.PatchAction)
		if !ok {
			continue
		}
		var body struct {
			Metadata struct {
				ResourceVersion string                 `json:"resourceVersion"`
				Annotations     map[string]interface{} `json:"annotations"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal(p.GetPatch(), &body); err != nil {
			t.Fatal(err)
		}
		out = append(out, [2]interface{}{body.Metadata.Annotations[kyvernetria.RememberedNodesAnnotation], body.Metadata.ResourceVersion})
	}
	return out
}

func TestSyncWritesOnlyOnChange(t *testing.T) {
	f := newFixture(t, "", runningPod("api-7f9-a", "ReplicaSet", "n1"), runningPod("api-7f9-b", "ReplicaSet", "n2"))
	key := placement.Holder{Kind: "Deployment", Namespace: "ns", Name: "api"}.String()
	if err := f.c.sync(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	got := f.patches(t)
	if len(got) != 1 || got[0][0] != "n1,n2" || got[0][1] != "7" {
		t.Fatalf("patches = %v, want one guarded write of n1,n2", got)
	}

	f = newFixture(t, "n1,n2,n0", runningPod("api-7f9-a", "ReplicaSet", "n1"), runningPod("api-7f9-b", "ReplicaSet", "n2"))
	if err := f.c.sync(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	if got := f.patches(t); len(got) != 0 {
		t.Errorf("unchanged memory was written: %v", got)
	}
}

func TestSyncClearsWhenTooSpread(t *testing.T) {
	var pods []*v1.Pod
	for i := 0; i <= kyvernetria.MaxRememberedNodes; i++ {
		pods = append(pods, runningPod(fmt.Sprintf("api-7f9-%d", i), "ReplicaSet", fmt.Sprintf("n%d", i)))
	}
	f := newFixture(t, "n1,n2", pods...)
	if err := f.c.sync(context.Background(), placement.Holder{Kind: "Deployment", Namespace: "ns", Name: "api"}.String()); err != nil {
		t.Fatal(err)
	}
	if got := f.patches(t); len(got) != 1 || got[0][0] != nil {
		t.Errorf("patches = %v, want the annotation removed", got)
	}
}

func TestSyncStatefulSetRemembersOrdinals(t *testing.T) {
	f := newFixture(t, "", runningPod("db-0", "StatefulSet", "n1"), runningPod("db-1", "StatefulSet", "n2"))
	if err := f.c.sync(context.Background(), placement.Holder{Kind: "StatefulSet", Namespace: "ns", Name: "db"}.String()); err != nil {
		t.Fatal(err)
	}
	if got := f.patches(t); len(got) != 1 || got[0][0] != "0=n1,1=n2" {
		t.Errorf("patches = %v", got)
	}
}

func TestSyncRetriesFromLiveObjectOnConflict(t *testing.T) {
	f := newFixture(t, "", runningPod("api-7f9-a", "ReplicaSet", "n1"))
	conflicted := false
	f.client.PrependReactor("patch", "deployments", func(clienttesting.Action) (bool, runtime.Object, error) {
		if conflicted {
			return false, nil, nil
		}
		conflicted = true
		// Someone else wrote a memory meanwhile.
		d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "api", ResourceVersion: "8",
			Annotations: map[string]string{kyvernetria.RememberedNodesAnnotation: "n9"}}}
		if err := f.client.Tracker().Update(appsv1.SchemeGroupVersion.WithResource("deployments"), d, "ns"); err != nil {
			t.Fatal(err)
		}
		return true, nil, apierrors.NewConflict(appsv1.Resource("deployments"), "api", fmt.Errorf("modified"))
	})
	if err := f.c.sync(context.Background(), placement.Holder{Kind: "Deployment", Namespace: "ns", Name: "api"}.String()); err != nil {
		t.Fatal(err)
	}
	got := f.patches(t)
	if len(got) != 2 || got[1][0] != "n1,n9" || got[1][1] != "8" {
		t.Errorf("patches = %v, want a retry based on the live object", got)
	}
}

func TestEnqueueSkipsRememberedPods(t *testing.T) {
	f := newFixture(t, "n1", runningPod("api-7f9-a", "ReplicaSet", "n1"))
	f.c.enqueuePod(runningPod("api-7f9-a", "ReplicaSet", "n1"))
	if f.c.queue.Len() != 0 {
		t.Error("a pod already remembered where it runs was queued")
	}
	f.c.enqueuePod(runningPod("api-7f9-b", "ReplicaSet", "n2"))
	f.c.enqueuePod(runningPod("api-7f9-c", "ReplicaSet", "n3"))
	if f.c.queue.Len() != 1 {
		t.Errorf("queue has %d keys, want the holder once", f.c.queue.Len())
	}
}
