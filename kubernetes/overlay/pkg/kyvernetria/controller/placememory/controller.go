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

// Package placememory is the controller half of object-location memory: it
// writes down where each workload's pods run. See package
// k8s.io/kubernetes/pkg/kyvernetria/placement.
//
// The work queue holds workloads (holders), never single placements: each
// sync recomputes the holder's memory from its current pods and writes only
// when the value really changes, guarded by the resourceVersion it was
// computed from, so concurrent workers and stale caches cannot lose updates.
package placememory

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	appsinformers "k8s.io/client-go/informers/apps/v1"
	coreinformers "k8s.io/client-go/informers/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	appslisters "k8s.io/client-go/listers/apps/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	"k8s.io/kubernetes/pkg/kyvernetria"
	"k8s.io/kubernetes/pkg/kyvernetria/placement"
)

var patchOptions = metav1.PatchOptions{FieldManager: "kyvernetria-place-memory"}

// Controller records pod placements on their long-lived owners.
type Controller struct {
	client       clientset.Interface
	pods         corelisters.PodLister
	replicaSets  appslisters.ReplicaSetLister
	deployments  appslisters.DeploymentLister
	statefulSets appslisters.StatefulSetLister
	synced       []cache.InformerSynced
	queue        workqueue.TypedRateLimitingInterface[string]
}

// New creates the controller.
func New(client clientset.Interface, pods coreinformers.PodInformer, rs appsinformers.ReplicaSetInformer,
	deploys appsinformers.DeploymentInformer, sts appsinformers.StatefulSetInformer) (*Controller, error) {
	c := &Controller{
		client:       client,
		pods:         pods.Lister(),
		replicaSets:  rs.Lister(),
		deployments:  deploys.Lister(),
		statefulSets: sts.Lister(),
		synced: []cache.InformerSynced{
			pods.Informer().HasSynced, rs.Informer().HasSynced,
			deploys.Informer().HasSynced, sts.Informer().HasSynced,
		},
		queue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.DefaultTypedControllerRateLimiter[string](),
			workqueue.TypedRateLimitingQueueConfig[string]{Name: "kyvernetria_place_memory"}),
	}
	if _, err := pods.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.enqueuePod,
		UpdateFunc: func(_, obj interface{}) { c.enqueuePod(obj) },
	}); err != nil {
		return nil, err
	}
	// A workload that was too spread to remember may shrink back.
	scaled := func(kind string) cache.ResourceEventHandlerFuncs {
		return cache.ResourceEventHandlerFuncs{UpdateFunc: func(old, obj interface{}) {
			if replicas(old) != replicas(obj) {
				if m, err := meta.Accessor(obj); err == nil {
					c.queue.Add(placement.Holder{Kind: kind, Namespace: m.GetNamespace(), Name: m.GetName()}.String())
				}
			}
		}}
	}
	if _, err := deploys.Informer().AddEventHandler(scaled(placement.Deployment)); err != nil {
		return nil, err
	}
	if _, err := sts.Informer().AddEventHandler(scaled(placement.StatefulSet)); err != nil {
		return nil, err
	}
	return c, nil
}

func replicas(obj interface{}) int32 {
	switch o := obj.(type) {
	case *appsv1.Deployment:
		if o.Spec.Replicas != nil {
			return *o.Spec.Replicas
		}
	case *appsv1.StatefulSet:
		if o.Spec.Replicas != nil {
			return *o.Spec.Replicas
		}
	}
	return 1
}

func running(pod *v1.Pod) bool {
	return pod.Spec.NodeName != "" && pod.Status.Phase == v1.PodRunning && pod.DeletionTimestamp == nil
}

// enqueuePod queues the pod's holder unless the pod is already remembered
// where it runs, so the steady state costs one lister lookup per pod event.
func (c *Controller) enqueuePod(obj interface{}) {
	pod, ok := obj.(*v1.Pod)
	if !ok || !running(pod) {
		return
	}
	h, ok := placement.HolderFor(pod, c.replicaSets)
	if !ok {
		return
	}
	if obj, err := c.holder(h); err == nil && c.remembers(h, obj.GetAnnotations(), pod) {
		return
	}
	c.queue.Add(h.String())
}

func (c *Controller) remembers(h placement.Holder, annotations map[string]string, pod *v1.Pod) bool {
	if h.Kind == placement.StatefulSet {
		ord, ok := placement.Ordinal(pod, h.Name)
		return ok && placement.DecodeOrdinals(annotations)[ord] == pod.Spec.NodeName
	}
	for _, n := range placement.Decode(annotations) {
		if n == pod.Spec.NodeName {
			return true
		}
	}
	return false
}

// holder returns the holder object from the cache.
func (c *Controller) holder(h placement.Holder) (metav1.Object, error) {
	switch h.Kind {
	case placement.Deployment:
		return c.deployments.Deployments(h.Namespace).Get(h.Name)
	case placement.StatefulSet:
		return c.statefulSets.StatefulSets(h.Namespace).Get(h.Name)
	case placement.ReplicaSet:
		return c.replicaSets.ReplicaSets(h.Namespace).Get(h.Name)
	}
	return nil, fmt.Errorf("unknown holder kind %q", h.Kind)
}

// Run starts workers until ctx is done.
func (c *Controller) Run(ctx context.Context, workers int) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()
	logger := klog.FromContext(ctx)
	logger.Info("Starting kyvernetria place-memory controller")
	defer logger.Info("Shutting down kyvernetria place-memory controller")
	if !cache.WaitForNamedCacheSync("kyvernetria-place-memory", ctx.Done(), c.synced...) {
		return
	}
	for i := 0; i < workers; i++ {
		go wait.UntilWithContext(ctx, c.worker, time.Second)
	}
	<-ctx.Done()
}

func (c *Controller) worker(ctx context.Context) {
	for c.next(ctx) {
	}
}

func (c *Controller) next(ctx context.Context) bool {
	k, quit := c.queue.Get()
	if quit {
		return false
	}
	defer c.queue.Done(k)
	if err := c.sync(ctx, k); err != nil {
		utilruntime.HandleErrorWithContext(ctx, err, "Remembering placement failed", "key", k)
		c.queue.AddRateLimited(k)
		return true
	}
	c.queue.Forget(k)
	return true
}

// runningPods maps each node to the ordinals of the holder's running pods
// there (the ordinals are only meaningful for a StatefulSet).
func (c *Controller) runningPods(h placement.Holder) (map[string][]int, error) {
	pods, err := c.pods.Pods(h.Namespace).List(labels.Everything())
	if err != nil {
		return nil, err
	}
	out := map[string][]int{}
	for _, pod := range pods {
		if !running(pod) {
			continue
		}
		if ph, ok := placement.HolderFor(pod, c.replicaSets); !ok || ph != h {
			continue
		}
		ord := -1
		if h.Kind == placement.StatefulSet {
			var ok bool
			if ord, ok = placement.Ordinal(pod, h.Name); !ok {
				continue
			}
		}
		out[pod.Spec.NodeName] = append(out[pod.Spec.NodeName], ord)
	}
	return out, nil
}

func (c *Controller) sync(ctx context.Context, key string) error {
	h, ok := placement.ParseHolder(key)
	if !ok {
		return nil
	}
	cached, err := c.holder(h)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	pods, err := c.runningPods(h)
	if err != nil {
		return err
	}
	current := cached
	for attempt := 0; attempt < 5; attempt++ {
		previous := current.GetAnnotations()[kyvernetria.RememberedNodesAnnotation]
		want := placement.Recall(h.Kind, previous, pods)
		if want == previous {
			return nil
		}
		err := c.write(ctx, h, current.GetResourceVersion(), want)
		if !apierrors.IsConflict(err) {
			return err
		}
		// Someone else changed the holder since our copy: start again from
		// the live object instead of overwriting their change.
		current, err = c.get(ctx, h)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
	}
	return fmt.Errorf("%s %s/%s kept changing while its memory was written", h.Kind, h.Namespace, h.Name)
}

// write sets (or, for "", removes) the annotation, but only if the holder
// is still at resourceVersion.
func (c *Controller) write(ctx context.Context, h placement.Holder, resourceVersion, value string) error {
	var annotation interface{} = value
	if value == "" {
		annotation = nil // a JSON merge patch null removes the key
	}
	patch, err := json.Marshal(map[string]interface{}{
		"metadata": map[string]interface{}{
			"resourceVersion": resourceVersion,
			"annotations":     map[string]interface{}{kyvernetria.RememberedNodesAnnotation: annotation},
		},
	})
	if err != nil {
		return err
	}
	apps := c.client.AppsV1()
	switch h.Kind {
	case placement.Deployment:
		_, err = apps.Deployments(h.Namespace).Patch(ctx, h.Name, types.MergePatchType, patch, patchOptions)
	case placement.StatefulSet:
		_, err = apps.StatefulSets(h.Namespace).Patch(ctx, h.Name, types.MergePatchType, patch, patchOptions)
	case placement.ReplicaSet:
		_, err = apps.ReplicaSets(h.Namespace).Patch(ctx, h.Name, types.MergePatchType, patch, patchOptions)
	}
	if err != nil && !apierrors.IsConflict(err) {
		return fmt.Errorf("patching %s %s/%s: %w", h.Kind, h.Namespace, h.Name, err)
	}
	return err
}

func (c *Controller) get(ctx context.Context, h placement.Holder) (metav1.Object, error) {
	apps := c.client.AppsV1()
	var obj runtime.Object
	var err error
	switch h.Kind {
	case placement.Deployment:
		obj, err = apps.Deployments(h.Namespace).Get(ctx, h.Name, metav1.GetOptions{})
	case placement.StatefulSet:
		obj, err = apps.StatefulSets(h.Namespace).Get(ctx, h.Name, metav1.GetOptions{})
	case placement.ReplicaSet:
		obj, err = apps.ReplicaSets(h.Namespace).Get(ctx, h.Name, metav1.GetOptions{})
	default:
		return nil, fmt.Errorf("unknown holder kind %q", h.Kind)
	}
	if err != nil {
		return nil, err
	}
	return meta.Accessor(obj)
}
