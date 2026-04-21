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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/forge/internal/common/conditions"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// Feature: CC-0067

// Condition type and reason constants for KeystoneAPIReady (CC-0067, W-002).
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
// advertise externally (CC-0065, CC-0067, REQ-001, REQ-004).
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

	checkCtx, cancel := context.WithTimeout(ctx, HealthCheckTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(checkCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("building health check request: %w", err)
	}

	resp, err := r.httpClient().Do(req)
	if err != nil {
		return r.handleHealthCheckError(ctx, keystone, err), nil
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		log.FromContext(ctx).V(1).Info("Keystone API health check passed", "status", resp.StatusCode)
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
// result in a requeue rather than a hard error (CC-0067, REQ-002, REQ-003).
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
// given HTTP client error (CC-0067, REQ-002, REQ-003).
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
