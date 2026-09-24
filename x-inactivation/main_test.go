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
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var wrapperBinary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "x-inactivation-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	wrapperBinary = filepath.Join(dir, "x-inactivation")
	if out, err := exec.Command("go", "build", "-o", wrapperBinary, ".").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build: %v\n%s", err, out)
		os.Exit(1)
	}
	logOutput = io.Discard // the simulations below log thousands of decisions
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// node is a fake control-plane node: the wrapper installed under a
// component's name next to two fake alleles, and a state directory.
type node struct {
	bin, state string
	env        []string
}

func newNode(t *testing.T, component, allele string, script map[string]string) *node {
	t.Helper()
	n := &node{bin: t.TempDir(), state: t.TempDir()}
	b, err := os.ReadFile(wrapperBinary)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(n.bin, component), b, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, a := range []string{Xm, Xp} {
		body, ok := script[a]
		if !ok {
			body = `echo allele ` + a + ` args "$@"`
		}
		if err := os.WriteFile(filepath.Join(n.bin, component+"."+a), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if allele != "" {
		if err := os.WriteFile(filepath.Join(n.state, "x-inactivation"), []byte(allele+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	minor := filepath.Join(t.TempDir(), "xp-minor")
	if err := os.WriteFile(minor, []byte("1.36\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	n.env = append(os.Environ(),
		"KYVERNETRIA_STATE_DIR="+n.state,
		"KYVERNETRIA_XP_MINOR_FILE="+minor,
		"KYVERNETRIA_ALLELE=",
		"PATH="+n.bin+":"+os.Getenv("PATH"))
	return n
}

// run starts the component by bare name through PATH, the way kubeadm
// static pods do it, and returns its output and exit code.
func (n *node) run(t *testing.T, component string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(component, args...)
	cmd.Path, cmd.Err = filepath.Join(n.bin, component), nil
	cmd.Dir = t.TempDir()
	cmd.Env = n.env
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return string(out), exit.ExitCode()
	} else if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	return string(out), 0
}

func TestWrapperExecsExpressedAllele(t *testing.T) {
	n := newNode(t, Mosaic, Xp, nil)
	out, code := n.run(t, Mosaic, "--secure-port=6443")
	if code != 0 || !strings.Contains(out, "allele Xp args --secure-port=6443\n") {
		t.Errorf("wrong allele or args (exit %d):\n%s", code, out)
	}
}

func TestXmApiserverEmulatesXp(t *testing.T) {
	n := newNode(t, Mosaic, Xm, nil)
	out, code := n.run(t, Mosaic, "--secure-port=6443")
	if code != 0 || !strings.Contains(out, "allele Xm args --secure-port=6443 --emulated-version=1.36\n") {
		t.Errorf("Xm apiserver does not emulate the Xp minor (exit %d):\n%s", code, out)
	}
	out, _ = n.run(t, Mosaic, "--emulated-version=1.37")
	if !strings.Contains(out, "allele Xm args --emulated-version=1.37\n") {
		t.Errorf("an explicit --emulated-version was not respected:\n%s", out)
	}
	n.env = append(n.env, "KYVERNETRIA_XP_MINOR_FILE=/nonexistent")
	out, code = n.run(t, Mosaic)
	if code == 0 || strings.Contains(out, "allele Xm") {
		t.Errorf("Xm started without knowing what to emulate (exit %d):\n%s", code, out)
	}
}

func TestControllerManagerAndSchedulerAlwaysExpressXp(t *testing.T) {
	for _, c := range []string{"kube-controller-manager", "kube-scheduler"} {
		n := newNode(t, c, Xm, nil)
		out, code := n.run(t, c, "--leader-elect=true")
		if code != 0 || !strings.Contains(out, "allele Xp args --leader-elect=true\n") {
			t.Errorf("%s on an Xm node (exit %d):\n%s", c, code, out)
		}
		// They need no state at all, and never create it.
		n = newNode(t, c, "", nil)
		if out, code := n.run(t, c); code != 0 || !strings.Contains(out, "allele Xp") {
			t.Errorf("%s on a fresh node (exit %d):\n%s", c, code, out)
		}
		if _, err := os.Stat(filepath.Join(n.state, "x-inactivation")); err == nil {
			t.Errorf("%s made the node's choice", c)
		}
	}
}

// (a) through the wrapper: a broken state file is a crash loop, not Xm.
func TestCorruptStateFailsLoudly(t *testing.T) {
	n := newNode(t, Mosaic, "", nil)
	if err := os.WriteFile(filepath.Join(n.state, "x-inactivation"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	out, code := n.run(t, Mosaic)
	if code == 0 || strings.Contains(out, "allele X") || !strings.Contains(out, "refusing to start") {
		t.Errorf("zero-length state (exit %d):\n%s", code, out)
	}
}

func TestFreshNodeChooses(t *testing.T) {
	n := newNode(t, Mosaic, "", nil)
	out, code := n.run(t, Mosaic)
	if code != 0 || !strings.Contains(out, "first boot of this cell") {
		t.Errorf("fresh node (exit %d):\n%s", code, out)
	}
}

// (e) exit codes and signal deaths are propagated like a shell does.
func TestExitCodes(t *testing.T) {
	for script, want := range map[string]int{
		"exit 0":        0,
		"exit 3":        3,
		"kill -TERM $$": 128 + 15,
		"kill -KILL $$": 128 + 9,
	} {
		n := newNode(t, Mosaic, Xp, map[string]string{Xp: script})
		if _, code := n.run(t, Mosaic); code != want {
			t.Errorf("%q: exit %d, want %d", script, code, want)
		}
	}
}

func TestWrapperCountsOnlyQuickCrashes(t *testing.T) {
	n := newNode(t, Mosaic, Xp, map[string]string{Xp: "exit 1"})
	n.run(t, Mosaic)
	b, err := os.ReadFile(filepath.Join(n.state, Mosaic+".crashes"))
	if err != nil || len(strings.Fields(string(b))) != 1 {
		t.Fatalf("quick crash not recorded: %q %v", b, err)
	}
	n = newNode(t, Mosaic, Xp, map[string]string{Xp: "exit 0"})
	n.run(t, Mosaic)
	if _, err := os.Stat(filepath.Join(n.state, Mosaic+".crashes")); err == nil {
		t.Error("a clean exit was recorded as a crash")
	}
	n = newNode(t, "kube-scheduler", Xp, map[string]string{Xp: "exit 1"})
	n.run(t, "kube-scheduler")
	if m, _ := filepath.Glob(filepath.Join(n.state, "*.crashes")); len(m) != 0 {
		t.Errorf("a scheduler crash was recorded: %v", m)
	}
}

// (f) following an allele switch: SIGTERM, then SIGKILL, and not a crash.
func TestSwitchRestartKillsAStubbornComponent(t *testing.T) {
	started := filepath.Join(t.TempDir(), "started")
	n := newNode(t, Mosaic, Xp, map[string]string{Xp: `trap 'echo ignoring TERM' TERM; touch "$STARTED"; while :; do sleep 0.1; done`})
	n.env = append(n.env, "KYVERNETRIA_FOLLOW_INTERVAL=100ms", "KYVERNETRIA_STOP_TIMEOUT=1s", "STARTED="+started)
	go func() {
		for i := 0; i < 600; i++ {
			if _, err := os.Stat(started); err == nil {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		_ = writeAtomic(filepath.Join(n.state, "x-inactivation"), []byte("Xm\n"))
	}()
	start := time.Now()
	out, code := n.run(t, Mosaic)
	if code != 128+9 {
		t.Errorf("exit %d, want %d:\n%s", code, 128+9, out)
	}
	if took := time.Since(start); took > 40*time.Second {
		t.Errorf("took %s to follow the switch", took)
	}
	for _, want := range []string{"ignoring TERM", "sending SIGKILL", "on request; not a crash"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if _, err := os.Stat(filepath.Join(n.state, Mosaic+".crashes")); err == nil {
		t.Error("the restart to follow the switch was recorded as a crash")
	}
}

func testWrapper(c *Cell, component string) *wrapper {
	return &wrapper{component: component, cell: c, minUptime: 120 * time.Second}
}

func TestExitsThatAreNotCrashes(t *testing.T) {
	c, _ := newCell(t, Xm)
	mustExpress(t, c, Xm)
	w := testWrapper(c, Mosaic)
	for i := 0; i < 10; i++ {
		w.exited(Xm, 0, time.Second, false)   // clean exit
		w.exited(Xm, 143, time.Second, true)  // stopped by kubelet or a switch
		w.exited(Xm, 1, 5*time.Minute, false) // failed after running a while
		testWrapper(c, "kube-scheduler").exited(Xp, 2, time.Second, false)
	}
	if m, _ := filepath.Glob(filepath.Join(c.Dir, "*.crashes")); len(m) != 0 {
		t.Errorf("non-crashes were recorded: %v", m)
	}
	mustExpress(t, c, Xm)
}

// The old design let a crash-looping controller manager and scheduler flip
// the whole node back and forth. Now they cannot move it at all, and a
// crash-looping apiserver moves it once.
func TestNoSiblingPingPong(t *testing.T) {
	c, clk := newCell(t, Xm)
	mustExpress(t, c, Xm)
	kcm, sched, api := testWrapper(c, "kube-controller-manager"), testWrapper(c, "kube-scheduler"), testWrapper(c, Mosaic)
	for i := 0; i < 1000; i++ {
		kcm.exited(Xp, 1, time.Second, false)
		sched.exited(Xp, 1, time.Second, false)
		clk.Add(5 * time.Second)
	}
	mustExpress(t, c, Xm)
	if _, err := os.Stat(filepath.Join(c.Dir, "escapes")); err == nil {
		t.Fatal("controller manager or scheduler crashes made the node escape")
	}

	// An apiserver that crashes on both alleles: every hour, a burst of
	// crashes well over the threshold.
	switches := 0
	allele := Xm
	burst := func() {
		for i := 0; i < 6; i++ {
			api.exited(allele, 1, time.Second, false)
			clk.Add(10 * time.Second)
			now, _, err := c.Expressed()
			if err != nil {
				t.Fatal(err)
			}
			if now != allele {
				switches++
				allele = now
			}
		}
		clk.Add(time.Hour)
	}
	for h := 0; h < 48; h++ { // two days on one boot
		burst()
	}
	if switches != 1 {
		t.Fatalf("the node switched %d times in two days on one boot, want 1", switches)
	}
	// Reboots every day: the 24h cooldown still bounds it to one a day.
	boot := 1
	for day := 0; day < 3; day++ {
		boot++
		b := fmt.Sprintf("boot-%d", boot)
		c.BootID = func() string { return b }
		for h := 0; h < 24; h++ {
			burst()
		}
	}
	if switches != 4 {
		t.Fatalf("%d switches in five days with daily reboots, want one a day", switches)
	}
}
