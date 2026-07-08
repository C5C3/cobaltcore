// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonconditions "github.com/c5c3/forge/internal/common/conditions"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// testFederationKeystone returns testKeystone() with the federation proxy
// image configured, as the managed ControlPlane path projects it.
func testFederationKeystone() *keystonev1alpha1.Keystone {
	ks := testKeystone()
	ks.Spec.Federation = &keystonev1alpha1.FederationSpec{
		ProxyImage: &commonv1.ImageSpec{Repository: "ghcr.io/c5c3/keystone-federation-proxy", Tag: "latest"},
	}
	return ks
}

// testProjectableOIDCBackend returns a DomainReady OIDC backend with explicit
// endpoints (no metadata fetch needed) attached to testKeystone().
func testProjectableOIDCBackend(name string) *keystonev1alpha1.KeystoneIdentityBackend {
	b := testOIDCBackend(name, name+"-domain")
	b.Spec.OIDC.Endpoints = &keystonev1alpha1.OIDCEndpointsSpec{
		AuthorizationEndpoint: "https://idp.example.com/realms/forge/protocol/openid-connect/auth",
		TokenEndpoint:         "https://idp.example.com/realms/forge/protocol/openid-connect/token",
		JWKSURI:               "https://idp.example.com/realms/forge/protocol/openid-connect/certs",
	}
	b.Status = keystonev1alpha1.KeystoneIdentityBackendStatus{
		Conditions: []metav1.Condition{{
			Type:               conditionTypeDomainReady,
			Status:             metav1.ConditionTrue,
			Reason:             "DomainProvisioned",
			LastTransitionTime: metav1.Now(),
		}},
		DomainID: "domain-0001",
	}
	return b
}

// testOIDCClientSecret returns the client-secret Secret referenced by a
// testProjectableOIDCBackend.
func testOIDCClientSecret(backendName string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: backendName + "-client", Namespace: "default"},
		Data:       map[string][]byte{"clientSecret": []byte("rp-secret\n")},
	}
}

func TestIssuerToMetadataBasename(t *testing.T) {
	g := NewGomegaWithT(t)
	cases := map[string]string{
		// The D7 filename contract: scheme-stripped, RFC3986-escaped issuer.
		"http://keycloak.openstack.svc.cluster.local:8080/realms/forge": "keycloak.openstack.svc.cluster.local%3A8080%2Frealms%2Fforge",
		"https://idp.example.com/realms/forge":                          "idp.example.com%2Frealms%2Fforge",
		// A trailing slash is not part of the issuer identity.
		"https://idp.example.com/realms/forge/": "idp.example.com%2Frealms%2Fforge",
		// No path segment at all.
		"https://accounts.example.com": "accounts.example.com",
	}
	for issuer, want := range cases {
		g.Expect(issuerToMetadataBasename(issuer)).To(Equal(want), issuer)
	}
}

func TestClaimStripHeaders_BothSpellingsDedupedAndSorted(t *testing.T) {
	g := NewGomegaWithT(t)

	mappings := []keystonev1alpha1.MappingRuleSpec{{
		Remote: []keystonev1alpha1.MappingRemoteRuleSpec{
			{Type: "HTTP_OIDC_PREFERRED_USERNAME"},
			{Type: "HTTP_OIDC_ISS"}, // duplicate of the remote-id attribute
			// A non-header WSGI key cannot be spoofed via headers.
			{Type: "REMOTE_USER"},
		},
	}}

	headers := claimStripHeaders("HTTP_OIDC_ISS", mappings)
	g.Expect(headers).To(Equal([]string{
		"OIDC-ISS",
		"OIDC-PREFERRED-USERNAME",
		"OIDC_ISS",
		"OIDC_PREFERRED_USERNAME",
	}))
}

func TestRenderProxyConf_DirectivesAndLocations(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testFederationKeystone()

	renders := []oidcRender{
		{
			backendName: "corp-oidc", idpName: "corp-oidc", protocolID: "openid",
			issuer:            "https://idp.example.com/realms/forge",
			metadataBasename:  issuerToMetadataBasename("https://idp.example.com/realms/forge"),
			introspection:     true,
			introspectionEP:   "https://idp.example.com/realms/forge/introspect",
			introspectionTLS:  true,
			clientID:          "keystone",
			clientSecret:      "rp-secret",
			sessionType:       "client-cookie",
			stateInputHeaders: "none",
			stripHeaders:      []string{"OIDC-ISS", "OIDC_ISS"},
		},
		{
			backendName: "corp-oidc2", idpName: "corp-oidc2", protocolID: "openid",
			issuer:            "https://idp.example.com/realms/forge2",
			metadataBasename:  issuerToMetadataBasename("https://idp.example.com/realms/forge2"),
			sessionType:       "client-cookie",
			stateInputHeaders: "none",
			stripHeaders:      []string{"OIDC-EMAIL", "OIDC_EMAIL"},
		},
	}

	conf := string(renderProxyConf(ks, renders, "pass-phrase"))

	// Server-level directives from the spike-validated parameter set.
	g.Expect(conf).To(ContainSubstring(`OIDCCryptoPassphrase "pass-phrase"`))
	g.Expect(conf).To(ContainSubstring("OIDCMetadataDir /etc/keystone-federation-proxy/metadata"))
	g.Expect(conf).To(ContainSubstring("OIDCRedirectURI /v3/OS-FEDERATION/redirect_uri"))
	g.Expect(conf).To(ContainSubstring(`OIDCClaimPrefix "OIDC-"`))
	g.Expect(conf).To(ContainSubstring("OIDCSessionType client-cookie"))
	g.Expect(conf).To(ContainSubstring("OIDCStateInputHeaders none"))

	// Header stripping: both spellings, merged across backends, early.
	for _, h := range []string{"OIDC-ISS", "OIDC_ISS", "OIDC-EMAIL", "OIDC_EMAIL"} {
		g.Expect(conf).To(ContainSubstring(`RequestHeader unset "` + h + `" early`))
	}

	// Reverse proxy to the localhost-bound uWSGI.
	g.Expect(conf).To(ContainSubstring(`ProxyPass "/" "http://127.0.0.1:5000/"`))

	// Introspection directives only for the single introspection backend. The
	// endpoint and client ID are quoted so a value with an embedded space
	// stays a single Apache directive argument.
	g.Expect(conf).To(ContainSubstring(`OIDCOAuthIntrospectionEndpoint "https://idp.example.com/realms/forge/introspect"`))
	g.Expect(conf).To(ContainSubstring(`OIDCOAuthClientID "keystone"`))
	g.Expect(strings.Count(conf, "OIDCOAuthIntrospectionEndpoint")).To(Equal(1))
	g.Expect(conf).NotTo(ContainSubstring("OIDCOAuthSSLValidateServer"),
		"TLS verification stays on unless explicitly opted out")

	// The explicit tlsVerify opt-out renders the validate-off directive.
	optOut := renders[:1]
	optOut[0].introspectionTLS = false
	g.Expect(string(renderProxyConf(ks, optOut, "pass-phrase"))).To(
		ContainSubstring("OIDCOAuthSSLValidateServer Off"),
	)
	optOut[0].introspectionTLS = true

	// Per-IdP protected Locations with OICDiscoverURL ?iss= pinning.
	g.Expect(conf).To(ContainSubstring(`<Location "/v3/auth/OS-FEDERATION/identity_providers/corp-oidc/protocols/openid/websso">`))
	g.Expect(conf).To(ContainSubstring(`<Location "/v3/auth/OS-FEDERATION/identity_providers/corp-oidc2/protocols/openid/websso">`))
	g.Expect(conf).To(ContainSubstring("?iss=" + "https%3A%2F%2Fidp.example.com%2Frealms%2Fforge"))
	g.Expect(conf).To(ContainSubstring("?iss=" + "https%3A%2F%2Fidp.example.com%2Frealms%2Fforge2"))

	// The introspection backend's auth path accepts bearer tokens.
	g.Expect(conf).To(ContainSubstring(`<Location "/v3/OS-FEDERATION/identity_providers/corp-oidc/protocols/openid/auth">`))
	g.Expect(conf).To(ContainSubstring("AuthType auth-openidc"))

	// With more than one backend the global websso path is NOT pinned —
	// per-IdP paths are the supported entry points.
	g.Expect(conf).NotTo(ContainSubstring(`<Location "/v3/auth/OS-FEDERATION/websso/`))

	// A single backend pins the global websso path.
	single := string(renderProxyConf(ks, renders[:1], "pass-phrase"))
	g.Expect(single).To(ContainSubstring(`<Location "/v3/auth/OS-FEDERATION/websso/openid">`))
}

func TestRenderProxyConf_QuotesIntrospectionClientIDWithSpace(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testFederationKeystone()

	// clientID carries no Pattern marker and the webhook's control-char check
	// allows spaces, so a value like "my client" reaches the renderer verbatim.
	renders := []oidcRender{{
		backendName: "corp-oidc", idpName: "corp-oidc", protocolID: "openid",
		issuer:            "https://idp.example.com/realms/forge",
		metadataBasename:  issuerToMetadataBasename("https://idp.example.com/realms/forge"),
		introspection:     true,
		introspectionEP:   "https://idp.example.com/realms/forge/introspect",
		introspectionTLS:  true,
		clientID:          "my client",
		clientSecret:      "rp-secret",
		sessionType:       "client-cookie",
		stateInputHeaders: "none",
	}}

	conf := string(renderProxyConf(ks, renders, "pass-phrase"))

	// The space-bearing clientID must render as a single quoted Apache
	// argument; unquoted it is two arguments — a config-parse error that
	// crash-loops the sidecar and (its targetPort owns the Service) takes the
	// Keystone API down cluster-wide.
	g.Expect(conf).To(ContainSubstring(`OIDCOAuthClientID "my client"`))
	g.Expect(conf).NotTo(ContainSubstring("OIDCOAuthClientID my client"))
	g.Expect(conf).To(ContainSubstring(`OIDCOAuthIntrospectionEndpoint "https://idp.example.com/realms/forge/introspect"`))
}

// TestRenderProxyConf_XForwardedHeadersGatedOnGateway pins the redirect-URI
// hardening: OIDCXForwardedHeaders is emitted only when a trusted Gateway is
// declared. With no gateway, in-cluster clients reach the sidecar directly and
// a spoofed X-Forwarded-Host would poison the redirect_uri, so the directive is
// omitted and mod_auth_openidc falls back to the request host.
func TestRenderProxyConf_XForwardedHeadersGatedOnGateway(t *testing.T) {
	g := NewGomegaWithT(t)
	renders := []oidcRender{{
		backendName: "corp-oidc", idpName: "corp-oidc", protocolID: "openid",
		issuer:            "https://idp.example.com/realms/forge",
		metadataBasename:  issuerToMetadataBasename("https://idp.example.com/realms/forge"),
		sessionType:       "client-cookie",
		stateInputHeaders: "none",
	}}

	// No gateway: the sidecar must not trust inbound X-Forwarded-* headers.
	noGateway := string(renderProxyConf(testFederationKeystone(), renders, "pass-phrase"))
	g.Expect(noGateway).NotTo(ContainSubstring("OIDCXForwardedHeaders"))

	// A declared Gateway is the trust boundary: honor the forwarded host/scheme.
	ks := testFederationKeystone()
	ks.Spec.Gateway = &keystonev1alpha1.GatewaySpec{
		ParentRef: keystonev1alpha1.GatewayParentRefSpec{Name: "public-gateway"},
		Hostname:  "keystone.example.com",
	}
	withGateway := string(renderProxyConf(ks, renders, "pass-phrase"))
	g.Expect(withGateway).To(ContainSubstring("OIDCXForwardedHeaders X-Forwarded-Host X-Forwarded-Proto"))
}

func TestValidateOIDCRenderInputs_RejectsDoubleQuoteAllowsSpace(t *testing.T) {
	g := NewGomegaWithT(t)

	// A double-quote is rejected by the render-time backstop, matching the
	// webhook's OIDC control-char check so a CR that bypassed admission cannot
	// smuggle a quote into a value rendered unquoted elsewhere.
	bad := testProjectableOIDCBackend("corp-oidc")
	bad.Spec.OIDC.ClientID = `keystone" OIDCFoo bar`
	g.Expect(validateOIDCRenderInputs(bad)).To(MatchError(errControlCharInValue))

	// A space alone is allowed through the backstop — the renderer quotes it.
	spaced := testProjectableOIDCBackend("corp-oidc")
	spaced.Spec.OIDC.ClientID = "my client"
	g.Expect(validateOIDCRenderInputs(spaced)).NotTo(HaveOccurred())
}

func TestRenderExplicitProviderMetadata_OptionalEndpoints(t *testing.T) {
	g := NewGomegaWithT(t)
	o := &keystonev1alpha1.OIDCBackendSpec{
		Issuer: "https://idp.example.com/realms/forge",
		Endpoints: &keystonev1alpha1.OIDCEndpointsSpec{
			AuthorizationEndpoint: "https://idp.example.com/auth",
			TokenEndpoint:         "https://idp.example.com/token",
			JWKSURI:               "https://idp.example.com/certs",
		},
	}

	doc, err := renderExplicitProviderMetadata(o)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(string(doc)).To(ContainSubstring(`"issuer": "https://idp.example.com/realms/forge"`))
	g.Expect(string(doc)).NotTo(ContainSubstring("introspection_endpoint"))

	o.Endpoints.IntrospectionEndpoint = "https://idp.example.com/introspect"
	doc, err = renderExplicitProviderMetadata(o)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(introspectionEndpointFromMetadata(doc)).To(Equal("https://idp.example.com/introspect"))
}

// metadataDoer answers every request with the given body and counts calls.
// When fail is set it simulates an IdP metadata-endpoint outage (HTTP 503).
type metadataDoer struct {
	body  string
	calls int
	fail  bool
}

func (m *metadataDoer) Do(_ *http.Request) (*http.Response, error) {
	m.calls++
	status := http.StatusOK
	if m.fail {
		status = http.StatusServiceUnavailable
	}
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(m.body)),
	}, nil
}

func TestFetchProviderMetadata_ValidatesIssuerAndCaches(t *testing.T) {
	g := NewGomegaWithT(t)
	backend := testProjectableOIDCBackend("corp-oidc")
	backend.Spec.OIDC.Endpoints = nil
	backend.Spec.OIDC.ProviderMetadataURL = "https://idp.example.com/realms/forge/.well-known/openid-configuration"

	doer := &metadataDoer{body: `{"issuer":"https://idp.example.com/realms/forge","token_endpoint":"https://idp.example.com/token"}`}
	r := newTestReconciler()
	r.HTTPClient = doer

	doc, err := r.fetchProviderMetadata(context.Background(), backend)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(string(doc)).To(ContainSubstring("token_endpoint"))
	g.Expect(doer.calls).To(Equal(1))

	// Same (uid, generation): served from the cache, no second fetch.
	_, err = r.fetchProviderMetadata(context.Background(), backend)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(doer.calls).To(Equal(1))

	// A spec edit (generation bump) refetches.
	backend.Generation++
	_, err = r.fetchProviderMetadata(context.Background(), backend)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(doer.calls).To(Equal(2))
}

func TestFetchProviderMetadata_RejectsIssuerMismatch(t *testing.T) {
	g := NewGomegaWithT(t)
	backend := testProjectableOIDCBackend("corp-oidc")
	backend.Spec.OIDC.Endpoints = nil

	r := newTestReconciler()
	r.HTTPClient = &metadataDoer{body: `{"issuer":"https://evil.example.com/realms/forge"}`}

	_, err := r.fetchProviderMetadata(context.Background(), backend)
	g.Expect(err).To(MatchError(errProviderMetadataUnavailable))
	g.Expect(err.Error()).To(ContainSubstring("does not match spec issuer"))
	// The error must not reflect the fetched document's issuer: it surfaces in a
	// tenant-visible Event, so echoing response content would make the fetch an
	// SSRF read oracle.
	g.Expect(err.Error()).NotTo(ContainSubstring("evil.example.com"))
}

// ctxDeadlineDoer records whether the request context carried a deadline.
type ctxDeadlineDoer struct {
	body        string
	hasDeadline bool
}

func (d *ctxDeadlineDoer) Do(req *http.Request) (*http.Response, error) {
	_, d.hasDeadline = req.Context().Deadline()
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(d.body)),
	}, nil
}

func TestFetchProviderMetadata_BoundsRequestContext(t *testing.T) {
	g := NewGomegaWithT(t)
	backend := testProjectableOIDCBackend("corp-oidc")
	backend.Spec.OIDC.Endpoints = nil
	backend.Spec.OIDC.ProviderMetadataURL = "https://idp.example.com/realms/forge/.well-known/openid-configuration"

	doer := &ctxDeadlineDoer{body: `{"issuer":"https://idp.example.com/realms/forge"}`}
	r := newTestReconciler()
	r.HTTPClient = doer

	_, err := r.fetchProviderMetadata(context.Background(), backend)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(doer.hasDeadline).To(BeTrue(),
		"the metadata fetch must bound the request context so a slow IdP cannot pin the reconcile worker")
}

func TestIsBlockedMetadataIP(t *testing.T) {
	g := NewGomegaWithT(t)

	blocked := []string{
		"127.0.0.1", "::1", // loopback
		"169.254.169.254", "fe80::1", // link-local (cloud IMDS, IPv6)
		"::ffff:169.254.169.254",                // IPv4-mapped IMDS (normalized before the range checks)
		"10.0.0.5", "192.168.1.1", "172.16.0.1", // RFC1918
		"100.64.0.1", "100.127.255.255", // RFC6598 carrier-grade NAT (in-cluster on EKS)
		"64:ff9b::a9fe:a9fe", "64:ff9b::808:808", // RFC6052 NAT64 (DNS64/NAT64 reaches IMDS)
		"fc00::1",       // IPv6 unique-local
		"0.0.0.0", "::", // unspecified
		"224.0.0.1", // multicast
	}
	for _, s := range blocked {
		g.Expect(isBlockedMetadataIP(net.ParseIP(s))).To(BeTrue(), s)
	}

	// Addresses just outside the blocked ranges stay public — the guard must not
	// over-block (64:ff9c::1 sits one hextet past the NAT64 /96 prefix).
	allowed := []string{
		"8.8.8.8", "93.184.216.34", "100.63.255.255", "100.128.0.0",
		"2606:2800:220:1:248:1893:25c8:1946", "64:ff9c::1",
	}
	for _, s := range allowed {
		g.Expect(isBlockedMetadataIP(net.ParseIP(s))).To(BeFalse(), s)
	}
}

func TestBlockMetadataDial(t *testing.T) {
	g := NewGomegaWithT(t)

	// Cloud-metadata and in-cluster addresses are refused after resolution.
	g.Expect(blockMetadataDial("tcp", "169.254.169.254:80", nil)).To(MatchError(errProviderMetadataUnavailable))
	g.Expect(blockMetadataDial("tcp", "10.0.0.5:443", nil)).To(MatchError(errProviderMetadataUnavailable))
	// A NAT64 address a DNS64 answer would resolve to IMDS is refused.
	g.Expect(blockMetadataDial("tcp", "[64:ff9b::a9fe:a9fe]:443", nil)).To(MatchError(errProviderMetadataUnavailable))
	// A public address is allowed through.
	g.Expect(blockMetadataDial("tcp", "8.8.8.8:443", nil)).NotTo(HaveOccurred())
	// A malformed address is refused rather than silently dialed.
	g.Expect(blockMetadataDial("tcp", "not-an-address", nil)).To(MatchError(errProviderMetadataUnavailable))
}

func TestNewHardenedMetadataClient_DoesNotFollowRedirects(t *testing.T) {
	g := NewGomegaWithT(t)
	c := newHardenedMetadataClient()
	g.Expect(c.CheckRedirect).NotTo(BeNil())
	g.Expect(c.CheckRedirect(nil, nil)).To(Equal(http.ErrUseLastResponse))
}

// TestRenderOIDCBackend_DiscoveryModeEgressPortsFromDocument pins the
// discovery-mode egress fix: the sidecar dials the endpoints named in the
// fetched .provider document (never re-resolving the issuer host), whose ports
// can differ from the issuer's, so the derived egress ports must include them —
// the NetworkPolicy egress rule is port-only.
func TestRenderOIDCBackend_DiscoveryModeEgressPortsFromDocument(t *testing.T) {
	g := NewGomegaWithT(t)
	backend := testProjectableOIDCBackend("corp-oidc")
	backend.Spec.OIDC.Endpoints = nil // discovery mode
	backend.Spec.OIDC.Issuer = "https://idp.example.com/realms/forge"

	r := newTestReconciler(testOIDCClientSecret("corp-oidc"))
	r.HTTPClient = &metadataDoer{body: `{` +
		`"issuer":"https://idp.example.com/realms/forge",` +
		`"authorization_endpoint":"https://idp.example.com/realms/forge/auth",` +
		`"token_endpoint":"https://tokens.example.com:9443/token",` +
		`"jwks_uri":"https://keys.example.com:7443/certs"}`}

	render, err := r.renderOIDCBackend(context.Background(), testFederationKeystone(), backend)
	g.Expect(err).NotTo(HaveOccurred())
	// 443 from the issuer + authorization_endpoint, plus the endpoint-specific
	// ports the sidecar would otherwise be blocked from reaching.
	g.Expect(render.egressPorts).To(ConsistOf(int32(443), int32(9443), int32(7443)))
}

// TestProviderMetadataEgressPorts covers the discovery-document port extraction:
// scheme defaulting, empty-endpoint skipping, and the nil-on-invalid-JSON path.
func TestProviderMetadataEgressPorts(t *testing.T) {
	g := NewGomegaWithT(t)

	doc := []byte(`{"authorization_endpoint":"https://idp.example.com/auth",` +
		`"token_endpoint":"http://idp.example.com/token",` +
		`"introspection_endpoint":"https://idp.example.com:8443/introspect"}`)
	// https→443, http→80, explicit 8443; jwks_uri/userinfo/end_session absent.
	g.Expect(providerMetadataEgressPorts(doc)).To(Equal([]int32{443, 80, 8443}))

	g.Expect(providerMetadataEgressPorts([]byte("not json"))).To(BeNil())
	g.Expect(providerMetadataEgressPorts([]byte(`{}`))).To(BeNil())
}

func TestReconcileIdentityBackends_OIDCRendersFederationSecret(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	ks := testFederationKeystone()
	backend := testProjectableOIDCBackend("corp-oidc")
	backend.Spec.OIDC.OAuth2Introspection = &keystonev1alpha1.OIDCIntrospectionSpec{Enabled: true}
	backend.Spec.OIDC.Endpoints.IntrospectionEndpoint = "https://idp.example.com/introspect"
	r := newTestReconciler(ks, backend, testOIDCClientSecret("corp-oidc"))

	projection, err := r.reconcileIdentityBackends(ctx, ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(projection.DomainsSecretName).To(BeEmpty(), "no LDAP backend is attached")
	g.Expect(projection.Federation).NotTo(BeNil())
	g.Expect(projection.Federation.SecretName).To(HavePrefix("test-keystone-federation-"))
	g.Expect(projection.Federation.RemoteIDAttribute).To(Equal("HTTP_OIDC_ISS"))
	g.Expect(projection.Federation.ProxyImage.Repository).To(Equal("ghcr.io/c5c3/keystone-federation-proxy"))

	// The Secret carries proxy.conf plus the three per-backend documents
	// under '%'-free keys; KeyToPath maps them to the real metadata names.
	var secret corev1.Secret
	g.Expect(r.Client.Get(ctx, client.ObjectKey{Namespace: "default", Name: projection.Federation.SecretName}, &secret)).To(Succeed())
	g.Expect(secret.Data).To(HaveKey("proxy.conf"))
	g.Expect(secret.Data).To(HaveKey("corp-oidc.provider"))
	g.Expect(secret.Data).To(HaveKey("corp-oidc.client"))
	g.Expect(secret.Data).To(HaveKey("corp-oidc.conf"))
	// The trailing newline of the Secret-sourced client secret is trimmed.
	g.Expect(string(secret.Data["corp-oidc.client"])).To(ContainSubstring(`"client_secret":"rp-secret"`))
	g.Expect(string(secret.Data["corp-oidc.conf"])).To(ContainSubstring(`"scope":"openid email profile"`))

	basename := issuerToMetadataBasename("https://idp.example.com/realms/forge")
	g.Expect(projection.Federation.MetadataItems).To(ContainElement(corev1.KeyToPath{
		Key: "corp-oidc.provider", Path: basename + ".provider",
	}))
	g.Expect(projection.Federation.MetadataItems).To(ContainElement(corev1.KeyToPath{
		Key: "corp-oidc.client", Path: basename + ".client",
	}))

	// The crypto passphrase Secret is stable-named and owner-referenced.
	var passSecret corev1.Secret
	g.Expect(r.Client.Get(ctx, client.ObjectKey{Namespace: "default", Name: "test-keystone-oidc-crypto-passphrase"}, &passSecret)).To(Succeed())
	g.Expect(passSecret.Data["passphrase"]).NotTo(BeEmpty())
	g.Expect(passSecret.OwnerReferences).To(HaveLen(1))

	cond := commonconditions.GetCondition(ks.Status.Conditions, conditionTypeIdentityBackendsReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonAllBackendsProjected))

	// A second pass reuses the passphrase, so the content hash is stable.
	projection2, err := r.reconcileIdentityBackends(ctx, ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(projection2.Federation.SecretName).To(Equal(projection.Federation.SecretName))
}

// Two OIDC backends sharing an issuer would render colliding KeyToPath mount
// paths in the federation Secret volume (mod_auth_openidc keys metadata files
// on the issuer). The webhook enforces identityProviderName uniqueness but not
// issuer uniqueness, so a bypassed CR can reach the reconciler — which must
// skip the colliding set rather than emit a duplicate-path volume that wedges
// the Deployment.
func TestReconcileIdentityBackends_SameIssuerSkipsCollidingSet(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	ks := testFederationKeystone()
	// Distinct names/domains/idpNames but the same issuer (from testOIDCBackend).
	a := testProjectableOIDCBackend("alpha-oidc")
	b := testProjectableOIDCBackend("beta-oidc")
	g.Expect(a.Spec.OIDC.Issuer).To(Equal(b.Spec.OIDC.Issuer), "fixture precondition: shared issuer")
	r := newTestReconciler(ks, a, b, testOIDCClientSecret("alpha-oidc"), testOIDCClientSecret("beta-oidc"))

	projection, err := r.reconcileIdentityBackends(ctx, ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(projection.Federation).To(BeNil(), "the colliding same-issuer set must not be projected")

	cond := commonconditions.GetCondition(ks.Status.Conditions, conditionTypeIdentityBackendsReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForBackends))
	g.Expect(cond.Message).To(ContainSubstring("metadata filename"))
}

// A discovery-based backend must ride out a transient IdP metadata-endpoint
// outage that coincides with a cache miss (e.g. an operator restart) by
// reusing the last-known-good discovery document persisted in the federation
// Secret, rather than tearing federation down.
func TestReconcileIdentityBackends_MetadataOutageReusesLastKnownGood(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	ks := testFederationKeystone()
	backend := testProjectableOIDCBackend("corp-oidc")
	backend.Spec.OIDC.Endpoints = nil // force metadata discovery
	backend.Spec.OIDC.ProviderMetadataURL = "https://idp.example.com/realms/forge/.well-known/openid-configuration"
	doer := &metadataDoer{body: `{"issuer":"https://idp.example.com/realms/forge","token_endpoint":"https://idp.example.com/token"}`}
	r := newTestReconciler(ks, backend, testOIDCClientSecret("corp-oidc"))
	r.HTTPClient = doer

	// First reconcile fetches and persists the discovery document.
	first, err := r.reconcileIdentityBackends(ctx, ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(first.Federation).NotTo(BeNil())

	// Simulate an operator restart (drop the in-memory cache) and an IdP outage.
	r.federationMetadataCache = nil
	doer.fail = true

	second, err := r.reconcileIdentityBackends(ctx, ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(second.Federation).NotTo(BeNil(), "federation must not be torn down by a transient metadata outage")
	g.Expect(second.Federation.SecretName).To(Equal(first.Federation.SecretName), "last-known-good metadata reproduces the same Secret")
	expectEvent(g, r, "Warning FederationMetadataStale")

	// The fallback seeds the cache, so a subsequent reconcile serves from it
	// without re-hitting the still-failing IdP.
	callsBefore := doer.calls
	third, err := r.reconcileIdentityBackends(ctx, ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(third.Federation).NotTo(BeNil())
	g.Expect(doer.calls).To(Equal(callsBefore), "cache seeded by the fallback prevents re-hammering the IdP")
}

func TestReconcileIdentityBackends_MixedLDAPAndOIDC(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	ks := testFederationKeystone()
	ldap := testIdentityBackend("corp-ldap", "corp")
	oidc := testProjectableOIDCBackend("corp-oidc")
	r := newTestReconciler(ks, ldap, oidc, testBindSecret("corp-ldap"), testOIDCClientSecret("corp-oidc"))

	projection, err := r.reconcileIdentityBackends(ctx, ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(projection.DomainsSecretName).To(HavePrefix("test-keystone-domains-"))
	g.Expect(projection.Federation).NotTo(BeNil())

	var domains corev1.Secret
	g.Expect(r.Client.Get(ctx, client.ObjectKey{Namespace: "default", Name: projection.DomainsSecretName}, &domains)).To(Succeed())
	g.Expect(domains.Data).To(HaveKey(domainConfFileName("corp")))
	g.Expect(domains.Data).NotTo(HaveKey("corp-oidc.provider"), "federation artifacts must not leak into the domains Secret")
}

func TestReconcileIdentityBackends_OIDCWithoutProxyImageStaysPending(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	ks := testKeystone() // no spec.federation
	backend := testProjectableOIDCBackend("corp-oidc")
	r := newTestReconciler(ks, backend, testOIDCClientSecret("corp-oidc"))

	projection, err := r.reconcileIdentityBackends(ctx, ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(projection.Federation).To(BeNil(), "no sidecar may be projected without an image")

	cond := commonconditions.GetCondition(ks.Status.Conditions, conditionTypeIdentityBackendsReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForBackends))
	g.Expect(cond.Message).To(ContainSubstring("spec.federation.proxyImage not set"))
	expectEvent(g, r, "Warning FederationProxyImageMissing")
}

func TestReconcileIdentityBackends_OIDCMissingClientSecretSkips(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	ks := testFederationKeystone()
	backend := testProjectableOIDCBackend("corp-oidc")
	r := newTestReconciler(ks, backend) // client Secret deliberately absent

	projection, err := r.reconcileIdentityBackends(ctx, ks)
	g.Expect(err).NotTo(HaveOccurred(), "a missing client Secret is a per-backend fault, not a pipeline failure")
	g.Expect(projection.Federation).To(BeNil())

	cond := commonconditions.GetCondition(ks.Status.Conditions, conditionTypeIdentityBackendsReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	expectEvent(g, r, "Warning IdentityBackendSkipped")
}

func TestReconcileIdentityBackends_OIDCControlCharSkips(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	ks := testFederationKeystone()
	backend := testProjectableOIDCBackend("corp-oidc")
	// A CRD-bypass CR could smuggle a newline into a value the proxy.conf
	// embeds — the renderer is the last line of defense.
	backend.Spec.OIDC.RemoteIDAttribute = "HTTP_OIDC_ISS\nOIDCOAuthClientSecret pwned"
	r := newTestReconciler(ks, backend, testOIDCClientSecret("corp-oidc"))

	projection, err := r.reconcileIdentityBackends(ctx, ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(projection.Federation).To(BeNil())
	expectEvent(g, r, "Warning IdentityBackendSkipped")
}

func TestIdentityBackendSecretNameExtractor_OIDCClientSecret(t *testing.T) {
	g := NewGomegaWithT(t)

	oidc := testProjectableOIDCBackend("corp-oidc")
	g.Expect(identityBackendSecretNameExtractor(oidc)).To(ConsistOf("corp-oidc-client"))

	ldap := testIdentityBackend("corp-ldap", "corp")
	g.Expect(identityBackendSecretNameExtractor(ldap)).To(ConsistOf("corp-ldap-bind"))

	// Nil-safe on an empty spec.
	g.Expect(identityBackendSecretNameExtractor(&keystonev1alpha1.KeystoneIdentityBackend{})).To(BeEmpty())
}

// TestRenderOIDCBackend_HTTPIntrospectionEndpointSkips pins the sidecar
// crash-loop guard: mod_auth_openidc rejects http introspection endpoints at
// Apache config-parse time, so a metadata-derived http endpoint (the shape
// the webhook cannot see) must degrade to a per-backend skip.
func TestRenderOIDCBackend_HTTPIntrospectionEndpointSkips(t *testing.T) {
	g := NewGomegaWithT(t)
	backend := testProjectableOIDCBackend("corp-oidc")
	backend.Spec.OIDC.Endpoints = nil
	backend.Spec.OIDC.Issuer = "http://idp.example.com/realms/forge"
	backend.Spec.OIDC.OAuth2Introspection = &keystonev1alpha1.OIDCIntrospectionSpec{Enabled: true}

	r := newTestReconciler(testOIDCClientSecret("corp-oidc"))
	r.HTTPClient = &metadataDoer{body: `{"issuer":"http://idp.example.com/realms/forge","introspection_endpoint":"http://idp.example.com/realms/forge/introspect"}`}

	ks := testFederationKeystone()
	_, err := r.renderOIDCBackend(context.Background(), ks, backend)
	g.Expect(err).To(MatchError(errProviderMetadataUnavailable))
	g.Expect(err.Error()).To(ContainSubstring("is not https"))
}
