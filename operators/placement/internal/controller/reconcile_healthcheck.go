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

	"github.com/c5c3/cobaltcore/internal/common/healthcheck"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	placementv1alpha1 "github.com/c5c3/cobaltcore/operators/placement/api/v1alpha1"
)

// Condition type and reason constants for PlacementAPIReady.
const (
	conditionTypePlacementAPIReady  = "PlacementAPIReady"
	conditionReasonAPIHealthy       = "APIHealthy"
	conditionReasonAPIUnhealthy     = "APIUnhealthy"
	conditionReasonEndpointNotReady = healthcheck.ReasonEndpointNotReady
)

// httpClient returns the reconciler's HTTPClient if set, otherwise
// http.DefaultClient.
func (r *PlacementReconciler) httpClient() healthcheck.HTTPDoer {
	if r.HTTPClient != nil {
		return r.HTTPClient
	}
	return http.DefaultClient
}

// reconcileHealthCheck performs an HTTP GET against the cluster-local Placement
// API root and sets the PlacementAPIReady condition from the response, via the
// shared probe flow. "/" is the version document, which placement answers
// without a token and without touching the database, so a 2xx there means the
// WSGI app is serving requests. The probe target is always the in-cluster
// Service URL, independent of spec.gateway: it verifies API readiness, not the
// ingress/DNS/cert/Gateway path status.endpoint may advertise externally.
func (r *PlacementReconciler) reconcileHealthCheck(ctx context.Context, placement *placementv1alpha1.Placement) (ctrl.Result, error) {
	// An injected HTTPClient wins whenever it is set. It is the test seam that
	// drives the probe with a stub transport, placed or not, and no binary sets
	// it. Otherwise a placed Placement is probed through the target API server's
	// service proxy, because its Service URL resolves on that cluster and
	// nowhere else; an unplaced one keeps http.DefaultClient.
	doer := r.httpClient()
	if r.HTTPClient == nil {
		var err error
		doer, err = commonmulticluster.ResolveHTTPDoer(ctx, r.Resolver, placement.Spec.TargetClusterRef, doer)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("resolving the health-probe transport for target cluster %q: %w",
				placement.Spec.TargetClusterRef.Name, err)
		}
	}

	return healthcheck.ReconcileProbe(ctx, healthcheck.ProbeFlowParams{
		Doer:               doer,
		Cache:              &r.healthProbeCache,
		Key:                client.ObjectKeyFromObject(placement),
		UID:                placement.UID,
		Subject:            "Placement API",
		EndpointConfigured: placement.Status.Endpoint != "",
		ProbeEndpoint:      internalPlacementURL(placement),
		Conditions:         &placement.Status.Conditions,
		Generation:         placement.Generation,
		ConditionType:      conditionTypePlacementAPIReady,
		HealthyReason:      conditionReasonAPIHealthy,
		UnhealthyReason:    conditionReasonAPIUnhealthy,
		Timeout:            healthcheck.HealthCheckTimeout,
		CacheTTL:           healthcheck.HealthCheckCacheTTL,
		RequeueAfter:       healthcheck.RequeueHealthCheck,
	})
}
