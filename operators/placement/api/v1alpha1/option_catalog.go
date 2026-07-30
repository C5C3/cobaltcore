// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"embed"

	"github.com/c5c3/forge/internal/common/config"
)

// catalogFS holds the generated per-release placement option catalogs embedded
// into the operator binary. Each file is named "<YYYY.N>.json" and decodes into
// a config.OptionCatalog, so the validating webhook can reject spec.extraConfig
// option names the pinned release does not accept without reaching a live
// placement.
//
//go:embed catalogs/*.json
var catalogFS embed.FS

// optionCatalogs maps a YYYY.N release base (the embedded file's stem) to the
// parsed placement option catalog for that release. It is built once at package
// initialization.
var optionCatalogs = config.MustParseEmbeddedCatalogs(catalogFS)

// OptionCatalogForRelease returns the embedded placement option catalog for the
// OpenStack release named by spec.openStackRelease. See config.LookupCatalog for
// the release-to-catalog resolution rules.
//
// Placement declares no catalog-exempt sections. Every section a placement
// deployment configures is statically registered by placement itself or by an
// oslo library, so the generated catalog enumerates all of them; placement has
// neither the dynamically named stores that force glance to exempt sections nor
// a plugin mechanism that would let a CR introduce a section of its own.
func OptionCatalogForRelease(openStackRelease string) (*config.OptionCatalog, bool) {
	return config.LookupCatalog(optionCatalogs, openStackRelease)
}
