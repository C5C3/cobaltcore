// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the two K-ORC clouds.yaml builders and the mode-aware auth_url /
// endpoint_type / region resolvers they render from.
package controller

import (
	"testing"

	. "github.com/onsi/gomega"

	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
)

// --- managed-mode golden output ---
//
// The two goldens below are FULL-STRING equality against the document the
// operator renders today. They are the byte-identical guarantee for managed mode:
// mode-awareness must not perturb a single character of the managed clouds.yaml,
// because a changed byte churns the operator-owned Secret, re-pushes to OpenBao
// and forces an ESO re-sync on every upgraded ControlPlane.

const managedAppCredCloudsYAMLGolden = `clouds:
  admin:
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
  admin:
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

func TestBuildPasswordCloudsYAML_External(t *testing.T) {
	g := NewWithT(t)
	cp := korcExternalControlPlane()
	cp.Spec.Region = "eu-de-1"
	cp.Spec.Services.Keystone.External.EndpointType = c5c3v1alpha1.ExternalEndpointTypeInternal
	cp.Spec.KORC.AdminCredential.CloudCredentialsRef.CloudName = "brownfield"

	got := buildPasswordCloudsYAML(cp, testAdminPassword)

	g.Expect(got).To(ContainSubstring("\n  brownfield:\n"))
	g.Expect(got).To(ContainSubstring(`auth_url: "https://keystone.example.com/v3"`))
	g.Expect(got).NotTo(ContainSubstring(".svc:5000"))
	g.Expect(got).To(ContainSubstring("endpoint_type: internal"))
	g.Expect(got).To(ContainSubstring(`region_name: "eu-de-1"`))
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

		g.Expect(buildPasswordCloudsYAML(cp, testAdminPassword)).To(ContainSubstring("\n  admin:\n"))
	})
}
