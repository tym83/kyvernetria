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

package util

import (
	"testing"

	apps "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/kubernetes/pkg/kyvernetria"
)

// The place memory belongs to the Deployment; copying it to every new
// ReplicaSet would turn each memory write into ReplicaSet writes too.
func TestPlaceMemoryIsNotCopiedToReplicaSets(t *testing.T) {
	d := &apps.Deployment{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
		kyvernetria.RememberedNodesAnnotation: "a,b",
		"team":                                "shop",
	}}}
	rs := &apps.ReplicaSet{}
	copyDeploymentAnnotationsToReplicaSet(d, rs)
	if _, ok := rs.Annotations[kyvernetria.RememberedNodesAnnotation]; ok {
		t.Error("place memory was copied to the ReplicaSet")
	}
	if rs.Annotations["team"] != "shop" {
		t.Error("ordinary annotations are no longer copied")
	}
}
