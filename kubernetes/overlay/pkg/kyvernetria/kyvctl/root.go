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

// Package kyvctl is kubectl with the Kyvernetria commands added. Every
// kubectl command works unchanged; errors are rendered for a person.
package kyvctl

import (
	"os"

	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	kubectlcmd "k8s.io/kubectl/pkg/cmd"
	"k8s.io/kubectl/pkg/cmd/plugin"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
)

// ownCommands are added after kubectl builds its tree, so kubectl must not
// mistake them for plugins (it looks plugins up while it is being built).
var ownCommands = map[string]bool{
	"remember": true, "relationships": true, "rels": true, "exclude": true,
	"include": true, "calm": true, "diagnose": true, "mosaic": true,
}

// kubectlArguments hides the command line from kubectl's plugin lookup when
// it invokes one of kyvctl's own commands.
func kubectlArguments(args []string) []string {
	for _, a := range args[1:] {
		if ownCommands[a] {
			return args[:1]
		}
	}
	return args
}

// NewCommand builds the kyvctl root command.
func NewCommand(streams genericiooptions.IOStreams) *cobra.Command {
	configFlags := genericclioptions.NewConfigFlags(true).
		WithDeprecatedPasswordFlag().WithDiscoveryBurst(300).WithDiscoveryQPS(50.0).
		WithWarningPrinter(streams)
	root := kubectlcmd.NewDefaultKubectlCommandWithArgs(kubectlcmd.KubectlOptions{
		PluginHandler: kubectlcmd.NewDefaultPluginHandler(plugin.ValidPluginFilenamePrefixes),
		Arguments:     kubectlArguments(os.Args),
		ConfigFlags:   configFlags,
		IOStreams:     streams,
	})
	root.Use = "kyvctl"
	root.Short = "kyvctl controls a Kyvernetria (a.k.a. Femenetes) cluster"
	root.Long = "kyvctl is kubectl for Kyvernetria (a.k.a. Femenetes). Every kubectl command works;\n" +
		"the commands below are what Kyvernetria adds. See docs/RESEARCH.md for why each exists."

	f := cmdutil.NewFactory(cmdutil.NewMatchVersionFlags(configFlags))
	for _, c := range []*cobra.Command{
		newRememberCommand(f, streams),
		newRelationshipsCommand(f, streams),
		newExcludeCommand(f, streams, true),
		newExcludeCommand(f, streams, false),
		newCalmCommand(f, streams),
		newDiagnoseCommand(f, streams),
		newMosaicCommand(f, streams),
	} {
		root.AddCommand(c)
	}
	return root
}
