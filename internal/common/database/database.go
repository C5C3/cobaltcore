// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package database

import (
	"context"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/forge/internal/common/apply"
	"github.com/c5c3/forge/internal/common/conditions"
)

// EnsureDatabase creates a MariaDB Database CR if it does not exist or applies
// its desired state via Server-Side Apply if it already exists. It returns
// (true, nil) when the Database has a Ready condition with status True,
// (false, nil) when it exists but is not yet ready, and (false, error) on
// unexpected failures. Server-defaulted fields the builder omits are not
// claimed, so a converged Database is applied without a write; readiness is
// read from the server-fresh apply response without a re-Get.
func EnsureDatabase(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, db *mariadbv1alpha1.Database) (bool, error) {
	if err := apply.EnsureObject(ctx, c, scheme, owner, db, apply.FieldManager); err != nil {
		return false, err
	}
	return isDatabaseReady(db), nil
}

// isDatabaseReady returns true if the Database has a Ready condition with
// status True.
func isDatabaseReady(db *mariadbv1alpha1.Database) bool {
	return conditions.IsReady(db.Status.Conditions)
}

// EnsureDatabaseUser creates or updates a MariaDB User CR and a Grant CR. It
// returns (true, nil) when both User and Grant have a Ready condition with
// status True, (false, nil) when either is not yet ready, and (false, error)
// on unexpected failures.
func EnsureDatabaseUser(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, user *mariadbv1alpha1.User, grant *mariadbv1alpha1.Grant) (bool, error) {
	userReady, err := ensureUser(ctx, c, scheme, owner, user)
	if err != nil {
		return false, err
	}
	if !userReady {
		// Wait for the MySQL-level user to exist before creating the Grant.
		// The MariaDB operator requires the user to be reconciled into an
		// actual MySQL user before a GRANT statement can succeed.
		return false, nil
	}

	return ensureGrant(ctx, c, scheme, owner, grant)
}

func ensureUser(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, user *mariadbv1alpha1.User) (bool, error) {
	if err := apply.EnsureObject(ctx, c, scheme, owner, user, apply.FieldManager); err != nil {
		return false, err
	}
	return isUserReady(user), nil
}

func ensureGrant(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, grant *mariadbv1alpha1.Grant) (bool, error) {
	if err := apply.EnsureObject(ctx, c, scheme, owner, grant, apply.FieldManager); err != nil {
		return false, err
	}
	return isGrantReady(grant), nil
}

// isUserReady returns true if the User has a Ready condition with status
// True.
func isUserReady(user *mariadbv1alpha1.User) bool {
	return conditions.IsReady(user.Status.Conditions)
}

// isGrantReady returns true if the Grant has a Ready condition with status
// True.
func isGrantReady(grant *mariadbv1alpha1.Grant) bool {
	return conditions.IsReady(grant.Status.Conditions)
}
