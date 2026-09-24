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
// (Klein & Flanagan 2016). Foreign "antigens" are rejected at the door:
// everything the Pod Security Standards "baseline" profile forbids
// (privileged containers, host namespaces, host ports, hostPath volumes,
// capabilities beyond the baseline set, unmasked /proc, unconfined seccomp
// or AppArmor, Windows host processes), plus explicit privilege escalation
// and floating image tags. The check runs when a pod is created, when its
// containers are changed (kubectl set image) and when ephemeral containers
// are added (kubectl debug); on changes only antigens the change introduces
// count, so existing pods can still be updated for unrelated reasons.
//
// Namespaces marked self are tolerated, as the immune system tolerates its
// own tissue. The downside is modeled too: when tolerance fails the plugin
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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apiserver/pkg/admission"
	genericadmissioninitializer "k8s.io/apiserver/pkg/admission/initializer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
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

// BaselineCapabilities are the capabilities a container may add without
// being foreign: the Pod Security Standards baseline allowlist, i.e. the
// container runtimes' default set. Anything else (SYS_ADMIN, NET_ADMIN,
// SYS_PTRACE, SYS_MODULE, BPF, ALL, ...) reaches into the node.
var BaselineCapabilities = sets.New(
	"AUDIT_WRITE", "CHOWN", "DAC_OVERRIDE", "FOWNER", "FSETID", "KILL", "MKNOD",
	"NET_BIND_SERVICE", "SETFCAP", "SETGID", "SETPCAP", "SETUID", "SYS_CHROOT",
)

// Register registers the plugin.
func Register(plugins *admission.Plugins) {
	plugins.Register(PluginName, func(io.Reader) (admission.Interface, error) {
		return New(), nil
	})
}

// Plugin rejects foreign pods outside self namespaces.
type Plugin struct {
	*admission.Handler
	client          kubernetes.Interface
	namespaceLister corev1listers.NamespaceLister
}

var _ admission.ValidationInterface = &Plugin{}
var _ = genericadmissioninitializer.WantsExternalKubeInformerFactory(&Plugin{})
var _ = genericadmissioninitializer.WantsExternalKubeClientSet(&Plugin{})

// New returns a new Immunity plugin.
func New() *Plugin {
	return &Plugin{Handler: admission.NewHandler(admission.Create, admission.Update)}
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

// Validate rejects pods carrying foreign antigens.
func (p *Plugin) Validate(ctx context.Context, a admission.Attributes, _ admission.ObjectInterfaces) error {
	if a.GetResource().GroupResource() != api.Resource("pods") {
		return nil
	}
	switch a.GetSubresource() {
	case "":
	case "ephemeralcontainers":
		if a.GetOperation() != admission.Update {
			return nil
		}
	default:
		return nil
	}
	pod, err := toV1(a.GetObject())
	if pod == nil {
		return err
	}
	var old *v1.Pod
	if a.GetOperation() == admission.Update {
		if old, err = toV1(a.GetOldObject()); old == nil {
			return err
		}
	}
	antigens := NewAntigens(old, pod)
	if len(antigens) == 0 {
		return nil
	}
	if !p.WaitForReady() {
		return admission.NewForbidden(a, fmt.Errorf("%s: not ready to recognize self yet", MessagePrefix))
	}
	self, err := p.isSelf(ctx, a.GetNamespace())
	if err != nil {
		return admission.NewForbidden(a, fmt.Errorf("%s: can't tell whether namespace %s is self: %w", MessagePrefix, a.GetNamespace(), err))
	}
	if self {
		return nil
	}
	return admission.NewForbidden(a, fmt.Errorf(
		"%s: %s. If this is your own workload, mark its namespace as self: kubectl label namespace %s %s=true",
		MessagePrefix, strings.Join(antigens, "; "), a.GetNamespace(), kyvernetria.SelfLabel))
}

func toV1(obj interface{}) (*v1.Pod, error) {
	internal, ok := obj.(*api.Pod)
	if !ok {
		return nil, nil
	}
	pod := &v1.Pod{}
	if err := apiv1.Convert_core_Pod_To_v1_Pod(internal, pod, nil); err != nil {
		return nil, errors.NewInternalError(err)
	}
	return pod, nil
}

// isSelf reports whether a namespace is tolerated. A namespace created a
// moment ago may not be in the informer yet; like NamespaceLifecycle, the
// plugin then asks the apiserver instead of treating it as foreign.
func (p *Plugin) isSelf(ctx context.Context, namespace string) (bool, error) {
	if SelfNamespaces[namespace] {
		return true, nil
	}
	ns, err := p.namespaceLister.Get(namespace)
	if errors.IsNotFound(err) && p.client != nil {
		ns, err = p.client.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	}
	if errors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return ns.Labels[kyvernetria.SelfLabel] == "true", nil
}

// NewAntigens lists the antigens of pod that old did not already carry. For
// a create (old is nil) that is every antigen.
func NewAntigens(old, pod *v1.Pod) []string {
	found := Antigens(pod)
	if old == nil {
		return found
	}
	had := sets.New(Antigens(old)...)
	var introduced []string
	for _, a := range found {
		if !had.Has(a) {
			introduced = append(introduced, a)
		}
	}
	return introduced
}

// Antigens lists everything in the pod the immune system does not accept.
func Antigens(pod *v1.Pod) []string {
	var found []string
	add := func(format string, args ...interface{}) {
		found = append(found, fmt.Sprintf(format, args...))
	}
	spec := &pod.Spec
	if spec.HostNetwork {
		add("pod uses the host network")
	}
	if spec.HostPID {
		add("pod uses the host PID namespace")
	}
	if spec.HostIPC {
		add("pod uses the host IPC namespace")
	}
	for _, vol := range spec.Volumes {
		if vol.HostPath != nil {
			add("volume %q mounts a host path", vol.Name)
		}
	}
	if psc := spec.SecurityContext; psc != nil {
		if psc.SeccompProfile != nil && psc.SeccompProfile.Type == v1.SeccompProfileTypeUnconfined {
			add("pod runs without a seccomp profile (Unconfined)")
		}
		if psc.AppArmorProfile != nil && psc.AppArmorProfile.Type == v1.AppArmorProfileTypeUnconfined {
			add("pod runs without an AppArmor profile (Unconfined)")
		}
		if psc.WindowsOptions != nil && psc.WindowsOptions.HostProcess != nil && *psc.WindowsOptions.HostProcess {
			add("pod runs as a Windows host process")
		}
	}
	visit := func(kind, name, image string, ports []v1.ContainerPort, sc *v1.SecurityContext) {
		for _, port := range ports {
			if port.HostPort != 0 {
				add("%s %q binds host port %d", kind, name, port.HostPort)
			}
		}
		if pod.Annotations[v1.DeprecatedAppArmorBetaContainerAnnotationKeyPrefix+name] == v1.DeprecatedAppArmorBetaProfileNameUnconfined {
			add("%s %q runs without an AppArmor profile (unconfined)", kind, name)
		}
		if FloatingTag(image) {
			add("%s %q uses an unpinned image %q (pin a tag other than latest, or a digest)", kind, name, image)
		}
		if sc == nil {
			return
		}
		if sc.Privileged != nil && *sc.Privileged {
			add("%s %q is privileged", kind, name)
		}
		if sc.AllowPrivilegeEscalation != nil && *sc.AllowPrivilegeEscalation {
			add("%s %q allows privilege escalation", kind, name)
		}
		if sc.Capabilities != nil {
			for _, c := range sc.Capabilities.Add {
				capability := strings.TrimPrefix(strings.ToUpper(string(c)), "CAP_")
				if !BaselineCapabilities.Has(capability) {
					add("%s %q adds capability %s", kind, name, capability)
				}
			}
		}
		if sc.ProcMount != nil && *sc.ProcMount == v1.UnmaskedProcMount {
			add("%s %q mounts /proc unmasked", kind, name)
		}
		if sc.SeccompProfile != nil && sc.SeccompProfile.Type == v1.SeccompProfileTypeUnconfined {
			add("%s %q runs without a seccomp profile (Unconfined)", kind, name)
		}
		if sc.AppArmorProfile != nil && sc.AppArmorProfile.Type == v1.AppArmorProfileTypeUnconfined {
			add("%s %q runs without an AppArmor profile (Unconfined)", kind, name)
		}
		if sc.WindowsOptions != nil && sc.WindowsOptions.HostProcess != nil && *sc.WindowsOptions.HostProcess {
			add("%s %q runs as a Windows host process", kind, name)
		}
	}
	for _, c := range spec.InitContainers {
		visit("init container", c.Name, c.Image, c.Ports, c.SecurityContext)
	}
	for _, c := range spec.Containers {
		visit("container", c.Name, c.Image, c.Ports, c.SecurityContext)
	}
	for _, c := range spec.EphemeralContainers {
		visit("ephemeral container", c.Name, c.Image, c.Ports, c.SecurityContext)
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
