// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"embed"

	"github.com/c5c3/cobaltcore/internal/common/config"
)

// catalogFS holds the generated per-release glance option catalogs embedded into
// the operator binary. Each file is named "<YYYY.N>.json" and decodes into a
// config.OptionCatalog, so the validating webhook can reject spec.extraConfig
// option names the pinned release does not accept without reaching a live glance.
//
//go:embed catalogs/*.json
var catalogFS embed.FS

// optionCatalogs maps a YYYY.N release base (the embedded file's stem) to the
// parsed glance option catalog for that release. It is built once at package
// initialization.
var optionCatalogs = config.MustParseEmbeddedCatalogs(catalogFS)

// CatalogExemptSections names the reserved glance_store sections the operator
// projects but the generated catalog cannot enumerate: the glance-image-service
// staging and tasks stores are per-worker file stores whose options are the
// backend-driver keys glance_store registers at runtime, not options the release
// catalog lists. They are exempt from the unknown-option scan so an override of
// any file-store option under them stays settable; the two operator-owned
// datadir keys in OwnedConfigKeys are a subset of what this exempts.
var CatalogExemptSections = []string{"os_glance_staging_store", "os_glance_tasks_store"}

// OptionCatalogForRelease returns the embedded glance option catalog for the
// OpenStack release named by spec.openStackRelease. See config.LookupCatalog for
// the release-to-catalog resolution rules.
func OptionCatalogForRelease(openStackRelease string) (*config.OptionCatalog, bool) {
	return config.LookupCatalog(optionCatalogs, openStackRelease)
}
