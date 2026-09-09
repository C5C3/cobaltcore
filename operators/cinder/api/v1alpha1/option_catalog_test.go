// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"testing"

	"github.com/onsi/gomega"
)

// TestCinderOptionCatalogs_EmbeddedReleasesParse pins the exact set of embedded
// catalogs and proves each parsed cleanly at package init
// (config.MustParseEmbeddedCatalogs would have panicked otherwise, so merely
// reading optionCatalogs exercises the happy path and keeps the panic branch
// unreachable in production).
func TestCinderOptionCatalogs_EmbeddedReleasesParse(t *testing.T) {
	g := gomega.NewWithT(t)

	g.Expect(optionCatalogs).To(gomega.HaveLen(2))
	g.Expect(optionCatalogs).To(gomega.HaveKey("2025.2"))
	g.Expect(optionCatalogs).To(gomega.HaveKey("2026.1"))

	for rel, catalog := range optionCatalogs {
		g.Expect(catalog.Service).To(gomega.Equal("cinder"), "release %s must name the cinder service", rel)
		g.Expect(catalog.Sections).To(gomega.HaveKey("DEFAULT"), "release %s must have a DEFAULT section", rel)
		g.Expect(catalog.Sections["DEFAULT"].Opts).To(gomega.ContainElement("debug"),
			"release %s DEFAULT must list debug", rel)
	}
}

// TestCinderOptionCatalogs_UseUnderscoreSpelling guards the invariant
// FindUnknownOptions relies on: catalog option names, deprecated keys, and
// deprecated replacements all use the underscore spelling oslo.config reads from
// a file, never the CLI dash form. A dash-spelled entry would silently make a
// valid underscore override look unknown (or vice versa).
func TestCinderOptionCatalogs_UseUnderscoreSpelling(t *testing.T) {
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

// TestCinderOptionCatalogs_CoverTheRenderedSections pins that every cinder.conf
// section the operator renders is one the catalog enumerates, so an override
// aimed at a rendered section is scanned against real option names instead of
// missing the catalog entirely. The privsep contexts ([cinder_sys_admin],
// [privsep_osbrick]) are deliberately absent: oslo.privsep registers a context's
// section at runtime, so the generator never sees them.
func TestCinderOptionCatalogs_CoverTheRenderedSections(t *testing.T) {
	g := gomega.NewWithT(t)

	for rel, catalog := range optionCatalogs {
		for _, section := range []string{
			"database", "keystone_authtoken", "service_user", "key_manager", "barbican",
			"oslo_messaging_rabbit", "oslo_messaging_notifications", "oslo_concurrency",
			"coordination", "oslo_policy",
		} {
			g.Expect(catalog.Sections).To(gomega.HaveKey(section),
				"release %s must carry the [%s] section", rel, section)
		}

		g.Expect(catalog.Sections["database"].Opts).To(gomega.ContainElement("connection"),
			"release %s [database] must list connection", rel)
		g.Expect(catalog.Sections["coordination"].Opts).To(gomega.ContainElement("backend_url"),
			"release %s [coordination] must list backend_url", rel)
		g.Expect(catalog.Sections["DEFAULT"].Opts).To(gomega.ContainElement("glance_api_servers"),
			"release %s DEFAULT must list glance_api_servers", rel)
		g.Expect(catalog.Sections["DEFAULT"].Opts).To(gomega.ContainElement("enabled_backends"),
			"release %s DEFAULT must list enabled_backends", rel)

		for _, section := range []string{"cinder_sys_admin", "privsep_osbrick"} {
			g.Expect(catalog.Sections).NotTo(gomega.HaveKey(section),
				"release %s must not carry the runtime-registered [%s] section", rel, section)
		}
	}
}

// TestCinderOptionCatalogForRelease covers the release-to-catalog resolution: a
// base release and its patch suffix both resolve to the same catalog, while an
// empty release, a digest-style "latest", and a parseable-but-unshipped release
// all miss.
func TestCinderOptionCatalogForRelease(t *testing.T) {
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
