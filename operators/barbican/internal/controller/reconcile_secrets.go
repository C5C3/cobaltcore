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

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	"github.com/c5c3/cobaltcore/internal/common/secrets"
	barbicanv1alpha1 "github.com/c5c3/cobaltcore/operators/barbican/api/v1alpha1"
)

// effectiveServiceUserKey returns the Secret data key holding the service-user
// password, defaulting to "password" when spec.serviceUser.secretRef.key is
// empty (a CR that bypassed the defaulting webhook).
func effectiveServiceUserKey(barbican *barbicanv1alpha1.Barbican) string {
	if key := barbican.Spec.ServiceUser.SecretRef.Key; key != "" {
		return key
	}
	return "password"
}

// reconcileSecrets checks that the ESO-provided Kubernetes Secrets exist before
// proceeding and returns the SHA-256 digest of the service-user password. It
// gates on the selected secret store first, then the database credentials and
// the service-user credentials Secrets, maintaining SecretsReady. The digest is
// stamped into a pod-template annotation by the deployment step so a rotated
// service-user password rolls the Barbican pods — the password is
// env-var-consumed (oslo.config OS_KEYSTONE_AUTHTOKEN__PASSWORD), not
// volume-mounted, so it only takes effect on a Pod restart.
//
// The selected store and the credential Secrets are read on the children
// cluster: they are materialised beside the workload that consumes them.
func (r *BarbicanReconciler) reconcileSecrets(ctx context.Context, children client.Client,
	barbican *barbicanv1alpha1.Barbican,
) (ctrl.Result, string, error) {
	// Check the selected secret store first so upstream backend outages surface
	// as SecretsReady=False even while per-ExternalSecret caches still report
	// Ready=True from their last successful sync. The store is the one this
	// Barbican selected via spec.secretStoreRef (default: the shared
	// cluster-scoped openbao-cluster-store); a namespaced store is resolved in the
	// Barbican's own namespace.
	storeReady, err := secrets.GateStoreReady(ctx, children,
		secrets.EffectiveStoreRef(barbican.Spec.SecretStoreRef), barbican.Namespace,
		&barbican.Status.Conditions, barbican.Generation, "SecretsReady")
	if err != nil {
		return ctrl.Result{}, "", err
	}
	if !storeReady {
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, "", nil
	}

	// Validate the credential Secrets from a declarative (secretRef, expectedKeys)
	// list. Each check reads the materialized Secret first (the steady-state fast
	// path) and only consults the ExternalSecret to build a precise
	// SecretsReady=False message when the Secret is not yet usable.
	serviceUserKey := effectiveServiceUserKey(barbican)
	credentialGates := []secrets.CredentialGateSpec{
		{
			Key:          client.ObjectKey{Namespace: barbican.Namespace, Name: barbican.Spec.Database.SecretRef.Name},
			Reason:       "WaitingForDBCredentials",
			Noun:         "Database credentials",
			WaitingMsg:   "Waiting for ESO to sync database credentials from OpenBao",
			ExpectedKeys: []string{"username", "password"},
		},
		{
			Key:          client.ObjectKey{Namespace: barbican.Namespace, Name: barbican.Spec.ServiceUser.SecretRef.Name},
			Reason:       "WaitingForServiceUserCredentials",
			Noun:         "Service-user credentials",
			WaitingMsg:   "Waiting for ESO to sync the Barbican service-user password from OpenBao",
			ExpectedKeys: []string{serviceUserKey},
		},
	}
	ready, err := secrets.GateCredentials(ctx, children, credentialGates,
		&barbican.Status.Conditions, barbican.Generation, "SecretsReady")
	if err != nil {
		return ctrl.Result{}, "", err
	}
	if !ready {
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, "", nil
	}

	// Digest the service-user password so the deployment step can roll pods when
	// it rotates at the OpenBao source.
	key := client.ObjectKey{Namespace: barbican.Namespace, Name: barbican.Spec.ServiceUser.SecretRef.Name}
	value, err := secrets.GetSecretValue(ctx, children, key, serviceUserKey)
	if err != nil {
		return ctrl.Result{}, "", fmt.Errorf("reading service-user password value: %w", err)
	}

	conditions.SetCondition(&barbican.Status.Conditions, metav1.Condition{
		Type:               "SecretsReady",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: barbican.Generation,
		Reason:             "SecretsAvailable",
	})
	return ctrl.Result{}, secrets.AdminPasswordDigest(value), nil
}
