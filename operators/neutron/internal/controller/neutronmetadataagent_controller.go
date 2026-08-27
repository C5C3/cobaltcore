// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"slices"

	appsv1 "k8s.io/api/apps/v1"
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
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	"github.com/c5c3/cobaltcore/internal/common/watch"
	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// metadataAgentAppName is the app.kubernetes.io/name label value carried by
// every child of a NeutronMetadataAgent. It is the CR kind in lower case, which
// is what keeps those children distinguishable from the ones a Neutron of the
// same name projects into the same namespace.
const metadataAgentAppName = "neutronmetadataagent"

// metadataAgentComponent is the app.kubernetes.io/component label value, the
// container name and the name suffix of the DaemonSet.
const metadataAgentComponent = "metadata-agent"

// NeutronMetadataAgentSecretNameIndexKey is the field-indexer key under which
// NeutronMetadataAgent CRs are indexed by the union of their referenced Secret
// names (spec.novaMetadata.sharedSecretRef.name and
// spec.messaging.secretRef.name). The Secret watch mapper resolves an event
// through it with an O(1) reverse lookup instead of listing every agent in the
// namespace.
// #nosec G101 -- field-indexer key (a JSONPath-like field selector), not a credential.
const NeutronMetadataAgentSecretNameIndexKey = "spec.secretRefs.name"

// NeutronMetadataAgentChassisRefIndexKey is the field-indexer key under which
// NeutronMetadataAgent CRs are indexed by the OVNChassis they run alongside.
// The indexed value is the bare name: spec.chassisRef is namespace-local,
// because the agent pods mount the chassis's runtime directory.
const NeutronMetadataAgentChassisRefIndexKey = "spec.chassisRef.name"

// OVNChassisCentralRefIndexKey is the field-indexer key under which OVNChassis
// CRs are indexed by the OVNCentral they attach to (spec.centralRef.name). The
// OVNCentral watch leg resolves an event through it instead of listing every
// OVNChassis in the namespace and filtering by hand — a list that deep-copies
// one OVNChassisNodeStatus per selected node, per chassis, on every status
// write of the central.
//
// A field index is registered on this manager's own cache, so the OVN operator
// owning the kind neither has to register it nor conflicts with it, and the
// OVNChassis informer this index needs already exists for the Watches leg.
const OVNChassisCentralRefIndexKey = "spec.centralRef.name"

// agentSubConditionTypes lists the condition types set by the individual
// NeutronMetadataAgent sub-reconcilers. The aggregate Ready condition is True
// only when all of these are True. The agent runs no optional step: every one of
// the three is set on every pass.
//
// It is a separate list from subConditionTypes because the two kinds carry
// separate status contracts, while one instrumenter serves both pipelines. The
// drift guard in instrumentation_test.go checks the metrics map against the
// union of the two.
var agentSubConditionTypes = []string{
	conditionTypeChassisReady,
	"SecretsReady",
	conditionTypeDaemonSetReady,
}

// agentSkeleton bundles the shared controller-skeleton glue (Ready aggregation,
// no-op-skipping status writes, config-failure marking) with the
// NeutronMetadataAgent sub-condition vocabulary and status accessor.
var agentSkeleton = commonreconcile.Skeleton[*neutronv1alpha1.NeutronMetadataAgent, neutronv1alpha1.NeutronMetadataAgentStatus]{
	SubConditionTypes: agentSubConditionTypes,
	Conditions: func(cr *neutronv1alpha1.NeutronMetadataAgent) *[]metav1.Condition {
		return &cr.Status.Conditions
	},
}

// NeutronMetadataAgentRemoteChildKinds are the kinds a NeutronMetadataAgent CR
// projects into the namespace of the target cluster it names, and the kinds the
// deletion sweep selects by ownership label when that CR is deleted. Nothing on
// the target cluster collects them, so a kind missing from this list is a kind
// that keeps running after its CR is gone.
//
// The list is short because the agent owns no state of its own: the DaemonSet,
// the immutable config ConfigMaps its pods mount, and the derived transport-URL
// Secret. The client Secret the pods mount beside it is not listed: cert-manager
// issues it and the OVNCentral publishes it, and this controller only reads its
// name off that central.
var NeutronMetadataAgentRemoteChildKinds = []schema.GroupVersionKind{
	appsv1.SchemeGroupVersion.WithKind("DaemonSet"),
	corev1.SchemeGroupVersion.WithKind("ConfigMap"),
	corev1.SchemeGroupVersion.WithKind("Secret"),
}

// agentSecretNameExtractor is the controller-runtime IndexerFunc registered
// under NeutronMetadataAgentSecretNameIndexKey. It returns the deduplicated,
// non-empty union of Secret names a NeutronMetadataAgent references. Both blocks
// are optional, and spec.messaging.secretRef is nil in managed mode, where the
// transport URL is derived from a RabbitmqCluster instead of read from a Secret,
// so an agent may index under no name at all.
func agentSecretNameExtractor(obj client.Object) []string {
	agent, ok := obj.(*neutronv1alpha1.NeutronMetadataAgent)
	if !ok {
		// controller-runtime should never call us with the wrong type; a nil
		// return is safer than a panic if it ever does.
		return nil
	}

	var referenced []string
	if ref := agentSharedSecretRef(agent); ref != nil {
		referenced = append(referenced, ref.Name)
	}
	if agent.Spec.Messaging != nil && agent.Spec.Messaging.SecretRef != nil {
		referenced = append(referenced, agent.Spec.Messaging.SecretRef.Name)
	}

	names := make([]string, 0, len(referenced))
	for _, name := range referenced {
		if name == "" || slices.Contains(names, name) {
			continue
		}
		names = append(names, name)
	}
	return names
}

// agentChassisRefExtractor is the IndexerFunc registered under
// NeutronMetadataAgentChassisRefIndexKey. spec.chassisRef.name is required and
// immutable, so an agent indexes under exactly one name.
func agentChassisRefExtractor(obj client.Object) []string {
	agent, ok := obj.(*neutronv1alpha1.NeutronMetadataAgent)
	if !ok || agent.Spec.ChassisRef.Name == "" {
		// controller-runtime should never call us with the wrong type; a nil
		// return is safer than a panic if it ever does.
		return nil
	}
	return []string{agent.Spec.ChassisRef.Name}
}

// ovnChassisCentralRefExtractor is the IndexerFunc registered under
// OVNChassisCentralRefIndexKey. spec.centralRef.name is required and immutable
// on an OVNChassis, so a chassis indexes under exactly one name.
func ovnChassisCentralRefExtractor(obj client.Object) []string {
	chassis, ok := obj.(*ovnv1alpha1.OVNChassis)
	if !ok || chassis.Spec.CentralRef.Name == "" {
		// controller-runtime should never call us with the wrong type; a nil
		// return is safer than a panic if it ever does.
		return nil
	}
	return []string{chassis.Spec.CentralRef.Name}
}

// registerAgentIndexes registers the three field indexers the agent's watch legs
// resolve through: two on the NeutronMetadataAgent itself and one on the
// OVNChassis the OVNCentral leg hops over. The errors are wrapped with the index
// key so the registration site is identifiable in manager-startup failure logs.
func registerAgentIndexes(ctx context.Context, indexer client.FieldIndexer) error {
	if err := watch.RegisterSecretNameIndex(ctx, indexer, &neutronv1alpha1.NeutronMetadataAgent{},
		NeutronMetadataAgentSecretNameIndexKey, agentSecretNameExtractor); err != nil {
		return err
	}
	if err := indexer.IndexField(ctx, &neutronv1alpha1.NeutronMetadataAgent{},
		NeutronMetadataAgentChassisRefIndexKey, agentChassisRefExtractor); err != nil {
		return fmt.Errorf("registering field indexer %q: %w", NeutronMetadataAgentChassisRefIndexKey, err)
	}
	if err := indexer.IndexField(ctx, &ovnv1alpha1.OVNChassis{},
		OVNChassisCentralRefIndexKey, ovnChassisCentralRefExtractor); err != nil {
		return fmt.Errorf("registering field indexer %q: %w", OVNChassisCentralRefIndexKey, err)
	}
	return nil
}

// NeutronMetadataAgentReconciler reconciles a NeutronMetadataAgent object. Its
// fields mirror the Neutron reconciler's core set, minus the API-specific seams:
// the agent serves no HTTP, so it has no health-check client, and it projects no
// NetworkPolicy, so it needs no operator namespace.
type NeutronMetadataAgentReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// MaxConcurrentReconciles bounds how many NeutronMetadataAgent CRs reconcile
	// concurrently. It is threaded from the --max-concurrent-reconciles flag and
	// applied to the controller's controller.Options in SetupWithManager. A value
	// <= 0 falls back to bootstrap.DefaultMaxConcurrentReconciles inside
	// bootstrap.ControllerOptions, so the zero value is safe.
	MaxConcurrentReconciles int

	// Resolver resolves the target cluster a NeutronMetadataAgent CR names in
	// spec.targetClusterRef into the client its children are read and written
	// with. Nil means always-local: every CR keeps its children on the management
	// cluster, which is what single-cluster tests and deployments want.
	Resolver commonmulticluster.ClusterResolver
}

// The markers below repeat the set the Neutron reconciler carries, verbatim.
// controller-gen collects them per operator and generates one ClusterRole from
// their union, so either block on its own already describes what the binary
// needs, and the two are kept identical rather than split by controller: a
// reader of this file sees the permissions its own children require without
// opening the sibling controller, and a marker trimmed on one side is a
// difference between two otherwise equal blocks.

// +kubebuilder:rbac:groups=neutron.openstack.c5c3.io,resources=neutrons,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=neutron.openstack.c5c3.io,resources=neutrons/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=neutron.openstack.c5c3.io,resources=neutrons/finalizers,verbs=update
// +kubebuilder:rbac:groups=neutron.openstack.c5c3.io,resources=neutronmetadataagents,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=neutron.openstack.c5c3.io,resources=neutronmetadataagents/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=neutron.openstack.c5c3.io,resources=neutronmetadataagents/finalizers,verbs=update
// +kubebuilder:rbac:groups=ovn.openstack.c5c3.io,resources=ovncentrals;ovnchassis,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=services;configmaps;secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=apps,resources=deployments;daemonsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs;cronjobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=k8s.mariadb.com,resources=databases;users;grants,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=k8s.mariadb.com,resources=mariadbs,verbs=get;list;watch
// +kubebuilder:rbac:groups=rabbitmq.com,resources=rabbitmqclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=external-secrets.io,resources=externalsecrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=external-secrets.io,resources=clustersecretstores;secretstores,verbs=get;list;watch
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes/status,verbs=get
// +kubebuilder:rbac:groups=scheduling.k8s.io,resources=priorityclasses,verbs=get;list;watch

// Reconcile is the main reconciliation loop for the NeutronMetadataAgent CR. It
// fetches the CR, drives the teardown of a terminating one, ensures the
// remote-children finalizer, then runs the sub-reconciler pipeline. Every exit
// funnels through updateStatus, which re-aggregates the Ready condition and
// stamps ObservedGeneration.
func (r *NeutronMetadataAgentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var cr neutronv1alpha1.NeutronMetadataAgent
	if err := r.Get(ctx, req.NamespacedName, &cr); err != nil {
		if apierrors.IsNotFound(err) {
			log.FromContext(ctx).V(1).Info("NeutronMetadataAgent resource not found; likely deleted")
			// Nothing to drop here, unlike the Neutron path: the operator's per-CR
			// collectors count db-sync runs, and only a Neutron owns a schema.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching NeutronMetadataAgent: %w", err)
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
			agentSkeleton.MarkFailed(&cr, conditionTypeChassisReady,
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
	// The embedded client stays on the management cluster (the CR, its status,
	// its finalizer and the two OVN CRs it reads live there); children carries
	// everything the CR projects into the target cluster. The resolution runs
	// before the finalizer is added so a CR naming an unresolvable cluster stays
	// clean of finalizers: nothing was created for it, so there is nothing to
	// clean up, and a finalizer would only block its deletion.
	children, err := commonmulticluster.ResolveChildrenClient(ctx, r.Resolver, r.Client, cr.Spec.TargetClusterRef)
	if err != nil {
		// This exit precedes the pipeline's status snapshot below, so it takes its
		// own baseline for the skip-unchanged write. ChassisReady is the pipeline's
		// first gate, so the failure lands on the condition the rest of the graph
		// waits behind.
		statusBefore := cr.Status.DeepCopy()
		agentSkeleton.MarkFailed(&cr, conditionTypeChassisReady, commonmulticluster.TargetClusterUnavailable, err)
		return r.updateStatus(ctx, &cr, statusBefore,
			ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, nil)
	}

	// The remote-children finalizer goes on only when the CR projects onto a
	// target cluster, and it is the only finalizer this controller installs. A CR
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

// pipelineSteps returns the ordered sub-reconciler pipeline for one
// NeutronMetadataAgent. Each step runs in dependency order; the first to return
// a non-zero result or an error short-circuits the chain and funnels through
// updateStatus.
//
// The chassis is the first gate: the nodes it selects, its central's Southbound
// address and that central's client Secret parameterise every later step, so an
// agent whose chassis has not resolved projects nothing at all. The credentials
// come next, because the rendered config is a function of the messaging block
// and the DaemonSet mounts the Secrets, and the config before the DaemonSet that
// mounts it.
//
// chassis, the transport digest and the ConfigMap name are threaded from the
// steps that produce them to the steps that consume them. A closure keeps that
// hand-off inside the pipeline instead of on the reconciler, where it would be
// state shared by every CR the controller reconciles concurrently.
//
// It is a method rather than a literal inside Reconcile so the drift guard can
// enumerate the step names without running a reconcile.
func (r *NeutronMetadataAgentReconciler) pipelineSteps(children client.Client,
	cr *neutronv1alpha1.NeutronMetadataAgent,
) []commonreconcile.Step {
	var (
		chassis                        resolvedChassis
		transportDigest, configMapName string
	)

	return []commonreconcile.Step{
		{Name: "Chassis", Fn: func(ctx context.Context) (ctrl.Result, error) {
			var res ctrl.Result
			var err error
			chassis, res, err = r.reconcileChassis(ctx, cr)
			return res, err
		}},
		{Name: "Secrets", Fn: func(ctx context.Context) (res ctrl.Result, err error) {
			res, transportDigest, err = r.reconcileAgentSecrets(ctx, children, cr)
			return res, err
		}},
		{Name: "Config", Fn: func(ctx context.Context) (res ctrl.Result, err error) {
			res, configMapName, err = r.reconcileAgentConfig(ctx, children, cr, chassis)
			return res, err
		}},
		{Name: "DaemonSet", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileDaemonSet(ctx, children, cr, chassis, configMapName, transportDigest)
		}},
	}
}

// reconcileDeleteRemoteChildren deletes everything this NeutronMetadataAgent
// projected onto the target cluster it names and releases the remote-children
// finalizer, as commonmulticluster.SweepRemoteChildren documents.
//
// What it removes is the agent, not the networking: the DaemonSet pods go, and
// with them the metadata proxies they run, while the logical model those proxies
// answered from stays in the OVN databases.
func (r *NeutronMetadataAgentReconciler) reconcileDeleteRemoteChildren(ctx context.Context, children client.Client,
	cr *neutronv1alpha1.NeutronMetadataAgent,
) error {
	return commonmulticluster.SweepRemoteChildren(ctx, r.Client, r.Resolver, r.Recorder, r.Scheme,
		cr, cr.Spec.TargetClusterRef, children, NeutronMetadataAgentRemoteChildKinds)
}

// updateStatus persists the current status conditions and returns the given
// result and error, delegating to the shared skeleton: the write is skipped when
// the pass left status semantically unchanged from the statusBefore snapshot, a
// failed write is joined with reconcileErr, and the mutate hook re-aggregates
// the Ready condition on every persist and stamps status.observedGeneration.
func (r *NeutronMetadataAgentReconciler) updateStatus(ctx context.Context, cr *neutronv1alpha1.NeutronMetadataAgent,
	statusBefore *neutronv1alpha1.NeutronMetadataAgentStatus, result ctrl.Result, reconcileErr error,
) (ctrl.Result, error) {
	return agentSkeleton.UpdateStatus(ctx, r.Client, cr, statusBefore, &cr.Status, func() {
		cr.Status.ObservedGeneration = cr.Generation
	}, result, reconcileErr)
}

// SetupWithManager registers the NeutronMetadataAgentReconciler with the
// controller manager. The shared controller options it applies let independent
// CRs reconcile in parallel instead of serialising at the controller-runtime
// default of 1, and the tuned RateLimiter caps per-item failure backoff at 30s
// rather than the default 1000s (see bootstrap.TypedControllerOptions).
func (r *NeutronMetadataAgentReconciler) SetupWithManager(mgr mcmanager.Manager) error {
	return r.setupWithOptions(mgr, bootstrap.TypedControllerOptions[mcreconcile.Request](r.MaxConcurrentReconciles))
}

// setupWithOptions carries the production watch wiring SetupWithManager applies.
// The controller options are a parameter so an envtest integration suite can
// register this exact chain with SkipNameValidation set, rather than a hand-built
// copy of it that drifts the moment a leg is added here.
func (r *NeutronMetadataAgentReconciler) setupWithOptions(mgr mcmanager.Manager, opts crcontroller.TypedOptions[mcreconcile.Request]) error {
	local := mgr.GetLocalManager()

	// The indexes go on the LOCAL field indexer, not mgr's: with a provider
	// configured, the multicluster manager's field indexer registers against the
	// provider clusters, which hold no NeutronMetadataAgent CR. See
	// registerNeutronIndexes for the full reasoning; it holds for both kinds.
	if err := registerAgentIndexes(context.Background(), local.GetFieldIndexer()); err != nil {
		return err
	}

	// Every leg watching the management cluster carries both engage options
	// below; see their definition for why an unpinned leg would stop watching it
	// once a provider is configured.
	engageLocal := commonmulticluster.EngageLocalCluster
	engageNoProviders := commonmulticluster.EngageNoProviderClusters

	// Every leg watching a target cluster is engaged on all of them, not on the
	// ones some CR names, so it has to drop the events belonging to a CR that
	// projects somewhere else (see commonmulticluster.RemoteRequests).
	targets := commonmulticluster.TargetClusterOf(local.GetClient(),
		func(cr *neutronv1alpha1.NeutronMetadataAgent) *commonv1.TargetClusterRefSpec {
			return cr.Spec.TargetClusterRef
		})

	b := mcbuilder.ControllerManagedBy(mgr).
		WithOptions(opts).
		// Filter the CR's own status-only updates so Status().Update does not
		// re-wake the controller (see watch.CRUpdatePredicate).
		For(&neutronv1alpha1.NeutronMetadataAgent{}, mcbuilder.WithPredicates(watch.CRUpdatePredicate()),
			engageLocal, engageNoProviders).
		Owns(&appsv1.DaemonSet{}, engageLocal, engageNoProviders).
		Owns(&corev1.ConfigMap{}, engageLocal, engageNoProviders).
		Owns(&corev1.Secret{}, engageLocal, engageNoProviders)

	// The same children, once more, on the clusters a CR can project onto. Owns
	// cannot see them: an owner reference does not cross a cluster boundary, so
	// the ownership labels are what maps a child back to its CR. No leg carries a
	// predicate, mirroring what Owns admits locally.
	b, err := commonmulticluster.AddRemoteChildWatches(b, local.GetScheme(), &neutronv1alpha1.NeutronMetadataAgent{},
		targets, NeutronMetadataAgentRemoteChildKinds, nil)
	if err != nil {
		return err
	}

	// Watch Secrets and map to the agents that reference them by name or own
	// them: the Nova shared secret and the brownfield transport URL on the input
	// side, the derived transport-URL Secret on the owned side.
	b, err = commonmulticluster.AddInputWatch(b, local.GetScheme(), targets, &corev1.Secret{},
		secretToAgentMapper(local.GetClient()))
	if err != nil {
		return err
	}

	return b.
		// The two OVN CRs live on the management cluster whatever cluster the
		// children land on, so neither leg needs a remote counterpart. Neither
		// carries a generation predicate: what the agents wait on are status flips
		// (the central publishing its Southbound address and its client Secret),
		// and those leave the generation untouched.
		Watches(&ovnv1alpha1.OVNChassis{},
			commonmulticluster.LocalRequests(chassisToAgentsMapper(local.GetClient())),
			engageLocal, engageNoProviders).
		Watches(&ovnv1alpha1.OVNCentral{},
			commonmulticluster.LocalRequests(centralToAgentsMapper(local.GetClient())),
			engageLocal, engageNoProviders).
		// The default wrapper turns an error matching multicluster.ErrClusterNotFound
		// into a successful reconcile. This operator instead surfaces an
		// unresolvable cluster as a TargetClusterUnavailable condition and
		// requeues, so the wrapper stays off and the error semantics remain
		// byte-identical to the classic builder's.
		WithClusterNotFoundWrapper(false).
		Complete(commonmulticluster.LocalReconciler(r))
}
