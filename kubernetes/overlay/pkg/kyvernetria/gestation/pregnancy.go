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

package gestation

import (
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	"k8s.io/kubernetes/pkg/kyvernetria"
)

// PlacentaImage is what the placeholder pods run: the pause image, which
// does nothing and exits on SIGTERM. Pinned by digest, as Immunity wants.
const PlacentaImage = "registry.k8s.io/pause:3.10.2@sha256:f548e0e8e3dc1896ca956272154dde3314e8cc4fde0a57577ee9fa1c63f5baf4"

// Priority classes. Placeholders sit just below every ordinary pod: any pod
// that needs their room preempts them, and they never preempt anything.
// -1 is above the cluster-autoscaler's default cutoff for expendable pods
// (-10), so the autoscaler adds nodes for them: that is the point.
const (
	PlacentaPriorityClass = "kyvernetria-placenta"
	PlacentaPriority      = -1
)

// Trimester places now between conception and the due date: 1, 2 or 3,
// or 0 once the due date has come.
func Trimester(conceived, due, now time.Time) int32 {
	if !now.Before(due) {
		return 0
	}
	total := due.Sub(conceived)
	if total <= 0 {
		return 3
	}
	elapsed := now.Sub(conceived)
	if elapsed < 0 {
		return 1
	}
	t := 1 + int32(3*elapsed/total)
	if t > 3 {
		t = 3
	}
	return t
}

// Reserved is how many placeholder pods hold room in a trimester. At term
// the reservation covers every replica the service launches with plus the
// extra one newborn care adds. Before that it grows in steps: a quarter in
// the first trimester, two thirds in the second, all of it in the third
// (rounded up). Maternal plasma volume grows by about 45-50% over a
// pregnancy: a little in the first trimester, fastest in the second, and
// close to its peak before term (Aguree & Gernand 2019). The principle
// carried over is the shape, capacity growing ahead of the demand and
// complete before the due date; the steps are engineering choices.
func Reserved(trimester, replicas int32) int32 {
	term := replicas + 1
	switch trimester {
	case 1:
		return (term + 3) / 4
	case 2:
		return (2*term + 2) / 3
	}
	return term // third trimester, and due but not yet delivered
}

// PlacentaName is the name of the placeholder Deployment.
func PlacentaName(g *Gestation) string {
	return g.Spec.Deployment + "-placenta"
}

// ParseSize reads the per-replica size.
func ParseSize(s Size) (v1.ResourceList, error) {
	cpu, err := resource.ParseQuantity(s.CPU)
	if err != nil {
		return nil, fmt.Errorf("cpu %q: %w", s.CPU, err)
	}
	mem, err := resource.ParseQuantity(s.Memory)
	if err != nil {
		return nil, fmt.Errorf("memory %q: %w", s.Memory, err)
	}
	if cpu.Sign() <= 0 || mem.Sign() <= 0 {
		return nil, fmt.Errorf("size must be positive, got %s/%s", s.CPU, s.Memory)
	}
	return v1.ResourceList{v1.ResourceCPU: cpu, v1.ResourceMemory: mem}, nil
}

// Placenta builds the placeholder Deployment. Each placeholder is the size
// of one real replica, so it reserves room where a real pod fits, not in
// scraps. When the service's Deployment already exists, the placeholders
// copy where its pods may run (node selector, affinity, tolerations), so
// the room is reserved where it can be used.
func Placenta(g *Gestation, gestationUID types.UID, reserved int32, size v1.ResourceList, template *v1.PodTemplateSpec) *appsv1.Deployment {
	labels := map[string]string{kyvernetria.PlacentaLabel: g.Name}
	spec := v1.PodSpec{
		PriorityClassName:             PlacentaPriorityClass,
		TerminationGracePeriodSeconds: ptr.To[int64](1),
		AutomountServiceAccountToken:  ptr.To(false),
		EnableServiceLinks:            ptr.To(false),
		SecurityContext: &v1.PodSecurityContext{
			RunAsNonRoot:   ptr.To(true),
			RunAsUser:      ptr.To[int64](65535),
			SeccompProfile: &v1.SeccompProfile{Type: v1.SeccompProfileTypeRuntimeDefault},
		},
		Containers: []v1.Container{{
			Name:  "placenta",
			Image: PlacentaImage,
			Resources: v1.ResourceRequirements{
				Requests: size.DeepCopy(),
			},
			SecurityContext: &v1.SecurityContext{
				AllowPrivilegeEscalation: ptr.To(false),
				ReadOnlyRootFilesystem:   ptr.To(true),
				Capabilities:             &v1.Capabilities{Drop: []v1.Capability{"ALL"}},
			},
		}},
	}
	if template != nil {
		spec.NodeSelector = template.Spec.NodeSelector
		spec.Tolerations = template.Spec.Tolerations
		if a := template.Spec.Affinity; a != nil && a.NodeAffinity != nil {
			spec.Affinity = &v1.Affinity{NodeAffinity: a.NodeAffinity.DeepCopy()}
		}
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      PlacentaName(g),
			Namespace: g.Namespace,
			Labels:    map[string]string{kyvernetria.PlacentaLabel: g.Name, ManagedByLabel: ManagedBy},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: Group + "/" + Version, Kind: Kind, Name: g.Name, UID: gestationUID,
				Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(false),
			}},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(reserved),
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       spec,
			},
		},
	}
}

// PriorityClasses are the classes the gestation controller installs.
func PriorityClasses() []*schedulingv1.PriorityClass {
	never := v1.PreemptNever
	out := []*schedulingv1.PriorityClass{{
		ObjectMeta: metav1.ObjectMeta{Name: PlacentaPriorityClass, Labels: map[string]string{ManagedByLabel: ManagedBy}},
		Value:      PlacentaPriority, PreemptionPolicy: &never,
		Description: "Kyvernetria: placeholders holding room for a service before its launch. Any ordinary pod preempts them.",
	}}
	for _, t := range careTiers {
		out = append(out, &schedulingv1.PriorityClass{
			ObjectMeta: metav1.ObjectMeta{Name: t.class, Labels: map[string]string{ManagedByLabel: ManagedBy}},
			Value:      t.priority, PreemptionPolicy: &never,
			Description: fmt.Sprintf("Kyvernetria: pods of a newborn service, hours %d to %d after launch. "+
				"Protected from preemption by ordinary pods; never preempts anything itself.",
				int(t.from/time.Hour), int(t.until/time.Hour)),
		})
	}
	return out
}
