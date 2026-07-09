// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the two K-ORC clouds.yaml builders and the mode-aware auth_url /
// endpoint_type / region resolvers they render from.
package controller

import (
	"testing"

	. "github.com/onsi/gomega"
	"sigs.k8s.io/yaml"

	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
)

// --- managed-mode golden output ---
//
// The two goldens below are FULL-STRING equality against the document the
// operator renders today. They are the byte-identical guarantee for managed mode:
// mode-awareness must not perturb a single character of the managed clouds.yaml,
// because a changed byte churns the operator-owned Secret, re-pushes to OpenBao
// and forces an ESO re-sync on every upgraded ControlPlane.
//
// BOTH cloud keys are quoted scalars: rendering the free-form cloudName through
// korcCloudName costs one %q (see its doc comment for why raw is unsafe). YAML
// parses `"admin":` and `admin:` identically, so K-ORC is unaffected; the quoting
// costs each already-deployed managed ControlPlane exactly one re-push, after
// which the document converges and the goldens hold byte-for-byte.

const managedAppCredCloudsYAMLGolden = `clouds:
  "admin":
    auth:
      auth_url: "http://cp-keystone.default.svc:5000/v3"
      application_credential_id: "ac-id"
      application_credential_secret: "ac-secret"
    auth_type: v3applicationcredential
    region_name: "RegionOne"
    endpoint_type: internal
    identity_api_version: 3
`

const managedPasswordCloudsYAMLGolden = `clouds:
  "admin":
    auth:
      auth_url: "http://cp-keystone.default.svc:5000/v3"
      username: "admin"
      password: "super-secret-admin-password"
      project_name: "admin"
      user_domain_name: "Default"
      project_domain_name: "Default"
    region_name: "RegionOne"
    endpoint_type: internal
    identity_api_version: 3
`

func TestBuildAppCredCloudsYAML_ManagedGolden(t *testing.T) {
	g := NewWithT(t)
	cp := korcControlPlane()

	g.Expect(buildAppCredCloudsYAML(cp, "ac-id", "ac-secret")).To(Equal(managedAppCredCloudsYAMLGolden),
		"managed-mode app-credential clouds.yaml must stay byte-identical")
}

func TestBuildPasswordCloudsYAML_ManagedGolden(t *testing.T) {
	g := NewWithT(t)
	cp := korcControlPlane()

	g.Expect(buildPasswordCloudsYAML(cp, testAdminPassword)).To(Equal(managedPasswordCloudsYAMLGolden),
		"managed-mode password clouds.yaml must stay byte-identical")
}

// TestBuildCloudsYAML_ManagedIgnoresExternalBlock proves the mode discriminator —
// not the presence of the external block — decides the rendering. A managed CR
// that somehow carries an external block (a mode flip that left the block behind)
// must keep dialling the in-cluster Service, never the external endpoint.
func TestBuildCloudsYAML_ManagedIgnoresExternalBlock(t *testing.T) {
	g := NewWithT(t)
	cp := korcControlPlane()
	cp.Spec.Services.Keystone.External = &c5c3v1alpha1.ExternalKeystoneSpec{
		AuthURL:      "https://keystone.example.com/v3",
		EndpointType: c5c3v1alpha1.ExternalEndpointTypeAdmin,
	}

	g.Expect(buildAppCredCloudsYAML(cp, "ac-id", "ac-secret")).To(Equal(managedAppCredCloudsYAMLGolden))
	g.Expect(buildPasswordCloudsYAML(cp, testAdminPassword)).To(Equal(managedPasswordCloudsYAMLGolden))
}

// --- External mode ---

func TestBuildAppCredCloudsYAML_External(t *testing.T) {
	cases := []struct {
		name             string
		endpointType     c5c3v1alpha1.ExternalEndpointType
		region           string
		wantEndpointType string
		wantRegion       string
	}{
		{
			name:             "defaulted endpointType and region",
			wantEndpointType: "public",
			wantRegion:       "RegionOne",
		},
		{
			name:             "explicit public",
			endpointType:     c5c3v1alpha1.ExternalEndpointTypePublic,
			region:           "eu-de-1",
			wantEndpointType: "public",
			wantRegion:       "eu-de-1",
		},
		{
			name:             "explicit internal",
			endpointType:     c5c3v1alpha1.ExternalEndpointTypeInternal,
			region:           "eu-de-1",
			wantEndpointType: "internal",
			wantRegion:       "eu-de-1",
		},
		{
			name:             "explicit admin",
			endpointType:     c5c3v1alpha1.ExternalEndpointTypeAdmin,
			region:           "eu-de-1",
			wantEndpointType: "admin",
			wantRegion:       "eu-de-1",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			cp := korcExternalControlPlane()
			cp.Spec.Region = tc.region
			cp.Spec.Services.Keystone.External.EndpointType = tc.endpointType

			got := buildAppCredCloudsYAML(cp, "ac-id", "ac-secret")

			g.Expect(got).To(ContainSubstring(`auth_url: "https://keystone.example.com/v3"`),
				"External mode must dial the external authURL, never the Service DNS")
			g.Expect(got).NotTo(ContainSubstring(".svc:5000"))
			g.Expect(got).To(ContainSubstring("endpoint_type: " + tc.wantEndpointType))
			g.Expect(got).To(ContainSubstring(`region_name: "` + tc.wantRegion + `"`))
		})
	}
}

// TestBuildPasswordCloudsYAML_External covers the spike-D4 non-default identity
// shape end to end: the external endpoint, the configured endpoint_type, and the
// three admin identities — with the single domainName feeding BOTH domain keys.
func TestBuildPasswordCloudsYAML_External(t *testing.T) {
	g := NewWithT(t)
	cp := korcExternalControlPlane()
	cp.Spec.Region = "eu-de-1"
	cp.Spec.Services.Keystone.External.EndpointType = c5c3v1alpha1.ExternalEndpointTypeInternal
	cp.Spec.KORC.AdminCredential.CloudCredentialsRef.CloudName = "brownfield"
	cp.Spec.KORC.AdminCredential.UserName = "brownfield-admin"
	cp.Spec.KORC.AdminCredential.ProjectName = "platform-admin"
	cp.Spec.KORC.AdminCredential.DomainName = "heimdall"

	g.Expect(buildPasswordCloudsYAML(cp, testAdminPassword)).To(Equal(`clouds:
  "brownfield":
    auth:
      auth_url: "https://keystone.example.com/v3"
      username: "brownfield-admin"
      password: "super-secret-admin-password"
      project_name: "platform-admin"
      user_domain_name: "heimdall"
      project_domain_name: "heimdall"
    region_name: "eu-de-1"
    endpoint_type: internal
    identity_api_version: 3
`))
}

// --- cloud-key invariants (both documents) ---

// renderedCloud is the parsed subset of one clouds.yaml cloud entry the cloud-key
// assertions below inspect.
type renderedCloud struct {
	Auth struct {
		AuthURL string `json:"auth_url"`
	} `json:"auth"`
}

// parseCloudsYAML parses a rendered clouds.yaml into its cloud entries, failing
// the test when the document does not parse.
func parseCloudsYAML(g Gomega, rendered string) map[string]renderedCloud {
	var doc struct {
		Clouds map[string]renderedCloud `json:"clouds"`
	}
	g.Expect(yaml.Unmarshal([]byte(rendered), &doc)).To(Succeed(),
		"a free-form cloudName must never make the credentials document unparseable")
	return doc.Clouds
}

// cloudsYAMLBuilders enumerates BOTH documents the operator renders. The cloud-key
// invariants hold for each: the password document mints the credential, the
// app-credential document replaces it, and every downstream consumer resolves the
// one CloudCredentialsRef.CloudName against whichever is current.
var cloudsYAMLBuilders = []struct {
	name  string
	build func(*c5c3v1alpha1.ControlPlane) string
}{
	{
		name:  "app-credential",
		build: func(cp *c5c3v1alpha1.ControlPlane) string { return buildAppCredCloudsYAML(cp, "ac-id", "ac-secret") },
	},
	{
		name:  "password",
		build: func(cp *c5c3v1alpha1.ControlPlane) string { return buildPasswordCloudsYAML(cp, testAdminPassword) },
	},
}

// TestCloudsYAMLBuilders_RenderTheConfiguredCloudName is the cross-document
// invariant. reconcileAdminCredential OVERWRITES clouds.yaml with the
// app-credential document once the mint succeeds, so a builder that hardcodes
// "admin" strands every non-default cloudName: the ApplicationCredential's
// importCredRef/acCredRef (reconcile_korc.go) and the catalog Service/Endpoint CRs
// (reconcile_catalog.go) all resolve CloudCredentialsRef.CloudName against a
// document whose only cloud is named something else.
//
// The failure is SILENT. reconcileAdminCredential's own compare re-parses the very
// document it just wrote, so AdminCredentialReady reads True; K-ORC swallows the
// resulting list failures and reports them as empty imports, so the Domain/User CRs
// hang on "Waiting for OpenStack resource to be created externally" forever.
func TestCloudsYAMLBuilders_RenderTheConfiguredCloudName(t *testing.T) {
	cases := []struct {
		name      string
		cloudName string
		want      string
	}{
		{name: "default", cloudName: "admin", want: "admin"},
		{name: "non-default", cloudName: "brownfield", want: "brownfield"},
		{name: "webhook-bypassed empty", cloudName: "", want: c5c3v1alpha1.DefaultCloudName},
	}

	for _, tc := range cases {
		for _, b := range cloudsYAMLBuilders {
			t.Run(tc.name+"/"+b.name, func(t *testing.T) {
				g := NewWithT(t)
				cp := korcExternalControlPlane()
				cp.Spec.KORC.AdminCredential.CloudCredentialsRef.CloudName = tc.cloudName

				clouds := parseCloudsYAML(g, b.build(cp))

				g.Expect(clouds).To(HaveLen(1), "exactly one cloud is rendered")
				g.Expect(clouds).To(HaveKey(tc.want),
					"both documents must key on the configured cloudName, not a hardcoded one")
			})
		}
	}
}

// TestCloudsYAMLBuilders_CloudNameCannotEscapeItsMappingKey pins the quoting of the
// one free-form spec string rendered into a YAML structure position. Its schema has
// no pattern, no maxLength and no enum, so admission accepts a value that — rendered
// raw — would reshape the credentials document rather than name a cloud in it: a
// sequence item, an alias, or a multi-line value that injects sibling keys such as a
// replacement auth_url. Both documents render it, so both are asserted.
func TestCloudsYAMLBuilders_CloudNameCannotEscapeItsMappingKey(t *testing.T) {
	cases := []struct {
		name      string
		cloudName string
	}{
		{name: "sequence item", cloudName: "- x"},
		{name: "yaml alias", cloudName: "*a"},
		{name: "comment marker", cloudName: "#admin"},
		{name: "embedded quote", cloudName: `he said "hi"`},
		{name: "injected auth_url", cloudName: "admin\"\n  evil:\n    auth:\n      auth_url: \"http://attacker.example.com"},
	}

	for _, tc := range cases {
		for _, b := range cloudsYAMLBuilders {
			t.Run(tc.name+"/"+b.name, func(t *testing.T) {
				g := NewWithT(t)
				cp := korcExternalControlPlane()
				cp.Spec.KORC.AdminCredential.CloudCredentialsRef.CloudName = tc.cloudName

				clouds := parseCloudsYAML(g, b.build(cp))

				g.Expect(clouds).To(HaveLen(1), "cloudName must not inject a second cloud entry")
				g.Expect(clouds).To(HaveKey(tc.cloudName), "the cloud key must be the verbatim cloudName")
				g.Expect(clouds[tc.cloudName].Auth.AuthURL).To(Equal("https://keystone.example.com/v3"),
					"cloudName must never redirect auth_url")
			})
		}
	}
}

// --- webhook-bypass fallbacks ---
//
// The defaulting webhook normally materializes endpointType, cloudName and the
// three identities before the reconciler ever sees the CR. A CR written straight
// to etcd (or a unit fixture) skips it, so every resolver must fall back rather
// than render an empty value — an empty endpoint_type silently means "public" to
// gophercloud, which is exactly the silent-empty hazard the builders exist to
// prevent.

func TestKORCResolvers_WebhookBypassFallbacks(t *testing.T) {
	t.Run("external with empty endpointType defaults to public", func(t *testing.T) {
		g := NewWithT(t)
		cp := korcExternalControlPlane()
		cp.Spec.Services.Keystone.External.EndpointType = ""

		g.Expect(korcEndpointType(cp)).To(Equal("public"))
	})

	t.Run("external mode with a nil external block does not panic", func(t *testing.T) {
		g := NewWithT(t)
		cp := korcControlPlane()
		cp.Spec.Services.Keystone = &c5c3v1alpha1.ServiceKeystoneSpec{
			Mode: c5c3v1alpha1.KeystoneModeExternal,
		}

		g.Expect(korcEndpointType(cp)).To(Equal("public"))
		g.Expect(korcAuthURL(cp)).To(BeEmpty(),
			"an External CR with no external block has no auth_url to render")
	})

	t.Run("nil keystone block reads as managed", func(t *testing.T) {
		g := NewWithT(t)
		cp := korcControlPlane()
		cp.Spec.Services.Keystone = nil

		g.Expect(korcEndpointType(cp)).To(Equal("internal"))
		g.Expect(korcAuthURL(cp)).To(Equal("http://cp-keystone.default.svc:5000/v3"))
	})

	t.Run("empty region defaults to RegionOne", func(t *testing.T) {
		g := NewWithT(t)
		cp := korcExternalControlPlane()
		cp.Spec.Region = ""

		g.Expect(korcRegion(cp)).To(Equal("RegionOne"))
	})

	t.Run("empty cloudName defaults to admin", func(t *testing.T) {
		g := NewWithT(t)
		cp := korcExternalControlPlane()
		cp.Spec.KORC.AdminCredential.CloudCredentialsRef.CloudName = ""

		g.Expect(korcCloudName(cp)).To(Equal("admin"))
		g.Expect(buildPasswordCloudsYAML(cp, testAdminPassword)).To(ContainSubstring("\n  \"admin\":\n"))
		g.Expect(buildAppCredCloudsYAML(cp, "ac-id", "ac-secret")).To(ContainSubstring("\n  \"admin\":\n"))
	})

	t.Run("empty identities default to the stock Keystone bootstrap ones", func(t *testing.T) {
		g := NewWithT(t)
		cp := korcExternalControlPlane()
		cp.Spec.KORC.AdminCredential.UserName = ""
		cp.Spec.KORC.AdminCredential.ProjectName = ""
		cp.Spec.KORC.AdminCredential.DomainName = ""

		g.Expect(adminUserName(cp)).To(Equal("admin"))
		g.Expect(adminProjectName(cp)).To(Equal("admin"))
		g.Expect(adminDomainName(cp)).To(Equal("Default"))
	})
}
