// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package networkpolicy

import (
	"context"
	"testing"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/managedfields"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/c5c3/forge/internal/common/conditions"
	commonv1 "github.com/c5c3/forge/internal/common/types"
)

func npScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("corev1: %v", err)
	}
	if err := networkingv1.AddToScheme(s); err != nil {
		t.Fatalf("networkingv1: %v", err)
	}
	return s
}

func npOwner() *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: "ns", UID: "u"}}
}

func desiredPolicy() *networkingv1.NetworkPolicy {
	tcp := corev1.ProtocolTCP
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "ns"},
		Spec: networkingv1.NetworkPolicySpec{
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From:  IngressPeers(IngressPeersParams{OperatorNamespace: "op"}),
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp}},
			}},
		},
	}
}

func TestCacheEgressPorts(t *testing.T) {
	for _, tc := range []struct {
		name       string
		clusterRef bool
		servers    []string
		want       []int32
	}{
		{name: "no cache", want: nil},
		{name: "managed", clusterRef: true, want: []int32{11211}},
		{name: "brownfield bare host defaults", servers: []string{"mc"}, want: []int32{11211}},
		{name: "brownfield explicit", servers: []string{"mc:11212"}, want: []int32{11212}},
		{name: "brownfield deduped", servers: []string{"a:11211", "b:11212", "c:11211"}, want: []int32{11211, 11212}},
		{name: "brownfield ipv6", servers: []string{"[::1]:11213"}, want: []int32{11213}},
		{name: "brownfield malformed defaults", servers: []string{"mc:notaport"}, want: []int32{11211}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			cache := commonv1.CacheSpec{Servers: tc.servers}
			if tc.clusterRef {
				cache.ClusterRef = &corev1.LocalObjectReference{Name: "mc"}
			}
			g.Expect(CacheEgressPorts(cache)).To(gomega.Equal(tc.want))
		})
	}
}

func TestDNSEgressRule(t *testing.T) {
	g := gomega.NewWithT(t)
	rule := DNSEgressRule()
	g.Expect(rule.Ports).To(gomega.HaveLen(2))
	g.Expect(*rule.Ports[0].Protocol).To(gomega.Equal(corev1.ProtocolUDP))
	g.Expect(rule.Ports[0].Port.IntValue()).To(gomega.Equal(53))
	g.Expect(*rule.Ports[1].Protocol).To(gomega.Equal(corev1.ProtocolTCP))
	g.Expect(rule.Ports[1].Port.IntValue()).To(gomega.Equal(53))
}

func TestDatabaseEgressRule(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(DatabaseEgressRule(commonv1.DatabaseSpec{}).Ports[0].Port.IntValue()).To(gomega.Equal(3306))
	g.Expect(DatabaseEgressRule(commonv1.DatabaseSpec{Port: 13306}).Ports[0].Port.IntValue()).To(gomega.Equal(13306))
}

func TestCacheEgressRule(t *testing.T) {
	g := gomega.NewWithT(t)
	_, ok := CacheEgressRule(commonv1.CacheSpec{})
	g.Expect(ok).To(gomega.BeFalse())
	rule, ok := CacheEgressRule(commonv1.CacheSpec{ClusterRef: &corev1.LocalObjectReference{Name: "mc"}})
	g.Expect(ok).To(gomega.BeTrue())
	g.Expect(rule.Ports[0].Port.IntValue()).To(gomega.Equal(11211))
}

func TestIngressPeers_Order(t *testing.T) {
	g := gomega.NewWithT(t)
	src := commonv1.NetworkPolicyIngressSource{
		NamespaceSelector: metav1.LabelSelector{MatchLabels: map[string]string{"team": "a"}},
	}
	peers := IngressPeers(IngressPeersParams{
		Sources:           []commonv1.NetworkPolicyIngressSource{src},
		GatewayNamespace:  "gw-ns",
		OperatorNamespace: "op-ns",
	})
	g.Expect(peers).To(gomega.HaveLen(3))
	g.Expect(peers[0].NamespaceSelector.MatchLabels).To(gomega.HaveKeyWithValue("team", "a"))
	g.Expect(peers[1].NamespaceSelector.MatchLabels).To(gomega.HaveKeyWithValue("kubernetes.io/metadata.name", "gw-ns"))
	g.Expect(peers[2].NamespaceSelector.MatchLabels).To(gomega.HaveKeyWithValue("kubernetes.io/metadata.name", "op-ns"))
}

func TestIngressPeers_OmitsEmptyNamespaces(t *testing.T) {
	g := gomega.NewWithT(t)
	peers := IngressPeers(IngressPeersParams{})
	g.Expect(peers).To(gomega.BeEmpty())
}

func TestReconcile_DeletesWhenNotConfigured(t *testing.T) {
	g := gomega.NewWithT(t)
	s := npScheme(t)
	existing := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "ns"}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(existing).Build()
	var conds []metav1.Condition

	res, err := Reconcile(context.Background(), c, s, npOwner(), FlowParams{
		Configured: false, Name: "svc", Namespace: "ns",
		Conditions: &conds, Generation: 1, ConditionType: "NetworkPolicyReady",
	})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(res.IsZero()).To(gomega.BeTrue())
	err = c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "svc"}, &networkingv1.NetworkPolicy{})
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(conditions.GetCondition(conds, "NetworkPolicyReady").Reason).To(gomega.Equal(ReasonNetworkPolicyNotRequired))
}

func TestReconcile_FailsClosedOnEmptyIngress(t *testing.T) {
	g := gomega.NewWithT(t)
	s := npScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).Build()
	var conds []metav1.Condition

	_, err := Reconcile(context.Background(), c, s, npOwner(), FlowParams{
		Configured: true, IngressSourceCount: 0, Name: "svc", Namespace: "ns",
		Conditions: &conds, Generation: 1, ConditionType: "NetworkPolicyReady",
	})
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("must not be empty")))
	// No condition is written on the fail-closed path.
	g.Expect(conds).To(gomega.BeEmpty())
}

func TestReconcile_EnsuresWhenConfigured(t *testing.T) {
	g := gomega.NewWithT(t)
	s := npScheme(t)
	// The fake client's default typed converter cannot apply a NetworkPolicy
	// ("expected objects with types from the same schema"); the deduced
	// converter applies it uniformly. Real SSA is exercised by the envtest suite.
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(npOwner()).
		WithTypeConverters(managedfields.NewDeducedTypeConverter()).Build()
	var conds []metav1.Condition

	res, err := Reconcile(context.Background(), c, s, npOwner(), FlowParams{
		Configured: true, IngressSourceCount: 1, Desired: desiredPolicy(),
		Name: "svc", Namespace: "ns",
		Conditions: &conds, Generation: 1, ConditionType: "NetworkPolicyReady",
	})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(res.IsZero()).To(gomega.BeTrue())
	fetched := &networkingv1.NetworkPolicy{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "svc"}, fetched)).To(gomega.Succeed())
	g.Expect(fetched.OwnerReferences).To(gomega.HaveLen(1))
	g.Expect(conditions.GetCondition(conds, "NetworkPolicyReady").Reason).To(gomega.Equal(ReasonNetworkPolicyReady))
}
