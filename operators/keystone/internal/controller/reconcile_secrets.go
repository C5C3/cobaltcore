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
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// openBaoClusterStoreName is the ClusterSecretStore that fronts the OpenBao
// backend used by deploy/eso/externalsecrets/*.yaml. The operator checks this
// store's Ready condition on every reconcile so SecretsReady reflects upstream
// backend outages within the ESO store-reconcile interval — ExternalSecrets
// themselves use a 1h refreshInterval and would otherwise mask short outages
// (CC-0047).
const openBaoClusterStoreName = "openbao-cluster-store"

// reconcileSecrets checks that ESO-provided Kubernetes Secrets exist before
// proceeding. It verifies the DB credentials and admin credentials
// ExternalSecrets are ready (CC-0013).
func (r *KeystoneReconciler) reconcileSecrets(ctx context.Context,
	keystone *keystonev1alpha1.Keystone,
) (ctrl.Result, error) {
	// Check the ClusterSecretStore first so upstream backend outages surface
	// as SecretsReady=False even while per-ExternalSecret caches still report
	// Ready=True from their last successful sync (CC-0047).
	storeReady, err := secrets.IsClusterSecretStoreReady(ctx, r.Client, openBaoClusterStoreName)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !storeReady {
		conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
			Type:               "SecretsReady",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: keystone.Generation,
			Reason:             "SecretStoreNotReady",
			Message: fmt.Sprintf("ClusterSecretStore %q is not ready; upstream secret backend unreachable",
				openBaoClusterStoreName),
		})
		return ctrl.Result{RequeueAfter: RequeueSecretPolling}, nil
	}

	dbSecretKey := client.ObjectKey{Namespace: keystone.Namespace, Name: keystone.Spec.Database.SecretRef.Name}
	adminSecretKey := client.ObjectKey{Namespace: keystone.Namespace, Name: keystone.Spec.Bootstrap.AdminPasswordSecretRef.Name}

	// Check DB credentials ExternalSecret sync status.
	ready, err := secrets.WaitForExternalSecret(ctx, r.Client, dbSecretKey)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !ready {
		conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
			Type:               "SecretsReady",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: keystone.Generation,
			Reason:             "WaitingForDBCredentials",
			Message:            "Waiting for ESO to sync database credentials from OpenBao",
		})
		return ctrl.Result{RequeueAfter: RequeueSecretPolling}, nil
	}

	// Verify the materialized DB Secret contains the expected keys (CC-0013).
	// ESO may update the sync-status condition before the Secret is committed
	// to etcd, so this second check guards against a status-vs-object race.
	secretReady, err := secrets.IsSecretReady(ctx, r.Client, dbSecretKey, "username", "password")
	if err != nil {
		return ctrl.Result{}, err
	}
	if !secretReady {
		conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
			Type:               "SecretsReady",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: keystone.Generation,
			Reason:             "WaitingForDBCredentials",
			Message:            "Database credentials Secret exists but is missing expected keys",
		})
		return ctrl.Result{RequeueAfter: RequeueSecretPolling}, nil
	}

	// Check admin credentials ExternalSecret sync status.
	ready, err = secrets.WaitForExternalSecret(ctx, r.Client, adminSecretKey)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !ready {
		conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
			Type:               "SecretsReady",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: keystone.Generation,
			Reason:             "WaitingForAdminCredentials",
			Message:            "Waiting for ESO to sync admin credentials from OpenBao",
		})
		return ctrl.Result{RequeueAfter: RequeueSecretPolling}, nil
	}

	// Verify the materialized admin Secret contains the expected keys (CC-0013).
	secretReady, err = secrets.IsSecretReady(ctx, r.Client, adminSecretKey, "password")
	if err != nil {
		return ctrl.Result{}, err
	}
	if !secretReady {
		conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
			Type:               "SecretsReady",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: keystone.Generation,
			Reason:             "WaitingForAdminCredentials",
			Message:            "Admin credentials Secret exists but is missing expected keys",
		})
		return ctrl.Result{RequeueAfter: RequeueSecretPolling}, nil
	}

	conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
		Type:               "SecretsReady",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: keystone.Generation,
		Reason:             "SecretsAvailable",
	})
	return ctrl.Result{}, nil
}
