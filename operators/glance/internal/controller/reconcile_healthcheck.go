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
	glancev1alpha1 "github.com/c5c3/forge/operators/glance/api/v1alpha1"
)

// Condition type and reason constants for GlanceAPIReady.
const (
	conditionTypeGlanceAPIReady     = "GlanceAPIReady"
	conditionReasonAPIHealthy       = "APIHealthy"
	conditionReasonAPIUnhealthy     = "APIUnhealthy"
	conditionReasonEndpointNotReady = healthcheck.ReasonEndpointNotReady
)

// HTTPDoer re-exports the shared health-check client seam so tests can inject a
// stub transport for the Glance API health check.
type HTTPDoer = healthcheck.HTTPDoer

// httpClient returns the reconciler's HTTPClient if set, otherwise
// http.DefaultClient.
func (r *GlanceReconciler) httpClient() HTTPDoer {
	if r.HTTPClient != nil {
		return r.HTTPClient
	}
	return http.DefaultClient
}

// evictHealthProbe drops the cached probe for a CR so the next reconcile
// re-probes. Called on any probe failure and on CR deletion.
func (r *GlanceReconciler) evictHealthProbe(key types.NamespacedName) {
	r.healthProbeCache.Evict(key)
}

// glanceHealthCheckURL returns the cluster-local /healthcheck URL probed by the
// health check. The oslo healthcheck middleware answers it without touching the
// database or Keystone, so it verifies the WSGI app is serving requests.
func glanceHealthCheckURL(glance *glancev1alpha1.Glance) string {
	return strings.TrimSuffix(internalGlanceURL(glance), "/") + "/healthcheck"
}

// reconcileHealthCheck performs an HTTP GET to the cluster-local /healthcheck
// endpoint and sets the GlanceAPIReady condition based on the response, via the
// shared probe flow. The probe target is always the in-cluster Service URL,
// independent of spec.gateway: we are verifying API readiness, not the
// ingress/DNS/cert/Gateway path status.endpoint may advertise externally.
func (r *GlanceReconciler) reconcileHealthCheck(ctx context.Context, glance *glancev1alpha1.Glance) (ctrl.Result, error) {
	return healthcheck.ReconcileProbe(ctx, healthcheck.ProbeFlowParams{
		Doer:               r.httpClient(),
		Cache:              &r.healthProbeCache,
		Key:                client.ObjectKeyFromObject(glance),
		UID:                glance.UID,
		Subject:            "Glance API",
		EndpointConfigured: glance.Status.Endpoint != "",
		ProbeEndpoint:      glanceHealthCheckURL(glance),
		Conditions:         &glance.Status.Conditions,
		Generation:         glance.Generation,
		ConditionType:      conditionTypeGlanceAPIReady,
		HealthyReason:      conditionReasonAPIHealthy,
		UnhealthyReason:    conditionReasonAPIUnhealthy,
		Timeout:            HealthCheckTimeout,
		CacheTTL:           HealthCheckCacheTTL,
		RequeueAfter:       RequeueHealthCheck,
	})
}
