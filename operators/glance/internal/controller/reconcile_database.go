// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/database"
	"github.com/c5c3/forge/internal/common/release"
	glancev1alpha1 "github.com/c5c3/forge/operators/glance/api/v1alpha1"
)

// Condition reason constants for DatabaseReady set on the release-transition
// path. The steady-state reasons live in internal/common/database as
// database.Reason*.
const (
	// conditionReasonInvalidReleaseTransition is set when the requested
	// spec.openStackRelease is an unsupported transition from the installed
	// release (a downgrade, or a jump that is neither patch-only nor a single
	// sequential step). It is terminal until the spec changes.
	conditionReasonInvalidReleaseTransition = "InvalidReleaseTransition"
	// conditionReasonWaitingForBackends mirrors the backends step's reason: the
	// schema cannot be provisioned until a ready default backend has produced a
	// rendered config to db-sync against.
	conditionReasonDatabaseWaitingForBackends = "WaitingForBackends"
)

// reconcileDatabase provisions and migrates the Glance database schema and
// tracks the installed OpenStack release. It always runs the shared provisioning
// flow (MariaDB cluster gate + Database/User/Grant in managed mode, no-op in
// brownfield), validates any release transition against the installed release
// (deliberately without keystone's expand-migrate-contract phase machine —
// Glance's schema migrations run in a single glance-manage db sync), and runs
// the db-sync Job once a rendered config exists.
func (r *GlanceReconciler) reconcileDatabase(ctx context.Context, glance *glancev1alpha1.Glance, configMapName string) (ctrl.Result, error) {
	// Managed/brownfield provisioning: MariaDB cluster gate, Database/User/Grant
	// ensure, Dynamic-credentials skip of the User/Grant. A non-zero result means
	// the flow set a not-ready condition and we must return it unchanged.
	res, err := database.ReconcileProvision(ctx, database.ProvisionFlowParams{
		Client:        r.Client,
		Scheme:        r.Scheme,
		Owner:         glance,
		InstanceName:  glance.Name,
		Namespace:     glance.Namespace,
		Database:      &glance.Spec.Database,
		Conditions:    &glance.Status.Conditions,
		Generation:    glance.Generation,
		ConditionType: "DatabaseReady",
		RequeueAfter:  RequeueDatabaseWait,
	})
	if err != nil || !res.IsZero() {
		return res, err
	}

	// Track the release being converged to. Stamped every pass so status reflects
	// the current spec even before the db-sync promotes InstalledRelease.
	glance.Status.TargetRelease = glance.Spec.OpenStackRelease

	// Validate a release transition against the installed release (skipped on a
	// fresh install or a same-release reconcile). A rejection short-circuits the
	// pipeline (non-zero requeue, no error) so the workload is NOT rolled to the
	// new code against an un-migrated schema; it stays rejected until the spec
	// changes and re-triggers the reconcile.
	if installed := glance.Status.InstalledRelease; installed != "" && installed != glance.Spec.OpenStackRelease {
		from, err := release.ParseRelease(installed)
		if err != nil {
			return r.rejectReleaseTransition(glance, fmt.Sprintf("cannot parse installed release %q: %v", installed, err))
		}
		to, err := release.ParseRelease(glance.Spec.OpenStackRelease)
		if err != nil {
			return r.rejectReleaseTransition(glance, fmt.Sprintf("cannot parse target release %q: %v", glance.Spec.OpenStackRelease, err))
		}
		if release.IsDowngrade(from, to) {
			return r.rejectReleaseTransition(glance, fmt.Sprintf("downgrade from %s to %s is not supported", from.Raw, to.Raw))
		}
		if !release.IsPatchOnly(from, to) && !release.IsSequentialUpgrade(from, to) {
			return r.rejectReleaseTransition(glance, fmt.Sprintf(
				"release transition from %s to %s is not supported: only a patch-level change or a single sequential upgrade is allowed",
				from.Raw, to.Raw,
			))
		}
	}

	// No config yet means no ready default backend has produced a glance-api.conf
	// to migrate against — glance-manage db sync needs [glance_store] and the
	// enabled_backends set. Wait rather than db-sync an incomplete config.
	if configMapName == "" {
		conditions.SetCondition(&glance.Status.Conditions, metav1.Condition{
			Type:               "DatabaseReady",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: glance.Generation,
			Reason:             conditionReasonDatabaseWaitingForBackends,
			Message:            "Waiting for a ready default backend before provisioning the database schema",
		})
		return ctrl.Result{RequeueAfter: RequeueDatabaseWait}, nil
	}

	// Steady-state db-sync. glance-manage db sync applies every pending migration
	// in one pass, so there is no schema-check step (SchemaCheckCommand nil).
	// InstalledRelease is promoted to spec.openStackRelease on Job success.
	return database.ReconcileSyncJobs(ctx, database.SyncFlowParams{
		Client:   r.Client,
		Scheme:   r.Scheme,
		Recorder: r.Recorder,
		Owner:    glance,
		Jobs:     glanceJobSetParams(glance, configMapName),
		RecordTerminal: func(jobSuffix string, observed *batchv1.Job) {
			r.recordDBJobTerminalState(ctx, glance, jobSuffix, observed)
		},
		Conditions:       &glance.Status.Conditions,
		Generation:       glance.Generation,
		ConditionType:    "DatabaseReady",
		RequeueAfter:     RequeueDatabaseWait,
		InstalledRelease: &glance.Status.InstalledRelease,
		ImageTag:         glance.Spec.OpenStackRelease,
	})
}

// glanceJobSetParams derives the shared migration-Job inputs from the Glance CR:
// the config mount, the DB-connection env override, and the glance-manage db
// sync command. The steady-state sync flow (database.ReconcileSyncJobs) consumes
// it; centralising it here lets tests build the identical Job.
func glanceJobSetParams(glance *glancev1alpha1.Glance, configMapName string) database.JobSetParams {
	return database.JobSetParams{
		InstanceName:    glance.Name,
		Namespace:       glance.Namespace,
		Image:           glance.Spec.Image.Reference(),
		ConfigMapName:   configMapName,
		ConfigMountPath: glanceConfigDir,
		// Override [database].connection via the oslo.config env-var so db-sync
		// reads the DB URL from the derived Secret instead of the ConfigMap.
		Env:         []corev1.EnvVar{database.ConnectionEnvVar(glance.Name)},
		SyncCommand: []string{"glance-manage", "--config-dir", glanceConfigDir, "db", "sync"},
		// No schema-check: glance-manage db sync is idempotent and applies all
		// pending migrations in one pass, unlike keystone's expand/migrate split.
		SchemaCheckCommand: nil,
	}
}

// rejectReleaseTransition sets DatabaseReady=False with the
// InvalidReleaseTransition reason, emits a Warning event, and returns a non-zero
// requeue (no error) so the pipeline short-circuits before the Deployment step.
// A zero result would NOT short-circuit RunPipeline, letting reconcileDeployment
// roll the workload to the new code against an un-migrated schema. The transition
// stays rejected until the spec changes and re-triggers the reconcile; the
// periodic re-confirm requeue is harmless and mirrors the sibling holding paths
// (WaitingForBackends, DBSyncInProgress), which also return RequeueDatabaseWait.
func (r *GlanceReconciler) rejectReleaseTransition(glance *glancev1alpha1.Glance, msg string) (ctrl.Result, error) {
	conditions.SetCondition(&glance.Status.Conditions, metav1.Condition{
		Type:               "DatabaseReady",
		Status:             metav1.ConditionFalse,
		ObservedGeneration: glance.Generation,
		Reason:             conditionReasonInvalidReleaseTransition,
		Message:            msg,
	})
	r.Recorder.Event(glance, corev1.EventTypeWarning, conditionReasonInvalidReleaseTransition, msg)
	return ctrl.Result{RequeueAfter: RequeueDatabaseWait}, nil
}
