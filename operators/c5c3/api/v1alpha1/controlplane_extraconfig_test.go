// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"testing"

	. "github.com/onsi/gomega"
	"k8s.io/utils/ptr"

	commonv1 "github.com/c5c3/forge/internal/common/types"
)

// TestMergedExtraConfig_PerServiceWinsPerKey verifies the merge semantics: a key
// present in both blocks yields the per-service value, global-only keys in the
// same section survive, and global-only sections survive.
func TestMergedExtraConfig_PerServiceWinsPerKey(t *testing.T) {
	g := NewGomegaWithT(t)

	global := map[string]map[string]string{
		"DEFAULT": {
			"debug":         "false",
			"transport_url": "rabbit://global",
		},
		"oslo_messaging": {
			"driver": "messagingv2",
		},
	}
	service := map[string]map[string]string{
		"DEFAULT": {
			"debug": "true",
		},
	}

	merged := MergedExtraConfig(global, service)

	// Per-service value wins the shared key.
	g.Expect(merged["DEFAULT"]["debug"]).To(Equal("true"))
	// Global-only key in a shared section survives.
	g.Expect(merged["DEFAULT"]["transport_url"]).To(Equal("rabbit://global"))
	// Global-only section survives.
	g.Expect(merged["oslo_messaging"]["driver"]).To(Equal("messagingv2"))
}

// TestMergedExtraConfig_NilWhenBothNil verifies two nil inputs normalize to a
// nil result rather than an empty map, so an absent field is projected.
func TestMergedExtraConfig_NilWhenBothNil(t *testing.T) {
	g := NewGomegaWithT(t)

	g.Expect(MergedExtraConfig(nil, nil)).To(BeNil())
}

// TestMergedExtraConfig_NilWhenBothEmpty verifies two empty (non-nil) maps
// normalize to nil, guarding the length-zero normalization against an empty
// merge result config.MergeDefaults would otherwise return.
func TestMergedExtraConfig_NilWhenBothEmpty(t *testing.T) {
	g := NewGomegaWithT(t)

	merged := MergedExtraConfig(
		map[string]map[string]string{},
		map[string]map[string]string{},
	)
	g.Expect(merged).To(BeNil())
}

// TestMergedExtraConfig_ReturnsIndependentCopy verifies mutating the returned
// map never writes back through to the single input that was set, exercising
// both the global-only and the service-only paths.
func TestMergedExtraConfig_ReturnsIndependentCopy(t *testing.T) {
	g := NewGomegaWithT(t)

	t.Run("only global set", func(t *testing.T) {
		global := map[string]map[string]string{
			"DEFAULT": {"debug": "false"},
		}
		merged := MergedExtraConfig(global, nil)

		// Add a key to an existing section and a whole new section.
		merged["DEFAULT"]["extra"] = "1"
		merged["new_section"] = map[string]string{"k": "v"}

		g.Expect(global["DEFAULT"]).To(HaveLen(1),
			"mutating the merged map must not add a key to the global input")
		g.Expect(global).NotTo(HaveKey("new_section"),
			"mutating the merged map must not add a section to the global input")
	})

	t.Run("only service set", func(t *testing.T) {
		service := map[string]map[string]string{
			"DEFAULT": {"debug": "true"},
		}
		merged := MergedExtraConfig(nil, service)

		merged["DEFAULT"]["extra"] = "1"
		merged["new_section"] = map[string]string{"k": "v"}

		g.Expect(service["DEFAULT"]).To(HaveLen(1),
			"mutating the merged map must not add a key to the service input")
		g.Expect(service).NotTo(HaveKey("new_section"),
			"mutating the merged map must not add a section to the service input")
	})
}

// TestDerivedPublicEndpoint pins the shared derivation used by both the
// identity-backend projection and the admission-time trusted-dashboard check.
func TestDerivedPublicEndpoint(t *testing.T) {
	g := NewGomegaWithT(t)

	for _, tc := range []struct {
		name string
		hz   *ServiceHorizonSpec
		want string
	}{
		{
			name: "nil receiver",
			hz:   nil,
			want: "",
		},
		{
			name: "explicit publicEndpoint with trailing slash is trimmed",
			hz:   &ServiceHorizonSpec{PublicEndpoint: "https://horizon.example.com:8443/"},
			want: "https://horizon.example.com:8443",
		},
		{
			name: "explicit publicEndpoint without slash is verbatim",
			hz:   &ServiceHorizonSpec{PublicEndpoint: "https://horizon.example.com"},
			want: "https://horizon.example.com",
		},
		{
			name: "gateway hostname derives the default-443 form",
			hz:   &ServiceHorizonSpec{Gateway: &commonv1.GatewaySpec{Hostname: "horizon.127-0-0-1.nip.io"}},
			want: "https://horizon.127-0-0-1.nip.io",
		},
		{
			name: "neither set yields empty",
			hz:   &ServiceHorizonSpec{},
			want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g.Expect(tc.hz.DerivedPublicEndpoint()).To(Equal(tc.want))
		})
	}
}

// TestValidateExtraConfigOwnership_ForbidsBarbicanRejectedKeys pins the three
// Rejected barbican keys. None of them is read from the rendered file at runtime
// — each arrives through an env override — so honoring the override buys
// nothing and copies credential material into the config Secret every API pod
// mounts. The check runs from whichever block carries the key, since the merged
// value is what would be rendered.
func TestValidateExtraConfigOwnership_ForbidsBarbicanRejectedKeys(t *testing.T) {
	for _, tc := range []struct {
		section string
		key     string
	}{
		{"keystone_authtoken", "password"},
		{"vault_plugin", "approle_secret_id"},
		{"vault_plugin", "root_token_id"},
	} {
		t.Run(tc.section+" "+tc.key, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := barbicanControlPlane()
			cp.Spec.Services.Barbican.ExtraConfig = map[string]map[string]string{
				tc.section: {tc.key: "s3cr3t"},
			}

			_, errs := validateExtraConfigOwnership(cp)
			g.Expect(errs.ToAggregate()).To(HaveOccurred())
			g.Expect(errs.ToAggregate().Error()).To(ContainSubstring(
				"spec.services.barbican.extraConfig[" + tc.section + "][" + tc.key + "]"))
			g.Expect(errs.ToAggregate().Error()).To(ContainSubstring("must not be set in extraConfig"))
		})
	}

	t.Run("in globalExtraConfig", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := barbicanControlPlane()
		cp.Spec.GlobalExtraConfig = map[string]map[string]string{
			"vault_plugin": {"root_token_id": "s.rootrootroot"},
		}

		_, errs := validateExtraConfigOwnership(cp)
		g.Expect(errs.ToAggregate()).To(HaveOccurred())
		g.Expect(errs.ToAggregate().Error()).To(ContainSubstring(
			"spec.globalExtraConfig[vault_plugin][root_token_id]"))
	})
}

// TestValidateExtraConfigCatalogs_ExemptsBarbicanStoreSectionsByPrefix pins the
// prefix expansion barbican needs and no other service does: the per-store
// [secretstore:<name>] sections are named after the BarbicanSecretStore CRs
// attached to a Barbican, so no release catalog lists them and no static
// exemption can spell them. Every other unknown section still fails, which is
// what keeps the exemption from swallowing real typos.
func TestValidateExtraConfigCatalogs_ExemptsBarbicanStoreSectionsByPrefix(t *testing.T) {
	t.Run("a per-store section is skipped whole", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := barbicanControlPlane()
		cp.Spec.Services.Barbican.ExtraConfig = map[string]map[string]string{
			"secretstore:foo": {"global_default": "True", "secret_store_plugin": "vault_plugin"},
		}

		_, errs := validateExtraConfigCatalogs(cp)
		g.Expect(errs).To(BeEmpty())
	})

	t.Run("a section that merely resembles one is not", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := barbicanControlPlane()
		cp.Spec.Services.Barbican.ExtraConfig = map[string]map[string]string{
			"secretstore_foo": {"global_default": "True"},
		}

		_, errs := validateExtraConfigCatalogs(cp)
		g.Expect(errs.ToAggregate()).To(HaveOccurred())
		g.Expect(errs.ToAggregate().Error()).To(ContainSubstring(
			"spec.services.barbican.extraConfig[secretstore_foo][global_default]"))
		g.Expect(errs.ToAggregate().Error()).To(ContainSubstring(
			"no such section in the barbican 2025.2 option catalog"))
	})

	t.Run("an unknown option in a known section is rejected", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := barbicanControlPlane()
		cp.Spec.Services.Barbican.ExtraConfig = map[string]map[string]string{
			"DEFAULT": {"host_hrefs": "https://barbican.example.com"},
		}

		_, errs := validateExtraConfigCatalogs(cp)
		g.Expect(errs.ToAggregate()).To(HaveOccurred())
		g.Expect(errs.ToAggregate().Error()).To(ContainSubstring(
			"no such option in the barbican 2025.2 option catalog"))
	})
}

// TestControlPlaneExtraConfigCatalogInputsChanged_Barbican pins the update gate
// for the barbican leg: the catalog family re-runs when the barbican block is
// added, dropped, or edited, and stays gated off for an update that leaves it
// alone.
func TestControlPlaneExtraConfigCatalogInputsChanged_Barbican(t *testing.T) {
	withExtraConfig := func(cfg map[string]map[string]string) *ControlPlane {
		cp := barbicanControlPlane()
		cp.Spec.Services.Barbican.ExtraConfig = cfg
		return cp
	}

	for _, tc := range []struct {
		name     string
		oldCP    *ControlPlane
		newCP    *ControlPlane
		expected bool
	}{
		{
			name:     "the barbican block is newly declared",
			oldCP:    validControlPlane(),
			newCP:    barbicanControlPlane(),
			expected: true,
		},
		{
			name:     "the barbican block is dropped",
			oldCP:    barbicanControlPlane(),
			newCP:    validControlPlane(),
			expected: true,
		},
		{
			name:     "the barbican extraConfig changes",
			oldCP:    withExtraConfig(map[string]map[string]string{"DEFAULT": {"debug": "false"}}),
			newCP:    withExtraConfig(map[string]map[string]string{"DEFAULT": {"debug": "true"}}),
			expected: true,
		},
		{
			name:  "an unrelated barbican edit leaves the gate closed",
			oldCP: barbicanControlPlane(),
			newCP: func() *ControlPlane {
				cp := barbicanControlPlane()
				cp.Spec.Services.Barbican.Replicas = ptr.To(int32(5))
				return cp
			}(),
			expected: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			g.Expect(controlPlaneExtraConfigCatalogInputsChanged(tc.oldCP, tc.newCP)).To(Equal(tc.expected))
		})
	}
}
