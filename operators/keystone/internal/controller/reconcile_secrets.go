// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

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

// keystoneOpenBaoFinalizer is the finalizer added to every Keystone CR so that
// the fernet-keys-backup and credential-keys-backup PushSecret CRs are deleted
// before the Keystone CR disappears from etcd. Deletion of those PushSecrets
// drives ESO to purge the corresponding KV-v2 paths in OpenBao via their
// Spec.DeletionPolicy=Delete setting. Defined once as the single source of
// truth for Reconcile, the finalizer handler, tests, and docs (CC-0079,
// REQ-005).
const keystoneOpenBaoFinalizer = "keystone.openstack.c5c3.io/openbao-finalizer"

// esoPushSecretFinalizer is the cleanup finalizer ESO installs on every
// PushSecret it adopts. Its presence is how the operator confirms that ESO's
// workqueue has picked up the PushSecret — without that confirmation a race
// window exists where the operator could Delete the PushSecret object outright
// before ESO gets a chance to honour Spec.DeletionPolicy=Delete and purge the
// corresponding KV-v2 path, leaking Fernet or credential key material in
// OpenBao. Declared as a single named constant so the handler and its tests
// reference the same string (CC-0091, REQ-007).
const esoPushSecretFinalizer = "pushsecret.externalsecrets.external-secrets.io/finalizer"

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

// openBaoBackupPushSecretNames returns the names of the backup PushSecrets
// the openbao-finalizer must delete before releasing the Keystone CR. Kept as
// a single source of truth so adding a third backup is a one-line change, in
// the spirit of mariaDBResourceCtors (CC-0079, REQ-002).
func openBaoBackupPushSecretNames(keystone *keystonev1alpha1.Keystone) []string {
	return []string{
		fmt.Sprintf("%s-fernet-keys-backup", keystone.Name),
		fmt.Sprintf("%s-credential-keys-backup", keystone.Name),
	}
}

// finalizeOpenBaoSecrets deletes the fernet-keys-backup and
// credential-keys-backup PushSecrets and confirms they are gone from the API
// server before allowing the Keystone CR's OpenBao finalizer to be released.
//
// Runs in three sequential passes over openBaoBackupPushSecretNames:
//  1. Pass 0 (adoption gate): Get each PushSecret. NotFound or
//     DeletionTimestamp already set counts as adopted-and-progressing. For
//     every present, live PushSecret without the ESO cleanup finalizer
//     installed, record SecretsReady=False/Reason=WaitingForESOAdoption and
//     return done=false WITHOUT entering Pass 1. This closes the race where
//     the operator would otherwise Delete a PushSecret before ESO's workqueue
//     picks it up, bypassing Spec.DeletionPolicy=Delete and leaking the
//     KV-v2 path in OpenBao (CC-0091, REQ-001, REQ-003).
//  2. Pass 1: Issue Delete on every backup PushSecret, tolerating NotFound.
//     Firing all Deletes up-front lets ESO's cleanup finalizers run in
//     parallel — a serialised Delete→Get loop doubles the worst-case
//     deletion window when both objects are held Terminating by ESO
//     (CC-0079, REQ-002).
//  3. Pass 2: Get each PushSecret. On the first one still present (typically
//     Terminating behind ESO's cleanup finalizer) record the
//     OpenBaoFinalizerBlocked condition and return done=false so the Keystone
//     CR stays alive for the next reconcile. Return done=true only when every
//     Get returns NotFound.
//
// NotFound on either Delete or Get is tolerated as success for idempotency —
// a repeated Delete against an already-terminating object is also a no-op.
// Non-NotFound errors propagate so controller-runtime retries with backoff
// (CC-0079, CC-0091, REQ-001, REQ-002, REQ-003, REQ-004).
func (r *KeystoneReconciler) finalizeOpenBaoSecrets(
	ctx context.Context,
	keystone *keystonev1alpha1.Keystone,
) (done bool, err error) {
	logger := log.FromContext(ctx).WithValues(
		"keystone", client.ObjectKeyFromObject(keystone),
	)

	names := openBaoBackupPushSecretNames(keystone)

	// Pass 0: adoption gate. Block Pass 1 until every present, live
	// PushSecret carries ESO's cleanup finalizer, so Delete is guaranteed
	// to route through ESO's DeletionPolicy=Delete handler rather than
	// removing the object outright (CC-0091, REQ-001, REQ-003).
	for _, name := range names {
		key := client.ObjectKey{Namespace: keystone.Namespace, Name: name}
		ps := &esov1alpha1.PushSecret{}
		getErr := r.Get(ctx, key, ps)
		if apierrors.IsNotFound(getErr) {
			continue
		}
		if getErr != nil {
			return false, fmt.Errorf("getting PushSecret %s: %w", key, getErr)
		}
		// A PushSecret already Terminating counts as adopted — ESO has
		// necessarily finished picking it up before its own controllers
		// (or another actor) flipped DeletionTimestamp.
		if !ps.GetDeletionTimestamp().IsZero() {
			continue
		}
		if !hasESOFinalizer(ps) {
			setOpenBaoWaitingForESOAdoptionCondition(keystone, name)
			logger.V(1).Info("openbao finalizer waiting for ESO to adopt PushSecret",
				"pushsecret", key)
			return false, nil
		}
	}

	// Pass 1: issue Delete on every backup PushSecret so ESO's cleanup
	// finalizers fire in parallel (CC-0079, REQ-002).
	for _, name := range names {
		key := client.ObjectKey{Namespace: keystone.Namespace, Name: name}
		ps := &esov1alpha1.PushSecret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: keystone.Namespace},
		}
		if delErr := r.Delete(ctx, ps); delErr != nil {
			if !apierrors.IsNotFound(delErr) {
				return false, fmt.Errorf("deleting PushSecret %s: %w", key, delErr)
			}
			logger.V(1).Info("openbao backup PushSecret already absent, skipping delete",
				"pushsecret", key)
		}
	}

	// Pass 2: confirm each PushSecret is gone. Returning on the first
	// still-present object is sufficient — the blocked condition is recorded
	// once per pass and the subsequent requeue re-enters this function to
	// re-check the remaining names (CC-0079, CC-0091, REQ-004).
	for _, name := range names {
		key := client.ObjectKey{Namespace: keystone.Namespace, Name: name}
		getErr := r.Get(ctx, key, &esov1alpha1.PushSecret{})
		if apierrors.IsNotFound(getErr) {
			continue
		}
		if getErr != nil {
			return false, fmt.Errorf("getting PushSecret %s: %w", key, getErr)
		}

		// PushSecret still present — likely Terminating behind ESO's cleanup
		// finalizer. Record the blocked condition, log which PushSecret is
		// holding up release, and requeue (REQ-004).
		setOpenBaoFinalizerBlockedCondition(keystone, name)
		logger.V(1).Info("openbao finalizer blocked on PushSecret garbage collection",
			"pushsecret", name)
		return false, nil
	}

	return true, nil
}

// hasESOFinalizer reports whether ps carries ESO's cleanup finalizer. Used by
// the openbao-finalizer adoption check to confirm that ESO has picked up the
// PushSecret before the operator issues Delete, closing the race that would
// otherwise remove the object outright and leak the KV-v2 path in OpenBao
// (CC-0091, REQ-001, REQ-007).
func hasESOFinalizer(ps *esov1alpha1.PushSecret) bool {
	for _, f := range ps.Finalizers {
		if f == esoPushSecretFinalizer {
			return true
		}
	}
	return false
}

// setOpenBaoFinalizerBlockedCondition records that the openbao finalizer is
// waiting on a backup PushSecret to finish garbage collection. Lifted into a
// helper to keep finalizeOpenBaoSecrets narrow (CC-0079, REQ-004). The message
// follows the shared `PushSecret %q: <state>` template used across the two
// openbao-finalizer wait reasons so operators can scan both reasons with a
// single eye pattern (CC-0091).
func setOpenBaoFinalizerBlockedCondition(keystone *keystonev1alpha1.Keystone, stuckName string) {
	conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
		Type:               "SecretsReady",
		Status:             metav1.ConditionFalse,
		ObservedGeneration: keystone.Generation,
		Reason:             "OpenBaoFinalizerBlocked",
		Message: fmt.Sprintf(
			"PushSecret %q: waiting for ESO-driven garbage collection to release openbao-finalizer",
			stuckName),
	})
}

// setOpenBaoWaitingForESOAdoptionCondition records that the openbao finalizer
// is blocked BEFORE issuing Delete because ESO has not yet installed its
// cleanup finalizer on the named PushSecret. Distinct from the post-Delete
// OpenBaoFinalizerBlocked reason so an SRE reading `kubectl describe keystone`
// can tell a pre-Delete ESO workqueue backlog apart from a post-Delete remote
// DeleteSecret wait — the two have different remediations (CC-0091, REQ-002).
// The message follows the shared `PushSecret %q: <state>` template used by
// setOpenBaoFinalizerBlockedCondition so the two openbao-finalizer wait
// reasons are visually aligned in logs and `kubectl describe` output.
func setOpenBaoWaitingForESOAdoptionCondition(keystone *keystonev1alpha1.Keystone, unadoptedName string) {
	conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
		Type:               "SecretsReady",
		Status:             metav1.ConditionFalse,
		ObservedGeneration: keystone.Generation,
		Reason:             "WaitingForESOAdoption",
		Message: fmt.Sprintf(
			"PushSecret %q: waiting for ESO adoption (cleanup finalizer not yet installed)",
			unadoptedName),
	})
}
