// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/forge/internal/common/conditions"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// healthProbeCacheEntry records the last successful Keystone API probe for one
// CR. uid guards against a CR recreated under the same name/namespace serving a
// stale probe; endpoint invalidates the entry when the target Service URL
// changes; probedAt drives the HealthCheckCacheTTL comparison.
type healthProbeCacheEntry struct {
	uid      types.UID
	endpoint string
	probedAt time.Time
}

// nowFn returns the reconciler's injected clock, or time.Now when unset.
func (r *KeystoneReconciler) nowFn() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// healthProbeCacheHit reports whether the cached probe for this CR can be
// reused in place of a fresh HTTP GET: the KeystoneAPIReady condition is
// already True, and a stored entry matches the CR's UID and endpoint and is
// still within HealthCheckCacheTTL.
func (r *KeystoneReconciler) healthProbeCacheHit(keystone *keystonev1alpha1.Keystone, endpoint string) bool {
	current := conditions.GetCondition(keystone.Status.Conditions, conditionTypeKeystoneAPIReady)
	if current == nil || current.Status != metav1.ConditionTrue {
		return false
	}
	r.healthProbeCacheMu.Lock()
	defer r.healthProbeCacheMu.Unlock()
	entry, ok := r.healthProbeCache[client.ObjectKeyFromObject(keystone)]
	if !ok {
		return false
	}
	return entry.uid == keystone.UID &&
		entry.endpoint == endpoint &&
		r.nowFn().Sub(entry.probedAt) < HealthCheckCacheTTL
}

// storeHealthProbe records a successful probe so reconciles within the TTL can
// skip the synchronous HTTP GET.
func (r *KeystoneReconciler) storeHealthProbe(keystone *keystonev1alpha1.Keystone, endpoint string) {
	r.healthProbeCacheMu.Lock()
	defer r.healthProbeCacheMu.Unlock()
	if r.healthProbeCache == nil {
		r.healthProbeCache = make(map[types.NamespacedName]healthProbeCacheEntry)
	}
	r.healthProbeCache[client.ObjectKeyFromObject(keystone)] = healthProbeCacheEntry{
		uid:      keystone.UID,
		endpoint: endpoint,
		probedAt: r.nowFn(),
	}
}

// evictHealthProbe drops the cached probe for a CR so the next reconcile
// re-probes. Called on any probe failure and on CR deletion.
func (r *KeystoneReconciler) evictHealthProbe(key types.NamespacedName) {
	r.healthProbeCacheMu.Lock()
	defer r.healthProbeCacheMu.Unlock()
	delete(r.healthProbeCache, key)
}

// Condition type and reason constants for KeystoneAPIReady.
const (
	conditionTypeKeystoneAPIReady     = "KeystoneAPIReady"
	conditionReasonEndpointNotReady   = "EndpointNotReady"
	conditionReasonAPIHealthy         = "APIHealthy"
	conditionReasonAPIUnhealthy       = "APIUnhealthy"
	conditionReasonHealthCheckTimeout = "HealthCheckTimeout"
	conditionReasonConnectionFailed   = "ConnectionFailed"
	conditionReasonHealthCheckFailed  = "HealthCheckFailed"
)

// HTTPDoer abstracts the Do method of *http.Client so that tests can inject a
// stub transport for the Keystone API health check.
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

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
		// Any probe error must evict so the next reconcile re-probes rather
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
// given HTTP client error.
func classifyHealthCheckError(err error) (reason, message string) {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return conditionReasonHealthCheckTimeout, "health check timed out"
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return conditionReasonEndpointNotReady, "endpoint not resolvable"
	}

	if strings.Contains(err.Error(), "connection refused") {
		return conditionReasonConnectionFailed, fmt.Sprintf("connection failed: %s", err)
	}

	return conditionReasonHealthCheckFailed, fmt.Sprintf("health check failed: %s", err)
}
