// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
)

// rivalPool returns another pool of validNova's Nova, created offset from the
// fixture pool, selecting label=value.
func rivalPool(name string, offset time.Duration, label, value string) *novav1alpha1.NovaCompute {
	rival := validNovaCompute()
	rival.Name = name
	rival.UID = ""
	rival.CreationTimestamp = metav1.NewTime(testPoolCreated.Add(offset))
	rival.Spec.NodeSelector = map[string]string{label: value}
	return rival
}

// entry is a status.nodes entry with a phase.
func entry(name string, phase novav1alpha1.NovaComputeNodePhase) novav1alpha1.NovaComputeNodeStatus {
	return novav1alpha1.NovaComputeNodeStatus{Name: name, Phase: phase, Zone: testZone}
}

// runNodes runs the Nodes step against a fake client holding objs.
func runNodes(t *testing.T, cr *novav1alpha1.NovaCompute, objs ...client.Object) (*novaComputePass, *record.FakeRecorder) {
	t.Helper()
	g := NewGomegaWithT(t)
	r := newNovaComputeTestReconciler(nil, append(objs, cr)...)
	pass := &novaComputePass{}
	result, err := r.reconcileNovaComputeNodes(context.Background(), r.Client, cr, pass)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue(), "the Nodes step never waits on a node")
	return pass, r.Recorder.(*record.FakeRecorder)
}

// isNodeList reports whether list is the Node list the Nodes step sends, which
// asks for the nodes' metadata alone.
func isNodeList(list client.ObjectList) bool {
	partial, ok := list.(*metav1.PartialObjectMetadataList)
	return ok && partial.GroupVersionKind() == corev1.SchemeGroupVersion.WithKind("NodeList")
}

func phaseOf(cr *novav1alpha1.NovaCompute, node string) novav1alpha1.NovaComputeNodePhase {
	for _, e := range cr.Status.Nodes {
		if e.Name == node {
			return e.Phase
		}
	}
	return ""
}

func TestReconcileNovaComputeNodes_NoMatchingNodes(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := validNovaCompute()
	runNodes(t, cr, poolNode("other", map[string]string{testPoolLabel: "b"}))

	g.Expect(cr.Status.Nodes).To(BeEmpty())
	cond := novaComputeCondition(cr, conditionTypeNodesReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonNoMatchingNodes))
}

func TestReconcileNovaComputeNodes_TakesASelectedNode(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := validNovaCompute()
	runNodes(t, cr, selectedNode(testNodeName))

	g.Expect(cr.Status.Nodes).To(Equal([]novav1alpha1.NovaComputeNodeStatus{
		{Name: testNodeName, Phase: novav1alpha1.NovaComputeNodePending, Zone: testZone},
	}))
	cond := novaComputeCondition(cr, conditionTypeNodesReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonNodesResolved))
}

func TestReconcileNovaComputeNodes_ForbiddenListWaits(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := validNovaCompute()
	c := novaFakeClientBuilder(cr).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if isNodeList(list) {
				return apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, "", nil)
			}
			return cl.List(ctx, list, opts...)
		},
	}).Build()
	r := &NovaComputeReconciler{Client: c, Scheme: c.Scheme(), Recorder: record.NewFakeRecorder(10)}

	result, err := r.reconcileNovaComputeNodes(context.Background(), c, cr, &novaComputePass{})

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))
	g.Expect(novaComputeCondition(cr, conditionTypeNodesReady).Reason).To(Equal(conditionReasonNodesForbidden))
}

func TestReconcileNovaComputeNodes_OtherListErrorFails(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := validNovaCompute()
	c := novaFakeClientBuilder(cr).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if isNodeList(list) {
				return apierrors.NewServiceUnavailable("etcd is down")
			}
			return cl.List(ctx, list, opts...)
		},
	}).Build()
	r := &NovaComputeReconciler{Client: c, Scheme: c.Scheme(), Recorder: record.NewFakeRecorder(10)}

	_, err := r.reconcileNovaComputeNodes(context.Background(), c, cr, &novaComputePass{})

	g.Expect(err).To(HaveOccurred())
	g.Expect(novaComputeCondition(cr, conditionTypeNodesReady).Reason).To(Equal(conditionReasonNodeListError))
}

// TestReconcileNovaComputeNodes_OlderPoolTakesASharedNode pins the tie-break
// between two pools of one Nova selecting the same node with neither holding
// it: the older takes it, the younger reports the conflict once.
func TestReconcileNovaComputeNodes_OlderPoolTakesASharedNode(t *testing.T) {
	g := NewGomegaWithT(t)
	node := poolNode(testNodeName, map[string]string{testPoolLabel: "a", "shared": "yes", zoneLabel: testZone})
	older := rivalPool("pool-older", -time.Hour, "shared", "yes")
	cr := validNovaCompute()

	pass, rec := runNodes(t, cr, node, older)

	g.Expect(cr.Status.Nodes).To(Equal([]novav1alpha1.NovaComputeNodeStatus{{
		Name: testNodeName, Phase: novav1alpha1.NovaComputeNodeConflict, Zone: testZone, ConflictsWith: "pool-older",
	}}))
	g.Expect(pass.excluded).To(Equal([]string{testNodeName}))
	cond := novaComputeCondition(cr, conditionTypeNodesReady)
	g.Expect(cond.Reason).To(Equal(conditionReasonNodeConflict))
	g.Expect(cond.Message).To(ContainSubstring("node-1 (held by pool-older)"))
	events := collectEvents(rec)
	g.Expect(events).To(ConsistOf(ContainSubstring("Warning NodeConflict Node node-1 is held by NovaCompute pool-older")))

	// The older pool, on its own pass, takes the node.
	olderPass, _ := runNodes(t, older, node, cr)
	g.Expect(phaseOf(older, testNodeName)).To(Equal(novav1alpha1.NovaComputeNodePending))
	g.Expect(olderPass.excluded).To(BeEmpty())

	// A second pass of the younger records no second event.
	_, rec = runNodes(t, cr, node, older)
	g.Expect(collectEvents(rec)).To(BeEmpty())
}

// TestReconcileNovaComputeNodes_SameTimestampFallsBackToTheName pins the
// tie-break of two pools created in the same second, which `kubectl apply` of
// a directory routinely does: the name decides, so exactly one takes the node.
func TestReconcileNovaComputeNodes_SameTimestampFallsBackToTheName(t *testing.T) {
	g := NewGomegaWithT(t)
	node := poolNode(testNodeName, map[string]string{testPoolLabel: "a", "shared": "yes", zoneLabel: testZone})
	twin := rivalPool("pool-0", 0, "shared", "yes")
	cr := validNovaCompute()

	runNodes(t, cr, node, twin)
	g.Expect(cr.Status.Nodes).To(Equal([]novav1alpha1.NovaComputeNodeStatus{{
		Name: testNodeName, Phase: novav1alpha1.NovaComputeNodeConflict, Zone: testZone, ConflictsWith: "pool-0",
	}}))

	twinPass, _ := runNodes(t, twin, node, cr)
	g.Expect(phaseOf(twin, testNodeName)).To(Equal(novav1alpha1.NovaComputeNodePending))
	g.Expect(twinPass.excluded).To(BeEmpty())
}

// TestReconcileNovaComputeNodes_DoubleHoldYieldsToTheOlder pins how two pools
// that both hold a node resolve it. A pass that read the other pool's status
// before its write landed leaves that behind, and two nova-compute processes
// would then share one libvirt: the younger yields, the older keeps it.
func TestReconcileNovaComputeNodes_DoubleHoldYieldsToTheOlder(t *testing.T) {
	node := poolNode(testNodeName, map[string]string{testPoolLabel: "a", "shared": "yes", zoneLabel: testZone})
	holding := func(phase novav1alpha1.NovaComputeNodePhase) []novav1alpha1.NovaComputeNodeStatus {
		e := entry(testNodeName, phase)
		e.ServiceID = "svc"
		return []novav1alpha1.NovaComputeNodeStatus{e}
	}

	t.Run("the younger yields", func(t *testing.T) {
		g := NewGomegaWithT(t)
		older := rivalPool("pool-older", -time.Hour, "shared", "yes")
		older.Status.Nodes = holding(novav1alpha1.NovaComputeNodeActive)
		cr := validNovaCompute()
		cr.Status.Nodes = holding(novav1alpha1.NovaComputeNodeActive)

		pass, rec := runNodes(t, cr, node, older)

		g.Expect(cr.Status.Nodes).To(Equal([]novav1alpha1.NovaComputeNodeStatus{{
			Name: testNodeName, Phase: novav1alpha1.NovaComputeNodeConflict, Zone: testZone, ConflictsWith: "pool-older",
		}}), "the service is left to the older pool")
		g.Expect(pass.excluded).To(Equal([]string{testNodeName}))
		g.Expect(collectEvents(rec)).To(ConsistOf(ContainSubstring("Warning NodeConflict Node node-1 is held by NovaCompute pool-older")))

		olderPass, _ := runNodes(t, older, node, cr)
		g.Expect(phaseOf(older, testNodeName)).To(Equal(novav1alpha1.NovaComputeNodeActive))
		g.Expect(olderPass.excluded).To(BeEmpty())
	})

	for _, tc := range []struct {
		name  string
		rival func() *novav1alpha1.NovaCompute
	}{
		{name: "not to a younger pool", rival: func() *novav1alpha1.NovaCompute {
			rival := rivalPool("pool-younger", time.Hour, "shared", "yes")
			rival.Status.Nodes = holding(novav1alpha1.NovaComputeNodeActive)
			return rival
		}},
		{name: "not to a pool on its way out of the node", rival: func() *novav1alpha1.NovaCompute {
			rival := rivalPool("pool-older", -time.Hour, "other", "yes")
			rival.Status.Nodes = holding(novav1alpha1.NovaComputeNodeDraining)
			return rival
		}},
		{name: "not to a pool being deleted", rival: func() *novav1alpha1.NovaCompute {
			rival := rivalPool("pool-older", -time.Hour, "shared", "yes")
			rival.Status.Nodes = holding(novav1alpha1.NovaComputeNodeActive)
			rival.DeletionTimestamp = ptr.To(metav1.Now())
			rival.Finalizers = []string{novaComputeDrainFinalizer}
			return rival
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cr := validNovaCompute()
			cr.Status.Nodes = holding(novav1alpha1.NovaComputeNodeActive)

			runNodes(t, cr, node, tc.rival())

			g.Expect(phaseOf(cr, testNodeName)).To(Equal(novav1alpha1.NovaComputeNodeActive))
		})
	}
}

// TestReconcileNovaComputeNodes_HolderKeepsItsNode pins that holding beats age:
// a pool recreated with an older timestamp does not take a node the fixture
// pool holds.
func TestReconcileNovaComputeNodes_HolderKeepsItsNode(t *testing.T) {
	g := NewGomegaWithT(t)
	node := poolNode(testNodeName, map[string]string{testPoolLabel: "a", "shared": "yes", zoneLabel: testZone})
	cr := validNovaCompute()
	cr.Status.Nodes = []novav1alpha1.NovaComputeNodeStatus{entry(testNodeName, novav1alpha1.NovaComputeNodeActive)}
	cr.Status.Nodes[0].ServiceID = "svc"
	recreated := rivalPool("pool-recreated", -24*time.Hour, "shared", "yes")

	runNodes(t, cr, node, recreated)
	g.Expect(phaseOf(cr, testNodeName)).To(Equal(novav1alpha1.NovaComputeNodeActive))

	runNodes(t, recreated, node, cr)
	g.Expect(recreated.Status.Nodes).To(ConsistOf(HaveField("ConflictsWith", testPoolName)))
	g.Expect(phaseOf(recreated, testNodeName)).To(Equal(novav1alpha1.NovaComputeNodeConflict))
}

// TestReconcileNovaComputeNodes_OnlyPoolsOfOneNovaOnOneClusterConflict pins the
// rival set: another Nova's pool, or a pool on another cluster, selecting the
// same node name is no rival.
func TestReconcileNovaComputeNodes_OnlyPoolsOfOneNovaOnOneClusterConflict(t *testing.T) {
	node := poolNode(testNodeName, map[string]string{testPoolLabel: "a", "shared": "yes", zoneLabel: testZone})

	otherNova := rivalPool("pool-other-nova", -time.Hour, "shared", "yes")
	otherNova.Spec.NovaRef.Name = "nova-2"
	otherNova.Status.Nodes = []novav1alpha1.NovaComputeNodeStatus{entry(testNodeName, novav1alpha1.NovaComputeNodeActive)}

	otherCluster := rivalPool("pool-other-cluster", -time.Hour, "shared", "yes")
	otherCluster.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "compute-a"}
	otherCluster.Status.Nodes = []novav1alpha1.NovaComputeNodeStatus{entry(testNodeName, novav1alpha1.NovaComputeNodeActive)}

	for _, rival := range []*novav1alpha1.NovaCompute{otherNova, otherCluster} {
		t.Run(rival.Name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cr := validNovaCompute()
			runNodes(t, cr, node, rival)
			g.Expect(phaseOf(cr, testNodeName)).To(Equal(novav1alpha1.NovaComputeNodePending))
		})
	}
}

// TestReconcileNovaComputeNodes_LeavingNodes pins rule 2: a held node that is
// no longer selected drains, unless a pool that is not being deleted selects
// it, which takes it over without a drain.
func TestReconcileNovaComputeNodes_LeavingNodes(t *testing.T) {
	relabelled := poolNode(testNodeName, map[string]string{testPoolLabel: "b", zoneLabel: testZone})

	t.Run("drains", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cr := validNovaCompute()
		cr.Status.Nodes = []novav1alpha1.NovaComputeNodeStatus{entry(testNodeName, novav1alpha1.NovaComputeNodeActive)}

		pass, _ := runNodes(t, cr, relabelled)

		g.Expect(phaseOf(cr, testNodeName)).To(Equal(novav1alpha1.NovaComputeNodeDraining))
		g.Expect(pass.keepPods).To(Equal([]string{testNodeName}))
		g.Expect(pass.handover).To(BeEmpty())
		cond := novaComputeCondition(cr, conditionTypeNodesReady)
		g.Expect(cond.Reason).To(Equal(conditionReasonNodesResolved))
		g.Expect(cond.Message).To(Equal("0 nodes selected, 1 leaving"))
	})

	t.Run("is handed over to a pool selecting it", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cr := validNovaCompute()
		cr.Status.Nodes = []novav1alpha1.NovaComputeNodeStatus{entry(testNodeName, novav1alpha1.NovaComputeNodeDraining)}
		taker := rivalPool("pool-b", time.Hour, testPoolLabel, "b")

		pass, _ := runNodes(t, cr, relabelled, taker)

		g.Expect(phaseOf(cr, testNodeName)).To(Equal(novav1alpha1.NovaComputeNodeReleasing))
		g.Expect(pass.handover).To(HaveKey(testNodeName))
		g.Expect(pass.keepPods).To(BeEmpty())
	})

	t.Run("is not handed over to a deleting pool", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cr := validNovaCompute()
		cr.Status.Nodes = []novav1alpha1.NovaComputeNodeStatus{entry(testNodeName, novav1alpha1.NovaComputeNodeActive)}
		taker := rivalPool("pool-b", time.Hour, testPoolLabel, "b")
		taker.DeletionTimestamp = ptr.To(metav1.Now())
		taker.Finalizers = []string{novaComputeDrainFinalizer}

		runNodes(t, cr, relabelled, taker)

		g.Expect(phaseOf(cr, testNodeName)).To(Equal(novav1alpha1.NovaComputeNodeDraining))
	})

	t.Run("a gone node drains and keeps its zone", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cr := validNovaCompute()
		cr.Status.Nodes = []novav1alpha1.NovaComputeNodeStatus{entry(testNodeName, novav1alpha1.NovaComputeNodeActive)}

		runNodes(t, cr)

		g.Expect(cr.Status.Nodes).To(ConsistOf(HaveField("Zone", testZone)))
		g.Expect(phaseOf(cr, testNodeName)).To(Equal(novav1alpha1.NovaComputeNodeDraining))
	})

	t.Run("a releasing node stays releasing", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cr := validNovaCompute()
		cr.Status.Nodes = []novav1alpha1.NovaComputeNodeStatus{entry(testNodeName, novav1alpha1.NovaComputeNodeReleasing)}

		pass, _ := runNodes(t, cr, relabelled)

		g.Expect(phaseOf(cr, testNodeName)).To(Equal(novav1alpha1.NovaComputeNodeReleasing))
		g.Expect(pass.keepPods).To(BeEmpty(), "a Releasing node's pod is released")
	})

	t.Run("a conflict that is no longer selected is dropped", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cr := validNovaCompute()
		cr.Status.Nodes = []novav1alpha1.NovaComputeNodeStatus{entry(testNodeName, novav1alpha1.NovaComputeNodeConflict)}

		runNodes(t, cr, relabelled)

		g.Expect(cr.Status.Nodes).To(BeEmpty())
	})
}

// TestReconcileNovaComputeNodes_ReselectedNodeComesBack pins that a node
// relabelled back into the pool mid-drain is Active again, with its service
// left as it is and its pod back under the selector term.
func TestReconcileNovaComputeNodes_ReselectedNodeComesBack(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := validNovaCompute()
	draining := entry(testNodeName, novav1alpha1.NovaComputeNodeDraining)
	draining.ServiceID = "svc"
	draining.ServiceStatus = "disabled"
	draining.Instances = ptr.To(int32(2))
	cr.Status.Nodes = []novav1alpha1.NovaComputeNodeStatus{draining}

	pass, _ := runNodes(t, cr, selectedNode(testNodeName))

	g.Expect(cr.Status.Nodes).To(ConsistOf(novav1alpha1.NovaComputeNodeStatus{
		Name: testNodeName, Phase: novav1alpha1.NovaComputeNodeActive, Zone: testZone,
		ServiceID: "svc", ServiceStatus: "disabled",
	}))
	g.Expect(pass.keepPods).To(BeEmpty())
}

// TestReconcileNovaComputeNodes_DeletingPoolListsNoNodes pins that a deleting
// pool selects nothing: every held node leaves, and the Node list is not even
// sent, which is what lets a namespace-scoped install delete a local pool.
func TestReconcileNovaComputeNodes_DeletingPoolListsNoNodes(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := validNovaCompute()
	cr.DeletionTimestamp = ptr.To(metav1.Now())
	cr.Finalizers = []string{novaComputeDrainFinalizer}
	cr.Status.Nodes = []novav1alpha1.NovaComputeNodeStatus{entry(testNodeName, novav1alpha1.NovaComputeNodeActive)}

	nodeLists := 0
	c := novaFakeClientBuilder(cr, selectedNode(testNodeName)).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if isNodeList(list) {
				nodeLists++
			}
			return cl.List(ctx, list, opts...)
		},
	}).Build()
	r := &NovaComputeReconciler{Client: c, Scheme: c.Scheme(), Recorder: record.NewFakeRecorder(10)}
	pass := &novaComputePass{}

	_, err := r.reconcileNovaComputeNodes(context.Background(), c, cr, pass)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(nodeLists).To(BeZero())
	g.Expect(phaseOf(cr, testNodeName)).To(Equal(novav1alpha1.NovaComputeNodeDraining))
	g.Expect(pass.keepPods).To(Equal([]string{testNodeName}))
}
