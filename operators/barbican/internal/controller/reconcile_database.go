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
	barbicanv1alpha1 "github.com/c5c3/forge/operators/barbican/api/v1alpha1"
)

// conditionReasonImageReleaseMismatch flags the operator error where the
// tag-pinned spec.image names a different OpenStack release than
// spec.openStackRelease. Release tracking keys on spec.openStackRelease while
// the migration Job and the Deployment run spec.image; the reconcile refuses to
// advance until the two agree rather than promoting an installed-release marker
// for a release the image is not. The steady-state and release-gate reasons come
// from the shared vocabulary in internal/common/database (database.Reason*).
const conditionReasonImageReleaseMismatch = "ImageReleaseMismatch"

// barbicanDBSyncCommand is the schema migration the db-sync Job runs: one
// alembic upgrade to head, which applies every pending revision in a single
// pass.
//
// No --config-file is passed. barbican-manage builds its oslo.config from the
// "barbican" project name, so it resolves barbican.conf through oslo.config's
// fixed search path — and barbicanConfigDir, where the Job mounts the rendered
// config Secret, is on it. That is the same resolution the API pods rely on, so
// the Job and the pods read the identical file.
var barbicanDBSyncCommand = []string{"barbican-manage", "db", "upgrade"}

// reconcileDatabase provisions and migrates the Barbican database schema and
// tracks the installed OpenStack release. It runs the shared provisioning flow
// (MariaDB cluster gate plus Database/User/Grant in managed mode, no-op in
// brownfield), gates the requested release against the installed one, and hands
// the migration to the shared single-pass sync flow, which promotes
// status.installedRelease and sets DatabaseReady on the Job's outcome.
//
// Barbican runs no expand-migrate-contract phase machine: its migrations are a
// single barbican-manage db upgrade pass and the CR carries no upgradePhase
// field. The release rules the phased flow enforces on its way in — no
// downgrades, no multi-release jumps — are enforced by gateReleaseTransition
// instead, so an unsupported transition is rejected before any Job runs.
//
// That single pass has a consequence the phased flow does not have. This step
// runs BEFORE the Deployment step and requeues while the sync Job runs, so the
// schema reaches release N before a single pod of release N exists: from Job
// completion until the last old pod terminates, release N-1 code serves traffic
// against a release N schema. Operators with the expand/contract split keep that
// window compatible by construction; here it is a property of whichever alembic
// revisions the target release happens to carry. A release containing a
// non-additive revision (a dropped or renamed column) therefore requires the API
// to be drained before the upgrade — DatabaseReady goes True the moment the Job
// completes and reports nothing about the rollout that follows.
//
// configSecretName names the rendered config Secret the migration Job mounts.
func (r *BarbicanReconciler) reconcileDatabase(ctx context.Context, barbican *barbicanv1alpha1.Barbican, configSecretName string) (ctrl.Result, error) {
	// Managed/brownfield provisioning: MariaDB cluster gate, Database/User/Grant
	// ensure, Dynamic-credentials skip of the User/Grant. A non-zero result means
	// the flow set a not-ready condition and we must return it unchanged.
	res, err := database.ReconcileProvision(ctx, database.ProvisionFlowParams{
		Client:        r.Client,
		Scheme:        r.Scheme,
		Owner:         barbican,
		InstanceName:  barbican.Name,
		Namespace:     barbican.Namespace,
		Database:      &barbican.Spec.Database,
		Conditions:    &barbican.Status.Conditions,
		Generation:    barbican.Generation,
		ConditionType: "DatabaseReady",
		RequeueAfter:  RequeueDatabaseWait,
	})
	if err != nil || !res.IsZero() {
		return res, err
	}

	// No config yet means no credential-ready default secret store has produced a
	// barbican.conf to migrate against. The migration Job mounts that Secret as
	// its whole /etc/barbican, so an empty name would render a volume the API
	// server rejects outright and every pass would fail on the Job create. Waiting
	// here also short-circuits the tail of the pipeline, keeping the db-clean
	// CronJob — which mounts the same Secret — from being built against nothing.
	// On a running Barbican the name is never empty: the config step recovers the
	// last-good one off the live Deployment.
	if configSecretName == "" {
		conditions.SetCondition(&barbican.Status.Conditions, metav1.Condition{
			Type:               "DatabaseReady",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: barbican.Generation,
			Reason:             conditionReasonWaitingForSecretStores,
			Message:            "Waiting for a credential-ready default secret store before provisioning the database schema",
		})
		return ctrl.Result{RequeueAfter: RequeueDatabaseWait}, nil
	}

	// Enforce the decoupled-field contract before release tracking advances (see
	// checkImageReleaseMismatch).
	if res, blocked := checkImageReleaseMismatch(barbican); blocked {
		return res, nil
	}

	if err := r.gateReleaseTransition(barbican); err != nil {
		return ctrl.Result{}, err
	}

	// Steady-state db-sync. barbican-manage db upgrade applies every pending
	// revision in one pass, so there is no schema-check step (SchemaCheckCommand
	// nil). InstalledRelease is promoted to spec.openStackRelease on Job success.
	res, err = database.ReconcileSyncJobs(ctx, database.SyncFlowParams{
		Client:   r.Client,
		Scheme:   r.Scheme,
		Recorder: r.Recorder,
		Owner:    barbican,
		Jobs:     barbicanJobSetParams(barbican, configSecretName),
		RecordTerminal: func(jobSuffix string, observed *batchv1.Job) {
			r.recordDBJobTerminalState(ctx, barbican, jobSuffix, observed)
		},
		Conditions:       &barbican.Status.Conditions,
		Generation:       barbican.Generation,
		ConditionType:    "DatabaseReady",
		RequeueAfter:     RequeueDatabaseWait,
		InstalledRelease: &barbican.Status.InstalledRelease,
		// The release the marker is promoted to on success. It is
		// spec.openStackRelease rather than spec.image.tag so a digest-pinned
		// image still tracks the release the operator was told to converge to.
		ImageTag: barbican.Spec.OpenStackRelease,
	})

	// status.targetRelease only describes a bump in flight. Once the sync flow
	// has promoted the installed release to the requested one, the CR is at its
	// target and the field is cleared; a failed or still-running Job leaves it
	// stamped so `kubectl get barbican` shows where the CR is heading.
	if barbican.Status.InstalledRelease == barbican.Spec.OpenStackRelease {
		barbican.Status.TargetRelease = ""
		// Record which image the installed schema was migrated by, so the release
		// gate can tell a real release bump (a new image) from a spec-only edit
		// that would migrate nothing. Only once the sync flow has settled: a
		// requeue or an error means the Job has not finished.
		if err == nil && res.IsZero() {
			barbican.Status.InstalledImage = barbican.Spec.Image.Reference()
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
// This is barbican's replacement for the guard rails the shared
// expand-migrate-contract flow applies in InitiateUpgrade. The reason vocabulary
// is the shared one (database.Reason*), so the conditions read the same as on
// the operators that do run the phase machine.
func (r *BarbicanReconciler) gateReleaseTransition(barbican *barbicanv1alpha1.Barbican) error {
	installed := barbican.Status.InstalledRelease
	requested := barbican.Spec.OpenStackRelease
	if installed == "" {
		// Fresh install: no transition to validate, and nothing to downgrade from.
		return nil
	}

	from, err := release.ParseRelease(installed)
	if err != nil {
		return r.rejectReleaseTransition(barbican, database.ReasonVersionParseError,
			fmt.Errorf("parsing installed release %q: %w", installed, err))
	}
	to, err := release.ParseRelease(requested)
	if err != nil {
		return r.rejectReleaseTransition(barbican, database.ReasonVersionParseError,
			fmt.Errorf("parsing requested release %q: %w", requested, err))
	}

	// Same release, or the same release rebuilt as a patch: the schema belongs to
	// the installed release either way, so the plain sync path applies.
	if release.IsPatchOnly(from, to) {
		return nil
	}
	if release.IsDowngrade(from, to) {
		return r.rejectReleaseTransition(barbican, database.ReasonDowngradeNotSupported,
			fmt.Errorf("downgrade from %s to %s is not supported", from.Raw, to.Raw))
	}
	if !release.IsSequentialUpgrade(from, to) {
		return r.rejectReleaseTransition(barbican, database.ReasonUpgradePathInvalid,
			fmt.Errorf("upgrade from %s to %s is not sequential; upgrade one release at a time", from.Raw, to.Raw))
	}

	// A release bump that leaves spec.image untouched migrates nothing: the
	// db-sync Job's pod template is identical (same image, same config Secret), so
	// the shared flow short-circuits on the already-completed Job and would
	// promote status.installedRelease off a run of the previous release's binary.
	// Refuse it, so the sequential-upgrade check above keeps chaining off markers
	// an actual migration produced rather than off a marker that only moved
	// because the field did. This is the digest-pinned counterpart of
	// checkImageReleaseMismatch, which can only compare a tag.
	//
	// The other way into this state is the reverse edit order: bumping
	// spec.image.digest first and spec.openStackRelease afterwards. A digest
	// carries no release, so the operator cannot tell that image apart from a
	// patch rebuild of the installed release — the two states are identical in
	// status — and the earlier pass therefore ran the migration and credited the
	// new image to the old release. The message names both readings, because only
	// the operator knows which one applies, and the recovery differs.
	if barbican.Status.InstalledImage != "" && barbican.Status.InstalledImage == barbican.Spec.Image.Reference() {
		return r.rejectReleaseTransition(barbican, conditionReasonImageReleaseMismatch,
			fmt.Errorf("upgrade from %s to %s leaves spec.image unchanged (%s), so no migration would run: "+
				"either bump spec.image in lockstep with spec.openStackRelease, or — if that image already migrated the "+
				"schema on an earlier pass, because its digest was bumped before spec.openStackRelease was — patch "+
				"status.installedRelease to %s to record the migration that has already run",
				from.Raw, to.Raw, barbican.Status.InstalledImage, to.Raw))
	}

	barbican.Status.TargetRelease = requested
	return nil
}

// rejectReleaseTransition reports a refused release transition on the
// DatabaseReady condition, records a Warning event, and returns err unchanged so
// the caller surfaces it as a reconcile error.
func (r *BarbicanReconciler) rejectReleaseTransition(barbican *barbicanv1alpha1.Barbican, reason string, err error) error {
	conditions.SetCondition(&barbican.Status.Conditions, metav1.Condition{
		Type:               "DatabaseReady",
		Status:             metav1.ConditionFalse,
		ObservedGeneration: barbican.Generation,
		Reason:             reason,
		Message:            err.Error(),
	})
	if r.Recorder != nil {
		r.Recorder.Event(barbican, corev1.EventTypeWarning, reason, err.Error())
	}
	return err
}

// checkImageReleaseMismatch enforces the decoupled-field contract between
// spec.openStackRelease and spec.image. spec.openStackRelease drives release
// tracking and the release gate, but the migration Job and the Deployment run
// spec.image — the two fields are deliberately separate (digest pinning) and
// nothing else enforces they agree. A tag-pinned image whose tag names a
// different OpenStack release would run the wrong barbican-manage binary against
// the schema: the sync Job exits 0 as a no-op while the flow promotes
// status.installedRelease to a release the pods neither run nor migrated to.
//
// When the two disagree it sets DatabaseReady=False/ImageReleaseMismatch and
// returns (requeue, true) so the caller refuses to advance until the image is
// bumped in lockstep. A digest-pinned image (Tag == "") or a tag that does not
// parse as a release carries no comparable release string and is left to the
// operator's explicit spec.openStackRelease declaration, matching the
// digest-disables-tracking contract; the patch suffix is ignored so a patched
// image build (e.g. tag 2026.1-p1) still matches release 2026.1.
func checkImageReleaseMismatch(barbican *barbicanv1alpha1.Barbican) (ctrl.Result, bool) {
	if barbican.Spec.Image.Tag == "" {
		return ctrl.Result{}, false
	}
	tagRel, tagErr := release.ParseRelease(barbican.Spec.Image.Tag)
	specRel, specErr := release.ParseRelease(barbican.Spec.OpenStackRelease)
	if tagErr != nil || specErr != nil {
		return ctrl.Result{}, false
	}
	if tagRel.Year == specRel.Year && tagRel.Minor == specRel.Minor {
		return ctrl.Result{}, false
	}
	conditions.SetCondition(&barbican.Status.Conditions, metav1.Condition{
		Type:               "DatabaseReady",
		Status:             metav1.ConditionFalse,
		ObservedGeneration: barbican.Generation,
		Reason:             conditionReasonImageReleaseMismatch,
		Message: fmt.Sprintf("spec.image.tag %q names OpenStack release %d.%d but spec.openStackRelease is %q; "+
			"bump spec.image in lockstep with spec.openStackRelease",
			barbican.Spec.Image.Tag, tagRel.Year, tagRel.Minor, barbican.Spec.OpenStackRelease),
	})
	return ctrl.Result{RequeueAfter: RequeueDatabaseWait}, true
}

// barbicanJobSetParams derives the shared migration-Job inputs from the Barbican
// CR: the config mount, the db-tls keypair, the [database] connection override,
// and the db-sync command. reconcileDatabase and the unit tests build the Job
// from this one source, so a seeded Job carries the same pod spec as the desired
// one.
//
// The config travels as a Secret rather than a ConfigMap: the rendered
// barbican.conf carries the vault plugin's approle_role_id, so the whole
// document is credential-bearing.
func barbicanJobSetParams(barbican *barbicanv1alpha1.Barbican, configSecretName string) database.JobSetParams {
	// Project the db-tls client keypair into the db-sync Job when database TLS is
	// enabled; the gate is centralised in barbicanDBTLSEnabled so the Job and the
	// deployment builders decide identically. The DSN in the derived
	// <name>-db-connection Secret carries ssl_ca/ssl_cert/ssl_key paths under
	// dbTLSMountPath, so without the mount barbican-manage cannot open them and
	// every migration fails.
	var extraVolumes []corev1.Volume
	var extraMounts []corev1.VolumeMount
	if barbicanDBTLSEnabled(barbican) {
		tlsVol, tlsMount := barbicanDBTLSVolumeAndMount(barbican)
		extraVolumes = append(extraVolumes, tlsVol)
		extraMounts = append(extraMounts, tlsMount)
	}
	return database.JobSetParams{
		InstanceName:     barbican.Name,
		Namespace:        barbican.Namespace,
		Image:            barbican.Spec.Image.Reference(),
		ConfigSecretName: configSecretName,
		// The Secret is mounted as a whole directory, at the same mount point the
		// API pods use, so oslo.config resolves the same barbican.conf in both.
		ConfigMountPath: barbicanConfigMountPath,
		// Override [database] connection via the oslo.config env var so the Job
		// reads the DB URL from the derived Secret instead of the placeholder in
		// the rendered config.
		Env:               []corev1.EnvVar{database.ConnectionEnvVar(barbican.Name)},
		ExtraVolumes:      extraVolumes,
		ExtraVolumeMounts: extraMounts,
		SyncCommand:       barbicanDBSyncCommand,
		// No schema-check: barbican-manage db upgrade is an idempotent alembic
		// upgrade to head, so a second read-only Job would assert nothing the sync
		// itself has not already established.
		SchemaCheckCommand: nil,
	}
}
