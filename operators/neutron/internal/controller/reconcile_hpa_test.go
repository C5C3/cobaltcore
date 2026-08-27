// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
)

func neutronAutoscalingSpec(minR, maxR int32) *neutronv1alpha1.AutoscalingSpec {
	return &neutronv1alpha1.AutoscalingSpec{
		MinReplicas:          ptr.To(minR),
		MaxReplicas:          maxR,
		TargetCPUUtilization: ptr.To(int32(80)),
	}
}

func TestReconcileHPA_DisabledDeletesAndNotRequired(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()
	stale := &autoscalingv2.HorizontalPodAutoscaler{}
	stale.Name = testNeutronName
	stale.Namespace = testNamespace
	r := newNeutronTestReconciler(neutron, stale)

	res, err := r.reconcileHPA(context.Background(), r.Client, neutron)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := neutronCondition(neutron, "HPAReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("HPANotRequired"))

	var gone autoscalingv2.HorizontalPodAutoscaler
	err = r.Get(context.Background(), neutronKey(neutron), &gone)
	g.Expect(err).To(HaveOccurred(), "stale HPA must be deleted when autoscaling is disabled")
}

func TestReconcileHPA_EnabledCreatesHPA(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()
	neutron.Spec.Autoscaling = neutronAutoscalingSpec(2, 5)
	r := newNeutronTestReconciler(neutron)

	res, err := r.reconcileHPA(context.Background(), r.Client, neutron)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())

	var hpa autoscalingv2.HorizontalPodAutoscaler
	g.Expect(r.Get(context.Background(), neutronKey(neutron), &hpa)).To(Succeed())
	g.Expect(hpa.Spec.ScaleTargetRef.Kind).To(Equal("Deployment"))
	g.Expect(hpa.Spec.ScaleTargetRef.Name).To(Equal(testNeutronName),
		"the HPA scales the API Deployment, which carries the CR's own name")
	g.Expect(hpa.Spec.MinReplicas).To(HaveValue(Equal(int32(2))))
	g.Expect(hpa.Spec.MaxReplicas).To(Equal(int32(5)))

	cond := neutronCondition(neutron, "HPAReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("HPAReady"))

	// The Deployment leaves replicas unset while the HPA owns the count, so the
	// two controllers do not fight over it every reconcile.
	g.Expect(buildNeutronDeployment(neutron, deploymentConfigMapName, "", "", "", "").Spec.Replicas).To(BeNil())
}

// One Neutron owns three Deployments and only the API one is autoscaled, so the
// HPA's scale target has to name it and neither worker.
func TestBuildNeutronHPA_TargetsTheAPIDeploymentAlone(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()
	neutron.Spec.Autoscaling = neutronAutoscalingSpec(2, 5)

	hpa := buildNeutronHPA(neutron)

	api := buildNeutronDeployment(neutron, deploymentConfigMapName, "", "", "", "")
	g.Expect(hpa.Spec.ScaleTargetRef.Name).To(Equal(api.Name))
	for _, component := range []string{componentPeriodicWorkers, componentOVNMaintenanceWorker} {
		worker := buildWorkerDeployment(neutron, component, nil, deploymentConfigMapName, "", "", "", "")
		g.Expect(hpa.Spec.ScaleTargetRef.Name).NotTo(Equal(worker.Name))
		g.Expect(worker.Spec.Replicas).NotTo(BeNil(),
			"no HPA owns the worker replica count, so the Deployment has to set it")
	}
}
