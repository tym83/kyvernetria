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

package gestation

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	policyv1 "k8s.io/api/policy/v1"
	storagev1 "k8s.io/api/storage/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	appslisters "k8s.io/client-go/listers/apps/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	discoverylisters "k8s.io/client-go/listers/discovery/v1"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	metricsfake "k8s.io/metrics/pkg/client/clientset/versioned/fake"
	"k8s.io/utils/ptr"

	"k8s.io/kubernetes/pkg/kyvernetria"
	api "k8s.io/kubernetes/pkg/kyvernetria/gestation"
)

var t0 = time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)

// stubLookup clears every screening check.
type stubLookup struct{}

func (stubLookup) SecretExists(context.Context, string, string) (bool, error)    { return true, nil }
func (stubLookup) ConfigMapExists(context.Context, string, string) (bool, error) { return true, nil }
func (stubLookup) PVC(string, string) (*v1.PersistentVolumeClaim, error)         { return nil, nil }
func (stubLookup) StorageClass(string) (*storagev1.StorageClass, error)          { return nil, nil }
func (stubLookup) ResourceQuotas(string) ([]*v1.ResourceQuota, error)            { return nil, nil }
func (stubLookup) PDBs(string) ([]*policyv1.PodDisruptionBudget, error)          { return nil, nil }
func (stubLookup) Image(context.Context, string) (string, string)                { return api.ResultClear, "ok" }

type harness struct {
	t                                    *testing.T
	ctx                                  context.Context
	now                                  time.Time
	kube                                 *fake.Clientset
	dyn                                  *dynamicfake.FakeDynamicClient
	c                                    *Controller
	recorder                             *record.FakeRecorder
	rs, pods, svcs, slices, nss, deploys cache.Indexer
}

func indexer() cache.Indexer {
	return cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
}

func established() *apiextensionsv1.CustomResourceDefinition {
	crd := api.CRD()
	crd.Status.Conditions = []apiextensionsv1.CustomResourceDefinitionCondition{{Type: apiextensionsv1.Established, Status: apiextensionsv1.ConditionTrue}}
	return crd
}

func template(image string) v1.PodTemplateSpec {
	probe := &v1.Probe{ProbeHandler: v1.ProbeHandler{HTTPGet: &v1.HTTPGetAction{Path: "/"}}}
	return v1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "web"}},
		Spec: v1.PodSpec{Containers: []v1.Container{{
			Name: "web", Image: image, ReadinessProbe: probe, LivenessProbe: probe,
			Resources: v1.ResourceRequirements{Requests: v1.ResourceList{
				v1.ResourceCPU: resource.MustParse("250m"), v1.ResourceMemory: resource.MustParse("256Mi"),
			}},
		}}},
	}
}

func newHarness(t *testing.T) *harness {
	h := &harness{t: t, ctx: context.Background(), now: t0, recorder: record.NewFakeRecorder(100),
		rs: indexer(), pods: indexer(), svcs: indexer(), slices: indexer(), nss: indexer(), deploys: indexer()}
	ns := &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop"}}
	// web runs revision 1 (v1); revision 2 (v2) is applied, paused.
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop", UID: "web-uid", Annotations: map[string]string{revisionAnnotation: "1"}},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](2), Paused: true,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
			Template: template("ghcr.io/shop/web:v2"),
		},
	}
	h.kube = fake.NewSimpleClientset(ns, d)
	h.must(h.nss.Add(ns))
	h.must(h.deploys.Add(d))
	h.addReplicaSet("web-v1", "1", "ghcr.io/shop/web:v1")

	scheme := runtime.NewScheme()
	g := &api.Gestation{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop", UID: "gest-uid", CreationTimestamp: metav1.NewTime(t0)},
		Spec: api.Spec{Deployment: "web", Due: t0.Add(90 * 24 * time.Hour).Format(time.DateOnly),
			Size: api.Size{CPU: "250m", Memory: "256Mi"}, Replicas: 2},
	}
	u, err := api.ToUnstructured(g)
	h.must(err)
	h.dyn = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		api.GVR: "GestationList", relationshipsGVR: "RelationshipList",
	}, u)

	h.c = &Controller{
		client:      h.kube,
		crds:        apiextensionsfake.NewSimpleClientset(established()),
		dynamic:     h.dyn,
		lookup:      stubLookup{},
		deployments: appslisters.NewDeploymentLister(h.deploys),
		replicaSets: appslisters.NewReplicaSetLister(h.rs),
		pods:        corelisters.NewPodLister(h.pods),
		services:    corelisters.NewServiceLister(h.svcs),
		slices:      discoverylisters.NewEndpointSliceLister(h.slices),
		namespaces:  corelisters.NewNamespaceLister(h.nss),
		recorder:    h.recorder,
		now:         func() time.Time { return h.now },
		period:      time.Second,
	}
	return h
}

func (h *harness) must(err error) {
	h.t.Helper()
	if err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) addReplicaSet(name, revision, image string) *appsv1.ReplicaSet {
	tmpl := template(image)
	tmpl.Labels[appsv1.DefaultDeploymentUniqueLabelKey] = name
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: "shop", UID: types.UID(name + "-uid"),
		Annotations:     map[string]string{revisionAnnotation: revision},
		OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "web", UID: "web-uid", Controller: ptr.To(true)}},
	}, Spec: appsv1.ReplicaSetSpec{Template: tmpl}}
	h.must(h.rs.Add(rs))
	return rs
}

func (h *harness) addPod(name string, rs *appsv1.ReplicaSet, ready bool, mutate func(*v1.Pod)) {
	status := v1.ConditionFalse
	if ready {
		status = v1.ConditionTrue
	}
	p := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop", Labels: rs.Spec.Template.Labels,
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: rs.Name, UID: rs.UID, Controller: ptr.To(true)}}},
		Status: v1.PodStatus{
			Phase:             v1.PodRunning,
			Conditions:        []v1.PodCondition{{Type: v1.PodReady, Status: status}},
			ContainerStatuses: []v1.ContainerStatus{{Name: "web", State: v1.ContainerState{Running: &v1.ContainerStateRunning{}}}},
		},
	}
	if mutate != nil {
		mutate(p)
	}
	h.must(h.pods.Add(p))
}

func (h *harness) sync() {
	h.t.Helper()
	if err := h.c.Sync(h.ctx); err != nil {
		h.t.Fatalf("sync at %s: %v", h.now.Sub(t0), err)
	}
}

func (h *harness) gestation() *api.Gestation {
	h.t.Helper()
	u, err := h.dyn.Resource(api.GVR).Namespace("shop").Get(h.ctx, "web", metav1.GetOptions{})
	h.must(err)
	g, err := api.FromUnstructured(u)
	h.must(err)
	return g
}

func (h *harness) patchSpec(spec map[string]interface{}) {
	h.t.Helper()
	raw, _ := json.Marshal(map[string]interface{}{"spec": spec})
	_, err := h.dyn.Resource(api.GVR).Namespace("shop").Patch(h.ctx, "web", types.MergePatchType, raw, metav1.PatchOptions{})
	h.must(err)
}

func (h *harness) deployment(name string) *appsv1.Deployment {
	h.t.Helper()
	d, err := h.kube.AppsV1().Deployments("shop").Get(h.ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil
	}
	return d
}

func (h *harness) events() []string {
	var out []string
	for {
		select {
		case e := <-h.recorder.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

func contains(events []string, s string) bool {
	for _, e := range events {
		if strings.Contains(e, s) {
			return true
		}
	}
	return false
}

func (h *harness) newbornAnnotation() string {
	ns, err := h.kube.CoreV1().Namespaces().Get(h.ctx, "shop", metav1.GetOptions{})
	h.must(err)
	// keep the lister in step, as the informer would
	h.must(h.nss.Update(ns))
	return ns.Annotations[kyvernetria.NewbornCareAnnotation]
}

func TestPregnancyReservesInTrimesterSteps(t *testing.T) {
	h := newHarness(t)
	want := map[int]int32{1: 1, 40: 2, 70: 3, 95: 3}
	for _, day := range []int{1, 40, 70, 95} {
		h.now = t0.Add(time.Duration(day) * 24 * time.Hour)
		h.sync()
		p := h.deployment("web-placenta")
		if p == nil || *p.Spec.Replicas != want[day] {
			t.Fatalf("day %d: placenta %v, want %d replicas", day, p, want[day])
		}
		if p.Spec.Template.Spec.PriorityClassName != api.PlacentaPriorityClass {
			t.Errorf("placenta class %q", p.Spec.Template.Spec.PriorityClassName)
		}
		g := h.gestation()
		if day < 90 && (g.Status.Phase != api.PhaseExpecting || g.Status.Reserved != want[day]) {
			t.Errorf("day %d: %s reserved %d", day, g.Status.Phase, g.Status.Reserved)
		}
	}
	g := h.gestation()
	if g.Status.Phase != api.PhaseDue || !strings.Contains(g.Status.Message, "Nothing happens on its own") {
		t.Errorf("past the due date: %s %q", g.Status.Phase, g.Status.Message)
	}
	events := h.events()
	for _, tri := range []string{"trimester 1", "trimester 2", "trimester 3"} {
		if !contains(events, "Prenatal screening, "+tri+": all clear") {
			t.Errorf("no screening for %s: %v", tri, events)
		}
	}
	for _, pc := range api.PriorityClasses() {
		if _, err := h.kube.SchedulingV1().PriorityClasses().Get(h.ctx, pc.Name, metav1.GetOptions{}); err != nil {
			t.Errorf("priority class %s: %v", pc.Name, err)
		}
	}
	// The placeholders are not someone else's Deployment: one with the
	// same name that the gestation doesn't own is left alone.
	h.must(h.kube.AppsV1().Deployments("shop").Delete(h.ctx, "web-placenta", metav1.DeleteOptions{}))
	_, err := h.kube.AppsV1().Deployments("shop").Create(h.ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "web-placenta", Namespace: "shop"}}, metav1.CreateOptions{})
	h.must(err)
	if err := h.c.Sync(h.ctx); err == nil || !strings.Contains(err.Error(), "not this gestation's placenta") {
		t.Errorf("a foreign web-placenta was not left alone: %v", err)
	}
}

func TestScreeningOnRequest(t *testing.T) {
	h := newHarness(t)
	h.sync()
	h.events()
	asked := metav1.NewTime(t0.Add(time.Hour))
	h.patchSpec(map[string]interface{}{"screenRequested": asked})
	h.now = t0.Add(2 * time.Hour)
	h.sync()
	g := h.gestation()
	if g.Status.Screening.Reason != "requested" || g.Status.ScreenedRequest == nil || !g.Status.ScreenedRequest.Equal(&asked) {
		t.Errorf("screening %+v answered %v", g.Status.Screening, g.Status.ScreenedRequest)
	}
	h.sync()
	if n := len(h.events()); n != 1 {
		t.Errorf("one request gave %d screenings", n)
	}
}

// born delivers web at t0+80d and plays the deployment controller.
func born(t *testing.T, h *harness) (*appsv1.ReplicaSet, time.Time) {
	h.now = t0.Add(80 * 24 * time.Hour)
	h.sync()
	h.patchSpec(map[string]interface{}{"delivery": 1})
	birth := h.now.Add(time.Minute)
	h.now = birth
	h.sync()

	g := h.gestation()
	if g.Status.Phase != api.PhaseNewbornCare || g.Status.Birth == nil {
		t.Fatalf("not born: %s %q", g.Status.Phase, g.Status.Message)
	}
	if h.deployment("web-placenta") != nil {
		t.Error("the placeholders were not released")
	}
	d := h.deployment("web")
	if *d.Spec.Replicas != 3 || d.Spec.Paused || d.Labels[kyvernetria.CareLabel] != kyvernetria.CareNewborn {
		t.Errorf("born deployment: replicas %d paused %v labels %v", *d.Spec.Replicas, d.Spec.Paused, d.Labels)
	}
	pdb, err := h.kube.PolicyV1().PodDisruptionBudgets("shop").Get(h.ctx, "web-newborn", metav1.GetOptions{})
	if err != nil || pdb.Spec.MinAvailable.IntValue() != 2 {
		t.Errorf("newborn PDB %v %v", pdb, err)
	}
	if ann := h.newbornAnnotation(); !strings.Contains(ann, `"selector":"app=web"`) || !strings.Contains(ann, "kyvernetria-newborn-1") {
		t.Errorf("newborn annotation %q", ann)
	}
	h.must(h.deploys.Update(d))

	rs := h.addReplicaSet("web-v2", "2", "ghcr.io/shop/web:v2")
	return rs, birth
}

func TestBirthApgarCareAndDeparture(t *testing.T) {
	h := newHarness(t)
	rs, birth := born(t, h)
	for _, n := range []string{"web-v2-a", "web-v2-b", "web-v2-c"} {
		h.addPod(n, rs, true, nil)
	}
	h.must(h.svcs.Add(&v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop"},
		Spec: v1.ServiceSpec{Selector: map[string]string{"app": "web"}}}))
	slice := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: "web-x", Namespace: "shop",
		Labels: map[string]string{"kubernetes.io/service-name": "web"}}}
	for _, n := range []string{"web-v2-a", "web-v2-b", "web-v2-c"} {
		slice.Endpoints = append(slice.Endpoints, discoveryv1.Endpoint{TargetRef: &v1.ObjectReference{Kind: "Pod", Name: n},
			Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)}})
	}
	h.must(h.slices.Add(slice))
	h.events()

	h.now = birth.Add(30 * time.Second)
	h.sync()
	if g := h.gestation(); len(g.Status.Apgar) != 0 || g.Status.Birth.Revision != "2" || g.Status.Birth.PreviousRevision != "1" {
		t.Fatalf("before one minute: %d scores, revisions %q/%q", len(g.Status.Apgar), g.Status.Birth.Revision, g.Status.Birth.PreviousRevision)
	}
	h.now = birth.Add(time.Minute)
	h.sync()
	h.now = birth.Add(5 * time.Minute)
	h.sync()
	g := h.gestation()
	if len(g.Status.Apgar) != 2 || g.Status.Apgar[1].Total != 10 {
		t.Fatalf("scores %+v", g.Status.Apgar)
	}
	events := h.events()
	if !contains(events, "Apgar 10 at one minute.") || !contains(events, "web arrived. Apgar 10 at five minutes. Welcome.") {
		t.Errorf("birth events %v", events)
	}

	h.now = birth.Add(30 * time.Hour)
	h.sync()
	if ann := h.newbornAnnotation(); !strings.Contains(ann, "kyvernetria-newborn-2") {
		t.Errorf("care did not taper at 30 hours: %q", ann)
	}

	// The care period ends with no caregivers: protection stays, gently.
	h.now = birth.Add(73 * time.Hour)
	h.sync()
	g = h.gestation()
	if g.Status.Phase != api.PhaseNeedsCaregivers || g.Status.Care.Tier != 3 {
		t.Errorf("without caregivers: %s tier %d", g.Status.Phase, g.Status.Care.Tier)
	}
	if !contains(h.events(), "web is ready to go home from newborn care, but nobody is named yet. Care is shared between two people") ||
		!strings.Contains(g.Status.Message, "nobody is named yet") {
		t.Errorf("no warm nag: %q", g.Status.Message)
	}
	h.now = birth.Add(74 * time.Hour)
	h.sync()
	if contains(h.events(), "ready to go home") {
		t.Error("nagged again within the hour")
	}

	d := h.deployment("web")
	d.Annotations[kyvernetria.PrimaryCaregiverAnnotation] = "alice"
	d.Annotations[kyvernetria.SecondaryCaregiverAnnotation] = "bob"
	_, err := h.kube.AppsV1().Deployments("shop").Update(h.ctx, d, metav1.UpdateOptions{})
	h.must(err)
	h.now = birth.Add(75 * time.Hour)
	h.sync()
	g = h.gestation()
	d = h.deployment("web")
	if g.Status.Phase != api.PhaseGrown || *d.Spec.Replicas != 2 || d.Labels[kyvernetria.CareLabel] != "" {
		t.Errorf("discharge: %s replicas %d labels %v", g.Status.Phase, *d.Spec.Replicas, d.Labels)
	}
	if _, err := h.kube.PolicyV1().PodDisruptionBudgets("shop").Get(h.ctx, "web-newborn", metav1.GetOptions{}); err == nil {
		t.Error("the newborn PDB outlived newborn care")
	}
	if ann := h.newbornAnnotation(); ann != "" {
		t.Errorf("newborn annotation left behind: %q", ann)
	}
	if !contains(h.events(), "goes home from newborn care. Two caregivers: alice and bob.") {
		t.Error("no discharge event")
	}

	cm, err := h.kube.CoreV1().ConfigMaps(kyvernetria.SystemNamespace).Get(h.ctx, api.MemoryConfigMap, metav1.GetOptions{})
	h.must(err)
	records := api.Records(cm.Data)
	if len(records) != 1 || records[0].Departed() || strings.Join(records[0].Owners, ",") != "alice,bob" {
		t.Fatalf("records while alive: %+v", records)
	}

	// web is decommissioned.
	h.must(h.kube.AppsV1().Deployments("shop").Delete(h.ctx, "web", metav1.DeleteOptions{}))
	h.must(h.deploys.Delete(d))
	h.now = birth.Add(100 * 24 * time.Hour)
	h.sync()
	g = h.gestation()
	cm, err = h.kube.CoreV1().ConfigMaps(kyvernetria.SystemNamespace).Get(h.ctx, api.MemoryConfigMap, metav1.GetOptions{})
	h.must(err)
	records = api.Records(cm.Data)
	if g.Status.Phase != api.PhaseDeparted || len(records) != 1 || records[0].Left != api.Stamp(h.now) || records[0].Born != api.Stamp(birth) {
		t.Errorf("departure: %s %+v", g.Status.Phase, records)
	}
}

func TestLowApgarRollsBack(t *testing.T) {
	h := newHarness(t)
	rs, birth := born(t, h)
	crashing := func(p *v1.Pod) {
		p.Status.ContainerStatuses[0] = v1.ContainerStatus{Name: "web", RestartCount: 4,
			State: v1.ContainerState{Waiting: &v1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}}
	}
	for _, n := range []string{"web-v2-a", "web-v2-b", "web-v2-c"} {
		h.addPod(n, rs, false, crashing)
	}
	_, err := h.kube.CoreV1().Events("shop").Create(h.ctx, &v1.Event{
		ObjectMeta: metav1.ObjectMeta{Name: "e1", Namespace: "shop"}, Type: v1.EventTypeWarning, Reason: "FailedMount",
		InvolvedObject: v1.ObjectReference{Kind: "Pod", Name: "web-v2-a"}, Count: 7, LastTimestamp: metav1.NewTime(birth.Add(time.Minute)),
	}, metav1.CreateOptions{})
	h.must(err)
	h.events()
	h.now = birth.Add(time.Minute)
	h.sync()
	h.now = birth.Add(5 * time.Minute)
	h.sync()

	g := h.gestation()
	if g.Status.Phase != api.PhaseRolledBack || g.Status.Birth.RolledBackTo != "1" {
		t.Fatalf("not rolled back: %s %q", g.Status.Phase, g.Status.Message)
	}
	if a := g.Status.Apgar[1]; a.Total != 2 || a.Grimace != 0 || a.Pulse != 0 {
		t.Errorf("five-minute score %+v", a)
	}
	d := h.deployment("web")
	if d.Spec.Template.Spec.Containers[0].Image != "ghcr.io/shop/web:v1" || d.Spec.Template.Labels[appsv1.DefaultDeploymentUniqueLabelKey] != "" {
		t.Errorf("template after rollback: %s %v", d.Spec.Template.Spec.Containers[0].Image, d.Spec.Template.Labels)
	}
	if *d.Spec.Replicas != 2 || d.Labels[kyvernetria.CareLabel] != "" {
		t.Errorf("newborn care left on a rolled-back deployment: %d %v", *d.Spec.Replicas, d.Labels)
	}
	if !contains(h.events(), "Going back to revision 1, where it was well.") {
		t.Error("no rollback event")
	}
	h.sync()
	if ann := h.newbornAnnotation(); ann != "" {
		t.Errorf("rolled back, still protected: %q", ann)
	}

	// Delivering again starts a new birth.
	h.patchSpec(map[string]interface{}{"delivery": 2})
	h.now = birth.Add(time.Hour)
	h.sync()
	if g := h.gestation(); g.Status.Phase != api.PhaseNewbornCare || len(g.Status.Apgar) != 0 || g.Status.Delivery != 2 {
		t.Errorf("second delivery: %s %d scores", g.Status.Phase, len(g.Status.Apgar))
	}
}

// The controller's own warnings on the Deployment (a low score at one
// minute) are not the newborn's grimace.
func TestApgarIgnoresItsOwnWarnings(t *testing.T) {
	h := newHarness(t)
	rs, birth := born(t, h)
	for _, n := range []string{"web-v2-a", "web-v2-b", "web-v2-c"} {
		h.addPod(n, rs, true, nil)
	}
	_, err := h.kube.CoreV1().Events("shop").Create(h.ctx, &v1.Event{
		ObjectMeta: metav1.ObjectMeta{Name: "own", Namespace: "shop"}, Type: v1.EventTypeWarning, Reason: ReasonApgarLow,
		InvolvedObject: v1.ObjectReference{Kind: "Deployment", Name: "web"}, Count: 1,
		Source: v1.EventSource{Component: EventSource}, LastTimestamp: metav1.NewTime(birth.Add(30 * time.Second)),
	}, metav1.CreateOptions{})
	h.must(err)
	h.now = birth.Add(time.Minute)
	h.sync()
	if g := h.gestation(); len(g.Status.Apgar) != 1 || g.Status.Apgar[0].Grimace != 2 {
		t.Fatalf("own warning counted: %+v", g.Status.Apgar)
	}
}

func TestLowApgarWithoutRollbackKeepsScoring(t *testing.T) {
	h := newHarness(t)
	h.patchSpec(map[string]interface{}{"autoRollback": false})
	rs, birth := born(t, h)
	h.addPod("web-v2-a", rs, false, func(p *v1.Pod) {
		p.Status.ContainerStatuses[0] = v1.ContainerStatus{Name: "web", RestartCount: 9,
			State: v1.ContainerState{Waiting: &v1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}}
	})
	for _, m := range []int{1, 5, 10, 15, 20, 25} {
		h.now = birth.Add(time.Duration(m) * time.Minute)
		h.sync()
	}
	g := h.gestation()
	var minutes []int32
	for _, a := range g.Status.Apgar {
		minutes = append(minutes, a.Minute)
	}
	if len(minutes) != 5 || minutes[4] != 20 || g.Status.Phase == api.PhaseRolledBack {
		t.Errorf("scores at %v, phase %s", minutes, g.Status.Phase)
	}
	if h.deployment("web").Spec.Template.Spec.Containers[0].Image != "ghcr.io/shop/web:v2" {
		t.Error("rolled back although rollback is off")
	}
}

func TestGrowthMeasurement(t *testing.T) {
	h := newHarness(t)
	g := &api.Gestation{ObjectMeta: metav1.ObjectMeta{Namespace: "shop"}, Status: api.Status{Birth: &api.Birth{Selector: "app=web"}}}
	h.c.measure(h.ctx, g, true, t0)
	if len(g.Status.Growth) != 0 {
		t.Error("measured without a metrics API")
	}
	pm := func(name string, cpu, mem string) *metricsv1beta1.PodMetrics {
		return &metricsv1beta1.PodMetrics{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop", Labels: map[string]string{"app": "web"}},
			Containers: []metricsv1beta1.ContainerMetrics{{Name: "web", Usage: v1.ResourceList{
				v1.ResourceCPU: resource.MustParse(cpu), v1.ResourceMemory: resource.MustParse(mem)}}},
		}
	}
	metrics := metricsfake.NewSimpleClientset()
	// The generated fake files PodMetrics under "podmetricses"; the client
	// lists "pods", as the real API does.
	metrics.PrependReactor("list", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, &metricsv1beta1.PodMetricsList{Items: []metricsv1beta1.PodMetrics{*pm("a", "100m", "100Mi"), *pm("b", "300m", "300Mi")}}, nil
	})
	h.c.metrics = metrics
	h.c.measure(h.ctx, g, true, t0)
	if len(g.Status.Growth) != 1 || g.Status.Growth[0].CPUMilli != 200 || g.Status.Growth[0].MemoryBytes != 200<<20 || g.Status.Growth[0].Pods != 2 {
		t.Fatalf("growth %+v", g.Status.Growth)
	}
	h.c.measure(h.ctx, g, true, t0.Add(5*time.Minute))
	h.c.measure(h.ctx, g, true, t0.Add(10*time.Minute))
	if len(g.Status.Growth) != 2 {
		t.Errorf("newborn measured %d times in ten minutes", len(g.Status.Growth))
	}
}

func TestCloseDepartedWithoutGestation(t *testing.T) {
	h := newHarness(t)
	m := &memory{data: map[string]string{}, now: t0}
	m.put(api.Record{Namespace: "shop", Name: "web", UID: "web-uid", Born: api.Stamp(t0)})
	m.put(api.Record{Namespace: "shop", Name: "gone", UID: "gone-uid", Born: api.Stamp(t0)})
	m.put(api.Record{Namespace: "shop", Name: "web", UID: "older-web", Born: api.Stamp(t0.Add(-time.Hour))})
	h.c.closeDeparted(m)
	left := map[string]bool{}
	for _, r := range api.Records(m.data) {
		left[r.UID] = r.Departed()
	}
	if left["web-uid"] || !left["gone-uid"] || !left["older-web"] {
		t.Errorf("departures %v", left)
	}
}
