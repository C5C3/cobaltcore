// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"cmp"
	"context"
	"fmt"
	"strings"

	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	"github.com/c5c3/cobaltcore/internal/common/bootstrap"
	"github.com/c5c3/cobaltcore/internal/common/conditions"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	"github.com/c5c3/cobaltcore/internal/common/watch"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// conditionTypeKeystoneCatalogEntryCatalogReady is the one sub-condition a
// KeystoneCatalogEntry carries; the aggregate Ready is derived from it.
const conditionTypeKeystoneCatalogEntryCatalogReady = "CatalogReady"

// keystoneCatalogEntrySubConditionTypes are the sub-conditions the aggregate
// Ready is derived from.
var keystoneCatalogEntrySubConditionTypes = []string{conditionTypeKeystoneCatalogEntryCatalogReady}

// keystoneCatalogEntryFinalizerName gates the teardown of the catalog rows an
// order created.
const keystoneCatalogEntryFinalizerName = "c5c3.io/keystonecatalogentry-teardown"

// keystoneCatalogEntryLabelKeys are the ownership labels an order's children
// carry, for the reason keystoneUserLabelKeys gives.
var keystoneCatalogEntryLabelKeys = orderLabelKeys{
	Name:      "c5c3.io/keystonecatalogentry-name",
	Namespace: "c5c3.io/keystonecatalogentry-namespace",
	Cluster:   "c5c3.io/keystonecatalogentry-cluster",
}

// keystoneCatalogEntryGVK is the kind the order watch filters target clusters
// on.
var keystoneCatalogEntryGVK = c5c3v1alpha1.GroupVersion.WithKind("KeystoneCatalogEntry")

// reasonKeystoneCatalogEntryNotAllowed reports an order from an assigned
// namespace whose entry does not set allowCatalogEntries. The order is frozen
// while it holds. The catalog outcomes reuse the KeystoneService and
// ControlPlane vocabulary (reasonKeystoneServiceCatalogRegistered,
// reasonKeystoneServiceCatalogCollision, reasonKeystoneServiceCatalogError,
// conditionReasonCatalogFailed, conditionReasonWaitingForCatalog).
const reasonKeystoneCatalogEntryNotAllowed = "CatalogNotAllowed"

// KeystoneCatalogEntryReconciler owns the KeystoneCatalogEntry lifecycle and is
// the single writer of its status. It serves orders on the management cluster
// and on target clusters the way the KeystoneUser reconciler does. The K-ORC
// Service, Region import and Endpoints live in the ControlPlane's namespace on
// the management cluster. It records no Events.
type KeystoneCatalogEntryReconciler struct {
	// Client is the management cluster's client.
	client.Client
	Scheme *runtime.Scheme
	// Resolver resolves the cluster an order lives on; nil resolves every
	// request to the management cluster.
	Resolver                commonmulticluster.ClusterResolver
	MaxConcurrentReconciles int
}

// RBAC for the KeystoneCatalogEntry kind: the controller reads the orders and
// updates them to install and release its finalizer; it never creates or
// deletes one. The K-ORC Services, Endpoints and Regions it writes are granted
// by the ControlPlane's marker block.
// +kubebuilder:rbac:groups=c5c3.io,resources=keystonecatalogentries,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=c5c3.io,resources=keystonecatalogentries/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=c5c3.io,resources=keystonecatalogentries/finalizers,verbs=update

// Reconcile drives one KeystoneCatalogEntry: the gates, finalizer installation,
// the registration and the teardown.
func (r *KeystoneCatalogEntryReconciler) Reconcile(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
	cluster := string(req.ClusterName)
	oc, err := orderClient(ctx, r.Resolver, r.Client, cluster)
	if err != nil {
		// The cluster was deregistered between the event and this pass. Nothing
		// can be read or written there, and a requeue would only repeat this.
		log.FromContext(ctx).Info("the cluster the KeystoneCatalogEntry lives on does not resolve; skipping it",
			"cluster", cluster, "order", req.NamespacedName, "reason", err.Error())
		return ctrl.Result{}, nil
	}

	var order c5c3v1alpha1.KeystoneCatalogEntry
	if err := oc.Get(ctx, req.NamespacedName, &order); err != nil {
		if apierrors.IsNotFound(err) {
			log.FromContext(ctx).V(1).Info("KeystoneCatalogEntry not found; likely deleted")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching KeystoneCatalogEntry: %w", err)
	}

	if order.DeletionTimestamp != nil {
		return r.reconcileDelete(ctx, oc, &order, cluster)
	}

	statusBefore := order.Status.DeepCopy()
	result, err := r.reconcileNormal(ctx, oc, &order, cluster)
	return r.updateStatus(ctx, oc, &order, statusBefore, result, err)
}

// reconcileNormal runs the shared gates and the catalog consent, installs the
// finalizer past them, and registers the entry.
//
// The catalog consent gates before the finalizer: an order its entry does not
// admit has registered nothing, so it has nothing to tear down. Clearing the
// flag later freezes the order the way a withdrawn entry does (#1327 D2).
func (r *KeystoneCatalogEntryReconciler) reconcileNormal(
	ctx context.Context, oc client.Client, order *c5c3v1alpha1.KeystoneCatalogEntry, cluster string,
) (ctrl.Result, error) {
	fail := keystoneCatalogEntryFail(order)
	ref := keystoneCatalogEntryRef(order, cluster)
	cp, entry, result, err := orderAdmission(ctx, r.Client, ref, order.Spec.ControlPlaneRef, fail)
	if err != nil || cp == nil {
		return result, err
	}

	if !entry.AllowCatalogEntries {
		fail(reasonKeystoneCatalogEntryNotAllowed, fmt.Sprintf(
			"ControlPlane %s/%s assigns namespace %q on %s without allowCatalogEntries (spec.namespaceAssignments); "+
				"the order is frozen: nothing is registered, repaired or swept, and what was created stays",
			cp.Namespace, cp.Name, order.Namespace, ref.location()))
		return ctrl.Result{RequeueAfter: namespaceAssignmentRequeueAfter}, nil
	}

	if added, err := commonreconcile.EnsureFinalizer(ctx, oc, order, keystoneCatalogEntryFinalizerName); err != nil {
		return ctrl.Result{}, err
	} else if added {
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueNextPass}, nil
	}

	if !orderAdminCredentialGate(cp, fail) {
		return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
	}

	result, err = instrumenter.Instrument(ctx, "KeystoneCatalogEntryProvision", func(ctx context.Context) (ctrl.Result, error) {
		return r.registerCatalogEntry(ctx, order, cp, cluster)
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	return orderPassResult(cluster,
		conditions.AllTrue(order.Status.Conditions, conditionTypeKeystoneCatalogEntryCatalogReady), result), nil
}

// registerCatalogEntry projects the order's catalog rows the way a
// KeystoneService's catalog block is projected (ensureCatalog): one managed
// K-ORC Service for the row, an unmanaged import of the ControlPlane's Keystone
// region, and one managed Endpoint per declared interface, registered in that
// region. It writes CatalogReady.
//
// A collision probe decides first, without adopt: K-ORC's service actuator
// matches an existing row on type and name, so a managed create would take over
// a row the order did not create, and deleting the order would delete it. The
// probe runs only while no managed Service exists, which is why the type and
// the name are frozen. An Endpoint the spec no longer declares is deleted on
// every pass, before the declared rows' outcomes are read, so a dropped row
// never waits on them; its removal is reported once the declared rows are
// Available.
func (r *KeystoneCatalogEntryReconciler) registerCatalogEntry(
	ctx context.Context, order *c5c3v1alpha1.KeystoneCatalogEntry, cp *c5c3v1alpha1.ControlPlane, cluster string,
) (ctrl.Result, error) {
	fail := keystoneCatalogEntryFail(order)
	requeue := ctrl.Result{RequeueAfter: korcRequeueAfter}
	serviceType := order.Spec.ServiceType
	serviceName := keystoneCatalogEntryName(order)
	ref := keystoneCatalogEntryRef(order, cluster)
	ensure := orderEnsure(r.Client, r.Scheme, order, ref)
	credRef, managedCredRef := keystoneServiceCredentialRefs(cp)
	order.Status.Endpoints = nil

	probe := unmanagedServiceImport(keystoneCatalogEntryServiceProbeRef(order, cluster), cp.Namespace,
		serviceType, serviceName, credRef)
	proceed, verdict, err := managedChildProbeGate(ctx, r.Client, managedChildProbeInput{
		kind:             "Service",
		managed:          &orcv1alpha1.Service{},
		managedName:      keystoneCatalogEntryServiceRef(order, cluster),
		namespace:        cp.Namespace,
		probe:            probe,
		dropProbeOnOwned: true,
		ensure:           ensure,
	})
	if err != nil {
		fail(reasonKeystoneServiceCatalogError, fmt.Sprintf("probing for a pre-existing catalog entry: %v", err))
		return ctrl.Result{}, err
	}
	if !proceed {
		switch verdict {
		case probeResolved:
			fail(reasonKeystoneServiceCatalogCollision, fmt.Sprintf(
				"a catalog entry of type %q named %q already exists in Keystone; the order never takes over a row "+
					"it did not create; change serviceName or delete the order", serviceType, serviceName))
		case probePending:
			waitOrClassifyCondition(cp, fail, reasonProbingForCollision, fmt.Sprintf(
				"probing whether a catalog entry of type %q named %q already exists in Keystone",
				serviceType, serviceName), probe)
		case probeAbsent:
			// Unreachable: an absent probe proceeds.
		}
		return requeue, nil
	}

	// The managed children authenticate through the admin PASSWORD cloud, as a
	// KeystoneService's catalog children do.
	service := managedCatalogServiceChild(keystoneCatalogEntryServiceRef(order, cluster), cp.Namespace,
		serviceType, serviceName, managedCredRef)
	if err := ensure(ctx, service); err != nil {
		fail(reasonKeystoneServiceCatalogError, fmt.Sprintf("applying the catalog Service: %v", err))
		return ctrl.Result{}, err
	}
	if service.Status.ID != nil {
		order.Status.ServiceID = *service.Status.ID
	}

	region := unmanagedRegionImport(keystoneCatalogEntryRegionRef(order, cluster), cp.Namespace, korcRegion(cp), credRef)
	if err := ensure(ctx, region); err != nil {
		fail(reasonKeystoneServiceCatalogError, fmt.Sprintf("applying the catalog Region import: %v", err))
		return ctrl.Result{}, err
	}

	declared := make(map[string]bool, len(order.Spec.Endpoints))
	endpoints := make([]*orcv1alpha1.Endpoint, 0, len(order.Spec.Endpoints))
	for _, ep := range order.Spec.Endpoints {
		endpoint := managedCatalogEndpointChild(keystoneCatalogEntryEndpointRef(order, cluster, ep.Interface),
			cp.Namespace, string(ep.Interface), ep.URL, service.Name, region.Name, managedCredRef)
		if err := ensure(ctx, endpoint); err != nil {
			fail(reasonKeystoneServiceCatalogError, fmt.Sprintf("applying the %q catalog Endpoint: %v", ep.Interface, err))
			return ctrl.Result{}, err
		}
		declared[endpoint.Name] = true
		endpoints = append(endpoints, endpoint)
		row := c5c3v1alpha1.KeystoneServiceEndpointStatus{Interface: ep.Interface}
		if endpoint.Status.ID != nil {
			row.ID = *endpoint.Status.ID
		}
		order.Status.Endpoints = append(order.Status.Endpoints, row)
	}

	removing, err := r.sweepUndeclaredEndpoints(ctx, order, ref, cp.Namespace, declared)
	if err != nil {
		fail(reasonKeystoneServiceCatalogError, fmt.Sprintf("removing undeclared catalog Endpoints: %v", err))
		return ctrl.Result{}, err
	}

	// A latched transport error is handed back to K-ORC first; korc_unlatch.go
	// states the policy.
	objs := []orcv1alpha1.ObjectWithConditions{service, region}
	for _, endpoint := range endpoints {
		objs = append(objs, endpoint)
	}
	if err := unlatchKORCTransportErrors(ctx, r.Client, objs...); err != nil {
		fail(conditionReasonTransportErrorRetryFailed, err.Error())
		return ctrl.Result{}, err
	}

	// The Service's and the Region's errors and waits are reported before the
	// Endpoints', so the root stuck dependency surfaces.
	if termErr := orcv1alpha1.GetTerminalError(service); termErr != nil {
		fail(conditionReasonCatalogFailed, fmt.Sprintf(
			"K-ORC reported a terminal error registering the catalog Service %q: %v", service.Name, termErr))
		return requeue, nil
	}
	if termErr := orcv1alpha1.GetTerminalError(region); termErr != nil {
		fail(conditionReasonCatalogFailed, fmt.Sprintf(
			"K-ORC reported a terminal error importing the catalog Region %q: %v", region.Name, termErr))
		return requeue, nil
	}
	for _, endpoint := range endpoints {
		if termErr := orcv1alpha1.GetTerminalError(endpoint); termErr != nil {
			fail(conditionReasonCatalogFailed, fmt.Sprintf(
				"K-ORC reported a terminal error registering the catalog Endpoint %q: %v", endpoint.Name, termErr))
			return requeue, nil
		}
	}
	if !korcAvailableUpToDate(service) {
		waitOrClassifyCondition(cp, fail, conditionReasonWaitingForCatalog,
			fmt.Sprintf("the catalog Service %q is registered but not yet Available", service.Name), service)
		return requeue, nil
	}
	if !korcAvailableUpToDate(region) {
		waitOrClassifyCondition(cp, fail, conditionReasonWaitingForCatalog, fmt.Sprintf(
			"the Keystone region %q the catalog endpoints are registered in is not resolved yet (Region %q)",
			korcRegion(cp), region.Name), region)
		return requeue, nil
	}
	for _, endpoint := range endpoints {
		if !korcAvailableUpToDate(endpoint) {
			waitOrClassifyCondition(cp, fail, conditionReasonWaitingForCatalog,
				fmt.Sprintf("the catalog Endpoint %q is registered but not yet Available", endpoint.Name), endpoint)
			return requeue, nil
		}
	}

	if len(removing) > 0 {
		fail(conditionReasonWaitingForCatalog, fmt.Sprintf(
			"the catalog Endpoint(s) %s the spec no longer declares are being removed", strings.Join(removing, ", ")))
		return requeue, nil
	}

	order.Status.ServiceName = serviceName
	keystoneCatalogEntrySetTrue(order, reasonKeystoneServiceCatalogRegistered, fmt.Sprintf(
		"catalog entry %q of type %q is registered with %d endpoint(s) in region %q",
		serviceName, serviceType, len(order.Spec.Endpoints), korcRegion(cp)))
	return ctrl.Result{}, nil
}

// sweepUndeclaredEndpoints deletes every Endpoint in namespace the order owns
// whose name declared does not hold, and returns the names still listed.
// Deleting the CR is what removes the Keystone row, through K-ORC's finalizer,
// so a name stays listed until that finalizer is gone.
func (r *KeystoneCatalogEntryReconciler) sweepUndeclaredEndpoints(
	ctx context.Context, order *c5c3v1alpha1.KeystoneCatalogEntry, ref orderRef, namespace string, declared map[string]bool,
) ([]string, error) {
	var listed orcv1alpha1.EndpointList
	if err := r.List(ctx, &listed, client.InNamespace(namespace), client.MatchingLabels(ref.childLabels())); err != nil {
		return nil, fmt.Errorf("listing order Endpoints: %w", err)
	}
	var removing []string
	for i := range listed.Items {
		endpoint := &listed.Items[i]
		if declared[endpoint.Name] || !ownsOrderChild(endpoint, order, ref) {
			continue
		}
		if endpoint.DeletionTimestamp == nil {
			if err := client.IgnoreNotFound(r.Delete(ctx, endpoint)); err != nil {
				return nil, fmt.Errorf("deleting Endpoint %q: %w", endpoint.Name, err)
			}
		}
		removing = append(removing, endpoint.Name)
	}
	return removing, nil
}

// keystoneCatalogEntryFail returns a closure bound to order that writes
// CatalogReady False, truncating the message for the reason keystoneUserFail
// gives.
func keystoneCatalogEntryFail(order *c5c3v1alpha1.KeystoneCatalogEntry) func(reason, message string) {
	return func(reason, message string) {
		setTruncatedCondition(&order.Status.Conditions, order.Generation,
			conditionTypeKeystoneCatalogEntryCatalogReady, metav1.ConditionFalse, reason, message)
	}
}

// keystoneCatalogEntrySetTrue writes CatalogReady True.
func keystoneCatalogEntrySetTrue(order *c5c3v1alpha1.KeystoneCatalogEntry, reason, message string) {
	setTruncatedCondition(&order.Status.Conditions, order.Generation,
		conditionTypeKeystoneCatalogEntryCatalogReady, metav1.ConditionTrue, reason, message)
}

// updateStatus persists the status through the order's cluster client, as the
// KeystoneUser reconciler's does.
func (r *KeystoneCatalogEntryReconciler) updateStatus(
	ctx context.Context, oc client.Client, order *c5c3v1alpha1.KeystoneCatalogEntry,
	statusBefore *c5c3v1alpha1.KeystoneCatalogEntryStatus, result ctrl.Result, reconcileErr error,
) (ctrl.Result, error) {
	return commonreconcile.UpdateStatus(ctx, oc, order, statusBefore, &order.Status, func() {
		commonreconcile.SetAggregateReady(&order.Status.Conditions, order.Generation, keystoneCatalogEntrySubConditionTypes)
		order.Status.ObservedGeneration = order.Generation
	}, result, reconcileErr)
}

// --- naming ---

// keystoneCatalogEntryRef identifies the order to the shared scaffold: its
// children carry the "catalogentry" segment and the keystonecatalogentry labels.
func keystoneCatalogEntryRef(order *c5c3v1alpha1.KeystoneCatalogEntry, cluster string) orderRef {
	return orderRef{
		Name: order.Name, Namespace: order.Namespace, Cluster: cluster,
		Segment: "catalogentry", Keys: keystoneCatalogEntryLabelKeys,
	}
}

func keystoneCatalogEntryServiceRef(order *c5c3v1alpha1.KeystoneCatalogEntry, cluster string) string {
	return keystoneCatalogEntryRef(order, cluster).childPrefix() + "service"
}

func keystoneCatalogEntryServiceProbeRef(order *c5c3v1alpha1.KeystoneCatalogEntry, cluster string) string {
	return keystoneCatalogEntryRef(order, cluster).childPrefix() + "service-probe"
}

func keystoneCatalogEntryRegionRef(order *c5c3v1alpha1.KeystoneCatalogEntry, cluster string) string {
	return keystoneCatalogEntryRef(order, cluster).childPrefix() + "region"
}

func keystoneCatalogEntryEndpointRef(
	order *c5c3v1alpha1.KeystoneCatalogEntry, cluster string, iface c5c3v1alpha1.ExternalEndpointType,
) string {
	return keystoneCatalogEntryRef(order, cluster).childPrefix() + "endpoint-" + string(iface)
}

// keystoneCatalogEntryName resolves the catalog service name, defaulting to the
// order's own name. No defaulting webhook exists for the kind, so the
// reconciler is the only place the default is resolved.
func keystoneCatalogEntryName(order *c5c3v1alpha1.KeystoneCatalogEntry) string {
	return cmp.Or(order.Spec.ServiceName, order.Name)
}

// --- teardown ---

// reconcileDelete removes the catalog rows the order created and releases the
// finalizer: the Endpoints first, then the managed Service and the probe, then
// the Region import, which K-ORC holds while an Endpoint names it.
func (r *KeystoneCatalogEntryReconciler) reconcileDelete(
	ctx context.Context, oc client.Client, order *c5c3v1alpha1.KeystoneCatalogEntry, cluster string,
) (ctrl.Result, error) {
	ref := keystoneCatalogEntryRef(order, cluster)
	managed := keystoneCatalogEntryServiceRef(order, cluster)
	return orderTeardown(ctx, r.Client, oc, order, keystoneCatalogEntryFinalizerName,
		orderControlPlaneKey(order.Spec.ControlPlaneRef, order.Namespace),
		func(ctx context.Context, childNS string) (int, error) {
			return sweepOrderLists(ctx, r.Client, order, ref, childNS,
				orderSweep{list: &orcv1alpha1.EndpointList{}},
				orderSweep{list: &orcv1alpha1.ServiceList{}, first: func(obj client.Object) bool { return obj.GetName() == managed }},
				orderSweep{list: &orcv1alpha1.RegionList{}})
		})
}

// --- manager setup ---

// KeystoneCatalogEntryControlPlaneRefIndexKey is the field-indexer key under
// which a KeystoneCatalogEntry on the management cluster is indexed by its
// resolved ControlPlane reference, "<namespace>/<name>".
const KeystoneCatalogEntryControlPlaneRefIndexKey = "spec.controlPlaneRef"

// keystoneCatalogEntryControlPlaneRefExtractor returns the resolved
// "<namespace>/<name>" of obj's ControlPlane reference. An empty name indexes
// nothing.
func keystoneCatalogEntryControlPlaneRefExtractor(obj client.Object) []string {
	order, ok := obj.(*c5c3v1alpha1.KeystoneCatalogEntry)
	if !ok || order.Spec.ControlPlaneRef.Name == "" {
		return nil
	}
	key := orderControlPlaneKey(order.Spec.ControlPlaneRef, order.Namespace)
	return []string{keystoneServiceControlPlaneRefIndexValue(key.Namespace, key.Name)}
}

// controlPlaneToKeystoneCatalogEntriesMapper maps a ControlPlane event to the
// orders on the management cluster that reference it. A List failure is logged
// and maps to nothing.
func controlPlaneToKeystoneCatalogEntriesMapper(c client.Reader) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		var orders c5c3v1alpha1.KeystoneCatalogEntryList
		if err := c.List(ctx, &orders, client.MatchingFields{
			KeystoneCatalogEntryControlPlaneRefIndexKey: keystoneServiceControlPlaneRefIndexValue(obj.GetNamespace(), obj.GetName()),
		}); err != nil {
			log.FromContext(ctx).Error(err, "listing KeystoneCatalogEntries for ControlPlane watch")
			return nil
		}
		requests := make([]reconcile.Request, 0, len(orders.Items))
		for i := range orders.Items {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&orders.Items[i])})
		}
		return requests
	}
}

// SetupWithManager registers the KeystoneCatalogEntryReconciler with the
// multicluster manager.
func (r *KeystoneCatalogEntryReconciler) SetupWithManager(mgr mcmanager.Manager) error {
	return r.setupWithOptions(mgr, bootstrap.TypedControllerOptions[mcreconcile.Request](r.MaxConcurrentReconciles))
}

// setupWithOptions carries the production setup SetupWithManager applies, with
// the controller options as a parameter so the integration suites register this
// chain with SkipNameValidation set.
//
// The watches:
//   - For: the order on the management cluster and on every engaged target
//     cluster that serves the kind.
//   - The Service, Endpoint and Region children in the ControlPlane's
//     namespace, mapped back by their labels.
//   - ControlPlane: the orders on the management cluster that reference it, on
//     the updates orderControlPlanePredicate passes.
//
// The index goes on the local field indexer, for the reason the KeystoneUser
// reconciler gives.
func (r *KeystoneCatalogEntryReconciler) setupWithOptions(mgr mcmanager.Manager, opts crcontroller.TypedOptions[mcreconcile.Request]) error {
	local := mgr.GetLocalManager()
	if err := local.GetFieldIndexer().IndexField(context.Background(), &c5c3v1alpha1.KeystoneCatalogEntry{},
		KeystoneCatalogEntryControlPlaneRefIndexKey, keystoneCatalogEntryControlPlaneRefExtractor); err != nil {
		return err
	}
	engageLocal := commonmulticluster.EngageLocalCluster
	engageNoProviders := commonmulticluster.EngageNoProviderClusters
	engageProviders := mcbuilder.WithEngageWithProviderClusters(true)
	children := orderChildRequests(keystoneCatalogEntryLabelKeys)

	return mcbuilder.ControllerManagedBy(mgr).
		WithOptions(opts).
		For(&c5c3v1alpha1.KeystoneCatalogEntry{}, mcbuilder.WithPredicates(watch.CRUpdatePredicate()),
			engageLocal, engageProviders,
			mcbuilder.WithClusterFilter(commonmulticluster.ClusterServesKind(keystoneCatalogEntryGVK))).
		Watches(&orcv1alpha1.Service{}, children, engageLocal, engageNoProviders).
		Watches(&orcv1alpha1.Endpoint{}, children, engageLocal, engageNoProviders).
		Watches(&orcv1alpha1.Region{}, children, engageLocal, engageNoProviders).
		Watches(&c5c3v1alpha1.ControlPlane{},
			commonmulticluster.LocalRequests(controlPlaneToKeystoneCatalogEntriesMapper(local.GetClient())),
			mcbuilder.WithPredicates(orderControlPlanePredicate()), engageLocal, engageNoProviders).
		// Reconcile answers an unresolvable cluster itself, so the wrapper that
		// would turn a cluster-not-found error into a success stays off.
		WithClusterNotFoundWrapper(false).
		Complete(r)
}
