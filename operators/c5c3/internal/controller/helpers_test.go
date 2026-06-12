// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"strings"
	"testing"
	"time"
)

func TestIntervalToCron(t *testing.T) {
	tests := []struct {
		name     string
		interval time.Duration
		want     string
		wantErr  bool
	}{
		{
			name:     "168h maps to weekly Sunday midnight",
			interval: 168 * time.Hour,
			want:     "0 0 * * 0",
		},
		{
			name:     "24h maps to daily midnight",
			interval: 24 * time.Hour,
			want:     "0 0 * * *",
		},
		{
			name:     "multiple of 24h maps to daily midnight",
			interval: 72 * time.Hour,
			want:     "0 0 * * *",
		},
		{
			name:     "unsupported interval returns an error",
			interval: 5 * time.Hour,
			wantErr:  true,
		},
		{
			name:     "zero interval returns an error",
			interval: 0,
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := intervalToCron(tt.interval)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("intervalToCron(%v) = %q, want error", tt.interval, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("intervalToCron(%v) returned unexpected error: %v", tt.interval, err)
			}
			if got != tt.want {
				t.Errorf("intervalToCron(%v) = %q, want %q", tt.interval, got, tt.want)
			}
		})
	}
}

func TestIntervalToCronErrorNamesUnsupportedValue(t *testing.T) {
	const interval = 5 * time.Hour
	_, err := intervalToCron(interval)
	if err == nil {
		t.Fatalf("intervalToCron(%v) = nil error, want error naming the value", interval)
	}
	if !strings.Contains(err.Error(), interval.String()) {
		t.Errorf("error %q does not name unsupported value %q", err.Error(), interval.String())
	}
}
