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

// Package startingdose implements the StartingDose admission plugin.
//
// Models: sex differences in pharmacokinetics (Zucker & Prendergast 2020).
// At the same dose women usually reach higher blood concentrations and
// clear drugs more slowly, so "one dose fits all", calibrated mostly on
// men, overmedicates many women; the FDA's answer for zolpidem (2013) was a
// lower starting dose. Kyvernetria takes the principle, not a number: dose
// deliberately, start low, then adjust from what you measure.
//
// Upstream gives a container that states no requests no dose at all: the
// pod is BestEffort, the scheduler counts it as free and the kubelet evicts
// it first. In a namespace labelled kyvernetria.io/dosing=start-low, this
// plugin gives each such container a small starting request for CPU and
// memory instead. It never changes a value someone set: a request or limit
// in the pod, a LimitRange default (LimitRanger runs first) and pod-level
// resources all win, and mutating webhooks run afterwards. Only new pods
// are dosed; mirror pods are left alone, since the kubelet owns their spec.
package startingdose

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/admission"
	genericadmissioninitializer "k8s.io/apiserver/pkg/admission/initializer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corev1listers "k8s.io/client-go/listers/core/v1"
	api "k8s.io/kubernetes/pkg/apis/core"

	"k8s.io/kubernetes/pkg/kyvernetria"
)

// PluginName is the name of the plugin.
const PluginName = "StartingDose"

// Doses are the starting requests, per resource.
var Doses = api.ResourceList{
	api.ResourceCPU:    resource.MustParse(kyvernetria.StartingDoseCPU),
	api.ResourceMemory: resource.MustParse(kyvernetria.StartingDoseMemory),
}

// Register registers the plugin.
func Register(plugins *admission.Plugins) {
	plugins.Register(PluginName, func(io.Reader) (admission.Interface, error) {
		return New(), nil
	})
}

// Plugin gives undosed containers a starting request in opted-in namespaces.
type Plugin struct {
	*admission.Handler
	client          kubernetes.Interface
	namespaceLister corev1listers.NamespaceLister
}

var _ admission.MutationInterface = &Plugin{}
var _ = genericadmissioninitializer.WantsExternalKubeInformerFactory(&Plugin{})
var _ = genericadmissioninitializer.WantsExternalKubeClientSet(&Plugin{})

// New returns a new StartingDose plugin.
func New() *Plugin {
	return &Plugin{Handler: admission.NewHandler(admission.Create)}
}

// SetExternalKubeInformerFactory wires the namespace lister.
func (p *Plugin) SetExternalKubeInformerFactory(f informers.SharedInformerFactory) {
	nsInformer := f.Core().V1().Namespaces()
	p.namespaceLister = nsInformer.Lister()
	p.SetReadyFunc(nsInformer.Informer().HasSynced)
}

// SetExternalKubeClientSet wires the client used for namespaces the
// informer hasn't seen yet.
func (p *Plugin) SetExternalKubeClientSet(client kubernetes.Interface) {
	p.client = client
}

// ValidateInitialization checks the plugin was fully wired.
func (p *Plugin) ValidateInitialization() error {
	if p.namespaceLister == nil {
		return fmt.Errorf("%s: missing namespace lister", PluginName)
	}
	if p.client == nil {
		return fmt.Errorf("%s: missing client", PluginName)
	}
	return nil
}

// Admit doses the containers of a new pod in an opted-in namespace.
func (p *Plugin) Admit(ctx context.Context, a admission.Attributes, _ admission.ObjectInterfaces) error {
	if a.GetResource().GroupResource() != api.Resource("pods") || a.GetSubresource() != "" {
		return nil
	}
	pod, ok := a.GetObject().(*api.Pod)
	if !ok || !NeedsDose(pod) {
		return nil
	}
	if !p.WaitForReady() {
		return admission.NewForbidden(a, fmt.Errorf("%s: not ready to read namespace labels yet", PluginName))
	}
	optedIn, err := p.optedIn(ctx, a.GetNamespace())
	if err != nil {
		return admission.NewForbidden(a, fmt.Errorf("%s: can't read the labels of namespace %s: %w", PluginName, a.GetNamespace(), err))
	}
	if optedIn {
		Dose(pod)
	}
	return nil
}

// optedIn reports whether a namespace asked for starting doses. A namespace
// created a moment ago may not be in the informer yet; like
// NamespaceLifecycle, the plugin then asks the apiserver.
func (p *Plugin) optedIn(ctx context.Context, namespace string) (bool, error) {
	ns, err := p.namespaceLister.Get(namespace)
	if errors.IsNotFound(err) {
		ns, err = p.client.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	}
	if errors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return ns.Labels[kyvernetria.DosingLabel] == kyvernetria.DosingStartLow, nil
}

// NeedsDose reports whether any container of the pod would get a dose.
func NeedsDose(pod *api.Pod) bool {
	if pod.Spec.Resources != nil || pod.Annotations[api.MirrorPodAnnotationKey] != "" {
		return false
	}
	need := false
	visit(pod, func(c *api.Container) {
		need = need || len(missing(c)) > 0
	})
	return need
}

// Dose gives every container a starting request for each resource it has
// neither a request nor a limit for, and records what it did in the
// starting-dose annotation. It reports whether it changed anything.
func Dose(pod *api.Pod) bool {
	if pod.Spec.Resources != nil || pod.Annotations[api.MirrorPodAnnotationKey] != "" {
		return false
	}
	var record []string
	visit(pod, func(c *api.Container) {
		names := missing(c)
		if len(names) == 0 {
			return
		}
		if c.Resources.Requests == nil {
			c.Resources.Requests = api.ResourceList{}
		}
		dosed := make([]string, 0, len(names))
		for _, name := range names {
			c.Resources.Requests[name] = Doses[name].DeepCopy()
			dosed = append(dosed, string(name))
		}
		record = append(record, c.Name+"="+strings.Join(dosed, ","))
	})
	if len(record) == 0 {
		return false
	}
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[kyvernetria.StartingDoseAnnotation] = strings.Join(record, ";")
	return true
}

// missing lists the dosed resources a container states nothing about, in a
// stable order. After API defaulting a limit always comes with a request,
// so "no request" means the resource was left out entirely.
func missing(c *api.Container) []api.ResourceName {
	var names []api.ResourceName
	for name := range Doses {
		_, requested := c.Resources.Requests[name]
		_, limited := c.Resources.Limits[name]
		if !requested && !limited {
			names = append(names, name)
		}
	}
	sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })
	return names
}

// visit calls fn for every init and regular container. Ephemeral containers
// cannot have resources.
func visit(pod *api.Pod, fn func(*api.Container)) {
	for i := range pod.Spec.InitContainers {
		fn(&pod.Spec.InitContainers[i])
	}
	for i := range pod.Spec.Containers {
		fn(&pod.Spec.Containers[i])
	}
}
