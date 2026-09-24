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
	"k8s.io/apimachinery/pkg/util/version"
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
	node, version, emulated, allele, note string
	restarts                              int32
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
			// gitVersion is the binary's version even when the apiserver
			// runs with --emulated-version; the emulated one is reported
			// separately.
			var v struct {
				GitVersion     string `json:"gitVersion"`
				EmulationMajor string `json:"emulationMajor"`
				EmulationMinor string `json:"emulationMinor"`
			}
			if err := json.Unmarshal(raw, &v); err != nil {
				c.note = "unreadable /version"
			}
			c.version = v.GitVersion
			if v.EmulationMajor != "" && v.EmulationMinor != "" {
				c.emulated = v.EmulationMajor + "." + v.EmulationMinor
			}
		}
		cells = append(cells, c)
	}
	renderMosaic(out, cells)
	return nil
}

// minorOf parses the major.minor of a version; ok is false for anything
// unparseable.
func minorOf(v string) (major, minor uint, ok bool) {
	parsed, err := version.ParseGeneric(v)
	if err != nil {
		return 0, 0, false
	}
	return parsed.Major(), parsed.Minor(), true
}

// Alleles names the allele each version belongs to. The two builds come
// from two different minor releases: the newest minor among the answering
// nodes is Xm, older ones are Xp. With only one minor answering there is no
// telling which allele it is, and every answering version gets "?"; so do
// versions that can't be parsed.
func Alleles(versions []string) (alleles []string, minors int) {
	type mm struct{ major, minor uint }
	seen := map[mm]bool{}
	var newest mm
	parsed := make([]*mm, len(versions))
	for i, v := range versions {
		major, minor, ok := minorOf(v)
		if !ok {
			continue
		}
		m := mm{major, minor}
		parsed[i] = &m
		seen[m] = true
		if m.major > newest.major || m.major == newest.major && m.minor > newest.minor {
			newest = m
		}
	}
	alleles = make([]string, len(versions))
	for i, m := range parsed {
		switch {
		case m == nil || len(seen) < 2:
			alleles[i] = "?"
		case *m == newest:
			alleles[i] = "Xm"
		default:
			alleles[i] = "Xp"
		}
	}
	return alleles, len(seen)
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
	versions := make([]string, len(cells))
	silent := 0
	for i, c := range cells {
		versions[i] = c.version
		if c.version == "" {
			silent++
		}
	}
	alleles, minors := Alleles(versions)
	counts := map[string]int{}
	for i := range cells {
		cells[i].allele = alleles[i]
		counts[alleles[i]]++
	}
	fmt.Fprintf(out, "%-28s %-6s %-26s %s\n", "NODE", "ALLELE", "VERSION", "NOTE")
	for _, c := range cells {
		var notes []string
		if c.note != "" {
			notes = append(notes, c.note)
		}
		if major, minor, ok := minorOf(c.version); ok && c.emulated != "" && c.emulated != fmt.Sprintf("%d.%d", major, minor) {
			notes = append(notes, "emulating "+c.emulated)
		}
		if c.restarts > 0 {
			notes = append(notes, fmt.Sprintf("%d restarts (an escape switches allele after repeated crashes)", c.restarts))
		}
		note := strings.Join(notes, "; ")
		fmt.Fprintf(out, "%-28s %-6s %-26s %s\n", c.node, c.allele, c.version, note)
	}
	switch {
	case silent > 0:
		fmt.Fprintf(out, "\n%d of %d nodes aren't answering, so I can't tell the whole mosaic yet.\n", silent, len(cells))
	case minors >= 2:
		fmt.Fprintf(out, "\nMosaic: %d Xm, %d Xp. A bug in either build leaves the other half serving.\n", counts["Xm"], counts["Xp"])
	case len(cells) == 1:
		fmt.Fprintln(out, "\nOne control-plane node, one allele: no mosaic protection. Add control-plane nodes for it.")
	default:
		fmt.Fprintln(out, "\nEvery node runs the same minor, so I can't tell which allele it is (only one minor answers). "+
			"Either way they all express the same build, and a common-mode bug would hit them all.")
	}
}
