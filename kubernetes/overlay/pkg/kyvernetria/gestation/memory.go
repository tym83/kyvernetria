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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
)

// Microchimerism: cells from a pregnancy can stay in the mother for
// decades (Bianchi et al. 1996). The cluster keeps a little of every
// service it gave birth to: a small, permanent record, written at birth,
// kept up to date while the service lives and closed when it leaves.
const (
	// MemoryConfigMap holds the records, in kyvernetria.SystemNamespace.
	MemoryConfigMap = "kyvernetria-microchimerism"
	// MaxRecords bounds the memory. When it is full, the record of the
	// service that left longest ago is let go first.
	MaxRecords = 256
	// maxListed bounds the owners and dependencies kept per record.
	maxListed = 16
	// maxField bounds every string in a record.
	maxField = 253
)

// Record is what the cluster keeps of a service.
type Record struct {
	Namespace    string   `json:"namespace"`
	Name         string   `json:"name"`
	UID          string   `json:"uid"`
	Born         string   `json:"born"`
	Left         string   `json:"left,omitempty"`
	Owners       []string `json:"owners,omitempty"`
	Dependencies []string `json:"dependencies,omitempty"`
	ConfigDigest string   `json:"configDigest,omitempty"`
}

// Key is the record's ConfigMap key: namespace.name.<uid prefix>, so a
// service that comes back under the same name is a new life.
func (r Record) Key() string {
	uid := r.UID
	if len(uid) > 8 {
		uid = uid[:8]
	}
	return r.Namespace + "." + r.Name + "." + uid
}

// Departed reports whether the service has left.
func (r Record) Departed() bool { return r.Left != "" }

// Bounded returns the record trimmed to its size limits.
func (r Record) Bounded() Record {
	trim := func(s string) string {
		if len(s) > maxField {
			return s[:maxField]
		}
		return s
	}
	list := func(in []string) []string {
		out := make([]string, 0, len(in))
		seen := map[string]bool{}
		for _, s := range in {
			s = trim(strings.TrimSpace(s))
			if s != "" && !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
		sort.Strings(out)
		if len(out) > maxListed {
			out = out[:maxListed]
		}
		if len(out) == 0 {
			return nil
		}
		return out
	}
	r.Namespace, r.Name, r.UID = trim(r.Namespace), trim(r.Name), trim(r.UID)
	r.Owners, r.Dependencies = list(r.Owners), list(r.Dependencies)
	return r
}

// Records reads the memory, oldest birth first.
func Records(data map[string]string) []Record {
	out := make([]Record, 0, len(data))
	for _, raw := range data {
		var r Record
		if json.Unmarshal([]byte(raw), &r) == nil && r.Name != "" {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Born != out[j].Born {
			return out[i].Born < out[j].Born
		}
		return out[i].Key() < out[j].Key()
	})
	return out
}

// Put stores a record and keeps the memory within MaxRecords. It reports
// whether data changed.
func Put(data map[string]string, r Record) bool {
	r = r.Bounded()
	raw, _ := json.Marshal(r) // plain strings cannot fail
	key := r.Key()
	if data[key] == string(raw) {
		return false
	}
	data[key] = string(raw)
	prune(data, key)
	return true
}

// prune lets go of the oldest departures first, then the oldest births,
// never the record just written.
func prune(data map[string]string, keep string) {
	if len(data) <= MaxRecords {
		return
	}
	records := Records(data)
	sort.SliceStable(records, func(i, j int) bool {
		a, b := records[i], records[j]
		if a.Departed() != b.Departed() {
			return a.Departed()
		}
		if a.Departed() {
			return a.Left < b.Left
		}
		return a.Born < b.Born
	})
	for _, r := range records {
		if len(data) <= MaxRecords {
			return
		}
		if r.Key() != keep {
			delete(data, r.Key())
		}
	}
}

// Stamp formats a time for a record.
func Stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// ConfigDigest identifies a pod template by content: the first 12 hex
// digits of the SHA-256 of its JSON.
func ConfigDigest(template *v1.PodTemplateSpec) string {
	raw, err := json.Marshal(template)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])[:12]
}

// Remembrance is the line kyvctl remember says.
const Remembrance = "You were here once. I keep a little of you."
