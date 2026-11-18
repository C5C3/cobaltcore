// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package lifecycle drives the management-cluster lifecycle of a service CR
// whose workloads can be projected onto a target cluster.
package lifecycle

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	"github.com/c5c3/cobaltcore/internal/common/multicluster"
	"github.com/c5c3/cobaltcore/internal/common/reconcile"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

// Fields points into a single fetched object. The driver snapshots Status
// before running service code and writes it through the management client.
type Fields[S any] struct {
	Target             *commonv1.TargetClusterRefSpec
	Suspended          bool
	Status             *S
	Conditions         *[]metav1.Condition
	ObservedGeneration *int64
}

// Driver orders the service hooks. Sync returns the pipeline result including
// any scheduled health re-probe. Finalize runs before Sweep, and may requeue
// while cleanup is in progress. A nil children client on deletion means the
// target remained unreachable through the abandonment window.
type Driver[T interface {
	client.Object
	DeepCopy() T
}, S any] struct {
	Client                     client.Client
	Resolver                   multicluster.ClusterResolver
	Recorder                   record.EventRecorder
	Finalizer                  string
	ReadyConditions            []string
	TargetUnavailableCondition string
	Fields                     func(T) Fields[S]
	Sync                       func(context.Context, client.Client, T) (ctrl.Result, error)
	Finalize                   func(context.Context, client.Client, T) (ctrl.Result, error)
	Sweep                      func(context.Context, client.Client, T) error
}

// Reconcile fetches obj, handles deletion even when suspended, and resolves the
// target before installing finalizers. Each finalizer is observed on a fresh
// pass before any children can be written.
func (d Driver[T, S]) Reconcile(ctx context.Context, req ctrl.Request, obj T) (ctrl.Result, error) {
	if err := d.Client.Get(ctx, req.NamespacedName, obj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	fields := d.Fields(obj)
	if !obj.GetDeletionTimestamp().IsZero() {
		children, wait := multicluster.ResolveChildrenClientForDeletion(ctx, d.Resolver, d.Client, fields.Target, *obj.GetDeletionTimestamp())
		if wait {
			return d.unavailable(ctx, obj, fields, fmt.Errorf("target cluster %s does not resolve; waiting at least %s before abandoning its children", fields.Target.Name, multicluster.AbandonAfter))
		}
		if result, err := d.Finalize(ctx, children, obj); !result.IsZero() || err != nil {
			return result, err
		}
		return ctrl.Result{}, d.Sweep(ctx, children, obj)
	}

	if stop, result, err := reconcile.GateSuspend(ctx, d.Client, d.Recorder, obj, fields.Conditions, fields.Suspended); stop || err != nil {
		return result, err
	}
	children, err := multicluster.ResolveChildrenClient(ctx, d.Resolver, d.Client, fields.Target)
	if err != nil {
		return d.unavailable(ctx, obj, fields, err)
	}

	finalizers := []string{d.Finalizer}
	if fields.Target != nil {
		finalizers = append(finalizers, multicluster.RemoteChildrenFinalizer)
	}
	for _, name := range finalizers {
		added, err := reconcile.EnsureFinalizer(ctx, d.Client, obj, name)
		if err != nil {
			return ctrl.Result{}, err
		}
		if added {
			return ctrl.Result{RequeueAfter: reconcile.RequeueNextPass}, nil
		}
	}

	before := d.Fields(obj.DeepCopy()).Status
	result, err := d.Sync(ctx, children, obj)
	return d.writeStatus(ctx, obj, fields, before, result, err)
}

func (d Driver[T, S]) unavailable(ctx context.Context, obj T, fields Fields[S], err error) (ctrl.Result, error) {
	before := d.Fields(obj.DeepCopy()).Status
	conditions.SetCondition(fields.Conditions, metav1.Condition{
		Type:               d.TargetUnavailableCondition,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: obj.GetGeneration(),
		Reason:             multicluster.TargetClusterUnavailable,
		Message:            err.Error(),
	})
	return d.writeStatus(ctx, obj, fields, before, ctrl.Result{RequeueAfter: reconcile.RequeueSecretPolling}, nil)
}

func (d Driver[T, S]) writeStatus(ctx context.Context, obj T, fields Fields[S], before *S, result ctrl.Result, err error) (ctrl.Result, error) {
	return reconcile.UpdateStatus(ctx, d.Client, obj, before, fields.Status, func() {
		SetReady(fields.Conditions, obj.GetGeneration(), d.ReadyConditions, err)
		*fields.ObservedGeneration = obj.GetGeneration()
	}, result, err)
}
