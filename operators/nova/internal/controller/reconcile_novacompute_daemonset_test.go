// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/c5c3/cobaltcore/internal/common/testutil/simulators"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
)

var novaComputeDaemonSetKey = types.NamespacedName{Namespace: testNamespace, Name: testPoolName + "-nova-compute"}

// daemonSetPass is a pass whose earlier steps resolved the Nova and the config.
func daemonSetPass() *novaComputePass {
	return &novaComputePass{
		image:         commonv1.ImageSpec{Repository: novaComputeDefaultRepository, Tag: "2025.2"},
		secretName:    testContract,
		configMapName: testPoolName + "-config-abc",
		configHash:    "hash-1",
	}
}

// ownedDaemonSet is the pool's DaemonSet as a previous pass left it, with the
// given counters.
func ownedDaemonSet(t *testing.T, cr *novav1alpha1.NovaCompute, desired, ready int32) *appsv1.DaemonSet {
	t.Helper()
	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{
		Name: novaComputeDaemonSetKey.Name, Namespace: novaComputeDaemonSetKey.Namespace,
	}}
	if err := controllerutil.SetControllerReference(cr, ds, testScheme()); err != nil {
		t.Fatalf("setting the controller reference: %v", err)
	}
	ds.Status = appsv1.DaemonSetStatus{
		DesiredNumberScheduled: desired, CurrentNumberScheduled: desired,
		UpdatedNumberScheduled: ready, NumberReady: ready,
	}
	return ds
}

func deletingNovaCompute() *novav1alpha1.NovaCompute {
	cr := validNovaCompute()
	cr.DeletionTimestamp = ptr.To(metav1.Now())
	cr.Finalizers = []string{novaComputeDrainFinalizer}
	return cr
}

func TestNovaComputeAffinity(t *testing.T) {
	inTerm := func(node string) corev1.NodeSelectorTerm {
		return corev1.NodeSelectorTerm{MatchFields: []corev1.NodeSelectorRequirement{{
			Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{node},
		}}}
	}

	t.Run("the selector term carries every label and one NotIn per excluded node", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cr := validNovaCompute()
		cr.Spec.NodeSelector = map[string]string{"z": "1", testPoolLabel: "a"}

		affinity := novaComputeAffinity(cr, []string{"node-c", "node-a"}, nil)

		terms := affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
		g.Expect(terms).To(Equal([]corev1.NodeSelectorTerm{{
			MatchExpressions: []corev1.NodeSelectorRequirement{
				{Key: testPoolLabel, Operator: corev1.NodeSelectorOpIn, Values: []string{"a"}},
				{Key: "z", Operator: corev1.NodeSelectorOpIn, Values: []string{"1"}},
			},
			MatchFields: []corev1.NodeSelectorRequirement{
				{Key: "metadata.name", Operator: corev1.NodeSelectorOpNotIn, Values: []string{"node-a"}},
				{Key: "metadata.name", Operator: corev1.NodeSelectorOpNotIn, Values: []string{"node-c"}},
			},
		}}))
	})

	t.Run("each held node is a term of its own", func(t *testing.T) {
		g := NewGomegaWithT(t)
		affinity := novaComputeAffinity(validNovaCompute(), nil, []string{"node-b", "node-a"})

		terms := affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
		g.Expect(terms).To(HaveLen(3))
		g.Expect(terms[1:]).To(Equal([]corev1.NodeSelectorTerm{inTerm("node-a"), inTerm("node-b")}))
	})

	t.Run("a deleting pool has no selector term", func(t *testing.T) {
		g := NewGomegaWithT(t)
		affinity := novaComputeAffinity(deletingNovaCompute(), []string{"node-x"}, []string{"node-a"})

		g.Expect(affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms).
			To(Equal([]corev1.NodeSelectorTerm{inTerm("node-a")}))
	})

	t.Run("no term is nil", func(t *testing.T) {
		g := NewGomegaWithT(t)
		g.Expect(novaComputeAffinity(deletingNovaCompute(), nil, nil)).To(BeNil())
	})
}

func TestReconcileNovaComputeDaemonSet_ProgressingMirrorsTheCounters(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := validNovaCompute()
	r := newNovaComputeTestReconciler(nil, cr, ownedDaemonSet(t, cr, 3, 2))

	result, err := r.reconcileNovaComputeDaemonSet(context.Background(), r.Client, cr, daemonSetPass())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue(), "a rollout is not a wait: the aggregates and the drain still run")
	cond := novaComputeCondition(cr, conditionTypeDaemonSetReady)
	g.Expect(cond.Reason).To(Equal(conditionReasonDaemonSetProgressing))
	g.Expect(cond.Message).To(ContainSubstring("2 of 3 nodes"))
	g.Expect(cr.Status.DesiredNumberScheduled).To(BeEquivalentTo(3))
	g.Expect(cr.Status.InstalledImage).To(BeEmpty())
}

func TestReconcileNovaComputeDaemonSet_ReadyRecordsTheImage(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := validNovaCompute()
	r := newNovaComputeTestReconciler(nil, cr, ownedDaemonSet(t, cr, 3, 2))
	_, err := r.reconcileNovaComputeDaemonSet(ctx, r.Client, cr, daemonSetPass())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(simulators.MarkDaemonSetReady(ctx, r.Client, novaComputeDaemonSetKey)).To(Succeed())

	result, err := r.reconcileNovaComputeDaemonSet(ctx, r.Client, cr, daemonSetPass())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue())
	g.Expect(novaComputeCondition(cr, conditionTypeDaemonSetReady).Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cr.Status.InstalledImage).To(Equal("ghcr.io/c5c3/nova-compute:2025.2"))
}

// TestReconcileNovaComputeDaemonSet_ZeroTermsDeletesTheDaemonSet pins the
// teardown path: a deleting pool that holds no draining node applies nothing,
// and deletes the DaemonSet it owns, which is what releases the pod of its last
// Releasing node.
func TestReconcileNovaComputeDaemonSet_ZeroTermsDeletesTheDaemonSet(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := deletingNovaCompute()
	r := newNovaComputeTestReconciler(nil, cr, ownedDaemonSet(t, cr, 1, 1))

	result, err := r.reconcileNovaComputeDaemonSet(ctx, r.Client, cr, daemonSetPass())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue())
	g.Expect(apierrors.IsNotFound(r.Get(ctx, novaComputeDaemonSetKey, &appsv1.DaemonSet{}))).To(BeTrue())
	cond := novaComputeCondition(cr, conditionTypeDaemonSetReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Message).To(ContainSubstring("removed"))
	g.Expect(cr.Status.DesiredNumberScheduled).To(BeZero())
}

func TestReconcileNovaComputeDaemonSet_ZeroTermsLeavesAForeignDaemonSet(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := deletingNovaCompute()
	foreign := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{
		Name: novaComputeDaemonSetKey.Name, Namespace: novaComputeDaemonSetKey.Namespace,
	}}
	r := newNovaComputeTestReconciler(nil, cr, foreign)

	_, err := r.reconcileNovaComputeDaemonSet(ctx, r.Client, cr, daemonSetPass())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(r.Get(ctx, novaComputeDaemonSetKey, &appsv1.DaemonSet{})).To(Succeed())
}

// TestReconcileNovaComputeDaemonSet_RotatedContractRollsThePods runs the
// PoolConfig and DaemonSet steps against two passwords: the hash annotation
// on the pod template follows the contract.
func TestReconcileNovaComputeDaemonSet_RotatedContractRollsThePods(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	annotationFor := func(password string) string {
		cr := validNovaCompute()
		r := newNovaComputeTestReconciler(nil, cr, computeContractSecret(password))
		pass := daemonSetPass()
		_, err := r.reconcileNovaComputeConfig(ctx, r.Client, cr, pass)
		g.Expect(err).NotTo(HaveOccurred())
		_, err = r.reconcileNovaComputeDaemonSet(ctx, r.Client, cr, pass)
		g.Expect(err).NotTo(HaveOccurred())
		ds := &appsv1.DaemonSet{}
		g.Expect(r.Get(ctx, novaComputeDaemonSetKey, ds)).To(Succeed())
		return ds.Spec.Template.Annotations[novaComputeConfigHashAnnotation]
	}

	first := annotationFor("pw-1")
	g.Expect(first).NotTo(BeEmpty())
	g.Expect(annotationFor("pw-1")).To(Equal(first))
	g.Expect(annotationFor("pw-2")).NotTo(Equal(first))
}

func TestNovaComputeUpdateStrategy(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := validNovaCompute()
	g.Expect(novaComputeUpdateStrategy(cr).RollingUpdate.MaxUnavailable.IntValue()).To(Equal(1))

	cr.Spec.UpdateStrategy.Type = "OnDelete"
	g.Expect(novaComputeUpdateStrategy(cr)).To(Equal(&appsv1.DaemonSetUpdateStrategy{Type: appsv1.OnDeleteDaemonSetStrategyType}))
}
