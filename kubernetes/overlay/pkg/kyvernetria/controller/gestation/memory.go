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
	"time"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"k8s.io/kubernetes/pkg/kyvernetria"
	api "k8s.io/kubernetes/pkg/kyvernetria/gestation"
)

// relationshipsGVR is read for a service's dependencies. The gestation
// package does not import the relationships controller, to stay small.
var relationshipsGVR = schema.GroupVersionResource{Group: "kyvernetria.io", Version: "v1alpha1", Resource: "relationships"}

// memory is the microchimerism ConfigMap during one pass.
type memory struct {
	cm      *v1.ConfigMap // nil until it exists
	data    map[string]string
	changed bool
	now     time.Time
}

func (c *Controller) loadMemory(ctx context.Context) (*memory, error) {
	m := &memory{data: map[string]string{}, now: c.now()}
	cm, err := c.client.CoreV1().ConfigMaps(kyvernetria.SystemNamespace).Get(ctx, api.MemoryConfigMap, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return m, nil
	case err != nil:
		return nil, fmt.Errorf("reading the cluster's memory: %w", err)
	}
	m.cm = cm
	for k, v := range cm.Data {
		m.data[k] = v
	}
	return m, nil
}

// put stores what the cluster knows of a living service. The birth date
// and departure of an existing record are kept.
func (m *memory) put(r api.Record) {
	var old api.Record
	if raw, ok := m.data[r.Key()]; ok && json.Unmarshal([]byte(raw), &old) == nil {
		if old.Born != "" {
			r.Born = old.Born
		}
		if r.Left == "" {
			r.Left = old.Left
		}
	}
	if api.Put(m.data, r) {
		m.changed = true
	}
}

// depart closes the record of a service that has left.
func (m *memory) depart(namespace, name, uid string, at time.Time) {
	key := api.Record{Namespace: namespace, Name: name, UID: uid}.Key()
	var r api.Record
	if raw, ok := m.data[key]; !ok || json.Unmarshal([]byte(raw), &r) != nil || r.Departed() {
		return
	}
	r.Left = api.Stamp(at)
	if api.Put(m.data, r) {
		m.changed = true
	}
}

// closeDeparted closes the records of services that left while nobody
// was watching them, for example after their Gestation was deleted.
func (c *Controller) closeDeparted(m *memory) {
	for _, r := range api.Records(m.data) {
		if r.Departed() {
			continue
		}
		d, err := c.deployments.Deployments(r.Namespace).Get(r.Name)
		if apierrors.IsNotFound(err) || (err == nil && string(d.UID) != r.UID) {
			m.depart(r.Namespace, r.Name, r.UID, m.now)
		}
	}
}

func (c *Controller) saveMemory(ctx context.Context, m *memory) error {
	if !m.changed {
		return nil
	}
	if m.cm != nil {
		updated := m.cm.DeepCopy()
		updated.Data = m.data
		_, err := c.client.CoreV1().ConfigMaps(kyvernetria.SystemNamespace).Update(ctx, updated, metav1.UpdateOptions{FieldManager: fieldManager})
		return err
	}
	_, err := c.client.CoreV1().Namespaces().Create(ctx, &v1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: kyvernetria.SystemNamespace, Labels: map[string]string{api.ManagedByLabel: api.ManagedBy},
	}}, metav1.CreateOptions{FieldManager: fieldManager})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	_, err = c.client.CoreV1().ConfigMaps(kyvernetria.SystemNamespace).Create(ctx, &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: api.MemoryConfigMap, Namespace: kyvernetria.SystemNamespace,
			Labels:      map[string]string{api.ManagedByLabel: api.ManagedBy},
			Annotations: map[string]string{"kyvernetria.io/description": "Services this cluster gave birth to. " + api.Remembrance},
		},
		Data: m.data,
	}, metav1.CreateOptions{FieldManager: fieldManager})
	return err
}

// remember keeps the record of a living service current.
func (c *Controller) remember(ctx context.Context, g *api.Gestation, d *appsv1.Deployment, m *memory) {
	r := api.Record{
		Namespace: d.Namespace, Name: d.Name, UID: string(d.UID),
		Born:         api.Stamp(g.Status.Birth.Time.Time),
		ConfigDigest: api.ConfigDigest(&d.Spec.Template),
		Dependencies: c.dependencies(ctx, d),
	}
	if cg := g.Status.Caregivers; cg != nil {
		r.Owners = []string{cg.Primary, cg.Secondary}
	}
	m.put(r)
}

// dependencies names what a service needs: the Secrets and ConfigMaps
// its pods refer to and the services it talks to, never their contents.
func (c *Controller) dependencies(ctx context.Context, d *appsv1.Deployment) []string {
	secrets, configMaps := api.References(&d.Spec.Template)
	var out []string
	for _, s := range secrets {
		out = append(out, "secret/"+s)
	}
	for _, cm := range configMaps {
		out = append(out, "configmap/"+cm)
	}
	if c.dynamic == nil {
		return out
	}
	rels, err := c.dynamic.Resource(relationshipsGVR).Namespace(d.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return out // no Relationship API: names from the pod template only
	}
	for i := range rels.Items {
		rel := &rels.Items[i]
		fromKind, _, _ := unstructured.NestedString(rel.Object, "spec", "from", "kind")
		fromName, _, _ := unstructured.NestedString(rel.Object, "spec", "from", "name")
		typ, _, _ := unstructured.NestedString(rel.Object, "spec", "type")
		if fromKind != "Deployment" || fromName != d.Name || typ != "talks-to" {
			continue
		}
		kind, _, _ := unstructured.NestedString(rel.Object, "spec", "to", "kind")
		ns, _, _ := unstructured.NestedString(rel.Object, "spec", "to", "namespace")
		name, _, _ := unstructured.NestedString(rel.Object, "spec", "to", "name")
		if kind == "Service" {
			kind = "svc"
		}
		if ns != "" && ns != d.Namespace {
			name = ns + "/" + name
		}
		out = append(out, kind+"/"+name)
	}
	return out
}
