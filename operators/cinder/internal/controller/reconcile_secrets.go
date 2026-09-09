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
	cinderv1alpha1 "github.com/c5c3/cobaltcore/operators/cinder/api/v1alpha1"
)

// effectiveServiceUserKey returns the Secret data key holding the service-user
// password, defaulting to "password" when spec.serviceUser.secretRef.key is
// empty (a CR that bypassed the defaulting webhook). A Cinder deployed without
// the Keystone integration carries no spec.serviceUser at all; callers gate on
// that before they use the key.
func effectiveServiceUserKey(cinder *cinderv1alpha1.Cinder) string {
	if cinder.Spec.ServiceUser != nil && cinder.Spec.ServiceUser.SecretRef.Key != "" {
		return cinder.Spec.ServiceUser.SecretRef.Key
	}
	return "password"
}

// reconcileSecrets checks that the ESO-provided Kubernetes Secrets exist before
// proceeding and returns the SHA-256 digest of the service-user password. It
// gates on the selected secret store first, then the database credentials and —
// only for a Cinder that configures the Keystone integration — the service-user
// credentials Secret, maintaining SecretsReady. The digest is stamped into a
// pod-template annotation by the deployment step so a rotated service-user
// password rolls the Cinder pods: the password is env-var-consumed (oslo.config
// OS_KEYSTONE_AUTHTOKEN__PASSWORD), not volume-mounted, so it only takes effect
// on a Pod restart.
//
// The digest is empty for a Cinder without spec.serviceUser. Such a deployment
// runs without Keystone (the pairing rule on CinderSpec keeps the endpoint and
// the service user together), so there is no password to roll the pods on.
//
// Both the selected store and the credential Secrets are read on the children
// cluster: they are materialised beside the workload that consumes them.
func (r *CinderReconciler) reconcileSecrets(ctx context.Context, children client.Client,
	cinder *cinderv1alpha1.Cinder,
) (ctrl.Result, string, error) {
	// Check the selected secret store first so upstream backend outages surface
	// as SecretsReady=False even while per-ExternalSecret caches still report
	// Ready=True from their last successful sync. The store is the one this
	// Cinder selected via spec.secretStoreRef (default: the shared cluster-scoped
	// openbao-cluster-store); a namespaced store is resolved in the Cinder's own
	// namespace.
	storeReady, err := secrets.GateStoreReady(ctx, children,
		secrets.EffectiveStoreRef(cinder.Spec.SecretStoreRef), cinder.Namespace,
		&cinder.Status.Conditions, cinder.Generation, "SecretsReady")
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
	credentialGates := []secrets.CredentialGateSpec{
		{
			Key:          client.ObjectKey{Namespace: cinder.Namespace, Name: cinder.Spec.Database.SecretRef.Name},
			Reason:       "WaitingForDBCredentials",
			Noun:         "Database credentials",
			WaitingMsg:   "Waiting for ESO to sync database credentials from OpenBao",
			ExpectedKeys: []string{"username", "password"},
		},
	}
	if cinder.Spec.ServiceUser != nil {
		credentialGates = append(credentialGates, secrets.CredentialGateSpec{
			Key:          client.ObjectKey{Namespace: cinder.Namespace, Name: cinder.Spec.ServiceUser.SecretRef.Name},
			Reason:       "WaitingForServiceUserCredentials",
			Noun:         "Service-user credentials",
			WaitingMsg:   "Waiting for ESO to sync the Cinder service-user password from OpenBao",
			ExpectedKeys: []string{effectiveServiceUserKey(cinder)},
		})
	}
	ready, err := secrets.GateCredentials(ctx, children, credentialGates,
		&cinder.Status.Conditions, cinder.Generation, "SecretsReady")
	if err != nil {
		return ctrl.Result{}, "", err
	}
	if !ready {
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, "", nil
	}

	// Digest the service-user password so the deployment step can roll pods when
	// it rotates at the OpenBao source.
	digest := ""
	if cinder.Spec.ServiceUser != nil {
		key := client.ObjectKey{Namespace: cinder.Namespace, Name: cinder.Spec.ServiceUser.SecretRef.Name}
		value, verr := secrets.GetSecretValue(ctx, children, key, effectiveServiceUserKey(cinder))
		if verr != nil {
			return ctrl.Result{}, "", fmt.Errorf("reading service-user password value: %w", verr)
		}
		digest = secrets.AdminPasswordDigest(value)
	}

	conditions.SetCondition(&cinder.Status.Conditions, metav1.Condition{
		Type:               "SecretsReady",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: cinder.Generation,
		Reason:             "SecretsAvailable",
	})
	return ctrl.Result{}, digest, nil
}
