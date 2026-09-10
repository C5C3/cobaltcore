// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Unit tests for the Cinder reconcile pipeline: the finalizer contract, the
// target-cluster gates, the teardown of the children a placed CR projected, and
// the Ready aggregation the sub-conditions feed.
package controller

import (
	"context"
	"testing"
	"time"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	mcruntime "sigs.k8s.io/multicluster-runtime/pkg/multicluster"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	cinderv1alpha1 "github.com/c5c3/cobaltcore/operators/cinder/api/v1alpha1"
	"github.com/c5c3/cobaltcore/operators/cinder/internal/metrics"
)

// getCinder re-reads the Cinder CR from the given client.
func getCinder(t *testing.T, c client.Client, name string) *cinderv1alpha1.Cinder {
	t.Helper()
	var cinder cinderv1alpha1.Cinder
	if err := c.Get(context.Background(), objectKey(name), &cinder); err != nil {
		t.Fatalf("re-reading Cinder %s: %v", name, err)
	}
	return &cinder
}

// --- the finalizer contract -------------------------------------------------

func TestReconcile_AddsFinalizerOnFirstPass(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder() // no finalizer yet
	r := newCinderTestReconciler(cinder)

	res, err := r.Reconcile(context.Background(), cinderRequest)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{RequeueAfter: commonreconcile.RequeueNextPass}),
		"the finalizer add requeues so the next pass sees it persisted")
	g.Expect(getCinder(t, r.Client, testCinderName).Finalizers).To(ContainElement(cinderFinalizer))
}

// TestReconcile_MissingCRIsDone covers the ordinary race between a delete and
// the queued event that still names the CR: nothing is left to reconcile, so the
// pass must end quietly instead of erroring the item back onto the queue.
func TestReconcile_MissingCRIsDone(t *testing.T) {
	g := NewGomegaWithT(t)
	r := newCinderTestReconciler()

	res, err := r.Reconcile(context.Background(), cinderRequest)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
}

// TestReconcile_FailingSecretsStepShortCircuitsPipeline pins the short-circuit:
// the first step to return a non-zero result ends the pass, so no later step
// gets to write a condition that would read as progress.
func TestReconcile_FailingSecretsStepShortCircuitsPipeline(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	cinder.Finalizers = []string{cinderFinalizer} // skip the finalizer-add requeue
	// The selected store is explicitly not Ready, so the Secrets step fails fast.
	r := newCinderTestReconciler(cinder, notReadyClusterSecretStore(openBaoClusterStoreName))

	res, err := r.Reconcile(context.Background(), cinderRequest)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))

	got := getCinder(t, r.Client, testCinderName)
	secretsReady := conditions.GetCondition(got.Status.Conditions, "SecretsReady")
	g.Expect(secretsReady).NotTo(BeNil())
	g.Expect(secretsReady.Status).To(Equal(metav1.ConditionFalse))
	// No later step ran: neither the backend projection nor the database step
	// ever reported.
	g.Expect(conditions.GetCondition(got.Status.Conditions, conditionTypeBackendsReady)).To(BeNil())
	g.Expect(conditions.GetCondition(got.Status.Conditions, "DatabaseReady")).To(BeNil())
}

// --- target-cluster gates ---------------------------------------------------

// unresolvableResolver is a ClusterResolver that never knows any cluster. It
// returns the upstream sentinel so the test asserts the message an operator
// actually reads on the CR, not a locally invented string.
type unresolvableResolver struct{}

func (unresolvableResolver) GetCluster(_ context.Context, _ mcruntime.ClusterName) (cluster.Cluster, error) {
	return nil, mcruntime.ErrClusterNotFound
}

// TestReconcile_TargetClusterUnavailableGatesBeforeFinalizer pins the failure
// surface of an unresolvable spec.targetClusterRef: the CR reports
// SecretsReady=False with the shared reason, the pass requeues instead of
// erroring, and the CR is left with neither a finalizer nor a single child
// object, because the resolution runs ahead of the finalizer-add.
func TestReconcile_TargetClusterUnavailableGatesBeforeFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	cinder.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "nowhere"}
	r := newCinderTestReconciler(cinder)
	r.Resolver = unresolvableResolver{}

	ctx := context.Background()
	res, err := r.Reconcile(ctx, cinderRequest)

	g.Expect(err).NotTo(HaveOccurred(),
		"an unregistered target cluster is a wait, not a reconcile failure")
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))

	got := getCinder(t, r.Client, testCinderName)
	cond := conditions.GetCondition(got.Status.Conditions, "SecretsReady")
	g.Expect(cond).NotTo(BeNil(), "the first gate condition must carry the failure")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(commonmulticluster.TargetClusterUnavailable))
	g.Expect(cond.Message).To(ContainSubstring("cluster not found"))

	g.Expect(got.Finalizers).To(BeEmpty(),
		"a CR whose target never resolves must not be pinned by a finalizer")

	// Nothing was projected: the pass returned before any sub-reconciler could
	// write, and the management cluster is the only one a failing resolver could
	// have handed back.
	var deployments appsv1.DeploymentList
	g.Expect(r.List(ctx, &deployments)).To(Succeed())
	g.Expect(deployments.Items).To(BeEmpty())
	var secrets corev1.SecretList
	g.Expect(r.List(ctx, &secrets)).To(Succeed())
	g.Expect(secrets.Items).To(BeEmpty())
	var cronJobs batchv1.CronJobList
	g.Expect(r.List(ctx, &cronJobs)).To(Succeed())
	g.Expect(cronJobs.Items).To(BeEmpty())
}

// TestReconcile_TerminatingCR_UnresolvableTargetReleasesFinalizer is the guard
// against a CR that can never be deleted. Deregistering a target cluster is a
// documented operation, and a CR that already provisioned on it carries the
// finalizer by then. While the resolution ran ahead of the deletion branch,
// every pass short-circuited on "cluster not found" before reconcileDelete, the
// finalizer was never released, and the CR stayed Terminating until someone
// stripped it by hand.
func TestReconcile_TerminatingCR_UnresolvableTargetReleasesFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)
	// The window this process has to sit out starts on its own first failure to
	// resolve, so it has to be compressed for the second pass to reach past it.
	abandonAfter := commonmulticluster.AbandonAfter
	t.Cleanup(func() { commonmulticluster.AbandonAfter = abandonAfter })
	commonmulticluster.AbandonAfter = time.Millisecond

	cinder := validCinder()
	cinder.Finalizers = []string{cinderFinalizer}
	cinder.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "deregistered"}
	// Terminating for far longer than the window, which by itself must not be
	// enough: a CR blocked in cleanup for minutes is ordinary, and giving up on it
	// the moment the operator comes back would strand its children.
	deletedAt := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	cinder.DeletionTimestamp = &deletedAt
	r := newCinderTestReconciler(cinder)
	r.Resolver = unresolvableResolver{}

	res, err := r.Reconcile(context.Background(), cinderRequest)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling),
		"the first pass this process fails to resolve on starts the window, it does not end it")
	g.Expect(getCinder(t, r.Client, testCinderName).Finalizers).To(ContainElement(cinderFinalizer))
	time.Sleep(10 * commonmulticluster.AbandonAfter)

	res, err = r.Reconcile(context.Background(), cinderRequest)

	g.Expect(err).NotTo(HaveOccurred(),
		"a target cluster that is gone must not fail the deletion pass")
	g.Expect(res.IsZero()).To(BeTrue(), "the deletion resolves once the window is out")

	// With the finalizer released, the fake client garbage-collects the CR.
	var gone cinderv1alpha1.Cinder
	err = r.Get(context.Background(), cinderRequest.NamespacedName, &gone)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
		"the finalizer must be released even without a reachable target cluster")

	// The abandoned children are announced rather than silently dropped.
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).
		To(ContainElement(ContainSubstring("RemoteChildrenAbandoned")))
}

// TestReconcile_TerminatingCR_TargetNotEngagedYetKeepsFinalizer pins the other
// half of that contract. Cluster engagement is asynchronous, so a registered
// cluster does not resolve either while the provider is still syncing after an
// operator restart. A CR deleted in that window must requeue rather than release
// its finalizer: abandoning here would leave its children running on a cluster
// that is perfectly reachable, with no CR left to retry from.
func TestReconcile_TerminatingCR_TargetNotEngagedYetKeepsFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	cinder.Finalizers = []string{cinderFinalizer}
	cinder.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "not-engaged-yet"}
	r := newCinderTestReconciler(cinder)
	r.Resolver = unresolvableResolver{}

	// Deleted just now, so the whole abandon window is still ahead.
	g.Expect(r.Delete(context.Background(), cinder)).To(Succeed())

	res, err := r.Reconcile(context.Background(), cinderRequest)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))

	got := getCinder(t, r.Client, testCinderName)
	g.Expect(got.Finalizers).To(ContainElement(cinderFinalizer),
		"a target that may still be engaging must not cost the CR its finalizer")
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).
		NotTo(ContainElement(ContainSubstring("RemoteChildrenAbandoned")))

	// The hold lasts minutes, so it has to be readable off the CR: without it, a
	// namespace stuck Terminating looks like a wedged finalizer and can only be
	// told apart by correlating operator logs across replicas.
	cond := conditions.GetCondition(got.Status.Conditions, "SecretsReady")
	g.Expect(cond).NotTo(BeNil(), "the deliberate hold must be visible on the CR")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(commonmulticluster.TargetClusterUnavailable))
	g.Expect(cond.Message).To(ContainSubstring("not-engaged-yet"))
}

func TestReconcile_InstallsRemoteChildrenFinalizerForATargetCluster(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	cinder.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "target"}
	cinder.Finalizers = []string{cinderFinalizer} // skip the database finalizer-add requeue
	r := newCinderTestReconciler(cinder)

	res, err := r.Reconcile(context.Background(), cinderRequest)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{RequeueAfter: commonreconcile.RequeueNextPass}),
		"the pass installing the finalizer must requeue before any sub-reconciler runs")
	g.Expect(getCinder(t, r.Client, testCinderName).Finalizers).
		To(ContainElement(commonmulticluster.RemoteChildrenFinalizer))
}

// TestReconcile_LocalCRNeverCarriesTheRemoteChildrenFinalizer pins the other
// half. A CR that keeps its children on the management cluster has nothing for
// the sweep to do, and a finalizer it does not need is one more thing that can
// block its deletion.
func TestReconcile_LocalCRNeverCarriesTheRemoteChildrenFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder() // no spec.targetClusterRef: children stay local
	cinder.Finalizers = []string{cinderFinalizer}
	r := newCinderTestReconciler(cinder)

	_, err := r.Reconcile(context.Background(), cinderRequest)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getCinder(t, r.Client, testCinderName).Finalizers).To(ConsistOf(cinderFinalizer),
		"a local CR must keep the finalizer set it always had")
}

// --- reconcileDelete --------------------------------------------------------

func TestReconcileDelete_LiveResourcesRetainFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	cinder.Finalizers = []string{cinderFinalizer}
	// A live MariaDB Database owned by this Cinder (key is the bare CR name).
	mdb := &mariadbv1alpha1.Database{}
	mdb.Name = testCinderName
	mdb.Namespace = testNamespace
	r := newCinderTestReconciler(cinder, mdb)

	// Move the Cinder into the deleting state.
	g.Expect(r.Delete(context.Background(), cinder)).To(Succeed())

	res, err := r.Reconcile(context.Background(), cinderRequest)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueDatabaseWait))
	g.Expect(getCinder(t, r.Client, testCinderName).Finalizers).To(ContainElement(cinderFinalizer),
		"the finalizer is retained one pass while live MariaDB resources remain")
}

// TestReconcileDelete_NoLiveResourcesReleasesFinalizerAndDropsMetrics covers the
// terminal pass: the CR leaves etcd and its per-CR series go with it, so a Cinder
// recreated under the same name does not inherit the counters of the one before
// it.
func TestReconcileDelete_NoLiveResourcesReleasesFinalizerAndDropsMetrics(t *testing.T) {
	g := NewGomegaWithT(t)
	g.Expect(metrics.Register()).To(Succeed())

	cinder := validCinder()
	cinder.Name = "delete-drops-metrics"
	cinder.Finalizers = []string{cinderFinalizer}
	t.Cleanup(func() { metrics.DeleteForCinder(cinder.Name, cinder.Namespace) })
	metrics.RecordDBPurge(cinder.Name, cinder.Namespace, "failed", time.Second)
	g.Expect(dbPurgeCount(t, cinder, "failed")).To(Equal(1.0), "the fixture series must exist to be dropped")

	r := newCinderTestReconciler(cinder) // no MariaDB CRs
	g.Expect(r.Delete(context.Background(), cinder)).To(Succeed())

	res, err := r.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: objectKey(cinder.Name)})

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	// With the finalizer released, the fake client garbage-collects the CR.
	var gone cinderv1alpha1.Cinder
	err = r.Get(context.Background(), objectKey(cinder.Name), &gone)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
		"the finalizer is released when no live MariaDB resource remains")
	g.Expect(dbPurgeCount(t, cinder, "failed")).To(BeZero(),
		"the per-CR series must not outlive the CR they are labelled with")
}

// TestReconcileDelete_WithoutTheFinalizerIsANoOp guards the entry condition: a CR
// this operator never claimed must not have its database CRs deleted out from
// under whoever does own them.
func TestReconcileDelete_WithoutTheFinalizerIsANoOp(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder() // no finalizer
	mdb := &mariadbv1alpha1.Database{}
	mdb.Name = testCinderName
	mdb.Namespace = testNamespace
	r := newCinderTestReconciler(cinder, mdb)

	ctx := context.Background()
	res, err := r.reconcileDelete(ctx, r.Client, cinder)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(r.Get(ctx, objectKey(testCinderName), &mariadbv1alpha1.Database{})).To(Succeed(),
		"an unclaimed CR must not trigger a database teardown")
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).To(BeEmpty())
}

// --- remote children --------------------------------------------------------

// ownedRemoteChild stamps the ownership labels the projection wrote on a child it
// put on the target cluster, so the sweep selects it exactly as it would there.
func ownedRemoteChild(t *testing.T, owner, child client.Object) client.Object {
	t.Helper()
	labels, err := commonmulticluster.OwnerLabels(testScheme(), owner)
	NewGomegaWithT(t).Expect(err).NotTo(HaveOccurred())
	child.SetLabels(labels)
	return child
}

// terminatingRemoteCinder returns a Cinder that names a target cluster and is
// being deleted, carrying the remote-children finalizer plus a foreign one so the
// CR survives the release and the test can read back which finalizers the pass
// dropped.
func terminatingRemoteCinder(t *testing.T) (*CinderReconciler, *cinderv1alpha1.Cinder) {
	t.Helper()
	g := NewGomegaWithT(t)
	cinder := validCinder()
	cinder.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "target"}
	cinder.Finalizers = []string{commonmulticluster.RemoteChildrenFinalizer, "foreign.example.com/keep-alive"}
	deletedAt := metav1.NewTime(time.Now())
	cinder.DeletionTimestamp = &deletedAt

	r := newCinderTestReconciler(cinder)
	var terminating cinderv1alpha1.Cinder
	g.Expect(r.Get(context.Background(), cinderRequest.NamespacedName, &terminating)).To(Succeed())
	return r, &terminating
}

// TestReconcileDeleteRemoteChildren_SweepsEveryLabelledChild is the whole point
// of the finalizer: no garbage collection cascade crosses the cluster boundary,
// so the objects this CR projected have to be deleted by name from here, across
// every API group they live in. What the sweep leaves standing matters as much: a
// target cluster carries other people's objects, and an object nobody claimed or
// another CR claimed is not ours to remove.
func TestReconcileDeleteRemoteChildren_SweepsEveryLabelledChild(t *testing.T) {
	g := NewGomegaWithT(t)
	r, terminating := terminatingRemoteCinder(t)

	deployment := ownedRemoteChild(t, terminating,
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: testCinderName + "-volume-a", Namespace: testNamespace}})
	service := ownedRemoteChild(t, terminating,
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: testCinderName, Namespace: testNamespace}})
	cronJob := ownedRemoteChild(t, terminating,
		&batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: testCinderName + "-db-purge", Namespace: testNamespace}})
	policy := ownedRemoteChild(t, terminating,
		&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: testCinderName, Namespace: testNamespace}})
	unlabelled := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cluster-ca", Namespace: testNamespace}}
	foreign := ownedRemoteChild(t,
		&cinderv1alpha1.Cinder{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: testNamespace}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "other-backend-a", Namespace: testNamespace}})
	target := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(deployment, service, cronJob, policy, unlabelled, foreign).Build()

	ctx := context.Background()
	err := r.reconcileDeleteRemoteChildren(ctx, commonmulticluster.Remote(target), terminating)

	g.Expect(err).NotTo(HaveOccurred())

	for _, child := range []client.Object{deployment, service, cronJob, policy} {
		err := target.Get(ctx, client.ObjectKeyFromObject(child), child.DeepCopyObject().(client.Object))
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "%T %s must be swept", child, child.GetName())
	}
	g.Expect(target.Get(ctx, client.ObjectKeyFromObject(unlabelled), &corev1.ConfigMap{})).To(Succeed(),
		"an object nobody claimed is nobody's child")
	g.Expect(target.Get(ctx, client.ObjectKeyFromObject(foreign), &corev1.Secret{})).To(Succeed(),
		"another Cinder's child must survive this one's teardown")

	g.Expect(getCinder(t, r.Client, testCinderName).Finalizers).To(ConsistOf("foreign.example.com/keep-alive"),
		"a completed sweep must release the remote-children finalizer and nothing else")
}

// TestReconcileDeleteRemoteChildren_NilChildrenAbandonsAndReleases covers the
// deregistered target cluster. Its children cannot be reached, so holding the
// finalizer would only strand the CR in Terminating; the objects left running are
// announced rather than silently dropped. Any attempt to sweep through the nil
// client would fault, which is what proves nothing was deleted.
func TestReconcileDeleteRemoteChildren_NilChildrenAbandonsAndReleases(t *testing.T) {
	g := NewGomegaWithT(t)
	r, terminating := terminatingRemoteCinder(t)

	err := r.reconcileDeleteRemoteChildren(context.Background(), nil, terminating)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).
		To(ContainElement(ContainSubstring("RemoteChildrenAbandoned")))
	g.Expect(getCinder(t, r.Client, testCinderName).Finalizers).
		NotTo(ContainElement(commonmulticluster.RemoteChildrenFinalizer),
			"an unreachable target cluster must not pin the CR forever")
}

// TestReconcileDeleteRemoteChildren_WithoutFinalizerIsANoOp pins the guard a
// local CR relies on. It never carries the finalizer, its children are collected
// from their owner references, and a sweep running anyway would delete objects on
// the management cluster that the cascade already owns.
func TestReconcileDeleteRemoteChildren_WithoutFinalizerIsANoOp(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	r := newCinderTestReconciler(cinder)

	child := ownedRemoteChild(t, cinder,
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: testCinderName, Namespace: testNamespace}})
	target := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(child).Build()

	ctx := context.Background()
	err := r.reconcileDeleteRemoteChildren(ctx, commonmulticluster.Remote(target), cinder)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(target.Get(ctx, client.ObjectKeyFromObject(child), &appsv1.Deployment{})).To(Succeed(),
		"a CR without the finalizer must not sweep anything")
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).To(BeEmpty())
}

// TestReconcileDeleteRemoteChildren_SweepFailureKeepsFinalizer is the guard
// against a CR that leaves etcd while its children keep running. A list the
// target cluster refuses says nothing about whether children exist, so the pass
// has to fail and sweep again on the next one.
func TestReconcileDeleteRemoteChildren_SweepFailureKeepsFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)
	r, terminating := terminatingRemoteCinder(t)

	child := ownedRemoteChild(t, terminating,
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: testCinderName, Namespace: testNamespace}})
	target := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(child).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if list.GetObjectKind().GroupVersionKind().Kind == "DeploymentList" {
					return apierrors.NewForbidden(appsv1.Resource("deployments"), "", nil)
				}
				return c.List(ctx, list, opts...)
			},
		}).Build()

	ctx := context.Background()
	err := r.reconcileDeleteRemoteChildren(ctx, commonmulticluster.Remote(target), terminating)

	g.Expect(err).To(MatchError(ContainSubstring("listing remote Deployment children for teardown")))
	g.Expect(target.Get(ctx, client.ObjectKeyFromObject(child), &appsv1.Deployment{})).To(Succeed())
	g.Expect(getCinder(t, r.Client, testCinderName).Finalizers).
		To(ContainElement(commonmulticluster.RemoteChildrenFinalizer),
			"a failed sweep must keep the finalizer so the next pass retries")
}

// --- indexes and the Ready aggregation --------------------------------------

// recordingFieldIndexer is a client.FieldIndexer that records the keys it was
// asked to register, so the registration helper can be exercised without a
// running manager.
type recordingFieldIndexer struct {
	keys []string
}

func (r *recordingFieldIndexer) IndexField(_ context.Context, _ client.Object, field string, _ client.IndexerFunc) error {
	r.keys = append(r.keys, field)
	return nil
}

// The two satellite kinds index under the same field path, so the registration
// has to be counted per kind rather than per key name: a helper that registered
// only one of them would leave the other's fan-out listing every CR.
func TestRegisterCinderIndexes_RegistersOnePerIndexedKind(t *testing.T) {
	g := NewGomegaWithT(t)

	idx := &recordingFieldIndexer{}
	g.Expect(registerCinderIndexes(context.Background(), idx)).To(Succeed())

	g.Expect(idx.keys).To(ConsistOf(CinderSecretNameIndexKey,
		CinderBackendCinderRefIndexKey, CinderBackupBackendCinderRefIndexKey))
}

// TestSubConditionTypes_PinsTheAggregatedVocabulary keeps the Ready contract
// deliberate: every entry is a condition some sub-reconciler sets, and
// ExtraConfigHealthy stays out because a user-owned overlay must not depool an
// API that serves.
func TestSubConditionTypes_PinsTheAggregatedVocabulary(t *testing.T) {
	g := NewGomegaWithT(t)

	g.Expect(subConditionTypes).To(ConsistOf(
		"SecretsReady",
		"BackendsReady",
		"BackupBackendReady",
		"DatabaseReady",
		"SchedulerReady",
		"VolumeServicesReady",
		"BackupServiceReady",
		"DeploymentReady",
		"CinderAPIReady",
		"HPAReady",
		"NetworkPolicyReady",
		"HTTPRouteReady",
		"DBPurgeReady",
	))
	g.Expect(subConditionTypes).NotTo(ContainElement("ExtraConfigHealthy"),
		"the extraConfig overlay is informational and must not gate Ready")
}

func TestSetReadyCondition_TrueOnlyWhenAllSubConditionsTrue(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()

	// Every sub-condition True → aggregate Ready True.
	for _, ct := range subConditionTypes {
		conditions.SetCondition(&cinder.Status.Conditions, metav1.Condition{
			Type:   ct,
			Status: metav1.ConditionTrue,
			Reason: "OK",
		})
	}
	setReadyCondition(cinder)
	ready := conditions.GetCondition(cinder.Status.Conditions, "Ready")
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))

	// Flip one sub-condition False → aggregate Ready flips False.
	conditions.SetCondition(&cinder.Status.Conditions, metav1.Condition{
		Type:   "VolumeServicesReady",
		Status: metav1.ConditionFalse,
		Reason: "Degraded",
	})
	setReadyCondition(cinder)
	ready = conditions.GetCondition(cinder.Status.Conditions, "Ready")
	g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
}

// --- the export egress the pipeline hands the network policy -----------------

func TestPipelineState_PolicyShareHosts(t *testing.T) {
	g := NewGomegaWithT(t)

	// No backup target: the volume backends' exports are passed through untouched.
	state := &pipelineState{shareHosts: testShareHosts}
	g.Expect(state.policyShareHosts()).To(Equal(testShareHosts))

	// A backup target adds its own export, and the backends' slice is not
	// rewritten under the step that produced it.
	state.backup = &backupProjection{name: "vault", server: "backup.nfs.example.com"}
	g.Expect(state.policyShareHosts()).To(Equal(append(append([]string{}, testShareHosts...),
		"tcp://backup.nfs.example.com:2049")))
	g.Expect(state.shareHosts).To(Equal(testShareHosts))

	// A backup-only Cinder still reports one export.
	backupOnly := &pipelineState{backup: state.backup}
	g.Expect(backupOnly.policyShareHosts()).To(ConsistOf("tcp://backup.nfs.example.com:2049"))
}

// TestParallelSteps_NetworkPolicyOpensTheBackupExport is the end of that thread:
// the backup target's export has to reach the rendered policy, or a Cinder whose
// only share is the backup one would have its cinder-backup pod blocked from the
// NFS server it writes to.
func TestParallelSteps_NetworkPolicyOpensTheBackupExport(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	cinder.Spec.NetworkPolicy = cinderNetworkPolicySpec()
	r := newNetworkPolicyTestReconciler(cinder)

	// No volume backend at all, so 2049 can only come from the backup target.
	state := &pipelineState{backup: &backupProjection{name: "vault", server: "backup.nfs.example.com"}}
	member := networkPolicyMember(t, r.parallelSteps(r.Client, state))

	res, err := member.Fn(context.Background(), cinder)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	var np networkingv1.NetworkPolicy
	g.Expect(r.Get(context.Background(), objectKey(testCinderName), &np)).To(Succeed())
	g.Expect(egressPorts(&np)).To(ContainElement(nfsEgressPort),
		"the backup share must open the NFS egress rule on its own")
}

// TestParallelSteps_NoShareOpensNoExportEgress is the discriminating half: with
// neither a volume backend nor a backup target there is no export to reach, so
// the rule must be absent rather than opened by default.
func TestParallelSteps_NoShareOpensNoExportEgress(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	cinder.Spec.NetworkPolicy = cinderNetworkPolicySpec()
	r := newNetworkPolicyTestReconciler(cinder)

	member := networkPolicyMember(t, r.parallelSteps(r.Client, &pipelineState{}))

	_, err := member.Fn(context.Background(), cinder)

	g.Expect(err).NotTo(HaveOccurred())
	var np networkingv1.NetworkPolicy
	g.Expect(r.Get(context.Background(), objectKey(testCinderName), &np)).To(Succeed())
	g.Expect(egressPorts(&np)).NotTo(ContainElement(nfsEgressPort))
}

// networkPolicyMember picks the NetworkPolicy member out of the parallel group so
// the tests above drive the production wiring rather than calling the
// sub-reconciler with hand-assembled arguments.
func networkPolicyMember(t *testing.T, subs []commonreconcile.ParallelStep[*cinderv1alpha1.Cinder],
) commonreconcile.ParallelStep[*cinderv1alpha1.Cinder] {
	t.Helper()
	for _, sub := range subs {
		if sub.Name == "NetworkPolicy" {
			return sub
		}
	}
	t.Fatal("the parallel group carries no NetworkPolicy member")
	return commonreconcile.ParallelStep[*cinderv1alpha1.Cinder]{}
}
