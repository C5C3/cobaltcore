// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"net/http"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/healthcheck"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// healthProbeCacheHit reports whether the cached probe for this CR can be
// reused in place of a fresh HTTP GET: the KeystoneAPIReady condition is
// already True, and the shared TTL probe cache holds an entry matching the
// CR's UID and endpoint within HealthCheckCacheTTL.
func (r *KeystoneReconciler) healthProbeCacheHit(keystone *keystonev1alpha1.Keystone, endpoint string) bool {
	current := conditions.GetCondition(keystone.Status.Conditions, conditionTypeKeystoneAPIReady)
	if current == nil || current.Status != metav1.ConditionTrue {
		return false
	}
	return r.healthProbeCache.Hit(client.ObjectKeyFromObject(keystone), keystone.UID, endpoint, HealthCheckCacheTTL)
}

// storeHealthProbe records a successful probe so reconciles within the TTL can
// skip the synchronous HTTP GET.
func (r *KeystoneReconciler) storeHealthProbe(keystone *keystonev1alpha1.Keystone, endpoint string) {
	r.healthProbeCache.Store(client.ObjectKeyFromObject(keystone), keystone.UID, endpoint)
}

// evictHealthProbe drops the cached probe for a CR so the next reconcile
// re-probes. Called on any probe failure and on CR deletion.
func (r *KeystoneReconciler) evictHealthProbe(key types.NamespacedName) {
	r.healthProbeCache.Evict(key)
}

// Condition type and reason constants for KeystoneAPIReady.
const (
	conditionTypeKeystoneAPIReady   = "KeystoneAPIReady"
	conditionReasonAPIHealthy       = "APIHealthy"
	conditionReasonAPIUnhealthy     = "APIUnhealthy"
	conditionReasonEndpointNotReady = healthcheck.ReasonEndpointNotReady

	conditionReasonHealthCheckTimeout = healthcheck.ReasonHealthCheckTimeout
	conditionReasonConnectionFailed   = healthcheck.ReasonConnectionFailed
	conditionReasonHealthCheckFailed  = healthcheck.ReasonHealthCheckFailed
)

// HTTPDoer re-exports the shared health-check client seam so that tests can
// inject a stub transport for the Keystone API health check.
type HTTPDoer = healthcheck.HTTPDoer

// httpClient returns the reconciler's HTTPClient if set, otherwise
// http.DefaultClient.
func (r *KeystoneReconciler) httpClient() HTTPDoer {
	if r.HTTPClient != nil {
		return r.HTTPClient
	}
	return http.DefaultClient
}

// reconcileHealthCheck performs an HTTP GET to the cluster-local Keystone /v3
// endpoint and sets the KeystoneAPIReady condition based on the response.
// The probe target is always the in-cluster Service URL returned by
// internalAPIURL, independent of spec.gateway: we are verifying API readiness,
// not the ingress/DNS/cert/Gateway path that keystone.Status.Endpoint may
// advertise externally.
func (r *KeystoneReconciler) reconcileHealthCheck(ctx context.Context, keystone *keystonev1alpha1.Keystone) (ctrl.Result, error) {
	if keystone.Status.Endpoint == "" {
		log.FromContext(ctx).Info("Keystone API endpoint not yet configured, requeuing")
		conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
			Type:               conditionTypeKeystoneAPIReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: keystone.Generation,
			Reason:             conditionReasonEndpointNotReady,
			Message:            "endpoint not yet configured",
		})
		return ctrl.Result{RequeueAfter: RequeueHealthCheck}, nil
	}
	endpoint := internalAPIURL(keystone)
	key := client.ObjectKeyFromObject(keystone)

	// Serve from the probe cache when the last successful probe for this exact
	// endpoint is still fresh and KeystoneAPIReady is already True. This keeps
	// the synchronous HTTP GET — which can take up to HealthCheckTimeout on a
	// flapping API — off the hot path for a steady CR. The condition is
	// re-upserted (not left untouched) so its ObservedGeneration tracks the
	// current spec; the message matches the probe path verbatim so a cache pass
	// and a probe pass produce byte-identical status and the status-diff gate
	// skips the write.
	if r.healthProbeCacheHit(keystone, endpoint) {
		log.FromContext(ctx).V(1).Info("Keystone API health check served from cache", "endpoint", endpoint)
		conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
			Type:               conditionTypeKeystoneAPIReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: keystone.Generation,
			Reason:             conditionReasonAPIHealthy,
			Message:            fmt.Sprintf("Keystone API is responding at %s", endpoint),
		})
		return ctrl.Result{}, nil
	}

	checkCtx, cancel := context.WithTimeout(ctx, HealthCheckTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(checkCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("building health check request: %w", err)
	}

	resp, err := r.httpClient().Do(req)
	if err != nil {
		// A cancelled parent context means a peer in the parallel post-deployment
		// group failed and errgroup cancelled gctx — the aborted probe is not an
		// API-health signal. Propagate the cancellation without flipping
		// KeystoneAPIReady or evicting the probe cache, so an unrelated
		// Bootstrap/HPA/HTTPRoute/TrustFlush failure cannot masquerade as
		// "Keystone API down" (issue #361). A genuine probe timeout fires
		// checkCtx's deadline while the parent ctx stays live (ctx.Err()==nil),
		// so it still routes through handleHealthCheckError below.
		if cerr := ctx.Err(); cerr != nil {
			return ctrl.Result{}, cerr
		}
		// Any other probe error must evict so the next reconcile re-probes rather
		// than serving a stale success.
		r.evictHealthProbe(key)
		return r.handleHealthCheckError(ctx, keystone, err), nil
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		log.FromContext(ctx).V(1).Info("Keystone API health check passed", "status", resp.StatusCode)
		r.storeHealthProbe(keystone, endpoint)
		conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
			Type:               conditionTypeKeystoneAPIReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: keystone.Generation,
			Reason:             conditionReasonAPIHealthy,
			Message:            fmt.Sprintf("Keystone API is responding at %s", endpoint),
		})
		return ctrl.Result{}, nil
	}

	log.FromContext(ctx).Info("Keystone API health check failed", "status", resp.StatusCode)
	// A non-2xx response is a failed probe: evict so recovery is detected on
	// the next reconcile instead of masked by a stale cached success.
	r.evictHealthProbe(key)
	conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
		Type:               conditionTypeKeystoneAPIReady,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: keystone.Generation,
		Reason:             conditionReasonAPIUnhealthy,
		Message:            fmt.Sprintf("Keystone API returned HTTP %d", resp.StatusCode),
	})
	return ctrl.Result{RequeueAfter: RequeueHealthCheck}, nil
}

// handleHealthCheckError classifies the HTTP client error and sets the
// KeystoneAPIReady condition with an appropriate Reason. All network errors
// result in a requeue rather than a hard error.
func (r *KeystoneReconciler) handleHealthCheckError(ctx context.Context, keystone *keystonev1alpha1.Keystone, err error) ctrl.Result {
	reason, message := classifyHealthCheckError(err)
	log.FromContext(ctx).Info("Keystone API health check error", "reason", reason, "error", err)
	conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
		Type:               conditionTypeKeystoneAPIReady,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: keystone.Generation,
		Reason:             reason,
		Message:            message,
	})
	return ctrl.Result{RequeueAfter: RequeueHealthCheck}
}

// classifyHealthCheckError returns the condition Reason and Message for the
// given HTTP client error, via the shared classifier.
func classifyHealthCheckError(err error) (reason, message string) {
	return healthcheck.ClassifyError(err)
}
