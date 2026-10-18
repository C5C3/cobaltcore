// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

// Integration coverage for the real SetupWithManager methods of the OVNCentral
// and OVNChassis reconcilers. The inline harness (registerOVNControllers)
// registers the production chains through setupWithOptions with
// SkipNameValidation set, so the probes, index and legs already run under
// envtest; what only this test runs is the public wrapper: the production
// bootstrap.TypedControllerOptions and the registration under the real
// controller names. This test wires both SetupWithManager methods onto an
// envtest-backed manager as main.go does, starts it, and waits for each
// controller's first status write.
//
// CONSTRAINT: exactly one test in this package binary may call these real
// SetupWithManager methods. controller-runtime's global controller-name tracker
// rejects a second controller named "ovncentral" or "ovnchassis" registered
// without controller.Options{SkipNameValidation}. The inline harness
// (registerOVNControllers) sets SkipNameValidation precisely so it does not
// contend with this test, and multicluster_integration_test.go registers only
// the kubeconfig provider under its own fixed name. A future second
// real-SetupWithManager test would need its own process or the same skip.
package controller

import (
	"testing"

	. "github.com/onsi/gomega"
	ctrl "sigs.k8s.io/controller-runtime"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"

	commonenvtest "github.com/c5c3/cobaltcore/internal/common/testutil/envtest"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
	"github.com/c5c3/cobaltcore/operators/ovn/internal/testutil"
)

// TestSetupWithManager_AllControllersStart registers the production
// OVNCentralReconciler and OVNChassisReconciler via their real
// SetupWithManager methods against an envtest manager that serves every
// watched CRD (the operator CRDs plus the shared fake CRDs), asserts the
// RESTMapper probes latched, and then waits for each controller to stamp
// status.observedGeneration on a CR of its kind.
func TestSetupWithManager_AllControllersStart(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	var (
		central *OVNCentralReconciler
		chassis *OVNChassisReconciler
	)
	c, ctx, _ := testutil.SetupOVNEnvTestWithController(
		t,
		ovnv1alpha1.AddToScheme,
		func(mgr ctrl.Manager) error {
			// mgr.GetAPIReader() mirrors main.go: admission lookups read the API
			// server directly, never a stale cache. The webhook manifests envtest
			// installs carry both kinds (failurePolicy=Fail), so both handlers
			// must be served.
			if err := (&ovnv1alpha1.OVNCentralWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr); err != nil {
				return err
			}
			return (&ovnv1alpha1.OVNChassisWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr)
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
			// The OVNCentral reconciler is registered first, as in main.go: its
			// SetupWithManager is the single registration site for the OVNChassis
			// field index the chassis controller's central-to-chassis mapper
			// lists through.
			central = &OVNCentralReconciler{
				Client:                  mgr.GetClient(),
				Scheme:                  mgr.GetScheme(),
				Recorder:                mgr.GetEventRecorderFor("ovncentral-controller"), //nolint:staticcheck // SA1019: reconciler consumes record.EventRecorder (old events API); GetEventRecorder returns the incompatible events/v1 type.
				MaxConcurrentReconciles: 1,
				Resolver:                mcMgr,
			}
			if err := central.SetupWithManager(mcMgr); err != nil {
				return err
			}
			chassis = &OVNChassisReconciler{
				Client:                  mgr.GetClient(),
				Scheme:                  mgr.GetScheme(),
				Recorder:                mgr.GetEventRecorderFor("ovnchassis-controller"), //nolint:staticcheck // SA1019: reconciler consumes record.EventRecorder (old events API); GetEventRecorder returns the incompatible events/v1 type.
				MaxConcurrentReconciles: 1,
				Resolver:                mcMgr,
			}
			return chassis.SetupWithManager(mcMgr)
		},
	)

	g.Expect(central).NotTo(BeNil(),
		"registerController callback must have constructed the OVNCentral reconciler")
	g.Expect(chassis).NotTo(BeNil(),
		"registerController callback must have constructed the OVNChassis reconciler")
	g.Expect(central.certManagerAvailable).To(BeTrue(),
		"SetupWithManager must detect the cert-manager Certificate CRD served from fake_crds/cert-manager")
	g.Expect(central.vpaAvailable).To(BeTrue(),
		"SetupWithManager must detect the VerticalPodAutoscaler CRD served from fake_crds/vertical-pod-autoscaler")
	g.Expect(chassis.vpaAvailable).To(BeTrue(),
		"the OVNChassis SetupWithManager must detect the VerticalPodAutoscaler CRD served from fake_crds/vertical-pod-autoscaler")

	ns := createTestNamespace(t, ctx, c)

	cr := integrationCentralCR("central", ns, nil)
	g.Expect(c.Create(ctx, cr)).To(Succeed(), "create OVNCentral fixture")
	commonenvtest.WaitForObservedGeneration(t, ctx, c, cr,
		func() int64 { return cr.Status.ObservedGeneration }, "OVNCentral", eventuallyTimeout, pollInterval)

	ovnChassis := integrationChassisCR("chassis", ns, "central", nil)
	g.Expect(c.Create(ctx, ovnChassis)).To(Succeed(), "create OVNChassis fixture")
	commonenvtest.WaitForObservedGeneration(t, ctx, c, ovnChassis,
		func() int64 { return ovnChassis.Status.ObservedGeneration }, "OVNChassis", eventuallyTimeout, pollInterval)
}
