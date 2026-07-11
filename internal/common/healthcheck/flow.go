// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package healthcheck

import (
	"context"
	"fmt"
	"net/http"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/forge/internal/common/conditions"
)

// ProbeFlowParams carries everything ReconcileProbe needs to run one operator's
// API-readiness probe. The service-specific parts — the endpoint, the condition
// vocabulary, and the human-readable Subject — are supplied by the caller; the
// flow itself (endpoint gate, TTL cache, timeout-bounded GET, error
// classification, condition reporting) is identical across operators.
type ProbeFlowParams struct {
	// Doer performs the HTTP GET. Tests inject a stub transport.
	Doer HTTPDoer
	// Cache memoizes the last successful probe per CR so a steady-state
	// reconcile skips the synchronous GET.
	Cache *ProbeCache
	// Key and UID identify the CR in the probe cache.
	Key types.NamespacedName
	UID types.UID
	// Subject is the human-readable name of the probed component (for example
	// "Keystone API" or "Horizon dashboard"). It drives both the log lines and
	// the healthy/unhealthy condition messages.
	Subject string
	// EndpointConfigured reports whether the CR's advertised status endpoint is
	// set; when false the flow reports the endpoint-not-ready condition and
	// requeues.
	EndpointConfigured bool
	// ProbeEndpoint is the in-cluster URL the flow GETs. It is always the
	// Service URL, independent of any external gateway path, because the flow
	// verifies API readiness rather than the ingress path.
	ProbeEndpoint string
	// Conditions is the CR's condition slice, mutated in place.
	Conditions *[]metav1.Condition
	// Generation is the CR generation stamped onto every condition it writes.
	Generation int64
	// ConditionType is the condition the flow reports on (for example
	// "KeystoneAPIReady").
	ConditionType string
	// HealthyReason and UnhealthyReason are the condition reasons for a 2xx and
	// a non-2xx response respectively.
	HealthyReason   string
	UnhealthyReason string
	// Timeout bounds the HTTP GET. CacheTTL bounds cache reuse. RequeueAfter is
	// the requeue interval on any not-ready outcome.
	Timeout      time.Duration
	CacheTTL     time.Duration
	RequeueAfter time.Duration
}

// cacheHit reports whether the last successful probe for this CR can be reused
// in place of a fresh GET: the readiness condition is already True and the TTL
// cache holds a matching entry.
func (p ProbeFlowParams) cacheHit() bool {
	current := conditions.GetCondition(*p.Conditions, p.ConditionType)
	if current == nil || current.Status != metav1.ConditionTrue {
		return false
	}
	return p.Cache.Hit(p.Key, p.UID, p.ProbeEndpoint, p.CacheTTL)
}

// ReconcileProbe performs an HTTP GET against the CR's in-cluster API endpoint
// and sets the readiness condition based on the response. It is the shared body
// of every operator's reconcileHealthCheck sub-reconciler; the operator supplies
// only the endpoint and condition vocabulary. All network errors requeue rather
// than returning a hard error, except a cancelled parent context (a peer in the
// parallel group failed), which propagates unchanged so an unrelated failure
// cannot masquerade as "API down".
func ReconcileProbe(ctx context.Context, p ProbeFlowParams) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if !p.EndpointConfigured {
		logger.Info(p.Subject + " endpoint not yet configured, requeuing")
		conditions.SetCondition(p.Conditions, metav1.Condition{
			Type:               p.ConditionType,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: p.Generation,
			Reason:             ReasonEndpointNotReady,
			Message:            "endpoint not yet configured",
		})
		return ctrl.Result{RequeueAfter: p.RequeueAfter}, nil
	}

	endpoint := p.ProbeEndpoint

	// Serve from the probe cache when the last successful probe for this exact
	// endpoint is still fresh and the readiness condition is already True. This
	// keeps the synchronous GET off the hot path for a steady CR. The condition
	// is re-upserted (not left untouched) so its ObservedGeneration tracks the
	// current spec; the message matches the probe path verbatim so a cache pass
	// and a probe pass produce byte-identical status and the status-diff gate
	// skips the write.
	if p.cacheHit() {
		logger.V(1).Info(p.Subject+" health check served from cache", "endpoint", endpoint)
		conditions.SetCondition(p.Conditions, metav1.Condition{
			Type:               p.ConditionType,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: p.Generation,
			Reason:             p.HealthyReason,
			Message:            fmt.Sprintf("%s is responding at %s", p.Subject, endpoint),
		})
		return ctrl.Result{}, nil
	}

	checkCtx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(checkCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("building health check request: %w", err)
	}

	resp, err := p.Doer.Do(req)
	if err != nil {
		// A cancelled parent context means a peer in the parallel post-deployment
		// group failed and errgroup cancelled gctx — the aborted probe is not an
		// API-health signal. Propagate the cancellation without flipping the
		// condition or evicting the probe cache, so an unrelated sub-reconciler
		// failure cannot masquerade as "API down" (issue #361). A genuine probe
		// timeout fires checkCtx's deadline while the parent ctx stays live
		// (ctx.Err()==nil), so it still routes through the error path below.
		if cerr := ctx.Err(); cerr != nil {
			return ctrl.Result{}, cerr
		}
		// Any other probe error must evict so the next reconcile re-probes rather
		// than serving a stale success.
		p.Cache.Evict(p.Key)
		reason, message := ClassifyError(err)
		logger.Info(p.Subject+" health check error", "reason", reason, "error", err)
		conditions.SetCondition(p.Conditions, metav1.Condition{
			Type:               p.ConditionType,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: p.Generation,
			Reason:             reason,
			Message:            message,
		})
		return ctrl.Result{RequeueAfter: p.RequeueAfter}, nil
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		logger.V(1).Info(p.Subject+" health check passed", "status", resp.StatusCode)
		p.Cache.Store(p.Key, p.UID, endpoint)
		conditions.SetCondition(p.Conditions, metav1.Condition{
			Type:               p.ConditionType,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: p.Generation,
			Reason:             p.HealthyReason,
			Message:            fmt.Sprintf("%s is responding at %s", p.Subject, endpoint),
		})
		return ctrl.Result{}, nil
	}

	logger.Info(p.Subject+" health check failed", "status", resp.StatusCode)
	// A non-2xx response is a failed probe: evict so recovery is detected on the
	// next reconcile instead of masked by a stale cached success.
	p.Cache.Evict(p.Key)
	conditions.SetCondition(p.Conditions, metav1.Condition{
		Type:               p.ConditionType,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: p.Generation,
		Reason:             p.UnhealthyReason,
		Message:            fmt.Sprintf("%s returned HTTP %d", p.Subject, resp.StatusCode),
	})
	return ctrl.Result{RequeueAfter: p.RequeueAfter}, nil
}
