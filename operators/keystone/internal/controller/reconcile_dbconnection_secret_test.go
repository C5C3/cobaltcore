// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"net/url"
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
		"mysql+pymysql://ks_user:ks_pass@db.example.com:3306/keystone?charset=utf8",
	))
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
		"mysql+pymysql://test-keystone:secret123@mariadb-cluster.default.svc:3306/keystone?charset=utf8",
	))
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

// --- CC-0106 task 4.1: DB-TLS DSN parameters ---
//
// These tests assert that reconcileDBConnectionSecret appends the pymysql ssl_*
// query parameters produced by modeToSSLParams (CC-0106, REQ-004) to the DSN
// when spec.database.tls is enabled, while leaving the pre-CC-0106 behaviour
// (charset=utf8 only) unchanged for the plaintext path. The merged query string
// is asserted via url.Values rather than substring matching so test stability
// does not depend on url.Values.Encode()'s lexical key ordering.

// parseDSNQuery extracts the query parameters from a pymysql DSN. Using
// url.ParseQuery keeps the assertions independent of the encoded ordering
// returned by url.Values.Encode().
func parseDSNQuery(t *testing.T, dsn string) url.Values {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse DSN %q: %v", dsn, err)
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		t.Fatalf("parse RawQuery %q: %v", u.RawQuery, err)
	}
	return q
}

func TestReconcileDBConnectionSecret_TLSDisabled_NoSSLParams(t *testing.T) {
	g := NewGomegaWithT(t)
	s := configTestScheme()
	ctx := context.Background()

	ks := configTestKeystone()
	// TLS is nil by default — assert no ssl_* parameters leak into the DSN.
	upstream := dbCredentialsSecret("default", "keystone-db-credentials", "ks_user", "ks_pass")
	r := newConfigTestReconciler(s, ks, upstream)

	_, err := r.reconcileDBConnectionSecret(ctx, ks)
	g.Expect(err).NotTo(HaveOccurred())

	derived := &corev1.Secret{}
	g.Expect(r.Get(ctx, client.ObjectKey{
		Namespace: "default",
		Name:      derivedDBConnectionSecretName(ks.Name),
	}, derived)).To(Succeed())

	q := parseDSNQuery(t, string(derived.Data[dbConnectionSecretKey]))
	g.Expect(q.Get("charset")).To(Equal("utf8"))
	for _, key := range []string{"ssl_ca", "ssl_cert", "ssl_key", "ssl_verify_cert", "ssl_verify_identity"} {
		g.Expect(q.Has(key)).To(BeFalse(), "ssl_* parameter %q must be absent when TLS is disabled", key)
	}
}

func TestReconcileDBConnectionSecret_TLSEnabled_AppendsModeSSLParams(t *testing.T) {
	cases := []struct {
		mode           string
		wantVerifyCert bool
		wantVerifyID   bool
	}{
		{"prefer", false, false},
		{"require", false, false},
		{"verify-ca", true, false},
		{"verify-full", true, true},
	}

	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			g := NewGomegaWithT(t)
			s := configTestScheme()
			ctx := context.Background()

			ks := configTestKeystone()
			ks.Spec.Database.TLS = &commonv1.DatabaseTLSSpec{
				Enabled:             true,
				Mode:                tc.mode,
				CABundleSecretRef:   commonv1.SecretRefSpec{Name: "db-ca-bundle"},
				ClientCertSecretRef: commonv1.SecretRefSpec{Name: "test-keystone-db-client"},
			}
			upstream := dbCredentialsSecret("default", "keystone-db-credentials", "ks_user", "ks_pass")
			r := newConfigTestReconciler(s, ks, upstream)

			_, err := r.reconcileDBConnectionSecret(ctx, ks)
			g.Expect(err).NotTo(HaveOccurred())

			derived := &corev1.Secret{}
			g.Expect(r.Get(ctx, client.ObjectKey{
				Namespace: "default",
				Name:      derivedDBConnectionSecretName(ks.Name),
			}, derived)).To(Succeed())

			q := parseDSNQuery(t, string(derived.Data[dbConnectionSecretKey]))

			// charset=utf8 (the pre-CC-0106 parameter) must survive the merge.
			g.Expect(q.Get("charset")).To(Equal("utf8"))

			// The ssl_ca/ssl_cert/ssl_key triple is present for every mode and
			// must match the canonical /etc/keystone/db-tls/ mount layout.
			wantPaths := dbTLSPathsForMount()
			g.Expect(q.Get("ssl_ca")).To(Equal(wantPaths.CA))
			g.Expect(q.Get("ssl_cert")).To(Equal(wantPaths.Cert))
			g.Expect(q.Get("ssl_key")).To(Equal(wantPaths.Key))

			if tc.wantVerifyCert {
				g.Expect(q.Get("ssl_verify_cert")).To(Equal("true"))
			} else {
				g.Expect(q.Has("ssl_verify_cert")).To(BeFalse(),
					"ssl_verify_cert must be absent for mode %q", tc.mode)
			}
			if tc.wantVerifyID {
				g.Expect(q.Get("ssl_verify_identity")).To(Equal("true"))
			} else {
				g.Expect(q.Has("ssl_verify_identity")).To(BeFalse(),
					"ssl_verify_identity must be absent for mode %q", tc.mode)
			}
		})
	}
}

// TestReconcileDBConnectionSecret_TLSEnabledButDisabledFlag_NoSSLParams covers
// the spec.database.tls != nil && !enabled edge case: the block is materialised
// (e.g. by a user toggling enabled to false to test rollback) but the DSN must
// stay plaintext. Mirrors the NotRequired path in reconcileDatabaseTLS.
func TestReconcileDBConnectionSecret_TLSEnabledButDisabledFlag_NoSSLParams(t *testing.T) {
	g := NewGomegaWithT(t)
	s := configTestScheme()
	ctx := context.Background()

	ks := configTestKeystone()
	ks.Spec.Database.TLS = &commonv1.DatabaseTLSSpec{
		Enabled:             false,
		Mode:                "verify-full",
		CABundleSecretRef:   commonv1.SecretRefSpec{Name: "db-ca-bundle"},
		ClientCertSecretRef: commonv1.SecretRefSpec{Name: "test-keystone-db-client"},
	}
	upstream := dbCredentialsSecret("default", "keystone-db-credentials", "ks_user", "ks_pass")
	r := newConfigTestReconciler(s, ks, upstream)

	_, err := r.reconcileDBConnectionSecret(ctx, ks)
	g.Expect(err).NotTo(HaveOccurred())

	derived := &corev1.Secret{}
	g.Expect(r.Get(ctx, client.ObjectKey{
		Namespace: "default",
		Name:      derivedDBConnectionSecretName(ks.Name),
	}, derived)).To(Succeed())

	q := parseDSNQuery(t, string(derived.Data[dbConnectionSecretKey]))
	g.Expect(q.Get("charset")).To(Equal("utf8"))
	for _, key := range []string{"ssl_ca", "ssl_cert", "ssl_key", "ssl_verify_cert", "ssl_verify_identity"} {
		g.Expect(q.Has(key)).To(BeFalse(),
			"ssl_* parameter %q must be absent when tls.enabled is false", key)
	}
}

// TestReconcileDBConnectionSecret_TLSEnabled_NoPercentEncodedSlashes guards
// against a regression where url.Values.Encode percent-encodes "/" in the
// ssl_ca/ssl_cert/ssl_key file paths as "%2F". keystone-manage db_sync hands
// the DSN to alembic's ConfigParser, which interprets "%" as interpolation
// syntax and aborts the db_sync Job with "invalid interpolation syntax".
func TestReconcileDBConnectionSecret_TLSEnabled_NoPercentEncodedSlashes(t *testing.T) {
	g := NewGomegaWithT(t)
	s := configTestScheme()
	ctx := context.Background()

	ks := configTestKeystone()
	ks.Spec.Database.TLS = &commonv1.DatabaseTLSSpec{
		Enabled:             true,
		Mode:                "verify-full",
		CABundleSecretRef:   commonv1.SecretRefSpec{Name: "db-ca-bundle"},
		ClientCertSecretRef: commonv1.SecretRefSpec{Name: "test-keystone-db-client"},
	}
	upstream := dbCredentialsSecret("default", "keystone-db-credentials", "ks_user", "ks_pass")
	r := newConfigTestReconciler(s, ks, upstream)

	_, err := r.reconcileDBConnectionSecret(ctx, ks)
	g.Expect(err).NotTo(HaveOccurred())

	derived := &corev1.Secret{}
	g.Expect(r.Get(ctx, client.ObjectKey{
		Namespace: "default",
		Name:      derivedDBConnectionSecretName(ks.Name),
	}, derived)).To(Succeed())

	connStr := string(derived.Data[dbConnectionSecretKey])
	g.Expect(connStr).NotTo(ContainSubstring("%2F"),
		"DSN must not contain percent-encoded slashes — they trip Python configparser interpolation in keystone-manage db_sync")
	g.Expect(connStr).NotTo(ContainSubstring("%2f"),
		"DSN must not contain percent-encoded slashes — they trip Python configparser interpolation in keystone-manage db_sync")
	g.Expect(connStr).To(ContainSubstring("ssl_ca=/etc/keystone/db-tls/ca.crt"))
	g.Expect(connStr).To(ContainSubstring("ssl_cert=/etc/keystone/db-tls/tls.crt"))
	g.Expect(connStr).To(ContainSubstring("ssl_key=/etc/keystone/db-tls/tls.key"))
}
