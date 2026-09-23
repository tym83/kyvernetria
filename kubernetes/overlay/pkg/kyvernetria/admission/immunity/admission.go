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

// Package immunity implements the Immunity admission plugin.
//
// Models: stronger innate and adaptive immune responses on average
// (Klein & Flanagan 2016). Foreign "antigens" (privileged containers, host
// namespaces, hostPath volumes, floating image tags) are rejected at the
// door. Namespaces marked self are tolerated, as the immune system tolerates
// its own tissue. The downside is modeled too: when tolerance fails the plugin
// attacks legitimate workloads (autoimmunity), which `kyvctl diagnose
// autoimmune` helps to find.
package immunity

import (
	"context"
	"fmt"
	"io"
	"strings"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apiserver/pkg/admission"
	genericadmissioninitializer "k8s.io/apiserver/pkg/admission/initializer"
	"k8s.io/client-go/informers"
	corev1listers "k8s.io/client-go/listers/core/v1"
	api "k8s.io/kubernetes/pkg/apis/core"
	apiv1 "k8s.io/kubernetes/pkg/apis/core/v1"

	"k8s.io/kubernetes/pkg/kyvernetria"
)

// PluginName is the name of the plugin.
const PluginName = "Immunity"

// MessagePrefix starts every rejection so that clients can recognize them.
const MessagePrefix = "kyvernetria immunity"

// SelfNamespaces are tolerated without a label: the cluster's own organs.
var SelfNamespaces = map[string]bool{
	"kube-system":        true,
	"kube-public":        true,
	"kube-node-lease":    true,
	"local-path-storage": true,
}

// Register registers the plugin.
func Register(plugins *admission.Plugins) {
	plugins.Register(PluginName, func(io.Reader) (admission.Interface, error) {
		return New(), nil
	})
}

// Plugin rejects foreign pods outside self namespaces.
type Plugin struct {
	*admission.Handler
	namespaceLister corev1listers.NamespaceLister
}

var _ admission.ValidationInterface = &Plugin{}
var _ = genericadmissioninitializer.WantsExternalKubeInformerFactory(&Plugin{})

// New returns a new Immunity plugin.
func New() *Plugin {
	return &Plugin{Handler: admission.NewHandler(admission.Create)}
}

// SetExternalKubeInformerFactory wires the namespace lister.
func (p *Plugin) SetExternalKubeInformerFactory(f informers.SharedInformerFactory) {
	nsInformer := f.Core().V1().Namespaces()
	p.namespaceLister = nsInformer.Lister()
	p.SetReadyFunc(nsInformer.Informer().HasSynced)
}

// ValidateInitialization checks the plugin was fully wired.
func (p *Plugin) ValidateInitialization() error {
	if p.namespaceLister == nil {
		return fmt.Errorf("%s: missing namespace lister", PluginName)
	}
	return nil
}

// Validate rejects pods carrying foreign antigens.
func (p *Plugin) Validate(ctx context.Context, a admission.Attributes, _ admission.ObjectInterfaces) error {
	if a.GetResource().GroupResource() != api.Resource("pods") || a.GetSubresource() != "" {
		return nil
	}
	internal, ok := a.GetObject().(*api.Pod)
	if !ok {
		return nil
	}
	if !p.WaitForReady() {
		return admission.NewForbidden(a, fmt.Errorf("%s: not ready to recognize self yet", MessagePrefix))
	}
	if p.isSelf(a.GetNamespace()) {
		return nil
	}
	pod := &v1.Pod{}
	if err := apiv1.Convert_core_Pod_To_v1_Pod(internal, pod, nil); err != nil {
		return errors.NewInternalError(err)
	}
	antigens := Antigens(pod)
	if len(antigens) == 0 {
		return nil
	}
	return admission.NewForbidden(a, fmt.Errorf(
		"%s: %s. If this is your own workload, mark its namespace as self: kubectl label namespace %s %s=true",
		MessagePrefix, strings.Join(antigens, "; "), a.GetNamespace(), kyvernetria.SelfLabel))
}

func (p *Plugin) isSelf(namespace string) bool {
	if SelfNamespaces[namespace] {
		return true
	}
	ns, err := p.namespaceLister.Get(namespace)
	if err != nil {
		return false
	}
	return ns.Labels[kyvernetria.SelfLabel] == "true"
}

// Antigens lists everything in the pod the immune system does not accept.
func Antigens(pod *v1.Pod) []string {
	var found []string
	if pod.Spec.HostNetwork {
		found = append(found, "pod uses the host network")
	}
	if pod.Spec.HostPID {
		found = append(found, "pod uses the host PID namespace")
	}
	if pod.Spec.HostIPC {
		found = append(found, "pod uses the host IPC namespace")
	}
	for _, vol := range pod.Spec.Volumes {
		if vol.HostPath != nil {
			found = append(found, fmt.Sprintf("volume %q mounts a host path", vol.Name))
		}
	}
	visit := func(kind, name, image string, sc *v1.SecurityContext) {
		if sc != nil && sc.Privileged != nil && *sc.Privileged {
			found = append(found, fmt.Sprintf("%s %q is privileged", kind, name))
		}
		if FloatingTag(image) {
			found = append(found, fmt.Sprintf("%s %q uses an unpinned image %q (pin a tag other than latest, or a digest)", kind, name, image))
		}
	}
	for _, c := range pod.Spec.InitContainers {
		visit("init container", c.Name, c.Image, c.SecurityContext)
	}
	for _, c := range pod.Spec.Containers {
		visit("container", c.Name, c.Image, c.SecurityContext)
	}
	for _, c := range pod.Spec.EphemeralContainers {
		visit("ephemeral container", c.Name, c.Image, c.SecurityContext)
	}
	return found
}

// FloatingTag reports whether an image reference can change under the pod:
// no tag at all, or the latest tag. Digests are always pinned.
func FloatingTag(image string) bool {
	if strings.Contains(image, "@") {
		return false
	}
	name := image
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	i := strings.LastIndex(name, ":")
	if i < 0 {
		return true
	}
	return name[i+1:] == "latest"
}
