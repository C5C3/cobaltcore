// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/c5c3/forge/internal/common/conditions"
	horizonv1alpha1 "github.com/c5c3/forge/operators/horizon/api/v1alpha1"
)

func networkPolicySpec() *horizonv1alpha1.NetworkPolicySpec {
	return &horizonv1alpha1.NetworkPolicySpec{
		Ingress: []horizonv1alpha1.NetworkPolicyIngressSource{{
			NamespaceSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{"kubernetes.io/metadata.name": "monitoring"},
			},
		}},
	}
}

func TestReconcileNetworkPolicy_DisabledDeletesAndNotRequired(t *testing.T) {
	g := NewGomegaWithT(t)
	h := testHorizon()
	stale := &networkingv1.NetworkPolicy{}
	stale.Name = "test-horizon"
	stale.Namespace = "default"
	r := newTestReconciler(testScheme(), h, stale)

	res, err := r.reconcileNetworkPolicy(context.Background(), h)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := conditions.GetCondition(h.Status.Conditions, conditionTypeNetworkPolicyReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonNetworkPolicyNotRequired))

	var gone networkingv1.NetworkPolicy
	err = r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-horizon"}, &gone)
	g.Expect(err).To(HaveOccurred(), "stale NetworkPolicy must be deleted when disabled")
}

func TestReconcileNetworkPolicy_EmptyIngressFailsClosed(t *testing.T) {
	g := NewGomegaWithT(t)
	h := testHorizon()
	h.Spec.NetworkPolicy = &horizonv1alpha1.NetworkPolicySpec{}
	r := newTestReconciler(testScheme(), h)

	_, err := r.reconcileNetworkPolicy(context.Background(), h)

	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("refusing to create NetworkPolicy that would allow all ingress"))
}

func TestBuildHorizonNetworkPolicy_Rules(t *testing.T) {
	g := NewGomegaWithT(t)
	h := testHorizon()
	h.Spec.NetworkPolicy = networkPolicySpec()

	np := buildHorizonNetworkPolicy(h, "horizon-system")

	g.Expect(np.Spec.PodSelector.MatchLabels).To(Equal(selectorLabels(h)))
	g.Expect(np.Spec.Ingress).To(HaveLen(1))
	// Ingress restricted to the dashboard port.
	g.Expect(np.Spec.Ingress[0].Ports).To(HaveLen(1))
	g.Expect(np.Spec.Ingress[0].Ports[0].Port.IntValue()).To(Equal(8080))
	// User source plus the operator-namespace peer.
	g.Expect(np.Spec.Ingress[0].From).To(HaveLen(2))
	g.Expect(np.Spec.Ingress[0].From[1].NamespaceSelector.MatchLabels).
		To(HaveKeyWithValue("kubernetes.io/metadata.name", "horizon-system"))

	// Egress: DNS, keystone (5000 from the endpoint URL), cache (11211).
	g.Expect(np.Spec.Egress).To(HaveLen(3))
	g.Expect(np.Spec.Egress[0].Ports[0].Port.IntValue()).To(Equal(53))
	g.Expect(np.Spec.Egress[1].Ports[0].Port.IntValue()).To(Equal(5000))
	g.Expect(np.Spec.Egress[2].Ports[0].Port.IntValue()).To(Equal(11211))
}

func TestBuildHorizonNetworkPolicy_GatewayPeerAppended(t *testing.T) {
	g := NewGomegaWithT(t)
	h := testHorizon()
	h.Spec.NetworkPolicy = networkPolicySpec()
	h.Spec.Gateway = gatewaySpec()

	np := buildHorizonNetworkPolicy(h, "")

	// User source plus the gateway-namespace peer (no operator peer: empty
	// operatorNamespace).
	g.Expect(np.Spec.Ingress[0].From).To(HaveLen(2))
	g.Expect(np.Spec.Ingress[0].From[1].NamespaceSelector.MatchLabels).
		To(HaveKeyWithValue("kubernetes.io/metadata.name", "envoy-gateway-system"))
}

func TestKeystoneEndpointPort_Table(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		want     int32
	}{
		{name: "explicit port", endpoint: "http://keystone.default.svc:5000/v3", want: 5000},
		{name: "http default", endpoint: "http://keystone.example.com/v3", want: 80},
		{name: "https default", endpoint: "https://keystone.example.com/v3", want: 443},
		{name: "unparseable falls back to 443", endpoint: "http://[::bad", want: 443},
		{name: "out-of-range port falls back to scheme default", endpoint: "http://keystone:99999/v3", want: 80},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			h := testHorizon()
			h.Spec.KeystoneEndpoint = tc.endpoint
			g.Expect(keystoneEndpointPort(h)).To(Equal(tc.want))
		})
	}
}

func TestCacheEgressPorts_BrownfieldDedupesAndDefaults(t *testing.T) {
	g := NewGomegaWithT(t)
	h := testHorizon()
	h.Spec.Cache.ClusterRef = nil
	h.Spec.Cache.Servers = []string{"mc-0:11212", "mc-1:11212", "mc-2", "mc-3:notaport"}

	// 11212 deduplicated; entries without a parseable port default to 11211.
	g.Expect(cacheEgressPorts(h)).To(Equal([]int32{11212, 11211}))
}

func TestCacheEgressPorts_NoCacheYieldsNil(t *testing.T) {
	g := NewGomegaWithT(t)
	h := testHorizon()
	h.Spec.Cache.ClusterRef = nil
	h.Spec.Cache.Servers = nil

	g.Expect(cacheEgressPorts(h)).To(BeNil())
}
