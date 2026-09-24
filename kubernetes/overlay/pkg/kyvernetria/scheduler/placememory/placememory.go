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
// score plugin that sends a pod back to where its workload lived. See
// package k8s.io/kubernetes/pkg/kyvernetria/placement.
//
// PreScore resolves the memory once per scheduling cycle and returns Skip
// when there is nothing to remember, so Score is a map lookup per node.
// A StatefulSet pod is drawn only to its own ordinal's node. A Deployment
// or ReplicaSet pod gets no points on nodes where a replica of the same
// workload runs right now, so memory brings a replacement home instead of
// piling replicas together against topology spreading.
package placememory

import (
	"context"
	"fmt"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	appslisters "k8s.io/client-go/listers/apps/v1"
	fwk "k8s.io/kube-scheduler/framework"

	"k8s.io/kubernetes/pkg/kyvernetria/placement"
)

// Name is the plugin name used in scheduler profiles.
const Name = "PlaceMemory"

const stateKey fwk.StateKey = Name

// PlaceMemory scores remembered nodes higher.
type PlaceMemory struct {
	replicaSets  appslisters.ReplicaSetLister
	deployments  appslisters.DeploymentLister
	statefulSets appslisters.StatefulSetLister
}

var _ fwk.PreScorePlugin = &PlaceMemory{}
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

// scores is the per-cycle state: the points of each remembered node.
type scores map[string]int64

// Clone is not needed: the state is never modified after PreScore.
func (s scores) Clone() fwk.StateData {
	return s
}

// PreScore works out which nodes the pod's workload remembers.
func (pl *PlaceMemory) PreScore(_ context.Context, state fwk.CycleState, pod *v1.Pod, nodes []fwk.NodeInfo) *fwk.Status {
	s := pl.scores(pod, nodes)
	if len(s) == 0 {
		return fwk.NewStatus(fwk.Skip)
	}
	state.Write(stateKey, s)
	return nil
}

func (pl *PlaceMemory) scores(pod *v1.Pod, nodes []fwk.NodeInfo) scores {
	holder, ok := placement.HolderFor(pod, pl.replicaSets)
	if !ok {
		return nil
	}
	switch holder.Kind {
	case placement.StatefulSet:
		sts, err := pl.statefulSets.StatefulSets(holder.Namespace).Get(holder.Name)
		if err != nil {
			return nil
		}
		ord, ok := placement.Ordinal(pod, holder.Name)
		if !ok {
			return nil
		}
		if node := placement.DecodeOrdinals(sts.Annotations)[ord]; node != "" {
			return scores{node: 100}
		}
		return nil
	case placement.Deployment:
		d, err := pl.deployments.Deployments(holder.Namespace).Get(holder.Name)
		if err != nil {
			return nil
		}
		return pl.homes(pod, holder, placement.Decode(d.Annotations), nodes)
	case placement.ReplicaSet:
		rs, err := pl.replicaSets.ReplicaSets(holder.Namespace).Get(holder.Name)
		if err != nil {
			return nil
		}
		return pl.homes(pod, holder, placement.Decode(rs.Annotations), nodes)
	}
	return nil
}

// homes scores the remembered nodes that no replica of the workload
// occupies right now.
func (pl *PlaceMemory) homes(pod *v1.Pod, holder placement.Holder, remembered []string, nodes []fwk.NodeInfo) scores {
	if len(remembered) == 0 {
		return nil
	}
	wanted := sets.New(remembered...)
	busy := sets.New[string]()
	sibling := map[string]bool{} // by owner UID, resolved once per cycle
	for _, ni := range nodes {
		node := ni.Node()
		if node == nil || !wanted.Has(node.Name) {
			continue
		}
		for _, pi := range ni.GetPods() {
			other := pi.GetPod()
			if other.Namespace != pod.Namespace || other.UID == pod.UID || other.DeletionTimestamp != nil {
				continue
			}
			ref := metav1.GetControllerOfNoCopy(other)
			if ref == nil {
				continue
			}
			same, seen := sibling[string(ref.UID)]
			if !seen {
				h, ok := placement.HolderFor(other, pl.replicaSets)
				same = ok && h == holder
				sibling[string(ref.UID)] = same
			}
			if same {
				busy.Insert(node.Name)
				break
			}
		}
	}
	return scores(placement.Scores(remembered, busy))
}

// Score returns how strongly the pod's workload remembers the node.
func (pl *PlaceMemory) Score(_ context.Context, state fwk.CycleState, _ *v1.Pod, nodeInfo fwk.NodeInfo) (int64, *fwk.Status) {
	node := nodeInfo.Node()
	if node == nil {
		return 0, nil
	}
	data, err := state.Read(stateKey)
	if err != nil {
		return 0, nil // PreScore skipped: nothing remembered
	}
	s, ok := data.(scores)
	if !ok {
		return 0, fwk.AsStatus(fmt.Errorf("%s: unexpected state %T", Name, data))
	}
	return s[node.Name], nil
}

// ScoreExtensions of the Score plugin.
func (pl *PlaceMemory) ScoreExtensions() fwk.ScoreExtensions {
	return nil
}
