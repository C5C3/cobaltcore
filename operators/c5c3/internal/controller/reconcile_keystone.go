// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"strconv"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/policy"
	commonreconcile "github.com/c5c3/forge/internal/common/reconcile"
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

	// Resolve the backing services Keystone actually talks to: its own dedicated
	// instances when it opted into them, the ControlPlane-wide shared ones
	// otherwise (the default).
	//
	// Nil-safety fail-safe. This managed projection points the Keystone child at
	// the backing services the ControlPlane provisioned, so an unresolvable
	// instance has nothing to project and the derefs below would panic. The
	// validating webhook requires spec.infrastructure outside External mode, and
	// the External branch above has already returned, so this only fires for a
	// webhook-bypassed CR — reconcileInfrastructure halts the same CR with
	// InfrastructureNotConfigured.
	database := effectiveKeystoneDatabase(cp)
	cache := effectiveKeystoneCache(cp)
	if database == nil || cache == nil {
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

	// Build the fully-projected desired Keystone. The projection is a pure
	// function of cp.Spec (it reads no live child state), so it is applied via
	// the shared child projector under Server-Side Apply.
	keystone.Spec.Image = image

	// Project the federation proxy (mod_auth_openidc sidecar) image so
	// attaching an OIDC KeystoneIdentityBackend works out of the box on the
	// managed path, and the WebSSO origin of the ControlPlane's own dashboard so
	// Keystone will accept the hand-off. Both fields are assigned unconditionally:
	// clearing the override or the horizon block must revert the child rather than
	// leave the previously-projected value pinned. Both are inert until a
	// federation backend attaches.
	keystone.Spec.Federation = &keystonev1alpha1.FederationSpec{
		ProxyImage:        federationProxyImage(cp),
		TrustedDashboards: trustedDashboards(cp),
	}

	// Point Keystone at the SAME backing services the ControlPlane provisioned for
	// it by reusing the EFFECTIVE specs resolved above. Projecting the effective
	// spec is what carries the dedicated opt-in through the rest of the chain with
	// no per-class special-casing: the keystone-operator derives its logical
	// database, its MariaDB User/Grant CRs, and its NetworkPolicy database/cache
	// egress rules from spec.database / spec.cache, so they follow the instance the
	// service actually talks to.
	//
	// DeepCopy (over a plain struct copy) is required because DatabaseSpec carries
	// pointer fields (ClusterRef, TLS): a shallow copy would share those pointers
	// with cp.Spec (#476).
	keystone.Spec.Database = *database.DeepCopy()

	// in managed mode the operator OWNS the service DB credential —
	// reconcileDBCredentials materialises it into a per-ControlPlane Secret named
	// dbCredentialSecretName(cp). Override the projected Keystone CR's
	// database.secretRef to that operator-owned Secret (key "password"). Brownfield
	// (ClusterRef == nil) leaves the user-supplied secretRef in place.
	if database.ClusterRef != nil {
		keystone.Spec.Database.SecretRef = commonv1.SecretRefSpec{Name: dbCredentialSecretName(cp), Key: "password"}
		// Project the EFFECTIVE credentials mode (Dynamic unless the CP opted into
		// Static), matching reconcileDBCredentials' effective-mode decision.
		if dbCredentialsDynamicEnabled(cp) {
			keystone.Spec.Database.CredentialsMode = commonv1.CredentialsModeDynamic
		} else {
			keystone.Spec.Database.CredentialsMode = commonv1.CredentialsModeStatic
		}
	}

	// DeepCopy for the same reason as Database above (#476).
	keystone.Spec.Cache = *cache.DeepCopy()

	// in managed mode the operator OWNS the admin password —
	// reconcileAdminPassword projects it from OpenBao into a per-ControlPlane
	// Secret named adminPasswordSecretName(cp). Brownfield leaves the
	// user-supplied ref in place.
	keystone.Spec.Bootstrap.AdminPasswordSecretRef = effectiveAdminPasswordSecretRef(cp)
	keystone.Spec.Bootstrap.Region = cp.Spec.Region

	// Project external exposure onto the Keystone CR's spec.gateway, then advertise
	// the externally routable URL via the bootstrap public endpoint. DeepCopy keeps
	// the projected gateway an independent object; a nil source yields nil,
	// clearing any previously-projected gateway.
	keystone.Spec.Gateway = cp.Spec.Services.Keystone.Gateway.DeepCopy()
	keystone.Spec.Bootstrap.PublicEndpoint = keystonePublicEndpoint(cp.Spec.Services.Keystone)

	if cp.Spec.Services.Keystone.Replicas != nil {
		keystone.Spec.Deployment.Replicas = *cp.Spec.Services.Keystone.Replicas
	}

	keystone.Spec.PolicyOverrides = merged

	// Project the ControlPlane's RESOLVED store selection onto the Keystone child:
	// the ControlPlane's explicit spec.secretStoreRef when set, otherwise the
	// operator-provisioned per-tenant store the child shares the namespace with.
	// The projected ref is always concrete, so the child never falls back to its
	// own shared-cluster-store default — the ControlPlane's selection is
	// authoritative and the child pushes fernet/credential keys through the
	// per-tenant identity.
	keystone.Spec.SecretStoreRef = effectiveControlPlaneStoreRefPtr(cp)

	if rotationSchedule != "" {
		keystone.Spec.Fernet.RotationSchedule = rotationSchedule
		keystone.Spec.CredentialKeys.RotationSchedule = rotationSchedule
	}

	return commonreconcile.ProjectChild(ctx, r.Client, r.Scheme, cp, commonreconcile.ChildProjectionParams[*keystonev1alpha1.Keystone]{
		Child:         keystone,
		ConditionType: conditionTypeKeystoneReady,
		ReadyReason:   "KeystoneReady",
		ReadyMessage:  "Projected Keystone CR is ready",
		WaitingReason: "WaitingForKeystone",
		// keystone.Name is set from keystoneName(cp) above.
		WaitingMessage: fmt.Sprintf("Keystone %q is not ready", keystone.Name),
		// An Invalid (HTTP 422) rejection is almost always a now-immutable
		// db/bootstrap field whose CEL transition rule refuses the projected change
		// — e.g. a spec.region edit that landed on the ControlPlane before its own
		// immutability webhook existed, leaving it diverged from the frozen child
		// (#466). Surface a distinct, actionable reason so the wedge is diagnosable.
		RejectedReason: "KeystoneProjectionRejected",
		RejectedMessage: func(err error) string {
			return fmt.Sprintf("Keystone API server rejected the projected spec (likely an immutable db/bootstrap field "+
				"diverged from the frozen Keystone child); reconcile the ControlPlane spec back to the child's values or "+
				"recreate the Keystone child to recover: %v", err)
		},
		ErrorReason:     "KeystoneError",
		ErrorMessage:    func(err error) string { return fmt.Sprintf("create-or-update Keystone: %v", err) },
		WaitRequeue:     infraRequeueAfter,
		Conditions:      &cp.Status.Conditions,
		Generation:      cp.Generation,
		ChildConditions: func(k *keystonev1alpha1.Keystone) []metav1.Condition { return k.Status.Conditions },
	})
}

// federationProxyImage resolves the mod_auth_openidc sidecar image projected
// onto the Keystone child: the explicit services.keystone.federationProxyImage
// override when set, else the release-independent
// ghcr.io/c5c3/keystone-federation-proxy:latest default (see
// defaultFederationProxyRepository).
func federationProxyImage(cp *c5c3v1alpha1.ControlPlane) *commonv1.ImageSpec {
	if ks := cp.Spec.Services.Keystone; ks != nil && ks.FederationProxyImage != nil {
		return ks.FederationProxyImage.DeepCopy()
	}
	return &commonv1.ImageSpec{Repository: defaultFederationProxyRepository, Tag: "latest"}
}

// trustedDashboards returns the WebSSO origins the Keystone child must trust,
// i.e. the origin of this ControlPlane's own dashboard. It returns nil when no
// Horizon block is declared or the dashboard is not externally reachable, so a
// Keystone-only ControlPlane renders no [federation] trusted_dashboard.
//
// Derived top-down from cp.Spec (never from the Horizon child's status), so it
// carries no ordering dependency on reconcileHorizon — which is gated on
// KeystoneReady and therefore runs strictly after this projection.
func trustedDashboards(cp *c5c3v1alpha1.ControlPlane) []string {
	base := horizonPublicEndpoint(cp.Spec.Services.Horizon)
	if base == "" {
		return nil
	}
	return []string{base + webSSOAuthWebssoPath}
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
	child := &keystonev1alpha1.Keystone{
		ObjectMeta: metav1.ObjectMeta{Name: keystoneName(cp), Namespace: childNamespace(cp)},
	}
	return commonreconcile.DeleteOrphanedChild(ctx, r.Client, cp, child)
}
