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

// Package kyvernetria holds the constants shared by every Kyvernetria
// component. Each constant points at the research finding it models; see
// docs/RESEARCH.md in the Kyvernetria repository for citations.
package kyvernetria

import "time"

const (
	// Prefix is the annotation and label prefix owned by the distribution.
	Prefix = "kyvernetria.io/"

	// SelfLabel marks a namespace as "self": the immunity admission plugin
	// tolerates it (immunological self-tolerance).
	SelfLabel = Prefix + "self"

	// ExcludedAnnotation removes a pod from Service endpoints without
	// killing it (indirect rather than physical aggression).
	ExcludedAnnotation = Prefix + "excluded"

	// DiscussedAnnotation allows an immediate (grace period 0) delete once
	// someone has explicitly agreed to it.
	DiscussedAnnotation = Prefix + "discussed"

	// RememberedNodesAnnotation lists nodes a workload has lived on, most
	// recent first (object-location memory).
	RememberedNodesAnnotation = Prefix + "remembered-nodes"

	// WorriedAnnotation lists the resources the worry controller is
	// currently worried about on a node ("cpu,memory"), so a new leader
	// knows what was already said.
	WorriedAnnotation = Prefix + "worried"

	// MaxRememberedNodes bounds the remembered-nodes list.
	MaxRememberedNodes = 16

	// DefaultTerminationGracePeriodSeconds replaces upstream's 30s: pods get
	// time to finish instead of being cut off (higher agreeableness).
	DefaultTerminationGracePeriodSeconds = 300

	// EventTTL replaces upstream's 1h (episodic memory).
	EventTTL = 30 * 24 * time.Hour

	// WorryThresholdPercent is where the worry controller starts warning
	// about node requests; upstream says nothing until pods stop fitting
	// (higher neuroticism: earlier, more sensitive alarms).
	WorryThresholdPercent = 70
)
