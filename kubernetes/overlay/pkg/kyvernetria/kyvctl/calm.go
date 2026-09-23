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
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
)

func newCalmCommand(f cmdutil.Factory, streams genericiooptions.IOStreams) *cobra.Command {
	var all bool
	var since time.Duration
	var top int
	c := &cobra.Command{
		Use:   "calm",
		Short: "Fold a noisy stream of warnings into the few things worth attention",
		Long: "Kyvernetria warns early and often (higher neuroticism has a cost: more alarms).\n" +
			"calm groups warnings by workload and reason, counts them, and lists the\n" +
			"few groups that deserve attention first.",
		Example: "  kyvctl calm\n  kyvctl calm -A --since 6h",
		Args:    cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			cmdutil.CheckErr(runCalm(cmd.Context(), f, streams.Out, all, since, top))
		},
	}
	c.Flags().BoolVarP(&all, "all-namespaces", "A", false, "Look at every namespace")
	c.Flags().DurationVar(&since, "since", time.Hour, "How far back to look")
	c.Flags().IntVar(&top, "top", 5, "How many groups to show")
	return c
}

func runCalm(ctx context.Context, f cmdutil.Factory, out io.Writer, all bool, since time.Duration, top int) error {
	ns, _, err := f.ToRawKubeConfigLoader().Namespace()
	if err != nil {
		return err
	}
	if all {
		ns = metav1.NamespaceAll
	}
	client, err := f.KubernetesClientSet()
	if err != nil {
		return err
	}
	list, err := client.CoreV1().Events(ns).List(ctx, metav1.ListOptions{FieldSelector: "type=Warning"})
	if err != nil {
		return err
	}
	renderCalm(out, Fold(list.Items, time.Now().Add(-since)), top, since)
	return nil
}

// Worry is one folded group of warnings.
type Worry struct {
	Namespace string
	Subject   string
	Reason    string
	Count     int32
	Last      time.Time
	Message   string
}

var (
	deploymentPod = regexp.MustCompile(`^(.+)-[a-z0-9]{8,10}-[a-z0-9]{5}$`)
	replicaSet    = regexp.MustCompile(`^(.+)-[a-z0-9]{8,10}$`)
	generatedPod  = regexp.MustCompile(`^(.+)-[a-z0-9]{5}$`)
	ordinalPod    = regexp.MustCompile(`^(.+)-[0-9]+$`)
)

// Subject names what a warning is really about: the workload behind a pod
// or ReplicaSet rather than one replica.
func Subject(kind, name string) string {
	switch kind {
	case "Pod":
		for _, re := range []*regexp.Regexp{deploymentPod, generatedPod, ordinalPod} {
			if m := re.FindStringSubmatch(name); m != nil {
				return m[1] + " (pods)"
			}
		}
	case "ReplicaSet":
		if m := replicaSet.FindStringSubmatch(name); m != nil {
			return m[1] + " (replicasets)"
		}
	}
	return strings.ToLower(kind) + "/" + name
}

// Fold groups warnings newer than after, most frequent first.
func Fold(events []v1.Event, after time.Time) []Worry {
	groups := map[string]*Worry{}
	for _, e := range events {
		t := eventTime(e)
		if e.Type != v1.EventTypeWarning || t.Before(after) {
			continue
		}
		subject := Subject(e.InvolvedObject.Kind, e.InvolvedObject.Name)
		key := e.Namespace + "\x00" + subject + "\x00" + e.Reason
		g, ok := groups[key]
		if !ok {
			g = &Worry{Namespace: e.Namespace, Subject: subject, Reason: e.Reason}
			groups[key] = g
		}
		count := e.Count
		if count < 1 {
			count = 1
		}
		g.Count += count
		if t.After(g.Last) {
			g.Last, g.Message = t, strings.TrimSpace(e.Message)
		}
	}
	out := make([]Worry, 0, len(groups))
	for _, g := range groups {
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Last.After(out[j].Last)
	})
	return out
}

func renderCalm(out io.Writer, worries []Worry, top int, since time.Duration) {
	if len(worries) == 0 {
		fmt.Fprintf(out, "Nothing to worry about in the last %s. Breathe.\n", since)
		return
	}
	var total int32
	for _, w := range worries {
		total += w.Count
	}
	fmt.Fprintf(out, "%d warnings in the last %s come down to %d things.", total, since, len(worries))
	if len(worries) > top {
		fmt.Fprintf(out, " The %d that deserve attention first:", top)
		worries = worries[:top]
	}
	fmt.Fprintln(out)
	for i, w := range worries {
		msg := w.Message
		if len(msg) > 140 {
			msg = msg[:137] + "..."
		}
		fmt.Fprintf(out, "\n%d. %s/%s: %s x%d, last %s ago\n   %s\n",
			i+1, w.Namespace, w.Subject, w.Reason, w.Count, time.Since(w.Last).Round(time.Second), msg)
	}
}
