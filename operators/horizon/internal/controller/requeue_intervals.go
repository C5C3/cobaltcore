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

	// RequeueHealthCheck is the interval for requeuing when the dashboard
	// health check fails. The login page may take a few seconds to start
	// responding after the Deployment reports ready.
	RequeueHealthCheck = 10 * time.Second

	// HealthCheckTimeout is the bounded timeout for the HTTP health check
	// request. Prevents a hanging dashboard from blocking the reconcile loop
	// indefinitely.
	HealthCheckTimeout = 10 * time.Second

	// HealthCheckCacheTTL bounds how long a successful login-page probe is
	// reused before the operator re-probes. Every reconcile pass otherwise
	// fires a synchronous HTTP GET (bounded by HealthCheckTimeout) which
	// dominates hot-path latency once a CR is Ready. Keep this TTL at or
	// below the HorizonAPIReady outage-detection SLO.
	HealthCheckCacheTTL = 30 * time.Second
)
