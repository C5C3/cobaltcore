// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/healthcheck"
	horizonv1alpha1 "github.com/c5c3/forge/operators/horizon/api/v1alpha1"
)

// Condition type and reason constants for HorizonAPIReady.
const (
	conditionTypeHorizonAPIReady    = "HorizonAPIReady"
	conditionReasonAPIHealthy       = "APIHealthy"
	conditionReasonAPIUnhealthy     = "APIUnhealthy"
	conditionReasonEndpointNotReady = healthcheck.ReasonEndpointNotReady
)

// HTTPDoer re-exports the shared health-check client seam so that tests can
// inject a stub transport for the dashboard health check.
type HTTPDoer = healthcheck.HTTPDoer

// httpClient returns the reconciler's HTTPClient if set, otherwise
// http.DefaultClient.
func (r *HorizonReconciler) httpClient() HTTPDoer {
	if r.HTTPClient != nil {
		return r.HTTPClient
	}
	return http.DefaultClient
}

// healthProbeCacheHit reports whether the cached probe for this CR can be
// reused in place of a fresh HTTP GET: the HorizonAPIReady condition is
// already True, and the shared TTL probe cache holds an entry matching the
// CR's UID and endpoint within HealthCheckCacheTTL.
func (r *HorizonReconciler) healthProbeCacheHit(horizon *horizonv1alpha1.Horizon, endpoint string) bool {
	current := conditions.GetCondition(horizon.Status.Conditions, conditionTypeHorizonAPIReady)
	if current == nil || current.Status != metav1.ConditionTrue {
		return false
	}
	return r.healthProbeCache.Hit(client.ObjectKeyFromObject(horizon), horizon.UID, endpoint, HealthCheckCacheTTL)
}

// storeHealthProbe records a successful probe so reconciles within the TTL
// can skip the synchronous HTTP GET.
func (r *HorizonReconciler) storeHealthProbe(horizon *horizonv1alpha1.Horizon, endpoint string) {
	r.healthProbeCache.Store(client.ObjectKeyFromObject(horizon), horizon.UID, endpoint)
}

// evictHealthProbe drops the cached probe for a CR so the next reconcile
// re-probes. Called on any probe failure and on CR deletion.
func (r *HorizonReconciler) evictHealthProbe(key types.NamespacedName) {
	r.healthProbeCache.Evict(key)
}

// dashboardLoginURL returns the cluster-local login-page URL probed by the
// health check. Rendering the login page exercises Django URL routing,
// template rendering, and the static-asset manifest without requiring a live
// Keystone — Keystone is only contacted on login submit.
func dashboardLoginURL(horizon *horizonv1alpha1.Horizon) string {
	return strings.TrimSuffix(internalDashboardURL(horizon), "/") + dashboardLoginPath
}

// reconcileHealthCheck performs an HTTP GET to the cluster-local dashboard
// login page and sets the HorizonAPIReady condition based on the response.
// The probe target is always the in-cluster Service URL, independent of
// spec.gateway: we are verifying dashboard readiness, not the
// ingress/DNS/cert/Gateway path that status.endpoint may advertise
// externally.
func (r *HorizonReconciler) reconcileHealthCheck(ctx context.Context, horizon *horizonv1alpha1.Horizon) (ctrl.Result, error) {
	if horizon.Status.Endpoint == "" {
		log.FromContext(ctx).Info("Horizon dashboard endpoint not yet configured, requeuing")
		conditions.SetCondition(&horizon.Status.Conditions, metav1.Condition{
			Type:               conditionTypeHorizonAPIReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: horizon.Generation,
			Reason:             conditionReasonEndpointNotReady,
			Message:            "endpoint not yet configured",
		})
		return ctrl.Result{RequeueAfter: RequeueHealthCheck}, nil
	}
	endpoint := dashboardLoginURL(horizon)
	key := client.ObjectKeyFromObject(horizon)

	// Serve from the probe cache when the last successful probe for this
	// exact endpoint is still fresh and HorizonAPIReady is already True. The
	// condition is re-upserted (not left untouched) so its ObservedGeneration
	// tracks the current spec; the message matches the probe path verbatim so
	// a cache pass and a probe pass produce byte-identical status and the
	// status-diff gate skips the write.
	if r.healthProbeCacheHit(horizon, endpoint) {
		log.FromContext(ctx).V(1).Info("Horizon dashboard health check served from cache", "endpoint", endpoint)
		conditions.SetCondition(&horizon.Status.Conditions, metav1.Condition{
			Type:               conditionTypeHorizonAPIReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: horizon.Generation,
			Reason:             conditionReasonAPIHealthy,
			Message:            fmt.Sprintf("Horizon dashboard is responding at %s", endpoint),
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
		// A cancelled parent context means a peer in the parallel group
		// failed and errgroup cancelled gctx — the aborted probe is not a
		// dashboard-health signal. Propagate the cancellation without
		// flipping HorizonAPIReady or evicting the probe cache. A genuine
		// probe timeout fires checkCtx's deadline while the parent ctx stays
		// live (ctx.Err()==nil), so it still routes through
		// handleHealthCheckError below.
		if cerr := ctx.Err(); cerr != nil {
			return ctrl.Result{}, cerr
		}
		// Any other probe error must evict so the next reconcile re-probes
		// rather than serving a stale success.
		r.evictHealthProbe(key)
		return r.handleHealthCheckError(ctx, horizon, err), nil
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		log.FromContext(ctx).V(1).Info("Horizon dashboard health check passed", "status", resp.StatusCode)
		r.storeHealthProbe(horizon, endpoint)
		conditions.SetCondition(&horizon.Status.Conditions, metav1.Condition{
			Type:               conditionTypeHorizonAPIReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: horizon.Generation,
			Reason:             conditionReasonAPIHealthy,
			Message:            fmt.Sprintf("Horizon dashboard is responding at %s", endpoint),
		})
		return ctrl.Result{}, nil
	}

	log.FromContext(ctx).Info("Horizon dashboard health check failed", "status", resp.StatusCode)
	// A non-2xx response is a failed probe: evict so recovery is detected on
	// the next reconcile instead of masked by a stale cached success.
	r.evictHealthProbe(key)
	conditions.SetCondition(&horizon.Status.Conditions, metav1.Condition{
		Type:               conditionTypeHorizonAPIReady,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: horizon.Generation,
		Reason:             conditionReasonAPIUnhealthy,
		Message:            fmt.Sprintf("Horizon dashboard returned HTTP %d", resp.StatusCode),
	})
	return ctrl.Result{RequeueAfter: RequeueHealthCheck}, nil
}

// handleHealthCheckError classifies the HTTP client error via the shared
// classifier and sets the HorizonAPIReady condition with an appropriate
// Reason. All network errors result in a requeue rather than a hard error.
func (r *HorizonReconciler) handleHealthCheckError(ctx context.Context, horizon *horizonv1alpha1.Horizon, err error) ctrl.Result {
	reason, message := healthcheck.ClassifyError(err)
	log.FromContext(ctx).Info("Horizon dashboard health check error", "reason", reason, "error", err)
	conditions.SetCondition(&horizon.Status.Conditions, metav1.Condition{
		Type:               conditionTypeHorizonAPIReady,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: horizon.Generation,
		Reason:             reason,
		Message:            message,
	})
	return ctrl.Result{RequeueAfter: RequeueHealthCheck}
}
