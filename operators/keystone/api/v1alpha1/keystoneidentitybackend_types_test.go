// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// testHelperBackend returns a backend named my-backend with the given spec.
func testHelperBackend(spec KeystoneIdentityBackendSpec) KeystoneIdentityBackend {
	return KeystoneIdentityBackend{ObjectMeta: metav1.ObjectMeta{Name: "my-backend"}, Spec: spec}
}

func TestIsFederationType(t *testing.T) {
	tests := []struct {
		name    string
		backend KeystoneIdentityBackend
		want    bool
	}{
		{"LDAP", testHelperBackend(KeystoneIdentityBackendSpec{Type: IdentityBackendTypeLDAP}), false},
		{"OIDC", testHelperBackend(KeystoneIdentityBackendSpec{Type: IdentityBackendTypeOIDC}), true},
		{"SAML", testHelperBackend(KeystoneIdentityBackendSpec{Type: IdentityBackendTypeSAML}), true},
		{"empty type", testHelperBackend(KeystoneIdentityBackendSpec{}), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.backend.IsFederationType(); got != tt.want {
				t.Errorf("IsFederationType() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFederationRemoteID(t *testing.T) {
	tests := []struct {
		name    string
		backend KeystoneIdentityBackend
		want    string
	}{
		{
			"OIDC issuer",
			testHelperBackend(KeystoneIdentityBackendSpec{
				Type: IdentityBackendTypeOIDC,
				OIDC: &OIDCBackendSpec{Issuer: "https://idp/realm"},
			}),
			"https://idp/realm",
		},
		{
			"SAML entityID",
			testHelperBackend(KeystoneIdentityBackendSpec{
				Type: IdentityBackendTypeSAML,
				SAML: &SAMLBackendSpec{IdPEntityID: "https://idp/saml"},
			}),
			"https://idp/saml",
		},
		{
			"both blocks prefer the OIDC issuer",
			testHelperBackend(KeystoneIdentityBackendSpec{
				OIDC: &OIDCBackendSpec{Issuer: "https://idp/realm"},
				SAML: &SAMLBackendSpec{IdPEntityID: "https://idp/saml"},
			}),
			"https://idp/realm",
		},
		{"neither block", testHelperBackend(KeystoneIdentityBackendSpec{Type: IdentityBackendTypeLDAP}), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.backend.FederationRemoteID(); got != tt.want {
				t.Errorf("FederationRemoteID() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEffectiveIdentityProviderName(t *testing.T) {
	tests := []struct {
		name    string
		backend KeystoneIdentityBackend
		want    string
	}{
		{
			"OIDC name",
			testHelperBackend(KeystoneIdentityBackendSpec{
				Type: IdentityBackendTypeOIDC,
				OIDC: &OIDCBackendSpec{IdentityProviderName: "corp"},
			}),
			"corp",
		},
		{
			"SAML name",
			testHelperBackend(KeystoneIdentityBackendSpec{
				Type: IdentityBackendTypeSAML,
				SAML: &SAMLBackendSpec{IdentityProviderName: "corp-saml"},
			}),
			"corp-saml",
		},
		{
			"OIDC block without a name falls back to the backend name",
			testHelperBackend(KeystoneIdentityBackendSpec{Type: IdentityBackendTypeOIDC, OIDC: &OIDCBackendSpec{}}),
			"my-backend",
		},
		{
			"SAML block without a name falls back to the backend name",
			testHelperBackend(KeystoneIdentityBackendSpec{Type: IdentityBackendTypeSAML, SAML: &SAMLBackendSpec{}}),
			"my-backend",
		},
		{
			"unnamed OIDC block falls through to the SAML name",
			testHelperBackend(KeystoneIdentityBackendSpec{
				OIDC: &OIDCBackendSpec{},
				SAML: &SAMLBackendSpec{IdentityProviderName: "corp-saml"},
			}),
			"corp-saml",
		},
		{"LDAP uses the backend name", testHelperBackend(KeystoneIdentityBackendSpec{Type: IdentityBackendTypeLDAP}), "my-backend"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.backend.EffectiveIdentityProviderName(); got != tt.want {
				t.Errorf("EffectiveIdentityProviderName() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEffectiveProtocolID(t *testing.T) {
	tests := []struct {
		name    string
		backend KeystoneIdentityBackend
		want    string
	}{
		{"LDAP has no protocol", testHelperBackend(KeystoneIdentityBackendSpec{Type: IdentityBackendTypeLDAP}), ""},
		{
			"OIDC default",
			testHelperBackend(KeystoneIdentityBackendSpec{Type: IdentityBackendTypeOIDC, OIDC: &OIDCBackendSpec{}}),
			"openid",
		},
		{
			"OIDC override",
			testHelperBackend(KeystoneIdentityBackendSpec{
				Type: IdentityBackendTypeOIDC,
				OIDC: &OIDCBackendSpec{ProtocolID: "oidc2"},
			}),
			"oidc2",
		},
		{"OIDC type without a block", testHelperBackend(KeystoneIdentityBackendSpec{Type: IdentityBackendTypeOIDC}), "openid"},
		{
			"SAML default",
			testHelperBackend(KeystoneIdentityBackendSpec{Type: IdentityBackendTypeSAML, SAML: &SAMLBackendSpec{}}),
			"mapped",
		},
		{
			"SAML override",
			testHelperBackend(KeystoneIdentityBackendSpec{
				Type: IdentityBackendTypeSAML,
				SAML: &SAMLBackendSpec{ProtocolID: "saml2"},
			}),
			"saml2",
		},
		{"SAML type without a block", testHelperBackend(KeystoneIdentityBackendSpec{Type: IdentityBackendTypeSAML}), "mapped"},
		{"unknown type", testHelperBackend(KeystoneIdentityBackendSpec{Type: "Foo"}), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.backend.EffectiveProtocolID(); got != tt.want {
				t.Errorf("EffectiveProtocolID() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEffectiveRemoteIDAttribute(t *testing.T) {
	tests := []struct {
		name    string
		backend KeystoneIdentityBackend
		want    string
	}{
		{
			"OIDC default",
			testHelperBackend(KeystoneIdentityBackendSpec{Type: IdentityBackendTypeOIDC, OIDC: &OIDCBackendSpec{}}),
			"HTTP_OIDC_ISS",
		},
		{
			"OIDC override",
			testHelperBackend(KeystoneIdentityBackendSpec{
				Type: IdentityBackendTypeOIDC,
				OIDC: &OIDCBackendSpec{RemoteIDAttribute: "HTTP_X"},
			}),
			"HTTP_X",
		},
		{
			"SAML default",
			testHelperBackend(KeystoneIdentityBackendSpec{Type: IdentityBackendTypeSAML, SAML: &SAMLBackendSpec{}}),
			"HTTP_MELLON_IDP",
		},
		{
			"SAML override",
			testHelperBackend(KeystoneIdentityBackendSpec{
				Type: IdentityBackendTypeSAML,
				SAML: &SAMLBackendSpec{RemoteIDAttribute: "HTTP_Y"},
			}),
			"HTTP_Y",
		},
		{"neither block", testHelperBackend(KeystoneIdentityBackendSpec{Type: IdentityBackendTypeLDAP}), "HTTP_OIDC_ISS"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.backend.EffectiveRemoteIDAttribute(); got != tt.want {
				t.Errorf("EffectiveRemoteIDAttribute() = %q, want %q", got, tt.want)
			}
		})
	}
}
