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
	"math"
	"sort"
	"time"
)

// Growth charts. The WHO Child Growth Standards draw percentile curves
// (3rd, 15th, 50th, 85th, 97th) and judge a child by the line it follows
// over time, not by one measurement. A service has no population that
// "should" grow alike, so Kyvernetria draws the bands from the service's
// own history, and adds the one comparison a cluster cares about: against
// what it requests.
const (
	// MinSamples is how many measurements a chart needs.
	MinSamples = 6
	// MaxSamples bounds the trajectory kept in status: two weeks of hourly
	// measurements.
	MaxSamples = 14 * 24
	// SampleInterval is how often a grown service is measured, and
	// NewbornSampleInterval how often during newborn care.
	SampleInterval        = time.Hour
	NewbornSampleInterval = 10 * time.Minute
)

// Bands are percentiles of a service's own history.
type Bands struct {
	P3, P15, P50, P85, P97 int64
}

// Assessment is one resource's growth.
type Assessment struct {
	Resource string // "cpu" or "memory"
	Samples  int
	Current  int64 // millicores or bytes
	Request  int64 // per pod; 0 when it requests nothing
	Bands    Bands
	Hint     string
}

// AppendSample adds a measurement, keeping at most MaxSamples.
func AppendSample(samples []Sample, s Sample) []Sample {
	samples = append(samples, s)
	if len(samples) > MaxSamples {
		samples = append([]Sample(nil), samples[len(samples)-MaxSamples:]...)
	}
	return samples
}

// SampleDue reports whether a new measurement is due.
func SampleDue(samples []Sample, newborn bool, now time.Time) bool {
	if len(samples) == 0 {
		return true
	}
	interval := SampleInterval
	if newborn {
		interval = NewbornSampleInterval
	}
	return !now.Before(samples[len(samples)-1].Time.Add(interval))
}

// AssessGrowth charts CPU and memory. requestCPU (millicores) and
// requestMemory (bytes) are per pod; current, when not nil, is a live
// measurement newer than the history.
func AssessGrowth(samples []Sample, requestCPU, requestMemory int64, current *Sample) []Assessment {
	if current == nil && len(samples) > 0 {
		// The latest measurement is the one judged against the rest.
		current = &samples[len(samples)-1]
		samples = samples[:len(samples)-1]
	}
	cpu := make([]int64, 0, len(samples))
	mem := make([]int64, 0, len(samples))
	for _, s := range samples {
		cpu = append(cpu, s.CPUMilli)
		mem = append(mem, s.MemoryBytes)
	}
	var curCPU, curMem int64
	if current != nil {
		curCPU, curMem = current.CPUMilli, current.MemoryBytes
	}
	return []Assessment{
		assess("cpu", cpu, curCPU, requestCPU),
		assess("memory", mem, curMem, requestMemory),
	}
}

func assess(res string, history []int64, current, request int64) Assessment {
	a := Assessment{Resource: res, Samples: len(history), Current: current, Request: request}
	sorted := append([]int64(nil), history...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	a.Bands = Bands{
		P3: percentile(sorted, 3), P15: percentile(sorted, 15), P50: percentile(sorted, 50),
		P85: percentile(sorted, 85), P97: percentile(sorted, 97),
	}
	enough := len(history) >= MinSamples
	switch {
	case request > 0 && current >= request:
		a.Hint = fmt.Sprintf("outgrowing its requests: it uses %d%% of what it asks for. Raise the request before it is squeezed", percentOf(current, request))
	case enough && request > 0 && 10*a.Bands.P85 >= 9*request:
		a.Hint = "outgrowing its requests: most of the time it uses more than 90% of what it asks for"
	case !enough:
		a.Hint = fmt.Sprintf("too early to chart: %d of %d measurements so far", len(history), MinSamples)
	case current < a.Bands.P3:
		a.Hint = "falling behind its own curve — check: it uses less than almost ever before. Is it still getting work?"
	case current > a.Bands.P97:
		a.Hint = "above its own curve: it is growing faster than it ever has"
	case request > 0 && 4*a.Bands.P97 <= request:
		a.Hint = "plenty of room: it has never used more than a quarter of its request"
	default:
		a.Hint = "on its curve"
	}
	return a
}

// percentile is the nearest-rank percentile of sorted values.
func percentile(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(p / 100 * float64(len(sorted))))
	if rank < 1 {
		rank = 1
	}
	return sorted[rank-1]
}

func percentOf(v, of int64) int64 {
	if of == 0 {
		return 0
	}
	return v * 100 / of
}
