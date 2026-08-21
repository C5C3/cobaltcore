// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package database

import (
	"context"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	"github.com/c5c3/cobaltcore/internal/common/job"
	"github.com/c5c3/cobaltcore/internal/common/release"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

// Condition and event reason constants for the expand-migrate-contract upgrade
// reasons shared by the upgrade flow. Unlike the steady-state Reason* set above,
// these drive the phased release-upgrade branch every database-backed operator
// runs through ReconcileUpgrade. The dynamic "{phase}InProgress"/"{phase}Failed"
// reasons runUpgradePhase computes from the phase name coincide with the
// matching constants here.
const (
	ReasonUpgradeTargetChanged      = "UpgradeTargetChanged"
	ReasonVersionParseError         = "VersionParseError"
	ReasonDowngradeNotSupported     = "DowngradeNotSupported"
	ReasonUpgradePathInvalid        = "UpgradePathInvalid"
	ReasonExpandInProgress          = "ExpandInProgress"
	ReasonExpandFailed              = "ExpandFailed"
	ReasonExpandComplete            = "ExpandComplete"
	ReasonMigrateInProgress         = "MigrateInProgress"
	ReasonMigrateFailed             = "MigrateFailed"
	ReasonMigrateComplete           = "MigrateComplete"
	ReasonContractInProgress        = "ContractInProgress"
	ReasonContractFailed            = "ContractFailed"
	ReasonUpgradeRollingUpdate      = "UpgradeRollingUpdate"
	ReasonUpgradeInitiated          = "UpgradeInitiated"
	ReasonUpgradeComplete           = "UpgradeComplete"
	ReasonUpgradeAborted            = "UpgradeAborted"
	ReasonDeploymentRolloutComplete = "DeploymentRolloutComplete"
)

// upgradeJobSuffixes lists the BuildJob nameSuffixes for the Jobs created during
// the expand-migrate-contract upgrade flow. AbortUpgrade deletes exactly these
// (and not db-sync or schema-check, which belong to the steady-state path) so
// reverting an in-flight upgrade leaves no stale phase Jobs behind (#468).
var upgradeJobSuffixes = []string{"db-expand", "db-migrate", "db-contract"}

// UpgradeFlowParams carries everything the expand-migrate-contract upgrade flow
// needs. The service-specific parts — the owner CR, the condition vocabulary,
// the spec-side release string, the phase Job builder, and the terminal-metric
// callback — are supplied by the caller; the phase choreography itself is
// identical across operators. The three status pointers (Phase, InstalledRelease,
// TargetRelease) are mutated in place so the caller persists them on the CR after
// the flow returns.
type UpgradeFlowParams struct {
	Client client.Client
	Scheme *runtime.Scheme
	// Recorder records the upgrade lifecycle events on Owner.
	Recorder record.EventRecorder
	// Owner is the CR that owns the phase Jobs and receives the events.
	Owner client.Object
	// Conditions is the CR's condition slice, mutated in place.
	Conditions *[]metav1.Condition
	// Generation is stamped onto every condition the flow writes.
	Generation int64
	// ConditionType is the readiness condition the flow reports on (for example
	// "DatabaseReady").
	ConditionType string
	// RequeueAfter is the requeue interval while a phase Job is in progress.
	RequeueAfter time.Duration
	// Phase is the CR's current upgrade phase, mutated in place as the flow
	// advances (Expanding -> Migrating -> RollingUpdate -> Contracting -> "").
	Phase *commonv1.UpgradePhase
	// InstalledRelease is the CR's installed-release marker, promoted to
	// TargetRelease when the contract phase completes.
	InstalledRelease *string
	// TargetRelease is the CR's in-flight upgrade target, set on initiation and
	// cleared on completion or abort.
	TargetRelease *string
	// SpecRelease is the release the spec currently requests (keystone:
	// spec.image.tag; glance: spec.openStackRelease). It is the single seam the
	// flow generalizes over.
	SpecRelease string
	// BuildPhaseJob builds the migration Job for the given phase. The caller pins
	// the correct release image and manage command per phase; the flow only runs
	// and observes the returned Job.
	BuildPhaseJob func(phase commonv1.UpgradePhase) *batchv1.Job
	// RecordTerminal, when non-nil, is invoked with the phase Job suffix
	// ("db-expand"/"db-migrate"/"db-contract") and the Job observed this pass so
	// the caller can emit its service-specific terminal metric.
	RecordTerminal func(jobSuffix string, observed *batchv1.Job)
}

// IsUpgrade returns true when specRelease differs from the installed release and
// the change requires the expand-migrate-contract upgrade flow. It returns false
// for fresh deployments (empty installed release), an empty spec release, the
// same release, and patch-only changes. On a parse error of either side it
// returns true so InitiateUpgrade surfaces the error with a proper condition.
func IsUpgrade(specRelease, installedRelease string) bool {
	// A digest-pinned image carries no release string, so the release/upgrade
	// machine has nothing to compare — skip release detection entirely.
	if specRelease == "" {
		return false
	}
	if installedRelease == "" {
		return false
	}
	if specRelease == installedRelease {
		return false
	}

	from, err := release.ParseRelease(installedRelease)
	if err != nil {
		// Let InitiateUpgrade handle the error with proper conditions.
		return true
	}
	to, err := release.ParseRelease(specRelease)
	if err != nil {
		return true
	}

	return !release.IsPatchOnly(from, to)
}

// InitiateUpgrade validates the upgrade path and sets the initial upgrade state.
// It parses the installed then the target release (reporting VersionParseError),
// rejects downgrades and non-sequential jumps, and on success stamps
// TargetRelease, sets Phase to Expanding, and requeues so ReconcileUpgrade runs
// the first phase on the next pass.
func InitiateUpgrade(ctx context.Context, p UpgradeFlowParams) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	from, err := release.ParseRelease(*p.InstalledRelease)
	if err != nil {
		conditions.SetCondition(p.Conditions, metav1.Condition{
			Type:               p.ConditionType,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: p.Generation,
			Reason:             ReasonVersionParseError,
			Message:            fmt.Sprintf("failed to parse installed release %q: %v", *p.InstalledRelease, err),
		})
		p.Recorder.Eventf(p.Owner, corev1.EventTypeWarning, ReasonVersionParseError, "Failed to parse installed release %q: %v", *p.InstalledRelease, err)
		return ctrl.Result{}, fmt.Errorf("parse installed release %q: %w", *p.InstalledRelease, err)
	}

	to, err := release.ParseRelease(p.SpecRelease)
	if err != nil {
		conditions.SetCondition(p.Conditions, metav1.Condition{
			Type:               p.ConditionType,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: p.Generation,
			Reason:             ReasonVersionParseError,
			Message:            fmt.Sprintf("failed to parse target release %q: %v", p.SpecRelease, err),
		})
		p.Recorder.Eventf(p.Owner, corev1.EventTypeWarning, ReasonVersionParseError, "Failed to parse target release %q: %v", p.SpecRelease, err)
		return ctrl.Result{}, fmt.Errorf("parse target release %q: %w", p.SpecRelease, err)
	}

	if release.IsDowngrade(from, to) {
		conditions.SetCondition(p.Conditions, metav1.Condition{
			Type:               p.ConditionType,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: p.Generation,
			Reason:             ReasonDowngradeNotSupported,
			Message:            fmt.Sprintf("downgrade from %s to %s is not supported", from.Raw, to.Raw),
		})
		p.Recorder.Eventf(p.Owner, corev1.EventTypeWarning, ReasonDowngradeNotSupported, "Downgrade from %s to %s is not supported", from.Raw, to.Raw)
		return ctrl.Result{}, fmt.Errorf("downgrade from %s to %s is not supported", from.Raw, to.Raw)
	}

	if !release.IsSequentialUpgrade(from, to) {
		conditions.SetCondition(p.Conditions, metav1.Condition{
			Type:               p.ConditionType,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: p.Generation,
			Reason:             ReasonUpgradePathInvalid,
			Message:            fmt.Sprintf("upgrade from %s to %s is not sequential; only sequential upgrades are supported", from.Raw, to.Raw),
		})
		p.Recorder.Eventf(p.Owner, corev1.EventTypeWarning, ReasonUpgradePathInvalid, "Upgrade from %s to %s is not sequential", from.Raw, to.Raw)
		return ctrl.Result{}, fmt.Errorf("upgrade from %s to %s is not sequential; only sequential upgrades are supported", from.Raw, to.Raw)
	}

	*p.TargetRelease = p.SpecRelease
	*p.Phase = commonv1.UpgradePhaseExpanding
	conditions.SetCondition(p.Conditions, metav1.Condition{
		Type:               p.ConditionType,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: p.Generation,
		Reason:             ReasonExpandInProgress,
		Message:            fmt.Sprintf("Upgrade detected: %s → %s (phase: Expanding)", from.Raw, to.Raw),
	})

	logger.Info("Upgrade detected", "from", from.Raw, "to", to.Raw, "phase", commonv1.UpgradePhaseExpanding)
	p.Recorder.Eventf(p.Owner, corev1.EventTypeNormal, ReasonUpgradeInitiated, "Upgrade initiated: %s → %s", from.Raw, to.Raw)
	return ctrl.Result{Requeue: true}, nil
}

// ReconcileUpgrade drives an active upgrade. It first handles an abort (the spec
// release reverted to the installed release), then guards against the spec
// release changing to a third value mid-upgrade, then dispatches to the handler
// for the current phase. Expand, migrate, and contract run a Job via
// runUpgradePhase; RollingUpdate is a pass-through that lets the caller proceed
// to its Deployment reconcile.
func ReconcileUpgrade(ctx context.Context, p UpgradeFlowParams) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Abort path: reverting the spec release back to the installed release cancels
	// the in-flight upgrade and unblocks a stuck or permanently failed
	// expand/migrate phase (#468). Checked before the target-changed guard below
	// because a revert also satisfies SpecRelease != TargetRelease and would
	// otherwise return a hard error.
	if p.SpecRelease == *p.InstalledRelease {
		return AbortUpgrade(ctx, p)
	}

	// Detect a spec-release change to a third value during an active upgrade.
	if p.SpecRelease != *p.TargetRelease {
		logger.Info("Spec release changed during active upgrade",
			"targetRelease", *p.TargetRelease,
			"newRelease", p.SpecRelease,
			"phase", *p.Phase)
		conditions.SetCondition(p.Conditions, metav1.Condition{
			Type:               p.ConditionType,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: p.Generation,
			Reason:             ReasonUpgradeTargetChanged,
			Message: fmt.Sprintf("Spec release changed to %s during active upgrade %s → %s; complete or roll back the current upgrade first",
				p.SpecRelease, *p.InstalledRelease, *p.TargetRelease),
		})
		p.Recorder.Eventf(p.Owner, corev1.EventTypeWarning, ReasonUpgradeTargetChanged, "Spec release changed to %s during active upgrade %s → %s", p.SpecRelease, *p.InstalledRelease, *p.TargetRelease)
		return ctrl.Result{}, fmt.Errorf("spec release changed during active upgrade: current upgrade targets %s but spec release is %s",
			*p.TargetRelease, p.SpecRelease)
	}

	switch *p.Phase {
	case commonv1.UpgradePhaseExpanding:
		return runUpgradePhase(ctx, p, upgradePhaseStep{
			name:       "Expand",
			jobSuffix:  "db-expand",
			phase:      commonv1.UpgradePhaseExpanding,
			failReason: ReasonExpandFailed,
			onComplete: completeExpand,
		})
	case commonv1.UpgradePhaseMigrating:
		return runUpgradePhase(ctx, p, upgradePhaseStep{
			name:       "Migrate",
			jobSuffix:  "db-migrate",
			phase:      commonv1.UpgradePhaseMigrating,
			failReason: ReasonMigrateFailed,
			onComplete: completeMigrate,
		})
	case commonv1.UpgradePhaseRollingUpdate:
		conditions.SetCondition(p.Conditions, metav1.Condition{
			Type:               p.ConditionType,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: p.Generation,
			Reason:             ReasonUpgradeRollingUpdate,
			Message:            fmt.Sprintf("Waiting for Deployment rollout: %s → %s", *p.InstalledRelease, *p.TargetRelease),
		})
		return ctrl.Result{}, nil
	case commonv1.UpgradePhaseContracting:
		return runUpgradePhase(ctx, p, upgradePhaseStep{
			name:       "Contract",
			jobSuffix:  "db-contract",
			phase:      commonv1.UpgradePhaseContracting,
			failReason: ReasonContractFailed,
			onComplete: completeContract,
		})
	default:
		return ctrl.Result{}, fmt.Errorf("unknown upgrade phase %q", *p.Phase)
	}
}

// upgradePhaseStep describes one job-running upgrade phase. The expand, migrate,
// and contract phases share an identical build/run/record/fail/requeue skeleton
// (runUpgradePhase) and differ only in the phase name, the phase constant the
// Job builder keys off, the failure event reason, and what completion does.
type upgradePhaseStep struct {
	// name is the phase name ("Expand"); it derives the "{name}InProgress" and
	// "{name}Failed" condition reasons and the failure event/error wording.
	name string
	// jobSuffix is the RecordTerminal key ("db-expand").
	jobSuffix string
	// phase is the UpgradePhase constant passed to BuildPhaseJob so the caller
	// builds the matching Job.
	phase commonv1.UpgradePhase
	// failReason is the Warning event reason emitted when the Job fails.
	failReason string
	// onComplete runs the phase-specific transition once the Job succeeds.
	onComplete func(ctx context.Context, p UpgradeFlowParams) (ctrl.Result, error)
}

// runUpgradePhase executes the shared build/run/record/fail/requeue skeleton for
// one job-running upgrade phase, delegating the phase-specific transition to
// step.onComplete.
//
// The phase Job is built by the caller's BuildPhaseJob, which runs it with the
// target release: per the OpenStack rolling-upgrade procedure the N+1 (target)
// migration tree owns the schema deltas, so expand and migrate are executed with
// the new binary. Running expand with the old binary only advances the expand
// head to the old release's HEAD, and a later contract run with the new binary
// then fails the service's upgrade-order validation.
func runUpgradePhase(ctx context.Context, p UpgradeFlowParams, step upgradePhaseStep) (ctrl.Result, error) {
	phaseJob := p.BuildPhaseJob(step.phase)
	done, observed, err := job.RunJob(ctx, p.Client, p.Scheme, p.Owner, phaseJob)
	// Emit db_sync metrics for the phase Job so the dashboard panel and
	// failure-rate alerts continue to observe activity during upgrades. The Job
	// RunJob already read is threaded in to avoid a re-Get.
	if p.RecordTerminal != nil {
		p.RecordTerminal(step.jobSuffix, observed)
	}
	if err != nil {
		setUpgradeJobFailed(p, step.name, phaseJob.Name, err)
		p.Recorder.Eventf(p.Owner, corev1.EventTypeWarning, step.failReason, "%s job %s failed: %v", step.name, phaseJob.Name, err)
		return ctrl.Result{}, fmt.Errorf("running %s job: %w", strings.ToLower(step.name), err)
	}
	if !done {
		setUpgradePhaseRunning(p, step.name)
		return ctrl.Result{RequeueAfter: p.RequeueAfter}, nil
	}
	return step.onComplete(ctx, p)
}

// setUpgradePhaseRunning sets the readiness condition to False with reason
// "{phase}InProgress" for an upgrade phase that is currently executing.
func setUpgradePhaseRunning(p UpgradeFlowParams, phase string) {
	conditions.SetCondition(p.Conditions, metav1.Condition{
		Type:               p.ConditionType,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: p.Generation,
		Reason:             phase + "InProgress",
		Message:            fmt.Sprintf("%s phase running: %s → %s", phase, *p.InstalledRelease, *p.TargetRelease),
	})
}

// setUpgradeJobFailed sets the readiness condition to False with reason
// "{phase}Failed" when an upgrade job fails.
func setUpgradeJobFailed(p UpgradeFlowParams, phase, jobName string, err error) {
	conditions.SetCondition(p.Conditions, metav1.Condition{
		Type:               p.ConditionType,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: p.Generation,
		Reason:             phase + "Failed",
		Message:            fmt.Sprintf("%s job %s failed: %v", phase, jobName, err),
	})
}

// completeExpand transitions the upgrade from Expanding to Migrating after the
// expand Job succeeds.
func completeExpand(_ context.Context, p UpgradeFlowParams) (ctrl.Result, error) {
	*p.Phase = commonv1.UpgradePhaseMigrating
	conditions.SetCondition(p.Conditions, metav1.Condition{
		Type:               p.ConditionType,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: p.Generation,
		Reason:             ReasonMigrateInProgress,
		Message:            fmt.Sprintf("Expand complete, starting migrate: %s → %s", *p.InstalledRelease, *p.TargetRelease),
	})
	p.Recorder.Eventf(p.Owner, corev1.EventTypeNormal, ReasonExpandComplete, "Expand phase complete: %s → %s", *p.InstalledRelease, *p.TargetRelease)
	return ctrl.Result{Requeue: true}, nil
}

// completeMigrate transitions the upgrade from Migrating to RollingUpdate after
// the migrate Job succeeds.
func completeMigrate(_ context.Context, p UpgradeFlowParams) (ctrl.Result, error) {
	*p.Phase = commonv1.UpgradePhaseRollingUpdate
	conditions.SetCondition(p.Conditions, metav1.Condition{
		Type:               p.ConditionType,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: p.Generation,
		Reason:             ReasonUpgradeRollingUpdate,
		Message:            fmt.Sprintf("Migrate complete, waiting for Deployment rollout: %s → %s", *p.InstalledRelease, *p.TargetRelease),
	})
	p.Recorder.Eventf(p.Owner, corev1.EventTypeNormal, ReasonMigrateComplete, "Migrate phase complete: %s → %s", *p.InstalledRelease, *p.TargetRelease)
	return ctrl.Result{Requeue: true}, nil
}

// completeContract finalizes the upgrade after the contract Job succeeds: it
// promotes TargetRelease to InstalledRelease, clears the upgrade state, and
// reports the readiness condition True.
func completeContract(ctx context.Context, p UpgradeFlowParams) (ctrl.Result, error) {
	from := *p.InstalledRelease
	to := *p.TargetRelease
	*p.InstalledRelease = to
	*p.TargetRelease = ""
	*p.Phase = ""
	conditions.SetCondition(p.Conditions, metav1.Condition{
		Type:               p.ConditionType,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: p.Generation,
		Reason:             ReasonDatabaseSynced,
		Message:            fmt.Sprintf("Database schema is up to date (upgraded %s → %s)", from, to),
	})
	p.Recorder.Eventf(p.Owner, corev1.EventTypeNormal, ReasonUpgradeComplete, "Upgrade complete: %s → %s", from, to)
	log.FromContext(ctx).Info("Upgrade complete", "from", from, "to", to)
	return ctrl.Result{}, nil
}

// AbortUpgrade cancels an in-flight upgrade when the spec release is reverted to
// the installed release. It deletes the expand/migrate/contract Jobs, clears the
// upgrade phase and target release, emits an UpgradeAborted event, and requeues
// so the steady-state sync path takes over and re-derives readiness on the next
// pass. This is the only escape from an upgrade wedged on a stuck or permanently
// failed expand/migrate Job (#468).
//
// Aborting is only safe before the contract phase has run. Expand and migrate
// are additive by design: they create the new columns/tables and backfill data
// without dropping anything the installed release still reads, so the
// pre-contract schema is a superset both releases run against. Contract drops
// the now-unused columns; once it has started, the installed release would be
// pointed at a schema missing fields it expects, so a post-contract revert is
// not a safe rollback. The reverted release is the operator's responsibility to
// validate. The flow does not gate the abort on the current phase.
func AbortUpgrade(ctx context.Context, p UpgradeFlowParams) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	abortedTarget := *p.TargetRelease
	abortedPhase := *p.Phase

	// Delete the phase Jobs before clearing status so a transient delete error
	// leaves the upgrade state intact and the next reconcile retries the abort
	// rather than dropping into the steady-state path with orphaned Jobs (#468).
	if err := deleteUpgradeJobs(ctx, p); err != nil {
		return ctrl.Result{}, fmt.Errorf("deleting upgrade jobs during abort: %w", err)
	}

	*p.Phase = ""
	*p.TargetRelease = ""

	logger.Info("Upgrade aborted: spec release reverted to installed release",
		"installedRelease", *p.InstalledRelease,
		"abortedTarget", abortedTarget,
		"abortedPhase", abortedPhase)
	p.Recorder.Eventf(p.Owner, corev1.EventTypeNormal, ReasonUpgradeAborted,
		"Upgrade %s → %s aborted: spec release reverted to installed release %s",
		*p.InstalledRelease, abortedTarget, *p.InstalledRelease)
	return ctrl.Result{Requeue: true}, nil
}

// deleteUpgradeJobs deletes the expand/migrate/contract Jobs owned by the CR.
// NotFound is tolerated so the call is idempotent across requeues, and Background
// propagation removes each Job's owned Pods too rather than orphaning them
// (#468).
func deleteUpgradeJobs(ctx context.Context, p UpgradeFlowParams) error {
	logger := log.FromContext(ctx)
	for _, suffix := range upgradeJobSuffixes {
		jobName := fmt.Sprintf("%s-%s", p.Owner.GetName(), suffix)
		j := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:      jobName,
				Namespace: p.Owner.GetNamespace(),
			},
		}
		err := p.Client.Delete(ctx, j, client.PropagationPolicy(metav1.DeletePropagationBackground))
		switch {
		case apierrors.IsNotFound(err):
			logger.V(1).Info("upgrade phase job already absent during abort", "job", jobName)
		case err != nil:
			return fmt.Errorf("deleting %s: %w", jobName, err)
		default:
			logger.V(1).Info("deleted upgrade phase job during abort", "job", jobName)
		}
	}
	return nil
}

// CompleteRollingUpdate transitions a RollingUpdate-phase upgrade to Contracting
// once the caller's Deployment rollout has completed. It is a no-op returning
// false for every other phase (including the empty steady-state phase). On the
// RollingUpdate phase it advances Phase to Contracting, emits the
// DeploymentRolloutComplete event, and returns true; the caller stamps its own
// DeploymentReady condition and requeues so ReconcileUpgrade runs the contract
// phase on the next pass.
func CompleteRollingUpdate(ctx context.Context, p UpgradeFlowParams) bool {
	if *p.Phase != commonv1.UpgradePhaseRollingUpdate {
		return false
	}
	*p.Phase = commonv1.UpgradePhaseContracting
	p.Recorder.Eventf(p.Owner, corev1.EventTypeNormal, ReasonDeploymentRolloutComplete, "Deployment rollout complete during upgrade %s → %s", *p.InstalledRelease, *p.TargetRelease)
	log.FromContext(ctx).Info("Deployment rollout complete, transitioning to contract phase",
		"from", *p.InstalledRelease, "to", *p.TargetRelease)
	return true
}
