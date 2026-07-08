// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/c5c3/forge/internal/common/config"
	"github.com/c5c3/forge/internal/common/secrets"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// federationProxyPort is the mod_auth_openidc sidecar's listen port. When
// federation is active the Service targetPort switches here so every request
// traverses the header-stripping proxy before reaching the localhost-bound
// uWSGI on 5000.
const federationProxyPort int32 = 5050

// federationRedirectURIPath is the module-owned redirect endpoint
// (OIDCRedirectURI). It lives under /v3/OS-FEDERATION/ so no real keystone
// route is shadowed.
const federationRedirectURIPath = "/v3/OS-FEDERATION/redirect_uri"

// uwsgiFederationBufferSize is appended as --buffer-size when federation is
// active: the spike measured claim headers plus the ~4 KiB client-cookie
// session blowing uWSGI's 4096-byte default (502s), and 64 KiB is uWSGI's
// documented maximum.
const uwsgiFederationBufferSize = 65535

// maxProviderMetadataBytes bounds a fetched OIDC discovery document. Real
// documents are a few KiB; the bound keeps a misbehaving IdP from bloating
// the federation Secret.
const maxProviderMetadataBytes = 256 * 1024

// federationMetadataFetchTimeout bounds the discovery-document GET. The
// reconcile context carries no deadline by default, so without this a slow or
// byte-withholding IdP (or an SSRF target) that completes the TCP handshake
// and then trickles bytes would pin the reconcile worker indefinitely — the
// health-check path bounds its probe the same way (HealthCheckTimeout).
const federationMetadataFetchTimeout = 10 * time.Second

// errProviderMetadataUnavailable classifies discovery-document failures
// (unreachable metadata URL, non-2xx, malformed JSON, issuer mismatch) as
// per-backend faults: the caller skips the backend and warns instead of
// failing the whole pipeline, mirroring the missing-bind-Secret handling.
var errProviderMetadataUnavailable = errors.New("provider metadata unavailable")

// federationProjection carries what the downstream builders need when at
// least one OIDC backend is projected: the content-hashed federation Secret,
// the KeyToPath items mapping its safe data keys onto the real
// mod_auth_openidc metadata filenames, the (webhook-uniform) remote-id
// attribute for keystone.conf, and the sidecar image.
type federationProjection struct {
	SecretName        string
	MetadataItems     []corev1.KeyToPath
	RemoteIDAttribute string
	ProxyImage        commonv1.ImageSpec
}

// identityBackendsProjection is reconcileIdentityBackends' aggregate result:
// the domains Secret name ("" when no LDAP backend is projected) and the
// federation projection (nil when no OIDC backend is projected).
type identityBackendsProjection struct {
	DomainsSecretName string
	Federation        *federationProjection
}

// oidcRender is one OIDC backend's rendered contribution to the federation
// Secret plus the parameters the proxy.conf assembly needs.
type oidcRender struct {
	backendName       string
	idpName           string
	protocolID        string
	issuer            string
	metadataBasename  string
	provider          []byte
	client            []byte
	conf              []byte
	introspection     bool
	introspectionEP   string
	clientID          string
	clientSecret      string
	sessionType       string
	stateInputHeaders string
	stripHeaders      []string
}

// federationSecretBaseName returns the content-hashed federation Secret's
// base name for a Keystone CR.
func federationSecretBaseName(keystone *keystonev1alpha1.Keystone) string {
	return keystone.Name + "-federation"
}

// oidcCryptoPassphraseSecretName returns the stable-named Secret carrying the
// operator-generated OIDCCryptoPassphrase.
func oidcCryptoPassphraseSecretName(keystone *keystonev1alpha1.Keystone) string {
	return keystone.Name + "-oidc-crypto-passphrase"
}

// federationProxyImage resolves spec.federation.proxyImage, returning nil
// when no usable image is configured (no hidden default is assumed — the
// managed ControlPlane path projects one, standalone installations set it).
func federationProxyImage(keystone *keystonev1alpha1.Keystone) *commonv1.ImageSpec {
	if keystone.Spec.Federation == nil || keystone.Spec.Federation.ProxyImage == nil ||
		keystone.Spec.Federation.ProxyImage.Repository == "" {
		return nil
	}
	return keystone.Spec.Federation.ProxyImage
}

// issuerToMetadataBasename converts an issuer URL into mod_auth_openidc's
// OIDCMetadataDir file basename: scheme stripped, trailing slash trimmed,
// every non-unreserved byte RFC3986 percent-escaped with uppercase hex —
// producing the <host>%3A<port>%2F<path> shape the federation spike pinned
// against the module's own oidc_metadata_issuer_to_filename.
func issuerToMetadataBasename(issuer string) string {
	s := issuer
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	s = strings.TrimRight(s, "/")
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '-', c == '.', c == '_', c == '~':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// claimStripHeaders derives the inbound request headers the proxy must unset
// so in-cluster clients cannot spoof claims past the module: for every WSGI
// environ key keystone consumes (the remote-id attribute plus every mapping
// remote type), both the dash and the underscore header spelling are
// stripped — Apache/uWSGI normalize both to the same HTTP_* key, and the
// module scrubs inbound claim headers only on module-protected paths (the
// spike's spoofing finding). Env names without the HTTP_ prefix cannot be
// injected via headers and are skipped. The result is deduplicated and
// sorted so the rendered proxy.conf is deterministic.
func claimStripHeaders(remoteIDAttribute string, mappings []keystonev1alpha1.MappingRuleSpec) []string {
	envNames := []string{remoteIDAttribute}
	for i := range mappings {
		for j := range mappings[i].Remote {
			envNames = append(envNames, mappings[i].Remote[j].Type)
		}
	}
	seen := map[string]struct{}{}
	var headers []string
	add := func(h string) {
		if _, ok := seen[h]; !ok {
			seen[h] = struct{}{}
			headers = append(headers, h)
		}
	}
	for _, env := range envNames {
		base, ok := strings.CutPrefix(env, "HTTP_")
		if !ok || base == "" {
			continue
		}
		add(base)                               // underscore spelling
		add(strings.ReplaceAll(base, "_", "-")) // dash spelling
	}
	sort.Strings(headers)
	return headers
}

// effectiveOIDCScopes returns spec.oidc.scopes, falling back to the
// documented default set.
func effectiveOIDCScopes(o *keystonev1alpha1.OIDCBackendSpec) []string {
	if len(o.Scopes) > 0 {
		return o.Scopes
	}
	return keystonev1alpha1.DefaultOIDCScopes
}

// effectiveOIDCResponseType returns spec.oidc.responseType, falling back to
// the documented default.
func effectiveOIDCResponseType(o *keystonev1alpha1.OIDCBackendSpec) string {
	if o.ResponseType != "" {
		return o.ResponseType
	}
	return keystonev1alpha1.DefaultOIDCResponseType
}

// effectiveOIDCSessionType returns spec.oidc.sessionType, falling back to
// the documented default.
func effectiveOIDCSessionType(o *keystonev1alpha1.OIDCBackendSpec) string {
	if o.SessionType != "" {
		return string(o.SessionType)
	}
	return string(keystonev1alpha1.OIDCSessionTypeClientCookie)
}

// effectiveOIDCStateInputHeaders returns spec.oidc.stateInputHeaders,
// falling back to the documented default.
func effectiveOIDCStateInputHeaders(o *keystonev1alpha1.OIDCBackendSpec) string {
	if o.StateInputHeaders != "" {
		return string(o.StateInputHeaders)
	}
	return string(keystonev1alpha1.OIDCStateInputHeadersNone)
}

// validateOIDCRenderInputs re-validates every spec value the render embeds
// into the Apache configuration for newline/carriage-return injection. The
// webhook rejects these up front, but the renderer is the only gate that
// still runs when a CR bypassed admission — mirroring the LDAP renderer's
// last-line-of-defense contract.
func validateOIDCRenderInputs(backend *keystonev1alpha1.KeystoneIdentityBackend) error {
	o := backend.Spec.OIDC
	values := []string{
		o.Issuer, o.ProviderMetadataURL, o.ClientID,
		backend.EffectiveIdentityProviderName(), backend.EffectiveOIDCProtocolID(),
		backend.EffectiveRemoteIDAttribute(),
		effectiveOIDCResponseType(o),
		effectiveOIDCSessionType(o), effectiveOIDCStateInputHeaders(o),
	}
	values = append(values, effectiveOIDCScopes(o)...)
	for i := range backend.Spec.Mappings {
		for j := range backend.Spec.Mappings[i].Remote {
			values = append(values, backend.Spec.Mappings[i].Remote[j].Type)
		}
	}
	for _, v := range values {
		// A double-quote must be rejected alongside newline/CR: even though the
		// render quotes clientID/introspectionEP with %q (which escapes it), this
		// backstop matches the webhook's OIDC checkNoCtrl so a CR that bypassed
		// admission cannot smuggle a quote into a value rendered unquoted elsewhere.
		if strings.ContainsAny(v, "\n\r\"") {
			return fmt.Errorf("oidc render input %q: %w", v, errControlCharInValue)
		}
	}
	return nil
}

// renderExplicitProviderMetadata assembles the OIDC discovery document from
// spec.oidc.endpoints for air-gapped operators whose metadata URL is
// unreachable from the operator pod.
func renderExplicitProviderMetadata(o *keystonev1alpha1.OIDCBackendSpec) ([]byte, error) {
	doc := map[string]string{
		"issuer":                 o.Issuer,
		"authorization_endpoint": o.Endpoints.AuthorizationEndpoint,
		"token_endpoint":         o.Endpoints.TokenEndpoint,
		"jwks_uri":               o.Endpoints.JWKSURI,
	}
	if o.Endpoints.UserinfoEndpoint != "" {
		doc["userinfo_endpoint"] = o.Endpoints.UserinfoEndpoint
	}
	if o.Endpoints.EndSessionEndpoint != "" {
		doc["end_session_endpoint"] = o.Endpoints.EndSessionEndpoint
	}
	if o.Endpoints.IntrospectionEndpoint != "" {
		doc["introspection_endpoint"] = o.Endpoints.IntrospectionEndpoint
	}
	payload, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshaling provider metadata: %w", err)
	}
	return payload, nil
}

// federationMetadataCacheEntry memoizes one backend's fetched discovery
// document, keyed on (uid, generation) so a spec edit refetches while the
// steady-state reconcile cadence never hammers the IdP. An operator restart
// drops the cache and refetches once — accepted (the document is stable, and
// a re-ordered JSON body merely re-hashes the federation Secret once).
type federationMetadataCacheEntry struct {
	uid        types.UID
	generation int64
	document   []byte
}

// cgnatCIDR is the RFC 6598 carrier-grade NAT range (100.64.0.0/10). Go's
// net.IP.IsPrivate covers only RFC1918 / RFC4193, so this range — routinely
// used for in-cluster Pod and Service networking on managed Kubernetes (e.g.
// EKS) — must be blocked explicitly or the discovery dial guard leaves an
// SSRF hole to those in-cluster addresses.
var _, cgnatCIDR, _ = net.ParseCIDR("100.64.0.0/10")

// nat64CIDR is the RFC 6052 well-known NAT64 prefix (64:ff9b::/96). It is IPv6
// global unicast — IsPrivate/IsLinkLocalUnicast are false and cgnatCIDR is an
// IPv4 net — so a NAT64 address like 64:ff9b::a9fe:a9fe slips past every other
// guard. On IPv6-single-stack / dual-stack managed clusters a DNS64/NAT64
// gateway translates it to 169.254.169.254, reaching cloud IMDS through the
// operator pod, so the prefix must be blocked explicitly.
var _, nat64CIDR, _ = net.ParseCIDR("64:ff9b::/96")

// isBlockedMetadataIP reports whether ip is a non-public address the operator
// must never issue the discovery GET against: loopback, link-local (cloud IMDS
// lives at 169.254.169.254), private (RFC1918 / IPv6 unique-local),
// carrier-grade NAT (RFC6598 100.64.0.0/10), the NAT64 well-known prefix
// (RFC6052 64:ff9b::/96), unspecified, or multicast.
func isBlockedMetadataIP(ip net.IP) bool {
	if v4 := ip.To4(); v4 != nil { // normalize IPv4-mapped before range checks
		ip = v4
	}
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() || ip.IsPrivate() ||
		cgnatCIDR.Contains(ip) || nat64CIDR.Contains(ip)
}

// blockMetadataDial is the net.Dialer.Control SSRF guard for the discovery
// fetch. It runs after DNS resolution with the concrete IP:port being dialed,
// so it rejects a providerMetadataURL — or a DNS-rebinding answer — that
// targets an in-cluster or cloud-metadata address the untrusted CR creator
// must not be able to reach through the operator pod.
func blockMetadataDial(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%w: parsing dial address %q: %w", errProviderMetadataUnavailable, address, err)
	}
	ip := net.ParseIP(host)
	if ip == nil || isBlockedMetadataIP(ip) {
		return fmt.Errorf("%w: refusing to dial non-public address %s", errProviderMetadataUnavailable, address)
	}
	return nil
}

// newHardenedMetadataClient builds the HTTP client the operator uses to fetch
// an OIDC discovery document from a namespaced CR's providerMetadataURL. Two
// SSRF controls: CheckRedirect refuses to follow redirects (a 302 to an
// internal target must not be chased past the dial guard — it surfaces as a
// non-2xx and the backend is skipped), and the dialer Control rejects
// connections to non-public addresses. No proxy is configured so the guard
// always inspects the real target (operators whose IdP is only reachable via a
// proxy use spec.oidc.endpoints); DisableKeepAlives keeps the single-use
// client from leaving an idle connection behind.
func newHardenedMetadataClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{
			DisableKeepAlives: true,
			DialContext: (&net.Dialer{
				Timeout: federationMetadataFetchTimeout,
				Control: blockMetadataDial,
			}).DialContext,
		},
	}
}

// federationMetadataClient returns the injected HTTPDoer (test seam) or a
// hardened, SSRF-guarded client for the discovery fetch. The shared
// httpClient() seam is deliberately not reused here: its default
// http.DefaultClient follows redirects and dials any address, and the
// health-check path legitimately targets an in-cluster (private) Service.
func (r *KeystoneReconciler) federationMetadataClient() HTTPDoer {
	if r.HTTPClient != nil {
		return r.HTTPClient
	}
	return newHardenedMetadataClient()
}

// fetchProviderMetadata GETs the backend's discovery document through the
// injectable HTTP seam, validates that the document's issuer equals the
// spec issuer (a mismatched document would silently bind the login flow to a
// foreign provider), and memoizes the result per (uid, generation).
func (r *KeystoneReconciler) fetchProviderMetadata(ctx context.Context, backend *keystonev1alpha1.KeystoneIdentityBackend) ([]byte, error) {
	key := client.ObjectKeyFromObject(backend)
	r.federationMetadataCacheMu.Lock()
	entry, ok := r.federationMetadataCache[key]
	r.federationMetadataCacheMu.Unlock()
	if ok && entry.uid == backend.UID && entry.generation == backend.Generation {
		return entry.document, nil
	}

	metadataURL := backend.Spec.OIDC.ProviderMetadataURL
	if metadataURL == "" {
		// The defaulting webhook materializes this; derive again for CRs that
		// bypassed it.
		metadataURL = strings.TrimRight(backend.Spec.OIDC.Issuer, "/") + "/.well-known/openid-configuration"
	}

	fetchCtx, cancel := context.WithTimeout(ctx, federationMetadataFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, metadataURL, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: building request for %s: %v", errProviderMetadataUnavailable, metadataURL, err)
	}
	resp, err := r.federationMetadataClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: fetching %s: %v", errProviderMetadataUnavailable, metadataURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%w: fetching %s: HTTP %d", errProviderMetadataUnavailable, metadataURL, resp.StatusCode)
	}
	document, err := io.ReadAll(io.LimitReader(resp.Body, maxProviderMetadataBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: reading %s: %v", errProviderMetadataUnavailable, metadataURL, err)
	}
	if len(document) > maxProviderMetadataBytes {
		return nil, fmt.Errorf("%w: %s exceeds the %d-byte budget", errProviderMetadataUnavailable, metadataURL, maxProviderMetadataBytes)
	}

	var doc struct {
		Issuer string `json:"issuer"`
	}
	if err := json.Unmarshal(document, &doc); err != nil {
		return nil, fmt.Errorf("%w: decoding %s: %v", errProviderMetadataUnavailable, metadataURL, err)
	}
	if doc.Issuer != backend.Spec.OIDC.Issuer {
		// Deliberately does not echo doc.Issuer: this error surfaces in a
		// tenant-visible IdentityBackendSkipped Event, so reflecting the
		// fetched document's content would turn the metadata fetch into an
		// SSRF read oracle — point providerMetadataURL at any
		// operator-reachable host:port and read its response's "issuer" JSON
		// field back. Only the tenant's own spec issuer is echoed.
		return nil, fmt.Errorf("%w: document issuer does not match spec issuer %q",
			errProviderMetadataUnavailable, backend.Spec.OIDC.Issuer)
	}

	r.cacheProviderMetadata(backend, document)
	return document, nil
}

// cacheProviderMetadata memoizes a discovery document for backend under its
// current (uid, generation), so the next steady-state reconcile serves from
// the cache instead of re-hitting the IdP. Used both by fetchProviderMetadata
// on a successful fetch and by the last-known-good fallback below.
func (r *KeystoneReconciler) cacheProviderMetadata(backend *keystonev1alpha1.KeystoneIdentityBackend, document []byte) {
	key := client.ObjectKeyFromObject(backend)
	r.federationMetadataCacheMu.Lock()
	defer r.federationMetadataCacheMu.Unlock()
	if r.federationMetadataCache == nil {
		r.federationMetadataCache = make(map[types.NamespacedName]federationMetadataCacheEntry)
	}
	r.federationMetadataCache[key] = federationMetadataCacheEntry{
		uid:        backend.UID,
		generation: backend.Generation,
		document:   document,
	}
}

// providerMetadataIssuer leniently extracts the issuer from a discovery
// document, returning "" when the bytes are absent or not valid JSON.
func providerMetadataIssuer(document []byte) string {
	var doc struct {
		Issuer string `json:"issuer"`
	}
	if err := json.Unmarshal(document, &doc); err != nil {
		return ""
	}
	return doc.Issuer
}

// lastKnownGoodProviderMetadata returns the discovery document a prior
// successful reconcile persisted for backend in the newest federation Secret,
// or nil when none exists. It lets a discovery-based backend ride out a
// transient IdP metadata-endpoint outage on a cache miss (e.g. an operator
// restart drops the in-memory cache) instead of tearing federation down: the
// document is stable, so the last-known-good copy is a safe stand-in until the
// IdP recovers.
func (r *KeystoneReconciler) lastKnownGoodProviderMetadata(ctx context.Context, keystone *keystonev1alpha1.Keystone, backend *keystonev1alpha1.KeystoneIdentityBackend) []byte {
	baseName := federationSecretBaseName(keystone)
	var list corev1.SecretList
	if err := r.List(ctx, &list, client.InNamespace(keystone.Namespace),
		client.MatchingLabels{config.ConfigBaseLabelKey: baseName}); err != nil {
		return nil
	}
	prefix := baseName + "-"
	var newest *corev1.Secret
	for i := range list.Items {
		s := &list.Items[i]
		if !strings.HasPrefix(s.Name, prefix) {
			continue
		}
		if ref := metav1.GetControllerOf(s); ref == nil || ref.UID != keystone.UID {
			continue
		}
		if newest == nil {
			newest = s
			continue
		}
		// Newest wins; a name tie-break keeps same-second selection stable.
		st, nt := s.CreationTimestamp.Time, newest.CreationTimestamp.Time
		if st.After(nt) || (st.Equal(nt) && s.Name > newest.Name) {
			newest = s
		}
	}
	if newest == nil {
		return nil
	}
	if doc := newest.Data[backend.Name+".provider"]; len(doc) > 0 {
		return doc
	}
	return nil
}

// introspectionEndpointFromMetadata extracts introspection_endpoint from a
// discovery document ("" when absent).
func introspectionEndpointFromMetadata(document []byte) string {
	var doc struct {
		IntrospectionEndpoint string `json:"introspection_endpoint"`
	}
	if err := json.Unmarshal(document, &doc); err != nil {
		return ""
	}
	return doc.IntrospectionEndpoint
}

// renderOIDCBackend renders one OIDC backend's federation artifacts: the
// pre-provisioned .provider discovery document (the read-only projection
// prevents the module's self-caching), the .client credentials document, and
// the per-provider .conf tuning document.
func (r *KeystoneReconciler) renderOIDCBackend(ctx context.Context, keystone *keystonev1alpha1.Keystone, backend *keystonev1alpha1.KeystoneIdentityBackend) (oidcRender, error) {
	o := backend.Spec.OIDC
	if o == nil {
		// The webhook + CEL union rule prevent this; fail loudly rather than
		// rendering an empty proxy block if admission was bypassed.
		return oidcRender{}, fmt.Errorf("backend %s has type %s but no oidc block", backend.Name, backend.Spec.Type)
	}
	if err := validateOIDCRenderInputs(backend); err != nil {
		return oidcRender{}, err
	}

	secretKey := client.ObjectKey{Namespace: keystone.Namespace, Name: o.ClientSecretRef.Name}
	clientSecret, err := secrets.GetSecretValue(ctx, r.Client, secretKey, "clientSecret")
	if err != nil {
		return oidcRender{}, err
	}
	// Right-trim a trailing CR/LF: a common tooling artifact, semantically not
	// part of the credential (the LDAP bind-credential precedent).
	clientSecret = strings.TrimRight(clientSecret, "\r\n")

	var provider []byte
	if o.Endpoints != nil {
		provider, err = renderExplicitProviderMetadata(o)
	} else {
		provider, err = r.fetchProviderMetadata(ctx, backend)
		if err != nil && errors.Is(err, errProviderMetadataUnavailable) {
			// A transient IdP metadata-endpoint outage on a cache miss (e.g. an
			// operator restart drops the in-memory cache) must not tear
			// federation down. Reuse the last-known-good discovery document a
			// prior reconcile persisted in the federation Secret — but only
			// when its embedded issuer still matches the spec issuer, so a
			// changed issuer never binds the login flow to a stale foreign
			// provider. Seeding the cache stops us from re-hammering the failing
			// IdP each reconcile (a spec edit bumps the generation and forces a
			// real refetch); the Warning surfaces the degraded state.
			if lkg := r.lastKnownGoodProviderMetadata(ctx, keystone, backend); lkg != nil && providerMetadataIssuer(lkg) == o.Issuer {
				r.cacheProviderMetadata(backend, lkg)
				r.Recorder.Eventf(keystone, corev1.EventTypeWarning, "FederationMetadataStale",
					"Identity backend %s: provider metadata unavailable (%v); reusing the last-known-good discovery document so federation stays up", backend.Name, err)
				provider, err = lkg, nil
			}
		}
	}
	if err != nil {
		return oidcRender{}, err
	}

	introspection := o.OAuth2Introspection != nil && o.OAuth2Introspection.Enabled
	var introspectionEP string
	if introspection {
		if o.Endpoints != nil {
			introspectionEP = o.Endpoints.IntrospectionEndpoint
		} else {
			introspectionEP = introspectionEndpointFromMetadata(provider)
		}
		if introspectionEP == "" {
			return oidcRender{}, fmt.Errorf("%w: oauth2Introspection is enabled but the provider metadata carries no introspection_endpoint", errProviderMetadataUnavailable)
		}
	}

	clientDoc, err := json.Marshal(map[string]string{
		"client_id":     o.ClientID,
		"client_secret": clientSecret,
	})
	if err != nil {
		return oidcRender{}, fmt.Errorf("marshaling client document: %w", err)
	}
	confDoc, err := json.Marshal(map[string]string{
		"scope":         strings.Join(effectiveOIDCScopes(o), " "),
		"response_type": effectiveOIDCResponseType(o),
	})
	if err != nil {
		return oidcRender{}, fmt.Errorf("marshaling provider conf document: %w", err)
	}

	return oidcRender{
		backendName:       backend.Name,
		idpName:           backend.EffectiveIdentityProviderName(),
		protocolID:        backend.EffectiveOIDCProtocolID(),
		issuer:            o.Issuer,
		metadataBasename:  issuerToMetadataBasename(o.Issuer),
		provider:          provider,
		client:            clientDoc,
		conf:              confDoc,
		introspection:     introspection,
		introspectionEP:   introspectionEP,
		clientID:          o.ClientID,
		clientSecret:      clientSecret,
		sessionType:       effectiveOIDCSessionType(o),
		stateInputHeaders: effectiveOIDCStateInputHeaders(o),
		stripHeaders:      claimStripHeaders(backend.EffectiveRemoteIDAttribute(), backend.Spec.Mappings),
	}, nil
}

// federationRedirectURI returns the absolute redirect URI clients are sent
// back to — the user-facing endpoint (gateway/public when configured,
// cluster-local otherwise), which is also where the IdP redirects browsers.
func federationRedirectURI(keystone *keystonev1alpha1.Keystone) string {
	return strings.TrimSuffix(keystoneStatusEndpoint(keystone), "/v3") + federationRedirectURIPath
}

// renderProxyConf assembles the operator-rendered mod_auth_openidc virtual
// host body (included by the image's static httpd-base.conf). Server-level
// module directives come first, then one RequestHeader unset per spoofable
// claim-header spelling, the reverse proxy to the localhost-bound uWSGI, the
// optional OAuth2 resource-server block, and the per-IdP protected
// Locations. The session/state knobs are taken from the first (name-sorted)
// backend — the webhook keeps remoteIDAttribute uniform, and mixed session
// knobs across backends would be server-scoped anyway. renders must be
// non-empty and name-sorted (the caller iterates the sorted backend list).
func renderProxyConf(keystone *keystonev1alpha1.Keystone, renders []oidcRender, passphrase string) []byte {
	redirectURI := federationRedirectURI(keystone)

	// Merge the per-backend strip lists into one deterministic set.
	seen := map[string]struct{}{}
	var stripHeaders []string
	for i := range renders {
		for _, h := range renders[i].stripHeaders {
			if _, ok := seen[h]; !ok {
				seen[h] = struct{}{}
				stripHeaders = append(stripHeaders, h)
			}
		}
	}
	sort.Strings(stripHeaders)

	var b strings.Builder
	w := func(format string, args ...any) {
		fmt.Fprintf(&b, format+"\n", args...)
	}

	w("# Rendered by the keystone operator — do not edit; changes are overwritten.")
	w("OIDCCryptoPassphrase %q", passphrase)
	w("OIDCMetadataDir %s", federationMetadataMountPath)
	w("OIDCRedirectURI %s", federationRedirectURIPath)
	w("OIDCClaimPrefix \"OIDC-\"")
	w("OIDCSessionType %s", renders[0].sessionType)
	w("OIDCStateInputHeaders %s", renders[0].stateInputHeaders)
	// Honor inbound X-Forwarded-Host/X-Forwarded-Proto for the redirect_uri /
	// current-URL computation only when a trusted Gateway is declared
	// (spec.gateway): behind it these headers are how the sidecar reconstructs
	// the public URL, and the Gateway is the trust boundary that must overwrite
	// them. With no declared gateway, in-cluster clients reach the sidecar
	// directly, so a spoofed X-Forwarded-Host would poison the redirect_uri —
	// the directive is omitted and mod_auth_openidc falls back to the request
	// host. See the OIDC federation guide's security note (register an exact
	// redirect_uri at the IdP).
	if keystone.Spec.Gateway != nil {
		w("OIDCXForwardedHeaders X-Forwarded-Host X-Forwarded-Proto")
	}
	w("")
	w("# Strip spoofable claim headers before authentication: the module only")
	w("# scrubs them on module-protected paths, and underscore spellings")
	w("# normalize to the same WSGI keys (validated by the federation spike).")
	for _, h := range stripHeaders {
		w("RequestHeader unset %q early", h)
	}
	w("")
	w("ProxyPreserveHost On")
	w("ProxyPass \"/\" \"http://127.0.0.1:5000/\"")
	w("ProxyPassReverse \"/\" \"http://127.0.0.1:5000/\"")

	for i := range renders {
		if !renders[i].introspection {
			continue
		}
		// mod_auth_openidc's OIDCOAuth* resource-server directives are
		// server-scoped, so at most one backend enables this
		// (webhook-enforced).
		w("")
		w("# OAuth2 resource server: bearer-token introspection for %s", renders[i].idpName)
		// %q so a value carrying a space (clientID has no Pattern marker, and the
		// webhook's control-char check allows spaces) renders as one Apache
		// directive argument — an unquoted "OIDCOAuthClientID my client" is two
		// arguments, an Apache config-parse error that crash-loops the sidecar and
		// (its targetPort owns the Service) takes the Keystone API down cluster-wide.
		w("OIDCOAuthIntrospectionEndpoint %q", renders[i].introspectionEP)
		w("OIDCOAuthClientID %q", renders[i].clientID)
		w("OIDCOAuthClientSecret %q", renders[i].clientSecret)
	}

	for i := range renders {
		rd := &renders[i]
		discoverURL := redirectURI + "?iss=" + url.QueryEscape(rd.issuer)
		authType := "openid-connect"
		if rd.introspection {
			// auth-openidc accepts both the session cookie and IdP-issued
			// bearer tokens on the CLI auth path.
			authType = "auth-openidc"
		}
		w("")
		w("<Location \"/v3/auth/OS-FEDERATION/identity_providers/%s/protocols/%s/websso\">", rd.idpName, rd.protocolID)
		w("    AuthType openid-connect")
		w("    Require valid-user")
		w("    OIDCDiscoverURL %s", discoverURL)
		w("</Location>")
		w("<Location \"/v3/OS-FEDERATION/identity_providers/%s/protocols/%s/auth\">", rd.idpName, rd.protocolID)
		w("    AuthType %s", authType)
		w("    Require valid-user")
		if authType == "openid-connect" {
			w("    OIDCDiscoverURL %s", discoverURL)
		}
		w("</Location>")
	}

	// The module-owned redirect endpoint must be protected so the module
	// handles the authorization-code response.
	w("")
	w("<Location %q>", federationRedirectURIPath)
	w("    AuthType openid-connect")
	w("    Require valid-user")
	w("</Location>")

	// The global websso path can pin a provider only when it is unambiguous;
	// with several backends the per-IdP paths (what Horizon uses) are the
	// supported entry points.
	if len(renders) == 1 {
		w("")
		w("<Location \"/v3/auth/OS-FEDERATION/websso/%s\">", renders[0].protocolID)
		w("    AuthType openid-connect")
		w("    Require valid-user")
		w("    OIDCDiscoverURL %s", redirectURI+"?iss="+url.QueryEscape(renders[0].issuer))
		w("</Location>")
	}

	return []byte(b.String())
}

// ensureOIDCCryptoPassphrase returns the operator-generated
// OIDCCryptoPassphrase, creating the stable-named Secret on first use. The
// passphrase encrypts the client-cookie session/state blobs only — it is
// regenerable (a rotation merely invalidates in-flight login sessions), so
// there is deliberately NO PushSecret/OpenBao backup. Because the passphrase
// is embedded into proxy.conf, a regenerated value re-hashes the federation
// Secret and rolls every pod together.
func (r *KeystoneReconciler) ensureOIDCCryptoPassphrase(ctx context.Context, keystone *keystonev1alpha1.Keystone) (string, error) {
	name := oidcCryptoPassphraseSecretName(keystone)
	key := client.ObjectKey{Namespace: keystone.Namespace, Name: name}
	var secret corev1.Secret
	err := r.Get(ctx, key, &secret)
	if err == nil {
		if v := secret.Data["passphrase"]; len(v) > 0 {
			return string(v), nil
		}
		return "", fmt.Errorf("crypto passphrase Secret %s exists but carries no passphrase key", key)
	}
	if !apierrors.IsNotFound(err) {
		return "", fmt.Errorf("fetching crypto passphrase Secret %s: %w", key, err)
	}

	passphrase, err := generateFernetKey() // 32 random bytes, base64 — exactly the entropy needed here.
	if err != nil {
		return "", fmt.Errorf("generating crypto passphrase: %w", err)
	}
	secret = corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: keystone.Namespace,
			Labels:    commonLabels(keystone),
		},
		Data: map[string][]byte{"passphrase": []byte(passphrase)},
	}
	if err := controllerutil.SetControllerReference(keystone, &secret, r.Scheme); err != nil {
		return "", fmt.Errorf("setting owner reference on crypto passphrase Secret: %w", err)
	}
	if err := r.Create(ctx, &secret); err != nil {
		return "", fmt.Errorf("creating crypto passphrase Secret %s: %w", key, err)
	}
	return passphrase, nil
}

// buildFederationProjection creates the content-hashed federation Secret from
// the rendered OIDC backends and assembles the projection the deployment
// builders consume. renders must be non-empty and name-sorted.
func (r *KeystoneReconciler) buildFederationProjection(ctx context.Context, keystone *keystonev1alpha1.Keystone, renders []oidcRender, remoteIDAttribute string, proxyImage commonv1.ImageSpec) (*federationProjection, error) {
	passphrase, err := r.ensureOIDCCryptoPassphrase(ctx, keystone)
	if err != nil {
		return nil, err
	}

	data := map[string][]byte{
		"proxy.conf": renderProxyConf(keystone, renders, passphrase),
	}
	items := make([]corev1.KeyToPath, 0, 3*len(renders))
	for i := range renders {
		rd := &renders[i]
		// Safe Secret keys ('%' is invalid there) mapped onto the real
		// mod_auth_openidc filenames via KeyToPath at mount time.
		providerKey := rd.backendName + ".provider"
		confKey := rd.backendName + ".conf"
		clientKey := federationClientKeyName(rd.backendName)
		data[providerKey] = rd.provider
		data[clientKey] = rd.client
		data[confKey] = rd.conf
		items = append(
			items,
			corev1.KeyToPath{Key: providerKey, Path: rd.metadataBasename + ".provider"},
			corev1.KeyToPath{Key: clientKey, Path: rd.metadataBasename + ".client"},
			corev1.KeyToPath{Key: confKey, Path: rd.metadataBasename + ".conf"},
		)
	}

	name, err := config.CreateImmutableSecret(ctx, r.Client, r.Scheme, keystone,
		federationSecretBaseName(keystone), keystone.Namespace, data)
	if err != nil {
		return nil, fmt.Errorf("creating federation Secret: %w", err)
	}
	return &federationProjection{
		SecretName:        name,
		MetadataItems:     items,
		RemoteIDAttribute: remoteIDAttribute,
		ProxyImage:        proxyImage,
	}, nil
}

// pruneStaleFederationSecrets removes historical immutable federation
// Secrets past the retain count. When federation is inactive (empty
// currentName) every historical Secret is removed — the last OIDC backend
// detached, so no client secret or passphrase copy may linger.
func (r *KeystoneReconciler) pruneStaleFederationSecrets(ctx context.Context, keystone *keystonev1alpha1.Keystone, federationSecretName string) error {
	retain := defaultConfigMapRetainCount
	if federationSecretName == "" {
		retain = 0
	}
	return config.PruneImmutableSecrets(ctx, r.Client, keystone, config.PruneOptions{
		BaseName:    federationSecretBaseName(keystone),
		Namespace:   keystone.Namespace,
		CurrentName: federationSecretName,
		Retain:      retain,
	})
}
