// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"cmp"
	"context"
	"crypto/sha256"
	"fmt"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/apply"
	"github.com/c5c3/cobaltcore/internal/common/conditions"
	"github.com/c5c3/cobaltcore/internal/common/deployment"
	"github.com/c5c3/cobaltcore/internal/common/job"
	"github.com/c5c3/cobaltcore/internal/common/naming"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
	ovnmetrics "github.com/c5c3/cobaltcore/operators/ovn/internal/metrics"
)

// conditionTypeBackupReady is the condition the backup step reports under. It is
// True while the CronJob is projected and the most recent run it spawned did not
// fail, and False while a run has failed or the volume the snapshots go on
// cannot be projected.
//
// A suspended backup stays True: pausing it is an operator's deliberate posture
// during a maintenance window, not a failure. It reports under its own reason,
// because a paused backup is the one state in which the CR looks healthy while
// the snapshots the CR exists to produce are not being taken.
const conditionTypeBackupReady = "BackupReady"

// The condition reasons of the backup step.
const (
	conditionReasonBackupScheduled  = "BackupScheduled"
	conditionReasonBackupSuspended  = "BackupSuspended"
	conditionReasonBackupJobFailed  = "BackupJobFailed"
	conditionReasonBackupPVCInvalid = "BackupPVCInvalid"
	conditionReasonBackupError      = "BackupError"
)

// componentBackup is the component-label value of the backup children and the
// name suffix of the volume they write to.
const componentBackup = "backup"

// backupNameSuffix names the backup CronJob and the volume after their
// OVNCentral. The api package mirrors this literal (it derives
// ovnv1alpha1.MaxOVNCentralNameLength from it) because it cannot import the
// controller package.
const backupNameSuffix = "-backup"

// backupNameHashLength is what the collapsed CronJob name below spends on the
// hash: a separator plus eight hex characters.
const backupNameHashLength = 1 + 8

// backupMountPath is where the snapshot volume is mounted in both containers of
// a backup run.
const backupMountPath = "/backup"

// backupScriptKey names the backup script in the shared scripts ConfigMap.
const backupScriptKey = "backup.sh"

// backupActiveDeadlineSeconds caps how long one run may stay active. It is what
// keeps a wedged backup observable: the step reports on runs that reached a
// terminal condition, and a pod stuck in ImagePullBackOff or an ovsdb-client
// blocked on an unreachable Raft leader reaches none on its own, so the run
// would stay active forever while BackupReady kept reporting a healthy
// schedule. Ten minutes is far above the runtime of a snapshot of two databases
// that hold a logical network model.
const backupActiveDeadlineSeconds int64 = 600

// backupScript snapshots both databases onto the backup volume and prunes the
// snapshots that fell out of the retention window. It is the fifth key of the
// shared scripts ConfigMap.
//
// Each snapshot is written to a ".tmp" file that is renamed only after
// ovsdb-client exits successfully and the file it wrote is non-empty, so a run
// that dies mid-copy leaves no file that looks like a snapshot and a run that
// produced nothing fails instead of reporting success: a volume that filled up
// lets ovsdb-client exit zero with nothing written, and a zero-byte file
// restores no database.
//
// A run the deadline or a drained node kills outright leaves its ".tmp" behind,
// and neither retention prune matches that name, so the leading sweep is what
// keeps a schedule that periodically hits backupActiveDeadlineSeconds from
// filling the volume with half-written dumps. The trailing zero-byte prune is
// the backstop for the leftovers of an operator version that had neither.
//
// The destination directory is read from BACKUP_DIR rather than hard-coded so
// the script can be exercised by backup_script_test.go, which runs it against a
// temp directory. The CronJob sets no BACKUP_DIR, so in production the fallback
// applies and the snapshots go to the mounted volume.
const backupScript = `#!/bin/bash
set -eu
dir="${BACKUP_DIR:-` + backupMountPath + `}"
ts="$(date -u +%Y%m%dT%H%M%SZ)"
find "${dir}" -name '*.backup.tmp' -type f -mtime +1 -delete
for spec in "nb:${NB_ADDR}:OVN_Northbound" "sb:${SB_ADDR}:OVN_Southbound"; do
  db="${spec%%:*}"; rest="${spec#*:}"; schema="${rest##*:}"; addr="${rest%:*}"
  out="${dir}/${db}-${ts}.backup"
  if ! ovsdb-client -p ` + ovnTLSDir + `/tls.key -c ` + ovnTLSDir + `/tls.crt -C ` + ovnTLSDir + `/ca.crt \
      backup "${addr}" "${schema}" > "${out}.tmp"; then
    rm -f "${out}.tmp"; echo "backup of ${schema} at ${addr} failed" >&2; exit 1
  fi
  if [ ! -s "${out}.tmp" ]; then
    rm -f "${out}.tmp"; echo "backup of ${schema} at ${addr} produced an empty snapshot" >&2; exit 1
  fi
  mv "${out}.tmp" "${out}"
done
find "${dir}" -name '*.backup' -type f -mtime "+${RETENTION_DAYS}" -delete
find "${dir}" -name '*.backup' -type f -size 0 -delete
`

// effectiveBackup returns the settings the backup CronJob runs with,
// materializing the operator defaults for the fields spec.backup leaves unset. A
// nil block behaves exactly like an empty one. It is pure and total: no input
// can fail, and the result is always fully resolved (Schedule non-empty,
// RetentionDays non-nil, Storage.Size non-empty).
//
// Resolving here rather than in the defaulting webhook keeps an unset field
// tracking the operator default across upgrades instead of freezing today's
// value into the stored CR, which is the contract effectiveImage follows too.
func effectiveBackup(cr *ovnv1alpha1.OVNCentral) ovnv1alpha1.OVNBackupSpec {
	var out ovnv1alpha1.OVNBackupSpec
	if cr.Spec.Backup != nil {
		out = *cr.Spec.Backup
	}
	out.Schedule = cmp.Or(out.Schedule, ovnv1alpha1.DefaultBackupSchedule)
	if out.RetentionDays == nil {
		out.RetentionDays = ptr.To(ovnv1alpha1.DefaultBackupRetentionDays)
	}
	out.Storage.Size = cmp.Or(out.Storage.Size, defaultStorageSize)
	return out
}

// backupCronJobName returns the name of the backup CronJob, collapsing an
// over-long OVNCentral name onto a content-stable hash.
//
// Admission bounds metadata.name so the plain "{name}-backup" form fits the
// MaxCronJobNameLength characters Kubernetes allows a CronJob name, but on
// create only, and it has to be: metadata.name is immutable, so on update the
// rule could only fire against an object a pre-upgrade operator already
// admitted, including the finalizer-removal update that completes its deletion.
// An operator upgrade therefore inherits OVNCentral CRs the bound would reject,
// and building their name unconditionally would fail every reconcile on an apply
// the API server refuses, forever, with no field left to edit to repair it.
// Every admissible name keeps the documented plain form.
func backupCronJobName(name string) string {
	if len(name) <= ovnv1alpha1.MaxOVNCentralNameLength {
		return name + backupNameSuffix
	}
	sum := sha256.Sum256([]byte(name))
	// The trim keeps the truncation from ending a DNS label on "-" or ".",
	// which would make the very name this function exists to produce
	// unapplicable. It only ever shortens, so the cap still holds.
	kept := strings.TrimRight(name[:ovnv1alpha1.MaxOVNCentralNameLength-backupNameHashLength], "-.")
	return fmt.Sprintf("%s-%x%s", kept, sum[:4], backupNameSuffix)
}

// backupVolumeName names the PersistentVolumeClaim the snapshots are written to.
// It keeps the plain form for every name, over-long ones included: a claim name
// is bounded by the DNS subdomain grammar rather than by the CronJob cap, so
// there is nothing to collapse.
func backupVolumeName(cr *ovnv1alpha1.OVNCentral) string {
	return cr.Name + backupNameSuffix
}

// reconcileBackup projects the snapshot volume and the CronJob that writes to
// it, and reports the outcome of the most recent run.
//
// A Raft cluster survives the loss of a minority of its members but not an
// operator error applied to all of them, so these snapshots are the only path
// back from a corrupted logical model. The CronJob is ensured on every pass: an
// OVNCentral without spec.backup still gets one, and spec.backup only varies its
// schedule, retention, suspension, volume, and whether the snapshots are copied
// off-cluster.
//
// Run visibility is derived rather than watched: the CronJob controller spawns
// one Job per firing and prunes them by history limit, so the step lists the
// Jobs carrying the backup component labels, keeps the ones the CronJob
// controls, and reports on the newest that reached a terminal state.
func (r *OVNCentralReconciler) reconcileBackup(ctx context.Context, children client.Client, cr *ovnv1alpha1.OVNCentral) (ctrl.Result, error) {
	backup := effectiveBackup(cr)

	// The two addresses reach the run as environment variables, so nothing is
	// projected before the endpoint step has published them. Applying the CronJob
	// early would schedule a run that connects to the empty string, and applying
	// only the volume would leave a claim bound to a node for a run that cannot
	// happen yet.
	if cr.Status.Northbound.InternalDbAddress == "" || cr.Status.Southbound.InternalDbAddress == "" {
		conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
			Type:               conditionTypeBackupReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cr.Generation,
			Reason:             conditionReasonWaitingForEndpoints,
			Message:            "Waiting for both database addresses to be published",
		})
		return ctrl.Result{RequeueAfter: RequeueRaftWait}, nil
	}

	if err := apply.EnsureObject(ctx, children, r.Scheme, cr, backupPVC(cr, backup), apply.FieldManager); err != nil {
		// A claim cannot shrink, so lowering spec.backup.storage.size makes the
		// API server reject the apply as Invalid on every pass. Retrying cannot
		// fix it and neither can the operator, so the API server's own message is
		// surfaced on the condition and the error is not returned: a returned
		// error would put the CR in exponential backoff over a spec edit that only
		// a human can undo.
		if apierrors.IsInvalid(err) {
			conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
				Type:               conditionTypeBackupReady,
				Status:             metav1.ConditionFalse,
				ObservedGeneration: cr.Generation,
				Reason:             conditionReasonBackupPVCInvalid,
				Message: fmt.Sprintf("The API server rejected PersistentVolumeClaim %s: %v",
					backupVolumeName(cr), err),
			})
			return ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, nil
		}
		err = fmt.Errorf("ensuring backup PVC: %w", err)
		centralSkeleton.MarkFailed(cr, conditionTypeBackupReady, conditionReasonBackupError, err)
		return ctrl.Result{}, err
	}

	cronJob := backupCronJob(cr, backup)
	if err := job.EnsureCronJob(ctx, children, r.Scheme, cr, cronJob); err != nil {
		err = fmt.Errorf("ensuring backup CronJob: %w", err)
		centralSkeleton.MarkFailed(cr, conditionTypeBackupReady, conditionReasonBackupError, err)
		return ctrl.Result{}, err
	}

	var jobs batchv1.JobList
	if err := children.List(ctx, &jobs, client.InNamespace(cr.Namespace),
		client.MatchingLabels(naming.ComponentLabels(centralAppName, cr.Name, componentBackup))); err != nil {
		err = fmt.Errorf("listing backup Jobs: %w", err)
		centralSkeleton.MarkFailed(cr, conditionTypeBackupReady, conditionReasonBackupError, err)
		return ctrl.Result{}, err
	}
	// EnsureCronJob decodes the apply response back into cronJob, so it carries
	// the persisted UID the owner-reference match below compares against.
	observed := newestTerminalJob(cronJob, jobs.Items)
	failed := observed != nil && job.TerminalCondition(observed) == batchv1.JobFailed

	switch {
	// Suspension outranks a failed run, because a suspended CronJob spawns no
	// successor to supersede it: the failed Job stays the newest terminal one for
	// good, so a JobFailed arm that won here would pin BackupReady False, and
	// with it the aggregate Ready, until someone deleted the Job by hand. The
	// failure is still named in the message; it is just not the state the CR is
	// in.
	case backup.Suspend:
		message := fmt.Sprintf("Backup CronJob suspended via spec.backup.suspend; no snapshots are being taken "+
			"(schedule %q, retention %d days when resumed)", backup.Schedule, *backup.RetentionDays)
		if failed {
			message += fmt.Sprintf("; the last run (Job %s) failed before the pause", observed.Name)
		}
		conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
			Type:               conditionTypeBackupReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: cr.Generation,
			Reason:             conditionReasonBackupSuspended,
			Message:            message,
		})
	case failed:
		conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
			Type:               conditionTypeBackupReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cr.Generation,
			Reason:             conditionReasonBackupJobFailed,
			Message: fmt.Sprintf("Backup Job %s failed; the snapshots are no longer current and a restore "+
				"would lose everything written since the last successful run", observed.Name),
		})
	default:
		conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
			Type:               conditionTypeBackupReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: cr.Generation,
			Reason:             conditionReasonBackupScheduled,
			Message: fmt.Sprintf("Backup scheduled %q, keeping snapshots of both databases for %d days",
				backup.Schedule, *backup.RetentionDays),
		})
	}

	// The shared helper dedupes on the Job UID via an annotation on the
	// OVNCentral, so a run feeds the metric exactly once no matter how many
	// passes observe it, and a nil observed (no terminal run yet) is a no-op. The
	// annotation lands on the owner CR, which is why the patch goes through the
	// embedded client rather than through children.
	//
	// The Warning event rides the same callback rather than the condition arm
	// above: the arm runs on every pass, so an event raised there would re-fire
	// the same failure until the next firing superseded it.
	job.RecordJobTerminalState(ctx, r.Client, r.Recorder, cr, componentBackup, observed,
		"BackupMetricEmissionDeferred",
		func(result string, duration time.Duration) {
			ovnmetrics.RecordBackup(cr.Name, cr.Namespace, result, duration)
			if result == "failed" {
				r.Recorder.Eventf(cr, corev1.EventTypeWarning, conditionReasonBackupJobFailed,
					"Backup Job %s failed; inspect its pod logs. The databases keep running, but there is no "+
						"snapshot of what they hold now", observed.Name)
			}
		})

	return ctrl.Result{}, nil
}

// newestTerminalJob returns the most recently created Job among jobs that the
// given CronJob controls and that has reached a terminal state, or nil when no
// such Job exists yet. Newest wins because the CronJob keeps a bounded history:
// an older failed run must not keep the condition False once a later run has
// succeeded.
func newestTerminalJob(cronJob *batchv1.CronJob, jobs []batchv1.Job) *batchv1.Job {
	var newest *batchv1.Job
	for i := range jobs {
		candidate := &jobs[i]
		if !metav1.IsControlledBy(candidate, cronJob) {
			continue
		}
		if job.TerminalCondition(candidate) == "" {
			continue
		}
		if newest == nil || candidate.CreationTimestamp.After(newest.CreationTimestamp.Time) {
			newest = candidate
		}
	}
	return newest
}

// backupPVC builds the volume the snapshots are written to. It is a plain
// ReadWriteOnce claim: one run writes it at a time, and the CronJob forbids
// concurrent runs. The storage class is only set when the CR names one, so a CR
// that names none takes the cluster's default class rather than pinning the
// empty string.
func backupPVC(cr *ovnv1alpha1.OVNCentral, backup ovnv1alpha1.OVNBackupSpec) *corev1.PersistentVolumeClaim {
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      backupVolumeName(cr),
			Namespace: cr.Namespace,
			Labels:    naming.ComponentLabels(centralAppName, cr.Name, componentBackup),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(backup.Storage.Size)},
			},
		},
	}
	if backup.Storage.StorageClassName != nil {
		claim.Spec.StorageClassName = backup.Storage.StorageClassName
	}
	return claim
}

// backupCronJob builds the CronJob that snapshots both databases.
//
// Without spec.backup.s3 the snapshot container is the whole run. With it the
// snapshot moves into an init container and the upload becomes the main one, so
// a failed snapshot never reaches the upload and the Job's terminal condition
// covers both halves: the pod is Failed whichever of the two exits non-zero.
func backupCronJob(cr *ovnv1alpha1.OVNCentral, backup ovnv1alpha1.OVNBackupSpec) *batchv1.CronJob {
	labels := naming.ComponentLabels(centralAppName, cr.Name, componentBackup)

	podSpec := corev1.PodSpec{
		// Never rather than OnFailure: backoffLimit 0 already makes the first
		// failure terminal, and a restarting pod would leave the Job active past
		// the point the failure is worth reporting.
		RestartPolicy: corev1.RestartPolicyNever,
		// fsGroup hands the snapshot volume to the group the containers run as.
		// Without it the run cannot write to a freshly provisioned volume, which
		// is owned by root.
		SecurityContext: &corev1.PodSecurityContext{
			FSGroup: ptr.To(deployment.OpenStackUID),
			SeccompProfile: &corev1.SeccompProfile{
				Type: corev1.SeccompProfileTypeRuntimeDefault,
			},
		},
		Containers: []corev1.Container{backupContainer(cr, backup)},
		Volumes:    backupVolumes(cr),
	}
	if backup.S3 != nil {
		podSpec.InitContainers = []corev1.Container{backupContainer(cr, backup)}
		podSpec.Containers = []corev1.Container{shifterContainer(cr, backup.S3)}
	}

	return &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      backupCronJobName(cr.Name),
			Namespace: cr.Namespace,
			Labels:    labels,
		},
		Spec: batchv1.CronJobSpec{
			Schedule: backup.Schedule,
			Suspend:  ptr.To(backup.Suspend),
			// Two runs writing the same volume would interleave their pruning with
			// the other's snapshots, and a ReadWriteOnce claim cannot mount into
			// two pods on different nodes anyway: the second run would sit Pending
			// until the first finished, one such run per firing with no ceiling.
			ConcurrencyPolicy:          batchv1.ForbidConcurrent,
			SuccessfulJobsHistoryLimit: ptr.To(int32(3)),
			FailedJobsHistoryLimit:     ptr.To(int32(3)),
			JobTemplate: batchv1.JobTemplateSpec{
				// The spawned Jobs inherit these labels, which is what lets the
				// reconcile list the runs by label instead of by name: the CronJob
				// controller names each Job after the firing timestamp.
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: batchv1.JobSpec{
					ActiveDeadlineSeconds: ptr.To(backupActiveDeadlineSeconds),
					// A retry would run against the same unreachable database or the
					// same full volume and produce the same failure, minutes later,
					// while the condition kept reporting a run in flight.
					BackoffLimit: ptr.To(int32(0)),
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: labels},
						Spec:       podSpec,
					},
				},
			},
		},
	}
}

// backupContainer builds the container that snapshots both databases. It runs
// the OVN image because ovsdb-client is what takes the snapshot, and it is
// handed the two published addresses rather than a discovery mechanism, for the
// reason northd is.
func backupContainer(cr *ovnv1alpha1.OVNCentral, backup ovnv1alpha1.OVNBackupSpec) corev1.Container {
	return corev1.Container{
		Name:            componentBackup,
		Image:           effectiveImage(cr.Spec.Image).Reference(),
		Command:         []string{"/bin/bash", centralScriptDir + "/" + backupScriptKey},
		SecurityContext: deployment.RestrictedSecurityContext(),
		Env: []corev1.EnvVar{
			{Name: "NB_ADDR", Value: cr.Status.Northbound.InternalDbAddress},
			{Name: "SB_ADDR", Value: cr.Status.Southbound.InternalDbAddress},
			{Name: "RETENTION_DAYS", Value: strconv.Itoa(int(*backup.RetentionDays))},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: componentBackup, MountPath: backupMountPath},
			{Name: tlsVolumeName, MountPath: ovnTLSDir, ReadOnly: true},
			{Name: scriptsVolumeName, MountPath: centralScriptDir},
			{Name: tmpVolumeName, MountPath: "/tmp"},
		},
	}
}

// shifterContainer builds the container that copies the snapshots to S3. It
// mounts the volume read-only: the retention window is the snapshot container's
// to enforce, and an upload that could delete would make a misconfigured bucket
// prefix destructive.
//
// rclone is configured entirely through environment variables, so no config file
// has to be projected. NO_CHECK_BUCKET skips the bucket-creation probe, which
// most S3-compatible implementations refuse to an account that may only write
// into an existing bucket.
func shifterContainer(cr *ovnv1alpha1.OVNCentral, s3 *ovnv1alpha1.OVNBackupS3Spec) corev1.Container {
	credential := func(key string) *corev1.EnvVarSource {
		return &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: s3.CredentialsSecretRef.Name},
				Key:                  key,
			},
		}
	}

	return corev1.Container{
		Name:            "shifter",
		Image:           effectiveShifterImage(s3.Image).Reference(),
		Command:         []string{"/bin/sh", "-c", `rclone copy ` + backupMountPath + ` ":s3:${BUCKET}/${PREFIX}"`},
		SecurityContext: deployment.RestrictedSecurityContext(),
		Env: []corev1.EnvVar{
			{Name: "RCLONE_S3_PROVIDER", Value: "Other"},
			{Name: "RCLONE_S3_ENDPOINT", Value: s3.Endpoint},
			{Name: "RCLONE_S3_REGION", Value: s3.Region},
			{Name: "RCLONE_S3_NO_CHECK_BUCKET", Value: "true"},
			{Name: "RCLONE_S3_ACCESS_KEY_ID", ValueFrom: credential("access-key-id")},
			{Name: "RCLONE_S3_SECRET_ACCESS_KEY", ValueFrom: credential("secret-access-key")},
			{Name: "BUCKET", Value: s3.Bucket},
			{Name: "PREFIX", Value: s3.Prefix},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: componentBackup, MountPath: backupMountPath, ReadOnly: true},
			{Name: tmpVolumeName, MountPath: "/tmp"},
		},
	}
}

// backupVolumes builds the pod volumes of a backup run. The scripts ConfigMap is
// the same one the database pods mount, so the snapshot script cannot drift from
// the run scripts it snapshots behind.
func backupVolumes(cr *ovnv1alpha1.OVNCentral) []corev1.Volume {
	return []corev1.Volume{
		{Name: componentBackup, VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: backupVolumeName(cr),
			},
		}},
		// The client keypair, not a server one: the run connects to both databases
		// the way any other OVN client does.
		{Name: tlsVolumeName, VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: clientSecretName(cr)},
		}},
		// 0555 rather than the default 0644: the container executes this file, and
		// a ConfigMap volume carries no executable bit unless it is asked for.
		{Name: scriptsVolumeName, VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: centralScriptsName(cr)},
				DefaultMode:          ptr.To(int32(0o555)),
			},
		}},
		{Name: tmpVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}
}
