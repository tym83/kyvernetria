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
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
)

// checkupWindow is how far back a restart still counts as a sign.
const checkupWindow = time.Hour

func newCheckupCommand(f cmdutil.Factory, streams genericiooptions.IOStreams) *cobra.Command {
	var all bool
	var selector string
	cmd := &cobra.Command{
		Use:   "checkup [-A] [-l selector]",
		Short: "Look past Ready: pods that pass their checks but show signs of trouble",
		Long: "Ready is a surface check, and a check can only see what it was built to see.\n" +
			"checkup looks at Ready pods only, the ones nothing else flags, for what\n" +
			"Ready can hide: containers that restarted in the last hour, a last exit\n" +
			"that was an OOM kill, and readiness probes that cannot see the\n" +
			"application at all (none, an open port only, or a command that always\n" +
			"succeeds). Autism in girls and women is under-diagnosed partly because\n" +
			"it often presents in ways current criteria were not built to see,\n" +
			"camouflaging among them (Hull et al. 2020).",
		Example: "  kyvctl checkup\n  kyvctl checkup -A\n  kyvctl checkup -n shop -l app=api",
		Args:    cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			cmdutil.CheckErr(runCheckup(cmd.Context(), f, streams.Out, all, selector, time.Now()))
		},
	}
	cmd.Flags().BoolVarP(&all, "all-namespaces", "A", false, "check pods in every namespace")
	cmd.Flags().StringVarP(&selector, "selector", "l", "", "check only pods with these labels")
	return cmd
}

func runCheckup(ctx context.Context, f cmdutil.Factory, out io.Writer, all bool, selector string, now time.Time) error {
	ns := ""
	if !all {
		var err error
		if ns, _, err = f.ToRawKubeConfigLoader().Namespace(); err != nil {
			return err
		}
	}
	client, err := f.KubernetesClientSet()
	if err != nil {
		return err
	}
	pods, err := client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return err
	}
	renderCheckup(out, pods.Items, now)
	return nil
}

func renderCheckup(out io.Writer, pods []v1.Pod, now time.Time) {
	ready, flagged := 0, 0
	sort.Slice(pods, func(i, j int) bool {
		if pods[i].Namespace != pods[j].Namespace {
			return pods[i].Namespace < pods[j].Namespace
		}
		return pods[i].Name < pods[j].Name
	})
	for i := range pods {
		p := &pods[i]
		if !podReady(p) {
			continue
		}
		ready++
		signs := Checkup(p, now)
		if len(signs) == 0 {
			continue
		}
		flagged++
		fmt.Fprintf(out, "%s/%s is Ready, but:\n", p.Namespace, p.Name)
		for _, s := range signs {
			fmt.Fprintf(out, "  - %s\n", s)
		}
	}
	switch {
	case ready == 0:
		fmt.Fprintln(out, "No pod here is Ready, so there is nothing hiding behind Ready to look for.")
	case flagged == 0:
		fmt.Fprintf(out, "Looked past Ready on %d pods: nothing underneath.\n", ready)
	default:
		fmt.Fprintf(out, "\n%d of %d Ready pods show something Ready doesn't.\n", flagged, ready)
	}
}

func podReady(p *v1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == v1.PodReady {
			return c.Status == v1.ConditionTrue
		}
	}
	return false
}

// Checkup lists the signs of trouble a pod's Ready condition does not show.
func Checkup(p *v1.Pod, now time.Time) []string {
	var signs []string
	for _, cs := range p.Status.ContainerStatuses {
		t := cs.LastTerminationState.Terminated
		if t == nil {
			continue
		}
		if !t.FinishedAt.IsZero() && now.Sub(t.FinishedAt.Time) < checkupWindow {
			signs = append(signs, fmt.Sprintf("container %s restarted %s ago (%d restarts in all), after exiting with %d (%s)",
				cs.Name, now.Sub(t.FinishedAt.Time).Round(time.Second), cs.RestartCount, t.ExitCode, orUnknown(t.Reason)))
		} else if t.Reason == "OOMKilled" {
			signs = append(signs, fmt.Sprintf("container %s was last stopped by running out of memory", cs.Name))
		}
	}
	for _, c := range p.Spec.Containers {
		if why := blindProbe(c.ReadinessProbe); why != "" {
			signs = append(signs, fmt.Sprintf("container %s: %s, so Ready says little about whether it answers", c.Name, why))
		}
	}
	return signs
}

// blindProbe says why a readiness probe cannot see the application, or ""
// when it can.
func blindProbe(p *v1.Probe) string {
	switch {
	case p == nil:
		return "no readiness probe: Ready only means it started"
	case p.TCPSocket != nil:
		return "the readiness probe only opens a port"
	case p.Exec != nil && alwaysSucceeds(p.Exec.Command):
		return fmt.Sprintf("the readiness probe runs %q, which always succeeds", strings.Join(p.Exec.Command, " "))
	}
	return ""
}

func alwaysSucceeds(command []string) bool {
	switch strings.Join(command, " ") {
	case "true", "/bin/true", "/usr/bin/true", ":", "sh -c true", "sh -c exit 0", "/bin/sh -c true", "/bin/sh -c exit 0", "exit 0":
		return true
	}
	return false
}

func orUnknown(s string) string {
	if s == "" {
		return "no reason given"
	}
	return s
}
