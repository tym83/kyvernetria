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

package kyvctl

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"k8s.io/kubernetes/pkg/kyvernetria"
	api "k8s.io/kubernetes/pkg/kyvernetria/gestation"
)

var conceivedAt = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

func dynWith(t *testing.T, gs ...*api.Gestation) *dynamicfake.FakeDynamicClient {
	t.Helper()
	var objs []runtime.Object
	for _, g := range gs {
		u, err := api.ToUnstructured(g)
		if err != nil {
			t.Fatal(err)
		}
		objs = append(objs, u)
	}
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{api.GVR: "GestationList"}, objs...)
}

func TestConceiveValidates(t *testing.T) {
	dyn := dynWith(t)
	ok := ConceiveOptions{Due: "2026-11-01", Size: "500m/512Mi", Replicas: 3}
	for name, tc := range map[string]struct {
		o    ConceiveOptions
		want string
	}{
		"bad date":   {ConceiveOptions{Due: "1 Nov", Size: "1/1Gi", Replicas: 1}, "--due needs a date"},
		"past":       {ConceiveOptions{Due: "2026-10-01", Size: "1/1Gi", Replicas: 1}, "not in the future"},
		"no slash":   {ConceiveOptions{Due: "2026-11-01", Size: "1Gi", Replicas: 1}, "CPU/MEMORY"},
		"bad size":   {ConceiveOptions{Due: "2026-11-01", Size: "lots/1Gi", Replicas: 1}, "--size"},
		"zero size":  {ConceiveOptions{Due: "2026-11-01", Size: "0/1Gi", Replicas: 1}, "positive"},
		"no replica": {ConceiveOptions{Due: "2026-11-01", Size: "1/1Gi", Replicas: 0}, "--replicas"},
	} {
		if _, err := Conceive(context.Background(), dyn, "shop", "web", tc.o, conceivedAt); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	g, err := Conceive(context.Background(), dyn, "shop", "web", ok, conceivedAt)
	if err != nil {
		t.Fatal(err)
	}
	if g.Spec.Deployment != "web" || g.Spec.Size.Memory != "512Mi" || !g.Spec.RollbackEnabled() {
		t.Errorf("conceived %+v", g.Spec)
	}
	var out bytes.Buffer
	renderConceived(&out, g, conceivedAt)
	if !strings.Contains(out.String(), "Conceived shop/web, due 2026-11-01 (in 28 days)") {
		t.Errorf("conceive output:\n%s", out.String())
	}
	if _, err := Conceive(context.Background(), dyn, "shop", "web", ok, conceivedAt); err == nil || !strings.Contains(err.Error(), "already on its way") {
		t.Errorf("conceiving twice: %v", err)
	}
	no, err := Conceive(context.Background(), dyn, "shop", "api", ConceiveOptions{Due: "2026-11-01", Size: "1/1Gi", Replicas: 1, NoRollback: true, Deployment: "api-v2"}, conceivedAt)
	if err != nil || no.Spec.RollbackEnabled() || no.Spec.Deployment != "api-v2" {
		t.Errorf("--no-rollback / --deployment: %+v %v", no, err)
	}
}

func gest(name string, status api.Status) *api.Gestation {
	return &api.Gestation{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop"},
		Spec: api.Spec{Deployment: name, Due: "2026-11-01", Size: api.Size{CPU: "1", Memory: "1Gi"}, Replicas: 1}, Status: status}
}

func TestDeliverRefusesWhatIsNotABirth(t *testing.T) {
	born := api.Status{Phase: api.PhaseGrown, Birth: &api.Birth{Time: metav1.NewTime(conceivedAt)}, Delivery: 1}
	rolled := api.Status{Phase: api.PhaseRolledBack, Birth: &api.Birth{Time: metav1.NewTime(conceivedAt), RolledBackTo: "3"}, Delivery: 1}
	dyn := dynWith(t, gest("new", api.Status{Phase: api.PhaseExpecting}), gest("grown", born), gest("rolled", rolled))
	ctx := context.Background()
	if g, err := Deliver(ctx, dyn, "shop", "new", false); err != nil || g.Spec.Delivery != 1 {
		t.Errorf("first delivery: %v %v", g, err)
	}
	if _, err := Deliver(ctx, dyn, "shop", "grown", false); err == nil || !strings.Contains(err.Error(), "A new rollout is not a new birth") {
		t.Errorf("delivering a grown service: %v", err)
	}
	if _, err := Deliver(ctx, dyn, "shop", "rolled", false); err == nil || !strings.Contains(err.Error(), "--again") {
		t.Errorf("delivering after a rollback without --again: %v", err)
	}
	if g, err := Deliver(ctx, dyn, "shop", "rolled", true); err != nil || g.Spec.Delivery != 2 {
		t.Errorf("--again: %v %v", g, err)
	}
	if _, err := Deliver(ctx, dyn, "shop", "missing", false); err == nil || !strings.Contains(err.Error(), "there is no gestation missing") {
		t.Errorf("unknown gestation: %v", err)
	}
}

func TestApgarRendering(t *testing.T) {
	g := gest("web", api.Status{Phase: api.PhaseNewbornCare, Birth: &api.Birth{Time: metav1.NewTime(conceivedAt), Revision: "4"},
		Apgar: []api.Apgar{
			api.Score(1, api.Signs{Desired: 3, Ready: 2, Running: 3, Services: 1, ReadyEndpoint: 2}),
			api.Score(5, api.Signs{Desired: 3, Ready: 3, Running: 3, Services: 1, ReadyEndpoint: 3}),
		}})
	dyn := dynWith(t, g)
	found, err := findGestation(context.Background(), dyn, "shop", "deploy/web")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	renderApgar(&out, found)
	for _, want := range []string{"1 min", "5 min", "Appearance", "ready 3/3", "Total", "web arrived. Apgar 10 at five minutes. Welcome."} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("apgar output lacks %q:\n%s", want, out.String())
		}
	}
	if _, err := findGestation(context.Background(), dyn, "shop", "deploy/api"); err == nil {
		t.Error("found a gestation for a deployment nobody conceived")
	}
	out.Reset()
	renderApgar(&out, gest("x", api.Status{Phase: api.PhaseExpecting}))
	if !strings.Contains(out.String(), "isn't born yet") {
		t.Errorf("unborn: %s", out.String())
	}
}

func TestMemoryRendering(t *testing.T) {
	data := map[string]string{}
	api.Put(data, api.Record{Namespace: "shop", Name: "old", UID: "u1", Born: "2026-01-02T00:00:00Z", Left: "2026-09-01T00:00:00Z",
		Owners: []string{"alice", "bob"}, Dependencies: []string{"svc/db"}, ConfigDigest: "sha256:abc"})
	api.Put(data, api.Record{Namespace: "shop", Name: "web", UID: "u2", Born: "2026-10-01T00:00:00Z"})
	kube := fake.NewSimpleClientset(&v1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: api.MemoryConfigMap, Namespace: kyvernetria.SystemNamespace}, Data: data})
	records, err := Memory(context.Background(), kube)
	if err != nil || len(records) != 2 {
		t.Fatalf("records %v %v", records, err)
	}
	var out bytes.Buffer
	renderMemory(&out, records, false)
	s := out.String()
	if !strings.Contains(s, "shop/old") || strings.Contains(s, "shop/web") || !strings.Contains(s, "alice, bob") ||
		!strings.Contains(s, "You were here once. I keep a little of you.") {
		t.Errorf("remember:\n%s", s)
	}
	out.Reset()
	renderMemory(&out, records, true)
	if !strings.Contains(out.String(), "still here") {
		t.Errorf("remember --all:\n%s", out.String())
	}
	if none, err := Memory(context.Background(), fake.NewSimpleClientset()); err != nil || none != nil {
		t.Errorf("no memory yet: %v %v", none, err)
	}
}

func TestGrowthWithoutMetrics(t *testing.T) {
	g := gest("web", api.Status{Birth: &api.Birth{Selector: "app=web"}, GrowthNote: "No measurements: the cluster serves no metrics API"})
	var out bytes.Buffer
	if err := Growth(context.Background(), fake.NewSimpleClientset(), nil, g, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "serves no metrics API") {
		t.Errorf("growth without metrics:\n%s", out.String())
	}
	for i := 0; i < 8; i++ {
		g.Status.Growth = append(g.Status.Growth, api.Sample{Time: metav1.NewTime(conceivedAt.Add(time.Duration(i) * time.Hour)),
			CPUMilli: int64(100 + 10*i), MemoryBytes: 200 << 20})
	}
	out.Reset()
	if err := Growth(context.Background(), fake.NewSimpleClientset(), nil, g, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"8 measurements over 7 hours", "P97", "CPU: ", "Memory: "} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("growth chart lacks %q:\n%s", want, out.String())
		}
	}
}

func TestGestationCommandsAreOwn(t *testing.T) {
	for _, c := range []string{"conceive", "screen", "deliver", "apgar", "growth"} {
		if !invokesOwnCommand([]string{"kyvctl", c, "web", "-n", "shop"}) {
			t.Errorf("%s is not routed to kyvctl", c)
		}
	}
}
