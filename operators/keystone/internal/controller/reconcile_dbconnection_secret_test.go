// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"strings"
	"testing"

	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1 "github.com/c5c3/forge/internal/common/types"
)

// derivedDBConnectionSecretName mirrors the naming pattern in
// reconcileDBConnectionSecret; using a helper avoids drift between tests and
// production code.
func derivedDBConnectionSecretName(keystoneName string) string {
	return keystoneName + "-db-connection"
}

func TestReconcileDBConnectionSecret_CreatesSecretWithCorrectURL_Brownfield(t *testing.T) {
	g := NewGomegaWithT(t)
	s := configTestScheme()
	ctx := context.Background()

	ks := configTestKeystone()
	upstream := dbCredentialsSecret("default", "keystone-db-credentials", "ks_user", "ks_pass")
	r := newConfigTestReconciler(s, ks, upstream)

	result, err := r.reconcileDBConnectionSecret(ctx, ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(BeZero())

	derived := &corev1.Secret{}
	g.Expect(r.Get(ctx, client.ObjectKey{
		Namespace: "default",
		Name:      derivedDBConnectionSecretName(ks.Name),
	}, derived)).To(Succeed())

	g.Expect(derived.Type).To(Equal(corev1.SecretTypeOpaque))
	g.Expect(derived.OwnerReferences).To(HaveLen(1))
	g.Expect(derived.OwnerReferences[0].UID).To(Equal(ks.UID))
	g.Expect(derived.Data).To(HaveLen(1))
	g.Expect(string(derived.Data[dbConnectionSecretKey])).To(Equal(
		"mysql+pymysql://ks_user:ks_pass@db.example.com:3306/keystone?charset=utf8"))
}

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
	// Managed mode ignores the username key; only password is read.
	upstream := dbCredentialsSecret("default", "keystone-db-credentials", "ignored", "secret123")
	r := newConfigTestReconciler(s, ks, upstream)

	result, err := r.reconcileDBConnectionSecret(ctx, ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(BeZero())

	derived := &corev1.Secret{}
	g.Expect(r.Get(ctx, client.ObjectKey{
		Namespace: "default",
		Name:      derivedDBConnectionSecretName(ks.Name),
	}, derived)).To(Succeed())

	g.Expect(derived.Data).To(HaveLen(1))
	g.Expect(string(derived.Data[dbConnectionSecretKey])).To(Equal(
		"mysql+pymysql://test-keystone:secret123@mariadb-cluster.default.svc:3306/keystone?charset=utf8"))
}

func TestReconcileDBConnectionSecret_UpdatesOnPasswordRotation(t *testing.T) {
	g := NewGomegaWithT(t)
	s := configTestScheme()
	ctx := context.Background()

	ks := configTestKeystone()
	upstream := dbCredentialsSecret("default", "keystone-db-credentials", "ks_user", "old")
	r := newConfigTestReconciler(s, ks, upstream)

	// First reconcile: derived Secret created with the old password.
	_, err := r.reconcileDBConnectionSecret(ctx, ks)
	g.Expect(err).NotTo(HaveOccurred())

	derivedKey := client.ObjectKey{
		Namespace: "default",
		Name:      derivedDBConnectionSecretName(ks.Name),
	}
	first := &corev1.Secret{}
	g.Expect(r.Get(ctx, derivedKey, first)).To(Succeed())
	g.Expect(string(first.Data[dbConnectionSecretKey])).To(ContainSubstring(":old@"))
	originalUID := first.UID

	// Rotate the upstream password and reconcile again.
	current := &corev1.Secret{}
	g.Expect(r.Get(ctx, client.ObjectKey{Namespace: "default", Name: "keystone-db-credentials"}, current)).To(Succeed())
	current.Data["password"] = []byte("new")
	g.Expect(r.Update(ctx, current)).To(Succeed())

	_, err = r.reconcileDBConnectionSecret(ctx, ks)
	g.Expect(err).NotTo(HaveOccurred())

	second := &corev1.Secret{}
	g.Expect(r.Get(ctx, derivedKey, second)).To(Succeed())
	g.Expect(second.Name).To(Equal(first.Name))
	g.Expect(second.UID).To(Equal(originalUID))
	g.Expect(second.Data).To(HaveLen(1))
	connStr := string(second.Data[dbConnectionSecretKey])
	g.Expect(connStr).To(ContainSubstring(":new@"))
	g.Expect(connStr).NotTo(ContainSubstring(":old@"))
}

func TestReconcileDBConnectionSecret_UpstreamSecretMissing_RequeueAndCondition(t *testing.T) {
	g := NewGomegaWithT(t)
	s := configTestScheme()
	ctx := context.Background()

	ks := configTestKeystone()
	// Deliberately do NOT seed the upstream credentials Secret.
	r := newConfigTestReconciler(s, ks)

	result, err := r.reconcileDBConnectionSecret(ctx, ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueSecretPolling))

	// No derived Secret should have been created.
	derived := &corev1.Secret{}
	getErr := r.Get(ctx, client.ObjectKey{
		Namespace: "default",
		Name:      derivedDBConnectionSecretName(ks.Name),
	}, derived)
	g.Expect(apierrors.IsNotFound(getErr)).To(BeTrue(),
		"derived Secret must not exist when upstream credentials are missing")

	// Status condition reflects the wait.
	var found *metav1.Condition
	for i := range ks.Status.Conditions {
		if ks.Status.Conditions[i].Type == "SecretsReady" {
			found = &ks.Status.Conditions[i]
			break
		}
	}
	g.Expect(found).NotTo(BeNil(), "SecretsReady condition must be set")
	g.Expect(found.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(found.Reason).To(Equal("WaitingForDBCredentials"))
}

// TestReconcileDBConnectionSecret_UpstreamSecretMissingKey_RequeueAndCondition
// exercises the ErrKeyNotFound branch of secrets.IsMissingSecretOrKey at the
// controller level: the upstream credentials Secret exists but is missing the
// "password" data key (brownfield mode). The reconciler must requeue with a
// SecretsReady=False / WaitingForDBCredentials condition and never write a
// derived Secret with partial credentials (CC-0080, REQ-002).
func TestReconcileDBConnectionSecret_UpstreamSecretMissingKey_RequeueAndCondition(t *testing.T) {
	g := NewGomegaWithT(t)
	s := configTestScheme()
	ctx := context.Background()

	ks := configTestKeystone()
	// Upstream Secret is present but lacks the "password" key — the
	// reconciler reads "username" first (succeeds) then "password" (missing),
	// hitting the ErrKeyNotFound branch of IsMissingSecretOrKey.
	upstream := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "keystone-db-credentials",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"username": []byte("ks_user"),
		},
	}
	r := newConfigTestReconciler(s, ks, upstream)

	result, err := r.reconcileDBConnectionSecret(ctx, ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueSecretPolling))

	// No derived Secret should have been created when a required data key is
	// absent: partial credentials must never be materialised.
	derived := &corev1.Secret{}
	getErr := r.Get(ctx, client.ObjectKey{
		Namespace: "default",
		Name:      derivedDBConnectionSecretName(ks.Name),
	}, derived)
	g.Expect(apierrors.IsNotFound(getErr)).To(BeTrue(),
		"derived Secret must not exist when upstream Secret is missing a required key")

	var found *metav1.Condition
	for i := range ks.Status.Conditions {
		if ks.Status.Conditions[i].Type == "SecretsReady" {
			found = &ks.Status.Conditions[i]
			break
		}
	}
	g.Expect(found).NotTo(BeNil(), "SecretsReady condition must be set")
	g.Expect(found.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(found.Reason).To(Equal("WaitingForDBCredentials"))
	g.Expect(found.Message).To(ContainSubstring(`"password"`))
}

// TestReconcile_NoPushSecretOrExternalSecretForDBConnection asserts REQ-010:
// the derived db-connection Secret must be a plain corev1.Secret with no
// PushSecret or ExternalSecret involvement. We verify by listing all Secrets
// with the derived name in the namespace and confirming exactly one exists
// (the corev1.Secret created by the reconciler itself).
func TestReconcile_NoPushSecretOrExternalSecretForDBConnection(t *testing.T) {
	g := NewGomegaWithT(t)
	s := configTestScheme()
	ctx := context.Background()

	ks := configTestKeystone()
	upstream := dbCredentialsSecret("default", "keystone-db-credentials", "ks_user", "ks_pass")
	r := newConfigTestReconciler(s, ks, upstream)

	_, err := r.reconcileDBConnectionSecret(ctx, ks)
	g.Expect(err).NotTo(HaveOccurred())

	derivedName := derivedDBConnectionSecretName(ks.Name)
	var list corev1.SecretList
	g.Expect(r.List(ctx, &list, client.InNamespace("default"))).To(Succeed())

	matching := 0
	for _, item := range list.Items {
		if strings.HasPrefix(item.Name, derivedName) {
			matching++
		}
	}
	g.Expect(matching).To(Equal(1),
		"exactly one corev1.Secret should manage the db-connection material; no PushSecret/ExternalSecret intermediates")
}
