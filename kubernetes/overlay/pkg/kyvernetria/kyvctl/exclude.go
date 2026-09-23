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
	"strings"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	"k8s.io/kubernetes/pkg/kyvernetria"
)

func newExcludeCommand(f cmdutil.Factory, streams genericiooptions.IOStreams, exclude bool) *cobra.Command {
	use, short := "include pod/NAME", "Let an excluded pod receive Service traffic again"
	if exclude {
		use, short = "exclude pod/NAME", "Stop sending Service traffic to a pod without killing it"
	}
	return &cobra.Command{
		Use:   use,
		Short: short,
		Long: "Kyvernetria doesn't use force (lower physical aggression), but it can leave a\n" +
			"misbehaving pod out: an excluded pod keeps running, is still Ready, and is\n" +
			"simply no longer in any Service's endpoints (indirect aggression, where the\n" +
			"sex difference is small or absent).",
		Example: "  kyvctl exclude pod/api-7f9c-x2kq\n  kyvctl include pod/api-7f9c-x2kq",
		Args:    cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			cmdutil.CheckErr(runExclude(cmd.Context(), f, streams.Out, args[0], exclude))
		},
	}
}

func runExclude(ctx context.Context, f cmdutil.Factory, out io.Writer, target string, exclude bool) error {
	name := strings.TrimPrefix(strings.TrimPrefix(target, "pod/"), "pods/")
	if strings.Contains(name, "/") {
		return fmt.Errorf("only pods can be excluded, got %q", target)
	}
	ns, _, err := f.ToRawKubeConfigLoader().Namespace()
	if err != nil {
		return err
	}
	client, err := f.KubernetesClientSet()
	if err != nil {
		return err
	}
	var value interface{} // nil removes the annotation
	if exclude {
		value = "true"
	}
	patch, err := json.Marshal(map[string]interface{}{
		"metadata": map[string]interface{}{"annotations": map[string]interface{}{kyvernetria.ExcludedAnnotation: value}},
	})
	if err != nil {
		return err
	}
	if _, err := client.CoreV1().Pods(ns).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return err
	}
	if exclude {
		fmt.Fprintf(out, "pod/%s is left out. It keeps running and stays Ready, but no Service sends it traffic.\n"+
			"Nobody killed it. Bring it back with: kyvctl include pod/%s\n", name, name)
	} else {
		fmt.Fprintf(out, "pod/%s is back in. Services will send it traffic again.\n", name)
	}
	return nil
}
