// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the Cinder sub-reconciler: the naming helpers in reconcile_cinder.go,
// the catalog URL they feed (cinderCatalogURL), the projected Cinder child with
// its CinderBackend and CinderBackupBackend satellites, and the CinderReady
// condition the projection drives.
package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esgenv1alpha1 "github.com/external-secrets/external-secrets/apis/generators/v1alpha1"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	"github.com/c5c3/cobaltcore/internal/common/messaging"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
	cinderv1alpha1 "github.com/c5c3/cobaltcore/operators/cinder/api/v1alpha1"
)

// cinderBusSecretName is the brownfield Secret the fixtures declare the shared bus
// through, and cinderBusURL the transport URL it carries.
const (
	cinderBusSecretName = "bus-url"
	cinderBusURL        = "rabbit://u:p@bus:5672/"
)

// cinderTestScheme registers c5c3, client-go, cinder, and external-secrets types
// (the projection ensures a DB-credential ExternalSecret).
func cinderTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("adding client-go scheme: %v", err)
	}
	if err := c5c3v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding c5c3 scheme: %v", err)
	}
	if err := cinderv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding cinder scheme: %v", err)
	}
	if err := esov1.AddToScheme(s); err != nil {
		t.Fatalf("adding external-secrets scheme: %v", err)
	}
	if err := esgenv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding external-secrets generators scheme: %v", err)
	}
	return s
}

// cinderControlPlane builds a ControlPlane running the block-storage service on
// one NFS volume backend and one NFS backup backend, co-located in the
// ControlPlane's own namespace, with the gate reconcileCinder reads off the
// ControlPlane itself True: KeystoneReady. The second gate is the projected
// KeystoneService child, which newCinderTestReconciler seeds Ready (see
// withReadyCinderRegistration), and the third is the shared bus, declared
// brownfield here and seeded by withCinderBusSecret.
func cinderControlPlane() *c5c3v1alpha1.ControlPlane {
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
				Messaging: &commonv1.MessagingSpec{
					SecretRef: &commonv1.SecretRefSpec{Name: cinderBusSecretName},
				},
			},
			Services: c5c3v1alpha1.ServicesSpec{
				Keystone: &c5c3v1alpha1.ServiceKeystoneSpec{},
				Cinder: &c5c3v1alpha1.ServiceCinderSpec{
					Backends: []c5c3v1alpha1.CinderBackendEntry{{
						Name: "nfs1",
						Type: "NFS",
						NFS: &c5c3v1alpha1.NFSShareSpec{
							Server: "nfs-server.openstack.svc.cluster.local",
							Path:   "/volumes",
						},
						ImageVolumeCache: &c5c3v1alpha1.CinderImageVolumeCacheSpec{Enabled: true},
					}},
					BackupBackend: &c5c3v1alpha1.CinderBackupBackendEntry{
						Name: "nfsbk",
						Type: "NFS",
						NFS: &c5c3v1alpha1.NFSShareSpec{
							Server: "nfs-server.openstack.svc.cluster.local",
							Path:   "/backups",
						},
					},
				},
			},
			KORC: c5c3v1alpha1.KORCSpec{
				AdminCredential: c5c3v1alpha1.AdminCredentialSpec{
					PasswordSecretRef: commonv1.SecretRefSpec{Name: "keystone-admin"},
				},
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
	return cp
}

// cinderRegistration builds the KeystoneService child at the projected
// name/namespace carrying the given conditions, for the tests that drive the gate
// and the readiness fold from a child that has not converged.
func cinderRegistration(cp *c5c3v1alpha1.ControlPlane, conds ...metav1.Condition) *c5c3v1alpha1.KeystoneService {
	ks := &c5c3v1alpha1.KeystoneService{
		ObjectMeta: metav1.ObjectMeta{Name: cinderName(cp), Namespace: cp.CinderNamespace()},
	}
	if ks.Namespace != cp.Namespace {
		stampControlPlaneChildLabels(ks, cp)
	}
	for _, cond := range conds {
		conditions.SetCondition(&ks.Status.Conditions, cond)
	}
	return ks
}

// readyCinderRegistration builds the KeystoneService child the Cinder projection
// gates on, converged: account provisioned (with the ids K-ORC resolved), catalog
// registered, aggregate Ready. A child in a dedicated namespace carries the
// ownership labels, so the projection re-applies it instead of refusing to adopt a
// same-named foreign CR.
func readyCinderRegistration(cp *c5c3v1alpha1.ControlPlane) *c5c3v1alpha1.KeystoneService {
	ks := desiredCinderRegistration(cp)
	if ks.Namespace != cp.Namespace {
		stampControlPlaneChildLabels(ks, cp)
	}
	ks.Status.Account = &c5c3v1alpha1.KeystoneServiceAccountStatus{
		ProjectID: "project-" + ks.Name,
		UserID:    "user-" + ks.Name,
	}
	for _, cond := range []metav1.Condition{{
		Type:    conditionTypeKeystoneServiceAccountReady,
		Status:  metav1.ConditionTrue,
		Reason:  reasonKeystoneServiceAccountProvisioned,
		Message: "account provisioned",
	}, {
		Type:    conditionTypeKeystoneServiceCatalogReady,
		Status:  metav1.ConditionTrue,
		Reason:  reasonKeystoneServiceCatalogRegistered,
		Message: "catalog registered",
	}, {
		Type:    conditionTypeReady,
		Status:  metav1.ConditionTrue,
		Reason:  "AllReady",
		Message: "All sub-conditions are ready",
	}} {
		conditions.SetCondition(&ks.Status.Conditions, cond)
	}
	return ks
}

// notReadyCinderDBCredES builds the Cinder DB-credential ExternalSecret with NO
// Ready condition, so WaitForExternalSecret reports not-Ready and the Dynamic
// readiness gate engages. Seeding it explicitly is what keeps withReadyCinderDBCred
// from substituting a Ready one.
func notReadyCinderDBCredES(cp *c5c3v1alpha1.ControlPlane) *esov1.ExternalSecret {
	es := dbCredentialGeneratorExternalSecret(cinderDBCredentialTarget(cp))
	// Stamped as this ControlPlane's child so the cross-namespace projection path
	// re-applies it instead of refusing to adopt a same-named foreign object.
	stampControlPlaneChildLabels(es, cp)
	return es
}

// readyCinderDBCredES builds a Ready Cinder DB-credential ExternalSecret at the
// derived name/namespace (Dynamic default shape), so WaitForExternalSecret reports
// Ready and the projection clears its dynamic readiness gate.
func readyCinderDBCredES(cp *c5c3v1alpha1.ControlPlane) *esov1.ExternalSecret {
	es := notReadyCinderDBCredES(cp)
	es.Status = esov1.ExternalSecretStatus{
		Conditions: []esov1.ExternalSecretStatusCondition{
			{Type: esov1.ExternalSecretReady, Status: corev1.ConditionTrue},
		},
	}
	return es
}

// materialisedCinderDBCredSecret builds the Secret an ESO sync of the
// generator-backed ExternalSecret would materialise: an ENGINE-ISSUED username
// (the OpenBao mysql-database-plugin prefix) plus its password. The Dynamic gate
// checks the username, not just the ExternalSecret's Ready condition, so a Secret
// carrying a static seed's username reads as "not yet issued".
func materialisedCinderDBCredSecret(cp *c5c3v1alpha1.ControlPlane) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cinderDBCredentialSecretName(cp),
			Namespace: cp.CinderNamespace(),
		},
		Data: map[string][]byte{
			"username": []byte(engineIssuedUsernamePrefix + "kubernetes-cinder-abc123-1750000000"),
			"password": []byte("engine-issued-password"),
		},
	}
}

// withReadyCinderDBCred seeds a Ready Cinder DB-credential ExternalSecret AND the
// engine-issued Secret an ESO sync of it would materialise, for the ControlPlane in
// objs, unless an ExternalSecret was already seeded explicitly.
//
// The Dynamic-default projection gates the child on both, the ExternalSecret having
// synced and the Secret behind it carrying an engine-issued username, and a fake
// client never runs ESO, so without this every projection test would stall on the
// gate and assert against a child that was deliberately not projected.
func withReadyCinderDBCred(objs []client.Object) []client.Object {
	cp := controlPlaneIn(objs)
	// Only the Dynamic path has a readiness gate; a Static ControlPlane projects a
	// KV-backed ExternalSecret of a different shape and must be left to build it.
	if cp == nil || !cinderDBCredentialsDynamicEnabled(cp) {
		return objs
	}
	name, ns := cinderDBCredentialSecretName(cp), cp.CinderNamespace()
	for _, o := range objs {
		if _, ok := o.(*esov1.ExternalSecret); ok && o.GetName() == name && o.GetNamespace() == ns {
			return objs
		}
	}
	return append(objs, readyCinderDBCredES(cp), materialisedCinderDBCredSecret(cp))
}

// withReadyCinderRegistration seeds the converged KeystoneService child the
// projection gates on, unless the test seeded one of its own, which is what the
// gate and readiness-fold tests do.
//
// A fake client runs no KeystoneService controller, so without this every
// projection test would hold at the registration gate and assert against a Cinder
// that was deliberately not projected.
func withReadyCinderRegistration(objs []client.Object) []client.Object {
	cp := controlPlaneIn(objs)
	if cp == nil || cp.Spec.Services.Cinder == nil {
		return objs
	}
	for _, o := range objs {
		if _, ok := o.(*c5c3v1alpha1.KeystoneService); ok {
			return objs
		}
	}
	return append(objs, readyCinderRegistration(cp))
}

// withCinderBusSecret seeds the brownfield transport-URL Secret the fixture's
// spec.infrastructure.messaging names, in the ControlPlane's OWN namespace where
// the bus is declared and read. Without it every projection test would halt on the
// messaging leg before reaching the child.
func withCinderBusSecret(objs []client.Object) []client.Object {
	cp := controlPlaneIn(objs)
	if cp == nil || cp.Spec.Infrastructure == nil || cp.Spec.Infrastructure.Messaging == nil {
		return objs
	}
	ref := cp.Spec.Infrastructure.Messaging.SecretRef
	if ref == nil {
		return objs
	}
	for _, o := range objs {
		if _, ok := o.(*corev1.Secret); ok && o.GetName() == ref.Name && o.GetNamespace() == cp.Namespace {
			return objs
		}
	}
	return append(objs, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: ref.Name, Namespace: cp.Namespace},
		Data:       map[string][]byte{commonv1.DefaultTransportURLSecretKey: []byte(cinderBusURL)},
	})
}

// withCinderTenantStore seeds the per-tenant SecretStore in the block-storage
// service's namespace when that service is PLACED, unless the test seeded one of
// its own. The credential mirror a placed service gets is gated on that store, so
// without it every placed test would hold at SecretStoreNotReady.
//
// The store lands on the local client because newCinderTestReconciler wires no
// resolver, which resolves every namespace to the management cluster. The tests
// that exercise the two-cluster legs build their reconciler themselves.
func withCinderTenantStore(objs []client.Object) []client.Object {
	cp := controlPlaneIn(objs)
	if cp == nil || targetClusterRefForNamespace(cp, cp.CinderNamespace()) == nil {
		return objs
	}
	for _, o := range objs {
		if _, ok := o.(*esov1.SecretStore); ok {
			return objs
		}
	}
	return append(objs, readyTenantSecretStore(esoTenantStoreName, cp.CinderNamespace(), "", ""))
}

// controlPlaneIn returns the ControlPlane among the seeded objects, or nil.
func controlPlaneIn(objs []client.Object) *c5c3v1alpha1.ControlPlane {
	for _, o := range objs {
		if cp, ok := o.(*c5c3v1alpha1.ControlPlane); ok {
			return cp
		}
	}
	return nil
}

func newCinderTestReconciler(t *testing.T, objs ...client.Object) *ControlPlaneReconciler {
	t.Helper()
	s := cinderTestScheme(t)
	seeded := withCinderTenantStore(withReadyCinderRegistration(withCinderBusSecret(withReadyCinderDBCred(objs))))
	cb := fake.NewClientBuilder().WithScheme(s).WithObjects(seeded...).
		WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}, &cinderv1alpha1.Cinder{},
			&c5c3v1alpha1.KeystoneService{})
	return &ControlPlaneReconciler{Client: cb.Build(), Scheme: s}
}

func getProjectedCinder(t *testing.T, c client.Client, cp *c5c3v1alpha1.ControlPlane) *cinderv1alpha1.Cinder {
	t.Helper()
	cn := &cinderv1alpha1.Cinder{}
	key := types.NamespacedName{Name: cinderName(cp), Namespace: cp.CinderNamespace()}
	if err := c.Get(context.Background(), key, cn); err != nil {
		t.Fatalf("getting projected Cinder %s: %v", key, err)
	}
	return cn
}

// getProjectedCinderBackend fetches the CinderBackend satellite projected for the
// entry named name, which is the object's own name.
func getProjectedCinderBackend(
	t *testing.T, c client.Client, cp *c5c3v1alpha1.ControlPlane, name string,
) *cinderv1alpha1.CinderBackend {
	t.Helper()
	backend := &cinderv1alpha1.CinderBackend{}
	key := types.NamespacedName{Name: name, Namespace: cp.CinderNamespace()}
	if err := c.Get(context.Background(), key, backend); err != nil {
		t.Fatalf("getting projected CinderBackend %s: %v", key, err)
	}
	return backend
}

// --- gates ---

func TestReconcileCinder_NotManagedWhenUnset(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := cinderControlPlane()
	cp.Spec.Services.Cinder = nil
	r := newCinderTestReconciler(t, cp)

	res, err := r.reconcileCinder(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeCinderReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("CinderNotManaged"))

	var list cinderv1alpha1.CinderList
	g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty())
}

// TestReconcileCinder_UnsetPreservesChildAndTearsDownDynamicGenerator covers the
// preserve-by-default branch: dropping spec.services.cinder keeps the child (an
// accidental block drop must not remove a running service) but must NOT keep the
// credential minter. A retained VaultDynamicSecret mints a fresh MySQL user with
// ALL PRIVILEGES every refresh interval, forever, for a service the operator was
// told it no longer manages, with no consumer, no revocation, and a
// CinderReady=True condition that surfaces none of it.
func TestReconcileCinder_UnsetPreservesChildAndTearsDownDynamicGenerator(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := cinderControlPlane()
	r := newCinderTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcileCinder(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: cinderDBCredentialSecretName(cp), Namespace: cp.CinderNamespace(),
	}, &esgenv1alpha1.VaultDynamicSecret{})).To(Succeed(), "the generator was projected alongside the child")

	// No opt-in annotation: the child is preserved.
	cp.Spec.Services.Cinder = nil
	_, err = r.reconcileCinder(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(getProjectedCinder(t, r.Client, cp)).NotTo(BeNil(), "the child must still be preserved")
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeCinderReady)
	g.Expect(cond.Reason).To(Equal("CinderNotManaged"))
	g.Expect(cond.Message).To(ContainSubstring(cinderDeletionAllowedAnnotation))

	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: cinderDBCredentialSecretName(cp), Namespace: cp.CinderNamespace(),
	}, &esgenv1alpha1.VaultDynamicSecret{})).NotTo(Succeed(),
		"the credential minter must be torn down even though the child is preserved")
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: cinderDBCredentialServiceAccountName, Namespace: cp.CinderNamespace(),
	}, &corev1.ServiceAccount{})).NotTo(Succeed(), "the generator's ServiceAccount must be torn down too")
	orphanCert := &unstructured.Unstructured{}
	orphanCert.SetGroupVersionKind(certificateGVK)
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: cinderDBCredentialClientCertName(cp), Namespace: cp.CinderNamespace(),
	}, orphanCert)).NotTo(Succeed(), "the generator's mTLS client Certificate must be torn down too")
}

// TestReconcileCinder_UnsetDeletesChildWithOptIn verifies the opt-in deletion sweep
// removes the child AND every DB-credential object: the generator-backed
// ExternalSecret plus the Dynamic-mode VaultDynamicSecret, Certificate, and
// ServiceAccount.
func TestReconcileCinder_UnsetDeletesChildWithOptIn(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := cinderControlPlane()
	r := newCinderTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcileCinder(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	// The Dynamic-default DB-credential objects were projected alongside the child.
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: cinderDBCredentialSecretName(cp), Namespace: cp.CinderNamespace(),
	}, &esov1.ExternalSecret{})).To(Succeed())
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: cinderDBCredentialSecretName(cp), Namespace: cp.CinderNamespace(),
	}, &esgenv1alpha1.VaultDynamicSecret{})).To(Succeed())

	cp.Spec.Services.Cinder = nil
	cp.Annotations = map[string]string{cinderDeletionAllowedAnnotation: "true"}

	_, err = r.reconcileCinder(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	var list cinderv1alpha1.CinderList
	g.Expect(r.Client.List(ctx, &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty(), "opt-in annotation must delete the owned child")

	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: cinderDBCredentialSecretName(cp), Namespace: cp.CinderNamespace(),
	}, &esov1.ExternalSecret{})).NotTo(Succeed(), "the DB-credential ExternalSecret must be swept too")
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: cinderDBCredentialSecretName(cp), Namespace: cp.CinderNamespace(),
	}, &esgenv1alpha1.VaultDynamicSecret{})).NotTo(Succeed(), "the VaultDynamicSecret generator must be swept too")
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: cinderDBCredentialServiceAccountName, Namespace: cp.CinderNamespace(),
	}, &corev1.ServiceAccount{})).NotTo(Succeed(), "the generator's ServiceAccount must be swept too")
	sweptCert := &unstructured.Unstructured{}
	sweptCert.SetGroupVersionKind(certificateGVK)
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: cinderDBCredentialClientCertName(cp), Namespace: cp.CinderNamespace(),
	}, sweptCert)).NotTo(Succeed(), "the mTLS client Certificate must be swept too")
}

// TestReconcileCinder_UnsetDeletesSatellitesWithOptIn covers the two satellite
// kinds on that same sweep: the volume backends and the backup backend come down
// with the Cinder child, so an unmanaged service leaves no cinder-volume or
// cinder-backup Deployment running behind it.
func TestReconcileCinder_UnsetDeletesSatellitesWithOptIn(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := cinderControlPlane()
	r := newCinderTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcileCinder(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedCinderBackend(t, r.Client, cp, "nfs1")).NotTo(BeNil())

	cp.Spec.Services.Cinder = nil
	cp.Annotations = map[string]string{cinderDeletionAllowedAnnotation: "true"}
	_, err = r.reconcileCinder(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	var backends cinderv1alpha1.CinderBackendList
	g.Expect(r.Client.List(ctx, &backends)).To(Succeed())
	g.Expect(backends.Items).To(BeEmpty(), "the opt-in annotation must delete the owned volume backends")
	var backupBackends cinderv1alpha1.CinderBackupBackendList
	g.Expect(r.Client.List(ctx, &backupBackends)).To(Succeed())
	g.Expect(backupBackends.Items).To(BeEmpty(), "the opt-in annotation must delete the owned backup backend")
}

// TestReconcileCinder_UnsetDeletesMessagingSecretsWithOptIn covers the bus delivery
// on that same sweep: the transport-URL Secret and the CA mirror are the only
// broker material in the namespace, and nothing else reaps them. A same-named
// Secret this ControlPlane never wrote survives, because the name is derived and a
// shared namespace may already carry one.
func TestReconcileCinder_UnsetDeletesMessagingSecretsWithOptIn(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	cp := cinderControlPlane()
	cp.Spec.Infrastructure.Messaging.TLS = &commonv1.MessagingTLSSpec{
		CABundleSecretRef: commonv1.SecretRefSpec{Name: "bus-ca", Key: "ca.crt"},
	}
	busCA := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "bus-ca", Namespace: cp.Namespace},
		Data:       map[string][]byte{"ca.crt": []byte("ca-bundle")},
	}
	r := newCinderTestReconciler(t, cp, busCA)

	_, err := r.reconcileCinder(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	for _, name := range []string{cinderMessagingSecretName(cp), cinderMessagingCASecretName(cp)} {
		g.Expect(r.Get(ctx, types.NamespacedName{Name: name, Namespace: cp.Namespace},
			&corev1.Secret{})).To(Succeed(), "the delivery was written alongside the child")
	}

	cp.Spec.Services.Cinder = nil
	cp.Annotations = map[string]string{cinderDeletionAllowedAnnotation: "true"}
	_, err = r.reconcileCinder(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	for _, name := range []string{cinderMessagingSecretName(cp), cinderMessagingCASecretName(cp)} {
		g.Expect(r.Get(ctx, types.NamespacedName{Name: name, Namespace: cp.Namespace},
			&corev1.Secret{})).NotTo(Succeed(), "the bus delivery must be swept with the child")
	}

	// A foreign Secret at the derived transport-URL name is never touched.
	other := cinderControlPlane()
	other.Spec.Services.Cinder = nil
	other.Annotations = map[string]string{cinderDeletionAllowedAnnotation: "true"}
	foreign := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: cinderMessagingSecretName(other), Namespace: other.Namespace,
	}}
	r2 := newCinderTestReconciler(t, other, foreign)

	_, err = r2.reconcileCinder(ctx, other)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(r2.Get(ctx, client.ObjectKeyFromObject(foreign), &corev1.Secret{})).To(Succeed(),
		"a messaging Secret we do not own must never be deleted")
}

// TestReconcileCinder_UnsetPreservesForeignObjects proves the deletion sweep is
// ownership-checked across every object it names: a Cinder child and, most
// importantly, the FIXED-name cinder-db-creds ServiceAccount that this ControlPlane
// does NOT own (no owner reference, no ownership labels) both survive an opt-in
// teardown. The ServiceAccount name is not CR-derived, so in a shared service
// namespace it is exactly the object a collision would hand to somebody else.
func TestReconcileCinder_UnsetPreservesForeignObjects(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := cinderControlPlane()
	cp.Spec.Services.Cinder = nil
	cp.Annotations = map[string]string{cinderDeletionAllowedAnnotation: "true"}

	foreignChild := &cinderv1alpha1.Cinder{
		ObjectMeta: metav1.ObjectMeta{Name: cinderName(cp), Namespace: cp.CinderNamespace()},
	}
	foreignSA := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name: cinderDBCredentialServiceAccountName, Namespace: cp.CinderNamespace(),
		},
	}
	r := newCinderTestReconciler(t, cp, foreignChild, foreignSA)
	ctx := context.Background()

	_, err := r.reconcileCinder(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: cinderName(cp), Namespace: cp.CinderNamespace(),
	}, &cinderv1alpha1.Cinder{})).To(Succeed(), "a Cinder child we do not own must never be deleted")
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: cinderDBCredentialServiceAccountName, Namespace: cp.CinderNamespace(),
	}, &corev1.ServiceAccount{})).To(Succeed(), "a foreign cinder-db-creds ServiceAccount must never be deleted")
}

// TestReconcileCinder_UnsetDeletionToleratesAlreadyGoneObjects covers the
// partially-cleaned state a repeated teardown reaches: every object the sweep names
// may already be gone (a previous pass removed it, or it was never projected), and
// each delete tolerates NotFound so the reconcile converges instead of failing on
// the first missing object.
func TestReconcileCinder_UnsetDeletionToleratesAlreadyGoneObjects(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := cinderControlPlane()
	cp.Spec.Services.Cinder = nil
	cp.Annotations = map[string]string{cinderDeletionAllowedAnnotation: "true"}
	r := newCinderTestReconciler(t, cp)
	ctx := context.Background()

	// Nothing was ever projected, so every named object is already absent.
	res, err := r.reconcileCinder(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())

	// And a second pass over the same empty state stays clean.
	_, err = r.reconcileCinder(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeCinderReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("CinderNotManaged"))
}

// TestReconcileCinder_UnresolvableBackingServicesRequeue is the nil-safety
// fail-safe: a webhook-bypassed CR that dropped spec.infrastructure, or only the
// bus block inside it, has nothing to project and no bus to deliver, so the
// projection requeues instead of dereferencing nil, and writes no condition it
// would then have to retract.
func TestReconcileCinder_UnresolvableBackingServicesRequeue(t *testing.T) {
	for _, tt := range []struct {
		name  string
		apply func(cp *c5c3v1alpha1.ControlPlane)
	}{
		{
			name:  "no infrastructure at all",
			apply: func(cp *c5c3v1alpha1.ControlPlane) { cp.Spec.Infrastructure = nil },
		},
		{
			name:  "infrastructure without a messaging block",
			apply: func(cp *c5c3v1alpha1.ControlPlane) { cp.Spec.Infrastructure.Messaging = nil },
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := cinderControlPlane()
			tt.apply(cp)
			r := newCinderTestReconciler(t, cp)

			res, err := r.reconcileCinder(context.Background(), cp)

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(res.RequeueAfter).To(Equal(infraRequeueAfter))
			g.Expect(conditions.GetCondition(cp.Status.Conditions, conditionTypeCinderReady)).To(BeNil(),
				"the fail-safe must not write a condition it cannot substantiate")

			var list cinderv1alpha1.CinderList
			g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
			g.Expect(list.Items).To(BeEmpty(), "nothing may be projected against unresolvable backing services")
		})
	}
}

func TestReconcileCinder_GatedOnKeystoneReady(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := cinderControlPlane()
	conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditionTypeKeystoneReady,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: 1,
		Reason:             "WaitingForKeystone",
		Message:            "not ready",
	})
	r := newCinderTestReconciler(t, cp)

	res, err := r.reconcileCinder(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(keystoneInfraGateRequeueAfter))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeCinderReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForKeystone"))

	var list cinderv1alpha1.CinderList
	g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty())
}

// TestReconcileCinder_MessagingWaitHalts covers the managed bus whose
// RabbitmqCluster is not there yet: the delivery is a wait, and until it lands
// neither the registration nor the child may be written, because a Cinder with no
// transport URL reaches neither its scheduler nor its volume services.
func TestReconcileCinder_MessagingWaitHalts(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := cinderControlPlane()
	cp.Spec.Infrastructure.Messaging = &commonv1.MessagingSpec{
		ClusterRef: &corev1.LocalObjectReference{Name: "openstack-rabbitmq"},
	}
	// Built without the seeded registration the other tests get, so an empty
	// KeystoneService list is evidence the leg never ran rather than a fixture.
	s := cinderTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(withReadyCinderDBCred([]client.Object{cp})...).
		WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}, &cinderv1alpha1.Cinder{},
			&c5c3v1alpha1.KeystoneService{}).
		Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	res, err := r.reconcileCinder(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred(), "a bus that has not been created yet is a wait, not a failure")
	g.Expect(res.RequeueAfter).To(Equal(infraRequeueAfter))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeCinderReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(messaging.ReasonWaitingForMessagingCredentials))

	var list cinderv1alpha1.CinderList
	g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty(), "no child may be projected without a transport URL")
	var registrations c5c3v1alpha1.KeystoneServiceList
	g.Expect(r.Client.List(context.Background(), &registrations)).To(Succeed())
	g.Expect(registrations.Items).To(BeEmpty(), "the bus gate runs before the registration is projected")
}

// TestReconcileCinder_MessagingErrorSurfaces covers the bus block that named
// neither a cluster nor a Secret, which only a bypassed admission produces: it does
// not converge on its own, so it is returned as an error rather than waited out.
func TestReconcileCinder_MessagingErrorSurfaces(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := cinderControlPlane()
	cp.Spec.Infrastructure.Messaging = &commonv1.MessagingSpec{}
	r := newCinderTestReconciler(t, cp)

	_, err := r.reconcileCinder(context.Background(), cp)

	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("resolving the shared bus transport URL"))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeCinderReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("CinderMessagingError"))
}

// TestReconcileCinder_GatedOnRegistrationAccountNotReady pins the registration
// gate: while the child's AccountReady is False no Cinder is projected, the child's
// own reason and message are relayed, and a Cinder projected by an earlier pass is
// left running on the credentials it already has.
func TestReconcileCinder_GatedOnRegistrationAccountNotReady(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := cinderControlPlane()
	ks := cinderRegistration(cp, metav1.Condition{
		Type:    conditionTypeKeystoneServiceAccountReady,
		Status:  metav1.ConditionFalse,
		Reason:  reasonServiceAccountCollision,
		Message: `user "cinder" already exists in Keystone`,
	})
	existing := &cinderv1alpha1.Cinder{
		ObjectMeta: metav1.ObjectMeta{Name: cinderName(cp), Namespace: cp.CinderNamespace()},
		Spec:       cinderv1alpha1.CinderSpec{Region: "RegionPrevious"},
	}
	r := newCinderTestReconciler(t, cp, ks, existing)

	res, err := r.reconcileCinder(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeCinderReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonWaitingForServiceRegistration))
	g.Expect(cond.Message).To(ContainSubstring(reasonServiceAccountCollision))
	g.Expect(cond.Message).To(ContainSubstring(`user "cinder" already exists in Keystone`))

	g.Expect(getProjectedCinder(t, r.Client, cp).Spec.Region).To(Equal("RegionPrevious"),
		"the gate must write no Cinder at all, leaving a previously projected one untouched")
}

// TestReconcileCinder_GatedOnRegistrationWithoutConditions covers the child that
// exists but has not been reconciled yet: the gate holds on a waiting message
// rather than reading a missing condition as ready.
func TestReconcileCinder_GatedOnRegistrationWithoutConditions(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := cinderControlPlane()
	r := newCinderTestReconciler(t, cp, cinderRegistration(cp))

	res, err := r.reconcileCinder(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeCinderReady)
	g.Expect(cond.Reason).To(Equal(reasonWaitingForServiceRegistration))
	g.Expect(cond.Message).To(ContainSubstring(conditionTypeKeystoneServiceAccountReady))

	var list cinderv1alpha1.CinderList
	g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty(), "the registration gate must block projection")
}

// TestReconcileCinder_RegistrationNotFoundAfterEnsureHolds covers the read-back
// that misses: a child the API server has not made readable yet is a wait, not an
// error.
func TestReconcileCinder_RegistrationNotFoundAfterEnsureHolds(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := cinderControlPlane()
	s := cinderTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(withCinderBusSecret(withReadyCinderDBCred([]client.Object{cp}))...).
		WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}, &cinderv1alpha1.Cinder{},
			&c5c3v1alpha1.KeystoneService{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*c5c3v1alpha1.KeystoneService); ok {
					return apierrors.NewNotFound(
						schema.GroupResource{Group: "c5c3.io", Resource: "keystoneservices"}, key.Name)
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	res, err := r.reconcileCinder(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred(), "a child that is not readable yet is not a failure")
	g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeCinderReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonWaitingForServiceRegistration))

	var list cinderv1alpha1.CinderList
	g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty())
}

// TestReconcileCinder_RegistrationReadFailureSurfaces covers the other half: a read
// that fails for any reason OTHER than absence is an error, wrapped with what it
// was reading.
func TestReconcileCinder_RegistrationReadFailureSurfaces(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := cinderControlPlane()
	s := cinderTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(withCinderBusSecret(withReadyCinderDBCred([]client.Object{cp}))...).
		WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}, &cinderv1alpha1.Cinder{},
			&c5c3v1alpha1.KeystoneService{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*c5c3v1alpha1.KeystoneService); ok {
					return apierrors.NewInternalError(errors.New("etcd is unavailable"))
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileCinder(context.Background(), cp)

	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("reading the cinder KeystoneService child:"))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeCinderReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonServiceRegistrationError))
}

// TestReconcileCinder_NeverAdoptsForeignRegistration proves the registration write
// is refused rather than allowed to overwrite a same-named KeystoneService in a
// namespace the ControlPlane does not own: the refusal surfaces on CinderReady and
// the foreign CR keeps its spec.
func TestReconcileCinder_NeverAdoptsForeignRegistration(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := cinderControlPlane()
	cp.Spec.Services.Cinder.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
		Name: "block", Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
	}
	foreign := &c5c3v1alpha1.KeystoneService{
		ObjectMeta: metav1.ObjectMeta{Name: cinderName(cp), Namespace: "block"},
		Spec: c5c3v1alpha1.KeystoneServiceSpec{
			ControlPlaneRef: c5c3v1alpha1.ControlPlaneRefSpec{Name: "someone-else"},
		},
	}
	r := newCinderTestReconciler(t, cp, foreign)

	_, err := r.reconcileCinder(context.Background(), cp)

	g.Expect(err).To(HaveOccurred(), "adopting a foreign registration must be refused")
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeCinderReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonServiceRegistrationError))
	g.Expect(cond.Message).To(ContainSubstring("refusing to adopt pre-existing"))

	var live c5c3v1alpha1.KeystoneService
	g.Expect(r.Get(context.Background(), types.NamespacedName{
		Name: cinderName(cp), Namespace: "block",
	}, &live)).To(Succeed())
	g.Expect(live.Spec.ControlPlaneRef.Name).To(Equal("someone-else"),
		"a foreign registration must never be overwritten")
	g.Expect(live.Labels).NotTo(HaveKey(controlPlaneNameLabel))
}

// TestReconcileCinder_DBCredentialErrorSurfacesAndReturns pins the error leg of the
// credential ensure: in a service namespace the ControlPlane does not own, a
// pre-existing foreign ExternalSecret at the derived name is never adopted, and the
// refusal is both reported as CinderDBCredentialError and returned to the pipeline
// rather than swallowed into a wait.
func TestReconcileCinder_DBCredentialErrorSurfacesAndReturns(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := cinderControlPlane()
	cp.Spec.Services.Cinder.DatabaseCredentialsMode = commonv1.CredentialsModeStatic
	cp.Spec.Services.Cinder.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
		Name: "block", Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleExternal,
	}
	// Somebody else's ExternalSecret under the name our Static branch projects.
	foreign := &esov1.ExternalSecret{ObjectMeta: metav1.ObjectMeta{
		Name: cinderDBCredentialSecretName(cp), Namespace: "block",
	}}
	r := newCinderTestReconciler(t, cp, foreign)

	res, err := r.reconcileCinder(context.Background(), cp)

	g.Expect(err).To(HaveOccurred(), "adopting a foreign ExternalSecret must be refused")
	g.Expect(res.IsZero()).To(BeTrue())
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeCinderReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("CinderDBCredentialError"))

	var list cinderv1alpha1.CinderList
	g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty(), "no child may be projected against a credential that was never ensured")
}

// TestReconcileCinder_DynamicCredentialNotReady_DefersProjection is the gate that
// keeps the Dynamic default from failing OPEN. The engine role behind the generator
// is provisioned by a MANUAL onboarding step (setup-database-tenant.sh), while the
// operator rolls out on its own, so a ControlPlane can reach here with no role to
// mint against. Until the credential materialises no Cinder child may be projected
// at all.
func TestReconcileCinder_DynamicCredentialNotReady_DefersProjection(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := cinderControlPlane()
	r := newCinderTestReconciler(t, cp, notReadyCinderDBCredES(cp))

	res, err := r.reconcileCinder(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(dbCredentialsRequeueAfter))

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeCinderReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForCinderDBCredential"))
	g.Expect(cond.Message).To(ContainSubstring(cinderDBDynamicCredsPathFor(cp)),
		"the condition must name the engine path an operator has to onboard")

	var list cinderv1alpha1.CinderList
	g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty(), "no Cinder child may be projected before the credential lands")
}

// staleStaticCinderDBCredSecret builds the Secret a MIGRATED cluster is left with
// right after the Static->Dynamic flip: materialised by the last STATIC sync, so it
// still carries the retired bootstrap's username=cinder seed. That name is a
// syntactically valid username, so a gate that only checks for a non-empty username
// would wave it through, but no MySQL user was ever created under it (the static
// login is the Cinder CR name).
func staleStaticCinderDBCredSecret(cp *c5c3v1alpha1.ControlPlane) *corev1.Secret {
	secret := materialisedCinderDBCredSecret(cp)
	secret.Data["username"] = []byte("cinder")
	return secret
}

// TestReconcileCinder_DynamicCredentialStaleStaticUsername_LeavesExistingChildStatic
// is the regression guard for the failure an ExternalSecret-only gate lets through.
// A Static->Dynamic flip create-or-updates the ExternalSecret IN PLACE, so on a
// migrated cluster it keeps reporting Ready from its last Static sync while the
// Secret behind it still holds the retired static seed. Flipping the child on that
// Ready alone stops the cinder-operator asserting the static User/Grant Cinder was
// serving on and points it at a login that never existed, an outage behind
// CinderReady=True.
func TestReconcileCinder_DynamicCredentialStaleStaticUsername_LeavesExistingChildStatic(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := cinderControlPlane()
	cp.Spec.Services.Cinder.DatabaseCredentialsMode = commonv1.CredentialsModeStatic
	// A Ready ExternalSecret over a Secret still holding the static seed: exactly
	// what a cluster upgraded from the Static path presents to the reconciler.
	r := newCinderTestReconciler(t, cp, readyCinderDBCredES(cp), staleStaticCinderDBCredSecret(cp))

	// Static deployment: the child is projected and runs on the static credential.
	_, err := r.reconcileCinder(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedCinder(t, r.Client, cp).Spec.Database.CredentialsMode).
		To(Equal(commonv1.CredentialsModeStatic))

	// Flip to Dynamic. The ExternalSecret is Ready, but only from the Static sync.
	cp.Spec.Services.Cinder.DatabaseCredentialsMode = commonv1.CredentialsModeDynamic
	res, err := r.reconcileCinder(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(dbCredentialsRequeueAfter))

	g.Expect(getProjectedCinder(t, r.Client, cp).Spec.Database.CredentialsMode).
		To(Equal(commonv1.CredentialsModeStatic),
			"a Ready ExternalSecret over a stale static username must not flip the running child to Dynamic")

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeCinderReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForCinderDBCredential"))
	g.Expect(cond.Message).To(ContainSubstring(`"cinder"`),
		"the condition must name the non-engine-issued username it found")
	g.Expect(cond.Message).To(ContainSubstring(cinderDBCredentialSecretName(cp)),
		"the condition must name the Secret an operator has to delete")
}

// --- projected child fields ---

// TestReconcileCinder_ProjectedChildFields is the field-mapping lock for the
// projection: the release-derived image, the backing services (with the fixed
// cinder schema, the operator-owned secretRef, and the Dynamic mode of the managed
// shared database), the top-down Keystone endpoint, the region, the service user
// the registration child declares, the resolved store ref, the API replicas, the
// bus delivery, and the internal tenant the registration's account IDs supply. The
// fixture declares neither Glance nor Barbican, so the two sibling-derived fields
// stay empty.
func TestReconcileCinder_ProjectedChildFields(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := cinderControlPlane()
	// An exposed Keystone: its external URL must reach the child as the public
	// endpoint only, never as the token-validation endpoint.
	cp.Spec.Services.Keystone.Gateway = &commonv1.GatewaySpec{
		ParentRef: commonv1.GatewayParentRefSpec{Name: "openstack-gw"},
		Hostname:  "keystone.example.com",
	}
	cp.Spec.Services.Keystone.PublicEndpoint = "https://keystone.example.com:8443/v3"
	r := newCinderTestReconciler(t, cp)

	_, err := r.reconcileCinder(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	cn := getProjectedCinder(t, r.Client, cp)
	g.Expect(cn.Name).To(Equal("cp-cinder"))
	g.Expect(cn.Spec.OpenStackRelease).To(Equal("2025.2"))
	g.Expect(cn.Spec.Image.Repository).To(Equal("ghcr.io/c5c3/cinder"))
	g.Expect(cn.Spec.Image.Tag).To(Equal("2025.2"), "the tag defaults to spec.openStackRelease")

	// Database: the shared cluster, the fixed cinder schema, the operator-owned
	// credential Secret, and the managed-shared Dynamic default.
	g.Expect(cn.Spec.Database.ClusterRef).NotTo(BeNil())
	g.Expect(cn.Spec.Database.ClusterRef.Name).To(Equal("openstack-db"))
	g.Expect(cn.Spec.Database.Database).To(Equal("cinder"),
		"the logical schema must be cinder, not the shared block's keystone")
	g.Expect(cn.Spec.Database.SecretRef.Name).To(Equal(cinderDBCredentialSecretName(cp)))
	g.Expect(cn.Spec.Database.SecretRef.Key).To(Equal("password"))
	g.Expect(cn.Spec.Database.CredentialsMode).To(Equal(commonv1.CredentialsModeDynamic),
		"a managed shared cinder database defaults to Dynamic (engine-issued) credentials")
	// DeepCopy: the projected pointers must not alias the ControlPlane spec.
	g.Expect(cn.Spec.Database.ClusterRef).NotTo(BeIdenticalTo(cp.Spec.Infrastructure.Database.ClusterRef))
	g.Expect(cn.Spec.Cache.ClusterRef).NotTo(BeNil())
	g.Expect(cn.Spec.Cache.ClusterRef.Name).To(Equal("openstack-memcached"))
	g.Expect(cn.Spec.Cache.ClusterRef).NotTo(BeIdenticalTo(cp.Spec.Infrastructure.Cache.ClusterRef))

	// The Keystone endpoint is derived top-down, never from the external exposure.
	g.Expect(cn.Spec.KeystoneEndpoint).To(Equal("http://cp-keystone.default.svc:5000/v3"),
		"the token-validation endpoint must be the cluster-local Service URL")
	g.Expect(cn.Spec.KeystonePublicEndpoint).To(Equal("https://keystone.example.com:8443/v3"),
		"the public endpoint carries the browser/client-facing URL")

	g.Expect(cn.Spec.Region).To(Equal("RegionOne"))

	// The service user names the account the registration child declares, and reads
	// its password from the consumer Secret the registration delivers.
	g.Expect(cn.Spec.ServiceUser).NotTo(BeNil())
	g.Expect(cn.Spec.ServiceUser.Username).To(Equal("cinder"))
	g.Expect(cn.Spec.ServiceUser.ProjectName).To(Equal("service-cinder"))
	// Both domains resolve to the ControlPlane's effective admin domain, which is
	// what the registration resolves its own unset domainName to.
	g.Expect(cn.Spec.ServiceUser.UserDomainName).To(Equal(adminDomainName(cp)))
	g.Expect(cn.Spec.ServiceUser.ProjectDomainName).To(Equal(adminDomainName(cp)))
	g.Expect(cn.Spec.ServiceUser.SecretRef.Name).To(Equal("cp-cinder-credentials"))
	g.Expect(cn.Spec.ServiceUser.SecretRef.Key).To(Equal("password"))

	// The resolved store selection, so the child never falls back to its own default.
	g.Expect(cn.Spec.SecretStoreRef).NotTo(BeNil())
	g.Expect(cn.Spec.SecretStoreRef.Kind).To(Equal(commonv1.SecretStoreKindNamespaced))
	g.Expect(cn.Spec.SecretStoreRef.Name).To(Equal("openbao-tenant-store"))

	g.Expect(cn.Spec.API.Deployment.Replicas).To(Equal(commonv1.DefaultReplicas))
	g.Expect(cn.Spec.Gateway).To(BeNil(), "an unexposed Cinder projects no gateway")

	// The bus reaches the child as a brownfield secretRef naming the delivery written
	// beside it, never as the ControlPlane's own clusterRef.
	g.Expect(cn.Spec.Messaging.ClusterRef).To(BeNil())
	g.Expect(cn.Spec.Messaging.SecretRef).NotTo(BeNil())
	g.Expect(cn.Spec.Messaging.SecretRef.Name).To(Equal("cp-cinder-messaging"))
	g.Expect(cn.Spec.Messaging.SecretRef.Key).To(Equal(commonv1.DefaultTransportURLSecretKey))
	g.Expect(cn.Spec.Messaging.TLS).To(BeNil(), "a plaintext bus projects no trust anchor")

	// The internal tenant the image-volume cache owns its cached volumes as, read off
	// the registration child's account status.
	g.Expect(cn.Spec.InternalTenant).To(Equal(&cinderv1alpha1.InternalTenantSpec{
		ProjectID: "project-cp-cinder", UserID: "user-cp-cinder",
	}))

	// Neither sibling is declared, so neither endpoint is projected.
	g.Expect(cn.Spec.GlanceEndpoint).To(BeEmpty(), "no glance block projects no glance endpoint")
	g.Expect(cn.Spec.KeyManager).To(BeNil(), "no barbican block projects no key manager")

	g.Expect(metav1.IsControlledBy(cn, cp)).To(BeTrue(),
		"the projected Cinder must carry the ControlPlane controller owner reference")
}

// TestReconcileCinder_LeavesTuningBlocksUnset pins the Placement posture on the
// blocks the ControlPlane deliberately does not drive, and the one replica it does
// write on the scheduler, volume and backup Deployments. Those three blocks are
// struct values, so the apply carries them either way and the API server defaults
// their replicas to three. Volume and backup take the one replica the Cinder CRD's
// CEL rules admit, because a second cinder-volume process under the same host
// identity is what the NFS drivers refuse; the scheduler takes the one the cinder
// operator's defaulting webhook gives a standalone CR.
func TestReconcileCinder_LeavesTuningBlocksUnset(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := cinderControlPlane()
	r := newCinderTestReconciler(t, cp)

	_, err := r.reconcileCinder(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	cn := getProjectedCinder(t, r.Client, cp)
	g.Expect(cn.Spec.Scheduler).To(Equal(cinderv1alpha1.CinderSchedulerSpec{
		Deployment: commonv1.DeploymentSpec{Replicas: 1},
	}), "the scheduler block carries the pinned replica count and nothing else")
	g.Expect(cn.Spec.Volume).To(Equal(cinderv1alpha1.CinderVolumeSpec{
		Deployment: commonv1.DeploymentSpec{Replicas: 1},
	}), "the volume block carries the pinned replica count and nothing else")
	g.Expect(cn.Spec.Backup).To(Equal(cinderv1alpha1.CinderBackupSpec{
		Deployment: commonv1.DeploymentSpec{Replicas: 1},
	}), "the backup block carries the pinned replica count and nothing else")
	g.Expect(cn.Spec.API.UWSGI).To(BeNil(), "the child-side uWSGI defaults stay authoritative")
	g.Expect(cn.Spec.DBPurge).To(BeNil(), "scheduling the database purge stays a standalone-CR decision")
	g.Expect(cn.Spec.NetworkPolicy).To(BeNil())
	g.Expect(cn.Spec.Autoscaling).To(BeNil())
	g.Expect(cn.Spec.Logging).To(BeNil())
	g.Expect(cn.Spec.PolicyOverrides).To(BeNil())
}

func TestReconcileCinder_ImageOverrideWins(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := cinderControlPlane()
	cp.Spec.Services.Cinder.Image = &commonv1.ImageSpec{
		Repository: "registry.example.com/mirror/cinder",
		Tag:        "custom",
	}
	r := newCinderTestReconciler(t, cp)

	_, err := r.reconcileCinder(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	cn := getProjectedCinder(t, r.Client, cp)
	g.Expect(cn.Spec.Image.Repository).To(Equal("registry.example.com/mirror/cinder"))
	g.Expect(cn.Spec.Image.Tag).To(Equal("custom"))
}

// TestReconcileCinder_DatabaseBrownfieldLeavesCredentialsModeUntouched is the other
// half of the credentials-mode contract: a database with no ClusterRef carries a
// user-supplied credential, so the mode and the secretRef are left as declared and
// no DB-credential ExternalSecret is projected.
func TestReconcileCinder_DatabaseBrownfieldLeavesCredentialsModeUntouched(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := cinderControlPlane()
	cp.Spec.Infrastructure.Database = commonv1.DatabaseSpec{
		Host:      "db.example.com",
		Database:  "keystone",
		SecretRef: commonv1.SecretRefSpec{Name: "brownfield-db"},
	}
	r := newCinderTestReconciler(t, cp)

	_, err := r.reconcileCinder(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	cn := getProjectedCinder(t, r.Client, cp)
	g.Expect(cn.Spec.Database.ClusterRef).To(BeNil())
	g.Expect(cn.Spec.Database.Database).To(Equal("cinder"),
		"the logical schema is always overridden to cinder, even for a brownfield database")
	g.Expect(cn.Spec.Database.CredentialsMode).To(BeEmpty(),
		"a brownfield database must keep its credentialsMode untouched")
	g.Expect(cn.Spec.Database.SecretRef.Name).To(Equal("brownfield-db"),
		"a brownfield database keeps its user-supplied secretRef")

	g.Expect(r.Get(context.Background(), types.NamespacedName{
		Name: cinderDBCredentialSecretName(cp), Namespace: cp.CinderNamespace(),
	}, &esov1.ExternalSecret{})).NotTo(Succeed(),
		"no DB-credential ExternalSecret is projected in brownfield mode")
}

// TestReconcileCinder_ProjectsBrownfieldMessagingSecretRef pins the delivery
// contract: the cinder operator resolves spec.messaging in the Cinder's own
// namespace on the Cinder's own cluster, so a managed bus declared on the
// ControlPlane reaches the child as a brownfield secretRef naming the Secret this
// pass wrote there.
func TestReconcileCinder_ProjectsBrownfieldMessagingSecretRef(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := cinderControlPlane()
	r := newCinderTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcileCinder(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	cn := getProjectedCinder(t, r.Client, cp)
	g.Expect(cn.Spec.Messaging.ClusterRef).To(BeNil(),
		"the child never resolves the ControlPlane's own bus reference")
	g.Expect(cn.Spec.Messaging.SecretRef).To(Equal(&commonv1.SecretRefSpec{
		Name: "cp-cinder-messaging", Key: commonv1.DefaultTransportURLSecretKey,
	}))
	g.Expect(cn.Spec.Messaging.TLS).To(BeNil())

	delivered := &corev1.Secret{}
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: cinderMessagingSecretName(cp), Namespace: cp.CinderNamespace(),
	}, delivered)).To(Succeed())
	g.Expect(delivered.Data).To(HaveKeyWithValue(commonv1.DefaultTransportURLSecretKey, []byte(cinderBusURL)))
}

// convergeCinderChild stands in for the cinder-operator: it stamps
// status.observedGeneration on the projected Cinder child and marks it Ready, the
// two halves of the verdict reconcileCinder reads before it reaps the CA mirror.
func convergeCinderChild(
	t *testing.T, r *ControlPlaneReconciler, cp *c5c3v1alpha1.ControlPlane, observedGeneration int64,
) {
	t.Helper()
	cn := getProjectedCinder(t, r.Client, cp)
	cn.Status.ObservedGeneration = observedGeneration
	conditions.SetCondition(&cn.Status.Conditions, metav1.Condition{
		Type:               conditionTypeReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: observedGeneration,
		Reason:             "AllReady",
		Message:            "ready",
	})
	if err := r.Client.Status().Update(context.Background(), cn); err != nil {
		t.Fatalf("converging the projected Cinder: %v", err)
	}
}

// TestReconcileCinder_ProjectsTLSMirrorWhenSharedBusHasTLS covers the trust anchor:
// a bus that declares TLS gets its CA bundle mirrored beside the child, and the
// child's messaging block names the mirror rather than the bundle Secret in the
// ControlPlane's namespace, which its own cluster may not carry. Dropping the tls
// block again has to revert BOTH halves on the same pass: the mirror is deleted, so
// a child left pointing at it would wedge its pods on a volume source that no
// longer exists.
func TestReconcileCinder_ProjectsTLSMirrorWhenSharedBusHasTLS(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := cinderControlPlane()
	cp.Spec.Infrastructure.Messaging.TLS = &commonv1.MessagingTLSSpec{
		CABundleSecretRef: commonv1.SecretRefSpec{Name: "bus-ca", Key: "ca.crt"},
	}
	bundle := []byte("-----BEGIN CERTIFICATE-----\nbus\n-----END CERTIFICATE-----\n")
	busCA := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "bus-ca", Namespace: cp.Namespace},
		Data:       map[string][]byte{"ca.crt": bundle},
	}
	r := newCinderTestReconciler(t, cp, busCA)
	ctx := context.Background()

	_, err := r.reconcileCinder(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	cn := getProjectedCinder(t, r.Client, cp)
	g.Expect(cn.Spec.Messaging.TLS).NotTo(BeNil())
	g.Expect(cn.Spec.Messaging.TLS.CABundleSecretRef).To(Equal(commonv1.SecretRefSpec{
		Name: "cp-cinder-messaging-ca", Key: serviceMessagingCAKey,
	}))

	mirror := &corev1.Secret{}
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: cinderMessagingCASecretName(cp), Namespace: cp.CinderNamespace(),
	}, mirror)).To(Succeed())
	g.Expect(mirror.Data).To(HaveKeyWithValue(serviceMessagingCAKey, bundle))

	// Drop the tls block: the child must revert instead of pinning the last value,
	// and the mirror comes down behind it. The child is converged first because the
	// reap waits for that verdict (see
	// TestReconcileCinder_KeepsTheCAMirrorUntilTheChildHasConvergedOnTheDrop).
	convergeCinderChild(t, r, cp, getProjectedCinder(t, r.Client, cp).Generation)
	cp.Spec.Infrastructure.Messaging.TLS = nil
	_, err = r.reconcileCinder(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	cn = getProjectedCinder(t, r.Client, cp)
	g.Expect(cn.Spec.Messaging.TLS).To(BeNil(),
		"the child must not keep a trust anchor whose mirror this same pass deleted")
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: cinderMessagingCASecretName(cp), Namespace: cp.CinderNamespace(),
	}, &corev1.Secret{})).NotTo(Succeed())
}

// TestReconcileCinder_KeepsTheCAMirrorUntilTheChildHasConvergedOnTheDrop covers the
// far side of the apply that removes the pointer. Removing spec.messaging.tls from
// the CR does not remove the volume from the workloads: the cinder-operator renders
// the Deployments on a pass of its own, and until it has, the live pod templates
// still name the mirror as a REQUIRED Secret volume source. Reaping it in that
// window leaves every pod created in it (an eviction, a node drain, a rollout the
// operator triggers for an unrelated digest) stuck on FailedMount, and a
// cinder-operator that is down or backing off never closes the window at all. So
// the reap waits for the child to report the generation the apply produced.
func TestReconcileCinder_KeepsTheCAMirrorUntilTheChildHasConvergedOnTheDrop(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cp := cinderControlPlane()
	cp.Spec.Infrastructure.Messaging.TLS = &commonv1.MessagingTLSSpec{
		CABundleSecretRef: commonv1.SecretRefSpec{Name: "bus-ca", Key: "ca.crt"},
	}
	busCA := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "bus-ca", Namespace: cp.Namespace},
		Data:       map[string][]byte{"ca.crt": []byte("bus bundle")},
	}
	r := newCinderTestReconciler(t, cp, busCA)
	mirrorKey := types.NamespacedName{
		Name: cinderMessagingCASecretName(cp), Namespace: cp.CinderNamespace(),
	}

	_, err := r.reconcileCinder(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(r.Get(ctx, mirrorKey, &corev1.Secret{})).To(Succeed())

	// Pin the child one generation behind: Ready and converged on the spec that still
	// carried the pointer, which is exactly the status the API server returns to the
	// apply that drops it.
	cn := getProjectedCinder(t, r.Client, cp)
	cn.Generation = 2
	g.Expect(r.Client.Update(ctx, cn)).To(Succeed())
	convergeCinderChild(t, r, cp, 1)

	cp.Spec.Infrastructure.Messaging.TLS = nil
	_, err = r.reconcileCinder(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedCinder(t, r.Client, cp).Spec.Messaging.TLS).To(BeNil(),
		"the apply must drop the pointer from the CR")
	g.Expect(r.Get(ctx, mirrorKey, &corev1.Secret{})).To(Succeed(),
		"the mirror must outlive the pointer until the child has re-rendered without it")

	// The cinder-operator catches up: the child reports the generation the apply
	// produced, so the volume source is gone from the workloads too.
	convergeCinderChild(t, r, cp, 2)
	_, err = r.reconcileCinder(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(apierrors.IsNotFound(r.Get(ctx, mirrorKey, &corev1.Secret{}))).To(BeTrue(),
		"a converged child leaves no trust anchor behind in the Cinder namespace")
}

// TestReconcileCinder_KeepsTheCAMirrorWhileTheChildStillNamesIt covers the window
// the same-pass revert above does not: the gates between the messaging leg and the
// projection can halt the pass with the child's spec.messaging.tls still naming the
// mirror. Reaping the mirror on the messaging leg would leave the LIVE child
// pointing at a volume source that no longer exists, so every Cinder pod that
// restarts during a service-account or DB-credential rotation wedges on
// CreateContainerConfigError, with nothing in the condition set naming the cause.
// The referent must outlive its last reference, not the other way round.
func TestReconcileCinder_KeepsTheCAMirrorWhileTheChildStillNamesIt(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cp := cinderControlPlane()
	cp.Spec.Infrastructure.Messaging.TLS = &commonv1.MessagingTLSSpec{
		CABundleSecretRef: commonv1.SecretRefSpec{Name: "bus-ca", Key: "ca.crt"},
	}
	busCA := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "bus-ca", Namespace: cp.Namespace},
		Data:       map[string][]byte{"ca.crt": []byte("bus bundle")},
	}
	r := newCinderTestReconciler(t, cp, busCA)

	_, err := r.reconcileCinder(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedCinder(t, r.Client, cp).Spec.Messaging.TLS).NotTo(BeNil())

	// Drop the tls block, and halt this pass after the messaging leg by putting the
	// registration back into a rotation: the child is never re-applied, so its
	// pointer at the mirror stays live.
	cp.Spec.Infrastructure.Messaging.TLS = nil
	rotating := &c5c3v1alpha1.KeystoneService{}
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: cinderName(cp), Namespace: cp.CinderNamespace(),
	}, rotating)).To(Succeed())
	conditions.SetCondition(&rotating.Status.Conditions, metav1.Condition{
		Type:    conditionTypeKeystoneServiceAccountReady,
		Status:  metav1.ConditionFalse,
		Reason:  "RotatingPassword",
		Message: "the service account password is being rotated",
	})
	g.Expect(r.Status().Update(ctx, rotating)).To(Succeed())

	res, err := r.reconcileCinder(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).NotTo(BeZero(), "the pass has to halt on the registration gate")
	g.Expect(conditions.GetCondition(cp.Status.Conditions, conditionTypeCinderReady).Reason).
		To(Equal(reasonWaitingForServiceRegistration))

	g.Expect(getProjectedCinder(t, r.Client, cp).Spec.Messaging.TLS).NotTo(BeNil(),
		"the halted pass left the child's pointer at the mirror in place")
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: cinderMessagingCASecretName(cp), Namespace: cp.CinderNamespace(),
	}, &corev1.Secret{})).To(Succeed(),
		"the mirror must not be deleted while the live child still names it as a volume source")
}

// TestReconcileCinder_ExtraConfigMerge proves the projected child's
// spec.extraConfig is the key-by-key merge of globalExtraConfig and the per-service
// block: the per-service value wins on an overlapping key, a global-only key in the
// same section survives, and a global-only section is carried over.
func TestReconcileCinder_ExtraConfigMerge(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := cinderControlPlane()
	cp.Spec.GlobalExtraConfig = map[string]map[string]string{
		"database": {
			"connection_recycle_time": "280",
			"max_pool_size":           "5",
		},
		"DEFAULT": {"debug": "true"},
	}
	cp.Spec.Services.Cinder.ExtraConfig = map[string]map[string]string{
		"database": {"connection_recycle_time": "600"}, // overrides global
	}
	r := newCinderTestReconciler(t, cp)

	_, err := r.reconcileCinder(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	cn := getProjectedCinder(t, r.Client, cp)
	g.Expect(cn.Spec.ExtraConfig).To(Equal(map[string]map[string]string{
		"database": {
			"connection_recycle_time": "600", // per-service wins
			"max_pool_size":           "5",   // global-only key in the same section
		},
		"DEFAULT": {"debug": "true"}, // global-only section
	}), "per-service extraConfig must win, global keys/sections merged in")
}

// TestReconcileCinder_ExtraConfigClearedProjectsNil proves the field is assigned
// unconditionally: clearing both extraConfig blocks reverts the child to an absent
// spec.extraConfig rather than leaving the previously-projected value pinned.
func TestReconcileCinder_ExtraConfigClearedProjectsNil(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := cinderControlPlane()
	cp.Spec.GlobalExtraConfig = map[string]map[string]string{"DEFAULT": {"debug": "true"}}
	cp.Spec.Services.Cinder.ExtraConfig = map[string]map[string]string{
		"database": {"connection_recycle_time": "600"},
	}
	r := newCinderTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcileCinder(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedCinder(t, r.Client, cp).Spec.ExtraConfig).NotTo(BeEmpty())

	cp.Spec.GlobalExtraConfig = nil
	cp.Spec.Services.Cinder.ExtraConfig = nil
	_, err = r.reconcileCinder(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedCinder(t, r.Client, cp).Spec.ExtraConfig).To(BeNil(),
		"clearing both extraConfig blocks must revert the child")
}

func TestReconcileCinder_GatewayNilClears(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := cinderControlPlane()
	cp.Spec.Services.Cinder.Gateway = &commonv1.GatewaySpec{
		ParentRef: commonv1.GatewayParentRefSpec{Name: "openstack-gw"},
		Hostname:  "cinder.example.com",
	}
	r := newCinderTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcileCinder(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	cn := getProjectedCinder(t, r.Client, cp)
	g.Expect(cn.Spec.Gateway).NotTo(BeNil())
	g.Expect(cn.Spec.Gateway.Hostname).To(Equal("cinder.example.com"))

	// Clearing the gateway reverts the child rather than pinning the old value.
	cp.Spec.Services.Cinder.Gateway = nil
	_, err = r.reconcileCinder(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	cn = getProjectedCinder(t, r.Client, cp)
	g.Expect(cn.Spec.Gateway).To(BeNil(), "clearing the gateway must tear the HTTPRoute down")
}

func TestReconcileCinder_ReplicasOverrideAndRevert(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := cinderControlPlane()
	cp.Spec.Sizing = sizingOf(c5c3v1alpha1.SizingSpec{Cinder: &c5c3v1alpha1.CinderSizingSpec{API: apiReplicas(5)}})
	r := newCinderTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcileCinder(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedCinder(t, r.Client, cp).Spec.API.Deployment.Replicas).To(Equal(int32(5)))

	cp.Spec.Sizing = nil
	_, err = r.reconcileCinder(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedCinder(t, r.Client, cp).Spec.API.Deployment.Replicas).To(Equal(commonv1.DefaultReplicas),
		"clearing the override must revert the child to the operator default")
}

// TestReconcileCinder_MirrorsChildReady exercises the readiness mirror: a fresh
// child is not ready (WaitingForCinder + requeue), a Ready child flips CinderReady
// True.
func TestReconcileCinder_MirrorsChildReady(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := cinderControlPlane()
	r := newCinderTestReconciler(t, cp)
	ctx := context.Background()

	res, err := r.reconcileCinder(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(infraRequeueAfter))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeCinderReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForCinder"))

	convergeCinderChild(t, r, cp, getProjectedCinder(t, r.Client, cp).Generation)

	res, err = r.reconcileCinder(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond = conditions.GetCondition(cp.Status.Conditions, conditionTypeCinderReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("CinderReady"))
}

// --- the sibling services Cinder consumes ---

// TestReconcileCinder_GlanceEndpointFollowsTheGlanceBlock pins the endpoint Cinder
// creates a volume from an image through: it is derived from the sibling block by
// naming convention, and a ControlPlane that runs no image service projects none,
// which leaves create-volume-from-image the one request Cinder cannot serve.
func TestReconcileCinder_GlanceEndpointFollowsTheGlanceBlock(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	withGlance := cinderControlPlane()
	withGlance.Spec.Services.Glance = &c5c3v1alpha1.ServiceGlanceSpec{}
	r := newCinderTestReconciler(t, withGlance)
	_, err := r.reconcileCinder(ctx, withGlance)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedCinder(t, r.Client, withGlance).Spec.GlanceEndpoint).
		To(Equal("http://cp-glance.default.svc:9292"))

	withoutGlance := cinderControlPlane()
	r2 := newCinderTestReconciler(t, withoutGlance)
	_, err = r2.reconcileCinder(ctx, withoutGlance)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedCinder(t, r2.Client, withoutGlance).Spec.GlanceEndpoint).To(BeEmpty())
}

// TestReconcileCinder_KeyManagerFollowsTheBarbicanBlock pins the key manager
// castellan stores volume-encryption keys in: Barbican when the ControlPlane runs
// it, nothing at all otherwise, which leaves encrypted volumes the one volume type
// Cinder cannot create.
func TestReconcileCinder_KeyManagerFollowsTheBarbicanBlock(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	withBarbican := cinderControlPlane()
	withBarbican.Spec.Services.Barbican = &c5c3v1alpha1.ServiceBarbicanSpec{}
	r := newCinderTestReconciler(t, withBarbican)
	_, err := r.reconcileCinder(ctx, withBarbican)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedCinder(t, r.Client, withBarbican).Spec.KeyManager).
		To(Equal(&cinderv1alpha1.KeyManagerSpec{
			Type:     cinderv1alpha1.KeyManagerTypeBarbican,
			Barbican: &cinderv1alpha1.BarbicanKeyManagerSpec{Endpoint: "http://cp-barbican.default.svc:9311"},
		}))

	withoutBarbican := cinderControlPlane()
	r2 := newCinderTestReconciler(t, withoutBarbican)
	_, err = r2.reconcileCinder(ctx, withoutBarbican)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedCinder(t, r2.Client, withoutBarbican).Spec.KeyManager).To(BeNil())
}

// TestReconcileCinder_WithoutGlanceAndBarbicanStillReachesReady is the other half
// of the two fields above: neither sibling is a gate. A ControlPlane that runs
// block storage alone serves every request that needs no image and no encryption
// key, so its Cinder reaches Ready with both fields empty.
func TestReconcileCinder_WithoutGlanceAndBarbicanStillReachesReady(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := cinderControlPlane()
	g.Expect(cp.Spec.Services.Glance).To(BeNil())
	g.Expect(cp.Spec.Services.Barbican).To(BeNil())
	r := newCinderTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcileCinder(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	convergeCinderChild(t, r, cp, getProjectedCinder(t, r.Client, cp).Generation)

	res, err := r.reconcileCinder(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeCinderReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("CinderReady"))
}

// TestReconcileCinder_InternalTenantFromRegistrationIDs pins where the internal
// tenant comes from and when it is left unset. Cinder passes both IDs to the volume
// API without resolving them through Keystone, so a half-published account would
// have the image-volume cache create its volumes under a project that does not
// exist; the block stays absent until Keystone has assigned both.
func TestReconcileCinder_InternalTenantFromRegistrationIDs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		account *c5c3v1alpha1.KeystoneServiceAccountStatus
		want    *cinderv1alpha1.InternalTenantSpec
	}{
		{
			name:    "both ids published",
			account: &c5c3v1alpha1.KeystoneServiceAccountStatus{ProjectID: "p1", UserID: "u1"},
			want:    &cinderv1alpha1.InternalTenantSpec{ProjectID: "p1", UserID: "u1"},
		},
		{
			name:    "only the project id published",
			account: &c5c3v1alpha1.KeystoneServiceAccountStatus{ProjectID: "p1"},
		},
		{
			name: "no account status at all",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := cinderControlPlane()
			ks := readyCinderRegistration(cp)
			ks.Status.Account = tc.account
			r := newCinderTestReconciler(t, cp, ks)

			_, err := r.reconcileCinder(context.Background(), cp)

			g.Expect(err).NotTo(HaveOccurred(), "a half-published account is not a failure")
			g.Expect(getProjectedCinder(t, r.Client, cp).Spec.InternalTenant).To(Equal(tc.want))
		})
	}
}

// --- the projected satellites ---

// TestReconcileCinder_ProjectsTwoBackends is the field-mapping lock for the volume
// satellites: one CinderBackend per entry, named after the entry itself, attached
// to this ControlPlane's Cinder, with the NFS export and the optional cache copied
// verbatim and an unset mountOptions left for the satellite CRD's own default.
func TestReconcileCinder_ProjectsTwoBackends(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := cinderControlPlane()
	cp.Spec.Services.Cinder.Backends[0].ImageVolumeCache = &c5c3v1alpha1.CinderImageVolumeCacheSpec{
		Enabled: true, MaxSizeGB: ptr.To(int32(200)), MaxCount: ptr.To(int32(10)),
	}
	cp.Spec.Services.Cinder.Backends = append(cp.Spec.Services.Cinder.Backends,
		c5c3v1alpha1.CinderBackendEntry{
			Name: "nfs2",
			Type: "NFS",
			NFS: &c5c3v1alpha1.NFSShareSpec{
				Server:       "nfs2.example.com",
				Path:         "/volumes2",
				MountOptions: "nfsvers=4.2,soft",
			},
		})
	r := newCinderTestReconciler(t, cp)

	_, err := r.reconcileCinder(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	var list cinderv1alpha1.CinderBackendList
	g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
	names := make([]string, 0, len(list.Items))
	for i := range list.Items {
		names = append(names, list.Items[i].Name)
	}
	g.Expect(names).To(ConsistOf("nfs1", "nfs2"), "a satellite carries the entry's bare name")

	first := getProjectedCinderBackend(t, r.Client, cp, "nfs1")
	g.Expect(first.Spec.CinderRef.Name).To(Equal("cp-cinder"),
		"the backend must attach to this ControlPlane's Cinder child")
	g.Expect(first.Spec.Type).To(Equal(cinderv1alpha1.CinderBackendTypeNFS))
	g.Expect(first.Spec.NFS).To(Equal(&cinderv1alpha1.NFSBackendSpec{
		Server: "nfs-server.openstack.svc.cluster.local", Path: "/volumes",
	}), "an unset mountOptions must stay unset so the CinderBackend CRD default applies")
	g.Expect(first.Spec.ImageVolumeCache).To(Equal(&cinderv1alpha1.ImageVolumeCacheSpec{
		Enabled: true, MaxSizeGB: ptr.To(int32(200)), MaxCount: ptr.To(int32(10)),
	}))
	g.Expect(metav1.IsControlledBy(first, cp)).To(BeTrue(),
		"a co-located satellite carries the ControlPlane controller owner reference")

	second := getProjectedCinderBackend(t, r.Client, cp, "nfs2")
	g.Expect(second.Spec.NFS.MountOptions).To(Equal("nfsvers=4.2,soft"))
	g.Expect(second.Spec.ImageVolumeCache).To(BeNil(), "an entry without a cache block enables none")
	g.Expect(metav1.IsControlledBy(second, cp)).To(BeTrue())
}

// TestReconcileCinder_ProjectsBackupBackend is the field-mapping lock for the one
// backup satellite: the chunking and compression knobs are carried through when the
// entry sets them, and left unset otherwise so the CinderBackupBackend CRD's own
// defaults apply at exactly one layer.
func TestReconcileCinder_ProjectsBackupBackend(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	bare := cinderControlPlane()
	r := newCinderTestReconciler(t, bare)
	_, err := r.reconcileCinder(ctx, bare)
	g.Expect(err).NotTo(HaveOccurred())

	backupBackend := &cinderv1alpha1.CinderBackupBackend{}
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: "nfsbk", Namespace: bare.CinderNamespace(),
	}, backupBackend)).To(Succeed())
	g.Expect(backupBackend.Spec.CinderRef.Name).To(Equal("cp-cinder"))
	g.Expect(backupBackend.Spec.Type).To(Equal(cinderv1alpha1.CinderBackupBackendTypeNFS))
	g.Expect(backupBackend.Spec.NFS).To(Equal(&cinderv1alpha1.NFSBackupBackendSpec{
		Server: "nfs-server.openstack.svc.cluster.local", Path: "/backups",
	}))
	g.Expect(backupBackend.Spec.FileSize).To(BeNil(), "an unset fileSize leaves the CRD default in charge")
	g.Expect(backupBackend.Spec.Compression).To(BeEmpty())
	g.Expect(metav1.IsControlledBy(backupBackend, bare)).To(BeTrue())

	tuned := cinderControlPlane()
	tuned.Spec.Services.Cinder.BackupBackend.FileSize = ptr.To(int64(104857600))
	tuned.Spec.Services.Cinder.BackupBackend.Compression = "zstd"
	r2 := newCinderTestReconciler(t, tuned)
	_, err = r2.reconcileCinder(ctx, tuned)
	g.Expect(err).NotTo(HaveOccurred())

	tunedBackend := &cinderv1alpha1.CinderBackupBackend{}
	g.Expect(r2.Get(ctx, types.NamespacedName{
		Name: "nfsbk", Namespace: tuned.CinderNamespace(),
	}, tunedBackend)).To(Succeed())
	g.Expect(tunedBackend.Spec.FileSize).To(Equal(ptr.To(int64(104857600))))
	g.Expect(tunedBackend.Spec.FileSize).NotTo(BeIdenticalTo(tuned.Spec.Services.Cinder.BackupBackend.FileSize),
		"the projected satellite must not alias the ControlPlane spec")
	g.Expect(tunedBackend.Spec.Compression).To(Equal("zstd"))
}

// TestReconcileCinder_RemovingBackupBackendPrunesIt covers the declared teardown of
// the backup service: dropping services.cinder.backupBackend prunes the satellite,
// and the Cinder child drops its backup Deployment behind it.
func TestReconcileCinder_RemovingBackupBackendPrunesIt(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := cinderControlPlane()
	r := newCinderTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcileCinder(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	var before cinderv1alpha1.CinderBackupBackendList
	g.Expect(r.Client.List(ctx, &before)).To(Succeed())
	g.Expect(before.Items).To(HaveLen(1))

	cp.Spec.Services.Cinder.BackupBackend = nil
	_, err = r.reconcileCinder(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	var after cinderv1alpha1.CinderBackupBackendList
	g.Expect(r.Client.List(ctx, &after)).To(Succeed())
	g.Expect(after.Items).To(BeEmpty(), "removing the block must prune the satellite")
	g.Expect(getProjectedCinderBackend(t, r.Client, cp, "nfs1")).NotTo(BeNil(),
		"the volume backends are a separate kind and stay")
}

// TestReconcileCinder_PruneRemovedBackend verifies removing an entry from the spec
// prunes exactly its projected satellite and leaves the others.
func TestReconcileCinder_PruneRemovedBackend(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := cinderControlPlane()
	cp.Spec.Services.Cinder.Backends = append(cp.Spec.Services.Cinder.Backends,
		c5c3v1alpha1.CinderBackendEntry{
			Name: "nfs2",
			Type: "NFS",
			NFS:  &c5c3v1alpha1.NFSShareSpec{Server: "nfs2.example.com", Path: "/volumes2"},
		})
	r := newCinderTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcileCinder(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	var before cinderv1alpha1.CinderBackendList
	g.Expect(r.Client.List(ctx, &before)).To(Succeed())
	g.Expect(before.Items).To(HaveLen(2))

	// Drop "nfs1" from the spec.
	cp.Spec.Services.Cinder.Backends = cp.Spec.Services.Cinder.Backends[1:]
	_, err = r.reconcileCinder(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	var after cinderv1alpha1.CinderBackendList
	g.Expect(r.Client.List(ctx, &after)).To(Succeed())
	g.Expect(after.Items).To(HaveLen(1))
	g.Expect(after.Items[0].Name).To(Equal("nfs2"), "only the removed entry's satellite is pruned")
}

// TestReconcileCinder_NeverPrunesForeignBackend proves the prune is
// ownership-checked rather than name-checked, which is what an empty name prefix
// leaves it resting on: a hand-created CinderBackend attached to the same Cinder
// survives the sweep, and so does one belonging to another ControlPlane in the same
// namespace.
func TestReconcileCinder_NeverPrunesForeignBackend(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := cinderControlPlane()

	// A hand-created backend attached to the same Cinder, carrying neither our owner
	// reference nor our labels.
	handmade := &cinderv1alpha1.CinderBackend{
		ObjectMeta: metav1.ObjectMeta{Name: "byo", Namespace: cp.CinderNamespace()},
		Spec: cinderv1alpha1.CinderBackendSpec{
			CinderRef: cinderv1alpha1.CinderRefSpec{Name: cinderName(cp)},
			Type:      cinderv1alpha1.CinderBackendTypeNFS,
			NFS:       &cinderv1alpha1.NFSBackendSpec{Server: "byo.example.com", Path: "/byo"},
		},
	}
	// And one another ControlPlane in the same namespace owns by its labels.
	other := &cinderv1alpha1.CinderBackend{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "other-nfs",
			Namespace: cp.CinderNamespace(),
			Labels: map[string]string{
				controlPlaneNameLabel:      "other",
				controlPlaneNamespaceLabel: cp.Namespace,
			},
		},
		Spec: cinderv1alpha1.CinderBackendSpec{
			CinderRef: cinderv1alpha1.CinderRefSpec{Name: "other-cinder"},
			Type:      cinderv1alpha1.CinderBackendTypeNFS,
			NFS:       &cinderv1alpha1.NFSBackendSpec{Server: "other.example.com", Path: "/other"},
		},
	}
	r := newCinderTestReconciler(t, cp, handmade, other)
	ctx := context.Background()

	_, err := r.reconcileCinder(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	for _, name := range []string{"byo", "other-nfs"} {
		g.Expect(r.Get(ctx, types.NamespacedName{
			Name: name, Namespace: cp.CinderNamespace(),
		}, &cinderv1alpha1.CinderBackend{})).To(Succeed(),
			"a CinderBackend we do not own must never be pruned")
	}
}

// TestReconcileCinder_RefusesToAdoptForeignBackendInOwnNamespace is why a satellite
// takes the adoption pre-check in EVERY namespace. A bare entry name is what a
// person naming a hand-made CinderBackend picks too, and the ControlPlane's own
// namespace is where both land: adopting it would overwrite its spec and, once the
// entry is dropped, delete it. The pass refuses before the first write and leaves
// the object byte-identical.
func TestReconcileCinder_RefusesToAdoptForeignBackendInOwnNamespace(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := cinderControlPlane()
	foreign := &cinderv1alpha1.CinderBackend{
		ObjectMeta: metav1.ObjectMeta{Name: "nfs1", Namespace: cp.Namespace},
		Spec: cinderv1alpha1.CinderBackendSpec{
			CinderRef: cinderv1alpha1.CinderRefSpec{Name: "someone-else"},
			Type:      cinderv1alpha1.CinderBackendTypeNFS,
			NFS:       &cinderv1alpha1.NFSBackendSpec{Server: "foreign.example.com", Path: "/foreign"},
		},
	}
	r := newCinderTestReconciler(t, cp, foreign)

	_, err := r.reconcileCinder(context.Background(), cp)

	g.Expect(err).To(HaveOccurred(), "adopting a foreign satellite must be refused")
	g.Expect(err.Error()).To(ContainSubstring("refusing to adopt pre-existing"))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeCinderReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("CinderBackendError"))

	var live cinderv1alpha1.CinderBackend
	g.Expect(r.Get(context.Background(), types.NamespacedName{
		Name: "nfs1", Namespace: cp.Namespace,
	}, &live)).To(Succeed())
	g.Expect(live.Spec).To(Equal(foreign.Spec), "a foreign satellite must never be overwritten")
	g.Expect(live.OwnerReferences).To(BeEmpty(), "ownership must never be claimed over an object we did not create")
	g.Expect(live.Labels).NotTo(HaveKey(controlPlaneNameLabel))
}

// newCinderReconcilerWithSatelliteApplyError wires a reconciler whose every
// CinderBackend apply fails with err, the two failure modes the satellite leg maps
// onto distinct conditions.
func newCinderReconcilerWithSatelliteApplyError(
	t *testing.T, cp *c5c3v1alpha1.ControlPlane, err error,
) *ControlPlaneReconciler {
	t.Helper()
	s := cinderTestScheme(t)
	seeded := withReadyCinderRegistration(withCinderBusSecret(withReadyCinderDBCred([]client.Object{cp})))
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(seeded...).
		WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}, &cinderv1alpha1.Cinder{},
			&c5c3v1alpha1.KeystoneService{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(ctx context.Context, cl client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
				if ac, ok := obj.(client.Object); ok &&
					ac.GetObjectKind().GroupVersionKind().Kind == "CinderBackend" {
					return err
				}
				return cl.Apply(ctx, obj, opts...)
			},
		}).Build()
	return &ControlPlaneReconciler{Client: c, Scheme: s}
}

// TestReconcileCinder_InvalidSatelliteMapsToProjectionRejected pins the rejection
// leg: an Invalid (HTTP 422) answer from the Cinder API server means the projected
// satellite violates a CRD or webhook rule, which no retry fixes, so the condition
// names the spec fields an operator has to correct.
func TestReconcileCinder_InvalidSatelliteMapsToProjectionRejected(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := cinderControlPlane()
	invalid := apierrors.NewInvalid(
		schema.GroupKind{Group: "cinder.c5c3.io", Kind: "CinderBackend"}, "nfs1",
		field.ErrorList{field.Invalid(field.NewPath("spec", "nfs", "path"), "/volumes", "must be an export path")})
	r := newCinderReconcilerWithSatelliteApplyError(t, cp, invalid)

	res, err := r.reconcileCinder(context.Background(), cp)

	g.Expect(err).To(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeCinderReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("CinderBackendProjectionRejected"))
	g.Expect(cond.Message).To(ContainSubstring("services.cinder.backends"))

	var list cinderv1alpha1.CinderList
	g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty(), "no child may be applied behind a satellite that was rejected")
}

// TestReconcileCinder_SatelliteApplyErrorIsWrapped covers the other failure mode: a
// transient write failure is returned for a retry, with the satellite it was
// projecting named in the error.
func TestReconcileCinder_SatelliteApplyErrorIsWrapped(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := cinderControlPlane()
	r := newCinderReconcilerWithSatelliteApplyError(t, cp,
		apierrors.NewInternalError(errors.New("etcd is unavailable")))

	_, err := r.reconcileCinder(context.Background(), cp)

	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring(`projecting CinderBackend "nfs1":`))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeCinderReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("CinderBackendError"))
}

// TestReconcileCinder_CrossNamespaceChildAndSatellitesAreLabelledNotOwned verifies
// the ownership substitute for a block-storage service placed in a namespace of its
// own: the Cinder child and both satellite kinds carry the ControlPlane's ownership
// labels and NO owner reference (Kubernetes forbids a cross-namespace one).
func TestReconcileCinder_CrossNamespaceChildAndSatellitesAreLabelledNotOwned(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := cinderControlPlane()
	cp.Spec.Services.Cinder.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
		Name: "block", Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
	}
	r := newCinderTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcileCinder(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	cn := getProjectedCinder(t, r.Client, cp)
	g.Expect(cn.Namespace).To(Equal("block"))

	backend := getProjectedCinderBackend(t, r.Client, cp, "nfs1")
	backupBackend := &cinderv1alpha1.CinderBackupBackend{}
	g.Expect(r.Get(ctx, types.NamespacedName{Name: "nfsbk", Namespace: "block"}, backupBackend)).To(Succeed())

	for _, obj := range []client.Object{cn, backend, backupBackend} {
		g.Expect(obj.GetOwnerReferences()).To(BeEmpty(),
			"a cross-namespace child cannot carry an owner reference")
		g.Expect(obj.GetLabels()).To(HaveKeyWithValue(controlPlaneNameLabel, "cp"))
		g.Expect(obj.GetLabels()).To(HaveKeyWithValue(controlPlaneNamespaceLabel, "default"))
	}
}

// --- the projected KeystoneService registration ---

func getProjectedCinderRegistration(
	t *testing.T, c client.Client, cp *c5c3v1alpha1.ControlPlane,
) *c5c3v1alpha1.KeystoneService {
	t.Helper()
	ks := &c5c3v1alpha1.KeystoneService{}
	key := types.NamespacedName{Name: cinderName(cp), Namespace: cp.CinderNamespace()}
	if err := c.Get(context.Background(), key, ks); err != nil {
		t.Fatalf("getting projected KeystoneService %s: %v", key, err)
	}
	return ks
}

// TestReconcileCinder_ProjectsTheRegistration pins the registration's content: the
// block-storage catalog entry with both endpoint rows on the project-less /v3 path,
// the service account in its own per-service project holding the admin role beside
// service, and the explicit controlPlaneRef a child in a dedicated namespace needs
// to resolve the ControlPlane at all. The public row's three-step fallback is
// covered with it, because it is the URL every client resolves to create its
// volumes.
func TestReconcileCinder_ProjectsTheRegistration(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := cinderControlPlane()
	r := newCinderTestReconciler(t, cp)

	_, err := r.reconcileCinder(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	ks := getProjectedCinderRegistration(t, r.Client, cp)
	g.Expect(ks.Name).To(Equal("cp-cinder"))
	g.Expect(ks.Namespace).To(Equal("default"))
	g.Expect(ks.Spec.ControlPlaneRef.Name).To(Equal("cp"))
	g.Expect(ks.Spec.ControlPlaneRef.Namespace).To(Equal("default"),
		"the namespace is explicit so a child in a dedicated namespace resolves the right ControlPlane")

	g.Expect(ks.Spec.Catalog).NotTo(BeNil())
	g.Expect(ks.Spec.Catalog.ServiceType).To(Equal("block-storage"))
	g.Expect(ks.Spec.Catalog.ServiceName).To(Equal("cinder"))
	g.Expect(ks.Spec.Catalog.Adopt).To(BeFalse(), "a colliding catalog row must fail loud, never be adopted")
	g.Expect(ks.Spec.Catalog.Endpoints).To(HaveLen(2))
	g.Expect(ks.Spec.Catalog.Endpoints[0].Interface).To(Equal(c5c3v1alpha1.ExternalEndpointTypeInternal))
	g.Expect(ks.Spec.Catalog.Endpoints[0].URL).To(Equal("http://cp-cinder.default.svc:8776/v3"))
	g.Expect(ks.Spec.Catalog.Endpoints[1].Interface).To(Equal(c5c3v1alpha1.ExternalEndpointTypePublic))
	g.Expect(ks.Spec.Catalog.Endpoints[1].URL).To(Equal("http://cp-cinder.default.svc:8776/v3"),
		"an unexposed Cinder advertises the in-cluster URL on both rows")

	g.Expect(ks.Spec.Account).NotTo(BeNil())
	g.Expect(ks.Spec.Account.UserName).To(Equal("cinder"))
	g.Expect(ks.Spec.Account.DomainName).To(BeEmpty(),
		"an unset domain lets the registration resolve the ControlPlane's admin domain")
	g.Expect(ks.Spec.Account.Adopt).To(BeFalse(), "a colliding user must fail loud, never be taken over")
	g.Expect(ks.Spec.Account.Project.Name).To(Equal("service-cinder"))
	g.Expect(ks.Spec.Account.Project.Create).To(BeTrue())
	g.Expect(ks.Spec.Account.Roles).To(Equal([]string{"service", "admin"}),
		"cinder deletes the Barbican secret of an encrypted volume as a fallback, which needs admin")

	g.Expect(metav1.IsControlledBy(ks, cp)).To(BeTrue(),
		"a co-located registration carries the ControlPlane controller owner reference")

	// A gateway alone yields the default-443 form.
	gated := cinderControlPlane()
	gated.Spec.Services.Cinder.Gateway = &commonv1.GatewaySpec{
		ParentRef: commonv1.GatewayParentRefSpec{Name: "openstack-gw"},
		Hostname:  "cinder.example.com",
	}
	rGated := newCinderTestReconciler(t, gated)
	_, err = rGated.reconcileCinder(context.Background(), gated)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedCinderRegistration(t, rGated.Client, gated).Spec.Catalog.Endpoints[1].URL).
		To(Equal("https://cinder.example.com/v3"))

	// An explicit publicEndpoint wins over it, the only way to advertise a non-443
	// external port.
	explicit := cinderControlPlane()
	explicit.Spec.Services.Cinder.Gateway = gated.Spec.Services.Cinder.Gateway
	explicit.Spec.Services.Cinder.PublicEndpoint = "https://cinder.example.com:8443"
	rExplicit := newCinderTestReconciler(t, explicit)
	_, err = rExplicit.reconcileCinder(context.Background(), explicit)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedCinderRegistration(t, rExplicit.Client, explicit).Spec.Catalog.Endpoints[1].URL).
		To(Equal("https://cinder.example.com:8443/v3"))
}

// TestReconcileCinder_PlacedRegistrationEndpointsFollowTheCinder covers the
// internal row of a placed service: the in-cluster Service URL resolves nowhere
// outside its cluster, so the placed entry advertises the public URL on both
// interfaces.
func TestReconcileCinder_PlacedRegistrationEndpointsFollowTheCinder(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := placedCinderControlPlane("remote-a")
	r := newCinderTestReconciler(t, cp)

	_, err := r.reconcileCinder(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	ks := getProjectedCinderRegistration(t, r.Client, cp)
	g.Expect(ks.Spec.Catalog.Endpoints[0].URL).To(Equal("https://cinder.example.com/v3"))
	g.Expect(ks.Spec.Catalog.Endpoints[1].URL).To(Equal("https://cinder.example.com/v3"))
}

// TestReconcileCinder_CrossNamespaceRegistrationIsLabelledNotOwned verifies the
// ownership substitute for a registration in a namespace of its own: the two
// ownership labels and no owner reference.
func TestReconcileCinder_CrossNamespaceRegistrationIsLabelledNotOwned(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := cinderControlPlane()
	cp.Spec.Services.Cinder.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
		Name: "block", Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
	}
	r := newCinderTestReconciler(t, cp)

	_, err := r.reconcileCinder(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	ks := getProjectedCinderRegistration(t, r.Client, cp)
	g.Expect(ks.Namespace).To(Equal("block"))
	g.Expect(ks.OwnerReferences).To(BeEmpty(), "a cross-namespace child cannot carry an owner reference")
	g.Expect(ks.Labels).To(HaveKeyWithValue(controlPlaneNameLabel, "cp"))
	g.Expect(ks.Labels).To(HaveKeyWithValue(controlPlaneNamespaceLabel, "default"))
	g.Expect(ks.Spec.ControlPlaneRef.Namespace).To(Equal("default"))
}

// TestReconcileCinder_ReadyFoldsInTheRegistration proves CinderReady is the
// conjunction of both children: a Ready Cinder whose registration collided on the
// catalog row keeps CinderReady False, naming the failing child condition.
func TestReconcileCinder_ReadyFoldsInTheRegistration(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := cinderControlPlane()
	ks := cinderRegistration(cp,
		metav1.Condition{
			Type:    conditionTypeKeystoneServiceAccountReady,
			Status:  metav1.ConditionTrue,
			Reason:  reasonKeystoneServiceAccountProvisioned,
			Message: "account provisioned",
		},
		metav1.Condition{
			Type:    conditionTypeKeystoneServiceCatalogReady,
			Status:  metav1.ConditionFalse,
			Reason:  reasonKeystoneServiceCatalogCollision,
			Message: `a service row of type "block-storage" named "cinder" already exists`,
		},
		metav1.Condition{
			Type:    conditionTypeReady,
			Status:  metav1.ConditionFalse,
			Reason:  "NotAllReady",
			Message: "One or more sub-conditions are not ready",
		},
	)
	r := newCinderTestReconciler(t, cp, ks)
	ctx := context.Background()

	_, err := r.reconcileCinder(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	// The Cinder child itself reaches Ready.
	convergeCinderChild(t, r, cp, getProjectedCinder(t, r.Client, cp).Generation)

	res, err := r.reconcileCinder(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeCinderReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse),
		"a Cinder nothing can discover through the catalog is not ready")
	g.Expect(cond.Reason).To(Equal(reasonKeystoneServiceCatalogCollision),
		"the failing sub-condition's reason is relayed, not the aggregate's")
	g.Expect(cond.Message).To(ContainSubstring(conditionTypeKeystoneServiceCatalogReady))
	g.Expect(cond.Message).To(ContainSubstring("cp-cinder"))
}

// TestReconcileCinder_UnsetDeletesRegistrationWithOptIn verifies the opt-in
// teardown removes the registration too, which is what unregisters Cinder from the
// catalog and the identity plane.
func TestReconcileCinder_UnsetDeletesRegistrationWithOptIn(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := cinderControlPlane()
	r := newCinderTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcileCinder(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	getProjectedCinderRegistration(t, r.Client, cp)

	cp.Spec.Services.Cinder = nil
	cp.Annotations = map[string]string{cinderDeletionAllowedAnnotation: "true"}
	_, err = r.reconcileCinder(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	var list c5c3v1alpha1.KeystoneServiceList
	g.Expect(r.Client.List(ctx, &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty(), "the opt-in annotation must delete the owned registration")
}

// TestReconcileCinder_UnsetPreservesForeignRegistration is the ownership guard on
// that sweep: a same-named KeystoneService the ControlPlane does not own survives.
func TestReconcileCinder_UnsetPreservesForeignRegistration(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := cinderControlPlane()
	cp.Spec.Services.Cinder = nil
	cp.Annotations = map[string]string{cinderDeletionAllowedAnnotation: "true"}

	foreign := &c5c3v1alpha1.KeystoneService{
		ObjectMeta: metav1.ObjectMeta{Name: cinderName(cp), Namespace: cp.CinderNamespace()},
		Spec: c5c3v1alpha1.KeystoneServiceSpec{
			ControlPlaneRef: c5c3v1alpha1.ControlPlaneRefSpec{Name: "someone-else"},
		},
	}
	r := newCinderTestReconciler(t, cp, foreign)

	_, err := r.reconcileCinder(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(r.Get(context.Background(), types.NamespacedName{
		Name: cinderName(cp), Namespace: cp.CinderNamespace(),
	}, &c5c3v1alpha1.KeystoneService{})).To(Succeed(),
		"a KeystoneService we do not own must never be deleted")
}

// TestReconcileCinder_UnsetPreservesRegistrationByDefault pins the preserve
// default: without the opt-in annotation a previously projected registration stays,
// so an accidental block drop never unregisters a running service.
func TestReconcileCinder_UnsetPreservesRegistrationByDefault(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := cinderControlPlane()
	r := newCinderTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcileCinder(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	cp.Spec.Services.Cinder = nil
	_, err = r.reconcileCinder(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	getProjectedCinderRegistration(t, r.Client, cp)
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeCinderReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("CinderNotManaged"))
}

// TestReconcileCinder_NilBlockProjectsNoRegistration covers the staged-adoption
// path: a ControlPlane that manages no block-storage service registers none either.
func TestReconcileCinder_NilBlockProjectsNoRegistration(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := cinderControlPlane()
	cp.Spec.Services.Cinder = nil
	r := newCinderTestReconciler(t, cp)

	_, err := r.reconcileCinder(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	var list c5c3v1alpha1.KeystoneServiceList
	g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty())
}

// TestCinderEndpointURL pins the in-cluster address the catalog's internal row is
// built from: the projected API Service by naming convention, on the cinder
// operator's port, in the namespace the service is assigned to.
func TestCinderEndpointURL(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := cinderControlPlane()
	g.Expect(cinderEndpointURL(cp)).To(Equal("http://cp-cinder.default.svc:8776"))

	cp.Spec.Services.Cinder.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{Name: "block"}
	g.Expect(cinderEndpointURL(cp)).To(Equal("http://cp-cinder.block.svc:8776"),
		"a placed service is reached in the namespace it was assigned")
}

// TestCinderCatalogURL walks the origin precedence of the block-storage catalog row
// and pins the "/v3" suffix on each outcome: an explicit publicEndpoint wins, then
// the gateway hostname, then the in-cluster URL. The suffix is appended exactly once
// whichever origin wins, so no client ever resolves a doubled "/v3".
func TestCinderCatalogURL(t *testing.T) {
	for name, tc := range map[string]struct {
		publicEndpoint string
		gateway        *commonv1.GatewaySpec
		want           string
	}{
		"an explicit publicEndpoint wins, port and all": {
			publicEndpoint: "https://cinder.example.com:8443",
			gateway:        &commonv1.GatewaySpec{Hostname: "cinder.example.com"},
			want:           "https://cinder.example.com:8443/v3",
		},
		"a gateway alone is advertised on the default https port": {
			gateway: &commonv1.GatewaySpec{Hostname: "cinder.example.com"},
			want:    "https://cinder.example.com/v3",
		},
		"without external exposure the in-cluster URL is registered": {
			want: "http://cp-cinder.default.svc:8776/v3",
		},
		// The webhook tolerates a single trailing slash on the publicEndpoint, so the
		// origin has to be normalized before the version prefix is joined: an
		// unnormalized join registers "//v3", which every client resolves to a path
		// cinder's routes do not map.
		"a trailing slash on the publicEndpoint is joined once": {
			publicEndpoint: "https://cinder.example.com/",
			want:           "https://cinder.example.com/v3",
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := cinderControlPlane()
			cp.Spec.Services.Cinder.PublicEndpoint = tc.publicEndpoint
			cp.Spec.Services.Cinder.Gateway = tc.gateway

			got := cinderCatalogURL(cp)

			g.Expect(got).To(Equal(tc.want))
			g.Expect(strings.Count(got, "/v3")).To(Equal(1), "the version prefix is appended exactly once")
		})
	}
}

// --- per-service target clusters ---

// placedCinderControlPlane places the block-storage service in a namespace of its
// own on a target cluster. Its database is brownfield, so the DB-credential leg,
// whose own placement is covered in reconcile_cinder_dbcredentials_test.go, projects
// nothing and the pass reaches the child projection over the local client alone.
func placedCinderControlPlane(targetCluster string) *c5c3v1alpha1.ControlPlane {
	cp := cinderControlPlane()
	cp.Spec.Infrastructure.Database = commonv1.DatabaseSpec{
		Host:      "db.example.com",
		Database:  "keystone",
		SecretRef: commonv1.SecretRefSpec{Name: "keystone-db"},
	}
	cp.Spec.Services.Cinder.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
		Name:      "block",
		Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
	}
	cp.Spec.Services.Cinder.PublicEndpoint = "https://cinder.example.com"
	cp.Spec.Services.Cinder.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: targetCluster}
	return cp
}

// TestReconcileCinder_ProjectsTheTargetClusterRef verifies the placement reaches the
// child verbatim, the cinder-operator owning everything on the target, and that an
// unplaced service projects no ref.
func TestReconcileCinder_ProjectsTheTargetClusterRef(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := placedCinderControlPlane("remote-a")
	r := newCinderTestReconciler(t, cp)

	_, err := r.reconcileCinder(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(getProjectedCinder(t, r.Client, cp).Spec.TargetClusterRef).
		To(Equal(&commonv1.TargetClusterRefSpec{Name: "remote-a"}))

	unplaced := cinderControlPlane()
	r2 := newCinderTestReconciler(t, unplaced)
	_, err = r2.reconcileCinder(context.Background(), unplaced)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedCinder(t, r2.Client, unplaced).Spec.TargetClusterRef).To(BeNil(),
		"a service that names no cluster must project no ref at all")
}

// TestCinderKeystoneEndpoint_FollowsTheCinder pins the endpoint policy: Cinder
// validates tokens against Keystone itself, so it gets the in-cluster Service DNS
// name exactly while the two services share a cluster, and the public URL as soon as
// they do not, because that name resolves nowhere else.
func TestCinderKeystoneEndpoint_FollowsTheCinder(t *testing.T) {
	const (
		inCluster = "http://cp-keystone.identity.svc:5000/v3"
		public    = "https://keystone.example.com/v3"
	)
	for _, tc := range []struct {
		name             string
		cinder, keystone *commonv1.TargetClusterRefSpec
		want             string
	}{
		{name: "both co-located", want: inCluster},
		{
			name:     "both on the same cluster",
			cinder:   &commonv1.TargetClusterRefSpec{Name: "remote-a"},
			keystone: &commonv1.TargetClusterRefSpec{Name: "remote-a"},
			want:     inCluster,
		},
		{
			name:   "Cinder placed, Keystone at home",
			cinder: &commonv1.TargetClusterRefSpec{Name: "remote-a"},
			want:   public,
		},
		{
			name:     "Keystone placed, Cinder at home",
			keystone: &commonv1.TargetClusterRefSpec{Name: "remote-a"},
			want:     public,
		},
		{
			name:     "different clusters",
			cinder:   &commonv1.TargetClusterRefSpec{Name: "remote-a"},
			keystone: &commonv1.TargetClusterRefSpec{Name: "remote-b"},
			want:     public,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := cinderControlPlane()
			cp.Spec.Services.Keystone.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{Name: "identity"}
			cp.Spec.Services.Keystone.PublicEndpoint = public
			cp.Spec.Services.Keystone.TargetClusterRef = tc.keystone
			cp.Spec.Services.Cinder.TargetClusterRef = tc.cinder

			g.Expect(cinderKeystoneEndpoint(cp)).To(Equal(tc.want))
		})
	}
}

// --- the credential mirror of a placed service ---

// newPlacedCinderReconciler wires a ControlPlane whose block-storage service is
// placed on a target cluster: the CR, its registration and the shared bus live on
// the management cluster, the objects in onTarget on the other one.
func newPlacedCinderReconciler(
	t *testing.T, cp *c5c3v1alpha1.ControlPlane, resolver *childrenResolver, onTarget ...client.Object,
) *ControlPlaneReconciler {
	t.Helper()
	s := cinderTestScheme(t)
	resolver.children = fake.NewClientBuilder().WithScheme(s).WithObjects(onTarget...).Build()
	local := fake.NewClientBuilder().WithScheme(s).
		WithObjects(withCinderBusSecret(withReadyCinderRegistration([]client.Object{cp}))...).
		WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}, &cinderv1alpha1.Cinder{},
			&c5c3v1alpha1.KeystoneService{}).
		Build()
	return &ControlPlaneReconciler{Client: local, Scheme: s, Resolver: resolver}
}

// TestReconcileCinder_MirrorsRegistrationCredentialsToTheTarget covers the reason
// the mirror exists: the registration delivers its consumer Secret at home, and a
// Cinder running on another cluster reads it there, from an ExternalSecret of the
// same name, over the same OpenBao path.
func TestReconcileCinder_MirrorsRegistrationCredentialsToTheTarget(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := placedCinderControlPlane("remote-a")
	resolver := &childrenResolver{}
	r := newPlacedCinderReconciler(t, cp, resolver,
		readyTenantSecretStore(esoTenantStoreName, "block", "", ""))

	_, err := r.reconcileCinder(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	var mirror esov1.ExternalSecret
	g.Expect(resolver.children.Get(context.Background(), types.NamespacedName{
		Name: "cp-cinder-credentials", Namespace: "block",
	}, &mirror)).To(Succeed())
	g.Expect(mirror.Spec.SecretStoreRef.Name).To(Equal(esoTenantStoreName))
	g.Expect(mirror.Spec.SecretStoreRef.Kind).To(Equal(string(commonv1.SecretStoreKindNamespaced)))
	g.Expect(mirror.Spec.Target.Name).To(Equal("cp-cinder-credentials"))
	for _, d := range mirror.Spec.Data {
		g.Expect(d.RemoteRef.Key).To(Equal("openstack/keystone/block/cp-cinder/service-accounts/credentials"))
	}
	// No owner reference crosses a cluster boundary, so the labels are the whole of
	// the mirror's identity, and what the teardown sweep selects on.
	g.Expect(mirror.Labels).To(HaveKeyWithValue(controlPlaneNameLabel, "cp"))
	g.Expect(mirror.Labels).To(HaveKeyWithValue(controlPlaneNamespaceLabel, "default"))
	g.Expect(mirror.OwnerReferences).To(BeEmpty())
}

// TestReconcileCinder_NoMirrorForACoLocatedService is the other half: the
// registration's own delivery already lands in a co-located service's namespace, so
// no second ExternalSecret is written for it.
func TestReconcileCinder_NoMirrorForACoLocatedService(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := cinderControlPlane()
	r := newCinderTestReconciler(t, cp)

	_, err := r.reconcileCinder(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(r.Get(context.Background(), types.NamespacedName{
		Name: "cp-cinder-credentials", Namespace: "default",
	}, &esov1.ExternalSecret{})).NotTo(Succeed(),
		"a co-located service must get no mirror at all")
}

// TestReconcileCinder_MirrorHoldsOnAnUnresolvableCluster covers the cluster that
// does not resolve: the resolver's own text reaches the condition, and nothing is
// projected. The bus delivery reaches that cluster first, so the halt is the
// messaging leg's rather than the mirror's, on the same reason.
func TestReconcileCinder_MirrorHoldsOnAnUnresolvableCluster(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := placedCinderControlPlane("remote-a")
	resolver := &childrenResolver{err: errors.New("cluster not found")}
	r := newPlacedCinderReconciler(t, cp, resolver)

	res, err := r.reconcileCinder(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(infraRequeueAfter))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeCinderReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(commonmulticluster.TargetClusterUnavailable))
	g.Expect(cond.Message).To(ContainSubstring("cluster not found"))

	var list cinderv1alpha1.CinderList
	g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty())
}

// TestReconcileCinder_MirrorHoldsOnANotReadyTargetStore covers the store gate on the
// target cluster: an ExternalSecret written against a store that is not ready never
// syncs, so the projection waits and names the store, the namespace, and the cluster
// it is missing on.
func TestReconcileCinder_MirrorHoldsOnANotReadyTargetStore(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := placedCinderControlPlane("remote-a")
	resolver := &childrenResolver{}
	// The store exists at home but not on the target, which is the cluster the mirror
	// is materialized on.
	r := newPlacedCinderReconciler(t, cp, resolver)

	res, err := r.reconcileCinder(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeCinderReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonServiceAccountStoreNotReady))
	g.Expect(cond.Message).To(ContainSubstring(esoTenantStoreName))
	g.Expect(cond.Message).To(ContainSubstring(`namespace "block"`))
	g.Expect(cond.Message).To(ContainSubstring("target cluster"))

	g.Expect(resolver.children.Get(context.Background(), types.NamespacedName{
		Name: "cp-cinder-credentials", Namespace: "block",
	}, &esov1.ExternalSecret{})).NotTo(Succeed(), "nothing may be written against a store that is not ready")
}

// TestReconcileCinder_MirrorStoreLookupFailurePropagates covers the store read that
// fails outright, as opposed to reporting not-ready: it is wrapped with what was
// being checked and returned, so the reconcile retries with backoff.
func TestReconcileCinder_MirrorStoreLookupFailurePropagates(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := placedCinderControlPlane("remote-a")
	s := cinderTestScheme(t)
	target := fake.NewClientBuilder().WithScheme(s).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*esov1.SecretStore); ok {
					return apierrors.NewInternalError(errors.New("the target apiserver is unavailable"))
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	local := fake.NewClientBuilder().WithScheme(s).
		WithObjects(withCinderBusSecret(withReadyCinderRegistration([]client.Object{cp}))...).
		WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}, &cinderv1alpha1.Cinder{},
			&c5c3v1alpha1.KeystoneService{}).
		Build()
	r := &ControlPlaneReconciler{Client: local, Scheme: s, Resolver: &childrenResolver{children: target}}

	_, err := r.reconcileCinder(context.Background(), cp)

	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring(`in namespace "block"`))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeCinderReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonServiceRegistrationError))
}

// TestReconcileCinder_NoSizingProjectsTodaysChild pins the no-roll guarantee:
// the API at three, the scheduler, volume and backup at one, and nothing else.
func TestReconcileCinder_NoSizingProjectsTodaysChild(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := cinderControlPlane()
	r := newCinderTestReconciler(t, cp)

	_, err := r.reconcileCinder(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	cn := getProjectedCinder(t, r.Client, cp)
	expectUnsized(g, cn.Spec.API.Deployment, commonv1.DefaultReplicas)
	expectUnsized(g, cn.Spec.Scheduler.Deployment, 1)
	expectUnsized(g, cn.Spec.Volume.Deployment, 1)
	expectUnsized(g, cn.Spec.Backup.Deployment, 1)
	g.Expect(cn.Spec.API.UWSGI).To(BeNil())
	g.Expect(cn.Spec.Autoscaling).To(BeNil())
	g.Expect(cn.Spec.Jobs).To(BeNil())
}

// TestReconcileCinder_SizingProjectsComponents projects Minimal plus overrides
// and finds each on the child field it sizes. The volume and backup
// Deployments keep their one replica.
func TestReconcileCinder_SizingProjectsComponents(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := cinderControlPlane()
	scheduler := deploymentReplicas(2)
	scheduler.SpreadConstraints = hostSpread()
	cp.Spec.Sizing = minimalWith(c5c3v1alpha1.SizingSpec{Cinder: &c5c3v1alpha1.CinderSizingSpec{
		API:       &c5c3v1alpha1.APISizingSpec{ProcessSizingSpec: c5c3v1alpha1.ProcessSizingSpec{Processes: ptr.To[int32](3)}},
		Scheduler: &scheduler,
		Volume: &c5c3v1alpha1.PinnedSizingSpec{PodPlacementSpec: c5c3v1alpha1.PodPlacementSpec{
			NodeSelector: map[string]string{"storage": "nfs"},
		}},
	}})
	r := newCinderTestReconciler(t, cp)

	_, err := r.reconcileCinder(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	cn := getProjectedCinder(t, r.Client, cp)
	g.Expect(cn.Spec.API.Deployment.Replicas).To(Equal(int32(1)))
	g.Expect(cn.Spec.API.UWSGI).To(Equal(&commonv1.UWSGISpec{Processes: 3, Threads: 1}))
	g.Expect(cn.Spec.Scheduler.Deployment.Replicas).To(Equal(int32(2)))
	g.Expect(cn.Spec.Scheduler.Deployment.TopologySpreadConstraints[0].LabelSelector.MatchLabels).To(
		Equal(cinderv1alpha1.SchedulerPodSelector(cn.Name)))
	g.Expect(cn.Spec.Volume.Deployment.Replicas).To(Equal(int32(1)))
	g.Expect(cn.Spec.Volume.Deployment.NodeSelector).To(Equal(map[string]string{"storage": "nfs"}))
	g.Expect(cn.Spec.Volume.Deployment.Resources.Requests.Cpu().String()).To(Equal("50m"))
	g.Expect(cn.Spec.Backup.Deployment.Replicas).To(Equal(int32(1)))
	g.Expect(cn.Spec.Backup.Deployment.Resources.Requests.Cpu().String()).To(Equal("50m"))
	g.Expect(cn.Spec.Jobs.Resources.Requests.Cpu().String()).To(Equal("50m"))
}
