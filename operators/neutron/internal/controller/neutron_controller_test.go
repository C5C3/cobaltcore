// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Unit tests for the Neutron reconcile entry point, the finalizer-gated
// deletion path, the remote-children sweep, and the field-index plumbing.
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
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
)

// --- field indexes ---------------------------------------------------------

func TestNeutronSecretNameExtractor(t *testing.T) {
	withMessagingSecret := func(name string) client.Object {
		neutron := validNeutron()
		neutron.Spec.Messaging.ClusterRef = nil
		neutron.Spec.Messaging.SecretRef = &commonv1.SecretRefSpec{Name: name, Key: "transport_url"}
		return neutron
	}

	tests := []struct {
		name  string
		obj   client.Object
		want  []string
		empty bool
	}{
		{
			name: "the two mandatory references are indexed",
			obj:  validNeutron(),
			want: []string{"neutron-db", "neutron-service-user"},
		},
		{
			name: "a brownfield transport URL Secret is indexed too",
			obj:  withMessagingSecret("neutron-transport"),
			want: []string{"neutron-db", "neutron-service-user", "neutron-transport"},
		},
		{
			name: "one Secret serving two references is indexed once",
			obj:  withMessagingSecret("neutron-db"),
			want: []string{"neutron-db", "neutron-service-user"},
		},
		{
			name: "an empty reference is skipped",
			obj: func() client.Object {
				neutron := validNeutron()
				neutron.Spec.ServiceUser.SecretRef.Name = ""
				return neutron
			}(),
			want: []string{"neutron-db"},
		},
		{
			name:  "a wrong-type object indexes under nothing",
			obj:   readyOVNCentral(testOVNCentralName, testNamespace, testNorthboundAddress, testSouthboundAddress, "ovn-client"),
			empty: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			got := neutronSecretNameExtractor(tc.obj)
			if tc.empty {
				g.Expect(got).To(BeEmpty())
				return
			}
			g.Expect(got).To(Equal(tc.want))
		})
	}
}

func TestNeutronOVNCentralRefExtractor(t *testing.T) {
	tests := []struct {
		name  string
		obj   client.Object
		want  []string
		empty bool
	}{
		{
			name: "an explicit namespace is indexed as given",
			obj:  validNeutron(),
			want: []string{testNamespace + "/" + testOVNCentralName},
		},
		{
			name: "an omitted namespace falls back to the CR's own",
			obj: func() client.Object {
				neutron := validNeutron()
				neutron.Namespace = "tenant"
				neutron.Spec.OVN.CentralRef.Namespace = ""
				return neutron
			}(),
			want: []string{"tenant/" + testOVNCentralName},
		},
		{
			name: "a reference without a name indexes under nothing",
			obj: func() client.Object {
				neutron := validNeutron()
				neutron.Spec.OVN.CentralRef.Name = ""
				return neutron
			}(),
			empty: true,
		},
		{
			name:  "a wrong-type object indexes under nothing",
			obj:   readyOVNCentral(testOVNCentralName, testNamespace, testNorthboundAddress, testSouthboundAddress, "ovn-client"),
			empty: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			got := neutronOVNCentralRefExtractor(tc.obj)
			if tc.empty {
				g.Expect(got).To(BeEmpty())
				return
			}
			g.Expect(got).To(Equal(tc.want))
		})
	}
}

// TestSecretNameIndexResolvesNeutron covers the index as the watch mapper will
// use it: a Secret name resolves to the referencing CR through the field
// selector rather than an unfiltered namespace List.
func TestSecretNameIndexResolvesNeutron(t *testing.T) {
	g := NewGomegaWithT(t)
	other := validNeutron()
	other.Name = "other-neutron"
	other.Spec.Database.SecretRef = commonv1.SecretRefSpec{Name: "other-db"}
	other.Spec.ServiceUser.SecretRef = commonv1.SecretRefSpec{Name: "other-service-user"}
	c := neutronFakeClientBuilder(validNeutron(), other).Build()

	var list neutronv1alpha1.NeutronList
	g.Expect(c.List(context.Background(), &list,
		client.InNamespace(testNamespace),
		client.MatchingFields{NeutronSecretNameIndexKey: "neutron-db"})).To(Succeed())

	g.Expect(list.Items).To(HaveLen(1))
	g.Expect(list.Items[0].Name).To(Equal(testNeutronName))
}

// TestOVNCentralRefIndexResolvesNeutronAcrossNamespaces pins the index the
// OVNCentral watch reads: the OVN control plane commonly lives in another
// namespace than the Neutron driving it, so the lookup is cluster-wide and keyed
// on the qualified reference rather than the bare name.
func TestOVNCentralRefIndexResolvesNeutronAcrossNamespaces(t *testing.T) {
	g := NewGomegaWithT(t)
	remote := validNeutron()
	remote.Namespace = "tenant"
	remote.Spec.OVN.CentralRef = neutronv1alpha1.OVNCentralRef{Name: testOVNCentralName, Namespace: "ovn-system"}
	// A same-named central in the Neutron's own namespace must not match: only
	// the qualified value does.
	local := validNeutron()
	local.Name = "local-neutron"
	c := neutronFakeClientBuilder(remote, local).Build()

	var list neutronv1alpha1.NeutronList
	g.Expect(c.List(context.Background(), &list,
		client.MatchingFields{NeutronOVNCentralRefIndexKey: "ovn-system/" + testOVNCentralName})).To(Succeed())

	g.Expect(list.Items).To(HaveLen(1))
	g.Expect(list.Items[0].Namespace).To(Equal("tenant"))
}

// --- the reconcile entry point ---------------------------------------------

func TestReconcile_AddsFinalizerOnFirstPass(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron() // no finalizer yet
	r := newNeutronTestReconciler(neutron)

	res, err := r.Reconcile(context.Background(), neutronRequest)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{RequeueAfter: commonreconcile.RequeueNextPass}),
		"the finalizer add requeues so the next pass sees it persisted")
	g.Expect(getNeutron(t, r.Client).Finalizers).To(ContainElement(neutronFinalizer))
}

func TestReconcile_NotFoundCRIsIgnored(t *testing.T) {
	g := NewGomegaWithT(t)
	r := newNeutronTestReconciler() // no Neutron seeded

	res, err := r.Reconcile(context.Background(), neutronRequest)

	g.Expect(err).NotTo(HaveOccurred(), "a deleted CR is not an error")
	g.Expect(res.IsZero()).To(BeTrue())
}

// TestReconcile_FailingSecretsStepShortCircuitsPipeline pins the pipeline's
// short-circuit: the first step to report a non-zero result ends the pass, so no
// later step can render or project against credentials that are not there.
func TestReconcile_FailingSecretsStepShortCircuitsPipeline(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()
	neutron.Finalizers = []string{neutronFinalizer} // skip the finalizer-add requeue
	// The selected store is explicitly not Ready, so the Secrets step fails fast.
	r := newNeutronTestReconciler(neutron, notReadyClusterSecretStore(openBaoClusterStoreName))

	ctx := context.Background()
	res, err := r.Reconcile(ctx, neutronRequest)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))

	got := getNeutron(t, r.Client)
	cond := neutronCondition(got, "SecretsReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	// A later step must not have run: the OVN endpoint step sets its condition on
	// every path it takes, so its absence is what proves the chain stopped.
	g.Expect(neutronCondition(got, conditionTypeOVNEndpointsReady)).To(BeNil())
	g.Expect(got.Status.ObservedGeneration).To(Equal(int64(1)))

	var deployments appsv1.DeploymentList
	g.Expect(r.List(ctx, &deployments)).To(Succeed())
	g.Expect(deployments.Items).To(BeEmpty(), "no workload is projected behind a failed credential gate")
}

// TestReconcile_TargetClusterUnavailableGatesBeforeFinalizer pins the failure
// surface of an unresolvable spec.targetClusterRef: the CR reports
// SecretsReady=False with the shared reason, the pass requeues instead of
// erroring, and the CR is left with neither a finalizer nor a single child
// object, because the resolution runs ahead of the finalizer-add.
func TestReconcile_TargetClusterUnavailableGatesBeforeFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()
	neutron.Finalizers = nil
	neutron.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "nowhere"}
	r := newNeutronTestReconciler(neutron)
	r.Resolver = unresolvableResolver{}

	ctx := context.Background()
	res, err := r.Reconcile(ctx, neutronRequest)

	g.Expect(err).NotTo(HaveOccurred(),
		"an unregistered target cluster is a wait, not a reconcile failure")
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))

	got := getNeutron(t, r.Client)
	cond := neutronCondition(got, "SecretsReady")
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
}

// TestReconcile_TerminatingCR_UnresolvableTargetReleasesFinalizer is the guard
// against a CR that can never be deleted. Deregistering a target cluster is a
// documented operation, and a CR that already provisioned on it carries the
// finalizer by then. While the resolution ran ahead of the deletion branch, every
// pass short-circuited on "cluster not found" before reconcileDelete, the
// finalizer was never released, and the CR, with its namespace, stayed
// Terminating until someone stripped it by hand.
func TestReconcile_TerminatingCR_UnresolvableTargetReleasesFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)
	// The window this process has to sit out starts on its own first failure to
	// resolve, so it has to be compressed for the second pass to reach past it.
	abandonAfter := commonmulticluster.AbandonAfter
	t.Cleanup(func() { commonmulticluster.AbandonAfter = abandonAfter })
	commonmulticluster.AbandonAfter = time.Millisecond

	neutron := validNeutron()
	neutron.Finalizers = []string{neutronFinalizer}
	neutron.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "deregistered"}
	// Terminating for far longer than the window, which by itself must not be
	// enough: a CR blocked in cleanup for minutes is ordinary, and giving up on it
	// the moment the operator comes back would strand its children.
	deletedAt := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	neutron.DeletionTimestamp = &deletedAt
	r := newNeutronTestReconciler(neutron)
	r.Resolver = unresolvableResolver{}

	res, err := r.Reconcile(context.Background(), neutronRequest)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling),
		"the first pass this process fails to resolve on starts the window, it does not end it")
	g.Expect(getNeutron(t, r.Client).Finalizers).To(ContainElement(neutronFinalizer))
	time.Sleep(10 * commonmulticluster.AbandonAfter)

	res, err = r.Reconcile(context.Background(), neutronRequest)

	g.Expect(err).NotTo(HaveOccurred(),
		"a target cluster that is gone must not fail the deletion pass")
	g.Expect(res.IsZero()).To(BeTrue(), "the deletion resolves once the window is out")

	// With the finalizer released, the fake client garbage-collects the CR.
	var gone neutronv1alpha1.Neutron
	err = r.Get(context.Background(), neutronRequest.NamespacedName, &gone)
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
	neutron := validNeutron()
	neutron.Finalizers = []string{neutronFinalizer}
	neutron.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "not-engaged-yet"}
	r := newNeutronTestReconciler(neutron)
	r.Resolver = unresolvableResolver{}

	// Deleted just now, so the whole abandon window is still ahead.
	g.Expect(r.Delete(context.Background(), neutron)).To(Succeed())

	res, err := r.Reconcile(context.Background(), neutronRequest)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))

	got := getNeutron(t, r.Client)
	g.Expect(got.Finalizers).To(ContainElement(neutronFinalizer),
		"a target that may still be engaging must not cost the CR its finalizer")
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).
		NotTo(ContainElement(ContainSubstring("RemoteChildrenAbandoned")))

	// The hold lasts minutes, so it has to be readable off the CR: without it, a
	// namespace stuck Terminating looks like a wedged finalizer and can only be
	// told apart by correlating operator logs across replicas.
	cond := neutronCondition(got, "SecretsReady")
	g.Expect(cond).NotTo(BeNil(), "the deliberate hold must be visible on the CR")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(commonmulticluster.TargetClusterUnavailable))
	g.Expect(cond.Message).To(ContainSubstring("not-engaged-yet"))
}

// --- the database finalizer ------------------------------------------------

func TestReconcileDelete_LiveResourcesRetainFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()
	neutron.Finalizers = []string{neutronFinalizer}
	// A live MariaDB Database owned by this Neutron (key is the bare CR name).
	mdb := &mariadbv1alpha1.Database{}
	mdb.Name = testNeutronName
	mdb.Namespace = testNamespace
	r := newNeutronTestReconciler(neutron, mdb)

	// Move the Neutron into the deleting state.
	g.Expect(r.Delete(context.Background(), neutron)).To(Succeed())

	res, err := r.Reconcile(context.Background(), neutronRequest)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueDatabaseWait))
	g.Expect(getNeutron(t, r.Client).Finalizers).To(ContainElement(neutronFinalizer),
		"the finalizer is retained one pass while live MariaDB resources remain")
}

func TestReconcileDelete_NoLiveResourcesReleasesFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()
	neutron.Finalizers = []string{neutronFinalizer}
	r := newNeutronTestReconciler(neutron) // no MariaDB CRs

	g.Expect(r.Delete(context.Background(), neutron)).To(Succeed())

	res, err := r.Reconcile(context.Background(), neutronRequest)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	// With the finalizer released, the fake client garbage-collects the CR.
	var gone neutronv1alpha1.Neutron
	err = r.Get(context.Background(), neutronRequest.NamespacedName, &gone)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
		"the finalizer is released when no live MariaDB resource remains")
}

func TestReconcileDelete_WithoutFinalizerIsNoOp(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()
	neutron.DeletionTimestamp = &metav1.Time{Time: metav1.Now().Time}
	r := newNeutronTestReconciler()

	res, err := r.reconcileDelete(context.Background(), r.Client, neutron)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).To(BeEmpty(),
		"a CR without the finalizer emits no cleanup events")
}

// --- the aggregate ---------------------------------------------------------

// TestSetReadyCondition_TrueOnlyWhenAllSubConditionsTrue pins the aggregate: one
// missing or False member is enough to keep Ready False, which is what stops a
// half-projected Neutron from advertising itself as usable.
func TestSetReadyCondition_TrueOnlyWhenAllSubConditionsTrue(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()

	// All but the last sub-condition True: Ready stays False.
	for _, conditionType := range subConditionTypes[:len(subConditionTypes)-1] {
		conditions.SetCondition(&neutron.Status.Conditions, metav1.Condition{
			Type: conditionType, Status: metav1.ConditionTrue, Reason: "Ready",
		})
	}
	setReadyCondition(neutron)
	g.Expect(conditions.GetCondition(neutron.Status.Conditions, "Ready").Status).To(Equal(metav1.ConditionFalse))

	// The last one flips Ready True.
	conditions.SetCondition(&neutron.Status.Conditions, metav1.Condition{
		Type: subConditionTypes[len(subConditionTypes)-1], Status: metav1.ConditionTrue, Reason: "Ready",
	})
	setReadyCondition(neutron)
	g.Expect(conditions.GetCondition(neutron.Status.Conditions, "Ready").Status).To(Equal(metav1.ConditionTrue))
}

// --- remote children -------------------------------------------------------

// ownedRemoteChild stamps the ownership labels the projection wrote on a child it
// put on the target cluster, so the sweep selects it exactly as it would there.
func ownedRemoteChild(t *testing.T, owner, child client.Object) client.Object {
	t.Helper()
	labels, err := commonmulticluster.OwnerLabels(testScheme(), owner)
	NewGomegaWithT(t).Expect(err).NotTo(HaveOccurred())
	child.SetLabels(labels)
	return child
}

// terminatingRemoteNeutron returns a Neutron that names a target cluster and is
// being deleted, carrying the remote-children finalizer plus a foreign one so the
// CR survives the release and the test can read back which finalizers the pass
// dropped.
func terminatingRemoteNeutron(t *testing.T) (*NeutronReconciler, *neutronv1alpha1.Neutron) {
	t.Helper()
	g := NewGomegaWithT(t)
	neutron := validNeutron()
	neutron.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "target"}
	neutron.Finalizers = []string{commonmulticluster.RemoteChildrenFinalizer, "foreign.example.com/keep-alive"}
	deletedAt := metav1.NewTime(time.Now())
	neutron.DeletionTimestamp = &deletedAt

	r := newNeutronTestReconciler(neutron)
	var terminating neutronv1alpha1.Neutron
	g.Expect(r.Get(context.Background(), neutronRequest.NamespacedName, &terminating)).To(Succeed())
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
	r, terminating := terminatingRemoteNeutron(t)

	deployment := ownedRemoteChild(t, terminating,
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: testNeutronName, Namespace: testNamespace}})
	service := ownedRemoteChild(t, terminating,
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: testNeutronName, Namespace: testNamespace}})
	cronJob := ownedRemoteChild(t, terminating,
		&batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: testNeutronName + "-ovn-db-sync", Namespace: testNamespace}})
	grant := ownedRemoteChild(t, terminating,
		&mariadbv1alpha1.Grant{ObjectMeta: metav1.ObjectMeta{Name: testNeutronName, Namespace: testNamespace}})
	unlabelled := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cluster-ca", Namespace: testNamespace}}
	foreign := ownedRemoteChild(t,
		&neutronv1alpha1.Neutron{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: testNamespace}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "other-ovn-client", Namespace: testNamespace}})
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
		"another Neutron's child must survive this one's teardown")

	updated := getNeutron(t, r.Client)
	g.Expect(updated.Finalizers).To(ConsistOf("foreign.example.com/keep-alive"),
		"a completed sweep must release the remote-children finalizer and nothing else")
}

// TestReconcileDeleteRemoteChildren_NilChildrenAbandonsAndReleases covers the
// deregistered target cluster. Its children cannot be reached, so holding the
// finalizer would only strand the CR in Terminating; the objects left running are
// announced rather than silently dropped. Any attempt to sweep through the nil
// client would fault, which is what proves nothing was deleted.
func TestReconcileDeleteRemoteChildren_NilChildrenAbandonsAndReleases(t *testing.T) {
	g := NewGomegaWithT(t)
	r, terminating := terminatingRemoteNeutron(t)

	err := r.reconcileDeleteRemoteChildren(context.Background(), nil, terminating)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).
		To(ContainElement(ContainSubstring("RemoteChildrenAbandoned")))
	g.Expect(getNeutron(t, r.Client).Finalizers).NotTo(ContainElement(commonmulticluster.RemoteChildrenFinalizer),
		"an unreachable target cluster must not pin the CR forever")
}

// TestReconcileDeleteRemoteChildren_WithoutFinalizerIsNoOp covers the local CR:
// it never carries the remote-children finalizer, its children are collected
// from their owner references, and the caller runs the sweep unconditionally.
func TestReconcileDeleteRemoteChildren_WithoutFinalizerIsNoOp(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()
	neutron.Finalizers = []string{neutronFinalizer}
	r := newNeutronTestReconciler(neutron)

	err := r.reconcileDeleteRemoteChildren(context.Background(), nil, neutron)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).To(BeEmpty(),
		"a CR that projected nothing remotely has nothing to abandon")
	g.Expect(getNeutron(t, r.Client).Finalizers).To(ConsistOf(neutronFinalizer))
}

// TestReconcileDeleteRemoteChildren_SweepFailureKeepsFinalizer is the guard
// against a CR that leaves etcd while its children keep running. A list the
// target cluster refuses says nothing about whether children exist, so the pass
// has to fail and sweep again on the next one.
func TestReconcileDeleteRemoteChildren_SweepFailureKeepsFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)
	r, terminating := terminatingRemoteNeutron(t)

	child := ownedRemoteChild(t, terminating,
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: testNeutronName, Namespace: testNamespace}})
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
	g.Expect(getNeutron(t, r.Client).Finalizers).To(ContainElement(commonmulticluster.RemoteChildrenFinalizer),
		"a failed sweep must keep the finalizer so the next pass retries")
}

// TestReconcile_InstallsRemoteChildrenFinalizerForATargetCluster verifies that a
// CR naming a target cluster is pinned before anything is projected onto it, so a
// deletion issued between this pass and the next still funnels through the sweep.
func TestReconcile_InstallsRemoteChildrenFinalizerForATargetCluster(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()
	neutron.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "target"}
	neutron.Finalizers = []string{neutronFinalizer} // skip the database finalizer-add requeue
	r := newNeutronTestReconciler(neutron)

	res, err := r.Reconcile(context.Background(), neutronRequest)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{RequeueAfter: commonreconcile.RequeueNextPass}),
		"the pass installing the finalizer must requeue before any sub-reconciler runs")
	g.Expect(getNeutron(t, r.Client).Finalizers).To(ContainElement(commonmulticluster.RemoteChildrenFinalizer))
}

// TestReconcile_LocalCRNeverCarriesTheRemoteChildrenFinalizer pins the other
// half. A CR that keeps its children on the management cluster has nothing for
// the sweep to do, and a finalizer it does not need is one more thing that can
// block its deletion.
func TestReconcile_LocalCRNeverCarriesTheRemoteChildrenFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron() // no spec.targetClusterRef: children stay local
	neutron.Finalizers = []string{neutronFinalizer}
	r := newNeutronTestReconciler(neutron)

	_, err := r.Reconcile(context.Background(), neutronRequest)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getNeutron(t, r.Client).Finalizers).To(ConsistOf(neutronFinalizer),
		"a local CR must keep the finalizer set it always had")
}
