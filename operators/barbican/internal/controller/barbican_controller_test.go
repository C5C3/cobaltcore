// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Unit tests for the Barbican reconcile entry point, the finalizer-gated
// deletion path, and the field-index plumbing.
package controller

import (
	"context"
	"testing"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/forge/internal/common/conditions"
	commonreconcile "github.com/c5c3/forge/internal/common/reconcile"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	barbicanv1alpha1 "github.com/c5c3/forge/operators/barbican/api/v1alpha1"
)

func TestBarbicanSecretNameExtractor(t *testing.T) {
	tests := []struct {
		name  string
		obj   client.Object
		want  []string
		empty bool
	}{
		{
			name: "both references are indexed",
			obj:  testBarbican(),
			want: []string{"barbican-service-user", "barbican-db"},
		},
		{
			name: "one Secret serving both references is indexed once",
			obj: func() client.Object {
				barbican := testBarbican()
				barbican.Spec.Database.SecretRef.Name = "barbican-service-user"
				return barbican
			}(),
			want: []string{"barbican-service-user"},
		},
		{
			name: "an empty reference is skipped",
			obj: func() client.Object {
				barbican := testBarbican()
				barbican.Spec.ServiceUser.SecretRef.Name = ""
				return barbican
			}(),
			want: []string{"barbican-db"},
		},
		{
			name:  "a wrong-type object indexes under nothing",
			obj:   testManagedStore(),
			empty: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			got := barbicanSecretNameExtractor(tc.obj)
			if tc.empty {
				g.Expect(got).To(BeEmpty())
				return
			}
			g.Expect(got).To(Equal(tc.want))
		})
	}
}

func TestBarbicanSecretStoreSecretNameExtractor(t *testing.T) {
	brownfieldWithCA := func() client.Object {
		store := testBrownfieldStore()
		store.Spec.OpenBao.Server.CABundleSecretRef = &barbicanv1alpha1.SecretNameRefSpec{Name: "brownfield-ca"}
		return store
	}
	brownfieldSharedSecret := func() client.Object {
		store := testBrownfieldStore()
		store.Spec.OpenBao.Server.CABundleSecretRef = &barbicanv1alpha1.SecretNameRefSpec{Name: "brownfield-approle"}
		return store
	}

	tests := []struct {
		name  string
		obj   client.Object
		want  []string
		empty bool
	}{
		{name: "credentials only", obj: testBrownfieldStore(), want: []string{"brownfield-approle"}},
		{name: "credentials and CA bundle", obj: brownfieldWithCA(), want: []string{"brownfield-approle", "brownfield-ca"}},
		{name: "one Secret serving both references is indexed once", obj: brownfieldSharedSecret(), want: []string{"brownfield-approle"}},
		{name: "a managed store references no Secret by name", obj: testManagedStore(), empty: true},
		{name: "a wrong-type object indexes under nothing", obj: testBarbican(), empty: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			got := barbicanSecretStoreSecretNameExtractor(tc.obj)
			if tc.empty {
				g.Expect(got).To(BeEmpty())
				return
			}
			g.Expect(got).To(Equal(tc.want))
		})
	}
}

func TestReconcile_AddsFinalizerOnFirstPass(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := testBarbican() // no finalizer yet
	r := newBarbicanTestReconciler(barbican)

	res, err := r.Reconcile(context.Background(), barbicanRequest)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{Requeue: true}), "the finalizer add requeues so the next pass sees it persisted")
	g.Expect(getBarbican(t, r.Client).Finalizers).To(ContainElement(barbicanFinalizer))
}

func TestReconcile_NotFoundCRIsIgnored(t *testing.T) {
	g := NewGomegaWithT(t)
	r := newBarbicanTestReconciler() // no Barbican seeded

	res, err := r.Reconcile(context.Background(), barbicanRequest)

	g.Expect(err).NotTo(HaveOccurred(), "a deleted CR is not an error")
	g.Expect(res.IsZero()).To(BeTrue())
}

func TestReconcile_FailingSecretsStepShortCircuitsPipeline(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := testBarbican()
	barbican.Finalizers = []string{barbicanFinalizer} // skip the finalizer-add requeue
	// The selected store is explicitly not Ready, so the Secrets step fails fast.
	r := newBarbicanTestReconciler(barbican, notReadyClusterSecretStore(openBaoClusterStoreName))

	res, err := r.Reconcile(context.Background(), barbicanRequest)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))

	got := getBarbican(t, r.Client)
	cond := barbicanCondition(got, "SecretsReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	// A later step must not have run: the config step never rendered, so the
	// ExtraConfigHealthy condition it maintains is absent.
	g.Expect(barbicanCondition(got, "ExtraConfigHealthy")).To(BeNil())
	g.Expect(got.Status.ObservedGeneration).To(Equal(int64(1)))
}

func TestReconcileDelete_LiveResourcesRetainFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := testBarbican()
	barbican.Finalizers = []string{barbicanFinalizer}
	// A live MariaDB Database owned by this Barbican (key is the bare CR name).
	mdb := &mariadbv1alpha1.Database{}
	mdb.Name = testBarbicanName
	mdb.Namespace = testNamespace
	r := newBarbicanTestReconciler(barbican, mdb)

	// Move the Barbican into the deleting state.
	g.Expect(r.Delete(context.Background(), barbican)).To(Succeed())

	res, err := r.Reconcile(context.Background(), barbicanRequest)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueDatabaseWait))
	g.Expect(getBarbican(t, r.Client).Finalizers).To(ContainElement(barbicanFinalizer),
		"the finalizer is retained one pass while live MariaDB resources remain")
}

func TestReconcileDelete_NoLiveResourcesReleasesFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := testBarbican()
	barbican.Finalizers = []string{barbicanFinalizer}
	r := newBarbicanTestReconciler(barbican) // no MariaDB CRs

	g.Expect(r.Delete(context.Background(), barbican)).To(Succeed())

	res, err := r.Reconcile(context.Background(), barbicanRequest)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	// With the finalizer released, the fake client garbage-collects the CR.
	var gone barbicanv1alpha1.Barbican
	err = r.Get(context.Background(), barbicanRequest.NamespacedName, &gone)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
		"the finalizer is released when no live MariaDB resource remains")
}

func TestReconcileDelete_WithoutFinalizerIsNoOp(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := testBarbican()
	barbican.DeletionTimestamp = &metav1.Time{Time: metav1.Now().Time}
	r := newBarbicanTestReconciler()

	res, err := r.reconcileDelete(context.Background(), barbican)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).To(BeEmpty(),
		"a CR without the finalizer emits no cleanup events")
}

// TestSetReadyCondition_TrueOnlyWhenAllSubConditionsTrue pins the aggregate: one
// missing or False member is enough to keep Ready False, which is what stops a
// half-projected Barbican from advertising itself as usable.
func TestSetReadyCondition_TrueOnlyWhenAllSubConditionsTrue(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := testBarbican()

	// All but the last sub-condition True: Ready stays False.
	for _, conditionType := range subConditionTypes[:len(subConditionTypes)-1] {
		conditions.SetCondition(&barbican.Status.Conditions, metav1.Condition{
			Type: conditionType, Status: metav1.ConditionTrue, Reason: "Ready",
		})
	}
	setReadyCondition(barbican)
	g.Expect(conditions.GetCondition(barbican.Status.Conditions, "Ready").Status).To(Equal(metav1.ConditionFalse))

	// The last one flips Ready True.
	conditions.SetCondition(&barbican.Status.Conditions, metav1.Condition{
		Type: subConditionTypes[len(subConditionTypes)-1], Status: metav1.ConditionTrue, Reason: "Ready",
	})
	setReadyCondition(barbican)
	g.Expect(conditions.GetCondition(barbican.Status.Conditions, "Ready").Status).To(Equal(metav1.ConditionTrue))
}

// TestSecretNameIndexResolvesBarbican covers the index as the watch mapper will
// use it: a Secret name resolves to the referencing CR through the field
// selector rather than an unfiltered namespace List.
func TestSecretNameIndexResolvesBarbican(t *testing.T) {
	g := NewGomegaWithT(t)
	other := testBarbican()
	other.Name = "other-barbican"
	other.Spec.ServiceUser.SecretRef = commonv1.SecretRefSpec{Name: "other-service-user"}
	other.Spec.Database.SecretRef = commonv1.SecretRefSpec{Name: "other-db"}
	c := barbicanFakeClientBuilder(testBarbican(), other).Build()

	var list barbicanv1alpha1.BarbicanList
	g.Expect(c.List(context.Background(), &list,
		client.InNamespace(testNamespace),
		client.MatchingFields{BarbicanSecretNameIndexKey: "barbican-db"})).To(Succeed())

	g.Expect(list.Items).To(HaveLen(1))
	g.Expect(list.Items[0].Name).To(Equal(testBarbicanName))
}
