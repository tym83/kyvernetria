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
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/client-go/kubernetes/fake"
)

func TestExplain(t *testing.T) {
	for msg, want := range map[string]string{
		`The connection to the server 127.0.0.1:6443 was refused - did you specify the right host or port? dial tcp 127.0.0.1:6443: connect: connection refused`: "isn't answering yet",
		`Error from server (Forbidden): pods is forbidden: User "alice" cannot list resource "pods" in API group "" in the namespace "default"`:                  `signed in as alice, and that identity isn't allowed to list pods`,
		`Error from server (NotFound): deployments.apps "api" not found`:                                                                                         `couldn't find deployments.apps "api"`,
		`error: the server doesn't have a resource type "widgets"`:                                                                                               `doesn't know the kind "widgets"`,
		`something nobody has seen before`: "",
	} {
		got := Explain(msg)
		if want == "" && got != "" || want != "" && !strings.Contains(got, want) {
			t.Errorf("Explain(%q) = %q, want it to contain %q", msg, got, want)
		}
	}
}

func TestFailuresNoticeRepetition(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	f := &Failures{Path: filepath.Join(t.TempDir(), "kyvernetria", "failures"), Window: 3 * time.Minute, Now: func() time.Time { return now }}
	args := []string{"get", "deploy/api", "--token", "s3cret"}
	if n := f.Record(args); n != 1 {
		t.Fatalf("first failure counted %d", n)
	}
	f.Record([]string{"get", "pods"})
	now = now.Add(time.Minute)
	if n := f.Record(args); n != 2 {
		t.Fatalf("second failure counted %d", n)
	}
	now = now.Add(time.Minute)
	n := f.Record(args)
	if n != 3 {
		t.Fatalf("third failure counted %d", n)
	}
	if c := Comfort(n, args); !strings.Contains(c, "kyvctl remember deploy/api") || !strings.Contains(c, "3 times in the last few minutes") {
		t.Errorf("unexpected comfort: %q", c)
	}
	raw, err := os.ReadFile(f.Path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "s3cret") || strings.Contains(string(raw), "deploy/api") {
		t.Errorf("history keeps the command line:\n%s", raw)
	}
	if info, err := os.Stat(filepath.Dir(f.Path)); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("history directory is not private: %v %v", info.Mode(), err)
	}
	now = now.Add(10 * time.Minute)
	if n := f.Record(args); n != 1 {
		t.Errorf("old failures were not forgotten: %d", n)
	}
	if Comfort(2, args) != "" {
		t.Error("comforted too early")
	}
}

func TestFingerprintRedacts(t *testing.T) {
	same := [][]string{
		{"get", "pods", "--token", "a"},
		{"get", "pods", "--token", "b"},
		{"get", "pods", "--token=c"},
	}
	for _, args := range same[1:] {
		if Fingerprint(args) != Fingerprint(same[0]) {
			t.Errorf("secret value changed the fingerprint: %v", args)
		}
	}
	if Fingerprint([]string{"get", "pods", "-n", "a"}) == Fingerprint([]string{"get", "pods", "-n", "b"}) {
		t.Error("namespace no longer tells commands apart")
	}
	if Fingerprint([]string{"exec", "p", "--", "sh", "-c", "echo x"}) != Fingerprint([]string{"exec", "p", "--", "cat", "/secret"}) {
		t.Error("the command after -- is part of the fingerprint")
	}
}

func TestFailuresWithoutCacheDirKeepNoHistory(t *testing.T) {
	f := &Failures{Window: time.Minute, Now: time.Now}
	for i := 0; i < 3; i++ {
		if n := f.Record([]string{"get", "pods"}); n != 1 {
			t.Fatalf("counted %d without a history file", n)
		}
	}
}

func TestFailuresRefuseSymlinkedHistory(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	history := filepath.Join(dir, "kyvernetria", "failures")
	if err := os.MkdirAll(filepath.Dir(history), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, history); err != nil {
		t.Skip("symlinks unsupported:", err)
	}
	f := &Failures{Path: history, Window: time.Minute, Now: time.Now}
	f.Record([]string{"get", "pods"})
	if raw, _ := os.ReadFile(victim); string(raw) != "keep\n" {
		t.Errorf("history was written through a symlink: %q", raw)
	}
}

func warning(ns, kind, name, reason string, count int32, at time.Time) v1.Event {
	return v1.Event{
		ObjectMeta:     metav1.ObjectMeta{Namespace: ns},
		InvolvedObject: v1.ObjectReference{Kind: kind, Name: name},
		Type:           v1.EventTypeWarning, Reason: reason, Count: count,
		LastTimestamp: metav1.NewTime(at), Message: reason + " happened",
	}
}

func TestFold(t *testing.T) {
	now := time.Now()
	events := []v1.Event{
		warning("shop", "Pod", "api-7f9c8d6b5-x2kq4", "BackOff", 30, now.Add(-time.Minute)),
		warning("shop", "Pod", "api-7f9c8d6b5-pl9mz", "BackOff", 12, now.Add(-2*time.Minute)),
		warning("shop", "Pod", "db-0", "Unhealthy", 3, now.Add(-5*time.Minute)),
		warning("shop", "Node", "worker-2", "Worried", 1, now.Add(-10*time.Minute)),
		warning("shop", "Pod", "db-1", "Unhealthy", 2, now.Add(-2*time.Hour)),
	}
	pods := PodSubjects([]v1.Pod{
		ownedPod("shop", "db-0", "StatefulSet", "db", ""),
		ownedPod("shop", "db-1", "StatefulSet", "db", ""),
	})
	worries := Fold(events, now.Add(-time.Hour), pods)
	if len(worries) != 3 {
		t.Fatalf("got %d groups: %+v", len(worries), worries)
	}
	if worries[0].Subject != "api (pods)" || worries[0].Count != 42 {
		t.Errorf("top worry = %+v", worries[0])
	}
	if worries[1].Subject != "db (pods)" || worries[1].Count != 3 {
		t.Errorf("second worry = %+v", worries[1])
	}
	var out bytes.Buffer
	renderCalm(&out, worries, 2, time.Hour)
	if !strings.Contains(out.String(), "46 warnings in the last 1h0m0s come down to 3 things. The 2 that deserve attention first") {
		t.Errorf("unexpected calm output:\n%s", out.String())
	}
}

func ownedPod(ns, name, kind, owner, hash string) v1.Pod {
	p := v1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	if hash != "" {
		p.Labels = map[string]string{"pod-template-hash": hash}
	}
	if kind != "" {
		controller := true
		p.OwnerReferences = []metav1.OwnerReference{{Kind: kind, Name: owner, Controller: &controller}}
	}
	return p
}

func TestCalmSubjects(t *testing.T) {
	pods := PodSubjects([]v1.Pod{
		ownedPod("kube-system", "kube-apiserver-kyv-cp-1", "Node", "kyv-cp-1", ""),
		ownedPod("kube-system", "kube-apiserver-kyv-cp-2", "Node", "kyv-cp-2", ""),
		ownedPod("monitoring", "stack-prometheus-node-exporter-j7k2p", "DaemonSet", "stack-prometheus-node-exporter", ""),
		ownedPod("shop", "api-7dc849f4f8-7q9hv", "ReplicaSet", "api-7dc849f4f8", "7dc849f4f8"),
		ownedPod("shop", "debug", "", "", ""),
	})
	for key, want := range map[string]string{
		"kube-system/kube-apiserver-kyv-cp-1":             "pod/kube-apiserver-kyv-cp-1",
		"kube-system/kube-apiserver-kyv-cp-2":             "pod/kube-apiserver-kyv-cp-2",
		"monitoring/stack-prometheus-node-exporter-j7k2p": "stack-prometheus-node-exporter (pods)",
		"shop/api-7dc849f4f8-7q9hv":                       "api (pods)",
		"shop/debug":                                      "pod/debug",
	} {
		if got := pods[key]; got != want {
			t.Errorf("PodSubjects[%s] = %q, want %q", key, got, want)
		}
	}
	// Pods that are gone fall back to their names.
	for name, want := range map[string]string{
		"api-7dc849f4f8-7q9hv":               "api (pods)",
		"agent-x2kq4":                        "agent (pods)",
		"prometheus-node-exporter":           "pod/prometheus-node-exporter",
		"kube-apiserver-kyv-cp-2":            "pod/kube-apiserver-kyv-cp-2",
		"stack-prometheus-node-exporter-abc": "pod/stack-prometheus-node-exporter-abc",
	} {
		if got := Subject("Pod", name); got != want {
			t.Errorf("Subject(Pod, %s) = %q, want %q", name, got, want)
		}
	}
}

func TestRejections(t *testing.T) {
	msg := func(antigen string) string {
		return `pods "x" is forbidden: kyvernetria immunity: ` + antigen + `. If this is your own workload, mark its namespace as self: ...`
	}
	ev := func(ns, name, antigen string) v1.Event {
		return v1.Event{ObjectMeta: metav1.ObjectMeta{Namespace: ns}, Reason: "FailedCreate", Count: 4,
			InvolvedObject: v1.ObjectReference{Kind: "DaemonSet", Name: name}, Message: msg(antigen)}
	}
	rs := Rejections([]v1.Event{
		ev("cilium", "cilium-agent", `pod uses the host network`),
		ev("shop", "miner", `container "x" is privileged`),
		ev("monitoring", "node-exporter", `volume "root" mounts a host path`),
		ev("already-fixed", "agent", `pod uses the host network`),
		{Message: "unrelated"},
	}, map[string]bool{"already-fixed": true})
	if len(rs) != 3 {
		t.Fatalf("got %+v", rs)
	}
	if !rs[0].LikelySelf || !rs[1].LikelySelf || rs[2].LikelySelf || rs[2].Namespace != "shop" {
		t.Errorf("self detection wrong: %+v", rs)
	}
	if rs[0].Antigens != "pod uses the host network" {
		t.Errorf("antigens not extracted: %q", rs[0].Antigens)
	}
	var out bytes.Buffer
	renderAutoimmune(&out, rs)
	if !strings.Contains(out.String(), "kyvctl label namespace cilium kyvernetria.io/self=true") ||
		strings.Contains(out.String(), "label namespace shop") {
		t.Errorf("unexpected advice:\n%s", out.String())
	}
}

func TestMosaic(t *testing.T) {
	alleles, minors := Alleles([]string{"v1.37.0-kyvernetria.0", "v1.36.4-kyvernetria.0", ""})
	if minors != 2 || alleles[0] != "Xm" || alleles[1] != "Xp" || alleles[2] != "?" {
		t.Errorf("allele detection wrong: %v %d", alleles, minors)
	}
	// Minor versions are numbers, not strings: 1.10 is newer than 1.9.
	if alleles, _ := Alleles([]string{"v1.9.3", "v1.10.0"}); alleles[0] != "Xp" || alleles[1] != "Xm" {
		t.Errorf("1.9 vs 1.10: %v", alleles)
	}
	var out bytes.Buffer
	renderMosaic(&out, []cell{
		{node: "cp-1", version: "v1.37.0-kyvernetria.0"},
		{node: "cp-2", version: "v1.36.4-kyvernetria.0", restarts: 5},
		{node: "cp-3", version: "v1.37.0-kyvernetria.0", emulated: "1.36"},
	})
	if !strings.Contains(out.String(), "Mosaic: 2 Xm, 1 Xp") || !strings.Contains(out.String(), "5 restarts") ||
		!strings.Contains(out.String(), "emulating 1.36") {
		t.Errorf("unexpected mosaic output:\n%s", out.String())
	}
	out.Reset()
	renderMosaic(&out, []cell{
		{node: "cp-1", version: "v1.37.0-kyvernetria.0"},
		{node: "cp-2", version: "v1.37.0-kyvernetria.0"},
		{node: "cp-3", note: "not answering"},
	})
	if !strings.Contains(out.String(), "1 of 3 nodes aren't answering") || strings.Contains(out.String(), "same minor") {
		t.Errorf("a silent node was mistaken for a uniform mosaic:\n%s", out.String())
	}
	// Only the older minor answers: that must not be called Xm just
	// because kyvctl itself is newer.
	out.Reset()
	renderMosaic(&out, []cell{
		{node: "cp-1", version: "v1.36.4-kyvernetria.0"},
		{node: "cp-2", version: "v1.36.4-kyvernetria.0"},
	})
	if strings.Contains(out.String(), "Xm") || strings.Contains(out.String(), "Xp") ||
		!strings.Contains(out.String(), "can't tell which allele it is (only one minor answers)") {
		t.Errorf("a single minor was labelled:\n%s", out.String())
	}
}

func TestRenderRelationships(t *testing.T) {
	rel := func(ns, fromKind, from, typ, toKind, toNS, to string) unstructured.Unstructured {
		return unstructured.Unstructured{Object: map[string]interface{}{
			"metadata": map[string]interface{}{"namespace": ns, "name": from + "-" + to},
			"spec": map[string]interface{}{
				"from": map[string]interface{}{"kind": fromKind, "name": from, "namespace": ns},
				"to":   map[string]interface{}{"kind": toKind, "name": to, "namespace": toNS},
				"type": typ,
			},
		}}
	}
	items := []unstructured.Unstructured{
		rel("shop", "Deployment", "frontend", "talks-to", "Service", "shop", "api"),
		rel("shop", "Service", "api", "serves", "Deployment", "shop", "api"),
		rel("shop", "Deployment", "api", "talks-to", "Service", "auth", "sso"),
		rel("shop", "Deployment", "apigw", "talks-to", "Service", "shop", "db"),
	}
	var out bytes.Buffer
	if err := renderRelationships(&out, items, "api"); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"deploy/frontend", "--talks-to--> svc/api", "--serves--> deploy/api", "svc/sso (auth)"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "apigw") {
		t.Errorf("focus on api matched apigw:\n%s", got)
	}
}

func TestInvokesOwnCommand(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{[]string{"kyvctl", "mosaic"}, true},
		{[]string{"kyvctl", "--kubeconfig", "/tmp/k", "mosaic"}, true},
		{[]string{"kyvctl", "--kubeconfig=/tmp/k", "-n", "shop", "remember", "deploy/api"}, true},
		{[]string{"kyvctl", "-nshop", "calm", "--top", "3"}, true},
		{[]string{"kyvctl", "-v", "4", "diagnose", "autoimmune"}, true},
		{[]string{"kyvctl", "--insecure-skip-tls-verify", "calm"}, true},
		{[]string{"kyvctl", "get", "pods", "calm"}, false},
		{[]string{"kyvctl", "-n", "diagnose", "get", "po"}, false},
		{[]string{"kyvctl", "--namespace", "calm", "logs", "api"}, false},
		{[]string{"kyvctl", "logs", "remember"}, false},
		{[]string{"kyvctl", "get", "pods"}, false},
		{[]string{"kyvctl"}, false},
	} {
		if got := invokesOwnCommand(tc.args); got != tc.want {
			t.Errorf("invokesOwnCommand(%v) = %v, want %v", tc.args, got, tc.want)
		}
	}
}

func TestOwnCommandKeepsItsArguments(t *testing.T) {
	root := newCommand(genericiooptions.IOStreams{In: strings.NewReader(""), Out: io.Discard, ErrOut: io.Discard},
		[]string{"kyvctl", "-n", "shop", "calm", "--top", "3"})
	c, rest, err := root.Find([]string{"-n", "shop", "calm", "--top", "3"})
	if err != nil || c.Name() != "calm" {
		t.Fatalf("calm not found: %v %v", c, err)
	}
	if len(rest) == 0 {
		t.Error("arguments were dropped")
	}
}

func TestCalmRejectsNonPositiveTop(t *testing.T) {
	for _, top := range []int{0, -1} {
		if err := runCalm(context.Background(), nil, io.Discard, false, time.Hour, top); err == nil || !strings.Contains(err.Error(), "--top") {
			t.Errorf("--top=%d: err = %v", top, err)
		}
	}
}

func TestDescends(t *testing.T) {
	for _, tc := range []struct {
		kind, name, objKind, objName string
		want                         bool
	}{
		{"Deployment", "api", "ReplicaSet", "api-7f9c8d6b5", true},
		{"Deployment", "api", "Pod", "api-7f9c8d6b5-x2kq4", true},
		{"Deployment", "api", "Pod", "api-gateway-7f9c8d6b5-x2kq4", false},
		{"Deployment", "api", "ReplicaSet", "api-gateway-7f9c8d6b5", false},
		{"StatefulSet", "db", "Pod", "db-0", true},
		{"StatefulSet", "db", "Pod", "db-backup-0", false},
		{"DaemonSet", "agent", "Pod", "agent-x2kq4", true},
		{"DaemonSet", "agent", "Pod", "agent-v2-x2kq4", false},
		{"ReplicaSet", "api-7f9c8d6b5", "Pod", "api-7f9c8d6b5-x2kq4", true},
	} {
		if got := Descends(tc.kind, tc.name, tc.objKind, tc.objName); got != tc.want {
			t.Errorf("Descends(%s %s, %s %s) = %v, want %v", tc.kind, tc.name, tc.objKind, tc.objName, got, tc.want)
		}
	}
}

func TestCollectStory(t *testing.T) {
	yes := true
	ev := func(kind, name string, uid types.UID, reason string) *v1.Event {
		return &v1.Event{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: name + "." + reason},
			InvolvedObject: v1.ObjectReference{Kind: kind, Name: name, UID: uid}, Reason: reason,
			LastTimestamp: metav1.Now()}
	}
	client := fake.NewSimpleClientset(
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "api", UID: "d-api"}},
		&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "api-custom", UID: "rs-custom",
			OwnerReferences: []metav1.OwnerReference{{UID: "d-api", Controller: &yes}}}},
		&v1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "odd-name", UID: "p-odd",
			OwnerReferences: []metav1.OwnerReference{{UID: "rs-custom", Controller: &yes}}}},
		ev("Deployment", "api", "d-api", "ScalingReplicaSet"),
		ev("Pod", "odd-name", "p-odd", "Started"),                          // live, found by owner
		ev("Pod", "api-7f9c8d6b5-x2kq4", "gone", "Killing"),                // gone, found by name
		ev("Pod", "api-gateway-7f9c8d6b5-x2kq4", "other", "Killing"),       // someone else
		ev("Deployment", "api-gateway", "d-gw", "ScalingReplicaSet"),       // someone else
		ev("ReplicaSet", "api-7f9c8d6b5", "gone-rs", "SuccessfulCreate"),   // gone, found by name
		ev("ReplicaSet", "api-gateway-7f9c8d6b5", "x", "SuccessfulCreate"), // someone else
	)
	s, err := collectStory(context.Background(), client, "shop", "Deployment", "api")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range s.events {
		got[e.InvolvedObject.Name] = true
	}
	for _, want := range []string{"api", "odd-name", "api-7f9c8d6b5-x2kq4", "api-7f9c8d6b5"} {
		if !got[want] {
			t.Errorf("missing events of %s: %v", want, got)
		}
	}
	if len(s.events) != 4 {
		t.Errorf("got %d events, want 4: %v", len(s.events), got)
	}
}

func TestComfortIgnoresFlagValues(t *testing.T) {
	args := []string{"--kubeconfig", "/home/me/.kube/config", "-n", "shop", "get", "deploy/nope"}
	if c := Comfort(3, args); !strings.Contains(c, "kyvctl remember deploy/nope") {
		t.Errorf("comfort pointed at a flag value: %q", c)
	}
	args = []string{"--kubeconfig=/home/me/.kube/config", "get", "pods"}
	if c := Comfort(3, args); strings.Contains(c, "remember") {
		t.Errorf("comfort invented an object: %q", c)
	}
}
