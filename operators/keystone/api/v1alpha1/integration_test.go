// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package v1alpha1

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	commonv1 "github.com/c5c3/forge/internal/common/types"
	"github.com/c5c3/forge/operators/keystone/internal/testutil"
)

// --- Helpers ---

// setupEnvTest wraps testutil.SetupKeystoneEnvTest with the v1alpha1 scheme
// registration and webhook setup, avoiding the import cycle between testutil
// and this package.
func setupEnvTest(t testing.TB) (client.Client, context.Context, context.CancelFunc) {
	t.Helper()
	return testutil.SetupKeystoneEnvTest(t, AddToScheme, func(mgr ctrl.Manager) error {
		// mgr.GetAPIReader() mirrors the production wiring in main.go: webhook
		// admission lookups read the API server directly, never a stale cache.
		return (&KeystoneWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr)
	})
}

// validIntegrationKeystone returns a valid Keystone CR suitable for envtest
// integration tests. It uses the same field values as validKeystone() from
// keystone_webhook_test.go but adds ObjectMeta for API server submission.
func validIntegrationKeystone(name, namespace string) *Keystone {
	k := validKeystone()
	k.Name = name
	k.Namespace = namespace
	return k
}

// --- Task 2.1: CRD installation and valid CR acceptance tests ---

func TestIntegration_CRDInstalled(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTest(t)

	crdList := &apiextensionsv1.CustomResourceDefinitionList{}
	g.Expect(c.List(ctx, crdList)).To(Succeed())

	installed := make(map[string]bool, len(crdList.Items))
	for _, crd := range crdList.Items {
		installed[crd.Name] = true
	}

	const expectedCRD = "keystones.keystone.openstack.c5c3.io"
	g.Expect(installed).To(HaveKey(expectedCRD), "expected CRD %q to be installed", expectedCRD)
}

func TestIntegration_ValidCRAccepted(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTest(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-valid-cr-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	k := validIntegrationKeystone("valid-cr", ns.Name)
	g.Expect(c.Create(ctx, k)).To(Succeed(), "valid Keystone CR should be accepted")

	// Verify it can be retrieved.
	got := &Keystone{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "valid-cr", Namespace: ns.Name}, got)).To(Succeed())
	g.Expect(got.Spec.Replicas).To(Equal(int32(3)))
	g.Expect(got.Spec.Database.Host).To(Equal("db.example.com"))
}

func TestIntegration_ValidCRWithClusterRefAccepted(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTest(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-clusterref-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	k := validIntegrationKeystone("clusterref-cr", ns.Name)
	// Switch database to ClusterRef mode.
	k.Spec.Database.ClusterRef = &corev1.LocalObjectReference{Name: "mariadb"}
	k.Spec.Database.Host = ""
	// Switch cache to ClusterRef mode.
	k.Spec.Cache.ClusterRef = &corev1.LocalObjectReference{Name: "memcached"}
	k.Spec.Cache.Servers = nil

	g.Expect(c.Create(ctx, k)).To(Succeed(), "Keystone CR with ClusterRef mode should be accepted")

	got := &Keystone{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "clusterref-cr", Namespace: ns.Name}, got)).To(Succeed())
	g.Expect(got.Spec.Database.ClusterRef.Name).To(Equal("mariadb"))
	g.Expect(got.Spec.Cache.ClusterRef.Name).To(Equal("memcached"))
}

// --- Task 2.2: CEL validation rejection tests ---

func TestIntegration_CELRejectsDBBothClusterRefAndHost(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTest(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-cel-db-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	k := validIntegrationKeystone("db-both", ns.Name)
	k.Spec.Database.ClusterRef = &corev1.LocalObjectReference{Name: "mariadb"}
	k.Spec.Database.Host = "db.example.com"

	err := c.Create(ctx, k)
	g.Expect(err).To(HaveOccurred(), "setting both database.clusterRef and database.host should be rejected")
	g.Expect(apierrors.IsInvalid(err) || apierrors.IsForbidden(err)).To(BeTrue(),
		fmt.Sprintf("expected Invalid or Forbidden status error, got: %v", err))
	g.Expect(err.Error()).To(ContainSubstring("database"))
}

func TestIntegration_CELRejectsCacheBothClusterRefAndServers(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTest(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-cel-cache-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	k := validIntegrationKeystone("cache-both", ns.Name)
	k.Spec.Cache.ClusterRef = &corev1.LocalObjectReference{Name: "memcached"}
	k.Spec.Cache.Servers = []string{"mc:11211"}

	err := c.Create(ctx, k)
	g.Expect(err).To(HaveOccurred(), "setting both cache.clusterRef and cache.servers should be rejected")
	g.Expect(apierrors.IsInvalid(err) || apierrors.IsForbidden(err)).To(BeTrue(),
		fmt.Sprintf("expected Invalid or Forbidden status error, got: %v", err))
	g.Expect(err.Error()).To(ContainSubstring("cache"))
}

func TestIntegration_CELRejectsReplicasBelowMinimum(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTest(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-cel-replicas-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	// Use -1 because the defaulting webhook converts 0 to 3. The
	// kubebuilder:validation:Minimum=1 CRD schema rule and the validating
	// webhook both reject negative values.
	k := validIntegrationKeystone("replicas-neg", ns.Name)
	k.Spec.Replicas = -1

	err := c.Create(ctx, k)
	g.Expect(err).To(HaveOccurred(), "negative replicas should be rejected")
	g.Expect(apierrors.IsInvalid(err) || apierrors.IsForbidden(err)).To(BeTrue(),
		fmt.Sprintf("expected Invalid or Forbidden status error, got: %v", err))
}

func TestIntegration_CELRejectsMaxActiveKeysBelowMinimum(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTest(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-cel-mak-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	// Use 1 because the defaulting webhook only converts 0 to 3.
	// The kubebuilder:validation:Minimum=3 CRD schema rule and the
	// validating webhook both reject values below 3 (except 0 which
	// is treated as "use default").
	k := validIntegrationKeystone("mak-below-min", ns.Name)
	k.Spec.Fernet.MaxActiveKeys = 1

	err := c.Create(ctx, k)
	g.Expect(err).To(HaveOccurred(), "maxActiveKeys=1 should be rejected (minimum is 3)")
	g.Expect(apierrors.IsInvalid(err) || apierrors.IsForbidden(err)).To(BeTrue(),
		fmt.Sprintf("expected Invalid or Forbidden status error, got: %v", err))
}

func TestIntegration_CELRejectsPolicyOverridesEmpty(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTest(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-cel-policy-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	k := validIntegrationKeystone("policy-empty", ns.Name)
	k.Spec.PolicyOverrides = &commonv1.PolicySpec{}

	err := c.Create(ctx, k)
	g.Expect(err).To(HaveOccurred(), "empty policyOverrides should be rejected")
	g.Expect(apierrors.IsInvalid(err) || apierrors.IsForbidden(err)).To(BeTrue(),
		fmt.Sprintf("expected Invalid or Forbidden status error, got: %v", err))
	g.Expect(err.Error()).To(ContainSubstring("policyOverrides"))
}

// TestIntegration_CELRejectsPolicyRuleEmptyValue pins the empty-rule-value
// constraint added by issue #479: a policyOverrides rule whose value is the
// empty string previously passed admission and reached oslo.policy. The
// XValidation rule on commonv1.PolicySpec now rejects it; this is the
// webhook-enabled flavour (the CRD-only variant below proves the CEL layer
// alone is sufficient).
func TestIntegration_CELRejectsPolicyRuleEmptyValue(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTest(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-cel-policyval-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	k := validIntegrationKeystone("policy-empty-value", ns.Name)
	k.Spec.PolicyOverrides = &commonv1.PolicySpec{Rules: map[string]string{"identity:get_user": ""}}

	err := c.Create(ctx, k)
	g.Expect(err).To(HaveOccurred(), "empty policy rule value should be rejected")
	g.Expect(apierrors.IsInvalid(err) || apierrors.IsForbidden(err)).To(BeTrue(),
		fmt.Sprintf("expected Invalid or Forbidden status error, got: %v", err))
	g.Expect(err.Error()).To(ContainSubstring("policyOverrides"))
}

// TestIntegration_CRD_CELOnly_RejectsPolicyRuleEmptyValue proves the empty
// rule-value rejection holds when the validating webhook is NOT installed —
// the CEL XValidation marker on commonv1.PolicySpec is the authoritative gate
// closing the audit's empty-value gap (issue #479).
func TestIntegration_CRD_CELOnly_RejectsPolicyRuleEmptyValue(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-celonly-policyval-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	k := validIntegrationKeystone("policy-empty-value-celonly", ns.Name)
	k.Spec.PolicyOverrides = &commonv1.PolicySpec{Rules: map[string]string{"identity:get_user": ""}}

	err := c.Create(ctx, k)
	g.Expect(err).To(HaveOccurred(),
		"CRD CEL rule alone must reject an empty policy rule value (#479)")
	g.Expect(apierrors.IsInvalid(err) || apierrors.IsForbidden(err)).To(BeTrue(),
		fmt.Sprintf("expected Invalid or Forbidden status error, got: %v", err))
	g.Expect(err.Error()).To(ContainSubstring("policy rule value must not be empty"))
}

// TestIntegration_CRD_CELRejectsInvalidTLS verifies the CEL rule on
// spec.database: when database.tls.enabled is true, both caBundleSecretRef.name
// and clientCertSecretRef.name must be non-empty. The base CR uses brownfield
// Host mode (no clusterRef), so the clusterRef/host XOR rule stays satisfied
// and only the new TLS contract rule fires. Acceptance via IsInvalid or
// IsForbidden covers both the CRD x-kubernetes-validations path and the
// defense-in-depth validating webhook.
//
// This is the webhook-enabled flavour; the CRD-only variant
// (TestIntegration_CRD_CELOnly_*) below pins that the CEL rule on its own
// rejects the same CR even when the validating webhook is not installed.
func TestIntegration_CRD_CELRejectsInvalidTLS(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTest(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-cel-dbtls-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	k := validIntegrationKeystone("db-tls-incomplete", ns.Name)
	// tls.enabled=true but both Secret references are left empty, which the
	// CEL rule rejects.
	k.Spec.Database.TLS = &commonv1.DatabaseTLSSpec{
		Mode: "verify-full",
	}

	err := c.Create(ctx, k)
	g.Expect(err).To(HaveOccurred(),
		"database.tls.enabled=true without caBundleSecretRef/clientCertSecretRef should be rejected")
	g.Expect(apierrors.IsInvalid(err) || apierrors.IsForbidden(err)).To(BeTrue(),
		fmt.Sprintf("expected Invalid or Forbidden status error, got: %v", err))
	g.Expect(err.Error()).To(ContainSubstring("database"))
}

// setupEnvTestNoWebhook wraps testutil.SetupKeystoneEnvTestNoWebhook with the
// v1alpha1 scheme registration, avoiding the import cycle between testutil and
// this package. Used by the CRD-only CEL tests below so the validating webhook
// cannot mask a missing CEL rule (review #1).
func setupEnvTestNoWebhook(t testing.TB) (client.Client, context.Context, context.CancelFunc) {
	t.Helper()
	return testutil.SetupKeystoneEnvTestNoWebhook(t, AddToScheme)
}

// TestIntegration_CRD_CELOnly_RejectsInvalidTLS pins the CEL rule on
// spec.database (caBundle + clientCert refs required when tls.enabled=true)
// against an envtest API server with NO validating webhook installed. If the
// CEL rule is ever removed from the CRD schema, this test fails immediately —
// the webhook-enabled flavour above would silently keep passing because the
// webhook's defense-in-depth check would still reject the CR.
func TestIntegration_CRD_CELOnly_RejectsInvalidTLS(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-celonly-dbtls-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	k := validIntegrationKeystone("db-tls-incomplete-celonly", ns.Name)
	k.Spec.Database.TLS = &commonv1.DatabaseTLSSpec{
		Mode: "verify-full",
	}

	err := c.Create(ctx, k)
	g.Expect(err).To(HaveOccurred(),
		"CRD CEL rule alone must reject database.tls.enabled=true without secret refs")
	g.Expect(apierrors.IsInvalid(err) || apierrors.IsForbidden(err)).To(BeTrue(),
		fmt.Sprintf("expected Invalid or Forbidden status error, got: %v", err))
	g.Expect(err.Error()).To(ContainSubstring("database"))
}

// TestIntegration_CRD_CELOnly_RejectsOutOfEnumTLSMode pins the
// +kubebuilder:validation:Enum constraint on DatabaseTLSSpec.Mode against an
// envtest API server with NO validating webhook installed. A mode value
// outside {prefer, require, verify-ca, verify-full} must be rejected by the
// CRD schema alone. This covers the second half of the
// contract that the original TestIntegration_CRD_CELRejectsInvalidTLS
// did not exercise.
func TestIntegration_CRD_CELOnly_RejectsOutOfEnumTLSMode(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-celonly-dbtls-mode-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	k := validIntegrationKeystone("db-tls-bad-mode-celonly", ns.Name)
	// mode is set to a value outside the documented enum. The CRD
	// +kubebuilder:validation:Enum=disabled;prefer;require;verify-ca;verify-full
	// must reject it without any webhook in the picture.
	k.Spec.Database.TLS = &commonv1.DatabaseTLSSpec{
		Mode:                "encrypt-all-the-things",
		CABundleSecretRef:   commonv1.SecretRefSpec{Name: "db-ca"},
		ClientCertSecretRef: commonv1.SecretRefSpec{Name: "db-client"},
	}

	err := c.Create(ctx, k)
	g.Expect(err).To(HaveOccurred(),
		"CRD enum on database.tls.mode must reject out-of-enum values")
	g.Expect(apierrors.IsInvalid(err) || apierrors.IsForbidden(err)).To(BeTrue(),
		fmt.Sprintf("expected Invalid or Forbidden status error, got: %v", err))
	g.Expect(err.Error()).To(SatisfyAny(
		ContainSubstring("database"),
		ContainSubstring("mode"),
	))
}

// --- Field immutability (CEL transition rules, #466) ---

// updateImmutableFieldRejected is the shared body for the field-immutability
// rejection tests. It creates a valid Keystone CR, re-reads it, applies the
// given mutation, and asserts the UPDATE is rejected with every expected
// message substring. The setup function selects whether the validating webhook
// is installed: the webhook deliberately discards oldObj and never checks
// immutability (keystone_webhook.go), so the CRD CEL transition rule is the sole
// gate. The no-webhook setup pins that the CEL rule rejects the change on its
// own; a webhook-enabled variant confirms the rule still fires when the webhook
// is present.
func updateImmutableFieldRejected(
	t *testing.T,
	setup func(testing.TB) (client.Client, context.Context, context.CancelFunc),
	nsPrefix string,
	mutate func(*Keystone),
	wantSubstrings ...string,
) {
	t.Helper()
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setup(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: nsPrefix}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	k := validIntegrationKeystone("immutable", ns.Name)
	g.Expect(c.Create(ctx, k)).To(Succeed(), "valid Keystone CR should be accepted on create")

	got := &Keystone{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "immutable", Namespace: ns.Name}, got)).To(Succeed())
	mutate(got)

	err := c.Update(ctx, got)
	g.Expect(err).To(HaveOccurred(), "mutating an immutable field must be rejected on update")
	g.Expect(apierrors.IsInvalid(err) || apierrors.IsForbidden(err)).To(BeTrue(),
		fmt.Sprintf("expected Invalid or Forbidden status error, got: %v", err))
	for _, want := range wantSubstrings {
		g.Expect(err.Error()).To(ContainSubstring(want))
	}
}

// TestIntegration_CRD_CELOnly_RejectsDatabaseNameChange pins the
// spec.database.database immutability transition rule against an API server with
// NO validating webhook installed: renaming the database is rejected by the CEL
// rule alone.
func TestIntegration_CRD_CELOnly_RejectsDatabaseNameChange(t *testing.T) {
	updateImmutableFieldRejected(t, setupEnvTestNoWebhook, "test-celonly-dbname-",
		func(k *Keystone) { k.Spec.Database.Database = "renamed" },
		"database", "immutable")
}

// TestIntegration_CRD_CELOnly_RejectsDatabaseModeFlip pins the database
// managed-vs-brownfield mode immutability rule: flipping the host-mode base CR
// to clusterRef mode is rejected by the CEL rule alone. The new object still
// satisfies the clusterRef/host XOR rule, so only the mode transition rule fires.
func TestIntegration_CRD_CELOnly_RejectsDatabaseModeFlip(t *testing.T) {
	updateImmutableFieldRejected(t, setupEnvTestNoWebhook, "test-celonly-dbmode-",
		func(k *Keystone) {
			k.Spec.Database.ClusterRef = &corev1.LocalObjectReference{Name: "mariadb"}
			k.Spec.Database.Host = ""
		},
		"database mode", "immutable")
}

// TestIntegration_CRD_CELOnly_RejectsAdminUserChange pins the
// spec.bootstrap.adminUser immutability rule against an API server with NO
// validating webhook installed.
func TestIntegration_CRD_CELOnly_RejectsAdminUserChange(t *testing.T) {
	updateImmutableFieldRejected(t, setupEnvTestNoWebhook, "test-celonly-adminuser-",
		func(k *Keystone) { k.Spec.Bootstrap.AdminUser = "root" },
		"adminUser", "immutable")
}

// TestIntegration_CRD_CELOnly_RejectsRegionChange pins the spec.bootstrap.region
// immutability rule against an API server with NO validating webhook installed.
func TestIntegration_CRD_CELOnly_RejectsRegionChange(t *testing.T) {
	updateImmutableFieldRejected(t, setupEnvTestNoWebhook, "test-celonly-region-",
		func(k *Keystone) { k.Spec.Bootstrap.Region = "RegionTwo" },
		"region", "immutable")
}

// TestIntegration_CELRejectsDatabaseNameChange is the webhook-enabled flavour of
// the database-name immutability test: even with the validating webhook
// installed (which never checks immutability itself), the CRD CEL transition
// rule still rejects the rename.
func TestIntegration_CELRejectsDatabaseNameChange(t *testing.T) {
	updateImmutableFieldRejected(t, setupEnvTest, "test-cel-dbname-",
		func(k *Keystone) { k.Spec.Database.Database = "renamed" },
		"database", "immutable")
}

// --- Task 2.3: Webhook defaulting tests ---

func TestIntegration_WebhookDefaultsSetsZeroValues(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTest(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-defaults-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	// Create a CR with zero values for fields that the webhook should default.
	// Required fields (database, cache, image, fernet.rotationSchedule,
	// bootstrap.adminPasswordSecretRef) must still be valid.
	k := &Keystone{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "defaults-zero",
			Namespace: ns.Name,
		},
		Spec: KeystoneSpec{
			Replicas: 0, // webhook defaults to 3
			Image:    commonv1.ImageSpec{Repository: "ghcr.io/c5c3/keystone", Tag: "2025.2"},
			Database: commonv1.DatabaseSpec{
				Host:      "db.example.com",
				Port:      3306,
				Database:  "keystone",
				SecretRef: commonv1.SecretRefSpec{Name: "keystone-db"},
			},
			Cache: commonv1.CacheSpec{
				Backend: "", // webhook defaults to "dogpile.cache.pymemcache"
				Servers: []string{"mc:11211"},
			},
			Fernet: FernetSpec{
				RotationSchedule: "0 0 * * 0",
				MaxActiveKeys:    0, // webhook defaults to 3
			},
			Bootstrap: BootstrapSpec{
				AdminUser:              "", // webhook defaults to "admin"
				AdminPasswordSecretRef: commonv1.SecretRefSpec{Name: "keystone-admin"},
				Region:                 "", // webhook defaults to "RegionOne"
			},
		},
	}

	g.Expect(c.Create(ctx, k)).To(Succeed(), "CR with zero values should be accepted after webhook defaults")

	got := &Keystone{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "defaults-zero", Namespace: ns.Name}, got)).To(Succeed())

	// Verify that the webhook applied defaults for zero-valued fields.
	g.Expect(got.Spec.Replicas).To(Equal(int32(3)), "replicas should be defaulted to 3")
	g.Expect(got.Spec.Fernet.MaxActiveKeys).To(Equal(int32(3)), "maxActiveKeys should be defaulted to 3")
	g.Expect(got.Spec.Cache.Backend).To(Equal("dogpile.cache.pymemcache"), "cache.backend should be defaulted")
	g.Expect(got.Spec.Bootstrap.AdminUser).To(Equal("admin"), "bootstrap.adminUser should be defaulted")
	g.Expect(got.Spec.Bootstrap.Region).To(Equal("RegionOne"), "bootstrap.region should be defaulted")
}

func TestIntegration_WebhookDefaultsPreservesExplicit(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTest(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-preserve-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	// Create a CR with explicit (non-zero) values for all defaultable fields.
	k := &Keystone{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "defaults-explicit",
			Namespace: ns.Name,
		},
		Spec: KeystoneSpec{
			Replicas: 5,
			Image:    commonv1.ImageSpec{Repository: "ghcr.io/c5c3/keystone", Tag: "2025.2"},
			Database: commonv1.DatabaseSpec{
				Host:      "db.example.com",
				Port:      3306,
				Database:  "keystone",
				SecretRef: commonv1.SecretRefSpec{Name: "keystone-db"},
			},
			Cache: commonv1.CacheSpec{
				Backend: "dogpile.cache.memcache",
				Servers: []string{"mc:11211"},
			},
			Fernet: FernetSpec{
				RotationSchedule: "0 */6 * * *",
				MaxActiveKeys:    7,
			},
			Bootstrap: BootstrapSpec{
				AdminUser:              "custom-admin",
				AdminPasswordSecretRef: commonv1.SecretRefSpec{Name: "keystone-admin"},
				Region:                 "EU-West",
			},
		},
	}

	g.Expect(c.Create(ctx, k)).To(Succeed(), "CR with explicit values should be accepted")

	got := &Keystone{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "defaults-explicit", Namespace: ns.Name}, got)).To(Succeed())

	// Verify that the webhook preserved all explicitly set values.
	g.Expect(got.Spec.Replicas).To(Equal(int32(5)), "explicit replicas should be preserved")
	g.Expect(got.Spec.Fernet.MaxActiveKeys).To(Equal(int32(7)), "explicit maxActiveKeys should be preserved")
	g.Expect(got.Spec.Cache.Backend).To(Equal("dogpile.cache.memcache"), "explicit cache.backend should be preserved")
	g.Expect(got.Spec.Bootstrap.AdminUser).To(Equal("custom-admin"), "explicit bootstrap.adminUser should be preserved")
	g.Expect(got.Spec.Bootstrap.Region).To(Equal("EU-West"), "explicit bootstrap.region should be preserved")
}

// --- Task: Resources defaulting and validation integration tests ---

func TestIntegration_ResourcesDefaultedWhenNil(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTest(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-res-default-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	k := validIntegrationKeystone("res-default", ns.Name)
	k.Spec.Resources = nil

	g.Expect(c.Create(ctx, k)).To(Succeed(), "CR without spec.resources should be accepted")

	got := &Keystone{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "res-default", Namespace: ns.Name}, got)).To(Succeed())

	g.Expect(got.Spec.Resources).NotTo(BeNil(), "resources should be defaulted")
	g.Expect(got.Spec.Resources.Requests).To(HaveKeyWithValue(corev1.ResourceMemory, DefaultMemoryRequest))
	g.Expect(got.Spec.Resources.Requests).To(HaveKeyWithValue(corev1.ResourceCPU, DefaultCPURequest))
	g.Expect(got.Spec.Resources.Limits).To(HaveKeyWithValue(corev1.ResourceMemory, DefaultMemoryLimit))
	g.Expect(got.Spec.Resources.Limits).To(HaveKeyWithValue(corev1.ResourceCPU, DefaultCPULimit))
}

func TestIntegration_ResourcesPreservedWhenExplicit(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTest(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-res-explicit-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	k := validIntegrationKeystone("res-explicit", ns.Name)
	k.Spec.Resources = &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("1Gi"),
			corev1.ResourceCPU:    resource.MustParse("200m"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("2Gi"),
			corev1.ResourceCPU:    resource.MustParse("1"),
		},
	}

	g.Expect(c.Create(ctx, k)).To(Succeed(), "CR with explicit resources should be accepted")

	got := &Keystone{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "res-explicit", Namespace: ns.Name}, got)).To(Succeed())

	g.Expect(got.Spec.Resources.Requests).To(HaveKeyWithValue(corev1.ResourceMemory, resource.MustParse("1Gi")))
	g.Expect(got.Spec.Resources.Requests).To(HaveKeyWithValue(corev1.ResourceCPU, resource.MustParse("200m")))
	g.Expect(got.Spec.Resources.Limits).To(HaveKeyWithValue(corev1.ResourceMemory, resource.MustParse("2Gi")))
	g.Expect(got.Spec.Resources.Limits).To(HaveKeyWithValue(corev1.ResourceCPU, resource.MustParse("1")))
}

func TestIntegration_ResourcesRequestExceedsLimitRejected(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTest(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-res-invalid-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	k := validIntegrationKeystone("res-invalid", ns.Name)
	k.Spec.Resources = &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("1000m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
	}

	err := c.Create(ctx, k)
	g.Expect(err).To(HaveOccurred(), "CPU request > limit should be rejected")
	g.Expect(apierrors.IsInvalid(err) || apierrors.IsForbidden(err)).To(BeTrue(),
		fmt.Sprintf("expected Invalid or Forbidden status error, got: %v", err))
	g.Expect(err.Error()).To(ContainSubstring("resources"))
}

// --- Task: UWSGI defaulting and validation integration tests ---

func TestIntegration_UWSGINilPreserved(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTest(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-uwsgi-nil-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	k := validIntegrationKeystone("uwsgi-nil", ns.Name)
	k.Spec.UWSGI = nil

	g.Expect(c.Create(ctx, k)).To(Succeed(), "CR without spec.uwsgi should be accepted")

	got := &Keystone{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "uwsgi-nil", Namespace: ns.Name}, got)).To(Succeed())

	g.Expect(got.Spec.UWSGI).To(BeNil(), "spec.uwsgi should remain nil when not set")
}

func TestIntegration_UWSGIDefaultsAppliedWhenEmpty(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTest(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-uwsgi-empty-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	k := validIntegrationKeystone("uwsgi-empty", ns.Name)
	k.Spec.UWSGI = &UWSGISpec{}

	g.Expect(c.Create(ctx, k)).To(Succeed(), "CR with empty spec.uwsgi should be accepted after defaults")

	got := &Keystone{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "uwsgi-empty", Namespace: ns.Name}, got)).To(Succeed())

	g.Expect(got.Spec.UWSGI).NotTo(BeNil(), "spec.uwsgi should not be nil")
	g.Expect(got.Spec.UWSGI.Processes).To(Equal(int32(2)), "processes should be defaulted to 2")
	g.Expect(got.Spec.UWSGI.Threads).To(Equal(int32(1)), "threads should be defaulted to 1")
	g.Expect(got.Spec.UWSGI.HTTPKeepAlive).To(HaveValue(BeTrue()), "httpKeepAlive should be defaulted to true")
}

func TestIntegration_UWSGIExplicitValuesPreserved(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTest(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-uwsgi-explicit-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	k := validIntegrationKeystone("uwsgi-explicit", ns.Name)
	k.Spec.UWSGI = &UWSGISpec{
		Processes:     8,
		Threads:       4,
		HTTPKeepAlive: ptr.To(true),
	}

	g.Expect(c.Create(ctx, k)).To(Succeed(), "CR with explicit uwsgi values should be accepted")

	got := &Keystone{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "uwsgi-explicit", Namespace: ns.Name}, got)).To(Succeed())

	g.Expect(got.Spec.UWSGI).NotTo(BeNil(), "spec.uwsgi should not be nil")
	g.Expect(got.Spec.UWSGI.Processes).To(Equal(int32(8)), "explicit processes should be preserved")
	g.Expect(got.Spec.UWSGI.Threads).To(Equal(int32(4)), "explicit threads should be preserved")
	g.Expect(got.Spec.UWSGI.HTTPKeepAlive).To(HaveValue(BeTrue()), "explicit httpKeepAlive should be preserved")
}

func TestIntegration_UWSGIPartialDefaulting(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTest(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-uwsgi-partial-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	// Only set processes; threads and httpKeepAlive left unset.
	// The webhook defaults threads to 1 and, because httpKeepAlive is now a
	// nil-preserving *bool, restores its documented default (true) when the
	// pointer is nil — regardless of the other sub-fields.
	k := validIntegrationKeystone("uwsgi-partial", ns.Name)
	k.Spec.UWSGI = &UWSGISpec{
		Processes: 4,
	}

	g.Expect(c.Create(ctx, k)).To(Succeed(), "CR with partial uwsgi values should be accepted after defaults")

	got := &Keystone{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "uwsgi-partial", Namespace: ns.Name}, got)).To(Succeed())

	g.Expect(got.Spec.UWSGI).NotTo(BeNil(), "spec.uwsgi should not be nil")
	g.Expect(got.Spec.UWSGI.Processes).To(Equal(int32(4)), "explicit processes should be preserved")
	g.Expect(got.Spec.UWSGI.Threads).To(Equal(int32(1)), "threads should be defaulted to 1")
	// httpKeepAlive is true because the defaulting webhook restores the nil
	// pointer to the documented default (true).
	g.Expect(got.Spec.UWSGI.HTTPKeepAlive).To(HaveValue(BeTrue()), "httpKeepAlive should be defaulted to true by the webhook")
}

func TestIntegration_UWSGIProcessesBelowMinimumRejected(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTest(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-uwsgi-proc-min-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	// Use -1 to bypass the defaulting webhook (which only defaults 0).
	// Both the CRD schema (+kubebuilder:validation:Minimum=1) and the
	// validating webhook reject negative values.
	k := validIntegrationKeystone("uwsgi-proc-neg", ns.Name)
	k.Spec.UWSGI = &UWSGISpec{
		Processes:     -1,
		Threads:       2,
		HTTPKeepAlive: ptr.To(true),
	}

	err := c.Create(ctx, k)
	g.Expect(err).To(HaveOccurred(), "uwsgi.processes=-1 should be rejected")
	g.Expect(apierrors.IsInvalid(err) || apierrors.IsForbidden(err)).To(BeTrue(),
		fmt.Sprintf("expected Invalid or Forbidden status error, got: %v", err))
	g.Expect(err.Error()).To(SatisfyAny(
		ContainSubstring("uwsgi"),
		ContainSubstring("processes"),
	))
}

func TestIntegration_UWSGIThreadsBelowMinimumRejected(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTest(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-uwsgi-thr-min-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	// Use -1 to bypass the defaulting webhook (which only defaults 0).
	// Both the CRD schema (+kubebuilder:validation:Minimum=1) and the
	// validating webhook reject negative values.
	k := validIntegrationKeystone("uwsgi-thr-neg", ns.Name)
	k.Spec.UWSGI = &UWSGISpec{
		Processes:     2,
		Threads:       -1,
		HTTPKeepAlive: ptr.To(true),
	}

	err := c.Create(ctx, k)
	g.Expect(err).To(HaveOccurred(), "uwsgi.threads=-1 should be rejected")
	g.Expect(apierrors.IsInvalid(err) || apierrors.IsForbidden(err)).To(BeTrue(),
		fmt.Sprintf("expected Invalid or Forbidden status error, got: %v", err))
	g.Expect(err.Error()).To(SatisfyAny(
		ContainSubstring("uwsgi"),
		ContainSubstring("threads"),
	))
}

// CRD schema must surface spec.logging with Format/Level enum constraints, Debug boolean, and PerLoggerLevels map<string,string>.
func TestIntegration_CRDSchemaContainsLoggingSpec(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTest(t)

	// enumStrings decodes a slice of apiextensionsv1.JSON enum entries (each
	// holding a JSON-encoded literal) into a []string for set-equality
	// assertions via Gomega's ConsistOf.
	enumStrings := func(in []apiextensionsv1.JSON) []string {
		out := make([]string, 0, len(in))
		for _, j := range in {
			var s string
			g.Expect(json.Unmarshal(j.Raw, &s)).To(Succeed(),
				fmt.Sprintf("enum value %q should decode as a JSON string", string(j.Raw)))
			out = append(out, s)
		}
		return out
	}

	crd := &apiextensionsv1.CustomResourceDefinition{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "keystones.keystone.openstack.c5c3.io"}, crd)).
		To(Succeed(), "keystones CRD should be installed by envtest")

	// Locate the served+stored version. v1alpha1 is the only version today,
	// but search by flags so the test is robust to future version additions.
	var servedSchema *apiextensionsv1.JSONSchemaProps
	for i := range crd.Spec.Versions {
		v := &crd.Spec.Versions[i]
		if v.Served && v.Storage {
			g.Expect(v.Schema).NotTo(BeNil(), "served version %q must declare a schema", v.Name)
			g.Expect(v.Schema.OpenAPIV3Schema).NotTo(BeNil(),
				"served version %q must declare openAPIV3Schema", v.Name)
			servedSchema = v.Schema.OpenAPIV3Schema
			break
		}
	}
	g.Expect(servedSchema).NotTo(BeNil(), "CRD must expose a served+stored version")

	specSchema, ok := servedSchema.Properties["spec"]
	g.Expect(ok).To(BeTrue(), "openAPIV3Schema must declare a spec property")

	logging, ok := specSchema.Properties["logging"]
	g.Expect(ok).To(BeTrue(), "spec.properties.logging must exist")

	format, ok := logging.Properties["format"]
	g.Expect(ok).To(BeTrue(), "spec.logging.format must exist")
	g.Expect(format.Type).To(Equal("string"), "spec.logging.format must be string-typed")
	g.Expect(enumStrings(format.Enum)).To(ConsistOf("text", "json"),
		"spec.logging.format enum must be exactly {text, json}")
	g.Expect(format.Default).NotTo(BeNil(), "spec.logging.format must declare a default")
	g.Expect(string(format.Default.Raw)).To(Equal(`"text"`),
		"spec.logging.format default must be \"text\"")

	level, ok := logging.Properties["level"]
	g.Expect(ok).To(BeTrue(), "spec.logging.level must exist")
	g.Expect(level.Type).To(Equal("string"), "spec.logging.level must be string-typed")
	g.Expect(enumStrings(level.Enum)).
		To(ConsistOf("DEBUG", "INFO", "WARNING", "ERROR", "CRITICAL"),
			"spec.logging.level enum must cover all oslo.log levels")
	g.Expect(level.Default).NotTo(BeNil(), "spec.logging.level must declare a default")
	g.Expect(string(level.Default.Raw)).To(Equal(`"INFO"`),
		"spec.logging.level default must be \"INFO\"")

	debug, ok := logging.Properties["debug"]
	g.Expect(ok).To(BeTrue(), "spec.logging.debug must exist")
	g.Expect(debug.Type).To(Equal("boolean"), "spec.logging.debug must be boolean-typed")
	g.Expect(debug.Default).NotTo(BeNil(), "spec.logging.debug must declare a default")
	g.Expect(string(debug.Default.Raw)).To(Equal(`false`),
		"spec.logging.debug default must be false")

	perLogger, ok := logging.Properties["perLoggerLevels"]
	g.Expect(ok).To(BeTrue(), "spec.logging.perLoggerLevels must exist")
	g.Expect(perLogger.Type).To(Equal("object"), "spec.logging.perLoggerLevels must be object-typed")
	g.Expect(perLogger.AdditionalProperties).NotTo(BeNil(),
		"spec.logging.perLoggerLevels must declare additionalProperties (map<string,string>)")
	g.Expect(perLogger.AdditionalProperties.Schema).NotTo(BeNil(),
		"spec.logging.perLoggerLevels.additionalProperties must carry a schema")
	g.Expect(perLogger.AdditionalProperties.Schema.Type).To(Equal("string"),
		"spec.logging.perLoggerLevels values must be string-typed")
}

// --- Validation-marker wave (issue #469): CRD-only rejection coverage ---

// TestIntegration_CRD_CELOnly_RejectsValidationMarkers pins the validation-marker
// wave (image refs, DB endpoints, secret refs, middleware enum, CEL parity)
// against an envtest API server with NO validating webhook installed, so a
// dropped marker or CEL rule fails here immediately — the webhook's
// defense-in-depth checks cannot mask it. Each case mutates exactly one field of
// an otherwise-valid CR, so the rejection is attributable to that field.
//
// The uwsgi httpKeepAlive/timeout cross-field rule is intentionally NOT covered
// here: a typed Go client drops httpKeepAlive: false via omitempty before the
// CRD default re-adds it, so the rule cannot fire through this client. The
// raw-YAML invalid-cr Chainsaw fixture (17-uwsgi-keepalive-timeout-conflict.yaml)
// is its primary coverage.
func TestIntegration_CRD_CELOnly_RejectsValidationMarkers(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)

	cases := []struct {
		name   string
		mutate func(*Keystone)
	}{
		{"empty image repository", func(k *Keystone) { k.Spec.Image.Repository = "" }},
		{"empty image tag", func(k *Keystone) { k.Spec.Image.Tag = "" }},
		{"database port above max", func(k *Keystone) { k.Spec.Database.Port = 70000 }},
		{"database port negative", func(k *Keystone) { k.Spec.Database.Port = -1 }},
		{"database name empty", func(k *Keystone) { k.Spec.Database.Database = "" }},
		{"database name invalid char", func(k *Keystone) { k.Spec.Database.Database = "keystone-prod" }},
		{"database name too long", func(k *Keystone) { k.Spec.Database.Database = strings.Repeat("a", 65) }},
		{"empty database secretRef name", func(k *Keystone) { k.Spec.Database.SecretRef.Name = "" }},
		{"empty admin password secretRef name", func(k *Keystone) {
			k.Spec.Bootstrap.AdminPasswordSecretRef.Name = ""
		}},
		{"middleware bad position", func(k *Keystone) {
			k.Spec.Middleware = []commonv1.MiddlewareSpec{{
				Name:          "audit",
				FilterFactory: "audit:filter_factory",
				Position:      "sideways",
			}}
		}},
		{"duplicate plugin config section", func(k *Keystone) {
			k.Spec.Plugins = []commonv1.PluginSpec{
				{Name: "a", ConfigSection: "dup"},
				{Name: "b", ConfigSection: "dup"},
			}
		}},
		{"autoscaling min greater than max", func(k *Keystone) {
			k.Spec.Autoscaling = &AutoscalingSpec{
				MinReplicas:          ptr.To(int32(10)),
				MaxReplicas:          5,
				TargetCPUUtilization: ptr.To(int32(80)),
			}
		}},
		{"prestop not less than grace period", func(k *Keystone) {
			k.Spec.PreStopSleepSeconds = ptr.To(int64(20))
			k.Spec.TerminationGracePeriodSeconds = ptr.To(int64(15))
		}},
		{"perLoggerLevels invalid value", func(k *Keystone) {
			k.Spec.Logging = &LoggingSpec{
				Format:          "text",
				Level:           "INFO",
				PerLoggerLevels: map[string]string{"sqlalchemy.engine": "BOGUS"},
			}
		}},
		{"perLoggerLevels empty key", func(k *Keystone) {
			k.Spec.Logging = &LoggingSpec{
				Format:          "text",
				Level:           "INFO",
				PerLoggerLevels: map[string]string{"": "INFO"},
			}
		}},
		{"non-URL bootstrap publicEndpoint", func(k *Keystone) {
			k.Spec.Bootstrap.PublicEndpoint = "keystone.example.com"
		}},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-celonly-marker-"}}
			g.Expect(c.Create(ctx, ns)).To(Succeed())

			k := validIntegrationKeystone(fmt.Sprintf("marker-reject-%d", i), ns.Name)
			tc.mutate(k)

			err := c.Create(ctx, k)
			g.Expect(err).To(HaveOccurred(), "CRD schema alone must reject: %s", tc.name)
			g.Expect(apierrors.IsInvalid(err) || apierrors.IsForbidden(err)).To(BeTrue(),
				fmt.Sprintf("expected Invalid or Forbidden status error for %q, got: %v", tc.name, err))
		})
	}
}

// TestIntegration_CRD_CELOnly_RejectsKeepAliveTimeoutWithoutKeepAlive pins the
// UWSGISpec cross-field CEL rule against an envtest API server with NO validating
// webhook installed. The CR is built as an *unstructured.Unstructured so
// httpKeepAlive: false survives to the API server — a typed Go client drops it
// via omitempty and the CRD default then re-adds true, which would mask the rule.
func TestIntegration_CRD_CELOnly_RejectsKeepAliveTimeoutWithoutKeepAlive(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-celonly-keepalive-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(
		validIntegrationKeystone("keepalive-timeout", ns.Name),
	)
	g.Expect(err).NotTo(HaveOccurred())
	u := &unstructured.Unstructured{Object: raw}
	u.SetGroupVersionKind(GroupVersion.WithKind("Keystone"))
	g.Expect(unstructured.SetNestedField(u.Object, false, "spec", "uwsgi", "httpKeepAlive")).To(Succeed())
	g.Expect(unstructured.SetNestedField(u.Object, int64(30), "spec", "uwsgi", "httpKeepAliveTimeout")).To(Succeed())

	err = c.Create(ctx, u)
	g.Expect(err).To(HaveOccurred(),
		"httpKeepAliveTimeout set while httpKeepAlive is false must be rejected by the CRD CEL rule")
	g.Expect(apierrors.IsInvalid(err) || apierrors.IsForbidden(err)).To(BeTrue(),
		fmt.Sprintf("expected Invalid or Forbidden status error, got: %v", err))
	g.Expect(err.Error()).To(ContainSubstring("httpKeepAliveTimeout"))
}

// TestIntegration_AcceptsValidNonDefaultMarkers verifies the validation-marker
// wave accepts valid NON-DEFAULT values, so a future over-restrictive marker
// edit (e.g. a too-narrow pattern) is caught — the positive-coverage counterpart
// to the rejection table above.
func TestIntegration_AcceptsValidNonDefaultMarkers(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTest(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-valid-markers-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	k := validIntegrationKeystone("valid-markers", ns.Name)
	// Non-default image (different repository + tag with separators).
	k.Spec.Image = commonv1.ImageSpec{Repository: "ghcr.io/c5c3/keystone-operator", Tag: "2025.2-upgraded"}
	// Brownfield DB with an explicit non-default port and an underscore name.
	k.Spec.Database.Host = "db.internal.svc.cluster.local"
	k.Spec.Database.Port = 13306
	k.Spec.Database.Database = "keystone_prod"
	// Middleware with a valid position; plugins with distinct config sections.
	k.Spec.Middleware = []commonv1.MiddlewareSpec{{
		Name:          "audit",
		FilterFactory: "audit:filter_factory",
		Position:      commonv1.PipelinePositionAfter,
	}}
	k.Spec.Plugins = []commonv1.PluginSpec{
		{Name: "ldap", ConfigSection: "ldap"},
		{Name: "federation", ConfigSection: "federation"},
	}
	// Valid HTTP(S) public endpoint (no gateway, so the webhook host check is skipped).
	k.Spec.Bootstrap.PublicEndpoint = "https://keystone.example.com/v3"
	// Valid per-logger levels and a valid keepalive+timeout combination.
	k.Spec.Logging = &LoggingSpec{
		Format:          "json",
		Level:           "WARNING",
		PerLoggerLevels: map[string]string{"sqlalchemy.engine": "WARNING"},
	}
	k.Spec.UWSGI = &UWSGISpec{
		Processes:            4,
		Threads:              2,
		HTTPKeepAlive:        ptr.To(true),
		HTTPKeepAliveTimeout: ptr.To(int32(30)),
	}

	g.Expect(c.Create(ctx, k)).To(Succeed(),
		"valid non-default validation-marker values should be accepted")
}
