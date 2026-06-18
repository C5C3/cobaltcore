// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/c5c3/forge/internal/common/conditions"
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

// defaultHTTPRoutePath is the URL path prefix applied when spec.gateway.path
// is empty.
const defaultHTTPRoutePath = "/"

// managedAnnotationsKey and managedLabelsKey are sentinel annotations that
// record the operator-managed annotation/label key sets on an HTTPRoute.
// On each reconcile, keys present in the previous set but absent from the
// desired set are removed, enabling removal of annotations/labels that
// disappear from spec.gateway. Stored as a comma-separated,
// sorted list of key names. The sentinel annotations themselves are never
// part of the tracked set.
const (
	managedAnnotationsKey = "keystone.openstack.c5c3.io/managed-annotations"
	managedLabelsKey      = "keystone.openstack.c5c3.io/managed-labels"
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
		if err := deleteHTTPRoute(ctx, r.Client, keystone.Namespace, subResourceName(keystone)); err != nil {
			return ctrl.Result{}, fmt.Errorf("deleting HTTPRoute: %w", err)
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

	// Path 1: gateway enabled — create or update the HTTPRoute.
	desired := buildKeystoneHTTPRoute(keystone)
	if err := ensureHTTPRoute(ctx, r.Client, r.Scheme, keystone, desired); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring HTTPRoute: %w", err)
	}

	// Re-fetch the HTTPRoute to read its parent status, which is written by
	// the Gateway controller (not the operator).
	current := &gatewayv1.HTTPRoute{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(desired), current); err != nil {
		return ctrl.Result{}, fmt.Errorf("getting HTTPRoute %s/%s: %w", desired.Namespace, desired.Name, err)
	}

	if isHTTPRouteAccepted(current) {
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
	gw := keystone.Spec.Gateway

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
			Name:      subResourceName(keystone),
			Namespace: keystone.Namespace,
			Labels:    commonLabels(keystone),
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
									Name: gatewayv1.ObjectName(subResourceName(keystone)),
									Port: ptr.To(keystoneAPIPort),
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

// isHTTPRouteAccepted returns true when at least one RouteParentStatus reports
// the Accepted condition with status True. Gateway controllers that have not
// observed the route yet leave Parents empty, so an empty slice is treated as
// "not yet accepted".
func isHTTPRouteAccepted(route *gatewayv1.HTTPRoute) bool {
	for _, parent := range route.Status.Parents {
		for _, cond := range parent.Conditions {
			if cond.Type == string(gatewayv1.RouteConditionAccepted) && cond.Status == metav1.ConditionTrue {
				return true
			}
		}
	}
	return false
}

// ensureHTTPRoute creates an HTTPRoute if it does not exist or updates its
// spec and metadata if it already exists. An owner reference is set so that
// the HTTPRoute is garbage-collected when the Keystone CR is deleted
//
// Merge strategy: .Spec is overwritten with the desired state on every
// reconcile. Labels and annotations use tracked-key merging: operator-managed
// keys are recorded in sentinel annotations so keys removed from spec.gateway
// between reconciles are also removed from the live object,
// while user-added keys not in the managed set are preserved.
func ensureHTTPRoute(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, route *gatewayv1.HTTPRoute) error {
	existing := &gatewayv1.HTTPRoute{}
	err := c.Get(ctx, client.ObjectKeyFromObject(route), existing)

	if apierrors.IsNotFound(err) {
		stampManagedMetadata(route)
		if err := controllerutil.SetControllerReference(owner, route, scheme); err != nil {
			return fmt.Errorf("setting owner reference on HTTPRoute %s/%s: %w", route.Namespace, route.Name, err)
		}
		if err := c.Create(ctx, route); err != nil {
			return fmt.Errorf("creating HTTPRoute %s/%s: %w", route.Namespace, route.Name, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("getting HTTPRoute %s/%s: %w", route.Namespace, route.Name, err)
	}

	before := existing.DeepCopy()

	if err := controllerutil.SetControllerReference(owner, existing, scheme); err != nil {
		return fmt.Errorf("updating owner reference on HTTPRoute %s/%s: %w", existing.Namespace, existing.Name, err)
	}

	applyManagedMetadata(existing, route.Annotations, route.Labels)

	existing.Spec = route.Spec

	if !apiequality.Semantic.DeepEqual(existing.Spec, before.Spec) ||
		!apiequality.Semantic.DeepEqual(hrNormalizeMap(existing.Labels), hrNormalizeMap(before.Labels)) ||
		!apiequality.Semantic.DeepEqual(hrNormalizeMap(existing.Annotations), hrNormalizeMap(before.Annotations)) ||
		!apiequality.Semantic.DeepEqual(existing.OwnerReferences, before.OwnerReferences) {
		if err := c.Update(ctx, existing); err != nil {
			return fmt.Errorf("updating HTTPRoute %s/%s: %w", existing.Namespace, existing.Name, err)
		}
	}

	return nil
}

// stampManagedMetadata records the key sets from route.Annotations and
// route.Labels in sentinel annotations on the route itself before creation.
// These sentinels are consulted on subsequent reconciles to remove keys that
// disappear from the desired set.
func stampManagedMetadata(route *gatewayv1.HTTPRoute) {
	annotationKeys := sortedMapKeys(route.Annotations)
	labelKeys := sortedMapKeys(route.Labels)

	if len(annotationKeys) == 0 && len(labelKeys) == 0 {
		return
	}
	if route.Annotations == nil {
		route.Annotations = make(map[string]string, 2)
	}
	if len(annotationKeys) > 0 {
		route.Annotations[managedAnnotationsKey] = strings.Join(annotationKeys, ",")
	}
	if len(labelKeys) > 0 {
		route.Annotations[managedLabelsKey] = strings.Join(labelKeys, ",")
	}
}

// applyManagedMetadata reconciles the live object's annotations and labels
// against the desired sets. Keys present in the previously-managed set (read
// from the sentinel annotations) but absent from the desired set are removed,
// then desired keys are applied, then the sentinels are updated to reflect the
// new managed set. This is the removal path that was missing from the naive
// additive merge.
func applyManagedMetadata(existing *gatewayv1.HTTPRoute, desiredAnnotations, desiredLabels map[string]string) {
	prevAnnotationKeys := splitManagedKeys(existing.Annotations[managedAnnotationsKey])
	prevLabelKeys := splitManagedKeys(existing.Annotations[managedLabelsKey])

	for _, k := range prevAnnotationKeys {
		if _, stillDesired := desiredAnnotations[k]; !stillDesired {
			delete(existing.Annotations, k)
		}
	}
	for _, k := range prevLabelKeys {
		if _, stillDesired := desiredLabels[k]; !stillDesired {
			delete(existing.Labels, k)
		}
	}

	for k, v := range desiredAnnotations {
		if existing.Annotations == nil {
			existing.Annotations = make(map[string]string, len(desiredAnnotations)+2)
		}
		existing.Annotations[k] = v
	}
	for k, v := range desiredLabels {
		if existing.Labels == nil {
			existing.Labels = make(map[string]string, len(desiredLabels))
		}
		existing.Labels[k] = v
	}

	annotationKeys := sortedMapKeys(desiredAnnotations)
	labelKeys := sortedMapKeys(desiredLabels)
	if len(annotationKeys) > 0 {
		if existing.Annotations == nil {
			existing.Annotations = make(map[string]string, 2)
		}
		existing.Annotations[managedAnnotationsKey] = strings.Join(annotationKeys, ",")
	} else if existing.Annotations != nil {
		delete(existing.Annotations, managedAnnotationsKey)
	}
	if len(labelKeys) > 0 {
		if existing.Annotations == nil {
			existing.Annotations = make(map[string]string, 1)
		}
		existing.Annotations[managedLabelsKey] = strings.Join(labelKeys, ",")
	} else if existing.Annotations != nil {
		delete(existing.Annotations, managedLabelsKey)
	}
}

// splitManagedKeys parses the comma-separated key list stored in a sentinel
// annotation. An empty or missing value produces a nil slice.
func splitManagedKeys(v string) []string {
	if v == "" {
		return nil
	}
	return strings.Split(v, ",")
}

// sortedMapKeys returns the keys of m in lexicographic order so the sentinel
// annotation value is deterministic across reconciles.
func sortedMapKeys(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// hrNormalizeMap converts empty maps to nil so apiequality.Semantic.DeepEqual
// does not report spurious diffs between nil and empty maps.
func hrNormalizeMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	return m
}

// deleteHTTPRoute deletes the HTTPRoute identified by namespace and name.
// It is a no-op if the HTTPRoute does not exist.
func deleteHTTPRoute(ctx context.Context, c client.Client, namespace, name string) error {
	route := &gatewayv1.HTTPRoute{}
	route.SetName(name)
	route.SetNamespace(namespace)
	if err := client.IgnoreNotFound(c.Delete(ctx, route)); err != nil {
		return fmt.Errorf("deleting HTTPRoute %s/%s: %w", namespace, name, err)
	}
	return nil
}
