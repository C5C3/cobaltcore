// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mchandler "sigs.k8s.io/multicluster-runtime/pkg/handler"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	"github.com/c5c3/cobaltcore/internal/common/bootstrap"
	"github.com/c5c3/cobaltcore/internal/common/conditions"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	"github.com/c5c3/cobaltcore/internal/common/watch"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// conditionTypeKeystoneProjectProjectReady is the one sub-condition a
// KeystoneProject carries; the aggregate Ready is derived from it.
const conditionTypeKeystoneProjectProjectReady = "ProjectReady"

// keystoneProjectSubConditionTypes are the sub-conditions the aggregate Ready
// is derived from.
var keystoneProjectSubConditionTypes = []string{conditionTypeKeystoneProjectProjectReady}

// keystoneProjectFinalizerName gates the teardown of the project an order
// created.
const keystoneProjectFinalizerName = "c5c3.io/keystoneproject-teardown"

// keystoneProjectLabelKeys are the ownership labels an order's children carry,
// for the reason keystoneUserLabelKeys gives.
var keystoneProjectLabelKeys = orderLabelKeys{
	Name:      "c5c3.io/keystoneproject-name",
	Namespace: "c5c3.io/keystoneproject-namespace",
	Cluster:   "c5c3.io/keystoneproject-cluster",
}

// keystoneProjectGVK is the kind the order watch filters target clusters on.
var keystoneProjectGVK = c5c3v1alpha1.GroupVersion.WithKind("KeystoneProject")

// Condition reasons this controller introduces. The gates reuse the scaffold's
// and the KeystoneService vocabulary instead (reasonOrderNamespaceNotAssigned,
// reasonOrderClusterNameTooLong, reasonKeystoneServiceControlPlaneNotFound,
// reasonWaitingForServiceAccountAdmin, reasonProbingForCollision,
// conditionReasonTransportErrorRetryFailed).
const (
	// reasonKeystoneProjectProvisioned is ProjectReady's True reason.
	reasonKeystoneProjectProvisioned = "ProjectProvisioned"
	// reasonKeystoneProjectCollision reports a project name the ControlPlane
	// reserves, or a project of that name the order did not create.
	reasonKeystoneProjectCollision = "ProjectCollision"
	// reasonKeystoneProjectWaiting reports a project K-ORC has not made
	// Available yet.
	reasonKeystoneProjectWaiting = "WaitingForProject"
	// reasonKeystoneProjectFailed reports a terminal K-ORC error on the project.
	reasonKeystoneProjectFailed = "ProjectFailed"
	// reasonKeystoneProjectError reports a Kubernetes-level failure writing or
	// reading the project's K-ORC objects.
	reasonKeystoneProjectError = "ProjectError"
)

// KeystoneProjectReconciler owns the KeystoneProject lifecycle and is the single
// writer of its status. Like the KeystoneUser reconciler it serves orders on the
// management cluster and on target clusters: a request carries the cluster the
// order was seen on, and the order, its status and its finalizer are read and
// written there. The K-ORC Project lives in the ControlPlane's namespace on the
// management cluster. It records no Events, for the reason the KeystoneUser
// reconciler gives.
type KeystoneProjectReconciler struct {
	// Client is the management cluster's client.
	client.Client
	Scheme *runtime.Scheme
	// Resolver resolves the cluster an order lives on; nil resolves every
	// request to the management cluster.
	Resolver                commonmulticluster.ClusterResolver
	MaxConcurrentReconciles int
}

// RBAC for the KeystoneProject kind: the controller reads the orders and
// updates them to install and release its finalizer; it never creates or
// deletes one. The K-ORC Projects it writes and the ControlPlane reads are
// granted by the ControlPlane's marker block. On a target cluster the
// target-cluster-access chart's Role for an assigned namespace grants the same
// verbs. The teardown holds read the KeystoneRoleAssignments and
// KeystoneApplicationCredentials beside the order.
// +kubebuilder:rbac:groups=c5c3.io,resources=keystoneprojects,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=c5c3.io,resources=keystoneprojects/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=c5c3.io,resources=keystoneprojects/finalizers,verbs=update
// +kubebuilder:rbac:groups=c5c3.io,resources=keystoneroleassignments,verbs=get;list;watch
// +kubebuilder:rbac:groups=c5c3.io,resources=keystoneapplicationcredentials,verbs=get;list;watch

// Reconcile drives one KeystoneProject: the gates, finalizer installation, the
// provision and the teardown.
func (r *KeystoneProjectReconciler) Reconcile(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
	cluster := string(req.ClusterName)
	oc, err := orderClient(ctx, r.Resolver, r.Client, cluster)
	if err != nil {
		// The cluster was deregistered between the event and this pass. Nothing
		// can be read or written there, and a requeue would only repeat this.
		log.FromContext(ctx).Info("the cluster the KeystoneProject lives on does not resolve; skipping it",
			"cluster", cluster, "order", req.NamespacedName, "reason", err.Error())
		return ctrl.Result{}, nil
	}

	var order c5c3v1alpha1.KeystoneProject
	if err := oc.Get(ctx, req.NamespacedName, &order); err != nil {
		if apierrors.IsNotFound(err) {
			log.FromContext(ctx).V(1).Info("KeystoneProject not found; likely deleted")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching KeystoneProject: %w", err)
	}

	if order.DeletionTimestamp != nil {
		return r.reconcileDelete(ctx, oc, &order, cluster)
	}

	statusBefore := order.Status.DeepCopy()
	result, err := r.reconcileNormal(ctx, oc, &order, cluster)
	return r.updateStatus(ctx, oc, &order, statusBefore, result, err)
}

// reconcileNormal runs the shared gates, installs the finalizer past the
// consent gate, and provisions the project.
func (r *KeystoneProjectReconciler) reconcileNormal(
	ctx context.Context, oc client.Client, order *c5c3v1alpha1.KeystoneProject, cluster string,
) (ctrl.Result, error) {
	fail := keystoneProjectFail(order)
	cp, _, result, err := orderAdmission(ctx, r.Client, keystoneProjectRef(order, cluster),
		order.Spec.ControlPlaneRef, fail)
	if err != nil || cp == nil {
		return result, err
	}

	if added, err := commonreconcile.EnsureFinalizer(ctx, oc, order, keystoneProjectFinalizerName); err != nil {
		return ctrl.Result{}, err
	} else if added {
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueNextPass}, nil
	}

	if !orderAdminCredentialGate(cp, fail) {
		return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
	}

	result, err = instrumenter.Instrument(ctx, "KeystoneProjectProvision", func(ctx context.Context) (ctrl.Result, error) {
		return r.provisionProject(ctx, order, cp, cluster)
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	return orderPassResult(cluster,
		conditions.AllTrue(order.Status.Conditions, conditionTypeKeystoneProjectProjectReady), result), nil
}

// provisionProject creates the order's Keystone project through K-ORC in the
// ControlPlane's namespace and writes ProjectReady. The project is created in
// the ControlPlane's admin domain. The outcomes are a chain of early returns: a
// reserved name, the collision probe, the apply, a terminal K-ORC error, the
// wait.
func (r *KeystoneProjectReconciler) provisionProject(
	ctx context.Context, order *c5c3v1alpha1.KeystoneProject, cp *c5c3v1alpha1.ControlPlane, cluster string,
) (ctrl.Result, error) {
	fail := keystoneProjectFail(order)
	requeue := ctrl.Result{RequeueAfter: korcRequeueAfter}
	name := keystoneProjectName(order)
	domain := adminDomainName(cp)
	ref := keystoneProjectRef(order, cluster)

	// An order may never resolve to a project the ControlPlane creates in the
	// admin domain itself: the managed Project would take it over and delete it
	// from Keystone at teardown. Only an edit of the ControlPlane clears the
	// admin case, so there is no requeue beyond the refresh.
	if slices.ContainsFunc(keystoneProjectReservedNames(cp), func(reserved string) bool {
		return keystoneNameKey(reserved) == keystoneNameKey(name)
	}) {
		fail(reasonKeystoneProjectCollision, fmt.Sprintf(
			"the order resolves to project %q in domain %q, which ControlPlane %s/%s reserves for its admin project "+
				"or a built-in service project; it is never provisioned", name, domain, cp.Namespace, cp.Name))
		return ctrl.Result{}, nil
	}

	// K-ORC's managed create takes over a same-named project instead of
	// failing, so a probe decides first. adopt is never set: an order never
	// takes a project over.
	credRef, managedCredRef := keystoneServiceCredentialRefs(cp)
	probe := unmanagedProjectImport(keystoneProjectProjectProbeRef(order, cluster), cp.Namespace,
		name, adminDomainRef(cp), credRef)
	proceed, verdict, err := managedChildProbeGate(ctx, r.Client, managedChildProbeInput{
		kind:        "Project",
		managed:     &orcv1alpha1.Project{},
		managedName: keystoneProjectProjectRef(order, cluster),
		namespace:   cp.Namespace,
		probe:       probe,
		ensure:      orderEnsure(r.Client, r.Scheme, order, ref),
	})
	if err != nil {
		fail(reasonKeystoneProjectError, fmt.Sprintf("probing for a pre-existing project: %v", err))
		return ctrl.Result{}, err
	}
	if !proceed {
		switch verdict {
		case probeResolved:
			fail(reasonKeystoneProjectCollision, fmt.Sprintf(
				"project %q in domain %q already exists; the order never takes over a project it did not create; "+
					"pick another projectName or delete the order", name, domain))
		case probePending:
			waitOrClassifyCondition(cp, fail, reasonProbingForCollision,
				fmt.Sprintf("probing whether project %q already exists before creating it", name), probe)
		case probeAbsent:
			// Unreachable: an absent probe proceeds.
		}
		return requeue, nil
	}

	project := managedProjectChild(keystoneProjectProjectRef(order, cluster), cp.Namespace,
		name, adminDomainRef(cp), managedCredRef)
	if err := ensureOrderChild(ctx, r.Client, r.Scheme, order, ref, project); err != nil {
		err = fmt.Errorf("ensuring the managed Project %q: %w", project.Name, err)
		fail(reasonKeystoneProjectError, err.Error())
		return ctrl.Result{}, err
	}

	// A latched transport error is handed back to K-ORC first; korc_unlatch.go
	// states the policy.
	if err := unlatchKORCTransportErrors(ctx, r.Client, project); err != nil {
		fail(conditionReasonTransportErrorRetryFailed, err.Error())
		return ctrl.Result{}, err
	}
	if termErr := orcv1alpha1.GetTerminalError(project); termErr != nil {
		fail(reasonKeystoneProjectFailed, fmt.Sprintf("K-ORC reported a terminal error on the project: %v", termErr))
		return requeue, nil
	}
	if !korcAvailableUpToDate(project) {
		waitOrClassifyCondition(cp, fail, reasonKeystoneProjectWaiting,
			"the project is registered but not yet Available", pendingServiceAccountObjs(project)...)
		return requeue, nil
	}

	if project.Status.ID != nil {
		order.Status.ProjectID = *project.Status.ID
	}
	order.Status.ProjectName = name
	order.Status.DomainName = domain
	keystoneProjectSetTrue(order, reasonKeystoneProjectProvisioned,
		fmt.Sprintf("project %q is provisioned in domain %q", name, domain))
	return ctrl.Result{}, nil
}

// keystoneProjectReservedNames are the Keystone projects a ControlPlane creates
// in its admin domain, where every order's project is created as well: the
// admin project and the projects of the built-in service registrations. A
// service's project is reserved whether or not the service is enabled yet.
func keystoneProjectReservedNames(cp *c5c3v1alpha1.ControlPlane) []string {
	return []string{
		adminProjectName(cp),
		c5c3v1alpha1.GlanceServiceProjectName,
		c5c3v1alpha1.PlacementServiceProjectName,
		c5c3v1alpha1.BarbicanServiceProjectName,
		c5c3v1alpha1.NeutronServiceProjectName,
		c5c3v1alpha1.CinderServiceProjectName,
		c5c3v1alpha1.NovaServiceProjectName,
	}
}

// keystoneProjectFail returns a closure bound to order that writes ProjectReady
// False, truncating the message for the reason keystoneUserFail gives.
func keystoneProjectFail(order *c5c3v1alpha1.KeystoneProject) func(reason, message string) {
	return func(reason, message string) {
		setTruncatedCondition(&order.Status.Conditions, order.Generation,
			conditionTypeKeystoneProjectProjectReady, metav1.ConditionFalse, reason, message)
	}
}

// keystoneProjectSetTrue writes ProjectReady True.
func keystoneProjectSetTrue(order *c5c3v1alpha1.KeystoneProject, reason, message string) {
	setTruncatedCondition(&order.Status.Conditions, order.Generation,
		conditionTypeKeystoneProjectProjectReady, metav1.ConditionTrue, reason, message)
}

// updateStatus persists the status through the order's cluster client, as the
// KeystoneUser reconciler's does.
func (r *KeystoneProjectReconciler) updateStatus(
	ctx context.Context, oc client.Client, order *c5c3v1alpha1.KeystoneProject,
	statusBefore *c5c3v1alpha1.KeystoneProjectStatus, result ctrl.Result, reconcileErr error,
) (ctrl.Result, error) {
	return commonreconcile.UpdateStatus(ctx, oc, order, statusBefore, &order.Status, func() {
		commonreconcile.SetAggregateReady(&order.Status.Conditions, order.Generation, keystoneProjectSubConditionTypes)
		order.Status.ObservedGeneration = order.Generation
	}, result, reconcileErr)
}

// --- naming ---

// keystoneProjectRef identifies the order to the shared scaffold: its children
// carry the "project" segment and the keystoneproject labels.
func keystoneProjectRef(order *c5c3v1alpha1.KeystoneProject, cluster string) orderRef {
	return orderRef{
		Name: order.Name, Namespace: order.Namespace, Cluster: cluster,
		Segment: "project", Keys: keystoneProjectLabelKeys,
	}
}

// keystoneProjectProjectRef is the managed K-ORC Project's name, which a
// KeystoneRoleAssignment names as its projectRef.
func keystoneProjectProjectRef(order *c5c3v1alpha1.KeystoneProject, cluster string) string {
	return keystoneProjectRef(order, cluster).childPrefix() + "project"
}

func keystoneProjectProjectProbeRef(order *c5c3v1alpha1.KeystoneProject, cluster string) string {
	return keystoneProjectRef(order, cluster).childPrefix() + "project-probe"
}

// keystoneProjectName resolves the Keystone project name, defaulting to the
// order's own name. No defaulting webhook exists for the kind, so the
// reconciler is the only place the default is resolved.
func keystoneProjectName(order *c5c3v1alpha1.KeystoneProject) string {
	return cmp.Or(order.Spec.ProjectName, order.Name)
}

// --- teardown ---

// reconcileDelete removes the project the order created and releases the
// finalizer: K-ORC's finalizer takes the project out of Keystone, and
// orderTeardown waits for it while the ControlPlane exists.
//
// The teardown holds while a KeystoneRoleAssignment in the order's namespace
// names it as projectRef, for the reason the KeystoneUser teardown gives, and
// while a KeystoneApplicationCredential does, whose credentials are scoped to
// the project.
func (r *KeystoneProjectReconciler) reconcileDelete(
	ctx context.Context, oc client.Client, order *c5c3v1alpha1.KeystoneProject, cluster string,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(order, keystoneProjectFinalizerName) {
		return ctrl.Result{}, nil
	}
	referencing, err := referencingRoleAssignments(ctx, oc, order.Namespace,
		func(ra *c5c3v1alpha1.KeystoneRoleAssignment) bool { return ra.Spec.ProjectRef.Name == order.Name })
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(referencing) > 0 {
		statusBefore := order.Status.DeepCopy()
		keystoneProjectFail(order)(reasonOrderReferencedByRoleAssignments,
			orderReferencedMessage(referencing, order.Namespace, "project"))
		return r.updateStatus(ctx, oc, order, statusBefore,
			ctrl.Result{RequeueAfter: orderReferenceHoldRequeueAfter}, nil)
	}
	credentials, err := referencingApplicationCredentials(ctx, oc, order.Namespace,
		func(ac *c5c3v1alpha1.KeystoneApplicationCredential) bool {
			return ac.Spec.ProjectRef.Name == order.Name
		})
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(credentials) > 0 {
		statusBefore := order.Status.DeepCopy()
		keystoneProjectFail(order)(reasonOrderReferencedByApplicationCredentials,
			orderReferencedByApplicationCredentialsMessage(credentials, order.Namespace, "project",
				"the credentials are scoped to the project"))
		return r.updateStatus(ctx, oc, order, statusBefore,
			ctrl.Result{RequeueAfter: orderReferenceHoldRequeueAfter}, nil)
	}

	ref := keystoneProjectRef(order, cluster)
	managed := keystoneProjectProjectRef(order, cluster)
	return orderTeardown(ctx, r.Client, oc, order, keystoneProjectFinalizerName,
		orderControlPlaneKey(order.Spec.ControlPlaneRef, order.Namespace),
		func(ctx context.Context, childNS string) (int, error) {
			// The managed Project first, then the probe, both of the one kind.
			return sweepOrderList(ctx, r.Client, order, ref, childNS, &orcv1alpha1.ProjectList{},
				func(obj client.Object) bool { return obj.GetName() == managed }, nil)
		})
}

// --- manager setup ---

// KeystoneProjectControlPlaneRefIndexKey is the field-indexer key under which a
// KeystoneProject on the management cluster is indexed by its resolved
// ControlPlane reference, "<namespace>/<name>".
const KeystoneProjectControlPlaneRefIndexKey = "spec.controlPlaneRef"

// keystoneProjectControlPlaneRefExtractor returns the resolved
// "<namespace>/<name>" of obj's ControlPlane reference. An empty name indexes
// nothing.
func keystoneProjectControlPlaneRefExtractor(obj client.Object) []string {
	order, ok := obj.(*c5c3v1alpha1.KeystoneProject)
	if !ok || order.Spec.ControlPlaneRef.Name == "" {
		return nil
	}
	key := orderControlPlaneKey(order.Spec.ControlPlaneRef, order.Namespace)
	return []string{keystoneServiceControlPlaneRefIndexValue(key.Namespace, key.Name)}
}

// controlPlaneToKeystoneProjectsMapper maps a ControlPlane event to the orders
// on the management cluster that reference it. Orders on target clusters come
// back on orderRefreshAfter. A List failure is logged and maps to
// nothing.
func controlPlaneToKeystoneProjectsMapper(c client.Reader) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		var orders c5c3v1alpha1.KeystoneProjectList
		if err := c.List(ctx, &orders, client.MatchingFields{
			KeystoneProjectControlPlaneRefIndexKey: keystoneServiceControlPlaneRefIndexValue(obj.GetNamespace(), obj.GetName()),
		}); err != nil {
			log.FromContext(ctx).Error(err, "listing KeystoneProjects for ControlPlane watch")
			return nil
		}
		requests := make([]reconcile.Request, 0, len(orders.Items))
		for i := range orders.Items {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&orders.Items[i])})
		}
		return requests
	}
}

// keystoneProjectReferenceRequests maps a KeystoneRoleAssignment to the project
// it names as projectRef.
func keystoneProjectReferenceRequests() mchandler.TypedEventHandlerFunc[client.Object, mcreconcile.Request] {
	return roleAssignmentToReferencedOrderRequests(roleAssignmentProjectName)
}

// SetupWithManager registers the KeystoneProjectReconciler with the
// multicluster manager.
func (r *KeystoneProjectReconciler) SetupWithManager(mgr mcmanager.Manager) error {
	return r.setupWithOptions(mgr, bootstrap.TypedControllerOptions[mcreconcile.Request](r.MaxConcurrentReconciles))
}

// setupWithOptions carries the production setup SetupWithManager applies, with
// the controller options as a parameter so the integration suites register this
// chain with SkipNameValidation set.
//
// The watches:
//   - For: the order on the management cluster and on every engaged target
//     cluster that serves the kind.
//   - The Project children in the ControlPlane's namespace, mapped back by their
//     labels.
//   - KeystoneRoleAssignment and KeystoneApplicationCredential: the project
//     one names, on its cluster, so an order leaving wakes a held teardown.
//   - ControlPlane: the orders on the management cluster that reference it, on
//     the updates orderControlPlanePredicate passes.
//
// The index goes on the local field indexer, for the reason the KeystoneUser
// reconciler gives.
func (r *KeystoneProjectReconciler) setupWithOptions(mgr mcmanager.Manager, opts crcontroller.TypedOptions[mcreconcile.Request]) error {
	local := mgr.GetLocalManager()
	if err := local.GetFieldIndexer().IndexField(context.Background(), &c5c3v1alpha1.KeystoneProject{},
		KeystoneProjectControlPlaneRefIndexKey, keystoneProjectControlPlaneRefExtractor); err != nil {
		return err
	}
	engageLocal := commonmulticluster.EngageLocalCluster
	engageNoProviders := commonmulticluster.EngageNoProviderClusters
	engageProviders := mcbuilder.WithEngageWithProviderClusters(true)

	return mcbuilder.ControllerManagedBy(mgr).
		WithOptions(opts).
		For(&c5c3v1alpha1.KeystoneProject{}, mcbuilder.WithPredicates(watch.CRUpdatePredicate()),
			engageLocal, engageProviders,
			mcbuilder.WithClusterFilter(commonmulticluster.ClusterServesKind(keystoneProjectGVK))).
		Watches(&orcv1alpha1.Project{}, orderChildRequests(keystoneProjectLabelKeys), engageLocal, engageNoProviders).
		Watches(&c5c3v1alpha1.KeystoneRoleAssignment{}, keystoneProjectReferenceRequests(),
			mcbuilder.WithPredicates(watch.CRUpdatePredicate()), engageLocal, engageProviders,
			mcbuilder.WithClusterFilter(commonmulticluster.ClusterServesKind(keystoneRoleAssignmentGVK))).
		Watches(&c5c3v1alpha1.KeystoneApplicationCredential{},
			applicationCredentialToReferencedOrderRequests(applicationCredentialProjectName),
			mcbuilder.WithPredicates(watch.CRUpdatePredicate()), engageLocal, engageProviders,
			mcbuilder.WithClusterFilter(commonmulticluster.ClusterServesKind(keystoneApplicationCredentialGVK))).
		Watches(&c5c3v1alpha1.ControlPlane{},
			commonmulticluster.LocalRequests(controlPlaneToKeystoneProjectsMapper(local.GetClient())),
			mcbuilder.WithPredicates(orderControlPlanePredicate()), engageLocal, engageNoProviders).
		// Reconcile answers an unresolvable cluster itself, so the wrapper that
		// would turn a cluster-not-found error into a success stays off.
		WithClusterNotFoundWrapper(false).
		Complete(r)
}
