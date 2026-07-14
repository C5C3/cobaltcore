// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"
	"time"

	commonv1 "github.com/c5c3/forge/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
)

// The effective-* resolvers below answer the one question every consumer of a
// backing service asks: which INSTANCE does this service actually talk to? A
// service that opted into a dedicated instance
// (services.<svc>.dedicatedBackingServices.<class>) talks to that one; a service
// that did not — the default — shares the ControlPlane-wide instance in
// spec.infrastructure.
//
// Routing every consumer through these resolvers is what makes the opt-in carry
// the same lifecycle guarantees as the shared block with no per-class
// special-casing: the infrastructure sub-reconciler provisions and gates
// readiness on the effective instances, the service projections point the child
// CRs at them (which in turn carries the child operators' credential wiring and
// NetworkPolicy egress derivation, both pure functions of the projected
// spec.database / spec.cache), and the DB-credential sub-reconciler decides the
// credential shape from them.
//
// They return nil when nothing resolves — an External-mode ControlPlane (no
// backing services at all) or a webhook-bypassed CR that dropped
// spec.infrastructure. Callers fail closed on nil rather than dereferencing it.

// effectiveKeystoneDatabase resolves the database instance the Keystone service
// connects to.
func effectiveKeystoneDatabase(cp *c5c3v1alpha1.ControlPlane) *commonv1.DatabaseSpec {
	if db := cp.DedicatedKeystoneDatabase(); db != nil {
		return db
	}
	if cp.Spec.Infrastructure != nil {
		return &cp.Spec.Infrastructure.Database
	}
	return nil
}

// effectiveKeystoneCache resolves the cache instance the Keystone service
// connects to.
func effectiveKeystoneCache(cp *c5c3v1alpha1.ControlPlane) *commonv1.CacheSpec {
	if cache := cp.DedicatedKeystoneCache(); cache != nil {
		return cache
	}
	if cp.Spec.Infrastructure != nil {
		return &cp.Spec.Infrastructure.Cache
	}
	return nil
}

// effectiveHorizonCache resolves the cache instance the Horizon dashboard
// connects to.
func effectiveHorizonCache(cp *c5c3v1alpha1.ControlPlane) *commonv1.CacheSpec {
	if cache := cp.DedicatedHorizonCache(); cache != nil {
		return cache
	}
	if cp.Spec.Infrastructure != nil {
		return &cp.Spec.Infrastructure.Cache
	}
	return nil
}

// intervalToCron converts a rotation interval into a cron expression suitable
// for a Kubernetes CronJob schedule.
//
// Supported intervals:
//   - 168h (7 days)                -> "0 0 * * 0" (weekly, Sunday midnight)
//   - any positive multiple of 24h -> "0 0 * * *" (daily midnight)
//
// The weekly case is matched first so the canonical 7-day interval yields the
// weekly schedule rather than a daily one. Any interval that is not a positive
// whole number of days returns an error naming the unsupported value.
//
// FIDELITY NOTE every multi-day interval that is NOT exactly 168h
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
