// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"net/http"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/forge/internal/common/healthcheck"
	commonmulticluster "github.com/c5c3/forge/internal/common/multicluster"
	barbicanv1alpha1 "github.com/c5c3/forge/operators/barbican/api/v1alpha1"
)

// Condition type and reason constants for BarbicanAPIReady.
const (
	conditionTypeBarbicanAPIReady   = "BarbicanAPIReady"
	conditionReasonAPIHealthy       = "APIHealthy"
	conditionReasonAPIUnhealthy     = "APIUnhealthy"
	conditionReasonEndpointNotReady = healthcheck.ReasonEndpointNotReady
)

// httpClient returns the reconciler's HTTPClient if set, otherwise
// http.DefaultClient.
func (r *BarbicanReconciler) httpClient() healthcheck.HTTPDoer {
	if r.HTTPClient != nil {
		return r.HTTPClient
	}
	return http.DefaultClient
}

// reconcileHealthCheck performs an HTTP GET against the cluster-local Barbican
// API healthcheck app and sets the BarbicanAPIReady condition from the response,
// via the shared probe flow. The paste composite routes /healthcheck to the
// oslo_middleware healthcheck app outside the authtoken pipeline, so a 2xx there
// means the WSGI app is serving requests without needing a token or a database
// round trip. The probe target is always the in-cluster Service URL, independent
// of spec.gateway: it verifies API readiness, not the ingress/DNS/cert/Gateway
// path status.endpoint may advertise externally.
func (r *BarbicanReconciler) reconcileHealthCheck(ctx context.Context, barbican *barbicanv1alpha1.Barbican) (ctrl.Result, error) {
	// An injected HTTPClient wins whenever it is set. It is the test seam that
	// drives the probe with a stub transport, placed or not, and no binary sets
	// it. Otherwise a placed Barbican is probed through the target API server's
	// service proxy, because its Service URL resolves on that cluster and
	// nowhere else; an unplaced one keeps http.DefaultClient.
	doer := r.httpClient()
	if r.HTTPClient == nil {
		var err error
		doer, err = commonmulticluster.ResolveHTTPDoer(ctx, r.Resolver, barbican.Spec.TargetClusterRef, doer)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("resolving the health-probe transport for target cluster %q: %w",
				barbican.Spec.TargetClusterRef.Name, err)
		}
	}

	return healthcheck.ReconcileProbe(ctx, healthcheck.ProbeFlowParams{
		Doer:               doer,
		Cache:              &r.healthProbeCache,
		Key:                client.ObjectKeyFromObject(barbican),
		UID:                barbican.UID,
		Subject:            "Barbican API",
		EndpointConfigured: barbican.Status.Endpoint != "",
		ProbeEndpoint:      internalBarbicanURL(barbican) + barbicanHealthcheckPath,
		Conditions:         &barbican.Status.Conditions,
		Generation:         barbican.Generation,
		ConditionType:      conditionTypeBarbicanAPIReady,
		HealthyReason:      conditionReasonAPIHealthy,
		UnhealthyReason:    conditionReasonAPIUnhealthy,
		Timeout:            healthcheck.HealthCheckTimeout,
		CacheTTL:           healthcheck.HealthCheckCacheTTL,
		RequeueAfter:       healthcheck.RequeueHealthCheck,
	})
}
