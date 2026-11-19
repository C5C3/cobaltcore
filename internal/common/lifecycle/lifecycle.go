// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package lifecycle orders the reconcile steps shared by service operators.
package lifecycle

import (
	"context"
	"fmt"

	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/multicluster"
	"github.com/c5c3/cobaltcore/internal/common/reconcile"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

// Fields exposes the service-specific spec and status on a fetched CR.
type Fields[S any] struct {
	Target             *commonv1.TargetClusterRefSpec
	Suspended          bool
	Status             *S
	CopyStatus         func() *S
	ObservedGeneration *int64
}

// Service runs the shared reconciliation stages around a controller’s workload
// and cleanup hooks.
type Service[T interface {
	client.Object
	DeepCopy() T
}, S any] struct {
	Client          client.Client
	Resolver        multicluster.ClusterResolver
	Recorder        record.EventRecorder
	Skeleton        reconcile.Skeleton[T, S]
	Finalizer       string
	TargetCondition string
	Fields          func(T) Fields[S]
	Sync            func(context.Context, client.Client, T) (ctrl.Result, error)
	Finalize        func(context.Context, client.Client, T) (ctrl.Result, error)
	Sweep           func(context.Context, client.Client, T) error
}

// Reconcile resolves the target and observes each required finalizer on a fresh
// read before Sync can create children. After Sync it aggregates Ready and
// writes changed status, even when Sync returns an error.
//
// Deletion takes precedence over suspension. A requeue or error from Finalize
// defers Sweep so service cleanup can finish first. Both cleanup hooks receive
// a nil child client if the target stays unresolved through the abandonment window.
func (s Service[T, S]) Reconcile(ctx context.Context, req ctrl.Request, obj T) (ctrl.Result, error) {
	if err := s.Client.Get(ctx, req.NamespacedName, obj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	fields := s.Fields(obj)
	deletion := obj.GetDeletionTimestamp()
	if deletion.IsZero() {
		if stop, result, err := reconcile.GateSuspend(ctx, s.Client, s.Recorder, obj, s.Skeleton.Conditions(obj), fields.Suspended); stop || err != nil {
			return result, err
		}
	}

	// Resuming may already have persisted the removal of Suspended.
	before := fields.CopyStatus()
	persist := func(result ctrl.Result, err error) (ctrl.Result, error) {
		return s.Skeleton.UpdateStatus(ctx, s.Client, obj, before, fields.Status, func() {
			*fields.ObservedGeneration = obj.GetGeneration()
		}, result, err)
	}
	unavailable := func(err error) (ctrl.Result, error) {
		s.Skeleton.MarkFailed(obj, s.TargetCondition, multicluster.TargetClusterUnavailable, err)
		return persist(ctrl.Result{RequeueAfter: reconcile.RequeueSecretPolling}, nil)
	}

	if !deletion.IsZero() {
		children, wait := multicluster.ResolveChildrenClientForDeletion(ctx, s.Resolver, s.Client, fields.Target, *deletion)
		if wait {
			return unavailable(fmt.Errorf("target cluster %s does not resolve; waiting at least %s before abandoning its children", fields.Target.Name, multicluster.AbandonAfter))
		}
		if result, err := s.Finalize(ctx, children, obj); !result.IsZero() || err != nil {
			return result, err
		}
		return ctrl.Result{}, s.Sweep(ctx, children, obj)
	}

	children, err := multicluster.ResolveChildrenClient(ctx, s.Resolver, s.Client, fields.Target)
	if err != nil {
		return unavailable(err)
	}
	finalizers := []string{s.Finalizer}
	if fields.Target != nil {
		finalizers = append(finalizers, multicluster.RemoteChildrenFinalizer)
	}
	for _, name := range finalizers {
		added, err := reconcile.EnsureFinalizer(ctx, s.Client, obj, name)
		if err != nil {
			return ctrl.Result{}, err
		}
		if added {
			return ctrl.Result{RequeueAfter: reconcile.RequeueNextPass}, nil
		}
	}
	return persist(s.Sync(ctx, children, obj))
}
