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

package nobruteforce

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	api "k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/apis/policy"

	"k8s.io/kubernetes/pkg/kyvernetria"
)

func deleteAttrs(pod *api.Pod, grace *int64, who string) admission.Attributes {
	return admission.NewAttributesRecord(nil, pod, api.Kind("Pod").WithVersion("version"), pod.Namespace, pod.Name,
		api.Resource("pods").WithVersion("version"), "", admission.Delete,
		&metav1.DeleteOptions{GracePeriodSeconds: grace}, false, &user.DefaultInfo{Name: who})
}

func TestValidate(t *testing.T) {
	zero, one, thirty := int64(0), int64(1), int64(30)
	now := metav1.Now()
	onNode := api.PodSpec{NodeName: "worker-1"}
	running := &api.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"}, Spec: onNode,
		Status: api.PodStatus{Phase: api.PodRunning}}
	terminating := &api.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default", DeletionTimestamp: &now}, Spec: onNode}
	discussed := &api.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default",
		Annotations: map[string]string{kyvernetria.DiscussedAnnotation: "true"}}, Spec: onNode}
	// The apiserver forces grace 0 on these itself, even for a plain delete.
	unscheduled := &api.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
		Status: api.PodStatus{Phase: api.PodPending}}
	finished := &api.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"}, Spec: onNode,
		Status: api.PodStatus{Phase: api.PodSucceeded}}

	for _, tc := range []struct {
		name    string
		pod     *api.Pod
		grace   *int64
		who     string
		allowed bool
	}{
		{"force delete of running pod by a human", running, &zero, "alice", false},
		{"normal delete", running, nil, "alice", true},
		{"explicit grace", running, &thirty, "alice", true},
		{"pod already terminating", terminating, &zero, "alice", true},
		{"discussed first", discussed, &zero, "alice", true},
		{"pod never scheduled", unscheduled, &zero, "alice", true},
		{"pod already finished", finished, &zero, "alice", true},
		{"kubelet finishing a pod", running, &zero, "system:node:worker-1", true},
		{"pod gc", running, &zero, "system:serviceaccount:kube-system:pod-garbage-collector", true},
		{"other kube-system service account", running, &zero, "system:serviceaccount:kube-system:replicaset-controller", false},
		{"kubectl delete --now", running, &one, "alice", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := New().Validate(context.Background(), deleteAttrs(tc.pod, tc.grace, tc.who), nil)
			if (err == nil) != tc.allowed {
				t.Errorf("allowed = %v, want %v (err: %v)", err == nil, tc.allowed, err)
			}
		})
	}
}

func evictionAttrs(ns, name string, grace *int64, who string) admission.Attributes {
	eviction := &policy.Eviction{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	if grace != nil {
		eviction.DeleteOptions = &metav1.DeleteOptions{GracePeriodSeconds: grace}
	}
	return admission.NewAttributesRecord(eviction, nil, policy.Kind("Eviction").WithVersion("version"), ns, name,
		api.Resource("pods").WithVersion("version"), "eviction", admission.Create,
		&metav1.CreateOptions{}, false, &user.DefaultInfo{Name: who})
}

func TestValidateEviction(t *testing.T) {
	zero, thirty := int64(0), int64(30)
	running := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "running", Namespace: "default"},
		Spec: corev1.PodSpec{NodeName: "worker-1"}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	discussed := running.DeepCopy()
	discussed.Name = "discussed"
	discussed.Annotations = map[string]string{kyvernetria.DiscussedAnnotation: "true"}
	finished := running.DeepCopy()
	finished.Name = "finished"
	finished.Status.Phase = corev1.PodSucceeded

	factory := informers.NewSharedInformerFactory(fake.NewSimpleClientset(), 0)
	p := New()
	p.SetExternalKubeInformerFactory(factory)
	for _, pod := range []*corev1.Pod{running, discussed, finished} {
		if err := factory.Core().V1().Pods().Informer().GetStore().Add(pod); err != nil {
			t.Fatal(err)
		}
	}
	p.SetReadyFunc(func() bool { return true })
	if err := p.ValidateInitialization(); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name    string
		pod     string
		grace   *int64
		who     string
		allowed bool
	}{
		{"forced eviction of a running pod", "running", &zero, "alice", false},
		{"eviction with the pod's grace period", "running", nil, "alice", true},
		{"eviction with explicit grace", "running", &thirty, "alice", true},
		{"forced eviction after discussion", "discussed", &zero, "alice", true},
		{"forced eviction of a finished pod", "finished", &zero, "alice", true},
		{"pod unknown to the cache", "gone", &zero, "alice", true},
		{"pod gc", "running", &zero, "system:serviceaccount:kube-system:pod-garbage-collector", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := p.Validate(context.Background(), evictionAttrs("default", tc.pod, tc.grace, tc.who), nil)
			if (err == nil) != tc.allowed {
				t.Errorf("allowed = %v, want %v (err: %v)", err == nil, tc.allowed, err)
			}
		})
	}
}
