// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for reconcileNamespaceAssignments, the read-only tail-group member that
// reports every spec.namespaceAssignments entry in status.namespaceAssignments
// and drives NamespaceAssignmentsReady.
package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	mcruntime "sigs.k8s.io/multicluster-runtime/pkg/multicluster"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// assignedControlPlane returns a ControlPlane in namespace "openstack" that
// assigns the entries passed in.
func assignedControlPlane(entries ...c5c3v1alpha1.NamespaceAssignmentSpec) *c5c3v1alpha1.ControlPlane {
	return &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: "openstack", Generation: 1, UID: types.UID("cp-uid")},
		Spec:       c5c3v1alpha1.ControlPlaneSpec{NamespaceAssignments: entries},
	}
}

// assignmentsReconciler returns a reconciler whose management cluster holds the
// objects passed in, with no cluster resolver: every entry without a ref reads
// the local client.
func assignmentsReconciler(t *testing.T, objs ...client.Object) (*ControlPlaneReconciler, client.Client) {
	t.Helper()
	s := namespacesTestScheme(t)
	local := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	return &ControlPlaneReconciler{Client: local, Scheme: s}, local
}

func namespaceAssignmentsCondition(cp *c5c3v1alpha1.ControlPlane) *metav1.Condition {
	return conditions.GetCondition(cp.Status.Conditions, conditionTypeNamespaceAssignmentsReady)
}

var edgeCluster = &commonv1.TargetClusterRefSpec{Name: "edge-1"}

// TestReconcileNamespaceAssignments_NoEntries pins the default: an absent and an
// empty list both report True/NoNamespaceAssignments at once and clear any
// status a removed list left behind.
func TestReconcileNamespaceAssignments_NoEntries(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries []c5c3v1alpha1.NamespaceAssignmentSpec
	}{
		{name: "absent", entries: nil},
		{name: "empty", entries: []c5c3v1alpha1.NamespaceAssignmentSpec{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := assignedControlPlane(tc.entries...)
			// What the last pass reported before every entry was removed.
			cp.Status.NamespaceAssignments = []c5c3v1alpha1.NamespaceAssignmentStatus{
				{Namespace: "tenant-a", Reason: c5c3v1alpha1.NamespaceAssignmentTargetClusterUnavailable},
			}
			conditionFailer(cp, conditionTypeNamespaceAssignmentsReady)(commonmulticluster.TargetClusterUnavailable, "stale")
			r, _ := assignmentsReconciler(t)

			res, err := r.reconcileNamespaceAssignments(context.Background(), cp)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(res).To(Equal(ctrl.Result{}))
			g.Expect(cp.Status.NamespaceAssignments).To(BeNil())

			cond := namespaceAssignmentsCondition(cp)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(cond.Reason).To(Equal("NoNamespaceAssignments"))
			g.Expect(cond.Message).To(Equal("no namespace is assigned to a service owner"))
		})
	}
}

// TestReconcileNamespaceAssignments_ExistingNamespace reports an assigned
// namespace that exists on a reachable cluster, with its roles echoed.
func TestReconcileNamespaceAssignments_ExistingNamespace(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := assignedControlPlane(c5c3v1alpha1.NamespaceAssignmentSpec{
		Namespace: "tenant-a", AllowedRoles: []string{"member", "reader"},
	})
	r, _ := assignmentsReconciler(t, existingNamespace("tenant-a", nil))

	res, err := r.reconcileNamespaceAssignments(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))

	g.Expect(cp.Status.NamespaceAssignments).To(Equal([]c5c3v1alpha1.NamespaceAssignmentStatus{{
		Namespace:        "tenant-a",
		AllowedRoles:     []string{"member", "reader"},
		ClusterReachable: true,
		NamespaceExists:  true,
		Reason:           c5c3v1alpha1.NamespaceAssignmentAssigned,
	}}))

	cond := namespaceAssignmentsCondition(cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("NamespaceAssignmentsReady"))
	g.Expect(cond.Message).To(Equal("all 1 namespace assignment(s) resolve to a reachable cluster"))
}

// TestReconcileNamespaceAssignments_MissingNamespace keeps the condition True
// for a namespace that does not exist yet, because an assignment may precede
// it, and requeues so the namespace shows up once it is created. The step
// creates nothing.
func TestReconcileNamespaceAssignments_MissingNamespace(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cp := assignedControlPlane(c5c3v1alpha1.NamespaceAssignmentSpec{Namespace: "tenant-a"})
	r, local := assignmentsReconciler(t)

	res, err := r.reconcileNamespaceAssignments(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(time.Minute))

	g.Expect(cp.Status.NamespaceAssignments).To(HaveLen(1))
	entry := cp.Status.NamespaceAssignments[0]
	g.Expect(entry.ClusterReachable).To(BeTrue())
	g.Expect(entry.NamespaceExists).To(BeFalse())
	g.Expect(entry.Reason).To(Equal(c5c3v1alpha1.NamespaceAssignmentNamespaceNotFound))
	g.Expect(entry.Message).To(ContainSubstring(`namespace "tenant-a" does not exist on the management cluster`))

	cond := namespaceAssignmentsCondition(cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("NamespaceAssignmentsReady"))
	g.Expect(cond.Message).To(ContainSubstring("1 assigned namespace(s) do not exist yet"))

	err = local.Get(ctx, types.NamespacedName{Name: "tenant-a"}, &corev1.Namespace{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the step must never create an assigned namespace")
}

// TestReconcileNamespaceAssignments_TerminatingNamespace reports a namespace
// that is being deleted as existing but Terminating.
func TestReconcileNamespaceAssignments_TerminatingNamespace(t *testing.T) {
	g := NewGomegaWithT(t)
	now := metav1.Now()
	terminating := existingNamespace("tenant-a", nil)
	// The fake client accepts a deletion timestamp only beside a finalizer.
	terminating.DeletionTimestamp = &now
	terminating.Finalizers = []string{"example.com/hold"}
	cp := assignedControlPlane(c5c3v1alpha1.NamespaceAssignmentSpec{Namespace: "tenant-a"})
	r, _ := assignmentsReconciler(t, terminating)

	res, err := r.reconcileNamespaceAssignments(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(namespaceAssignmentRequeueAfter))

	entry := cp.Status.NamespaceAssignments[0]
	g.Expect(entry.ClusterReachable).To(BeTrue())
	g.Expect(entry.NamespaceExists).To(BeTrue())
	g.Expect(entry.Reason).To(Equal(c5c3v1alpha1.NamespaceAssignmentNamespaceTerminating))
	g.Expect(namespaceAssignmentsCondition(cp).Status).To(Equal(metav1.ConditionTrue))
}

// TestReconcileNamespaceAssignments_UnknownCluster refuses an entry whose
// cluster does not resolve in status, without an error, and the refusal turns
// the aggregate Ready False.
func TestReconcileNamespaceAssignments_UnknownCluster(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := assignedControlPlane(c5c3v1alpha1.NamespaceAssignmentSpec{
		Namespace: "tenant-a", TargetClusterRef: edgeCluster, AllowedRoles: []string{"member"},
	})
	r, _ := assignmentsReconciler(t)
	r.Resolver = &childrenResolver{err: mcruntime.ErrClusterNotFound}

	res, err := r.reconcileNamespaceAssignments(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(time.Minute))

	entry := cp.Status.NamespaceAssignments[0]
	g.Expect(entry.TargetClusterRef).To(Equal(edgeCluster))
	g.Expect(entry.AllowedRoles).To(Equal([]string{"member"}))
	g.Expect(entry.ClusterReachable).To(BeFalse())
	g.Expect(entry.NamespaceExists).To(BeFalse())
	g.Expect(entry.Reason).To(Equal(c5c3v1alpha1.NamespaceAssignmentTargetClusterUnavailable))
	g.Expect(entry.Message).To(Equal(mcruntime.ErrClusterNotFound.Error()))

	cond := namespaceAssignmentsCondition(cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(commonmulticluster.TargetClusterUnavailable))
	g.Expect(cond.Message).To(ContainSubstring(`tenant-a on target cluster "edge-1"`))
	g.Expect(cond.Message).To(ContainSubstring(
		"orders from these namespaces are refused until the cluster resolves or the entry is removed"))

	// Every other sub-condition True: the refusal alone keeps Ready False.
	for _, condType := range subConditionTypes {
		if condType != conditionTypeNamespaceAssignmentsReady {
			conditions.SetCondition(&cp.Status.Conditions, trueCondition(condType))
		}
	}
	setReadyCondition(cp)
	g.Expect(conditions.GetCondition(cp.Status.Conditions, conditionTypeReady).Status).To(Equal(metav1.ConditionFalse))
}

// TestReconcileNamespaceAssignments_ForbiddenRead reports a cluster that
// answers the namespace read with an error other than NotFound as unreachable.
func TestReconcileNamespaceAssignments_ForbiddenRead(t *testing.T) {
	g := NewGomegaWithT(t)
	s := namespacesTestScheme(t)
	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, "tenant-a", errors.New("rbac"))
	local := fake.NewClientBuilder().WithScheme(s).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.Namespace); ok {
				return forbidden
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}).Build()
	r := &ControlPlaneReconciler{Client: local, Scheme: s}
	cp := assignedControlPlane(c5c3v1alpha1.NamespaceAssignmentSpec{Namespace: "tenant-a"})

	res, err := r.reconcileNamespaceAssignments(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(namespaceAssignmentRequeueAfter))

	entry := cp.Status.NamespaceAssignments[0]
	g.Expect(entry.ClusterReachable).To(BeFalse())
	g.Expect(entry.Reason).To(Equal(c5c3v1alpha1.NamespaceAssignmentClusterUnreachable))
	g.Expect(entry.Message).To(ContainSubstring(`getting namespace "tenant-a"`))

	cond := namespaceAssignmentsCondition(cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("ClusterUnreachable"))
	g.Expect(cond.Message).To(ContainSubstring("tenant-a on the management cluster"))
}

// TestReconcileNamespaceAssignments_UnresolvedOutranksUnreachable pins the
// reason when both failures occur: an unresolved cluster reports
// TargetClusterUnavailable, and the message still lists the unreachable entry.
func TestReconcileNamespaceAssignments_UnresolvedOutranksUnreachable(t *testing.T) {
	g := NewGomegaWithT(t)
	s := namespacesTestScheme(t)
	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, "tenant-b", errors.New("rbac"))
	local := fake.NewClientBuilder().WithScheme(s).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.Namespace); ok {
				return forbidden
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}).Build()
	r := &ControlPlaneReconciler{
		Client: local, Scheme: s,
		Resolver: &childrenResolver{errNames: map[string]error{"edge-1": mcruntime.ErrClusterNotFound}},
	}
	cp := assignedControlPlane(
		c5c3v1alpha1.NamespaceAssignmentSpec{Namespace: "tenant-a", TargetClusterRef: edgeCluster},
		c5c3v1alpha1.NamespaceAssignmentSpec{Namespace: "tenant-b"},
	)

	_, err := r.reconcileNamespaceAssignments(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	cond := namespaceAssignmentsCondition(cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(commonmulticluster.TargetClusterUnavailable))
	g.Expect(cond.Message).To(ContainSubstring(`tenant-a on target cluster "edge-1"`))
	g.Expect(cond.Message).To(ContainSubstring("tenant-b on the management cluster"))
}

// TestReconcileNamespaceAssignments_UnansweredClusterIsReadOnce pins that a
// cluster that does not answer a namespace read costs one bounded read per
// pass: its later entries report the same error without a read of their own,
// and an entry on another cluster is still read.
func TestReconcileNamespaceAssignments_UnansweredClusterIsReadOnce(t *testing.T) {
	g := NewGomegaWithT(t)
	s := namespacesTestScheme(t)
	var reads []string
	unbounded := false
	edge := fake.NewClientBuilder().WithScheme(s).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.Namespace); ok {
				reads = append(reads, key.Name)
				if _, bounded := ctx.Deadline(); !bounded {
					unbounded = true
				}
				return errors.New("dial tcp 192.0.2.1:6443: i/o timeout")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}).Build()
	r, _ := assignmentsReconciler(t, existingNamespace("tenant-c", nil))
	r.Resolver = &childrenResolver{children: edge}
	cp := assignedControlPlane(
		c5c3v1alpha1.NamespaceAssignmentSpec{Namespace: "tenant-a", TargetClusterRef: edgeCluster},
		c5c3v1alpha1.NamespaceAssignmentSpec{Namespace: "tenant-b", TargetClusterRef: edgeCluster},
		c5c3v1alpha1.NamespaceAssignmentSpec{Namespace: "tenant-c"},
	)

	_, err := r.reconcileNamespaceAssignments(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(reads).To(Equal([]string{"tenant-a"}), "a cluster that did not answer is not asked again in the same pass")
	g.Expect(unbounded).To(BeFalse(), "every namespace read carries a deadline")

	g.Expect(cp.Status.NamespaceAssignments).To(HaveLen(3))
	first, second := cp.Status.NamespaceAssignments[0], cp.Status.NamespaceAssignments[1]
	g.Expect(first.Reason).To(Equal(c5c3v1alpha1.NamespaceAssignmentClusterUnreachable))
	g.Expect(first.Message).To(ContainSubstring(`getting namespace "tenant-a": dial tcp`))
	g.Expect(second.Reason).To(Equal(c5c3v1alpha1.NamespaceAssignmentClusterUnreachable))
	g.Expect(second.ClusterReachable).To(BeFalse())
	g.Expect(second.Message).To(ContainSubstring(
		`namespace "tenant-b" not read: target cluster "edge-1" did not answer an earlier read in this pass: dial tcp`))
	g.Expect(cp.Status.NamespaceAssignments[2].Reason).To(Equal(c5c3v1alpha1.NamespaceAssignmentAssigned))

	cond := namespaceAssignmentsCondition(cp)
	g.Expect(cond.Reason).To(Equal("ClusterUnreachable"))
	g.Expect(cond.Message).To(ContainSubstring(`tenant-a on target cluster "edge-1", tenant-b on target cluster "edge-1"`))
}

// TestReconcileNamespaceAssignments_AnsweredErrorIsReadPerEntry pins that an
// API error does not stand in for the cluster's other entries: the cluster
// answered, and its answer may differ per namespace.
func TestReconcileNamespaceAssignments_AnsweredErrorIsReadPerEntry(t *testing.T) {
	g := NewGomegaWithT(t)
	s := namespacesTestScheme(t)
	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, "tenant-a", errors.New("rbac"))
	local := fake.NewClientBuilder().WithScheme(s).WithObjects(existingNamespace("tenant-b", nil)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.Namespace); ok && key.Name == "tenant-a" {
					return forbidden
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).Build()
	r := &ControlPlaneReconciler{Client: local, Scheme: s}
	cp := assignedControlPlane(
		c5c3v1alpha1.NamespaceAssignmentSpec{Namespace: "tenant-a"},
		c5c3v1alpha1.NamespaceAssignmentSpec{Namespace: "tenant-b"},
	)

	_, err := r.reconcileNamespaceAssignments(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(cp.Status.NamespaceAssignments[0].Reason).To(Equal(c5c3v1alpha1.NamespaceAssignmentClusterUnreachable))
	g.Expect(cp.Status.NamespaceAssignments[1].Reason).To(Equal(c5c3v1alpha1.NamespaceAssignmentAssigned))
}

// TestReconcileNamespaceAssignments_MixedEntries pins that one unresolved entry
// fails the condition while its healthy peer still reads Assigned, in spec
// order.
func TestReconcileNamespaceAssignments_MixedEntries(t *testing.T) {
	g := NewGomegaWithT(t)
	s := namespacesTestScheme(t)
	target := fake.NewClientBuilder().WithScheme(s).Build()
	r, _ := assignmentsReconciler(t, existingNamespace("tenant-b", nil))
	r.Resolver = &childrenResolver{children: target, errNames: map[string]error{"edge-1": mcruntime.ErrClusterNotFound}}
	cp := assignedControlPlane(
		c5c3v1alpha1.NamespaceAssignmentSpec{Namespace: "tenant-a", TargetClusterRef: edgeCluster},
		c5c3v1alpha1.NamespaceAssignmentSpec{Namespace: "tenant-b"},
	)

	_, err := r.reconcileNamespaceAssignments(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(cp.Status.NamespaceAssignments).To(HaveLen(2))
	g.Expect(cp.Status.NamespaceAssignments[0].Namespace).To(Equal("tenant-a"))
	g.Expect(cp.Status.NamespaceAssignments[0].Reason).To(Equal(c5c3v1alpha1.NamespaceAssignmentTargetClusterUnavailable))
	g.Expect(cp.Status.NamespaceAssignments[1].Namespace).To(Equal("tenant-b"))
	g.Expect(cp.Status.NamespaceAssignments[1].Reason).To(Equal(c5c3v1alpha1.NamespaceAssignmentAssigned))

	cond := namespaceAssignmentsCondition(cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(commonmulticluster.TargetClusterUnavailable))
	g.Expect(cond.Message).NotTo(ContainSubstring("tenant-b"))
}

// TestReconcileNamespaceAssignments_TargetClusterReadsLive pins that a target
// cluster's namespace is read through its uncached reader, not through the
// cached client, so a namespace created moments ago is not reported missing.
func TestReconcileNamespaceAssignments_TargetClusterReadsLive(t *testing.T) {
	g := NewGomegaWithT(t)
	s := namespacesTestScheme(t)
	cached := fake.NewClientBuilder().WithScheme(s).Build()
	live := fake.NewClientBuilder().WithScheme(s).WithObjects(existingNamespace("tenant-a", nil)).Build()
	resolver := &childrenResolver{children: cached, reader: live}
	r, _ := assignmentsReconciler(t)
	r.Resolver = resolver
	cp := assignedControlPlane(c5c3v1alpha1.NamespaceAssignmentSpec{Namespace: "tenant-a", TargetClusterRef: edgeCluster})

	res, err := r.reconcileNamespaceAssignments(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))
	g.Expect(resolver.names).To(Equal([]mcruntime.ClusterName{"edge-1"}))
	g.Expect(cp.Status.NamespaceAssignments[0].Reason).To(Equal(c5c3v1alpha1.NamespaceAssignmentAssigned))
	g.Expect(cp.Status.NamespaceAssignments[0].NamespaceExists).To(BeTrue())
}

// TestReconcileNamespaceAssignments_ManagementEntryCostsNoLookup pins that a
// management-cluster entry costs no resolver lookup.
func TestReconcileNamespaceAssignments_ManagementEntryCostsNoLookup(t *testing.T) {
	g := NewGomegaWithT(t)
	resolver := &childrenResolver{err: errors.New("must not be asked")}
	r, _ := assignmentsReconciler(t, existingNamespace("tenant-a", nil))
	r.Resolver = resolver
	cp := assignedControlPlane(c5c3v1alpha1.NamespaceAssignmentSpec{Namespace: "tenant-a"})

	_, err := r.reconcileNamespaceAssignments(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(resolver.names).To(BeEmpty())
	g.Expect(cp.Status.NamespaceAssignments[0].Reason).To(Equal(c5c3v1alpha1.NamespaceAssignmentAssigned))
}
