// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package job

// Feature: CC-0005

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// RunJob creates or updates a Kubernetes Job and checks whether it has
// completed. The desired parameter must be a fully constructed *batchv1.Job
// including ObjectMeta (Name, Namespace) and Spec. The owner is set as the
// controller reference so that the Job is garbage-collected when the owner is
// deleted.
//
// Returns (true, nil) when the Job has succeeded, (false, nil) when still
// running, and (false, error) on failure.
func RunJob(ctx context.Context, c client.Client, owner client.Object, desired *batchv1.Job) (bool, error) {
	existing := &batchv1.Job{}
	existing.Name = desired.Name
	existing.Namespace = desired.Namespace

	_, err := controllerutil.CreateOrUpdate(ctx, c, existing, func() error {
		if err := controllerutil.SetControllerReference(owner, existing, c.Scheme()); err != nil {
			return fmt.Errorf("setting controller reference: %w", err)
		}
		existing.Spec = desired.Spec
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("creating or updating Job %s/%s: %w", desired.Namespace, desired.Name, err)
	}

	// Re-fetch the Job to get current status after create/update.
	if err := c.Get(ctx, client.ObjectKeyFromObject(existing), existing); err != nil {
		return false, fmt.Errorf("fetching Job status %s/%s: %w", desired.Namespace, desired.Name, err)
	}

	return IsJobComplete(existing), nil
}

// EnsureCronJob creates or updates a Kubernetes CronJob. The desired parameter
// must be a fully constructed *batchv1.CronJob including ObjectMeta and Spec.
// The owner is set as the controller reference so that the CronJob is
// garbage-collected when the owner is deleted.
func EnsureCronJob(ctx context.Context, c client.Client, owner client.Object, desired *batchv1.CronJob) error {
	existing := &batchv1.CronJob{}
	existing.Name = desired.Name
	existing.Namespace = desired.Namespace

	_, err := controllerutil.CreateOrUpdate(ctx, c, existing, func() error {
		if err := controllerutil.SetControllerReference(owner, existing, c.Scheme()); err != nil {
			return fmt.Errorf("setting controller reference: %w", err)
		}
		existing.Spec = desired.Spec
		return nil
	})
	if err != nil {
		return fmt.Errorf("creating or updating CronJob %s/%s: %w", desired.Namespace, desired.Name, err)
	}

	return nil
}

// IsJobComplete returns true if the given Job has completed successfully,
// indicated by at least one succeeded pod.
func IsJobComplete(job *batchv1.Job) bool {
	if job == nil {
		return false
	}
	return job.Status.Succeeded >= 1
}
