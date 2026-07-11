// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package reconcile

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/forge/internal/common/apply"
	"github.com/c5c3/forge/internal/common/conditions"
)

// ChildProjectionParams carries the inputs of ProjectChild: the fully-built
// desired child CR, the readiness condition to drive, and its reason/message
// vocabulary. An orchestrating operator (the ControlPlane) projects a child
// service CR — a Keystone, a Horizon — and mirrors its readiness; this captures
// the five-step projector every such projection repeats.
type ChildProjectionParams[T client.Object] struct {
	// Child is the fully-built desired child; ProjectChild applies it via SSA
	// (so the builder must be a pure projection of the owner spec, reading no
	// live child state) and decodes the server response back into it, so its
	// status is populated for the readiness mirror.
	Child T
	// ConditionType is the readiness condition the projection reports on (for
	// example "KeystoneReady").
	ConditionType string
	// ReadyReason / ReadyMessage describe a ready child.
	ReadyReason  string
	ReadyMessage string
	// WaitingReason / WaitingMessage describe a not-yet-ready child.
	WaitingReason  string
	WaitingMessage string
	// RejectedReason / RejectedMessage describe an Invalid (HTTP 422) rejection —
	// typically an immutable child field the projected change would violate. Kept
	// distinct from ErrorReason so the wedge is diagnosable from the condition.
	RejectedReason  string
	RejectedMessage func(error) string
	// ErrorReason / ErrorMessage describe any other apply error.
	ErrorReason  string
	ErrorMessage func(error) string
	// WaitRequeue is the interval to requeue while the child is not ready.
	WaitRequeue time.Duration
	// Conditions is the owner's condition slice, mutated in place.
	Conditions *[]metav1.Condition
	// Generation is the owner generation stamped onto every condition written.
	Generation int64
	// ChildConditions extracts the child's status conditions for the readiness
	// mirror.
	ChildConditions func(T) []metav1.Condition
}

// ProjectChild applies the desired child via Server-Side Apply under the shared
// field manager (which sets ownership), classifies an Invalid rejection
// distinctly from other errors, mirrors the child's readiness into the owner's
// condition, and requeues while the child is not ready. It is the generic form
// of the ControlPlane's per-service projection so onboarding a new service adds
// configuration rather than another copy.
func ProjectChild[T client.Object](ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, p ChildProjectionParams[T]) (ctrl.Result, error) {
	if err := apply.EnsureObject(ctx, c, scheme, owner, p.Child, apply.FieldManager); err != nil {
		reason := p.ErrorReason
		message := p.ErrorMessage(err)
		if apierrors.IsInvalid(err) {
			reason = p.RejectedReason
			message = p.RejectedMessage(err)
		}
		conditions.SetCondition(p.Conditions, metav1.Condition{
			Type:               p.ConditionType,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: p.Generation,
			Reason:             reason,
			Message:            message,
		})
		return ctrl.Result{}, err
	}

	if !conditions.IsReady(p.ChildConditions(p.Child)) {
		conditions.SetCondition(p.Conditions, metav1.Condition{
			Type:               p.ConditionType,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: p.Generation,
			Reason:             p.WaitingReason,
			Message:            p.WaitingMessage,
		})
		return ctrl.Result{RequeueAfter: p.WaitRequeue}, nil
	}

	conditions.SetCondition(p.Conditions, metav1.Condition{
		Type:               p.ConditionType,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: p.Generation,
		Reason:             p.ReadyReason,
		Message:            p.ReadyMessage,
	})
	return ctrl.Result{}, nil
}

// DeleteOrphanedChild deletes a previously-projected child that the owner no
// longer manages, but only when the owner still controls it (a colliding
// externally-owned object is left alone). It is NotFound-tolerant and uses
// background propagation so the child's own resources are garbage-collected
// behind it. child is a zero-valued object of the child type with its Name and
// Namespace set.
func DeleteOrphanedChild(ctx context.Context, c client.Client, owner, child client.Object) error {
	key := client.ObjectKeyFromObject(child)
	if err := c.Get(ctx, key, child); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("getting %T %s for orphan cleanup: %w", child, key, err)
	}
	if !metav1.IsControlledBy(child, owner) {
		// Not our child (externally managed with a colliding name) — leave it.
		return nil
	}
	if err := c.Delete(ctx, child, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting orphaned %T %s: %w", child, key, err)
	}
	return nil
}
