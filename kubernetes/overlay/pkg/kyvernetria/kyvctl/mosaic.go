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
	"k8s.io/client-go/kubernetes"
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
	// chosen is the allele the node records for itself (on kuberoot, the
	// node label kinit's choice is published as); empty when only the
	// version tells.
	chosen   string
	restarts int32
}

// ChoiceLabel is the label a kuberoot node carries for the kube-apiserver
// build its kinit chose at first boot: kuberoot's service alternatives,
// which run the mosaic there (distro/kuberoot/kyvernetria).
const ChoiceLabel = "alternative.kuberoot.dev/kube-apiserver"

func runMosaic(ctx context.Context, f cmdutil.Factory, out io.Writer) error {
	client, err := f.KubernetesClientSet()
	if err != nil {
		return err
	}
	cells, err := apiserverCells(ctx, client)
	if err != nil {
		return err
	}
	renderMosaic(out, cells)
	return nil
}

// apiserverCells asks every apiserver for its /version, so that each
// answers for itself rather than whichever one the load balancer picks:
// the kube-apiserver pods in kube-system through the pod proxy (kubeadm),
// or, where the control plane runs as node services and there are no such
// pods (kuberoot), each node labelled with its choice through the node
// proxy.
func apiserverCells(ctx context.Context, client kubernetes.Interface) ([]cell, error) {
	pods, err := client.CoreV1().Pods("kube-system").List(ctx, metav1.ListOptions{LabelSelector: "component=kube-apiserver"})
	if err != nil {
		return nil, err
	}
	if len(pods.Items) == 0 {
		return labelledCells(ctx, client)
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
		c.readVersion(raw, err)
		cells = append(cells, c)
	}
	return cells, nil
}

// labelledCells reads the mosaic from the nodes' own records of their
// choice, and each apiserver's version through the node proxy.
func labelledCells(ctx context.Context, client kubernetes.Interface) ([]cell, error) {
	cells, err := choiceCells(ctx, client)
	if err != nil {
		return nil, err
	}
	for i := range cells {
		raw, err := client.CoreV1().RESTClient().Get().
			AbsPath("/api/v1/nodes", "https:"+cells[i].node+":6443", "proxy", "version").
			DoRaw(ctx)
		cells[i].readVersion(raw, err)
	}
	return cells, nil
}

// choiceCells lists the nodes that record a kube-apiserver choice.
func choiceCells(ctx context.Context, client kubernetes.Interface) ([]cell, error) {
	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: ChoiceLabel})
	if err != nil {
		return nil, err
	}
	var cells []cell
	for _, n := range nodes.Items {
		cells = append(cells, cell{node: n.Name, chosen: n.Labels[ChoiceLabel]})
	}
	return cells, nil
}

// readVersion fills in what an apiserver's /version says.
func (c *cell) readVersion(raw []byte, err error) {
	if err != nil {
		c.note = "not answering: " + firstLine(err.Error())
		return
	}
	// gitVersion is the binary's version even when the apiserver runs with
	// --emulated-version; the emulated one is reported separately.
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
		fmt.Fprintln(out, "I can't see any apiserver pods in kube-system, nor nodes labelled "+ChoiceLabel+
			"; this doesn't look like a Kyvernetria control plane.")
		return
	}
	sort.Slice(cells, func(i, j int) bool { return cells[i].node < cells[j].node })
	versions := make([]string, len(cells))
	silent, recorded := 0, 0
	for i, c := range cells {
		versions[i] = c.version
		if c.version == "" {
			silent++
		}
		if c.chosen != "" {
			recorded++
		}
	}
	alleles, minors := Alleles(versions)
	counts := map[string]int{}
	for i := range cells {
		cells[i].allele = alleles[i]
		if cells[i].chosen != "" {
			// The node's own record beats an inference from versions.
			cells[i].allele = cells[i].chosen
		}
		counts[cells[i].allele]++
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
	expressed := 0
	for _, a := range []string{"Xm", "Xp"} {
		if counts[a] > 0 {
			expressed++
		}
	}
	switch {
	case recorded == len(cells) && expressed == 2:
		fmt.Fprintf(out, "\nMosaic: %d Xm, %d Xp, as each node chose at first boot. A bug in either build leaves the other half serving.\n", counts["Xm"], counts["Xp"])
	case recorded == len(cells) && len(cells) == 1:
		fmt.Fprintln(out, "\nOne control-plane node, one allele: no mosaic protection. Add control-plane nodes for it.")
	case recorded == len(cells):
		fmt.Fprintf(out, "\nEvery node chose %s, so a common-mode bug would hit them all. The choice is random per node; more control-plane nodes make a mosaic likelier.\n", cells[0].allele)
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
