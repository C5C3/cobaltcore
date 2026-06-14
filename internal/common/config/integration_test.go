// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package config

import (
	"fmt"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/gomega"

	envtestutil "github.com/c5c3/forge/internal/common/testutil/envtest"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// creationTimestampGranularity is slightly over one second: Kubernetes
// serializes metadata.creationTimestamp as RFC3339 which truncates to whole
// seconds, so ConfigMaps created within the same second share a timestamp and
// cannot be ordered by CreationTimestamp. Sleeping this amount between creates
// guarantees distinct, monotonically increasing timestamps in envtest.
const creationTimestampGranularity = 1100 * time.Millisecond

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

func TestIntegration_PruneImmutableConfigMaps(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := envtestutil.SetupEnvTest(t)
	scheme := envtestutil.SharedScheme()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-prune-cm"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	owner := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "prune-owner", Namespace: ns.Name},
	}
	g.Expect(c.Create(ctx, owner)).To(Succeed())

	const baseName = "prune-test-config"

	// Create 5 ConfigMaps with different data to get different hash suffixes.
	// Sleep between creates so each ConfigMap receives a distinct
	// CreationTimestamp second; pruning is ordered by CreationTimestamp and
	// would otherwise be non-deterministic across same-second creations.
	var firstCreatedName, currentName string
	for i := 1; i <= 5; i++ {
		if i > 1 {
			time.Sleep(creationTimestampGranularity)
		}
		data := map[string]string{"key": fmt.Sprintf("value-%d", i)}
		name, err := CreateImmutableConfigMap(ctx, c, scheme, owner, baseName, ns.Name, data)
		g.Expect(err).NotTo(HaveOccurred())
		if i == 1 {
			firstCreatedName = name
		}
		currentName = name
	}

	// Re-read the owner so we have the UID assigned by the API server.
	g.Expect(c.Get(ctx, client.ObjectKey{Name: owner.Name, Namespace: ns.Name}, owner)).To(Succeed())

	err := PruneImmutableConfigMaps(ctx, c, owner, PruneOptions{BaseName: baseName, Namespace: ns.Name, CurrentName: currentName, Retain: 3})
	g.Expect(err).NotTo(HaveOccurred())

	// List remaining ConfigMaps that match the baseName prefix.
	var cmList corev1.ConfigMapList
	g.Expect(c.List(ctx, &cmList, client.InNamespace(ns.Name))).To(Succeed())

	var remaining []string
	for _, cm := range cmList.Items {
		if strings.HasPrefix(cm.Name, baseName+"-") {
			remaining = append(remaining, cm.Name)
		}
	}

	g.Expect(remaining).To(HaveLen(4), "current + 3 retained historical ConfigMaps expected")
	g.Expect(remaining).To(ContainElement(currentName), "current ConfigMap must survive pruning")
	g.Expect(remaining).NotTo(ContainElement(firstCreatedName), "oldest ConfigMap should have been pruned")
}

func TestIntegration_PruneImmutableConfigMaps_idempotent(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := envtestutil.SetupEnvTest(t)
	scheme := envtestutil.SharedScheme()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-prune-idem"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	owner := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "prune-idem-owner", Namespace: ns.Name},
	}
	g.Expect(c.Create(ctx, owner)).To(Succeed())

	const baseName = "prune-idem-config"

	// See comment in TestIntegration_PruneImmutableConfigMaps: sleeping between
	// creates ensures distinct CreationTimestamps so pruning order is stable.
	var firstCreatedName, currentName string
	for i := 1; i <= 5; i++ {
		if i > 1 {
			time.Sleep(creationTimestampGranularity)
		}
		data := map[string]string{"key": fmt.Sprintf("value-%d", i)}
		name, err := CreateImmutableConfigMap(ctx, c, scheme, owner, baseName, ns.Name, data)
		g.Expect(err).NotTo(HaveOccurred())
		if i == 1 {
			firstCreatedName = name
		}
		currentName = name
	}

	// Re-read the owner so we have the UID assigned by the API server.
	g.Expect(c.Get(ctx, client.ObjectKey{Name: owner.Name, Namespace: ns.Name}, owner)).To(Succeed())

	// First prune call.
	err := PruneImmutableConfigMaps(ctx, c, owner, PruneOptions{BaseName: baseName, Namespace: ns.Name, CurrentName: currentName, Retain: 3})
	g.Expect(err).NotTo(HaveOccurred())

	// Second prune call (idempotent).
	err = PruneImmutableConfigMaps(ctx, c, owner, PruneOptions{BaseName: baseName, Namespace: ns.Name, CurrentName: currentName, Retain: 3})
	g.Expect(err).NotTo(HaveOccurred())

	// Verify the same result after both calls.
	var cmList corev1.ConfigMapList
	g.Expect(c.List(ctx, &cmList, client.InNamespace(ns.Name))).To(Succeed())

	var remaining []string
	for _, cm := range cmList.Items {
		if strings.HasPrefix(cm.Name, baseName+"-") {
			remaining = append(remaining, cm.Name)
		}
	}

	g.Expect(remaining).To(HaveLen(4), "current + 3 retained historical ConfigMaps expected after idempotent prune")
	g.Expect(remaining).To(ContainElement(currentName), "current ConfigMap must survive pruning")
	g.Expect(remaining).NotTo(ContainElement(firstCreatedName), "oldest ConfigMap should have been pruned")
}
