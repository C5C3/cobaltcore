// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Unit tests for the Nova reconcile pipeline: the finalizer contract, the
// target-cluster gates, the teardown of both schemas and of the children a
// placed CR projected, and the Ready aggregation the sub-conditions feed.
package controller

import (
	"context"
	"slices"
	"testing"
	"time"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	mcruntime "sigs.k8s.io/multicluster-runtime/pkg/multicluster"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	"github.com/c5c3/cobaltcore/internal/common/database"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
	"github.com/c5c3/cobaltcore/operators/nova/internal/metrics"
)

// getNova re-reads the Nova CR from the given client.
func getNova(t *testing.T, c client.Client, name string) *novav1alpha1.Nova {
	t.Helper()
	var nova novav1alpha1.Nova
	if err := c.Get(context.Background(), objectKey(name), &nova); err != nil {
		t.Fatalf("re-reading Nova %s: %v", name, err)
	}
	return &nova
}

// --- the finalizer contract -------------------------------------------------

func TestReconcile_AddsFinalizerOnFirstPass(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova() // no finalizer yet
	r := newNovaTestReconciler(nova)

	res, err := r.Reconcile(context.Background(), novaRequest)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{RequeueAfter: commonreconcile.RequeueNextPass}),
		"the finalizer add requeues so the next pass sees it persisted")
	g.Expect(getNova(t, r.Client, testNovaName).Finalizers).To(ContainElement(novaFinalizer))
}

// TestReconcile_MissingCRIsDone covers the ordinary race between a delete and
// the queued event that still names the CR: nothing is left to reconcile, so the
// pass must end quietly instead of erroring the item back onto the queue.
func TestReconcile_MissingCRIsDone(t *testing.T) {
	g := NewGomegaWithT(t)
	r := newNovaTestReconciler()

	res, err := r.Reconcile(context.Background(), novaRequest)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
}

// TestReconcile_FailingSecretsStepShortCircuitsPipeline pins the short-circuit:
// the first step to return a non-zero result ends the pass, so no later step
// gets to write a condition that would read as progress. The status that pass
// persists still names the generation it describes: a client waiting for its
// spec change to be observed reads status.observedGeneration, and one left at
// an older generation would keep it waiting on a CR the operator has long since
// reported on.
func TestReconcile_FailingSecretsStepShortCircuitsPipeline(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	nova.Generation = 3
	nova.Finalizers = []string{novaFinalizer} // skip the finalizer-add requeue
	// The selected store is explicitly not Ready, so the Secrets step fails fast.
	r := newNovaTestReconciler(nova, notReadyClusterSecretStore(openBaoClusterStoreName))

	res, err := r.Reconcile(context.Background(), novaRequest)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))

	got := getNova(t, r.Client, testNovaName)
	secretsReady := novaCondition(got, "SecretsReady")
	g.Expect(secretsReady).NotTo(BeNil())
	g.Expect(secretsReady.Status).To(Equal(metav1.ConditionFalse))
	// No later step ran: neither the compute contract nor the database step ever
	// reported.
	g.Expect(novaCondition(got, conditionTypeComputeConfigReady)).To(BeNil())
	g.Expect(novaCondition(got, "DatabaseReady")).To(BeNil())
	g.Expect(got.Status.ObservedGeneration).To(Equal(int64(3)),
		"the persisted status must be stamped with the generation this pass reconciled")
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
	nova := validNova()
	nova.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "nowhere"}
	r := newNovaTestReconciler(nova)
	r.Resolver = unresolvableResolver{}

	ctx := context.Background()
	res, err := r.Reconcile(ctx, novaRequest)

	g.Expect(err).NotTo(HaveOccurred(),
		"an unregistered target cluster is a wait, not a reconcile failure")
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))

	got := getNova(t, r.Client, testNovaName)
	cond := novaCondition(got, "SecretsReady")
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

	nova := validNova()
	nova.Finalizers = []string{novaFinalizer}
	nova.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "deregistered"}
	// Terminating for far longer than the window, which by itself must not be
	// enough: a CR blocked in cleanup for minutes is ordinary, and giving up on it
	// the moment the operator comes back would strand its children.
	deletedAt := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	nova.DeletionTimestamp = &deletedAt
	r := newNovaTestReconciler(nova)
	r.Resolver = unresolvableResolver{}

	res, err := r.Reconcile(context.Background(), novaRequest)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling),
		"the first pass this process fails to resolve on starts the window, it does not end it")
	g.Expect(getNova(t, r.Client, testNovaName).Finalizers).To(ContainElement(novaFinalizer))
	time.Sleep(10 * commonmulticluster.AbandonAfter)

	res, err = r.Reconcile(context.Background(), novaRequest)

	g.Expect(err).NotTo(HaveOccurred(),
		"a target cluster that is gone must not fail the deletion pass")
	g.Expect(res.IsZero()).To(BeTrue(), "the deletion resolves once the window is out")

	// With the finalizer released, the fake client garbage-collects the CR.
	var gone novav1alpha1.Nova
	err = r.Get(context.Background(), novaRequest.NamespacedName, &gone)
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
	nova := validNova()
	nova.Finalizers = []string{novaFinalizer}
	nova.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "not-engaged-yet"}
	r := newNovaTestReconciler(nova)
	r.Resolver = unresolvableResolver{}

	// Deleted just now, so the whole abandon window is still ahead.
	g.Expect(r.Delete(context.Background(), nova)).To(Succeed())

	res, err := r.Reconcile(context.Background(), novaRequest)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))

	got := getNova(t, r.Client, testNovaName)
	g.Expect(got.Finalizers).To(ContainElement(novaFinalizer),
		"a target that may still be engaging must not cost the CR its finalizer")
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).
		NotTo(ContainElement(ContainSubstring("RemoteChildrenAbandoned")))

	// The hold lasts minutes, so it has to be readable off the CR: without it, a
	// namespace stuck Terminating looks like a wedged finalizer and can only be
	// told apart by correlating operator logs across replicas.
	cond := novaCondition(got, "SecretsReady")
	g.Expect(cond).NotTo(BeNil(), "the deliberate hold must be visible on the CR")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(commonmulticluster.TargetClusterUnavailable))
	g.Expect(cond.Message).To(ContainSubstring("not-engaged-yet"))
}

func TestReconcile_InstallsRemoteChildrenFinalizerForATargetCluster(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	nova.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "target"}
	nova.Finalizers = []string{novaFinalizer} // skip the database finalizer-add requeue
	r := newNovaTestReconciler(nova)

	res, err := r.Reconcile(context.Background(), novaRequest)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{RequeueAfter: commonreconcile.RequeueNextPass}),
		"the pass installing the finalizer must requeue before any sub-reconciler runs")
	g.Expect(getNova(t, r.Client, testNovaName).Finalizers).
		To(ContainElement(commonmulticluster.RemoteChildrenFinalizer))
}

// TestReconcile_LocalCRNeverCarriesTheRemoteChildrenFinalizer pins the other
// half. A CR that keeps its children on the management cluster has nothing for
// the sweep to do, and a finalizer it does not need is one more thing that can
// block its deletion.
func TestReconcile_LocalCRNeverCarriesTheRemoteChildrenFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova() // no spec.targetClusterRef: children stay local
	nova.Finalizers = []string{novaFinalizer}
	r := newNovaTestReconciler(nova)

	_, err := r.Reconcile(context.Background(), novaRequest)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getNova(t, r.Client, testNovaName).Finalizers).To(ConsistOf(novaFinalizer),
		"a local CR must keep the finalizer set it always had")
}

// --- reconcileDelete --------------------------------------------------------

// cell0ResourceName is the object name the cell0 Database and Grant carry: an
// additional schema of the cell block is named after its block instance and its
// SQL schema, so it never collides with the cell schema's own CRs.
func cell0ResourceName(nova *novav1alpha1.Nova) string {
	return database.AdditionalResourceName(nova.Name, cell0Schema(nova))
}

// novaDatabaseCRs returns the eight MariaDB CRs one Nova owns, each as a
// constructor so a test can seed exactly one of them: the Database, User and
// Grant of the nova_api block, the same three of the cell block, and the
// Database and Grant of cell0, which shares the cell block's User.
func novaDatabaseCRs(nova *novav1alpha1.Nova) map[string]func() client.Object {
	apiName := apiInstanceName(nova)
	cellName := nova.Name
	cell0Name := cell0ResourceName(nova)

	named := func(obj client.Object, name string) func() client.Object {
		return func() client.Object {
			obj.SetName(name)
			obj.SetNamespace(nova.Namespace)
			return obj
		}
	}
	return map[string]func() client.Object{
		"api Database":   named(&mariadbv1alpha1.Database{}, apiName),
		"api User":       named(&mariadbv1alpha1.User{}, apiName),
		"api Grant":      named(&mariadbv1alpha1.Grant{}, apiName),
		"cell Database":  named(&mariadbv1alpha1.Database{}, cellName),
		"cell User":      named(&mariadbv1alpha1.User{}, cellName),
		"cell Grant":     named(&mariadbv1alpha1.Grant{}, cellName),
		"cell0 Database": named(&mariadbv1alpha1.Database{}, cell0Name),
		"cell0 Grant":    named(&mariadbv1alpha1.Grant{}, cell0Name),
	}
}

// TestReconcileDelete_LiveResourcesRetainFinalizer walks every MariaDB CR a Nova
// owns one at a time. Each of them alone has to hold the CR for another pass,
// which is what proves both schema keys are probed: cell0 in particular is an
// additional schema of the cell block rather than a key of its own, so a
// teardown that only probed the two block names would drop its Database and
// Grant on the floor.
func TestReconcileDelete_LiveResourcesRetainFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)
	g.Expect(cell0ResourceName(validNova())).To(Equal("nova-nova-cell0"),
		"the cell0 CRs are named after the cell instance and the cell0 schema")

	for label, ctor := range novaDatabaseCRs(validNova()) {
		t.Run(label, func(t *testing.T) {
			g := NewGomegaWithT(t)
			nova := validNova()
			nova.Finalizers = []string{novaFinalizer}
			r := newNovaTestReconciler(nova, ctor())

			// Move the Nova into the deleting state.
			g.Expect(r.Delete(context.Background(), nova)).To(Succeed())

			res, err := r.Reconcile(context.Background(), novaRequest)

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(res.RequeueAfter).To(Equal(RequeueDatabaseWait))
			g.Expect(getNova(t, r.Client, testNovaName).Finalizers).To(ContainElement(novaFinalizer),
				"the finalizer is retained one pass while a live MariaDB resource remains")
			g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).
				To(ContainElement(ContainSubstring("FinalizingDatabase")))
		})
	}
}

// TestReconcileDelete_DeletesBothSchemasAndCell0 is the other side of the
// teardown: every CR of both blocks, cell0 included, has to be issued a Delete
// in the one pass that observes them live.
func TestReconcileDelete_DeletesBothSchemasAndCell0(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	nova.Finalizers = []string{novaFinalizer}

	ctors := novaDatabaseCRs(nova)
	seeded := make([]client.Object, 0, len(ctors))
	for _, ctor := range ctors {
		seeded = append(seeded, ctor())
	}
	r := newNovaTestReconciler(append(seeded, nova)...)
	g.Expect(r.Delete(context.Background(), nova)).To(Succeed())

	res, err := r.Reconcile(context.Background(), novaRequest)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueDatabaseWait))
	for label, ctor := range novaDatabaseCRs(nova) {
		obj := ctor()
		err := r.Get(context.Background(), client.ObjectKeyFromObject(obj), obj)
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the %s must be deleted", label)
	}
}

// TestReconcileDelete_NoLiveResourcesReleasesFinalizerAndDropsMetrics covers the
// terminal pass: the CR leaves etcd and its per-CR series go with it, so a Nova
// recreated under the same name does not inherit the counters of the one before
// it.
func TestReconcileDelete_NoLiveResourcesReleasesFinalizerAndDropsMetrics(t *testing.T) {
	g := NewGomegaWithT(t)
	g.Expect(metrics.Register()).To(Succeed())

	nova := validNova()
	nova.Name = "delete-drops-metrics"
	nova.Finalizers = []string{novaFinalizer}
	t.Cleanup(func() { metrics.DeleteForNova(nova.Name, nova.Namespace) })
	metrics.RecordDBArchive(nova.Name, nova.Namespace, "failed", time.Second)
	g.Expect(dbArchiveCount(t, nova, "failed")).To(Equal(1.0), "the fixture series must exist to be dropped")

	r := newNovaTestReconciler(nova) // no MariaDB CRs
	g.Expect(r.Delete(context.Background(), nova)).To(Succeed())

	res, err := r.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: objectKey(nova.Name)})

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	// With the finalizer released, the fake client garbage-collects the CR.
	var gone novav1alpha1.Nova
	err = r.Get(context.Background(), objectKey(nova.Name), &gone)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
		"the finalizer is released when no live MariaDB resource remains")
	g.Expect(dbArchiveCount(t, nova, "failed")).To(BeZero(),
		"the per-CR series must not outlive the CR they are labelled with")
}

// TestReconcileDelete_WithoutTheFinalizerIsANoOp guards the entry condition: a CR
// this operator never claimed must not have its database CRs deleted out from
// under whoever does own them.
func TestReconcileDelete_WithoutTheFinalizerIsANoOp(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova() // no finalizer
	mdb := &mariadbv1alpha1.Database{}
	mdb.Name = testNovaName
	mdb.Namespace = testNamespace
	r := newNovaTestReconciler(nova, mdb)

	ctx := context.Background()
	res, err := r.reconcileDelete(ctx, r.Client, nova)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(r.Get(ctx, objectKey(testNovaName), &mariadbv1alpha1.Database{})).To(Succeed(),
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

// terminatingRemoteNova returns a Nova that names a target cluster and is being
// deleted, carrying the remote-children finalizer plus a foreign one so the CR
// survives the release and the test can read back which finalizers the pass
// dropped.
func terminatingRemoteNova(t *testing.T) (*NovaReconciler, *novav1alpha1.Nova) {
	t.Helper()
	g := NewGomegaWithT(t)
	nova := validNova()
	nova.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "target"}
	nova.Finalizers = []string{commonmulticluster.RemoteChildrenFinalizer, "foreign.example.com/keep-alive"}
	deletedAt := metav1.NewTime(time.Now())
	nova.DeletionTimestamp = &deletedAt

	r := newNovaTestReconciler(nova)
	var terminating novav1alpha1.Nova
	g.Expect(r.Get(context.Background(), novaRequest.NamespacedName, &terminating)).To(Succeed())
	return r, &terminating
}

// TestReconcileDeleteRemoteChildren_SweepsEveryLabelledChild is the whole point
// of the finalizer: no garbage collection cascade crosses the cluster boundary,
// so the objects this CR projected have to be deleted by name from here, across
// every API group they live in. One child per kind in NovaRemoteChildKinds is
// seeded, the HPA and the three routes included, because a kind dropped from
// that list leaves its objects running on the target with nothing to report
// it; a kind added to the list without a child here fails the kind check. What
// the sweep leaves standing matters as much: a target cluster carries other
// people's objects, and an object nobody claimed or another CR claimed is not
// ours to remove.
func TestReconcileDeleteRemoteChildren_SweepsEveryLabelledChild(t *testing.T) {
	g := NewGomegaWithT(t)
	r, terminating := terminatingRemoteNova(t)

	owned := func(child client.Object, name string) client.Object {
		child.SetName(name)
		child.SetNamespace(testNamespace)
		return ownedRemoteChild(t, terminating, child)
	}
	projected := []client.Object{
		owned(&appsv1.Deployment{}, testNovaName+"-conductor"),
		owned(&corev1.Service{}, testNovaName),
		owned(&corev1.ConfigMap{}, testNovaName+"-config-abc123"),
		owned(&corev1.Secret{}, testNovaName+"-transport-url"),
		owned(&batchv1.Job{}, testNovaName+"-db-sync"),
		owned(&batchv1.CronJob{}, testNovaName+"-db-archive"),
		owned(&policyv1.PodDisruptionBudget{}, testNovaName),
		owned(&autoscalingv2.HorizontalPodAutoscaler{}, testNovaName),
		owned(&networkingv1.NetworkPolicy{}, testNovaName),
		// The API, the metadata API and the console proxy each carry a route.
		owned(&gatewayv1.HTTPRoute{}, testNovaName),
		owned(&gatewayv1.HTTPRoute{}, testNovaName+"-metadata"),
		owned(&gatewayv1.HTTPRoute{}, testNovaName+"-console"),
		owned(&mariadbv1alpha1.Database{}, testNovaName),
		owned(&mariadbv1alpha1.User{}, testNovaName),
		owned(&mariadbv1alpha1.Grant{}, testNovaName),
	}
	var seededKinds []schema.GroupVersionKind
	for _, child := range projected {
		gvk, err := apiutil.GVKForObject(child, testScheme())
		g.Expect(err).NotTo(HaveOccurred())
		if !slices.Contains(seededKinds, gvk) {
			seededKinds = append(seededKinds, gvk)
		}
	}
	g.Expect(seededKinds).To(ConsistOf(NovaRemoteChildKinds),
		"every kind the sweep covers needs a child here, or the list grew untested")

	unlabelled := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cluster-ca", Namespace: testNamespace}}
	foreign := ownedRemoteChild(t,
		&novav1alpha1.Nova{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: testNamespace}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "other-compute-config", Namespace: testNamespace}})
	target := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(append([]client.Object{unlabelled, foreign}, projected...)...).Build()

	ctx := context.Background()
	err := r.reconcileDeleteRemoteChildren(ctx, commonmulticluster.Remote(target), terminating)

	g.Expect(err).NotTo(HaveOccurred())

	for _, child := range projected {
		err := target.Get(ctx, client.ObjectKeyFromObject(child), child.DeepCopyObject().(client.Object))
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "%T %s must be swept", child, child.GetName())
	}
	g.Expect(target.Get(ctx, client.ObjectKeyFromObject(unlabelled), &corev1.ConfigMap{})).To(Succeed(),
		"an object nobody claimed is nobody's child")
	g.Expect(target.Get(ctx, client.ObjectKeyFromObject(foreign), &corev1.Secret{})).To(Succeed(),
		"another Nova's child must survive this one's teardown")

	g.Expect(getNova(t, r.Client, testNovaName).Finalizers).To(ConsistOf("foreign.example.com/keep-alive"),
		"a completed sweep must release the remote-children finalizer and nothing else")
}

// TestReconcileDeleteRemoteChildren_NilChildrenAbandonsAndReleases covers the
// deregistered target cluster. Its children cannot be reached, so holding the
// finalizer would only strand the CR in Terminating; the objects left running are
// announced rather than silently dropped. Any attempt to sweep through the nil
// client would fault, which is what proves nothing was deleted.
func TestReconcileDeleteRemoteChildren_NilChildrenAbandonsAndReleases(t *testing.T) {
	g := NewGomegaWithT(t)
	r, terminating := terminatingRemoteNova(t)

	err := r.reconcileDeleteRemoteChildren(context.Background(), nil, terminating)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).
		To(ContainElement(ContainSubstring("RemoteChildrenAbandoned")))
	g.Expect(getNova(t, r.Client, testNovaName).Finalizers).
		NotTo(ContainElement(commonmulticluster.RemoteChildrenFinalizer),
			"an unreachable target cluster must not pin the CR forever")
}

// TestReconcileDeleteRemoteChildren_WithoutFinalizerIsANoOp pins the guard a
// local CR relies on. It never carries the finalizer, its children are collected
// from their owner references, and a sweep running anyway would delete objects on
// the management cluster that the cascade already owns.
func TestReconcileDeleteRemoteChildren_WithoutFinalizerIsANoOp(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	r := newNovaTestReconciler(nova)

	child := ownedRemoteChild(t, nova,
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: testNovaName, Namespace: testNamespace}})
	target := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(child).Build()

	ctx := context.Background()
	err := r.reconcileDeleteRemoteChildren(ctx, commonmulticluster.Remote(target), nova)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(target.Get(ctx, client.ObjectKeyFromObject(child), &appsv1.Deployment{})).To(Succeed(),
		"a CR without the finalizer must not sweep anything")
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).To(BeEmpty())
}

// TestReconcileDeleteRemoteChildren_SweepFailureKeepsFinalizer is the guard
// against a CR that leaves etcd while its children keep running. A list the
// target cluster refuses says nothing about whether children exist, so the pass
// has to fail and sweep again on the next one: released here, the finalizer
// would let the CR go while its conductor and scheduler keep running on the
// target with nothing left to delete them.
func TestReconcileDeleteRemoteChildren_SweepFailureKeepsFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)
	r, terminating := terminatingRemoteNova(t)

	child := ownedRemoteChild(t, terminating,
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: testNovaName + "-conductor", Namespace: testNamespace}})
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
	g.Expect(getNova(t, r.Client, testNovaName).Finalizers).
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

// Nova indexes one field: the Secret-name union its Secret watch resolves
// through. A helper that registered nothing would leave that mapper listing
// every Nova in the namespace on every Secret event.
func TestRegisterNovaIndexes_RegistersOne(t *testing.T) {
	g := NewGomegaWithT(t)

	idx := &recordingFieldIndexer{}
	g.Expect(registerNovaIndexes(context.Background(), idx)).To(Succeed())

	g.Expect(idx.keys).To(ConsistOf(NovaSecretNameIndexKey))
}

// TestSubConditionTypes_PinsTheAggregatedVocabulary keeps the Ready contract
// deliberate: every entry is a condition some sub-reconciler sets, and
// ExtraConfigHealthy stays out because a user-owned overlay must not depool an
// API that serves.
func TestSubConditionTypes_PinsTheAggregatedVocabulary(t *testing.T) {
	g := NewGomegaWithT(t)

	g.Expect(subConditionTypes).To(ConsistOf(
		"SecretsReady",
		"ComputeConfigReady",
		"DatabaseReady",
		"ConductorReady",
		"SchedulerReady",
		"MetadataReady",
		"ConsoleProxyReady",
		"DeploymentReady",
		"DBArchiveReady",
		"NovaAPIReady",
		"HPAReady",
		"NetworkPolicyReady",
		"HTTPRouteReady",
		"MetadataHTTPRouteReady",
		"ConsoleHTTPRouteReady",
	))
	g.Expect(subConditionTypes).NotTo(ContainElement("ExtraConfigHealthy"),
		"the extraConfig overlay is informational and must not gate Ready")
}

func TestSetReadyCondition_TrueOnlyWhenAllSubConditionsTrue(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()

	// Every sub-condition True -> aggregate Ready True.
	for _, ct := range subConditionTypes {
		conditions.SetCondition(&nova.Status.Conditions, metav1.Condition{
			Type:   ct,
			Status: metav1.ConditionTrue,
			Reason: "OK",
		})
	}
	setReadyCondition(nova)
	ready := conditions.GetCondition(nova.Status.Conditions, "Ready")
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))

	// Flip one sub-condition False -> aggregate Ready flips False.
	conditions.SetCondition(&nova.Status.Conditions, metav1.Condition{
		Type:   "ConsoleProxyReady",
		Status: metav1.ConditionFalse,
		Reason: "Degraded",
	})
	setReadyCondition(nova)
	ready = conditions.GetCondition(nova.Status.Conditions, "Ready")
	g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
}

// --- the broker port the pipeline hands the network policy ------------------

// networkPolicyMember returns the NetworkPolicy member of the parallel group.
func networkPolicyMember(t *testing.T, subs []commonreconcile.ParallelStep[*novav1alpha1.Nova],
) commonreconcile.ParallelStep[*novav1alpha1.Nova] {
	t.Helper()
	for _, sub := range subs {
		if sub.Name == "NetworkPolicy" {
			return sub
		}
	}
	t.Fatal("the parallel group carries no NetworkPolicy member")
	return commonreconcile.ParallelStep[*novav1alpha1.Nova]{}
}

// TestParallelSteps_NetworkPolicyOpensTheBrokerPort is the end of the thread the
// messaging step starts. No selector reaches the broker, so the port it
// resolved is the only thing the bus rule is built from: a member that lost it
// on the way would render a policy without that rule, and the scheduler, the
// conductor and the API would lose the bus every compute node listens on. The
// port differs from the fixture's 5672 so a default standing in for the
// resolved one does not pass.
func TestParallelSteps_NetworkPolicyOpensTheBrokerPort(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := hardenedNova()
	r := newNetworkPolicyTestReconciler(nova)

	member := networkPolicyMember(t, r.parallelSteps(r.Client, &pipelineState{egressPort: 5671}))

	res, err := member.Fn(context.Background(), nova)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	var np networkingv1.NetworkPolicy
	g.Expect(r.Get(context.Background(), objectKey(testNovaName), &np)).To(Succeed())
	g.Expect(egressPorts(&np)).To(ContainElement(5671),
		"the broker port the messaging step resolved must open the bus rule")
}
