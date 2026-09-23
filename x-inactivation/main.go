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
package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func logf(component, format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "x-inactivation[%s]: %s\n", component, fmt.Sprintf(format, args...))
}

func main() {
	component := filepath.Base(os.Args[0])
	if component == "x-inactivation" {
		fmt.Fprintln(os.Stderr, "x-inactivation is installed under a component's name, e.g. /usr/local/bin/kube-apiserver")
		os.Exit(2)
	}
	os.Exit(run(component, os.Args[1:]))
}

func run(component string, args []string) int {
	starts, _ := strconv.Atoi(env("KYVERNETRIA_ESCAPE_STARTS", "5"))
	window, err := time.ParseDuration(env("KYVERNETRIA_ESCAPE_WINDOW", "10m"))
	if err != nil || starts < 2 {
		logf(component, "invalid escape settings, using 5 starts in 10m")
		starts, window = 5, 10*time.Minute
	}
	cell := &Cell{
		Dir:          env("KYVERNETRIA_STATE_DIR", "/var/lib/kyvernetria"),
		EscapeStarts: starts,
		EscapeWindow: window,
		Now:          time.Now,
		Random:       RandomAllele,
	}

	allele, firstBoot, err := cell.Expressed()
	if pinned := os.Getenv("KYVERNETRIA_ALLELE"); pinned == Xm || pinned == Xp {
		allele, firstBoot, err = pinned, false, nil
		logf(component, "allele pinned to %s by KYVERNETRIA_ALLELE", pinned)
	}
	if err != nil {
		// Without a writable state directory the cell cannot remember its
		// choice; fall back to the newer build rather than failing to start.
		logf(component, "cannot keep inactivation state (%v); expressing %s", err, Xm)
		allele = Xm
	} else {
		if firstBoot {
			logf(component, "first boot of this cell: silenced %s, expressing %s for good", Other(allele), allele)
		}
		if looping, err := cell.RecordStart(component); err != nil {
			logf(component, "cannot record start: %v", err)
		} else if looping && os.Getenv("KYVERNETRIA_ALLELE") == "" {
			to, err := cell.Escape(allele)
			if err != nil {
				logf(component, "crash loop on %s, but escape failed: %v", allele, err)
			} else {
				logf(component, "%d starts within %s on %s: escaping inactivation, the node now expresses %s", starts, window, allele, to)
				allele = to
			}
		}
	}

	binary := filepath.Join(env("KYVERNETRIA_BIN_DIR", executableDir()), component+"."+allele)
	logf(component, "expressing %s (%s)", allele, binary)
	return supervise(component, cell, allele, binary, args)
}

// executableDir is where the wrapper itself lives. os.Args[0] is not enough:
// kubeadm starts components by bare name, resolved through PATH.
func executableDir() string {
	if exe, err := os.Executable(); err == nil {
		return filepath.Dir(exe)
	}
	return "/usr/local/bin"
}

// supervise runs the component and restarts the container (by exiting) when
// the node's allele changes underneath it, e.g. because a sibling component
// escaped. Signals are forwarded so the component shuts down gracefully.
func supervise(component string, cell *Cell, allele, binary string, args []string) int {
	cmd := exec.Command(binary, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		logf(component, "cannot start %s: %v", binary, err)
		return 1
	}
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case sig := <-signals:
			_ = cmd.Process.Signal(sig)
		case err := <-done:
			if exit, ok := err.(*exec.ExitError); ok {
				return exit.ExitCode()
			}
			if err != nil {
				return 1
			}
			return 0
		case <-tick.C:
			if now, err := cell.read(); err == nil && now != allele && os.Getenv("KYVERNETRIA_ALLELE") == "" {
				logf(component, "the node switched to %s; restarting to follow it", now)
				_ = cmd.Process.Signal(syscall.SIGTERM)
			}
		}
	}
}
