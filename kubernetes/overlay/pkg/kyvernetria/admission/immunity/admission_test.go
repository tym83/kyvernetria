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

package immunity

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	api "k8s.io/kubernetes/pkg/apis/core"

	"k8s.io/kubernetes/pkg/kyvernetria"
)

func TestFloatingTag(t *testing.T) {
	for image, want := range map[string]bool{
		"nginx":                        true,
		"nginx:latest":                 true,
		"nginx:1.27":                   false,
		"registry:5000/nginx":          true,
		"registry:5000/nginx:1.27":     false,
		"nginx@sha256:0123456789abcdef": false,
	} {
		if got := FloatingTag(image); got != want {
			t.Errorf("FloatingTag(%q) = %v, want %v", image, got, want)
		}
	}
}

func newPlugin(t *testing.T, namespaces ...*corev1.Namespace) *Plugin {
	t.Helper()
	client := fake.NewSimpleClientset()
	factory := informers.NewSharedInformerFactory(client, 0)
	p := New()
	p.SetExternalKubeInformerFactory(factory)
	for _, ns := range namespaces {
		if err := factory.Core().V1().Namespaces().Informer().GetStore().Add(ns); err != nil {
			t.Fatal(err)
		}
	}
	p.SetReadyFunc(func() bool { return true })
	return p
}

func attrs(ns string, pod *api.Pod) admission.Attributes {
	return admission.NewAttributesRecord(pod, nil, api.Kind("Pod").WithVersion("version"), ns, pod.Name,
		api.Resource("pods").WithVersion("version"), "", admission.Create, &metav1.CreateOptions{}, false, nil)
}

func TestValidate(t *testing.T) {
	privileged := true
	foreign := &api.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p"},
		Spec: api.PodSpec{
			Containers: []api.Container{{Name: "c", Image: "busybox", SecurityContext: &api.SecurityContext{Privileged: &privileged}}},
			Volumes:    []api.Volume{{Name: "root", VolumeSource: api.VolumeSource{HostPath: &api.HostPathVolumeSource{Path: "/"}}}},
		},
	}
	clean := &api.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p"},
		Spec:       api.PodSpec{Containers: []api.Container{{Name: "c", Image: "busybox:1.36"}}},
	}
	selfNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "mine", Labels: map[string]string{kyvernetria.SelfLabel: "true"}}}
	p := newPlugin(t, selfNS, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}})

	err := p.Validate(context.Background(), attrs("default", foreign), nil)
	if err == nil {
		t.Fatal("foreign pod was admitted")
	}
	for _, want := range []string{MessagePrefix, "privileged", "host path", "unpinned image"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("rejection %q does not mention %q", err, want)
		}
	}
	if err := p.Validate(context.Background(), attrs("default", clean), nil); err != nil {
		t.Errorf("clean pod rejected: %v", err)
	}
	if err := p.Validate(context.Background(), attrs("mine", foreign), nil); err != nil {
		t.Errorf("self namespace not tolerated: %v", err)
	}
	if err := p.Validate(context.Background(), attrs("kube-system", foreign), nil); err != nil {
		t.Errorf("kube-system not tolerated: %v", err)
	}
}
