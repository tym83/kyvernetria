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

// Package placement is the shared half of object-location memory: which
// object remembers where a pod lived, and how the memory is encoded.
//
// Models: a female advantage in object-location memory (Voyer et al. 2007).
// The controller manager writes down which nodes a workload lived on; the
// scheduler prefers those nodes when the workload comes back, so caches and
// local volumes stay warm instead of being scattered on every restart.
package placement

import (
	"strings"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	appslisters "k8s.io/client-go/listers/apps/v1"

	"k8s.io/kubernetes/pkg/kyvernetria"
)

// Holder identifies the object that carries a pod's placement memory. It
// is the long-lived owner: a Deployment rather than its current ReplicaSet,
// so that the memory survives rollouts.
type Holder struct {
	Kind      string
	Namespace string
	Name      string
}

// HolderFor resolves the memory holder of a pod. ok is false for pods that
// no workload controller owns (nothing to remember them by).
func HolderFor(pod *v1.Pod, replicaSets appslisters.ReplicaSetLister) (Holder, bool) {
	ref := metav1.GetControllerOf(pod)
	if ref == nil {
		return Holder{}, false
	}
	switch ref.Kind {
	case "StatefulSet", "Deployment":
		return Holder{Kind: ref.Kind, Namespace: pod.Namespace, Name: ref.Name}, true
	case "ReplicaSet":
		if replicaSets != nil {
			if rs, err := replicaSets.ReplicaSets(pod.Namespace).Get(ref.Name); err == nil {
				if owner := metav1.GetControllerOf(rs); owner != nil && owner.Kind == "Deployment" {
					return Holder{Kind: "Deployment", Namespace: pod.Namespace, Name: owner.Name}, true
				}
			}
		}
		return Holder{Kind: "ReplicaSet", Namespace: pod.Namespace, Name: ref.Name}, true
	}
	return Holder{}, false
}

// Decode reads the remembered nodes from an annotation map.
func Decode(annotations map[string]string) []string {
	raw := annotations[kyvernetria.RememberedNodesAnnotation]
	if raw == "" {
		return nil
	}
	return strings.Split(raw, ",")
}

// Remember adds a new home at the front of nodes, capped at
// kyvernetria.MaxRememberedNodes so the oldest homes fade. A node that is
// already remembered stays where it is: replicas living on different nodes
// must not keep reordering the list (and patching the owner) on every pod
// update. changed is false when nothing was added.
func Remember(nodes []string, node string) (updated []string, changed bool) {
	for _, n := range nodes {
		if n == node {
			return nodes, false
		}
	}
	updated = append(updated, node)
	for _, n := range nodes {
		if n != node && len(updated) < kyvernetria.MaxRememberedNodes {
			updated = append(updated, n)
		}
	}
	return updated, true
}

// Encode renders the remembered nodes as an annotation value.
func Encode(nodes []string) string {
	return strings.Join(nodes, ",")
}

// Score returns how strongly a node is remembered: 100 for the most recent
// home, fading by 5 per step, never below 50 while it is remembered at all.
func Score(nodes []string, node string) int64 {
	for i, n := range nodes {
		if n == node {
			s := int64(100 - 5*i)
			if s < 50 {
				s = 50
			}
			return s
		}
	}
	return 0
}
