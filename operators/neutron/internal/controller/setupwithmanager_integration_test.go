// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

// Integration coverage for the real SetupWithManager methods of the Neutron and
// NeutronMetadataAgent reconcilers. The inline harness
// (registerNeutronControllers) registers the production chains through
// setupWithOptions with SkipNameValidation set, so the probes, indexes and legs
// already run under envtest; what only this test runs is the public wrapper:
// the production bootstrap.TypedControllerOptions and the registration under
// the real controller names. This test wires both SetupWithManager methods onto
// an envtest-backed manager as main.go does, starts it, and waits for each
// controller's first status write.
//
// CONSTRAINT: exactly one test in this package binary may call these real
// SetupWithManager methods. controller-runtime's global controller-name tracker
// rejects a second controller named "neutron" or "neutronmetadataagent"
// registered without controller.Options{SkipNameValidation}. The inline harness
// (registerNeutronControllers) sets SkipNameValidation precisely so it does not
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
	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
	"github.com/c5c3/cobaltcore/operators/neutron/internal/testutil"
)

// TestSetupWithManager_AllControllersStart registers the production
// NeutronReconciler and NeutronMetadataAgentReconciler via their real
// SetupWithManager methods against an envtest manager that serves every
// watched CRD (the operator CRDs, the OVN CRDs and the shared fake CRDs),
// asserts the RESTMapper probes latched, and then waits for each controller to
// stamp status.observedGeneration on a CR of its kind.
func TestSetupWithManager_AllControllersStart(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	var (
		neutron *NeutronReconciler
		agent   *NeutronMetadataAgentReconciler
	)
	c, ctx, _ := testutil.SetupNeutronEnvTestWithController(
		t,
		neutronv1alpha1.AddToScheme,
		func(mgr ctrl.Manager) error {
			// mgr.GetAPIReader() mirrors main.go: admission lookups read the API
			// server directly, never a stale cache. The webhook manifests envtest
			// installs carry both kinds (failurePolicy=Fail), so both handlers
			// must be served.
			if err := (&neutronv1alpha1.NeutronWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr); err != nil {
				return err
			}
			return (&neutronv1alpha1.NeutronMetadataAgentWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr)
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
			// The Neutron reconciler is registered first, as in main.go: its
			// SetupWithManager is the single registration site for the Neutron
			// field indexes.
			neutron = &NeutronReconciler{
				Client:                  mgr.GetClient(),
				Scheme:                  mgr.GetScheme(),
				Recorder:                mgr.GetEventRecorderFor("neutron-controller"), //nolint:staticcheck // SA1019: reconciler consumes record.EventRecorder (old events API); GetEventRecorder returns the incompatible events/v1 type.
				MaxConcurrentReconciles: 1,
				Resolver:                mcMgr,
				// The healthy stub keeps the health-check probe from firing HTTP
				// GETs at a Service DNS name envtest cannot resolve.
				HTTPClient: &stubDoer{status: http.StatusOK},
			}
			if err := neutron.SetupWithManager(mcMgr); err != nil {
				return err
			}
			agent = &NeutronMetadataAgentReconciler{
				Client:                  mgr.GetClient(),
				Scheme:                  mgr.GetScheme(),
				Recorder:                mgr.GetEventRecorderFor("neutronmetadataagent-controller"), //nolint:staticcheck // SA1019: reconciler consumes record.EventRecorder (old events API); GetEventRecorder returns the incompatible events/v1 type.
				MaxConcurrentReconciles: 1,
				Resolver:                mcMgr,
			}
			return agent.SetupWithManager(mcMgr)
		},
	)

	g.Expect(neutron).NotTo(BeNil(),
		"registerController callback must have constructed the Neutron reconciler")
	g.Expect(agent).NotTo(BeNil(),
		"registerController callback must have constructed the NeutronMetadataAgent reconciler")
	g.Expect(neutron.gatewayAPIAvailable).To(BeTrue(),
		"SetupWithManager must detect the Gateway API HTTPRoute CRD served from fake_crds/gateway-api")
	g.Expect(neutron.vpaAvailable).To(BeTrue(),
		"SetupWithManager must detect the VerticalPodAutoscaler CRD served from fake_crds/vertical-pod-autoscaler")
	g.Expect(agent.vpaAvailable).To(BeTrue(),
		"the NeutronMetadataAgent SetupWithManager must detect the VerticalPodAutoscaler CRD served from fake_crds/vertical-pod-autoscaler")

	ns := createTestNamespace(t, ctx, c)

	cr := integrationNeutronCR("neutron", ns, "central", ns, nil)
	g.Expect(c.Create(ctx, cr)).To(Succeed(), "create Neutron fixture")
	commonenvtest.WaitForObservedGeneration(t, ctx, c, cr,
		func() int64 { return cr.Status.ObservedGeneration }, "Neutron", eventuallyTimeout, pollInterval)

	metadataAgent := integrationAgentCR("agent", ns, "chassis", nil)
	g.Expect(c.Create(ctx, metadataAgent)).To(Succeed(), "create NeutronMetadataAgent fixture")
	commonenvtest.WaitForObservedGeneration(t, ctx, c, metadataAgent,
		func() int64 { return metadataAgent.Status.ObservedGeneration }, "NeutronMetadataAgent", eventuallyTimeout, pollInterval)
}
