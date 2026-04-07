// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Feature: CC-0044

package controller

import "time"

// Requeue interval constants centralise the polling durations used by each
// sub-reconciler. Keeping them in a single file makes tuning straightforward
// and ensures test assertions stay in sync with production code (CC-0044).
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
)
