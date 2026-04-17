// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Feature: CC-0080

package controller

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1 "github.com/c5c3/forge/internal/common/types"
)

// TestReconcileDBConnectionSecret_CreatesSecretWithCorrectURL_Brownfield
// verifies that the sub-reconciler creates the derived
// <keystone>-db-connection Secret using the upstream credentials and the
// brownfield Host/Port for the PyMySQL URL (CC-0080, REQ-002, REQ-010).
func TestReconcileDBConnectionSecret_CreatesSecretWithCorrectURL_Brownfield(t *testing.T) {
	g := NewGomegaWithT(t)
	s := configTestScheme()
	ctx := context.Background()

	ks := configTestKeystone()
	upstream := dbCredentialsSecret("default", "keystone-db-credentials", "ks_user", "ks_pass")
	r := newConfigTestReconciler(s, ks, upstream)

	res, err := r.reconcileDBConnectionSecret(ctx, ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(BeZero())

	derived := &corev1.Secret{}
	g.Expect(r.Client.Get(ctx, client.ObjectKey{Namespace: "default", Name: "test-keystone-db-connection"}, derived)).To(Succeed())

	g.Expect(derived.Type).To(Equal(corev1.SecretTypeOpaque))
	g.Expect(string(derived.Data["connection"])).To(Equal(
		"mysql+pymysql://ks_user:ks_pass@db.example.com:3306/keystone?charset=utf8"))

	g.Expect(derived.OwnerReferences).To(HaveLen(1))
	g.Expect(derived.OwnerReferences[0].Name).To(Equal("test-keystone"))
	g.Expect(derived.OwnerReferences[0].UID).To(Equal(ks.UID))
}

// TestReconcileDBConnectionSecret_CreatesSecretWithCorrectURL_Managed
// verifies that in managed mode the username is the Keystone CR name and the
// host is the MariaDB cluster service DNS (CC-0080, REQ-002, REQ-010).
func TestReconcileDBConnectionSecret_CreatesSecretWithCorrectURL_Managed(t *testing.T) {
	g := NewGomegaWithT(t)
	s := configTestScheme()
	ctx := context.Background()

	ks := configTestKeystone()
	ks.Spec.Database = commonv1.DatabaseSpec{
		ClusterRef: &corev1.LocalObjectReference{Name: "mariadb-cluster"},
		Database:   "keystone",
		SecretRef:  commonv1.SecretRefSpec{Name: "keystone-db-credentials"},
	}
	upstream := dbCredentialsSecret("default", "keystone-db-credentials", "keystone", "secret123")
	r := newConfigTestReconciler(s, ks, upstream)

	res, err := r.reconcileDBConnectionSecret(ctx, ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(BeZero())

	derived := &corev1.Secret{}
	g.Expect(r.Client.Get(ctx, client.ObjectKey{Namespace: "default", Name: "test-keystone-db-connection"}, derived)).To(Succeed())

	g.Expect(derived.Type).To(Equal(corev1.SecretTypeOpaque))
	g.Expect(string(derived.Data["connection"])).To(Equal(
		"mysql+pymysql://test-keystone:secret123@mariadb-cluster.default.svc:3306/keystone?charset=utf8"))
}

// TestReconcileDBConnectionSecret_UpdatesOnPasswordRotation verifies that the
// sub-reconciler is idempotent and rewrites Data["connection"] when the
// upstream password rotates, preserving Name/UID (CC-0080, REQ-002).
func TestReconcileDBConnectionSecret_UpdatesOnPasswordRotation(t *testing.T) {
	g := NewGomegaWithT(t)
	s := configTestScheme()
	ctx := context.Background()

	ks := configTestKeystone()
	upstream := dbCredentialsSecret("default", "keystone-db-credentials", "ks_user", "old")
	r := newConfigTestReconciler(s, ks, upstream)

	_, err := r.reconcileDBConnectionSecret(ctx, ks)
	g.Expect(err).NotTo(HaveOccurred())

	first := &corev1.Secret{}
	g.Expect(r.Client.Get(ctx, client.ObjectKey{Namespace: "default", Name: "test-keystone-db-connection"}, first)).To(Succeed())
	g.Expect(string(first.Data["connection"])).To(ContainSubstring("old"))
	firstUID := first.UID
	firstName := first.Name

	// Rotate upstream password.
	upstreamUpdated := &corev1.Secret{}
	g.Expect(r.Client.Get(ctx, client.ObjectKey{Namespace: "default", Name: "keystone-db-credentials"}, upstreamUpdated)).To(Succeed())
	upstreamUpdated.Data["password"] = []byte("new")
	g.Expect(r.Client.Update(ctx, upstreamUpdated)).To(Succeed())

	_, err = r.reconcileDBConnectionSecret(ctx, ks)
	g.Expect(err).NotTo(HaveOccurred())

	second := &corev1.Secret{}
	g.Expect(r.Client.Get(ctx, client.ObjectKey{Namespace: "default", Name: "test-keystone-db-connection"}, second)).To(Succeed())
	g.Expect(string(second.Data["connection"])).To(ContainSubstring("new"))
	g.Expect(string(second.Data["connection"])).NotTo(ContainSubstring("old"))
	g.Expect(second.Name).To(Equal(firstName))
	g.Expect(second.UID).To(Equal(firstUID))
}

// TestReconcileDBConnectionSecret_UpstreamSecretMissing_RequeueAndCondition
// verifies that when the upstream DB credentials Secret does not exist, the
// sub-reconciler returns a requeue without error and does not create the
// derived Secret (CC-0080, REQ-002).
func TestReconcileDBConnectionSecret_UpstreamSecretMissing_RequeueAndCondition(t *testing.T) {
	g := NewGomegaWithT(t)
	s := configTestScheme()
	ctx := context.Background()

	ks := configTestKeystone()
	// No upstream secret.
	r := newConfigTestReconciler(s, ks)

	res, err := r.reconcileDBConnectionSecret(ctx, ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueSecretPolling))

	// Derived Secret must NOT exist.
	derived := &corev1.Secret{}
	getErr := r.Get(ctx, client.ObjectKey{Namespace: "default", Name: "test-keystone-db-connection"}, derived)
	g.Expect(apierrors.IsNotFound(getErr)).To(BeTrue())
}

// TestReconcile_NoPushSecretOrExternalSecretForDBConnection verifies that the
// sub-reconciler creates a plain Kubernetes Secret — no ExternalSecret or
// PushSecret — for the derived connection Secret (CC-0080, REQ-010).
func TestReconcile_NoPushSecretOrExternalSecretForDBConnection(t *testing.T) {
	g := NewGomegaWithT(t)
	s := configTestScheme()
	ctx := context.Background()

	ks := configTestKeystone()
	upstream := dbCredentialsSecret("default", "keystone-db-credentials", "ks_user", "ks_pass")
	r := newConfigTestReconciler(s, ks, upstream)

	_, err := r.reconcileDBConnectionSecret(ctx, ks)
	g.Expect(err).NotTo(HaveOccurred())

	// Only the upstream and derived corev1.Secret should exist in the namespace.
	var secretList corev1.SecretList
	g.Expect(r.Client.List(ctx, &secretList, client.InNamespace("default"))).To(Succeed())

	names := map[types.NamespacedName]bool{}
	for _, sec := range secretList.Items {
		names[types.NamespacedName{Namespace: sec.Namespace, Name: sec.Name}] = true
	}
	g.Expect(names).To(HaveKey(types.NamespacedName{Namespace: "default", Name: "keystone-db-credentials"}))
	g.Expect(names).To(HaveKey(types.NamespacedName{Namespace: "default", Name: "test-keystone-db-connection"}))
	// Derived is the only non-upstream secret.
	g.Expect(secretList.Items).To(HaveLen(2))
}
