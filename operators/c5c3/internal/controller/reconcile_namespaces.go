// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/c5c3/forge/internal/common/conditions"
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

// claimChildOwnership makes obj a child of cp, by the only mechanism the child's
// namespace permits: a controller owner reference when it shares cp's namespace
// (so the GC cascade reaps it), the ownership labels when it does not (so the
// finalizer-driven teardown can find and delete it). It is the single decision
// point, so no projection site has to re-derive which mechanism applies.
func claimChildOwnership(cp *c5c3v1alpha1.ControlPlane, obj client.Object, scheme *runtime.Scheme) error {
	if obj.GetNamespace() != cp.Namespace {
		stampControlPlaneChildLabels(obj, cp)
		return nil
	}
	return controllerutil.SetControllerReference(cp, obj, scheme)
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

// crossNamespaceChildHandler is crossNamespaceChildMapper as an event handler,
// so SetupWithManager's watch legs read as one call per kind.
func crossNamespaceChildHandler() handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(crossNamespaceChildMapper)
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
// A ControlPlane with no assignments (the default) has nothing to ensure and
// reports True immediately.
func (r *ControlPlaneReconciler) reconcileNamespaces(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
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
		ns := &corev1.Namespace{}
		err := r.Get(ctx, types.NamespacedName{Name: assignment.Name}, ns)

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
			continue
		}

		// Managed lifecycle.
		switch {
		case apierrors.IsNotFound(err):
			created := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: assignment.Name}}
			stampControlPlaneChildLabels(created, cp)
			created.Labels[managedByLabel] = managedByValue
			if cerr := r.Create(ctx, created); cerr != nil && !apierrors.IsAlreadyExists(cerr) {
				fail("NamespaceError", fmt.Sprintf("creating namespace %q: %v", assignment.Name, cerr))
				return ctrl.Result{}, fmt.Errorf("creating managed service namespace %q: %w", assignment.Name, cerr)
			}
			logger.Info("created managed service namespace", "namespace", assignment.Name)
			// Requeue rather than falling through: an AlreadyExists means another
			// writer won the race, and the next pass re-Gets it and applies the
			// ownership check below to whatever is actually there.
			return ctrl.Result{RequeueAfter: namespaceRequeueAfter}, nil
		case err != nil:
			fail("NamespaceError", fmt.Sprintf("getting namespace %q: %v", assignment.Name, err))
			return ctrl.Result{}, fmt.Errorf("getting managed service namespace %q: %w", assignment.Name, err)
		}

		// The namespace exists. Adopt it ONLY if we created it — the ownership
		// labels are the proof. Anything else belongs to somebody else, and a
		// Managed lifecycle would eventually DELETE it, taking every workload in it
		// along. Fail loud instead: the operator either picks a free name or
		// switches the assignment to lifecycle External.
		if !isControlPlaneChild(ns, cp) {
			logger.Info("refusing to adopt a pre-existing namespace under the Managed lifecycle",
				"namespace", assignment.Name)
			fail("NamespaceNotOwned", fmt.Sprintf(
				"namespace %q already exists and was not created by this ControlPlane; the Managed lifecycle would "+
					"delete it (and everything in it) at teardown, so it is never adopted. Use lifecycle External to "+
					"place the service in a namespace the operator does not own, or pick a free name",
				assignment.Name,
			))
			return ctrl.Result{RequeueAfter: namespaceRequeueAfter}, nil
		}

		if !ns.DeletionTimestamp.IsZero() {
			logger.Info("managed service namespace is Terminating, requeuing", "namespace", assignment.Name)
			fail("NamespaceTerminating", fmt.Sprintf(
				"namespace %q is Terminating; waiting for it to be reclaimed before re-creating it", assignment.Name,
			))
			return ctrl.Result{RequeueAfter: namespaceRequeueAfter}, nil
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
