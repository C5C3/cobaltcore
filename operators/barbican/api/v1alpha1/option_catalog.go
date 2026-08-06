// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"embed"

	"github.com/c5c3/forge/internal/common/config"
)

// catalogFS holds the generated per-release barbican option catalogs embedded
// into the operator binary. Each file is named "<YYYY.N>.json" and decodes into
// a config.OptionCatalog, so the validating webhook can reject spec.extraConfig
// option names the pinned release does not accept without reaching a live
// barbican.
//
//go:embed catalogs/*.json
var catalogFS embed.FS

// optionCatalogs maps a YYYY.N release base (the embedded file's stem) to the
// parsed barbican option catalog for that release. It is built once at package
// initialization.
var optionCatalogs = config.MustParseEmbeddedCatalogs(catalogFS)

// CatalogExemptSections names the statically spelled sections the unknown-option
// scan skips whole. It is empty, and that emptiness is a finding rather than a
// placeholder: every fixed-name section the operator writes ([DEFAULT],
// [database], [keystone_authtoken], [secretstore], [vault_plugin], [queue] and
// [oslo_policy]) is registered by barbican itself or by an oslo library, so both
// generated catalogs enumerate all of them. The only sections no catalog can
// enumerate are the per-store ones CatalogExemptSectionPrefixes covers. The
// variable exists so a section that later escapes the catalog has a declared
// place to go and so the ControlPlane guard reads the same pair of exemption
// lists from every service package.
var CatalogExemptSections = []string{}

// CatalogExemptSectionPrefixes names the section-name prefixes whose sections the
// unknown-option scan skips whole. The per-store [secretstore:<name>] sections
// are named after the BarbicanSecretStore CRs attached to a Barbican, so no
// release catalog can list them and no static exemption can spell them. The
// shared config.CatalogExemptions.Sections set matches exact section names only,
// so the validating webhook expands each prefix here against the section names
// the merged configuration actually carries and feeds the resulting exact names
// into the scan.
var CatalogExemptSectionPrefixes = []string{"secretstore:"}

// OptionCatalogForRelease returns the embedded barbican option catalog for the
// OpenStack release named by spec.openStackRelease. See config.LookupCatalog for
// the release-to-catalog resolution rules.
func OptionCatalogForRelease(openStackRelease string) (*config.OptionCatalog, bool) {
	return config.LookupCatalog(optionCatalogs, openStackRelease)
}
