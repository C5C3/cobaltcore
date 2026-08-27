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
	"github.com/c5c3/cobaltcore/internal/common/deployment"
	"github.com/c5c3/cobaltcore/internal/common/messaging"
	"github.com/c5c3/cobaltcore/internal/common/release"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
)

// Condition reasons for DatabaseReady set on the Neutron-specific paths. The
// steady-state and expand-migrate-contract reasons come from the shared
// vocabulary in internal/common/database (database.Reason*).
const (
	// conditionReasonDatabaseWaitingForConfig is set while no rendered config
	// exists to migrate against. It mirrors the config step's own gate: the OVN
	// endpoints parameterise ml2_conf.ini, so an unresolved OVNCentral leaves the
	// database step with nothing to mount.
	conditionReasonDatabaseWaitingForConfig = "WaitingForConfig"
	// conditionReasonImageReleaseMismatch flags the operator error where the
	// tag-pinned spec.image names a different OpenStack release than
	// spec.openStackRelease. Release tracking and the upgrade detection key on
	// spec.openStackRelease while every migration Job and the Deployment run
	// spec.image; the reconcile refuses to advance until the two agree rather than
	// promoting an installed-release marker for a release the image is not.
	conditionReasonImageReleaseMismatch = "ImageReleaseMismatch"
)

// upgradePhaseJobBackoffLimit is the retry budget of the expand, migrate and
// contract Jobs, keystone parity.
const upgradePhaseJobBackoffLimit int32 = 4

// neutronCommand assembles the command line of a neutron process: the binary,
// the two --config-file flags in the order neutron reads them, and the
// process-specific arguments. Both files are named explicitly because the config
// ConfigMap is mounted as a whole directory and neutron picks up nothing by
// scanning it. The order is the one reconcileConfig documents: an ml2 option set
// in both files resolves to the ml2_conf.ini value.
func neutronCommand(binary string, args ...string) []string {
	cmd := []string{
		binary,
		"--config-file", neutronConfigMountPath + "/" + neutronConfDataKey,
		"--config-file", neutronConfigMountPath + "/" + ml2ConfDataKey,
	}
	return append(cmd, args...)
}

// neutronDBSyncCommand is the schema migration the db-sync Job runs: one alembic
// upgrade to head, which applies every pending revision of both the expand and
// the contract branch in a single pass.
var neutronDBSyncCommand = neutronCommand("neutron-db-manage", "upgrade", "head")

// neutronMaxUserConnections sizes the SQL user's max_user_connections cap for
// the CR's own topology. Every uWSGI worker holds at least one pooled connection
// once its app has loaded, so the API floor is pods × processes × threads, with
// pods being the autoscaling ceiling when an HPA owns the replica count. On top
// of it comes one surge pod, because the rollout strategy (maxSurge=1,
// maxUnavailable=0) runs a full extra pod's workers alongside the fleet during
// an update.
//
// The two worker Deployments come next. Each runs
// spec.workers.deployment.replicas pods plus one surge pod of its own, and each
// worker process keeps one pooled connection, which is the 2×(replicas+1) term.
// Last, two transient job connections (db-sync and the ovn-db-sync run that may
// overlap it).
//
// Left unsized, the mariadb-operator CRD default of 10 applies, which the
// default topology already exceeds before a single request is served: the API
// pods alone take (3+1)×2 = 8, the workers another 2×(3+1) = 8, and the last
// processes to start fail their pool with MySQL error 1226 while --need-app
// crash-loops their pod indefinitely.
func neutronMaxUserConnections(neutron *neutronv1alpha1.Neutron) int32 {
	pods := deployment.EffectiveReplicas(&neutron.Spec.Deployment)
	if neutron.Spec.Autoscaling != nil {
		pods = neutron.Spec.Autoscaling.MaxReplicas
	}
	var uwsgi *neutronv1alpha1.UWSGISpec
	if neutron.Spec.APIServer != nil {
		uwsgi = neutron.Spec.APIServer.UWSGI
	}
	processes, threads := deployment.EffectiveUWSGIConcurrency(uwsgi)
	workers := deployment.EffectiveReplicas(&neutron.Spec.Workers.Deployment)
	return (pods+1)*processes*threads + 2*(workers+1) + 2
}

// reconcileDatabase provisions and migrates the Neutron database schema and
// tracks the installed OpenStack release. It always runs the shared provisioning
// flow (MariaDB cluster gate plus Database/User/Grant in managed mode, no-op in
// brownfield). A release transition (a non-patch change from the installed
// release) runs the shared expand-migrate-contract upgrade flow, walking
// Expanding → Migrating → RollingUpdate → Contracting; fresh installs and patch
// bumps stay on the single-pass neutron-db-manage upgrade head path.
//
// configMapName names the rendered config ConfigMap the migration Jobs mount.
// An empty name means the OVN endpoints are still unresolved and nothing has
// been rendered yet, so the step waits rather than migrating against an empty
// mount.
//
// The image/release agreement is enforced ahead of the upgrade branch rather
// than beside it: aborting an upgrade by reverting spec.openStackRelease
// therefore requires reverting spec.image in lockstep, which is the same
// contract the message of the mismatch condition states.
func (r *NeutronReconciler) reconcileDatabase(ctx context.Context, children client.Client,
	neutron *neutronv1alpha1.Neutron, configMapName string,
) (ctrl.Result, error) {
	// Managed/brownfield provisioning: MariaDB cluster gate, Database/User/Grant
	// ensure, Dynamic-credentials skip of the User/Grant. A non-zero result means
	// the flow set a not-ready condition and we must return it unchanged.
	res, err := database.ReconcileProvision(ctx, database.ProvisionFlowParams{
		Client:             children,
		Scheme:             r.Scheme,
		Owner:              neutron,
		InstanceName:       neutron.Name,
		Namespace:          neutron.Namespace,
		Database:           &neutron.Spec.Database,
		Conditions:         &neutron.Status.Conditions,
		Generation:         neutron.Generation,
		ConditionType:      "DatabaseReady",
		RequeueAfter:       RequeueDatabaseWait,
		MaxUserConnections: neutronMaxUserConnections(neutron),
	})
	if err != nil || !res.IsZero() {
		return res, err
	}

	// No config yet means the OVN endpoint step has not resolved the two database
	// addresses, so no neutron.conf and no ml2_conf.ini exist. The migration Jobs
	// mount that ConfigMap as their whole /etc/neutron, so an empty name would
	// render a volume the API server rejects outright and every pass would fail on
	// the Job create.
	if configMapName == "" {
		conditions.SetCondition(&neutron.Status.Conditions, metav1.Condition{
			Type:               "DatabaseReady",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: neutron.Generation,
			Reason:             conditionReasonDatabaseWaitingForConfig,
			Message:            "Waiting for the resolved OVN endpoints and the rendered config before provisioning the database schema",
		})
		return ctrl.Result{RequeueAfter: RequeueDatabaseWait}, nil
	}

	// Enforce the decoupled-field contract before release tracking advances (see
	// checkImageReleaseMismatch).
	if res, blocked := checkImageReleaseMismatch(neutron); blocked {
		return res, nil
	}

	// Active upgrade: the shared flow handles the abort (spec.openStackRelease
	// reverted to the installed release), the target-changed guard, and the phase
	// dispatch.
	if neutron.Status.UpgradePhase != "" {
		return database.ReconcileUpgrade(ctx, r.upgradeFlowParams(ctx, children, neutron, configMapName))
	}

	if err := r.gateReleaseTransition(neutron); err != nil {
		return ctrl.Result{}, err
	}

	// Detect a release upgrade (patch-only and same-release changes stay on the
	// steady-state path).
	if database.IsUpgrade(neutron.Spec.OpenStackRelease, neutron.Status.InstalledRelease) {
		return database.InitiateUpgrade(ctx, r.upgradeFlowParams(ctx, children, neutron, configMapName))
	}

	// Steady-state db-sync. neutron-db-manage upgrade head applies every pending
	// revision in one pass, so there is no schema-check step (SchemaCheckCommand
	// nil). InstalledRelease is promoted to spec.openStackRelease on Job success.
	res, err = database.ReconcileSyncJobs(ctx, database.SyncFlowParams{
		Client:   children,
		Scheme:   r.Scheme,
		Recorder: r.Recorder,
		Owner:    neutron,
		Jobs:     neutronJobSetParams(neutron, configMapName),
		RecordTerminal: func(jobSuffix string, observed *batchv1.Job) {
			r.recordDBJobTerminalState(ctx, neutron, jobSuffix, observed)
		},
		Conditions:       &neutron.Status.Conditions,
		Generation:       neutron.Generation,
		ConditionType:    "DatabaseReady",
		RequeueAfter:     RequeueDatabaseWait,
		InstalledRelease: &neutron.Status.InstalledRelease,
		// The release the marker is promoted to on success. It is
		// spec.openStackRelease rather than spec.image.tag so a digest-pinned image
		// still tracks the release the operator was told to converge to.
		ImageTag: neutron.Spec.OpenStackRelease,
	})

	// status.targetRelease only describes a bump in flight. Once the sync flow has
	// promoted the installed release to the requested one, the CR is at its target
	// and the field is cleared; a failed or still-running Job leaves it stamped so
	// `kubectl get neutron` shows where the CR is heading.
	if neutron.Status.InstalledRelease == neutron.Spec.OpenStackRelease {
		neutron.Status.TargetRelease = ""
		// Record which image the installed schema was migrated by, so the release
		// gate can tell a real release bump (a new image) from a spec-only edit that
		// would migrate nothing. Only once the sync flow has settled: a requeue or an
		// error means the Job has not finished.
		if err == nil && res.IsZero() {
			neutron.Status.InstalledImage = neutron.Spec.Image.Reference()
		}
	}
	return res, err
}

// gateReleaseTransition validates a change of spec.openStackRelease against
// status.installedRelease before the upgrade flow is initiated. A fresh install
// (no installed release) and a patch bump pass straight through to the sync
// path. A downgrade, a jump of more than one release, and an unparseable release
// on either side are rejected: DatabaseReady=False with the shared reason, a
// Warning event, and a returned error so the controller backs off instead of
// migrating a schema the image cannot serve.
//
// The shared flow's InitiateUpgrade applies the same release rules on its way
// in. Running them here first keeps a rejected transition from ever stamping an
// upgrade phase, and adds the one rule the shared flow cannot know about: a
// release bump that leaves spec.image untouched.
func (r *NeutronReconciler) gateReleaseTransition(neutron *neutronv1alpha1.Neutron) error {
	installed := neutron.Status.InstalledRelease
	requested := neutron.Spec.OpenStackRelease
	if installed == "" {
		// Fresh install: no transition to validate, and nothing to downgrade from.
		return nil
	}

	from, err := release.ParseRelease(installed)
	if err != nil {
		return r.rejectReleaseTransition(neutron, database.ReasonVersionParseError,
			fmt.Errorf("parsing installed release %q: %w", installed, err))
	}
	to, err := release.ParseRelease(requested)
	if err != nil {
		return r.rejectReleaseTransition(neutron, database.ReasonVersionParseError,
			fmt.Errorf("parsing requested release %q: %w", requested, err))
	}

	// Same release, or the same release rebuilt as a patch: the schema belongs to
	// the installed release either way, so the plain sync path applies.
	if release.IsPatchOnly(from, to) {
		return nil
	}
	if release.IsDowngrade(from, to) {
		return r.rejectReleaseTransition(neutron, database.ReasonDowngradeNotSupported,
			fmt.Errorf("downgrade from %s to %s is not supported", from.Raw, to.Raw))
	}
	if !release.IsSequentialUpgrade(from, to) {
		return r.rejectReleaseTransition(neutron, database.ReasonUpgradePathInvalid,
			fmt.Errorf("upgrade from %s to %s is not sequential; upgrade one release at a time", from.Raw, to.Raw))
	}

	// A release bump that leaves spec.image untouched migrates nothing: the phase
	// Jobs' pod templates are identical (same image, same config ConfigMap), so the
	// shared flow short-circuits on the already-completed Jobs and would promote
	// status.installedRelease off a run of the previous release's binary. Refuse
	// it, so the sequential-upgrade check above keeps chaining off markers an
	// actual migration produced rather than off a marker that only moved because
	// the field did. This is the digest-pinned counterpart of
	// checkImageReleaseMismatch, which can only compare a tag.
	//
	// The other way into this state is the reverse edit order: bumping
	// spec.image.digest first and spec.openStackRelease afterwards. A digest
	// carries no release, so the operator cannot tell that image apart from a patch
	// rebuild of the installed release — the two states are identical in status —
	// and the earlier pass therefore ran the migration and credited the new image
	// to the old release. The message names both readings, because only the
	// operator knows which one applies, and the recovery differs.
	if neutron.Status.InstalledImage != "" && neutron.Status.InstalledImage == neutron.Spec.Image.Reference() {
		return r.rejectReleaseTransition(neutron, conditionReasonImageReleaseMismatch,
			fmt.Errorf("upgrade from %s to %s leaves spec.image unchanged (%s), so no migration would run: "+
				"either bump spec.image in lockstep with spec.openStackRelease, or — if that image already migrated the "+
				"schema on an earlier pass, because its digest was bumped before spec.openStackRelease was — patch "+
				"status.installedRelease to %s to record the migration that has already run",
				from.Raw, to.Raw, neutron.Status.InstalledImage, to.Raw))
	}

	return nil
}

// rejectReleaseTransition reports a refused release transition on the
// DatabaseReady condition, records a Warning event, and returns err unchanged so
// the caller surfaces it as a reconcile error.
func (r *NeutronReconciler) rejectReleaseTransition(neutron *neutronv1alpha1.Neutron, reason string, err error) error {
	conditions.SetCondition(&neutron.Status.Conditions, metav1.Condition{
		Type:               "DatabaseReady",
		Status:             metav1.ConditionFalse,
		ObservedGeneration: neutron.Generation,
		Reason:             reason,
		Message:            err.Error(),
	})
	if r.Recorder != nil {
		r.Recorder.Event(neutron, corev1.EventTypeWarning, reason, err.Error())
	}
	return err
}

// checkImageReleaseMismatch enforces the decoupled-field contract between
// spec.openStackRelease and spec.image. spec.openStackRelease drives release
// tracking and the upgrade detection, but every migration Job and the Deployment
// run spec.image — the two fields are deliberately separate (digest pinning) and
// nothing else enforces they agree. A tag-pinned image whose tag names a
// different OpenStack release would run the wrong neutron-db-manage binary
// against a schema already at its own head: the phase Jobs exit 0 as no-ops
// while the flow promotes status.installedRelease to a release the pods neither
// run nor migrated to.
//
// When the two disagree it sets DatabaseReady=False/ImageReleaseMismatch and
// returns (requeue, true) so the caller refuses to advance until the image is
// bumped in lockstep. A digest-pinned image (Tag == "") or a tag that does not
// parse as a release carries no comparable release string and is left to the
// operator's explicit spec.openStackRelease declaration, matching the
// digest-disables-tracking contract; the patch suffix is ignored so a patched
// image build (e.g. tag 2026.1-p1) still matches release 2026.1.
func checkImageReleaseMismatch(neutron *neutronv1alpha1.Neutron) (ctrl.Result, bool) {
	if neutron.Spec.Image.Tag == "" {
		return ctrl.Result{}, false
	}
	tagRel, tagErr := release.ParseRelease(neutron.Spec.Image.Tag)
	specRel, specErr := release.ParseRelease(neutron.Spec.OpenStackRelease)
	if tagErr != nil || specErr != nil {
		return ctrl.Result{}, false
	}
	if tagRel.Year == specRel.Year && tagRel.Minor == specRel.Minor {
		return ctrl.Result{}, false
	}
	conditions.SetCondition(&neutron.Status.Conditions, metav1.Condition{
		Type:               "DatabaseReady",
		Status:             metav1.ConditionFalse,
		ObservedGeneration: neutron.Generation,
		Reason:             conditionReasonImageReleaseMismatch,
		Message: fmt.Sprintf("spec.image.tag %q names OpenStack release %d.%d but spec.openStackRelease is %q; "+
			"bump spec.image in lockstep with spec.openStackRelease",
			neutron.Spec.Image.Tag, tagRel.Year, tagRel.Minor, neutron.Spec.OpenStackRelease),
	})
	return ctrl.Result{RequeueAfter: RequeueDatabaseWait}, true
}

// neutronJobSetParams derives the shared migration-Job inputs from the Neutron
// CR: the config mount, the two credential env overrides, the projected TLS
// material, and the db-sync command. The steady-state sync flow
// (database.ReconcileSyncJobs) and the upgrade-phase builders
// (upgradeFlowParams.BuildPhaseJob) both consume it; centralising it here lets
// tests build the identical Job.
func neutronJobSetParams(neutron *neutronv1alpha1.Neutron, configMapName string) database.JobSetParams {
	// The OVN client keypair travels with the config: the ml2_conf.ini the Job
	// mounts names its three files in the [ovn] section, so a Job without the
	// mount carries a config pointing at paths that do not exist in its pod.
	ovnVol, ovnMount := ovnTLSVolumeAndMount(neutron)
	extraVolumes := []corev1.Volume{ovnVol}
	extraMounts := []corev1.VolumeMount{ovnMount}

	// Project the db-tls client keypair when database TLS is enabled; the gate is
	// centralised in neutronDBTLSEnabled so the migration Jobs and the workload
	// builders decide identically. The DSN in the derived <name>-db-connection
	// Secret carries ssl_ca/ssl_cert/ssl_key paths under dbTLSMountPath, so without
	// the mount neutron-db-manage cannot open them and every migration fails.
	if neutronDBTLSEnabled(neutron) {
		tlsVol, tlsMount := neutronDBTLSVolumeAndMount(neutron)
		extraVolumes = append(extraVolumes, tlsVol)
		extraMounts = append(extraMounts, tlsMount)
	}

	return database.JobSetParams{
		InstanceName:  neutron.Name,
		Namespace:     neutron.Namespace,
		Image:         neutron.Spec.Image.Reference(),
		ConfigMapName: configMapName,
		// The ConfigMap is mounted as a whole directory, at the same mount point the
		// API pods use, so the Job and the pods read the identical files.
		ConfigMountPath: neutronConfigMountPath,
		// Override [database] connection and [DEFAULT] transport_url via the
		// oslo.config env vars so the Job reads both from their derived Secrets
		// instead of the placeholder in the rendered config. neutron-db-manage loads
		// the whole file, and an alembic revision that queues a notification would
		// otherwise publish to nothing.
		Env: []corev1.EnvVar{
			database.ConnectionEnvVar(neutron.Name),
			messaging.TransportURLEnvVar(neutron.Name),
		},
		ExtraVolumes:      extraVolumes,
		ExtraVolumeMounts: extraMounts,
		SyncCommand:       neutronDBSyncCommand,
		// No schema-check: neutron-db-manage upgrade head is an idempotent alembic
		// upgrade to head, so a second read-only Job would assert nothing the sync
		// itself has not already established. Release upgrades instead run the shared
		// expand-migrate-contract phase machine (see upgradeFlowParams).
		SchemaCheckCommand: nil,
	}
}

// upgradeFlowParams assembles the shared expand-migrate-contract upgrade-flow
// inputs for this Neutron CR. The service-specific parts — the owner CR, the
// "DatabaseReady" condition vocabulary, spec.openStackRelease as the requested
// release, the per-phase Job builder, and the terminal-metric callback — are
// bound here; the phase choreography itself lives in internal/common/database.
// The three status pointers (UpgradePhase, InstalledRelease, TargetRelease) are
// mutated in place by the flow and persisted by the caller after it returns.
func (r *NeutronReconciler) upgradeFlowParams(ctx context.Context, children client.Client,
	neutron *neutronv1alpha1.Neutron, configMapName string,
) database.UpgradeFlowParams {
	image := neutron.Spec.Image.Reference()
	return database.UpgradeFlowParams{
		Client:           children,
		Scheme:           r.Scheme,
		Recorder:         r.Recorder,
		Owner:            neutron,
		Conditions:       &neutron.Status.Conditions,
		Generation:       neutron.Generation,
		ConditionType:    "DatabaseReady",
		RequeueAfter:     RequeueUpgradeWait,
		Phase:            &neutron.Status.UpgradePhase,
		InstalledRelease: &neutron.Status.InstalledRelease,
		TargetRelease:    &neutron.Status.TargetRelease,
		SpecRelease:      neutron.Spec.OpenStackRelease,
		// Each phase Job runs spec.image: the operator's contract is that the image
		// is bumped together with spec.openStackRelease, so the new release's
		// migration tree owns the expand and contract schema deltas.
		BuildPhaseJob: func(phase commonv1.UpgradePhase) *batchv1.Job {
			params := neutronJobSetParams(neutron, configMapName)
			switch phase {
			case commonv1.UpgradePhaseExpanding:
				return database.BuildJob(params, image, "db-expand",
					neutronCommand("neutron-db-manage", "upgrade", "--expand"), upgradePhaseJobBackoffLimit)
			case commonv1.UpgradePhaseMigrating:
				// Neutron has no data-migration command: its alembic tree splits into an
				// expand and a contract branch alone. The shared flow runs a Job for every
				// phase, so this one runs neutron-db-manage current, which prints the
				// revision each branch sits at and exits 0. It is what makes the phase
				// observable rather than a no-op the flow cannot report on.
				return database.BuildJob(params, image, "db-migrate",
					neutronCommand("neutron-db-manage", "current"), upgradePhaseJobBackoffLimit)
			case commonv1.UpgradePhaseContracting:
				return database.BuildJob(params, image, "db-contract",
					neutronCommand("neutron-db-manage", "upgrade", "--contract"), upgradePhaseJobBackoffLimit)
			case commonv1.UpgradePhaseRollingUpdate:
				// RollingUpdate builds no Job; the Deployment rollout drives it.
				return nil
			default:
				// The empty steady-state phase never reaches BuildPhaseJob.
				return nil
			}
		},
		RecordTerminal: func(jobSuffix string, observed *batchv1.Job) {
			r.recordDBJobTerminalState(ctx, neutron, jobSuffix, observed)
		},
	}
}
