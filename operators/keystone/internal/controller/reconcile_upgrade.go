// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/job"
	"github.com/c5c3/forge/internal/common/release"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// isUpgrade returns true when spec.image.tag differs from status.installedRelease
// and the change requires the expand-migrate-contract upgrade flow.
// Returns false for fresh deployments (empty installedRelease), same version,
// and patch-only changes.
func isUpgrade(keystone *keystonev1alpha1.Keystone) bool {
	// Digest-pinned images carry no tag, so the tag-keyed release/upgrade machine
	// has nothing to compare — skip release detection entirely (Decision E).
	if keystone.Spec.Image.Tag == "" {
		return false
	}
	if keystone.Status.InstalledRelease == "" {
		return false
	}
	if keystone.Spec.Image.Tag == keystone.Status.InstalledRelease {
		return false
	}

	from, err := release.ParseRelease(keystone.Status.InstalledRelease)
	if err != nil {
		// Let initiateUpgrade handle the error with proper conditions.
		return true
	}
	to, err := release.ParseRelease(keystone.Spec.Image.Tag)
	if err != nil {
		return true
	}

	return !release.IsPatchOnly(from, to)
}

// initiateUpgrade validates the upgrade path and sets the initial upgrade state.
func (r *KeystoneReconciler) initiateUpgrade(ctx context.Context, keystone *keystonev1alpha1.Keystone) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	from, err := release.ParseRelease(keystone.Status.InstalledRelease)
	if err != nil {
		conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
			Type:               "DatabaseReady",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: keystone.Generation,
			Reason:             conditionReasonVersionParseError,
			Message:            fmt.Sprintf("failed to parse installed release %q: %v", keystone.Status.InstalledRelease, err),
		})
		r.Recorder.Eventf(keystone, corev1.EventTypeWarning, conditionReasonVersionParseError, "Failed to parse installed release %q: %v", keystone.Status.InstalledRelease, err)
		return ctrl.Result{}, fmt.Errorf("parse installed release %q: %w", keystone.Status.InstalledRelease, err)
	}

	to, err := release.ParseRelease(keystone.Spec.Image.Tag)
	if err != nil {
		conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
			Type:               "DatabaseReady",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: keystone.Generation,
			Reason:             conditionReasonVersionParseError,
			Message:            fmt.Sprintf("failed to parse target release %q: %v", keystone.Spec.Image.Tag, err),
		})
		r.Recorder.Eventf(keystone, corev1.EventTypeWarning, conditionReasonVersionParseError, "Failed to parse target release %q: %v", keystone.Spec.Image.Tag, err)
		return ctrl.Result{}, fmt.Errorf("parse target release %q: %w", keystone.Spec.Image.Tag, err)
	}

	if release.IsDowngrade(from, to) {
		conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
			Type:               "DatabaseReady",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: keystone.Generation,
			Reason:             conditionReasonDowngradeNotSupported,
			Message:            fmt.Sprintf("downgrade from %s to %s is not supported", from.Raw, to.Raw),
		})
		r.Recorder.Eventf(keystone, corev1.EventTypeWarning, conditionReasonDowngradeNotSupported, "Downgrade from %s to %s is not supported", from.Raw, to.Raw)
		return ctrl.Result{}, fmt.Errorf("downgrade from %s to %s is not supported", from.Raw, to.Raw)
	}

	if !release.IsSequentialUpgrade(from, to) {
		conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
			Type:               "DatabaseReady",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: keystone.Generation,
			Reason:             conditionReasonUpgradePathInvalid,
			Message:            fmt.Sprintf("upgrade from %s to %s is not sequential; only sequential upgrades are supported", from.Raw, to.Raw),
		})
		r.Recorder.Eventf(keystone, corev1.EventTypeWarning, conditionReasonUpgradePathInvalid, "Upgrade from %s to %s is not sequential", from.Raw, to.Raw)
		return ctrl.Result{}, fmt.Errorf("upgrade from %s to %s is not sequential; only sequential upgrades are supported", from.Raw, to.Raw)
	}

	keystone.Status.TargetRelease = keystone.Spec.Image.Tag
	keystone.Status.UpgradePhase = keystonev1alpha1.UpgradePhaseExpanding
	conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
		Type:               "DatabaseReady",
		Status:             metav1.ConditionFalse,
		ObservedGeneration: keystone.Generation,
		Reason:             conditionReasonExpandInProgress,
		Message:            fmt.Sprintf("Upgrade detected: %s \u2192 %s (phase: Expanding)", from.Raw, to.Raw),
	})

	logger.Info("Upgrade detected", "from", from.Raw, "to", to.Raw, "phase", keystonev1alpha1.UpgradePhaseExpanding)
	r.Recorder.Eventf(keystone, corev1.EventTypeNormal, conditionReasonUpgradeInitiated, "Upgrade initiated: %s \u2192 %s", from.Raw, to.Raw)
	return ctrl.Result{Requeue: true}, nil
}

// setUpgradePhaseRunning sets DatabaseReady=False with reason "{phase}InProgress"
// for an upgrade phase that is currently executing.
func setUpgradePhaseRunning(keystone *keystonev1alpha1.Keystone, phase string) {
	conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
		Type:               "DatabaseReady",
		Status:             metav1.ConditionFalse,
		ObservedGeneration: keystone.Generation,
		Reason:             phase + "InProgress",
		Message:            fmt.Sprintf("%s phase running: %s \u2192 %s", phase, keystone.Status.InstalledRelease, keystone.Status.TargetRelease),
	})
}

// setUpgradeJobFailed sets DatabaseReady=False with reason "{phase}Failed"
// when an upgrade job fails.
func setUpgradeJobFailed(keystone *keystonev1alpha1.Keystone, phase, jobName string, err error) {
	conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
		Type:               "DatabaseReady",
		Status:             metav1.ConditionFalse,
		ObservedGeneration: keystone.Generation,
		Reason:             phase + "Failed",
		Message:            fmt.Sprintf("%s job %s failed: %v", phase, jobName, err),
	})
}

// reconcileUpgrade handles phased database migration during an active upgrade.
// It dispatches to the handler for the current UpgradePhase.
func (r *KeystoneReconciler) reconcileUpgrade(ctx context.Context, keystone *keystonev1alpha1.Keystone, configMapName string) (ctrl.Result, error) {
	switch keystone.Status.UpgradePhase {
	case keystonev1alpha1.UpgradePhaseExpanding:
		return r.reconcileExpand(ctx, keystone, configMapName)
	case keystonev1alpha1.UpgradePhaseMigrating:
		return r.reconcileMigrate(ctx, keystone, configMapName)
	case keystonev1alpha1.UpgradePhaseRollingUpdate:
		return r.reconcileRollingUpdate(ctx, keystone)
	case keystonev1alpha1.UpgradePhaseContracting:
		return r.reconcileContract(ctx, keystone, configMapName)
	default:
		return ctrl.Result{}, fmt.Errorf("unknown upgrade phase %q", keystone.Status.UpgradePhase)
	}
}

// upgradePhaseStep describes one job-running upgrade phase. The expand, migrate,
// and contract phases share an identical build/run/record/fail/requeue skeleton
// (runUpgradePhase) and differ only in the phase name, the Job builder, the
// failure event reason, and what completion does.
type upgradePhaseStep struct {
	// name is the phase name ("Expand"); it derives the "{name}InProgress" and
	// "{name}Failed" condition reasons and the failure event/error wording.
	name string
	// jobSuffix is the recordDBJobTerminalState key ("db-expand").
	jobSuffix string
	// buildJob builds the phase Job for the given image tag.
	buildJob func(keystone *keystonev1alpha1.Keystone, configMapName, imageTag string) *batchv1.Job
	// failReason is the Warning event reason emitted when the Job fails.
	failReason string
	// onComplete runs the phase-specific transition once the Job succeeds.
	onComplete func(r *KeystoneReconciler, ctx context.Context, keystone *keystonev1alpha1.Keystone) (ctrl.Result, error)
}

// runUpgradePhase executes the shared build/run/record/fail/requeue skeleton for
// one job-running upgrade phase, delegating the phase-specific transition to
// step.onComplete. The Job runs with the target image (spec.image.tag);
// see reconcileExpand for why expand and migrate also use the new release.
func (r *KeystoneReconciler) runUpgradePhase(ctx context.Context, keystone *keystonev1alpha1.Keystone, configMapName string, step upgradePhaseStep) (ctrl.Result, error) {
	phaseJob := step.buildJob(keystone, configMapName, keystone.Spec.Image.Tag)
	done, observed, err := job.RunJob(ctx, r.Client, r.Scheme, keystone, phaseJob)
	// Emit db_sync metrics for the phase Job so the dashboard panel and
	// failure-rate alerts continue to observe activity during upgrades. The Job
	// RunJob already read is threaded in to avoid a re-Get.
	r.recordDBJobTerminalState(ctx, keystone, step.jobSuffix, observed)
	if err != nil {
		setUpgradeJobFailed(keystone, step.name, phaseJob.Name, err)
		r.Recorder.Eventf(keystone, corev1.EventTypeWarning, step.failReason, "%s job %s failed: %v", step.name, phaseJob.Name, err)
		return ctrl.Result{}, fmt.Errorf("running %s job: %w", strings.ToLower(step.name), err)
	}
	if !done {
		setUpgradePhaseRunning(keystone, step.name)
		return ctrl.Result{RequeueAfter: RequeueUpgradeWait}, nil
	}
	return step.onComplete(r, ctx, keystone)
}

// reconcileExpand runs the db_sync --expand Job using the NEW image (spec.image.tag).
//
// Per the OpenStack Keystone rolling upgrade procedure, expand migrations are
// executed with the N+1 (target) code: the N+1 alembic tree owns the schema
// deltas for the upgrade, and running expand with the N (old) binary only
// advances the expand head to the old release's HEAD. A subsequent contract
// run with the new binary then fails keystone's _validate_upgrade_order check
// with "upgrade contract ahead of expand".
func (r *KeystoneReconciler) reconcileExpand(ctx context.Context, keystone *keystonev1alpha1.Keystone, configMapName string) (ctrl.Result, error) {
	return r.runUpgradePhase(ctx, keystone, configMapName, upgradePhaseStep{
		name:       "Expand",
		jobSuffix:  "db-expand",
		buildJob:   buildExpandJob,
		failReason: conditionReasonExpandFailed,
		onComplete: (*KeystoneReconciler).completeExpand,
	})
}

// completeExpand transitions the upgrade from Expanding to Migrating after the
// expand Job succeeds.
func (r *KeystoneReconciler) completeExpand(ctx context.Context, keystone *keystonev1alpha1.Keystone) (ctrl.Result, error) {
	// Expand complete \u2014 transition to Migrating.
	keystone.Status.UpgradePhase = keystonev1alpha1.UpgradePhaseMigrating
	conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
		Type:               "DatabaseReady",
		Status:             metav1.ConditionFalse,
		ObservedGeneration: keystone.Generation,
		Reason:             conditionReasonMigrateInProgress,
		Message:            fmt.Sprintf("Expand complete, starting migrate: %s \u2192 %s", keystone.Status.InstalledRelease, keystone.Status.TargetRelease),
	})
	r.Recorder.Eventf(keystone, corev1.EventTypeNormal, conditionReasonExpandComplete, "Expand phase complete: %s \u2192 %s", keystone.Status.InstalledRelease, keystone.Status.TargetRelease)
	return ctrl.Result{Requeue: true}, nil
}

// reconcileMigrate runs the db_sync --migrate Job using the NEW image (spec.image.tag).
//
// Like expand, migrate is executed with the N+1 (target) code so that the
// alembic data-migration steps defined in the new release are applied (see
// reconcileExpand for the full rationale).
func (r *KeystoneReconciler) reconcileMigrate(ctx context.Context, keystone *keystonev1alpha1.Keystone, configMapName string) (ctrl.Result, error) {
	return r.runUpgradePhase(ctx, keystone, configMapName, upgradePhaseStep{
		name:       "Migrate",
		jobSuffix:  "db-migrate",
		buildJob:   buildMigrateJob,
		failReason: conditionReasonMigrateFailed,
		onComplete: (*KeystoneReconciler).completeMigrate,
	})
}

// completeMigrate transitions the upgrade from Migrating to RollingUpdate after
// the migrate Job succeeds.
func (r *KeystoneReconciler) completeMigrate(ctx context.Context, keystone *keystonev1alpha1.Keystone) (ctrl.Result, error) {
	// Migrate complete \u2014 transition to RollingUpdate.
	keystone.Status.UpgradePhase = keystonev1alpha1.UpgradePhaseRollingUpdate
	conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
		Type:               "DatabaseReady",
		Status:             metav1.ConditionFalse,
		ObservedGeneration: keystone.Generation,
		Reason:             conditionReasonUpgradeRollingUpdate,
		Message:            fmt.Sprintf("Migrate complete, waiting for Deployment rollout: %s \u2192 %s", keystone.Status.InstalledRelease, keystone.Status.TargetRelease),
	})
	r.Recorder.Eventf(keystone, corev1.EventTypeNormal, conditionReasonMigrateComplete, "Migrate phase complete: %s \u2192 %s", keystone.Status.InstalledRelease, keystone.Status.TargetRelease)
	return ctrl.Result{Requeue: true}, nil
}

// reconcileRollingUpdate is a pass-through that sets a condition and returns an empty
// result so the main reconcile loop proceeds to reconcileDeployment.
func (r *KeystoneReconciler) reconcileRollingUpdate(ctx context.Context, keystone *keystonev1alpha1.Keystone) (ctrl.Result, error) {
	conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
		Type:               "DatabaseReady",
		Status:             metav1.ConditionFalse,
		ObservedGeneration: keystone.Generation,
		Reason:             conditionReasonUpgradeRollingUpdate,
		Message:            fmt.Sprintf("Waiting for Deployment rollout: %s \u2192 %s", keystone.Status.InstalledRelease, keystone.Status.TargetRelease),
	})
	return ctrl.Result{}, nil
}

// reconcileContract runs the db_sync --contract Job using the NEW image (spec.image.tag)
// and finalizes the upgrade on completion.
func (r *KeystoneReconciler) reconcileContract(ctx context.Context, keystone *keystonev1alpha1.Keystone, configMapName string) (ctrl.Result, error) {
	return r.runUpgradePhase(ctx, keystone, configMapName, upgradePhaseStep{
		name:       "Contract",
		jobSuffix:  "db-contract",
		buildJob:   buildContractJob,
		failReason: conditionReasonContractFailed,
		onComplete: (*KeystoneReconciler).completeContract,
	})
}

// completeContract finalizes the upgrade after the contract Job succeeds: it
// promotes TargetRelease to InstalledRelease, clears the upgrade state, and
// reports DatabaseReady=True.
func (r *KeystoneReconciler) completeContract(ctx context.Context, keystone *keystonev1alpha1.Keystone) (ctrl.Result, error) {
	// Contract complete \u2014 upgrade finished.
	from := keystone.Status.InstalledRelease
	to := keystone.Status.TargetRelease
	keystone.Status.InstalledRelease = to
	keystone.Status.TargetRelease = ""
	keystone.Status.UpgradePhase = ""
	conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
		Type:               "DatabaseReady",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: keystone.Generation,
		Reason:             conditionReasonDatabaseSynced,
		Message:            fmt.Sprintf("Database schema is up to date (upgraded %s \u2192 %s)", from, to),
	})
	r.Recorder.Eventf(keystone, corev1.EventTypeNormal, conditionReasonUpgradeComplete, "Upgrade complete: %s \u2192 %s", from, to)
	log.FromContext(ctx).Info("Upgrade complete", "from", from, "to", to)
	return ctrl.Result{}, nil
}
