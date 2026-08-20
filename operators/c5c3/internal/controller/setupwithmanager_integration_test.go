// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

// Integration coverage for the real c5c3 SetupWithManager paths. The
// ControlPlane full-chain integration test builds the reconciler inline
// (bypassing SetupWithManager), and nothing exercises the CredentialRotation
// or KeystoneService builders at all, so the twelve ControlPlane Owns
// (including the unstructured Memcached/Certificate GVKs) and both its
// Watches, the CredentialRotation builder, and the KeystoneService chain (ten
// Owns plus the ControlPlane and consumer-Secret Watches) are never executed
// by any test. This test wires the production SetupWithManager methods of all
// three controllers onto an envtest-backed manager and starts it, mirroring
// the main.go wiring, so a regression that drops a watch or crashes the
// manager on a missing kind fails here instead of only in a live cluster.
//
// CONSTRAINT: exactly one test in this package binary may call these real
// SetupWithManager methods. controller-runtime's global controller-name tracker
// rejects a second controller named "controlplane" registered without
// controller.Options{SkipNameValidation}. The inline harness
// (setupControlPlaneEnvTest) sets SkipNameValidation precisely so it does not
// contend with this test. TestBuildControlPlaneController_StartsWithoutServiceCRDs
// below likewise does not contend: it wires the controller through
// buildControlPlaneController on a builder configured with SkipNameValidation —
// the same escape the inline harness uses — instead of calling
// SetupWithManager. TestKeystoneServiceSetup_WatchesConverge takes the same
// escape through setupWithOptions.
package controller

import (
	"context"
	"testing"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	"github.com/c5c3/forge/internal/common/conditions"
	commonmulticluster "github.com/c5c3/forge/internal/common/multicluster"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
	"github.com/c5c3/forge/operators/c5c3/internal/testutil"
)

// TestSetupWithManager_AllControllersStart registers the production
// ControlPlaneReconciler, CredentialRotationReconciler and
// KeystoneServiceReconciler via their real SetupWithManager methods against an
// envtest manager that has every watched CRD installed (c5c3 + keystone CRDs
// plus the shared fake CRDs for MariaDB, Memcached, ESO, cert-manager, and
// K-ORC). The shared skeleton then starts the manager, so every Owns/Watches
// informer — including the unstructured Memcached and Certificate Owns and the
// ClusterSecretStore Watch — must sync against the real API server; a missing
// watched kind would fail mgr.Start.
func TestSetupWithManager_AllControllersStart(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	registered := false
	// The setup helper starts the manager and blocks until the webhook server is
	// ready; if it returns, mgr.Start succeeded and every registered informer
	// synced. A watched CRD that was missing would have failed mgr.Start and the
	// helper would have surfaced the error via t.Errorf.
	_, _, _ = testutil.SetupC5c3EnvTestWithController(
		t,
		c5c3v1alpha1.AddToScheme,
		func(mgr ctrl.Manager) error {
			// mgr.GetAPIReader() mirrors main.go: admission lookups read the API
			// server directly, never a stale cache.
			if err := (&c5c3v1alpha1.ControlPlaneWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr); err != nil {
				return err
			}
			// The webhook manifests installed by envtest carry the KeystoneService
			// entries (failurePolicy=Fail), so the handler must be served here too.
			return (&c5c3v1alpha1.KeystoneServiceWebhook{}).SetupWebhookWithManager(mgr)
		},
		func(mgr ctrl.Manager) error {
			// Mirror operators/c5c3/main.go: all three controllers are registered
			// on the same manager, the ControlPlane one through the multicluster
			// wrapper. A nil provider engages no target cluster, so the remote
			// legs add nothing and every local informer must still sync. That is
			// the single-cluster default path: an operator started with an empty
			// --clusters-namespace clears the provider the same way.
			mcMgr, err := mcmanager.WithMultiCluster(mgr, nil)
			if err != nil {
				return err
			}
			if err := (&ControlPlaneReconciler{
				Client:   mgr.GetClient(),
				Scheme:   mgr.GetScheme(),
				Recorder: mgr.GetEventRecorderFor("controlplane-controller"),
				Resolver: mcMgr,
			}).SetupWithManager(mcMgr); err != nil {
				return err
			}
			if err := (&CredentialRotationReconciler{
				Client:   mgr.GetClient(),
				Scheme:   mgr.GetScheme(),
				Recorder: mgr.GetEventRecorderFor("credentialrotation-controller"),
			}).SetupWithManager(mgr); err != nil {
				return err
			}
			if err := (&KeystoneServiceReconciler{
				Client:   mgr.GetClient(),
				Scheme:   mgr.GetScheme(),
				Recorder: mgr.GetEventRecorderFor("keystoneservice-controller"),
			}).SetupWithManager(mgr); err != nil {
				return err
			}
			registered = true
			return nil
		},
	)

	g.Expect(registered).To(BeTrue(),
		"all three SetupWithManager calls must have completed without error")
}

// TestBuildControlPlaneController_StartsWithoutServiceCRDs is the exact scenario
// that crash-looped before #648: the c5c3-operator installed BEFORE its sibling
// operators, so the Keystone/Horizon/Glance/GlanceBackend/KeystoneIdentityBackend/
// Placement CRDs are unserved when the ControlPlane controller starts. Before the fix the
// controller registered watches for those absent kinds, their informers never
// synced, controller-runtime aborted on the CacheSyncTimeout, and the leader
// crash-looped. With the discovery guard in buildControlPlaneController the
// manager starts with the optional legs skipped and the CR still reconciles.
// K-ORC stays served via the common fake CRDs (BaselineCRDDirectoryPaths) because
// its kinds are Owned unconditionally as hard dependencies — the manager would fail
// to start without them. The discovery guard covers only the sibling service-operator
// legs, which this baseline leaves unserved.
func TestBuildControlPlaneController_StartsWithoutServiceCRDs(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	// The setup helper only returns once mgr.Start succeeded and every registered
	// informer synced. A regression that re-registered a watch for one of the
	// unserved Keystone/Horizon/Glance/GlanceBackend/KeystoneIdentityBackend/Placement
	// kinds would leave that informer stuck, fail mgr.Start on the CacheSyncTimeout, and
	// the shared harness (internal/common/testutil/envtest/manager.go) would surface
	// the error via t.Errorf — so the crash-loop regression fails this test either
	// way. The controller is wired through buildControlPlaneController (the same
	// builder the production SetupWithManager drives) on a builder carrying
	// SkipNameValidation, so it does not contend with
	// TestSetupWithManager_BothControllersStart for the global controller name.
	c, ctx, _ := testutil.SetupC5c3EnvTestWithControllerAndCRDs(
		t,
		testutil.BaselineCRDDirectoryPaths(),
		c5c3v1alpha1.AddToScheme,
		func(mgr ctrl.Manager) error {
			// mgr.GetAPIReader() mirrors main.go: admission lookups read the API
			// server directly, never a stale cache.
			if err := (&c5c3v1alpha1.ControlPlaneWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr); err != nil {
				return err
			}
			// The webhook manifests installed by envtest carry the KeystoneService
			// entries (failurePolicy=Fail), so the handler must be served here too.
			return (&c5c3v1alpha1.KeystoneServiceWebhook{}).SetupWebhookWithManager(mgr)
		},
		func(mgr ctrl.Manager) error {
			r := &ControlPlaneReconciler{
				Client:   mgr.GetClient(),
				Scheme:   mgr.GetScheme(),
				Recorder: mgr.GetEventRecorderFor("controlplane-controller"),
			}
			// A nil provider engages no target cluster, so the remote legs add
			// nothing here and only the local ones have to sync — which is what
			// this test is about.
			mcMgr, err := mcmanager.WithMultiCluster(mgr, nil)
			if err != nil {
				return err
			}
			b, err := r.buildControlPlaneController(mcMgr, mcbuilder.ControllerManagedBy(mcMgr).
				WithOptions(controller.TypedOptions[mcreconcile.Request]{SkipNameValidation: ptr.To(true)}))
			if err != nil {
				return err
			}
			return b.WithClusterNotFoundWrapper(false).Complete(commonmulticluster.LocalReconciler(r))
		},
	)

	// Positive reconcile signal: create a minimal ControlPlane and wait for the
	// controller to install controlPlaneORCFinalizer. Reconcile adds that finalizer
	// (controlplane_controller.go, before the sub-reconciler pipeline runs), so its
	// appearance proves the controller both started and reconciles despite the
	// service CRDs being absent.
	cp := integrationMinimalControlPlane("baseline-cp", "default")
	g.Expect(c.Create(ctx, cp)).To(Succeed())

	g.Eventually(func() ([]string, error) {
		var got c5c3v1alpha1.ControlPlane
		if err := c.Get(ctx, client.ObjectKeyFromObject(cp), &got); err != nil {
			return nil, err
		}
		return got.Finalizers, nil
	}, itEventuallyTimeout, itPollInterval).Should(ContainElement(controlPlaneORCFinalizer),
		"Reconcile must install the ORC-teardown finalizer even with the service CRDs unserved")
}

// TestKeystoneServiceSetup_WatchesConverge drives the KeystoneService wiring end
// to end against a real API server: only the KeystoneService chain is
// registered (through setupWithOptions with SkipNameValidation, so it does not
// contend with TestSetupWithManager_AllControllersStart for the controller
// name), and a catalog-only registration walks the gate arc a GitOps apply
// takes — dangling reference, plane created, admin credential ready. No
// ControlPlane controller runs here, so the hand-written AdminCredentialReady
// status patch is never overwritten. The 10-second korcRequeueAfter would also
// converge each step, so this asserts behavior, not watch latency; the
// mappers' exactness is pinned by their unit tests.
func TestKeystoneServiceSetup_WatchesConverge(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := testutil.SetupC5c3EnvTestWithController(
		t,
		c5c3v1alpha1.AddToScheme,
		func(mgr ctrl.Manager) error {
			// mgr.GetAPIReader() mirrors main.go: admission lookups read the API
			// server directly, never a stale cache.
			if err := (&c5c3v1alpha1.ControlPlaneWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr); err != nil {
				return err
			}
			// The webhook manifests installed by envtest carry the KeystoneService
			// entries (failurePolicy=Fail), so the handler must be served here too.
			return (&c5c3v1alpha1.KeystoneServiceWebhook{}).SetupWebhookWithManager(mgr)
		},
		func(mgr ctrl.Manager) error {
			r := &KeystoneServiceReconciler{
				Client:   mgr.GetClient(),
				Scheme:   mgr.GetScheme(),
				Recorder: mgr.GetEventRecorderFor("keystoneservice-controller"),
			}
			return r.setupWithOptions(mgr, controller.Options{SkipNameValidation: ptr.To(true)})
		},
	)

	// A catalog-only registration (the minimal admissible spec, mirroring
	// ksCatalogSpec) referencing a plane that does not exist yet, in the same
	// namespace the plane will take: the gate admits own-namespace CRs only.
	ks := &c5c3v1alpha1.KeystoneService{
		ObjectMeta: metav1.ObjectMeta{Name: "registration-864", Namespace: "default"},
		Spec: c5c3v1alpha1.KeystoneServiceSpec{
			ControlPlaneRef: c5c3v1alpha1.ControlPlaneRefSpec{Name: "cp-864"},
			Catalog:         &c5c3v1alpha1.KeystoneServiceCatalogSpec{ServiceType: "image"},
		},
	}
	g.Expect(c.Create(ctx, ks)).To(Succeed())

	// catalogReason reads the CatalogReady reason once the finalizer is
	// installed; both are written by the same reconcile pass sequence, and the
	// finalizer guard keeps the assertion from racing the very first pass.
	catalogReason := func() (string, error) {
		var got c5c3v1alpha1.KeystoneService
		if err := c.Get(ctx, client.ObjectKeyFromObject(ks), &got); err != nil {
			return "", err
		}
		if !controllerutil.ContainsFinalizer(&got, keystoneServiceFinalizerName) {
			return "", nil
		}
		cond := conditions.GetCondition(got.Status.Conditions, conditionTypeKeystoneServiceCatalogReady)
		if cond == nil || cond.Status != metav1.ConditionFalse {
			return "", nil
		}
		return cond.Reason, nil
	}

	g.Eventually(catalogReason, itEventuallyTimeout, itPollInterval).
		Should(Equal(reasonKeystoneServiceControlPlaneNotFound),
			"a dangling controlPlaneRef must surface as ControlPlaneNotFound behind the teardown finalizer")

	// Creating the plane must move the registration to the next gate: the CR
	// itself is untouched, so the ControlPlane watch is what carries the event.
	cp := integrationMinimalControlPlane("cp-864", "default")
	g.Expect(c.Create(ctx, cp)).To(Succeed())

	g.Eventually(catalogReason, itEventuallyTimeout, itPollInterval).
		Should(Equal(reasonWaitingForServiceAccountAdmin),
			"creating the referenced ControlPlane must re-drive the registration to the admin-credential gate")

	// Flip AdminCredentialReady on the plane's status — the wake signal the
	// ControlPlane watch exists for. The registration must run past the shared
	// gates and apply the catalog collision probe.
	g.Eventually(func() error {
		var got c5c3v1alpha1.ControlPlane
		if err := c.Get(ctx, client.ObjectKeyFromObject(cp), &got); err != nil {
			return err
		}
		setAdminCredentialReady(&got)
		return c.Status().Update(ctx, &got)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed())

	g.Eventually(func() error {
		var probe orcv1alpha1.Service
		return c.Get(ctx, client.ObjectKey{Namespace: ks.Namespace, Name: keystoneServiceCatalogServiceProbeRef(ks)}, &probe)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
		"the AdminCredentialReady status flip must re-drive the registration into the catalog collision probe")
}

// TestControlPlaneSetup_ProjectedChildStatusReEnqueues drives the two legs the
// projected registration children ride, over the production ControlPlane chain
// (buildControlPlaneController on a builder carrying SkipNameValidation, so it
// does not contend with TestSetupWithManager_AllControllersStart for the
// controller name).
//
// Glance holds its projection until the KeystoneService child it projects reports
// AccountReady, and the child's STATUS is the only thing that moves that gate. No
// KeystoneService controller runs here, so the test writes that status itself and
// then reads the relayed reason and message back off the ControlPlane's
// GlanceReady condition — which is exactly what a status-only child update has to
// re-enqueue the ControlPlane for.
//
// Both subtests walk the same arc on the two ownership shapes: a child co-located
// with the ControlPlane carries a controller owner reference and rides the Owns
// leg, while one placed in a namespace of its own can carry none and rides the
// labelled cross-namespace leg.
//
// The gate requeues on korcRequeueAfter, which would converge each relay on its
// own, so this asserts behavior rather than watch latency; the mapper and the
// label predicate are pinned by their unit tests.
func TestControlPlaneSetup_ProjectedChildStatusReEnqueues(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := testutil.SetupC5c3EnvTestWithController(
		t,
		c5c3v1alpha1.AddToScheme,
		func(mgr ctrl.Manager) error {
			// mgr.GetAPIReader() mirrors main.go: admission lookups read the API
			// server directly, never a stale cache.
			if err := (&c5c3v1alpha1.ControlPlaneWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr); err != nil {
				return err
			}
			// The webhook manifests installed by envtest carry the KeystoneService
			// entries (failurePolicy=Fail), so the handler must be served here too.
			return (&c5c3v1alpha1.KeystoneServiceWebhook{}).SetupWebhookWithManager(mgr)
		},
		func(mgr ctrl.Manager) error {
			r := &ControlPlaneReconciler{
				Client:   mgr.GetClient(),
				Scheme:   mgr.GetScheme(),
				Recorder: mgr.GetEventRecorderFor("controlplane-controller"),
			}
			// A nil provider engages no target cluster, so every service stays on
			// the management cluster and only the local legs are registered.
			mcMgr, err := mcmanager.WithMultiCluster(mgr, nil)
			if err != nil {
				return err
			}
			b, err := r.buildControlPlaneController(mcMgr, mcbuilder.ControllerManagedBy(mcMgr).
				WithOptions(controller.TypedOptions[mcreconcile.Request]{SkipNameValidation: ptr.To(true)}))
			if err != nil {
				return err
			}
			return b.WithClusterNotFoundWrapper(false).Complete(commonmulticluster.LocalReconciler(r))
		},
	)
	ensureReadyClusterSecretStore(t, ctx, c)

	t.Run("co-located child", func(t *testing.T) {
		g := NewGomegaWithT(t)

		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-owned-child-"}}
		g.Expect(c.Create(ctx, ns)).To(Succeed(), "create the ControlPlane namespace")

		cp := integrationManagedControlPlane("cp", ns.Name)
		cp.Spec.Services.Glance = integrationGlanceService()
		g.Expect(c.Create(ctx, cp)).To(Succeed(), "create the ControlPlane CR")
		driveControlPlaneToAdminCredentialReady(t, ctx, c, cp)

		child := waitForGlanceRegistrationGate(t, ctx, c, cp)
		g.Expect(child.Namespace).To(Equal(cp.Namespace))
		g.Expect(metav1.GetControllerOf(child)).NotTo(BeNil(),
			"a co-located child is owned by controller reference, which is what the Owns leg matches")

		relayChildAccountCondition(t, ctx, c, cp, child,
			reasonServiceAccountCollision, `user "glance" already exists in Keystone`)
	})

	t.Run("dedicated-namespace child", func(t *testing.T) {
		g := NewGomegaWithT(t)

		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-labelled-child-"}}
		g.Expect(c.Create(ctx, ns)).To(Succeed(), "create the ControlPlane namespace")
		imagesNS := ns.Name + "-images"

		cp := integrationManagedControlPlane("cp", ns.Name)
		cp.Spec.Services.Glance = integrationGlanceService()
		cp.Spec.Services.Glance.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
			Name:      imagesNS,
			Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
		}
		g.Expect(c.Create(ctx, cp)).To(Succeed(), "create the ControlPlane CR")

		// The image namespace carries a second set of the gates the drive helper
		// below opens at home: the backing services there are as load-bearing for
		// InfrastructureReady as the ones in the ControlPlane's own namespace, and
		// that namespace's tenant store holds the pipeline just as the one at home
		// does. The helper waits on the pipeline as a whole, so both gates are
		// opened here, before it runs. Every instance is served first, the ones at
		// home included (which the helper then serves again), because the tenant
		// stores are written only once InfrastructureReady is True.
		for _, namespace := range []string{imagesNS, ns.Name} {
			simulateMariaDBReadyWhenPresent(t, ctx, c,
				client.ObjectKey{Name: c5c3v1alpha1.DefaultDatabaseClusterRefName, Namespace: namespace})
			simulateMemcachedReadyWhenPresent(t, ctx, c,
				client.ObjectKey{Name: c5c3v1alpha1.DefaultCacheClusterRefName, Namespace: namespace})
		}
		// The image namespace's store is waited for rather than pre-created: the
		// operator refuses to adopt an object it did not write itself in a namespace
		// outside its own, so a pre-created store would fail that step rather than
		// open it.
		g.Eventually(func() error {
			return c.Get(ctx, client.ObjectKey{Name: esoTenantStoreName, Namespace: imagesNS}, &esov1.SecretStore{})
		}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
			"the operator must provision a tenant store in the image namespace")
		ensureReadySecretStore(t, ctx, c, esoTenantStoreName, imagesNS)

		driveControlPlaneToAdminCredentialReady(t, ctx, c, cp)

		child := waitForGlanceRegistrationGate(t, ctx, c, cp)
		g.Expect(child.Namespace).To(Equal(imagesNS))
		g.Expect(metav1.GetControllerOf(child)).To(BeNil(),
			"the API server rejects a cross-namespace controller owner reference")
		g.Expect(child.Labels).To(HaveKeyWithValue(controlPlaneNameLabel, cp.Name),
			"the ownership labels are what the cross-namespace leg matches")
		g.Expect(child.Labels).To(HaveKeyWithValue(controlPlaneNamespaceLabel, cp.Namespace))

		relayChildAccountCondition(t, ctx, c, cp, child,
			reasonServiceAccountStoreNotReady, "upstream secret backend unreachable")
	})
}

// waitForGlanceRegistrationGate waits until the Glance stage holds at the
// registration gate and returns the KeystoneService child it projected. The gate
// is where the ControlPlane stops on its own: the child exists, and nothing has
// written the AccountReady condition it reads.
func waitForGlanceRegistrationGate(
	t testing.TB, ctx context.Context, c client.Client, cp *c5c3v1alpha1.ControlPlane,
) *c5c3v1alpha1.KeystoneService {
	t.Helper()
	g := NewGomegaWithT(t)

	g.Eventually(func() string {
		return glanceReadyCondition(ctx, c, cp).Reason
	}, itEventuallyTimeout, itPollInterval).Should(Equal(reasonWaitingForServiceRegistration),
		"Glance must hold at the registration gate while the child reports no AccountReady")

	child := &c5c3v1alpha1.KeystoneService{}
	g.Expect(c.Get(ctx, client.ObjectKey{
		Name: glanceName(cp), Namespace: cp.GlanceNamespace(),
	}, child)).To(Succeed(), "the Glance registration child must be projected")
	return child
}

// relayChildAccountCondition writes reason and message onto the child's
// AccountReady condition and waits for the ControlPlane to relay both onto
// GlanceReady. The write touches the status subresource alone, so the event that
// carries it is the one the ownership legs deliver and the registration mapper
// leg's predicate drops.
//
// The write is retried: the reconciler re-applies the registration's spec on
// every pass, so the resourceVersion read here can be stale by the time the
// status update lands.
func relayChildAccountCondition(
	t testing.TB, ctx context.Context, c client.Client,
	cp *c5c3v1alpha1.ControlPlane, child *c5c3v1alpha1.KeystoneService, reason, message string,
) {
	t.Helper()
	g := NewGomegaWithT(t)

	g.Eventually(func() error {
		live := &c5c3v1alpha1.KeystoneService{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(child), live); err != nil {
			return err
		}
		conditions.SetCondition(&live.Status.Conditions, metav1.Condition{
			Type:               conditionTypeKeystoneServiceAccountReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: live.Generation,
			Reason:             reason,
			Message:            message,
		})
		return c.Status().Update(ctx, live)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
		"write the child's AccountReady condition")

	g.Eventually(func() string {
		return glanceReadyCondition(ctx, c, cp).Message
	}, itEventuallyTimeout, itPollInterval).Should(And(ContainSubstring(reason), ContainSubstring(message)),
		"the child's status-only update must re-enqueue the ControlPlane, which relays the child's own reason and message")
}

// glanceReadyCondition reads the ControlPlane's GlanceReady condition, answering
// with the zero condition while the CR is unreadable or the condition unwritten,
// so it composes with Eventually.
func glanceReadyCondition(ctx context.Context, c client.Client, cp *c5c3v1alpha1.ControlPlane) metav1.Condition {
	live := &c5c3v1alpha1.ControlPlane{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(cp), live); err != nil {
		return metav1.Condition{}
	}
	if cond := conditions.GetCondition(live.Status.Conditions, conditionTypeGlanceReady); cond != nil {
		return *cond
	}
	return metav1.Condition{}
}
