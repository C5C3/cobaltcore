// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
)

// poolsOfTwoNovas are two pools of validNova's Nova, one of them placed, and
// one pool of another Nova in the same namespace.
func poolsOfTwoNovas() (*novav1alpha1.NovaCompute, *novav1alpha1.NovaCompute, *novav1alpha1.NovaCompute) {
	a := validNovaCompute()
	b := validNovaCompute()
	b.Name = "pool-b"
	b.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "compute-a"}
	other := validNovaCompute()
	other.Name = "pool-other"
	other.Spec.NovaRef.Name = "nova-2"
	return a, b, other
}

func requestFor(name string) reconcile.Request {
	return reconcile.Request{NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: name}}
}

func TestNovaToNovaComputesMapper(t *testing.T) {
	g := NewGomegaWithT(t)
	a, b, other := poolsOfTwoNovas()
	c := novaFakeClientBuilder(a, b, other).Build()

	got := novaToNovaComputesMapper(c)(context.Background(), readyNovaForCompute())

	g.Expect(got).To(ConsistOf(requestFor(testPoolName), requestFor("pool-b")))
}

func TestNovaComputeToRivalsMapper(t *testing.T) {
	g := NewGomegaWithT(t)
	a, b, other := poolsOfTwoNovas()
	c := novaFakeClientBuilder(a, b, other).Build()

	g.Expect(novaComputeToRivalsMapper(c)(context.Background(), a)).To(ConsistOf(requestFor("pool-b")))
	g.Expect(novaComputeToRivalsMapper(c)(context.Background(), other)).To(BeEmpty())
	g.Expect(novaComputeToRivalsMapper(c)(context.Background(), &corev1.Node{})).To(BeEmpty())
}

func TestComputeConfigSecretToNovaComputesMapper(t *testing.T) {
	g := NewGomegaWithT(t)
	a, b, other := poolsOfTwoNovas()
	c := novaFakeClientBuilder(a, b, other).Build()
	mapper := computeConfigSecretToNovaComputesMapper(c)
	secret := func(name string) *corev1.Secret {
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace}}
	}

	g.Expect(mapper(context.Background(), secret(testContract))).To(ConsistOf(requestFor(testPoolName), requestFor("pool-b")))
	g.Expect(mapper(context.Background(), secret("nova-2-compute-config"))).To(ConsistOf(requestFor("pool-other")))
	g.Expect(mapper(context.Background(), secret(testServiceUserSecret))).To(BeEmpty())
	g.Expect(mapper(context.Background(), secret("-compute-config"))).To(BeEmpty())
}

func TestNodeToPlacedNovaComputesMapper(t *testing.T) {
	g := NewGomegaWithT(t)
	a, b, other := poolsOfTwoNovas()
	holding := validNovaCompute()
	holding.Name = "pool-c"
	holding.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "compute-a"}
	holding.Spec.NodeSelector = map[string]string{testPoolLabel: "c"}
	holding.Status.Nodes = []novav1alpha1.NovaComputeNodeStatus{entry("node-held", novav1alpha1.NovaComputeNodeActive)}
	c := novaFakeClientBuilder(a, b, other, holding).Build()
	mapper := nodeToPlacedNovaComputesMapper(c)

	g.Expect(mapper(context.Background(), selectedNode(testNodeName))).To(ConsistOf(requestFor("pool-b")),
		"a placed pool selecting the node; the local pool selecting it has no Node leg")
	g.Expect(mapper(context.Background(), poolNode("node-held", nil))).To(ConsistOf(requestFor("pool-c")),
		"the pool a node leaves")
	g.Expect(mapper(context.Background(), poolNode("unrelated", map[string]string{"role": "storage"}))).To(BeEmpty())
}

func TestNovaChangePredicate(t *testing.T) {
	updated := func(mutate func(n *novav1alpha1.Nova)) event.UpdateEvent {
		old := readyNovaForCompute()
		cur := old.DeepCopy()
		mutate(cur)
		return event.UpdateEvent{ObjectOld: old, ObjectNew: cur}
	}
	for _, tc := range []struct {
		name   string
		mutate func(n *novav1alpha1.Nova)
		want   bool
	}{
		{name: "a condition", mutate: func(n *novav1alpha1.Nova) {
			n.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue}}
		}, want: false},
		{name: "a spec change", mutate: func(n *novav1alpha1.Nova) { n.Generation++ }, want: true},
		{name: "the installed release", mutate: func(n *novav1alpha1.Nova) { n.Status.InstalledRelease = "2026.1" }, want: true},
		{name: "the published contract", mutate: func(n *novav1alpha1.Nova) { n.Status.ComputeConfigSecretRef = nil }, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			NewGomegaWithT(t).Expect(novaChangePredicate().Update(updated(tc.mutate))).To(Equal(tc.want))
		})
	}
}

func TestRivalChangePredicate(t *testing.T) {
	updated := func(mutate func(cr *novav1alpha1.NovaCompute)) event.UpdateEvent {
		old := validNovaCompute()
		old.Status.Nodes = []novav1alpha1.NovaComputeNodeStatus{entry(testNodeName, novav1alpha1.NovaComputeNodeDraining)}
		cur := old.DeepCopy()
		mutate(cur)
		return event.UpdateEvent{ObjectOld: old, ObjectNew: cur}
	}
	for _, tc := range []struct {
		name   string
		mutate func(cr *novav1alpha1.NovaCompute)
		want   bool
	}{
		{name: "an instance count", mutate: func(cr *novav1alpha1.NovaCompute) {
			cr.Status.Nodes[0].Instances = ptr.To(int32(3))
		}, want: false},
		{name: "the DaemonSet counters", mutate: func(cr *novav1alpha1.NovaCompute) { cr.Status.NumberReady = 1 }, want: false},
		{name: "a spec change", mutate: func(cr *novav1alpha1.NovaCompute) { cr.Generation++ }, want: true},
		{name: "the start of a deletion", mutate: func(cr *novav1alpha1.NovaCompute) {
			cr.DeletionTimestamp = ptr.To(metav1.Now())
		}, want: true},
		{name: "a phase", mutate: func(cr *novav1alpha1.NovaCompute) {
			cr.Status.Nodes[0].Phase = novav1alpha1.NovaComputeNodeReleasing
		}, want: true},
		{name: "a zone", mutate: func(cr *novav1alpha1.NovaCompute) { cr.Status.Nodes[0].Zone = "az2" }, want: true},
		{name: "a node taken", mutate: func(cr *novav1alpha1.NovaCompute) {
			cr.Status.Nodes = append(cr.Status.Nodes, entry("node-2", novav1alpha1.NovaComputeNodePending))
		}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			NewGomegaWithT(t).Expect(rivalChangePredicate().Update(updated(tc.mutate))).To(Equal(tc.want))
		})
	}

	t.Run("creations and deletions pass", func(t *testing.T) {
		g := NewGomegaWithT(t)
		obj := validNovaCompute()
		g.Expect(rivalChangePredicate().Create(event.CreateEvent{Object: obj})).To(BeTrue())
		g.Expect(rivalChangePredicate().Delete(event.DeleteEvent{Object: obj})).To(BeTrue())
	})
}

func TestNovaComputeNovaRefExtractor(t *testing.T) {
	g := NewGomegaWithT(t)
	g.Expect(novaComputeNovaRefExtractor(validNovaCompute())).To(Equal([]string{testNovaName}))
	g.Expect(novaComputeNovaRefExtractor(&corev1.Secret{})).To(BeNil())

	empty := validNovaCompute()
	empty.Spec.NovaRef.Name = ""
	g.Expect(novaComputeNovaRefExtractor(empty)).To(BeNil())
}
