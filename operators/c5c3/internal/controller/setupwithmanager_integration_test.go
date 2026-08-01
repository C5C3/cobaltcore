// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

// Integration coverage for both real c5c3 SetupWithManager paths. The
// ControlPlane full-chain integration test builds the reconciler inline
// (bypassing SetupWithManager), and nothing exercises
// CredentialRotationReconciler.SetupWithManager at all, so the twelve
// ControlPlane Owns (including the unstructured Memcached/Certificate GVKs) and
// both Watches, plus the CredentialRotation builder, are never executed by any
// test. This test wires the production SetupWithManager methods of both
// controllers onto an envtest-backed manager and starts it, mirroring the
// main.go wiring, so a regression that drops a watch or crashes the manager on
// a missing kind fails here instead of only in a live cluster.
//
// CONSTRAINT: exactly one test in this package binary may call these real
// SetupWithManager methods. controller-runtime's global controller-name tracker
// rejects a second controller named "controlplane" registered without
// controller.Options{SkipNameValidation}. The inline harness
// (setupControlPlaneEnvTest) sets SkipNameValidation precisely so it does not
// contend with this test. TestBuildControlPlaneController_StartsWithoutServiceCRDs
// below likewise does not contend: it wires the controller through
// buildControlPlaneController on a builder configured with
// controller.Options{SkipNameValidation: ptr.To(true)} — the same escape the
// inline harness uses — instead of calling SetupWithManager.
package controller

import (
	"testing"

	. "github.com/onsi/gomega"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"

	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
	"github.com/c5c3/forge/operators/c5c3/internal/testutil"
)

// TestSetupWithManager_BothControllersStart registers the production
// ControlPlaneReconciler and CredentialRotationReconciler via their real
// SetupWithManager methods against an envtest manager that has every watched
// CRD installed (c5c3 + keystone CRDs plus the shared fake CRDs for MariaDB,
// Memcached, ESO, cert-manager, and K-ORC). The shared skeleton then starts the
// manager, so every Owns/Watches informer — including the unstructured
// Memcached and Certificate Owns and the ClusterSecretStore Watch — must sync
// against the real API server; a missing watched kind would fail mgr.Start.
func TestSetupWithManager_BothControllersStart(t *testing.T) {
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
			return (&c5c3v1alpha1.ControlPlaneWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr)
		},
		func(mgr ctrl.Manager) error {
			// Mirror operators/c5c3/main.go: both controllers are registered on the
			// same manager.
			if err := (&ControlPlaneReconciler{
				Client:   mgr.GetClient(),
				Scheme:   mgr.GetScheme(),
				Recorder: mgr.GetEventRecorderFor("controlplane-controller"),
			}).SetupWithManager(mgr); err != nil {
				return err
			}
			if err := (&CredentialRotationReconciler{
				Client:   mgr.GetClient(),
				Scheme:   mgr.GetScheme(),
				Recorder: mgr.GetEventRecorderFor("credentialrotation-controller"),
			}).SetupWithManager(mgr); err != nil {
				return err
			}
			registered = true
			return nil
		},
	)

	g.Expect(registered).To(BeTrue(),
		"both SetupWithManager calls must have completed without error")
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
	// controller.Options{SkipNameValidation: ptr.To(true)}, so it does not contend
	// with TestSetupWithManager_BothControllersStart for the global controller name.
	c, ctx, _ := testutil.SetupC5c3EnvTestWithControllerAndCRDs(
		t,
		testutil.BaselineCRDDirectoryPaths(),
		c5c3v1alpha1.AddToScheme,
		func(mgr ctrl.Manager) error {
			// mgr.GetAPIReader() mirrors main.go: admission lookups read the API
			// server directly, never a stale cache.
			return (&c5c3v1alpha1.ControlPlaneWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr)
		},
		func(mgr ctrl.Manager) error {
			r := &ControlPlaneReconciler{
				Client:   mgr.GetClient(),
				Scheme:   mgr.GetScheme(),
				Recorder: mgr.GetEventRecorderFor("controlplane-controller"),
			}
			b, err := r.buildControlPlaneController(mgr, ctrl.NewControllerManagedBy(mgr).
				WithOptions(controller.Options{SkipNameValidation: ptr.To(true)}))
			if err != nil {
				return err
			}
			return b.Complete(r)
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
