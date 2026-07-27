// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import "time"

// Glance-specific requeue intervals that no other operator shares.
const (
	// RequeueDatabaseWait is the interval for waiting on MariaDB CR readiness,
	// db-sync Job completion, and a ready default backend before the database
	// schema can be provisioned. These operations are moderately slow, so a
	// longer interval avoids unnecessary API churn. It mirrors keystone's
	// RequeueDatabaseWait.
	RequeueDatabaseWait = 30 * time.Second

	// RequeueUpgradeWait is the interval for polling upgrade Job completion.
	// Upgrade Jobs (expand, migrate, contract) may take several minutes depending
	// on database size. A moderate interval balances responsiveness with API load.
	// It mirrors keystone's RequeueUpgradeWait.
	RequeueUpgradeWait = 30 * time.Second
)
