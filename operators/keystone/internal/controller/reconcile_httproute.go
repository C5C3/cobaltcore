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
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// Condition type and reason constants for HTTPRoute readiness.
const (
	conditionTypeHTTPRouteReady           = "HTTPRouteReady"
	conditionReasonHTTPRouteAccepted      = "HTTPRouteAccepted"
	conditionReasonHTTPRouteNotAccepted   = "HTTPRouteNotAccepted"
	conditionReasonHTTPRouteNotRequired   = "HTTPRouteNotRequired"
	conditionReasonGatewayAPINotInstalled = "GatewayAPINotInstalled"
)

// keystoneStatusEndpoint returns the externally reachable Keystone API endpoint
// URL.
//
// Resolution order when spec.gateway is set:
//  1. spec.bootstrap.publicEndpoint, if non-empty — used verbatim.
//  2. otherwise https://{spec.gateway.hostname}/v3 (implicit port 443).
//
// publicEndpoint takes precedence because the externally reachable URL can
// include a port that no Kubernetes object captures: the Gateway listener is
// always the in-cluster TLS port (443), but kind extraPortMappings,
// LoadBalancer overrides, and edge proxies can republish that listener on a
// different host-side port (e.g. KIND_HOST_PORT=8443). Synthesising
// https://{hostname}/v3 in that case would diverge from spec.bootstrap.publicEndpoint
// and from the URL the operator writes into the Keystone service catalog
// (reconcile_bootstrap.go), so consumers of status.endpoint would see a
// stale URL. The webhook enforces that publicEndpoint, when set, uses
// spec.gateway.hostname as its host, preventing drift.
//
// spec.gateway.hostname is validated non-empty by both the CRD schema
// (+kubebuilder:validation:MinLength=1) and the admission webhook,
// so the fallback branch cannot produce https:///v3 post-admission.
//
// When spec.gateway is nil, the cluster-local Service DNS name is returned so
// existing CRs without external exposure continue to report a usable URL.
func keystoneStatusEndpoint(keystone *keystonev1alpha1.Keystone) string {
	if keystone.Spec.Gateway != nil {
		if pe := keystone.Spec.Bootstrap.PublicEndpoint; pe != "" {
			return pe
		}
		return fmt.Sprintf("https://%s/v3", keystone.Spec.Gateway.Hostname)
	}
	return internalAPIURL(keystone)
}

// internalAPIURL returns the cluster-local Keystone API URL used by the
// operator's health check. Unlike keystoneStatusEndpoint, this never depends
// on spec.gateway: the operator must be able to verify API readiness without
// relying on cluster DNS resolution of the public hostname, egress to the
// external VIP, TLS trust for Gateway-terminated certs, or the Gateway data
// plane being healthy — all of which would conflate ingress health with API
// readiness and break KeystoneAPIReady in environments where the operator pod
// has no Internet egress.
func internalAPIURL(keystone *keystonev1alpha1.Keystone) string {
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:5000/v3", subResourceName(keystone), keystone.Namespace)
}

// keystoneAPIPort is the backend Service port targeted by the HTTPRoute
// The Keystone Service is named after the CR
// (metadata.name, no suffix) and listens on port 5000 in every
// existing deployment.
const keystoneAPIPort = gatewayv1.PortNumber(5000)

// requeueHTTPRouteAccepted is the interval for requeuing while waiting for a
// Gateway controller to report Accepted=True on the HTTPRoute's parent status.
// Acceptance is typically near-immediate, so a short interval keeps the
// controller responsive without incurring excessive API load.
const requeueHTTPRouteAccepted = RequeueDeploymentPolling

// reconcileHTTPRoute ensures the HTTPRoute that exposes the Keystone API
// through a Gateway matches the desired state. Three lifecycle paths
//
//   - spec.gateway set: create or update the HTTPRoute and reflect the
//     parent Accepted condition as HTTPRouteReady.
//   - spec.gateway nil: delete any existing HTTPRoute and set
//     HTTPRouteReady=True/HTTPRouteNotRequired.
//   - error: propagate errors from ensure/delete operations.
func (r *KeystoneReconciler) reconcileHTTPRoute(ctx context.Context, keystone *keystonev1alpha1.Keystone) (ctrl.Result, error) {
	// Path 0: Gateway API CRD is not installed. The watch was
	// skipped in SetupWithManager; skip the delete attempt too — c.Delete
	// would fail with "no matches for kind HTTPRoute" — and surface a clear
	// condition instead of erroring.
	if !r.gatewayAPIAvailable {
		if keystone.Spec.Gateway == nil {
			conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
				Type:               conditionTypeHTTPRouteReady,
				Status:             metav1.ConditionTrue,
				ObservedGeneration: keystone.Generation,
				Reason:             conditionReasonHTTPRouteNotRequired,
				Message:            "External API exposure via Gateway API is not configured",
			})
			return ctrl.Result{}, nil
		}
		conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
			Type:               conditionTypeHTTPRouteReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: keystone.Generation,
			Reason:             conditionReasonGatewayAPINotInstalled,
			Message:            "spec.gateway is set but the gateway.networking.k8s.io/v1 HTTPRoute CRD is not installed in this cluster; install Gateway API and restart the operator to enable external API exposure",
		})
		return ctrl.Result{}, nil
	}

	// Path 2: gateway disabled — delete any existing HTTPRoute.
	if keystone.Spec.Gateway == nil {
		if err := gateway.DeleteHTTPRoute(ctx, r.Client, keystone.Namespace, subResourceName(keystone)); err != nil {
			return ctrl.Result{}, err
		}
		conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
			Type:               conditionTypeHTTPRouteReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: keystone.Generation,
			Reason:             conditionReasonHTTPRouteNotRequired,
			Message:            "External API exposure via Gateway API is not configured",
		})
		return ctrl.Result{}, nil
	}

	// Path 1: gateway enabled — create or update the HTTPRoute. ensureHTTPRoute
	// applies via Server-Side Apply and decodes the server response back into
	// desired, so its parent status — written by the Gateway controller — is
	// already populated without a second Get (issue #361).
	desired := buildKeystoneHTTPRoute(keystone)
	if err := gateway.EnsureHTTPRoute(ctx, r.Client, r.Scheme, keystone, desired); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring HTTPRoute: %w", err)
	}

	if gateway.IsHTTPRouteAccepted(desired) {
		conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
			Type:               conditionTypeHTTPRouteReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: keystone.Generation,
			Reason:             conditionReasonHTTPRouteAccepted,
			Message:            "HTTPRoute accepted by Gateway",
		})
		return ctrl.Result{}, nil
	}

	conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
		Type:               conditionTypeHTTPRouteReady,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: keystone.Generation,
		Reason:             conditionReasonHTTPRouteNotAccepted,
		Message:            "HTTPRoute not yet accepted by Gateway",
	})
	return ctrl.Result{RequeueAfter: requeueHTTPRouteAccepted}, nil
}

// buildKeystoneHTTPRoute constructs the desired HTTPRoute for the Keystone API.
// It attaches to the Gateway referenced by spec.gateway.parentRef, matches the
// configured hostname with a PathPrefix match on spec.gateway.path (or "/" when
// empty), and forwards to the {name} Service on port 5000 (dropped the historical -api suffix).
func buildKeystoneHTTPRoute(keystone *keystonev1alpha1.Keystone) *gatewayv1.HTTPRoute {
	return gateway.BuildHTTPRoute(keystone.Spec.Gateway, gateway.RouteParams{
		Name:           subResourceName(keystone),
		Namespace:      keystone.Namespace,
		Labels:         commonLabels(keystone),
		BackendService: subResourceName(keystone),
		BackendPort:    int32(keystoneAPIPort),
	})
}
