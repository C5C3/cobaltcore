// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"regexp"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	commonv1 "github.com/c5c3/forge/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// choiceIDPattern mirrors the +kubebuilder:validation:Pattern marker on the
// Horizon CRD's websso.choices[].id, so a truncated id is checked against the
// same character set the API server enforces.
var choiceIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// backend builds a KeystoneIdentityBackend fixture. ready toggles the
// aggregate Ready condition the projection gates on. OIDC fixtures use a
// non-Default domain: the CRD forbids backing the Default domain — which hosts
// the SQL service users and the bootstrap admin — with an external backend.
func backend(name string, typ keystonev1alpha1.IdentityBackendType, domain string, ready bool) keystonev1alpha1.KeystoneIdentityBackend {
	status := metav1.ConditionFalse
	if ready {
		status = metav1.ConditionTrue
	}
	b := keystonev1alpha1.KeystoneIdentityBackend{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "openstack"},
		Spec: keystonev1alpha1.KeystoneIdentityBackendSpec{
			KeystoneRef: keystonev1alpha1.KeystoneRefSpec{Name: "cp-keystone"},
			Domain:      keystonev1alpha1.DomainSpec{Name: domain},
			Type:        typ,
		},
		Status: keystonev1alpha1.KeystoneIdentityBackendStatus{
			Conditions: []metav1.Condition{{
				Type:   "Ready",
				Status: status,
				Reason: "Test",
			}},
		},
	}
	return b
}

// TestListIdentityBackends_SkipsTerminatingAndForeignKeystoneRef guards the two
// filters the projection depends on. The Terminating case is the sharp one: a
// backend under teardown keeps Ready=True, because its own reconcileDelete parks
// on a requeue waiting for de-projection and never demotes the condition. Left in
// the list it would keep contributing a websso choice pointing at federation
// objects that are being deleted, keep the projection believing a backend is
// attached, and collide with the same-named replacement the backend webhook
// deliberately admits while the old one is Terminating.
func TestListIdentityBackends_SkipsTerminatingAndForeignKeystoneRef(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := mapperControlPlane("cp", "openstack", "admin-secret")

	live := backend("keycloak", keystonev1alpha1.IdentityBackendTypeOIDC, "federated", true)

	// A Terminating backend still reports Ready=True. The fake client demands a
	// finalizer alongside the DeletionTimestamp — which is exactly what holds the
	// real object in Terminating too.
	terminating := backend("keycloak-old", keystonev1alpha1.IdentityBackendTypeOIDC, "legacy", true)
	deletedAt := metav1.Now()
	terminating.DeletionTimestamp = &deletedAt
	terminating.Finalizers = []string{"keystone.openstack.c5c3.io/identity-backend"}

	foreign := backend("standalone", keystonev1alpha1.IdentityBackendTypeOIDC, "other", true)
	foreign.Spec.KeystoneRef.Name = "standalone-keystone"

	r := identityBackendMapperReconciler(t, cp, &live, &terminating, &foreign)

	got, err := r.listIdentityBackends(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(got).To(HaveLen(1))
	g.Expect(got[0].Name).To(Equal("keycloak"))

	// The Terminating backend therefore offers no websso choice either.
	g.Expect(readyFederationBackends(got)).To(HaveLen(1))
}

func TestReadyFederationBackends_SkipsNotReadyAndNonOIDC(t *testing.T) {
	g := NewGomegaWithT(t)
	backends := []keystonev1alpha1.KeystoneIdentityBackend{
		backend("keycloak", keystonev1alpha1.IdentityBackendTypeOIDC, "federated", true),
		backend("pending-idp", keystonev1alpha1.IdentityBackendTypeOIDC, "federated", false),
		backend("ldap", keystonev1alpha1.IdentityBackendTypeLDAP, "planetexpress", true),
	}

	got := readyFederationBackends(backends)
	g.Expect(got).To(HaveLen(1))
	g.Expect(got[0].Name).To(Equal("keycloak"))
}

// TestReadyFederationBackends_SortedByIdentityProvider guards the projection's
// determinism: an unstable order would churn the Horizon child's spec (and its
// content-addressed settings ConfigMap) on every reconcile.
func TestReadyFederationBackends_SortedByIdentityProvider(t *testing.T) {
	g := NewGomegaWithT(t)
	backends := []keystonev1alpha1.KeystoneIdentityBackend{
		backend("zulu", keystonev1alpha1.IdentityBackendTypeOIDC, "federated", true),
		backend("alpha", keystonev1alpha1.IdentityBackendTypeOIDC, "federated", true),
		backend("mike", keystonev1alpha1.IdentityBackendTypeOIDC, "federated", true),
	}

	got := readyFederationBackends(backends)
	g.Expect(got).To(HaveLen(3))
	g.Expect(got[0].Name).To(Equal("alpha"))
	g.Expect(got[1].Name).To(Equal("mike"))
	g.Expect(got[2].Name).To(Equal("zulu"))
}

func TestReadyFederationBackends_EmptyInputYieldsNil(t *testing.T) {
	g := NewGomegaWithT(t)
	g.Expect(readyFederationBackends(nil)).To(BeNil())
	g.Expect(hasReadyDomainBackend(nil)).To(BeFalse())
}

func TestHasReadyDomainBackend_LDAPOnly(t *testing.T) {
	g := NewGomegaWithT(t)

	// A Ready OIDC backend and a not-Ready LDAP backend both fail to qualify.
	g.Expect(hasReadyDomainBackend([]keystonev1alpha1.KeystoneIdentityBackend{
		backend("oidc", keystonev1alpha1.IdentityBackendTypeOIDC, "federated", true),
		backend("ldap-pending", keystonev1alpha1.IdentityBackendTypeLDAP, "bender", false),
	})).To(BeFalse())

	g.Expect(hasReadyDomainBackend([]keystonev1alpha1.KeystoneIdentityBackend{
		backend("ldap-pending", keystonev1alpha1.IdentityBackendTypeLDAP, "bender", false),
		backend("ldap-ready", keystonev1alpha1.IdentityBackendTypeLDAP, "amy", true),
	})).To(BeTrue())
}

// TestWebSSOChoiceID pins the "<idp>_<protocol>" contract the choice id, the
// idpMapping key, and the login form's auth_type value all share.
func TestWebSSOChoiceID(t *testing.T) {
	g := NewGomegaWithT(t)

	b := backend("keycloak", keystonev1alpha1.IdentityBackendTypeOIDC, "federated", true)
	g.Expect(webSSOChoiceID(&b)).To(Equal("keycloak_openid"))

	// Explicit identityProviderName and protocolID win over the defaults.
	b.Spec.OIDC = &keystonev1alpha1.OIDCBackendSpec{IdentityProviderName: "corp", ProtocolID: "oidc"}
	g.Expect(webSSOChoiceID(&b)).To(Equal("corp_oidc"))
}

// TestWebSSOChoiceID_TruncatesToTheHorizonIDBound guards the projection against
// the wedge an over-long id would cause: the two source fields are bounded at 64
// characters EACH, but Horizon's choices[].id takes 64 in total and the API
// server would reject the whole child.
func TestWebSSOChoiceID_TruncatesToTheHorizonIDBound(t *testing.T) {
	g := NewGomegaWithT(t)

	// 58 characters + "_openid" = 65, one past the bound.
	longIDP := strings.Repeat("a", 58)
	b := backend("keycloak", keystonev1alpha1.IdentityBackendTypeOIDC, "federated", true)
	b.Spec.OIDC = &keystonev1alpha1.OIDCBackendSpec{IdentityProviderName: longIDP}

	id := webSSOChoiceID(&b)
	g.Expect(id).To(HaveLen(maxWebSSOChoiceIDLen))
	g.Expect(choiceIDPattern.MatchString(id)).To(BeTrue(), "the truncated id must still satisfy the CRD pattern")

	// Two backends sharing a truncation prefix keep distinct ids, so their
	// idpMapping entries never collide.
	other := backend("keycloak", keystonev1alpha1.IdentityBackendTypeOIDC, "federated", true)
	other.Spec.OIDC = &keystonev1alpha1.OIDCBackendSpec{IdentityProviderName: longIDP, ProtocolID: "openid2"}
	g.Expect(webSSOChoiceID(&other)).To(HaveLen(maxWebSSOChoiceIDLen))
	g.Expect(webSSOChoiceID(&other)).NotTo(Equal(id))

	// An id exactly at the bound is passed through verbatim.
	exact := backend("keycloak", keystonev1alpha1.IdentityBackendTypeOIDC, "federated", true)
	exact.Spec.OIDC = &keystonev1alpha1.OIDCBackendSpec{IdentityProviderName: strings.Repeat("a", 57)}
	g.Expect(webSSOChoiceID(&exact)).To(Equal(strings.Repeat("a", 57) + "_openid"))
}

func TestHorizonPublicEndpoint(t *testing.T) {
	tests := []struct {
		name string
		hz   *c5c3v1alpha1.ServiceHorizonSpec
		want string
	}{
		{"nil block", nil, ""},
		{"neither publicEndpoint nor gateway", &c5c3v1alpha1.ServiceHorizonSpec{}, ""},
		{
			"explicit publicEndpoint wins over gateway",
			&c5c3v1alpha1.ServiceHorizonSpec{
				PublicEndpoint: "https://dash.example.com:8443",
				Gateway:        &commonv1.GatewaySpec{Hostname: "horizon.example.com"},
			},
			"https://dash.example.com:8443",
		},
		{
			"derived from gateway hostname on the default port",
			&c5c3v1alpha1.ServiceHorizonSpec{Gateway: &commonv1.GatewaySpec{Hostname: "horizon.example.com"}},
			"https://horizon.example.com",
		},
		{
			"trailing slash trimmed so the derived origin carries exactly one",
			&c5c3v1alpha1.ServiceHorizonSpec{PublicEndpoint: "https://horizon.example.com/"},
			"https://horizon.example.com",
		},
		{
			"gateway with empty hostname yields no endpoint",
			&c5c3v1alpha1.ServiceHorizonSpec{Gateway: &commonv1.GatewaySpec{}},
			"",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			g.Expect(horizonPublicEndpoint(tc.hz)).To(Equal(tc.want))
		})
	}
}

// TestTrustedDashboards_AppendsAuthWebssoPath pins the exact origin Keystone
// matches verbatim — including the trailing slash.
func TestTrustedDashboards_AppendsAuthWebssoPath(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := &c5c3v1alpha1.ControlPlane{}
	g.Expect(trustedDashboards(cp)).To(BeEmpty(), "no horizon block means no origin to trust")

	cp.Spec.Services.Horizon = &c5c3v1alpha1.ServiceHorizonSpec{PublicEndpoint: "https://horizon.example.com:8443"}
	g.Expect(trustedDashboards(cp)).To(Equal([]string{"https://horizon.example.com:8443/auth/websso/"}))
}

func TestTrustedDashboards_NilWithoutReachableDashboard(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := &c5c3v1alpha1.ControlPlane{}
	g.Expect(trustedDashboards(cp)).To(BeNil())

	// A horizon block with neither publicEndpoint nor gateway is not externally
	// reachable, so there is still no origin to trust.
	cp.Spec.Services.Horizon = &c5c3v1alpha1.ServiceHorizonSpec{}
	g.Expect(trustedDashboards(cp)).To(BeNil())

	cp.Spec.Services.Horizon = &c5c3v1alpha1.ServiceHorizonSpec{
		Gateway: &commonv1.GatewaySpec{Hostname: "horizon.example.com"},
	}
	g.Expect(trustedDashboards(cp)).To(Equal([]string{"https://horizon.example.com/auth/websso/"}))
}

func TestFederationProxyImage_OverrideWinsOverDefault(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := &c5c3v1alpha1.ControlPlane{}
	cp.Spec.Services.Keystone = &c5c3v1alpha1.ServiceKeystoneSpec{}
	g.Expect(federationProxyImage(cp)).To(Equal(&commonv1.ImageSpec{
		Repository: defaultFederationProxyRepository,
		Tag:        "latest",
	}))

	override := &commonv1.ImageSpec{Repository: "ghcr.io/c5c3/keystone-federation-proxy", Tag: "dev"}
	cp.Spec.Services.Keystone.FederationProxyImage = override
	got := federationProxyImage(cp)
	g.Expect(got).To(Equal(override))
	// DeepCopied, so a later mutation of the projected child cannot alias the
	// ControlPlane's own spec.
	g.Expect(got).NotTo(BeIdenticalTo(override))

	// A nil keystone service block falls back to the default rather than panicking.
	cp.Spec.Services.Keystone = nil
	g.Expect(federationProxyImage(cp).Tag).To(Equal("latest"))
}

// mapperTestReconciler builds a reconciler whose fake client can List
// ControlPlanes, for the identity-backend mapper.
func identityBackendMapperReconciler(t *testing.T, objs ...client.Object) *ControlPlaneReconciler {
	t.Helper()
	s := runtime.NewScheme()
	if err := c5c3v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding c5c3 scheme: %v", err)
	}
	if err := keystonev1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding keystone scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	return &ControlPlaneReconciler{Client: c, Scheme: s}
}

// TestIdentityBackendToControlPlaneMapper_MatchesKeystoneChild proves the watch
// wiring: a backend attached to "cp-keystone" wakes the ControlPlane "cp".
func TestIdentityBackendToControlPlaneMapper_MatchesKeystoneChild(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := mapperControlPlane("cp", "openstack", "admin-secret")
	r := identityBackendMapperReconciler(t, cp)

	b := backend("keycloak", keystonev1alpha1.IdentityBackendTypeOIDC, "federated", true)
	reqs := r.identityBackendToControlPlaneMapper(context.Background(), &b)

	g.Expect(reqs).To(HaveLen(1))
	g.Expect(reqs[0].Name).To(Equal("cp"))
	g.Expect(reqs[0].Namespace).To(Equal("openstack"))
}

// TestIdentityBackendToControlPlaneMapper_IgnoresForeignKeystoneRef guards the
// keystoneName gate: a backend attached to a hand-rolled Keystone beside a
// ControlPlane must not wake it.
func TestIdentityBackendToControlPlaneMapper_IgnoresForeignKeystoneRef(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := mapperControlPlane("cp", "openstack", "admin-secret")
	r := identityBackendMapperReconciler(t, cp)

	b := backend("keycloak", keystonev1alpha1.IdentityBackendTypeOIDC, "federated", true)
	b.Spec.KeystoneRef.Name = "standalone-keystone"

	g.Expect(r.identityBackendToControlPlaneMapper(context.Background(), &b)).To(BeEmpty())
}

// TestIdentityBackendToControlPlaneMapper_IgnoresOtherNamespaces keeps the
// mapper namespace-scoped: a backend in one namespace must never wake a
// ControlPlane in another, even when the Keystone names coincide.
func TestIdentityBackendToControlPlaneMapper_IgnoresOtherNamespaces(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := mapperControlPlane("cp", "other-namespace", "admin-secret")
	r := identityBackendMapperReconciler(t, cp)

	b := backend("keycloak", keystonev1alpha1.IdentityBackendTypeOIDC, "federated", true)
	g.Expect(b.Namespace).To(Equal("openstack"))

	g.Expect(r.identityBackendToControlPlaneMapper(context.Background(), &b)).To(BeEmpty())
}

// TestIdentityBackendToControlPlaneMapper_IgnoresWrongType covers the type
// assertion's failure path — a non-backend object yields no requests rather
// than panicking.
func TestIdentityBackendToControlPlaneMapper_IgnoresWrongType(t *testing.T) {
	g := NewGomegaWithT(t)
	r := identityBackendMapperReconciler(t, mapperControlPlane("cp", "openstack", "admin-secret"))

	g.Expect(r.identityBackendToControlPlaneMapper(context.Background(),
		mapperControlPlane("cp", "openstack", "admin-secret"))).To(BeEmpty())
}
