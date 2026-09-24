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
		"nginx":                         true,
		"nginx:latest":                  true,
		"nginx:1.27":                    false,
		"registry:5000/nginx":           true,
		"registry:5000/nginx:1.27":      false,
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
	p.SetExternalKubeClientSet(client)
	if err := p.ValidateInitialization(); err != nil {
		t.Fatal(err)
	}
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

func updateAttrs(ns, subresource string, old, pod *api.Pod) admission.Attributes {
	return admission.NewAttributesRecord(pod, old, api.Kind("Pod").WithVersion("version"), ns, pod.Name,
		api.Resource("pods").WithVersion("version"), subresource, admission.Update, &metav1.UpdateOptions{}, false, nil)
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

func TestAntigens(t *testing.T) {
	yes := true
	unmasked := corev1.UnmaskedProcMount
	withSC := func(sc *corev1.SecurityContext) *corev1.Pod {
		return &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "app:1", SecurityContext: sc}}}}
	}
	for name, tc := range map[string]struct {
		pod  *corev1.Pod
		want string
	}{
		"SYS_ADMIN": {withSC(&corev1.SecurityContext{Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"SYS_ADMIN"}}}),
			`container "c" adds capability SYS_ADMIN`},
		"CAP_NET_ADMIN": {withSC(&corev1.SecurityContext{Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"CAP_NET_ADMIN"}}}),
			`container "c" adds capability NET_ADMIN`},
		"ALL": {withSC(&corev1.SecurityContext{Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"all"}}}),
			`container "c" adds capability ALL`},
		"privilege escalation": {withSC(&corev1.SecurityContext{AllowPrivilegeEscalation: &yes}),
			`container "c" allows privilege escalation`},
		"unmasked proc": {withSC(&corev1.SecurityContext{ProcMount: &unmasked}),
			`container "c" mounts /proc unmasked`},
		"container seccomp": {withSC(&corev1.SecurityContext{SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined}}),
			`container "c" runs without a seccomp profile`},
		"container apparmor": {withSC(&corev1.SecurityContext{AppArmorProfile: &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeUnconfined}}),
			`container "c" runs without an AppArmor profile`},
		"pod seccomp": {&corev1.Pod{Spec: corev1.PodSpec{SecurityContext: &corev1.PodSecurityContext{
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined}}}},
			`pod runs without a seccomp profile`},
		"pod apparmor": {&corev1.Pod{Spec: corev1.PodSpec{SecurityContext: &corev1.PodSecurityContext{
			AppArmorProfile: &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeUnconfined}}}},
			`pod runs without an AppArmor profile`},
		"apparmor annotation": {&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{"container.apparmor.security.beta.kubernetes.io/c": "unconfined"}},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "app:1"}}}},
			`container "c" runs without an AppArmor profile`},
		"host port": {&corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "app:1",
			Ports: []corev1.ContainerPort{{ContainerPort: 80, HostPort: 8080}}}}}},
			`container "c" binds host port 8080`},
	} {
		t.Run(name, func(t *testing.T) {
			got := strings.Join(Antigens(tc.pod), "; ")
			if !strings.Contains(got, tc.want) {
				t.Errorf("Antigens = %q, want it to contain %q", got, tc.want)
			}
		})
	}
	no := false
	benign := withSC(&corev1.SecurityContext{AllowPrivilegeEscalation: &no,
		Capabilities:   &corev1.Capabilities{Add: []corev1.Capability{"NET_BIND_SERVICE", "CAP_CHOWN"}, Drop: []corev1.Capability{"ALL"}},
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}})
	if got := Antigens(benign); len(got) != 0 {
		t.Errorf("baseline-compatible pod has antigens: %v", got)
	}
}

func TestValidateUpdate(t *testing.T) {
	yes := true
	p := newPlugin(t, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}})
	running := &api.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
		Spec:       api.PodSpec{Containers: []api.Container{{Name: "c", Image: "busybox:1.36"}}},
	}

	floating := running.DeepCopy()
	floating.Spec.Containers[0].Image = "busybox:latest"
	if err := p.Validate(context.Background(), updateAttrs("default", "", running, floating), nil); err == nil ||
		!strings.Contains(err.Error(), "unpinned image") {
		t.Errorf("kubectl set image to a floating tag was admitted: %v", err)
	}

	debugged := running.DeepCopy()
	debugged.Spec.EphemeralContainers = []api.EphemeralContainer{{EphemeralContainerCommon: api.EphemeralContainerCommon{
		Name: "debugger", Image: "busybox:1.36", SecurityContext: &api.SecurityContext{Privileged: &yes}}}}
	if err := p.Validate(context.Background(), updateAttrs("default", "ephemeralcontainers", running, debugged), nil); err == nil ||
		!strings.Contains(err.Error(), `ephemeral container "debugger" is privileged`) {
		t.Errorf("kubectl debug --profile=sysadmin was admitted: %v", err)
	}

	// A pod admitted before (or in a namespace that was self back then)
	// can still be updated for unrelated reasons.
	legacy := running.DeepCopy()
	legacy.Spec.Containers[0].Image = "busybox"
	relabeled := legacy.DeepCopy()
	relabeled.Labels = map[string]string{"tier": "web"}
	if err := p.Validate(context.Background(), updateAttrs("default", "", legacy, relabeled), nil); err != nil {
		t.Errorf("unrelated update of a pod with an existing antigen was refused: %v", err)
	}
	pinned := running.DeepCopy()
	pinned.Spec.Containers[0].Image = "busybox:1.37"
	if err := p.Validate(context.Background(), updateAttrs("default", "", running, pinned), nil); err != nil {
		t.Errorf("updating to another pinned tag was refused: %v", err)
	}
	if err := p.Validate(context.Background(), updateAttrs("default", "status", running, floating), nil); err != nil {
		t.Errorf("status update was checked: %v", err)
	}
}

func TestSelfNamespaceNotYetInInformer(t *testing.T) {
	privileged := true
	fresh := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "fresh", Labels: map[string]string{kyvernetria.SelfLabel: "true"}}}
	client := fake.NewSimpleClientset(fresh)
	p := New()
	p.SetExternalKubeInformerFactory(informers.NewSharedInformerFactory(client, 0)) // never started: empty cache
	p.SetExternalKubeClientSet(client)
	p.SetReadyFunc(func() bool { return true })
	pod := &api.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p"},
		Spec:       api.PodSpec{Containers: []api.Container{{Name: "c", Image: "app:1", SecurityContext: &api.SecurityContext{Privileged: &privileged}}}},
	}
	if err := p.Validate(context.Background(), attrs("fresh", pod), nil); err != nil {
		t.Errorf("self namespace missing from the informer was treated as foreign: %v", err)
	}
	if err := p.Validate(context.Background(), attrs("missing", pod), nil); err == nil {
		t.Error("pod in an unknown namespace was admitted")
	}
}
