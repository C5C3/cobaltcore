// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"time"

	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/secrets"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// openBaoClusterStoreName re-exports the shared ClusterSecretStore name (see
// secrets.OpenBaoClusterStoreName) for the watches and tests in this package.
const openBaoClusterStoreName = secrets.OpenBaoClusterStoreName

// keystoneOpenBaoFinalizer is the finalizer added to every Keystone CR so that
// the fernet-keys-backup and credential-keys-backup PushSecret CRs are deleted
// before the Keystone CR disappears from etcd. Deletion of those PushSecrets
// drives ESO to purge the corresponding KV-v2 paths in OpenBao via their
// Spec.DeletionPolicy=Delete setting. Defined once as the single source of
// truth for Reconcile, the finalizer handler, tests, and docs.
const keystoneOpenBaoFinalizer = "keystone.openstack.c5c3.io/openbao-finalizer"

// esoPushSecretFinalizer is the finalizer ESO installs on a PushSecret when it
// adopts (begins managing) the object. Its presence is the Pass-0 *adoption*
// signal — NOT a remote-cleanup marker: finalizeOpenBaoSecrets requires it on
// each backup PushSecret before issuing Delete, because a Delete that races
// ahead of ESO's first reconcile would remove the PushSecret outright and
// leave the referenced KV-v2 path orphaned in OpenBao. This
// is the literal hasESOFinalizer checks and the only ESO finalizer production
// code branches on. The pinned ESO version's use of this exact string is
// asserted by the deletion-cleanup e2e suite, so an upstream rename fails CI
// loudly instead of hanging CR deletion at WaitingForESOAdoption (issue #475).
// Declared once as the single source of truth for the handler and tests.
const esoPushSecretFinalizer = "pushsecret.externalsecrets.io/finalizer"

// esoCleanupFinalizer is the *remote-purge* finalizer external-secrets uses
// while it deletes the kv-v2 path for a PushSecret with
// spec.deletionPolicy=Delete. Production code never adds, removes, or branches
// on it; it is declared here only so the tests can simulate ESO holding a
// PushSecret Terminating during remote cleanup, using one identical string in
// both unit tests (default build) and integration tests (//go:build
// integration) rather than hard-coding the literal twice.
const esoCleanupFinalizer = "external-secrets.io/cleanup"

// reconcileSecrets checks that ESO-provided Kubernetes Secrets exist before
// proceeding. It verifies the DB credentials and admin credentials
// ExternalSecrets are ready.
func (r *KeystoneReconciler) reconcileSecrets(ctx context.Context,
	keystone *keystonev1alpha1.Keystone,
) (ctrl.Result, error) {
	// Check the ClusterSecretStore first so upstream backend outages surface
	// as SecretsReady=False even while per-ExternalSecret caches still report
	// Ready=True from their last successful sync.
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

	// Validate the credential Secrets from a declarative (secretRef,
	// expectedKeys) list. Each check reads the materialized Secret first (the
	// steady-state fast path) and only consults the ExternalSecret to build a
	// precise SecretsReady=False message when the Secret is not yet usable
	// (secrets.GateSyncedSecret). On a not-ready check the helper sets the
	// condition and the caller requeues.
	credentialGates := []struct {
		key          client.ObjectKey
		reason       string
		noun         string
		waitingMsg   string
		expectedKeys []string
	}{
		{
			key:          client.ObjectKey{Namespace: keystone.Namespace, Name: keystone.Spec.Database.SecretRef.Name},
			reason:       "WaitingForDBCredentials",
			noun:         "Database credentials",
			waitingMsg:   "Waiting for ESO to sync database credentials from OpenBao",
			expectedKeys: []string{"username", "password"},
		},
		{
			key:          client.ObjectKey{Namespace: keystone.Namespace, Name: keystone.Spec.Bootstrap.AdminPasswordSecretRef.Name},
			reason:       "WaitingForAdminCredentials",
			noun:         "Admin credentials",
			waitingMsg:   "Waiting for ESO to sync admin credentials from OpenBao",
			expectedKeys: []string{"password"},
		},
	}
	for _, gate := range credentialGates {
		if ready, err := r.checkCredentialSecret(ctx, keystone, gate.key,
			gate.reason, gate.noun, gate.waitingMsg, gate.expectedKeys...); err != nil {
			return ctrl.Result{}, err
		} else if !ready {
			return ctrl.Result{RequeueAfter: RequeueSecretPolling}, nil
		}
	}

	conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
		Type:               "SecretsReady",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: keystone.Generation,
		Reason:             "SecretsAvailable",
	})
	return ctrl.Result{}, nil
}

// checkCredentialSecret verifies that a credential Secret is usable, checking
// the materialized Secret before the ExternalSecret to save a read in steady
// state. It returns (true, nil) when the Secret exists with all requiredKeys.
// On a miss it consults the ExternalSecret only to produce a precise
// SecretsReady=False message — "ExternalSecret not found yet" vs "waiting for
// ESO to sync" vs "missing expected keys" — sets the condition itself with the
// given reason, and returns (false, nil).
//
// Semantic note: because the materialized Secret is checked first, an
// ExternalSecret whose Ready condition is momentarily False while the Secret
// still holds valid keys no longer flips SecretsReady=False. That matches how
// pods consume the Secret directly; the ClusterSecretStore check in
// reconcileSecrets remains the authoritative backend-outage detector.
func (r *KeystoneReconciler) checkCredentialSecret(
	ctx context.Context,
	keystone *keystonev1alpha1.Keystone,
	key client.ObjectKey,
	reason, noun, waitingMsg string,
	requiredKeys ...string,
) (bool, error) {
	state, err := secrets.GateSyncedSecret(ctx, r.Client, key, requiredKeys...)
	if err != nil {
		return false, err
	}
	if state == secrets.GateReady {
		return true, nil
	}

	// The materialized Secret is absent or missing keys; the gate state
	// attributes the cause so the operator surfaces an actionable message.
	msg := waitingMsg
	switch state {
	case secrets.GateExternalSecretMissing:
		msg = fmt.Sprintf("%s ExternalSecret %s/%s not found yet", noun, key.Namespace, key.Name)
	case secrets.GateSecretKeysMissing:
		msg = fmt.Sprintf("%s Secret exists but is missing expected keys", noun)
	case secrets.GateExternalSecretNotSynced, secrets.GateReady:
		// NotSynced keeps the generic waiting message; Ready returned above.
	}
	conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
		Type:               "SecretsReady",
		Status:             metav1.ConditionFalse,
		ObservedGeneration: keystone.Generation,
		Reason:             reason,
		Message:            msg,
	})
	return false, nil
}

// openBaoBackupPushSecretNames returns the names of the backup PushSecrets
// the openbao-finalizer must delete before releasing the Keystone CR. Kept as
// a single source of truth so adding a third backup is a one-line change, in
// the spirit of mariaDBResourceCtors.
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
//
//  0. Adoption wait. For each still-live (non-Terminating) PushSecret, require
//     ESO's cleanup finalizer before issuing Delete. Without this gate the
//     operator's Delete can race ahead of ESO's first reconcile — the
//     PushSecret object would be removed from the API server outright, ESO
//     would never observe a DeletionTimestamp, and the referenced kv-v2 path
//     in OpenBao would be orphaned. On the first unadopted PushSecret record
//     WaitingForESOAdoption and return done=false WITHOUT firing any Delete
//
// The wait is bounded by
//
//	   OpenBaoAdoptionWaitTimeout: past that deadline an unadopted PushSecret no
//	   longer blocks — Pass-1 force-deletes it after an ESOAdoptionTimedOut
//	   Warning — so a renamed/absent ESO finalizer cannot hang CR deletion
//	   forever (issue #475).
//	1. Issue Delete on every backup PushSecret, tolerating NotFound. Firing all
//	   Deletes up-front lets ESO's cleanup finalizers run in parallel — a
//	   serialised Delete→Get loop doubles the worst-case deletion window when
//	   both objects are held Terminating by ESO.
//	2. Get each PushSecret. On the first one still present (typically
//	   Terminating behind ESO's cleanup finalizer) record the
//	   OpenBaoFinalizerBlocked condition and return done=false so the Keystone
//	   CR stays alive for the next reconcile. Return done=true only when every
//	   Get returns NotFound.
//
// NotFound on Get or Delete is tolerated as success for idempotency — a
// repeated Delete against an already-terminating object is also a no-op.
// Non-NotFound errors propagate so controller-runtime retries with backoff
func (r *KeystoneReconciler) finalizeOpenBaoSecrets(
	ctx context.Context,
	keystone *keystonev1alpha1.Keystone,
) (done bool, err error) {
	logger := log.FromContext(ctx).WithValues(
		"keystone", client.ObjectKeyFromObject(keystone),
	)

	names := openBaoBackupPushSecretNames(keystone)

	// Pass 0: adoption wait. For each present PushSecret, require ESO's
	// cleanup finalizer before we issue Delete. If a PushSecret is missing
	// that finalizer, record WaitingForESOAdoption and return without firing
	// any Delete — a racing Delete here would remove the PushSecret object
	// outright and orphan the kv-v2 path in OpenBao.
	//
	// The wait is bounded by OpenBaoAdoptionWaitTimeout (issue #475): once the
	// CR has been deleting longer than that, an unadopted PushSecret stops
	// blocking and Pass-1 force-deletes it (after an ESOAdoptionTimedOut
	// Warning), so an ESO finalizer rename — or ESO being down — cannot hang CR
	// deletion forever. The force-delete is still safe when ESO merely renamed
	// its finalizer: the PushSecret carries that (renamed) finalizer, so Delete
	// only marks it Terminating and ESO still purges the kv-v2 path via it. The
	// kv-v2 path is orphaned only if ESO is genuinely not running during the
	// deletion window — an explicit, event-surfaced trade-off over hanging.
	adoptionDeadlinePassed := !keystone.DeletionTimestamp.IsZero() &&
		time.Since(keystone.DeletionTimestamp.Time) > OpenBaoAdoptionWaitTimeout

	for _, name := range names {
		key := client.ObjectKey{Namespace: keystone.Namespace, Name: name}
		ps := &esov1alpha1.PushSecret{}
		getErr := r.Get(ctx, key, ps)
		if apierrors.IsNotFound(getErr) {
			// Already deleted elsewhere — nothing to adopt, nothing to delete.
			continue
		}
		if getErr != nil {
			return false, fmt.Errorf("getting PushSecret %s for adoption check: %w", key, getErr)
		}
		if !ps.GetDeletionTimestamp().IsZero() {
			// Already Terminating — Pass-0 is irrelevant; let Pass-2 wait on
			// gone. An object in Terminating state has necessarily been
			// through a prior Delete, which means the adoption question was
			// already resolved.
			continue
		}
		if !hasESOFinalizer(ps) {
			if adoptionDeadlinePassed {
				// Bounded wait exceeded — surface the force-delete and break to
				// Pass-1 rather than blocking forever (issue #475).
				r.Recorder.Eventf(keystone, corev1.EventTypeWarning, "ESOAdoptionTimedOut",
					"PushSecret %q not adopted by ESO within %s of deletion; force-deleting "+
						"to release the openbao-finalizer (the OpenBao kv-v2 path may be "+
						"orphaned only if ESO is not running)", name, OpenBaoAdoptionWaitTimeout)
				logger.Info("openbao finalizer adoption wait timed out; proceeding to force-delete",
					"pushsecret", name, "timeout", OpenBaoAdoptionWaitTimeout)
				break
			}
			setOpenBaoWaitingForESOAdoptionCondition(keystone, name)
			logger.V(1).Info("openbao finalizer waiting for ESO adoption",
				"pushsecret", name)
			return false, nil
		}
	}

	// Pass 1: issue Delete on every backup PushSecret so ESO's cleanup
	// finalizers fire in parallel.
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
	// re-check the remaining names.
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
		// holding up release, and requeue.
		setOpenBaoFinalizerBlockedCondition(keystone, name)
		logger.V(1).Info("openbao finalizer blocked on PushSecret garbage collection",
			"pushsecret", name)
		return false, nil
	}

	return true, nil
}

// setOpenBaoFinalizerBlockedCondition records that the openbao finalizer is
// waiting on a backup PushSecret to finish garbage collection. Lifted into a
// helper to keep finalizeOpenBaoSecrets narrow.
func setOpenBaoFinalizerBlockedCondition(keystone *keystonev1alpha1.Keystone, stuckName string) {
	conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
		Type:               "SecretsReady",
		Status:             metav1.ConditionFalse,
		ObservedGeneration: keystone.Generation,
		Reason:             "OpenBaoFinalizerBlocked",
		Message: fmt.Sprintf(
			"Waiting for PushSecret %q to be garbage-collected before releasing openbao-finalizer",
			stuckName,
		),
	})
}

// hasESOFinalizer reports whether the given PushSecret carries ESO's cleanup
// finalizer. Presence of that finalizer is the signal that ESO has adopted
// the PushSecret and will run its DeletionPolicy=Delete branch on Delete —
// without it, a racing operator Delete would orphan the kv-v2 path in OpenBao
func hasESOFinalizer(ps *esov1alpha1.PushSecret) bool {
	for _, f := range ps.Finalizers {
		if f == esoPushSecretFinalizer {
			return true
		}
	}
	return false
}

// setOpenBaoWaitingForESOAdoptionCondition records that the openbao finalizer
// is waiting for ESO to adopt a backup PushSecret (i.e., install its cleanup
// finalizer) before the operator issues Delete. Distinct from
// setOpenBaoFinalizerBlockedCondition so an SRE reading `kubectl describe
// keystone` can tell pre-Delete adoption waits (ESO workqueue backlog) from
// post-Delete gone-waits (remote DeleteSecret in flight) — the two have
// different remediations.
func setOpenBaoWaitingForESOAdoptionCondition(keystone *keystonev1alpha1.Keystone, unadoptedName string) {
	conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
		Type:               "SecretsReady",
		Status:             metav1.ConditionFalse,
		ObservedGeneration: keystone.Generation,
		Reason:             "WaitingForESOAdoption",
		Message: fmt.Sprintf(
			"Waiting for ESO to adopt PushSecret %q (cleanup finalizer not yet installed)",
			unadoptedName,
		),
	})
}
