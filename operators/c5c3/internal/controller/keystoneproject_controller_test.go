// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the KeystoneProject reconciler: the reserved names, the collision
// probe, the managed Project and its waits, the freeze, the teardown and the
// ControlPlane watch mapping.
package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	mcruntime "sigs.k8s.io/multicluster-runtime/pkg/multicluster"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

const kpTestName = "workflow-project"

// keystoneProjectCR returns the order "workflow-project" in kuTestNamespace,
// referencing the ControlPlane cp in "default" and already carrying the
// teardown finalizer.
func keystoneProjectCR() *c5c3v1alpha1.KeystoneProject {
	return &c5c3v1alpha1.KeystoneProject{
		ObjectMeta: metav1.ObjectMeta{
			Name:       kpTestName,
			Namespace:  kuTestNamespace,
			Generation: 1,
			UID:        types.UID("kp-uid"),
			Finalizers: []string{keystoneProjectFinalizerName},
		},
		Spec: c5c3v1alpha1.KeystoneProjectSpec{
			ControlPlaneRef: c5c3v1alpha1.ControlPlaneRefSpec{Name: "cp", Namespace: "default"},
		},
	}
}

func newKPHarness(t *testing.T, cluster string, mgmtFuncs *interceptor.Funcs, objs ...client.Object) *orderHarness {
	t.Helper()
	return newOrderHarness(t, cluster, types.NamespacedName{Namespace: kuTestNamespace, Name: kpTestName},
		mgmtFuncs, nil, objs, func(mgmt client.Client, resolver commonmulticluster.ClusterResolver) orderReconciler {
			return &KeystoneProjectReconciler{Client: mgmt, Scheme: mgmt.Scheme(), Resolver: resolver}
		})
}

func kpGet(t *testing.T, h *orderHarness) *c5c3v1alpha1.KeystoneProject {
	t.Helper()
	return reloadOrder[c5c3v1alpha1.KeystoneProject](t, h)
}

func kpCondition(order *c5c3v1alpha1.KeystoneProject) *metav1.Condition {
	return conditions.GetCondition(order.Status.Conditions, conditionTypeKeystoneProjectProjectReady)
}

// kpLabelled counts the Projects in the ControlPlane's namespace that carry the
// order's name label.
func kpLabelled(t *testing.T, c client.Client) int {
	t.Helper()
	return labelledIn(t, c, &orcv1alpha1.ProjectList{}, keystoneProjectLabelKeys.Name, kpTestName)
}

// kpManagedProject returns the order's managed Project as K-ORC reports it,
// with the given status conditions.
func kpManagedProject(order *c5c3v1alpha1.KeystoneProject, cluster string, conds []metav1.Condition) *orcv1alpha1.Project {
	project := managedProjectChild(keystoneProjectProjectRef(order, cluster), "default", keystoneProjectName(order),
		"cp-domain-default", orcv1alpha1.CloudCredentialsReference{})
	project.Labels = keystoneProjectRef(order, cluster).childLabels()
	project.Status = orcv1alpha1.ProjectStatus{Conditions: conds, ID: ptr.To("p-1")}
	return project
}

func TestKeystoneProject_ProbesThenProjectsTheManagedProject(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	order := keystoneProjectCR()
	cp := keystoneUserControlPlane(assignOn(kuTestNamespace, c5c3v1alpha1.ManagementCluster))
	h := newKPHarness(t, c5c3v1alpha1.ManagementCluster, nil, order, cp)

	result, err := h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(10 * time.Second))
	g.Expect(kpCondition(kpGet(t, h)).Reason).To(Equal(reasonProbingForCollision))

	prefix := keystoneProjectRef(order, "").childPrefix()
	g.Expect(prefix).To(MatchRegexp(`^workflow-project-[0-9a-f]{8}-project-$`))
	probe := &orcv1alpha1.Project{}
	g.Expect(h.mgmt.Get(ctx, types.NamespacedName{Namespace: "default", Name: prefix + "project-probe"}, probe)).To(Succeed())
	g.Expect(probe.Spec.ManagementPolicy).To(Equal(orcv1alpha1.ManagementPolicyUnmanaged))
	g.Expect(string(*probe.Spec.Import.Filter.Name)).To(Equal(kpTestName))
	g.Expect(string(*probe.Spec.Import.Filter.DomainRef)).To(Equal("cp-domain-default"))
	g.Expect(probe.Labels).To(Equal(keystoneProjectRef(order, "").childLabels()))

	probe.Status.Conditions = pendingImportConditions(time.Minute)
	g.Expect(h.mgmt.Update(ctx, probe)).To(Succeed())

	result, err = h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(10 * time.Second))
	g.Expect(kpCondition(kpGet(t, h)).Reason).To(Equal(reasonKeystoneProjectWaiting),
		"the project is registered but not yet Available")

	project := &orcv1alpha1.Project{}
	g.Expect(h.mgmt.Get(ctx, types.NamespacedName{Namespace: "default", Name: prefix + "project"}, project)).To(Succeed())
	g.Expect(project.Spec.ManagementPolicy).To(Equal(orcv1alpha1.ManagementPolicyManaged))
	g.Expect(string(*project.Spec.Resource.Name)).To(Equal(kpTestName))
	g.Expect(string(*project.Spec.Resource.DomainRef)).To(Equal("cp-domain-default"))
	g.Expect(project.Labels).To(Equal(map[string]string{
		"c5c3.io/keystoneproject-name": kpTestName, "c5c3.io/keystoneproject-namespace": kuTestNamespace,
		"c5c3.io/keystoneproject-cluster": "",
	}))
	g.Expect(project.OwnerReferences).To(BeEmpty())
	err = h.mgmt.Get(ctx, client.ObjectKeyFromObject(probe), &orcv1alpha1.Project{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "an absent probe is dropped")
}

func TestKeystoneProject_ReservedNamesAreRefused(t *testing.T) {
	cases := []struct {
		name, projectName, reserved string
	}{
		{name: "a built-in service project", projectName: "service-glance", reserved: "service-glance"},
		{name: "the admin project in another case", projectName: "ADMIN", reserved: "ADMIN"},
		{name: "the admin project with an accent", projectName: "ádmin", reserved: "ádmin"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			order := keystoneProjectCR()
			order.Spec.ProjectName = tc.projectName
			h := newKPHarness(t, c5c3v1alpha1.ManagementCluster, nil,
				order, keystoneUserControlPlane(assignOn(kuTestNamespace, "")))

			result, err := h.reconcile(context.Background())

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(result.RequeueAfter).To(Equal(10*time.Minute), "only a ControlPlane edit clears it")
			cond := kpCondition(kpGet(t, h))
			g.Expect(cond.Reason).To(Equal(reasonKeystoneProjectCollision))
			g.Expect(cond.Message).To(ContainSubstring(`project "` + tc.reserved + `"`))
			g.Expect(cond.Message).To(ContainSubstring("reserves"))
			g.Expect(kpLabelled(t, h.mgmt)).To(BeZero(), "neither a probe nor a Project is created")
		})
	}
}

func TestKeystoneProject_CollisionProbe(t *testing.T) {
	cases := []struct {
		name       string
		conds      []metav1.Condition
		wantReason string
		wantSub    string
	}{
		{
			name: "the probe resolves an existing project", conds: availableImportConditions(),
			wantReason: reasonKeystoneProjectCollision, wantSub: "never takes over",
		},
		{name: "the probe is pending", wantReason: reasonProbingForCollision, wantSub: "probing whether project"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			order := keystoneProjectCR()
			probe := unmanagedProjectImport(keystoneProjectProjectProbeRef(order, ""), "default", kpTestName,
				"cp-domain-default", orcv1alpha1.CloudCredentialsReference{})
			probe.Labels = keystoneProjectRef(order, "").childLabels()
			probe.Status.Conditions = tc.conds
			h := newKPHarness(t, c5c3v1alpha1.ManagementCluster, nil,
				order, probe, keystoneUserControlPlane(assignOn(kuTestNamespace, "")))

			result, err := h.reconcile(context.Background())

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(result.RequeueAfter).To(Equal(10 * time.Second))
			cond := kpCondition(kpGet(t, h))
			g.Expect(cond.Reason).To(Equal(tc.wantReason))
			g.Expect(cond.Message).To(ContainSubstring(tc.wantSub))
			err = h.mgmt.Get(context.Background(),
				types.NamespacedName{Namespace: "default", Name: keystoneProjectProjectRef(order, "")}, &orcv1alpha1.Project{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "no managed Project is created")
		})
	}
}

func TestKeystoneProject_AvailableProjectIsProvisioned(t *testing.T) {
	g := NewGomegaWithT(t)

	order := keystoneProjectCR()
	h := newKPHarness(t, c5c3v1alpha1.ManagementCluster, nil, order,
		kpManagedProject(order, "", availableImportConditions()),
		keystoneUserControlPlane(assignOn(kuTestNamespace, "")))

	result, err := h.reconcile(context.Background())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue(), "a converged order on the management cluster waits for an event")
	got := kpGet(t, h)
	cond := kpCondition(got)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(reasonKeystoneProjectProvisioned))
	g.Expect(got.Status.ProjectID).To(Equal("p-1"))
	g.Expect(got.Status.ProjectName).To(Equal(kpTestName))
	g.Expect(got.Status.DomainName).To(Equal("Default"))
	g.Expect(conditions.AllTrue(got.Status.Conditions, "Ready")).To(BeTrue())
}

func TestKeystoneProject_ConvergedOrderOnATargetClusterRefreshes(t *testing.T) {
	g := NewGomegaWithT(t)

	order := keystoneProjectCR()
	h := newKPHarness(t, kuTestCluster, nil, order,
		kpManagedProject(order, kuTestCluster, availableImportConditions()),
		keystoneUserControlPlane(assignOn(kuTestNamespace, kuTestCluster)))

	result, err := h.reconcile(context.Background())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(10 * time.Minute))
	g.Expect(kpCondition(kpGet(t, h)).Reason).To(Equal(reasonKeystoneProjectProvisioned))
	project := &orcv1alpha1.Project{}
	g.Expect(h.mgmt.Get(context.Background(), types.NamespacedName{
		Namespace: "default", Name: keystoneProjectProjectRef(order, kuTestCluster),
	}, project)).To(Succeed())
	g.Expect(project.Labels).To(HaveKeyWithValue("c5c3.io/keystoneproject-cluster", kuTestCluster))
}

func TestKeystoneProject_KORCOutcomes(t *testing.T) {
	cases := []struct {
		name       string
		conds      []metav1.Condition
		wantReason string
	}{
		{name: "a terminal error", conds: terminalImportConditions("bad domain"), wantReason: reasonKeystoneProjectFailed},
		{name: "not yet Available", conds: pendingImportConditions(0), wantReason: reasonKeystoneProjectWaiting},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			order := keystoneProjectCR()
			h := newKPHarness(t, c5c3v1alpha1.ManagementCluster, nil, order,
				kpManagedProject(order, "", tc.conds), keystoneUserControlPlane(assignOn(kuTestNamespace, "")))

			result, err := h.reconcile(context.Background())

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(result.RequeueAfter).To(Equal(10 * time.Second))
			g.Expect(kpCondition(kpGet(t, h)).Reason).To(Equal(tc.wantReason))
		})
	}
}

func TestKeystoneProject_ApplyErrorIsReported(t *testing.T) {
	g := NewGomegaWithT(t)

	order := keystoneProjectCR()
	boom := errors.New("boom")
	h := newKPHarness(t, c5c3v1alpha1.ManagementCluster, &interceptor.Funcs{
		Apply: func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error {
			return boom
		},
	}, order, kpManagedProject(order, "", availableImportConditions()), keystoneUserControlPlane(assignOn(kuTestNamespace, "")))

	_, err := h.reconcile(context.Background())

	g.Expect(err).To(MatchError(boom))
	g.Expect(err.Error()).To(ContainSubstring("ensuring the managed Project"))
	g.Expect(kpCondition(kpGet(t, h)).Reason).To(Equal(reasonKeystoneProjectError))
}

func TestKeystoneProject_ProbeReadErrorIsReported(t *testing.T) {
	g := NewGomegaWithT(t)

	order := keystoneProjectCR()
	boom := errors.New("boom")
	h := newKPHarness(t, c5c3v1alpha1.ManagementCluster, &interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*orcv1alpha1.Project); ok && key.Name == keystoneProjectProjectRef(order, "") {
				return boom
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	}, order, keystoneUserControlPlane(assignOn(kuTestNamespace, "")))

	_, err := h.reconcile(context.Background())

	g.Expect(err).To(MatchError(boom))
	cond := kpCondition(kpGet(t, h))
	g.Expect(cond.Reason).To(Equal(reasonKeystoneProjectError))
	g.Expect(cond.Message).To(HavePrefix("probing for a pre-existing project"))
}

// TestKeystoneProject_ReconcileEntry covers the branches Reconcile answers
// itself: a cluster that does not resolve is skipped without a requeue, and a
// failed read of the order is returned.
func TestKeystoneProject_ReconcileEntry(t *testing.T) {
	t.Run("an unresolvable cluster is skipped", func(t *testing.T) {
		g := NewGomegaWithT(t)
		h := newKPHarness(t, kuTestCluster, nil,
			keystoneProjectCR(), keystoneUserControlPlane(assignOn(kuTestNamespace, kuTestCluster)))
		h.r.(*KeystoneProjectReconciler).Resolver = &childrenResolver{err: mcruntime.ErrClusterNotFound}

		result, err := h.reconcile(context.Background())

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(result.IsZero()).To(BeTrue())
		g.Expect(kpLabelled(t, h.mgmt)).To(BeZero(), "nothing is written for an unresolvable cluster")
		g.Expect(kpGet(t, h).Status.Conditions).To(BeEmpty())
	})

	t.Run("a failed read of the order is returned", func(t *testing.T) {
		g := NewGomegaWithT(t)
		boom := errors.New("boom")
		h := newKPHarness(t, c5c3v1alpha1.ManagementCluster, &interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*c5c3v1alpha1.KeystoneProject); ok {
					return boom
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}, keystoneProjectCR(), keystoneUserControlPlane(assignOn(kuTestNamespace, "")))

		_, err := h.reconcile(context.Background())

		g.Expect(err).To(MatchError(boom))
		g.Expect(err.Error()).To(ContainSubstring("fetching KeystoneProject"))
	})
}

func TestControlPlaneToKeystoneProjectsMapper(t *testing.T) {
	g := NewGomegaWithT(t)

	explicit := keystoneProjectCR()
	defaulted := keystoneProjectCR()
	defaulted.Name, defaulted.Namespace = "same-namespace", "default"
	defaulted.Spec.ControlPlaneRef.Namespace = ""
	elsewhere := keystoneProjectCR()
	elsewhere.Name = "other-plane"
	elsewhere.Spec.ControlPlaneRef.Namespace = "other"
	c := fake.NewClientBuilder().WithScheme(korcTestScheme(t)).WithObjects(explicit, defaulted, elsewhere).
		WithIndex(&c5c3v1alpha1.KeystoneProject{}, KeystoneProjectControlPlaneRefIndexKey,
			keystoneProjectControlPlaneRefExtractor).
		Build()
	mapper := controlPlaneToKeystoneProjectsMapper(c)

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
	g.Expect(controlPlaneToKeystoneProjectsMapper(failing)(context.Background(), ksControlPlane())).To(BeNil())
}

func TestKeystoneProjectControlPlaneRefExtractor(t *testing.T) {
	g := NewGomegaWithT(t)

	order := keystoneProjectCR()
	g.Expect(keystoneProjectControlPlaneRefExtractor(order)).To(Equal([]string{"default/cp"}))
	order.Spec.ControlPlaneRef.Namespace = ""
	g.Expect(keystoneProjectControlPlaneRefExtractor(order)).To(Equal([]string{"tenant-a/cp"}),
		"an empty namespace defaults to the order's own")
	order.Spec.ControlPlaneRef.Name = ""
	g.Expect(keystoneProjectControlPlaneRefExtractor(order)).To(BeNil())
	g.Expect(keystoneProjectControlPlaneRefExtractor(ksControlPlane())).To(BeNil())
}

// TestKeystoneProject_ConsentAndFreeze covers an order without an entry, which
// projects nothing, and one whose entry is withdrawn after convergence, which
// keeps its Project.
func TestKeystoneProject_ConsentAndFreeze(t *testing.T) {
	t.Run("no entry", func(t *testing.T) {
		g := NewGomegaWithT(t)
		order := keystoneProjectCR()
		order.Finalizers = nil
		h := newKPHarness(t, c5c3v1alpha1.ManagementCluster, nil, order, keystoneUserControlPlane())

		result, err := h.reconcile(context.Background())

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(result.RequeueAfter).To(Equal(time.Minute))
		got := kpGet(t, h)
		g.Expect(kpCondition(got).Reason).To(Equal(reasonOrderNamespaceNotAssigned))
		g.Expect(got.Finalizers).To(BeEmpty(), "an order the operator does not serve gets no finalizer")
		g.Expect(kpLabelled(t, h.mgmt)).To(BeZero())
	})

	t.Run("the entry is withdrawn after convergence", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ctx := context.Background()
		order := keystoneProjectCR()
		cp := keystoneUserControlPlane(assignOn(kuTestNamespace, ""))
		h := newKPHarness(t, c5c3v1alpha1.ManagementCluster, nil, order,
			kpManagedProject(order, "", availableImportConditions()), cp)
		_, err := h.reconcile(ctx)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(kpCondition(kpGet(t, h)).Status).To(Equal(metav1.ConditionTrue))

		live := &c5c3v1alpha1.ControlPlane{}
		g.Expect(h.mgmt.Get(ctx, client.ObjectKeyFromObject(cp), live)).To(Succeed())
		live.Spec.NamespaceAssignments = nil
		g.Expect(h.mgmt.Update(ctx, live)).To(Succeed())

		_, err = h.reconcile(ctx)
		g.Expect(err).NotTo(HaveOccurred())
		got := kpGet(t, h)
		g.Expect(kpCondition(got).Reason).To(Equal(reasonOrderNamespaceNotAssigned))
		g.Expect(got.Status.ProjectID).To(Equal("p-1"), "the frozen order keeps its status")
		g.Expect(kpLabelled(t, h.mgmt)).To(Equal(1), "a frozen order revokes nothing")
	})
}

// kpDeletingOrder returns a converged order marked for deletion with its managed
// Project, held by K-ORC's finalizer, and a leftover probe.
func kpDeletingOrder(cluster string) (*c5c3v1alpha1.KeystoneProject, []client.Object) {
	order := keystoneProjectCR()
	order.DeletionTimestamp = ptr.To(metav1.Now())
	project := kpManagedProject(order, cluster, availableImportConditions())
	project.Finalizers = []string{"openstack.k-orc.cloud/project"}
	probe := unmanagedProjectImport(keystoneProjectProjectProbeRef(order, cluster), "default", kpTestName,
		"cp-domain-default", orcv1alpha1.CloudCredentialsReference{})
	probe.Labels = keystoneProjectRef(order, cluster).childLabels()
	return order, []client.Object{project, probe}
}

func TestKeystoneProject_DeleteRemovesTheProjectThenReleasesTheFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	order, seeded := kpDeletingOrder("")
	var deleted []string
	h := newKPHarness(t, c5c3v1alpha1.ManagementCluster, &interceptor.Funcs{
		Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			deleted = append(deleted, obj.GetName())
			return cl.Delete(ctx, obj, opts...)
		},
	}, append([]client.Object{order, keystoneUserControlPlane(assignOn(kuTestNamespace, ""))}, seeded...)...)

	result, err := h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(korcRequeueAfter))
	prefix := keystoneProjectRef(order, "").childPrefix()
	g.Expect(deleted).To(Equal([]string{prefix + "project", prefix + "project-probe"}))
	g.Expect(kpGet(t, h).Finalizers).To(ContainElement(keystoneProjectFinalizerName),
		"the finalizer is held while K-ORC still holds the Project")

	project := &orcv1alpha1.Project{}
	g.Expect(h.mgmt.Get(ctx, types.NamespacedName{Namespace: "default", Name: prefix + "project"}, project)).To(Succeed())
	project.Finalizers = nil
	g.Expect(h.mgmt.Update(ctx, project)).To(Succeed())

	_, err = h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(kpGet(t, h)).To(BeNil(), "the order is gone once its finalizer is released")
	g.Expect(kpLabelled(t, h.mgmt)).To(BeZero())
}

func TestKeystoneProject_DeleteFailsOpenWithoutTheControlPlane(t *testing.T) {
	g := NewGomegaWithT(t)

	order, seeded := kpDeletingOrder("")
	h := newKPHarness(t, c5c3v1alpha1.ManagementCluster, nil, append([]client.Object{order}, seeded...)...)

	_, err := h.reconcile(context.Background())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(kpGet(t, h)).To(BeNil(), "without a plane the finalizer is released at once")
	g.Expect(kpLabelled(t, h.mgmt)).To(Equal(1), "only the Project K-ORC still holds is left")
}

func TestKeystoneProject_DeleteHoldsWhileRoleAssignmentsReferenceIt(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	order, seeded := kpDeletingOrder(kuTestCluster)
	assignment := keystoneRoleAssignmentCR()
	assignment.Finalizers = nil
	other := keystoneRoleAssignmentCR()
	other.Name = "another-project"
	other.Spec.ProjectRef.Name = "another"
	var deleted []string
	h := newKPHarness(t, kuTestCluster, &interceptor.Funcs{
		Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			deleted = append(deleted, obj.GetName())
			return cl.Delete(ctx, obj, opts...)
		},
	}, append([]client.Object{order, assignment, other, keystoneUserControlPlane(assignOn(kuTestNamespace, kuTestCluster))},
		seeded...)...)

	result, err := h.reconcile(ctx)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(time.Minute))
	g.Expect(deleted).To(BeEmpty(), "a held teardown deletes nothing")
	cond := kpCondition(kpGet(t, h))
	g.Expect(cond.Reason).To(Equal(reasonOrderReferencedByRoleAssignments))
	g.Expect(cond.Message).To(ContainSubstring(`KeystoneRoleAssignment(s) ["workflow-member"] in namespace "tenant-a"`))
	g.Expect(cond.Message).To(ContainSubstring("still reference this project"))

	g.Expect(h.order.Delete(ctx, assignment)).To(Succeed())
	_, err = h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	prefix := keystoneProjectRef(order, kuTestCluster).childPrefix()
	g.Expect(deleted).To(Equal([]string{prefix + "project", prefix + "project-probe"}),
		"with the assignment gone the teardown proceeds")
}

func TestKeystoneProject_DeleteHoldsWhileApplicationCredentialsReferenceIt(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	order, seeded := kpDeletingOrder(kuTestCluster)
	credential := keystoneApplicationCredentialCR()
	credential.Finalizers = nil
	otherProject := keystoneApplicationCredentialCR()
	otherProject.Name = "other-project-appcred"
	otherProject.Spec.ProjectRef.Name = "other-project"
	var deleted []string
	h := newKPHarness(t, kuTestCluster, &interceptor.Funcs{
		Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			deleted = append(deleted, obj.GetName())
			return cl.Delete(ctx, obj, opts...)
		},
	}, append([]client.Object{
		order, credential, otherProject, keystoneUserControlPlane(assignOn(kuTestNamespace, kuTestCluster)),
	}, seeded...)...)

	result, err := h.reconcile(ctx)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(orderReferenceHoldRequeueAfter))
	g.Expect(deleted).To(BeEmpty(), "a held teardown deletes nothing")
	cond := kpCondition(kpGet(t, h))
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonOrderReferencedByApplicationCredentials))
	g.Expect(cond.Message).To(Equal(orderReferencedByApplicationCredentialsMessage(
		[]string{"workflow-appcred"}, kuTestNamespace, "project", "the credentials are scoped to the project")))

	g.Expect(h.order.Delete(ctx, credential)).To(Succeed())
	_, err = h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	prefix := keystoneProjectRef(order, kuTestCluster).childPrefix()
	g.Expect(deleted).To(Equal([]string{prefix + "project", prefix + "project-probe"}),
		"with the credential order gone the teardown proceeds")
}
