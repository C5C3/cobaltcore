// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/c5c3/forge/internal/common/gateway"
	horizonv1alpha1 "github.com/c5c3/forge/operators/horizon/api/v1alpha1"
)

// Condition type and reason constants for HTTPRoute readiness. The reason
// vocabulary is shared across operators via the gateway package.
const (
	conditionTypeHTTPRouteReady           = "HTTPRouteReady"
	conditionReasonHTTPRouteAccepted      = gateway.ReasonHTTPRouteAccepted
	conditionReasonHTTPRouteNotAccepted   = gateway.ReasonHTTPRouteNotAccepted
	conditionReasonHTTPRouteNotRequired   = gateway.ReasonHTTPRouteNotRequired
	conditionReasonGatewayAPINotInstalled = gateway.ReasonGatewayAPINotInstalled
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

// reconcileHTTPRoute ensures the HTTPRoute that exposes the dashboard through a
// Gateway matches the desired state, via the shared route flow. It keeps only
// the service-specific parts: the desired route builder, the backend identity,
// and the exposure noun for the messages.
func (r *HorizonReconciler) reconcileHTTPRoute(ctx context.Context, horizon *horizonv1alpha1.Horizon) (ctrl.Result, error) {
	// buildHorizonHTTPRoute dereferences spec.gateway, so build the desired
	// route only when external exposure is requested; the flow uses Desired only
	// on the gateway-enabled path.
	var desired *gatewayv1.HTTPRoute
	if horizon.Spec.Gateway != nil {
		desired = buildHorizonHTTPRoute(horizon)
	}
	return gateway.ReconcileHTTPRoute(ctx, r.Client, r.Scheme, horizon, gateway.RouteFlowParams{
		GatewayAPIAvailable: r.gatewayAPIAvailable,
		GatewayConfigured:   horizon.Spec.Gateway != nil,
		Desired:             desired,
		RouteName:           subResourceName(horizon),
		RouteNamespace:      horizon.Namespace,
		ExposureNoun:        "dashboard",
		Conditions:          &horizon.Status.Conditions,
		Generation:          horizon.Generation,
		ConditionType:       conditionTypeHTTPRouteReady,
		RequeueAccepted:     requeueHTTPRouteAccepted,
	})
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
