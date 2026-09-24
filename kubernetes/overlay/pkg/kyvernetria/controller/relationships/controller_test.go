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
	"context"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clienttesting "k8s.io/client-go/testing"
)

func established(crd *apiextensionsv1.CustomResourceDefinition) *apiextensionsv1.CustomResourceDefinition {
	crd.Status.Conditions = []apiextensionsv1.CustomResourceDefinitionCondition{
		{Type: apiextensionsv1.Established, Status: apiextensionsv1.ConditionTrue},
	}
	return crd
}

func TestEnsureCRDUpdatesAnOutdatedDefinition(t *testing.T) {
	old := established(CRD())
	old.Spec.Names.Categories = []string{"all"}
	old.Spec.Conversion = &apiextensionsv1.CustomResourceConversion{Strategy: apiextensionsv1.NoneConverter}
	client := fake.NewSimpleClientset(old)
	c := &Controller{crds: client}

	if err := c.ensureCRD(context.Background()); err == nil {
		t.Fatal("an outdated definition was accepted as is")
	}
	got, err := client.ApiextensionsV1().CustomResourceDefinitions().Get(context.Background(), old.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Spec.Names.Categories) != 0 {
		t.Errorf("categories not removed: %v", got.Spec.Names.Categories)
	}

	client.ClearActions()
	if err := c.ensureCRD(context.Background()); err != nil {
		t.Fatalf("up-to-date definition: %v", err)
	}
	for _, a := range client.Actions() {
		if a.GetVerb() != "get" {
			t.Errorf("up-to-date definition was written: %v", a.(clienttesting.Action).GetVerb())
		}
	}
}

func TestCRDHasNoAllCategory(t *testing.T) {
	for _, c := range CRD().Spec.Names.Categories {
		if c == "all" {
			t.Error("relationships are in the all category: kubectl delete all --all would delete them")
		}
	}
}
