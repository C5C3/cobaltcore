// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package controller implements the reconcilers of the OVN operator.
package controller

import (
	"context"
	"fmt"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
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
	"sigs.k8s.io/controller-runtime/pkg/log"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	"github.com/c5c3/cobaltcore/internal/common/bootstrap"
	"github.com/c5c3/cobaltcore/internal/common/gateway"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	"github.com/c5c3/cobaltcore/internal/common/watch"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
	ovnmetrics "github.com/c5c3/cobaltcore/operators/ovn/internal/metrics"
)

// OVNChassisCentralRefIndexKey is the field-indexer key under which OVNChassis
// CRs are indexed by the OVNCentral they attach to (spec.centralRef.name). The
// chassis controller's central-to-chassis mapper resolves an OVNCentral event
// through it with an O(1) reverse lookup, instead of listing every OVNChassis
// in the namespace and filtering by hand.
const OVNChassisCentralRefIndexKey = "spec.centralRef.name"

// ovnChassisCentralRefExtractor is the controller-runtime IndexerFunc
// registered under OVNChassisCentralRefIndexKey. spec.centralRef.name is
// required and immutable, so a chassis indexes under exactly one name.
func ovnChassisCentralRefExtractor(obj client.Object) []string {
	chassis, ok := obj.(*ovnv1alpha1.OVNChassis)
	if !ok || chassis.Spec.CentralRef.Name == "" {
		// controller-runtime should never call us with the wrong type; a nil
		// return is safer than a panic if it ever does.
		return nil
	}
	return []string{chassis.Spec.CentralRef.Name}
}

// registerOVNIndexes registers the OVNChassis field indexer under
// OVNChassisCentralRefIndexKey. It is the single registration site for both
// controllers of this operator: main.go sets the OVNCentral reconciler up
// first, so the chassis controller finds the index in place. The error is
// wrapped with the index key so the registration site is identifiable in
// manager-startup failure logs.
func registerOVNIndexes(ctx context.Context, indexer client.FieldIndexer) error {
	if err := indexer.IndexField(ctx, &ovnv1alpha1.OVNChassis{}, OVNChassisCentralRefIndexKey,
		ovnChassisCentralRefExtractor); err != nil {
		return fmt.Errorf("registering field indexer %q: %w", OVNChassisCentralRefIndexKey, err)
	}
	return nil
}

// centralSubConditionTypes lists the condition types set by the individual
// OVNCentral sub-reconcilers. The aggregate Ready condition is True only when
// all of them are True, so every sub-reconciler has to set its condition on
// every path it takes, including the ones where it creates nothing.
//
// The order is the pipeline's: the certificates come first because every OVN
// connection is authenticated with them, the two Raft databases next, their
// published addresses after that, and northd, the relay and the backup last,
// since each of those consumes an address the endpoint step publishes.
//
// Every entry references the owning sub-reconciler's own constant, so a rename
// cannot leave a stale literal behind here.
var centralSubConditionTypes = []string{
	conditionTypeTLSReady,
	conditionTypeNorthboundReady,
	conditionTypeSouthboundReady,
	conditionTypeEndpointsReady,
	conditionTypeNorthdReady,
	conditionTypeRelayReady,
	conditionTypeBackupReady,
}

// centralSkeleton bundles the shared controller-skeleton glue (Ready
// aggregation, no-op-skipping status writes, config-failure marking) with the
// OVNCentral sub-condition vocabulary and status accessor.
var centralSkeleton = commonreconcile.Skeleton[*ovnv1alpha1.OVNCentral, ovnv1alpha1.OVNCentralStatus]{
	SubConditionTypes: centralSubConditionTypes,
	Conditions:        func(c *ovnv1alpha1.OVNCentral) *[]metav1.Condition { return &c.Status.Conditions },
}

// certificateGVK identifies the cert-manager Certificate kind every OVN
// certificate is requested through. cert-manager owns the Secrets those
// Certificates write, which is why the Secret kind stays off the remote-child
// list below.
var certificateGVK = schema.GroupVersionKind{
	Group:   certmanagerv1.SchemeGroupVersion.Group,
	Version: certmanagerv1.SchemeGroupVersion.Version,
	Kind:    "Certificate",
}

// OVNCentralReconciler reconciles an OVNCentral object. Its fields mirror the
// sibling service reconcilers' core set.
type OVNCentralReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// MaxConcurrentReconciles bounds how many OVNCentral CRs reconcile
	// concurrently. It is threaded from the --max-concurrent-reconciles flag and
	// applied to the controller's controller.Options in SetupWithManager. A value
	// <= 0 falls back to bootstrap.DefaultMaxConcurrentReconciles inside
	// bootstrap.ControllerOptions, so the zero value is safe.
	MaxConcurrentReconciles int

	// certManagerAvailable is set during SetupWithManager from the management
	// cluster's RESTMapper and says whether the cert-manager.io/v1 Certificate
	// CRD is installed there. commonmulticluster.ChildrenServeKind answers with
	// it for local children while probing the target cluster's RESTMapper for
	// remote ones, and reconcileTLS turns a negative verdict into a
	// TLSReady=False/CertManagerUnavailable wait instead of applying a
	// Certificate the cluster would reject with "no matches for kind
	// Certificate".
	certManagerAvailable bool

	// Resolver resolves the target cluster an OVNCentral CR names in
	// spec.targetClusterRef into the client its children are read and written
	// with. Nil means always-local: every CR keeps its children on the
	// management cluster, which is what single-cluster tests and deployments
	// want.
	Resolver commonmulticluster.ClusterResolver
}

// OVNCentralRemoteChildKinds are the kinds an OVNCentral CR projects into the
// namespace of the target cluster it names, and the kinds the deletion sweep
// selects by ownership label when that CR is deleted. Nothing on the target
// cluster collects them, so a kind missing from this list is a kind that keeps
// running after its CR is gone.
//
// Secret is deliberately absent: every Secret the control plane mounts is a
// cert-manager Certificate's output, and cert-manager deletes it when the
// Certificate goes. The database PersistentVolumeClaims are absent for the same
// reason at one remove (the StatefulSet controller removes them through the
// persistentVolumeClaimRetentionPolicy the volumeClaimTemplates carry), while
// PersistentVolumeClaim itself stays on the list for the backup volume, which no
// controller owns.
var OVNCentralRemoteChildKinds = []schema.GroupVersionKind{
	appsv1.SchemeGroupVersion.WithKind("StatefulSet"),
	appsv1.SchemeGroupVersion.WithKind("Deployment"),
	corev1.SchemeGroupVersion.WithKind("Service"),
	corev1.SchemeGroupVersion.WithKind("ConfigMap"),
	corev1.SchemeGroupVersion.WithKind("PersistentVolumeClaim"),
	batchv1.SchemeGroupVersion.WithKind("CronJob"),
	batchv1.SchemeGroupVersion.WithKind("Job"),
	certificateGVK,
}

// The markers below cover both CR kinds of this operator. controller-gen
// collects them per operator and the Helm chart's ClusterRole is generated from
// that union, so every kind any controller in the binary reaches has to appear
// here: DaemonSet and the OVNChassis subresources belong to the chassis
// controller, the rest to this one.
//
// The CR kinds carry no create or delete verb: an OVNCentral and an OVNChassis
// are written by whoever deploys the control plane, and the operator only reads
// them, stamps their status and manages their finalizers. The projected child
// kinds carry the full set. The read-only core kinds are inputs: nodes and pods
// for the addresses the endpoint step publishes, and secrets for the
// certificate material cert-manager writes.

// No create and no delete: both CR kinds are written by whoever deploys the
// control plane. The operator reads them, stamps their status and manages their
// finalizers, and never brings one into being or takes it away.
// +kubebuilder:rbac:groups=ovn.openstack.c5c3.io,resources=ovncentrals;ovnchassis,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=ovn.openstack.c5c3.io,resources=ovncentrals/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=ovn.openstack.c5c3.io,resources=ovnchassis/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=ovn.openstack.c5c3.io,resources=ovncentrals/finalizers,verbs=update
// +kubebuilder:rbac:groups=ovn.openstack.c5c3.io,resources=ovnchassis/finalizers,verbs=update
// The three inputs the operator reads and never writes: nodes and pods for the
// addresses the endpoint step publishes to the chassis layer, secrets for the
// certificate material cert-manager writes.
// +kubebuilder:rbac:groups=core,resources=nodes;pods;secrets,verbs=get;list;watch
// persistentvolumeclaims covers the backup volume; the database volumes come
// from the StatefulSet volumeClaimTemplates.
// +kubebuilder:rbac:groups=core,resources=services;configmaps;persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
// statefulsets carry the northbound and southbound databases, deployments the
// northd and relay tiers, daemonsets the per-node chassis pods.
// +kubebuilder:rbac:groups=apps,resources=statefulsets;deployments;daemonsets,verbs=get;list;watch;create;update;patch;delete
// jobs covers the chassis maintenance Jobs; cronjobs covers the recurring
// database backup.
// +kubebuilder:rbac:groups=batch,resources=jobs;cronjobs,verbs=get;list;watch;create;update;patch;delete
// The operator issues the OVN client and server certificates through
// cert-manager and reads back the Secrets they write.
// +kubebuilder:rbac:groups=cert-manager.io,resources=certificates,verbs=get;list;watch;create;update;patch;delete

// Reconcile is the main reconciliation loop for the OVNCentral CR. It fetches
// the CR, drives the teardown of a terminating one, ensures the remote-children
// finalizer, then runs the sub-reconciler pipeline. Every exit funnels through
// updateStatus, which re-aggregates the Ready condition and stamps
// ObservedGeneration.
func (r *OVNCentralReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var cr ovnv1alpha1.OVNCentral
	if err := r.Get(ctx, req.NamespacedName, &cr); err != nil {
		if apierrors.IsNotFound(err) {
			log.FromContext(ctx).V(1).Info("OVNCentral resource not found; likely deleted")
			// The per-CR series are dropped here rather than from the teardown
			// path, which a CR keeping its children on the management cluster never
			// reaches: it carries no finalizer, so it leaves etcd on the Delete and
			// this pass is the only one that observes the deletion. Dropping series
			// for a name that recorded none is a no-op.
			ovnmetrics.DeleteForOVNCentral(req.Name, req.Namespace)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching OVNCentral: %w", err)
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
			centralSkeleton.MarkFailed(&cr, conditionTypeTLSReady,
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
	// the target cluster. The resolution runs before the finalizer is added so a
	// CR naming an unresolvable cluster stays clean of finalizers: nothing was
	// created for it, so there is nothing to clean up, and a finalizer would only
	// block its deletion.
	children, err := commonmulticluster.ResolveChildrenClient(ctx, r.Resolver, r.Client, cr.Spec.TargetClusterRef)
	if err != nil {
		// This exit precedes the pipeline's status snapshot below, so it takes its
		// own baseline for the skip-unchanged write. TLSReady is the pipeline's
		// first gate, so the failure lands on the condition the rest of the graph
		// waits behind.
		statusBefore := cr.Status.DeepCopy()
		centralSkeleton.MarkFailed(&cr, conditionTypeTLSReady, commonmulticluster.TargetClusterUnavailable, err)
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
	statusBefore := cr.Status.DeepCopy()

	result, err := commonreconcile.RunPipeline(ctx, instrumenter.Instrument, r.pipelineSteps(children, &cr))
	return r.updateStatus(ctx, &cr, statusBefore, result, err)
}

// pipelineSteps returns the ordered sub-reconciler pipeline for one OVNCentral.
// Each step runs in dependency order; the first to return a non-zero result or
// an error short-circuits the chain and funnels through updateStatus.
//
// TLS is the first gate: every OVN connection is authenticated with the
// certificates it requests, so a database projected before them would come up
// with nothing to present. The two Raft databases follow, then the endpoint step
// that publishes their addresses, which the three members of the parallel group
// all consume.
//
// It is a method rather than a literal inside Reconcile so the drift guard can
// enumerate the step names without running a reconcile.
func (r *OVNCentralReconciler) pipelineSteps(children client.Client, cr *ovnv1alpha1.OVNCentral) []commonreconcile.Step {
	return []commonreconcile.Step{
		{Name: "TLS", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileTLS(ctx, children, cr)
		}},
		{Name: "Northbound", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileNorthbound(ctx, children, cr)
		}},
		{Name: "Southbound", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileSouthbound(ctx, children, cr)
		}},
		{Name: "Endpoints", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileEndpoints(ctx, children, cr)
		}},
		// northd, the relay and the backup read the published addresses and
		// nothing of each other's output, so they run concurrently. Each member
		// sets exactly one condition type; the group self-instruments its members,
		// so this step carries no sub_reconciler name.
		{Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileParallelGroup(ctx, cr, r.parallelSteps(children, cr))
		}},
	}
}

// parallelSteps returns the members of the post-endpoint parallel group. Each
// member sets exactly one condition type and receives its own copy of the CR, so
// none of them reads a value another one produces.
//
// primary is the CR the pipeline runs on. RunParallelGroup merges the
// conditions and the metadata off a member's copy and nothing else, so the two
// fields a member publishes into status — the relay address an OVNChassis
// dials, and the image northd is running, which is what tells a rollout that
// reached the pods from one that has not — are copied onto it here or they are
// discarded with the copy. The two members write different fields, so the
// primary is touched by at most one goroutine per field.
//
// It is a method rather than a literal inside pipelineSteps so the drift guard
// can enumerate the member names and their condition types without running a
// reconcile.
func (r *OVNCentralReconciler) parallelSteps(children client.Client, primary *ovnv1alpha1.OVNCentral) []commonreconcile.ParallelStep[*ovnv1alpha1.OVNCentral] {
	return []commonreconcile.ParallelStep[*ovnv1alpha1.OVNCentral]{
		{
			Name:          "Northd",
			ConditionType: conditionTypeNorthdReady,
			Fn: func(ctx context.Context, cr *ovnv1alpha1.OVNCentral) (ctrl.Result, error) {
				res, err := r.reconcileNorthd(ctx, children, cr)
				primary.Status.InstalledImage = cr.Status.InstalledImage
				return res, err
			},
		},
		{
			Name:          "Relay",
			ConditionType: conditionTypeRelayReady,
			Fn: func(ctx context.Context, cr *ovnv1alpha1.OVNCentral) (ctrl.Result, error) {
				res, err := r.reconcileRelay(ctx, children, cr)
				primary.Status.RelayAddress = cr.Status.RelayAddress
				return res, err
			},
		},
		{
			Name:          "Backup",
			ConditionType: conditionTypeBackupReady,
			Fn: func(ctx context.Context, cr *ovnv1alpha1.OVNCentral) (ctrl.Result, error) {
				return r.reconcileBackup(ctx, children, cr)
			},
		},
	}
}

// reconcileParallelGroup runs the given sub-reconcilers concurrently, delegating
// to the shared skeleton: each member operates on its own DeepCopy of the
// OVNCentral CR, conditions from every member (including those that succeeded
// before a peer failed) are merged back into the primary cr, and on success the
// shortest non-zero RequeueAfter is returned. Members instrument individually
// via instrumenter.Instrument.
func (r *OVNCentralReconciler) reconcileParallelGroup(
	ctx context.Context,
	cr *ovnv1alpha1.OVNCentral,
	subs []commonreconcile.ParallelStep[*ovnv1alpha1.OVNCentral],
) (ctrl.Result, error) {
	return centralSkeleton.RunParallelGroup(ctx, cr, instrumenter.Instrument, subs)
}

// reconcileDeleteRemoteChildren deletes everything this OVNCentral projected
// onto the target cluster it names and releases the remote-children finalizer,
// as commonmulticluster.SweepRemoteChildren documents. It is the operator's
// whole teardown path: nothing else outlives the CR, because the databases and
// their volumes are children like the rest, and a CR that kept its children
// local is left to the garbage collection cascade.
func (r *OVNCentralReconciler) reconcileDeleteRemoteChildren(ctx context.Context, children client.Client, cr *ovnv1alpha1.OVNCentral) error {
	return commonmulticluster.SweepRemoteChildren(ctx, r.Client, r.Resolver, r.Recorder, r.Scheme,
		cr, cr.Spec.TargetClusterRef, children, OVNCentralRemoteChildKinds)
}

// updateStatus persists the current status conditions and returns the given
// result and error, delegating to the shared skeleton: the write is skipped when
// the pass left status semantically unchanged from the statusBefore snapshot, a
// failed write is joined with reconcileErr, and the mutate hook re-aggregates the
// Ready condition on every persist and stamps status.observedGeneration.
func (r *OVNCentralReconciler) updateStatus(ctx context.Context, cr *ovnv1alpha1.OVNCentral, statusBefore *ovnv1alpha1.OVNCentralStatus, result ctrl.Result, reconcileErr error) (ctrl.Result, error) {
	return centralSkeleton.UpdateStatus(ctx, r.Client, cr, statusBefore, &cr.Status, func() {
		cr.Status.ObservedGeneration = cr.Generation
	}, result, reconcileErr)
}

// setReadyCondition sets the aggregate Ready condition based on all
// sub-conditions, delegating to the shared skeleton with the OVNCentral
// sub-condition vocabulary.
func setReadyCondition(cr *ovnv1alpha1.OVNCentral) {
	centralSkeleton.SetReady(cr)
}

// SetupWithManager registers the OVNCentralReconciler with the controller
// manager. The shared controller options it applies let independent CRs
// reconcile in parallel instead of serialising at the controller-runtime
// default of 1, and the tuned RateLimiter caps per-item failure backoff at 30s
// rather than the default 1000s (see bootstrap.TypedControllerOptions).
func (r *OVNCentralReconciler) SetupWithManager(mgr mcmanager.Manager) error {
	return r.setupWithOptions(mgr, bootstrap.TypedControllerOptions[mcreconcile.Request](r.MaxConcurrentReconciles))
}

// setupWithOptions carries the production watch wiring SetupWithManager
// applies. The controller options are a parameter so an envtest integration
// suite can register this exact chain with SkipNameValidation set, rather than
// a hand-built copy of it that drifts the moment a leg is added here.
func (r *OVNCentralReconciler) setupWithOptions(mgr mcmanager.Manager, opts crcontroller.TypedOptions[mcreconcile.Request]) error {
	local := mgr.GetLocalManager()

	// Detect cert-manager so the operator can Owns(Certificate) and so
	// reconcileTLS knows whether a Certificate can exist at all on the cluster
	// the children land on. spec.tls is required, so no OVNCentral runs without
	// cert-manager, but the operator itself has to start on a cluster that lacks
	// it: an unconditional Owns(Certificate) would fail at Start with "no matches
	// for kind Certificate", which takes down every controller in the binary
	// instead of reporting the missing CRD on the CR.
	r.certManagerAvailable = gateway.IsGVKAvailable(local.GetRESTMapper(), certificateGVK)
	setupLog := ctrl.Log.WithName("ovncentral-setup")
	if r.certManagerAvailable {
		setupLog.Info("cert-manager detected; enabling Certificate watch and reconciliation")
	} else {
		setupLog.Info("cert-manager not installed; Certificate watch disabled, spec.tls will be reported through the TLSReady condition")
	}

	// Register the OVNChassis field indexer before Watches so the chassis
	// controller's mapper can rely on it for its MatchingFields lookup. The
	// index goes on the LOCAL field indexer, not mgr's: with a provider
	// configured, the multicluster manager's field indexer registers against the
	// provider clusters, which hold no OVNChassis CR. Registration stays local by
	// contract: the index is on a CR kind, which exists on the management cluster
	// alone, and every request the watches emit is pinned to that cluster
	// (LocalRequests / RemoteRequests, internal/common/multicluster/watch.go), so
	// a remote event resolves to its CR through the local cache. Registering on
	// the fleet would fail the engagement of every target cluster, because the
	// kubeconfig provider applies its stored indexes while engaging one.
	if err := registerOVNIndexes(context.Background(), local.GetFieldIndexer()); err != nil {
		return err
	}

	// Every leg watching the management cluster carries both engage options
	// below; see their definition for why an unpinned leg would stop watching
	// it once a provider is configured.
	engageLocal := commonmulticluster.EngageLocalCluster
	engageNoProviders := commonmulticluster.EngageNoProviderClusters

	// Every leg watching a target cluster is engaged on all of them, not on the
	// ones some CR names, so it has to drop the events belonging to a CR that
	// projects somewhere else (see commonmulticluster.RemoteRequests).
	targets := commonmulticluster.TargetClusterOf(local.GetClient(),
		func(cr *ovnv1alpha1.OVNCentral) *commonv1.TargetClusterRefSpec {
			return cr.Spec.TargetClusterRef
		})

	b := mcbuilder.ControllerManagedBy(mgr).
		WithOptions(opts).
		// Filter the CR's own status-only updates so Status().Update does not
		// re-wake the controller (see watch.CRUpdatePredicate).
		For(&ovnv1alpha1.OVNCentral{}, mcbuilder.WithPredicates(watch.CRUpdatePredicate()), engageLocal, engageNoProviders).
		Owns(&appsv1.StatefulSet{}, engageLocal, engageNoProviders).
		Owns(&appsv1.Deployment{}, engageLocal, engageNoProviders).
		Owns(&corev1.Service{}, engageLocal, engageNoProviders).
		Owns(&corev1.ConfigMap{}, engageLocal, engageNoProviders).
		Owns(&corev1.PersistentVolumeClaim{}, engageLocal, engageNoProviders).
		Owns(&batchv1.CronJob{}, engageLocal, engageNoProviders).
		Owns(&batchv1.Job{}, engageLocal, engageNoProviders)

	if r.certManagerAvailable {
		b = b.Owns(&certmanagerv1.Certificate{}, engageLocal, engageNoProviders)
	}

	// The same children, once more, on the clusters a CR can project onto. Owns
	// cannot see them: an owner reference does not cross a cluster boundary, so
	// the ownership labels are what maps a child back to its CR. No leg carries a
	// predicate, mirroring what Owns admits locally.
	b, err := commonmulticluster.AddRemoteChildWatches(b, local.GetScheme(), &ovnv1alpha1.OVNCentral{},
		targets, OVNCentralRemoteChildKinds, nil)
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
