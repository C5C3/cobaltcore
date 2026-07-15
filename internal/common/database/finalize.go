// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package database

import (
	"context"
	"fmt"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// resourceCtors is the single source of truth for the set of MariaDB CR kinds
// owned by a database-backed service CR. Both FinalizeResources and
// HasLiveResources iterate over this list, so adding a new kind (e.g. Backup)
// only requires appending one entry here — the cleanup and the sentinel then
// stay in lockstep. Each ctor returns a zeroed pointer suitable for Get/Delete;
// callers set Name and Namespace from the resource key.
var resourceCtors = []func() client.Object{
	func() client.Object { return &mariadbv1alpha1.Database{} },
	func() client.Object { return &mariadbv1alpha1.User{} },
	func() client.Object { return &mariadbv1alpha1.Grant{} },
}

// FinalizeResources issues Delete for the MariaDB Database, User, and Grant CRs
// identified by key. It returns as soon as the Delete requests have been
// accepted (or tolerated as NotFound) so the owning CR's finalizer can be
// released in the same reconcile pass.
//
// The function intentionally does not block on the MariaDB operator completing
// its own teardown. Waiting for the CRs to disappear from etcd created a
// deadlock under concurrent deletions: as long as the owning finalizer held the
// CR in etcd, Kubernetes garbage collection could not cascade-delete the owned
// Deployment, so the service Pod kept its connections open, so the MariaDB
// operator could not DROP DATABASE, so the Database CR stayed Terminating — and
// the finalizer never released. Releasing immediately breaks that cycle: GC
// cascade-deletes the Pod, connections close, and the MariaDB operator completes
// the drop asynchronously. Owner references set at provisioning time guarantee
// the CRs are still cleaned up even if the explicit Delete below is a no-op.
//
// NotFound responses from Delete are treated as success so repeat invocations
// (operator restart, external deletion, prior completed cleanup) are idempotent.
// Brownfield CRs (Host-only, no ClusterRef) flow through the same path: every
// Delete is a no-op NotFound.
func FinalizeResources(ctx context.Context, c client.Client, key client.ObjectKey) error {
	logger := log.FromContext(ctx).WithValues("resource", key)

	for _, ctor := range resourceCtors {
		obj := ctor()
		obj.SetName(key.Name)
		obj.SetNamespace(key.Namespace)
		err := c.Delete(ctx, obj)
		if apierrors.IsNotFound(err) {
			logger.V(1).Info("MariaDB resource already absent, skipping delete",
				"resource", fmt.Sprintf("%T", obj))
			continue
		}
		if err != nil {
			return fmt.Errorf("deleting %T %s: %w", obj, key, err)
		}
	}

	return nil
}

// HasLiveResources reports whether any of the three MariaDB CRs (Database, User,
// Grant) identified by key still exists with DeletionTimestamp unset — i.e., real
// cleanup work remains. Brownfield CRs (no MariaDB CRs ever created) report false
// so a "cleaning up" event can be suppressed when there is nothing to announce.
func HasLiveResources(ctx context.Context, c client.Client, key client.ObjectKey) (bool, error) {
	for _, ctor := range resourceCtors {
		obj := ctor()
		err := c.Get(ctx, key, obj)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("checking %T %s: %w", obj, key, err)
		}
		if obj.GetDeletionTimestamp().IsZero() {
			return true, nil
		}
	}
	return false, nil
}
