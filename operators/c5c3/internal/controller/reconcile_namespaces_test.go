// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the namespace sub-reconciler reconcileNamespaces and the ownership
// labels that stand in for the controller owner reference a cross-namespace child
// cannot carry. The tests cover both lifecycles (Managed creates and labels;
// External only verifies), the never-adopt guard that keeps a Managed lifecycle
// from taking over — and eventually deleting — somebody else's namespace, the
// Terminating waits, and the no-assignments short circuit.
package controller

import (
	"context"
	"testing"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	mcruntime "sigs.k8s.io/multicluster-runtime/pkg/multicluster"

	"github.com/c5c3/forge/internal/common/conditions"
	commonmulticluster "github.com/c5c3/forge/internal/common/multicluster"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
)

func namespacesTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("adding client-go scheme: %v", err)
	}
	if err := c5c3v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding c5c3 scheme: %v", err)
	}
	return s
}

// namespacedControlPlane builds a ControlPlane that places Keystone in an
// operator-owned namespace and the dashboard in a pre-existing one.
func namespacedControlPlane() *c5c3v1alpha1.ControlPlane {
	return &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "cp",
			Namespace:  "openstack",
			Generation: 1,
			UID:        types.UID("cp-uid"),
		},
		Spec: c5c3v1alpha1.ControlPlaneSpec{
			OpenStackRelease: "2025.2",
			Services: c5c3v1alpha1.ServicesSpec{
				Keystone: &c5c3v1alpha1.ServiceKeystoneSpec{
					Namespace: &c5c3v1alpha1.ServiceNamespaceSpec{
						Name:      "identity",
						Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
					},
				},
				Horizon: &c5c3v1alpha1.ServiceHorizonSpec{
					Namespace: &c5c3v1alpha1.ServiceNamespaceSpec{
						Name:      "dashboard",
						Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleExternal,
					},
				},
			},
		},
	}
}

// placedControlPlane builds namespacedControlPlane with Keystone's Managed
// namespace placed on a target cluster, and the dashboard dropped so the
// assignment under test is the only one.
func placedControlPlane(targetCluster string) *c5c3v1alpha1.ControlPlane {
	cp := namespacedControlPlane()
	cp.Spec.Services.Horizon = nil
	cp.Spec.Services.Keystone.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: targetCluster}
	return cp
}

// childrenResolver stands in for the multicluster manager: it registers one
// target cluster under every name, records the names it was asked for so a test
// can prove a namespace resolved (or that an unplaced one cost no lookup), and
// fails every lookup when err is set.
//
// errNames fails the NAMED clusters alone, leaving every other name resolvable.
// It is what a ControlPlane spanning two clusters, one of them deregistered,
// looks like to the resolver — the shape err (all of them) cannot express.
//
// reader, when set, answers the cluster's uncached API reader while children
// stays its cached client, so a test can pin which of the two a read went
// through by seeding them differently.
type childrenResolver struct {
	children client.Client
	reader   client.Reader
	err      error
	errNames map[string]error
	names    []mcruntime.ClusterName
}

func (r *childrenResolver) GetCluster(_ context.Context, name mcruntime.ClusterName) (cluster.Cluster, error) {
	r.names = append(r.names, name)
	if r.err != nil {
		return nil, r.err
	}
	if err := r.errNames[string(name)]; err != nil {
		return nil, err
	}
	return fakeTargetCluster{c: r.children, reader: r.reader}, nil
}

// fakeTargetCluster implements the two accessors ResolveChildrenClient reads.
// Embedding the interface leaves every other method nil, which panics if
// anything reaches for one. A fake client is read-your-writes, so it stands in
// for both the cached client and the uncached API reader unless a test hands in
// a reader of its own.
type fakeTargetCluster struct {
	cluster.Cluster
	c      client.Client
	reader client.Reader
}

func (f fakeTargetCluster) GetClient() client.Client { return f.c }
func (f fakeTargetCluster) GetAPIReader() client.Reader {
	if f.reader != nil {
		return f.reader
	}
	return f.c
}

// remoteChildLabels is the label set every child written to a TARGET cluster has
// to end up with: the owner triple the shared teardown selects on, plus the two
// cross-namespace labels this operator's watch legs map an event back to its
// ControlPlane by. A remote child carries no owner reference, so its labels are
// the whole of its identity — which is why the write sites assert against the
// full set rather than a key or two of it.
func remoteChildLabels(cp *c5c3v1alpha1.ControlPlane) map[string]string {
	return map[string]string{
		commonmulticluster.OwnerKindLabel:      "ControlPlane",
		commonmulticluster.OwnerNameLabel:      cp.Name,
		commonmulticluster.OwnerNamespaceLabel: cp.Namespace,
		controlPlaneNameLabel:                  cp.Name,
		controlPlaneNamespaceLabel:             cp.Namespace,
	}
}

// localWriter is a client on the management cluster, for the claim and adoption
// helpers a test calls outside a reconciler. They read nothing off it beyond
// whether it is a resolved target-cluster client, which a bare fake is not.
func localWriter() client.Client {
	return fake.NewClientBuilder().Build()
}

func namespacesCondition(cp *c5c3v1alpha1.ControlPlane) *metav1.Condition {
	return conditions.GetCondition(cp.Status.Conditions, conditionTypeNamespacesReady)
}

// existingNamespace returns a Namespace object with the given labels.
func existingNamespace(name string, labels map[string]string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

// TestReconcileNamespaces_NoAssignments verifies the default path costs nothing:
// a ControlPlane whose services stay in its own namespace reports True at once.
func TestReconcileNamespaces_NoAssignments(t *testing.T) {
	g := NewGomegaWithT(t)
	s := namespacesTestScheme(t)
	cp := namespacedControlPlane()
	cp.Spec.Services.Keystone.Namespace = nil
	cp.Spec.Services.Horizon.Namespace = nil

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	res, err := r.reconcileNamespaces(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := namespacesCondition(cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("NoDedicatedNamespaces"))
}

// TestReconcileNamespaces_ManagedCreatesAndLabels verifies the Managed lifecycle
// creates the namespace and stamps it with the ownership labels — the labels are
// what license the teardown to delete it again, and what let the watch resolve an
// event on it back to the ControlPlane.
func TestReconcileNamespaces_ManagedCreatesAndLabels(t *testing.T) {
	g := NewGomegaWithT(t)
	s := namespacesTestScheme(t)
	cp := namespacedControlPlane()
	cp.Spec.Services.Horizon = nil // Keystone's Managed namespace only.

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	// A namespace created with our labels is ours by construction, so the pass
	// goes Ready straight away rather than waiting a requeue to re-read what it
	// just wrote.
	res, err := r.reconcileNamespaces(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())

	ns := &corev1.Namespace{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: "identity"}, ns)).To(Succeed())
	g.Expect(ns.Labels).To(HaveKeyWithValue(controlPlaneNameLabel, "cp"))
	g.Expect(ns.Labels).To(HaveKeyWithValue(controlPlaneNamespaceLabel, "openstack"))
	g.Expect(ns.Labels).To(HaveKeyWithValue(managedByLabel, managedByValue))

	cond := namespacesCondition(cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("NamespacesReady"))

	// Idempotent: a second pass observes the namespace it owns and stays Ready.
	res, err = r.reconcileNamespaces(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(namespacesCondition(cp).Reason).To(Equal("NamespacesReady"))
}

// TestReconcileNamespaces_ManagedRefusesToAdoptForeignNamespace is the guard that
// matters most: a Managed lifecycle DELETES its namespace at teardown, so silently
// adopting a pre-existing one would destroy every workload in it. The reconciler
// fails loud and never touches it.
func TestReconcileNamespaces_ManagedRefusesToAdoptForeignNamespace(t *testing.T) {
	g := NewGomegaWithT(t)
	s := namespacesTestScheme(t)
	cp := namespacedControlPlane()
	cp.Spec.Services.Horizon = nil

	foreign := existingNamespace("identity", map[string]string{"team": "platform"})
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, foreign).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	res, err := r.reconcileNamespaces(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(namespaceRequeueAfter))

	cond := namespacesCondition(cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("NamespaceNotOwned"))
	g.Expect(cond.Message).To(ContainSubstring("lifecycle External"))

	// The foreign namespace is left exactly as it was.
	live := &corev1.Namespace{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: "identity"}, live)).To(Succeed())
	g.Expect(live.Labels).To(Equal(map[string]string{"team": "platform"}))
}

// TestReconcileNamespaces_ExternalRequiresThePreexistingNamespace verifies the
// External lifecycle never creates: a missing namespace parks the condition and
// requeues rather than conjuring the namespace the lifecycle said is not ours.
func TestReconcileNamespaces_ExternalRequiresThePreexistingNamespace(t *testing.T) {
	g := NewGomegaWithT(t)
	s := namespacesTestScheme(t)
	cp := namespacedControlPlane()
	cp.Spec.Services.Keystone.Namespace = nil // dashboard's External namespace only.

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	res, err := r.reconcileNamespaces(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(namespaceRequeueAfter))

	cond := namespacesCondition(cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("NamespaceNotFound"))

	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: "dashboard"}, &corev1.Namespace{})).
		NotTo(Succeed(), "an External namespace must never be created by the operator")
}

// TestReconcileNamespaces_ExternalIsNeverLabelled verifies a pre-existing External
// namespace passes the gate untouched: no ownership labels, so the teardown can
// never mistake it for one it may delete.
func TestReconcileNamespaces_ExternalIsNeverLabelled(t *testing.T) {
	g := NewGomegaWithT(t)
	s := namespacesTestScheme(t)
	cp := namespacedControlPlane()
	cp.Spec.Services.Keystone.Namespace = nil

	preexisting := existingNamespace("dashboard", map[string]string{"team": "platform"})
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, preexisting).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	res, err := r.reconcileNamespaces(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(namespacesCondition(cp).Status).To(Equal(metav1.ConditionTrue))

	live := &corev1.Namespace{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: "dashboard"}, live)).To(Succeed())
	g.Expect(live.Labels).To(Equal(map[string]string{"team": "platform"}),
		"an External namespace must never be labelled by the operator")
}

// TestReconcileNamespaces_TerminatingWaits verifies a namespace on its way out —
// ours or somebody else's — parks the condition instead of projecting children
// into a namespace the API server is about to reject writes for.
func TestReconcileNamespaces_TerminatingWaits(t *testing.T) {
	now := metav1.Now()

	t.Run("managed", func(t *testing.T) {
		g := NewGomegaWithT(t)
		s := namespacesTestScheme(t)
		cp := namespacedControlPlane()
		cp.Spec.Services.Horizon = nil

		terminating := existingNamespace("identity", controlPlaneChildLabels(cp))
		terminating.DeletionTimestamp = &now
		terminating.Finalizers = []string{"kubernetes"}

		c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, terminating).Build()
		r := &ControlPlaneReconciler{Client: c, Scheme: s}

		res, err := r.reconcileNamespaces(context.Background(), cp)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.RequeueAfter).To(Equal(namespaceRequeueAfter))
		g.Expect(namespacesCondition(cp).Reason).To(Equal("NamespaceTerminating"))
	})

	t.Run("external", func(t *testing.T) {
		g := NewGomegaWithT(t)
		s := namespacesTestScheme(t)
		cp := namespacedControlPlane()
		cp.Spec.Services.Keystone.Namespace = nil

		terminating := existingNamespace("dashboard", nil)
		terminating.DeletionTimestamp = &now
		terminating.Finalizers = []string{"kubernetes"}

		c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, terminating).Build()
		r := &ControlPlaneReconciler{Client: c, Scheme: s}

		res, err := r.reconcileNamespaces(context.Background(), cp)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.RequeueAfter).To(Equal(namespaceRequeueAfter))
		g.Expect(namespacesCondition(cp).Reason).To(Equal("NamespaceTerminating"))
	})
}

// TestReconcileNamespaces_PlacedManagedIsCreatedOnBothClusters pins the split a
// placed namespace lives in: the ControlPlane keeps projecting into it at home,
// and the service operator that picks the workload CR up projects into the
// namespace of the same name on the target cluster, so it is created on both.
//
// The remote copy's FULL label set is asserted, not a few keys of it. The claim
// there goes through ClaimWithLabels, which REBUILDS the label set from what it
// is handed, so a caller passing less than the whole set silently drops labels —
// managed-by among them — and nothing but this assertion would notice.
func TestReconcileNamespaces_PlacedManagedIsCreatedOnBothClusters(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	s := namespacesTestScheme(t)
	cp := placedControlPlane("remote-a")

	target := fake.NewClientBuilder().WithScheme(s).Build()
	resolver := &childrenResolver{children: target}
	r := &ControlPlaneReconciler{
		Client:   fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build(),
		Scheme:   s,
		Resolver: resolver,
	}

	res, err := r.reconcileNamespaces(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(namespacesCondition(cp).Reason).To(Equal("NamespacesReady"))
	g.Expect(resolver.names).To(Equal([]mcruntime.ClusterName{"remote-a"}),
		"the namespace resolves the cluster the service placed in it names")

	// At home the namespace is claimed exactly as an unplaced one is: the two
	// cross-namespace labels plus managed-by, and no owner-* label.
	local := &corev1.Namespace{}
	g.Expect(r.Client.Get(ctx, types.NamespacedName{Name: "identity"}, local)).To(Succeed())
	g.Expect(local.Labels).To(Equal(map[string]string{
		controlPlaneNameLabel:      "cp",
		controlPlaneNamespaceLabel: "openstack",
		managedByLabel:             managedByValue,
	}))

	remote := &corev1.Namespace{}
	g.Expect(target.Get(ctx, types.NamespacedName{Name: "identity"}, remote)).To(Succeed())
	g.Expect(remote.Labels).To(Equal(map[string]string{
		commonmulticluster.OwnerKindLabel:      "ControlPlane",
		commonmulticluster.OwnerNameLabel:      "cp",
		commonmulticluster.OwnerNamespaceLabel: "openstack",
		controlPlaneNameLabel:                  "cp",
		controlPlaneNamespaceLabel:             "openstack",
		managedByLabel:                         managedByValue,
	}), "a remote child is recognized by its labels alone, so the whole set has to survive the claim")
	g.Expect(remote.OwnerReferences).To(BeEmpty(),
		"an owner reference on the target cluster names a UID that cluster cannot resolve")
	g.Expect(remote.Annotations).To(HaveKeyWithValue(controlPlaneUIDAnnotation, "cp-uid"),
		"the labels name a ControlPlane by name and namespace, which two management clusters can share; "+
			"the UID is what an adoption and a namespace delete on the target are decided by")
}

// TestReconcileNamespaces_PlacedManagedRefusesAnotherManagementClustersNamespace
// is the never-adopt guard against a collision the labels cannot see. A target
// cluster is registerable from any number of management clusters, each able to
// run a ControlPlane called "cp" in namespace "openstack" and to place a service
// in "identity" — so the label pair matches and the namespace is somebody else's
// all the same. Adopting it would have two operators write into one namespace and
// either teardown cascade the other's database away.
func TestReconcileNamespaces_PlacedManagedRefusesAnotherManagementClustersNamespace(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	s := namespacesTestScheme(t)
	cp := placedControlPlane("remote-a")

	theirs := existingNamespace("identity", map[string]string{
		controlPlaneNameLabel:      cp.Name,
		controlPlaneNamespaceLabel: cp.Namespace,
		managedByLabel:             managedByValue,
	})
	theirs.Annotations = map[string]string{controlPlaneUIDAnnotation: "another-management-clusters-cp-uid"}
	target := fake.NewClientBuilder().WithScheme(s).WithObjects(theirs).Build()
	r := &ControlPlaneReconciler{
		Client:   fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build(),
		Scheme:   s,
		Resolver: &childrenResolver{children: target},
	}

	res, err := r.reconcileNamespaces(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(namespaceRequeueAfter))

	cond := namespacesCondition(cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("NamespaceNotOwned"))
	// The message has to name the cause an operator can act on. This namespace WAS
	// created by a ControlPlane of this name in a namespace of this name, so the
	// generic "was not created by this ControlPlane" would send them looking for
	// the wrong thing.
	g.Expect(cond.Message).To(ContainSubstring("another-management-clusters-cp-uid"))
	g.Expect(cond.Message).To(ContainSubstring("records another ControlPlane's UID"))

	live := &corev1.Namespace{}
	g.Expect(target.Get(ctx, types.NamespacedName{Name: "identity"}, live)).To(Succeed())
	g.Expect(live.Annotations).To(HaveKeyWithValue(controlPlaneUIDAnnotation,
		"another-management-clusters-cp-uid"), "the other ControlPlane's mark must be left where it is")
}

// TestReconcileNamespaces_PlacedManagedRefusesAnUnmarkedNamespace covers the
// other half of that verdict, and the reason adoption asks for the mark itself
// rather than for the labels around it. Both labels are derived from the CR's name
// and namespace, values the CR publishes, so on a target cluster anyone holding
// patch on a namespace can write them onto one this ControlPlane never created —
// here a namespace full of somebody else's workloads. Only the UID says otherwise,
// and it is minted on the management cluster and unreadable from here.
//
// Adopting on the labels alone would take the namespace over on the strength of
// that one patch: the pass would stamp OUR mark into it, and the Managed teardown
// would then cascade it — every workload, PVC and Secret in it — away. So it is
// refused, nothing is written to it, and the message names both remedies.
func TestReconcileNamespaces_PlacedManagedRefusesAnUnmarkedNamespace(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	s := namespacesTestScheme(t)
	cp := placedControlPlane("remote-a")

	unmarked := existingNamespace("identity", map[string]string{
		commonmulticluster.OwnerKindLabel:      "ControlPlane",
		commonmulticluster.OwnerNameLabel:      cp.Name,
		commonmulticluster.OwnerNamespaceLabel: cp.Namespace,
		controlPlaneNameLabel:                  cp.Name,
		controlPlaneNamespaceLabel:             cp.Namespace,
		managedByLabel:                         managedByValue,
	})
	target := fake.NewClientBuilder().WithScheme(s).WithObjects(unmarked).Build()
	r := &ControlPlaneReconciler{
		Client:   fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build(),
		Scheme:   s,
		Resolver: &childrenResolver{children: target},
	}

	res, err := r.reconcileNamespaces(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(namespaceRequeueAfter))

	cond := namespacesCondition(cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("NamespaceNotOwned"))
	// The two causes are indistinguishable from the object, so the message has to
	// carry both — and the UID to restore, for the half that is a stripped mark.
	g.Expect(cond.Message).To(ContainSubstring(controlPlaneUIDAnnotation))
	g.Expect(cond.Message).To(ContainSubstring(string(cp.UID)))

	live := &corev1.Namespace{}
	g.Expect(target.Get(ctx, types.NamespacedName{Name: "identity"}, live)).To(Succeed())
	g.Expect(live.Annotations).NotTo(HaveKey(controlPlaneUIDAnnotation),
		"a refused namespace must not be stamped with this ControlPlane's mark: that stamp is what would license "+
			"the teardown cascade the refusal exists to prevent")
}

// TestReconcileNamespaces_PlacedManagedDecidesFromLiveState pins which of the
// target cluster's two readers the adoption verdict is taken from. It is an
// ownership decision about marks this very operator writes on that cluster, and
// the same verdict on the teardown side already reads live: a cache one resync
// behind reports a namespace that exists as absent, and the pass would create —
// and eventually own — something on the strength of a view that had not caught
// up.
func TestReconcileNamespaces_PlacedManagedDecidesFromLiveState(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	s := namespacesTestScheme(t)
	cp := placedControlPlane("remote-a")

	// Somebody else's namespace, already on the target cluster and not yet in the
	// cache the cached client answers from.
	foreign := existingNamespace("identity", map[string]string{"team": "platform"})
	cached := fake.NewClientBuilder().WithScheme(s).Build()
	livecluster := fake.NewClientBuilder().WithScheme(s).WithObjects(foreign).Build()
	r := &ControlPlaneReconciler{
		Client:   fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build(),
		Scheme:   s,
		Resolver: &childrenResolver{children: cached, reader: livecluster},
	}

	res, err := r.reconcileNamespaces(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(namespaceRequeueAfter))

	cond := namespacesCondition(cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("NamespaceNotOwned"),
		"the namespace the target cluster actually holds is the one the verdict is about")

	g.Expect(cached.Get(ctx, types.NamespacedName{Name: "identity"}, &corev1.Namespace{})).NotTo(Succeed(),
		"nothing may be created on the strength of a cache that has not caught up")
}

// TestReconcileNamespaces_PlacedManagedRefusesAForeignNamespaceOnTheTarget is the
// never-adopt guard on the far side. The target cluster is somebody else's
// cluster: a name free at home may well be taken there, and a Managed lifecycle
// DELETES its namespace at teardown.
func TestReconcileNamespaces_PlacedManagedRefusesAForeignNamespaceOnTheTarget(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	s := namespacesTestScheme(t)
	cp := placedControlPlane("remote-a")

	foreign := existingNamespace("identity", map[string]string{"team": "platform"})
	target := fake.NewClientBuilder().WithScheme(s).WithObjects(foreign).Build()
	r := &ControlPlaneReconciler{
		Client:   fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build(),
		Scheme:   s,
		Resolver: &childrenResolver{children: target},
	}

	res, err := r.reconcileNamespaces(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(namespaceRequeueAfter))

	cond := namespacesCondition(cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("NamespaceNotOwned"))

	live := &corev1.Namespace{}
	g.Expect(target.Get(ctx, types.NamespacedName{Name: "identity"}, live)).To(Succeed())
	g.Expect(live.Labels).To(Equal(map[string]string{"team": "platform"}),
		"a namespace the ControlPlane did not create must not be labelled, and so must not become deletable")
}

// TestReconcileNamespaces_PlacedExternalIsVerifiedOnBothClusters covers the
// lifecycle that creates nothing: the namespace has to be provisioned on both
// clusters out-of-band, and a missing one on either side parks the condition on
// the reason it always had.
func TestReconcileNamespaces_PlacedExternalIsVerifiedOnBothClusters(t *testing.T) {
	placedExternal := func() *c5c3v1alpha1.ControlPlane {
		cp := namespacedControlPlane()
		cp.Spec.Services.Keystone.Namespace = nil // the dashboard's External namespace only
		cp.Spec.Services.Horizon.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "remote-a"}
		return cp
	}

	t.Run("present on both", func(t *testing.T) {
		g := NewGomegaWithT(t)
		s := namespacesTestScheme(t)
		cp := placedExternal()

		target := fake.NewClientBuilder().WithScheme(s).WithObjects(existingNamespace("dashboard", nil)).Build()
		r := &ControlPlaneReconciler{
			Client:   fake.NewClientBuilder().WithScheme(s).WithObjects(cp, existingNamespace("dashboard", nil)).Build(),
			Scheme:   s,
			Resolver: &childrenResolver{children: target},
		}

		res, err := r.reconcileNamespaces(context.Background(), cp)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.IsZero()).To(BeTrue())
		g.Expect(namespacesCondition(cp).Status).To(Equal(metav1.ConditionTrue))
	})

	t.Run("missing on the target", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ctx := context.Background()
		s := namespacesTestScheme(t)
		cp := placedExternal()

		// Present at home, so only the target's verification can fail the pass.
		target := fake.NewClientBuilder().WithScheme(s).Build()
		r := &ControlPlaneReconciler{
			Client:   fake.NewClientBuilder().WithScheme(s).WithObjects(cp, existingNamespace("dashboard", nil)).Build(),
			Scheme:   s,
			Resolver: &childrenResolver{children: target},
		}

		res, err := r.reconcileNamespaces(ctx, cp)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.RequeueAfter).To(Equal(namespaceRequeueAfter))
		g.Expect(namespacesCondition(cp).Reason).To(Equal("NamespaceNotFound"))

		g.Expect(target.Get(ctx, types.NamespacedName{Name: "dashboard"}, &corev1.Namespace{})).NotTo(Succeed(),
			"an External namespace must never be created by the operator, on either cluster")
	})
}

// TestReconcileNamespaces_UnresolvableTargetCreatesNothing covers the cluster
// that does not resolve — never registered, or deregistered under a running
// ControlPlane. The pass ends before anything is written, on either cluster, and
// reports the resolver's own message so an operator reads why rather than a
// prefix this repo invented.
func TestReconcileNamespaces_UnresolvableTargetCreatesNothing(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	s := namespacesTestScheme(t)
	cp := placedControlPlane("remote-a")

	target := fake.NewClientBuilder().WithScheme(s).Build()
	r := &ControlPlaneReconciler{
		Client:   fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build(),
		Scheme:   s,
		Resolver: &childrenResolver{children: target, err: mcruntime.ErrClusterNotFound},
	}

	res, err := r.reconcileNamespaces(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred(), "an unregistered cluster is a state to wait out, not a reconcile failure")
	g.Expect(res.RequeueAfter).To(Equal(namespaceRequeueAfter))

	cond := namespacesCondition(cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(commonmulticluster.TargetClusterUnavailable))
	g.Expect(cond.Message).To(ContainSubstring("cluster not found"))

	g.Expect(r.Client.Get(ctx, types.NamespacedName{Name: "identity"}, &corev1.Namespace{})).NotTo(Succeed(),
		"a namespace whose cluster does not resolve must not be created at home either")
	g.Expect(target.Get(ctx, types.NamespacedName{Name: "identity"}, &corev1.Namespace{})).NotTo(Succeed())
}

// TestReconcileNamespaces_UnplacedNamespaceStaysLocal is the regression guard for
// the ControlPlanes that place nothing, which is every one of them today: a
// registered target cluster must cost neither a lookup nor a namespace, and the
// namespace must be ensured exactly once.
func TestReconcileNamespaces_UnplacedNamespaceStaysLocal(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	s := namespacesTestScheme(t)
	cp := namespacedControlPlane()
	cp.Spec.Services.Horizon = nil

	target := fake.NewClientBuilder().WithScheme(s).Build()
	resolver := &childrenResolver{children: target}
	r := &ControlPlaneReconciler{
		Client:   fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build(),
		Scheme:   s,
		Resolver: resolver,
	}

	res, err := r.reconcileNamespaces(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(resolver.names).To(BeEmpty(), "a namespace no service placed must not cost a cluster lookup")

	local := &corev1.Namespace{}
	g.Expect(r.Client.Get(ctx, types.NamespacedName{Name: "identity"}, local)).To(Succeed())
	g.Expect(local.Labels).NotTo(HaveKey(commonmulticluster.OwnerKindLabel),
		"a local child is claimed by the cross-namespace labels alone")
	g.Expect(target.Get(ctx, types.NamespacedName{Name: "identity"}, &corev1.Namespace{})).NotTo(Succeed())
}

// TestIsControlPlaneChild covers both ownership tests: the owner reference (the
// same-namespace case) and the labels (the cross-namespace case, where no owner
// reference is possible), plus the collision an object carrying neither must not
// be adopted through.
func TestIsControlPlaneChild(t *testing.T) {
	g := NewGomegaWithT(t)
	s := namespacesTestScheme(t)
	cp := namespacedControlPlane()

	owned := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "owned", Namespace: "openstack"}}
	g.Expect(controllerutil.SetControllerReference(cp, owned, s)).To(Succeed())
	g.Expect(isControlPlaneChild(owned, cp)).To(BeTrue(), "the owner reference must be honoured")

	labelled := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "labelled", Namespace: "identity"}}
	stampControlPlaneChildLabels(labelled, cp)
	g.Expect(isControlPlaneChild(labelled, cp)).To(BeTrue(), "the ownership labels must be honoured")

	// A same-named object of ANOTHER ControlPlane: the name matches, the namespace
	// label does not, so it must not be adopted.
	other := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name:      "labelled",
		Namespace: "identity",
		Labels: map[string]string{
			controlPlaneNameLabel:      "cp",
			controlPlaneNamespaceLabel: "other-ns",
		},
	}}
	g.Expect(isControlPlaneChild(other, cp)).To(BeFalse(),
		"a child of a same-named ControlPlane in another namespace must not be adopted")

	foreign := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "foreign", Namespace: "identity"}}
	g.Expect(isControlPlaneChild(foreign, cp)).To(BeFalse())
}

// TestCrossNamespaceChildMapper verifies a labelled child resolves back to its
// ControlPlane, and an unlabelled object wakes nobody — the same-namespace
// children keep flowing through Owns() alone, and a foreign object in a service
// namespace must not enqueue a reconcile.
func TestCrossNamespaceChildMapper(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := namespacedControlPlane()

	labelled := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "child", Namespace: "identity"}}
	stampControlPlaneChildLabels(labelled, cp)
	g.Expect(crossNamespaceChildMapper(context.Background(), labelled)).To(ConsistOf(
		reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "openstack", Name: "cp"}},
	))

	unlabelled := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "child", Namespace: "identity"}}
	g.Expect(crossNamespaceChildMapper(context.Background(), unlabelled)).To(BeEmpty())

	// A half-stamped object (one label only) is not resolvable and must not be
	// mapped to a ControlPlane in the empty namespace "".
	partial := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: "child", Namespace: "identity",
		Labels: map[string]string{controlPlaneNameLabel: "cp"},
	}}
	g.Expect(crossNamespaceChildMapper(context.Background(), partial)).To(BeEmpty())
}

// TestCrossNamespaceChildPredicate verifies the Watch-leg predicate admits only
// objects carrying both ownership labels, across every event kind. An unlabelled
// or half-stamped object is filtered before the mapper runs, so the shared
// informers — and the cluster-wide Namespace informer — never wake the mapper for
// a foreign object.
func TestCrossNamespaceChildPredicate(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := namespacedControlPlane()
	p := crossNamespaceChildPredicate()

	labelled := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "child", Namespace: "identity"}}
	stampControlPlaneChildLabels(labelled, cp)
	g.Expect(p.Create(event.CreateEvent{Object: labelled})).To(BeTrue())
	g.Expect(p.Update(event.UpdateEvent{ObjectOld: labelled, ObjectNew: labelled})).To(BeTrue())
	g.Expect(p.Delete(event.DeleteEvent{Object: labelled})).To(BeTrue())
	g.Expect(p.Generic(event.GenericEvent{Object: labelled})).To(BeTrue())

	unlabelled := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "child", Namespace: "identity"}}
	g.Expect(p.Create(event.CreateEvent{Object: unlabelled})).To(BeFalse())
	g.Expect(p.Update(event.UpdateEvent{ObjectOld: unlabelled, ObjectNew: unlabelled})).To(BeFalse())
	g.Expect(p.Delete(event.DeleteEvent{Object: unlabelled})).To(BeFalse())
	g.Expect(p.Generic(event.GenericEvent{Object: unlabelled})).To(BeFalse())

	// A half-stamped object (one label only) is not a resolvable child, so the
	// predicate filters it exactly as crossNamespaceChildMapper would discard it.
	partial := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: "child", Namespace: "identity",
		Labels: map[string]string{controlPlaneNameLabel: "cp"},
	}}
	g.Expect(p.Create(event.CreateEvent{Object: partial})).To(BeFalse())
}

// TestControlPlaneNamespaces verifies the occupied-namespace set: the
// ControlPlane's own namespace plus every service namespace, deduplicated.
func TestControlPlaneNamespaces(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := namespacedControlPlane()
	g.Expect(controlPlaneNamespaces(cp)).To(ConsistOf("openstack", "identity", "dashboard"))

	plain := namespacedControlPlane()
	plain.Spec.Services.Keystone.Namespace = nil
	plain.Spec.Services.Horizon.Namespace = nil
	g.Expect(controlPlaneNamespaces(plain)).To(ConsistOf("openstack"))

	colocated := namespacedControlPlane()
	colocated.Spec.Services.Horizon.Namespace.Name = "identity"
	colocated.Spec.Services.Horizon.Namespace.Lifecycle = c5c3v1alpha1.ServiceNamespaceLifecycleManaged
	g.Expect(controlPlaneNamespaces(colocated)).To(ConsistOf("openstack", "identity"),
		"co-located services share one namespace, which is listed once")
}

// TestRefuseForeignAdoption_NamesTheKind pins the one thing an operator has to go
// on when the guard fires: the refusal names the refused object's KIND.
//
// The typed case is the regression. A *corev1.Secret built in-code carries an
// empty TypeMeta and the typed client does not populate it on Get, so reading the
// kind off the object rendered it BLANK — the guard refused correctly, but the
// resulting ServiceAccountsReady=False message said "refusing to adopt
// pre-existing  identity/cp-..." and never named what was refused. The
// unstructured Certificate is the control: it carries its own GVK and must keep
// resolving identically now that the kind comes from the scheme.
func TestRefuseForeignAdoption_NamesTheKind(t *testing.T) {
	cp := namespacedControlPlane()

	// A typed Secret exactly as CreateOrUpdate hands it to the guard: built
	// in-code (empty TypeMeta), Get-populated with a foreign object's UID and
	// labels, in a service namespace the ControlPlane does not own.
	foreignSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name:      "cp-service-account-nova-source",
		Namespace: "dashboard",
		UID:       types.UID("foreign-secret-uid"),
		Labels:    map[string]string{"owner": "someone-else"},
	}}
	foreignCert := &unstructured.Unstructured{}
	foreignCert.SetGroupVersionKind(certificateGVK)
	foreignCert.SetName(esoTenantClientCertName)
	foreignCert.SetNamespace("dashboard")
	foreignCert.SetUID(types.UID("foreign-cert-uid"))
	foreignCert.SetLabels(map[string]string{"owner": "someone-else"})

	tests := []struct {
		name string
		live client.Object
		kind string
	}{
		{name: "typed Secret with an empty TypeMeta", live: foreignSecret, kind: "Secret"},
		{name: "unstructured Certificate", live: foreignCert, kind: "Certificate"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)

			err := refuseForeignAdoption(localWriter(), cp, tc.live, namespacesTestScheme(t))

			g.Expect(err).To(HaveOccurred(), "a foreign object in an unowned namespace must be refused")
			g.Expect(err.Error()).To(ContainSubstring("refusing to adopt pre-existing "+tc.kind+" "),
				"the refusal must name the kind: it is all an operator has to identify WHAT was refused")
		})
	}
}

// TestRefuseForeignAdoption_AllowsOwnAndAbsent covers the three states that are
// not a foreign adoption, so resolving the kind from the scheme did not tighten
// the guard itself: our own labelled child, a name in the ControlPlane's own
// namespace (an owner reference is legal there), and an absent object
// CreateOrUpdate is about to create.
func TestRefuseForeignAdoption_AllowsOwnAndAbsent(t *testing.T) {
	cp := namespacedControlPlane()

	ownChild := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name:      "cp-service-account-nova-source",
		Namespace: "dashboard",
		UID:       types.UID("our-secret-uid"),
		Labels:    controlPlaneChildLabels(cp),
	}}
	atHome := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name:      "cp-admin-app-credential",
		Namespace: cp.Namespace,
		UID:       types.UID("home-secret-uid"),
		Labels:    map[string]string{"owner": "someone-else"},
	}}
	absent := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name:      "cp-service-account-nova-source",
		Namespace: "dashboard",
	}}

	tests := []struct {
		name string
		live client.Object
	}{
		{name: "our own labelled child in a service namespace", live: ownChild},
		{name: "a foreign name in the ControlPlane's own namespace", live: atHome},
		{name: "an absent object about to be created", live: absent},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			g.Expect(refuseForeignAdoption(localWriter(), cp, tc.live, namespacesTestScheme(t))).To(Succeed())
		})
	}
}

// TestRefuseForeignAdoption_UnresolvableKindStillRefuses guards the fallback: a
// kind the scheme cannot resolve must still be REFUSED — refusing is the security
// behavior, and naming the kind is only the diagnostic on top of it.
func TestRefuseForeignAdoption_UnresolvableKindStillRefuses(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := namespacedControlPlane()

	foreign := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name:      "cp-service-account-nova-source",
		Namespace: "dashboard",
		UID:       types.UID("foreign-secret-uid"),
	}}

	// An EMPTY scheme resolves no kind at all, so the guard falls back to the
	// object's own (here: blank) GVK rather than erroring out and letting the
	// adoption through.
	err := refuseForeignAdoption(localWriter(), cp, foreign, runtime.NewScheme())

	g.Expect(err).To(HaveOccurred(), "an unresolvable kind must not turn a refusal into an adoption")
	g.Expect(err.Error()).To(ContainSubstring("refusing to adopt pre-existing"))
}

// TestEnsureUnownedOrOwned_ClaimsARemoteChildWithTheOwnerLabels pins the claim on
// the far side of an SSA projection. A cross-namespace child is stamped with the
// two ControlPlane labels at home, which is all the local teardown needs — but on
// a target cluster the shared teardown selects on the owner-* triple, and a child
// carrying only this operator's own pair would be invisible to it and outlive
// every ControlPlane that could delete it. Both label sets have to survive.
func TestEnsureUnownedOrOwned_ClaimsARemoteChildWithTheOwnerLabels(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	s := namespacesTestScheme(t)
	if err := esov1.AddToScheme(s); err != nil {
		t.Fatalf("adding external-secrets scheme: %v", err)
	}
	cp := placedControlPlane("remote-a")

	target := fake.NewClientBuilder().WithScheme(s).Build()
	r := &ControlPlaneReconciler{
		Client:   fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build(),
		Scheme:   s,
		Resolver: &childrenResolver{children: target},
	}
	children, err := r.childrenClientFor(ctx, cp, "identity")
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(r.ensureUnownedOrOwned(ctx, children, cp, &esov1.ExternalSecret{ObjectMeta: metav1.ObjectMeta{
		Namespace: "identity", Name: "cp-admin-password",
	}})).To(Succeed())

	remote := &esov1.ExternalSecret{}
	g.Expect(target.Get(ctx, types.NamespacedName{Namespace: "identity", Name: "cp-admin-password"}, remote)).To(Succeed())
	g.Expect(remote.Labels).To(Equal(remoteChildLabels(cp)))
	g.Expect(remote.OwnerReferences).To(BeEmpty(),
		"an owner reference on the target cluster names a UID that cluster cannot resolve")

	// The local cross-namespace claim is unchanged: the two labels and nothing more.
	g.Expect(r.ensureUnownedOrOwned(ctx, r.Client, cp, &esov1.ExternalSecret{ObjectMeta: metav1.ObjectMeta{
		Namespace: "identity", Name: "cp-db-credentials",
	}})).To(Succeed())
	local := &esov1.ExternalSecret{}
	g.Expect(r.Client.Get(ctx, types.NamespacedName{Namespace: "identity", Name: "cp-db-credentials"}, local)).To(Succeed())
	g.Expect(local.Labels).To(Equal(controlPlaneChildLabels(cp)))
}

// countingReader is an uncached reader that records how many Gets were routed
// through it, so a test can tell WHICH reader ensureUnownedOrOwned's adoption
// pre-check used.
type countingReader struct {
	client.Reader
	gets int
}

func (c *countingReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	c.gets++
	return c.Reader.Get(ctx, key, obj, opts...)
}

// TestEnsureUnownedOrOwned_ReadsWatchedKindsThroughTheCache pins which reader the
// adoption pre-check uses per kind. The uncached reader exists for the kinds the
// operator never watches (the three RBAC kinds and ServiceAccount), where a cached
// read would start an unfiltered cluster-wide informer. Routing the WATCHED kinds
// through it too would spend a direct API GET per cross-namespace projection on
// every pass — dozens on the dedicated-service-namespace layout — against the
// reconciler's shared client-side rate limit, for a cache hit that is already free.
//
// The two readers are seeded differently so the choice is observable: only the
// cached client holds the foreign ExternalSecret the pre-check exists to refuse.
func TestEnsureUnownedOrOwned_ReadsWatchedKindsThroughTheCache(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	s := namespacesTestScheme(t)
	if err := esov1.AddToScheme(s); err != nil {
		t.Fatalf("adding external-secrets scheme: %v", err)
	}
	cp := namespacedControlPlane()

	foreign := &esov1.ExternalSecret{ObjectMeta: metav1.ObjectMeta{
		Namespace: "identity",
		Name:      "cp-admin-password",
		Labels:    map[string]string{"owner": "someone-else"},
	}}
	direct := &countingReader{Reader: fake.NewClientBuilder().WithScheme(s).Build()}
	r := &ControlPlaneReconciler{
		Client:    fake.NewClientBuilder().WithScheme(s).WithObjects(cp, foreign).Build(),
		Scheme:    s,
		APIReader: direct,
	}

	err := r.ensureUnownedOrOwned(ctx, r.Client, cp, &esov1.ExternalSecret{ObjectMeta: metav1.ObjectMeta{
		Namespace: "identity", Name: "cp-admin-password",
	}})
	g.Expect(err).To(HaveOccurred(), "the informer already holds the foreign object the pre-check must refuse")
	g.Expect(err.Error()).To(ContainSubstring("refusing to adopt pre-existing"))
	g.Expect(direct.gets).To(Equal(0), "a watched kind must not spend a direct API GET on this pre-check")

	g.Expect(r.ensureUnownedOrOwned(ctx, r.Client, cp, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "cp-barbican-bao-auth-delegator"},
	})).To(Succeed())
	g.Expect(direct.gets).To(Equal(1), "an unwatched kind has no informer to read from")
}

// TestEnsureUnownedOrOwned_ConfirmsACacheMissAgainstTheAPIServer covers the one
// answer the informer can get WRONG. A cached Get reports NotFound for as long as
// the watch has not delivered the ADD, and everything past the pre-check is
// destructive on a name that turns out to be taken: the SSA apply overwrites the
// foreign object's spec, and the ownership labels the projection stamps make the
// teardown residue sweep DELETE it — with no error, no condition and no event to
// show for either. So a cached miss is confirmed against the API server before it
// is believed.
//
// The two readers are seeded the other way round from the test above: the foreign
// ExternalSecret is on the API server and the cache has not caught up with it.
func TestEnsureUnownedOrOwned_ConfirmsACacheMissAgainstTheAPIServer(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	s := namespacesTestScheme(t)
	if err := esov1.AddToScheme(s); err != nil {
		t.Fatalf("adding external-secrets scheme: %v", err)
	}
	cp := namespacedControlPlane()

	foreign := &esov1.ExternalSecret{ObjectMeta: metav1.ObjectMeta{
		Namespace: "identity",
		Name:      "cp-admin-password",
		Labels:    map[string]string{"owner": "someone-else"},
	}}
	r := &ControlPlaneReconciler{
		Client:    fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build(),
		Scheme:    s,
		APIReader: fake.NewClientBuilder().WithScheme(s).WithObjects(foreign).Build(),
	}

	err := r.ensureUnownedOrOwned(ctx, r.Client, cp, &esov1.ExternalSecret{ObjectMeta: metav1.ObjectMeta{
		Namespace: "identity", Name: "cp-admin-password",
	}})
	g.Expect(err).To(HaveOccurred(), "a lagging informer must not turn a foreign object into an adoption")
	g.Expect(err.Error()).To(ContainSubstring("refusing to adopt pre-existing"))

	// The confirmation refuses only what is actually there: a name free on BOTH
	// readers is still created.
	g.Expect(r.ensureUnownedOrOwned(ctx, r.Client, cp, &esov1.ExternalSecret{ObjectMeta: metav1.ObjectMeta{
		Namespace: "identity", Name: "cp-db-credentials",
	}})).To(Succeed())
}
