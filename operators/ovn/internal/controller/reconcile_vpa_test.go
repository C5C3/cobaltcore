// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	vpav1 "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/apis/autoscaling.k8s.io/v1"

	"github.com/c5c3/cobaltcore/internal/common/deployment"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// northd is always a target, the relay only while spec.relay is set, and the
// two Raft StatefulSets always, each fed by its own block.
func TestCentralVPATargets(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := testOVNCentral()
	cr.Spec.Relay = nil

	g.Expect(centralVPATargets(cr)).To(Equal([]deployment.VPATarget{
		{Kind: "Deployment", Name: cr.Name + "-northd"},
		{Kind: "StatefulSet", Name: cr.Name + "-nb"},
		{Kind: "StatefulSet", Name: cr.Name + "-sb"},
	}), "without spec.relay and without any block the targets carry no spec")

	northd := &commonv1.VerticalAutoscalingSpec{UpdateMode: "Off"}
	relay := &commonv1.VerticalAutoscalingSpec{UpdateMode: "Initial"}
	cr.Spec.Northd.Deployment.VerticalAutoscaling = northd
	cr.Spec.Relay = &ovnv1alpha1.OVNRelaySpec{Replicas: 2, VerticalAutoscaling: relay}
	targets := centralVPATargets(cr)
	g.Expect(targets).To(HaveLen(4))
	g.Expect(targets[0]).To(Equal(deployment.VPATarget{Kind: "Deployment", Name: cr.Name + "-northd", Spec: northd}))
	g.Expect(targets[1]).To(Equal(deployment.VPATarget{Kind: "Deployment", Name: cr.Name + "-sb-relay", Spec: relay}))
}

// A Raft VPA never moves a member below the request floor: minAllowed carries
// 70m CPU and 256Mi memory where the CR names neither, and a value the CR names
// is kept, even one below the floor.
func TestCentralVPATargets_RaftFloor(t *testing.T) {
	floor := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("70m"), corev1.ResourceMemory: resource.MustParse("256Mi")}
	for _, tc := range []struct {
		name string
		min  corev1.ResourceList
		want corev1.ResourceList
	}{
		{name: "none named", min: nil, want: floor},
		{
			name: "memory named below the floor",
			min:  corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")},
			want: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("70m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
		},
		{
			name: "cpu named, memory filled",
			min:  corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
			want: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("256Mi")},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cr := testOVNCentral()
			cr.Spec.Northbound.VerticalAutoscaling = &commonv1.VerticalAutoscalingSpec{UpdateMode: "Off", MinAllowed: tc.min}
			cr.Spec.Southbound.VerticalAutoscaling = &commonv1.VerticalAutoscalingSpec{UpdateMode: "Off", MinAllowed: tc.min}
			before := cr.DeepCopy()

			for _, target := range centralVPATargets(cr) {
				if target.Kind != "StatefulSet" {
					continue
				}
				g.Expect(target.Spec.MinAllowed).To(HaveLen(2))
				for name, q := range tc.want {
					got := target.Spec.MinAllowed[name]
					g.Expect(got.Cmp(q)).To(BeZero(), "%s of %s", name, target.Name)
				}
			}
			g.Expect(cr.Spec).To(Equal(before.Spec), "the fill works on a copy and leaves the CR untouched")
		})
	}
}

func TestReconcileCentralVPA_CreatesStatefulSetVPAs(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := testOVNCentral()
	cr.Spec.Northbound.VerticalAutoscaling = &commonv1.VerticalAutoscalingSpec{UpdateMode: "Off"}
	r := newTestOVNCentralReconciler(t, cr)
	r.vpaAvailable = true

	_, err := r.reconcileVPA(context.Background(), r.Client, cr)
	g.Expect(err).NotTo(HaveOccurred())

	var vpa vpav1.VerticalPodAutoscaler
	g.Expect(r.Get(context.Background(), types.NamespacedName{Name: cr.Name + "-nb", Namespace: cr.Namespace}, &vpa)).To(Succeed())
	g.Expect(vpa.Spec.TargetRef.Kind).To(Equal("StatefulSet"))
	g.Expect(vpa.Spec.ResourcePolicy.ContainerPolicies[0].MinAllowed).To(HaveLen(2))
	g.Expect(vpa.Labels).To(HaveKeyWithValue("app.kubernetes.io/name", centralAppName))
	cond := meta.FindStatusCondition(cr.Status.Conditions, "VPAReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Reason).To(Equal(deployment.ReasonVPAReady))
}

func TestReconcileCentralVPA_LatchFalse_SetsNotInstalled(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := testOVNCentral()
	cr.Spec.Southbound.VerticalAutoscaling = &commonv1.VerticalAutoscalingSpec{UpdateMode: "Off"}
	r := newTestOVNCentralReconciler(t, cr)

	_, err := r.reconcileVPA(context.Background(), r.Client, cr)
	g.Expect(err).NotTo(HaveOccurred())
	cond := meta.FindStatusCondition(cr.Status.Conditions, "VPAReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(deployment.ReasonVPANotInstalled))
	g.Expect(cond.Message).To(ContainSubstring(cr.Name + "-sb"))
}

// One block feeds both chassis DaemonSets.
func TestChassisVPATargets(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := testOVNChassis()
	cr.Spec.VerticalAutoscaling = &commonv1.VerticalAutoscalingSpec{UpdateMode: "Off"}

	g.Expect(chassisVPATargets(cr)).To(Equal([]deployment.VPATarget{
		{Kind: "DaemonSet", Name: cr.Name + "-ovs", Spec: cr.Spec.VerticalAutoscaling},
		{Kind: "DaemonSet", Name: cr.Name + "-ovn-controller", Spec: cr.Spec.VerticalAutoscaling},
	}))
}

func TestReconcileChassisVPA_OptOutRemovesBothVPAs(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := testOVNChassis()
	cr.Spec.VerticalAutoscaling = &commonv1.VerticalAutoscalingSpec{UpdateMode: "Off"}
	r := newTestOVNChassisReconciler(t, cr)
	r.vpaAvailable = true
	ctx := context.Background()

	_, err := r.reconcileVPA(ctx, r.Client, cr)
	g.Expect(err).NotTo(HaveOccurred())
	var list vpav1.VerticalPodAutoscalerList
	g.Expect(r.List(ctx, &list)).To(Succeed())
	g.Expect(list.Items).To(HaveLen(2))

	cr.Spec.VerticalAutoscaling = nil
	_, err = r.reconcileVPA(ctx, r.Client, cr)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(r.List(ctx, &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty())
	cond := meta.FindStatusCondition(cr.Status.Conditions, "VPAReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Reason).To(Equal(deployment.ReasonVPANotRequired))
}
