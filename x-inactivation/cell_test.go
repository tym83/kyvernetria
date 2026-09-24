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
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newCell(t *testing.T, pick string) *Cell {
	t.Helper()
	now := time.Unix(1_800_000_000, 0)
	return &Cell{
		Dir: t.TempDir(), EscapeStarts: 3, EscapeWindow: time.Minute,
		Now:    func() time.Time { return now },
		Random: func() (string, error) { return pick, nil },
	}
}

func TestInactivationIsClonal(t *testing.T) {
	c := newCell(t, Xp)
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

func TestEscapeAfterCrashLoop(t *testing.T) {
	c := newCell(t, Xm)
	now := time.Unix(1_800_000_000, 0)
	c.Now = func() time.Time { return now }
	if _, _, err := c.Expressed(); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 2; i++ {
		if looping, _ := c.RecordStart("kube-apiserver"); looping {
			t.Fatalf("start %d counted as a crash loop", i)
		}
		now = now.Add(10 * time.Second)
	}
	looping, err := c.RecordStart("kube-apiserver")
	if err != nil || !looping {
		t.Fatalf("third start in a minute not a crash loop: %v %v", looping, err)
	}
	to, err := c.Escape(Xm)
	if err != nil || to != Xp {
		t.Fatalf("escape: %s %v", to, err)
	}
	if a, _, _ := c.Expressed(); a != Xp {
		t.Fatalf("node still expresses %s after escape", a)
	}
	if b, _ := os.ReadFile(filepath.Join(c.Dir, "escapes")); !strings.Contains(string(b), "escaped Xm -> Xp") {
		t.Errorf("escape not logged: %q", b)
	}
	if looping, _ := c.RecordStart("kube-apiserver"); looping {
		t.Error("history was not reset after the escape")
	}
}

func TestSlowRestartsAreNotACrashLoop(t *testing.T) {
	c := newCell(t, Xm)
	now := time.Unix(1_800_000_000, 0)
	c.Now = func() time.Time { return now }
	for i := 0; i < 10; i++ {
		if looping, _ := c.RecordStart("kube-scheduler"); looping {
			t.Fatalf("restart %d, 40s apart, counted as a crash loop", i)
		}
		now = now.Add(40 * time.Second)
	}
}

// TestWrapperExecsExpressedAllele builds the wrapper, installs it under a
// component name next to two fake alleles, and checks which one runs.
func TestWrapperExecsExpressedAllele(t *testing.T) {
	bin := t.TempDir()
	wrapper := filepath.Join(bin, "kube-apiserver")
	if out, err := exec.Command("go", "build", "-o", wrapper, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	for _, a := range []string{Xm, Xp} {
		script := "#!/bin/sh\necho allele " + a + " args \"$@\"\n"
		if err := os.WriteFile(filepath.Join(bin, "kube-apiserver."+a), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	state := t.TempDir()
	if err := os.WriteFile(filepath.Join(state, "x-inactivation"), []byte("Xp\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Started by bare name through PATH, the way kubeadm static pods do it.
	cmd := exec.Command("kube-apiserver", "--secure-port=6443")
	cmd.Path, cmd.Err = wrapper, nil
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), "KYVERNETRIA_STATE_DIR="+state, "PATH="+bin+":"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "allele Xp args --secure-port=6443") {
		t.Errorf("wrong allele or args:\n%s", out)
	}
}
