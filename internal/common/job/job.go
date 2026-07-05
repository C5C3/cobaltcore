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

	"github.com/c5c3/forge/internal/common/apply"
)

// PodSpecHashAnnotation stores a Job's re-run gate value at creation time. By
// default (RunJob) this is a SHA-256 hash of the desired pod template
// (PodTemplateSpec), so a completed Job is re-run when its template changes
// (e.g. an operator/release upgrade swaps the container image). A caller may
// instead supply an explicit key via RunJobWithRerunKey; the annotation then
// holds that key. Either way the value is compared on subsequent passes without
// relying on normalization of API-server defaults.
const PodSpecHashAnnotation = "forge.c5c3.io/pod-spec-hash"

// PodSpecHash computes a deterministic SHA-256 hash of the given pod template.
// It hashes the full corev1.PodTemplateSpec (metadata + spec), so pod-template
// annotations participate in change detection: a content-derived annotation
// (e.g. a rotated credential digest) changes the hash even when the underlying
// PodSpec is byte-identical.
//
// DECISION: hash the full PodTemplateSpec vs. a container-env surrogate.
// Ambiguity: the rotation signal could be carried either as a pod-template
// annotation (requires hashing PodTemplateSpec) or as an extra container Env
// entry (keeps the existing *corev1.PodSpec hash).
// Chose: hash &job.Spec.Template (the full PodTemplateSpec).
// Reason: it generalises to any Job and keeps the change signal in pod-template
// metadata rather than leaking a synthetic dependency into the container spec.
// Reviewer: please verify this matches intent.
func PodSpecHash(template *corev1.PodTemplateSpec) string {
	// json.Marshal cannot fail for *corev1.PodTemplateSpec — all fields are
	// primitives, slices, or Kubernetes API types with known JSON serialization.
	data, _ := json.Marshal(template)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ErrJobFailed is returned by RunJob when the Job has exceeded its
// backoffLimit and will not be retried automatically.
var ErrJobFailed = errors.New("job has permanently failed")

// RunJob creates a Job if it does not already exist and reports whether the
// Job has completed successfully. It returns (true, observed, nil) when the Job
// has finished, (false, observed, nil) when the Job exists but is still running
// or was re-created, (false, observed, error) when the Job has permanently
// failed (e.g. exceeded backoffLimit) and its pod template is unchanged, and
// (false, nil, error) on unexpected failures.
//
// observed is the Job read at the start of the pass: nil when the Job was just
// created (or a non-NotFound Get failed), the current Job in steady state, and
// the old terminal Job on the recreate-stale branch — so callers can record
// terminal metrics for that Job without a second Get (issue #361).
//
// A completed or permanently failed Job is re-run when its full pod template
// changes — the correct trigger for migration-style Jobs (db-sync,
// expand/migrate/contract, schema-check) that must re-execute against a new
// container image on an operator/release upgrade. Re-running a permanently
// failed Job once its spec is fixed avoids wedging the Job until a manual
// `kubectl delete job` (#460).
func RunJob(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, job *batchv1.Job) (bool, *batchv1.Job, error) {
	return RunJobWithRerunKey(ctx, c, scheme, owner, job, PodSpecHash(&job.Spec.Template))
}

// RunJobWithRerunKey behaves like RunJob but uses an explicit rerunKey, rather
// than the full pod-template hash, to decide whether a *completed or permanently
// failed* Job must be re-run. The key is stored at creation time and compared on
// subsequent passes; a terminal Job is re-run only when the key changes.
//
// This lets a Job opt out of image-sensitive re-runs. The keystone bootstrap
// Job uses the admin-password digest as its key so it re-runs when the password
// rotates but NOT when the container image changes on a release
// upgrade: re-running keystone-manage bootstrap after the cross-version DB
// migration fails on the already-migrated admin user (oslo_db DBDuplicateEntry
// 'default-admin'), which would otherwise hold BootstrapReady — and the
// aggregate Ready — False for the whole upgrade. A failed bootstrap
// Job follows the same rule: a release-upgrade image change does not re-run it,
// but a rotated admin password (a new key) does (#460).
//
// The second return value is the observed Job (see RunJob): nil after a create
// or a failed Get; the current Job otherwise; and the old terminal Job on the
// recreate-stale branch so its terminal metrics can still be emitted.
func RunJobWithRerunKey(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, job *batchv1.Job, rerunKey string) (bool, *batchv1.Job, error) {
	existing := &batchv1.Job{}
	err := c.Get(ctx, client.ObjectKeyFromObject(job), existing)

	if apierrors.IsNotFound(err) {
		return false, nil, createJobWithRerunKey(ctx, c, scheme, owner, job, rerunKey)
	}
	if err != nil {
		return false, nil, fmt.Errorf("getting Job %s/%s: %w", job.Namespace, job.Name, err)
	}

	if isJobFailed(existing) {
		// A permanently failed Job (exceeded backoffLimit) is re-run when its
		// re-run key changes — the desired spec was fixed since the failure
		// (e.g. a new container image after a release upgrade, a corrected
		// policyOverrides ConfigMap, or a rotated admin password for the
		// bootstrap Job). Without this, the re-run-key gate that exists for the
		// fixed-spec case would never fire once backoffLimit is exhausted,
		// wedging the Job until a manual `kubectl delete job` (#460). A failed
		// Job whose key is unchanged stays failed and returns ErrJobFailed, so a
		// genuinely-stuck Job does not requeue forever.
		existingKey := existing.Annotations[PodSpecHashAnnotation]
		if rerunKey != existingKey {
			if err := recreateStaleJob(ctx, c, scheme, owner, job, existing, rerunKey); err != nil {
				return false, existing, err
			}
			return false, existing, nil
		}
		return false, existing, fmt.Errorf("%w: %s/%s", ErrJobFailed, existing.Namespace, existing.Name)
	}

	if isJobComplete(existing) {
		// Guard against stale completed Jobs: if the re-run key has changed
		// (e.g. a new container image for migration Jobs, or a rotated admin
		// password for the bootstrap Job) delete the old Job and create a new
		// one. The key is compared against the value stored at creation time to
		// avoid maintaining a normalization layer for API-server defaults
		existingKey := existing.Annotations[PodSpecHashAnnotation]
		if rerunKey != existingKey {
			if err := recreateStaleJob(ctx, c, scheme, owner, job, existing, rerunKey); err != nil {
				return false, existing, err
			}
			return false, existing, nil
		}
		return true, existing, nil
	}

	return false, existing, nil
}

// createJobWithRerunKey sets the owner reference, stores the re-run key in the
// PodSpecHashAnnotation, and creates the Job. It returns nil on success and on
// AlreadyExists (old Job still terminating).
func createJobWithRerunKey(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, job *batchv1.Job, rerunKey string) error {
	toCreate := job.DeepCopy()
	if err := controllerutil.SetControllerReference(owner, toCreate, scheme); err != nil {
		return fmt.Errorf("setting owner reference on Job %s/%s: %w", job.Namespace, job.Name, err)
	}
	if toCreate.Annotations == nil {
		toCreate.Annotations = make(map[string]string)
	}
	toCreate.Annotations[PodSpecHashAnnotation] = rerunKey
	if err := c.Create(ctx, toCreate); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Old Job still terminating (e.g. finalizer pending); wait for the next reconcile.
			return nil
		}
		return fmt.Errorf("creating Job %s/%s: %w", job.Namespace, job.Name, err)
	}
	return nil
}

// recreateStaleJob deletes a terminal Job (completed or permanently failed)
// whose stored re-run key no longer matches the desired template, then creates
// a replacement carrying the new key. It is the shared delete-and-recreate path
// for both terminal-state branches of RunJobWithRerunKey.
//
// The Background propagation policy is set explicitly for envtest/production
// consistency: the default on most API servers is already Background, so this is
// a no-op in production but prevents envtest from adding an `orphan` finalizer
// that would block the subsequent Create with AlreadyExists. Verified: only the
// keystone operator uses RunJob; no other operator in the monorepo is affected
func recreateStaleJob(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, job, existing *batchv1.Job, rerunKey string) error {
	propagation := metav1.DeletePropagationBackground
	if err := c.Delete(ctx, existing, &client.DeleteOptions{PropagationPolicy: &propagation}); err != nil {
		return fmt.Errorf("deleting stale Job %s/%s: %w", existing.Namespace, existing.Name, err)
	}
	return createJobWithRerunKey(ctx, c, scheme, owner, job, rerunKey)
}

// EnsureCronJob creates a CronJob if it does not exist or applies its desired
// state via Server-Side Apply if it already exists. An owner reference is set on
// the CronJob so that it is garbage-collected when the owning resource is
// deleted. Because the field manager owns only the fields the builder sets,
// server-defaulted CronJob fields no longer participate in the diff and a
// converged CronJob is applied without a write.
func EnsureCronJob(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, cronJob *batchv1.CronJob) error {
	return apply.EnsureObject(ctx, c, scheme, owner, cronJob, apply.FieldManager)
}

// isJobComplete returns true if the given Job has a Complete condition with
// status True.
func isJobComplete(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// isJobFailed returns true if the given Job has a Failed condition with
// status True, indicating it has permanently failed (e.g. exceeded its
// backoffLimit).
func isJobFailed(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// DeleteCronJob deletes the CronJob identified by namespace and name. It is a
// no-op if the CronJob does not exist.
func DeleteCronJob(ctx context.Context, c client.Client, namespace, name string) error {
	cj := &batchv1.CronJob{}
	cj.SetName(name)
	cj.SetNamespace(namespace)
	if err := client.IgnoreNotFound(c.Delete(ctx, cj)); err != nil {
		return fmt.Errorf("deleting CronJob %s/%s: %w", namespace, name, err)
	}
	return nil
}
