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

	"github.com/c5c3/cobaltcore/internal/common/gateway"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	barbicanv1alpha1 "github.com/c5c3/cobaltcore/operators/barbican/api/v1alpha1"
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

// internalBarbicanURL returns the cluster-local Barbican API URL used by the
// operator's health check. Unlike barbicanPublicEndpoint, this never depends on
// spec.gateway: the operator must verify API readiness without relying on
// external DNS, TLS trust for Gateway-terminated certs, or the Gateway data
// plane being healthy.
func internalBarbicanURL(barbican *barbicanv1alpha1.Barbican) string {
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", subResourceName(barbican), barbican.Namespace, barbicanAPIPort)
}

// reconcileHTTPRoute ensures the HTTPRoute that exposes the Barbican API through
// a Gateway matches the desired state, via the shared route flow. It keeps only
// the service-specific parts: the desired route builder, the backend identity,
// and the exposure noun for the messages.
func (r *BarbicanReconciler) reconcileHTTPRoute(ctx context.Context, children client.Client, barbican *barbicanv1alpha1.Barbican) (ctrl.Result, error) {
	// buildBarbicanHTTPRoute dereferences spec.gateway, so build the desired
	// route only when external exposure is requested; the flow uses Desired only
	// on the gateway-enabled path.
	var desired *gatewayv1.HTTPRoute
	if barbican.Spec.Gateway != nil {
		desired = buildBarbicanHTTPRoute(barbican)
	}
	return gateway.ReconcileHTTPRoute(ctx, children, r.Scheme, barbican, gateway.RouteFlowParams{
		LocalGatewayAPIAvailable: r.gatewayAPIAvailable,
		GatewayConfigured:        barbican.Spec.Gateway != nil,
		Desired:                  desired,
		RouteName:                subResourceName(barbican),
		RouteNamespace:           barbican.Namespace,
		ExposureNoun:             "Barbican API",
		Conditions:               &barbican.Status.Conditions,
		Generation:               barbican.Generation,
		ConditionType:            conditionTypeHTTPRouteReady,
		RequeueAccepted:          requeueHTTPRouteAccepted,
	})
}

// buildBarbicanHTTPRoute constructs the desired HTTPRoute for the Barbican API.
// It attaches to the Gateway referenced by spec.gateway.parentRef, matches the
// configured hostname with a PathPrefix match on spec.gateway.path (or "/" when
// empty), and forwards to the {name} Service on the API port. It renders no
// request timeout: barbican answers short JSON requests, so the gateway
// implementation's default is the right cap.
func buildBarbicanHTTPRoute(barbican *barbicanv1alpha1.Barbican) *gatewayv1.HTTPRoute {
	return gateway.BuildHTTPRoute(barbican.Spec.Gateway, gateway.RouteParams{
		Name:           subResourceName(barbican),
		Namespace:      barbican.Namespace,
		Labels:         commonLabels(barbican),
		BackendService: subResourceName(barbican),
		BackendPort:    barbicanAPIPort,
	})
}
