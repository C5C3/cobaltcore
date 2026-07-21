// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"testing"

	"github.com/onsi/gomega"
)

// TestGlanceOptionCatalogs_EmbeddedReleasesParse pins the exact set of embedded
// catalogs and proves each parsed cleanly at package init
// (config.MustParseEmbeddedCatalogs would have panicked otherwise, so merely
// reading optionCatalogs exercises the happy path and keeps the panic branch
// unreachable in production).
func TestGlanceOptionCatalogs_EmbeddedReleasesParse(t *testing.T) {
	g := gomega.NewWithT(t)

	g.Expect(optionCatalogs).To(gomega.HaveLen(2))
	g.Expect(optionCatalogs).To(gomega.HaveKey("2025.2"))
	g.Expect(optionCatalogs).To(gomega.HaveKey("2026.1"))

	for rel, catalog := range optionCatalogs {
		g.Expect(catalog.Sections).To(gomega.HaveKey("DEFAULT"), "release %s must have a DEFAULT section", rel)
		g.Expect(catalog.Sections["DEFAULT"].Opts).To(gomega.ContainElement("debug"),
			"release %s DEFAULT must list debug", rel)
	}
}

// TestGlanceOptionCatalogs_UseUnderscoreSpelling guards the invariant
// FindUnknownOptions relies on: catalog option names, deprecated keys, and
// deprecated replacements all use the underscore spelling oslo.config reads from
// a file, never the CLI dash form. A dash-spelled entry would silently make a
// valid underscore override look unknown (or vice versa).
func TestGlanceOptionCatalogs_UseUnderscoreSpelling(t *testing.T) {
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

// TestGlanceOptionCatalogForRelease covers the release-to-catalog resolution: a
// base release and its patch suffix both resolve to the same catalog, while an
// empty release, a digest-style "latest", and a parseable-but-unshipped release
// all miss.
func TestGlanceOptionCatalogForRelease(t *testing.T) {
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

// TestGlanceOptionCatalog2025_DeprecatesLogfile pins one representative
// deprecation the webhook's deprecated-option warning depends on.
func TestGlanceOptionCatalog2025_DeprecatesLogfile(t *testing.T) {
	g := gomega.NewWithT(t)

	catalog, ok := OptionCatalogForRelease("2025.2")
	g.Expect(ok).To(gomega.BeTrue())
	g.Expect(catalog.Sections["DEFAULT"].Deprecated).To(gomega.HaveKeyWithValue("logfile", "[DEFAULT] log_file"))
}

// TestGlanceCatalogExemptSections pins the reserved glance_store sections the
// generated catalog cannot enumerate. Their non-registry keys stay settable, so
// validateExtraConfigOptions must skip them whole alongside the plugin sections.
func TestGlanceCatalogExemptSections(t *testing.T) {
	g := gomega.NewWithT(t)

	g.Expect(CatalogExemptSections).To(gomega.ConsistOf("os_glance_staging_store", "os_glance_tasks_store"))
}
