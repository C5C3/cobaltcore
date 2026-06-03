// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"reflect"
	"strings"
	"testing"
	"time"

	commonv1 "github.com/c5c3/forge/internal/common/types"

	corev1 "k8s.io/api/core/v1"
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

func TestProjectPolicyOverridesPerServiceWinsOnConflict(t *testing.T) {
	global := &commonv1.PolicySpec{
		Rules: map[string]string{
			"compute:create": "role:admin",
			"compute:delete": "role:admin",
		},
		ConfigMapRef: &corev1.LocalObjectReference{Name: "global-cm"},
	}
	perService := &commonv1.PolicySpec{
		Rules: map[string]string{
			"compute:create": "role:member", // overrides global
			"compute:list":   "role:reader", // new key
		},
		ConfigMapRef: &corev1.LocalObjectReference{Name: "service-cm"},
	}

	got := projectPolicyOverrides(global, perService)

	wantRules := map[string]string{
		"compute:create": "role:member",
		"compute:delete": "role:admin",
		"compute:list":   "role:reader",
	}
	if !reflect.DeepEqual(got.Rules, wantRules) {
		t.Errorf("merged Rules = %v, want %v", got.Rules, wantRules)
	}
	if got.ConfigMapRef == nil || got.ConfigMapRef.Name != "service-cm" {
		t.Errorf("ConfigMapRef = %v, want perService 'service-cm'", got.ConfigMapRef)
	}
}

func TestProjectPolicyOverridesConfigMapRefFallsBackToGlobal(t *testing.T) {
	global := &commonv1.PolicySpec{
		ConfigMapRef: &corev1.LocalObjectReference{Name: "global-cm"},
	}
	perService := &commonv1.PolicySpec{
		Rules: map[string]string{"compute:list": "role:reader"},
	}

	got := projectPolicyOverrides(global, perService)

	if got.ConfigMapRef == nil || got.ConfigMapRef.Name != "global-cm" {
		t.Errorf("ConfigMapRef = %v, want fallback to global 'global-cm'", got.ConfigMapRef)
	}
}

func TestProjectPolicyOverridesGlobalOnly(t *testing.T) {
	global := &commonv1.PolicySpec{
		Rules:        map[string]string{"compute:create": "role:admin"},
		ConfigMapRef: &corev1.LocalObjectReference{Name: "global-cm"},
	}

	got := projectPolicyOverrides(global, nil)

	if got == nil {
		t.Fatal("projectPolicyOverrides(global, nil) = nil, want copy of global")
	}
	if !reflect.DeepEqual(got.Rules, global.Rules) {
		t.Errorf("Rules = %v, want %v", got.Rules, global.Rules)
	}
	if got.ConfigMapRef == nil || got.ConfigMapRef.Name != "global-cm" {
		t.Errorf("ConfigMapRef = %v, want 'global-cm'", got.ConfigMapRef)
	}
}

func TestProjectPolicyOverridesPerServiceOnly(t *testing.T) {
	perService := &commonv1.PolicySpec{
		Rules:        map[string]string{"compute:list": "role:reader"},
		ConfigMapRef: &corev1.LocalObjectReference{Name: "service-cm"},
	}

	got := projectPolicyOverrides(nil, perService)

	if got == nil {
		t.Fatal("projectPolicyOverrides(nil, perService) = nil, want copy of perService")
	}
	if !reflect.DeepEqual(got.Rules, perService.Rules) {
		t.Errorf("Rules = %v, want %v", got.Rules, perService.Rules)
	}
	if got.ConfigMapRef == nil || got.ConfigMapRef.Name != "service-cm" {
		t.Errorf("ConfigMapRef = %v, want 'service-cm'", got.ConfigMapRef)
	}
}

func TestProjectPolicyOverridesBothNilReturnsNil(t *testing.T) {
	if got := projectPolicyOverrides(nil, nil); got != nil {
		t.Errorf("projectPolicyOverrides(nil, nil) = %v, want nil", got)
	}
}

func TestProjectPolicyOverridesDoesNotMutateInputs(t *testing.T) {
	global := &commonv1.PolicySpec{
		Rules:        map[string]string{"compute:create": "role:admin"},
		ConfigMapRef: &corev1.LocalObjectReference{Name: "global-cm"},
	}
	perService := &commonv1.PolicySpec{
		Rules:        map[string]string{"compute:create": "role:member"},
		ConfigMapRef: &corev1.LocalObjectReference{Name: "service-cm"},
	}

	globalRulesBefore := map[string]string{"compute:create": "role:admin"}
	perServiceRulesBefore := map[string]string{"compute:create": "role:member"}

	got := projectPolicyOverrides(global, perService)

	// Mutating the result must not bleed back into the inputs.
	got.Rules["compute:create"] = "role:tampered"
	got.Rules["compute:new"] = "role:added"

	if !reflect.DeepEqual(global.Rules, globalRulesBefore) {
		t.Errorf("global.Rules mutated: got %v, want %v", global.Rules, globalRulesBefore)
	}
	if !reflect.DeepEqual(perService.Rules, perServiceRulesBefore) {
		t.Errorf("perService.Rules mutated: got %v, want %v", perService.Rules, perServiceRulesBefore)
	}
}
