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
	"context"
	"fmt"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// Screening checks.
const (
	CheckDeployment  = "deployment"
	CheckReservation = "reservation"
	CheckImage       = "image"
	CheckReferences  = "references"
	CheckVolumes     = "volumes"
	CheckQuota       = "quota"
	CheckProbes      = "probes"
	CheckRequests    = "requests"
	CheckPDB         = "disruption-budget"
)

// Lookup is what screening needs to know about the cluster. Secrets and
// ConfigMaps are looked up by name only: an implementation must never read
// their data.
type Lookup interface {
	SecretExists(ctx context.Context, namespace, name string) (bool, error)
	ConfigMapExists(ctx context.Context, namespace, name string) (bool, error)
	// PVC returns nil when the claim does not exist.
	PVC(namespace, name string) (*v1.PersistentVolumeClaim, error)
	// StorageClass returns nil when the class does not exist; with name ""
	// it returns the default class, if any.
	StorageClass(name string) (*storagev1.StorageClass, error)
	ResourceQuotas(namespace string) ([]*v1.ResourceQuota, error)
	PDBs(namespace string) ([]*policyv1.PodDisruptionBudget, error)
	// Image checks that an image reference resolves, best effort. It
	// returns a Finding result and a sentence.
	Image(ctx context.Context, image string) (string, string)
}

// Input is what is screened.
type Input struct {
	Gestation *Gestation
	// Deployment is nil when it does not exist yet.
	Deployment *appsv1.Deployment
	// Placenta is what the placeholders currently take from quotas; it is
	// given back at birth, so it counts as available.
	Placenta v1.ResourceList
}

// Screen runs every check. A check that cannot run says so as info.
func Screen(ctx context.Context, in Input, l Lookup) []Finding {
	g := in.Gestation
	size, sizeErr := ParseSize(g.Spec.Size)
	var out []Finding
	add := func(check, result, format string, args ...interface{}) {
		out = append(out, Finding{Check: check, Result: result, Message: fmt.Sprintf(format, args...)})
	}

	if sizeErr != nil {
		add(CheckReservation, ResultFail, "the size can't be read: %v", sizeErr)
	}
	if sizeErr == nil {
		out = append(out, screenQuota(g, size, in.Placenta, l)...)
	}
	d := in.Deployment
	if d == nil {
		add(CheckDeployment, ResultInfo, "Deployment %s doesn't exist yet, so there is nothing more to look at. "+
			"Create it paused or with 0 replicas, and the next screening will look at its pods.", g.Spec.Deployment)
		return out
	}
	add(CheckDeployment, ResultClear, "Deployment %s exists", d.Name)
	tmpl := &d.Spec.Template
	if sizeErr == nil {
		out = append(out, screenReservation(tmpl, size)...)
	}
	for _, c := range allContainers(tmpl) {
		result, msg := l.Image(ctx, c.Image)
		add(CheckImage, result, "%s: %s", c.Name, msg)
	}
	out = append(out, screenReferences(ctx, d.Namespace, tmpl, l)...)
	out = append(out, screenVolumes(d.Namespace, tmpl, l)...)
	out = append(out, screenProbes(tmpl)...)
	out = append(out, screenRequests(tmpl)...)
	out = append(out, screenPDB(d, l)...)
	return out
}

func allContainers(t *v1.PodTemplateSpec) []v1.Container {
	return append(append([]v1.Container(nil), t.Spec.InitContainers...), t.Spec.Containers...)
}

func screenReservation(t *v1.PodTemplateSpec, size v1.ResourceList) []Finding {
	pod := podRequests(t)
	var short []string
	for _, r := range []v1.ResourceName{v1.ResourceCPU, v1.ResourceMemory} {
		want, have := pod[r], size[r]
		if want.Cmp(have) > 0 {
			short = append(short, fmt.Sprintf("%s %s (room reserved: %s)", r, want.String(), have.String()))
		}
	}
	if len(short) > 0 {
		return []Finding{{Check: CheckReservation, Result: ResultWarn,
			Message: "each pod asks for more than the room reserved for it: " + strings.Join(short, ", ") +
				". Conceive it again with a larger --size"}}
	}
	return []Finding{{Check: CheckReservation, Result: ResultClear, Message: "each pod fits in the room reserved for it"}}
}

// podRequests sums the app containers' requests.
func podRequests(t *v1.PodTemplateSpec) v1.ResourceList {
	out := v1.ResourceList{}
	for _, c := range t.Spec.Containers {
		for name, q := range c.Resources.Requests {
			sum := out[name]
			sum.Add(q)
			out[name] = sum
		}
	}
	return out
}

func screenReferences(ctx context.Context, ns string, t *v1.PodTemplateSpec, l Lookup) []Finding {
	secrets, configMaps := References(t)
	var missing []string
	var errs []string
	for _, name := range secrets {
		ok, err := l.SecretExists(ctx, ns, name)
		switch {
		case err != nil:
			errs = append(errs, "secret "+name)
		case !ok:
			missing = append(missing, "Secret "+name)
		}
	}
	for _, name := range configMaps {
		ok, err := l.ConfigMapExists(ctx, ns, name)
		switch {
		case err != nil:
			errs = append(errs, "configmap "+name)
		case !ok:
			missing = append(missing, "ConfigMap "+name)
		}
	}
	switch {
	case len(missing) > 0:
		return []Finding{{Check: CheckReferences, Result: ResultFail,
			Message: "it needs what isn't there yet: " + strings.Join(missing, ", ")}}
	case len(errs) > 0:
		return []Finding{{Check: CheckReferences, Result: ResultInfo,
			Message: "couldn't look up " + strings.Join(errs, ", ")}}
	case len(secrets)+len(configMaps) == 0:
		return []Finding{{Check: CheckReferences, Result: ResultClear, Message: "it refers to no Secrets or ConfigMaps"}}
	}
	return []Finding{{Check: CheckReferences, Result: ResultClear,
		Message: fmt.Sprintf("all %d Secrets and ConfigMaps it needs exist (names checked, never contents)", len(secrets)+len(configMaps))}}
}

// References lists the Secrets and ConfigMaps a pod template needs, by
// name, sorted, without the optional ones.
func References(t *v1.PodTemplateSpec) (secrets, configMaps []string) {
	s, c := map[string]bool{}, map[string]bool{}
	optional := func(b *bool) bool { return b != nil && *b }
	for _, ref := range t.Spec.ImagePullSecrets {
		if ref.Name != "" {
			s[ref.Name] = true
		}
	}
	for _, vol := range t.Spec.Volumes {
		if v := vol.Secret; v != nil && !optional(v.Optional) {
			s[v.SecretName] = true
		}
		if v := vol.ConfigMap; v != nil && !optional(v.Optional) {
			c[v.Name] = true
		}
		if p := vol.Projected; p != nil {
			for _, src := range p.Sources {
				if v := src.Secret; v != nil && !optional(v.Optional) {
					s[v.Name] = true
				}
				if v := src.ConfigMap; v != nil && !optional(v.Optional) {
					c[v.Name] = true
				}
			}
		}
	}
	for _, ctr := range allContainers(t) {
		for _, e := range ctr.EnvFrom {
			if r := e.SecretRef; r != nil && !optional(r.Optional) {
				s[r.Name] = true
			}
			if r := e.ConfigMapRef; r != nil && !optional(r.Optional) {
				c[r.Name] = true
			}
		}
		for _, e := range ctr.Env {
			if e.ValueFrom == nil {
				continue
			}
			if r := e.ValueFrom.SecretKeyRef; r != nil && !optional(r.Optional) {
				s[r.Name] = true
			}
			if r := e.ValueFrom.ConfigMapKeyRef; r != nil && !optional(r.Optional) {
				c[r.Name] = true
			}
		}
	}
	return sortedKeys(s), sortedKeys(c)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		if k != "" {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func screenVolumes(ns string, t *v1.PodTemplateSpec, l Lookup) []Finding {
	var out []Finding
	add := func(result, format string, args ...interface{}) {
		out = append(out, Finding{Check: CheckVolumes, Result: result, Message: fmt.Sprintf(format, args...)})
	}
	for _, vol := range t.Spec.Volumes {
		switch {
		case vol.PersistentVolumeClaim != nil:
			name := vol.PersistentVolumeClaim.ClaimName
			pvc, err := l.PVC(ns, name)
			switch {
			case err != nil:
				add(ResultInfo, "couldn't look up PersistentVolumeClaim %s: %v", name, err)
			case pvc == nil:
				add(ResultFail, "PersistentVolumeClaim %s doesn't exist", name)
			case pvc.Status.Phase == v1.ClaimBound:
				add(ResultClear, "PersistentVolumeClaim %s is bound", name)
			default:
				out = append(out, bindable(fmt.Sprintf("PersistentVolumeClaim %s", name), pvc.Spec.StorageClassName,
					pvc.Spec.Resources.Requests, l)...)
			}
		case vol.Ephemeral != nil && vol.Ephemeral.VolumeClaimTemplate != nil:
			spec := vol.Ephemeral.VolumeClaimTemplate.Spec
			out = append(out, bindable(fmt.Sprintf("ephemeral volume %s", vol.Name), spec.StorageClassName,
				spec.Resources.Requests, l)...)
		}
	}
	if len(out) == 0 {
		add(ResultClear, "it claims no persistent storage")
	}
	return out
}

// bindable checks that a pending claim can be provisioned: its storage
// class exists and its size is plausible.
func bindable(what string, class *string, requests v1.ResourceList, l Lookup) []Finding {
	f := func(result, format string, args ...interface{}) []Finding {
		return []Finding{{Check: CheckVolumes, Result: result, Message: fmt.Sprintf(format, args...)}}
	}
	size, ok := requests[v1.ResourceStorage]
	if !ok || size.Sign() <= 0 {
		return f(ResultFail, "%s asks for no storage size", what)
	}
	if size.Cmp(resource.MustParse("64Ti")) > 0 {
		return f(ResultWarn, "%s asks for %s; is that the size you meant?", what, size.String())
	}
	name := ""
	if class != nil {
		name = *class
		if name == "" {
			return f(ResultInfo, "%s asks for no storage class, so it binds only to a matching PersistentVolume made by hand", what)
		}
	}
	sc, err := l.StorageClass(name)
	switch {
	case err != nil:
		return f(ResultInfo, "couldn't look up the storage class of %s: %v", what, err)
	case sc == nil && name == "":
		return f(ResultFail, "%s relies on a default storage class, and the cluster has none", what)
	case sc == nil:
		return f(ResultFail, "%s wants storage class %s, which doesn't exist", what, name)
	}
	return f(ResultClear, "%s can be provisioned by storage class %s (%s)", what, sc.Name, size.String())
}

func screenQuota(g *Gestation, size, placenta v1.ResourceList, l Lookup) []Finding {
	quotas, err := l.ResourceQuotas(g.Namespace)
	if err != nil {
		return []Finding{{Check: CheckQuota, Result: ResultInfo, Message: fmt.Sprintf("couldn't read quotas: %v", err)}}
	}
	if len(quotas) == 0 {
		return []Finding{{Check: CheckQuota, Result: ResultClear, Message: "no ResourceQuota limits the namespace"}}
	}
	pods := g.Spec.Replicas + 1 // newborn care adds a replica
	need := v1.ResourceList{v1.ResourcePods: *resource.NewQuantity(int64(pods), resource.DecimalSI)}
	for _, r := range []v1.ResourceName{v1.ResourceCPU, v1.ResourceMemory} {
		q := size[r].DeepCopy()
		total := resource.Quantity{Format: q.Format}
		for i := int32(0); i < pods; i++ {
			total.Add(q)
		}
		need[r] = total
		need["requests."+r] = total
	}
	var short []string
	for _, quota := range quotas {
		for name, want := range need {
			hard, limited := quota.Status.Hard[name]
			if !limited {
				hard, limited = quota.Spec.Hard[name]
			}
			if !limited {
				continue
			}
			avail := hard.DeepCopy()
			avail.Sub(quota.Status.Used[name])
			avail.Add(placenta[v1.ResourceName(strings.TrimPrefix(string(name), "requests."))])
			if avail.Cmp(want) < 0 {
				short = append(short, fmt.Sprintf("%s allows %s more %s, the launch needs %s", quota.Name, avail.String(), name, want.String()))
			}
		}
	}
	if len(short) > 0 {
		sort.Strings(short)
		return []Finding{{Check: CheckQuota, Result: ResultFail, Message: strings.Join(short, "; ") +
			fmt.Sprintf(" (%d replicas plus one for newborn care)", g.Spec.Replicas)}}
	}
	return []Finding{{Check: CheckQuota, Result: ResultClear,
		Message: fmt.Sprintf("the quota has room for %d replicas plus one for newborn care", g.Spec.Replicas)}}
}

func screenProbes(t *v1.PodTemplateSpec) []Finding {
	var noReady, noLive []string
	for _, c := range t.Spec.Containers {
		if c.ReadinessProbe == nil {
			noReady = append(noReady, c.Name)
		}
		if c.LivenessProbe == nil {
			noLive = append(noLive, c.Name)
		}
	}
	var out []Finding
	if len(noReady) > 0 {
		out = append(out, Finding{Check: CheckProbes, Result: ResultWarn, Message: "no readiness probe on " +
			strings.Join(noReady, ", ") + ": it would get traffic before it can answer, and its Apgar score can't tell"})
	}
	if len(noLive) > 0 {
		out = append(out, Finding{Check: CheckProbes, Result: ResultInfo, Message: "no liveness probe on " +
			strings.Join(noLive, ", ") + "; fine if it exits when it is stuck"})
	}
	if len(out) == 0 {
		out = append(out, Finding{Check: CheckProbes, Result: ResultClear, Message: "every container has readiness and liveness probes"})
	}
	return out
}

func screenRequests(t *v1.PodTemplateSpec) []Finding {
	var missing []string
	for _, c := range t.Spec.Containers {
		var r []string
		for _, name := range []v1.ResourceName{v1.ResourceCPU, v1.ResourceMemory} {
			if _, ok := c.Resources.Requests[name]; !ok {
				r = append(r, string(name))
			}
		}
		if len(r) > 0 {
			missing = append(missing, fmt.Sprintf("%s (%s)", c.Name, strings.Join(r, ", ")))
		}
	}
	if len(missing) > 0 {
		return []Finding{{Check: CheckRequests, Result: ResultWarn, Message: "requests not set on " + strings.Join(missing, ", ") +
			": the scheduler can't count what it will need"}}
	}
	return []Finding{{Check: CheckRequests, Result: ResultClear, Message: "every container states its CPU and memory requests"}}
}

func screenPDB(d *appsv1.Deployment, l Lookup) []Finding {
	pdbs, err := l.PDBs(d.Namespace)
	if err != nil {
		return []Finding{{Check: CheckPDB, Result: ResultInfo, Message: fmt.Sprintf("couldn't read PodDisruptionBudgets: %v", err)}}
	}
	if pdb := CoveringPDB(pdbs, d.Spec.Template.Labels, ""); pdb != "" {
		return []Finding{{Check: CheckPDB, Result: ResultClear, Message: "PodDisruptionBudget " + pdb + " covers it"}}
	}
	return []Finding{{Check: CheckPDB, Result: ResultInfo, Message: "no PodDisruptionBudget covers it. Newborn care adds one for its first 72 hours; " +
		"after that a node drain may take all its pods at once. Consider adding your own"}}
}

// CoveringPDB returns the name of a PodDisruptionBudget, other than skip,
// whose selector matches the pod labels.
func CoveringPDB(pdbs []*policyv1.PodDisruptionBudget, podLabels map[string]string, skip string) string {
	for _, pdb := range pdbs {
		if pdb.Name == skip || pdb.Spec.Selector == nil {
			continue
		}
		sel, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
		if err != nil || sel.Empty() {
			continue
		}
		if sel.Matches(labels.Set(podLabels)) {
			return pdb.Name
		}
	}
	return ""
}

// Summarize says a screening in one sentence.
func Summarize(reason string, findings []Finding) (warning bool, msg string) {
	var concerns []string
	for _, f := range findings {
		if f.Result == ResultFail || f.Result == ResultWarn {
			concerns = append(concerns, f.Message)
		}
	}
	what := "Prenatal screening"
	if strings.HasPrefix(reason, "trimester-") {
		what += ", trimester " + strings.TrimPrefix(reason, "trimester-")
	}
	if len(concerns) == 0 {
		return false, fmt.Sprintf("%s: all clear (%d checks).", what, len(findings))
	}
	return true, fmt.Sprintf("%s: %d to look at before the due date. %s.", what, len(concerns), concerns[0])
}
