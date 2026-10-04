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
	"encoding/json"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"

	"k8s.io/kubernetes/pkg/kyvernetria"
	api "k8s.io/kubernetes/pkg/kyvernetria/gestation"
)

func inCare(g *api.Gestation) bool {
	return g.Status.Care != nil && (g.Status.Phase == api.PhaseNewbornCare || g.Status.Phase == api.PhaseNeedsCaregivers)
}

// afterBirth looks after a born service: Apgar scores, newborn care,
// growth, and its record.
func (c *Controller) afterBirth(ctx context.Context, g *api.Gestation, d *appsv1.Deployment, now time.Time, mem *memory) error {
	b := g.Status.Birth
	if d == nil || string(d.UID) != b.UID {
		return c.depart(ctx, g, now, mem)
	}
	var errs []error
	if err := c.resolveRevisions(g, d); err != nil {
		errs = append(errs, err)
	}
	if inCare(g) {
		if err := c.apgar(ctx, g, d, now); err != nil {
			errs = append(errs, err)
		}
	}
	if inCare(g) {
		if err := c.care(ctx, g, d, now); err != nil {
			errs = append(errs, err)
		}
	}
	c.measure(ctx, g, inCare(g), now)
	if mem != nil {
		c.remember(ctx, g, d, mem)
	}
	return utilerrors.NewAggregate(errs)
}

// care tapers the protection, and discharges the newborn once the care
// period is over and two caregivers are named.
func (c *Controller) care(ctx context.Context, g *api.Gestation, d *appsv1.Deployment, now time.Time) error {
	since := now.Sub(g.Status.Birth.Time.Time)
	cg, ok, missing := api.CaregiversOf(d.Annotations)
	g.Status.Caregivers = nil
	if cg.Primary != "" || cg.Secondary != "" {
		g.Status.Caregivers = &cg
	}
	care := g.Status.Care
	if tier, class := api.CareTier(since); tier > 0 {
		care.Tier, care.PriorityClass = tier, class
		g.Status.Phase = api.PhaseNewbornCare
		g.Status.Message = fmt.Sprintf("Newborn care, hour %d of %d.", int(since/time.Hour)+1, int(kyvernetria.NewbornCareDuration/time.Hour))
		if ok {
			g.Status.Message += fmt.Sprintf(" Caregivers: %s and %s.", cg.Primary, cg.Secondary)
		} else {
			g.Status.Message += " To go home it needs two caregivers; " + missing + "."
		}
		return nil
	}
	if ok {
		return c.discharge(ctx, g, d, now, cg)
	}
	care.Tier, care.PriorityClass = api.LastTierClass()
	g.Status.Phase = api.PhaseNeedsCaregivers
	g.Status.Message = fmt.Sprintf("%s is ready to leave newborn care, but %s. Care is shared: name two people on call, and it goes home.", d.Name, missing)
	if care.LastNag == nil || now.Sub(care.LastNag.Time) >= nagInterval {
		c.deployEvent(d, v1.EventTypeWarning, ReasonNeedsCaregive,
			"%s is ready to go home from newborn care, but %s. Care is shared between two people: "+
				"kubectl annotate deployment %s %s=<name> %s=<name>. Until then it keeps its protection.",
			d.Name, missing, d.Name, kyvernetria.PrimaryCaregiverAnnotation, kyvernetria.SecondaryCaregiverAnnotation)
		care.LastNag = &metav1.Time{Time: now}
	}
	return nil
}

// discharge ends newborn care.
func (c *Controller) discharge(ctx context.Context, g *api.Gestation, d *appsv1.Deployment, now time.Time, cg api.Caregivers) error {
	updated := d.DeepCopy()
	endCare(updated, g.Status.Birth)
	if _, err := c.client.AppsV1().Deployments(d.Namespace).Update(ctx, updated, metav1.UpdateOptions{FieldManager: fieldManager}); err != nil {
		return err
	}
	if err := c.deletePDB(ctx, g); err != nil {
		return err
	}
	care := g.Status.Care
	care.Tier, care.PriorityClass = 0, ""
	care.Discharged = &metav1.Time{Time: now}
	g.Status.Phase = api.PhaseGrown
	g.Status.Message = fmt.Sprintf("Went home from newborn care on %s with two caregivers, %s and %s.",
		now.UTC().Format(time.DateOnly), cg.Primary, cg.Secondary)
	c.deployEvent(d, v1.EventTypeNormal, ReasonDischarged, "%s goes home from newborn care. Two caregivers: %s and %s.",
		d.Name, cg.Primary, cg.Secondary)
	return nil
}

// depart closes the life of a service whose Deployment is gone.
func (c *Controller) depart(ctx context.Context, g *api.Gestation, now time.Time, mem *memory) error {
	if g.Status.Phase == api.PhaseDeparted {
		return nil
	}
	err := c.deletePDB(ctx, g)
	g.Status.Phase = api.PhaseDeparted
	g.Status.Care = nil
	g.Status.Message = fmt.Sprintf("%s left on %s. The cluster keeps a little of it: kyvctl remember.",
		g.Spec.Deployment, now.UTC().Format(time.DateOnly))
	if mem != nil {
		mem.depart(g.Namespace, g.Spec.Deployment, g.Status.Birth.UID, now)
	}
	c.event(g, v1.EventTypeNormal, ReasonDeparted, "%s", g.Status.Message)
	return err
}

// setNewborn sets or, with class "", removes one workload's entry in its
// namespace's newborn-care annotation.
func (c *Controller) setNewborn(ctx context.Context, namespace, selector, class string) error {
	ns, err := c.client.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if err != nil {
		return err
	}
	entries, _ := api.DecodeNewborns(ns.Annotations[kyvernetria.NewbornCareAnnotation])
	var out []api.NewbornEntry
	for _, e := range entries {
		if e.Selector != selector {
			out = append(out, e)
		}
	}
	if class != "" {
		out = append(out, api.NewbornEntry{Selector: selector, PriorityClass: class})
	}
	return c.patchNewborns(ctx, ns, api.EncodeNewborns(out))
}

// syncNewborns makes every namespace's newborn-care annotation list
// exactly the services in care in it.
func (c *Controller) syncNewborns(ctx context.Context, all []*api.Gestation) error {
	desired := map[string][]api.NewbornEntry{}
	for _, g := range all {
		if inCare(g) && g.Status.Birth != nil && g.Status.Care.PriorityClass != "" {
			desired[g.Namespace] = append(desired[g.Namespace],
				api.NewbornEntry{Selector: g.Status.Birth.Selector, PriorityClass: g.Status.Care.PriorityClass})
		}
	}
	namespaces, err := c.namespaces.List(labels.Everything())
	if err != nil {
		return err
	}
	var errs []error
	for _, ns := range namespaces {
		want := api.EncodeNewborns(desired[ns.Name])
		if ns.Annotations[kyvernetria.NewbornCareAnnotation] == want {
			continue
		}
		if err := c.patchNewborns(ctx, ns, want); err != nil {
			errs = append(errs, err)
		}
	}
	return utilerrors.NewAggregate(errs)
}

func (c *Controller) patchNewborns(ctx context.Context, ns *v1.Namespace, value string) error {
	if ns.Annotations[kyvernetria.NewbornCareAnnotation] == value {
		return nil
	}
	var v interface{} = value
	if value == "" {
		v = nil // a JSON merge patch null removes the key
	}
	patch, err := json.Marshal(map[string]interface{}{
		"metadata": map[string]interface{}{"annotations": map[string]interface{}{kyvernetria.NewbornCareAnnotation: v}},
	})
	if err != nil {
		return err
	}
	_, err = c.client.CoreV1().Namespaces().Patch(ctx, ns.Name, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: fieldManager})
	return err
}

// measure adds a growth measurement when one is due.
func (c *Controller) measure(ctx context.Context, g *api.Gestation, newborn bool, now time.Time) {
	if c.metrics == nil || !api.SampleDue(g.Status.Growth, newborn, now) {
		return
	}
	list, err := c.metrics.MetricsV1beta1().PodMetricses(g.Namespace).List(ctx, metav1.ListOptions{LabelSelector: g.Status.Birth.Selector})
	if err != nil {
		g.Status.GrowthNote = "No measurements: the cluster serves no metrics API (metrics.k8s.io; install metrics-server to chart growth)."
		return
	}
	var cpu, mem int64
	n := int64(len(list.Items))
	if n == 0 {
		return
	}
	for _, pm := range list.Items {
		for _, ctr := range pm.Containers {
			cpu += ctr.Usage.Cpu().MilliValue()
			mem += ctr.Usage.Memory().Value()
		}
	}
	g.Status.Growth = api.AppendSample(g.Status.Growth, api.Sample{
		Time: metav1.Time{Time: now}, CPUMilli: cpu / n, MemoryBytes: mem / n, Pods: int32(n),
	})
	g.Status.GrowthNote = ""
}
