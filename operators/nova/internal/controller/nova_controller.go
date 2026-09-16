// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package controller implements the Nova reconciler.
package controller

import (
	"context"
	"fmt"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	"github.com/c5c3/cobaltcore/internal/common/bootstrap"
	"github.com/c5c3/cobaltcore/internal/common/conditions"
	"github.com/c5c3/cobaltcore/internal/common/database"
	"github.com/c5c3/cobaltcore/internal/common/gateway"
	"github.com/c5c3/cobaltcore/internal/common/healthcheck"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	"github.com/c5c3/cobaltcore/internal/common/secrets"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	"github.com/c5c3/cobaltcore/internal/common/watch"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
	"github.com/c5c3/cobaltcore/operators/nova/internal/metrics"
)

// subConditionTypes lists the condition types set by the individual Nova
// sub-reconcilers. The aggregate Ready condition is True only when all of these
// are True. Every parallel-group member (HPA, NetworkPolicy and the three
// HTTPRoutes) always sets its condition, configured-ready, NotRequired, or
// waiting, so a gateway-less or autoscaling-less cluster still resolves the
// aggregate (the NotRequired paths report True), exactly as the sibling
// operators aggregate their optional conditions.
//
// ExtraConfigHealthy stays out: it reports on an overlay the user owns, is
// informational, and must not depool a Nova whose API serves fine.
var subConditionTypes = []string{
	"SecretsReady",
	"ComputeConfigReady",
	"DatabaseReady",
	"ConductorReady",
	"SchedulerReady",
	"MetadataReady",
	"ConsoleProxyReady",
	"DeploymentReady",
	"DBArchiveReady",
	"NovaAPIReady",
	"HPAReady",
	"NetworkPolicyReady",
	"HTTPRouteReady",
	"MetadataHTTPRouteReady",
	"ConsoleHTTPRouteReady",
}

// novaFinalizer blocks removal of a Nova CR from etcd until the MariaDB
// Database, User, and Grant CRs of both schemas have been issued a Delete, so
// the schema teardown is triggered before the owner-ref chain disappears. It is
// the single source of truth for Reconcile, the finalizer handler, and tests.
const novaFinalizer = "nova.openstack.c5c3.io/finalizer"

// httpRouteGVK identifies the HTTPRoute kind the operator watches when Gateway
// API is installed. Availability is probed at setup time via the shared
// gateway.IsGVKAvailable RESTMapper probe.
var httpRouteGVK = schema.GroupVersionKind{
	Group:   gatewayv1.GroupVersion.Group,
	Version: gatewayv1.GroupVersion.Version,
	Kind:    "HTTPRoute",
}

// novaSkeleton bundles the shared controller-skeleton glue (Ready aggregation,
// no-op-skipping status writes, config-failure marking) with nova's
// sub-condition vocabulary and status accessor. The wrapper helper below
// delegates to it.
var novaSkeleton = commonreconcile.Skeleton[*novav1alpha1.Nova, novav1alpha1.NovaStatus]{
	SubConditionTypes: subConditionTypes,
	Conditions:        func(n *novav1alpha1.Nova) *[]metav1.Condition { return &n.Status.Conditions },
}

// NovaReconciler reconciles a Nova object: it drives the sub-reconciler chain
// that projects the API, metadata, scheduler, conductor and console-proxy
// workloads together with the Secrets, config and database children they read.
type NovaReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// OperatorNamespace is the Namespace the operator Pod runs in (resolved at
	// startup by bootstrap.DetectOperatorNamespace). The networkpolicy step
	// appends an ingress peer for this Namespace so the operator's own health
	// check can reach the Nova API. Empty when the namespace could not be
	// determined, in which case no operator-namespace peer is added.
	OperatorNamespace string

	// MaxConcurrentReconciles bounds how many Nova CRs reconcile concurrently.
	// It is threaded from the --max-concurrent-reconciles flag and applied to the
	// controller's controller.Options in SetupWithManager. A value <= 0 falls back
	// to bootstrap.DefaultMaxConcurrentReconciles inside
	// bootstrap.ControllerOptions, so the zero value is safe.
	MaxConcurrentReconciles int

	// HTTPClient is the health-check client seam. Production leaves it nil so the
	// health check uses http.DefaultClient; tests inject a stub transport.
	HTTPClient healthcheck.HTTPDoer

	// apiReader is the management cluster's direct, uncached reader, set during
	// SetupWithManager from mgr.GetAPIReader(). The database step reads the
	// termination message of a finished migration Job's pod through it (or
	// through the target cluster's own uncached reader), so that one read per Job
	// does not start a cluster-wide pod informer the operator's RBAC cannot
	// watch. Nil in unit tests that construct the reconciler without a manager;
	// those read through the children client.
	apiReader client.Reader

	// Resolver resolves the target cluster a Nova CR names in
	// spec.targetClusterRef into the client its children are read and written
	// with. Nil means always-local: every CR keeps its children on the management
	// cluster, which is what single-cluster tests and deployments want.
	Resolver commonmulticluster.ClusterResolver

	// gatewayAPIAvailable is set during SetupWithManager from the management
	// cluster's RESTMapper and indicates whether the
	// gateway.networking.k8s.io/v1 HTTPRoute CRD is installed there. Two
	// consumers read it: the local HTTPRoute watch leg, which SetupWithManager
	// skips when false so the controller does not crash on a missing kind, and
	// commonmulticluster.ChildrenServeKind, which answers with it for local
	// children while probing the target cluster's RESTMapper for remote ones.
	gatewayAPIAvailable bool

	// healthProbeCache memoizes the last successful Nova API probe per CR
	// (shared TTL probe cache) so a steady-state reconcile does not fire a
	// synchronous HTTP GET on every pass. The cache's internal mutex guards
	// concurrent access under MaxConcurrentReconciles > 1.
	healthProbeCache healthcheck.ProbeCache
}

// setReadyCondition sets the aggregate Ready condition based on all
// sub-conditions, delegating to the shared skeleton with nova's sub-condition
// vocabulary.
func setReadyCondition(nova *novav1alpha1.Nova) {
	novaSkeleton.SetReady(nova)
}

// conditionReasonConfigError is the SecretsReady=False reason set when
// reconcileConfig fails. Config artefacts (the rendered nova.conf ConfigMap)
// gate the same downstream graph as the upstream credential Secrets, so failures
// reuse SecretsReady rather than a dedicated condition, matching
// reconcileDBConnectionSecrets' Config to SecretsReady mapping.
const conditionReasonConfigError = "ConfigError"

// markConfigFailed flips SecretsReady to False so a reconcileConfig failure
// cannot leave the aggregate Ready condition stale-True at the new
// ObservedGeneration. It mirrors the sibling operators' markConfigFailed helper.
func markConfigFailed(nova *novav1alpha1.Nova, err error) {
	conditions.SetCondition(&nova.Status.Conditions, metav1.Condition{
		Type:               "SecretsReady",
		Status:             metav1.ConditionFalse,
		ObservedGeneration: nova.Generation,
		Reason:             conditionReasonConfigError,
		Message:            err.Error(),
	})
}

// registerNovaIndexes registers the field indexer this operator relies on: the
// Nova Secret-name union the Secret watch resolves through. It is the single
// registration site for the operator, shared by SetupWithManager and the envtest
// helper.
func registerNovaIndexes(ctx context.Context, indexer client.FieldIndexer) error {
	return watch.RegisterSecretNameIndex(ctx, indexer, &novav1alpha1.Nova{},
		NovaSecretNameIndexKey, novaSecretNameExtractor)
}

// NovaRemoteChildKinds are the kinds a Nova CR projects into the namespace of
// the target cluster it names, and the kinds reconcileDeleteRemoteChildren
// sweeps by ownership label when that CR is deleted. Nothing on the target
// cluster collects them, so a kind missing from this list is a kind that keeps
// running after its CR is gone.
//
// HTTPRoute is listed although the kind is optional: a target cluster without
// Gateway API answers the sweep's list with a no-match, which
// commonmulticluster.DeleteRemoteChildren skips, and the watch leg is likewise
// engaged only on the clusters that serve the kind. Leaving it out instead would
// strand the routes of every cluster that does have Gateway API.
var NovaRemoteChildKinds = []schema.GroupVersionKind{
	appsv1.SchemeGroupVersion.WithKind("Deployment"),
	corev1.SchemeGroupVersion.WithKind("Service"),
	corev1.SchemeGroupVersion.WithKind("ConfigMap"),
	corev1.SchemeGroupVersion.WithKind("Secret"),
	batchv1.SchemeGroupVersion.WithKind("Job"),
	batchv1.SchemeGroupVersion.WithKind("CronJob"),
	policyv1.SchemeGroupVersion.WithKind("PodDisruptionBudget"),
	autoscalingv2.SchemeGroupVersion.WithKind("HorizontalPodAutoscaler"),
	networkingv1.SchemeGroupVersion.WithKind("NetworkPolicy"),
	httpRouteGVK,
	mariadbv1alpha1.GroupVersion.WithKind("Database"),
	mariadbv1alpha1.GroupVersion.WithKind("User"),
	mariadbv1alpha1.GroupVersion.WithKind("Grant"),
}

// The operator never creates a Nova; it reads them, patches their status and
// holds them with a finalizer.
// +kubebuilder:rbac:groups=nova.openstack.c5c3.io,resources=novas,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=nova.openstack.c5c3.io,resources=novas/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=nova.openstack.c5c3.io,resources=novas/finalizers,verbs=update
// configmaps carry the rendered nova.conf and its role overlays; secrets covers
// the derived db-connection, transport-url and compute-contract Secrets.
// +kubebuilder:rbac:groups=core,resources=services;configmaps;secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
// Required to read the termination message a finished migration Job left on its
// Pod, which carries the upgrade-check findings and the cell mapping the
// operator reports as events and status.
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list
// deployments carry the API, the metadata API, the scheduler, the conductor and
// the console proxy.
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// jobs covers the db-sync of both schemas and the expand/migrate/contract
// phases; cronjobs covers the recurring archive of soft-deleted rows.
// +kubebuilder:rbac:groups=batch,resources=jobs;cronjobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=k8s.mariadb.com,resources=databases;users;grants,verbs=get;list;watch;create;update;patch;delete
// Required for the operator to observe the referenced MariaDB clusters' Ready
// condition and reflect outages in DatabaseReady.
// +kubebuilder:rbac:groups=k8s.mariadb.com,resources=mariadbs,verbs=get;list;watch
// In managed messaging mode the transport URL is derived from the named
// RabbitmqCluster instead of read from a Secret, so the operator reads and
// watches the cluster.
// +kubebuilder:rbac:groups=rabbitmq.com,resources=rabbitmqclusters,verbs=get;list;watch
// The database, service-user, metadata and messaging credential Secrets are
// ESO-managed; the operator only reads the ExternalSecrets to attribute a
// not-synced Secret in SecretsReady messages.
// +kubebuilder:rbac:groups=external-secrets.io,resources=externalsecrets,verbs=get;list;watch
// Required so the operator can observe the selected store's Ready condition and
// reflect upstream secret-backend outages in SecretsReady. A CR selects either
// the shared cluster-scoped ClusterSecretStore (default) or a namespaced
// SecretStore, so both kinds must be watchable.
// +kubebuilder:rbac:groups=external-secrets.io,resources=clustersecretstores;secretstores,verbs=get;list;watch
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// Required to create/update/delete the HTTPRoutes that expose the Nova API, the
// metadata API and the console proxy externally.
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch;create;update;patch;delete
// Required so the operator can observe the Accepted condition set by the
// upstream Gateway controller and reflect it in the three route conditions.
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes/status,verbs=get
// Required for the webhook to validate that spec.priorityClassName references
// an existing PriorityClass at admission time.
// +kubebuilder:rbac:groups=scheduling.k8s.io,resources=priorityclasses,verbs=get;list;watch

// Reconcile is the main reconciliation loop for the Nova CR. It fetches the CR,
// drives the finalizer-gated deletion path, ensures the finalizers, then runs
// the sub-reconciler pipeline. Every exit funnels through updateStatus, which
// re-aggregates the Ready condition and stamps ObservedGeneration.
func (r *NovaReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var nova novav1alpha1.Nova
	if err := r.Get(ctx, req.NamespacedName, &nova); err != nil {
		if apierrors.IsNotFound(err) {
			log.FromContext(ctx).V(1).Info("Nova resource not found; likely deleted")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching Nova: %w", err)
	}

	// Handle deletion via the finalizer: issue Delete on the MariaDB CRs of both
	// schemas, then release the finalizer once no live (not-yet-deleted) resource
	// remains. The cleanup itself sets no conditions, so it returns directly
	// without updateStatus; only the hold on an unresolvable target below reports.
	//
	// It comes before the target-cluster resolution below and uses the deletion
	// variant, which never fails the pass: a CR whose cluster was deregistered
	// after the finalizer went on would otherwise short-circuit on the
	// unresolvable ref on every pass and stay Terminating forever. A target that
	// has not resolved yet requeues instead of being given up on: engagement is
	// asynchronous, so an operator restart looks exactly like a deregistration
	// until the provider has synced.
	if !nova.DeletionTimestamp.IsZero() {
		children, wait := commonmulticluster.ResolveChildrenClientForDeletion(
			ctx, r.Resolver, r.Client, nova.Spec.TargetClusterRef, *nova.DeletionTimestamp)
		if wait {
			// The hold goes on the CR, not only into the operator's log. It is a
			// deliberate state a CR can sit in for minutes, and "Terminating,
			// waiting on the target cluster" has to be distinguishable from a
			// wedged finalizer without correlating logs across replicas. This exit
			// precedes the pipeline's status snapshot below, so it takes its own
			// baseline for the skip-unchanged write.
			statusBefore := nova.Status.DeepCopy()
			novaSkeleton.MarkFailed(&nova, "SecretsReady",
				commonmulticluster.TargetClusterUnavailable,
				fmt.Errorf("target cluster %s does not resolve; waiting at least %s before abandoning its children",
					nova.Spec.TargetClusterRef.Name, commonmulticluster.AbandonAfter))
			return r.updateStatus(ctx, &nova, statusBefore,
				ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, nil)
		}
		if result, err := r.reconcileDelete(ctx, children, &nova); !result.IsZero() || err != nil {
			return result, err
		}
		// The label-selected sweep runs after the named MariaDB cleanup, never
		// before it: that flow waits one pass on the CRs it deletes by name, and a
		// sweep running first would delete them out from under it. The ordering
		// mirrors the local one, where the garbage collection cascade starts only
		// once every finalizer has been released.
		if err := r.reconcileDeleteRemoteChildren(ctx, children, &nova); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Resolve the client every child object of this CR is read and written with.
	// The embedded client stays on the management cluster (the CR, its status and
	// its finalizers live there); children carries everything the CR projects into
	// the target cluster. The resolution runs before the finalizer is added so a
	// CR naming an unresolvable cluster stays clean of finalizers: nothing was
	// created for it, so there is nothing to clean up, and a finalizer would only
	// block its deletion.
	children, err := commonmulticluster.ResolveChildrenClient(ctx, r.Resolver, r.Client, nova.Spec.TargetClusterRef)
	if err != nil {
		// This exit precedes the pipeline's status snapshot below, so it takes its
		// own baseline for the skip-unchanged write.
		statusBefore := nova.Status.DeepCopy()
		novaSkeleton.MarkFailed(&nova, "SecretsReady", commonmulticluster.TargetClusterUnavailable, err)
		return r.updateStatus(ctx, &nova, statusBefore,
			ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, nil)
	}

	// Ensure the finalizer is installed before any sub-reconciler runs so a
	// deletion issued before the next pass still funnels through reconcileDelete.
	// Requeuing after the Update guarantees the next reconcile observes the
	// persisted finalizer rather than the in-memory copy.
	if added, err := commonreconcile.EnsureFinalizer(ctx, r.Client, &nova, novaFinalizer); err != nil {
		return ctrl.Result{}, err
	} else if added {
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueNextPass}, nil
	}

	// The remote-children finalizer goes on only when the CR projects onto a
	// target cluster. A local CR keeps the garbage collection cascade, which reaps
	// its children from their owner references, so it has nothing for this
	// finalizer to hold the CR open for. spec.targetClusterRef is immutable, so
	// the condition cannot flip under a live CR.
	if nova.Spec.TargetClusterRef != nil {
		if added, err := commonreconcile.EnsureFinalizer(ctx, r.Client, &nova,
			commonmulticluster.RemoteChildrenFinalizer); err != nil {
			return ctrl.Result{}, err
		} else if added {
			return ctrl.Result{RequeueAfter: commonreconcile.RequeueNextPass}, nil
		}
	}

	// Snapshot the persisted status so updateStatus can skip the write when a pass
	// leaves status unchanged (no write, no watch event, no resourceVersion
	// churn). Taken after the finalizer add so an early requeue there does not
	// race a status write.
	statusBefore := nova.Status.DeepCopy()

	result, err := commonreconcile.RunPipeline(ctx, instrumenter.Instrument,
		r.pipelineSteps(children, &nova, &pipelineState{}))
	return r.updateStatus(ctx, &nova, statusBefore, result, err)
}

// pipelineState carries the values one sub-reconciler hands to a later one
// within a single reconcile pass. It is filled in step order: the credential
// values and their digests by the three Secret steps, the broker port and the
// transport URL by the messaging step, and the config artefacts by the render
// step. Every field keeps its zero value on the waiting and error paths of the
// step that produces it, where the downstream steps either omit what it would
// have configured or wait themselves.
type pipelineState struct {
	// serviceUserDigest is the SHA-256 of the service-user password and
	// metadataSecretDigest the one of the metadata shared secret; apiDSNDigest and
	// cellDSNDigest are the digests of the two assembled DSNs and transportDigest
	// the one of the transport URL. The workload steps stamp them into
	// pod-template annotations so a rotated credential rolls the pods.
	serviceUserDigest    string
	metadataSecretDigest string
	apiDSNDigest         string
	cellDSNDigest        string
	transportDigest      string
	// transportURL is the assembled rabbit:// URL. The compute-config step writes
	// it into the contract Secret a compute cluster reads its bus from.
	transportURL string
	// egressPort is the broker's TCP port the networkpolicy member opens and the
	// RPC workloads probe their bus on.
	egressPort int32
	// values are the credential values the Secrets step read. The compute-config
	// step writes the metadata shared secret and the broker CA into the contract.
	values secretValues
	// art names the rendered config ConfigMap and its data keys.
	art configArtifacts
}

// digests bundles the five content digests the workload steps stamp into their
// pod templates.
func (s *pipelineState) digests() workloadDigests {
	return workloadDigests{
		apiDSN:         s.apiDSNDigest,
		cellDSN:        s.cellDSNDigest,
		authToken:      s.serviceUserDigest,
		transport:      s.transportDigest,
		metadataSecret: s.metadataSecretDigest,
	}
}

// pipelineSteps returns the ordered sub-reconciler pipeline for one Nova. Each
// step runs in dependency order; the first to return a non-zero result or an
// error short-circuits the chain and funnels through updateStatus. state is the
// pass-local scratch space the steps hand their outputs along in.
//
// It is a method rather than a literal inside Reconcile so the drift guard can
// enumerate the step names without running a reconcile.
func (r *NovaReconciler) pipelineSteps(children client.Client, nova *novav1alpha1.Nova,
	state *pipelineState,
) []commonreconcile.Step {
	return []commonreconcile.Step{
		// reconcileSecrets gates the store and the four (five on a verified bus)
		// credential Secrets and reads the two values the later steps digest. The
		// digests are taken here rather than inside the sub-reconciler so the
		// waiting and error paths leave them empty, which is what keeps a
		// half-read pass from clearing a rollout annotation.
		{Name: "Secrets", Fn: func(ctx context.Context) (ctrl.Result, error) {
			res, values, err := r.reconcileSecrets(ctx, children, nova)
			if err != nil || !res.IsZero() {
				return res, err
			}
			state.values = values
			state.serviceUserDigest = secrets.AdminPasswordDigest(values.serviceUserPassword)
			state.metadataSecretDigest = secrets.AdminPasswordDigest(values.metadataSharedSecret)
			return res, nil
		}},
		// reconcileDBConnectionSecrets materialises both database URLs into the
		// derived <nova.Name>-api-db-connection and <nova.Name>-db-connection
		// Secrets. It runs after Secrets (upstream credentials must be synced) and
		// before Config; failures set SecretsReady=False, the same condition
		// reconcileSecrets uses.
		{Name: "DBConnectionSecrets", Fn: func(ctx context.Context) (res ctrl.Result, err error) {
			var dsn dsnDigests
			res, dsn, err = r.reconcileDBConnectionSecrets(ctx, children, nova)
			state.apiDSNDigest, state.cellDSNDigest = dsn.api, dsn.cell
			return res, err
		}},
		// reconcileTransportURLSecret materialises the rabbit:// URL into the
		// derived <nova.Name>-transport-url Secret. It also reports the broker port
		// the networkpolicy member opens as an egress peer and the RPC workloads
		// probe their bus on.
		{Name: "TransportURLSecret", Fn: func(ctx context.Context) (res ctrl.Result, err error) {
			res, state.transportURL, state.transportDigest, state.egressPort, err = r.reconcileTransportURLSecret(ctx, children, nova)
			return res, err
		}},
		// reconcileConfig renders nova.conf and the four role overlays into an
		// immutable ConfigMap. It self-marks SecretsReady=False on failure via
		// markConfigFailed, so the wrapper only threads the result.
		{Name: "Config", Fn: func(ctx context.Context) (res ctrl.Result, err error) {
			res, state.art, err = r.reconcileConfig(ctx, children, nova)
			return res, err
		}},
		// reconcileComputeConfig publishes the contract a compute cluster joins on:
		// the shared config fragment, the transport URL, the metadata shared secret
		// and the broker CA.
		{Name: "ComputeConfig", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileComputeConfig(ctx, children, nova, state.transportURL, state.values)
		}},
		// reconcileDatabase provisions the nova_api schema, the cell schema and
		// cell0, gates the requested OpenStack release against the installed one,
		// and runs the migration Jobs against the rendered config.
		{Name: "Database", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileDatabase(ctx, children, nova, state.art.configMapName)
		}},
		// reconcileConductor projects the nova-conductor Deployment. It runs after
		// Database so the process only starts once the schemas it writes exist.
		{Name: "Conductor", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileConductor(ctx, children, nova, state.art, state.digests(), state.egressPort)
		}},
		// reconcileScheduler projects the nova-scheduler Deployment.
		{Name: "Scheduler", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileScheduler(ctx, children, nova, state.art, state.digests(), state.egressPort)
		}},
		// reconcileMetadata projects the nova-metadata-api Deployment and its
		// Service, which the Neutron metadata agent proxies to.
		{Name: "Metadata", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileMetadata(ctx, children, nova, state.art, state.digests())
		}},
		// reconcileConsoleProxy projects the nova-novncproxy Deployment and its
		// Service, or removes them when the console is switched off.
		{Name: "ConsoleProxy", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileConsoleProxy(ctx, children, nova, state.art, state.digests())
		}},
		// reconcileDeployment projects the API Deployment, its Service and the
		// PodDisruptionBudget, and stamps status.endpoint.
		{Name: "Deployment", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileDeployment(ctx, children, nova, state.art, state.digests())
		}},
		// reconcileDBArchive projects the recurring archive CronJob. It runs BEFORE
		// the parallel group rather than inside it because the group runs behind the
		// whole chain, and the archive only needs the rendered config the Config
		// step above produced.
		{Name: "DBArchive", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileDBArchive(ctx, children, nova, state.art)
		}},
		// Once the workload outputs are in place, the three routes, the health
		// check, the HPA and the network policy have no inter-dependency and run
		// concurrently. Each member sets exactly one condition type; the group
		// self-instruments its members, so this step carries no sub_reconciler name.
		{Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileParallelGroup(ctx, nova, r.parallelSteps(children, state))
		}},
	}
}

// parallelSteps returns the members of the post-deployment parallel group. Each
// member sets exactly one condition type and receives its own copy of the CR, so
// none of them reads a value another one produces. state carries the outputs the
// earlier steps produced in the same pass.
//
// It is a method rather than a literal inside pipelineSteps so the drift guard
// can enumerate the member names and their condition types without running a
// reconcile.
func (r *NovaReconciler) parallelSteps(children client.Client,
	state *pipelineState,
) []commonreconcile.ParallelStep[*novav1alpha1.Nova] {
	return []commonreconcile.ParallelStep[*novav1alpha1.Nova]{
		{
			Name:          "HTTPRoute",
			ConditionType: conditionTypeHTTPRouteReady,
			Fn: func(ctx context.Context, n *novav1alpha1.Nova) (ctrl.Result, error) {
				return r.reconcileHTTPRoute(ctx, children, n)
			},
		},
		{
			Name:          "MetadataHTTPRoute",
			ConditionType: conditionTypeMetadataHTTPRouteReady,
			Fn: func(ctx context.Context, n *novav1alpha1.Nova) (ctrl.Result, error) {
				return r.reconcileMetadataHTTPRoute(ctx, children, n)
			},
		},
		{
			Name:          "ConsoleHTTPRoute",
			ConditionType: conditionTypeConsoleHTTPRouteReady,
			Fn: func(ctx context.Context, n *novav1alpha1.Nova) (ctrl.Result, error) {
				return r.reconcileConsoleHTTPRoute(ctx, children, n)
			},
		},
		{
			Name:          "HealthCheck",
			ConditionType: conditionTypeNovaAPIReady,
			Fn: func(ctx context.Context, n *novav1alpha1.Nova) (ctrl.Result, error) {
				return r.reconcileHealthCheck(ctx, n)
			},
		},
		{
			Name:          "HPA",
			ConditionType: "HPAReady",
			Fn: func(ctx context.Context, n *novav1alpha1.Nova) (ctrl.Result, error) {
				return r.reconcileHPA(ctx, children, n)
			},
		},
		{
			Name:          "NetworkPolicy",
			ConditionType: conditionTypeNetworkPolicyReady,
			Fn: func(ctx context.Context, n *novav1alpha1.Nova) (ctrl.Result, error) {
				return r.reconcileNetworkPolicy(ctx, children, n, state.egressPort)
			},
		},
	}
}

// reconcileParallelGroup runs the given sub-reconcilers concurrently, delegating
// to the shared skeleton: each member operates on its own DeepCopy of the Nova
// CR, conditions from every member (including those that succeeded before a peer
// failed) are merged back into the primary nova, and on success the shortest
// non-zero RequeueAfter is returned. Members instrument individually via
// instrumenter.Instrument.
func (r *NovaReconciler) reconcileParallelGroup(
	ctx context.Context,
	nova *novav1alpha1.Nova,
	subs []commonreconcile.ParallelStep[*novav1alpha1.Nova],
) (ctrl.Result, error) {
	return novaSkeleton.RunParallelGroup(ctx, nova, instrumenter.Instrument, subs)
}

// reconcileDelete drives the finalizer cleanup when the Nova CR is being
// deleted. It is a no-op when the finalizer is absent. Otherwise it issues
// Delete on the MariaDB Database/User/Grant CRs of both schemas (idempotent,
// NotFound-tolerant) and, while at least one of them was still live (not yet
// issued a Delete), holds the finalizer for one more pass so the schema teardown
// is triggered before the owner-ref chain disappears. Once no live resource
// remains it drops the per-CR metrics, evicts the health-probe cache, and
// releases the finalizer.
//
// The two schemas are finalized under the names they were provisioned with: the
// nova_api block under the "-api" instance name, the cell block under the bare
// CR name. cell0 is an additional schema of the cell block, so its Database and
// Grant are reached through the cell key rather than a key of their own.
//
// A nil children client means the target cluster this CR named is no longer
// registered. Its MariaDB CRs cannot be reached, so they stay behind on a cluster
// that has not resolved for the whole abandon window, and the finalizer is
// released anyway: holding it would only strand the CR in Terminating.
func (r *NovaReconciler) reconcileDelete(ctx context.Context, children client.Client,
	nova *novav1alpha1.Nova,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(nova, novaFinalizer) {
		return ctrl.Result{}, nil
	}

	apiKey := client.ObjectKey{Name: apiInstanceName(nova), Namespace: nova.Namespace}
	cellKey := client.ObjectKey{Name: nova.Name, Namespace: nova.Namespace}

	if children == nil {
		r.Recorder.Event(nova, corev1.EventTypeWarning, "RemoteChildrenAbandoned",
			"Target cluster is no longer registered; releasing the finalizer without deleting the MariaDB Databases, Users, and Grants on it")
	} else {
		// Observe whether any MariaDB CR is still live BEFORE issuing the Delete: a
		// Delete flips DeletionTimestamp, so a post-Delete check would always report
		// none-live and release immediately. Gating on the pre-Delete observation
		// keeps the CR alive one extra pass so the teardown is actually triggered.
		// Both schemas are probed before either is deleted, so a live cell0 alone
		// still holds the CR.
		apiLive, err := database.HasLiveResources(ctx, children, apiKey)
		if err != nil {
			return ctrl.Result{}, err
		}
		cellLive, err := database.HasLiveResources(ctx, children, cellKey, cell0Schema(nova))
		if err != nil {
			return ctrl.Result{}, err
		}

		if err := database.FinalizeResources(ctx, children, apiKey); err != nil {
			return ctrl.Result{}, err
		}
		if err := database.FinalizeResources(ctx, children, cellKey, cell0Schema(nova)); err != nil {
			return ctrl.Result{}, err
		}

		if apiLive || cellLive {
			r.Recorder.Event(nova, corev1.EventTypeNormal, "FinalizingDatabase",
				"Cleaning up the MariaDB Databases, Users, and Grants of both schemas before removing Nova")
			return ctrl.Result{RequeueAfter: RequeueDatabaseWait}, nil
		}

		r.Recorder.Event(nova, corev1.EventTypeNormal, "DatabaseFinalized",
			"MariaDB Databases, Users, and Grants marked for deletion; releasing finalizer")
	}

	controllerutil.RemoveFinalizer(nova, novaFinalizer)
	if err := r.Update(ctx, nova); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}
	metrics.DeleteForNova(nova.Name, nova.Namespace)
	// Drop the per-CR health-probe cache so a CR recreated under the same
	// name/namespace never serves a stale probe keyed on the deleted CR's UID.
	// cellKey is the CR's own name and namespace, which is what the cache keys on.
	r.healthProbeCache.Evict(cellKey)
	return ctrl.Result{}, nil
}

// reconcileDeleteRemoteChildren deletes everything this Nova projected onto the
// target cluster it names and releases the remote-children finalizer, as
// commonmulticluster.SweepRemoteChildren documents. reconcileDelete above deletes
// the MariaDB CRs it tracks by name; this pass is what reaches the rest, selected
// on the ownership labels Claim stamped on them.
func (r *NovaReconciler) reconcileDeleteRemoteChildren(ctx context.Context, children client.Client,
	nova *novav1alpha1.Nova,
) error {
	return commonmulticluster.SweepRemoteChildren(ctx, r.Client, r.Resolver, r.Recorder, r.Scheme,
		nova, nova.Spec.TargetClusterRef, children, NovaRemoteChildKinds)
}

// updateStatus persists the current status conditions and returns the given
// result and error, delegating to the shared skeleton: the write is skipped when
// the pass left status semantically unchanged from the statusBefore snapshot, a
// failed write is joined with reconcileErr, and the mutate hook re-aggregates the
// Ready condition on every persist and stamps status.observedGeneration.
func (r *NovaReconciler) updateStatus(ctx context.Context, nova *novav1alpha1.Nova,
	statusBefore *novav1alpha1.NovaStatus, result ctrl.Result, reconcileErr error,
) (ctrl.Result, error) {
	return novaSkeleton.UpdateStatus(ctx, r.Client, nova, statusBefore, &nova.Status, func() {
		nova.Status.ObservedGeneration = nova.Generation
	}, result, reconcileErr)
}

// SetupWithManager registers the NovaReconciler with the controller manager. The
// shared controller options it applies let independent CRs reconcile in parallel
// instead of serialising at the controller-runtime default of 1, and the tuned
// RateLimiter caps per-item failure backoff at 30s rather than the default 1000s
// (see bootstrap.TypedControllerOptions).
func (r *NovaReconciler) SetupWithManager(mgr mcmanager.Manager) error {
	return r.setupWithOptions(mgr, bootstrap.TypedControllerOptions[mcreconcile.Request](r.MaxConcurrentReconciles))
}

// setupWithOptions carries the production watch wiring SetupWithManager applies.
// The controller options are a parameter so an envtest integration suite can
// register this exact chain with SkipNameValidation set, rather than a hand-built
// copy of it that drifts the moment a leg is added here.
func (r *NovaReconciler) setupWithOptions(mgr mcmanager.Manager, opts crcontroller.TypedOptions[mcreconcile.Request]) error {
	local := mgr.GetLocalManager()

	// Detect whether the Gateway API CRD is installed. The three gateway blocks
	// are optional, so the operator must run on clusters without Gateway API.
	// Adding Owns(HTTPRoute) unconditionally would fail at Start with "no matches
	// for kind HTTPRoute" when the CRD is missing, blocking every Nova CR.
	r.gatewayAPIAvailable = gateway.IsGVKAvailable(local.GetRESTMapper(), httpRouteGVK)
	r.apiReader = local.GetAPIReader()
	setupLog := ctrl.Log.WithName("nova-setup")
	if r.gatewayAPIAvailable {
		setupLog.Info("Gateway API detected; enabling HTTPRoute watch and reconciliation")
	} else {
		setupLog.Info("Gateway API not installed; HTTPRoute watch disabled, the gateway blocks will be rejected via the route conditions")
	}

	// Register the field indexer before Watches so the Secret mapper can rely on
	// it for its MatchingFields lookup. The index goes on the LOCAL field indexer,
	// not mgr's: with a provider configured, the multicluster manager's field
	// indexer registers against the provider clusters, which hold no Nova CR.
	// Registration stays local by contract: the index is on a CR kind, which
	// exists on the management cluster alone, and every request the watches emit
	// is pinned to that cluster (LocalRequests / RemoteRequests,
	// internal/common/multicluster/watch.go), so a remote event resolves to its CR
	// through the local cache. Registering on the fleet would fail the engagement
	// of every target cluster, because the kubeconfig provider applies its stored
	// indexes while engaging one.
	if err := registerNovaIndexes(context.Background(), local.GetFieldIndexer()); err != nil {
		return err
	}

	// Every leg watching the management cluster carries both engage options below;
	// see their definition for why an unpinned leg would stop watching it once a
	// provider is configured.
	engageLocal := commonmulticluster.EngageLocalCluster
	engageNoProviders := commonmulticluster.EngageNoProviderClusters

	// Every leg watching a target cluster is engaged on all of them, not on the
	// ones some CR names, so it has to drop the events belonging to a CR that
	// projects somewhere else (see commonmulticluster.RemoteRequests).
	targets := commonmulticluster.TargetClusterOf(local.GetClient(),
		func(nova *novav1alpha1.Nova) *commonv1.TargetClusterRefSpec {
			return nova.Spec.TargetClusterRef
		})

	b := mcbuilder.ControllerManagedBy(mgr).
		WithOptions(opts).
		// Filter the CR's own status-only updates so Status().Update does not
		// re-wake the controller (see watch.CRUpdatePredicate).
		For(&novav1alpha1.Nova{}, mcbuilder.WithPredicates(watch.CRUpdatePredicate()), engageLocal, engageNoProviders).
		Owns(&appsv1.Deployment{}, engageLocal, engageNoProviders).
		Owns(&corev1.Service{}, engageLocal, engageNoProviders).
		Owns(&corev1.ConfigMap{}, engageLocal, engageNoProviders).
		Owns(&corev1.Secret{}, engageLocal, engageNoProviders).
		Owns(&batchv1.Job{}, engageLocal, engageNoProviders).
		Owns(&batchv1.CronJob{}, engageLocal, engageNoProviders).
		Owns(&policyv1.PodDisruptionBudget{}, engageLocal, engageNoProviders).
		Owns(&autoscalingv2.HorizontalPodAutoscaler{}, engageLocal, engageNoProviders).
		Owns(&networkingv1.NetworkPolicy{}, engageLocal, engageNoProviders)

	if r.gatewayAPIAvailable {
		b = b.Owns(&gatewayv1.HTTPRoute{}, engageLocal, engageNoProviders)
	}

	// The same children, once more, on the clusters a CR can project onto. Owns
	// cannot see them: an owner reference does not cross a cluster boundary, so
	// the ownership labels are what maps a child back to its CR. No leg carries a
	// predicate, mirroring what Owns admits locally.
	b, err := commonmulticluster.AddRemoteChildWatches(b, local.GetScheme(), &novav1alpha1.Nova{},
		targets, NovaRemoteChildKinds, nil)
	if err != nil {
		return err
	}

	// Watch Secrets and map to the Nova CRs that reference them by name or own
	// them. ESO-managed Secrets are owned by the ExternalSecret controller, not the
	// Nova CR, so EnqueueRequestForOwner would never match them.
	b, err = commonmulticluster.AddInputWatch(b, local.GetScheme(), targets, &corev1.Secret{},
		secretToNovaMapper(local.GetClient()))
	if err != nil {
		return err
	}

	// Watch the MariaDB cluster CRs referenced by spec.apiDatabase.clusterRef and
	// spec.database.clusterRef so the operator reflects upstream database outages
	// in DatabaseReady without waiting for the next periodic requeue. One leg
	// carries both references: a Nova is enqueued when either block names the
	// cluster the event came from.
	b, err = commonmulticluster.AddInputWatch(b, local.GetScheme(), targets, &mariadbv1alpha1.MariaDB{},
		mariaDBToNovaMapper(local.GetClient()))
	if err != nil {
		return err
	}

	// Watch both the cluster-scoped ClusterSecretStore and the namespaced
	// SecretStore a Nova can select via spec.secretStoreRef, so the operator
	// reflects upstream secret-backend outages in SecretsReady as soon as ESO flips
	// the selected store's Ready condition.
	b, err = commonmulticluster.AddInputWatch(b, local.GetScheme(), targets, &esov1.ClusterSecretStore{},
		storeToNovaMapper(local.GetClient(), commonv1.SecretStoreKindCluster))
	if err != nil {
		return err
	}
	b, err = commonmulticluster.AddInputWatch(b, local.GetScheme(), targets, &esov1.SecretStore{},
		storeToNovaMapper(local.GetClient(), commonv1.SecretStoreKindNamespaced))
	if err != nil {
		return err
	}

	return b.
		// The default wrapper turns an error matching multicluster.ErrClusterNotFound
		// into a successful reconcile. This operator instead surfaces an
		// unresolvable cluster as a TargetClusterUnavailable condition and requeues,
		// so the wrapper stays off and the error semantics remain byte-identical to
		// the classic builder's.
		WithClusterNotFoundWrapper(false).
		Complete(commonmulticluster.LocalReconciler(r))
}
