// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import "time"

// Nova-specific requeue intervals that no other operator shares.
const (
	// RequeueDatabaseWait is the interval for waiting on MariaDB CR readiness and
	// db-sync Job completion before either schema can be provisioned. Nova runs
	// two of them, the nova_api schema and the cell schema, and both migrations
	// are moderately slow, so a longer interval avoids unnecessary API churn. It
	// mirrors cinder's RequeueDatabaseWait.
	RequeueDatabaseWait = 30 * time.Second

	// RequeueUpgradeWait is the interval for polling upgrade Job completion.
	// Upgrade Jobs (expand, migrate, contract) may take several minutes depending
	// on database size. A moderate interval balances responsiveness with API load.
	// It mirrors cinder's RequeueUpgradeWait.
	RequeueUpgradeWait = 30 * time.Second

	// RequeueComputeServicePolling is how often a NovaCompute whose nodes are
	// all settled polls Nova for their compute services. It is the only signal
	// a service going down or being disabled from outside has: Nova sends none.
	RequeueComputeServicePolling = 60 * time.Second

	// RequeueComputeDrainPolling is how often a NovaCompute polls a node that
	// is Draining (waiting for its instances to leave) or Pending (waiting for
	// its service to register). It is also the backoff after a failed Keystone
	// or Nova call.
	RequeueComputeDrainPolling = 30 * time.Second

	// RequeueComputeReleasePolling is how often a NovaCompute polls a node that
	// is Releasing, waiting for its pod to leave before the compute service is
	// deleted. The wait is a pod termination, so it is short.
	RequeueComputeReleasePolling = 10 * time.Second
)
