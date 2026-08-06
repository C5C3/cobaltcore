// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import "time"

// Barbican-specific requeue intervals that no other operator shares.
const (
	// RequeueDatabaseWait is the interval for waiting on MariaDB CR readiness and
	// db-sync Job completion before the database schema can be provisioned, and
	// for holding the finalizer while the MariaDB CRs tear down. These operations
	// are moderately slow, so a longer interval avoids unnecessary API churn.
	RequeueDatabaseWait = 30 * time.Second

	// RequeueSecretStoreRetry is the interval every waiting and failure state of
	// the BarbicanSecretStore controller retries at: a missing OpenBaoCluster, an
	// instance that is not Available yet, an unreachable or denying OpenBao
	// server, and credentials that have not been provided yet. None of these
	// states reaches the workqueue as an error, so this interval — not the
	// exponential backoff — is what paces the retries.
	RequeueSecretStoreRetry = 30 * time.Second

	// RequeueSecretStoreRevalidate is the cadence at which a store re-validates
	// its credentials against the server they authenticate to: every pass for a
	// brownfield store, and for a managed store whose secret ID carries no TTL
	// and therefore has no re-mint timer to ride on. Nothing in the cluster
	// changes when a secret ID expires or an administrator revokes the AppRole
	// outside this operator, so without a timed revalidation the first symptom
	// would be barbican answering 5xx. The interval is long enough to be cheap
	// per store and short enough that the CredentialsReady condition carries the
	// gap before an operator has to read barbican's logs.
	RequeueSecretStoreRevalidate = 15 * time.Minute
)
