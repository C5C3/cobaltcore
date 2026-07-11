// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/secrets"
	horizonv1alpha1 "github.com/c5c3/forge/operators/horizon/api/v1alpha1"
)

// openBaoClusterStoreName re-exports the shared ClusterSecretStore name (see
// secrets.OpenBaoClusterStoreName) for the watches and tests in this package.
const openBaoClusterStoreName = secrets.OpenBaoClusterStoreName

// effectiveSecretKeyKey returns the Secret data key holding the Django
// SECRET_KEY, defaulting to horizonv1alpha1.DefaultSecretKeyKey when
// spec.secretKeyRef.key is empty (a CR that bypassed the defaulting webhook).
func effectiveSecretKeyKey(horizon *horizonv1alpha1.Horizon) string {
	if key := horizon.Spec.SecretKeyRef.Key; key != "" {
		return key
	}
	return horizonv1alpha1.DefaultSecretKeyKey
}

// reconcileSecrets checks that the ESO-provided Django SECRET_KEY Secret
// exists before proceeding and returns the SHA-256 digest of the key
// material. The digest is stamped into a pod-template annotation by
// reconcileDeployment so a rotated SECRET_KEY rolls the dashboard pods (the
// key is env-var-consumed, not volume-mounted).
func (r *HorizonReconciler) reconcileSecrets(ctx context.Context,
	horizon *horizonv1alpha1.Horizon,
) (ctrl.Result, string, error) {
	// Check the ClusterSecretStore first so upstream backend outages surface
	// as SecretsReady=False even while the per-ExternalSecret cache still
	// reports Ready=True from its last successful sync.
	storeReady, err := secrets.GateClusterStoreReady(ctx, r.Client, openBaoClusterStoreName,
		&horizon.Status.Conditions, horizon.Generation, "SecretsReady")
	if err != nil {
		return ctrl.Result{}, "", err
	}
	if !storeReady {
		return ctrl.Result{RequeueAfter: RequeueSecretPolling}, "", nil
	}

	// Gate on the materialized SECRET_KEY Secret via the shared ladder: the
	// Secret is read first (steady-state fast path) and the ExternalSecret is
	// only consulted to attribute the cause of a miss.
	key := client.ObjectKey{Namespace: horizon.Namespace, Name: horizon.Spec.SecretKeyRef.Name}
	dataKey := effectiveSecretKeyKey(horizon)
	state, err := secrets.GateSyncedSecret(ctx, r.Client, key, dataKey)
	if err != nil {
		return ctrl.Result{}, "", err
	}
	if state != secrets.GateReady {
		msg := "Waiting for ESO to sync the Django SECRET_KEY from OpenBao"
		switch state {
		case secrets.GateExternalSecretMissing:
			msg = fmt.Sprintf("SECRET_KEY ExternalSecret %s/%s not found yet", key.Namespace, key.Name)
		case secrets.GateSecretKeysMissing:
			msg = fmt.Sprintf("SECRET_KEY Secret exists but is missing expected key %q", dataKey)
		case secrets.GateExternalSecretNotSynced, secrets.GateReady:
			// NotSynced keeps the generic waiting message; Ready handled above.
		}
		conditions.SetCondition(&horizon.Status.Conditions, metav1.Condition{
			Type:               "SecretsReady",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: horizon.Generation,
			Reason:             "WaitingForSecretKey",
			Message:            msg,
		})
		return ctrl.Result{RequeueAfter: RequeueSecretPolling}, "", nil
	}

	// Digest the key material so reconcileDeployment can roll pods when it
	// rotates at the OpenBao source.
	value, err := secrets.GetSecretValue(ctx, r.Client, key, dataKey)
	if err != nil {
		return ctrl.Result{}, "", fmt.Errorf("reading SECRET_KEY value: %w", err)
	}

	conditions.SetCondition(&horizon.Status.Conditions, metav1.Condition{
		Type:               "SecretsReady",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: horizon.Generation,
		Reason:             "SecretsAvailable",
	})
	return ctrl.Result{}, secrets.AdminPasswordDigest(value), nil
}
