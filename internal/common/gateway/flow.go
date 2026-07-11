// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/c5c3/forge/internal/common/conditions"
)

// Condition reason constants for the HTTPRoute readiness condition, shared so
// every operator's route condition uses the same vocabulary.
const (
	ReasonHTTPRouteAccepted      = "HTTPRouteAccepted"
	ReasonHTTPRouteNotAccepted   = "HTTPRouteNotAccepted"
	ReasonHTTPRouteNotRequired   = "HTTPRouteNotRequired"
	ReasonGatewayAPINotInstalled = "GatewayAPINotInstalled"
)

// RouteFlowParams carries everything ReconcileHTTPRoute needs. The
// service-specific parts — whether the Gateway API is installed, whether
// spec.gateway is set, the built desired route, the route identity, the
// exposure noun for the messages, and the condition type — are supplied by the
// caller; the three-path flow itself is identical across operators.
type RouteFlowParams struct {
	// GatewayAPIAvailable reports whether the HTTPRoute CRD is installed (the
	// watch was registered at startup).
	GatewayAPIAvailable bool
	// GatewayConfigured reports whether the CR requests external exposure
	// (spec.gateway != nil).
	GatewayConfigured bool
	// Desired is the built HTTPRoute, applied when GatewayConfigured is true.
	Desired *gatewayv1.HTTPRoute
	// RouteName and RouteNamespace identify the route for the delete path.
	RouteName      string
	RouteNamespace string
	// ExposureNoun is the human-readable noun for the not-required and
	// not-installed messages, for example "API" or "dashboard".
	ExposureNoun string
	// Conditions is the CR's condition slice, mutated in place.
	Conditions *[]metav1.Condition
	// Generation is stamped onto every condition the flow writes.
	Generation int64
	// ConditionType is the readiness condition the flow reports on (for example
	// "HTTPRouteReady").
	ConditionType string
	// RequeueAccepted is the interval to requeue while waiting for the Gateway
	// controller to report Accepted=True.
	RequeueAccepted time.Duration
}

// ReconcileHTTPRoute ensures the HTTPRoute that exposes a service through a
// Gateway matches the desired state. It is the shared body of every operator's
// reconcileHTTPRoute sub-reconciler and implements three lifecycle paths:
//
//   - Gateway API CRD absent: skip the delete (which would fail with "no matches
//     for kind HTTPRoute") and surface a clear condition.
//   - spec.gateway nil: delete any existing HTTPRoute and set the condition
//     True/HTTPRouteNotRequired.
//   - spec.gateway set: apply the route via SSA and reflect the parent Accepted
//     condition as the readiness condition.
func ReconcileHTTPRoute(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, p RouteFlowParams) (ctrl.Result, error) {
	notConfiguredMsg := fmt.Sprintf("External %s exposure via Gateway API is not configured", p.ExposureNoun)

	// Path 0: Gateway API CRD is not installed.
	if !p.GatewayAPIAvailable {
		if !p.GatewayConfigured {
			conditions.SetCondition(p.Conditions, metav1.Condition{
				Type:               p.ConditionType,
				Status:             metav1.ConditionTrue,
				ObservedGeneration: p.Generation,
				Reason:             ReasonHTTPRouteNotRequired,
				Message:            notConfiguredMsg,
			})
			return ctrl.Result{}, nil
		}
		conditions.SetCondition(p.Conditions, metav1.Condition{
			Type:               p.ConditionType,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: p.Generation,
			Reason:             ReasonGatewayAPINotInstalled,
			Message: fmt.Sprintf("spec.gateway is set but the gateway.networking.k8s.io/v1 HTTPRoute CRD is "+
				"not installed in this cluster; install Gateway API and restart the operator to enable "+
				"external %s exposure", p.ExposureNoun),
		})
		return ctrl.Result{}, nil
	}

	// Path 2: gateway disabled — delete any existing HTTPRoute.
	if !p.GatewayConfigured {
		if err := DeleteHTTPRoute(ctx, c, p.RouteNamespace, p.RouteName); err != nil {
			return ctrl.Result{}, err
		}
		conditions.SetCondition(p.Conditions, metav1.Condition{
			Type:               p.ConditionType,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: p.Generation,
			Reason:             ReasonHTTPRouteNotRequired,
			Message:            notConfiguredMsg,
		})
		return ctrl.Result{}, nil
	}

	// Path 1: gateway enabled — create or update the HTTPRoute. EnsureHTTPRoute
	// applies via Server-Side Apply and decodes the server response back into
	// desired, so its parent status — written by the Gateway controller — is
	// already populated without a second Get (issue #361).
	if err := EnsureHTTPRoute(ctx, c, scheme, owner, p.Desired); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring HTTPRoute: %w", err)
	}

	if IsHTTPRouteAccepted(p.Desired) {
		conditions.SetCondition(p.Conditions, metav1.Condition{
			Type:               p.ConditionType,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: p.Generation,
			Reason:             ReasonHTTPRouteAccepted,
			Message:            "HTTPRoute accepted by Gateway",
		})
		return ctrl.Result{}, nil
	}

	conditions.SetCondition(p.Conditions, metav1.Condition{
		Type:               p.ConditionType,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: p.Generation,
		Reason:             ReasonHTTPRouteNotAccepted,
		Message:            "HTTPRoute not yet accepted by Gateway",
	})
	return ctrl.Result{RequeueAfter: p.RequeueAccepted}, nil
}
