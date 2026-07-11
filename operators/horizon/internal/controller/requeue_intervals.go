// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"github.com/c5c3/forge/internal/common/healthcheck"
	commonreconcile "github.com/c5c3/forge/internal/common/reconcile"
)

// The polling and health-check timing durations shared by every service
// operator now live in the shared library so a tuning change lands once. These
// aliases keep the package-local names the sub-reconcilers reference.
const (
	// RequeueDeploymentPolling is the interval for polling Deployment readiness.
	RequeueDeploymentPolling = commonreconcile.RequeueDeploymentPolling

	// RequeueSecretPolling is the interval for polling ESO-managed Secret
	// readiness.
	RequeueSecretPolling = commonreconcile.RequeueSecretPolling

	// RequeueHealthCheck is the interval for requeuing when the dashboard
	// health check fails.
	RequeueHealthCheck = healthcheck.RequeueHealthCheck

	// HealthCheckTimeout is the bounded timeout for the HTTP health check
	// request.
	HealthCheckTimeout = healthcheck.HealthCheckTimeout

	// HealthCheckCacheTTL bounds how long a successful login-page probe is
	// reused before the operator re-probes.
	HealthCheckCacheTTL = healthcheck.HealthCheckCacheTTL
)
