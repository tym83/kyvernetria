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
package placememory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	appsinformers "k8s.io/client-go/informers/apps/v1"
	coreinformers "k8s.io/client-go/informers/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	appslisters "k8s.io/client-go/listers/apps/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	"k8s.io/kubernetes/pkg/kyvernetria"
	"k8s.io/kubernetes/pkg/kyvernetria/placement"
)

var metav1PatchOptions = metav1.PatchOptions{FieldManager: "kyvernetria-place-memory"}

// Controller records pod placements on their long-lived owners.
type Controller struct {
	client       clientset.Interface
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
	_, err := pods.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.enqueue,
		UpdateFunc: func(_, obj interface{}) { c.enqueue(obj) },
	})
	return c, err
}

// key encodes kind/namespace/name/node.
func key(h placement.Holder, node string) string {
	return strings.Join([]string{h.Kind, h.Namespace, h.Name, node}, "/")
}

func (c *Controller) enqueue(obj interface{}) {
	pod, ok := obj.(*v1.Pod)
	if !ok || pod.Spec.NodeName == "" || pod.Status.Phase != v1.PodRunning || pod.DeletionTimestamp != nil {
		return
	}
	if h, ok := placement.HolderFor(pod, c.replicaSets); ok {
		c.queue.Add(key(h, pod.Spec.NodeName))
	}
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

func (c *Controller) sync(ctx context.Context, k string) error {
	parts := strings.Split(k, "/")
	if len(parts) != 4 {
		return nil
	}
	kind, ns, name, node := parts[0], parts[1], parts[2], parts[3]

	var annotations map[string]string
	switch kind {
	case "Deployment":
		d, err := c.deployments.Deployments(ns).Get(name)
		if err != nil {
			return nil
		}
		annotations = d.Annotations
	case "StatefulSet":
		s, err := c.statefulSets.StatefulSets(ns).Get(name)
		if err != nil {
			return nil
		}
		annotations = s.Annotations
	case "ReplicaSet":
		rs, err := c.replicaSets.ReplicaSets(ns).Get(name)
		if err != nil {
			return nil
		}
		annotations = rs.Annotations
	default:
		return nil
	}

	updated, changed := placement.Remember(placement.Decode(annotations), node)
	if !changed {
		return nil
	}
	patch, err := json.Marshal(map[string]interface{}{
		"metadata": map[string]interface{}{
			"annotations": map[string]string{kyvernetria.RememberedNodesAnnotation: placement.Encode(updated)},
		},
	})
	if err != nil {
		return err
	}
	apps := c.client.AppsV1()
	switch kind {
	case "Deployment":
		_, err = apps.Deployments(ns).Patch(ctx, name, types.MergePatchType, patch, metav1PatchOptions)
	case "StatefulSet":
		_, err = apps.StatefulSets(ns).Patch(ctx, name, types.MergePatchType, patch, metav1PatchOptions)
	case "ReplicaSet":
		_, err = apps.ReplicaSets(ns).Patch(ctx, name, types.MergePatchType, patch, metav1PatchOptions)
	}
	if err != nil {
		return fmt.Errorf("patching %s %s/%s: %w", kind, ns, name, err)
	}
	return nil
}
