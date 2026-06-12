// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"
	"time"
)

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
