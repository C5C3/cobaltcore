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

	cinderv1alpha1 "github.com/c5c3/cobaltcore/operators/cinder/api/v1alpha1"
)

func cinderAutoscalingSpec(minR, maxR int32) *cinderv1alpha1.AutoscalingSpec {
	return &cinderv1alpha1.AutoscalingSpec{
		MinReplicas:          ptr.To(minR),
		MaxReplicas:          maxR,
		TargetCPUUtilization: ptr.To(int32(80)),
	}
}

func TestReconcileHPA_DisabledDeletesAndNotRequired(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	stale := &autoscalingv2.HorizontalPodAutoscaler{}
	stale.Name = testCinderName
	stale.Namespace = testNamespace
	r := newCinderTestReconciler(cinder, stale)

	res, err := r.reconcileHPA(context.Background(), r.Client, cinder)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := cinderCondition(cinder, "HPAReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("HPANotRequired"))

	var gone autoscalingv2.HorizontalPodAutoscaler
	err = r.Get(context.Background(), objectKey(testCinderName), &gone)
	g.Expect(err).To(HaveOccurred(), "stale HPA must be deleted when autoscaling is disabled")
}

func TestReconcileHPA_EnabledCreatesHPA(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := workloadCinder()
	cinder.Spec.Autoscaling = cinderAutoscalingSpec(2, 5)
	r := newCinderTestReconciler(cinder)

	res, err := r.reconcileHPA(context.Background(), r.Client, cinder)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())

	var hpa autoscalingv2.HorizontalPodAutoscaler
	g.Expect(r.Get(context.Background(), objectKey(testCinderName), &hpa)).To(Succeed())
	g.Expect(hpa.Spec.ScaleTargetRef.Kind).To(Equal("Deployment"))
	g.Expect(hpa.Spec.ScaleTargetRef.Name).To(Equal(testCinderName),
		"the HPA scales the API Deployment, which carries the CR's own name")
	g.Expect(hpa.Spec.MinReplicas).To(HaveValue(Equal(int32(2))))
	g.Expect(hpa.Spec.MaxReplicas).To(Equal(int32(5)))

	cond := cinderCondition(cinder, "HPAReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("HPAReady"))

	// The Deployment leaves replicas unset while the HPA owns the count, so the
	// two controllers do not fight over it every reconcile.
	g.Expect(buildCinderDeployment(cinder, workloadArtifacts(), workloadDigests{}).Spec.Replicas).To(BeNil())
}

// One Cinder owns the API, the scheduler, one volume service per backend and the
// backup service, and only the API is autoscaled: the other three are pinned at
// one replica because each owns a host identity a second pod would compete for.
func TestBuildCinderHPA_TargetsTheAPIDeploymentAlone(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := workloadCinder()
	cinder.Spec.Autoscaling = cinderAutoscalingSpec(2, 5)

	hpa := buildCinderHPA(cinder)

	g.Expect(hpa.Spec.ScaleTargetRef.Name).
		To(Equal(buildCinderDeployment(cinder, workloadArtifacts(), workloadDigests{}).Name))
	scheduler := buildSchedulerDeployment(cinder, workloadArtifacts(), workloadDigests{}, testEgressPort)
	g.Expect(hpa.Spec.ScaleTargetRef.Name).NotTo(Equal(scheduler.Name))
	g.Expect(scheduler.Spec.Replicas).NotTo(BeNil(),
		"no HPA owns the scheduler replica count, so the Deployment has to set it")
}

// MinReplicas is optional on the API: leaving it unset must track the
// Deployment's effective replica count rather than collapse the floor to one.
func TestBuildCinderHPA_MinReplicasFallsBackToTheDeployment(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := workloadCinder()
	cinder.Spec.API.Deployment.Replicas = 3
	cinder.Spec.Autoscaling = &cinderv1alpha1.AutoscalingSpec{MaxReplicas: 6}

	hpa := buildCinderHPA(cinder)

	g.Expect(hpa.Spec.MinReplicas).To(HaveValue(Equal(int32(3))))
	g.Expect(hpa.Spec.MaxReplicas).To(Equal(int32(6)))
}
