// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"embed"

	"github.com/c5c3/cobaltcore/internal/common/config"
)

// catalogFS holds the generated per-release cinder option catalogs embedded into
// the operator binary. Each file is named "<YYYY.N>.json" and decodes into a
// config.OptionCatalog, so the validating webhook can reject spec.extraConfig
// option names the pinned release does not accept without reaching a live cinder.
//
// A catalog covers the namespaces cinder's own generator config registers, which
// includes the castellan, keystonemiddleware, os_brick, osprofiler and oslo.*
// options the service loads into the same oslo CONF as its own.
//
//go:embed catalogs/*.json
var catalogFS embed.FS

// optionCatalogs maps a YYYY.N release base (the embedded file's stem) to the
// parsed cinder option catalog for that release. It is built once at package
// initialization.
var optionCatalogs = config.MustParseEmbeddedCatalogs(catalogFS)

// OptionCatalogForRelease returns the embedded cinder option catalog for the
// OpenStack release named by spec.openStackRelease. See config.LookupCatalog for
// the release-to-catalog resolution rules.
func OptionCatalogForRelease(openStackRelease string) (*config.OptionCatalog, bool) {
	return config.LookupCatalog(optionCatalogs, openStackRelease)
}
