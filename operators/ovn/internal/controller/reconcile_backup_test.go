// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	dto "github.com/prometheus/client_model/go"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/c5c3/cobaltcore/internal/common/job"
	"github.com/c5c3/cobaltcore/internal/common/naming"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
	ovnmetrics "github.com/c5c3/cobaltcore/operators/ovn/internal/metrics"
)

// backupOVNCentral is the fixture the backup step runs against: both database
// addresses published, and a UID so the children it applies carry a resolvable
// controller reference.
func backupOVNCentral() *ovnv1alpha1.OVNCentral {
	cr := publishEndpoints(testOVNCentral())
	cr.UID = "ovn-central-uid"
	return cr
}

// seededBackupCronJob returns the backup CronJob carrying a stable UID, so a Job
// seeded with a controller reference to it is recognised by
// metav1.IsControlledBy. The fake client mints no UID on create, so the CronJob
// has to be seeded rather than only created by the reconcile.
func seededBackupCronJob(cr *ovnv1alpha1.OVNCentral) *batchv1.CronJob {
	cronJob := backupCronJob(cr, effectiveBackup(cr))
	cronJob.UID = types.UID(cr.Name + "-backup-uid")
	return cronJob
}

// backupRunJob returns a Job as the CronJob controller would spawn it: the
// inherited component labels the reconcile lists by, the controller reference it
// filters on, the given terminal condition, and a stable UID for the
// terminal-metric dedupe.
func backupRunJob(cr *ovnv1alpha1.OVNCentral, cronJob *batchv1.CronJob, name string,
	condition batchv1.JobConditionType, createdAt metav1.Time,
) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         cr.Namespace,
			UID:               types.UID(name + "-uid"),
			CreationTimestamp: createdAt,
			Labels:            naming.ComponentLabels(centralAppName, cr.Name, componentBackup),
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: batchv1.SchemeGroupVersion.String(),
				Kind:       "CronJob",
				Name:       cronJob.Name,
				UID:        cronJob.UID,
				Controller: ptr.To(true),
			}},
		},
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{
				{Type: condition, Status: corev1.ConditionTrue, LastTransitionTime: createdAt},
			},
		},
	}
}

// backupCounter reads one ovn_operator_backup_total series off the
// controller-runtime registry, or returns nil when the CR has none with those
// labels.
func backupCounter(t *testing.T, cr *ovnv1alpha1.OVNCentral, result string) *dto.Metric {
	t.Helper()
	g := NewGomegaWithT(t)

	families, err := ctrlmetrics.Registry.Gather()
	g.Expect(err).NotTo(HaveOccurred())
	want := map[string]string{"ovncentral": cr.Name, "namespace": cr.Namespace, "result": result}
	for _, fam := range families {
		if fam.GetName() != "ovn_operator_backup_total" {
			continue
		}
		for _, m := range fam.GetMetric() {
			got := map[string]string{}
			for _, l := range m.GetLabel() {
				got[l.GetName()] = l.GetValue()
			}
			if got["ovncentral"] == want["ovncentral"] && got["namespace"] == want["namespace"] &&
				got["result"] == want["result"] {
				return m
			}
		}
	}
	return nil
}

// registerBackupMetrics exposes the collectors and drops this CR's series again
// when the test ends, so one test's counts never reach another's assertions.
func registerBackupMetrics(t *testing.T, cr *ovnv1alpha1.OVNCentral) {
	t.Helper()
	g := NewGomegaWithT(t)

	g.Expect(ovnmetrics.Register()).To(Succeed())
	t.Cleanup(func() { ovnmetrics.DeleteForOVNCentral(cr.Name, cr.Namespace) })
}

func TestEffectiveBackup(t *testing.T) {
	tests := []struct {
		name              string
		backup            *ovnv1alpha1.OVNBackupSpec
		wantSchedule      string
		wantRetentionDays int32
		wantSize          string
		wantSuspend       bool
		wantS3            bool
	}{
		{
			name:              "nil block resolves every default",
			backup:            nil,
			wantSchedule:      ovnv1alpha1.DefaultBackupSchedule,
			wantRetentionDays: ovnv1alpha1.DefaultBackupRetentionDays,
			wantSize:          defaultStorageSize,
		},
		{
			name:              "empty block behaves like a nil one",
			backup:            &ovnv1alpha1.OVNBackupSpec{},
			wantSchedule:      ovnv1alpha1.DefaultBackupSchedule,
			wantRetentionDays: ovnv1alpha1.DefaultBackupRetentionDays,
			wantSize:          defaultStorageSize,
		},
		{
			name:              "a stated schedule leaves the other defaults alone",
			backup:            &ovnv1alpha1.OVNBackupSpec{Schedule: "*/30 * * * *"},
			wantSchedule:      "*/30 * * * *",
			wantRetentionDays: ovnv1alpha1.DefaultBackupRetentionDays,
			wantSize:          defaultStorageSize,
		},
		{
			// One day is the floor the webhook admits, and it is not the default,
			// so a resolver that treated any non-nil pointer as unset would show up
			// here rather than in the nil case.
			name:              "a stated retention is kept",
			backup:            &ovnv1alpha1.OVNBackupSpec{RetentionDays: ptr.To(int32(1))},
			wantSchedule:      ovnv1alpha1.DefaultBackupSchedule,
			wantRetentionDays: 1,
			wantSize:          defaultStorageSize,
		},
		{
			name: "a full block is returned unchanged",
			backup: &ovnv1alpha1.OVNBackupSpec{
				Schedule:      "0 */6 * * *",
				RetentionDays: ptr.To(int32(3)),
				Suspend:       true,
				Storage:       ovnv1alpha1.OVNStorageSpec{Size: "5Gi"},
				S3:            &ovnv1alpha1.OVNBackupS3Spec{Bucket: "ovn-backups"},
			},
			wantSchedule:      "0 */6 * * *",
			wantRetentionDays: 3,
			wantSize:          "5Gi",
			wantSuspend:       true,
			wantS3:            true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cr := testOVNCentral()
			cr.Spec.Backup = tc.backup

			got := effectiveBackup(cr)

			g.Expect(got.Schedule).To(Equal(tc.wantSchedule))
			g.Expect(got.RetentionDays).NotTo(BeNil(),
				"the resolver must be total: the CronJob builder dereferences this")
			g.Expect(*got.RetentionDays).To(Equal(tc.wantRetentionDays))
			g.Expect(got.Storage.Size).NotTo(BeEmpty(),
				"an empty size would panic resource.MustParse in the claim builder")
			g.Expect(got.Storage.Size).To(Equal(tc.wantSize))
			g.Expect(got.Suspend).To(Equal(tc.wantSuspend))
			g.Expect(got.S3 != nil).To(Equal(tc.wantS3))
		})
	}
}

// backupCronJobName has to be total. The metadata.name bound that keeps
// "{name}-backup" inside the CronJob cap is enforced on create only, and can
// only be, since metadata.name is immutable and rejecting the finalizer-removal
// update would wedge the CR in Terminating. An operator upgrade therefore
// inherits OVNCentral CRs the bound would reject, and building their name
// unconditionally would fail every reconcile on an apply the API server
// refuses, with nothing left to edit to repair it.
func TestBackupCronJobName(t *testing.T) {
	g := NewGomegaWithT(t)

	atLimit := strings.Repeat("o", ovnv1alpha1.MaxOVNCentralNameLength)
	g.Expect(backupCronJobName(atLimit)).To(Equal(atLimit+backupNameSuffix),
		"an admissible name keeps the documented {name}-backup form")

	overLimit := strings.Repeat("o", ovnv1alpha1.MaxOVNCentralNameLength+1)
	collapsed := backupCronJobName(overLimit)
	g.Expect(collapsed).To(HavePrefix(strings.Repeat("o", 36)))
	g.Expect(collapsed).To(HaveSuffix(backupNameSuffix))
	g.Expect(collapsed).To(HaveLen(ovnv1alpha1.MaxCronJobNameLength))

	// Sweep every truncation offset against both characters a DNS label may not
	// end on, so the collapsed name is applicable wherever the boundary falls.
	for _, sep := range []string{"-", "."} {
		for i := 1; i < ovnv1alpha1.MaxOVNCentralNameLength; i++ {
			name := strings.Repeat("o", i) + sep + strings.Repeat("v", ovnv1alpha1.MaxOVNCentralNameLength)
			got := backupCronJobName(name)
			g.Expect(len(got)).To(BeNumerically("<=", ovnv1alpha1.MaxCronJobNameLength), name)
			g.Expect(got).To(HaveSuffix(backupNameSuffix), name)
			g.Expect(utilvalidation.IsDNS1123Subdomain(got)).To(BeEmpty(), name)
			g.Expect(backupCronJobName(name)).To(Equal(got),
				"the name must be stable across passes, or every reconcile orphans the last CronJob")
		}
	}

	// Two CRs sharing the truncated prefix must not collapse onto one CronJob.
	shared := strings.Repeat("o", ovnv1alpha1.MaxOVNCentralNameLength)
	g.Expect(backupCronJobName(shared + "-alpha")).NotTo(Equal(backupCronJobName(shared + "-beta")))
}

// The steady-state pass: both children go in and the condition reports the
// schedule the snapshots are taken on.
func TestReconcileBackup_ProjectsTheVolumeAndTheCronJob(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := backupOVNCentral()
	r := newTestOVNCentralReconciler(t, cr)

	res, err := r.reconcileBackup(ctx, r.Client, cr)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())

	var claim corev1.PersistentVolumeClaim
	g.Expect(r.Get(ctx, centralKey("ovn-backup"), &claim)).To(Succeed())
	g.Expect(claim.Spec.Resources.Requests).To(HaveKey(corev1.ResourceStorage))

	var cronJob batchv1.CronJob
	g.Expect(r.Get(ctx, centralKey("ovn-backup"), &cronJob)).To(Succeed())
	g.Expect(cronJob.Spec.Schedule).To(Equal(ovnv1alpha1.DefaultBackupSchedule))

	cond := ovnCentralCondition(cr, conditionTypeBackupReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonBackupScheduled))
	g.Expect(cond.Message).To(ContainSubstring(ovnv1alpha1.DefaultBackupSchedule))
}

// The reconcile must go through for a CR the name bound would reject: the
// CronJob is applied under the collapsed name and BackupReady is reported like
// any other OVNCentral.
func TestReconcileBackup_AppliesForAnOverlongName(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := backupOVNCentral()
	cr.Name = strings.Repeat("o", ovnv1alpha1.MaxOVNCentralNameLength+1)
	r := newTestOVNCentralReconciler(t, cr)

	_, err := r.reconcileBackup(ctx, r.Client, cr)
	g.Expect(err).NotTo(HaveOccurred())

	var cronJob batchv1.CronJob
	g.Expect(r.Get(ctx, client.ObjectKey{
		Namespace: cr.Namespace, Name: backupCronJobName(cr.Name),
	}, &cronJob)).To(Succeed())

	cond := ovnCentralCondition(cr, conditionTypeBackupReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonBackupScheduled))
}

// The two addresses reach a run as environment variables. Projecting before the
// endpoint step has published them would schedule a run that connects to the
// empty string, so the step applies nothing at all.
func TestReconcileBackup_WaitingForEndpointsAppliesNoCronJob(t *testing.T) {
	for _, tc := range []struct {
		name  string
		clear func(cr *ovnv1alpha1.OVNCentral)
	}{
		{"northbound unpublished", func(cr *ovnv1alpha1.OVNCentral) {
			cr.Status.Northbound.InternalDbAddress = ""
		}},
		{"southbound unpublished", func(cr *ovnv1alpha1.OVNCentral) {
			cr.Status.Southbound.InternalDbAddress = ""
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			ctx := context.Background()
			cr := backupOVNCentral()
			tc.clear(cr)
			r := newTestOVNCentralReconciler(t, cr)

			res, err := r.reconcileBackup(ctx, r.Client, cr)

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(res.RequeueAfter).To(Equal(RequeueRaftWait))

			cond := ovnCentralCondition(cr, conditionTypeBackupReady)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForEndpoints))

			g.Expect(apierrors.IsNotFound(r.Get(ctx, centralKey("ovn-backup"), &batchv1.CronJob{}))).
				To(BeTrue(), "a CronJob applied here would schedule a run against the empty string")
			g.Expect(apierrors.IsNotFound(r.Get(ctx, centralKey("ovn-backup"), &corev1.PersistentVolumeClaim{}))).
				To(BeTrue(), "the volume waits with the CronJob: it binds to a node for a run that cannot happen yet")
		})
	}
}

// A suspended CronJob spawns no successor, so a failed run stays the newest
// terminal one for good. A JobFailed arm winning here would pin BackupReady
// False, and with it the aggregate Ready, until someone deleted the Job by hand.
func TestReconcileBackup_SuspendedOutranksFailure(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := backupOVNCentral()
	cr.Name = "backup-suspended"
	cr.Spec.Backup = &ovnv1alpha1.OVNBackupSpec{Suspend: true}
	registerBackupMetrics(t, cr)

	cronJob := seededBackupCronJob(cr)
	failed := backupRunJob(cr, cronJob, cr.Name+"-backup-28000000", batchv1.JobFailed, metav1.Now())
	r := newTestOVNCentralReconciler(t, cr, cronJob, failed)

	_, err := r.reconcileBackup(ctx, r.Client, cr)
	g.Expect(err).NotTo(HaveOccurred())

	cond := ovnCentralCondition(cr, conditionTypeBackupReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonBackupSuspended))
	g.Expect(cond.Message).To(ContainSubstring(failed.Name),
		"a pre-pause failure is still named, it is just not the state the CR is in")

	// The run failed whether or not the schedule is paused, so it is counted.
	metric := backupCounter(t, cr, "failed")
	g.Expect(metric).NotTo(BeNil())
	g.Expect(metric.GetCounter().GetValue()).To(Equal(1.0))
}

// TestReconcileBackup_FailedJobSetsConditionEventAndMetric covers the run-failure
// path: the CronJob itself applies cleanly, so a backup that stopped producing
// snapshots is only visible through the Job it spawned. The second pass is what
// pins the once-per-Job guarantee for both the event and the counter.
func TestReconcileBackup_FailedJobSetsConditionEventAndMetric(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := backupOVNCentral()
	cr.Name = "backup-failed-metric"
	registerBackupMetrics(t, cr)

	cronJob := seededBackupCronJob(cr)
	failed := backupRunJob(cr, cronJob, cr.Name+"-backup-28000000", batchv1.JobFailed, metav1.Now())
	r := newTestOVNCentralReconciler(t, cr, cronJob, failed)
	recorder, ok := r.Recorder.(*record.FakeRecorder)
	g.Expect(ok).To(BeTrue())

	_, err := r.reconcileBackup(ctx, r.Client, cr)
	g.Expect(err).NotTo(HaveOccurred(),
		"a failed run is a status signal, not a reconcile error: retrying the pass cannot fix it")

	cond := ovnCentralCondition(cr, conditionTypeBackupReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonBackupJobFailed))
	g.Expect(cond.Message).To(ContainSubstring(failed.Name))

	g.Expect(cr.Annotations).To(HaveKey(job.JobUIDAnnotationKey(componentBackup)),
		"the terminal metric must stamp the dedupe annotation so a run is counted once")

	// A second pass observes the same Job. Nothing new may come of it.
	_, err = r.reconcileBackup(ctx, r.Client, cr)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(recorder.Events).To(Receive(And(
		ContainSubstring(corev1.EventTypeWarning),
		ContainSubstring(conditionReasonBackupJobFailed),
		ContainSubstring(failed.Name),
	)))
	g.Expect(recorder.Events).NotTo(Receive(),
		"the Warning must fire once per Job, not once per pass")

	metric := backupCounter(t, cr, "failed")
	g.Expect(metric).NotTo(BeNil(), "a failed run must be counted as result=failed")
	g.Expect(metric.GetCounter().GetValue()).To(Equal(1.0),
		"two passes over one Job must produce one increment")
}

// The newest-wins rule: an older failed run must not hold the condition False
// once a later run has succeeded, or a single bad night would wedge the
// aggregate Ready until the CronJob's history limit pruned the failure away.
func TestReconcileBackup_SucceededJobKeepsScheduledAndRecordsSuccess(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := backupOVNCentral()
	cr.Name = "backup-succeeded-metric"
	registerBackupMetrics(t, cr)

	cronJob := seededBackupCronJob(cr)
	now := metav1.Now()
	older := backupRunJob(cr, cronJob, cr.Name+"-backup-28000000",
		batchv1.JobFailed, metav1.NewTime(now.Add(-24*time.Hour)))
	newer := backupRunJob(cr, cronJob, cr.Name+"-backup-28001440", batchv1.JobComplete, now)
	r := newTestOVNCentralReconciler(t, cr, cronJob, older, newer)

	_, err := r.reconcileBackup(ctx, r.Client, cr)
	g.Expect(err).NotTo(HaveOccurred())

	cond := ovnCentralCondition(cr, conditionTypeBackupReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonBackupScheduled))

	metric := backupCounter(t, cr, "succeeded")
	g.Expect(metric).NotTo(BeNil(), "the newest terminal run must be the one counted")
	g.Expect(metric.GetCounter().GetValue()).To(Equal(1.0))
	g.Expect(backupCounter(t, cr, "failed")).To(BeNil(),
		"the superseded older failure must not be counted")
}

// The reconcile lists Jobs by label alone, so metav1.IsControlledBy is the only
// thing separating this CronJob's runs from any other Job carrying the same
// component labels. `kubectl create job --from=cronjob/...` produces exactly
// such a Job, with the labels copied but no controller reference. Loosening the
// filter to "has any owner" would keep the rest of the suite green while a
// foreign Job flipped the condition and burned the once-per-UID dedupe
// annotation on a UID this operator never scheduled.
func TestReconcileBackup_IgnoresJobsItDoesNotControl(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := backupOVNCentral()

	cronJob := seededBackupCronJob(cr)
	foreign := backupRunJob(cr, cronJob, cr.Name+"-backup-manual", batchv1.JobFailed, metav1.Now())
	foreign.OwnerReferences = nil
	r := newTestOVNCentralReconciler(t, cr, cronJob, foreign)

	_, err := r.reconcileBackup(ctx, r.Client, cr)
	g.Expect(err).NotTo(HaveOccurred())

	cond := ovnCentralCondition(cr, conditionTypeBackupReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonBackupScheduled))
	g.Expect(cr.Annotations).NotTo(HaveKey(job.JobUIDAnnotationKey(componentBackup)),
		"a foreign run must not consume the dedupe annotation of a run this operator scheduled")
}

// A claim cannot shrink, so lowering spec.backup.storage.size makes the API
// server reject the apply as Invalid on every pass. Returning that error would
// put the CR in exponential backoff over a spec edit only a human can undo, so
// it is reported on the condition instead.
func TestReconcileBackup_PVCInvalidSurfacesConditionWithoutError(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := backupOVNCentral()

	c := ovnCentralFakeClientBuilder(t, cr).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(ctx context.Context, cl client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
				if kinded, ok := obj.(kindedApplyConfiguration); ok && kinded.GetKind() == "PersistentVolumeClaim" {
					return apierrors.NewInvalid(
						corev1.SchemeGroupVersion.WithKind("PersistentVolumeClaim").GroupKind(),
						"ovn-backup",
						field.ErrorList{field.Forbidden(
							field.NewPath("spec", "resources", "requests", "storage"),
							"field can not be less than previous value",
						)},
					)
				}
				return cl.Apply(ctx, obj, opts...)
			},
		}).Build()
	r := &OVNCentralReconciler{Client: c, Scheme: newTestScheme(t), Recorder: record.NewFakeRecorder(10)}

	res, err := r.reconcileBackup(ctx, r.Client, cr)

	g.Expect(err).NotTo(HaveOccurred(),
		"a rejected shrink is not transient: retrying it as an error only backs the whole CR off")
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))

	cond := ovnCentralCondition(cr, conditionTypeBackupReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonBackupPVCInvalid))
	g.Expect(cond.Message).To(ContainSubstring("can not be less than previous value"),
		"the API server's own message is what names the field to repair")

	g.Expect(apierrors.IsNotFound(r.Get(ctx, centralKey("ovn-backup"), &batchv1.CronJob{}))).To(BeTrue(),
		"a CronJob whose volume does not exist would fail every run it schedules")
}

// Any other apply failure is the operator's own problem (a target cluster that
// grants no persistentvolumeclaims verb lands here), so it is returned and the
// pipeline attributes it to this step.
func TestReconcileBackup_PVCForbiddenIsBackupError(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := backupOVNCentral()

	c := ovnCentralFakeClientBuilder(t, cr).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(ctx context.Context, cl client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
				if kinded, ok := obj.(kindedApplyConfiguration); ok && kinded.GetKind() == "PersistentVolumeClaim" {
					return apierrors.NewForbidden(corev1.Resource("persistentvolumeclaims"), "ovn-backup", nil)
				}
				return cl.Apply(ctx, obj, opts...)
			},
		}).Build()
	r := &OVNCentralReconciler{Client: c, Scheme: newTestScheme(t), Recorder: record.NewFakeRecorder(10)}

	_, err := r.reconcileBackup(ctx, r.Client, cr)

	g.Expect(err).To(MatchError(ContainSubstring("ensuring backup PVC")))
	g.Expect(apierrors.IsForbidden(err)).To(BeTrue(), "the API error must stay unwrappable")

	cond := ovnCentralCondition(cr, conditionTypeBackupReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonBackupError))
}

// The CronJob apply failing is the second half of the same story: the volume is
// already in, and the step still has to report rather than leave BackupReady
// stale-True at the new observedGeneration.
func TestReconcileBackup_CronJobApplyFailureIsBackupError(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := backupOVNCentral()

	c := ovnCentralFakeClientBuilder(t, cr).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(ctx context.Context, cl client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
				if kinded, ok := obj.(kindedApplyConfiguration); ok && kinded.GetKind() == "CronJob" {
					return apierrors.NewForbidden(batchv1.Resource("cronjobs"), "ovn-backup", nil)
				}
				return cl.Apply(ctx, obj, opts...)
			},
		}).Build()
	r := &OVNCentralReconciler{Client: c, Scheme: newTestScheme(t), Recorder: record.NewFakeRecorder(10)}

	_, err := r.reconcileBackup(ctx, r.Client, cr)

	g.Expect(err).To(MatchError(ContainSubstring("ensuring backup CronJob")))
	g.Expect(apierrors.IsForbidden(err)).To(BeTrue(), "the API error must stay unwrappable")

	g.Expect(r.Get(ctx, centralKey("ovn-backup"), &corev1.PersistentVolumeClaim{})).To(Succeed(),
		"the volume went in before the failure and stays: the next pass re-applies the CronJob")

	cond := ovnCentralCondition(cr, conditionTypeBackupReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonBackupError))
}
