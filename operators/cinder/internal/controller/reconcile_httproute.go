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
	cinderv1alpha1 "github.com/c5c3/cobaltcore/operators/cinder/api/v1alpha1"
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

// cinderStatusEndpoint returns the externally reachable Cinder API URL. When
// spec.gateway is set, https://{hostname}/ (implicit port 443, the Gateway
// listener terminates TLS). Otherwise the cluster-local Service URL, so a CR
// without external exposure still reports a usable address.
func cinderStatusEndpoint(cinder *cinderv1alpha1.Cinder) string {
	if cinder.Spec.Gateway != nil {
		return fmt.Sprintf("https://%s/", cinder.Spec.Gateway.Hostname)
	}
	return internalCinderURL(cinder)
}

// reconcileHTTPRoute ensures the HTTPRoute that exposes the Cinder API through a
// Gateway matches the desired state, via the shared route flow. It keeps only
// the service-specific parts: the desired route builder, the backend identity,
// and the exposure noun for the messages.
func (r *CinderReconciler) reconcileHTTPRoute(ctx context.Context, children client.Client,
	cinder *cinderv1alpha1.Cinder,
) (ctrl.Result, error) {
	// buildCinderHTTPRoute dereferences spec.gateway, so build the desired route
	// only when external exposure is requested; the flow uses Desired only on the
	// gateway-enabled path.
	var desired *gatewayv1.HTTPRoute
	if cinder.Spec.Gateway != nil {
		desired = buildCinderHTTPRoute(cinder)
	}
	return gateway.ReconcileHTTPRoute(ctx, children, r.Scheme, cinder, gateway.RouteFlowParams{
		LocalGatewayAPIAvailable: r.gatewayAPIAvailable,
		GatewayConfigured:        cinder.Spec.Gateway != nil,
		Desired:                  desired,
		RouteName:                cinder.Name,
		RouteNamespace:           cinder.Namespace,
		ExposureNoun:             "Cinder API",
		Conditions:               &cinder.Status.Conditions,
		Generation:               cinder.Generation,
		ConditionType:            conditionTypeHTTPRouteReady,
		RequeueAccepted:          requeueHTTPRouteAccepted,
	})
}

// buildCinderHTTPRoute constructs the desired HTTPRoute for the Cinder API. It
// attaches to the Gateway referenced by spec.gateway.parentRef, matches the
// configured hostname with a PathPrefix match on spec.gateway.path (or "/" when
// empty), and forwards to the {name} Service on the API port. It renders no
// request timeout: the block-storage API answers short JSON requests — volume
// data never crosses it — so the gateway implementation's own default is the
// right cap.
func buildCinderHTTPRoute(cinder *cinderv1alpha1.Cinder) *gatewayv1.HTTPRoute {
	return gateway.BuildHTTPRoute(cinder.Spec.Gateway, gateway.RouteParams{
		Name:           cinder.Name,
		Namespace:      cinder.Namespace,
		Labels:         commonLabels(cinder),
		BackendService: cinder.Name,
		BackendPort:    cinderAPIPort,
	})
}
