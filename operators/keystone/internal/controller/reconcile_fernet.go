// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/base64"
	"fmt"
	"strconv"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/config"
	"github.com/c5c3/forge/internal/common/job"
	"github.com/c5c3/forge/internal/common/secrets"
	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"

	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// fernetRotateScript is the shell script executed by the Fernet rotation CronJob.
// It rotates the keys on an emptyDir working copy, then pushes the updated keys
// back to the Kubernetes Secret via the API using the pod's ServiceAccount token.
// Only Python standard library modules are used to avoid image dependencies.
// Extracted to scripts/fernet_rotate.sh for independent linting and testing.
//
//go:embed scripts/fernet_rotate.sh
var fernetRotateScript string

// reconcileFernetKeys ensures that a Fernet keys Secret exists, a rotation
// CronJob is configured, and a PushSecret backs up the keys to OpenBao.
func (r *KeystoneReconciler) reconcileFernetKeys(ctx context.Context,
	keystone *keystonev1alpha1.Keystone, configMapName string,
) (ctrl.Result, error) {
	// 1. Ensure the Fernet keys Secret exists.
	secretName := fmt.Sprintf("%s-fernet-keys", keystone.Name)
	secretKey := client.ObjectKey{Namespace: keystone.Namespace, Name: secretName}

	existing := &corev1.Secret{}
	err := r.Get(ctx, secretKey, existing)
	if apierrors.IsNotFound(err) {
		// Generate initial Fernet keys.
		if err := r.createKeysSecret(ctx, keystone, secretName, normalizedFernetMaxActiveKeys(keystone)); err != nil {
			return ctrl.Result{}, fmt.Errorf("creating fernet keys secret: %w", err)
		}
		r.Recorder.Event(keystone, corev1.EventTypeNormal, "FernetKeysGenerated", "Initial Fernet encryption keys have been generated")
		conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
			Type:               "FernetKeysReady",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: keystone.Generation,
			Reason:             "GeneratingKeys",
			Message:            "Initial Fernet keys have been generated",
		})
		// Requeue to confirm the secret is available before proceeding. Uses
		// RequeueAfter (not the deprecated ctrl.Result.Requeue field) so the
		// parallel group's shortestRequeue propagates this non-zero result and
		// the chain short-circuits, instead of dropping it and continuing in the
		// same pass (issue #467).
		return ctrl.Result{RequeueAfter: RequeueSecretPolling}, nil
	} else if err != nil {
		return ctrl.Result{}, fmt.Errorf("getting fernet keys secret: %w", err)
	}

	// 2. Ensure the staging Secret exists for the rotation CronJob to PATCH
	//    into. The operator owns the lifecycle (labels, owner ref)
	//    and the CronJob owns the Data — this is the split-compute-write
	//    boundary that keeps token-forgery primitives out of the CronJob's
	//    RBAC on the production Secret.
	if err := r.ensureFernetStagingSecret(ctx, keystone); err != nil {
		return ctrl.Result{}, err
	}

	// Refresh the key_rotation_age gauge from the rotation-completed
	// annotation. The helper reads the production Secret
	// first (durable across the inter-rotation steady state) and falls back
	// to the staging Secret to cover the very-first-rotation pre-apply
	// window. Called BEFORE applyRotationOutput so that, when the apply path
	// runs and re-stamps the production annotation, the next reconcile picks
	// up the freshest timestamp.
	r.observeRotationAge(ctx, keystone, secretName, fernetStagingSecretName(keystone), "fernet")

	// 3. Apply any completed rotation staged by the CronJob. On a valid apply we short-circuit the rest of
	//    the step chain and requeue so the next pass re-enters the happy
	//    path with the production Secret already updated.
	applied, err := r.applyRotationOutput(
		ctx, keystone,
		fernetStagingSecretName(keystone),
		secretName,
		"FernetKeysRotated",
		3,
		normalizedFernetMaxActiveKeys(keystone)+1,
	)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("applying fernet rotation output: %w", err)
	}
	if applied {
		conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
			Type:               "FernetKeysReady",
			Status:             metav1.ConditionTrue,
			ObservedGeneration: keystone.Generation,
			Reason:             "FernetKeysRotated",
			Message:            "rotation applied; staging secret cleared",
		})
		// Short-circuit the rest of the step chain via RequeueAfter (not the
		// deprecated ctrl.Result.Requeue field) so the parallel group's
		// shortestRequeue propagates it and the next pass re-enters the happy
		// path with the production Secret already updated (issue #467).
		return ctrl.Result{RequeueAfter: RequeueSecretPolling}, nil
	}

	// 4. Ensure the RBAC resources for the rotation CronJob exist.
	if err := r.ensureFernetRotationRBAC(ctx, keystone, secretName); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring fernet rotation RBAC: %w", err)
	}

	// 5. Create the immutable ConfigMap containing the rotation script.
	scriptConfigMapName, err := config.CreateImmutableConfigMap(ctx, r.Client, r.Scheme, keystone,
		fmt.Sprintf("%s-fernet-rotate-script", keystone.Name), keystone.Namespace,
		map[string]string{"fernet_rotate.sh": fernetRotateScript})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("creating fernet rotate script ConfigMap: %w", err)
	}

	// 6. Ensure the rotation CronJob exists.
	cronJob := fernetRotationCronJob(keystone, configMapName, scriptConfigMapName)
	if err := job.EnsureCronJob(ctx, r.Client, r.Scheme, keystone, cronJob); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring fernet rotation cronjob: %w", err)
	}

	// 7. Ensure the PushSecret for OpenBao backup exists.
	ps := fernetKeysPushSecret(keystone)
	if err := secrets.EnsurePushSecret(ctx, r.Client, r.Scheme, keystone, ps); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring fernet keys pushsecret: %w", err)
	}

	// 8. Set the FernetKeysReady condition.
	conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
		Type:               "FernetKeysReady",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: keystone.Generation,
		Reason:             "FernetKeysAvailable",
		Message:            "Fernet keys Secret exists and rotation CronJob is configured",
	})

	return ctrl.Result{}, nil
}

// ensureFernetRotationRBAC ensures the ServiceAccount, Role, and RoleBinding for
// the Fernet rotation CronJob via the shared ensureRotationRBAC helper:
// read-only `get` on the production fernet keys Secret and `get`+`patch` on the
// dedicated staging Secret.
func (r *KeystoneReconciler) ensureFernetRotationRBAC(ctx context.Context, keystone *keystonev1alpha1.Keystone, secretName string) error {
	return r.ensureRotationRBAC(ctx, keystone,
		fmt.Sprintf("%s-fernet-rotate", keystone.Name), secretName, fernetStagingSecretName(keystone))
}

// ensureFernetStagingSecret ensures the Fernet staging Secret exists with the
// `fernet-keys` rotation-target label. Thin wrapper over the shared
// ensureStagingSecret helper; see rotation_staging.go for the field-ownership
// contract.
func (r *KeystoneReconciler) ensureFernetStagingSecret(ctx context.Context, keystone *keystonev1alpha1.Keystone) error {
	return r.ensureStagingSecret(ctx, keystone, fernetStagingSecretName(keystone), "fernet-keys")
}

// normalizedFernetMaxActiveKeys returns the effective maximum number of active
// Fernet keys, applying a minimum floor of 3. The webhook defaults 0 to 3, but
// this provides defense-in-depth for the reconciler.
func normalizedFernetMaxActiveKeys(keystone *keystonev1alpha1.Keystone) int {
	return max(int(keystone.Spec.Fernet.MaxActiveKeys), 3)
}

// createKeysSecret generates numKeys Fernet-format keys (32 bytes, base64url
// with padding) and creates the named Secret to hold them, owned by keystone.
// Shared by the Fernet and credential sub-reconcilers, which use the identical
// key format and differ only in the configured key count.
func (r *KeystoneReconciler) createKeysSecret(ctx context.Context,
	keystone *keystonev1alpha1.Keystone, secretName string, numKeys int,
) error {
	data := make(map[string][]byte, numKeys)
	for i := 0; i < numKeys; i++ {
		key, err := generateFernetKey()
		if err != nil {
			return err
		}
		data[strconv.Itoa(i)] = []byte(key)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: keystone.Namespace,
			Labels:    commonLabels(keystone),
		},
		Data: data,
	}

	if err := controllerutil.SetControllerReference(keystone, secret, r.Scheme); err != nil {
		return fmt.Errorf("setting owner reference on keys secret %s: %w", secretName, err)
	}

	if err := r.Create(ctx, secret); err != nil {
		return fmt.Errorf("creating keys secret %s: %w", secretName, err)
	}

	return nil
}

// generateFernetKey generates a Fernet-compatible key (32 bytes, base64url-encoded with standard padding).
// Keystone (via Python's cryptography.fernet.Fernet) expects 44-char base64url strings WITH padding.
func generateFernetKey() (string, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return "", fmt.Errorf("generating fernet key: %w", err)
	}
	return base64.URLEncoding.EncodeToString(key), nil
}

// fernetRotationCronJob builds the CronJob that rotates Fernet keys and PATCHes
// the result onto the staging Secret via the API. Thin wrapper over the shared
// keyRotationCronJob builder; see rotation_cronjob.go for the Pod spec.
func fernetRotationCronJob(keystone *keystonev1alpha1.Keystone, configMapName string, scriptConfigMapName string) *batchv1.CronJob {
	cronJob := keyRotationCronJob(keystone, configMapName, scriptConfigMapName, keyRotationParams{
		keyKind:           "fernet",
		otherKeyKind:      "credential",
		stagingSecretName: fernetStagingSecretName(keystone),
		schedule:          keystone.Spec.Fernet.RotationSchedule,
		maxActiveKeysEnv:  "OS_fernet_tokens__max_active_keys",
		maxActiveKeys:     normalizedFernetMaxActiveKeys(keystone),
	})
	// Suspend pauses the rotation CronJob without changing its cadence. It reads
	// the per-flavour spec field, so it is applied here rather than in the shared
	// keyRotationCronJob builder.
	cronJob.Spec.Suspend = ptr.To(keystone.Spec.Fernet.Suspend)
	return cronJob
}

// fernetKeysPushSecret builds the PushSecret CR that backs up Fernet keys to OpenBao.
//
// The RemoteKey embeds both keystone.Namespace and keystone.Name as path
// segments (kv-v2/openstack/keystone/{keystone.Namespace}/{keystone.Name}/fernet-keys)
// so two Keystone CRs sharing a Name in different namespaces never share a
// backing OpenBao object (namespace segment).
//
// DeletionPolicy=Delete wires the backup PushSecret into the OpenBao finalizer
// flow: when the keystone.openstack.c5c3.io/openbao-finalizer handler deletes
// this PushSecret, ESO purges the remote
// kv-v2/openstack/keystone/{keystone.Namespace}/{keystone.Name}/fernet-keys
// path before letting the PushSecret object be garbage-collected.
func fernetKeysPushSecret(keystone *keystonev1alpha1.Keystone) *esov1alpha1.PushSecret {
	return &esov1alpha1.PushSecret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-fernet-keys-backup", keystone.Name),
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
					Name: fmt.Sprintf("%s-fernet-keys", keystone.Name),
				},
			},
			Data: []esov1alpha1.PushSecretData{{
				Match: esov1alpha1.PushSecretMatch{
					RemoteRef: esov1alpha1.PushSecretRemoteRef{
						// DECISION: boundary 4 — chose option (a), a keystone.Namespace
						// path segment, so two Keystone CRs with the same Name in different namespaces
						// resolve to distinct OpenBao leaves. Reviewer: please verify.
						RemoteKey: "openstack/keystone/" + keystone.Namespace + "/" + keystone.Name + "/fernet-keys",
					},
				},
			}},
		},
	}
}
