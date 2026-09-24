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
	"k8s.io/apimachinery/pkg/fields"
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

// pageSize bounds each list request; long histories are paged.
const pageSize = 500

// collectStory gathers events for the object and its descendants. Live
// descendants are recognized by owner UID; pods and ReplicaSets that are
// already gone are recognized by the generated names their controller
// gives them (the same patterns as calm), because their UIDs are gone with
// them.
func collectStory(ctx context.Context, client kubernetes.Interface, ns, kind, name string) (*story, error) {
	s := &story{title: fmt.Sprintf("%s %s/%s", kind, ns, name)}
	uids := map[types.UID]bool{}
	apps := client.AppsV1()
	var descendantKinds []string // event kinds that may concern a descendant
	switch kind {
	case "Deployment":
		d, err := apps.Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		uids[d.UID] = true
		s.homes = placement.Decode(d.Annotations)
		descendantKinds = []string{"ReplicaSet", "Pod"}
	case "StatefulSet":
		st, err := apps.StatefulSets(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		uids[st.UID] = true
		s.homes = ordinalHomes(name, placement.DecodeOrdinals(st.Annotations))
		descendantKinds = []string{"Pod"}
	case "DaemonSet":
		d, err := apps.DaemonSets(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		uids[d.UID] = true
		descendantKinds = []string{"Pod"}
	case "ReplicaSet":
		rs, err := apps.ReplicaSets(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		uids[rs.UID] = true
		s.homes = placement.Decode(rs.Annotations)
		descendantKinds = []string{"Pod"}
	case "Node":
		ns = metav1.NamespaceAll
		s.title = "Node " + name
	}
	if len(descendantKinds) > 0 {
		if err := addLiveDescendants(ctx, client, ns, kind, uids); err != nil {
			return nil, err
		}
	}

	own := fields.Set{"involvedObject.kind": kind, "involvedObject.name": name}.AsSelector().String()
	err := eachEvent(ctx, client, ns, own, func(e v1.Event) {
		if e.InvolvedObject.Kind == kind && e.InvolvedObject.Name == name {
			s.events = append(s.events, e)
		}
	})
	if err != nil {
		return nil, err
	}
	for _, dk := range descendantKinds {
		sel := fields.OneTermEqualSelector("involvedObject.kind", dk).String()
		err := eachEvent(ctx, client, ns, sel, func(e v1.Event) {
			o := e.InvolvedObject
			if o.Kind == dk && (uids[o.UID] || Descends(kind, name, o.Kind, o.Name)) {
				s.events = append(s.events, e)
			}
		})
		if err != nil {
			return nil, err
		}
	}
	sort.SliceStable(s.events, func(i, j int) bool { return eventTime(s.events[i]).Before(eventTime(s.events[j])) })
	return s, nil
}

// addLiveDescendants adds the UIDs of the ReplicaSets and pods the object
// owns, directly or through its ReplicaSets.
func addLiveDescendants(ctx context.Context, client kubernetes.Interface, ns, kind string, uids map[types.UID]bool) error {
	owned := func(refs []metav1.OwnerReference) bool {
		for _, r := range refs {
			if uids[r.UID] {
				return true
			}
		}
		return false
	}
	if kind == "Deployment" {
		opts := metav1.ListOptions{Limit: pageSize}
		for {
			list, err := client.AppsV1().ReplicaSets(ns).List(ctx, opts)
			if err != nil {
				return err
			}
			for _, rs := range list.Items {
				if owned(rs.OwnerReferences) {
					uids[rs.UID] = true
				}
			}
			if opts.Continue = list.Continue; opts.Continue == "" {
				break
			}
		}
	}
	opts := metav1.ListOptions{Limit: pageSize}
	for {
		list, err := client.CoreV1().Pods(ns).List(ctx, opts)
		if err != nil {
			return err
		}
		for _, pod := range list.Items {
			if owned(pod.OwnerReferences) {
				uids[pod.UID] = true
			}
		}
		if opts.Continue = list.Continue; opts.Continue == "" {
			return nil
		}
	}
}

// eachEvent pages through the events matching fieldSelector.
func eachEvent(ctx context.Context, client kubernetes.Interface, ns, fieldSelector string, fn func(v1.Event)) error {
	opts := metav1.ListOptions{FieldSelector: fieldSelector, Limit: pageSize}
	for {
		list, err := client.CoreV1().Events(ns).List(ctx, opts)
		if err != nil {
			return err
		}
		for _, e := range list.Items {
			fn(e)
		}
		if opts.Continue = list.Continue; opts.Continue == "" {
			return nil
		}
	}
}

// Descends reports whether an object named objName of kind objKind carries
// the name its controller would generate as a descendant of kind/name:
// deploy api -> ReplicaSet api-7f9c8d6b5 -> Pod api-7f9c8d6b5-x2kq4,
// sts db -> Pod db-0, ds agent / rs api-7f9c8d6b5 -> Pod <name>-x2kq4.
// A plain prefix is not enough: api-gateway's pods are not api's.
func Descends(kind, name, objKind, objName string) bool {
	match := func(re *regexp.Regexp) bool {
		m := re.FindStringSubmatch(objName)
		return m != nil && m[1] == name
	}
	switch {
	case kind == "Deployment" && objKind == "ReplicaSet":
		return match(replicaSet)
	case kind == "Deployment" && objKind == "Pod":
		return match(deploymentPod)
	case kind == "StatefulSet" && objKind == "Pod":
		return match(ordinalPod)
	case (kind == "DaemonSet" || kind == "ReplicaSet") && objKind == "Pod":
		return match(generatedPod)
	}
	return false
}

func ordinalHomes(name string, homes map[int]string) []string {
	ords := make([]int, 0, len(homes))
	for o := range homes {
		ords = append(ords, o)
	}
	sort.Ints(ords)
	out := make([]string, 0, len(ords))
	for _, o := range ords {
		out = append(out, fmt.Sprintf("%s-%d on %s", name, o, homes[o]))
	}
	return out
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
