// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	mchandler "sigs.k8s.io/multicluster-runtime/pkg/handler"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	"github.com/c5c3/forge/internal/common/apply"
	"github.com/c5c3/forge/internal/common/conditions"
	commonmulticluster "github.com/c5c3/forge/internal/common/multicluster"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
)

// The ownership labels are the cross-namespace substitute for a controller owner
// reference. Kubernetes forbids a cross-namespace owner reference (garbage
// collection only cascades within one namespace), so a child the ControlPlane
// places in a service namespace carries no owner reference at all: it is stamped
// with these two labels instead, which name the owning ControlPlane
// unambiguously (a ControlPlane name alone is not unique across namespaces).
//
// They carry the two jobs an owner reference would have done, and the reconciler
// does both by hand because nothing does them for it:
//
//   - RECOGNITION. isControlPlaneChild answers "may this ControlPlane write to,
//     and delete, this object?" — for a same-namespace child from the owner
//     reference, for a cross-namespace one from these labels. A colliding object
//     carrying neither is adopted by nobody and left alone.
//   - CLEANUP. No GC cascade reaches a cross-namespace child, so the ORC-teardown
//     finalizer deletes them explicitly (teardownDedicatedNamespaces).
//
// crossNamespaceChildMapper resolves them back to a reconcile request, so a
// status transition on such a child still wakes its ControlPlane.
const (
	controlPlaneNameLabel      = "c5c3.io/controlplane-name"
	controlPlaneNamespaceLabel = "c5c3.io/controlplane-namespace"
	// managedByLabel is the standard recommended label the operator stamps on a
	// namespace it creates. It is informational only: neither adoption in
	// reconcileNamespaces nor deletion in deleteManagedNamespace consults it —
	// both gate solely on the two ownership labels via isControlPlaneChild. It
	// records, for humans and external tooling, that the operator owns the
	// namespace.
	managedByLabel = "app.kubernetes.io/managed-by"
	// managedByValue is the operator identity stamped into managedByLabel.
	managedByValue = "c5c3-operator"
	// controlPlaneUIDAnnotation records the UID of the ControlPlane a MANAGED
	// namespace was created for. On the local cluster the two ownership labels
	// already identify that ControlPlane uniquely, because validateNamespaceClaims
	// admits one ControlPlane per namespace and an owner reference matches on UID.
	// Neither guarantee crosses a cluster boundary: a target cluster can be
	// registered by any number of management clusters, each of which can run a
	// ControlPlane of the same name in a namespace of the same name — the
	// quickstart defaults — so on a target the label pair alone would have two
	// operators adopt one namespace and either teardown cascade the other's
	// database away with it. The UID is generated per CR by the API server, so it
	// is the one mark that tells them apart — and the one part of the claim that
	// cannot be read off the target cluster, let alone forged there. It is stamped
	// at creation and required before a namespace on a TARGET cluster is adopted.
	controlPlaneUIDAnnotation = "c5c3.io/controlplane-uid"
)

// controlPlaneChildLabels returns the ownership labels identifying cp as the
// owner of a cross-namespace child.
func controlPlaneChildLabels(cp *c5c3v1alpha1.ControlPlane) map[string]string {
	return map[string]string{
		controlPlaneNameLabel:      cp.Name,
		controlPlaneNamespaceLabel: cp.Namespace,
	}
}

// stampControlPlaneChildLabels merges cp's ownership labels onto obj, preserving
// any labels already there. Called on every cross-namespace projection before the
// apply, so the child is recognizable the moment it exists.
func stampControlPlaneChildLabels(obj client.Object, cp *c5c3v1alpha1.ControlPlane) {
	labels := obj.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	for k, v := range controlPlaneChildLabels(cp) {
		labels[k] = v
	}
	obj.SetLabels(labels)
}

// claimChildOwnership makes obj a child of cp, by the only mechanism the client
// c writes obj with, and the namespace it lands in, permit: a controller owner
// reference when it shares cp's namespace on the local cluster (so the GC
// cascade reaps it), the ownership labels when it does not (so the
// finalizer-driven teardown can find and delete it). It is the single decision
// point, so no projection site has to re-derive which mechanism applies.
//
// On a target cluster no owner reference is usable at all — it would name a UID
// that cluster does not have — so the claim goes through the shared
// ClaimWithLabels, which adds the owner-kind/name/namespace labels every operator
// marks a remote child with. That call REBUILDS obj's label set from the map it
// is handed, so obj is stamped first and its own labels are passed back in:
// handing it less would drop what the projection site composed, managed-by among
// it. A remote child ends up with both marks, the owner triple the shared
// teardown selects on and the cross-namespace pair the watch legs map a child
// back to its ControlPlane by.
func claimChildOwnership(c client.Client, cp *c5c3v1alpha1.ControlPlane, obj client.Object, scheme *runtime.Scheme) error {
	if commonmulticluster.IsRemote(c) {
		stampControlPlaneChildLabels(obj, cp)
		return commonmulticluster.ClaimWithLabels(c, scheme, cp, obj, obj.GetLabels())
	}
	if obj.GetNamespace() != cp.Namespace {
		stampControlPlaneChildLabels(obj, cp)
		return nil
	}
	return controllerutil.SetControllerReference(cp, obj, scheme)
}

// ensureUnownedOrOwned is the Server-Side-Apply twin of claimChildOwnership: it
// applies obj under the shared field manager, owner-referenced when it shares cp's
// namespace and label-stamped-and-unowned when it does not. Every SSA projection
// that can land in a service namespace routes through it, so no call site has to
// re-derive which ownership mechanism its namespace permits.
//
// In a namespace the ControlPlane does not own (an External-lifecycle service
// namespace, or a name we lost the create race for) a same-named object may
// already belong to somebody else. Adopting it would be doubly destructive: the
// SSA apply overwrites its spec to point at our OpenBao, and the ownership labels
// we stamp make the teardown residue sweep DELETE it. So a pre-existing object we
// did not create is refused here, mirroring reconcileNamespaces' NamespaceNotOwned
// refusal for the namespace itself.
//
// The claim goes through claimChildOwnership rather than stamping the two
// cross-namespace labels here, so an object applied to a TARGET cluster carries
// the owner triple the shared teardown selects on as well. Stamping only what
// this operator's own watch legs read would leave a remote child unreachable for
// commonmulticluster.DeleteRemoteChildren, which selects on the owner labels
// alone.
func (r *ControlPlaneReconciler) ensureUnownedOrOwned(ctx context.Context, c client.Client, cp *c5c3v1alpha1.ControlPlane, obj client.Object) error {
	if obj.GetNamespace() == cp.Namespace {
		return apply.EnsureObject(ctx, c, r.Scheme, cp, obj, apply.FieldManager)
	}
	live := obj.DeepCopyObject().(client.Object)
	reader, cached := adoptionPrecheckReader(r, c, obj)
	err := reader.Get(ctx, client.ObjectKeyFromObject(obj), live)
	if cached && apierrors.IsNotFound(err) {
		// A cache MISS is not proof of absence: the informer trails the API server
		// by however long the watch takes to deliver the ADD, and everything below
		// this point is destructive if the name turns out to be taken — the SSA
		// apply overwrites the foreign object's spec, and the labels we stamp get
		// it deleted at teardown. Confirm against the API server before concluding
		// the name is free. It costs a round trip only while the object does not
		// exist yet; once it does, the cache answers and this never runs.
		live = obj.DeepCopyObject().(client.Object)
		err = r.apiReader().Get(ctx, client.ObjectKeyFromObject(obj), live)
	}
	switch {
	case apierrors.IsNotFound(err) || meta.IsNoMatchError(err):
		// Absent (or its CRD is not installed): safe to create.
	case err != nil:
		return fmt.Errorf("checking for a pre-existing %T %s before adopting it: %w",
			obj, client.ObjectKeyFromObject(obj), err)
	default:
		if !isControlPlaneChild(live, cp) {
			return fmt.Errorf("refusing to adopt pre-existing %T %s in unowned namespace %q: it was not created by "+
				"this ControlPlane, so adopting it would overwrite its spec and delete it at teardown",
				obj, client.ObjectKeyFromObject(obj), obj.GetNamespace())
		}
	}
	if err := claimChildOwnership(c, cp, obj, r.Scheme); err != nil {
		return err
	}
	return apply.EnsureUnownedObject(ctx, c, r.Scheme, obj, apply.FieldManager)
}

// adoptionPrecheckReader picks the reader ensureUnownedOrOwned's adoption
// pre-check reads obj through — the UNCACHED one (see
// ControlPlaneReconciler.APIReader) for the kinds the operator never watches, the
// cached client for every other kind — and reports whether that reader is the
// cached one, whose miss the caller has to confirm before trusting it.
//
// A write against a target cluster is decided from THAT cluster's own uncached
// reader (commonmulticluster.LiveReader), whatever the kind: the local API reader
// answers for the wrong cluster, and the kind routing above is about which local
// informers the operator is willing to start. Its miss needs no confirmation
// because it is already the live answer.
//
// For an unwatched kind this pre-check is the only read the kind ever gets, and a
// cached read would have controller-runtime start an unfiltered cluster-wide
// informer for it — every Role, RoleBinding and ServiceAccount in the cluster held
// in memory to track a handful of objects per ControlPlane. For a WATCHED kind the
// informer is already running and its cache is already populated, so a direct GET
// on the HIT buys nothing and costs a round trip against the reconciler's shared
// client-side rate limit — on the dedicated-service-namespace layout, one per
// cross-namespace projection on every pass. A cached MISS is the one answer the
// informer can get wrong, which is what the second return value is for.
func adoptionPrecheckReader(r *ControlPlaneReconciler, c client.Client, obj client.Object) (client.Reader, bool) {
	if commonmulticluster.IsRemote(c) {
		return commonmulticluster.LiveReader(c), false
	}
	switch obj.(type) {
	case *rbacv1.Role, *rbacv1.RoleBinding, *rbacv1.ClusterRoleBinding, *corev1.ServiceAccount:
		return r.apiReader(), false
	}
	return r.Client, true
}

// clustersFor returns the clusters an object in a namespace has to be written to
// or swept on: the management cluster always, and the target cluster on top of it
// when children resolved to one. ResolveChildrenClient hands the local client
// itself back for an unplaced namespace and marks a resolved one as remote, so
// that mark is what keeps an unplaced namespace visited exactly once.
//
// A placed namespace exists on both clusters and an unplaced one only at home —
// one invariant, which the create path and the two teardown paths all act on. Left
// open-coded at each of them they could disagree, and a namespace created on two
// clusters but reaped on one leaves the other standing with nothing to reclaim it.
func (r *ControlPlaneReconciler) clustersFor(children client.Client) []client.Client {
	if commonmulticluster.IsRemote(children) {
		return []client.Client{r.Client, children}
	}
	return []client.Client{r.Client}
}

// refuseForeignAdoption is the CreateOrUpdate-mutate twin of ensureUnownedOrOwned's
// pre-apply guard, for the projections that stay read-modify-write: the two
// cert-manager Certificates (no Go type ships for them) and the owned Secrets
// whose mutate reads live Data. live is the object CreateOrUpdate has just
// Get-populated, so a set UID means it already exists. If it exists in a namespace
// the ControlPlane does not own and is not already our child, refuse: reshaping
// and later sweeping a same-named foreign object would clobber somebody else's
// resource, the same reason ensureUnownedOrOwned refuses foreign adoption.
//
// The kind is resolved from the SCHEME, not from live.GetObjectKind(): a TYPED
// object built in-code carries an empty TypeMeta and the typed client does not
// populate it on Get, so reading the GVK off the object would name the refused
// object as an empty kind — in the one message an operator has to go on. The
// unstructured Certificates carry their own GVK and resolve identically here.
//
// The exemption for cp's own namespace is a fact about the LOCAL cluster, so it
// is taken only when c writes to it. A namespace of that name on a target
// cluster is a different namespace on a different cluster, where the
// ControlPlane owns nothing by reference and only the ownership labels answer
// for a live object.
func refuseForeignAdoption(c client.Client, cp *c5c3v1alpha1.ControlPlane, live client.Object, scheme *runtime.Scheme) error {
	if !commonmulticluster.IsRemote(c) && live.GetNamespace() == cp.Namespace {
		return nil
	}
	if live.GetUID() == "" || isControlPlaneChild(live, cp) {
		return nil
	}
	kind := live.GetObjectKind().GroupVersionKind().Kind
	if gvk, err := apiutil.GVKForObject(live, scheme); err == nil {
		kind = gvk.Kind
	}
	return fmt.Errorf("refusing to adopt pre-existing %s %s/%s in unowned namespace %q: it was not created by this "+
		"ControlPlane, so adopting it would overwrite its spec and delete it at teardown",
		kind, live.GetNamespace(), live.GetName(), live.GetNamespace())
}

// isControlPlaneChild reports whether cp owns obj: either it is the controller
// owner reference (the same-namespace case), or obj carries cp's ownership labels
// (the cross-namespace case, where no owner reference is possible). It is the
// single ownership test every write and every delete gates on, so an
// externally-provisioned object sharing a name with one of our children is never
// reshaped and never deleted.
func isControlPlaneChild(obj client.Object, cp *c5c3v1alpha1.ControlPlane) bool {
	if metav1.IsControlledBy(obj, cp) {
		return true
	}
	labels := obj.GetLabels()
	return labels[controlPlaneNameLabel] == cp.Name && labels[controlPlaneNamespaceLabel] == cp.Namespace
}

// ownsManagedNamespace reports whether cp may ADOPT the Managed namespace ns on
// the cluster c reads and writes. It is isControlPlaneChild plus the one fact the
// ownership labels cannot carry across a cluster boundary: on a TARGET cluster the
// namespace has to carry cp's own mark (controlPlaneUIDAnnotation) as well.
//
// The extra proof is demanded only there. On the local cluster the labels are
// already unambiguous, and requiring an annotation would refuse every namespace
// created before it existed — on a target cluster there is no such namespace,
// because placement creates them all and stamps the mark as it goes.
//
// Adoption is where the mark has to be exact. Both labels are derived from the
// CR's name and namespace, values the CR publishes and the docs spell out, so
// anyone holding patch on a namespace of the target cluster can write them onto a
// namespace they do not own; the UID is minted by the management cluster's API
// server and is not readable from the target at all. Were an unmarked namespace
// accepted as ours, that one patch would hand the operator a foreign namespace to
// project into — and to cascade away, with every workload, PVC and Secret in it,
// at teardown. So a namespace whose mark is missing is refused here exactly as one
// naming somebody else is, and the operator restores the mark or picks a free
// name.
func ownsManagedNamespace(c client.Client, ns *corev1.Namespace, cp *c5c3v1alpha1.ControlPlane) bool {
	if !isControlPlaneChild(ns, cp) {
		return false
	}
	if !commonmulticluster.IsRemote(c) {
		return true
	}
	return ns.Annotations[controlPlaneUIDAnnotation] == string(cp.UID)
}

// unownedNamespaceMessage explains an ownsManagedNamespace refusal in the terms
// the operator has to act on, which differ per cause. A namespace carrying
// somebody else's UID mark WAS created by a ControlPlane of this name in a
// namespace of this name — on another management cluster, or as an earlier
// instance of this one whose namespace outlived it — so telling its owner that it
// "was not created by this ControlPlane" would be both false and unactionable.
// The remedy differs too: that namespace is handed over by removing it (or its
// mark) on the target cluster, whereas a namespace belonging to nobody here is
// left alone and the assignment moves.
//
// A namespace carrying our labels and NO mark is the third case, and it is only
// reachable on a target cluster: at home the labels are the whole verdict. Either
// something stripped the mark off a namespace this ControlPlane created, or
// somebody wrote the labels onto a namespace it did not — nothing left on the
// object tells those apart, which is why the mark is required and not inferred.
// Both remedies are the operator's to pick, so both are named.
func unownedNamespaceMessage(ns *corev1.Namespace, cp *c5c3v1alpha1.ControlPlane) string {
	if isControlPlaneChild(ns, cp) && ns.Annotations[controlPlaneUIDAnnotation] == "" {
		return fmt.Sprintf(
			"namespace %q on the target cluster carries this ControlPlane's ownership labels but no %s annotation, "+
				"which is the only mark a target cluster cannot forge — so it is not adopted, because the Managed "+
				"lifecycle would delete it (and everything in it) at teardown. Either something on that cluster "+
				"stripped the mark off a namespace this ControlPlane created, in which case set the annotation back "+
				"to %s and stop whatever removes it, or the labels were put there by somebody else, in which case "+
				"pick a free name",
			ns.Name, controlPlaneUIDAnnotation, cp.UID,
		)
	}
	if recorded := ns.Annotations[controlPlaneUIDAnnotation]; recorded != "" && recorded != string(cp.UID) {
		return fmt.Sprintf(
			"namespace %q on the target cluster records another ControlPlane's UID (%s, this one is %s), so it "+
				"belongs to a same-named ControlPlane on another management cluster — or to a deleted instance of "+
				"this one whose namespace was left behind. It is never adopted, because the Managed lifecycle would "+
				"delete it (and everything in it) at teardown. Pick a free name, or — once the objects in it are "+
				"known to be disposable — delete the namespace on the target cluster and let this ControlPlane "+
				"create it again",
			ns.Name, recorded, cp.UID,
		)
	}
	return fmt.Sprintf(
		"namespace %q already exists and was not created by this ControlPlane; the Managed lifecycle would "+
			"delete it (and everything in it) at teardown, so it is never adopted. Use lifecycle External to "+
			"place the service in a namespace the operator does not own, or pick a free name",
		ns.Name,
	)
}

// crossNamespaceChildMapper maps an event on a labelled cross-namespace child
// back to its owning ControlPlane. An unlabelled object yields no request, so
// same-namespace children keep flowing through Owns() alone and a foreign object
// in a service namespace wakes nobody.
func crossNamespaceChildMapper(_ context.Context, obj client.Object) []reconcile.Request {
	labels := obj.GetLabels()
	name, namespace := labels[controlPlaneNameLabel], labels[controlPlaneNamespaceLabel]
	if name == "" || namespace == "" {
		return nil
	}
	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{Namespace: namespace, Name: name},
	}}
}

// crossNamespaceChildHandler is crossNamespaceChildMapper as the event-handler
// factory a multicluster watch leg takes, so SetupWithManager's legs read as one
// call per kind. Every request it produces is pinned to the management cluster,
// where the ControlPlane CRs live, whatever cluster delivered the event (see
// commonmulticluster.LocalRequests).
func crossNamespaceChildHandler() mchandler.TypedEventHandlerFunc[client.Object, mcreconcile.Request] {
	return commonmulticluster.LocalRequests(crossNamespaceChildMapper)
}

// crossNamespaceChildPredicate admits only objects carrying both ControlPlane
// ownership labels — the same gate crossNamespaceChildMapper applies before it
// builds a request. Wiring it onto every cross-namespace Watch leg keeps the
// shared informers (and the newly-added cluster-wide Namespace informer) from
// invoking the mapper on every unlabelled object's events cluster-wide — foreign
// namespaces churned by other operators, ESO status ticks on ExternalSecrets this
// ControlPlane never placed — so only a labelled child's events reach the mapper,
// which then discards nothing.
func crossNamespaceChildPredicate() predicate.Predicate {
	return predicate.NewPredicateFuncs(func(obj client.Object) bool {
		labels := obj.GetLabels()
		return labels[controlPlaneNameLabel] != "" && labels[controlPlaneNamespaceLabel] != ""
	})
}

// reconcileNamespaces ensures the namespaces the ControlPlane's services are
// placed in outside its own, and drives the NamespacesReady condition. It runs
// FIRST in the pipeline: every later sub-reconciler projects into one of these
// namespaces, and applying into a namespace that does not exist (or is
// Terminating) fails with an error that names neither the ControlPlane nor the
// assignment that caused it.
//
// The two lifecycles are deliberately asymmetric:
//
//   - Managed — the operator CREATES the namespace and stamps it with the
//     ownership labels plus managed-by, which is what licenses the teardown to
//     delete it again. A namespace that already exists WITHOUT those labels is
//     never adopted: the condition fails loud with NamespaceNotOwned. Silently
//     taking over a namespace somebody else provisioned would mean deleting it,
//     and everything in it, when the ControlPlane goes.
//   - External — the operator only VERIFIES the namespace is there. It is never
//     created, never labelled, and never deleted; a missing one is an operator
//     error to fix out-of-band, so the condition parks on NamespaceNotFound and
//     requeues rather than conjuring the namespace the lifecycle said it does not
//     own.
//
// A namespace whose services name a target cluster is ensured on that cluster as
// well as at home, under whichever lifecycle it declares (see
// ensureServiceNamespace). The client is resolved per namespace and BEFORE
// anything is written, so a cluster that does not resolve parks the condition on
// the reason every operator reports that failure under and creates nothing, on
// either cluster.
//
// A ControlPlane with no assignments (the default) has nothing to ensure and
// reports True immediately.
func (r *ControlPlaneReconciler) reconcileNamespaces(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) (ctrl.Result, error) {
	fail := conditionFailer(cp, conditionTypeNamespacesReady)

	assignments := cp.DedicatedServiceNamespaces()
	if len(assignments) == 0 {
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeNamespacesReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: cp.Generation,
			Reason:             "NoDedicatedNamespaces",
			Message:            "no service declares a namespace of its own; every child is placed in the ControlPlane's namespace",
		})
		return ctrl.Result{}, nil
	}

	for _, assignment := range assignments {
		children, err := commonmulticluster.ResolveChildrenClient(ctx, r.Resolver, r.Client,
			targetClusterRefForNamespace(cp, assignment.Name))
		if err != nil {
			fail(commonmulticluster.TargetClusterUnavailable, err.Error())
			return ctrl.Result{RequeueAfter: namespaceRequeueAfter}, nil
		}

		// The management cluster always, the target cluster on top of it when the
		// namespace is placed on one (see clustersFor).
		for _, c := range r.clustersFor(children) {
			if result, err := r.ensureServiceNamespace(ctx, c, cp, assignment, fail); !result.IsZero() || err != nil {
				return result, err
			}
		}
	}

	conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditionTypeNamespacesReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: cp.Generation,
		Reason:             "NamespacesReady",
		Message:            fmt.Sprintf("all %d dedicated service namespace(s) are present", len(assignments)),
	})
	return ctrl.Result{}, nil
}

// ensureServiceNamespace applies one assignment's lifecycle rules on the cluster
// c reads and writes: it creates the namespace under Managed, verifies it under
// External, and reports through fail what reconcileNamespaces returns when the
// namespace is not usable there. A zero result and no error mean this cluster is
// settled.
//
// A placed namespace runs through it once per cluster, because it has to exist
// on both. The workload CR the ControlPlane projects does not move: it is
// created and reconciled on the management cluster, in the namespace the service
// is assigned to. The service operator that picks it up then projects ITS
// children into the namespace of the SAME NAME on the target cluster, where a
// write into a missing namespace fails. The two sides are ensured independently
// and report through one reason vocabulary, so an operator reads the same
// condition whichever cluster the namespace is missing on.
//
// The read goes through that cluster's LIVE reader (commonmulticluster.LiveReader,
// c itself on the management cluster). What it answers is an ownership question —
// adopt this namespace, or refuse it and park the whole chain — and an ownership
// decision made from a cache can be made on marks that are one resync behind:
// a namespace stamped moments ago would read as somebody else's. The teardown
// side of the same verdict already reads live (see deleteManagedNamespace).
func (r *ControlPlaneReconciler) ensureServiceNamespace(
	ctx context.Context,
	c client.Client,
	cp *c5c3v1alpha1.ControlPlane,
	assignment c5c3v1alpha1.ServiceNamespaceSpec,
	fail func(reason, message string),
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	ns := &corev1.Namespace{}
	err := commonmulticluster.LiveReader(c).Get(ctx, types.NamespacedName{Name: assignment.Name}, ns)

	if assignment.Lifecycle == c5c3v1alpha1.ServiceNamespaceLifecycleExternal {
		switch {
		case apierrors.IsNotFound(err):
			logger.Info("external service namespace does not exist, requeuing",
				"namespace", assignment.Name)
			fail("NamespaceNotFound", fmt.Sprintf(
				"namespace %q is declared with lifecycle External, so the operator never creates it; "+
					"provision it before this ControlPlane can converge", assignment.Name,
			))
			return ctrl.Result{RequeueAfter: namespaceRequeueAfter}, nil
		case err != nil:
			fail("NamespaceError", fmt.Sprintf("getting namespace %q: %v", assignment.Name, err))
			return ctrl.Result{}, fmt.Errorf("getting external service namespace %q: %w", assignment.Name, err)
		}
		// Present. Deliberately not labelled and not mutated: the lifecycle says
		// this namespace is not ours.
		if !ns.DeletionTimestamp.IsZero() {
			fail("NamespaceTerminating", fmt.Sprintf(
				"namespace %q is Terminating; waiting for it to be re-provisioned", assignment.Name,
			))
			return ctrl.Result{RequeueAfter: namespaceRequeueAfter}, nil
		}
		return ctrl.Result{}, nil
	}

	// Managed lifecycle.
	switch {
	case apierrors.IsNotFound(err):
		created := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name:        assignment.Name,
			Labels:      map[string]string{managedByLabel: managedByValue},
			Annotations: map[string]string{controlPlaneUIDAnnotation: string(cp.UID)},
		}}
		if cerr := claimChildOwnership(c, cp, created, r.Scheme); cerr != nil {
			fail("NamespaceError", fmt.Sprintf("claiming namespace %q: %v", assignment.Name, cerr))
			return ctrl.Result{}, fmt.Errorf("claiming managed service namespace %q: %w", assignment.Name, cerr)
		}
		switch cerr := c.Create(ctx, created); {
		case cerr == nil:
			// Created with our labels, so it is ours by construction: move on rather
			// than waiting a requeue to re-read what we just wrote.
			logger.Info("created managed service namespace", "namespace", assignment.Name)
			return ctrl.Result{}, nil
		case apierrors.IsAlreadyExists(cerr):
			// Another writer won the race. Requeue so the next pass re-Gets the
			// namespace and applies the ownership check below to whatever is
			// actually there — it may well be a namespace we must refuse to adopt.
			logger.Info("lost the race to create a managed service namespace, re-evaluating",
				"namespace", assignment.Name)
			return ctrl.Result{RequeueAfter: namespaceRequeueAfter}, nil
		default:
			fail("NamespaceError", fmt.Sprintf("creating namespace %q: %v", assignment.Name, cerr))
			return ctrl.Result{}, fmt.Errorf("creating managed service namespace %q: %w", assignment.Name, cerr)
		}
	case err != nil:
		fail("NamespaceError", fmt.Sprintf("getting namespace %q: %v", assignment.Name, err))
		return ctrl.Result{}, fmt.Errorf("getting managed service namespace %q: %w", assignment.Name, err)
	}

	// The namespace exists. Adopt it ONLY if we created it — the ownership labels
	// are the proof at home, and on a target cluster, where anyone with patch
	// access can write those two labels, cp's own UID mark on top of them
	// (ownsManagedNamespace). Anything else belongs to somebody else, and a Managed
	// lifecycle would eventually DELETE it, taking every workload in it along. Fail
	// loud instead: the operator either picks a free name or switches the
	// assignment to lifecycle External.
	if !ownsManagedNamespace(c, ns, cp) {
		logger.Info("refusing to adopt a pre-existing namespace under the Managed lifecycle",
			"namespace", assignment.Name)
		fail("NamespaceNotOwned", unownedNamespaceMessage(ns, cp))
		return ctrl.Result{RequeueAfter: namespaceRequeueAfter}, nil
	}

	if !ns.DeletionTimestamp.IsZero() {
		logger.Info("managed service namespace is Terminating, requeuing", "namespace", assignment.Name)
		fail("NamespaceTerminating", fmt.Sprintf(
			"namespace %q is Terminating; waiting for it to be reclaimed before re-creating it", assignment.Name,
		))
		return ctrl.Result{RequeueAfter: namespaceRequeueAfter}, nil
	}

	return ctrl.Result{}, nil
}
