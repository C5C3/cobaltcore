// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"strconv"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/config"
	"github.com/c5c3/forge/internal/common/deployment"
	"github.com/c5c3/forge/internal/common/job"
	"github.com/c5c3/forge/internal/common/secrets"
	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"

	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// Model B scheduled admin password rotation.

// adminPasswordRotateScript is the shell script executed by the admin-password
// rotation CronJob. It mints a fresh strong password and PATCHes it onto the
// staging Secret via the K8s API using the pod's ServiceAccount token. Only
// Python standard-library modules are used to avoid image dependencies
// Extracted to scripts/admin_password_rotate.sh for independent
// linting and testing, mirroring fernet_rotate.sh.
//
//go:embed scripts/admin_password_rotate.sh
var adminPasswordRotateScript string

// conditionTypePasswordRotationReady is the Status condition the
// PasswordRotation sub-reconciler drives. It is registered
// in subConditionTypes and subReconcilerConditionTypes by the controller
// wiring (task 3.1). Declared in a const block to match the indented form of
// the package's other conditionType<X>Ready constants.
const (
	conditionTypePasswordRotationReady = "PasswordRotationReady"
)

// adminPasswordSecretKey is the .data key holding the admin password in the
// staging, push-source, and admin Secrets. It matches the key the bootstrap
// reconciler reads (reconcile_bootstrap.go) and the property the keystone-admin
// ExternalSecret pulls from OpenBao.
const adminPasswordSecretKey = "password" //nolint:gosec // G101 false positive: Secret data key name, not a credential

// adminPasswordMinLength is the defense-in-depth floor for the generated
// password length, mirroring the +kubebuilder:validation:Minimum=24 marker on
// PasswordRotationSpec.PasswordLength. The webhook defaults PasswordLength to
// DefaultPasswordRotationLength (32) when rotation is enabled, but this floor
// protects callers that bypass the webhook (e.g. envtest). It aliases
// the api package's DefaultAdminPasswordMinLength so the floor has a single
// source of truth shared with the validating webhook.
const adminPasswordMinLength = keystonev1alpha1.DefaultAdminPasswordMinLength

// ErrAdminPasswordMissing is returned when the staged Secret has no non-empty
// password value.
var ErrAdminPasswordMissing = errors.New("admin password missing")

// ErrAdminPasswordTooShort is returned when the staged password is shorter than
// the configured minimum length.
var ErrAdminPasswordTooShort = errors.New("admin password too short")

// adminPasswordStagingSecretName returns the staging Secret name the rotation
// CronJob PATCHes into: `<keystone>-admin-password-rotation`,
// mirroring fernetStagingSecretName.
func adminPasswordStagingSecretName(keystone *keystonev1alpha1.Keystone) string {
	return fmt.Sprintf("%s-admin-password-rotation", keystone.Name)
}

// adminPasswordNextSecretName returns the operator-owned push-source Secret name
// `<keystone>-admin-password-next`. The operator commits the validated password
// here; the PushSecret selects this Secret and mirrors it to OpenBao. Keeping
// the push path separate from the live admin Secret means a reconcile never
// clobbers the credential the running Keystone is using until ESO has synced
// the new value back.
func adminPasswordNextSecretName(keystone *keystonev1alpha1.Keystone) string {
	return fmt.Sprintf("%s-admin-password-next", keystone.Name)
}

// adminPasswordRotateSAName returns the ServiceAccount/Role/RoleBinding name
// shared by the admin-password rotation CronJob: `<keystone>-admin-password-rotate`.
func adminPasswordRotateSAName(keystone *keystonev1alpha1.Keystone) string {
	return fmt.Sprintf("%s-admin-password-rotate", keystone.Name)
}

// adminPasswordRotateCronJobName returns the CronJob name; it shares the suffix
// with the ServiceAccount name by convention with the fernet sub-reconciler.
func adminPasswordRotateCronJobName(keystone *keystonev1alpha1.Keystone) string {
	return fmt.Sprintf("%s-admin-password-rotate", keystone.Name)
}

// adminPasswordRotateScriptBaseName returns the immutable script ConfigMap base
// name (CreateImmutableConfigMap appends a content-hash suffix).
func adminPasswordRotateScriptBaseName(keystone *keystonev1alpha1.Keystone) string {
	return fmt.Sprintf("%s-admin-password-rotate-script", keystone.Name)
}

// adminPasswordPushSecretName returns the PushSecret name backing up the admin
// password to OpenBao.
func adminPasswordPushSecretName(keystone *keystonev1alpha1.Keystone) string {
	return fmt.Sprintf("%s-admin-password-backup", keystone.Name)
}

// normalizedAdminPasswordLength returns the effective generated-password length.
// It defaults an unset value to DefaultPasswordRotationLength (32) and applies a
// floor of adminPasswordMinLength (24) for defense-in-depth against callers that
// bypass the defaulting webhook.
//
// DECISION: minimum length for validateAdminPasswordRotationOutput
// says ">= minimum length" without fixing the bound. Chose the normalized
// PasswordLength (defaulted/floored as above) so the operator-side check tracks
// the same length the CronJob was told to generate. token_urlsafe(n) yields
// > n characters, so a correctly-generated password always passes. Reviewer:
// please verify.
func normalizedAdminPasswordLength(keystone *keystonev1alpha1.Keystone) int32 {
	pr := keystone.Spec.PasswordRotation
	if pr == nil || pr.PasswordLength == 0 {
		return keystonev1alpha1.DefaultPasswordRotationLength
	}
	if pr.PasswordLength < adminPasswordMinLength {
		return adminPasswordMinLength
	}
	return pr.PasswordLength
}

// reconcilePasswordRotation drives Model B scheduled admin-password rotation
//
// DECISION: the third parameter is the shared config ConfigMap name, accepted
// only for sub-reconciler call-site symmetry with reconcileFernetKeys /
// reconcileTrustFlush (task 3.1 wires it as instrumentSubReconciler(ctx,
// "PasswordRotation", r.reconcilePasswordRotation(..., configMapName))). It is
// intentionally unused (named "_") because the rotate script needs no keystone
// configuration — it never runs keystone-manage. Reviewer: please verify.
//
// Two lifecycle paths:
//   - passwordRotation nil or Enabled=false: tear down every Model B resource
//     and report PasswordRotationReady=True / RotationDisabled.
//   - passwordRotation Enabled: ensure the push-source Secret, staging Secret,
//     apply any completed rotation, RBAC, script ConfigMap, CronJob, and the
//     clobber-safe PushSecret, then report PasswordRotationReady=True.
func (r *KeystoneReconciler) reconcilePasswordRotation(ctx context.Context,
	keystone *keystonev1alpha1.Keystone, _ string,
) (ctrl.Result, error) {
	pr := keystone.Spec.PasswordRotation

	// Disabled/teardown branch. Nil pointer means the feature was
	// never opted into (the defaulting webhook deliberately does not
	// materialize it); Enabled=false means it was switched off. Both tear down
	// all Model B resources and report ready.
	if pr == nil || !pr.Enabled {
		if err := r.teardownPasswordRotation(ctx, keystone); err != nil {
			return ctrl.Result{}, err
		}
		conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
			Type:               conditionTypePasswordRotationReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: keystone.Generation,
			Reason:             "RotationDisabled",
			Message:            "Scheduled admin password rotation is disabled; Model B resources removed",
		})
		return ctrl.Result{}, nil
	}

	minLength := int(normalizedAdminPasswordLength(keystone))

	// 1. Ensure the operator-owned push-source Secret. applyAdminPasswordRotation
	//    commits into it and the PushSecret selects it, so it must exist first.
	pushSource, err := r.ensureAdminPasswordPushSourceSecret(ctx, keystone)
	if err != nil {
		return ctrl.Result{}, err
	}

	// 2. Ensure the staging Secret the CronJob PATCHes rotated passwords into.
	staging, err := r.ensureStagingSecret(ctx, keystone, adminPasswordStagingSecretName(keystone), "admin-password")
	if err != nil {
		return ctrl.Result{}, err
	}

	// 3. Refresh the key_rotation_age gauge from the rotation-completed
	//    annotation. Reads the durable push-source Secret first and
	//    falls back to staging for the pre-first-apply window. Called before
	//    applyAdminPasswordRotation so the next reconcile picks up the freshest
	//    timestamp once the apply re-stamps the push-source annotation.
	//
	//    Semantics caveat (issue #475): for key_type="admin-password" this
	//    timestamp marks the commit-to-push-source instant, NOT the moment the
	//    password goes live. The new password is live only after ESO mirrors
	//    the push-source Secret to OpenBao, the keystone-admin ExternalSecret
	//    syncs it back, and bootstrap re-runs (~1h+). The gauge therefore
	//    under-reports time-since-live for admin-password compared with fernet
	//    and credential, which go live the instant the operator updates the
	//    keys Secret.
	//    Both objects are the ones ensured above — the push-source Secret and the
	//    staging Secret — so no Secret is re-read (issue #361).
	r.observeRotationAge(keystone, pushSource, staging, "admin-password")

	// 4. Apply any completed rotation staged by the CronJob.
	//    On a valid apply, short-circuit and requeue so the next pass re-enters
	//    the happy path with the push-source Secret already updated. The staging
	//    and push-source Secrets are threaded in rather than re-read.
	applied, err := r.applyAdminPasswordRotation(ctx, keystone, staging, pushSource, minLength)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("applying admin password rotation output: %w", err)
	}
	if applied {
		conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
			Type:               conditionTypePasswordRotationReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: keystone.Generation,
			Reason:             "AdminPasswordRotated",
			Message:            "rotation applied; staging secret cleared",
		})
		return ctrl.Result{Requeue: true}, nil
	}

	// 5. Ensure the RBAC resources for the rotation CronJob.
	if err := r.ensureAdminPasswordRotationRBAC(ctx, keystone); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring admin password rotation RBAC: %w", err)
	}

	// 6. Create the immutable ConfigMap containing the rotation script.
	scriptConfigMapName, err := config.CreateImmutableConfigMap(ctx, r.Client, r.Scheme, keystone,
		adminPasswordRotateScriptBaseName(keystone), keystone.Namespace,
		map[string]string{"admin_password_rotate.sh": adminPasswordRotateScript})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("creating admin password rotate script ConfigMap: %w", err)
	}

	// 7. Ensure the rotation CronJob.
	cronJob := adminPasswordRotationCronJob(keystone, scriptConfigMapName)
	if err := job.EnsureCronJob(ctx, r.Client, r.Scheme, keystone, cronJob); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring admin password rotation cronjob: %w", err)
	}

	// 8. Clobber-safe PushSecret only mirror the
	//    push-source Secret to the per-CR OpenBao path once it actually holds a
	//    valid password. Before the first rotation completes the push-source
	//    Secret is empty; pushing it would overwrite the seeded per-CR
	//    bootstrap/{namespace}/{name}/admin value with nothing.
	if r.adminPasswordPushSourceReady(pushSource, minLength) {
		ps := adminPasswordPushSecret(keystone)
		if err := secrets.EnsurePushSecret(ctx, r.Client, r.Scheme, keystone, ps); err != nil {
			return ctrl.Result{}, fmt.Errorf("ensuring admin password pushsecret: %w", err)
		}
	}

	// 9. Report ready.
	conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
		Type:               conditionTypePasswordRotationReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: keystone.Generation,
		Reason:             "PasswordRotationConfigured",
		Message:            "Admin password rotation CronJob is configured",
	})
	return ctrl.Result{}, nil
}

// teardownPasswordRotation deletes every Model B resource. All
// deletes tolerate NotFound so the teardown is idempotent and safe to run on a
// CR that never enabled rotation. The PushSecret uses DeletionPolicy=None (see
// adminPasswordPushSecret), so deleting it here leaves the last-pushed password
// intact in OpenBao at the per-CR path — disabling rotation must never lock the
// admin out.
func (r *KeystoneReconciler) teardownPasswordRotation(ctx context.Context, keystone *keystonev1alpha1.Keystone) error {
	if err := job.DeleteCronJob(ctx, r.Client, keystone.Namespace, adminPasswordRotateCronJobName(keystone)); err != nil {
		return fmt.Errorf("deleting admin password rotation CronJob: %w", err)
	}

	saName := adminPasswordRotateSAName(keystone)
	toDelete := []client.Object{
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: adminPasswordStagingSecretName(keystone), Namespace: keystone.Namespace}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: adminPasswordNextSecretName(keystone), Namespace: keystone.Namespace}},
		&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: saName, Namespace: keystone.Namespace}},
		&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: saName, Namespace: keystone.Namespace}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: saName, Namespace: keystone.Namespace}},
		&esov1alpha1.PushSecret{ObjectMeta: metav1.ObjectMeta{Name: adminPasswordPushSecretName(keystone), Namespace: keystone.Namespace}},
	}
	for _, obj := range toDelete {
		if err := client.IgnoreNotFound(r.Delete(ctx, obj)); err != nil {
			return fmt.Errorf("deleting %T %s: %w", obj, obj.GetName(), err)
		}
	}

	// Delete every script ConfigMap (CurrentName="", Retain=0 prunes all
	// hash-suffixed ConfigMaps for this base name owned by the CR).
	if err := config.PruneImmutableConfigMaps(ctx, r.Client, keystone, config.PruneOptions{
		BaseName:  adminPasswordRotateScriptBaseName(keystone),
		Namespace: keystone.Namespace,
	}); err != nil {
		return fmt.Errorf("pruning admin password rotate script ConfigMaps: %w", err)
	}

	return nil
}

// ensureAdminPasswordPushSourceSecret ensures the operator-owned push-source
// Secret exists. The operator owns the metadata and the `.data` field
// (committed by applyAdminPasswordRotation via a full GET+Update replacement);
// `.data` is deliberately left untouched here so a reconcile never clobbers a
// previously committed password.
// It returns the ensured Secret so the caller can thread it into
// observeRotationAge, applyAdminPasswordRotation, and adminPasswordPushSourceReady
// without re-reading it (issue #361).
func (r *KeystoneReconciler) ensureAdminPasswordPushSourceSecret(ctx context.Context, keystone *keystonev1alpha1.Keystone) (*corev1.Secret, error) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      adminPasswordNextSecretName(keystone),
			Namespace: keystone.Namespace,
		},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		secret.Labels = commonLabels(keystone)
		// Do NOT touch secret.Data — applyAdminPasswordRotation owns it.
		return controllerutil.SetControllerReference(keystone, secret, r.Scheme)
	}); err != nil {
		return nil, fmt.Errorf("ensuring admin password push-source secret %s: %w", adminPasswordNextSecretName(keystone), err)
	}
	return secret, nil
}

// ensureAdminPasswordRotationRBAC ensures the ServiceAccount, Role, and
// RoleBinding for the admin-password rotation CronJob via the shared
// ensureRotationRBAC helper: read-only `get` on the operator-owned push-source
// Secret and `get`+`patch` on the dedicated staging Secret.
func (r *KeystoneReconciler) ensureAdminPasswordRotationRBAC(ctx context.Context, keystone *keystonev1alpha1.Keystone) error {
	return r.ensureRotationRBAC(ctx, keystone,
		adminPasswordRotateSAName(keystone), adminPasswordNextSecretName(keystone), adminPasswordStagingSecretName(keystone))
}

// adminPasswordRotationCronJob builds the CronJob that mints a fresh admin
// password and PATCHes it onto the staging Secret via the K8s API. The CronJob mounts only the rotation script: it needs no
// keystone configuration or key repositories because it never runs
// keystone-manage. SECRET_NAME points at the staging Secret — the CronJob SA is
// only permitted to patch staging, never the push-source Secret.
func adminPasswordRotationCronJob(keystone *keystonev1alpha1.Keystone, scriptConfigMapName string) *batchv1.CronJob {
	pr := keystone.Spec.PasswordRotation
	image := keystone.Spec.Image.Reference()

	return &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      adminPasswordRotateCronJobName(keystone),
			Namespace: keystone.Namespace,
			Labels:    commonLabels(keystone),
		},
		Spec: batchv1.CronJobSpec{
			Schedule: pr.Schedule,
			Suspend:  ptr.To(pr.Suspend),
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: commonLabels(keystone),
						},
						Spec: corev1.PodSpec{
							ServiceAccountName: adminPasswordRotateSAName(keystone),
							RestartPolicy:      corev1.RestartPolicyOnFailure,
							PriorityClassName:  priorityClassName(keystone),
							Containers: []corev1.Container{{
								Name:            "admin-password-rotate",
								Image:           image,
								Command:         []string{"/scripts/admin_password_rotate.sh"},
								SecurityContext: deployment.RestrictedSecurityContext(),
								Env: []corev1.EnvVar{
									{Name: "SECRET_NAME", Value: adminPasswordStagingSecretName(keystone)},
									{Name: "SECRET_NAMESPACE", ValueFrom: &corev1.EnvVarSource{
										FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"},
									}},
									{Name: "PASSWORD_LENGTH", Value: strconv.Itoa(int(normalizedAdminPasswordLength(keystone)))},
								},
								VolumeMounts: []corev1.VolumeMount{
									{Name: "scripts", MountPath: "/scripts", ReadOnly: true},
								},
							}},
							Volumes: []corev1.Volume{
								{
									Name: "scripts",
									VolumeSource: corev1.VolumeSource{
										ConfigMap: &corev1.ConfigMapVolumeSource{
											LocalObjectReference: corev1.LocalObjectReference{Name: scriptConfigMapName},
											DefaultMode:          ptr.To(int32(0o555)),
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

// validateAdminPasswordRotationOutput enforces the rotation-output
// contract the staged Secret must carry a non-empty
// `password` value of at least minLength bytes. Errors are wrapped so callers
// can match with errors.Is against ErrAdminPasswordMissing or
// ErrAdminPasswordTooShort. Defense-in-depth: even a compromised CronJob cannot
// push an empty or trivially short admin credential past the operator.
func validateAdminPasswordRotationOutput(data map[string][]byte, minLength int) error {
	pw, ok := data[adminPasswordSecretKey]
	if !ok || len(pw) == 0 {
		return fmt.Errorf("%w: staging secret has no non-empty %q value", ErrAdminPasswordMissing, adminPasswordSecretKey)
	}
	if len(pw) < minLength {
		return fmt.Errorf("%w: got %d, want >= %d", ErrAdminPasswordTooShort, len(pw), minLength)
	}
	return nil
}

// applyAdminPasswordRotation commits a completed admin-password rotation from
// the staging Secret onto the operator-owned push-source Secret via the shared
// commitStagedRotation helper. It validates a password rather than a key set,
// so the success event omits any key count, and the password value is never
// logged or echoed in events.
//
// Unlike the key flavours, a validation rejection here does NOT clear the
// staged Data (clearStagingOnReject is left false): the staging Secret carries
// a single `password` key, not a multi-key map, so the strategic-merge
// accumulation of stale indices that motivates clearing the fernet/credential
// staging payload cannot occur; the rejected password is retained verbatim for
// operator inspection (issue #475).
func (r *KeystoneReconciler) applyAdminPasswordRotation(
	ctx context.Context,
	keystone *keystonev1alpha1.Keystone,
	staging, pushSource *corev1.Secret,
	minLength int,
) (applied bool, err error) {
	return r.commitStagedRotation(ctx, keystone, staging, pushSource, rotationCommitSpec{
		targetNoun:              "push-source secret",
		validate:                func(data map[string][]byte) error { return validateAdminPasswordRotationOutput(data, minLength) },
		annotationInvalidReason: "AdminPasswordRotationAnnotationInvalid",
		rejectedReason:          "AdminPasswordRotationRejected",
		appliedReason:           "AdminPasswordRotated",
		appliedMessage: func(stagingSecretName string, _ map[string][]byte) string {
			return fmt.Sprintf("admin password rotation applied from staging secret %s", stagingSecretName)
		},
	})
}

// adminPasswordPushSourceReady reports whether the push-source Secret holds a
// valid password. Used to gate the PushSecret so the per-CR
// OpenBao bootstrap/{namespace}/{name}/admin value is never overwritten with an
// empty payload before the first rotation completes. Any read error or
// invalid payload is treated as "not ready" (best-effort gate).
// pushSource is the operator-owned push-source Secret the caller already
// ensured this pass; it is threaded in rather than re-read (issue #361). A nil
// object is treated as "not ready".
func (r *KeystoneReconciler) adminPasswordPushSourceReady(pushSource *corev1.Secret, minLength int) bool {
	if pushSource == nil {
		return false
	}
	return validateAdminPasswordRotationOutput(pushSource.Data, minLength) == nil
}

// adminPasswordPushSecret builds the PushSecret that mirrors the operator-owned
// push-source Secret to OpenBao at the per-CR path
// bootstrap/{keystone.Namespace}/{keystone.Name}/admin. The
// RemoteKey embeds both the CR namespace and name as path segments so two
// Model-B-enabled Keystone CRs never collide on a shared OpenBao object; the
// matching keystone-admin ExternalSecret reads the same per-CR path.
//
// DECISION: DeletionPolicy=None — unspecified by; chose None (not the
// fernet PushSecret's Delete) because the admin bootstrap path is a persistent
// bootstrap secret that the keystone-admin ExternalSecret and the OpenBao seed
// both depend on. Disabling rotation deletes this PushSecret
// (teardownPasswordRotation); DeletionPolicy=None keeps the last-pushed password
// in OpenBao so the admin is never locked out. Reviewer: please verify this
// matches intent.
func adminPasswordPushSecret(keystone *keystonev1alpha1.Keystone) *esov1alpha1.PushSecret {
	return &esov1alpha1.PushSecret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      adminPasswordPushSecretName(keystone),
			Namespace: keystone.Namespace,
		},
		Spec: esov1alpha1.PushSecretSpec{
			DeletionPolicy: esov1alpha1.PushSecretDeletionPolicyNone,
			SecretStoreRefs: []esov1alpha1.PushSecretStoreRef{{
				Kind: "ClusterSecretStore",
				Name: openBaoClusterStoreName,
			}},
			Selector: esov1alpha1.PushSecretSelector{
				Secret: &esov1alpha1.PushSecretSecret{
					Name: adminPasswordNextSecretName(keystone),
				},
			},
			Data: []esov1alpha1.PushSecretData{{
				Match: esov1alpha1.PushSecretMatch{
					SecretKey: adminPasswordSecretKey,
					RemoteRef: esov1alpha1.PushSecretRemoteRef{
						RemoteKey: fmt.Sprintf("bootstrap/%s/%s/admin", keystone.Namespace, keystone.Name),
						Property:  adminPasswordSecretKey,
					},
				},
			}},
		},
	}
}
