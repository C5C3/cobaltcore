// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package multicluster

import (
	"context"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/log"
	mcruntime "sigs.k8s.io/multicluster-runtime/pkg/multicluster"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

// ClusterResolver looks up a registered cluster by name. It is the one method
// ResolveChildrenClient needs from the multicluster manager, and
// mcmanager.Manager satisfies it as-is. Taking the narrow interface rather
// than the manager keeps the reconcilers testable with a fake.
type ClusterResolver interface {
	GetCluster(ctx context.Context, name mcruntime.ClusterName) (cluster.Cluster, error)
}

// TargetClusterUnavailable is the condition reason every service operator sets
// on its first gate condition when the target cluster a CR names cannot be
// resolved. Sharing the reason keeps the failure recognizable across
// operators, whatever the CR's first condition type happens to be.
const TargetClusterUnavailable = "TargetClusterUnavailable"

// ResolveChildrenClient returns the client an operator writes a CR's children
// with. A nil resolver (the operator runs without a multicluster manager) or a
// nil ref (the CR names no target cluster) both select local, so a
// single-cluster deployment never leaves the management cluster.
//
// A named cluster's client comes back marked as remote, and the local one comes
// back unmarked. That mark is what Claim reads to decide whether a child is
// owned by a controller owner reference or by the ownership labels, so a write
// site never has to know which cluster it is writing to. The mark also carries
// that cluster's uncached API reader, which LiveReader hands to the write sites
// that must decide from live state rather than from a cache.
//
// The resolver's error is returned unwrapped. Callers put its text straight
// into a status condition under the TargetClusterUnavailable reason, where the
// upstream message ("cluster not found" for an unregistered name) is the part
// an operator reading the CR needs. Wrapping it, as the rest of this repo
// does, would only prefix that message with a constant.
func ResolveChildrenClient(
	ctx context.Context,
	resolver ClusterResolver,
	local client.Client,
	ref *commonv1.TargetClusterRefSpec,
) (client.Client, error) {
	if resolver == nil || ref == nil {
		return local, nil
	}

	cl, err := resolver.GetCluster(ctx, mcruntime.ClusterName(ref.Name))
	if err != nil {
		return nil, err
	}

	return remoteClient{Client: cl.GetClient(), reader: cl.GetAPIReader()}, nil
}

// AbandonAfter is how long a terminating CR keeps waiting for a target cluster
// that does not resolve before its children are given up. It has to outlast
// cluster engagement, which is asynchronous: the provider's own controller and
// the reconcilers all start as ordinary manager runnables, so for the first
// moments after an operator restart a registered cluster is indistinguishable
// from a deregistered one.
//
// It is a var only so tests, whose CRs are deleted at wall-clock now, can
// compress the window into their own timeout budget. No operator binary
// assigns it.
var AbandonAfter = 5 * time.Minute

// unresolvableSince records, per target-cluster name, when this operator
// process first failed to resolve that cluster. The abandon window has to run
// from here and not from the CR's deletion timestamp alone, because the two
// measure different things: the timestamp says how long the CR has been
// terminating, which is already past AbandonAfter for any CR whose cleanup
// legitimately took minutes. Deciding from that alone gives up on the very
// first unresolvable pass whenever the operator restarts under a terminating CR
// — or whenever the provider drops a cluster to engage a rotated kubeconfig,
// which it does by removing it and building it again — and that is exactly the
// race AbandonAfter exists to prevent.
//
// Losing the map on restart is the semantics, not a gap: what it holds is
// per-process by definition, so a fresh process owes every cluster a fresh
// window. It is keyed by cluster name rather than by CR so it stays bounded by
// the number of clusters an operator has ever named, and an entry is dropped
// as soon as its cluster resolves again.
var (
	unresolvableMu    sync.Mutex
	unresolvableSince = map[string]time.Time{}
)

// firstUnresolvable returns when this process first failed to resolve name,
// recording now on the first call.
func firstUnresolvable(name string) time.Time {
	unresolvableMu.Lock()
	defer unresolvableMu.Unlock()

	since, seen := unresolvableSince[name]
	if !seen {
		since = time.Now()
		unresolvableSince[name] = since
	}
	return since
}

// forgetUnresolvable drops name's record, so a cluster that resolves again
// starts a full window over if it later stops resolving.
func forgetUnresolvable(name string) {
	unresolvableMu.Lock()
	defer unresolvableMu.Unlock()

	delete(unresolvableSince, name)
}

// ResolveChildrenClientForDeletion resolves the children client of a CR that is
// already terminating. Unlike ResolveChildrenClient it never fails the pass. It
// answers one of three ways:
//
//   - (client, false): the target resolved; clean up through it.
//   - (nil, true): the target does not resolve yet. The caller requeues and
//     tries again, so the CR keeps its finalizers.
//   - (nil, false): the target has not resolved for AbandonAfter. The caller
//     finishes the cleanup without a client, releasing the finalizers and
//     leaving the children where they are.
//
// Two windows have to elapse before that last answer is given: AbandonAfter
// since this process first failed to resolve the cluster, and AbandonAfter
// since the CR was marked for deletion. Either alone abandons too eagerly —
// the first would give up on a CR deleted seconds ago on a cluster that has
// been unreachable for hours, the second on the first pass after an operator
// restart under a CR that has been terminating for longer than the window.
//
// A CR that carries finalizers has to reach its cleanup path even when the
// cluster it named was deregistered under it — a documented operation. Failing
// the pass instead would short-circuit every reconcile before the cleanup runs,
// the finalizers would never be released, and the CR (with its namespace) would
// stay Terminating until someone strips them by hand. Answering "gone" on the
// first unresolvable pass is as wrong in the other direction, because from here
// "not engaged yet" and "deregistered" are the same observable state: a CR that
// loses the race between the provider's cache sync and its own reconcile would
// leave etcd while its children keep running on a cluster that is perfectly
// reachable, with no CR left to retry from. AbandonAfter separates the two.
//
// The children on a cluster that stays unresolvable are unreachable either way
// and are left where they are. Abandoning the cluster is therefore also
// abandoning them: they keep running on it, and once the finalizers are
// released no CR remains to reclaim them if it ever comes back.
func ResolveChildrenClientForDeletion(
	ctx context.Context,
	resolver ClusterResolver,
	local client.Client,
	ref *commonv1.TargetClusterRefSpec,
	deletedAt metav1.Time,
) (client.Client, bool) {
	children, err := ResolveChildrenClient(ctx, resolver, local, ref)
	if err == nil {
		if ref != nil {
			forgetUnresolvable(ref.Name)
		}
		return children, false
	}

	// Only a named ref can fail to resolve, so ref is non-nil here.
	if time.Since(firstUnresolvable(ref.Name)) < AbandonAfter ||
		time.Since(deletedAt.Time) < AbandonAfter {
		log.FromContext(ctx).Info(
			"target cluster does not resolve yet while the CR is being deleted; "+
				"waiting for it to be engaged",
			"targetCluster", ref.Name, "error", err.Error(), "abandonAfter", AbandonAfter)
		return nil, true
	}

	log.FromContext(ctx).Info(
		"target cluster did not resolve within the abandon window; "+
			"releasing the finalizers and leaving its children behind",
		"targetCluster", ref.Name, "error", err.Error(), "abandonAfter", AbandonAfter)
	return nil, false
}

// ResolveChildrenAPIReader returns the uncached reader of the very cluster
// ResolveChildrenClient hands out the client for. A caller that must not decide
// from a cache which may still predate its own last write — the API Service
// selector latch is the one such caller — reads through it instead of through
// the children client, whose cache is as capable of lagging on a target cluster
// as the local one is on the management cluster.
//
// local is returned unchanged when there is no resolver or no ref, and may
// itself be nil: a reconciler constructed without a manager (unit tests) has no
// API reader. Callers substitute their children client in that case, which for
// a fake client is read-your-writes anyway.
func ResolveChildrenAPIReader(
	ctx context.Context,
	resolver ClusterResolver,
	local client.Reader,
	ref *commonv1.TargetClusterRefSpec,
) (client.Reader, error) {
	if resolver == nil || ref == nil {
		return local, nil
	}

	cl, err := resolver.GetCluster(ctx, mcruntime.ClusterName(ref.Name))
	if err != nil {
		return nil, err
	}

	return cl.GetAPIReader(), nil
}
