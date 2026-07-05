// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package reconcile

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
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
