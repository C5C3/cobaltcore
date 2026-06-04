// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
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
	key := types.NamespacedName{
		Name:      cp.Spec.Infrastructure.Database.ClusterRef.Name,
		Namespace: childNamespace(cp),
	}
	mariadb := &mariadbv1alpha1.MariaDB{}
	err := r.Get(ctx, key, mariadb)
	switch {
	case apierrors.IsNotFound(err):
		// Create fresh with the projected, production-shaped spec.
		size, perr := resource.ParseQuantity(infraMariaDBStorageSize)
		if perr != nil {
			return false, fmt.Errorf("parsing MariaDB storage size %q: %w", infraMariaDBStorageSize, perr)
		}
		mariadb.Name = key.Name
		mariadb.Namespace = key.Namespace
		mariadb.Spec.Replicas = infraMariaDBReplicas
		mariadb.Spec.Galera = &mariadbv1alpha1.Galera{Enabled: true}
		mariadb.Spec.Storage = mariadbv1alpha1.Storage{Size: &size}
		if serr := controllerutil.SetControllerReference(cp, mariadb, r.Scheme); serr != nil {
			return false, fmt.Errorf("setting owner reference on MariaDB %q: %w", key.Name, serr)
		}
		if cerr := r.Create(ctx, mariadb); cerr != nil {
			return false, fmt.Errorf("creating MariaDB %q: %w", key.Name, cerr)
		}
	case err != nil:
		return false, fmt.Errorf("getting MariaDB %q: %w", key.Name, err)
	default:
		// A MariaDB with this clusterRef name already exists. Two sub-cases:
		//
		//  1. It is OWNED by this ControlPlane (we created it on an earlier pass):
		//     reconcile the MUTABLE projection — spec.replicas — so a changed
		//     projection default (infraMariaDBReplicas) takes effect on the cluster
		//     we own, rather than being frozen at first-creation. spec.storage is
		//     deliberately NOT re-projected even when owned: the mariadb-operator
		//     webhook rejects changing spec.storage.* on an existing CR, so storage
		//     stays as first created.
		//
		//  2. It is NOT owned (e.g. the infrastructure stack provisions
		//     "openstack-db" under the same name): adopt it as-is and reconcile only
		//     against its status. Re-projecting our defaults would be rejected
		//     (immutable storage) or needlessly reshape a running database, and we
		//     never claim GC ownership of a resource we did not create, so deleting
		//     the ControlPlane never cascades into shared infra.
		if metav1.IsControlledBy(mariadb, cp) && mariadb.Spec.Replicas != infraMariaDBReplicas {
			mariadb.Spec.Replicas = infraMariaDBReplicas
			if uerr := r.Update(ctx, mariadb); uerr != nil {
				return false, fmt.Errorf("updating owned MariaDB %q replicas: %w", key.Name, uerr)
			}
		}
	}

	return conditions.IsReady(mariadb.Status.Conditions), nil
}

// ensureMemcached create-or-updates the owned Memcached CR named after
// spec.infrastructure.cache.clusterRef and reports whether it is Ready. The
// Memcached CR is handled as an unstructured.Unstructured because
// memcached.c5c3.io ships no Go module (see memcachedGVK).
func (r *ControlPlaneReconciler) ensureMemcached(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) (bool, error) {
	key := types.NamespacedName{
		Name:      cp.Spec.Infrastructure.Cache.ClusterRef.Name,
		Namespace: childNamespace(cp),
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(memcachedGVK)
	err := r.Get(ctx, key, u)
	switch {
	case apierrors.IsNotFound(err):
		u.SetName(key.Name)
		u.SetNamespace(key.Namespace)
		// int32 must be widened to int64 for unstructured nested-field storage.
		if serr := unstructured.SetNestedField(u.Object, int64(cp.Spec.Infrastructure.Cache.Replicas), "spec", "replicas"); serr != nil {
			return false, fmt.Errorf("setting spec.replicas: %w", serr)
		}
		if serr := controllerutil.SetControllerReference(cp, u, r.Scheme); serr != nil {
			return false, fmt.Errorf("setting owner reference on Memcached %q: %w", key.Name, serr)
		}
		if cerr := r.Create(ctx, u); cerr != nil {
			return false, fmt.Errorf("creating Memcached %q: %w", key.Name, cerr)
		}
	case err != nil:
		return false, fmt.Errorf("getting Memcached %q: %w", key.Name, err)
	default:
		// An existing Memcached. If this ControlPlane OWNS it (we created it on an
		// earlier pass), reconcile spec.replicas so a ControlPlane spec change
		// (cp.Spec.Infrastructure.Cache.Replicas) actually scales the cache we own
		// instead of being ignored after first creation. If it is a pre-existing /
		// externally-provisioned instance (NOT owned) we adopt it as-is and never
		// reshape it — same rationale as ensureMariaDB — nor claim GC ownership of
		// shared infra.
		if metav1.IsControlledBy(u, cp) {
			desired := int64(cp.Spec.Infrastructure.Cache.Replicas)
			current, found, gerr := unstructured.NestedInt64(u.Object, "spec", "replicas")
			if gerr != nil {
				return false, fmt.Errorf("reading Memcached %q spec.replicas: %w", key.Name, gerr)
			}
			if !found || current != desired {
				if serr := unstructured.SetNestedField(u.Object, desired, "spec", "replicas"); serr != nil {
					return false, fmt.Errorf("setting Memcached %q spec.replicas: %w", key.Name, serr)
				}
				if uerr := r.Update(ctx, u); uerr != nil {
					return false, fmt.Errorf("updating owned Memcached %q replicas: %w", key.Name, uerr)
				}
			}
		}
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
