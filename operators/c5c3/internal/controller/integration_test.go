// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

// Package controller contains the envtest integration test for the ControlPlane
// reconciler (CC-0110, REQ-027). Unlike the fake-client unit tests, this test
// runs the reconciler inside a real controller-runtime manager against a live
// envtest API server (CRDs + validating/defaulting webhook), and drives the full
// sub-reconciler chain — Infrastructure -> Keystone -> KORC -> AdminCredential ->
// Catalog — to the aggregate Ready=True by simulating each external dependency's
// readiness exactly as the production operators would report it.
package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	"github.com/c5c3/forge/internal/common/testutil/simulators"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
	"github.com/c5c3/forge/operators/c5c3/internal/testutil"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// Feature: CC-0110, REQ-027

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
// DECISION (CC-0110, REQ-027): the controller is registered via an inline
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
	return testutil.SetupC5c3EnvTestWithController(t,
		c5c3v1alpha1.AddToScheme,
		func(mgr ctrl.Manager) error {
			return (&c5c3v1alpha1.ControlPlaneWebhook{Client: mgr.GetClient()}).SetupWebhookWithManager(mgr)
		},
		func(mgr ctrl.Manager) error {
			r := &ControlPlaneReconciler{
				Client:   mgr.GetClient(),
				Scheme:   mgr.GetScheme(),
				Recorder: mgr.GetEventRecorderFor("controlplane-controller"),
			}
			// Register the ControlPlane secret-name field indexer so
			// secretToControlPlaneMapper's MatchingFields lookup works, mirroring
			// what SetupWithManager does in production (CC-0110, REQ-012).
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
			Infrastructure: c5c3v1alpha1.InfrastructureSpec{
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
				Keystone: c5c3v1alpha1.ServiceKeystoneSpec{
					Replicas: ptr.To(int32(3)),
				},
			},
			// One global oslo.policy override so the test can assert the reconciler
			// merges it into the projected Keystone CR's PolicyOverrides (REQ-027).
			Global: &commonv1.PolicySpec{
				Rules: map[string]string{"identity:list_users": "role:admin"},
			},
			KORC: c5c3v1alpha1.KORCSpec{
				AdminCredential: c5c3v1alpha1.AdminCredentialSpec{
					CloudCredentialsRef: c5c3v1alpha1.CloudCredentialsRef{
						CloudName: "admin",
					},
					PasswordSecretRef: commonv1.SecretRefSpec{Name: "keystone-admin", Key: "password"},
				},
			},
		},
	}
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

// TestIntegration_FullReconcile_ManagedToReady drives a managed-mode ControlPlane
// through every sub-reconciler to the aggregate Ready=True, simulating each
// external dependency's readiness in dependency order. It is the single primary
// end-to-end test for the ControlPlane reconciler (CC-0110, REQ-027).
func TestIntegration_FullReconcile_ManagedToReady(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupControlPlaneEnvTest(t)

	// Isolated test namespace per run (namespace-per-test with GenerateName).
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-controlplane-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed(), "create test namespace")

	// Admin password Secret the KORC sub-reconciler hashes to drive the mint
	// (read from cp.Namespace, key "password").
	adminSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "keystone-admin", Namespace: ns.Name},
		Data:       map[string][]byte{"password": []byte("super-secret-admin-password")},
	}
	g.Expect(c.Create(ctx, adminSecret)).To(Succeed(), "create admin password Secret")

	// The K-ORC clouds.yaml ExternalSecret AdminCredentialReady gates on. Per the
	// C1 co-location fix it lives in the SAME namespace as the K-ORC resource CRs
	// (the ControlPlane namespace), NOT a fixed orc-system one, because K-ORC
	// resolves CloudCredentialsRef in the resource's own namespace. Create it and
	// simulate its ESO sync so WaitForExternalSecret reports Ready. On a live
	// cluster this Secret is seeded with a password-based bootstrap clouds.yaml by
	// write-bootstrap-secrets.sh and later overwritten by the operator's PushSecret.
	cloudsYamlES := &esov1.ExternalSecret{
		ObjectMeta: metav1.ObjectMeta{Name: korcCloudsYamlSecretName, Namespace: ns.Name},
		Spec: esov1.ExternalSecretSpec{
			SecretStoreRef: esov1.SecretStoreRef{Kind: "ClusterSecretStore", Name: "openbao-cluster-store"},
			Target:         esov1.ExternalSecretTarget{Name: korcCloudsYamlSecretName},
		},
	}
	g.Expect(c.Create(ctx, cloudsYamlES)).To(Succeed(), "create k-orc clouds.yaml ExternalSecret")
	g.Expect(simulators.SimulateExternalSecretSync(ctx, c,
		client.ObjectKey{Namespace: ns.Name, Name: korcCloudsYamlSecretName})).
		To(Succeed(), "simulate k-orc clouds.yaml ExternalSecret sync")

	// Create the ControlPlane CR (the defaulting webhook fills region etc.).
	cp := integrationManagedControlPlane("cp", ns.Name)
	g.Expect(c.Create(ctx, cp)).To(Succeed(), "create ControlPlane CR")
	cpKey := types.NamespacedName{Name: cp.Name, Namespace: ns.Name}

	// --- Phase 1: Infrastructure (MariaDB + Memcached). ---
	simulateMariaDBReadyWhenPresent(t, ctx, c, client.ObjectKey{Name: "openstack-db", Namespace: ns.Name})
	simulateMemcachedReadyWhenPresent(t, ctx, c, client.ObjectKey{Name: "openstack-memcached", Namespace: ns.Name})
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeInfrastructureReady, metav1.ConditionTrue, itEventuallyTimeout)

	// --- Phase 2: Keystone child. ---
	simulateKeystoneReadyWhenPresent(t, ctx, c, client.ObjectKey{Name: keystoneName(cp), Namespace: ns.Name})
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeKeystoneReady, metav1.ConditionTrue, itEventuallyTimeout)

	// --- Phase 3: K-ORC admin ApplicationCredential. ---
	simulateApplicationCredentialAvailableWhenPresent(t, ctx, c,
		client.ObjectKey{Name: adminAppCredentialName(cp), Namespace: ns.Name})
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeKORCReady, metav1.ConditionTrue, itEventuallyTimeout)

	// --- Phase 4: AdminCredential push (gated on the clouds.yaml ES, already
	// synced above). The owned minted-credential Secret and the OpenBao
	// PushSecret are not status-gated, so AdminCredentialReady flips once the
	// gate passes. ---
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeAdminCredentialReady, metav1.ConditionTrue, itEventuallyTimeout)

	// --- Phase 5: Catalog (Service + Endpoint). Not status-gated. ---
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeCatalogReady, metav1.ConditionTrue, itEventuallyTimeout)

	// --- Aggregate: Ready=True. ---
	waitForControlPlaneCondition(t, ctx, c, cpKey, conditionTypeReady, metav1.ConditionTrue, itEventuallyTimeout)

	// Final assertions on the converged CR.
	final := &c5c3v1alpha1.ControlPlane{}
	g.Expect(c.Get(ctx, cpKey, final)).To(Succeed(), "get converged ControlPlane")

	for _, condType := range []string{
		conditionTypeInfrastructureReady,
		conditionTypeKeystoneReady,
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

	// Every condition records the generation it was observed against (REQ-007).
	for _, cond := range final.Status.Conditions {
		g.Expect(cond.ObservedGeneration).To(Equal(final.Generation),
			"condition %s ObservedGeneration should match CR generation", cond.Type)
	}

	// The reflected admin application-credential status mirrors the simulated AC.
	g.Expect(final.Status.AdminApplicationCredential).NotTo(BeNil(),
		"status.adminApplicationCredential should be populated")
	g.Expect(final.Status.AdminApplicationCredential.ID).To(Equal("ac-id-integration"))
	g.Expect(final.Status.CatalogReady).To(BeTrue(), "status.catalogReady should be true")

	// --- Intermediate projected specs (REQ-027, TE7b). Asserting only the final
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
	g.Expect(ac.Spec.CloudCredentialsRef.SecretName).To(Equal(korcCloudsYamlSecretName))

	// Catalog: identity Service + public Endpoint shape.
	svc := &orcv1alpha1.Service{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: keystoneServiceName(final), Namespace: ns.Name}, svc)).
		To(Succeed(), "get projected identity Service CR")
	g.Expect(svc.Spec.Resource).NotTo(BeNil())
	g.Expect(svc.Spec.Resource.Type).To(Equal("identity"), "Service type must be identity")

	ep := &orcv1alpha1.Endpoint{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: keystoneEndpointName(final), Namespace: ns.Name}, ep)).
		To(Succeed(), "get projected identity Endpoint CR")
	g.Expect(ep.Spec.Resource).NotTo(BeNil())
	g.Expect(ep.Spec.Resource.Interface).To(Equal("public"), "Endpoint interface must be public")
	g.Expect(string(ep.Spec.Resource.ServiceRef)).To(Equal(keystoneServiceName(final)),
		"Endpoint serviceRef must reference the identity Service CR")
	g.Expect(ep.Spec.Resource.URL).NotTo(BeEmpty(), "Endpoint URL must be derived")
}
