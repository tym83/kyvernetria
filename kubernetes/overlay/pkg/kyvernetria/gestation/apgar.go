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

package gestation

import (
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The Apgar score (Apgar 1953) rates a newborn on five signs, 0 to 2 each,
// at one and five minutes. A total of 7 to 10 is reassuring. When the
// five-minute score is below 7, it is taken again every five minutes up to
// 20 minutes (AAP and ACOG). Kyvernetria scores a launch the same way; the
// criteria below are its own, and so is what it does with a low score:
// in medicine the score describes, and does not decide treatment.
const (
	// Reassuring is the lowest reassuring total.
	Reassuring = 7
	// LastApgarMinute is the latest repeat score.
	LastApgarMinute = 20
)

// Apgar is one score.
type Apgar struct {
	Minute      int32       `json:"minute"`
	Time        metav1.Time `json:"time"`
	Appearance  int32       `json:"appearance"`
	Pulse       int32       `json:"pulse"`
	Grimace     int32       `json:"grimace"`
	Activity    int32       `json:"activity"`
	Respiration int32       `json:"respiration"`
	Total       int32       `json:"total"`
	// Notes explain each sign, in the order above.
	Notes []string `json:"notes,omitempty"`
}

// Signs is what the controller observed of the newborn's pods.
type Signs struct {
	// Desired and Ready replicas of the Deployment.
	Desired, Ready int32
	// Running pods (all containers running), for services nobody calls.
	Running int32
	// Restarts of the newborn's containers; CrashLooping when any waits in
	// CrashLoopBackOff; LivenessFailures counts "Liveness probe failed".
	Restarts         int32
	CrashLooping     bool
	LivenessFailures int32
	// Warnings counts other Warning events about the newborn (each event's
	// count included). Readiness probe failures are left to Appearance,
	// and liveness failures and back-offs to Pulse.
	Warnings int32
	// Services selecting the pods, and ready endpoints of the newborn's
	// pods among them.
	Services      int32
	ReadyEndpoint int32
	// OOMKills of the newborn's containers, and its evicted pods.
	OOMKills, Evictions int32
}

// Score computes the Apgar score at minute from the signs.
func Score(minute int32, s Signs) Apgar {
	a := Apgar{Minute: minute}
	var note string

	// Appearance: how many replicas are ready.
	switch {
	case s.Desired > 0 && s.Ready >= s.Desired:
		a.Appearance = 2
	case s.Ready > 0 && 2*s.Ready >= s.Desired:
		a.Appearance = 1
	}
	a.Notes = append(a.Notes, fmt.Sprintf("ready %d/%d", s.Ready, s.Desired))

	// Pulse: restarts and liveness failures.
	switch {
	case s.CrashLooping || s.Restarts > s.Desired:
		a.Pulse = 0
	case s.Restarts > 0 || s.LivenessFailures > 0:
		a.Pulse = 1
	default:
		a.Pulse = 2
	}
	note = fmt.Sprintf("%s, %s", plural(s.Restarts, "restart"), plural(s.LivenessFailures, "liveness failure"))
	if s.CrashLooping {
		note += ", crash-looping"
	}
	a.Notes = append(a.Notes, note)

	// Grimace: distress the cluster reported.
	switch {
	case s.Warnings == 0:
		a.Grimace = 2
	case s.Warnings <= 5:
		a.Grimace = 1
	}
	a.Notes = append(a.Notes, plural(s.Warnings, "warning event"))

	// Activity: serving. With no metrics source, ready endpoints stand in
	// for traffic; a service nobody calls is judged by its running pods.
	have := s.ReadyEndpoint
	if s.Services == 0 {
		have = s.Running
		note = fmt.Sprintf("no Service selects it; %d/%d pods running", s.Running, s.Desired)
	} else {
		note = fmt.Sprintf("%s serving behind %s", plural(s.ReadyEndpoint, "ready endpoint"), plural(s.Services, "Service"))
	}
	switch {
	case s.Desired > 0 && have >= s.Desired:
		a.Activity = 2
	case have > 0:
		a.Activity = 1
	}
	a.Notes = append(a.Notes, note)

	// Respiration: room to breathe. Without a metrics source there is no
	// throttling signal, so OOM kills and evictions only.
	switch {
	case s.OOMKills >= 2:
		a.Respiration = 0
	case s.OOMKills == 1 || s.Evictions > 0:
		a.Respiration = 1
	default:
		a.Respiration = 2
	}
	a.Notes = append(a.Notes, fmt.Sprintf("%s, %s", plural(s.OOMKills, "OOM kill"), plural(s.Evictions, "eviction")))

	a.Total = a.Appearance + a.Pulse + a.Grimace + a.Activity + a.Respiration
	return a
}

// NextMinute returns the minute of the next score, or 0 when scoring is
// over: 1, then 5, then every 5 minutes up to 20 while the last score is
// below Reassuring.
func NextMinute(scores []Apgar) int32 {
	if len(scores) == 0 {
		return 1
	}
	last := scores[len(scores)-1]
	switch {
	case last.Minute < 5:
		return 5
	case last.Total < Reassuring && last.Minute < LastApgarMinute:
		return last.Minute + 5
	}
	return 0
}

// Rollback decides what a score means for the launch. It rolls back only
// on the five-minute score, only when it is below Reassuring, only when
// rollback is enabled, and only to a revision that exists.
func Rollback(score Apgar, enabled bool, previousRevision string) (bool, string) {
	if score.Minute != 5 || score.Total >= Reassuring {
		return false, ""
	}
	if !enabled {
		return false, "automatic rollback is off; scoring again every five minutes up to 20"
	}
	if previousRevision == "" {
		return false, "there is no earlier revision to return to; it needs hands now"
	}
	return true, fmt.Sprintf("returning to revision %s", previousRevision)
}

// Weakest names the signs that scored below 2, for messages.
func (a Apgar) Weakest() string {
	names := []string{"appearance", "pulse", "grimace", "activity", "respiration"}
	values := []int32{a.Appearance, a.Pulse, a.Grimace, a.Activity, a.Respiration}
	var parts []string
	for i, v := range values {
		if v < 2 {
			p := fmt.Sprintf("%s %d", names[i], v)
			if i < len(a.Notes) {
				p += ": " + a.Notes[i]
			}
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, "; ")
}

// MinuteWords spells a score's minute for messages.
func MinuteWords(m int32) string {
	switch m {
	case 1:
		return "one minute"
	case 5:
		return "five minutes"
	}
	return fmt.Sprintf("%d minutes", m)
}

func plural(n int32, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	if n == 0 {
		return "no " + noun + "s"
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
