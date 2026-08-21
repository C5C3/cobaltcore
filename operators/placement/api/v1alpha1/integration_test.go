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

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	"github.com/c5c3/cobaltcore/operators/placement/internal/testutil"
)

// --- Helpers ---

// setupEnvTest wraps testutil.SetupPlacementEnvTest with the v1alpha1 scheme
// registration and the webhook setup, avoiding the import cycle between testutil
// and this package.
func setupEnvTest(t testing.TB) (client.Client, context.Context, context.CancelFunc) {
	t.Helper()
	return testutil.SetupPlacementEnvTest(t, AddToScheme, func(mgr ctrl.Manager) error {
		// mgr.GetAPIReader() mirrors production wiring in main.go: webhook
		// admission lookups (the PriorityClass existence check) read the API
		// server directly, never a stale informer cache.
		return (&PlacementWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr)
	})
}

// setupEnvTestNoWebhook wraps testutil.SetupPlacementEnvTestNoWebhook with the
// v1alpha1 scheme registration. Used by the CRD-only tests so no webhook can
// mask a missing schema rule or supply a default the CRD schema must supply
// itself.
func setupEnvTestNoWebhook(t testing.TB) (client.Client, context.Context, context.CancelFunc) {
	t.Helper()
	return testutil.SetupPlacementEnvTestNoWebhook(t, AddToScheme)
}

// newNamespace creates a uniquely named namespace for a test.
func newNamespace(t testing.TB, ctx context.Context, c client.Client, prefix string) string {
	t.Helper()
	g := NewGomegaWithT(t)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: prefix}}
	g.Expect(c.Create(ctx, ns)).To(Succeed(), "create namespace")
	return ns.Name
}

// integrationPlacement returns validPlacement() stamped with name/namespace for
// API server submission. validPlacement() is a webhook-unit helper whose
// validators do not enforce the CRD required-field markers, so it omits
// spec.database.database and spec.database.secretRef.name; those are filled here
// so the object clears the real CRD schema (both are required even in managed/
// clusterRef mode).
func integrationPlacement(name, namespace string) *Placement {
	placement := validPlacement()
	placement.Name = name
	placement.Namespace = namespace
	placement.Spec.Database.Database = "placement"
	placement.Spec.Database.SecretRef = commonv1.SecretRefSpec{Name: "placement-db"}
	return placement
}

// --- CRD-level schema / CEL enforcement (no validating webhook installed) ---

// TestIntegration_CRD_CELOnly_OpenStackReleasePattern pins the openStackRelease
// pattern (^\d{4}\.[12]$) at the CRD level: cadence releases are accepted; a
// non-cadence minor and a non-numeric value are rejected by the schema alone.
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
		t.Run(tc.release, func(t *testing.T) {
			g := NewGomegaWithT(t)
			ns := newNamespace(t, ctx, c, "release-pattern-")

			placement := integrationPlacement(fmt.Sprintf("placement-%d", i), ns)
			placement.Spec.OpenStackRelease = tc.release

			err := c.Create(ctx, placement)
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

// TestIntegration_CRD_CELOnly_RejectsKeystoneEndpointWithoutScheme pins the
// keystoneEndpoint pattern (^https?://) at the CRD level: a bare host:port value
// carries no scheme, so keystonemiddleware could not build an auth_url from it,
// and the schema alone rejects it.
func TestIntegration_CRD_CELOnly_RejectsKeystoneEndpointWithoutScheme(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)
	ns := newNamespace(t, ctx, c, "keystone-endpoint-")

	placement := integrationPlacement("placement", ns)
	placement.Spec.KeystoneEndpoint = "keystone.openstack.svc.cluster.local:5000/v3"

	err := c.Create(ctx, placement)
	g.Expect(err).To(HaveOccurred(), "a scheme-less keystoneEndpoint must be rejected")
	g.Expect(apierrors.IsInvalid(err) || apierrors.IsForbidden(err)).To(BeTrue(),
		fmt.Sprintf("expected Invalid or Forbidden status error, got: %v", err))
	g.Expect(err.Error()).To(ContainSubstring("keystoneEndpoint"))
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

	placement := integrationPlacement("placement", ns)
	placement.Spec.APIServer = &APIServerSpec{
		UWSGI: &UWSGISpec{
			Processes:            2,
			Threads:              1,
			HTTPKeepAlive:        ptr.To(false),
			HTTPKeepAliveTimeout: ptr.To(int32(30)),
		},
	}

	err := c.Create(ctx, placement)
	g.Expect(err).To(HaveOccurred(), "httpKeepAliveTimeout with httpKeepAlive=false must be rejected")
	g.Expect(apierrors.IsInvalid(err) || apierrors.IsForbidden(err)).To(BeTrue(),
		fmt.Sprintf("expected Invalid or Forbidden status error, got: %v", err))
	g.Expect(err.Error()).To(ContainSubstring("httpKeepAliveTimeout may only be set when httpKeepAlive is true"))
}

// TestIntegration_CRD_CELOnly_RejectsCacheControlChars pins the schema-layer
// half of the cache control-character guard, which the shared commonv1.CacheSpec
// carries for every operator that embeds it: an items pattern on servers and a
// CEL rule on clusterRef.name. Both cache shapes feed the verbatim INI renderer
// through cache.ResolveServers, so a newline injects an extra
// [keystone_authtoken] line. The webhook enforces the same rule
// (validation.CacheNoControlChars); this test runs without it so a regression in
// the markers cannot hide behind the webhook.
func TestIntegration_CRD_CELOnly_RejectsCacheControlChars(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)

	cases := []struct {
		name    string
		cache   commonv1.CacheSpec
		wantSub string
	}{
		{
			name: "server with a newline",
			cache: commonv1.CacheSpec{
				Backend: commonv1.DefaultCacheBackend,
				Servers: []string{"mc-0:11211\nauth_url = http://attacker.example/v3"},
			},
			wantSub: "servers",
		},
		{
			name: "clusterRef name with a carriage return",
			cache: commonv1.CacheSpec{
				Backend:    commonv1.DefaultCacheBackend,
				ClusterRef: &corev1.LocalObjectReference{Name: "mc\rauth_url = http://attacker.example/v3"},
			},
			wantSub: "clusterRef.name must not contain a newline or carriage return",
		},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			ns := newNamespace(t, ctx, c, "cache-control-chars-")

			placement := integrationPlacement(fmt.Sprintf("placement-%d", i), ns)
			placement.Spec.Cache = tc.cache

			err := c.Create(ctx, placement)
			g.Expect(err).To(HaveOccurred(), "a cache value with a control character must be rejected")
			g.Expect(apierrors.IsInvalid(err) || apierrors.IsForbidden(err)).To(BeTrue(),
				fmt.Sprintf("expected Invalid or Forbidden status error, got: %v", err))
			g.Expect(err.Error()).To(ContainSubstring(tc.wantSub))
		})
	}
}

// TestIntegration_CRD_CELOnly_RejectsTargetClusterRefChange pins the
// targetClusterRef rename rule: re-pointing a Placement at another target
// cluster is rejected by the CRD CEL rule alone, without the validating webhook.
func TestIntegration_CRD_CELOnly_RejectsTargetClusterRefChange(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)
	ns := newNamespace(t, ctx, c, "targetcluster-rename-")

	placement := integrationPlacement("placement", ns)
	placement.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "cluster-a"}
	g.Expect(c.Create(ctx, placement)).To(Succeed(), "valid Placement should be accepted")

	got := &Placement{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "placement", Namespace: ns}, got)).To(Succeed())
	got.Spec.TargetClusterRef.Name = "cluster-b"

	err := c.Update(ctx, got)
	g.Expect(err).To(HaveOccurred(), "renaming targetClusterRef must be rejected on update")
	g.Expect(apierrors.IsInvalid(err) || apierrors.IsForbidden(err)).To(BeTrue(),
		fmt.Sprintf("expected Invalid or Forbidden status error, got: %v", err))
	g.Expect(err.Error()).To(ContainSubstring("targetClusterRef is immutable"))
}

// TestIntegration_CRD_CELOnly_RejectsTargetClusterRefPresenceFlip pins the
// presence rule in both directions: a CR created without the ref cannot gain
// one, and a CR created with it cannot drop it. Either edit would move the
// children away from the cluster that already holds them.
func TestIntegration_CRD_CELOnly_RejectsTargetClusterRefPresenceFlip(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)
	ns := newNamespace(t, ctx, c, "targetcluster-flip-")

	local := integrationPlacement("placement-local", ns)
	g.Expect(c.Create(ctx, local)).To(Succeed(), "Placement without targetClusterRef should be accepted")

	got := &Placement{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "placement-local", Namespace: ns}, got)).To(Succeed())
	got.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "cluster-a"}

	err := c.Update(ctx, got)
	g.Expect(err).To(HaveOccurred(), "adding targetClusterRef must be rejected on update")
	g.Expect(apierrors.IsInvalid(err) || apierrors.IsForbidden(err)).To(BeTrue(),
		fmt.Sprintf("expected Invalid or Forbidden status error, got: %v", err))
	g.Expect(err.Error()).To(ContainSubstring("targetClusterRef is immutable"))

	remote := integrationPlacement("placement-remote", ns)
	remote.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "cluster-a"}
	g.Expect(c.Create(ctx, remote)).To(Succeed(), "Placement with targetClusterRef should be accepted")

	got = &Placement{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "placement-remote", Namespace: ns}, got)).To(Succeed())
	got.Spec.TargetClusterRef = nil

	err = c.Update(ctx, got)
	g.Expect(err).To(HaveOccurred(), "removing targetClusterRef must be rejected on update")
	g.Expect(apierrors.IsInvalid(err) || apierrors.IsForbidden(err)).To(BeTrue(),
		fmt.Sprintf("expected Invalid or Forbidden status error, got: %v", err))
	g.Expect(err.Error()).To(ContainSubstring("targetClusterRef is immutable"))
}

// --- Live admission round-trip (webhooks running) ---

// TestIntegration_WebhookDefaultsMinimalCR proves the mutating webhook turns a
// minimal CR — the required fields plus only the service-user password Secret
// name — into a fully specified one: the service-user identity and secretRef
// key, the placement-specific container resource budget, and a materialized
// spec.logging block.
func TestIntegration_WebhookDefaultsMinimalCR(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTest(t)
	ns := newNamespace(t, ctx, c, "placement-defaults-")

	placement := integrationPlacement("placement", ns)
	// Minimal service-user block: only the password Secret name.
	placement.Spec.ServiceUser = ServiceUserSpec{SecretRef: commonv1.SecretRefSpec{Name: "placement-service-password"}}

	g.Expect(c.Create(ctx, placement)).To(Succeed(), "minimal Placement should be accepted after defaults")

	got := &Placement{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "placement", Namespace: ns}, got)).To(Succeed())

	g.Expect(got.Spec.ServiceUser.Username).To(Equal("placement"))
	g.Expect(got.Spec.ServiceUser.ProjectName).To(Equal("service"))
	g.Expect(got.Spec.ServiceUser.UserDomainName).To(Equal("Default"))
	g.Expect(got.Spec.ServiceUser.ProjectDomainName).To(Equal("Default"))
	g.Expect(got.Spec.ServiceUser.SecretRef.Key).To(Equal("password"))

	// The placement-specific memory budget (512Mi/1Gi) replaces the shared
	// 256Mi/512Mi baseline; CPU keeps the shared defaults.
	g.Expect(got.Spec.Deployment.Resources).NotTo(BeNil(), "resources must be defaulted")
	requestedMemory := got.Spec.Deployment.Resources.Requests[corev1.ResourceMemory]
	limitMemory := got.Spec.Deployment.Resources.Limits[corev1.ResourceMemory]
	requestedCPU := got.Spec.Deployment.Resources.Requests[corev1.ResourceCPU]
	limitCPU := got.Spec.Deployment.Resources.Limits[corev1.ResourceCPU]
	g.Expect(requestedMemory.String()).To(Equal("512Mi"))
	g.Expect(limitMemory.String()).To(Equal("1Gi"))
	g.Expect(requestedCPU.String()).To(Equal("100m"))
	g.Expect(limitCPU.String()).To(Equal("500m"))

	// spec.logging is materialized so the reconciler never dereferences a nil
	// pointer.
	g.Expect(got.Spec.Logging).NotTo(BeNil(), "logging must be materialized")
	g.Expect(got.Spec.Logging.Format).To(Equal("text"))
	g.Expect(got.Spec.Logging.Level).To(Equal("INFO"))
	g.Expect(got.Spec.Logging.Debug).NotTo(BeNil())
	g.Expect(*got.Spec.Logging.Debug).To(BeFalse())
}

// TestIntegration_WebhookRejectsMissingPriorityClass proves the validating
// webhook rejects a CR naming a PriorityClass that does not exist. The rule
// comes from the shared validation.PriorityClassExists validator and needs a
// cluster lookup, so no CRD schema rule can express it: reaching the rejection
// proves the request travelled all the way to the validating webhook and that
// its injected reader resolved against the live API server.
func TestIntegration_WebhookRejectsMissingPriorityClass(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTest(t)
	ns := newNamespace(t, ctx, c, "priorityclass-")

	placement := integrationPlacement("placement", ns)
	placement.Spec.Deployment.PriorityClassName = ptr.To("no-such-priority-class")

	err := c.Create(ctx, placement)
	g.Expect(err).To(HaveOccurred(), "an unknown priorityClassName must be rejected")
	g.Expect(apierrors.IsInvalid(err) || apierrors.IsForbidden(err)).To(BeTrue(),
		fmt.Sprintf("expected Invalid or Forbidden status error, got: %v", err))
	g.Expect(err.Error()).To(ContainSubstring("spec.deployment.priorityClassName"))
	g.Expect(err.Error()).To(ContainSubstring("no-such-priority-class"))
}

// TestIntegration_CRLifecycleRoundTrip walks a Placement through the full CR
// lifecycle against the live API server with the webhooks running: create, read
// back, update a spec field, read the update back, delete, and confirm the
// object is gone.
func TestIntegration_CRLifecycleRoundTrip(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTest(t)
	ns := newNamespace(t, ctx, c, "placement-lifecycle-")
	key := types.NamespacedName{Name: "placement", Namespace: ns}

	placement := integrationPlacement("placement", ns)
	g.Expect(c.Create(ctx, placement)).To(Succeed(), "create Placement")

	created := &Placement{}
	g.Expect(c.Get(ctx, key, created)).To(Succeed(), "read the created Placement back")
	g.Expect(created.Spec.OpenStackRelease).To(Equal("2025.2"))
	g.Expect(created.Spec.Deployment.Replicas).To(Equal(int32(3)))
	// Captured before the Update, which writes the server's response back into
	// created and would otherwise carry the bumped generation.
	createdGeneration := created.Generation

	created.Spec.Deployment.Replicas = 5
	g.Expect(c.Update(ctx, created)).To(Succeed(), "update spec.deployment.replicas")

	updated := &Placement{}
	g.Expect(c.Get(ctx, key, updated)).To(Succeed(), "read the updated Placement back")
	g.Expect(updated.Spec.Deployment.Replicas).To(Equal(int32(5)))
	g.Expect(updated.Generation).To(BeNumerically(">", createdGeneration),
		"a spec edit must bump metadata.generation")

	g.Expect(c.Delete(ctx, updated)).To(Succeed(), "delete Placement")
	err := c.Get(ctx, key, &Placement{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
		fmt.Sprintf("Placement should be gone after delete, got: %v", err))
}
