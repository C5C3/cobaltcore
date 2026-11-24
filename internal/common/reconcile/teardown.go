// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package reconcile

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

// SweepRemoteChildren is the teardown pass every service operator runs from its
// deletion path: it deletes what a CR projected onto the target cluster it names
// and then releases commonmulticluster.RemoteChildrenFinalizer, the finalizer
// that held the CR in etcd long enough to do it.
//
// It is a no-op for a CR that does not carry the finalizer, which is every CR
// that keeps its children on the management cluster: those are collected from
// their owner references, and only a CR naming a target cluster ever carries it.
// Callers therefore run it unconditionally from their cleanup path.
//
// children is what commonmulticluster.ResolveChildrenClientForDeletion returned.
// A nil client means the target cluster was deregistered and has stayed
// unresolvable past commonmulticluster.AbandonAfter. Its children cannot be
// reached, so they are left running, a Warning event records that they were,
// and the finalizer is released anyway: holding it would only strand the CR in
// Terminating.
//
// The sweep reads through the target cluster's uncached API reader rather than
// through the children client's cache. Its success licenses the finalizer
// release, and a child the cache has not caught up on would be missed, leaving
// no CR behind to delete it.
//
// The release goes through RemoveFinalizer and answers in three ways. A
// released finalizer and an owner already gone (404) return a zero result. A
// 409 returns a requeue after RequeueNextPass and a nil error, so the next pass
// sweeps again from a fresh read. Any other failure returns the error wrapped
// as "removing remote-children finalizer".
func SweepRemoteChildren(
	ctx context.Context,
	mgmt client.Client,
	resolver commonmulticluster.ClusterResolver,
	recorder record.EventRecorder,
	scheme *runtime.Scheme,
	owner client.Object,
	ref *commonv1.TargetClusterRefSpec,
	children client.Client,
	kinds []schema.GroupVersionKind,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(owner, commonmulticluster.RemoteChildrenFinalizer) {
		return ctrl.Result{}, nil
	}

	if children == nil {
		gvk, err := commonmulticluster.OwnerGVK(scheme, owner)
		if err != nil {
			return ctrl.Result{}, err
		}
		recorder.Event(owner, corev1.EventTypeWarning, "RemoteChildrenAbandoned",
			fmt.Sprintf("Target cluster is no longer registered; releasing the remote-children finalizer without "+
				"deleting the objects on it labelled as owned by this %s", gvk.Kind))
	} else {
		reader, err := commonmulticluster.ResolveChildrenAPIReader(ctx, resolver, children, ref)
		if err != nil {
			return ctrl.Result{}, err
		}
		if err := commonmulticluster.DeleteRemoteChildren(ctx, reader, children, scheme, owner, owner.GetNamespace(), kinds); err != nil {
			return ctrl.Result{}, err
		}
	}

	outcome, err := RemoveFinalizer(ctx, mgmt, owner, commonmulticluster.RemoteChildrenFinalizer)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("removing remote-children finalizer: %w", err)
	}
	if outcome == FinalizerConflict {
		return ctrl.Result{RequeueAfter: RequeueNextPass}, nil
	}
	return ctrl.Result{}, nil
}
