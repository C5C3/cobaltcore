// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package database

import (
	"context"
	"fmt"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/c5c3/forge/internal/common/conditions"
)

// EnsureDatabase creates a MariaDB Database CR if it does not exist or updates
// its spec if it already exists. It returns (true, nil) when the Database has a
// Ready condition with status True, (false, nil) when it exists but is not yet
// ready, and (false, error) on unexpected failures.
func EnsureDatabase(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, db *mariadbv1alpha1.Database) (bool, error) {
	existing := &mariadbv1alpha1.Database{}
	err := c.Get(ctx, client.ObjectKeyFromObject(db), existing)

	if apierrors.IsNotFound(err) {
		if err := controllerutil.SetControllerReference(owner, db, scheme); err != nil {
			return false, fmt.Errorf("setting owner reference on Database %s/%s: %w", db.Namespace, db.Name, err)
		}
		if err := c.Create(ctx, db); err != nil {
			return false, fmt.Errorf("creating Database %s/%s: %w", db.Namespace, db.Name, err)
		}
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("getting Database %s/%s: %w", db.Namespace, db.Name, err)
	}

	if !apiequality.Semantic.DeepEqual(existing.Spec, db.Spec) {
		existing.Spec = db.Spec
		if err := c.Update(ctx, existing); err != nil {
			return false, fmt.Errorf("updating Database %s/%s: %w", db.Namespace, db.Name, err)
		}
		// Re-fetch to avoid evaluating stale status from before the spec
		// update.
		if err := c.Get(ctx, client.ObjectKeyFromObject(db), existing); err != nil {
			return false, fmt.Errorf("re-fetching Database %s/%s after update: %w", db.Namespace, db.Name, err)
		}
	}

	return isDatabaseReady(existing), nil
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
	existing := &mariadbv1alpha1.User{}
	err := c.Get(ctx, client.ObjectKeyFromObject(user), existing)

	if apierrors.IsNotFound(err) {
		if err := controllerutil.SetControllerReference(owner, user, scheme); err != nil {
			return false, fmt.Errorf("setting owner reference on User %s/%s: %w", user.Namespace, user.Name, err)
		}
		if err := c.Create(ctx, user); err != nil {
			return false, fmt.Errorf("creating User %s/%s: %w", user.Namespace, user.Name, err)
		}
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("getting User %s/%s: %w", user.Namespace, user.Name, err)
	}

	if !apiequality.Semantic.DeepEqual(existing.Spec, user.Spec) {
		existing.Spec = user.Spec
		if err := c.Update(ctx, existing); err != nil {
			return false, fmt.Errorf("updating User %s/%s: %w", user.Namespace, user.Name, err)
		}
		// Re-fetch to avoid evaluating stale status from before the spec
		// update.
		if err := c.Get(ctx, client.ObjectKeyFromObject(user), existing); err != nil {
			return false, fmt.Errorf("re-fetching User %s/%s after update: %w", user.Namespace, user.Name, err)
		}
	}

	return isUserReady(existing), nil
}

func ensureGrant(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, grant *mariadbv1alpha1.Grant) (bool, error) {
	existing := &mariadbv1alpha1.Grant{}
	err := c.Get(ctx, client.ObjectKeyFromObject(grant), existing)

	if apierrors.IsNotFound(err) {
		if err := controllerutil.SetControllerReference(owner, grant, scheme); err != nil {
			return false, fmt.Errorf("setting owner reference on Grant %s/%s: %w", grant.Namespace, grant.Name, err)
		}
		if err := c.Create(ctx, grant); err != nil {
			return false, fmt.Errorf("creating Grant %s/%s: %w", grant.Namespace, grant.Name, err)
		}
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("getting Grant %s/%s: %w", grant.Namespace, grant.Name, err)
	}

	if !apiequality.Semantic.DeepEqual(existing.Spec, grant.Spec) {
		existing.Spec = grant.Spec
		if err := c.Update(ctx, existing); err != nil {
			return false, fmt.Errorf("updating Grant %s/%s: %w", grant.Namespace, grant.Name, err)
		}
		// Re-fetch to avoid evaluating stale status from before the spec
		// update.
		if err := c.Get(ctx, client.ObjectKeyFromObject(grant), existing); err != nil {
			return false, fmt.Errorf("re-fetching Grant %s/%s after update: %w", grant.Namespace, grant.Name, err)
		}
	}

	return isGrantReady(existing), nil
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
