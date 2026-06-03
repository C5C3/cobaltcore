// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/forge/internal/common/conditions"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
)

// DECISION (CC-0110, task 2.5/2.6): every child CR the ControlPlane projects
// (MariaDB, Memcached, Keystone) is created in the SAME namespace as the owning
// ControlPlane CR (cp.Namespace), NOT a hardcoded "openstack" literal.
// Rationale: controllerutil.SetControllerReference rejects cross-namespace owner
// references because Kubernetes garbage collection only cascades within a single
// namespace — a child in "openstack" owned by a ControlPlane in "default" would
// fail admission ("cross-namespace owner references are disallowed") and, even
// if forced, would never be GC'd. Co-locating the children with their owner
// keeps the owner reference valid and the GC cascade intact. In production the
// ControlPlane is deployed INTO the openstack control-plane namespace (the same
// namespace the deploy stack places MariaDB/Memcached/Keystone in via
// deploy/flux-system/infrastructure/*.yaml), so the projected children land in
// "openstack" exactly as before — the namespace is now derived from the owner
// rather than assumed.
//
// childNamespace centralises this derivation so every sub-reconciler agrees on
// the projection target.
func childNamespace(cp *c5c3v1alpha1.ControlPlane) string {
	return cp.Namespace
}

// DECISION (CC-0110, task 2.5): the managed-mode MariaDB CR is provisioned with
// a MINIMAL but VALID spec. The mariadb-operator's webhook requires
// Storage.Size (or a VolumeClaimTemplate) — see Storage.Validate in the
// vendored v0.38.1 types — so a 100Gi size and a Galera HA topology are set to
// mirror the production baseline (deploy/flux-system/infrastructure/mariadb.yaml
// uses replicas:3 + galera.enabled + storage.size:100Gi). TLS / issuerRefs are
// deliberately NOT set here: the baseline wires those from cluster-specific
// ClusterIssuers that are an infrastructure concern outside the aggregate's
// knowledge, and the keystone DB-client baseline reads TLS from
// cp.Spec.Infrastructure.Database.TLS rather than the MariaDB CR. The minimal
// spec keeps the CR admissible while leaving site-specific hardening to the
// platform team.
const (
	infraMariaDBStorageSize = "100Gi"
	infraMariaDBReplicas    = int32(3)
)

// memcachedGVK is the GroupVersionKind of the Memcached CR projected in managed
// cache mode. DECISION (CC-0110, task 2.5): memcached.c5c3.io publishes NO Go
// module, so the Memcached child is built and applied as an
// unstructured.Unstructured rather than a typed client object. The fake client
// and the real apiserver both accept an unstructured object carrying this GVK;
// no scheme registration is required.
var memcachedGVK = schema.GroupVersionKind{
	Group:   "memcached.c5c3.io",
	Version: "v1beta1",
	Kind:    "Memcached",
}

// reconcileInfrastructure reconciles the shared backing services (MariaDB,
// Memcached) declared in spec.infrastructure and drives the
// InfrastructureReady condition (CC-0110, REQ-007, REQ-008).
//
// Managed mode (ClusterRef set) ensures an owned child CR per backing service;
// brownfield mode (Host / Servers set) provisions nothing. InfrastructureReady
// is True once every managed child is ensured and reports Ready; while a child
// is still converging the sub-reconciler requeues with InfrastructureReady
// False. When the control plane uses only brownfield infra there is nothing to
// provision, so InfrastructureReady is True immediately.
func (r *ControlPlaneReconciler) reconcileInfrastructure(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	dbManaged := cp.Spec.Infrastructure.Database.ClusterRef != nil
	cacheManaged := cp.Spec.Infrastructure.Cache.ClusterRef != nil

	// Ensure every managed child FIRST, in a single pass, so a half-provisioned
	// control plane (e.g. DB created but Memcached missing) never occurs: both
	// children are created/updated before readiness is gated on. Readiness is
	// then evaluated collectively after every child has been ensured.
	dbReady := true
	if dbManaged {
		ready, err := r.ensureMariaDB(ctx, cp)
		if err != nil {
			conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
				Type:               conditionTypeInfrastructureReady,
				Status:             metav1.ConditionFalse,
				ObservedGeneration: cp.Generation,
				Reason:             "MariaDBError",
				Message:            fmt.Sprintf("ensuring MariaDB: %v", err),
			})
			return ctrl.Result{}, err
		}
		dbReady = ready
	}

	cacheReady := true
	if cacheManaged {
		ready, err := r.ensureMemcached(ctx, cp)
		if err != nil {
			conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
				Type:               conditionTypeInfrastructureReady,
				Status:             metav1.ConditionFalse,
				ObservedGeneration: cp.Generation,
				Reason:             "MemcachedError",
				Message:            fmt.Sprintf("ensuring Memcached: %v", err),
			})
			return ctrl.Result{}, err
		}
		cacheReady = ready
	}

	if !dbReady {
		logger.Info("MariaDB not ready, requeuing",
			"cluster", cp.Spec.Infrastructure.Database.ClusterRef.Name)
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeInfrastructureReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "WaitingForDatabase",
			Message: fmt.Sprintf("MariaDB %q is not ready",
				cp.Spec.Infrastructure.Database.ClusterRef.Name),
		})
		return ctrl.Result{RequeueAfter: infraRequeueAfter}, nil
	}

	if !cacheReady {
		logger.Info("Memcached not ready, requeuing",
			"cluster", cp.Spec.Infrastructure.Cache.ClusterRef.Name)
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeInfrastructureReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "WaitingForCache",
			Message: fmt.Sprintf("Memcached %q is not ready",
				cp.Spec.Infrastructure.Cache.ClusterRef.Name),
		})
		return ctrl.Result{RequeueAfter: infraRequeueAfter}, nil
	}

	conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditionTypeInfrastructureReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: cp.Generation,
		Reason:             "InfrastructureReady",
		Message:            "All managed backing services are ensured and ready",
	})
	return ctrl.Result{}, nil
}

// ensureMariaDB create-or-updates the owned MariaDB CR named after
// spec.infrastructure.database.clusterRef and reports whether it is Ready.
func (r *ControlPlaneReconciler) ensureMariaDB(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) (bool, error) {
	size, err := resource.ParseQuantity(infraMariaDBStorageSize)
	if err != nil {
		return false, fmt.Errorf("parsing MariaDB storage size %q: %w", infraMariaDBStorageSize, err)
	}

	mariadb := &mariadbv1alpha1.MariaDB{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cp.Spec.Infrastructure.Database.ClusterRef.Name,
			Namespace: childNamespace(cp),
		},
	}

	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, mariadb, func() error {
		mariadb.Spec.Replicas = infraMariaDBReplicas
		mariadb.Spec.Galera = &mariadbv1alpha1.Galera{Enabled: true}
		mariadb.Spec.Storage = mariadbv1alpha1.Storage{Size: &size}
		return controllerutil.SetControllerReference(cp, mariadb, r.Scheme)
	}); err != nil {
		return false, fmt.Errorf("create-or-update MariaDB %q: %w", mariadb.Name, err)
	}

	return conditions.IsReady(mariadb.Status.Conditions), nil
}

// ensureMemcached create-or-updates the owned Memcached CR named after
// spec.infrastructure.cache.clusterRef and reports whether it is Ready. The
// Memcached CR is handled as an unstructured.Unstructured because
// memcached.c5c3.io ships no Go module (see memcachedGVK).
func (r *ControlPlaneReconciler) ensureMemcached(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) (bool, error) {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(memcachedGVK)
	u.SetName(cp.Spec.Infrastructure.Cache.ClusterRef.Name)
	u.SetNamespace(childNamespace(cp))

	replicas := cp.Spec.Infrastructure.Cache.Replicas

	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, u, func() error {
		// int32 must be widened to int64 for unstructured nested-field storage.
		if err := unstructured.SetNestedField(u.Object, int64(replicas), "spec", "replicas"); err != nil {
			return fmt.Errorf("setting spec.replicas: %w", err)
		}
		return controllerutil.SetControllerReference(cp, u, r.Scheme)
	}); err != nil {
		return false, fmt.Errorf("create-or-update Memcached %q: %w", u.GetName(), err)
	}

	return unstructuredReady(u), nil
}

// unstructuredReady reports whether an unstructured object carries a
// status.conditions entry of type "Ready" with status "True". A missing or
// malformed conditions list is treated as not-ready rather than an error so a
// freshly-created child simply requeues.
func unstructuredReady(u *unstructured.Unstructured) bool {
	conds, found, err := unstructured.NestedSlice(u.Object, "status", "conditions")
	if err != nil || !found {
		return false
	}
	for _, c := range conds {
		cond, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if cond["type"] == "Ready" && cond["status"] == string(metav1.ConditionTrue) {
			return true
		}
	}
	return false
}
