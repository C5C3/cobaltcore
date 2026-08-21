// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package multicluster

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

// teardownPageSize bounds one LIST the sweep issues. The sweep cannot narrow its
// lists with a server-side selector — ownership is decided object by object, for
// the two reasons DeleteRemoteChildren documents — so the API server answers
// with every object of the kind in the namespace, and a target namespace is
// shared with everything else deployed into it. Unpaged, a namespace holding
// tens of thousands of objects would arrive as a single response, per kind,
// which is enough to have the operator OOMKilled mid-sweep — and it would
// restart into the same sweep, so the CR would never leave Terminating. Paging
// caps the peak at one page per kind, at the cost of a round trip per page.
//
// The page size is only half of that bound, and the smaller half. What it
// multiplies is what one item costs, and that is why the sweep lists metadata
// alone (see DeleteRemoteChildren): a Secret or a ConfigMap is capped at about a
// megabyte of payload the sweep never reads, so a page of whole objects could
// exceed the operator's entire memory limit several times over at any page size
// worth issuing.
const teardownPageSize = 500

// SweepRemoteChildren is the teardown pass every service operator runs from its
// deletion path: it deletes what a CR projected onto the target cluster it names
// and then releases RemoteChildrenFinalizer, the finalizer that held the CR in
// etcd long enough to do it.
//
// It is a no-op for a CR that does not carry the finalizer, which is every CR
// that keeps its children on the management cluster: those are collected from
// their owner references, and only a CR naming a target cluster ever carries it.
// Callers therefore run it unconditionally from their cleanup path.
//
// children is what ResolveChildrenClientForDeletion returned. A nil client means
// the target cluster was deregistered and has stayed unresolvable past
// AbandonAfter. Its children cannot be reached, so they are left running, a
// Warning event records that they were, and the finalizer is released anyway:
// holding it would only strand the CR in Terminating.
//
// The sweep reads through the target cluster's uncached API reader rather than
// through the children client's cache. Its success licenses the finalizer
// release, and a child the cache has not caught up on would be missed, leaving
// no CR behind to delete it.
func SweepRemoteChildren(
	ctx context.Context,
	mgmt client.Client,
	resolver ClusterResolver,
	recorder record.EventRecorder,
	scheme *runtime.Scheme,
	owner client.Object,
	ref *commonv1.TargetClusterRefSpec,
	children client.Client,
	kinds []schema.GroupVersionKind,
) error {
	if !controllerutil.ContainsFinalizer(owner, RemoteChildrenFinalizer) {
		return nil
	}

	if children == nil {
		gvk, err := ownerGVK(scheme, owner)
		if err != nil {
			return err
		}
		recorder.Event(owner, corev1.EventTypeWarning, "RemoteChildrenAbandoned",
			fmt.Sprintf("Target cluster is no longer registered; releasing the remote-children finalizer without "+
				"deleting the objects on it labelled as owned by this %s", gvk.Kind))
	} else {
		reader, err := ResolveChildrenAPIReader(ctx, resolver, children, ref)
		if err != nil {
			return err
		}
		if err := DeleteRemoteChildren(ctx, reader, children, scheme, owner, owner.GetNamespace(), kinds); err != nil {
			return err
		}
	}

	controllerutil.RemoveFinalizer(owner, RemoteChildrenFinalizer)
	if err := mgmt.Update(ctx, owner); err != nil {
		return fmt.Errorf("removing remote-children finalizer: %w", err)
	}
	return nil
}

// DeleteRemoteChildren deletes every object of the given kinds that owner owns
// in namespace. It is the cleanup half of the ownership contract: no garbage
// collection cascade crosses a cluster boundary, so a CR that projected children
// onto a target cluster deletes them itself, and RemoteChildrenFinalizer is what
// keeps the CR in etcd long enough to do it.
//
// namespace is the one namespace the sweep lists in, and naming it is the
// caller's job because a projection is namespace-scoped but not always into the
// owner's own namespace. An owner whose children all live beside it passes
// owner.GetNamespace(); an owner that projects into a namespace it does not
// itself live in passes that namespace instead.
//
// Ownership is decided by Controls, object by object, rather than by a
// server-side label selector, so the sweep reclaims exactly the set every other
// write site recognizes as owner's — which also means the API server returns
// every object of the kind in the namespace, and the sweep pages through them
// (see teardownPageSize) instead of asking for a shared namespace in one
// response. Two kinds of child are only reachable this way:
//
//   - One written before ownership moved off the owner reference. It carries a
//     dangling controller reference to owner and no ownership labels, and every
//     create-once write site (the immutable config objects, the derived
//     db-connection Secret, the fernet and credential keys, the Certificates,
//     the Jobs) leaves it exactly as it found it. A selector would not return it,
//     the sweep would report success, and key material would keep running on the
//     target with no CR left to reclaim it.
//   - One belonging to a CR whose name exceeds MaxOwnerNameLength. Such a name
//     is not a valid label value, so a selector built from it is rejected with a
//     400 that is neither NotFound nor a no-match — the sweep would fail every
//     pass and strand the CR in Terminating on a finalizer nothing could release.
//
// A local owner is a no-op. writer is unmarked, nil comes back before anything
// is listed, and the children are left to the owner references that collect
// them. Callers therefore call this unconditionally from their cleanup path
// instead of branching on which cluster they wrote to.
//
// The pages are metadata-only (PartialObjectMetadataList). Both things the sweep
// does with an item — deciding ownership from its labels and owner references,
// and naming it in a delete — live in its metadata, while its payload does not:
// the swept kinds include Secret and ConfigMap, each capped at about a megabyte,
// so a page of whole objects would put tens of megabytes of data the sweep never
// reads into an operator whose memory limit is a fraction of that, and OOMKill it
// into the same sweep it just restarted from.
//
// reader must be the target cluster's uncached API reader, the one
// ResolveChildrenAPIReader returns. A remote informer cache lags as readily as a
// local one, and a child it has not caught up on would be missed by a sweep
// whose success licenses the finalizer release: the CR would leave etcd while
// that child keeps running with nothing left to delete it.
//
// A kind the target cluster does not serve is skipped (meta.IsNoMatchError):
// without cert-manager there is no Certificate kind, without Gateway API no
// HTTPRoute, and a kind that is not registered can have no children. Every other
// list error fails the pass, so the caller keeps its finalizer and sweeps again
// rather than releasing a CR whose children it never got to see.
//
// It does not wait for the deleted objects to leave etcd. A child holding a
// finalizer of its own (the MariaDB CRs are the slow ones) keeps terminating on
// the target afterwards, exactly as it does under the local cascade, where the
// owner is collected while its children are still draining.
func DeleteRemoteChildren(
	ctx context.Context,
	reader client.Reader,
	writer client.Client,
	scheme *runtime.Scheme,
	owner client.Object,
	namespace string,
	kinds []schema.GroupVersionKind,
) error {
	if !IsRemote(writer) {
		return nil
	}

	labels, err := OwnerLabels(scheme, owner)
	if err != nil {
		return err
	}

	for _, gvk := range kinds {
		listGVK := gvk.GroupVersion().WithKind(gvk.Kind + "List")

		for continueToken, rescanned := "", false; ; {
			page := &metav1.PartialObjectMetadataList{}
			page.SetGroupVersionKind(listGVK)

			err := reader.List(ctx, page, client.InNamespace(namespace),
				client.Limit(teardownPageSize), client.Continue(continueToken))
			if meta.IsNoMatchError(err) {
				break
			}
			if apierrors.IsResourceExpired(err) && !rescanned {
				// A paged list is served from the etcd revision the first page was
				// read at, and a sweep that deletes its way through a large kind can
				// outlive that revision's compaction window. The continue token is
				// then refused for good, so the kind is scanned once more from the
				// start rather than failing a pass that would only restart here
				// anyway: deleting is idempotent, and what is left to scan is
				// smaller than it was. A second expiry does fail the pass, so a
				// target cluster that expires every scan requeues the CR instead of
				// pinning a reconcile worker on an unbounded retry.
				continueToken, rescanned = "", true
				continue
			}
			if err != nil {
				return fmt.Errorf("listing remote %s children for teardown: %w", gvk.Kind, err)
			}

			for i := range page.Items {
				child := &page.Items[i]
				if !controls(owner, child, labels) {
					continue
				}
				// A metadata list leaves its items without a kind, and the delete is
				// routed by theirs, not by the list's.
				child.SetGroupVersionKind(gvk)
				err := writer.Delete(ctx, child, client.PropagationPolicy(metav1.DeletePropagationBackground))
				if err != nil && !apierrors.IsNotFound(err) {
					return fmt.Errorf("deleting remote %s %s/%s: %w",
						gvk.Kind, child.GetNamespace(), child.GetName(), err)
				}
			}

			continueToken = page.GetContinue()
			if continueToken == "" {
				break
			}
		}
	}

	return nil
}
