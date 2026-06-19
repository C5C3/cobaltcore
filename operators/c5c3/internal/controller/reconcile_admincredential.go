// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/yaml"

	"github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/secrets"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
)

// reconcileAdminCredential commits the minted application credential into an
// operator-owned Secret and mirrors it to OpenBao, driving the
// AdminCredentialReady condition.
//
// It is GATED on KORCReady: until reconcileKORC reports the AC minted there is
// nothing to push. It is additionally gated on the K-ORC clouds.yaml
// ExternalSecret being Ready in the ControlPlane's OWN namespace
// (childNamespace(cp)/<CloudCredentialsRef.SecretName>) — co-located with the
// K-ORC CRs per C1 — so the credential is never published before K-ORC can
// authenticate with its admin cloud.
func (r *ControlPlaneReconciler) reconcileAdminCredential(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	fail := conditionFailer(cp, conditionTypeAdminCredentialReady)

	// Gate on KORCReady.
	if !conditions.AllTrue(cp.Status.Conditions, conditionTypeKORCReady) {
		logger.Info("KORC not ready, deferring admin credential push")
		fail("WaitingForKORC", "KORCReady is not True; admin credential push deferred")
		return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
	}

	// Gate on the OpenBao-backed ClusterSecretStore so an ESO/OpenBao outage
	// surfaces as AdminCredentialReady=False promptly. The clouds.yaml
	// ExternalSecret read below would eventually requeue on its own, but ESO only
	// re-syncs at the refreshInterval (default 1h); the ClusterSecretStore watch
	// wakes the ControlPlane the moment ESO flips the store condition, so the
	// credential condition does not stay stale-True through a short outage (#476).
	storeReady, err := secrets.IsClusterSecretStoreReady(ctx, r.Client, openBaoClusterStoreName)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !storeReady {
		logger.Info("ClusterSecretStore not ready, deferring admin credential push")
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeAdminCredentialReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "SecretStoreNotReady",
			Message: fmt.Sprintf("ClusterSecretStore %q is not ready; upstream secret backend unreachable",
				openBaoClusterStoreName),
		})
		return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
	}

	// Gate on the K-ORC clouds.yaml ExternalSecret being Ready. It MUST materialise
	// in the SAME namespace as the K-ORC resource CRs (childNamespace) because
	// K-ORC resolves CloudCredentialsRef in the resource's own namespace (C1). The
	// Secret name follows the spec's CloudCredentialsRef.SecretName — the exact
	// value reconcileKORC sets on the AC CR — defaulted to korcCloudsYamlSecretName
	// by the webhook (the fallback below covers a webhook-bypass edge case).
	cloudsYamlName := cp.Spec.KORC.AdminCredential.CloudCredentialsRef.SecretName
	if cloudsYamlName == "" {
		cloudsYamlName = korcCloudsYamlSecretName
	}
	exists, ready, err := secrets.WaitForExternalSecret(ctx, r.Client,
		types.NamespacedName{Namespace: childNamespace(cp), Name: cloudsYamlName})
	if err != nil {
		fail("CloudsYamlError", fmt.Sprintf("checking k-orc clouds.yaml ExternalSecret: %v", err))
		return ctrl.Result{}, err
	}
	if !ready {
		// k-orc writes this clouds.yaml Secret upstream, so exists is normally
		// true here; a false value is surfaced in the log rather than the
		// user-facing status.
		logger.Info("k-orc clouds.yaml ExternalSecret not ready, requeuing", "exists", exists)
		fail("WaitingForCloudsYaml", "k-orc clouds.yaml ExternalSecret is not yet Ready")
		return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
	}

	// Assemble the application-credential clouds.yaml into the operator-owned Secret
	// so the PushSecret mirrors it to OpenBao (and ESO re-materialises it as the
	// admin clouds.yaml, replacing the password-based bootstrap seed). K-ORC does
	// NOT write this — it only consumed Secret.Data["value"] to mint — so we build
	// it from the minted credential id (AC status, surfaced on cp.Status) and the
	// generated secret value. The "value" key is preserved untouched.
	acID := ""
	if cp.Status.AdminApplicationCredential != nil {
		acID = cp.Status.AdminApplicationCredential.ID
	}
	if acID == "" {
		logger.Info("admin application credential id not yet reported, deferring credential assembly")
		fail("WaitingForCredentialID", "minted application credential id is not yet reported by K-ORC")
		return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
	}

	// The operator-owned Secret (with its generated "value" and owner reference)
	// was created by ensureAppCredentialSecret during reconcileKORC, which KORCReady
	// — gated above — guarantees has run and K-ORC has read the "value" to mint.
	// Get it directly rather than CreateOrUpdate: a NotFound here is an invariant
	// violation (the minted secret vanished), NOT a create opportunity — minting a
	// fresh "value" would not match the credential Keystone already issued, so we
	// requeue and let reconcileKORC re-establish the Secret instead of writing a
	// brand-new empty one that would immediately fail the "value" check.
	secretKey := types.NamespacedName{
		Name:      adminAppCredentialSecretName(cp),
		Namespace: childNamespace(cp),
	}
	secret := &corev1.Secret{}
	if err := r.Get(ctx, secretKey, secret); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("app-credential secret not found, deferring credential assembly", "secret", secretKey.Name)
			fail("WaitingForAppCredentialSecret", fmt.Sprintf("app-credential secret %q does not exist yet", secretKey.Name))
			return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
		}
		fail("SecretError", fmt.Sprintf("reading admin app-credential secret %q: %v", secretKey.Name, err))
		return ctrl.Result{}, err
	}

	value := secret.Data[appCredSecretValueKey]
	if len(value) == 0 {
		logger.Info("app-credential secret has no value yet, deferring credential assembly", "secret", secretKey.Name)
		fail("WaitingForAppCredentialSecret", fmt.Sprintf("app-credential secret %q has no %q key (mint not complete?)",
			secret.Name, appCredSecretValueKey))
		return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
	}

	// Persist the assembled clouds.yaml under appCredCloudsYAMLKey, leaving the
	// "value" key untouched. Skip the write when it already matches so repeated
	// reconciles do not churn the Secret (and wake ESO to re-push).
	cloudsYAML := []byte(buildAppCredCloudsYAML(cp, acID, string(value)))
	if !bytes.Equal(secret.Data[appCredCloudsYAMLKey], cloudsYAML) {
		secret.Data[appCredCloudsYAMLKey] = cloudsYAML
		if err := r.Update(ctx, secret); err != nil {
			fail("SecretError", fmt.Sprintf("assembling admin app-credential clouds.yaml: %v", err))
			return ctrl.Result{}, err
		}
	}

	// CLOBBER-SAFE PushSecret: EnsurePushSecret applies via Server-Side Apply
	// under a fixed field manager that owns only the fields the operator sets, so
	// repeated applies of an unchanged desired Spec are no-ops at the API server.
	// Reconciles therefore do not churn the PushSecret, so ESO is not woken to
	// re-push an unchanged credential.
	ps := adminAppCredentialPushSecret(cp)
	if err := secrets.EnsurePushSecret(ctx, r.Client, r.Scheme, cp, ps); err != nil {
		fail("PushSecretError", fmt.Sprintf("ensuring admin app-credential PushSecret: %v", err))
		return ctrl.Result{}, err
	}

	// Gate AdminCredentialReady on the PushSecret actually syncing to OpenBao — not
	// merely on the CR existing. Otherwise a backend permission failure (e.g. the
	// ESO role missing the push-app-credentials policy) yields a false-positive
	// Ready while OpenBao still serves the password-based bootstrap clouds.yaml.
	pushed := &esov1alpha1.PushSecret{}
	if err := r.Get(ctx, types.NamespacedName{Name: ps.Name, Namespace: ps.Namespace}, pushed); err != nil {
		fail("PushSecretError", fmt.Sprintf("reading admin app-credential PushSecret: %v", err))
		return ctrl.Result{}, err
	}
	if !pushSecretReady(pushed) {
		logger.Info("admin app-credential PushSecret not yet synced, requeuing")
		fail("WaitingForPushSecret", "admin app-credential PushSecret has not synced to OpenBao yet")
		return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
	}

	// Close the post-re-mint stale-credential window. A re-mint revokes the old
	// Keystone credential immediately, but the k-orc-clouds-yaml Secret only
	// refreshes from OpenBao at the ExternalSecret's hourly refreshInterval, so the
	// PushSecret-Ready gate above can pass while the materialized Secret K-ORC
	// actually authenticates with still holds the revoked credential — up to ~2h of
	// auth against a dead credential while AdminCredentialReady reads True.
	//
	// Force an immediate ESO re-sync (a force-sync annotation keyed by the assembled
	// content hash — idempotent, so a steady-state pass does not churn the
	// ExternalSecret) and gate AdminCredentialReady on the materialized Secret bytes
	// actually matching the freshly assembled clouds.yaml. The byte-compare — not the
	// best-effort force-sync — is the correctness guarantee: it never reports Ready
	// while the materialized credential is stale.
	contentSum := sha256.Sum256(cloudsYAML)
	contentHash := hex.EncodeToString(contentSum[:])
	if err := r.forceSyncKORCCloudsYAMLExternalSecret(ctx, cp, cloudsYamlName, contentHash); err != nil {
		fail("CloudsYamlError", fmt.Sprintf("forcing k-orc clouds.yaml ExternalSecret re-sync: %v", err))
		return ctrl.Result{}, err
	}

	materialized := &corev1.Secret{}
	matKey := types.NamespacedName{Namespace: childNamespace(cp), Name: cloudsYamlName}
	if err := r.Get(ctx, matKey, materialized); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("materialized k-orc clouds.yaml Secret not present yet, requeuing")
			fail("WaitingForCloudsYamlSync",
				"k-orc clouds.yaml Secret has not yet materialized the assembled application credential")
			return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
		}
		fail("CloudsYamlError", fmt.Sprintf("reading materialized k-orc clouds.yaml Secret: %v", err))
		return ctrl.Result{}, err
	}
	// Compare SEMANTICALLY, not byte-for-byte: a bare bytes.Equal would pin
	// AdminCredentialReady at False forever if ESO/OpenBao ever re-serialised the
	// value once (a stripped trailing newline, reordered keys, changed quoting),
	// even though K-ORC parses the YAML and the credential is functionally
	// identical. Comparing the parsed application-credential id+secret keeps the
	// stale-credential guarantee (a revoked credential never reads Ready) without a
	// brittle byte gate that a benign normalisation could wedge permanently.
	if !sameAppCredIdentity(materialized.Data[appCredCloudsYAMLKey], cloudsYAML) {
		// Bound the wait so a never-converging sync is distinguishable from a
		// 2-second transient miss. LastRotation marks when the current credential id
		// was (re-)minted, so a materialised Secret that still does not carry it
		// after cloudsYamlSyncStuckTimeout means ESO/OpenBao is not going to agree on
		// its own; escalate from the transient WaitingForCloudsYamlSync to the
		// alertable CloudsYamlSyncStuck so on-call gets a terminal, bounded signal.
		reason := "WaitingForCloudsYamlSync"
		message := "materialized k-orc clouds.yaml does not yet match the assembled application credential; awaiting ESO re-sync"
		var rotatedAt *metav1.Time
		if cp.Status.AdminApplicationCredential != nil {
			rotatedAt = cp.Status.AdminApplicationCredential.LastRotation
		}
		if rotatedAt != nil && time.Since(rotatedAt.Time) > cloudsYamlSyncStuckTimeout {
			reason = "CloudsYamlSyncStuck"
			message = fmt.Sprintf("materialized k-orc clouds.yaml has not matched the assembled application credential "+
				"for over %s; the ESO ExternalSecret or OpenBao backend may be unable to sync — manual intervention required",
				cloudsYamlSyncStuckTimeout)
			logger.Info("materialized k-orc clouds.yaml sync is stuck", "rotatedAt", rotatedAt.Time, "timeout", cloudsYamlSyncStuckTimeout)
		} else {
			logger.Info("materialized k-orc clouds.yaml is stale, requeuing for ESO re-sync")
		}
		fail(reason, message)
		return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
	}

	conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditionTypeAdminCredentialReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: cp.Generation,
		Reason:             "AdminCredentialReady",
		Message:            "Admin application credential committed and mirrored to OpenBao",
	})
	return ctrl.Result{}, nil
}

// pushSecretReady reports whether an ESO PushSecret has synced to its backend
// (its "Ready" condition is True).
func pushSecretReady(ps *esov1alpha1.PushSecret) bool {
	for _, c := range ps.Status.Conditions {
		if c.Type == esov1alpha1.PushSecretReady && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// appCredIdentity is the credential-identifying subset of an assembled
// app-credential clouds.yaml: the application credential id and secret. The
// materialized (ESO) and freshly-assembled documents are compared on these parsed
// fields rather than byte-for-byte (see reconcileAdminCredential).
type appCredIdentity struct {
	id     string
	secret string
}

// sameAppCredIdentity reports whether two assembled app-credential clouds.yaml
// documents carry the same application-credential id and secret. A parse failure
// or a missing id/secret on either side is treated as NOT matching, so the
// stale-credential gate never reports a credential fresh against malformed,
// empty, or password-based (non-app-credential) input.
func sameAppCredIdentity(materialized, assembled []byte) bool {
	m, mok := parseAppCredIdentity(materialized)
	a, aok := parseAppCredIdentity(assembled)
	if !mok || !aok {
		return false
	}
	return m.id == a.id && m.secret == a.secret
}

// parseAppCredIdentity extracts the application-credential id and secret from the
// single cloud entry of an assembled clouds.yaml. ok is false when the document
// does not parse or no cloud carries both an id and a secret (e.g. a password-based
// bootstrap document), so callers treat such input as a non-match.
func parseAppCredIdentity(cloudsYAML []byte) (appCredIdentity, bool) {
	if len(cloudsYAML) == 0 {
		return appCredIdentity{}, false
	}
	var doc struct {
		Clouds map[string]struct {
			Auth struct {
				ApplicationCredentialID     string `json:"application_credential_id"`
				ApplicationCredentialSecret string `json:"application_credential_secret"`
			} `json:"auth"`
		} `json:"clouds"`
	}
	if err := yaml.Unmarshal(cloudsYAML, &doc); err != nil {
		return appCredIdentity{}, false
	}
	for _, cloud := range doc.Clouds {
		if cloud.Auth.ApplicationCredentialID != "" && cloud.Auth.ApplicationCredentialSecret != "" {
			return appCredIdentity{id: cloud.Auth.ApplicationCredentialID, secret: cloud.Auth.ApplicationCredentialSecret}, true
		}
	}
	return appCredIdentity{}, false
}

// forceSyncKORCCloudsYAMLExternalSecret nudges ESO to re-materialise the K-ORC
// clouds.yaml Secret immediately rather than at the next hourly refresh, by
// stamping the external-secrets.io/force-sync annotation with the content hash of
// the freshly assembled clouds.yaml. ESO folds the ExternalSecret's annotations
// into its sync-decision hash, so a changed value forces a re-sync; an unchanged
// value is a no-op, so a steady-state pass does not churn the ExternalSecret.
//
// A missing ExternalSecret is treated as a no-op nil — reconcileKORC owns its
// creation (ensureKORCCloudsYAMLExternalSecret), and the byte-compare gate in
// reconcileAdminCredential, not this nudge, is what guarantees the materialized
// credential is fresh before AdminCredentialReady flips True.
func (r *ControlPlaneReconciler) forceSyncKORCCloudsYAMLExternalSecret(ctx context.Context, cp *c5c3v1alpha1.ControlPlane, name, hash string) error {
	key := types.NamespacedName{Namespace: childNamespace(cp), Name: name}
	// Read-modify-write the force-sync annotation under RetryOnConflict: ESO mutates
	// this ExternalSecret's status and its own annotations on every refresh (and on
	// the force-sync it triggers), so a 409 Conflict between our Get and Update is
	// expected concurrency — NOT a clouds.yaml fault. Re-reading and retrying keeps a
	// transient conflict from flipping AdminCredentialReady to False/CloudsYamlError
	// (and incrementing the sub-reconciler error counter) for what self-heals on the
	// very next attempt.
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		es := &esov1.ExternalSecret{}
		if err := r.Get(ctx, key, es); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		if es.Annotations[esov1.AnnotationForceSync] == hash {
			return nil
		}
		if es.Annotations == nil {
			es.Annotations = map[string]string{}
		}
		es.Annotations[esov1.AnnotationForceSync] = hash
		return r.Update(ctx, es)
	}); err != nil {
		return fmt.Errorf("forcing k-orc clouds.yaml ExternalSecret %q re-sync: %w", name, err)
	}
	return nil
}
