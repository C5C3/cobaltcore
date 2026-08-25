// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Unit tests for the field-index extractors and registration helpers shared by
// the Glance and GlanceBackend controllers.
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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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
	glancev1alpha1 "github.com/c5c3/cobaltcore/operators/glance/api/v1alpha1"
)

func TestReconcile_AddsFinalizerOnFirstPass(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance() // no finalizer yet
	r := newGlanceTestReconciler(glance)

	res, err := r.Reconcile(context.Background(), glanceRequest)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{RequeueAfter: commonreconcile.RequeueNextPass}), "the finalizer add requeues so the next pass sees it persisted")
	got := getGlance(t, r.Client, "test-glance")
	g.Expect(got.Finalizers).To(ContainElement(glanceFinalizer))
}

func TestReconcile_FailingSecretsStepShortCircuitsPipeline(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	glance.Finalizers = []string{glanceFinalizer} // skip the finalizer-add requeue
	// The selected store is explicitly not Ready, so the Secrets step fails fast.
	r := newGlanceTestReconciler(glance, notReadyClusterSecretStore(openBaoClusterStoreName))

	res, err := r.Reconcile(context.Background(), glanceRequest)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))

	got := getGlance(t, r.Client, "test-glance")
	secrets := conditions.GetCondition(got.Status.Conditions, "SecretsReady")
	g.Expect(secrets).NotTo(BeNil())
	g.Expect(secrets.Status).To(Equal(metav1.ConditionFalse))
	// A later step must not have run: BackendsReady is never set when Secrets
	// short-circuits the pipeline.
	g.Expect(conditions.GetCondition(got.Status.Conditions, "BackendsReady")).To(BeNil())
	g.Expect(conditions.GetCondition(got.Status.Conditions, "DatabaseReady")).To(BeNil())
}

// unresolvableResolver is a ClusterResolver that never knows any cluster. It
// returns the upstream sentinel so the test asserts the message an operator
// actually reads on the CR, not a locally invented string.
type unresolvableResolver struct{}

func (unresolvableResolver) GetCluster(_ context.Context, _ mcruntime.ClusterName) (cluster.Cluster, error) {
	return nil, mcruntime.ErrClusterNotFound
}

// TestReconcile_TerminatingCR_UnresolvableTargetReleasesFinalizer is the guard against a
// CR that can never be deleted. Deregistering a target cluster is a documented
// operation, and a CR that already provisioned on it carries the finalizer by
// then. While the resolution ran ahead of the deletion branch, every pass
// short-circuited on "cluster not found" before reconcileDelete, the finalizer
// was never released, and the CR — with its namespace — stayed Terminating
// until someone stripped it by hand.
func TestReconcile_TerminatingCR_UnresolvableTargetReleasesFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)
	// The window this process has to sit out starts on its own first failure to
	// resolve, so it has to be compressed for the second pass to reach past it.
	abandonAfter := commonmulticluster.AbandonAfter
	t.Cleanup(func() { commonmulticluster.AbandonAfter = abandonAfter })
	commonmulticluster.AbandonAfter = time.Millisecond

	glance := testGlance()
	glance.Finalizers = []string{glanceFinalizer}
	glance.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "deregistered"}
	// Terminating for far longer than the window, which by itself must not be
	// enough: a CR blocked in cleanup for minutes is ordinary, and giving up on
	// it the moment the operator comes back would strand its children.
	deletedAt := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	glance.DeletionTimestamp = &deletedAt
	r := newGlanceTestReconciler(glance)
	r.Resolver = unresolvableResolver{}

	res, err := r.Reconcile(context.Background(), glanceRequest)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling),
		"the first pass this process fails to resolve on starts the window, it does not end it")
	g.Expect(getGlance(t, r.Client, "test-glance").Finalizers).To(ContainElement(glanceFinalizer))
	time.Sleep(10 * commonmulticluster.AbandonAfter)

	res, err = r.Reconcile(context.Background(), glanceRequest)

	g.Expect(err).NotTo(HaveOccurred(),
		"a target cluster that is gone must not fail the deletion pass")
	g.Expect(res.IsZero()).To(BeTrue(), "the deletion resolves once the window is out")

	// With the finalizer released, the fake client garbage-collects the CR.
	var gone glancev1alpha1.Glance
	err = r.Get(context.Background(), glanceRequest.NamespacedName, &gone)
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
	glance := testGlance()
	glance.Finalizers = []string{glanceFinalizer}
	glance.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "not-engaged-yet"}
	r := newGlanceTestReconciler(glance)
	r.Resolver = unresolvableResolver{}

	// Deleted just now, so the whole abandon window is still ahead.
	g.Expect(r.Delete(context.Background(), glance)).To(Succeed())

	res, err := r.Reconcile(context.Background(), glanceRequest)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))

	got := getGlance(t, r.Client, "test-glance")
	g.Expect(got.Finalizers).To(ContainElement(glanceFinalizer),
		"a target that may still be engaging must not cost the CR its finalizer")
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).
		NotTo(ContainElement(ContainSubstring("RemoteChildrenAbandoned")))

	// The hold lasts minutes, so it has to be readable off the CR: without it,
	// a namespace stuck Terminating looks like a wedged finalizer and can only
	// be told apart by correlating operator logs across replicas.
	cond := conditions.GetCondition(got.Status.Conditions, "SecretsReady")
	g.Expect(cond).NotTo(BeNil(), "the deliberate hold must be visible on the CR")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(commonmulticluster.TargetClusterUnavailable))
	g.Expect(cond.Message).To(ContainSubstring("not-engaged-yet"))
}

// TestReconcile_TargetClusterUnavailableGatesBeforeFinalizer pins the failure
// surface of an unresolvable spec.targetClusterRef: the CR reports
// SecretsReady=False with the shared reason, the pass requeues instead of
// erroring, and the CR is left with neither a finalizer nor a single child
// object, because the resolution runs ahead of the finalizer-add.
func TestReconcile_TargetClusterUnavailableGatesBeforeFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	glance.Finalizers = nil
	glance.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "nowhere"}
	r := newGlanceTestReconciler(glance)
	r.Resolver = unresolvableResolver{}

	ctx := context.Background()
	res, err := r.Reconcile(ctx, glanceRequest)

	g.Expect(err).NotTo(HaveOccurred(),
		"an unregistered target cluster is a wait, not a reconcile failure")
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))

	got := getGlance(t, r.Client, "test-glance")
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
	var configMaps corev1.ConfigMapList
	g.Expect(r.List(ctx, &configMaps)).To(Succeed())
	g.Expect(configMaps.Items).To(BeEmpty())
	var cronJobs batchv1.CronJobList
	g.Expect(r.List(ctx, &cronJobs)).To(Succeed())
	g.Expect(cronJobs.Items).To(BeEmpty())
}

func TestReconcileDelete_LiveResourcesRetainFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	glance.Finalizers = []string{glanceFinalizer}
	// A live MariaDB Database owned by this Glance (key is the bare CR name).
	mdb := &mariadbv1alpha1.Database{}
	mdb.Name = "test-glance"
	mdb.Namespace = "default"
	r := newGlanceTestReconciler(glance, mdb)

	// Move the Glance into the deleting state.
	g.Expect(r.Delete(context.Background(), glance)).To(Succeed())

	res, err := r.Reconcile(context.Background(), glanceRequest)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueDatabaseWait))
	got := getGlance(t, r.Client, "test-glance")
	g.Expect(got.Finalizers).To(ContainElement(glanceFinalizer),
		"the finalizer is retained one pass while live MariaDB resources remain")
}

func TestReconcileDelete_NoLiveResourcesReleasesFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	glance.Finalizers = []string{glanceFinalizer}
	r := newGlanceTestReconciler(glance) // no MariaDB CRs

	g.Expect(r.Delete(context.Background(), glance)).To(Succeed())

	res, err := r.Reconcile(context.Background(), glanceRequest)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	// With the finalizer released, the fake client garbage-collects the CR.
	var gone glancev1alpha1.Glance
	err = r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-glance"}, &gone)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
		"the finalizer is released when no live MariaDB resource remains")
}

func TestSetReadyCondition_TrueOnlyWhenAllSubConditionsTrue(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()

	// Every sub-condition True → aggregate Ready True.
	for _, ct := range subConditionTypes {
		conditions.SetCondition(&glance.Status.Conditions, metav1.Condition{
			Type:   ct,
			Status: metav1.ConditionTrue,
			Reason: "OK",
		})
	}
	setReadyCondition(glance)
	ready := conditions.GetCondition(glance.Status.Conditions, "Ready")
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))

	// Flip one sub-condition False → aggregate Ready flips False.
	conditions.SetCondition(&glance.Status.Conditions, metav1.Condition{
		Type:   "HPAReady",
		Status: metav1.ConditionFalse,
		Reason: "Degraded",
	})
	setReadyCondition(glance)
	ready = conditions.GetCondition(glance.Status.Conditions, "Ready")
	g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
}

// recordingFieldIndexer is a client.FieldIndexer that records the keys it was
// asked to register, so the registration helpers can be exercised without a
// running manager.
type recordingFieldIndexer struct {
	keys []string
}

func (r *recordingFieldIndexer) IndexField(_ context.Context, _ client.Object, field string, _ client.IndexerFunc) error {
	r.keys = append(r.keys, field)
	return nil
}

func TestGlanceSecretNameExtractor(t *testing.T) {
	g := NewGomegaWithT(t)

	// serviceUser + database Secret names, deduplicated.
	glance := testGlance()
	g.Expect(glanceSecretNameExtractor(glance)).To(ConsistOf("glance-service-user", "glance-db"))

	// The same Secret backing both references collapses to one entry.
	glance.Spec.Database.SecretRef.Name = "glance-service-user"
	g.Expect(glanceSecretNameExtractor(glance)).To(ConsistOf("glance-service-user"))

	// An empty database Secret name is skipped.
	glance.Spec.Database.SecretRef.Name = ""
	g.Expect(glanceSecretNameExtractor(glance)).To(ConsistOf("glance-service-user"))

	// The wrong object type yields nil rather than a panic.
	g.Expect(glanceSecretNameExtractor(&corev1.Secret{})).To(BeNil())
}

func TestGlanceBackendSecretNameExtractor(t *testing.T) {
	g := NewGomegaWithT(t)

	// Credentials Secret only.
	b := testGlanceBackend("store", "test-glance")
	g.Expect(glanceBackendSecretNameExtractor(b)).To(ConsistOf("store-s3-creds"))

	// A nil S3 block (bypassed admission) indexes nothing.
	b.Spec.S3 = nil
	g.Expect(glanceBackendSecretNameExtractor(b)).To(BeEmpty())

	// The wrong object type yields nil rather than a panic.
	g.Expect(glanceBackendSecretNameExtractor(&corev1.Secret{})).To(BeNil())
}

func TestGlanceBackendGlanceRefExtractor(t *testing.T) {
	g := NewGomegaWithT(t)

	b := testGlanceBackend("store", "test-glance")
	g.Expect(glanceBackendGlanceRefExtractor(b)).To(ConsistOf("test-glance"))

	// An empty glanceRef (bypassed admission) indexes nothing.
	b.Spec.GlanceRef.Name = ""
	g.Expect(glanceBackendGlanceRefExtractor(b)).To(BeNil())

	// The wrong object type yields nil rather than a panic.
	g.Expect(glanceBackendGlanceRefExtractor(&corev1.Secret{})).To(BeNil())
}

func TestRegisterGlanceIndexes_RegistersSecretNameKey(t *testing.T) {
	g := NewGomegaWithT(t)

	idx := &recordingFieldIndexer{}
	g.Expect(registerGlanceIndexes(context.Background(), idx)).To(Succeed())
	g.Expect(idx.keys).To(ConsistOf(GlanceSecretNameIndexKey))
}

func TestRegisterGlanceBackendIndexes_RegistersBothKeys(t *testing.T) {
	g := NewGomegaWithT(t)

	idx := &recordingFieldIndexer{}
	g.Expect(registerGlanceBackendIndexes(context.Background(), idx)).To(Succeed())
	g.Expect(idx.keys).To(ConsistOf(GlanceBackendGlanceRefIndexKey, GlanceBackendSecretNameIndexKey))
}

// --- remote children -------------------------------------------------------

// ownedRemoteChild stamps the ownership labels the projection wrote on a child
// it put on the target cluster, so the sweep selects it exactly as it would
// there.
func ownedRemoteChild(t *testing.T, owner, child client.Object) client.Object {
	t.Helper()
	labels, err := commonmulticluster.OwnerLabels(testScheme(), owner)
	NewGomegaWithT(t).Expect(err).NotTo(HaveOccurred())
	child.SetLabels(labels)
	return child
}

// terminatingRemoteGlance returns a Glance that names a target cluster and is
// being deleted, carrying the remote-children finalizer plus a foreign one so
// the CR survives the release and the test can read back which finalizers the
// pass dropped.
func terminatingRemoteGlance(t *testing.T) (*GlanceReconciler, *glancev1alpha1.Glance) {
	t.Helper()
	g := NewGomegaWithT(t)
	glance := testGlance()
	glance.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "target"}
	glance.Finalizers = []string{commonmulticluster.RemoteChildrenFinalizer, "foreign.example.com/keep-alive"}
	deletedAt := metav1.NewTime(time.Now())
	glance.DeletionTimestamp = &deletedAt

	r := newGlanceTestReconciler(glance)
	var terminating glancev1alpha1.Glance
	g.Expect(r.Get(context.Background(), glanceRequest.NamespacedName, &terminating)).To(Succeed())
	return r, &terminating
}

// TestReconcileDeleteRemoteChildren_SweepsEveryLabelledChild is the whole point
// of the finalizer: no garbage collection cascade crosses the cluster boundary,
// so the objects this CR projected have to be deleted by name from here, across
// every API group they live in. What the sweep leaves standing matters as much:
// a target cluster carries other people's objects, and an object nobody claimed
// or another CR claimed is not ours to remove.
func TestReconcileDeleteRemoteChildren_SweepsEveryLabelledChild(t *testing.T) {
	g := NewGomegaWithT(t)
	r, terminating := terminatingRemoteGlance(t)

	deployment := ownedRemoteChild(t, terminating,
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "test-glance", Namespace: "default"}})
	service := ownedRemoteChild(t, terminating,
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "test-glance", Namespace: "default"}})
	cronJob := ownedRemoteChild(t, terminating,
		&batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: "test-glance-db-purge", Namespace: "default"}})
	grant := ownedRemoteChild(t, terminating,
		&mariadbv1alpha1.Grant{ObjectMeta: metav1.ObjectMeta{Name: "test-glance", Namespace: "default"}})
	unlabelled := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cluster-ca", Namespace: "default"}}
	foreign := ownedRemoteChild(t,
		&glancev1alpha1.Glance{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "default"}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "other-backends", Namespace: "default"}})
	target := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(deployment, service, cronJob, grant, unlabelled, foreign).Build()

	ctx := context.Background()
	err := r.reconcileDeleteRemoteChildren(ctx, commonmulticluster.Remote(target), terminating)

	g.Expect(err).NotTo(HaveOccurred())

	for _, child := range []client.Object{deployment, service, cronJob, grant} {
		err := target.Get(ctx, client.ObjectKeyFromObject(child), child.DeepCopyObject().(client.Object))
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "%T %s must be swept", child, child.GetName())
	}
	g.Expect(target.Get(ctx, client.ObjectKeyFromObject(unlabelled), &corev1.ConfigMap{})).To(Succeed(),
		"an object nobody claimed is nobody's child")
	g.Expect(target.Get(ctx, client.ObjectKeyFromObject(foreign), &corev1.Secret{})).To(Succeed(),
		"another Glance's child must survive this one's teardown")

	updated := getGlance(t, r.Client, "test-glance")
	g.Expect(updated.Finalizers).To(ConsistOf("foreign.example.com/keep-alive"),
		"a completed sweep must release the remote-children finalizer and nothing else")
}

// TestReconcileDeleteRemoteChildren_NilChildrenAbandonsAndReleases covers the
// deregistered target cluster. Its children cannot be reached, so holding the
// finalizer would only strand the CR in Terminating; the objects left running
// are announced rather than silently dropped. Any attempt to sweep through the
// nil client would fault, which is what proves nothing was deleted.
func TestReconcileDeleteRemoteChildren_NilChildrenAbandonsAndReleases(t *testing.T) {
	g := NewGomegaWithT(t)
	r, terminating := terminatingRemoteGlance(t)

	err := r.reconcileDeleteRemoteChildren(context.Background(), nil, terminating)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).
		To(ContainElement(ContainSubstring("RemoteChildrenAbandoned")))
	g.Expect(getGlance(t, r.Client, "test-glance").Finalizers).NotTo(ContainElement(commonmulticluster.RemoteChildrenFinalizer),
		"an unreachable target cluster must not pin the CR forever")
}

// TestReconcileDeleteRemoteChildren_WithoutFinalizerIsANoOp pins the guard a
// local CR relies on. It never carries the finalizer, its children are collected
// from their owner references, and a sweep running anyway would delete objects
// on the management cluster that the cascade already owns.
func TestReconcileDeleteRemoteChildren_WithoutFinalizerIsANoOp(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	r := newGlanceTestReconciler(glance)

	child := ownedRemoteChild(t, glance,
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "test-glance", Namespace: "default"}})
	target := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(child).Build()

	ctx := context.Background()
	err := r.reconcileDeleteRemoteChildren(ctx, commonmulticluster.Remote(target), glance)

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
	r, terminating := terminatingRemoteGlance(t)

	child := ownedRemoteChild(t, terminating,
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "test-glance", Namespace: "default"}})
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
	g.Expect(getGlance(t, r.Client, "test-glance").Finalizers).To(ContainElement(commonmulticluster.RemoteChildrenFinalizer),
		"a failed sweep must keep the finalizer so the next pass retries")
}

// TestReconcile_InstallsRemoteChildrenFinalizerForATargetCluster verifies that a
// CR naming a target cluster is pinned before anything is projected onto it, so
// a deletion issued between this pass and the next still funnels through the
// sweep.
func TestReconcile_InstallsRemoteChildrenFinalizerForATargetCluster(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	glance.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "target"}
	glance.Finalizers = []string{glanceFinalizer} // skip the database finalizer-add requeue
	r := newGlanceTestReconciler(glance)

	res, err := r.Reconcile(context.Background(), glanceRequest)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{RequeueAfter: commonreconcile.RequeueNextPass}),
		"the pass installing the finalizer must requeue before any sub-reconciler runs")
	g.Expect(getGlance(t, r.Client, "test-glance").Finalizers).To(ContainElement(commonmulticluster.RemoteChildrenFinalizer))
}

// TestReconcile_LocalCRNeverCarriesTheRemoteChildrenFinalizer pins the other
// half. A CR that keeps its children on the management cluster has nothing for
// the sweep to do, and a finalizer it does not need is one more thing that can
// block its deletion.
func TestReconcile_LocalCRNeverCarriesTheRemoteChildrenFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance() // no spec.targetClusterRef: children stay local
	glance.Finalizers = []string{glanceFinalizer}
	r := newGlanceTestReconciler(glance)

	_, err := r.Reconcile(context.Background(), glanceRequest)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getGlance(t, r.Client, "test-glance").Finalizers).To(ConsistOf(glanceFinalizer),
		"a local CR must keep the finalizer set it always had")
}
