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
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"k8s.io/kubernetes/pkg/kyvernetria"
)

// careTier is one step of the newborn's priority bump.
type careTier struct {
	class       string
	priority    int32
	from, until time.Duration
}

// careTiers taper the protection. A newborn is protected at first by
// antibodies it got from its mother, which it does not make itself and
// which wane over its first months. Kyvernetria compresses months into
// hours: the bump halves every 24 hours and ends at 72. The values sit
// above ordinary pods (priority 0) and far below anything a cluster
// marks as critical. All of these numbers are engineering choices.
var careTiers = []careTier{
	{class: "kyvernetria-newborn-1", priority: 1000, from: 0, until: 24 * time.Hour},
	{class: "kyvernetria-newborn-2", priority: 500, from: 24 * time.Hour, until: 48 * time.Hour},
	{class: "kyvernetria-newborn-3", priority: 250, from: 48 * time.Hour, until: kyvernetria.NewbornCareDuration},
}

// CareTier returns the tier (1 to 3) and priority class for a service
// born sinceBirth ago, or 0 and "" once the care period is over.
func CareTier(sinceBirth time.Duration) (int32, string) {
	for i, t := range careTiers {
		if sinceBirth < t.until {
			return int32(i + 1), t.class
		}
	}
	return 0, ""
}

// LastTierClass is the class kept while a grown service waits for its
// caregivers: the gentlest step, held rather than dropped.
func LastTierClass() (int32, string) {
	return int32(len(careTiers)), careTiers[len(careTiers)-1].class
}

// IsNewbornClass reports whether a priority class is one of the tiers.
func IsNewbornClass(name string) bool {
	for _, t := range careTiers {
		if t.class == name {
			return true
		}
	}
	return false
}

// CaregiversOf reads the two caregivers from a Deployment's annotations.
// ok is true only when both are named and are different people; otherwise
// missing says, in a sentence, what is missing.
func CaregiversOf(annotations map[string]string) (c Caregivers, ok bool, missing string) {
	c.Primary = strings.TrimSpace(annotations[kyvernetria.PrimaryCaregiverAnnotation])
	c.Secondary = strings.TrimSpace(annotations[kyvernetria.SecondaryCaregiverAnnotation])
	switch {
	case c.Primary == "" && c.Secondary == "":
		return c, false, "nobody is named yet"
	case c.Primary == "":
		return c, false, "the primary caregiver is not named yet"
	case c.Secondary == "":
		return c, false, "the secondary caregiver is not named yet"
	case strings.EqualFold(c.Primary, c.Secondary):
		return c, false, fmt.Sprintf("%s is named twice", c.Primary)
	}
	return c, true, ""
}

// NewbornEntry is one newborn workload in a namespace: the pods its
// selector matches get the priority class when they are created.
type NewbornEntry struct {
	Selector      string `json:"selector"`
	PriorityClass string `json:"priorityClass"`
}

// EncodeNewborns renders the namespace annotation; "" for none.
func EncodeNewborns(entries []NewbornEntry) string {
	if len(entries) == 0 {
		return ""
	}
	sorted := append([]NewbornEntry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Selector < sorted[j].Selector })
	raw, _ := json.Marshal(sorted) // cannot fail for plain strings
	return string(raw)
}

// DecodeNewborns reads the namespace annotation.
func DecodeNewborns(raw string) ([]NewbornEntry, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var entries []NewbornEntry
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return nil, fmt.Errorf("%s: %w", kyvernetria.NewbornCareAnnotation, err)
	}
	return entries, nil
}
