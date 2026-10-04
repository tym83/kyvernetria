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
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	"k8s.io/kubernetes/pkg/kyvernetria"
	api "k8s.io/kubernetes/pkg/kyvernetria/gestation"
)

// revisionAnnotation is where the deployment controller numbers revisions.
const revisionAnnotation = "deployment.kubernetes.io/revision"

// bornAnnotation records the birth on the Deployment.
const bornAnnotation = kyvernetria.Prefix + "born"

// deliver releases the reserved room and lets the service be born.
func (c *Controller) deliver(ctx context.Context, g *api.Gestation, d *appsv1.Deployment, now time.Time, mem *memory) error {
	held, err := c.releasePlacenta(ctx, g)
	if err != nil {
		return err
	}
	g.Status.Reserved, g.Status.Trimester = int32(held), 0
	started := now
	if g.Status.DeliveryStarted != nil {
		started = g.Status.DeliveryStarted.Time
	}
	if held > 0 && now.Sub(started) < placentaGrace {
		g.Status.Message = fmt.Sprintf("Releasing the room reserved for %s: %d places still held.", g.Spec.Deployment, held)
		return nil
	}
	if d == nil {
		g.Status.Message = fmt.Sprintf("Ready to be born: waiting for Deployment %s to exist.", g.Spec.Deployment)
		return nil
	}

	sel, err := metav1.LabelSelectorAsSelector(d.Spec.Selector)
	if err != nil || sel.Empty() {
		g.Status.Message = fmt.Sprintf("Deployment %s has no usable selector, so its pods can't be told apart: %v", d.Name, err)
		return nil
	}
	birth := &api.Birth{Time: metav1.Time{Time: now}, UID: string(d.UID), Selector: sel.String()}
	care := &api.Care{Until: metav1.Time{Time: now.Add(kyvernetria.NewbornCareDuration)}}
	care.Tier, care.PriorityClass = api.CareTier(0)

	// The priority bump has to be in place before the first pods exist.
	if err := c.setNewborn(ctx, d.Namespace, birth.Selector, care.PriorityClass); err != nil {
		return err
	}

	want := ptr.Deref(d.Spec.Replicas, 1)
	if want == 0 {
		want = g.Spec.Replicas
	}
	autoscaled, err := c.autoscaled(ctx, d)
	if err != nil {
		return err
	}
	if autoscaled {
		care.Notes = append(care.Notes, "an autoscaler manages its replicas, so newborn care adds none")
	} else {
		want++
		birth.AddedReplica = true
	}

	pdbs, err := c.client.PolicyV1().PodDisruptionBudgets(d.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	if covering := api.CoveringPDB(ptrs(pdbs.Items), d.Spec.Template.Labels, ""); covering != "" {
		care.Notes = append(care.Notes, "PodDisruptionBudget "+covering+" already covers it")
	} else {
		name, err := c.createPDB(ctx, g, d, want)
		if err != nil {
			return err
		}
		birth.PDB = name
		if want <= 1 {
			care.Notes = append(care.Notes, "with one replica, a node drain waits until newborn care ends")
		}
	}

	updated := d.DeepCopy()
	updated.Spec.Replicas = ptr.To(want)
	updated.Spec.Paused = false
	if updated.Labels == nil {
		updated.Labels = map[string]string{}
	}
	updated.Labels[kyvernetria.CareLabel] = kyvernetria.CareNewborn
	if updated.Annotations == nil {
		updated.Annotations = map[string]string{}
	}
	updated.Annotations[bornAnnotation] = api.Stamp(now)
	if _, err := c.client.AppsV1().Deployments(d.Namespace).Update(ctx, updated, metav1.UpdateOptions{FieldManager: fieldManager}); err != nil {
		return err
	}

	g.Status.Birth, g.Status.Care, g.Status.Apgar = birth, care, nil
	g.Status.Phase = api.PhaseNewbornCare
	g.Status.Message = fmt.Sprintf("%s is being born. The first Apgar score comes at one minute.", d.Name)
	c.deployEvent(d, v1.EventTypeNormal, ReasonDelivered, "Delivered: the room reserved for it is released. First Apgar score at one minute.")
	if mem != nil {
		mem.put(api.Record{Namespace: d.Namespace, Name: d.Name, UID: string(d.UID), Born: api.Stamp(now),
			ConfigDigest: api.ConfigDigest(&d.Spec.Template)})
	}
	return nil
}

func ptrs[T any](items []T) []*T {
	out := make([]*T, len(items))
	for i := range items {
		out[i] = &items[i]
	}
	return out
}

// autoscaled reports whether a HorizontalPodAutoscaler targets d.
func (c *Controller) autoscaled(ctx context.Context, d *appsv1.Deployment) (bool, error) {
	hpas, err := c.client.AutoscalingV2().HorizontalPodAutoscalers(d.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, err
	}
	for _, h := range hpas.Items {
		ref := h.Spec.ScaleTargetRef
		if ref.Kind == "Deployment" && ref.Name == d.Name {
			return true, nil
		}
	}
	return false, nil
}

// createPDB keeps all but one replica up during a drain: with the extra
// replica, that is every replica the service launched with.
func (c *Controller) createPDB(ctx context.Context, g *api.Gestation, d *appsv1.Deployment, replicas int32) (string, error) {
	minAvailable := replicas - 1
	if minAvailable < 1 {
		minAvailable = 1
	}
	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:            d.Name + "-newborn",
			Namespace:       d.Namespace,
			Labels:          map[string]string{api.ManagedByLabel: api.ManagedBy},
			OwnerReferences: []metav1.OwnerReference{gestationOwner(g)},
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: ptr.To(intstr.FromInt32(minAvailable)),
			Selector:     d.Spec.Selector.DeepCopy(),
		},
	}
	_, err := c.client.PolicyV1().PodDisruptionBudgets(d.Namespace).Create(ctx, pdb, metav1.CreateOptions{FieldManager: fieldManager})
	if apierrors.IsAlreadyExists(err) {
		err = nil
	}
	return pdb.Name, err
}

// deletePDB removes the budget newborn care created.
func (c *Controller) deletePDB(ctx context.Context, g *api.Gestation) error {
	if g.Status.Birth == nil || g.Status.Birth.PDB == "" {
		return nil
	}
	pdbs := c.client.PolicyV1().PodDisruptionBudgets(g.Namespace)
	pdb, err := pdbs.Get(ctx, g.Status.Birth.PDB, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if pdb.Labels[api.ManagedByLabel] != api.ManagedBy {
		return nil // someone else's by now
	}
	err = pdbs.Delete(ctx, pdb.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &pdb.UID}})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// resolveRevisions finds the birth revision, once the deployment
// controller has made it, and the revision before it.
func (c *Controller) resolveRevisions(g *api.Gestation, d *appsv1.Deployment) error {
	b := g.Status.Birth
	if b.Revision != "" || d.Status.ObservedGeneration < d.Generation {
		return nil
	}
	rss, err := c.ownedReplicaSets(d)
	if err != nil {
		return err
	}
	newest := newReplicaSet(d, rss)
	if newest == nil {
		return nil
	}
	b.Revision = newest.Annotations[revisionAnnotation]
	born := revisionNumber(newest)
	var prev int64
	for _, rs := range rss {
		if n := revisionNumber(rs); n < born && n > prev {
			prev = n
		}
	}
	if prev > 0 {
		b.PreviousRevision = strconv.FormatInt(prev, 10)
	}
	return nil
}

// newReplicaSet is the ReplicaSet whose template is the Deployment's.
func newReplicaSet(d *appsv1.Deployment, rss []*appsv1.ReplicaSet) *appsv1.ReplicaSet {
	for _, rs := range rss {
		if sameTemplate(&rs.Spec.Template, &d.Spec.Template) {
			return rs
		}
	}
	return nil
}

// sameTemplate compares templates without the pod-template-hash label.
func sameTemplate(a, b *v1.PodTemplateSpec) bool {
	a, b = a.DeepCopy(), b.DeepCopy()
	delete(a.Labels, appsv1.DefaultDeploymentUniqueLabelKey)
	delete(b.Labels, appsv1.DefaultDeploymentUniqueLabelKey)
	return apiequality.Semantic.DeepEqual(a, b)
}

func revisionNumber(rs *appsv1.ReplicaSet) int64 {
	n, _ := strconv.ParseInt(rs.Annotations[revisionAnnotation], 10, 64)
	return n
}

func (c *Controller) replicaSetAt(d *appsv1.Deployment, revision string) (*appsv1.ReplicaSet, error) {
	rss, err := c.ownedReplicaSets(d)
	if err != nil {
		return nil, err
	}
	for _, rs := range rss {
		if rs.Annotations[revisionAnnotation] == revision {
			return rs, nil
		}
	}
	return nil, nil
}

// apgar scores the launch when a score is due, and rolls back a launch
// whose five-minute score is low.
func (c *Controller) apgar(ctx context.Context, g *api.Gestation, d *appsv1.Deployment, now time.Time) error {
	b := g.Status.Birth
	next := api.NextMinute(g.Status.Apgar)
	if next == 0 || b.Revision == "" || now.Before(b.Time.Add(time.Duration(next)*time.Minute)) {
		return nil
	}
	signs, err := c.signs(ctx, d, b)
	if err != nil {
		return err
	}
	score := api.Score(next, signs)
	score.Time = metav1.Time{Time: now}
	roll, why := api.Rollback(score, g.Spec.RollbackEnabled(), b.PreviousRevision)
	if roll {
		if err := c.rollback(ctx, g, d); err != nil {
			return err // scored again on the next pass
		}
	}
	g.Status.Apgar = append(g.Status.Apgar, score)

	switch {
	case roll:
		g.Status.Phase = api.PhaseRolledBack
		g.Status.Message = fmt.Sprintf("Apgar %d at five minutes. %s went back to revision %s, where it was well. "+
			"When it is ready, deliver it again: kyvctl deliver %s -n %s --again.", score.Total, d.Name, b.PreviousRevision, g.Name, g.Namespace)
		c.deployEvent(d, v1.EventTypeWarning, ReasonRolledBack, "Apgar %d at five minutes (%s). Going back to revision %s, where it was well.",
			score.Total, score.Weakest(), b.PreviousRevision)
	case score.Total < api.Reassuring:
		msg := fmt.Sprintf("Apgar %d at %s (%s)", score.Total, api.MinuteWords(next), score.Weakest())
		if why != "" {
			msg += ". " + strings.ToUpper(why[:1]) + why[1:]
		}
		c.deployEvent(d, v1.EventTypeWarning, ReasonApgarLow, "%s.", msg)
		g.Status.Message = msg + "."
	case next == 1:
		c.deployEvent(d, v1.EventTypeNormal, ReasonApgar, "Apgar %d at one minute.", score.Total)
	default:
		c.deployEvent(d, v1.EventTypeNormal, ReasonApgar, "%s arrived. Apgar %d at %s. Welcome.", d.Name, score.Total, api.MinuteWords(next))
		g.Status.Message = fmt.Sprintf("%s arrived. Apgar %d at %s. Welcome.", d.Name, score.Total, api.MinuteWords(next))
	}
	return nil
}

// rollback returns the Deployment to its previous revision and ends
// newborn care: what was born is no longer running.
func (c *Controller) rollback(ctx context.Context, g *api.Gestation, d *appsv1.Deployment) error {
	b := g.Status.Birth
	rs, err := c.replicaSetAt(d, b.PreviousRevision)
	if err != nil {
		return err
	}
	if rs == nil {
		return fmt.Errorf("revision %s of %s is gone", b.PreviousRevision, d.Name)
	}
	updated := d.DeepCopy()
	updated.Spec.Template = *rs.Spec.Template.DeepCopy()
	delete(updated.Spec.Template.Labels, appsv1.DefaultDeploymentUniqueLabelKey)
	endCare(updated, b)
	if _, err := c.client.AppsV1().Deployments(d.Namespace).Update(ctx, updated, metav1.UpdateOptions{FieldManager: fieldManager}); err != nil {
		return err
	}
	b.RolledBackTo = b.PreviousRevision
	g.Status.Care = nil
	return c.deletePDB(ctx, g)
}

// endCare takes newborn care off a Deployment: the extra replica and the
// label for alerting.
func endCare(d *appsv1.Deployment, b *api.Birth) {
	if b.AddedReplica && ptr.Deref(d.Spec.Replicas, 1) > 1 {
		d.Spec.Replicas = ptr.To(ptr.Deref(d.Spec.Replicas, 1) - 1)
	}
	delete(d.Labels, kyvernetria.CareLabel)
}

// signs observes the newborn: the pods of its birth ReplicaSet.
func (c *Controller) signs(ctx context.Context, d *appsv1.Deployment, b *api.Birth) (api.Signs, error) {
	s := api.Signs{Desired: ptr.Deref(d.Spec.Replicas, 1)}
	rs, err := c.replicaSetAt(d, b.Revision)
	if err != nil || rs == nil {
		return s, err
	}
	all, err := c.pods.Pods(d.Namespace).List(labels.Everything())
	if err != nil {
		return s, err
	}
	var pods []*v1.Pod
	names := map[string]bool{}
	for _, p := range all {
		if ref := metav1.GetControllerOf(p); ref != nil && ref.UID == rs.UID {
			pods = append(pods, p)
			names[p.Name] = true
		}
	}
	for _, p := range pods {
		if p.Status.Phase == v1.PodFailed && p.Status.Reason == "Evicted" {
			s.Evictions++
			continue
		}
		if podReady(p) {
			s.Ready++
		}
		running := p.Status.Phase == v1.PodRunning && len(p.Status.ContainerStatuses) > 0
		for _, cs := range p.Status.ContainerStatuses {
			s.Restarts += cs.RestartCount
			if cs.State.Running == nil {
				running = false
			}
			if w := cs.State.Waiting; w != nil && w.Reason == "CrashLoopBackOff" {
				s.CrashLooping = true
			}
			if t := cs.LastTerminationState.Terminated; t != nil && t.Reason == "OOMKilled" {
				s.OOMKills++
			} else if t := cs.State.Terminated; t != nil && t.Reason == "OOMKilled" {
				s.OOMKills++
			}
		}
		if running {
			s.Running++
		}
	}

	// Events: warnings about the newborn's pods (gone ones by name), its
	// ReplicaSet and its Deployment, since birth. Not this controller's
	// own: a low score at one minute would lower the score at five.
	about := func(e *v1.Event) bool {
		o := e.InvolvedObject
		switch o.Kind {
		case "Pod":
			return names[o.Name] || strings.HasPrefix(o.Name, rs.Name+"-")
		case "ReplicaSet":
			return o.Name == rs.Name
		case "Deployment":
			return o.Name == d.Name
		}
		return false
	}
	opts := metav1.ListOptions{FieldSelector: fields.OneTermEqualSelector("type", v1.EventTypeWarning).String(), Limit: 500}
	for {
		list, err := c.client.CoreV1().Events(d.Namespace).List(ctx, opts)
		if err != nil {
			return s, err
		}
		for i := range list.Items {
			e := &list.Items[i]
			if e.Type != v1.EventTypeWarning || !about(e) || eventTime(e).Before(b.Time.Time) ||
				e.Source.Component == EventSource || e.ReportingController == EventSource {
				continue
			}
			n := e.Count
			if n < 1 {
				n = 1
			}
			switch {
			case e.Reason == "Unhealthy" && strings.Contains(e.Message, "Liveness probe"):
				s.LivenessFailures += n
			case e.Reason == "Unhealthy" && (strings.Contains(e.Message, "Readiness probe") || strings.Contains(e.Message, "Startup probe")):
				// Not ready yet is Appearance's business.
			case e.Reason == "BackOff":
				// Restarts are Pulse's business.
			default:
				s.Warnings += n
			}
		}
		if opts.Continue = list.Continue; opts.Continue == "" {
			break
		}
	}

	// Activity: ready endpoints of the newborn's pods behind the Services
	// that select them.
	svcs, err := c.services.Services(d.Namespace).List(labels.Everything())
	if err != nil {
		return s, err
	}
	serving := map[string]bool{}
	for _, svc := range svcs {
		if len(svc.Spec.Selector) == 0 || !labels.SelectorFromSet(svc.Spec.Selector).Matches(labels.Set(rs.Spec.Template.Labels)) {
			continue
		}
		s.Services++
		slices, err := c.slices.EndpointSlices(d.Namespace).List(labels.SelectorFromSet(labels.Set{"kubernetes.io/service-name": svc.Name}))
		if err != nil {
			return s, err
		}
		for _, sl := range slices {
			for _, ep := range sl.Endpoints {
				if ep.TargetRef != nil && ep.TargetRef.Kind == "Pod" && names[ep.TargetRef.Name] &&
					(ep.Conditions.Ready == nil || *ep.Conditions.Ready) {
					serving[ep.TargetRef.Name] = true
				}
			}
		}
	}
	s.ReadyEndpoint = int32(len(serving))
	return s, nil
}

func podReady(p *v1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == v1.PodReady {
			return c.Status == v1.ConditionTrue
		}
	}
	return false
}

func eventTime(e *v1.Event) time.Time {
	switch {
	case !e.LastTimestamp.IsZero():
		return e.LastTimestamp.Time
	case !e.EventTime.IsZero():
		return e.EventTime.Time
	}
	return e.FirstTimestamp.Time
}
