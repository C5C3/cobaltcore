// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/job"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// isUpgrade returns true when spec.image.tag differs from status.installedRelease
// and the change requires the expand-migrate-contract upgrade flow.
// Returns false for fresh deployments (empty installedRelease), same version,
// and patch-only changes.
func isUpgrade(keystone *keystonev1alpha1.Keystone) bool {
	if keystone.Status.InstalledRelease == "" {
		return false
	}
	if keystone.Spec.Image.Tag == keystone.Status.InstalledRelease {
		return false
	}

	from, err := ParseRelease(keystone.Status.InstalledRelease)
	if err != nil {
		// Let initiateUpgrade handle the error with proper conditions.
		return true
	}
	to, err := ParseRelease(keystone.Spec.Image.Tag)
	if err != nil {
		return true
	}

	return !IsPatchOnly(from, to)
}

// initiateUpgrade validates the upgrade path and sets the initial upgrade state.
func (r *KeystoneReconciler) initiateUpgrade(ctx context.Context, keystone *keystonev1alpha1.Keystone) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	from, err := ParseRelease(keystone.Status.InstalledRelease)
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

	to, err := ParseRelease(keystone.Spec.Image.Tag)
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

	if IsDowngrade(from, to) {
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

	if !IsSequentialUpgrade(from, to) {
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

// reconcileExpand runs the db_sync --expand Job using the NEW image (spec.image.tag).
//
// Per the OpenStack Keystone rolling upgrade procedure, expand migrations are
// executed with the N+1 (target) code: the N+1 alembic tree owns the schema
// deltas for the upgrade, and running expand with the N (old) binary only
// advances the expand head to the old release's HEAD. A subsequent contract
// run with the new binary then fails keystone's _validate_upgrade_order check
// with "upgrade contract ahead of expand".
func (r *KeystoneReconciler) reconcileExpand(ctx context.Context, keystone *keystonev1alpha1.Keystone, configMapName string) (ctrl.Result, error) {
	expandJob := buildExpandJob(keystone, configMapName, keystone.Spec.Image.Tag)
	done, err := job.RunJob(ctx, r.Client, r.Scheme, keystone, expandJob)
	// Emit db_sync metrics for the expand-phase Job so the dashboard panel
	// and failure-rate alerts continue to observe activity during upgrades.
	r.recordDBJobTerminalState(ctx, keystone, "db-expand")
	if err != nil {
		setUpgradeJobFailed(keystone, "Expand", expandJob.Name, err)
		r.Recorder.Eventf(keystone, corev1.EventTypeWarning, conditionReasonExpandFailed, "Expand job %s failed: %v", expandJob.Name, err)
		return ctrl.Result{}, fmt.Errorf("running expand job: %w", err)
	}
	if !done {
		setUpgradePhaseRunning(keystone, "Expand")
		return ctrl.Result{RequeueAfter: RequeueUpgradeWait}, nil
	}

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
	migrateJob := buildMigrateJob(keystone, configMapName, keystone.Spec.Image.Tag)
	done, err := job.RunJob(ctx, r.Client, r.Scheme, keystone, migrateJob)
	// Emit db_sync metrics for the migrate-phase Job so the dashboard panel
	// and failure-rate alerts continue to observe activity during upgrades.
	r.recordDBJobTerminalState(ctx, keystone, "db-migrate")
	if err != nil {
		setUpgradeJobFailed(keystone, "Migrate", migrateJob.Name, err)
		r.Recorder.Eventf(keystone, corev1.EventTypeWarning, conditionReasonMigrateFailed, "Migrate job %s failed: %v", migrateJob.Name, err)
		return ctrl.Result{}, fmt.Errorf("running migrate job: %w", err)
	}
	if !done {
		setUpgradePhaseRunning(keystone, "Migrate")
		return ctrl.Result{RequeueAfter: RequeueUpgradeWait}, nil
	}

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
	contractJob := buildContractJob(keystone, configMapName, keystone.Spec.Image.Tag)
	done, err := job.RunJob(ctx, r.Client, r.Scheme, keystone, contractJob)
	// Emit db_sync metrics for the contract-phase Job so the dashboard panel
	// and failure-rate alerts continue to observe activity during upgrades.
	r.recordDBJobTerminalState(ctx, keystone, "db-contract")
	if err != nil {
		setUpgradeJobFailed(keystone, "Contract", contractJob.Name, err)
		r.Recorder.Eventf(keystone, corev1.EventTypeWarning, conditionReasonContractFailed, "Contract job %s failed: %v", contractJob.Name, err)
		return ctrl.Result{}, fmt.Errorf("running contract job: %w", err)
	}
	if !done {
		setUpgradePhaseRunning(keystone, "Contract")
		return ctrl.Result{RequeueAfter: RequeueUpgradeWait}, nil
	}

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
