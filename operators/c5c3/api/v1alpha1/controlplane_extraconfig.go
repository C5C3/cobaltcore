// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"strings"

	"github.com/c5c3/forge/internal/common/config"
)

// MergedExtraConfig merges the ControlPlane-wide globalExtraConfig with one
// service's own extraConfig and returns the effective INI block projected onto
// that service's child CR. Sections are unioned, the per-service value wins per
// key, and a global key with no per-service counterpart stays effective. A
// merge that produces no sections is normalized to nil, so an empty merge
// projects an absent field rather than an empty map.
//
// This is the SINGLE merge function both the reconciler and the validating
// webhook consume: validating one merge while projecting another is exactly the
// drift this helper exists to prevent, so admission and reconciliation always
// agree on the effective config.
func MergedExtraConfig(global, service map[string]map[string]string) map[string]map[string]string {
	// config.MergeDefaults(userConfig, defaults) merges key by key with
	// userConfig winning, into a fresh map without mutating either input, so the
	// per-service block is the userConfig and the global block is the defaults.
	// It never returns nil, hence the length-zero normalization below.
	merged := config.MergeDefaults(service, global)
	if len(merged) == 0 {
		return nil
	}
	return merged
}

// DerivedPublicEndpoint returns the public URL the ControlPlane will derive for
// the dashboard. It is shared by the identity-backend projection and the
// admission-time trusted-dashboard check so both derive the gate identically.
//
// An explicit publicEndpoint wins (with any trailing slash trimmed); otherwise,
// when a gateway is set, it is derived as "https://{gateway.hostname}" (the
// default-443 form). A nil receiver (services.horizon unset) and the
// neither-set case both yield "".
func (s *ServiceHorizonSpec) DerivedPublicEndpoint() string {
	if s == nil {
		return ""
	}
	if s.PublicEndpoint != "" {
		return strings.TrimRight(s.PublicEndpoint, "/")
	}
	if s.Gateway != nil && s.Gateway.Hostname != "" {
		return "https://" + s.Gateway.Hostname
	}
	return ""
}
