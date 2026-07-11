// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"net/http"
	"strings"

	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

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
// login page and sets the HorizonAPIReady condition based on the response, via
// the shared probe flow. The probe target is always the in-cluster Service URL,
// independent of spec.gateway: we are verifying dashboard readiness, not the
// ingress/DNS/cert/Gateway path that status.endpoint may advertise externally.
func (r *HorizonReconciler) reconcileHealthCheck(ctx context.Context, horizon *horizonv1alpha1.Horizon) (ctrl.Result, error) {
	return healthcheck.ReconcileProbe(ctx, healthcheck.ProbeFlowParams{
		Doer:               r.httpClient(),
		Cache:              &r.healthProbeCache,
		Key:                client.ObjectKeyFromObject(horizon),
		UID:                horizon.UID,
		Subject:            "Horizon dashboard",
		EndpointConfigured: horizon.Status.Endpoint != "",
		ProbeEndpoint:      dashboardLoginURL(horizon),
		Conditions:         &horizon.Status.Conditions,
		Generation:         horizon.Generation,
		ConditionType:      conditionTypeHorizonAPIReady,
		HealthyReason:      conditionReasonAPIHealthy,
		UnhealthyReason:    conditionReasonAPIUnhealthy,
		Timeout:            HealthCheckTimeout,
		CacheTTL:           HealthCheckCacheTTL,
		RequeueAfter:       RequeueHealthCheck,
	})
}
