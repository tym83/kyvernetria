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
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

func TestHostPattern(t *testing.T) {
	for s, want := range map[string]bool{
		"http://api:8080":    true,
		"api":                true,
		"api.internal":       false,
		"my-api":             false,
		"user@api/path":      true,
		"--target=api":       true,
		"x,api,y":            true,
		"apiserver":          false,
	} {
		if got := hostPattern("api").MatchString(s); got != want {
			t.Errorf("hostPattern(api).Match(%q) = %v, want %v", s, got, want)
		}
	}
}
