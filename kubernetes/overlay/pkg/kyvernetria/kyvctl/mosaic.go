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
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
)

func newMosaicCommand(f cmdutil.Factory, streams genericiooptions.IOStreams) *cobra.Command {
	return &cobra.Command{
		Use:   "mosaic",
		Short: "Show which allele each control-plane node expresses",
		Long: "Every Kyvernetria control-plane node carries two builds of the control plane,\n" +
			"Xm and Xp, made from two different Kubernetes minor releases. Each node\n" +
			"silences one at first boot, at random and for good, like X-inactivation;\n" +
			"a crash-looping allele is escaped by switching to the other. Together the\n" +
			"nodes form a mosaic, so a bug in one build never takes out every apiserver.",
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			cmdutil.CheckErr(runMosaic(cmd.Context(), f, streams.Out))
		},
	}
}

type cell struct {
	node, version, allele, note string
	restarts                    int32
}

func runMosaic(ctx context.Context, f cmdutil.Factory, out io.Writer) error {
	client, err := f.KubernetesClientSet()
	if err != nil {
		return err
	}
	pods, err := client.CoreV1().Pods("kube-system").List(ctx, metav1.ListOptions{LabelSelector: "component=kube-apiserver"})
	if err != nil {
		return err
	}
	var cells []cell
	for _, pod := range pods.Items {
		c := cell{node: pod.Spec.NodeName}
		for _, cs := range pod.Status.ContainerStatuses {
			c.restarts += cs.RestartCount
		}
		raw, err := client.CoreV1().RESTClient().Get().
			AbsPath("/api/v1/namespaces/kube-system/pods", "https:"+pod.Name+":6443", "proxy", "version").
			DoRaw(ctx)
		if err != nil {
			c.note = "not answering: " + firstLine(err.Error())
		} else {
			var v struct {
				GitVersion string `json:"gitVersion"`
			}
			if err := json.Unmarshal(raw, &v); err != nil {
				c.note = "unreadable /version"
			}
			c.version = v.GitVersion
		}
		cells = append(cells, c)
	}
	renderMosaic(out, cells)
	return nil
}

// AlleleOf names the allele a version belongs to: the newer minor is Xm,
// the older Xp.
func AlleleOf(version, newest string) string {
	if version == "" {
		return "?"
	}
	if minor(version) == minor(newest) {
		return "Xm"
	}
	return "Xp"
}

func minor(version string) string {
	parts := strings.SplitN(strings.TrimPrefix(version, "v"), ".", 3)
	if len(parts) < 2 {
		return version
	}
	return parts[0] + "." + parts[1]
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func renderMosaic(out io.Writer, cells []cell) {
	if len(cells) == 0 {
		fmt.Fprintln(out, "I can't see any apiserver pods in kube-system; this doesn't look like a kubeadm-style control plane.")
		return
	}
	sort.Slice(cells, func(i, j int) bool { return cells[i].node < cells[j].node })
	newest := ""
	for _, c := range cells {
		if c.version != "" && (newest == "" || minor(c.version) > minor(newest)) {
			newest = c.version
		}
	}
	counts := map[string]int{}
	for i := range cells {
		cells[i].allele = AlleleOf(cells[i].version, newest)
		counts[cells[i].allele]++
	}
	fmt.Fprintf(out, "%-28s %-6s %-26s %s\n", "NODE", "ALLELE", "VERSION", "NOTE")
	for _, c := range cells {
		note := c.note
		if note == "" && c.restarts > 0 {
			note = fmt.Sprintf("%d restarts (an escape switches allele after repeated crashes)", c.restarts)
		}
		fmt.Fprintf(out, "%-28s %-6s %-26s %s\n", c.node, c.allele, c.version, note)
	}
	switch {
	case counts["Xm"] > 0 && counts["Xp"] > 0:
		fmt.Fprintf(out, "\nMosaic: %d Xm, %d Xp. A bug in either build leaves the other half serving.\n", counts["Xm"], counts["Xp"])
	case len(cells) == 1:
		fmt.Fprintln(out, "\nOne control-plane node, one allele: no mosaic protection. Add control-plane nodes for it.")
	default:
		fmt.Fprintln(out, "\nEvery node expresses the same allele (it happens by chance). A common-mode bug would hit them all.")
	}
}
