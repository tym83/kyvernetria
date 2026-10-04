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
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	"k8s.io/kubernetes/pkg/kyvernetria"
)

// lifecycle is an upstream minor's patch-support calendar. maintenance is
// zero where this table does not record it.
type lifecycle struct {
	maintenance, eol time.Time
}

func day(s string) time.Time {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return t
}

// upstreamLifecycles comes from https://kubernetes.io/releases/patch-releases/
// and https://kubernetes.io/releases/ (retrieved 2026-10-04). Add each new
// minor when a release starts building it.
var upstreamLifecycles = map[uint]lifecycle{
	32: {eol: day("2026-02-28")},
	33: {eol: day("2026-06-28")},
	34: {maintenance: day("2026-08-27"), eol: day("2026-10-27")},
	35: {maintenance: day("2026-12-28"), eol: day("2027-02-28")},
	36: {maintenance: day("2027-04-28"), eol: day("2027-06-28")},
	37: {maintenance: day("2027-08-28"), eol: day("2027-10-28")},
}

// Stage is where a minor is in its support window.
type Stage int

const (
	// Supported: upstream's standard period, every kind of fix.
	Supported Stage = iota
	// Maintenance: upstream fixes CVEs, dependencies and critical bugs only.
	Maintenance
	// Grace: upstream end of life has passed. Nothing is fixed any more;
	// Kyvernetria still builds and tests the minor, and the upgrade off it.
	Grace
	// Unsupported: past Kyvernetria's grace too.
	Unsupported
	// Unknown: a minor newer than this kyvctl's table.
	Unknown
)

func (s Stage) String() string {
	return [...]string{"supported", "maintenance mode", "upgrade grace, no fixes", "out of support", "unknown"}[s]
}

// MinorSupport is one Kubernetes 1.x minor's support as of a date.
type MinorSupport struct {
	Minor uint
	Stage Stage
	// UpstreamEOL and Until (Kyvernetria's end of support) are zero for
	// Unknown, and for minors older than the table, which are all past it.
	UpstreamEOL, Until time.Time
}

// AssessMinor places Kubernetes 1.<minor> in its support window on now.
func AssessMinor(minor uint, now time.Time) MinorSupport {
	s := MinorSupport{Minor: minor}
	lc, ok := upstreamLifecycles[minor]
	if !ok {
		oldest, newest := tableRange()
		switch {
		case minor < oldest:
			s.Stage = Unsupported
		case minor > newest:
			s.Stage = Unknown
		default:
			s.Stage = Unknown // a gap in the table; should not happen
		}
		return s
	}
	s.UpstreamEOL = lc.eol
	s.Until = lc.eol.Add(kyvernetria.SupportGrace)
	switch {
	case !now.Before(s.Until):
		s.Stage = Unsupported
	case !now.Before(lc.eol):
		s.Stage = Grace
	case !lc.maintenance.IsZero() && !now.Before(lc.maintenance):
		s.Stage = Maintenance
	default:
		s.Stage = Supported
	}
	return s
}

func tableRange() (oldest, newest uint) {
	first := true
	for m := range upstreamLifecycles {
		if first || m < oldest {
			oldest = m
		}
		if first || m > newest {
			newest = m
		}
		first = false
	}
	return oldest, newest
}

func newSupportCommand(f cmdutil.Factory, streams genericiooptions.IOStreams) *cobra.Command {
	var on string
	cmd := &cobra.Command{
		Use:   "support",
		Short: "Show how long this cluster's Kubernetes minors are supported",
		Long: "Lists every Kubernetes minor the cluster runs (apiservers and kubelets), where\n" +
			"each is in upstream's ~14-month support window, and when Kyvernetria's support\n" +
			"for it ends. Kyvernetria adds 30 days of upgrade grace after upstream's end of\n" +
			"life, about 7% of the window, the female-to-male ratio of life expectancy.\n" +
			"The grace is time to upgrade, not fixes: nobody patches a minor after upstream\n" +
			"end of life. Exits non-zero once a minor in use is out of support.",
		Example: "  kyvctl support\n  kyvctl support --on 2027-07-01   # what it will say on that day",
		Args:    cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			now := time.Now().UTC()
			if on != "" {
				t, err := time.Parse(time.DateOnly, on)
				if err != nil {
					cmdutil.CheckErr(fmt.Errorf("--on wants a date like 2027-07-01: %w", err))
				}
				now = t
			}
			cmdutil.CheckErr(runSupport(cmd.Context(), f, streams.Out, now))
		},
	}
	cmd.Flags().StringVar(&on, "on", "", "assess support as of this date (YYYY-MM-DD) instead of today")
	return cmd
}

// usage counts what runs a minor: apiservers and kubelets.
type usage struct{ apiservers, kubelets int }

func runSupport(ctx context.Context, f cmdutil.Factory, out io.Writer, now time.Time) error {
	client, err := f.KubernetesClientSet()
	if err != nil {
		return err
	}
	used := map[uint]*usage{}
	at := func(minor uint) *usage {
		if used[minor] == nil {
			used[minor] = &usage{}
		}
		return used[minor]
	}
	cells, err := apiserverCells(ctx, client)
	if err != nil {
		return err
	}
	silent := 0
	for _, c := range cells {
		// The binary's minor, not the emulated one: support is about
		// whose code runs, and an Xm apiserver runs Xm code.
		if _, minor, ok := minorOf(c.version); ok {
			at(minor).apiservers++
		} else {
			silent++
		}
	}
	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, n := range nodes.Items {
		if _, minor, ok := minorOf(n.Status.NodeInfo.KubeletVersion); ok {
			at(minor).kubelets++
		}
	}
	return renderSupport(out, used, silent, now)
}

func date(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Format(time.DateOnly)
}

// renderSupport prints the table and the verdict, and returns an error
// once a minor in use is out of support.
func renderSupport(out io.Writer, used map[uint]*usage, silent int, now time.Time) error {
	if len(used) == 0 {
		fmt.Fprintln(out, "I can't tell which Kubernetes versions this cluster runs: no apiserver answered and no node reports a kubelet version.")
		return nil
	}
	minors := make([]uint, 0, len(used))
	for m := range used {
		minors = append(minors, m)
	}
	sort.Slice(minors, func(i, j int) bool { return minors[i] > minors[j] })

	fmt.Fprintf(out, "%-6s %-26s %-14s %-19s %s\n", "MINOR", "RUNS", "UPSTREAM EOL", "KYVERNETRIA UNTIL", "STATUS (as of "+date(now)+")")
	assessed := make([]MinorSupport, 0, len(minors))
	for _, m := range minors {
		s := AssessMinor(m, now)
		assessed = append(assessed, s)
		var runs []string
		if u := used[m]; u.apiservers > 0 {
			runs = append(runs, plural(u.apiservers, "apiserver"))
		}
		if u := used[m]; u.kubelets > 0 {
			runs = append(runs, plural(u.kubelets, "kubelet"))
		}
		fmt.Fprintf(out, "%-6s %-26s %-14s %-19s %s\n", fmt.Sprintf("1.%d", m), strings.Join(runs, ", "), date(s.UpstreamEOL), date(s.Until), s.Stage)
	}
	fmt.Fprintln(out)
	if silent > 0 {
		fmt.Fprintf(out, "%s didn't answer, so a minor may be missing above.\n", plural(silent, "apiserver"))
	}

	var known []MinorSupport
	var gone, grace []string
	for _, s := range assessed {
		switch s.Stage {
		case Unknown:
			fmt.Fprintf(out, "1.%d is newer than this kyvctl knows; see https://kubernetes.io/releases/ for its dates.\n", s.Minor)
		case Unsupported:
			gone = append(gone, fmt.Sprintf("1.%d", s.Minor))
		case Grace:
			grace = append(grace, fmt.Sprintf("1.%d", s.Minor))
			fmt.Fprintf(out, "1.%d reached upstream end of life on %s: nothing is fixed any more, not even security bugs. "+
				"Kyvernetria still builds and tests it, and the upgrade off it, until %s.\n", s.Minor, date(s.UpstreamEOL), date(s.Until))
		}
		if !s.Until.IsZero() {
			known = append(known, s)
		}
	}
	if len(gone) > 0 {
		return fmt.Errorf("this cluster runs %s, out of support: nothing fixes it any more, and Kyvernetria no longer builds "+
			"or tests it. Upgrade one minor at a time, as docs/OPERATIONS.md describes", strings.Join(gone, ", "))
	}
	if len(known) == 0 {
		return nil
	}
	// The cluster is supported as long as its oldest minor is.
	oldest := known[len(known)-1]
	fmt.Fprintf(out, "This cluster is supported until %s, bounded by 1.%d, its oldest minor (%s left). ",
		date(oldest.Until), oldest.Minor, daysLeft(oldest.Until.Sub(now)))
	if len(grace) == 0 {
		fmt.Fprintf(out, "Plan the upgrade before %s, when upstream stops fixing 1.%d.\n", date(oldest.UpstreamEOL), oldest.Minor)
	} else {
		fmt.Fprintln(out, "Upgrade now: docs/OPERATIONS.md.")
	}
	if newest := known[0]; newest.Minor != oldest.Minor {
		fmt.Fprintf(out, "Running two minors costs time: 1.%d alone would be supported until %s.\n", newest.Minor, date(newest.Until))
	}
	return nil
}

func daysLeft(d time.Duration) string {
	return plural(int(d.Hours()/24), "day")
}
