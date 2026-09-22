// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the Nova sub-reconciler: the naming helpers in reconcile_nova.go,
// the catalog URL they feed (novaCatalogURL), the projected Nova child, and the
// NovaReady condition the projection drives.
package controller

import (
	"strings"
	"testing"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esgenv1alpha1 "github.com/external-secrets/external-secrets/apis/generators/v1alpha1"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
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
