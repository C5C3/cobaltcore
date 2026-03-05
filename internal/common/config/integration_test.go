// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package config

// Feature: CC-0005

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	envtestutil "github.com/c5c3/forge/internal/common/testutil/envtest"
)

// createNamespace creates a unique namespace in the cluster for test isolation.
func createNamespace(ctx context.Context, g *GomegaWithT, c client.Client, name string) *corev1.Namespace {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())
	return ns
}

// createOwner creates a ConfigMap via the API server so it gets a real UID assigned.
func createOwner(ctx context.Context, g *GomegaWithT, c client.Client, namespace string) *corev1.ConfigMap {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-owner",
			Namespace: namespace,
		},
	}
	g.Expect(c.Create(ctx, cm)).To(Succeed())
	return cm
}

// ---------------------------------------------------------------------------
// CreateImmutableConfigMap integration tests
// ---------------------------------------------------------------------------

func TestIntegration_CreateImmutableConfigMap_CreatesInCluster(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, cancel := envtestutil.SetupEnvTest(t)
	defer cancel()

	ns := createNamespace(ctx, g, c, "test-immcm-create")
	owner := createOwner(ctx, g, c, ns.Name)

	data := map[string]string{"keystone.conf": "[DEFAULT]\ndebug = true\n"}

	cm, err := CreateImmutableConfigMap(ctx, c, owner, "keystone-config", data)
	g.Expect(err).NotTo(HaveOccurred())

	// Verify the ConfigMap was created with hashed name.
	hash := hashConfigMapData(data)
	expectedName := "keystone-config-" + hash[:8]
	g.Expect(cm.Name).To(Equal(expectedName))
	g.Expect(cm.Namespace).To(Equal(ns.Name))

	// Verify it exists in the cluster.
	fetched := &corev1.ConfigMap{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: expectedName, Namespace: ns.Name}, fetched)).To(Succeed())

	// Verify immutability flag.
	g.Expect(fetched.Immutable).NotTo(BeNil())
	g.Expect(*fetched.Immutable).To(BeTrue())

	// Verify data.
	g.Expect(fetched.Data).To(Equal(data))

	// Verify owner reference.
	g.Expect(fetched.OwnerReferences).To(HaveLen(1))
	g.Expect(fetched.OwnerReferences[0].Name).To(Equal("test-owner"))
	g.Expect(fetched.OwnerReferences[0].UID).To(Equal(owner.UID))
}

func TestIntegration_CreateImmutableConfigMap_Idempotent(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, cancel := envtestutil.SetupEnvTest(t)
	defer cancel()

	ns := createNamespace(ctx, g, c, "test-immcm-idempotent")
	owner := createOwner(ctx, g, c, ns.Name)

	data := map[string]string{"nova.conf": "[DEFAULT]\nlog_file = /var/log/nova.log\n"}

	// Create the ConfigMap.
	cm1, err := CreateImmutableConfigMap(ctx, c, owner, "nova-config", data)
	g.Expect(err).NotTo(HaveOccurred())

	// Create again with the same data — should succeed and return same name.
	cm2, err := CreateImmutableConfigMap(ctx, c, owner, "nova-config", data)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(cm1.Name).To(Equal(cm2.Name), "same data must produce same ConfigMap name")

	// Verify only one ConfigMap exists with that name.
	fetched := &corev1.ConfigMap{}
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(cm1), fetched)).To(Succeed())
}

func TestIntegration_CreateImmutableConfigMap_DifferentDataDifferentName(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, cancel := envtestutil.SetupEnvTest(t)
	defer cancel()

	ns := createNamespace(ctx, g, c, "test-immcm-diffdata")
	owner := createOwner(ctx, g, c, ns.Name)

	cm1, err := CreateImmutableConfigMap(ctx, c, owner, "cfg", map[string]string{"key": "value1"})
	g.Expect(err).NotTo(HaveOccurred())

	cm2, err := CreateImmutableConfigMap(ctx, c, owner, "cfg", map[string]string{"key": "value2"})
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(cm1.Name).NotTo(Equal(cm2.Name), "different data must produce different ConfigMap names")
}
