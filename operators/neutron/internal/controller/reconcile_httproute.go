// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/c5c3/cobaltcore/internal/common/gateway"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
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

// reconcileHTTPRoute ensures the HTTPRoute that exposes the Neutron API through
// a Gateway matches the desired state, via the shared route flow. It keeps only
// the service-specific parts: the desired route builder, the backend identity,
// and the exposure noun for the messages.
func (r *NeutronReconciler) reconcileHTTPRoute(ctx context.Context, children client.Client, neutron *neutronv1alpha1.Neutron) (ctrl.Result, error) {
	// buildNeutronHTTPRoute dereferences spec.gateway, so build the desired route
	// only when external exposure is requested; the flow uses Desired only on the
	// gateway-enabled path.
	var desired *gatewayv1.HTTPRoute
	if neutron.Spec.Gateway != nil {
		desired = buildNeutronHTTPRoute(neutron)
	}
	return gateway.ReconcileHTTPRoute(ctx, children, r.Scheme, neutron, gateway.RouteFlowParams{
		LocalGatewayAPIAvailable: r.gatewayAPIAvailable,
		GatewayConfigured:        neutron.Spec.Gateway != nil,
		Desired:                  desired,
		RouteName:                neutron.Name,
		RouteNamespace:           neutron.Namespace,
		ExposureNoun:             "Neutron API",
		Conditions:               &neutron.Status.Conditions,
		Generation:               neutron.Generation,
		ConditionType:            conditionTypeHTTPRouteReady,
		RequeueAccepted:          requeueHTTPRouteAccepted,
	})
}

// buildNeutronHTTPRoute constructs the desired HTTPRoute for the Neutron API. It
// attaches to the Gateway referenced by spec.gateway.parentRef, matches the
// configured hostname with a PathPrefix match on spec.gateway.path (or "/" when
// empty), and forwards to the {name} Service on the API port. It renders no
// request timeout: neutron answers short JSON requests, so the gateway
// implementation's default is the right cap.
func buildNeutronHTTPRoute(neutron *neutronv1alpha1.Neutron) *gatewayv1.HTTPRoute {
	return gateway.BuildHTTPRoute(neutron.Spec.Gateway, gateway.RouteParams{
		Name:           neutron.Name,
		Namespace:      neutron.Namespace,
		Labels:         commonLabels(neutron),
		BackendService: neutron.Name,
		BackendPort:    neutronAPIPort,
	})
}
