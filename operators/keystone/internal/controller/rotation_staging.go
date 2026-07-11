// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package controller — staging Secret helpers for the split-compute-write
// rotation architecture. The rotation CronJob PATCHes a dedicated staging
// Secret; the operator reads it, validates, and applies the keys to the
// production Secret using its own privileged ServiceAccount. The mechanics are
// shared via internal/common/rotation; these thin wrappers bind the
// keystone-specific labels, metrics, and event vocabulary.
package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/c5c3/forge/internal/common/rotation"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
	"github.com/c5c3/forge/operators/keystone/internal/metrics"
)

// StagingSecretLabelKey labels staging Secrets so operator watches and
// consumers can distinguish them from the production key Secrets.
const StagingSecretLabelKey = rotation.StagingSecretLabelKey //nolint:gosec // label key, not a credential

// RotationCompletedAnnotation is the RFC3339 UTC timestamp the rotation
// CronJob writes atomically with its staging Secret PATCH. Its presence is
// the single-shot commit marker gating the operator's apply path.
const RotationCompletedAnnotation = rotation.CompletedAnnotation //nolint:gosec // annotation key, not a credential

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
// gauge from the rotation-completed annotation, via the shared rotation.ObserveAge.
// It reads the annotation from the production keys Secret first (durable across
// the steady state) and falls back to the staging Secret for the
// post-CronJob-PATCH/pre-apply window. Both Secrets are passed in so this helper
// performs no client reads (issue #361).
func (r *KeystoneReconciler) observeRotationAge(
	keystone *keystonev1alpha1.Keystone,
	mainSecret, stagingSecret *corev1.Secret,
	keyType string,
) {
	rotation.ObserveAge(mainSecret, stagingSecret, func(completedAt time.Time) error {
		return metrics.SetKeyRotationAge(keystone.Name, keystone.Namespace, keyType, completedAt)
	})
}

// rotationCompletedAt parses the RotationCompletedAnnotation off the given
// Secret via the shared rotation.CompletedAt.
func rotationCompletedAt(secret *corev1.Secret) (time.Time, bool) {
	return rotation.CompletedAt(secret)
}

// ensureStagingSecret creates (or ensures) an empty staging Secret that the
// rotation CronJob PATCHes rotated keys into, via the shared
// rotation.EnsureStagingSecret. labelValue is the per-kind value written to the
// StagingSecretLabelKey label (e.g. "fernet-keys", "credential-keys").
func (r *KeystoneReconciler) ensureStagingSecret(
	ctx context.Context,
	keystone *keystonev1alpha1.Keystone,
	name, labelValue string,
) (*corev1.Secret, error) {
	// Merge commonLabels with the rotation-target marker so operator-owned
	// labels stay authoritative.
	labels := commonLabels(keystone)
	labels[StagingSecretLabelKey] = labelValue
	return rotation.EnsureStagingSecret(ctx, r.Client, r.Scheme, keystone, name, labels)
}

// commitStagedRotation copies a completed staging Secret onto an operator-owned
// target Secret and deletes the staging Secret, via the shared
// rotation.CommitStaged.
func (r *KeystoneReconciler) commitStagedRotation(ctx context.Context, keystone *keystonev1alpha1.Keystone, staging, target *corev1.Secret, spec rotation.CommitSpec) (applied bool, err error) {
	return rotation.CommitStaged(ctx, r.Client, r.Recorder, keystone, staging, target, spec)
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
	staging, mainSecret *corev1.Secret,
	eventReason string,
	minKeys, maxKeys int,
) (applied bool, err error) {
	return r.commitStagedRotation(ctx, keystone, staging, mainSecret, rotation.CommitSpec{
		TargetNoun:              "main secret",
		Validate:                func(data map[string][]byte) error { return validateRotationOutput(data, minKeys, maxKeys) },
		ClearStagingOnReject:    true,
		AnnotationInvalidReason: "RotationAnnotationInvalid",
		RejectedReason:          "RotationRejected",
		AppliedReason:           eventReason,
		AppliedMessage: func(stagingSecretName string, data map[string][]byte) string {
			// Count reflects the number of active keys now in the production
			// Secret (len(staging.Data) == len(main.Data) after replacement).
			return fmt.Sprintf("rotation applied from staging secret %s (%d active keys)", stagingSecretName, len(data))
		},
	})
}
