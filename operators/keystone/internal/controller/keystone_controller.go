// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package controller implements the Keystone reconciler (CC-0013).
package controller

import (
	"context"
	"errors"
	"fmt"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	"golang.org/x/sync/errgroup"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/c5c3/forge/internal/common/conditions"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// subConditionTypes lists the condition types set by individual sub-reconcilers.
// The Ready condition is True only when all of these are True.
var subConditionTypes = []string{
	"SecretsReady",
	"FernetKeysReady",
	"CredentialKeysReady",
	"DatabaseReady",
	conditionTypePolicyValidReady,
	"DeploymentReady",
	conditionTypeKeystoneAPIReady,
	"HPAReady",
	"NetworkPolicyReady",
	"BootstrapReady",
	"TrustFlushReady",
}

// KeystoneReconciler reconciles a Keystone object.
type KeystoneReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	Recorder   record.EventRecorder
	HTTPClient HTTPDoer
}

// +kubebuilder:rbac:groups=keystone.openstack.c5c3.io,resources=keystones,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=keystone.openstack.c5c3.io,resources=keystones/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=keystone.openstack.c5c3.io,resources=keystones/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services;configmaps;secrets;serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list
// +kubebuilder:rbac:groups=batch,resources=jobs;cronjobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=k8s.mariadb.com,resources=databases;users;grants,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=k8s.mariadb.com,resources=mariadbs,verbs=get;list;watch
// +kubebuilder:rbac:groups=external-secrets.io,resources=externalsecrets;pushsecrets,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=external-secrets.io,resources=clustersecretstores,verbs=get;list;watch
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=scheduling.k8s.io,resources=priorityclasses,verbs=get;list;watch

// Reconcile is the main reconciliation loop for the Keystone CR.
func (r *KeystoneReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// Fetch the Keystone CR.
	var keystone keystonev1alpha1.Keystone
	if err := r.Get(ctx, req.NamespacedName, &keystone); err != nil {
		if apierrors.IsNotFound(err) {
			log.FromContext(ctx).V(1).Info("Keystone resource not found; likely deleted")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching Keystone: %w", err)
	}

	// Run sub-reconcilers in dependency order; independent groups run concurrently.
	if result, err := r.reconcileSecrets(ctx, &keystone); !result.IsZero() || err != nil {
		return r.updateStatus(ctx, &keystone, result, err)
	}

	// reconcileConfig must run before reconcileFernetKeys and reconcileDatabase
	// because both the fernet rotation CronJob and the db_sync Job require the
	// keystone.conf ConfigMap.
	configMapName, err := r.reconcileConfig(ctx, &keystone)
	if err != nil {
		return r.updateStatus(ctx, &keystone, ctrl.Result{}, err)
	}

	// FernetKeys, CredentialKeys, and NetworkPolicy are independent of each
	// other and can run concurrently. All three depend on reconcileConfig
	// (above) having completed. NetworkPolicy has no data dependency on the
	// Deployment — it uses selectorLabels derived from the CR
	// (CC-0039, CC-0071, REQ-001).
	if result, err := r.reconcileParallelGroup(ctx, &keystone, []parallelSubReconciler{
		{
			conditionType: "FernetKeysReady",
			fn: func(ctx context.Context, ks *keystonev1alpha1.Keystone) (ctrl.Result, error) {
				return r.reconcileFernetKeys(ctx, ks, configMapName)
			},
		},
		{
			conditionType: "CredentialKeysReady",
			fn: func(ctx context.Context, ks *keystonev1alpha1.Keystone) (ctrl.Result, error) {
				return r.reconcileCredentialKeys(ctx, ks, configMapName)
			},
		},
		{
			conditionType: "NetworkPolicyReady",
			fn: func(ctx context.Context, ks *keystonev1alpha1.Keystone) (ctrl.Result, error) {
				return r.reconcileNetworkPolicy(ctx, ks)
			},
		},
	}); !result.IsZero() || err != nil {
		return r.updateStatus(ctx, &keystone, result, err)
	}

	if result, err := r.reconcileDatabase(ctx, &keystone, configMapName); !result.IsZero() || err != nil {
		return r.updateStatus(ctx, &keystone, result, err)
	}

	// Policy validation gates the Deployment: invalid oslo.policy overrides
	// must be caught before reaching running pods (CC-0058).
	if result, err := r.reconcilePolicyValidation(ctx, &keystone, configMapName); !result.IsZero() || err != nil {
		return r.updateStatus(ctx, &keystone, result, err)
	}

	if result, err := r.reconcileDeployment(ctx, &keystone, configMapName); !result.IsZero() || err != nil {
		return r.updateStatus(ctx, &keystone, result, err)
	}

	// Prune stale immutable ConfigMaps after Deployment is ready to ensure
	// all pods are running the new config before old ConfigMaps are deleted
	// (CC-0077, REQ-007).
	if err := r.pruneStaleConfigMaps(ctx, &keystone, configMapName); err != nil {
		return r.updateStatus(ctx, &keystone, ctrl.Result{}, err)
	}

	// Health check runs after Deployment because it depends on
	// Status.Endpoint which reconcileDeployment sets (CC-0067, REQ-007).
	if result, err := r.reconcileHealthCheck(ctx, &keystone); !result.IsZero() || err != nil {
		return r.updateStatus(ctx, &keystone, result, err)
	}

	if result, err := r.reconcileHPA(ctx, &keystone); !result.IsZero() || err != nil {
		return r.updateStatus(ctx, &keystone, result, err)
	}

	if result, err := r.reconcileBootstrap(ctx, &keystone, configMapName); !result.IsZero() || err != nil {
		return r.updateStatus(ctx, &keystone, result, err)
	}

	if result, err := r.reconcileTrustFlush(ctx, &keystone, configMapName); !result.IsZero() || err != nil {
		return r.updateStatus(ctx, &keystone, result, err)
	}

	// Aggregate the Ready condition.
	setReadyCondition(&keystone)

	return r.updateStatus(ctx, &keystone, ctrl.Result{}, nil)
}

// updateStatus persists the current status conditions and returns the given result and error.
// When both reconcileErr and the status update fail, both errors are preserved via errors.Join
// so that the original reconcile failure is visible in controller-runtime logs (CC-0068).
func (r *KeystoneReconciler) updateStatus(ctx context.Context, keystone *keystonev1alpha1.Keystone, result ctrl.Result, reconcileErr error) (ctrl.Result, error) {
	if err := r.Status().Update(ctx, keystone); err != nil {
		log.FromContext(ctx).Error(err, "unable to update Keystone status")
		return ctrl.Result{}, errors.Join(reconcileErr, fmt.Errorf("updating status: %w", err))
	}
	return result, reconcileErr
}

// setReadyCondition sets the aggregate Ready condition based on all sub-conditions.
func setReadyCondition(keystone *keystonev1alpha1.Keystone) {
	if aggregateReady(keystone.Status.Conditions) {
		conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			ObservedGeneration: keystone.Generation,
			Reason:             "AllReady",
			Message:            "All sub-conditions are ready",
		})
	} else {
		conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: keystone.Generation,
			Reason:             "NotAllReady",
			Message:            "One or more sub-conditions are not ready",
		})
	}
}

// aggregateReady returns true if all sub-condition types are True.
func aggregateReady(conds []metav1.Condition) bool {
	return conditions.AllTrue(conds, subConditionTypes...)
}

// shortestRequeue returns the ctrl.Result with the shortest non-zero
// RequeueAfter from the given results. If no result requests a requeue,
// a zero ctrl.Result is returned (CC-0071, REQ-003).
func shortestRequeue(results ...ctrl.Result) ctrl.Result {
	var shortest ctrl.Result
	for _, r := range results {
		if r.RequeueAfter <= 0 {
			continue
		}
		if shortest.RequeueAfter <= 0 || r.RequeueAfter < shortest.RequeueAfter {
			shortest = r
		}
	}
	return shortest
}

// mergeParallelConditions copies a single condition of the given type from src
// into dst. If src does not contain a condition of that type, dst is left
// unchanged. Pre-existing conditions on dst are preserved (CC-0071, REQ-004).
func mergeParallelConditions(dst, src *keystonev1alpha1.Keystone, conditionType string) {
	cond := conditions.GetCondition(src.Status.Conditions, conditionType)
	if cond == nil {
		return
	}
	conditions.SetCondition(&dst.Status.Conditions, *cond)
}

// parallelSubReconciler describes a sub-reconciler that runs in a parallel
// group. Each sub-reconciler receives its own DeepCopy of the Keystone CR
// and sets exactly one condition type (CC-0071, REQ-001).
type parallelSubReconciler struct {
	conditionType string
	fn            func(ctx context.Context, keystone *keystonev1alpha1.Keystone) (ctrl.Result, error)
}

// reconcileParallelGroup runs the given sub-reconcilers concurrently using
// errgroup.WithContext. Each goroutine operates on a DeepCopy of the Keystone
// CR to avoid data races (CC-0071, REQ-002). After all goroutines complete,
// conditions from every sub-reconciler — including those that succeeded before
// a peer failed — are merged back into the primary keystone so that partial
// progress is visible in status. On success the shortest non-zero RequeueAfter
// is returned (CC-0071, REQ-001, REQ-005).
func (r *KeystoneReconciler) reconcileParallelGroup(
	ctx context.Context,
	keystone *keystonev1alpha1.Keystone,
	subs []parallelSubReconciler,
) (ctrl.Result, error) {
	g, gctx := errgroup.WithContext(ctx)

	type outcome struct {
		result   ctrl.Result
		copy     *keystonev1alpha1.Keystone
		condType string
		err      error
	}
	outcomes := make([]outcome, len(subs))

	for i, sub := range subs {
		ksCopy := keystone.DeepCopy()
		outcomes[i].condType = sub.conditionType
		g.Go(func() error {
			res, err := sub.fn(gctx, ksCopy)
			outcomes[i].result = res
			outcomes[i].copy = ksCopy
			outcomes[i].err = err
			return err
		})
	}

	groupErr := g.Wait()

	// Merge conditions from all completed sub-reconcilers back into the
	// primary keystone, even on partial failure, so the caller can persist
	// partial progress via updateStatus.
	var results []ctrl.Result
	for _, o := range outcomes {
		mergeParallelConditions(keystone, o.copy, o.condType)
		if o.err == nil {
			results = append(results, o.result)
		}
	}

	if groupErr != nil {
		return ctrl.Result{}, groupErr
	}

	return shortestRequeue(results...), nil
}

// SetupWithManager registers the KeystoneReconciler with the controller manager.
func (r *KeystoneReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&keystonev1alpha1.Keystone{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&batchv1.Job{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Owns(&autoscalingv2.HorizontalPodAutoscaler{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Owns(&batchv1.CronJob{}).
		// Watch Secrets and map to the Keystone CRs that reference them.
		// ESO-managed secrets (spec.database.secretRef, spec.bootstrap.adminPasswordSecretRef)
		// are owned by the ExternalSecret controller, not by the Keystone CR, so
		// EnqueueRequestForOwner would never match them. This MapFunc performs a
		// namespace-scoped lookup instead (CC-0013).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(
			secretToKeystoneMapper(mgr.GetClient()),
		)).
		// Watch the MariaDB cluster CR referenced by spec.database.clusterRef so
		// that the operator reflects upstream database outages in DatabaseReady
		// without waiting for the next periodic requeue (CC-0047).
		Watches(&mariadbv1alpha1.MariaDB{}, handler.EnqueueRequestsFromMapFunc(
			mariaDBToKeystoneMapper(mgr.GetClient()),
		)).
		// Watch the OpenBao-backed ClusterSecretStore so the operator reflects
		// upstream secret-backend outages in SecretsReady as soon as ESO flips
		// the store's Ready condition, rather than waiting for the next periodic
		// requeue (CC-0047).
		Watches(&esov1.ClusterSecretStore{}, handler.EnqueueRequestsFromMapFunc(
			clusterSecretStoreToKeystoneMapper(mgr.GetClient()),
		)).
		Complete(r)
}

// secretToKeystoneMapper returns a MapFunc that maps Secret events to reconcile
// requests for Keystone CRs that reference the Secret by name or own it.
func secretToKeystoneMapper(c client.Reader) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		var keystones keystonev1alpha1.KeystoneList
		if err := c.List(ctx, &keystones, client.InNamespace(obj.GetNamespace())); err != nil {
			log.FromContext(ctx).Error(err, "listing Keystone CRs for secret watch")
			return nil
		}

		secretName := obj.GetName()
		var requests []reconcile.Request
		for i := range keystones.Items {
			ks := &keystones.Items[i]
			if ks.Spec.Database.SecretRef.Name == secretName ||
				ks.Spec.Bootstrap.AdminPasswordSecretRef.Name == secretName ||
				isOwnedBy(obj, ks) {
				requests = append(requests, reconcile.Request{
					NamespacedName: client.ObjectKeyFromObject(ks),
				})
			}
		}
		return requests
	}
}

// mariaDBToKeystoneMapper returns a MapFunc that maps MariaDB cluster events
// to reconcile requests for Keystone CRs whose spec.database.clusterRef
// targets the MariaDB by name in the same namespace (CC-0047).
func mariaDBToKeystoneMapper(c client.Reader) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		var keystones keystonev1alpha1.KeystoneList
		if err := c.List(ctx, &keystones, client.InNamespace(obj.GetNamespace())); err != nil {
			log.FromContext(ctx).Error(err, "listing Keystone CRs for MariaDB watch")
			return nil
		}

		mariadbName := obj.GetName()
		var requests []reconcile.Request
		for i := range keystones.Items {
			ks := &keystones.Items[i]
			if ks.Spec.Database.ClusterRef != nil && ks.Spec.Database.ClusterRef.Name == mariadbName {
				requests = append(requests, reconcile.Request{
					NamespacedName: client.ObjectKeyFromObject(ks),
				})
			}
		}
		return requests
	}
}

// clusterSecretStoreToKeystoneMapper returns a MapFunc that enqueues every
// Keystone CR in the cluster when the OpenBao-backed ClusterSecretStore
// changes. The store is cluster-scoped and shared across namespaces, so any
// status transition (e.g. ESO losing the backend connection) must retrigger
// reconcile on all Keystones that route secrets through it (CC-0047).
func clusterSecretStoreToKeystoneMapper(c client.Reader) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		if obj.GetName() != openBaoClusterStoreName {
			return nil
		}

		var keystones keystonev1alpha1.KeystoneList
		if err := c.List(ctx, &keystones); err != nil {
			log.FromContext(ctx).Error(err, "listing Keystone CRs for ClusterSecretStore watch")
			return nil
		}

		requests := make([]reconcile.Request, 0, len(keystones.Items))
		for i := range keystones.Items {
			requests = append(requests, reconcile.Request{
				NamespacedName: client.ObjectKeyFromObject(&keystones.Items[i]),
			})
		}
		return requests
	}
}

// isOwnedBy returns true if obj has an ownerReference pointing to owner.
func isOwnedBy(obj client.Object, owner client.Object) bool {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.UID == owner.GetUID() {
			return true
		}
	}
	return false
}
