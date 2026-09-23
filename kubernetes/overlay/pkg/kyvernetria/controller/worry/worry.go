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

// Package worry raises a warning before a node is full, not after.
//
// Models: higher average neuroticism (Costa et al. 2001; Weisberg et al.
// 2011), i.e. more sensitivity to potential threats. Upstream Kubernetes is
// silent until pods stop fitting; the worry controller speaks up when a
// node's requests cross a threshold, and says so again when it relaxes.
// Its downside, more alerts, is what `kyvctl calm` is for.
package worry

import (
	"context"
	"fmt"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	coreinformers "k8s.io/client-go/informers/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	resourcehelper "k8s.io/component-helpers/resource"
	"k8s.io/klog/v2"

	"k8s.io/kubernetes/pkg/kyvernetria"
)

// Event reasons.
const (
	ReasonWorried  = "Worried"
	ReasonRelieved = "Relieved"
)

// hysteresis keeps a node hovering at the threshold from flapping.
const hysteresis = 5

// Controller watches node requests against allocatable.
type Controller struct {
	nodes    corelisters.NodeLister
	pods     corelisters.PodLister
	synced   []cache.InformerSynced
	recorder record.EventRecorder
	period   time.Duration
	worried  map[string]bool // node/resource
}

// New creates the controller.
func New(nodes coreinformers.NodeInformer, pods coreinformers.PodInformer, recorder record.EventRecorder) *Controller {
	return &Controller{
		nodes:    nodes.Lister(),
		pods:     pods.Lister(),
		synced:   []cache.InformerSynced{nodes.Informer().HasSynced, pods.Informer().HasSynced},
		recorder: recorder,
		period:   time.Minute,
		worried:  map[string]bool{},
	}
}

// Run checks every period until ctx is done.
func (c *Controller) Run(ctx context.Context) {
	defer utilruntime.HandleCrash()
	logger := klog.FromContext(ctx)
	logger.Info("Starting kyvernetria worry controller")
	defer logger.Info("Shutting down kyvernetria worry controller")
	if !cache.WaitForNamedCacheSync("kyvernetria-worry", ctx.Done(), c.synced...) {
		return
	}
	wait.UntilWithContext(ctx, func(ctx context.Context) {
		if err := c.check(); err != nil {
			utilruntime.HandleErrorWithContext(ctx, err, "Worrying failed")
		}
	}, c.period)
}

func (c *Controller) check() error {
	nodes, err := c.nodes.List(labels.Everything())
	if err != nil {
		return err
	}
	pods, err := c.pods.List(labels.Everything())
	if err != nil {
		return err
	}
	requested := Requested(pods)
	for _, node := range nodes {
		for _, res := range []v1.ResourceName{v1.ResourceCPU, v1.ResourceMemory} {
			c.feel(node, res, Percent(requested[node.Name][res], node.Status.Allocatable[res]))
		}
	}
	return nil
}

// feel turns a usage percentage into at most one event per state change.
func (c *Controller) feel(node *v1.Node, res v1.ResourceName, pct int64) {
	key := node.Name + "/" + string(res)
	switch {
	case !c.worried[key] && pct >= kyvernetria.WorryThresholdPercent:
		c.worried[key] = true
		c.recorder.Eventf(node, v1.EventTypeWarning, ReasonWorried,
			"%s requests on %s reached %d%% of allocatable. Nothing is failing yet; I'm telling you early.",
			label(res), node.Name, pct)
	case c.worried[key] && pct < kyvernetria.WorryThresholdPercent-hysteresis:
		delete(c.worried, key)
		c.recorder.Eventf(node, v1.EventTypeNormal, ReasonRelieved,
			"%s requests on %s are back to %d%% of allocatable.", label(res), node.Name, pct)
	}
}

func label(res v1.ResourceName) string {
	if res == v1.ResourceCPU {
		return "CPU"
	}
	return "Memory"
}

// Requested sums the requests of non-terminal pods per node and resource.
func Requested(pods []*v1.Pod) map[string]v1.ResourceList {
	out := map[string]v1.ResourceList{}
	for _, pod := range pods {
		if pod.Spec.NodeName == "" || pod.Status.Phase == v1.PodSucceeded || pod.Status.Phase == v1.PodFailed {
			continue
		}
		sum, ok := out[pod.Spec.NodeName]
		if !ok {
			sum = v1.ResourceList{}
			out[pod.Spec.NodeName] = sum
		}
		for name, q := range resourcehelper.PodRequests(pod, resourcehelper.PodResourcesOptions{}) {
			total := sum[name]
			total.Add(q)
			sum[name] = total
		}
	}
	return out
}

// Percent returns used as a whole percentage of total; 0 when total is 0.
func Percent(used, total resource.Quantity) int64 {
	if total.IsZero() {
		return 0
	}
	return used.MilliValue() * 100 / total.MilliValue()
}

// String is for logs and tests.
func (c *Controller) String() string {
	return fmt.Sprintf("worry(%d worries)", len(c.worried))
}
