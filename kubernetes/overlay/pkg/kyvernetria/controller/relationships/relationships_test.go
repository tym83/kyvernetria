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
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

func svc(ns, name string, selector map[string]string) *v1.Service {
	return &v1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Spec: v1.ServiceSpec{Selector: selector}}
}

func deploy(ns, name string, labels map[string]string, env map[string]string, args ...string) *appsv1.Deployment {
	c := v1.Container{Name: "app", Args: args}
	for k, v := range env {
		c.Env = append(c.Env, v1.EnvVar{Name: k, Value: v})
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: appsv1.DeploymentSpec{Template: v1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: labels},
			Spec:       v1.PodSpec{Containers: []v1.Container{c}},
		}},
	}
}

func TestGraph(t *testing.T) {
	services := []*v1.Service{
		svc("shop", "api", map[string]string{"app": "api"}),
		svc("shop", "db", map[string]string{"app": "db"}),
		svc("shop", "api-v2", map[string]string{"app": "api-v2"}),
		svc("auth", "sso", map[string]string{"app": "sso"}),
		svc("shop", "headless-external", nil),
	}
	workloads := Workloads([]*appsv1.Deployment{
		deploy("shop", "frontend", map[string]string{"app": "frontend"}, map[string]string{"API_URL": "http://api:8080/v1"}),
		deploy("shop", "api", map[string]string{"app": "api"},
			map[string]string{"DATABASE": "postgres://user@db:5432/shop", "SELF": "http://api:8080"},
			"--sso=https://sso.auth.svc.cluster.local"),
		deploy("shop", "db", map[string]string{"app": "db"}, nil),
		deploy("other", "stranger", map[string]string{"app": "x"}, map[string]string{"TARGET": "http://api:8080"}),
	}, nil, nil)

	got := map[string]string{}
	for _, r := range Graph(services, workloads) {
		got[Name(r)] = r.Evidence
	}
	want := []string{
		"svc-api.serves.deploy-api",
		"svc-db.serves.deploy-db",
		"deploy-frontend.talks-to.svc-api",
		"deploy-api.talks-to.svc-db",
		"deploy-api.talks-to.svc-sso.auth",
	}
	for _, name := range want {
		if _, ok := got[name]; !ok {
			t.Errorf("missing relationship %s; got %v", name, got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %d relationships, want %d: %v", len(got), len(want), got)
	}
	if _, ok := got["deploy-api.talks-to.svc-api"]; ok {
		t.Error("a service calling itself became a relationship")
	}
	if _, ok := got["deploy-frontend.talks-to.svc-api-v2"]; ok {
		t.Error("api matched inside api-v2")
	}
}

func TestShortNameCountsOnlyAsHost(t *testing.T) {
	services := []*v1.Service{
		svc("shop", "api", map[string]string{"app": "api"}),
		svc("shop", "worker", map[string]string{"app": "worker"}),
	}
	for _, tc := range []struct {
		name string
		env  map[string]string
		args []string
		want bool
	}{
		{"URL host", nil, []string{"http://api:8080/v1"}, true},
		{"URL with a query", nil, []string{"--target=http://api/v1?mode=a=b"}, true},
		{"host:port", nil, []string{"api:8080"}, true},
		{"host:port in a flag", nil, []string{"--upstream=api:8080"}, true},
		{"user@host", nil, []string{"deploy@api/path"}, true},
		{"address variable", map[string]string{"API_HOST": "api"}, nil, true},
		{"address list variable", map[string]string{"PEER_HOSTS": "x,api,y"}, nil, true},
		{"endpoint variable", map[string]string{"UPSTREAM_ENDPOINT": "api"}, nil, true},
		{"bare argument", nil, []string{"api"}, false},
		{"flag value", nil, []string{"--mode=api"}, false},
		{"plain variable", map[string]string{"MODE": "api"}, nil, false},
		{"longer host", nil, []string{"http://api-v2:8080"}, false},
		{"other domain", nil, []string{"http://api.internal:8080"}, false},
		{"not a port", nil, []string{"api:latest"}, false},
		{"qualified bare word", nil, []string{"--peer=api.shop"}, true},
		{"qualified with another cluster domain", nil, []string{"api.shop.svc.example.org"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := Workloads([]*appsv1.Deployment{deploy("shop", "client", map[string]string{"app": "client"}, tc.env, tc.args...)}, nil, nil)
			got := false
			for _, r := range Graph(services, w) {
				if r.Type == TalksTo && r.To.Name == "api" {
					got = true
				}
				if r.To.Name == "worker" {
					t.Errorf("bare word matched worker: %+v", r)
				}
			}
			if got != tc.want {
				t.Errorf("talks to api = %v, want %v", got, tc.want)
			}
		})
	}
	w := Workloads([]*appsv1.Deployment{deploy("shop", "queue", map[string]string{"app": "queue"}, nil, "worker", "--mode=worker")}, nil, nil)
	if rels := Graph(services, w); len(rels) != 0 {
		t.Errorf("args [worker --mode=worker] made relationships: %+v", rels)
	}
}

func TestNameTruncationKeepsNamesDistinct(t *testing.T) {
	long := strings.Repeat("a", 240)
	a := Relationship{From: Ref{Kind: "Deployment", Namespace: "ns", Name: long + "-one"}, To: Ref{Kind: "Service", Namespace: "ns", Name: "db"}, Type: TalksTo}
	b := a
	b.From.Name = long + "-two"
	na, nb := Name(a), Name(b)
	if na == nb {
		t.Fatalf("truncated names collide: %s", na)
	}
	for _, n := range []string{na, nb} {
		if errs := validation.IsDNS1123Subdomain(n); len(errs) > 0 {
			t.Errorf("invalid name %q: %v", n, errs)
		}
	}
	if Name(a) != na {
		t.Error("name is not deterministic")
	}
	short := Relationship{From: Ref{Kind: "Deployment", Namespace: "ns", Name: "api"}, To: Ref{Kind: "Service", Namespace: "ns", Name: "db"}, Type: TalksTo}
	if got := Name(short); got != "deploy-api.talks-to.svc-db" {
		t.Errorf("short name changed: %s", got)
	}
}
