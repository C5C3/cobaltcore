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
	vpav1 "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/apis/autoscaling.k8s.io/v1"

	"github.com/c5c3/cobaltcore/internal/common/deployment"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

// A Neutron runs three long-running workloads: the API Deployment, fed by
// spec.deployment, and the two worker Deployments, both fed by
// spec.workers.deployment.
func TestNeutronVPATargets(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()

	g.Expect(neutronVPATargets(neutron)).To(Equal([]deployment.VPATarget{
		{Kind: "Deployment", Name: testNeutronName},
		{Kind: "Deployment", Name: testNeutronName + "-periodic-workers"},
		{Kind: "Deployment", Name: testNeutronName + "-ovn-maintenance-worker"},
	}), "a CR without any block lists every target without a spec")

	api := &commonv1.VerticalAutoscalingSpec{UpdateMode: "Initial"}
	workers := &commonv1.VerticalAutoscalingSpec{UpdateMode: "Off"}
	neutron.Spec.Deployment.VerticalAutoscaling = api
	neutron.Spec.Workers.Deployment.VerticalAutoscaling = workers
	targets := neutronVPATargets(neutron)
	g.Expect(targets[0].Spec).To(BeIdenticalTo(api))
	g.Expect(targets[1].Spec).To(BeIdenticalTo(workers))
	g.Expect(targets[2].Spec).To(BeIdenticalTo(workers))
}

// The worker opt-in creates one VPA per worker Deployment and none for the API
// Deployment, which does not opt in.
func TestReconcileVPA_WorkersOptInCreatesTwoVPAs(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()
	neutron.Spec.Workers.Deployment.VerticalAutoscaling = &commonv1.VerticalAutoscalingSpec{UpdateMode: "Off"}
	r := newNeutronTestReconciler(neutron)
	r.vpaAvailable = true

	_, err := r.reconcileVPA(context.Background(), r.Client, neutron)
	g.Expect(err).NotTo(HaveOccurred())

	var list vpav1.VerticalPodAutoscalerList
	g.Expect(r.List(context.Background(), &list)).To(Succeed())
	var names []string
	for _, vpa := range list.Items {
		names = append(names, vpa.Name)
		g.Expect(vpa.Spec.TargetRef.Name).To(Equal(vpa.Name))
		g.Expect(metav1.IsControlledBy(&vpa, neutron)).To(BeTrue())
	}
	g.Expect(names).To(ConsistOf(testNeutronName+"-periodic-workers", testNeutronName+"-ovn-maintenance-worker"))
	cond := neutronCondition(neutron, "VPAReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(deployment.ReasonVPAReady))
}

func TestReconcileVPA_NeutronLatchFalse_SetsNotInstalled(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()
	neutron.Spec.Deployment.VerticalAutoscaling = &commonv1.VerticalAutoscalingSpec{UpdateMode: "Off"}
	r := newNeutronTestReconciler(neutron)

	_, err := r.reconcileVPA(context.Background(), r.Client, neutron)
	g.Expect(err).NotTo(HaveOccurred())

	cond := neutronCondition(neutron, "VPAReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(deployment.ReasonVPANotInstalled))
	g.Expect(cond.Message).To(ContainSubstring("opted-in workloads: " + testNeutronName + " ("))
}

// The agent's one long-running workload is its DaemonSet, fed by
// spec.verticalAutoscaling.
func TestAgentVPATargets(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := validAgent()

	g.Expect(agentVPATargets(cr)).To(Equal([]deployment.VPATarget{
		{Kind: "DaemonSet", Name: cr.Name + "-metadata-agent"},
	}))

	cr.Spec.VerticalAutoscaling = &commonv1.VerticalAutoscalingSpec{UpdateMode: "Off"}
	g.Expect(agentVPATargets(cr)[0].Spec).To(BeIdenticalTo(cr.Spec.VerticalAutoscaling))
}

func TestReconcileAgentVPA_OptedInCreatesTheVPA(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := validAgent()
	cr.Spec.VerticalAutoscaling = &commonv1.VerticalAutoscalingSpec{UpdateMode: "Off"}
	r := newAgentTestReconciler(cr)
	r.vpaAvailable = true

	_, err := r.reconcileVPA(context.Background(), r.Client, cr)
	g.Expect(err).NotTo(HaveOccurred())

	var vpa vpav1.VerticalPodAutoscaler
	g.Expect(r.Get(context.Background(), types.NamespacedName{Name: agentDaemonSetName(cr), Namespace: cr.Namespace}, &vpa)).To(Succeed())
	g.Expect(vpa.Spec.TargetRef.Kind).To(Equal("DaemonSet"))
	g.Expect(vpa.Labels).To(HaveKeyWithValue("app.kubernetes.io/name", metadataAgentAppName))
	cond := agentCondition(cr, "VPAReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Reason).To(Equal(deployment.ReasonVPAReady))
}

func TestReconcileAgentVPA_LatchFalse_SetsNotInstalled(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := validAgent()
	cr.Spec.VerticalAutoscaling = &commonv1.VerticalAutoscalingSpec{UpdateMode: "Off"}
	r := newAgentTestReconciler(cr)

	_, err := r.reconcileVPA(context.Background(), r.Client, cr)
	g.Expect(err).NotTo(HaveOccurred())
	cond := agentCondition(cr, "VPAReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(deployment.ReasonVPANotInstalled))
	g.Expect(cond.Message).To(ContainSubstring(agentDaemonSetName(cr)))
}
