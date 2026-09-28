// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	vpav1 "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/apis/autoscaling.k8s.io/v1"

	"github.com/c5c3/cobaltcore/internal/common/deployment"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

// The API and scheduler Deployments are always targets; the volume service
// yields one target per projected backend, every one fed by
// spec.volume.deployment; the backup Deployment is a target only while a
// backup target is projected.
func TestCinderVPATargets(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	backends := []backendProjection{testBackendProjection("nfs"), testBackendProjection("nfs-second")}

	g.Expect(cinderVPATargets(cinder, nil, nil)).To(Equal([]deployment.VPATarget{
		{Kind: "Deployment", Name: cinder.Name},
		{Kind: "Deployment", Name: cinder.Name + "-scheduler"},
	}), "without backends and a backup target only the API and scheduler are targets")

	api := &commonv1.VerticalAutoscalingSpec{UpdateMode: "Off"}
	scheduler := &commonv1.VerticalAutoscalingSpec{UpdateMode: "Auto"}
	volume := &commonv1.VerticalAutoscalingSpec{UpdateMode: "Initial"}
	backup := &commonv1.VerticalAutoscalingSpec{UpdateMode: "Recreate"}
	cinder.Spec.API.Deployment.VerticalAutoscaling = api
	cinder.Spec.Scheduler.Deployment.VerticalAutoscaling = scheduler
	cinder.Spec.Volume.Deployment.VerticalAutoscaling = volume
	cinder.Spec.Backup.Deployment.VerticalAutoscaling = backup

	g.Expect(cinderVPATargets(cinder, backends, testBackupProjection())).To(Equal([]deployment.VPATarget{
		{Kind: "Deployment", Name: cinder.Name, Spec: api},
		{Kind: "Deployment", Name: cinder.Name + "-scheduler", Spec: scheduler},
		{Kind: "Deployment", Name: cinder.Name + "-volume-nfs", Spec: volume},
		{Kind: "Deployment", Name: cinder.Name + "-volume-nfs-second", Spec: volume},
		{Kind: "Deployment", Name: cinder.Name + "-backup", Spec: backup},
	}))

	g.Expect(cinderVPATargets(cinder, backends, nil)).NotTo(ContainElement(HaveField("Name", cinder.Name+"-backup")),
		"a backup block without a projected backup target renders no Deployment and no VPA")
}

// A detached backend drops out of the projection, so its VPA is pruned with
// its Deployment while the remaining backend keeps its own.
func TestReconcileVPA_DetachedBackendLosesItsVPA(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	cinder.Spec.Volume.Deployment.VerticalAutoscaling = &commonv1.VerticalAutoscalingSpec{UpdateMode: "Off"}
	r := newCinderTestReconciler(cinder)
	r.vpaAvailable = true
	ctx := context.Background()

	_, err := r.reconcileVPA(ctx, r.Client, cinder,
		[]backendProjection{testBackendProjection("nfs"), testBackendProjection("nfs-second")}, nil)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(vpaNames(t, r)).To(ConsistOf(cinder.Name+"-volume-nfs", cinder.Name+"-volume-nfs-second"))

	_, err = r.reconcileVPA(ctx, r.Client, cinder, []backendProjection{testBackendProjection("nfs")}, nil)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(vpaNames(t, r)).To(ConsistOf(cinder.Name + "-volume-nfs"))
	cond := cinderCondition(cinder, "VPAReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(deployment.ReasonVPAReady))
}

func TestReconcileVPA_LatchFalse_SetsNotInstalled(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	cinder.Spec.Scheduler.Deployment.VerticalAutoscaling = &commonv1.VerticalAutoscalingSpec{UpdateMode: "Off"}
	r := newCinderTestReconciler(cinder)

	_, err := r.reconcileVPA(context.Background(), r.Client, cinder, nil, nil)
	g.Expect(err).NotTo(HaveOccurred())

	cond := cinderCondition(cinder, "VPAReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(deployment.ReasonVPANotInstalled))
	g.Expect(cond.Message).To(ContainSubstring(cinder.Name + "-scheduler"))
	g.Expect(vpaNames(t, r)).To(BeEmpty())
}

func vpaNames(t *testing.T, r *CinderReconciler) []string {
	t.Helper()
	var list vpav1.VerticalPodAutoscalerList
	NewGomegaWithT(t).Expect(r.List(context.Background(), &list)).To(Succeed())
	names := make([]string, 0, len(list.Items))
	for _, vpa := range list.Items {
		names = append(names, vpa.Name)
	}
	return names
}
