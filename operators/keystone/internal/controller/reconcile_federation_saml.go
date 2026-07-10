// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"reflect"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/c5c3/forge/internal/common/secrets"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// Shared SAML federation projection vocabulary. Like the OIDC constants in
// reconcile_federation_objects.go, these are defined once and used by both the
// keystone-side projection (buildFederationProjection, buildFederationVolumes)
// and the dedicated backend controller (isConfigProjected), so the
// volume/key/file-name contract holds by construction.
const (
	// federationMellonVolumeName projects the SP key/metadata + IdP metadata
	// into the mod_auth_mellon sidecar.
	federationMellonVolumeName = "federation-mellon"
	// federationMellonMountPath is where the sidecar reads the mellon files.
	federationMellonMountPath = "/etc/keystone-federation-proxy/mellon"

	// The fixed mellon file names inside the mount (also the Secret data keys —
	// they carry no '%', so they are valid Secret keys and map onto themselves).
	samlSPKeyFileName      = "sp-key.pem"
	samlSPCertFileName     = "sp-cert.pem"
	samlSPMetadataFileName = "sp-metadata.xml"
)

// samlIdPMetadataKeyName returns the Secret data key (and mounted file name)
// carrying one backend's IdP metadata. Keyed by backend name so the
// isConfigProjected observation and the projection agree.
func samlIdPMetadataKeyName(backendName string) string {
	return backendName + ".idp-metadata.xml"
}

// samlSPKeypairSecretName returns the stable-named Secret carrying the
// operator-generated SP keypair (kubernetes.io/tls).
func samlSPKeypairSecretName(keystone *keystonev1alpha1.Keystone) string {
	return keystone.Name + "-saml-sp"
}

// samlSPMetadataSecretName returns the stable-named Secret exposing the SP
// metadata for out-of-band IdP registration.
func samlSPMetadataSecretName(keystone *keystonev1alpha1.Keystone) string {
	return keystone.Name + "-saml-sp-metadata"
}

// samlRender is one SAML backend's rendered contribution to the federation
// Secret plus the parameters the proxy.conf assembly needs.
type samlRender struct {
	backendName  string
	idpName      string
	protocolID   string
	idpEntityID  string
	remoteIDAttr string
	idpMetadata  []byte
	spKey        []byte
	spCert       []byte
	spMetadata   []byte
	spEntityID   string
	endpointPath string
	stripHeaders []string
	// forwardEnvs holds the reqenv variable names (e.g. "MELLON_USERNAME") the
	// sidecar forwards to keystone; the request-header spelling is derived at the
	// write site as strings.ReplaceAll(env, "_", "-").
	forwardEnvs []string
}

// samlSPEndpointPath returns the mellon endpoint path for a backend (the shared
// path mod_auth_mellon owns for the SP metadata/ACS/logout endpoints).
func samlSPEndpointPath(backend *keystonev1alpha1.KeystoneIdentityBackend) string {
	return fmt.Sprintf("/v3/OS-FEDERATION/identity_providers/%s/protocols/%s/auth/mellon",
		backend.EffectiveIdentityProviderName(), backend.EffectiveProtocolID())
}

// samlSPEndpointBaseURL returns the absolute mellon endpoint base URL — the
// user-facing endpoint (gateway/public when configured, cluster-local
// otherwise) — under which the SP metadata/ACS/logout endpoints live.
func samlSPEndpointBaseURL(keystone *keystonev1alpha1.Keystone, backend *keystonev1alpha1.KeystoneIdentityBackend) string {
	return strings.TrimSuffix(keystoneStatusEndpoint(keystone), "/v3") + samlSPEndpointPath(backend)
}

// samlStripHeaders derives the inbound header set the proxy must unset for a
// SAML backend: the remote-id attribute and mapping remote types (shared with
// the OIDC derivation) plus the always-populated MELLON_IDP / MELLON_NAME_ID
// and the per-forwardAttribute MELLON_<ATTR> env keys.
func samlStripHeaders(backend *keystonev1alpha1.KeystoneIdentityBackend) []string {
	envNames := []string{backend.EffectiveRemoteIDAttribute(), "HTTP_MELLON_IDP", "HTTP_MELLON_NAME_ID"}
	for i := range backend.Spec.Mappings {
		for j := range backend.Spec.Mappings[i].Remote {
			envNames = append(envNames, backend.Spec.Mappings[i].Remote[j].Type)
		}
	}
	for _, attr := range backend.Spec.SAML.ForwardAttributes {
		envNames = append(envNames, "HTTP_MELLON_"+strings.ToUpper(attr))
	}
	return stripHeadersFromEnvNames(envNames)
}

// validateSAMLRenderInputs re-validates every spec value the render embeds into
// the Apache configuration for newline/carriage-return/double-quote injection.
// The webhook rejects these up front, but the renderer is the only gate that
// still runs when a CR bypassed admission — mirroring validateOIDCRenderInputs.
func validateSAMLRenderInputs(backend *keystonev1alpha1.KeystoneIdentityBackend) error {
	s := backend.Spec.SAML
	values := []string{
		s.IdPEntityID,
		backend.EffectiveIdentityProviderName(), backend.EffectiveProtocolID(),
		backend.EffectiveRemoteIDAttribute(),
	}
	values = append(values, s.ForwardAttributes...)
	for i := range backend.Spec.Mappings {
		for j := range backend.Spec.Mappings[i].Remote {
			values = append(values, backend.Spec.Mappings[i].Remote[j].Type)
		}
	}
	for _, v := range values {
		if strings.ContainsAny(v, "\n\r\"") {
			return fmt.Errorf("saml render input %q: %w", v, errControlCharInValue)
		}
	}
	return nil
}

// generateSelfSignedSPKeypair produces an RSA-2048 self-signed certificate and
// key for the SAML service provider (CN cn, 10-year validity). IdPs pin the SP
// certificate from the SP metadata, so self-signed is the standard shape.
func generateSelfSignedSPKeypair(cn string) (certPEM, keyPEM, certDER []byte, err error) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("generating SP RSA key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("generating SP certificate serial: %w", err)
	}
	now := time.Now()
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("creating SP certificate: %w", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)})
	return certPEM, keyPEM, der, nil
}

// certDERFromPEM extracts the DER bytes of the first CERTIFICATE block in a PEM
// bundle (needed for the base64 X509Certificate element in the SP metadata).
func certDERFromPEM(certPEM []byte) ([]byte, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("no CERTIFICATE PEM block found")
	}
	return block.Bytes, nil
}

// ensureSAMLSPKeypair returns the SP certificate + key PEM (and the cert DER for
// the metadata). When spec.saml.sp.certificateSecretRef is set it reads the
// kubernetes.io/tls-shaped Secret; otherwise it get-or-creates the stable
// <name>-saml-sp Secret with an operator-generated self-signed keypair. The
// keypair is create-once and NEVER regenerated — regeneration would invalidate
// the out-of-band IdP registration.
func (r *KeystoneReconciler) ensureSAMLSPKeypair(ctx context.Context, keystone *keystonev1alpha1.Keystone, backend *keystonev1alpha1.KeystoneIdentityBackend) (certPEM, keyPEM, certDER []byte, err error) {
	if sp := backend.Spec.SAML.SP; sp != nil && sp.CertificateSecretRef != nil {
		key := client.ObjectKey{Namespace: keystone.Namespace, Name: sp.CertificateSecretRef.Name}
		crt, err := secrets.GetSecretValue(ctx, r.Client, key, "tls.crt")
		if err != nil {
			return nil, nil, nil, err
		}
		k, err := secrets.GetSecretValue(ctx, r.Client, key, "tls.key")
		if err != nil {
			return nil, nil, nil, err
		}
		der, derErr := certDERFromPEM([]byte(crt))
		if derErr != nil {
			return nil, nil, nil, fmt.Errorf("parsing SP certificate from Secret %s: %w", key, derErr)
		}
		return []byte(crt), []byte(k), der, nil
	}

	name := samlSPKeypairSecretName(keystone)
	key := client.ObjectKey{Namespace: keystone.Namespace, Name: name}
	var secret corev1.Secret
	switch err := r.Get(ctx, key, &secret); {
	case err == nil:
		crt := secret.Data["tls.crt"]
		k := secret.Data["tls.key"]
		if len(crt) == 0 || len(k) == 0 {
			return nil, nil, nil, fmt.Errorf("SP keypair Secret %s carries no tls.crt/tls.key", key)
		}
		der, derErr := certDERFromPEM(crt)
		if derErr != nil {
			return nil, nil, nil, fmt.Errorf("parsing SP certificate from Secret %s: %w", key, derErr)
		}
		return crt, k, der, nil
	case !apierrors.IsNotFound(err):
		return nil, nil, nil, fmt.Errorf("fetching SP keypair Secret %s: %w", key, err)
	}

	crt, k, der, genErr := generateSelfSignedSPKeypair(fmt.Sprintf("%s-saml-sp", keystone.Name))
	if genErr != nil {
		return nil, nil, nil, genErr
	}
	secret = corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: keystone.Namespace,
			Labels:    commonLabels(keystone),
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{"tls.crt": crt, "tls.key": k},
	}
	if err := controllerutil.SetControllerReference(keystone, &secret, r.Scheme); err != nil {
		return nil, nil, nil, fmt.Errorf("setting owner reference on SP keypair Secret: %w", err)
	}
	if err := r.Create(ctx, &secret); err != nil {
		return nil, nil, nil, fmt.Errorf("creating SP keypair Secret %s: %w", key, err)
	}
	return crt, k, der, nil
}

// renderSAMLSPMetadata assembles the deterministic SP EntityDescriptor: an
// SPSSODescriptor (AuthnRequestsSigned=false, WantAssertionsSigned=true) with
// signing + encryption KeyDescriptors carrying the base64 certificate, an
// HTTP-POST AssertionConsumerService at <base>/postResponse, and an
// HTTP-Redirect SingleLogoutService at <base>/logout — the
// mellon_create_metadata.sh shape.
func renderSAMLSPMetadata(entityID, endpointBaseURL string, certDER []byte) []byte {
	certB64 := base64.StdEncoding.EncodeToString(certDER)
	keyDescriptor := func(use string) string {
		return fmt.Sprintf(`    <KeyDescriptor use="%s">
      <ds:KeyInfo xmlns:ds="http://www.w3.org/2000/09/xmldsig#">
        <ds:X509Data>
          <ds:X509Certificate>%s</ds:X509Certificate>
        </ds:X509Data>
      </ds:KeyInfo>
    </KeyDescriptor>`, use, certB64)
	}
	return []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<EntityDescriptor xmlns="urn:oasis:names:tc:SAML:2.0:metadata" entityID="%s">
  <SPSSODescriptor AuthnRequestsSigned="false" WantAssertionsSigned="true" protocolSupportEnumeration="urn:oasis:names:tc:SAML:2.0:protocol">
%s
%s
    <SingleLogoutService Binding="urn:oasis:names:tc:SAML:2.0:bindings:HTTP-Redirect" Location="%s/logout"/>
    <AssertionConsumerService index="0" isDefault="true" Binding="urn:oasis:names:tc:SAML:2.0:bindings:HTTP-POST" Location="%s/postResponse"/>
  </SPSSODescriptor>
</EntityDescriptor>
`, entityID, keyDescriptor("signing"), keyDescriptor("encryption"), endpointBaseURL, endpointBaseURL))
}

// fetchSAMLIdPMetadata GETs the backend's IdP metadata document through the
// injectable, SSRF-guarded HTTP seam, validates that the document's single
// EntityDescriptor entityID equals spec.saml.idpEntityID, and memoizes the
// result per (uid, generation). Mirrors fetchProviderMetadata (which validates
// the OIDC issuer instead); the shared client and cache are reused.
func (r *KeystoneReconciler) fetchSAMLIdPMetadata(ctx context.Context, backend *keystonev1alpha1.KeystoneIdentityBackend) ([]byte, error) {
	key := client.ObjectKeyFromObject(backend)
	r.federationMetadataCacheMu.Lock()
	entry, ok := r.federationMetadataCache[key]
	r.federationMetadataCacheMu.Unlock()
	if ok && entry.uid == backend.UID && entry.generation == backend.Generation {
		return entry.document, nil
	}

	metadataURL := backend.Spec.SAML.IdPMetadata.URL
	fetchCtx, cancel := context.WithTimeout(ctx, federationMetadataFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, metadataURL, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: building request for %s: %w", errProviderMetadataUnavailable, metadataURL, err)
	}
	resp, err := r.federationMetadataClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: fetching %s: %w", errProviderMetadataUnavailable, metadataURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%w: fetching %s: HTTP %d", errProviderMetadataUnavailable, metadataURL, resp.StatusCode)
	}
	document, err := io.ReadAll(io.LimitReader(resp.Body, maxProviderMetadataBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: reading %s: %w", errProviderMetadataUnavailable, metadataURL, err)
	}
	if len(document) > maxProviderMetadataBytes {
		return nil, fmt.Errorf("%w: %s exceeds the %d-byte budget", errProviderMetadataUnavailable, metadataURL, maxProviderMetadataBytes)
	}

	entityID, err := keystonev1alpha1.SAMLEntityIDFromMetadata(document)
	if err != nil {
		return nil, fmt.Errorf("%w: parsing %s: %w", errProviderMetadataUnavailable, metadataURL, err)
	}
	if entityID != backend.Spec.SAML.IdPEntityID {
		// Deliberately does not echo the fetched entityID: this error surfaces in
		// a tenant-visible IdentityBackendSkipped Event, so reflecting the fetched
		// document's content would turn the metadata fetch into an SSRF read
		// oracle. Only the tenant's own spec entityID is echoed.
		return nil, fmt.Errorf("%w: document entityID does not match spec idpEntityID %q",
			errProviderMetadataUnavailable, backend.Spec.SAML.IdPEntityID)
	}

	r.cacheProviderMetadata(backend, document)
	return document, nil
}

// lastKnownGoodSAMLMetadata returns the IdP metadata a prior successful
// reconcile persisted for backend in the newest federation Secret, or nil when
// none exists — the SAML analog of lastKnownGoodProviderMetadata.
func (r *KeystoneReconciler) lastKnownGoodSAMLMetadata(ctx context.Context, keystone *keystonev1alpha1.Keystone, backend *keystonev1alpha1.KeystoneIdentityBackend) []byte {
	newest := r.newestFederationSecret(ctx, keystone)
	if newest == nil {
		return nil
	}
	if doc := newest.Data[samlIdPMetadataKeyName(backend.Name)]; len(doc) > 0 {
		return doc
	}
	return nil
}

// resolveSAMLIdPMetadata resolves the IdP metadata from the configured source
// (inline / secretRef / url) and verifies its single EntityDescriptor entityID
// matches spec.saml.idpEntityID. A URL fetch rides out a transient IdP outage on
// a cache miss via the last-known-good copy (issuer-verified) so federation
// stays up, mirroring the OIDC discovery-document fallback.
func (r *KeystoneReconciler) resolveSAMLIdPMetadata(ctx context.Context, keystone *keystonev1alpha1.Keystone, backend *keystonev1alpha1.KeystoneIdentityBackend) ([]byte, error) {
	s := backend.Spec.SAML
	var document []byte
	switch {
	case s.IdPMetadata.Inline != "":
		document = []byte(s.IdPMetadata.Inline)
	case s.IdPMetadata.SecretRef != nil:
		key := client.ObjectKey{Namespace: keystone.Namespace, Name: s.IdPMetadata.SecretRef.Name}
		v, err := secrets.GetSecretValue(ctx, r.Client, key, "idp-metadata.xml")
		if err != nil {
			return nil, err
		}
		document = []byte(v)
	case s.IdPMetadata.URL != "":
		doc, err := r.fetchSAMLIdPMetadata(ctx, backend)
		if err != nil && errors.Is(err, errProviderMetadataUnavailable) {
			if lkg := r.lastKnownGoodSAMLMetadata(ctx, keystone, backend); lkg != nil {
				if id, e := keystonev1alpha1.SAMLEntityIDFromMetadata(lkg); e == nil && id == s.IdPEntityID {
					r.cacheProviderMetadata(backend, lkg)
					r.Recorder.Eventf(keystone, corev1.EventTypeWarning, "FederationMetadataStale",
						"Identity backend %s: IdP metadata unavailable (%v); reusing the last-known-good document so federation stays up", backend.Name, err)
					doc, err = lkg, nil
				}
			}
		}
		if err != nil {
			return nil, err
		}
		document = doc
	default:
		return nil, fmt.Errorf("backend %s SAML idpMetadata has no source", backend.Name)
	}

	entityID, err := keystonev1alpha1.SAMLEntityIDFromMetadata(document)
	if err != nil {
		// Classify as a per-backend fault so the caller skips + warns rather than
		// failing the whole pipeline.
		return nil, fmt.Errorf("%w: parsing IdP metadata for backend %s: %w", errProviderMetadataUnavailable, backend.Name, err)
	}
	if entityID != s.IdPEntityID {
		// No-echo: do not reflect the resolved entityID (SSRF read oracle for the
		// URL source). Only the tenant's own spec entityID is echoed.
		return nil, fmt.Errorf("%w: IdP metadata entityID does not match spec.saml.idpEntityID %q",
			errProviderMetadataUnavailable, s.IdPEntityID)
	}
	return document, nil
}

// renderSAMLBackend renders one SAML backend's federation artifacts: the SP
// keypair (consumed from spec or operator-generated), the deterministic SP
// metadata, and the resolved+verified IdP metadata, plus the proxy.conf
// parameters (strip headers, forwarded attribute env mappings).
func (r *KeystoneReconciler) renderSAMLBackend(ctx context.Context, keystone *keystonev1alpha1.Keystone, backend *keystonev1alpha1.KeystoneIdentityBackend) (samlRender, error) {
	s := backend.Spec.SAML
	if s == nil {
		// The webhook + CEL union rule prevent this; fail loudly rather than
		// rendering an empty mellon block if admission was bypassed.
		return samlRender{}, fmt.Errorf("backend %s has type %s but no saml block", backend.Name, backend.Spec.Type)
	}
	if err := validateSAMLRenderInputs(backend); err != nil {
		return samlRender{}, err
	}

	spCert, spKey, spCertDER, err := r.ensureSAMLSPKeypair(ctx, keystone, backend)
	if err != nil {
		return samlRender{}, err
	}

	endpointPath := samlSPEndpointPath(backend)
	endpointBase := samlSPEndpointBaseURL(keystone, backend)
	spEntityID := endpointBase + "/metadata"
	spMetadata := renderSAMLSPMetadata(spEntityID, endpointBase, spCertDER)

	idpMetadata, err := r.resolveSAMLIdPMetadata(ctx, keystone, backend)
	if err != nil {
		return samlRender{}, err
	}

	forwardEnvs := []string{"MELLON_IDP", "MELLON_NAME_ID"}
	for _, attr := range s.ForwardAttributes {
		forwardEnvs = append(forwardEnvs, "MELLON_"+strings.ToUpper(attr))
	}

	return samlRender{
		backendName:  backend.Name,
		idpName:      backend.EffectiveIdentityProviderName(),
		protocolID:   backend.EffectiveProtocolID(),
		idpEntityID:  s.IdPEntityID,
		remoteIDAttr: backend.EffectiveRemoteIDAttribute(),
		idpMetadata:  idpMetadata,
		spKey:        spKey,
		spCert:       spCert,
		spMetadata:   spMetadata,
		spEntityID:   spEntityID,
		endpointPath: endpointPath,
		stripHeaders: samlStripHeaders(backend),
		forwardEnvs:  forwardEnvs,
	}, nil
}

// writeMellonProtectedLocation writes one mellon-protected <Location>: AuthType
// Mellon + Require valid-user, and one RequestHeader set per forwarded env. The
// set runs in the default (late) phase — after mellon populates reqenv and after
// the anti-spoof `unset ... early` lines — so a forged inbound header is
// overwritten with the genuine (possibly empty) value, which doubles as spoof
// defense.
func writeMellonProtectedLocation(w func(string, ...any), location string, rd *samlRender) {
	w("<Location %q>", location)
	w("    AuthType Mellon")
	w("    MellonEnable auth")
	w("    Require valid-user")
	for _, env := range rd.forwardEnvs {
		w("    RequestHeader set %q \"expr=%%{reqenv:%s}\"", strings.ReplaceAll(env, "_", "-"), env)
	}
	w("</Location>")
}

// writeMellonConf emits the mod_auth_mellon configuration for one SAML backend:
// a <Location "/v3"> parent block that enables mellon in info mode and points at
// the mounted SP/IdP material, then the protected websso and auth Locations. The
// caller emits the ProxyPass exclusion for rd.endpointPath before the catch-all.
func writeMellonConf(w func(string, ...any), rd *samlRender) {
	mount := federationMellonMountPath
	w("")
	w("# mod_auth_mellon SP for identity provider %s (protocol %s).", rd.idpName, rd.protocolID)
	// The parent Location enables mellon in info mode across /v3 so the module
	// serves its SP endpoints and populates the assertion env vars; the
	// protected Locations below flip it to auth mode + Require valid-user.
	w("<Location \"/v3\">")
	w("    MellonEnable \"info\"")
	// Merge multi-valued attributes into single MELLON_<attr> env vars (no _N
	// index suffix) so the forwarded MELLON-<attr> headers are predictable.
	w("    MellonMergeEnvVars On")
	w("    MellonSPPrivateKeyFile %s/%s", mount, samlSPKeyFileName)
	w("    MellonSPCertFile %s/%s", mount, samlSPCertFileName)
	w("    MellonSPMetadataFile %s/%s", mount, samlSPMetadataFileName)
	w("    MellonIdPMetadataFile %s/%s", mount, samlIdPMetadataKeyName(rd.backendName))
	w("    MellonEndpointPath %s", rd.endpointPath)
	// Behind the sidecar / gateway the SubjectConfirmationData Address never
	// matches the client's source IP, so the check must be disabled.
	w("    MellonSubjectConfirmationDataAddressCheck Off")
	w("</Location>")

	for _, loc := range []string{
		fmt.Sprintf("/v3/auth/OS-FEDERATION/identity_providers/%s/protocols/%s/websso", rd.idpName, rd.protocolID),
		fmt.Sprintf("/v3/OS-FEDERATION/identity_providers/%s/protocols/%s/auth", rd.idpName, rd.protocolID),
	} {
		writeMellonProtectedLocation(w, loc, rd)
	}
}

// ensureSAMLSPMetadataSecret creates or updates the stable-named SP metadata
// Secret (data keys "sp-metadata.xml" and "entityID"), owner-ref'd to the
// Keystone CR, so an operator can register the service provider with the IdP out
// of band. Update-on-drift.
func (r *KeystoneReconciler) ensureSAMLSPMetadataSecret(ctx context.Context, keystone *keystonev1alpha1.Keystone, rd *samlRender) error {
	name := samlSPMetadataSecretName(keystone)
	key := client.ObjectKey{Namespace: keystone.Namespace, Name: name}
	desired := map[string][]byte{
		"sp-metadata.xml": rd.spMetadata,
		"entityID":        []byte(rd.spEntityID),
	}

	var existing corev1.Secret
	err := r.Get(ctx, key, &existing)
	if apierrors.IsNotFound(err) {
		secret := corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: keystone.Namespace,
				Labels:    commonLabels(keystone),
			},
			Data: desired,
		}
		if err := controllerutil.SetControllerReference(keystone, &secret, r.Scheme); err != nil {
			return fmt.Errorf("setting owner reference on SP metadata Secret: %w", err)
		}
		if err := r.Create(ctx, &secret); err != nil {
			return fmt.Errorf("creating SP metadata Secret %s: %w", key, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("fetching SP metadata Secret %s: %w", key, err)
	}
	if !reflect.DeepEqual(existing.Data, desired) {
		existing.Data = desired
		if err := r.Update(ctx, &existing); err != nil {
			return fmt.Errorf("updating SP metadata Secret %s: %w", key, err)
		}
	}
	return nil
}

// deleteSAMLSPMetadataSecret removes the SP metadata export Secret when no SAML
// backend is attached anymore. Not-found is a clean no-op.
func (r *KeystoneReconciler) deleteSAMLSPMetadataSecret(ctx context.Context, keystone *keystonev1alpha1.Keystone) error {
	secret := corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name:      samlSPMetadataSecretName(keystone),
		Namespace: keystone.Namespace,
	}}
	if err := r.Delete(ctx, &secret); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting SP metadata Secret: %w", err)
	}
	return nil
}
