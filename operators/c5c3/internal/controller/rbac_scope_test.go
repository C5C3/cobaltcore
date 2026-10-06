// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	. "github.com/onsi/gomega"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

// TestRBAC_DeploymentGrantIsScopedToKORC guards least privilege on Deployments:
// exactly one ClusterRole rule may reach apps/deployments, the get/patch rule
// scoped to orc-controller-manager. It matches by membership, so a rule
// controller-gen merged with another resource still counts. The chart's
// ClusterRole mirrors config/rbac/role.yaml (make verify-helm-rbac), so this
// covers the shipped chart too.
func TestRBAC_DeploymentGrantIsScopedToKORC(t *testing.T) {
	g := NewGomegaWithT(t)
	data, err := os.ReadFile(filepath.Join("..", "..", "config", "rbac", "role.yaml"))
	g.Expect(err).NotTo(HaveOccurred())
	var role rbacv1.ClusterRole
	g.Expect(yaml.Unmarshal(data, &role)).To(Succeed())

	var reaching []rbacv1.PolicyRule
	for _, rule := range role.Rules {
		if (slices.Contains(rule.APIGroups, "apps") || slices.Contains(rule.APIGroups, "*")) &&
			(slices.Contains(rule.Resources, "deployments") || slices.Contains(rule.Resources, "*")) {
			reaching = append(reaching, rule)
		}
	}
	g.Expect(reaching).To(Equal([]rbacv1.PolicyRule{{
		APIGroups:     []string{"apps"},
		Resources:     []string{"deployments"},
		ResourceNames: []string{korcDeploymentName},
		Verbs:         []string{"get", "patch"},
	}}))
}
