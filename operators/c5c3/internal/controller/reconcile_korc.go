// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"reflect"
	"time"

	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/secrets"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
)

// adminAppCredentialNameSuffix is appended to the ControlPlane name to derive
// the deterministic, collision-free name of the owned K-ORC ApplicationCredential
// CR, mirroring the keystoneNameSuffix discipline so a single
// namespace can host the admin AC of multiple ControlPlanes.
const adminAppCredentialNameSuffix = "-admin-app-credential" //nolint:gosec // G101 false positive: name suffix, not a credential.

// adminPasswordHashAnnotation stamps the SHA-256 of the admin password the
// application credential was last minted against onto the owned AC CR. Mirrors the hash+annotation pattern in the keystone operator's
// password-rotation reconciler. A mismatch on a later pass drives a re-mint.
const adminPasswordHashAnnotation = "forge.c5c3.io/admin-password-hash" //nolint:gosec // G101 false positive: annotation key, not a credential.

// adminAppCredentialName returns the deterministic name of the owned K-ORC
// ApplicationCredential CR for the given ControlPlane.
func adminAppCredentialName(cp *c5c3v1alpha1.ControlPlane) string {
	return cp.Name + adminAppCredentialNameSuffix
}

// conditionFailer returns a closure bound to cp and condType that stamps a
// metav1.ConditionFalse status (with cp.Generation as the observed generation)
// onto cp.Status.Conditions. It collapses the ~30 identical
// SetCondition(... ConditionFalse ...) blocks across the K-ORC, admin-credential
// and catalog sub-reconcilers into a single fail(reason, message) call; the
// caller keeps its own (Result, error) return so the requeue-vs-hard-error
// decision stays explicit at each site.
func conditionFailer(cp *c5c3v1alpha1.ControlPlane, condType string) func(reason, message string) {
	return func(reason, message string) {
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               condType,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             reason,
			Message:            message,
		})
	}
}

// reconcileKORC reconciles the K-ORC (OpenStack Resource Controller)
// integration and drives the KORCReady condition.
//
// It create-or-updates an OWNED ApplicationCredential CR that instructs K-ORC to
// mint the admin application credential. The CR maps the ControlPlane's
// AdminCredential spec onto the K-ORC ApplicationCredentialSpec, taking care of
// the Restricted <-> Unrestricted inversion (see below). The AC authenticates via
// the operator-owned password-cloud (ensureAdminPasswordCloud), NOT
// k-orc-clouds-yaml, so it can always re-authenticate as admin even while the
// minted app credential is being revoked.
//
// RE-MINT K-ORC's AC actuator implements only Create + Delete, so a
// rotated admin password cannot re-mint the credential in place. reconcileKORC
// therefore compares the SHA-256 of the current admin password against the
// adminPasswordHashAnnotation stamped on the AC; on a mismatch it DELETES the AC
// (the finalizer revokes the old credential) and regenerates the secret "value"
// (remintAdminApplicationCredential), and the next pass recreates it for a fresh
// mint. The hash compare and the delete+recreate live here, co-located with the
// resource K-ORC reacts to; reconcileAdminCredential only commits/pushes the
// already-(re-)minted secret.
//
// HARD CRD DEPENDENCY: K-ORC (and Memcached, ESO, MariaDB, Keystone) are hard
// dependencies of the ControlPlane operator. SetupWithManager Owns/Watches their
// kinds, so the manager fails fast at startup if any CRD is absent — a missing
// K-ORC CRD never reaches this reconcile path. No-match errors are therefore
// handled by the generic error returns below (manager backoff requeue) rather
// than a dedicated KORCReady=False branch that could only fire if a CRD were
// deleted after the manager had started (#476).
func (r *ControlPlaneReconciler) reconcileKORC(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	fail := conditionFailer(cp, conditionTypeKORCReady)

	adminCred := cp.Spec.KORC.AdminCredential

	// Read the admin password used to (re-)mint the AC. The cleartext is
	// needed both to derive the rotation hash AND to render the password-based
	// clouds.yaml the AC mints with. A read failure (missing Secret/key) is
	// surfaced as KORCReady False with a requeue rather than a hard error so a
	// not-yet-seeded admin password simply defers minting.
	password, err := readAdminPassword(ctx, r.Client, cp)
	if err != nil {
		if secrets.IsMissingSecretOrKey(err) {
			logger.Info("admin password not yet available, deferring K-ORC mint")
			fail("WaitingForAdminPassword", "admin password Secret is not yet available; deferring application-credential mint")
			return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
		}
		fail("AdminPasswordError", fmt.Sprintf("reading admin password: %v", err))
		return ctrl.Result{}, err
	}
	pwHash := secrets.AdminPasswordDigest(password)

	// Read the private-CA bundle an External-mode ControlPlane may reference. It is
	// projected verbatim into BOTH operator-owned credentials Secrets as the inline
	// "cacert" key K-ORC reads natively. Managed mode has no bundle (empty string),
	// so the Secrets stay byte-identical to today. A not-yet-created — or created but
	// not-yet-populated — CA Secret defers exactly like a not-yet-created admin
	// password: minting against an endpoint whose certificate we cannot verify would
	// only fail at K-ORC.
	caBundle, err := readExternalCABundle(ctx, r.Client, cp)
	if err != nil {
		if secrets.IsMissingSecretOrKey(err) {
			logger.Info("external CA bundle not yet available, deferring K-ORC mint")
			fail("WaitingForCABundle",
				"the CA bundle Secret referenced by spec.services.keystone.external.caBundleSecretRef "+
					"is not yet available; deferring application-credential mint")
			return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
		}
		fail("CABundleError", fmt.Sprintf("reading external CA bundle: %v", err))
		return ctrl.Result{}, err
	}

	// Ensure the operator-owned password-based clouds.yaml the AC mints with always
	// tracks the current admin password. This is what breaks the self-referential
	// bootstrap deadlock and lets the delete+recreate re-mint below re-authenticate
	// as admin even while k-orc-clouds-yaml still holds the (about-to-be-revoked)
	// app credential.
	if err := r.ensureAdminPasswordCloud(ctx, cp, password, caBundle); err != nil {
		fail("PasswordCloudError", fmt.Sprintf("ensuring admin password-cloud secret: %v", err))
		return ctrl.Result{}, err
	}

	// restricted defaults to true (the safe least-privilege baseline) when unset,
	// matching the +kubebuilder:default=true marker and the defaulting webhook.
	restricted := true
	if adminCred.ApplicationCredential.Restricted != nil {
		restricted = *adminCred.ApplicationCredential.Restricted
	}

	// importCredRef authenticates the Domain/User imports and the catalog
	// Service/Endpoint: they stay on the spec's CloudCredentialsRef
	// (k-orc-clouds-yaml) and tolerate the brief auth gap during a re-mint by
	// requeueing. acCredRef points the AC itself at the operator-owned
	// password-cloud so a delete+recreate can always re-authenticate (see
	// adminPasswordCloudSecretSuffix).
	importCredRef := orcv1alpha1.CloudCredentialsReference{
		SecretName: adminCred.CloudCredentialsRef.SecretName,
		CloudName:  adminCred.CloudCredentialsRef.CloudName,
	}
	acCredRef := orcv1alpha1.CloudCredentialsReference{
		SecretName: adminPasswordCloudSecretName(cp),
		CloudName:  adminCred.CloudCredentialsRef.CloudName,
	}

	// The admin ApplicationCredential's UserRef points at a K-ORC User that must
	// already exist. The Keystone bootstrap creates the real admin user in the
	// Default domain, so import it (and its domain) as UNMANAGED K-ORC resources
	// before minting — otherwise the AC blocks forever on "Waiting for User/admin
	// to be created".
	imports, err := r.ensureKORCAdminImports(ctx, cp, importCredRef)
	if err != nil {
		fail("AdminImportError", fmt.Sprintf("ensuring K-ORC admin User/Domain imports: %v", err))
		return ctrl.Result{}, err
	}
	importMsg := imports.statusFragment()

	// K-ORC's managed ApplicationCredential reads the DESIRED secret from
	// Secret.Data["value"] and passes it to Keystone when creating the credential
	// (it does NOT generate or write the secret itself). So the operator-owned
	// Secret MUST exist with a generated "value" BEFORE the AC is reconciled —
	// otherwise the AC blocks on "Waiting for Secret … to be created".
	if err := r.ensureAppCredentialSecret(ctx, cp, caBundle); err != nil {
		fail("SecretError", fmt.Sprintf("ensuring application-credential secret: %v", err))
		return ctrl.Result{}, err
	}

	// Seed the bootstrap clouds.yaml, mirror it to OpenBao, and create the per-CR
	// ExternalSecret that reads it back — all BEFORE the AC is minted, so the
	// AdminCredentialReady chicken-and-egg gate opens on a fresh cluster without any
	// external shell seed (//). The seed is
	// write-if-empty, so once the credential is minted these become no-ops that never
	// clobber the minted clouds.yaml.
	//
	// DECISION (placement): the issue says "after ensureAdminPasswordCloud and
	// ensureAppCredentialSecret AND before ensureKORCAdminImports", but the real call
	// order is ensureAdminPasswordCloud -> ensureKORCAdminImports ->
	// ensureAppCredentialSecret, so those two constraints are inconsistent. Chose to
	// insert the three steps immediately AFTER ensureAppCredentialSecret (so the seed
	// updates the very Secret that call just created/owns rather than racing two
	// CreateOrUpdate passes over it) and BEFORE the re-mint/CreateOrUpdate AC
	// decision. The "before the imports" intent is moot: K-ORC retries authentication
	// asynchronously, so the relative order within one synchronous pass does not
	// change convergence. Reviewer: please verify.
	if err := r.seedBootstrapCloudsYAML(ctx, cp, password); err != nil {
		fail("SeedCloudsYamlError", fmt.Sprintf("seeding bootstrap clouds.yaml: %v", err))
		return ctrl.Result{}, err
	}
	if err := secrets.EnsurePushSecret(ctx, r.Client, r.Scheme, cp, adminAppCredentialPushSecret(cp)); err != nil {
		fail("PushSecretError", fmt.Sprintf("ensuring admin app-credential PushSecret: %v", err))
		return ctrl.Result{}, err
	}
	// Nudge the push BEFORE the ExternalSecret below declares a read-back for the
	// "cacert" property, because only a completed push creates that property.
	// ensureAppCredentialSecret just wrote the key into the source Secret, but ESO's
	// PushSecret controller does NOT watch its source Secret — it re-pushes only on a
	// change to the PushSecret's own metadata hash. Without this stamp the key sits
	// unpushed until the PushSecret's refreshInterval (ESO default 1h) while the
	// ExternalSecret, resolving a property the remote key does not carry, reports
	// Ready=False and wedges reconcileAdminCredential on WaitingForCloudsYaml —
	// blaming the clouds.yaml Secret for an un-pushed CA bundle. That sub-reconciler's
	// own re-push cannot break the wedge: it sits BEHIND the very ExternalSecret gate
	// this read-back is about to close. The trigger covers the empty bundle too, so
	// removing the ref re-pushes a Secret without the key (see caCertPushTrigger).
	if err := r.forceRepushAdminAppCredential(ctx, cp, adminAppCredentialPushSecretName(cp),
		adminAppCredentialCACertHashAnnotation, caCertPushTrigger(caBundle)); err != nil {
		fail("PushSecretError", fmt.Sprintf("forcing CA bundle re-push: %v", err))
		return ctrl.Result{}, err
	}
	if err := r.ensureKORCCloudsYAMLExternalSecret(ctx, cp, caBundle); err != nil {
		fail("ExternalSecretError", fmt.Sprintf("ensuring k-orc clouds.yaml ExternalSecret: %v", err))
		return ctrl.Result{}, err
	}

	// Decide between steady-state convergence and a re-mint BEFORE the
	// CreateOrUpdate: K-ORC's ApplicationCredential actuator only implements
	// Create + Delete (no in-place re-mint), so the only way to a fresh Keystone
	// credential on a password rotation is to delete the AC (finalizer revokes the
	// old credential) and let the next pass recreate it. A hash mismatch — a stale
	// stamped hash after a password rotation, or the empty annotation the
	// CredentialRotation reconciler writes to nudge — is the re-mint signal.
	acKey := types.NamespacedName{Name: adminAppCredentialName(cp), Namespace: childNamespace(cp)}
	existing := &orcv1alpha1.ApplicationCredential{}
	switch getErr := r.Get(ctx, acKey, existing); {
	case getErr == nil:
		if existing.Annotations[adminPasswordHashAnnotation] != pwHash {
			return r.remintAdminApplicationCredential(ctx, cp, existing)
		}
		// K-ORC declares spec.resource immutable (CEL self == oldSelf), so an
		// in-place CreateOrUpdate of a changed restricted/accessRules block is
		// rejected on every pass — a permanent KORCReady=False loop that only a
		// manual AC deletion clears. Route a legal, webhook-admitted change to those
		// fields through the same delete+recreate re-mint instead of the update.
		if adminACResourceDrifted(existing, cp, restricted) {
			return r.remintAdminApplicationCredential(ctx, cp, existing)
		}
		// Hash matches and the resource block is unchanged: fall through to the
		// idempotent CreateOrUpdate below, which converges the spec without
		// re-minting (no-op when nothing changed).
	case apierrors.IsNotFound(getErr):
		// First mint, or the recreate after a re-mint delete: CreateOrUpdate below.
	default:
		fail("ApplicationCredentialError", fmt.Sprintf("reading ApplicationCredential: %v", getErr))
		return ctrl.Result{}, getErr
	}

	ac := &orcv1alpha1.ApplicationCredential{
		ObjectMeta: metav1.ObjectMeta{
			Name:      adminAppCredentialName(cp),
			Namespace: childNamespace(cp),
		},
	}

	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, ac, func() error {
		ac.Spec.ManagementPolicy = orcv1alpha1.ManagementPolicyManaged
		ac.Spec.CloudCredentialsRef = acCredRef

		if ac.Spec.Resource == nil {
			ac.Spec.Resource = &orcv1alpha1.ApplicationCredentialResourceSpec{}
		}
		// CRITICAL INVERSION our spec is Restricted; K-ORC's field is
		// Unrestricted. restricted=true => Unrestricted=false (and vice versa).
		ac.Spec.Resource.Unrestricted = ptr.To(!restricted)
		ac.Spec.Resource.UserRef = orcv1alpha1.KubernetesNameRef(adminUserRef(cp))
		ac.Spec.Resource.SecretRef = orcv1alpha1.KubernetesNameRef(adminAppCredentialSecretName(cp))
		ac.Spec.Resource.AccessRules = projectAccessRules(adminCred.ApplicationCredential.AccessRules)

		// Stamp the password hash this credential was minted against — but ONLY on a
		// fresh mint or when the annotation is absent (see shouldStampPasswordHash).
		// A present-but-empty value is the CredentialRotation reconciler's re-mint
		// nudge marker; if the top-level re-mint decision above read the matching
		// hash and fell through while a concurrent nudge zeroed the annotation,
		// re-stamping pwHash here would silently overwrite the marker without a
		// re-mint (the lost-rotation race). Preserving the empty marker lets the
		// NEXT pass's re-mint decision observe the mismatch and re-mint.
		if ac.Annotations == nil {
			ac.Annotations = map[string]string{}
		}
		if shouldStampPasswordHash(ac) {
			ac.Annotations[adminPasswordHashAnnotation] = pwHash
		}

		return controllerutil.SetControllerReference(cp, ac, r.Scheme)
	})
	if err != nil {
		fail("ApplicationCredentialError", fmt.Sprintf("create-or-update ApplicationCredential: %v", err))
		return ctrl.Result{}, err
	}
	if op != controllerutil.OperationResultNone {
		logger.Info("ensured K-ORC ApplicationCredential", "name", ac.Name, "operation", op)
	}

	// Reflect the AC CR's observed state into status on every pass. The
	// ID is populated by K-ORC once the credential is minted; Restricted is the
	// inverse of the K-ORC-reported Unrestricted (falling back to the desired
	// value while status is empty). LastRotation is stamped on a fresh mint/re-mint.
	r.updateAdminApplicationCredentialStatus(cp, ac, restricted)

	// EXTERNAL-MODE FAILURE CLASSIFICATION. K-ORC collapses every hard failure
	// against a pre-existing Keystone — a wrong admin password (401), an
	// unresolvable authURL, a private CA it does not trust, a region/endpointType
	// absent from the catalog — into a NON-terminal Progressing condition with
	// reason=TransientError. Nothing in the observed inventory is terminal, so
	// neither GetTerminalError nor the reason discriminates: the failure class only
	// survives in the free-text message. Classify on message substrings and relay
	// K-ORC's message VERBATIM, so an operator can tell "fix the Secret" from "fix
	// DNS" from "add the CA bundle" straight off `kubectl describe`.
	//
	// Gated on External mode so a managed CR's KORCReady reasons stay byte-identical:
	// in managed mode the same transient message during bootstrap is a legitimate
	// wait, not a misconfiguration.
	if cp.IsExternalKeystone() {
		if result, handled := r.classifyExternalKORCState(cp, imports, ac); handled {
			return result, nil
		}
	}

	// Surface a TERMINAL K-ORC failure distinctly: GetTerminalError is non-nil only
	// when the AC's Progressing condition reports an unrecoverable/invalid-config
	// reason (e.g. K-ORC cannot authenticate with the clouds.yaml). Without this the
	// AC would report KORCReady=False/WaitingForApplicationCredential forever even
	// though it will never converge — keeping on-call MTTR high. Fold the admin
	// Domain/User import status into the message so the stuck dependency is named.
	if termErr := orcv1alpha1.GetTerminalError(ac); termErr != nil {
		logger.Info("ApplicationCredential reported a terminal error", "name", ac.Name, "error", termErr)
		message := fmt.Sprintf("ApplicationCredential %q failed terminally: %v", ac.Name, termErr)
		if importMsg != "" {
			message += "; " + importMsg
		}
		fail("ApplicationCredentialFailed", message)
		return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
	}

	// Gate KORCReady on the AC CR reporting Available=True. K-ORC uses the
	// "Available" condition (not "Ready") to signal a usable resource; while it
	// converges, requeue with KORCReady=False. Fold the admin Domain/User import
	// status into the message so an import stuck on "created externally" (the
	// documented endpoint/clouds.yaml failure class) points at the real dependency
	// instead of an opaque eternal wait.
	if !orcv1alpha1.IsAvailable(ac) {
		logger.Info("ApplicationCredential not yet Available, requeuing", "name", ac.Name)
		message := fmt.Sprintf("ApplicationCredential %q is not yet Available", ac.Name)
		if importMsg != "" {
			message += "; " + importMsg
		}
		fail("WaitingForApplicationCredential", message)
		return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
	}

	conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditionTypeKORCReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: cp.Generation,
		Reason:             "ApplicationCredentialMinted",
		Message:            "K-ORC admin application credential is minted and available",
	})
	return ctrl.Result{}, nil
}

// classifyExternalKORCState maps an External-mode K-ORC failure onto a specific
// KORCReady reason, returning handled=false when the generic (mode-agnostic)
// branches in reconcileKORC should decide instead.
//
// It only ever fires while the ApplicationCredential is NOT usable — not Available,
// or carrying a terminal error. A healthy, Available credential is never
// re-classified: K-ORC leaves the message of the last transient attempt on the
// Progressing condition, and classifying that would flip a converged ControlPlane
// to AuthenticationFailed on a message it has already recovered from.
//
// Precedence:
//
//  1. A classifiable message on the admin Domain, the admin User, or the AC (in
//     that dependency order) wins, and K-ORC's message is relayed verbatim.
//  2. Otherwise, an import stuck past externalImportStallGrace on "waiting to be
//     created externally" is the silent-empty hazard — every External-mode import
//     target pre-exists, so the wait never ends on its own.
//  3. Otherwise the caller's generic branches apply.
func (r *ControlPlaneReconciler) classifyExternalKORCState(
	cp *c5c3v1alpha1.ControlPlane, imports korcAdminImports, ac *orcv1alpha1.ApplicationCredential,
) (ctrl.Result, bool) {
	if orcv1alpha1.IsAvailable(ac) && orcv1alpha1.GetTerminalError(ac) == nil {
		return ctrl.Result{}, false
	}
	fail := conditionFailer(cp, conditionTypeKORCReady)
	authURL := externalKeystoneAuthURL(cp)

	objs := append(imports.objects(), ac)
	if reason, rawMessage := classifyExternalKORCFailure(objs...); reason != "" {
		// Announce credential drift loudly — and exactly once per transition into
		// the drifted state, not on every 10s requeue. Read the reason BEFORE fail()
		// overwrites it: on this path no earlier fail() has fired, so the condition
		// still carries the previous pass's reason.
		if isCredentialDriftReason(reason) {
			if prev := conditions.GetCondition(cp.Status.Conditions, conditionTypeKORCReady); prev == nil || !isCredentialDriftReason(prev.Reason) {
				r.Recorder.Event(cp, "Warning", conditionReasonCredentialDrift, fmt.Sprintf(
					"the external Keystone at %s no longer accepts the admin credential derived from Secret %q; "+
						"the operator does not remediate the external installation: %s",
					authURL, cp.Spec.KORC.AdminCredential.PasswordSecretRef.Name, rawMessage,
				))
			}
		}
		fail(reason, fmt.Sprintf("external Keystone at %s: %s", authURL, rawMessage))
		return ctrl.Result{RequeueAfter: korcRequeueAfter}, true
	}

	if stuck, ok := imports.stalledImport(externalImportStallGrace); ok {
		fail(conditionReasonImportStalled, fmt.Sprintf(
			"admin import %s has been waiting to be created externally in %s for longer than %s; "+
				"in External mode the import target already exists, so this is a misconfiguration — "+
				"check spec.services.keystone.external.endpointType and spec.region",
			stuck, authURL, externalImportStallGrace,
		))
		return ctrl.Result{RequeueAfter: korcRequeueAfter}, true
	}

	return ctrl.Result{}, false
}

// remintAdminApplicationCredential drives the actual re-mint when the stamped
// password hash no longer matches the current admin password. K-ORC's AC actuator
// has no in-place re-mint, so it DELETES the AC (the finalizer revokes the old
// Keystone credential, authenticating via the operator-owned password-cloud) and
// regenerates the app-credential secret "value" so the recreated AC mints a
// brand-new credential. The next reconcileKORC pass observes the now-absent AC and
// recreates it via CreateOrUpdate.
//
// While the old AC is Terminating it reports KORCReady=False/ReMinting, escalating
// to ReMintStalled once it has been deleting longer than remintStallTimeout — a
// stuck finalizer (e.g. K-ORC cannot reach Keystone to revoke) otherwise loops on
// ReMinting forever with no operator-visible signal.
func (r *ControlPlaneReconciler) remintAdminApplicationCredential(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane, ac *orcv1alpha1.ApplicationCredential,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	fail := conditionFailer(cp, conditionTypeKORCReady)

	// Already Terminating: wait for K-ORC's finalizer to revoke + remove the AC
	// before the next pass recreates it. Escalate to ReMintStalled past the timeout.
	if !ac.DeletionTimestamp.IsZero() {
		reason := "ReMinting"
		message := fmt.Sprintf("re-minting admin application credential %q; awaiting revoke of the previous credential", ac.Name)
		if time.Since(ac.DeletionTimestamp.Time) > remintStallTimeout {
			reason = "ReMintStalled"
			message = fmt.Sprintf("admin application credential %q has been Terminating longer than %s; "+
				"K-ORC may be unable to revoke the previous Keystone credential", ac.Name, remintStallTimeout)
			logger.Info("admin application credential re-mint stalled",
				"name", ac.Name, "terminatingSince", ac.DeletionTimestamp.Time)
		}
		fail(reason, message)
		return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
	}

	// Trigger the re-mint: delete the AC, then regenerate the secret "value" so the
	// recreated AC mints a fresh credential (a NotFound on delete is benign — the
	// AC is already gone, the recreate happens next pass).
	if err := r.Delete(ctx, ac); err != nil && !apierrors.IsNotFound(err) {
		fail("ApplicationCredentialError", fmt.Sprintf("deleting ApplicationCredential for re-mint: %v", err))
		return ctrl.Result{}, err
	}

	if err := r.regenerateAppCredentialSecretValue(ctx, cp); err != nil {
		fail("SecretError", fmt.Sprintf("regenerating application-credential secret value for re-mint: %v", err))
		return ctrl.Result{}, err
	}

	logger.Info("deleted admin application credential to trigger re-mint", "name", ac.Name)
	fail("ReMinting", fmt.Sprintf("deleted admin application credential %q; a fresh credential will be minted from the rotated admin password", ac.Name))
	return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
}

// projectAccessRules maps our AccessRule{Service,Method,Path} list onto K-ORC's
// ApplicationCredentialAccessRule list. K-ORC models the service as a serviceRef
// (a reference to an ORC Service CR named after the service type) and the method
// as a typed HTTPMethod enum; path is a plain string pointer.
func projectAccessRules(rules []c5c3v1alpha1.AccessRule) []orcv1alpha1.ApplicationCredentialAccessRule {
	if len(rules) == 0 {
		return nil
	}
	out := make([]orcv1alpha1.ApplicationCredentialAccessRule, 0, len(rules))
	for _, rule := range rules {
		projected := orcv1alpha1.ApplicationCredentialAccessRule{}
		if rule.Path != "" {
			projected.Path = ptr.To(rule.Path)
		}
		if rule.Method != "" {
			method := orcv1alpha1.HTTPMethod(rule.Method)
			projected.Method = &method
		}
		// DECISION (AccessRule.Service): K-ORC takes a serviceRef (KubernetesNameRef
		// to an ORC Service CR), not a free-form service-type string. Per the
		// vendored K-ORC actuator (internal/controllers/applicationcredential/
		// actuator.go) K-ORC resolves serviceRef to an EXISTING Service CR by
		// metadata.name and uses that Service's Status.Resource.Type as the OpenStack
		// access-rule service. We pass rule.Service verbatim as that CR name, so a
		// site using access rules MUST provision a K-ORC Service CR whose
		// metadata.name == rule.Service (e.g. a Service named "identity"). NOTE: this
		// is NOT the catalog Service reconcileCatalog registers — that one is named
		// keystoneServiceName(cp) = "{cp.Name}-identity-service" (type "identity"),
		// so it does not satisfy a rule.Service of "identity" by name. AccessRules are
		// unused on the default/E2E path (the list is empty), so this does not affect
		// the headline credential chain. Reviewer: please verify the intended
		// rule.Service → Service-CR-name convention on a live cluster.
		if rule.Service != "" {
			ref := orcv1alpha1.KubernetesNameRef(rule.Service)
			projected.ServiceRef = &ref
		}
		out = append(out, projected)
	}
	return out
}

// adminACResourceDrifted reports whether the immutable K-ORC spec.resource block on
// the existing admin ApplicationCredential differs from what reconcileKORC would
// project for the current ControlPlane spec. K-ORC declares the whole resource block
// immutable (CEL self == oldSelf), so any drift in the operator-managed fields
// cannot be reconciled by an in-place update — it must be a delete+recreate re-mint.
//
// It compares ONLY the fields reconcileKORC sets (Unrestricted, UserRef, SecretRef,
// AccessRules), never the whole struct: K-ORC and CRD defaulting may populate other
// sub-fields (Name, RoleRefs, …) that a whole-struct compare would read as permanent
// drift, which would itself become the re-mint loop this issue fixes. A nil resource
// block is NOT drift — the CEL self==oldSelf rule does not fire on the initial
// unset→set transition, so the idempotent CreateOrUpdate is free to populate it
// (this is also the never-minted fixture state).
func adminACResourceDrifted(existing *orcv1alpha1.ApplicationCredential, cp *c5c3v1alpha1.ControlPlane, restricted bool) bool {
	res := existing.Spec.Resource
	if res == nil {
		return false
	}
	if res.Unrestricted == nil || *res.Unrestricted != !restricted {
		return true
	}
	if res.UserRef != orcv1alpha1.KubernetesNameRef(adminUserRef(cp)) {
		return true
	}
	if res.SecretRef != orcv1alpha1.KubernetesNameRef(adminAppCredentialSecretName(cp)) {
		return true
	}
	return accessRulesDrifted(res.AccessRules,
		projectAccessRules(cp.Spec.KORC.AdminCredential.ApplicationCredential.AccessRules))
}

// accessRulesDrifted reports whether two projected access-rule lists differ on the
// operator-managed sub-fields only (Path, Method, ServiceRef). It deliberately does
// NOT whole-struct DeepEqual the rules so a K-ORC/CRD-defaulted sub-field can never
// register as permanent drift.
func accessRulesDrifted(existing, desired []orcv1alpha1.ApplicationCredentialAccessRule) bool {
	if len(existing) != len(desired) {
		return true
	}
	for i := range desired {
		if !reflect.DeepEqual(existing[i].Path, desired[i].Path) ||
			!reflect.DeepEqual(existing[i].Method, desired[i].Method) ||
			!reflect.DeepEqual(existing[i].ServiceRef, desired[i].ServiceRef) {
			return true
		}
	}
	return false
}

// shouldStampPasswordHash reports whether reconcileKORC's CreateOrUpdate may
// (re-)stamp the password-hash annotation on the AC. It stamps on a fresh mint
// (zero CreationTimestamp) or when the annotation key is absent, but NEVER when the
// key is present — even with an empty value. A present-but-empty value is the
// CredentialRotation reconciler's re-mint nudge marker; overwriting it would
// silently consume the nudge without a re-mint (the lost-rotation race). A present
// non-empty value can only equal the current hash on this path (a stale hash would
// have been caught by the re-mint decision before CreateOrUpdate runs), so leaving
// it untouched is also a no-op for the steady state.
func shouldStampPasswordHash(ac *orcv1alpha1.ApplicationCredential) bool {
	if ac.CreationTimestamp.IsZero() {
		return true
	}
	_, present := ac.Annotations[adminPasswordHashAnnotation]
	return !present
}

// updateAdminApplicationCredentialStatus reflects the observed AC CR into
// cp.Status.AdminApplicationCredential. LastRotation is
// (re-)stamped to now whenever the recorded credential ID changes (initial mint
// or re-mint), so a rotation is observable from status; once the ID is stable it
// is preserved across reconciles.
func (r *ControlPlaneReconciler) updateAdminApplicationCredentialStatus(
	cp *c5c3v1alpha1.ControlPlane, ac *orcv1alpha1.ApplicationCredential, desiredRestricted bool,
) {
	var id string
	if ac.Status.ID != nil {
		id = *ac.Status.ID
	}

	restricted := desiredRestricted
	if ac.Status.Resource != nil {
		// K-ORC reports Unrestricted; invert back to our Restricted semantics.
		restricted = !ac.Status.Resource.Unrestricted
	}

	prev := cp.Status.AdminApplicationCredential
	rotated := prev == nil || prev.ID != id

	status := &c5c3v1alpha1.AdminApplicationCredentialStatus{
		ID:         id,
		Restricted: restricted,
	}
	switch {
	case rotated && id != "":
		now := metav1.Now()
		status.LastRotation = &now
	case prev != nil:
		status.LastRotation = prev.LastRotation
	}
	cp.Status.AdminApplicationCredential = status
}
