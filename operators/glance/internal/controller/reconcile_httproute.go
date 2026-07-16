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
	glancev1alpha1 "github.com/c5c3/forge/operators/glance/api/v1alpha1"
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
// Gateway controller to report Accepted=True on the HTTPRoute's parent status.
const requeueHTTPRouteAccepted = RequeueDeploymentPolling

// glanceStatusEndpoint returns the externally reachable Glance API URL. When
// spec.gateway is set, https://{hostname}/ (implicit port 443, the Gateway
// listener terminates TLS). Otherwise the cluster-local Service DNS URL so CRs
// without external exposure still report a usable address.
func glanceStatusEndpoint(glance *glancev1alpha1.Glance) string {
	if glance.Spec.Gateway != nil {
		return fmt.Sprintf("https://%s/", glance.Spec.Gateway.Hostname)
	}
	return internalGlanceURL(glance)
}

// internalGlanceURL returns the cluster-local Glance API URL used by the
// operator's health check. Unlike glanceStatusEndpoint, this never depends on
// spec.gateway: the operator must verify API readiness without relying on
// external DNS, TLS trust for Gateway-terminated certs, or the Gateway data
// plane being healthy.
func internalGlanceURL(glance *glancev1alpha1.Glance) string {
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d/", subResourceName(glance), glance.Namespace, glanceAPIPort)
}

// reconcileHTTPRoute ensures the HTTPRoute that exposes the Glance API through a
// Gateway matches the desired state, via the shared route flow. It keeps only
// the service-specific parts: the desired route builder, the backend identity,
// and the exposure noun for the messages.
func (r *GlanceReconciler) reconcileHTTPRoute(ctx context.Context, glance *glancev1alpha1.Glance) (ctrl.Result, error) {
	// buildGlanceHTTPRoute dereferences spec.gateway, so build the desired route
	// only when external exposure is requested; the flow uses Desired only on the
	// gateway-enabled path.
	var desired *gatewayv1.HTTPRoute
	if glance.Spec.Gateway != nil {
		desired = buildGlanceHTTPRoute(glance)
	}
	return gateway.ReconcileHTTPRoute(ctx, r.Client, r.Scheme, glance, gateway.RouteFlowParams{
		GatewayAPIAvailable: r.gatewayAPIAvailable,
		GatewayConfigured:   glance.Spec.Gateway != nil,
		Desired:             desired,
		RouteName:           subResourceName(glance),
		RouteNamespace:      glance.Namespace,
		ExposureNoun:        "Glance API",
		Conditions:          &glance.Status.Conditions,
		Generation:          glance.Generation,
		ConditionType:       conditionTypeHTTPRouteReady,
		RequeueAccepted:     requeueHTTPRouteAccepted,
	})
}

// buildGlanceHTTPRoute constructs the desired HTTPRoute for the Glance API. It
// attaches to the Gateway referenced by spec.gateway.parentRef, matches the
// configured hostname with a PathPrefix match on spec.gateway.path (or "/" when
// empty), and forwards to the {name} Service on the API port.
func buildGlanceHTTPRoute(glance *glancev1alpha1.Glance) *gatewayv1.HTTPRoute {
	return gateway.BuildHTTPRoute(glance.Spec.Gateway, gateway.RouteParams{
		Name:           subResourceName(glance),
		Namespace:      glance.Namespace,
		Labels:         commonLabels(glance),
		BackendService: subResourceName(glance),
		BackendPort:    glanceAPIPort,
	})
}
