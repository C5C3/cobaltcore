// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"embed"

	"github.com/c5c3/cobaltcore/internal/common/config"
)

// catalogFS holds the generated per-release neutron option catalogs embedded into
// the operator binary. Each file is named "<YYYY.N>.json" and decodes into a
// config.OptionCatalog, so the validating webhook can reject spec.extraConfig
// option names the pinned release does not accept without reaching a live neutron.
//
// A catalog is the flat union of the neutron.conf, ml2_conf.ini and
// neutron_ovn_metadata_agent.ini generator files and carries no per-file
// provenance. An option placed in the section of the other process therefore
// passes validation, and oslo.config ignores it at runtime.
//
//go:embed catalogs/*.json
var catalogFS embed.FS

// optionCatalogs maps a YYYY.N release base (the embedded file's stem) to the
// parsed neutron option catalog for that release. It is built once at package
// initialization.
var optionCatalogs = config.MustParseEmbeddedCatalogs(catalogFS)

// OptionCatalogForRelease returns the embedded neutron option catalog for the
// OpenStack release named by spec.openStackRelease. See config.LookupCatalog for
// the release-to-catalog resolution rules.
func OptionCatalogForRelease(openStackRelease string) (*config.OptionCatalog, bool) {
	return config.LookupCatalog(optionCatalogs, openStackRelease)
}
