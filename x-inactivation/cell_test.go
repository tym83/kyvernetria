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

package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// clock is a settable fake clock.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newCell(t *testing.T, pick string) (*Cell, *clock) {
	t.Helper()
	clk := &clock{now: time.Unix(1_800_000_000, 0)}
	return &Cell{
		Dir: t.TempDir(), EscapeCrashes: 5, EscapeWindow: 10 * time.Minute, Cooldown: 24 * time.Hour,
		Now:    clk.Now,
		Random: func() (string, error) { return pick, nil },
		BootID: func() string { return "boot-1" },
	}, clk
}

func mustExpress(t *testing.T, c *Cell, want string) {
	t.Helper()
	a, _, err := c.Expressed()
	if err != nil || a != want {
		t.Fatalf("node expresses %q (%v), want %s", a, err, want)
	}
}

func crash(t *testing.T, c *Cell, allele string) Decision {
	t.Helper()
	d, err := c.RecordCrash(Mosaic, allele)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestInactivationIsClonal(t *testing.T) {
	c, _ := newCell(t, Xp)
	a, first, err := c.Expressed()
	if err != nil || a != Xp || !first {
		t.Fatalf("first boot: %s %v %v", a, first, err)
	}
	if info, err := os.Stat(filepath.Join(c.Dir, "x-inactivation")); err != nil || info.Mode().Perm() != 0o644 {
		t.Errorf("choice file is not readable by everyone: %v %v", info, err)
	}
	c.Random = func() (string, error) { return Xm, nil }
	a, first, err = c.Expressed()
	if err != nil || a != Xp || first {
		t.Fatalf("the choice was not inherited: %s %v %v", a, first, err)
	}
	leftovers, _ := filepath.Glob(filepath.Join(c.Dir, ".tmp-*"))
	if len(leftovers) != 0 {
		t.Errorf("temporary files left behind: %v", leftovers)
	}
}

func TestConcurrentFirstBootAgrees(t *testing.T) {
	dir := t.TempDir()
	var wg sync.WaitGroup
	results := make([]string, 16)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			pick := Xm
			if i%2 == 1 {
				pick = Xp
			}
			c := &Cell{Dir: dir, Now: time.Now, Random: func() (string, error) { return pick, nil }}
			a, _, err := c.Expressed()
			if err != nil {
				t.Error(err)
			}
			results[i] = a
		}(i)
	}
	wg.Wait()
	for _, r := range results {
		if r != results[0] {
			t.Fatalf("components on one node disagree: %v", results)
		}
	}
}

func TestRandomAlleleProducesBoth(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200 && len(seen) < 2; i++ {
		a, err := RandomAllele()
		if err != nil {
			t.Fatal(err)
		}
		seen[a] = true
	}
	if !seen[Xm] || !seen[Xp] {
		t.Fatalf("200 draws produced only %v", seen)
	}
}

// (a) Only a node without state chooses; anything else fails loudly.
func TestBrokenStateIsAnErrorNotXm(t *testing.T) {
	for name, setup := range map[string]func(path string) error{
		"zero-length": func(p string) error { return os.WriteFile(p, nil, 0o644) },
		"garbage":     func(p string) error { return os.WriteFile(p, []byte("Xq\n"), 0o644) },
		"unreadable":  func(p string) error { return os.Mkdir(p, 0o755) }, // read(2) on a directory fails
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := newCell(t, Xm)
			if err := setup(c.choicePath()); err != nil {
				t.Fatal(err)
			}
			a, first, err := c.Expressed()
			if err == nil || a != "" || first {
				t.Fatalf("broken state gave %q first=%v err=%v, want an error", a, first, err)
			}
		})
	}
}

func TestEscapeAfterCrashLoop(t *testing.T) {
	c, clk := newCell(t, Xm)
	mustExpress(t, c, Xm)
	// A history left by an older release is cleared by the escape too.
	if err := os.WriteFile(filepath.Join(c.Dir, "kube-scheduler.starts"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 4; i++ {
		if d := crash(t, c, Xm); d.Escaped || d.Crashes != i {
			t.Fatalf("crash %d: %+v", i, d)
		}
		clk.Add(time.Minute)
	}
	d := crash(t, c, Xm)
	if !d.Escaped || d.To != Xp {
		t.Fatalf("fifth crash in ten minutes did not escape: %+v", d)
	}
	mustExpress(t, c, Xp)
	if b, _ := os.ReadFile(filepath.Join(c.Dir, "escapes")); !strings.Contains(string(b), "kube-apiserver escaped Xm -> Xp") {
		t.Errorf("escape not logged: %q", b)
	}
	for _, pattern := range []string{"*.crashes", "*.starts"} {
		if m, _ := filepath.Glob(filepath.Join(c.Dir, pattern)); len(m) != 0 {
			t.Errorf("histories not cleared after the escape: %v", m)
		}
	}
	if b, _ := os.ReadFile(c.lastEscapePath()); string(b) != "1800000240 boot-1\n" {
		t.Errorf("last escape recorded as %q", b)
	}
}

func TestSlowCrashesAreNotACrashLoop(t *testing.T) {
	c, clk := newCell(t, Xm)
	mustExpress(t, c, Xm)
	for i := 0; i < 20; i++ {
		if d := crash(t, c, Xm); d.Escaped {
			t.Fatalf("crash %d, three minutes apart, escaped: %+v", i, d)
		}
		clk.Add(3 * time.Minute)
	}
}

// (d) A clock stepped back must not make old crashes count forever.
func TestFutureHistoryIsIgnored(t *testing.T) {
	c, clk := newCell(t, Xm)
	mustExpress(t, c, Xm)
	future := clk.Now().Add(time.Hour).Unix()
	var lines []string
	for i := 0; i < 10; i++ {
		lines = append(lines, strconv.FormatInt(future+int64(i), 10))
	}
	if err := os.WriteFile(filepath.Join(c.Dir, Mosaic+".crashes"), []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	if d := crash(t, c, Xm); d.Escaped || d.Crashes != 1 {
		t.Fatalf("crashes from the future were counted: %+v", d)
	}
	// Nor does an escape "in the future" start a cooldown that never ends,
	// but the one-escape-per-boot rule still applies.
	if err := os.WriteFile(c.lastEscapePath(), []byte(strconv.FormatInt(future, 10)+" boot-0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if reason := c.escapeBlocked(clk.Now()); reason != "" {
		t.Errorf("an escape stamped in the future blocks escapes: %s", reason)
	}
}

// (c) Concurrent crash reports are serialised: one escape, no lost crashes.
func TestConcurrentCrashesEscapeOnce(t *testing.T) {
	c, _ := newCell(t, Xm)
	mustExpress(t, c, Xm)
	var wg sync.WaitGroup
	var mu sync.Mutex
	escapes := 0
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, err := c.RecordCrash(Mosaic, Xm)
			if err != nil {
				t.Error(err)
			}
			if d.Escaped {
				mu.Lock()
				escapes++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if escapes != 1 {
		t.Fatalf("%d escapes from five concurrent crashes, want 1", escapes)
	}
	mustExpress(t, c, Xp)
}

func TestCooldownAndOneEscapePerBoot(t *testing.T) {
	c, clk := newCell(t, Xm)
	mustExpress(t, c, Xm)
	loop := func(allele string) Decision {
		var d Decision
		for i := 0; i < 5; i++ {
			d = crash(t, c, allele)
			clk.Add(10 * time.Second)
			if d.Escaped {
				break
			}
		}
		return d
	}
	if d := loop(Xm); !d.Escaped {
		t.Fatalf("first crash loop did not escape: %+v", d)
	}
	if d := loop(Xp); d.Escaped || !strings.Contains(d.Reason, "since it booted") {
		t.Fatalf("second escape in the same boot: %+v", d)
	}
	clk.Add(25 * time.Hour)
	if d := loop(Xp); d.Escaped {
		t.Fatalf("escaped twice in one boot, a day apart: %+v", d)
	}
	c.BootID = func() string { return "boot-2" }
	clk.Add(-24 * time.Hour) // one hour after the escape, on a new boot
	if d := loop(Xp); d.Escaped || !strings.Contains(d.Reason, "cooling down") {
		t.Fatalf("escaped during the cooldown: %+v", d)
	}
	clk.Add(24 * time.Hour)
	if d := loop(Xp); !d.Escaped || d.To != Xm {
		t.Fatalf("no escape after the cooldown on a new boot: %+v", d)
	}
}

func TestUpgradingFlagBlocksEscapes(t *testing.T) {
	c, _ := newCell(t, Xm)
	mustExpress(t, c, Xm)
	if err := os.WriteFile(filepath.Join(c.Dir, UpgradingFlag), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var d Decision
	for i := 0; i < 10; i++ {
		d = crash(t, c, Xm)
	}
	if d.Escaped || !strings.Contains(d.Reason, UpgradingFlag) {
		t.Fatalf("escaped during an upgrade: %+v", d)
	}
	mustExpress(t, c, Xm)
}

// (b) Atomic writes replace the file whole and leave nothing behind.
func TestWriteAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	for _, s := range []string{"one\n", "two\n"} {
		if err := writeAtomic(path, []byte(s)); err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(path); string(b) != s {
			t.Fatalf("got %q, want %q", b, s)
		}
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o644 {
		t.Errorf("mode %v", info.Mode())
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("temporary files left behind: %v", entries)
	}
}
