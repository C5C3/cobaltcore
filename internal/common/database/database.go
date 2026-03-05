// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package database

// Feature: CC-0005

import (
	"context"
	"fmt"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// EnsureDatabase creates or updates a MariaDB Operator Database CR. The owner
// is set as the controller reference so that the Database is garbage-collected
// when the owner is deleted. Returns (true, nil) when the Database is ready,
// (false, nil) when it is not yet ready, and (false, error) on failure.
func EnsureDatabase(ctx context.Context, c client.Client, owner client.Object, desired *mariadbv1alpha1.Database) (bool, error) {
	existing := &mariadbv1alpha1.Database{
		ObjectMeta: metav1.ObjectMeta{
			Name:      desired.Name,
			Namespace: desired.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, c, existing, func() error {
		existing.Spec = desired.Spec
		return controllerutil.SetControllerReference(owner, existing, c.Scheme())
	})
	if err != nil {
		return false, fmt.Errorf("creating or updating Database %s/%s: %w", desired.Namespace, desired.Name, err)
	}

	// Re-fetch to get current status after create/update.
	if err := c.Get(ctx, client.ObjectKeyFromObject(existing), existing); err != nil {
		return false, fmt.Errorf("fetching Database status %s/%s: %w", desired.Namespace, desired.Name, err)
	}

	return isDatabaseReady(existing), nil
}

// EnsureDatabaseUser creates or updates a MariaDB Operator User CR and its
// associated Grant CR. The owner is set as the controller reference on both
// resources. Returns (true, nil) when both the User and Grant are ready,
// (false, nil) when either is not yet ready, and (false, error) on failure.
func EnsureDatabaseUser(ctx context.Context, c client.Client, owner client.Object, desiredUser *mariadbv1alpha1.User, desiredGrant *mariadbv1alpha1.Grant) (bool, error) {
	// Ensure the User CR.
	existingUser := &mariadbv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{
			Name:      desiredUser.Name,
			Namespace: desiredUser.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, c, existingUser, func() error {
		existingUser.Spec = desiredUser.Spec
		return controllerutil.SetControllerReference(owner, existingUser, c.Scheme())
	})
	if err != nil {
		return false, fmt.Errorf("creating or updating User %s/%s: %w", desiredUser.Namespace, desiredUser.Name, err)
	}

	// Ensure the Grant CR.
	existingGrant := &mariadbv1alpha1.Grant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      desiredGrant.Name,
			Namespace: desiredGrant.Namespace,
		},
	}

	_, err = controllerutil.CreateOrUpdate(ctx, c, existingGrant, func() error {
		existingGrant.Spec = desiredGrant.Spec
		return controllerutil.SetControllerReference(owner, existingGrant, c.Scheme())
	})
	if err != nil {
		return false, fmt.Errorf("creating or updating Grant %s/%s: %w", desiredGrant.Namespace, desiredGrant.Name, err)
	}

	// Re-fetch both to get current status.
	if err := c.Get(ctx, client.ObjectKeyFromObject(existingUser), existingUser); err != nil {
		return false, fmt.Errorf("fetching User status %s/%s: %w", desiredUser.Namespace, desiredUser.Name, err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(existingGrant), existingGrant); err != nil {
		return false, fmt.Errorf("fetching Grant status %s/%s: %w", desiredGrant.Namespace, desiredGrant.Name, err)
	}

	return isUserReady(existingUser) && isGrantReady(existingGrant), nil
}

// RunDBSyncJob creates or updates a Kubernetes Job intended for database schema
// synchronisation (migrations, seed data, etc.). The owner is set as the
// controller reference so that the Job is garbage-collected when the owner is
// deleted. Returns (true, nil) when the Job has succeeded, (false, nil) when
// still running, and (false, error) on failure.
func RunDBSyncJob(ctx context.Context, c client.Client, owner client.Object, desired *batchv1.Job) (bool, error) {
	existing := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      desired.Name,
			Namespace: desired.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, c, existing, func() error {
		existing.Spec = desired.Spec
		return controllerutil.SetControllerReference(owner, existing, c.Scheme())
	})
	if err != nil {
		return false, fmt.Errorf("creating or updating DB sync Job %s/%s: %w", desired.Namespace, desired.Name, err)
	}

	// Re-fetch the Job to get current status after create/update.
	if err := c.Get(ctx, client.ObjectKeyFromObject(existing), existing); err != nil {
		return false, fmt.Errorf("fetching DB sync Job status %s/%s: %w", desired.Namespace, desired.Name, err)
	}

	return existing.Status.Succeeded >= 1, nil
}

// isDatabaseReady returns true if the Database has a Ready=True condition.
func isDatabaseReady(db *mariadbv1alpha1.Database) bool {
	return meta.IsStatusConditionTrue(db.Status.Conditions, "Ready")
}

// isUserReady returns true if the User has a Ready=True condition.
func isUserReady(user *mariadbv1alpha1.User) bool {
	return meta.IsStatusConditionTrue(user.Status.Conditions, "Ready")
}

// isGrantReady returns true if the Grant has a Ready=True condition.
func isGrantReady(grant *mariadbv1alpha1.Grant) bool {
	return meta.IsStatusConditionTrue(grant.Status.Conditions, "Ready")
}
