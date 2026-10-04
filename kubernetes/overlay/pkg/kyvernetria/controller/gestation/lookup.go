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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/distribution/reference"

	v1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/metadata"

	api "k8s.io/kubernetes/pkg/kyvernetria/gestation"
)

// clusterLookup answers screening's questions from the API. Secrets and
// ConfigMaps are fetched as metadata only (PartialObjectMetadata), so
// their data never reaches the controller.
type clusterLookup struct {
	client   kubernetes.Interface
	metadata metadata.Interface
	images   *imageChecker
}

var _ api.Lookup = &clusterLookup{}

var (
	secretsGVR    = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	configMapsGVR = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
)

func (l *clusterLookup) exists(ctx context.Context, gvr schema.GroupVersionResource, ns, name string) (bool, error) {
	if l.metadata == nil {
		return false, fmt.Errorf("no metadata client")
	}
	_, err := l.metadata.Resource(gvr).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

func (l *clusterLookup) SecretExists(ctx context.Context, ns, name string) (bool, error) {
	return l.exists(ctx, secretsGVR, ns, name)
}

func (l *clusterLookup) ConfigMapExists(ctx context.Context, ns, name string) (bool, error) {
	return l.exists(ctx, configMapsGVR, ns, name)
}

func (l *clusterLookup) PVC(ns, name string) (*v1.PersistentVolumeClaim, error) {
	pvc, err := l.client.CoreV1().PersistentVolumeClaims(ns).Get(context.TODO(), name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	return pvc, err
}

// defaultClassAnnotations mark the default StorageClass (GA and beta).
var defaultClassAnnotations = []string{"storageclass.kubernetes.io/is-default-class", "storageclass.beta.kubernetes.io/is-default-class"}

func (l *clusterLookup) StorageClass(name string) (*storagev1.StorageClass, error) {
	classes := l.client.StorageV1().StorageClasses()
	if name != "" {
		sc, err := classes.Get(context.TODO(), name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return sc, err
	}
	list, err := classes.List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for i := range list.Items {
		for _, a := range defaultClassAnnotations {
			if list.Items[i].Annotations[a] == "true" {
				return &list.Items[i], nil
			}
		}
	}
	return nil, nil
}

func (l *clusterLookup) ResourceQuotas(ns string) ([]*v1.ResourceQuota, error) {
	list, err := l.client.CoreV1().ResourceQuotas(ns).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return ptrs(list.Items), nil
}

func (l *clusterLookup) PDBs(ns string) ([]*policyv1.PodDisruptionBudget, error) {
	list, err := l.client.PolicyV1().PodDisruptionBudgets(ns).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return ptrs(list.Items), nil
}

func (l *clusterLookup) Image(ctx context.Context, image string) (string, string) {
	return l.images.check(ctx, image)
}

// HTTPDoer sends HTTP requests; *http.Client is one.
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// imageChecker checks, best effort, that an image exists: first on the
// nodes, then by asking its registry for the manifest anonymously. It
// never sends credentials and never reads pull secrets: a private image
// is reported as "can't check", and the kubelet is the first to pull it.
type imageChecker struct {
	nodes corelisters.NodeLister
	http  HTTPDoer
	// scheme is "https"; tests use "http".
	scheme string
}

const registryTimeout = 5 * time.Second

func (i *imageChecker) check(ctx context.Context, image string) (string, string) {
	named, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return api.ResultFail, fmt.Sprintf("%q is not a valid image reference: %v", image, err)
	}
	if node := i.onNode(named); node != "" {
		return api.ResultClear, fmt.Sprintf("%s is already on node %s", image, node)
	}
	if i.http == nil {
		return api.ResultInfo, fmt.Sprintf("%s is not on any node yet; its first pull happens at birth", image)
	}
	ref := ""
	if d, ok := named.(reference.Digested); ok {
		ref = d.Digest().String()
	} else {
		ref = reference.TagNameOnly(named).(reference.Tagged).Tag()
	}
	host := reference.Domain(named)
	if host == "docker.io" {
		host = "registry-1.docker.io"
	}
	scheme := i.scheme
	if scheme == "" {
		scheme = "https"
	}
	manifest := fmt.Sprintf("%s://%s/v2/%s/manifests/%s", scheme, host, reference.Path(named), ref)

	ctx, cancel := context.WithTimeout(ctx, registryTimeout)
	defer cancel()
	resp, err := i.head(ctx, manifest, "")
	if err != nil {
		return api.ResultInfo, fmt.Sprintf("couldn't reach %s to check %s: %v", host, image, err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		if token, ok := i.anonymousToken(ctx, resp.Header.Get("WWW-Authenticate"), scheme); ok {
			if resp, err = i.head(ctx, manifest, token); err != nil {
				return api.ResultInfo, fmt.Sprintf("couldn't reach %s to check %s: %v", host, image, err)
			}
		}
	}
	switch resp.StatusCode {
	case http.StatusOK:
		msg := fmt.Sprintf("the registry knows %s", image)
		if digest := resp.Header.Get("Docker-Content-Digest"); digest != "" && !strings.Contains(image, "@") {
			msg += " (" + digest + ")"
		}
		return api.ResultClear, msg
	case http.StatusNotFound:
		return api.ResultFail, fmt.Sprintf("the registry says %s doesn't exist", image)
	case http.StatusUnauthorized, http.StatusForbidden:
		return api.ResultInfo, fmt.Sprintf("%s is private. I don't read pull secrets, so I can't check it; the kubelet will try with them at birth", image)
	}
	return api.ResultInfo, fmt.Sprintf("the registry answered %d for %s", resp.StatusCode, image)
}

func (i *imageChecker) head(ctx context.Context, url, token string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", strings.Join([]string{
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.docker.distribution.manifest.v2+json",
	}, ", "))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := i.http.Do(req)
	if err != nil {
		return nil, err
	}
	resp.Body.Close()
	return resp, nil
}

var challengeParam = regexp.MustCompile(`(\w+)="([^"]*)"`)

// anonymousToken follows a Bearer challenge without credentials, as
// `docker pull` of a public image does.
func (i *imageChecker) anonymousToken(ctx context.Context, challenge, scheme string) (string, bool) {
	if !strings.HasPrefix(strings.ToLower(challenge), "bearer ") {
		return "", false
	}
	params := map[string]string{}
	for _, m := range challengeParam.FindAllStringSubmatch(challenge, -1) {
		params[strings.ToLower(m[1])] = m[2]
	}
	realm, err := url.Parse(params["realm"])
	if err != nil || realm.Scheme != scheme || realm.Host == "" {
		return "", false
	}
	q := realm.Query()
	for _, k := range []string{"service", "scope"} {
		if params[k] != "" {
			q.Set(k, params[k])
		}
	}
	realm.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return "", false
	}
	resp, err := i.http.Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", false
	}
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body) != nil {
		return "", false
	}
	if body.Token != "" {
		return body.Token, true
	}
	return body.AccessToken, body.AccessToken != ""
}

// onNode returns a node that already has the image, or "".
func (i *imageChecker) onNode(named reference.Named) string {
	if i.nodes == nil {
		return ""
	}
	want := map[string]bool{}
	if d, ok := named.(reference.Digested); ok {
		want[named.Name()+"@"+d.Digest().String()] = true
	} else {
		want[reference.TagNameOnly(named).String()] = true
	}
	nodes, err := i.nodes.List(labels.Everything())
	if err != nil {
		return ""
	}
	for _, n := range nodes {
		for _, img := range n.Status.Images {
			for _, name := range img.Names {
				if want[name] {
					return n.Name
				}
				if norm, err := reference.ParseNormalizedNamed(name); err == nil && want[norm.String()] {
					return n.Name
				}
			}
		}
	}
	return ""
}
