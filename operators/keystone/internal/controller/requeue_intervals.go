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

	// HealthCheckCacheTTL bounds how long a successful Keystone API probe is
	// reused before the operator re-probes. Every reconcile pass otherwise
	// fires a synchronous HTTP GET (bounded by HealthCheckTimeout, up to 10s on
	// a flapping API), which dominates hot-path latency once a CR is Ready.
	// Caching for 30s suppresses re-probes during event/resync bursts.
	//
	// Trade-off: a wedged-but-Ready Keystone API — one whose pods still pass
	// their readiness probe (so DeploymentReady stays True) while requests hang
	// — is masked for up to HealthCheckCacheTTL after the last good probe.
	// Within that window reconciles serve KeystoneAPIReady=True from cache
	// without probing, so failure-detection latency for this case is increased
	// by up to HealthCheckCacheTTL. The probe-error/non-2xx eviction does NOT
	// bound this: eviction fires only once a probe runs, and inside the TTL the
	// cache is exactly what suppresses that probe. Keep this TTL at or below the
	// KeystoneAPIReady outage-detection SLO.
	HealthCheckCacheTTL = 30 * time.Second

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
