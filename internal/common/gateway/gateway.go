// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package gateway provides the Gateway API HTTPRoute scaffolding shared by
// the service operators: the CRD-availability probe (IsGVKAvailable), the
// route builder over the shared commonv1.GatewaySpec (BuildHTTPRoute), the
// parent-acceptance check (IsHTTPRouteAccepted), and the SSA ensure / delete
// helpers. The three-path reconcile flow (enabled / disabled / API-absent)
// and its condition vocabulary stay per-operator glue over these primitives.
package gateway

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/c5c3/forge/internal/common/apply"
	commonv1 "github.com/c5c3/forge/internal/common/types"
)

// defaultHTTPRoutePath is the URL path prefix applied when spec.gateway.path
// is empty.
const defaultHTTPRoutePath = "/"

// IsGVKAvailable probes the given RESTMapper for the GVK. It returns false
// when the mapper has no mapping (CRD not installed) and true when the
// mapping exists. Other mapper errors are treated as "unknown"; returning
// false in that case is conservative — the operator starts without the watch
// on the kind and a clear status condition replaces the cryptic
// controller-runtime "no matches for kind" startup error.
func IsGVKAvailable(mapper meta.RESTMapper, gvk schema.GroupVersionKind) bool {
	if mapper == nil {
		return false
	}
	if _, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version); err != nil {
		return false
	}
	return true
}

// RouteParams carries the per-operator inputs of BuildHTTPRoute: the route's
// identity (Name/Namespace/Labels, from the shared naming convention) and the
// backend Service the route forwards to.
type RouteParams struct {
	Name           string
	Namespace      string
	Labels         map[string]string
	BackendService string
	BackendPort    int32
}

// BuildHTTPRoute constructs the desired HTTPRoute for a service API. It
// attaches to the Gateway referenced by spec.parentRef, matches the
// configured hostname with a PathPrefix match on spec.path (or "/" when
// empty), and forwards to the backend Service/port from params.
func BuildHTTPRoute(gw *commonv1.GatewaySpec, p RouteParams) *gatewayv1.HTTPRoute {
	parentRef := gatewayv1.ParentReference{
		Name: gatewayv1.ObjectName(gw.ParentRef.Name),
	}
	if gw.ParentRef.Namespace != "" {
		parentRef.Namespace = ptr.To(gatewayv1.Namespace(gw.ParentRef.Namespace))
	}
	if gw.ParentRef.SectionName != "" {
		parentRef.SectionName = ptr.To(gatewayv1.SectionName(gw.ParentRef.SectionName))
	}

	// Normalize spec.gateway.path to a valid HTTPPathMatch.Value: empty falls
	// back to "/", missing leading slashes are prepended so values like
	// "identity" behave as intended ("/identity") instead of producing an
	// HTTPRoute that Gateway controllers reject.
	path := gw.Path
	if path == "" {
		path = defaultHTTPRoutePath
	} else if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      p.Name,
			Namespace: p.Namespace,
			Labels:    p.Labels,
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{parentRef},
			},
			Hostnames: []gatewayv1.Hostname{gatewayv1.Hostname(gw.Hostname)},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					Matches: []gatewayv1.HTTPRouteMatch{
						{
							Path: &gatewayv1.HTTPPathMatch{
								Type:  ptr.To(gatewayv1.PathMatchPathPrefix),
								Value: &path,
							},
						},
					},
					BackendRefs: []gatewayv1.HTTPBackendRef{
						{
							BackendRef: gatewayv1.BackendRef{
								BackendObjectReference: gatewayv1.BackendObjectReference{
									Kind: ptr.To(gatewayv1.Kind("Service")),
									Name: gatewayv1.ObjectName(p.BackendService),
									Port: ptr.To(gatewayv1.PortNumber(p.BackendPort)),
								},
							},
						},
					},
				},
			},
		},
	}

	if len(gw.Annotations) > 0 {
		route.Annotations = make(map[string]string, len(gw.Annotations))
		for k, v := range gw.Annotations {
			route.Annotations[k] = v
		}
	}

	return route
}

// IsHTTPRouteAccepted returns true when at least one RouteParentStatus
// reports the Accepted condition with status True. Gateway controllers that
// have not observed the route yet leave Parents empty, so an empty slice is
// treated as "not yet accepted".
func IsHTTPRouteAccepted(route *gatewayv1.HTTPRoute) bool {
	for _, parent := range route.Status.Parents {
		for _, cond := range parent.Conditions {
			if cond.Type == string(gatewayv1.RouteConditionAccepted) && cond.Status == metav1.ConditionTrue {
				return true
			}
		}
	}
	return false
}

// EnsureHTTPRoute creates or updates the HTTPRoute via Server-Side Apply
// under the shared field manager and sets the owner CR as its controller
// owner so it is garbage-collected with the CR. The server response is
// decoded back into route, so its parent status — written by the Gateway
// controller — is already populated without a second Get (issue #361).
//
// Merge strategy: the field manager owns exactly the fields the builder sets
// (the whole Spec and the annotations/labels derived from spec.gateway).
// Annotations or labels that disappear from spec.gateway between reconciles
// are relinquished and removed by the API server — no sentinel bookkeeping is
// needed — while keys owned by other managers are preserved. A converged
// HTTPRoute is applied without a write.
func EnsureHTTPRoute(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, route *gatewayv1.HTTPRoute) error {
	return apply.EnsureObject(ctx, c, scheme, owner, route, apply.FieldManager)
}

// DeleteHTTPRoute deletes the HTTPRoute identified by namespace and name.
// It is a no-op if the HTTPRoute does not exist.
func DeleteHTTPRoute(ctx context.Context, c client.Client, namespace, name string) error {
	route := &gatewayv1.HTTPRoute{}
	route.SetName(name)
	route.SetNamespace(namespace)
	if err := client.IgnoreNotFound(c.Delete(ctx, route)); err != nil {
		return fmt.Errorf("deleting HTTPRoute %s/%s: %w", namespace, name, err)
	}
	return nil
}
