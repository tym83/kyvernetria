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

// Package nobruteforce implements the NoBruteForce admission plugin.
//
// Models: lower average physical aggression, a moderate to large difference
// (Archer 2004). A human deleting a running pod with grace period 0
// (kubectl delete --force --grace-period=0), directly or through the
// Eviction API (POST pods/NAME/eviction with deleteOptions.gracePeriodSeconds
// 0, e.g. kubectl drain --grace-period=0), is refused until the pod carries
// the discussed annotation. A grace period of 1 second (kubectl delete --now)
// is still a request to stop, not a kill, and is allowed.
//
// Cluster components are not affected: the kubelet finishes pods on its own
// node with grace 0, and the pod garbage collector, the only
// kube-controller-manager controller that deletes pods with grace 0
// (orphaned, terminated and terminating-on-unready-node pods), keeps working.
// Every other kube-system service account is treated like a person.
package nobruteforce

import (
	"context"
	"fmt"
	"io"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/admission"
	genericadmissioninitializer "k8s.io/apiserver/pkg/admission/initializer"
	"k8s.io/apiserver/pkg/authentication/serviceaccount"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/client-go/informers"
	corev1listers "k8s.io/client-go/listers/core/v1"
	api "k8s.io/kubernetes/pkg/apis/core"
	apiv1 "k8s.io/kubernetes/pkg/apis/core/v1"
	"k8s.io/kubernetes/pkg/apis/policy"

	"k8s.io/kubernetes/pkg/kyvernetria"
)

// PluginName is the name of the plugin.
const PluginName = "NoBruteForce"

// MessagePrefix starts every rejection so that clients can recognize them.
const MessagePrefix = "kyvernetria: let's talk first"

// forceDeleters are the kube-system service accounts that delete running
// pods with grace period 0 as part of their job. Upstream, only the pod
// garbage collector does (pkg/controller/podgc: NewDeleteOptions(0)); the
// taint-eviction, node-lifecycle, ReplicaSet, DaemonSet, Job and StatefulSet
// controllers use the pod's own grace period.
var forceDeleters = map[string]bool{
	serviceaccount.MakeUsername("kube-system", "pod-garbage-collector"): true,
}

// Register registers the plugin.
func Register(plugins *admission.Plugins) {
	plugins.Register(PluginName, func(io.Reader) (admission.Interface, error) {
		return New(), nil
	})
}

// Plugin refuses immediate deletes and evictions of running pods.
type Plugin struct {
	*admission.Handler
	podLister corev1listers.PodLister
}

var _ admission.ValidationInterface = &Plugin{}
var _ = genericadmissioninitializer.WantsExternalKubeInformerFactory(&Plugin{})

// New returns a new NoBruteForce plugin.
func New() *Plugin {
	return &Plugin{Handler: admission.NewHandler(admission.Delete, admission.Create)}
}

// SetExternalKubeInformerFactory wires the pod lister used for evictions.
// kube-apiserver already runs a pod informer (service account token
// validation, PodSecurity), so this adds no watch of its own.
func (p *Plugin) SetExternalKubeInformerFactory(f informers.SharedInformerFactory) {
	pods := f.Core().V1().Pods()
	p.podLister = pods.Lister()
	p.SetReadyFunc(pods.Informer().HasSynced)
}

// ValidateInitialization checks the plugin was fully wired.
func (p *Plugin) ValidateInitialization() error {
	if p.podLister == nil {
		return fmt.Errorf("%s: missing pod lister", PluginName)
	}
	return nil
}

// Validate refuses grace-period-0 deletes and evictions of pods that are
// not terminating yet.
func (p *Plugin) Validate(_ context.Context, a admission.Attributes, _ admission.ObjectInterfaces) error {
	if a.GetResource().GroupResource() != api.Resource("pods") {
		return nil
	}
	var pod *api.Pod
	switch {
	case a.GetOperation() == admission.Delete && a.GetSubresource() == "":
		opts, ok := a.GetOperationOptions().(*metav1.DeleteOptions)
		if !ok || !immediate(opts) || isClusterComponent(a.GetUserInfo()) {
			return nil
		}
		if pod, ok = a.GetOldObject().(*api.Pod); !ok {
			return nil
		}
	case a.GetOperation() == admission.Create && a.GetSubresource() == "eviction":
		eviction, ok := a.GetObject().(*policy.Eviction)
		if !ok || !immediate(eviction.DeleteOptions) || isClusterComponent(a.GetUserInfo()) {
			return nil
		}
		var err error
		if pod, err = p.evictedPod(a); pod == nil {
			return err
		}
	default:
		return nil
	}
	// The apiserver itself sets grace period 0 for pods that never started
	// (not scheduled) or already finished; nothing is cut off there.
	if !isRunning(pod) || pod.DeletionTimestamp != nil || pod.Annotations[kyvernetria.DiscussedAnnotation] == "true" {
		return nil
	}
	return admission.NewForbidden(a, fmt.Errorf(
		"%s. Pod %s/%s is still running and would be cut off without a chance to finish. "+
			"Delete or evict it normally, or if you really mean it: kubectl annotate pod -n %s %s %s=true",
		MessagePrefix, pod.Namespace, pod.Name, pod.Namespace, pod.Name, kyvernetria.DiscussedAnnotation))
}

// evictedPod loads the pod an eviction targets. A pod the cache doesn't
// know is let through: the eviction itself will report it missing.
func (p *Plugin) evictedPod(a admission.Attributes) (*api.Pod, error) {
	if !p.WaitForReady() {
		return nil, admission.NewForbidden(a, fmt.Errorf("%s: not ready to check evictions yet", MessagePrefix))
	}
	external, err := p.podLister.Pods(a.GetNamespace()).Get(a.GetName())
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, apierrors.NewInternalError(err)
	}
	pod := &api.Pod{}
	if err := apiv1.Convert_v1_Pod_To_core_Pod(external, pod, nil); err != nil {
		return nil, apierrors.NewInternalError(err)
	}
	return pod, nil
}

// immediate reports whether the options ask for grace period 0.
func immediate(opts *metav1.DeleteOptions) bool {
	return opts != nil && opts.GracePeriodSeconds != nil && *opts.GracePeriodSeconds == 0
}

func isClusterComponent(u user.Info) bool {
	if u == nil {
		return false
	}
	name := u.GetName()
	if strings.HasPrefix(name, "system:node:") || forceDeleters[name] {
		return true
	}
	switch name {
	case user.KubeControllerManager, user.KubeScheduler, user.APIServerUser:
		return true
	}
	return false
}

func isRunning(pod *api.Pod) bool {
	return pod.Spec.NodeName != "" && pod.Status.Phase != api.PodSucceeded && pod.Status.Phase != api.PodFailed
}
