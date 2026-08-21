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
	placementv1alpha1 "github.com/c5c3/cobaltcore/operators/placement/api/v1alpha1"
)

// conditionReasonImageReleaseMismatch flags the operator error where the
// tag-pinned spec.image names a different OpenStack release than
// spec.openStackRelease. Release tracking keys on spec.openStackRelease while
// the migration Job and the Deployment run spec.image; the reconcile refuses to
// advance until the two agree rather than promoting an installed-release marker
// for a release the image is not. The steady-state and release-gate reasons come
// from the shared vocabulary in internal/common/database (database.Reason*).
const conditionReasonImageReleaseMismatch = "ImageReleaseMismatch"

// placementConfFilePath is the rendered placement.conf the migration Job reads.
// It is the "placement.conf" key of the config ConfigMap (see reconcileConfig)
// resolved inside placementConfigDir, the directory the Job mounts that
// ConfigMap at. placement-manage and placement-status take the file explicitly
// (--config-file) because the image ships no /etc/placement/placement.conf of
// its own and neither tool reads OS_PLACEMENT_CONFIG_DIR.
const placementConfFilePath = placementConfigDir + "placement.conf"

// placementDBSyncScript is the schema-migration script the db-sync Job runs
// under /bin/sh -eu -c. The three steps are one Job: db sync applies every
// pending schema migration, db online_data_migrations moves the rows the new
// schema expects, and placement-status upgrade check validates the result.
//
// The brace group around the check scopes the exit-1 tolerance to that command
// alone. placement-status exits 1 for warnings (an acceptable outcome the Job
// must not fail on) and 2 for errors, so `|| [ $? -eq 1 ]` swallows exactly the
// warning status while an exit 2 still fails the group and, under sh -e, the
// Job. The two placement-manage commands run outside the group, so their
// failures remain fatal.
const placementDBSyncScript = "placement-manage --config-file " + placementConfFilePath + " db sync && " +
	"placement-manage --config-file " + placementConfFilePath + " db online_data_migrations && " +
	"{ placement-status --config-file " + placementConfFilePath + " upgrade check || [ $? -eq 1 ]; }"

// reconcileDatabase provisions and migrates the Placement database schema and
// tracks the installed OpenStack release. It runs the shared provisioning flow
// (MariaDB cluster gate plus Database/User/Grant in managed mode, no-op in
// brownfield), gates the requested release against the installed one, and hands
// the migration to the shared single-pass sync flow, which promotes
// status.installedRelease and sets DatabaseReady on the Job's outcome.
//
// Placement runs no expand-migrate-contract phase machine: its migrations are a
// single placement-manage db sync pass and the CR carries no upgradePhase field.
// The release rules the phased flow enforces on its way in — no downgrades, no
// multi-release jumps — are enforced by gateReleaseTransition instead, so an
// unsupported transition is rejected before any Job runs.
func (r *PlacementReconciler) reconcileDatabase(ctx context.Context, children client.Client, placement *placementv1alpha1.Placement, configMapName string) (ctrl.Result, error) {
	// Managed/brownfield provisioning: MariaDB cluster gate, Database/User/Grant
	// ensure, Dynamic-credentials skip of the User/Grant. A non-zero result means
	// the flow set a not-ready condition and we must return it unchanged.
	res, err := database.ReconcileProvision(ctx, database.ProvisionFlowParams{
		Client:        children,
		Scheme:        r.Scheme,
		Owner:         placement,
		InstanceName:  placement.Name,
		Namespace:     placement.Namespace,
		Database:      &placement.Spec.Database,
		Conditions:    &placement.Status.Conditions,
		Generation:    placement.Generation,
		ConditionType: "DatabaseReady",
		RequeueAfter:  RequeueDatabaseWait,
	})
	if err != nil || !res.IsZero() {
		return res, err
	}

	// Enforce the decoupled-field contract before release tracking advances (see
	// checkImageReleaseMismatch).
	if res, blocked := checkImageReleaseMismatch(placement); blocked {
		return res, nil
	}

	if err := r.gateReleaseTransition(placement); err != nil {
		return ctrl.Result{}, err
	}

	// Steady-state db-sync. placement-manage db sync applies every pending
	// migration in one pass, so there is no schema-check step
	// (SchemaCheckCommand nil). InstalledRelease is promoted to
	// spec.openStackRelease on Job success.
	res, err = database.ReconcileSyncJobs(ctx, database.SyncFlowParams{
		Client:   children,
		Scheme:   r.Scheme,
		Recorder: r.Recorder,
		Owner:    placement,
		Jobs:     placementJobSetParams(placement, configMapName),
		RecordTerminal: func(jobSuffix string, observed *batchv1.Job) {
			r.recordDBJobTerminalState(ctx, placement, jobSuffix, observed)
		},
		Conditions:       &placement.Status.Conditions,
		Generation:       placement.Generation,
		ConditionType:    "DatabaseReady",
		RequeueAfter:     RequeueDatabaseWait,
		InstalledRelease: &placement.Status.InstalledRelease,
		// The release the marker is promoted to on success. It is
		// spec.openStackRelease rather than spec.image.tag so a digest-pinned
		// image still tracks the release the operator was told to converge to.
		ImageTag: placement.Spec.OpenStackRelease,
	})

	// status.targetRelease only describes a bump in flight. Once the sync flow
	// has promoted the installed release to the requested one, the CR is at its
	// target and the field is cleared; a failed or still-running Job leaves it
	// stamped so `kubectl get placement` shows where the CR is heading.
	if placement.Status.InstalledRelease == placement.Spec.OpenStackRelease {
		placement.Status.TargetRelease = ""
		// Record which image the installed schema was migrated by, so the release
		// gate can tell a real release bump (a new image) from a spec-only edit
		// that would migrate nothing. Only once the sync flow has settled: a
		// requeue or an error means the Job has not finished.
		if err == nil && res.IsZero() {
			placement.Status.InstalledImage = placement.Spec.Image.Reference()
		}
	}
	return res, err
}

// gateReleaseTransition validates a change of spec.openStackRelease against
// status.installedRelease before any migration Job runs. A fresh install (no
// installed release) and a patch bump pass straight through to the sync path. A
// downgrade, a jump of more than one release, and an unparseable release on
// either side are rejected: DatabaseReady=False with the shared reason, a
// Warning event, and a returned error so the controller backs off instead of
// migrating a schema the image cannot serve. An accepted bump stamps
// status.targetRelease, which the caller clears once the sync flow has promoted
// status.installedRelease.
//
// This is placement's replacement for the guard rails the shared
// expand-migrate-contract flow applies in InitiateUpgrade. The reason vocabulary
// is the shared one (database.Reason*), so the conditions read the same as on
// the operators that do run the phase machine.
func (r *PlacementReconciler) gateReleaseTransition(placement *placementv1alpha1.Placement) error {
	installed := placement.Status.InstalledRelease
	requested := placement.Spec.OpenStackRelease
	if installed == "" {
		// Fresh install: no transition to validate, and nothing to downgrade from.
		return nil
	}

	from, err := release.ParseRelease(installed)
	if err != nil {
		return r.rejectReleaseTransition(placement, database.ReasonVersionParseError,
			fmt.Errorf("parsing installed release %q: %w", installed, err))
	}
	to, err := release.ParseRelease(requested)
	if err != nil {
		return r.rejectReleaseTransition(placement, database.ReasonVersionParseError,
			fmt.Errorf("parsing requested release %q: %w", requested, err))
	}

	// Same release, or the same release rebuilt as a patch: the schema belongs to
	// the installed release either way, so the plain sync path applies.
	if release.IsPatchOnly(from, to) {
		return nil
	}
	if release.IsDowngrade(from, to) {
		return r.rejectReleaseTransition(placement, database.ReasonDowngradeNotSupported,
			fmt.Errorf("downgrade from %s to %s is not supported", from.Raw, to.Raw))
	}
	if !release.IsSequentialUpgrade(from, to) {
		return r.rejectReleaseTransition(placement, database.ReasonUpgradePathInvalid,
			fmt.Errorf("upgrade from %s to %s is not sequential; upgrade one release at a time", from.Raw, to.Raw))
	}

	// A release bump that leaves spec.image untouched migrates nothing: the
	// db-sync Job's pod template is identical (same image, release-independent
	// placement.conf), so the shared flow short-circuits on the already-completed
	// Job and would promote status.installedRelease off a run of the previous
	// release's binary. Refuse it, so the sequential-upgrade check above keeps
	// chaining off markers an actual migration produced rather than off a marker
	// that only moved because the field did. This is the digest-pinned counterpart
	// of checkImageReleaseMismatch, which can only compare a tag.
	if placement.Status.InstalledImage != "" && placement.Status.InstalledImage == placement.Spec.Image.Reference() {
		return r.rejectReleaseTransition(placement, conditionReasonImageReleaseMismatch,
			fmt.Errorf("upgrade from %s to %s leaves spec.image unchanged (%s), so no migration would run; "+
				"bump spec.image in lockstep with spec.openStackRelease", from.Raw, to.Raw, placement.Status.InstalledImage))
	}

	placement.Status.TargetRelease = requested
	return nil
}

// rejectReleaseTransition reports a refused release transition on the
// DatabaseReady condition, records a Warning event, and returns err unchanged so
// the caller surfaces it as a reconcile error.
func (r *PlacementReconciler) rejectReleaseTransition(placement *placementv1alpha1.Placement, reason string, err error) error {
	conditions.SetCondition(&placement.Status.Conditions, metav1.Condition{
		Type:               "DatabaseReady",
		Status:             metav1.ConditionFalse,
		ObservedGeneration: placement.Generation,
		Reason:             reason,
		Message:            err.Error(),
	})
	if r.Recorder != nil {
		r.Recorder.Event(placement, corev1.EventTypeWarning, reason, err.Error())
	}
	return err
}

// checkImageReleaseMismatch enforces the decoupled-field contract between
// spec.openStackRelease and spec.image. spec.openStackRelease drives release
// tracking and the release gate, but the migration Job and the Deployment run
// spec.image — the two fields are deliberately separate (digest pinning) and
// nothing else enforces they agree. A tag-pinned image whose tag names a
// different OpenStack release would run the wrong placement-manage binary
// against the schema: the sync Job exits 0 as a no-op while the flow promotes
// status.installedRelease to a release the pods neither run nor migrated to.
//
// When the two disagree it sets DatabaseReady=False/ImageReleaseMismatch and
// returns (requeue, true) so the caller refuses to advance until the image is
// bumped in lockstep. A digest-pinned image (Tag == "") or a tag that does not
// parse as a release carries no comparable release string and is left to the
// operator's explicit spec.openStackRelease declaration, matching the
// digest-disables-tracking contract; the patch suffix is ignored so a patched
// image build (e.g. tag 2026.1-p1) still matches release 2026.1.
func checkImageReleaseMismatch(placement *placementv1alpha1.Placement) (ctrl.Result, bool) {
	if placement.Spec.Image.Tag == "" {
		return ctrl.Result{}, false
	}
	tagRel, tagErr := release.ParseRelease(placement.Spec.Image.Tag)
	specRel, specErr := release.ParseRelease(placement.Spec.OpenStackRelease)
	if tagErr != nil || specErr != nil {
		return ctrl.Result{}, false
	}
	if tagRel.Year == specRel.Year && tagRel.Minor == specRel.Minor {
		return ctrl.Result{}, false
	}
	conditions.SetCondition(&placement.Status.Conditions, metav1.Condition{
		Type:               "DatabaseReady",
		Status:             metav1.ConditionFalse,
		ObservedGeneration: placement.Generation,
		Reason:             conditionReasonImageReleaseMismatch,
		Message: fmt.Sprintf("spec.image.tag %q names OpenStack release %d.%d but spec.openStackRelease is %q; "+
			"bump spec.image in lockstep with spec.openStackRelease",
			placement.Spec.Image.Tag, tagRel.Year, tagRel.Minor, placement.Spec.OpenStackRelease),
	})
	return ctrl.Result{RequeueAfter: RequeueDatabaseWait}, true
}

// placementJobSetParams derives the shared migration-Job inputs from the
// Placement CR: the config mount, the db-tls keypair, the [placement_database]
// connection override, and the db-sync command. reconcileDatabase and the unit
// tests build the Job from this one source, so a seeded Job carries the same pod
// spec as the desired one.
func placementJobSetParams(placement *placementv1alpha1.Placement, configMapName string) database.JobSetParams {
	// Project the db-tls client keypair into the db-sync Job when database TLS is
	// enabled; the gate is centralised in placementDBTLSEnabled so the Job and the
	// deployment builders decide identically. The DSN in the derived
	// <name>-db-connection Secret carries ssl_ca/ssl_cert/ssl_key paths under
	// dbTLSMountPath, so without the mount placement-manage cannot open them and
	// every migration fails.
	var extraVolumes []corev1.Volume
	var extraMounts []corev1.VolumeMount
	if placementDBTLSEnabled(placement) {
		tlsVol, tlsMount := placementDBTLSVolumeAndMount(placement)
		extraVolumes = append(extraVolumes, tlsVol)
		extraMounts = append(extraMounts, tlsMount)
	}
	return database.JobSetParams{
		InstanceName:  placement.Name,
		Namespace:     placement.Namespace,
		Image:         placement.Spec.Image.Reference(),
		ConfigMapName: configMapName,
		// The ConfigMap is mounted as a whole directory, at the same mount point
		// the API pods use, so the mount and the --config-file paths in
		// placementDBSyncScript are derived from the same constant and cannot
		// drift apart.
		ConfigMountPath: placementConfigMountPath,
		// Override [placement_database] connection via the oslo.config env var so
		// the Job reads the DB URL from the derived Secret instead of the
		// placeholder in the rendered config.
		Env:               []corev1.EnvVar{database.ConnectionEnvVarForSection(placement.Name, "placement_database")},
		ExtraVolumes:      extraVolumes,
		ExtraVolumeMounts: extraMounts,
		SyncCommand:       []string{"/bin/sh", "-eu", "-c", placementDBSyncScript},
		// No schema-check: placement-manage db sync is idempotent and applies all
		// pending migrations in one pass, and the upgrade check inside the sync
		// script already validates the migrated schema.
		SchemaCheckCommand: nil,
	}
}
