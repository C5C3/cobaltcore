// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"
	"strconv"
	"strings"
)

// Release represents a parsed OpenStack release version.
type Release struct {
	Year  int    // e.g. 2025
	Minor int    // e.g. 2 (the N in YYYY.N)
	Patch string // e.g. "p1" (optional, empty for base releases)
	Raw   string // original unparsed string
}

// ParseRelease parses an OpenStack release version string in YYYY.N or YYYY.N-suffix format.
// Returns an error for unparseable formats.
// Valid examples: "2025.2", "2026.1", "2025.2-p1"
// Invalid examples: "latest", "abc", "2025", "2025.2.3"
func ParseRelease(tag string) (Release, error) {
	if tag == "" {
		return Release{}, fmt.Errorf("empty release tag")
	}

	// Separate optional patch suffix (e.g. "2025.2-p1" -> "2025.2", "p1").
	base, patch, _ := strings.Cut(tag, "-")

	// Parse YYYY.N from the base part.
	parts := strings.Split(base, ".")
	if len(parts) != 2 {
		return Release{}, fmt.Errorf("invalid release format %q: expected YYYY.N", tag)
	}

	year, err := strconv.Atoi(parts[0])
	if err != nil {
		return Release{}, fmt.Errorf("invalid release year in %q: %w", tag, err)
	}
	if year < 2010 {
		return Release{}, fmt.Errorf("invalid release year %d in %q: must be >= 2010", year, tag)
	}

	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return Release{}, fmt.Errorf("invalid release minor version in %q: %w", tag, err)
	}
	if minor != 1 && minor != 2 {
		return Release{}, fmt.Errorf("invalid release minor version %d in %q: must be 1 or 2", minor, tag)
	}

	return Release{
		Year:  year,
		Minor: minor,
		Patch: patch,
		Raw:   tag,
	}, nil
}

// IsSequentialUpgrade checks if upgrading from `from` to `to` is a valid sequential upgrade.
// Sequential means exactly one release step forward in the OpenStack release numbering:
//   - Same year, minor increments by 1: 2025.1 -> 2025.2
//   - Year increments by 1, from.Minor=2 -> to.Minor=1: 2025.2 -> 2026.1
//
// OpenStack releases 2 versions per year: YYYY.1 and YYYY.2.
// Patch suffix is ignored for sequential comparison.
func IsSequentialUpgrade(from, to Release) bool {
	if from.Year == to.Year && from.Minor == 1 && to.Minor == 2 {
		return true
	}
	if to.Year == from.Year+1 && from.Minor == 2 && to.Minor == 1 {
		return true
	}
	return false
}

// IsDowngrade checks if `to` is an older release than `from`.
// A release is older if its year is earlier, or the same year with a lower minor.
// Patch suffix is ignored for comparison.
func IsDowngrade(from, to Release) bool {
	return to.Year < from.Year || (to.Year == from.Year && to.Minor < from.Minor)
}

// IsPatchOnly checks if two releases differ only in their patch suffix.
// e.g., 2025.2 -> 2025.2-p1 is patch-only. 2025.2 -> 2026.1 is not.
func IsPatchOnly(from, to Release) bool {
	return from.Year == to.Year && from.Minor == to.Minor
}
