// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import "time"

// Requeue interval constants centralise the polling durations used by each
// sub-reconciler. Keeping them in a single file makes tuning straightforward
// and ensures test assertions stay in sync with production code.
const (
	// RequeueDeploymentPolling is the interval for polling Deployment readiness.
	// Deployments converge quickly, so a short interval is appropriate.
	RequeueDeploymentPolling = 10 * time.Second

	// RequeueSecretPolling is the interval for polling ESO-managed Secret
	// readiness. ESO sync is fast but depends on an external vault, so a
	// moderate interval balances responsiveness with API load.
	RequeueSecretPolling = 15 * time.Second

	// RequeueDatabaseWait is the interval for waiting on MariaDB CR readiness
	// and db_sync Job completion. These operations are moderately slow, so a
	// longer interval avoids unnecessary API churn.
	RequeueDatabaseWait = 30 * time.Second

	// RequeueBootstrapWait is the interval for waiting on the bootstrap Job.
	// Bootstrap is a one-time heavyweight operation; gentle polling is
	// sufficient.
	RequeueBootstrapWait = 60 * time.Second

	// RequeueUpgradeWait is the interval for polling upgrade Job completion.
	// Upgrade Jobs (expand, migrate, contract) may take several minutes depending
	// on database size. A moderate interval balances responsiveness with API load.
	RequeueUpgradeWait = 30 * time.Second

	// RequeueValidationWait is the interval for polling policy validation Job
	// completion. The oslopolicy-validator runs quickly, so a short interval
	// balances responsiveness with API load.
	RequeueValidationWait = 15 * time.Second

	// RequeueHealthCheck is the interval for requeuing when the Keystone API
	// health check fails. The API may take a few seconds to start responding
	// after the Deployment reports ready, so a moderate interval is appropriate.
	RequeueHealthCheck = 10 * time.Second

	// HealthCheckTimeout is the bounded timeout for the HTTP health check
	// request. Prevents a hanging Keystone API from blocking the reconcile
	// loop indefinitely.
	HealthCheckTimeout = 10 * time.Second

	// defaultMaxConcurrentReconciles is the fallback worker count applied by
	// effectiveMaxConcurrentReconciles when the reconciler's
	// MaxConcurrentReconciles field is unset (<= 0). It matches the shared
	// bootstrap default so a programmatically constructed reconciler (envtest,
	// tests) still parallelises independent CRs instead of serialising at the
	// controller-runtime default of 1.
	defaultMaxConcurrentReconciles = 2

	// rateLimiterBaseDelay is the initial per-item requeue delay of the
	// controller's exponential failure rate limiter. It matches the
	// controller-runtime default so a first failure retries after 5ms.
	rateLimiterBaseDelay = 5 * time.Millisecond

	// rateLimiterMaxDelay caps the per-item exponential backoff. The
	// controller-runtime default of 1000s is far too conservative for an
	// I/O-bound operator; capping at 30s keeps a persistently failing CR
	// retrying on a bounded cadence. Genuinely slow external waits (DB,
	// bootstrap) do NOT ride this limiter — they use explicit RequeueAfter,
	// which enqueues via AddAfter and bypasses the failure backoff.
	rateLimiterMaxDelay = 30 * time.Second

	// OpenBaoAdoptionWaitTimeout bounds how long the OpenBao finalizer's Pass-0
	// adoption gate blocks Keystone CR deletion while waiting for ESO to stamp
	// its cleanup finalizer on a backup PushSecret. ESO adoption normally
	// completes within seconds, so this is generous enough never to trip under
	// healthy operation; it exists so a renamed or absent ESO finalizer cannot
	// hang CR deletion forever at WaitingForESOAdoption. Once exceeded, Pass-1
	// force-deletes the unadopted PushSecret after an ESOAdoptionTimedOut
	// Warning event (issue #475).
	OpenBaoAdoptionWaitTimeout = 10 * time.Minute
)
