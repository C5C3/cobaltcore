// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the Barbican sub-reconciler and its secret-store child.
package controller

import (
	"context"
	"errors"
	"testing"

	openbaov1alpha1 "github.com/dc-tec/openbao-operator/api/v1alpha1"
	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esgenv1alpha1 "github.com/external-secrets/external-secrets/apis/generators/v1alpha1"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
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
	mcruntime "sigs.k8s.io/multicluster-runtime/pkg/multicluster"

	"github.com/c5c3/forge/internal/common/conditions"
	commonmulticluster "github.com/c5c3/forge/internal/common/multicluster"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	barbicanv1alpha1 "github.com/c5c3/forge/operators/barbican/api/v1alpha1"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
)

// barbicanTestScheme registers c5c3, client-go, barbican, openbao, and
// external-secrets types: the projection ensures a DB-credential ExternalSecret
// and provisions an OpenBao ensemble.
func barbicanTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("adding client-go scheme: %v", err)
	}
	if err := c5c3v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding c5c3 scheme: %v", err)
	}
	if err := barbicanv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding barbican scheme: %v", err)
	}
	if err := openbaov1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding openbao scheme: %v", err)
	}
	if err := esov1.AddToScheme(s); err != nil {
		t.Fatalf("adding external-secrets scheme: %v", err)
	}
	if err := esgenv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding external-secrets generators scheme: %v", err)
	}
	return s
}

// barbicanControlPlane builds a ControlPlane with services.barbican set on a
// dedicated secret store and a KeystoneReady=True condition — the one gate
// reconcileBarbican reads off the ControlPlane itself. The other gate is the
// projected KeystoneService child, which newBarbicanTestReconciler seeds Ready
// (see withReadyBarbicanRegistration).
func barbicanControlPlane() *c5c3v1alpha1.ControlPlane {
	cp := &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "cp",
			Namespace:  "default",
			Generation: 1,
			UID:        types.UID("cp-uid"),
		},
		Spec: c5c3v1alpha1.ControlPlaneSpec{
			OpenStackRelease: "2026.1",
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
				Keystone: &c5c3v1alpha1.ServiceKeystoneSpec{},
				Barbican: &c5c3v1alpha1.ServiceBarbicanSpec{
					SecretStore: c5c3v1alpha1.ServiceBarbicanSecretStoreSpec{
						Dedicated: &c5c3v1alpha1.BarbicanDedicatedSecretStoreSpec{},
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

// readyBarbicanRegistration builds the KeystoneService child the Barbican
// projection gates on, converged: account provisioned, catalog registered,
// aggregate Ready. A child in a dedicated namespace carries the ownership labels,
// so the projection re-applies it instead of refusing to adopt a same-named
// foreign CR.
func readyBarbicanRegistration(cp *c5c3v1alpha1.ControlPlane) *c5c3v1alpha1.KeystoneService {
	ks := barbicanRegistration(cp, metav1.Condition{
		Type:    conditionTypeKeystoneServiceAccountReady,
		Status:  metav1.ConditionTrue,
		Reason:  reasonKeystoneServiceAccountProvisioned,
		Message: "account provisioned",
	})
	conditions.SetCondition(&ks.Status.Conditions, metav1.Condition{
		Type:    conditionTypeKeystoneServiceCatalogReady,
		Status:  metav1.ConditionTrue,
		Reason:  reasonKeystoneServiceCatalogRegistered,
		Message: "catalog registered",
	})
	conditions.SetCondition(&ks.Status.Conditions, metav1.Condition{
		Type:    conditionTypeReady,
		Status:  metav1.ConditionTrue,
		Reason:  "AllReady",
		Message: "All sub-conditions are ready",
	})
	return ks
}

// barbicanRegistration builds the KeystoneService child at the projected
// name/namespace carrying the given conditions, for the tests that drive the gate
// and the readiness fold from a child that has not converged.
func barbicanRegistration(cp *c5c3v1alpha1.ControlPlane, conds ...metav1.Condition) *c5c3v1alpha1.KeystoneService {
	ks := &c5c3v1alpha1.KeystoneService{
		ObjectMeta: metav1.ObjectMeta{Name: barbicanName(cp), Namespace: cp.BarbicanNamespace()},
	}
	if ks.Namespace != cp.Namespace {
		stampControlPlaneChildLabels(ks, cp)
	}
	for _, cond := range conds {
		conditions.SetCondition(&ks.Status.Conditions, cond)
	}
	return ks
}

// externalBarbicanSecretStore switches cp's Barbican onto a secret store run
// outside this control plane, which is the mode that provisions no OpenBao
// ensemble at all.
func externalBarbicanSecretStore(cp *c5c3v1alpha1.ControlPlane) {
	cp.Spec.Services.Barbican.SecretStore = c5c3v1alpha1.ServiceBarbicanSecretStoreSpec{
		External: &c5c3v1alpha1.BarbicanExternalSecretStoreSpec{
			URL:                  "https://openbao.example.com:8200",
			CredentialsSecretRef: barbicanv1alpha1.SecretNameRefSpec{Name: "barbican-approle"},
			CABundleSecretRef:    &barbicanv1alpha1.SecretNameRefSpec{Name: "barbican-ca"},
			KVMountpoint:         "tenant-barbican",
			Namespace:            "tenant-a",
		},
	}
}

// availableBarbicanOpenBaoCluster builds the dedicated OpenBao instance as the
// openbao-operator leaves it once it serves requests, carrying this ControlPlane's
// ownership labels so the ensemble adopts it instead of refusing it.
func availableBarbicanOpenBaoCluster(cp *c5c3v1alpha1.ControlPlane) *openbaov1alpha1.OpenBaoCluster {
	instance := &openbaov1alpha1.OpenBaoCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      barbicanOpenBaoName(cp),
			Namespace: cp.BarbicanNamespace(),
			Labels:    controlPlaneChildLabels(cp),
		},
		Status: openbaov1alpha1.OpenBaoClusterStatus{
			Conditions: []metav1.Condition{{
				Type:               string(openbaov1alpha1.ConditionAvailable),
				Status:             metav1.ConditionTrue,
				Reason:             "Available",
				Message:            "serving",
				LastTransitionTime: metav1.Now(),
			}},
		},
	}
	return instance
}

// notReadyBarbicanDBCredES builds the Barbican DB-credential ExternalSecret with
// NO Ready condition, so WaitForExternalSecret reports not-Ready and the Dynamic
// readiness gate engages. Seeding it explicitly is what keeps
// withReadyBarbicanDBCred from substituting a Ready one.
func notReadyBarbicanDBCredES(cp *c5c3v1alpha1.ControlPlane) *esov1.ExternalSecret {
	es := dbCredentialGeneratorExternalSecret(barbicanDBCredentialTarget(cp))
	// Stamped as this ControlPlane's child so the cross-namespace projection path
	// re-applies it instead of refusing to adopt a same-named foreign object.
	stampControlPlaneChildLabels(es, cp)
	return es
}

// readyBarbicanDBCredES builds a Ready Barbican DB-credential ExternalSecret at
// the derived name/namespace (Dynamic default shape), so WaitForExternalSecret
// reports Ready and the projection clears its dynamic readiness gate.
func readyBarbicanDBCredES(cp *c5c3v1alpha1.ControlPlane) *esov1.ExternalSecret {
	es := notReadyBarbicanDBCredES(cp)
	es.Status = esov1.ExternalSecretStatus{
		Conditions: []esov1.ExternalSecretStatusCondition{
			{Type: esov1.ExternalSecretReady, Status: corev1.ConditionTrue},
		},
	}
	return es
}

// materialisedBarbicanDBCredSecret builds the Secret an ESO sync of the
// generator-backed ExternalSecret would materialise: an ENGINE-ISSUED username
// (the OpenBao mysql-database-plugin prefix) plus its password. The Dynamic gate
// checks the username, not just the ExternalSecret's Ready condition.
func materialisedBarbicanDBCredSecret(cp *c5c3v1alpha1.ControlPlane) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      barbicanDBCredentialSecretName(cp),
			Namespace: cp.BarbicanNamespace(),
		},
		Data: map[string][]byte{
			"username": []byte(engineIssuedUsernamePrefix + "kubernetes-barbican-abc123-1750000000"),
			"password": []byte("engine-issued-password"),
		},
	}
}

// withBarbicanGatesPassed seeds the two things a fake client never produces on its
// own and every projection test needs: the engine-issued DB credential (a Ready
// ExternalSecret plus the Secret behind it) and an Available dedicated OpenBao
// instance. Both are skipped when the test seeded its own, which is how the gate
// tests reach the branches they exercise.
func withBarbicanGatesPassed(objs []client.Object) []client.Object {
	var cp *c5c3v1alpha1.ControlPlane
	for _, o := range objs {
		if c, ok := o.(*c5c3v1alpha1.ControlPlane); ok {
			cp = c
			break
		}
	}
	if cp == nil || cp.Spec.Services.Barbican == nil {
		return objs
	}

	// Only the Dynamic path has a readiness gate; a Static or brownfield
	// ControlPlane projects a different credential shape and must be left to build
	// it.
	if barbicanDBCredentialsDynamicEnabled(cp) && effectiveBarbicanDatabase(cp) != nil &&
		effectiveBarbicanDatabase(cp).ClusterRef != nil {
		name, ns := barbicanDBCredentialSecretName(cp), cp.BarbicanNamespace()
		seeded := false
		for _, o := range objs {
			if _, ok := o.(*esov1.ExternalSecret); ok && o.GetName() == name && o.GetNamespace() == ns {
				seeded = true
			}
		}
		if !seeded {
			objs = append(objs, readyBarbicanDBCredES(cp), materialisedBarbicanDBCredSecret(cp))
		}
	}

	if !barbicanSecretStoreDedicated(cp) {
		return objs
	}
	for _, o := range objs {
		if _, ok := o.(*openbaov1alpha1.OpenBaoCluster); ok {
			return objs
		}
	}
	return append(objs, availableBarbicanOpenBaoCluster(cp))
}

// withReadyBarbicanRegistration seeds the converged KeystoneService child the
// projection gates on, unless the test seeded one of its own — which is what the
// gate and readiness-fold tests do.
//
// A fake client runs no KeystoneService controller, so without this every
// projection test would hold at the registration gate and assert against a
// Barbican that was deliberately not projected.
func withReadyBarbicanRegistration(objs []client.Object) []client.Object {
	var cp *c5c3v1alpha1.ControlPlane
	for _, o := range objs {
		if c, ok := o.(*c5c3v1alpha1.ControlPlane); ok {
			cp = c
			break
		}
	}
	if cp == nil || cp.Spec.Services.Barbican == nil {
		return objs
	}
	for _, o := range objs {
		if _, ok := o.(*c5c3v1alpha1.KeystoneService); ok {
			return objs
		}
	}
	return append(objs, readyBarbicanRegistration(cp))
}

// withBarbicanTenantStore seeds the per-tenant SecretStore in the key manager's
// namespace when that service is PLACED, unless the test seeded one of its own.
// The credential mirror a placed service gets is gated on that store, so without
// it every placement test would hold at SecretStoreNotReady.
//
// The store lands on the local client because newBarbicanTestReconciler wires no
// resolver, which resolves every namespace to the management cluster. The tests
// that exercise the two-cluster legs build their reconciler themselves.
func withBarbicanTenantStore(objs []client.Object) []client.Object {
	var cp *c5c3v1alpha1.ControlPlane
	for _, o := range objs {
		if c, ok := o.(*c5c3v1alpha1.ControlPlane); ok {
			cp = c
			break
		}
	}
	if cp == nil || targetClusterRefForNamespace(cp, cp.BarbicanNamespace()) == nil {
		return objs
	}
	for _, o := range objs {
		if _, ok := o.(*esov1.SecretStore); ok {
			return objs
		}
	}
	return append(objs, readyTenantSecretStore(esoTenantStoreName, cp.BarbicanNamespace(), "", ""))
}

func newBarbicanTestReconciler(t *testing.T, objs ...client.Object) *ControlPlaneReconciler {
	t.Helper()
	s := barbicanTestScheme(t)
	seeded := withBarbicanTenantStore(withReadyBarbicanRegistration(withBarbicanGatesPassed(objs)))
	cb := fake.NewClientBuilder().WithScheme(s).
		WithObjects(seedAPIServerEndpointSlice(seeded)...).
		WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}, &barbicanv1alpha1.Barbican{},
			&openbaov1alpha1.OpenBaoCluster{}, &c5c3v1alpha1.KeystoneService{})
	return &ControlPlaneReconciler{Client: cb.Build(), Scheme: s}
}

func getProjectedBarbican(t *testing.T, c client.Client, cp *c5c3v1alpha1.ControlPlane) *barbicanv1alpha1.Barbican {
	t.Helper()
	b := &barbicanv1alpha1.Barbican{}
	key := types.NamespacedName{Name: barbicanName(cp), Namespace: cp.BarbicanNamespace()}
	if err := c.Get(context.Background(), key, b); err != nil {
		t.Fatalf("getting projected Barbican %s: %v", key, err)
	}
	return b
}

func getProjectedBarbicanStore(
	t *testing.T, c client.Client, cp *c5c3v1alpha1.ControlPlane,
) *barbicanv1alpha1.BarbicanSecretStore {
	t.Helper()
	store := &barbicanv1alpha1.BarbicanSecretStore{}
	key := types.NamespacedName{Name: barbicanSecretStoreName(cp), Namespace: cp.BarbicanNamespace()}
	if err := c.Get(context.Background(), key, store); err != nil {
		t.Fatalf("getting projected BarbicanSecretStore %s: %v", key, err)
	}
	return store
}

// --- gates ---

func TestReconcileBarbican_NotManagedWhenUnset(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanControlPlane()
	cp.Spec.Services.Barbican = nil
	r := newBarbicanTestReconciler(t, cp)

	res, err := r.reconcileBarbican(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeBarbicanReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("BarbicanNotManaged"))

	var list barbicanv1alpha1.BarbicanList
	g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty())
}

// TestReconcileBarbican_UnsetPreservesChildAndTearsDownDynamicGenerator covers the
// preserve-by-default branch: dropping spec.services.barbican keeps the child (an
// accidental block drop must not remove a running key manager) but must NOT keep
// the credential minter. A retained VaultDynamicSecret mints a fresh MySQL user
// with ALL PRIVILEGES every refresh interval, forever, for a service the operator
// was told it no longer manages.
func TestReconcileBarbican_UnsetPreservesChildAndTearsDownDynamicGenerator(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanControlPlane()
	r := newBarbicanTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcileBarbican(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: barbicanDBCredentialSecretName(cp), Namespace: cp.BarbicanNamespace(),
	}, &esgenv1alpha1.VaultDynamicSecret{})).To(Succeed(), "the generator was projected alongside the child")

	// No opt-in annotation: the child and its secret store are preserved.
	cp.Spec.Services.Barbican = nil
	_, err = r.reconcileBarbican(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(getProjectedBarbican(t, r.Client, cp)).NotTo(BeNil(), "the child must still be preserved")
	g.Expect(getProjectedBarbicanStore(t, r.Client, cp)).NotTo(BeNil(), "its secret store must be preserved too")
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeBarbicanReady)
	g.Expect(cond.Reason).To(Equal("BarbicanNotManaged"))
	g.Expect(cond.Message).To(ContainSubstring(barbicanDeletionAllowedAnnotation))

	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: barbicanDBCredentialSecretName(cp), Namespace: cp.BarbicanNamespace(),
	}, &esgenv1alpha1.VaultDynamicSecret{})).NotTo(Succeed(),
		"the credential minter must be torn down even though the child is preserved")
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: barbicanDBCredentialServiceAccountName, Namespace: cp.BarbicanNamespace(),
	}, &corev1.ServiceAccount{})).NotTo(Succeed(), "the generator's ServiceAccount must be torn down too")
	orphanCert := &unstructured.Unstructured{}
	orphanCert.SetGroupVersionKind(certificateGVK)
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: barbicanDBCredentialClientCertName(cp), Namespace: cp.BarbicanNamespace(),
	}, orphanCert)).NotTo(Succeed(), "the generator's mTLS client Certificate must be torn down too")
}

// TestReconcileBarbican_UnsetDeletesChildStoreAndEnsembleWithOptIn verifies the
// opt-in deletion sweep removes everything the projection placed: the child, its
// secret store, every object of the dedicated OpenBao ensemble (down to the
// cluster-scoped auth-delegator binding), and the DB-credential objects.
func TestReconcileBarbican_UnsetDeletesChildStoreAndEnsembleWithOptIn(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanControlPlane()
	r := newBarbicanTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcileBarbican(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	instanceName := barbicanOpenBaoName(cp)
	ns := cp.BarbicanNamespace()
	g.Expect(r.Get(ctx, types.NamespacedName{Name: instanceName + barbicanOpenBaoTenantSuffix, Namespace: ns},
		&openbaov1alpha1.OpenBaoTenant{})).To(Succeed(), "the ensemble was projected alongside the child")

	cp.Spec.Services.Barbican = nil
	// BOTH opt-ins: the second one is what authorises destroying the stored
	// secrets along with the service.
	cp.Annotations = map[string]string{
		barbicanDeletionAllowedAnnotation:          "true",
		barbicanStoreDataDeletionAllowedAnnotation: "true",
	}

	res, err := r.reconcileBarbican(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue(), "with no finalizer holding the instance the sweep completes in one pass")

	var children barbicanv1alpha1.BarbicanList
	g.Expect(r.Client.List(ctx, &children)).To(Succeed())
	g.Expect(children.Items).To(BeEmpty(), "opt-in annotation must delete the owned child")
	var stores barbicanv1alpha1.BarbicanSecretStoreList
	g.Expect(r.Client.List(ctx, &stores)).To(Succeed())
	g.Expect(stores.Items).To(BeEmpty(), "the secret store must be swept with the child")

	gone := []struct {
		what string
		key  types.NamespacedName
		obj  client.Object
	}{
		{
			"DB-credential ExternalSecret",
			types.NamespacedName{Name: barbicanDBCredentialSecretName(cp), Namespace: ns},
			&esov1.ExternalSecret{},
		},
		{
			"VaultDynamicSecret generator",
			types.NamespacedName{Name: barbicanDBCredentialSecretName(cp), Namespace: ns},
			&esgenv1alpha1.VaultDynamicSecret{},
		},
		{
			"generator ServiceAccount",
			types.NamespacedName{Name: barbicanDBCredentialServiceAccountName, Namespace: ns},
			&corev1.ServiceAccount{},
		},
		{"OpenBaoCluster", types.NamespacedName{Name: instanceName, Namespace: ns}, &openbaov1alpha1.OpenBaoCluster{}},
		{
			"OpenBaoTenant",
			types.NamespacedName{Name: instanceName + barbicanOpenBaoTenantSuffix, Namespace: ns},
			&openbaov1alpha1.OpenBaoTenant{},
		},
		{
			"provisioner ServiceAccount",
			types.NamespacedName{Name: instanceName + barbicanOpenBaoProvisionerSuffix, Namespace: ns},
			&corev1.ServiceAccount{},
		},
		{
			"TokenRequest Role",
			types.NamespacedName{Name: instanceName + barbicanOpenBaoTokenGrantSuffix, Namespace: ns},
			&rbacv1.Role{},
		},
		{
			"TokenRequest RoleBinding",
			types.NamespacedName{Name: instanceName + barbicanOpenBaoTokenGrantSuffix, Namespace: ns},
			&rbacv1.RoleBinding{},
		},
		{
			"unseal-key Secret",
			types.NamespacedName{Name: instanceName + barbicanOpenBaoUnsealSecretSuffix, Namespace: ns},
			&corev1.Secret{},
		},
		{
			"auth-delegator ClusterRoleBinding",
			types.NamespacedName{Name: barbicanOpenBaoAuthDelegatorName(instanceName, ns)},
			&rbacv1.ClusterRoleBinding{},
		},
	}
	for _, tc := range gone {
		g.Expect(r.Get(ctx, tc.key, tc.obj)).NotTo(Succeed(), "the %s must be swept", tc.what)
	}
	for _, suffix := range []string{barbicanOpenBaoServerCertSuffix, barbicanOpenBaoCACertSuffix} {
		cert := &unstructured.Unstructured{}
		cert.SetGroupVersionKind(certificateGVK)
		g.Expect(r.Get(ctx, types.NamespacedName{Name: instanceName + suffix, Namespace: ns}, cert)).
			NotTo(Succeed(), "the %s Certificate must be swept", suffix)
	}
}

// TestReconcileBarbican_UnsetDeletionDefersTenantWhileInstanceFinalizes pins the
// one ordering constraint of the teardown: the OpenBaoTenant admits the namespace
// to the openbao-operator, and the instance's own finalizer runs under that
// admission, so the tenant only goes once the instance has left etcd.
func TestReconcileBarbican_UnsetDeletionDefersTenantWhileInstanceFinalizes(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanControlPlane()
	ns := cp.BarbicanNamespace()
	instanceName := barbicanOpenBaoName(cp)

	instance := availableBarbicanOpenBaoCluster(cp)
	instance.Finalizers = []string{"openbao.org/cluster-teardown"}
	tenant := &openbaov1alpha1.OpenBaoTenant{
		ObjectMeta: metav1.ObjectMeta{
			Name: instanceName + barbicanOpenBaoTenantSuffix, Namespace: ns, Labels: controlPlaneChildLabels(cp),
		},
	}
	cp.Spec.Services.Barbican = nil
	cp.Annotations = map[string]string{
		barbicanDeletionAllowedAnnotation:          "true",
		barbicanStoreDataDeletionAllowedAnnotation: "true",
	}
	r := newBarbicanTestReconciler(t, cp, instance, tenant)
	ctx := context.Background()

	res, err := r.reconcileBarbican(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(infraRequeueAfter))

	live := &openbaov1alpha1.OpenBaoCluster{}
	g.Expect(r.Get(ctx, types.NamespacedName{Name: instanceName, Namespace: ns}, live)).To(Succeed())
	g.Expect(live.DeletionTimestamp).NotTo(BeNil(), "the instance teardown must have been started")
	g.Expect(r.Get(ctx, types.NamespacedName{Name: instanceName + barbicanOpenBaoTenantSuffix, Namespace: ns},
		&openbaov1alpha1.OpenBaoTenant{})).To(Succeed(),
		"the tenant must outlive the instance it admits the namespace for")
}

// TestReconcileBarbican_UnsetPreservesForeignObjects proves the deletion sweep is
// ownership-checked across every object it names: a Barbican child, its secret
// store, the OpenBao instance, and the FIXED-name barbican-db-creds ServiceAccount
// that this ControlPlane does NOT own all survive an opt-in teardown. The
// ServiceAccount name is not CR-derived, so in a shared service namespace it is
// exactly the object a collision would hand to somebody else.
func TestReconcileBarbican_UnsetPreservesForeignObjects(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanControlPlane()
	cp.Spec.Services.Barbican = nil
	cp.Annotations = map[string]string{barbicanDeletionAllowedAnnotation: "true"}
	ns := cp.BarbicanNamespace()

	foreignChild := &barbicanv1alpha1.Barbican{
		ObjectMeta: metav1.ObjectMeta{Name: barbicanName(cp), Namespace: ns},
	}
	foreignStore := &barbicanv1alpha1.BarbicanSecretStore{
		ObjectMeta: metav1.ObjectMeta{Name: barbicanSecretStoreName(cp), Namespace: ns},
	}
	foreignInstance := &openbaov1alpha1.OpenBaoCluster{
		ObjectMeta: metav1.ObjectMeta{Name: barbicanOpenBaoName(cp), Namespace: ns},
	}
	foreignSA := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: barbicanDBCredentialServiceAccountName, Namespace: ns},
	}
	r := newBarbicanTestReconciler(t, cp, foreignChild, foreignStore, foreignInstance, foreignSA)
	ctx := context.Background()

	_, err := r.reconcileBarbican(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(r.Get(ctx, types.NamespacedName{Name: barbicanName(cp), Namespace: ns},
		&barbicanv1alpha1.Barbican{})).To(Succeed(), "a Barbican child we do not own must never be deleted")
	g.Expect(r.Get(ctx, types.NamespacedName{Name: barbicanSecretStoreName(cp), Namespace: ns},
		&barbicanv1alpha1.BarbicanSecretStore{})).To(Succeed(), "a foreign secret store must never be deleted")
	g.Expect(r.Get(ctx, types.NamespacedName{Name: barbicanOpenBaoName(cp), Namespace: ns},
		&openbaov1alpha1.OpenBaoCluster{})).To(Succeed(), "a foreign OpenBao instance must never be deleted")
	g.Expect(r.Get(ctx, types.NamespacedName{Name: barbicanDBCredentialServiceAccountName, Namespace: ns},
		&corev1.ServiceAccount{})).To(Succeed(), "a foreign barbican-db-creds ServiceAccount must never be deleted")
}

// TestReconcileBarbican_UnsetDeletionToleratesAlreadyGoneObjects covers the
// partially-cleaned state a repeated teardown reaches: every object the sweep
// names may already be gone, and each delete tolerates NotFound so the reconcile
// converges instead of failing on the first missing object.
func TestReconcileBarbican_UnsetDeletionToleratesAlreadyGoneObjects(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanControlPlane()
	cp.Spec.Services.Barbican = nil
	cp.Annotations = map[string]string{barbicanDeletionAllowedAnnotation: "true"}
	r := newBarbicanTestReconciler(t, cp)
	ctx := context.Background()

	// Nothing was ever projected, so every named object is already absent.
	res, err := r.reconcileBarbican(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())

	// And a second pass over the same empty state stays clean.
	_, err = r.reconcileBarbican(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeBarbicanReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("BarbicanNotManaged"))
}

// TestReconcileBarbican_UnresolvableBackingServicesRequeue is the nil-safety
// fail-safe: a webhook-bypassed CR whose database or cache does not resolve has
// nothing to project, so the projection requeues instead of dereferencing nil.
func TestReconcileBarbican_UnresolvableBackingServicesRequeue(t *testing.T) {
	t.Run("neither backing service resolves", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := barbicanControlPlane()
		cp.Spec.Infrastructure = nil
		r := newBarbicanTestReconciler(t, cp)

		res, err := r.reconcileBarbican(context.Background(), cp)

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.RequeueAfter).To(Equal(infraRequeueAfter))

		var list barbicanv1alpha1.BarbicanList
		g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
		g.Expect(list.Items).To(BeEmpty(), "nothing may be projected against unresolvable backing services")
	})

	t.Run("only the cache is unresolvable", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := barbicanControlPlane()
		// A dedicated database resolves, but with no shared block the cache does not.
		cp.Spec.Infrastructure = nil
		cp.Spec.Services.Barbican.DedicatedBackingServices = &c5c3v1alpha1.BarbicanDedicatedBackingServicesSpec{
			Database: &commonv1.DatabaseSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: "cp-barbican-db"},
				Database:   "barbican",
				SecretRef:  commonv1.SecretRefSpec{Name: "barbican-db"},
			},
		}
		r := newBarbicanTestReconciler(t, cp)

		res, err := r.reconcileBarbican(context.Background(), cp)

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.RequeueAfter).To(Equal(infraRequeueAfter))

		var list barbicanv1alpha1.BarbicanList
		g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
		g.Expect(list.Items).To(BeEmpty(), "the Barbican CRD requires a cache, so an unresolvable one blocks")
	})
}

func TestReconcileBarbican_GatedOnKeystoneReady(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanControlPlane()
	conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditionTypeKeystoneReady,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: 1,
		Reason:             "WaitingForKeystone",
		Message:            "not ready",
	})
	r := newBarbicanTestReconciler(t, cp)

	res, err := r.reconcileBarbican(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(keystoneInfraGateRequeueAfter))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeBarbicanReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForKeystone"))

	var list barbicanv1alpha1.BarbicanList
	g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty())
}

// TestReconcileBarbican_GatedOnRegistrationAccountNotReady pins the registration
// gate: while the child's AccountReady is False no Barbican is projected, the
// child's own reason and message are relayed, and a Barbican projected by an
// earlier pass is left running on the credentials it already has.
func TestReconcileBarbican_GatedOnRegistrationAccountNotReady(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanControlPlane()
	ks := barbicanRegistration(cp, metav1.Condition{
		Type:    conditionTypeKeystoneServiceAccountReady,
		Status:  metav1.ConditionFalse,
		Reason:  reasonServiceAccountCollision,
		Message: `user "barbican" already exists in Keystone`,
	})
	existing := &barbicanv1alpha1.Barbican{
		ObjectMeta: metav1.ObjectMeta{Name: barbicanName(cp), Namespace: cp.BarbicanNamespace()},
		Spec:       barbicanv1alpha1.BarbicanSpec{Region: "RegionPrevious"},
	}
	r := newBarbicanTestReconciler(t, cp, ks, existing)

	res, err := r.reconcileBarbican(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeBarbicanReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonWaitingForServiceRegistration))
	g.Expect(cond.Message).To(ContainSubstring(reasonServiceAccountCollision))
	g.Expect(cond.Message).To(ContainSubstring(`user "barbican" already exists in Keystone`))

	g.Expect(getProjectedBarbican(t, r.Client, cp).Spec.Region).To(Equal("RegionPrevious"),
		"the gate must write no Barbican at all, leaving a previously projected one untouched")
}

// TestReconcileBarbican_GatedOnRegistrationWithoutConditions covers the child that
// exists but has not been reconciled yet: the gate holds on a waiting message
// rather than reading a missing condition as ready.
func TestReconcileBarbican_GatedOnRegistrationWithoutConditions(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanControlPlane()
	r := newBarbicanTestReconciler(t, cp, barbicanRegistration(cp))

	res, err := r.reconcileBarbican(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeBarbicanReady)
	g.Expect(cond.Reason).To(Equal(reasonWaitingForServiceRegistration))
	g.Expect(cond.Message).To(ContainSubstring(conditionTypeKeystoneServiceAccountReady))

	var list barbicanv1alpha1.BarbicanList
	g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty(), "the registration gate must block projection")
}

// TestReconcileBarbican_RegistrationNotFoundAfterEnsureHolds covers the read-back
// that misses: a child the API server has not made readable yet is a wait, not an
// error.
func TestReconcileBarbican_RegistrationNotFoundAfterEnsureHolds(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanControlPlane()
	s := barbicanTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(seedAPIServerEndpointSlice(withBarbicanGatesPassed([]client.Object{cp}))...).
		WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}, &barbicanv1alpha1.Barbican{},
			&openbaov1alpha1.OpenBaoCluster{}, &c5c3v1alpha1.KeystoneService{}).
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

	res, err := r.reconcileBarbican(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred(), "a child that is not readable yet is not a failure")
	g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeBarbicanReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonWaitingForServiceRegistration))

	var list barbicanv1alpha1.BarbicanList
	g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty())
}

// TestReconcileBarbican_RegistrationReadFailureSurfaces covers the other half: a
// read that fails for any reason OTHER than absence is an error, wrapped with what
// it was reading.
func TestReconcileBarbican_RegistrationReadFailureSurfaces(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanControlPlane()
	s := barbicanTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(seedAPIServerEndpointSlice(withBarbicanGatesPassed([]client.Object{cp}))...).
		WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}, &barbicanv1alpha1.Barbican{},
			&openbaov1alpha1.OpenBaoCluster{}, &c5c3v1alpha1.KeystoneService{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*c5c3v1alpha1.KeystoneService); ok {
					return apierrors.NewInternalError(errors.New("etcd is unavailable"))
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileBarbican(context.Background(), cp)

	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("reading the barbican KeystoneService child:"))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeBarbicanReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonServiceRegistrationError))
}

// TestReconcileBarbican_NeverAdoptsForeignRegistration proves the registration
// write is refused rather than allowed to overwrite a same-named KeystoneService in
// a namespace the ControlPlane does not own: the refusal surfaces on BarbicanReady
// and the foreign CR keeps its spec.
func TestReconcileBarbican_NeverAdoptsForeignRegistration(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanControlPlane()
	cp.Spec.Services.Barbican.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
		Name: "barbican", Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
	}
	foreign := &c5c3v1alpha1.KeystoneService{
		ObjectMeta: metav1.ObjectMeta{Name: barbicanName(cp), Namespace: "barbican"},
		Spec: c5c3v1alpha1.KeystoneServiceSpec{
			ControlPlaneRef: c5c3v1alpha1.ControlPlaneRefSpec{Name: "someone-else"},
		},
	}
	r := newBarbicanTestReconciler(t, cp, foreign)

	_, err := r.reconcileBarbican(context.Background(), cp)

	g.Expect(err).To(HaveOccurred(), "adopting a foreign registration must be refused")
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeBarbicanReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonServiceRegistrationError))
	g.Expect(cond.Message).To(ContainSubstring("refusing to adopt pre-existing"))

	var live c5c3v1alpha1.KeystoneService
	g.Expect(r.Get(context.Background(), types.NamespacedName{
		Name: barbicanName(cp), Namespace: "barbican",
	}, &live)).To(Succeed())
	g.Expect(live.Spec.ControlPlaneRef.Name).To(Equal("someone-else"),
		"a foreign registration must never be overwritten")
	g.Expect(live.Labels).NotTo(HaveKey(controlPlaneNameLabel))
}

// TestReconcileBarbican_DynamicCredentialNotReady_DefersProjection is the gate
// that keeps the Dynamic default from failing OPEN. The engine role behind the
// generator is provisioned by a MANUAL onboarding step
// (setup-database-tenant.sh), while the operator rolls out on its own, so a
// ControlPlane can reach here with no role to mint against. Until the credential
// materialises no Barbican child may be projected at all.
func TestReconcileBarbican_DynamicCredentialNotReady_DefersProjection(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanControlPlane()
	r := newBarbicanTestReconciler(t, cp, notReadyBarbicanDBCredES(cp))

	res, err := r.reconcileBarbican(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(dbCredentialsRequeueAfter))

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeBarbicanReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForBarbicanDBCredential"))
	g.Expect(cond.Message).To(ContainSubstring(barbicanDBDynamicCredsPathFor(cp)),
		"the condition must name the engine path an operator has to onboard")

	var list barbicanv1alpha1.BarbicanList
	g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty(), "no Barbican child may be projected before the credential lands")
}

// TestReconcileBarbican_OpenBaoInstanceNotAvailableDefersStoreAndChild is the
// secret-store gate: a dedicated instance that does not yet serve requests holds
// back BOTH the store and the child. Attaching a store to an initialising
// instance leaves it reporting ProvisioningDenied, which the operator would then
// have to drive out of a failure state.
func TestReconcileBarbican_OpenBaoInstanceNotAvailableDefersStoreAndChild(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanControlPlane()
	// Seeded WITHOUT the Available condition, so the ensemble reports not-ready.
	initialising := availableBarbicanOpenBaoCluster(cp)
	initialising.Status = openbaov1alpha1.OpenBaoClusterStatus{}
	r := newBarbicanTestReconciler(t, cp, initialising)

	res, err := r.reconcileBarbican(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(infraRequeueAfter))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeBarbicanReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForOpenBaoInstance"))
	g.Expect(cond.Message).To(ContainSubstring(barbicanOpenBaoName(cp)))

	var stores barbicanv1alpha1.BarbicanSecretStoreList
	g.Expect(r.Client.List(context.Background(), &stores)).To(Succeed())
	g.Expect(stores.Items).To(BeEmpty(), "no store may be attached to an instance that does not serve requests")
	var children barbicanv1alpha1.BarbicanList
	g.Expect(r.Client.List(context.Background(), &children)).To(Succeed())
	g.Expect(children.Items).To(BeEmpty())
}

// --- projected child fields ---

// TestReconcileBarbican_ProjectedChildFields is the field-mapping lock for the
// projection: the release-derived image, the backing services (with the fixed
// barbican schema, the operator-owned secretRef, and the Dynamic mode of the
// managed shared database), the top-down Keystone endpoint, the region, the
// service user the registration child declares, the resolved ESO store ref, the
// gateway, the default replicas, and the deliberately-unset apiServer/dbClean.
func TestReconcileBarbican_ProjectedChildFields(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanControlPlane()
	// An exposed Keystone: its external URL must reach the child as the public
	// endpoint only, never as the token-validation endpoint.
	cp.Spec.Services.Keystone.Gateway = &commonv1.GatewaySpec{
		ParentRef: commonv1.GatewayParentRefSpec{Name: "openstack-gw"},
		Hostname:  "keystone.example.com",
	}
	cp.Spec.Services.Keystone.PublicEndpoint = "https://keystone.example.com:8443/v3"
	cp.Spec.Services.Barbican.Gateway = &commonv1.GatewaySpec{
		ParentRef: commonv1.GatewayParentRefSpec{Name: "openstack-gw"},
		Hostname:  "barbican.example.com",
	}
	r := newBarbicanTestReconciler(t, cp)

	_, err := r.reconcileBarbican(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	b := getProjectedBarbican(t, r.Client, cp)
	g.Expect(b.Name).To(Equal("cp-barbican"))
	g.Expect(b.Spec.OpenStackRelease).To(Equal("2026.1"))
	g.Expect(b.Spec.Image.Repository).To(Equal("ghcr.io/c5c3/barbican"))
	g.Expect(b.Spec.Image.Tag).To(Equal("2026.1"), "the tag defaults to spec.openStackRelease")

	// Database: the shared cluster, the fixed barbican schema, the operator-owned
	// credential Secret, and the managed-shared Dynamic default.
	g.Expect(b.Spec.Database.ClusterRef).NotTo(BeNil())
	g.Expect(b.Spec.Database.ClusterRef.Name).To(Equal("openstack-db"))
	g.Expect(b.Spec.Database.Database).To(Equal("barbican"),
		"the logical schema must be barbican, the one the OpenBao role can issue credentials for")
	g.Expect(b.Spec.Database.SecretRef.Name).To(Equal(barbicanDBCredentialSecretName(cp)))
	g.Expect(b.Spec.Database.SecretRef.Key).To(Equal("password"))
	g.Expect(b.Spec.Database.CredentialsMode).To(Equal(commonv1.CredentialsModeDynamic),
		"a managed shared barbican database defaults to Dynamic (engine-issued) credentials")
	// DeepCopy: the projected pointers must not alias the ControlPlane spec.
	g.Expect(b.Spec.Database.ClusterRef).NotTo(BeIdenticalTo(cp.Spec.Infrastructure.Database.ClusterRef))
	g.Expect(b.Spec.Cache.ClusterRef).NotTo(BeNil())
	g.Expect(b.Spec.Cache.ClusterRef.Name).To(Equal("openstack-memcached"))
	g.Expect(b.Spec.Cache.ClusterRef).NotTo(BeIdenticalTo(cp.Spec.Infrastructure.Cache.ClusterRef))

	// The Keystone endpoint is derived top-down, never from the external exposure.
	g.Expect(b.Spec.KeystoneEndpoint).To(Equal("http://cp-keystone.default.svc:5000/v3"),
		"the token-validation endpoint must be the cluster-local Service URL")
	g.Expect(b.Spec.KeystonePublicEndpoint).To(Equal("https://keystone.example.com:8443/v3"),
		"the public endpoint carries the browser/client-facing URL")

	g.Expect(b.Spec.Region).To(Equal("RegionOne"))

	// The service user names the account the registration child declares. The user
	// and project names are what the injected spec.korc.serviceAccounts entry
	// carried too, so the consumer Secret is the discriminator: it is the one the
	// registration delivers, not the one reconcileServiceAccounts materialises.
	g.Expect(b.Spec.ServiceUser.Username).To(Equal("barbican"))
	g.Expect(b.Spec.ServiceUser.ProjectName).To(Equal("service-barbican"))
	// Both domains resolve to the ControlPlane's effective admin domain, which is
	// what the registration resolves its own unset domainName to.
	g.Expect(b.Spec.ServiceUser.UserDomainName).To(Equal(adminDomainName(cp)))
	g.Expect(b.Spec.ServiceUser.ProjectDomainName).To(Equal(adminDomainName(cp)))
	g.Expect(b.Spec.ServiceUser.SecretRef.Name).To(Equal("cp-barbican-credentials"))
	g.Expect(b.Spec.ServiceUser.SecretRef.Key).To(Equal("password"))

	// The resolved ESO store selection, so the child never falls back to its own
	// shared-cluster-store default.
	g.Expect(b.Spec.SecretStoreRef).NotTo(BeNil())
	g.Expect(b.Spec.SecretStoreRef.Kind).To(Equal(commonv1.SecretStoreKindNamespaced))
	g.Expect(b.Spec.SecretStoreRef.Name).To(Equal("openbao-tenant-store"))

	g.Expect(b.Spec.Gateway).NotTo(BeNil())
	g.Expect(b.Spec.Gateway.Hostname).To(Equal("barbican.example.com"))
	g.Expect(b.Spec.Gateway).NotTo(BeIdenticalTo(cp.Spec.Services.Barbican.Gateway))

	g.Expect(b.Spec.Deployment.Replicas).To(Equal(commonv1.DefaultReplicas))
	g.Expect(b.Spec.APIServer).To(BeNil(), "the child-side uWSGI defaults stay authoritative")
	g.Expect(b.Spec.DBClean).To(BeNil(), "the clean-up knobs the operator resolves at reconcile time stay its own")
	g.Expect(metav1.IsControlledBy(b, cp)).To(BeTrue(),
		"the projected Barbican must carry the ControlPlane controller owner reference")
}

func TestReconcileBarbican_ImageOverrideWins(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanControlPlane()
	cp.Spec.Services.Barbican.Image = &commonv1.ImageSpec{
		Repository: "registry.example.com/mirror/barbican",
		Tag:        "custom",
	}
	r := newBarbicanTestReconciler(t, cp)

	_, err := r.reconcileBarbican(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	b := getProjectedBarbican(t, r.Client, cp)
	g.Expect(b.Spec.Image.Repository).To(Equal("registry.example.com/mirror/barbican"))
	g.Expect(b.Spec.Image.Tag).To(Equal("custom"))
}

// TestReconcileBarbican_DatabaseBrownfieldLeavesCredentialsModeUntouched is the
// other half of the credentials-mode contract: a database with no ClusterRef
// carries a user-supplied credential, so the mode and the secretRef are left as
// declared and no DB-credential ExternalSecret is projected.
func TestReconcileBarbican_DatabaseBrownfieldLeavesCredentialsModeUntouched(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanControlPlane()
	cp.Spec.Infrastructure.Database = commonv1.DatabaseSpec{
		Host:      "db.example.com",
		Database:  "keystone",
		SecretRef: commonv1.SecretRefSpec{Name: "brownfield-db"},
	}
	r := newBarbicanTestReconciler(t, cp)

	_, err := r.reconcileBarbican(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	b := getProjectedBarbican(t, r.Client, cp)
	g.Expect(b.Spec.Database.ClusterRef).To(BeNil())
	g.Expect(b.Spec.Database.Database).To(Equal("barbican"),
		"the logical schema is always overridden to barbican, even for a brownfield database")
	g.Expect(b.Spec.Database.CredentialsMode).To(BeEmpty(),
		"a brownfield database must keep its credentialsMode untouched")
	g.Expect(b.Spec.Database.SecretRef.Name).To(Equal("brownfield-db"),
		"a brownfield database keeps its user-supplied secretRef")

	g.Expect(r.Get(context.Background(), types.NamespacedName{
		Name: barbicanDBCredentialSecretName(cp), Namespace: cp.BarbicanNamespace(),
	}, &esov1.ExternalSecret{})).NotTo(Succeed(),
		"no DB-credential ExternalSecret is projected in brownfield mode")
}

// TestReconcileBarbican_ExtraConfigMerge proves the projected child's
// spec.extraConfig is the key-by-key merge of globalExtraConfig and the
// per-service block: the per-service value wins on an overlapping key, a
// global-only key in the same section survives, and a global-only section is
// carried over.
func TestReconcileBarbican_ExtraConfigMerge(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanControlPlane()
	cp.Spec.GlobalExtraConfig = map[string]map[string]string{
		"database": {
			"connection_recycle_time": "280",
			"max_pool_size":           "5",
		},
		"DEFAULT": {"debug": "true"},
	}
	cp.Spec.Services.Barbican.ExtraConfig = map[string]map[string]string{
		"database": {"connection_recycle_time": "600"}, // overrides global
	}
	r := newBarbicanTestReconciler(t, cp)

	_, err := r.reconcileBarbican(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	b := getProjectedBarbican(t, r.Client, cp)
	g.Expect(b.Spec.ExtraConfig).To(Equal(map[string]map[string]string{
		"database": {
			"connection_recycle_time": "600", // per-service wins
			"max_pool_size":           "5",   // global-only key in the same section
		},
		"DEFAULT": {"debug": "true"}, // global-only section
	}), "per-service extraConfig must win, global keys/sections merged in")
}

// TestReconcileBarbican_ExtraConfigClearedProjectsNil proves the field is assigned
// unconditionally: clearing both extraConfig blocks reverts the child to an absent
// spec.extraConfig rather than leaving the previously-projected value pinned.
func TestReconcileBarbican_ExtraConfigClearedProjectsNil(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanControlPlane()
	cp.Spec.GlobalExtraConfig = map[string]map[string]string{"DEFAULT": {"debug": "true"}}
	cp.Spec.Services.Barbican.ExtraConfig = map[string]map[string]string{
		"database": {"connection_recycle_time": "600"},
	}
	r := newBarbicanTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcileBarbican(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedBarbican(t, r.Client, cp).Spec.ExtraConfig).NotTo(BeEmpty())

	cp.Spec.GlobalExtraConfig = nil
	cp.Spec.Services.Barbican.ExtraConfig = nil
	_, err = r.reconcileBarbican(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedBarbican(t, r.Client, cp).Spec.ExtraConfig).To(BeNil(),
		"clearing both extraConfig blocks must revert the child")
}

func TestReconcileBarbican_GatewayNilClears(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanControlPlane()
	cp.Spec.Services.Barbican.Gateway = &commonv1.GatewaySpec{
		ParentRef: commonv1.GatewayParentRefSpec{Name: "openstack-gw"},
		Hostname:  "barbican.example.com",
	}
	r := newBarbicanTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcileBarbican(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedBarbican(t, r.Client, cp).Spec.Gateway).NotTo(BeNil())

	// Clearing the gateway reverts the child rather than pinning the old value.
	cp.Spec.Services.Barbican.Gateway = nil
	_, err = r.reconcileBarbican(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedBarbican(t, r.Client, cp).Spec.Gateway).To(BeNil(),
		"clearing the gateway must tear the HTTPRoute down")
}

func TestReconcileBarbican_ReplicasOverrideAndRevert(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanControlPlane()
	cp.Spec.Services.Barbican.Replicas = ptr.To(int32(5))
	r := newBarbicanTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcileBarbican(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedBarbican(t, r.Client, cp).Spec.Deployment.Replicas).To(Equal(int32(5)))

	cp.Spec.Services.Barbican.Replicas = nil
	_, err = r.reconcileBarbican(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedBarbican(t, r.Client, cp).Spec.Deployment.Replicas).To(Equal(commonv1.DefaultReplicas),
		"clearing the override must revert the child to the operator default")
}

// TestReconcileBarbican_MirrorsChildReady exercises the readiness mirror: a fresh
// child is not ready (WaitingForBarbican + requeue), a Ready child flips
// BarbicanReady True.
func TestReconcileBarbican_MirrorsChildReady(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanControlPlane()
	r := newBarbicanTestReconciler(t, cp)
	ctx := context.Background()

	res, err := r.reconcileBarbican(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(infraRequeueAfter))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeBarbicanReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForBarbican"))

	b := getProjectedBarbican(t, r.Client, cp)
	conditions.SetCondition(&b.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: b.Generation,
		Reason:             "AllReady",
		Message:            "ready",
	})
	g.Expect(r.Client.Status().Update(ctx, b)).To(Succeed())

	res, err = r.reconcileBarbican(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond = conditions.GetCondition(cp.Status.Conditions, conditionTypeBarbicanReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("BarbicanReady"))
}

// TestReconcileBarbican_CrossNamespaceChildrenAreLabelledNotOwned verifies the
// ownership substitute for a Barbican placed in a namespace of its own: the child
// and its secret store carry the ControlPlane's ownership labels and NO owner
// reference (Kubernetes forbids a cross-namespace one).
func TestReconcileBarbican_CrossNamespaceChildrenAreLabelledNotOwned(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanControlPlane()
	cp.Spec.Services.Barbican.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
		Name: "barbican", Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
	}
	r := newBarbicanTestReconciler(t, cp)

	_, err := r.reconcileBarbican(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	b := getProjectedBarbican(t, r.Client, cp)
	g.Expect(b.Namespace).To(Equal("barbican"))
	g.Expect(b.OwnerReferences).To(BeEmpty(), "a cross-namespace child cannot carry an owner reference")
	g.Expect(b.Labels).To(HaveKeyWithValue(controlPlaneNameLabel, "cp"))
	g.Expect(b.Labels).To(HaveKeyWithValue(controlPlaneNamespaceLabel, "default"))

	store := getProjectedBarbicanStore(t, r.Client, cp)
	g.Expect(store.Namespace).To(Equal("barbican"))
	g.Expect(store.OwnerReferences).To(BeEmpty(), "a cross-namespace store cannot carry an owner reference")
	g.Expect(store.Labels).To(HaveKeyWithValue(controlPlaneNameLabel, "cp"))
	// It still attaches to the Barbican child by name.
	g.Expect(store.Spec.BarbicanRef.Name).To(Equal("cp-barbican"))
}

// --- the projected KeystoneService registration ---

func getProjectedBarbicanRegistration(
	t *testing.T, c client.Client, cp *c5c3v1alpha1.ControlPlane,
) *c5c3v1alpha1.KeystoneService {
	t.Helper()
	ks := &c5c3v1alpha1.KeystoneService{}
	key := types.NamespacedName{Name: barbicanName(cp), Namespace: cp.BarbicanNamespace()}
	if err := c.Get(context.Background(), key, ks); err != nil {
		t.Fatalf("getting projected KeystoneService %s: %v", key, err)
	}
	return ks
}

// TestReconcileBarbican_ProjectsTheRegistration pins the registration's content:
// the key-manager catalog entry with both endpoint rows, the service account in
// its own per-service project, and the explicit controlPlaneRef a child in a
// dedicated namespace needs to resolve the ControlPlane at all.
func TestReconcileBarbican_ProjectsTheRegistration(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanControlPlane()
	r := newBarbicanTestReconciler(t, cp)

	_, err := r.reconcileBarbican(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	ks := getProjectedBarbicanRegistration(t, r.Client, cp)
	g.Expect(ks.Name).To(Equal("cp-barbican"))
	g.Expect(ks.Namespace).To(Equal("default"))
	g.Expect(ks.Spec.ControlPlaneRef.Name).To(Equal("cp"))
	g.Expect(ks.Spec.ControlPlaneRef.Namespace).To(Equal("default"),
		"the namespace is explicit so a child in a dedicated namespace resolves the right ControlPlane")

	g.Expect(ks.Spec.Catalog).NotTo(BeNil())
	g.Expect(ks.Spec.Catalog.ServiceType).To(Equal("key-manager"))
	g.Expect(ks.Spec.Catalog.ServiceName).To(Equal("barbican"))
	g.Expect(ks.Spec.Catalog.Adopt).To(BeFalse(), "a colliding catalog row must fail loud, never be adopted")
	g.Expect(ks.Spec.Catalog.Endpoints).To(HaveLen(2))
	g.Expect(ks.Spec.Catalog.Endpoints[0].Interface).To(Equal(c5c3v1alpha1.ExternalEndpointTypeInternal))
	g.Expect(ks.Spec.Catalog.Endpoints[0].URL).To(Equal(barbicanEndpointURL(cp)))
	g.Expect(ks.Spec.Catalog.Endpoints[1].Interface).To(Equal(c5c3v1alpha1.ExternalEndpointTypePublic))
	g.Expect(ks.Spec.Catalog.Endpoints[1].URL).To(Equal(barbicanCatalogURL(cp)))

	g.Expect(ks.Spec.Account).NotTo(BeNil())
	g.Expect(ks.Spec.Account.UserName).To(Equal("barbican"))
	g.Expect(ks.Spec.Account.DomainName).To(BeEmpty(),
		"an unset domain lets the registration resolve the ControlPlane's admin domain")
	g.Expect(ks.Spec.Account.Adopt).To(BeFalse(), "a colliding user must fail loud, never be taken over")
	g.Expect(ks.Spec.Account.Project.Name).To(Equal("service-barbican"))
	g.Expect(ks.Spec.Account.Project.Create).To(BeTrue())
	g.Expect(ks.Spec.Account.Roles).To(Equal([]string{"service"}))

	g.Expect(metav1.IsControlledBy(ks, cp)).To(BeTrue(),
		"a co-located registration carries the ControlPlane controller owner reference")
}

// TestReconcileBarbican_PlacedRegistrationEndpointsFollowThePlacement covers the
// internal row of a placed service: the in-cluster Service URL resolves nowhere
// outside its cluster, so the placed entry advertises the public URL on both
// interfaces.
func TestReconcileBarbican_PlacedRegistrationEndpointsFollowThePlacement(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := placedBarbicanControlPlane("remote-a")
	cp.Spec.Services.Barbican.PublicEndpoint = "https://barbican.example.com"
	r := newBarbicanTestReconciler(t, cp)

	_, err := r.reconcileBarbican(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	ks := getProjectedBarbicanRegistration(t, r.Client, cp)
	g.Expect(ks.Spec.Catalog.Endpoints[0].URL).To(Equal("https://barbican.example.com"))
	g.Expect(ks.Spec.Catalog.Endpoints[1].URL).To(Equal("https://barbican.example.com"))
}

// TestReconcileBarbican_CrossNamespaceRegistrationIsLabelledNotOwned verifies the
// ownership substitute for a registration in a namespace of its own: the two
// ownership labels and no owner reference.
func TestReconcileBarbican_CrossNamespaceRegistrationIsLabelledNotOwned(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanControlPlane()
	cp.Spec.Services.Barbican.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
		Name: "barbican", Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
	}
	r := newBarbicanTestReconciler(t, cp)

	_, err := r.reconcileBarbican(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	ks := getProjectedBarbicanRegistration(t, r.Client, cp)
	g.Expect(ks.Namespace).To(Equal("barbican"))
	g.Expect(ks.OwnerReferences).To(BeEmpty(), "a cross-namespace child cannot carry an owner reference")
	g.Expect(ks.Labels).To(HaveKeyWithValue(controlPlaneNameLabel, "cp"))
	g.Expect(ks.Labels).To(HaveKeyWithValue(controlPlaneNamespaceLabel, "default"))
	g.Expect(ks.Spec.ControlPlaneRef.Namespace).To(Equal("default"))
}

// TestReconcileBarbican_ReadyFoldsInTheRegistration proves BarbicanReady is the
// conjunction of both children: a Ready Barbican whose registration collided on
// the catalog row keeps BarbicanReady False, naming the failing child condition.
func TestReconcileBarbican_ReadyFoldsInTheRegistration(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanControlPlane()
	ks := barbicanRegistration(cp,
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
			Message: `a service row of type "key-manager" named "barbican" already exists`,
		},
		metav1.Condition{
			Type:    conditionTypeReady,
			Status:  metav1.ConditionFalse,
			Reason:  "NotAllReady",
			Message: "One or more sub-conditions are not ready",
		},
	)
	r := newBarbicanTestReconciler(t, cp, ks)
	ctx := context.Background()

	_, err := r.reconcileBarbican(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	// The Barbican child itself reaches Ready.
	b := getProjectedBarbican(t, r.Client, cp)
	conditions.SetCondition(&b.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: b.Generation,
		Reason:             "AllReady",
		Message:            "ready",
	})
	g.Expect(r.Client.Status().Update(ctx, b)).To(Succeed())

	res, err := r.reconcileBarbican(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeBarbicanReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse),
		"a Barbican nothing can discover through the catalog is not ready")
	g.Expect(cond.Reason).To(Equal(reasonKeystoneServiceCatalogCollision),
		"the failing sub-condition's reason is relayed, not the aggregate's")
	g.Expect(cond.Message).To(ContainSubstring(conditionTypeKeystoneServiceCatalogReady))
	g.Expect(cond.Message).To(ContainSubstring("cp-barbican"))
}

// TestReconcileBarbican_UnsetDeletesRegistrationWithOptIn verifies the opt-in
// teardown removes the registration too — which is what unregisters Barbican from
// the catalog and the identity plane.
func TestReconcileBarbican_UnsetDeletesRegistrationWithOptIn(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanControlPlane()
	r := newBarbicanTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcileBarbican(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	getProjectedBarbicanRegistration(t, r.Client, cp)

	cp.Spec.Services.Barbican = nil
	cp.Annotations = map[string]string{barbicanDeletionAllowedAnnotation: "true"}
	_, err = r.reconcileBarbican(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	var list c5c3v1alpha1.KeystoneServiceList
	g.Expect(r.Client.List(ctx, &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty(), "the opt-in annotation must delete the owned registration")
}

// TestReconcileBarbican_UnsetPreservesForeignRegistration is the ownership guard on
// that sweep: a same-named KeystoneService the ControlPlane does not own survives.
func TestReconcileBarbican_UnsetPreservesForeignRegistration(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanControlPlane()
	cp.Spec.Services.Barbican = nil
	cp.Annotations = map[string]string{barbicanDeletionAllowedAnnotation: "true"}

	foreign := &c5c3v1alpha1.KeystoneService{
		ObjectMeta: metav1.ObjectMeta{Name: barbicanName(cp), Namespace: cp.BarbicanNamespace()},
		Spec: c5c3v1alpha1.KeystoneServiceSpec{
			ControlPlaneRef: c5c3v1alpha1.ControlPlaneRefSpec{Name: "someone-else"},
		},
	}
	r := newBarbicanTestReconciler(t, cp, foreign)

	_, err := r.reconcileBarbican(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(r.Get(context.Background(), types.NamespacedName{
		Name: barbicanName(cp), Namespace: cp.BarbicanNamespace(),
	}, &c5c3v1alpha1.KeystoneService{})).To(Succeed(),
		"a KeystoneService we do not own must never be deleted")
}

// TestReconcileBarbican_UnsetPreservesRegistrationByDefault pins the preserve
// default: without the opt-in annotation a previously projected registration stays,
// so an accidental block drop never unregisters a running service.
func TestReconcileBarbican_UnsetPreservesRegistrationByDefault(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanControlPlane()
	r := newBarbicanTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcileBarbican(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	cp.Spec.Services.Barbican = nil
	_, err = r.reconcileBarbican(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	getProjectedBarbicanRegistration(t, r.Client, cp)
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeBarbicanReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("BarbicanNotManaged"))
}

// TestReconcileBarbican_NilBlockProjectsNoRegistration covers the staged-adoption
// path: a ControlPlane that manages no key manager registers none either.
func TestReconcileBarbican_NilBlockProjectsNoRegistration(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanControlPlane()
	cp.Spec.Services.Barbican = nil
	r := newBarbicanTestReconciler(t, cp)

	_, err := r.reconcileBarbican(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	var list c5c3v1alpha1.KeystoneServiceList
	g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty())
}

// TestReconcileBarbican_InvalidRejectionSurfacesDistinctReason covers the wedge an
// immutable child field produces: the Barbican API server rejects the UPDATE with
// an Invalid (422) error and the loop re-attempts it on every requeue with no
// self-heal, so the condition must name the rejection rather than the generic
// BarbicanError.
func TestReconcileBarbican_InvalidRejectionSurfacesDistinctReason(t *testing.T) {
	g := NewGomegaWithT(t)
	s := barbicanTestScheme(t)
	cp := barbicanControlPlane()
	// A brownfield database and an external store leave the Barbican child as the
	// only object this pass applies, so the interceptor stands in for its rejection
	// alone.
	cp.Spec.Infrastructure.Database = commonv1.DatabaseSpec{
		Host:      "db.example.com",
		Database:  "keystone",
		SecretRef: commonv1.SecretRefSpec{Name: "brownfield-db"},
	}
	externalBarbicanSecretStore(cp)
	store := barbicanSecretStoreFor(cp, cp.BarbicanNamespace())

	invalidErr := apierrors.NewInvalid(
		schema.GroupKind{Group: barbicanv1alpha1.GroupVersion.Group, Kind: "Barbican"},
		barbicanName(cp),
		field.ErrorList{field.Invalid(
			field.NewPath("spec", "database", "database"), "barbican", "database is immutable",
		)},
	)
	applies := 0
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, readyBarbicanRegistration(cp)).
		WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}, &barbicanv1alpha1.Barbican{},
			&c5c3v1alpha1.KeystoneService{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(ctx context.Context, wc client.WithWatch, ac runtime.ApplyConfiguration,
				opts ...client.ApplyOption,
			) error {
				// The registration and the store apply normally; only the child is
				// rejected.
				applies++
				if applies <= 2 {
					return wc.Apply(ctx, ac, opts...)
				}
				return invalidErr
			},
		}).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileBarbican(context.Background(), cp)

	g.Expect(err).To(HaveOccurred(), "the Invalid rejection must propagate so the manager requeues with backoff")
	g.Expect(apierrors.IsInvalid(err)).To(BeTrue())
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeBarbicanReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("BarbicanProjectionRejected"),
		"an Invalid (422) rejection must surface a distinct, actionable reason, not the generic BarbicanError")
	g.Expect(cond.Message).To(ContainSubstring("immutable"))
	// The store was applied before the child, so the rejection is the child's.
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(store),
		&barbicanv1alpha1.BarbicanSecretStore{})).To(Succeed())
}

// TestBarbicanEndpointURL pins the in-cluster Barbican API endpoint convention the
// catalog registers against: http://{name}.{ns}.svc:9311.
func TestBarbicanEndpointURL(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanControlPlane()
	g.Expect(barbicanEndpointURL(cp)).To(Equal("http://cp-barbican.default.svc:9311"))
}

// --- the secret store ---

// TestReconcileBarbican_SecretStoreDedicatedMode locks the store projected for a
// dedicated instance: exactly one store, the Barbican's default, pointing at the
// instance the ControlPlane provisions and at the one KV mount its self-init
// contract creates.
func TestReconcileBarbican_SecretStoreDedicatedMode(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanControlPlane()
	r := newBarbicanTestReconciler(t, cp)

	_, err := r.reconcileBarbican(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	var list barbicanv1alpha1.BarbicanSecretStoreList
	g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(HaveLen(1), "the ControlPlane attaches exactly one store")

	store := getProjectedBarbicanStore(t, r.Client, cp)
	g.Expect(store.Name).To(Equal("cp-barbican-store"))
	g.Expect(store.Spec.BarbicanRef.Name).To(Equal("cp-barbican"))
	g.Expect(store.Spec.Type).To(Equal(barbicanv1alpha1.BarbicanSecretStoreTypeOpenBao))
	g.Expect(store.Spec.IsDefault).To(BeTrue(), "a Barbican with no default store never reaches Ready")
	g.Expect(store.Spec.OpenBao).NotTo(BeNil())
	g.Expect(store.Spec.OpenBao.InstanceRef).NotTo(BeNil())
	g.Expect(store.Spec.OpenBao.InstanceRef.Name).To(Equal(barbicanOpenBaoName(cp)))
	g.Expect(store.Spec.OpenBao.Server).To(BeNil(), "a managed store addresses no external server")
	g.Expect(store.Spec.OpenBao.KVMountpoint).To(Equal("barbican"),
		"the projection spells the mount out so the desired store stays comparable with the defaulted live one")
	g.Expect(store.Spec.OpenBao.Namespace).To(BeEmpty(), "a managed store lives at the root namespace")
	g.Expect(metav1.IsControlledBy(store, cp)).To(BeTrue())
}

// TestReconcileBarbican_SecretStoreExternalMode is the other mode: the store
// carries the server URL and the two Secret references, and NOTHING of the
// dedicated OpenBao ensemble is projected.
func TestReconcileBarbican_SecretStoreExternalMode(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanControlPlane()
	externalBarbicanSecretStore(cp)
	r := newBarbicanTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcileBarbican(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	store := getProjectedBarbicanStore(t, r.Client, cp)
	g.Expect(store.Spec.OpenBao).NotTo(BeNil())
	g.Expect(store.Spec.OpenBao.InstanceRef).To(BeNil())
	g.Expect(store.Spec.OpenBao.Server).NotTo(BeNil())
	g.Expect(store.Spec.OpenBao.Server.URL).To(Equal("https://openbao.example.com:8200"))
	g.Expect(store.Spec.OpenBao.Server.CredentialsSecretRef.Name).To(Equal("barbican-approle"))
	g.Expect(store.Spec.OpenBao.Server.CABundleSecretRef).NotTo(BeNil())
	g.Expect(store.Spec.OpenBao.Server.CABundleSecretRef.Name).To(Equal("barbican-ca"))
	g.Expect(store.Spec.OpenBao.Server.CABundleSecretRef).NotTo(
		BeIdenticalTo(cp.Spec.Services.Barbican.SecretStore.External.CABundleSecretRef),
		"the projected reference must not alias the ControlPlane spec")
	g.Expect(store.Spec.OpenBao.KVMountpoint).To(Equal("tenant-barbican"))
	g.Expect(store.Spec.OpenBao.Namespace).To(Equal("tenant-a"))

	var instances openbaov1alpha1.OpenBaoClusterList
	g.Expect(r.Client.List(ctx, &instances)).To(Succeed())
	g.Expect(instances.Items).To(BeEmpty(), "an external store provisions no instance")
	var tenants openbaov1alpha1.OpenBaoTenantList
	g.Expect(r.Client.List(ctx, &tenants)).To(Succeed())
	g.Expect(tenants.Items).To(BeEmpty(), "and admits no namespace to the openbao-operator")
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: barbicanOpenBaoName(cp) + barbicanOpenBaoUnsealSecretSuffix, Namespace: cp.BarbicanNamespace(),
	}, &corev1.Secret{})).NotTo(Succeed(), "and generates no seal key")
}

// TestReconcileBarbican_SecretStoreImmutableDriftRecreates covers the mode flip
// the store CRD refuses to update through: the live store is deleted and the pass
// requeues, so the next one recreates it in the new mode instead of re-attempting
// a write the API server rejects forever.
func TestReconcileBarbican_SecretStoreImmutableDriftRecreates(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanControlPlane()
	r := newBarbicanTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcileBarbican(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedBarbicanStore(t, r.Client, cp).Spec.OpenBao.InstanceRef).NotTo(BeNil())

	// Flip the ControlPlane onto an external server: the store mode and the KV
	// mountpoint both move, and both are frozen by CEL.
	externalBarbicanSecretStore(cp)
	res, err := r.reconcileBarbican(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(infraRequeueAfter))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeBarbicanReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("RecreatingBarbicanSecretStore"))

	var list barbicanv1alpha1.BarbicanSecretStoreList
	g.Expect(r.Client.List(ctx, &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty(), "the store whose frozen fields moved must be deleted, not updated")

	// The next pass recreates it in the new mode.
	_, err = r.reconcileBarbican(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	store := getProjectedBarbicanStore(t, r.Client, cp)
	g.Expect(store.Spec.OpenBao.InstanceRef).To(BeNil())
	g.Expect(store.Spec.OpenBao.Server).NotTo(BeNil())
	g.Expect(store.Spec.OpenBao.KVMountpoint).To(Equal("tenant-barbican"))
}

// TestReconcileBarbican_SecretStoreMutableChangeUpdatesInPlace is the counterpart:
// a field the CRD leaves mutable (the external server URL) is updated on the live
// store, which keeps its identity rather than being recreated.
func TestReconcileBarbican_SecretStoreMutableChangeUpdatesInPlace(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanControlPlane()
	externalBarbicanSecretStore(cp)
	r := newBarbicanTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcileBarbican(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	// Mark the live store so a delete-and-recreate is distinguishable from an
	// update: the marker belongs to no field manager of ours, so an apply keeps it
	// while a recreation loses it.
	created := getProjectedBarbicanStore(t, r.Client, cp)
	created.Annotations = map[string]string{"test.c5c3.io/identity": "first"}
	g.Expect(r.Update(ctx, created)).To(Succeed())

	cp.Spec.Services.Barbican.SecretStore.External.URL = "https://openbao-2.example.com:8200"
	res, err := r.reconcileBarbican(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(infraRequeueAfter), "the pass continues to the child, which is not yet ready")

	store := getProjectedBarbicanStore(t, r.Client, cp)
	g.Expect(store.Spec.OpenBao.Server.URL).To(Equal("https://openbao-2.example.com:8200"))
	g.Expect(store.Annotations).To(HaveKeyWithValue("test.c5c3.io/identity", "first"),
		"a mutable change must update the live store, not recreate it")
}

// TestReconcileBarbican_SecretStorePruneLeavesForeignStoresAlone proves the prune
// sweep is ownership-checked: a hand-created store sharing the projected name
// prefix, and a store of somebody else's Barbican in the same namespace, both
// survive.
func TestReconcileBarbican_SecretStorePruneLeavesForeignStoresAlone(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanControlPlane()

	handmade := &barbicanv1alpha1.BarbicanSecretStore{
		ObjectMeta: metav1.ObjectMeta{Name: "cp-barbican-handmade", Namespace: cp.BarbicanNamespace()},
		Spec: barbicanv1alpha1.BarbicanSecretStoreSpec{
			BarbicanRef: barbicanv1alpha1.BarbicanRefSpec{Name: "cp-barbican"},
			Type:        barbicanv1alpha1.BarbicanSecretStoreTypeOpenBao,
			OpenBao: &barbicanv1alpha1.OpenBaoStoreSpec{
				Server: &barbicanv1alpha1.OpenBaoServerSpec{
					URL:                  "https://byo.example.com",
					CredentialsSecretRef: barbicanv1alpha1.SecretNameRefSpec{Name: "byo-creds"},
				},
				KVMountpoint: "byo",
			},
		},
	}
	// A store belonging to a different Barbican entirely: outside our prefix AND
	// unowned, so neither the prefix nor the ownership test claims it.
	other := handmade.DeepCopy()
	other.Name = "other-barbican-store"
	other.Spec.BarbicanRef.Name = "other-barbican"
	// And the POSITIVE case the sweep exists for: a store this ControlPlane
	// projected under a name it no longer declares — what a spec edit that renames
	// the store leaves behind. Without it the r.Delete below is never executed.
	stale := handmade.DeepCopy()
	stale.Name = "cp-barbican-stale"
	stale.Labels = controlPlaneChildLabels(cp)
	r := newBarbicanTestReconciler(t, cp, handmade, other, stale)
	ctx := context.Background()

	_, err := r.reconcileBarbican(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	for _, name := range []string{"cp-barbican-handmade", "other-barbican-store"} {
		live := &barbicanv1alpha1.BarbicanSecretStore{}
		g.Expect(r.Get(ctx, types.NamespacedName{Name: name, Namespace: cp.BarbicanNamespace()}, live)).
			To(Succeed(), "a store we do not own must never be pruned")
		g.Expect(live.Labels).NotTo(HaveKey(controlPlaneNameLabel),
			"ownership must never be claimed over a store we did not create")
	}
	g.Expect(r.Get(ctx, types.NamespacedName{Name: "cp-barbican-stale", Namespace: cp.BarbicanNamespace()},
		&barbicanv1alpha1.BarbicanSecretStore{})).NotTo(Succeed(),
		"an owned, prefix-matching store nobody declares any more must be pruned")
}

// TestReconcileBarbican_SecretStoreNeverAdoptsForeignStore proves the projection
// refuses foreign adoption in a namespace the ControlPlane does not own: a
// pre-existing, unowned store at the projected name is neither overwritten nor
// adopted, and the refusal surfaces as BarbicanSecretStoreError.
func TestReconcileBarbican_SecretStoreNeverAdoptsForeignStore(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanControlPlane()
	cp.Spec.Services.Barbican.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
		Name: "barbican", Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleExternal,
	}
	externalBarbicanSecretStore(cp)

	foreign := &barbicanv1alpha1.BarbicanSecretStore{
		ObjectMeta: metav1.ObjectMeta{Name: barbicanSecretStoreName(cp), Namespace: "barbican"},
		Spec: barbicanv1alpha1.BarbicanSecretStoreSpec{
			BarbicanRef: barbicanv1alpha1.BarbicanRefSpec{Name: "someone-else"},
			Type:        barbicanv1alpha1.BarbicanSecretStoreTypeOpenBao,
			OpenBao: &barbicanv1alpha1.OpenBaoStoreSpec{
				Server: &barbicanv1alpha1.OpenBaoServerSpec{
					URL:                  "https://foreign.example.com",
					CredentialsSecretRef: barbicanv1alpha1.SecretNameRefSpec{Name: "foreign-creds"},
				},
				KVMountpoint: "tenant-barbican",
			},
		},
	}
	r := newBarbicanTestReconciler(t, cp, foreign)

	_, err := r.reconcileBarbican(context.Background(), cp)

	g.Expect(err).To(HaveOccurred(), "adopting a foreign secret store must be refused")
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeBarbicanReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("BarbicanSecretStoreError"))

	live := &barbicanv1alpha1.BarbicanSecretStore{}
	g.Expect(r.Get(context.Background(), client.ObjectKeyFromObject(foreign), live)).To(Succeed())
	g.Expect(live.Spec.BarbicanRef.Name).To(Equal("someone-else"), "a foreign store must never be overwritten")
	g.Expect(live.Labels).NotTo(HaveKey(controlPlaneNameLabel))

	var children barbicanv1alpha1.BarbicanList
	g.Expect(r.Client.List(context.Background(), &children)).To(Succeed())
	g.Expect(children.Items).To(BeEmpty(), "no child may be projected against a store that was never attached")
}

// TestReconcileBarbican_SecretStoreRejectionSurfacesDistinctReason pins the store's
// own rejection vocabulary: an Invalid (422) response to the store apply is
// reported as BarbicanSecretStoreProjectionRejected rather than the generic error
// reason, so the wedge names the block an operator has to fix.
func TestReconcileBarbican_SecretStoreRejectionSurfacesDistinctReason(t *testing.T) {
	g := NewGomegaWithT(t)
	s := barbicanTestScheme(t)
	cp := barbicanControlPlane()
	// Brownfield database and external store: the store is the only object this
	// pass applies past the registration.
	cp.Spec.Infrastructure.Database = commonv1.DatabaseSpec{
		Host:      "db.example.com",
		Database:  "keystone",
		SecretRef: commonv1.SecretRefSpec{Name: "brownfield-db"},
	}
	externalBarbicanSecretStore(cp)

	invalidErr := apierrors.NewInvalid(
		schema.GroupKind{Group: barbicanv1alpha1.GroupVersion.Group, Kind: "BarbicanSecretStore"},
		barbicanSecretStoreName(cp),
		field.ErrorList{field.Invalid(
			field.NewPath("spec", "openBao", "kvMountpoint"), "tenant-barbican", "kvMountpoint is immutable",
		)},
	)
	applies := 0
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, readyBarbicanRegistration(cp)).
		WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}, &barbicanv1alpha1.Barbican{},
			&c5c3v1alpha1.KeystoneService{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(ctx context.Context, wc client.WithWatch, ac runtime.ApplyConfiguration,
				opts ...client.ApplyOption,
			) error {
				// The registration applies normally; the store is the next object out.
				applies++
				if applies == 1 {
					return wc.Apply(ctx, ac, opts...)
				}
				return invalidErr
			},
		}).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileBarbican(context.Background(), cp)

	g.Expect(err).To(HaveOccurred())
	g.Expect(apierrors.IsInvalid(err)).To(BeTrue())
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeBarbicanReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("BarbicanSecretStoreProjectionRejected"))
	g.Expect(cond.Message).To(ContainSubstring("services.barbican.secretStore"))

	var children barbicanv1alpha1.BarbicanList
	g.Expect(c.List(context.Background(), &children)).To(Succeed())
	g.Expect(children.Items).To(BeEmpty(), "the child must not be projected past a rejected store")
}

// --- credentials-mode projection ---

// staleStaticBarbicanDBCredSecret builds the Secret a MIGRATED cluster is left
// with right after the Static->Dynamic flip: materialised by the last STATIC
// sync, so it still carries the retired bootstrap's username=barbican seed. That
// name is a syntactically valid username, so a gate that only checked for a
// non-empty username would wave it through — but no MySQL user was ever created
// under it (the static login is the Barbican CR name).
func staleStaticBarbicanDBCredSecret(cp *c5c3v1alpha1.ControlPlane) *corev1.Secret {
	secret := materialisedBarbicanDBCredSecret(cp)
	secret.Data["username"] = []byte("barbican")
	return secret
}

// TestReconcileBarbican_StaticCredentialsProjection covers the ELSE arm of the
// credentials-mode branch, which every other fixture in this file leaves
// unexecuted: a Static override builds a KV-backed ExternalSecret rather than the
// VaultDynamicSecret generator, and the child is stamped Static over the same
// operator-owned Secret name.
func TestReconcileBarbican_StaticCredentialsProjection(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanControlPlane()
	cp.Spec.Services.Barbican.DatabaseCredentialsMode = commonv1.CredentialsModeStatic
	r := newBarbicanTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcileBarbican(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	b := getProjectedBarbican(t, r.Client, cp)
	g.Expect(b.Spec.Database.CredentialsMode).To(Equal(commonv1.CredentialsModeStatic))
	g.Expect(b.Spec.Database.SecretRef.Name).To(Equal(barbicanDBCredentialSecretName(cp)))
	g.Expect(b.Spec.Database.SecretRef.Key).To(Equal("password"))

	// Static projects no generator: the dynamic objects belong to the other arm.
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: barbicanDBCredentialSecretName(cp), Namespace: cp.BarbicanNamespace(),
	}, &esgenv1alpha1.VaultDynamicSecret{})).NotTo(Succeed(),
		"the Static arm must not project the VaultDynamicSecret generator")
}

// TestReconcileBarbican_DynamicCredentialStaleStaticUsername_LeavesExistingChildStatic
// is the migration twin of the projection above, and the regression guard for the
// failure an ExternalSecret-only gate lets through. A Static->Dynamic flip
// create-or-updates the ExternalSecret IN PLACE, so on a migrated cluster it keeps
// reporting Ready from its last Static sync while the Secret behind it still holds
// the retired static seed. Flipping the child on that Ready alone would point the
// barbican-operator at a login that never existed — an outage behind
// BarbicanReady=True.
func TestReconcileBarbican_DynamicCredentialStaleStaticUsername_LeavesExistingChildStatic(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanControlPlane()
	cp.Spec.Services.Barbican.DatabaseCredentialsMode = commonv1.CredentialsModeStatic
	r := newBarbicanTestReconciler(t, cp, readyBarbicanDBCredES(cp), staleStaticBarbicanDBCredSecret(cp))
	ctx := context.Background()

	_, err := r.reconcileBarbican(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedBarbican(t, r.Client, cp).Spec.Database.CredentialsMode).
		To(Equal(commonv1.CredentialsModeStatic))

	cp.Spec.Services.Barbican.DatabaseCredentialsMode = commonv1.CredentialsModeDynamic
	res, err := r.reconcileBarbican(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(dbCredentialsRequeueAfter))

	g.Expect(getProjectedBarbican(t, r.Client, cp).Spec.Database.CredentialsMode).
		To(Equal(commonv1.CredentialsModeStatic),
			"a Ready ExternalSecret over a stale static username must not flip the running child to Dynamic")

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeBarbicanReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForBarbicanDBCredential"))
}

// --- error legs ---

// TestReconcileBarbican_DBCredentialErrorSurfacesAndReturns pins the error leg of
// the credential ensure: in a service namespace the ControlPlane does not own, a
// pre-existing foreign ExternalSecret at the derived name is never adopted, and
// the refusal is both reported as BarbicanDBCredentialError and returned to the
// pipeline rather than swallowed into a wait.
func TestReconcileBarbican_DBCredentialErrorSurfacesAndReturns(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanControlPlane()
	cp.Spec.Services.Barbican.DatabaseCredentialsMode = commonv1.CredentialsModeStatic
	cp.Spec.Services.Barbican.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
		Name: "barbican", Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleExternal,
	}
	// Somebody else's ExternalSecret under the name our Static branch projects.
	foreign := &esov1.ExternalSecret{ObjectMeta: metav1.ObjectMeta{
		Name: barbicanDBCredentialSecretName(cp), Namespace: "barbican",
	}}
	r := newBarbicanTestReconciler(t, cp, foreign)

	res, err := r.reconcileBarbican(context.Background(), cp)

	g.Expect(err).To(HaveOccurred(), "adopting a foreign ExternalSecret must be refused")
	g.Expect(res.IsZero()).To(BeTrue())
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeBarbicanReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("BarbicanDBCredentialError"))

	var list barbicanv1alpha1.BarbicanList
	g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty(), "no child may be projected against a credential that was never ensured")
}

// TestReconcileBarbican_OpenBaoErrorSurfacesAndReturns pins the other error leg:
// a same-named OpenBaoCluster the ControlPlane did not create, in a namespace it
// does not own, is refused rather than reshaped — and the refusal reaches the
// ControlPlane as BarbicanOpenBaoError instead of only the operator log. It is
// the ONLY signal an operator gets for that collision, so neither the store nor
// the child may be projected behind it.
func TestReconcileBarbican_OpenBaoErrorSurfacesAndReturns(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanControlPlane()
	cp.Spec.Services.Barbican.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
		Name: "barbican", Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleExternal,
	}
	// The UID is what refuseForeignAdoption reads as "this already exists"; the
	// fake client does not mint one on seed.
	foreign := &openbaov1alpha1.OpenBaoCluster{ObjectMeta: metav1.ObjectMeta{
		Name: barbicanOpenBaoName(cp), Namespace: "barbican", UID: types.UID("foreign-instance-uid"),
	}}
	r := newBarbicanTestReconciler(t, cp, foreign)
	ctx := context.Background()

	res, err := r.reconcileBarbican(ctx, cp)

	g.Expect(err).To(HaveOccurred(), "adopting a foreign OpenBaoCluster must be refused")
	g.Expect(res.IsZero()).To(BeTrue())
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeBarbicanReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("BarbicanOpenBaoError"))

	var children barbicanv1alpha1.BarbicanList
	g.Expect(r.Client.List(ctx, &children)).To(Succeed())
	g.Expect(children.Items).To(BeEmpty(), "no child may be projected against a refused instance")
	var stores barbicanv1alpha1.BarbicanSecretStoreList
	g.Expect(r.Client.List(ctx, &stores)).To(Succeed())
	g.Expect(stores.Items).To(BeEmpty(), "no store may be attached to a refused instance")
}

// TestReconcileBarbican_UnsetDeletionPreservesTheStoreDataWithoutTheSecondOptIn
// is the regression guard for the one delete on this branch that cannot be
// undone. The dedicated OpenBaoCluster carries DeletionPolicy DeletePVCs, so
// deleting it wipes the raft volume, and the ensemble sweep removes the
// static-seal Secret with it — every secret Barbican ever stored, gone, with no
// snapshot and no grace period. The shared allow-barbican-deletion annotation is
// the same boolean four stateless services use, so on its own it tears the
// service down and leaves the store, its volume, and its seal key standing.
func TestReconcileBarbican_UnsetDeletionPreservesTheStoreDataWithoutTheSecondOptIn(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanControlPlane()
	r := newBarbicanTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcileBarbican(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	ns, instanceName := cp.BarbicanNamespace(), barbicanOpenBaoName(cp)
	cp.Spec.Services.Barbican = nil
	cp.Annotations = map[string]string{barbicanDeletionAllowedAnnotation: "true"}

	res, err := r.reconcileBarbican(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())

	// The service is gone.
	var children barbicanv1alpha1.BarbicanList
	g.Expect(r.Client.List(ctx, &children)).To(Succeed())
	g.Expect(children.Items).To(BeEmpty(), "the shared annotation still tears the service down")
	var stores barbicanv1alpha1.BarbicanSecretStoreList
	g.Expect(r.Client.List(ctx, &stores)).To(Succeed())
	g.Expect(stores.Items).To(BeEmpty())

	// The stored secrets are not.
	kept := []struct {
		what string
		key  types.NamespacedName
		obj  client.Object
	}{
		{"OpenBaoCluster", types.NamespacedName{Name: instanceName, Namespace: ns}, &openbaov1alpha1.OpenBaoCluster{}},
		{
			"unseal-key Secret",
			types.NamespacedName{Name: instanceName + barbicanOpenBaoUnsealSecretSuffix, Namespace: ns},
			&corev1.Secret{},
		},
		{
			"OpenBaoTenant",
			types.NamespacedName{Name: instanceName + barbicanOpenBaoTenantSuffix, Namespace: ns},
			&openbaov1alpha1.OpenBaoTenant{},
		},
	}
	for _, tc := range kept {
		g.Expect(r.Get(ctx, tc.key, tc.obj)).To(Succeed(),
			"the %s must survive without the data-deletion opt-in", tc.what)
	}

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeBarbicanReady)
	g.Expect(cond.Reason).To(Equal("BarbicanNotManaged"))
	g.Expect(cond.Message).To(ContainSubstring(barbicanStoreDataDeletionAllowedAnnotation),
		"the condition must name the annotation that would destroy the stored secrets")
}

// --- per-service target clusters: the ensemble follows the service ---

// placedBarbicanControlPlane places the Barbican service, and with it the
// dedicated OpenBao ensemble, in a namespace of its own on a target cluster. Its
// database is brownfield, so the DB-credential leg — whose own placement is
// covered in reconcile_dbcredentials_test.go — projects nothing and the ensemble
// is the first thing in the pass to resolve the cluster.
func placedBarbicanControlPlane(targetCluster string) *c5c3v1alpha1.ControlPlane {
	cp := barbicanControlPlane()
	cp.Spec.Infrastructure.Database = commonv1.DatabaseSpec{
		Host:      "db.example.com",
		Database:  "keystone",
		SecretRef: commonv1.SecretRefSpec{Name: "keystone-db"},
	}
	cp.Spec.Services.Barbican.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
		Name:      "key-manager",
		Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
	}
	cp.Spec.Services.Barbican.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: targetCluster}
	return cp
}

// splitBarbicanReconciler builds a reconciler over two fake clusters: the
// management one holding cp and its converged registration, where every projected
// CR stays, and a target cluster — registered under every name — holding target,
// the OpenBao ensemble the service takes with it.
//
// The target also carries the per-tenant SecretStore the credential mirror is
// gated on, so the placed pass reaches the legs these tests are about.
func splitBarbicanReconciler(
	t *testing.T, cp *c5c3v1alpha1.ControlPlane, target ...client.Object,
) (*ControlPlaneReconciler, client.Client) {
	t.Helper()
	s := barbicanTestScheme(t)
	target = append(target, readyTenantSecretStore(esoTenantStoreName, cp.BarbicanNamespace(), "", ""))
	remote := fake.NewClientBuilder().WithScheme(s).WithObjects(target...).
		WithStatusSubresource(&openbaov1alpha1.OpenBaoCluster{}).Build()
	return &ControlPlaneReconciler{
		Client: fake.NewClientBuilder().WithScheme(s).WithObjects(cp, readyBarbicanRegistration(cp)).
			WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}, &barbicanv1alpha1.Barbican{},
				&c5c3v1alpha1.KeystoneService{}).Build(),
		Scheme:   s,
		Resolver: &childrenResolver{children: remote},
	}, remote
}

// TestReconcileBarbican_PlacedOpenBaoGateIsAnsweredFromTheTarget verifies the
// Available gate reads the instance from the cluster it was provisioned on. The
// instance exists only there, so a gate still reading at home would park on
// WaitingForOpenBaoInstance for good and the store would never be attached.
//
// It also pins what does NOT move: the BarbicanSecretStore and the Barbican child
// are the CRs the barbican-operator reconciles from the management cluster, so
// they stay there whichever cluster the ensemble behind them runs on.
func TestReconcileBarbican_PlacedOpenBaoGateIsAnsweredFromTheTarget(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cp := placedBarbicanControlPlane("remote-a")

	instance := availableBarbicanOpenBaoCluster(cp)
	instance.Labels = remoteChildLabels(cp)
	r, remote := splitBarbicanReconciler(t, cp, instance, defaultKubernetesEndpointSlice())

	res, err := r.reconcileBarbican(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(infraRequeueAfter))

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeBarbicanReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForBarbican"),
		"an Available instance on the target must carry the pass through to the child")

	g.Expect(getProjectedBarbicanStore(t, r.Client, cp)).NotTo(BeNil())
	g.Expect(getProjectedBarbican(t, r.Client, cp)).NotTo(BeNil())
	var placedChildren barbicanv1alpha1.BarbicanList
	g.Expect(remote.List(ctx, &placedChildren)).To(Succeed())
	g.Expect(placedChildren.Items).To(BeEmpty(), "the child CR is reconciled from the management cluster")
}

// TestReconcileBarbican_UnresolvableTargetParksTheEnsemble covers the cluster that
// does not resolve: the pass parks on the reason every operator reports that
// failure under and writes nothing on either cluster — not the ensemble, not the
// store, not the child. The credential mirror resolves the cluster first, so the
// requeue is the registration leg's cadence.
func TestReconcileBarbican_UnresolvableTargetParksTheEnsemble(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	s := barbicanTestScheme(t)
	cp := placedBarbicanControlPlane("remote-a")

	remote := fake.NewClientBuilder().WithScheme(s).Build()
	r := &ControlPlaneReconciler{
		Client: fake.NewClientBuilder().WithScheme(s).WithObjects(cp, readyBarbicanRegistration(cp)).
			WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}, &barbicanv1alpha1.Barbican{},
				&c5c3v1alpha1.KeystoneService{}).Build(),
		Scheme:   s,
		Resolver: &childrenResolver{children: remote, err: mcruntime.ErrClusterNotFound},
	}

	res, err := r.reconcileBarbican(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred(), "an unregistered cluster is a state to wait out, not a reconcile failure")
	g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter))

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeBarbicanReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(commonmulticluster.TargetClusterUnavailable))
	g.Expect(cond.Message).To(ContainSubstring("cluster not found"))

	for name, c := range map[string]client.Client{"management": r.Client, "target": remote} {
		var instances openbaov1alpha1.OpenBaoClusterList
		g.Expect(c.List(ctx, &instances)).To(Succeed())
		g.Expect(instances.Items).To(BeEmpty(), "no instance may be provisioned on the %s cluster", name)

		var stores barbicanv1alpha1.BarbicanSecretStoreList
		g.Expect(c.List(ctx, &stores)).To(Succeed())
		g.Expect(stores.Items).To(BeEmpty(), "no store may be attached on the %s cluster", name)

		var children barbicanv1alpha1.BarbicanList
		g.Expect(c.List(ctx, &children)).To(Succeed())
		g.Expect(children.Items).To(BeEmpty(), "no child may be projected on the %s cluster", name)
	}
}

// TestReconcileBarbican_ProjectsTheTargetClusterRef verifies the placement
// reaches the child verbatim — the barbican-operator owns everything on the
// target, so the ref is the whole hand-over — and that an unplaced service
// projects no ref.
func TestReconcileBarbican_ProjectsTheTargetClusterRef(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := placedBarbicanControlPlane("remote-a")

	instance := availableBarbicanOpenBaoCluster(cp)
	instance.Labels = remoteChildLabels(cp)
	r, _ := splitBarbicanReconciler(t, cp, instance, defaultKubernetesEndpointSlice())

	_, err := r.reconcileBarbican(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(getProjectedBarbican(t, r.Client, cp).Spec.TargetClusterRef).
		To(Equal(&commonv1.TargetClusterRefSpec{Name: "remote-a"}))

	unplaced := barbicanControlPlane()
	r2 := newBarbicanTestReconciler(t, unplaced)
	_, err = r2.reconcileBarbican(context.Background(), unplaced)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedBarbican(t, r2.Client, unplaced).Spec.TargetClusterRef).To(BeNil(),
		"a service that names no cluster must project no ref at all")
}

// TestBarbicanKeystoneEndpoint_FollowsThePlacement pins the endpoint policy:
// Barbican validates tokens against Keystone itself, so it gets the in-cluster
// Service DNS name exactly while the two services share a cluster, and the public
// URL as soon as they do not — that name resolves nowhere else.
func TestBarbicanKeystoneEndpoint_FollowsThePlacement(t *testing.T) {
	const (
		inCluster = "http://cp-keystone.identity.svc:5000/v3"
		public    = "https://keystone.example.com/v3"
	)
	for _, tc := range []struct {
		name               string
		barbican, keystone *commonv1.TargetClusterRefSpec
		want               string
	}{
		{name: "both co-located", want: inCluster},
		{
			name:     "both on the same cluster",
			barbican: &commonv1.TargetClusterRefSpec{Name: "remote-a"},
			keystone: &commonv1.TargetClusterRefSpec{Name: "remote-a"},
			want:     inCluster,
		},
		{
			name:     "Barbican placed, Keystone at home",
			barbican: &commonv1.TargetClusterRefSpec{Name: "remote-a"},
			want:     public,
		},
		{
			name:     "Keystone placed, Barbican at home",
			keystone: &commonv1.TargetClusterRefSpec{Name: "remote-a"},
			want:     public,
		},
		{
			name:     "different clusters",
			barbican: &commonv1.TargetClusterRefSpec{Name: "remote-a"},
			keystone: &commonv1.TargetClusterRefSpec{Name: "remote-b"},
			want:     public,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := barbicanControlPlane()
			cp.Spec.Services.Keystone.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{Name: "identity"}
			cp.Spec.Services.Keystone.PublicEndpoint = public
			cp.Spec.Services.Keystone.TargetClusterRef = tc.keystone
			cp.Spec.Services.Barbican.TargetClusterRef = tc.barbican

			g.Expect(barbicanKeystoneEndpoint(cp)).To(Equal(tc.want))
		})
	}
}

// --- the credential mirror of a placed service ---

// TestReconcileBarbican_MirrorsRegistrationCredentialsToTheTarget covers the
// reason the mirror exists: the registration delivers its consumer Secret at home,
// and a Barbican running on another cluster reads it there — from an
// ExternalSecret of the same name, over the same OpenBao path.
func TestReconcileBarbican_MirrorsRegistrationCredentialsToTheTarget(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cp := placedBarbicanControlPlane("remote-a")

	instance := availableBarbicanOpenBaoCluster(cp)
	instance.Labels = remoteChildLabels(cp)
	r, remote := splitBarbicanReconciler(t, cp, instance, defaultKubernetesEndpointSlice())

	_, err := r.reconcileBarbican(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	var mirror esov1.ExternalSecret
	g.Expect(remote.Get(ctx, types.NamespacedName{
		Name: "cp-barbican-credentials", Namespace: "key-manager",
	}, &mirror)).To(Succeed())
	g.Expect(mirror.Spec.SecretStoreRef.Name).To(Equal(esoTenantStoreName))
	g.Expect(mirror.Spec.SecretStoreRef.Kind).To(Equal(string(commonv1.SecretStoreKindNamespaced)))
	g.Expect(mirror.Spec.Target.Name).To(Equal("cp-barbican-credentials"))
	for _, d := range mirror.Spec.Data {
		g.Expect(d.RemoteRef.Key).To(Equal("openstack/keystone/key-manager/cp-barbican/service-accounts/credentials"))
	}
	// No owner reference crosses a cluster boundary, so the labels are the whole of
	// the mirror's identity — and what the teardown sweep selects on.
	g.Expect(mirror.Labels).To(HaveKeyWithValue(controlPlaneNameLabel, "cp"))
	g.Expect(mirror.Labels).To(HaveKeyWithValue(controlPlaneNamespaceLabel, "default"))
	g.Expect(mirror.OwnerReferences).To(BeEmpty())
}

// TestReconcileBarbican_NoMirrorForACoLocatedService is the other half: the
// registration's own delivery already lands in a co-located service's namespace,
// so no second ExternalSecret is written for it.
func TestReconcileBarbican_NoMirrorForACoLocatedService(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanControlPlane()
	r := newBarbicanTestReconciler(t, cp)

	_, err := r.reconcileBarbican(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(r.Get(context.Background(), types.NamespacedName{
		Name: "cp-barbican-credentials", Namespace: "default",
	}, &esov1.ExternalSecret{})).NotTo(Succeed(),
		"a co-located service must get no mirror at all")
}

// TestReconcileBarbican_MirrorHoldsOnANotReadyTargetStore covers the store gate on
// the target cluster: an ExternalSecret written against a store that is not ready
// never syncs, so the projection waits and names the store, the namespace, and the
// cluster it is missing on. The cluster that does not resolve at all is covered by
// TestReconcileBarbican_UnresolvableTargetParksTheEnsemble.
func TestReconcileBarbican_MirrorHoldsOnANotReadyTargetStore(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	s := barbicanTestScheme(t)
	cp := placedBarbicanControlPlane("remote-a")

	// The store exists at home but not on the target, which is the cluster the
	// mirror is materialized on.
	remote := fake.NewClientBuilder().WithScheme(s).Build()
	r := &ControlPlaneReconciler{
		Client: fake.NewClientBuilder().WithScheme(s).
			WithObjects(cp, readyBarbicanRegistration(cp),
				readyTenantSecretStore(esoTenantStoreName, cp.BarbicanNamespace(), "", "")).
			WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}, &barbicanv1alpha1.Barbican{},
				&c5c3v1alpha1.KeystoneService{}).Build(),
		Scheme:   s,
		Resolver: &childrenResolver{children: remote},
	}

	res, err := r.reconcileBarbican(ctx, cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeBarbicanReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonServiceAccountStoreNotReady))
	g.Expect(cond.Message).To(ContainSubstring(esoTenantStoreName))
	g.Expect(cond.Message).To(ContainSubstring(`namespace "key-manager"`))
	g.Expect(cond.Message).To(ContainSubstring("target cluster"))

	g.Expect(remote.Get(ctx, types.NamespacedName{
		Name: "cp-barbican-credentials", Namespace: "key-manager",
	}, &esov1.ExternalSecret{})).NotTo(Succeed(), "nothing may be written against a store that is not ready")
	var instances openbaov1alpha1.OpenBaoClusterList
	g.Expect(remote.List(ctx, &instances)).To(Succeed())
	g.Expect(instances.Items).To(BeEmpty(), "the gate holds ahead of the ensemble too")
}

// TestReconcileBarbican_MirrorStoreLookupFailurePropagates covers the store read
// that fails outright, as opposed to reporting not-ready: it is wrapped with what
// was being checked and returned, so the reconcile retries with backoff.
func TestReconcileBarbican_MirrorStoreLookupFailurePropagates(t *testing.T) {
	g := NewGomegaWithT(t)
	s := barbicanTestScheme(t)
	cp := placedBarbicanControlPlane("remote-a")

	remote := fake.NewClientBuilder().WithScheme(s).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*esov1.SecretStore); ok {
					return apierrors.NewInternalError(errors.New("the target apiserver is unavailable"))
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	r := &ControlPlaneReconciler{
		Client: fake.NewClientBuilder().WithScheme(s).WithObjects(cp, readyBarbicanRegistration(cp)).
			WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}, &barbicanv1alpha1.Barbican{},
				&c5c3v1alpha1.KeystoneService{}).Build(),
		Scheme:   s,
		Resolver: &childrenResolver{children: remote},
	}

	_, err := r.reconcileBarbican(context.Background(), cp)

	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring(`in namespace "key-manager"`))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeBarbicanReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonServiceRegistrationError))
}
