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

package relationships

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
)

// Relationship types.
const (
	Serves  = "serves"
	TalksTo = "talks-to"
)

// Ref points at one party of a relationship.
type Ref struct {
	Kind      string
	Namespace string
	Name      string
	UID       types.UID
}

// Relationship is one edge of the cluster's social graph.
type Relationship struct {
	From     Ref
	To       Ref
	Type     string
	Evidence string
}

// Workload is the part of a Deployment, StatefulSet or DaemonSet that the
// graph needs.
type Workload struct {
	Ref
	Template v1.PodTemplateSpec
}

// Workloads flattens the workload kinds into one list.
func Workloads(deploys []*appsv1.Deployment, sts []*appsv1.StatefulSet, ds []*appsv1.DaemonSet) []Workload {
	var out []Workload
	for _, d := range deploys {
		out = append(out, Workload{Ref{"Deployment", d.Namespace, d.Name, d.UID}, d.Spec.Template})
	}
	for _, s := range sts {
		out = append(out, Workload{Ref{"StatefulSet", s.Namespace, s.Name, s.UID}, s.Spec.Template})
	}
	for _, d := range ds {
		out = append(out, Workload{Ref{"DaemonSet", d.Namespace, d.Name, d.UID}, d.Spec.Template})
	}
	return out
}

// Graph derives relationships from what the cluster already declares:
// a Service serves the workloads its selector matches, and a workload talks
// to every Service whose DNS name appears in its environment, arguments or
// command.
func Graph(services []*v1.Service, workloads []Workload) []Relationship {
	var out []Relationship
	for _, svc := range services {
		if len(svc.Spec.Selector) == 0 {
			continue
		}
		selector := labels.SelectorFromSet(svc.Spec.Selector)
		for _, w := range workloads {
			if w.Namespace == svc.Namespace && selector.Matches(labels.Set(w.Template.Labels)) {
				out = append(out, Relationship{
					From:     Ref{"Service", svc.Namespace, svc.Name, svc.UID},
					To:       w.Ref,
					Type:     Serves,
					Evidence: "selector " + labels.Set(svc.Spec.Selector).String(),
				})
			}
		}
	}
	for _, w := range workloads {
		texts := templateTexts(w.Template)
		for _, svc := range services {
			if evidence, ok := mentions(texts, svc, w.Namespace); ok {
				if svc.Namespace == w.Namespace && serves(out, svc, w.Ref) {
					continue // a service calling itself is not a relationship
				}
				out = append(out, Relationship{
					From:     w.Ref,
					To:       Ref{"Service", svc.Namespace, svc.Name, svc.UID},
					Type:     TalksTo,
					Evidence: evidence,
				})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return Name(out[i]) < Name(out[j]) })
	return out
}

func serves(rels []Relationship, svc *v1.Service, w Ref) bool {
	for _, r := range rels {
		if r.Type == Serves && r.From.Name == svc.Name && r.From.Namespace == svc.Namespace && r.To == w {
			return true
		}
	}
	return false
}

type text struct {
	where string
	value string
}

func templateTexts(t v1.PodTemplateSpec) []text {
	var out []text
	add := func(c v1.Container) {
		for _, e := range c.Env {
			if e.Value != "" {
				out = append(out, text{fmt.Sprintf("container %s env %s", c.Name, e.Name), e.Value})
			}
		}
		for i, a := range c.Args {
			out = append(out, text{fmt.Sprintf("container %s arg %d", c.Name, i), a})
		}
		for i, a := range c.Command {
			out = append(out, text{fmt.Sprintf("container %s command %d", c.Name, i), a})
		}
	}
	for _, c := range t.Spec.InitContainers {
		add(c)
	}
	for _, c := range t.Spec.Containers {
		add(c)
	}
	return out
}

// mentions reports whether any text addresses svc by a DNS name that
// resolves from namespace ns: the short name only works in the same
// namespace, the qualified forms work from anywhere.
func mentions(texts []text, svc *v1.Service, ns string) (string, bool) {
	names := []string{
		svc.Name + "." + svc.Namespace + ".svc.cluster.local",
		svc.Name + "." + svc.Namespace + ".svc",
		svc.Name + "." + svc.Namespace,
	}
	if svc.Namespace == ns {
		names = append(names, svc.Name)
	}
	for _, t := range texts {
		for _, n := range names {
			if hostPattern(n).MatchString(t.value) {
				return fmt.Sprintf("%s mentions %s", t.where, n), true
			}
		}
	}
	return "", false
}

// hostPattern matches name as a whole host: bounded by start/end, a scheme
// or credentials separator, a port, a path, whitespace, quotes or commas,
// and not followed by another DNS label.
func hostPattern(name string) *regexp.Regexp {
	return regexp.MustCompile(`(^|[/@=\s,"'])` + regexp.QuoteMeta(name) + `($|[:/\s,"'])`)
}

// Name is a deterministic object name for a relationship.
func Name(r Relationship) string {
	n := strings.ToLower(fmt.Sprintf("%s-%s.%s.%s-%s", kindShort(r.From.Kind), r.From.Name, r.Type, kindShort(r.To.Kind), r.To.Name))
	if r.To.Namespace != r.From.Namespace {
		n += "." + r.To.Namespace
	}
	if len(n) > 253 {
		n = n[:253]
	}
	return strings.Trim(n, ".-")
}

func kindShort(kind string) string {
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
	return strings.ToLower(kind)
}
