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
	cinderv1alpha1 "github.com/c5c3/cobaltcore/operators/cinder/api/v1alpha1"
)

// Condition type and reason constants for CinderAPIReady.
const (
	conditionTypeCinderAPIReady     = "CinderAPIReady"
	conditionReasonAPIHealthy       = "APIHealthy"
	conditionReasonAPIUnhealthy     = "APIUnhealthy"
	conditionReasonEndpointNotReady = healthcheck.ReasonEndpointNotReady
)

// httpClient returns the reconciler's HTTPClient if set, otherwise
// http.DefaultClient.
func (r *CinderReconciler) httpClient() healthcheck.HTTPDoer {
	if r.HTTPClient != nil {
		return r.HTTPClient
	}
	return http.DefaultClient
}

// cinderHealthCheckURL returns the cluster-local /healthcheck URL the probe
// GETs. The oslo healthcheck middleware answers it without a token and without
// touching the database or the message bus, so a 2xx there means the WSGI app is
// serving requests.
func cinderHealthCheckURL(cinder *cinderv1alpha1.Cinder) string {
	return internalCinderURL(cinder) + "/healthcheck"
}

// reconcileHealthCheck performs an HTTP GET against the cluster-local
// /healthcheck endpoint and sets the CinderAPIReady condition from the response,
// via the shared probe flow. The probe target is always the in-cluster Service
// URL, independent of spec.gateway: it verifies API readiness, not the
// ingress/DNS/cert/Gateway path status.endpoint may advertise externally.
func (r *CinderReconciler) reconcileHealthCheck(ctx context.Context, cinder *cinderv1alpha1.Cinder) (ctrl.Result, error) {
	// An injected HTTPClient wins whenever it is set. It is the test seam that
	// drives the probe with a stub transport, placed or not, and no binary sets
	// it. Otherwise a placed Cinder is probed through the target API server's
	// service proxy, because its Service URL resolves on that cluster and
	// nowhere else; an unplaced one keeps http.DefaultClient.
	doer := r.httpClient()
	if r.HTTPClient == nil {
		var err error
		doer, err = commonmulticluster.ResolveHTTPDoer(ctx, r.Resolver, cinder.Spec.TargetClusterRef, doer)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("resolving the health-probe transport for target cluster %q: %w",
				cinder.Spec.TargetClusterRef.Name, err)
		}
	}

	return healthcheck.ReconcileProbe(ctx, healthcheck.ProbeFlowParams{
		Doer:               doer,
		Cache:              &r.healthProbeCache,
		Key:                client.ObjectKeyFromObject(cinder),
		UID:                cinder.UID,
		Subject:            "Cinder API",
		EndpointConfigured: cinder.Status.Endpoint != "",
		ProbeEndpoint:      cinderHealthCheckURL(cinder),
		Conditions:         &cinder.Status.Conditions,
		Generation:         cinder.Generation,
		ConditionType:      conditionTypeCinderAPIReady,
		HealthyReason:      conditionReasonAPIHealthy,
		UnhealthyReason:    conditionReasonAPIUnhealthy,
		Timeout:            healthcheck.HealthCheckTimeout,
		CacheTTL:           healthcheck.HealthCheckCacheTTL,
		RequeueAfter:       healthcheck.RequeueHealthCheck,
	})
}
