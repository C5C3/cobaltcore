// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package controller implements the Cinder, CinderBackend and
// CinderBackupBackend reconcilers.
package controller

import (
	"context"
	"fmt"
	"slices"

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
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	"github.com/c5c3/cobaltcore/internal/common/watch"
	cinderv1alpha1 "github.com/c5c3/cobaltcore/operators/cinder/api/v1alpha1"
	"github.com/c5c3/cobaltcore/operators/cinder/internal/metrics"
)

// subConditionTypes lists the condition types set by the individual Cinder
// sub-reconcilers. The aggregate Ready condition is True only when all of these
// are True. Every parallel-group member (HTTPRoute, HealthCheck, HPA,
// NetworkPolicy) always sets its condition, configured-ready, NotRequired, or
// waiting, so a gateway-less or autoscaling-less cluster still resolves the
// aggregate (the NotRequired paths report True), exactly as the sibling
// operators aggregate their optional conditions.
//
// ExtraConfigHealthy stays out: it reports on an overlay the user owns, is
// informational, and must not depool a Cinder whose API serves fine.
var subConditionTypes = []string{
	"SecretsReady",
	conditionTypeBackendsReady,
	conditionTypeBackupBackendReady,
	"DatabaseReady",
	"SchedulerReady",
	"VolumeServicesReady",
	"BackupServiceReady",
	"DeploymentReady",
	conditionTypeCinderAPIReady,
	"HPAReady",
	conditionTypeNetworkPolicyReady,
	conditionTypeHTTPRouteReady,
	conditionTypeDBPurgeReady,
}

// cinderFinalizer blocks removal of a Cinder CR from etcd until the MariaDB
// Database, User, and Grant CRs it owns have been issued a Delete, so the schema
// teardown is triggered before the owner-ref chain disappears. It is the single
// source of truth for Reconcile, the finalizer handler, and tests.
const cinderFinalizer = "cinder.openstack.c5c3.io/finalizer"

// httpRouteGVK identifies the HTTPRoute kind the operator watches when Gateway
// API is installed. Availability is probed at setup time via the shared
// gateway.IsGVKAvailable RESTMapper probe.
var httpRouteGVK = schema.GroupVersionKind{
	Group:   gatewayv1.GroupVersion.Group,
	Version: gatewayv1.GroupVersion.Version,
	Kind:    "HTTPRoute",
}

// cinderSkeleton bundles the shared controller-skeleton glue (Ready
// aggregation, no-op-skipping status writes, config-failure marking) with
// cinder's sub-condition vocabulary and status accessor. The wrapper helpers
// below delegate to it.
var cinderSkeleton = commonreconcile.Skeleton[*cinderv1alpha1.Cinder, cinderv1alpha1.CinderStatus]{
	SubConditionTypes: subConditionTypes,
	Conditions:        func(c *cinderv1alpha1.Cinder) *[]metav1.Condition { return &c.Status.Conditions },
}

// CinderReconciler reconciles a Cinder object: it drives the sub-reconciler
// chain that projects the API, scheduler, volume and backup workloads together
// with the Secrets, config and database children they read.
type CinderReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// OperatorNamespace is the Namespace the operator Pod runs in (resolved at
	// startup by bootstrap.DetectOperatorNamespace). The networkpolicy step
	// appends an ingress peer for this Namespace so the operator's own health
	// check can reach the Cinder API. Empty when the namespace could not be
	// determined, in which case no operator-namespace peer is added.
	OperatorNamespace string

	// MaxConcurrentReconciles bounds how many Cinder CRs reconcile concurrently.
	// It is threaded from the --max-concurrent-reconciles flag and applied to the
	// controller's controller.Options in SetupWithManager. A value <= 0 falls back
	// to bootstrap.DefaultMaxConcurrentReconciles inside
	// bootstrap.ControllerOptions, so the zero value is safe.
	MaxConcurrentReconciles int

	// HTTPClient is the health-check client seam. Production leaves it nil so the
	// health check uses http.DefaultClient; tests inject a stub transport.
	HTTPClient healthcheck.HTTPDoer

	// Resolver resolves the target cluster a Cinder CR names in
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

	// healthProbeCache memoizes the last successful Cinder API probe per CR
	// (shared TTL probe cache) so a steady-state reconcile does not fire a
	// synchronous HTTP GET on every pass. The cache's internal mutex guards
	// concurrent access under MaxConcurrentReconciles > 1.
	healthProbeCache healthcheck.ProbeCache
}

// conditionReasonConfigError is the SecretsReady=False reason set when
// reconcileConfig fails. Config artefacts (the rendered cinder.conf ConfigMap)
// gate the same downstream graph as the upstream credential Secrets, so failures
// reuse SecretsReady rather than a dedicated condition — matching
// reconcileDBConnectionSecret's Config→SecretsReady mapping.
const conditionReasonConfigError = "ConfigError"

// markConfigFailed flips SecretsReady to False so a reconcileConfig failure
// cannot leave the aggregate Ready condition stale-True at the new
// ObservedGeneration. It mirrors the sibling operators' markConfigFailed helper.
func markConfigFailed(cinder *cinderv1alpha1.Cinder, err error) {
	conditions.SetCondition(&cinder.Status.Conditions, metav1.Condition{
		Type:               "SecretsReady",
		Status:             metav1.ConditionFalse,
		ObservedGeneration: cinder.Generation,
		Reason:             conditionReasonConfigError,
		Message:            err.Error(),
	})
}

// registerCinderIndexes registers the three field indexers this operator relies
// on: the Cinder Secret-name union the Secret watch resolves through, and the
// two satellite parent references the projections list their attached
// CinderBackends and CinderBackupBackends by. It is the single registration site
// for the operator: main.go and the envtest helper set the Cinder reconciler up
// before the two satellite ones, so all three controllers find the indexes in
// place.
func registerCinderIndexes(ctx context.Context, indexer client.FieldIndexer) error {
	if err := watch.RegisterSecretNameIndex(ctx, indexer, &cinderv1alpha1.Cinder{},
		CinderSecretNameIndexKey, cinderSecretNameExtractor); err != nil {
		return err
	}
	if err := watch.RegisterParentRefIndex(ctx, indexer, &cinderv1alpha1.CinderBackend{},
		CinderBackendCinderRefIndexKey, cinderBackendParentName); err != nil {
		return err
	}
	return watch.RegisterParentRefIndex(ctx, indexer, &cinderv1alpha1.CinderBackupBackend{},
		CinderBackupBackendCinderRefIndexKey, cinderBackupBackendParentName)
}

// CinderRemoteChildKinds are the kinds a Cinder CR projects into the namespace
// of the target cluster it names, and the kinds reconcileDeleteRemoteChildren
// sweeps by ownership label when that CR is deleted. Nothing on the target
// cluster collects them, so a kind missing from this list is a kind that keeps
// running after its CR is gone.
//
// HTTPRoute is listed although the kind is optional: a target cluster without
// Gateway API answers the sweep's list with a no-match, which
// commonmulticluster.DeleteRemoteChildren skips, and the watch leg is likewise
// engaged only on the clusters that serve the kind. Leaving it out instead would
// strand the routes of every cluster that does have Gateway API.
var CinderRemoteChildKinds = []schema.GroupVersionKind{
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

// The operator never creates a Cinder; it reads them, patches their status and
// holds them with a finalizer.
// +kubebuilder:rbac:groups=cinder.openstack.c5c3.io,resources=cinders,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=cinder.openstack.c5c3.io,resources=cinders/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cinder.openstack.c5c3.io,resources=cinders/finalizers,verbs=update
// Users create the two satellite kinds; the operator lists and watches them to
// assemble the aggregate backend configuration, and updates a CinderBackend to
// release the service-remove finalizer it detaches under.
// +kubebuilder:rbac:groups=cinder.openstack.c5c3.io,resources=cinderbackends;cinderbackupbackends,verbs=get;list;watch;update
// +kubebuilder:rbac:groups=cinder.openstack.c5c3.io,resources=cinderbackends/status;cinderbackupbackends/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=services;configmaps;secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
// Required to read the termination message a finished migration Job left on its
// Pod, which carries the upgrade-check findings the operator reports as events.
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list
// deployments carry the API, the scheduler, one cinder-volume per attached
// backend and the backup service.
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// jobs covers the db-sync, the expand/migrate/contract phases and the
// service-remove run of a detaching backend; cronjobs covers the recurring
// database purge.
// +kubebuilder:rbac:groups=batch,resources=jobs;cronjobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=k8s.mariadb.com,resources=databases;users;grants,verbs=get;list;watch;create;update;patch;delete
// Required for the operator to observe the referenced MariaDB cluster's
// Ready condition and reflect outages in DatabaseReady.
// +kubebuilder:rbac:groups=k8s.mariadb.com,resources=mariadbs,verbs=get;list;watch
// In managed messaging mode the transport URL is derived from the named
// RabbitmqCluster instead of read from a Secret, so the operator reads and
// watches the cluster.
// +kubebuilder:rbac:groups=rabbitmq.com,resources=rabbitmqclusters,verbs=get;list;watch
// The database, service-user and messaging credential Secrets are ESO-managed;
// the operator only reads the ExternalSecrets to attribute a not-synced Secret
// in SecretsReady messages.
// +kubebuilder:rbac:groups=external-secrets.io,resources=externalsecrets,verbs=get;list;watch
// Required so the operator can observe the selected store's Ready condition and
// reflect upstream secret-backend outages in SecretsReady. A CR selects either
// the shared cluster-scoped ClusterSecretStore (default) or a namespaced
// SecretStore, so both kinds must be watchable.
// +kubebuilder:rbac:groups=external-secrets.io,resources=clustersecretstores;secretstores,verbs=get;list;watch
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// Required to create/update/delete HTTPRoutes that expose the Cinder API
// externally.
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch;create;update;patch;delete
// Required so the operator can observe the Accepted condition set by the
// upstream Gateway controller and reflect it in HTTPRouteReady.
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes/status,verbs=get
// Required for the webhook to validate that spec.priorityClassName references
// an existing PriorityClass at admission time.
// +kubebuilder:rbac:groups=scheduling.k8s.io,resources=priorityclasses,verbs=get;list;watch

// Reconcile is the main reconciliation loop for the Cinder CR. It fetches the
// CR, drives the finalizer-gated deletion path, ensures the finalizers, then
// runs the sub-reconciler pipeline. Every exit funnels through updateStatus,
// which re-aggregates the Ready condition and stamps ObservedGeneration.
func (r *CinderReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var cinder cinderv1alpha1.Cinder
	if err := r.Get(ctx, req.NamespacedName, &cinder); err != nil {
		if apierrors.IsNotFound(err) {
			log.FromContext(ctx).V(1).Info("Cinder resource not found; likely deleted")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching Cinder: %w", err)
	}

	// Handle deletion via the finalizer: issue Delete on the MariaDB CRs, then
	// release the finalizer once no live (not-yet-deleted) resource remains. The
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
	if !cinder.DeletionTimestamp.IsZero() {
		children, wait := commonmulticluster.ResolveChildrenClientForDeletion(
			ctx, r.Resolver, r.Client, cinder.Spec.TargetClusterRef, *cinder.DeletionTimestamp)
		if wait {
			// The hold goes on the CR, not only into the operator's log. It is a
			// deliberate state a CR can sit in for minutes, and "Terminating,
			// waiting on the target cluster" has to be distinguishable from a
			// wedged finalizer without correlating logs across replicas. This exit
			// precedes the pipeline's status snapshot below, so it takes its own
			// baseline for the skip-unchanged write.
			statusBefore := cinder.Status.DeepCopy()
			cinderSkeleton.MarkFailed(&cinder, "SecretsReady",
				commonmulticluster.TargetClusterUnavailable,
				fmt.Errorf("target cluster %s does not resolve; waiting at least %s before abandoning its children",
					cinder.Spec.TargetClusterRef.Name, commonmulticluster.AbandonAfter))
			return r.updateStatus(ctx, &cinder, statusBefore,
				ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, nil)
		}
		if result, err := r.reconcileDelete(ctx, children, &cinder); !result.IsZero() || err != nil {
			return result, err
		}
		// The label-selected sweep runs after the named MariaDB cleanup, never
		// before it: that flow waits one pass on the CRs it deletes by name, and a
		// sweep running first would delete them out from under it. The ordering
		// mirrors the local one, where the garbage collection cascade starts only
		// once every finalizer has been released.
		if err := r.reconcileDeleteRemoteChildren(ctx, children, &cinder); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Resolve the client every child object of this CR is read and written with.
	// The embedded client stays on the management cluster (the CR, its status, its
	// finalizers and the two satellite kinds live there); children carries
	// everything the CR projects into the target cluster. The resolution runs
	// before the finalizer is added so a CR naming an unresolvable cluster stays
	// clean of finalizers: nothing was created for it, so there is nothing to
	// clean up, and a finalizer would only block its deletion.
	children, err := commonmulticluster.ResolveChildrenClient(ctx, r.Resolver, r.Client, cinder.Spec.TargetClusterRef)
	if err != nil {
		// This exit precedes the pipeline's status snapshot below, so it takes its
		// own baseline for the skip-unchanged write.
		statusBefore := cinder.Status.DeepCopy()
		cinderSkeleton.MarkFailed(&cinder, "SecretsReady", commonmulticluster.TargetClusterUnavailable, err)
		return r.updateStatus(ctx, &cinder, statusBefore,
			ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, nil)
	}

	// Ensure the finalizer is installed before any sub-reconciler runs so a
	// deletion issued before the next pass still funnels through reconcileDelete.
	// Requeuing after the Update guarantees the next reconcile observes the
	// persisted finalizer rather than the in-memory copy.
	if added, err := commonreconcile.EnsureFinalizer(ctx, r.Client, &cinder, cinderFinalizer); err != nil {
		return ctrl.Result{}, err
	} else if added {
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueNextPass}, nil
	}

	// The remote-children finalizer goes on only when the CR projects onto a
	// target cluster. A local CR keeps the garbage collection cascade, which reaps
	// its children from their owner references, so it has nothing for this
	// finalizer to hold the CR open for. spec.targetClusterRef is immutable, so
	// the condition cannot flip under a live CR.
	if cinder.Spec.TargetClusterRef != nil {
		if added, err := commonreconcile.EnsureFinalizer(ctx, r.Client, &cinder,
			commonmulticluster.RemoteChildrenFinalizer); err != nil {
			return ctrl.Result{}, err
		} else if added {
			return ctrl.Result{RequeueAfter: commonreconcile.RequeueNextPass}, nil
		}
	}

	// Snapshot the persisted status so updateStatus can skip the write when a pass
	// leaves status unchanged (no write → no watch event → no resourceVersion
	// churn). Taken after the finalizer add so an early requeue there does not
	// race a status write.
	statusBefore := cinder.Status.DeepCopy()

	result, err := commonreconcile.RunPipeline(ctx, instrumenter.Instrument,
		r.pipelineSteps(children, &cinder, &pipelineState{}))
	return r.updateStatus(ctx, &cinder, statusBefore, result, err)
}

// pipelineState carries the values one sub-reconciler hands to a later one
// within a single reconcile pass. It is filled in step order: the three digests
// and the broker port by the credential steps, the two projections by the
// backend steps, and the config artefacts by the render step. Every field keeps
// its zero value on the waiting and error paths of the step that produces it,
// where the downstream steps either omit what it would have configured or wait
// themselves.
type pipelineState struct {
	// authTokenDigest is the SHA-256 of the service-user password, dsnDigest the
	// one of the assembled DSN, transportDigest the one of the transport URL. The
	// workload steps stamp them into pod-template annotations so a rotated
	// credential rolls the pods.
	authTokenDigest string
	dsnDigest       string
	transportDigest string
	// egressPort is the broker's TCP port the networkpolicy member opens.
	egressPort int32
	// backends are the projected volume backends and shareHosts the NFS exports
	// they mount, one tcp:// URL per backend.
	backends   []backendProjection
	shareHosts []string
	// backup is the projected backup target, nil when no backup service renders.
	backup *backupProjection
	// art names the rendered config ConfigMap and its data keys.
	art configArtifacts
}

// digests bundles the three content digests the workload steps stamp into their
// pod templates.
func (s *pipelineState) digests() workloadDigests {
	return workloadDigests{
		dsn:       s.dsnDigest,
		authToken: s.authTokenDigest,
		transport: s.transportDigest,
	}
}

// policyShareHosts returns the NFS exports the networkpolicy member opens egress
// to: the volume backends' shares plus the backup target's, because the backup
// service mounts its own export alongside the volumes it reads and a Cinder that
// only backs up would otherwise get no export rule at all.
//
// The repeated host a shared NFS server produces needs no deduplication:
// networkpolicy.HostPortsEgressRule keys the rule on the distinct ports it
// parses out of these URLs and leaves the destination unrestricted, so every
// export contributes the same single 2049 port whatever its host.
func (s *pipelineState) policyShareHosts() []string {
	if s.backup == nil {
		return s.shareHosts
	}
	return append(slices.Clone(s.shareHosts),
		fmt.Sprintf("tcp://%s:%d", s.backup.server, nfsEgressPort))
}

// pipelineSteps returns the ordered sub-reconciler pipeline for one Cinder. Each
// step runs in dependency order; the first to return a non-zero result or an
// error short-circuits the chain and funnels through updateStatus. state is the
// pass-local scratch space the steps hand their outputs along in.
//
// It is a method rather than a literal inside Reconcile so the drift guard can
// enumerate the step names without running a reconcile.
func (r *CinderReconciler) pipelineSteps(children client.Client, cinder *cinderv1alpha1.Cinder,
	state *pipelineState,
) []commonreconcile.Step {
	return []commonreconcile.Step{
		{Name: "Secrets", Fn: func(ctx context.Context) (res ctrl.Result, err error) {
			res, state.authTokenDigest, err = r.reconcileSecrets(ctx, children, cinder)
			return res, err
		}},
		// reconcileDBConnectionSecret materialises the DB URL into the derived
		// <cinder.Name>-db-connection Secret. It runs after Secrets (upstream
		// credentials must be synced) and before Config; failures set
		// SecretsReady=False, the same condition reconcileSecrets uses.
		{Name: "DBConnectionSecret", Fn: func(ctx context.Context) (res ctrl.Result, err error) {
			res, state.dsnDigest, err = r.reconcileDBConnectionSecret(ctx, children, cinder)
			return res, err
		}},
		// reconcileTransportURLSecret materialises the rabbit:// URL into the
		// derived <cinder.Name>-transport-url Secret. It also reports the broker
		// port the networkpolicy member opens as an egress peer.
		{Name: "TransportURLSecret", Fn: func(ctx context.Context) (res ctrl.Result, err error) {
			res, state.transportDigest, state.egressPort, err = r.reconcileTransportURLSecret(ctx, children, cinder)
			return res, err
		}},
		// reconcileBackends projects every attached, credential-ready CinderBackend
		// into its own Secret. Waiting states NEVER short-circuit the pipeline: the
		// step returns a zero result so a Cinder without backends still converges,
		// and a backend status flip re-enqueues this Cinder through the satellite
		// watch.
		{Name: "Backends", Fn: func(ctx context.Context) (res ctrl.Result, err error) {
			res, state.backends, state.shareHosts, err = r.reconcileBackends(ctx, children, cinder)
			return res, err
		}},
		// reconcileBackupBackend projects the single attached backup target under
		// the same never-short-circuit contract as Backends.
		{Name: "BackupBackend", Fn: func(ctx context.Context) (res ctrl.Result, err error) {
			res, state.backup, err = r.reconcileBackupBackend(ctx, children, cinder)
			return res, err
		}},
		// reconcileConfig renders cinder.conf and scheduler.conf into an immutable
		// ConfigMap. It self-marks SecretsReady=False on failure via
		// markConfigFailed, so the wrapper only threads the result.
		{Name: "Config", Fn: func(ctx context.Context) (res ctrl.Result, err error) {
			res, state.art, err = r.reconcileConfig(ctx, children, cinder)
			return res, err
		}},
		// reconcileDatabase provisions the schema, gates the requested OpenStack
		// release against the installed one, and runs the migration Jobs against
		// the rendered config. The projected backend count sizes the database
		// user's connection cap, because each backend runs its own cinder-volume.
		{Name: "Database", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileDatabase(ctx, children, cinder, state.art.configMapName, volumeDeploymentCount(state.backends))
		}},
		// reconcileScheduler projects the cinder-scheduler Deployment. It runs
		// after Database so the process only starts once the schema it queries
		// exists.
		{Name: "Scheduler", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileScheduler(ctx, children, cinder, state.art, state.digests(), state.egressPort)
		}},
		// reconcileVolumeServices projects one cinder-volume Deployment per
		// projected backend and drives the detach of the backends that left.
		{Name: "VolumeServices", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileVolumeServices(ctx, children, cinder, state.backends, state.art,
				state.digests(), state.egressPort)
		}},
		// reconcileBackupService projects the cinder-backup Deployment, or removes
		// it when no backup target is attached. It takes the volume backends too:
		// the backup pod mounts their exports to read the volumes it backs up.
		{Name: "BackupService", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileBackupService(ctx, children, cinder, state.backup, state.backends,
				state.art, state.digests(), state.egressPort)
		}},
		// reconcileDeployment projects the API Deployment, its Service and the
		// PodDisruptionBudget, and stamps status.endpoint.
		{Name: "Deployment", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileDeployment(ctx, children, cinder, state.art, state.digests())
		}},
		// reconcileDBPurge projects the recurring purge CronJob. It runs BEFORE the
		// parallel group rather than inside it because the group runs behind the
		// whole chain, and the purge only needs the rendered config the Config step
		// above produced.
		{Name: "DBPurge", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileDBPurge(ctx, children, cinder, state.art)
		}},
		// Once the workload outputs are in place, HTTPRoute, HealthCheck, HPA and
		// NetworkPolicy have no inter-dependency and run concurrently. Each member
		// sets exactly one condition type; the group self-instruments its members,
		// so this step carries no sub_reconciler name.
		{Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileParallelGroup(ctx, cinder, r.parallelSteps(children, state))
		}},
	}
}

// volumeDeploymentCount returns how many cinder-volume Deployments the projected
// backends amount to, as the connection-cap sizing expects it. The count is
// bounded by the CinderBackends attached to one Cinder, so it never approaches
// the int32 range the cap arithmetic works in.
func volumeDeploymentCount(backends []backendProjection) int32 {
	return int32(len(backends)) //nolint:gosec // G115: the count is bounded by the CinderBackends attached to one Cinder.
}

// parallelSteps returns the members of the post-deployment parallel group. Each
// member sets exactly one condition type and receives its own copy of the CR, so
// none of them reads a value another one produces. state carries the outputs the
// earlier steps produced in the same pass.
//
// It is a method rather than a literal inside pipelineSteps so the drift guard
// can enumerate the member names and their condition types without running a
// reconcile.
func (r *CinderReconciler) parallelSteps(children client.Client,
	state *pipelineState,
) []commonreconcile.ParallelStep[*cinderv1alpha1.Cinder] {
	return []commonreconcile.ParallelStep[*cinderv1alpha1.Cinder]{
		{
			Name:          "HTTPRoute",
			ConditionType: conditionTypeHTTPRouteReady,
			Fn: func(ctx context.Context, c *cinderv1alpha1.Cinder) (ctrl.Result, error) {
				return r.reconcileHTTPRoute(ctx, children, c)
			},
		},
		{
			Name:          "HealthCheck",
			ConditionType: conditionTypeCinderAPIReady,
			Fn: func(ctx context.Context, c *cinderv1alpha1.Cinder) (ctrl.Result, error) {
				return r.reconcileHealthCheck(ctx, c)
			},
		},
		{
			Name:          "HPA",
			ConditionType: "HPAReady",
			Fn: func(ctx context.Context, c *cinderv1alpha1.Cinder) (ctrl.Result, error) {
				return r.reconcileHPA(ctx, children, c)
			},
		},
		{
			Name:          "NetworkPolicy",
			ConditionType: conditionTypeNetworkPolicyReady,
			Fn: func(ctx context.Context, c *cinderv1alpha1.Cinder) (ctrl.Result, error) {
				return r.reconcileNetworkPolicy(ctx, children, c, state.egressPort, state.policyShareHosts())
			},
		},
	}
}

// reconcileParallelGroup runs the given sub-reconcilers concurrently, delegating
// to the shared skeleton: each member operates on its own DeepCopy of the Cinder
// CR, conditions from every member (including those that succeeded before a peer
// failed) are merged back into the primary cinder, and on success the shortest
// non-zero RequeueAfter is returned. Members instrument individually via
// instrumenter.Instrument.
func (r *CinderReconciler) reconcileParallelGroup(
	ctx context.Context,
	cinder *cinderv1alpha1.Cinder,
	subs []commonreconcile.ParallelStep[*cinderv1alpha1.Cinder],
) (ctrl.Result, error) {
	return cinderSkeleton.RunParallelGroup(ctx, cinder, instrumenter.Instrument, subs)
}

// reconcileDelete drives the finalizer cleanup when the Cinder CR is being
// deleted. It is a no-op when the finalizer is absent. Otherwise it issues Delete
// on the MariaDB Database/User/Grant CRs (idempotent, NotFound-tolerant) and,
// while at least one of them was still live (not yet issued a Delete), holds the
// finalizer for one more pass so the schema teardown is triggered before the
// owner-ref chain disappears. Once no live resource remains it drops the per-CR
// metrics, evicts the health-probe cache, and releases the finalizer.
//
// A nil children client means the target cluster this CR named is no longer
// registered. Its MariaDB CRs cannot be reached, so they stay behind on a cluster
// that has not resolved for the whole abandon window, and the finalizer is
// released anyway: holding it would only strand the CR in Terminating.
func (r *CinderReconciler) reconcileDelete(ctx context.Context, children client.Client,
	cinder *cinderv1alpha1.Cinder,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(cinder, cinderFinalizer) {
		return ctrl.Result{}, nil
	}

	key := client.ObjectKey{Name: cinder.Name, Namespace: cinder.Namespace}

	if children == nil {
		r.Recorder.Event(cinder, corev1.EventTypeWarning, "RemoteChildrenAbandoned",
			"Target cluster is no longer registered; releasing the finalizer without deleting the MariaDB Database, User, and Grant on it")
	} else {
		// Observe whether any MariaDB CR is still live BEFORE issuing the Delete: a
		// Delete flips DeletionTimestamp, so a post-Delete check would always report
		// none-live and release immediately. Gating on the pre-Delete observation
		// keeps the CR alive one extra pass so the teardown is actually triggered.
		hasLive, err := database.HasLiveResources(ctx, children, key)
		if err != nil {
			return ctrl.Result{}, err
		}

		if err := database.FinalizeResources(ctx, children, key); err != nil {
			return ctrl.Result{}, err
		}

		if hasLive {
			r.Recorder.Event(cinder, corev1.EventTypeNormal, "FinalizingDatabase",
				"Cleaning up MariaDB Database, User, and Grant before removing Cinder")
			return ctrl.Result{RequeueAfter: RequeueDatabaseWait}, nil
		}

		r.Recorder.Event(cinder, corev1.EventTypeNormal, "DatabaseFinalized",
			"MariaDB Database, User, and Grant marked for deletion; releasing finalizer")
	}

	controllerutil.RemoveFinalizer(cinder, cinderFinalizer)
	if err := r.Update(ctx, cinder); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}
	metrics.DeleteForCinder(cinder.Name, cinder.Namespace)
	// Drop the per-CR health-probe cache so a CR recreated under the same
	// name/namespace never serves a stale probe keyed on the deleted CR's UID.
	r.healthProbeCache.Evict(key)
	return ctrl.Result{}, nil
}

// reconcileDeleteRemoteChildren deletes everything this Cinder projected onto the
// target cluster it names and releases the remote-children finalizer, as
// commonmulticluster.SweepRemoteChildren documents. reconcileDelete above deletes
// the three MariaDB CRs it tracks by name; this pass is what reaches the rest,
// selected on the ownership labels Claim stamped on them.
func (r *CinderReconciler) reconcileDeleteRemoteChildren(ctx context.Context, children client.Client,
	cinder *cinderv1alpha1.Cinder,
) error {
	return commonmulticluster.SweepRemoteChildren(ctx, r.Client, r.Resolver, r.Recorder, r.Scheme,
		cinder, cinder.Spec.TargetClusterRef, children, CinderRemoteChildKinds)
}

// updateStatus persists the current status conditions and returns the given
// result and error, delegating to the shared skeleton: the write is skipped when
// the pass left status semantically unchanged from the statusBefore snapshot, a
// failed write is joined with reconcileErr, and the mutate hook re-aggregates the
// Ready condition on every persist and stamps status.observedGeneration.
func (r *CinderReconciler) updateStatus(ctx context.Context, cinder *cinderv1alpha1.Cinder,
	statusBefore *cinderv1alpha1.CinderStatus, result ctrl.Result, reconcileErr error,
) (ctrl.Result, error) {
	return cinderSkeleton.UpdateStatus(ctx, r.Client, cinder, statusBefore, &cinder.Status, func() {
		cinder.Status.ObservedGeneration = cinder.Generation
	}, result, reconcileErr)
}

// setReadyCondition sets the aggregate Ready condition based on all
// sub-conditions, delegating to the shared skeleton with cinder's sub-condition
// vocabulary.
func setReadyCondition(cinder *cinderv1alpha1.Cinder) {
	cinderSkeleton.SetReady(cinder)
}

// SetupWithManager registers the CinderReconciler with the controller manager.
// The shared controller options it applies let independent CRs reconcile in
// parallel instead of serialising at the controller-runtime default of 1, and the
// tuned RateLimiter caps per-item failure backoff at 30s rather than the default
// 1000s (see bootstrap.TypedControllerOptions).
func (r *CinderReconciler) SetupWithManager(mgr mcmanager.Manager) error {
	return r.setupWithOptions(mgr, bootstrap.TypedControllerOptions[mcreconcile.Request](r.MaxConcurrentReconciles))
}

// setupWithOptions carries the production watch wiring SetupWithManager applies.
// The controller options are a parameter so an envtest integration suite can
// register this exact chain with SkipNameValidation set, rather than a hand-built
// copy of it that drifts the moment a leg is added here.
func (r *CinderReconciler) setupWithOptions(mgr mcmanager.Manager, opts crcontroller.TypedOptions[mcreconcile.Request]) error {
	local := mgr.GetLocalManager()

	// Detect whether the Gateway API CRD is installed. spec.gateway is optional,
	// so the operator must run on clusters without Gateway API. Adding
	// Owns(HTTPRoute) unconditionally would fail at Start with "no matches for
	// kind HTTPRoute" when the CRD is missing, blocking every Cinder CR.
	r.gatewayAPIAvailable = gateway.IsGVKAvailable(local.GetRESTMapper(), httpRouteGVK)
	setupLog := ctrl.Log.WithName("cinder-setup")
	if r.gatewayAPIAvailable {
		setupLog.Info("Gateway API detected; enabling HTTPRoute watch and reconciliation")
	} else {
		setupLog.Info("Gateway API not installed; HTTPRoute watch disabled, spec.gateway will be rejected via HTTPRouteReady condition")
	}

	// Register the field indexers before Watches so the Secret mapper and the two
	// satellite fan-outs can rely on them for their MatchingFields lookups. This
	// is the single registration site for all three controllers of the operator.
	// The indexes go on the LOCAL field indexer, not mgr's: with a provider
	// configured, the multicluster manager's field indexer registers against the
	// provider clusters, which hold no Cinder CR. Registration stays local by
	// contract: the indexes are on CR kinds, which exist on the management cluster
	// alone, and every request the watches emit is pinned to that cluster
	// (LocalRequests / RemoteRequests, internal/common/multicluster/watch.go), so a
	// remote event resolves to its CR through the local cache. Registering on the
	// fleet would fail the engagement of every target cluster, because the
	// kubeconfig provider applies its stored indexes while engaging one.
	if err := registerCinderIndexes(context.Background(), local.GetFieldIndexer()); err != nil {
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
		func(cinder *cinderv1alpha1.Cinder) *commonv1.TargetClusterRefSpec {
			return cinder.Spec.TargetClusterRef
		})

	b := mcbuilder.ControllerManagedBy(mgr).
		WithOptions(opts).
		// Filter the CR's own status-only updates so Status().Update does not
		// re-wake the controller (see watch.CRUpdatePredicate).
		For(&cinderv1alpha1.Cinder{}, mcbuilder.WithPredicates(watch.CRUpdatePredicate()), engageLocal, engageNoProviders).
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
	b, err := commonmulticluster.AddRemoteChildWatches(b, local.GetScheme(), &cinderv1alpha1.Cinder{},
		targets, CinderRemoteChildKinds, nil)
	if err != nil {
		return err
	}

	// Watch Secrets and map to the Cinder CRs that reference them by name or own
	// them. ESO-managed Secrets are owned by the ExternalSecret controller, not the
	// Cinder CR, so EnqueueRequestForOwner would never match them.
	b, err = commonmulticluster.AddInputWatch(b, local.GetScheme(), targets, &corev1.Secret{},
		secretToCinderMapper(local.GetClient()))
	if err != nil {
		return err
	}

	// Watch the MariaDB cluster CR referenced by spec.database.clusterRef so the
	// operator reflects upstream database outages in DatabaseReady without waiting
	// for the next periodic requeue.
	b, err = commonmulticluster.AddInputWatch(b, local.GetScheme(), targets, &mariadbv1alpha1.MariaDB{},
		mariaDBToCinderMapper(local.GetClient()))
	if err != nil {
		return err
	}

	// Watch both the cluster-scoped ClusterSecretStore and the namespaced
	// SecretStore a Cinder can select via spec.secretStoreRef, so the operator
	// reflects upstream secret-backend outages in SecretsReady as soon as ESO flips
	// the selected store's Ready condition.
	b, err = commonmulticluster.AddInputWatch(b, local.GetScheme(), targets, &esov1.ClusterSecretStore{},
		storeToCinderMapper(local.GetClient(), commonv1.SecretStoreKindCluster))
	if err != nil {
		return err
	}
	b, err = commonmulticluster.AddInputWatch(b, local.GetScheme(), targets, &esov1.SecretStore{},
		storeToCinderMapper(local.GetClient(), commonv1.SecretStoreKindNamespaced))
	if err != nil {
		return err
	}

	return b.
		// Watch the two satellite kinds and map them to their parent Cinder:
		// their status flips (CredentialsReady turning True) trigger projection,
		// DeletionTimestamp flips trigger de-projection. No generation predicate:
		// the status transitions ARE the signal. Both legs stay local-only, because
		// a satellite is a management-plane CR and lives nowhere else.
		Watches(&cinderv1alpha1.CinderBackend{}, commonmulticluster.LocalRequests(
			cinderBackendToCinderMapper(),
		), engageLocal, engageNoProviders).
		Watches(&cinderv1alpha1.CinderBackupBackend{}, commonmulticluster.LocalRequests(
			cinderBackupBackendToCinderMapper(),
		), engageLocal, engageNoProviders).
		// The default wrapper turns an error matching multicluster.ErrClusterNotFound
		// into a successful reconcile. This operator instead surfaces an
		// unresolvable cluster as a TargetClusterUnavailable condition and requeues,
		// so the wrapper stays off and the error semantics remain byte-identical to
		// the classic builder's.
		WithClusterNotFoundWrapper(false).
		Complete(commonmulticluster.LocalReconciler(r))
}
