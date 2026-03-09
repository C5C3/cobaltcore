// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package config

import (
	"testing"

	. "github.com/onsi/gomega"

	envtestutil "github.com/c5c3/forge/internal/common/testutil/envtest"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Feature: CC-0005

func TestIntegration_CreateImmutableConfigMap(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := envtestutil.SetupEnvTest(t)
	scheme := envtestutil.SharedScheme()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-config-create"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	owner := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "config-owner", Namespace: ns.Name},
	}
	g.Expect(c.Create(ctx, owner)).To(Succeed())

	data := map[string]string{"keystone.conf": "[DEFAULT]\ndebug = true\n"}

	name, err := CreateImmutableConfigMap(ctx, c, scheme, owner, "keystone-config", ns.Name, data)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(name).To(HavePrefix("keystone-config-"))

	// Verify the ConfigMap was actually created with correct properties.
	var cm corev1.ConfigMap
	g.Expect(c.Get(ctx, client.ObjectKey{Name: name, Namespace: ns.Name}, &cm)).To(Succeed())
	g.Expect(cm.Data).To(Equal(data))
	g.Expect(*cm.Immutable).To(BeTrue())
	g.Expect(cm.OwnerReferences).To(HaveLen(1))
	g.Expect(cm.OwnerReferences[0].Name).To(Equal("config-owner"))
}

func TestIntegration_CreateImmutableConfigMap_immutabilityFlag(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := envtestutil.SetupEnvTest(t)
	scheme := envtestutil.SharedScheme()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-config-immutable"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	owner := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "immut-owner", Namespace: ns.Name},
	}
	g.Expect(c.Create(ctx, owner)).To(Succeed())

	data := map[string]string{"app.conf": "setting = value"}
	name, err := CreateImmutableConfigMap(ctx, c, scheme, owner, "immut-cfg", ns.Name, data)
	g.Expect(err).NotTo(HaveOccurred())

	var cm corev1.ConfigMap
	g.Expect(c.Get(ctx, client.ObjectKey{Name: name, Namespace: ns.Name}, &cm)).To(Succeed())
	g.Expect(cm.Immutable).NotTo(BeNil())
	g.Expect(*cm.Immutable).To(BeTrue(), "ConfigMap must be immutable")
}

func TestIntegration_CreateImmutableConfigMap_ownerReference(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := envtestutil.SetupEnvTest(t)
	scheme := envtestutil.SharedScheme()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-config-owner"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	owner := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "owner-ref-test", Namespace: ns.Name},
	}
	g.Expect(c.Create(ctx, owner)).To(Succeed())

	data := map[string]string{"conf": "data"}
	name, err := CreateImmutableConfigMap(ctx, c, scheme, owner, "ownerref-cfg", ns.Name, data)
	g.Expect(err).NotTo(HaveOccurred())

	var cm corev1.ConfigMap
	g.Expect(c.Get(ctx, client.ObjectKey{Name: name, Namespace: ns.Name}, &cm)).To(Succeed())
	g.Expect(cm.OwnerReferences).To(HaveLen(1))
	g.Expect(cm.OwnerReferences[0].Name).To(Equal("owner-ref-test"))
	g.Expect(*cm.OwnerReferences[0].Controller).To(BeTrue())
}

func TestIntegration_CreateImmutableConfigMap_idempotent(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := envtestutil.SetupEnvTest(t)
	scheme := envtestutil.SharedScheme()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-config-idem"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	owner := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "idem-owner", Namespace: ns.Name},
	}
	g.Expect(c.Create(ctx, owner)).To(Succeed())

	data := map[string]string{"key": "value"}

	name1, err := CreateImmutableConfigMap(ctx, c, scheme, owner, "idem-cfg", ns.Name, data)
	g.Expect(err).NotTo(HaveOccurred())

	name2, err := CreateImmutableConfigMap(ctx, c, scheme, owner, "idem-cfg", ns.Name, data)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(name2).To(Equal(name1), "idempotent calls must return the same name")

	// Only one ConfigMap should exist with this name.
	var cm corev1.ConfigMap
	g.Expect(c.Get(ctx, client.ObjectKey{Name: name1, Namespace: ns.Name}, &cm)).To(Succeed())
}
