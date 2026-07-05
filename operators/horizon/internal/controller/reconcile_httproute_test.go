// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/c5c3/forge/internal/common/conditions"
	horizonv1alpha1 "github.com/c5c3/forge/operators/horizon/api/v1alpha1"
)

func gatewaySpec() *horizonv1alpha1.GatewaySpec {
	return &horizonv1alpha1.GatewaySpec{
		ParentRef: horizonv1alpha1.GatewayParentRefSpec{Name: "openstack-gw", Namespace: "envoy-gateway-system"},
		Hostname:  "horizon.127-0-0-1.nip.io",
	}
}

func TestReconcileHTTPRoute_GatewayNilDeletesAndNotRequired(t *testing.T) {
	g := NewGomegaWithT(t)
	h := testHorizon()
	// Pre-existing HTTPRoute from an earlier gateway-enabled generation.
	stale := &gatewayv1.HTTPRoute{}
	stale.Name = "test-horizon"
	stale.Namespace = "default"
	r := newTestReconciler(testScheme(), h, stale)
	r.gatewayAPIAvailable = true

	res, err := r.reconcileHTTPRoute(context.Background(), h)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := conditions.GetCondition(h.Status.Conditions, conditionTypeHTTPRouteReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonHTTPRouteNotRequired))

	var gone gatewayv1.HTTPRoute
	err = r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-horizon"}, &gone)
	g.Expect(err).To(HaveOccurred(), "stale HTTPRoute must be deleted when spec.gateway is nil")
}

func TestReconcileHTTPRoute_GatewayAPINotInstalledWithGatewaySet(t *testing.T) {
	g := NewGomegaWithT(t)
	h := testHorizon()
	h.Spec.Gateway = gatewaySpec()
	r := newTestReconciler(testScheme(), h)
	r.gatewayAPIAvailable = false

	res, err := r.reconcileHTTPRoute(context.Background(), h)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := conditions.GetCondition(h.Status.Conditions, conditionTypeHTTPRouteReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonGatewayAPINotInstalled))
}

func TestReconcileHTTPRoute_NotAcceptedRequeues(t *testing.T) {
	g := NewGomegaWithT(t)
	h := testHorizon()
	h.Spec.Gateway = gatewaySpec()
	r := newTestReconciler(testScheme(), h)
	r.gatewayAPIAvailable = true

	res, err := r.reconcileHTTPRoute(context.Background(), h)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(requeueHTTPRouteAccepted))
	cond := conditions.GetCondition(h.Status.Conditions, conditionTypeHTTPRouteReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonHTTPRouteNotAccepted))
}

func TestBuildHorizonHTTPRoute_TargetsDashboardService(t *testing.T) {
	g := NewGomegaWithT(t)
	h := testHorizon()
	h.Spec.Gateway = gatewaySpec()

	route := buildHorizonHTTPRoute(h)

	g.Expect(route.Name).To(Equal("test-horizon"))
	g.Expect(route.Spec.Hostnames).To(ContainElement(gatewayv1.Hostname("horizon.127-0-0-1.nip.io")))
	g.Expect(route.Spec.Rules).NotTo(BeEmpty())
	g.Expect(route.Spec.Rules[0].BackendRefs).NotTo(BeEmpty())
	backend := route.Spec.Rules[0].BackendRefs[0]
	g.Expect(string(backend.Name)).To(Equal("test-horizon"))
	g.Expect(backend.Port).To(HaveValue(Equal(gatewayv1.PortNumber(8080))))
}

func TestHorizonStatusEndpoint(t *testing.T) {
	g := NewGomegaWithT(t)
	h := testHorizon()

	g.Expect(horizonStatusEndpoint(h)).To(Equal("http://test-horizon.default.svc.cluster.local:8080/"))

	h.Spec.Gateway = gatewaySpec()
	g.Expect(horizonStatusEndpoint(h)).To(Equal("https://horizon.127-0-0-1.nip.io/"))
}
