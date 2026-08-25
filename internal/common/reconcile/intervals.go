// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package reconcile

import "time"

// Requeue interval constants centralise the polling durations shared by every
// service operator's sub-reconcilers. Keeping them in one place makes tuning
// straightforward and ensures test assertions stay in sync with production
// code. Service-specific intervals (database, bootstrap, upgrade, …) stay in
// the operator that owns them.
const (
	// RequeueDeploymentPolling is the interval for polling Deployment readiness.
	// Deployments converge quickly, so a short interval is appropriate.
	RequeueDeploymentPolling = 10 * time.Second

	// RequeueSecretPolling is the interval for polling ESO-managed Secret
	// readiness. ESO sync is fast but depends on an external vault, so a
	// moderate interval balances responsiveness with API load.
	RequeueSecretPolling = 15 * time.Second

	// RequeueNextPass is the interval a reconcile returns after it has persisted
	// state the next pass must observe — a finalizer it just added, an annotation
	// it just recorded, an upgrade phase it just advanced. It replaces the
	// deprecated Result.Requeue flag: short enough that the follow-up pass reads
	// the write back immediately, long enough not to spin the work queue when the
	// write has not reached the informer cache yet.
	RequeueNextPass = 1 * time.Second
)
