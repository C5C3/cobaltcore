// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/xml"
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

const testSAMLIdPEntityID = "https://idp.example.com/realms/forge"

func testSAMLIdPMetadataXML(entityID string) string {
	return `<EntityDescriptor xmlns="urn:oasis:names:tc:SAML:2.0:metadata" entityID="` + entityID +
		`"><IDPSSODescriptor protocolSupportEnumeration="urn:oasis:names:tc:SAML:2.0:protocol"/></EntityDescriptor>`
}

// testProjectableSAMLBackend returns a DomainReady SAML backend (secretRef
// metadata source) attached to testKeystone().
func testProjectableSAMLBackend(name string) *keystonev1alpha1.KeystoneIdentityBackend {
	b := testSAMLBackend(name, name+"-domain")
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

// testSAMLIdPMetadataSecret returns the IdP-metadata Secret referenced by a
// testProjectableSAMLBackend.
func testSAMLIdPMetadataSecret(backendName string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: backendName + "-idp-metadata", Namespace: "default"},
		Data:       map[string][]byte{"idp-metadata.xml": []byte(testSAMLIdPMetadataXML(testSAMLIdPEntityID))},
	}
}

func TestRenderSAMLBackend_SecretRefSource(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	ks := testFederationKeystone()
	backend := testProjectableSAMLBackend("corp-saml")
	r := newTestReconciler(ks, backend, testSAMLIdPMetadataSecret("corp-saml"))

	rd, err := r.renderSAMLBackend(ctx, ks, backend)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(rd.idpName).To(Equal("corp-saml"))
	g.Expect(rd.protocolID).To(Equal("mapped"))
	g.Expect(rd.remoteIDAttr).To(Equal("HTTP_MELLON_IDP"))
	g.Expect(string(rd.idpMetadata)).To(ContainSubstring(testSAMLIdPEntityID))
	g.Expect(rd.spKey).NotTo(BeEmpty())
	g.Expect(rd.spCert).NotTo(BeEmpty())

	// forwardEnvs always carry MELLON-IDP / MELLON-NAME-ID plus the per-attribute
	// entry (the fixture forwards "username").
	headers := make([]string, 0, len(rd.forwardEnvs))
	for _, env := range rd.forwardEnvs {
		headers = append(headers, strings.ReplaceAll(env, "_", "-"))
	}
	g.Expect(headers).To(ContainElements("MELLON-IDP", "MELLON-NAME-ID", "MELLON-USERNAME"))

	// strip headers cover the remote-id attribute, the mapping remote types, and
	// the MELLON attribute env keys in both spellings.
	g.Expect(rd.stripHeaders).To(ContainElements("MELLON-IDP", "MELLON_IDP", "MELLON-NAME-ID", "MELLON_NAME_ID", "MELLON-USERNAME", "MELLON_USERNAME"))

	// The SP metadata parses as a single EntityDescriptor with an HTTP-POST ACS
	// at <base>/postResponse and both KeyDescriptors.
	var meta struct {
		EntityID string `xml:"entityID,attr"`
		SP       struct {
			KeyDescriptors []struct {
				Use string `xml:"use,attr"`
			} `xml:"KeyDescriptor"`
			ACS struct {
				Binding  string `xml:"Binding,attr"`
				Location string `xml:"Location,attr"`
			} `xml:"AssertionConsumerService"`
		} `xml:"SPSSODescriptor"`
	}
	g.Expect(xml.Unmarshal(rd.spMetadata, &meta)).To(Succeed())
	g.Expect(meta.EntityID).To(Equal(rd.spEntityID))
	g.Expect(meta.SP.ACS.Binding).To(Equal("urn:oasis:names:tc:SAML:2.0:bindings:HTTP-POST"))
	g.Expect(meta.SP.ACS.Location).To(HaveSuffix("/auth/mellon/postResponse"))
	uses := []string{}
	for _, kd := range meta.SP.KeyDescriptors {
		uses = append(uses, kd.Use)
	}
	g.Expect(uses).To(ConsistOf("signing", "encryption"))
}

func TestRenderSAMLBackend_InlineSource(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	ks := testFederationKeystone()
	backend := testProjectableSAMLBackend("corp-saml")
	backend.Spec.SAML.IdPMetadata = keystonev1alpha1.SAMLIdPMetadataSpec{
		Inline: testSAMLIdPMetadataXML(testSAMLIdPEntityID),
	}
	r := newTestReconciler(ks, backend)

	rd, err := r.renderSAMLBackend(ctx, ks, backend)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(string(rd.idpMetadata)).To(ContainSubstring(testSAMLIdPEntityID))
}

func TestRenderSAMLBackend_URLSource(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	ks := testFederationKeystone()
	backend := testProjectableSAMLBackend("corp-saml")
	backend.Spec.SAML.IdPMetadata = keystonev1alpha1.SAMLIdPMetadataSpec{
		URL: "https://idp.example.com/realms/forge/descriptor",
	}
	r := newTestReconciler(ks, backend)
	r.HTTPClient = &metadataDoer{body: testSAMLIdPMetadataXML(testSAMLIdPEntityID)}

	rd, err := r.renderSAMLBackend(ctx, ks, backend)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(string(rd.idpMetadata)).To(ContainSubstring(testSAMLIdPEntityID))
}

// A fetched document whose entityID does not match spec.saml.idpEntityID is a
// per-backend fault (errProviderMetadataUnavailable), and the error MUST NOT
// echo the fetched entityID (SSRF read-oracle guard).
func TestRenderSAMLBackend_EntityIDMismatchSkips(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	ks := testFederationKeystone()
	backend := testProjectableSAMLBackend("corp-saml")
	backend.Spec.SAML.IdPMetadata = keystonev1alpha1.SAMLIdPMetadataSpec{
		URL: "https://idp.example.com/realms/forge/descriptor",
	}
	r := newTestReconciler(ks, backend)
	r.HTTPClient = &metadataDoer{body: testSAMLIdPMetadataXML("https://evil.example.com/idp")}

	_, err := r.renderSAMLBackend(ctx, ks, backend)
	g.Expect(err).To(MatchError(errProviderMetadataUnavailable))
	g.Expect(err.Error()).NotTo(ContainSubstring("evil.example.com"), "must not echo the fetched entityID")
}

// An inline aggregate (EntitiesDescriptor) or non-EntityDescriptor document is
// rejected before it can bind the login flow.
func TestRenderSAMLBackend_RejectsAggregateMetadata(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	ks := testFederationKeystone()
	backend := testProjectableSAMLBackend("corp-saml")
	backend.Spec.SAML.IdPMetadata = keystonev1alpha1.SAMLIdPMetadataSpec{
		Inline: `<EntitiesDescriptor xmlns="urn:oasis:names:tc:SAML:2.0:metadata"/>`,
	}
	r := newTestReconciler(ks, backend)

	_, err := r.renderSAMLBackend(ctx, ks, backend)
	g.Expect(err).To(MatchError(errProviderMetadataUnavailable))
}

// A control character in a value the render embeds into the Apache config is
// rejected by the last-line-of-defense backstop.
func TestValidateSAMLRenderInputs_RejectsControlChar(t *testing.T) {
	g := NewGomegaWithT(t)
	backend := testProjectableSAMLBackend("corp-saml")
	backend.Spec.SAML.IdPEntityID = "https://idp.example.com\nMellonEnable auth"
	g.Expect(validateSAMLRenderInputs(backend)).To(MatchError(errControlCharInValue))
}

// The SP keypair is create-once: two renders return the same key material, and
// the stable-named tls Secret is created exactly once.
func TestEnsureSAMLSPKeypair_CreateOnce(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	ks := testFederationKeystone()
	backend := testProjectableSAMLBackend("corp-saml")
	r := newTestReconciler(ks, backend)

	cert1, key1, _, err := r.ensureSAMLSPKeypair(ctx, ks, backend)
	g.Expect(err).NotTo(HaveOccurred())
	cert2, key2, _, err := r.ensureSAMLSPKeypair(ctx, ks, backend)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(cert2).To(Equal(cert1))
	g.Expect(key2).To(Equal(key1))

	var secret corev1.Secret
	g.Expect(r.Get(ctx, client.ObjectKey{Namespace: "default", Name: "test-keystone-saml-sp"}, &secret)).To(Succeed())
	g.Expect(secret.Type).To(Equal(corev1.SecretTypeTLS))
}

// A user-supplied certificateSecretRef is consumed instead of generating one.
func TestEnsureSAMLSPKeypair_ConsumesCertificateSecretRef(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	ks := testFederationKeystone()
	backend := testProjectableSAMLBackend("corp-saml")
	// Generate a keypair once to obtain valid PEM, then feed it back via a
	// user-supplied Secret.
	cert, key, _, err := r0(t, ks, backend).ensureSAMLSPKeypair(ctx, ks, backend)
	g.Expect(err).NotTo(HaveOccurred())

	backend.Spec.SAML.SP = &keystonev1alpha1.SAMLSPSpec{
		CertificateSecretRef: &commonv1.SecretRefSpec{Name: "user-sp-cert"},
	}
	userSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "user-sp-cert", Namespace: "default"},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{"tls.crt": cert, "tls.key": key},
	}
	r := newTestReconciler(ks, backend, userSecret)

	gotCert, gotKey, _, err := r.ensureSAMLSPKeypair(ctx, ks, backend)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(gotCert).To(Equal(cert))
	g.Expect(gotKey).To(Equal(key))
	// No operator-generated Secret was created.
	var generated corev1.Secret
	g.Expect(r.Get(ctx, client.ObjectKey{Namespace: "default", Name: "test-keystone-saml-sp"}, &generated)).NotTo(Succeed())
}

// r0 builds a throwaway reconciler for keypair pre-generation.
func r0(t *testing.T, ks *keystonev1alpha1.Keystone, backend *keystonev1alpha1.KeystoneIdentityBackend) *KeystoneReconciler {
	t.Helper()
	return newTestReconciler(ks.DeepCopy(), backend.DeepCopy())
}

func TestReconcileIdentityBackends_SAMLRendersFederationSecret(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	ks := testFederationKeystone()
	backend := testProjectableSAMLBackend("corp-saml")
	r := newTestReconciler(ks, backend, testSAMLIdPMetadataSecret("corp-saml"))

	projection, err := r.reconcileIdentityBackends(ctx, ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(projection.DomainsSecretName).To(BeEmpty(), "no LDAP backend is attached")
	g.Expect(projection.Federation).NotTo(BeNil())
	g.Expect(projection.Federation.RemoteIDAttribute).To(BeEmpty(), "no OIDC backend, so no [openid] remote-id")
	g.Expect(projection.Federation.SAMLProtocolID).To(Equal("mapped"))
	g.Expect(projection.Federation.SAMLRemoteIDAttribute).To(Equal("HTTP_MELLON_IDP"))
	g.Expect(projection.Federation.MellonItems).NotTo(BeEmpty())
	g.Expect(projection.Federation.MetadataItems).To(BeEmpty(), "no OIDC metadata items for a SAML-only projection")

	// The federation Secret carries the SP material and the IdP metadata.
	var secret corev1.Secret
	g.Expect(r.Get(ctx, client.ObjectKey{Namespace: "default", Name: projection.Federation.SecretName}, &secret)).To(Succeed())
	g.Expect(secret.Data).To(HaveKey("proxy.conf"))
	g.Expect(secret.Data).To(HaveKey(samlSPKeyFileName))
	g.Expect(secret.Data).To(HaveKey(samlSPMetadataFileName))
	g.Expect(secret.Data).To(HaveKey("corp-saml.idp-metadata.xml"))
	// No OIDC crypto passphrase Secret is created for a SAML-only Keystone.
	var passSecret corev1.Secret
	g.Expect(r.Get(ctx, client.ObjectKey{Namespace: "default", Name: "test-keystone-oidc-crypto-passphrase"}, &passSecret)).NotTo(Succeed())

	// The SP metadata export Secret exists for out-of-band IdP registration.
	var exportSecret corev1.Secret
	g.Expect(r.Get(ctx, client.ObjectKey{Namespace: "default", Name: "test-keystone-saml-sp-metadata"}, &exportSecret)).To(Succeed())
	g.Expect(exportSecret.Data).To(HaveKey("sp-metadata.xml"))
	g.Expect(exportSecret.Data).To(HaveKey("entityID"))
}

// A missing IdP-metadata Secret is a per-backend fault: the backend is skipped
// and warned, IdentityBackendsReady goes WaitingForBackends.
func TestReconcileIdentityBackends_SAMLMissingMetadataSecretSkips(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	ks := testFederationKeystone()
	backend := testProjectableSAMLBackend("corp-saml")
	r := newTestReconciler(ks, backend) // no metadata Secret

	projection, err := r.reconcileIdentityBackends(ctx, ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(projection.Federation).To(BeNil())

	cond := commonconditions.GetCondition(ks.Status.Conditions, conditionTypeIdentityBackendsReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForBackends))
}

// A SAML backend attached without a proxy image is pending (shared with OIDC).
func TestReconcileIdentityBackends_SAMLMissingProxyImagePending(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	ks := testKeystone() // no spec.federation.proxyImage
	backend := testProjectableSAMLBackend("corp-saml")
	r := newTestReconciler(ks, backend, testSAMLIdPMetadataSecret("corp-saml"))

	projection, err := r.reconcileIdentityBackends(ctx, ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(projection.Federation).To(BeNil())
	cond := commonconditions.GetCondition(ks.Status.Conditions, conditionTypeIdentityBackendsReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Message).To(ContainSubstring("proxyImage not set"))
}

// Two SAML backends on one Keystone: the defensive skip projects NONE.
func TestReconcileIdentityBackends_TwoSAMLBackendsSkipped(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	ks := testFederationKeystone()
	a := testProjectableSAMLBackend("a-saml")
	b := testProjectableSAMLBackend("b-saml")
	r := newTestReconciler(ks, a, b, testSAMLIdPMetadataSecret("a-saml"), testSAMLIdPMetadataSecret("b-saml"))

	projection, err := r.reconcileIdentityBackends(ctx, ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(projection.Federation).To(BeNil())
	cond := commonconditions.GetCondition(ks.Status.Conditions, conditionTypeIdentityBackendsReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Message).To(ContainSubstring("more than one SAML backend"))
}

// Detaching the SAML backend (Terminating, so it is still listed but excluded
// from the projection) removes the SP metadata export Secret.
func TestReconcileIdentityBackends_SAMLExportSecretDeletedOnDetach(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	ks := testFederationKeystone()
	// A deleting SAML backend keeps the list non-empty (avoiding the
	// zero-backend early return) but contributes no render.
	deleting := testProjectableSAMLBackend("corp-saml")
	now := metav1.Now()
	deleting.DeletionTimestamp = &now
	deleting.Finalizers = []string{identityBackendFinalizerName}
	// Seed an export Secret as if the backend was previously projected.
	stale := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "test-keystone-saml-sp-metadata", Namespace: "default"}}
	r := newTestReconciler(ks, deleting, stale)

	_, err := r.reconcileIdentityBackends(ctx, ks)
	g.Expect(err).NotTo(HaveOccurred())
	var gone corev1.Secret
	g.Expect(r.Get(ctx, client.ObjectKey{Namespace: "default", Name: "test-keystone-saml-sp-metadata"}, &gone)).NotTo(Succeed())
}

// The Secret-name extractor indexes the SAML IdP-metadata and SP-certificate
// Secrets so a rotation re-renders the federation Secret.
func TestIdentityBackendSecretNameExtractor_SAMLSecrets(t *testing.T) {
	g := NewGomegaWithT(t)

	saml := testProjectableSAMLBackend("corp-saml")
	g.Expect(identityBackendSecretNameExtractor(saml)).To(ConsistOf("corp-saml-idp-metadata"))

	saml.Spec.SAML.SP = &keystonev1alpha1.SAMLSPSpec{
		CertificateSecretRef: &commonv1.SecretRefSpec{Name: "corp-saml-cert"},
	}
	g.Expect(identityBackendSecretNameExtractor(saml)).To(ConsistOf("corp-saml-idp-metadata", "corp-saml-cert"))

	// An inline-metadata SAML backend references no metadata Secret.
	inline := testProjectableSAMLBackend("corp-saml")
	inline.Spec.SAML.IdPMetadata = keystonev1alpha1.SAMLIdPMetadataSpec{Inline: testSAMLIdPMetadataXML(testSAMLIdPEntityID)}
	g.Expect(identityBackendSecretNameExtractor(inline)).To(BeEmpty())
}

func TestRenderProxyConf_SAMLOnly(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testFederationKeystone()
	rd := samlRender{
		backendName:  "corp-saml",
		idpName:      "corp-saml",
		protocolID:   "mapped",
		idpEntityID:  testSAMLIdPEntityID,
		remoteIDAttr: "HTTP_MELLON_IDP",
		endpointPath: "/v3/OS-FEDERATION/identity_providers/corp-saml/protocols/mapped/auth/mellon",
		stripHeaders: []string{"MELLON-IDP", "MELLON_IDP"},
		forwardEnvs:  []string{"MELLON_IDP"},
	}
	conf := string(renderProxyConf(ks, nil, []samlRender{rd}, ""))

	// No OIDC server-level directives and no passphrase for a SAML-only render.
	g.Expect(conf).NotTo(ContainSubstring("OIDCCryptoPassphrase"))
	g.Expect(conf).NotTo(ContainSubstring("OIDCMetadataDir"))
	// The mellon ProxyPass exclusion must precede the catch-all.
	exclusion := `ProxyPass "/v3/OS-FEDERATION/identity_providers/corp-saml/protocols/mapped/auth/mellon" !`
	g.Expect(conf).To(ContainSubstring(exclusion))
	g.Expect(strings.Index(conf, exclusion)).To(BeNumerically("<", strings.Index(conf, `ProxyPass "/" "http://127.0.0.1:5000/"`)))
	// The mellon parent block and a protected Location.
	g.Expect(conf).To(ContainSubstring(`<Location "/v3">`))
	g.Expect(conf).To(ContainSubstring("MellonEnable \"info\""))
	g.Expect(conf).To(ContainSubstring("MellonSubjectConfirmationDataAddressCheck Off"))
	g.Expect(conf).To(ContainSubstring(`<Location "/v3/OS-FEDERATION/identity_providers/corp-saml/protocols/mapped/auth">`))
	g.Expect(conf).To(ContainSubstring("AuthType Mellon"))
	g.Expect(conf).To(ContainSubstring(`RequestHeader set "MELLON-IDP" "expr=%{reqenv:MELLON_IDP}"`))
	// The strip line runs early; the forward set runs late (no `early`).
	g.Expect(conf).To(ContainSubstring(`RequestHeader unset "MELLON-IDP" early`))
	// Global websso for the unique protocol.
	g.Expect(conf).To(ContainSubstring(`<Location "/v3/auth/OS-FEDERATION/websso/mapped">`))
}

func TestRenderProxyConf_OIDCAndSAMLCoexist(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testFederationKeystone()
	oidc := []oidcRender{{
		backendName: "corp-oidc", idpName: "corp-oidc", protocolID: "openid",
		issuer:            "https://idp.example.com/realms/forge",
		metadataBasename:  issuerToMetadataBasename("https://idp.example.com/realms/forge"),
		sessionType:       "client-cookie",
		stateInputHeaders: "none",
		stripHeaders:      []string{"OIDC-ISS", "OIDC_ISS"},
	}}
	saml := []samlRender{{
		backendName: "corp-saml", idpName: "corp-saml", protocolID: "mapped",
		idpEntityID:  testSAMLIdPEntityID,
		remoteIDAttr: "HTTP_MELLON_IDP",
		endpointPath: "/v3/OS-FEDERATION/identity_providers/corp-saml/protocols/mapped/auth/mellon",
		stripHeaders: []string{"MELLON-IDP", "MELLON_IDP"},
		forwardEnvs:  []string{"MELLON_IDP"},
	}}
	conf := string(renderProxyConf(ks, oidc, saml, "pass-phrase"))

	// Both modules' directives coexist.
	g.Expect(conf).To(ContainSubstring(`OIDCCryptoPassphrase "pass-phrase"`))
	g.Expect(conf).To(ContainSubstring(`<Location "/v3/auth/OS-FEDERATION/identity_providers/corp-oidc/protocols/openid/websso">`))
	g.Expect(conf).To(ContainSubstring("AuthType openid-connect"))
	g.Expect(conf).To(ContainSubstring("AuthType Mellon"))
	// Merged strip list carries both types.
	g.Expect(conf).To(ContainSubstring(`RequestHeader unset "OIDC-ISS" early`))
	g.Expect(conf).To(ContainSubstring(`RequestHeader unset "MELLON-IDP" early`))
	// Distinct global websso paths, one per unique protocolID.
	g.Expect(conf).To(ContainSubstring(`<Location "/v3/auth/OS-FEDERATION/websso/openid">`))
	g.Expect(conf).To(ContainSubstring(`<Location "/v3/auth/OS-FEDERATION/websso/mapped">`))
}
