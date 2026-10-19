// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package release

import (
	"fmt"
	"testing"
)

// FuzzParseRelease checks that ParseRelease returns the zero Release with
// every error, keeps an accepted release inside its year and minor bounds,
// and agrees with itself on the canonical YYYY.N[-patch] form of what it
// accepted.
func FuzzParseRelease(f *testing.F) {
	for _, tag := range []string{
		// The inputs of TestParseRelease.
		"2025.2", "2026.1", "2026.2", "2025.2-p1", "2024.1-rc1",
		"latest", "abc", "2025", "2025.3", "2025.0", "2025.2.3", "", "1999.1",
		// A sign and leading zeros strconv.Atoi accepts, an empty patch, a
		// patch holding a dash, and a year out of int range.
		"+2026.1", "02026.1", "2026.1-", "2026.1-p1-p2", "99999999999999999999.1",
	} {
		f.Add(tag)
	}

	f.Fuzz(func(t *testing.T, tag string) {
		rel, err := ParseRelease(tag)
		again, errAgain := ParseRelease(tag)
		if again != rel || (errAgain == nil) != (err == nil) {
			t.Fatalf("determinism: ParseRelease(%q) returned (%+v, %v), then (%+v, %v)", tag, rel, err, again, errAgain)
		}
		if err != nil {
			if rel != (Release{}) {
				t.Fatalf("zero value on error: ParseRelease(%q) = (%+v, %v), want the zero Release", tag, rel, err)
			}
			return
		}
		if rel.Raw != tag || rel.Year < 2010 || (rel.Minor != 1 && rel.Minor != 2) {
			t.Fatalf("bounds: ParseRelease(%q) = %+v, want Raw %q, Year >= 2010 and Minor 1 or 2", tag, rel, tag)
		}

		canonical := fmt.Sprintf("%d.%d", rel.Year, rel.Minor)
		if rel.Patch != "" {
			canonical += "-" + rel.Patch
		}
		reparsed, err := ParseRelease(canonical)
		if err != nil {
			t.Fatalf("canonical re-parse: ParseRelease(%q) for %q failed: %v", canonical, tag, err)
		}
		if reparsed.Year != rel.Year || reparsed.Minor != rel.Minor || reparsed.Patch != rel.Patch {
			t.Fatalf("canonical re-parse: ParseRelease(%q) = %+v, want %+v for %q", canonical, reparsed, rel, tag)
		}
		if !IsPatchOnly(rel, reparsed) || rel.IsSupported() != reparsed.IsSupported() {
			t.Fatalf("canonical re-parse: %+v and %+v disagree on IsPatchOnly or IsSupported for %q", rel, reparsed, tag)
		}
	})
}
