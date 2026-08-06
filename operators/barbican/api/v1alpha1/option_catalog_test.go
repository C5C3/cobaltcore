// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"strings"
	"testing"

	"github.com/onsi/gomega"

	"github.com/c5c3/forge/internal/common/config"
)

// TestBarbicanOptionCatalogs_EmbeddedReleasesParse pins the exact set of
// embedded catalogs and proves each parsed cleanly at package init
// (config.MustParseEmbeddedCatalogs would have panicked otherwise, so merely
// reading optionCatalogs exercises the happy path and keeps the panic branch
// unreachable in production).
func TestBarbicanOptionCatalogs_EmbeddedReleasesParse(t *testing.T) {
	g := gomega.NewWithT(t)

	g.Expect(optionCatalogs).To(gomega.HaveLen(2))
	g.Expect(optionCatalogs).To(gomega.HaveKey("2025.2"))
	g.Expect(optionCatalogs).To(gomega.HaveKey("2026.1"))

	for rel, catalog := range optionCatalogs {
		g.Expect(catalog.Service).To(gomega.Equal("barbican"), "release %s must be a barbican catalog", rel)
		g.Expect(catalog.Sections).To(gomega.HaveKey("DEFAULT"), "release %s must have a DEFAULT section", rel)
		g.Expect(catalog.Sections["DEFAULT"].Opts).To(gomega.ContainElement("debug"),
			"release %s DEFAULT must list debug", rel)
	}
}

// TestBarbicanOptionCatalogs_UseUnderscoreSpelling guards the invariant
// FindUnknownOptions relies on: catalog option names, deprecated keys, and
// deprecated replacements all use the underscore spelling oslo.config reads from
// a file, never the CLI dash form. A dash-spelled entry would silently make a
// valid underscore override look unknown (or vice versa).
func TestBarbicanOptionCatalogs_UseUnderscoreSpelling(t *testing.T) {
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

// TestBarbicanOptionCatalogForRelease covers the release-to-catalog resolution:
// a base release and its patch suffix both resolve to the same catalog, while an
// empty release, a digest-style "latest", and a parseable-but-unshipped release
// all miss.
func TestBarbicanOptionCatalogForRelease(t *testing.T) {
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

// TestBarbicanOptionCatalogs_OwnedSectionsAreEnumerated is the evidence behind
// CatalogExemptSections being empty: every fixed section OwnedConfigKeys claims
// is one both catalogs enumerate, so none of them needs a static exemption. The
// per-store [secretstore:<name>] form is checked to be absent, since that is the
// one shape only CatalogExemptSectionPrefixes can cover.
func TestBarbicanOptionCatalogs_OwnedSectionsAreEnumerated(t *testing.T) {
	g := gomega.NewWithT(t)

	g.Expect(CatalogExemptSections).To(gomega.BeEmpty())
	g.Expect(CatalogExemptSectionPrefixes).To(gomega.ConsistOf("secretstore:"))

	for rel, catalog := range optionCatalogs {
		for _, owned := range OwnedConfigKeys {
			g.Expect(catalog.Sections).To(gomega.HaveKey(owned.Section),
				"release %s must enumerate the operator-owned section [%s]", rel, owned.Section)
		}
		g.Expect(catalog.Sections["secretstore"].Opts).To(gomega.ContainElement("stores_lookup_suffix"),
			"release %s secretstore must list stores_lookup_suffix", rel)
		g.Expect(catalog.Sections["vault_plugin"].Opts).To(gomega.ContainElement("approle_role_id"),
			"release %s vault_plugin must list approle_role_id", rel)
		for section := range catalog.Sections {
			g.Expect(section).NotTo(gomega.HavePrefix("secretstore:"),
				"release %s must not enumerate a per-store section (%s)", rel, section)
		}
	}
}

// TestBarbicanOptionCatalog2025_Deprecations pins two representative
// deprecations the webhook's deprecated-option warning depends on: a rename
// within DEFAULT and a rename keystonemiddleware carries in its own section.
func TestBarbicanOptionCatalog2025_Deprecations(t *testing.T) {
	g := gomega.NewWithT(t)

	catalog, ok := OptionCatalogForRelease("2025.2")
	g.Expect(ok).To(gomega.BeTrue())
	g.Expect(catalog.Sections["DEFAULT"].Deprecated).To(gomega.HaveKeyWithValue("logfile", "[DEFAULT] log_file"))
	g.Expect(catalog.Sections["keystone_authtoken"].Deprecated).To(
		gomega.HaveKeyWithValue("memcache_servers", "[keystone_authtoken] memcached_servers"),
	)
}

// TestBarbicanOptionCatalog_ExemptionsCoverTheDynamicStores exercises the scan
// the validating webhook runs: the ownership registry carries the operator-owned
// keys, and the CatalogExemptSectionPrefixes entry is expanded against the
// sections the merged configuration actually carries. A per-store section passes,
// while an unknown key in a known section and a section the shipped image cannot
// load are both still reported.
func TestBarbicanOptionCatalog_ExemptionsCoverTheDynamicStores(t *testing.T) {
	g := gomega.NewWithT(t)

	catalog, ok := OptionCatalogForRelease("2026.1")
	g.Expect(ok).To(gomega.BeTrue())

	extraConfig := map[string]map[string]string{
		// db_auto_create is operator-owned, so the registry exempts it; logfile is
		// a live deprecated alias and must surface as such.
		"DEFAULT": {"db_auto_create": "false", "logfile": "/var/log/barbican.log"},
		// An operator-owned key and a plain catalog option in the same section.
		"secretstore":  {"stores_lookup_suffix": "primary", "enabled_secretstore_plugins": "store_crypto"},
		"vault_plugin": {"approle_secret_id": "s.redacted"},
		// A per-store section: dynamically named, so only the prefix exemption can
		// reach it.
		"secretstore:primary": {"global_default": "True"},
		// Neither of these is accepted: the key is not an option of a known
		// section, and [kmip_plugin] is absent from the catalog because the shipped
		// image ships no pykmip, so the generator never registered that namespace.
		"queue":       {"not_an_option": "x"},
		"kmip_plugin": {"host": "kmip.example.com"},
	}

	unknown, deprecated := catalog.FindUnknownOptions(extraConfig, webhookExemptions(extraConfig))

	g.Expect(deprecated).To(gomega.ConsistOf(
		config.DeprecatedOption{Section: "DEFAULT", Key: "logfile", Replacement: "[DEFAULT] log_file"},
	))
	g.Expect(unknown).To(gomega.ConsistOf(
		config.UnknownOption{Section: "kmip_plugin", Key: "host", SectionUnknown: true},
		config.UnknownOption{Section: "queue", Key: "not_an_option"},
	))
}

// webhookExemptions builds the exemption set the validating webhook feeds into
// the unknown-option scan: the static sections, every section of extraConfig
// matching a CatalogExemptSectionPrefixes entry, and the operator-owned keys.
func webhookExemptions(extraConfig map[string]map[string]string) config.CatalogExemptions {
	sections := make(map[string]struct{}, len(CatalogExemptSections))
	for _, section := range CatalogExemptSections {
		sections[section] = struct{}{}
	}
	for section := range extraConfig {
		for _, prefix := range CatalogExemptSectionPrefixes {
			if strings.HasPrefix(section, prefix) {
				sections[section] = struct{}{}
			}
		}
	}
	return config.CatalogExemptions{
		Sections: sections,
		Keys:     config.KeyExemptionsFromRegistry(OwnedConfigKeys),
	}
}
