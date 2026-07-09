// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"strconv"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/policy"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// DECISION the projected Keystone CR is named
// "{controlplane.Name}-keystone" — a deterministic, collision-free name derived
// from the owning ControlPlane so a single namespace can host the Keystone CRs
// of multiple ControlPlanes without clashing, and so re-reconciles always target
// the same child. It lives in the ControlPlane's own namespace (childNamespace),
// for the same cross-namespace-owner-reference reason documented on
// childNamespace in reconcile_infrastructure.go.
const keystoneNameSuffix = "-keystone"

// DECISION the default Keystone image repository is
// "ghcr.io/c5c3/keystone" — the canonical repo the keystone operator's own
// fixtures, tempest CRs, and e2e manifests all use (e.g.
// tests/tempest/keystone-2025-2/00-keystone-cr.yaml). The tag is derived from
// spec.openStackRelease unless spec.services.keystone.image overrides the whole
// image reference.
const defaultKeystoneRepository = "ghcr.io/c5c3/keystone"

// defaultFederationProxyRepository is the mod_auth_openidc sidecar image the
// managed ControlPlane path projects onto the child Keystone's
// spec.federation.proxyImage. The image is release-independent (distro
// Apache + module, no OpenStack code), so unlike the keystone image no
// release-derived tag exists — the build publishes :latest and :<sha>, and
// :latest is projected. Operators wanting an immutable pin override via
// spec.services.keystone (or directly on a standalone Keystone CR) with a
// digest-carrying ImageSpec.
const defaultFederationProxyRepository = "ghcr.io/c5c3/keystone-federation-proxy"

// keystoneDeletionAllowedAnnotation, when set to a truthy value on a
// ControlPlane, opts that ControlPlane in to DESTRUCTIVE teardown of a
// previously-projected Keystone child when spec.services.keystone is unset.
// Without it the reconciler PRESERVES the running child (see reconcileKeystone),
// because that child owns irreplaceable state: the <name>-credential-keys Secret
// encrypts every application-credential / EC2 credential / TOTP secret, and
// losing those keys — together with their OpenBao backup, which is purged with
// them via the PushSecret DeletionPolicy=Delete — is permanent (unlike fernet
// keys, whose loss only forces re-authentication). A single YAML edit that drops
// the services.keystone block (e.g. a GitOps template that stops rendering it)
// must therefore not silently destroy that data; deleting the child is opt-in.
const keystoneDeletionAllowedAnnotation = "c5c3.io/allow-keystone-deletion"

// keystoneName returns the deterministic name of the Keystone CR projected from
// the given ControlPlane (see keystoneNameSuffix).
func keystoneName(cp *c5c3v1alpha1.ControlPlane) string {
	return cp.Name + keystoneNameSuffix
}

// keystoneDeletionAllowed reports whether cp opts in to deleting its projected
// Keystone child when spec.services.keystone is unset, via a truthy
// keystoneDeletionAllowedAnnotation. A missing, malformed, or non-truthy value
// means "preserve" — the fail-safe default that protects the child's
// irreplaceable credential/fernet keys.
func keystoneDeletionAllowed(cp *c5c3v1alpha1.ControlPlane) bool {
	allowed, err := strconv.ParseBool(cp.Annotations[keystoneDeletionAllowedAnnotation])
	return err == nil && allowed
}

// reconcileKeystone projects spec.services.keystone into an owned Keystone CR
// and drives the KeystoneReady condition.
//
// The sub-reconciler is GATED on InfrastructureReady: until the infrastructure
// sub-reconciler reports the managed MariaDB/Memcached as Ready, no Keystone CR
// is created (a half-provisioned database would only make Keystone crash-loop).
// Once gated through, it create-or-updates the Keystone CR, pointing it at the
// same backing services the ControlPlane provisioned, and mirrors the child's
// Ready condition back into KeystoneReady.
func (r *ControlPlaneReconciler) reconcileKeystone(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// spec.services.keystone is optional. When unset, this ControlPlane manages
	// no Keystone service and reports KeystoneReady as not-managed so the
	// aggregate Ready condition is not blocked (staged adoption).
	//
	// A previously-projected Keystone child, however, owns irreplaceable state,
	// so tearing it down here is DESTRUCTIVE and IRREVERSIBLE: its
	// <name>-credential-keys Secret (and the OpenBao backup purged with it via the
	// PushSecret DeletionPolicy=Delete) encrypts every application-credential /
	// EC2 credential / TOTP secret, which becomes permanently undecryptable once
	// the keys are gone. A cascade delete on an accidental unset — a single YAML
	// edit, or a GitOps template that stops rendering the block — would silently
	// lose that data. Fail safe: preserve the running child by default and only
	// delete it when the operator explicitly opts in via
	// keystoneDeletionAllowedAnnotation.
	if cp.Spec.Services.Keystone == nil {
		message := "spec.services.keystone is unset; no Keystone service is managed by this ControlPlane"
		if keystoneDeletionAllowed(cp) {
			if err := r.deleteOrphanedKeystone(ctx, cp); err != nil {
				return ctrl.Result{}, err
			}
		} else {
			message += fmt.Sprintf("; any previously-projected Keystone child is preserved to protect its "+
				"credential/fernet keys (set annotation %s=true to allow deletion)", keystoneDeletionAllowedAnnotation)
		}
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeKeystoneReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: cp.Generation,
			Reason:             "KeystoneNotManaged",
			Message:            message,
		})
		return ctrl.Result{}, nil
	}

	// External-mode short-circuit: identity is managed against a pre-existing
	// Keystone, so no child is projected.
	//
	// It also does NOT delete a previously-projected child. A Managed -> External
	// flip is rejected outright by the validating webhook (adopting an existing
	// installation must be a fresh External-mode ControlPlane), so no child can
	// exist here. Were one to appear anyway, the deliberate fail-safe above —
	// preserve the child unless keystoneDeletionAllowedAnnotation opts in — is the
	// only sanctioned teardown path, because the child's credential/fernet keys are
	// irreplaceable.
	//
	// The message embeds authURL, so it is bounded by truncateConditionMessage —
	// see the sibling short-circuit in reconcileInfrastructure for why.
	if cp.IsExternalKeystone() {
		logger.Info("External keystone mode; no Keystone child is projected",
			"authURL", externalKeystoneAuthURL(cp))
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeKeystoneReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: cp.Generation,
			Reason:             conditionReasonExternallyManaged,
			Message: truncateConditionMessage(fmt.Sprintf("External keystone mode: identity is managed against %s; "+
				"no Keystone child is projected", externalKeystoneAuthURL(cp))),
		})
		return ctrl.Result{}, nil
	}

	// Nil-safety fail-safe. This managed projection points the Keystone child at
	// the backing services the ControlPlane provisioned, so a nil block has nothing
	// to project and the derefs below would panic. The validating webhook requires
	// spec.infrastructure outside External mode, and the External branch above has
	// already returned, so this only fires for a webhook-bypassed CR —
	// reconcileInfrastructure halts the same CR with InfrastructureNotConfigured.
	if cp.Spec.Infrastructure == nil {
		return ctrl.Result{RequeueAfter: infraRequeueAfter}, nil
	}

	// Gate on InfrastructureReady.
	if !conditions.AllTrue(cp.Status.Conditions, conditionTypeInfrastructureReady) {
		logger.Info("Infrastructure not ready, deferring Keystone projection")
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeKeystoneReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "WaitingForInfrastructure",
			Message:            "InfrastructureReady is not True; Keystone projection deferred",
		})
		return ctrl.Result{RequeueAfter: keystoneInfraGateRequeueAfter}, nil
	}

	// Resolve the Keystone image. spec.services.keystone.image overrides the
	// release-derived default when set.
	image := commonv1.ImageSpec{
		Repository: defaultKeystoneRepository,
		Tag:        cp.Spec.OpenStackRelease,
	}
	if override := cp.Spec.Services.Keystone.Image; override != nil {
		image = *override
	}

	keystone := &keystonev1alpha1.Keystone{
		ObjectMeta: metav1.ObjectMeta{
			Name:      keystoneName(cp),
			Namespace: childNamespace(cp),
		},
	}

	// Compute the Fernet/CredentialKeys rotation schedule before the mutate
	// closure so a bad rotation interval surfaces a clean condition rather than
	// a partial apply.
	var rotationSchedule string
	if interval := cp.Spec.Services.Keystone.RotationInterval; interval != nil {
		cron, err := intervalToCron(interval.Duration)
		if err != nil {
			conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
				Type:               conditionTypeKeystoneReady,
				Status:             metav1.ConditionFalse,
				ObservedGeneration: cp.Generation,
				Reason:             "InvalidRotationInterval",
				Message:            fmt.Sprintf("invalid keystone rotation interval: %v", err),
			})
			// Return the error so the reconcile chain stops here (the guard in
			// Reconcile keys off err != nil) and the manager requeues with
			// backoff, rather than returning a zero Result that lets the chain
			// continue past this failed sub-reconciler (#476). The webhook now
			// rejects unrepresentable intervals at admission, so this path is
			// defense-in-depth for callers that bypass it.
			return ctrl.Result{}, fmt.Errorf("invalid keystone rotation interval: %w", err)
		}
		rotationSchedule = cron
	}

	merged := policy.MergePolicies(cp.Spec.GlobalPolicyOverrides, cp.Spec.Services.Keystone.PolicyOverrides)

	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, keystone, func() error {
		keystone.Spec.Image = image

		// Project the federation proxy (mod_auth_openidc sidecar) image
		// default so attaching an OIDC KeystoneIdentityBackend works out of
		// the box on the managed path. Inert until a federation backend
		// attaches — the keystone operator projects the sidecar only then.
		keystone.Spec.Federation = &keystonev1alpha1.FederationSpec{
			ProxyImage: &commonv1.ImageSpec{Repository: defaultFederationProxyRepository, Tag: "latest"},
		}

		// Point Keystone at the SAME backing services the ControlPlane
		// provisioned by reusing the infrastructure specs. DeepCopy (over a plain
		// struct copy) is required because DatabaseSpec carries pointer fields
		// (ClusterRef, TLS): a shallow copy would share those pointers with
		// cp.Spec, so the SecretRef override below — or any later mutation of
		// either spec — could alias the ControlPlane's own spec, exactly as the
		// Gateway projection already guards against with DeepCopy (#476).
		keystone.Spec.Database = *cp.Spec.Infrastructure.Database.DeepCopy()

		// in managed mode the operator OWNS the service DB
		// credential — reconcileDBCredentials materialises it into a per-ControlPlane
		// Secret named dbCredentialSecretName(cp). Override the projected Keystone CR's
		// database.secretRef to that operator-owned Secret (key "password") so Keystone
		// consumes the scoped credential rather than the cp-level default name. This
		// reassigns only the projected child's SecretRef value; cp.Spec is left
		// untouched. Brownfield (Database.ClusterRef == nil) leaves the user-supplied
		// secretRef in place — the user owns that Secret out-of-band.
		if cp.Spec.Infrastructure.Database.ClusterRef != nil {
			keystone.Spec.Database.SecretRef = commonv1.SecretRefSpec{Name: dbCredentialSecretName(cp), Key: "password"}
			// Project the EFFECTIVE credentials mode (Dynamic unless the CP opted
			// into Static) so the Keystone operator consumes the engine-issued
			// credential, overriding its webhook's Static-when-empty default. This
			// must match reconcileDBCredentials' effective-mode decision
			// (dbCredentialsDynamicEnabled) or the projected Keystone would read a
			// Secret shaped for the other mode.
			if dbCredentialsDynamicEnabled(cp) {
				keystone.Spec.Database.CredentialsMode = commonv1.CredentialsModeDynamic
			} else {
				keystone.Spec.Database.CredentialsMode = commonv1.CredentialsModeStatic
			}
		}

		// DeepCopy for the same reason as Database above: CacheSpec carries a
		// pointer ClusterRef and a Servers slice, so a shallow copy would alias
		// cp.Spec (#476).
		keystone.Spec.Cache = *cp.Spec.Infrastructure.Cache.DeepCopy()

		// in managed mode the operator OWNS the admin password —
		// reconcileAdminPassword projects it from OpenBao into a per-ControlPlane
		// Secret named adminPasswordSecretName(cp). Point the projected Keystone CR's
		// bootstrap admin-password ref (via effectiveAdminPasswordSecretRef) at that
		// operator-owned Secret (key "password") so Keystone consumes the scoped
		// credential rather than the cp-level default name. This reassigns only the
		// projected child's ref value; cp.Spec is left untouched. Brownfield
		// (Database.ClusterRef == nil) leaves the user-supplied ref in place — the
		// user owns that Secret out-of-band.
		keystone.Spec.Bootstrap.AdminPasswordSecretRef = effectiveAdminPasswordSecretRef(cp)
		keystone.Spec.Bootstrap.Region = cp.Spec.Region

		// Project external exposure onto the Keystone CR's spec.gateway, then
		// advertise the externally routable URL via the bootstrap public endpoint.
		//
		// DECISION both sides are now commonv1.GatewaySpec, so the L2
		// mapping is a single DeepCopy instead of a field-by-field copy. DeepCopy
		// (over a direct pointer share) keeps the projected Keystone CR's gateway an
		// independent object, so a later mutation of either spec can never alias the
		// other. A nil source yields nil (DeepCopy handles a nil receiver), clearing
		// any previously-projected gateway so removal tears the HTTPRoute down and
		// Keystone falls back to its in-cluster DNS.
		keystone.Spec.Gateway = cp.Spec.Services.Keystone.Gateway.DeepCopy()
		keystone.Spec.Bootstrap.PublicEndpoint = keystonePublicEndpoint(cp.Spec.Services.Keystone)

		if cp.Spec.Services.Keystone.Replicas != nil {
			keystone.Spec.Deployment.Replicas = *cp.Spec.Services.Keystone.Replicas
		}

		keystone.Spec.PolicyOverrides = merged

		if rotationSchedule != "" {
			keystone.Spec.Fernet.RotationSchedule = rotationSchedule
			keystone.Spec.CredentialKeys.RotationSchedule = rotationSchedule
		}

		return controllerutil.SetControllerReference(cp, keystone, r.Scheme)
	}); err != nil {
		reason := "KeystoneError"
		message := fmt.Sprintf("create-or-update Keystone: %v", err)
		// An Invalid (HTTP 422) rejection from the Keystone API server is almost
		// always a now-immutable db/bootstrap field whose CEL transition rule
		// (self == oldSelf) refuses the projected change — e.g. a spec.region or
		// spec.database.database edit that landed on the ControlPlane before its
		// own immutability webhook existed, leaving it diverged from the already-
		// frozen Keystone child (#466). validateImmutable cannot catch that
		// pre-webhook edit, so this projection loops forever with no self-heal.
		// Surface a distinct, actionable reason so the wedge is diagnosable from
		// the condition instead of being buried under a generic KeystoneError.
		if apierrors.IsInvalid(err) {
			reason = "KeystoneProjectionRejected"
			message = fmt.Sprintf("Keystone API server rejected the projected spec (likely an immutable db/bootstrap field "+
				"diverged from the frozen Keystone child); reconcile the ControlPlane spec back to the child's values or "+
				"recreate the Keystone child to recover: %v", err)
		}
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeKeystoneReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             reason,
			Message:            message,
		})
		return ctrl.Result{}, err
	}

	// Mirror the child's Ready condition into KeystoneReady.
	if !conditions.IsReady(keystone.Status.Conditions) {
		logger.Info("Keystone CR not ready, requeuing", "keystone", keystone.Name)
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeKeystoneReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "WaitingForKeystone",
			Message:            fmt.Sprintf("Keystone %q is not ready", keystone.Name),
		})
		return ctrl.Result{RequeueAfter: infraRequeueAfter}, nil
	}

	conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditionTypeKeystoneReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: cp.Generation,
		Reason:             "KeystoneReady",
		Message:            "Projected Keystone CR is ready",
	})
	return ctrl.Result{}, nil
}

// keystonePublicEndpoint returns the externally routable Keystone identity URL
// the reconciler projects into the Keystone bootstrap (--bootstrap-public-url)
// and reuses for the K-ORC identity catalog Endpoint (keystoneCatalogURL). An
// explicit publicEndpoint wins; otherwise, when a gateway is set, it is derived
// as "https://{gateway.hostname}/v3" (the default-443 form, matching the
// keystone-operator's own status.endpoint convention). When neither is set it
// returns "" — Keystone then falls back to its in-cluster Service DNS and no
// external URL is advertised.
//
// It takes the *ServiceKeystoneSpec (rather than the ControlPlane) so it
// operates only on the keystone service block. A nil pointer (services.keystone
// unset) yields "" — no external URL is advertised.
func keystonePublicEndpoint(ks *c5c3v1alpha1.ServiceKeystoneSpec) string {
	if ks == nil {
		return ""
	}
	if ks.PublicEndpoint != "" {
		return ks.PublicEndpoint
	}
	if ks.Gateway != nil {
		return fmt.Sprintf("https://%s/v3", ks.Gateway.Hostname)
	}
	return ""
}

// deleteOrphanedKeystone removes a previously-projected Keystone child when
// spec.services.keystone is unset AND the ControlPlane has opted in to deletion
// via keystoneDeletionAllowedAnnotation (the caller gates this — without the
// opt-in the child is preserved, since the cascade would destroy its
// irreplaceable credential/fernet keys). The child carries this ControlPlane as
// its controller owner reference, so it is only deleted when still owned here;
// DeletePropagationBackground lets Kubernetes garbage-collect the Keystone's own
// children (Deployment, Jobs, Secrets) behind it. Not-found and an
// externally-owned collision are both treated as nothing to do.
func (r *ControlPlaneReconciler) deleteOrphanedKeystone(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) error {
	key := client.ObjectKey{Name: keystoneName(cp), Namespace: childNamespace(cp)}
	keystone := &keystonev1alpha1.Keystone{}
	if err := r.Get(ctx, key, keystone); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("getting Keystone %s for orphan cleanup: %w", key, err)
	}
	if !metav1.IsControlledBy(keystone, cp) {
		// Not our child (externally managed with a colliding name) — leave it.
		return nil
	}
	if err := r.Delete(ctx, keystone, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting orphaned Keystone %s: %w", key, err)
	}
	log.FromContext(ctx).Info("Deleted orphaned Keystone child after services.keystone was unset", "keystone", key)
	return nil
}
