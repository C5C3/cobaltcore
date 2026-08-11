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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1 "github.com/c5c3/forge/internal/common/types"
	"github.com/c5c3/forge/operators/barbican/internal/testutil"
)

// --- Helpers ---

// setupEnvTest wraps testutil.SetupBarbicanEnvTest with the v1alpha1 scheme
// registration and both webhook setups (Barbican + BarbicanSecretStore),
// avoiding the import cycle between testutil and this package. The webhook
// manifests envtest installs carry both kinds (failurePolicy=Fail), so both
// handlers must be served or admission of the unserved kind fails.
func setupEnvTest(t testing.TB) (client.Client, context.Context, context.CancelFunc) {
	t.Helper()
	return testutil.SetupBarbicanEnvTest(t, AddToScheme, func(mgr ctrl.Manager) error {
		// mgr.GetAPIReader() mirrors production wiring in main.go: webhook
		// admission lookups (PriorityClass existence, the sibling-default and
		// OpenBao-uniqueness Lists) read the API server directly, never a stale
		// informer cache.
		if err := (&BarbicanWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr); err != nil {
			return err
		}
		return (&BarbicanSecretStoreWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr)
	})
}

// setupEnvTestNoWebhook wraps testutil.SetupBarbicanEnvTestNoWebhook with the
// v1alpha1 scheme registration. Used by the CRD-only tests so no webhook can
// mask a missing CEL rule or supply a default the CRD schema must supply itself.
func setupEnvTestNoWebhook(t testing.TB) (client.Client, context.Context, context.CancelFunc) {
	t.Helper()
	return testutil.SetupBarbicanEnvTestNoWebhook(t, AddToScheme)
}

// newNamespace creates a uniquely named namespace for a test.
func newNamespace(t testing.TB, ctx context.Context, c client.Client, prefix string) string {
	t.Helper()
	g := NewGomegaWithT(t)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: prefix}}
	g.Expect(c.Create(ctx, ns)).To(Succeed(), "create namespace")
	return ns.Name
}

// integrationBarbican returns validBarbican() stamped with name/namespace for
// API server submission. validBarbican() is a webhook-unit helper whose
// validators do not enforce the CRD required-field markers, so it omits
// spec.database.database and spec.database.secretRef.name; those are filled here
// so the object clears the real CRD schema (both are required even in
// managed/clusterRef mode).
func integrationBarbican(name, namespace string) *Barbican {
	barbican := validBarbican()
	barbican.Name = name
	barbican.Namespace = namespace
	barbican.Spec.Database.Database = "barbican"
	barbican.Spec.Database.SecretRef = commonv1.SecretRefSpec{Name: "barbican-db"}
	return barbican
}

// integrationStore returns validBarbicanSecretStore() stamped with
// name/namespace and pointed at barbicanRef, for API server submission.
func integrationStore(name, namespace, barbicanRef string) *BarbicanSecretStore {
	store := validBarbicanSecretStore()
	store.Name = name
	store.Namespace = namespace
	store.Spec.BarbicanRef = BarbicanRefSpec{Name: barbicanRef}
	return store
}

// --- CRD-level CEL / schema enforcement (no validating webhook installed) ---

// TestIntegration_CRD_CELOnly_RejectsBarbicanRefChange pins the barbicanRef
// immutability transition rule: re-pointing a store at a different Barbican is
// rejected by the CRD CEL rule alone (the validating webhook never re-checks
// it).
func TestIntegration_CRD_CELOnly_RejectsBarbicanRefChange(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)
	ns := newNamespace(t, ctx, c, "barbicanref-immutable-")

	store := integrationStore("store", ns, "barbican-a")
	g.Expect(c.Create(ctx, store)).To(Succeed(), "valid store should be accepted")

	got := &BarbicanSecretStore{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "store", Namespace: ns}, got)).To(Succeed())
	got.Spec.BarbicanRef.Name = "barbican-b"

	err := c.Update(ctx, got)
	g.Expect(err).To(HaveOccurred(), "re-pointing barbicanRef must be rejected on update")
	g.Expect(apierrors.IsInvalid(err) || apierrors.IsForbidden(err)).To(BeTrue(),
		fmt.Sprintf("expected Invalid or Forbidden status error, got: %v", err))
	g.Expect(err.Error()).To(ContainSubstring("barbicanRef is immutable"))
}

// TestIntegration_CRD_CELOnly_RejectsTypeChange pins the type immutability
// transition rule. type is a single-value enum (OpenBao), so no valid transition
// exists; changing it to any other value is rejected at the CRD layer, by the
// immutability CEL rule, the enum constraint, or both.
func TestIntegration_CRD_CELOnly_RejectsTypeChange(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)
	ns := newNamespace(t, ctx, c, "type-immutable-")

	store := integrationStore("store", ns, "barbican-a")
	g.Expect(c.Create(ctx, store)).To(Succeed(), "valid store should be accepted")

	got := &BarbicanSecretStore{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "store", Namespace: ns}, got)).To(Succeed())
	got.Spec.Type = BarbicanSecretStoreType("Vault")

	err := c.Update(ctx, got)
	g.Expect(err).To(HaveOccurred(), "changing type must be rejected on update")
	g.Expect(apierrors.IsInvalid(err) || apierrors.IsForbidden(err)).To(BeTrue(),
		fmt.Sprintf("expected Invalid or Forbidden status error, got: %v", err))
	g.Expect(err.Error()).To(SatisfyAny(
		ContainSubstring("type is immutable"),
		ContainSubstring("Unsupported value"),
	))
}

// TestIntegration_CRD_CELOnly_RejectsStoreRelocation pins the two transition
// rules that carry the same data-safety argument as the frozen spec.type: the
// mount and the mode decide where the secret material lives, so material written
// under the old mount is not reachable under a new one, and flipping between a
// managed instance and a brownfield server re-points the plugin at a different
// server entirely. Neither is re-checked by the validating webhook, which never
// sees the old object.
func TestIntegration_CRD_CELOnly_RejectsStoreRelocation(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)

	cases := []struct {
		name    string
		seed    func(*BarbicanSecretStore)
		mutate  func(*BarbicanSecretStore)
		wantSub string
	}{
		{
			name: "brownfield store moved to another mount",
			seed: func(s *BarbicanSecretStore) {
				s.Spec.OpenBao.InstanceRef = nil
				s.Spec.OpenBao.Server = &OpenBaoServerSpec{
					URL:                  "https://bao.example.com:8200",
					CredentialsSecretRef: SecretNameRefSpec{Name: "brownfield-approle"},
				}
			},
			mutate:  func(s *BarbicanSecretStore) { s.Spec.OpenBao.KVMountpoint = "kv-elsewhere" },
			wantSub: "kvMountpoint is immutable",
		},
		{
			name: "managed store switched to a brownfield server",
			seed: func(*BarbicanSecretStore) {},
			mutate: func(s *BarbicanSecretStore) {
				s.Spec.OpenBao.InstanceRef = nil
				s.Spec.OpenBao.Server = &OpenBaoServerSpec{
					URL:                  "https://bao.example.com:8200",
					CredentialsSecretRef: SecretNameRefSpec{Name: "brownfield-approle"},
				}
			},
			wantSub: "the store mode (instanceRef vs server) is immutable",
		},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			ns := newNamespace(t, ctx, c, "store-relocation-")

			name := fmt.Sprintf("store-%d", i)
			store := integrationStore(name, ns, "barbican-a")
			tc.seed(store)
			g.Expect(c.Create(ctx, store)).To(Succeed(), "the seeded store shape must be valid")

			got := &BarbicanSecretStore{}
			g.Expect(c.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, got)).To(Succeed())
			tc.mutate(got)

			err := c.Update(ctx, got)
			g.Expect(err).To(HaveOccurred(), "relocating a store's secret material must be rejected on update")
			g.Expect(apierrors.IsInvalid(err) || apierrors.IsForbidden(err)).To(BeTrue(),
				fmt.Sprintf("expected Invalid or Forbidden status error, got: %v", err))
			g.Expect(err.Error()).To(ContainSubstring(tc.wantSub))
		})
	}
}

// TestIntegration_CRD_CELOnly_RejectsStoreUnionViolations walks the three union
// CEL rules the store schema carries, one case per rule violation: the
// type/openBao union, the instanceRef/server union, and the mount layout a
// managed store is frozen to. Each case mutates exactly one aspect of an
// otherwise valid store, so the rejection is attributable to a single rule.
func TestIntegration_CRD_CELOnly_RejectsStoreUnionViolations(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)

	// server is the brownfield block the both-modes case adds alongside the
	// instanceRef the valid fixture already carries.
	server := &OpenBaoServerSpec{
		URL:                  "https://bao.example.com:8200",
		CredentialsSecretRef: SecretNameRefSpec{Name: "brownfield-approle"},
	}

	cases := []struct {
		name    string
		mutate  func(*BarbicanSecretStore)
		wantSub string
	}{
		{
			name:    "type OpenBao without an openBao block",
			mutate:  func(s *BarbicanSecretStore) { s.Spec.OpenBao = nil },
			wantSub: "exactly one store block matching spec.type must be set",
		},
		{
			name:    "both instanceRef and server",
			mutate:  func(s *BarbicanSecretStore) { s.Spec.OpenBao.Server = server },
			wantSub: "exactly one of instanceRef or server must be set",
		},
		{
			name:    "neither instanceRef nor server",
			mutate:  func(s *BarbicanSecretStore) { s.Spec.OpenBao.InstanceRef = nil },
			wantSub: "exactly one of instanceRef or server must be set",
		},
		{
			name:    "managed store on a foreign kvMountpoint",
			mutate:  func(s *BarbicanSecretStore) { s.Spec.OpenBao.KVMountpoint = "kv-barbican" },
			wantSub: "must keep kvMountpoint barbican and leave namespace unset",
		},
		{
			name:    "managed store scoped to an OpenBao namespace",
			mutate:  func(s *BarbicanSecretStore) { s.Spec.OpenBao.Namespace = "tenant-a" },
			wantSub: "must keep kvMountpoint barbican and leave namespace unset",
		},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			ns := newNamespace(t, ctx, c, "store-union-")

			store := integrationStore(fmt.Sprintf("store-%d", i), ns, "barbican-a")
			tc.mutate(store)

			err := c.Create(ctx, store)
			g.Expect(err).To(HaveOccurred(), "the schema must reject this store shape")
			g.Expect(apierrors.IsInvalid(err) || apierrors.IsForbidden(err)).To(BeTrue(),
				fmt.Sprintf("expected Invalid or Forbidden status error, got: %v", err))
			g.Expect(err.Error()).To(ContainSubstring(tc.wantSub))
		})
	}
}

// TestIntegration_CRD_KVMountpointDefaultMaterialized proves the CRD schema
// default (barbican) is materialized without the mutating webhook: a store whose
// openBao block omits kvMountpoint comes back carrying the provisioned mount.
func TestIntegration_CRD_KVMountpointDefaultMaterialized(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)
	ns := newNamespace(t, ctx, c, "kvmountpoint-default-")

	store := integrationStore("store", ns, "barbican-a")
	store.Spec.OpenBao.KVMountpoint = ""
	g.Expect(c.Create(ctx, store)).To(Succeed(), "a store without kvMountpoint should be accepted")

	got := &BarbicanSecretStore{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "store", Namespace: ns}, got)).To(Succeed())
	g.Expect(got.Spec.OpenBao.KVMountpoint).To(Equal(DefaultKVMountpoint),
		"the CRD default must materialize kvMountpoint=barbican")
}

// TestIntegration_CRD_CELOnly_RejectsTargetClusterRefChange pins the
// targetClusterRef rename rule: re-pointing a Barbican at another target
// cluster is rejected by the CRD CEL rule alone, without the validating webhook.
func TestIntegration_CRD_CELOnly_RejectsTargetClusterRefChange(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)
	ns := newNamespace(t, ctx, c, "targetcluster-rename-")

	barbican := integrationBarbican("barbican", ns)
	barbican.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "cluster-a"}
	g.Expect(c.Create(ctx, barbican)).To(Succeed(), "valid Barbican should be accepted")

	got := &Barbican{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "barbican", Namespace: ns}, got)).To(Succeed())
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

	local := integrationBarbican("barbican-local", ns)
	g.Expect(c.Create(ctx, local)).To(Succeed(), "Barbican without targetClusterRef should be accepted")

	got := &Barbican{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "barbican-local", Namespace: ns}, got)).To(Succeed())
	got.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "cluster-a"}

	err := c.Update(ctx, got)
	g.Expect(err).To(HaveOccurred(), "adding targetClusterRef must be rejected on update")
	g.Expect(apierrors.IsInvalid(err) || apierrors.IsForbidden(err)).To(BeTrue(),
		fmt.Sprintf("expected Invalid or Forbidden status error, got: %v", err))
	g.Expect(err.Error()).To(ContainSubstring("targetClusterRef is immutable"))

	remote := integrationBarbican("barbican-remote", ns)
	remote.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "cluster-a"}
	g.Expect(c.Create(ctx, remote)).To(Succeed(), "Barbican with targetClusterRef should be accepted")

	got = &Barbican{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "barbican-remote", Namespace: ns}, got)).To(Succeed())
	got.Spec.TargetClusterRef = nil

	err = c.Update(ctx, got)
	g.Expect(err).To(HaveOccurred(), "removing targetClusterRef must be rejected on update")
	g.Expect(apierrors.IsInvalid(err) || apierrors.IsForbidden(err)).To(BeTrue(),
		fmt.Sprintf("expected Invalid or Forbidden status error, got: %v", err))
	g.Expect(err.Error()).To(ContainSubstring("targetClusterRef is immutable"))
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

	barbican := integrationBarbican("barbican", ns)
	// Minimal service-user block: only the password Secret name.
	barbican.Spec.ServiceUser = ServiceUserSpec{SecretRef: commonv1.SecretRefSpec{Name: "barbican-service-password"}}

	g.Expect(c.Create(ctx, barbican)).To(Succeed(), "minimal Barbican should be accepted after defaults")

	got := &Barbican{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "barbican", Namespace: ns}, got)).To(Succeed())
	g.Expect(got.Spec.ServiceUser.Username).To(Equal("barbican"))
	g.Expect(got.Spec.ServiceUser.ProjectName).To(Equal("service"))
	g.Expect(got.Spec.ServiceUser.UserDomainName).To(Equal("Default"))
	g.Expect(got.Spec.ServiceUser.ProjectDomainName).To(Equal("Default"))
	g.Expect(got.Spec.ServiceUser.SecretRef.Key).To(Equal("password"))
}
