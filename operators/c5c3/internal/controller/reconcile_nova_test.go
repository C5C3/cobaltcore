// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the Nova sub-reconciler: the naming helpers in reconcile_nova.go,
// the catalog URL they feed (novaCatalogURL), the projected Nova child, and the
// NovaReady condition the projection drives.
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
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
)

// novaBusSecretName is the brownfield Secret the fixtures declare the shared bus
// through, and novaBusURL the transport URL it carries.
const (
	novaBusSecretName = "bus-url"
	novaBusURL        = "rabbit://u:p@bus:5672/"
)

// novaTestScheme registers c5c3, client-go, nova, and external-secrets types
// (the projection ensures two DB-credential ExternalSecrets and the metadata
// generator pair).
func novaTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("adding client-go scheme: %v", err)
	}
	if err := c5c3v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding c5c3 scheme: %v", err)
	}
	if err := novav1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding nova scheme: %v", err)
	}
	if err := esov1.AddToScheme(s); err != nil {
		t.Fatalf("adding external-secrets scheme: %v", err)
	}
	if err := esgenv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding external-secrets generators scheme: %v", err)
	}
	return s
}

// novaControlPlane builds a ControlPlane running the compute service co-located
// in the ControlPlane's own namespace, with the two gates reconcileNova reads off
// the ControlPlane itself True: KeystoneReady and PlacementReady. The third gate
// is the projected KeystoneService child, which newNovaTestReconciler seeds Ready
// (see withReadyNovaRegistration), and the fourth is the shared bus, declared
// brownfield here and seeded by withNovaBusSecret.
//
// The network and image services are declared beside the compute one, the shape
// a plane that boots instances has. Block storage and the key manager are not:
// they are the two optional client sections, so leaving them out keeps the
// fixture on the off branch of both.
func novaControlPlane() *c5c3v1alpha1.ControlPlane {
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
					SecretRef: &commonv1.SecretRefSpec{Name: novaBusSecretName},
				},
			},
			Services: c5c3v1alpha1.ServicesSpec{
				Keystone:  &c5c3v1alpha1.ServiceKeystoneSpec{},
				Placement: &c5c3v1alpha1.ServicePlacementSpec{},
				Glance:    &c5c3v1alpha1.ServiceGlanceSpec{},
				Neutron: &c5c3v1alpha1.ServiceNeutronSpec{
					OVN: c5c3v1alpha1.NeutronOVNSpec{
						CentralRef: c5c3v1alpha1.NeutronOVNCentralRef{Name: "ovn"},
					},
				},
				Nova: &c5c3v1alpha1.ServiceNovaSpec{},
			},
			KORC: c5c3v1alpha1.KORCSpec{
				AdminCredential: c5c3v1alpha1.AdminCredentialSpec{
					PasswordSecretRef: commonv1.SecretRefSpec{Name: "keystone-admin"},
				},
			},
		},
	}
	for _, conditionType := range []string{conditionTypeKeystoneReady, conditionTypePlacementReady} {
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionType,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: 1,
			Reason:             conditionType,
			Message:            "ready",
		})
	}
	return cp
}

// markRegistrationConverged marks ks as a KeystoneService whose controller has
// finished: account provisioned, catalog registered, aggregate Ready. An
// account-only registration reports the catalog condition too, so one helper
// converges both shapes.
func markRegistrationConverged(ks *c5c3v1alpha1.KeystoneService) *c5c3v1alpha1.KeystoneService {
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

// readyNovaRegistration builds the KeystoneService child the Nova projection
// gates on, converged. A child in a dedicated namespace carries the ownership
// labels, so the projection re-applies it instead of refusing to adopt a
// same-named foreign CR.
func readyNovaRegistration(cp *c5c3v1alpha1.ControlPlane) *c5c3v1alpha1.KeystoneService {
	ks := desiredNovaRegistration(cp)
	if ks.Namespace != cp.Namespace {
		stampControlPlaneChildLabels(ks, cp)
	}
	return markRegistrationConverged(ks)
}

// readyNeutronNovaNotifierRegistration builds the converged account-only
// KeystoneService child for the user neutron notifies nova as.
func readyNeutronNovaNotifierRegistration(cp *c5c3v1alpha1.ControlPlane) *c5c3v1alpha1.KeystoneService {
	ks := desiredNeutronNovaNotifierRegistration(cp)
	if ks.Namespace != cp.Namespace {
		stampControlPlaneChildLabels(ks, cp)
	}
	return markRegistrationConverged(ks)
}

// TestNovaEndpointURL pins the in-cluster address the catalog's internal row is
// built from: the projected API Service by naming convention, on the nova
// operator's port, in the namespace the service is assigned to.
func TestNovaEndpointURL(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := novaControlPlane()
	g.Expect(novaEndpointURL(cp)).To(Equal("http://cp-nova.default.svc:8774"))

	cp.Spec.Services.Nova.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{Name: "compute"}
	g.Expect(novaEndpointURL(cp)).To(Equal("http://cp-nova.compute.svc:8774"),
		"a placed service is reached in the namespace it was assigned")
}

// TestNovaCatalogURL walks the origin precedence of the compute catalog row and
// pins the "/v2.1" suffix on each outcome: an explicit publicEndpoint wins, then
// the API gateway hostname, then the in-cluster URL. The suffix is appended
// exactly once whichever origin wins, so no client ever resolves a doubled
// "/v2.1".
func TestNovaCatalogURL(t *testing.T) {
	for name, tc := range map[string]struct {
		publicEndpoint string
		gateway        *commonv1.GatewaySpec
		want           string
	}{
		"an explicit publicEndpoint wins, port and all": {
			publicEndpoint: "https://nova.example.com:8443",
			gateway:        &commonv1.GatewaySpec{Hostname: "nova.example.com"},
			want:           "https://nova.example.com:8443/v2.1",
		},
		"a gateway alone is advertised on the default https port": {
			gateway: &commonv1.GatewaySpec{Hostname: "nova.example.com"},
			want:    "https://nova.example.com/v2.1",
		},
		"without external exposure the in-cluster URL is registered": {
			want: "http://cp-nova.default.svc:8774/v2.1",
		},
		// The webhook tolerates a single trailing slash on the publicEndpoint, so
		// the origin has to be normalized before the version prefix is joined: an
		// unnormalized join registers "//v2.1", which every client resolves to a
		// path nova's routes do not map.
		"a trailing slash on the publicEndpoint is joined once": {
			publicEndpoint: "https://nova.example.com/",
			want:           "https://nova.example.com/v2.1",
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := novaControlPlane()
			cp.Spec.Services.Nova.PublicEndpoint = tc.publicEndpoint
			cp.Spec.Services.Nova.Gateway = tc.gateway

			got := novaCatalogURL(cp)

			g.Expect(got).To(Equal(tc.want))
			g.Expect(strings.Count(got, "/v2.1")).To(Equal(1), "the version prefix is appended exactly once")
		})
	}
}

// TestNovaCatalogURL_MetadataGatewayIsNoCatalogRow pins which gateway the
// compute row reads. The metadata API and the console proxy take hostnames of
// their own, dialed by a metadata agent and a browser rather than by an API
// client, so neither may become the address the catalog hands every consumer.
func TestNovaCatalogURL_MetadataGatewayIsNoCatalogRow(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	cp.Spec.Services.Nova.MetadataGateway = &commonv1.GatewaySpec{Hostname: "nova-metadata.example.com"}
	cp.Spec.Services.Nova.ConsoleProxy = &c5c3v1alpha1.ServiceNovaConsoleProxySpec{
		Gateway: &commonv1.GatewaySpec{Hostname: "nova-console.example.com"},
	}

	g.Expect(novaCatalogURL(cp)).To(Equal("http://cp-nova.default.svc:8774/v2.1"),
		"only the API gateway decides the catalog row")
}

// notReadyNovaDBCredES builds one of the two Nova DB-credential ExternalSecrets
// with NO Ready condition, so WaitForExternalSecret reports not-Ready and the
// Dynamic readiness gate engages. Seeding one explicitly is what keeps
// withReadyNovaDBCreds from substituting a Ready one.
func notReadyNovaDBCredES(cp *c5c3v1alpha1.ControlPlane, target dbCredentialTarget) *esov1.ExternalSecret {
	es := dbCredentialGeneratorExternalSecret(target)
	// Stamped as this ControlPlane's child so the cross-namespace projection path
	// re-applies it instead of refusing to adopt a same-named foreign object.
	stampControlPlaneChildLabels(es, cp)
	return es
}

// readyNovaDBCredES builds a Ready Nova DB-credential ExternalSecret at one
// target's derived name/namespace (Dynamic default shape).
func readyNovaDBCredES(cp *c5c3v1alpha1.ControlPlane, target dbCredentialTarget) *esov1.ExternalSecret {
	es := notReadyNovaDBCredES(cp, target)
	es.Status = esov1.ExternalSecretStatus{
		Conditions: []esov1.ExternalSecretStatusCondition{
			{Type: esov1.ExternalSecretReady, Status: corev1.ConditionTrue},
		},
	}
	return es
}

// materialisedNovaDBCredSecret builds the Secret an ESO sync of one
// generator-backed ExternalSecret would materialise: an ENGINE-ISSUED username
// (the OpenBao mysql-database-plugin prefix) plus its password. The Dynamic gate
// checks the username, not just the ExternalSecret's Ready condition.
func materialisedNovaDBCredSecret(target dbCredentialTarget) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: target.secretName, Namespace: target.namespace},
		Data: map[string][]byte{
			"username": []byte(engineIssuedUsernamePrefix + "kubernetes-nova-abc123-1750000000"),
			"password": []byte("engine-issued-password"),
		},
	}
}

// novaDBCredentialTargets returns the two credential chains the compute service
// takes, API first, the order reconcileNova ensures them in.
func novaDBCredentialTargets(cp *c5c3v1alpha1.ControlPlane) []dbCredentialTarget {
	return []dbCredentialTarget{novaAPIDBCredentialTarget(cp), novaCellDBCredentialTarget(cp)}
}

// withReadyNovaDBCreds seeds a Ready ExternalSecret AND the engine-issued Secret
// an ESO sync of it would materialise for BOTH credential chains, unless the test
// seeded one of them explicitly.
//
// The Dynamic-default projection gates the child on both chains having synced and
// on the Secrets behind them carrying engine-issued usernames, and a fake client
// never runs ESO, so without this every projection test would stall on the first
// gate and assert against a child that was deliberately not projected.
func withReadyNovaDBCreds(objs []client.Object) []client.Object {
	cp := controlPlaneIn(objs)
	// Only the Dynamic path has a readiness gate; a Static ControlPlane projects
	// KV-backed ExternalSecrets of a different shape and must be left to build them.
	if cp == nil || cp.Spec.Services.Nova == nil || !novaDBCredentialsDynamicEnabled(cp) {
		return objs
	}
	for _, target := range novaDBCredentialTargets(cp) {
		seeded := false
		for _, o := range objs {
			if _, ok := o.(*esov1.ExternalSecret); ok &&
				o.GetName() == target.secretName && o.GetNamespace() == target.namespace {
				seeded = true
			}
		}
		if !seeded {
			objs = append(objs, readyNovaDBCredES(cp, target), materialisedNovaDBCredSecret(target))
		}
	}
	return objs
}

// withReadyNovaRegistration seeds the converged KeystoneService child the
// projection gates on, unless the test seeded one of its own, which is what the
// gate and readiness-fold tests do.
func withReadyNovaRegistration(objs []client.Object) []client.Object {
	cp := controlPlaneIn(objs)
	if cp == nil || cp.Spec.Services.Nova == nil {
		return objs
	}
	for _, o := range objs {
		if _, ok := o.(*c5c3v1alpha1.KeystoneService); ok {
			return objs
		}
	}
	return append(objs, readyNovaRegistration(cp))
}

// withNovaBusSecret seeds the brownfield transport-URL Secret the fixture's
// spec.infrastructure.messaging names, in the ControlPlane's OWN namespace where
// the bus is declared and read. Without it every projection test would halt on
// the messaging leg before reaching the child.
func withNovaBusSecret(objs []client.Object) []client.Object {
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
		Data:       map[string][]byte{commonv1.DefaultTransportURLSecretKey: []byte(novaBusURL)},
	})
}

// withNovaTenantStore seeds the per-tenant SecretStore in the compute service's
// namespace when that service is PLACED, unless the test seeded one of its own.
// The credential mirror a placed service gets is gated on that store, so without
// it every placed test would hold at SecretStoreNotReady.
func withNovaTenantStore(objs []client.Object) []client.Object {
	cp := controlPlaneIn(objs)
	if cp == nil || targetClusterRefForNamespace(cp, cp.NovaNamespace()) == nil {
		return objs
	}
	for _, o := range objs {
		if _, ok := o.(*esov1.SecretStore); ok {
			return objs
		}
	}
	return append(objs, readyTenantSecretStore(esoTenantStoreName, cp.NovaNamespace(), "", ""))
}

func newNovaTestReconciler(t *testing.T, objs ...client.Object) *ControlPlaneReconciler {
	t.Helper()
	s := novaTestScheme(t)
	seeded := withNovaTenantStore(withReadyNovaRegistration(withNovaBusSecret(withReadyNovaDBCreds(objs))))
	cb := fake.NewClientBuilder().WithScheme(s).WithObjects(seeded...).
		WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}, &novav1alpha1.Nova{},
			&c5c3v1alpha1.KeystoneService{})
	return &ControlPlaneReconciler{Client: cb.Build(), Scheme: s}
}

func getProjectedNova(t *testing.T, c client.Client, cp *c5c3v1alpha1.ControlPlane) *novav1alpha1.Nova {
	t.Helper()
	nv := &novav1alpha1.Nova{}
	key := types.NamespacedName{Name: novaName(cp), Namespace: cp.NovaNamespace()}
	if err := c.Get(context.Background(), key, nv); err != nil {
		t.Fatalf("getting projected Nova %s: %v", key, err)
	}
	return nv
}

// novaRegistration builds the KeystoneService child at the projected
// name/namespace carrying the given conditions, for the tests that drive the gate
// and the readiness fold from a child that has not converged.
func novaRegistration(cp *c5c3v1alpha1.ControlPlane, conds ...metav1.Condition) *c5c3v1alpha1.KeystoneService {
	ks := &c5c3v1alpha1.KeystoneService{
		ObjectMeta: metav1.ObjectMeta{Name: novaName(cp), Namespace: cp.NovaNamespace()},
	}
	if ks.Namespace != cp.Namespace {
		stampControlPlaneChildLabels(ks, cp)
	}
	for _, cond := range conds {
		conditions.SetCondition(&ks.Status.Conditions, cond)
	}
	return ks
}

// convergeNovaChild stands in for the nova-operator: it stamps
// status.observedGeneration on the projected Nova child and marks it Ready, the
// two halves of the verdict reconcileNova reads before it reaps the CA mirror.
func convergeNovaChild(
	t *testing.T, r *ControlPlaneReconciler, cp *c5c3v1alpha1.ControlPlane, observedGeneration int64,
) {
	t.Helper()
	nv := getProjectedNova(t, r.Client, cp)
	nv.Status.ObservedGeneration = observedGeneration
	conditions.SetCondition(&nv.Status.Conditions, metav1.Condition{
		Type:               conditionTypeReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: observedGeneration,
		Reason:             "AllReady",
		Message:            "ready",
	})
	if err := r.Client.Status().Update(context.Background(), nv); err != nil {
		t.Fatalf("converging the projected Nova: %v", err)
	}
}

// novaCondition returns the NovaReady condition the pass left on cp.
func novaCondition(t *testing.T, cp *c5c3v1alpha1.ControlPlane) *metav1.Condition {
	t.Helper()
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeNovaReady)
	if cond == nil {
		t.Fatalf("no %s condition was set", conditionTypeNovaReady)
	}
	return cond
}

// --- gates ---

func TestReconcileNova_NotManagedWhenUnset(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	cp.Spec.Services.Nova = nil
	r := newNovaTestReconciler(t, cp)

	res, err := r.reconcileNova(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := novaCondition(t, cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("NovaNotManaged"))

	var list novav1alpha1.NovaList
	g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty())
}

// TestReconcileNova_UnsetPreservesChildAndTearsDownBothDynamicGenerators covers
// the preserve-by-default branch: dropping spec.services.nova keeps the child (an
// accidental block drop must not remove the compute control plane of a cloud with
// running instances) but must NOT keep the credential minters. A retained
// VaultDynamicSecret mints a fresh MySQL user with ALL PRIVILEGES every refresh
// interval, forever, for a service the operator was told it no longer manages,
// and the compute service has two of them.
func TestReconcileNova_UnsetPreservesChildAndTearsDownBothDynamicGenerators(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	r := newNovaTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcileNova(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	for _, target := range novaDBCredentialTargets(cp) {
		g.Expect(r.Get(ctx, types.NamespacedName{Name: target.secretName, Namespace: target.namespace},
			&esgenv1alpha1.VaultDynamicSecret{})).To(Succeed(), "the generators were projected alongside the child")
	}

	// No opt-in annotation: the child is preserved.
	cp.Spec.Services.Nova = nil
	_, err = r.reconcileNova(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(getProjectedNova(t, r.Client, cp)).NotTo(BeNil(), "the child must still be preserved")
	cond := novaCondition(t, cp)
	g.Expect(cond.Reason).To(Equal("NovaNotManaged"))
	g.Expect(cond.Message).To(ContainSubstring(novaDeletionAllowedAnnotation))

	for _, target := range []dbCredentialTarget{
		novaAPIDBCredentialTarget(cp), novaCellDBCredentialTarget(cp),
	} {
		g.Expect(r.Get(ctx, types.NamespacedName{Name: target.secretName, Namespace: target.namespace},
			&esgenv1alpha1.VaultDynamicSecret{})).NotTo(Succeed(),
			"both credential minters must be torn down even though the child is preserved")
		g.Expect(r.Get(ctx, types.NamespacedName{Name: target.saName, Namespace: target.namespace},
			&corev1.ServiceAccount{})).NotTo(Succeed(), "the generators' ServiceAccounts must be torn down too")
		orphanCert := &unstructured.Unstructured{}
		orphanCert.SetGroupVersionKind(certificateGVK)
		g.Expect(r.Get(ctx, types.NamespacedName{Name: target.certName, Namespace: target.namespace},
			orphanCert)).NotTo(Succeed(), "the generators' mTLS client Certificates must be torn down too")
	}

	// The metadata shared secret is not a minter: it is generated once and read by
	// every agent, so it stays with the preserved child.
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: novaMetadataSecretName(cp), Namespace: cp.NovaNamespace(),
	}, &esgenv1alpha1.Password{})).To(Succeed(),
		"a one-shot generated value is preserved with the service it belongs to")
}

// TestReconcileNova_UnsetPreservesRegistrationCredentialsAndBusByDefault pins
// what the preserve-by-default branch must NOT sweep beside the child: the
// registration the compute service authenticates as, the two DB-credential
// ExternalSecrets (CreatePolicyOwner, so deleting one also collects the password
// Secret the preserved child reads), and the transport-URL Secret. Reusing the
// opt-in sweep here would leave the preserved child with no Keystone user, no
// database login and no bus.
func TestReconcileNova_UnsetPreservesRegistrationCredentialsAndBusByDefault(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	r := newNovaTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcileNova(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	cp.Spec.Services.Nova = nil
	_, err = r.reconcileNova(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	novaNS := cp.Namespace
	g.Expect(r.Get(ctx, types.NamespacedName{Name: novaName(cp), Namespace: novaNS},
		&c5c3v1alpha1.KeystoneService{})).To(Succeed(), "the registration must be preserved with the child")
	for _, target := range []dbCredentialTarget{
		novaAPIDBCredentialTarget(cp), novaCellDBCredentialTarget(cp),
	} {
		g.Expect(r.Get(ctx, types.NamespacedName{Name: target.secretName, Namespace: novaNS},
			&esov1.ExternalSecret{})).To(Succeed(),
			"the %s ExternalSecret owns the password Secret the preserved child reads", target.secretName)
	}
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: novaMessagingSecretName(cp), Namespace: novaNS,
	}, &corev1.Secret{})).To(Succeed(), "the preserved child still reads its transport URL")
}

// TestReconcileNova_UnsetDeletionErrorIsReturned pins that a failure in the
// opt-in sweep surfaces. Swallowing it would report NovaReady=True/NovaNotManaged
// with no requeue while a VaultDynamicSecret keeps minting MySQL users with ALL
// PRIVILEGES for a service the ControlPlane was told to remove.
func TestReconcileNova_UnsetDeletionErrorIsReturned(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	s := novaTestScheme(t)
	seeded := withNovaTenantStore(withReadyNovaRegistration(withNovaBusSecret(withReadyNovaDBCreds(
		[]client.Object{cp}))))
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(seeded...).
		WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}, &novav1alpha1.Nova{},
			&c5c3v1alpha1.KeystoneService{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if _, isGenerator := obj.(*esgenv1alpha1.VaultDynamicSecret); isGenerator {
					return apierrors.NewInternalError(errors.New("etcd is unavailable"))
				}
				return cl.Delete(ctx, obj, opts...)
			},
		}).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}
	ctx := context.Background()

	_, err := r.reconcileNova(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	cp.Spec.Services.Nova = nil
	cp.Annotations = map[string]string{novaDeletionAllowedAnnotation: "true"}
	_, err = r.reconcileNova(ctx, cp)

	g.Expect(err).To(MatchError(ContainSubstring("etcd is unavailable")))
	api := novaAPIDBCredentialTarget(cp)
	g.Expect(r.Get(ctx, types.NamespacedName{Name: api.secretName, Namespace: api.namespace},
		&esgenv1alpha1.VaultDynamicSecret{})).To(Succeed(),
		"the generator the sweep could not delete is still there to be retried")
}

// TestReconcileNova_UnsetDeletesChildWithOptIn verifies the opt-in deletion sweep
// removes the child AND every object that follows it: both credential chains
// whole, the generated metadata pair, the two messaging Secrets, and the
// registration that unregisters the compute service from the catalog.
func TestReconcileNova_UnsetDeletesChildWithOptIn(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	cp.Spec.Infrastructure.Messaging.TLS = &commonv1.MessagingTLSSpec{
		CABundleSecretRef: commonv1.SecretRefSpec{Name: "bus-ca", Key: "ca.crt"},
	}
	busCA := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "bus-ca", Namespace: cp.Namespace},
		Data:       map[string][]byte{"ca.crt": []byte("ca-bundle")},
	}
	r := newNovaTestReconciler(t, cp, busCA)
	ctx := context.Background()

	_, err := r.reconcileNova(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	for _, name := range []string{novaMessagingSecretName(cp), novaMessagingCASecretName(cp)} {
		g.Expect(r.Get(ctx, types.NamespacedName{Name: name, Namespace: cp.NovaNamespace()},
			&corev1.Secret{})).To(Succeed(), "the bus delivery was written alongside the child")
	}

	cp.Spec.Services.Nova = nil
	cp.Annotations = map[string]string{novaDeletionAllowedAnnotation: "true"}

	_, err = r.reconcileNova(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	var list novav1alpha1.NovaList
	g.Expect(r.Client.List(ctx, &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty(), "the opt-in annotation must delete the owned child")

	for _, target := range novaDBCredentialTargets(cp) {
		key := types.NamespacedName{Name: target.secretName, Namespace: target.namespace}
		g.Expect(r.Get(ctx, key, &esov1.ExternalSecret{})).NotTo(Succeed(),
			"the DB-credential ExternalSecrets must be swept too")
		g.Expect(r.Get(ctx, key, &esgenv1alpha1.VaultDynamicSecret{})).NotTo(Succeed(),
			"the VaultDynamicSecret generators must be swept too")
		g.Expect(r.Get(ctx, types.NamespacedName{Name: target.saName, Namespace: target.namespace},
			&corev1.ServiceAccount{})).NotTo(Succeed(), "the generators' ServiceAccounts must be swept too")
		sweptCert := &unstructured.Unstructured{}
		sweptCert.SetGroupVersionKind(certificateGVK)
		g.Expect(r.Get(ctx, types.NamespacedName{Name: target.certName, Namespace: target.namespace},
			sweptCert)).NotTo(Succeed(), "the mTLS client Certificates must be swept too")
	}

	metadataKey := types.NamespacedName{Name: novaMetadataSecretName(cp), Namespace: cp.NovaNamespace()}
	g.Expect(r.Get(ctx, metadataKey, &esov1.ExternalSecret{})).NotTo(Succeed(),
		"the metadata ExternalSecret must be swept too")
	g.Expect(r.Get(ctx, metadataKey, &esgenv1alpha1.Password{})).NotTo(Succeed(),
		"the metadata Password generator must be swept too")

	for _, name := range []string{novaMessagingSecretName(cp), novaMessagingCASecretName(cp)} {
		g.Expect(r.Get(ctx, types.NamespacedName{Name: name, Namespace: cp.NovaNamespace()},
			&corev1.Secret{})).NotTo(Succeed(), "the bus delivery must be swept with the child")
	}

	var registrations c5c3v1alpha1.KeystoneServiceList
	g.Expect(r.Client.List(ctx, &registrations)).To(Succeed())
	g.Expect(registrations.Items).To(BeEmpty(), "the opt-in annotation must delete the owned registration")
}

// TestReconcileNova_UnsetPreservesForeignObjects proves the deletion sweep is
// ownership-checked across every object it names: a Nova child, a same-named
// KeystoneService, a messaging Secret and, most importantly, the FIXED-name
// nova-api-db-creds ServiceAccount that this ControlPlane does NOT own all
// survive an opt-in teardown. The ServiceAccount name is not CR-derived, so in a
// shared service namespace it is exactly the object a collision would hand to
// somebody else.
func TestReconcileNova_UnsetPreservesForeignObjects(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	cp.Spec.Services.Nova = nil
	cp.Annotations = map[string]string{novaDeletionAllowedAnnotation: "true"}

	foreign := []client.Object{
		&novav1alpha1.Nova{
			ObjectMeta: metav1.ObjectMeta{Name: novaName(cp), Namespace: cp.NovaNamespace()},
		},
		&corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{
				Name: novaAPIDBCredentialServiceAccountName, Namespace: cp.NovaNamespace(),
			},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: novaMessagingSecretName(cp), Namespace: cp.NovaNamespace()},
		},
		&esgenv1alpha1.Password{
			ObjectMeta: metav1.ObjectMeta{Name: novaMetadataSecretName(cp), Namespace: cp.NovaNamespace()},
		},
		&c5c3v1alpha1.KeystoneService{
			ObjectMeta: metav1.ObjectMeta{Name: novaName(cp), Namespace: cp.NovaNamespace()},
			Spec: c5c3v1alpha1.KeystoneServiceSpec{
				ControlPlaneRef: c5c3v1alpha1.ControlPlaneRefSpec{Name: "someone-else"},
			},
		},
	}
	r := newNovaTestReconciler(t, append([]client.Object{cp}, foreign...)...)
	ctx := context.Background()

	_, err := r.reconcileNova(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	for _, obj := range foreign {
		live := obj.DeepCopyObject().(client.Object)
		g.Expect(r.Get(ctx, client.ObjectKeyFromObject(obj), live)).To(Succeed(),
			"a %T we do not own must never be deleted", obj)
	}
}

// TestReconcileNova_UnsetDeletionToleratesAlreadyGoneObjects covers the
// partially-cleaned state a repeated teardown reaches: every object the sweep
// names may already be gone (a previous pass removed it, or it was never
// projected), and each delete tolerates NotFound so the reconcile converges
// instead of failing on the first missing object.
func TestReconcileNova_UnsetDeletionToleratesAlreadyGoneObjects(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	cp.Spec.Services.Nova = nil
	cp.Annotations = map[string]string{novaDeletionAllowedAnnotation: "true"}
	r := newNovaTestReconciler(t, cp)
	ctx := context.Background()

	// Nothing was ever projected, so every named object is already absent.
	res, err := r.reconcileNova(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())

	// And a second pass over the same empty state stays clean.
	_, err = r.reconcileNova(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	cond := novaCondition(t, cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("NovaNotManaged"))
}

// TestReconcileNova_UnresolvableBackingServicesRequeue is the nil-safety
// fail-safe: a webhook-bypassed CR that dropped spec.infrastructure, or only the
// bus block inside it, has nothing to project and no bus to deliver, so the
// projection requeues instead of dereferencing nil, and writes no condition it
// would then have to retract.
func TestReconcileNova_UnresolvableBackingServicesRequeue(t *testing.T) {
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
			cp := novaControlPlane()
			tt.apply(cp)
			r := newNovaTestReconciler(t, cp)

			res, err := r.reconcileNova(context.Background(), cp)

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(res.RequeueAfter).To(Equal(infraRequeueAfter))
			g.Expect(conditions.GetCondition(cp.Status.Conditions, conditionTypeNovaReady)).To(BeNil(),
				"the fail-safe must not write a condition it cannot substantiate")

			var list novav1alpha1.NovaList
			g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
			g.Expect(list.Items).To(BeEmpty(), "nothing may be projected against unresolvable backing services")
		})
	}
}

func TestReconcileNova_GatedOnKeystoneReady(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditionTypeKeystoneReady,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: 1,
		Reason:             "WaitingForKeystone",
		Message:            "not ready",
	})
	r := newNovaTestReconciler(t, cp)

	res, err := r.reconcileNova(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(keystoneInfraGateRequeueAfter))
	cond := novaCondition(t, cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForKeystone"))

	var list novav1alpha1.NovaList
	g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty())
}

// TestReconcileNova_GatedOnPlacementReady pins the gate Nova takes that its
// siblings do not. Every instance boot claims its resources in Placement before
// it lands on a host, so a compute service projected ahead of a working placement
// service accepts requests it cannot serve. A ControlPlane that manages no
// placement service reports the condition True under a not-managed reason, and
// that passes: the gate reads the condition, never the block.
func TestReconcileNova_GatedOnPlacementReady(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditionTypePlacementReady,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: 1,
		Reason:             "WaitingForPlacement",
		Message:            "not ready",
	})
	r := newNovaTestReconciler(t, cp)

	res, err := r.reconcileNova(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(keystoneInfraGateRequeueAfter))
	cond := novaCondition(t, cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForPlacement"))
	g.Expect(cond.Message).To(Equal("PlacementReady is not True; Nova projection deferred"))

	var list novav1alpha1.NovaList
	g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty())

	// A plane that runs no placement service of its own reports the condition True
	// with its own reason, which the gate lets through.
	notManaged := novaControlPlane()
	conditions.SetCondition(&notManaged.Status.Conditions, metav1.Condition{
		Type:               conditionTypePlacementReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: 1,
		Reason:             "PlacementNotManaged",
		Message:            "spec.services.placement is unset",
	})
	r2 := newNovaTestReconciler(t, notManaged)

	_, err = r2.reconcileNova(context.Background(), notManaged)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedNova(t, r2.Client, notManaged)).NotTo(BeNil(),
		"a True PlacementReady passes the gate whatever its reason")
}

// TestReconcileNova_MessagingWaitHalts covers the managed bus whose
// RabbitmqCluster is not there yet: the delivery is a wait, and until it lands
// neither the registration nor the child may be written, because a Nova with no
// transport URL reaches neither its conductor nor a single compute node.
func TestReconcileNova_MessagingWaitHalts(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	cp.Spec.Infrastructure.Messaging = &commonv1.MessagingSpec{
		ClusterRef: &corev1.LocalObjectReference{Name: "openstack-rabbitmq"},
	}
	// Built without the seeded registration the other tests get, so an empty
	// KeystoneService list is evidence the leg never ran rather than a fixture.
	s := novaTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(withReadyNovaDBCreds([]client.Object{cp})...).
		WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}, &novav1alpha1.Nova{},
			&c5c3v1alpha1.KeystoneService{}).
		Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	res, err := r.reconcileNova(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred(), "a bus that has not been created yet is a wait, not a failure")
	g.Expect(res.RequeueAfter).To(Equal(infraRequeueAfter))
	cond := novaCondition(t, cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(messaging.ReasonWaitingForMessagingCredentials))

	var list novav1alpha1.NovaList
	g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty(), "no child may be projected without a transport URL")
	var registrations c5c3v1alpha1.KeystoneServiceList
	g.Expect(r.Client.List(context.Background(), &registrations)).To(Succeed())
	g.Expect(registrations.Items).To(BeEmpty(), "the bus gate runs before the registration is projected")
}

// TestReconcileNova_MessagingErrorSurfaces covers the bus block that named
// neither a cluster nor a Secret, which only a bypassed admission produces: it
// does not converge on its own, so it is returned as an error rather than waited
// out.
func TestReconcileNova_MessagingErrorSurfaces(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	cp.Spec.Infrastructure.Messaging = &commonv1.MessagingSpec{}
	r := newNovaTestReconciler(t, cp)

	_, err := r.reconcileNova(context.Background(), cp)

	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("resolving the shared bus transport URL"))
	cond := novaCondition(t, cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("NovaMessagingError"))
}

// TestReconcileNova_GatedOnRegistrationAccountNotReady pins the registration
// gate: while the child's AccountReady is False no Nova is projected, the child's
// own reason and message are relayed, and a Nova projected by an earlier pass is
// left running on the credentials it already has.
func TestReconcileNova_GatedOnRegistrationAccountNotReady(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	ks := novaRegistration(cp, metav1.Condition{
		Type:    conditionTypeKeystoneServiceAccountReady,
		Status:  metav1.ConditionFalse,
		Reason:  reasonServiceAccountCollision,
		Message: `user "nova" already exists in Keystone`,
	})
	existing := &novav1alpha1.Nova{
		ObjectMeta: metav1.ObjectMeta{Name: novaName(cp), Namespace: cp.NovaNamespace()},
		Spec:       novav1alpha1.NovaSpec{Region: "RegionPrevious"},
	}
	r := newNovaTestReconciler(t, cp, ks, existing)

	res, err := r.reconcileNova(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter))
	cond := novaCondition(t, cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonWaitingForServiceRegistration))
	g.Expect(cond.Message).To(ContainSubstring(reasonServiceAccountCollision))
	g.Expect(cond.Message).To(ContainSubstring(`user "nova" already exists in Keystone`))

	g.Expect(getProjectedNova(t, r.Client, cp).Spec.Region).To(Equal("RegionPrevious"),
		"the gate must write no Nova at all, leaving a previously projected one untouched")
}

// TestReconcileNova_RegistrationReadFailureSurfaces covers the read that fails
// for any reason OTHER than absence: it is an error, wrapped with what it was
// reading.
func TestReconcileNova_RegistrationReadFailureSurfaces(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	s := novaTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(withNovaBusSecret(withReadyNovaDBCreds([]client.Object{cp}))...).
		WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}, &novav1alpha1.Nova{},
			&c5c3v1alpha1.KeystoneService{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object,
				opts ...client.GetOption,
			) error {
				if _, ok := obj.(*c5c3v1alpha1.KeystoneService); ok {
					return apierrors.NewInternalError(errors.New("etcd is unavailable"))
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileNova(context.Background(), cp)

	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("reading the nova KeystoneService child:"))
	cond := novaCondition(t, cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonServiceRegistrationError))
}

// TestReconcileNova_NeverAdoptsForeignRegistration proves the registration write
// is refused rather than allowed to overwrite a same-named KeystoneService in a
// namespace the ControlPlane does not own: the refusal surfaces on NovaReady and
// the foreign CR keeps its spec.
func TestReconcileNova_NeverAdoptsForeignRegistration(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	cp.Spec.Services.Nova.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
		Name: "compute", Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
	}
	foreign := &c5c3v1alpha1.KeystoneService{
		ObjectMeta: metav1.ObjectMeta{Name: novaName(cp), Namespace: "compute"},
		Spec: c5c3v1alpha1.KeystoneServiceSpec{
			ControlPlaneRef: c5c3v1alpha1.ControlPlaneRefSpec{Name: "someone-else"},
		},
	}
	r := newNovaTestReconciler(t, cp, foreign)

	_, err := r.reconcileNova(context.Background(), cp)

	g.Expect(err).To(HaveOccurred(), "adopting a foreign registration must be refused")
	cond := novaCondition(t, cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonServiceRegistrationError))
	g.Expect(cond.Message).To(ContainSubstring("refusing to adopt pre-existing"))

	var live c5c3v1alpha1.KeystoneService
	g.Expect(r.Get(context.Background(), types.NamespacedName{
		Name: novaName(cp), Namespace: "compute",
	}, &live)).To(Succeed())
	g.Expect(live.Spec.ControlPlaneRef.Name).To(Equal("someone-else"),
		"a foreign registration must never be overwritten")
	g.Expect(live.Labels).NotTo(HaveKey(controlPlaneNameLabel))
}

// --- the two credential chains ---

// TestReconcileNova_APIDBCredentialErrorSurfacesAndReturns pins the error leg of
// the first credential ensure: in a service namespace the ControlPlane does not
// own, a pre-existing foreign ExternalSecret at the derived name is never
// adopted, and the refusal is both reported as NovaAPIDBCredentialError and
// returned to the pipeline rather than swallowed into a wait.
func TestReconcileNova_APIDBCredentialErrorSurfacesAndReturns(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	cp.Spec.Services.Nova.DatabaseCredentialsMode = commonv1.CredentialsModeStatic
	cp.Spec.Services.Nova.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
		Name: "compute", Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleExternal,
	}
	// Somebody else's ExternalSecret under the name our Static branch projects.
	foreign := &esov1.ExternalSecret{ObjectMeta: metav1.ObjectMeta{
		Name: novaAPIDBCredentialSecretName(cp), Namespace: "compute",
	}}
	r := newNovaTestReconciler(t, cp, foreign)

	res, err := r.reconcileNova(context.Background(), cp)

	g.Expect(err).To(HaveOccurred(), "adopting a foreign ExternalSecret must be refused")
	g.Expect(res.IsZero()).To(BeTrue())
	cond := novaCondition(t, cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("NovaAPIDBCredentialError"))

	var list novav1alpha1.NovaList
	g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty(), "no child may be projected against a credential that was never ensured")
}

// TestReconcileNova_CellDBCredentialErrorSurfacesAndReturns is the twin on the
// second chain: the cell schema's credential fails on its own reason, so an
// operator reading the condition knows which of the two to look at.
func TestReconcileNova_CellDBCredentialErrorSurfacesAndReturns(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	cp.Spec.Services.Nova.DatabaseCredentialsMode = commonv1.CredentialsModeStatic
	cp.Spec.Services.Nova.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
		Name: "compute", Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleExternal,
	}
	foreign := &esov1.ExternalSecret{ObjectMeta: metav1.ObjectMeta{
		Name: novaCellDBCredentialSecretName(cp), Namespace: "compute",
	}}
	r := newNovaTestReconciler(t, cp, foreign)

	_, err := r.reconcileNova(context.Background(), cp)

	g.Expect(err).To(HaveOccurred())
	cond := novaCondition(t, cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("NovaCellDBCredentialError"))
}

// TestReconcileNova_DynamicAPICredentialNotReady_DefersProjection is the gate
// that keeps the Dynamic default from failing OPEN. The engine roles behind the
// generators are provisioned by a MANUAL onboarding step
// (setup-database-tenant.sh), while the operator rolls out on its own, so a
// ControlPlane can reach here with no role to mint against. Until the API
// credential materialises no Nova child may be projected at all.
func TestReconcileNova_DynamicAPICredentialNotReady_DefersProjection(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	r := newNovaTestReconciler(t, cp, notReadyNovaDBCredES(cp, novaAPIDBCredentialTarget(cp)))

	res, err := r.reconcileNova(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(dbCredentialsRequeueAfter))

	cond := novaCondition(t, cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForNovaAPIDBCredential"))
	g.Expect(cond.Message).To(ContainSubstring(novaAPIDBDynamicCredsPathFor(cp)),
		"the condition must name the engine path an operator has to onboard")

	var list novav1alpha1.NovaList
	g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty(), "no Nova child may be projected before both credentials land")
}

// TestReconcileNova_DynamicCellCredentialNotReady_DefersProjection is the twin on
// the second chain: the API credential having landed is not enough, because a
// conductor without a cell login serves no instance.
func TestReconcileNova_DynamicCellCredentialNotReady_DefersProjection(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	r := newNovaTestReconciler(t, cp, notReadyNovaDBCredES(cp, novaCellDBCredentialTarget(cp)))

	res, err := r.reconcileNova(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(dbCredentialsRequeueAfter))
	cond := novaCondition(t, cp)
	g.Expect(cond.Reason).To(Equal("WaitingForNovaCellDBCredential"))
	g.Expect(cond.Message).To(ContainSubstring(novaCellDBDynamicCredsPathFor(cp)))

	var list novav1alpha1.NovaList
	g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty())
}

// --- the metadata shared secret ---

// TestReconcileNova_MetadataSecretGenerated proves the projection runs the
// generation leg and hands the child the reference it produces.
func TestReconcileNova_MetadataSecretGenerated(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	r := newNovaTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcileNova(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	key := types.NamespacedName{Name: novaMetadataSecretName(cp), Namespace: cp.NovaNamespace()}
	g.Expect(r.Get(ctx, key, &esgenv1alpha1.Password{})).To(Succeed())
	g.Expect(r.Get(ctx, key, &esov1.ExternalSecret{})).To(Succeed())
	g.Expect(getProjectedNova(t, r.Client, cp).Spec.Metadata.SharedSecretRef).To(Equal(commonv1.SecretRefSpec{
		Name: "cp-nova-metadata-secret", Key: "shared_secret",
	}))
}

// TestReconcileNova_MetadataSecretReapedOnceTheChildConverges covers the flip to
// a ControlPlane-supplied Secret. The apply re-points the child at once, but the
// live metadata Deployment keeps sourcing its env from the generated Secret
// until the nova operator has re-rendered it, and that operator parks on the new
// Secret before its Deployment step while the Secret is absent. So the generated
// pair outlives the re-pointing pass and is reaped only once the child reports
// the generation that apply produced.
func TestReconcileNova_MetadataSecretReapedOnceTheChildConverges(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	r := newNovaTestReconciler(t, cp)
	ctx := context.Background()
	key := types.NamespacedName{Name: novaMetadataSecretName(cp), Namespace: cp.NovaNamespace()}

	_, err := r.reconcileNova(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	// Pin the child one generation behind: Ready and converged on the spec that
	// still named the generated Secret, the status the API server returns to the
	// apply that re-points it.
	nv := getProjectedNova(t, r.Client, cp)
	nv.Generation = 2
	g.Expect(r.Client.Update(ctx, nv)).To(Succeed())
	convergeNovaChild(t, r, cp, 1)

	cp.Spec.Services.Nova.MetadataSharedSecretRef = &commonv1.SecretRefSpec{
		Name: "seeded-metadata", Key: "shared_secret",
	}
	_, err = r.reconcileNova(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedNova(t, r.Client, cp).Spec.Metadata.SharedSecretRef).To(Equal(commonv1.SecretRefSpec{
		Name: "seeded-metadata", Key: "shared_secret",
	}), "the apply re-points the child on the same pass")
	g.Expect(r.Get(ctx, key, &esgenv1alpha1.Password{})).To(Succeed(),
		"the generator must outlive the reference until the child has re-rendered without it")
	g.Expect(r.Get(ctx, key, &esov1.ExternalSecret{})).To(Succeed(),
		"so must the ExternalSecret whose owner reference takes the Secret down")

	// The nova operator catches up: the child reports the generation the apply
	// produced, so no workload sources the generated Secret any more.
	convergeNovaChild(t, r, cp, 2)
	_, err = r.reconcileNova(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(apierrors.IsNotFound(r.Get(ctx, key, &esgenv1alpha1.Password{}))).To(BeTrue())
	g.Expect(apierrors.IsNotFound(r.Get(ctx, key, &esov1.ExternalSecret{}))).To(BeTrue())
}

// TestReconcileNova_MetadataSecretKeptWhileTheChildWaitsOnTheOverride covers a
// child that never becomes Ready on the supplied reference, for instance because
// the Secret it names is created later. The pass returns on the readiness wait,
// so the generated pair the live Deployment still reads is never reaped in the
// meantime.
func TestReconcileNova_MetadataSecretKeptWhileTheChildWaitsOnTheOverride(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	r := newNovaTestReconciler(t, cp)
	ctx := context.Background()
	key := types.NamespacedName{Name: novaMetadataSecretName(cp), Namespace: cp.NovaNamespace()}

	_, err := r.reconcileNova(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	cp.Spec.Services.Nova.MetadataSharedSecretRef = &commonv1.SecretRefSpec{Name: "seeded-metadata"}
	res, err := r.reconcileNova(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).NotTo(BeZero())
	g.Expect(novaCondition(t, cp).Reason).To(Equal("WaitingForNova"))
	g.Expect(r.Get(ctx, key, &esgenv1alpha1.Password{})).To(Succeed(),
		"a child that is not Ready on the new reference still runs pods reading the old one")
	g.Expect(r.Get(ctx, key, &esov1.ExternalSecret{})).To(Succeed())
}

// TestReconcileNova_MetadataSecretReapErrorSurfaces pins the failure leg of the
// reap: a delete that fails parks NovaReady on NovaMetadataSecretError and
// returns the error rather than reporting a converged override over a generator
// that is still live.
func TestReconcileNova_MetadataSecretReapErrorSurfaces(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	s := novaTestScheme(t)
	seeded := withNovaTenantStore(withReadyNovaRegistration(withNovaBusSecret(withReadyNovaDBCreds(
		[]client.Object{cp}))))
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(seeded...).
		WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}, &novav1alpha1.Nova{},
			&c5c3v1alpha1.KeystoneService{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if _, isGenerator := obj.(*esgenv1alpha1.Password); isGenerator {
					return apierrors.NewInternalError(errors.New("etcd is unavailable"))
				}
				return cl.Delete(ctx, obj, opts...)
			},
		}).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}
	ctx := context.Background()

	_, err := r.reconcileNova(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	cp.Spec.Services.Nova.MetadataSharedSecretRef = &commonv1.SecretRefSpec{Name: "seeded-metadata"}
	_, err = r.reconcileNova(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	convergeNovaChild(t, r, cp, getProjectedNova(t, r.Client, cp).Generation)

	_, err = r.reconcileNova(ctx, cp)

	g.Expect(err).To(MatchError(ContainSubstring("etcd is unavailable")))
	cond := novaCondition(t, cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("NovaMetadataSecretError"))
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: novaMetadataSecretName(cp), Namespace: cp.NovaNamespace(),
	}, &esgenv1alpha1.Password{})).To(Succeed())
}

// TestReconcileNova_MetadataSecretApplyErrorSurfaces pins the halt: a generation
// that cannot be written stops the pass before the child is projected, because a
// Nova pointed at a shared secret that will never exist answers no metadata
// request.
func TestReconcileNova_MetadataSecretApplyErrorSurfaces(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	s := novaTestScheme(t)
	seeded := withReadyNovaRegistration(withNovaBusSecret(withReadyNovaDBCreds([]client.Object{cp})))
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(seeded...).
		WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}, &novav1alpha1.Nova{},
			&c5c3v1alpha1.KeystoneService{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(ctx context.Context, cl client.WithWatch, obj runtime.ApplyConfiguration,
				opts ...client.ApplyOption,
			) error {
				if ac, ok := obj.(client.Object); ok &&
					ac.GetObjectKind().GroupVersionKind().Kind == "Password" {
					return apierrors.NewInternalError(errors.New("etcd is unavailable"))
				}
				return cl.Apply(ctx, obj, opts...)
			},
		}).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileNova(context.Background(), cp)

	g.Expect(err).To(HaveOccurred())
	cond := novaCondition(t, cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("NovaMetadataSecretError"))

	var list novav1alpha1.NovaList
	g.Expect(r.Client.List(context.Background(), &list)).To(Succeed())
	g.Expect(list.Items).To(BeEmpty(), "no child may be projected against a shared secret that was never written")
}

// --- projected child fields ---

// TestReconcileNova_ProjectedChildFields is the field-mapping lock for the
// projection: the release-derived image, the two schemas on the resolved database
// with their own credential Secrets and the one mode they share, the cache, the
// bus delivery, the top-down Keystone endpoints, the region, the service user the
// registration child declares, the resolved store ref, the four replica counts,
// and the two optional client sections the declared siblings switch on.
func TestReconcileNova_ProjectedChildFields(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	// An exposed Keystone: its external URL must reach the child as the public
	// endpoint only, never as the token-validation endpoint.
	cp.Spec.Services.Keystone.Gateway = &commonv1.GatewaySpec{
		ParentRef: commonv1.GatewayParentRefSpec{Name: "openstack-gw"},
		Hostname:  "keystone.example.com",
	}
	cp.Spec.Services.Keystone.PublicEndpoint = "https://keystone.example.com:8443/v3"
	// Both optional siblings declared, so both client sections switch on.
	cp.Spec.Services.Cinder = &c5c3v1alpha1.ServiceCinderSpec{}
	cp.Spec.Services.Barbican = &c5c3v1alpha1.ServiceBarbicanSpec{}
	r := newNovaTestReconciler(t, cp)

	_, err := r.reconcileNova(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	nv := getProjectedNova(t, r.Client, cp)
	g.Expect(nv.Name).To(Equal("cp-nova"))
	g.Expect(nv.Spec.OpenStackRelease).To(Equal("2025.2"))
	g.Expect(nv.Spec.Image.Repository).To(Equal("ghcr.io/c5c3/nova"))
	g.Expect(nv.Spec.Image.Tag).To(Equal("2025.2"), "the tag defaults to spec.openStackRelease")

	// Two schemas on one instance, each with its own operator-owned credential and
	// the managed-shared Dynamic default both blocks have to agree on.
	g.Expect(nv.Spec.APIDatabase.ClusterRef).NotTo(BeNil())
	g.Expect(nv.Spec.APIDatabase.ClusterRef.Name).To(Equal("openstack-db"))
	g.Expect(nv.Spec.APIDatabase.Database).To(Equal("nova_api"))
	g.Expect(nv.Spec.APIDatabase.SecretRef).To(Equal(commonv1.SecretRefSpec{
		Name: "cp-nova-api-db-credentials", Key: "password",
	}))
	g.Expect(nv.Spec.APIDatabase.CredentialsMode).To(Equal(commonv1.CredentialsModeDynamic))
	g.Expect(nv.Spec.Database.ClusterRef).NotTo(BeNil())
	g.Expect(nv.Spec.Database.ClusterRef.Name).To(Equal("openstack-db"))
	g.Expect(nv.Spec.Database.Database).To(Equal("nova"))
	g.Expect(nv.Spec.Database.SecretRef).To(Equal(commonv1.SecretRefSpec{
		Name: "cp-nova-db-credentials", Key: "password",
	}))
	g.Expect(nv.Spec.Database.CredentialsMode).To(Equal(commonv1.CredentialsModeDynamic))
	// DeepCopy: neither block may alias the ControlPlane spec, nor each other.
	g.Expect(nv.Spec.APIDatabase.ClusterRef).NotTo(BeIdenticalTo(cp.Spec.Infrastructure.Database.ClusterRef))
	g.Expect(nv.Spec.APIDatabase.ClusterRef).NotTo(BeIdenticalTo(nv.Spec.Database.ClusterRef))

	g.Expect(nv.Spec.Cache.ClusterRef).NotTo(BeNil())
	g.Expect(nv.Spec.Cache.ClusterRef.Name).To(Equal("openstack-memcached"))
	g.Expect(nv.Spec.Cache.ClusterRef).NotTo(BeIdenticalTo(cp.Spec.Infrastructure.Cache.ClusterRef))

	// The Keystone endpoint is derived top-down, never from the external exposure.
	g.Expect(nv.Spec.KeystoneEndpoint).To(Equal("http://cp-keystone.default.svc:5000/v3"),
		"the token-validation endpoint must be the cluster-local Service URL")
	g.Expect(nv.Spec.KeystonePublicEndpoint).To(Equal("https://keystone.example.com:8443/v3"))

	g.Expect(nv.Spec.Region).To(Equal("RegionOne"))

	g.Expect(nv.Spec.ServiceUser.Username).To(Equal("nova"))
	g.Expect(nv.Spec.ServiceUser.ProjectName).To(Equal("service-nova"))
	g.Expect(nv.Spec.ServiceUser.UserDomainName).To(Equal(adminDomainName(cp)))
	g.Expect(nv.Spec.ServiceUser.ProjectDomainName).To(Equal(adminDomainName(cp)))
	g.Expect(nv.Spec.ServiceUser.SecretRef).To(Equal(commonv1.SecretRefSpec{
		Name: "cp-nova-credentials", Key: "password",
	}))

	g.Expect(nv.Spec.SecretStoreRef).NotTo(BeNil())
	g.Expect(nv.Spec.SecretStoreRef.Kind).To(Equal(commonv1.SecretStoreKindNamespaced))
	g.Expect(nv.Spec.SecretStoreRef.Name).To(Equal("openbao-tenant-store"))

	// The bus reaches the child as a brownfield secretRef naming the delivery
	// written beside it, never as the ControlPlane's own clusterRef.
	g.Expect(nv.Spec.Messaging.ClusterRef).To(BeNil())
	g.Expect(nv.Spec.Messaging.SecretRef).To(Equal(&commonv1.SecretRefSpec{
		Name: "cp-nova-messaging", Key: commonv1.DefaultTransportURLSecretKey,
	}))
	g.Expect(nv.Spec.Messaging.TLS).To(BeNil(), "a plaintext bus projects no trust anchor")

	// Three replicas for the API, one for each of the three processes whose blocks
	// the API server would otherwise default to three.
	g.Expect(nv.Spec.API.Deployment.Replicas).To(Equal(commonv1.DefaultReplicas))
	g.Expect(nv.Spec.Metadata.Deployment.Replicas).To(Equal(int32(1)))
	g.Expect(nv.Spec.Scheduler.Deployment.Replicas).To(Equal(int32(1)))
	g.Expect(nv.Spec.Conductor.Deployment.Replicas).To(Equal(int32(1)))

	// No gateway of any kind, and the console proxy left to the nova webhook.
	g.Expect(nv.Spec.Gateway).To(BeNil())
	g.Expect(nv.Spec.Metadata.Gateway).To(BeNil())
	g.Expect(nv.Spec.ConsoleProxy).To(Equal(novav1alpha1.NovaConsoleProxySpec{}),
		"an undeclared console proxy is left for the nova defaulting webhook to enable")

	// Both optional client sections on, and every override empty: the catalog's
	// internal rows already carry the managed URLs.
	g.Expect(nv.Spec.Endpoints).To(Equal(novav1alpha1.NovaEndpointsSpec{
		Cinder:   novav1alpha1.NovaOptionalEndpointSpec{Enabled: true},
		Barbican: novav1alpha1.NovaOptionalEndpointSpec{Enabled: true},
	}), "the five client sections resolve through the catalog, so no override is projected")

	g.Expect(nv.Spec.DBArchive).To(BeNil(), "an undeclared archive resolves at the nova operator's defaults")

	g.Expect(metav1.IsControlledBy(nv, cp)).To(BeTrue(),
		"the projected Nova must carry the ControlPlane controller owner reference")
}

// TestReconcileNova_ConsoleProxyCases walks the three shapes the console proxy
// takes. The zero block is the one that needs saying: it leaves both the switch
// and the deployment absent on the wire, which is the only state the nova
// defaulting webhook acts on.
func TestReconcileNova_ConsoleProxyCases(t *testing.T) {
	for name, tc := range map[string]struct {
		proxy *c5c3v1alpha1.ServiceNovaConsoleProxySpec
		want  novav1alpha1.NovaConsoleProxySpec
	}{
		"an absent block is left to the nova defaulting webhook": {
			want: novav1alpha1.NovaConsoleProxySpec{},
		},
		"a disabled proxy carries the switch and nothing else": {
			proxy: &c5c3v1alpha1.ServiceNovaConsoleProxySpec{Enabled: ptr.To(false)},
			want:  novav1alpha1.NovaConsoleProxySpec{Enabled: ptr.To(false)},
		},
		"an enabled proxy carries its sizing at the operator default": {
			proxy: &c5c3v1alpha1.ServiceNovaConsoleProxySpec{Enabled: ptr.To(true)},
			want: novav1alpha1.NovaConsoleProxySpec{
				Enabled:    ptr.To(true),
				Deployment: &novav1alpha1.DeploymentSpec{Replicas: 1},
			},
		},
		"an enabled proxy carries its replicas and its own listener": {
			proxy: &c5c3v1alpha1.ServiceNovaConsoleProxySpec{
				Replicas: ptr.To(int32(4)),
				Gateway: &commonv1.GatewaySpec{
					ParentRef: commonv1.GatewayParentRefSpec{Name: "openstack-gw"},
					Hostname:  "nova-console.example.com",
				},
			},
			want: novav1alpha1.NovaConsoleProxySpec{
				Enabled:    ptr.To(true),
				Deployment: &novav1alpha1.DeploymentSpec{Replicas: 4},
				Gateway: &commonv1.GatewaySpec{
					ParentRef: commonv1.GatewayParentRefSpec{Name: "openstack-gw"},
					Hostname:  "nova-console.example.com",
				},
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := novaControlPlane()
			cp.Spec.Services.Nova.ConsoleProxy = tc.proxy
			r := newNovaTestReconciler(t, cp)

			_, err := r.reconcileNova(context.Background(), cp)
			g.Expect(err).NotTo(HaveOccurred())

			nv := getProjectedNova(t, r.Client, cp)
			g.Expect(nv.Spec.ConsoleProxy).To(Equal(tc.want))
			if tc.proxy != nil && tc.proxy.Gateway != nil {
				g.Expect(nv.Spec.ConsoleProxy.Gateway).NotTo(BeIdenticalTo(tc.proxy.Gateway),
					"the projected listener must not alias the ControlPlane spec")
			}
		})
	}
}

// TestReconcileNova_DBArchiveCopiedFieldByField pins the copy of the archive
// block: every knob reaches the child, the pointers are copied by value so the
// child never aliases the ControlPlane spec, and clearing the block reverts the
// child to nil rather than pinning the last schedule.
func TestReconcileNova_DBArchiveCopiedFieldByField(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	cp.Spec.Services.Nova.DBArchive = &c5c3v1alpha1.ServiceNovaDBArchiveSpec{
		Schedule:      "@weekly",
		MaxRows:       ptr.To(int32(5000)),
		Sleep:         ptr.To(int32(0)),
		RetentionDays: ptr.To(int32(30)),
		Suspend:       true,
	}
	r := newNovaTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcileNova(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	nv := getProjectedNova(t, r.Client, cp)
	g.Expect(nv.Spec.DBArchive).To(Equal(&novav1alpha1.DBArchiveSpec{
		Schedule:      "@weekly",
		MaxRows:       ptr.To(int32(5000)),
		Sleep:         ptr.To(int32(0)),
		RetentionDays: ptr.To(int32(30)),
		Suspend:       true,
	}))
	g.Expect(nv.Spec.DBArchive.MaxRows).NotTo(BeIdenticalTo(cp.Spec.Services.Nova.DBArchive.MaxRows),
		"the projected archive must not alias the ControlPlane spec")

	cp.Spec.Services.Nova.DBArchive = nil
	_, err = r.reconcileNova(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedNova(t, r.Client, cp).Spec.DBArchive).To(BeNil(),
		"clearing the block reverts the child to the nova operator's own resolution")
}

// TestReconcileNova_ThreeGatewaysProjectedAndCleared covers the three listeners
// the compute service can expose, each independent of the others: the API, the
// metadata API a compute cluster's agents dial, and the console proxy a browser
// opens a WebSocket against. Clearing them reverts the child so the HTTPRoutes
// come down.
func TestReconcileNova_ThreeGatewaysProjectedAndCleared(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	gateway := func(hostname string) *commonv1.GatewaySpec {
		return &commonv1.GatewaySpec{
			ParentRef: commonv1.GatewayParentRefSpec{Name: "openstack-gw"},
			Hostname:  hostname,
		}
	}
	cp.Spec.Services.Nova.Gateway = gateway("nova.example.com")
	cp.Spec.Services.Nova.MetadataGateway = gateway("nova-metadata.example.com")
	cp.Spec.Services.Nova.ConsoleProxy = &c5c3v1alpha1.ServiceNovaConsoleProxySpec{
		Gateway: gateway("nova-console.example.com"),
	}
	r := newNovaTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcileNova(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	nv := getProjectedNova(t, r.Client, cp)
	g.Expect(nv.Spec.Gateway.Hostname).To(Equal("nova.example.com"))
	g.Expect(nv.Spec.Metadata.Gateway.Hostname).To(Equal("nova-metadata.example.com"))
	g.Expect(nv.Spec.ConsoleProxy.Gateway.Hostname).To(Equal("nova-console.example.com"))
	g.Expect(nv.Spec.Gateway).NotTo(BeIdenticalTo(cp.Spec.Services.Nova.Gateway),
		"the projected gateway must not alias the ControlPlane spec")

	cp.Spec.Services.Nova.Gateway = nil
	cp.Spec.Services.Nova.MetadataGateway = nil
	cp.Spec.Services.Nova.ConsoleProxy = nil
	_, err = r.reconcileNova(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	nv = getProjectedNova(t, r.Client, cp)
	g.Expect(nv.Spec.Gateway).To(BeNil(), "clearing a gateway must tear its HTTPRoute down")
	g.Expect(nv.Spec.Metadata.Gateway).To(BeNil())
	g.Expect(nv.Spec.ConsoleProxy.Gateway).To(BeNil())
}

// TestReconcileNova_ReplicasOverridesWinAndRevert covers all four counts: each
// override reaches its own Deployment, and clearing it reverts the child to the
// default instead of leaving the previously-projected value pinned.
func TestReconcileNova_ReplicasOverridesWinAndRevert(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	cp.Spec.Services.Nova.Replicas = ptr.To(int32(5))
	cp.Spec.Services.Nova.MetadataReplicas = ptr.To(int32(4))
	cp.Spec.Services.Nova.SchedulerReplicas = ptr.To(int32(3))
	cp.Spec.Services.Nova.ConductorReplicas = ptr.To(int32(2))
	r := newNovaTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcileNova(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	nv := getProjectedNova(t, r.Client, cp)
	g.Expect(nv.Spec.API.Deployment.Replicas).To(Equal(int32(5)))
	g.Expect(nv.Spec.Metadata.Deployment.Replicas).To(Equal(int32(4)))
	g.Expect(nv.Spec.Scheduler.Deployment.Replicas).To(Equal(int32(3)))
	g.Expect(nv.Spec.Conductor.Deployment.Replicas).To(Equal(int32(2)))

	cp.Spec.Services.Nova.Replicas = nil
	cp.Spec.Services.Nova.MetadataReplicas = nil
	cp.Spec.Services.Nova.SchedulerReplicas = nil
	cp.Spec.Services.Nova.ConductorReplicas = nil
	_, err = r.reconcileNova(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	nv = getProjectedNova(t, r.Client, cp)
	g.Expect(nv.Spec.API.Deployment.Replicas).To(Equal(commonv1.DefaultReplicas),
		"clearing the override must revert the API to the shared operator default")
	g.Expect(nv.Spec.Metadata.Deployment.Replicas).To(Equal(int32(1)),
		"the three struct-valued blocks revert to one, not to the shared default of three")
	g.Expect(nv.Spec.Scheduler.Deployment.Replicas).To(Equal(int32(1)))
	g.Expect(nv.Spec.Conductor.Deployment.Replicas).To(Equal(int32(1)))
}

// TestReconcileNova_SiblingEndpointSwitchesFollowTheBlocks pins the two optional
// client sections: they follow the sibling blocks and are neither gates nor
// defaults. A Nova with both off runs and serves every request that needs neither
// a volume nor an encryption key.
func TestReconcileNova_SiblingEndpointSwitchesFollowTheBlocks(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	bare := novaControlPlane()
	r := newNovaTestReconciler(t, bare)
	_, err := r.reconcileNova(ctx, bare)
	g.Expect(err).NotTo(HaveOccurred())
	endpoints := getProjectedNova(t, r.Client, bare).Spec.Endpoints
	g.Expect(endpoints.Cinder.Enabled).To(BeFalse(), "no cinder block projects no cinder section")
	g.Expect(endpoints.Barbican.Enabled).To(BeFalse())

	// Declaring a sibling switches its section on, and dropping it again switches
	// the section back off rather than pinning the previous value.
	bare.Spec.Services.Cinder = &c5c3v1alpha1.ServiceCinderSpec{}
	_, err = r.reconcileNova(ctx, bare)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedNova(t, r.Client, bare).Spec.Endpoints.Cinder.Enabled).To(BeTrue())

	bare.Spec.Services.Cinder = nil
	_, err = r.reconcileNova(ctx, bare)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedNova(t, r.Client, bare).Spec.Endpoints.Cinder.Enabled).To(BeFalse(),
		"dropping the block must revert the section")
}

// --- the credentials-mode contract ---

// TestReconcileNova_DatabaseBrownfieldLeavesCredentialsModeUntouched is the other
// half of that contract: a database with no ClusterRef carries user-supplied
// credentials, so the mode and the secretRef are left as declared on BOTH schemas
// and no DB-credential ExternalSecret is projected for either.
func TestReconcileNova_DatabaseBrownfieldLeavesCredentialsModeUntouched(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	cp.Spec.Infrastructure.Database = commonv1.DatabaseSpec{
		Host:      "db.example.com",
		Database:  "keystone",
		SecretRef: commonv1.SecretRefSpec{Name: "brownfield-db"},
	}
	r := newNovaTestReconciler(t, cp)

	_, err := r.reconcileNova(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	nv := getProjectedNova(t, r.Client, cp)
	for name, block := range map[string]commonv1.DatabaseSpec{
		"nova_api": nv.Spec.APIDatabase,
		"nova":     nv.Spec.Database,
	} {
		g.Expect(block.ClusterRef).To(BeNil())
		g.Expect(block.Database).To(Equal(name),
			"the logical schema is always overridden, even for a brownfield database")
		g.Expect(block.CredentialsMode).To(BeEmpty(),
			"a brownfield database must keep its credentialsMode untouched")
		g.Expect(block.SecretRef.Name).To(Equal("brownfield-db"),
			"a brownfield database keeps its user-supplied secretRef")
	}

	for _, target := range novaDBCredentialTargets(cp) {
		g.Expect(r.Get(context.Background(), types.NamespacedName{
			Name: target.secretName, Namespace: target.namespace,
		}, &esov1.ExternalSecret{})).NotTo(Succeed(),
			"no DB-credential ExternalSecret is projected in brownfield mode")
	}
}

// TestReconcileNova_StaticOverrideProjectsStatic covers the per-service opt-out:
// both schemas flip together, because the Nova CRD rejects a child whose two
// database blocks disagree on the mode.
func TestReconcileNova_StaticOverrideProjectsStatic(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	cp.Spec.Services.Nova.DatabaseCredentialsMode = commonv1.CredentialsModeStatic
	r := newNovaTestReconciler(t, cp)

	_, err := r.reconcileNova(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	nv := getProjectedNova(t, r.Client, cp)
	g.Expect(nv.Spec.APIDatabase.CredentialsMode).To(Equal(commonv1.CredentialsModeStatic))
	g.Expect(nv.Spec.Database.CredentialsMode).To(Equal(commonv1.CredentialsModeStatic))
	g.Expect(nv.Spec.APIDatabase.SecretRef.Name).To(Equal("cp-nova-api-db-credentials"),
		"the Static branch still reads an operator-owned Secret, from the KV path instead")
}

// TestReconcileNova_DedicatedDatabaseProjectsStatic pins the dedicated instance:
// the OpenBao database engine carries one connection per namespace bootstrapped
// against the SHARED cluster, so no engine role exists that could issue
// credentials for it and both schemas take the Static branch.
func TestReconcileNova_DedicatedDatabaseProjectsStatic(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	cp.Spec.Services.Nova.DedicatedBackingServices = &c5c3v1alpha1.NovaDedicatedBackingServicesSpec{
		Database: &commonv1.DatabaseSpec{
			ClusterRef: &corev1.LocalObjectReference{Name: "cp-nova-db"},
			Database:   "nova",
			SecretRef:  commonv1.SecretRefSpec{Name: "nova-db"},
			Replicas:   1,
		},
	}
	r := newNovaTestReconciler(t, cp)

	_, err := r.reconcileNova(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	nv := getProjectedNova(t, r.Client, cp)
	g.Expect(nv.Spec.APIDatabase.ClusterRef.Name).To(Equal("cp-nova-db"))
	g.Expect(nv.Spec.Database.ClusterRef.Name).To(Equal("cp-nova-db"))
	g.Expect(nv.Spec.APIDatabase.CredentialsMode).To(Equal(commonv1.CredentialsModeStatic))
	g.Expect(nv.Spec.Database.CredentialsMode).To(Equal(commonv1.CredentialsModeStatic))

	// The Static branch projects the KV-backed ExternalSecrets, one per schema.
	for _, target := range novaDBCredentialTargets(cp) {
		es := &esov1.ExternalSecret{}
		g.Expect(r.Get(context.Background(), types.NamespacedName{
			Name: target.secretName, Namespace: target.namespace,
		}, es)).To(Succeed())
		g.Expect(es.Spec.DataFrom).To(BeEmpty())
		g.Expect(es.Spec.Data).To(HaveLen(2))
		g.Expect(es.Spec.Data[0].RemoteRef.Key).To(Equal(target.kvPath))
	}
}

// TestReconcileNova_LeavesTuningBlocksUnset pins the Placement posture on the
// blocks the ControlPlane deliberately does not drive: the child-side defaults
// stay authoritative, and tuning them stays a standalone-CR concern. Both uWSGI
// blocks are included, because the compute service runs two WSGI applications.
func TestReconcileNova_LeavesTuningBlocksUnset(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	r := newNovaTestReconciler(t, cp)

	_, err := r.reconcileNova(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	nv := getProjectedNova(t, r.Client, cp)
	g.Expect(nv.Spec.NetworkPolicy).To(BeNil())
	g.Expect(nv.Spec.Autoscaling).To(BeNil())
	g.Expect(nv.Spec.Logging).To(BeNil())
	g.Expect(nv.Spec.API.UWSGI).To(BeNil(), "the child-side uWSGI defaults stay authoritative")
	g.Expect(nv.Spec.Metadata.UWSGI).To(BeNil())
	g.Expect(nv.Spec.Scheduler.Workers).To(BeNil(), "the worker counts stay a standalone-CR concern")
	g.Expect(nv.Spec.Conductor.Workers).To(BeNil())
}

func TestReconcileNova_ImageOverrideWins(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	cp.Spec.Services.Nova.Image = &commonv1.ImageSpec{
		Repository: "registry.example.com/mirror/nova",
		Tag:        "custom",
	}
	r := newNovaTestReconciler(t, cp)

	_, err := r.reconcileNova(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	nv := getProjectedNova(t, r.Client, cp)
	g.Expect(nv.Spec.Image.Repository).To(Equal("registry.example.com/mirror/nova"))
	g.Expect(nv.Spec.Image.Tag).To(Equal("custom"))
}

// TestReconcileNova_ExtraConfigMerge proves the projected child's
// spec.extraConfig is the key-by-key merge of globalExtraConfig and the
// per-service block, and that clearing both reverts the child rather than pinning
// the last value.
func TestReconcileNova_ExtraConfigMerge(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	cp.Spec.GlobalExtraConfig = map[string]map[string]string{
		"DEFAULT":  {"debug": "true", "cpu_allocation_ratio": "1.0"},
		"database": {"max_pool_size": "5"},
	}
	cp.Spec.Services.Nova.ExtraConfig = map[string]map[string]string{
		"DEFAULT": {"cpu_allocation_ratio": "16.0"}, // overrides global
	}
	r := newNovaTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcileNova(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedNova(t, r.Client, cp).Spec.ExtraConfig).To(Equal(map[string]map[string]string{
		"DEFAULT": {
			"debug":                "true", // global-only key in the same section
			"cpu_allocation_ratio": "16.0", // per-service wins
		},
		"database": {"max_pool_size": "5"}, // global-only section
	}), "per-service extraConfig must win, global keys/sections merged in")

	cp.Spec.GlobalExtraConfig = nil
	cp.Spec.Services.Nova.ExtraConfig = nil
	_, err = r.reconcileNova(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedNova(t, r.Client, cp).Spec.ExtraConfig).To(BeNil(),
		"clearing both extraConfig blocks must revert the child")
}

// TestReconcileNova_ProjectsBrownfieldMessagingSecretRef pins the delivery
// contract: the nova operator resolves spec.messaging in the Nova's own namespace
// on the Nova's own cluster, so a managed bus declared on the ControlPlane reaches
// the child as a brownfield secretRef naming the Secret this pass wrote there.
func TestReconcileNova_ProjectsBrownfieldMessagingSecretRef(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	r := newNovaTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcileNova(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	nv := getProjectedNova(t, r.Client, cp)
	g.Expect(nv.Spec.Messaging.ClusterRef).To(BeNil(),
		"the child never resolves the ControlPlane's own bus reference")
	g.Expect(nv.Spec.Messaging.SecretRef).To(Equal(&commonv1.SecretRefSpec{
		Name: "cp-nova-messaging", Key: commonv1.DefaultTransportURLSecretKey,
	}))

	delivered := &corev1.Secret{}
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: novaMessagingSecretName(cp), Namespace: cp.NovaNamespace(),
	}, delivered)).To(Succeed())
	g.Expect(delivered.Data).To(HaveKeyWithValue(commonv1.DefaultTransportURLSecretKey, []byte(novaBusURL)))
}

// TestReconcileNova_ProjectsTLSMirrorWhenSharedBusHasTLS covers the trust anchor:
// a bus that declares TLS gets its CA bundle mirrored beside the child, and the
// child's messaging block names the mirror rather than the bundle Secret in the
// ControlPlane's namespace, which its own cluster may not carry. Dropping the tls
// block again reverts both halves once the child has converged on the
// pointer-free spec.
func TestReconcileNova_ProjectsTLSMirrorWhenSharedBusHasTLS(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	cp.Spec.Infrastructure.Messaging.TLS = &commonv1.MessagingTLSSpec{
		CABundleSecretRef: commonv1.SecretRefSpec{Name: "bus-ca", Key: "ca.crt"},
	}
	bundle := []byte("-----BEGIN CERTIFICATE-----\nbus\n-----END CERTIFICATE-----\n")
	busCA := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "bus-ca", Namespace: cp.Namespace},
		Data:       map[string][]byte{"ca.crt": bundle},
	}
	r := newNovaTestReconciler(t, cp, busCA)
	ctx := context.Background()

	_, err := r.reconcileNova(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	nv := getProjectedNova(t, r.Client, cp)
	g.Expect(nv.Spec.Messaging.TLS).NotTo(BeNil())
	g.Expect(nv.Spec.Messaging.TLS.CABundleSecretRef).To(Equal(commonv1.SecretRefSpec{
		Name: "cp-nova-messaging-ca", Key: serviceMessagingCAKey,
	}))

	mirror := &corev1.Secret{}
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: novaMessagingCASecretName(cp), Namespace: cp.NovaNamespace(),
	}, mirror)).To(Succeed())
	g.Expect(mirror.Data).To(HaveKeyWithValue(serviceMessagingCAKey, bundle))

	// Drop the tls block: the child must revert, and the mirror comes down behind
	// it once the child reports the generation the apply produced.
	convergeNovaChild(t, r, cp, getProjectedNova(t, r.Client, cp).Generation)
	cp.Spec.Infrastructure.Messaging.TLS = nil
	_, err = r.reconcileNova(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	nv = getProjectedNova(t, r.Client, cp)
	g.Expect(nv.Spec.Messaging.TLS).To(BeNil(),
		"the child must not keep a trust anchor whose mirror this same pass deleted")
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: novaMessagingCASecretName(cp), Namespace: cp.NovaNamespace(),
	}, &corev1.Secret{})).NotTo(Succeed())
}

// TestReconcileNova_KeepsTheCAMirrorUntilTheChildHasConvergedOnTheDrop covers
// the far side of the apply that removes the pointer. Removing
// spec.messaging.tls from the CR does not remove the volume from the workloads:
// the nova operator renders its Deployments on a pass of its own, and until it
// has, the live pod templates still name the mirror as a REQUIRED Secret volume
// source. Reaping it in that window leaves every pod created in it on
// FailedMount, so the reap waits for the child to report the generation the
// apply produced.
func TestReconcileNova_KeepsTheCAMirrorUntilTheChildHasConvergedOnTheDrop(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cp := novaControlPlane()
	cp.Spec.Infrastructure.Messaging.TLS = &commonv1.MessagingTLSSpec{
		CABundleSecretRef: commonv1.SecretRefSpec{Name: "bus-ca", Key: "ca.crt"},
	}
	busCA := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "bus-ca", Namespace: cp.Namespace},
		Data:       map[string][]byte{"ca.crt": []byte("bus bundle")},
	}
	r := newNovaTestReconciler(t, cp, busCA)
	mirrorKey := types.NamespacedName{Name: novaMessagingCASecretName(cp), Namespace: cp.NovaNamespace()}

	_, err := r.reconcileNova(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(r.Get(ctx, mirrorKey, &corev1.Secret{})).To(Succeed())

	// Pin the child one generation behind: Ready and converged on the spec that
	// still carried the pointer.
	nv := getProjectedNova(t, r.Client, cp)
	nv.Generation = 2
	g.Expect(r.Client.Update(ctx, nv)).To(Succeed())
	convergeNovaChild(t, r, cp, 1)

	cp.Spec.Infrastructure.Messaging.TLS = nil
	_, err = r.reconcileNova(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedNova(t, r.Client, cp).Spec.Messaging.TLS).To(BeNil(),
		"the apply must drop the pointer from the CR")
	g.Expect(r.Get(ctx, mirrorKey, &corev1.Secret{})).To(Succeed(),
		"the mirror must outlive the pointer until the child has re-rendered without it")

	convergeNovaChild(t, r, cp, 2)
	_, err = r.reconcileNova(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(apierrors.IsNotFound(r.Get(ctx, mirrorKey, &corev1.Secret{}))).To(BeTrue(),
		"a converged child leaves no trust anchor behind in the Nova namespace")
}

// TestReconcileNova_KeepsTheCAMirrorWhileTheChildStillNamesIt covers the window
// the convergence gate above does not: the gates between the messaging leg and
// the projection can halt the pass with the child's spec.messaging.tls still
// naming the mirror. Reaping the mirror there would leave the live child pointing
// at a volume source that no longer exists.
func TestReconcileNova_KeepsTheCAMirrorWhileTheChildStillNamesIt(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cp := novaControlPlane()
	cp.Spec.Infrastructure.Messaging.TLS = &commonv1.MessagingTLSSpec{
		CABundleSecretRef: commonv1.SecretRefSpec{Name: "bus-ca", Key: "ca.crt"},
	}
	busCA := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "bus-ca", Namespace: cp.Namespace},
		Data:       map[string][]byte{"ca.crt": []byte("bus bundle")},
	}
	r := newNovaTestReconciler(t, cp, busCA)

	_, err := r.reconcileNova(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedNova(t, r.Client, cp).Spec.Messaging.TLS).NotTo(BeNil())

	// Drop the tls block, and halt this pass after the messaging leg by putting
	// the registration back into a rotation: the child is never re-applied, so
	// its pointer at the mirror stays live.
	cp.Spec.Infrastructure.Messaging.TLS = nil
	rotating := &c5c3v1alpha1.KeystoneService{}
	g.Expect(r.Get(ctx, types.NamespacedName{Name: novaName(cp), Namespace: cp.NovaNamespace()},
		rotating)).To(Succeed())
	conditions.SetCondition(&rotating.Status.Conditions, metav1.Condition{
		Type:    conditionTypeKeystoneServiceAccountReady,
		Status:  metav1.ConditionFalse,
		Reason:  "RotatingPassword",
		Message: "the service account password is being rotated",
	})
	g.Expect(r.Status().Update(ctx, rotating)).To(Succeed())

	res, err := r.reconcileNova(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).NotTo(BeZero(), "the pass has to halt on the registration gate")
	g.Expect(novaCondition(t, cp).Reason).To(Equal(reasonWaitingForServiceRegistration))

	g.Expect(getProjectedNova(t, r.Client, cp).Spec.Messaging.TLS).NotTo(BeNil(),
		"the halted pass left the child's pointer at the mirror in place")
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: novaMessagingCASecretName(cp), Namespace: cp.NovaNamespace(),
	}, &corev1.Secret{})).To(Succeed(),
		"the mirror must not be deleted while the live child still names it as a volume source")
}

// --- readiness ---

// TestReconcileNova_MirrorsChildReady exercises the readiness mirror: a fresh
// child is not ready (WaitingForNova + requeue), a Ready child flips NovaReady
// True.
func TestReconcileNova_MirrorsChildReady(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	r := newNovaTestReconciler(t, cp)
	ctx := context.Background()

	res, err := r.reconcileNova(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(infraRequeueAfter))
	cond := novaCondition(t, cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForNova"))

	convergeNovaChild(t, r, cp, getProjectedNova(t, r.Client, cp).Generation)

	res, err = r.reconcileNova(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond = novaCondition(t, cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("NovaReady"))
}

// TestReconcileNova_ReadyFoldsInTheRegistration proves NovaReady is the
// conjunction of both children: a Ready Nova whose registration collided on the
// catalog row keeps NovaReady False, naming the failing child condition.
func TestReconcileNova_ReadyFoldsInTheRegistration(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	ks := novaRegistration(cp,
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
			Message: `a service row of type "compute" named "nova" already exists`,
		},
		metav1.Condition{
			Type:    conditionTypeReady,
			Status:  metav1.ConditionFalse,
			Reason:  "NotAllReady",
			Message: "One or more sub-conditions are not ready",
		},
	)
	r := newNovaTestReconciler(t, cp, ks)
	ctx := context.Background()

	_, err := r.reconcileNova(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	// The Nova child itself reaches Ready.
	convergeNovaChild(t, r, cp, getProjectedNova(t, r.Client, cp).Generation)

	res, err := r.reconcileNova(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter))
	cond := novaCondition(t, cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse),
		"a Nova nothing can discover through the catalog is not ready")
	g.Expect(cond.Reason).To(Equal(reasonKeystoneServiceCatalogCollision),
		"the failing sub-condition's reason is relayed, not the aggregate's")
	g.Expect(cond.Message).To(ContainSubstring(conditionTypeKeystoneServiceCatalogReady))
	g.Expect(cond.Message).To(ContainSubstring("cp-nova"))
}

// newNovaReconcilerWithChildApplyError wires a reconciler whose every Nova apply
// fails with err, the two failure modes the projection maps onto distinct
// conditions.
func newNovaReconcilerWithChildApplyError(
	t *testing.T, cp *c5c3v1alpha1.ControlPlane, applyErr error,
) *ControlPlaneReconciler {
	t.Helper()
	s := novaTestScheme(t)
	seeded := withReadyNovaRegistration(withNovaBusSecret(withReadyNovaDBCreds([]client.Object{cp})))
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(seeded...).
		WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}, &novav1alpha1.Nova{},
			&c5c3v1alpha1.KeystoneService{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(ctx context.Context, cl client.WithWatch, obj runtime.ApplyConfiguration,
				opts ...client.ApplyOption,
			) error {
				if ac, ok := obj.(client.Object); ok &&
					ac.GetObjectKind().GroupVersionKind().Kind == "Nova" {
					return applyErr
				}
				return cl.Apply(ctx, obj, opts...)
			},
		}).Build()
	return &ControlPlaneReconciler{Client: c, Scheme: s}
}

// TestReconcileNova_InvalidChildMapsToProjectionRejected pins the rejection leg:
// an Invalid (HTTP 422) answer from the Nova API server means the projected spec
// violates a CRD or webhook rule, which no retry fixes, so the condition names
// what an operator has to correct.
func TestReconcileNova_InvalidChildMapsToProjectionRejected(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	invalid := apierrors.NewInvalid(
		schema.GroupKind{Group: "nova.openstack.c5c3.io", Kind: "Nova"}, novaName(cp),
		field.ErrorList{field.Invalid(
			field.NewPath("spec", "apiDatabase", "database"), "nova_api", "must differ from database.database")})
	r := newNovaReconcilerWithChildApplyError(t, cp, invalid)

	_, err := r.reconcileNova(context.Background(), cp)

	g.Expect(err).To(HaveOccurred())
	cond := novaCondition(t, cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("NovaProjectionRejected"))
	g.Expect(cond.Message).To(ContainSubstring("reconcile the ControlPlane spec"))
}

// TestReconcileNova_CreateErrorMapsToNovaError covers the other failure mode: a
// transient write failure is returned for a retry under the generic reason.
func TestReconcileNova_CreateErrorMapsToNovaError(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	r := newNovaReconcilerWithChildApplyError(t, cp,
		apierrors.NewInternalError(errors.New("etcd is unavailable")))

	_, err := r.reconcileNova(context.Background(), cp)

	g.Expect(err).To(HaveOccurred())
	cond := novaCondition(t, cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("NovaError"))
	g.Expect(cond.Message).To(ContainSubstring("etcd is unavailable"))
}

// --- the projected KeystoneService registration ---

// TestReconcileNova_ProjectsTheRegistration pins the registration the leg writes:
// the compute catalog entry with both endpoint rows on the "/v2.1" path, the
// service account in its own per-service project holding the admin role beside
// service, and the explicit controlPlaneRef a child in a dedicated namespace
// needs to resolve the ControlPlane at all.
func TestReconcileNova_ProjectsTheRegistration(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	r := newNovaTestReconciler(t, cp)

	_, err := r.reconcileNova(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	ks := &c5c3v1alpha1.KeystoneService{}
	g.Expect(r.Get(context.Background(), types.NamespacedName{
		Name: novaName(cp), Namespace: cp.NovaNamespace(),
	}, ks)).To(Succeed())
	g.Expect(ks.Spec.ControlPlaneRef).To(Equal(c5c3v1alpha1.ControlPlaneRefSpec{Name: "cp", Namespace: "default"}))
	g.Expect(ks.Spec.Catalog.ServiceType).To(Equal("compute"))
	g.Expect(ks.Spec.Catalog.ServiceName).To(Equal("nova"))
	g.Expect(ks.Spec.Catalog.Adopt).To(BeFalse(), "a colliding catalog row must fail loud, never be adopted")
	g.Expect(ks.Spec.Catalog.Endpoints[0].URL).To(Equal("http://cp-nova.default.svc:8774/v2.1"))
	g.Expect(ks.Spec.Catalog.Endpoints[1].URL).To(Equal("http://cp-nova.default.svc:8774/v2.1"),
		"an unexposed Nova advertises the in-cluster URL on both rows")
	g.Expect(ks.Spec.Account.UserName).To(Equal("nova"))
	g.Expect(ks.Spec.Account.Project.Name).To(Equal("service-nova"))
	g.Expect(ks.Spec.Account.Roles).To(Equal([]string{"service", "admin"}))
	g.Expect(metav1.IsControlledBy(ks, cp)).To(BeTrue(),
		"a co-located registration carries the ControlPlane controller owner reference")
}

// --- per-service target clusters ---

// TestReconcileNova_ProjectsTheTargetClusterRef verifies the placement reaches
// the child verbatim, the nova-operator owning everything on the target, and that
// an unplaced service projects no ref.
func TestReconcileNova_ProjectsTheTargetClusterRef(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := placedNovaControlPlane("remote-a")
	r := newNovaTestReconciler(t, cp)

	_, err := r.reconcileNova(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(getProjectedNova(t, r.Client, cp).Spec.TargetClusterRef).
		To(Equal(&commonv1.TargetClusterRefSpec{Name: "remote-a"}))

	unplaced := novaControlPlane()
	r2 := newNovaTestReconciler(t, unplaced)
	_, err = r2.reconcileNova(context.Background(), unplaced)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getProjectedNova(t, r2.Client, unplaced).Spec.TargetClusterRef).To(BeNil(),
		"a service that names no cluster must project no ref at all")
}

// placedNovaControlPlane places the compute service in a namespace of its own on
// a target cluster. Its database is brownfield, so the two credential legs
// project nothing and the pass reaches the child projection over the local client
// alone.
func placedNovaControlPlane(targetCluster string) *c5c3v1alpha1.ControlPlane {
	cp := novaControlPlane()
	cp.Spec.Infrastructure.Database = commonv1.DatabaseSpec{
		Host:      "db.example.com",
		Database:  "keystone",
		SecretRef: commonv1.SecretRefSpec{Name: "keystone-db"},
	}
	cp.Spec.Services.Nova.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
		Name:      "compute",
		Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
	}
	cp.Spec.Services.Nova.PublicEndpoint = "https://nova.example.com"
	cp.Spec.Services.Nova.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: targetCluster}
	return cp
}

// TestReconcileNova_CrossNamespaceChildIsLabelledNotOwned verifies the ownership
// substitute for a compute service placed in a namespace of its own: the Nova
// child carries the ControlPlane's ownership labels and NO owner reference
// (Kubernetes forbids a cross-namespace one).
func TestReconcileNova_CrossNamespaceChildIsLabelledNotOwned(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	cp.Spec.Services.Nova.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
		Name: "compute", Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
	}
	r := newNovaTestReconciler(t, cp)

	_, err := r.reconcileNova(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	nv := getProjectedNova(t, r.Client, cp)
	g.Expect(nv.Namespace).To(Equal("compute"))
	g.Expect(nv.OwnerReferences).To(BeEmpty(), "a cross-namespace child cannot carry an owner reference")
	g.Expect(nv.Labels).To(HaveKeyWithValue(controlPlaneNameLabel, "cp"))
	g.Expect(nv.Labels).To(HaveKeyWithValue(controlPlaneNamespaceLabel, "default"))
}

// TestNovaKeystoneEndpoint_FollowsTheNova pins the endpoint policy: Nova
// validates tokens against Keystone itself, so it gets the in-cluster Service DNS
// name exactly while the two services share a cluster, and the public URL as soon
// as they do not, because that name resolves nowhere else.
func TestNovaKeystoneEndpoint_FollowsTheNova(t *testing.T) {
	const (
		inCluster = "http://cp-keystone.identity.svc:5000/v3"
		public    = "https://keystone.example.com/v3"
	)
	for _, tc := range []struct {
		name           string
		nova, keystone *commonv1.TargetClusterRefSpec
		want           string
	}{
		{name: "both co-located", want: inCluster},
		{
			name:     "both on the same cluster",
			nova:     &commonv1.TargetClusterRefSpec{Name: "remote-a"},
			keystone: &commonv1.TargetClusterRefSpec{Name: "remote-a"},
			want:     inCluster,
		},
		{
			name: "Nova placed, Keystone at home",
			nova: &commonv1.TargetClusterRefSpec{Name: "remote-a"},
			want: public,
		},
		{
			name:     "Keystone placed, Nova at home",
			keystone: &commonv1.TargetClusterRefSpec{Name: "remote-a"},
			want:     public,
		},
		{
			name:     "different clusters",
			nova:     &commonv1.TargetClusterRefSpec{Name: "remote-a"},
			keystone: &commonv1.TargetClusterRefSpec{Name: "remote-b"},
			want:     public,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := novaControlPlane()
			cp.Spec.Services.Keystone.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{Name: "identity"}
			cp.Spec.Services.Keystone.PublicEndpoint = public
			cp.Spec.Services.Keystone.TargetClusterRef = tc.keystone
			cp.Spec.Services.Nova.TargetClusterRef = tc.nova

			g.Expect(novaKeystoneEndpoint(cp)).To(Equal(tc.want))
		})
	}
}

// --- the compute-config mirror seam ---

// TestReconcileNova_NoMirrorTargetsWritesNothing pins the seam as it stands: no
// compute cluster is attached to a ControlPlane yet, so the mirror enumerates no
// target, writes nothing, and never holds the compute service back from Ready.
func TestReconcileNova_NoMirrorTargetsWritesNothing(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	g.Expect(novaComputeConfigMirrorTargets(cp)).To(BeEmpty(),
		"#1013 is what fills this from the compute-cluster attachment")

	// The contract is published, so a mirror that ran would have something to
	// copy: the only Secret under that name must stay the published one.
	published := publishedComputeConfig(cp)
	r := newNovaTestReconciler(t, cp, published)
	ctx := context.Background()

	_, err := r.reconcileNova(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	convergeNovaChild(t, r, cp, getProjectedNova(t, r.Client, cp).Generation)

	res, err := r.reconcileNova(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(novaCondition(t, cp).Status).To(Equal(metav1.ConditionTrue),
		"a seam with no target must not park the plane")

	var secrets corev1.SecretList
	g.Expect(r.Client.List(ctx, &secrets)).To(Succeed())
	var copies []string
	for _, secret := range secrets.Items {
		if secret.Name == novaComputeConfigSecretName(cp) {
			copies = append(copies, secret.Namespace)
		}
	}
	g.Expect(copies).To(ConsistOf(published.Namespace),
		"nothing copies the compute contract while no target is enumerated")
}

// publishedComputeConfig builds the Secret the nova operator publishes the
// compute contract under, the one every mirror target is fed from.
func publishedComputeConfig(cp *c5c3v1alpha1.ControlPlane) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: novaComputeConfigSecretName(cp), Namespace: cp.NovaNamespace(),
		},
		Data: map[string][]byte{
			"nova-compute.conf": []byte("[DEFAULT]\ntransport_url = rabbit://u:p@bus:5672/\n"),
		},
	}
}

// TestMirrorNovaComputeConfig_CopiesTheSecretToTheTarget covers the delivery: the
// contract the nova operator published reaches the namespace a compute cluster's
// agents read it from, data for data, carrying the ownership labels the teardown
// sweep selects on.
func TestMirrorNovaComputeConfig_CopiesTheSecretToTheTarget(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	published := publishedComputeConfig(cp)
	r := newNovaTestReconciler(t, cp, published)
	ctx := context.Background()

	ok, reason, message, err := r.mirrorNovaComputeConfig(ctx, cp,
		computeConfigMirrorTarget{Namespace: "compute-nodes"})

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ok).To(BeTrue())
	g.Expect(reason).To(BeEmpty())
	g.Expect(message).To(BeEmpty())

	mirror := &corev1.Secret{}
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: novaComputeConfigSecretName(cp), Namespace: "compute-nodes",
	}, mirror)).To(Succeed())
	g.Expect(mirror.Data).To(Equal(published.Data))
	g.Expect(mirror.Labels).To(HaveKeyWithValue(controlPlaneNameLabel, "cp"))
	g.Expect(mirror.Labels).To(HaveKeyWithValue(controlPlaneNamespaceLabel, "default"))
	g.Expect(mirror.OwnerReferences).To(BeEmpty(),
		"a mirror in another namespace cannot carry an owner reference")
}

// TestMirrorNovaComputeConfig_DeliversOnTheTargetCluster covers a target on
// another cluster: the contract is read where the nova operator published it and
// written through the target's own client, so nothing lands on the management
// cluster, where no compute node would read it.
func TestMirrorNovaComputeConfig_DeliversOnTheTargetCluster(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	published := publishedComputeConfig(cp)
	r := newNovaTestReconciler(t, cp, published)
	target := fake.NewClientBuilder().WithScheme(r.Scheme).Build()
	r.Resolver = &childrenResolver{children: target}
	ctx := context.Background()
	key := types.NamespacedName{Name: novaComputeConfigSecretName(cp), Namespace: "compute-nodes"}

	ok, _, _, err := r.mirrorNovaComputeConfig(ctx, cp, computeConfigMirrorTarget{
		ClusterRef: &commonv1.TargetClusterRefSpec{Name: "edge-a"},
		Namespace:  "compute-nodes",
	})

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ok).To(BeTrue())
	mirror := &corev1.Secret{}
	g.Expect(target.Get(ctx, key, mirror)).To(Succeed(), "the mirror lands on the compute cluster")
	g.Expect(mirror.Data).To(Equal(published.Data))
	g.Expect(mirror.Labels).To(HaveKeyWithValue(controlPlaneNameLabel, "cp"))
	g.Expect(apierrors.IsNotFound(r.Get(ctx, key, &corev1.Secret{}))).To(BeTrue(),
		"nothing may be written on the management cluster")
}

// TestMirrorNovaComputeConfig_RefusesAForeignSecret pins the adoption guard on
// the delivery: the contract carries the bus credentials, and a same-named
// Secret somebody else wrote into the target namespace must be neither
// overwritten nor claimed for the teardown to delete.
func TestMirrorNovaComputeConfig_RefusesAForeignSecret(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	foreign := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: novaComputeConfigSecretName(cp), Namespace: "compute-nodes"},
		Data:       map[string][]byte{"theirs": []byte("keep me")},
	}
	r := newNovaTestReconciler(t, cp, publishedComputeConfig(cp), foreign)
	ctx := context.Background()

	ok, _, _, err := r.mirrorNovaComputeConfig(ctx, cp, computeConfigMirrorTarget{Namespace: "compute-nodes"})

	g.Expect(err).To(MatchError(ContainSubstring("refusing to adopt")))
	g.Expect(ok).To(BeFalse())
	live := &corev1.Secret{}
	g.Expect(r.Get(ctx, client.ObjectKeyFromObject(foreign), live)).To(Succeed())
	g.Expect(live.Data).To(Equal(foreign.Data), "the foreign Secret's data must be untouched")
	g.Expect(live.Labels).NotTo(HaveKey(controlPlaneNameLabel), "nor may it be marked for the teardown")
}

// TestMirrorNovaComputeConfig_WaitsWhileTheSourceIsAbsent covers the ordinary
// transient: the nova operator writes the contract once the control plane has
// converged, so until then the target is a wait rather than a failure.
func TestMirrorNovaComputeConfig_WaitsWhileTheSourceIsAbsent(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	r := newNovaTestReconciler(t, cp)

	ok, reason, message, err := r.mirrorNovaComputeConfig(context.Background(), cp,
		computeConfigMirrorTarget{Namespace: "compute-nodes"})

	g.Expect(err).NotTo(HaveOccurred(), "a contract that has not been published yet is not a failure")
	g.Expect(ok).To(BeFalse())
	g.Expect(reason).To(Equal(reasonWaitingForComputeConfig))
	g.Expect(message).To(ContainSubstring(novaComputeConfigSecretName(cp)))
	g.Expect(message).To(ContainSubstring("compute-nodes"))

	g.Expect(r.Get(context.Background(), types.NamespacedName{
		Name: novaComputeConfigSecretName(cp), Namespace: "compute-nodes",
	}, &corev1.Secret{})).NotTo(Succeed(), "nothing may be written before the source exists")
}

// TestMirrorNovaComputeConfig_ReportsAnUnresolvableCluster covers the target on a
// cluster that does not resolve: the resolver's own text is what an operator
// reading the condition needs, so it is relayed rather than returned as a failed
// reconcile.
func TestMirrorNovaComputeConfig_ReportsAnUnresolvableCluster(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	r := newNovaTestReconciler(t, cp, publishedComputeConfig(cp))
	r.Resolver = &childrenResolver{err: errors.New("cluster not found")}

	ok, reason, message, err := r.mirrorNovaComputeConfig(context.Background(), cp,
		computeConfigMirrorTarget{
			ClusterRef: &commonv1.TargetClusterRefSpec{Name: "edge-a"},
			Namespace:  "compute-nodes",
		})

	g.Expect(err).NotTo(HaveOccurred(), "an unresolvable cluster is a wait, not a failed reconcile")
	g.Expect(ok).To(BeFalse())
	g.Expect(reason).To(Equal(commonmulticluster.TargetClusterUnavailable))
	g.Expect(message).To(ContainSubstring("cluster not found"))
}
