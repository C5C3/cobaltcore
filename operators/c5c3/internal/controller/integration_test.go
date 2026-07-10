// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

// Package controller contains the envtest integration test for the ControlPlane
// reconciler. Unlike the fake-client unit tests, this test
// runs the reconciler inside a real controller-runtime manager against a live
// envtest API server (CRDs + validating/defaulting webhook), and drives the full
// sub-reconciler chain — Infrastructure -> Keystone -> KORC -> AdminCredential ->
// Catalog — to the aggregate Ready=True by simulating each external dependency's
// readiness exactly as the production operators would report it.
package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	esgenv1alpha1 "github.com/external-secrets/external-secrets/apis/generators/v1alpha1"
	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	"github.com/c5c3/forge/internal/common/testutil/simulators"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
	"github.com/c5c3/forge/operators/c5c3/internal/testutil"
	horizonv1alpha1 "github.com/c5c3/forge/operators/horizon/api/v1alpha1"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// Integration test timing constants. Polling is generous because every step
// waits on the manager's reconcile loop to observe an externally-simulated
// status transition and requeue (the sub-reconcilers requeue on the order of
// 5-15s, but condition flips are picked up on the next watch-triggered
// reconcile, so the timeouts only bound real stalls).
const (
	itEventuallyTimeout = 60 * time.Second
	itPollInterval      = 500 * time.Millisecond
)

// setupControlPlaneEnvTest wraps testutil.SetupC5c3EnvTestWithController with the
// c5c3 scheme, the ControlPlane webhook, and an INLINE controller builder.
//
// DECISION the controller is registered via an inline
// builder (For/Owns/Watches + the field-indexer registration) rather than by
// calling ControlPlaneReconciler.SetupWithManager directly, and uses
// WithOptions(controller.Options{SkipNameValidation: ptr.To(true)}). This mirrors
// the keystone integration wrapper and keeps the helper reusable if a second
// integration test function ever registers the controller in the same package
// test binary — controller-runtime rejects two controllers with the same name
// unless name validation is skipped. The builder is kept byte-for-byte in step
// with SetupWithManager (same Owns set, same Watches mapper, same indexer) so it
// exercises the real wiring.
func setupControlPlaneEnvTest(t testing.TB) (client.Client, context.Context, context.CancelFunc) {
	t.Helper()
	return testutil.SetupC5c3EnvTestWithController(
		t,
		c5c3v1alpha1.AddToScheme,
		func(mgr ctrl.Manager) error {
			// mgr.GetAPIReader() mirrors the production wiring in main.go: webhook
			// admission lookups read the API server directly, never a stale cache.
			return (&c5c3v1alpha1.ControlPlaneWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr)
		},
		func(mgr ctrl.Manager) error {
			r := &ControlPlaneReconciler{
				Client:   mgr.GetClient(),
				Scheme:   mgr.GetScheme(),
				Recorder: mgr.GetEventRecorderFor("controlplane-controller"),
			}
			// Register the ControlPlane secret-name field indexer so
			// secretToControlPlaneMapper's MatchingFields lookup works, mirroring
			// what SetupWithManager does in production.
			if err := registerControlPlaneSecretNameIndex(context.Background(), mgr.GetFieldIndexer()); err != nil {
				return err
			}

			memcached := &unstructured.Unstructured{}
			memcached.SetGroupVersionKind(memcachedGVK)

			return ctrl.NewControllerManagedBy(mgr).
				For(&c5c3v1alpha1.ControlPlane{}).
				Owns(&mariadbv1alpha1.MariaDB{}).
				Owns(&keystonev1alpha1.Keystone{}).
				Owns(&orcv1alpha1.ApplicationCredential{}).
				Owns(&orcv1alpha1.Service{}).
				Owns(&orcv1alpha1.Endpoint{}).
				Owns(memcached).
				// Mirror the identity-backend watch SetupWithManager registers, so
				// a backend event wakes the ControlPlane exactly as in production.
				Watches(&keystonev1alpha1.KeystoneIdentityBackend{}, handler.EnqueueRequestsFromMapFunc(
					r.identityBackendToControlPlaneMapper,
				)).
				Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(
					secretToControlPlaneMapper(mgr.GetClient()),
				)).
				WithOptions(controller.Options{SkipNameValidation: ptr.To(true)}).
				Complete(r)
		},
	)
}

// integrationManagedControlPlane returns a valid managed-mode ControlPlane CR:
// database and cache reference managed clusters (clusterRef set), so the
// reconciler projects a MariaDB and a Memcached child. The spec satisfies the
// validating webhook (openStackRelease pattern, database/cache XOR,
// passwordSecretRef.name required); region / cloudCredentialsRef.secretName /
// applicationCredential.restricted / rotation.mode are left for the defaulting
// webhook to fill.
func integrationManagedControlPlane(name, namespace string) *c5c3v1alpha1.ControlPlane {
	return &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: c5c3v1alpha1.ControlPlaneSpec{
			OpenStackRelease: "2025.2",
			Infrastructure: &c5c3v1alpha1.InfrastructureSpec{
				Database: commonv1.DatabaseSpec{
					ClusterRef: &corev1.LocalObjectReference{Name: "openstack-db"},
					Database:   "keystone",
					SecretRef:  commonv1.SecretRefSpec{Name: "keystone-db"},
				},
				Cache: commonv1.CacheSpec{
					ClusterRef: &corev1.LocalObjectReference{Name: "openstack-memcached"},
					Backend:    "dogpile.cache.pymemcache",
					Replicas:   3,
				},
			},
			Services: c5c3v1alpha1.ServicesSpec{
				Keystone: &c5c3v1alpha1.ServiceKeystoneSpec{
					Replicas: ptr.To(int32(3)),
				},
			},
			// One global oslo.policy override so the test can assert the reconciler
			// merges it into the projected Keystone CR's PolicyOverrides.
			GlobalPolicyOverrides: &commonv1.PolicySpec{
				Rules: map[string]string{"identity:list_users": "role:admin"},
			},
			KORC: c5c3v1alpha1.KORCSpec{
				AdminCredential: c5c3v1alpha1.AdminCredentialSpec{
					CloudCredentialsRef: c5c3v1alpha1.CloudCredentialsRef{
						CloudName: "admin",
					},
					// DECISION the spec-level ref is kept at the canonical
					// brownfield default "keystone-admin" (== DefaultAdminPasswordSecretName)
					// rather than renamed to adminPasswordSecretName(cp). In managed mode
					// effectiveAdminPasswordSecretRef ALWAYS overrides to adminPasswordSecretName(cp)
					// regardless of this value, so keeping it distinct makes the projected-child
					// admin-ref assertions below genuinely prove the override (the projected name
					// differs from this spec ref) — exactly mirroring the DB-credential fixture,
					// whose spec ref stays "keystone-db" != dbCredentialSecretName(cp). The
					// pre-created cleartext Secret is named adminPasswordSecretName(cp) (the name
					// readAdminPassword resolves via the effective ref). Reviewer: please verify.
					PasswordSecretRef: commonv1.SecretRefSpec{Name: "keystone-admin", Key: "password"},
				},
			},
		},
	}
}

// integrationMinimalControlPlane returns a ControlPlane with ONLY the two
// genuinely-required user inputs set — openStackRelease and the keystone service
// block — and spec.infrastructure / spec.korc OMITTED (zero structs). The
// defaulting webhook must therefore construct the database, cache, and
// admin-credential blocks from its well-known defaults; TestIntegration_MinimalManagedToReady
// asserts it does and that the CR still converges to Ready=True.
func integrationMinimalControlPlane(name, namespace string) *c5c3v1alpha1.ControlPlane {
	return &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: c5c3v1alpha1.ControlPlaneSpec{
			OpenStackRelease: "2025.2",
			Services: c5c3v1alpha1.ServicesSpec{
				Keystone: &c5c3v1alpha1.ServiceKeystoneSpec{
					Replicas: ptr.To(int32(1)),
				},
			},
		},
	}
}

// ensureReadyClusterSecretStore creates the cluster-scoped OpenBao-backed
// ClusterSecretStore the DB-credential, admin-password and admin-credential
// sub-reconcilers gate on (#476) and marks it Ready. It is idempotent across the
// shared envtest cluster: the store is cluster-scoped, so a second test reuses
// the existing object and only refreshes its Ready status. Call it before
// creating a ControlPlane so the first reconcile sees the store Ready and the
// credential gates open; without it the chain stalls at DBCredentialsReady=False
// with reason SecretStoreNotReady. Mirrors the keystone operator's helper.
func ensureReadyClusterSecretStore(t testing.TB, ctx context.Context, c client.Client) {
	t.Helper()
	g := NewGomegaWithT(t)

	store := &esov1.ClusterSecretStore{
		ObjectMeta: metav1.ObjectMeta{Name: openBaoClusterStoreName},
	}
	err := c.Get(ctx, client.ObjectKeyFromObject(store), store)
	if apierrors.IsNotFound(err) {
		g.Expect(c.Create(ctx, store)).To(Succeed(), "create ClusterSecretStore")
	} else {
		g.Expect(err).NotTo(HaveOccurred(), "get ClusterSecretStore")
	}

	store.Status = esov1.SecretStoreStatus{
		Conditions: []esov1.SecretStoreStatusCondition{
			{Type: esov1.SecretStoreReady, Status: corev1.ConditionTrue},
		},
	}
	g.Expect(c.Status().Update(ctx, store)).To(Succeed(), "update ClusterSecretStore status")
}

// waitForControlPlaneCondition polls the ControlPlane CR until the named
// condition reaches the expected status, or the timeout is reached. Returns the
// observed condition.
func waitForControlPlaneCondition(
	t testing.TB, ctx context.Context, c client.Client,
	key types.NamespacedName, condType string, expected metav1.ConditionStatus, timeout time.Duration,
) *metav1.Condition {
	t.Helper()
	g := NewGomegaWithT(t)

	var cond *metav1.Condition
	g.Eventually(func() metav1.ConditionStatus {
		cp := &c5c3v1alpha1.ControlPlane{}
		if err := c.Get(ctx, key, cp); err != nil {
			return ""
		}
		cond = meta.FindStatusCondition(cp.Status.Conditions, condType)
		if cond == nil {
			return ""
		}
		return cond.Status
	}, timeout, itPollInterval).Should(Equal(expected),
		fmt.Sprintf("ControlPlane condition %s should reach %s", condType, expected))

	return cond
}

// simulateMariaDBReadyWhenPresent waits for the projected MariaDB child to be
// created by reconcileInfrastructure, then sets its status Ready=True via the
// shared simulator so InfrastructureReady can advance.
func simulateMariaDBReadyWhenPresent(t testing.TB, ctx context.Context, c client.Client, key client.ObjectKey) {
	t.Helper()
	g := NewGomegaWithT(t)

	g.Eventually(func() error {
		return c.Get(ctx, key, &mariadbv1alpha1.MariaDB{})
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "MariaDB child should be created")
	g.Expect(simulators.SimulateMariaDBReady(ctx, c, key, 3)).To(Succeed(), "simulate MariaDB ready")
}

// simulateMemcachedReadyWhenPresent waits for the projected (unstructured)
// Memcached child, then sets its status Ready=True via the shared simulator
// (which targets the same memcachedGVK the reconciler uses).
func simulateMemcachedReadyWhenPresent(t testing.TB, ctx context.Context, c client.Client, key client.ObjectKey) {
	t.Helper()
	g := NewGomegaWithT(t)

	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(memcachedGVK)
	g.Eventually(func() error {
		return c.Get(ctx, key, u)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "Memcached child should be created")
	g.Expect(simulators.SimulateMemcachedReady(ctx, c, key, 3, []string{"openstack-memcached:11211"})).
		To(Succeed(), "simulate Memcached ready")
}

// simulateKeystoneReadyWhenPresent waits for the projected Keystone child, then
// sets its Ready condition True inline (there is no Keystone simulator — the
// reconcileKeystone gate mirrors the child Ready condition).
func simulateKeystoneReadyWhenPresent(t testing.TB, ctx context.Context, c client.Client, key client.ObjectKey) {
	t.Helper()
	g := NewGomegaWithT(t)

	ks := &keystonev1alpha1.Keystone{}
	g.Eventually(func() error {
		return c.Get(ctx, key, ks)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "Keystone child should be created")

	meta.SetStatusCondition(&ks.Status.Conditions, metav1.Condition{
		Type:    "Ready",
		Status:  metav1.ConditionTrue,
		Reason:  "KeystoneReady",
		Message: "simulated ready",
	})
	g.Expect(c.Status().Update(ctx, ks)).To(Succeed(), "set Keystone Ready=True")
}

// simulateHorizonReadyWhenPresent waits for the projected Horizon child, then
// sets its aggregate Ready condition True so reconcileHorizon's mirror flips
// HorizonReady (there is no horizon-operator running in envtest).
func simulateHorizonReadyWhenPresent(t testing.TB, ctx context.Context, c client.Client, key client.ObjectKey) {
	t.Helper()
	g := NewGomegaWithT(t)

	hz := &horizonv1alpha1.Horizon{}
	g.Eventually(func() error {
		return c.Get(ctx, key, hz)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "Horizon child should be created")

	meta.SetStatusCondition(&hz.Status.Conditions, metav1.Condition{
		Type:    "Ready",
		Status:  metav1.ConditionTrue,
		Reason:  "AllReady",
		Message: "simulated ready",
	})
	g.Expect(c.Status().Update(ctx, hz)).To(Succeed(), "set Horizon Ready=True")
}

// simulateApplicationCredentialAvailableWhenPresent waits for the owned K-ORC
// ApplicationCredential, then sets its Available condition True and a status.id
// inline (there is no K-ORC simulator — reconcileKORC gates KORCReady on
// orcv1alpha1.IsAvailable and reflects status.id into the ControlPlane status).
func simulateApplicationCredentialAvailableWhenPresent(t testing.TB, ctx context.Context, c client.Client, key client.ObjectKey) {
	t.Helper()
	g := NewGomegaWithT(t)

	ac := &orcv1alpha1.ApplicationCredential{}
	g.Eventually(func() error {
		return c.Get(ctx, key, ac)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "ApplicationCredential should be minted")

	ac.Status.ID = ptr.To("ac-id-integration")
	meta.SetStatusCondition(&ac.Status.Conditions, metav1.Condition{
		Type:    orcv1alpha1.ConditionAvailable,
		Status:  metav1.ConditionTrue,
		Reason:  orcv1alpha1.ConditionReasonSuccess,
		Message: "simulated available",
	})
	g.Expect(c.Status().Update(ctx, ac)).To(Succeed(), "set ApplicationCredential Available=True")
}

// simulatePushSecretSyncedWhenPresent waits for the named PushSecret to be
// created, then sets its Ready condition True via the shared simulator. There is
// no ESO controller in envtest, so reconcileAdminCredential — which gates
// AdminCredentialReady on the admin app-credential PushSecret actually syncing to
// OpenBao — would otherwise wait forever. SimulatePushSecretSynced
// returns an error until the PushSecret exists, so polling it doubles as the
// "WhenPresent" wait without needing the esov1alpha1 type here.
func simulatePushSecretSyncedWhenPresent(t testing.TB, ctx context.Context, c client.Client, key client.ObjectKey) {
	t.Helper()
	g := NewGomegaWithT(t)

	g.Eventually(func() error {
		return simulators.SimulatePushSecretSynced(ctx, c, key)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
		"admin app-credential PushSecret should be created and synced")
}

// simulateCloudsYamlMaterializedWhenPresent performs the ESO round-trip envtest has
// no controller for: it reads the operator-owned app-credential Secret the PushSecret
// mirrors to OpenBao and writes its assembled clouds.yaml into the k-orc-clouds-yaml
// Secret K-ORC authenticates with. reconcileAdminCredential now byte-compares the
// materialized Secret against the freshly assembled clouds.yaml before flipping
// AdminCredentialReady True (closing the post-re-mint stale-credential window), so
// without this materialisation the gate would wait forever.
func simulateCloudsYamlMaterializedWhenPresent(t testing.TB, ctx context.Context, c client.Client, cp *c5c3v1alpha1.ControlPlane) {
	t.Helper()
	g := NewGomegaWithT(t)

	name := cp.Spec.KORC.AdminCredential.CloudCredentialsRef.SecretName
	if name == "" {
		name = korcCloudsYamlSecretName
	}

	// Wait for the operator-owned Secret to hold the MINTED application-credential
	// clouds.yaml, not the password-based bootstrap seed: reconcileKORC creates the
	// PushSecret (and seeds the password clouds.yaml) before reconcileAdminCredential
	// overwrites it with the app-credential document, so copying too early would
	// materialise the wrong bytes and the byte-compare gate would never match.
	src := &corev1.Secret{}
	g.Eventually(func() error {
		if err := c.Get(ctx, client.ObjectKey{Namespace: childNamespace(cp), Name: adminAppCredentialSecretName(cp)}, src); err != nil {
			return err
		}
		if !strings.Contains(string(src.Data[appCredCloudsYAMLKey]), "application_credential_id") {
			return fmt.Errorf("operator-owned Secret still holds the password seed, not the minted app-credential clouds.yaml")
		}
		return nil
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
		"operator must assemble the app-credential clouds.yaml before ESO can materialise it back")

	materialized := &corev1.Secret{}
	err := c.Get(ctx, client.ObjectKey{Namespace: childNamespace(cp), Name: name}, materialized)
	switch {
	case apierrors.IsNotFound(err):
		materialized = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: childNamespace(cp)},
			Data:       map[string][]byte{appCredCloudsYAMLKey: src.Data[appCredCloudsYAMLKey]},
		}
		g.Expect(c.Create(ctx, materialized)).To(Succeed(), "materialize the k-orc clouds.yaml Secret")
	case err == nil:
		if materialized.Data == nil {
			materialized.Data = map[string][]byte{}
		}
		materialized.Data[appCredCloudsYAMLKey] = src.Data[appCredCloudsYAMLKey]
		g.Expect(c.Update(ctx, materialized)).To(Succeed(), "refresh the materialized k-orc clouds.yaml Secret")
	default:
		g.Expect(err).NotTo(HaveOccurred(), "get materialized k-orc clouds.yaml Secret")
	}
}

// simulateCatalogServiceEndpointAvailableWhenPresent waits for the owned K-ORC
// identity Service and Endpoint, then sets their Available condition True inline.
// reconcileCatalog now gates CatalogReady on both child CRs reporting Available
// (registering them is not enough — the catalog entry must actually land in
// Keystone), and there is no K-ORC controller in envtest to mark them Available.
func simulateCatalogServiceEndpointAvailableWhenPresent(t testing.TB, ctx context.Context, c client.Client, cp *c5c3v1alpha1.ControlPlane) {
	t.Helper()
	g := NewGomegaWithT(t)
	ns := childNamespace(cp)

	svc := &orcv1alpha1.Service{}
	g.Eventually(func() error {
		return c.Get(ctx, client.ObjectKey{Namespace: ns, Name: keystoneServiceName(cp)}, svc)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "identity Service should be registered")
	meta.SetStatusCondition(&svc.Status.Conditions, metav1.Condition{
		Type:   orcv1alpha1.ConditionAvailable,
		Status: metav1.ConditionTrue,
		Reason: orcv1alpha1.ConditionReasonSuccess,
		// reconcileCatalog gates on korcAvailableUpToDate, which requires the
		// Available condition's ObservedGeneration to match the object's
		// generation — mirror what the real K-ORC actuator stamps so the gate
		// flips True (the in-cluster apiserver assigns Generation>=1 on create).
		ObservedGeneration: svc.Generation,
		Message:            "simulated available",
	})
	g.Expect(c.Status().Update(ctx, svc)).To(Succeed(), "set identity Service Available=True")

	ep := &orcv1alpha1.Endpoint{}
	g.Eventually(func() error {
		return c.Get(ctx, client.ObjectKey{Namespace: ns, Name: keystoneEndpointName(cp)}, ep)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "identity Endpoint should be registered")
	meta.SetStatusCondition(&ep.Status.Conditions, metav1.Condition{
		Type:               orcv1alpha1.ConditionAvailable,
		Status:             metav1.ConditionTrue,
		Reason:             orcv1alpha1.ConditionReasonSuccess,
		ObservedGeneration: ep.Generation,
		Message:            "simulated available",
	})
	g.Expect(c.Status().Update(ctx, ep)).To(Succeed(), "set identity Endpoint Available=True")
}

// simulateExternalCatalogImportsAvailableWhenPresent waits for the four UNMANAGED
// catalog import CRs an External-mode ControlPlane projects — the identity Service
// plus one Endpoint per interface — and resolves each one inline: Available=True
// with a matching ObservedGeneration (korcAvailableUpToDate refuses a stale
// condition) and a resolved OpenStack id. That is what K-ORC stamps once an import
// matches a live catalog entry, and there is no K-ORC controller in envtest.
func simulateExternalCatalogImportsAvailableWhenPresent(
	t testing.TB, ctx context.Context, c client.Client, cp *c5c3v1alpha1.ControlPlane,
) {
	t.Helper()
	g := NewGomegaWithT(t)
	ns := childNamespace(cp)

	svc := &orcv1alpha1.Service{}
	g.Eventually(func() error {
		return c.Get(ctx, client.ObjectKey{Namespace: ns, Name: keystoneServiceName(cp)}, svc)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "the identity Service import should be projected")
	meta.SetStatusCondition(&svc.Status.Conditions, metav1.Condition{
		Type:               orcv1alpha1.ConditionAvailable,
		Status:             metav1.ConditionTrue,
		Reason:             orcv1alpha1.ConditionReasonSuccess,
		ObservedGeneration: svc.Generation,
		Message:            "simulated import resolved",
	})
	svc.Status.ID = ptr.To("simulated-identity-service-id")
	g.Expect(c.Status().Update(ctx, svc)).To(Succeed(), "resolve the identity Service import")

	for _, iface := range externalCatalogInterfaces {
		ep := &orcv1alpha1.Endpoint{}
		g.Eventually(func() error {
			return c.Get(ctx, client.ObjectKey{Namespace: ns, Name: keystoneEndpointImportName(cp, iface)}, ep)
		}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "the %q Endpoint import should be projected", iface)
		meta.SetStatusCondition(&ep.Status.Conditions, metav1.Condition{
			Type:               orcv1alpha1.ConditionAvailable,
			Status:             metav1.ConditionTrue,
			Reason:             orcv1alpha1.ConditionReasonSuccess,
			ObservedGeneration: ep.Generation,
			Message:            "simulated import resolved",
		})
		ep.Status.ID = ptr.To("simulated-endpoint-id-" + string(iface))
		g.Expect(c.Status().Update(ctx, ep)).To(Succeed(), "resolve the %q Endpoint import", iface)
	}
}

// simulateAdminPasswordExternalSecretSyncWhenPresent waits for the operator-created
// per-ControlPlane admin-password ExternalSecret (named adminPasswordSecretName(cp)
// in childNamespace(cp)), asserts it reads this CR's keystone-NAME-scoped OpenBao
// path (adminPasswordRemoteKeyFor) and is controller-owned by the ControlPlane, then
// simulates the ESO sync. SimulateExternalSecretSync patches ONLY the ExternalSecret
// .status — it never creates the backing Secret — so the pre-created plain Secret
// (named adminPasswordSecretName(cp)) remains the cleartext source readAdminPassword
// reads. This is the admin-password analog of the inline DB-credential ExternalSecret
// sync, gating the Keystone projection on AdminPasswordReady.
func simulateAdminPasswordExternalSecretSyncWhenPresent(
	t testing.TB, ctx context.Context, c client.Client, cp *c5c3v1alpha1.ControlPlane,
) {
	t.Helper()
	g := NewGomegaWithT(t)

	es := &esov1.ExternalSecret{}
	g.Eventually(func() error {
		return c.Get(ctx, client.ObjectKey{Namespace: childNamespace(cp), Name: adminPasswordSecretName(cp)}, es)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
		"operator must create the per-CP admin password ExternalSecret")
	g.Expect(es.Spec.Data).NotTo(BeEmpty(), "admin password ExternalSecret must declare Data entries")
	g.Expect(es.Spec.Data[0].RemoteRef.Key).To(Equal(adminPasswordRemoteKeyFor(cp)),
		"admin password ExternalSecret must read this CR's keystone-name-scoped OpenBao path")
	owner := metav1.GetControllerOf(es)
	g.Expect(owner).NotTo(BeNil(), "admin password ExternalSecret must be controller-owned by the ControlPlane")
	g.Expect(owner.Kind).To(Equal("ControlPlane"))
	g.Expect(owner.Name).To(Equal(cp.Name))
	g.Expect(simulators.SimulateExternalSecretSync(ctx, c,
		client.ObjectKey{Namespace: childNamespace(cp), Name: adminPasswordSecretName(cp)})).
		To(Succeed(), "simulate per-CP admin password ExternalSecret sync")
}

// TestIntegration_FullReconcile_ManagedToReady drives a managed-mode ControlPlane
// through every sub-reconciler to the aggregate Ready=True, simulating each
// external dependency's readiness in dependency order. It is the single primary
// end-to-end test for the ControlPlane reconciler.
func TestIntegration_FullReconcile_ManagedToReady(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupControlPlaneEnvTest(t)

	// The OpenBao-backed ClusterSecretStore must be Ready before the chain
	// reaches the credential gates (#476).
	ensureReadyClusterSecretStore(t, ctx, c)

	// Isolated test namespace per run (namespace-per-test with GenerateName).
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-controlplane-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed(), "create test namespace")

	// Create the ControlPlane CR (the defaulting webhook fills region etc.).
	// Horizon is enabled HERE (not in the shared fixture) so only this
	// full-chain test — which simulates the Horizon child in Phase 2.5 —
	// carries the second service; the gate-focused tests reusing the fixture
	// would otherwise wedge at the unsimulated Horizon step before KORC.
	cp := integrationManagedControlPlane("cp", ns.Name)
	cp.Spec.Services.Horizon = &c5c3v1alpha1.ServiceHorizonSpec{}

	// Admin password Secret the KORC sub-reconciler hashes to drive the mint. In
	// managed mode readAdminPassword resolves the operator-owned per-CP name
	// (effectiveAdminPasswordSecretRef -> adminPasswordSecretName(cp)), so pre-create
	// the cleartext Secret under that name. ESO would own this Secret in production;
	// envtest has no ESO, and SimulateExternalSecretSync patches only the ES status,
	// so this plain Secret remains the cleartext source readAdminPassword reads
	adminSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: adminPasswordSecretName(cp), Namespace: ns.Name},
		Data:       map[string][]byte{"password": []byte("super-secret-admin-password")},
	}
	g.Expect(c.Create(ctx, adminSecret)).To(Succeed(), "create admin password Secret")

	g.Expect(c.Create(ctx, cp)).To(Succeed(), "create ControlPlane CR")
	cpKey := types.NamespacedName{Name: cp.Name, Namespace: ns.Name}

	// --- Phase 1: Infrastructure (MariaDB + Memcached). ---
	simulateMariaDBReadyWhenPresent(t, ctx, c, client.ObjectKey{Name: "openstack-db", Namespace: ns.Name})
	simulateMemcachedReadyWhenPresent(t, ctx, c, client.ObjectKey{Name: "openstack-memcached", Namespace: ns.Name})
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeInfrastructureReady, metav1.ConditionTrue, itEventuallyTimeout)

	// gate Keystone on the per-CP DB credential ExternalSecret. DECISION:
	// harness sync-simulation lives here to keep this level bisectable (full suite
	// green); the projected-secretRef assertion is made below in the Keystone block
	// Reviewer: please verify.
	// Managed mode defaults to Dynamic (engine-issued) credentials: the operator
	// projects a per-CP VaultDynamicSecret generator plus an ExternalSecret that
	// draws from it via dataFrom.sourceRef.generatorRef (no static Data refs).
	dbCredES := &esov1.ExternalSecret{}
	g.Eventually(func() error {
		return c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: dbCredentialSecretName(cp)}, dbCredES)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "operator must create the per-CP DB credential ExternalSecret")
	g.Expect(dbCredES.Spec.DataFrom).NotTo(BeEmpty(), "Dynamic DB credential ExternalSecret must declare a generatorRef")
	g.Expect(dbCredES.Spec.DataFrom[0].SourceRef).NotTo(BeNil())
	g.Expect(dbCredES.Spec.DataFrom[0].SourceRef.GeneratorRef).NotTo(BeNil())
	g.Expect(dbCredES.Spec.DataFrom[0].SourceRef.GeneratorRef.Kind).To(Equal("VaultDynamicSecret"))
	g.Expect(dbCredES.Spec.Data).To(BeEmpty(), "Dynamic DB credential ExternalSecret carries no static Data refs")
	// The per-CP VaultDynamicSecret generator reads this tenant's creds path.
	vds := &esgenv1alpha1.VaultDynamicSecret{}
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: dbCredentialSecretName(cp)}, vds)).
		To(Succeed(), "operator must create the per-CP VaultDynamicSecret generator")
	g.Expect(vds.Spec.Path).To(Equal(dbDynamicCredsPathFor(cp)))
	g.Expect(metav1.GetControllerOf(vds)).NotTo(BeNil(), "VaultDynamicSecret must be owned by the ControlPlane")
	g.Expect(simulators.SimulateExternalSecretSync(ctx, c,
		client.ObjectKey{Namespace: ns.Name, Name: dbCredentialSecretName(cp)})).
		To(Succeed(), "simulate per-CP DB credential ExternalSecret sync")
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeDBCredentialsReady, metav1.ConditionTrue, itEventuallyTimeout)

	// --- Phase 1.5: AdminPassword (between Infrastructure/DBCredentials and Keystone).
	// The keystone-operator's SecretsReady gate needs the admin Secret backed by a
	// Ready ExternalSecret, so reconcileAdminPassword must create+ready the per-CP
	// admin-password ExternalSecret before the Keystone child is projected. Assert the
	// operator-rendered shape (keystone-name-scoped OpenBao path + controller owner-ref),
	// simulate the ESO sync (status-only — the renamed plain Secret above stays the
	// cleartext source), then AdminPasswordReady flips True. ---
	simulateAdminPasswordExternalSecretSyncWhenPresent(t, ctx, c, cp)
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeAdminPasswordReady, metav1.ConditionTrue, itEventuallyTimeout)

	// --- Phase 2: Keystone child. ---
	simulateKeystoneReadyWhenPresent(t, ctx, c, client.ObjectKey{Name: keystoneName(cp), Namespace: ns.Name})
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeKeystoneReady, metav1.ConditionTrue, itEventuallyTimeout)

	// --- Phase 2.5: Horizon child. Projection is gated on KeystoneReady, so
	// the child only appears now; assert its operator-derived spec before
	// simulating readiness. ---
	horizonKey := client.ObjectKey{Name: horizonName(cp), Namespace: ns.Name}
	projectedHorizon := &horizonv1alpha1.Horizon{}
	g.Eventually(func() error {
		return c.Get(ctx, horizonKey, projectedHorizon)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "Horizon child should be projected once KeystoneReady")
	g.Expect(projectedHorizon.Spec.KeystoneEndpoint).To(
		Equal(fmt.Sprintf("http://%s.%s.svc:5000/v3", keystoneName(cp), ns.Name)),
		"keystoneEndpoint must be the cluster-local convention URL (same as the K-ORC auth_url)",
	)
	g.Expect(projectedHorizon.Spec.Cache.ClusterRef).NotTo(BeNil())
	g.Expect(projectedHorizon.Spec.Cache.ClusterRef.Name).To(Equal("openstack-memcached"))
	simulateHorizonReadyWhenPresent(t, ctx, c, horizonKey)
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeHorizonReady, metav1.ConditionTrue, itEventuallyTimeout)

	// --- Phase 3: K-ORC admin ApplicationCredential. ---
	simulateApplicationCredentialAvailableWhenPresent(t, ctx, c,
		client.ObjectKey{Name: adminAppCredentialName(cp), Namespace: ns.Name})
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeKORCReady, metav1.ConditionTrue, itEventuallyTimeout)

	// The K-ORC clouds.yaml ExternalSecret AdminCredentialReady gates on is now
	// CREATED BY THE OPERATOR (reconcileKORC -> ensureKORCCloudsYAMLExternalSecret),
	// co-located in the ControlPlane namespace because K-ORC resolves
	// CloudCredentialsRef in the resource's own namespace — it is no longer seeded by
	// write-bootstrap-secrets.sh. reconcileKORC creates it before
	// the AC-Available gate, so it exists by the time KORCReady flips True (above).
	// Assert its operator-rendered shape, then simulate the ESO sync (no ESO
	// controller in envtest) so WaitForExternalSecret reports Ready and Phase 4 can
	// progress.
	cloudsYamlES := &esov1.ExternalSecret{}
	g.Eventually(func() error {
		return c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: korcCloudsYamlSecretName}, cloudsYamlES)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "operator must create the k-orc clouds.yaml ExternalSecret")
	g.Expect(cloudsYamlES.Spec.Data).To(HaveLen(1), "clouds.yaml ExternalSecret must declare exactly one Data entry")
	g.Expect(cloudsYamlES.Spec.Data[0].SecretKey).To(Equal(appCredCloudsYAMLKey))
	g.Expect(cloudsYamlES.Spec.Data[0].RemoteRef.Key).To(Equal(adminAppCredentialRemoteKeyFor(cp)),
		"clouds.yaml ExternalSecret must read the per-CR OpenBao path")
	g.Expect(cloudsYamlES.Spec.Data[0].RemoteRef.Property).To(Equal(appCredCloudsYAMLKey))
	owner := metav1.GetControllerOf(cloudsYamlES)
	g.Expect(owner).NotTo(BeNil(), "clouds.yaml ExternalSecret must be controller-owned by the ControlPlane")
	g.Expect(owner.Kind).To(Equal("ControlPlane"))
	g.Expect(owner.Name).To(Equal(cp.Name))
	g.Expect(simulators.SimulateExternalSecretSync(ctx, c,
		client.ObjectKey{Namespace: ns.Name, Name: korcCloudsYamlSecretName})).
		To(Succeed(), "simulate k-orc clouds.yaml ExternalSecret sync")

	// --- Phase 4: AdminCredential push. Gated on the clouds.yaml ES (synced
	// above), on the admin app-credential PushSecret syncing to OpenBao, AND on the
	// materialized k-orc-clouds-yaml Secret matching the assembled credential. The
	// PushSecret sync is status-gated and the materialisation is the ESO round-trip,
	// so simulate both — otherwise AdminCredentialReady never flips in envtest. ---
	simulatePushSecretSyncedWhenPresent(t, ctx, c,
		client.ObjectKey{Name: adminAppCredentialPushSecretName(cp), Namespace: childNamespace(cp)})
	simulateCloudsYamlMaterializedWhenPresent(t, ctx, c, cp)
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeAdminCredentialReady, metav1.ConditionTrue, itEventuallyTimeout)

	// --- Phase 5: Catalog (Service + Endpoint). Gated on both child CRs reporting
	// Available, so simulate the K-ORC actuator marking them Available. ---
	simulateCatalogServiceEndpointAvailableWhenPresent(t, ctx, c, cp)
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeCatalogReady, metav1.ConditionTrue, itEventuallyTimeout)

	// --- Aggregate: Ready=True. ---
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeReady, metav1.ConditionTrue, itEventuallyTimeout)

	// Final assertions on the converged CR.
	final := &c5c3v1alpha1.ControlPlane{}
	g.Expect(c.Get(ctx, cpKey, final)).To(Succeed(), "get converged ControlPlane")

	for _, condType := range []string{
		conditionTypeInfrastructureReady,
		conditionTypeDBCredentialsReady,
		conditionTypeKeystoneReady,
		conditionTypeHorizonReady,
		conditionTypeKORCReady,
		conditionTypeAdminCredentialReady,
		conditionTypeCatalogReady,
		conditionTypeReady,
	} {
		cond := meta.FindStatusCondition(final.Status.Conditions, condType)
		g.Expect(cond).NotTo(BeNil(), "condition %s should exist", condType)
		g.Expect(cond.Status).To(Equal(metav1.ConditionTrue), "condition %s should be True", condType)
	}

	// Ready reports the aggregate reason.
	readyCond := meta.FindStatusCondition(final.Status.Conditions, conditionTypeReady)
	g.Expect(readyCond.Reason).To(Equal("AllReady"), "Ready reason should be AllReady")

	// status.observedGeneration tracks the reconciled generation.
	g.Expect(final.Status.ObservedGeneration).To(Equal(final.Generation),
		"status.observedGeneration should match the CR generation")

	// status.services reports one entry per configured service, both ready.
	g.Expect(final.Status.Services).To(HaveLen(2), "two services are configured (keystone, horizon)")
	g.Expect(final.Status.Services[0].Name).To(Equal("keystone"))
	g.Expect(final.Status.Services[0].Ready).To(BeTrue())
	g.Expect(final.Status.Services[1].Name).To(Equal("horizon"))
	g.Expect(final.Status.Services[1].Ready).To(BeTrue())

	// Every condition records the generation it was observed against.
	for _, cond := range final.Status.Conditions {
		g.Expect(cond.ObservedGeneration).To(Equal(final.Generation),
			"condition %s ObservedGeneration should match CR generation", cond.Type)
	}

	// The reflected admin application-credential status mirrors the simulated AC.
	g.Expect(final.Status.AdminApplicationCredential).NotTo(BeNil(),
		"status.adminApplicationCredential should be populated")
	g.Expect(final.Status.AdminApplicationCredential.ID).To(Equal("ac-id-integration"))
	catalogCond := meta.FindStatusCondition(final.Status.Conditions, conditionTypeCatalogReady)
	g.Expect(catalogCond).NotTo(BeNil(), "CatalogReady condition should exist")
	g.Expect(catalogCond.Status).To(Equal(metav1.ConditionTrue),
		"CatalogReady condition should be True once the catalog is registered")

	// --- Intermediate projected specs (TE7b). Asserting only the final
	// aggregate condition would not catch a projection regression, so verify the
	// shape of each projected child the chain produced. ---

	// Keystone CR: image tag derived from openStackRelease, clusterRefs wired to
	// the infra CRs, and the global oslo.policy override merged in.
	ks := &keystonev1alpha1.Keystone{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: keystoneName(final), Namespace: ns.Name}, ks)).
		To(Succeed(), "get projected Keystone CR")
	g.Expect(ks.Spec.Image.Repository).To(Equal(defaultKeystoneRepository))
	g.Expect(ks.Spec.Image.Tag).To(Equal("2025.2"), "Keystone image tag must derive from openStackRelease")
	g.Expect(ks.Spec.Database.ClusterRef).NotTo(BeNil(), "Keystone database clusterRef must be wired")
	g.Expect(ks.Spec.Database.ClusterRef.Name).To(Equal("openstack-db"))
	g.Expect(ks.Spec.Database.SecretRef.Name).To(Equal(dbCredentialSecretName(final)),
		"managed Keystone DB secretRef must point at the operator-owned per-CP DB-credential Secret")
	g.Expect(ks.Spec.Database.SecretRef.Key).To(Equal("password"))
	// Admin-ref analog in managed mode reconcileKeystone overrides
	// the projected child's bootstrap admin-password ref via effectiveAdminPasswordSecretRef
	// to the operator-owned per-CP Secret. Because the spec ref stays "keystone-admin"
	// (see the fixture DECISION), this differs from the spec ref and genuinely proves
	// the override fired.
	g.Expect(ks.Spec.Bootstrap.AdminPasswordSecretRef.Name).To(Equal(adminPasswordSecretName(final)),
		"managed Keystone admin-password secretRef must point at the operator-owned per-CP admin Secret")
	g.Expect(ks.Spec.Bootstrap.AdminPasswordSecretRef.Key).To(Equal("password"))
	g.Expect(ks.Spec.Cache.ClusterRef).NotTo(BeNil(), "Keystone cache clusterRef must be wired")
	g.Expect(ks.Spec.Cache.ClusterRef.Name).To(Equal("openstack-memcached"))
	g.Expect(ks.Spec.PolicyOverrides).NotTo(BeNil(), "merged policy must be projected")
	g.Expect(ks.Spec.PolicyOverrides.Rules).To(HaveKeyWithValue("identity:list_users", "role:admin"),
		"global oslo.policy override must be merged into the projected Keystone CR")

	// ApplicationCredential CR: the restricted/Unrestricted inversion. restricted
	// defaults to true (least privilege), so K-ORC's Unrestricted must be false.
	ac := &orcv1alpha1.ApplicationCredential{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: adminAppCredentialName(final), Namespace: ns.Name}, ac)).
		To(Succeed(), "get projected ApplicationCredential CR")
	g.Expect(ac.Spec.Resource).NotTo(BeNil())
	g.Expect(ac.Spec.Resource.Unrestricted).NotTo(BeNil())
	g.Expect(*ac.Spec.Resource.Unrestricted).To(BeFalse(),
		"restricted:true (default) MUST project to K-ORC Unrestricted=false (critical inversion)")
	// The AC mints via the operator-owned password-cloud (so a delete+recreate
	// re-mint can always re-authenticate), NOT k-orc-clouds-yaml.
	g.Expect(ac.Spec.CloudCredentialsRef.SecretName).To(Equal(adminPasswordCloudSecretName(final)))

	// Catalog: identity Service + public Endpoint shape.
	svc := &orcv1alpha1.Service{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: keystoneServiceName(final), Namespace: ns.Name}, svc)).
		To(Succeed(), "get projected identity Service CR")
	g.Expect(svc.Spec.Resource).NotTo(BeNil())
	g.Expect(svc.Spec.Resource.Type).To(Equal("identity"), "Service type must be identity")
	// The catalog keeps using k-orc-clouds-yaml (only the AC moves to the
	// password-cloud); this locks in that split.
	g.Expect(svc.Spec.CloudCredentialsRef.SecretName).To(Equal(korcCloudsYamlSecretName))

	ep := &orcv1alpha1.Endpoint{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: keystoneEndpointName(final), Namespace: ns.Name}, ep)).
		To(Succeed(), "get projected identity Endpoint CR")
	g.Expect(ep.Spec.Resource).NotTo(BeNil())
	g.Expect(ep.Spec.Resource.Interface).To(Equal("public"), "Endpoint interface must be public")
	g.Expect(string(ep.Spec.Resource.ServiceRef)).To(Equal(keystoneServiceName(final)),
		"Endpoint serviceRef must reference the identity Service CR")
	g.Expect(ep.Spec.Resource.URL).NotTo(BeEmpty(), "Endpoint URL must be derived")

	// --- Per-CR OpenBao RemoteKey lock. ---
	//
	// On the single-ControlPlane path the admin app-credential PushSecret must
	// already mirror to the per-CR OpenBao path scoped by the CR's Namespace and
	// Name (adminAppCredentialRemoteKeyFor), NOT the legacy flat
	// openstack/keystone/admin/app-credential. Locking this here on the baseline
	// end-to-end test guards the single-CP rendering of the path the multi-CP test
	// asserts is distinct between CRs.
	adminPS := &esov1alpha1.PushSecret{}
	g.Expect(c.Get(ctx, types.NamespacedName{
		Namespace: childNamespace(final), Name: adminAppCredentialPushSecretName(final),
	}, adminPS)).To(Succeed(), "get admin app-credential PushSecret")
	g.Expect(adminPS.Spec.Data).NotTo(BeEmpty(), "admin app-credential PushSecret must declare a Data entry")
	g.Expect(adminPS.Spec.Data[0].Match.RemoteRef.RemoteKey).To(Equal(adminAppCredentialRemoteKeyFor(final)),
		"admin app-credential PushSecret RemoteKey must be the per-CR OpenBao path")
}

// TestIntegration_MinimalManagedToReady drives the SMALLEST valid ControlPlane —
// only openStackRelease + services.keystone — to the aggregate Ready=True. The CR
// omits spec.infrastructure and spec.korc entirely, so the defaulting webhook
// must construct the database, cache, and admin-credential blocks from
// its well-known defaults before the validating webhook's required-checks run.
// The test asserts all eight defaults on the converged spec, then drives
// every sub-reconciler to Ready exactly as TestIntegration_FullReconcile_ManagedToReady
// does, and finally asserts the projected Keystone CR's clusterRefs are wired to
// the defaulted managed infra — proving the defaults flow through projection.
func TestIntegration_MinimalManagedToReady(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupControlPlaneEnvTest(t)

	// The OpenBao-backed ClusterSecretStore must be Ready before the chain
	// reaches the credential gates (#476).
	ensureReadyClusterSecretStore(t, ctx, c)

	// Isolated test namespace per run (namespace-per-test with GenerateName).
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-minimal-cp-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed(), "create test namespace")

	// Create the MINIMAL ControlPlane CR. Create succeeds because the defaulting
	// webhook fills passwordSecretRef.name (and the whole infra/korc blocks) BEFORE
	// the validating webhook's required-check runs.
	cp := integrationMinimalControlPlane("cp", ns.Name)
	g.Expect(c.Create(ctx, cp)).To(Succeed(),
		"create minimal ControlPlane CR (required fields satisfied by the defaulting webhook)")
	cpKey := types.NamespacedName{Name: cp.Name, Namespace: ns.Name}

	// --- Core of the test: assert the well-known defaults (plus the cloudCredentialsRef.secretName) on the spec the webhook constructed from the
	// omitted infrastructure/korc blocks. ---
	defaulted := &c5c3v1alpha1.ControlPlane{}
	g.Expect(c.Get(ctx, cpKey, defaulted)).To(Succeed(), "re-fetch defaulted ControlPlane")
	db := defaulted.Spec.Infrastructure.Database
	cache := defaulted.Spec.Infrastructure.Cache
	cred := defaulted.Spec.KORC.AdminCredential
	g.Expect(db.Database).To(Equal(c5c3v1alpha1.DefaultDatabaseName),
		"defaulting webhook must materialize database.database")
	g.Expect(db.SecretRef.Name).To(Equal(c5c3v1alpha1.DefaultDatabaseSecretName),
		"defaulting webhook must materialize database.secretRef.name")
	g.Expect(db.ClusterRef).NotTo(BeNil(), "defaulting webhook must materialize database.clusterRef")
	g.Expect(db.ClusterRef.Name).To(Equal(c5c3v1alpha1.DefaultDatabaseClusterRefName),
		"defaulting webhook must materialize database.clusterRef.name")
	g.Expect(cache.Backend).To(Equal(c5c3v1alpha1.DefaultCacheBackend),
		"defaulting webhook must materialize cache.backend")
	g.Expect(cache.ClusterRef).NotTo(BeNil(), "defaulting webhook must materialize cache.clusterRef")
	g.Expect(cache.ClusterRef.Name).To(Equal(c5c3v1alpha1.DefaultCacheClusterRefName),
		"defaulting webhook must materialize cache.clusterRef.name")
	g.Expect(cred.PasswordSecretRef.Name).To(Equal(c5c3v1alpha1.DefaultAdminPasswordSecretName),
		"defaulting webhook must materialize korc.adminCredential.passwordSecretRef.name")
	g.Expect(cred.PasswordSecretRef.Key).To(Equal(c5c3v1alpha1.DefaultAdminPasswordSecretKey),
		"defaulting webhook must materialize korc.adminCredential.passwordSecretRef.key")
	g.Expect(cred.CloudCredentialsRef.CloudName).To(Equal(c5c3v1alpha1.DefaultCloudName),
		"defaulting webhook must materialize korc.adminCredential.cloudCredentialsRef.cloudName")
	g.Expect(cred.CloudCredentialsRef.SecretName).To(Equal(c5c3v1alpha1.DefaultCloudCredentialsSecretName),
		"defaulting webhook must materialize korc.adminCredential.cloudCredentialsRef.secretName")

	// --- Phases 1-4: provision the per-ControlPlane dependency set (admin Secret,
	// clouds.yaml ExternalSecret) and drive Infrastructure -> Keystone -> KORC ->
	// AdminCredential to Ready. The shared helper provisions those dependencies and
	// the managed infra children at the DEFAULTED well-known names (via the same
	// Default* constants asserted above), so reusing it here still proves the
	// defaults flow through to the reconciler. ---
	driveControlPlaneToAdminCredentialReady(t, ctx, c, cp)

	// --- Phase 5: Catalog. The minimal CR sets no gateway/publicEndpoint, so
	// keystoneCatalogURL falls back to the in-cluster Service URL. CatalogReady is
	// gated on both child CRs reporting Available, so simulate the K-ORC actuator. ---
	simulateCatalogServiceEndpointAvailableWhenPresent(t, ctx, c, cp)
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeCatalogReady, metav1.ConditionTrue, itEventuallyTimeout)

	// --- Aggregate: Ready=True. ---
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeReady, metav1.ConditionTrue, itEventuallyTimeout)

	// --- The defaulted managed infra must flow through to the projected Keystone
	// CR's clusterRefs (proving the webhook defaults are honoured by projection). ---
	final := &c5c3v1alpha1.ControlPlane{}
	g.Expect(c.Get(ctx, cpKey, final)).To(Succeed(), "get converged ControlPlane")
	ks := &keystonev1alpha1.Keystone{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: keystoneName(final), Namespace: ns.Name}, ks)).
		To(Succeed(), "get projected Keystone CR")
	g.Expect(ks.Spec.Database.ClusterRef).NotTo(BeNil(), "Keystone database clusterRef must be wired")
	g.Expect(ks.Spec.Database.ClusterRef.Name).To(Equal(c5c3v1alpha1.DefaultDatabaseClusterRefName),
		"Keystone database clusterRef must reference the defaulted managed MariaDB")
	g.Expect(ks.Spec.Database.SecretRef.Name).To(Equal(dbCredentialSecretName(final)),
		"managed Keystone DB secretRef must point at the operator-owned per-CP DB-credential Secret")
	g.Expect(ks.Spec.Database.SecretRef.Key).To(Equal("password"))
	// Admin-ref analog the defaulted managed CR also gets the
	// operator-owned per-CP admin-password ref projected into the Keystone child.
	g.Expect(ks.Spec.Bootstrap.AdminPasswordSecretRef.Name).To(Equal(adminPasswordSecretName(final)),
		"managed Keystone admin-password secretRef must point at the operator-owned per-CP admin Secret")
	g.Expect(ks.Spec.Bootstrap.AdminPasswordSecretRef.Key).To(Equal("password"))
	g.Expect(ks.Spec.Cache.ClusterRef).NotTo(BeNil(), "Keystone cache clusterRef must be wired")
	g.Expect(ks.Spec.Cache.ClusterRef.Name).To(Equal(c5c3v1alpha1.DefaultCacheClusterRefName),
		"Keystone cache clusterRef must reference the defaulted managed Memcached")
}

// TestIntegration_DBCredentialsGate_BlocksKeystoneUntilSecretExists proves the
// DBCredentials gate blocks Keystone projection until the per-CP DB-credential
// ExternalSecret is Ready once Infrastructure is Ready the
// operator creates the DB-credential ExternalSecret, but DBCredentialsReady stays
// False with reason WaitingForDBCredentialSecret and NO Keystone CR is projected
// until the ExternalSecret syncs. Simulating the sync then flips DBCredentialsReady
// True and the Keystone CR appears — pointing at the operator-owned DB-credential
// Secret. This is the negative counterpart to the full-reconcile happy path: it
// pins that the gate genuinely holds Keystone back rather than projecting it early.
func TestIntegration_DBCredentialsGate_BlocksKeystoneUntilSecretExists(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupControlPlaneEnvTest(t)

	// The OpenBao-backed ClusterSecretStore must be Ready before the chain
	// reaches the credential gates (#476).
	ensureReadyClusterSecretStore(t, ctx, c)

	// Isolated test namespace per run (namespace-per-test with GenerateName).
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-dbgate-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed(), "create test namespace")

	// Create the ControlPlane CR (the defaulting webhook fills region etc.).
	cp := integrationManagedControlPlane("cp", ns.Name)

	// Admin password Secret (mirrors driveControlPlaneToAdminCredentialReady) at the
	// operator-owned per-CP name so the later sub-reconcilers don't error — this test
	// stops at the gate, but create it for realism/consistency.
	adminSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: adminPasswordSecretName(cp), Namespace: ns.Name},
		Data:       map[string][]byte{"password": []byte("super-secret-admin-password")},
	}
	g.Expect(c.Create(ctx, adminSecret)).To(Succeed(), "create admin password Secret")

	g.Expect(c.Create(ctx, cp)).To(Succeed(), "create ControlPlane CR")
	cpKey := types.NamespacedName{Name: cp.Name, Namespace: ns.Name}

	// --- Phase 1: Infrastructure (MariaDB + Memcached) -> InfrastructureReady. ---
	simulateMariaDBReadyWhenPresent(t, ctx, c, client.ObjectKey{Name: "openstack-db", Namespace: ns.Name})
	simulateMemcachedReadyWhenPresent(t, ctx, c, client.ObjectKey{Name: "openstack-memcached", Namespace: ns.Name})
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeInfrastructureReady, metav1.ConditionTrue, itEventuallyTimeout)

	// The operator creates the per-CP DB-credential ExternalSecret as soon as
	// Infrastructure is Ready.
	g.Eventually(func() error {
		return c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: dbCredentialSecretName(cp)}, &esov1.ExternalSecret{})
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "operator must create the per-CP DB credential ExternalSecret")

	// --- The gate: BEFORE simulating the ExternalSecret sync, DBCredentialsReady must
	// be False with reason WaitingForDBCredentialSecret, and NO Keystone CR may exist. ---
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeDBCredentialsReady, metav1.ConditionFalse, itEventuallyTimeout)
	gated := &c5c3v1alpha1.ControlPlane{}
	g.Expect(c.Get(ctx, cpKey, gated)).To(Succeed(), "get gated ControlPlane")
	dbCond := meta.FindStatusCondition(gated.Status.Conditions, conditionTypeDBCredentialsReady)
	g.Expect(dbCond).NotTo(BeNil(), "DBCredentialsReady condition must exist while gated")
	g.Expect(dbCond.Reason).To(Equal("WaitingForDBCredentialSecret"),
		"DBCredentialsReady must report it is waiting on the DB credential Secret")

	// No premature/flapping Keystone CR: it must stay NotFound across a short window.
	g.Consistently(func() bool {
		err := c.Get(ctx, types.NamespacedName{Name: keystoneName(cp), Namespace: ns.Name}, &keystonev1alpha1.Keystone{})
		return apierrors.IsNotFound(err)
	}, 2*time.Second, itPollInterval).Should(BeTrue(),
		"Keystone CR must NOT be projected while the DB credential gate is closed")

	// --- Open the gate: simulate the DB-credential ExternalSecret sync. ---
	g.Expect(simulators.SimulateExternalSecretSync(ctx, c,
		client.ObjectKey{Namespace: ns.Name, Name: dbCredentialSecretName(cp)})).
		To(Succeed(), "simulate per-CP DB credential ExternalSecret sync")
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeDBCredentialsReady, metav1.ConditionTrue, itEventuallyTimeout)

	// with DBCredentials open the chain reaches the admin-password gate, which
	// ALSO blocks Keystone. Sync the operator-created admin-password ExternalSecret so
	// AdminPasswordReady flips True and the Keystone projection can proceed.
	simulateAdminPasswordExternalSecretSyncWhenPresent(t, ctx, c, cp)
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeAdminPasswordReady, metav1.ConditionTrue, itEventuallyTimeout)

	// Now the Keystone CR is projected, pointing at the operator-owned DB-credential Secret.
	gatedKs := &keystonev1alpha1.Keystone{}
	g.Eventually(func() error {
		return c.Get(ctx, types.NamespacedName{Name: keystoneName(cp), Namespace: ns.Name}, gatedKs)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
		"Keystone CR must be projected once the DB credential gate opens")
	g.Expect(gatedKs.Spec.Database.SecretRef.Name).To(Equal(dbCredentialSecretName(cp)),
		"projected Keystone DB secretRef must point at the per-CP DB-credential Secret")
}

// TestIntegration_AdminPasswordGate_BlocksKeystoneUntilExternalSecretReady proves the
// AdminPassword gate blocks Keystone projection until the per-CP admin-password
// ExternalSecret is Ready once Infrastructure and the DB-credential
// gate are satisfied the chain reaches reconcileAdminPassword, which creates the
// admin-password ExternalSecret — but AdminPasswordReady stays False with reason
// WaitingForAdminPasswordSecret and NO Keystone CR is projected until the
// ExternalSecret syncs. Simulating the sync then flips AdminPasswordReady True and the
// Keystone CR appears, its bootstrap admin-password ref pointing at the operator-owned
// per-CP admin Secret. This is the admin-password counterpart to
// TestIntegration_DBCredentialsGate_BlocksKeystoneUntilSecretExists: it pins that the
// gate genuinely holds Keystone back rather than projecting it early.
func TestIntegration_AdminPasswordGate_BlocksKeystoneUntilExternalSecretReady(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupControlPlaneEnvTest(t)

	// The OpenBao-backed ClusterSecretStore must be Ready before the chain
	// reaches the credential gates (#476).
	ensureReadyClusterSecretStore(t, ctx, c)

	// Isolated test namespace per run (namespace-per-test with GenerateName).
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-adminpwgate-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed(), "create test namespace")

	// Create the ControlPlane CR (the defaulting webhook fills region etc.).
	cp := integrationManagedControlPlane("cp", ns.Name)

	// Admin password Secret at the operator-owned per-CP name. This test stops at the
	// admin-password gate, but create it for realism/consistency with the full path.
	adminSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: adminPasswordSecretName(cp), Namespace: ns.Name},
		Data:       map[string][]byte{"password": []byte("super-secret-admin-password")},
	}
	g.Expect(c.Create(ctx, adminSecret)).To(Succeed(), "create admin password Secret")

	g.Expect(c.Create(ctx, cp)).To(Succeed(), "create ControlPlane CR")
	cpKey := types.NamespacedName{Name: cp.Name, Namespace: ns.Name}

	// --- Phase 1: Infrastructure (MariaDB + Memcached) -> InfrastructureReady. ---
	simulateMariaDBReadyWhenPresent(t, ctx, c, client.ObjectKey{Name: "openstack-db", Namespace: ns.Name})
	simulateMemcachedReadyWhenPresent(t, ctx, c, client.ObjectKey{Name: "openstack-memcached", Namespace: ns.Name})
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeInfrastructureReady, metav1.ConditionTrue, itEventuallyTimeout)

	// --- Open the DB-credential gate so the chain advances to the admin-password gate. ---
	g.Eventually(func() error {
		return c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: dbCredentialSecretName(cp)}, &esov1.ExternalSecret{})
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "operator must create the per-CP DB credential ExternalSecret")
	g.Expect(simulators.SimulateExternalSecretSync(ctx, c,
		client.ObjectKey{Namespace: ns.Name, Name: dbCredentialSecretName(cp)})).
		To(Succeed(), "simulate per-CP DB credential ExternalSecret sync")
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeDBCredentialsReady, metav1.ConditionTrue, itEventuallyTimeout)

	// The operator creates the per-CP admin-password ExternalSecret as soon as the
	// chain reaches reconcileAdminPassword (DB-credential gate open).
	g.Eventually(func() error {
		return c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: adminPasswordSecretName(cp)}, &esov1.ExternalSecret{})
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "operator must create the per-CP admin password ExternalSecret")

	// --- The gate: BEFORE simulating the admin-password ExternalSecret sync,
	// AdminPasswordReady must be False with reason WaitingForAdminPasswordSecret, and
	// NO Keystone CR may exist. ---
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeAdminPasswordReady, metav1.ConditionFalse, itEventuallyTimeout)
	gated := &c5c3v1alpha1.ControlPlane{}
	g.Expect(c.Get(ctx, cpKey, gated)).To(Succeed(), "get gated ControlPlane")
	pwCond := meta.FindStatusCondition(gated.Status.Conditions, conditionTypeAdminPasswordReady)
	g.Expect(pwCond).NotTo(BeNil(), "AdminPasswordReady condition must exist while gated")
	g.Expect(pwCond.Reason).To(Equal("WaitingForAdminPasswordSecret"),
		"AdminPasswordReady must report it is waiting on the admin password Secret")

	// No premature/flapping Keystone CR: it must stay NotFound across a short window.
	g.Consistently(func() bool {
		err := c.Get(ctx, types.NamespacedName{Name: keystoneName(cp), Namespace: ns.Name}, &keystonev1alpha1.Keystone{})
		return apierrors.IsNotFound(err)
	}, 2*time.Second, itPollInterval).Should(BeTrue(),
		"Keystone CR must NOT be projected while the admin password gate is closed")

	// --- Open the gate: simulate the admin-password ExternalSecret sync. ---
	g.Expect(simulators.SimulateExternalSecretSync(ctx, c,
		client.ObjectKey{Namespace: ns.Name, Name: adminPasswordSecretName(cp)})).
		To(Succeed(), "simulate per-CP admin password ExternalSecret sync")
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeAdminPasswordReady, metav1.ConditionTrue, itEventuallyTimeout)

	// Now the Keystone CR is projected, its bootstrap admin-password ref pointing at
	// the operator-owned per-CP admin Secret.
	gatedKs := &keystonev1alpha1.Keystone{}
	g.Eventually(func() error {
		return c.Get(ctx, types.NamespacedName{Name: keystoneName(cp), Namespace: ns.Name}, gatedKs)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
		"Keystone CR must be projected once the admin password gate opens")
	g.Expect(gatedKs.Spec.Bootstrap.AdminPasswordSecretRef.Name).To(Equal(adminPasswordSecretName(cp)),
		"projected Keystone admin-password ref must point at the per-CP admin Secret")
}

// driveControlPlaneToAdminCredentialReady provisions the full per-ControlPlane
// dependency set in cp.Namespace and drives the CR through phases 1-4 of the
// sub-reconciler chain (Infrastructure -> Keystone -> KORC -> AdminCredential) to
// conditionTypeAdminCredentialReady=True, simulating each external dependency's
// readiness exactly as TestIntegration_FullReconcile_ManagedToReady does. It
// stops short of the Catalog/aggregate-Ready phases. The namespace and the CR
// must already exist.
//
// The two managed infra clusterRef children use the shared Default*
// constants (which equal the literal names integrationManagedControlPlane sets
// explicitly), while the admin password Secret uses the per-ControlPlane
// operator-owned name adminPasswordSecretName(cp) that effectiveAdminPasswordSecretRef
// resolves in managed mode — derived from cp.Name, so it is
// distinct per CR. This lets both consumers reuse the helper:
//   - TestIntegration_MultiControlPlane_DistinctAdminCredentialPaths, whose CRs set the infra names explicitly, and
//   - TestIntegration_MinimalManagedToReady, whose minimal CR omits the
//     infra/korc blocks so the defaulting webhook materializes the very same infra
//     names — so driving the simulators at the Default* names still proves the
//
// defaults flow through to the reconciler.
func driveControlPlaneToAdminCredentialReady(
	t testing.TB, ctx context.Context, c client.Client, cp *c5c3v1alpha1.ControlPlane,
) {
	t.Helper()
	g := NewGomegaWithT(t)
	ns := cp.Namespace

	// Admin password Secret the KORC sub-reconciler hashes to drive the mint, at the
	// operator-owned per-CP name effectiveAdminPasswordSecretRef resolves in managed
	// mode — readAdminPassword reads the cleartext via that ref.
	adminSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: adminPasswordSecretName(cp), Namespace: ns},
		Data:       map[string][]byte{"password": []byte("super-secret-admin-password")},
	}
	g.Expect(c.Create(ctx, adminSecret)).To(Succeed(), "create admin password Secret")

	cpKey := types.NamespacedName{Name: cp.Name, Namespace: ns}

	// --- Phase 1: Infrastructure (MariaDB + Memcached) at the defaulted clusterRef
	// names. ---
	simulateMariaDBReadyWhenPresent(t, ctx, c,
		client.ObjectKey{Name: c5c3v1alpha1.DefaultDatabaseClusterRefName, Namespace: ns})
	simulateMemcachedReadyWhenPresent(t, ctx, c,
		client.ObjectKey{Name: c5c3v1alpha1.DefaultCacheClusterRefName, Namespace: ns})
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeInfrastructureReady, metav1.ConditionTrue, itEventuallyTimeout)

	// gate Keystone on the per-CP DB credential ExternalSecret. DECISION:
	// harness sync-simulation lives here to keep this helper's callers bisectable
	// (full suite green). This SHARED helper deliberately does NOT assert the
	// projected Keystone secretRef — that assertion lives in the
	// individual tests that fetch their own converged Keystone CR. Reviewer: please verify.
	dbCredES := &esov1.ExternalSecret{}
	g.Eventually(func() error {
		return c.Get(ctx, client.ObjectKey{Namespace: ns, Name: dbCredentialSecretName(cp)}, dbCredES)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "operator must create the per-CP DB credential ExternalSecret")
	// Managed mode defaults to Dynamic: the ExternalSecret draws from the per-CP
	// VaultDynamicSecret generator (no static Data refs). Per-tenant path
	// distinctness is asserted by TestIntegration_MultiControlPlane_DistinctDBCredentialPaths.
	g.Expect(dbCredES.Spec.DataFrom).NotTo(BeEmpty(), "Dynamic DB credential ExternalSecret must declare a generatorRef")
	g.Expect(simulators.SimulateExternalSecretSync(ctx, c,
		client.ObjectKey{Namespace: ns, Name: dbCredentialSecretName(cp)})).
		To(Succeed(), "simulate per-CP DB credential ExternalSecret sync")
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeDBCredentialsReady, metav1.ConditionTrue, itEventuallyTimeout)

	// gate Keystone on the per-CP admin-password ExternalSecret.
	// Sync-simulating here keeps this helper's callers bisectable (full suite green),
	// mirroring the DB-credential sync above.
	simulateAdminPasswordExternalSecretSyncWhenPresent(t, ctx, c, cp)
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeAdminPasswordReady, metav1.ConditionTrue, itEventuallyTimeout)

	// --- Phase 2: Keystone child. ---
	simulateKeystoneReadyWhenPresent(t, ctx, c, client.ObjectKey{Name: keystoneName(cp), Namespace: ns})
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeKeystoneReady, metav1.ConditionTrue, itEventuallyTimeout)

	// --- Phase 3: K-ORC admin ApplicationCredential. ---
	simulateApplicationCredentialAvailableWhenPresent(t, ctx, c,
		client.ObjectKey{Name: adminAppCredentialName(cp), Namespace: ns})
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeKORCReady, metav1.ConditionTrue, itEventuallyTimeout)

	// The K-ORC clouds.yaml ExternalSecret is created per-CR BY THE OPERATOR
	// (reconcileKORC -> ensureKORCCloudsYAMLExternalSecret) in the CR's own
	// namespace, no longer seeded by write-bootstrap-secrets.sh.
	// Each CR reads a DISTINCT per-CR OpenBao path (adminAppCredentialRemoteKeyFor) —
	// the meaningful multi-CP check here; full distinctness across CRs is asserted by
	// the caller via the PushSecret RemoteKeys. Assert the per-CR path, then simulate
	// its ESO sync so Phase 4 can progress.
	cloudsYamlES := &esov1.ExternalSecret{}
	g.Eventually(func() error {
		return c.Get(ctx, client.ObjectKey{Namespace: ns, Name: korcCloudsYamlSecretName}, cloudsYamlES)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "operator must create the k-orc clouds.yaml ExternalSecret")
	g.Expect(cloudsYamlES.Spec.Data).To(HaveLen(1), "clouds.yaml ExternalSecret must declare exactly one Data entry")
	g.Expect(cloudsYamlES.Spec.Data[0].RemoteRef.Key).To(Equal(adminAppCredentialRemoteKeyFor(cp)),
		"clouds.yaml ExternalSecret must read this CR's per-CR OpenBao path")
	g.Expect(simulators.SimulateExternalSecretSync(ctx, c,
		client.ObjectKey{Namespace: ns, Name: korcCloudsYamlSecretName})).
		To(Succeed(), "simulate k-orc clouds.yaml ExternalSecret sync")

	// --- Phase 4: AdminCredential push (gated on the synced clouds.yaml ES, the
	// admin app-credential PushSecret syncing to OpenBao, AND the materialized
	// k-orc-clouds-yaml Secret matching the assembled credential — the byte-compare
	// gate that closes the post-re-mint stale-credential window). ---
	simulatePushSecretSyncedWhenPresent(t, ctx, c,
		client.ObjectKey{Name: adminAppCredentialPushSecretName(cp), Namespace: childNamespace(cp)})
	simulateCloudsYamlMaterializedWhenPresent(t, ctx, c, cp)
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeAdminCredentialReady, metav1.ConditionTrue, itEventuallyTimeout)
}

// TestIntegration_MultiControlPlane_DistinctAdminCredentialPaths brings up TWO
// ControlPlanes and drives both to AdminCredentialReady=True, then asserts each
// CR's admin-credential OpenBao path (the app-credential PushSecret RemoteKey) and
// its imported admin User CR name are scoped per-ControlPlane and distinct, so two
// ControlPlanes never clobber each other's admin credential on the cluster-global
// OpenBao backend.
//
// DECISION the two ControlPlanes use DIFFERENT names (cp-a,
// cp-b) in DIFFERENT namespaces (generated from test-mcp-a- / test-mcp-b-). The
// validating webhook enforces one ControlPlane per namespace,
// so the CRs MUST live in separate namespaces; the distinct names additionally
// make the imported admin User CR names (adminUserRef = "<name>-user-admin")
// differ, which the per-CR-name assertion below requires. The per-CR OpenBao path
// is scoped by BOTH Namespace and Name (adminAppCredentialRemoteKeyFor), so either
// axis alone would distinguish them — using both is the realistic deployment shape.
func TestIntegration_MultiControlPlane_DistinctAdminCredentialPaths(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupControlPlaneEnvTest(t)

	// The OpenBao-backed ClusterSecretStore must be Ready before either chain
	// reaches the credential gates (#476).
	ensureReadyClusterSecretStore(t, ctx, c)

	// Two isolated namespaces (namespace-per-CR with GenerateName).
	nsA := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-mcp-a-"}}
	nsB := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-mcp-b-"}}
	g.Expect(c.Create(ctx, nsA)).To(Succeed(), "create namespace A")
	g.Expect(c.Create(ctx, nsB)).To(Succeed(), "create namespace B")

	// Distinct names in distinct namespaces (see DECISION above).
	cpA := integrationManagedControlPlane("cp-a", nsA.Name)
	cpB := integrationManagedControlPlane("cp-b", nsB.Name)
	g.Expect(c.Create(ctx, cpA)).To(Succeed(), "create ControlPlane A")
	g.Expect(c.Create(ctx, cpB)).To(Succeed(), "create ControlPlane B")

	driveControlPlaneToAdminCredentialReady(t, ctx, c, cpA)
	driveControlPlaneToAdminCredentialReady(t, ctx, c, cpB)

	// --- Assert the admin app-credential OpenBao paths are per-CR and distinct. ---
	psA := &esov1alpha1.PushSecret{}
	psB := &esov1alpha1.PushSecret{}
	g.Expect(c.Get(ctx, types.NamespacedName{
		Namespace: childNamespace(cpA), Name: adminAppCredentialPushSecretName(cpA),
	}, psA)).To(Succeed(), "get admin app-credential PushSecret for cp-a")
	g.Expect(c.Get(ctx, types.NamespacedName{
		Namespace: childNamespace(cpB), Name: adminAppCredentialPushSecretName(cpB),
	}, psB)).To(Succeed(), "get admin app-credential PushSecret for cp-b")

	g.Expect(psA.Spec.Data).NotTo(BeEmpty(), "cp-a PushSecret must declare a Data entry")
	g.Expect(psB.Spec.Data).NotTo(BeEmpty(), "cp-b PushSecret must declare a Data entry")
	keyA := psA.Spec.Data[0].Match.RemoteRef.RemoteKey
	keyB := psB.Spec.Data[0].Match.RemoteRef.RemoteKey

	g.Expect(keyA).To(Equal(adminAppCredentialRemoteKeyFor(cpA)),
		"cp-a OpenBao path must be the per-CR path")
	g.Expect(keyB).To(Equal(adminAppCredentialRemoteKeyFor(cpB)),
		"cp-b OpenBao path must be the per-CR path")
	g.Expect(keyA).NotTo(Equal(keyB), "the two ControlPlanes' admin OpenBao paths must be distinct")

	// Each path is scoped by its own ControlPlane's Namespace AND Name.
	g.Expect(keyA).To(ContainSubstring(cpA.Namespace), "cp-a path must contain cp-a's namespace")
	g.Expect(keyA).To(ContainSubstring(cpA.Name), "cp-a path must contain cp-a's name")
	g.Expect(keyB).To(ContainSubstring(cpB.Namespace), "cp-b path must contain cp-b's namespace")
	g.Expect(keyB).To(ContainSubstring(cpB.Name), "cp-b path must contain cp-b's name")

	// --- Assert the imported admin User CRs are per-CR and distinctly named. ---
	userA := &orcv1alpha1.User{}
	userB := &orcv1alpha1.User{}
	g.Expect(c.Get(ctx, types.NamespacedName{
		Namespace: childNamespace(cpA), Name: adminUserRef(cpA),
	}, userA)).To(Succeed(), "get imported admin User CR for cp-a")
	g.Expect(c.Get(ctx, types.NamespacedName{
		Namespace: childNamespace(cpB), Name: adminUserRef(cpB),
	}, userB)).To(Succeed(), "get imported admin User CR for cp-b")

	g.Expect(userA.Name).To(Equal(adminUserRef(cpA)), "cp-a admin User CR must be named per-CR")
	g.Expect(userB.Name).To(Equal(adminUserRef(cpB)), "cp-b admin User CR must be named per-CR")
	g.Expect(userA.Name).NotTo(Equal(userB.Name), "the two ControlPlanes' admin User CR names must be distinct")
}

// TestIntegration_MultiControlPlane_DistinctDBCredentialPaths brings up TWO
// ControlPlanes and drives both to AdminCredentialReady=True, then asserts each
// CR's per-tenant dynamic DB-credential engine path (the per-CP VaultDynamicSecret
// generator's spec.path) and the DB-credential Secret name are scoped
// per-ControlPlane and distinct, so two ControlPlanes never draw from the same
// engine role (AC 4).
//
// DECISION mirroring the admin-credential multi-CP test, the two
// ControlPlanes use DIFFERENT names (cp-a, cp-b) in DIFFERENT namespaces (the
// validating webhook enforces one ControlPlane per namespace), so the CRs MUST live
// in separate namespaces. The per-tenant creds path is keyed on the Namespace
// ALONE (dbDynamicRoleFor): the one-ControlPlane-per-namespace contract makes the
// namespace the unique tenant key, so distinct namespaces are what keep the two
// engine paths distinct.
func TestIntegration_MultiControlPlane_DistinctDBCredentialPaths(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupControlPlaneEnvTest(t)

	// The OpenBao-backed ClusterSecretStore must be Ready before either chain
	// reaches the credential gates (#476).
	ensureReadyClusterSecretStore(t, ctx, c)

	// Two isolated namespaces (namespace-per-CR with GenerateName).
	nsA := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-mcpdb-a-"}}
	nsB := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-mcpdb-b-"}}
	g.Expect(c.Create(ctx, nsA)).To(Succeed(), "create namespace A")
	g.Expect(c.Create(ctx, nsB)).To(Succeed(), "create namespace B")

	// Distinct names in distinct namespaces (see DECISION above).
	cpA := integrationManagedControlPlane("cp-a", nsA.Name)
	cpB := integrationManagedControlPlane("cp-b", nsB.Name)
	g.Expect(c.Create(ctx, cpA)).To(Succeed(), "create ControlPlane A")
	g.Expect(c.Create(ctx, cpB)).To(Succeed(), "create ControlPlane B")

	driveControlPlaneToAdminCredentialReady(t, ctx, c, cpA)
	driveControlPlaneToAdminCredentialReady(t, ctx, c, cpB)

	// --- Assert the per-CP dynamic DB-credential engine paths are per-CR and
	// distinct (AC 4). In Dynamic mode each ControlPlane owns a VaultDynamicSecret
	// generator reading its own per-tenant engine role, so a revoke against one
	// tenant's role cannot affect another. ---
	vdsA := &esgenv1alpha1.VaultDynamicSecret{}
	vdsB := &esgenv1alpha1.VaultDynamicSecret{}
	g.Expect(c.Get(ctx, types.NamespacedName{
		Namespace: childNamespace(cpA), Name: dbCredentialSecretName(cpA),
	}, vdsA)).To(Succeed(), "get VaultDynamicSecret for cp-a")
	g.Expect(c.Get(ctx, types.NamespacedName{
		Namespace: childNamespace(cpB), Name: dbCredentialSecretName(cpB),
	}, vdsB)).To(Succeed(), "get VaultDynamicSecret for cp-b")

	pathA := vdsA.Spec.Path
	pathB := vdsB.Spec.Path

	g.Expect(pathA).To(Equal(dbDynamicCredsPathFor(cpA)),
		"cp-a dynamic DB credential path must be the per-tenant engine path")
	g.Expect(pathB).To(Equal(dbDynamicCredsPathFor(cpB)),
		"cp-b dynamic DB credential path must be the per-tenant engine path")
	g.Expect(pathA).NotTo(Equal(pathB), "the two ControlPlanes' dynamic DB credential paths must be distinct")

	// Each path is keyed on its own ControlPlane's Namespace (the tenant key —
	// see dbDynamicRoleFor); the CR name is deliberately NOT part of the role.
	g.Expect(pathA).To(ContainSubstring(cpA.Namespace), "cp-a path must contain cp-a's namespace")
	g.Expect(pathB).To(ContainSubstring(cpB.Namespace), "cp-b path must contain cp-b's namespace")

	// The generator-backed ExternalSecrets exist and carry no static Data refs.
	esA := &esov1.ExternalSecret{}
	esB := &esov1.ExternalSecret{}
	g.Expect(c.Get(ctx, types.NamespacedName{
		Namespace: childNamespace(cpA), Name: dbCredentialSecretName(cpA),
	}, esA)).To(Succeed(), "get DB credential ExternalSecret for cp-a")
	g.Expect(c.Get(ctx, types.NamespacedName{
		Namespace: childNamespace(cpB), Name: dbCredentialSecretName(cpB),
	}, esB)).To(Succeed(), "get DB credential ExternalSecret for cp-b")
	g.Expect(esA.Spec.Data).To(BeEmpty(), "cp-a Dynamic ExternalSecret carries no static Data refs")
	g.Expect(esB.Spec.Data).To(BeEmpty(), "cp-b Dynamic ExternalSecret carries no static Data refs")

	// The DB-credential Secret NAMES are distinct too, so the two CRs never share a
	// materialised Secret in the (separate) namespaces.
	g.Expect(dbCredentialSecretName(cpA)).NotTo(Equal(dbCredentialSecretName(cpB)),
		"the two ControlPlanes' DB credential Secret names must be distinct")
}

// fakeKORCFinalizer mimics the finalizer K-ORC adds to the ApplicationCredential
// it manages. envtest runs no K-ORC controller, so the test injects this
// finalizer to hold the AC Terminating exactly as a real revoke-against-Keystone
// finalizer would, then removes it to let teardown complete.
const fakeKORCFinalizer = "openstack.k-orc.cloud/applicationcredential"

// TestIntegration_ControlPlaneDeletion_SequencesORCTeardown proves the
// ORC-teardown finalizer sequences deletion: the operator deletes the owned
// K-ORC ApplicationCredential FIRST and holds the ControlPlane CR (and thus,
// via the deferred owner-ref GC cascade, the projected Keystone child) until the
// AC is gone, then releases the finalizer so the rest can be garbage-collected.
//
// envtest runs no garbage collector, so the owner-ref cascade that tears down
// Keystone/MariaDB once the ControlPlane CR is removed is asserted in the e2e
// test, not here. What this test pins is the sequencing invariant the finalizer
// adds on top of GC: while a K-ORC CR is still Terminating, the ControlPlane CR
// is held and Keystone is NOT yet torn down.
func TestIntegration_ControlPlaneDeletion_SequencesORCTeardown(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupControlPlaneEnvTest(t)

	// The OpenBao-backed ClusterSecretStore must be Ready before the chain
	// reaches the credential gates (#476); otherwise reconcileDBCredentials
	// short-circuits at SecretStoreNotReady and never projects the DB-credential
	// ExternalSecret driveControlPlaneToAdminCredentialReady waits for.
	ensureReadyClusterSecretStore(t, ctx, c)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-cp-deletion-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed(), "create test namespace")

	cp := integrationMinimalControlPlane("cp", ns.Name)
	g.Expect(c.Create(ctx, cp)).To(Succeed(), "create ControlPlane CR")
	cpKey := types.NamespacedName{Name: cp.Name, Namespace: ns.Name}

	// Drive the chain until the K-ORC ApplicationCredential and the projected
	// Keystone child exist.
	driveControlPlaneToAdminCredentialReady(t, ctx, c, cp)

	acKey := types.NamespacedName{Name: adminAppCredentialName(cp), Namespace: ns.Name}
	ksKey := types.NamespacedName{Name: keystoneName(cp), Namespace: ns.Name}
	g.Expect(c.Get(ctx, ksKey, &keystonev1alpha1.Keystone{})).To(Succeed(),
		"projected Keystone child must exist before deletion")

	// The ControlPlane must carry the ORC-teardown finalizer once reconciled.
	g.Eventually(func() bool {
		got := &c5c3v1alpha1.ControlPlane{}
		if err := c.Get(ctx, cpKey, got); err != nil {
			return false
		}
		return controllerutil.ContainsFinalizer(got, controlPlaneORCFinalizer)
	}, itEventuallyTimeout, itPollInterval).Should(BeTrue(), "ControlPlane must carry the ORC-teardown finalizer")

	// Inject a fake K-ORC finalizer onto the AC so deleting it leaves it
	// Terminating (as a real revoke-against-Keystone finalizer would), rather
	// than removing it outright in the GC-less envtest.
	g.Eventually(func() error {
		ac := &orcv1alpha1.ApplicationCredential{}
		if err := c.Get(ctx, acKey, ac); err != nil {
			return err
		}
		if controllerutil.ContainsFinalizer(ac, fakeKORCFinalizer) {
			return nil
		}
		controllerutil.AddFinalizer(ac, fakeKORCFinalizer)
		return c.Update(ctx, ac)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "inject fake K-ORC finalizer on the AC")

	// Delete the ControlPlane.
	g.Expect(c.Delete(ctx, cp)).To(Succeed(), "delete ControlPlane CR")

	// Teardown must be initiated: the operator deletes the AC, which is held
	// Terminating by the fake K-ORC finalizer.
	g.Eventually(func() bool {
		ac := &orcv1alpha1.ApplicationCredential{}
		if err := c.Get(ctx, acKey, ac); err != nil {
			return false
		}
		return !ac.DeletionTimestamp.IsZero()
	}, itEventuallyTimeout, itPollInterval).Should(BeTrue(), "operator must delete the owned AC first")

	// Sequencing invariant: while the AC is still Terminating, the ControlPlane
	// CR is HELD (finalizer not released) and the Keystone child is NOT torn down.
	g.Consistently(func() bool {
		gotCP := &c5c3v1alpha1.ControlPlane{}
		if err := c.Get(ctx, cpKey, gotCP); err != nil {
			return false
		}
		if !controllerutil.ContainsFinalizer(gotCP, controlPlaneORCFinalizer) {
			return false
		}
		return c.Get(ctx, ksKey, &keystonev1alpha1.Keystone{}) == nil
	}, 3*time.Second, itPollInterval).Should(BeTrue(),
		"ControlPlane finalizer must hold (and Keystone must survive) while the K-ORC CR is Terminating")

	// Release the AC by removing the fake finalizer; the operator then releases
	// the ControlPlane finalizer.
	g.Eventually(func() error {
		ac := &orcv1alpha1.ApplicationCredential{}
		err := c.Get(ctx, acKey, ac)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		controllerutil.RemoveFinalizer(ac, fakeKORCFinalizer)
		return c.Update(ctx, ac)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "remove fake K-ORC finalizer from the AC")

	// Once the AC is gone the operator releases the ControlPlane finalizer, so
	// both objects disappear.
	g.Eventually(func() bool {
		acErr := c.Get(ctx, acKey, &orcv1alpha1.ApplicationCredential{})
		cpErr := c.Get(ctx, cpKey, &c5c3v1alpha1.ControlPlane{})
		return apierrors.IsNotFound(acErr) && apierrors.IsNotFound(cpErr)
	}, itEventuallyTimeout, itPollInterval).Should(BeTrue(),
		"AC and ControlPlane must be removed once the K-ORC finalizer clears")
}

// TestIntegration_ControlPlane_ValidationMarkers pins the validation-marker wave
// on the ControlPlane CRD against the envtest API server (CRD schema + CEL +
// validating webhook). Each rejection case mutates one field of an otherwise
// valid managed ControlPlane in its own namespace (the webhook enforces one
// ControlPlane per namespace); the final case asserts valid non-default
// accessRules, bootstrapResources, and publicEndpoint are accepted.
func TestIntegration_ControlPlane_ValidationMarkers(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupControlPlaneEnvTest(t)

	cases := []struct {
		name    string
		mutate  func(*c5c3v1alpha1.ControlPlane)
		wantErr bool
	}{
		{
			name:    "database both clusterRef and host",
			wantErr: true,
			mutate: func(cp *c5c3v1alpha1.ControlPlane) {
				cp.Spec.Infrastructure.Database.Host = "db.example.com"
			},
		},
		{
			name:    "cache both clusterRef and servers",
			wantErr: true,
			mutate: func(cp *c5c3v1alpha1.ControlPlane) {
				cp.Spec.Infrastructure.Cache.Servers = []string{"mc:11211"}
			},
		},
		{
			name:    "non-URL keystone publicEndpoint",
			wantErr: true,
			mutate: func(cp *c5c3v1alpha1.ControlPlane) {
				cp.Spec.Services.Keystone.PublicEndpoint = "keystone.example.com"
			},
		},
		{
			name:    "accessRule invalid method",
			wantErr: true,
			mutate: func(cp *c5c3v1alpha1.ControlPlane) {
				cp.Spec.KORC.AdminCredential.ApplicationCredential.AccessRules = []c5c3v1alpha1.AccessRule{
					{Service: "compute", Method: "FETCH", Path: "/v2.1/servers"},
				}
			},
		},
		{
			name:    "accessRule non-absolute path",
			wantErr: true,
			mutate: func(cp *c5c3v1alpha1.ControlPlane) {
				cp.Spec.KORC.AdminCredential.ApplicationCredential.AccessRules = []c5c3v1alpha1.AccessRule{
					{Service: "compute", Method: "GET", Path: "v2.1/servers"},
				}
			},
		},
		{
			name:    "bootstrapResource invalid kind",
			wantErr: true,
			mutate: func(cp *c5c3v1alpha1.ControlPlane) {
				cp.Spec.KORC.AdminCredential.BootstrapResources = []c5c3v1alpha1.BootstrapResourceSpec{
					{Kind: "Network", Name: "ext"},
				}
			},
		},
		{
			name:    "valid access rules, bootstrap resources, and public endpoint",
			wantErr: false,
			mutate: func(cp *c5c3v1alpha1.ControlPlane) {
				cp.Spec.Services.Keystone.PublicEndpoint = "https://keystone.example.com/v3"
				cp.Spec.KORC.AdminCredential.ApplicationCredential.AccessRules = []c5c3v1alpha1.AccessRule{
					{Service: "compute", Method: "GET", Path: "/v2.1/servers"},
				}
				cp.Spec.KORC.AdminCredential.BootstrapResources = []c5c3v1alpha1.BootstrapResourceSpec{
					{Kind: "Project", Name: "service"},
					{Kind: "Role", Name: "admin"},
				}
			},
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-cp-marker-"}}
			g.Expect(c.Create(ctx, ns)).To(Succeed())

			cp := integrationManagedControlPlane(fmt.Sprintf("cp-marker-%d", i), ns.Name)
			tc.mutate(cp)

			err := c.Create(ctx, cp)
			if tc.wantErr {
				g.Expect(err).To(HaveOccurred(), "admission must reject: %s", tc.name)
				g.Expect(apierrors.IsInvalid(err) || apierrors.IsForbidden(err)).To(BeTrue(),
					fmt.Sprintf("expected Invalid or Forbidden status error for %q, got: %v", tc.name, err))
			} else {
				g.Expect(err).NotTo(HaveOccurred(), "admission must accept: %s", tc.name)
			}
		})
	}
}

// integrationExternalControlPlane returns the issue's minimal External-mode
// sketch CR (mode: External + external.authURL + korc.adminCredential.
// passwordSecretRef, no infrastructure block) for the envtest matrix.
func integrationExternalControlPlane(name, namespace string) *c5c3v1alpha1.ControlPlane {
	return &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: c5c3v1alpha1.ControlPlaneSpec{
			OpenStackRelease: "2025.2",
			Services: c5c3v1alpha1.ServicesSpec{
				Keystone: &c5c3v1alpha1.ServiceKeystoneSpec{
					Mode: c5c3v1alpha1.KeystoneModeExternal,
					External: &c5c3v1alpha1.ExternalKeystoneSpec{
						AuthURL: "https://keystone.example.com/v3",
					},
				},
			},
			KORC: c5c3v1alpha1.KORCSpec{
				AdminCredential: c5c3v1alpha1.AdminCredentialSpec{
					CloudCredentialsRef: c5c3v1alpha1.CloudCredentialsRef{CloudName: "admin"},
					PasswordSecretRef:   commonv1.SecretRefSpec{Name: "external-admin", Key: "password"},
				},
			},
		},
	}
}

// TestIntegration_ExternalMode_AcceptedAndDefaulted drives the External-mode API
// surface against the real envtest API server (CRD schema + CEL + defaulting and
// validating webhooks). It proves the sketch CR is admitted and stored with the
// External-mode defaults materialized and NO infrastructure block invented.
func TestIntegration_ExternalMode_AcceptedAndDefaulted(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupControlPlaneEnvTest(t)
	g := NewGomegaWithT(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-cp-external-ok-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	cp := integrationExternalControlPlane("cp-external", ns.Name)
	g.Expect(c.Create(ctx, cp)).To(Succeed(), "the minimal External-mode sketch CR must be admitted")

	fetched := &c5c3v1alpha1.ControlPlane{}
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(cp), fetched)).To(Succeed())

	g.Expect(fetched.Spec.Infrastructure).To(BeNil(),
		"External mode must persist with no infrastructure block")
	g.Expect(fetched.Spec.Services.Keystone.Mode).To(Equal(c5c3v1alpha1.KeystoneModeExternal))
	g.Expect(fetched.Spec.Services.Keystone.External).NotTo(BeNil())
	g.Expect(fetched.Spec.Services.Keystone.External.EndpointType).
		To(Equal(c5c3v1alpha1.ExternalEndpointTypePublic), "endpointType must default to public")
	// admin identity defaults materialize in External mode too.
	g.Expect(fetched.Spec.KORC.AdminCredential.UserName).To(Equal("admin"))
	g.Expect(fetched.Spec.KORC.AdminCredential.ProjectName).To(Equal("admin"))
	g.Expect(fetched.Spec.KORC.AdminCredential.DomainName).To(Equal("Default"))
}

// TestIntegration_ExternalMode_Rejections exercises the External-mode rejection
// matrix at the real admission chain. The CEL cases prove the schema layer holds
// even if the validating webhook were bypassed; the cross-field cases exercise
// the webhook-only rules.
func TestIntegration_ExternalMode_Rejections(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupControlPlaneEnvTest(t)

	cases := []struct {
		name   string
		mutate func(*c5c3v1alpha1.ControlPlane)
	}{
		{
			name: "CEL: managed-only replicas set in External mode",
			mutate: func(cp *c5c3v1alpha1.ControlPlane) {
				cp.Spec.Services.Keystone.Replicas = ptr.To(int32(3))
			},
		},
		{
			name: "CEL: external block set in Managed mode",
			mutate: func(cp *c5c3v1alpha1.ControlPlane) {
				cp.Spec.Services.Keystone.Mode = c5c3v1alpha1.KeystoneModeManaged
				// Managed mode requires infrastructure; supply it so the ONLY
				// violation under test is external-in-Managed.
				cp.Spec.Infrastructure = &c5c3v1alpha1.InfrastructureSpec{
					Database: commonv1.DatabaseSpec{Host: "db", Database: "d", SecretRef: commonv1.SecretRefSpec{Name: "s"}},
					Cache:    commonv1.CacheSpec{Backend: "b", Servers: []string{"mc:11211"}},
				}
			},
		},
		{
			name: "schema: endpointType outside the enum",
			mutate: func(cp *c5c3v1alpha1.ControlPlane) {
				cp.Spec.Services.Keystone.External.EndpointType = "gopher"
			},
		},
		{
			name: "schema: authURL not an http(s) URL",
			mutate: func(cp *c5c3v1alpha1.ControlPlane) {
				cp.Spec.Services.Keystone.External.AuthURL = "keystone.example.com"
			},
		},
		{
			// The coarse ^https?:// prefix accepted a scheme-only, hostless URL;
			// the tightened ^https?://[^\s/]+ pattern (and net/url webhook) reject
			// it so the identity consumer never dials a hostless endpoint.
			name: "schema: authURL has no host",
			mutate: func(cp *c5c3v1alpha1.ControlPlane) {
				cp.Spec.Services.Keystone.External.AuthURL = "https://"
			},
		},
		{
			name: "webhook: infrastructure set in External mode",
			mutate: func(cp *c5c3v1alpha1.ControlPlane) {
				cp.Spec.Infrastructure = &c5c3v1alpha1.InfrastructureSpec{
					Database: commonv1.DatabaseSpec{Host: "db", Database: "d", SecretRef: commonv1.SecretRefSpec{Name: "s"}},
					Cache:    commonv1.CacheSpec{Backend: "b", Servers: []string{"mc:11211"}},
				}
			},
		},
		{
			name: "webhook: external block missing in External mode",
			mutate: func(cp *c5c3v1alpha1.ControlPlane) {
				cp.Spec.Services.Keystone.External = nil
			},
		},
		{
			// The identity entry is owned by the imports; declaring it as a managed
			// entry is the one way the creation opt-in could clobber the external
			// catalog's own identity row.
			name: "CEL: identity declared as a managed catalog entry",
			mutate: func(cp *c5c3v1alpha1.ControlPlane) {
				cp.Spec.Services.Keystone.External.Catalog = &c5c3v1alpha1.ExternalCatalogSpec{
					ManagedEntries: []c5c3v1alpha1.ExternalCatalogEntrySpec{
						{Type: c5c3v1alpha1.IdentityCatalogServiceType},
					},
				}
			},
		},
		{
			name: "schema: managed catalog entry type is not a DNS-1123 label",
			mutate: func(cp *c5c3v1alpha1.ControlPlane) {
				cp.Spec.Services.Keystone.External.Catalog = &c5c3v1alpha1.ExternalCatalogSpec{
					ManagedEntries: []c5c3v1alpha1.ExternalCatalogEntrySpec{{Type: "Image_Service"}},
				}
			},
		},
		{
			name: "schema: managed catalog endpoint URL is not an http(s) URL",
			mutate: func(cp *c5c3v1alpha1.ControlPlane) {
				cp.Spec.Services.Keystone.External.Catalog = &c5c3v1alpha1.ExternalCatalogSpec{
					ManagedEntries: []c5c3v1alpha1.ExternalCatalogEntrySpec{{
						Type: "image",
						Endpoints: []c5c3v1alpha1.ExternalCatalogEndpointSpec{
							{Interface: c5c3v1alpha1.ExternalEndpointTypePublic, URL: "glance.example.com"},
						},
					}},
				}
			},
		},
		{
			// endpoints is a listType=map keyed on interface, so the apiserver — not
			// the webhook — refuses the duplicate.
			name: "schema: two managed catalog endpoints share an interface",
			mutate: func(cp *c5c3v1alpha1.ControlPlane) {
				cp.Spec.Services.Keystone.External.Catalog = &c5c3v1alpha1.ExternalCatalogSpec{
					ManagedEntries: []c5c3v1alpha1.ExternalCatalogEntrySpec{{
						Type: "image",
						Endpoints: []c5c3v1alpha1.ExternalCatalogEndpointSpec{
							{Interface: c5c3v1alpha1.ExternalEndpointTypePublic, URL: "https://a.example.com"},
							{Interface: c5c3v1alpha1.ExternalEndpointTypePublic, URL: "https://b.example.com"},
						},
					}},
				}
			},
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-cp-external-"}}
			g.Expect(c.Create(ctx, ns)).To(Succeed())

			cp := integrationExternalControlPlane(fmt.Sprintf("cp-external-%d", i), ns.Name)
			tc.mutate(cp)

			err := c.Create(ctx, cp)
			g.Expect(err).To(HaveOccurred(), "admission must reject: %s", tc.name)
			g.Expect(apierrors.IsInvalid(err)).To(BeTrue(),
				fmt.Sprintf("expected Invalid status error for %q, got: %v", tc.name, err))
		})
	}
}

// TestIntegration_ExternalMode_TransitionsRejected verifies the keystone-mode
// transition gating on real UPDATEs against the envtest API server: a live
// managed ControlPlane cannot flip to External, and a live External ControlPlane
// cannot flip to Managed.
func TestIntegration_ExternalMode_TransitionsRejected(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupControlPlaneEnvTest(t)

	t.Run("managed -> External rejected", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-cp-m2e-"}}
		g.Expect(c.Create(ctx, ns)).To(Succeed())

		cp := integrationManagedControlPlane("cp-m2e", ns.Name)
		g.Expect(c.Create(ctx, cp)).To(Succeed())

		fetched := &c5c3v1alpha1.ControlPlane{}
		g.Expect(c.Get(ctx, client.ObjectKeyFromObject(cp), fetched)).To(Succeed())
		fetched.Spec.Services.Keystone.Mode = c5c3v1alpha1.KeystoneModeExternal
		fetched.Spec.Services.Keystone.Replicas = nil
		fetched.Spec.Services.Keystone.External = &c5c3v1alpha1.ExternalKeystoneSpec{
			AuthURL: "https://keystone.example.com/v3",
		}
		fetched.Spec.Infrastructure = nil

		err := c.Update(ctx, fetched)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("cannot be changed to External"))
	})

	t.Run("External -> Managed rejected", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-cp-e2m-"}}
		g.Expect(c.Create(ctx, ns)).To(Succeed())

		cp := integrationExternalControlPlane("cp-e2m", ns.Name)
		g.Expect(c.Create(ctx, cp)).To(Succeed())

		fetched := &c5c3v1alpha1.ControlPlane{}
		g.Expect(c.Get(ctx, client.ObjectKeyFromObject(cp), fetched)).To(Succeed())
		fetched.Spec.Services.Keystone.Mode = c5c3v1alpha1.KeystoneModeManaged
		fetched.Spec.Services.Keystone.External = nil
		fetched.Spec.Infrastructure = &c5c3v1alpha1.InfrastructureSpec{
			Database: commonv1.DatabaseSpec{Host: "db", Database: "d", SecretRef: commonv1.SecretRefSpec{Name: "s"}},
			Cache:    commonv1.CacheSpec{Backend: "b", Servers: []string{"mc:11211"}},
		}

		err := c.Update(ctx, fetched)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("phase-3"))
	})
}

// driveExternalControlPlaneToReady creates the user-supplied admin-password
// Secret an External-mode ControlPlane reads, then simulates every external
// dependency the K-ORC chain waits on. The four skipped sub-reconcilers need no
// simulation at all — that is the point of External mode.
func driveExternalControlPlaneToReady(
	t testing.TB, ctx context.Context, c client.Client, cp *c5c3v1alpha1.ControlPlane,
) {
	t.Helper()
	g := NewGomegaWithT(t)
	ns := cp.Namespace
	cpKey := types.NamespacedName{Name: cp.Name, Namespace: ns}

	// In External mode effectiveAdminPasswordSecretRef resolves to the USER's
	// Secret, so the cleartext readAdminPassword reads lives under that name.
	adminSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cp.Spec.KORC.AdminCredential.PasswordSecretRef.Name,
			Namespace: ns,
		},
		Data: map[string][]byte{"password": []byte("super-secret-admin-password")},
	}
	g.Expect(c.Create(ctx, adminSecret)).To(Succeed(), "create the user-supplied admin password Secret")

	g.Expect(c.Create(ctx, cp)).To(Succeed(), "create the External-mode ControlPlane CR")

	// The four skipped sub-reconcilers converge with no simulation whatsoever.
	for _, condType := range []string{
		conditionTypeInfrastructureReady,
		conditionTypeDBCredentialsReady,
		conditionTypeAdminPasswordReady,
		conditionTypeKeystoneReady,
	} {
		waitForControlPlaneCondition(t, ctx, c, cpKey, condType, metav1.ConditionTrue, itEventuallyTimeout)
	}
	// services.horizon is forbidden in External mode, so Horizon reports not-managed.
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeHorizonReady, metav1.ConditionTrue, itEventuallyTimeout)

	// K-ORC chain: identical to managed mode, driven against the external Keystone.
	simulateApplicationCredentialAvailableWhenPresent(t, ctx, c,
		client.ObjectKey{Name: adminAppCredentialName(cp), Namespace: ns})
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeKORCReady, metav1.ConditionTrue, itEventuallyTimeout)

	g.Eventually(func() error {
		return c.Get(ctx, client.ObjectKey{Namespace: ns, Name: korcCloudsYamlSecretName}, &esov1.ExternalSecret{})
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "operator must create the k-orc clouds.yaml ExternalSecret")
	g.Expect(simulators.SimulateExternalSecretSync(ctx, c,
		client.ObjectKey{Namespace: ns, Name: korcCloudsYamlSecretName})).To(Succeed())

	simulatePushSecretSyncedWhenPresent(t, ctx, c,
		client.ObjectKey{Name: adminAppCredentialPushSecretName(cp), Namespace: ns})
	simulateCloudsYamlMaterializedWhenPresent(t, ctx, c, cp)
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeAdminCredentialReady, metav1.ConditionTrue, itEventuallyTimeout)

	// The catalog is IMPORTED, not registered: resolve the unmanaged imports.
	simulateExternalCatalogImportsAvailableWhenPresent(t, ctx, c, cp)
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeCatalogReady, metav1.ConditionTrue, itEventuallyTimeout)
}

// TestIntegration_ExternalMode_ConvergesToReadyWithNoWorkloads is the headline
// acceptance criterion: an External-mode ControlPlane whose external dependencies
// are reachable converges Ready=True while creating ZERO MariaDB, Memcached,
// Keystone or Horizon resources — and no operator-owned credential ExternalSecrets.
func TestIntegration_ExternalMode_ConvergesToReadyWithNoWorkloads(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupControlPlaneEnvTest(t)
	ensureReadyClusterSecretStore(t, ctx, c)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-cp-external-ready-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed(), "create test namespace")

	cp := integrationExternalControlPlane("cp-external", ns.Name)
	driveExternalControlPlaneToReady(t, ctx, c, cp)

	cpKey := types.NamespacedName{Name: cp.Name, Namespace: ns.Name}
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeReady, metav1.ConditionTrue, itEventuallyTimeout)

	final := &c5c3v1alpha1.ControlPlane{}
	g.Expect(c.Get(ctx, cpKey, final)).To(Succeed())

	readyCond := meta.FindStatusCondition(final.Status.Conditions, conditionTypeReady)
	g.Expect(readyCond.Reason).To(Equal("AllReady"))
	g.Expect(final.Status.ObservedGeneration).To(Equal(final.Generation))

	// Each skipped sub-reconciler reports the dedicated ExternallyManaged reason.
	for _, condType := range []string{
		conditionTypeInfrastructureReady,
		conditionTypeDBCredentialsReady,
		conditionTypeAdminPasswordReady,
		conditionTypeKeystoneReady,
	} {
		cond := meta.FindStatusCondition(final.Status.Conditions, condType)
		g.Expect(cond).NotTo(BeNil(), "condition %s should exist", condType)
		g.Expect(cond.Reason).To(Equal(conditionReasonExternallyManaged),
			"condition %s must report the External-mode skip reason", condType)
		g.Expect(cond.Message).To(ContainSubstring("https://keystone.example.com/v3"),
			"condition %s must name the external endpoint", condType)
	}
	horizonCond := meta.FindStatusCondition(final.Status.Conditions, conditionTypeHorizonReady)
	g.Expect(horizonCond.Reason).To(Equal("HorizonNotManaged"))

	// ZERO workloads. This is the acceptance criterion, so assert absence directly.
	var mariadbList mariadbv1alpha1.MariaDBList
	g.Expect(c.List(ctx, &mariadbList, client.InNamespace(ns.Name))).To(Succeed())
	g.Expect(mariadbList.Items).To(BeEmpty(), "External mode must create no MariaDB")

	memcachedList := &unstructured.UnstructuredList{}
	memcachedList.SetGroupVersionKind(memcachedGVK)
	g.Expect(c.List(ctx, memcachedList, client.InNamespace(ns.Name))).To(Succeed())
	g.Expect(memcachedList.Items).To(BeEmpty(), "External mode must create no Memcached")

	var keystoneList keystonev1alpha1.KeystoneList
	g.Expect(c.List(ctx, &keystoneList, client.InNamespace(ns.Name))).To(Succeed())
	g.Expect(keystoneList.Items).To(BeEmpty(), "External mode must create no Keystone child")

	var horizonList horizonv1alpha1.HorizonList
	g.Expect(c.List(ctx, &horizonList, client.InNamespace(ns.Name))).To(Succeed())
	g.Expect(horizonList.Items).To(BeEmpty(), "External mode must create no Horizon child")

	// No operator-owned credential projections either.
	g.Expect(apierrors.IsNotFound(c.Get(ctx,
		client.ObjectKey{Namespace: ns.Name, Name: dbCredentialSecretName(cp)}, &esov1.ExternalSecret{}))).
		To(BeTrue(), "External mode must project no DB-credential ExternalSecret")
	g.Expect(apierrors.IsNotFound(c.Get(ctx,
		client.ObjectKey{Namespace: ns.Name, Name: adminPasswordSecretName(cp)}, &esov1.ExternalSecret{}))).
		To(BeTrue(), "External mode must project no admin-password ExternalSecret")

	// status.services reports the single configured service.
	g.Expect(final.Status.Services).To(HaveLen(1))
	g.Expect(final.Status.Services[0].Name).To(Equal(keystoneServiceKey))
	g.Expect(final.Status.Services[0].Ready).To(BeTrue())

	// ZERO catalog entries. Pointed at a populated catalog, the ControlPlane must
	// have created nothing: every K-ORC Service/Endpoint in the namespace is an
	// unmanaged import. This is the import-first acceptance criterion.
	catalogCond := meta.FindStatusCondition(final.Status.Conditions, conditionTypeCatalogReady)
	g.Expect(catalogCond.Reason).To(Equal(conditionReasonCatalogImported))

	var korcServices orcv1alpha1.ServiceList
	g.Expect(c.List(ctx, &korcServices, client.InNamespace(ns.Name))).To(Succeed())
	g.Expect(korcServices.Items).To(HaveLen(1))
	g.Expect(korcServices.Items[0].Spec.ManagementPolicy).To(Equal(orcv1alpha1.ManagementPolicyUnmanaged),
		"External mode must create no managed catalog Service")

	var korcEndpoints orcv1alpha1.EndpointList
	g.Expect(c.List(ctx, &korcEndpoints, client.InNamespace(ns.Name))).To(Succeed())
	g.Expect(korcEndpoints.Items).To(HaveLen(len(externalCatalogInterfaces)))
	for _, ep := range korcEndpoints.Items {
		g.Expect(ep.Spec.ManagementPolicy).To(Equal(orcv1alpha1.ManagementPolicyUnmanaged),
			"External mode must create no managed catalog Endpoint")
	}

	// ... and the existing identity service/endpoints appear as resolved imports.
	// CatalogReady gates only on the REQUIRED imports — the identity Service and the
	// single endpointType interface (public here) — so the two non-gating interface
	// imports resolve on a later reconcile driven by their K-ORC status watch, which
	// can land after Ready flips. Poll for that convergence rather than asserting on
	// the single snapshot taken the instant Ready went True.
	g.Eventually(func() error {
		live := &c5c3v1alpha1.ControlPlane{}
		if err := c.Get(ctx, cpKey, live); err != nil {
			return err
		}
		if live.Status.Catalog == nil {
			return fmt.Errorf("status.catalog not projected yet")
		}
		if got, want := len(live.Status.Catalog.Imports), 1+len(externalCatalogInterfaces); got != want {
			return fmt.Errorf("status.catalog.imports has %d entries, want %d", got, want)
		}
		for _, imp := range live.Status.Catalog.Imports {
			if !imp.Resolved {
				return fmt.Errorf("import %s not reported resolved yet", imp.Name)
			}
			if imp.ID == "" {
				return fmt.Errorf("import %s reports no resolved OpenStack id", imp.Name)
			}
		}
		return nil
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
		"every catalog import — the identity Service and all %d interfaces — must resolve with an id",
		len(externalCatalogInterfaces))
}

// TestIntegration_ExternalMode_OptInCatalogEntryLifecycle is the opt-in acceptance
// criterion, end to end against a real API server: declaring an entry creates
// exactly it, and removing the declaration deletes exactly it — the unmanaged
// identity imports are never touched.
func TestIntegration_ExternalMode_OptInCatalogEntryLifecycle(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupControlPlaneEnvTest(t)
	ensureReadyClusterSecretStore(t, ctx, c)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-cp-external-optin-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed(), "create test namespace")

	cp := integrationExternalControlPlane("cp-external", ns.Name)
	driveExternalControlPlaneToReady(t, ctx, c, cp)
	cpKey := types.NamespacedName{Name: cp.Name, Namespace: ns.Name}
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeReady, metav1.ConditionTrue, itEventuallyTimeout)

	entryServiceKey := client.ObjectKey{Namespace: ns.Name, Name: catalogEntryServiceName(cp, "image")}
	entryEndpointKey := client.ObjectKey{
		Namespace: ns.Name,
		Name:      catalogEntryEndpointName(cp, "image", c5c3v1alpha1.ExternalEndpointTypePublic),
	}

	// Declare one entry.
	g.Eventually(func() error {
		live := &c5c3v1alpha1.ControlPlane{}
		if err := c.Get(ctx, cpKey, live); err != nil {
			return err
		}
		live.Spec.Services.Keystone.External.Catalog = &c5c3v1alpha1.ExternalCatalogSpec{
			ManagedEntries: []c5c3v1alpha1.ExternalCatalogEntrySpec{{
				Type: "image",
				Endpoints: []c5c3v1alpha1.ExternalCatalogEndpointSpec{
					{Interface: c5c3v1alpha1.ExternalEndpointTypePublic, URL: "https://glance.example.com"},
				},
			}},
		}
		return c.Update(ctx, live)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "declare a managed catalog entry")

	entryService := &orcv1alpha1.Service{}
	g.Eventually(func() error {
		return c.Get(ctx, entryServiceKey, entryService)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "the declared entry's Service must be created")
	g.Expect(entryService.Spec.ManagementPolicy).To(Equal(orcv1alpha1.ManagementPolicyManaged))
	g.Expect(entryService.Spec.Resource.Type).To(Equal("image"))

	entryEndpoint := &orcv1alpha1.Endpoint{}
	g.Eventually(func() error {
		return c.Get(ctx, entryEndpointKey, entryEndpoint)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "the declared entry's Endpoint must be created")
	g.Expect(entryEndpoint.Spec.Resource.URL).To(Equal("https://glance.example.com"))

	// Nothing else was created: still exactly one unmanaged identity Service import.
	var services orcv1alpha1.ServiceList
	g.Expect(c.List(ctx, &services, client.InNamespace(ns.Name))).To(Succeed())
	g.Expect(services.Items).To(HaveLen(2), "exactly the identity import plus the one declared entry")

	// Remove the declaration: exactly those two CRs go, the imports stay.
	g.Eventually(func() error {
		live := &c5c3v1alpha1.ControlPlane{}
		if err := c.Get(ctx, cpKey, live); err != nil {
			return err
		}
		live.Spec.Services.Keystone.External.Catalog = nil
		return c.Update(ctx, live)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "remove the managed catalog entry")

	g.Eventually(func() bool {
		svcGone := apierrors.IsNotFound(c.Get(ctx, entryServiceKey, &orcv1alpha1.Service{}))
		epGone := apierrors.IsNotFound(c.Get(ctx, entryEndpointKey, &orcv1alpha1.Endpoint{}))
		return svcGone && epGone
	}, itEventuallyTimeout, itPollInterval).Should(BeTrue(),
		"removing the opt-in must delete exactly the declared entry's CRs")

	// The unmanaged identity imports are untouched by the removal sweep.
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: keystoneServiceName(cp)},
		&orcv1alpha1.Service{})).To(Succeed(), "the identity Service import must survive")
	for _, iface := range externalCatalogInterfaces {
		g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: keystoneEndpointImportName(cp, iface)},
			&orcv1alpha1.Endpoint{})).To(Succeed(), "the %q Endpoint import must survive", iface)
	}
}

// TestIntegration_ExternalMode_DeletionLeavesUnmanagedImportsAlone is the AC-4
// end-to-end guard: deleting a converged External-mode ControlPlane tears down the
// operator-owned K-ORC CRs and provably nothing else — a foreign K-ORC User in the
// same namespace survives.
func TestIntegration_ExternalMode_DeletionLeavesUnmanagedImportsAlone(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupControlPlaneEnvTest(t)
	ensureReadyClusterSecretStore(t, ctx, c)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-cp-external-delete-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed(), "create test namespace")

	// A K-ORC User this ControlPlane never created. It shares the namespace and the
	// kind of the operator's own admin import, so only NAME scoping keeps it safe.
	foreign := &orcv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "foreign-user", Namespace: ns.Name},
		Spec: orcv1alpha1.UserSpec{
			ManagementPolicy: orcv1alpha1.ManagementPolicyUnmanaged,
			Import:           &orcv1alpha1.UserImport{Filter: &orcv1alpha1.UserFilter{}},
		},
	}
	g.Expect(c.Create(ctx, foreign)).To(Succeed(), "create an unrelated K-ORC User import")

	cp := integrationExternalControlPlane("cp-external", ns.Name)
	driveExternalControlPlaneToReady(t, ctx, c, cp)

	cpKey := types.NamespacedName{Name: cp.Name, Namespace: ns.Name}
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeReady, metav1.ConditionTrue, itEventuallyTimeout)

	g.Expect(c.Delete(ctx, cp)).To(Succeed(), "delete the External-mode ControlPlane")

	// Every owned K-ORC CR disappears and the finalizer releases the ControlPlane.
	// envtest runs no garbage collector, so this is the reconcileDelete sweep, not GC.
	g.Eventually(func() bool {
		for _, child := range orcChildObjects(cp) {
			obj := child.newObj()
			key := client.ObjectKey{Name: child.name, Namespace: ns.Name}
			if err := c.Get(ctx, key, obj); !apierrors.IsNotFound(err) {
				return false
			}
		}
		return apierrors.IsNotFound(c.Get(ctx, cpKey, &c5c3v1alpha1.ControlPlane{}))
	}, itEventuallyTimeout, itPollInterval).Should(BeTrue(),
		"the owned K-ORC CRs — including the per-interface identity catalog imports — and the ControlPlane must be gone")

	// ... and the foreign import is untouched.
	g.Expect(c.Get(ctx, client.ObjectKey{Name: "foreign-user", Namespace: ns.Name}, &orcv1alpha1.User{})).
		To(Succeed(), "a K-ORC CR the ControlPlane does not own must survive its deletion")
}

// --- External keystone mode: the generated credentials and their lifecycle ---

// itExternalCABundle is the private-CA bundle the External-mode envtest scenarios
// reference. It is projected verbatim, so the assertions compare bytes.
const itExternalCABundle = "-----BEGIN CERTIFICATE-----\nZW52dGVzdC1jYQ==\n-----END CERTIFICATE-----\n"

// createExternalCABundleSecret provisions the user-supplied CA bundle Secret and
// points the ControlPlane's external block at it. It must run BEFORE the
// ControlPlane is created: reconcileKORC defers with WaitingForCABundle while the
// Secret is absent.
func createExternalCABundleSecret(t testing.TB, ctx context.Context, c client.Client, cp *c5c3v1alpha1.ControlPlane) {
	t.Helper()
	g := NewGomegaWithT(t)

	g.Expect(c.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "external-keystone-ca", Namespace: cp.Namespace},
		Data:       map[string][]byte{"ca.crt": []byte(itExternalCABundle)},
	})).To(Succeed(), "create the user-supplied CA bundle Secret")

	cp.Spec.Services.Keystone.External.CABundleSecretRef = &commonv1.SecretRefSpec{
		Name: "external-keystone-ca", Key: "ca.crt",
	}
}

// TestIntegration_ExternalMode_GeneratedCloudsYAMLTargetsExternalKeystone is the
// headline acceptance criterion of the clouds.yaml work: once an External-mode
// ControlPlane converges, BOTH generated Secrets — the bootstrap password cloud and
// the minted admin app-cred cloud — carry the external authURL, the configured
// endpoint_type, and the CA bundle.
func TestIntegration_ExternalMode_GeneratedCloudsYAMLTargetsExternalKeystone(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupControlPlaneEnvTest(t)
	ensureReadyClusterSecretStore(t, ctx, c)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-cp-external-clouds-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed(), "create test namespace")

	cp := integrationExternalControlPlane("cp-external", ns.Name)
	cp.Spec.Region = "eu-de-1"
	cp.Spec.Services.Keystone.External.EndpointType = c5c3v1alpha1.ExternalEndpointTypeInternal
	createExternalCABundleSecret(t, ctx, c, cp)

	driveExternalControlPlaneToReady(t, ctx, c, cp)
	waitForControlPlaneCondition(t, ctx, c,
		types.NamespacedName{Name: cp.Name, Namespace: ns.Name}, conditionTypeReady, metav1.ConditionTrue, itEventuallyTimeout)

	// The minted app-credential cloud: the document K-ORC authenticates with once
	// the credential exists, and the PushSecret's source for the OpenBao round-trip.
	appCred := &corev1.Secret{}
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: adminAppCredentialSecretName(cp)}, appCred)).To(Succeed())
	appCredDoc := string(appCred.Data[appCredCloudsYAMLKey])
	g.Expect(appCredDoc).To(ContainSubstring(`auth_url: "https://keystone.example.com/v3"`))
	g.Expect(appCredDoc).To(ContainSubstring("endpoint_type: internal"))
	g.Expect(appCredDoc).To(ContainSubstring(`region_name: "eu-de-1"`))
	g.Expect(appCredDoc).To(ContainSubstring("application_credential_id"))
	g.Expect(appCredDoc).NotTo(ContainSubstring(".svc:5000"), "External mode must never dial the Service DNS")
	g.Expect(string(appCred.Data[korcCACertKey])).To(Equal(itExternalCABundle))

	// The bootstrap password cloud: the document the ApplicationCredential mints and
	// revokes with.
	pwCloud := &corev1.Secret{}
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: adminPasswordCloudSecretName(cp)}, pwCloud)).To(Succeed())
	pwDoc := string(pwCloud.Data[appCredCloudsYAMLKey])
	g.Expect(pwDoc).To(ContainSubstring(`auth_url: "https://keystone.example.com/v3"`))
	g.Expect(pwDoc).To(ContainSubstring("endpoint_type: internal"))
	g.Expect(pwDoc).To(ContainSubstring(`region_name: "eu-de-1"`))
	g.Expect(pwDoc).NotTo(ContainSubstring(".svc:5000"))
	g.Expect(string(pwCloud.Data[korcCACertKey])).To(Equal(itExternalCABundle))

	// The clouds.yaml ExternalSecret reads the CA back from the same OpenBao path,
	// so the materialized credentials Secret K-ORC actually reads carries the trust
	// anchor next to clouds.yaml.
	es := &esov1.ExternalSecret{}
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: korcCloudsYamlSecretName}, es)).To(Succeed())
	g.Expect(es.Spec.Data).To(HaveLen(2))
	g.Expect(es.Spec.Data[1].SecretKey).To(Equal(korcCACertKey))
	g.Expect(es.Spec.Data[1].RemoteRef.Property).To(Equal(korcCACertKey))
}

// TestIntegration_ExternalMode_PasswordRotationReMintsAgainstExternalKeystone is
// the credential-lifecycle acceptance criterion: updating the USER-supplied
// admin-password Secret out-of-band re-mints the application credential (delete +
// recreate, because K-ORC's actuator has no in-place re-mint), and the re-assembled
// clouds.yaml — still carrying the external auth_url — lands in the PushSecret
// source and the materialized Secret.
func TestIntegration_ExternalMode_PasswordRotationReMintsAgainstExternalKeystone(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupControlPlaneEnvTest(t)
	ensureReadyClusterSecretStore(t, ctx, c)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-cp-external-remint-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed(), "create test namespace")

	cp := integrationExternalControlPlane("cp-external", ns.Name)
	driveExternalControlPlaneToReady(t, ctx, c, cp)

	cpKey := types.NamespacedName{Name: cp.Name, Namespace: ns.Name}
	acKey := client.ObjectKey{Name: adminAppCredentialName(cp), Namespace: ns.Name}
	appCredKey := client.ObjectKey{Name: adminAppCredentialSecretName(cp), Namespace: ns.Name}

	before := &orcv1alpha1.ApplicationCredential{}
	g.Expect(c.Get(ctx, acKey, before)).To(Succeed())
	beforeSecret := &corev1.Secret{}
	g.Expect(c.Get(ctx, appCredKey, beforeSecret)).To(Succeed())
	beforeValue := string(beforeSecret.Data[appCredSecretValueKey])
	g.Expect(beforeValue).NotTo(BeEmpty())

	// Rotate the admin password OUT-OF-BAND — the only supported rotation path for
	// an external Keystone, since the operator never writes to the installation.
	userSecret := &corev1.Secret{}
	g.Expect(c.Get(ctx, client.ObjectKey{
		Name: cp.Spec.KORC.AdminCredential.PasswordSecretRef.Name, Namespace: ns.Name,
	}, userSecret)).To(Succeed())
	userSecret.Data["password"] = []byte("rotated-admin-password")
	g.Expect(c.Update(ctx, userSecret)).To(Succeed(), "rotate the user-supplied admin password")

	// The hash mismatch drives a delete + recreate, so the AC comes back with a new
	// UID; the secret "value" is regenerated so the recreated AC mints afresh.
	g.Eventually(func() bool {
		after := &orcv1alpha1.ApplicationCredential{}
		if err := c.Get(ctx, acKey, after); err != nil {
			return false
		}
		return after.UID != before.UID
	}, itEventuallyTimeout, itPollInterval).Should(BeTrue(),
		"rotating the admin password must delete and recreate the ApplicationCredential")

	g.Eventually(func() string {
		after := &corev1.Secret{}
		if err := c.Get(ctx, appCredKey, after); err != nil {
			return beforeValue
		}
		return string(after.Data[appCredSecretValueKey])
	}, itEventuallyTimeout, itPollInterval).ShouldNot(Equal(beforeValue),
		"the re-mint must regenerate the application-credential secret value")

	// The password-cloud the re-mint re-authenticates with tracks the rotated
	// password immediately — otherwise K-ORC could not revoke the old credential.
	g.Eventually(func() string {
		pwCloud := &corev1.Secret{}
		if err := c.Get(ctx, client.ObjectKey{Name: adminPasswordCloudSecretName(cp), Namespace: ns.Name}, pwCloud); err != nil {
			return ""
		}
		return string(pwCloud.Data[appCredCloudsYAMLKey])
	}, itEventuallyTimeout, itPollInterval).Should(ContainSubstring("rotated-admin-password"))

	// Simulate K-ORC minting the replacement credential, then pump the ESO
	// simulators until the chain re-converges: the re-assembled clouds.yaml has to
	// reach the PushSecret source AND the materialized Secret before
	// AdminCredentialReady may report True again (the stale-credential gate).
	reminted := &orcv1alpha1.ApplicationCredential{}
	g.Eventually(func() error { return c.Get(ctx, acKey, reminted) }, itEventuallyTimeout, itPollInterval).Should(Succeed())
	reminted.Status.ID = ptr.To("ac-id-integration-remint")
	meta.SetStatusCondition(&reminted.Status.Conditions, metav1.Condition{
		Type:    orcv1alpha1.ConditionAvailable,
		Status:  metav1.ConditionTrue,
		Reason:  orcv1alpha1.ConditionReasonSuccess,
		Message: "simulated available after re-mint",
	})
	g.Expect(c.Status().Update(ctx, reminted)).To(Succeed())

	g.Eventually(func() error {
		return pumpMaterializedCloudsYAML(ctx, c, cp)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
		"the re-assembled app-credential clouds.yaml must reach the materialized Secret")

	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeAdminCredentialReady, metav1.ConditionTrue, itEventuallyTimeout)

	// The re-assembled document carries the FRESH credential and still targets the
	// external Keystone. The materialized Secret — what K-ORC reads — agrees.
	afterSecret := &corev1.Secret{}
	g.Expect(c.Get(ctx, appCredKey, afterSecret)).To(Succeed())
	afterDoc := string(afterSecret.Data[appCredCloudsYAMLKey])
	g.Expect(afterDoc).To(ContainSubstring("ac-id-integration-remint"))
	g.Expect(afterDoc).To(ContainSubstring(`auth_url: "https://keystone.example.com/v3"`))
	g.Expect(afterDoc).To(ContainSubstring("endpoint_type: public"))

	materialized := &corev1.Secret{}
	g.Expect(c.Get(ctx, client.ObjectKey{Name: korcCloudsYamlSecretName, Namespace: ns.Name}, materialized)).To(Succeed())
	g.Expect(string(materialized.Data[appCredCloudsYAMLKey])).To(Equal(afterDoc),
		"the credential K-ORC reads must be the freshly minted one, not the revoked predecessor")

	// The PushSecret carries a new content hash, so ESO re-pushes the rotated
	// credential to OpenBao instead of waiting for the hourly refresh.
	ps := &esov1alpha1.PushSecret{}
	g.Expect(c.Get(ctx, client.ObjectKey{Name: adminAppCredentialPushSecretName(cp), Namespace: ns.Name}, ps)).To(Succeed())
	g.Expect(ps.Annotations[adminAppCredentialPushContentHashAnnotation]).NotTo(BeEmpty())
}

// pumpMaterializedCloudsYAML copies the operator-assembled app-credential
// clouds.yaml into the ESO-materialized Secret, but ONLY once the source actually
// holds a minted credential document. There is no ESO controller in envtest, so a
// re-mint would otherwise leave the materialized Secret pinned at the previous
// credential and the stale-credential gate would (correctly) never open. It returns
// an error while the source is not yet re-assembled, so callers can poll it.
func pumpMaterializedCloudsYAML(ctx context.Context, c client.Client, cp *c5c3v1alpha1.ControlPlane) error {
	src := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: childNamespace(cp), Name: adminAppCredentialSecretName(cp)}, src); err != nil {
		return err
	}
	assembled := src.Data[appCredCloudsYAMLKey]
	if !strings.Contains(string(assembled), "application_credential_id") {
		return fmt.Errorf("app-credential clouds.yaml not re-assembled yet")
	}

	materialized := &corev1.Secret{}
	key := client.ObjectKey{Namespace: childNamespace(cp), Name: korcCloudsYamlSecretName}
	if err := c.Get(ctx, key, materialized); err != nil {
		return err
	}
	if string(materialized.Data[appCredCloudsYAMLKey]) == string(assembled) {
		return nil
	}
	materialized.Data[appCredCloudsYAMLKey] = assembled
	return c.Update(ctx, materialized)
}

// TestIntegration_FederationBackendWakesReconcileAndProjectsWebSSO drives a
// managed ControlPlane to a projected Horizon child, then attaches an OIDC
// KeystoneIdentityBackend and marks it Ready.
//
// It is the load-bearing proof for the identity-backend watch and its field
// index: nothing else touches the ControlPlane after the Horizon child exists,
// so the websso block can only appear if the backend event woke the reconciler
// and listIdentityBackends resolved through the index. It also asserts the
// Ready gate (a not-Ready backend contributes no choice) and the Keystone-side
// trusted_dashboard projection.
func TestIntegration_FederationBackendWakesReconcileAndProjectsWebSSO(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupControlPlaneEnvTest(t)
	ensureReadyClusterSecretStore(t, ctx, c)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-controlplane-sso-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed(), "create test namespace")

	cp := integrationManagedControlPlane("cp", ns.Name)
	cp.Spec.Services.Keystone.PublicEndpoint = "https://keystone.example.com/v3"
	cp.Spec.Services.Horizon = &c5c3v1alpha1.ServiceHorizonSpec{
		PublicEndpoint: "https://horizon.example.com:8443",
	}

	adminSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: adminPasswordSecretName(cp), Namespace: ns.Name},
		Data:       map[string][]byte{"password": []byte("super-secret-admin-password")},
	}
	g.Expect(c.Create(ctx, adminSecret)).To(Succeed(), "create admin password Secret")
	g.Expect(c.Create(ctx, cp)).To(Succeed(), "create ControlPlane CR")
	cpKey := types.NamespacedName{Name: cp.Name, Namespace: ns.Name}

	// Drive the chain far enough for the Horizon child to be projected.
	simulateMariaDBReadyWhenPresent(t, ctx, c, client.ObjectKey{Name: "openstack-db", Namespace: ns.Name})
	simulateMemcachedReadyWhenPresent(t, ctx, c, client.ObjectKey{Name: "openstack-memcached", Namespace: ns.Name})
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeInfrastructureReady, metav1.ConditionTrue, itEventuallyTimeout)

	g.Eventually(func() error {
		return simulators.SimulateExternalSecretSync(ctx, c,
			client.ObjectKey{Namespace: ns.Name, Name: dbCredentialSecretName(cp)})
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "simulate per-CP DB credential ExternalSecret sync")
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeDBCredentialsReady, metav1.ConditionTrue, itEventuallyTimeout)

	simulateAdminPasswordExternalSecretSyncWhenPresent(t, ctx, c, cp)
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeAdminPasswordReady, metav1.ConditionTrue, itEventuallyTimeout)

	keystoneKey := client.ObjectKey{Name: keystoneName(cp), Namespace: ns.Name}
	simulateKeystoneReadyWhenPresent(t, ctx, c, keystoneKey)
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeKeystoneReady, metav1.ConditionTrue, itEventuallyTimeout)

	// The Keystone child carries the dashboard's WebSSO origin — derived
	// top-down from cp.Spec, so it is present before any backend attaches.
	projectedKeystone := &keystonev1alpha1.Keystone{}
	g.Eventually(func() []string {
		if err := c.Get(ctx, keystoneKey, projectedKeystone); err != nil || projectedKeystone.Spec.Federation == nil {
			return nil
		}
		return projectedKeystone.Spec.Federation.TrustedDashboards
	}, itEventuallyTimeout, itPollInterval).Should(Equal([]string{"https://horizon.example.com:8443/auth/websso/"}),
		"trusted_dashboard must carry the dashboard's non-default port verbatim")

	horizonKey := client.ObjectKey{Name: horizonName(cp), Namespace: ns.Name}
	projectedHorizon := &horizonv1alpha1.Horizon{}
	g.Eventually(func() error {
		return c.Get(ctx, horizonKey, projectedHorizon)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "Horizon child should be projected once KeystoneReady")
	g.Expect(projectedHorizon.Spec.WebSSO).To(BeNil(), "no backend attached yet, so no SSO choice")

	// Attach an OIDC backend. It starts NOT Ready, so the login page must not
	// gain an SSO button that dead-ends.
	oidcBackend := &keystonev1alpha1.KeystoneIdentityBackend{
		ObjectMeta: metav1.ObjectMeta{Name: "keycloak", Namespace: ns.Name},
		Spec: keystonev1alpha1.KeystoneIdentityBackendSpec{
			KeystoneRef: keystonev1alpha1.KeystoneRefSpec{Name: keystoneName(cp)},
			// The Default domain hosts the SQL-backed service users and the
			// bootstrap admin, so the CRD forbids backing it with an external
			// identity backend — every federated backend gets its own domain.
			Domain: keystonev1alpha1.DomainSpec{Name: "federated"},
			Type:   keystonev1alpha1.IdentityBackendTypeOIDC,
			OIDC: &keystonev1alpha1.OIDCBackendSpec{
				Issuer:          "https://keycloak.example.com/realms/forge",
				ClientID:        "keystone",
				ClientSecretRef: commonv1.SecretRefSpec{Name: "keycloak-client", Key: "client-secret"},
			},
		},
	}
	g.Expect(c.Create(ctx, oidcBackend)).To(Succeed(), "create OIDC identity backend")

	g.Consistently(func() *horizonv1alpha1.WebSSOSpec {
		h := &horizonv1alpha1.Horizon{}
		if err := c.Get(ctx, horizonKey, h); err != nil {
			return nil
		}
		return h.Spec.WebSSO
	}, 3*time.Second, itPollInterval).Should(BeNil(), "a not-Ready backend must contribute no websso choice")

	// Mark the backend Ready. Nothing else touches the ControlPlane, so the
	// projection below can only happen via the identity-backend watch.
	g.Eventually(func() error {
		fresh := &keystonev1alpha1.KeystoneIdentityBackend{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(oidcBackend), fresh); err != nil {
			return err
		}
		meta.SetStatusCondition(&fresh.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			Reason:             "AllBackendsProjected",
			Message:            "simulated",
			ObservedGeneration: fresh.Generation,
		})
		return c.Status().Update(ctx, fresh)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "mark the OIDC backend Ready")

	g.Eventually(func() *horizonv1alpha1.WebSSOSpec {
		h := &horizonv1alpha1.Horizon{}
		if err := c.Get(ctx, horizonKey, h); err != nil {
			return nil
		}
		return h.Spec.WebSSO
	}, itEventuallyTimeout, itPollInterval).ShouldNot(BeNil(),
		"the identity-backend watch must wake the ControlPlane and project the websso block")

	h := &horizonv1alpha1.Horizon{}
	g.Expect(c.Get(ctx, horizonKey, h)).To(Succeed())
	g.Expect(h.Spec.WebSSO.Enabled).To(BeTrue())
	g.Expect(h.Spec.WebSSO.Choices).To(Equal([]horizonv1alpha1.WebSSOChoice{
		{ID: horizonv1alpha1.DefaultWebSSOLocalChoiceID, Label: horizonv1alpha1.DefaultWebSSOLocalChoiceLabel},
		{ID: "keycloak_openid", Label: "keycloak"},
	}))
	g.Expect(h.Spec.WebSSO.IDPMapping).To(HaveKeyWithValue("keycloak_openid",
		horizonv1alpha1.WebSSOIDPTarget{IdentityProvider: "keycloak", Protocol: "openid"}))
	// The browser follows the SSO redirect, so it must target the external
	// Keystone endpoint — never the cluster-local Service URL.
	g.Expect(h.Spec.WebSSO.KeystoneURL).To(Equal("https://keystone.example.com/v3"))
	g.Expect(h.Spec.KeystoneEndpoint).To(HavePrefix("http://" + keystoneName(cp)))

	// Detaching the backend clears the block, so the SSO button disappears.
	g.Expect(c.Delete(ctx, oidcBackend)).To(Succeed())
	g.Eventually(func() *horizonv1alpha1.WebSSOSpec {
		fresh := &horizonv1alpha1.Horizon{}
		if err := c.Get(ctx, horizonKey, fresh); err != nil {
			return &horizonv1alpha1.WebSSOSpec{}
		}
		return fresh.Spec.WebSSO
	}, itEventuallyTimeout, itPollInterval).Should(BeNil(),
		"detaching the last backend must clear the websso block")
}
