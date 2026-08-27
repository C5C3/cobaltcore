// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"testing"

	"github.com/onsi/gomega"
)

// TestNeutronOptionCatalogs_EmbeddedReleasesParse pins the exact set of embedded
// catalogs and proves each parsed cleanly at package init
// (config.MustParseEmbeddedCatalogs would have panicked otherwise, so merely
// reading optionCatalogs exercises the happy path and keeps the panic branch
// unreachable in production).
func TestNeutronOptionCatalogs_EmbeddedReleasesParse(t *testing.T) {
	g := gomega.NewWithT(t)

	g.Expect(optionCatalogs).To(gomega.HaveLen(2))
	g.Expect(optionCatalogs).To(gomega.HaveKey("2025.2"))
	g.Expect(optionCatalogs).To(gomega.HaveKey("2026.1"))

	for rel, catalog := range optionCatalogs {
		g.Expect(catalog.Service).To(gomega.Equal("neutron"), "release %s must name the neutron service", rel)
		g.Expect(catalog.Sections).To(gomega.HaveKey("DEFAULT"), "release %s must have a DEFAULT section", rel)
		g.Expect(catalog.Sections["DEFAULT"].Opts).To(gomega.ContainElement("debug"),
			"release %s DEFAULT must list debug", rel)
	}
}

// TestNeutronOptionCatalogs_UseUnderscoreSpelling guards the invariant
// FindUnknownOptions relies on: catalog option names, deprecated keys, and
// deprecated replacements all use the underscore spelling oslo.config reads from
// a file, never the CLI dash form. A dash-spelled entry would silently make a
// valid underscore override look unknown (or vice versa).
func TestNeutronOptionCatalogs_UseUnderscoreSpelling(t *testing.T) {
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

// TestNeutronOptionCatalogs_AreTheFlatProcessUnion pins the union property the
// generator hands over: one catalog per release covers both the neutron server
// (neutron.conf plus ml2_conf.ini) and the metadata agent
// (neutron_ovn_metadata_agent.ini), without a field saying which file a section
// came from. Splitting the catalogs per process later would break this.
func TestNeutronOptionCatalogs_AreTheFlatProcessUnion(t *testing.T) {
	g := gomega.NewWithT(t)

	for rel, catalog := range optionCatalogs {
		for _, section := range []string{"ml2", "ml2_type_vlan", "ovn", "quotas", "ovs", "agent"} {
			g.Expect(catalog.Sections).To(gomega.HaveKey(section),
				"release %s must carry the [%s] section", rel, section)
		}

		g.Expect(catalog.Sections["ml2_type_vlan"].Opts).To(gomega.ContainElement("network_vlan_ranges"),
			"release %s [ml2_type_vlan] must list network_vlan_ranges", rel)
		g.Expect(catalog.Sections["quotas"].Opts).To(gomega.ContainElement("quota_network"),
			"release %s [quotas] must list quota_network", rel)
		g.Expect(catalog.Sections["ovs"].Opts).To(gomega.ContainElement("ovsdb_connection"),
			"release %s [ovs] must list ovsdb_connection", rel)
		g.Expect(catalog.Sections["DEFAULT"].Opts).To(gomega.ContainElement("metadata_proxy_shared_secret"),
			"release %s DEFAULT must list metadata_proxy_shared_secret", rel)
	}
}

// TestNeutronOptionCatalogForRelease covers the release-to-catalog resolution: a
// base release and its patch suffix both resolve to the same catalog, while an
// empty release, a digest-style "latest", and a parseable-but-unshipped release
// all miss.
func TestNeutronOptionCatalogForRelease(t *testing.T) {
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
