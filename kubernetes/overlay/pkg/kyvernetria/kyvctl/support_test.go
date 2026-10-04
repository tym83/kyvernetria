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
)

func TestAssessMinor(t *testing.T) {
	for _, tc := range []struct {
		minor uint
		on    string
		want  Stage
		until string
	}{
		{36, "2026-10-04", Supported, "2027-07-28"},
		{36, "2027-04-27", Supported, "2027-07-28"},
		{36, "2027-04-28", Maintenance, "2027-07-28"},
		{36, "2027-06-27", Maintenance, "2027-07-28"},
		{36, "2027-06-28", Grace, "2027-07-28"},
		{36, "2027-07-27", Grace, "2027-07-28"},
		{36, "2027-07-28", Unsupported, "2027-07-28"},
		{33, "2026-07-01", Grace, "2026-07-28"}, // no maintenance date recorded
		{33, "2026-10-04", Unsupported, "2026-07-28"},
		{20, "2026-10-04", Unsupported, "-"}, // older than the table
		{99, "2026-10-04", Unknown, "-"},
	} {
		s := AssessMinor(tc.minor, day(tc.on))
		if s.Stage != tc.want || date(s.Until) != tc.until {
			t.Errorf("1.%d on %s: %s until %s, want %s until %s", tc.minor, tc.on, s.Stage, date(s.Until), tc.want, tc.until)
		}
	}
}

func TestSupportGraceIsAboutSevenPercent(t *testing.T) {
	// The grace models the female-to-male life-expectancy ratio (WHO 2019:
	// 75.7 / 70.6 = 1.072) applied to upstream's window, release to EOL.
	window := day("2026-10-27").Sub(day("2025-08-27")) // 1.34
	got := float64(AssessMinor(34, day("2026-01-01")).Until.Sub(day("2026-10-27"))) / float64(window)
	if got < 0.06 || got > 0.08 {
		t.Errorf("grace is %.3f of the window, want about 0.072", got)
	}
}

func TestRenderSupport(t *testing.T) {
	used := map[uint]*usage{37: {apiservers: 2}, 36: {apiservers: 1, kubelets: 4}}
	var out bytes.Buffer
	if err := renderSupport(&out, used, 0, day("2026-10-04")); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"1.37   2 apiservers",
		"1.36   1 apiserver, 4 kubelets",
		"2027-06-28     2027-07-28          supported",
		"supported until 2027-07-28, bounded by 1.36",
		"1.37 alone would be supported until 2027-11-27",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in:\n%s", want, out.String())
		}
	}

	out.Reset()
	if err := renderSupport(&out, used, 1, day("2027-07-01")); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"1 apiserver didn't answer", "upgrade grace, no fixes", "not even security bugs", "builds and tests it", "Upgrade now"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("in grace: missing %q in:\n%s", want, out.String())
		}
	}

	out.Reset()
	err := renderSupport(&out, used, 0, day("2027-08-01"))
	if err == nil || !strings.Contains(err.Error(), "1.36, out of support") {
		t.Errorf("past grace: err = %v, want 1.36 out of support", err)
	}

	out.Reset()
	if err := renderSupport(&out, map[uint]*usage{99: {kubelets: 1}}, 0, day("2026-10-04")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "newer than this kyvctl knows") {
		t.Errorf("unknown minor: %s", out.String())
	}
}
