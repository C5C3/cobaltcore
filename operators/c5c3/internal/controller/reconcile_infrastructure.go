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
	commonv1 "github.com/c5c3/forge/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
)

// DECISION (/2.6): every child CR the ControlPlane projects
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

// DECISION the managed-mode MariaDB CR is provisioned with
// a MINIMAL but VALID spec. The mariadb-operator's webhook requires
// Storage.Size (or a VolumeClaimTemplate) — see Storage.Validate in the
// vendored v0.38.1 types — so a size is always set. Both the replica topology
// and the storage size are DERIVED from the DECLARED instance — the shared
// spec.infrastructure.database or a per-service dedicated one, which is exactly
// what lets an operator size a busy service's database independently of the
// shared cluster: its replicas drive the topology (the default 3 yields a Galera
// HA cluster matching the production baseline, a single replica a single-instance
// non-Galera MariaDB so the fresh-create path schedules on a constrained cluster
// such as a single-node kind), and its storageSize drives the volume size
// (default 100Gi mirrors deploy/flux-system/infrastructure/mariadb.yaml; kind/CI
// pins a far smaller value). TLS / issuerRefs are deliberately NOT set here: the
// baseline wires those from cluster-specific ClusterIssuers that are an
// infrastructure concern outside the aggregate's knowledge, and the keystone
// DB-client baseline reads TLS from cp.Spec.Infrastructure.Database.TLS rather
// than the MariaDB CR. The minimal spec keeps the CR admissible while leaving
// site-specific hardening to the platform team.
const (
	// infraMariaDBStorageSizeDefault is the zero-value fallback applied when
	// spec.infrastructure.database.storageSize is unset (""). The CRD default is
	// 100Gi, so this only fires when validation was bypassed (e.g. a fake-client
	// unit test that builds the CR directly); it keeps the projection admissible
	// (the mariadb-operator requires a non-empty size) and matches the production
	// baseline rather than requesting a zero-sized volume. It shares
	// commonv1.DatabaseStorageSizeDefault with the ControlPlane webhook's
	// migration normalization so the fallback and the webhook cannot disagree
	// on what "" means.
	infraMariaDBStorageSizeDefault = commonv1.DatabaseStorageSizeDefault
	// infraMariaDBReplicasDefault is the zero-value floor applied when
	// spec.infrastructure.database.replicas is unset (0). The CRD default is 3,
	// so this only fires when validation was bypassed; it keeps the projection
	// admissible (replicas >= 1) rather than creating a zero-replica MariaDB.
	infraMariaDBReplicasDefault = int32(3)
)

// memcachedGVK is the GroupVersionKind of the Memcached CR projected in managed
// cache mode. DECISION memcached.c5c3.io publishes NO Go
// module, so the Memcached child is built and applied as an
// unstructured.Unstructured rather than a typed client object. The fake client
// and the real apiserver both accept an unstructured object carrying this GVK;
// no scheme registration is required.
var memcachedGVK = schema.GroupVersionKind{
	Group:   "memcached.c5c3.io",
	Version: "v1beta1",
	Kind:    "Memcached",
}

// reconcileInfrastructure reconciles the backing services (MariaDB, Memcached)
// the ControlPlane provisions and drives the InfrastructureReady condition.
//
// That set is the instances the ControlPlane's services actually RESOLVE to
// (managedInfraInstances enumerates them): the SHARED instances in
// spec.infrastructure, and the per-service DEDICATED instances under
// services.<svc>.dedicatedBackingServices that a service opted into instead. A
// shared instance every service has opted out of has no consumer and is NOT
// provisioned. Managed mode (ClusterRef set) ensures an owned child CR per
// instance; brownfield mode (Host / Servers set) provisions nothing.
// InfrastructureReady is True once every managed child is ensured and reports
// Ready; while any child is still converging the sub-reconciler requeues with
// InfrastructureReady False. When the control plane uses only brownfield infra
// there is nothing to provision, so InfrastructureReady is True immediately.
//
// External keystone mode has NO infrastructure block at all, so the skip is
// keyed on the mode discriminator (cp.IsExternalKeystone) rather than on the
// database shape the brownfield short-circuits read.
func (r *ControlPlaneReconciler) reconcileInfrastructure(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// External-mode short-circuit: identity is managed against a pre-existing
	// Keystone, so there are no backing services to provision. Report the
	// condition True with the dedicated ExternallyManaged reason — the condition
	// SCHEMA is identical across modes, so subConditionTypes, setReadyCondition
	// and the condition_type drift guard need no mode awareness.
	//
	// The message embeds authURL, so it is bounded by truncateConditionMessage like
	// every assembled failure message: authURL's MaxLength keeps it far under the
	// apiserver's cap on the admission path, but a webhook- and CRD-bypassed CR
	// would otherwise make the WHOLE status.conditions write unpersistable — every
	// condition, not just this one — and wedge the reconciler in a backoff loop.
	if cp.IsExternalKeystone() {
		logger.Info("External keystone mode; no backing services are provisioned",
			"authURL", externalKeystoneAuthURL(cp))
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeInfrastructureReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: cp.Generation,
			Reason:             conditionReasonExternallyManaged,
			Message: truncateConditionMessage(fmt.Sprintf("External keystone mode: identity is managed against %s; "+
				"no MariaDB/Memcached is provisioned", externalKeystoneAuthURL(cp))),
		})
		return ctrl.Result{}, nil
	}

	// Nil-safety fail-safe. spec.infrastructure is optional at the Go/CRD layer
	// because External mode omits it, but the validating webhook REQUIRES it
	// outside External mode — so this branch is unreachable on the admission path
	// and only fires for a webhook-bypassed CR (direct etcd write, admission
	// misconfigured). Fail closed with a named reason rather than dereferencing
	// the nil block below.
	if cp.Spec.Infrastructure == nil {
		logger.Info("spec.infrastructure is unset on a non-External ControlPlane; refusing to provision")
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeInfrastructureReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             conditionReasonInfrastructureNotConfigured,
			Message: "spec.infrastructure is unset but services.keystone.mode is not External; " +
				"the backing services cannot be provisioned",
		})
		return ctrl.Result{RequeueAfter: infraRequeueAfter}, nil
	}

	// Ensure every managed instance FIRST, in a single pass, so a half-provisioned
	// control plane (e.g. DB created but Memcached missing) never occurs: every
	// child is created/updated before readiness is gated on. Readiness is then
	// evaluated collectively across ALL of them — the shared instances and every
	// per-service dedicated one alike, so a service's dedicated database is as
	// load-bearing for InfrastructureReady (and therefore for the projection gate
	// on the consuming service) as the shared cluster is.
	instances := r.managedInfraInstances(cp)

	var notReady *infraInstance
	for _, inst := range instances {
		ready, err := inst.ensure(ctx)
		if err != nil {
			conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
				Type:               conditionTypeInfrastructureReady,
				Status:             metav1.ConditionFalse,
				ObservedGeneration: cp.Generation,
				Reason:             inst.errorReason,
				Message:            fmt.Sprintf("ensuring %s %q (%s): %v", inst.kind, inst.name, inst.declaredAt, err),
			})
			return ctrl.Result{}, err
		}
		if !ready && notReady == nil {
			notReady = &inst
		}
	}

	if notReady != nil {
		// Report the first instance still converging. The reason stays the
		// class-level WaitingForDatabase / WaitingForCache (unchanged for the shared
		// block), and the message names the instance so an operator can tell a
		// pending dedicated database from a pending shared one.
		inst := notReady
		logger.Info("managed backing service not ready, requeuing",
			"kind", inst.kind, "cluster", inst.name, "declaredAt", inst.declaredAt)
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeInfrastructureReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             inst.waitReason,
			Message:            fmt.Sprintf("%s %q (%s) is not ready", inst.kind, inst.name, inst.declaredAt),
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

// infraInstance is one MANAGED backing-service instance the ControlPlane
// provisions and owns: an instance some service EFFECTIVELY resolves to — the
// shared database/cache from spec.infrastructure, or a per-service dedicated one.
// Brownfield instances are not represented — there is nothing to provision or
// gate readiness on.
type infraInstance struct {
	// kind and name identify the child CR; declaredAt is the spec path the
	// instance was declared at, so a condition message tells a pending dedicated
	// database from a pending shared one.
	kind       string
	name       string
	declaredAt string
	// errorReason / waitReason are the condition reasons for a failed ensure and
	// for a child still converging. They are per CLASS, not per instance, so the
	// reason vocabulary is unchanged by the dedicated opt-in.
	errorReason string
	waitReason  string
	ensure      func(context.Context) (bool, error)
}

// managedInfraInstances enumerates the managed backing-service instances of cp.
// It is the single place the set is derived, so adding a backing-service class or
// a service extends the enumeration rather than the reconcile flow around it.
//
// The set is the instances the ControlPlane's services actually RESOLVE to (the
// effective-* resolvers), NOT the set of declared blocks. That distinction is
// load-bearing once a service opts out: Keystone is the only database consumer,
// so a ControlPlane whose Keystone took a dedicated database leaves the shared
// spec.infrastructure.database with no consumer at all — and the webhook
// materializes that block (3 Galera replicas, 100Gi) whenever it is omitted.
// Enumerating declarations would provision that cluster anyway and then gate
// InfrastructureReady on it, so an instance nothing talks to could hold the whole
// control plane back. The same holds for the shared cache once every service has
// taken one of its own.
//
// Entries are deduplicated on (kind, name) — the identity of the child CR they
// resolve to. That is what the common case needs (Keystone and Horizon both on
// the shared cache ensure it once), and it also fails closed on a webhook-bypassed
// CR whose dedicated clusterRef collides with the shared one
// (validateDedicatedBackingServices rejects the duplicate at admission): without
// it, two entries would run ensure against the SAME child CR in one pass, each
// projecting a different desired topology, and — because the controller Owns()
// the child with no update predicate — each write would re-enqueue the
// ControlPlane into a self-sustaining loop of conflicting writes. First
// resolution wins.
func (r *ControlPlaneReconciler) managedInfraInstances(cp *c5c3v1alpha1.ControlPlane) []infraInstance {
	var instances []infraInstance

	seen := map[string]struct{}{}
	claim := func(kind, name string) bool {
		key := kind + "/" + name
		if _, dup := seen[key]; dup {
			return false
		}
		seen[key] = struct{}{}
		return true
	}

	addDatabase := func(db *commonv1.DatabaseSpec, declaredAt string) {
		if db == nil || db.ClusterRef == nil {
			return // absent, or brownfield: nothing to provision.
		}
		if !claim("MariaDB", db.ClusterRef.Name) {
			return
		}
		instances = append(instances, infraInstance{
			kind:        "MariaDB",
			name:        db.ClusterRef.Name,
			declaredAt:  declaredAt,
			errorReason: "MariaDBError",
			waitReason:  "WaitingForDatabase",
			ensure:      func(ctx context.Context) (bool, error) { return r.ensureMariaDB(ctx, cp, db) },
		})
	}
	addCache := func(cache *commonv1.CacheSpec, declaredAt string) {
		if cache == nil || cache.ClusterRef == nil {
			return
		}
		if !claim("Memcached", cache.ClusterRef.Name) {
			return
		}
		instances = append(instances, infraInstance{
			kind:        "Memcached",
			name:        cache.ClusterRef.Name,
			declaredAt:  declaredAt,
			errorReason: "MemcachedError",
			waitReason:  "WaitingForCache",
			ensure:      func(ctx context.Context) (bool, error) { return r.ensureMemcached(ctx, cp, cache) },
		})
	}

	addDatabase(effectiveKeystoneDatabase(cp), keystoneDatabaseDeclaredAt(cp))
	addCache(effectiveKeystoneCache(cp), keystoneCacheDeclaredAt(cp))
	addCache(effectiveHorizonCache(cp), horizonCacheDeclaredAt(cp))

	return instances
}

// The declaredAt-* helpers name the spec path the instance a service resolves to
// was declared at, so a condition message tells a pending dedicated instance from
// a pending shared one.
func keystoneDatabaseDeclaredAt(cp *c5c3v1alpha1.ControlPlane) string {
	if cp.DedicatedKeystoneDatabase() != nil {
		return "spec.services.keystone.dedicatedBackingServices.database"
	}
	return "spec.infrastructure.database"
}

func keystoneCacheDeclaredAt(cp *c5c3v1alpha1.ControlPlane) string {
	if cp.DedicatedKeystoneCache() != nil {
		return "spec.services.keystone.dedicatedBackingServices.cache"
	}
	return "spec.infrastructure.cache"
}

func horizonCacheDeclaredAt(cp *c5c3v1alpha1.ControlPlane) string {
	if cp.DedicatedHorizonCache() != nil {
		return "spec.services.horizon.dedicatedBackingServices.cache"
	}
	return "spec.infrastructure.cache"
}

// ensureMariaDB create-or-updates the owned MariaDB CR named after db.clusterRef
// and reports whether it is Ready. db is the declared instance — the shared
// spec.infrastructure.database or a per-service dedicated one; both are
// provisioned, owned, and (via the owner reference) torn down identically, which
// is what makes a dedicated instance carry the shared block's lifecycle
// guarantees rather than a parallel set of its own.
//
// It stays read-modify-write (not Server-Side Apply): the write is gated on the
// LIVE object's owner references — an owned CR has its topology re-projected,
// while an externally-provisioned CR sharing the name is adopted read-only and
// never has ownership claimed. That adoption-vs-projection decision reads live
// state, so it cannot be expressed as a pure projection of cp.Spec.
func (r *ControlPlaneReconciler) ensureMariaDB(ctx context.Context, cp *c5c3v1alpha1.ControlPlane, db *commonv1.DatabaseSpec) (bool, error) {
	key := types.NamespacedName{
		Name:      db.ClusterRef.Name,
		Namespace: childNamespace(cp),
	}
	// Derive the projected topology from the ControlPlane spec. A single replica
	// yields a single-instance MariaDB with Galera off, so a single-node kind can
	// schedule the fresh-create path; any multi-replica count (the default is 3)
	// enables the Galera clustering the production baseline uses. Floor a
	// zero/negative value (only reachable when CRD validation was bypassed) to
	// the default.
	replicas := db.Replicas
	if replicas < 1 {
		replicas = infraMariaDBReplicasDefault
	}
	galeraEnabled := replicas > 1

	// Derive the projected volume size from the spec, falling back to the
	// production baseline when the field is empty (only reachable when the CRD
	// default was bypassed). Storage is immutable on the mariadb-operator CR, so
	// this value is honoured on fresh create only and never re-projected below.
	storageSize := db.StorageSize
	if storageSize == "" {
		storageSize = infraMariaDBStorageSizeDefault
	}

	mariadb := &mariadbv1alpha1.MariaDB{}
	err := r.Get(ctx, key, mariadb)
	switch {
	case apierrors.IsNotFound(err):
		// Create fresh with the projected, spec-derived topology.
		size, perr := resource.ParseQuantity(storageSize)
		if perr != nil {
			return false, fmt.Errorf("parsing MariaDB storage size %q: %w", storageSize, perr)
		}
		mariadb.Name = key.Name
		mariadb.Namespace = key.Namespace
		mariadb.Spec.Replicas = replicas
		mariadb.Spec.Galera = &mariadbv1alpha1.Galera{Enabled: galeraEnabled}
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
		//     re-assert the spec-derived projection — spec.replicas and the derived
		//     Galera topology — so external drift on the owned cluster is corrected
		//     back to the declared topology. spec.infrastructure.database.replicas
		//     is itself immutable after creation (the ControlPlane validating
		//     webhook rejects a change), so this never scales down or toggles Galera
		//     in response to a user edit — only in response to drift. spec.storage
		//     is deliberately NOT re-projected even when owned: the mariadb-operator
		//     webhook rejects changing spec.storage.* on an existing CR, so storage
		//     stays as first created.
		//
		//  2. It is NOT owned (e.g. the infrastructure stack provisions
		//     "openstack-db" under the same name): adopt it as-is and reconcile only
		//     against its status. Re-projecting our defaults would be rejected
		//     (immutable storage) or needlessly reshape a running database, and we
		//     never claim GC ownership of a resource we did not create, so deleting
		//     the ControlPlane never cascades into shared infra.
		if metav1.IsControlledBy(mariadb, cp) {
			currentGalera := mariadb.Spec.Galera != nil && mariadb.Spec.Galera.Enabled
			if mariadb.Spec.Replicas != replicas || currentGalera != galeraEnabled {
				mariadb.Spec.Replicas = replicas
				mariadb.Spec.Galera = &mariadbv1alpha1.Galera{Enabled: galeraEnabled}
				if uerr := r.Update(ctx, mariadb); uerr != nil {
					return false, fmt.Errorf("updating owned MariaDB %q topology: %w", key.Name, uerr)
				}
			}
		}
	}

	return conditions.IsReady(mariadb.Status.Conditions), nil
}

// ensureMemcached create-or-updates the owned Memcached CR named after
// cache.clusterRef and reports whether it is Ready. cache is the declared
// instance — the shared spec.infrastructure.cache or a per-service dedicated one
// (see ensureMariaDB). The Memcached CR is handled as an
// unstructured.Unstructured because memcached.c5c3.io ships no Go module (see
// memcachedGVK).
//
// Like ensureMariaDB it stays read-modify-write: it reads the live object's
// owner references to project only onto an owned CR and adopt an externally
// provisioned one read-only (never claiming GC ownership), and it is
// unstructured, which apply.EnsureObject's typed-struct path does not cover.
func (r *ControlPlaneReconciler) ensureMemcached(ctx context.Context, cp *c5c3v1alpha1.ControlPlane, cache *commonv1.CacheSpec) (bool, error) {
	key := types.NamespacedName{
		Name:      cache.ClusterRef.Name,
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
		if serr := unstructured.SetNestedField(u.Object, int64(cache.Replicas), "spec", "replicas"); serr != nil {
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
		// (the declared instance's cache.replicas) actually scales the cache we own
		// instead of being ignored after first creation. If it is a pre-existing /
		// externally-provisioned instance (NOT owned) we adopt it as-is and never
		// reshape it — same rationale as ensureMariaDB — nor claim GC ownership of
		// shared infra.
		if metav1.IsControlledBy(u, cp) {
			desired := int64(cache.Replicas)
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
