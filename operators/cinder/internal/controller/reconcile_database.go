// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	"github.com/c5c3/cobaltcore/internal/common/database"
	"github.com/c5c3/cobaltcore/internal/common/deployment"
	"github.com/c5c3/cobaltcore/internal/common/job"
	"github.com/c5c3/cobaltcore/internal/common/keystoneauth"
	"github.com/c5c3/cobaltcore/internal/common/messaging"
	"github.com/c5c3/cobaltcore/internal/common/release"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	cinderv1alpha1 "github.com/c5c3/cobaltcore/operators/cinder/api/v1alpha1"
)

// Condition reason constants for DatabaseReady set on the Cinder-specific paths.
// The steady-state and expand-migrate-contract upgrade reasons live in
// internal/common/database as database.Reason*.
const (
	// conditionReasonDatabaseWaitingForConfig is set while no rendered config
	// exists: the migration Jobs mount the config ConfigMap as their whole
	// config directory, so an empty name would render a volume the API server
	// rejects and every pass would fail on the Job create.
	conditionReasonDatabaseWaitingForConfig = "WaitingForConfig"
	// conditionReasonImageReleaseMismatch flags the operator error where the
	// tag-pinned spec.image names a different OpenStack release than
	// spec.openStackRelease. Release tracking and the expand-migrate-contract
	// upgrade detection key on spec.openStackRelease while every migration Job
	// and every workload run spec.image; the reconcile refuses to advance until
	// the two agree rather than promoting an installed-release marker for a
	// release the image is not.
	conditionReasonImageReleaseMismatch = "ImageReleaseMismatch"
)

// dbTLSVolumeName is the pod volume the db-tls client keypair is projected
// through into the migration Jobs and the workloads.
const dbTLSVolumeName = "db-tls"

// cinderAPIProcessConnections is how many pooled connections one API worker
// process holds at steady state: the request-serving session, plus the second
// one oslo.db's pool keeps open behind it while a request is in flight.
const cinderAPIProcessConnections int32 = 2

// upgradePhaseJobBackoffLimit is how often an expand/migrate/contract Job is
// retried before it is declared failed. It is keystone parity.
const upgradePhaseJobBackoffLimit int32 = 4

// Job name suffixes of the three upgrade phases. They are the flow's own
// vocabulary (internal/common/database), repeated here because the phase Job
// builder and the terminal-state reporter both key on them.
const (
	upgradeExpandJobSuffix   = "db-expand"
	upgradeMigrateJobSuffix  = "db-migrate"
	upgradeContractJobSuffix = "db-contract"
)

// cinderDBSyncCommand is the schema migration the db-sync Job and the expand
// phase run: cinder-manage db sync applies every pending alembic revision in one
// pass.
var cinderDBSyncCommand = []string{"cinder-manage", "--config-dir", cinderConfigDir, "db", "sync"}

// terminationLogPath is the file the kubelet reads a terminated container's
// message off. The migrate phase writes the cinder-status exit code there, which
// is how reportUpgradeCheck gets it without streaming the pod's logs.
const terminationLogPath = "/dev/termination-log"

// The migrate phase's command, split at its destination so the script test can
// run these exact bytes against a path it is allowed to write. Nothing but the
// tests uses the two halves on their own.
const (
	upgradeCheckScriptHead = "rc=0; cinder-status --config-dir " + cinderConfigDir + " upgrade check || rc=$?; " +
		"echo \"cinder-status upgrade check exit $rc\" > "
	upgradeCheckScriptTail = "; case \"$rc\" in 0|1|2) exit 0;; *) exit \"$rc\";; esac"

	// upgradeCheckScript runs cinder-status upgrade check, which reports
	// readiness for the target release. Its exit code is a severity rather than a
	// success flag: 0 is clean, 1 carries warnings, and 2 means a check failed —
	// for a Cinder that is typically a volume whose service_uuid is still NULL,
	// which the service backfills on its own. None of the three may wedge the
	// upgrade, and the Job package counts a Job as done only on JobComplete, so
	// the script normalises 0/1/2 to a zero exit and lets anything else fail the
	// Job. The real exit code travels in the termination log, which
	// reportUpgradeCheck turns into an event on the Cinder.
	upgradeCheckScript = upgradeCheckScriptHead + terminationLogPath + upgradeCheckScriptTail
)

// cinderMaxUserConnections sizes the SQL user's max_user_connections cap for the
// CR's own topology. The API floor is pods × processes × threads ×
// cinderAPIProcessConnections, with pods being the autoscaling ceiling when an
// HPA owns the replica count. The +1 on pods is the rolling-update surge: the
// strategy runs a full extra pod's workers alongside the fleet during an update.
//
// The single-writer services come next. Each scheduler pod, each cinder-volume
// Deployment (one per attached backend, hence volumeDeployments) and the one
// backup service — the +1 inside the parenthesis — keeps one pooled connection
// and one more behind it, which is the 2×(…) term. Last, the trailing +2 is the
// migration Jobs' headroom: a db-sync or a phase Job may overlap the fleet.
//
// A cap below the fleet's steady state does not degrade gracefully: the last
// processes to start fail their pool with MySQL error 1226 and crash-loop their
// pod. Left unsized, the mariadb-operator CRD default of 10 applies, which the
// default topology exceeds before a single request is served.
func cinderMaxUserConnections(cinder *cinderv1alpha1.Cinder, volumeDeployments int32) int32 {
	pods := deployment.EffectiveReplicas(&cinder.Spec.API.Deployment)
	if cinder.Spec.Autoscaling != nil {
		pods = cinder.Spec.Autoscaling.MaxReplicas
	}
	processes, threads := deployment.EffectiveUWSGIConcurrency(cinder.Spec.API.UWSGI)
	// The schedulers are peers and may be raised; the defaulting webhook resolves
	// an absent block to one rather than to the shared default of three, so the
	// same fallback applies here for a CR that bypassed it.
	schedulerReplicas := cinder.Spec.Scheduler.Deployment.Replicas
	if schedulerReplicas == 0 {
		schedulerReplicas = 1
	}
	return (pods+1)*processes*threads*cinderAPIProcessConnections +
		2*(schedulerReplicas+volumeDeployments+1) + 2
}

// reconcileDatabase provisions and migrates the Cinder database schema and
// tracks the installed OpenStack release. It always runs the shared provisioning
// flow (MariaDB cluster gate plus Database/User/Grant in managed mode, no-op in
// brownfield). A release transition (a non-patch change from the installed
// release) runs the shared expand-migrate-contract upgrade flow, walking
// Expanding → Migrating → RollingUpdate → Contracting; fresh installs and patch
// bumps stay on the single-pass cinder-manage db sync path.
//
// configMapName names the rendered config ConfigMap the migration Jobs mount,
// and volumeDeployments how many cinder-volume Deployments the attached backends
// project into, which is what sizes the database user's connection cap.
func (r *CinderReconciler) reconcileDatabase(ctx context.Context, children client.Client,
	cinder *cinderv1alpha1.Cinder, configMapName string, volumeDeployments int32,
) (ctrl.Result, error) {
	// Managed/brownfield provisioning: MariaDB cluster gate, Database/User/Grant
	// ensure, Dynamic-credentials skip of the User/Grant. A non-zero result means
	// the flow set a not-ready condition and we must return it unchanged.
	res, err := database.ReconcileProvision(ctx, database.ProvisionFlowParams{
		Client:             children,
		Scheme:             r.Scheme,
		Owner:              cinder,
		InstanceName:       cinder.Name,
		Namespace:          cinder.Namespace,
		Database:           &cinder.Spec.Database,
		Conditions:         &cinder.Status.Conditions,
		Generation:         cinder.Generation,
		ConditionType:      "DatabaseReady",
		RequeueAfter:       RequeueDatabaseWait,
		MaxUserConnections: cinderMaxUserConnections(cinder, volumeDeployments),
	})
	if err != nil || !res.IsZero() {
		return res, err
	}

	// No config yet means nothing has been rendered, and the migration Jobs mount
	// that ConfigMap as their whole config directory. Wait rather than migrate
	// against an empty mount. Mid-upgrade an empty name is unreachable anyway:
	// the config step recovers the last-good name off the live Deployment.
	if configMapName == "" {
		conditions.SetCondition(&cinder.Status.Conditions, metav1.Condition{
			Type:               "DatabaseReady",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cinder.Generation,
			Reason:             conditionReasonDatabaseWaitingForConfig,
			Message:            "Waiting for the rendered config before provisioning the database schema",
		})
		return ctrl.Result{RequeueAfter: RequeueDatabaseWait}, nil
	}

	// Active upgrade: the shared flow handles the abort (spec.openStackRelease
	// reverted to the installed release), the target-changed guard, and the phase
	// dispatch.
	if cinder.Status.UpgradePhase != "" {
		// Enforce the decoupled-field contract mid-upgrade too. The shared flow's
		// target-changed guard only watches spec.openStackRelease, so a lone
		// spec.image.tag edit to an inconsistent release would otherwise slip
		// through and dispatch phase Jobs built from the wrong image (every phase
		// Job runs spec.image). Skip the check when spec.openStackRelease has been
		// reverted to the installed release: that is the shared flow's abort
		// trigger (SpecRelease == InstalledRelease in ReconcileUpgrade), which must
		// stay reachable even while the two fields disagree so a wedged upgrade can
		// always be unstuck.
		if cinder.Spec.OpenStackRelease != cinder.Status.InstalledRelease {
			if res, blocked := checkImageReleaseMismatch(cinder); blocked {
				return res, nil
			}
		}
		return database.ReconcileUpgrade(ctx, r.upgradeFlowParams(ctx, children, cinder, configMapName))
	}

	// Enforce the decoupled-field contract before either the upgrade or the
	// steady-state path advances release tracking (see checkImageReleaseMismatch).
	if res, blocked := checkImageReleaseMismatch(cinder); blocked {
		return res, nil
	}

	// Detect a release upgrade (patch-only and same-release changes stay on the
	// steady-state path).
	if database.IsUpgrade(cinder.Spec.OpenStackRelease, cinder.Status.InstalledRelease) {
		return database.InitiateUpgrade(ctx, r.upgradeFlowParams(ctx, children, cinder, configMapName))
	}

	// Steady-state db-sync. cinder-manage db sync applies every pending migration
	// in one pass, so there is no schema-check step (SchemaCheckCommand nil).
	// InstalledRelease is promoted to spec.openStackRelease on Job success.
	return database.ReconcileSyncJobs(ctx, database.SyncFlowParams{
		Client:   children,
		Scheme:   r.Scheme,
		Recorder: r.Recorder,
		Owner:    cinder,
		Jobs:     cinderJobSetParams(cinder, configMapName),
		RecordTerminal: func(jobSuffix string, observed *batchv1.Job) {
			r.recordDBJobTerminalState(ctx, cinder, jobSuffix, observed)
		},
		Conditions:       &cinder.Status.Conditions,
		Generation:       cinder.Generation,
		ConditionType:    "DatabaseReady",
		RequeueAfter:     RequeueDatabaseWait,
		InstalledRelease: &cinder.Status.InstalledRelease,
		// The release the marker is promoted to on success. It is
		// spec.openStackRelease rather than spec.image.tag so a digest-pinned image
		// still tracks the release the operator was told to converge to.
		ImageTag: cinder.Spec.OpenStackRelease,
	})
}

// checkImageReleaseMismatch enforces the decoupled-field contract between
// spec.openStackRelease and spec.image. spec.openStackRelease drives release
// tracking and the upgrade detection, but every migration Job and every workload
// runs spec.image — the two fields are deliberately separate (digest pinning)
// and nothing else enforces they agree. A tag-pinned image whose tag names a
// different OpenStack release would run the wrong cinder-manage binary against a
// schema already at its own HEAD: the expand/migrate/contract Jobs exit 0 as
// no-ops while the flow promotes status.installedRelease to a release the pods
// neither run nor migrated to.
//
// When the two disagree it sets DatabaseReady=False/ImageReleaseMismatch and
// returns (requeue, true) so the caller refuses to advance until the image is
// bumped in lockstep. A digest-pinned image (Tag == "") or a tag that does not
// parse as a release carries no comparable release string and is left to the
// operator's explicit spec.openStackRelease declaration, matching the
// digest-disables-tracking contract; the patch suffix is ignored so a patched
// image build (e.g. tag 2026.1-p1) still matches release 2026.1.
func checkImageReleaseMismatch(cinder *cinderv1alpha1.Cinder) (ctrl.Result, bool) {
	if cinder.Spec.Image.Tag == "" {
		return ctrl.Result{}, false
	}
	tagRel, tagErr := release.ParseRelease(cinder.Spec.Image.Tag)
	specRel, specErr := release.ParseRelease(cinder.Spec.OpenStackRelease)
	if tagErr != nil || specErr != nil {
		return ctrl.Result{}, false
	}
	if tagRel.Year == specRel.Year && tagRel.Minor == specRel.Minor {
		return ctrl.Result{}, false
	}
	conditions.SetCondition(&cinder.Status.Conditions, metav1.Condition{
		Type:               "DatabaseReady",
		Status:             metav1.ConditionFalse,
		ObservedGeneration: cinder.Generation,
		Reason:             conditionReasonImageReleaseMismatch,
		Message: fmt.Sprintf("spec.image.tag %q names OpenStack release %d.%d but spec.openStackRelease is %q; "+
			"bump spec.image in lockstep with spec.openStackRelease",
			cinder.Spec.Image.Tag, tagRel.Year, tagRel.Minor, cinder.Spec.OpenStackRelease),
	})
	return ctrl.Result{RequeueAfter: RequeueDatabaseWait}, true
}

// cinderWorkloadEnv is the environment every Cinder process runs with: the
// database URL, the message-bus transport URL, and — for a Cinder that
// configures the Keystone integration — the two service-user passwords. All four
// are oslo.config OS_<GROUP>__<OPTION> overrides sourced from Secrets, which is
// what keeps the credentials out of the rendered config the pods mount.
//
// The migration Jobs run it too: they read no bus and no Keystone, but the
// overrides are inert without the sections that consume them, and one
// environment for every process is what keeps a Job from ever migrating against
// a different database than the API serves.
func cinderWorkloadEnv(cinder *cinderv1alpha1.Cinder) []corev1.EnvVar {
	env := []corev1.EnvVar{
		database.ConnectionEnvVar(cinder.Name),
		messaging.TransportURLEnvVar(cinder.Name),
	}
	if serviceUser := keystoneServiceUser(cinder); serviceUser != nil {
		secretName := serviceUser.SecretRef.Name
		key := effectiveServiceUserKey(cinder)
		env = append(env,
			keystoneauth.PasswordEnvVar(secretName, key),
			keystoneauth.ServiceUserPasswordEnvVar(secretName, key))
	}
	return env
}

// cinderJobSetParams derives the shared migration-Job inputs from the Cinder CR:
// the config mount, the db-tls keypair, the workload environment, and the
// cinder-manage db sync command. The steady-state sync flow
// (database.ReconcileSyncJobs) and the upgrade-phase builders
// (upgradeFlowParams.BuildPhaseJob) both consume it, so a seeded Job carries the
// same pod spec as the desired one.
//
// The whole ConfigMap is mounted at cinderConfigDir rather than a per-key
// subset: the Jobs need no file selection, and the one extra file they see is
// scheduler.conf, which sets a host identity cinder-manage does not read.
func cinderJobSetParams(cinder *cinderv1alpha1.Cinder, configMapName string) database.JobSetParams {
	// Project the db-tls client keypair into the migration Jobs when database TLS
	// is enabled. The DSN in the derived <name>-db-connection Secret carries
	// ssl_ca/ssl_cert/ssl_key paths under dbTLSMountPath, so without the mount
	// cinder-manage cannot open them and every migration fails.
	var extraVolumes []corev1.Volume
	var extraMounts []corev1.VolumeMount
	if cinder.Spec.Database.TLS.IsEnabled() {
		volume, mount := cinderDBTLSVolumeAndMount(cinder)
		extraVolumes = append(extraVolumes, volume)
		extraMounts = append(extraMounts, mount)
	}
	return database.JobSetParams{
		InstanceName:      cinder.Name,
		Namespace:         cinder.Namespace,
		Image:             cinder.Spec.Image.Reference(),
		ConfigMapName:     configMapName,
		ConfigMountPath:   cinderConfigDir,
		Env:               cinderWorkloadEnv(cinder),
		ExtraVolumes:      extraVolumes,
		ExtraVolumeMounts: extraMounts,
		SyncCommand:       cinderDBSyncCommand,
		// No schema-check: cinder-manage db sync is an idempotent alembic upgrade
		// to head, so a second read-only Job would assert nothing the sync itself
		// has not already established. Release upgrades instead run the shared
		// expand-migrate-contract phase machine (see upgradeFlowParams).
		SchemaCheckCommand: nil,
	}
}

// cinderDBTLSVolumeAndMount builds the Volume + VolumeMount pair projecting the
// client TLS material (ca.crt from caBundleSecretRef; tls.crt + tls.key from
// clientCertSecretRef) at dbTLSMountPath, so the ssl_ca/ssl_cert/ssl_key DSN
// paths derived from that mount point stay a single source of truth. Callers
// must only invoke it while spec.database.tls is enabled. DefaultMode 0o400 lets
// the openstack UID read the material while group and world have no access.
func cinderDBTLSVolumeAndMount(cinder *cinderv1alpha1.Cinder) (corev1.Volume, corev1.VolumeMount) {
	tlsSpec := cinder.Spec.Database.TLS
	volume := corev1.Volume{
		Name: dbTLSVolumeName,
		VolumeSource: corev1.VolumeSource{
			Projected: &corev1.ProjectedVolumeSource{
				DefaultMode: ptr.To(int32(0o400)),
				Sources: []corev1.VolumeProjection{
					{
						Secret: &corev1.SecretProjection{
							LocalObjectReference: corev1.LocalObjectReference{
								Name: tlsSpec.CABundleSecretRef.Name,
							},
							Items: []corev1.KeyToPath{
								{Key: database.TLSCAFileName, Path: database.TLSCAFileName},
							},
						},
					},
					{
						Secret: &corev1.SecretProjection{
							LocalObjectReference: corev1.LocalObjectReference{
								Name: tlsSpec.ClientCertSecretRef.Name,
							},
							Items: []corev1.KeyToPath{
								{Key: database.TLSCertFileName, Path: database.TLSCertFileName},
								{Key: database.TLSKeyFileName, Path: database.TLSKeyFileName},
							},
						},
					},
				},
			},
		},
	}
	mount := corev1.VolumeMount{
		Name:      dbTLSVolumeName,
		MountPath: dbTLSMountPath,
		ReadOnly:  true,
	}
	return volume, mount
}

// upgradeFlowParams assembles the shared expand-migrate-contract upgrade-flow
// inputs for this Cinder CR. The service-specific parts — the owner CR, the
// "DatabaseReady" condition vocabulary, spec.openStackRelease as the requested
// release, the per-phase Job builder, and the terminal callback — are bound
// here; the phase choreography itself lives in internal/common/database. The
// three status pointers (UpgradePhase, InstalledRelease, TargetRelease) are
// mutated in place by the flow and persisted by the caller after it returns.
func (r *CinderReconciler) upgradeFlowParams(ctx context.Context, children client.Client,
	cinder *cinderv1alpha1.Cinder, configMapName string,
) database.UpgradeFlowParams {
	return database.UpgradeFlowParams{
		Client:           children,
		Scheme:           r.Scheme,
		Recorder:         r.Recorder,
		Owner:            cinder,
		Conditions:       &cinder.Status.Conditions,
		Generation:       cinder.Generation,
		ConditionType:    "DatabaseReady",
		RequeueAfter:     RequeueUpgradeWait,
		Phase:            &cinder.Status.UpgradePhase,
		InstalledRelease: &cinder.Status.InstalledRelease,
		TargetRelease:    &cinder.Status.TargetRelease,
		SpecRelease:      cinder.Spec.OpenStackRelease,
		// Each phase Job runs spec.image: the operator's contract is that the image
		// tag is bumped together with spec.openStackRelease (checkImageReleaseMismatch
		// enforces it), so the new release's migration tree owns the schema deltas.
		//
		// Cinder has no separate expand/contract verbs. The expand phase runs the
		// same idempotent cinder-manage db sync the steady state does, and the
		// contract phase runs the online data migrations that backfill the rows the
		// new schema needs — which is what the RollingUpdate before it makes safe,
		// because by then every pod runs the target release.
		BuildPhaseJob: func(phase commonv1.UpgradePhase) *batchv1.Job {
			params := cinderJobSetParams(cinder, configMapName)
			image := cinder.Spec.Image.Reference()
			switch phase {
			case commonv1.UpgradePhaseExpanding:
				return database.BuildJob(params, image, upgradeExpandJobSuffix,
					cinderDBSyncCommand, upgradePhaseJobBackoffLimit)
			case commonv1.UpgradePhaseMigrating:
				return database.BuildJob(params, image, upgradeMigrateJobSuffix,
					[]string{"/bin/sh", "-eu", "-c", upgradeCheckScript}, upgradePhaseJobBackoffLimit)
			case commonv1.UpgradePhaseContracting:
				return database.BuildJob(params, image, upgradeContractJobSuffix,
					[]string{"cinder-manage", "--config-dir", cinderConfigDir, "db", "online_data_migrations"},
					upgradePhaseJobBackoffLimit)
			case commonv1.UpgradePhaseRollingUpdate:
				// RollingUpdate builds no Job; the Deployment rollout drives it.
				return nil
			default:
				// The empty steady-state phase never reaches BuildPhaseJob.
				return nil
			}
		},
		RecordTerminal: func(jobSuffix string, observed *batchv1.Job) {
			r.recordUpgradePhaseTerminal(ctx, children, cinder, jobSuffix, observed)
		},
	}
}

// recordUpgradePhaseTerminal emits the terminal-state metric of a finished
// upgrade-phase Job and, for the migrate phase, reports what cinder-status
// upgrade check found. The metric is the same one the steady-state db-sync
// emits; the check report is the migrate phase's own, because its exit code is a
// severity the Job deliberately swallows.
func (r *CinderReconciler) recordUpgradePhaseTerminal(ctx context.Context, children client.Client,
	cinder *cinderv1alpha1.Cinder, jobSuffix string, observed *batchv1.Job,
) {
	r.recordDBJobTerminalState(ctx, cinder, jobSuffix, observed)
	if jobSuffix == upgradeMigrateJobSuffix {
		r.reportUpgradeCheck(ctx, children, cinder, observed)
	}
}

// reportUpgradeCheck turns the migrate Job's termination log into an event on
// the Cinder: Normal UpgradeCheckCompleted for a clean run, Warning
// UpgradeCheckWarnings carrying the message otherwise (exit 1 are warnings, exit
// 2 is a failed check — typically a volume whose service_uuid is still NULL —
// and neither wedges the upgrade). A pod the operator cannot read the message
// off leaves a Normal event saying so rather than a silent gap.
//
// It is best-effort throughout: the message is diagnostic, so a List failure is
// logged and reported as unavailable rather than failing the reconcile that is
// otherwise done. The shared per-Job-UID dedupe keeps it to one event per Job.
func (r *CinderReconciler) reportUpgradeCheck(ctx context.Context, children client.Client,
	cinder *cinderv1alpha1.Cinder, observed *batchv1.Job,
) {
	job.RecordJobTerminalState(ctx, r.Client, r.Recorder, cinder,
		upgradeMigrateJobSuffix+"-check", observed, "UpgradeCheckEventEmissionDeferred",
		func(string, time.Duration) {
			message := upgradeCheckMessage(ctx, children, cinder.Namespace, observed.Name)
			switch {
			case message == "":
				r.Recorder.Event(cinder, corev1.EventTypeNormal, "UpgradeCheckCompleted",
					"cinder-status upgrade check exit code unavailable")
			case strings.HasSuffix(message, "exit 0"):
				r.Recorder.Event(cinder, corev1.EventTypeNormal, "UpgradeCheckCompleted", message)
			default:
				r.Recorder.Event(cinder, corev1.EventTypeWarning, "UpgradeCheckWarnings", message)
			}
		})
}

// upgradeCheckMessage reads the termination message of the migrate Job's pod:
// the first pod the Job labels, its first container, the current termination
// state or the previous one when the container has already been restarted. It
// returns "" when there is no pod, no message, or the List failed — the three
// cases the caller reports as unavailable. The List error is logged rather than
// returned: nothing about the upgrade depends on the message.
func upgradeCheckMessage(ctx context.Context, children client.Client, namespace, jobName string) string {
	var pods corev1.PodList
	if err := children.List(ctx, &pods, client.InNamespace(namespace),
		client.MatchingLabels{"batch.kubernetes.io/job-name": jobName}); err != nil {
		log.FromContext(ctx).Info("listing the pods of the upgrade-check Job failed; "+
			"the cinder-status exit code is reported as unavailable",
			"job", jobName, "err", err.Error())
		return ""
	}
	if len(pods.Items) == 0 {
		return ""
	}
	statuses := pods.Items[0].Status.ContainerStatuses
	if len(statuses) == 0 {
		return ""
	}
	if terminated := statuses[0].State.Terminated; terminated != nil && terminated.Message != "" {
		return terminated.Message
	}
	if terminated := statuses[0].LastTerminationState.Terminated; terminated != nil {
		return terminated.Message
	}
	return ""
}
