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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/client-go/kubernetes"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	"k8s.io/kubernetes/pkg/kyvernetria/placement"
)

func newRememberCommand(f cmdutil.Factory, streams genericiooptions.IOStreams) *cobra.Command {
	return &cobra.Command{
		Use:   "remember TYPE/NAME",
		Short: "Tell the story of a workload: everything that happened to it and its pods",
		Long: "Kyvernetria keeps events for 30 days instead of one hour (episodic memory).\n" +
			"remember shows them as one timeline for the object, its ReplicaSets and its pods,\n" +
			"including pods that no longer exist, plus the nodes it has lived on.",
		Example: "  kyvctl remember deploy/api\n  kyvctl remember sts/db -n shop",
		Args:    cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			cmdutil.CheckErr(runRemember(cmd.Context(), f, streams.Out, args[0]))
		},
	}
}

func runRemember(ctx context.Context, f cmdutil.Factory, out io.Writer, target string) error {
	kind, name, ok := strings.Cut(target, "/")
	if !ok {
		return fmt.Errorf("I need TYPE/NAME, for example deploy/api")
	}
	ns, _, err := f.ToRawKubeConfigLoader().Namespace()
	if err != nil {
		return err
	}
	client, err := f.KubernetesClientSet()
	if err != nil {
		return err
	}
	story, err := collectStory(ctx, client, ns, normalizeKind(kind), name)
	if err != nil {
		return err
	}
	story.render(out)
	return nil
}

func normalizeKind(kind string) string {
	switch strings.ToLower(kind) {
	case "deploy", "deploys", "deployment", "deployments":
		return "Deployment"
	case "sts", "statefulset", "statefulsets":
		return "StatefulSet"
	case "ds", "daemonset", "daemonsets":
		return "DaemonSet"
	case "rs", "replicaset", "replicasets":
		return "ReplicaSet"
	case "po", "pod", "pods":
		return "Pod"
	case "no", "node", "nodes":
		return "Node"
	case "svc", "service", "services":
		return "Service"
	}
	return kind
}

type story struct {
	title  string
	homes  []string
	events []v1.Event
}

// collectStory gathers events for the object and its descendants. Pods and
// ReplicaSets that are already gone are recognized by name prefix, because
// their UIDs are gone with them.
func collectStory(ctx context.Context, client kubernetes.Interface, ns, kind, name string) (*story, error) {
	s := &story{title: fmt.Sprintf("%s %s/%s", kind, ns, name)}
	uids := map[types.UID]bool{}
	prefixes := []string{}
	apps := client.AppsV1()
	switch kind {
	case "Deployment":
		d, err := apps.Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		uids[d.UID] = true
		s.homes = placement.Decode(d.Annotations)
		prefixes = append(prefixes, name+"-")
	case "StatefulSet":
		st, err := apps.StatefulSets(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		uids[st.UID] = true
		s.homes = placement.Decode(st.Annotations)
		prefixes = append(prefixes, name+"-")
	case "DaemonSet":
		d, err := apps.DaemonSets(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		uids[d.UID] = true
		prefixes = append(prefixes, name+"-")
	case "ReplicaSet":
		rs, err := apps.ReplicaSets(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		uids[rs.UID] = true
		s.homes = placement.Decode(rs.Annotations)
		prefixes = append(prefixes, name+"-")
	case "Node":
		ns = metav1.NamespaceAll
		s.title = "Node " + name
	}

	list, err := client.CoreV1().Events(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for _, e := range list.Items {
		o := e.InvolvedObject
		if uids[o.UID] || (o.Kind == kind && o.Name == name) || hasAnyPrefix(o.Name, prefixes) && (o.Kind == "Pod" || o.Kind == "ReplicaSet") {
			s.events = append(s.events, e)
		}
	}
	sort.SliceStable(s.events, func(i, j int) bool { return eventTime(s.events[i]).Before(eventTime(s.events[j])) })
	return s, nil
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func eventTime(e v1.Event) time.Time {
	switch {
	case !e.LastTimestamp.IsZero():
		return e.LastTimestamp.Time
	case !e.EventTime.IsZero():
		return e.EventTime.Time
	}
	return e.FirstTimestamp.Time
}

func (s *story) render(out io.Writer) {
	fmt.Fprintf(out, "The story of %s\n", s.title)
	if len(s.homes) > 0 {
		fmt.Fprintf(out, "Lived on: %s\n", strings.Join(s.homes, ", "))
	}
	if len(s.events) == 0 {
		fmt.Fprintln(out, "\nNothing happened to it that I remember. Quiet life.")
		return
	}
	day := ""
	for _, e := range s.events {
		t := eventTime(e).Local()
		if d := t.Format("Mon 2 Jan 2006"); d != day {
			day = d
			fmt.Fprintf(out, "\n%s\n", d)
		}
		count := ""
		if e.Count > 1 {
			count = fmt.Sprintf(" (x%d)", e.Count)
		}
		mark := " "
		if e.Type == v1.EventTypeWarning {
			mark = "!"
		}
		fmt.Fprintf(out, "  %s %s %s %s: %s%s\n", t.Format("15:04"), mark,
			strings.ToLower(e.InvolvedObject.Kind)+"/"+e.InvolvedObject.Name, e.Reason, strings.TrimSpace(e.Message), count)
	}
}
