// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"embed"

	"github.com/c5c3/forge/internal/common/config"
)

// catalogFS holds the generated per-release keystone option catalogs embedded
// into the operator binary. Each file is named "<YYYY.N>.json" and decodes into
// a config.OptionCatalog, so the validating webhook can reject spec.extraConfig
// option names the pinned release does not accept without reaching a live
// keystone.
//
//go:embed catalogs/*.json
var catalogFS embed.FS

// optionCatalogs maps a YYYY.N release base (the embedded file's stem) to the
// parsed keystone option catalog for that release. It is built once at package
// initialization.
var optionCatalogs = config.MustParseEmbeddedCatalogs(catalogFS)

// OptionCatalogForRelease returns the embedded keystone option catalog for the
// OpenStack release named by the given image tag. See config.LookupCatalog for
// the tag-to-catalog resolution rules.
func OptionCatalogForRelease(tag string) (*config.OptionCatalog, bool) {
	return config.LookupCatalog(optionCatalogs, tag)
}
