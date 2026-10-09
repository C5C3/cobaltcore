// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	mchandler "sigs.k8s.io/multicluster-runtime/pkg/handler"
	mcruntime "sigs.k8s.io/multicluster-runtime/pkg/multicluster"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	"github.com/c5c3/cobaltcore/internal/common/apply"
	"github.com/c5c3/cobaltcore/internal/common/conditions"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// This file is the scaffold every order kind shares. An order lives in a
// namespace a ControlPlane assigns to a service owner, on the cluster that
// namespace is assigned on; its children live in the ControlPlane's namespace on
// the management cluster and carry three ownership labels instead of an owner
// reference. The scaffold covers what does not depend on the kind: the naming
// and ownership of the children, the consent gates, the pass result, the
// teardown and the watch mapping.

// reasonOrderReferencedByRoleAssignments reports a KeystoneUser or
// KeystoneProject whose deletion holds while a KeystoneRoleAssignment from the
// same namespace still references it.
const reasonOrderReferencedByRoleAssignments = "ReferencedByRoleAssignments"

// orderLabelKeys are the three ownership label keys of one order kind.
type orderLabelKeys struct{ Name, Namespace, Cluster string }

// orderRef identifies one order on its cluster and the prefix segment of its
// children. The cluster is c5c3v1alpha1.ManagementCluster for an order on the
// management cluster.
type orderRef struct {
	Name, Namespace, Cluster string
	// Segment names the kind in the children's prefix: "user", "project",
	// "roleassignment" or "catalogentry".
	Segment string
	Keys    orderLabelKeys
}

// childPrefix scopes every child an order creates in the ControlPlane's
// namespace, and every sweep that removes one. The hash covers the cluster, the
// namespace and the name, because the same namespace and name on two clusters
// are two orders whose children share that one namespace. The readable base
// stays in front so a listing reads as the order it belongs to.
func (o orderRef) childPrefix() string {
	sum := sha256.Sum256([]byte(o.Cluster + "/" + o.Namespace + "/" + o.Name))
	return o.Name + "-" + hex.EncodeToString(sum[:])[:8] + "-" + o.Segment + "-"
}

// childLabels returns the three ownership labels of the order's children.
func (o orderRef) childLabels() map[string]string {
	return map[string]string{
		o.Keys.Name:      o.Name,
		o.Keys.Namespace: o.Namespace,
		o.Keys.Cluster:   o.Cluster,
	}
}

// clusterRef returns the target-cluster ref of the order's cluster, or nil for
// the management cluster.
func (o orderRef) clusterRef() *commonv1.TargetClusterRefSpec {
	if o.Cluster == c5c3v1alpha1.ManagementCluster {
		return nil
	}
	return &commonv1.TargetClusterRefSpec{Name: o.Cluster}
}

// location names the cluster the order lives on in the phrase the
// namespace-assignment messages share.
func (o orderRef) location() string {
	return (&c5c3v1alpha1.NamespaceAssignmentSpec{TargetClusterRef: o.clusterRef()}).Location()
}

// --- ownership ---

// isOrderChild reports whether owner owns obj: obj carries all three of the
// order's labels (the cluster label present, also when it is empty), or owner is
// obj's controller owner reference (a Secret delivered beside the order).
func isOrderChild(obj, owner client.Object, o orderRef) bool {
	if metav1.IsControlledBy(obj, owner) {
		return true
	}
	labels := obj.GetLabels()
	objCluster, ok := labels[o.Keys.Cluster]
	return ok && objCluster == o.Cluster &&
		labels[o.Keys.Name] == o.Name &&
		labels[o.Keys.Namespace] == o.Namespace
}

// ownsOrderChild reports whether obj is a child the order created in the
// ControlPlane's namespace. The ownership test and the prefix test must both
// pass, so an object of the plane, of a KeystoneService or of another order that
// shares the namespace is never reshaped or swept.
func ownsOrderChild(obj, owner client.Object, o orderRef) bool {
	return isOrderChild(obj, owner, o) && strings.HasPrefix(obj.GetName(), o.childPrefix())
}

// claimOrderChild sets the order's labels on obj, keeping any label already
// there. It never sets an owner reference: every labelled child lives in another
// namespace or on another cluster than the order.
func claimOrderChild(obj client.Object, o orderRef) {
	labels := obj.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	for k, v := range o.childLabels() {
		labels[k] = v
	}
	obj.SetLabels(labels)
}

// ensureOrderChild applies a child in the ControlPlane's namespace with
// Server-Side Apply. A live object of that name the order did not create is
// refused: the apply would overwrite its spec and the teardown would delete it.
func ensureOrderChild(
	ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, o orderRef, obj client.Object,
) error {
	live := obj.DeepCopyObject().(client.Object)
	switch err := c.Get(ctx, client.ObjectKeyFromObject(obj), live); {
	case apierrors.IsNotFound(err) || meta.IsNoMatchError(err):
	case err != nil:
		return fmt.Errorf("checking for a pre-existing %T %s before adopting it: %w",
			obj, client.ObjectKeyFromObject(obj), err)
	default:
		if !isOrderChild(live, owner, o) {
			return fmt.Errorf("refusing to adopt pre-existing %T %s: it was not created by this order",
				obj, client.ObjectKeyFromObject(obj))
		}
	}
	claimOrderChild(obj, o)
	return apply.EnsureUnownedObject(ctx, c, scheme, obj, apply.FieldManager)
}

// orderEnsure returns the ensure seam the shared projection helpers take, bound
// to the order: every child they build is written through ensureOrderChild.
func orderEnsure(c client.Client, scheme *runtime.Scheme, owner client.Object, o orderRef) registrationEnsure {
	return func(ctx context.Context, obj client.Object) error {
		return ensureOrderChild(ctx, c, scheme, owner, o, obj)
	}
}

// --- gates ---

// The reasons of the admission gates every order kind writes.
const (
	// reasonOrderNamespaceNotAssigned reports that the ControlPlane has no
	// spec.namespaceAssignments entry for the order's namespace and cluster. The
	// order is frozen while it holds.
	reasonOrderNamespaceNotAssigned = "NamespaceNotAssigned"
	// reasonOrderClusterNameTooLong reports an order on a target cluster whose
	// name is too long to be carried as a label value on the children.
	reasonOrderClusterNameTooLong = "ClusterNameTooLong"
)

// orderClient returns the client of the cluster an order lives on: mgmt for
// c5c3v1alpha1.ManagementCluster, and the resolved target cluster's client
// otherwise.
func orderClient(
	ctx context.Context, resolver commonmulticluster.ClusterResolver, mgmt client.Client, cluster string,
) (client.Client, error) {
	return commonmulticluster.ResolveChildrenClient(ctx, resolver, mgmt, orderRef{Cluster: cluster}.clusterRef())
}

// orderControlPlaneKey resolves an order's controlPlaneRef, the namespace
// defaulting to the order's own.
func orderControlPlaneKey(ref c5c3v1alpha1.ControlPlaneRefSpec, namespace string) client.ObjectKey {
	return client.ObjectKey{Namespace: cmp.Or(ref.Namespace, namespace), Name: ref.Name}
}

// orderNotAssignedMessage is the NamespaceNotAssigned message for the
// ControlPlane cpKey names.
func orderNotAssignedMessage(cpKey client.ObjectKey, namespace, cluster string) string {
	return fmt.Sprintf("ControlPlane %s assigns no namespace %q on %s (spec.namespaceAssignments); "+
		"the order is frozen: nothing is provisioned, delivered or repaired, and what was created stays",
		cpKey, namespace, orderRef{Cluster: cluster}.location())
}

// orderAdmission runs the gates every order kind passes before it acts: the
// cluster name, the ControlPlane read and the consent lookup. It returns the
// ControlPlane and the order's spec.namespaceAssignments entry once the order is
// admitted. A nil ControlPlane with a nil error means fail wrote the refusal
// and the returned result is the pass's.
//
// The freeze of #1327 D2 is the consent lookup: without an entry the pass
// returns before anything is read or written in the ControlPlane's namespace.
//
// For an order on a target cluster a missing ControlPlane writes the
// NamespaceNotAssigned a plane without the assignment writes, with the same
// message: the owner there cannot read the management cluster, and the reason
// must not tell them which ControlPlanes exist on it.
func orderAdmission(
	ctx context.Context, mgmt client.Client, o orderRef, ref c5c3v1alpha1.ControlPlaneRefSpec,
	fail func(reason, message string),
) (*c5c3v1alpha1.ControlPlane, *c5c3v1alpha1.NamespaceAssignmentSpec, ctrl.Result, error) {
	// The cluster name is a label value on every child, which Kubernetes caps at
	// 63 characters, while a registration Secret's name may run to 253. Refusing
	// here keeps every child write from failing in the API server. The name is the
	// cluster's identity, so no edit of the order lifts the refusal.
	if len(o.Cluster) > validation.LabelValueMaxLength {
		fail(reasonOrderClusterNameTooLong, fmt.Sprintf(
			"the order lives on %s, whose name exceeds %d characters and cannot be carried as a label value; "+
				"nothing is provisioned for an order on that cluster", o.location(), validation.LabelValueMaxLength))
		return nil, nil, ctrl.Result{}, nil
	}

	key := orderControlPlaneKey(ref, o.Namespace)
	var cp c5c3v1alpha1.ControlPlane
	if err := mgmt.Get(ctx, key, &cp); err != nil {
		if apierrors.IsNotFound(err) && o.Cluster != c5c3v1alpha1.ManagementCluster {
			fail(reasonOrderNamespaceNotAssigned, orderNotAssignedMessage(key, o.Namespace, o.Cluster))
			return nil, nil, ctrl.Result{RequeueAfter: namespaceAssignmentRequeueAfter}, nil
		}
		if apierrors.IsNotFound(err) {
			fail(reasonKeystoneServiceControlPlaneNotFound, fmt.Sprintf(
				"ControlPlane %s not found; the order is deferred until it exists", key))
			return nil, nil, ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
		}
		return nil, nil, ctrl.Result{}, fmt.Errorf("fetching ControlPlane %s/%s: %w", key.Namespace, key.Name, err)
	}

	entry := cp.NamespaceAssignmentFor(o.Namespace, o.Cluster)
	if entry == nil {
		fail(reasonOrderNamespaceNotAssigned, orderNotAssignedMessage(key, o.Namespace, o.Cluster))
		return nil, nil, ctrl.Result{RequeueAfter: namespaceAssignmentRequeueAfter}, nil
	}
	return &cp, entry, ctrl.Result{}, nil
}

// orderAdminCredentialGate reports whether the ControlPlane's admin credential
// is ready. K-ORC cannot talk to Keystone before it exists, so a false return
// has written WaitingForAdminCredential through fail and the pass requeues
// after korcRequeueAfter.
func orderAdminCredentialGate(cp *c5c3v1alpha1.ControlPlane, fail func(reason, message string)) bool {
	if conditions.AllTrue(cp.Status.Conditions, conditionTypeAdminCredentialReady) {
		return true
	}
	fail(reasonWaitingForServiceAccountAdmin, fmt.Sprintf(
		"ControlPlane %s/%s reports AdminCredentialReady is not True; the order is deferred",
		cp.Namespace, cp.Name))
	return false
}

// orderPassResult decides when the next pass runs once the kind's legs had
// their say. A requeue a leg asked for stands. A converged order on the
// management cluster waits for an event: the ControlPlane watch reaches it.
// Every other order comes back on orderRefreshAfter, because no watch
// reaches it: a converged order on a target cluster, and a refusal that only an
// edit outside the order's own watches lifts.
func orderPassResult(cluster string, converged bool, result ctrl.Result) ctrl.Result {
	if !result.IsZero() {
		return result
	}
	if cluster == c5c3v1alpha1.ManagementCluster && converged {
		return ctrl.Result{}
	}
	return ctrl.Result{RequeueAfter: orderRefreshAfter}
}

// --- teardown ---

// orderKind names the kind of owner in the teardown's log lines and errors.
func orderKind(owner client.Object) string {
	t := fmt.Sprintf("%T", owner)
	return t[strings.LastIndex(t, ".")+1:]
}

// orderTeardown removes everything an order created and releases its finalizer.
// The assignment is not consulted: a withdrawn assignment freezes an order, and
// deleting a frozen order still tears it down.
//
// sweep issues the deletes of the order's children in childNS and reports how
// many are still listed. With the ControlPlane present the teardown is patient:
// K-ORC's finalizers take the resources out of Keystone, so the finalizer is
// held until sweep reports none. With the ControlPlane gone it fails open:
// K-ORC has no credential left to reach Keystone with, so the children are
// deleted and the finalizer is released whatever their outcome.
func orderTeardown(
	ctx context.Context, mgmt, oc client.Client, owner client.Object, finalizer string, cpKey client.ObjectKey,
	sweep func(ctx context.Context, childNS string) (int, error),
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(owner, finalizer) {
		return ctrl.Result{}, nil
	}
	logger := log.FromContext(ctx)
	kind := orderKind(owner)

	controlPlaneGone := false
	if err := mgmt.Get(ctx, cpKey, &c5c3v1alpha1.ControlPlane{}); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("fetching ControlPlane %s during teardown: %w", cpKey, err)
		}
		controlPlaneGone = true
	}

	// cpKey.Namespace is where the children are, whether or not the plane is
	// still there to be read.
	remaining, err := sweep(ctx, cpKey.Namespace)
	if err != nil {
		if !controlPlaneGone {
			return ctrl.Result{}, err
		}
		logger.Error(err, "best-effort teardown of "+kind+" children failed; releasing the finalizer anyway",
			"controlPlane", cpKey)
	} else {
		if !controlPlaneGone && remaining > 0 {
			logger.V(1).Info("waiting for "+kind+" children to be removed before releasing the finalizer",
				"remaining", remaining)
			return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
		}
		if controlPlaneGone {
			logger.Info("referenced ControlPlane is gone; releasing the "+kind+" finalizer", "controlPlane", cpKey)
		}
	}

	controllerutil.RemoveFinalizer(owner, finalizer)
	if err := oc.Update(ctx, owner); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing %s finalizer: %w", kind, err)
	}
	return ctrl.Result{}, nil
}

// sweepOrderList lists list in childNS by the order's labels and deletes every
// item the order owns, the items first selects before the rest, and returns the
// number of deletes issued. A child is counted on the pass that issued its
// delete: K-ORC holds its objects behind finalizers while it clears Keystone,
// and the teardown waits for that. A nil first keeps the listed order.
func sweepOrderList(
	ctx context.Context, mgmt client.Client, owner client.Object, o orderRef, childNS string,
	list client.ObjectList, first func(client.Object) bool,
) (int, error) {
	if err := mgmt.List(ctx, list, client.InNamespace(childNS), client.MatchingLabels(o.childLabels())); err != nil {
		return 0, fmt.Errorf("listing order %T: %w", list, err)
	}
	items, err := meta.ExtractList(list)
	if err != nil {
		return 0, fmt.Errorf("reading order %T: %w", list, err)
	}
	logger := log.FromContext(ctx)
	kind := orderKind(owner)

	deleted := 0
	for _, firstPass := range []bool{true, false} {
		for _, item := range items {
			obj, ok := item.(client.Object)
			if !ok || !ownsOrderChild(obj, owner, o) || (first != nil && first(obj)) != firstPass {
				continue
			}
			logger.Info("removing a "+kind+" child", "name", obj.GetName(), "namespace", obj.GetNamespace())
			if err := client.IgnoreNotFound(mgmt.Delete(ctx, obj)); err != nil {
				return 0, fmt.Errorf("deleting %s child %q: %w", kind, obj.GetName(), err)
			}
			deleted++
		}
	}
	return deleted, nil
}

// orderSweep is one list sweepOrderLists sweeps, with the selector of the items
// deleted first.
type orderSweep struct {
	list  client.ObjectList
	first func(client.Object) bool
}

// sweepOrderLists runs sweepOrderList over each list in turn and returns the
// deletes issued across all of them. The first error stops the sweep.
func sweepOrderLists(
	ctx context.Context, mgmt client.Client, owner client.Object, o orderRef, childNS string, sweeps ...orderSweep,
) (int, error) {
	deleted := 0
	for _, sw := range sweeps {
		n, err := sweepOrderList(ctx, mgmt, owner, o, childNS, sw.list, sw.first)
		if err != nil {
			return 0, err
		}
		deleted += n
	}
	return deleted, nil
}

// orderReferencedMessage is the ReferencedByRoleAssignments message of an order
// whose deletion holds while the KeystoneRoleAssignments names lists still
// reference it. noun is "user" or "project".
func orderReferencedMessage(names []string, namespace, noun string) string {
	return fmt.Sprintf("KeystoneRoleAssignment(s) %q in namespace %q still reference this %s; delete them first, "+
		"because deleting the %s would take their assignments with it", names, namespace, noun, noun)
}

// --- watches ---

// orderControlPlanePredicate passes the ControlPlane updates an order reads: a
// spec change, which moves the generation, a deletion, and a flip of
// AdminCredentialReady, the one status condition the gates consult. The plane's
// other status writes, of which a rollout makes many, would wake every order on
// the management cluster for nothing.
func orderControlPlanePredicate() predicate.Funcs {
	adminCredentialReady := func(obj client.Object) bool {
		cp, ok := obj.(*c5c3v1alpha1.ControlPlane)
		return ok && conditions.AllTrue(cp.Status.Conditions, conditionTypeAdminCredentialReady)
	}
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			return e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration() ||
				!e.ObjectOld.GetDeletionTimestamp().Equal(e.ObjectNew.GetDeletionTimestamp()) ||
				adminCredentialReady(e.ObjectOld) != adminCredentialReady(e.ObjectNew)
		},
	}
}

// orderChildToRequests maps a child in the ControlPlane's namespace back to its
// order by the three labels keys names. The cluster label becomes the request's
// cluster, so the event reaches the order on the cluster it lives on. An object
// without the name and namespace labels belongs to something else and maps to
// nothing.
func orderChildToRequests(keys orderLabelKeys) func(context.Context, client.Object) []mcreconcile.Request {
	return func(_ context.Context, obj client.Object) []mcreconcile.Request {
		labels := obj.GetLabels()
		name, namespace := labels[keys.Name], labels[keys.Namespace]
		if name == "" || namespace == "" {
			return nil
		}
		return []mcreconcile.Request{{
			Request:     reconcile.Request{NamespacedName: types.NamespacedName{Namespace: namespace, Name: name}},
			ClusterName: mcruntime.ClusterName(labels[keys.Cluster]),
		}}
	}
}

// orderChildRequests is the event-handler factory of the child legs. It keeps
// the cluster orderChildToRequests chose, where LocalRequests would pin every
// request to the management cluster.
func orderChildRequests(keys orderLabelKeys) mchandler.TypedEventHandlerFunc[client.Object, mcreconcile.Request] {
	return func(_ mcruntime.ClusterName, _ cluster.Cluster) handler.TypedEventHandler[client.Object, mcreconcile.Request] {
		return mchandler.TypedEnqueueRequestsFromMapFuncWithClusterPreservation(orderChildToRequests(keys))
	}
}
