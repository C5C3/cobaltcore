// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"embed"

	"github.com/c5c3/cobaltcore/internal/common/config"
)

// catalogFS holds the generated per-release nova option catalogs embedded into
// the operator binary. Each file is named "<YYYY.N>.json" and decodes into a
// config.OptionCatalog, so the validating webhook can reject spec.extraConfig
// option names the pinned release does not accept without reaching a live nova.
//
// A catalog covers the namespaces nova's own generator config registers, which
// includes the castellan, keystonemiddleware, os_brick, osprofiler and oslo.*
// options the service loads into the same oslo CONF as its own.
//
//go:embed catalogs/*.json
var catalogFS embed.FS

// optionCatalogs maps a YYYY.N release base (the embedded file's stem) to the
// parsed nova option catalog for that release. It is built once at package
// initialization.
var optionCatalogs = config.MustParseEmbeddedCatalogs(catalogFS)

// RenderedSections lists the nova.conf sections the operator renders. Every one
// of them must be a section the embedded catalogs enumerate, so an override
// aimed at a rendered section is scanned against real option names instead of
// missing the catalog and being reported as an unknown section. The catalog test
// in this package checks the sections against both catalogs; the controller
// package checks the rendered file against this same list.
var RenderedSections = []string{
	"DEFAULT",
	"api",
	"api_database",
	"database",
	"keystone_authtoken",
	"service_user",
	"placement",
	"neutron",
	"glance",
	"cinder",
	"key_manager",
	"barbican",
	"oslo_messaging_rabbit",
	"oslo_messaging_notifications",
	"oslo_concurrency",
	"upgrade_levels",
	"cache",
	"scheduler",
	"conductor",
	"vnc",
}

// OptionCatalogForRelease returns the embedded nova option catalog for the
// OpenStack release named by spec.openStackRelease. See config.LookupCatalog for
// the release-to-catalog resolution rules.
func OptionCatalogForRelease(openStackRelease string) (*config.OptionCatalog, bool) {
	return config.LookupCatalog(optionCatalogs, openStackRelease)
}
