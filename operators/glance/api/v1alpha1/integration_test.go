// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package v1alpha1

import (
	"context"
	"fmt"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1 "github.com/c5c3/forge/internal/common/types"
	"github.com/c5c3/forge/operators/glance/internal/testutil"
)

// --- Helpers ---

// setupEnvTest wraps testutil.SetupGlanceEnvTest with the v1alpha1 scheme
// registration and both webhook setups (Glance + GlanceBackend), avoiding the
// import cycle between testutil and this package. The webhook manifests envtest
// installs carry both kinds (failurePolicy=Fail), so both handlers must be
// served or admission of the unserved kind fails.
func setupEnvTest(t testing.TB) (client.Client, context.Context, context.CancelFunc) {
	t.Helper()
	return testutil.SetupGlanceEnvTest(t, AddToScheme, func(mgr ctrl.Manager) error {
		// mgr.GetAPIReader() mirrors production wiring in main.go: webhook
		// admission lookups (PriorityClass existence, the sibling-default List)
		// read the API server directly, never a stale informer cache.
		if err := (&GlanceWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr); err != nil {
			return err
		}
		return (&GlanceBackendWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr)
	})
}

// setupEnvTestNoWebhook wraps testutil.SetupGlanceEnvTestNoWebhook with the
// v1alpha1 scheme registration. Used by the CRD-only tests so no webhook can
// mask a missing CEL rule or supply a default the CRD schema must supply itself.
func setupEnvTestNoWebhook(t testing.TB) (client.Client, context.Context, context.CancelFunc) {
	t.Helper()
	return testutil.SetupGlanceEnvTestNoWebhook(t, AddToScheme)
}

// newNamespace creates a uniquely named namespace for a test.
func newNamespace(t testing.TB, ctx context.Context, c client.Client, prefix string) string {
	t.Helper()
	g := NewGomegaWithT(t)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: prefix}}
	g.Expect(c.Create(ctx, ns)).To(Succeed(), "create namespace")
	return ns.Name
}

// integrationGlance returns validGlance() stamped with name/namespace for API
// server submission. validGlance() is a webhook-unit helper whose validators do
// not enforce the CRD MinLength markers, so it omits spec.database.database and
// spec.database.secretRef.name; those are filled here so the object clears the
// real CRD schema (both are required even in managed/clusterRef mode).
func integrationGlance(name, namespace string) *Glance {
	glance := validGlance()
	glance.Name = name
	glance.Namespace = namespace
	glance.Spec.Database.Database = "glance"
	glance.Spec.Database.SecretRef = commonv1.SecretRefSpec{Name: "glance-db"}
	return glance
}

// integrationBackend returns validGlanceBackend() stamped with name/namespace
// and pointed at glanceRef, for API server submission.
func integrationBackend(name, namespace, glanceRef string) *GlanceBackend {
	b := validGlanceBackend()
	b.Name = name
	b.Namespace = namespace
	b.Spec.GlanceRef = GlanceRefSpec{Name: glanceRef}
	return b
}

// --- CRD-level CEL / schema enforcement (no validating webhook installed) ---

// TestIntegration_CRD_CELOnly_RejectsGlanceRefChange pins the glanceRef
// immutability transition rule: re-pointing a backend at a different Glance is
// rejected by the CRD CEL rule alone (the validating webhook never re-checks it).
func TestIntegration_CRD_CELOnly_RejectsGlanceRefChange(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)
	ns := newNamespace(t, ctx, c, "glanceref-immutable-")

	b := integrationBackend("backend", ns, "glance-a")
	g.Expect(c.Create(ctx, b)).To(Succeed(), "valid backend should be accepted")

	got := &GlanceBackend{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "backend", Namespace: ns}, got)).To(Succeed())
	got.Spec.GlanceRef.Name = "glance-b"

	err := c.Update(ctx, got)
	g.Expect(err).To(HaveOccurred(), "re-pointing glanceRef must be rejected on update")
	g.Expect(apierrors.IsInvalid(err) || apierrors.IsForbidden(err)).To(BeTrue(),
		fmt.Sprintf("expected Invalid or Forbidden status error, got: %v", err))
	g.Expect(err.Error()).To(ContainSubstring("glanceRef is immutable"))
}

// TestIntegration_CRD_CELOnly_RejectsTypeChange pins the type immutability
// transition rule. type is a single-value enum (S3), so no valid transition
// exists; changing it to any other value is rejected at the CRD layer — by the
// immutability CEL rule, the enum constraint, or both.
func TestIntegration_CRD_CELOnly_RejectsTypeChange(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)
	ns := newNamespace(t, ctx, c, "type-immutable-")

	b := integrationBackend("backend", ns, "glance-a")
	g.Expect(c.Create(ctx, b)).To(Succeed(), "valid backend should be accepted")

	got := &GlanceBackend{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "backend", Namespace: ns}, got)).To(Succeed())
	got.Spec.Type = GlanceBackendType("Ceph")

	err := c.Update(ctx, got)
	g.Expect(err).To(HaveOccurred(), "changing type must be rejected on update")
	g.Expect(apierrors.IsInvalid(err) || apierrors.IsForbidden(err)).To(BeTrue(),
		fmt.Sprintf("expected Invalid or Forbidden status error, got: %v", err))
	g.Expect(err.Error()).To(SatisfyAny(
		ContainSubstring("type is immutable"),
		ContainSubstring("Unsupported value"),
	))
}

// TestIntegration_CRD_CELOnly_RejectsS3UnionMissingBlock pins the type/s3 union
// rule at the CRD level: a type-S3 backend without a spec.s3 block is rejected
// by the CEL rule alone.
func TestIntegration_CRD_CELOnly_RejectsS3UnionMissingBlock(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)
	ns := newNamespace(t, ctx, c, "s3-union-")

	b := integrationBackend("backend", ns, "glance-a")
	b.Spec.S3 = nil

	err := c.Create(ctx, b)
	g.Expect(err).To(HaveOccurred(), "type S3 without spec.s3 must be rejected")
	g.Expect(apierrors.IsInvalid(err) || apierrors.IsForbidden(err)).To(BeTrue(),
		fmt.Sprintf("expected Invalid or Forbidden status error, got: %v", err))
	g.Expect(err.Error()).To(ContainSubstring("exactly one backend block matching spec.type"))
}

// TestIntegration_CRD_BucketURLFormatDefaultMaterialized proves the CRD schema
// default (path) is materialized without the mutating webhook: an S3 block that
// omits bucketURLFormat comes back with "path".
func TestIntegration_CRD_BucketURLFormatDefaultMaterialized(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)
	ns := newNamespace(t, ctx, c, "bucketurl-default-")

	b := integrationBackend("backend", ns, "glance-a")
	b.Spec.S3.BucketURLFormat = ""
	g.Expect(c.Create(ctx, b)).To(Succeed(), "backend without bucketURLFormat should be accepted")

	got := &GlanceBackend{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "backend", Namespace: ns}, got)).To(Succeed())
	g.Expect(got.Spec.S3.BucketURLFormat).To(Equal("path"),
		"CRD default must materialize bucketURLFormat=path")
}

// TestIntegration_CRD_CELOnly_RejectsExtraOptionsSizeGuard pins the
// MaxProperties=32 guard on spec.extraOptions: a 33-entry map is rejected by the
// CRD schema alone.
func TestIntegration_CRD_CELOnly_RejectsExtraOptionsSizeGuard(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)
	ns := newNamespace(t, ctx, c, "extraopts-size-")

	b := integrationBackend("backend", ns, "glance-a")
	b.Spec.ExtraOptions = map[string]string{}
	for i := 0; i < 33; i++ {
		b.Spec.ExtraOptions[fmt.Sprintf("opt_%d", i)] = "v"
	}

	err := c.Create(ctx, b)
	g.Expect(err).To(HaveOccurred(), "more than 32 extraOptions must be rejected")
	g.Expect(apierrors.IsInvalid(err) || apierrors.IsForbidden(err)).To(BeTrue(),
		fmt.Sprintf("expected Invalid or Forbidden status error, got: %v", err))
	g.Expect(err.Error()).To(ContainSubstring("extraOptions"))
}

// TestIntegration_CRD_CELOnly_OpenStackReleasePattern pins the
// openStackRelease pattern (^\d{4}\.[12]$) at the CRD level: cadence releases
// are accepted; a non-cadence minor and a non-numeric value are rejected.
func TestIntegration_CRD_CELOnly_OpenStackReleasePattern(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)

	cases := []struct {
		release string
		accept  bool
	}{
		{"2025.2", true},
		{"2026.1", true},
		{"2025.3", false},
		{"banana", false},
	}
	for i, tc := range cases {
		tc := tc
		t.Run(tc.release, func(t *testing.T) {
			g := NewGomegaWithT(t)
			ns := newNamespace(t, ctx, c, "release-pattern-")

			glance := integrationGlance(fmt.Sprintf("glance-%d", i), ns)
			glance.Spec.OpenStackRelease = tc.release

			err := c.Create(ctx, glance)
			if tc.accept {
				g.Expect(err).NotTo(HaveOccurred(), "release %q should be accepted", tc.release)
				return
			}
			g.Expect(err).To(HaveOccurred(), "release %q should be rejected", tc.release)
			g.Expect(apierrors.IsInvalid(err) || apierrors.IsForbidden(err)).To(BeTrue(),
				fmt.Sprintf("expected Invalid or Forbidden status error, got: %v", err))
			g.Expect(err.Error()).To(ContainSubstring("openStackRelease"))
		})
	}
}

// TestIntegration_CRD_CELOnly_RejectsUWSGIKeepAliveTimeout pins the UWSGISpec
// cross-field CEL rule: httpKeepAliveTimeout may only be set when httpKeepAlive
// is true. Setting the timeout alongside an explicit httpKeepAlive=false is
// rejected by the CRD schema alone. httpKeepAlive is a nil-preserving *bool, so
// ptr.To(false) is serialized verbatim (omitempty only drops a nil pointer),
// letting the rule fire through a typed client.
func TestIntegration_CRD_CELOnly_RejectsUWSGIKeepAliveTimeout(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)
	ns := newNamespace(t, ctx, c, "uwsgi-keepalive-")

	glance := integrationGlance("glance", ns)
	glance.Spec.OpenStackRelease = "2026.1"
	glance.Spec.APIServer = &APIServerSpec{
		UWSGI: &UWSGISpec{
			Processes:            2,
			Threads:              1,
			HTTPKeepAlive:        ptr.To(false),
			HTTPKeepAliveTimeout: ptr.To(int32(30)),
		},
	}

	err := c.Create(ctx, glance)
	g.Expect(err).To(HaveOccurred(), "httpKeepAliveTimeout with httpKeepAlive=false must be rejected")
	g.Expect(apierrors.IsInvalid(err) || apierrors.IsForbidden(err)).To(BeTrue(),
		fmt.Sprintf("expected Invalid or Forbidden status error, got: %v", err))
	g.Expect(err.Error()).To(ContainSubstring("httpKeepAliveTimeout may only be set when httpKeepAlive is true"))
}

// --- Live admission round-trip (webhooks running) ---

// TestIntegration_WebhookDefaultsServiceUser proves the mutating webhook fills
// the service-user identity defaults and the secretRef key on a minimal CR that
// supplies only the password Secret name.
func TestIntegration_WebhookDefaultsServiceUser(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTest(t)
	ns := newNamespace(t, ctx, c, "serviceuser-defaults-")

	glance := integrationGlance("glance", ns)
	// Minimal service-user block: only the password Secret name.
	glance.Spec.ServiceUser = ServiceUserSpec{SecretRef: commonv1.SecretRefSpec{Name: "glance-service-password"}}

	g.Expect(c.Create(ctx, glance)).To(Succeed(), "minimal Glance should be accepted after defaults")

	got := &Glance{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "glance", Namespace: ns}, got)).To(Succeed())
	g.Expect(got.Spec.ServiceUser.Username).To(Equal("glance"))
	g.Expect(got.Spec.ServiceUser.ProjectName).To(Equal("service"))
	g.Expect(got.Spec.ServiceUser.UserDomainName).To(Equal("Default"))
	g.Expect(got.Spec.ServiceUser.ProjectDomainName).To(Equal("Default"))
	g.Expect(got.Spec.ServiceUser.SecretRef.Key).To(Equal("password"))
}

// TestIntegration_WebhookRejectsReservedBackendName proves the validating
// webhook rejects a backend whose metadata.name uses the reserved os-glance-
// store-section prefix.
func TestIntegration_WebhookRejectsReservedBackendName(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTest(t)
	ns := newNamespace(t, ctx, c, "reserved-name-")

	b := integrationBackend("os-glance-foo", ns, "glance-a")

	err := c.Create(ctx, b)
	g.Expect(err).To(HaveOccurred(), "a reserved-prefix backend name must be rejected")
	g.Expect(err.Error()).To(ContainSubstring(`reserved "os_glance_" / "os-glance-" store-section prefix`))
}

// TestIntegration_WebhookEnforcesSingleDefault proves the validating webhook
// enforces the single-default invariant per Glance via the live API: a second
// default for the same glanceRef is rejected, while a default for a different
// glanceRef is accepted.
func TestIntegration_WebhookEnforcesSingleDefault(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTest(t)
	ns := newNamespace(t, ctx, c, "single-default-")

	first := integrationBackend("backend-a1", ns, "glance-a")
	first.Spec.IsDefault = true
	g.Expect(c.Create(ctx, first)).To(Succeed(), "first default for glance-a should be accepted")

	second := integrationBackend("backend-a2", ns, "glance-a")
	second.Spec.IsDefault = true
	err := c.Create(ctx, second)
	g.Expect(err).To(HaveOccurred(), "a second default for the same Glance must be rejected")
	g.Expect(err.Error()).To(ContainSubstring("already marked isDefault"))

	// A default for a DIFFERENT Glance is unaffected by glance-a's default.
	other := integrationBackend("backend-b1", ns, "glance-b")
	other.Spec.IsDefault = true
	g.Expect(c.Create(ctx, other)).To(Succeed(), "a default for a different Glance should be accepted")
}
