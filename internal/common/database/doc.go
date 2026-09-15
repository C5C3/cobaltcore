// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package database manages MariaDB database resources for CobaltCore operators.
//
// It provides both the low-level primitives — ensuring Database, User, and
// Grant CRs exist (EnsureDatabase/EnsureDatabaseUser), assembling the pymysql
// DSN (BuildDSN/Digest, SSLParams/AppendTLSParams), and resolving host/port/
// username from the shared DatabaseSpec — and the reconcile flows layered on
// top that a database-backed service operator wires into its reconcile loop:
//
//   - ReconcileProvision drives the managed/brownfield provisioning branch: the
//     MariaDB cluster-Ready gate, the Database/User/Grant ensure, and the
//     Dynamic-credentials skip of the User/Grant. It also provisions a block's
//     additional schemas (AdditionalDatabaseNames) as one more Database per
//     schema and, in Static mode, one more Grant per schema on the block's user.
//   - ReconcileSyncJobs sequences the db-sync and schema-check migration Jobs
//     (built from the parameterized JobSetParams table) and promotes the
//     installed-release marker.
//   - ReconcileUpgrade drives the expand-migrate-contract release upgrades
//     shared by keystone and glance: it detects the upgrade, runs the phased
//     migration Jobs, and aborts a reverted upgrade.
//   - ReconcileConnectionSecret reads the ESO-synced credentials Secret,
//     assembles the DSN, materialises the derived <name>-db-connection Secret,
//     and returns the digest that rolls the pods on a credential rotation.
//   - FinalizeResources and HasLiveResources drive the finalizer cleanup of the
//     owned MariaDB CRs.
//
// A CR that owns several database blocks calls the flows once per block with a
// derived instance name (for example <name>-api), so every derived resource,
// Secret, and env var follows the instance-name convention.
//
// The flows are parameterized by the service-specific bits (manage command,
// config mount, image, job naming, condition vocabulary) so keystone, glance,
// and every future oslo/MariaDB service share one implementation rather than
// forking the choreography.
package database
