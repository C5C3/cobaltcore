// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package reconcile

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// EnsureFinalizer installs finalizer on obj and persists the Update when it
// is not present yet, reporting added=true so the caller can requeue: the
// next reconcile then observes the persisted finalizer rather than relying on
// the in-memory copy. When the finalizer is already present it is a no-op
// with added=false.
func EnsureFinalizer(ctx context.Context, c client.Client, obj client.Object, finalizer string) (added bool, err error) {
	if controllerutil.ContainsFinalizer(obj, finalizer) {
		return false, nil
	}
	controllerutil.AddFinalizer(obj, finalizer)
	if err := c.Update(ctx, obj); err != nil {
		return false, fmt.Errorf("adding finalizer %q: %w", finalizer, err)
	}
	return true, nil
}

// FinalizerRemoval is what RemoveFinalizer reports about its Update.
type FinalizerRemoval int

const (
	// FinalizerRemovalFailed accompanies a non-nil error.
	FinalizerRemovalFailed FinalizerRemoval = iota
	// FinalizerRemoved: the Update persisted, or obj carried none of the
	// finalizers and nothing was written.
	FinalizerRemoved
	// FinalizerObjectGone: the API server answered 404 Not Found.
	FinalizerObjectGone
	// FinalizerConflict: the API server answered 409 Conflict and nothing
	// was written; the caller requeues after RequeueNextPass.
	FinalizerConflict
)

// RemoveFinalizer drops finalizers from obj and persists the change with one
// Update. It writes nothing and reports FinalizerRemoved when obj carries none
// of them, or when no name is passed.
//
// Both answers of the API server that a cache read trailing etcd produces end
// without an error. A 404 means an earlier pass already released the object
// and it left etcd, so nothing is left to release: FinalizerObjectGone, logged
// at V(1). A 409 means obj is older than the stored object, so every decision
// of the pass rests on a stale read: FinalizerConflict, logged as the Info
// line "finalizer removal conflict, requeueing", and the caller requeues after
// RequeueNextPass to decide again from a fresh read. Any other error comes
// back unwrapped with FinalizerRemovalFailed, so the caller's own wrapper is
// its only prefix.
//
// After a 404 or a 409 the in-memory obj no longer lists the named finalizers
// although nothing was persisted. After a 409 the caller returns and decides
// again from a fresh read. After a 404 a caller may go on to release its other
// finalizers from the same obj, and each of those Updates meets the same 404.
func RemoveFinalizer(ctx context.Context, c client.Client, obj client.Object, finalizers ...string) (FinalizerRemoval, error) {
	removed := false
	for _, finalizer := range finalizers {
		if controllerutil.RemoveFinalizer(obj, finalizer) {
			removed = true
		}
	}
	if !removed {
		return FinalizerRemoved, nil
	}
	if err := c.Update(ctx, obj); err != nil {
		if apierrors.IsNotFound(err) {
			log.FromContext(ctx).V(1).Info("finalizer removal found the object gone")
			return FinalizerObjectGone, nil
		}
		if apierrors.IsConflict(err) {
			log.FromContext(ctx).Info("finalizer removal conflict, requeueing", "error", err)
			return FinalizerConflict, nil
		}
		return FinalizerRemovalFailed, err
	}
	return FinalizerRemoved, nil
}
