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
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	"github.com/c5c3/cobaltcore/internal/common/database"
	"github.com/c5c3/cobaltcore/internal/common/release"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	glancev1alpha1 "github.com/c5c3/cobaltcore/operators/glance/api/v1alpha1"
)

// Condition reason constants for DatabaseReady set on the Glance-specific paths.
// The steady-state and expand-migrate-contract upgrade reasons live in
// internal/common/database as database.Reason*.
const (
	// conditionReasonWaitingForBackends mirrors the backends step's reason: the
	// schema cannot be provisioned until a ready default backend has produced a
	// rendered config to db-sync against.
	conditionReasonDatabaseWaitingForBackends = "WaitingForBackends"
	// conditionReasonImageReleaseMismatch flags the operator error where the
	// tag-pinned spec.image names a different OpenStack release than
	// spec.openStackRelease. Release tracking, the API launch mode, and the
	// expand-migrate-contract upgrade detection all key on spec.openStackRelease
	// while every migration Job and the Deployment run spec.image; the reconcile
	// refuses to advance until the two agree rather than promoting an installed-
	// release marker for a release the image is not.
	conditionReasonImageReleaseMismatch = "ImageReleaseMismatch"
)

// reconcileDatabase provisions and migrates the Glance database schema and
// tracks the installed OpenStack release. It always runs the shared provisioning
// flow (MariaDB cluster gate + Database/User/Grant in managed mode, no-op in
// brownfield). A release transition (a non-patch change from the installed
// release) runs the shared expand-migrate-contract upgrade flow
// (internal/common/database), walking Expanding → Migrating → RollingUpdate →
// Contracting with glance-manage db expand|migrate|contract phase Jobs; fresh
// installs and patch bumps stay on the single-pass glance-manage db sync path.
// Both run only once a rendered config exists.
func (r *GlanceReconciler) reconcileDatabase(ctx context.Context, children client.Client, glance *glancev1alpha1.Glance, configMapName string) (ctrl.Result, error) {
	// Managed/brownfield provisioning: MariaDB cluster gate, Database/User/Grant
	// ensure, Dynamic-credentials skip of the User/Grant. A non-zero result means
	// the flow set a not-ready condition and we must return it unchanged.
	res, err := database.ReconcileProvision(ctx, database.ProvisionFlowParams{
		Client:        children,
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

	// No config yet means no ready default backend has produced a glance-api.conf
	// to migrate against — glance-manage db sync (and the expand/migrate/contract
	// phase Jobs) needs [glance_store] and the enabled_backends set, so a phase
	// Job must never build against an empty config mount. Wait rather than migrate
	// an incomplete config. The gate runs before the upgrade branches below;
	// mid-upgrade an empty configMapName is unreachable anyway because the config
	// step recovers the last-good names off the live Deployment.
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

	// Active upgrade: the shared flow handles the abort (spec.openStackRelease
	// reverted to the installed release), the target-changed guard, and the
	// phase dispatch.
	if glance.Status.UpgradePhase != "" {
		// Enforce the decoupled-field contract mid-upgrade too. The shared flow's
		// target-changed guard only watches spec.openStackRelease, so a lone
		// spec.image.tag edit to an inconsistent release would otherwise slip
		// through and dispatch phase Jobs built from the wrong image (every phase
		// Job runs spec.image) while re-rendering the RollingUpdate Deployment in
		// the target release's launch mode against an image that cannot launch that
		// way. Skip the check when spec.openStackRelease has been reverted to the
		// installed release: that is the shared flow's abort trigger (SpecRelease ==
		// InstalledRelease in ReconcileUpgrade), which must stay reachable even while
		// the two fields disagree so a wedged upgrade can always be unstuck.
		if glance.Spec.OpenStackRelease != glance.Status.InstalledRelease {
			if res, blocked := checkImageReleaseMismatch(glance); blocked {
				return res, nil
			}
		}
		return database.ReconcileUpgrade(ctx, r.upgradeFlowParams(ctx, children, glance, configMapName))
	}

	// Enforce the decoupled-field contract before either the upgrade or the
	// steady-state path advances release tracking (see checkImageReleaseMismatch).
	if res, blocked := checkImageReleaseMismatch(glance); blocked {
		return res, nil
	}

	// Detect a release upgrade (patch-only and same-release changes stay on the
	// steady-state path).
	if database.IsUpgrade(glance.Spec.OpenStackRelease, glance.Status.InstalledRelease) {
		return database.InitiateUpgrade(ctx, r.upgradeFlowParams(ctx, children, glance, configMapName))
	}

	// Steady-state db-sync. glance-manage db sync applies every pending migration
	// in one pass, so there is no schema-check step (SchemaCheckCommand nil).
	// InstalledRelease is promoted to spec.openStackRelease on Job success.
	return database.ReconcileSyncJobs(ctx, database.SyncFlowParams{
		Client:   children,
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

// checkImageReleaseMismatch enforces the decoupled-field contract between
// spec.openStackRelease and spec.image. spec.openStackRelease drives release
// tracking, the API launch mode, and the upgrade detection, but every migration
// Job and the Deployment run spec.image — the two fields are deliberately separate
// (digest pinning, launch-mode selection) and nothing else enforces they agree. A
// tag-pinned image whose tag names a different OpenStack release would run the
// wrong glance-manage binary against a schema already at its own HEAD: the
// expand/migrate/contract Jobs exit 0 as no-ops while the flow promotes
// status.installedRelease to a release the pods neither run nor migrated to, and
// the RollingUpdate phase re-renders the Deployment in the target release's launch
// mode against an image that cannot launch that way.
//
// When the two disagree it sets DatabaseReady=False/ImageReleaseMismatch and
// returns (requeue, true) so the caller refuses to advance until the image is
// bumped in lockstep. A digest-pinned image (Tag == "") or a tag that does not
// parse as a release carries no comparable release string and is left to the
// operator's explicit spec.openStackRelease declaration, matching the
// digest-disables-tracking contract; the patch suffix is ignored so a patched
// image build (e.g. tag 2026.1-p1) still matches release 2026.1.
func checkImageReleaseMismatch(glance *glancev1alpha1.Glance) (ctrl.Result, bool) {
	if glance.Spec.Image.Tag == "" {
		return ctrl.Result{}, false
	}
	tagRel, tagErr := release.ParseRelease(glance.Spec.Image.Tag)
	specRel, specErr := release.ParseRelease(glance.Spec.OpenStackRelease)
	if tagErr != nil || specErr != nil {
		return ctrl.Result{}, false
	}
	if tagRel.Year == specRel.Year && tagRel.Minor == specRel.Minor {
		return ctrl.Result{}, false
	}
	conditions.SetCondition(&glance.Status.Conditions, metav1.Condition{
		Type:               "DatabaseReady",
		Status:             metav1.ConditionFalse,
		ObservedGeneration: glance.Generation,
		Reason:             conditionReasonImageReleaseMismatch,
		Message: fmt.Sprintf("spec.image.tag %q names OpenStack release %d.%d but spec.openStackRelease is %q; "+
			"bump spec.image in lockstep with spec.openStackRelease",
			glance.Spec.Image.Tag, tagRel.Year, tagRel.Minor, glance.Spec.OpenStackRelease),
	})
	return ctrl.Result{RequeueAfter: RequeueDatabaseWait}, true
}

// glanceMetadefsSourcePath is the directory the glance image ships the default
// metadata-definition JSON files at. Glance's setup.cfg installs etc/metadefs/*
// to <prefix>/etc/glance/metadefs, and the image installs glance with
// --prefix /var/lib/openstack (images/glance/Dockerfile); the two paths must
// stay in lockstep. The oslo default (/etc/glance/metadefs/) does not exist in
// the config-free image, so the db-sync Job passes this path explicitly.
const glanceMetadefsSourcePath = "/var/lib/openstack/etc/glance/metadefs"

// glanceJobSetParams derives the shared migration-Job inputs from the Glance CR:
// the config mount, the DB-connection env override, and the glance-manage db
// sync command. The steady-state sync flow (database.ReconcileSyncJobs) and the
// upgrade-phase builders (upgradeFlowParams.BuildPhaseJob) both consume it;
// centralising it here lets tests build the identical Job.
func glanceJobSetParams(glance *glancev1alpha1.Glance, configMapName string) database.JobSetParams {
	return database.JobSetParams{
		InstanceName:    glance.Name,
		Namespace:       glance.Namespace,
		Image:           glance.Spec.Image.Reference(),
		ConfigMapName:   configMapName,
		ConfigMountPath: glanceConfigDir,
		// Override [database].connection via the oslo.config env-var so db-sync
		// reads the DB URL from the derived Secret instead of the ConfigMap.
		Env: []corev1.EnvVar{database.ConnectionEnvVar(glance.Name)},
		// After the schema migration, load the default metadata-definitions
		// catalog — the managed counterpart of the classic deployment's
		// `glance-manage db_load_metadefs` step. Without it GET
		// /v2/metadefs/resource_types serves an empty catalog. The load is
		// idempotent: namespaces already in the database are skipped (no
		// --merge), so re-runs and release upgrades only insert new files.
		SyncCommand: []string{
			"/bin/sh", "-eu", "-c",
			"glance-manage --config-dir " + glanceConfigDir + " db sync && " +
				"glance-manage --config-dir " + glanceConfigDir + " db load_metadefs --path " + glanceMetadefsSourcePath,
		},
		// No schema-check: glance-manage db sync is idempotent and applies all
		// pending migrations in one pass for fresh installs and patch bumps.
		// Release upgrades instead run the shared expand-migrate-contract phase
		// machine (see upgradeFlowParams), so there is no schema-check step here.
		SchemaCheckCommand: nil,
	}
}

// upgradeFlowParams assembles the shared expand-migrate-contract upgrade-flow
// inputs for this Glance CR. The service-specific parts — the owner CR, the
// "DatabaseReady" condition vocabulary, spec.openStackRelease as the requested
// release, the per-phase Job builder, and the terminal-metric callback — are
// bound here; the phase choreography itself lives in internal/common/database.
// The three status pointers (UpgradePhase, InstalledRelease, TargetRelease) are
// mutated in place by the flow and persisted by the caller after it returns.
func (r *GlanceReconciler) upgradeFlowParams(ctx context.Context, children client.Client, glance *glancev1alpha1.Glance, configMapName string) database.UpgradeFlowParams {
	return database.UpgradeFlowParams{
		Client:           children,
		Scheme:           r.Scheme,
		Recorder:         r.Recorder,
		Owner:            glance,
		Conditions:       &glance.Status.Conditions,
		Generation:       glance.Generation,
		ConditionType:    "DatabaseReady",
		RequeueAfter:     RequeueUpgradeWait,
		Phase:            &glance.Status.UpgradePhase,
		InstalledRelease: &glance.Status.InstalledRelease,
		TargetRelease:    &glance.Status.TargetRelease,
		SpecRelease:      glance.Spec.OpenStackRelease,
		// Each phase Job runs spec.image (glance.Spec.Image.Reference()): the
		// operator's existing contract is that the image tag is bumped together
		// with spec.openStackRelease, so the new release's migration tree owns the
		// expand/migrate/contract schema deltas. backoffLimit 4 is keystone parity.
		BuildPhaseJob: func(phase commonv1.UpgradePhase) *batchv1.Job {
			switch phase {
			case commonv1.UpgradePhaseExpanding:
				return database.BuildJob(glanceJobSetParams(glance, configMapName), glance.Spec.Image.Reference(), "db-expand",
					[]string{"glance-manage", "--config-dir", glanceConfigDir, "db", "expand"}, 4)
			case commonv1.UpgradePhaseMigrating:
				return database.BuildJob(glanceJobSetParams(glance, configMapName), glance.Spec.Image.Reference(), "db-migrate",
					[]string{"glance-manage", "--config-dir", glanceConfigDir, "db", "migrate"}, 4)
			case commonv1.UpgradePhaseContracting:
				return database.BuildJob(glanceJobSetParams(glance, configMapName), glance.Spec.Image.Reference(), "db-contract",
					[]string{"glance-manage", "--config-dir", glanceConfigDir, "db", "contract"}, 4)
			case commonv1.UpgradePhaseRollingUpdate:
				// RollingUpdate builds no Job; the Deployment rollout drives it.
				return nil
			default:
				// The empty steady-state phase never reaches BuildPhaseJob.
				return nil
			}
		},
		RecordTerminal: func(jobSuffix string, observed *batchv1.Job) {
			r.recordDBJobTerminalState(ctx, glance, jobSuffix, observed)
		},
	}
}
