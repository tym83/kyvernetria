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

	"github.com/spf13/cobra"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	"k8s.io/kubernetes/pkg/kyvernetria"
	"k8s.io/kubernetes/pkg/kyvernetria/admission/immunity"
)

func newDiagnoseCommand(f cmdutil.Factory, streams genericiooptions.IOStreams) *cobra.Command {
	c := &cobra.Command{
		Use:   "diagnose",
		Short: "Diagnose conditions the cluster is prone to",
	}
	c.AddCommand(&cobra.Command{
		Use:   "autoimmune",
		Short: "Find workloads the immunity plugin is attacking although they are probably your own",
		Long: "A stronger immune response comes with more autoimmunity: autoimmune disease\n" +
			"is about twice as common in women. Kyvernetria's Immunity admission plugin is\n" +
			"strict too, and sometimes rejects legitimate infrastructure. This lists every\n" +
			"workload it is currently rejecting and flags the ones that look like self.",
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			cmdutil.CheckErr(runAutoimmune(cmd.Context(), f, streams.Out))
		},
	})
	return c
}

// Rejection is one workload the immune system is attacking.
type Rejection struct {
	Namespace  string
	Object     string
	Antigens   string
	Count      int32
	LikelySelf bool
}

// infrastructure namespaces are the cluster's own tissue in most clusters.
var infrastructure = regexp.MustCompile(`^(kube-.*|.*-system|monitoring|observability|logging|ingress.*|cert-manager|cilium.*|calico.*|tigera.*|metallb.*|longhorn.*|rook.*|piraeus.*|linstor.*|flux.*|argocd|cozy-.*|gpu-operator|nvidia.*)$`)

// Rejections extracts immunity rejections from events. Namespaces already
// marked self are skipped: those rejections are stale.
func Rejections(events []v1.Event, selfNamespaces map[string]bool) []Rejection {
	groups := map[string]*Rejection{}
	for _, e := range events {
		i := strings.Index(e.Message, immunity.MessagePrefix+": ")
		if i < 0 || selfNamespaces[e.Namespace] {
			continue
		}
		antigens := e.Message[i+len(immunity.MessagePrefix)+2:]
		if j := strings.Index(antigens, ". If this is your own workload"); j >= 0 {
			antigens = antigens[:j]
		}
		object := strings.ToLower(e.InvolvedObject.Kind) + "/" + e.InvolvedObject.Name
		key := e.Namespace + "/" + object
		r, ok := groups[key]
		if !ok {
			r = &Rejection{Namespace: e.Namespace, Object: object, Antigens: antigens,
				LikelySelf: infrastructure.MatchString(e.Namespace)}
			groups[key] = r
		}
		count := e.Count
		if count < 1 {
			count = 1
		}
		r.Count += count
	}
	out := make([]Rejection, 0, len(groups))
	for _, r := range groups {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].LikelySelf != out[j].LikelySelf {
			return out[i].LikelySelf
		}
		return out[i].Namespace+out[i].Object < out[j].Namespace+out[j].Object
	})
	return out
}

func runAutoimmune(ctx context.Context, f cmdutil.Factory, out io.Writer) error {
	client, err := f.KubernetesClientSet()
	if err != nil {
		return err
	}
	events, err := client.CoreV1().Events(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	namespaces, err := client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{LabelSelector: kyvernetria.SelfLabel + "=true"})
	if err != nil {
		return err
	}
	self := map[string]bool{}
	for _, ns := range namespaces.Items {
		self[ns.Name] = true
	}
	renderAutoimmune(out, Rejections(events.Items, self))
	return nil
}

func renderAutoimmune(out io.Writer, rejections []Rejection) {
	if len(rejections) == 0 {
		fmt.Fprintln(out, "No autoimmune activity: the immune system isn't rejecting anything right now.")
		return
	}
	selfCount := 0
	for _, r := range rejections {
		if r.LikelySelf {
			selfCount++
		}
	}
	fmt.Fprintf(out, "The immune system is rejecting %d workloads; %d look like your own tissue.\n", len(rejections), selfCount)
	fixes := map[string]bool{}
	for _, r := range rejections {
		verdict := "foreign?"
		if r.LikelySelf {
			verdict = "probably self (autoimmune)"
			fixes[r.Namespace] = true
		}
		fmt.Fprintf(out, "\n  %s/%s  x%d  %s\n    %s\n", r.Namespace, r.Object, r.Count, verdict, r.Antigens)
	}
	if len(fixes) > 0 {
		names := make([]string, 0, len(fixes))
		for ns := range fixes {
			names = append(names, ns)
		}
		sort.Strings(names)
		fmt.Fprintln(out, "\nTo restore tolerance for what is yours:")
		for _, ns := range names {
			fmt.Fprintf(out, "  kyvctl label namespace %s %s=true\n", ns, kyvernetria.SelfLabel)
		}
	}
}
