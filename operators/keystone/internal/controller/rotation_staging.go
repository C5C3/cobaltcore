// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package controller — staging Secret helpers for the split-compute-write
// rotation architecture introduced by CC-0081. The rotation CronJob PATCHes
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
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// StagingSecretLabelKey labels staging Secrets so operator watches and
// consumers can distinguish them from the production key Secrets (CC-0081).
const StagingSecretLabelKey = "forge.c5c3.io/rotation-target" //nolint:gosec // label key, not a credential

// RotationCompletedAnnotation is the RFC3339 UTC timestamp the rotation
// CronJob writes atomically with its staging Secret PATCH. Its presence is
// the single-shot commit marker gating the operator's apply path (CC-0081).
const RotationCompletedAnnotation = "forge.c5c3.io/rotation-completed-at" //nolint:gosec // annotation key, not a credential

// fernetStagingSecretName returns the staging Secret name for Fernet key
// rotation: `<keystone>-fernet-keys-rotation` (CC-0081).
func fernetStagingSecretName(keystone *keystonev1alpha1.Keystone) string {
	return fmt.Sprintf("%s-fernet-keys-rotation", keystone.Name)
}

// credentialStagingSecretName returns the staging Secret name for credential
// key rotation: `<keystone>-credential-keys-rotation` (CC-0081).
func credentialStagingSecretName(keystone *keystonev1alpha1.Keystone) string {
	return fmt.Sprintf("%s-credential-keys-rotation", keystone.Name)
}

// ensureStagingSecret creates (or ensures) an empty staging Secret that the
// rotation CronJob PATCHes rotated keys into (CC-0081). The operator owns the
// object's metadata and lifecycle — labels, owner reference — while the
// CronJob owns the `.data` field via a narrow get+patch RBAC grant. Data is
// deliberately left nil on creation and untouched on update so the CronJob's
// PATCH is never clobbered by a reconcile.
//
// labelValue is the per-kind value written to the StagingSecretLabelKey label
// (e.g. "fernet-keys", "credential-keys") so operators can grep label state
// per sub-reconciler.
//
// Note on CronJob/operator PATCH-vs-Update race (CC-0081): controllerutil.
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
		// call so operator-owned labels stay authoritative (CC-0081).
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

// applyRotationOutput copies a completed staging Secret onto the main keys
// Secret and deletes the staging Secret. It is the operator-side commit for
// the split-compute-write rotation architecture (CC-0081, REQ-005, REQ-006,
// REQ-012):
//
//  1. GET the staging Secret. If absent, return (false, nil) — nothing to do.
//  2. Require RotationCompletedAnnotation to be present and parseable as
//     RFC3339; a malformed annotation emits a Warning event and is retried on
//     the next CronJob run (no requeue-with-error).
//  3. Validate the Secret's Data via validateRotationOutput; a rejection
//     emits a Warning event and retains the staging Secret for operator
//     inspection.
//  4. GET the main Secret, replace its Data with the staging payload verbatim,
//     and Update — full-object replacement guarantees the atomic-swap
//     semantics (stale indices not in the staging payload are removed).
//  5. DELETE the staging Secret (tolerate NotFound for races).
//  6. Emit a Normal event with the given eventReason.
//
// DECISION (CC-0081): UPDATE-then-DELETE ordering — if DELETE fails, the
// production Secret is already updated; a subsequent reconcile will no-op on
// the next CronJob PATCH because the annotation flips to a new timestamp
// (and a stale, pre-this-run annotation would fail validation the same way
// on retry). Tolerating NotFound on DELETE handles the common race where a
// human operator removed the staging Secret by hand.
//
// DECISION (CC-0081): The production Secret's `.data` field is fully
// replaced under the controller-owned ResourceVersion via a GET+Update
// round-trip. A strategic-merge PATCH would merge map entries by key rather
// than replace the map (corev1.Secret.Data has no patchStrategy tag), which
// would allow stale key indices — e.g. those trimmed by a
// max_active_keys reduction or renumbered by keystone-manage fernet_rotate —
// to accumulate indefinitely on the production Secret. Full replacement is
// the only semantic that realises the intended atomic swap.
func (r *KeystoneReconciler) applyRotationOutput(
	ctx context.Context,
	keystone *keystonev1alpha1.Keystone,
	stagingSecretName string,
	mainSecretName string,
	eventReason string,
	minKeys, maxKeys int,
) (applied bool, err error) {
	// 1. GET staging Secret.
	var staging corev1.Secret
	if getErr := r.Get(ctx, types.NamespacedName{
		Name:      stagingSecretName,
		Namespace: keystone.Namespace,
	}, &staging); getErr != nil {
		if apierrors.IsNotFound(getErr) {
			return false, nil
		}
		return false, fmt.Errorf("getting staging secret %s: %w", stagingSecretName, getErr)
	}

	// 2. Require RotationCompletedAnnotation present and well-formed RFC3339.
	completedAt := staging.Annotations[RotationCompletedAnnotation]
	if completedAt == "" {
		// Distinguish "CronJob hasn't run yet" (empty staging.Data — expected
		// steady state between rotations) from "CronJob wrote Data but forgot
		// to annotate" (non-empty Data — likely a script bug). Logged at V(1)
		// so normal operation is not spammed (CC-0081).
		if len(staging.Data) > 0 {
			log.FromContext(ctx).V(1).Info(
				"staging secret has data without completion annotation; "+
					"skipping apply until next CronJob run writes the annotation",
				"staging", stagingSecretName,
				"annotation", RotationCompletedAnnotation,
				"dataKeys", len(staging.Data),
			)
		}
		return false, nil
	}
	if _, parseErr := time.Parse(time.RFC3339, completedAt); parseErr != nil {
		r.Recorder.Eventf(keystone, corev1.EventTypeWarning, "RotationAnnotationInvalid",
			"staging secret %s has malformed %s annotation: %v",
			stagingSecretName, RotationCompletedAnnotation, parseErr)
		return false, nil
	}

	// 3. Validate staging payload.
	if valErr := validateRotationOutput(staging.Data, minKeys, maxKeys); valErr != nil {
		r.Recorder.Eventf(keystone, corev1.EventTypeWarning, "RotationRejected",
			"staging secret %s rejected: %v", stagingSecretName, valErr)
		return false, nil
	}

	// 4. GET main Secret, replace Data verbatim, Update. Full replacement is
	//    required — see the DECISION comment above.
	var mainSecret corev1.Secret
	if getErr := r.Get(ctx, types.NamespacedName{
		Name:      mainSecretName,
		Namespace: keystone.Namespace,
	}, &mainSecret); getErr != nil {
		return false, fmt.Errorf("getting main secret %s: %w", mainSecretName, getErr)
	}
	mainSecret.Data = staging.Data
	if updateErr := r.Update(ctx, &mainSecret); updateErr != nil {
		return false, fmt.Errorf("updating main secret %s: %w", mainSecretName, updateErr)
	}

	// 5. DELETE staging Secret; tolerate NotFound for races.
	if delErr := r.Delete(ctx, &staging); delErr != nil && !apierrors.IsNotFound(delErr) {
		return false, fmt.Errorf("deleting staging secret %s: %w", stagingSecretName, delErr)
	}

	// 6. Emit a success event. Count reflects the number of active keys now
	//    in the production Secret (len(staging.Data) == len(mainSecret.Data)
	//    after the full-replacement Update).
	r.Recorder.Eventf(keystone, corev1.EventTypeNormal, eventReason,
		"rotation applied from staging secret %s (%d active keys)", stagingSecretName, len(staging.Data))

	return true, nil
}
