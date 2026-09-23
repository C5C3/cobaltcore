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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/cobaltcore/internal/common/healthcheck"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
	"github.com/c5c3/cobaltcore/operators/nova/internal/computeapi"
)

// novaComputeAppName is the app.kubernetes.io/name label value of every child
// of a NovaCompute. It is the kind in lower case, which keeps those children
// apart from the ones a Nova of the same name projects into the namespace.
const novaComputeAppName = "novacompute"

// novaComputeComponent is the app.kubernetes.io/component label value, the
// container name and the name suffix of the DaemonSet.
const novaComputeComponent = "nova-compute"

// novaComputeDrainFinalizer holds a NovaCompute in etcd until every node it
// held has been drained and its compute service deleted. It goes on every CR,
// local or placed, because the drain runs against the Nova API whatever
// cluster the pods run on.
const novaComputeDrainFinalizer = "nova.openstack.c5c3.io/compute-drain"

// NovaComputeNovaRefIndexKey is the field-indexer key under which NovaCompute
// CRs are indexed by the Nova they join. The Nova, rival and compute-contract
// watch legs, the conflict rules and the aggregate set all resolve through it.
const NovaComputeNovaRefIndexKey = "spec.novaRef.name"

// Condition types of the NovaCompute pipeline, one per step.
const (
	conditionTypeNovaReady       = "NovaReady"
	conditionTypeNodesReady      = "NodesReady"
	conditionTypeConfigReady     = "ConfigReady"
	conditionTypeDaemonSetReady  = "DaemonSetReady"
	conditionTypeAggregatesReady = "AggregatesReady"
	conditionTypeServicesReady   = "ServicesReady"
)

// Event reasons the NovaCompute controller records.
const (
	eventReasonComputeServiceDisabled    = "ComputeServiceDisabled"
	eventReasonComputeServiceDeleted     = "ComputeServiceDeleted"
	eventReasonAggregateCreated          = "AggregateCreated"
	eventReasonAggregateDeleted          = "AggregateDeleted"
	eventReasonComputeConfigMirrorReaped = "ComputeConfigMirrorReaped"
	eventReasonNodeConflict              = "NodeConflict"
)

// novaComputeSubConditionTypes lists the condition types the NovaCompute
// steps set. The aggregate Ready condition is True only when all of them are.
//
// It is a separate list from subConditionTypes because the two kinds carry
// separate status contracts, while one instrumenter serves both pipelines. The
// drift guard in instrumentation_test.go checks the metrics map against the
// union of the two. ExtraConfigHealthy stays out, as it does for the Nova: it
// reports on an overlay the user owns.
var novaComputeSubConditionTypes = []string{
	conditionTypeNovaReady,
	conditionTypeNodesReady,
	conditionTypeConfigReady,
	conditionTypeDaemonSetReady,
	conditionTypeAggregatesReady,
	conditionTypeServicesReady,
}

// novaComputeSkeleton bundles the shared controller-skeleton glue with the
// NovaCompute sub-condition vocabulary and status accessor.
var novaComputeSkeleton = commonreconcile.Skeleton[*novav1alpha1.NovaCompute, novav1alpha1.NovaComputeStatus]{
	SubConditionTypes: novaComputeSubConditionTypes,
	Conditions: func(cr *novav1alpha1.NovaCompute) *[]metav1.Condition {
		return &cr.Status.Conditions
	},
}

// NovaComputeRemoteChildKinds are the kinds a NovaCompute projects into its
// namespace on the target cluster it names, and the kinds the deletion sweep
// selects by ownership label. The contract Secret the pods mount is not listed:
// the Nova publishes it, or the ControlPlane mirrors it, and the teardown reaps
// a mirror on its own (see reapComputeConfigMirror).
var NovaComputeRemoteChildKinds = []schema.GroupVersionKind{
	appsv1.SchemeGroupVersion.WithKind("DaemonSet"),
	corev1.SchemeGroupVersion.WithKind("ConfigMap"),
}

// novaComputeNovaRefExtractor is the IndexerFunc registered under
// NovaComputeNovaRefIndexKey. spec.novaRef.name is required and immutable, so a
// NovaCompute indexes under exactly one name.
func novaComputeNovaRefExtractor(obj client.Object) []string {
	cr, ok := obj.(*novav1alpha1.NovaCompute)
	if !ok || cr.Spec.NovaRef.Name == "" {
		// controller-runtime should never call us with the wrong type; a nil
		// return is safer than a panic if it ever does.
		return nil
	}
	return []string{cr.Spec.NovaRef.Name}
}

// NovaComputeReconciler reconciles a NovaCompute: it runs nova-compute on the
// node pool the CR selects, keeps the host aggregates the pool's zones need,
// and drains a node through the Nova API before its pod goes.
type NovaComputeReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// APIReader is the management cluster's direct, uncached reader. The Nodes
	// step lists Nodes and the Services step lists pods through it (or through
	// the target cluster's own uncached reader), because a cached Node read
	// would start a cluster-wide Node informer a namespace-scoped install
	// cannot sync. Nil in unit tests, which read through the children client.
	APIReader client.Reader

	// MaxConcurrentReconciles bounds how many NovaCompute CRs reconcile
	// concurrently. A value <= 0 falls back to
	// bootstrap.DefaultMaxConcurrentReconciles, so the zero value is safe.
	MaxConcurrentReconciles int

	// Resolver resolves the target clusters a NovaCompute and its Nova name.
	// Nil means always-local.
	Resolver commonmulticluster.ClusterResolver

	// HTTPClient is the Keystone and Nova client seam. Production leaves it nil:
	// a placed Nova is then reached through its cluster's service proxy and a
	// local one with http.DefaultClient. Tests inject computeapitest.Fake.
	HTTPClient healthcheck.HTTPDoer
}

// novaComputePass carries what one step hands a later one within a single
// reconcile pass. A closure keeps it inside the pass instead of on the
// reconciler, where it would be shared by every CR reconciled concurrently.
type novaComputePass struct {
	// The NovaRef step resolves the Nova and everything the pods and the API
	// calls need from it.
	image       commonv1.ImageSpec
	secretName  string
	doer        computeapi.Doer
	keystoneURL string
	computeURL  string
	creds       computeapi.Credentials

	// api is built on first use and shared by the Aggregates and Services
	// steps, so a pass requests one token.
	api *computeapi.Client

	// The Nodes step hands the DaemonSet step the nodes to keep the pod off
	// (excluded, in Conflict) and on (keepPods, Draining), the Services step
	// the Releasing nodes another pool now selects, and the Aggregates step the
	// other NovaComputes of the Nova on any cluster.
	siblings []novav1alpha1.NovaCompute
	excluded []string
	keepPods []string
	handover map[string]bool

	// The PoolConfig step names the rendered ConfigMap and hashes the contract.
	configMapName string
	configHash    string

	// aggregatesEnsured is set when the Aggregates step completed, which the
	// teardown waits for so the last pool of a Nova removes the aggregates it
	// created.
	aggregatesEnsured bool
}

// computeClient returns the pass's compute API client, authenticating on first
// use.
func (p *novaComputePass) computeClient(ctx context.Context) (*computeapi.Client, error) {
	if p.api != nil {
		return p.api, nil
	}
	api, err := computeapi.New(ctx, p.doer, p.keystoneURL, p.computeURL, p.creds)
	if err != nil {
		return nil, err
	}
	p.api = api
	return api, nil
}

// +kubebuilder:rbac:groups=nova.openstack.c5c3.io,resources=novacomputes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=nova.openstack.c5c3.io,resources=novacomputes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=nova.openstack.c5c3.io,resources=novacomputes/finalizers,verbs=update
// daemonsets carry the nova-compute pods of a node pool.
// +kubebuilder:rbac:groups=apps,resources=daemonsets,verbs=get;list;watch;create;update;patch;delete
// nodes are read to resolve a pool's selection, its zones and its conflicts.
// +kubebuilder:rbac:groups=core,resources=nodes,verbs=get;list;watch

// Reconcile is the main reconciliation loop for the NovaCompute CR. It fetches
// the CR, drives the teardown of a terminating one, ensures the finalizers,
// then runs the pipeline. Every exit that reports funnels through
// updateStatus, which re-aggregates Ready and stamps ObservedGeneration.
func (r *NovaComputeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var cr novav1alpha1.NovaCompute
	if err := r.Get(ctx, req.NamespacedName, &cr); err != nil {
		if apierrors.IsNotFound(err) {
			log.FromContext(ctx).V(1).Info("NovaCompute resource not found; likely deleted")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching NovaCompute: %w", err)
	}

	// The deletion branch comes before the target-cluster resolution and uses
	// the deletion variant, so a CR whose cluster was deregistered is not held
	// Terminating by an unresolvable ref.
	if !cr.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &cr)
	}

	// The resolution runs before the finalizers are added, so a CR naming an
	// unresolvable cluster stays free of them: nothing was created for it.
	children, err := commonmulticluster.ResolveChildrenClient(ctx, r.Resolver, r.Client, cr.Spec.TargetClusterRef)
	if err != nil {
		statusBefore := cr.Status.DeepCopy()
		novaComputeSkeleton.MarkFailed(&cr, conditionTypeNovaReady, commonmulticluster.TargetClusterUnavailable, err)
		return r.updateStatus(ctx, &cr, statusBefore,
			ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, nil)
	}

	// Both finalizers go on before any step runs, so a deletion issued before
	// the next pass still funnels through the drain and the sweep. The
	// remote-children finalizer is added only for a placed CR: a local CR's
	// children are collected from their owner references.
	if added, err := commonreconcile.EnsureFinalizer(ctx, r.Client, &cr, novaComputeDrainFinalizer); err != nil {
		return ctrl.Result{}, err
	} else if added {
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueNextPass}, nil
	}
	if cr.Spec.TargetClusterRef != nil {
		if added, err := commonreconcile.EnsureFinalizer(ctx, r.Client, &cr,
			commonmulticluster.RemoteChildrenFinalizer); err != nil {
			return ctrl.Result{}, err
		} else if added {
			return ctrl.Result{RequeueAfter: commonreconcile.RequeueNextPass}, nil
		}
	}

	statusBefore := cr.Status.DeepCopy()
	result, err := commonreconcile.RunPipeline(ctx, instrumenter.Instrument,
		r.pipelineSteps(children, &cr, &novaComputePass{}))
	return r.updateStatus(ctx, &cr, statusBefore, result, err)
}

// pipelineSteps returns the ordered pipeline for one NovaCompute. The Nova
// gates everything, the node set parameterises the config and the DaemonSet,
// the pods have to exist before their services can register, and the
// aggregates are ensured before the services are polled so a node never waits
// on an aggregate its onboarding needs.
//
// A step that sets its condition False for a reason that is not a wait on an
// input returns a zero result, so the later steps still run: an empty
// selection still reconciles the drain of the nodes the pool held.
//
// It is a method rather than a literal inside Reconcile so the drift guard can
// enumerate the step names without running a reconcile.
func (r *NovaComputeReconciler) pipelineSteps(children client.Client, cr *novav1alpha1.NovaCompute,
	pass *novaComputePass,
) []commonreconcile.Step {
	return []commonreconcile.Step{
		{Name: "NovaRef", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileNovaComputeNova(ctx, cr, pass)
		}},
		{Name: "Nodes", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileNovaComputeNodes(ctx, children, cr, pass)
		}},
		{Name: "PoolConfig", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileNovaComputeConfig(ctx, children, cr, pass)
		}},
	}
}

// reconcileDelete drives the teardown of a terminating NovaCompute.
//
// While the referenced Nova exists and the target cluster is reachable, the
// pipeline runs with every held node leaving: each is drained, released and has
// its compute service deleted, and the aggregates are recomputed without this
// CR, so the last pool of a Nova removes the ones it created (see
// teardownSteps). The finalizers
// stay until status.nodes is empty and the aggregates step completed. An
// unreachable Nova API therefore blocks the deletion; removing the drain
// finalizer by hand is the escape.
//
// Once that is done, or at once when the Nova is gone or the target cluster
// was abandoned, the remote children are swept, the compute-contract mirror is
// reaped when no other pool on the cluster needs it, and the drain finalizer
// is released.
func (r *NovaComputeReconciler) reconcileDelete(ctx context.Context, cr *novav1alpha1.NovaCompute) (ctrl.Result, error) {
	children, wait := commonmulticluster.ResolveChildrenClientForDeletion(
		ctx, r.Resolver, r.Client, cr.Spec.TargetClusterRef, *cr.DeletionTimestamp)
	if wait {
		statusBefore := cr.Status.DeepCopy()
		novaComputeSkeleton.MarkFailed(cr, conditionTypeNovaReady,
			commonmulticluster.TargetClusterUnavailable,
			fmt.Errorf("target cluster %s does not resolve; waiting at least %s before abandoning its children",
				cr.Spec.TargetClusterRef.Name, commonmulticluster.AbandonAfter))
		return r.updateStatus(ctx, cr, statusBefore,
			ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, nil)
	}

	if children != nil && controllerutil.ContainsFinalizer(cr, novaComputeDrainFinalizer) {
		novaExists, err := r.novaExists(ctx, cr)
		if err != nil {
			return ctrl.Result{}, err
		}
		if novaExists {
			statusBefore := cr.Status.DeepCopy()
			pass := &novaComputePass{}
			result, err := commonreconcile.RunPipeline(ctx, instrumenter.Instrument,
				r.teardownSteps(children, cr, pass))
			if err != nil || len(cr.Status.Nodes) > 0 || !pass.aggregatesEnsured {
				if err == nil && result.IsZero() {
					result = ctrl.Result{RequeueAfter: RequeueComputeDrainPolling}
				}
				return r.updateStatus(ctx, cr, statusBefore, result, err)
			}
		}
	}

	if err := commonmulticluster.SweepRemoteChildren(ctx, r.Client, r.Resolver, r.Recorder, r.Scheme,
		cr, cr.Spec.TargetClusterRef, children, NovaComputeRemoteChildKinds); err != nil {
		return ctrl.Result{}, err
	}
	if children != nil {
		if err := r.reapComputeConfigMirror(ctx, children, cr); err != nil {
			return ctrl.Result{}, err
		}
	}
	if controllerutil.RemoveFinalizer(cr, novaComputeDrainFinalizer) {
		if err := r.Update(ctx, cr); err != nil {
			return ctrl.Result{}, fmt.Errorf("removing the compute-drain finalizer: %w", err)
		}
	}
	return ctrl.Result{}, nil
}

// teardownSteps is the pipeline a terminating NovaCompute runs. While it holds
// a node, that is the whole pipeline: the node's pod stays through the
// DaemonSet until its instances are gone. Once it holds none, only the NovaRef
// and Aggregates steps run. Nothing is left for a pod or a config to serve, and
// a pass working from a cached copy of the CR that predates the release of its
// finalizers must not recreate the children the sweep has already removed.
func (r *NovaComputeReconciler) teardownSteps(children client.Client, cr *novav1alpha1.NovaCompute,
	pass *novaComputePass,
) []commonreconcile.Step {
	steps := r.pipelineSteps(children, cr, pass)
	if len(cr.Status.Nodes) > 0 {
		return steps
	}
	return slices.DeleteFunc(steps, func(step commonreconcile.Step) bool {
		return step.Name != "NovaRef" && step.Name != "Aggregates"
	})
}

// novaExists reports whether the Nova the CR names is still there.
func (r *NovaComputeReconciler) novaExists(ctx context.Context, cr *novav1alpha1.NovaCompute) (bool, error) {
	err := r.Get(ctx, types.NamespacedName{Namespace: cr.Namespace, Name: cr.Spec.NovaRef.Name}, &novav1alpha1.Nova{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("getting Nova %s/%s: %w", cr.Namespace, cr.Spec.NovaRef.Name, err)
	}
	return true, nil
}

// reapComputeConfigMirror deletes the compute-contract Secret the ControlPlane
// mirrored into this pool's namespace, once no other pool of the same Nova on
// the same cluster needs it. A pool being deleted still needs it while it
// holds a node: the pod draining that node mounts the Secret, and the pool's
// own teardown cannot get past its PoolConfig step without it. The
// ControlPlane mirrors for pools that are not being deleted only, so a mirror
// reaped too early is not put back.
//
// The Secret carries the Nova's name and nothing else to identify it
// ("<nova>-compute-config"), so the reap works after the Nova is gone. A
// Secret without ComputeConfigMirrorLabel is left alone: it is the Nova's own,
// or one a person copied by hand.
func (r *NovaComputeReconciler) reapComputeConfigMirror(ctx context.Context, children client.Client,
	cr *novav1alpha1.NovaCompute,
) error {
	siblings, err := novaComputesOfNova(ctx, r.Client, cr.Namespace, cr.Spec.NovaRef.Name)
	if err != nil {
		return err
	}
	for i := range siblings {
		sibling := &siblings[i]
		if sibling.Name != cr.Name && sameTargetCluster(sibling, cr) &&
			(sibling.DeletionTimestamp.IsZero() || len(sibling.Status.Nodes) > 0) {
			return nil
		}
	}

	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: cr.Namespace, Name: cr.Spec.NovaRef.Name + "-" + componentComputeConfig}
	if err := commonmulticluster.LiveReader(children).Get(ctx, key, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("getting compute-contract Secret %s: %w", key, err)
	}
	if secret.Labels[novav1alpha1.ComputeConfigMirrorLabel] != "true" {
		return nil
	}
	if err := children.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting compute-contract mirror %s: %w", key, err)
	}
	r.Recorder.Eventf(cr, corev1.EventTypeNormal, eventReasonComputeConfigMirrorReaped,
		"Deleted the compute-contract mirror %s: no other NovaCompute of Nova %s uses it on this cluster",
		key.Name, cr.Spec.NovaRef.Name)
	return nil
}

// novaComputesOfNova lists the NovaComputes in namespace that join the named
// Nova, through the field index.
func novaComputesOfNova(ctx context.Context, c client.Reader, namespace, nova string) ([]novav1alpha1.NovaCompute, error) {
	var list novav1alpha1.NovaComputeList
	if err := c.List(ctx, &list, client.InNamespace(namespace),
		client.MatchingFields{NovaComputeNovaRefIndexKey: nova}); err != nil {
		return nil, fmt.Errorf("listing the NovaComputes of Nova %s/%s: %w", namespace, nova, err)
	}
	return list.Items, nil
}

// sameTargetCluster reports whether two NovaComputes project onto the same
// cluster: both local, or both naming the same target.
func sameTargetCluster(a, b *novav1alpha1.NovaCompute) bool {
	return targetClusterName(a.Spec.TargetClusterRef) == targetClusterName(b.Spec.TargetClusterRef)
}

// targetClusterName is the name a ref names, "" for the local cluster.
func targetClusterName(ref *commonv1.TargetClusterRefSpec) string {
	if ref == nil {
		return ""
	}
	return ref.Name
}

// updateStatus persists the status and returns the given result and error,
// delegating to the shared skeleton, which skips a write that changes nothing.
func (r *NovaComputeReconciler) updateStatus(ctx context.Context, cr *novav1alpha1.NovaCompute,
	statusBefore *novav1alpha1.NovaComputeStatus, result ctrl.Result, reconcileErr error,
) (ctrl.Result, error) {
	return novaComputeSkeleton.UpdateStatus(ctx, r.Client, cr, statusBefore, &cr.Status, func() {
		cr.Status.ObservedGeneration = cr.Generation
	}, result, reconcileErr)
}
