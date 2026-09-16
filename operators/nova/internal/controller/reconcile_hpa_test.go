// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
)

func novaAutoscalingSpec(minR, maxR int32) *novav1alpha1.AutoscalingSpec {
	return &novav1alpha1.AutoscalingSpec{
		MinReplicas:          ptr.To(minR),
		MaxReplicas:          maxR,
		TargetCPUUtilization: ptr.To(int32(80)),
	}
}

func TestReconcileHPA_DisabledDeletesAndNotRequired(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	stale := &autoscalingv2.HorizontalPodAutoscaler{}
	stale.Name = testNovaName
	stale.Namespace = testNamespace
	r := newNovaTestReconciler(nova, stale)

	res, err := r.reconcileHPA(context.Background(), r.Client, nova)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := novaCondition(nova, "HPAReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("HPANotRequired"))

	var gone autoscalingv2.HorizontalPodAutoscaler
	g.Expect(r.Get(context.Background(), objectKey(testNovaName), &gone)).NotTo(Succeed(),
		"the stale HPA must be deleted when autoscaling is disabled")
}

func TestReconcileHPA_EnabledCreatesHPA(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	nova.Spec.Autoscaling = novaAutoscalingSpec(2, 5)
	r := newNovaTestReconciler(nova)

	res, err := r.reconcileHPA(context.Background(), r.Client, nova)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())

	var hpa autoscalingv2.HorizontalPodAutoscaler
	g.Expect(r.Get(context.Background(), objectKey(testNovaName), &hpa)).To(Succeed())
	g.Expect(hpa.Spec.ScaleTargetRef.Kind).To(Equal("Deployment"))
	g.Expect(hpa.Spec.ScaleTargetRef.Name).To(Equal(testNovaName),
		"the HPA scales the API Deployment, which carries the CR's own name")
	g.Expect(hpa.Spec.MinReplicas).To(HaveValue(Equal(int32(2))))
	g.Expect(hpa.Spec.MaxReplicas).To(Equal(int32(5)))

	cond := novaCondition(nova, "HPAReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("HPAReady"))

	// The Deployment leaves replicas unset while the HPA owns the count, so the
	// two controllers do not fight over it every reconcile.
	g.Expect(buildAPIDeployment(nova, workloadArtifacts(), workloadDigests{}).Spec.Replicas).To(BeNil())
}

// One Nova owns five Deployments and only the API is autoscaled: the metadata
// API follows the instance population, and the scheduler, the conductor and the
// console proxy answer no request rate at all.
func TestBuildNovaHPA_TargetsTheAPIDeploymentAlone(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	nova.Spec.Autoscaling = novaAutoscalingSpec(2, 5)

	hpa := buildNovaHPA(nova)

	art := workloadArtifacts()
	g.Expect(hpa.Spec.ScaleTargetRef.Name).
		To(Equal(buildAPIDeployment(nova, art, workloadDigests{}).Name))
	others := map[string]*appsv1.Deployment{
		"metadata":      buildMetadataDeployment(nova, art, workloadDigests{}),
		"scheduler":     buildSchedulerDeployment(nova, art, workloadDigests{}, testEgressPort),
		"conductor":     buildConductorDeployment(nova, art, workloadDigests{}, testEgressPort),
		"console proxy": buildConsoleProxyDeployment(nova, art, workloadDigests{}),
	}
	for component, deploy := range others {
		g.Expect(hpa.Spec.ScaleTargetRef.Name).NotTo(Equal(deploy.Name))
		g.Expect(deploy.Spec.Replicas).NotTo(BeNil(),
			"no HPA owns the %s replica count, so the Deployment has to set it", component)
	}
}

// MinReplicas is optional on the API: leaving it unset must track the
// Deployment's effective replica count rather than collapse the floor to one.
func TestBuildNovaHPA_MinReplicasFallsBackToTheDeployment(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	nova.Spec.API.Deployment.Replicas = 3
	nova.Spec.Autoscaling = &novav1alpha1.AutoscalingSpec{MaxReplicas: 6}

	hpa := buildNovaHPA(nova)

	g.Expect(hpa.Spec.MinReplicas).To(HaveValue(Equal(int32(3))))
	g.Expect(hpa.Spec.MaxReplicas).To(Equal(int32(6)))
}
