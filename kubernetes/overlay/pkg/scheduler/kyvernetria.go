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

package scheduler

import (
	"errors"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	fwk "k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/pkg/apis/core/validation"
	"k8s.io/kubernetes/pkg/scheduler/framework"

	"k8s.io/kubernetes/pkg/kyvernetria/humanize"
)

// kyvernetriaSchedulingMessage renders the FailedScheduling event for a
// person. Only the event text changes; the pod condition keeps the
// upstream message. The result fits the event note limit with the upstream
// text intact, so truncateMessage leaves it alone.
func kyvernetriaSchedulingMessage(pod *v1.Pod, err error, upstream string) string {
	var fitErr *framework.FitError
	if !errors.As(err, &fitErr) {
		return upstream
	}
	reasons := map[string]int{}
	d := fitErr.Diagnosis
	switch {
	case d.PreFilterMsg != "":
		reasons[d.PreFilterMsg] = fitErr.NumAllNodes
	case d.NodeToStatus != nil:
		d.NodeToStatus.ForEachExplicitNode(func(_ string, status *fwk.Status) {
			for _, r := range status.Reasons() {
				reasons[r]++
			}
		})
		if n := d.NodeToStatus.Len(); n < fitErr.NumAllNodes {
			for _, r := range d.NodeToStatus.AbsentNodesStatus().Reasons() {
				reasons[r] += fitErr.NumAllNodes - n
			}
		}
	}
	return humanize.SchedulingFailure(klog.KObj(pod).String(), fitErr.NumAllNodes, reasons, upstream, validation.NoteLengthLimit)
}
