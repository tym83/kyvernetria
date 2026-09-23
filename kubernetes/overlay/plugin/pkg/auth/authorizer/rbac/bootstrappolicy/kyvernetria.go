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
		ObjectMeta: metav1.ObjectMeta{Name: saRolePrefix + "kyvernetria-place-memory"},
		Rules: []rbacv1.PolicyRule{
			rbacv1helpers.NewRule("get", "patch").Groups(appsGroup).Resources("deployments", "statefulsets", "replicasets").RuleOrDie(),
			eventsRule(),
		},
	})
	addControllerRole(roles, bindings, rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: saRolePrefix + "kyvernetria-relationships"},
		Rules: []rbacv1.PolicyRule{
			rbacv1helpers.NewRule("get", "create").Groups(kyvernetriaExtensionsGroup).Resources("customresourcedefinitions").RuleOrDie(),
			rbacv1helpers.NewRule("get", "list", "watch", "create", "update", "delete").Groups(kyvernetriaGroup).Resources("relationships").RuleOrDie(),
			eventsRule(),
		},
	})
	addControllerRole(roles, bindings, rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: saRolePrefix + "kyvernetria-worry"},
		Rules: []rbacv1.PolicyRule{
			eventsRule(),
		},
	})
}
