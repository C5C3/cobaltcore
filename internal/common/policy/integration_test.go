// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package policy

import (
	"testing"

	. "github.com/onsi/gomega"

	envtestutil "github.com/c5c3/forge/internal/common/testutil/envtest"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Feature: CC-0005

func TestIntegration_LoadPolicyFromConfigMap_success(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := envtestutil.SetupEnvTest(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-policy-load"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-policy",
			Namespace: ns.Name,
		},
		Data: map[string]string{
			PolicyConfigMapKey: "compute:create: role:admin\ncompute:delete: role:admin\n",
		},
	}
	g.Expect(c.Create(ctx, cm)).To(Succeed())

	rules, err := LoadPolicyFromConfigMap(ctx, c, client.ObjectKey{Name: "test-policy", Namespace: ns.Name})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(rules).To(Equal(map[string]string{
		"compute:create": "role:admin",
		"compute:delete": "role:admin",
	}))
}

func TestIntegration_LoadPolicyFromConfigMap_notFound(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := envtestutil.SetupEnvTest(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-policy-notfound"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	_, err := LoadPolicyFromConfigMap(ctx, c, client.ObjectKey{Name: "missing", Namespace: ns.Name})
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("getting ConfigMap"))
}

func TestIntegration_LoadPolicyFromConfigMap_missingKey(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := envtestutil.SetupEnvTest(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-policy-nokey"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "no-key-policy",
			Namespace: ns.Name,
		},
		Data: map[string]string{
			"other.yaml": "some: data\n",
		},
	}
	g.Expect(c.Create(ctx, cm)).To(Succeed())

	_, err := LoadPolicyFromConfigMap(ctx, c, client.ObjectKey{Name: "no-key-policy", Namespace: ns.Name})
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring(PolicyConfigMapKey))
}

func TestIntegration_LoadPolicyFromConfigMap_invalidYAML(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := envtestutil.SetupEnvTest(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-policy-badyaml"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bad-yaml-policy",
			Namespace: ns.Name,
		},
		Data: map[string]string{
			PolicyConfigMapKey: ": : : not valid yaml [[[",
		},
	}
	g.Expect(c.Create(ctx, cm)).To(Succeed())

	_, err := LoadPolicyFromConfigMap(ctx, c, client.ObjectKey{Name: "bad-yaml-policy", Namespace: ns.Name})
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("parsing " + PolicyConfigMapKey))
}
