// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Sizing sub-reconciler: resolves spec.sizing and reports SizingReady.
package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// referencedSizingProfile reads the SizingProfile spec.sizing.profileRef
// names, or returns nil when the ControlPlane names none. The read goes
// through the cached client, since the kind is watched. A missing profile
// returns an error apierrors.IsNotFound recognizes: the webhook rejects such a
// reference at admission, but a referenced profile can still be deleted later,
// since no admission rule guards the DELETE.
func (r *ControlPlaneReconciler) referencedSizingProfile(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane,
) (*c5c3v1alpha1.SizingProfile, error) {
	s := cp.Spec.Sizing
	if s == nil || s.ProfileRef == nil {
		return nil, nil
	}
	name := s.ProfileRef.Name
	profile := &c5c3v1alpha1.SizingProfile{}
	if err := r.Get(ctx, client.ObjectKey{Name: name}, profile); err != nil {
		return nil, fmt.Errorf("reading SizingProfile %q: %w", name, err)
	}
	return profile, nil
}

// reconcileSizing resolves the ControlPlane's sizing and reports it in
// SizingReady. It runs first in the pipeline: every later step projects a
// child from the resolved sizing, so a sizing that cannot be resolved stops
// the pass and the children keep what they were last projected, following
// the InvalidRotationInterval precedent in reconcileKeystone.
func (r *ControlPlaneReconciler) reconcileSizing(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) (ctrl.Result, error) {
	profile, err := r.referencedSizingProfile(ctx, cp)
	if err != nil {
		reason := "SizingProfileError"
		message := err.Error()
		if apierrors.IsNotFound(err) {
			reason = "SizingProfileNotFound"
			message = fmt.Sprintf("SizingProfile %q not found; the children keep their last projected sizing",
				cp.Spec.Sizing.ProfileRef.Name)
		}
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeSizingReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             reason,
			Message:            message,
		})
		return ctrl.Result{}, err
	}

	base := c5c3v1alpha1.EffectiveSizingBase(cp, profile)
	message := fmt.Sprintf("sizing resolved from built-in profile %q", base)
	if profile != nil {
		message = fmt.Sprintf("sizing resolved from SizingProfile %q (base %q)", profile.Name, base)
	}
	conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditionTypeSizingReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: cp.Generation,
		Reason:             "SizingResolved",
		Message:            message,
	})
	return ctrl.Result{}, nil
}

// sizingProfileToControlPlaneMapper maps a SizingProfile event onto the
// ControlPlanes whose spec.sizing.profileRef names it, so an edit or a
// deletion of the profile re-resolves their sizing without waiting for a
// periodic resync. The profile is cluster-scoped and carries no owner
// reference, hence the cluster-wide List.
func (r *ControlPlaneReconciler) sizingProfileToControlPlaneMapper(ctx context.Context, obj client.Object) []reconcile.Request {
	var list c5c3v1alpha1.ControlPlaneList
	if err := r.List(ctx, &list); err != nil {
		// Surface the failure rather than silently dropping the event: an
		// unhealthy informer cache would otherwise leave SizingReady and the
		// projected sizing stale until the next periodic resync.
		log.FromContext(ctx).Error(err, "listing ControlPlanes for SizingProfile event",
			"sizingProfile", obj.GetName())
		return nil
	}
	var requests []reconcile.Request
	for i := range list.Items {
		cp := &list.Items[i]
		if s := cp.Spec.Sizing; s != nil && s.ProfileRef != nil && s.ProfileRef.Name == obj.GetName() {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(cp)})
		}
	}
	return requests
}
