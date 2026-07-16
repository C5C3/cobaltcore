// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package controller implements the Glance and GlanceBackend reconcilers.
package controller

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonreconcile "github.com/c5c3/forge/internal/common/reconcile"
	"github.com/c5c3/forge/internal/common/watch"
	glancev1alpha1 "github.com/c5c3/forge/operators/glance/api/v1alpha1"
)

// This file carries the field-index plumbing shared by the Glance and
// GlanceBackend controllers. The GlanceReconciler and its sub-reconciler chain
// are added below the index section in a later commit; keeping the index keys,
// extractors, and registration helpers here mirrors keystone_controller.go,
// where both controllers rely on a single index-registration site
// (GlanceReconciler.SetupWithManager, next commit) run before the backend
// controller starts.

// GlanceSecretNameIndexKey is the field-indexer key under which Glance CRs are
// indexed by the union of their referenced Secret names
// (spec.serviceUser.secretRef.name and spec.database.secretRef.name). Used by
// SetupWithManager to register the indexer and by secretToGlanceMapper to
// perform an O(1) reverse lookup instead of an unfiltered List of all Glance
// CRs in the namespace.
// #nosec G101 -- field-indexer key (a JSONPath-like field selector), not a credential.
const GlanceSecretNameIndexKey = "spec.secretRefs.name"

// glanceSecretNameExtractor is the controller-runtime IndexerFunc registered
// under GlanceSecretNameIndexKey. It returns the deduplicated, non-empty union
// of Secret names referenced by a Glance CR — currently
// spec.serviceUser.secretRef.name and spec.database.secretRef.name — so the
// field indexer can resolve a Secret event to the referencing CR(s) without
// listing every Glance in the namespace. It mirrors keystone's extractor for
// the database block (spec.database.secretRef.name only).
func glanceSecretNameExtractor(obj client.Object) []string {
	g, ok := obj.(*glancev1alpha1.Glance)
	if !ok {
		// controller-runtime should never call us with the wrong type; a nil
		// return is safer than a panic if it ever does.
		return nil
	}
	serviceUser := g.Spec.ServiceUser.SecretRef.Name
	dbName := g.Spec.Database.SecretRef.Name

	names := make([]string, 0, 2)
	if serviceUser != "" {
		names = append(names, serviceUser)
	}
	if dbName != "" && dbName != serviceUser {
		names = append(names, dbName)
	}
	return names
}

// registerGlanceIndexes registers the Glance field indexer under
// GlanceSecretNameIndexKey with the given FieldIndexer.
// GlanceReconciler.SetupWithManager calls this once against
// mgr.GetFieldIndexer() (next commit) so secretToGlanceMapper can resolve a
// Secret event to the referencing Glance CRs via an O(1) reverse lookup instead
// of an unfiltered namespace-scoped List.
func registerGlanceIndexes(ctx context.Context, indexer client.FieldIndexer) error {
	return watch.RegisterSecretNameIndex(ctx, indexer, &glancev1alpha1.Glance{}, GlanceSecretNameIndexKey, glanceSecretNameExtractor)
}

// GlanceBackendGlanceRefIndexKey is the field-indexer key under which
// GlanceBackend CRs are indexed by spec.glanceRef.name. Used by the glance-side
// sub-reconciler (list the backends attached to one Glance) and
// glanceToGlanceBackendsMapper (fan a Glance event out to its backends).
const GlanceBackendGlanceRefIndexKey = "spec.glanceRef.name"

// GlanceBackendSecretNameIndexKey is the field-indexer key under which
// GlanceBackend CRs are indexed by their referenced S3 credentials Secret name.
// Used by the Secret mapper so a credential Secret change re-renders the owning
// Glance's config through the backend's parent.
// #nosec G101 -- field-indexer key (a JSONPath-like field selector), not a credential.
const GlanceBackendSecretNameIndexKey = "spec.secretRefs.name"

// glanceBackendGlanceRefExtractor is the controller-runtime IndexerFunc
// registered under GlanceBackendGlanceRefIndexKey: it maps a backend to its
// spec.glanceRef.name so an attached-backends list is an O(1) indexed lookup.
// Exported to tests so fake clients can register the identical index.
func glanceBackendGlanceRefExtractor(obj client.Object) []string {
	b, ok := obj.(*glancev1alpha1.GlanceBackend)
	if !ok || b.Spec.GlanceRef.Name == "" {
		return nil
	}
	return []string{b.Spec.GlanceRef.Name}
}

// glanceBackendSecretNameExtractor returns the S3 credentials Secret name a
// GlanceBackend references (nil for a wrong-type object or a nil S3 block).
// Indexing it means a rotated credential re-renders the owning Glance's config
// through the backend's parent.
func glanceBackendSecretNameExtractor(obj client.Object) []string {
	b, ok := obj.(*glancev1alpha1.GlanceBackend)
	if !ok || b.Spec.S3 == nil || b.Spec.S3.CredentialsSecretRef.Name == "" {
		return nil
	}
	return []string{b.Spec.S3.CredentialsSecretRef.Name}
}

// registerGlanceBackendIndexes registers the two GlanceBackend field indexers.
// It lives beside registerGlanceIndexes so index registration has a single
// site: GlanceReconciler.SetupWithManager runs before
// GlanceBackendReconciler.SetupWithManager (next commit), so both controllers
// can rely on the indexes. The returned error is wrapped with the index key so
// the registration site is identifiable in manager-startup failure logs.
func registerGlanceBackendIndexes(ctx context.Context, indexer client.FieldIndexer) error {
	if err := indexer.IndexField(ctx, &glancev1alpha1.GlanceBackend{}, GlanceBackendGlanceRefIndexKey,
		glanceBackendGlanceRefExtractor); err != nil {
		return fmt.Errorf("registering field indexer %q: %w", GlanceBackendGlanceRefIndexKey, err)
	}
	if err := indexer.IndexField(ctx, &glancev1alpha1.GlanceBackend{}, GlanceBackendSecretNameIndexKey,
		glanceBackendSecretNameExtractor); err != nil {
		return fmt.Errorf("registering field indexer %q: %w", GlanceBackendSecretNameIndexKey, err)
	}
	return nil
}

// subConditionTypes lists the condition types set by the individual Glance
// sub-reconcilers. The aggregate Ready condition is True only when all of these
// are True. This commit lands the config/database chain; the remaining
// workload conditions (DeploymentReady, GlanceAPIReady, HPAReady,
// NetworkPolicyReady, HTTPRouteReady) join the list with their sub-reconcilers
// in the next commit.
var subConditionTypes = []string{
	"SecretsReady",
	"BackendsReady",
	"DatabaseReady",
}

// GlanceReconciler reconciles a Glance object. Its fields mirror
// KeystoneReconciler's core set; the pipeline (Reconcile/SetupWithManager) and
// the Gateway/cert-manager availability flags land with the workload
// sub-reconcilers in the next commit.
type GlanceReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// OperatorNamespace is the Namespace the operator Pod runs in (resolved at
	// startup by bootstrap.DetectOperatorNamespace). The networkpolicy step
	// (next commit) appends an ingress peer for this Namespace so the operator's
	// own health check can reach the Glance API. Empty when the namespace could
	// not be determined, in which case no operator-namespace peer is added.
	OperatorNamespace string

	// MaxConcurrentReconciles bounds how many Glance CRs reconcile concurrently.
	// It is threaded from the --max-concurrent-reconciles flag and applied to
	// the controller's controller.Options in SetupWithManager (next commit). A
	// value <= 0 falls back to bootstrap.DefaultMaxConcurrentReconciles inside
	// bootstrap.ControllerOptions, so the zero value is safe.
	MaxConcurrentReconciles int
}

// glanceSkeleton bundles the shared controller-skeleton glue (Ready
// aggregation, no-op-skipping status writes, config-failure marking) with
// glance's sub-condition vocabulary and status accessor. The wrapper helpers
// below delegate to it; the pipeline wiring that also uses it (updateStatus,
// RunParallelGroup) lands in the next commit.
var glanceSkeleton = commonreconcile.Skeleton[*glancev1alpha1.Glance, glancev1alpha1.GlanceStatus]{
	SubConditionTypes: subConditionTypes,
	Conditions:        func(g *glancev1alpha1.Glance) *[]metav1.Condition { return &g.Status.Conditions },
}

// conditionReasonConfigError is the SecretsReady=False reason set when
// reconcileConfig fails. Config artefacts (the rendered glance-api.conf
// ConfigMap) gate the same downstream graph as the upstream credential
// Secrets, so failures reuse SecretsReady rather than a dedicated condition —
// matching reconcileDBConnectionSecret's Config→SecretsReady mapping.
const conditionReasonConfigError = "ConfigError"

// markConfigFailed flips SecretsReady to False so a reconcileConfig failure
// cannot leave the aggregate Ready condition stale-True at the new
// ObservedGeneration. It mirrors keystone's markConfigFailed helper.
func markConfigFailed(glance *glancev1alpha1.Glance, err error) {
	glanceSkeleton.MarkFailed(glance, "SecretsReady", conditionReasonConfigError, err)
}
