// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/c5c3/forge/internal/common/gateway"
	commonreconcile "github.com/c5c3/forge/internal/common/reconcile"
	placementv1alpha1 "github.com/c5c3/forge/operators/placement/api/v1alpha1"
)

// Condition type and reason constants for HTTPRoute readiness. The reason
// vocabulary is shared across operators via the gateway package.
const (
	conditionTypeHTTPRouteReady           = "HTTPRouteReady"
	conditionReasonHTTPRouteNotAccepted   = gateway.ReasonHTTPRouteNotAccepted
	conditionReasonHTTPRouteNotRequired   = gateway.ReasonHTTPRouteNotRequired
	conditionReasonGatewayAPINotInstalled = gateway.ReasonGatewayAPINotInstalled
)

// requeueHTTPRouteAccepted is the interval for requeuing while waiting for a
// Gateway controller to report Accepted=True on the HTTPRoute's parent status.
const requeueHTTPRouteAccepted = commonreconcile.RequeueDeploymentPolling

// placementStatusEndpoint returns the externally reachable Placement API URL.
// When spec.gateway is set, https://{hostname}/ (implicit port 443, the Gateway
// listener terminates TLS). Otherwise the cluster-local Service DNS URL so CRs
// without external exposure still report a usable address.
func placementStatusEndpoint(placement *placementv1alpha1.Placement) string {
	if placement.Spec.Gateway != nil {
		return fmt.Sprintf("https://%s/", placement.Spec.Gateway.Hostname)
	}
	return internalPlacementURL(placement)
}

// internalPlacementURL returns the cluster-local Placement API URL used by the
// operator's health check. Unlike placementStatusEndpoint, this never depends on
// spec.gateway: the operator must verify API readiness without relying on
// external DNS, TLS trust for Gateway-terminated certs, or the Gateway data
// plane being healthy.
func internalPlacementURL(placement *placementv1alpha1.Placement) string {
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d/", subResourceName(placement), placement.Namespace, placementAPIPort)
}

// reconcileHTTPRoute ensures the HTTPRoute that exposes the Placement API
// through a Gateway matches the desired state, via the shared route flow. It
// keeps only the service-specific parts: the desired route builder, the backend
// identity, and the exposure noun for the messages.
func (r *PlacementReconciler) reconcileHTTPRoute(ctx context.Context, children client.Client, placement *placementv1alpha1.Placement) (ctrl.Result, error) {
	// buildPlacementHTTPRoute dereferences spec.gateway, so build the desired
	// route only when external exposure is requested; the flow uses Desired only
	// on the gateway-enabled path.
	var desired *gatewayv1.HTTPRoute
	if placement.Spec.Gateway != nil {
		desired = buildPlacementHTTPRoute(placement)
	}
	return gateway.ReconcileHTTPRoute(ctx, children, r.Scheme, placement, gateway.RouteFlowParams{
		LocalGatewayAPIAvailable: r.gatewayAPIAvailable,
		GatewayConfigured:        placement.Spec.Gateway != nil,
		Desired:                  desired,
		RouteName:                subResourceName(placement),
		RouteNamespace:           placement.Namespace,
		ExposureNoun:             "Placement API",
		Conditions:               &placement.Status.Conditions,
		Generation:               placement.Generation,
		ConditionType:            conditionTypeHTTPRouteReady,
		RequeueAccepted:          requeueHTTPRouteAccepted,
	})
}

// buildPlacementHTTPRoute constructs the desired HTTPRoute for the Placement
// API. It attaches to the Gateway referenced by spec.gateway.parentRef, matches
// the configured hostname with a PathPrefix match on spec.gateway.path (or "/"
// when empty), and forwards to the {name} Service on the API port. It renders no
// request timeout: placement answers short JSON requests, so the gateway
// implementation's default is the right cap.
func buildPlacementHTTPRoute(placement *placementv1alpha1.Placement) *gatewayv1.HTTPRoute {
	return gateway.BuildHTTPRoute(placement.Spec.Gateway, gateway.RouteParams{
		Name:           subResourceName(placement),
		Namespace:      placement.Namespace,
		Labels:         commonLabels(placement),
		BackendService: subResourceName(placement),
		BackendPort:    placementAPIPort,
	})
}
