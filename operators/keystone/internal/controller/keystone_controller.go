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
	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	"golang.org/x/sync/errgroup"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/c5c3/forge/internal/common/conditions"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// keystoneFinalizer is the name of the finalizer added to every Keystone CR so
// that MariaDB Database, User, and Grant CRs are deterministically cleaned up
// before the Keystone CR is removed from etcd. Defined once as the single
// source of truth for Reconcile, the finalizer handler, tests, and docs
// (CC-0078, REQ-005).
const keystoneFinalizer = "keystone.openstack.c5c3.io/finalizer"

// KeystoneSecretNameIndexKey is the field-indexer key under which Keystone
// CRs are indexed by the union of their referenced Secret names
// (spec.database.secretRef.name and spec.bootstrap.adminPasswordSecretRef.name).
// Used by SetupWithManager to register the indexer and by
// secretToKeystoneMapper to perform an O(1) reverse lookup, replacing the
// prior unfiltered List of all Keystone CRs in the namespace (CC-0087, REQ-001, REQ-006).
// #nosec G101 -- field-indexer key (a JSONPath-like field selector), not a credential.
const KeystoneSecretNameIndexKey = "spec.secretRefs.name"

// keystoneSecretNameExtractor is the controller-runtime IndexerFunc registered
// under KeystoneSecretNameIndexKey. It returns the deduplicated, non-empty
// union of Secret names referenced by a Keystone CR — currently
// spec.database.secretRef.name and spec.bootstrap.adminPasswordSecretRef.name —
// so the field indexer can resolve a Secret event to the referencing CR(s)
// without listing every Keystone in the namespace (CC-0087, REQ-001).
func keystoneSecretNameExtractor(obj client.Object) []string {
	ks, ok := obj.(*keystonev1alpha1.Keystone)
	if !ok {
		// controller-runtime should never call us with the wrong type; a nil
		// return is safer than a panic if it ever does.
		return nil
	}
	dbName := ks.Spec.Database.SecretRef.Name
	adminName := ks.Spec.Bootstrap.AdminPasswordSecretRef.Name

	names := make([]string, 0, 2)
	if dbName != "" {
		names = append(names, dbName)
	}
	if adminName != "" && adminName != dbName {
		names = append(names, adminName)
	}
	return names
}

// registerSecretNameIndex registers the Keystone field indexer under
// KeystoneSecretNameIndexKey with the given FieldIndexer. SetupWithManager
// calls this once against mgr.GetFieldIndexer() so that secretToKeystoneMapper
// can resolve a Secret event to the referencing Keystone CRs via an O(1)
// reverse lookup instead of an unfiltered namespace-scoped List. The returned
// error is wrapped with the index key so the registration site is identifiable
// in manager-startup failure logs (CC-0087, REQ-001, REQ-006).
func registerSecretNameIndex(ctx context.Context, indexer client.FieldIndexer) error {
	if err := indexer.IndexField(ctx, &keystonev1alpha1.Keystone{}, KeystoneSecretNameIndexKey, keystoneSecretNameExtractor); err != nil {
		return fmt.Errorf("registering field indexer %q: %w", KeystoneSecretNameIndexKey, err)
	}
	return nil
}

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
	conditionTypeHTTPRouteReady,
	"BootstrapReady",
	"TrustFlushReady",
}

// KeystoneReconciler reconciles a Keystone object.
type KeystoneReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	Recorder   record.EventRecorder
	HTTPClient HTTPDoer

	// gatewayAPIAvailable is set during SetupWithManager from the cluster's
	// RESTMapper and indicates whether the gateway.networking.k8s.io/v1
	// HTTPRoute CRD is installed. When false, the controller skips the
	// HTTPRoute watch entirely so it does not crash on a missing kind, and
	// reconcileHTTPRoute surfaces a clear HTTPRouteReady=False condition if
	// the user nonetheless sets spec.gateway (CC-0065).
	gatewayAPIAvailable bool
}

// httpRouteGVK identifies the HTTPRoute kind the operator would watch when
// Gateway API is installed.
var httpRouteGVK = schema.GroupVersionKind{
	Group:   gatewayv1.GroupVersion.Group,
	Version: gatewayv1.GroupVersion.Version,
	Kind:    "HTTPRoute",
}

// isGatewayAPIAvailable probes the manager's RESTMapper for the HTTPRoute kind.
// Returns false when the mapper has no mapping (CRD not installed) and true
// when the mapping exists. Other mapper errors are treated as "unknown";
// returning false in that case is conservative — the operator starts without
// the HTTPRoute watch and a clear status condition replaces the cryptic
// controller-runtime "no matches for kind" startup error (CC-0065).
func isGatewayAPIAvailable(mapper meta.RESTMapper) bool {
	if mapper == nil {
		return false
	}
	if _, err := mapper.RESTMapping(httpRouteGVK.GroupKind(), httpRouteGVK.Version); err != nil {
		return false
	}
	return true
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
// +kubebuilder:rbac:groups=external-secrets.io,resources=externalsecrets,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=external-secrets.io,resources=pushsecrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=external-secrets.io,resources=clustersecretstores,verbs=get;list;watch
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes/status,verbs=get
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

	// Handle CR deletion via finalizers: block removal from etcd until the
	// MariaDB Database/User/Grant CRs (CC-0078) and the OpenBao backup
	// PushSecrets (CC-0079) owned by this Keystone are cleaned up. Both
	// finalizers run in the same pass; reconcileDeleteOpenBao may requeue while
	// a PushSecret is still Terminating, in which case updateStatus persists
	// the OpenBaoFinalizerBlocked condition (CC-0079, REQ-002, REQ-004,
	// REQ-006, REQ-007).
	if !keystone.DeletionTimestamp.IsZero() {
		if result, err := r.reconcileDelete(ctx, &keystone); !result.IsZero() || err != nil {
			return result, err
		}
		if result, err := r.reconcileDeleteOpenBao(ctx, &keystone); !result.IsZero() || err != nil {
			return r.updateStatus(ctx, &keystone, result, err)
		}
		return ctrl.Result{}, nil
	}

	// Ensure the MariaDB finalizer is installed before any sub-reconciler runs
	// so that a deletion issued between now and the next pass still funnels
	// through reconcileDelete (CC-0078, REQ-001, REQ-006). Returning
	// Requeue=true after the Update guarantees the next reconcile observes the
	// persisted finalizer rather than relying on the in-memory copy.
	if !controllerutil.ContainsFinalizer(&keystone, keystoneFinalizer) {
		controllerutil.AddFinalizer(&keystone, keystoneFinalizer)
		if err := r.Update(ctx, &keystone); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Ensure the OpenBao finalizer is installed so that deleting the Keystone
	// CR blocks on cleanup of the fernet-keys-backup and credential-keys-backup
	// PushSecrets, which ESO then uses to purge the kv-v2 paths in OpenBao
	// (CC-0079, REQ-001, REQ-006).
	if !controllerutil.ContainsFinalizer(&keystone, keystoneOpenBaoFinalizer) {
		controllerutil.AddFinalizer(&keystone, keystoneOpenBaoFinalizer)
		if err := r.Update(ctx, &keystone); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding openbao finalizer: %w", err)
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Run sub-reconcilers in dependency order; independent groups run concurrently.
	if result, err := r.reconcileSecrets(ctx, &keystone); !result.IsZero() || err != nil {
		return r.updateStatus(ctx, &keystone, result, err)
	}

	// reconcileDBConnectionSecret materialises the DB URL into the derived
	// <keystone.Name>-db-connection Secret so that keystone.conf can reference
	// the placeholder while the real credentials live in a Secret consumed via
	// OS_DATABASE__CONNECTION. It must run after reconcileSecrets (upstream
	// credentials must be synced) and before reconcileConfig (the derived
	// Secret is consumed by downstream pods/Jobs that reference the ConfigMap).
	// Failures set SecretsReady=False — the same condition used by
	// reconcileSecrets — so no new subConditionTypes entry is required
	// (CC-0080, REQ-005).
	if result, err := r.reconcileDBConnectionSecret(ctx, &keystone); !result.IsZero() || err != nil {
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

	// HTTPRoute reconciliation runs after the Deployment/Service are ensured
	// so that the backend Service is present before the Gateway controller
	// resolves backendRefs (CC-0065, REQ-009, REQ-010).
	if result, err := r.reconcileHTTPRoute(ctx, &keystone); !result.IsZero() || err != nil {
		return r.updateStatus(ctx, &keystone, result, err)
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

// reconcileDelete drives the finalizer cleanup when the Keystone CR is being
// deleted. It is a no-op if the Keystone finalizer is absent (e.g. a CR created
// before this operator version, or whose finalizer was already released).
// Otherwise it emits FinalizingDatabase when there is real cleanup work to
// announce, issues Delete on the MariaDB Database/User/Grant CRs, emits
// DatabaseFinalized, and releases the finalizer in a single pass.
//
// The handler deliberately does not wait for the MariaDB CRs to disappear from
// etcd: waiting created a deadlock where the Keystone finalizer kept the CR
// alive, Kubernetes GC could not cascade-delete the keystone Deployment,
// the Pod kept its connections open, and the MariaDB operator could not DROP
// DATABASE. Owner references set by reconcileDatabase ensure the MariaDB CRs
// are still reclaimed after the Keystone CR is gone — either via their own
// finalizers or via GC (CC-0078, REQ-002, REQ-007).
func (r *KeystoneReconciler) reconcileDelete(ctx context.Context, keystone *keystonev1alpha1.Keystone) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(keystone, keystoneFinalizer) {
		return ctrl.Result{}, nil
	}

	// Only emit FinalizingDatabase when at least one MariaDB CR is still live
	// so brownfield CRs (no MariaDB CRs ever created) do not produce a
	// misleading "cleaning up" event (CC-0078, REQ-007).
	hasLiveCleanupWork, err := r.hasLiveMariaDBResources(ctx, keystone)
	if err != nil {
		return ctrl.Result{}, err
	}
	if hasLiveCleanupWork {
		r.Recorder.Event(keystone, corev1.EventTypeNormal, "FinalizingDatabase",
			"Cleaning up MariaDB Database, User, and Grant before removing Keystone")
	}

	if err := r.finalizeDatabaseResources(ctx, keystone); err != nil {
		return ctrl.Result{}, err
	}

	r.Recorder.Event(keystone, corev1.EventTypeNormal, "DatabaseFinalized",
		"MariaDB Database, User, and Grant marked for deletion; releasing finalizer")

	controllerutil.RemoveFinalizer(keystone, keystoneFinalizer)
	if err := r.Update(ctx, keystone); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

// hasLiveMariaDBResources reports whether any of the three MariaDB CRs
// (Database, User, Grant) owned by this Keystone still exists with
// DeletionTimestamp unset — i.e., real cleanup work remains. Brownfield CRs
// (no MariaDB CRs ever created) report false so the FinalizingDatabase event
// is suppressed when there is nothing to announce (CC-0078, REQ-007).
func (r *KeystoneReconciler) hasLiveMariaDBResources(ctx context.Context, keystone *keystonev1alpha1.Keystone) (bool, error) {
	key := mariaDBResourceKey(keystone)
	for _, ctor := range mariaDBResourceCtors {
		obj := ctor()
		err := r.Get(ctx, key, obj)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("checking %T %s: %w", obj, key, err)
		}
		if obj.GetDeletionTimestamp().IsZero() {
			return true, nil
		}
	}
	return false, nil
}

// reconcileDeleteOpenBao drives the openbao-finalizer cleanup when the Keystone
// CR is being deleted. It is a no-op if the openbao finalizer is absent.
// Otherwise it emits FinalizingOpenBaoSecrets when at least one backup
// PushSecret has been adopted by ESO and is not yet Terminating (dedupes the
// event across requeues because subsequent passes observe the PushSecrets
// gone, Terminating, or still unadopted and suppress the emit), calls
// finalizeOpenBaoSecrets, and on done=true emits OpenBaoSecretsFinalized and
// releases the finalizer. A PushSecret held Terminating by ESO's cleanup
// finalizer surfaces as ctrl.Result{RequeueAfter: RequeueSecretPolling} so the
// Keystone CR stays live until ESO has purged the kv-v2 path (CC-0079,
// CC-0091, REQ-002, REQ-004, REQ-006, REQ-007).
func (r *KeystoneReconciler) reconcileDeleteOpenBao(ctx context.Context, keystone *keystonev1alpha1.Keystone) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(keystone, keystoneOpenBaoFinalizer) {
		return ctrl.Result{}, nil
	}

	// Only emit FinalizingOpenBaoSecrets when a backup PushSecret is adopted
	// by ESO and not yet Terminating — subsequent requeues observe the same
	// PushSecret Terminating (DeletionTimestamp set), absent, or still
	// unadopted and suppress the emit, giving exactly-once semantics per
	// termination. Gating on ESO adoption is what preserves the exactly-once
	// contract across the Pass-0 adoption-wait window added by CC-0091:
	// without the gate, the 15s RequeueSecretPolling tick would fire a fresh
	// FinalizingOpenBaoSecrets event on every requeue until ESO adopts
	// (CC-0079, CC-0091, REQ-007).
	hasLiveCleanupWork, err := r.hasLiveOpenBaoBackupPushSecrets(ctx, keystone)
	if err != nil {
		return ctrl.Result{}, err
	}
	if hasLiveCleanupWork {
		r.Recorder.Event(keystone, corev1.EventTypeNormal, "FinalizingOpenBaoSecrets",
			"Cleaning up OpenBao backup PushSecrets before removing Keystone")
	}

	done, err := r.finalizeOpenBaoSecrets(ctx, keystone)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !done {
		return ctrl.Result{RequeueAfter: RequeueSecretPolling}, nil
	}

	r.Recorder.Event(keystone, corev1.EventTypeNormal, "OpenBaoSecretsFinalized",
		"OpenBao backup PushSecrets deleted; releasing openbao-finalizer")

	controllerutil.RemoveFinalizer(keystone, keystoneOpenBaoFinalizer)
	if err := r.Update(ctx, keystone); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing openbao finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

// hasLiveOpenBaoBackupPushSecrets reports whether any backup PushSecret is
// ready to be announced via FinalizingOpenBaoSecrets — i.e. is present, not
// Terminating, AND has already been adopted by ESO (carries the ESO cleanup
// finalizer). Three disqualifiers explicitly return false:
//
//   - NotFound: nothing to clean up.
//   - Terminating (DeletionTimestamp set): the event was already emitted on
//     the prior transition; counting it again would double-announce on every
//     requeue.
//   - Adopted=false: Pass-0 is still blocking the Delete, so there is nothing
//     to announce yet. Without this gate the 15s RequeueSecretPolling tick
//     would fire a fresh Event on every requeue until ESO adopts, regressing
//     the exactly-once contract established by CC-0079 (CC-0091, REQ-007).
func (r *KeystoneReconciler) hasLiveOpenBaoBackupPushSecrets(ctx context.Context, keystone *keystonev1alpha1.Keystone) (bool, error) {
	for _, name := range openBaoBackupPushSecretNames(keystone) {
		key := client.ObjectKey{Namespace: keystone.Namespace, Name: name}
		ps := &esov1alpha1.PushSecret{}
		err := r.Get(ctx, key, ps)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("checking PushSecret %s: %w", key, err)
		}
		if ps.GetDeletionTimestamp().IsZero() && hasESOFinalizer(ps) {
			return true, nil
		}
	}
	return false, nil
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
	// Detect whether the Gateway API CRD is installed. spec.gateway is
	// optional (CC-0065), so the operator must run on clusters without
	// Gateway API. Adding Owns(HTTPRoute) unconditionally would cause the
	// controller to fail at Start with "no matches for kind HTTPRoute"
	// when the CRD is missing, preventing every Keystone CR from being
	// reconciled — including those that do not use spec.gateway.
	r.gatewayAPIAvailable = isGatewayAPIAvailable(mgr.GetRESTMapper())
	setupLog := ctrl.Log.WithName("keystone-setup")
	if r.gatewayAPIAvailable {
		setupLog.Info("Gateway API detected; enabling HTTPRoute watch and reconciliation")
	} else {
		setupLog.Info("Gateway API not installed; HTTPRoute watch disabled, spec.gateway will be rejected via HTTPRouteReady condition")
	}

	// Register the Keystone field indexer before Watches so
	// secretToKeystoneMapper can rely on it for its MatchingFields lookup
	// (CC-0087, REQ-001, REQ-006).
	if err := registerSecretNameIndex(context.Background(), mgr.GetFieldIndexer()); err != nil {
		return err
	}

	builder := ctrl.NewControllerManagedBy(mgr).
		For(&keystonev1alpha1.Keystone{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&batchv1.Job{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Owns(&autoscalingv2.HorizontalPodAutoscaler{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Owns(&batchv1.CronJob{}).
		// EnsurePushSecret (internal/common/secrets/secrets.go) sets a
		// controller reference so Keystone owns each backup PushSecret.
		// Watching the owned PushSecrets collapses the openbao-finalizer
		// adoption-wait latency from up to RequeueSecretPolling (15s) to
		// watch-event delivery latency once ESO installs its cleanup
		// finalizer on first reconcile (CC-0091).
		Owns(&esov1alpha1.PushSecret{})

	if r.gatewayAPIAvailable {
		builder = builder.Owns(&gatewayv1.HTTPRoute{})
	}

	return builder.
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
// requests for Keystone CRs that either reference the Secret by name
// (resolved via the KeystoneSecretNameIndexKey field indexer) or own it via
// an OwnerReference with Kind=Keystone and APIVersion in the Keystone API
// group (e.g. rotation staging Secrets) (CC-0087, REQ-001, REQ-002, REQ-003,
// REQ-005).
//
// Owner-ref matching is evaluated directly on the event object's metadata and
// is scoped to ref.Kind=="Keystone" and any version in
// keystonev1alpha1.GroupVersion.Group, so Secrets persisted with an older
// APIVersion continue to resolve correctly after a future API version bump.
// For each matching ref, the mapper performs a cached Get to drop owner-refs
// whose target Keystone no longer exists in the informer cache; any
// non-NotFound error falls through to enqueue, so a transient cache blip
// cannot swallow a legitimate event.
func secretToKeystoneMapper(c client.Reader) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		namespace := obj.GetNamespace()
		secretName := obj.GetName()
		seen := make(map[types.NamespacedName]struct{})

		var keystones keystonev1alpha1.KeystoneList
		if err := c.List(ctx, &keystones,
			client.InNamespace(namespace),
			client.MatchingFields{KeystoneSecretNameIndexKey: secretName},
		); err != nil {
			// Log and swallow: the owner-ref path below is independent of
			// the index and must still run for rotation staging Secrets.
			log.FromContext(ctx).Error(err, "listing Keystone CRs for secret watch")
		} else {
			for i := range keystones.Items {
				seen[client.ObjectKeyFromObject(&keystones.Items[i])] = struct{}{}
			}
		}

		expectedGroup := keystonev1alpha1.GroupVersion.Group
		for _, ref := range obj.GetOwnerReferences() {
			if ref.Kind != "Keystone" {
				continue
			}
			gv, err := schema.ParseGroupVersion(ref.APIVersion)
			if err != nil || gv.Group != expectedGroup {
				continue
			}
			key := types.NamespacedName{Namespace: namespace, Name: ref.Name}
			// Drop stale/spurious owner-refs whose target Keystone no longer
			// exists. A cached Get is an in-memory lookup — no API server
			// round-trip (CC-0087 review #1).
			var ks keystonev1alpha1.Keystone
			if err := c.Get(ctx, key, &ks); err != nil {
				if apierrors.IsNotFound(err) {
					continue
				}
				// Non-NotFound errors (cache mid-sync, disconnected informer,
				// unregistered GVK) must not silently drop the event; log at
				// V(1) and fall through to enqueue so reconcile can resolve
				// authoritatively (CC-0087 review #3).
				log.FromContext(ctx).V(1).Info("owner-ref Get returned non-NotFound error; enqueueing anyway",
					"secret", client.ObjectKeyFromObject(obj),
					"ownerRef", key,
					"error", err)
			}
			seen[key] = struct{}{}
		}

		if len(seen) == 0 {
			return nil
		}
		requests := make([]reconcile.Request, 0, len(seen))
		for key := range seen {
			requests = append(requests, reconcile.Request{NamespacedName: key})
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
