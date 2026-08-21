// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"net/http"

	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/healthcheck"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	keystonev1alpha1 "github.com/c5c3/cobaltcore/operators/keystone/api/v1alpha1"
)

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
// endpoint and sets the KeystoneAPIReady condition based on the response, via
// the shared probe flow. The probe target is always the in-cluster Service URL
// returned by internalAPIURL, independent of spec.gateway: we are verifying API
// readiness, not the ingress/DNS/cert/Gateway path that keystone.Status.Endpoint
// may advertise externally.
func (r *KeystoneReconciler) reconcileHealthCheck(ctx context.Context, keystone *keystonev1alpha1.Keystone) (ctrl.Result, error) {
	// An injected HTTPClient wins whenever it is set. It is the test seam that
	// drives the probe with a stub transport, placed or not, and no binary sets
	// it. Otherwise a placed Keystone is probed through the target API server's
	// service proxy, because its Service URL resolves on that cluster and
	// nowhere else; an unplaced one keeps http.DefaultClient.
	doer := r.httpClient()
	if r.HTTPClient == nil {
		var err error
		doer, err = commonmulticluster.ResolveHTTPDoer(ctx, r.Resolver, keystone.Spec.TargetClusterRef, doer)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("resolving the health-probe transport for target cluster %q: %w",
				keystone.Spec.TargetClusterRef.Name, err)
		}
	}

	return healthcheck.ReconcileProbe(ctx, healthcheck.ProbeFlowParams{
		Doer:               doer,
		Cache:              &r.healthProbeCache,
		Key:                client.ObjectKeyFromObject(keystone),
		UID:                keystone.UID,
		Subject:            "Keystone API",
		EndpointConfigured: keystone.Status.Endpoint != "",
		ProbeEndpoint:      internalAPIURL(keystone),
		Conditions:         &keystone.Status.Conditions,
		Generation:         keystone.Generation,
		ConditionType:      conditionTypeKeystoneAPIReady,
		HealthyReason:      conditionReasonAPIHealthy,
		UnhealthyReason:    conditionReasonAPIUnhealthy,
		Timeout:            healthcheck.HealthCheckTimeout,
		CacheTTL:           healthcheck.HealthCheckCacheTTL,
		RequeueAfter:       healthcheck.RequeueHealthCheck,
	})
}
