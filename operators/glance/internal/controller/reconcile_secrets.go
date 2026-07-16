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
	glancev1alpha1 "github.com/c5c3/forge/operators/glance/api/v1alpha1"
)

// effectiveServiceUserKey returns the Secret data key holding the service-user
// password, defaulting to "password" when spec.serviceUser.secretRef.key is
// empty (a CR that bypassed the defaulting webhook). It mirrors horizon's
// effectiveSecretKeyKey nil-tolerance.
func effectiveServiceUserKey(glance *glancev1alpha1.Glance) string {
	if key := glance.Spec.ServiceUser.SecretRef.Key; key != "" {
		return key
	}
	return "password"
}

// reconcileSecrets checks that the ESO-provided Kubernetes Secrets exist before
// proceeding and returns the SHA-256 digest of the service-user password. It
// gates on the selected secret store first, then the database credentials and
// the service-user credentials Secrets, maintaining SecretsReady. The digest is
// stamped into a pod-template annotation by reconcileDeployment (next commit)
// so a rotated service-user password rolls the Glance pods — the password is
// env-var-consumed (oslo.config OS_KEYSTONE_AUTHTOKEN__PASSWORD), not
// volume-mounted, so it only takes effect on a Pod restart.
func (r *GlanceReconciler) reconcileSecrets(ctx context.Context,
	glance *glancev1alpha1.Glance,
) (ctrl.Result, string, error) {
	// Check the selected secret store first so upstream backend outages surface
	// as SecretsReady=False even while per-ExternalSecret caches still report
	// Ready=True from their last successful sync. The store is the one this
	// Glance selected via spec.secretStoreRef (default: the shared cluster-scoped
	// openbao-cluster-store); a namespaced store is resolved in the Glance's own
	// namespace.
	storeReady, err := secrets.GateStoreReady(ctx, r.Client,
		secrets.EffectiveStoreRef(glance.Spec.SecretStoreRef), glance.Namespace,
		&glance.Status.Conditions, glance.Generation, "SecretsReady")
	if err != nil {
		return ctrl.Result{}, "", err
	}
	if !storeReady {
		return ctrl.Result{RequeueAfter: RequeueSecretPolling}, "", nil
	}

	// Validate the credential Secrets from a declarative (secretRef, expectedKeys)
	// list. Each check reads the materialized Secret first (the steady-state fast
	// path) and only consults the ExternalSecret to build a precise
	// SecretsReady=False message when the Secret is not yet usable.
	serviceUserKey := effectiveServiceUserKey(glance)
	credentialGates := []secrets.CredentialGateSpec{
		{
			Key:          client.ObjectKey{Namespace: glance.Namespace, Name: glance.Spec.Database.SecretRef.Name},
			Reason:       "WaitingForDBCredentials",
			Noun:         "Database credentials",
			WaitingMsg:   "Waiting for ESO to sync database credentials from OpenBao",
			ExpectedKeys: []string{"username", "password"},
		},
		{
			Key:          client.ObjectKey{Namespace: glance.Namespace, Name: glance.Spec.ServiceUser.SecretRef.Name},
			Reason:       "WaitingForServiceUserCredentials",
			Noun:         "Service-user credentials",
			WaitingMsg:   "Waiting for ESO to sync the Glance service-user password from OpenBao",
			ExpectedKeys: []string{serviceUserKey},
		},
	}
	ready, err := secrets.GateCredentials(ctx, r.Client, credentialGates,
		&glance.Status.Conditions, glance.Generation, "SecretsReady")
	if err != nil {
		return ctrl.Result{}, "", err
	}
	if !ready {
		return ctrl.Result{RequeueAfter: RequeueSecretPolling}, "", nil
	}

	// Digest the service-user password so reconcileDeployment can roll pods when
	// it rotates at the OpenBao source.
	key := client.ObjectKey{Namespace: glance.Namespace, Name: glance.Spec.ServiceUser.SecretRef.Name}
	value, err := secrets.GetSecretValue(ctx, r.Client, key, serviceUserKey)
	if err != nil {
		return ctrl.Result{}, "", fmt.Errorf("reading service-user password value: %w", err)
	}

	conditions.SetCondition(&glance.Status.Conditions, metav1.Condition{
		Type:               "SecretsReady",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: glance.Generation,
		Reason:             "SecretsAvailable",
	})
	return ctrl.Result{}, secrets.AdminPasswordDigest(value), nil
}
