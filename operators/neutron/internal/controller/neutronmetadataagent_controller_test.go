// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Unit tests for the NeutronMetadataAgent reconcile entry point, the
// remote-children teardown path, and the field-index plumbing.
package controller

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	mctestutil "github.com/c5c3/cobaltcore/internal/common/testutil/multicluster"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
)

// A pass that observes a CR after it left etcd must not fail the workqueue.
// Unlike the Neutron path it drops no metric series: the operator's per-CR
// collectors count db-sync runs, which only a Neutron owns.
func TestReconcileAgent_NotFoundIsIgnored(t *testing.T) {
	g := NewGomegaWithT(t)
	r := newAgentTestReconciler() // no NeutronMetadataAgent seeded

	res, err := r.Reconcile(context.Background(), agentRequest)

	g.Expect(err).NotTo(HaveOccurred(), "a deleted CR is not an error")
	g.Expect(res.IsZero()).To(BeTrue())
}

// An unresolvable spec.targetClusterRef reports ChassisReady=False with the
// shared reason, requeues instead of erroring, and leaves the CR with neither a
// finalizer nor a single child object, because the resolution runs ahead of the
// finalizer add.
func TestReconcileAgent_TargetClusterUnavailableGatesBeforeFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := validAgent()
	cr.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "nowhere"}
	r := newAgentTestReconciler(cr,
		readyOVNChassis(testOVNChassisName, testNamespace, testOVNCentralName), agentCentral())
	r.Resolver = unresolvableResolver{}

	ctx := context.Background()
	res, err := r.Reconcile(ctx, agentRequest)

	g.Expect(err).NotTo(HaveOccurred(),
		"an unregistered target cluster is a wait, not a reconcile failure")
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))

	got := getAgent(t, r.Client)
	cond := agentCondition(got, conditionTypeChassisReady)
	g.Expect(cond).NotTo(BeNil(), "the first gate condition must carry the failure")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(commonmulticluster.TargetClusterUnavailable))
	g.Expect(cond.Message).To(ContainSubstring("cluster not found"))

	g.Expect(got.Finalizers).To(BeEmpty(),
		"a CR whose target never resolves must not be pinned by a finalizer")

	var daemonSets appsv1.DaemonSetList
	g.Expect(r.List(ctx, &daemonSets)).To(Succeed())
	g.Expect(daemonSets.Items).To(BeEmpty())
	var configMaps corev1.ConfigMapList
	g.Expect(r.List(ctx, &configMaps)).To(Succeed())
	g.Expect(configMaps.Items).To(BeEmpty())
}

// A CR naming a target cluster is pinned before anything is projected onto it,
// so a deletion issued between this pass and the next still funnels through the
// sweep.
func TestReconcileAgent_InstallsRemoteChildrenFinalizerForATargetCluster(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := validAgent()
	cr.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "target"}
	r := newAgentTestReconciler(cr)
	r.Resolver = mctestutil.ResolverFor(mctestutil.TargetCluster{
		Client: neutronFakeClientBuilder().Build(),
	})

	res, err := r.Reconcile(context.Background(), agentRequest)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{RequeueAfter: commonreconcile.RequeueNextPass}),
		"the pass installing the finalizer must requeue before any sub-reconciler runs")
	g.Expect(getAgent(t, r.Client).Finalizers).
		To(ContainElement(commonmulticluster.RemoteChildrenFinalizer))
}

// A CR that keeps its children on the management cluster has nothing for the
// sweep to do, and a finalizer it does not need is one more thing that can block
// its deletion.
func TestReconcileAgent_LocalCRNeverCarriesTheRemoteChildrenFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)
	r := newAgentTestReconciler(validAgent()) // no spec.targetClusterRef

	_, err := r.Reconcile(context.Background(), agentRequest)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getAgent(t, r.Client).Finalizers).To(BeEmpty(),
		"a local CR must stay free of the remote-children finalizer")
}

// Deregistering a target cluster is a documented operation, and a CR that
// already projected onto it carries the finalizer by then; every pass
// short-circuiting on "cluster not found" ahead of the sweep would leave it
// Terminating until someone stripped the finalizer by hand.
func TestReconcileAgent_TerminatingCR_UnresolvableTargetReleasesFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)
	// The window this process has to sit out starts on its own first failure to
	// resolve, so it has to be compressed for the second pass to reach past it.
	abandonAfter := commonmulticluster.AbandonAfter
	t.Cleanup(func() { commonmulticluster.AbandonAfter = abandonAfter })
	commonmulticluster.AbandonAfter = time.Millisecond

	cr := validAgent()
	// The name is this test's own: the abandon window is tracked per cluster name
	// in a process-global map, and a sibling test compressing it too would
	// otherwise have started this one's window already.
	cr.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "deregistered-agent"}
	cr.Finalizers = []string{commonmulticluster.RemoteChildrenFinalizer}
	// Terminating for far longer than the window, which by itself must not be
	// enough: a CR blocked in cleanup for minutes is ordinary, and giving up on it
	// the moment the operator comes back would strand its children.
	deletedAt := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	cr.DeletionTimestamp = &deletedAt
	r := newAgentTestReconciler(cr)
	r.Resolver = unresolvableResolver{}

	ctx := context.Background()
	res, err := r.Reconcile(ctx, agentRequest)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling),
		"the first pass this process fails to resolve on starts the window, it does not end it")
	g.Expect(getAgent(t, r.Client).Finalizers).
		To(ContainElement(commonmulticluster.RemoteChildrenFinalizer))
	time.Sleep(10 * commonmulticluster.AbandonAfter)

	res, err = r.Reconcile(ctx, agentRequest)

	g.Expect(err).NotTo(HaveOccurred(),
		"a target cluster that is gone must not fail the deletion pass")
	g.Expect(res.IsZero()).To(BeTrue(), "the deletion resolves once the window is out")

	// With the finalizer released, the fake client garbage-collects the CR.
	var gone neutronv1alpha1.NeutronMetadataAgent
	err = r.Get(ctx, agentRequest.NamespacedName, &gone)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
		"the finalizer must be released even without a reachable target cluster")

	// The abandoned children are announced rather than silently dropped.
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).
		To(ContainElement(ContainSubstring("RemoteChildrenAbandoned")))
}

// Cluster engagement is asynchronous, so a registered cluster does not resolve
// either while the provider is still syncing after an operator restart. A CR
// deleted in that window must requeue rather than release its finalizer:
// abandoning here would leave the DaemonSet running on a cluster that is
// perfectly reachable, with no CR left to retry from.
func TestReconcileAgent_TerminatingCR_TargetNotEngagedYetKeepsFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := validAgent()
	cr.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "not-engaged-yet-agent"}
	cr.Finalizers = []string{commonmulticluster.RemoteChildrenFinalizer}
	r := newAgentTestReconciler(cr)
	r.Resolver = unresolvableResolver{}

	ctx := context.Background()
	// Deleted just now, so the whole abandon window is still ahead.
	g.Expect(r.Delete(ctx, cr)).To(Succeed())

	res, err := r.Reconcile(ctx, agentRequest)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))

	got := getAgent(t, r.Client)
	g.Expect(got.Finalizers).To(ContainElement(commonmulticluster.RemoteChildrenFinalizer),
		"a target that may still be engaging must not cost the CR its finalizer")
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).
		NotTo(ContainElement(ContainSubstring("RemoteChildrenAbandoned")))

	// The hold lasts minutes, so it has to be readable off the CR: without it, a
	// namespace stuck Terminating looks like a wedged finalizer and can only be
	// told apart by correlating operator logs across replicas.
	cond := agentCondition(got, conditionTypeChassisReady)
	g.Expect(cond).NotTo(BeNil(), "the deliberate hold must be visible on the CR")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(commonmulticluster.TargetClusterUnavailable))
	g.Expect(cond.Message).To(ContainSubstring("not-engaged-yet-agent"))
}

// The whole pipeline of a local CR whose chassis and central have resolved: the
// config ConfigMap and the DaemonSet are projected, and the aggregate turns True
// because a DaemonSet that selects no node has nothing left to roll out.
func TestReconcileAgent_FullPassReachesReady(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := validAgent()
	cr.Generation = 4
	r := newAgentTestReconciler(cr,
		readyOVNChassis(testOVNChassisName, testNamespace, testOVNCentralName), agentCentral())

	res, err := r.Reconcile(ctx, agentRequest)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue(), "a converged agent is not polled")

	got := getAgent(t, r.Client)
	g.Expect(agentCondition(got, conditionTypeChassisReady).Status).To(Equal(metav1.ConditionTrue))
	g.Expect(agentCondition(got, "SecretsReady").Status).To(Equal(metav1.ConditionTrue))
	g.Expect(agentCondition(got, conditionTypeDaemonSetReady).Status).To(Equal(metav1.ConditionTrue))
	g.Expect(agentCondition(got, "Ready").Status).To(Equal(metav1.ConditionTrue))
	g.Expect(got.Status.ObservedGeneration).To(Equal(int64(4)),
		"a status write stamps the generation it was decided from")
	g.Expect(got.Status.InstalledImage).To(Equal(cr.Spec.Image.Reference()))

	var ds appsv1.DaemonSet
	g.Expect(r.Get(ctx, agentDaemonSetKey, &ds)).To(Succeed())
	g.Expect(ds.Spec.Template.Spec.NodeSelector).To(HaveKeyWithValue(testChassisNodeLabel, "true"))

	// The mounted ConfigMap is the one the config step rendered on this pass.
	var configMaps corev1.ConfigMapList
	g.Expect(r.List(ctx, &configMaps, client.InNamespace(testNamespace))).To(Succeed())
	g.Expect(configMaps.Items).To(HaveLen(1))
	var mounted string
	for _, volume := range ds.Spec.Template.Spec.Volumes {
		if volume.Name == configVolumeName {
			mounted = volume.ConfigMap.Name
		}
	}
	g.Expect(mounted).To(Equal(configMaps.Items[0].Name))
}

// The first gate stops the pass at the entry point rather than at the step list.
// An agent applied before its chassis has no node to run on and no Secret to
// mount, so nothing may be projected.
func TestReconcileAgent_ChassisNotFoundGatesThePipeline(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	r := newAgentTestReconciler(validAgent()) // no OVNChassis seeded

	res, err := r.Reconcile(ctx, agentRequest)

	g.Expect(err).NotTo(HaveOccurred(),
		"an OVNChassis that has not been applied yet is a wait, not a reconcile failure")
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))

	got := getAgent(t, r.Client)
	cond := agentCondition(got, conditionTypeChassisReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonChassisNotFound))
	g.Expect(agentCondition(got, "Ready").Status).To(Equal(metav1.ConditionFalse))

	// The steps behind the gate never ran, so nothing was projected.
	var daemonSets appsv1.DaemonSetList
	g.Expect(r.List(ctx, &daemonSets)).To(Succeed())
	g.Expect(daemonSets.Items).To(BeEmpty())
	g.Expect(agentCondition(got, conditionTypeDaemonSetReady)).To(BeNil())
}

// TestAgentPipelineSteps_OrderAndNames pins the pipeline: four named steps in
// dependency order and no parallel group, because each step consumes what the
// previous one resolved.
func TestAgentPipelineSteps_OrderAndNames(t *testing.T) {
	g := NewGomegaWithT(t)
	r := &NeutronMetadataAgentReconciler{}

	var named []string
	for _, step := range r.pipelineSteps(nil, validAgent()) {
		named = append(named, step.Name)
	}

	g.Expect(named).To(Equal([]string{"Chassis", "Secrets", "Config", "DaemonSet"}))
}

// registerAgentIndexes is the single registration site for the agent's indexes,
// the OVNChassis one included. A manager that starts without them serves watch
// legs that resolve to nothing.
func TestRegisterAgentIndexes(t *testing.T) {
	g := NewGomegaWithT(t)
	indexer := &recordingFieldIndexer{}

	g.Expect(registerAgentIndexes(context.Background(), indexer)).To(Succeed())

	g.Expect(indexer.keys).To(ConsistOf(
		NeutronMetadataAgentSecretNameIndexKey,
		NeutronMetadataAgentChassisRefIndexKey,
		OVNChassisCentralRefIndexKey,
	))
}

// recordingFieldIndexer captures the keys registerAgentIndexes registers.
type recordingFieldIndexer struct{ keys []string }

func (i *recordingFieldIndexer) IndexField(_ context.Context, _ client.Object, field string, _ client.IndexerFunc) error {
	i.keys = append(i.keys, field)
	return nil
}

// --- remote children -------------------------------------------------------

// terminatingRemoteAgent returns a NeutronMetadataAgent that names a target
// cluster and is being deleted, carrying the remote-children finalizer plus a
// foreign one so the CR survives the release and the test can read back which
// finalizers the pass dropped.
func terminatingRemoteAgent(t *testing.T) (*NeutronMetadataAgentReconciler, *neutronv1alpha1.NeutronMetadataAgent) {
	t.Helper()
	g := NewGomegaWithT(t)

	cr := validAgent()
	cr.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "target"}
	cr.Finalizers = []string{commonmulticluster.RemoteChildrenFinalizer, "foreign.example.com/keep-alive"}
	deletedAt := metav1.NewTime(time.Now())
	cr.DeletionTimestamp = &deletedAt

	r := newAgentTestReconciler(cr)
	var terminating neutronv1alpha1.NeutronMetadataAgent
	g.Expect(r.Get(context.Background(), agentRequest.NamespacedName, &terminating)).To(Succeed())
	return r, &terminating
}

// No garbage collection cascade crosses the cluster boundary, so the objects
// this CR projected have to be deleted by name from here. What the sweep leaves
// standing matters as much: a target cluster carries other people's objects, and
// an object nobody claimed or another CR claimed is not ours to remove.
func TestReconcileAgentDeleteRemoteChildren_SweepsEveryLabelledChild(t *testing.T) {
	g := NewGomegaWithT(t)
	r, terminating := terminatingRemoteAgent(t)

	daemonSet := ownedRemoteChild(t, terminating, &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: agentDaemonSetKey.Name, Namespace: testNamespace},
	})
	configMap := ownedRemoteChild(t, terminating, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: testAgentName + "-config-abcdef12", Namespace: testNamespace},
	})
	secret := ownedRemoteChild(t, terminating, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testAgentName + "-transport-url", Namespace: testNamespace},
	})
	// The client identity is cert-manager's and the central's, mounted by name;
	// this controller never wrote it, so it is not labelled and must survive.
	clientSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "ovn-client", Namespace: testNamespace}}
	foreign := ownedRemoteChild(t,
		&neutronv1alpha1.NeutronMetadataAgent{ObjectMeta: metav1.ObjectMeta{
			Name: "other", Namespace: testNamespace,
		}},
		&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{
			Name: "other-metadata-agent", Namespace: testNamespace,
		}})
	target := neutronFakeClientBuilder(daemonSet, configMap, secret, clientSecret, foreign).Build()

	ctx := context.Background()
	err := r.reconcileDeleteRemoteChildren(ctx, commonmulticluster.Remote(target), terminating)

	g.Expect(err).NotTo(HaveOccurred())

	for _, child := range []client.Object{daemonSet, configMap, secret} {
		err := target.Get(ctx, client.ObjectKeyFromObject(child), child.DeepCopyObject().(client.Object))
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "%T %s must be swept", child, child.GetName())
	}
	g.Expect(target.Get(ctx, client.ObjectKeyFromObject(clientSecret), &corev1.Secret{})).To(Succeed(),
		"the client Secret the central publishes is nobody's child here")
	g.Expect(target.Get(ctx, client.ObjectKeyFromObject(foreign), &appsv1.DaemonSet{})).To(Succeed(),
		"another agent's child must survive this one's teardown")

	updated := getAgent(t, r.Client)
	g.Expect(updated.Finalizers).To(ConsistOf("foreign.example.com/keep-alive"),
		"a completed sweep must release the remote-children finalizer and nothing else")
}

// A deregistered target cluster cannot be reached, so holding the finalizer
// would only strand the CR in Terminating; the objects left running are
// announced rather than silently dropped. Any attempt to sweep through the nil
// client would fault, which is what proves nothing was deleted.
func TestReconcileAgentDeleteRemoteChildren_NilChildrenAbandonsAndReleases(t *testing.T) {
	g := NewGomegaWithT(t)
	r, terminating := terminatingRemoteAgent(t)

	err := r.reconcileDeleteRemoteChildren(context.Background(), nil, terminating)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).
		To(ContainElement(ContainSubstring("RemoteChildrenAbandoned")))
	g.Expect(getAgent(t, r.Client).Finalizers).
		NotTo(ContainElement(commonmulticluster.RemoteChildrenFinalizer),
			"an unreachable target cluster must not pin the CR forever")
}

// A local CR never carries the finalizer, its children are collected from their
// owner references, and a sweep running anyway would delete objects on the
// management cluster that the cascade already owns.
func TestReconcileAgentDeleteRemoteChildren_WithoutFinalizerIsANoOp(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := validAgent()
	r := newAgentTestReconciler(cr)

	child := ownedRemoteChild(t, cr, &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: agentDaemonSetKey.Name, Namespace: testNamespace},
	})
	target := neutronFakeClientBuilder(child).Build()

	ctx := context.Background()
	err := r.reconcileDeleteRemoteChildren(ctx, commonmulticluster.Remote(target), cr)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(target.Get(ctx, client.ObjectKeyFromObject(child), &appsv1.DaemonSet{})).To(Succeed(),
		"a CR without the finalizer must not sweep anything")
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).To(BeEmpty())
}

// A list the target cluster refuses says nothing about whether children exist,
// so the pass has to fail and sweep again on the next one rather than let the CR
// leave etcd while its DaemonSet keeps running.
func TestReconcileAgentDeleteRemoteChildren_SweepFailureKeepsFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)
	r, terminating := terminatingRemoteAgent(t)

	child := ownedRemoteChild(t, terminating, &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: agentDaemonSetKey.Name, Namespace: testNamespace},
	})
	target := neutronFakeClientBuilder(child).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if list.GetObjectKind().GroupVersionKind().Kind == "DaemonSetList" {
					return apierrors.NewForbidden(appsv1.Resource("daemonsets"), "", nil)
				}
				return c.List(ctx, list, opts...)
			},
		}).Build()

	ctx := context.Background()
	err := r.reconcileDeleteRemoteChildren(ctx, commonmulticluster.Remote(target), terminating)

	g.Expect(err).To(MatchError(ContainSubstring("listing remote DaemonSet children for teardown")))
	g.Expect(target.Get(ctx, client.ObjectKeyFromObject(child), &appsv1.DaemonSet{})).To(Succeed())
	g.Expect(getAgent(t, r.Client).Finalizers).
		To(ContainElement(commonmulticluster.RemoteChildrenFinalizer),
			"a failed sweep must keep the finalizer so the next pass retries")
}
