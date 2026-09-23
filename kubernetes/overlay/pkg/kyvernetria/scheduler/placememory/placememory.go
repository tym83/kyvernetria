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

// Package placememory is the scheduler half of object-location memory: a
// score plugin that prefers nodes a workload already lived on. See package
// k8s.io/kubernetes/pkg/kyvernetria/placement.
package placememory

import (
	"context"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	appslisters "k8s.io/client-go/listers/apps/v1"
	fwk "k8s.io/kube-scheduler/framework"

	"k8s.io/kubernetes/pkg/kyvernetria/placement"
)

// Name is the plugin name used in scheduler profiles.
const Name = "PlaceMemory"

// PlaceMemory scores remembered nodes higher.
type PlaceMemory struct {
	replicaSets  appslisters.ReplicaSetLister
	deployments  appslisters.DeploymentLister
	statefulSets appslisters.StatefulSetLister
}

var _ fwk.ScorePlugin = &PlaceMemory{}

// New initializes the plugin.
func New(_ context.Context, _ runtime.Object, h fwk.Handle) (fwk.Plugin, error) {
	apps := h.SharedInformerFactory().Apps().V1()
	return &PlaceMemory{
		replicaSets:  apps.ReplicaSets().Lister(),
		deployments:  apps.Deployments().Lister(),
		statefulSets: apps.StatefulSets().Lister(),
	}, nil
}

// Name returns the plugin name.
func (pl *PlaceMemory) Name() string {
	return Name
}

// Score returns how strongly the pod's workload remembers the node.
func (pl *PlaceMemory) Score(_ context.Context, _ fwk.CycleState, pod *v1.Pod, nodeInfo fwk.NodeInfo) (int64, *fwk.Status) {
	node := nodeInfo.Node()
	if node == nil {
		return 0, nil
	}
	return placement.Score(pl.remembered(pod), node.Name), nil
}

// ScoreExtensions of the Score plugin.
func (pl *PlaceMemory) ScoreExtensions() fwk.ScoreExtensions {
	return nil
}

func (pl *PlaceMemory) remembered(pod *v1.Pod) []string {
	holder, ok := placement.HolderFor(pod, pl.replicaSets)
	if !ok {
		return nil
	}
	switch holder.Kind {
	case "Deployment":
		if d, err := pl.deployments.Deployments(holder.Namespace).Get(holder.Name); err == nil {
			return placement.Decode(d.Annotations)
		}
	case "StatefulSet":
		if s, err := pl.statefulSets.StatefulSets(holder.Namespace).Get(holder.Name); err == nil {
			return placement.Decode(s.Annotations)
		}
	case "ReplicaSet":
		if rs, err := pl.replicaSets.ReplicaSets(holder.Namespace).Get(holder.Name); err == nil {
			return placement.Decode(rs.Annotations)
		}
	}
	return nil
}
