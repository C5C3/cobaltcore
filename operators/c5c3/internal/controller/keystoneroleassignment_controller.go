// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"slices"

	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	crcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mchandler "sigs.k8s.io/multicluster-runtime/pkg/handler"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcruntime "sigs.k8s.io/multicluster-runtime/pkg/multicluster"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	"github.com/c5c3/cobaltcore/internal/common/bootstrap"
	"github.com/c5c3/cobaltcore/internal/common/conditions"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	"github.com/c5c3/cobaltcore/internal/common/watch"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// conditionTypeKeystoneRoleAssignmentAssignmentReady is the one sub-condition
// a KeystoneRoleAssignment carries; the aggregate Ready is derived from it.
const conditionTypeKeystoneRoleAssignmentAssignmentReady = "AssignmentReady"

// keystoneRoleAssignmentSubConditionTypes are the sub-conditions the aggregate
// Ready is derived from.
var keystoneRoleAssignmentSubConditionTypes = []string{conditionTypeKeystoneRoleAssignmentAssignmentReady}

// keystoneRoleAssignmentFinalizerName gates the teardown of the assignment an
// order created.
const keystoneRoleAssignmentFinalizerName = "c5c3.io/keystoneroleassignment-teardown"

// keystoneRoleAssignmentLabelKeys are the ownership labels an order's children
// carry, for the reason keystoneUserLabelKeys gives.
var keystoneRoleAssignmentLabelKeys = orderLabelKeys{
	Name:      "c5c3.io/keystoneroleassignment-name",
	Namespace: "c5c3.io/keystoneroleassignment-namespace",
	Cluster:   "c5c3.io/keystoneroleassignment-cluster",
}

// keystoneRoleAssignmentGVK is the kind the order watch filters target clusters
// on, and the kind the user and project holds watch.
var keystoneRoleAssignmentGVK = c5c3v1alpha1.GroupVersion.WithKind("KeystoneRoleAssignment")

// Condition reasons this controller introduces. The role refusal reuses
// reasonKeystoneServiceRoleNotAllowed, a project not yet provisioned
// reasonKeystoneProjectWaiting, and the K-ORC wait
// reasonWaitingForServiceAccounts; the gates reuse the scaffold's vocabulary.
const (
	// reasonKeystoneRoleAssignmentAssigned is AssignmentReady's True reason.
	reasonKeystoneRoleAssignmentAssigned = "RoleAssigned"
	// reasonKeystoneRoleAssignmentUserNotFound reports a userRef that names no
	// KeystoneUser in the order's namespace.
	reasonKeystoneRoleAssignmentUserNotFound = "UserNotFound"
	// reasonKeystoneRoleAssignmentProjectNotFound reports a projectRef that names
	// no KeystoneProject in the order's namespace.
	reasonKeystoneRoleAssignmentProjectNotFound = "ProjectNotFound"
	// reasonKeystoneRoleAssignmentControlPlaneMismatch reports a referenced order
	// of another ControlPlane.
	reasonKeystoneRoleAssignmentControlPlaneMismatch = "ControlPlaneMismatch"
	// reasonKeystoneRoleAssignmentWaitingForUser reports a referenced user that
	// is not provisioned yet.
	reasonKeystoneRoleAssignmentWaitingForUser = "WaitingForUser"
	// reasonKeystoneRoleAssignmentDuplicate reports an older order of the same
	// user, project and role in the namespace.
	reasonKeystoneRoleAssignmentDuplicate = "DuplicateRoleAssignment"
	// reasonKeystoneRoleAssignmentFailed reports a terminal K-ORC error on the
	// Role import or the RoleAssignment.
	reasonKeystoneRoleAssignmentFailed = "RoleAssignmentFailed"
	// reasonKeystoneRoleAssignmentError reports a Kubernetes-level failure
	// writing or reading the assignment's K-ORC objects.
	reasonKeystoneRoleAssignmentError = "RoleAssignmentError"
)

// KeystoneRoleAssignmentReconciler owns the KeystoneRoleAssignment lifecycle
// and is the single writer of its status. It serves orders on the management
// cluster and on target clusters the way the KeystoneUser reconciler does. The
// Role import and the RoleAssignment live in the ControlPlane's namespace on the
// management cluster. It records no Events.
type KeystoneRoleAssignmentReconciler struct {
	// Client is the management cluster's client.
	client.Client
	Scheme *runtime.Scheme
	// Resolver resolves the cluster an order lives on; nil resolves every
	// request to the management cluster.
	Resolver                commonmulticluster.ClusterResolver
	MaxConcurrentReconciles int
}

// RBAC for the KeystoneRoleAssignment kind: the controller reads the orders and
// updates them to install and release its finalizer; it never creates or
// deletes one. It reads the KeystoneUser and KeystoneProject orders an
// assignment references, which the markers of those kinds grant. The K-ORC Roles
// and RoleAssignments it writes are granted by the ControlPlane's marker block.
// The teardown hold reads the KeystoneApplicationCredentials beside the order.
// +kubebuilder:rbac:groups=c5c3.io,resources=keystoneroleassignments,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=c5c3.io,resources=keystoneroleassignments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=c5c3.io,resources=keystoneroleassignments/finalizers,verbs=update
// +kubebuilder:rbac:groups=c5c3.io,resources=keystoneapplicationcredentials,verbs=get;list;watch

// Reconcile drives one KeystoneRoleAssignment: the gates, finalizer
// installation, the projection and the teardown.
func (r *KeystoneRoleAssignmentReconciler) Reconcile(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
	cluster := string(req.ClusterName)
	oc, err := orderClient(ctx, r.Resolver, r.Client, cluster)
	if err != nil {
		// The cluster was deregistered between the event and this pass. Nothing
		// can be read or written there, and a requeue would only repeat this.
		log.FromContext(ctx).Info("the cluster the KeystoneRoleAssignment lives on does not resolve; skipping it",
			"cluster", cluster, "order", req.NamespacedName, "reason", err.Error())
		return ctrl.Result{}, nil
	}

	var order c5c3v1alpha1.KeystoneRoleAssignment
	if err := oc.Get(ctx, req.NamespacedName, &order); err != nil {
		if apierrors.IsNotFound(err) {
			log.FromContext(ctx).V(1).Info("KeystoneRoleAssignment not found; likely deleted")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching KeystoneRoleAssignment: %w", err)
	}

	if order.DeletionTimestamp != nil {
		return r.reconcileDelete(ctx, oc, &order, cluster)
	}

	statusBefore := order.Status.DeepCopy()
	result, err := r.reconcileNormal(ctx, oc, &order, cluster)
	return r.updateStatus(ctx, oc, &order, statusBefore, result, err)
}

// reconcileNormal runs the shared gates, then the order's own: the role
// allowlist, the two references and the duplicate check. Each of these refusals
// returns the refresh result: the ControlPlane watch delivers an allowlist edit
// on the management cluster, and the reference watches deliver a referenced
// order's change.
//
// A role taken off the allowlist freezes the order like a withdrawn entry
// (#1327 D2): nothing is projected, repaired or swept, and an assignment that
// already exists stays.
func (r *KeystoneRoleAssignmentReconciler) reconcileNormal(
	ctx context.Context, oc client.Client, order *c5c3v1alpha1.KeystoneRoleAssignment, cluster string,
) (ctrl.Result, error) {
	fail := keystoneRoleAssignmentFail(order)
	ref := keystoneRoleAssignmentRef(order, cluster)
	cp, entry, result, err := orderAdmission(ctx, r.Client, ref, order.Spec.ControlPlaneRef, fail)
	if err != nil || cp == nil {
		return result, err
	}

	if added, err := commonreconcile.EnsureFinalizer(ctx, oc, order, keystoneRoleAssignmentFinalizerName); err != nil {
		return ctrl.Result{}, err
	} else if added {
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueNextPass}, nil
	}

	if !orderAdminCredentialGate(cp, fail) {
		return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
	}

	refused := ctrl.Result{RequeueAfter: orderRefreshAfter}
	if roles := entry.RolesOutside([]string{order.Spec.Role}); len(roles) > 0 {
		fail(reasonKeystoneServiceRoleNotAllowed, namespaceAssignmentRoleMessage(cp, entry, roles))
		return refused, nil
	}

	user := &c5c3v1alpha1.KeystoneUser{}
	project := &c5c3v1alpha1.KeystoneProject{}
	cpKey := orderControlPlaneKey(order.Spec.ControlPlaneRef, order.Namespace)
	for _, reference := range userAndProjectReferences(order.Spec.UserRef.Name, order.Spec.ProjectRef.Name, user, project) {
		admitted, err := reference.gate(ctx, oc, order.Namespace, ref.location(), cpKey, fail)
		if err != nil || !admitted {
			return refused, err
		}
	}

	// The siblings are only read, so the cache's objects are listed without a copy.
	var siblings c5c3v1alpha1.KeystoneRoleAssignmentList
	if err := oc.List(ctx, &siblings, client.InNamespace(order.Namespace), client.UnsafeDisableDeepCopy); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing KeystoneRoleAssignments in %q: %w", order.Namespace, err)
	}
	if older := olderDuplicateRoleAssignment(order, siblings.Items); older != "" {
		fail(reasonKeystoneRoleAssignmentDuplicate, fmt.Sprintf(
			"KeystoneRoleAssignment %q already assigns role %q to user %q on project %q; delete this order",
			older, order.Spec.Role, order.Spec.UserRef.Name, order.Spec.ProjectRef.Name))
		return refused, nil
	}

	result, err = instrumenter.Instrument(ctx, "KeystoneRoleAssignmentProvision", func(ctx context.Context) (ctrl.Result, error) {
		return r.assignRole(ctx, order, cp, cluster, user, project)
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	return orderPassResult(cluster,
		conditions.AllTrue(order.Status.Conditions, conditionTypeKeystoneRoleAssignmentAssignmentReady), result), nil
}

// roleAssignmentReference is one order an assignment references, with the
// accessors its gate reads once obj is loaded.
type roleAssignmentReference struct {
	kind, noun, name  string
	obj               client.Object
	notFound, waiting string
	ready             func() *metav1.Condition
	controlPlaneRef   func() c5c3v1alpha1.ControlPlaneRefSpec
}

// userAndProjectReferences returns the gates of an order naming the
// KeystoneUser userName and the KeystoneProject projectName, loading them into
// user and project: a KeystoneRoleAssignment and a
// KeystoneApplicationCredential reference the same two kinds.
func userAndProjectReferences(
	userName, projectName string, user *c5c3v1alpha1.KeystoneUser, project *c5c3v1alpha1.KeystoneProject,
) []roleAssignmentReference {
	return []roleAssignmentReference{
		{
			kind: "KeystoneUser", noun: "user", name: userName, obj: user,
			notFound: reasonKeystoneRoleAssignmentUserNotFound, waiting: reasonKeystoneRoleAssignmentWaitingForUser,
			ready: func() *metav1.Condition {
				return conditions.GetCondition(user.Status.Conditions, conditionTypeKeystoneUserUserReady)
			},
			controlPlaneRef: func() c5c3v1alpha1.ControlPlaneRefSpec { return user.Spec.ControlPlaneRef },
		},
		{
			kind: "KeystoneProject", noun: "project", name: projectName, obj: project,
			notFound: reasonKeystoneRoleAssignmentProjectNotFound, waiting: reasonKeystoneProjectWaiting,
			ready: func() *metav1.Condition {
				return conditions.GetCondition(project.Status.Conditions, conditionTypeKeystoneProjectProjectReady)
			},
			controlPlaneRef: func() c5c3v1alpha1.ControlPlaneRefSpec { return project.Spec.ControlPlaneRef },
		},
	}
}

// gate reads the referenced order into obj from namespace on the assignment's
// cluster and reports whether the assignment may use it: the order exists,
// orders from the ControlPlane cpKey names, and is provisioned. A refusal is
// written through fail. A read error other than NotFound is returned wrapped.
func (ref roleAssignmentReference) gate(
	ctx context.Context, oc client.Client, namespace, location string, cpKey client.ObjectKey,
	fail func(reason, message string),
) (bool, error) {
	if err := oc.Get(ctx, types.NamespacedName{Namespace: namespace, Name: ref.name}, ref.obj); err != nil {
		if apierrors.IsNotFound(err) {
			fail(ref.notFound, fmt.Sprintf("%s %q is not in namespace %q on %s; the %s must be ordered from the same namespace",
				ref.kind, ref.name, namespace, location, ref.noun))
			return false, nil
		}
		return false, fmt.Errorf("reading referenced %s %s/%s: %w", ref.kind, namespace, ref.name, err)
	}
	if theirs := orderControlPlaneKey(ref.controlPlaneRef(), namespace); theirs != cpKey {
		fail(reasonKeystoneRoleAssignmentControlPlaneMismatch, fmt.Sprintf(
			"%s %q orders from ControlPlane %s, this order from %s", ref.kind, ref.name, theirs, cpKey))
		return false, nil
	}
	if cond := ref.ready(); cond == nil || cond.Status != metav1.ConditionTrue {
		reason := "not reconciled yet"
		if cond != nil {
			reason = cond.Reason
		}
		fail(ref.waiting, fmt.Sprintf("%s %q is not provisioned yet (%s)", ref.kind, ref.name, reason))
		return false, nil
	}
	return true, nil
}

// olderDuplicateRoleAssignment returns the name of the oldest other order in
// siblings that assigns order's role to the same user on the same project and
// was created before it, ties broken by name, or "" when there is none. Two
// such orders would project one Keystone assignment twice, and deleting either
// would unassign the role the other still declares.
func olderDuplicateRoleAssignment(
	order *c5c3v1alpha1.KeystoneRoleAssignment, siblings []c5c3v1alpha1.KeystoneRoleAssignment,
) string {
	older := func(a, b *c5c3v1alpha1.KeystoneRoleAssignment) bool {
		if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
			return a.CreationTimestamp.Before(&b.CreationTimestamp)
		}
		return a.Name < b.Name
	}
	var oldest *c5c3v1alpha1.KeystoneRoleAssignment
	for i := range siblings {
		other := &siblings[i]
		if other.Name == order.Name || other.Spec.UserRef.Name != order.Spec.UserRef.Name ||
			other.Spec.ProjectRef.Name != order.Spec.ProjectRef.Name || other.Spec.Role != order.Spec.Role ||
			!older(other, order) {
			continue
		}
		if oldest == nil || older(other, oldest) {
			oldest = other
		}
	}
	if oldest == nil {
		return ""
	}
	return oldest.Name
}

// assignRole projects the order's Role import and its managed RoleAssignment
// through applyAccountRole and writes AssignmentReady. The assignment binds the
// referenced user's managed User to the referenced project's managed Project and
// authenticates through the operator's admin PASSWORD cloud.
func (r *KeystoneRoleAssignmentReconciler) assignRole(
	ctx context.Context, order *c5c3v1alpha1.KeystoneRoleAssignment, cp *c5c3v1alpha1.ControlPlane, cluster string,
	user *c5c3v1alpha1.KeystoneUser, project *c5c3v1alpha1.KeystoneProject,
) (ctrl.Result, error) {
	fail := keystoneRoleAssignmentFail(order)
	requeue := ctrl.Result{RequeueAfter: korcRequeueAfter}
	role := order.Spec.Role
	credRef, managedCredRef := keystoneServiceCredentialRefs(cp)

	out, err := applyAccountRole(ctx, r.Client,
		keystoneRoleAssignmentRoleRef(order, cluster), keystoneRoleAssignmentAssignmentRef(order, cluster),
		cp.Namespace, role, credRef, managedCredRef,
		keystoneUserUserRef(user, cluster), keystoneProjectProjectRef(project, cluster),
		orderEnsure(r.Client, r.Scheme, order, keystoneRoleAssignmentRef(order, cluster)))
	if err != nil {
		fail(reasonKeystoneRoleAssignmentError, err.Error())
		return ctrl.Result{}, err
	}
	if termErr := out.terminalErr; termErr != nil {
		fail(reasonKeystoneRoleAssignmentFailed, fmt.Sprintf("K-ORC reported a terminal error on role %q: %v", role, termErr))
		return requeue, nil
	}
	if out.blocked != nil {
		message := fmt.Sprintf("the RoleAssignment for role %q is registered but not yet Available", role)
		if _, isImport := out.blocked.(*orcv1alpha1.Role); isImport {
			message = fmt.Sprintf("the Role import for role %q is registered but not yet Available", role)
			if out.stalled {
				message += fmt.Sprintf("; role %q may not exist in Keystone — create it there or fix the spelling", role)
			}
		}
		waitOrClassifyCondition(cp, fail, reasonWaitingForServiceAccounts, message, out.blocked)
		return requeue, nil
	}

	if res := out.assignment.Status.Resource; res != nil {
		order.Status.RoleID = res.RoleID
		order.Status.UserID = res.UserID
		order.Status.ProjectID = res.ProjectID
	}
	keystoneRoleAssignmentSetTrue(order, reasonKeystoneRoleAssignmentAssigned, fmt.Sprintf(
		"role %q is assigned to user %q on project %q", role, keystoneUserName(user), keystoneProjectName(project)))
	return ctrl.Result{}, nil
}

// referencingRoleAssignments returns the sorted names of the
// KeystoneRoleAssignments in namespace that matches selects, read through the
// order's cluster client. A cluster that does not serve the kind has none.
func referencingRoleAssignments(
	ctx context.Context, oc client.Client, namespace string, matches func(*c5c3v1alpha1.KeystoneRoleAssignment) bool,
) ([]string, error) {
	var list c5c3v1alpha1.KeystoneRoleAssignmentList
	if err := oc.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		if meta.IsNoMatchError(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("listing KeystoneRoleAssignments in %q: %w", namespace, err)
	}
	var names []string
	for i := range list.Items {
		if matches(&list.Items[i]) {
			names = append(names, list.Items[i].Name)
		}
	}
	slices.Sort(names)
	return names, nil
}

// keystoneRoleAssignmentFail returns a closure bound to order that writes
// AssignmentReady False, truncating the message for the reason keystoneUserFail
// gives.
func keystoneRoleAssignmentFail(order *c5c3v1alpha1.KeystoneRoleAssignment) func(reason, message string) {
	return func(reason, message string) {
		setTruncatedCondition(&order.Status.Conditions, order.Generation,
			conditionTypeKeystoneRoleAssignmentAssignmentReady, metav1.ConditionFalse, reason, message)
	}
}

// keystoneRoleAssignmentSetTrue writes AssignmentReady True.
func keystoneRoleAssignmentSetTrue(order *c5c3v1alpha1.KeystoneRoleAssignment, reason, message string) {
	setTruncatedCondition(&order.Status.Conditions, order.Generation,
		conditionTypeKeystoneRoleAssignmentAssignmentReady, metav1.ConditionTrue, reason, message)
}

// updateStatus persists the status through the order's cluster client, as the
// KeystoneUser reconciler's does.
func (r *KeystoneRoleAssignmentReconciler) updateStatus(
	ctx context.Context, oc client.Client, order *c5c3v1alpha1.KeystoneRoleAssignment,
	statusBefore *c5c3v1alpha1.KeystoneRoleAssignmentStatus, result ctrl.Result, reconcileErr error,
) (ctrl.Result, error) {
	return commonreconcile.UpdateStatus(ctx, oc, order, statusBefore, &order.Status, func() {
		commonreconcile.SetAggregateReady(&order.Status.Conditions, order.Generation, keystoneRoleAssignmentSubConditionTypes)
		order.Status.ObservedGeneration = order.Generation
	}, result, reconcileErr)
}

// --- naming ---

// keystoneRoleAssignmentRef identifies the order to the shared scaffold: its
// children carry the "roleassignment" segment and the keystoneroleassignment
// labels.
func keystoneRoleAssignmentRef(order *c5c3v1alpha1.KeystoneRoleAssignment, cluster string) orderRef {
	return orderRef{
		Name: order.Name, Namespace: order.Namespace, Cluster: cluster,
		Segment: "roleassignment", Keys: keystoneRoleAssignmentLabelKeys,
	}
}

// keystoneRoleAssignmentRoleRef is the unmanaged Role import's name.
func keystoneRoleAssignmentRoleRef(order *c5c3v1alpha1.KeystoneRoleAssignment, cluster string) string {
	return keystoneRoleAssignmentRef(order, cluster).childPrefix() + "role"
}

// keystoneRoleAssignmentAssignmentRef is the managed RoleAssignment's name.
func keystoneRoleAssignmentAssignmentRef(order *c5c3v1alpha1.KeystoneRoleAssignment, cluster string) string {
	return keystoneRoleAssignmentRef(order, cluster).childPrefix() + "assignment"
}

// --- teardown ---

// reconcileDelete removes the RoleAssignment and the Role import the order
// created and releases the finalizer. K-ORC unassigns the role in Keystone
// before its finalizer releases the RoleAssignment, and orderTeardown waits for
// that while the ControlPlane exists.
//
// The teardown holds while a KeystoneApplicationCredential in the order's
// namespace names the same user and project: K-ORC mints and deletes its
// credentials with a token scoped to the project, which needs the role. Every
// assignment of the pair holds, not only the last one, so the rule is
// predictable: the credential order goes first.
func (r *KeystoneRoleAssignmentReconciler) reconcileDelete(
	ctx context.Context, oc client.Client, order *c5c3v1alpha1.KeystoneRoleAssignment, cluster string,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(order, keystoneRoleAssignmentFinalizerName) {
		return ctrl.Result{}, nil
	}
	credentials, err := referencingApplicationCredentials(ctx, oc, order.Namespace,
		func(ac *c5c3v1alpha1.KeystoneApplicationCredential) bool {
			return roleAssignmentBindsCredentialPair(order, ac)
		})
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(credentials) > 0 {
		statusBefore := order.Status.DeepCopy()
		keystoneRoleAssignmentFail(order)(reasonOrderReferencedByApplicationCredentials,
			orderReferencedByApplicationCredentialsMessage(credentials, order.Namespace, "role assignment",
				"the credentials are minted and deleted with a token scoped to the project, which needs the role"))
		return r.updateStatus(ctx, oc, order, statusBefore,
			ctrl.Result{RequeueAfter: orderReferenceHoldRequeueAfter}, nil)
	}

	ref := keystoneRoleAssignmentRef(order, cluster)
	return orderTeardown(ctx, r.Client, oc, order, keystoneRoleAssignmentFinalizerName,
		orderControlPlaneKey(order.Spec.ControlPlaneRef, order.Namespace),
		func(ctx context.Context, childNS string) (int, error) {
			return sweepOrderLists(ctx, r.Client, order, ref, childNS,
				orderSweep{list: &orcv1alpha1.RoleAssignmentList{}}, orderSweep{list: &orcv1alpha1.RoleList{}})
		})
}

// --- manager setup ---

// KeystoneRoleAssignmentControlPlaneRefIndexKey is the field-indexer key under
// which a KeystoneRoleAssignment on the management cluster is indexed by its
// resolved ControlPlane reference, "<namespace>/<name>".
const KeystoneRoleAssignmentControlPlaneRefIndexKey = "spec.controlPlaneRef"

// keystoneRoleAssignmentControlPlaneRefExtractor returns the resolved
// "<namespace>/<name>" of obj's ControlPlane reference. An empty name indexes
// nothing.
func keystoneRoleAssignmentControlPlaneRefExtractor(obj client.Object) []string {
	order, ok := obj.(*c5c3v1alpha1.KeystoneRoleAssignment)
	if !ok || order.Spec.ControlPlaneRef.Name == "" {
		return nil
	}
	key := orderControlPlaneKey(order.Spec.ControlPlaneRef, order.Namespace)
	return []string{keystoneServiceControlPlaneRefIndexValue(key.Namespace, key.Name)}
}

// controlPlaneToKeystoneRoleAssignmentsMapper maps a ControlPlane event to the
// orders on the management cluster that reference it. A List failure is logged
// and maps to nothing.
func controlPlaneToKeystoneRoleAssignmentsMapper(c client.Reader) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		var orders c5c3v1alpha1.KeystoneRoleAssignmentList
		if err := c.List(ctx, &orders, client.MatchingFields{
			KeystoneRoleAssignmentControlPlaneRefIndexKey: keystoneServiceControlPlaneRefIndexValue(obj.GetNamespace(), obj.GetName()),
		}); err != nil {
			log.FromContext(ctx).Error(err, "listing KeystoneRoleAssignments for ControlPlane watch")
			return nil
		}
		requests := make([]reconcile.Request, 0, len(orders.Items))
		for i := range orders.Items {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&orders.Items[i])})
		}
		return requests
	}
}

// roleAssignmentUserName reads the KeystoneUser an assignment names.
func roleAssignmentUserName(ra *c5c3v1alpha1.KeystoneRoleAssignment) string {
	return ra.Spec.UserRef.Name
}

// roleAssignmentProjectName reads the KeystoneProject an assignment names.
func roleAssignmentProjectName(ra *c5c3v1alpha1.KeystoneRoleAssignment) string {
	return ra.Spec.ProjectRef.Name
}

// roleAssignmentToReferencedOrderRequests is the event-handler factory of the
// user and project hold legs: it maps a KeystoneRoleAssignment to the order
// refName reads from it, in the assignment's namespace, on the cluster the event
// came from. referencedOrderToRoleAssignmentRequests maps the other way.
func roleAssignmentToReferencedOrderRequests(
	refName func(*c5c3v1alpha1.KeystoneRoleAssignment) string,
) mchandler.TypedEventHandlerFunc[client.Object, mcreconcile.Request] {
	return mchandler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []reconcile.Request {
		ra, ok := obj.(*c5c3v1alpha1.KeystoneRoleAssignment)
		if !ok || refName(ra) == "" {
			return nil
		}
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: ra.Namespace, Name: refName(ra)}}}
	})
}

// keystoneRoleAssignmentReferenceMapper maps a referenced KeystoneUser or
// KeystoneProject on clusterName to every assignment in its namespace whose
// refName is the referenced order's name, listing through c, that cluster's
// cache. A List failure is logged and maps to nothing.
func keystoneRoleAssignmentReferenceMapper(
	clusterName mcruntime.ClusterName, c client.Reader, refName func(*c5c3v1alpha1.KeystoneRoleAssignment) string,
) func(context.Context, client.Object) []mcreconcile.Request {
	return func(ctx context.Context, obj client.Object) []mcreconcile.Request {
		// The items are only read, matched by name and dropped, so no copy is taken.
		var orders c5c3v1alpha1.KeystoneRoleAssignmentList
		if err := c.List(ctx, &orders, client.InNamespace(obj.GetNamespace()), client.UnsafeDisableDeepCopy); err != nil {
			log.FromContext(ctx).Error(err, "listing KeystoneRoleAssignments for a referenced order",
				"cluster", clusterName, "order", client.ObjectKeyFromObject(obj))
			return nil
		}
		var requests []mcreconcile.Request
		for i := range orders.Items {
			if refName(&orders.Items[i]) == obj.GetName() {
				requests = append(requests, mcreconcile.Request{
					Request:     reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&orders.Items[i])},
					ClusterName: clusterName,
				})
			}
		}
		return requests
	}
}

// referencedOrderToRoleAssignmentRequests is the event-handler factory of the
// reference legs: it maps a referenced KeystoneUser or KeystoneProject to the
// assignments naming it, binding keystoneRoleAssignmentReferenceMapper to the
// cluster the event came from.
func referencedOrderToRoleAssignmentRequests(
	refName func(*c5c3v1alpha1.KeystoneRoleAssignment) string,
) mchandler.TypedEventHandlerFunc[client.Object, mcreconcile.Request] {
	return func(clusterName mcruntime.ClusterName, cl cluster.Cluster) handler.TypedEventHandler[client.Object, mcreconcile.Request] {
		return mchandler.TypedEnqueueRequestsFromMapFuncWithClusterPreservation(
			keystoneRoleAssignmentReferenceMapper(clusterName, cl.GetClient(), refName))
	}
}

// applicationCredentialToRoleAssignmentRequests is the event-handler factory of
// the hold leg: it maps a KeystoneApplicationCredential to every assignment in
// its namespace binding the same user and project, listing through the cache of
// the cluster the event came from. A List failure is logged and maps to
// nothing.
func applicationCredentialToRoleAssignmentRequests() mchandler.TypedEventHandlerFunc[client.Object, mcreconcile.Request] {
	return func(clusterName mcruntime.ClusterName, cl cluster.Cluster) handler.TypedEventHandler[client.Object, mcreconcile.Request] {
		return mchandler.TypedEnqueueRequestsFromMapFuncWithClusterPreservation(
			func(ctx context.Context, obj client.Object) []mcreconcile.Request {
				ac, ok := obj.(*c5c3v1alpha1.KeystoneApplicationCredential)
				if !ok {
					return nil
				}
				// The items are only read, matched and dropped, so no copy is taken.
				var orders c5c3v1alpha1.KeystoneRoleAssignmentList
				if err := cl.GetClient().List(ctx, &orders, client.InNamespace(ac.Namespace),
					client.UnsafeDisableDeepCopy); err != nil {
					log.FromContext(ctx).Error(err, "listing KeystoneRoleAssignments for a KeystoneApplicationCredential",
						"cluster", clusterName, "order", client.ObjectKeyFromObject(ac))
					return nil
				}
				var requests []mcreconcile.Request
				for i := range orders.Items {
					ra := &orders.Items[i]
					if roleAssignmentBindsCredentialPair(ra, ac) {
						requests = append(requests, mcreconcile.Request{
							Request:     reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ra)},
							ClusterName: clusterName,
						})
					}
				}
				return requests
			})
	}
}

// keystoneRoleAssignmentReferencePredicate passes the updates of a referenced
// order an assignment's gates read: a spec change, which moves the generation,
// a deletion, and a change of the Ready condition or the sub-condition the gate
// checks (UserReady, ProjectReady). watch.CRUpdatePredicate would drop the
// status changes, and a referenced order becoming Ready is what an assignment
// waits for.
func keystoneRoleAssignmentReferencePredicate() predicate.Funcs {
	readiness := func(obj client.Object) [2]metav1.ConditionStatus {
		var conds []metav1.Condition
		gateType := ""
		switch o := obj.(type) {
		case *c5c3v1alpha1.KeystoneUser:
			conds, gateType = o.Status.Conditions, conditionTypeKeystoneUserUserReady
		case *c5c3v1alpha1.KeystoneProject:
			conds, gateType = o.Status.Conditions, conditionTypeKeystoneProjectProjectReady
		}
		status := func(condType string) metav1.ConditionStatus {
			if cond := conditions.GetCondition(conds, condType); cond != nil {
				return cond.Status
			}
			return ""
		}
		return [2]metav1.ConditionStatus{status(conditionTypeReady), status(gateType)}
	}
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			return e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration() ||
				!e.ObjectOld.GetDeletionTimestamp().Equal(e.ObjectNew.GetDeletionTimestamp()) ||
				readiness(e.ObjectOld) != readiness(e.ObjectNew)
		},
	}
}

// SetupWithManager registers the KeystoneRoleAssignmentReconciler with the
// multicluster manager.
func (r *KeystoneRoleAssignmentReconciler) SetupWithManager(mgr mcmanager.Manager) error {
	return r.setupWithOptions(mgr, bootstrap.TypedControllerOptions[mcreconcile.Request](r.MaxConcurrentReconciles))
}

// setupWithOptions carries the production setup SetupWithManager applies, with
// the controller options as a parameter so the integration suites register this
// chain with SkipNameValidation set.
//
// The watches:
//   - For: the order on the management cluster and on every engaged target
//     cluster that serves the kind.
//   - The RoleAssignment and Role children in the ControlPlane's namespace,
//     mapped back by their labels.
//   - KeystoneUser and KeystoneProject: the assignments in the referenced
//     order's namespace that name it, on the referenced order's cluster, on the
//     updates keystoneRoleAssignmentReferencePredicate passes.
//   - KeystoneApplicationCredential: the assignments of the same user and
//     project, so a credential order leaving wakes a held teardown.
//   - ControlPlane: the orders on the management cluster that reference it, on
//     the updates orderControlPlanePredicate passes.
//
// The index goes on the local field indexer, for the reason the KeystoneUser
// reconciler gives.
func (r *KeystoneRoleAssignmentReconciler) setupWithOptions(mgr mcmanager.Manager, opts crcontroller.TypedOptions[mcreconcile.Request]) error {
	local := mgr.GetLocalManager()
	if err := local.GetFieldIndexer().IndexField(context.Background(), &c5c3v1alpha1.KeystoneRoleAssignment{},
		KeystoneRoleAssignmentControlPlaneRefIndexKey, keystoneRoleAssignmentControlPlaneRefExtractor); err != nil {
		return err
	}
	engageLocal := commonmulticluster.EngageLocalCluster
	engageNoProviders := commonmulticluster.EngageNoProviderClusters
	engageProviders := mcbuilder.WithEngageWithProviderClusters(true)
	referencePredicate := mcbuilder.WithPredicates(keystoneRoleAssignmentReferencePredicate())

	return mcbuilder.ControllerManagedBy(mgr).
		WithOptions(opts).
		For(&c5c3v1alpha1.KeystoneRoleAssignment{}, mcbuilder.WithPredicates(watch.CRUpdatePredicate()),
			engageLocal, engageProviders,
			mcbuilder.WithClusterFilter(commonmulticluster.ClusterServesKind(keystoneRoleAssignmentGVK))).
		Watches(&orcv1alpha1.RoleAssignment{}, orderChildRequests(keystoneRoleAssignmentLabelKeys),
			engageLocal, engageNoProviders).
		Watches(&orcv1alpha1.Role{}, orderChildRequests(keystoneRoleAssignmentLabelKeys), engageLocal, engageNoProviders).
		Watches(&c5c3v1alpha1.KeystoneUser{},
			referencedOrderToRoleAssignmentRequests(roleAssignmentUserName),
			referencePredicate, engageLocal, engageProviders,
			mcbuilder.WithClusterFilter(commonmulticluster.ClusterServesKind(keystoneUserGVK))).
		Watches(&c5c3v1alpha1.KeystoneProject{},
			referencedOrderToRoleAssignmentRequests(roleAssignmentProjectName),
			referencePredicate, engageLocal, engageProviders,
			mcbuilder.WithClusterFilter(commonmulticluster.ClusterServesKind(keystoneProjectGVK))).
		Watches(&c5c3v1alpha1.KeystoneApplicationCredential{}, applicationCredentialToRoleAssignmentRequests(),
			mcbuilder.WithPredicates(watch.CRUpdatePredicate()), engageLocal, engageProviders,
			mcbuilder.WithClusterFilter(commonmulticluster.ClusterServesKind(keystoneApplicationCredentialGVK))).
		Watches(&c5c3v1alpha1.ControlPlane{},
			commonmulticluster.LocalRequests(controlPlaneToKeystoneRoleAssignmentsMapper(local.GetClient())),
			mcbuilder.WithPredicates(orderControlPlanePredicate()), engageLocal, engageNoProviders).
		// Reconcile answers an unresolvable cluster itself, so the wrapper that
		// would turn a cluster-not-found error into a success stays off.
		WithClusterNotFoundWrapper(false).
		Complete(r)
}
