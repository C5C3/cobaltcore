// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"testing"

	. "github.com/onsi/gomega"
)

// TestOptionCatalogs_EmbeddedReleasesParse pins the exact set of embedded
// catalogs and proves each parsed cleanly at package init
// (config.MustParseEmbeddedCatalogs would have panicked otherwise, so merely
// reading optionCatalogs exercises the happy path and keeps the panic branch
// unreachable in production).
func TestOptionCatalogs_EmbeddedReleasesParse(t *testing.T) {
	g := NewGomegaWithT(t)

	g.Expect(optionCatalogs).To(HaveLen(2))
	g.Expect(optionCatalogs).To(HaveKey("2025.2"))
	g.Expect(optionCatalogs).To(HaveKey("2026.1"))

	for rel, catalog := range optionCatalogs {
		g.Expect(catalog.Sections).To(HaveKey("DEFAULT"), "release %s must have a DEFAULT section", rel)
		g.Expect(catalog.Sections["DEFAULT"].Opts).To(ContainElement("debug"),
			"release %s DEFAULT must list debug", rel)
	}
}

// TestOptionCatalogs_UseUnderscoreSpelling guards the invariant FindUnknownOptions
// relies on: catalog option names, deprecated keys, and deprecated replacements
// all use the underscore spelling oslo.config reads from a file, never the CLI
// dash form. A dash-spelled entry would silently make a valid underscore
// override look unknown (or vice versa).
func TestOptionCatalogs_UseUnderscoreSpelling(t *testing.T) {
	g := NewGomegaWithT(t)

	for rel, catalog := range optionCatalogs {
		g.Expect(catalog.Sections["DEFAULT"].Opts).To(ContainElement("log_config_append"),
			"release %s DEFAULT must list log_config_append", rel)
		for section, body := range catalog.Sections {
			for _, opt := range body.Opts {
				g.Expect(opt).NotTo(ContainSubstring("-"),
					"release %s [%s] opt %q must use underscore spelling", rel, section, opt)
			}
			for key, replacement := range body.Deprecated {
				g.Expect(key).NotTo(ContainSubstring("-"),
					"release %s [%s] deprecated key %q must use underscore spelling", rel, section, key)
				g.Expect(replacement).NotTo(ContainSubstring("-"),
					"release %s [%s] deprecated replacement %q must use underscore spelling", rel, section, replacement)
			}
		}
	}
}

// TestOptionCatalogForRelease covers the release-to-catalog resolution: a base
// release and its patch suffix both resolve to the same catalog, while an empty
// tag, a digest-style "latest", and a parseable-but-unshipped release all miss.
func TestOptionCatalogForRelease(t *testing.T) {
	g := NewGomegaWithT(t)

	cat2025, ok := OptionCatalogForRelease("2025.2")
	g.Expect(ok).To(BeTrue())
	g.Expect(cat2025).NotTo(BeNil())
	g.Expect(cat2025.Release).To(Equal("2025.2"))

	// A patch suffix strips to the same base-release catalog (same pointer).
	catPatch, ok := OptionCatalogForRelease("2025.2-p1")
	g.Expect(ok).To(BeTrue())
	g.Expect(catPatch).To(BeIdenticalTo(cat2025))

	// Tags that do not resolve to an embedded catalog. "2024.2" parses as a
	// release but no catalog is embedded for it.
	for _, tag := range []string{"", "latest", "2024.2"} {
		catalog, ok := OptionCatalogForRelease(tag)
		g.Expect(ok).To(BeFalse(), "tag %q must not resolve to a catalog", tag)
		g.Expect(catalog).To(BeNil(), "tag %q must return a nil catalog", tag)
	}
}

// TestOptionCatalog2025_DeprecatesLogfile pins one representative deprecation the
// webhook's deprecated-option warning depends on.
func TestOptionCatalog2025_DeprecatesLogfile(t *testing.T) {
	g := NewGomegaWithT(t)

	catalog, ok := OptionCatalogForRelease("2025.2")
	g.Expect(ok).To(BeTrue())
	g.Expect(catalog.Sections["DEFAULT"].Deprecated).To(HaveKeyWithValue("logfile", "[DEFAULT] log_file"))
}
