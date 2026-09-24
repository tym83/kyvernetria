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
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/component-base/logs"
	kubectlcmd "k8s.io/kubectl/pkg/cmd"
	"k8s.io/kubectl/pkg/cmd/plugin"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
)

// ownCommands are added after kubectl builds its tree.
var ownCommands = map[string]bool{
	"remember": true, "relationships": true, "rels": true, "exclude": true,
	"include": true, "calm": true, "diagnose": true, "mosaic": true,
}

// invokesOwnCommand reports whether the command line runs one of kyvctl's
// own commands, i.e. its first positional argument names one. kubectl would
// otherwise look it up as a plugin while building its tree and fail.
func invokesOwnCommand(args []string) bool {
	candidate := false
	for _, a := range args[1:] {
		candidate = candidate || ownCommands[a]
	}
	if !candidate {
		return false // the common case: no need to learn kubectl's flags
	}
	return ownCommands[firstPositional(args[1:], globalFlags())]
}

// globalFlags are the flags kubectl accepts before a command name.
func globalFlags() *pflag.FlagSet {
	probe := kubectlcmd.NewKubectlCommand(kubectlcmd.KubectlOptions{
		Arguments: []string{"kubectl"},
		IOStreams: genericiooptions.IOStreams{In: strings.NewReader(""), Out: io.Discard, ErrOut: io.Discard},
	})
	fs := pflag.NewFlagSet("global", pflag.ContinueOnError)
	fs.AddFlagSet(probe.PersistentFlags())
	logs.AddFlags(fs)
	return fs
}

// firstPositional returns the first argument that is neither a flag nor a
// flag's value: --flag value, --flag=value, -n value, -nvalue and boolean
// flags are all skipped.
func firstPositional(args []string, flags *pflag.FlagSet) string {
	if p := positionals(args, flags); len(p) > 0 {
		return p[0]
	}
	return ""
}

// positionals returns every argument that is neither a flag nor a flag's
// value, in order; everything after "--" is positional.
func positionals(args []string, flags *pflag.FlagSet) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return append(out, args[i+1:]...)
		case strings.HasPrefix(a, "--"):
			name := strings.ReplaceAll(a[2:], "_", "-")
			if strings.Contains(name, "=") {
				continue
			}
			if f := flags.Lookup(name); f != nil && f.NoOptDefVal == "" {
				i++ // the value follows
			}
		case strings.HasPrefix(a, "-") && len(a) > 1:
			// A group of shorthands: the first one that takes a value
			// takes the rest of the group, or the next argument.
			for j := 1; j < len(a); j++ {
				f := flags.ShorthandLookup(a[j : j+1])
				if f == nil || f.NoOptDefVal != "" {
					continue
				}
				if j == len(a)-1 {
					i++
				}
				break
			}
		default:
			out = append(out, a)
		}
	}
	return out
}

// NewCommand builds the kyvctl root command.
func NewCommand(streams genericiooptions.IOStreams) *cobra.Command {
	return newCommand(streams, os.Args)
}

func newCommand(streams genericiooptions.IOStreams, args []string) *cobra.Command {
	configFlags := genericclioptions.NewConfigFlags(true).
		WithDeprecatedPasswordFlag().WithDiscoveryBurst(300).WithDiscoveryQPS(50.0).
		WithWarningPrinter(streams)
	opts := kubectlcmd.KubectlOptions{
		PluginHandler: kubectlcmd.NewDefaultPluginHandler(plugin.ValidPluginFilenamePrefixes),
		Arguments:     args,
		ConfigFlags:   configFlags,
		IOStreams:     streams,
	}
	if invokesOwnCommand(args) {
		// kubectl still sees the whole command line, so kuberc (aliases,
		// defaults, credential plugin policy) applies; only the plugin
		// lookup, which would reject a command kubectl doesn't know, is off.
		opts.PluginHandler = nil
	}
	root := kubectlcmd.NewDefaultKubectlCommandWithArgs(opts)
	root.Use = "kyvctl"
	root.Short = "kyvctl controls a Kyvernetria (a.k.a. Femenetes) cluster"
	root.Long = "kyvctl is kubectl for Kyvernetria (a.k.a. Femenetes). Every kubectl command works;\n" +
		"the commands below are what Kyvernetria adds. See docs/RESEARCH.md for why each exists."

	// A kuberc alias may not shadow a kyvctl command.
	for _, c := range root.Commands() {
		if ownCommands[c.Name()] {
			root.RemoveCommand(c)
		}
	}
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
