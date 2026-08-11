// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Unit tests for the Placement reconcile entry point, the finalizer-gated
// deletion path, and the field-index plumbing.
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
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	mcruntime "sigs.k8s.io/multicluster-runtime/pkg/multicluster"

	"github.com/c5c3/forge/internal/common/conditions"
	commonmulticluster "github.com/c5c3/forge/internal/common/multicluster"
	commonreconcile "github.com/c5c3/forge/internal/common/reconcile"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	placementv1alpha1 "github.com/c5c3/forge/operators/placement/api/v1alpha1"
)

func TestReconcile_AddsFinalizerOnFirstPass(t *testing.T) {
	g := NewGomegaWithT(t)
	placement := testPlacement() // no finalizer yet
	r := newPlacementTestReconciler(placement)

	res, err := r.Reconcile(context.Background(), placementRequest)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{Requeue: true}), "the finalizer add requeues so the next pass sees it persisted")
	got := getPlacement(t, r.Client, "test-placement")
	g.Expect(got.Finalizers).To(ContainElement(placementFinalizer))
}

func TestReconcile_FailingSecretsStepShortCircuitsPipeline(t *testing.T) {
	g := NewGomegaWithT(t)
	placement := testPlacement()
	placement.Finalizers = []string{placementFinalizer} // skip the finalizer-add requeue
	// The selected store is explicitly not Ready, so the Secrets step fails fast.
	r := newPlacementTestReconciler(placement, notReadyClusterSecretStore(openBaoClusterStoreName))

	res, err := r.Reconcile(context.Background(), placementRequest)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))

	got := getPlacement(t, r.Client, "test-placement")
	cond := conditions.GetCondition(got.Status.Conditions, "SecretsReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	// A later step must not have run: the config step never rendered, so the
	// ExtraConfigHealthy condition it maintains is absent.
	g.Expect(conditions.GetCondition(got.Status.Conditions, "ExtraConfigHealthy")).To(BeNil())
	g.Expect(got.Status.ObservedGeneration).To(Equal(int64(1)))
}

func TestReconcile_NotFoundCRIsIgnored(t *testing.T) {
	g := NewGomegaWithT(t)
	r := newPlacementTestReconciler() // no Placement seeded

	res, err := r.Reconcile(context.Background(), placementRequest)

	g.Expect(err).NotTo(HaveOccurred(), "a deleted CR is not an error")
	g.Expect(res.IsZero()).To(BeTrue())
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

	placement := testPlacement()
	placement.Finalizers = []string{placementFinalizer}
	placement.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "deregistered"}
	// Terminating for far longer than the window, which by itself must not be
	// enough: a CR blocked in cleanup for minutes is ordinary, and giving up on
	// it the moment the operator comes back would strand its children.
	deletedAt := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	placement.DeletionTimestamp = &deletedAt
	r := newPlacementTestReconciler(placement)
	r.Resolver = unresolvableResolver{}

	res, err := r.Reconcile(context.Background(), placementRequest)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling),
		"the first pass this process fails to resolve on starts the window, it does not end it")
	g.Expect(getPlacement(t, r.Client, "test-placement").Finalizers).To(ContainElement(placementFinalizer))
	time.Sleep(10 * commonmulticluster.AbandonAfter)

	res, err = r.Reconcile(context.Background(), placementRequest)

	g.Expect(err).NotTo(HaveOccurred(),
		"a target cluster that is gone must not fail the deletion pass")
	g.Expect(res.IsZero()).To(BeTrue(), "the deletion resolves once the window is out")

	// With the finalizer released, the fake client garbage-collects the CR.
	var gone placementv1alpha1.Placement
	err = r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-placement"}, &gone)
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
	placement := testPlacement()
	placement.Finalizers = []string{placementFinalizer}
	placement.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "not-engaged-yet"}
	r := newPlacementTestReconciler(placement)
	r.Resolver = unresolvableResolver{}

	// Deleted just now, so the whole abandon window is still ahead.
	g.Expect(r.Delete(context.Background(), placement)).To(Succeed())

	res, err := r.Reconcile(context.Background(), placementRequest)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))

	got := getPlacement(t, r.Client, "test-placement")
	g.Expect(got.Finalizers).To(ContainElement(placementFinalizer),
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
	placement := testPlacement()
	placement.Finalizers = nil
	placement.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "nowhere"}
	r := newPlacementTestReconciler(placement)
	r.Resolver = unresolvableResolver{}

	ctx := context.Background()
	res, err := r.Reconcile(ctx, placementRequest)

	g.Expect(err).NotTo(HaveOccurred(),
		"an unregistered target cluster is a wait, not a reconcile failure")
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))

	got := getPlacement(t, r.Client, "test-placement")
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
	var jobs batchv1.JobList
	g.Expect(r.List(ctx, &jobs)).To(Succeed())
	g.Expect(jobs.Items).To(BeEmpty())
}

func TestReconcileDelete_LiveResourcesRetainFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)
	placement := testPlacement()
	placement.Finalizers = []string{placementFinalizer}
	// A live MariaDB Database owned by this Placement (key is the bare CR name).
	mdb := &mariadbv1alpha1.Database{}
	mdb.Name = "test-placement"
	mdb.Namespace = "default"
	r := newPlacementTestReconciler(placement, mdb)

	// Move the Placement into the deleting state.
	g.Expect(r.Delete(context.Background(), placement)).To(Succeed())

	res, err := r.Reconcile(context.Background(), placementRequest)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueDatabaseWait))
	got := getPlacement(t, r.Client, "test-placement")
	g.Expect(got.Finalizers).To(ContainElement(placementFinalizer),
		"the finalizer is retained one pass while live MariaDB resources remain")
}

func TestReconcileDelete_NoLiveResourcesReleasesFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)
	placement := testPlacement()
	placement.Finalizers = []string{placementFinalizer}
	r := newPlacementTestReconciler(placement) // no MariaDB CRs

	g.Expect(r.Delete(context.Background(), placement)).To(Succeed())

	res, err := r.Reconcile(context.Background(), placementRequest)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	// With the finalizer released, the fake client garbage-collects the CR.
	var gone placementv1alpha1.Placement
	err = r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-placement"}, &gone)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
		"the finalizer is released when no live MariaDB resource remains")
}

func TestReconcileDelete_WithoutFinalizerIsNoOp(t *testing.T) {
	g := NewGomegaWithT(t)
	placement := testPlacement()
	placement.DeletionTimestamp = &metav1.Time{Time: metav1.Now().Time}
	r := newPlacementTestReconciler()

	res, err := r.reconcileDelete(context.Background(), r.Client, placement)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).To(BeEmpty(),
		"a CR without the finalizer emits no cleanup events")
}

func TestSetReadyCondition_TrueOnlyWhenAllSubConditionsTrue(t *testing.T) {
	g := NewGomegaWithT(t)
	placement := testPlacement()

	// Every sub-condition True → aggregate Ready True.
	for _, ct := range subConditionTypes {
		conditions.SetCondition(&placement.Status.Conditions, metav1.Condition{
			Type:   ct,
			Status: metav1.ConditionTrue,
			Reason: "OK",
		})
	}
	setReadyCondition(placement)
	ready := conditions.GetCondition(placement.Status.Conditions, "Ready")
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))

	// Flip one sub-condition False → aggregate Ready flips False.
	conditions.SetCondition(&placement.Status.Conditions, metav1.Condition{
		Type:   "HPAReady",
		Status: metav1.ConditionFalse,
		Reason: "Degraded",
	})
	setReadyCondition(placement)
	ready = conditions.GetCondition(placement.Status.Conditions, "Ready")
	g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
}

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

func TestPlacementSecretNameExtractor(t *testing.T) {
	g := NewGomegaWithT(t)

	// serviceUser + database Secret names, deduplicated.
	placement := testPlacement()
	g.Expect(placementSecretNameExtractor(placement)).To(ConsistOf("placement-service-user", "placement-db"))

	// The same Secret backing both references collapses to one entry.
	placement.Spec.Database.SecretRef.Name = "placement-service-user"
	g.Expect(placementSecretNameExtractor(placement)).To(ConsistOf("placement-service-user"))

	// An empty database Secret name is skipped.
	placement.Spec.Database.SecretRef.Name = ""
	g.Expect(placementSecretNameExtractor(placement)).To(ConsistOf("placement-service-user"))

	// The wrong object type yields nil rather than a panic.
	g.Expect(placementSecretNameExtractor(&corev1.Secret{})).To(BeNil())
}

func TestRegisterPlacementIndexes_RegistersSecretNameKey(t *testing.T) {
	g := NewGomegaWithT(t)

	idx := &recordingFieldIndexer{}
	g.Expect(registerPlacementIndexes(context.Background(), idx)).To(Succeed())
	g.Expect(idx.keys).To(ConsistOf(PlacementSecretNameIndexKey))
}
