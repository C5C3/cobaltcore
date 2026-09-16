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
)
