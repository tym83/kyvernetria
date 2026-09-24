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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
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
// to every Service it addresses by a DNS name in its environment, arguments
// or command.
//
// Matching is one pass over the texts: every text is split into host-like
// tokens once, and each token is looked up in an index of service names.
// The cost is proportional to services + texts, not to their product.
func Graph(services []*v1.Service, workloads []Workload) []Relationship {
	var out []Relationship
	served := map[servesKey]bool{}
	for _, svc := range services {
		if len(svc.Spec.Selector) == 0 {
			continue
		}
		selector := labels.SelectorFromSet(svc.Spec.Selector)
		for _, w := range workloads {
			if w.Namespace == svc.Namespace && selector.Matches(labels.Set(w.Template.Labels)) {
				served[servesKey{svc.Namespace, svc.Name, w.Ref}] = true
				out = append(out, Relationship{
					From:     Ref{"Service", svc.Namespace, svc.Name, svc.UID},
					To:       w.Ref,
					Type:     Serves,
					Evidence: "selector " + labels.Set(svc.Spec.Selector).String(),
				})
			}
		}
	}
	idx := newServiceIndex(services)
	for _, w := range workloads {
		seen := map[*v1.Service]bool{}
		for _, t := range templateTexts(w.Template) {
			for _, tok := range hostTokens(t.value, t.hostish) {
				svc := idx.lookup(tok, w.Namespace)
				if svc == nil || seen[svc] {
					continue
				}
				seen[svc] = true
				if served[servesKey{svc.Namespace, svc.Name, w.Ref}] {
					continue // a service calling itself is not a relationship
				}
				out = append(out, Relationship{
					From:     w.Ref,
					To:       Ref{"Service", svc.Namespace, svc.Name, svc.UID},
					Type:     TalksTo,
					Evidence: fmt.Sprintf("%s mentions %s", t.where, tok.host),
				})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return Name(out[i]) < Name(out[j]) })
	return out
}

type servesKey struct {
	namespace, name string
	workload        Ref
}

type text struct {
	where string
	value string
	// hostish is set for environment variables whose name says the value
	// is an address (API_HOST, DATABASE_URL, ...).
	hostish bool
}

// hostishEnvSuffixes mark environment variables that hold an address.
var hostishEnvSuffixes = []string{"_HOST", "_HOSTS", "_URL", "_ADDR", "_ADDRESS", "_SERVICE", "_ENDPOINT", "_URI"}

func isHostishEnv(name string) bool {
	name = strings.ToUpper(name)
	for _, s := range hostishEnvSuffixes {
		if strings.HasSuffix(name, s) {
			return true
		}
	}
	return false
}

func templateTexts(t v1.PodTemplateSpec) []text {
	var out []text
	add := func(c v1.Container) {
		for _, e := range c.Env {
			if e.Value != "" {
				out = append(out, text{fmt.Sprintf("container %s env %s", c.Name, e.Name), e.Value, isHostishEnv(e.Name)})
			}
		}
		for i, a := range c.Args {
			out = append(out, text{fmt.Sprintf("container %s arg %d", c.Name, i), a, false})
		}
		for i, a := range c.Command {
			out = append(out, text{fmt.Sprintf("container %s command %d", c.Name, i), a, false})
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

// token is a host name found in a text. strong is set when the text uses
// it as a host (URL host, host:port, user@host, or an address variable);
// a bare word is weak.
type token struct {
	host   string
	strong bool
}

// hostTokens extracts the host-like tokens of a text.
func hostTokens(value string, hostish bool) []token {
	var out []token
	fields := strings.FieldsFunc(value, func(r rune) bool {
		switch r {
		case ' ', '\t', '\n', '\r', ',', ';', '"', '\'', '(', ')', '[', ']', '{', '}', '<', '>':
			return true
		}
		return false
	})
	for _, f := range fields {
		// --flag=value and KEY=value: the value is what may address a host.
		// A "/" before the "=" means the "=" is inside a URL instead.
		if i := strings.IndexByte(f, '='); i >= 0 && !strings.Contains(f[:i], "/") {
			f = f[i+1:]
		}
		if t, ok := hostToken(f, hostish); ok {
			out = append(out, t)
		}
	}
	return out
}

func hostToken(f string, hostish bool) (token, bool) {
	if i := strings.Index(f, "://"); i >= 0 {
		return hostOf(f[i+3:], true)
	}
	if i := strings.LastIndexByte(f, '@'); i >= 0 {
		return hostOf(f[i+1:], true)
	}
	return hostOf(f, hostish)
}

// hostOf reads the host at the start of s: [user@]host[:port][/path...].
// A port makes a bare word a host.
func hostOf(s string, strong bool) (token, bool) {
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndexByte(s, '@'); i >= 0 {
		s = s[i+1:]
	}
	if host, port, ok := strings.Cut(s, ":"); ok {
		if !isDigits(port) {
			return token{}, false
		}
		s, strong = host, true
	}
	s = strings.TrimSuffix(strings.ToLower(s), ".")
	if s == "" || !isHostName(s) {
		return token{}, false
	}
	return token{host: s, strong: strong}, true
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func isHostName(s string) bool {
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '.') {
			return false
		}
	}
	return true
}

// serviceIndex finds services by the DNS names that resolve to them.
type serviceIndex struct {
	qualified map[string]*v1.Service // name.namespace, the forms below it are normalized to it
	short     map[string]*v1.Service // namespace/name
}

func newServiceIndex(services []*v1.Service) *serviceIndex {
	idx := &serviceIndex{qualified: map[string]*v1.Service{}, short: map[string]*v1.Service{}}
	for _, svc := range services {
		idx.qualified[svc.Name+"."+svc.Namespace] = svc
		idx.short[svc.Namespace+"/"+svc.Name] = svc
	}
	return idx
}

// lookup resolves a token as a workload in namespace ns would: the
// qualified forms (name.namespace, name.namespace.svc and
// name.namespace.svc.<cluster domain>) from anywhere, the short name only
// from the same namespace and only where the text uses it as a host.
func (idx *serviceIndex) lookup(t token, ns string) *v1.Service {
	host := t.host
	if strings.Contains(host, ".") {
		if i := strings.Index(host, ".svc."); i >= 0 {
			host = host[:i]
		} else {
			host = strings.TrimSuffix(host, ".svc")
		}
		return idx.qualified[host]
	}
	if !t.strong {
		return nil
	}
	return idx.short[ns+"/"+host]
}

// Name is a deterministic object name for a relationship.
func Name(r Relationship) string {
	n := strings.ToLower(fmt.Sprintf("%s-%s.%s.%s-%s", kindShort(r.From.Kind), r.From.Name, r.Type, kindShort(r.To.Kind), r.To.Name))
	if r.To.Namespace != r.From.Namespace {
		n += "." + r.To.Namespace
	}
	n = strings.Trim(n, ".-")
	if len(n) > validation.DNS1123SubdomainMaxLength {
		// Distinct long names must stay distinct: keep a hash of the whole.
		sum := sha256.Sum256([]byte(n))
		suffix := "-" + hex.EncodeToString(sum[:])[:10]
		n = strings.TrimRight(n[:validation.DNS1123SubdomainMaxLength-len(suffix)], ".-") + suffix
	}
	return n
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
