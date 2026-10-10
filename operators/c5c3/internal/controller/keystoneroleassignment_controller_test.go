// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the KeystoneRoleAssignment reconciler: the role allowlist, the
// reference gates, the duplicate rule, the projection and its waits, the
// teardown, and the watches that reach an assignment from the orders it names
// and from its ControlPlane.
package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	mcruntime "sigs.k8s.io/multicluster-runtime/pkg/multicluster"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

const kraTestName = "workflow-member"

// keystoneRoleAssignmentCR returns the order "workflow-member" in
// kuTestNamespace, assigning member to the user "workflow" on the project
// "workflow-project", and already carrying the teardown finalizer.
func keystoneRoleAssignmentCR() *c5c3v1alpha1.KeystoneRoleAssignment {
	return &c5c3v1alpha1.KeystoneRoleAssignment{
		ObjectMeta: metav1.ObjectMeta{
			Name:              kraTestName,
			Namespace:         kuTestNamespace,
			Generation:        1,
			UID:               types.UID("kra-uid"),
			Finalizers:        []string{keystoneRoleAssignmentFinalizerName},
			CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Hour)),
		},
		Spec: c5c3v1alpha1.KeystoneRoleAssignmentSpec{
			ControlPlaneRef: c5c3v1alpha1.ControlPlaneRefSpec{Name: "cp", Namespace: "default"},
			UserRef:         c5c3v1alpha1.KeystoneOrderRef{Name: kuTestName},
			ProjectRef:      c5c3v1alpha1.KeystoneOrderRef{Name: kpTestName},
			Role:            "member",
		},
	}
}

// kraReadyReferences returns the user and project orders the fixture names,
// both provisioned.
func kraReadyReferences() (*c5c3v1alpha1.KeystoneUser, *c5c3v1alpha1.KeystoneProject) {
	user := keystoneUserCR(kuTestNamespace)
	user.Status.Conditions = []metav1.Condition{{
		Type: conditionTypeKeystoneUserUserReady, Status: metav1.ConditionTrue, Reason: reasonKeystoneUserProvisioned,
		LastTransitionTime: metav1.Now(),
	}}
	project := keystoneProjectCR()
	project.Status.Conditions = []metav1.Condition{{
		Type: conditionTypeKeystoneProjectProjectReady, Status: metav1.ConditionTrue,
		Reason: reasonKeystoneProjectProvisioned, LastTransitionTime: metav1.Now(),
	}}
	return user, project
}

// kraControlPlane assigns kuTestNamespace on cluster with allowedRoles roles.
func kraControlPlane(cluster string, roles ...string) *c5c3v1alpha1.ControlPlane {
	entry := assignOn(kuTestNamespace, cluster)
	entry.AllowedRoles = roles
	return keystoneUserControlPlane(entry)
}

func newKRAHarness(t *testing.T, cluster string, mgmtFuncs, orderFuncs *interceptor.Funcs, objs ...client.Object) *orderHarness {
	t.Helper()
	return newOrderHarness(t, cluster, types.NamespacedName{Namespace: kuTestNamespace, Name: kraTestName},
		mgmtFuncs, orderFuncs, objs, func(mgmt client.Client, resolver commonmulticluster.ClusterResolver) orderReconciler {
			return &KeystoneRoleAssignmentReconciler{Client: mgmt, Scheme: mgmt.Scheme(), Resolver: resolver}
		})
}

func kraGet(t *testing.T, h *orderHarness) *c5c3v1alpha1.KeystoneRoleAssignment {
	t.Helper()
	return reloadOrder[c5c3v1alpha1.KeystoneRoleAssignment](t, h)
}

func kraCondition(order *c5c3v1alpha1.KeystoneRoleAssignment) *metav1.Condition {
	return conditions.GetCondition(order.Status.Conditions, conditionTypeKeystoneRoleAssignmentAssignmentReady)
}

// kraLabelled counts the Roles and RoleAssignments in the ControlPlane's
// namespace that carry the order's name label.
func kraLabelled(t *testing.T, c client.Client) int {
	t.Helper()
	key := keystoneRoleAssignmentLabelKeys.Name
	return labelledIn(t, c, &orcv1alpha1.RoleList{}, key, kraTestName) +
		labelledIn(t, c, &orcv1alpha1.RoleAssignmentList{}, key, kraTestName)
}

// kraConvergedChildren returns the order's Role import and RoleAssignment as
// K-ORC reports them once the role is assigned.
func kraConvergedChildren(order *c5c3v1alpha1.KeystoneRoleAssignment, cluster string) []client.Object {
	labels := keystoneRoleAssignmentRef(order, cluster).childLabels()
	role := unmanagedRoleImport(keystoneRoleAssignmentRoleRef(order, cluster), "default", order.Spec.Role,
		orcv1alpha1.CloudCredentialsReference{})
	role.Labels = labels
	role.Status = orcv1alpha1.RoleStatus{Conditions: availableImportConditions(), ID: ptr.To("r-1")}
	assignment := &orcv1alpha1.RoleAssignment{
		ObjectMeta: metav1.ObjectMeta{
			Name: keystoneRoleAssignmentAssignmentRef(order, cluster), Namespace: "default", Labels: labels,
		},
		Status: orcv1alpha1.RoleAssignmentStatus{
			Conditions: availableImportConditions(),
			Resource:   &orcv1alpha1.RoleAssignmentResourceStatus{RoleID: "r-1", UserID: "u-1", ProjectID: "p-1"},
		},
	}
	return []client.Object{role, assignment}
}

func TestKeystoneRoleAssignment_RoleOutsideTheAllowlistIsRefused(t *testing.T) {
	cases := []struct {
		name    string
		roles   []string
		role    string
		wantSub []string
	}{
		{
			name: "a role off the list", roles: []string{"member"}, role: "service",
			wantSub: []string{"spec.namespaceAssignments", `["member"]`, `["service"]`},
		},
		{name: "no allowedRoles", role: "member", wantSub: []string{"admits roles []", `["member"]`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			order := keystoneRoleAssignmentCR()
			order.Spec.Role = tc.role
			user, project := kraReadyReferences()
			h := newKRAHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil,
				order, user, project, kraControlPlane("", tc.roles...))

			result, err := h.reconcile(context.Background())

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(result.RequeueAfter).To(Equal(10 * time.Minute))
			cond := kraCondition(kraGet(t, h))
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(reasonKeystoneServiceRoleNotAllowed))
			for _, sub := range tc.wantSub {
				g.Expect(cond.Message).To(ContainSubstring(sub))
			}
			g.Expect(kraLabelled(t, h.mgmt)).To(BeZero())
		})
	}
}

func TestKeystoneRoleAssignment_ReferenceGates(t *testing.T) {
	cases := []struct {
		name       string
		objs       func() []client.Object
		wantReason string
		wantSub    []string
	}{
		{
			name: "no user",
			objs: func() []client.Object {
				_, project := kraReadyReferences()
				return []client.Object{project}
			},
			wantReason: reasonKeystoneRoleAssignmentUserNotFound,
			wantSub:    []string{`KeystoneUser "workflow"`, `namespace "tenant-a"`},
		},
		{
			name: "no project",
			objs: func() []client.Object {
				user, _ := kraReadyReferences()
				return []client.Object{user}
			},
			wantReason: reasonKeystoneRoleAssignmentProjectNotFound,
			wantSub:    []string{`KeystoneProject "workflow-project"`},
		},
		{
			name: "a user of another ControlPlane",
			objs: func() []client.Object {
				user, project := kraReadyReferences()
				user.Spec.ControlPlaneRef = c5c3v1alpha1.ControlPlaneRefSpec{Name: "other", Namespace: "default"}
				return []client.Object{user, project}
			},
			wantReason: reasonKeystoneRoleAssignmentControlPlaneMismatch,
			wantSub:    []string{"ControlPlane default/other", "this order from default/cp"},
		},
		{
			name: "a user not provisioned yet",
			objs: func() []client.Object {
				user, project := kraReadyReferences()
				user.Status.Conditions[0].Status = metav1.ConditionFalse
				user.Status.Conditions[0].Reason = reasonProbingForCollision
				return []client.Object{user, project}
			},
			wantReason: reasonKeystoneRoleAssignmentWaitingForUser,
			wantSub:    []string{"not provisioned yet (ProbingForCollision)"},
		},
		{
			name: "a project not provisioned yet",
			objs: func() []client.Object {
				user, project := kraReadyReferences()
				project.Status.Conditions = nil
				return []client.Object{user, project}
			},
			wantReason: reasonKeystoneProjectWaiting,
			wantSub:    []string{`KeystoneProject "workflow-project" is not provisioned yet`},
		},
	}
	for _, tc := range cases {
		for _, cluster := range []string{c5c3v1alpha1.ManagementCluster, kuTestCluster} {
			t.Run(tc.name+" on "+orderRef{Cluster: cluster}.location(), func(t *testing.T) {
				g := NewGomegaWithT(t)
				objs := append([]client.Object{keystoneRoleAssignmentCR(), kraControlPlane(cluster, "member")}, tc.objs()...)
				h := newKRAHarness(t, cluster, nil, nil, objs...)

				result, err := h.reconcile(context.Background())

				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(result.RequeueAfter).To(Equal(10 * time.Minute))
				cond := kraCondition(kraGet(t, h))
				g.Expect(cond.Reason).To(Equal(tc.wantReason))
				for _, sub := range tc.wantSub {
					g.Expect(cond.Message).To(ContainSubstring(sub))
				}
				if tc.wantReason == reasonKeystoneRoleAssignmentUserNotFound {
					g.Expect(cond.Message).To(ContainSubstring(orderRef{Cluster: cluster}.location()))
				}
				g.Expect(kraLabelled(t, h.mgmt)).To(BeZero(), "a refused order projects nothing")
			})
		}
	}
}

func TestKeystoneRoleAssignment_ReferenceReadErrorIsWrapped(t *testing.T) {
	g := NewGomegaWithT(t)

	boom := errors.New("boom")
	user, project := kraReadyReferences()
	h := newKRAHarness(t, kuTestCluster, nil, &interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*c5c3v1alpha1.KeystoneUser); ok {
				return boom
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	}, keystoneRoleAssignmentCR(), user, project, kraControlPlane(kuTestCluster, "member"))

	_, err := h.reconcile(context.Background())

	g.Expect(err).To(MatchError(boom))
	g.Expect(err.Error()).To(ContainSubstring("reading referenced KeystoneUser tenant-a/workflow"))
}

func TestReferencingRoleAssignments(t *testing.T) {
	assignment := func(name, user string) *c5c3v1alpha1.KeystoneRoleAssignment {
		ra := keystoneRoleAssignmentCR()
		ra.Name = name
		ra.Spec.UserRef.Name = user
		return ra
	}
	byUser := func(ra *c5c3v1alpha1.KeystoneRoleAssignment) bool { return ra.Spec.UserRef.Name == kuTestName }

	t.Run("matching names are sorted", func(t *testing.T) {
		g := NewGomegaWithT(t)
		c := orderFakeClient(t, nil, assignment("b-reader", kuTestName), assignment("a-member", kuTestName),
			assignment("other", "someone-else"))
		names, err := referencingRoleAssignments(context.Background(), c, kuTestNamespace, byUser)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(names).To(Equal([]string{"a-member", "b-reader"}))
	})

	t.Run("a cluster that does not serve the kind has none", func(t *testing.T) {
		g := NewGomegaWithT(t)
		c := orderFakeClient(t, &interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return &meta.NoKindMatchError{GroupKind: keystoneRoleAssignmentGVK.GroupKind()}
			},
		})
		names, err := referencingRoleAssignments(context.Background(), c, kuTestNamespace, byUser)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(names).To(BeNil())
	})

	t.Run("a list error is wrapped", func(t *testing.T) {
		g := NewGomegaWithT(t)
		boom := errors.New("boom")
		c := orderFakeClient(t, &interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error { return boom },
		})
		_, err := referencingRoleAssignments(context.Background(), c, kuTestNamespace, byUser)
		g.Expect(err).To(MatchError(boom))
		g.Expect(err.Error()).To(ContainSubstring(`listing KeystoneRoleAssignments in "tenant-a"`))
	})
}

func TestKeystoneRoleAssignment_YoungerDuplicateIsRefused(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	older := keystoneRoleAssignmentCR()
	older.Name = "older"
	older.CreationTimestamp = metav1.NewTime(time.Now().Add(-2 * time.Hour))
	user, project := kraReadyReferences()
	h := newKRAHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil,
		keystoneRoleAssignmentCR(), older, user, project, kraControlPlane("", "member"))

	result, err := h.reconcile(ctx)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(10 * time.Minute))
	cond := kraCondition(kraGet(t, h))
	g.Expect(cond.Reason).To(Equal(reasonKeystoneRoleAssignmentDuplicate))
	g.Expect(cond.Message).To(ContainSubstring(`KeystoneRoleAssignment "older" already assigns role "member"`))
	g.Expect(kraLabelled(t, h.mgmt)).To(BeZero())

	h.key.Name = "older"
	_, err = h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	got := kraGet(t, h)
	g.Expect(kraCondition(got).Reason).NotTo(Equal(reasonKeystoneRoleAssignmentDuplicate), "the older order is unaffected")
	g.Expect(labelledIn(t, h.mgmt, &orcv1alpha1.RoleAssignmentList{}, keystoneRoleAssignmentLabelKeys.Name, "older")).
		To(Equal(1))
}

func TestOlderDuplicateRoleAssignment(t *testing.T) {
	g := NewGomegaWithT(t)

	order := keystoneRoleAssignmentCR()
	sibling := func(name string, age time.Duration, mutate func(*c5c3v1alpha1.KeystoneRoleAssignment)) c5c3v1alpha1.KeystoneRoleAssignment {
		ra := keystoneRoleAssignmentCR()
		ra.Name = name
		ra.CreationTimestamp = metav1.NewTime(order.CreationTimestamp.Add(-age))
		if mutate != nil {
			mutate(ra)
		}
		return *ra
	}
	g.Expect(olderDuplicateRoleAssignment(order, nil)).To(BeEmpty())
	g.Expect(olderDuplicateRoleAssignment(order, []c5c3v1alpha1.KeystoneRoleAssignment{
		*order,
		sibling("younger", -time.Minute, nil),
		sibling("another-role", time.Hour, func(ra *c5c3v1alpha1.KeystoneRoleAssignment) { ra.Spec.Role = "reader" }),
	})).To(BeEmpty(), "neither a younger order nor another tuple is a duplicate")
	g.Expect(olderDuplicateRoleAssignment(order, []c5c3v1alpha1.KeystoneRoleAssignment{
		sibling("old", time.Minute, nil), sibling("oldest", time.Hour, nil),
	})).To(Equal("oldest"))
	g.Expect(olderDuplicateRoleAssignment(order, []c5c3v1alpha1.KeystoneRoleAssignment{
		sibling("a-same-age", 0, nil), sibling("z-same-age", 0, nil),
	})).To(Equal("a-same-age"), "a tie is broken by name order")
}

func TestKeystoneRoleAssignment_ProjectsTheRoleAndTheAssignment(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	order := keystoneRoleAssignmentCR()
	user, project := kraReadyReferences()
	h := newKRAHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil, order, user, project, kraControlPlane("", "member"))

	result, err := h.reconcile(ctx)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(korcRequeueAfter))
	g.Expect(kraCondition(kraGet(t, h)).Reason).To(Equal(reasonWaitingForServiceAccounts))

	prefix := keystoneRoleAssignmentRef(order, "").childPrefix()
	g.Expect(prefix).To(MatchRegexp(`^workflow-member-[0-9a-f]{8}-roleassignment-$`))
	labels := keystoneRoleAssignmentRef(order, "").childLabels()

	role := &orcv1alpha1.Role{}
	g.Expect(h.mgmt.Get(ctx, types.NamespacedName{Namespace: "default", Name: prefix + "role"}, role)).To(Succeed())
	g.Expect(role.Spec.ManagementPolicy).To(Equal(orcv1alpha1.ManagementPolicyUnmanaged))
	g.Expect(string(*role.Spec.Import.Filter.Name)).To(Equal("member"))
	g.Expect(role.Labels).To(Equal(labels))
	g.Expect(role.OwnerReferences).To(BeEmpty())

	assignment := &orcv1alpha1.RoleAssignment{}
	g.Expect(h.mgmt.Get(ctx, types.NamespacedName{Namespace: "default", Name: prefix + "assignment"}, assignment)).To(Succeed())
	g.Expect(assignment.Spec.ManagementPolicy).To(Equal(orcv1alpha1.ManagementPolicyManaged))
	g.Expect(string(assignment.Spec.Resource.RoleRef)).To(Equal(prefix + "role"))
	g.Expect(string(*assignment.Spec.Resource.UserRef)).To(MatchRegexp(`^workflow-[0-9a-f]{8}-user-user$`))
	g.Expect(string(*assignment.Spec.Resource.UserRef)).To(Equal(keystoneUserUserRef(user, "")))
	g.Expect(string(*assignment.Spec.Resource.ProjectRef)).To(MatchRegexp(`^workflow-project-[0-9a-f]{8}-project-project$`))
	g.Expect(string(*assignment.Spec.Resource.ProjectRef)).To(Equal(keystoneProjectProjectRef(project, "")))
	_, managedCredRef := keystoneServiceCredentialRefs(kraControlPlane("", "member"))
	g.Expect(assignment.Spec.CloudCredentialsRef).To(Equal(managedCredRef))
	g.Expect(assignment.Labels).To(Equal(labels))
}

func TestKeystoneRoleAssignment_KORCOutcomes(t *testing.T) {
	t.Run("a stalled Role import", func(t *testing.T) {
		g := NewGomegaWithT(t)
		order := keystoneRoleAssignmentCR()
		children := kraConvergedChildren(order, "")
		children[0].(*orcv1alpha1.Role).Status.Conditions = pendingImportConditions(2 * externalImportStallGrace)
		user, project := kraReadyReferences()
		h := newKRAHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil,
			append([]client.Object{order, user, project, kraControlPlane("", "member")}, children...)...)

		result, err := h.reconcile(context.Background())

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(result.RequeueAfter).To(Equal(korcRequeueAfter))
		cond := kraCondition(kraGet(t, h))
		g.Expect(cond.Reason).To(Equal(reasonWaitingForServiceAccounts))
		g.Expect(cond.Message).To(ContainSubstring("may not exist in Keystone"))
	})

	t.Run("a terminal error", func(t *testing.T) {
		g := NewGomegaWithT(t)
		order := keystoneRoleAssignmentCR()
		children := kraConvergedChildren(order, "")
		children[1].(*orcv1alpha1.RoleAssignment).Status.Conditions = terminalImportConditions("no such user")
		user, project := kraReadyReferences()
		h := newKRAHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil,
			append([]client.Object{order, user, project, kraControlPlane("", "member")}, children...)...)

		result, err := h.reconcile(context.Background())

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(result.RequeueAfter).To(Equal(korcRequeueAfter))
		g.Expect(kraCondition(kraGet(t, h)).Reason).To(Equal(reasonKeystoneRoleAssignmentFailed))
	})

	t.Run("an Available assignment", func(t *testing.T) {
		g := NewGomegaWithT(t)
		order := keystoneRoleAssignmentCR()
		user, project := kraReadyReferences()
		h := newKRAHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil,
			append([]client.Object{order, user, project, kraControlPlane("", "member")}, kraConvergedChildren(order, "")...)...)

		result, err := h.reconcile(context.Background())

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(result.IsZero()).To(BeTrue())
		got := kraGet(t, h)
		cond := kraCondition(got)
		g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(cond.Reason).To(Equal(reasonKeystoneRoleAssignmentAssigned))
		g.Expect(cond.Message).To(Equal(`role "member" is assigned to user "workflow" on project "workflow-project"`))
		g.Expect(got.Status.RoleID).To(Equal("r-1"))
		g.Expect(got.Status.UserID).To(Equal("u-1"))
		g.Expect(got.Status.ProjectID).To(Equal("p-1"))
	})

	t.Run("an apply error", func(t *testing.T) {
		g := NewGomegaWithT(t)
		order := keystoneRoleAssignmentCR()
		user, project := kraReadyReferences()
		stranger := &orcv1alpha1.Role{ObjectMeta: metav1.ObjectMeta{
			Name: keystoneRoleAssignmentRoleRef(order, ""), Namespace: "default",
		}}
		h := newKRAHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil,
			order, user, project, stranger, kraControlPlane("", "member"))

		_, err := h.reconcile(context.Background())

		g.Expect(err).To(MatchError(ContainSubstring("it was not created by this order")))
		g.Expect(kraCondition(kraGet(t, h)).Reason).To(Equal(reasonKeystoneRoleAssignmentError))
	})
}

// TestKeystoneRoleAssignment_RoleTakenOffTheListFreezes pins #1327 D2 for an
// assignment: a role removed from allowedRoles refuses the order and leaves
// the assignment in place.
func TestKeystoneRoleAssignment_RoleTakenOffTheListFreezes(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	order := keystoneRoleAssignmentCR()
	cp := kraControlPlane("", "member")
	user, project := kraReadyReferences()
	h := newKRAHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil,
		append([]client.Object{order, user, project, cp}, kraConvergedChildren(order, "")...)...)
	_, err := h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(kraCondition(kraGet(t, h)).Status).To(Equal(metav1.ConditionTrue))

	live := &c5c3v1alpha1.ControlPlane{}
	g.Expect(h.mgmt.Get(ctx, client.ObjectKeyFromObject(cp), live)).To(Succeed())
	live.Spec.NamespaceAssignments[0].AllowedRoles = nil
	g.Expect(h.mgmt.Update(ctx, live)).To(Succeed())

	_, err = h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(kraCondition(kraGet(t, h)).Reason).To(Equal(reasonKeystoneServiceRoleNotAllowed))
	g.Expect(kraLabelled(t, h.mgmt)).To(Equal(2), "a frozen assignment revokes nothing")
}

func TestKeystoneRoleAssignment_DeleteRemovesTheAssignmentThenTheRole(t *testing.T) {
	for _, tc := range []struct {
		name string
		cp   *c5c3v1alpha1.ControlPlane
	}{
		{name: "an admitted order", cp: kraControlPlane("", "member")},
		{name: "a frozen order", cp: keystoneUserControlPlane()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			ctx := context.Background()

			order := keystoneRoleAssignmentCR()
			order.DeletionTimestamp = ptr.To(metav1.Now())
			children := kraConvergedChildren(order, "")
			children[1].SetFinalizers([]string{"openstack.k-orc.cloud/roleassignment"})
			var deleted []string
			h := newKRAHarness(t, c5c3v1alpha1.ManagementCluster, &interceptor.Funcs{
				Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					deleted = append(deleted, obj.GetName())
					return cl.Delete(ctx, obj, opts...)
				},
			}, nil, append([]client.Object{order, tc.cp}, children...)...)

			result, err := h.reconcile(ctx)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(result.RequeueAfter).To(Equal(korcRequeueAfter))
			prefix := keystoneRoleAssignmentRef(order, "").childPrefix()
			g.Expect(deleted).To(Equal([]string{prefix + "assignment", prefix + "role"}))
			g.Expect(kraGet(t, h).Finalizers).To(ContainElement(keystoneRoleAssignmentFinalizerName),
				"the finalizer is held while K-ORC still holds the RoleAssignment")

			assignment := &orcv1alpha1.RoleAssignment{}
			g.Expect(h.mgmt.Get(ctx, types.NamespacedName{Namespace: "default", Name: prefix + "assignment"}, assignment)).
				To(Succeed())
			assignment.Finalizers = nil
			g.Expect(h.mgmt.Update(ctx, assignment)).To(Succeed())

			_, err = h.reconcile(ctx)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(kraGet(t, h)).To(BeNil())
			g.Expect(kraLabelled(t, h.mgmt)).To(BeZero())
		})
	}
}

// TestKeystoneRoleAssignment_DeleteHoldsWhileApplicationCredentialsUseIt pins
// the hold: a credential order of the same user and project keeps the
// assignment, one of another project does not, and the teardown runs once the
// credential order is gone.
func TestKeystoneRoleAssignment_DeleteHoldsWhileApplicationCredentialsUseIt(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	order := keystoneRoleAssignmentCR()
	order.DeletionTimestamp = ptr.To(metav1.Now())
	credential := keystoneApplicationCredentialCR()
	credential.Finalizers = nil
	otherProject := keystoneApplicationCredentialCR()
	otherProject.Name = "other-project-appcred"
	otherProject.Spec.ProjectRef.Name = "other-project"
	var deleted []string
	h := newKRAHarness(t, kuTestCluster, &interceptor.Funcs{
		Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			deleted = append(deleted, obj.GetName())
			return cl.Delete(ctx, obj, opts...)
		},
	}, nil, append([]client.Object{order, credential, otherProject, kraControlPlane(kuTestCluster, "member")},
		kraConvergedChildren(order, kuTestCluster)...)...)

	result, err := h.reconcile(ctx)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(orderReferenceHoldRequeueAfter))
	g.Expect(deleted).To(BeEmpty(), "a held teardown deletes nothing")
	cond := kraCondition(kraGet(t, h))
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonOrderReferencedByApplicationCredentials))
	g.Expect(cond.Message).To(Equal(orderReferencedByApplicationCredentialsMessage(
		[]string{"workflow-appcred"}, kuTestNamespace, "role assignment",
		"the credentials are minted and deleted with a token scoped to the project, which needs the role")))

	g.Expect(h.order.Delete(ctx, credential)).To(Succeed())
	_, err = h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	prefix := keystoneRoleAssignmentRef(order, kuTestCluster).childPrefix()
	g.Expect(deleted).To(Equal([]string{prefix + "assignment", prefix + "role"}),
		"a credential order of another project does not hold the assignment")
}

func TestKeystoneRoleAssignment_DeleteHoldListErrorIsReturned(t *testing.T) {
	g := NewGomegaWithT(t)

	boom := errors.New("boom")
	order := keystoneRoleAssignmentCR()
	order.DeletionTimestamp = ptr.To(metav1.Now())
	h := newKRAHarness(t, c5c3v1alpha1.ManagementCluster, &interceptor.Funcs{
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*c5c3v1alpha1.KeystoneApplicationCredentialList); ok {
				return boom
			}
			return cl.List(ctx, list, opts...)
		},
	}, nil, append([]client.Object{order, kraControlPlane("", "member")}, kraConvergedChildren(order, "")...)...)

	_, err := h.reconcile(context.Background())

	g.Expect(err).To(MatchError(boom))
	g.Expect(err.Error()).To(ContainSubstring(`listing KeystoneApplicationCredentials in "tenant-a"`))
	g.Expect(kraGet(t, h).Finalizers).To(ContainElement(keystoneRoleAssignmentFinalizerName))
}

func TestApplicationCredentialToRoleAssignmentRequests(t *testing.T) {
	g := NewGomegaWithT(t)

	matching := keystoneRoleAssignmentCR()
	reader := keystoneRoleAssignmentCR()
	reader.Name = "workflow-reader"
	reader.Spec.Role = "reader"
	otherProject := keystoneRoleAssignmentCR()
	otherProject.Name = "other-project"
	otherProject.Spec.ProjectRef.Name = "other"
	c := orderFakeClient(t, nil, matching, reader, otherProject)
	request := func(ra client.Object) mcreconcile.Request {
		return mcreconcile.Request{
			Request: reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ra)}, ClusterName: kuTestCluster,
		}
	}

	g.Expect(kacEnqueued(applicationCredentialToRoleAssignmentRequests(), kuTestCluster, c,
		keystoneApplicationCredentialCR())).To(ConsistOf(request(matching), request(reader)),
		"every assignment of the pair holds")
	g.Expect(kacEnqueued(applicationCredentialToRoleAssignmentRequests(), kuTestCluster, c,
		keystoneUserCR(kuTestNamespace))).To(BeEmpty(), "an object of another kind maps to nothing")

	failing := orderFakeClient(t, &interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("cache not synced")
		},
	})
	g.Expect(kacEnqueued(applicationCredentialToRoleAssignmentRequests(), kuTestCluster, failing,
		keystoneApplicationCredentialCR())).To(BeEmpty())
}

func TestKeystoneRoleAssignmentReferenceMapper(t *testing.T) {
	g := NewGomegaWithT(t)

	named := keystoneRoleAssignmentCR()
	reader := keystoneRoleAssignmentCR()
	reader.Name = "workflow-reader"
	reader.Spec.Role = "reader"
	elsewhere := keystoneRoleAssignmentCR()
	elsewhere.Name = "someone-elses"
	elsewhere.Spec.UserRef.Name = "someone"
	c := orderFakeClient(t, nil, named, reader, elsewhere)
	mapper := keystoneRoleAssignmentReferenceMapper(kuTestCluster, c, roleAssignmentUserName)

	g.Expect(mapper(context.Background(), keystoneUserCR(kuTestNamespace))).To(ConsistOf(
		mcreconcile.Request{Request: reconcile.Request{NamespacedName: client.ObjectKeyFromObject(named)}, ClusterName: kuTestCluster},
		mcreconcile.Request{Request: reconcile.Request{NamespacedName: client.ObjectKeyFromObject(reader)}, ClusterName: kuTestCluster},
	))
	unreferenced := keystoneUserCR(kuTestNamespace)
	unreferenced.Name = "nobody"
	g.Expect(mapper(context.Background(), unreferenced)).To(BeEmpty())

	failing := orderFakeClient(t, &interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("cache not synced")
		},
	})
	g.Expect(keystoneRoleAssignmentReferenceMapper(kuTestCluster, failing, roleAssignmentUserName)(
		context.Background(), keystoneUserCR(kuTestNamespace))).To(BeNil())
}

func TestKeystoneRoleAssignmentReferencePredicate(t *testing.T) {
	user, project := kraReadyReferences()
	user.Status.Conditions[0].Status = metav1.ConditionFalse
	cases := []struct {
		name string
		old  client.Object
		new  func() client.Object
		want bool
	}{
		{name: "a status write that changes nothing the gates read", old: user, new: func() client.Object {
			u := user.DeepCopy()
			u.Status.UserID = "u-1"
			u.Status.ObservedGeneration = 1
			return u
		}, want: false},
		{name: "the user's UserReady flips", old: user, new: func() client.Object {
			u := user.DeepCopy()
			u.Status.Conditions[0].Status = metav1.ConditionTrue
			return u
		}, want: true},
		{name: "the project's Ready flips", old: project, new: func() client.Object {
			p := project.DeepCopy()
			conditions.SetCondition(&p.Status.Conditions, metav1.Condition{
				Type: "Ready", Status: metav1.ConditionTrue, Reason: "AllReady",
			})
			return p
		}, want: true},
		{name: "a spec change", old: user, new: func() client.Object {
			u := user.DeepCopy()
			u.Generation = 2
			return u
		}, want: true},
		{name: "a deletion", old: project, new: func() client.Object {
			p := project.DeepCopy()
			p.DeletionTimestamp = ptr.To(metav1.Now())
			return p
		}, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			g.Expect(keystoneRoleAssignmentReferencePredicate().Update(event.UpdateEvent{
				ObjectOld: tc.old, ObjectNew: tc.new(),
			})).To(Equal(tc.want))
		})
	}
}

func TestRoleAssignmentToReferencedOrderRequests(t *testing.T) {
	g := NewGomegaWithT(t)

	order := keystoneRoleAssignmentCR()

	// enqueued fires a create event for obj through the hold-leg handler built
	// for clusterName and returns what lands in the workqueue.
	enqueued := func(
		clusterName mcruntime.ClusterName, refName func(*c5c3v1alpha1.KeystoneRoleAssignment) string, obj client.Object,
	) []mcreconcile.Request {
		queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[mcreconcile.Request]())
		defer queue.ShutDown()
		roleAssignmentToReferencedOrderRequests(refName)(clusterName, nil).
			Create(context.Background(), event.TypedCreateEvent[client.Object]{Object: obj}, queue)
		var requests []mcreconcile.Request
		for queue.Len() > 0 {
			request, _ := queue.Get()
			requests = append(requests, request)
			queue.Done(request)
		}
		return requests
	}

	g.Expect(enqueued(kuTestCluster, roleAssignmentUserName, order)).To(Equal([]mcreconcile.Request{{
		Request:     reconcile.Request{NamespacedName: types.NamespacedName{Namespace: kuTestNamespace, Name: kuTestName}},
		ClusterName: kuTestCluster,
	}}))
	g.Expect(enqueued("", roleAssignmentProjectName, order)).To(Equal([]mcreconcile.Request{{
		Request: reconcile.Request{NamespacedName: types.NamespacedName{Namespace: kuTestNamespace, Name: kpTestName}},
	}}))
	g.Expect(enqueued("", roleAssignmentUserName, keystoneUserCR(kuTestNamespace))).
		To(BeNil(), "an object of another kind maps to nothing")
}

// TestKeystoneRoleAssignment_ReconcileEntry covers the branches Reconcile
// answers itself: a cluster that does not resolve is skipped without a requeue,
// and a failed read of the order is returned.
func TestKeystoneRoleAssignment_ReconcileEntry(t *testing.T) {
	t.Run("an unresolvable cluster is skipped", func(t *testing.T) {
		g := NewGomegaWithT(t)
		user, project := kraReadyReferences()
		h := newKRAHarness(t, kuTestCluster, nil, nil,
			keystoneRoleAssignmentCR(), user, project, kraControlPlane(kuTestCluster, "member"))
		h.r.(*KeystoneRoleAssignmentReconciler).Resolver = &childrenResolver{err: mcruntime.ErrClusterNotFound}

		result, err := h.reconcile(context.Background())

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(result.IsZero()).To(BeTrue())
		g.Expect(kraLabelled(t, h.mgmt)).To(BeZero(), "nothing is written for an unresolvable cluster")
		g.Expect(kraGet(t, h).Status.Conditions).To(BeEmpty())
	})

	t.Run("a failed read of the order is returned", func(t *testing.T) {
		g := NewGomegaWithT(t)
		boom := errors.New("boom")
		user, project := kraReadyReferences()
		h := newKRAHarness(t, c5c3v1alpha1.ManagementCluster, &interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*c5c3v1alpha1.KeystoneRoleAssignment); ok {
					return boom
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}, nil, keystoneRoleAssignmentCR(), user, project, kraControlPlane("", "member"))

		_, err := h.reconcile(context.Background())

		g.Expect(err).To(MatchError(boom))
		g.Expect(err.Error()).To(ContainSubstring("fetching KeystoneRoleAssignment"))
	})
}

func TestControlPlaneToKeystoneRoleAssignmentsMapper(t *testing.T) {
	g := NewGomegaWithT(t)

	explicit := keystoneRoleAssignmentCR()
	defaulted := keystoneRoleAssignmentCR()
	defaulted.Name, defaulted.Namespace = "same-namespace", "default"
	defaulted.Spec.ControlPlaneRef.Namespace = ""
	elsewhere := keystoneRoleAssignmentCR()
	elsewhere.Name = "other-plane"
	elsewhere.Spec.ControlPlaneRef.Namespace = "other"
	c := fake.NewClientBuilder().WithScheme(korcTestScheme(t)).WithObjects(explicit, defaulted, elsewhere).
		WithIndex(&c5c3v1alpha1.KeystoneRoleAssignment{}, KeystoneRoleAssignmentControlPlaneRefIndexKey,
			keystoneRoleAssignmentControlPlaneRefExtractor).
		Build()
	mapper := controlPlaneToKeystoneRoleAssignmentsMapper(c)

	g.Expect(mapper(context.Background(), ksControlPlane())).To(ConsistOf(
		reconcile.Request{NamespacedName: client.ObjectKeyFromObject(explicit)},
		reconcile.Request{NamespacedName: client.ObjectKeyFromObject(defaulted)},
	))
	unrelated := ksControlPlane()
	unrelated.Name = "another"
	g.Expect(mapper(context.Background(), unrelated)).To(BeEmpty())

	failing := fake.NewClientBuilder().WithScheme(korcTestScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("cache not synced")
		},
	}).Build()
	g.Expect(controlPlaneToKeystoneRoleAssignmentsMapper(failing)(context.Background(), ksControlPlane())).To(BeNil())
}

func TestKeystoneRoleAssignmentControlPlaneRefExtractor(t *testing.T) {
	g := NewGomegaWithT(t)

	order := keystoneRoleAssignmentCR()
	g.Expect(keystoneRoleAssignmentControlPlaneRefExtractor(order)).To(Equal([]string{"default/cp"}))
	order.Spec.ControlPlaneRef.Namespace = ""
	g.Expect(keystoneRoleAssignmentControlPlaneRefExtractor(order)).To(Equal([]string{"tenant-a/cp"}),
		"an empty namespace defaults to the order's own")
	order.Spec.ControlPlaneRef.Name = ""
	g.Expect(keystoneRoleAssignmentControlPlaneRefExtractor(order)).To(BeNil())
	g.Expect(keystoneRoleAssignmentControlPlaneRefExtractor(ksControlPlane())).To(BeNil())
}
