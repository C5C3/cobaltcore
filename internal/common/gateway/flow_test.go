// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/c5c3/forge/internal/common/conditions"
)

func flowScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := gatewayv1.Install(s); err != nil {
		t.Fatalf("installing gateway scheme: %v", err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("adding corev1 scheme: %v", err)
	}
	return s
}

func flowOwner() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: "openstack", UID: "owner-uid"},
	}
}

func baseRouteParams(conds *[]metav1.Condition) RouteFlowParams {
	return RouteFlowParams{
		RouteName:       "ks",
		RouteNamespace:  "openstack",
		ExposureNoun:    "API",
		Conditions:      conds,
		Generation:      2,
		ConditionType:   "HTTPRouteReady",
		RequeueAccepted: 10 * time.Second,
	}
}

func TestReconcileHTTPRoute_APIAbsentGatewaySet(t *testing.T) {
	g := gomega.NewWithT(t)
	s := flowScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).Build()
	var conds []metav1.Condition
	p := baseRouteParams(&conds)
	p.GatewayAPIAvailable = false
	p.GatewayConfigured = true

	res, err := ReconcileHTTPRoute(context.Background(), c, s, flowOwner(), p)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(res.IsZero()).To(gomega.BeTrue())
	cond := conditions.GetCondition(conds, p.ConditionType)
	g.Expect(cond.Status).To(gomega.Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(gomega.Equal(ReasonGatewayAPINotInstalled))
	g.Expect(cond.Message).To(gomega.ContainSubstring("enable external API exposure"))
}

func TestReconcileHTTPRoute_APIAbsentGatewayNil(t *testing.T) {
	g := gomega.NewWithT(t)
	s := flowScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).Build()
	var conds []metav1.Condition
	p := baseRouteParams(&conds)
	p.GatewayAPIAvailable = false
	p.GatewayConfigured = false

	res, err := ReconcileHTTPRoute(context.Background(), c, s, flowOwner(), p)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(res.IsZero()).To(gomega.BeTrue())
	cond := conditions.GetCondition(conds, p.ConditionType)
	g.Expect(cond.Status).To(gomega.Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(gomega.Equal(ReasonHTTPRouteNotRequired))
	g.Expect(cond.Message).To(gomega.Equal("External API exposure via Gateway API is not configured"))
}

func TestReconcileHTTPRoute_GatewayNilDeletes(t *testing.T) {
	g := gomega.NewWithT(t)
	s := flowScheme(t)
	existing := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: "ks", Namespace: "openstack"}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(existing).Build()
	var conds []metav1.Condition
	p := baseRouteParams(&conds)
	p.GatewayAPIAvailable = true
	p.GatewayConfigured = false

	res, err := ReconcileHTTPRoute(context.Background(), c, s, flowOwner(), p)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(res.IsZero()).To(gomega.BeTrue())
	// The existing route is deleted.
	fetched := &gatewayv1.HTTPRoute{}
	err = c.Get(context.Background(), types.NamespacedName{Namespace: "openstack", Name: "ks"}, fetched)
	g.Expect(err).To(gomega.HaveOccurred())
	cond := conditions.GetCondition(conds, p.ConditionType)
	g.Expect(cond.Reason).To(gomega.Equal(ReasonHTTPRouteNotRequired))
}

func TestReconcileHTTPRoute_GatewayEnabledNotAccepted(t *testing.T) {
	g := gomega.NewWithT(t)
	s := flowScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(flowOwner()).Build()
	var conds []metav1.Condition
	p := baseRouteParams(&conds)
	p.GatewayAPIAvailable = true
	p.GatewayConfigured = true
	p.Desired = BuildHTTPRoute(testGatewaySpec(), RouteParams{
		Name: "ks", Namespace: "openstack", BackendService: "ks", BackendPort: 5000,
	})

	res, err := ReconcileHTTPRoute(context.Background(), c, s, flowOwner(), p)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	// A fresh route has no accepted parent status, so the flow requeues.
	g.Expect(res.RequeueAfter).To(gomega.Equal(p.RequeueAccepted))
	cond := conditions.GetCondition(conds, p.ConditionType)
	g.Expect(cond.Status).To(gomega.Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(gomega.Equal(ReasonHTTPRouteNotAccepted))
}
