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

// Command x-inactivation starts one of two builds ("alleles") of a
// Kubernetes control-plane component. It is installed under the component's
// own name (kube-apiserver, kube-controller-manager, kube-scheduler) next to
// <name>.Xm and <name>.Xp.
//
// Models: random, clonal X-chromosome inactivation (Lyon 1961), which makes
// every woman a cellular mosaic and is why X-linked conditions such as
// red-green colour blindness are an order of magnitude rarer in women. In
// engineering terms it is N-version programming (Avizienis 1985): nodes
// running different builds do not share every bug.
//
// Only kube-apiserver follows the node's allele. The version skew policy
// requires kube-controller-manager and kube-scheduler to be no newer than
// any apiserver in the cluster, so they always express Xp. An apiserver
// expressing Xm runs with --emulated-version set to the Xp minor, so both
// alleles serve the same API and feature-gate defaults: the mosaic is in the
// code, not in the API. See docs/OPERATIONS.md.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Mosaic is the only component that follows the node's allele.
const Mosaic = "kube-apiserver"

// DefaultXpMinorFile is where hack/build.sh bakes the Xp minor into the image.
const DefaultXpMinorFile = "/usr/local/share/kyvernetria/xp-minor"

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func duration(component, name string, def time.Duration) time.Duration {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		logf(component, "invalid %s=%q, using %s", name, v, def)
		return def
	}
	return d
}

// logOutput is where decisions are logged; the container log.
var logOutput io.Writer = os.Stderr

func logf(component, format string, args ...interface{}) {
	fmt.Fprintf(logOutput, "x-inactivation[%s]: %s\n", component, fmt.Sprintf(format, args...))
}

func main() {
	component := filepath.Base(os.Args[0])
	if component == "x-inactivation" {
		fmt.Fprintln(os.Stderr, "x-inactivation is installed under a component's name, e.g. /usr/local/bin/kube-apiserver")
		os.Exit(2)
	}
	os.Exit(run(component, os.Args[1:]))
}

// wrapper holds one run's settings.
type wrapper struct {
	component   string
	cell        *Cell
	pinned      bool
	minUptime   time.Duration // a failure after running this long is not a crash
	stopTimeout time.Duration // SIGTERM to SIGKILL when following an allele switch
	follow      time.Duration // how often to check the node's allele
}

func newWrapper(component string) *wrapper {
	crashes, err := strconv.Atoi(env("KYVERNETRIA_ESCAPE_CRASHES", "5"))
	if err != nil || crashes < 2 {
		logf(component, "invalid KYVERNETRIA_ESCAPE_CRASHES, using 5")
		crashes = 5
	}
	return &wrapper{
		component: component,
		cell: &Cell{
			Dir:           env("KYVERNETRIA_STATE_DIR", "/var/lib/kyvernetria"),
			EscapeCrashes: crashes,
			EscapeWindow:  duration(component, "KYVERNETRIA_ESCAPE_WINDOW", 10*time.Minute),
			Cooldown:      duration(component, "KYVERNETRIA_ESCAPE_COOLDOWN", 24*time.Hour),
			Now:           time.Now,
			Random:        RandomAllele,
			BootID:        KernelBootID,
		},
		minUptime:   duration(component, "KYVERNETRIA_CRASH_UPTIME", 120*time.Second),
		stopTimeout: duration(component, "KYVERNETRIA_STOP_TIMEOUT", 30*time.Second),
		follow:      duration(component, "KYVERNETRIA_FOLLOW_INTERVAL", 5*time.Second),
	}
}

func run(component string, args []string) int {
	w := newWrapper(component)
	allele, err := w.choose()
	if err != nil {
		logf(component, "refusing to start: %v", err)
		return 1
	}
	if component == Mosaic && allele == Xm {
		if args, err = withEmulation(args); err != nil {
			logf(component, "refusing to start %s: %v", Xm, err)
			return 1
		}
	}
	binary := filepath.Join(env("KYVERNETRIA_BIN_DIR", executableDir()), component+"."+allele)
	logf(component, "expressing %s (%s %s)", allele, binary, strings.Join(args, " "))
	return w.supervise(allele, binary, args)
}

// choose returns the allele this component expresses.
func (w *wrapper) choose() (string, error) {
	if pinned := os.Getenv("KYVERNETRIA_ALLELE"); pinned != "" {
		if pinned != Xm && pinned != Xp {
			return "", fmt.Errorf("KYVERNETRIA_ALLELE=%q, want %s or %s", pinned, Xm, Xp)
		}
		w.pinned = true
		logf(w.component, "allele pinned to %s by KYVERNETRIA_ALLELE; no escapes", pinned)
		if w.component != Mosaic && pinned == Xm {
			logf(w.component, "WARNING: %s newer than an Xp apiserver breaks the version skew policy", w.component)
		}
		return pinned, nil
	}
	if w.component != Mosaic {
		logf(w.component, "expressing %s regardless of the node's allele: the skew policy forbids a %s newer than any apiserver", Xp, w.component)
		return Xp, nil
	}
	allele, firstBoot, err := w.cell.Expressed()
	if err != nil {
		return "", fmt.Errorf("cannot read the node's inactivation state: %w; fix or remove %s (removing it makes the node choose again)", err, w.cell.choicePath())
	}
	if firstBoot {
		logf(w.component, "first boot of this cell: silenced %s, expressing %s for good", Other(allele), allele)
	}
	return allele, nil
}

// withEmulation appends --emulated-version=<Xp minor> unless the caller
// already chose an emulated version.
func withEmulation(args []string) ([]string, error) {
	for _, a := range args {
		if a == "--emulated-version" || strings.HasPrefix(a, "--emulated-version=") {
			return args, nil
		}
	}
	minor, err := xpMinor()
	if err != nil {
		return nil, err
	}
	return append(append([]string(nil), args...), "--emulated-version="+minor), nil
}

// xpMinor returns the Xp major.minor baked into the image by hack/build.sh.
func xpMinor() (string, error) {
	v := os.Getenv("KYVERNETRIA_XP_MINOR")
	if v == "" {
		path := env("KYVERNETRIA_XP_MINOR_FILE", DefaultXpMinorFile)
		b, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("cannot tell which version to emulate: %w", err)
		}
		v = strings.TrimSpace(string(b))
	}
	parts := strings.Split(v, ".")
	if len(parts) != 2 {
		return "", fmt.Errorf("Xp minor %q is not major.minor", v)
	}
	for _, p := range parts {
		if _, err := strconv.Atoi(p); err != nil {
			return "", fmt.Errorf("Xp minor %q is not major.minor", v)
		}
	}
	return v, nil
}

// executableDir is where the wrapper itself lives. os.Args[0] is not enough:
// kubeadm starts components by bare name, resolved through PATH.
func executableDir() string {
	if exe, err := os.Executable(); err == nil {
		return filepath.Dir(exe)
	}
	return "/usr/local/bin"
}

// supervise runs the component, forwards signals to it, and restarts the
// container (by exiting) when the node's allele changes underneath it. It
// returns the component's exit code, or 128+signal if a signal killed it.
func (w *wrapper) supervise(allele, binary string, args []string) int {
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT)
	defer signal.Stop(signals)

	started := time.Now()
	cmd := exec.Command(binary, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		logf(w.component, "cannot start %s: %v", binary, err)
		w.exited(allele, 1, 0, false)
		return 1
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	tick := time.NewTicker(w.follow)
	defer tick.Stop()
	var kill <-chan time.Time
	stopping := false // we asked the component to stop: its exit is not a crash
	for {
		select {
		case sig := <-signals:
			stopping = true
			_ = cmd.Process.Signal(sig)
		case err := <-done:
			code := exitCode(err)
			w.exited(allele, code, time.Since(started), stopping)
			return code
		case <-kill:
			logf(w.component, "%s still running %s after SIGTERM; sending SIGKILL", binary, w.stopTimeout)
			_ = cmd.Process.Kill()
		case <-tick.C:
			if w.component != Mosaic || w.pinned || stopping {
				continue
			}
			now, err := w.cell.read()
			if err != nil {
				logf(w.component, "cannot read the node's allele while running (%v); keeping %s", err, allele)
				continue
			}
			if now != allele {
				logf(w.component, "the node switched to %s; stopping %s to follow it", now, allele)
				stopping = true
				_ = cmd.Process.Signal(syscall.SIGTERM)
				kill = time.After(w.stopTimeout)
			}
		}
	}
}

// exitCode converts the result of cmd.Wait into a shell-style exit code.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if ws, ok := exit.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return exit.ExitCode()
	}
	return 1
}

// exited decides whether an exit was a crash and, for kube-apiserver,
// records it, which may make the node escape to the other allele.
func (w *wrapper) exited(allele string, code int, ran time.Duration, stopping bool) {
	switch {
	case code == 0:
		logf(w.component, "exited cleanly after %s; not a crash", ran.Round(time.Second))
		return
	case stopping:
		logf(w.component, "exited with %d after %s on request; not a crash", code, ran.Round(time.Second))
		return
	case ran >= w.minUptime:
		logf(w.component, "exited with %d after %s, longer than %s; not counted as a crash", code, ran.Round(time.Second), w.minUptime)
		return
	case w.component != Mosaic:
		logf(w.component, "crashed with %d after %s; %s never escapes, it always expresses %s", code, ran.Round(time.Second), w.component, Xp)
		return
	case w.pinned:
		logf(w.component, "crashed with %d after %s; allele pinned, no escape", code, ran.Round(time.Second))
		return
	}
	d, err := w.cell.RecordCrash(w.component, allele)
	if err != nil {
		logf(w.component, "crashed with %d after %s on %s, but the crash could not be recorded: %v", code, ran.Round(time.Second), allele, err)
	}
	if d.Escaped {
		logf(w.component, "crashed with %d after %s: %s on %s; escaping inactivation, the node now expresses %s", code, ran.Round(time.Second), d.Reason, allele, d.To)
		return
	}
	logf(w.component, "crashed with %d after %s on %s: %s; no escape", code, ran.Round(time.Second), allele, d.Reason)
}
