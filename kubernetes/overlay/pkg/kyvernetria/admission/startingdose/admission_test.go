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

package startingdose

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	api "k8s.io/kubernetes/pkg/apis/core"

	"k8s.io/kubernetes/pkg/kyvernetria"
)

func namespace(name string, labels map[string]string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

var startLow = map[string]string{kyvernetria.DosingLabel: kyvernetria.DosingStartLow}

func newPlugin(t *testing.T, inInformer []*corev1.Namespace, onlyInAPI ...*corev1.Namespace) *Plugin {
	t.Helper()
	client := fake.NewSimpleClientset()
	for _, ns := range onlyInAPI {
		if _, err := client.CoreV1().Namespaces().Create(context.Background(), ns, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	factory := informers.NewSharedInformerFactory(client, 0)
	p := New()
	p.SetExternalKubeInformerFactory(factory)
	p.SetExternalKubeClientSet(client)
	if err := p.ValidateInitialization(); err != nil {
		t.Fatal(err)
	}
	for _, ns := range inInformer {
		if err := factory.Core().V1().Namespaces().Informer().GetStore().Add(ns); err != nil {
			t.Fatal(err)
		}
	}
	p.SetReadyFunc(func() bool { return true })
	return p
}

func attrs(ns, subresource string, op admission.Operation, pod *api.Pod) admission.Attributes {
	return admission.NewAttributesRecord(pod, nil, api.Kind("Pod").WithVersion("version"), ns, pod.Name,
		api.Resource("pods").WithVersion("version"), subresource, op, &metav1.CreateOptions{}, false, nil)
}

func bare(names ...string) *api.Pod {
	pod := &api.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	for _, n := range names {
		pod.Spec.Containers = append(pod.Spec.Containers, api.Container{Name: n, Image: "img:1"})
	}
	return pod
}

func request(c api.Container, name api.ResourceName) string {
	q, ok := c.Resources.Requests[name]
	if !ok {
		return ""
	}
	return q.String()
}

func TestAdmitOptedIn(t *testing.T) {
	p := newPlugin(t, []*corev1.Namespace{namespace("shop", startLow)})
	pod := bare("app", "sidecar")
	pod.Spec.InitContainers = []api.Container{{Name: "init", Image: "img:1"}}
	// sidecar states memory (as a limit, which defaulting copies to the
	// request) but not CPU.
	pod.Spec.Containers[1].Resources = api.ResourceRequirements{
		Limits:   api.ResourceList{api.ResourceMemory: resource.MustParse("1Gi")},
		Requests: api.ResourceList{api.ResourceMemory: resource.MustParse("1Gi")},
	}
	if err := p.Admit(context.Background(), attrs("shop", "", admission.Create, pod), nil); err != nil {
		t.Fatal(err)
	}
	app, sidecar, init := pod.Spec.Containers[0], pod.Spec.Containers[1], pod.Spec.InitContainers[0]
	for _, c := range []api.Container{app, init} {
		if request(c, api.ResourceCPU) != "50m" || request(c, api.ResourceMemory) != "64Mi" {
			t.Errorf("%s: requests %v, want the starting dose", c.Name, c.Resources.Requests)
		}
		if len(c.Resources.Limits) != 0 {
			t.Errorf("%s: limits %v, want none", c.Name, c.Resources.Limits)
		}
	}
	if request(sidecar, api.ResourceCPU) != "50m" || request(sidecar, api.ResourceMemory) != "1Gi" {
		t.Errorf("sidecar: requests %v, want cpu dosed and memory kept", sidecar.Resources.Requests)
	}
	want := "init=cpu,memory;app=cpu,memory;sidecar=cpu"
	if got := pod.Annotations[kyvernetria.StartingDoseAnnotation]; got != want {
		t.Errorf("annotation %q, want %q", got, want)
	}
}

func TestAdmitLeavesOthersAlone(t *testing.T) {
	p := newPlugin(t, []*corev1.Namespace{
		namespace("plain", nil),
		namespace("other-value", map[string]string{kyvernetria.DosingLabel: "true"}),
		namespace("shop", startLow),
	})
	for name, tc := range map[string]struct {
		ns, sub string
		op      admission.Operation
		pod     func() *api.Pod
	}{
		"namespace not opted in":   {ns: "plain", op: admission.Create, pod: func() *api.Pod { return bare("app") }},
		"label with another value": {ns: "other-value", op: admission.Create, pod: func() *api.Pod { return bare("app") }},
		"unknown namespace":        {ns: "gone", op: admission.Create, pod: func() *api.Pod { return bare("app") }},
		"update":                   {ns: "shop", op: admission.Update, pod: func() *api.Pod { return bare("app") }},
		"subresource":              {ns: "shop", sub: "status", op: admission.Create, pod: func() *api.Pod { return bare("app") }},
		"mirror pod": {ns: "shop", op: admission.Create, pod: func() *api.Pod {
			pod := bare("app")
			pod.Annotations = map[string]string{api.MirrorPodAnnotationKey: "x"}
			return pod
		}},
		"pod-level resources": {ns: "shop", op: admission.Create, pod: func() *api.Pod {
			pod := bare("app")
			pod.Spec.Resources = &api.ResourceRequirements{Requests: api.ResourceList{api.ResourceCPU: resource.MustParse("1")}}
			return pod
		}},
		"fully stated": {ns: "shop", op: admission.Create, pod: func() *api.Pod {
			pod := bare("app")
			pod.Spec.Containers[0].Resources.Requests = api.ResourceList{
				api.ResourceCPU: resource.MustParse("10m"), api.ResourceMemory: resource.MustParse("8Mi"),
			}
			return pod
		}},
	} {
		t.Run(name, func(t *testing.T) {
			pod := tc.pod()
			before := pod.DeepCopy()
			// The handler filters operations before Admit is called.
			if !p.Handles(tc.op) {
				return
			}
			if err := p.Admit(context.Background(), attrs(tc.ns, tc.sub, tc.op, pod), nil); err != nil {
				t.Fatal(err)
			}
			if !apiequality.Semantic.DeepEqual(pod, before) {
				t.Errorf("pod changed: %+v", pod.Spec.Containers[0].Resources)
			}
		})
	}
}

func TestOnlyCreatesAreHandled(t *testing.T) {
	p := New()
	for _, op := range []admission.Operation{admission.Update, admission.Delete, admission.Connect} {
		if p.Handles(op) {
			t.Errorf("handles %s", op)
		}
	}
}

func TestNamespaceNotYetInInformer(t *testing.T) {
	p := newPlugin(t, nil, namespace("fresh", startLow))
	pod := bare("app")
	if err := p.Admit(context.Background(), attrs("fresh", "", admission.Create, pod), nil); err != nil {
		t.Fatal(err)
	}
	if request(pod.Spec.Containers[0], api.ResourceCPU) != "50m" {
		t.Errorf("fresh namespace not dosed: %v", pod.Spec.Containers[0].Resources.Requests)
	}
}

func TestDoseIsIdempotent(t *testing.T) {
	pod := bare("app")
	if !Dose(pod) {
		t.Fatal("first dose changed nothing")
	}
	once := pod.DeepCopy()
	if Dose(pod) {
		t.Error("second dose changed the pod")
	}
	if pod.Annotations[kyvernetria.StartingDoseAnnotation] != once.Annotations[kyvernetria.StartingDoseAnnotation] {
		t.Error("second dose rewrote the annotation")
	}
}
