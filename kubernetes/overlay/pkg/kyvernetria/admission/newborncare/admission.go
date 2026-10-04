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

// Package newborncare implements the NewbornCare admission plugin.
//
// Models: a newborn is protected at first by antibodies it got from its
// mother, which wane over its first months (Palmeira et al. 2012; Niewiesk
// 2014). The gestation controller lists the services in newborn care in
// their namespace's kyvernetria.io/newborn-care annotation, each with the
// priority class of the current care tier. This plugin gives a new pod of
// such a service that class, so the scheduler does not preempt it for an
// ordinary pod and the kubelet evicts it later under node pressure.
//
// A pod's priority is fixed when the pod is created; Kubernetes cannot
// change it afterwards. So the protection tapers for pods created later in
// the care period (replacements, scale-ups), and a pod keeps the class it
// was created with until it is replaced. The plugin never overrides a
// priority class someone chose, and never lowers a pod below the cluster's
// global default.
package newborncare

import (
	"context"
	"fmt"
	"io"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apiserver/pkg/admission"
	genericadmissioninitializer "k8s.io/apiserver/pkg/admission/initializer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corev1listers "k8s.io/client-go/listers/core/v1"
	schedulingv1listers "k8s.io/client-go/listers/scheduling/v1"
	api "k8s.io/kubernetes/pkg/apis/core"

	"k8s.io/kubernetes/pkg/kyvernetria"
	"k8s.io/kubernetes/pkg/kyvernetria/gestation"
)

// PluginName is the name of the plugin.
const PluginName = "NewbornCare"

// Register registers the plugin.
func Register(plugins *admission.Plugins) {
	plugins.Register(PluginName, func(io.Reader) (admission.Interface, error) {
		return New(), nil
	})
}

// Plugin gives the pods of newborn services their care tier's priority.
type Plugin struct {
	*admission.Handler
	client          kubernetes.Interface
	namespaceLister corev1listers.NamespaceLister
	classLister     schedulingv1listers.PriorityClassLister
}

var _ admission.MutationInterface = &Plugin{}
var _ = genericadmissioninitializer.WantsExternalKubeInformerFactory(&Plugin{})
var _ = genericadmissioninitializer.WantsExternalKubeClientSet(&Plugin{})

// New returns a new NewbornCare plugin.
func New() *Plugin {
	return &Plugin{Handler: admission.NewHandler(admission.Create)}
}

// SetExternalKubeInformerFactory wires the listers.
func (p *Plugin) SetExternalKubeInformerFactory(f informers.SharedInformerFactory) {
	ns := f.Core().V1().Namespaces()
	pcs := f.Scheduling().V1().PriorityClasses()
	p.namespaceLister = ns.Lister()
	p.classLister = pcs.Lister()
	p.SetReadyFunc(func() bool { return ns.Informer().HasSynced() && pcs.Informer().HasSynced() })
}

// SetExternalKubeClientSet wires the client used for namespaces the
// informer hasn't seen yet.
func (p *Plugin) SetExternalKubeClientSet(client kubernetes.Interface) {
	p.client = client
}

// ValidateInitialization checks the plugin was fully wired.
func (p *Plugin) ValidateInitialization() error {
	if p.namespaceLister == nil || p.classLister == nil {
		return fmt.Errorf("%s: missing listers", PluginName)
	}
	if p.client == nil {
		return fmt.Errorf("%s: missing client", PluginName)
	}
	return nil
}

// Admit sets the care tier's priority class on a new pod of a newborn.
func (p *Plugin) Admit(ctx context.Context, a admission.Attributes, _ admission.ObjectInterfaces) error {
	if a.GetResource().GroupResource() != api.Resource("pods") || a.GetSubresource() != "" {
		return nil
	}
	pod, ok := a.GetObject().(*api.Pod)
	if !ok || pod.Spec.PriorityClassName != "" || pod.Spec.Priority != nil || pod.Annotations[api.MirrorPodAnnotationKey] != "" {
		return nil
	}
	if !p.WaitForReady() {
		// Protection is a kindness, not a gate: never block a pod for it.
		return nil
	}
	raw, err := p.annotation(ctx, a.GetNamespace())
	if err != nil || raw == "" {
		return nil
	}
	class := p.classFor(raw, pod.Labels)
	if class == "" {
		return nil
	}
	pod.Spec.PriorityClassName = class
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[kyvernetria.NewbornCareAnnotation] = class
	return nil
}

// classFor picks the class of the first newborn entry that selects the
// pod, if that class exists and is above the cluster's global default.
func (p *Plugin) classFor(raw string, podLabels map[string]string) string {
	entries, err := gestation.DecodeNewborns(raw)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !gestation.IsNewbornClass(e.PriorityClass) {
			continue // only the care tiers, whatever the annotation says
		}
		sel, err := labels.Parse(e.Selector)
		if err != nil || sel.Empty() || !sel.Matches(labels.Set(podLabels)) {
			continue
		}
		pc, err := p.classLister.Get(e.PriorityClass)
		if err != nil {
			return ""
		}
		classes, err := p.classLister.List(labels.Everything())
		if err != nil {
			return ""
		}
		for _, other := range classes {
			if other.GlobalDefault && other.Value >= pc.Value {
				return "" // the default already protects it as much
			}
		}
		return pc.Name
	}
	return ""
}

func (p *Plugin) annotation(ctx context.Context, namespace string) (string, error) {
	ns, err := p.namespaceLister.Get(namespace)
	if apierrors.IsNotFound(err) {
		ns, err = p.client.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	}
	if err != nil {
		return "", err
	}
	return ns.Annotations[kyvernetria.NewbornCareAnnotation], nil
}
