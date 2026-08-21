// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"

	"github.com/c5c3/cobaltcore/internal/common/release"
)

// CatalogSection is one INI section of a per-release option catalog. Opts lists
// the option names oslo.config accepts in that section, spelled exactly as they
// appear when read from a file (the underscore form). Deprecated maps a still
// accepted but deprecated option name to its replacement, so a rendered override
// of a deprecated option is honored while callers can surface the migration.
type CatalogSection struct {
	Opts       []string          `json:"opts"`
	Deprecated map[string]string `json:"deprecated"`
}

// OptionCatalog is the set of option names a single OpenStack service accepts in
// a single release. Service is the service name ("keystone", "glance"), Release
// is the release identifier the catalog was generated for, and Sections maps an
// INI section name to the options it accepts. It is the release-pinned reference
// that FindUnknownOptions validates a merged configuration against.
type OptionCatalog struct {
	Service  string                    `json:"service"`
	Release  string                    `json:"release"`
	Sections map[string]CatalogSection `json:"sections"`
}

// ParseOptionCatalog decodes the JSON option-catalog document in data into an
// OptionCatalog. It wraps an unmarshal failure as "parsing option catalog: %w"
// and rejects a document that carries no sections (a nil or empty Sections map)
// with "option catalog has no sections", since a catalog with nothing to
// validate against is never a usable reference.
func ParseOptionCatalog(data []byte) (*OptionCatalog, error) {
	var catalog OptionCatalog
	if err := json.Unmarshal(data, &catalog); err != nil {
		return nil, fmt.Errorf("parsing option catalog: %w", err)
	}
	if len(catalog.Sections) == 0 {
		return nil, errors.New("option catalog has no sections")
	}
	return &catalog, nil
}

// MustParseEmbeddedCatalogs parses every file under the "catalogs" directory of
// fsys into a map keyed by each file's YYYY.N stem. It panics on any read or parse
// failure: the catalogs are build artifacts vendored into an operator, so a broken
// or missing catalog is a build defect that must fail loudly at startup rather
// than a runtime condition to tolerate. A unit test parses the same files, so the
// panic path can never first surface in production.
func MustParseEmbeddedCatalogs(fsys fs.FS) map[string]*OptionCatalog {
	entries, err := fs.ReadDir(fsys, "catalogs")
	if err != nil {
		panic(fmt.Sprintf("reading embedded catalogs directory: %v", err))
	}
	catalogs := make(map[string]*OptionCatalog, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		data, err := fs.ReadFile(fsys, "catalogs/"+name)
		if err != nil {
			panic(fmt.Sprintf("reading embedded catalog %q: %v", name, err))
		}
		catalog, err := ParseOptionCatalog(data)
		if err != nil {
			panic(fmt.Sprintf("parsing embedded catalog %q: %v", name, err))
		}
		// Strip the ".json" extension to recover the YYYY.N stem.
		stem := name[:len(name)-len(".json")]
		catalogs[stem] = catalog
	}
	return catalogs
}

// LookupCatalog returns the option catalog in catalogs for the OpenStack release
// named by tag. The tag is parsed as a release version (YYYY.N with an optional
// -pN patch suffix); the patch suffix is stripped before lookup, so a patch
// release shares its base release's catalog. It returns (nil, false) when the tag
// does not name a release (an empty tag, a digest pin, or "latest") or when
// catalogs holds no entry for the resolved YYYY.N base.
func LookupCatalog(catalogs map[string]*OptionCatalog, tag string) (*OptionCatalog, bool) {
	rel, err := release.ParseRelease(tag)
	if err != nil {
		return nil, false
	}
	base := fmt.Sprintf("%d.%d", rel.Year, rel.Minor)
	catalog, ok := catalogs[base]
	if !ok {
		return nil, false
	}
	return catalog, true
}

// CatalogExemptions carves the parts of a merged configuration that a catalog
// cannot judge out of the unknown-option scan. Sections names INI sections
// skipped whole because their option set is not enumerable from the release
// catalog: oslo plugin sections and per-service dynamically named sections. Keys
// maps a section name to the option names skipped individually within it, used
// to carry the ownership registry so an operator-owned key never doubles as an
// unknown option.
type CatalogExemptions struct {
	Sections map[string]struct{}
	Keys     map[string]map[string]struct{}
}

// KeyExemptionsFromRegistry builds the (section, key) exemption set consumed as
// CatalogExemptions.Keys from an OwnedKey registry. Every registry entry is
// included regardless of its Rejected value, because an operator-owned key is
// out of the catalog's scope whether the webhook rejects the override or only
// reports it. A nil registry yields an empty, non-nil map.
func KeyExemptionsFromRegistry(registry []OwnedKey) map[string]map[string]struct{} {
	pairs := make(map[string]map[string]struct{})
	for _, entry := range registry {
		if pairs[entry.Section] == nil {
			pairs[entry.Section] = make(map[string]struct{})
		}
		pairs[entry.Section][entry.Key] = struct{}{}
	}
	return pairs
}

// UnknownOption is a configuration option the catalog does not accept. Section
// and Key locate the override; SectionUnknown is true when the whole INI section
// is absent from the catalog and false when the section exists but does not list
// the key.
type UnknownOption struct {
	Section        string
	Key            string
	SectionUnknown bool
}

// DeprecatedOption is a configuration option the catalog still accepts but marks
// as deprecated. Section and Key locate the override; Replacement is the option
// name the catalog records as its successor.
type DeprecatedOption struct {
	Section     string
	Key         string
	Replacement string
}

// FindUnknownOptions validates extraConfig against the catalog and returns the
// options it does not accept and the deprecated-but-accepted options it carries.
// It is a pure function with no error path: a nil or empty extraConfig returns
// (nil, nil).
//
// A section named in ex.Sections is skipped whole; a (section, key) pair whose
// key is in ex.Keys[section] is skipped individually. For a remaining key, a
// section absent from the catalog yields an UnknownOption with SectionUnknown
// true, a key the section lists in neither Opts nor Deprecated yields an
// UnknownOption with SectionUnknown false, a key present only in Deprecated
// yields a DeprecatedOption carrying its replacement, and a key in Opts is
// accepted silently. Keys are compared verbatim, so a dash-spelled key is
// unknown by design because the catalog holds the underscore form. Both slices
// come back sorted by section then key.
func (c *OptionCatalog) FindUnknownOptions(extraConfig map[string]map[string]string, ex CatalogExemptions) ([]UnknownOption, []DeprecatedOption) {
	if len(extraConfig) == 0 {
		return nil, nil
	}

	var unknown []UnknownOption
	var deprecated []DeprecatedOption
	for section, keys := range extraConfig {
		if _, skip := ex.Sections[section]; skip {
			continue
		}
		exemptKeys := ex.Keys[section]
		catalogSection, known := c.Sections[section]
		// Materialize the section's accepted-option set once on entry so the
		// per-key lookup below never rescans the Opts slice.
		var opts map[string]struct{}
		if known {
			opts = make(map[string]struct{}, len(catalogSection.Opts))
			for _, opt := range catalogSection.Opts {
				opts[opt] = struct{}{}
			}
		}
		for key := range keys {
			if _, exempt := exemptKeys[key]; exempt {
				continue
			}
			if !known {
				unknown = append(unknown, UnknownOption{Section: section, Key: key, SectionUnknown: true})
				continue
			}
			if _, ok := opts[key]; ok {
				continue
			}
			if replacement, ok := catalogSection.Deprecated[key]; ok {
				deprecated = append(deprecated, DeprecatedOption{Section: section, Key: key, Replacement: replacement})
				continue
			}
			unknown = append(unknown, UnknownOption{Section: section, Key: key})
		}
	}

	sort.Slice(unknown, func(i, j int) bool {
		if unknown[i].Section != unknown[j].Section {
			return unknown[i].Section < unknown[j].Section
		}
		return unknown[i].Key < unknown[j].Key
	})
	sort.Slice(deprecated, func(i, j int) bool {
		if deprecated[i].Section != deprecated[j].Section {
			return deprecated[i].Section < deprecated[j].Section
		}
		return deprecated[i].Key < deprecated[j].Key
	})
	return unknown, deprecated
}
