// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/c5c3/forge/internal/common/conditions"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	glancev1alpha1 "github.com/c5c3/forge/operators/glance/api/v1alpha1"
)

func glanceNetworkPolicySpec() *glancev1alpha1.NetworkPolicySpec {
	return &glancev1alpha1.NetworkPolicySpec{
		Ingress: []glancev1alpha1.NetworkPolicyIngressSource{{
			NamespaceSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{"kubernetes.io/metadata.name": "monitoring"},
			},
		}},
	}
}

func TestReconcileNetworkPolicy_DisabledDeletesAndNotRequired(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	stale := &networkingv1.NetworkPolicy{}
	stale.Name = "test-glance"
	stale.Namespace = "default"
	r := newGlanceTestReconciler(glance, stale)

	res, err := r.reconcileNetworkPolicy(context.Background(), glance, backendsProjection{})

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := conditions.GetCondition(glance.Status.Conditions, conditionTypeNetworkPolicyReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonNetworkPolicyNotRequired))

	var gone networkingv1.NetworkPolicy
	err = r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-glance"}, &gone)
	g.Expect(err).To(HaveOccurred(), "stale NetworkPolicy must be deleted when disabled")
}

func TestReconcileNetworkPolicy_EmptyIngressFailsClosed(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	glance.Spec.NetworkPolicy = &glancev1alpha1.NetworkPolicySpec{}
	r := newGlanceTestReconciler(glance)

	_, err := r.reconcileNetworkPolicy(context.Background(), glance, backendsProjection{})

	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("refusing to create NetworkPolicy that would allow all ingress"))
}

func TestBuildGlanceNetworkPolicy_IngressAndEgressRules(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	glance.Spec.NetworkPolicy = glanceNetworkPolicySpec()

	// Two attached backends' hosts drive the S3 egress; testGlance's cache and
	// database provide the cache/DB egress.
	np := buildGlanceNetworkPolicy(glance, "glance-system", []string{"https://s3.example.com"})

	g.Expect(np.Spec.PodSelector.MatchLabels).To(Equal(selectorLabels(glance)))
	// Ingress restricted to the API port from the user source + operator peer.
	g.Expect(np.Spec.Ingress).To(HaveLen(1))
	g.Expect(np.Spec.Ingress[0].Ports).To(HaveLen(1))
	g.Expect(np.Spec.Ingress[0].Ports[0].Port.IntValue()).To(Equal(9292))
	g.Expect(np.Spec.Ingress[0].From).To(HaveLen(2))
	g.Expect(np.Spec.Ingress[0].From[1].NamespaceSelector.MatchLabels).
		To(HaveKeyWithValue("kubernetes.io/metadata.name", "glance-system"))

	// Egress: DNS (53), database (3306), cache (11211), S3 (443).
	g.Expect(np.Spec.Egress).To(HaveLen(4))
	g.Expect(np.Spec.Egress[0].Ports[0].Port.IntValue()).To(Equal(53))
	g.Expect(np.Spec.Egress[1].Ports[0].Port.IntValue()).To(Equal(3306))
	g.Expect(np.Spec.Egress[2].Ports[0].Port.IntValue()).To(Equal(11211))
	g.Expect(np.Spec.Egress[3].Ports[0].Port.IntValue()).To(Equal(443))
}

func TestBuildGlanceNetworkPolicy_OmitsCacheAndS3WhenAbsent(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	glance.Spec.NetworkPolicy = glanceNetworkPolicySpec()
	// No cache configured (bypassed XOR) and no attached backend hosts.
	glance.Spec.Cache = commonv1.CacheSpec{}

	np := buildGlanceNetworkPolicy(glance, "", nil)

	// Only DNS and database egress remain.
	g.Expect(np.Spec.Egress).To(HaveLen(2))
	g.Expect(np.Spec.Egress[0].Ports[0].Port.IntValue()).To(Equal(53))
	g.Expect(np.Spec.Egress[1].Ports[0].Port.IntValue()).To(Equal(3306))
}

func TestBuildGlanceNetworkPolicy_AdditionalEgressAppended(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	np9999 := intstr.FromInt32(9999)
	tcp := corev1.ProtocolTCP
	glance.Spec.NetworkPolicy = glanceNetworkPolicySpec()
	glance.Spec.NetworkPolicy.AdditionalEgress = []networkingv1.NetworkPolicyEgressRule{{
		Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &np9999}},
	}}

	np := buildGlanceNetworkPolicy(glance, "", []string{"https://s3.example.com"})

	// Auto rules (DNS, DB, cache, S3) then the user-supplied additional rule last.
	last := np.Spec.Egress[len(np.Spec.Egress)-1]
	g.Expect(last.Ports[0].Port.IntValue()).To(Equal(9999))
}
