// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

// Integration coverage for the real SetupWithManager methods of the Cinder,
// CinderBackend and CinderBackupBackend reconcilers. The inline harness
// (registerCinderControllers) registers the production chains through
// setupWithOptions with SkipNameValidation set, so the probes, indexes and legs
// already run under envtest; what only this test runs is the public wrapper:
// the production bootstrap controller options and the registration under the
// real controller names. This test wires all three SetupWithManager methods
// onto an envtest-backed manager as main.go does, starts it, and waits for each
// controller's first status write.
//
// CONSTRAINT: exactly one test in this package binary may call these real
// SetupWithManager methods. controller-runtime's global controller-name tracker
// rejects a second controller named "cinder", "cinderbackend" or
// "cinderbackupbackend" registered without
// controller.Options{SkipNameValidation}. The inline harness
// (registerCinderControllers) sets SkipNameValidation precisely so it does not
// contend with this test, and multicluster_integration_test.go registers only
// the kubeconfig provider under its own fixed name. A future second
// real-SetupWithManager test would need its own process or the same skip.
package controller

import (
	"net/http"
	"testing"

	. "github.com/onsi/gomega"
	ctrl "sigs.k8s.io/controller-runtime"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"

	commonenvtest "github.com/c5c3/cobaltcore/internal/common/testutil/envtest"
	cinderv1alpha1 "github.com/c5c3/cobaltcore/operators/cinder/api/v1alpha1"
	"github.com/c5c3/cobaltcore/operators/cinder/internal/testutil"
)

// TestSetupWithManager_AllControllersStart registers the production
// CinderReconciler, CinderBackendReconciler and CinderBackupBackendReconciler
// via their real SetupWithManager methods against an envtest manager that
// serves every watched CRD (the operator CRDs plus the shared fake CRDs),
// asserts the RESTMapper probes latched, and then waits for each controller to
// stamp status.observedGeneration on a CR of its kind.
func TestSetupWithManager_AllControllersStart(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	var (
		cinder        *CinderReconciler
		backend       *CinderBackendReconciler
		backupBackend *CinderBackupBackendReconciler
	)
	c, ctx, _ := testutil.SetupCinderEnvTestWithController(
		t,
		cinderv1alpha1.AddToScheme,
		func(mgr ctrl.Manager) error {
			// mgr.GetAPIReader() mirrors main.go: admission lookups read the API
			// server directly, never a stale cache. The webhook manifests envtest
			// installs carry all three kinds (failurePolicy=Fail), so every
			// handler must be served.
			if err := (&cinderv1alpha1.CinderWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr); err != nil {
				return err
			}
			if err := (&cinderv1alpha1.CinderBackendWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr); err != nil {
				return err
			}
			return (&cinderv1alpha1.CinderBackupBackendWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr)
		},
		func(mgr ctrl.Manager) error {
			// A nil provider engages no remote cluster, so the remote legs add
			// nothing and every local informer must still sync. That is the
			// single-cluster default path: an operator started with an empty
			// --clusters-namespace clears the provider the same way.
			mcMgr, err := mcmanager.WithMultiCluster(mgr, nil)
			if err != nil {
				return err
			}
			// The Cinder reconciler is registered first, as in main.go: its
			// SetupWithManager is the single registration site for the field
			// indexes all three controllers use.
			cinder = &CinderReconciler{
				Client:                  mgr.GetClient(),
				Scheme:                  mgr.GetScheme(),
				Recorder:                mgr.GetEventRecorderFor("cinder-controller"), //nolint:staticcheck // SA1019: reconciler consumes record.EventRecorder (old events API); GetEventRecorder returns the incompatible events/v1 type.
				MaxConcurrentReconciles: 1,
				Resolver:                mcMgr,
				// The healthy stub keeps the health-check probe from firing HTTP
				// GETs at a Service DNS name envtest cannot resolve.
				HTTPClient: &stubDoer{status: http.StatusOK},
			}
			if err := cinder.SetupWithManager(mcMgr); err != nil {
				return err
			}
			// The satellites take the local manager, as in main.go.
			backend = &CinderBackendReconciler{
				Client:                  mgr.GetClient(),
				Scheme:                  mgr.GetScheme(),
				Recorder:                mgr.GetEventRecorderFor("cinderbackend-controller"), //nolint:staticcheck // SA1019: reconciler consumes record.EventRecorder (old events API); GetEventRecorder returns the incompatible events/v1 type.
				MaxConcurrentReconciles: 1,
				Resolver:                mcMgr,
			}
			if err := backend.SetupWithManager(mgr); err != nil {
				return err
			}
			backupBackend = &CinderBackupBackendReconciler{
				Client:                  mgr.GetClient(),
				Scheme:                  mgr.GetScheme(),
				Recorder:                mgr.GetEventRecorderFor("cinderbackupbackend-controller"), //nolint:staticcheck // SA1019: reconciler consumes record.EventRecorder (old events API); GetEventRecorder returns the incompatible events/v1 type.
				MaxConcurrentReconciles: 1,
				Resolver:                mcMgr,
			}
			return backupBackend.SetupWithManager(mgr)
		},
	)

	g.Expect(cinder).NotTo(BeNil(),
		"registerController callback must have constructed the Cinder reconciler")
	g.Expect(backend).NotTo(BeNil(),
		"registerController callback must have constructed the CinderBackend reconciler")
	g.Expect(backupBackend).NotTo(BeNil(),
		"registerController callback must have constructed the CinderBackupBackend reconciler")
	g.Expect(cinder.gatewayAPIAvailable).To(BeTrue(),
		"SetupWithManager must detect the Gateway API HTTPRoute CRD served from fake_crds/gateway-api")
	g.Expect(cinder.vpaAvailable).To(BeTrue(),
		"SetupWithManager must detect the VerticalPodAutoscaler CRD served from fake_crds/vertical-pod-autoscaler")

	ns := createTestNamespace(t, ctx, c)

	cr := integrationCinderCR("cinder", ns, nil)
	g.Expect(c.Create(ctx, cr)).To(Succeed(), "create Cinder fixture")
	commonenvtest.WaitForObservedGeneration(t, ctx, c, cr,
		func() int64 { return cr.Status.ObservedGeneration }, "Cinder", eventuallyTimeout, pollInterval)

	volumes := integrationBackendCR("volumes", ns, "cinder")
	g.Expect(c.Create(ctx, volumes)).To(Succeed(), "create CinderBackend fixture")
	commonenvtest.WaitForObservedGeneration(t, ctx, c, volumes,
		func() int64 { return volumes.Status.ObservedGeneration }, "CinderBackend", eventuallyTimeout, pollInterval)

	backups := integrationBackupBackendCR("backups", ns, "cinder")
	g.Expect(c.Create(ctx, backups)).To(Succeed(), "create CinderBackupBackend fixture")
	commonenvtest.WaitForObservedGeneration(t, ctx, c, backups,
		func() int64 { return backups.Status.ObservedGeneration }, "CinderBackupBackend", eventuallyTimeout, pollInterval)
}
