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
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"k8s.io/kubernetes/pkg/kyvernetria"
)

// ManagedByLabel marks what the gestation controller installs.
const (
	ManagedByLabel = kyvernetria.Prefix + "managed-by"
	ManagedBy      = "gestation-controller"
)

// CRD is the Gestation API definition installed by the controller.
func CRD() *apiextensionsv1.CustomResourceDefinition {
	str := apiextensionsv1.JSONSchemaProps{Type: "string"}
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name:   Resource + "." + Group,
			Labels: map[string]string{ManagedByLabel: ManagedBy},
		},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: Group,
			Scope: apiextensionsv1.NamespaceScoped,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural: Resource, Singular: "gestation", Kind: Kind, ListKind: Kind + "List",
				// No "all" category: kubectl delete all --all must not
				// take the launch plan with the workloads.
				ShortNames: []string{"gest"},
			},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name: Version, Served: true, Storage: true,
				Subresources: &apiextensionsv1.CustomResourceSubresources{
					Status: &apiextensionsv1.CustomResourceSubresourceStatus{},
				},
				Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
					Type:        "object",
					Description: "A service on its way to launch: room reserved ahead of time, prenatal screening, the Apgar score at launch, and newborn care.",
					Properties: map[string]apiextensionsv1.JSONSchemaProps{
						"apiVersion": str, "kind": str,
						"metadata": {Type: "object"},
						"spec": {
							Type:     "object",
							Required: []string{"deployment", "due", "size", "replicas"},
							Properties: map[string]apiextensionsv1.JSONSchemaProps{
								"deployment": {Type: "string", MinLength: ptr.To[int64](1), MaxLength: ptr.To[int64](253)},
								"due":        {Type: "string", Pattern: `^[0-9]{4}-[0-9]{2}-[0-9]{2}$`},
								"size": {
									Type:     "object",
									Required: []string{"cpu", "memory"},
									Properties: map[string]apiextensionsv1.JSONSchemaProps{
										"cpu": str, "memory": str,
									},
								},
								"replicas":        {Type: "integer", Format: "int32", Minimum: ptr.To[float64](1), Maximum: ptr.To[float64](1000)},
								"screenRequested": {Type: "string", Format: "date-time"},
								"delivery":        {Type: "integer", Format: "int32", Minimum: ptr.To[float64](0)},
								"autoRollback":    {Type: "boolean"},
							},
						},
						"status": {Type: "object", XPreserveUnknownFields: ptr.To(true)},
					},
				}},
				AdditionalPrinterColumns: []apiextensionsv1.CustomResourceColumnDefinition{
					{Name: "Deployment", Type: "string", JSONPath: ".spec.deployment"},
					{Name: "Due", Type: "string", JSONPath: ".spec.due"},
					{Name: "Phase", Type: "string", JSONPath: ".status.phase"},
					{Name: "Reserved", Type: "integer", JSONPath: ".status.reserved"},
					{Name: "Message", Type: "string", JSONPath: ".status.message", Priority: 1},
					{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
				},
			}},
		},
	}
}
