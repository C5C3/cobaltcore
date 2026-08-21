// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"testing"

	"github.com/onsi/gomega"

	"github.com/c5c3/cobaltcore/internal/common/config"
)

// TestPlacementOptionCatalogs_EmbeddedReleasesParse pins the exact set of
// embedded catalogs and proves each parsed cleanly at package init
// (config.MustParseEmbeddedCatalogs would have panicked otherwise, so merely
// reading optionCatalogs exercises the happy path and keeps the panic branch
// unreachable in production).
func TestPlacementOptionCatalogs_EmbeddedReleasesParse(t *testing.T) {
	g := gomega.NewWithT(t)

	g.Expect(optionCatalogs).To(gomega.HaveLen(2))
	g.Expect(optionCatalogs).To(gomega.HaveKey("2025.2"))
	g.Expect(optionCatalogs).To(gomega.HaveKey("2026.1"))

	for rel, catalog := range optionCatalogs {
		g.Expect(catalog.Service).To(gomega.Equal("placement"), "release %s must be a placement catalog", rel)
		g.Expect(catalog.Sections).To(gomega.HaveKey("DEFAULT"), "release %s must have a DEFAULT section", rel)
		g.Expect(catalog.Sections["DEFAULT"].Opts).To(gomega.ContainElement("debug"),
			"release %s DEFAULT must list debug", rel)
	}
}

// TestPlacementOptionCatalogs_UseUnderscoreSpelling guards the invariant
// FindUnknownOptions relies on: catalog option names, deprecated keys, and
// deprecated replacements all use the underscore spelling oslo.config reads from
// a file, never the CLI dash form. A dash-spelled entry would silently make a
// valid underscore override look unknown (or vice versa).
func TestPlacementOptionCatalogs_UseUnderscoreSpelling(t *testing.T) {
	g := gomega.NewWithT(t)

	for rel, catalog := range optionCatalogs {
		g.Expect(catalog.Sections["DEFAULT"].Opts).To(gomega.ContainElement("log_config_append"),
			"release %s DEFAULT must list log_config_append", rel)
		for section, body := range catalog.Sections {
			for _, opt := range body.Opts {
				g.Expect(opt).NotTo(gomega.ContainSubstring("-"),
					"release %s [%s] opt %q must use underscore spelling", rel, section, opt)
			}
			for key, replacement := range body.Deprecated {
				g.Expect(key).NotTo(gomega.ContainSubstring("-"),
					"release %s [%s] deprecated key %q must use underscore spelling", rel, section, key)
				g.Expect(replacement).NotTo(gomega.ContainSubstring("-"),
					"release %s [%s] deprecated replacement %q must use underscore spelling", rel, section, replacement)
			}
		}
	}
}

// TestPlacementOptionCatalogForRelease covers the release-to-catalog resolution:
// a base release and its patch suffix both resolve to the same catalog, while an
// empty release, a digest-style "latest", and a parseable-but-unshipped release
// all miss.
func TestPlacementOptionCatalogForRelease(t *testing.T) {
	g := gomega.NewWithT(t)

	cat2025, ok := OptionCatalogForRelease("2025.2")
	g.Expect(ok).To(gomega.BeTrue())
	g.Expect(cat2025).NotTo(gomega.BeNil())
	g.Expect(cat2025.Release).To(gomega.Equal("2025.2"))

	// A patch suffix strips to the same base-release catalog (same pointer).
	catPatch, ok := OptionCatalogForRelease("2025.2-p1")
	g.Expect(ok).To(gomega.BeTrue())
	g.Expect(catPatch).To(gomega.BeIdenticalTo(cat2025))

	// Values that do not resolve to an embedded catalog. "2024.2" parses as a
	// release but no catalog is embedded for it.
	for _, rel := range []string{"", "latest", "2024.2"} {
		catalog, ok := OptionCatalogForRelease(rel)
		g.Expect(ok).To(gomega.BeFalse(), "release %q must not resolve to a catalog", rel)
		g.Expect(catalog).To(gomega.BeNil(), "release %q must return a nil catalog", rel)
	}
}

// TestPlacementOptionCatalogs_DatabaseSectionIsPlacementDatabase pins the
// section split placement is unusual for: it reads its connection URL from
// [placement_database], and plain [database] is not a section it registers at
// all. Both releases must agree, since an override addressed to [database]
// silently does nothing at runtime and the catalog is what turns that into an
// admission error.
func TestPlacementOptionCatalogs_DatabaseSectionIsPlacementDatabase(t *testing.T) {
	g := gomega.NewWithT(t)

	for rel, catalog := range optionCatalogs {
		g.Expect(catalog.Sections).To(gomega.HaveKey("placement_database"),
			"release %s must have a placement_database section", rel)
		g.Expect(catalog.Sections["placement_database"].Opts).To(gomega.ContainElement("connection"),
			"release %s placement_database must list connection", rel)
		g.Expect(catalog.Sections).NotTo(gomega.HaveKey("database"),
			"release %s must not register a plain database section", rel)
	}
}

// TestPlacementOptionCatalog2025_Deprecations pins two representative
// deprecations the webhook's deprecated-option warning depends on: a rename
// within DEFAULT and a rename that moves the option to another section.
func TestPlacementOptionCatalog2025_Deprecations(t *testing.T) {
	g := gomega.NewWithT(t)

	catalog, ok := OptionCatalogForRelease("2025.2")
	g.Expect(ok).To(gomega.BeTrue())
	g.Expect(catalog.Sections["DEFAULT"].Deprecated).To(gomega.HaveKeyWithValue("logfile", "[DEFAULT] log_file"))
	g.Expect(catalog.Sections["DEFAULT"].Deprecated).To(gomega.HaveKeyWithValue("auth_strategy", "[api] auth_strategy"))
}

// TestPlacementOptionCatalog_NoExemptionsNeeded is the evidence behind placement
// declaring no catalog-exempt sections: scanned with an empty exemption set, the
// catalog accepts every section placement actually registers and still reports
// an unknown key and an unknown section. If placement ever gained a section
// whose options are registered at runtime, this test would start failing on that
// section instead of silently passing it through.
func TestPlacementOptionCatalog_NoExemptionsNeeded(t *testing.T) {
	g := gomega.NewWithT(t)

	catalog, ok := OptionCatalogForRelease("2026.1")
	g.Expect(ok).To(gomega.BeTrue())

	extraConfig := map[string]map[string]string{
		"DEFAULT":            {"debug": "true"},
		"api":                {"auth_strategy": "keystone"},
		"placement":          {"randomize_allocation_candidates": "true"},
		"placement_database": {"max_pool_size": "10"},
		"oslo_policy":        {"policy_file": "/etc/placement/policy.yaml"},
		// Neither of these is accepted: the key is not an option of a known
		// section, and the section is one placement does not register.
		"cors":     {"not_an_option": "x"},
		"database": {"connection": "mysql+pymysql://"},
	}

	unknown, deprecated := catalog.FindUnknownOptions(extraConfig, config.CatalogExemptions{})

	g.Expect(deprecated).To(gomega.BeEmpty())
	g.Expect(unknown).To(gomega.ConsistOf(
		config.UnknownOption{Section: "cors", Key: "not_an_option"},
		config.UnknownOption{Section: "database", Key: "connection", SectionUnknown: true},
	))
}
