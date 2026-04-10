// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package job

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// PodSpecHashAnnotation stores a SHA-256 hash of the desired PodSpec at
// creation time. It is used to detect whether a completed Job's spec has
// changed (e.g. operator upgrade) without relying on normalization of
// API-server defaults (CC-0005).
const PodSpecHashAnnotation = "forge.c5c3.io/pod-spec-hash"

// PodSpecHash computes a deterministic SHA-256 hash of the given PodSpec.
func PodSpecHash(spec *corev1.PodSpec) string {
	// json.Marshal cannot fail for *corev1.PodSpec — all fields are
	// primitives, slices, or Kubernetes API types with known JSON serialization.
	data, _ := json.Marshal(spec)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Feature: CC-0005

// ErrJobFailed is returned by RunJob when the Job has exceeded its
// backoffLimit and will not be retried automatically (CC-0005).
var ErrJobFailed = errors.New("job has permanently failed")

// RunJob creates a Job if it does not already exist and reports whether the
// Job has completed successfully. It returns (true, nil) when the Job has
// finished, (false, nil) when the Job exists but is still running,
// (false, error) when the Job has permanently failed (e.g. exceeded
// backoffLimit), and (false, error) on unexpected failures (CC-0005).
func RunJob(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, job *batchv1.Job) (bool, error) {
	existing := &batchv1.Job{}
	err := c.Get(ctx, client.ObjectKeyFromObject(job), existing)

	if apierrors.IsNotFound(err) {
		return false, createJobWithHash(ctx, c, scheme, owner, job)
	}
	if err != nil {
		return false, fmt.Errorf("getting Job %s/%s: %w", job.Namespace, job.Name, err)
	}

	if IsJobFailed(existing) {
		return false, fmt.Errorf("%w: %s/%s", ErrJobFailed, existing.Namespace, existing.Name)
	}

	if IsJobComplete(existing) {
		// Guard against stale completed Jobs: if the pod spec has changed
		// (e.g. operator upgrade with a new container image), delete the
		// old Job and create a new one so the updated migration runs.
		// Comparison uses a hash annotation stored at creation time to
		// avoid maintaining a normalization layer for API-server defaults
		// (CC-0005).
		desiredHash := PodSpecHash(&job.Spec.Template.Spec)
		existingHash := existing.Annotations[PodSpecHashAnnotation]
		if desiredHash != existingHash {
			// Intentional behavioral alignment: explicitly set Background propagation
			// policy for both envtest and production consistency. The default on most
			// API servers is already Background, so this is a no-op in production but
			// prevents envtest from adding an `orphan` finalizer that would block the
			// subsequent Create with AlreadyExists. Verified: only the keystone
			// operator uses RunJob; no other operator in the monorepo is affected
			// (CC-0056).
			propagation := metav1.DeletePropagationBackground
			if err := c.Delete(ctx, existing, &client.DeleteOptions{PropagationPolicy: &propagation}); err != nil {
				return false, fmt.Errorf("deleting stale Job %s/%s: %w", existing.Namespace, existing.Name, err)
			}
			if err := createJobWithHash(ctx, c, scheme, owner, job); err != nil {
				return false, err
			}
			return false, nil
		}
		return true, nil
	}

	return false, nil
}

// createJobWithHash sets the owner reference, stores a pod-spec hash
// annotation, and creates the Job. It returns nil on success and on
// AlreadyExists (old Job still terminating) (CC-0005).
func createJobWithHash(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, job *batchv1.Job) error {
	toCreate := job.DeepCopy()
	if err := controllerutil.SetControllerReference(owner, toCreate, scheme); err != nil {
		return fmt.Errorf("setting owner reference on Job %s/%s: %w", job.Namespace, job.Name, err)
	}
	if toCreate.Annotations == nil {
		toCreate.Annotations = make(map[string]string)
	}
	toCreate.Annotations[PodSpecHashAnnotation] = PodSpecHash(&job.Spec.Template.Spec)
	if err := c.Create(ctx, toCreate); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Old Job still terminating (e.g. finalizer pending); wait for the next reconcile.
			return nil
		}
		return fmt.Errorf("creating Job %s/%s: %w", job.Namespace, job.Name, err)
	}
	return nil
}

// EnsureCronJob creates a CronJob if it does not exist or updates its spec if
// it already exists. An owner reference is set on the CronJob so that it is
// garbage-collected when the owning resource is deleted (CC-0005).
func EnsureCronJob(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, cronJob *batchv1.CronJob) error {
	existing := &batchv1.CronJob{}
	err := c.Get(ctx, client.ObjectKeyFromObject(cronJob), existing)

	if apierrors.IsNotFound(err) {
		if err := controllerutil.SetControllerReference(owner, cronJob, scheme); err != nil {
			return fmt.Errorf("setting owner reference on CronJob %s/%s: %w", cronJob.Namespace, cronJob.Name, err)
		}
		if err := c.Create(ctx, cronJob); err != nil {
			return fmt.Errorf("creating CronJob %s/%s: %w", cronJob.Namespace, cronJob.Name, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("getting CronJob %s/%s: %w", cronJob.Namespace, cronJob.Name, err)
	}

	// Always update the spec to the desired state. This avoids maintaining
	// a normalization layer to replicate API-server defaulting logic
	// (CC-0005).
	existing.Spec = cronJob.Spec
	if err := c.Update(ctx, existing); err != nil {
		return fmt.Errorf("updating CronJob %s/%s: %w", cronJob.Namespace, cronJob.Name, err)
	}
	return nil
}

// IsJobComplete returns true if the given Job has a Complete condition with
// status True (CC-0005).
func IsJobComplete(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// IsJobFailed returns true if the given Job has a Failed condition with
// status True, indicating it has permanently failed (e.g. exceeded its
// backoffLimit) (CC-0005).
func IsJobFailed(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}
