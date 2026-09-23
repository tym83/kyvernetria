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

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	"k8s.io/kubernetes/pkg/kyvernetria/controller/relationships"
)

func newRelationshipsCommand(f cmdutil.Factory, streams genericiooptions.IOStreams) *cobra.Command {
	var all bool
	c := &cobra.Command{
		Use:     "relationships [NAME]",
		Aliases: []string{"rels"},
		Short:   "Show who serves whom and who talks to whom",
		Long: "Relationships between services are first-class objects in Kyvernetria\n" +
			"(kubectl get relationships works too). This command draws them as a map;\n" +
			"with NAME it shows only the relationships NAME takes part in.",
		Example: "  kyvctl relationships\n  kyvctl relationships api -n shop\n  kyvctl rels -A",
		Args:    cobra.MaximumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			focus := ""
			if len(args) == 1 {
				focus = args[0]
			}
			cmdutil.CheckErr(runRelationships(cmd.Context(), f, streams.Out, all, focus))
		},
	}
	c.Flags().BoolVarP(&all, "all-namespaces", "A", false, "Show relationships in every namespace")
	return c
}

func runRelationships(ctx context.Context, f cmdutil.Factory, out io.Writer, all bool, focus string) error {
	ns, _, err := f.ToRawKubeConfigLoader().Namespace()
	if err != nil {
		return err
	}
	if all {
		ns = metav1.NamespaceAll
	}
	dyn, err := f.DynamicClient()
	if err != nil {
		return err
	}
	list, err := dyn.Resource(relationships.GVR).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	return renderRelationships(out, list.Items, focus)
}

type edge struct {
	namespace, from, rel, to, evidence string
}

func edgeOf(u unstructured.Unstructured) edge {
	get := func(fields ...string) string {
		s, _, _ := unstructured.NestedString(u.Object, fields...)
		return s
	}
	to := shortKind(get("spec", "to", "kind")) + "/" + get("spec", "to", "name")
	if toNS := get("spec", "to", "namespace"); toNS != "" && toNS != u.GetNamespace() {
		to += " (" + toNS + ")"
	}
	return edge{
		namespace: u.GetNamespace(),
		from:      shortKind(get("spec", "from", "kind")) + "/" + get("spec", "from", "name"),
		rel:       get("spec", "type"),
		to:        to,
		evidence:  get("spec", "evidence"),
	}
}

func shortKind(kind string) string {
	switch kind {
	case "Deployment":
		return "deploy"
	case "StatefulSet":
		return "sts"
	case "DaemonSet":
		return "ds"
	case "Service":
		return "svc"
	}
	return kind
}

func renderRelationships(out io.Writer, items []unstructured.Unstructured, focus string) error {
	var edges []edge
	for _, item := range items {
		e := edgeOf(item)
		if focus == "" || hasName(e.from, focus) || hasName(e.to, focus) {
			edges = append(edges, e)
		}
	}
	if len(edges) == 0 {
		fmt.Fprintln(out, "No relationships yet. Services and workloads here don't refer to each other.")
		return nil
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].namespace != edges[j].namespace {
			return edges[i].namespace < edges[j].namespace
		}
		if edges[i].from != edges[j].from {
			return edges[i].from < edges[j].from
		}
		return edges[i].to < edges[j].to
	})
	ns := "\x00"
	for _, e := range edges {
		if e.namespace != ns {
			ns = e.namespace
			fmt.Fprintf(out, "%s\n", ns)
		}
		fmt.Fprintf(out, "  %-24s --%s--> %s\n", e.from, e.rel, e.to)
	}
	return nil
}

func hasName(ref, name string) bool {
	for i := 0; i < len(ref); i++ {
		if ref[i] == '/' {
			rest := ref[i+1:]
			if len(rest) >= len(name) && rest[:len(name)] == name && (len(rest) == len(name) || rest[len(name)] == ' ') {
				return true
			}
		}
	}
	return false
}
