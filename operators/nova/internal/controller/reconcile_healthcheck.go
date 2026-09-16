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
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
)

// Condition type and reason constants for NovaAPIReady.
const (
	conditionTypeNovaAPIReady       = "NovaAPIReady"
	conditionReasonAPIHealthy       = "APIHealthy"
	conditionReasonAPIUnhealthy     = "APIUnhealthy"
	conditionReasonEndpointNotReady = healthcheck.ReasonEndpointNotReady
)

// httpClient returns the reconciler's HTTPClient if set, otherwise
// http.DefaultClient.
func (r *NovaReconciler) httpClient() healthcheck.HTTPDoer {
	if r.HTTPClient != nil {
		return r.HTTPClient
	}
	return http.DefaultClient
}

// novaHealthCheckURL returns the cluster-local URL the probe GETs: the API root,
// which answers the version document without a token and without touching the
// database or the message bus. The compute API ships no /healthcheck route of
// its own, so the root is what a 2xx can be read off.
func novaHealthCheckURL(nova *novav1alpha1.Nova) string {
	return internalNovaURL(nova) + "/"
}

// reconcileHealthCheck performs an HTTP GET against the cluster-local API root
// and sets the NovaAPIReady condition from the response, via the shared probe
// flow. The probe target is always the in-cluster Service URL, independent of
// spec.gateway: it verifies API readiness, not the ingress/DNS/cert/Gateway path
// status.endpoint may advertise externally.
func (r *NovaReconciler) reconcileHealthCheck(ctx context.Context, nova *novav1alpha1.Nova) (ctrl.Result, error) {
	// An injected HTTPClient wins whenever it is set. It is the test seam that
	// drives the probe with a stub transport, placed or not, and no binary sets
	// it. Otherwise a placed Nova is probed through the target API server's
	// service proxy, because its Service URL resolves on that cluster and
	// nowhere else; an unplaced one keeps http.DefaultClient.
	doer := r.httpClient()
	if r.HTTPClient == nil {
		var err error
		doer, err = commonmulticluster.ResolveHTTPDoer(ctx, r.Resolver, nova.Spec.TargetClusterRef, doer)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("resolving the health-probe transport for target cluster %q: %w",
				nova.Spec.TargetClusterRef.Name, err)
		}
	}

	return healthcheck.ReconcileProbe(ctx, healthcheck.ProbeFlowParams{
		Doer:               doer,
		Cache:              &r.healthProbeCache,
		Key:                client.ObjectKeyFromObject(nova),
		UID:                nova.UID,
		Subject:            "Nova API",
		EndpointConfigured: nova.Status.Endpoint != "",
		ProbeEndpoint:      novaHealthCheckURL(nova),
		Conditions:         &nova.Status.Conditions,
		Generation:         nova.Generation,
		ConditionType:      conditionTypeNovaAPIReady,
		HealthyReason:      conditionReasonAPIHealthy,
		UnhealthyReason:    conditionReasonAPIUnhealthy,
		Timeout:            healthcheck.HealthCheckTimeout,
		CacheTTL:           healthcheck.HealthCheckCacheTTL,
		RequeueAfter:       healthcheck.RequeueHealthCheck,
	})
}
