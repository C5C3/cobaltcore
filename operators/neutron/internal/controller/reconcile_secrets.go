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
	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
)

// effectiveServiceUserKey returns the Secret data key holding the service-user
// password, defaulting to "password" when spec.serviceUser.secretRef.key is
// empty (a CR that bypassed the defaulting webhook).
func effectiveServiceUserKey(neutron *neutronv1alpha1.Neutron) string {
	if key := neutron.Spec.ServiceUser.SecretRef.Key; key != "" {
		return key
	}
	return "password"
}

// effectiveNovaNotifierKey returns the Secret data key holding the Nova
// notifier password, defaulting to "password" when spec.nova.serviceUser
// .secretRef.key is empty (a CR that bypassed the defaulting webhook). It reads
// the block, so every caller guards on spec.nova being set.
func effectiveNovaNotifierKey(neutron *neutronv1alpha1.Neutron) string {
	if key := neutron.Spec.Nova.ServiceUser.SecretRef.Key; key != "" {
		return key
	}
	return "password"
}

// reconcileSecrets checks that the ESO-provided Kubernetes Secrets exist before
// proceeding and returns the SHA-256 digests of the two passwords the pods read
// from their environment: the service user's, and, while spec.nova is set, the
// Nova notifier's (empty otherwise). It gates on the selected secret store
// first, then the database credentials, the service-user credentials and, while
// spec.nova is set, the Nova notifier credentials, maintaining SecretsReady. The
// deployment step stamps each digest into a pod-template annotation of its own
// so a rotated password rolls the Neutron pods — the passwords are
// env-var-consumed (oslo.config OS_KEYSTONE_AUTHTOKEN__PASSWORD and
// OS_NOVA__PASSWORD), not volume-mounted, so they only take effect on a Pod
// restart — and the annotation that changed names the password that rotated.
//
// Both the selected store and the credential Secrets are read on the children
// cluster: they are materialised beside the workload that consumes them.
func (r *NeutronReconciler) reconcileSecrets(ctx context.Context, children client.Client,
	neutron *neutronv1alpha1.Neutron,
) (res ctrl.Result, authtokenDigest, novaNotifierDigest string, err error) {
	// Check the selected secret store first so upstream backend outages surface
	// as SecretsReady=False even while per-ExternalSecret caches still report
	// Ready=True from their last successful sync. The store is the one this
	// Neutron selected via spec.secretStoreRef (default: the shared
	// cluster-scoped openbao-cluster-store); a namespaced store is resolved in the
	// Neutron's own namespace.
	storeReady, err := secrets.GateStoreReady(ctx, children,
		secrets.EffectiveStoreRef(neutron.Spec.SecretStoreRef), neutron.Namespace,
		&neutron.Status.Conditions, neutron.Generation, "SecretsReady")
	if err != nil {
		return ctrl.Result{}, "", "", err
	}
	if !storeReady {
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, "", "", nil
	}

	// Validate the credential Secrets from a declarative (secretRef, expectedKeys)
	// list. Each check reads the materialized Secret first (the steady-state fast
	// path) and only consults the ExternalSecret to build a precise
	// SecretsReady=False message when the Secret is not yet usable.
	serviceUserKey := effectiveServiceUserKey(neutron)
	credentialGates := []secrets.CredentialGateSpec{
		{
			Key:          client.ObjectKey{Namespace: neutron.Namespace, Name: neutron.Spec.Database.SecretRef.Name},
			Reason:       "WaitingForDBCredentials",
			Noun:         "Database credentials",
			WaitingMsg:   "Waiting for ESO to sync database credentials from OpenBao",
			ExpectedKeys: []string{"username", "password"},
		},
		{
			Key:          client.ObjectKey{Namespace: neutron.Namespace, Name: neutron.Spec.ServiceUser.SecretRef.Name},
			Reason:       "WaitingForServiceUserCredentials",
			Noun:         "Service-user credentials",
			WaitingMsg:   "Waiting for ESO to sync the Neutron service-user password from OpenBao",
			ExpectedKeys: []string{serviceUserKey},
		},
	}
	if nova := neutron.Spec.Nova; nova != nil {
		credentialGates = append(credentialGates, secrets.CredentialGateSpec{
			Key:          client.ObjectKey{Namespace: neutron.Namespace, Name: nova.ServiceUser.SecretRef.Name},
			Reason:       "WaitingForNovaNotifierCredentials",
			Noun:         "Nova notifier credentials",
			WaitingMsg:   "Waiting for ESO to sync the Neutron-to-Nova notifier password from OpenBao",
			ExpectedKeys: []string{effectiveNovaNotifierKey(neutron)},
		})
	}
	ready, err := secrets.GateCredentials(ctx, children, credentialGates,
		&neutron.Status.Conditions, neutron.Generation, "SecretsReady")
	if err != nil {
		return ctrl.Result{}, "", "", err
	}
	if !ready {
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, "", "", nil
	}

	// Digest the service-user password so the deployment step can roll pods when
	// it rotates at the OpenBao source.
	key := client.ObjectKey{Namespace: neutron.Namespace, Name: neutron.Spec.ServiceUser.SecretRef.Name}
	value, err := secrets.GetSecretValue(ctx, children, key, serviceUserKey)
	if err != nil {
		return ctrl.Result{}, "", "", fmt.Errorf("reading service-user password value: %w", err)
	}
	// And the notifier password while spec.nova is set, so rotating it rolls the
	// pods too. A CR without the block returns no notifier digest, which leaves
	// its pod template without the annotation.
	if nova := neutron.Spec.Nova; nova != nil {
		novaKey := client.ObjectKey{Namespace: neutron.Namespace, Name: nova.ServiceUser.SecretRef.Name}
		notifierPassword, err := secrets.GetSecretValue(ctx, children, novaKey, effectiveNovaNotifierKey(neutron))
		if err != nil {
			return ctrl.Result{}, "", "", fmt.Errorf("reading the nova notifier password value: %w", err)
		}
		novaNotifierDigest = secrets.AdminPasswordDigest(notifierPassword)
	}

	conditions.SetCondition(&neutron.Status.Conditions, metav1.Condition{
		Type:               "SecretsReady",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: neutron.Generation,
		Reason:             "SecretsAvailable",
	})
	return ctrl.Result{}, secrets.AdminPasswordDigest(value), novaNotifierDigest, nil
}
