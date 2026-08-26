// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	"github.com/c5c3/cobaltcore/internal/common/bootstrap"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	"github.com/c5c3/cobaltcore/internal/common/watch"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// chassisAppName is the app.kubernetes.io/name label value carried by every
// child of an OVNChassis. It is the CR kind in lower case, which is what keeps
// the children of a chassis distinguishable from the control-plane children an
// OVNCentral of the same name projects into the same namespace.
const chassisAppName = "ovnchassis"

// chassisSubConditionTypes lists the condition types set by the individual
// OVNChassis sub-reconcilers. The aggregate Ready condition is True only when
// all of them are True, so every sub-reconciler has to set its condition on
// every path it takes, including the ones where it creates nothing.
//
// The order is the pipeline's: the OVNCentral the chassis attach to comes
// first, because its Southbound address and client Secret are what every later
// step is parameterised by, the per-node values next, then the two DaemonSets
// that mount them, and the maintenance Jobs last, since they act on nodes the
// node step has already marked as leaving or as evacuating.
var chassisSubConditionTypes = []string{
	conditionTypeCentralReady,
	conditionTypeNodesReady,
	conditionTypeOVSReady,
	conditionTypeControllerReady,
	conditionTypeMaintenanceReady,
}

// chassisSkeleton bundles the shared controller-skeleton glue (Ready
// aggregation, no-op-skipping status writes, config-failure marking) with the
// OVNChassis sub-condition vocabulary and status accessor.
var chassisSkeleton = commonreconcile.Skeleton[*ovnv1alpha1.OVNChassis, ovnv1alpha1.OVNChassisStatus]{
	SubConditionTypes: chassisSubConditionTypes,
	Conditions:        func(c *ovnv1alpha1.OVNChassis) *[]metav1.Condition { return &c.Status.Conditions },
}

// OVNChassisRemoteChildKinds are the kinds an OVNChassis CR projects into the
// namespace of the target cluster it names, and the kinds the deletion sweep
// selects by ownership label when that CR is deleted. Nothing on the target
// cluster collects them, so a kind missing from this list is a kind that keeps
// running after its CR is gone.
//
// The list is short because a chassis owns no state of its own: the two
// DaemonSets, the two ConfigMaps that carry the per-node values and the scripts
// the pods run, and the maintenance Jobs that evacuate a gateway node and
// deregister a leaving chassis.
var OVNChassisRemoteChildKinds = []schema.GroupVersionKind{
	appsv1.SchemeGroupVersion.WithKind("DaemonSet"),
	corev1.SchemeGroupVersion.WithKind("ConfigMap"),
	batchv1.SchemeGroupVersion.WithKind("Job"),
}

// OVNChassisReconciler reconciles an OVNChassis object. Its fields mirror the
// OVNCentral reconciler's core set, minus the cert-manager probe: a chassis
// requests no certificate of its own, it mounts the client Secret the
// OVNCentral it names publishes.
type OVNChassisReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// MaxConcurrentReconciles bounds how many OVNChassis CRs reconcile
	// concurrently. It is threaded from the --max-concurrent-reconciles flag and
	// applied to the controller's controller.Options in SetupWithManager. A value
	// <= 0 falls back to bootstrap.DefaultMaxConcurrentReconciles inside
	// bootstrap.ControllerOptions, so the zero value is safe.
	MaxConcurrentReconciles int

	// Resolver resolves the target cluster an OVNChassis CR names in
	// spec.targetClusterRef into the client its children are read and written
	// with. Nil means always-local: every CR keeps its children on the
	// management cluster, which is what single-cluster tests and deployments
	// want.
	Resolver commonmulticluster.ClusterResolver
}

// The markers below repeat the set the OVNCentral reconciler carries, verbatim.
// controller-gen collects them per operator and generates one ClusterRole from
// their union, so either block on its own already describes what the binary
// needs, and the two are kept identical rather than split by controller: a
// reader of this file sees the permissions its own children require without
// opening the sibling controller, and a marker trimmed on one side is a
// difference between two otherwise equal blocks.
//
// The CR kinds carry no create or delete verb: an OVNCentral and an OVNChassis
// are written by whoever deploys the control plane, and the operator only reads
// them, stamps their status and manages their finalizers. The projected child
// kinds carry the full set. The read-only core kinds are inputs: nodes for the
// selection this controller renders and for the addresses the OVNCentral
// endpoint step publishes, pods for the same, and secrets for the certificate
// material cert-manager writes.

// +kubebuilder:rbac:groups=ovn.openstack.c5c3.io,resources=ovncentrals;ovnchassis,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=ovn.openstack.c5c3.io,resources=ovncentrals/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=ovn.openstack.c5c3.io,resources=ovnchassis/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=ovn.openstack.c5c3.io,resources=ovncentrals/finalizers,verbs=update
// +kubebuilder:rbac:groups=ovn.openstack.c5c3.io,resources=ovnchassis/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=nodes;pods;secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=services;configmaps;persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=apps,resources=statefulsets;deployments;daemonsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs;cronjobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cert-manager.io,resources=certificates,verbs=get;list;watch;create;update;patch;delete

// Reconcile is the main reconciliation loop for the OVNChassis CR. It fetches
// the CR, drives the teardown of a terminating one, ensures the remote-children
// finalizer, then runs the sub-reconciler pipeline. Every exit funnels through
// updateStatus, which re-aggregates the Ready condition and stamps
// ObservedGeneration.
func (r *OVNChassisReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var cr ovnv1alpha1.OVNChassis
	if err := r.Get(ctx, req.NamespacedName, &cr); err != nil {
		if apierrors.IsNotFound(err) {
			log.FromContext(ctx).V(1).Info("OVNChassis resource not found; likely deleted")
			// Nothing to drop here, unlike the OVNCentral path: the operator's
			// per-CR collectors count backup runs, and only an OVNCentral takes
			// backups.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching OVNChassis: %w", err)
	}

	// Handle deletion: sweep what this CR projected onto the target cluster it
	// names and release the finalizer that held it open for the sweep. The
	// cleanup itself sets no conditions, so it returns directly without
	// updateStatus; only the hold on an unresolvable target below reports.
	//
	// It comes before the target-cluster resolution below and uses the deletion
	// variant, which never fails the pass: a CR whose cluster was deregistered
	// after the finalizer went on would otherwise short-circuit on the
	// unresolvable ref on every pass and stay Terminating forever. A target that
	// has not resolved yet requeues instead of being given up on: engagement is
	// asynchronous, so an operator restart looks exactly like a deregistration
	// until the provider has synced.
	if !cr.DeletionTimestamp.IsZero() {
		children, wait := commonmulticluster.ResolveChildrenClientForDeletion(
			ctx, r.Resolver, r.Client, cr.Spec.TargetClusterRef, *cr.DeletionTimestamp)
		if wait {
			// The hold goes on the CR, not only into the operator's log. It is a
			// deliberate state a CR can sit in for minutes, and "Terminating,
			// waiting on the target cluster" has to be distinguishable from a
			// wedged finalizer without correlating logs across replicas. This exit
			// precedes the pipeline's status snapshot below, so it takes its own
			// baseline for the skip-unchanged write.
			statusBefore := cr.Status.DeepCopy()
			chassisSkeleton.MarkFailed(&cr, conditionTypeCentralReady,
				commonmulticluster.TargetClusterUnavailable,
				fmt.Errorf("target cluster %s does not resolve; waiting at least %s before abandoning its children",
					cr.Spec.TargetClusterRef.Name, commonmulticluster.AbandonAfter))
			return r.updateStatus(ctx, &cr, statusBefore,
				ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, nil)
		}
		if err := r.reconcileDeleteRemoteChildren(ctx, children, &cr); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Resolve the client every child object of this CR is read and written with.
	// The embedded client stays on the management cluster (the CR, its status and
	// its finalizer live there); children carries everything the CR projects into
	// the target cluster, and it is also what the node list is read through,
	// because the nodes a chassis configures are that cluster's. The resolution
	// runs before the finalizer is added so a CR naming an unresolvable cluster
	// stays clean of finalizers: nothing was created for it, so there is nothing
	// to clean up, and a finalizer would only block its deletion.
	children, err := commonmulticluster.ResolveChildrenClient(ctx, r.Resolver, r.Client, cr.Spec.TargetClusterRef)
	if err != nil {
		// This exit precedes the pipeline's status snapshot below, so it takes its
		// own baseline for the skip-unchanged write. CentralReady is the
		// pipeline's first gate, so the failure lands on the condition the rest of
		// the graph waits behind.
		statusBefore := cr.Status.DeepCopy()
		chassisSkeleton.MarkFailed(&cr, conditionTypeCentralReady, commonmulticluster.TargetClusterUnavailable, err)
		return r.updateStatus(ctx, &cr, statusBefore,
			ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, nil)
	}

	// The remote-children finalizer goes on only when the CR projects onto a
	// target cluster, and it is the only finalizer this operator installs. A CR
	// that keeps its children on the management cluster keeps the garbage
	// collection cascade, which reaps them from their owner references, so it has
	// nothing for a finalizer to hold it open for. spec.targetClusterRef is
	// immutable, so the condition cannot flip under a live CR.
	//
	// It is installed before any sub-reconciler runs, so a deletion issued
	// between this pass and the next still funnels through the sweep. Requeuing
	// after the Update guarantees the next reconcile observes the persisted
	// finalizer rather than the in-memory copy.
	if cr.Spec.TargetClusterRef != nil {
		if added, err := commonreconcile.EnsureFinalizer(ctx, r.Client, &cr,
			commonmulticluster.RemoteChildrenFinalizer); err != nil {
			return ctrl.Result{}, err
		} else if added {
			return ctrl.Result{RequeueAfter: commonreconcile.RequeueNextPass}, nil
		}
	}

	// Snapshot the persisted status so updateStatus can skip the write when a
	// pass leaves status unchanged (no write → no watch event → no
	// resourceVersion churn). Taken after the finalizer add so an early requeue
	// there does not race a status write.
	//
	// The maintenance step reads the same snapshot for a second reason: it is
	// the only record of what each node was last known to run, and the node step
	// overwrites status.nodes before maintenance runs.
	statusBefore := cr.Status.DeepCopy()

	result, err := commonreconcile.RunPipeline(ctx, instrumenter.Instrument,
		r.pipelineSteps(children, &cr, statusBefore))
	return r.updateStatus(ctx, &cr, statusBefore, result, err)
}

// pipelineSteps returns the ordered sub-reconciler pipeline for one OVNChassis.
// Each step runs in dependency order; the first to return a non-zero result or
// an error short-circuits the chain and funnels through updateStatus.
//
// The OVNCentral is the first gate: its Southbound address and its client
// Secret parameterise every later step, so a chassis whose central has not
// published them yet projects nothing at all. The per-node values come next,
// because both DaemonSets mount the ConfigMap holding them, and the maintenance
// Jobs last, since they act on nodes the node step has already marked as
// leaving or as giving up the gateway role.
//
// There is no parallel group. The two DaemonSets look independent, but Open
// vSwitch owns the local database ovn-controller writes its chassis record
// into, so a node that runs the second without the first has nothing to
// register against.
//
// central and nodes are threaded from the steps that resolve them to the steps
// that consume them. A closure keeps that hand-off inside the pipeline instead
// of on the reconciler, where it would be state shared by every CR the
// controller reconciles concurrently.
//
// It is a method rather than a literal inside Reconcile so the drift guard can
// enumerate the step names without running a reconcile.
func (r *OVNChassisReconciler) pipelineSteps(children client.Client, cr *ovnv1alpha1.OVNChassis,
	statusBefore *ovnv1alpha1.OVNChassisStatus,
) []commonreconcile.Step {
	var (
		central resolvedCentral
		nodes   renderedNodes
	)

	return []commonreconcile.Step{
		{Name: "Central", Fn: func(ctx context.Context) (ctrl.Result, error) {
			var res ctrl.Result
			var err error
			central, res, err = r.reconcileCentral(ctx, cr)
			return res, err
		}},
		{Name: "Nodes", Fn: func(ctx context.Context) (ctrl.Result, error) {
			var res ctrl.Result
			var err error
			nodes, res, err = r.reconcileNodes(ctx, children, cr)
			return res, err
		}},
		{Name: "OVS", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileOVS(ctx, children, cr)
		}},
		{Name: "Controller", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileController(ctx, children, cr, central)
		}},
		{Name: "Maintenance", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileMaintenance(ctx, children, cr, statusBefore, central, nodes)
		}},
	}
}

// reconcileDeleteRemoteChildren deletes everything this OVNChassis projected
// onto the target cluster it names and releases the remote-children finalizer,
// as commonmulticluster.SweepRemoteChildren documents.
//
// What it removes is the configuration of a node, not the node itself: the
// DaemonSet pods go, and with them ovn-controller, while the chassis records
// they registered in the Southbound database are the maintenance step's to
// deregister. A CR deleted outright therefore leaves those records behind, the
// same way deleting a Deployment leaves the rows its pods wrote.
func (r *OVNChassisReconciler) reconcileDeleteRemoteChildren(ctx context.Context, children client.Client, cr *ovnv1alpha1.OVNChassis) error {
	return commonmulticluster.SweepRemoteChildren(ctx, r.Client, r.Resolver, r.Recorder, r.Scheme,
		cr, cr.Spec.TargetClusterRef, children, OVNChassisRemoteChildKinds)
}

// updateStatus persists the current status conditions and returns the given
// result and error, delegating to the shared skeleton: the write is skipped when
// the pass left status semantically unchanged from the statusBefore snapshot, a
// failed write is joined with reconcileErr, and the mutate hook re-aggregates the
// Ready condition on every persist and stamps status.observedGeneration.
func (r *OVNChassisReconciler) updateStatus(ctx context.Context, cr *ovnv1alpha1.OVNChassis, statusBefore *ovnv1alpha1.OVNChassisStatus, result ctrl.Result, reconcileErr error) (ctrl.Result, error) {
	return chassisSkeleton.UpdateStatus(ctx, r.Client, cr, statusBefore, &cr.Status, func() {
		cr.Status.ObservedGeneration = cr.Generation
	}, result, reconcileErr)
}

// setChassisReadyCondition sets the aggregate Ready condition based on all
// sub-conditions, delegating to the shared skeleton with the OVNChassis
// sub-condition vocabulary.
func setChassisReadyCondition(cr *ovnv1alpha1.OVNChassis) {
	chassisSkeleton.SetReady(cr)
}

// centralToChassisMapper maps an event on an OVNCentral to a reconcile request
// for every OVNChassis attached to it, resolved through the
// OVNChassisCentralRefIndexKey index rather than by listing the namespace and
// filtering by hand.
//
// The leg carries no generation predicate: what the chassis wait on are the
// central's status flips (the Southbound address being published, the client
// Secret being named), and those leave the generation untouched.
//
// The List is namespace-scoped because spec.centralRef is: a chassis attaches
// to the OVNCentral beside it. A List failure is logged and maps to nothing,
// per the handler.MapFunc contract, and the requeue the central step polls with
// is the fallback.
func centralToChassisMapper(c client.Reader) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		central, ok := obj.(*ovnv1alpha1.OVNCentral)
		if !ok {
			// The leg watches OVNCentral alone, so this cannot happen; mapping an
			// object of another kind by its name would enqueue the chassis of the
			// OVNCentral that happens to share it.
			return nil
		}

		var chassis ovnv1alpha1.OVNChassisList
		if err := c.List(ctx, &chassis,
			client.InNamespace(central.Namespace),
			client.MatchingFields{OVNChassisCentralRefIndexKey: central.Name},
		); err != nil {
			log.FromContext(ctx).Error(err, "listing OVNChassis for the OVNCentral watch",
				"ovncentral", client.ObjectKeyFromObject(central))
			return nil
		}

		requests := make([]reconcile.Request, 0, len(chassis.Items))
		for i := range chassis.Items {
			requests = append(requests, reconcile.Request{
				NamespacedName: client.ObjectKeyFromObject(&chassis.Items[i]),
			})
		}
		return requests
	}
}

// nodeToChassisMapper maps an event on a Node to a reconcile request for every
// OVNChassis the local cache holds, across all namespaces.
//
// The fan-out is deliberate. A node's labels are what every CR's selector is
// evaluated against, and the CR that has to re-render is as often the one that
// just lost the node as the one that gained it, so there is no index to resolve
// a Node to a subset of CRs through. The leg is narrowed on the other side
// instead: it admits label changes only, so the kubelet's status heartbeat does
// not reach it.
//
// A List failure is logged and maps to nothing, per the handler.MapFunc
// contract.
func nodeToChassisMapper(c client.Reader) handler.MapFunc {
	return func(ctx context.Context, _ client.Object) []reconcile.Request {
		var chassis ovnv1alpha1.OVNChassisList
		if err := c.List(ctx, &chassis); err != nil {
			log.FromContext(ctx).Error(err, "listing OVNChassis for the Node watch")
			return nil
		}

		requests := make([]reconcile.Request, 0, len(chassis.Items))
		for i := range chassis.Items {
			requests = append(requests, reconcile.Request{
				NamespacedName: client.ObjectKeyFromObject(&chassis.Items[i]),
			})
		}
		return requests
	}
}

// SetupWithManager registers the OVNChassisReconciler with the controller
// manager. The shared controller options it applies let independent CRs
// reconcile in parallel instead of serialising at the controller-runtime
// default of 1, and the tuned RateLimiter caps per-item failure backoff at 30s
// rather than the default 1000s (see bootstrap.TypedControllerOptions).
func (r *OVNChassisReconciler) SetupWithManager(mgr mcmanager.Manager) error {
	return r.setupWithOptions(mgr, bootstrap.TypedControllerOptions[mcreconcile.Request](r.MaxConcurrentReconciles))
}

// setupWithOptions carries the production watch wiring SetupWithManager
// applies. The controller options are a parameter so an envtest integration
// suite can register this exact chain with SkipNameValidation set, rather than
// a hand-built copy of it that drifts the moment a leg is added here.
//
// The OVNChassis field index the central-to-chassis mapper looks up is
// registered by the OVNCentral reconciler's setup, which main.go runs first;
// see registerOVNIndexes for why one registration serves both controllers.
func (r *OVNChassisReconciler) setupWithOptions(mgr mcmanager.Manager, opts crcontroller.TypedOptions[mcreconcile.Request]) error {
	local := mgr.GetLocalManager()

	// Every leg watching the management cluster carries both engage options
	// below; see their definition for why an unpinned leg would stop watching
	// it once a provider is configured.
	engageLocal := commonmulticluster.EngageLocalCluster
	engageNoProviders := commonmulticluster.EngageNoProviderClusters

	// Every leg watching a target cluster is engaged on all of them, not on the
	// ones some CR names, so it has to drop the events belonging to a CR that
	// projects somewhere else (see commonmulticluster.RemoteRequests).
	targets := commonmulticluster.TargetClusterOf(local.GetClient(),
		func(cr *ovnv1alpha1.OVNChassis) *commonv1.TargetClusterRefSpec {
			return cr.Spec.TargetClusterRef
		})

	b := mcbuilder.ControllerManagedBy(mgr).
		WithOptions(opts).
		// Filter the CR's own status-only updates so Status().Update does not
		// re-wake the controller (see watch.CRUpdatePredicate).
		For(&ovnv1alpha1.OVNChassis{}, mcbuilder.WithPredicates(watch.CRUpdatePredicate()), engageLocal, engageNoProviders).
		Owns(&appsv1.DaemonSet{}, engageLocal, engageNoProviders).
		Owns(&corev1.ConfigMap{}, engageLocal, engageNoProviders).
		Owns(&batchv1.Job{}, engageLocal, engageNoProviders)

	// The same children, once more, on the clusters a CR can project onto. Owns
	// cannot see them: an owner reference does not cross a cluster boundary, so
	// the ownership labels are what maps a child back to its CR. No leg carries a
	// predicate, mirroring what Owns admits locally.
	b, err := commonmulticluster.AddRemoteChildWatches(b, local.GetScheme(), &ovnv1alpha1.OVNChassis{},
		targets, OVNChassisRemoteChildKinds, nil)
	if err != nil {
		return err
	}

	// The OVNCentral a chassis attaches to lives beside it on the management
	// cluster whatever cluster the children land on, so this leg needs no remote
	// counterpart.
	b = b.Watches(&ovnv1alpha1.OVNCentral{},
		commonmulticluster.LocalRequests(centralToChassisMapper(local.GetClient())),
		engageLocal, engageNoProviders)

	// The nodes, on both sides: the management cluster's for a chassis that
	// keeps its children local, and the target clusters' for a placed one. A
	// label change is what makes a node join or leave a chassis, so the
	// predicate is what keeps the fan-out off every other Node event.
	b, err = commonmulticluster.AddInputWatch(b, local.GetScheme(), targets, &corev1.Node{},
		nodeToChassisMapper(local.GetClient()),
		mcbuilder.WithPredicates(predicate.LabelChangedPredicate{}))
	if err != nil {
		return err
	}

	return b.
		// The default wrapper turns an error matching multicluster.ErrClusterNotFound
		// into a successful reconcile. This operator instead surfaces an
		// unresolvable cluster as a TargetClusterUnavailable condition and
		// requeues, so the wrapper stays off and the error semantics remain
		// byte-identical to the classic builder's.
		WithClusterNotFoundWrapper(false).
		Complete(commonmulticluster.LocalReconciler(r))
}
