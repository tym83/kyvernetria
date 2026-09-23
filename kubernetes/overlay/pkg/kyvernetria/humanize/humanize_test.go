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

package humanize

import "testing"

func TestSchedulingFailure(t *testing.T) {
	for _, tc := range []struct {
		name    string
		total   int
		reasons map[string]int
		want    string
	}{
		{
			name:    "all nodes short on cpu",
			total:   3,
			reasons: map[string]int{"Insufficient cpu": 3},
			want:    "We couldn't place default/api yet: every node is short on CPU. I'll try again as soon as something changes. (upstream)",
		},
		{
			name:    "mixed",
			total:   3,
			reasons: map[string]int{"Insufficient memory": 2, "node(s) had untolerated taint {node-role.kubernetes.io/control-plane: }": 1},
			want:    "We couldn't place default/api yet: 2 nodes are short on memory, 1 node has a taint the pod doesn't tolerate. I'll try again as soon as something changes. (upstream)",
		},
		{
			name:    "unknown reason is passed through",
			total:   2,
			reasons: map[string]int{"something new": 2},
			want:    "We couldn't place default/api yet: 2 nodes: something new. I'll try again as soon as something changes. (upstream)",
		},
		{
			name:  "no nodes",
			total: 0,
			want:  "We couldn't place default/api yet: the cluster has no nodes I can use. (upstream)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := SchedulingFailure("default/api", tc.total, tc.reasons, "upstream"); got != tc.want {
				t.Errorf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}
