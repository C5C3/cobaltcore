// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"testing"

	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	commonv1 "github.com/c5c3/forge/internal/common/types"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

func npTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = networkingv1.AddToScheme(s)
	_ = keystonev1alpha1.AddToScheme(s)
	return s
}

func npTestKeystone() *keystonev1alpha1.Keystone {
	return &keystonev1alpha1.Keystone{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-keystone",
			Namespace:  "default",
			UID:        "ks-uid",
			Generation: 1,
		},
		Spec: keystonev1alpha1.KeystoneSpec{
			Replicas: 3,
			Image:    commonv1.ImageSpec{Repository: "ghcr.io/c5c3/keystone", Tag: "2025.2"},
			Database: commonv1.DatabaseSpec{
				Host:      "db.example.com",
				Port:      3306,
				Database:  "keystone",
				SecretRef: commonv1.SecretRefSpec{Name: "keystone-db-credentials"},
			},
			Cache: commonv1.CacheSpec{Backend: "dogpile.cache.pymemcache", Servers: []string{"mc:11211"}},
			Bootstrap: keystonev1alpha1.BootstrapSpec{
				AdminUser:              "admin",
				AdminPasswordSecretRef: commonv1.SecretRefSpec{Name: "keystone-admin"},
				Region:                 "RegionOne",
			},
		},
	}
}

func newNPTestReconciler(s *runtime.Scheme, objs ...client.Object) *KeystoneReconciler {
	cb := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...)
	cb = cb.WithStatusSubresource(&keystonev1alpha1.Keystone{})
	return &KeystoneReconciler{
		Client:   cb.Build(),
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}
}

// --- Task 2.2: buildKeystoneNetworkPolicy unit tests ---

func TestBuildKeystoneNetworkPolicy_NameAndNamespace(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := npTestKeystone()
	ks.Spec.NetworkPolicy = &keystonev1alpha1.NetworkPolicySpec{
		Ingress: []keystonev1alpha1.NetworkPolicyIngressSource{
			{NamespaceSelector: map[string]string{"kubernetes.io/metadata.name": "openstack"}},
		},
	}

	np := buildKeystoneNetworkPolicy(ks)

	g.Expect(np.Name).To(Equal("test-keystone"))
	g.Expect(np.Namespace).To(Equal("default"))
}

func TestBuildKeystoneNetworkPolicy_Labels(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := npTestKeystone()
	ks.Spec.NetworkPolicy = &keystonev1alpha1.NetworkPolicySpec{
		Ingress: []keystonev1alpha1.NetworkPolicyIngressSource{
			{NamespaceSelector: map[string]string{"kubernetes.io/metadata.name": "openstack"}},
		},
	}

	np := buildKeystoneNetworkPolicy(ks)

	g.Expect(np.Labels).To(HaveKeyWithValue("app.kubernetes.io/name", "keystone"))
	g.Expect(np.Labels).To(HaveKeyWithValue("app.kubernetes.io/instance", "test-keystone"))
	g.Expect(np.Labels).To(HaveKeyWithValue("app.kubernetes.io/managed-by", "keystone-operator"))
}

func TestBuildKeystoneNetworkPolicy_PodSelector(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := npTestKeystone()
	ks.Spec.NetworkPolicy = &keystonev1alpha1.NetworkPolicySpec{
		Ingress: []keystonev1alpha1.NetworkPolicyIngressSource{
			{NamespaceSelector: map[string]string{"kubernetes.io/metadata.name": "openstack"}},
		},
	}

	np := buildKeystoneNetworkPolicy(ks)

	g.Expect(np.Spec.PodSelector.MatchLabels).To(HaveKeyWithValue("app.kubernetes.io/name", "keystone"))
	g.Expect(np.Spec.PodSelector.MatchLabels).To(HaveKeyWithValue("app.kubernetes.io/instance", "test-keystone"))
	g.Expect(np.Spec.PodSelector.MatchLabels).To(HaveLen(2))
}

func TestBuildKeystoneNetworkPolicy_PolicyTypes(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := npTestKeystone()
	ks.Spec.NetworkPolicy = &keystonev1alpha1.NetworkPolicySpec{
		Ingress: []keystonev1alpha1.NetworkPolicyIngressSource{
			{NamespaceSelector: map[string]string{"kubernetes.io/metadata.name": "openstack"}},
		},
	}

	np := buildKeystoneNetworkPolicy(ks)

	g.Expect(np.Spec.PolicyTypes).To(ConsistOf(
		networkingv1.PolicyTypeIngress,
		networkingv1.PolicyTypeEgress,
	))
}

func TestBuildKeystoneNetworkPolicy_IngressRules_SingleSource(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := npTestKeystone()
	ks.Spec.NetworkPolicy = &keystonev1alpha1.NetworkPolicySpec{
		Ingress: []keystonev1alpha1.NetworkPolicyIngressSource{
			{NamespaceSelector: map[string]string{"kubernetes.io/metadata.name": "openstack"}},
		},
	}

	np := buildKeystoneNetworkPolicy(ks)

	g.Expect(np.Spec.Ingress).To(HaveLen(1))
	g.Expect(np.Spec.Ingress[0].Ports).To(HaveLen(1))
	g.Expect(*np.Spec.Ingress[0].Ports[0].Protocol).To(Equal(corev1.ProtocolTCP))
	g.Expect(np.Spec.Ingress[0].Ports[0].Port.IntValue()).To(Equal(5000))

	g.Expect(np.Spec.Ingress[0].From).To(HaveLen(1))
	g.Expect(np.Spec.Ingress[0].From[0].NamespaceSelector).NotTo(BeNil())
	g.Expect(np.Spec.Ingress[0].From[0].NamespaceSelector.MatchLabels).To(
		HaveKeyWithValue("kubernetes.io/metadata.name", "openstack"),
	)
	g.Expect(np.Spec.Ingress[0].From[0].PodSelector).To(BeNil())
}

func TestBuildKeystoneNetworkPolicy_IngressRules_MultipleSources_WithPodSelector(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := npTestKeystone()
	ks.Spec.NetworkPolicy = &keystonev1alpha1.NetworkPolicySpec{
		Ingress: []keystonev1alpha1.NetworkPolicyIngressSource{
			{
				NamespaceSelector: map[string]string{"kubernetes.io/metadata.name": "openstack"},
				PodSelector:       map[string]string{"app": "horizon"},
			},
			{
				NamespaceSelector: map[string]string{"kubernetes.io/metadata.name": "monitoring"},
			},
		},
	}

	np := buildKeystoneNetworkPolicy(ks)

	g.Expect(np.Spec.Ingress).To(HaveLen(1))
	g.Expect(np.Spec.Ingress[0].From).To(HaveLen(2))

	// First peer: namespace + pod selector.
	g.Expect(np.Spec.Ingress[0].From[0].NamespaceSelector.MatchLabels).To(
		HaveKeyWithValue("kubernetes.io/metadata.name", "openstack"),
	)
	g.Expect(np.Spec.Ingress[0].From[0].PodSelector).NotTo(BeNil())
	g.Expect(np.Spec.Ingress[0].From[0].PodSelector.MatchLabels).To(
		HaveKeyWithValue("app", "horizon"),
	)

	// Second peer: namespace selector only.
	g.Expect(np.Spec.Ingress[0].From[1].NamespaceSelector.MatchLabels).To(
		HaveKeyWithValue("kubernetes.io/metadata.name", "monitoring"),
	)
	g.Expect(np.Spec.Ingress[0].From[1].PodSelector).To(BeNil())
}

func TestBuildKeystoneNetworkPolicy_AutoDerivedEgress_BrownfieldOnly_DNSOnly(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := npTestKeystone()
	// Brownfield mode: no ClusterRef on either database or cache.
	ks.Spec.NetworkPolicy = &keystonev1alpha1.NetworkPolicySpec{
		Ingress: []keystonev1alpha1.NetworkPolicyIngressSource{
			{NamespaceSelector: map[string]string{"kubernetes.io/metadata.name": "openstack"}},
		},
	}

	np := buildKeystoneNetworkPolicy(ks)

	// Only DNS egress should be present in brownfield mode.
	g.Expect(np.Spec.Egress).To(HaveLen(1))
	g.Expect(np.Spec.Egress[0].Ports).To(HaveLen(2))
	g.Expect(*np.Spec.Egress[0].Ports[0].Protocol).To(Equal(corev1.ProtocolUDP))
	g.Expect(np.Spec.Egress[0].Ports[0].Port.IntValue()).To(Equal(53))
	g.Expect(*np.Spec.Egress[0].Ports[1].Protocol).To(Equal(corev1.ProtocolTCP))
	g.Expect(np.Spec.Egress[0].Ports[1].Port.IntValue()).To(Equal(53))
}

func TestBuildKeystoneNetworkPolicy_AutoDerivedEgress_ManagedDB(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := npTestKeystone()
	ks.Spec.Database.ClusterRef = &corev1.LocalObjectReference{Name: "mariadb"}
	ks.Spec.Database.Host = ""
	ks.Spec.NetworkPolicy = &keystonev1alpha1.NetworkPolicySpec{
		Ingress: []keystonev1alpha1.NetworkPolicyIngressSource{
			{NamespaceSelector: map[string]string{"kubernetes.io/metadata.name": "openstack"}},
		},
	}

	np := buildKeystoneNetworkPolicy(ks)

	// DNS + MariaDB egress.
	g.Expect(np.Spec.Egress).To(HaveLen(2))
	g.Expect(np.Spec.Egress[1].Ports).To(HaveLen(1))
	g.Expect(*np.Spec.Egress[1].Ports[0].Protocol).To(Equal(corev1.ProtocolTCP))
	g.Expect(np.Spec.Egress[1].Ports[0].Port.IntValue()).To(Equal(3306))
}

func TestBuildKeystoneNetworkPolicy_AutoDerivedEgress_ManagedCache(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := npTestKeystone()
	ks.Spec.Cache.ClusterRef = &corev1.LocalObjectReference{Name: "memcached"}
	ks.Spec.Cache.Servers = nil
	ks.Spec.NetworkPolicy = &keystonev1alpha1.NetworkPolicySpec{
		Ingress: []keystonev1alpha1.NetworkPolicyIngressSource{
			{NamespaceSelector: map[string]string{"kubernetes.io/metadata.name": "openstack"}},
		},
	}

	np := buildKeystoneNetworkPolicy(ks)

	// DNS + Memcached egress.
	g.Expect(np.Spec.Egress).To(HaveLen(2))
	g.Expect(np.Spec.Egress[1].Ports).To(HaveLen(1))
	g.Expect(*np.Spec.Egress[1].Ports[0].Protocol).To(Equal(corev1.ProtocolTCP))
	g.Expect(np.Spec.Egress[1].Ports[0].Port.IntValue()).To(Equal(11211))
}

func TestBuildKeystoneNetworkPolicy_AutoDerivedEgress_BothManaged(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := npTestKeystone()
	ks.Spec.Database.ClusterRef = &corev1.LocalObjectReference{Name: "mariadb"}
	ks.Spec.Database.Host = ""
	ks.Spec.Cache.ClusterRef = &corev1.LocalObjectReference{Name: "memcached"}
	ks.Spec.Cache.Servers = nil
	ks.Spec.NetworkPolicy = &keystonev1alpha1.NetworkPolicySpec{
		Ingress: []keystonev1alpha1.NetworkPolicyIngressSource{
			{NamespaceSelector: map[string]string{"kubernetes.io/metadata.name": "openstack"}},
		},
	}

	np := buildKeystoneNetworkPolicy(ks)

	// DNS + MariaDB + Memcached egress.
	g.Expect(np.Spec.Egress).To(HaveLen(3))
	g.Expect(*np.Spec.Egress[1].Ports[0].Protocol).To(Equal(corev1.ProtocolTCP))
	g.Expect(np.Spec.Egress[1].Ports[0].Port.IntValue()).To(Equal(3306))
	g.Expect(*np.Spec.Egress[2].Ports[0].Protocol).To(Equal(corev1.ProtocolTCP))
	g.Expect(np.Spec.Egress[2].Ports[0].Port.IntValue()).To(Equal(11211))
}

// --- Gateway-aware ingress rules ---

// TestBuildKeystoneNetworkPolicy_GatewayNil_NoExtraIngressPeer verifies that
// when spec.gateway is nil the ingress rules only contain the user-defined
// peers, matching the pre-existing behavior.
func TestBuildKeystoneNetworkPolicy_GatewayNil_NoExtraIngressPeer(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := npTestKeystone()
	ks.Spec.NetworkPolicy = &keystonev1alpha1.NetworkPolicySpec{
		Ingress: []keystonev1alpha1.NetworkPolicyIngressSource{
			{NamespaceSelector: map[string]string{"kubernetes.io/metadata.name": "openstack"}},
		},
	}
	// Gateway is nil — no extra peer should be appended.

	np := buildKeystoneNetworkPolicy(ks)

	g.Expect(np.Spec.Ingress).To(HaveLen(1),
		"spec.gateway nil must not add a separate ingress rule")
	g.Expect(np.Spec.Ingress[0].From).To(HaveLen(1),
		"spec.gateway nil must not add an extra peer to the ingress rule")
	g.Expect(np.Spec.Ingress[0].From[0].NamespaceSelector.MatchLabels).To(
		HaveKeyWithValue("kubernetes.io/metadata.name", "openstack"),
	)
}

// TestBuildKeystoneNetworkPolicy_GatewaySet_AppendsIngressPeerForGatewayNamespace
// verifies that when both spec.gateway and spec.networkPolicy are set, an
// additional ingress peer is appended targeting the Gateway's namespace on
// TCP 5000, so the Gateway data-plane pods can reach the Keystone Service
func TestBuildKeystoneNetworkPolicy_GatewaySet_AppendsIngressPeerForGatewayNamespace(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := npTestKeystone()
	ks.Spec.NetworkPolicy = &keystonev1alpha1.NetworkPolicySpec{
		Ingress: []keystonev1alpha1.NetworkPolicyIngressSource{
			{NamespaceSelector: map[string]string{"kubernetes.io/metadata.name": "openstack"}},
		},
	}
	ks.Spec.Gateway = &keystonev1alpha1.GatewaySpec{
		ParentRef: keystonev1alpha1.GatewayParentRefSpec{
			Name:      "public-gateway",
			Namespace: "gateway-system",
		},
		Hostname: "keystone.example.com",
	}

	np := buildKeystoneNetworkPolicy(ks)

	// All peers coexist in a single ingress rule targeting TCP 5000.
	g.Expect(np.Spec.Ingress).To(HaveLen(1),
		"gateway peer must be appended to the existing TCP 5000 ingress rule")
	g.Expect(*np.Spec.Ingress[0].Ports[0].Protocol).To(Equal(corev1.ProtocolTCP))
	g.Expect(np.Spec.Ingress[0].Ports[0].Port.IntValue()).To(Equal(5000))

	g.Expect(np.Spec.Ingress[0].From).To(HaveLen(2),
		"gateway-set + networkPolicy-set must add exactly one gateway peer")

	// The first peer is the user-defined source.
	g.Expect(np.Spec.Ingress[0].From[0].NamespaceSelector.MatchLabels).To(
		HaveKeyWithValue("kubernetes.io/metadata.name", "openstack"),
	)

	// The appended peer targets the gateway's namespace by metadata.name.
	gatewayPeer := np.Spec.Ingress[0].From[1]
	g.Expect(gatewayPeer.NamespaceSelector).NotTo(BeNil())
	g.Expect(gatewayPeer.NamespaceSelector.MatchLabels).To(
		HaveKeyWithValue("kubernetes.io/metadata.name", "gateway-system"),
	)
	g.Expect(gatewayPeer.PodSelector).To(BeNil(),
		"gateway peer must not restrict pods inside the gateway namespace")
}

// TestBuildKeystoneNetworkPolicy_GatewaySet_EmptyParentNamespace_UsesKeystoneNamespace
// verifies that when spec.gateway.parentRef.namespace is empty, the Keystone
// CR's own namespace is used for the gateway ingress peer.
func TestBuildKeystoneNetworkPolicy_GatewaySet_EmptyParentNamespace_UsesKeystoneNamespace(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := npTestKeystone()
	ks.Namespace = "keystone-ns"
	ks.Spec.NetworkPolicy = &keystonev1alpha1.NetworkPolicySpec{
		Ingress: []keystonev1alpha1.NetworkPolicyIngressSource{
			{NamespaceSelector: map[string]string{"kubernetes.io/metadata.name": "openstack"}},
		},
	}
	ks.Spec.Gateway = &keystonev1alpha1.GatewaySpec{
		ParentRef: keystonev1alpha1.GatewayParentRefSpec{
			Name: "public-gateway",
			// Namespace left empty.
		},
		Hostname: "keystone.example.com",
	}

	np := buildKeystoneNetworkPolicy(ks)

	g.Expect(np.Spec.Ingress[0].From).To(HaveLen(2))
	gatewayPeer := np.Spec.Ingress[0].From[1]
	g.Expect(gatewayPeer.NamespaceSelector.MatchLabels).To(
		HaveKeyWithValue("kubernetes.io/metadata.name", "keystone-ns"),
		"empty parentRef.namespace must fall back to the Keystone CR's namespace",
	)
}

// TestReconcileNetworkPolicy_GatewaySet_NetworkPolicyNil_NoNetworkPolicyCreated
// verifies that when spec.gateway is set but spec.networkPolicy is nil, no
// NetworkPolicy is created — the gateway-aware ingress peer is only appended
// when network isolation is opted in.
func TestReconcileNetworkPolicy_GatewaySet_NetworkPolicyNil_NoNetworkPolicyCreated(t *testing.T) {
	g := NewGomegaWithT(t)
	s := npTestScheme()
	ks := npTestKeystone()
	// networkPolicy nil — existing behavior must not change.
	ks.Spec.Gateway = &keystonev1alpha1.GatewaySpec{
		ParentRef: keystonev1alpha1.GatewayParentRefSpec{
			Name:      "public-gateway",
			Namespace: "gateway-system",
		},
		Hostname: "keystone.example.com",
	}
	r := newNPTestReconciler(s, ks)

	result, err := r.reconcileNetworkPolicy(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(BeZero())

	// Verify no NetworkPolicy was created.
	var np networkingv1.NetworkPolicy
	getErr := r.Get(context.Background(), types.NamespacedName{
		Name: "test-keystone", Namespace: "default",
	}, &np)
	g.Expect(getErr).To(HaveOccurred())
	g.Expect(client.IgnoreNotFound(getErr)).To(Succeed(),
		"spec.gateway set without spec.networkPolicy must leave NetworkPolicy absent")

	// Condition should still be NotRequired.
	cond := meta.FindStatusCondition(ks.Status.Conditions, "NetworkPolicyReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Reason).To(Equal("NetworkPolicyNotRequired"))
}

func TestBuildKeystoneNetworkPolicy_AdditionalEgress(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := npTestKeystone()
	port8080 := intstr.FromInt32(8080)
	tcp := corev1.ProtocolTCP
	ks.Spec.NetworkPolicy = &keystonev1alpha1.NetworkPolicySpec{
		Ingress: []keystonev1alpha1.NetworkPolicyIngressSource{
			{NamespaceSelector: map[string]string{"kubernetes.io/metadata.name": "openstack"}},
		},
		AdditionalEgress: []networkingv1.NetworkPolicyEgressRule{
			{
				Ports: []networkingv1.NetworkPolicyPort{
					{Protocol: &tcp, Port: &port8080},
				},
			},
		},
	}

	np := buildKeystoneNetworkPolicy(ks)

	// DNS + additional egress (brownfield, no MariaDB/Memcached).
	g.Expect(np.Spec.Egress).To(HaveLen(2))
	g.Expect(np.Spec.Egress[1].Ports).To(HaveLen(1))
	g.Expect(np.Spec.Egress[1].Ports[0].Port.IntValue()).To(Equal(8080))
}

// --- Task 2.3: reconcileNetworkPolicy lifecycle unit tests ---

// --- Path 1: networkPolicy enabled — create NetworkPolicy ---

func TestReconcileNetworkPolicy_NetworkPolicySet_CreatesNetworkPolicy(t *testing.T) {
	g := NewGomegaWithT(t)
	s := npTestScheme()
	ks := npTestKeystone()
	ks.Spec.NetworkPolicy = &keystonev1alpha1.NetworkPolicySpec{
		Ingress: []keystonev1alpha1.NetworkPolicyIngressSource{
			{NamespaceSelector: map[string]string{"kubernetes.io/metadata.name": "openstack"}},
		},
	}
	r := newNPTestReconciler(s, ks)

	result, err := r.reconcileNetworkPolicy(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(BeZero())

	var np networkingv1.NetworkPolicy
	g.Expect(r.Get(context.Background(), types.NamespacedName{
		Name: "test-keystone", Namespace: "default",
	}, &np)).To(Succeed())

	g.Expect(np.OwnerReferences).To(HaveLen(1))
	g.Expect(np.OwnerReferences[0].Name).To(Equal("test-keystone"))

	// Verify NetworkPolicyReady condition is set with reason NetworkPolicyReady.
	cond := meta.FindStatusCondition(ks.Status.Conditions, "NetworkPolicyReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("NetworkPolicyReady"))
}

func TestReconcileNetworkPolicy_ConditionObservedGeneration(t *testing.T) {
	g := NewGomegaWithT(t)
	s := npTestScheme()

	// Test ObservedGeneration for the networkPolicy-enabled path.
	ks := npTestKeystone()
	ks.Generation = 7
	ks.Spec.NetworkPolicy = &keystonev1alpha1.NetworkPolicySpec{
		Ingress: []keystonev1alpha1.NetworkPolicyIngressSource{
			{NamespaceSelector: map[string]string{"kubernetes.io/metadata.name": "openstack"}},
		},
	}
	r := newNPTestReconciler(s, ks)

	_, err := r.reconcileNetworkPolicy(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())

	cond := meta.FindStatusCondition(ks.Status.Conditions, "NetworkPolicyReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.ObservedGeneration).To(Equal(int64(7)))

	// Test ObservedGeneration for the not-required path.
	ks2 := npTestKeystone()
	ks2.Generation = 12
	r2 := newNPTestReconciler(s, ks2)

	_, err = r2.reconcileNetworkPolicy(context.Background(), ks2)
	g.Expect(err).NotTo(HaveOccurred())

	cond2 := meta.FindStatusCondition(ks2.Status.Conditions, "NetworkPolicyReady")
	g.Expect(cond2).NotTo(BeNil())
	g.Expect(cond2.ObservedGeneration).To(Equal(int64(12)))
}

func TestReconcileNetworkPolicy_NetworkPolicyEnabled_NetworkPolicyUpdated(t *testing.T) {
	g := NewGomegaWithT(t)
	s := npTestScheme()
	ks := npTestKeystone()
	ks.Spec.NetworkPolicy = &keystonev1alpha1.NetworkPolicySpec{
		Ingress: []keystonev1alpha1.NetworkPolicyIngressSource{
			{NamespaceSelector: map[string]string{"kubernetes.io/metadata.name": "openstack"}},
		},
	}
	r := newNPTestReconciler(s, ks)
	ctx := context.Background()

	// First reconcile creates NetworkPolicy.
	_, err := r.reconcileNetworkPolicy(ctx, ks)
	g.Expect(err).NotTo(HaveOccurred())

	// Add a second ingress source and re-reconcile.
	ks.Spec.NetworkPolicy.Ingress = append(
		ks.Spec.NetworkPolicy.Ingress,
		keystonev1alpha1.NetworkPolicyIngressSource{
			NamespaceSelector: map[string]string{"kubernetes.io/metadata.name": "monitoring"},
		},
	)
	_, err = r.reconcileNetworkPolicy(ctx, ks)
	g.Expect(err).NotTo(HaveOccurred())

	var np networkingv1.NetworkPolicy
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: "test-keystone", Namespace: "default",
	}, &np)).To(Succeed())

	g.Expect(np.Spec.Ingress[0].From).To(HaveLen(2))
}

func TestReconcileNetworkPolicy_NetworkPolicyEnabled_NoChange_SkipsUpdate(t *testing.T) {
	g := NewGomegaWithT(t)
	s := npTestScheme()
	ks := npTestKeystone()
	ks.Spec.NetworkPolicy = &keystonev1alpha1.NetworkPolicySpec{
		Ingress: []keystonev1alpha1.NetworkPolicyIngressSource{
			{NamespaceSelector: map[string]string{"kubernetes.io/metadata.name": "openstack"}},
		},
	}

	// Track update calls to verify the snapshot-comparison guard skips
	// no-op updates.
	updateCount := 0
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(ks).
		WithStatusSubresource(&keystonev1alpha1.Keystone{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if _, ok := obj.(*networkingv1.NetworkPolicy); ok {
					updateCount++
				}
				return c.Update(ctx, obj, opts...)
			},
		}).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}
	ctx := context.Background()

	// First reconcile creates the NetworkPolicy (no update call expected).
	_, err := r.reconcileNetworkPolicy(ctx, ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(updateCount).To(Equal(0), "create path should not trigger update")

	// Second reconcile with identical spec should skip the update.
	updateCount = 0
	_, err = r.reconcileNetworkPolicy(ctx, ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(updateCount).To(Equal(0), "idempotent reconciliation should skip update when spec is unchanged")
}

// --- Path 2: networkPolicy disabled — delete NetworkPolicy ---

func TestReconcileNetworkPolicy_NetworkPolicyNil_NoExistingNP_SetsNotRequired(t *testing.T) {
	g := NewGomegaWithT(t)
	s := npTestScheme()
	ks := npTestKeystone()
	// networkPolicy is nil by default.
	r := newNPTestReconciler(s, ks)

	result, err := r.reconcileNetworkPolicy(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(BeZero())

	cond := meta.FindStatusCondition(ks.Status.Conditions, "NetworkPolicyReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("NetworkPolicyNotRequired"))
}

func TestReconcileNetworkPolicy_NetworkPolicyNil_ExistingNP_DeletesNetworkPolicy(t *testing.T) {
	g := NewGomegaWithT(t)
	s := npTestScheme()
	ks := npTestKeystone()

	// Pre-create a NetworkPolicy as if networkPolicy was previously enabled.
	existingNP := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-keystone",
			Namespace: "default",
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
		},
	}
	r := newNPTestReconciler(s, ks, existingNP)
	ctx := context.Background()

	// Verify NetworkPolicy exists before reconcile.
	var np networkingv1.NetworkPolicy
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: "test-keystone", Namespace: "default",
	}, &np)).To(Succeed())

	// reconcileNetworkPolicy with nil networkPolicy should delete the NP.
	result, err := r.reconcileNetworkPolicy(ctx, ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(BeZero())

	// Verify NetworkPolicy was deleted.
	err = r.Get(ctx, types.NamespacedName{
		Name: "test-keystone", Namespace: "default",
	}, &np)
	g.Expect(err).To(HaveOccurred())
	g.Expect(client.IgnoreNotFound(err)).To(Succeed())

	// Verify NetworkPolicyReady condition is set with reason NetworkPolicyNotRequired.
	cond := meta.FindStatusCondition(ks.Status.Conditions, "NetworkPolicyReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("NetworkPolicyNotRequired"))
}

// --- Empty ingress guard (security) ---

func TestReconcileNetworkPolicy_EmptyIngress_ReturnsError(t *testing.T) {
	g := NewGomegaWithT(t)
	s := npTestScheme()
	ks := npTestKeystone()
	ks.Spec.NetworkPolicy = &keystonev1alpha1.NetworkPolicySpec{
		Ingress: []keystonev1alpha1.NetworkPolicyIngressSource{}, // empty — bypassed validation
	}
	r := newNPTestReconciler(s, ks)

	_, err := r.reconcileNetworkPolicy(context.Background(), ks)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("spec.networkPolicy.ingress must not be empty"))

	// Verify no NetworkPolicy was created (fail closed).
	var np networkingv1.NetworkPolicy
	getErr := r.Get(context.Background(), types.NamespacedName{
		Name: "test-keystone", Namespace: "default",
	}, &np)
	g.Expect(getErr).To(HaveOccurred())
	g.Expect(client.IgnoreNotFound(getErr)).To(Succeed())
}

// --- Path 3: error scenarios ---

func TestReconcileNetworkPolicy_EnsureError_Propagated(t *testing.T) {
	g := NewGomegaWithT(t)
	s := npTestScheme()
	ks := npTestKeystone()
	ks.Spec.NetworkPolicy = &keystonev1alpha1.NetworkPolicySpec{
		Ingress: []keystonev1alpha1.NetworkPolicyIngressSource{
			{NamespaceSelector: map[string]string{"kubernetes.io/metadata.name": "openstack"}},
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(ks).
		WithStatusSubresource(&keystonev1alpha1.Keystone{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*networkingv1.NetworkPolicy); ok {
					return fmt.Errorf("simulated NetworkPolicy creation error")
				}
				return c.Create(ctx, obj, opts...)
			},
		}).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	_, err := r.reconcileNetworkPolicy(context.Background(), ks)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("ensuring NetworkPolicy"))
	g.Expect(err.Error()).To(ContainSubstring("simulated NetworkPolicy creation error"))
}

func TestReconcileNetworkPolicy_DeleteError_Propagated(t *testing.T) {
	g := NewGomegaWithT(t)
	s := npTestScheme()
	ks := npTestKeystone()
	// networkPolicy is nil — triggers delete path.

	existingNP := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-keystone",
			Namespace: "default",
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(ks, existingNP).
		WithStatusSubresource(&keystonev1alpha1.Keystone{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if _, ok := obj.(*networkingv1.NetworkPolicy); ok {
					return fmt.Errorf("simulated NetworkPolicy deletion error")
				}
				return c.Delete(ctx, obj, opts...)
			},
		}).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	_, err := r.reconcileNetworkPolicy(context.Background(), ks)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("deleting NetworkPolicy"))
	g.Expect(err.Error()).To(ContainSubstring("simulated NetworkPolicy deletion error"))
}

// TestBuildKeystoneNetworkPolicy_NameMatchesCR pins the NetworkPolicy
// ObjectMeta.Name to the bare CR name. Symmetric with the Deployment, Service,
// PDB, and HPA name guards: the rename must hit every operator-managed
// sub-resource.
func TestBuildKeystoneNetworkPolicy_NameMatchesCR(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := npTestKeystone()
	ks.Spec.NetworkPolicy = &keystonev1alpha1.NetworkPolicySpec{
		Ingress: []keystonev1alpha1.NetworkPolicyIngressSource{
			{NamespaceSelector: map[string]string{"kubernetes.io/metadata.name": "openstack"}},
		},
	}

	np := buildKeystoneNetworkPolicy(ks)

	g.Expect(np.Name).To(Equal(ks.Name),
		"NetworkPolicy Name must equal the CR name")
	g.Expect(np.Name).NotTo(HaveSuffix("-api"),
		"NetworkPolicy Name must not carry the legacy `-api` suffix")
}
