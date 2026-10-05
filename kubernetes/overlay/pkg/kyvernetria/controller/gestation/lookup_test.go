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

package gestation

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	corelisters "k8s.io/client-go/listers/core/v1"
	metadatafake "k8s.io/client-go/metadata/fake"

	api "k8s.io/kubernetes/pkg/kyvernetria/gestation"
)

// registry serves /v2/<repo>/manifests/<ref> behind an anonymous token,
// like Docker Hub and ghcr.io, and fails the test if credentials arrive.
func registry(t *testing.T) *httptest.Server {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if strings.HasPrefix(auth, "Basic") {
			t.Errorf("credentials sent to the registry: %s", auth)
		}
		switch {
		case r.URL.Path == "/token":
			if r.URL.Query().Get("scope") == "" {
				t.Error("token request without scope")
			}
			_, _ = w.Write([]byte(`{"token":"anon"}`))
		case auth != "Bearer anon":
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+srv.URL+`/token",service="test",scope="repository:x:pull"`)
			w.WriteHeader(http.StatusUnauthorized)
		case strings.HasSuffix(r.URL.Path, "/shop/web/manifests/v1"):
			w.Header().Set("Docker-Content-Digest", "sha256:abc")
			w.WriteHeader(http.StatusOK)
		case strings.Contains(r.URL.Path, "/private/"):
			w.WriteHeader(http.StatusForbidden)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestImageChecker(t *testing.T) {
	srv := registry(t)
	host := strings.TrimPrefix(srv.URL, "http://")
	nodes := indexer()
	_ = nodes.Add(&v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "w1"}, Status: v1.NodeStatus{Images: []v1.ContainerImage{
		{Names: []string{"docker.io/library/nginx:1.27", "docker.io/library/nginx@sha256:" + strings.Repeat("b", 64)}},
	}}})
	c := &imageChecker{nodes: corelisters.NewNodeLister(nodes), http: srv.Client(), scheme: "http"}
	for image, want := range map[string]string{
		"nginx:1.27": api.ResultClear,
		"nginx@sha256:" + strings.Repeat("b", 64): api.ResultClear,
		host + "/shop/web:v1":                     api.ResultClear,
		host + "/shop/web:v2":                     api.ResultFail,
		host + "/private/web:v1":                  api.ResultInfo,
		"nginx:not a tag!":                        api.ResultFail,
	} {
		got, msg := c.check(context.Background(), image)
		if got != want {
			t.Errorf("%s: %s (%s), want %s", image, got, msg, want)
		}
	}
	if r, msg := c.check(context.Background(), host+"/shop/web:v1"); r != api.ResultClear || !strings.Contains(msg, "sha256:abc") {
		t.Errorf("digest not reported: %s", msg)
	}
	offline := &imageChecker{}
	if r, _ := offline.check(context.Background(), "nginx:1.27"); r != api.ResultInfo {
		t.Errorf("without nodes or network: %s", r)
	}
	unreachable := &imageChecker{http: http.DefaultClient, scheme: "http"}
	if r, msg := unreachable.check(context.Background(), "127.0.0.1:1/x:y"); r != api.ResultInfo || !strings.Contains(msg, "couldn't reach") {
		t.Errorf("unreachable registry: %s %s", r, msg)
	}
}

func TestLookupReadsMetadataOnly(t *testing.T) {
	scheme := metadatafake.NewTestScheme()
	_ = metav1.AddMetaToScheme(scheme)
	secret := &metav1.PartialObjectMetadata{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "shop"}}
	meta := metadatafake.NewSimpleMetadataClient(scheme, secret)
	kube := fake.NewSimpleClientset(&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "fast",
		Annotations: map[string]string{"storageclass.kubernetes.io/is-default-class": "true"}}})
	l := &clusterLookup{client: kube, metadata: meta, images: &imageChecker{}}
	if ok, err := l.SecretExists(context.Background(), "shop", "db"); !ok || err != nil {
		t.Errorf("existing secret: %v %v", ok, err)
	}
	if ok, err := l.SecretExists(context.Background(), "shop", "nope"); ok || err != nil {
		t.Errorf("missing secret: %v %v", ok, err)
	}
	for _, a := range kube.Actions() {
		if a.GetResource().Resource == "secrets" {
			t.Errorf("secrets read through the typed client: %v", a)
		}
	}
	if sc, err := l.StorageClass(""); err != nil || sc == nil || sc.Name != "fast" {
		t.Errorf("default class: %v %v", sc, err)
	}
	if sc, err := l.StorageClass("slow"); err != nil || sc != nil {
		t.Errorf("missing class: %v %v", sc, err)
	}
}
