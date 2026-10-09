// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// reconcileNamespaceAssignments reports what every spec.namespaceAssignments
// entry resolves to in status.namespaceAssignments, one status entry per spec
// entry and in spec order, and drives NamespaceAssignmentsReady.
//
// It only reads. Per entry it resolves the entry's cluster and GETs the assigned
// namespace there, through the uncached reader on a target cluster: at most 32
// GETs per pass and no LIST. It never creates, labels or deletes an assigned
// namespace, and it writes nothing on any cluster, so the remote-children
// finalizer is not involved.
//
// Each GET is bounded by namespaceAssignmentReadTimeout, and a cluster that did
// not answer one is not asked again in the same pass: its later entries report
// the same error. A cluster that accepts no connection therefore costs the pass
// one timeout in total.
//
// It never returns an error. A cluster that does not resolve or does not answer
// becomes per-entry status and a False condition, which turns the aggregate Ready
// False: an order from that namespace is refused until the cluster resolves or
// the entry is removed. A missing namespace keeps the condition True, because an
// assignment may precede its namespace. While any entry is not Assigned the step
// requeues after namespaceAssignmentRequeueAfter, because no watch reaches an
// assigned namespace.
func (r *ControlPlaneReconciler) reconcileNamespaceAssignments(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane,
) (ctrl.Result, error) {
	assignments := cp.Spec.NamespaceAssignments
	if len(assignments) == 0 {
		cp.Status.NamespaceAssignments = nil
		setNamespaceAssignmentsReady(cp, "NoNamespaceAssignments", "no namespace is assigned to a service owner")
		return ctrl.Result{}, nil
	}

	statuses := make([]c5c3v1alpha1.NamespaceAssignmentStatus, 0, len(assignments))
	var unavailable []string
	unresolved, unreachable, missing := false, false, 0
	var result ctrl.Result
	unanswered := map[string]error{}
	for i := range assignments {
		a := &assignments[i]
		status := r.observeNamespaceAssignment(ctx, a, unanswered)
		statuses = append(statuses, status)
		if status.Reason != c5c3v1alpha1.NamespaceAssignmentAssigned {
			result.RequeueAfter = namespaceAssignmentRequeueAfter
		}
		switch status.Reason {
		case c5c3v1alpha1.NamespaceAssignmentTargetClusterUnavailable:
			unresolved = true
			unavailable = append(unavailable, a.Describe())
		case c5c3v1alpha1.NamespaceAssignmentClusterUnreachable:
			unreachable = true
			unavailable = append(unavailable, a.Describe())
		case c5c3v1alpha1.NamespaceAssignmentNamespaceNotFound:
			missing++
		case c5c3v1alpha1.NamespaceAssignmentAssigned, c5c3v1alpha1.NamespaceAssignmentNamespaceTerminating:
			// The cluster answered and the namespace exists; nothing to count.
		}
	}
	cp.Status.NamespaceAssignments = statuses

	fail := conditionFailer(cp, conditionTypeNamespaceAssignmentsReady)
	unavailableMessage := "namespace assignment(s) without a reachable cluster: " + strings.Join(unavailable, ", ") +
		"; orders from these namespaces are refused until the cluster resolves or the entry is removed"
	switch {
	case unresolved:
		fail(commonmulticluster.TargetClusterUnavailable, unavailableMessage)
	case unreachable:
		fail(string(c5c3v1alpha1.NamespaceAssignmentClusterUnreachable), unavailableMessage)
	default:
		message := fmt.Sprintf("all %d namespace assignment(s) resolve to a reachable cluster", len(assignments))
		if missing > 0 {
			message += fmt.Sprintf("; %d assigned namespace(s) do not exist yet", missing)
		}
		setNamespaceAssignmentsReady(cp, "NamespaceAssignmentsReady", message)
	}
	return result, nil
}

// namespaceAssignmentReadTimeout bounds one namespace GET of the
// NamespaceAssignments step. A remote rest.Config carries no timeout of its own,
// so without it a cluster that accepts no connection holds the pass for the
// transport's dial and health-check limits.
const namespaceAssignmentReadTimeout = 10 * time.Second

// observeNamespaceAssignment resolves one entry's cluster and reads its
// namespace there, returning the entry's status. A failure is logged and
// reported in the returned status rather than returned as an error.
//
// unanswered maps a cluster name to the error of a read that cluster did not
// answer earlier in the pass. An entry on such a cluster reports that error
// without a read of its own, and a read the cluster does not answer adds it. A
// read the cluster answers with an API error is not recorded, because the
// answer may differ per namespace.
func (r *ControlPlaneReconciler) observeNamespaceAssignment(
	ctx context.Context, a *c5c3v1alpha1.NamespaceAssignmentSpec, unanswered map[string]error,
) c5c3v1alpha1.NamespaceAssignmentStatus {
	logger := log.FromContext(ctx).WithValues("assignedNamespace", a.Namespace, "location", a.Location())
	status := c5c3v1alpha1.NamespaceAssignmentStatus{
		Namespace:           a.Namespace,
		TargetClusterRef:    a.TargetClusterRef.DeepCopy(),
		AllowedRoles:        slices.Clone(a.AllowedRoles),
		AllowCatalogEntries: a.AllowCatalogEntries,
	}

	c, err := commonmulticluster.ResolveChildrenClient(ctx, r.Resolver, r.Client, a.TargetClusterRef)
	if err != nil {
		logger.Info("the cluster of a namespace assignment does not resolve", "error", err.Error())
		status.Reason = c5c3v1alpha1.NamespaceAssignmentTargetClusterUnavailable
		status.Message = truncateConditionMessage(err.Error())
		return status
	}

	cluster := a.TargetClusterName()
	if prev, seen := unanswered[cluster]; seen {
		status.Reason = c5c3v1alpha1.NamespaceAssignmentClusterUnreachable
		status.Message = truncateConditionMessage(fmt.Sprintf(
			"namespace %q not read: %s did not answer an earlier read in this pass: %v", a.Namespace, a.Location(), prev))
		return status
	}

	ns := &corev1.Namespace{}
	readCtx, cancel := context.WithTimeout(ctx, namespaceAssignmentReadTimeout)
	defer cancel()
	err = commonmulticluster.LiveReader(c).Get(readCtx, types.NamespacedName{Name: a.Namespace}, ns)
	switch {
	case apierrors.IsNotFound(err):
		status.ClusterReachable = true
		status.Reason = c5c3v1alpha1.NamespaceAssignmentNamespaceNotFound
		status.Message = truncateConditionMessage(fmt.Sprintf(
			"namespace %q does not exist on %s yet", a.Namespace, a.Location()))
	case err != nil:
		logger.Info("reading the namespace of a namespace assignment failed", "error", err.Error())
		var apiStatus apierrors.APIStatus
		if !errors.As(err, &apiStatus) {
			unanswered[cluster] = err
		}
		status.Reason = c5c3v1alpha1.NamespaceAssignmentClusterUnreachable
		status.Message = truncateConditionMessage(fmt.Sprintf("getting namespace %q: %v", a.Namespace, err))
	case !ns.DeletionTimestamp.IsZero():
		status.ClusterReachable, status.NamespaceExists = true, true
		status.Reason = c5c3v1alpha1.NamespaceAssignmentNamespaceTerminating
		status.Message = truncateConditionMessage(fmt.Sprintf(
			"namespace %q on %s is terminating", a.Namespace, a.Location()))
	default:
		status.ClusterReachable, status.NamespaceExists = true, true
		status.Reason = c5c3v1alpha1.NamespaceAssignmentAssigned
	}
	return status
}

// setNamespaceAssignmentsReady upserts the True arms of the condition. The False
// arms go through conditionFailer, which truncates for them.
func setNamespaceAssignmentsReady(cp *c5c3v1alpha1.ControlPlane, reason, message string) {
	conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditionTypeNamespaceAssignmentsReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: cp.Generation,
		Reason:             reason,
		Message:            message,
	})
}
