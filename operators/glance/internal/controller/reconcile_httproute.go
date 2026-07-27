// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/c5c3/forge/internal/common/gateway"
	commonreconcile "github.com/c5c3/forge/internal/common/reconcile"
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
const requeueHTTPRouteAccepted = commonreconcile.RequeueDeploymentPolling

// glanceRouteRequestTimeout is the per-request route timeout rendered on the
// Glance HTTPRoute. See buildGlanceHTTPRoute for why it is neither the
// implementation default nor disabled.
const glanceRouteRequestTimeout = "4h"

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
		// Raised far above the implementation's default (15s on Envoy
		// Gateway) but NOT disabled. The image data plane legitimately
		// streams for hours, so the default truncates multi-GiB uploads and
		// imports; "0s" — the Gateway API spelling of no timeout — would
		// remove the only request-duration cap on the public edge instead.
		// Four hours clears any legitimate transfer while still capping how
		// long one stalled request holds what it holds. Deliberately not a
		// spec knob — no CR should be able to truncate an in-flight image
		// transfer.
		//
		// It bounds duration, not concurrency, and is NOT what keeps the
		// Glance API from wedging. The rule matches a single "/" prefix, so
		// it covers every Glance path, while a glance-api pod serves
		// DefaultUWSGIProcesses x DefaultUWSGIThreads = 2 concurrent requests
		// with --harakiri opt-in and off by default — at DefaultReplicas,
		// six slots for the whole service. Nothing in front of them caps
		// concurrent requests: this operator renders no BackendTrafficPolicy,
		// circuit breaker, or rate limit, and Envoy's stream idle timeout
		// resets on every byte, so it never fires for a client trickling one.
		// Six requests that stay barely alive therefore occupy every slot for
		// the full window — six honest uploads over slow links do it as
		// readily as an abusive client. Lowering the deadline truncates
		// legitimate transfers and raising it lengthens the window, so the
		// levers are sizing (spec.deployment.replicas,
		// spec.apiServer.uwsgi.processes/threads) and a concurrency policy on
		// the Gateway, which is infrastructure outside this operator.
		RequestTimeout: ptr.To(gatewayv1.Duration(glanceRouteRequestTimeout)),
	})
}
