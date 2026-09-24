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

// Package relationships makes the links between services a first-class
// object of the cluster.
//
// Models: the largest well-replicated sex difference in interests,
// people-orientation versus things-orientation (Su, Rounds & Armstrong 2009).
// Upstream Kubernetes is built around things: pods are cattle. Kyvernetria
// keeps the things, and adds the relationships between them to the API:
// `kubectl get relationships`.
package relationships

import (
	"context"
	"fmt"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	appsinformers "k8s.io/client-go/informers/apps/v1"
	coreinformers "k8s.io/client-go/informers/core/v1"
	appslisters "k8s.io/client-go/listers/apps/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"k8s.io/kubernetes/pkg/kyvernetria"
)

// Group, version and resource of the Relationship API.
const (
	Group    = "kyvernetria.io"
	Version  = "v1alpha1"
	Resource = "relationships"
	Kind     = "Relationship"
)

// GVR is the Relationship resource.
var GVR = schema.GroupVersionResource{Group: Group, Version: Version, Resource: Resource}

const managedByLabel = kyvernetria.Prefix + "managed-by"
const managedBy = "relationships-controller"

// Controller keeps Relationship objects in step with the cluster.
type Controller struct {
	crds         apiextensionsclient.Interface
	dynamic      dynamic.Interface
	services     corelisters.ServiceLister
	deployments  appslisters.DeploymentLister
	statefulSets appslisters.StatefulSetLister
	daemonSets   appslisters.DaemonSetLister
	synced       []cache.InformerSynced
	period       time.Duration
}

// New creates the controller.
func New(crds apiextensionsclient.Interface, dyn dynamic.Interface, svcs coreinformers.ServiceInformer,
	deploys appsinformers.DeploymentInformer, sts appsinformers.StatefulSetInformer, ds appsinformers.DaemonSetInformer) *Controller {
	return &Controller{
		crds:         crds,
		dynamic:      dyn,
		services:     svcs.Lister(),
		deployments:  deploys.Lister(),
		statefulSets: sts.Lister(),
		daemonSets:   ds.Lister(),
		synced: []cache.InformerSynced{
			svcs.Informer().HasSynced, deploys.Informer().HasSynced,
			sts.Informer().HasSynced, ds.Informer().HasSynced,
		},
		period: 30 * time.Second,
	}
}

// Run installs the API and reconciles until ctx is done.
func (c *Controller) Run(ctx context.Context) {
	defer utilruntime.HandleCrash()
	logger := klog.FromContext(ctx)
	logger.Info("Starting kyvernetria relationships controller")
	defer logger.Info("Shutting down kyvernetria relationships controller")
	if !cache.WaitForNamedCacheSync("kyvernetria-relationships", ctx.Done(), c.synced...) {
		return
	}
	wait.UntilWithContext(ctx, func(ctx context.Context) {
		if err := c.ensureCRD(ctx); err != nil {
			utilruntime.HandleErrorWithContext(ctx, err, "Installing the Relationship API failed")
			return
		}
		if err := c.reconcile(ctx); err != nil {
			utilruntime.HandleErrorWithContext(ctx, err, "Reconciling relationships failed")
		}
	}, c.period)
}

func (c *Controller) ensureCRD(ctx context.Context) error {
	crd := CRD()
	crds := c.crds.ApiextensionsV1().CustomResourceDefinitions()
	existing, err := crds.Get(ctx, crd.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = crds.Create(ctx, crd, metav1.CreateOptions{})
		if err == nil {
			return fmt.Errorf("relationship API created, waiting for it to be served")
		}
		return err
	}
	if err != nil {
		return err
	}
	if !specUpToDate(existing, crd) {
		// An older release installed a different definition (for example
		// with the "all" category): bring it in line.
		updated := existing.DeepCopy()
		updated.Spec = crd.Spec
		updated.Spec.Conversion = existing.Spec.Conversion // defaulted by the server
		if _, err := crds.Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("updating the relationship API: %w", err)
		}
		return fmt.Errorf("relationship API updated, waiting for it to be served")
	}
	for _, cond := range existing.Status.Conditions {
		if cond.Type == apiextensionsv1.Established && cond.Status == apiextensionsv1.ConditionTrue {
			return nil
		}
	}
	return fmt.Errorf("relationship API is not established yet")
}

// specUpToDate compares the parts of the definition the controller owns;
// fields the server defaults are taken from the existing object.
func specUpToDate(existing, want *apiextensionsv1.CustomResourceDefinition) bool {
	w := want.Spec.DeepCopy()
	w.Conversion = existing.Spec.Conversion
	w.PreserveUnknownFields = existing.Spec.PreserveUnknownFields
	return apiequality.Semantic.DeepEqual(&existing.Spec, w)
}

func (c *Controller) reconcile(ctx context.Context) error {
	services, err := c.services.List(labels.Everything())
	if err != nil {
		return err
	}
	deploys, err := c.deployments.List(labels.Everything())
	if err != nil {
		return err
	}
	sts, err := c.statefulSets.List(labels.Everything())
	if err != nil {
		return err
	}
	ds, err := c.daemonSets.List(labels.Everything())
	if err != nil {
		return err
	}

	desired := map[string]*unstructured.Unstructured{}
	for _, r := range Graph(services, Workloads(deploys, sts, ds)) {
		obj := toObject(r)
		desired[obj.GetNamespace()+"/"+obj.GetName()] = obj
	}

	res := c.dynamic.Resource(GVR)
	current, err := res.Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{LabelSelector: managedByLabel + "=" + managedBy})
	if err != nil {
		return err
	}
	var errs []error
	for i := range current.Items {
		item := &current.Items[i]
		key := item.GetNamespace() + "/" + item.GetName()
		want, ok := desired[key]
		if !ok {
			if err := res.Namespace(item.GetNamespace()).Delete(ctx, item.GetName(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				errs = append(errs, err)
			}
			continue
		}
		delete(desired, key)
		if equalSpec(item, want) {
			continue
		}
		want.SetResourceVersion(item.GetResourceVersion())
		if _, err := res.Namespace(want.GetNamespace()).Update(ctx, want, metav1.UpdateOptions{}); err != nil {
			errs = append(errs, err)
		}
	}
	for _, obj := range desired {
		if _, err := res.Namespace(obj.GetNamespace()).Create(ctx, obj, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%d relationship writes failed, first: %w", len(errs), errs[0])
	}
	return nil
}

func equalSpec(a, b *unstructured.Unstructured) bool {
	as, _, _ := unstructured.NestedMap(a.Object, "spec")
	bs, _, _ := unstructured.NestedMap(b.Object, "spec")
	return fmt.Sprint(as) == fmt.Sprint(bs)
}

func refMap(r Ref) map[string]interface{} {
	return map[string]interface{}{"kind": r.Kind, "namespace": r.Namespace, "name": r.Name}
}

func toObject(r Relationship) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": Group + "/" + Version,
		"kind":       Kind,
		"metadata": map[string]interface{}{
			"name":      Name(r),
			"namespace": r.From.Namespace,
			"labels":    map[string]interface{}{managedByLabel: managedBy},
		},
		"spec": map[string]interface{}{
			"from":     refMap(r.From),
			"to":       refMap(r.To),
			"type":     r.Type,
			"evidence": r.Evidence,
		},
	}}
	if r.From.UID != "" {
		apiVersion := "apps/v1"
		if r.From.Kind == "Service" {
			apiVersion = "v1"
		}
		obj.SetOwnerReferences([]metav1.OwnerReference{{
			APIVersion: apiVersion, Kind: r.From.Kind, Name: r.From.Name, UID: r.From.UID,
		}})
	}
	return obj
}

// CRD is the Relationship API definition installed by the controller.
func CRD() *apiextensionsv1.CustomResourceDefinition {
	str := apiextensionsv1.JSONSchemaProps{Type: "string"}
	ref := apiextensionsv1.JSONSchemaProps{
		Type:     "object",
		Required: []string{"kind", "name"},
		Properties: map[string]apiextensionsv1.JSONSchemaProps{
			"kind": str, "namespace": str, "name": str,
		},
	}
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name:   Resource + "." + Group,
			Labels: map[string]string{managedByLabel: managedBy},
		},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: Group,
			Scope: apiextensionsv1.NamespaceScoped,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural: Resource, Singular: "relationship", Kind: Kind, ListKind: Kind + "List",
				// No "all" category: kubectl delete all --all must not
				// delete the relationships along with the workloads.
				ShortNames: []string{"rel", "rels"},
			},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name: Version, Served: true, Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
					Type:        "object",
					Description: "A relationship between two parties in the cluster, derived from what the cluster already declares.",
					Properties: map[string]apiextensionsv1.JSONSchemaProps{
						"apiVersion": str, "kind": str,
						"metadata": {Type: "object"},
						"spec": {
							Type:     "object",
							Required: []string{"from", "to", "type"},
							Properties: map[string]apiextensionsv1.JSONSchemaProps{
								"from": ref, "to": ref,
								"type": {Type: "string", Enum: []apiextensionsv1.JSON{
									{Raw: []byte(`"` + Serves + `"`)}, {Raw: []byte(`"` + TalksTo + `"`)},
								}},
								"evidence": str,
							},
						},
					},
				}},
				AdditionalPrinterColumns: []apiextensionsv1.CustomResourceColumnDefinition{
					{Name: "From", Type: "string", JSONPath: ".spec.from.name"},
					{Name: "Relationship", Type: "string", JSONPath: ".spec.type"},
					{Name: "To", Type: "string", JSONPath: ".spec.to.name"},
					{Name: "Evidence", Type: "string", JSONPath: ".spec.evidence", Priority: 1},
					{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
				},
			}},
		},
	}
}
