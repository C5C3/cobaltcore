// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"

	. "github.com/onsi/gomega"
)

func TestParseRelease(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantErr   bool
		wantYear  int
		wantMinor int
		wantPatch string
		wantRaw   string
	}{
		{
			name:      "valid base release 2025.2",
			input:     "2025.2",
			wantYear:  2025,
			wantMinor: 2,
			wantPatch: "",
			wantRaw:   "2025.2",
		},
		{
			name:      "valid base release 2026.1",
			input:     "2026.1",
			wantYear:  2026,
			wantMinor: 1,
			wantPatch: "",
			wantRaw:   "2026.1",
		},
		{
			name:      "valid release with patch suffix",
			input:     "2025.2-p1",
			wantYear:  2025,
			wantMinor: 2,
			wantPatch: "p1",
			wantRaw:   "2025.2-p1",
		},
		{
			name:      "valid release with rc suffix",
			input:     "2024.1-rc1",
			wantYear:  2024,
			wantMinor: 1,
			wantPatch: "rc1",
			wantRaw:   "2024.1-rc1",
		},
		{
			name:    "invalid: latest",
			input:   "latest",
			wantErr: true,
		},
		{
			name:    "invalid: abc",
			input:   "abc",
			wantErr: true,
		},
		{
			name:    "invalid: no minor version",
			input:   "2025",
			wantErr: true,
		},
		{
			name:    "invalid: minor must be 1 or 2",
			input:   "2025.3",
			wantErr: true,
		},
		{
			name:    "invalid: minor zero",
			input:   "2025.0",
			wantErr: true,
		},
		{
			name:    "invalid: too many dots",
			input:   "2025.2.3",
			wantErr: true,
		},
		{
			name:    "invalid: empty string",
			input:   "",
			wantErr: true,
		},
		{
			name:    "invalid: year too old",
			input:   "1999.1",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)

			rel, err := ParseRelease(tt.input)
			if tt.wantErr {
				g.Expect(err).To(HaveOccurred())
				return
			}

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(rel.Year).To(Equal(tt.wantYear))
			g.Expect(rel.Minor).To(Equal(tt.wantMinor))
			g.Expect(rel.Patch).To(Equal(tt.wantPatch))
			g.Expect(rel.Raw).To(Equal(tt.wantRaw))
		})
	}
}

func TestIsSequentialUpgrade(t *testing.T) {
	tests := []struct {
		name string
		from Release
		to   Release
		want bool
	}{
		{
			name: "same year minor increment",
			from: Release{Year: 2025, Minor: 1},
			to:   Release{Year: 2025, Minor: 2},
			want: true,
		},
		{
			name: "year rollover",
			from: Release{Year: 2025, Minor: 2},
			to:   Release{Year: 2026, Minor: 1},
			want: true,
		},
		{
			name: "another year rollover",
			from: Release{Year: 2024, Minor: 2},
			to:   Release{Year: 2025, Minor: 1},
			want: true,
		},
		{
			name: "same version is not sequential",
			from: Release{Year: 2025, Minor: 1},
			to:   Release{Year: 2025, Minor: 1},
			want: false,
		},
		{
			name: "downgrade is not sequential",
			from: Release{Year: 2025, Minor: 2},
			to:   Release{Year: 2025, Minor: 1},
			want: false,
		},
		{
			name: "skip-level is not sequential",
			from: Release{Year: 2024, Minor: 2},
			to:   Release{Year: 2026, Minor: 1},
			want: false,
		},
		{
			name: "skipping .2 is not sequential",
			from: Release{Year: 2025, Minor: 1},
			to:   Release{Year: 2026, Minor: 1},
			want: false,
		},
		{
			name: "skipping two steps is not sequential",
			from: Release{Year: 2025, Minor: 1},
			to:   Release{Year: 2026, Minor: 2},
			want: false,
		},
		{
			name: "sequential ignoring patch suffix",
			from: Release{Year: 2025, Minor: 1, Patch: "p1"},
			to:   Release{Year: 2025, Minor: 2},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			g.Expect(IsSequentialUpgrade(tt.from, tt.to)).To(Equal(tt.want))
		})
	}
}

func TestIsDowngrade(t *testing.T) {
	tests := []struct {
		name string
		from Release
		to   Release
		want bool
	}{
		{
			name: "same year lower minor is downgrade",
			from: Release{Year: 2025, Minor: 2},
			to:   Release{Year: 2025, Minor: 1},
			want: true,
		},
		{
			name: "earlier year is downgrade",
			from: Release{Year: 2026, Minor: 1},
			to:   Release{Year: 2025, Minor: 2},
			want: true,
		},
		{
			name: "earlier year same minor is downgrade",
			from: Release{Year: 2026, Minor: 1},
			to:   Release{Year: 2025, Minor: 1},
			want: true,
		},
		{
			name: "same version is not downgrade",
			from: Release{Year: 2025, Minor: 2},
			to:   Release{Year: 2025, Minor: 2},
			want: false,
		},
		{
			name: "upgrade is not downgrade",
			from: Release{Year: 2025, Minor: 1},
			to:   Release{Year: 2025, Minor: 2},
			want: false,
		},
		{
			name: "year rollover upgrade is not downgrade",
			from: Release{Year: 2025, Minor: 2},
			to:   Release{Year: 2026, Minor: 1},
			want: false,
		},
		{
			name: "patch suffix ignored",
			from: Release{Year: 2025, Minor: 2, Patch: "p1"},
			to:   Release{Year: 2025, Minor: 1},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			g.Expect(IsDowngrade(tt.from, tt.to)).To(Equal(tt.want))
		})
	}
}

func TestIsPatchOnly(t *testing.T) {
	tests := []struct {
		name string
		from Release
		to   Release
		want bool
	}{
		{
			name: "adding patch suffix",
			from: Release{Year: 2025, Minor: 2},
			to:   Release{Year: 2025, Minor: 2, Patch: "p1"},
			want: true,
		},
		{
			name: "changing patch suffix",
			from: Release{Year: 2025, Minor: 2, Patch: "p1"},
			to:   Release{Year: 2025, Minor: 2, Patch: "p2"},
			want: true,
		},
		{
			name: "removing patch suffix",
			from: Release{Year: 2025, Minor: 2, Patch: "p1"},
			to:   Release{Year: 2025, Minor: 2},
			want: true,
		},
		{
			name: "different minor is not patch only",
			from: Release{Year: 2025, Minor: 1},
			to:   Release{Year: 2025, Minor: 2},
			want: false,
		},
		{
			name: "different year is not patch only",
			from: Release{Year: 2025, Minor: 2},
			to:   Release{Year: 2026, Minor: 1},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			g.Expect(IsPatchOnly(tt.from, tt.to)).To(Equal(tt.want))
		})
	}
}
