// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package policy

// Feature: CC-0005

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	envtestutil "github.com/c5c3/forge/internal/common/testutil/envtest"
)

// createNamespace creates a unique namespace in the cluster for test isolation.
func createNamespace(ctx context.Context, g *GomegaWithT, c client.Client, name string) *corev1.Namespace {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())
	return ns
}

// ---------------------------------------------------------------------------
// LoadPolicyFromConfigMap integration tests
// ---------------------------------------------------------------------------

func TestIntegration_LoadPolicyFromConfigMap_SuccessfulLoad(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, cancel := envtestutil.SetupEnvTest(t)
	defer cancel()

	ns := createNamespace(ctx, g, c, "test-policy-load")

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "policy-cm",
			Namespace: ns.Name,
		},
		Data: map[string]string{
			"policy.yaml": "compute:create: role:admin\nidentity:list_users: role:reader\n",
		},
	}
	g.Expect(c.Create(ctx, cm)).To(Succeed())

	rules, err := LoadPolicyFromConfigMap(ctx, c, ns.Name, "policy-cm")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(rules).To(HaveLen(2))
	g.Expect(rules).To(HaveKeyWithValue("compute:create", "role:admin"))
	g.Expect(rules).To(HaveKeyWithValue("identity:list_users", "role:reader"))
}

func TestIntegration_LoadPolicyFromConfigMap_NotFound(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, cancel := envtestutil.SetupEnvTest(t)
	defer cancel()

	createNamespace(ctx, g, c, "test-policy-notfound")

	_, err := LoadPolicyFromConfigMap(ctx, c, "test-policy-notfound", "nonexistent")
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("getting ConfigMap"))
}

func TestIntegration_LoadPolicyFromConfigMap_MissingKey(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, cancel := envtestutil.SetupEnvTest(t)
	defer cancel()

	ns := createNamespace(ctx, g, c, "test-policy-nokey")

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "no-policy-key",
			Namespace: ns.Name,
		},
		Data: map[string]string{
			"other.yaml": "key: value",
		},
	}
	g.Expect(c.Create(ctx, cm)).To(Succeed())

	_, err := LoadPolicyFromConfigMap(ctx, c, ns.Name, "no-policy-key")
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring(`does not contain key "policy.yaml"`))
}

func TestIntegration_LoadPolicyFromConfigMap_InvalidYAML(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, cancel := envtestutil.SetupEnvTest(t)
	defer cancel()

	ns := createNamespace(ctx, g, c, "test-policy-badyaml")

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bad-yaml",
			Namespace: ns.Name,
		},
		Data: map[string]string{
			"policy.yaml": "not: [valid: yaml: {{{",
		},
	}
	g.Expect(c.Create(ctx, cm)).To(Succeed())

	_, err := LoadPolicyFromConfigMap(ctx, c, ns.Name, "bad-yaml")
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("parsing policy.yaml"))
}
