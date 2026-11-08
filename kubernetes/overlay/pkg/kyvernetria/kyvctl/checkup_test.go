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

package kyvctl

import (
	"bytes"
	"strings"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func checkupPod(name string, ready bool, probe *v1.Probe, last *v1.ContainerStateTerminated) v1.Pod {
	status := v1.ConditionFalse
	if ready {
		status = v1.ConditionTrue
	}
	return v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: name},
		Spec:       v1.PodSpec{Containers: []v1.Container{{Name: "app", ReadinessProbe: probe}}},
		Status: v1.PodStatus{
			Conditions:        []v1.PodCondition{{Type: v1.PodReady, Status: status}},
			ContainerStatuses: []v1.ContainerStatus{{Name: "app", RestartCount: 4, LastTerminationState: v1.ContainerState{Terminated: last}}},
		},
	}
}

func TestCheckup(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	httpProbe := &v1.Probe{ProbeHandler: v1.ProbeHandler{HTTPGet: &v1.HTTPGetAction{Path: "/ready", Port: intstr.FromInt32(8080)}}}
	for _, c := range []struct {
		name  string
		pod   v1.Pod
		signs []string
	}{
		{"healthy", checkupPod("a", true, httpProbe, nil), nil},
		{"restarted recently", checkupPod("a", true, httpProbe,
			&v1.ContainerStateTerminated{ExitCode: 2, Reason: "Error", FinishedAt: metav1.NewTime(now.Add(-10 * time.Minute))}),
			[]string{"restarted 10m0s ago (4 restarts in all), after exiting with 2 (Error)"}},
		{"restarted long ago", checkupPod("a", true, httpProbe,
			&v1.ContainerStateTerminated{ExitCode: 2, FinishedAt: metav1.NewTime(now.Add(-2 * time.Hour))}), nil},
		{"old OOM kill", checkupPod("a", true, httpProbe,
			&v1.ContainerStateTerminated{ExitCode: 137, Reason: "OOMKilled", FinishedAt: metav1.NewTime(now.Add(-2 * time.Hour))}),
			[]string{"running out of memory"}},
		{"no probe", checkupPod("a", true, nil, nil), []string{"no readiness probe"}},
		{"port only", checkupPod("a", true, &v1.Probe{ProbeHandler: v1.ProbeHandler{TCPSocket: &v1.TCPSocketAction{Port: intstr.FromInt32(80)}}}, nil),
			[]string{"only opens a port"}},
		{"always true", checkupPod("a", true, &v1.Probe{ProbeHandler: v1.ProbeHandler{Exec: &v1.ExecAction{Command: []string{"sh", "-c", "exit 0"}}}}, nil),
			[]string{`runs "sh -c exit 0", which always succeeds`}},
		{"real command", checkupPod("a", true, &v1.Probe{ProbeHandler: v1.ProbeHandler{Exec: &v1.ExecAction{Command: []string{"pg_isready"}}}}, nil), nil},
	} {
		got := Checkup(&c.pod, now)
		if len(got) != len(c.signs) {
			t.Errorf("%s: signs %q, want %d", c.name, got, len(c.signs))
			continue
		}
		for i := range got {
			if !strings.Contains(got[i], c.signs[i]) {
				t.Errorf("%s: sign %q, want %q in it", c.name, got[i], c.signs[i])
			}
		}
	}
}

func TestRenderCheckupLooksOnlyPastReady(t *testing.T) {
	now := time.Now()
	var out bytes.Buffer
	renderCheckup(&out, []v1.Pod{checkupPod("unready", false, nil, nil), checkupPod("masked", true, nil, nil)}, now)
	if strings.Contains(out.String(), "unready") || !strings.Contains(out.String(), "shop/masked is Ready, but:") ||
		!strings.Contains(out.String(), "1 of 1 Ready pods") {
		t.Errorf("unexpected checkup:\n%s", out.String())
	}
	out.Reset()
	renderCheckup(&out, []v1.Pod{checkupPod("unready", false, nil, nil)}, now)
	if !strings.Contains(out.String(), "No pod here is Ready") {
		t.Errorf("unexpected checkup:\n%s", out.String())
	}
}
