// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// projectedChildren names one projected satellite kind of one service.
type projectedChildren struct {
	List      client.ObjectList   // an empty typed list, e.g. &glancev1alpha1.GlanceBackendList{}
	Kind      string              // "GlanceBackend": the kind name every wrapped error carries
	Namespace string              // the namespace the service's children live in
	Prefix    string              // the name prefix every projected child carries, e.g. glanceBackendNamePrefix(cp)
	Keep      map[string]struct{} // names never deleted and never reported; nil keeps nothing
}

// ownedProjectedChildren lists p.List in p.Namespace on r.Client and returns, in
// list order, the objects this ControlPlane owns (isControlPlaneChild) whose name
// carries p.Prefix and is not in p.Keep. The List error is returned unwrapped; the
// callers name their own phase.
func (r *ControlPlaneReconciler) ownedProjectedChildren(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane, p projectedChildren,
) ([]client.Object, error) {
	if err := r.List(ctx, p.List, client.InNamespace(p.Namespace)); err != nil {
		return nil, err
	}
	items, err := meta.ExtractList(p.List)
	if err != nil {
		return nil, err
	}

	var owned []client.Object
	for _, item := range items {
		obj, ok := item.(client.Object)
		if !ok {
			continue
		}
		if !isControlPlaneChild(obj, cp) || !strings.HasPrefix(obj.GetName(), p.Prefix) {
			continue
		}
		if _, kept := p.Keep[obj.GetName()]; kept {
			continue
		}
		owned = append(owned, obj)
	}
	return owned, nil
}

// pruneProjectedChildren deletes every owned, prefixed child not in p.Keep
// (background propagation, NotFound ignored). A List error is wrapped
// "listing <Kind>s in %q for prune: %w", a Delete error "pruning undeclared
// <Kind> %s/%s: %w" (namespace, name).
func (r *ControlPlaneReconciler) pruneProjectedChildren(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane, p projectedChildren,
) error {
	owned, err := r.ownedProjectedChildren(ctx, cp, p)
	if err != nil {
		return fmt.Errorf("listing %ss in %q for prune: %w", p.Kind, p.Namespace, err)
	}
	for _, obj := range owned {
		if err := client.IgnoreNotFound(
			r.Delete(ctx, obj, client.PropagationPolicy(metav1.DeletePropagationBackground)),
		); err != nil {
			return fmt.Errorf("pruning undeclared %s %s/%s: %w", p.Kind, obj.GetNamespace(), obj.GetName(), err)
		}
	}
	return nil
}

// sweepProjectedChildren is the teardown shape. An absent CRD (meta.IsNoMatchError
// on the List) is nothing to sweep and returns (nil, nil); any other List error is
// wrapped "listing <Kind>s in %q for cross-namespace teardown: %w". Every owned,
// prefixed child not in p.Keep is deleted when its DeletionTimestamp is zero (a
// Delete error is wrapped "deleting <Kind> %s/%s: %w") and, deleted or already
// Terminating, reported as "<namespace>/<name>" so the caller's finalizer waits for
// it. Nothing owned returns (nil, nil).
func (r *ControlPlaneReconciler) sweepProjectedChildren(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane, p projectedChildren,
) ([]string, error) {
	owned, err := r.ownedProjectedChildren(ctx, cp, p)
	if err != nil {
		if meta.IsNoMatchError(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("listing %ss in %q for cross-namespace teardown: %w", p.Kind, p.Namespace, err)
	}

	var remaining []string
	for _, obj := range owned {
		if obj.GetDeletionTimestamp().IsZero() {
			if err := client.IgnoreNotFound(
				r.Delete(ctx, obj, client.PropagationPolicy(metav1.DeletePropagationBackground)),
			); err != nil {
				return nil, fmt.Errorf("deleting %s %s/%s: %w", p.Kind, obj.GetNamespace(), obj.GetName(), err)
			}
		}
		remaining = append(remaining, fmt.Sprintf("%s/%s", obj.GetNamespace(), obj.GetName()))
	}
	return remaining, nil
}
