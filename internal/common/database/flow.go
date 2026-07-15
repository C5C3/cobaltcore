// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package database

import (
	"context"
	"fmt"
	"time"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/job"
	commonv1 "github.com/c5c3/forge/internal/common/types"
)

// Condition reason constants for the steady-state database-readiness condition,
// shared so every database-backed operator's condition uses the same
// vocabulary. The expand-migrate-contract upgrade reasons stay operator-private
// because they are parameterised by the upgrade phase name.
const (
	ReasonClusterNotReady       = "ClusterNotReady"
	ReasonWaitingForDatabase    = "WaitingForDatabase"
	ReasonDBSyncFailed          = "DBSyncFailed"
	ReasonDBSyncInProgress      = "DBSyncInProgress"
	ReasonSchemaDriftDetected   = "SchemaDriftDetected"
	ReasonSchemaCheckInProgress = "SchemaCheckInProgress"
	ReasonDatabaseSynced        = "DatabaseSynced"
)

// IsClusterReady returns whether the MariaDB cluster referenced by
// db.ClusterRef currently reports a Ready=True condition. It returns
// (false, nil) when the cluster CR does not yet exist or is not ready, and
// surfaces transient errors so the reconciler can retry. Keeping this check on
// the provisioning path ensures the operator reflects upstream database outages
// in the readiness condition rather than caching the last sync result forever.
func IsClusterReady(ctx context.Context, c client.Client, db *commonv1.DatabaseSpec, namespace string) (bool, error) {
	cluster := &mariadbv1alpha1.MariaDB{}
	key := client.ObjectKey{Namespace: namespace, Name: db.ClusterRef.Name}
	if err := c.Get(ctx, key, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("getting MariaDB %s: %w", key, err)
	}
	return conditions.IsReady(cluster.Status.Conditions), nil
}

// ProvisionFlowParams carries everything ReconcileProvision needs. The
// service-specific parts — the owner CR, the instance naming, the DatabaseSpec,
// and the condition vocabulary — are supplied by the caller; the managed/
// brownfield provisioning branch itself is identical across operators.
type ProvisionFlowParams struct {
	Client client.Client
	Scheme *runtime.Scheme
	// Owner is the CR whose owner reference is set on the provisioned MariaDB
	// CRs.
	Owner client.Object
	// InstanceName is the Database/User/Grant resource name and the Static-mode
	// SQL username. Namespace is their namespace.
	InstanceName string
	Namespace    string
	// Labels are applied to the provisioned MariaDB CRs; nil leaves them
	// unlabelled.
	Labels map[string]string
	// Database is the shared DatabaseSpec (managed vs brownfield, credentials
	// mode).
	Database *commonv1.DatabaseSpec
	// Conditions is the CR's condition slice, mutated in place.
	Conditions *[]metav1.Condition
	// Generation is stamped onto every condition the flow writes.
	Generation int64
	// ConditionType is the readiness condition the flow reports on (for example
	// "DatabaseReady").
	ConditionType string
	// RequeueAfter is the requeue interval while waiting for the cluster or the
	// MariaDB CRs to become Ready.
	RequeueAfter time.Duration
}

// ReconcileProvision ensures the MariaDB Database, User, and Grant CRs exist and
// are Ready. In managed mode (ClusterRef set) it gates on the MariaDB cluster
// health, ensures the Database, and — unless the credentials mode is Dynamic —
// ensures the User and Grant. In brownfield mode (ClusterRef nil) it is a no-op:
// no MariaDB CRs are provisioned.
//
// A zero ctrl.Result with a nil error means provisioning is complete and the
// caller should continue to the sync path; a non-zero result means the flow set
// a not-ready condition and the caller must return it unchanged.
func ReconcileProvision(ctx context.Context, p ProvisionFlowParams) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Brownfield mode: the database is external, no MariaDB CRs are managed.
	if p.Database.ClusterRef == nil {
		return ctrl.Result{}, nil
	}

	clusterReady, err := IsClusterReady(ctx, p.Client, p.Database, p.Namespace)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("checking MariaDB cluster readiness: %w", err)
	}
	if !clusterReady {
		logger.Info("MariaDB cluster not ready, requeuing", "cluster", p.Database.ClusterRef.Name)
		conditions.SetCondition(p.Conditions, metav1.Condition{
			Type:               p.ConditionType,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: p.Generation,
			Reason:             ReasonClusterNotReady,
			Message:            fmt.Sprintf("MariaDB cluster %q is not ready", p.Database.ClusterRef.Name),
		})
		return ctrl.Result{RequeueAfter: p.RequeueAfter}, nil
	}

	pp := ProvisionParams{
		Name:               p.InstanceName,
		Namespace:          p.Namespace,
		Labels:             p.Labels,
		ClusterRef:         p.Database.ClusterRef.Name,
		DatabaseName:       p.Database.Database,
		PasswordSecretName: p.Database.SecretRef.Name,
	}

	dbReady, err := EnsureDatabase(ctx, p.Client, p.Scheme, p.Owner, BuildDatabase(pp))
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring Database: %w", err)
	}
	if !dbReady {
		logger.Info("MariaDB Database not ready, requeuing")
		conditions.SetCondition(p.Conditions, metav1.Condition{
			Type:               p.ConditionType,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: p.Generation,
			Reason:             ReasonWaitingForDatabase,
			Message:            "MariaDB Database CR is not ready",
		})
		return ctrl.Result{RequeueAfter: p.RequeueAfter}, nil
	}

	// In Dynamic credentials mode the OpenBao database engine owns the DB user
	// lifecycle: it issues short-lived MySQL users on demand (via the role's
	// creation_statements) and revokes them at lease end, so the operator does
	// NOT provision a MariaDB User/Grant CR. The engine's GRANT covers the same
	// database, so the schema (EnsureDatabase above) is still operator-managed.
	//
	// A pre-existing operator-provisioned User/Grant (from a Static deployment
	// mid-migration) is intentionally NOT deleted here so its grant overlaps the
	// engine-issued logins for a downtime-free cutover; retiring the static user
	// is a documented migration step (see the migration guide).
	if p.Database.CredentialsMode != commonv1.CredentialsModeDynamic {
		userReady, err := EnsureDatabaseUser(ctx, p.Client, p.Scheme, p.Owner, BuildUser(pp), BuildGrant(pp))
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("ensuring database user: %w", err)
		}
		if !userReady {
			logger.Info("MariaDB User/Grant not ready, requeuing")
			conditions.SetCondition(p.Conditions, metav1.Condition{
				Type:               p.ConditionType,
				Status:             metav1.ConditionFalse,
				ObservedGeneration: p.Generation,
				Reason:             ReasonWaitingForDatabase,
				Message:            "MariaDB User or Grant CR is not ready",
			})
			return ctrl.Result{RequeueAfter: p.RequeueAfter}, nil
		}
	}

	return ctrl.Result{}, nil
}

// SyncFlowParams carries everything ReconcileSyncJobs needs. The service bits —
// the built Job set, the terminal-metric callback, the condition vocabulary, and
// the installed-release marker — are supplied by the caller; the db-sync then
// schema-check sequencing is identical across operators.
type SyncFlowParams struct {
	Client   client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	// Owner is the CR that owns the migration Jobs.
	Owner client.Object
	// Jobs carries the service-specific Job bits (image, config mount, commands,
	// extra volumes). A nil Jobs.SchemaCheckCommand skips the schema-check step.
	Jobs JobSetParams
	// RecordTerminal, when non-nil, is invoked with the Job phase suffix
	// ("db-sync"/"schema-check") and the Job observed this pass, so the caller can
	// emit its service-specific terminal metric. It is called on every RunJob
	// return, including failure.
	RecordTerminal func(jobSuffix string, observed *batchv1.Job)
	// Conditions is the CR's condition slice, mutated in place.
	Conditions *[]metav1.Condition
	// Generation is stamped onto every condition the flow writes.
	Generation int64
	// ConditionType is the readiness condition the flow reports on.
	ConditionType string
	// RequeueAfter is the requeue interval while a migration Job is in progress.
	RequeueAfter time.Duration
	// InstalledRelease, when non-nil, is set to ImageTag after a successful sync
	// (and schema check). ImageTag is empty for digest-pinned images, which
	// disables release tracking, so the marker is left untouched then.
	InstalledRelease *string
	ImageTag         string
}

// recordTerminal invokes the caller's terminal-metric callback when set.
func (p SyncFlowParams) recordTerminal(jobSuffix string, observed *batchv1.Job) {
	if p.RecordTerminal != nil {
		p.RecordTerminal(jobSuffix, observed)
	}
}

// ReconcileSyncJobs runs the db-sync Job and, when configured, the schema-check
// Job, then promotes the installed-release marker and reports the readiness
// condition. It is the shared steady-state body of every database-backed
// operator's reconcileDatabase; the operator supplies the built Job set and the
// condition vocabulary. A permanently-failed db-sync/schema-check Job returns a
// hard error (exponential backoff); an in-progress Job requeues.
func ReconcileSyncJobs(ctx context.Context, p SyncFlowParams) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	done, observed, err := job.RunJob(ctx, p.Client, p.Scheme, p.Owner, SyncJob(p.Jobs))
	// Emit db_sync metrics on the terminal-transition observation path
	// regardless of (done, err). The Job RunJob already read is threaded in to
	// avoid a re-Get.
	p.recordTerminal("db-sync", observed)
	if err != nil {
		conditions.SetCondition(p.Conditions, metav1.Condition{
			Type:               p.ConditionType,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: p.Generation,
			Reason:             ReasonDBSyncFailed,
			Message:            fmt.Sprintf("db_sync job failed: %v", err),
		})
		if p.Recorder != nil {
			p.Recorder.Eventf(p.Owner, corev1.EventTypeWarning, ReasonDBSyncFailed, "db_sync job failed: %v", err)
		}
		return ctrl.Result{}, fmt.Errorf("running db_sync: %w", err)
	}
	if !done {
		logger.Info("db_sync job in progress, requeuing")
		conditions.SetCondition(p.Conditions, metav1.Condition{
			Type:               p.ConditionType,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: p.Generation,
			Reason:             ReasonDBSyncInProgress,
			Message:            "db_sync job is running",
		})
		return ctrl.Result{RequeueAfter: p.RequeueAfter}, nil
	}

	// Schema drift detection after db_sync, when the service defines a check.
	if p.Jobs.SchemaCheckCommand != nil {
		done, observed, err = job.RunJob(ctx, p.Client, p.Scheme, p.Owner, SchemaCheckJob(p.Jobs))
		// Same terminal-transition emission rule as db_sync above so
		// dashboards/alerts observe schema-check failures.
		p.recordTerminal("schema-check", observed)
		if err != nil {
			conditions.SetCondition(p.Conditions, metav1.Condition{
				Type:               p.ConditionType,
				Status:             metav1.ConditionFalse,
				ObservedGeneration: p.Generation,
				Reason:             ReasonSchemaDriftDetected,
				Message:            fmt.Sprintf("schema-check job failed: %v", err),
			})
			if p.Recorder != nil {
				p.Recorder.Eventf(p.Owner, corev1.EventTypeWarning, ReasonSchemaDriftDetected, "schema-check job failed: %v", err)
			}
			return ctrl.Result{}, fmt.Errorf("running schema-check: %w", err)
		}
		if !done {
			logger.Info("schema-check job in progress, requeuing")
			conditions.SetCondition(p.Conditions, metav1.Condition{
				Type:               p.ConditionType,
				Status:             metav1.ConditionFalse,
				ObservedGeneration: p.Generation,
				Reason:             ReasonSchemaCheckInProgress,
				Message:            "schema-check job is running",
			})
			return ctrl.Result{RequeueAfter: p.RequeueAfter}, nil
		}
	}

	// Track installed release after successful db_sync and schema check. A
	// digest-pinned image has no tag, so leave the marker untouched — digest
	// mode disables release tracking/upgrades (Decision E).
	if p.InstalledRelease != nil && p.ImageTag != "" {
		*p.InstalledRelease = p.ImageTag
	}

	conditions.SetCondition(p.Conditions, metav1.Condition{
		Type:               p.ConditionType,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: p.Generation,
		Reason:             ReasonDatabaseSynced,
		Message:            "Database schema is up to date (revision verified)",
	})
	if p.Recorder != nil {
		p.Recorder.Event(p.Owner, corev1.EventTypeNormal, ReasonDatabaseSynced, "Database schema is up to date")
	}
	return ctrl.Result{}, nil
}
