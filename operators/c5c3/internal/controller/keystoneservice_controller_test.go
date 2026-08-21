// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the KeystoneService reconciler's lifecycle: the finalizer, the gates
// every block shares, the naming contract, the prune sweep, and teardown.
package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// The fixtures share one CR identity so the child names a test asserts on are
// derivable from the helpers rather than hand-written.
const (
	ksTestName      = "glance-registration"
	ksTestNamespace = "default"
)

// ksControlPlane returns a managed ControlPlane with AdminCredentialReady already
// satisfied, so a KeystoneService reconcile runs past the shared gates.
func ksControlPlane() *c5c3v1alpha1.ControlPlane {
	cp := korcControlPlane()
	setAdminCredentialReady(cp)
	return cp
}

// keystoneServiceCR returns a KeystoneService in the ControlPlane's namespace
// ALREADY carrying the teardown finalizer, so a reconcile projects instead of
// returning on the finalizer-install pass. Tests declare the blocks they need.
func keystoneServiceCR() *c5c3v1alpha1.KeystoneService {
	return &c5c3v1alpha1.KeystoneService{
		ObjectMeta: metav1.ObjectMeta{
			Name:       ksTestName,
			Namespace:  ksTestNamespace,
			Generation: 1,
			UID:        types.UID("ks-uid"),
			Finalizers: []string{keystoneServiceFinalizerName},
		},
		Spec: c5c3v1alpha1.KeystoneServiceSpec{
			ControlPlaneRef: c5c3v1alpha1.ControlPlaneRefSpec{Name: "cp"},
		},
	}
}

// ksAccountSpec is the minimal account block: a referenced project, no roles.
func ksAccountSpec() *c5c3v1alpha1.KeystoneServiceAccountSpec {
	return &c5c3v1alpha1.KeystoneServiceAccountSpec{
		Project: c5c3v1alpha1.ServiceAccountProjectSpec{Name: "service"},
	}
}

// ksCatalogSpec is the minimal catalog block: one service type, no endpoints.
func ksCatalogSpec() *c5c3v1alpha1.KeystoneServiceCatalogSpec {
	return &c5c3v1alpha1.KeystoneServiceCatalogSpec{ServiceType: "image"}
}

// ownedByKS hand-builds a controller OwnerReference to ks so
// metav1.IsControlledBy recognises a seeded child as one the sweep owns.
func ownedByKS(ks *c5c3v1alpha1.KeystoneService) []metav1.OwnerReference {
	return []metav1.OwnerReference{{
		APIVersion:         c5c3v1alpha1.GroupVersion.String(),
		Kind:               "KeystoneService",
		Name:               ks.Name,
		UID:                ks.UID,
		Controller:         ptr.To(true),
		BlockOwnerDeletion: ptr.To(true),
	}}
}

// ksClientBuilder seeds the fake client every KeystoneService test starts from:
// the CR, its ControlPlane, and the ready stores the account's delivery leg gates
// on.
func ksClientBuilder(t *testing.T, objs ...client.Object) (client.Client, *KeystoneServiceReconciler) {
	t.Helper()
	s := korcTestScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(objs...).
		WithStatusSubresource(&c5c3v1alpha1.KeystoneService{}).
		Build()
	return c, &KeystoneServiceReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(20)}
}

// reconcileKeystoneService runs one full Reconcile against the seeded objects and
// returns the reloaded CR (nil once it is gone), the client, and the raw result
// and error, for the tests that assert on those.
func reconcileKeystoneService(
	t *testing.T, objs ...client.Object,
) (*c5c3v1alpha1.KeystoneService, client.Client, ctrl.Result, error) {
	t.Helper()
	c, r := ksClientBuilder(t, objs...)
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: ksTestNamespace, Name: ksTestName},
	})
	got := &c5c3v1alpha1.KeystoneService{}
	if getErr := c.Get(context.Background(),
		types.NamespacedName{Namespace: ksTestNamespace, Name: ksTestName}, got); getErr != nil {
		if apierrors.IsNotFound(getErr) {
			return nil, c, result, err
		}
		t.Fatalf("reloading the KeystoneService: %v", getErr)
	}
	return got, c, result, err
}

// runKeystoneService is reconcileKeystoneService for the tests that expect the
// pass to succeed.
func runKeystoneService(t *testing.T, objs ...client.Object) (*c5c3v1alpha1.KeystoneService, client.Client) {
	t.Helper()
	g := NewGomegaWithT(t)
	got, c, _, err := reconcileKeystoneService(t, objs...)
	g.Expect(err).NotTo(HaveOccurred())
	return got, c
}

func ksCondition(ks *c5c3v1alpha1.KeystoneService, condType string) *metav1.Condition {
	return conditions.GetCondition(ks.Status.Conditions, condType)
}

func ksAccountCondition(ks *c5c3v1alpha1.KeystoneService) *metav1.Condition {
	return ksCondition(ks, conditionTypeKeystoneServiceAccountReady)
}

func ksCatalogCondition(ks *c5c3v1alpha1.KeystoneService) *metav1.Condition {
	return ksCondition(ks, conditionTypeKeystoneServiceCatalogReady)
}

// --- lifecycle ---

func TestKeystoneService_MissingCRIsNotAnError(t *testing.T) {
	g := NewGomegaWithT(t)

	// Nothing seeded at all: the CR was deleted between the event and the read.
	got, _, result, err := reconcileKeystoneService(t)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue())
	g.Expect(got).To(BeNil())
}

func TestKeystoneService_ReadErrorIsWrapped(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	boom := errors.New("apiserver unavailable")
	c := fake.NewClientBuilder().WithScheme(s).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(_ context.Context, _ client.WithWatch, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
				if _, ok := obj.(*c5c3v1alpha1.KeystoneService); ok {
					return boom
				}
				return apierrors.NewNotFound(corev1.Resource("secrets"), key.Name)
			},
		}).Build()
	r := &KeystoneServiceReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(20)}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: ksTestNamespace, Name: ksTestName},
	})

	g.Expect(err).To(HaveOccurred())
	g.Expect(errors.Is(err, boom)).To(BeTrue(), "the transport failure must stay unwrappable")
	g.Expect(err.Error()).To(ContainSubstring("fetching KeystoneService"))
}

func TestKeystoneService_InstallsFinalizerBeforeProjecting(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := keystoneServiceCR()
	ks.Finalizers = nil
	ks.Spec.Account = ksAccountSpec()
	ks.Spec.Catalog = ksCatalogSpec()

	got, c, result, err := reconcileKeystoneService(t, cp, ks, readyTenantStoreFor(cp))

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeFalse(), "the installing pass must ask to be re-run")
	g.Expect(got.Finalizers).To(ContainElement(keystoneServiceFinalizerName))

	// Nothing may be projected before the finalizer is persisted: a deletion
	// arriving now would otherwise leave orphaned Keystone state behind.
	var users orcv1alpha1.UserList
	g.Expect(c.List(context.Background(), &users)).To(Succeed())
	g.Expect(users.Items).To(BeEmpty())
	var services orcv1alpha1.ServiceList
	g.Expect(c.List(context.Background(), &services)).To(Succeed())
	g.Expect(services.Items).To(BeEmpty())
}

// --- undeclared blocks ---

func TestKeystoneService_CatalogOnlyReportsAccountNotDeclared(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := keystoneServiceCR()
	ks.Spec.Catalog = ksCatalogSpec()

	// No store is seeded at all: a catalog-only registration must never gate on
	// the delivery machinery it does not use.
	got, _ := runKeystoneService(t, cp, ks)

	account := ksAccountCondition(got)
	g.Expect(account).NotTo(BeNil())
	g.Expect(account.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(account.Reason).To(Equal(reasonKeystoneServiceAccountNotDeclared))
	g.Expect(got.Status.Account).To(BeNil())
	// The catalog block is projected and merely waiting on its probe, never on a
	// secret store.
	g.Expect(ksCatalogCondition(got).Reason).NotTo(Equal(reasonServiceAccountStoreNotReady))
}

func TestKeystoneService_AccountOnlyReportsCatalogNotDeclared(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := keystoneServiceCR()
	ks.Spec.Account = ksAccountSpec()

	got, _ := runKeystoneService(t, cp, ks, readyTenantStoreFor(cp))

	catalog := ksCatalogCondition(got)
	g.Expect(catalog).NotTo(BeNil())
	g.Expect(catalog.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(catalog.Reason).To(Equal(reasonKeystoneServiceCatalogNotDeclared))
	g.Expect(got.Status.Catalog).To(BeNil())
}

// --- the shared gates ---

func TestKeystoneService_ControlPlaneNotFound(t *testing.T) {
	g := NewGomegaWithT(t)

	ks := keystoneServiceCR()
	ks.Spec.Account = ksAccountSpec()
	ks.Spec.Catalog = ksCatalogSpec()

	// No ControlPlane seeded: GitOps ordering, not an error.
	got, c, result, err := reconcileKeystoneService(t, ks)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(korcRequeueAfter))
	for _, cond := range []*metav1.Condition{ksAccountCondition(got), ksCatalogCondition(got)} {
		g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(cond.Reason).To(Equal(reasonKeystoneServiceControlPlaneNotFound))
	}
	var users orcv1alpha1.UserList
	g.Expect(c.List(context.Background(), &users)).To(Succeed())
	g.Expect(users.Items).To(BeEmpty(), "a dangling reference must project nothing")
}

func TestKeystoneService_ControlPlaneReadErrorIsWrapped(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := keystoneServiceCR()
	ks.Spec.Account = ksAccountSpec()

	s := korcTestScheme(t)
	boom := errors.New("etcd leader election in progress")
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, ks).
		WithStatusSubresource(&c5c3v1alpha1.KeystoneService{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*c5c3v1alpha1.ControlPlane); ok {
					return boom
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	r := &KeystoneServiceReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(20)}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: ksTestNamespace, Name: ksTestName},
	})

	g.Expect(err).To(HaveOccurred())
	g.Expect(errors.Is(err, boom)).To(BeTrue())
	g.Expect(err.Error()).To(ContainSubstring("fetching ControlPlane"))
}

// ksForeignCR returns a two-block registration sitting in the foreign namespace
// tenant-a, referencing the ControlPlane explicitly. It is the shared fixture of
// the allowlist gate tests below.
func ksForeignCR(cp *c5c3v1alpha1.ControlPlane) *c5c3v1alpha1.KeystoneService {
	ks := keystoneServiceCR()
	ks.Namespace = "tenant-a"
	ks.Spec.Account = ksAccountSpec()
	ks.Spec.Catalog = ksCatalogSpec()
	ks.Spec.ControlPlaneRef.Namespace = cp.Namespace
	return ks
}

// reconcileForeignKS drives one Reconcile against the CR in tenant-a and returns
// the reloaded CR and the client, so a gate test can assert on both the
// conditions and what was (not) projected.
func reconcileForeignKS(
	t *testing.T, objs ...client.Object,
) (*c5c3v1alpha1.KeystoneService, client.Client) {
	t.Helper()
	g := NewGomegaWithT(t)
	c, r := ksClientBuilder(t, objs...)
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "tenant-a", Name: ksTestName},
	})
	g.Expect(err).NotTo(HaveOccurred())

	got := &c5c3v1alpha1.KeystoneService{}
	g.Expect(c.Get(context.Background(),
		types.NamespacedName{Namespace: "tenant-a", Name: ksTestName}, got)).To(Succeed())
	return got, c
}

// expectNamespaceNotAllowed asserts the shared gate failure on both declared
// blocks and that the pass projected nothing.
func expectNamespaceNotAllowed(t *testing.T, got *c5c3v1alpha1.KeystoneService, c client.Client) {
	t.Helper()
	g := NewGomegaWithT(t)
	for _, cond := range []*metav1.Condition{ksAccountCondition(got), ksCatalogCondition(got)} {
		g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(cond.Reason).To(Equal(reasonKeystoneServiceNamespaceNotAllowed))
	}
	var users orcv1alpha1.UserList
	g.Expect(c.List(context.Background(), &users)).To(Succeed())
	g.Expect(users.Items).To(BeEmpty(), "an unlisted namespace must project nothing")
	var services orcv1alpha1.ServiceList
	g.Expect(c.List(context.Background(), &services)).To(Succeed())
	g.Expect(services.Items).To(BeEmpty(), "an unlisted namespace must project nothing")
}

// TestKeystoneService_ForeignNamespaceIsNotAllowed covers the three ways a
// ControlPlane withholds consent from a foreign namespace: no allowlist block at
// all (the default posture), a declared block with no entries, and a block that
// lists only OTHER namespaces.
func TestKeystoneService_ForeignNamespaceIsNotAllowed(t *testing.T) {
	cases := []struct {
		name    string
		allowed *c5c3v1alpha1.ServiceRegistrationsSpec
	}{
		{name: "no serviceRegistrations block", allowed: nil},
		{name: "empty allowlist", allowed: &c5c3v1alpha1.ServiceRegistrationsSpec{}},
		{
			name: "allowlist naming only other namespaces",
			allowed: &c5c3v1alpha1.ServiceRegistrationsSpec{
				AllowedNamespaces: []string{"tenant-b", "tenant-c"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cp := ksControlPlane()
			cp.Spec.KORC.ServiceRegistrations = tc.allowed
			ks := ksForeignCR(cp)

			got, c := reconcileForeignKS(t, cp, ks, readyTenantStoreFor(cp))
			expectNamespaceNotAllowed(t, got, c)
		})
	}
}

// TestKeystoneService_NamespaceNotAllowedNamesTheRemedy pins that the condition
// alone tells an operator what to edit. Without the field path the only signal is
// a registration stuck at NamespaceNotAllowed with nothing naming the surface that
// would admit it.
func TestKeystoneService_NamespaceNotAllowedNamesTheRemedy(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksForeignCR(cp)

	got, _ := reconcileForeignKS(t, cp, ks, readyTenantStoreFor(cp))

	g.Expect(ksCatalogCondition(got).Message).To(
		ContainSubstring("spec.korc.serviceRegistrations.allowedNamespaces"))
}

// TestKeystoneService_AllowlistedForeignNamespaceIsAdmitted pins the widened
// gate: a listed namespace passes and the catalog block projects. The observable
// is the collision PROBE, not the managed Service row — ensureCatalog probes
// before it creates, so the probe is what a first pass past the gate leaves
// behind.
func TestKeystoneService_AllowlistedForeignNamespaceIsAdmitted(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	cp.Spec.KORC.ServiceRegistrations = &c5c3v1alpha1.ServiceRegistrationsSpec{
		AllowedNamespaces: []string{"tenant-a"},
	}
	ks := ksForeignCR(cp)
	ks.Spec.Account = nil // catalog-only: the account leg gates on a tenant store of its own.

	got, c := reconcileForeignKS(t, cp, ks, readyTenantStoreFor(cp))

	g.Expect(ksCatalogCondition(got).Reason).NotTo(Equal(reasonKeystoneServiceNamespaceNotAllowed))
	probe := &orcv1alpha1.Service{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{
		Namespace: keystoneServiceChildNamespace(cp), Name: keystoneServiceCatalogServiceProbeRef(ks),
	}, probe)).To(Succeed(),
		"an admitted registration projects beside the admin credential K-ORC authenticates its children with")
	g.Expect(probe.Labels).To(HaveKeyWithValue(keystoneServiceNameLabel, ks.Name))
	g.Expect(probe.Labels).To(HaveKeyWithValue(keystoneServiceNamespaceLabel, ks.Namespace))
	g.Expect(probe.OwnerReferences).To(BeEmpty(),
		"an owner reference cannot cross namespaces, so ownership is carried by the labels alone")
	g.Expect(apierrors.IsNotFound(c.Get(context.Background(), types.NamespacedName{
		Namespace: ks.Namespace, Name: keystoneServiceCatalogServiceProbeRef(ks),
	}, &orcv1alpha1.Service{}))).To(BeTrue(),
		"no K-ORC child is left in the registration's own namespace, where it could not authenticate")
}

// TestKeystoneService_DedicatedServiceNamespaceIsAdmitted pins the implicit
// layer: a namespace the ControlPlane placed one of its own services in is
// admitted without an allowlist entry. It is already the ControlPlane's, and it
// is where the built-in registrations project.
func TestKeystoneService_DedicatedServiceNamespaceIsAdmitted(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	cp.Spec.Services.Keystone.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
		Name: "tenant-a", Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
	}
	g.Expect(cp.Spec.KORC.ServiceRegistrations).To(BeNil(), "no allowlist entry may be needed")
	ks := ksForeignCR(cp)
	ks.Spec.Account = nil

	got, c := reconcileForeignKS(t, cp, ks, readyTenantStoreFor(cp))

	g.Expect(ksCatalogCondition(got).Reason).NotTo(Equal(reasonKeystoneServiceNamespaceNotAllowed))
	probe := &orcv1alpha1.Service{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{
		Namespace: keystoneServiceChildNamespace(cp), Name: keystoneServiceCatalogServiceProbeRef(ks),
	}, probe)).To(Succeed())
}

// TestKeystoneService_DeListingFreezesInsteadOfTearingDown pins decision D9: the
// gate returns BEFORE the prune sweep, so removing a namespace from the allowlist
// stops reconciliation dead instead of reaping what the registration already
// minted.
//
// The pair is what makes it a regression test. Both legs seed the SAME
// undeclared child — an endpoint row for an interface the spec no longer carries,
// which a reconciling pass is supposed to prune. De-listed, it must survive; still
// listed, the very same object is swept. A gate that ran after sweepChildren would
// pass the first leg by accident, since a frozen CR that reaps nothing and a
// reconciling CR that reaps everything look alike until the two are compared.
func TestKeystoneService_DeListingFreezesInsteadOfTearingDown(t *testing.T) {
	// The child the spec does not declare: ksCatalogSpec() carries no endpoints. It
	// is seeded where the controller projects one — in the ControlPlane's
	// namespace, marked by the ownership labels, since a foreign registration
	// cannot owner-reference across namespaces.
	undeclaredEndpoint := func(
		ks *c5c3v1alpha1.KeystoneService, cp *c5c3v1alpha1.ControlPlane,
	) *orcv1alpha1.Endpoint {
		return &orcv1alpha1.Endpoint{
			ObjectMeta: metav1.ObjectMeta{
				Name:      keystoneServiceCatalogEndpointRef(ks, c5c3v1alpha1.ExternalEndpointTypePublic),
				Namespace: keystoneServiceChildNamespace(cp),
				Labels:    keystoneServiceChildLabels(ks),
			},
		}
	}
	endpointExists := func(
		c client.Client, ks *c5c3v1alpha1.KeystoneService, cp *c5c3v1alpha1.ControlPlane,
	) error {
		return c.Get(context.Background(), types.NamespacedName{
			Namespace: keystoneServiceChildNamespace(cp),
			Name:      keystoneServiceCatalogEndpointRef(ks, c5c3v1alpha1.ExternalEndpointTypePublic),
		}, &orcv1alpha1.Endpoint{})
	}

	t.Run("de-listed: the registration freezes and the child survives", func(t *testing.T) {
		g := NewGomegaWithT(t)

		cp := ksControlPlane() // de-listed: no allowlist block left
		ks := ksForeignCR(cp)
		ks.Spec.Account = nil

		got, c := reconcileForeignKS(t, cp, ks, undeclaredEndpoint(ks, cp), readyTenantStoreFor(cp))

		g.Expect(ksCatalogCondition(got).Reason).To(Equal(reasonKeystoneServiceNamespaceNotAllowed))
		g.Expect(endpointExists(c, ks, cp)).To(Succeed(),
			"de-listing must freeze the registration, never reap what it already minted")
	})

	t.Run("still listed: the same undeclared child is pruned", func(t *testing.T) {
		g := NewGomegaWithT(t)

		cp := ksControlPlane()
		cp.Spec.KORC.ServiceRegistrations = &c5c3v1alpha1.ServiceRegistrationsSpec{
			AllowedNamespaces: []string{"tenant-a"},
		}
		ks := ksForeignCR(cp)
		ks.Spec.Account = nil

		_, c := reconcileForeignKS(t, cp, ks, undeclaredEndpoint(ks, cp), readyTenantStoreFor(cp))

		g.Expect(apierrors.IsNotFound(endpointExists(c, ks, cp))).To(BeTrue(),
			"an admitted registration prunes what its spec stopped declaring")
	})
}

func TestKeystoneService_WaitsForAdminCredential(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := korcControlPlane() // AdminCredentialReady deliberately unset
	ks := keystoneServiceCR()
	ks.Spec.Account = ksAccountSpec()
	ks.Spec.Catalog = ksCatalogSpec()

	got, c := runKeystoneService(t, cp, ks, readyTenantStoreFor(cp))

	for _, cond := range []*metav1.Condition{ksAccountCondition(got), ksCatalogCondition(got)} {
		g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(cond.Reason).To(Equal(reasonWaitingForServiceAccountAdmin))
	}
	var services orcv1alpha1.ServiceList
	g.Expect(c.List(context.Background(), &services)).To(Succeed())
	g.Expect(services.Items).To(BeEmpty())
}

func TestKeystoneService_AccountWaitsForSecretStore(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := keystoneServiceCR()
	ks.Spec.Account = ksAccountSpec()

	// No tenant store seeded.
	got, _ := runKeystoneService(t, cp, ks)

	cond := ksAccountCondition(got)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonServiceAccountStoreNotReady))
}

func TestKeystoneService_StoreReadErrorIsReported(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	cp.Spec.SecretStoreRef = &ksUnknownStoreRef
	ks := keystoneServiceCR()
	ks.Spec.Account = ksAccountSpec()

	got, _, _, err := reconcileKeystoneService(t, cp, ks)

	g.Expect(err).To(HaveOccurred())
	cond := ksAccountCondition(got)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonServiceAccountError))
}

// --- naming ---

func TestKeystoneService_ChildPrefixIsStableAndNamespaceQualified(t *testing.T) {
	g := NewGomegaWithT(t)

	here := keystoneServiceCR()
	elsewhere := keystoneServiceCR()
	elsewhere.Namespace = "tenant-a"

	g.Expect(keystoneServiceChildPrefix(here)).To(Equal(keystoneServiceChildPrefix(here)),
		"the prefix must be a pure function of the CR's identity")
	g.Expect(keystoneServiceChildPrefix(here)).NotTo(Equal(keystoneServiceChildPrefix(elsewhere)),
		"same-named CRs in different namespaces must not alias each other's children")
	g.Expect(keystoneServiceChildPrefix(here)).To(HavePrefix(ksTestName+"-"),
		"the readable base must stay in front of the hash")
	g.Expect(keystoneServiceChildPrefix(here)).To(HaveSuffix("-registration-"))
}

func TestKeystoneService_ConsumerSecretIsNamedForItsConsumers(t *testing.T) {
	g := NewGomegaWithT(t)

	ks := keystoneServiceCR()

	g.Expect(keystoneServiceCredentialsSecretName(ks)).To(Equal(ksTestName + "-credentials"))
}

func TestKeystoneService_RemoteKeyFitsTheTenantPolicyPath(t *testing.T) {
	g := NewGomegaWithT(t)

	ks := keystoneServiceCR()

	// The templated eso-tenant policy admits
	// openstack/keystone/{caller-namespace}/+/service-accounts/+ and nothing else.
	g.Expect(keystoneServiceRemoteKeyFor(ks)).To(Equal(
		"openstack/keystone/" + ksTestNamespace + "/" + ksTestName + "/service-accounts/credentials"))
	g.Expect(strings.Split(keystoneServiceRemoteKeyFor(ks), "/")).To(HaveLen(6))
}

// --- prune ---

func TestKeystoneService_PrunesARemovedEndpointInterface(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := keystoneServiceCR()
	ks.Spec.Catalog = ksCatalogSpec()
	ks.Spec.Catalog.Endpoints = []c5c3v1alpha1.KeystoneServiceEndpointSpec{
		{Interface: c5c3v1alpha1.ExternalEndpointTypePublic, URL: "https://image.example/public"},
	}

	// The internal interface was dropped from the spec; its row must leave the
	// catalog rather than linger as an address nothing serves.
	stale := &orcv1alpha1.Endpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:            keystoneServiceCatalogEndpointRef(ks, c5c3v1alpha1.ExternalEndpointTypeInternal),
			Namespace:       ks.Namespace,
			OwnerReferences: ownedByKS(ks),
		},
	}

	got, c := runKeystoneService(t, append(ksConvergedCatalog(ks), cp, ks, readyTenantStoreFor(cp), stale)...)

	err := c.Get(context.Background(), types.NamespacedName{
		Name:      keystoneServiceCatalogEndpointRef(ks, c5c3v1alpha1.ExternalEndpointTypeInternal),
		Namespace: ks.Namespace,
	}, &orcv1alpha1.Endpoint{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the undeclared endpoint interface must be pruned")

	// The declared interface survives.
	g.Expect(c.Get(context.Background(), types.NamespacedName{
		Name:      keystoneServiceCatalogEndpointRef(ks, c5c3v1alpha1.ExternalEndpointTypePublic),
		Namespace: ks.Namespace,
	}, &orcv1alpha1.Endpoint{})).To(Succeed())

	cond := ksCatalogCondition(got)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForCatalog))
	g.Expect(cond.Message).To(ContainSubstring("still being removed"))
}

func TestKeystoneService_PrunesUndeclaredRoleChildren(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := keystoneServiceCR()
	ks.Spec.Account = ksAccountSpec()
	ks.Spec.Account.Roles = []string{"member"}

	// A role the spec no longer declares, left over from an earlier generation.
	stale := &orcv1alpha1.RoleAssignment{
		ObjectMeta: metav1.ObjectMeta{
			Name:            keystoneServiceRoleAssignmentRef(ks, "admin"),
			Namespace:       ks.Namespace,
			OwnerReferences: ownedByKS(ks),
		},
	}
	staleImport := &orcv1alpha1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:            keystoneServiceRoleImportRef(ks, "admin"),
			Namespace:       ks.Namespace,
			OwnerReferences: ownedByKS(ks),
		},
	}

	got, c := runKeystoneService(t, append(ksConvergedAccount(ks, cp), cp, ks, readyTenantStoreFor(cp), stale, staleImport)...)

	for _, name := range []string{
		keystoneServiceRoleAssignmentRef(ks, "admin"),
		keystoneServiceRoleImportRef(ks, "admin"),
	} {
		err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ks.Namespace}, &orcv1alpha1.Role{})
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the undeclared child %q must be pruned", name)
	}
	cond := ksAccountCondition(got)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonWaitingForServiceAccounts))
	g.Expect(cond.Message).To(ContainSubstring("still being removed"))
}

func TestKeystoneService_PruneLeavesForeignObjectsAlone(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := keystoneServiceCR()
	ks.Spec.Catalog = ksCatalogSpec()

	// Right prefix, no owner reference: somebody else's object.
	unowned := &orcv1alpha1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: keystoneServiceChildPrefix(ks) + "service-old", Namespace: ks.Namespace},
	}
	// Owned, but not a name this CR ever projects.
	misnamed := &orcv1alpha1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "somebody-elses-service",
			Namespace:       ks.Namespace,
			OwnerReferences: ownedByKS(ks),
		},
	}

	_, c := runKeystoneService(t, cp, ks, readyTenantStoreFor(cp), unowned, misnamed)

	for _, name := range []string{unowned.Name, misnamed.Name} {
		err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ks.Namespace}, &orcv1alpha1.Service{})
		g.Expect(err).NotTo(HaveOccurred(), "%q fails one half of the ownership test and must survive", name)
	}
}

func TestKeystoneService_PrunesTheChildrenOfARemovedBlock(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := keystoneServiceCR()
	ks.Spec.Catalog = ksCatalogSpec() // the account block was removed from the spec

	leftover := &orcv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{
			Name:            keystoneServiceUserRef(ks),
			Namespace:       ks.Namespace,
			OwnerReferences: ownedByKS(ks),
		},
	}
	leftoverPassword := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            keystoneServicePasswordSecretName(ks, 1),
			Namespace:       ks.Namespace,
			OwnerReferences: ownedByKS(ks),
		},
	}

	got, c := runKeystoneService(t, cp, ks, readyTenantStoreFor(cp), leftover, leftoverPassword)

	err := c.Get(context.Background(),
		types.NamespacedName{Name: keystoneServiceUserRef(ks), Namespace: ks.Namespace}, &orcv1alpha1.User{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the removed block's user must be pruned")
	err = c.Get(context.Background(),
		types.NamespacedName{Name: keystoneServicePasswordSecretName(ks, 1), Namespace: ks.Namespace}, &corev1.Secret{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "its password Secret goes with it")

	// The block is undeclared, so its condition holds a bounded wait until the
	// removal completes rather than flipping straight to NotDeclared.
	cond := ksAccountCondition(got)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonWaitingForServiceAccounts))
}

func TestKeystoneService_DeclaredPasswordSecretsSurviveThePrune(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := keystoneServiceCR()
	ks.Spec.Account = ksAccountSpec()

	_, c := runKeystoneService(t, append(ksConvergedAccount(ks, cp), cp, ks, readyTenantStoreFor(cp))...)

	// The current generation belongs to ensureKeystoneServiceUser's lifecycle, not
	// to the sweep: pruning it would delete the password the managed User points at.
	err := c.Get(context.Background(),
		types.NamespacedName{Name: keystoneServicePasswordSecretName(ks, 1), Namespace: ks.Namespace}, &corev1.Secret{})
	g.Expect(err).NotTo(HaveOccurred())
}

// --- teardown ---

func TestKeystoneService_DeleteRemovesChildrenThenReleasesTheFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := keystoneServiceCR()
	ks.Spec.Account = ksAccountSpec()
	ks.DeletionTimestamp = ptr.To(metav1.Now())

	user := &orcv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{
			Name:            keystoneServiceUserRef(ks),
			Namespace:       ks.Namespace,
			OwnerReferences: ownedByKS(ks),
			Finalizers:      []string{"openstack.k-orc.cloud/user"},
		},
	}

	c, r := ksClientBuilder(t, cp, ks, user)
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: ksTestNamespace, Name: ksTestName},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(korcRequeueAfter), "the pass that issued the deletes must requeue")

	// The CR is still held: K-ORC's finalizer keeps the User Terminating while it
	// removes the Keystone identity.
	got := &c5c3v1alpha1.KeystoneService{}
	g.Expect(c.Get(context.Background(),
		types.NamespacedName{Namespace: ksTestNamespace, Name: ksTestName}, got)).To(Succeed())
	g.Expect(got.Finalizers).To(ContainElement(keystoneServiceFinalizerName))

	// Once K-ORC releases the child, the next pass releases the CR.
	live := &orcv1alpha1.User{}
	g.Expect(c.Get(context.Background(),
		types.NamespacedName{Name: user.Name, Namespace: user.Namespace}, live)).To(Succeed())
	live.Finalizers = nil
	g.Expect(c.Update(context.Background(), live)).To(Succeed())

	_, err = r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: ksTestNamespace, Name: ksTestName},
	})
	g.Expect(err).NotTo(HaveOccurred())
	err = c.Get(context.Background(), types.NamespacedName{Namespace: ksTestNamespace, Name: ksTestName}, got)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the CR is collected once the finalizer is released")
}

func TestKeystoneService_DeleteRemovesDependentsFirst(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := keystoneServiceCR()
	ks.Spec.Account = ksAccountSpec()
	ks.Spec.Account.Roles = []string{"member"}
	ks.Spec.Catalog = ksCatalogSpec()
	ks.Spec.Catalog.Endpoints = []c5c3v1alpha1.KeystoneServiceEndpointSpec{
		{Interface: c5c3v1alpha1.ExternalEndpointTypePublic, URL: "https://image.example/public"},
	}
	ks.DeletionTimestamp = ptr.To(metav1.Now())

	children := []client.Object{
		&orcv1alpha1.RoleAssignment{ObjectMeta: metav1.ObjectMeta{
			Name: keystoneServiceRoleAssignmentRef(ks, "member"), Namespace: ks.Namespace, OwnerReferences: ownedByKS(ks),
		}},
		&orcv1alpha1.Role{ObjectMeta: metav1.ObjectMeta{
			Name: keystoneServiceRoleImportRef(ks, "member"), Namespace: ks.Namespace, OwnerReferences: ownedByKS(ks),
		}},
		&orcv1alpha1.Endpoint{ObjectMeta: metav1.ObjectMeta{
			Name:      keystoneServiceCatalogEndpointRef(ks, c5c3v1alpha1.ExternalEndpointTypePublic),
			Namespace: ks.Namespace, OwnerReferences: ownedByKS(ks),
		}},
		&orcv1alpha1.Service{ObjectMeta: metav1.ObjectMeta{
			Name: keystoneServiceCatalogServiceRef(ks), Namespace: ks.Namespace, OwnerReferences: ownedByKS(ks),
		}},
		&orcv1alpha1.User{ObjectMeta: metav1.ObjectMeta{
			Name: keystoneServiceUserRef(ks), Namespace: ks.Namespace, OwnerReferences: ownedByKS(ks),
		}},
		&orcv1alpha1.Project{ObjectMeta: metav1.ObjectMeta{
			Name: keystoneServiceProjectRef(ks), Namespace: ks.Namespace, OwnerReferences: ownedByKS(ks),
		}},
	}

	var order []string
	s := korcTestScheme(t)
	seeded := append([]client.Object{cp, ks}, children...)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(seeded...).
		WithStatusSubresource(&c5c3v1alpha1.KeystoneService{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				order = append(order, obj.GetObjectKind().GroupVersionKind().Kind+"/"+obj.GetName())
				return cl.Delete(ctx, obj, opts...)
			},
		}).Build()
	r := &KeystoneServiceReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(20)}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: ksTestNamespace, Name: ksTestName},
	})
	g.Expect(err).NotTo(HaveOccurred())

	// A dependent must be deleted before the object it references: an assignment
	// before its role, an endpoint before the service it hangs off, and both
	// before the user and project they bind.
	indexOf := func(suffix string) int {
		for i, name := range order {
			if strings.HasSuffix(name, suffix) {
				return i
			}
		}
		return -1
	}
	assignment := indexOf(keystoneServiceRoleAssignmentRef(ks, "member"))
	role := indexOf(keystoneServiceRoleImportRef(ks, "member"))
	endpoint := indexOf(keystoneServiceCatalogEndpointRef(ks, c5c3v1alpha1.ExternalEndpointTypePublic))
	service := indexOf(keystoneServiceCatalogServiceRef(ks))
	user := indexOf(keystoneServiceUserRef(ks))
	project := indexOf(keystoneServiceProjectRef(ks))

	g.Expect(order).To(HaveLen(len(children)))
	g.Expect(assignment).To(BeNumerically("<", role))
	g.Expect(endpoint).To(BeNumerically("<", service))
	g.Expect(assignment).To(BeNumerically("<", user))
	g.Expect(user).To(BeNumerically("<", project))
}

func TestKeystoneService_DeleteWithoutChildrenReleasesImmediately(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := keystoneServiceCR()
	ks.Spec.Account = ksAccountSpec()
	ks.DeletionTimestamp = ptr.To(metav1.Now())

	c, r := ksClientBuilder(t, cp, ks)
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: ksTestNamespace, Name: ksTestName},
	})

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue(), "a CR that projected nothing must not requeue")
	err = c.Get(context.Background(),
		types.NamespacedName{Namespace: ksTestNamespace, Name: ksTestName}, &c5c3v1alpha1.KeystoneService{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
}

func TestKeystoneService_DeleteFailsOpenWhenTheControlPlaneIsGone(t *testing.T) {
	g := NewGomegaWithT(t)

	ks := keystoneServiceCR()
	ks.Spec.Account = ksAccountSpec()
	ks.DeletionTimestamp = ptr.To(metav1.Now())

	// A K-ORC child that will never finish deleting: without its ControlPlane
	// there are no credentials left to reach Keystone with.
	user := &orcv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{
			Name:            keystoneServiceUserRef(ks),
			Namespace:       ks.Namespace,
			OwnerReferences: ownedByKS(ks),
			Finalizers:      []string{"openstack.k-orc.cloud/user"},
		},
	}

	c, r := ksClientBuilder(t, ks, user) // no ControlPlane
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: ksTestNamespace, Name: ksTestName},
	})

	g.Expect(err).NotTo(HaveOccurred())
	err = c.Get(context.Background(),
		types.NamespacedName{Namespace: ksTestNamespace, Name: ksTestName}, &c5c3v1alpha1.KeystoneService{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
		"the finalizer must not hold the CR hostage to a plane that no longer exists")
}

func TestKeystoneService_DeleteWithoutFinalizerIsANoOp(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := keystoneServiceCR()
	ks.Finalizers = nil
	ks.DeletionTimestamp = ptr.To(metav1.Now())
	ks.Spec.Account = ksAccountSpec()

	// A CR the fake client keeps only because it still carries our finalizer would
	// vanish here; seeding it without one exercises the early return.
	s := korcTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).
		WithStatusSubresource(&c5c3v1alpha1.KeystoneService{}).Build()
	r := &KeystoneServiceReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(20)}
	result, err := r.reconcileDelete(context.Background(), ks)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue())
}

// --- the aggregate ---

func TestKeystoneService_AggregateReadyOnceBothBlocksConverge(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := keystoneServiceCR()
	ks.Spec.Account = ksAccountSpec()
	ks.Spec.Catalog = ksCatalogSpec()

	seeded := append(ksConvergedAccount(ks, cp), ksConvergedCatalog(ks)...)
	got, _ := runKeystoneService(t, append(seeded, cp, ks, readyTenantStoreFor(cp))...)

	g.Expect(ksAccountCondition(got).Status).To(Equal(metav1.ConditionTrue))
	g.Expect(ksAccountCondition(got).Reason).To(Equal(reasonKeystoneServiceAccountProvisioned))
	g.Expect(ksCatalogCondition(got).Status).To(Equal(metav1.ConditionTrue))
	g.Expect(ksCatalogCondition(got).Reason).To(Equal(reasonKeystoneServiceCatalogRegistered))

	ready := ksCondition(got, conditionTypeReady)
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(got.Status.ObservedGeneration).To(Equal(ks.Generation))
}

func TestKeystoneService_AggregateNotReadyWhileOneBlockIsPending(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := keystoneServiceCR()
	ks.Spec.Account = ksAccountSpec()
	ks.Spec.Catalog = ksCatalogSpec()

	// Only the account converges; the catalog is still probing.
	got, _ := runKeystoneService(t, append(ksConvergedAccount(ks, cp), cp, ks, readyTenantStoreFor(cp))...)

	g.Expect(ksAccountCondition(got).Status).To(Equal(metav1.ConditionTrue))
	g.Expect(ksCatalogCondition(got).Status).To(Equal(metav1.ConditionFalse))
	g.Expect(ksCondition(got, conditionTypeReady).Status).To(Equal(metav1.ConditionFalse))
}

// --- shared fixtures ---

// ksConvergedAccount seeds everything the account block needs to reach
// AccountProvisioned in one pass: the referenced project, the managed user with
// generation 1 applied, the password Secret behind it, and the delivery objects
// ESO would have produced.
func ksConvergedAccount(ks *c5c3v1alpha1.KeystoneService, cp *c5c3v1alpha1.ControlPlane) []client.Object {
	password := []byte("seeded-password")
	user := &orcv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{
			Name:      keystoneServiceUserRef(ks),
			Namespace: ks.Namespace,
			// A live object always carries a creation timestamp, and the generation
			// stamp reads it to tell a fresh create from a steady-state pass; the fake
			// client does not set one, so the fixture must.
			CreationTimestamp: metav1.Now(),
			Annotations:       map[string]string{serviceAccountPasswordGenerationAnnotation: "1"},
			OwnerReferences:   ownedByKS(ks),
		},
		Spec: orcv1alpha1.UserSpec{
			ManagementPolicy: orcv1alpha1.ManagementPolicyManaged,
			Resource: &orcv1alpha1.UserResourceSpec{
				PasswordRef: ptr.To(orcv1alpha1.KubernetesNameRef(keystoneServicePasswordSecretName(ks, 1))),
			},
		},
		Status: orcv1alpha1.UserStatus{
			Conditions: availableImportConditions(),
			ID:         ptr.To("ks-user-id"),
			Resource:   &orcv1alpha1.UserResourceStatus{AppliedPasswordRef: keystoneServicePasswordSecretName(ks, 1)},
		},
	}
	project := &orcv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: keystoneServiceProjectRef(ks), Namespace: ks.Namespace},
		Status:     orcv1alpha1.ProjectStatus{Conditions: availableImportConditions(), ID: ptr.To("ks-project-id")},
	}
	pw := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            keystoneServicePasswordSecretName(ks, 1),
			Namespace:       ks.Namespace,
			OwnerReferences: ownedByKS(ks),
		},
		Data: map[string][]byte{serviceAccountPasswordKey: password},
	}
	push := &esov1alpha1.PushSecret{
		ObjectMeta: metav1.ObjectMeta{Name: keystoneServicePushSecretName(ks), Namespace: ks.Namespace},
		Status: esov1alpha1.PushSecretStatus{
			Conditions:            []esov1alpha1.PushSecretStatusCondition{{Type: esov1alpha1.PushSecretReady, Status: corev1.ConditionTrue}},
			SyncedResourceVersion: "1",
		},
	}
	materialized := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: keystoneServiceCredentialsSecretName(ks), Namespace: ks.Namespace},
		Data:       map[string][]byte{serviceAccountPasswordKey: password},
	}
	return []client.Object{user, project, pw, push, materialized}
}

// ksConvergedCatalog seeds the catalog children as K-ORC reports them once the
// rows exist.
func ksConvergedCatalog(ks *c5c3v1alpha1.KeystoneService) []client.Object {
	service := &orcv1alpha1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:            keystoneServiceCatalogServiceRef(ks),
			Namespace:       ks.Namespace,
			OwnerReferences: ownedByKS(ks),
		},
		Status: orcv1alpha1.ServiceStatus{Conditions: availableImportConditions(), ID: ptr.To("ks-service-id")},
	}
	return []client.Object{service}
}

// ksUnknownStoreRef selects a store kind IsStoreRefReady refuses to resolve,
// which is how a store READ failure is provoked without an interceptor.
var ksUnknownStoreRef = commonv1.SecretStoreRefSpec{Kind: "NotAStoreKind", Name: "openbao"}

// --- manager wiring: extractor and watch mappers ---

// newKeystoneServiceMapperClient returns a fake client pre-registered with the
// KeystoneServiceControlPlaneRefIndexKey field indexer so
// controlPlaneToKeystoneServicesMapper can resolve its MatchingFields lookup,
// mirroring newControlPlaneMapperClient.
func newKeystoneServiceMapperClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(korcTestScheme(t)).
		WithObjects(objs...).
		WithIndex(&c5c3v1alpha1.KeystoneService{}, KeystoneServiceControlPlaneRefIndexKey, keystoneServiceControlPlaneRefExtractor).
		Build()
}

// mapperKeystoneService builds a minimal KeystoneService referencing the named
// ControlPlane. An empty refNamespace leaves the reference namespace unset, the
// shape the defaulting contract resolves to the CR's own namespace.
func mapperKeystoneService(name, namespace, refName, refNamespace string) *c5c3v1alpha1.KeystoneService {
	return &c5c3v1alpha1.KeystoneService{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: types.UID(name + "-uid")},
		Spec: c5c3v1alpha1.KeystoneServiceSpec{
			ControlPlaneRef: c5c3v1alpha1.ControlPlaneRefSpec{Name: refName, Namespace: refNamespace},
		},
	}
}

func TestKeystoneServiceControlPlaneRefExtractor_ExplicitNamespace(t *testing.T) {
	g := NewGomegaWithT(t)

	ks := mapperKeystoneService("reg", "team-a", "cp", "plane-ns")
	g.Expect(keystoneServiceControlPlaneRefExtractor(ks)).To(ConsistOf("plane-ns/cp"),
		"an explicit reference namespace must qualify the index value")
}

func TestKeystoneServiceControlPlaneRefExtractor_DefaultsNamespace(t *testing.T) {
	g := NewGomegaWithT(t)

	ks := mapperKeystoneService("reg", "team-a", "cp", "")
	g.Expect(keystoneServiceControlPlaneRefExtractor(ks)).To(ConsistOf("team-a/cp"),
		"an omitted reference namespace must default to the CR's own, as resolveControlPlane does")
}

func TestKeystoneServiceControlPlaneRefExtractor_EmptyNameIndexesNothing(t *testing.T) {
	g := NewGomegaWithT(t)

	ks := mapperKeystoneService("reg", "team-a", "", "")
	g.Expect(keystoneServiceControlPlaneRefExtractor(ks)).To(BeEmpty(),
		"an admission-bypassed CR without a reference name must not index a dangling namespace key")
}

func TestControlPlaneToKeystoneServicesMapper_EnqueuesReferencingRegistrations(t *testing.T) {
	g := NewGomegaWithT(t)

	// Two referencing registrations — one with an explicit reference namespace,
	// one relying on the same-namespace default — and one referencing another
	// plane, which must stay out of the result.
	c := newKeystoneServiceMapperClient(t,
		mapperKeystoneService("reg-explicit", "default", "cp", "default"),
		mapperKeystoneService("reg-defaulted", "default", "cp", ""),
		mapperKeystoneService("reg-other", "default", "other-cp", ""),
	)
	mapper := controlPlaneToKeystoneServicesMapper(c)

	cp := &c5c3v1alpha1.ControlPlane{ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: "default"}}
	g.Expect(mapper(context.Background(), cp)).To(ConsistOf(
		reconcile.Request{NamespacedName: client.ObjectKey{Namespace: "default", Name: "reg-explicit"}},
		reconcile.Request{NamespacedName: client.ObjectKey{Namespace: "default", Name: "reg-defaulted"}},
	), "exactly the registrations whose resolved reference names the event ControlPlane must be enqueued")
}

func TestControlPlaneToKeystoneServicesMapper_ReturnsNothingWithoutReferrers(t *testing.T) {
	g := NewGomegaWithT(t)

	c := newKeystoneServiceMapperClient(t)
	mapper := controlPlaneToKeystoneServicesMapper(c)

	cp := &c5c3v1alpha1.ControlPlane{ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: "default"}}
	g.Expect(mapper(context.Background(), cp)).To(BeEmpty(),
		"a ControlPlane nothing references must map to no requests")
}

func TestControlPlaneToKeystoneServicesMapper_IgnoresSameNamedControlPlaneElsewhere(t *testing.T) {
	g := NewGomegaWithT(t)

	c := newKeystoneServiceMapperClient(t,
		mapperKeystoneService("reg", "team-a", "cp", ""),
	)
	mapper := controlPlaneToKeystoneServicesMapper(c)

	// Same ControlPlane name, different namespace: the namespace-qualified index
	// key must not match the team-a registration.
	cp := &c5c3v1alpha1.ControlPlane{ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: "team-b"}}
	g.Expect(mapper(context.Background(), cp)).To(BeEmpty(),
		"a same-named ControlPlane in another namespace must not wake the registration")
}

func TestControlPlaneToKeystoneServicesMapper_ReturnsNilOnListError(t *testing.T) {
	g := NewGomegaWithT(t)

	c := fake.NewClientBuilder().
		WithScheme(korcTestScheme(t)).
		WithIndex(&c5c3v1alpha1.KeystoneService{}, KeystoneServiceControlPlaneRefIndexKey, keystoneServiceControlPlaneRefExtractor).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
				return errors.New("cache down")
			},
		}).
		Build()
	mapper := controlPlaneToKeystoneServicesMapper(c)

	cp := &c5c3v1alpha1.ControlPlane{ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: "default"}}
	g.Expect(mapper(context.Background(), cp)).To(BeNil(),
		"a List failure must map to nothing; the periodic requeues are the fallback")
}

func TestCredentialsSecretToKeystoneServiceMapper_EnqueuesOwningRegistration(t *testing.T) {
	g := NewGomegaWithT(t)

	c := newKeystoneServiceMapperClient(t, mapperKeystoneService("reg", "default", "cp", ""))
	mapper := credentialsSecretToKeystoneServiceMapper(c)

	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "reg-credentials", Namespace: "default"}}
	g.Expect(mapper(context.Background(), secret)).To(ConsistOf(
		reconcile.Request{NamespacedName: client.ObjectKey{Namespace: "default", Name: "reg"}},
	), "the consumer Secret must enqueue exactly the registration its name contract points at")
}

func TestCredentialsSecretToKeystoneServiceMapper_IgnoresNonCredentialsNames(t *testing.T) {
	g := NewGomegaWithT(t)

	// The Get interceptor proves the mapper never reads for a Secret outside the
	// name contract: reaching it would fail the assertion below.
	got := false
	c := fake.NewClientBuilder().
		WithScheme(korcTestScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
				got = true
				return errors.New("unexpected read")
			},
		}).
		Build()
	mapper := credentialsSecretToKeystoneServiceMapper(c)

	for _, name := range []string{"reg-password-v1", "-credentials"} {
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}}
		g.Expect(mapper(context.Background(), secret)).To(BeNil(),
			"Secret %q is outside the name contract and must map to nothing", name)
	}
	g.Expect(got).To(BeFalse(), "no Get may be issued for a Secret outside the name contract")
}

func TestCredentialsSecretToKeystoneServiceMapper_IgnoresUnknownRegistration(t *testing.T) {
	g := NewGomegaWithT(t)

	c := newKeystoneServiceMapperClient(t)
	mapper := credentialsSecretToKeystoneServiceMapper(c)

	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "reg-credentials", Namespace: "default"}}
	g.Expect(mapper(context.Background(), secret)).To(BeNil(),
		"a -credentials Secret without a matching registration is somebody else's Secret")
}

func TestCredentialsSecretToKeystoneServiceMapper_ReturnsNilOnGetError(t *testing.T) {
	g := NewGomegaWithT(t)

	c := fake.NewClientBuilder().
		WithScheme(korcTestScheme(t)).
		WithObjects(mapperKeystoneService("reg", "default", "cp", "")).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
				return errors.New("cache down")
			},
		}).
		Build()
	mapper := credentialsSecretToKeystoneServiceMapper(c)

	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "reg-credentials", Namespace: "default"}}
	g.Expect(mapper(context.Background(), secret)).To(BeNil(),
		"a non-NotFound Get failure must map to nothing; the periodic requeues are the fallback")
}

// --- where the children land ---

// TestKeystoneService_ForeignChildrenProjectBesideTheAdminCredential pins the
// placement the whole cross-namespace mechanism rests on. K-ORC reads the
// clouds.yaml named by a child's cloudCredentialsRef, and resolves its domainRef,
// in the CHILD's own namespace. The admin credential is materialized once, in the
// ControlPlane's namespace, and stays there — copying it into every allowlisted
// namespace would hand each tenant the cloud-admin credential the registration
// allowlist exists to withhold. So the children go to the credential.
//
// Delivery is the other direction: the consumer Secret and the ESO objects that
// materialize it belong to the service that reads them, in the CR's own namespace,
// on that namespace's tenant store.
func TestKeystoneService_ForeignChildrenProjectBesideTheAdminCredential(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	cp.Spec.KORC.ServiceRegistrations = &c5c3v1alpha1.ServiceRegistrationsSpec{
		AllowedNamespaces: []string{"tenant-a"},
	}
	ks := ksForeignCR(cp)
	ks.Spec.Catalog = nil // account-only: the catalog leg has its own placement test.

	// An absent user probe lets the pass reach the managed User in one reconcile.
	userProbe := &orcv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{
			Name: keystoneServiceUserProbeRef(ks), Namespace: keystoneServiceChildNamespace(cp),
			Labels: keystoneServiceChildLabels(ks),
		},
		Status: orcv1alpha1.UserStatus{Conditions: pendingImportConditions(0)},
	}
	got, c := reconcileForeignKS(t, cp, ks, userProbe,
		readyTenantSecretStore(esoTenantStoreName, ks.Namespace, "", ""))

	user := &orcv1alpha1.User{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{
		Namespace: keystoneServiceChildNamespace(cp), Name: keystoneServiceUserRef(ks),
	}, user)).To(Succeed(), "the managed User belongs where K-ORC can read the admin credential")
	g.Expect(user.Labels).To(HaveKeyWithValue(keystoneServiceNameLabel, ks.Name))
	g.Expect(user.Labels).To(HaveKeyWithValue(keystoneServiceNamespaceLabel, ks.Namespace))
	g.Expect(user.OwnerReferences).To(BeEmpty(),
		"Kubernetes rejects an owner reference across namespaces, so the labels carry ownership")
	g.Expect(string(*user.Spec.Resource.DomainRef)).To(Equal(adminDomainRef(cp)),
		"the admin Domain import it resolves against lives in that same namespace")

	// The generation-scoped password Secret follows the User: K-ORC resolves
	// passwordRef in the User's namespace too.
	g.Expect(c.Get(context.Background(), types.NamespacedName{
		Namespace: keystoneServiceChildNamespace(cp), Name: keystoneServicePasswordSecretName(ks, 1),
	}, &corev1.Secret{})).To(Succeed())

	// Nothing K-ORC is left behind in the tenant's namespace, where none of it
	// could authenticate.
	var tenantUsers orcv1alpha1.UserList
	g.Expect(c.List(context.Background(), &tenantUsers, client.InNamespace(ks.Namespace))).To(Succeed())
	g.Expect(tenantUsers.Items).To(BeEmpty(), "no K-ORC child may sit in the registration's own namespace")
	var tenantProjects orcv1alpha1.ProjectList
	g.Expect(c.List(context.Background(), &tenantProjects, client.InNamespace(ks.Namespace))).To(Succeed())
	g.Expect(tenantProjects.Items).To(BeEmpty())
	var tenantDomains orcv1alpha1.DomainList
	g.Expect(c.List(context.Background(), &tenantDomains, client.InNamespace(ks.Namespace))).To(Succeed())
	g.Expect(tenantDomains.Items).To(BeEmpty())

	// The pass got past every gate and is waiting on K-ORC rather than failing:
	// the delivery leg runs once the User reports the current password applied.
	g.Expect(ksAccountCondition(got).Reason).To(Equal(reasonWaitingForServiceAccounts))
}

// TestKeystoneService_CoLocatedChildKeepsItsOwnerReference is the other half of
// the placement contract: a registration in the ControlPlane's own namespace is
// unchanged by the move, because its namespace already IS the child namespace. It
// keeps the owner reference, so the garbage collector still reaps its children.
func TestKeystoneService_CoLocatedChildKeepsItsOwnerReference(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := keystoneServiceCR()
	ks.Spec.Catalog = ksCatalogSpec()
	g.Expect(ks.Namespace).To(Equal(keystoneServiceChildNamespace(cp)), "the fixture must be co-located")

	c, r := ksClientBuilder(t, cp, ks, readyTenantStoreFor(cp))
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: ks.Namespace, Name: ks.Name},
	})
	g.Expect(err).NotTo(HaveOccurred())

	probe := &orcv1alpha1.Service{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{
		Namespace: ks.Namespace, Name: keystoneServiceCatalogServiceProbeRef(ks),
	}, probe)).To(Succeed())
	g.Expect(metav1.IsControlledBy(probe, ks)).To(BeTrue(),
		"a co-located child is still owner-referenced, so GC reaps it")
	g.Expect(probe.Labels).To(HaveKeyWithValue(keystoneServiceNameLabel, ks.Name),
		"it carries the labels too, so one watch leg reaches both placements")
}

// TestKeystoneService_TeardownReapsLabelOwnedChildren proves the deletion path
// follows the children to the ControlPlane's namespace. Nothing else can: a
// cross-namespace child has no owner reference, so no garbage collection cascade
// reaches it once the CR is gone.
func TestKeystoneService_TeardownReapsLabelOwnedChildren(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksForeignCR(cp)
	ks.DeletionTimestamp = ptr.To(metav1.Now())
	ks.Finalizers = []string{keystoneServiceFinalizerName}

	child := &orcv1alpha1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      keystoneServiceCatalogServiceRef(ks),
			Namespace: keystoneServiceChildNamespace(cp),
			Labels:    keystoneServiceChildLabels(ks),
		},
	}
	stranger := &orcv1alpha1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      keystoneServiceCatalogServiceRef(ks) + "-not-ours",
			Namespace: keystoneServiceChildNamespace(cp),
		},
	}

	c, r := ksClientBuilder(t, cp, ks, child, stranger)
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: ks.Namespace, Name: ks.Name},
	})
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(apierrors.IsNotFound(c.Get(context.Background(), types.NamespacedName{
		Namespace: keystoneServiceChildNamespace(cp), Name: keystoneServiceCatalogServiceRef(ks),
	}, &orcv1alpha1.Service{}))).To(BeTrue(), "the label-owned child is torn down with the registration")
	g.Expect(c.Get(context.Background(), types.NamespacedName{
		Namespace: keystoneServiceChildNamespace(cp), Name: stranger.Name,
	}, &orcv1alpha1.Service{})).To(Succeed(), "an unlabelled neighbour is never touched")
}

func TestKeystoneServiceChildToRequest(t *testing.T) {
	g := NewGomegaWithT(t)

	labelled := &orcv1alpha1.User{ObjectMeta: metav1.ObjectMeta{
		Name: "child", Namespace: "default",
		Labels: map[string]string{
			keystoneServiceNameLabel:      "reg",
			keystoneServiceNamespaceLabel: "tenant-a",
		},
	}}
	g.Expect(keystoneServiceChildToRequest(context.Background(), labelled)).To(Equal([]reconcile.Request{{
		NamespacedName: types.NamespacedName{Namespace: "tenant-a", Name: "reg"},
	}}))

	// A child of the ControlPlane's own inline machinery shares these namespaces
	// and must map to nothing, as must a half-labelled object.
	for _, obj := range []client.Object{
		&orcv1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: "inline", Namespace: "default"}},
		&orcv1alpha1.User{ObjectMeta: metav1.ObjectMeta{
			Name: "half", Namespace: "default",
			Labels: map[string]string{keystoneServiceNameLabel: "reg"},
		}},
	} {
		g.Expect(keystoneServiceChildToRequest(context.Background(), obj)).To(BeEmpty())
	}
}
