// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	commonv1 "github.com/c5c3/forge/internal/common/types"
)

// validIdentityBackend returns a minimal valid KeystoneIdentityBackend the
// per-rule tests mutate one field of, so every rejection is attributable to
// exactly one rule.
func validIdentityBackend() *KeystoneIdentityBackend {
	return &KeystoneIdentityBackend{
		ObjectMeta: metav1.ObjectMeta{Name: "corp-ldap", Namespace: "openstack"},
		Spec: KeystoneIdentityBackendSpec{
			KeystoneRef: KeystoneRefSpec{Name: "keystone"},
			Domain: DomainSpec{
				Name:           "corp",
				Mode:           DomainModeManage,
				DeletionPolicy: DomainDeletionPolicyRetain,
			},
			Type: IdentityBackendTypeLDAP,
			LDAP: &LDAPBackendSpec{
				URL:                      "ldap://ldap.corp.example.com:389",
				BindCredentialsSecretRef: commonv1.SecretRefSpec{Name: "corp-ldap-bind"},
				Suffix:                   "dc=corp,dc=example,dc=com",
				Users:                    LDAPUserSpec{TreeDN: "ou=people,dc=corp,dc=example,dc=com"},
				ReadOnly:                 ptr.To(true),
			},
		},
	}
}

func identityBackendScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := AddToScheme(s); err != nil {
		t.Fatalf("adding keystone scheme: %v", err)
	}
	return s
}

func TestIdentityBackendDefault_MaterializesDocumentedDefaults(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneIdentityBackendWebhook{}

	b := validIdentityBackend()
	b.Spec.Domain.Mode = ""
	b.Spec.Domain.DeletionPolicy = ""
	b.Spec.LDAP.ReadOnly = nil

	g.Expect(w.Default(context.Background(), b)).To(Succeed())
	g.Expect(b.Spec.Domain.Mode).To(Equal(DomainModeManage))
	g.Expect(b.Spec.Domain.DeletionPolicy).To(Equal(DomainDeletionPolicyRetain))
	g.Expect(b.Spec.LDAP.ReadOnly).To(HaveValue(BeTrue()))
}

// Default() followed by validate() on a zero-value object must not hide a
// precondition behind the happy-path fixture: the errors reported are the
// genuinely missing required fields, not a nil-pointer panic or a cryptic
// parser message.
func TestIdentityBackendDefaultThenValidate_ZeroValueObject(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneIdentityBackendWebhook{}

	b := &KeystoneIdentityBackend{ObjectMeta: metav1.ObjectMeta{Name: "zero", Namespace: "openstack"}}
	g.Expect(w.Default(context.Background(), b)).To(Succeed())

	_, err := w.ValidateCreate(context.Background(), b)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("keystoneRef.name must be set"))
	g.Expect(err.Error()).To(ContainSubstring("domain.name must be set"))
}

func TestIdentityBackendValidate_AcceptsValidBackend(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneIdentityBackendWebhook{}

	_, err := w.ValidateCreate(context.Background(), validIdentityBackend())
	g.Expect(err).NotTo(HaveOccurred())
}

func TestIdentityBackendValidate_RejectsUnionMismatch(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneIdentityBackendWebhook{}

	b := validIdentityBackend()
	b.Spec.LDAP = nil

	_, err := w.ValidateCreate(context.Background(), b)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("exactly one backend block matching spec.type"))
}

func TestIdentityBackendValidate_RejectsDefaultDomainCaseInsensitive(t *testing.T) {
	for _, name := range []string{"default", "Default", "DEFAULT"} {
		t.Run(name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			w := &KeystoneIdentityBackendWebhook{}

			b := validIdentityBackend()
			b.Spec.Domain.Name = name

			_, err := w.ValidateCreate(context.Background(), b)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("must never be backed by an external identity backend"))
		})
	}
}

func TestIdentityBackendValidate_RejectsBadURLScheme(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneIdentityBackendWebhook{}

	b := validIdentityBackend()
	b.Spec.LDAP.URL = "http://ldap.corp.example.com"

	_, err := w.ValidateCreate(context.Background(), b)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("ldap:// or ldaps://"))
}

func TestIdentityBackendValidate_RejectsBindSecretRefKey(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneIdentityBackendWebhook{}

	b := validIdentityBackend()
	b.Spec.LDAP.BindCredentialsSecretRef.Key = "bindpw"

	_, err := w.ValidateCreate(context.Background(), b)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring(`data keys are fixed ("username" and "password")`))
}

func TestIdentityBackendValidate_RejectsCASecretRefKey(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneIdentityBackendWebhook{}

	b := validIdentityBackend()
	b.Spec.LDAP.TLS = &LDAPTLSSpec{
		CABundleSecretRef: commonv1.SecretRefSpec{Name: "corp-ldap-ca", Key: "bundle.pem"},
	}

	_, err := w.ValidateCreate(context.Background(), b)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring(`data key is fixed ("ca.crt")`))
}

// A newline (or carriage-return) in any rendered LDAP value is an INI-injection
// vector — RenderINI writes values verbatim, so a smuggled "\nuser_allow_create
// = true" would defeat the readOnly forcing. The webhook rejects it per field.
func TestIdentityBackendValidate_RejectsControlCharsInLDAPFields(t *testing.T) {
	inject := "ldap://ldap.corp.example.com\nuser_allow_create = true"
	tests := []struct {
		name  string
		mutin func(*KeystoneIdentityBackend)
	}{
		{"url", func(b *KeystoneIdentityBackend) { b.Spec.LDAP.URL = inject }},
		{"suffix", func(b *KeystoneIdentityBackend) { b.Spec.LDAP.Suffix = "dc=x\nuser_allow_create = true" }},
		{"users.treeDN", func(b *KeystoneIdentityBackend) { b.Spec.LDAP.Users.TreeDN = "ou=p\r,dc=x" }},
		{"users.filter", func(b *KeystoneIdentityBackend) { b.Spec.LDAP.Users.Filter = "(x)\nuser_allow_create = true" }},
		{"users.mailAttribute", func(b *KeystoneIdentityBackend) { b.Spec.LDAP.Users.MailAttribute = "mail\nfoo = bar" }},
		{"groups.memberAttribute", func(b *KeystoneIdentityBackend) {
			b.Spec.LDAP.Groups = &LDAPGroupSpec{TreeDN: "ou=g,dc=x", MemberAttribute: "member\nfoo = bar"}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			w := &KeystoneIdentityBackendWebhook{}

			b := validIdentityBackend()
			tc.mutin(b)

			_, err := w.ValidateCreate(context.Background(), b)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("must not contain newline or carriage-return"))
		})
	}
}

// The extraOptions escape hatch is the highest-value injection vector: a value
// containing "\nuser_allow_create = true" would render after the forced-false
// options and win under oslo.config's last-value-wins scalar semantics. Reject
// control characters in extraOptions values regardless of how innocuous the
// key is.
func TestIdentityBackendValidate_RejectsControlCharsInExtraOptionValue(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneIdentityBackendWebhook{}

	b := validIdentityBackend()
	b.Spec.ExtraOptions = map[string]string{
		"zzz_pwn": "x\nuser_allow_create = true\ngroup_allow_delete = true",
	}

	_, err := w.ValidateCreate(context.Background(), b)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("must not contain newline or carriage-return"))
}

// A newline in an extraOptions KEY is the same INI-injection vector as a
// newline in the value: RenderINI writes `key = value` verbatim, so a key of
// "zzz_pwn\nuser_allow_create = true" injects that line regardless of how
// innocuous the value is. The value-side guard never inspects the key, so the
// key allowlist is the gate.
func TestIdentityBackendValidate_RejectsControlCharsInExtraOptionKey(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneIdentityBackendWebhook{}

	b := validIdentityBackend()
	b.Spec.ExtraOptions = map[string]string{
		"zzz_pwn\nuser_allow_create = true": "x",
	}

	_, err := w.ValidateCreate(context.Background(), b)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("option name must match"))
}

// A trailing space on an extraOptions KEY evades both the exact-string
// denylist and the readOnly forced-option check ("user_allow_create " !=
// "user_allow_create"), yet oslo.config strips it to a duplicate that
// overrides the forced false. The key allowlist rejects the malformed key
// before either exact-match check runs.
func TestIdentityBackendValidate_RejectsDenylistEvadingExtraOptionKey(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneIdentityBackendWebhook{}

	b := validIdentityBackend()
	b.Spec.ExtraOptions = map[string]string{"user_allow_create ": "true"}

	_, err := w.ValidateCreate(context.Background(), b)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("option name must match"))
}

func TestIdentityBackendValidate_ExtraOptionsDenylist(t *testing.T) {
	tests := []struct {
		name    string
		option  string
		errText string
	}{
		{"typed-field option url", "url", `option "url" is owned by`},
		{"typed-field option user_tree_dn", "user_tree_dn", `option "user_tree_dn" is owned by`},
		{"operator-owned driver", "driver", `option "driver" is owned by`},
		{"operator-owned domain_config_dir", "domain_config_dir", `option "domain_config_dir" is owned by`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			w := &KeystoneIdentityBackendWebhook{}

			b := validIdentityBackend()
			b.Spec.ExtraOptions = map[string]string{tc.option: "x"}

			_, err := w.ValidateCreate(context.Background(), b)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring(tc.errText))
		})
	}
}

func TestIdentityBackendValidate_ExtraOptionsReadOnlyForced(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneIdentityBackendWebhook{}

	b := validIdentityBackend()
	b.Spec.ExtraOptions = map[string]string{"user_allow_create": "true"}

	_, err := w.ValidateCreate(context.Background(), b)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring(`conflicts with readOnly: true`))

	// With an explicit readOnly: false, the same option is permitted (the
	// operator no longer forces the write-enabling options).
	b.Spec.LDAP.ReadOnly = ptr.To(false)
	_, err = w.ValidateCreate(context.Background(), b)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestIdentityBackendValidate_ExtraOptionsAllowsUnownedOption(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneIdentityBackendWebhook{}

	b := validIdentityBackend()
	b.Spec.ExtraOptions = map[string]string{"page_size": "100"}

	_, err := w.ValidateCreate(context.Background(), b)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestIdentityBackendValidate_ExtraOptionsRejectsEmptyKey(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneIdentityBackendWebhook{}

	b := validIdentityBackend()
	b.Spec.ExtraOptions = map[string]string{"": "x"}

	_, err := w.ValidateCreate(context.Background(), b)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("option name must not be empty"))
}

func TestIdentityBackendValidate_DomainUniqueness(t *testing.T) {
	g := NewGomegaWithT(t)
	s := identityBackendScheme(t)

	existing := validIdentityBackend()
	existing.Name = "existing-ldap"
	existing.Spec.Domain.Name = "Corp" // differs only in case

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(existing).Build()
	w := &KeystoneIdentityBackendWebhook{Client: c}

	// Same Keystone + case-insensitively equal domain name: rejected.
	b := validIdentityBackend()
	_, err := w.ValidateCreate(context.Background(), b)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("domain name collides"))
	g.Expect(err.Error()).To(ContainSubstring("existing-ldap"))

	// Different Keystone, same domain name: accepted.
	b2 := validIdentityBackend()
	b2.Spec.KeystoneRef.Name = "keystone-other"
	_, err = w.ValidateCreate(context.Background(), b2)
	g.Expect(err).NotTo(HaveOccurred())
}

// On UPDATE the object under validation appears in the sibling List and must
// not collide with itself.
func TestIdentityBackendValidate_DomainUniquenessSkipsSelfOnUpdate(t *testing.T) {
	g := NewGomegaWithT(t)
	s := identityBackendScheme(t)

	self := validIdentityBackend()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(self).Build()
	w := &KeystoneIdentityBackendWebhook{Client: c}

	updated := validIdentityBackend()
	updated.Spec.Domain.Description = "updated"
	_, err := w.ValidateUpdate(context.Background(), self, updated)
	g.Expect(err).NotTo(HaveOccurred())
}

// A Terminating sibling must not block a replacement backend for the same
// domain (recreate-during-teardown).
func TestIdentityBackendValidate_DomainUniquenessIgnoresTerminatingSibling(t *testing.T) {
	g := NewGomegaWithT(t)
	s := identityBackendScheme(t)

	terminating := validIdentityBackend()
	terminating.Name = "old-ldap"
	now := metav1.Now()
	terminating.DeletionTimestamp = &now
	terminating.Finalizers = []string{"keystone.openstack.c5c3.io/identitybackend"}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(terminating).Build()
	w := &KeystoneIdentityBackendWebhook{Client: c}

	b := validIdentityBackend()
	_, err := w.ValidateCreate(context.Background(), b)
	g.Expect(err).NotTo(HaveOccurred())
}

// Aggregate test proving error accumulation: every violated rule surfaces in
// one admission error, with a substring assertion per rule (no
// short-circuiting).
func TestIdentityBackendValidateCreate_RunsAllValidations(t *testing.T) {
	g := NewGomegaWithT(t)
	s := identityBackendScheme(t)

	existing := validIdentityBackend()
	existing.Name = "existing-ldap"
	existing.Spec.Domain.Name = "broken" // collides with the CR under test
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(existing).Build()
	w := &KeystoneIdentityBackendWebhook{Client: c}

	b := validIdentityBackend()
	b.Spec.Domain.Name = "broken"
	b.Spec.LDAP.URL = "http://not-ldap"
	b.Spec.LDAP.BindCredentialsSecretRef.Key = "oops"
	b.Spec.ExtraOptions = map[string]string{
		"driver":            "sql",
		"user_allow_create": "true",
	}

	_, err := w.ValidateCreate(context.Background(), b)
	g.Expect(err).To(HaveOccurred())
	msg := err.Error()
	g.Expect(msg).To(ContainSubstring("ldap:// or ldaps://"))
	g.Expect(msg).To(ContainSubstring(`data keys are fixed`))
	g.Expect(msg).To(ContainSubstring(`option "driver" is owned by`))
	g.Expect(msg).To(ContainSubstring(`conflicts with readOnly: true`))
	g.Expect(msg).To(ContainSubstring("domain name collides"))
}

// validOIDCBackend returns a minimal valid OIDC-typed KeystoneIdentityBackend
// the per-rule tests mutate one field of.
func validOIDCBackend() *KeystoneIdentityBackend {
	return &KeystoneIdentityBackend{
		ObjectMeta: metav1.ObjectMeta{Name: "corp-oidc", Namespace: "openstack"},
		Spec: KeystoneIdentityBackendSpec{
			KeystoneRef: KeystoneRefSpec{Name: "keystone"},
			Domain: DomainSpec{
				Name:           "sso",
				Mode:           DomainModeManage,
				DeletionPolicy: DomainDeletionPolicyRetain,
			},
			Type: IdentityBackendTypeOIDC,
			OIDC: &OIDCBackendSpec{
				Issuer:          "https://idp.example.com/realms/forge",
				ClientID:        "keystone",
				ClientSecretRef: commonv1.SecretRefSpec{Name: "corp-oidc-client"},
			},
		},
	}
}

func TestIdentityBackendDefault_MaterializesOIDCDefaults(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneIdentityBackendWebhook{}

	b := validOIDCBackend()
	g.Expect(w.Default(context.Background(), b)).To(Succeed())

	g.Expect(b.Spec.OIDC.ProtocolID).To(Equal("openid"))
	g.Expect(b.Spec.OIDC.IdentityProviderName).To(Equal("corp-oidc"))
	g.Expect(b.Spec.OIDC.RemoteIDAttribute).To(Equal("HTTP_OIDC_ISS"))
	g.Expect(b.Spec.OIDC.Scopes).To(Equal([]string{"openid", "email", "profile"}))
	g.Expect(b.Spec.OIDC.ResponseType).To(Equal("code"))
	g.Expect(b.Spec.OIDC.SessionType).To(Equal(OIDCSessionTypeClientCookie))
	g.Expect(b.Spec.OIDC.StateInputHeaders).To(Equal(OIDCStateInputHeadersNone))
	g.Expect(b.Spec.OIDC.ProviderMetadataURL).To(
		Equal("https://idp.example.com/realms/forge/.well-known/openid-configuration"),
	)
}

// A trailing slash on the issuer must not double up in the derived discovery
// URL, and explicit endpoints must suppress the metadata-URL derivation
// entirely (the two discovery shapes are mutually exclusive).
func TestIdentityBackendDefault_MetadataURLDerivationEdgeCases(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneIdentityBackendWebhook{}

	trailing := validOIDCBackend()
	trailing.Spec.OIDC.Issuer = "https://idp.example.com/realms/forge/"
	g.Expect(w.Default(context.Background(), trailing)).To(Succeed())
	g.Expect(trailing.Spec.OIDC.ProviderMetadataURL).To(
		Equal("https://idp.example.com/realms/forge/.well-known/openid-configuration"),
	)

	explicit := validOIDCBackend()
	explicit.Spec.OIDC.Endpoints = &OIDCEndpointsSpec{
		AuthorizationEndpoint: "https://idp.example.com/auth",
		TokenEndpoint:         "https://idp.example.com/token",
		JWKSURI:               "https://idp.example.com/certs",
	}
	g.Expect(w.Default(context.Background(), explicit)).To(Succeed())
	g.Expect(explicit.Spec.OIDC.ProviderMetadataURL).To(BeEmpty())
}

func TestIdentityBackendValidate_AcceptsValidOIDCBackend(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneIdentityBackendWebhook{}

	b := validOIDCBackend()
	g.Expect(w.Default(context.Background(), b)).To(Succeed())
	_, err := w.ValidateCreate(context.Background(), b)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestIdentityBackendValidate_RejectsOIDCUnionMismatch(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneIdentityBackendWebhook{}

	// type OIDC without spec.oidc.
	b := validOIDCBackend()
	b.Spec.OIDC = nil
	_, err := w.ValidateCreate(context.Background(), b)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("type OIDC requires spec.oidc"))

	// spec.oidc alongside type LDAP.
	b2 := validIdentityBackend()
	b2.Spec.OIDC = validOIDCBackend().Spec.OIDC
	_, err = w.ValidateCreate(context.Background(), b2)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("type OIDC requires spec.oidc"))
}

func TestIdentityBackendValidate_RejectsBadOIDCIssuerScheme(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneIdentityBackendWebhook{}

	b := validOIDCBackend()
	b.Spec.OIDC.Issuer = "ldap://not-an-idp"

	_, err := w.ValidateCreate(context.Background(), b)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("http:// or https://"))
}

func TestIdentityBackendValidate_RejectsDiscoveryShapeConflict(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneIdentityBackendWebhook{}

	b := validOIDCBackend()
	b.Spec.OIDC.ProviderMetadataURL = "https://idp.example.com/.well-known/openid-configuration"
	b.Spec.OIDC.Endpoints = &OIDCEndpointsSpec{
		AuthorizationEndpoint: "https://idp.example.com/auth",
		TokenEndpoint:         "https://idp.example.com/token",
		JWKSURI:               "https://idp.example.com/certs",
	}

	_, err := w.ValidateCreate(context.Background(), b)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("mutually exclusive"))
}

func TestIdentityBackendValidate_RejectsClientSecretRefKey(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneIdentityBackendWebhook{}

	b := validOIDCBackend()
	b.Spec.OIDC.ClientSecretRef.Key = "secret"

	_, err := w.ValidateCreate(context.Background(), b)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring(`data key is fixed ("clientSecret")`))
}

func TestIdentityBackendValidate_RejectsMappingsOnLDAPBackend(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneIdentityBackendWebhook{}

	b := validIdentityBackend()
	b.Spec.Mappings = []MappingRuleSpec{{
		Local:  []MappingLocalRuleSpec{{Groups: "{0}"}},
		Remote: []MappingRemoteRuleSpec{{Type: "HTTP_OIDC_ISS"}},
	}}

	_, err := w.ValidateCreate(context.Background(), b)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("mappings are only supported on federation backends"))
}

func TestIdentityBackendValidate_RejectsExtraOptionsOnOIDCBackend(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneIdentityBackendWebhook{}

	b := validOIDCBackend()
	b.Spec.ExtraOptions = map[string]string{"page_size": "100"}

	_, err := w.ValidateCreate(context.Background(), b)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("only supported on type LDAP"))
}

func TestIdentityBackendValidate_RejectsIncompleteMappingRules(t *testing.T) {
	tests := []struct {
		name    string
		rule    MappingRuleSpec
		errText string
	}{
		{
			"missing remote",
			MappingRuleSpec{Local: []MappingLocalRuleSpec{{Groups: "{0}"}}},
			"at least one remote entry",
		},
		{
			"missing local",
			MappingRuleSpec{Remote: []MappingRemoteRuleSpec{{Type: "HTTP_OIDC_ISS"}}},
			"at least one local entry",
		},
		{
			"empty remote type",
			MappingRuleSpec{
				Local:  []MappingLocalRuleSpec{{Groups: "{0}"}},
				Remote: []MappingRemoteRuleSpec{{Type: ""}},
			},
			"remote.type must be set",
		},
		{
			// A newline in remote.type would inject Apache directives via the
			// generated RequestHeader-unset lines.
			"header-unsafe remote type",
			MappingRuleSpec{
				Local:  []MappingLocalRuleSpec{{Groups: "{0}"}},
				Remote: []MappingRemoteRuleSpec{{Type: "HTTP_OIDC_ISS\nProxyPass / http://evil/"}},
			},
			"remote.type must match",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			w := &KeystoneIdentityBackendWebhook{}

			b := validOIDCBackend()
			b.Spec.Mappings = []MappingRuleSpec{tc.rule}

			_, err := w.ValidateCreate(context.Background(), b)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring(tc.errText))
		})
	}
}

func TestIdentityBackendValidate_RejectsRoleAssignmentScopeConflict(t *testing.T) {
	tests := []struct {
		name string
		ra   FederationRoleAssignmentSpec
	}{
		{"both project and domain", FederationRoleAssignmentSpec{
			Role:    "member",
			Project: &FederationProjectScopeSpec{Name: "demo"},
			Domain:  true,
		}},
		{"neither project nor domain", FederationRoleAssignmentSpec{Role: "member"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			w := &KeystoneIdentityBackendWebhook{}

			b := validOIDCBackend()
			b.Spec.Groups = []FederationGroupSpec{{
				Name:            "federated-users",
				RoleAssignments: []FederationRoleAssignmentSpec{tc.ra},
			}}

			_, err := w.ValidateCreate(context.Background(), b)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("exactly one of project or domain"))
		})
	}
}

// A newline or quote in any value rendered into the Apache proxy config or
// the metadata JSON is a config-injection vector, exactly like the LDAP INI
// guard.
func TestIdentityBackendValidate_RejectsControlCharsInOIDCFields(t *testing.T) {
	tests := []struct {
		name  string
		mutin func(*KeystoneIdentityBackend)
	}{
		{"issuer newline", func(b *KeystoneIdentityBackend) {
			b.Spec.OIDC.Issuer = "https://idp.example.com\nProxyPass / http://evil/"
		}},
		{"clientID quote", func(b *KeystoneIdentityBackend) { b.Spec.OIDC.ClientID = `keystone"evil` }},
		{"scope newline", func(b *KeystoneIdentityBackend) { b.Spec.OIDC.Scopes = []string{"openid\nemail"} }},
		{"responseType quote", func(b *KeystoneIdentityBackend) { b.Spec.OIDC.ResponseType = `code"` }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			w := &KeystoneIdentityBackendWebhook{}

			b := validOIDCBackend()
			tc.mutin(b)

			_, err := w.ValidateCreate(context.Background(), b)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("must not contain newline, carriage-return, or double-quote"))
		})
	}
}

func TestIdentityBackendValidate_RejectsIntrospectionWithoutEndpointWhenExplicit(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneIdentityBackendWebhook{}

	b := validOIDCBackend()
	b.Spec.OIDC.OAuth2Introspection = &OIDCIntrospectionSpec{Enabled: true}
	b.Spec.OIDC.Endpoints = &OIDCEndpointsSpec{
		AuthorizationEndpoint: "https://idp.example.com/auth",
		TokenEndpoint:         "https://idp.example.com/token",
		JWKSURI:               "https://idp.example.com/certs",
	}

	_, err := w.ValidateCreate(context.Background(), b)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("introspectionEndpoint must be set"))
}

func TestIdentityBackendValidate_RejectsHTTPIntrospectionEndpoint(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneIdentityBackendWebhook{}

	b := validOIDCBackend()
	b.Spec.OIDC.OAuth2Introspection = &OIDCIntrospectionSpec{Enabled: true}
	b.Spec.OIDC.Endpoints = &OIDCEndpointsSpec{
		AuthorizationEndpoint: "https://idp.example.com/auth",
		TokenEndpoint:         "https://idp.example.com/token",
		JWKSURI:               "https://idp.example.com/certs",
		// mod_auth_openidc rejects http introspection endpoints at Apache
		// config-parse time — the webhook fails this at admission.
		IntrospectionEndpoint: "http://idp.example.com/introspect",
	}

	_, err := w.ValidateCreate(context.Background(), b)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("introspectionEndpoint must use https://"))
}

// Cross-CR checks: identityProviderName uniqueness, remoteIDAttribute
// uniformity, and the single-introspection-backend limit are all evaluated
// against the OIDC siblings attached to the same Keystone.
func TestIdentityBackendValidate_OIDCSiblingChecks(t *testing.T) {
	g := NewGomegaWithT(t)
	s := identityBackendScheme(t)

	sibling := validOIDCBackend()
	sibling.Name = "existing-oidc"
	sibling.Spec.Domain.Name = "sso-existing"
	sibling.Spec.OIDC.IdentityProviderName = "corp-idp"
	sibling.Spec.OIDC.RemoteIDAttribute = "HTTP_OIDC_ISS"
	sibling.Spec.OIDC.OAuth2Introspection = &OIDCIntrospectionSpec{Enabled: true}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(sibling).Build()
	w := &KeystoneIdentityBackendWebhook{Client: c}

	b := validOIDCBackend()
	b.Spec.OIDC.IdentityProviderName = "corp-idp"                           // collides
	b.Spec.OIDC.RemoteIDAttribute = "HTTP_OIDC_ISSUER"                      // conflicts
	b.Spec.OIDC.OAuth2Introspection = &OIDCIntrospectionSpec{Enabled: true} // second introspection

	_, err := w.ValidateCreate(context.Background(), b)
	g.Expect(err).To(HaveOccurred())
	msg := err.Error()
	g.Expect(msg).To(ContainSubstring("identity provider name collides"))
	g.Expect(msg).To(ContainSubstring("remoteIDAttribute must be uniform"))
	g.Expect(msg).To(ContainSubstring("at most one OIDC backend per Keystone may enable oauth2Introspection"))

	// A sibling attached to a different Keystone triggers none of the checks.
	b2 := validOIDCBackend()
	b2.Spec.KeystoneRef.Name = "keystone-other"
	b2.Spec.OIDC.IdentityProviderName = "corp-idp"
	_, err = w.ValidateCreate(context.Background(), b2)
	g.Expect(err).NotTo(HaveOccurred())
}

// Aggregate OIDC test proving error accumulation across the OIDC rules (the
// LDAP aggregate above cannot exercise them — a CR is either LDAP or OIDC).
func TestIdentityBackendValidateCreate_RunsAllOIDCValidations(t *testing.T) {
	g := NewGomegaWithT(t)
	s := identityBackendScheme(t)

	sibling := validOIDCBackend()
	sibling.Name = "existing-oidc"
	sibling.Spec.Domain.Name = "sso-existing"
	sibling.Spec.OIDC.IdentityProviderName = "corp-idp"
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(sibling).Build()
	w := &KeystoneIdentityBackendWebhook{Client: c}

	b := validOIDCBackend()
	b.Spec.OIDC.Issuer = "ftp://bad-scheme"
	b.Spec.OIDC.ClientSecretRef.Key = "oops"
	b.Spec.OIDC.IdentityProviderName = "corp-idp"
	b.Spec.OIDC.ClientID = `keystone"evil`
	b.Spec.Mappings = []MappingRuleSpec{{Local: []MappingLocalRuleSpec{{Groups: "{0}"}}}}
	b.Spec.Groups = []FederationGroupSpec{{
		Name:            "g",
		RoleAssignments: []FederationRoleAssignmentSpec{{Role: "member"}},
	}}
	b.Spec.ExtraOptions = map[string]string{"page_size": "100"}

	_, err := w.ValidateCreate(context.Background(), b)
	g.Expect(err).To(HaveOccurred())
	msg := err.Error()
	g.Expect(msg).To(ContainSubstring("http:// or https://"))
	g.Expect(msg).To(ContainSubstring(`data key is fixed ("clientSecret")`))
	g.Expect(msg).To(ContainSubstring("identity provider name collides"))
	g.Expect(msg).To(ContainSubstring("must not contain newline, carriage-return, or double-quote"))
	g.Expect(msg).To(ContainSubstring("at least one remote entry"))
	g.Expect(msg).To(ContainSubstring("exactly one of project or domain"))
	g.Expect(msg).To(ContainSubstring("only supported on type LDAP"))
}

// samlInlineMetadata is a minimal valid single EntityDescriptor whose entityID
// matches validSAMLBackend()'s idpEntityID.
const samlInlineMetadata = `<EntityDescriptor xmlns="urn:oasis:names:tc:SAML:2.0:metadata" entityID="https://idp.example.com/realms/forge">` +
	`<IDPSSODescriptor protocolSupportEnumeration="urn:oasis:names:tc:SAML:2.0:protocol"/></EntityDescriptor>`

func validSAMLBackend() *KeystoneIdentityBackend {
	return &KeystoneIdentityBackend{
		ObjectMeta: metav1.ObjectMeta{Name: "corp-saml", Namespace: "openstack"},
		Spec: KeystoneIdentityBackendSpec{
			KeystoneRef: KeystoneRefSpec{Name: "keystone"},
			Domain: DomainSpec{
				Name:           "saml",
				Mode:           DomainModeManage,
				DeletionPolicy: DomainDeletionPolicyRetain,
			},
			Type: IdentityBackendTypeSAML,
			SAML: &SAMLBackendSpec{
				IdPEntityID: "https://idp.example.com/realms/forge",
				IdPMetadata: SAMLIdPMetadataSpec{
					SecretRef: &commonv1.SecretRefSpec{Name: "corp-saml-idp-metadata"},
				},
			},
		},
	}
}

func TestIdentityBackendDefault_MaterializesSAMLDefaults(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneIdentityBackendWebhook{}

	b := validSAMLBackend()
	g.Expect(w.Default(context.Background(), b)).To(Succeed())
	g.Expect(b.Spec.SAML.ProtocolID).To(Equal("mapped"))
	g.Expect(b.Spec.SAML.IdentityProviderName).To(Equal("corp-saml"))
	g.Expect(b.Spec.SAML.RemoteIDAttribute).To(Equal("HTTP_MELLON_IDP"))
}

// Default() then validate() on a bare SAML object surfaces the genuinely
// missing required fields, not a nil-pointer panic.
func TestIdentityBackendDefaultThenValidate_ZeroValueSAMLObject(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneIdentityBackendWebhook{}

	b := &KeystoneIdentityBackend{
		ObjectMeta: metav1.ObjectMeta{Name: "zero-saml", Namespace: "openstack"},
		Spec: KeystoneIdentityBackendSpec{
			KeystoneRef: KeystoneRefSpec{Name: "keystone"},
			Domain:      DomainSpec{Name: "saml"},
			Type:        IdentityBackendTypeSAML,
			SAML:        &SAMLBackendSpec{},
		},
	}
	g.Expect(w.Default(context.Background(), b)).To(Succeed())

	_, err := w.ValidateCreate(context.Background(), b)
	g.Expect(err).To(HaveOccurred())
	msg := err.Error()
	g.Expect(msg).To(ContainSubstring("idpEntityID must be set"))
	// A bare idpMetadata block has zero sources set.
	g.Expect(msg).To(ContainSubstring("exactly one of inline, secretRef, or url"))
}

func TestIdentityBackendValidate_AcceptsValidSAMLBackend(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneIdentityBackendWebhook{}

	b := validSAMLBackend()
	g.Expect(w.Default(context.Background(), b)).To(Succeed())
	_, err := w.ValidateCreate(context.Background(), b)
	g.Expect(err).NotTo(HaveOccurred())

	// Inline metadata whose entityID matches is also accepted.
	inline := validSAMLBackend()
	inline.Spec.SAML.IdPMetadata = SAMLIdPMetadataSpec{Inline: samlInlineMetadata}
	g.Expect(w.Default(context.Background(), inline)).To(Succeed())
	_, err = w.ValidateCreate(context.Background(), inline)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestIdentityBackendValidate_RejectsSAMLUnionMismatch(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneIdentityBackendWebhook{}

	// type SAML without spec.saml.
	b := validSAMLBackend()
	b.Spec.SAML = nil
	_, err := w.ValidateCreate(context.Background(), b)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("type SAML requires spec.saml"))

	// spec.saml alongside type LDAP.
	b2 := validIdentityBackend()
	b2.Spec.SAML = validSAMLBackend().Spec.SAML
	_, err = w.ValidateCreate(context.Background(), b2)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("type SAML requires spec.saml"))
}

func TestIdentityBackendValidate_RejectsSAMLMetadataSourceCount(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneIdentityBackendWebhook{}

	// Zero sources.
	zero := validSAMLBackend()
	zero.Spec.SAML.IdPMetadata = SAMLIdPMetadataSpec{}
	_, err := w.ValidateCreate(context.Background(), zero)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("exactly one of inline, secretRef, or url"))

	// Two sources.
	two := validSAMLBackend()
	two.Spec.SAML.IdPMetadata = SAMLIdPMetadataSpec{
		SecretRef: &commonv1.SecretRefSpec{Name: "corp-saml-idp-metadata"},
		URL:       "https://idp.example.com/metadata",
	}
	_, err = w.ValidateCreate(context.Background(), two)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("exactly one of inline, secretRef, or url"))
}

func TestIdentityBackendValidate_RejectsSAMLMetadataURLScheme(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneIdentityBackendWebhook{}

	b := validSAMLBackend()
	b.Spec.SAML.IdPMetadata = SAMLIdPMetadataSpec{URL: "ldap://not-a-metadata-url"}
	_, err := w.ValidateCreate(context.Background(), b)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("url must use https://"))
}

// The IdP metadata document is the assertion-signing trust anchor, so a
// plaintext http:// fetch must be rejected (unlike the OIDC endpoints, which
// tolerate http). A valid https:// URL is accepted.
func TestIdentityBackendValidate_RejectsSAMLMetadataURLPlaintext(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneIdentityBackendWebhook{}

	plaintext := validSAMLBackend()
	plaintext.Spec.SAML.IdPMetadata = SAMLIdPMetadataSpec{URL: "http://idp.corp.internal/metadata"}
	g.Expect(w.Default(context.Background(), plaintext)).To(Succeed())
	_, err := w.ValidateCreate(context.Background(), plaintext)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("url must use https://"))

	secure := validSAMLBackend()
	secure.Spec.SAML.IdPMetadata = SAMLIdPMetadataSpec{URL: "https://idp.corp.internal/metadata"}
	g.Expect(w.Default(context.Background(), secure)).To(Succeed())
	_, err = w.ValidateCreate(context.Background(), secure)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestIdentityBackendValidate_RejectsSAMLFixedKeyContracts(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneIdentityBackendWebhook{}

	metaKey := validSAMLBackend()
	metaKey.Spec.SAML.IdPMetadata.SecretRef.Key = "custom"
	_, err := w.ValidateCreate(context.Background(), metaKey)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring(`data key is fixed ("idp-metadata.xml")`))

	certKey := validSAMLBackend()
	certKey.Spec.SAML.SP = &SAMLSPSpec{CertificateSecretRef: &commonv1.SecretRefSpec{Name: "sp-cert", Key: "custom"}}
	_, err = w.ValidateCreate(context.Background(), certKey)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring(`data keys are fixed ("tls.crt" and "tls.key")`))
}

func TestIdentityBackendValidate_RejectsSAMLRemoteIDAttributeWithoutHTTPPrefix(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneIdentityBackendWebhook{}

	b := validSAMLBackend()
	b.Spec.SAML.RemoteIDAttribute = "MELLON_IDP" // missing HTTP_ prefix
	_, err := w.ValidateCreate(context.Background(), b)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("^HTTP_[A-Za-z0-9_]+$"))
}

func TestIdentityBackendValidate_RejectsSAMLProtocolIDSectionCollision(t *testing.T) {
	w := &KeystoneIdentityBackendWebhook{}

	// memcache is the section reconcileConfig writes unconditionally (the
	// server list); a SAML protocolID of "memcache" would have its
	// remote_id_attribute clobbered, so it must collide like every other
	// operator-owned section.
	for _, protocolID := range []string{"openid", "cache", "memcache"} {
		t.Run(protocolID, func(t *testing.T) {
			g := NewGomegaWithT(t)
			b := validSAMLBackend()
			b.Spec.SAML.ProtocolID = protocolID
			_, err := w.ValidateCreate(context.Background(), b)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("collides with the operator-owned keystone.conf section"))
		})
	}
}

func TestIdentityBackendValidate_RejectsSAMLForwardAttributes(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneIdentityBackendWebhook{}

	bad := validSAMLBackend()
	bad.Spec.SAML.ForwardAttributes = []string{"user-name"} // dash not allowed
	_, err := w.ValidateCreate(context.Background(), bad)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("^[A-Za-z0-9_]+$"))

	dup := validSAMLBackend()
	dup.Spec.SAML.ForwardAttributes = []string{"username", "username"}
	_, err = w.ValidateCreate(context.Background(), dup)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("Duplicate value"))
}

func TestIdentityBackendValidate_RejectsSAMLInlineEntityIDMismatch(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneIdentityBackendWebhook{}

	mismatch := validSAMLBackend()
	mismatch.Spec.SAML.IdPEntityID = "https://other.example.com/idp"
	mismatch.Spec.SAML.IdPMetadata = SAMLIdPMetadataSpec{Inline: samlInlineMetadata}
	_, err := w.ValidateCreate(context.Background(), mismatch)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("does not match spec.saml.idpEntityID"))

	// An EntitiesDescriptor aggregate is rejected as not-a-single-EntityDescriptor.
	agg := validSAMLBackend()
	agg.Spec.SAML.IdPMetadata = SAMLIdPMetadataSpec{
		Inline: `<EntitiesDescriptor xmlns="urn:oasis:names:tc:SAML:2.0:metadata"/>`,
	}
	_, err = w.ValidateCreate(context.Background(), agg)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("not a single SAML EntityDescriptor"))
}

// Cross-CR SAML checks: at most one SAML backend per Keystone, and the
// identityProviderName collision rule spans OIDC and SAML siblings.
func TestIdentityBackendValidate_SAMLSiblingChecks(t *testing.T) {
	g := NewGomegaWithT(t)
	s := identityBackendScheme(t)

	// A second SAML backend on the same Keystone is rejected.
	samlSibling := validSAMLBackend()
	samlSibling.Name = "existing-saml"
	samlSibling.Spec.Domain.Name = "saml-existing"
	samlSibling.Spec.SAML.IdentityProviderName = "existing-saml"
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(samlSibling).Build()
	w := &KeystoneIdentityBackendWebhook{Client: c}

	b := validSAMLBackend()
	_, err := w.ValidateCreate(context.Background(), b)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("at most one SAML backend per Keystone"))

	// An OIDC sibling whose identityProviderName collides with a new SAML
	// backend is rejected (cross-type collision).
	oidcSibling := validOIDCBackend()
	oidcSibling.Name = "existing-oidc"
	oidcSibling.Spec.Domain.Name = "sso-existing"
	oidcSibling.Spec.OIDC.IdentityProviderName = "shared-idp"
	c2 := fake.NewClientBuilder().WithScheme(s).WithObjects(oidcSibling).Build()
	w2 := &KeystoneIdentityBackendWebhook{Client: c2}

	b2 := validSAMLBackend()
	b2.Spec.SAML.IdentityProviderName = "shared-idp"
	_, err = w2.ValidateCreate(context.Background(), b2)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("identity provider name collides"))

	// A Terminating SAML sibling does not block a replacement.
	terminating := validSAMLBackend()
	terminating.Name = "terminating-saml"
	now := metav1.Now()
	terminating.DeletionTimestamp = &now
	terminating.Finalizers = []string{"keystone.openstack.c5c3.io/identitybackend"}
	c3 := fake.NewClientBuilder().WithScheme(s).WithObjects(terminating).Build()
	w3 := &KeystoneIdentityBackendWebhook{Client: c3}
	_, err = w3.ValidateCreate(context.Background(), validSAMLBackend())
	g.Expect(err).NotTo(HaveOccurred())
}

// Aggregate SAML test proving error accumulation across the SAML rules.
func TestIdentityBackendValidateCreate_RunsAllSAMLValidations(t *testing.T) {
	g := NewGomegaWithT(t)
	s := identityBackendScheme(t)

	sibling := validSAMLBackend()
	sibling.Name = "existing-saml"
	sibling.Spec.Domain.Name = "saml-existing"
	sibling.Spec.SAML.IdentityProviderName = "shared-idp"
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(sibling).Build()
	w := &KeystoneIdentityBackendWebhook{Client: c}

	b := validSAMLBackend()
	b.Spec.SAML.IdentityProviderName = "shared-idp"      // collides + second SAML
	b.Spec.SAML.RemoteIDAttribute = "MELLON_IDP"         // missing HTTP_ prefix
	b.Spec.SAML.ProtocolID = "federation"                // reserved section
	b.Spec.SAML.ForwardAttributes = []string{"bad-attr"} // charset
	b.Spec.SAML.IdPMetadata = SAMLIdPMetadataSpec{}      // zero sources
	b.Spec.SAML.SP = &SAMLSPSpec{CertificateSecretRef: &commonv1.SecretRefSpec{Name: "sp", Key: "x"}}
	b.Spec.Mappings = []MappingRuleSpec{{Local: []MappingLocalRuleSpec{{Groups: "{0}"}}}}
	b.Spec.ExtraOptions = map[string]string{"page_size": "100"} // only supported on LDAP

	_, err := w.ValidateCreate(context.Background(), b)
	g.Expect(err).To(HaveOccurred())
	msg := err.Error()
	g.Expect(msg).To(ContainSubstring("identity provider name collides"))
	g.Expect(msg).To(ContainSubstring("at most one SAML backend per Keystone"))
	g.Expect(msg).To(ContainSubstring("^HTTP_[A-Za-z0-9_]+$"))
	g.Expect(msg).To(ContainSubstring("collides with the operator-owned keystone.conf section"))
	g.Expect(msg).To(ContainSubstring("^[A-Za-z0-9_]+$"))
	g.Expect(msg).To(ContainSubstring("exactly one of inline, secretRef, or url"))
	g.Expect(msg).To(ContainSubstring(`data keys are fixed ("tls.crt" and "tls.key")`))
	g.Expect(msg).To(ContainSubstring("at least one remote entry"))
	g.Expect(msg).To(ContainSubstring("only supported on type LDAP"))
}
