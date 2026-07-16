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
	glancev1alpha1 "github.com/c5c3/forge/operators/glance/api/v1alpha1"
)

func glanceGatewaySpec() *glancev1alpha1.GatewaySpec {
	return &glancev1alpha1.GatewaySpec{
		ParentRef: glancev1alpha1.GatewayParentRefSpec{Name: "openstack-gw", Namespace: "envoy-gateway-system"},
		Hostname:  "glance.127-0-0-1.nip.io",
	}
}

func TestReconcileHTTPRoute_GatewayNilDeletesAndNotRequired(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	stale := &gatewayv1.HTTPRoute{}
	stale.Name = "test-glance"
	stale.Namespace = "default"
	r := newGlanceTestReconciler(glance, stale)
	r.gatewayAPIAvailable = true

	res, err := r.reconcileHTTPRoute(context.Background(), glance)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := conditions.GetCondition(glance.Status.Conditions, conditionTypeHTTPRouteReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonHTTPRouteNotRequired))

	var gone gatewayv1.HTTPRoute
	err = r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-glance"}, &gone)
	g.Expect(err).To(HaveOccurred(), "stale HTTPRoute must be deleted when spec.gateway is nil")
}

func TestReconcileHTTPRoute_GatewayAPINotInstalledWithGatewaySet(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	glance.Spec.Gateway = glanceGatewaySpec()
	r := newGlanceTestReconciler(glance)
	r.gatewayAPIAvailable = false

	res, err := r.reconcileHTTPRoute(context.Background(), glance)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := conditions.GetCondition(glance.Status.Conditions, conditionTypeHTTPRouteReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonGatewayAPINotInstalled))
}

func TestReconcileHTTPRoute_NotAcceptedRequeues(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	glance.Spec.Gateway = glanceGatewaySpec()
	r := newGlanceTestReconciler(glance)
	r.gatewayAPIAvailable = true

	res, err := r.reconcileHTTPRoute(context.Background(), glance)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(requeueHTTPRouteAccepted))
	cond := conditions.GetCondition(glance.Status.Conditions, conditionTypeHTTPRouteReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonHTTPRouteNotAccepted))
}

func TestBuildGlanceHTTPRoute_TargetsAPIService(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	glance.Spec.Gateway = glanceGatewaySpec()

	route := buildGlanceHTTPRoute(glance)

	g.Expect(route.Name).To(Equal("test-glance"))
	g.Expect(route.Spec.Hostnames).To(ContainElement(gatewayv1.Hostname("glance.127-0-0-1.nip.io")))
	g.Expect(route.Spec.Rules).NotTo(BeEmpty())
	g.Expect(route.Spec.Rules[0].BackendRefs).NotTo(BeEmpty())
	backend := route.Spec.Rules[0].BackendRefs[0]
	g.Expect(string(backend.Name)).To(Equal("test-glance"))
	g.Expect(backend.Port).To(HaveValue(Equal(gatewayv1.PortNumber(9292))))
}

func TestGlanceStatusEndpoint(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()

	g.Expect(glanceStatusEndpoint(glance)).To(Equal("http://test-glance.default.svc.cluster.local:9292/"))

	glance.Spec.Gateway = glanceGatewaySpec()
	g.Expect(glanceStatusEndpoint(glance)).To(Equal("https://glance.127-0-0-1.nip.io/"))
}
