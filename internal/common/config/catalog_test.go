// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"testing"

	. "github.com/onsi/gomega"
)

func TestParseOptionCatalog_Valid(t *testing.T) {
	g := NewGomegaWithT(t)

	data := []byte(`{
		"service": "keystone",
		"release": "2026.1",
		"sections": {
			"DEFAULT": {
				"opts": ["debug", "log_dir"],
				"deprecated": {"verbose": "debug"}
			},
			"token": {
				"opts": ["provider", "expiration"]
			}
		}
	}`)

	catalog, err := ParseOptionCatalog(data)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(catalog.Service).To(Equal("keystone"))
	g.Expect(catalog.Release).To(Equal("2026.1"))
	g.Expect(catalog.Sections).To(HaveKey("DEFAULT"))
	g.Expect(catalog.Sections["DEFAULT"].Opts).To(ConsistOf("debug", "log_dir"))
	g.Expect(catalog.Sections["DEFAULT"].Deprecated).To(Equal(map[string]string{"verbose": "debug"}))
	g.Expect(catalog.Sections["token"].Opts).To(ConsistOf("provider", "expiration"))
}

func TestParseOptionCatalog_MalformedJSON(t *testing.T) {
	g := NewGomegaWithT(t)

	_, err := ParseOptionCatalog([]byte(`{"service": "keystone",`))
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("parsing option catalog:"))
}

func TestParseOptionCatalog_NoSections(t *testing.T) {
	g := NewGomegaWithT(t)

	// The sections member is absent entirely.
	_, err := ParseOptionCatalog([]byte(`{"service": "keystone", "release": "2026.1"}`))
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(Equal("option catalog has no sections"))

	// The sections member is present but empty.
	_, err = ParseOptionCatalog([]byte(`{"service": "keystone", "sections": {}}`))
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(Equal("option catalog has no sections"))
}

// testCatalog is the reference catalog reused across FindUnknownOptions cases:
// a known "DEFAULT" section with an opt and a deprecated option, plus a known
// "token" section. It deliberately has no "memcache" section so the plugin/
// dynamic-section exemption paths can be exercised.
func testCatalog() *OptionCatalog {
	return &OptionCatalog{
		Service: "keystone",
		Release: "2026.1",
		Sections: map[string]CatalogSection{
			"DEFAULT": {
				Opts:       []string{"debug", "log_dir"},
				Deprecated: map[string]string{"verbose": "debug"},
			},
			"token": {
				Opts: []string{"provider", "expiration"},
			},
		},
	}
}

func TestFindUnknownOptions_EmptyInputs(t *testing.T) {
	catalog := testCatalog()

	tests := []struct {
		name        string
		extraConfig map[string]map[string]string
	}{
		{name: "nil extraConfig", extraConfig: nil},
		{name: "empty non-nil map", extraConfig: map[string]map[string]string{}},
		{
			name:        "section with empty key map",
			extraConfig: map[string]map[string]string{"DEFAULT": {}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			unknown, deprecated := catalog.FindUnknownOptions(tt.extraConfig, CatalogExemptions{})
			g.Expect(unknown).To(BeNil())
			g.Expect(deprecated).To(BeNil())
		})
	}
}

func TestFindUnknownOptions_UnknownKeyInKnownSection(t *testing.T) {
	g := NewGomegaWithT(t)
	catalog := testCatalog()

	// "log_level" is not listed by the known "DEFAULT" section.
	unknown, deprecated := catalog.FindUnknownOptions(map[string]map[string]string{
		"DEFAULT": {"log_level": "INFO"},
	}, CatalogExemptions{})

	g.Expect(deprecated).To(BeNil())
	g.Expect(unknown).To(Equal([]UnknownOption{
		{Section: "DEFAULT", Key: "log_level", SectionUnknown: false},
	}))
}

func TestFindUnknownOptions_UnknownSection(t *testing.T) {
	g := NewGomegaWithT(t)
	catalog := testCatalog()

	// The catalog has no "cache" section, so every key is section-unknown.
	unknown, deprecated := catalog.FindUnknownOptions(map[string]map[string]string{
		"cache": {"enabled": "true", "backend": "dogpile"},
	}, CatalogExemptions{})

	g.Expect(deprecated).To(BeNil())
	g.Expect(unknown).To(Equal([]UnknownOption{
		{Section: "cache", Key: "backend", SectionUnknown: true},
		{Section: "cache", Key: "enabled", SectionUnknown: true},
	}))
}

func TestFindUnknownOptions_SortedBySectionThenKey(t *testing.T) {
	g := NewGomegaWithT(t)
	catalog := testCatalog()

	// Sections and keys are chosen so map-iteration order would not already be
	// sorted: "zeta" before "alpha", "yankee" before "alpha" within each.
	unknown, deprecated := catalog.FindUnknownOptions(map[string]map[string]string{
		"zeta":  {"yankee": "1", "alpha": "2"},
		"alpha": {"yankee": "3", "alpha": "4"},
	}, CatalogExemptions{})

	g.Expect(deprecated).To(BeNil())
	g.Expect(unknown).To(Equal([]UnknownOption{
		{Section: "alpha", Key: "alpha", SectionUnknown: true},
		{Section: "alpha", Key: "yankee", SectionUnknown: true},
		{Section: "zeta", Key: "alpha", SectionUnknown: true},
		{Section: "zeta", Key: "yankee", SectionUnknown: true},
	}))
}

func TestFindUnknownOptions_KeyExemptionPrecedence(t *testing.T) {
	g := NewGomegaWithT(t)
	catalog := testCatalog()

	// The [memcache] shape: the ownership registry owns "servers" in a section
	// the catalog does not know. The owned key passes while its sibling is still
	// reported as section-unknown.
	exemptions := CatalogExemptions{
		Keys: KeyExemptionsFromRegistry([]OwnedKey{
			{Section: "memcache", Key: "servers", OwnedBy: "operator-computed"},
		}),
	}

	unknown, deprecated := catalog.FindUnknownOptions(map[string]map[string]string{
		"memcache": {"servers": "127.0.0.1:11211", "dead_retry": "60"},
	}, exemptions)

	g.Expect(deprecated).To(BeNil())
	g.Expect(unknown).To(Equal([]UnknownOption{
		{Section: "memcache", Key: "dead_retry", SectionUnknown: true},
	}))
}

func TestFindUnknownOptions_SectionExemptionSkipsWhole(t *testing.T) {
	g := NewGomegaWithT(t)
	catalog := testCatalog()

	// A plugin section listed in ex.Sections is skipped whole even though its
	// keys are absent from the catalog.
	exemptions := CatalogExemptions{
		Sections: map[string]struct{}{"oslo_messaging_rabbit": {}},
	}

	unknown, deprecated := catalog.FindUnknownOptions(map[string]map[string]string{
		"oslo_messaging_rabbit": {"rabbit_qos_prefetch_count": "0", "heartbeat_timeout_threshold": "60"},
	}, exemptions)

	g.Expect(unknown).To(BeNil())
	g.Expect(deprecated).To(BeNil())
}

func TestFindUnknownOptions_DeprecatedAccepted(t *testing.T) {
	g := NewGomegaWithT(t)
	catalog := testCatalog()

	// "verbose" lives only in DEFAULT's Deprecated map: accepted, not unknown,
	// and returned with its replacement.
	unknown, deprecated := catalog.FindUnknownOptions(map[string]map[string]string{
		"DEFAULT": {"verbose": "true"},
	}, CatalogExemptions{})

	g.Expect(unknown).To(BeNil())
	g.Expect(deprecated).To(Equal([]DeprecatedOption{
		{Section: "DEFAULT", Key: "verbose", Replacement: "debug"},
	}))
}

func TestKeyExemptionsFromRegistry_NilIsEmptyNonNil(t *testing.T) {
	g := NewGomegaWithT(t)

	pairs := KeyExemptionsFromRegistry(nil)
	g.Expect(pairs).NotTo(BeNil())
	g.Expect(pairs).To(BeEmpty())
}

func TestKeyExemptionsFromRegistry_IncludesRejectedAndReported(t *testing.T) {
	g := NewGomegaWithT(t)

	// Both a rejected-class and a reported-class entry must land in the pair set.
	pairs := KeyExemptionsFromRegistry([]OwnedKey{
		{Section: "database", Key: "connection", Rejected: true, OwnedBy: "operator-computed"},
		{Section: "token", Key: "provider", Rejected: false, OwnedBy: "operator-computed"},
	})

	g.Expect(pairs).To(Equal(map[string]map[string]struct{}{
		"database": {"connection": {}},
		"token":    {"provider": {}},
	}))
}
