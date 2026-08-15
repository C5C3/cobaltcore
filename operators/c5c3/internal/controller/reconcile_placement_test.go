// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the Placement sub-reconciler.
package controller

import (
	"context"
	"testing"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esgenv1alpha1 "github.com/external-secrets/external-secrets/apis/generators/v1alpha1"
	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/c5c3/forge/internal/common/conditions"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
	placementv1alpha1 "github.com/c5c3/forge/operators/placement/api/v1alpha1"
)

// placementTestScheme registers c5c3, client-go, placement, K-ORC, and
// external-secrets types (the projection ensures a DB-credential ExternalSecret,
// the teardown sweeps the catalog CRs).
func placementTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("adding client-go scheme: %v", err)
	}
	if err := c5c3v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding c5c3 scheme: %v", err)
	}
	if err := placementv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding placement scheme: %v", err)
	}
	if err := orcv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding K-ORC scheme: %v", err)
	}
	if err := esov1.AddToScheme(s); err != nil {
		t.Fatalf("adding external-secrets scheme: %v", err)
	}
	if err := esgenv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding external-secrets generators scheme: %v", err)
	}
	return s
}

// placementControlPlane builds a ControlPlane with services.placement set, a
// KeystoneReady=True condition, the auto-injected placement service account, and a
// Ready per-account status for it — i.e. every gate reconcilePlacement checks is
// already passed.
func placementControlPlane() *c5c3v1alpha1.ControlPlane {
	cp := &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "cp",
			Namespace:  "default",
			Generation: 1,
			UID:        types.UID("cp-uid"),
		},
		Spec: c5c3v1alpha1.ControlPlaneSpec{
			OpenStackRelease: "2025.2",
			Region:           "RegionOne",
			Infrastructure: &c5c3v1alpha1.InfrastructureSpec{
				Database: commonv1.DatabaseSpec{
					ClusterRef: &corev1.LocalObjectReference{Name: "openstack-db"},
					Database:   "keystone",
					SecretRef:  commonv1.SecretRefSpec{Name: "keystone-db"},
				},
				Cache: commonv1.CacheSpec{
					ClusterRef: &corev1.LocalObjectReference{Name: "openstack-memcached"},
					Backend:    "dogpile.cache.pymemcache",
					Replicas:   3,
				},
			},
			Services: c5c3v1alpha1.ServicesSpec{
				Keystone:  &c5c3v1alpha1.ServiceKeystoneSpec{},
				Placement: &c5c3v1alpha1.ServicePlacementSpec{},
			},
			KORC: c5c3v1alpha1.KORCSpec{
				AdminCredential: c5c3v1alpha1.AdminCredentialSpec{
					PasswordSecretRef: commonv1.SecretRefSpec{Name: "keystone-admin"},
				},
				ServiceAccounts: []c5c3v1alpha1.ServiceAccountSpec{{
					Name:    "placement",
					Project: c5c3v1alpha1.ServiceAccountProjectSpec{Name: "service-placement", Create: true},
					Roles:   []string{"service"},
				}},
			},
		},
	}
	conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditionTypeKeystoneReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: 1,
		Reason:             "KeystoneReady",
		Message:            "ready",
	})
	cp.Status.ServiceAccounts = []c5c3v1alpha1.ServiceAccountStatus{{Name: "placement", Ready: true}}
	return cp
}

// notReadyPlacementDBCredES builds the Placement DB-credential ExternalSecret with
// NO Ready condition, so WaitForExternalSecret reports not-Ready and the Dynamic
// readiness gate engages. Seeding it explicitly is what keeps
// withReadyPlacementDBCred from substituting a Ready one.
func notReadyPlacementDBCredES(cp *c5c3v1alpha1.ControlPlane) *esov1.ExternalSecret {
	es := dbCredentialGeneratorExternalSecret(placementDBCredentialTarget(cp))
	// Stamped as this ControlPlane's child so the cross-namespace projection path
	// re-applies it instead of refusing to adopt a same-named foreign object.
	stampControlPlaneChildLabels(es, cp)
	return es
}

// readyPlacementDBCredES builds a Ready Placement DB-credential ExternalSecret at
// the derived name/namespace (Dynamic default shape), so WaitForExternalSecret
// reports Ready and the projection clears its dynamic readiness gate.
func readyPlacementDBCredES(cp *c5c3v1alpha1.ControlPlane) *esov1.ExternalSecret {
	es := notReadyPlacementDBCredES(cp)
	es.Status = esov1.ExternalSecretStatus{
		Conditions: []esov1.ExternalSecretStatusCondition{
			{Type: esov1.ExternalSecretReady, Status: corev1.ConditionTrue},
		},
	}
	return es
}

// materialisedPlacementDBCredSecret builds the Secret an ESO sync of the
// generator-backed ExternalSecret would materialise: an ENGINE-ISSUED username
// (the OpenBao mysql-database-plugin prefix) plus its password. The Dynamic gate
// checks the username, not just the ExternalSecret's Ready condition, so a Secret
// carrying a static seed's username reads as "not yet issued".
func materialisedPlacementDBCredSecret(cp *c5c3v1alpha1.ControlPlane) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      placementDBCredentialSecretName(cp),
			Namespace: cp.PlacementNamespace(),
		},
		Data: map[string][]byte{
			"username": []byte(engineIssuedUsernamePrefix + "kubernetes-placement-abc123-1750000000"),
			"password": []byte("engine-issued-password"),
		},
	}
}

// withReadyPlacementDBCred seeds a Ready Placement DB-credential ExternalSecret
// AND the engine-issued Secret an ESO sync of it would materialise, for the
// ControlPlane in objs, unless an ExternalSecret was already seeded explicitly.
//
// The Dynamic-default projection gates the child on both — the ExternalSecret
// having synced and the Secret behind it carrying an engine-issued username — and
// a fake client never runs ESO, so without this every projection test would stall
// on the gate and assert against a child that was deliberately not projected.
func withReadyPlacementDBCred(objs []client.Object) []client.Object {
	var cp *c5c3v1alpha1.ControlPlane
	for _, o := range objs {
		if c, ok := o.(*c5c3v1alpha1.ControlPlane); ok {
			cp = c
			break
		}
	}
	// Only the Dynamic path has a readiness gate; a Static ControlPlane projects a
	// KV-backed ExternalSecret of a different shape and must be left to build it.
	if cp == nil || !placementDBCredentialsDynamicEnabled(cp) {
		return objs
	}
	name, ns := placementDBCredentialSecretName(cp), cp.PlacementNamespace()
	for _, o := range objs {
		if _, ok := o.(*esov1.ExternalSecret); ok && o.GetName() == name && o.GetNamespace() == ns {
			return objs
		}
	}
	return append(objs, readyPlacementDBCredES(cp), materialisedPlacementDBCredSecret(cp))
}

func newPlacementTestReconciler(t *testing.T, objs ...client.Object) *ControlPlaneReconciler {
	t.Helper()
	s := placementTestScheme(t)
	cb := fake.NewClientBuilder().WithScheme(s).WithObjects(withReadyPlacementDBCred(objs)...).
		WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}, &placementv1alpha1.Placement{})
	return &ControlPlaneReconciler{Client: cb.Build(), Scheme: s}
}

func getProjectedPlacement(t *testing.T, c client.Client, cp *c5c3v1alpha1.ControlPlane) *placementv1alpha1.Placement {
	t.Helper()
	pl := &placementv1alpha1.Placement{}
	key := types.NamespacedName{Name: placementName(cp), Namespace: cp.PlacementNamespace()}
	if err := c.Get(context.Background(), key, pl); err != nil {
		t.Fatalf("getting projected Placement %s: %v", key, err)
	}
	return pl
}

// --- gates ---

func TestReconcilePlacement_NotManagedWhenUnset(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := placementControlPlane()
	cp.Spec.Services.Placement = nil
	r := newPlacementTestReconciler(t, cp)

	res, err := r.reconcilePlacement(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypePlacementReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("PlacementNotManaged"))

	var list placementv1alpha1.PlacementList
	g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty())
}

// TestReconcilePlacement_UnsetPreservesChildAndTearsDownDynamicGenerator covers
// the preserve-by-default branch: dropping spec.services.placement keeps the child
// (an accidental block drop must not remove a running service) but must NOT keep
// the credential minter. A retained VaultDynamicSecret mints a fresh MySQL user
// with ALL PRIVILEGES every refresh interval, forever, for a service the operator
// was told it no longer manages — with no consumer, no revocation, and a
// PlacementReady=True condition that surfaces none of it.
func TestReconcilePlacement_UnsetPreservesChildAndTearsDownDynamicGenerator(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := placementControlPlane()
	r := newPlacementTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcilePlacement(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: placementDBCredentialSecretName(cp), Namespace: cp.PlacementNamespace(),
	}, &esgenv1alpha1.VaultDynamicSecret{})).To(Succeed(), "the generator was projected alongside the child")

	// No opt-in annotation: the child is preserved.
	cp.Spec.Services.Placement = nil
	_, err = r.reconcilePlacement(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(getProjectedPlacement(t, r.Client, cp)).NotTo(BeNil(), "the child must still be preserved")
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypePlacementReady)
	g.Expect(cond.Reason).To(Equal("PlacementNotManaged"))
	g.Expect(cond.Message).To(ContainSubstring(placementDeletionAllowedAnnotation))

	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: placementDBCredentialSecretName(cp), Namespace: cp.PlacementNamespace(),
	}, &esgenv1alpha1.VaultDynamicSecret{})).NotTo(Succeed(),
		"the credential minter must be torn down even though the child is preserved")
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: placementDBCredentialServiceAccountName, Namespace: cp.PlacementNamespace(),
	}, &corev1.ServiceAccount{})).NotTo(Succeed(), "the generator's ServiceAccount must be torn down too")
	orphanCert := &unstructured.Unstructured{}
	orphanCert.SetGroupVersionKind(certificateGVK)
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: placementDBCredentialClientCertName(cp), Namespace: cp.PlacementNamespace(),
	}, orphanCert)).NotTo(Succeed(), "the generator's mTLS client Certificate must be torn down too")
}

// TestReconcilePlacement_UnsetDeletesChildWithOptIn verifies the opt-in deletion
// sweep removes the child AND every DB-credential object — the generator-backed
// ExternalSecret plus the Dynamic-mode VaultDynamicSecret, Certificate, and
// ServiceAccount — and the placement catalog CRs that follow it.
func TestReconcilePlacement_UnsetDeletesChildWithOptIn(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := placementControlPlane()
	// A catalog Service CR as the catalog sub-reconciler left it, owned by this
	// ControlPlane so the sweep is allowed to remove it.
	catalogService := &orcv1alpha1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: placementCatalogServiceName(cp), Namespace: childNamespace(cp),
			Labels: controlPlaneChildLabels(cp),
		},
	}
	r := newPlacementTestReconciler(t, cp, catalogService)
	ctx := context.Background()

	_, err := r.reconcilePlacement(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	// The Dynamic-default DB-credential objects were projected alongside the child.
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: placementDBCredentialSecretName(cp), Namespace: cp.PlacementNamespace(),
	}, &esov1.ExternalSecret{})).To(Succeed())
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: placementDBCredentialSecretName(cp), Namespace: cp.PlacementNamespace(),
	}, &esgenv1alpha1.VaultDynamicSecret{})).To(Succeed())

	cp.Spec.Services.Placement = nil
	cp.Annotations = map[string]string{placementDeletionAllowedAnnotation: "true"}

	_, err = r.reconcilePlacement(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	var list placementv1alpha1.PlacementList
	g.Expect(r.Client.List(ctx, &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty(), "opt-in annotation must delete the owned child")

	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: placementDBCredentialSecretName(cp), Namespace: cp.PlacementNamespace(),
	}, &esov1.ExternalSecret{})).NotTo(Succeed(), "the DB-credential ExternalSecret must be swept too")
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: placementDBCredentialSecretName(cp), Namespace: cp.PlacementNamespace(),
	}, &esgenv1alpha1.VaultDynamicSecret{})).NotTo(Succeed(), "the VaultDynamicSecret generator must be swept too")
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: placementDBCredentialServiceAccountName, Namespace: cp.PlacementNamespace(),
	}, &corev1.ServiceAccount{})).NotTo(Succeed(), "the generator's ServiceAccount must be swept too")
	sweptCert := &unstructured.Unstructured{}
	sweptCert.SetGroupVersionKind(certificateGVK)
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: placementDBCredentialClientCertName(cp), Namespace: cp.PlacementNamespace(),
	}, sweptCert)).NotTo(Succeed(), "the mTLS client Certificate must be swept too")
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: placementCatalogServiceName(cp), Namespace: childNamespace(cp),
	}, &orcv1alpha1.Service{})).NotTo(Succeed(), "the placement catalog Service CR must be swept too")
}

// TestReconcilePlacement_UnsetPreservesForeignObjects proves the deletion sweep
// is ownership-checked across every object it names: a Placement child, a
// catalog Service CR and — most importantly — the FIXED-name placement-db-creds
// ServiceAccount that this ControlPlane does NOT own (no owner reference, no
// ownership labels) all survive an opt-in teardown. The ServiceAccount name is
// not CR-derived, so in a shared service namespace it is exactly the object a
// collision would hand to somebody else.
func TestReconcilePlacement_UnsetPreservesForeignObjects(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := placementControlPlane()
	cp.Spec.Services.Placement = nil
	cp.Annotations = map[string]string{placementDeletionAllowedAnnotation: "true"}

	foreignChild := &placementv1alpha1.Placement{
		ObjectMeta: metav1.ObjectMeta{Name: placementName(cp), Namespace: cp.PlacementNamespace()},
	}
	foreignSA := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name: placementDBCredentialServiceAccountName, Namespace: cp.PlacementNamespace(),
		},
	}
	foreignCatalogService := &orcv1alpha1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: placementCatalogServiceName(cp), Namespace: childNamespace(cp)},
	}
	r := newPlacementTestReconciler(t, cp, foreignChild, foreignSA, foreignCatalogService)
	ctx := context.Background()

	_, err := r.reconcilePlacement(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: placementName(cp), Namespace: cp.PlacementNamespace(),
	}, &placementv1alpha1.Placement{})).To(Succeed(), "a Placement child we do not own must never be deleted")
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: placementDBCredentialServiceAccountName, Namespace: cp.PlacementNamespace(),
	}, &corev1.ServiceAccount{})).To(Succeed(), "a foreign placement-db-creds ServiceAccount must never be deleted")
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: placementCatalogServiceName(cp), Namespace: childNamespace(cp),
	}, &orcv1alpha1.Service{})).To(Succeed(), "a catalog Service CR we do not own must never be deleted")
}

// TestReconcilePlacement_UnsetDeletionToleratesAlreadyGoneObjects covers the
// partially-cleaned state a repeated teardown reaches: every object the sweep
// names may already be gone (a previous pass removed it, or it was never
// projected), and each delete tolerates NotFound so the reconcile converges
// instead of failing on the first missing object.
func TestReconcilePlacement_UnsetDeletionToleratesAlreadyGoneObjects(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := placementControlPlane()
	cp.Spec.Services.Placement = nil
	cp.Annotations = map[string]string{placementDeletionAllowedAnnotation: "true"}
	r := newPlacementTestReconciler(t, cp)
	ctx := context.Background()

	// Nothing was ever projected, so every named object is already absent.
	res, err := r.reconcilePlacement(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())

	// And a second pass over the same empty state stays clean.
	_, err = r.reconcilePlacement(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypePlacementReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("PlacementNotManaged"))
}

// TestReconcilePlacement_UnresolvableBackingServicesRequeue is the nil-safety
// fail-safe: a webhook-bypassed CR that dropped spec.infrastructure has no
// database and no cache to project, so the projection requeues instead of
// dereferencing nil.
func TestReconcilePlacement_UnresolvableBackingServicesRequeue(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := placementControlPlane()
	cp.Spec.Infrastructure = nil
	r := newPlacementTestReconciler(t, cp)

	res, err := r.reconcilePlacement(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(infraRequeueAfter))

	var list placementv1alpha1.PlacementList
	g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty(), "nothing may be projected against unresolvable backing services")
}

func TestReconcilePlacement_GatedOnKeystoneReady(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := placementControlPlane()
	conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditionTypeKeystoneReady,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: 1,
		Reason:             "WaitingForKeystone",
		Message:            "not ready",
	})
	r := newPlacementTestReconciler(t, cp)

	res, err := r.reconcilePlacement(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(keystoneInfraGateRequeueAfter))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypePlacementReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForKeystone"))

	var list placementv1alpha1.PlacementList
	g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty())
}

func TestReconcilePlacement_GatedOnServiceAccountNotDeclared(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := placementControlPlane()
	// Drop the auto-injected placement service account (a webhook-bypassed CR).
	cp.Spec.KORC.ServiceAccounts = nil
	r := newPlacementTestReconciler(t, cp)

	res, err := r.reconcilePlacement(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue(), "a missing SA declaration is not a transient wait — no requeue")
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypePlacementReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("ServiceAccountNotDeclared"))
	g.Expect(cond.Message).To(ContainSubstring("spec.korc.serviceAccounts"))

	var list placementv1alpha1.PlacementList
	g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty())
}

func TestReconcilePlacement_GatedOnServiceAccountNotReady(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := placementControlPlane()
	cp.Status.ServiceAccounts = []c5c3v1alpha1.ServiceAccountStatus{{Name: "placement", Ready: false}}
	r := newPlacementTestReconciler(t, cp)

	res, err := r.reconcilePlacement(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypePlacementReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForServiceAccount"))

	var list placementv1alpha1.PlacementList
	g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty(), "the SA gate must block projection")
}

// TestReconcilePlacement_DBCredentialErrorSurfacesAndReturns pins the error leg of
// the credential ensure: in a service namespace the ControlPlane does not own, a
// pre-existing foreign ExternalSecret at the derived name is never adopted, and
// the refusal is both reported as PlacementDBCredentialError and returned to the
// pipeline rather than swallowed into a wait.
func TestReconcilePlacement_DBCredentialErrorSurfacesAndReturns(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := placementControlPlane()
	cp.Spec.Services.Placement.DatabaseCredentialsMode = commonv1.CredentialsModeStatic
	cp.Spec.Services.Placement.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
		Name: "placement", Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleExternal,
	}
	// Somebody else's ExternalSecret under the name our Static branch projects.
	foreign := &esov1.ExternalSecret{ObjectMeta: metav1.ObjectMeta{
		Name: placementDBCredentialSecretName(cp), Namespace: "placement",
	}}
	r := newPlacementTestReconciler(t, cp, foreign)

	res, err := r.reconcilePlacement(context.Background(), cp)

	g.Expect(err).To(HaveOccurred(), "adopting a foreign ExternalSecret must be refused")
	g.Expect(res.IsZero()).To(BeTrue())
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypePlacementReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("PlacementDBCredentialError"))

	var list placementv1alpha1.PlacementList
	g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty(), "no child may be projected against a credential that was never ensured")
}

// TestReconcilePlacement_DynamicCredentialNotReady_DefersProjection is the gate
// that keeps the Dynamic default from failing OPEN. The engine role behind the
// generator is provisioned by a MANUAL onboarding step (setup-database-tenant.sh),
// while the operator rolls out on its own, so a ControlPlane can reach here with
// no role to mint against. Until the credential materialises no Placement child
// may be projected at all.
func TestReconcilePlacement_DynamicCredentialNotReady_DefersProjection(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := placementControlPlane()
	r := newPlacementTestReconciler(t, cp, notReadyPlacementDBCredES(cp))

	res, err := r.reconcilePlacement(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(dbCredentialsRequeueAfter))

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypePlacementReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForPlacementDBCredential"))
	g.Expect(cond.Message).To(ContainSubstring(placementDBDynamicCredsPathFor(cp)),
		"the condition must name the engine path an operator has to onboard")

	var list placementv1alpha1.PlacementList
	g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty(), "no Placement child may be projected before the credential lands")
}

// staleStaticPlacementDBCredSecret builds the Secret a MIGRATED cluster is left
// with right after the Static->Dynamic flip: materialised by the last STATIC sync,
// so it still carries the retired bootstrap's username=placement seed. That name
// is a syntactically valid username, so a gate that only checks for a non-empty
// username would wave it through — but no MySQL user was ever created under it
// (the static login is the Placement CR name).
func staleStaticPlacementDBCredSecret(cp *c5c3v1alpha1.ControlPlane) *corev1.Secret {
	secret := materialisedPlacementDBCredSecret(cp)
	secret.Data["username"] = []byte("placement")
	return secret
}

// TestReconcilePlacement_DynamicCredentialStaleStaticUsername_LeavesExistingChildStatic
// is the regression guard for the failure an ExternalSecret-only gate lets
// through. A Static->Dynamic flip create-or-updates the ExternalSecret IN PLACE,
// so on a migrated cluster it keeps reporting Ready from its last Static sync
// while the Secret behind it still holds the retired static seed. Flipping the
// child on that Ready alone stops the placement-operator asserting the static
// User/Grant Placement was serving on and points it at a login that never existed
// — an outage behind PlacementReady=True.
func TestReconcilePlacement_DynamicCredentialStaleStaticUsername_LeavesExistingChildStatic(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := placementControlPlane()
	cp.Spec.Services.Placement.DatabaseCredentialsMode = commonv1.CredentialsModeStatic
	// A Ready ExternalSecret over a Secret still holding the static seed: exactly
	// what a cluster upgraded from the Static path presents to the reconciler.
	r := newPlacementTestReconciler(t, cp, readyPlacementDBCredES(cp), staleStaticPlacementDBCredSecret(cp))

	// Static deployment: the child is projected and runs on the static credential.
	_, err := r.reconcilePlacement(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedPlacement(t, r.Client, cp).Spec.Database.CredentialsMode).
		To(Equal(commonv1.CredentialsModeStatic))

	// Flip to Dynamic. The ExternalSecret is Ready, but only from the Static sync.
	cp.Spec.Services.Placement.DatabaseCredentialsMode = commonv1.CredentialsModeDynamic
	res, err := r.reconcilePlacement(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(dbCredentialsRequeueAfter))

	g.Expect(getProjectedPlacement(t, r.Client, cp).Spec.Database.CredentialsMode).
		To(Equal(commonv1.CredentialsModeStatic),
			"a Ready ExternalSecret over a stale static username must not flip the running child to Dynamic")

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypePlacementReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForPlacementDBCredential"))
	g.Expect(cond.Message).To(ContainSubstring(`"placement"`),
		"the condition must name the non-engine-issued username it found")
	g.Expect(cond.Message).To(ContainSubstring(placementDBCredentialSecretName(cp)),
		"the condition must name the Secret an operator has to delete")
}

// --- projected child fields ---

// TestReconcilePlacement_ProjectedChildFields is the field-mapping lock for the
// projection: the release-derived image, the backing services (with the fixed
// placement schema, the operator-owned secretRef, and the Dynamic mode of the
// managed shared database), the top-down Keystone endpoint, the region, the
// service user derived from the injected account, the resolved store ref, the
// default replicas, and the deliberately-unset apiServer.
func TestReconcilePlacement_ProjectedChildFields(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := placementControlPlane()
	// An exposed Keystone: its external URL must reach the child as the public
	// endpoint only, never as the token-validation endpoint.
	cp.Spec.Services.Keystone.Gateway = &commonv1.GatewaySpec{
		ParentRef: commonv1.GatewayParentRefSpec{Name: "openstack-gw"},
		Hostname:  "keystone.example.com",
	}
	cp.Spec.Services.Keystone.PublicEndpoint = "https://keystone.example.com:8443/v3"
	sa := cp.Spec.KORC.ServiceAccounts[0]
	r := newPlacementTestReconciler(t, cp)

	_, err := r.reconcilePlacement(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	pl := getProjectedPlacement(t, r.Client, cp)
	g.Expect(pl.Name).To(Equal("cp-placement"))
	g.Expect(pl.Spec.OpenStackRelease).To(Equal("2025.2"))
	g.Expect(pl.Spec.Image.Repository).To(Equal("ghcr.io/c5c3/placement"))
	g.Expect(pl.Spec.Image.Tag).To(Equal("2025.2"), "the tag defaults to spec.openStackRelease")

	// Database: the shared cluster, the fixed placement schema, the operator-owned
	// credential Secret, and the managed-shared Dynamic default.
	g.Expect(pl.Spec.Database.ClusterRef).NotTo(BeNil())
	g.Expect(pl.Spec.Database.ClusterRef.Name).To(Equal("openstack-db"))
	g.Expect(pl.Spec.Database.Database).To(Equal("placement"),
		"the logical schema must be placement, not the shared block's keystone")
	g.Expect(pl.Spec.Database.SecretRef.Name).To(Equal(placementDBCredentialSecretName(cp)))
	g.Expect(pl.Spec.Database.SecretRef.Key).To(Equal("password"))
	g.Expect(pl.Spec.Database.CredentialsMode).To(Equal(commonv1.CredentialsModeDynamic),
		"a managed shared placement database defaults to Dynamic (engine-issued) credentials")
	// DeepCopy: the projected pointers must not alias the ControlPlane spec.
	g.Expect(pl.Spec.Database.ClusterRef).NotTo(BeIdenticalTo(cp.Spec.Infrastructure.Database.ClusterRef))
	g.Expect(pl.Spec.Cache.ClusterRef).NotTo(BeNil())
	g.Expect(pl.Spec.Cache.ClusterRef.Name).To(Equal("openstack-memcached"))
	g.Expect(pl.Spec.Cache.ClusterRef).NotTo(BeIdenticalTo(cp.Spec.Infrastructure.Cache.ClusterRef))

	// The Keystone endpoint is derived top-down, never from the external exposure.
	g.Expect(pl.Spec.KeystoneEndpoint).To(Equal("http://cp-keystone.default.svc:5000/v3"),
		"the token-validation endpoint must be the cluster-local Service URL")
	g.Expect(pl.Spec.KeystonePublicEndpoint).To(Equal("https://keystone.example.com:8443/v3"),
		"the public endpoint carries the browser/client-facing URL")

	g.Expect(pl.Spec.Region).To(Equal("RegionOne"))

	// The service user comes from the injected placement account.
	g.Expect(pl.Spec.ServiceUser.Username).To(Equal("placement"))
	g.Expect(pl.Spec.ServiceUser.ProjectName).To(Equal("service-placement"))
	g.Expect(pl.Spec.ServiceUser.UserDomainName).To(Equal(serviceAccountDomainName(cp, sa)))
	g.Expect(pl.Spec.ServiceUser.ProjectDomainName).To(Equal(pl.Spec.ServiceUser.UserDomainName))
	g.Expect(pl.Spec.ServiceUser.SecretRef.Name).To(Equal("cp-service-account-placement-credentials"))
	g.Expect(pl.Spec.ServiceUser.SecretRef.Key).To(Equal("password"))

	// The resolved store selection, so the child never falls back to its own default.
	g.Expect(pl.Spec.SecretStoreRef).NotTo(BeNil())
	g.Expect(pl.Spec.SecretStoreRef.Kind).To(Equal(commonv1.SecretStoreKindNamespaced))
	g.Expect(pl.Spec.SecretStoreRef.Name).To(Equal("openbao-tenant-store"))

	g.Expect(pl.Spec.Deployment.Replicas).To(Equal(commonv1.DefaultReplicas))
	g.Expect(pl.Spec.APIServer).To(BeNil(), "the child-side uWSGI defaults stay authoritative")
	g.Expect(metav1.IsControlledBy(pl, cp)).To(BeTrue(),
		"the projected Placement must carry the ControlPlane controller owner reference")
}

func TestReconcilePlacement_ImageOverrideWins(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := placementControlPlane()
	cp.Spec.Services.Placement.Image = &commonv1.ImageSpec{
		Repository: "registry.example.com/mirror/placement",
		Tag:        "custom",
	}
	r := newPlacementTestReconciler(t, cp)

	_, err := r.reconcilePlacement(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	pl := getProjectedPlacement(t, r.Client, cp)
	g.Expect(pl.Spec.Image.Repository).To(Equal("registry.example.com/mirror/placement"))
	g.Expect(pl.Spec.Image.Tag).To(Equal("custom"))
}

// TestReconcilePlacement_DatabaseBrownfieldLeavesCredentialsModeUntouched is the
// other half of the credentials-mode contract: a database with no ClusterRef
// carries a user-supplied credential, so the mode and the secretRef are left as
// declared and no DB-credential ExternalSecret is projected.
func TestReconcilePlacement_DatabaseBrownfieldLeavesCredentialsModeUntouched(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := placementControlPlane()
	cp.Spec.Infrastructure.Database = commonv1.DatabaseSpec{
		Host:      "db.example.com",
		Database:  "keystone",
		SecretRef: commonv1.SecretRefSpec{Name: "brownfield-db"},
	}
	r := newPlacementTestReconciler(t, cp)

	_, err := r.reconcilePlacement(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	pl := getProjectedPlacement(t, r.Client, cp)
	g.Expect(pl.Spec.Database.ClusterRef).To(BeNil())
	g.Expect(pl.Spec.Database.Database).To(Equal("placement"),
		"the logical schema is always overridden to placement, even for a brownfield database")
	g.Expect(pl.Spec.Database.CredentialsMode).To(BeEmpty(),
		"a brownfield database must keep its credentialsMode untouched")
	g.Expect(pl.Spec.Database.SecretRef.Name).To(Equal("brownfield-db"),
		"a brownfield database keeps its user-supplied secretRef")

	g.Expect(r.Get(context.Background(), types.NamespacedName{
		Name: placementDBCredentialSecretName(cp), Namespace: cp.PlacementNamespace(),
	}, &esov1.ExternalSecret{})).NotTo(Succeed(),
		"no DB-credential ExternalSecret is projected in brownfield mode")
}

// TestReconcilePlacement_ExtraConfigMerge proves the projected child's
// spec.extraConfig is the key-by-key merge of globalExtraConfig and the
// per-service block: the per-service value wins on an overlapping key, a
// global-only key in the same section survives, and a global-only section is
// carried over.
func TestReconcilePlacement_ExtraConfigMerge(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := placementControlPlane()
	cp.Spec.GlobalExtraConfig = map[string]map[string]string{
		"placement_database": {
			"connection_recycle_time": "280",
			"max_pool_size":           "5",
		},
		"DEFAULT": {"debug": "true"},
	}
	cp.Spec.Services.Placement.ExtraConfig = map[string]map[string]string{
		"placement_database": {"connection_recycle_time": "600"}, // overrides global
	}
	r := newPlacementTestReconciler(t, cp)

	_, err := r.reconcilePlacement(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	pl := getProjectedPlacement(t, r.Client, cp)
	g.Expect(pl.Spec.ExtraConfig).To(Equal(map[string]map[string]string{
		"placement_database": {
			"connection_recycle_time": "600", // per-service wins
			"max_pool_size":           "5",   // global-only key in the same section
		},
		"DEFAULT": {"debug": "true"}, // global-only section
	}), "per-service extraConfig must win, global keys/sections merged in")
}

// TestReconcilePlacement_ExtraConfigClearedProjectsNil proves the field is
// assigned unconditionally: clearing both extraConfig blocks reverts the child to
// an absent spec.extraConfig rather than leaving the previously-projected value
// pinned.
func TestReconcilePlacement_ExtraConfigClearedProjectsNil(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := placementControlPlane()
	cp.Spec.GlobalExtraConfig = map[string]map[string]string{"DEFAULT": {"debug": "true"}}
	cp.Spec.Services.Placement.ExtraConfig = map[string]map[string]string{
		"placement_database": {"connection_recycle_time": "600"},
	}
	r := newPlacementTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcilePlacement(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedPlacement(t, r.Client, cp).Spec.ExtraConfig).NotTo(BeEmpty())

	cp.Spec.GlobalExtraConfig = nil
	cp.Spec.Services.Placement.ExtraConfig = nil
	_, err = r.reconcilePlacement(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedPlacement(t, r.Client, cp).Spec.ExtraConfig).To(BeNil(),
		"clearing both extraConfig blocks must revert the child")
}

func TestReconcilePlacement_GatewayNilClears(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := placementControlPlane()
	cp.Spec.Services.Placement.Gateway = &commonv1.GatewaySpec{
		ParentRef: commonv1.GatewayParentRefSpec{Name: "openstack-gw"},
		Hostname:  "placement.example.com",
	}
	r := newPlacementTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcilePlacement(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	pl := getProjectedPlacement(t, r.Client, cp)
	g.Expect(pl.Spec.Gateway).NotTo(BeNil())
	g.Expect(pl.Spec.Gateway.Hostname).To(Equal("placement.example.com"))

	// Clearing the gateway reverts the child rather than pinning the old value.
	cp.Spec.Services.Placement.Gateway = nil
	_, err = r.reconcilePlacement(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	pl = getProjectedPlacement(t, r.Client, cp)
	g.Expect(pl.Spec.Gateway).To(BeNil(), "clearing the gateway must tear the HTTPRoute down")
}

func TestReconcilePlacement_ReplicasOverrideAndRevert(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := placementControlPlane()
	cp.Spec.Services.Placement.Replicas = ptr.To(int32(5))
	r := newPlacementTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcilePlacement(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedPlacement(t, r.Client, cp).Spec.Deployment.Replicas).To(Equal(int32(5)))

	cp.Spec.Services.Placement.Replicas = nil
	_, err = r.reconcilePlacement(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedPlacement(t, r.Client, cp).Spec.Deployment.Replicas).To(Equal(commonv1.DefaultReplicas),
		"clearing the override must revert the child to the operator default")
}

// TestReconcilePlacement_MirrorsChildReady exercises the readiness mirror: a fresh
// child is not ready (WaitingForPlacement + requeue), a Ready child flips
// PlacementReady True.
func TestReconcilePlacement_MirrorsChildReady(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := placementControlPlane()
	r := newPlacementTestReconciler(t, cp)
	ctx := context.Background()

	res, err := r.reconcilePlacement(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(infraRequeueAfter))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypePlacementReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForPlacement"))

	pl := getProjectedPlacement(t, r.Client, cp)
	conditions.SetCondition(&pl.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: pl.Generation,
		Reason:             "AllReady",
		Message:            "ready",
	})
	g.Expect(r.Client.Status().Update(ctx, pl)).To(Succeed())

	res, err = r.reconcilePlacement(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond = conditions.GetCondition(cp.Status.Conditions, conditionTypePlacementReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("PlacementReady"))
}

// TestReconcilePlacement_CrossNamespaceChildIsLabelledNotOwned verifies the
// ownership substitute for a Placement placed in a namespace of its own: the child
// carries the ControlPlane's ownership labels and NO owner reference (Kubernetes
// forbids a cross-namespace one).
func TestReconcilePlacement_CrossNamespaceChildIsLabelledNotOwned(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := placementControlPlane()
	cp.Spec.Services.Placement.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
		Name: "placement", Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
	}
	r := newPlacementTestReconciler(t, cp)

	_, err := r.reconcilePlacement(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	pl := getProjectedPlacement(t, r.Client, cp)
	g.Expect(pl.Namespace).To(Equal("placement"))
	g.Expect(pl.OwnerReferences).To(BeEmpty(), "a cross-namespace child cannot carry an owner reference")
	g.Expect(pl.Labels).To(HaveKeyWithValue(controlPlaneNameLabel, "cp"))
	g.Expect(pl.Labels).To(HaveKeyWithValue(controlPlaneNamespaceLabel, "default"))
}

// TestPlacementEndpointURL pins the in-cluster Placement API endpoint convention
// the catalog registers against: http://{name}.{ns}.svc:8778.
func TestPlacementEndpointURL(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := placementControlPlane()
	g.Expect(placementEndpointURL(cp)).To(Equal("http://cp-placement.default.svc:8778"))
}

// --- per-service target clusters ---

// placedPlacementControlPlane places the placement service in a namespace of its
// own on a target cluster. Its database is brownfield, so the DB-credential leg —
// whose own placement is covered in reconcile_dbcredentials_test.go — projects
// nothing and the pass reaches the child projection over the local client alone.
func placedPlacementControlPlane(targetCluster string) *c5c3v1alpha1.ControlPlane {
	cp := placementControlPlane()
	cp.Spec.Infrastructure.Database = commonv1.DatabaseSpec{
		Host:      "db.example.com",
		Database:  "keystone",
		SecretRef: commonv1.SecretRefSpec{Name: "keystone-db"},
	}
	cp.Spec.Services.Placement.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
		Name:      "placement",
		Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
	}
	cp.Spec.Services.Placement.PublicEndpoint = "https://placement.example.com"
	cp.Spec.Services.Placement.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: targetCluster}
	return cp
}

// TestReconcilePlacement_ProjectsTheTargetClusterRef verifies the placement
// reaches the child verbatim — the placement-operator owns everything on the
// target, so the ref is the whole hand-over — and that an unplaced service
// projects no ref.
func TestReconcilePlacement_ProjectsTheTargetClusterRef(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := placedPlacementControlPlane("remote-a")
	r := newPlacementTestReconciler(t, cp)

	_, err := r.reconcilePlacement(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(getProjectedPlacement(t, r.Client, cp).Spec.TargetClusterRef).
		To(Equal(&commonv1.TargetClusterRefSpec{Name: "remote-a"}))

	unplaced := placementControlPlane()
	r2 := newPlacementTestReconciler(t, unplaced)
	_, err = r2.reconcilePlacement(context.Background(), unplaced)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedPlacement(t, r2.Client, unplaced).Spec.TargetClusterRef).To(BeNil(),
		"a service that names no cluster must project no ref at all")
}

// TestPlacementKeystoneEndpoint_FollowsThePlacement pins the endpoint policy:
// Placement validates tokens against Keystone itself, so it gets the in-cluster
// Service DNS name exactly while the two services share a cluster, and the public
// URL as soon as they do not — that name resolves nowhere else.
func TestPlacementKeystoneEndpoint_FollowsThePlacement(t *testing.T) {
	const (
		inCluster = "http://cp-keystone.identity.svc:5000/v3"
		public    = "https://keystone.example.com/v3"
	)
	for _, tc := range []struct {
		name                string
		placement, keystone *commonv1.TargetClusterRefSpec
		want                string
	}{
		{name: "both co-located", want: inCluster},
		{
			name:      "both on the same cluster",
			placement: &commonv1.TargetClusterRefSpec{Name: "remote-a"},
			keystone:  &commonv1.TargetClusterRefSpec{Name: "remote-a"},
			want:      inCluster,
		},
		{
			name:      "Placement placed, Keystone at home",
			placement: &commonv1.TargetClusterRefSpec{Name: "remote-a"},
			want:      public,
		},
		{
			name:     "Keystone placed, Placement at home",
			keystone: &commonv1.TargetClusterRefSpec{Name: "remote-a"},
			want:     public,
		},
		{
			name:      "different clusters",
			placement: &commonv1.TargetClusterRefSpec{Name: "remote-a"},
			keystone:  &commonv1.TargetClusterRefSpec{Name: "remote-b"},
			want:      public,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := placementControlPlane()
			cp.Spec.Services.Keystone.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{Name: "identity"}
			cp.Spec.Services.Keystone.PublicEndpoint = public
			cp.Spec.Services.Keystone.TargetClusterRef = tc.keystone
			cp.Spec.Services.Placement.TargetClusterRef = tc.placement

			g.Expect(placementKeystoneEndpoint(cp)).To(Equal(tc.want))
		})
	}
}
