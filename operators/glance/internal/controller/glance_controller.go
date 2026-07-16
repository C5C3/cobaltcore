// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package controller implements the Glance and GlanceBackend reconcilers.
package controller

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

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
