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
// (kubectl delete --force --grace-period=0) is refused until the pod carries
// the discussed annotation. Cluster components are not affected: the kubelet
// finishes already-terminating pods with grace 0, and the pod GC collects
// orphans.
package nobruteforce

import (
	"context"
	"fmt"
	"io"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/authentication/user"
	api "k8s.io/kubernetes/pkg/apis/core"

	"k8s.io/kubernetes/pkg/kyvernetria"
)

// PluginName is the name of the plugin.
const PluginName = "NoBruteForce"

// MessagePrefix starts every rejection so that clients can recognize them.
const MessagePrefix = "kyvernetria: let's talk first"

// Register registers the plugin.
func Register(plugins *admission.Plugins) {
	plugins.Register(PluginName, func(io.Reader) (admission.Interface, error) {
		return New(), nil
	})
}

// Plugin refuses immediate deletes of running pods.
type Plugin struct {
	*admission.Handler
}

var _ admission.ValidationInterface = &Plugin{}

// New returns a new NoBruteForce plugin.
func New() *Plugin {
	return &Plugin{Handler: admission.NewHandler(admission.Delete)}
}

// Validate refuses grace-period-0 deletes of pods that are not terminating yet.
func (p *Plugin) Validate(_ context.Context, a admission.Attributes, _ admission.ObjectInterfaces) error {
	if a.GetResource().GroupResource() != api.Resource("pods") || a.GetSubresource() != "" {
		return nil
	}
	opts, ok := a.GetOperationOptions().(*metav1.DeleteOptions)
	if !ok || opts.GracePeriodSeconds == nil || *opts.GracePeriodSeconds != 0 {
		return nil
	}
	if isClusterComponent(a.GetUserInfo()) {
		return nil
	}
	pod, ok := a.GetOldObject().(*api.Pod)
	if !ok {
		return nil
	}
	if pod.DeletionTimestamp != nil || pod.Annotations[kyvernetria.DiscussedAnnotation] == "true" {
		return nil
	}
	return admission.NewForbidden(a, fmt.Errorf(
		"%s. Pod %s/%s is still running and would be cut off without a chance to finish. "+
			"Delete it normally, or if you really mean it: kubectl annotate pod -n %s %s %s=true",
		MessagePrefix, pod.Namespace, pod.Name, pod.Namespace, pod.Name, kyvernetria.DiscussedAnnotation))
}

func isClusterComponent(u user.Info) bool {
	if u == nil {
		return false
	}
	name := u.GetName()
	if strings.HasPrefix(name, "system:node:") || strings.HasPrefix(name, "system:serviceaccount:kube-system:") {
		return true
	}
	switch name {
	case user.KubeControllerManager, user.KubeScheduler, user.APIServerUser:
		return true
	}
	return false
}
