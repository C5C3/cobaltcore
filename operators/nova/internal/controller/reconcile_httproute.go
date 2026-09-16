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
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
)

// Condition type and reason constants for the three HTTPRoute readiness
// conditions. Nova has three front ends a browser or a peer service reaches over
// HTTP, each with a gateway block of its own, so each route reports on its own
// condition. The reason vocabulary is shared across operators via the gateway
// package.
const (
	conditionTypeHTTPRouteReady         = "HTTPRouteReady"
	conditionTypeMetadataHTTPRouteReady = "MetadataHTTPRouteReady"
	conditionTypeConsoleHTTPRouteReady  = "ConsoleHTTPRouteReady"

	conditionReasonHTTPRouteNotAccepted   = gateway.ReasonHTTPRouteNotAccepted
	conditionReasonHTTPRouteNotRequired   = gateway.ReasonHTTPRouteNotRequired
	conditionReasonGatewayAPINotInstalled = gateway.ReasonGatewayAPINotInstalled
)

// requeueHTTPRouteAccepted is the interval for requeuing while waiting for a
// Gateway controller to report Accepted=True on the HTTPRoute's parent status.
const requeueHTTPRouteAccepted = commonreconcile.RequeueDeploymentPolling

// consoleRouteName returns the name of the console proxy's HTTPRoute. It is not
// the name of the Deployment and Service behind it: those carry the process name
// the image ships (novncproxy), while the route is named after what it exposes.
func consoleRouteName(nova *novav1alpha1.Nova) string {
	return nova.Name + "-console"
}

// reconcileNovaRoute ensures one of Nova's three HTTPRoutes matches the desired
// state, via the shared route flow. gw is the route's gateway block, nil when no
// external exposure is requested; build dereferences it, so the desired route is
// built only when gw is set, and the flow uses Desired only on that path.
func (r *NovaReconciler) reconcileNovaRoute(ctx context.Context, children client.Client,
	nova *novav1alpha1.Nova, gw *novav1alpha1.GatewaySpec,
	build func(*novav1alpha1.Nova) *gatewayv1.HTTPRoute, routeName, noun, conditionType string,
) (ctrl.Result, error) {
	var desired *gatewayv1.HTTPRoute
	if gw != nil {
		desired = build(nova)
	}
	return gateway.ReconcileHTTPRoute(ctx, children, r.Scheme, nova, gateway.RouteFlowParams{
		LocalGatewayAPIAvailable: r.gatewayAPIAvailable,
		GatewayConfigured:        gw != nil,
		Desired:                  desired,
		RouteName:                routeName,
		RouteNamespace:           nova.Namespace,
		ExposureNoun:             noun,
		Conditions:               &nova.Status.Conditions,
		Generation:               nova.Generation,
		ConditionType:            conditionType,
		RequeueAccepted:          requeueHTTPRouteAccepted,
	})
}

// reconcileHTTPRoute ensures the HTTPRoute that exposes the Nova API through a
// Gateway matches the desired state.
func (r *NovaReconciler) reconcileHTTPRoute(ctx context.Context, children client.Client,
	nova *novav1alpha1.Nova,
) (ctrl.Result, error) {
	return r.reconcileNovaRoute(ctx, children, nova, nova.Spec.Gateway, buildAPIHTTPRoute,
		nova.Name, "Nova API", conditionTypeHTTPRouteReady)
}

// reconcileMetadataHTTPRoute ensures the HTTPRoute that exposes the metadata API
// matches the desired state.
//
// The block is rarely set. An instance reaches 169.254.169.254 through the
// Neutron metadata agent, which dials the metadata Service from inside the
// cluster, so a deployment that exposes this API externally does it for a reason
// of its own.
func (r *NovaReconciler) reconcileMetadataHTTPRoute(ctx context.Context, children client.Client,
	nova *novav1alpha1.Nova,
) (ctrl.Result, error) {
	return r.reconcileNovaRoute(ctx, children, nova, nova.Spec.Metadata.Gateway, buildMetadataHTTPRoute,
		metadataName(nova), "Nova metadata API", conditionTypeMetadataHTTPRouteReady)
}

// reconcileConsoleHTTPRoute ensures the HTTPRoute that exposes the noVNC console
// proxy matches the desired state. A disabled proxy is handled as an absent
// gateway block: the flow deletes the route and reports HTTPRouteNotRequired,
// which is the same pass that deletes the proxy Deployment and its Service.
//
// The proxy takes a hostname of its own rather than a path under the API's. The
// console URL the API hands a browser is
// https://{host}/vnc_lite.html?path=%3Ftoken%3D{token}, so the page and the
// WebSocket that follows it both open on "/" with nothing but a query string to
// tell them apart. Splitting them off the API hostname by path is therefore not
// possible, and routing them by the Upgrade header is not something the shared
// route builder expresses.
func (r *NovaReconciler) reconcileConsoleHTTPRoute(ctx context.Context, children client.Client,
	nova *novav1alpha1.Nova,
) (ctrl.Result, error) {
	gw := nova.Spec.ConsoleProxy.Gateway
	if !nova.Spec.ConsoleProxyEnabled() {
		gw = nil
	}
	return r.reconcileNovaRoute(ctx, children, nova, gw, buildConsoleHTTPRoute,
		consoleRouteName(nova), "Nova console proxy", conditionTypeConsoleHTTPRouteReady)
}

// buildAPIHTTPRoute constructs the desired HTTPRoute for the Nova API, forwarded
// to the {name} Service on the API port.
func buildAPIHTTPRoute(nova *novav1alpha1.Nova) *gatewayv1.HTTPRoute {
	return buildNovaHTTPRoute(nova, nova.Spec.Gateway, nova.Name, nova.Name, novaAPIPort)
}

// buildMetadataHTTPRoute constructs the desired HTTPRoute for the metadata API,
// forwarded to the {name}-metadata Service on the metadata port.
func buildMetadataHTTPRoute(nova *novav1alpha1.Nova) *gatewayv1.HTTPRoute {
	return buildNovaHTTPRoute(nova, nova.Spec.Metadata.Gateway,
		metadataName(nova), metadataName(nova), novaMetadataPort)
}

// buildConsoleHTTPRoute constructs the desired HTTPRoute for the console proxy,
// forwarded to the {name}-novncproxy Service on the console port.
func buildConsoleHTTPRoute(nova *novav1alpha1.Nova) *gatewayv1.HTTPRoute {
	return buildNovaHTTPRoute(nova, nova.Spec.ConsoleProxy.Gateway,
		consoleRouteName(nova), consoleProxyName(nova), novaConsolePort)
}

// buildNovaHTTPRoute constructs one of Nova's three HTTPRoutes, delegating to
// the shared builder: it attaches to the Gateway referenced by gw.parentRef,
// matches gw.hostname with a PathPrefix match on gw.path (or "/" when empty),
// and forwards to the named Service and port. It renders no request timeout, so
// the gateway implementation's own default applies.
func buildNovaHTTPRoute(nova *novav1alpha1.Nova, gw *novav1alpha1.GatewaySpec,
	name, backendService string, backendPort int32,
) *gatewayv1.HTTPRoute {
	return gateway.BuildHTTPRoute(gw, gateway.RouteParams{
		Name:           name,
		Namespace:      nova.Namespace,
		Labels:         commonLabels(nova),
		BackendService: backendService,
		BackendPort:    backendPort,
	})
}
