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
// Models: a small-to-moderate, task-dependent female advantage in
// remembering where things were (Voyer et al. 2007). The controller manager
// writes down which nodes a workload lives on; the scheduler sends a
// replacement pod back to where the one it replaces lived, so caches and
// local volumes stay warm instead of being scattered on every restart.
//
// Memory must not fight spreading: for a Deployment or ReplicaSet, a node
// where a replica of the same workload currently runs gets no points (the
// memory is for returning home, not for piling replicas together). A
// StatefulSet remembers each ordinal's node separately, and a pod is only
// drawn to its own ordinal's node.
package placement

import (
	"sort"
	"strconv"
	"strings"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	appslisters "k8s.io/client-go/listers/apps/v1"

	"k8s.io/kubernetes/pkg/kyvernetria"
)

// Holder kinds.
const (
	Deployment  = "Deployment"
	ReplicaSet  = "ReplicaSet"
	StatefulSet = "StatefulSet"
)

// Holder identifies the object that carries a pod's placement memory. It
// is the long-lived owner: a Deployment rather than its current ReplicaSet,
// so that the memory survives rollouts.
type Holder struct {
	Kind      string
	Namespace string
	Name      string
}

// String renders the holder as a work queue key.
func (h Holder) String() string {
	return h.Kind + "/" + h.Namespace + "/" + h.Name
}

// ParseHolder reverses String.
func ParseHolder(key string) (Holder, bool) {
	parts := strings.Split(key, "/")
	if len(parts) != 3 {
		return Holder{}, false
	}
	return Holder{Kind: parts[0], Namespace: parts[1], Name: parts[2]}, true
}

// appsOwner returns the controller of an object if it is one of the
// built-in apps/v1 kinds; lookalikes from other API groups (for example
// apps.kruise.io StatefulSets) are not ours to annotate.
func appsOwner(obj metav1.Object) *metav1.OwnerReference {
	ref := metav1.GetControllerOfNoCopy(obj)
	if ref == nil {
		return nil
	}
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil || gv.Group != "apps" {
		return nil
	}
	return ref
}

// HolderFor resolves the memory holder of a pod. ok is false for pods that
// no apps/v1 workload controller owns (nothing to remember them by).
func HolderFor(pod *v1.Pod, replicaSets appslisters.ReplicaSetLister) (Holder, bool) {
	ref := appsOwner(pod)
	if ref == nil {
		return Holder{}, false
	}
	switch ref.Kind {
	case StatefulSet:
		return Holder{Kind: StatefulSet, Namespace: pod.Namespace, Name: ref.Name}, true
	case ReplicaSet:
		if replicaSets != nil {
			if rs, err := replicaSets.ReplicaSets(pod.Namespace).Get(ref.Name); err == nil {
				if owner := appsOwner(rs); owner != nil && owner.Kind == Deployment {
					return Holder{Kind: Deployment, Namespace: pod.Namespace, Name: owner.Name}, true
				}
			}
		}
		return Holder{Kind: ReplicaSet, Namespace: pod.Namespace, Name: ref.Name}, true
	}
	return Holder{}, false
}

// Ordinal returns a StatefulSet pod's ordinal.
func Ordinal(pod *v1.Pod, statefulSet string) (int, bool) {
	raw, ok := pod.Labels["apps.kubernetes.io/pod-index"]
	if !ok {
		raw, ok = strings.CutPrefix(pod.Name, statefulSet+"-")
	}
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(raw)
	return n, err == nil && n >= 0
}

// Decode reads the remembered nodes of a Deployment or ReplicaSet, most
// recent first.
func Decode(annotations map[string]string) []string {
	raw := annotations[kyvernetria.RememberedNodesAnnotation]
	if raw == "" {
		return nil
	}
	return strings.Split(raw, ",")
}

// Encode renders the remembered nodes as an annotation value.
func Encode(nodes []string) string {
	return strings.Join(nodes, ",")
}

// DecodeOrdinals reads a StatefulSet's memory: ordinal=node pairs.
func DecodeOrdinals(annotations map[string]string) map[int]string {
	raw := annotations[kyvernetria.RememberedNodesAnnotation]
	if raw == "" {
		return nil
	}
	out := map[int]string{}
	for _, pair := range strings.Split(raw, ",") {
		ord, node, ok := strings.Cut(pair, "=")
		if n, err := strconv.Atoi(ord); ok && err == nil && node != "" {
			out[n] = node
		}
	}
	return out
}

// EncodeOrdinals renders a StatefulSet's memory, ordered by ordinal.
func EncodeOrdinals(m map[int]string) string {
	ords := make([]int, 0, len(m))
	for o := range m {
		ords = append(ords, o)
	}
	sort.Ints(ords)
	pairs := make([]string, 0, len(ords))
	for _, o := range ords {
		pairs = append(pairs, strconv.Itoa(o)+"="+m[o])
	}
	return strings.Join(pairs, ",")
}

// Recall is what a holder should remember, given what it remembers
// (previous annotation value) and where its running pods are now
// (ordinal -> node for a StatefulSet; for other holders the ordinals are
// ignored). It is a pure function of its inputs, so recomputing it on an
// unchanged cluster gives the same value and the controller writes
// nothing.
//
// A workload running on more than kyvernetria.MaxRememberedNodes nodes is
// spread too widely for memory to mean anything, and remembering part of it
// would evict current homes in turn on every pass: the memory is cleared.
func Recall(kind, previous string, running map[string][]int) string {
	if len(running) > kyvernetria.MaxRememberedNodes {
		return ""
	}
	annotations := map[string]string{kyvernetria.RememberedNodesAnnotation: previous}
	if kind == StatefulSet {
		return recallOrdinals(DecodeOrdinals(annotations), running)
	}
	return recallNodes(Decode(annotations), running)
}

func recallNodes(previous []string, running map[string][]int) string {
	known := sets.New(previous...)
	var added []string
	for node := range running {
		if !known.Has(node) {
			added = append(added, node)
		}
	}
	sort.Strings(added)
	nodes := append(added, previous...)
	// Fade the oldest homes first, never a current one.
	for i := len(nodes) - 1; i >= 0 && len(nodes) > kyvernetria.MaxRememberedNodes; i-- {
		if _, current := running[nodes[i]]; !current {
			nodes = append(nodes[:i], nodes[i+1:]...)
		}
	}
	return Encode(nodes)
}

func recallOrdinals(m map[int]string, running map[string][]int) string {
	if m == nil {
		m = map[int]string{}
	}
	current := sets.New[int]()
	for node, ords := range running {
		for _, o := range ords {
			m[o] = node
			current.Insert(o)
		}
	}
	if len(m) > kyvernetria.MaxRememberedNodes {
		ords := make([]int, 0, len(m))
		for o := range m {
			ords = append(ords, o)
		}
		sort.Sort(sort.Reverse(sort.IntSlice(ords)))
		for _, o := range ords {
			if len(m) <= kyvernetria.MaxRememberedNodes {
				break
			}
			if !current.Has(o) {
				delete(m, o)
			}
		}
	}
	return EncodeOrdinals(m)
}

// Score returns how strongly a node is remembered: 100 for the most recent
// home, fading by 5 per step, never below 50 while it is remembered at all.
func Score(nodes []string, node string) int64 {
	for i, n := range nodes {
		if n == node {
			return scoreAt(i)
		}
	}
	return 0
}

func scoreAt(i int) int64 {
	s := int64(100 - 5*i)
	if s < 50 {
		s = 50
	}
	return s
}

// Scores maps each remembered node to its score, leaving out busy nodes
// (where a replica of the same workload runs right now).
func Scores(nodes []string, busy sets.Set[string]) map[string]int64 {
	out := map[string]int64{}
	for i, n := range nodes {
		if !busy.Has(n) {
			out[n] = scoreAt(i)
		}
	}
	return out
}
