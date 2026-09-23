// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	"github.com/c5c3/cobaltcore/internal/common/bootstrap"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	"github.com/c5c3/cobaltcore/internal/common/watch"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
)

// registerNovaComputeIndexes registers the field index every NovaCompute watch
// leg and step resolves the pools of a Nova through.
func registerNovaComputeIndexes(ctx context.Context, indexer client.FieldIndexer) error {
	if err := indexer.IndexField(ctx, &novav1alpha1.NovaCompute{},
		NovaComputeNovaRefIndexKey, novaComputeNovaRefExtractor); err != nil {
		return fmt.Errorf("registering field indexer %q: %w", NovaComputeNovaRefIndexKey, err)
	}
	return nil
}

// requestsFor turns NovaComputes into reconcile requests, skipping the one
// named skip.
func requestsFor(items []novav1alpha1.NovaCompute, skip string) []reconcile.Request {
	var requests []reconcile.Request
	for _, item := range items {
		if item.Name == skip {
			continue
		}
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{Namespace: item.Namespace, Name: item.Name},
		})
	}
	return requests
}

// novaToNovaComputesMapper wakes the pools of a Nova when it changes: its
// installed release, its published contract and its placement feed the NovaRef
// step, and each of them is a status flip rather than a generation bump.
func novaToNovaComputesMapper(c client.Reader) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		// A failed list maps to nothing: the periodic poll catches up. The
		// other mappers below do the same.
		items, _ := novaComputesOfNova(ctx, c, obj.GetNamespace(), obj.GetName())
		return requestsFor(items, "")
	}
}

// novaChangePredicate admits the Nova updates its pools act on: a spec change,
// which covers the service user, the endpoints and the placement, and the two
// status fields the NovaRef step waits on, the installed release and the
// published contract. Both are status flips rather than generation bumps. The
// Nova's other status writes wake no pool.
func novaChangePredicate() predicate.Predicate {
	return predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
		old, okOld := e.ObjectOld.(*novav1alpha1.Nova)
		cur, okNew := e.ObjectNew.(*novav1alpha1.Nova)
		if !okOld || !okNew {
			return true
		}
		return old.Generation != cur.Generation ||
			old.Status.InstalledRelease != cur.Status.InstalledRelease ||
			!equality.Semantic.DeepEqual(old.Status.ComputeConfigSecretRef, cur.Status.ComputeConfigSecretRef)
	}}
}

// rivalChangePredicate admits the NovaCompute updates the other pools of its
// Nova act on: a spec change (the selector), the start of a deletion, and a
// change in which nodes it holds, in which phase and zone. The rest of what a
// pool writes to its status, the instance counts, the service states and the
// DaemonSet counters, wakes no sibling.
func rivalChangePredicate() predicate.Predicate {
	return predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
		old, okOld := e.ObjectOld.(*novav1alpha1.NovaCompute)
		cur, okNew := e.ObjectNew.(*novav1alpha1.NovaCompute)
		if !okOld || !okNew {
			return true
		}
		return old.Generation != cur.Generation ||
			old.DeletionTimestamp.IsZero() != cur.DeletionTimestamp.IsZero() ||
			!slices.EqualFunc(old.Status.Nodes, cur.Status.Nodes, func(a, b novav1alpha1.NovaComputeNodeStatus) bool {
				return a.Name == b.Name && a.Phase == b.Phase && a.Zone == b.Zone
			})
	}}
}

// novaComputeToRivalsMapper wakes the other pools of the same Nova when one
// changes, so a node one of them takes, holds or releases re-evaluates the
// conflicts of the rest. Self is left to the For leg.
func novaComputeToRivalsMapper(c client.Reader) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		cr, ok := obj.(*novav1alpha1.NovaCompute)
		if !ok {
			return nil
		}
		items, _ := novaComputesOfNova(ctx, c, cr.Namespace, cr.Spec.NovaRef.Name)
		return requestsFor(items, cr.Name)
	}
}

// computeConfigSecretToNovaComputesMapper wakes the pools of Nova <nova> when
// the Secret "<nova>-compute-config" in their namespace changes: it is the
// contract their pods mount, on whichever cluster the pods run.
func computeConfigSecretToNovaComputesMapper(c client.Reader) handler.MapFunc {
	suffix := "-" + componentComputeConfig
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		nova, ok := strings.CutSuffix(obj.GetName(), suffix)
		if !ok || nova == "" {
			return nil
		}
		items, _ := novaComputesOfNova(ctx, c, obj.GetNamespace(), nova)
		return requestsFor(items, "")
	}
}

// nodeToPlacedNovaComputesMapper wakes the NovaComputes placed on a target
// cluster that a Node's label change there concerns: the ones whose selector
// matches the node now and the ones that hold it, so a node leaving a pool
// wakes the pool it leaves. RemoteRequests then keeps only the ones placed on
// the cluster the event came from. A local pool has no Node leg: the Node
// informer it needs cannot sync on a namespace-scoped install, and its
// periodic poll picks a relabel up instead.
func nodeToPlacedNovaComputesMapper(c client.Reader) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		var list novav1alpha1.NovaComputeList
		if err := c.List(ctx, &list); err != nil {
			return nil
		}
		nodeLabels := labels.Set(obj.GetLabels())
		var placed []novav1alpha1.NovaCompute
		for _, item := range list.Items {
			if item.Spec.TargetClusterRef == nil {
				continue
			}
			if poolSelector(&item).Matches(nodeLabels) ||
				slices.ContainsFunc(item.Status.Nodes, func(e novav1alpha1.NovaComputeNodeStatus) bool {
					return e.Name == obj.GetName()
				}) {
				placed = append(placed, item)
			}
		}
		return requestsFor(placed, "")
	}
}

// SetupWithManager registers the NovaComputeReconciler with the controller
// manager, with the shared controller options (see
// bootstrap.TypedControllerOptions).
func (r *NovaComputeReconciler) SetupWithManager(mgr mcmanager.Manager) error {
	return r.setupWithOptions(mgr, bootstrap.TypedControllerOptions[mcreconcile.Request](r.MaxConcurrentReconciles))
}

// setupWithOptions carries the production watch wiring SetupWithManager applies.
// The controller options are a parameter so an envtest suite can register this
// exact chain with SkipNameValidation set.
func (r *NovaComputeReconciler) setupWithOptions(mgr mcmanager.Manager, opts crcontroller.TypedOptions[mcreconcile.Request]) error {
	local := mgr.GetLocalManager()

	// The index goes on the LOCAL field indexer: the CRs live on the management
	// cluster alone, and every request the legs emit is pinned to it.
	if err := registerNovaComputeIndexes(context.Background(), local.GetFieldIndexer()); err != nil {
		return err
	}

	engageLocal := commonmulticluster.EngageLocalCluster
	engageNoProviders := commonmulticluster.EngageNoProviderClusters

	targets := commonmulticluster.TargetClusterOf(local.GetClient(),
		func(cr *novav1alpha1.NovaCompute) *commonv1.TargetClusterRefSpec {
			return cr.Spec.TargetClusterRef
		})

	b := mcbuilder.ControllerManagedBy(mgr).
		WithOptions(opts).
		For(&novav1alpha1.NovaCompute{}, mcbuilder.WithPredicates(watch.CRUpdatePredicate()),
			engageLocal, engageNoProviders).
		Owns(&appsv1.DaemonSet{}, engageLocal, engageNoProviders).
		Owns(&corev1.ConfigMap{}, engageLocal, engageNoProviders)

	b, err := commonmulticluster.AddRemoteChildWatches(b, local.GetScheme(), &novav1alpha1.NovaCompute{},
		targets, NovaComputeRemoteChildKinds, nil)
	if err != nil {
		return err
	}

	// The contract Secret, on the management cluster and on the target
	// clusters, where the ControlPlane's mirror lands.
	b, err = commonmulticluster.AddInputWatch(b, local.GetScheme(), targets, &corev1.Secret{},
		computeConfigSecretToNovaComputesMapper(local.GetClient()))
	if err != nil {
		return err
	}

	nodeGVK := corev1.SchemeGroupVersion.WithKind("Node")
	return b.
		// Neither management-cluster leg carries a generation predicate alone:
		// what a pool waits on in a Nova, and what a rival's change means for
		// its siblings, are status changes. Each admits the ones the steps read.
		Watches(&novav1alpha1.Nova{},
			commonmulticluster.LocalRequests(novaToNovaComputesMapper(local.GetClient())),
			engageLocal, engageNoProviders, mcbuilder.WithPredicates(novaChangePredicate())).
		Watches(&novav1alpha1.NovaCompute{},
			commonmulticluster.LocalRequests(novaComputeToRivalsMapper(local.GetClient())),
			engageLocal, engageNoProviders, mcbuilder.WithPredicates(rivalChangePredicate())).
		// A remote-only Node leg, narrowed to label changes so a kubelet status
		// heartbeat does not reconcile every pool of the fleet.
		Watches(&corev1.Node{},
			commonmulticluster.RemoteRequests(nodeToPlacedNovaComputesMapper(local.GetClient()), targets),
			append(commonmulticluster.RemoteWatchOptions(nodeGVK),
				mcbuilder.WithPredicates(predicate.LabelChangedPredicate{}))...).
		// This operator surfaces an unresolvable cluster as a condition and
		// requeues, so the wrapper that turns that error into a successful
		// reconcile stays off.
		WithClusterNotFoundWrapper(false).
		Complete(commonmulticluster.LocalReconciler(r))
}
