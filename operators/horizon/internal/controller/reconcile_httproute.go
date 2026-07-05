// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/gateway"
	horizonv1alpha1 "github.com/c5c3/forge/operators/horizon/api/v1alpha1"
)

// Condition type and reason constants for HTTPRoute readiness.
const (
	conditionTypeHTTPRouteReady           = "HTTPRouteReady"
	conditionReasonHTTPRouteAccepted      = "HTTPRouteAccepted"
	conditionReasonHTTPRouteNotAccepted   = "HTTPRouteNotAccepted"
	conditionReasonHTTPRouteNotRequired   = "HTTPRouteNotRequired"
	conditionReasonGatewayAPINotInstalled = "GatewayAPINotInstalled"
)

// requeueHTTPRouteAccepted is the interval for requeuing while waiting for a
// Gateway controller to report Accepted=True on the HTTPRoute's parent
// status.
const requeueHTTPRouteAccepted = RequeueDeploymentPolling

// horizonStatusEndpoint returns the externally reachable dashboard URL.
// When spec.gateway is set, https://{hostname}/ (implicit port 443, the
// Gateway listener terminates TLS). Otherwise the cluster-local Service DNS
// URL so CRs without external exposure still report a usable address.
func horizonStatusEndpoint(horizon *horizonv1alpha1.Horizon) string {
	if horizon.Spec.Gateway != nil {
		return fmt.Sprintf("https://%s/", horizon.Spec.Gateway.Hostname)
	}
	return internalDashboardURL(horizon)
}

// internalDashboardURL returns the cluster-local dashboard URL used by the
// operator's health check. Unlike horizonStatusEndpoint, this never depends
// on spec.gateway: the operator must verify dashboard readiness without
// relying on external DNS, TLS trust for Gateway-terminated certs, or the
// Gateway data plane being healthy.
func internalDashboardURL(horizon *horizonv1alpha1.Horizon) string {
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d/", subResourceName(horizon), horizon.Namespace, horizonAPIPort)
}

// reconcileHTTPRoute ensures the HTTPRoute that exposes the dashboard
// through a Gateway matches the desired state. Three lifecycle paths:
//
//   - spec.gateway set: create or update the HTTPRoute and reflect the
//     parent Accepted condition as HTTPRouteReady.
//   - spec.gateway nil: delete any existing HTTPRoute and set
//     HTTPRouteReady=True/HTTPRouteNotRequired.
//   - error: propagate errors from ensure/delete operations.
func (r *HorizonReconciler) reconcileHTTPRoute(ctx context.Context, horizon *horizonv1alpha1.Horizon) (ctrl.Result, error) {
	// Path 0: Gateway API CRD is not installed. The watch was skipped in
	// SetupWithManager; skip the delete attempt too and surface a clear
	// condition instead of erroring.
	if !r.gatewayAPIAvailable {
		if horizon.Spec.Gateway == nil {
			conditions.SetCondition(&horizon.Status.Conditions, metav1.Condition{
				Type:               conditionTypeHTTPRouteReady,
				Status:             metav1.ConditionTrue,
				ObservedGeneration: horizon.Generation,
				Reason:             conditionReasonHTTPRouteNotRequired,
				Message:            "External dashboard exposure via Gateway API is not configured",
			})
			return ctrl.Result{}, nil
		}
		conditions.SetCondition(&horizon.Status.Conditions, metav1.Condition{
			Type:               conditionTypeHTTPRouteReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: horizon.Generation,
			Reason:             conditionReasonGatewayAPINotInstalled,
			Message:            "spec.gateway is set but the gateway.networking.k8s.io/v1 HTTPRoute CRD is not installed in this cluster; install Gateway API and restart the operator to enable external dashboard exposure",
		})
		return ctrl.Result{}, nil
	}

	// Path 2: gateway disabled — delete any existing HTTPRoute.
	if horizon.Spec.Gateway == nil {
		if err := gateway.DeleteHTTPRoute(ctx, r.Client, horizon.Namespace, subResourceName(horizon)); err != nil {
			return ctrl.Result{}, err
		}
		conditions.SetCondition(&horizon.Status.Conditions, metav1.Condition{
			Type:               conditionTypeHTTPRouteReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: horizon.Generation,
			Reason:             conditionReasonHTTPRouteNotRequired,
			Message:            "External dashboard exposure via Gateway API is not configured",
		})
		return ctrl.Result{}, nil
	}

	// Path 1: gateway enabled — create or update the HTTPRoute.
	// EnsureHTTPRoute applies via Server-Side Apply and decodes the server
	// response back into desired, so its parent status — written by the
	// Gateway controller — is already populated without a second Get.
	desired := buildHorizonHTTPRoute(horizon)
	if err := gateway.EnsureHTTPRoute(ctx, r.Client, r.Scheme, horizon, desired); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring HTTPRoute: %w", err)
	}

	if gateway.IsHTTPRouteAccepted(desired) {
		conditions.SetCondition(&horizon.Status.Conditions, metav1.Condition{
			Type:               conditionTypeHTTPRouteReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: horizon.Generation,
			Reason:             conditionReasonHTTPRouteAccepted,
			Message:            "HTTPRoute accepted by Gateway",
		})
		return ctrl.Result{}, nil
	}

	conditions.SetCondition(&horizon.Status.Conditions, metav1.Condition{
		Type:               conditionTypeHTTPRouteReady,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: horizon.Generation,
		Reason:             conditionReasonHTTPRouteNotAccepted,
		Message:            "HTTPRoute not yet accepted by Gateway",
	})
	return ctrl.Result{RequeueAfter: requeueHTTPRouteAccepted}, nil
}

// buildHorizonHTTPRoute constructs the desired HTTPRoute for the dashboard.
// It attaches to the Gateway referenced by spec.gateway.parentRef, matches
// the configured hostname with a PathPrefix match on spec.gateway.path (or
// "/" when empty), and forwards to the {name} Service on port 8080.
func buildHorizonHTTPRoute(horizon *horizonv1alpha1.Horizon) *gatewayv1.HTTPRoute {
	return gateway.BuildHTTPRoute(horizon.Spec.Gateway, gateway.RouteParams{
		Name:           subResourceName(horizon),
		Namespace:      horizon.Namespace,
		Labels:         commonLabels(horizon),
		BackendService: subResourceName(horizon),
		BackendPort:    horizonAPIPort,
	})
}
