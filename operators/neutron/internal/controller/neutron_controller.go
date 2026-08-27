// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package controller implements the Neutron and NeutronMetadataAgent
// reconcilers.
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
	"github.com/c5c3/cobaltcore/internal/common/database"
	"github.com/c5c3/cobaltcore/internal/common/gateway"
	"github.com/c5c3/cobaltcore/internal/common/healthcheck"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	"github.com/c5c3/cobaltcore/internal/common/watch"
	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
	neutronmetrics "github.com/c5c3/cobaltcore/operators/neutron/internal/metrics"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// NeutronSecretNameIndexKey is the field-indexer key under which Neutron CRs are
// indexed by the union of their referenced Secret names
// (spec.database.secretRef.name, spec.serviceUser.secretRef.name and
// spec.messaging.secretRef.name). Used by setupWithOptions to register the
// indexer and by the Secret watch mapper to perform an O(1) reverse lookup
// instead of an unfiltered List of all Neutron CRs in the namespace.
// #nosec G101 -- field-indexer key (a JSONPath-like field selector), not a credential.
const NeutronSecretNameIndexKey = "spec.secretRefs.name"

// NeutronOVNCentralRefIndexKey is the field-indexer key under which Neutron CRs
// are indexed by the OVNCentral they drive. The indexed value is
// "<namespace>/<name>", not the bare name: spec.ovn.centralRef carries a
// namespace, because the OVN control plane commonly lives in the privileged
// networking namespace while the Neutron API lives with the rest of the control
// plane, so a bare name would collide across namespaces.
const NeutronOVNCentralRefIndexKey = "spec.ovn.centralRef"

// neutronSecretNameExtractor is the controller-runtime IndexerFunc registered
// under NeutronSecretNameIndexKey. It returns the deduplicated, non-empty union
// of Secret names a Neutron CR references, so the field indexer can resolve a
// Secret event to the referencing CR(s) without listing every Neutron in the
// namespace. spec.messaging.secretRef is nil in managed mode, where the transport
// URL is derived from a RabbitmqCluster instead of read from a Secret.
func neutronSecretNameExtractor(obj client.Object) []string {
	neutron, ok := obj.(*neutronv1alpha1.Neutron)
	if !ok {
		// controller-runtime should never call us with the wrong type; a nil
		// return is safer than a panic if it ever does.
		return nil
	}

	referenced := []string{
		neutron.Spec.Database.SecretRef.Name,
		neutron.Spec.ServiceUser.SecretRef.Name,
	}
	if neutron.Spec.Messaging.SecretRef != nil {
		referenced = append(referenced, neutron.Spec.Messaging.SecretRef.Name)
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

// ovnCentralRefIndexValue renders the value neutronOVNCentralRefExtractor
// indexes under and the two watch mappers look up. It is one function so the
// index and its readers cannot drift apart on the separator or the namespace
// fallback.
func ovnCentralRefIndexValue(namespace, name string) string {
	return namespace + "/" + name
}

// neutronOVNCentralRefExtractor is the IndexerFunc registered under
// NeutronOVNCentralRefIndexKey. spec.ovn.centralRef.name is required, so a
// Neutron indexes under exactly one central; an omitted namespace resolves to
// the Neutron's own, which is how the endpoint step resolves it too.
func neutronOVNCentralRefExtractor(obj client.Object) []string {
	neutron, ok := obj.(*neutronv1alpha1.Neutron)
	if !ok || neutron.Spec.OVN.CentralRef.Name == "" {
		// controller-runtime should never call us with the wrong type; a nil
		// return is safer than a panic if it ever does.
		return nil
	}
	namespace := neutron.Spec.OVN.CentralRef.Namespace
	if namespace == "" {
		namespace = neutron.Namespace
	}
	return []string{ovnCentralRefIndexValue(namespace, neutron.Spec.OVN.CentralRef.Name)}
}

// registerNeutronIndexes registers the two Neutron field indexers. It is the
// single registration site for this operator: main.go and the envtest helper set
// the Neutron reconciler up before the NeutronMetadataAgent one, so both find
// the indexes in place. The errors are wrapped with the index key so the
// registration site is identifiable in manager-startup failure logs.
func registerNeutronIndexes(ctx context.Context, indexer client.FieldIndexer) error {
	if err := watch.RegisterSecretNameIndex(ctx, indexer, &neutronv1alpha1.Neutron{},
		NeutronSecretNameIndexKey, neutronSecretNameExtractor); err != nil {
		return err
	}
	if err := indexer.IndexField(ctx, &neutronv1alpha1.Neutron{}, NeutronOVNCentralRefIndexKey,
		neutronOVNCentralRefExtractor); err != nil {
		return fmt.Errorf("registering field indexer %q: %w", NeutronOVNCentralRefIndexKey, err)
	}
	return nil
}

// subConditionTypes lists the condition types set by the individual Neutron
// sub-reconcilers. The aggregate Ready condition is True only when all of these
// are True. Every parallel-group member (HTTPRoute, HealthCheck, HPA,
// NetworkPolicy) always sets its condition, configured-ready, NotRequired, or
// waiting, so a gateway-less or autoscaling-less cluster still resolves the
// aggregate (the NotRequired paths report True), exactly as the sibling
// operators aggregate their optional conditions.
var subConditionTypes = []string{
	"SecretsReady",
	conditionTypeOVNEndpointsReady,
	"DatabaseReady",
	"DeploymentReady",
	conditionTypeWorkersReady,
	conditionTypeNeutronAPIReady,
	"HPAReady",
	conditionTypeNetworkPolicyReady,
	conditionTypeHTTPRouteReady,
	conditionTypeOVNDBSyncReady,
}

// neutronFinalizer blocks removal of a Neutron CR from etcd until the MariaDB
// Database, User, and Grant CRs it owns have been issued a Delete, so the schema
// teardown is triggered before the owner-ref chain disappears. It is the single
// source of truth for Reconcile, the finalizer handler, and tests.
const neutronFinalizer = "neutron.openstack.c5c3.io/finalizer"

// httpRouteGVK identifies the HTTPRoute kind the operator watches when Gateway
// API is installed. Availability is probed at setup time via the shared
// gateway.IsGVKAvailable RESTMapper probe.
var httpRouteGVK = schema.GroupVersionKind{
	Group:   gatewayv1.GroupVersion.Group,
	Version: gatewayv1.GroupVersion.Version,
	Kind:    "HTTPRoute",
}

// neutronSkeleton bundles the shared controller-skeleton glue (Ready
// aggregation, no-op-skipping status writes, config-failure marking) with
// neutron's sub-condition vocabulary and status accessor. The wrapper helpers
// below delegate to it.
var neutronSkeleton = commonreconcile.Skeleton[*neutronv1alpha1.Neutron, neutronv1alpha1.NeutronStatus]{
	SubConditionTypes: subConditionTypes,
	Conditions:        func(n *neutronv1alpha1.Neutron) *[]metav1.Condition { return &n.Status.Conditions },
}

// NeutronReconciler reconciles a Neutron object. Its fields mirror the sibling
// service reconcilers' core set.
type NeutronReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// OperatorNamespace is the Namespace the operator Pod runs in (resolved at
	// startup by bootstrap.DetectOperatorNamespace). The networkpolicy step
	// appends an ingress peer for this Namespace so the operator's own health
	// check can reach the Neutron API. Empty when the namespace could not be
	// determined, in which case no operator-namespace peer is added.
	OperatorNamespace string

	// MaxConcurrentReconciles bounds how many Neutron CRs reconcile concurrently.
	// It is threaded from the --max-concurrent-reconciles flag and applied to the
	// controller's controller.Options in SetupWithManager. A value <= 0 falls back
	// to bootstrap.DefaultMaxConcurrentReconciles inside
	// bootstrap.ControllerOptions, so the zero value is safe.
	MaxConcurrentReconciles int

	// HTTPClient is the health-check client seam. Production leaves it nil so the
	// health check uses http.DefaultClient; tests inject a stub transport.
	HTTPClient healthcheck.HTTPDoer

	// Resolver resolves the target cluster a Neutron CR names in
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

	// healthProbeCache memoizes the last successful Neutron API probe per CR
	// (shared TTL probe cache) so a steady-state reconcile does not fire a
	// synchronous HTTP GET on every pass. The cache's internal mutex guards
	// concurrent access under MaxConcurrentReconciles > 1.
	healthProbeCache healthcheck.ProbeCache
}

// NeutronRemoteChildKinds are the kinds a Neutron CR projects into the namespace
// of the target cluster it names, and the kinds reconcileDeleteRemoteChildren
// sweeps by ownership label when that CR is deleted. Nothing on the target
// cluster collects them, so a kind missing from this list is a kind that keeps
// running after its CR is gone.
//
// The list is cross-checked against the create verbs of the kubebuilder RBAC
// markers on this controller below: the operator can only leave behind what it
// is allowed to create.
var NeutronRemoteChildKinds = []schema.GroupVersionKind{
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

// Reconcile is the main reconciliation loop for the Neutron CR. It fetches the
// CR, drives the finalizer-gated deletion path, ensures the finalizers, then
// runs the sub-reconciler pipeline. Every exit funnels through updateStatus,
// which re-aggregates the Ready condition and stamps ObservedGeneration.
func (r *NeutronReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var neutron neutronv1alpha1.Neutron
	if err := r.Get(ctx, req.NamespacedName, &neutron); err != nil {
		if apierrors.IsNotFound(err) {
			log.FromContext(ctx).V(1).Info("Neutron resource not found; likely deleted")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching Neutron: %w", err)
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
	if !neutron.DeletionTimestamp.IsZero() {
		children, wait := commonmulticluster.ResolveChildrenClientForDeletion(
			ctx, r.Resolver, r.Client, neutron.Spec.TargetClusterRef, *neutron.DeletionTimestamp)
		if wait {
			// The hold goes on the CR, not only into the operator's log. It is a
			// deliberate state a CR can sit in for minutes, and "Terminating,
			// waiting on the target cluster" has to be distinguishable from a
			// wedged finalizer without correlating logs across replicas. This exit
			// precedes the pipeline's status snapshot below, so it takes its own
			// baseline for the skip-unchanged write.
			statusBefore := neutron.Status.DeepCopy()
			neutronSkeleton.MarkFailed(&neutron, "SecretsReady",
				commonmulticluster.TargetClusterUnavailable,
				fmt.Errorf("target cluster %s does not resolve; waiting at least %s before abandoning its children",
					neutron.Spec.TargetClusterRef.Name, commonmulticluster.AbandonAfter))
			return r.updateStatus(ctx, &neutron, statusBefore,
				ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, nil)
		}
		if result, err := r.reconcileDelete(ctx, children, &neutron); !result.IsZero() || err != nil {
			return result, err
		}
		// The label-selected sweep runs after the named MariaDB cleanup, never
		// before it: that flow waits one pass on the CRs it deletes by name, and a
		// sweep running first would delete them out from under it. The ordering
		// mirrors the local one, where the garbage collection cascade starts only
		// once every finalizer has been released.
		if err := r.reconcileDeleteRemoteChildren(ctx, children, &neutron); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Resolve the client every child object of this CR is read and written with.
	// The embedded client stays on the management cluster (the CR, its status, its
	// finalizers and the OVNCentral it names live there); children carries
	// everything the CR projects into the target cluster. The resolution runs
	// before the finalizer is added so a CR naming an unresolvable cluster stays
	// clean of finalizers: nothing was created for it, so there is nothing to
	// clean up, and a finalizer would only block its deletion.
	children, err := commonmulticluster.ResolveChildrenClient(ctx, r.Resolver, r.Client, neutron.Spec.TargetClusterRef)
	if err != nil {
		// This exit precedes the pipeline's status snapshot below, so it takes its
		// own baseline for the skip-unchanged write.
		statusBefore := neutron.Status.DeepCopy()
		neutronSkeleton.MarkFailed(&neutron, "SecretsReady", commonmulticluster.TargetClusterUnavailable, err)
		return r.updateStatus(ctx, &neutron, statusBefore,
			ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, nil)
	}

	// Ensure the finalizer is installed before any sub-reconciler runs so a
	// deletion issued before the next pass still funnels through reconcileDelete.
	// Requeuing after the Update guarantees the next reconcile observes the
	// persisted finalizer rather than the in-memory copy.
	if added, err := commonreconcile.EnsureFinalizer(ctx, r.Client, &neutron, neutronFinalizer); err != nil {
		return ctrl.Result{}, err
	} else if added {
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueNextPass}, nil
	}

	// The remote-children finalizer goes on only when the CR projects onto a
	// target cluster. A local CR keeps the garbage collection cascade, which reaps
	// its children from their owner references, so it has nothing for this
	// finalizer to hold the CR open for. spec.targetClusterRef is immutable, so
	// the condition cannot flip under a live CR.
	if neutron.Spec.TargetClusterRef != nil {
		if added, err := commonreconcile.EnsureFinalizer(ctx, r.Client, &neutron,
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
	statusBefore := neutron.Status.DeepCopy()

	result, err := commonreconcile.RunPipeline(ctx, instrumenter.Instrument, r.pipelineSteps(children, &neutron))
	return r.updateStatus(ctx, &neutron, statusBefore, result, err)
}

// pipelineSteps returns the ordered sub-reconciler pipeline for one Neutron.
// Each step runs in dependency order; the first to return a non-zero result or
// an error short-circuits the chain and funnels through updateStatus.
//
// It is a method rather than a literal inside Reconcile so the drift guard can
// enumerate the step names without running a reconcile.
func (r *NeutronReconciler) pipelineSteps(children client.Client, neutron *neutronv1alpha1.Neutron) []commonreconcile.Step {
	// The values one sub-reconciler hands to a later one within a single
	// reconcile pass, captured by the step closures. The four digests are the
	// content digests of the service-user password, the assembled DSN, the
	// transport URL and the OVN client identity; the deployment and worker steps
	// stamp them into pod-template annotations so a rotated credential rolls the
	// pods. egressPort is the broker's TCP port the networkpolicy step opens.
	// ovn carries the two resolved OVN database addresses, and configMapName names
	// the rendered config ConfigMap the db-sync Job, the API pods, the workers and
	// the OVN sync CronJob mount.
	var (
		authtokenDigest, dsnDigest, transportDigest string
		egressPort                                  int32
		ovn                                         resolvedOVNEndpoints
		ovnClientDigest, configMapName              string
	)

	return []commonreconcile.Step{
		{Name: "Secrets", Fn: func(ctx context.Context) (res ctrl.Result, err error) {
			res, authtokenDigest, err = r.reconcileSecrets(ctx, children, neutron)
			return res, err
		}},
		// reconcileDBConnectionSecret materialises the DB URL into the derived
		// <neutron.Name>-db-connection Secret. It runs after Secrets (upstream
		// credentials must be synced) and before Config; failures set
		// SecretsReady=False, the same condition reconcileSecrets uses.
		{Name: "DBConnectionSecret", Fn: func(ctx context.Context) (res ctrl.Result, err error) {
			res, dsnDigest, err = r.reconcileDBConnectionSecret(ctx, children, neutron)
			return res, err
		}},
		// reconcileTransportURLSecret materialises the rabbit:// URL into the
		// derived <neutron.Name>-transport-url Secret. It also reports the broker
		// port the networkpolicy member opens as an egress peer.
		{Name: "TransportURLSecret", Fn: func(ctx context.Context) (res ctrl.Result, err error) {
			res, transportDigest, egressPort, err = r.reconcileTransportURLSecret(ctx, children, neutron)
			return res, err
		}},
		// reconcileOVNEndpoints resolves the Northbound and Southbound addresses
		// off the OVNCentral this Neutron drives. It reads the management cluster,
		// where both CRs live, so it takes no children client.
		{Name: "OVNEndpoints", Fn: func(ctx context.Context) (res ctrl.Result, err error) {
			ovn, res, err = r.reconcileOVNEndpoints(ctx, neutron)
			return res, err
		}},
		// reconcileOVNClientSecret mirrors the client identity the central
		// published into the namespace the Neutron pods run in. It runs behind
		// OVNEndpoints because it reads the source Secret name off the resolved
		// central, and both steps report under OVNEndpointsReady.
		{Name: "OVNClientSecret", Fn: func(ctx context.Context) (res ctrl.Result, err error) {
			ovnClientDigest, res, err = r.reconcileOVNClientSecret(ctx, children, neutron, ovn)
			return res, err
		}},
		// reconcileConfig renders neutron.conf and ml2_conf.ini into an immutable
		// ConfigMap. It runs behind both OVN steps because the [ovn] section is
		// parameterised by the resolved addresses. It self-marks SecretsReady=False
		// on failure via markConfigFailed, so the wrapper only threads the result.
		{Name: "Config", Fn: func(ctx context.Context) (res ctrl.Result, err error) {
			res, configMapName, err = r.reconcileConfig(ctx, children, neutron, ovn)
			return res, err
		}},
		// reconcileOVNDBSync projects the recurring synchronisation CronJob. It
		// runs BEFORE Database, not in the parallel group behind it: the pipeline
		// short-circuits at the Database step for as long as the schema migration
		// Job runs, so a step placed behind it is never reached to report on the
		// schedule during the one window that changes it. Its only input is the
		// rendered config the CronJob mounts, which the Config step above produced.
		{Name: "OVNDBSync", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileOVNDBSync(ctx, children, neutron, configMapName)
		}},
		// reconcileDatabase provisions the schema, gates the requested OpenStack
		// release against the installed one, and runs the db-sync Job against the
		// rendered config.
		{Name: "Database", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileDatabase(ctx, children, neutron, configMapName)
		}},
		// reconcileDeployment projects the API Deployment, its Service, and the
		// PodDisruptionBudget. It runs after Database so the pods only start once
		// the schema they query exists.
		{Name: "Deployment", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileDeployment(ctx, children, neutron, configMapName,
				dsnDigest, authtokenDigest, transportDigest, ovnClientDigest)
		}},
		// reconcileWorkers projects the two Deployments running the neutron
		// processes that serve no HTTP. They read the same database, broker and OVN
		// client identity as the API pods, so they carry the same four digests.
		{Name: "Workers", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileWorkers(ctx, children, neutron, configMapName,
				dsnDigest, authtokenDigest, transportDigest, ovnClientDigest)
		}},
		// Once the Deployment/Service outputs are in place, HTTPRoute, HealthCheck,
		// HPA, and NetworkPolicy have no inter-dependency and run concurrently.
		// Each member sets exactly one condition type; the group self-instruments
		// its members, so this step carries no sub_reconciler name.
		{Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileParallelGroup(ctx, neutron, r.parallelSteps(children, ovn, egressPort))
		}},
	}
}

// parallelSteps returns the members of the post-deployment parallel group. Each
// member sets exactly one condition type and receives its own copy of the CR, so
// none of them reads a value another one produces. ovn and egressPort are the
// outputs earlier steps produced in the same pass.
//
// It is a method rather than a literal inside pipelineSteps so the drift guard
// can enumerate the member names and their condition types without running a
// reconcile.
func (r *NeutronReconciler) parallelSteps(children client.Client, ovn resolvedOVNEndpoints, egressPort int32) []commonreconcile.ParallelStep[*neutronv1alpha1.Neutron] {
	return []commonreconcile.ParallelStep[*neutronv1alpha1.Neutron]{
		{
			Name:          "HTTPRoute",
			ConditionType: conditionTypeHTTPRouteReady,
			Fn: func(ctx context.Context, n *neutronv1alpha1.Neutron) (ctrl.Result, error) {
				return r.reconcileHTTPRoute(ctx, children, n)
			},
		},
		{
			Name:          "HealthCheck",
			ConditionType: conditionTypeNeutronAPIReady,
			Fn: func(ctx context.Context, n *neutronv1alpha1.Neutron) (ctrl.Result, error) {
				return r.reconcileHealthCheck(ctx, n)
			},
		},
		{
			Name:          "HPA",
			ConditionType: "HPAReady",
			Fn: func(ctx context.Context, n *neutronv1alpha1.Neutron) (ctrl.Result, error) {
				return r.reconcileHPA(ctx, children, n)
			},
		},
		{
			Name:          "NetworkPolicy",
			ConditionType: conditionTypeNetworkPolicyReady,
			Fn: func(ctx context.Context, n *neutronv1alpha1.Neutron) (ctrl.Result, error) {
				return r.reconcileNetworkPolicy(ctx, children, n, ovn, egressPort)
			},
		},
	}
}

// reconcileParallelGroup runs the given sub-reconcilers concurrently, delegating
// to the shared skeleton: each member operates on its own DeepCopy of the Neutron
// CR, conditions from every member (including those that succeeded before a peer
// failed) are merged back into the primary neutron, and on success the shortest
// non-zero RequeueAfter is returned. Members instrument individually via
// instrumenter.Instrument.
func (r *NeutronReconciler) reconcileParallelGroup(
	ctx context.Context,
	neutron *neutronv1alpha1.Neutron,
	subs []commonreconcile.ParallelStep[*neutronv1alpha1.Neutron],
) (ctrl.Result, error) {
	return neutronSkeleton.RunParallelGroup(ctx, neutron, instrumenter.Instrument, subs)
}

// reconcileDelete drives the finalizer cleanup when the Neutron CR is being
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
func (r *NeutronReconciler) reconcileDelete(ctx context.Context, children client.Client, neutron *neutronv1alpha1.Neutron) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(neutron, neutronFinalizer) {
		return ctrl.Result{}, nil
	}

	key := client.ObjectKey{Name: neutron.Name, Namespace: neutron.Namespace}

	if children == nil {
		r.Recorder.Event(neutron, corev1.EventTypeWarning, "RemoteChildrenAbandoned",
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
			r.Recorder.Event(neutron, corev1.EventTypeNormal, "FinalizingDatabase",
				"Cleaning up MariaDB Database, User, and Grant before removing Neutron")
			return ctrl.Result{RequeueAfter: RequeueDatabaseWait}, nil
		}

		r.Recorder.Event(neutron, corev1.EventTypeNormal, "DatabaseFinalized",
			"MariaDB Database, User, and Grant marked for deletion; releasing finalizer")
	}

	controllerutil.RemoveFinalizer(neutron, neutronFinalizer)
	if err := r.Update(ctx, neutron); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}
	neutronmetrics.DeleteForNeutron(neutron.Name, neutron.Namespace)
	// Drop the per-CR health-probe cache so a CR recreated under the same
	// name/namespace never serves a stale probe keyed on the deleted CR's UID.
	r.healthProbeCache.Evict(key)
	return ctrl.Result{}, nil
}

// reconcileDeleteRemoteChildren deletes everything this Neutron projected onto
// the target cluster it names and releases the remote-children finalizer, as
// commonmulticluster.SweepRemoteChildren documents. reconcileDelete above deletes
// the three MariaDB CRs it tracks by name; this pass is what reaches the rest,
// selected on the ownership labels Claim stamped on them.
func (r *NeutronReconciler) reconcileDeleteRemoteChildren(ctx context.Context, children client.Client, neutron *neutronv1alpha1.Neutron) error {
	return commonmulticluster.SweepRemoteChildren(ctx, r.Client, r.Resolver, r.Recorder, r.Scheme,
		neutron, neutron.Spec.TargetClusterRef, children, NeutronRemoteChildKinds)
}

// updateStatus persists the current status conditions and returns the given
// result and error, delegating to the shared skeleton: the write is skipped when
// the pass left status semantically unchanged from the statusBefore snapshot, a
// failed write is joined with reconcileErr, and the mutate hook re-aggregates the
// Ready condition on every persist and stamps status.observedGeneration.
func (r *NeutronReconciler) updateStatus(ctx context.Context, neutron *neutronv1alpha1.Neutron, statusBefore *neutronv1alpha1.NeutronStatus, result ctrl.Result, reconcileErr error) (ctrl.Result, error) {
	return neutronSkeleton.UpdateStatus(ctx, r.Client, neutron, statusBefore, &neutron.Status, func() {
		neutron.Status.ObservedGeneration = neutron.Generation
	}, result, reconcileErr)
}

// setReadyCondition sets the aggregate Ready condition based on all
// sub-conditions, delegating to the shared skeleton with neutron's sub-condition
// vocabulary.
func setReadyCondition(neutron *neutronv1alpha1.Neutron) {
	neutronSkeleton.SetReady(neutron)
}

// SetupWithManager registers the NeutronReconciler with the controller manager.
// The shared controller options it applies let independent CRs reconcile in
// parallel instead of serialising at the controller-runtime default of 1, and the
// tuned RateLimiter caps per-item failure backoff at 30s rather than the default
// 1000s (see bootstrap.TypedControllerOptions).
func (r *NeutronReconciler) SetupWithManager(mgr mcmanager.Manager) error {
	return r.setupWithOptions(mgr, bootstrap.TypedControllerOptions[mcreconcile.Request](r.MaxConcurrentReconciles))
}

// setupWithOptions carries the production watch wiring SetupWithManager applies.
// The controller options are a parameter so an envtest integration suite can
// register this exact chain with SkipNameValidation set, rather than a hand-built
// copy of it that drifts the moment a leg is added here.
func (r *NeutronReconciler) setupWithOptions(mgr mcmanager.Manager, opts crcontroller.TypedOptions[mcreconcile.Request]) error {
	local := mgr.GetLocalManager()

	// Detect whether the Gateway API CRD is installed. spec.gateway is optional,
	// so the operator must run on clusters without Gateway API. Adding
	// Owns(HTTPRoute) unconditionally would fail at Start with "no matches for
	// kind HTTPRoute" when the CRD is missing, blocking every Neutron CR.
	r.gatewayAPIAvailable = gateway.IsGVKAvailable(local.GetRESTMapper(), httpRouteGVK)
	setupLog := ctrl.Log.WithName("neutron-setup")
	if r.gatewayAPIAvailable {
		setupLog.Info("Gateway API detected; enabling HTTPRoute watch and reconciliation")
	} else {
		setupLog.Info("Gateway API not installed; HTTPRoute watch disabled, spec.gateway will be rejected via HTTPRouteReady condition")
	}

	// Register the Neutron field indexers before Watches so secretToNeutronMapper
	// and centralToNeutronsMapper can rely on them for their MatchingFields
	// lookups. The indexes go on the LOCAL field indexer, not mgr's: with a
	// provider configured, the multicluster manager's field indexer registers
	// against the provider clusters, which hold no Neutron CR. Registration stays
	// local by contract: the indexes are on a CR kind, which exists on the
	// management cluster alone, and every request the watches emit is pinned to
	// that cluster (LocalRequests / RemoteRequests,
	// internal/common/multicluster/watch.go), so a remote event resolves to its CR
	// through the local cache. Registering on the fleet would fail the engagement
	// of every target cluster, because the kubeconfig provider applies its stored
	// indexes while engaging one.
	if err := registerNeutronIndexes(context.Background(), local.GetFieldIndexer()); err != nil {
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
		func(neutron *neutronv1alpha1.Neutron) *commonv1.TargetClusterRefSpec {
			return neutron.Spec.TargetClusterRef
		})

	b := mcbuilder.ControllerManagedBy(mgr).
		WithOptions(opts).
		// Filter the CR's own status-only updates so Status().Update does not
		// re-wake the controller (see watch.CRUpdatePredicate).
		For(&neutronv1alpha1.Neutron{}, mcbuilder.WithPredicates(watch.CRUpdatePredicate()), engageLocal, engageNoProviders).
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
	b, err := commonmulticluster.AddRemoteChildWatches(b, local.GetScheme(), &neutronv1alpha1.Neutron{},
		targets, NeutronRemoteChildKinds, nil)
	if err != nil {
		return err
	}

	// Watch Secrets and map to the Neutron CRs that reference them by name, own
	// them, or drive the OVNCentral that published them. ESO-managed Secrets are
	// owned by the ExternalSecret controller, not the Neutron CR, so
	// EnqueueRequestForOwner would never match them.
	b, err = commonmulticluster.AddInputWatch(b, local.GetScheme(), targets, &corev1.Secret{},
		secretToNeutronMapper(local.GetClient()))
	if err != nil {
		return err
	}

	// Watch the MariaDB cluster CR referenced by spec.database.clusterRef so the
	// operator reflects upstream database outages in DatabaseReady without waiting
	// for the next periodic requeue.
	b, err = commonmulticluster.AddInputWatch(b, local.GetScheme(), targets, &mariadbv1alpha1.MariaDB{},
		mariaDBToNeutronMapper(local.GetClient()))
	if err != nil {
		return err
	}

	// Watch both the cluster-scoped ClusterSecretStore and the namespaced
	// SecretStore a Neutron can select via spec.secretStoreRef, so the operator
	// reflects upstream secret-backend outages in SecretsReady as soon as ESO flips
	// the selected store's Ready condition.
	b, err = commonmulticluster.AddInputWatch(b, local.GetScheme(), targets, &esov1.ClusterSecretStore{},
		esoStoreToNeutronMapper(local.GetClient(), commonv1.SecretStoreKindCluster))
	if err != nil {
		return err
	}
	b, err = commonmulticluster.AddInputWatch(b, local.GetScheme(), targets, &esov1.SecretStore{},
		esoStoreToNeutronMapper(local.GetClient(), commonv1.SecretStoreKindNamespaced))
	if err != nil {
		return err
	}

	return b.
		// The OVNCentral a Neutron drives lives on the management cluster whatever
		// cluster the children land on, so this leg needs no remote counterpart. It
		// carries no generation predicate: the central's status flips (the two
		// database addresses being published, the client Secret being named) are
		// exactly what the OVN steps wait on, and those leave the generation
		// untouched.
		Watches(&ovnv1alpha1.OVNCentral{},
			commonmulticluster.LocalRequests(centralToNeutronsMapper(local.GetClient())),
			engageLocal, engageNoProviders).
		// The default wrapper turns an error matching multicluster.ErrClusterNotFound
		// into a successful reconcile. This operator instead surfaces an
		// unresolvable cluster as a TargetClusterUnavailable condition and requeues,
		// so the wrapper stays off and the error semantics remain byte-identical to
		// the classic builder's.
		WithClusterNotFoundWrapper(false).
		Complete(commonmulticluster.LocalReconciler(r))
}
