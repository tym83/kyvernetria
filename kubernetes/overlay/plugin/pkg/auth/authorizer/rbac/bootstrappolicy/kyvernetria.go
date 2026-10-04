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

package bootstrappolicy

import (
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	rbacv1helpers "k8s.io/kubernetes/pkg/apis/rbac/v1"
)

const (
	kyvernetriaGroup           = "kyvernetria.io"
	kyvernetriaExtensionsGroup = "apiextensions.k8s.io"
)

// kyvernetriaSchedulerRules lets the PlaceMemory plugin read the memory
// kept on Deployments.
func kyvernetriaSchedulerRules() []rbacv1.PolicyRule {
	return []rbacv1.PolicyRule{
		rbacv1helpers.NewRule(Read...).Groups(appsGroup).Resources("deployments").RuleOrDie(),
	}
}

// addKyvernetriaControllerRoles registers the roles of the Kyvernetria
// controllers in kube-controller-manager.
func addKyvernetriaControllerRoles(roles *[]rbacv1.ClusterRole, bindings *[]rbacv1.ClusterRoleBinding) {
	addControllerRole(roles, bindings, rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: saRolePrefix + "kyvernetria-gestation"},
		Rules: []rbacv1.PolicyRule{
			rbacv1helpers.NewRule("get", "create", "update").Groups(kyvernetriaExtensionsGroup).Resources("customresourcedefinitions").RuleOrDie(),
			rbacv1helpers.NewRule("get", "list", "watch").Groups(kyvernetriaGroup).Resources("gestations").RuleOrDie(),
			rbacv1helpers.NewRule("update").Groups(kyvernetriaGroup).Resources("gestations/status").RuleOrDie(),
			// Dependencies for the memory of a service: names only.
			rbacv1helpers.NewRule("list", "watch").Groups(kyvernetriaGroup).Resources("relationships").RuleOrDie(),
			// Placeholders, birth, rollback and newborn care.
			rbacv1helpers.NewRule("get", "create", "update", "delete").Groups(appsGroup).Resources("deployments").RuleOrDie(),
			rbacv1helpers.NewRule("get", "list", "watch", "create", "delete").Groups(policyGroup).Resources("poddisruptionbudgets").RuleOrDie(),
			rbacv1helpers.NewRule("get", "create").Groups(schedulingGroup).Resources("priorityclasses").RuleOrDie(),
			rbacv1helpers.NewRule("list", "watch").Groups(autoscalingGroup).Resources("horizontalpodautoscalers").RuleOrDie(),
			// The newborn-care annotation, and kyvernetria-system.
			rbacv1helpers.NewRule("get", "create", "patch").Groups(legacyGroup).Resources("namespaces").RuleOrDie(),
			// Prenatal screening. Secrets and ConfigMaps are fetched as
			// metadata only: RBAC has no metadata-only verb, the code
			// never asks for their data.
			rbacv1helpers.NewRule("get").Groups(legacyGroup).Resources("secrets", "configmaps", "persistentvolumeclaims").RuleOrDie(),
			rbacv1helpers.NewRule("list", "watch").Groups(legacyGroup).Resources("resourcequotas", "events").RuleOrDie(),
			rbacv1helpers.NewRule("get", "list", "watch").Groups(storageGroup).Resources("storageclasses").RuleOrDie(),
			// The memory of services that left.
			rbacv1helpers.NewRule("create").Groups(legacyGroup).Resources("configmaps").RuleOrDie(),
			rbacv1helpers.NewRule("update").Groups(legacyGroup).Resources("configmaps").Names("kyvernetria-microchimerism").RuleOrDie(),
			// Growth charts.
			rbacv1helpers.NewRule("list", "watch").Groups(resMetricsGroup).Resources("pods").RuleOrDie(),
			eventsRule(),
		},
	})
	addControllerRole(roles, bindings, rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: saRolePrefix + "kyvernetria-place-memory"},
		Rules: []rbacv1.PolicyRule{
			rbacv1helpers.NewRule("get", "patch").Groups(appsGroup).Resources("deployments", "statefulsets", "replicasets").RuleOrDie(),
			eventsRule(),
		},
	})
	addControllerRole(roles, bindings, rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: saRolePrefix + "kyvernetria-relationships"},
		Rules: []rbacv1.PolicyRule{
			rbacv1helpers.NewRule("get", "create", "update").Groups(kyvernetriaExtensionsGroup).Resources("customresourcedefinitions").RuleOrDie(),
			rbacv1helpers.NewRule("get", "list", "watch", "create", "update", "delete").Groups(kyvernetriaGroup).Resources("relationships").RuleOrDie(),
			eventsRule(),
		},
	})
	addControllerRole(roles, bindings, rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: saRolePrefix + "kyvernetria-worry"},
		Rules: []rbacv1.PolicyRule{
			// Remembers its worries on the node (kyvernetria.io/worried).
			rbacv1helpers.NewRule("patch").Groups(legacyGroup).Resources("nodes").RuleOrDie(),
			eventsRule(),
		},
	})
}
