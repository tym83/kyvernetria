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
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Alleles. Xm is built from the newer Kubernetes minor, Xp from the older.
const (
	Xm = "Xm"
	Xp = "Xp"
)

// Other returns the allele that is silenced when a is expressed.
func Other(a string) string {
	if a == Xm {
		return Xp
	}
	return Xm
}

// UpgradingFlag is the name, inside the state directory, of the file that
// blocks escapes while an operator upgrades the node (docs/OPERATIONS.md).
const UpgradingFlag = "upgrading"

// Cell is one node's inactivation state on disk.
type Cell struct {
	Dir           string
	EscapeCrashes int           // crashes within EscapeWindow that trigger an escape
	EscapeWindow  time.Duration // how far back crashes are counted
	Cooldown      time.Duration // no second escape within this long after one
	Now           func() time.Time
	Random        func() (string, error)
	BootID        func() string // identifies the current boot; "" if unknown
}

// RandomAllele picks Xm or Xp with equal probability.
func RandomAllele() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(2))
	if err != nil {
		return "", err
	}
	if n.Int64() == 0 {
		return Xm, nil
	}
	return Xp, nil
}

// KernelBootID returns the kernel's random boot id, which changes on every
// boot, or "" where the kernel does not provide one.
func KernelBootID() string {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (c *Cell) choicePath() string     { return filepath.Join(c.Dir, "x-inactivation") }
func (c *Cell) lastEscapePath() string { return filepath.Join(c.Dir, "last-escape") }

// Expressed returns the node's kube-apiserver allele, choosing it at random
// on the very first call. The choice is made once and inherited by every
// later start, like X-inactivation, which happens early in the embryo and is
// kept by all descendants of each cell.
//
// Only a node without any state (fs.ErrNotExist) makes a new choice. A state
// file that cannot be read or holds anything but an allele is an error: the
// caller must fail loudly rather than guess, because guessing could put a
// node on the wrong side of the version skew policy.
func (c *Cell) Expressed() (allele string, firstBoot bool, err error) {
	if a, err := c.read(); err == nil {
		return a, false, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", false, err
	}
	pick, err := c.Random()
	if err != nil {
		return "", false, err
	}
	if err := os.MkdirAll(c.Dir, 0o755); err != nil {
		return "", false, err
	}
	// Write the choice to a private, synced file, then publish it with
	// link(2), which fails if the choice already exists: readers never see a
	// half-written file, and exactly one starter wins.
	tmp, err := writeTemp(c.Dir, []byte(pick+"\n"))
	if err != nil {
		return "", false, err
	}
	defer os.Remove(tmp)
	if err := os.Link(tmp, c.choicePath()); errors.Is(err, fs.ErrExist) {
		a, err := c.read()
		return a, false, err
	} else if err != nil {
		return "", false, err
	}
	if err := syncDir(c.Dir); err != nil {
		return "", false, err
	}
	return pick, true, nil
}

func (c *Cell) read() (string, error) {
	b, err := os.ReadFile(c.choicePath())
	if err != nil {
		return "", err
	}
	a := strings.TrimSpace(string(b))
	if a != Xm && a != Xp {
		return "", fmt.Errorf("%s holds %q, want %s or %s", c.choicePath(), a, Xm, Xp)
	}
	return a, nil
}

// Upgrading reports whether an operator has blocked escapes on this node.
func (c *Cell) Upgrading() bool {
	_, err := os.Stat(filepath.Join(c.Dir, UpgradingFlag))
	return err == nil
}

// Decision is what RecordCrash concluded, for the log.
type Decision struct {
	Crashes int    // crashes of the component within the window, this one included
	Escaped bool   // the node switched alleles
	To      string // the allele the node expresses now
	Reason  string // why the node did or did not escape
}

// RecordCrash notes a crash of component while it expressed allele, and
// switches the node to the other allele once there have been EscapeCrashes
// crashes within EscapeWindow, unless an escape is blocked. The whole
// read-check-write runs under an exclusive lock on the state directory.
func (c *Cell) RecordCrash(component, allele string) (Decision, error) {
	d := Decision{To: allele}
	if err := os.MkdirAll(c.Dir, 0o755); err != nil {
		return d, err
	}
	unlock, err := c.lock()
	if err != nil {
		return d, err
	}
	defer unlock()

	now := c.Now()
	history := filepath.Join(c.Dir, component+".crashes")
	recent := c.recent(history, now)
	recent = append(recent, now.Unix())
	d.Crashes = len(recent)
	if len(recent) < c.EscapeCrashes {
		d.Reason = fmt.Sprintf("%d of %d crashes within %s", len(recent), c.EscapeCrashes, c.EscapeWindow)
		return d, writeAtomic(history, formatTimes(recent))
	}
	current, err := c.read()
	if err != nil {
		d.Reason = "cannot read the node's allele"
		return d, errors.Join(err, writeAtomic(history, formatTimes(recent)))
	}
	if current != allele {
		d.To = current
		d.Reason = fmt.Sprintf("the node already switched to %s", current)
		return d, writeAtomic(history, formatTimes(recent))
	}
	if reason := c.escapeBlocked(now); reason != "" {
		d.Reason = reason
		return d, writeAtomic(history, formatTimes(recent))
	}

	to := Other(allele)
	if err := writeAtomic(c.choicePath(), []byte(to+"\n")); err != nil {
		return d, err
	}
	d.Escaped, d.To = true, to
	d.Reason = fmt.Sprintf("%d crashes within %s", len(recent), c.EscapeWindow)
	stamp := strconv.FormatInt(now.Unix(), 10)
	if boot := c.bootID(); boot != "" {
		stamp += " " + boot
	}
	var errs []error
	errs = append(errs, writeAtomic(c.lastEscapePath(), []byte(stamp+"\n")))
	errs = append(errs, c.clearHistories())
	errs = append(errs, appendLine(filepath.Join(c.Dir, "escapes"),
		fmt.Sprintf("%s %s escaped %s -> %s", now.UTC().Format(time.RFC3339), component, allele, to)))
	return d, errors.Join(errs...)
}

// escapeBlocked returns why the node may not escape now, or "".
func (c *Cell) escapeBlocked(now time.Time) string {
	if c.Upgrading() {
		return fmt.Sprintf("escapes are blocked by %s", filepath.Join(c.Dir, UpgradingFlag))
	}
	b, err := os.ReadFile(c.lastEscapePath())
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return ""
	}
	if len(fields) > 1 && fields[1] == c.bootID() {
		return "the node already escaped once since it booted"
	}
	sec, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return ""
	}
	last := time.Unix(sec, 0)
	// A last escape in the future means the clock was stepped back; it says
	// nothing about how long ago the escape really was.
	if !last.After(now) && now.Sub(last) < c.Cooldown {
		return fmt.Sprintf("cooling down after the escape at %s until %s",
			last.UTC().Format(time.RFC3339), last.Add(c.Cooldown).UTC().Format(time.RFC3339))
	}
	return ""
}

func (c *Cell) bootID() string {
	if c.BootID == nil {
		return ""
	}
	return c.BootID()
}

// recent returns the timestamps in a history file that fall within the
// window before now. Entries in the future (the clock was stepped back) and
// unparsable lines are ignored.
func (c *Cell) recent(path string, now time.Time) []int64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []int64
	for _, line := range strings.Fields(string(b)) {
		sec, err := strconv.ParseInt(line, 10, 64)
		if err != nil {
			continue
		}
		t := time.Unix(sec, 0)
		if t.After(now) || now.Sub(t) > c.EscapeWindow {
			continue
		}
		out = append(out, sec)
	}
	return out
}

// clearHistories forgets every component's crash history (and the start
// histories of older releases), so an escape needs a fresh crash loop.
func (c *Cell) clearHistories() error {
	var errs []error
	for _, pattern := range []string{"*.crashes", "*.starts"} {
		matches, err := filepath.Glob(filepath.Join(c.Dir, pattern))
		if err != nil {
			return err
		}
		for _, m := range matches {
			if err := os.Remove(m); err != nil && !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// lock takes an exclusive flock on the state directory's lock file.
func (c *Cell) lock() (func(), error) {
	f, err := os.OpenFile(filepath.Join(c.Dir, ".lock"), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

func formatTimes(ts []int64) []byte {
	var b strings.Builder
	for _, t := range ts {
		b.WriteString(strconv.FormatInt(t, 10))
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

// writeTemp writes data to a new, synced, world-readable temporary file in
// dir and returns its path. Which allele a node expresses is not a secret.
func writeTemp(dir string, data []byte) (string, error) {
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return "", err
	}
	err = errors.Join(f.Chmod(0o644), write(f, data), f.Sync(), f.Close())
	if err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

func write(f *os.File, data []byte) error {
	_, err := f.Write(data)
	return err
}

// writeAtomic replaces path with data: readers see the old or the new
// content, never a mix, and the new content survives a power loss once
// writeAtomic returns.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := writeTemp(dir, data)
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}

func appendLine(path, line string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	_, err = f.WriteString(line + "\n")
	return errors.Join(err, f.Sync(), f.Close())
}
