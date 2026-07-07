// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	_ "embed"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/config"
	"github.com/c5c3/forge/internal/common/job"
	"github.com/c5c3/forge/internal/common/secrets"
	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"

	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// credentialRotateScript is the shell script executed by the credential rotation CronJob.
// It rotates credential keys, migrates existing encrypted credentials in the database
// to use the new primary key, then pushes the updated keys back to the Kubernetes
// Secret via the API using the pod's ServiceAccount token.
// The credential_migrate step is critical: without it, credentials encrypted with the
// old primary key become inaccessible once that key is purged.
// Extracted to scripts/credential_rotate.sh for independent linting and testing.
//
//go:embed scripts/credential_rotate.sh
var credentialRotateScript string

// normalizedCredentialMaxActiveKeys returns the effective maximum number of active
// credential keys, applying a minimum floor of 3. The webhook defaults 0 to 3, but
// this provides defense-in-depth for the reconciler.
func normalizedCredentialMaxActiveKeys(keystone *keystonev1alpha1.Keystone) int {
	return max(int(keystone.Spec.CredentialKeys.MaxActiveKeys), 3)
}

// reconcileCredentialKeys ensures that a credential keys Secret exists, a rotation
// CronJob is configured, and a PushSecret backs up the keys to OpenBao.
func (r *KeystoneReconciler) reconcileCredentialKeys(ctx context.Context,
	keystone *keystonev1alpha1.Keystone, configMapName, domainsSecretName string,
) (ctrl.Result, error) {
	// 1. Ensure the credential keys Secret exists.
	secretName := fmt.Sprintf("%s-credential-keys", keystone.Name)

	existing := &corev1.Secret{}
	err := r.Get(ctx, client.ObjectKey{Namespace: keystone.Namespace, Name: secretName}, existing)
	if apierrors.IsNotFound(err) {
		if err := r.createKeysSecret(ctx, keystone, secretName, normalizedCredentialMaxActiveKeys(keystone)); err != nil {
			return ctrl.Result{}, fmt.Errorf("creating credential keys secret: %w", err)
		}
		r.Recorder.Event(keystone, corev1.EventTypeNormal, "CredentialKeysGenerated", "Initial credential encryption keys have been generated")
		conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
			Type:               "CredentialKeysReady",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: keystone.Generation,
			Reason:             "GeneratingKeys",
			Message:            "Initial credential keys have been generated",
		})
		// Requeue to confirm the secret is available before proceeding. Uses
		// RequeueAfter (not the deprecated ctrl.Result.Requeue field) so the
		// parallel group's shortestRequeue propagates this non-zero result and
		// the chain short-circuits, instead of dropping it and continuing in the
		// same pass (issue #467).
		return ctrl.Result{RequeueAfter: RequeueSecretPolling}, nil
	}
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("getting credential keys secret: %w", err)
	}

	// 2. Ensure the staging Secret exists for the rotation CronJob to PATCH
	//    into. The operator owns the lifecycle (labels, owner ref)
	//    and the CronJob owns the Data — this is the split-compute-write
	//    boundary that keeps token-forgery primitives out of the CronJob's
	//    RBAC on the production Secret.
	staging, err := r.ensureCredentialStagingSecret(ctx, keystone)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Refresh the key_rotation_age gauge from the rotation-completed
	// annotation. The helper reads the production Secret first (durable across
	// the inter-rotation steady state) and falls back to the staging Secret to
	// cover the very-first-rotation pre-apply window. Both objects are the ones
	// already fetched this pass — the production Secret (existing) and the
	// staging Secret returned by ensureCredentialStagingSecret — so no Secret is
	// re-read (issue #361).
	r.observeRotationAge(keystone, existing, staging, "credential")

	// 3. Apply any completed staging rotation onto the production Secret
	// When applyRotationOutput returns
	//    applied=true, short-circuit and requeue so the next reconcile
	//    re-creates the empty staging Secret for the next CronJob run. The
	//    upper bound on keys is normalized max + 1 to account for the extra
	//    incoming primary key produced by `keystone-manage credential_rotate`.
	//    The staging and production Secrets are threaded in rather than re-read.
	applied, err := r.applyRotationOutput(
		ctx,
		keystone,
		staging,
		existing,
		"CredentialKeysRotated",
		3,
		normalizedCredentialMaxActiveKeys(keystone)+1,
	)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("applying credential rotation output: %w", err)
	}
	if applied {
		conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
			Type:               "CredentialKeysReady",
			Status:             metav1.ConditionTrue,
			ObservedGeneration: keystone.Generation,
			Reason:             "CredentialKeysRotated",
			Message:            "rotation applied; staging secret cleared",
		})
		// Short-circuit the rest of the step chain via RequeueAfter (not the
		// deprecated ctrl.Result.Requeue field) so the parallel group's
		// shortestRequeue propagates it and the next pass re-enters the happy
		// path with the production Secret already updated (issue #467).
		return ctrl.Result{RequeueAfter: RequeueSecretPolling}, nil
	}

	// 4. Ensure the RBAC resources for the rotation CronJob exist.
	if err := r.ensureCredentialRotationRBAC(ctx, keystone, secretName); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring credential rotation RBAC: %w", err)
	}

	// 5. Create the immutable ConfigMap containing the rotation script.
	scriptConfigMapName, err := config.CreateImmutableConfigMap(ctx, r.Client, r.Scheme, keystone,
		fmt.Sprintf("%s-credential-rotate-script", keystone.Name), keystone.Namespace,
		map[string]string{"credential_rotate.sh": credentialRotateScript})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("creating credential rotate script ConfigMap: %w", err)
	}

	// 6. Ensure the rotation CronJob exists.
	cronJob := credentialRotationCronJob(keystone, configMapName, scriptConfigMapName, domainsSecretName)
	if err := job.EnsureCronJob(ctx, r.Client, r.Scheme, keystone, cronJob); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring credential rotation cronjob: %w", err)
	}

	// 7. Ensure the PushSecret for OpenBao backup exists.
	ps := credentialKeysPushSecret(keystone)
	if err := secrets.EnsurePushSecret(ctx, r.Client, r.Scheme, keystone, ps); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring credential keys pushsecret: %w", err)
	}

	// 8. Set the CredentialKeysReady condition.
	conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
		Type:               "CredentialKeysReady",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: keystone.Generation,
		Reason:             "CredentialKeysAvailable",
		Message:            "Credential keys Secret exists and rotation CronJob is configured",
	})

	return ctrl.Result{}, nil
}

// ensureCredentialRotationRBAC ensures the ServiceAccount, Role, and RoleBinding
// for the credential rotation CronJob via the shared ensureRotationRBAC helper:
// read-only `get` on the production credential-keys Secret and `get`+`patch` on
// the dedicated staging Secret.
func (r *KeystoneReconciler) ensureCredentialRotationRBAC(ctx context.Context, keystone *keystonev1alpha1.Keystone, secretName string) error {
	return r.ensureRotationRBAC(ctx, keystone,
		fmt.Sprintf("%s-credential-rotate", keystone.Name), secretName, credentialStagingSecretName(keystone))
}

// ensureCredentialStagingSecret ensures the credential staging Secret exists
// with the `credential-keys` rotation-target label. Thin wrapper
// over the shared ensureStagingSecret helper; see rotation_staging.go for the
// field-ownership contract.
func (r *KeystoneReconciler) ensureCredentialStagingSecret(ctx context.Context, keystone *keystonev1alpha1.Keystone) (*corev1.Secret, error) {
	return r.ensureStagingSecret(ctx, keystone, credentialStagingSecretName(keystone), "credential-keys")
}

// credentialRotationCronJob builds the CronJob that rotates credential keys
// (running credential_migrate against the DB) and PATCHes the result onto the
// staging Secret via the API. Thin wrapper over the shared keyRotationCronJob
// builder; see rotation_cronjob.go for the Pod spec.
func credentialRotationCronJob(keystone *keystonev1alpha1.Keystone, configMapName, scriptConfigMapName, domainsSecretName string) *batchv1.CronJob {
	cronJob := keyRotationCronJob(keystone, configMapName, scriptConfigMapName, domainsSecretName, keyRotationParams{
		keyKind:           "credential",
		otherKeyKind:      "fernet",
		stagingSecretName: credentialStagingSecretName(keystone),
		schedule:          keystone.Spec.CredentialKeys.RotationSchedule,
		maxActiveKeysEnv:  "OS_credential__max_active_keys",
		maxActiveKeys:     normalizedCredentialMaxActiveKeys(keystone),
	})
	// Suspend pauses the rotation CronJob without changing its cadence. It reads
	// the per-flavour spec field, so it is applied here rather than in the shared
	// keyRotationCronJob builder.
	cronJob.Spec.Suspend = ptr.To(keystone.Spec.CredentialKeys.Suspend)
	return cronJob
}

// credentialKeysPushSecret builds the PushSecret CR that backs up credential keys to OpenBao.
//
// The RemoteKey embeds both keystone.Namespace and keystone.Name as path
// segments (kv-v2/openstack/keystone/{keystone.Namespace}/{keystone.Name}/credential-keys)
// so two Keystone CRs sharing a Name in different namespaces never share a
// backing OpenBao object (namespace segment).
//
// DeletionPolicy=Delete wires the backup PushSecret into the OpenBao finalizer
// flow: when the keystone.openstack.c5c3.io/openbao-finalizer handler deletes
// this PushSecret, ESO purges the remote
// kv-v2/openstack/keystone/{keystone.Namespace}/{keystone.Name}/credential-keys
// path before letting the PushSecret object be garbage-collected.
func credentialKeysPushSecret(keystone *keystonev1alpha1.Keystone) *esov1alpha1.PushSecret {
	return &esov1alpha1.PushSecret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-credential-keys-backup", keystone.Name),
			Namespace: keystone.Namespace,
		},
		Spec: esov1alpha1.PushSecretSpec{
			DeletionPolicy: esov1alpha1.PushSecretDeletionPolicyDelete,
			SecretStoreRefs: []esov1alpha1.PushSecretStoreRef{{
				Kind: "ClusterSecretStore",
				Name: openBaoClusterStoreName,
			}},
			Selector: esov1alpha1.PushSecretSelector{
				Secret: &esov1alpha1.PushSecretSecret{
					Name: fmt.Sprintf("%s-credential-keys", keystone.Name),
				},
			},
			Data: []esov1alpha1.PushSecretData{{
				Match: esov1alpha1.PushSecretMatch{
					RemoteRef: esov1alpha1.PushSecretRemoteRef{
						// DECISION: boundary 4 — chose option (a), a keystone.Namespace
						// path segment, so two Keystone CRs with the same Name in different namespaces
						// resolve to distinct OpenBao leaves. Reviewer: please verify.
						RemoteKey: "openstack/keystone/" + keystone.Namespace + "/" + keystone.Name + "/credential-keys",
					},
				},
			}},
		},
	}
}
