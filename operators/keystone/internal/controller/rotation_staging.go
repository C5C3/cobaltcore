// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package controller — staging Secret helpers for the split-compute-write
// rotation architecture. The rotation CronJob PATCHes
// a dedicated staging Secret; the operator reads it, validates, and applies
// the keys to the production Secret using its own privileged ServiceAccount.
package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
	"github.com/c5c3/forge/operators/keystone/internal/metrics"
)

// StagingSecretLabelKey labels staging Secrets so operator watches and
// consumers can distinguish them from the production key Secrets.
const StagingSecretLabelKey = "forge.c5c3.io/rotation-target" //nolint:gosec // label key, not a credential

// RotationCompletedAnnotation is the RFC3339 UTC timestamp the rotation
// CronJob writes atomically with its staging Secret PATCH. Its presence is
// the single-shot commit marker gating the operator's apply path.
const RotationCompletedAnnotation = "forge.c5c3.io/rotation-completed-at" //nolint:gosec // annotation key, not a credential

// fernetStagingSecretName returns the staging Secret name for Fernet key
// rotation: `<keystone>-fernet-keys-rotation`.
func fernetStagingSecretName(keystone *keystonev1alpha1.Keystone) string {
	return fmt.Sprintf("%s-fernet-keys-rotation", keystone.Name)
}

// credentialStagingSecretName returns the staging Secret name for credential
// key rotation: `<keystone>-credential-keys-rotation`.
func credentialStagingSecretName(keystone *keystonev1alpha1.Keystone) string {
	return fmt.Sprintf("%s-credential-keys-rotation", keystone.Name)
}

// observeRotationAge refreshes the keystone_operator_key_rotation_age_seconds
// gauge from the rotation-completed annotation. It reads
// the annotation from the production keys Secret first — applyRotationOutput
// stamps it there on every successful apply, so the annotation is durable
// across the inter-rotation steady state and the gauge value (computed as
// time.Since(completedAt) on every reconcile) tracks wall-clock time
// correctly. If the production Secret has no annotation (the very-first
// rotation has not yet been applied) the helper falls back to the staging
// Secret to cover the post-CronJob-PATCH/pre-apply window.
//
// The gauge is a best-effort observability signal: any error reading either
// Secret, an absent annotation on both, or a malformed timestamp is silently
// skipped rather than surfaced as a reconcile failure. PromQL absent(...)
// remains the alerting signal for "rotation has never completed".
func (r *KeystoneReconciler) observeRotationAge(
	ctx context.Context,
	keystone *keystonev1alpha1.Keystone,
	mainSecretName, stagingSecretName, keyType string,
) {
	if completedAt, ok := r.readRotationCompletedAt(ctx, mainSecretName, keystone.Namespace); ok {
		_ = metrics.SetKeyRotationAge(keystone.Name, keystone.Namespace, keyType, completedAt)
		return
	}
	if completedAt, ok := r.readRotationCompletedAt(ctx, stagingSecretName, keystone.Namespace); ok {
		_ = metrics.SetKeyRotationAge(keystone.Name, keystone.Namespace, keyType, completedAt)
	}
}

// readRotationCompletedAt fetches the named Secret and parses the
// RotationCompletedAnnotation. It returns (zero, false) if the Secret is
// absent, the annotation is missing, or the timestamp does not parse as
// RFC3339 — all of which are valid steady-state observations for the gauge
// caller.
func (r *KeystoneReconciler) readRotationCompletedAt(
	ctx context.Context,
	name, namespace string,
) (time.Time, bool) {
	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, &secret); err != nil {
		return time.Time{}, false
	}
	completedAt := secret.Annotations[RotationCompletedAnnotation]
	if completedAt == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339, completedAt)
	if err != nil {
		return time.Time{}, false
	}
	return parsed, true
}

// ensureStagingSecret creates (or ensures) an empty staging Secret that the
// rotation CronJob PATCHes rotated keys into. The operator owns the
// object's metadata and lifecycle — labels, owner reference — while the
// CronJob owns the `.data` field via a narrow get+patch RBAC grant. Data is
// deliberately left nil on creation and untouched on update so the CronJob's
// PATCH is never clobbered by a reconcile.
//
// labelValue is the per-kind value written to the StagingSecretLabelKey label
// (e.g. "fernet-keys", "credential-keys") so operators can grep label state
// per sub-reconciler.
//
// Note on CronJob/operator PATCH-vs-Update race controllerutil.
// CreateOrUpdate runs Get → mutate → Update. If the CronJob PATCHes the
// staging Secret's `.data` between this function's Get and Update, the
// Update carries a stale ResourceVersion and the API server rejects it with
// 409 Conflict. The reconciler requeues; the next invocation's Get observes
// the CronJob's Data and — because the mutator above does not touch
// `secret.Data` — the CronJob's payload is preserved through the subsequent
// Update. Net effect: the CronJob's rotation output is never lost, but
// transient error-requeue log lines are expected during rotation windows
// and are not a bug.
func (r *KeystoneReconciler) ensureStagingSecret(
	ctx context.Context,
	keystone *keystonev1alpha1.Keystone,
	name, labelValue string,
) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: keystone.Namespace,
		},
	}

	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		// Merge commonLabels with the rotation-target marker. Rebuilt every
		// call so operator-owned labels stay authoritative.
		labels := commonLabels(keystone)
		labels[StagingSecretLabelKey] = labelValue
		secret.Labels = labels

		// Do NOT touch secret.Data — the CronJob's PATCH owns that field.
		return controllerutil.SetControllerReference(keystone, secret, r.Scheme)
	}); err != nil {
		return fmt.Errorf("ensuring staging secret %s: %w", name, err)
	}

	return nil
}

// rotationCommitSpec parameterises commitStagedRotation for one rotation
// flavour (Fernet keys, credential keys, or the Model B admin password). Every
// difference between the flavours is captured here; the commit sequence itself
// is identical.
type rotationCommitSpec struct {
	// stagingSecretName is the CronJob-written staging Secret to commit from;
	// targetSecretName is the operator-owned Secret committed into.
	stagingSecretName string
	targetSecretName  string
	// targetNoun names the target Secret in returned error messages
	// ("main secret" for keys, "push-source secret" for the admin password).
	targetNoun string
	// validate enforces the per-flavour staging-payload contract; a non-nil
	// error rejects the commit and retains the staging Secret for inspection.
	validate func(data map[string][]byte) error
	// clearStagingOnReject, when true, wipes the staged .data and the
	// completion annotation when validate rejects the payload, so the next
	// CronJob strategic-merge PATCH starts from an empty base rather than
	// accumulating leftover key indices over the rejected payload. The
	// Fernet/credential key flavours set this because their .data is a
	// multi-key map; the admin-password flavour leaves it false because its
	// single `password` key cannot accumulate stale indices and the rejected
	// value is retained verbatim for operator inspection (issue #475).
	clearStagingOnReject bool
	// annotationInvalidReason / rejectedReason are the Warning event reasons
	// emitted when the completion annotation is malformed or the payload is
	// rejected; the commit is retried on the next CronJob run.
	annotationInvalidReason string
	rejectedReason          string
	// appliedReason / appliedMessage build the Normal event emitted on a
	// successful commit. appliedMessage receives the staging Secret name and
	// the committed data so flavours can vary the wording (e.g. the key
	// flavours append the active-key count; the password flavour does not).
	appliedReason  string
	appliedMessage func(stagingSecretName string, data map[string][]byte) string
}

// commitStagedRotation copies a completed staging Secret onto an operator-owned
// target Secret and deletes the staging Secret. It is the operator-side commit
// for the split-compute-write rotation architecture, shared by every rotation
// flavour via rotationCommitSpec:
//
//  1. GET the staging Secret. If absent, return (false, nil) — nothing to do.
//  2. Require RotationCompletedAnnotation to be present and parseable as
//     RFC3339; a malformed annotation emits spec.annotationInvalidReason and is
//     retried on the next CronJob run (no requeue-with-error).
//  3. Validate the Secret's Data via spec.validate; a rejection emits
//     spec.rejectedReason. When spec.clearStagingOnReject is set the staged
//     Data and completion annotation are cleared (the staging Secret object is
//     kept) so the next CronJob strategic-merge PATCH starts from an empty base
//     rather than accumulating leftover key indices over a rejected payload;
//     otherwise the staging Secret is retained verbatim for operator
//     inspection.
//  4. GET the target Secret, replace its Data with the staging payload
//     verbatim, stamp the annotation, and Update — full-object replacement
//     guarantees the atomic-swap semantics (stale indices not in the staging
//     payload are removed).
//  5. DELETE the staging Secret under client.Preconditions{UID,
//     ResourceVersion} from the step-1 read; tolerate NotFound and Conflict.
//  6. Emit a Normal event built from spec.appliedReason / spec.appliedMessage.
//
// DECISION: UPDATE-then-DELETE ordering — if DELETE fails, the target Secret is
// already updated; a subsequent reconcile will no-op on the next CronJob PATCH
// because the annotation flips to a new timestamp (and a stale, pre-this-run
// annotation would fail validation the same way on retry). Tolerating NotFound
// on DELETE handles the common race where a human operator removed the staging
// Secret by hand.
//
// DECISION: the step-5 DELETE carries client.Preconditions with the UID and
// ResourceVersion read in step 1. An unconditional DELETE would silently remove
// a staging Secret the CronJob had PATCHed with fresh rotation output between
// the step-1 Get and the DELETE, losing that output uncommitted. With the
// preconditions the API server returns 409 Conflict instead; the newer payload
// survives on the staging Secret and commits on the next reconcile. Both
// Conflict (concurrent PATCH) and NotFound (concurrent delete) are therefore
// tolerated — this run's payload is already on the target Secret, so the apply
// is complete either way.
//
// DECISION: The target Secret's `.data` field is fully replaced under the
// controller-owned ResourceVersion via a GET+Update round-trip. A
// strategic-merge PATCH would merge map entries by key rather than replace the
// map (corev1.Secret.Data has no patchStrategy tag), which would allow stale
// key indices — e.g. those trimmed by a max_active_keys reduction or renumbered
// by keystone-manage fernet_rotate — to accumulate indefinitely. Full
// replacement is the only semantic that realises the intended atomic swap.
//
// Persisting the annotation on the target Secret is also what makes the
// keystone_operator_key_rotation_age_seconds gauge refresh on every reconcile:
// the staging Secret is deleted on step 5, so the target Secret is the only
// durable record of the last successful rotation timestamp.
func (r *KeystoneReconciler) commitStagedRotation(ctx context.Context, keystone *keystonev1alpha1.Keystone, spec rotationCommitSpec) (applied bool, err error) {
	// 1. GET staging Secret.
	var staging corev1.Secret
	if getErr := r.Get(ctx, types.NamespacedName{
		Name:      spec.stagingSecretName,
		Namespace: keystone.Namespace,
	}, &staging); getErr != nil {
		if apierrors.IsNotFound(getErr) {
			return false, nil
		}
		return false, fmt.Errorf("getting staging secret %s: %w", spec.stagingSecretName, getErr)
	}

	// 2. Require RotationCompletedAnnotation present and well-formed RFC3339.
	completedAt := staging.Annotations[RotationCompletedAnnotation]
	if completedAt == "" {
		// Distinguish "CronJob hasn't run yet" (empty staging.Data — expected
		// steady state between rotations) from "CronJob wrote Data but forgot
		// to annotate" (non-empty Data — likely a script bug). Logged at V(1)
		// so normal operation is not spammed.
		if len(staging.Data) > 0 {
			log.FromContext(ctx).V(1).Info(
				"staging secret has data without completion annotation; "+
					"skipping apply until next CronJob run writes the annotation",
				"staging", spec.stagingSecretName,
				"annotation", RotationCompletedAnnotation,
				"dataKeys", len(staging.Data),
			)
		}
		return false, nil
	}
	if _, parseErr := time.Parse(time.RFC3339, completedAt); parseErr != nil {
		r.Recorder.Eventf(keystone, corev1.EventTypeWarning, spec.annotationInvalidReason,
			"staging secret %s has malformed %s annotation: %v",
			spec.stagingSecretName, RotationCompletedAnnotation, parseErr)
		return false, nil
	}

	// 3. Validate staging payload. On rejection, emit the Warning event; when
	//    spec.clearStagingOnReject is set, clear the staged Data and completion
	//    annotation before returning. The key-rotation scripts PATCH the staging
	//    Secret with strategic-merge semantics over a multi-key `.data` map
	//    (scripts/fernet_rotate.sh, credential_rotate.sh), so leftover key
	//    indices from a rejected payload would otherwise survive and merge under
	//    the next CronJob run — letting the validator accept a key set
	//    keystone-manage never produced together. Clearing makes the next PATCH
	//    start from an empty base. The rejection event message (not the
	//    now-empty staging Data) is the diagnostic of record (issue #475).
	if valErr := spec.validate(staging.Data); valErr != nil {
		r.Recorder.Eventf(keystone, corev1.EventTypeWarning, spec.rejectedReason,
			"staging secret %s rejected: %v", spec.stagingSecretName, valErr)
		if spec.clearStagingOnReject {
			staging.Data = nil
			delete(staging.Annotations, RotationCompletedAnnotation)
			if updErr := r.Update(ctx, &staging); updErr != nil {
				if !apierrors.IsConflict(updErr) && !apierrors.IsNotFound(updErr) {
					return false, fmt.Errorf("clearing rejected staging secret %s: %w", spec.stagingSecretName, updErr)
				}
				// A concurrent CronJob PATCH (Conflict) or a manual delete
				// (NotFound) raced the clear; the next reconcile re-reads and
				// re-validates, so the stale base cannot survive (issue #475).
				log.FromContext(ctx).V(1).Info(
					"rejected staging secret changed since read; clear retried next reconcile",
					"staging", spec.stagingSecretName,
				)
			}
		}
		return false, nil
	}

	// 4. GET target Secret, replace Data verbatim, stamp the rotation-completed
	//    annotation, and Update. Full Data replacement is required — see the
	//    DECISION comment above.
	var target corev1.Secret
	if getErr := r.Get(ctx, types.NamespacedName{
		Name:      spec.targetSecretName,
		Namespace: keystone.Namespace,
	}, &target); getErr != nil {
		return false, fmt.Errorf("getting %s %s: %w", spec.targetNoun, spec.targetSecretName, getErr)
	}
	target.Data = staging.Data
	if target.Annotations == nil {
		target.Annotations = map[string]string{}
	}
	target.Annotations[RotationCompletedAnnotation] = completedAt
	if updateErr := r.Update(ctx, &target); updateErr != nil {
		return false, fmt.Errorf("updating %s %s: %w", spec.targetNoun, spec.targetSecretName, updateErr)
	}

	// 5. DELETE staging Secret under the UID + ResourceVersion observed at the
	//    step-1 Get. The preconditions make a concurrent CronJob PATCH (fresh
	//    rotation output written between the read and this DELETE) surface as
	//    409 Conflict rather than being silently deleted uncommitted; the newer
	//    payload then commits on the next reconcile. Conflict and NotFound are
	//    both tolerated — this run's payload is already on the target Secret.
	delOpts := client.Preconditions{UID: &staging.UID, ResourceVersion: &staging.ResourceVersion}
	if delErr := r.Delete(ctx, &staging, delOpts); delErr != nil {
		if !apierrors.IsNotFound(delErr) && !apierrors.IsConflict(delErr) {
			return false, fmt.Errorf("deleting staging secret %s: %w", spec.stagingSecretName, delErr)
		}
		log.FromContext(ctx).V(1).Info(
			"staging secret changed since read; newer rotation output applied next reconcile",
			"staging", spec.stagingSecretName,
		)
	}

	// 6. Emit a success event.
	r.Recorder.Event(keystone, corev1.EventTypeNormal, spec.appliedReason,
		spec.appliedMessage(spec.stagingSecretName, staging.Data))

	return true, nil
}

// applyRotationOutput commits a completed Fernet/credential key rotation from
// the staging Secret onto the main keys Secret via commitStagedRotation. The
// staged key set is validated against [minKeys, maxKeys] and the success event
// reports the resulting active-key count. A rejected payload clears the staging
// Data so a stale key set cannot survive as a strategic-merge base for the next
// CronJob run (issue #475).
func (r *KeystoneReconciler) applyRotationOutput(
	ctx context.Context,
	keystone *keystonev1alpha1.Keystone,
	stagingSecretName string,
	mainSecretName string,
	eventReason string,
	minKeys, maxKeys int,
) (applied bool, err error) {
	return r.commitStagedRotation(ctx, keystone, rotationCommitSpec{
		stagingSecretName:       stagingSecretName,
		targetSecretName:        mainSecretName,
		targetNoun:              "main secret",
		validate:                func(data map[string][]byte) error { return validateRotationOutput(data, minKeys, maxKeys) },
		clearStagingOnReject:    true,
		annotationInvalidReason: "RotationAnnotationInvalid",
		rejectedReason:          "RotationRejected",
		appliedReason:           eventReason,
		appliedMessage: func(stagingSecretName string, data map[string][]byte) string {
			// Count reflects the number of active keys now in the production
			// Secret (len(staging.Data) == len(main.Data) after replacement).
			return fmt.Sprintf("rotation applied from staging secret %s (%d active keys)", stagingSecretName, len(data))
		},
	})
}
