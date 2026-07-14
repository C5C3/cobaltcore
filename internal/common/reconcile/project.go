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
	// Unowned applies the child WITHOUT an owner reference. Set it when the child
	// lives in a different namespace than the owner: Kubernetes rejects a
	// cross-namespace controller owner reference, so such a child cannot be owned
	// and the apply would fail before it reached the API server. The caller must
	// then carry ownership itself — stamp the child with resolvable ownership
	// labels before handing it over, and delete it explicitly at teardown, since
	// no GC cascade reaches it.
	Unowned bool
}

// ProjectChild applies the desired child via Server-Side Apply under the shared
// field manager (which sets ownership), classifies an Invalid rejection
// distinctly from other errors, mirrors the child's readiness into the owner's
// condition, and requeues while the child is not ready. It is the generic form
// of the ControlPlane's per-service projection so onboarding a new service adds
// configuration rather than another copy.
func ProjectChild[T client.Object](ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, p ChildProjectionParams[T]) (ctrl.Result, error) {
	ensure := func() error {
		if p.Unowned {
			return apply.EnsureUnownedObject(ctx, c, scheme, p.Child, apply.FieldManager)
		}
		return apply.EnsureObject(ctx, c, scheme, owner, p.Child, apply.FieldManager)
	}
	if err := ensure(); err != nil {
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
	return DeleteOrphanedChildFunc(ctx, c, child, func(live client.Object) bool {
		return metav1.IsControlledBy(live, owner)
	})
}

// DeleteOrphanedChildFunc is DeleteOrphanedChild with the ownership test supplied
// by the caller. An owner reference is the right test only while the child shares
// the owner's namespace — Kubernetes forbids a cross-namespace controller
// reference, so a child placed in another namespace carries none and must be
// recognized by the ownership labels the projection stamped on it instead.
// controls decides, from the LIVE object, whether this owner may delete it; a
// colliding object it does not control is left alone.
func DeleteOrphanedChildFunc(ctx context.Context, c client.Client, child client.Object, controls func(client.Object) bool) error {
	key := client.ObjectKeyFromObject(child)
	if err := c.Get(ctx, key, child); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("getting %T %s for orphan cleanup: %w", child, key, err)
	}
	if !controls(child) {
		// Not our child (externally managed with a colliding name) — leave it.
		return nil
	}
	if err := c.Delete(ctx, child, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting orphaned %T %s: %w", child, key, err)
	}
	return nil
}
