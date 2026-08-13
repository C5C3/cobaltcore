// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package multicluster

import (
	"context"
	"fmt"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mchandler "sigs.k8s.io/multicluster-runtime/pkg/handler"
	mcruntime "sigs.k8s.io/multicluster-runtime/pkg/multicluster"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	commonv1 "github.com/c5c3/forge/internal/common/types"
)

// ChildToOwner maps a child on a target cluster back to the CR that owns it,
// reading the ownership labels. It is the target-cluster counterpart of
// handler.EnqueueRequestForOwner, which has nothing to go on where a child
// carries no owner reference.
//
// An object whose owner-kind is not ownerKind, or which is missing either half
// of its owner's name, maps to no request at all. Both cases are ordinary: the
// children of every CR projecting into a shared target namespace arrive through
// the same watch, and an object nobody stamped is nobody's child.
//
// So does an object claiming an owner in another namespace. A child lands in
// its owner's own namespace — that is what Claim stamps and what the teardown
// sweep lists by — so the labels are trusted only where the operator's own
// writes could have put them. Without that constraint the trust the labels
// carry inside a projected namespace would extend to every namespace of the
// cluster: anyone able to create a labelled object anywhere on it could name an
// arbitrary CR and have the operator reconcile it on their timing.
//
// The namespace is as far as this can go on its own. Write access inside a
// projected namespace amounts to control of the children only on the cluster
// the CR actually projects onto, and nothing an object carries says which
// cluster it was seen on. That half of the check belongs to the watch leg,
// where the engaged cluster is known: see RemoteRequests.
func ChildToOwner(ownerKind string) handler.MapFunc {
	return func(_ context.Context, obj client.Object) []reconcile.Request {
		labels := obj.GetLabels()
		if labels[OwnerKindLabel] != ownerKind {
			return nil
		}

		name, namespace := labels[OwnerNameLabel], labels[OwnerNamespaceLabel]
		if name == "" || namespace == "" || obj.GetNamespace() != namespace {
			return nil
		}

		return []reconcile.Request{{
			NamespacedName: types.NamespacedName{Namespace: namespace, Name: name},
		}}
	}
}

// LocalRequests turns a map function into the event-handler factory a
// multicluster watch takes, pinning every request it produces to the local
// cluster. The factory ignores which cluster it is engaged for, because the CR
// a request names never lives there: an operator's CRs are all on the
// management cluster, whatever cluster their children were projected onto.
//
// The empty ClusterName is what holds the workqueue to one key per CR. A
// request is deduplicated by its whole value, cluster name included, so a child
// event from cluster A and an event on the CR itself would otherwise become two
// items and reconcile the same CR concurrently. The upstream
// EnqueueRequestsFromMapFunc cannot produce that, since it overwrites the
// cluster name with the one the event was delivered from; the preserving
// variant leaves the choice to the map function, and this one always chooses
// local.
func LocalRequests(fn handler.MapFunc) mchandler.TypedEventHandlerFunc[client.Object, mcreconcile.Request] {
	return func(_ mcruntime.ClusterName, _ cluster.Cluster) handler.TypedEventHandler[client.Object, mcreconcile.Request] {
		return mchandler.TypedEnqueueRequestsFromMapFuncWithClusterPreservation(
			func(ctx context.Context, obj client.Object) []mcreconcile.Request {
				var requests []mcreconcile.Request
				for _, req := range fn(ctx, obj) {
					requests = append(requests, mcreconcile.Request{Request: req})
				}
				return requests
			})
	}
}

// TargetClusterFunc reports the name of the target cluster the CR at key
// projects onto, read from the management cluster where that CR lives. A CR
// naming no target cluster keeps its children local and answers with the empty
// string, which no engaged provider cluster is called.
//
// It is what a remote watch leg gates its requests on, so it has to answer from
// the CR itself: everything a target cluster carries is writable by whoever can
// write to that cluster, and that is precisely the claim being checked.
type TargetClusterFunc func(ctx context.Context, key types.NamespacedName) (string, error)

// TargetClusterOf builds the TargetClusterFunc of one CR type from the
// management cluster's reader and the CR's own target-cluster ref. Every
// converted operator needs the same lookup over a different CR type, and the
// two lines a call site spends on it are the accessor, which is the only part
// that differs.
//
// The read costs no API call and no second informer: a controller's own For leg
// already holds every CR of its kind in the local cache this reads through.
func TargetClusterOf[T any, PT interface {
	*T
	client.Object
}](c client.Reader, ref func(PT) *commonv1.TargetClusterRefSpec) TargetClusterFunc {
	return func(ctx context.Context, key types.NamespacedName) (string, error) {
		cr := PT(new(T))
		if err := c.Get(ctx, key, cr); err != nil {
			return "", err
		}
		if target := ref(cr); target != nil {
			return target.Name, nil
		}
		return "", nil
	}
}

// RemoteRequests is LocalRequests for a leg watching the target clusters: it
// pins every request to the local cluster the same way, and drops the ones
// naming a CR that does not project onto the cluster the event came from.
//
// That second half is the only place the cluster dimension can be checked at
// all. A leg is engaged on every provider cluster the builder offers it, not on
// the ones some CR happens to name, so without this gate every registered
// cluster is an event source for every CR — and what a leg maps by is the
// ownership labels or a name, both of which anyone able to create an object on
// any target cluster can write. Toggling such an object in a loop would pin
// somebody else's CR to continuous reconciliation, against the management
// cluster, its own target, and every backing service behind it. The per-item
// rate limiter is no help there: it backs off items that errored, and these
// reconciles succeed.
//
// A CR that cannot be read is not reconciled. NotFound is the ordinary answer —
// a child outliving the CR that projected it, an event naming a CR that never
// existed — and is silent. Anything else is logged, because a CR that exists
// and cannot be read is a leg that has quietly stopped correcting drift.
func RemoteRequests(
	fn handler.MapFunc,
	targets TargetClusterFunc,
) mchandler.TypedEventHandlerFunc[client.Object, mcreconcile.Request] {
	return func(name mcruntime.ClusterName, cl cluster.Cluster) handler.TypedEventHandler[client.Object, mcreconcile.Request] {
		return LocalRequests(func(ctx context.Context, obj client.Object) []reconcile.Request {
			var requests []reconcile.Request
			for _, req := range fn(ctx, obj) {
				target, err := targets(ctx, req.NamespacedName)
				if err != nil {
					if !apierrors.IsNotFound(err) {
						log.FromContext(ctx).Error(err, "reading the CR an event on a target cluster names; "+
							"dropping the event rather than reconciling on the strength of the object alone",
							"cr", req.NamespacedName, "cluster", name)
					}
					continue
				}
				if target == string(name) {
					requests = append(requests, req)
				}
			}
			return requests
		})(name, cl)
	}
}

// LocalReconciler adapts a classic reconciler to the multicluster request type
// by dropping the cluster name and forwarding the request as it stands. Every
// converted controller reconciles CRs on the management cluster only, so its
// Reconcile keeps the signature it has, and the cluster a CR's children belong
// on is read from the CR's own target-cluster ref rather than from the request.
func LocalReconciler(r reconcile.Reconciler) mcreconcile.Reconciler {
	return mcreconcile.Func(func(ctx context.Context, req mcreconcile.Request) (reconcile.Result, error) {
		return r.Reconcile(ctx, req.Request)
	})
}

// ClusterServesKind reports, per engaged cluster, whether that cluster's
// RESTMapper serves gvk. It is the filter a watch leg has to carry on any kind
// not every target cluster installs: without it, a leg on an absent kind fails
// the engagement of that whole cluster and the provider retries it under
// backoff, so one missing CRD costs every other watch on that cluster too. With
// it the leg alone is skipped and the rest of the engagement proceeds.
//
// The filter is asked once, when the cluster is engaged. A CRD installed after
// that is watched only once the cluster is engaged again.
//
// A no-match answer — the cluster genuinely does not serve the kind — is the
// ordinary one this filter is for. Discovery can also fail for reasons that say
// nothing about gvk: the target's API server briefly unreachable, a 429 under
// throttling, an aggregated APIService reporting Available=False, which fails
// the group lookup for every group the mapper has not already cached. Those
// answers skip the leg too, because the two ways of being wrong are not the
// same size. A leg skipped on a kind the cluster does serve costs that one
// kind's drift correction on that one cluster until the cluster is engaged
// again — the latch a CRD installed later already sits behind. A leg engaged on
// a kind the cluster does not serve costs the engagement of the whole cluster,
// and where the discovery failure is a persistent one — a broken APIService
// stays broken until somebody fixes it — the provider's retry meets the same
// answer every time, so every CR projecting onto that cluster loses every watch
// it has, for as long as it lasts, over one optional kind. The error is logged,
// because a leg missing for this reason is worth reading the reason for.
//
// The mapping probe is spelled out here rather than taken from
// gateway.IsGVKAvailable, which is the same check: the gateway package reaches
// this one through apply and so cannot be imported back.
func ClusterServesKind(gvk schema.GroupVersionKind) mcbuilder.ClusterFilterFunc {
	return func(name mcruntime.ClusterName, cl cluster.Cluster) bool {
		mapper := cl.GetRESTMapper()
		if mapper == nil {
			return false
		}

		_, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err == nil {
			return true
		}
		if !meta.IsNoMatchError(err) {
			log.Log.WithName("multicluster-watch").Error(err,
				"probing whether a target cluster serves a kind failed; skipping the leg until the cluster "+
					"is engaged again rather than risking the engagement of the whole cluster",
				"cluster", name, "gvk", gvk)
		}
		return false
	}
}

// EngageLocalCluster and EngageNoProviderClusters are the option pair every leg
// watching the management cluster carries, and has to: once a provider is
// configured, the builder's per-leg default is the inverse of the
// single-cluster one — provider clusters yes, local no — so an unpinned leg
// would quietly stop watching the management cluster, taking every CR event
// with it. They are the local counterpart of RemoteWatchOptions, named here so
// the reason lives in one place rather than in a comment beside every leg.
//
// Both are values built from an immutable option, so sharing them across every
// controller is safe.
var (
	EngageLocalCluster       = mcbuilder.WithEngageWithLocalCluster(true)
	EngageNoProviderClusters = mcbuilder.WithEngageWithProviderClusters(false)
)

// RemoteWatchOptions is the option set every leg watching a target cluster
// carries: provider clusters only, and among those only the ones serving gvk.
// Spelling the three in one place is what keeps them together, because each is
// silent on its own — a leg left on the builder's default watches the wrong
// side, and one without the filter fails the whole engagement of a cluster
// missing the kind rather than just itself.
func RemoteWatchOptions(gvk schema.GroupVersionKind) []mcbuilder.WatchesOption {
	return []mcbuilder.WatchesOption{
		mcbuilder.WithEngageWithLocalCluster(false),
		mcbuilder.WithEngageWithProviderClusters(true),
		mcbuilder.WithClusterFilter(ClusterServesKind(gvk)),
	}
}

// AddInputWatch adds the pair of legs an input object needs — one on the
// management cluster, one on the clusters a CR can project onto — both mapping
// obj's events through fn, and returns the builder so the call chains.
//
// The remote leg is not the child watch over again. An input is written by
// something other than the operator: ESO writes a Secret, the MariaDB operator
// its cluster CR. Neither carries the ownership labels, so ChildToOwner maps
// them to nothing and only a leg of their own makes a rotation or an outage on
// the target reach the CR at watch latency rather than at the next periodic
// requeue, exactly as it does management-side.
//
// The same fn serves both legs because every caller's mappers resolve CRs
// through field indexes on the LOCAL cache, which is where the CRs are whatever
// cluster delivered the event.
//
// The kind the remote leg filters clusters by is resolved from obj through the
// scheme rather than spelled out beside it. The two have to describe the same
// kind and nothing would have checked that: a filter naming a kind the target
// serves while the leg watches one it does not engages the leg anyway, which is
// the whole-cluster engagement failure ClusterServesKind exists to prevent, and
// the mismatch the other way drops the input watch without a word.
//
// targets gates the remote leg alone (see RemoteRequests). An input is named
// rather than labelled, so what the mapper matches on is a name somebody else
// can take on a cluster the CR never projects onto; the local leg needs no gate,
// because the management cluster is where the CR itself lives.
func AddInputWatch(
	b *mcbuilder.Builder,
	scheme *runtime.Scheme,
	targets TargetClusterFunc,
	obj client.Object,
	fn handler.MapFunc,
) (*mcbuilder.Builder, error) {
	gvk, err := apiutil.GVKForObject(obj, scheme)
	if err != nil {
		return nil, fmt.Errorf("resolving input kind for remote watch: %w", err)
	}

	return b.
		Watches(obj, LocalRequests(fn), EngageLocalCluster, EngageNoProviderClusters).
		Watches(obj, RemoteRequests(fn, targets), RemoteWatchOptions(gvk)...), nil
}

// AddRemoteChildWatches adds one watch leg per projected kind to b: provider
// clusters only, mapping a labelled child back to the CR its ownership labels
// name, and skipping the clusters that do not serve the kind. It returns the
// builder, so the call chains with the rest of a controller's setup, and leaves
// b untouched when kinds is empty.
//
// kinds is the operator's own sweep list, the one its teardown deletes through.
// Driving both from a single list is the point: a kind added to a projection
// later gets its watch and its teardown from the same edit, and a kind that is
// swept but not watched is a child whose drift nothing notices.
//
// The owner is taken as an object and its kind resolved through the scheme,
// the same way the write side derives the label it stamps. A hand-typed kind
// would be a second source of truth for one value, and getting it wrong costs
// no compile error and no runtime error — every child would map to nothing and
// the drift correction this adds would simply be off.
//
// targets is what keeps a leg engaged on one cluster from waking a CR that
// projects onto another (see RemoteRequests). The ownership labels a child is
// matched by are writable by anyone who can create an object on any registered
// cluster, and only the CR itself says which cluster its children belong on.
//
// extra carries per-kind options a caller needs on top of the three set here, a
// predicate for instance. A nil map means none for any kind. A key naming a
// kind outside kinds is refused rather than ignored: the options would be
// silently dropped, and a predicate that vanishes reads as reconcile churn in
// production rather than as a wiring mistake.
func AddRemoteChildWatches(
	b *mcbuilder.Builder,
	scheme *runtime.Scheme,
	owner client.Object,
	targets TargetClusterFunc,
	kinds []schema.GroupVersionKind,
	extra map[schema.GroupVersionKind][]mcbuilder.WatchesOption,
) (*mcbuilder.Builder, error) {
	resolved, err := ownerGVK(scheme, owner)
	if err != nil {
		return nil, err
	}
	ownerKind := resolved.Kind

	for gvk := range extra {
		if !slices.Contains(kinds, gvk) {
			return nil, fmt.Errorf("remote watch options given for %s, which is not a projected child kind", gvk)
		}
	}

	for _, gvk := range kinds {
		obj, err := scheme.New(gvk)
		if err != nil {
			return nil, fmt.Errorf("building remote watch object for %s: %w", gvk, err)
		}

		child, ok := obj.(client.Object)
		if !ok {
			return nil, fmt.Errorf("building remote watch object for %s: %T carries no object metadata", gvk, obj)
		}

		opts := append(RemoteWatchOptions(gvk), extra[gvk]...)

		b = b.Watches(child, RemoteRequests(ChildToOwner(ownerKind), targets), opts...)
	}

	return b, nil
}
