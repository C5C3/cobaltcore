// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"testing"

	. "github.com/onsi/gomega"

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
