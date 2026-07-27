// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import "time"

// Keystone-specific requeue intervals that no other operator shares.
const (
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
