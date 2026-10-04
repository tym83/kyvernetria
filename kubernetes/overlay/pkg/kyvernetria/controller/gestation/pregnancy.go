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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/utils/ptr"

	"k8s.io/kubernetes/pkg/kyvernetria"
	api "k8s.io/kubernetes/pkg/kyvernetria/gestation"
)

// placentaHashAnnotation records what the placeholder template was built
// from, so the controller rewrites it only when that changes (the server
// adds defaults that a plain comparison would trip over).
const placentaHashAnnotation = kyvernetria.Prefix + "placenta-hash"

// pregnancy reserves room in trimester steps and screens every trimester.
func (c *Controller) pregnancy(ctx context.Context, g *api.Gestation, d *appsv1.Deployment, now time.Time) error {
	due, err := g.Spec.DueTime()
	if err != nil {
		g.Status.Phase, g.Status.Message = api.PhaseExpecting, err.Error()
		return nil
	}
	size, err := api.ParseSize(g.Spec.Size)
	if err != nil {
		g.Status.Phase, g.Status.Message = api.PhaseExpecting, err.Error()
		return nil
	}
	tri := api.Trimester(g.CreationTimestamp.Time, due, now)
	reserved := api.Reserved(tri, g.Spec.Replicas)
	var tmpl *v1.PodTemplateSpec
	if d != nil {
		tmpl = &d.Spec.Template
	}
	placeErr := c.ensurePlacenta(ctx, g, reserved, size, tmpl)
	g.Status.Trimester, g.Status.Reserved = tri, reserved
	if tri == 0 {
		g.Status.Phase = api.PhaseDue
		g.Status.Message = fmt.Sprintf("Due since %s. Nothing happens on its own; deliver it when it is ready "+
			"(kyvctl deliver %s -n %s). Its room stays reserved until then.", g.Spec.Due, g.Name, g.Namespace)
	} else {
		g.Status.Phase = api.PhaseExpecting
		g.Status.Message = fmt.Sprintf("Trimester %d of 3, due %s. Room reserved for %d of the %d pods it will need at birth.",
			tri, g.Spec.Due, reserved, g.Spec.Replicas+1)
	}
	if tri > g.Status.ScreenedTrimester {
		c.screen(ctx, g, d, fmt.Sprintf("trimester-%d", tri), now)
		g.Status.ScreenedTrimester = tri
	}
	return placeErr
}

// ensurePlacenta creates or resizes the placeholder Deployment.
func (c *Controller) ensurePlacenta(ctx context.Context, g *api.Gestation, reserved int32, size v1.ResourceList, tmpl *v1.PodTemplateSpec) error {
	want := api.Placenta(g, g.UID, reserved, size, tmpl)
	raw, _ := json.Marshal(want.Spec.Template)
	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:])[:16]
	want.Annotations = map[string]string{placentaHashAnnotation: hash}

	deploys := c.client.AppsV1().Deployments(g.Namespace)
	existing, err := deploys.Get(ctx, want.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = deploys.Create(ctx, want, metav1.CreateOptions{FieldManager: fieldManager})
		return err
	}
	if err != nil {
		return err
	}
	if ref := metav1.GetControllerOf(existing); ref == nil || ref.UID != g.UID {
		return fmt.Errorf("deployment %s exists and is not this gestation's placenta; leaving it alone", want.Name)
	}
	if ptr.Deref(existing.Spec.Replicas, 1) == reserved && existing.Annotations[placentaHashAnnotation] == hash {
		return nil
	}
	updated := existing.DeepCopy()
	updated.Spec.Replicas = ptr.To(reserved)
	if updated.Annotations[placentaHashAnnotation] != hash {
		updated.Spec.Template = want.Spec.Template
		if updated.Annotations == nil {
			updated.Annotations = map[string]string{}
		}
		updated.Annotations[placentaHashAnnotation] = hash
	}
	_, err = deploys.Update(ctx, updated, metav1.UpdateOptions{FieldManager: fieldManager})
	return err
}

// releasePlacenta deletes the placeholders. It returns how many
// placeholder pods still hold room.
func (c *Controller) releasePlacenta(ctx context.Context, g *api.Gestation) (int, error) {
	deploys := c.client.AppsV1().Deployments(g.Namespace)
	existing, err := deploys.Get(ctx, api.PlacentaName(g), metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return 0, err
	default:
		if ref := metav1.GetControllerOf(existing); ref == nil || ref.UID != g.UID {
			break // not ours
		}
		err = deploys.Delete(ctx, existing.Name, metav1.DeleteOptions{
			Preconditions:     &metav1.Preconditions{UID: &existing.UID},
			PropagationPolicy: ptr.To(metav1.DeletePropagationBackground),
		})
		if err != nil && !apierrors.IsNotFound(err) {
			return 0, err
		}
	}
	return len(c.placentaPods(g)), nil
}

// placentaPods are the gestation's live placeholder pods.
func (c *Controller) placentaPods(g *api.Gestation) []*v1.Pod {
	pods, err := c.pods.Pods(g.Namespace).List(labels.SelectorFromSet(labels.Set{kyvernetria.PlacentaLabel: g.Name}))
	if err != nil {
		return nil
	}
	var out []*v1.Pod
	for _, p := range pods {
		if p.Status.Phase != v1.PodSucceeded && p.Status.Phase != v1.PodFailed {
			out = append(out, p)
		}
	}
	return out
}

// placentaUsage is what the placeholders take from the namespace's quota.
func (c *Controller) placentaUsage(g *api.Gestation) v1.ResourceList {
	pods := c.placentaPods(g)
	out := v1.ResourceList{v1.ResourcePods: *resource.NewQuantity(int64(len(pods)), resource.DecimalSI)}
	for _, p := range pods {
		for _, ctr := range p.Spec.Containers {
			for name, q := range ctr.Resources.Requests {
				sum := out[name]
				sum.Add(q)
				out[name] = sum
			}
		}
	}
	return out
}

// screen runs prenatal screening and reports it in status and events.
func (c *Controller) screen(ctx context.Context, g *api.Gestation, d *appsv1.Deployment, reason string, now time.Time) {
	findings := api.Screen(ctx, api.Input{Gestation: g, Deployment: d, Placenta: c.placentaUsage(g)}, c.lookup)
	g.Status.Screening = &api.Screening{Time: metav1.Time{Time: now}, Reason: reason, Findings: findings}
	warning, msg := api.Summarize(reason, findings)
	eventType := v1.EventTypeNormal
	if warning {
		eventType = v1.EventTypeWarning
	}
	c.event(g, eventType, ReasonScreened, "%s", msg)
}
