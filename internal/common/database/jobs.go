// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package database

import (
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/c5c3/forge/internal/common/deployment"
	"github.com/c5c3/forge/internal/common/job"
)

// Backoff limits for the steady-state migration Jobs. db-sync retries a handful
// of times against a transiently-unavailable database; the read-only
// schema-check needs fewer retries.
const (
	syncJobBackoffLimit        int32 = 4
	schemaCheckJobBackoffLimit int32 = 2
)

// JobSetParams carries the service-specific inputs of the database migration
// Jobs (db-sync, schema-check, and the expand/migrate/contract phases built via
// BuildJob). Every database-backed service operator runs the same Job skeleton
// against the same config mount and DB-connection env; only the instance name,
// image, mount path, extra volumes, and the manage command differ. The operator
// supplies those; the builders assemble the batch/v1 Jobs with the restricted
// security context every workload uses.
type JobSetParams struct {
	// InstanceName is the CR instance name; every Job is named
	// "<InstanceName>-<suffix>".
	InstanceName string
	// Namespace is the Job namespace.
	Namespace string
	// Image is the fully-qualified image reference the steady-state Jobs run
	// (SyncJob/SchemaCheckJob). BuildJob callers (the upgrade phases) pass an
	// explicit image so they can pin the old/new release independently.
	Image string
	// ConfigMapName is the rendered config ConfigMap, mounted read-only at
	// ConfigMountPath.
	ConfigMapName   string
	ConfigMountPath string
	// Env are extra environment variables (typically the DB-connection URL
	// override).
	Env []corev1.EnvVar
	// ExtraVolumes and ExtraVolumeMounts are appended after the config volume
	// (for example a DB-TLS keypair or per-domain config).
	ExtraVolumes      []corev1.Volume
	ExtraVolumeMounts []corev1.VolumeMount
	// PriorityClassName sets the Pod priority class; empty leaves it unset.
	PriorityClassName string
	// SyncCommand is the schema-migration command SyncJob runs (for example
	// keystone-manage db_sync or glance-manage db sync).
	SyncCommand []string
	// SchemaCheckCommand is the read-only drift-check command SchemaCheckJob
	// runs. A nil command means the service has no schema-check step and
	// ReconcileSyncJobs skips it.
	SchemaCheckCommand []string
}

// BuildJob constructs a migration Job with the shared container spec, config
// mount, extra volumes, and restricted security context. It is the single
// builder every db_sync variant (db-sync, schema-check, expand/migrate/contract)
// delegates to, so those Jobs never drift from one another.
//
// image is the fully-qualified reference the Job runs: the steady-state Jobs use
// p.Image (which honors a pinned digest), while the upgrade phases pass a
// specific "repo:tag" so they can pin the old/new release image independently of
// p.Image. nameSuffix is both the Job-name suffix ("<InstanceName>-<suffix>")
// and the single container's name.
func BuildJob(p JobSetParams, image, nameSuffix string, command []string, backoffLimit int32) *batchv1.Job {
	return job.BuildMigrationJob(job.MigrationJobParams{
		Name:              p.InstanceName + "-" + nameSuffix,
		Namespace:         p.Namespace,
		Image:             image,
		ContainerName:     nameSuffix,
		Command:           command,
		ConfigMapName:     p.ConfigMapName,
		ConfigMountPath:   p.ConfigMountPath,
		Env:               p.Env,
		ExtraVolumes:      p.ExtraVolumes,
		ExtraVolumeMounts: p.ExtraVolumeMounts,
		PriorityClassName: p.PriorityClassName,
		BackoffLimit:      backoffLimit,
		SecurityContext:   deployment.RestrictedSecurityContext(),
	})
}

// SyncJob builds the schema-migration Job ("<InstanceName>-db-sync") that runs
// p.SyncCommand against p.Image.
func SyncJob(p JobSetParams) *batchv1.Job {
	return BuildJob(p, p.Image, "db-sync", p.SyncCommand, syncJobBackoffLimit)
}

// SchemaCheckJob builds the read-only drift-check Job ("<InstanceName>-schema-
// check") that runs p.SchemaCheckCommand against p.Image after db-sync completes.
// It uses a lower backoff limit than the sync Job. TTLSecondsAfterFinished is
// intentionally left unset: the completed Job is the RunJob state record, and a
// TTL-driven garbage-collection would re-create it on the next reconcile,
// causing a re-creation loop (#415). The Job is cleaned up via owner-reference
// GC with the owning CR.
func SchemaCheckJob(p JobSetParams) *batchv1.Job {
	return BuildJob(p, p.Image, "schema-check", p.SchemaCheckCommand, schemaCheckJobBackoffLimit)
}
