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
	commonv1 "github.com/c5c3/forge/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
	horizonv1alpha1 "github.com/c5c3/forge/operators/horizon/api/v1alpha1"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// The projected Horizon CR is named "{controlplane.Name}-horizon" — the same
// deterministic, collision-free naming convention as the Keystone child (see
// keystoneNameSuffix) — and lives in the ControlPlane's own namespace.
const horizonNameSuffix = "-horizon"

// defaultHorizonRepository is the canonical dashboard image repository; the
// tag is derived from spec.openStackRelease unless
// spec.services.horizon.image overrides the whole image reference.
const defaultHorizonRepository = "ghcr.io/c5c3/horizon"

// defaultHorizonSecretKeyName / defaultHorizonSecretKeyKey identify the
// Django SECRET_KEY Secret the projection defaults to when
// spec.services.horizon.secretKeyRef is nil. The default is the
// kind-infrastructure shim (deploy/kind/infrastructure/
// horizon-secret-key-externalsecret.yaml), which is pinned to the default
// ControlPlane identity — multi-ControlPlane deployments must set
// secretKeyRef explicitly (documented on ServiceHorizonSpec).
const (
	defaultHorizonSecretKeyName = "horizon-secret-key"
	defaultHorizonSecretKeyKey  = "secret-key"
)

// horizonDeletionAllowedAnnotation, when set to a truthy value on a
// ControlPlane, opts that ControlPlane in to tearing down a
// previously-projected Horizon child when spec.services.horizon is unset.
// Unlike the Keystone child, the dashboard is stateless (signed-cookie
// sessions, no database) so deletion loses no data — the preserve-by-default
// posture mirrors the Keystone annotation purely for a consistent operator
// UX: an accidental block drop never silently removes a running service.
const horizonDeletionAllowedAnnotation = "c5c3.io/allow-horizon-deletion"

// horizonName returns the deterministic name of the Horizon CR projected from
// the given ControlPlane (see horizonNameSuffix).
func horizonName(cp *c5c3v1alpha1.ControlPlane) string {
	return cp.Name + horizonNameSuffix
}

// horizonDeletionAllowed reports whether cp opts in to deleting its projected
// Horizon child when spec.services.horizon is unset, via a truthy
// horizonDeletionAllowedAnnotation. A missing, malformed, or non-truthy value
// means "preserve".
func horizonDeletionAllowed(cp *c5c3v1alpha1.ControlPlane) bool {
	allowed, err := strconv.ParseBool(cp.Annotations[horizonDeletionAllowedAnnotation])
	return err == nil && allowed
}

// reconcileHorizon projects spec.services.horizon into an owned Horizon CR
// and drives the HorizonReady condition.
//
// The sub-reconciler is GATED on KeystoneReady: the dashboard authenticates
// every login against the ControlPlane's Keystone child, so no Horizon CR is
// created until that child is ready. Once gated through, it create-or-updates
// the Horizon CR — cache DeepCopied from the shared infrastructure, the
// Keystone endpoint derived top-down (horizonKeystoneEndpoint) — and mirrors
// the child's Ready condition back into HorizonReady.
func (r *ControlPlaneReconciler) reconcileHorizon(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// spec.services.horizon is optional. When unset, this ControlPlane manages
	// no dashboard and reports HorizonReady as not-managed so the aggregate
	// Ready condition is not blocked (staged adoption). A previously-projected
	// child is preserved unless the ControlPlane opts in to deletion —
	// consistent with the Keystone annotation UX, though the dashboard itself
	// is stateless and deletion would lose no data.
	if cp.Spec.Services.Horizon == nil {
		message := "spec.services.horizon is unset; no Horizon dashboard is managed by this ControlPlane"
		if horizonDeletionAllowed(cp) {
			if err := r.deleteOrphanedHorizon(ctx, cp); err != nil {
				return ctrl.Result{}, err
			}
		} else {
			message += fmt.Sprintf("; any previously-projected Horizon child is preserved "+
				"(set annotation %s=true to allow deletion)", horizonDeletionAllowedAnnotation)
		}
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeHorizonReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: cp.Generation,
			Reason:             "HorizonNotManaged",
			Message:            message,
		})
		return ctrl.Result{}, nil
	}

	// Nil-safety fail-safe. The dashboard projection DeepCopies the shared cache
	// from spec.infrastructure, so a nil block has nothing to project and the deref
	// below would panic. The validating webhook forbids services.horizon in
	// External mode (the dashboard needs its own External-mode design) and requires
	// spec.infrastructure otherwise, so an External-mode CR always returns at the
	// HorizonNotManaged early-exit above and this guard only fires for a
	// webhook-bypassed CR.
	if cp.Spec.Infrastructure == nil {
		return ctrl.Result{RequeueAfter: infraRequeueAfter}, nil
	}

	// Gate on KeystoneReady.
	if !conditions.AllTrue(cp.Status.Conditions, conditionTypeKeystoneReady) {
		logger.Info("Keystone not ready, deferring Horizon projection")
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeHorizonReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "WaitingForKeystone",
			Message:            "KeystoneReady is not True; Horizon projection deferred",
		})
		return ctrl.Result{RequeueAfter: keystoneInfraGateRequeueAfter}, nil
	}

	// Resolve the Horizon image. spec.services.horizon.image overrides the
	// release-derived default when set.
	image := commonv1.ImageSpec{
		Repository: defaultHorizonRepository,
		Tag:        cp.Spec.OpenStackRelease,
	}
	if override := cp.Spec.Services.Horizon.Image; override != nil {
		image = *override
	}

	// Resolve the SECRET_KEY reference. spec.services.horizon.secretKeyRef
	// overrides the default-identity shim Secret when set.
	secretKeyRef := commonv1.SecretRefSpec{
		Name: defaultHorizonSecretKeyName,
		Key:  defaultHorizonSecretKeyKey,
	}
	if override := cp.Spec.Services.Horizon.SecretKeyRef; override != nil {
		secretKeyRef = *override
	}

	// Resolve the identity backends attached to the Keystone child. Their Ready
	// subset drives the login page's SSO choices and domain dropdown. A List
	// failure must stop the chain rather than silently project an empty
	// websso block, which would remove a working SSO button from the login page.
	backends, err := r.listIdentityBackends(ctx, cp)
	if err != nil {
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeHorizonReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "IdentityBackendsUnavailable",
			Message:            fmt.Sprintf("listing identity backends for the Horizon projection: %v", err),
		})
		return ctrl.Result{}, err
	}

	horizon := &horizonv1alpha1.Horizon{
		ObjectMeta: metav1.ObjectMeta{
			Name:      horizonName(cp),
			Namespace: childNamespace(cp),
		},
	}

	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, horizon, func() error {
		horizon.Spec.Image = image

		// Point the dashboard at the SAME Memcached the ControlPlane
		// provisioned. DeepCopy (over a plain struct copy) is required because
		// CacheSpec carries a pointer ClusterRef and a Servers slice — a
		// shallow copy would alias cp.Spec (same rationale as the Keystone
		// projection).
		horizon.Spec.Cache = *cp.Spec.Infrastructure.Cache.DeepCopy()

		// The shared CacheSpec.Backend is overloaded: infrastructure.cache.backend
		// carries the oslo.cache dogpile path Keystone consumes
		// (dogpile.cache.pymemcache), but the dashboard renders spec.cache.backend
		// verbatim as the Django CACHES backend, which must resolve to an
		// importable Django class. Projecting the dogpile path unchanged makes
		// Django raise InvalidCacheBackendError on the first cache access (the
		// login page the readiness/startup probes hit), so the pods never go
		// Ready. Override it to the Horizon Django default — the endpoint-bearing
		// fields (clusterRef/servers/replicas) DeepCopied above are the only parts
		// of the shared cache the dashboard actually needs. Assigning the
		// webhook-defaulted value (rather than clearing it) keeps the projection
		// idempotent so CreateOrUpdate does not churn each reconcile.
		horizon.Spec.Cache.Backend = horizonv1alpha1.DefaultCacheBackend

		// The Keystone endpoint is derived top-down from the ControlPlane
		// rather than read from the Keystone child's status — no machine
		// consumer reads status endpoints per the settled convention
		// (docs/contributing/adding-a-new-operator.md).
		horizon.Spec.KeystoneEndpoint = horizonKeystoneEndpoint(cp)

		horizon.Spec.SecretKeyRef = secretKeyRef

		// DeepCopy for the same aliasing reason as Cache above; a nil source
		// yields nil, clearing any previously-projected gateway so removal
		// tears the HTTPRoute down.
		horizon.Spec.Gateway = cp.Spec.Services.Horizon.Gateway.DeepCopy()

		// Resolve replicas to the shared operator default, then let an override
		// win. Assigning unconditionally — unlike a set-only-when-present branch
		// — means clearing spec.services.horizon.replicas reverts the child to
		// the default instead of leaving the previously-projected value pinned
		// on the fetched child (a lost update). commonv1.DefaultReplicas is the
		// same constant the Horizon defaulting webhook applies, so the projected
		// value matches the webhook-defaulted value and no reconcile churn results.
		horizon.Spec.Deployment.Replicas = commonv1.DefaultReplicas
		if cp.Spec.Services.Horizon.Replicas != nil {
			horizon.Spec.Deployment.Replicas = *cp.Spec.Services.Horizon.Replicas
		}

		// Project the federated-login surface from the Ready backends.
		// Detaching the last backend clears the block so the login page reverts
		// to local credentials rather than keeping a dead SSO button pinned on
		// the child; a backend that is merely unhealthy retains it (see
		// projectWebSSO / projectMultiDomain).
		horizon.Spec.WebSSO = projectWebSSO(ctx, cp, backends, horizon.Spec.WebSSO)
		horizon.Spec.MultiDomain = projectMultiDomain(ctx, backends, horizon.Spec.MultiDomain)

		return controllerutil.SetControllerReference(cp, horizon, r.Scheme)
	}); err != nil {
		reason := "HorizonError"
		message := fmt.Sprintf("create-or-update Horizon: %v", err)
		// An Invalid (HTTP 422) rejection from the Horizon API server means
		// the projected spec violates a CRD/webhook rule — surface a
		// distinct, actionable reason so the wedge is diagnosable from the
		// condition instead of being buried under a generic HorizonError.
		if apierrors.IsInvalid(err) {
			reason = "HorizonProjectionRejected"
			message = fmt.Sprintf("Horizon API server rejected the projected spec; "+
				"reconcile the ControlPlane spec to a valid projection to recover: %v", err)
		}
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeHorizonReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             reason,
			Message:            message,
		})
		return ctrl.Result{}, err
	}

	// Mirror the child's Ready condition into HorizonReady.
	if !conditions.IsReady(horizon.Status.Conditions) {
		logger.Info("Horizon CR not ready, requeuing", "horizon", horizon.Name)
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeHorizonReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "WaitingForHorizon",
			Message:            fmt.Sprintf("Horizon %q is not ready", horizon.Name),
		})
		return ctrl.Result{RequeueAfter: infraRequeueAfter}, nil
	}

	conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditionTypeHorizonReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: cp.Generation,
		Reason:             "HorizonReady",
		Message:            "Projected Horizon CR is ready",
	})
	return ctrl.Result{}, nil
}

// projectWebSSO decides what to write onto the Horizon child's spec.websso.
// current is the block already carried by the fetched child.
//
// Ready OIDC backends produce the block. When none is Ready the block is
// cleared — UNLESS backends are still attached, in which case current is
// retained: an unhealthy backend has not un-provisioned Keystone's federation
// objects, so the SSO button still completes, and clearing the block would roll
// the dashboard twice (once on the demotion, once on recovery) for nothing.
// Only a genuine detach, or a ControlPlane spec that no longer carries the
// browser-facing endpoints the hand-off needs, clears it.
func projectWebSSO(
	ctx context.Context,
	cp *c5c3v1alpha1.ControlPlane,
	backends []keystonev1alpha1.KeystoneIdentityBackend,
	current *horizonv1alpha1.WebSSOSpec,
) *horizonv1alpha1.WebSSOSpec {
	if websso := horizonWebSSO(ctx, cp, backends); websso != nil {
		return websso
	}
	if backendsAwaitingReady(backends, keystonev1alpha1.IdentityBackendTypeOIDC) {
		log.FromContext(ctx).Info("OIDC identity backends are attached but none is Ready; "+
			"retaining the dashboard's websso block rather than rolling the Horizon Deployment",
			"retained", current != nil)
		return current
	}
	return nil
}

// projectMultiDomain decides what to write onto the Horizon child's
// spec.multiDomain, retaining the previously-projected block while LDAP
// backends are attached but unhealthy — same reasoning as projectWebSSO.
func projectMultiDomain(
	ctx context.Context,
	backends []keystonev1alpha1.KeystoneIdentityBackend,
	current *horizonv1alpha1.MultiDomainSpec,
) *horizonv1alpha1.MultiDomainSpec {
	if md := horizonMultiDomain(backends); md != nil {
		return md
	}
	if backendsAwaitingReady(backends, keystonev1alpha1.IdentityBackendTypeLDAP) {
		log.FromContext(ctx).Info("LDAP identity backends are attached but none is Ready; "+
			"retaining the dashboard's multiDomain block rather than rolling the Horizon Deployment",
			"retained", current != nil)
		return current
	}
	return nil
}

// maxProjectedFederationChoices bounds the federated entries the projection
// emits. The Horizon CRD caps websso.choices at 17 items — 16 federated plus
// the leading local-credentials fallback — and websso.idpMapping at 16
// properties. Nothing bounds how many KeystoneIdentityBackend CRs attach to
// one Keystone, so without this cap the 17th Ready OIDC backend would make the
// API server reject the projected child and wedge every later Horizon change
// (image bump, replica count, secret rotation) behind a failing CreateOrUpdate.
// Dropping the excess keeps the projection converging; the dropped backends are
// logged rather than silently discarded.
const maxProjectedFederationChoices = 16

// horizonWebSSO builds the Horizon child's spec.websso from the Ready OIDC
// backends attached to the Keystone child. It returns nil when none are Ready,
// so the dashboard renders no WEBSSO_* settings at all and the login page shows
// the plain credentials form.
//
// It ALSO returns nil when the ControlPlane cannot complete a WebSSO hand-off,
// even though backends are Ready. The hand-off needs two endpoints the backends
// know nothing about: a trusted dashboard origin (trustedDashboards), without
// which Keystone bounces the browser with "… is not a trusted dashboard host"
// only AFTER the user has entered their corporate credentials; and a
// browser-facing Keystone URL (keystonePublicEndpoint), without which the SSO
// redirect targets a cluster-local DNS name the browser cannot resolve. A
// button that can never complete is worse than no button, so both are
// prerequisites for offering one.
//
// The credentials fallback leads the choice list — the same entry the Horizon
// defaulting webhook would prepend — so enabling SSO never locks out local or
// LDAP-domain accounts, and so the login page opens on the local form.
//
// KeystoneURL is the BROWSER-facing endpoint (keystonePublicEndpoint), not the
// cluster-local one the dashboard's own Django backend talks to: the SSO
// redirect is followed by the user's browser.
func horizonWebSSO(ctx context.Context, cp *c5c3v1alpha1.ControlPlane, backends []keystonev1alpha1.KeystoneIdentityBackend) *horizonv1alpha1.WebSSOSpec {
	federated := readyFederationBackends(backends)
	if len(federated) == 0 {
		return nil
	}
	logger := log.FromContext(ctx)

	keystoneURL := keystonePublicEndpoint(cp.Spec.Services.Keystone)
	if len(trustedDashboards(cp)) == 0 || keystoneURL == "" {
		logger.Info("Ready OIDC identity backends found, but the WebSSO hand-off has no browser-facing endpoints; "+
			"omitting the login page's SSO choices",
			"readyBackends", len(federated),
			"trustedDashboardOrigin", horizonPublicEndpoint(cp.Spec.Services.Horizon),
			"keystoneURL", keystoneURL)
		return nil
	}

	if len(federated) > maxProjectedFederationChoices {
		dropped := make([]string, 0, len(federated)-maxProjectedFederationChoices)
		for i := maxProjectedFederationChoices; i < len(federated); i++ {
			dropped = append(dropped, federated[i].Name)
		}
		logger.Info("More Ready OIDC identity backends than the Horizon websso block can carry; dropping the excess",
			"max", maxProjectedFederationChoices, "dropped", dropped)
		federated = federated[:maxProjectedFederationChoices]
	}

	choices := []horizonv1alpha1.WebSSOChoice{{
		ID:    horizonv1alpha1.DefaultWebSSOLocalChoiceID,
		Label: horizonv1alpha1.DefaultWebSSOLocalChoiceLabel,
	}}
	mapping := make(map[string]horizonv1alpha1.WebSSOIDPTarget, len(federated))
	for i := range federated {
		b := &federated[i]
		id := webSSOChoiceID(b)
		choices = append(choices, horizonv1alpha1.WebSSOChoice{
			ID:    id,
			Label: b.EffectiveIdentityProviderName(),
		})
		mapping[id] = horizonv1alpha1.WebSSOIDPTarget{
			IdentityProvider: b.EffectiveIdentityProviderName(),
			Protocol:         b.EffectiveProtocolID(),
		}
	}

	return &horizonv1alpha1.WebSSOSpec{
		Enabled:       true,
		Choices:       choices,
		IDPMapping:    mapping,
		InitialChoice: horizonv1alpha1.DefaultWebSSOLocalChoiceID,
		KeystoneURL:   keystoneURL,
	}
}

// horizonMultiDomain builds the Horizon child's spec.multiDomain once any LDAP
// backend is Ready. It returns nil while none are: without a domain-backed
// identity source there is no second domain to name, and the login form
// authenticates against the default domain.
//
// It deliberately does NOT populate domainChoices / domainDropdown. Upstream
// openstack_auth swaps the login form's free-text domain field for a select
// bounded by OPENSTACK_KEYSTONE_DOMAIN_CHOICES, and Django then rejects every
// domain outside that list. The operator only ever sees the LDAP-backed
// domains, so a dropdown built from them would lock out every user of a domain
// it cannot enumerate — a SQL-backed domain populated out-of-band, or the
// domain an OIDC backend targets. Attaching one LDAP backend must not take
// those users' logins away, so the form keeps its free-text domain field.
func horizonMultiDomain(backends []keystonev1alpha1.KeystoneIdentityBackend) *horizonv1alpha1.MultiDomainSpec {
	if !hasReadyDomainBackend(backends) {
		return nil
	}
	return &horizonv1alpha1.MultiDomainSpec{
		Enabled:       true,
		DefaultDomain: horizonv1alpha1.DefaultMultiDomainDefaultDomain,
	}
}

// horizonKeystoneEndpoint returns the Keystone endpoint URL projected into
// the Horizon child's spec.keystoneEndpoint. It is ALWAYS the cluster-local
// convention URL of the projected Keystone child (keystoneEndpointURL, the
// same URL K-ORC authenticates against) — never the external publicEndpoint
// or gateway hostname. OPENSTACK_KEYSTONE_URL is consumed server-side by the
// dashboard's Django backend, not by the browser: an externally routable URL
// that resolves to a host-only address (a kind port-mapping, an external LB)
// is unreachable from the dashboard pods and breaks every login with
// "Unable to establish connection to keystone endpoint". Derived top-down
// from the naming convention, never read from the child's status.
func horizonKeystoneEndpoint(cp *c5c3v1alpha1.ControlPlane) string {
	return keystoneEndpointURL(cp)
}

// deleteOrphanedHorizon removes a previously-projected Horizon child when
// spec.services.horizon is unset AND the ControlPlane has opted in to
// deletion via horizonDeletionAllowedAnnotation (the caller gates this). The
// child carries this ControlPlane as its controller owner reference, so it is
// only deleted when still owned here; DeletePropagationBackground lets
// Kubernetes garbage-collect the dashboard's own children (Deployment,
// Service, ConfigMaps) behind it. Not-found and an externally-owned collision
// are both treated as nothing to do.
func (r *ControlPlaneReconciler) deleteOrphanedHorizon(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) error {
	key := client.ObjectKey{Name: horizonName(cp), Namespace: childNamespace(cp)}
	horizon := &horizonv1alpha1.Horizon{}
	if err := r.Get(ctx, key, horizon); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("getting Horizon %s for orphan cleanup: %w", key, err)
	}
	if !metav1.IsControlledBy(horizon, cp) {
		// Not our child (externally managed with a colliding name) — leave it.
		return nil
	}
	if err := r.Delete(ctx, horizon, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting orphaned Horizon %s: %w", key, err)
	}
	log.FromContext(ctx).Info("Deleted orphaned Horizon child after services.horizon was unset", "horizon", key)
	return nil
}
