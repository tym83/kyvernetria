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

// Package gestation runs a new service's launch, from the reservation of
// its room to its first days in production.
//
// Before the due date it holds room with placeholder ("placenta") pods that
// grow in trimester steps, and screens the service every trimester and on
// demand. At delivery it releases the room, scores the launch at one and
// five minutes (the Apgar score) and rolls back a launch that scores below
// 7 at five minutes. For 72 hours after it protects the newborn, with a
// taper, and lets it go home only to two named caregivers. While it lives
// it charts its growth against its own trajectory, and when it leaves, the
// cluster keeps a small record of it.
//
// Models, as principles and never as numbers: maternal blood-volume
// expansion, the Apgar score, the waning of maternal antibodies, WHO
// growth charts, shared caregiving and fetal microchimerism. See
// docs/RESEARCH.md, claims 38 to 44.
package gestation

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	appslisters "k8s.io/client-go/listers/apps/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	discoverylisters "k8s.io/client-go/listers/discovery/v1"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned"

	api "k8s.io/kubernetes/pkg/kyvernetria/gestation"
)

// Event reasons.
const (
	ReasonScreened      = "PrenatalScreening"
	ReasonDelivered     = "Delivered"
	ReasonApgar         = "Apgar"
	ReasonApgarLow      = "ApgarLow"
	ReasonRolledBack    = "RolledBack"
	ReasonDischarged    = "Discharged"
	ReasonNeedsCaregive = "NeedsCaregivers"
	ReasonDeparted      = "Departed"
)

// Timing. Engineering choices.
const (
	// placentaGrace is how long delivery waits for the placeholders to go
	// before the service's own pods are scaled up anyway.
	placentaGrace = 2 * time.Minute
	// nagInterval is how often a service waiting for caregivers is
	// mentioned again.
	nagInterval = 12 * time.Hour
)

// Clients are what the controller talks to the API with.
type Clients struct {
	Kube       kubernetes.Interface
	CRDs       apiextensionsclient.Interface
	Dynamic    dynamic.Interface
	Metadata   metadata.Interface
	Metrics    metricsclient.Interface // nil: no growth measurements
	HTTPClient HTTPDoer                // for image checks; nil: none
}

// Controller runs gestations.
type Controller struct {
	client      kubernetes.Interface
	crds        apiextensionsclient.Interface
	dynamic     dynamic.Interface
	metrics     metricsclient.Interface
	lookup      api.Lookup
	deployments appslisters.DeploymentLister
	replicaSets appslisters.ReplicaSetLister
	pods        corelisters.PodLister
	services    corelisters.ServiceLister
	slices      discoverylisters.EndpointSliceLister
	namespaces  corelisters.NamespaceLister
	synced      []cache.InformerSynced
	recorder    record.EventRecorder
	now         func() time.Time
	period      time.Duration
}

// New creates the controller.
func New(c Clients, f informers.SharedInformerFactory, recorder record.EventRecorder) *Controller {
	deploys := f.Apps().V1().Deployments()
	rs := f.Apps().V1().ReplicaSets()
	pods := f.Core().V1().Pods()
	svcs := f.Core().V1().Services()
	slices := f.Discovery().V1().EndpointSlices()
	nodes := f.Core().V1().Nodes()
	nss := f.Core().V1().Namespaces()
	return &Controller{
		client:      c.Kube,
		crds:        c.CRDs,
		dynamic:     c.Dynamic,
		metrics:     c.Metrics,
		lookup:      &clusterLookup{client: c.Kube, metadata: c.Metadata, images: &imageChecker{nodes: nodes.Lister(), http: c.HTTPClient}},
		deployments: deploys.Lister(),
		replicaSets: rs.Lister(),
		pods:        pods.Lister(),
		services:    svcs.Lister(),
		slices:      slices.Lister(),
		namespaces:  nss.Lister(),
		synced: []cache.InformerSynced{
			deploys.Informer().HasSynced, rs.Informer().HasSynced, pods.Informer().HasSynced,
			svcs.Informer().HasSynced, slices.Informer().HasSynced, nodes.Informer().HasSynced,
			nss.Informer().HasSynced,
		},
		recorder: recorder,
		now:      time.Now,
		period:   15 * time.Second,
	}
}

// Run reconciles every period until ctx is done.
func (c *Controller) Run(ctx context.Context) {
	defer utilruntime.HandleCrash()
	logger := klog.FromContext(ctx)
	logger.Info("Starting kyvernetria gestation controller")
	defer logger.Info("Shutting down kyvernetria gestation controller")
	if !cache.WaitForNamedCacheSync("kyvernetria-gestation", ctx.Done(), c.synced...) {
		return
	}
	wait.UntilWithContext(ctx, func(ctx context.Context) {
		if err := c.Sync(ctx); err != nil {
			utilruntime.HandleErrorWithContext(ctx, err, "Gestation sync failed")
		}
	}, c.period)
}

// Sync runs one pass over every gestation.
func (c *Controller) Sync(ctx context.Context) error {
	if err := c.ensureCRD(ctx); err != nil {
		return err
	}
	var errs []error
	if err := c.ensurePriorityClasses(ctx); err != nil {
		errs = append(errs, err)
	}
	list, err := c.dynamic.Resource(api.GVR).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	mem, err := c.loadMemory(ctx)
	if err != nil {
		errs = append(errs, err)
	}
	var all []*api.Gestation
	for i := range list.Items {
		g, err := api.FromUnstructured(&list.Items[i])
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := c.reconcile(ctx, g, mem); err != nil {
			errs = append(errs, fmt.Errorf("%s/%s: %w", g.Namespace, g.Name, err))
		}
		all = append(all, g)
	}
	if err := c.syncNewborns(ctx, all); err != nil {
		errs = append(errs, err)
	}
	if mem != nil {
		c.closeDeparted(mem)
		if err := c.saveMemory(ctx, mem); err != nil {
			errs = append(errs, err)
		}
	}
	return utilerrors.NewAggregate(errs)
}

// reconcile moves one gestation along and records its status.
func (c *Controller) reconcile(ctx context.Context, g *api.Gestation, mem *memory) error {
	before, _ := json.Marshal(g.Status)
	now := c.now()
	d, err := c.client.AppsV1().Deployments(g.Namespace).Get(ctx, g.Spec.Deployment, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		d = nil
	case err != nil:
		return err
	}

	if g.Spec.Delivery > g.Status.Delivery && (g.Status.Birth == nil || g.Status.Phase == api.PhaseRolledBack) {
		g.Status.Delivery = g.Spec.Delivery
		g.Status.DeliveryStarted = &metav1.Time{Time: now}
		g.Status.Birth, g.Status.Apgar, g.Status.Care = nil, nil, nil
		g.Status.Phase = api.PhaseDelivering
	}

	var stepErr error
	switch {
	case g.Status.Phase == api.PhaseDelivering:
		stepErr = c.deliver(ctx, g, d, now, mem)
	case g.Status.Birth == nil:
		stepErr = c.pregnancy(ctx, g, d, now)
	default:
		stepErr = c.afterBirth(ctx, g, d, now, mem)
	}
	if r := g.Spec.ScreenRequested; r != nil && (g.Status.ScreenedRequest == nil || !g.Status.ScreenedRequest.Equal(r)) {
		c.screen(ctx, g, d, "requested", now)
		g.Status.ScreenedRequest = r.DeepCopy()
	}

	after, _ := json.Marshal(g.Status)
	if string(before) != string(after) {
		if err := c.updateStatus(ctx, g); err != nil {
			return utilerrors.NewAggregate([]error{stepErr, err})
		}
	}
	return stepErr
}

func (c *Controller) updateStatus(ctx context.Context, g *api.Gestation) error {
	u, err := api.ToUnstructured(g)
	if err != nil {
		return err
	}
	_, err = c.dynamic.Resource(api.GVR).Namespace(g.Namespace).UpdateStatus(ctx, u, metav1.UpdateOptions{FieldManager: fieldManager})
	return err
}

const fieldManager = "kyvernetria-gestation"

// EventSource is the component the controller's events come from. Apgar
// leaves them out: its own warnings are not the newborn's grimace.
const EventSource = "kyvernetria-gestation"

// event records an event on the gestation.
func (c *Controller) event(g *api.Gestation, eventType, reason, format string, args ...interface{}) {
	ref := &unstructured.Unstructured{}
	ref.SetAPIVersion(api.Group + "/" + api.Version)
	ref.SetKind(api.Kind)
	ref.SetNamespace(g.Namespace)
	ref.SetName(g.Name)
	ref.SetUID(g.UID)
	c.recorder.Eventf(ref, eventType, reason, format, args...)
}

// deployEvent records an event on the service's Deployment, where
// `kyvctl remember deploy/x` and `kubectl describe` find it.
func (c *Controller) deployEvent(d *appsv1.Deployment, eventType, reason, format string, args ...interface{}) {
	c.recorder.Eventf(d, eventType, reason, format, args...)
}

func (c *Controller) ensureCRD(ctx context.Context) error {
	crd := api.CRD()
	crds := c.crds.ApiextensionsV1().CustomResourceDefinitions()
	existing, err := crds.Get(ctx, crd.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if _, err := crds.Create(ctx, crd, metav1.CreateOptions{}); err != nil {
			return err
		}
		return fmt.Errorf("gestation API created, waiting for it to be served")
	}
	if err != nil {
		return err
	}
	want := crd.Spec.DeepCopy()
	want.Conversion = existing.Spec.Conversion
	want.PreserveUnknownFields = existing.Spec.PreserveUnknownFields
	if !apiequality.Semantic.DeepEqual(&existing.Spec, want) {
		updated := existing.DeepCopy()
		updated.Spec = *want
		if _, err := crds.Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("updating the gestation API: %w", err)
		}
		return fmt.Errorf("gestation API updated, waiting for it to be served")
	}
	for _, cond := range existing.Status.Conditions {
		if cond.Type == apiextensionsv1.Established && cond.Status == apiextensionsv1.ConditionTrue {
			return nil
		}
	}
	return fmt.Errorf("gestation API is not established yet")
}

// ensurePriorityClasses creates the classes that are missing. A class's
// value cannot change once created, so existing ones are left alone.
func (c *Controller) ensurePriorityClasses(ctx context.Context) error {
	var errs []error
	for _, pc := range api.PriorityClasses() {
		_, err := c.client.SchedulingV1().PriorityClasses().Get(ctx, pc.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			_, err = c.client.SchedulingV1().PriorityClasses().Create(ctx, pc, metav1.CreateOptions{FieldManager: fieldManager})
			if apierrors.IsAlreadyExists(err) {
				err = nil
			}
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	return utilerrors.NewAggregate(errs)
}

// ownedReplicaSets lists the Deployment's ReplicaSets.
func (c *Controller) ownedReplicaSets(d *appsv1.Deployment) ([]*appsv1.ReplicaSet, error) {
	all, err := c.replicaSets.ReplicaSets(d.Namespace).List(everything())
	if err != nil {
		return nil, err
	}
	var out []*appsv1.ReplicaSet
	for _, rs := range all {
		if ref := metav1.GetControllerOf(rs); ref != nil && ref.UID == d.UID {
			out = append(out, rs)
		}
	}
	return out, nil
}

// gestationOwner is the owner reference objects created for a gestation
// carry, so they go when it goes.
func gestationOwner(g *api.Gestation) metav1.OwnerReference {
	f := false
	return metav1.OwnerReference{APIVersion: api.Group + "/" + api.Version, Kind: api.Kind, Name: g.Name, UID: g.UID, BlockOwnerDeletion: &f}
}

func everything() labels.Selector { return labels.Everything() }
