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

// Package humanize turns terse component messages into sentences addressed
// to a person.
//
// Models: more social, person-directed word use (pronouns, "we", "you"),
// a small effect (Newman et al. 2008). It deliberately does not make
// messages longer than necessary: women and men speak a similar number of
// words per day (Mehl et al. 2007; a larger 2025 replication found no
// conclusive overall difference). The original upstream text is kept in
// parentheses so that nothing grepping for it breaks.
package humanize

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

type phrase struct {
	prefix   string
	singular string
	plural   string
}

// Ordered from the most specific prefix to the least.
var phrases = []phrase{
	{"Insufficient cpu", "is short on CPU", "are short on CPU"},
	{"Insufficient memory", "is short on memory", "are short on memory"},
	{"Insufficient ephemeral-storage", "is short on local disk", "are short on local disk"},
	{"Insufficient ", "is short on a resource the pod asked for", "are short on a resource the pod asked for"},
	{"Too many pods", "already runs as many pods as it can", "already run as many pods as they can"},
	{"node(s) had untolerated taint", "has a taint the pod doesn't tolerate", "have a taint the pod doesn't tolerate"},
	{"node(s) didn't match Pod's node affinity/selector", "isn't one the pod asked for", "aren't ones the pod asked for"},
	{"node(s) didn't have free ports", "has the requested host port taken", "have the requested host port taken"},
	{"node(s) were unschedulable", "is cordoned", "are cordoned"},
	{"node(s) didn't match pod anti-affinity rules", "already runs a pod this one wants to keep away from", "already run a pod this one wants to keep away from"},
	{"node(s) didn't match pod affinity rules", "doesn't run the pods this one wants to be near", "don't run the pods this one wants to be near"},
	{"node(s) had volume node affinity conflict", "can't reach the pod's volume", "can't reach the pod's volume"},
	{"node(s) didn't satisfy plugin(s)", "was turned down by a scheduling plugin", "were turned down by a scheduling plugin"},
}

// Reason renders one upstream scheduling reason seen on count nodes.
func Reason(reason string, count, total int) string {
	for _, p := range phrases {
		if strings.HasPrefix(reason, p.prefix) {
			switch {
			case count == total && total > 1:
				return "every node " + p.singular
			case count == 1:
				return "1 node " + p.singular
			default:
				return fmt.Sprintf("%d nodes %s", count, p.plural)
			}
		}
	}
	if count == 1 {
		return fmt.Sprintf("1 node: %s", reason)
	}
	return fmt.Sprintf("%d nodes: %s", count, reason)
}

// SchedulingFailure renders a FitError for a person in at most limit
// bytes. The upstream text is always kept whole, in parentheses at the end,
// so that anything grepping for it still matches; when the message is too
// long, the human part is shortened instead. If even the upstream text
// doesn't fit, it is returned alone for the caller to truncate as
// upstream does.
func SchedulingFailure(pod string, totalNodes int, reasons map[string]int, upstream string, limit int) string {
	var human string
	if totalNodes == 0 {
		human = fmt.Sprintf("We couldn't place %s yet: the cluster has no nodes we can use.", pod)
	} else {
		keys := make([]string, 0, len(reasons))
		for k := range reasons {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			if reasons[keys[i]] != reasons[keys[j]] {
				return reasons[keys[i]] > reasons[keys[j]]
			}
			return keys[i] < keys[j]
		})
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, Reason(k, reasons[k], totalNodes))
		}
		why := strings.Join(parts, ", ")
		if why == "" {
			why = "no node fits"
		}
		human = fmt.Sprintf("We couldn't place %s yet: %s. We'll try again as soon as something changes.", pod, why)
	}
	return withUpstream(human, upstream, limit)
}

// minHuman is the shortest human part worth keeping in front of the
// upstream text.
const minHuman = 40

func withUpstream(human, upstream string, limit int) string {
	suffix := " (" + upstream + ")"
	if len(human)+len(suffix) <= limit {
		return human + suffix
	}
	const ellipsis = "..."
	room := limit - len(suffix) - len(ellipsis)
	if room < minHuman {
		return upstream
	}
	cut := room
	for cut > 0 && !utf8.RuneStart(human[cut]) {
		cut--
	}
	return strings.TrimRight(human[:cut], " ,") + ellipsis + suffix
}
