// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/c5c3/forge/internal/common/conditions"
	placementv1alpha1 "github.com/c5c3/forge/operators/placement/api/v1alpha1"
)

// placementGatewaySpec returns the external-exposure block the route tests and
// the endpoint assertions share.
func placementGatewaySpec() *placementv1alpha1.GatewaySpec {
	return &placementv1alpha1.GatewaySpec{
		ParentRef: placementv1alpha1.GatewayParentRefSpec{Name: "openstack-gw", Namespace: "envoy-gateway-system"},
		Hostname:  "placement.127-0-0-1.nip.io",
	}
}

func TestReconcileHTTPRoute_GatewayNilDeletesAndNotRequired(t *testing.T) {
	g := NewGomegaWithT(t)
	placement := testPlacement()
	stale := &gatewayv1.HTTPRoute{}
	stale.Name = "test-placement"
	stale.Namespace = "default"
	r := newPlacementTestReconciler(placement, stale)
	r.gatewayAPIAvailable = true

	res, err := r.reconcileHTTPRoute(context.Background(), placement)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := conditions.GetCondition(placement.Status.Conditions, conditionTypeHTTPRouteReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonHTTPRouteNotRequired))

	var gone gatewayv1.HTTPRoute
	err = r.Get(context.Background(), placementRequest.NamespacedName, &gone)
	g.Expect(err).To(HaveOccurred(), "stale HTTPRoute must be deleted when spec.gateway is nil")
}

func TestReconcileHTTPRoute_GatewayAPINotInstalledWithGatewaySet(t *testing.T) {
	g := NewGomegaWithT(t)
	placement := testPlacement()
	placement.Spec.Gateway = placementGatewaySpec()
	r := newPlacementTestReconciler(placement)
	r.gatewayAPIAvailable = false

	res, err := r.reconcileHTTPRoute(context.Background(), placement)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := conditions.GetCondition(placement.Status.Conditions, conditionTypeHTTPRouteReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonGatewayAPINotInstalled))
}

func TestReconcileHTTPRoute_NotAcceptedRequeues(t *testing.T) {
	g := NewGomegaWithT(t)
	placement := testPlacement()
	placement.Spec.Gateway = placementGatewaySpec()
	r := newPlacementTestReconciler(placement)
	r.gatewayAPIAvailable = true

	res, err := r.reconcileHTTPRoute(context.Background(), placement)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(requeueHTTPRouteAccepted))
	cond := conditions.GetCondition(placement.Status.Conditions, conditionTypeHTTPRouteReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonHTTPRouteNotAccepted))

	var route gatewayv1.HTTPRoute
	g.Expect(r.Get(context.Background(), placementRequest.NamespacedName, &route)).To(Succeed())
}

func TestBuildPlacementHTTPRoute_TargetsAPIService(t *testing.T) {
	g := NewGomegaWithT(t)
	placement := testPlacement()
	placement.Spec.Gateway = placementGatewaySpec()

	route := buildPlacementHTTPRoute(placement)

	g.Expect(route.Name).To(Equal("test-placement"))
	g.Expect(route.Spec.Hostnames).To(ContainElement(gatewayv1.Hostname("placement.127-0-0-1.nip.io")))
	g.Expect(route.Spec.Rules).NotTo(BeEmpty())
	g.Expect(route.Spec.Rules[0].BackendRefs).NotTo(BeEmpty())
	backend := route.Spec.Rules[0].BackendRefs[0]
	g.Expect(string(backend.Name)).To(Equal("test-placement"))
	g.Expect(backend.Port).To(HaveValue(Equal(gatewayv1.PortNumber(8778))))
	// No timeouts stanza: placement answers short JSON requests, so the gateway
	// implementation's own default is the right cap.
	g.Expect(route.Spec.Rules[0].Timeouts).To(BeNil())
}

func TestPlacementStatusEndpoint(t *testing.T) {
	g := NewGomegaWithT(t)
	placement := testPlacement()

	g.Expect(placementStatusEndpoint(placement)).To(Equal("http://test-placement.default.svc.cluster.local:8778/"))

	placement.Spec.Gateway = placementGatewaySpec()
	g.Expect(placementStatusEndpoint(placement)).To(Equal("https://placement.127-0-0-1.nip.io/"))
}
