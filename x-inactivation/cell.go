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

// Cell is one node's inactivation state on disk.
type Cell struct {
	Dir          string
	EscapeStarts int           // starts within EscapeWindow that trigger an escape
	EscapeWindow time.Duration // how far back starts are counted
	Now          func() time.Time
	Random       func() (string, error)
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

func (c *Cell) choicePath() string { return filepath.Join(c.Dir, "x-inactivation") }

// Expressed returns the node's active allele, choosing it at random on the
// very first call. The choice is made once and inherited by every later
// start of every component on the node, like X-inactivation, which happens
// early in the embryo and is kept by all descendants of each cell. Components
// starting at the same moment agree because only one of them can create the
// file.
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
	// Write the choice to a private file, then publish it with link(2), which
	// fails if the choice already exists: readers never see a half-written
	// file, and exactly one component wins.
	tmp, err := os.CreateTemp(c.Dir, ".x-inactivation-*")
	if err != nil {
		return "", false, err
	}
	defer os.Remove(tmp.Name())
	// Readable by everyone: which allele a node expresses is not a secret.
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return "", false, err
	}
	if _, err := tmp.WriteString(pick + "\n"); err != nil {
		_ = tmp.Close()
		return "", false, err
	}
	if err := tmp.Close(); err != nil {
		return "", false, err
	}
	if err := os.Link(tmp.Name(), c.choicePath()); errors.Is(err, fs.ErrExist) {
		a, err := c.read()
		return a, false, err
	} else if err != nil {
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

// Escape switches the whole node to the other allele. In biology, a minority
// of X-linked genes escape inactivation one by one; here the whole
// chromosome switches, because the Kubernetes version skew policy forbids a
// controller manager or scheduler newer than the apiserver it talks to.
func (c *Cell) Escape(from string) (string, error) {
	to := Other(from)
	tmp := c.choicePath() + ".tmp"
	if err := os.WriteFile(tmp, []byte(to+"\n"), 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, c.choicePath()); err != nil {
		return "", err
	}
	line := fmt.Sprintf("%s escaped %s -> %s\n", c.Now().UTC().Format(time.RFC3339), from, to)
	f, err := os.OpenFile(filepath.Join(c.Dir, "escapes"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err == nil {
		_, _ = f.WriteString(line)
		_ = f.Close()
	}
	return to, nil
}

// RecordStart notes that component is starting and reports whether it has
// started often enough within the window to count as a crash loop. The
// history is cleared when it does, so one escape needs a fresh crash loop.
func (c *Cell) RecordStart(component string) (crashLooping bool, err error) {
	path := filepath.Join(c.Dir, component+".starts")
	now := c.Now()
	var recent []string
	if b, err := os.ReadFile(path); err == nil {
		for _, line := range strings.Fields(string(b)) {
			sec, err := strconv.ParseInt(line, 10, 64)
			if err == nil && now.Sub(time.Unix(sec, 0)) <= c.EscapeWindow {
				recent = append(recent, line)
			}
		}
	}
	recent = append(recent, strconv.FormatInt(now.Unix(), 10))
	if len(recent) >= c.EscapeStarts {
		return true, os.WriteFile(path, nil, 0o644)
	}
	return false, os.WriteFile(path, []byte(strings.Join(recent, "\n")+"\n"), 0o644)
}
