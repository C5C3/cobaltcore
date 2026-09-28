// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	vpav1 "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/apis/autoscaling.k8s.io/v1"
	"k8s.io/utils/ptr"

	"github.com/c5c3/cobaltcore/internal/common/deployment"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	"github.com/c5c3/cobaltcore/operators/nova/internal/computeapi/computeapitest"
)

// The API, metadata, scheduler and conductor Deployments are always targets,
// each fed by its own block; the console proxy is a target only while it is
// enabled, fed by the block the proxy Deployment is rendered from.
func TestNovaVPATargets(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	nova.Spec.ConsoleProxy.Enabled = ptr.To(true)
	api := &commonv1.VerticalAutoscalingSpec{UpdateMode: "Off"}
	metadata := &commonv1.VerticalAutoscalingSpec{UpdateMode: "Initial"}
	scheduler := &commonv1.VerticalAutoscalingSpec{UpdateMode: "Recreate"}
	conductor := &commonv1.VerticalAutoscalingSpec{UpdateMode: "Auto"}
	console := &commonv1.VerticalAutoscalingSpec{UpdateMode: "Off", MinReplicas: ptr.To(int32(1))}
	nova.Spec.API.Deployment.VerticalAutoscaling = api
	nova.Spec.Metadata.Deployment.VerticalAutoscaling = metadata
	nova.Spec.Scheduler.Deployment.VerticalAutoscaling = scheduler
	nova.Spec.Conductor.Deployment.VerticalAutoscaling = conductor
	nova.Spec.ConsoleProxy.Deployment = &commonv1.DeploymentSpec{Replicas: 1, VerticalAutoscaling: console}

	g.Expect(novaVPATargets(nova)).To(Equal([]deployment.VPATarget{
		{Kind: "Deployment", Name: testNovaName, Spec: api},
		{Kind: "Deployment", Name: testNovaName + "-metadata", Spec: metadata},
		{Kind: "Deployment", Name: testNovaName + "-scheduler", Spec: scheduler},
		{Kind: "Deployment", Name: testNovaName + "-conductor", Spec: conductor},
		{Kind: "Deployment", Name: testNovaName + "-novncproxy", Spec: console},
	}))

	nova.Spec.ConsoleProxy.Enabled = ptr.To(false)
	g.Expect(novaVPATargets(nova)).To(HaveLen(4), "a disabled console proxy renders no Deployment and no VPA")
	g.Expect(novaVPATargets(nova)).NotTo(ContainElement(HaveField("Name", testNovaName+"-novncproxy")))
}

// An enabled proxy without a deployment block is rendered from the defaulted
// block, which carries no opt-in.
func TestNovaVPATargets_ConsoleProxyWithoutBlock(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	nova.Spec.ConsoleProxy.Enabled = nil
	nova.Spec.ConsoleProxy.Deployment = nil

	targets := novaVPATargets(nova)
	g.Expect(targets).To(HaveLen(5))
	g.Expect(targets[4]).To(Equal(deployment.VPATarget{Kind: "Deployment", Name: testNovaName + "-novncproxy"}))
}

func TestReconcileVPA_ConductorOptInCreatesTheVPA(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	nova.Spec.Conductor.Deployment.VerticalAutoscaling = &commonv1.VerticalAutoscalingSpec{UpdateMode: "Initial"}
	r := newNovaTestReconciler(nova)
	r.vpaAvailable = true

	_, err := r.reconcileVPA(context.Background(), r.Client, nova)
	g.Expect(err).NotTo(HaveOccurred())

	var vpa vpav1.VerticalPodAutoscaler
	g.Expect(r.Get(context.Background(), types.NamespacedName{Name: testNovaName + "-conductor", Namespace: nova.Namespace}, &vpa)).To(Succeed())
	g.Expect(vpa.Spec.UpdatePolicy.UpdateMode).To(HaveValue(Equal(vpav1.UpdateModeInitial)))
	g.Expect(metav1.IsControlledBy(&vpa, nova)).To(BeTrue())
	cond := novaCondition(nova, "VPAReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Reason).To(Equal(deployment.ReasonVPAReady))
}

func TestReconcileVPA_NovaLatchFalse_SetsNotInstalled(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	nova.Spec.Scheduler.Deployment.VerticalAutoscaling = &commonv1.VerticalAutoscalingSpec{UpdateMode: "Off"}
	r := newNovaTestReconciler(nova)

	_, err := r.reconcileVPA(context.Background(), r.Client, nova)
	g.Expect(err).NotTo(HaveOccurred())
	cond := novaCondition(nova, "VPAReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(deployment.ReasonVPANotInstalled))
	g.Expect(cond.Message).To(ContainSubstring(testNovaName + "-scheduler"))
}

// The pool's DaemonSet is its one target. It opts in when the DaemonSet step
// of the pass rendered the DaemonSet, and not when the step deleted it.
func TestNovaComputeVPATargets(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := validNovaCompute()
	cr.Spec.VerticalAutoscaling = &commonv1.VerticalAutoscalingSpec{UpdateMode: "Off"}

	g.Expect(novaComputeVPATargets(cr, &novaComputePass{daemonSetRendered: true})).To(Equal([]deployment.VPATarget{
		{Kind: "DaemonSet", Name: cr.Name + "-nova-compute", Spec: cr.Spec.VerticalAutoscaling},
	}))
	g.Expect(novaComputeVPATargets(cr, &novaComputePass{})[0].Spec).To(BeNil(),
		"a DaemonSet the step deleted gets no VPA")
}

func TestReconcileComputeVPA_LatchFalse_SetsNotInstalled(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := validNovaCompute()
	cr.Spec.VerticalAutoscaling = &commonv1.VerticalAutoscalingSpec{UpdateMode: "Off"}
	r := newNovaComputeTestReconciler(computeapitest.New(), cr)

	_, err := r.reconcileVPA(context.Background(), r.Client, cr, &novaComputePass{daemonSetRendered: true})
	g.Expect(err).NotTo(HaveOccurred())
	cond := apimeta.FindStatusCondition(cr.Status.Conditions, "VPAReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(deployment.ReasonVPANotInstalled))
}

func TestReconcileComputeVPA_OptedInCreatesTheVPA(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := validNovaCompute()
	cr.Spec.VerticalAutoscaling = &commonv1.VerticalAutoscalingSpec{UpdateMode: "Off"}
	r := newNovaComputeTestReconciler(computeapitest.New(), cr)
	r.vpaAvailable = true

	_, err := r.reconcileVPA(context.Background(), r.Client, cr, &novaComputePass{daemonSetRendered: true})
	g.Expect(err).NotTo(HaveOccurred())
	var vpa vpav1.VerticalPodAutoscaler
	g.Expect(r.Get(context.Background(), types.NamespacedName{Name: cr.Name + "-nova-compute", Namespace: cr.Namespace}, &vpa)).To(Succeed())
	g.Expect(vpa.Spec.TargetRef.Kind).To(Equal("DaemonSet"))
	g.Expect(vpa.Labels).To(HaveKeyWithValue("app.kubernetes.io/name", novaComputeAppName))
}

// The whole pipeline hands the DaemonSet step's decision to the VPA step of the
// same pass: an opted-in pool whose DaemonSet was applied gets its VPA.
func TestNovaComputeReconcile_OptedInPoolGetsItsVPA(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := validNovaCompute()
	cr.Finalizers = []string{novaComputeDrainFinalizer}
	cr.Spec.VerticalAutoscaling = &commonv1.VerticalAutoscalingSpec{UpdateMode: "Off"}
	r := newNovaComputeTestReconciler(computeapitest.New(), append(controlPlaneObjects(), cr, selectedNode(testNodeName))...)
	r.vpaAvailable = true

	reconcilePool(t, r)

	g.Expect(r.Get(context.Background(), novaComputeDaemonSetKey, &vpav1.VerticalPodAutoscaler{})).To(Succeed())
	cond := apimeta.FindStatusCondition(getPool(t, r).Status.Conditions, "VPAReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Reason).To(Equal(deployment.ReasonVPAReady))
}
