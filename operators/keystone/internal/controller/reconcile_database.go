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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	"github.com/c5c3/cobaltcore/internal/common/database"
	"github.com/c5c3/cobaltcore/internal/common/deployment"
	"github.com/c5c3/cobaltcore/internal/common/release"
	keystonev1alpha1 "github.com/c5c3/cobaltcore/operators/keystone/api/v1alpha1"
)

// mariaDBResourceKey returns the client.ObjectKey used for the MariaDB
// Database, User, and Grant CRs owned by keystone. Keeping the naming
// convention (keystone.Name, keystone.Namespace) in one place ensures that the
// provisioning builders, the finalizer cleanup, and the live-resource sentinel
// never disagree on the key — a future name suffix or namespace override
// changes here and propagates to every call site.
func mariaDBResourceKey(keystone *keystonev1alpha1.Keystone) client.ObjectKey {
	return client.ObjectKey{Name: keystone.Name, Namespace: keystone.Namespace}
}

// keystoneProcessExtraConnections is how many pooled connections one uWSGI
// process holds beyond one per thread. Measured on the 2025.2 image under the
// rendered uWSGI flags and a mixed issue/validate/list/write load: two
// single-threaded processes settled at 2 and 1 connections, one process with 2
// threads at 3, with 4 threads at 5. A single-threaded process only opens a
// second connection while its first is checked out, and oslo.db's pool keeps it
// afterwards, so counting one per thread undersizes such a fleet by half.
const keystoneProcessExtraConnections int32 = 1

// keystoneMaxUserConnections sizes the SQL user's max_user_connections cap for
// the CR's own topology. The API floor is pods × processes × (threads +
// keystoneProcessExtraConnections), with pods being the autoscaling ceiling when
// an HPA owns the replica count. On top of it comes one surge pod, because the
// rollout strategy (maxSurge=1, maxUnavailable=0) runs a full extra pod's workers
// alongside the fleet during an update, and two transient job connections (a
// db_sync variant and the trust-flush CronJob may overlap).
//
// A cap below the fleet's demand does not degrade gracefully: the process that
// opens the connection past it gets MySQL error 1226, GET /v3/auth/tokens answers
// 500, and every service validating tokens through keystonemiddleware turns that
// into 401s and 503s of its own. Left unsized, the mariadb-operator CRD default of
// 10 applies, which the default topology (3 replicas × 2 processes × 1 thread)
// can exceed: its six processes hold up to 12 connections.
func keystoneMaxUserConnections(keystone *keystonev1alpha1.Keystone) int32 {
	pods := deployment.EffectiveReplicas(&keystone.Spec.Deployment)
	if keystone.Spec.Autoscaling != nil {
		pods = keystone.Spec.Autoscaling.MaxReplicas
	}
	processes, threads := deployment.EffectiveUWSGIConcurrency(keystone.Spec.UWSGI)
	return (pods+1)*processes*(threads+keystoneProcessExtraConnections) + 2
}

// Condition and event reason constants for the expand-migrate-contract upgrade
// path, aliased to the shared vocabulary in internal/common/database so the
// keystone code and unit tests keep asserting the local names while the values
// stay in lockstep with the shared upgrade flow. Only the reasons keystone still
// emits locally (the target-changed guard in reconcileDatabase) or the tests
// reference by name are aliased here; every other upgrade reason is emitted by
// the shared flow directly. conditionReasonImageReleaseMismatch below is
// keystone's own: the two image guards (rejectUnchangedImageUpgrade and
// rejectAbortOnTargetBuild) emit it.
const (
	conditionReasonUpgradeTargetChanged  = database.ReasonUpgradeTargetChanged
	conditionReasonVersionParseError     = database.ReasonVersionParseError
	conditionReasonDowngradeNotSupported = database.ReasonDowngradeNotSupported
	conditionReasonUpgradePathInvalid    = database.ReasonUpgradePathInvalid
	conditionReasonExpandInProgress      = database.ReasonExpandInProgress
	conditionReasonMigrateInProgress     = database.ReasonMigrateInProgress
	conditionReasonUpgradeRollingUpdate  = database.ReasonUpgradeRollingUpdate
)

// conditionReasonImageReleaseMismatch is the DatabaseReady reason and Warning
// event reason of a release bump of spec.image.tag that leaves spec.image the
// image that migrated the installed schema, and of an abort that reverts
// spec.image.tag but leaves spec.image.digest on another build.
const conditionReasonImageReleaseMismatch = "ImageReleaseMismatch"

// reconcileDatabase ensures the Keystone database schema is provisioned and
// migrated. In managed mode (ClusterRef set) it creates MariaDB Database, User,
// and Grant CRs and waits for them to become Ready before running the db_sync
// Job. In brownfield mode (Host set) it skips the MariaDB CRs and runs db_sync
// directly. The provisioning, steady-state sync, and expand-migrate-contract
// upgrade flows are all shared (internal/common/database); only the
// keystone-specific inputs (assembled by upgradeFlowParams), the target-changed
// guard below, the two image guards (rejectAbortOnTargetBuild and
// rejectUnchangedImageUpgrade), and the recording of status.installedImage stay
// local. The image reference is recorded after a sync pass that settled
// (db-sync and schema check complete) and in the pass whose contract Job
// completed an upgrade; an aborted upgrade records nothing.
func (r *KeystoneReconciler) reconcileDatabase(ctx context.Context, children client.Client, keystone *keystonev1alpha1.Keystone, configMapName, domainsSecretName string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Managed/brownfield provisioning: MariaDB cluster gate, Database/User/Grant
	// ensure, Dynamic-credentials skip of the User/Grant. A non-zero result means
	// the flow set a not-ready condition and we must return it unchanged.
	res, err := database.ReconcileProvision(ctx, database.ProvisionFlowParams{
		Client:             children,
		Scheme:             r.Scheme,
		Owner:              keystone,
		InstanceName:       keystone.Name,
		Namespace:          keystone.Namespace,
		Database:           &keystone.Spec.Database,
		Conditions:         &keystone.Status.Conditions,
		Generation:         keystone.Generation,
		ConditionType:      "DatabaseReady",
		RequeueAfter:       RequeueDatabaseWait,
		MaxUserConnections: keystoneMaxUserConnections(keystone),
	})
	if err != nil || !res.IsZero() {
		return res, err
	}

	// Active upgrade: the shared flow handles the abort (#468) and the phase
	// dispatch. The target-changed guard stays keystone-side so the error keeps
	// the "image tag" prose the unit test pins (the shared flow phrases it
	// generically as "spec release"). The abort case — spec.image.tag reverted
	// to the installed release — bypasses the guard so the shared AbortUpgrade
	// runs; it is checked first there because a revert also satisfies
	// tag != targetRelease and would otherwise trip this guard. An abort whose
	// pinned digest still names another build than the installed one is refused
	// by rejectAbortOnTargetBuild before it reaches AbortUpgrade.
	if keystone.Status.UpgradePhase != "" {
		if keystone.Spec.Image.Tag != keystone.Status.InstalledRelease &&
			keystone.Spec.Image.Tag != keystone.Status.TargetRelease {
			logger.Info("Image tag changed during active upgrade",
				"targetRelease", keystone.Status.TargetRelease,
				"newTag", keystone.Spec.Image.Tag,
				"phase", keystone.Status.UpgradePhase)
			conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
				Type:               "DatabaseReady",
				Status:             metav1.ConditionFalse,
				ObservedGeneration: keystone.Generation,
				Reason:             conditionReasonUpgradeTargetChanged,
				Message: fmt.Sprintf("Image tag changed to %s during active upgrade %s → %s; complete or roll back the current upgrade first",
					keystone.Spec.Image.Tag, keystone.Status.InstalledRelease, keystone.Status.TargetRelease),
			})
			r.Recorder.Eventf(keystone, corev1.EventTypeWarning, conditionReasonUpgradeTargetChanged, "Image tag changed to %s during active upgrade %s → %s", keystone.Spec.Image.Tag, keystone.Status.InstalledRelease, keystone.Status.TargetRelease)
			return ctrl.Result{}, fmt.Errorf("image tag changed during active upgrade: current upgrade targets %s but spec.image.tag is %s",
				keystone.Status.TargetRelease, keystone.Spec.Image.Tag)
		}
		if err := r.rejectAbortOnTargetBuild(keystone); err != nil {
			return ctrl.Result{}, err
		}
		target := keystone.Status.TargetRelease
		res, err := database.ReconcileUpgrade(ctx, r.upgradeFlowParams(ctx, children, keystone, configMapName, domainsSecretName))
		// The contract Job completed in this pass when the installed release
		// reached the target; an abort leaves installedRelease on the old release.
		if err == nil && target != "" && keystone.Status.InstalledRelease == target {
			keystone.Status.InstalledImage = keystone.Spec.Image.Reference()
		}
		return res, err
	}

	// Detect upgrade.
	if isUpgrade(keystone) {
		if err := r.rejectUnchangedImageUpgrade(keystone); err != nil {
			return ctrl.Result{}, err
		}
		return database.InitiateUpgrade(ctx, r.upgradeFlowParams(ctx, children, keystone, configMapName, domainsSecretName))
	}

	// Non-upgrade path: db_sync then schema-check, with terminal metrics emitted
	// per phase and the installed release and image tracked on success.
	res, err = database.ReconcileSyncJobs(ctx, database.SyncFlowParams{
		Client:   children,
		Scheme:   r.Scheme,
		Recorder: r.Recorder,
		Owner:    keystone,
		Jobs:     keystoneJobSetParams(keystone, configMapName, domainsSecretName),
		RecordTerminal: func(jobSuffix string, observed *batchv1.Job) {
			r.recordDBJobTerminalState(ctx, keystone, jobSuffix, observed)
		},
		Conditions:       &keystone.Status.Conditions,
		Generation:       keystone.Generation,
		ConditionType:    "DatabaseReady",
		RequeueAfter:     RequeueDatabaseWait,
		InstalledRelease: &keystone.Status.InstalledRelease,
		ImageTag:         keystone.Spec.Image.Tag,
	})
	if err == nil && res.IsZero() {
		keystone.Status.InstalledImage = keystone.Spec.Image.Reference()
	}
	return res, err
}

// rejectUnchangedImageUpgrade refuses a sequential release bump of
// spec.image.tag that leaves spec.image on the build whose db_sync last
// completed (status.installedImage, compared by runsInstalledBuild). The phase
// Jobs would then run the installed build: with a digest pinned they run
// spec.image.Reference(), which still names that build, so they would migrate
// nothing and still promote status.installedRelease to a release no migration
// produced. The guard keeps that marker honest, the way Neutron's
// gateReleaseTransition does for spec.openStackRelease.
//
// It answers only a sequential bump of a parseable release whose image is
// unchanged. An empty status.installedImage (a Keystone whose first settled
// pass has not run yet) skips it, and every other transition is left to
// database.InitiateUpgrade, which reports VersionParseError,
// DowngradeNotSupported and UpgradePathInvalid.
//
// A refusal sets DatabaseReady=False with reason ImageReleaseMismatch, records a
// Warning event of the same reason, and returns the error. It sets no upgrade
// phase or target release and creates no Job. The other way into this
// state is the reverse edit order: a digest bumped before the tag migrated the
// schema through plain db_sync and credited the new image to the old release,
// so the message names both recoveries.
func (r *KeystoneReconciler) rejectUnchangedImageUpgrade(keystone *keystonev1alpha1.Keystone) error {
	if keystone.Status.InstalledImage == "" {
		return nil
	}
	from, fromErr := release.ParseRelease(keystone.Status.InstalledRelease)
	to, toErr := release.ParseRelease(keystone.Spec.Image.Tag)
	// A release that does not parse is InitiateUpgrade's to report.
	sequential := fromErr == nil && toErr == nil && release.IsSequentialUpgrade(from, to)
	if !sequential || !runsInstalledBuild(keystone) {
		return nil
	}

	return r.refuseImageReleaseMismatch(keystone, fmt.Errorf("upgrade from %s to %s leaves spec.image unchanged (%s), so the phase Jobs would run the installed build: "+
		"either bump spec.image.digest in lockstep with spec.image.tag, or, if that image already migrated the schema "+
		"because its digest was bumped before spec.image.tag was, patch status.installedRelease to %s to record the "+
		"migration that has already run",
		from.Raw, to.Raw, keystone.Spec.Image.Reference(), to.Raw))
}

// rejectAbortOnTargetBuild refuses an abort, spec.image.tag reverted to
// status.installedRelease during an active upgrade, that leaves a pinned
// spec.image.digest on another build than status.installedImage. The abort
// hands the next pass to the steady-state db-sync Job, which runs
// spec.image.Reference(): on the target build its plain db_sync would apply the
// target release's expand and contract migrations in one Job, outside the
// phases, and the schema would be credited to the installed release.
//
// It needs a pinned digest and a recorded image, so a tag-only Keystone, and
// one whose first settled pass has not run yet, abort as before. A recorded
// tag matches no digest, so on such a Keystone the message names two edits
// instead of the revert: remove the digest, which leaves the Keystone on the
// mutable tag, or patch status.installedImage to the digest of the installed
// build and set spec.image.digest to it, which keeps the pin. A refusal sets
// DatabaseReady=False with reason ImageReleaseMismatch, records a Warning event
// of the same reason, and returns the error. The upgrade phase, the target
// release and the phase Jobs stay, so an abort that reverts the digest as well
// runs the shared AbortUpgrade.
func (r *KeystoneReconciler) rejectAbortOnTargetBuild(keystone *keystonev1alpha1.Keystone) error {
	if keystone.Spec.Image.Tag != keystone.Status.InstalledRelease ||
		keystone.Spec.Image.Digest == "" ||
		keystone.Status.InstalledImage == "" ||
		runsInstalledBuild(keystone) {
		return nil
	}
	remedy := "revert spec.image.digest in the same edit, so the db_sync after the abort runs the installed build"
	if !strings.Contains(keystone.Status.InstalledImage, "@") {
		remedy = "the installed build is recorded by tag, which no digest matches: either remove spec.image.digest " +
			`("digest":null in a merge patch) in the same edit, which leaves the db_sync after the abort and the Deployment on the mutable tag ` +
			keystone.Spec.Image.Repository + ":" + keystone.Spec.Image.Tag + ", or, to keep the pin, patch status.installedImage (status subresource) to " +
			keystone.Spec.Image.Repository + "@<digest of the installed build> and set spec.image.digest to that digest"
	}
	return r.refuseImageReleaseMismatch(keystone, fmt.Errorf("abort of upgrade %s → %s reverts spec.image.tag but leaves spec.image on %s instead of the installed build %s: %s",
		keystone.Status.InstalledRelease, keystone.Status.TargetRelease, keystone.Spec.Image.Reference(), keystone.Status.InstalledImage, remedy))
}

// runsInstalledBuild reports whether spec.image names the build recorded in
// status.installedImage. A digest names a build whichever repository serves
// it, so when spec.image pins a digest and the recorded reference carries one,
// only the two digests are compared; otherwise the whole references are.
func runsInstalledBuild(keystone *keystonev1alpha1.Keystone) bool {
	installed := keystone.Status.InstalledImage
	if digest := keystone.Spec.Image.Digest; digest != "" {
		if i := strings.LastIndex(installed, "@"); i >= 0 {
			return installed[i+1:] == digest
		}
	}
	return installed == keystone.Spec.Image.Reference()
}

// refuseImageReleaseMismatch sets DatabaseReady=False with reason
// ImageReleaseMismatch and the message of err, records a Warning event of the
// same reason and message, and returns err.
func (r *KeystoneReconciler) refuseImageReleaseMismatch(keystone *keystonev1alpha1.Keystone, err error) error {
	conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
		Type:               "DatabaseReady",
		Status:             metav1.ConditionFalse,
		ObservedGeneration: keystone.Generation,
		Reason:             conditionReasonImageReleaseMismatch,
		Message:            err.Error(),
	})
	r.Recorder.Event(keystone, corev1.EventTypeWarning, conditionReasonImageReleaseMismatch, err.Error())
	return err
}

// isUpgrade reports whether spec.image.tag requires the shared
// expand-migrate-contract flow for this Keystone CR. It is a thin package-private
// wrapper over database.IsUpgrade (kept because the unit test calls it directly)
// that passes the two values database.IsUpgrade reads: the spec release and the
// installed release.
func isUpgrade(keystone *keystonev1alpha1.Keystone) bool {
	return database.IsUpgrade(keystone.Spec.Image.Tag, keystone.Status.InstalledRelease)
}

// upgradeFlowParams assembles the shared expand-migrate-contract upgrade-flow
// inputs for this Keystone CR. The service-specific parts — the owner CR, the
// "DatabaseReady" condition vocabulary, spec.image.tag as the requested release,
// the per-phase Job builder, and the terminal-metric callback — are bound here;
// the phase choreography itself lives in internal/common/database. The three
// status pointers (UpgradePhase, InstalledRelease, TargetRelease) are mutated in
// place by the flow and persisted by the caller after it returns.
func (r *KeystoneReconciler) upgradeFlowParams(ctx context.Context, children client.Client, keystone *keystonev1alpha1.Keystone, configMapName, domainsSecretName string) database.UpgradeFlowParams {
	return database.UpgradeFlowParams{
		Client:           children,
		Scheme:           r.Scheme,
		Recorder:         r.Recorder,
		Owner:            keystone,
		Conditions:       &keystone.Status.Conditions,
		Generation:       keystone.Generation,
		ConditionType:    "DatabaseReady",
		RequeueAfter:     RequeueUpgradeWait,
		Phase:            &keystone.Status.UpgradePhase,
		InstalledRelease: &keystone.Status.InstalledRelease,
		TargetRelease:    &keystone.Status.TargetRelease,
		SpecRelease:      keystone.Spec.Image.Tag,
		BuildPhaseJob: func(phase keystonev1alpha1.UpgradePhase) *batchv1.Job {
			switch phase {
			case keystonev1alpha1.UpgradePhaseExpanding:
				return buildExpandJob(keystone, configMapName, domainsSecretName, keystone.Spec.Image.Tag)
			case keystonev1alpha1.UpgradePhaseMigrating:
				return buildMigrateJob(keystone, configMapName, domainsSecretName, keystone.Spec.Image.Tag)
			case keystonev1alpha1.UpgradePhaseContracting:
				return buildContractJob(keystone, configMapName, domainsSecretName, keystone.Spec.Image.Tag)
			case keystonev1alpha1.UpgradePhaseRollingUpdate:
				// RollingUpdate builds no Job; the Deployment rollout drives it.
				return nil
			default:
				// The empty steady-state phase never reaches BuildPhaseJob.
				return nil
			}
		},
		RecordTerminal: func(jobSuffix string, observed *batchv1.Job) {
			r.recordDBJobTerminalState(ctx, keystone, jobSuffix, observed)
		},
	}
}

// finalizeDatabaseResources issues Delete for the MariaDB Database, User, and
// Grant CRs named after the Keystone CR, delegating to the shared
// database.FinalizeResources. It returns as soon as the Delete requests have
// been accepted (or tolerated as NotFound) so the Keystone CR finalizer can be
// released in the same reconcile pass; see database.FinalizeResources for the
// deadlock-avoidance rationale.
func (r *KeystoneReconciler) finalizeDatabaseResources(ctx context.Context, children client.Client, keystone *keystonev1alpha1.Keystone) error {
	return database.FinalizeResources(ctx, children, mariaDBResourceKey(keystone))
}
