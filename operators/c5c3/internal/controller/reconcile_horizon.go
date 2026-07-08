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

	// spec.infrastructure is optional (External keystone mode omits it). The
	// dashboard projection DeepCopies the shared cache from that block, so a nil
	// block has nothing to project and the deref below would panic. Guard locally
	// rather than trusting the pipeline short-circuit: reconcileInfrastructure
	// runs first and halts a nil-block CR with an ExternalModeNotImplemented
	// requeue, so this is unreachable today, but a later pipeline reorder must not
	// reach the nil dereference. The Infrastructure sub-reconciler owns the
	// External-mode requeue.
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
