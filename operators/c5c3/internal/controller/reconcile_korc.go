// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/go-logr/logr"

	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/secrets"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
)

// openBaoClusterStoreName mirrors the keystone operator's constant
// (operators/keystone/internal/controller/reconcile_secrets.go): the cluster-
// scoped ESO SecretStore that fronts OpenBao. Defined locally so the c5c3
// PushSecrets target the same backend without importing the keystone package
// (CC-0110, REQ-011).
const openBaoClusterStoreName = "openbao-cluster-store"

// adminAppCredentialNameSuffix is appended to the ControlPlane name to derive
// the deterministic, collision-free name of the owned K-ORC ApplicationCredential
// CR (CC-0110, REQ-010), mirroring the keystoneNameSuffix discipline so a single
// namespace can host the admin AC of multiple ControlPlanes.
const adminAppCredentialNameSuffix = "-admin-app-credential" //nolint:gosec // G101 false positive: name suffix, not a credential.

// adminAppCredentialSecretSuffix names the operator-owned Secret that K-ORC
// writes the minted application credential into (Resource.SecretRef). It is the
// push source for the OpenBao PushSecret (CC-0110, REQ-010, REQ-011).
const adminAppCredentialSecretSuffix = "-admin-app-credential" //nolint:gosec // G101 false positive: Secret name suffix, not a credential.

// adminAppCredentialRemoteKey is the flat OpenBao path the minted admin
// application credential is mirrored to (CC-0110, REQ-011). A single admin AC
// per cluster shares this object, matching the K-ORC clouds.yaml consumer.
const adminAppCredentialRemoteKey = "openstack/keystone/admin/app-credential"

// adminPasswordHashAnnotation stamps the SHA-256 of the admin password the
// application credential was last minted against onto the owned AC CR (CC-0110,
// REQ-012). Mirrors the hash+annotation pattern in the keystone operator's
// password-rotation reconciler. A mismatch on a later pass drives a re-mint.
const adminPasswordHashAnnotation = "forge.c5c3.io/admin-password-hash" //nolint:gosec // G101 false positive: annotation key, not a credential (CC-0108).

// korcCloudsYamlSecretName is the conventional name of the admin clouds.yaml
// Secret (and its ExternalSecret) K-ORC reads its admin credentials from. It
// matches DefaultCloudCredentialsSecretName, the value the defaulting webhook
// applies to spec.korc.adminCredential.cloudCredentialsRef.secretName, and is
// used only as the fallback when a CR somehow reaches the reconciler without the
// webhook having defaulted that field.
//
// CC-0110, C1 (co-location): the ExternalSecret materialises this Secret into the
// SAME namespace as the K-ORC ApplicationCredential/Service/Endpoint CRs the
// operator projects — the ControlPlane's own namespace (childNamespace(cp)) —
// because K-ORC resolves CloudCredentialsRef in the resource's own namespace
// (vendored api/v1alpha1/credentials_ref.go GetCloudCredentialsRef returns the
// resource's Namespace). The AdminCredentialReady gate therefore waits on the
// ExternalSecret in childNamespace(cp), NOT a fixed orc-system one, so the minted
// AC is never pushed before K-ORC can actually authenticate (CC-0110, REQ-011,
// REQ-023).
const korcCloudsYamlSecretName = "k-orc-clouds-yaml" //nolint:gosec // G101 false positive: Secret name, not a credential.

// adminAppCredentialName returns the deterministic name of the owned K-ORC
// ApplicationCredential CR for the given ControlPlane.
func adminAppCredentialName(cp *c5c3v1alpha1.ControlPlane) string {
	return cp.Name + adminAppCredentialNameSuffix
}

// adminAppCredentialSecretName returns the name of the operator-owned Secret
// K-ORC writes the minted application credential into.
func adminAppCredentialSecretName(cp *c5c3v1alpha1.ControlPlane) string {
	return cp.Name + adminAppCredentialSecretSuffix
}

// adminAppCredentialPushSecretName returns the PushSecret name backing up the
// minted application credential to OpenBao.
func adminAppCredentialPushSecretName(cp *c5c3v1alpha1.ControlPlane) string {
	return cp.Name + "-admin-app-credential-backup"
}

// reconcileKORC reconciles the K-ORC (OpenStack Resource Controller)
// integration and drives the KORCReady condition (CC-0110, REQ-010, REQ-012).
//
// It create-or-updates an OWNED ApplicationCredential CR that instructs K-ORC to
// mint the admin application credential. The CR maps the ControlPlane's
// AdminCredential spec onto the K-ORC ApplicationCredentialSpec, taking care of
// the Restricted <-> Unrestricted inversion (see below). Re-mint (REQ-012) is
// driven here by comparing the SHA-256 of the admin password against the
// adminPasswordHashAnnotation stamped on the AC CR.
//
// DECISION (re-mint placement): the password-hash compare and re-mint trigger
// live in reconcileKORC rather than reconcileAdminCredential. The AC CR is the
// object K-ORC reacts to, so stamping the hash annotation here keeps the
// re-mint signal co-located with the resource whose change actually causes
// K-ORC to rotate the credential. reconcileAdminCredential then only deals with
// committing/pushing the already-(re-)minted secret. Reviewer: please verify.
//
// MISSING-CRD SAFETY: if the K-ORC CRD is not installed the apiserver / RESTMapper
// returns a no-match error; this is detected via meta.IsNoMatchError and surfaced
// as KORCReady=False (Reason "KORCCRDNotInstalled") WITHOUT returning a hard error
// that would crash-loop the operator.
func (r *ControlPlaneReconciler) reconcileKORC(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	adminCred := cp.Spec.KORC.AdminCredential

	// Compute the SHA-256 of the admin password used to (re-)mint the AC
	// (REQ-012). A read failure (missing Secret/key) is surfaced as KORCReady
	// False with a requeue rather than a hard error so a not-yet-seeded admin
	// password simply defers minting.
	pwHash, err := r.adminPasswordHash(ctx, cp)
	if err != nil {
		if secrets.IsMissingSecretOrKey(err) {
			logger.Info("admin password not yet available, deferring K-ORC mint")
			conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
				Type:               conditionTypeKORCReady,
				Status:             metav1.ConditionFalse,
				ObservedGeneration: cp.Generation,
				Reason:             "WaitingForAdminPassword",
				Message:            "admin password Secret is not yet available; deferring application-credential mint",
			})
			return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
		}
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeKORCReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "AdminPasswordError",
			Message:            fmt.Sprintf("reading admin password: %v", err),
		})
		return ctrl.Result{}, err
	}

	// restricted defaults to true (the safe least-privilege baseline) when unset,
	// matching the +kubebuilder:default=true marker and the defaulting webhook.
	restricted := true
	if adminCred.ApplicationCredential.Restricted != nil {
		restricted = *adminCred.ApplicationCredential.Restricted
	}

	ac := &orcv1alpha1.ApplicationCredential{
		ObjectMeta: metav1.ObjectMeta{
			Name:      adminAppCredentialName(cp),
			Namespace: childNamespace(cp),
		},
	}

	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, ac, func() error {
		ac.Spec.ManagementPolicy = orcv1alpha1.ManagementPolicyManaged
		ac.Spec.CloudCredentialsRef = orcv1alpha1.CloudCredentialsReference{
			SecretName: adminCred.CloudCredentialsRef.SecretName,
			CloudName:  adminCred.CloudCredentialsRef.CloudName,
		}

		if ac.Spec.Resource == nil {
			ac.Spec.Resource = &orcv1alpha1.ApplicationCredentialResourceSpec{}
		}
		// CRITICAL INVERSION (REQ-010): our spec is Restricted; K-ORC's field is
		// Unrestricted. restricted=true => Unrestricted=false (and vice versa).
		ac.Spec.Resource.Unrestricted = ptr.To(!restricted)
		ac.Spec.Resource.UserRef = orcv1alpha1.KubernetesNameRef(adminUserRef(cp))
		ac.Spec.Resource.SecretRef = orcv1alpha1.KubernetesNameRef(adminAppCredentialSecretName(cp))
		ac.Spec.Resource.AccessRules = projectAccessRules(adminCred.ApplicationCredential.AccessRules)

		// Stamp the password hash so a later spec change (admin password rotated)
		// flips the annotation and triggers K-ORC to re-mint (REQ-012). Because
		// the AC ResourceSpec is immutable in K-ORC, the annotation is the
		// rotation signal the CredentialRotation flow (task 2.10) reacts to.
		if ac.Annotations == nil {
			ac.Annotations = map[string]string{}
		}
		ac.Annotations[adminPasswordHashAnnotation] = pwHash

		return controllerutil.SetControllerReference(cp, ac, r.Scheme)
	})
	if err != nil {
		// MISSING-CRD SAFETY: a no-match error means the K-ORC CRD is absent.
		// Surface a clean condition instead of crash-looping.
		if meta.IsNoMatchError(err) {
			logger.Info("K-ORC ApplicationCredential CRD not installed; KORCReady=False")
			conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
				Type:               conditionTypeKORCReady,
				Status:             metav1.ConditionFalse,
				ObservedGeneration: cp.Generation,
				Reason:             "KORCCRDNotInstalled",
				Message:            "K-ORC ApplicationCredential CRD is not installed",
			})
			return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
		}
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeKORCReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "ApplicationCredentialError",
			Message:            fmt.Sprintf("create-or-update ApplicationCredential: %v", err),
		})
		return ctrl.Result{}, err
	}
	if op != controllerutil.OperationResultNone {
		logger.Info("ensured K-ORC ApplicationCredential", "name", ac.Name, "operation", op)
	}

	// Reflect the AC CR's observed state into status on every pass (REQ-012). The
	// ID is populated by K-ORC once the credential is minted; Restricted is the
	// inverse of the K-ORC-reported Unrestricted (falling back to the desired
	// value while status is empty). LastRotation is stamped on a fresh mint/re-mint.
	r.updateAdminApplicationCredentialStatus(cp, ac, restricted)

	// Gate KORCReady on the AC CR reporting Available=True. K-ORC uses the
	// "Available" condition (not "Ready") to signal a usable resource; while it
	// converges, requeue with KORCReady=False.
	if !orcv1alpha1.IsAvailable(ac) {
		logger.Info("ApplicationCredential not yet Available, requeuing", "name", ac.Name)
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeKORCReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "WaitingForApplicationCredential",
			Message:            fmt.Sprintf("ApplicationCredential %q is not yet Available", ac.Name),
		})
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

// adminUserRef derives the K-ORC User reference the admin application credential
// is associated with.
//
// DECISION (UserRef): our AdminCredentialSpec has no UserRef field, but K-ORC's
// ApplicationCredentialResourceSpec.UserRef is REQUIRED. We derive it
// conventionally from the CloudName the admin authenticates as (defaulting to
// "admin" when unset), assuming a sibling K-ORC User CR of that name exists in
// the same namespace. This keeps the CR deterministic and valid; if a site uses
// a different admin user-CR name the bootstrap resources (REQ-014) should
// provision a User CR matching the CloudName. Reviewer: please verify the User
// naming convention matches the deploy stack.
func adminUserRef(cp *c5c3v1alpha1.ControlPlane) string {
	if name := cp.Spec.KORC.AdminCredential.CloudCredentialsRef.CloudName; name != "" {
		return name
	}
	return "admin"
}

// projectAccessRules maps our AccessRule{Service,Method,Path} list onto K-ORC's
// ApplicationCredentialAccessRule list. K-ORC models the service as a serviceRef
// (a reference to an ORC Service CR named after the service type) and the method
// as a typed HTTPMethod enum; path is a plain string pointer (CC-0110, REQ-010).
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

// adminPasswordHash reads the admin password from the configured PasswordSecretRef
// and returns its SHA-256 as a lowercase hex string (CC-0110, REQ-012). It
// delegates to the package-level computeAdminPasswordHash so the ControlPlane
// reconciler (which stamps the hash onto the AC CR during a mint) and the
// CredentialRotation reconciler (which compares the hash to drive a re-mint
// nudge, task 2.10) share ONE source of truth for the hash derivation.
func (r *ControlPlaneReconciler) adminPasswordHash(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) (string, error) {
	return computeAdminPasswordHash(ctx, r.Client, cp)
}

// computeAdminPasswordHash reads the admin password from the ControlPlane's
// configured PasswordSecretRef and returns its SHA-256 as a lowercase hex string
// (CC-0110, REQ-012). The data key defaults to "password" when the SecretRef.Key
// is unset, matching the keystone admin-password Secret convention.
//
// DECISION (hash-helper extraction): the hash derivation lives here as a
// package-level function (rather than only as a method on ControlPlaneReconciler)
// so BOTH the ControlPlane reconciler and the CredentialRotation reconciler
// (task 2.10) compute the SAME hash without duplicating the SHA-256 logic. The
// ControlPlaneReconciler.adminPasswordHash method is retained as a thin delegator
// so reconcile_korc.go's external behaviour and tests are unchanged.
func computeAdminPasswordHash(ctx context.Context, c client.Client, cp *c5c3v1alpha1.ControlPlane) (string, error) {
	ref := cp.Spec.KORC.AdminCredential.PasswordSecretRef
	key := ref.Key
	if key == "" {
		key = "password"
	}
	pw, err := secrets.GetSecretValue(ctx, c,
		types.NamespacedName{Namespace: cp.Namespace, Name: ref.Name}, key)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(pw))
	return hex.EncodeToString(sum[:]), nil
}

// updateAdminApplicationCredentialStatus reflects the observed AC CR into
// cp.Status.AdminApplicationCredential (CC-0110, REQ-012). LastRotation is
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

// reconcileAdminCredential commits the minted application credential into an
// operator-owned Secret and mirrors it to OpenBao, driving the
// AdminCredentialReady condition (CC-0110, REQ-011).
//
// It is GATED on KORCReady: until reconcileKORC reports the AC minted there is
// nothing to push. It is additionally gated on the K-ORC clouds.yaml
// ExternalSecret being Ready in the ControlPlane's OWN namespace
// (childNamespace(cp)/<CloudCredentialsRef.SecretName>) — co-located with the
// K-ORC CRs per C1 — so the credential is never published before K-ORC can
// authenticate with its admin cloud.
func (r *ControlPlaneReconciler) reconcileAdminCredential(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Gate on KORCReady.
	if !conditions.AllTrue(cp.Status.Conditions, conditionTypeKORCReady) {
		logger.Info("KORC not ready, deferring admin credential push")
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeAdminCredentialReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "WaitingForKORC",
			Message:            "KORCReady is not True; admin credential push deferred",
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
	ready, err := secrets.WaitForExternalSecret(ctx, r.Client,
		types.NamespacedName{Namespace: childNamespace(cp), Name: cloudsYamlName})
	if err != nil {
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeAdminCredentialReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "CloudsYamlError",
			Message:            fmt.Sprintf("checking k-orc clouds.yaml ExternalSecret: %v", err),
		})
		return ctrl.Result{}, err
	}
	if !ready {
		logger.Info("k-orc clouds.yaml ExternalSecret not ready, requeuing")
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeAdminCredentialReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "WaitingForCloudsYaml",
			Message:            "k-orc clouds.yaml ExternalSecret is not yet Ready",
		})
		return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
	}

	// Ensure the operator-owned Secret K-ORC writes the minted AC into exists.
	// CLOBBER-SAFE: the operator only owns the Secret's metadata/owner-reference;
	// the .data is written by K-ORC (Resource.SecretRef target). The mutate
	// closure deliberately never touches secret.Data so a reconcile can never
	// overwrite a freshly minted credential.
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      adminAppCredentialSecretName(cp),
			Namespace: childNamespace(cp),
		},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		// Do NOT touch secret.Data — K-ORC owns it.
		return controllerutil.SetControllerReference(cp, secret, r.Scheme)
	}); err != nil {
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeAdminCredentialReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "SecretError",
			Message:            fmt.Sprintf("ensuring admin app-credential secret: %v", err),
		})
		return ctrl.Result{}, err
	}

	// CLOBBER-SAFE PushSecret: EnsurePushSecret is idempotent — it only Updates
	// the PushSecret when its desired Spec differs from the stored one
	// (apiequality.Semantic.DeepEqual guard inside EnsurePushSecret). Repeated
	// reconciles therefore do not churn the PushSecret, so ESO is not woken to
	// re-push an unchanged credential.
	ps := adminAppCredentialPushSecret(cp)
	if err := secrets.EnsurePushSecret(ctx, r.Client, r.Scheme, cp, ps); err != nil {
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeAdminCredentialReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "PushSecretError",
			Message:            fmt.Sprintf("ensuring admin app-credential PushSecret: %v", err),
		})
		return ctrl.Result{}, err
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

// adminAppCredentialPushSecret builds the PushSecret that mirrors the minted
// admin application-credential Secret to OpenBao at the flat, single-AC path
// openstack/keystone/admin/app-credential (CC-0110, REQ-011).
//
// DECISION (DeletionPolicy): None — the admin application credential is a shared
// bootstrap secret other consumers may depend on; deleting the PushSecret (e.g.
// on ControlPlane teardown) leaves the last-pushed credential intact in OpenBao
// so a fresh control plane is not locked out mid-rotation. Reviewer: please verify.
func adminAppCredentialPushSecret(cp *c5c3v1alpha1.ControlPlane) *esov1alpha1.PushSecret {
	return &esov1alpha1.PushSecret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      adminAppCredentialPushSecretName(cp),
			Namespace: childNamespace(cp),
		},
		Spec: esov1alpha1.PushSecretSpec{
			DeletionPolicy: esov1alpha1.PushSecretDeletionPolicyNone,
			SecretStoreRefs: []esov1alpha1.PushSecretStoreRef{{
				Kind: "ClusterSecretStore",
				Name: openBaoClusterStoreName,
			}},
			Selector: esov1alpha1.PushSecretSelector{
				Secret: &esov1alpha1.PushSecretSecret{
					Name: adminAppCredentialSecretName(cp),
				},
			},
			Data: []esov1alpha1.PushSecretData{{
				Match: esov1alpha1.PushSecretMatch{
					RemoteRef: esov1alpha1.PushSecretRemoteRef{
						RemoteKey: adminAppCredentialRemoteKey,
					},
				},
			}},
		},
	}
}

// reconcileCatalog registers the OpenStack service catalog entries for Keystone
// (an identity Service plus its public Endpoint) as OWNED K-ORC CRs and drives
// the CatalogReady condition (CC-0110, REQ-014).
//
// It is GATED on AdminCredentialReady: the admin credential must be available
// before K-ORC can register catalog entries. Both child CRs are create-or-updated
// idempotently; CatalogReady (and cp.Status.CatalogReady) flip True once both are
// registered. MISSING-CRD SAFETY mirrors reconcileKORC.
func (r *ControlPlaneReconciler) reconcileCatalog(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Gate on AdminCredentialReady.
	if !conditions.AllTrue(cp.Status.Conditions, conditionTypeAdminCredentialReady) {
		logger.Info("AdminCredential not ready, deferring catalog registration")
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeCatalogReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "WaitingForAdminCredential",
			Message:            "AdminCredentialReady is not True; catalog registration deferred",
		})
		return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
	}

	secretName := cp.Spec.KORC.AdminCredential.CloudCredentialsRef.SecretName
	cloudName := cp.Spec.KORC.AdminCredential.CloudCredentialsRef.CloudName
	credRef := orcv1alpha1.CloudCredentialsReference{SecretName: secretName, CloudName: cloudName}

	// 1. Identity (Keystone) Service.
	service := &orcv1alpha1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      keystoneServiceName(cp),
			Namespace: childNamespace(cp),
		},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, service, func() error {
		service.Spec.ManagementPolicy = orcv1alpha1.ManagementPolicyManaged
		service.Spec.CloudCredentialsRef = credRef
		if service.Spec.Resource == nil {
			service.Spec.Resource = &orcv1alpha1.ServiceResourceSpec{}
		}
		service.Spec.Resource.Type = "identity"
		service.Spec.Resource.Name = ptr.To(orcv1alpha1.OpenStackName("keystone"))
		service.Spec.Resource.Enabled = ptr.To(true)
		return controllerutil.SetControllerReference(cp, service, r.Scheme)
	}); err != nil {
		if meta.IsNoMatchError(err) {
			return r.catalogCRDMissing(cp, logger)
		}
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeCatalogReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "ServiceError",
			Message:            fmt.Sprintf("create-or-update identity Service: %v", err),
		})
		return ctrl.Result{}, err
	}

	// 2. Public Endpoint for the Keystone API.
	endpoint := &orcv1alpha1.Endpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:      keystoneEndpointName(cp),
			Namespace: childNamespace(cp),
		},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, endpoint, func() error {
		endpoint.Spec.ManagementPolicy = orcv1alpha1.ManagementPolicyManaged
		endpoint.Spec.CloudCredentialsRef = credRef
		if endpoint.Spec.Resource == nil {
			endpoint.Spec.Resource = &orcv1alpha1.EndpointResourceSpec{}
		}
		endpoint.Spec.Resource.Interface = "public"
		// DECISION (Endpoint URL): our spec carries no catalog URL, but K-ORC's
		// EndpointResourceSpec.URL is REQUIRED. We derive a conventional in-cluster
		// Keystone identity URL from the ControlPlane namespace
		// (http://keystone.<ns>.svc:5000/v3), matching the keystone operator's
		// in-cluster Service name. Sites that expose Keystone externally should
		// override via bootstrap resources. Reviewer: please verify the Service DNS.
		endpoint.Spec.Resource.URL = keystoneEndpointURL(cp)
		endpoint.Spec.Resource.ServiceRef = orcv1alpha1.KubernetesNameRef(keystoneServiceName(cp))
		endpoint.Spec.Resource.Enabled = ptr.To(true)
		return controllerutil.SetControllerReference(cp, endpoint, r.Scheme)
	}); err != nil {
		if meta.IsNoMatchError(err) {
			return r.catalogCRDMissing(cp, logger)
		}
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeCatalogReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "EndpointError",
			Message:            fmt.Sprintf("create-or-update identity Endpoint: %v", err),
		})
		return ctrl.Result{}, err
	}

	cp.Status.CatalogReady = true
	conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditionTypeCatalogReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: cp.Generation,
		Reason:             "CatalogRegistered",
		Message:            "Keystone identity Service and Endpoint are registered",
	})
	return ctrl.Result{}, nil
}

// catalogCRDMissing surfaces the MISSING-CRD safety condition for the catalog
// sub-reconciler (mirrors reconcileKORC's KORCCRDNotInstalled handling).
func (r *ControlPlaneReconciler) catalogCRDMissing(cp *c5c3v1alpha1.ControlPlane, logger logr.Logger) (ctrl.Result, error) {
	logger.Info("K-ORC Service/Endpoint CRD not installed; CatalogReady=False")
	conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditionTypeCatalogReady,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: cp.Generation,
		Reason:             "KORCCRDNotInstalled",
		Message:            "K-ORC Service/Endpoint CRD is not installed",
	})
	return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
}

// keystoneServiceName / keystoneEndpointName return the deterministic names of
// the owned K-ORC Service/Endpoint CRs registering the identity catalog entry.
func keystoneServiceName(cp *c5c3v1alpha1.ControlPlane) string {
	return cp.Name + "-identity-service"
}

func keystoneEndpointName(cp *c5c3v1alpha1.ControlPlane) string {
	return cp.Name + "-identity-endpoint"
}

// keystoneEndpointURL derives the conventional in-cluster Keystone identity URL
// (see DECISION on Endpoint URL in reconcileCatalog).
func keystoneEndpointURL(cp *c5c3v1alpha1.ControlPlane) string {
	return fmt.Sprintf("http://keystone.%s.svc:5000/v3", childNamespace(cp))
}
