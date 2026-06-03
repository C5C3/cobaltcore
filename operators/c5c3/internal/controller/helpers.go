// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"
	"time"

	commonv1 "github.com/c5c3/forge/internal/common/types"
)

// Feature: CC-0110 (REQ-016, REQ-017)

// intervalToCron converts a rotation interval into a cron expression suitable
// for a Kubernetes CronJob schedule (REQ-016).
//
// Supported intervals:
//   - 168h (7 days)                -> "0 0 * * 0" (weekly, Sunday midnight)
//   - any positive multiple of 24h -> "0 0 * * *" (daily midnight)
//
// The weekly case is matched first so the canonical 7-day interval yields the
// weekly schedule rather than a daily one. Any interval that is not a positive
// whole number of days returns an error naming the unsupported value.
//
// FIDELITY NOTE (CC-0110): every multi-day interval that is NOT exactly 168h
// (e.g. 48h, 72h, 336h) INTENTIONALLY collapses to the same daily schedule —
// the cron form models only "daily" and "weekly", so the interval magnitude
// beyond {24h, 168h} is not preserved. This is a deliberate, documented
// limitation; a future level that needs other cadences must extend this
// mapping. The ControlPlane admission webhook (controlplane_webhook.go) rejects
// any rotationInterval that is not a positive whole number of days, so only the
// daily/weekly-representable set reaches this function in production.
func intervalToCron(d time.Duration) (string, error) {
	const (
		day  = 24 * time.Hour
		week = 7 * day
	)

	switch {
	case d == week:
		return "0 0 * * 0", nil
	case d > 0 && d%day == 0:
		return "0 0 * * *", nil
	default:
		return "", fmt.Errorf("unsupported rotation interval %q: must be 168h (weekly) or a positive multiple of 24h (daily)", d)
	}
}

// projectPolicyOverrides merges a global policy (base) with a per-service
// policy (overrides) into a single freshly allocated *commonv1.PolicySpec
// (REQ-017). Per-service values win on conflict:
//
//   - Rules: start from global.Rules, then per-service rules overwrite on key
//     conflict.
//   - ConfigMapRef: per-service wins when set, otherwise global's is used.
//
// Nil handling: both nil returns nil; if exactly one side is nil a fresh copy
// of the other is returned. The inputs are never mutated or aliased — the
// returned struct, its Rules map, and its ConfigMapRef are independent copies.
func projectPolicyOverrides(global, perService *commonv1.PolicySpec) *commonv1.PolicySpec {
	if global == nil && perService == nil {
		return nil
	}
	if global == nil {
		return copyPolicySpec(perService)
	}
	if perService == nil {
		return copyPolicySpec(global)
	}

	merged := &commonv1.PolicySpec{}

	// Merge Rules: global is the base, per-service overrides on key conflict.
	if global.Rules != nil || perService.Rules != nil {
		rules := make(map[string]string, len(global.Rules)+len(perService.Rules))
		for k, v := range global.Rules {
			rules[k] = v
		}
		for k, v := range perService.Rules {
			rules[k] = v
		}
		merged.Rules = rules
	}

	// ConfigMapRef: per-service wins when set, else fall back to global.
	switch {
	case perService.ConfigMapRef != nil:
		merged.ConfigMapRef = perService.ConfigMapRef.DeepCopy()
	case global.ConfigMapRef != nil:
		merged.ConfigMapRef = global.ConfigMapRef.DeepCopy()
	}

	return merged
}

// copyPolicySpec returns a fresh *commonv1.PolicySpec whose Rules map and
// ConfigMapRef are independent copies of src's, so callers can mutate the
// result without affecting the original. src must be non-nil.
func copyPolicySpec(src *commonv1.PolicySpec) *commonv1.PolicySpec {
	out := &commonv1.PolicySpec{}
	if src.Rules != nil {
		rules := make(map[string]string, len(src.Rules))
		for k, v := range src.Rules {
			rules[k] = v
		}
		out.Rules = rules
	}
	if src.ConfigMapRef != nil {
		out.ConfigMapRef = src.ConfigMapRef.DeepCopy()
	}
	return out
}
