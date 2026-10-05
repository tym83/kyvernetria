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

package newborncare

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	api "k8s.io/kubernetes/pkg/apis/core"

	"k8s.io/kubernetes/pkg/kyvernetria"
	"k8s.io/kubernetes/pkg/kyvernetria/gestation"
)

func newPlugin(t *testing.T, annotation string, classes ...*schedulingv1.PriorityClass) *Plugin {
	t.Helper()
	client := fake.NewSimpleClientset()
	factory := informers.NewSharedInformerFactory(client, 0)
	p := New()
	p.SetExternalKubeInformerFactory(factory)
	p.SetExternalKubeClientSet(client)
	if err := p.ValidateInitialization(); err != nil {
		t.Fatal(err)
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop",
		Annotations: map[string]string{kyvernetria.NewbornCareAnnotation: annotation}}}
	if err := factory.Core().V1().Namespaces().Informer().GetStore().Add(ns); err != nil {
		t.Fatal(err)
	}
	for _, pc := range classes {
		if err := factory.Scheduling().V1().PriorityClasses().Informer().GetStore().Add(pc); err != nil {
			t.Fatal(err)
		}
	}
	p.SetReadyFunc(func() bool { return true })
	return p
}

func tiers() []*schedulingv1.PriorityClass {
	var out []*schedulingv1.PriorityClass
	for _, pc := range gestation.PriorityClasses() {
		if gestation.IsNewbornClass(pc.Name) {
			out = append(out, pc)
		}
	}
	return out
}

func admit(t *testing.T, p *Plugin, pod *api.Pod, subresource string) {
	t.Helper()
	a := admission.NewAttributesRecord(pod, nil, api.Kind("Pod").WithVersion("version"), "shop", pod.Name,
		api.Resource("pods").WithVersion("version"), subresource, admission.Create, &metav1.CreateOptions{}, false, nil)
	if err := p.Admit(context.Background(), a, nil); err != nil {
		t.Fatal(err)
	}
}

func pod(labels map[string]string) *api.Pod {
	return &api.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Labels: labels}}
}

var webTier2 = gestation.EncodeNewborns([]gestation.NewbornEntry{{Selector: "app=web", PriorityClass: "kyvernetria-newborn-2"}})

func TestNewbornPodsGetTheirTier(t *testing.T) {
	p := newPlugin(t, webTier2, tiers()...)
	newborn := pod(map[string]string{"app": "web", "pod-template-hash": "abc"})
	admit(t, p, newborn, "")
	if newborn.Spec.PriorityClassName != "kyvernetria-newborn-2" || newborn.Annotations[kyvernetria.NewbornCareAnnotation] != "kyvernetria-newborn-2" {
		t.Errorf("newborn pod: class %q annotations %v", newborn.Spec.PriorityClassName, newborn.Annotations)
	}
	other := pod(map[string]string{"app": "api"})
	admit(t, p, other, "")
	if other.Spec.PriorityClassName != "" {
		t.Errorf("a pod of another service got %q", other.Spec.PriorityClassName)
	}
}

func TestNewbornCareNeverOverrides(t *testing.T) {
	p := newPlugin(t, webTier2, tiers()...)
	chosen := pod(map[string]string{"app": "web"})
	chosen.Spec.PriorityClassName = "batch-low"
	admit(t, p, chosen, "")
	if chosen.Spec.PriorityClassName != "batch-low" {
		t.Error("overrode a class someone chose")
	}
	mirror := pod(map[string]string{"app": "web"})
	mirror.Annotations = map[string]string{api.MirrorPodAnnotationKey: "x"}
	admit(t, p, mirror, "")
	if mirror.Spec.PriorityClassName != "" {
		t.Error("touched a mirror pod")
	}
	status := pod(map[string]string{"app": "web"})
	admit(t, p, status, "status")
	if status.Spec.PriorityClassName != "" {
		t.Error("acted on a subresource")
	}
}

func TestNewbornCareIsSafe(t *testing.T) {
	// A class that doesn't exist (yet) would make the Priority plugin
	// reject the pod: skip rather than block a launch.
	p := newPlugin(t, webTier2)
	missing := pod(map[string]string{"app": "web"})
	admit(t, p, missing, "")
	if missing.Spec.PriorityClassName != "" {
		t.Error("set a class that doesn't exist")
	}

	// The annotation may only grant the care tiers.
	forged := gestation.EncodeNewborns([]gestation.NewbornEntry{{Selector: "app=web", PriorityClass: "system-cluster-critical"}})
	p = newPlugin(t, forged, append(tiers(), &schedulingv1.PriorityClass{ObjectMeta: metav1.ObjectMeta{Name: "system-cluster-critical"}, Value: 2000000000})...)
	escalated := pod(map[string]string{"app": "web"})
	admit(t, p, escalated, "")
	if escalated.Spec.PriorityClassName != "" {
		t.Errorf("the annotation granted %q", escalated.Spec.PriorityClassName)
	}

	// Never below the global default.
	p = newPlugin(t, webTier2, append(tiers(), &schedulingv1.PriorityClass{ObjectMeta: metav1.ObjectMeta{Name: "default"}, Value: 600, GlobalDefault: true})...)
	lowered := pod(map[string]string{"app": "web"})
	admit(t, p, lowered, "")
	if lowered.Spec.PriorityClassName != "" {
		t.Error("lowered a pod below the global default")
	}

	// A broken annotation or an empty selector does nothing.
	for _, raw := range []string{"{", gestation.EncodeNewborns([]gestation.NewbornEntry{{Selector: "", PriorityClass: "kyvernetria-newborn-1"}})} {
		p = newPlugin(t, raw, tiers()...)
		anyPod := pod(map[string]string{"app": "web"})
		admit(t, p, anyPod, "")
		if anyPod.Spec.PriorityClassName != "" {
			t.Errorf("annotation %q bumped a pod", raw)
		}
	}
}
