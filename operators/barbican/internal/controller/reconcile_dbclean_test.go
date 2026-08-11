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
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/database"
	"github.com/c5c3/forge/internal/common/deployment"
	"github.com/c5c3/forge/internal/common/job"
	"github.com/c5c3/forge/internal/common/naming"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	barbicanv1alpha1 "github.com/c5c3/forge/operators/barbican/api/v1alpha1"
	barbicanmetrics "github.com/c5c3/forge/operators/barbican/internal/metrics"
)

// dbCleanConfigSecretName is the rendered-config Secret the clean-up CronJob
// mounts, as the Config step hands it to the parallel group.
const dbCleanConfigSecretName = "test-barbican-config-abc"

// dbCleanBarbican returns the shared test fixture with its schema already at the
// requested release. Every clean-up test below exercises the steady state, and
// the projected CronJob is suspended while a migration is in flight — an unset
// status.installedRelease is a fresh install, which is exactly such a window.
func dbCleanBarbican() *barbicanv1alpha1.Barbican {
	barbican := testBarbican()
	barbican.Status.InstalledRelease = barbican.Spec.OpenStackRelease
	return barbican
}

// dbCleanKey addresses the CronJob built for the shared test fixture.
var dbCleanKey = client.ObjectKey{Namespace: testNamespace, Name: testBarbicanName + dbCleanNameSuffix}

// seededDBCleanCronJob returns the clean-up CronJob carrying a stable UID, so a
// Job seeded with a controller reference to it is recognised by
// metav1.IsControlledBy. The fake client does not mint UIDs on create, so the
// CronJob has to be seeded rather than only created by the reconcile.
func seededDBCleanCronJob(barbican *barbicanv1alpha1.Barbican) *batchv1.CronJob {
	cronJob := dbCleanCronJob(barbican, dbCleanConfigSecretName)
	cronJob.UID = types.UID(barbican.Name + "-db-clean-uid")
	return cronJob
}

// dbCleanRunJob returns a Job as the CronJob controller would spawn it — the
// inherited commonLabels the reconcile lists by, plus the controller reference
// it filters on — in the given terminal state and with a stable UID for the
// terminal-metric dedupe.
func dbCleanRunJob(barbican *barbicanv1alpha1.Barbican, cronJob *batchv1.CronJob, name string,
	condition batchv1.JobConditionType, createdAt metav1.Time,
) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         barbican.Namespace,
			UID:               types.UID(name + "-uid"),
			CreationTimestamp: createdAt,
			Labels:            commonLabels(barbican),
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

func TestEffectiveDBClean(t *testing.T) {
	tests := []struct {
		name string
		in   *barbicanv1alpha1.DBCleanSpec
		want barbicanv1alpha1.DBCleanSpec
	}{
		{
			name: "nil block resolves every operator default",
			in:   nil,
			want: barbicanv1alpha1.DBCleanSpec{
				RetentionDays:             ptr.To(barbicanv1alpha1.DefaultDBCleanRetentionDays),
				Schedule:                  defaultDBCleanSchedule,
				SoftDeleteExpiredSecrets:  ptr.To(true),
				CleanUnassociatedProjects: ptr.To(false),
			},
		},
		{
			name: "empty block resolves every operator default",
			in:   &barbicanv1alpha1.DBCleanSpec{},
			want: barbicanv1alpha1.DBCleanSpec{
				RetentionDays:             ptr.To(barbicanv1alpha1.DefaultDBCleanRetentionDays),
				Schedule:                  defaultDBCleanSchedule,
				SoftDeleteExpiredSecrets:  ptr.To(true),
				CleanUnassociatedProjects: ptr.To(false),
			},
		},
		{
			name: "only the schedule set keeps the default retention",
			in:   &barbicanv1alpha1.DBCleanSpec{Schedule: "0 3 * * *"},
			want: barbicanv1alpha1.DBCleanSpec{
				RetentionDays:             ptr.To(barbicanv1alpha1.DefaultDBCleanRetentionDays),
				Schedule:                  "0 3 * * *",
				SoftDeleteExpiredSecrets:  ptr.To(true),
				CleanUnassociatedProjects: ptr.To(false),
			},
		},
		{
			name: "an explicit softDeleteExpiredSecrets=false stays false",
			in:   &barbicanv1alpha1.DBCleanSpec{SoftDeleteExpiredSecrets: ptr.To(false)},
			want: barbicanv1alpha1.DBCleanSpec{
				RetentionDays:             ptr.To(barbicanv1alpha1.DefaultDBCleanRetentionDays),
				Schedule:                  defaultDBCleanSchedule,
				SoftDeleteExpiredSecrets:  ptr.To(false),
				CleanUnassociatedProjects: ptr.To(false),
			},
		},
		{
			name: "a fully set block is passed through verbatim",
			in: &barbicanv1alpha1.DBCleanSpec{
				RetentionDays:             ptr.To(int32(1)),
				Schedule:                  "@weekly",
				SoftDeleteExpiredSecrets:  ptr.To(false),
				CleanUnassociatedProjects: ptr.To(true),
				Suspend:                   true,
			},
			want: barbicanv1alpha1.DBCleanSpec{
				RetentionDays:             ptr.To(int32(1)),
				Schedule:                  "@weekly",
				SoftDeleteExpiredSecrets:  ptr.To(false),
				CleanUnassociatedProjects: ptr.To(true),
				Suspend:                   true,
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			g.Expect(effectiveDBClean(tc.in)).To(Equal(tc.want))
		})
	}
}

// A nil spec.dbClean is not "no clean-up": barbican never hard-deletes on its
// own, so an unbounded soft-delete backlog is a deferred outage. The two inputs
// must therefore produce a byte-identical CronJob.
func TestDBCleanCronJob_NilBlockEqualsTheEmptyStruct(t *testing.T) {
	g := NewGomegaWithT(t)

	withNil := dbCleanBarbican()
	withNil.Spec.DBClean = nil
	withEmpty := dbCleanBarbican()
	withEmpty.Spec.DBClean = &barbicanv1alpha1.DBCleanSpec{}

	g.Expect(dbCleanCronJob(withNil, dbCleanConfigSecretName)).
		To(Equal(dbCleanCronJob(withEmpty, dbCleanConfigSecretName)))
}

func TestDBCleanCommand(t *testing.T) {
	tests := []struct {
		name string
		spec *barbicanv1alpha1.DBCleanSpec
		want []string
	}{
		{
			// The soft-delete pass is on by default, a deliberate deviation from
			// barbican's own CLI default: without it an expired secret's row is never
			// soft-deleted and therefore never reaches the retention window.
			name: "defaults sweep expired secrets but leave projects alone",
			spec: nil,
			want: []string{"barbican-manage", "db", "clean", "--min-days", "90", "--soft-delete-expired-secrets"},
		},
		{
			name: "the upstream default is reachable",
			spec: &barbicanv1alpha1.DBCleanSpec{SoftDeleteExpiredSecrets: ptr.To(false)},
			want: []string{"barbican-manage", "db", "clean", "--min-days", "90"},
		},
		{
			name: "the destructive project pass is opt-in",
			spec: &barbicanv1alpha1.DBCleanSpec{
				RetentionDays:             ptr.To(int32(7)),
				CleanUnassociatedProjects: ptr.To(true),
			},
			want: []string{
				"barbican-manage", "db", "clean", "--min-days", "7",
				"--soft-delete-expired-secrets", "--clean-unassociated-projects",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			g.Expect(dbCleanCommand(effectiveDBClean(tc.spec))).To(Equal(tc.want))
		})
	}
}

func TestReconcileDBClean_CreatesCronJobWithDefaults(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := dbCleanBarbican() // spec.dbClean is nil
	r := newBarbicanTestReconciler(barbican)

	res, err := r.reconcileDBClean(context.Background(), r.Client, barbican, dbCleanConfigSecretName)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue(), "the clean-up step never requeues on its own")

	var cronJob batchv1.CronJob
	g.Expect(r.Get(context.Background(), dbCleanKey, &cronJob)).To(Succeed())

	g.Expect(cronJob.Spec.Schedule).To(Equal("1 0 * * *"))
	g.Expect(cronJob.Spec.Suspend).To(Equal(ptr.To(false)), "spec.dbClean.suspend defaults to running")
	g.Expect(cronJob.Spec.ConcurrencyPolicy).To(Equal(batchv1.ForbidConcurrent),
		"a run that outlasts its interval must not be overtaken by the next firing")
	g.Expect(cronJob.Spec.JobTemplate.Spec.ActiveDeadlineSeconds).To(Equal(ptr.To(int64(3600))),
		"a wedged run must reach a terminal state, or DBCleanReady reports a clean-up that never happens")
	g.Expect(cronJob.OwnerReferences).To(HaveLen(1))
	g.Expect(cronJob.OwnerReferences[0].Name).To(Equal(testBarbicanName))

	// The spawned Jobs must be listable by label, so the common labels sit on the
	// CronJob and the JobTemplate alike. The pod template adds the component on
	// top, which is what keeps the clean-up pods out of the API Service.
	g.Expect(cronJob.Labels).To(Equal(commonLabels(barbican)))
	g.Expect(cronJob.Spec.JobTemplate.Labels).To(Equal(commonLabels(barbican)),
		"the JobTemplate must carry the labels reconcileDBClean lists runs by")
	podTemplate := cronJob.Spec.JobTemplate.Spec.Template
	g.Expect(podTemplate.Labels).To(Equal(componentLabels(barbican, dbCleanComponent)))
	g.Expect(podTemplate.Labels).To(HaveKeyWithValue(naming.LabelKeyComponent, "db-clean"))
	g.Expect(podTemplate.Spec.RestartPolicy).To(Equal(corev1.RestartPolicyOnFailure))

	g.Expect(podTemplate.Spec.Containers).To(HaveLen(1))
	container := podTemplate.Spec.Containers[0]
	g.Expect(container.Name).To(Equal("db-clean"))
	g.Expect(container.Image).To(Equal("ghcr.io/c5c3/barbican:2026.1"))
	g.Expect(container.SecurityContext).To(Equal(deployment.RestrictedSecurityContext()))
	g.Expect(container.Command).To(Equal([]string{
		"barbican-manage", "db", "clean", "--min-days", "90", "--soft-delete-expired-secrets",
	}))

	g.Expect(container.Env).To(HaveLen(1))
	g.Expect(container.Env[0].Name).To(Equal(database.ConnectionEnvVarName))
	g.Expect(container.VolumeMounts).To(Equal([]corev1.VolumeMount{
		{Name: configVolumeName, MountPath: barbicanConfigMountPath, ReadOnly: true},
	}))
	g.Expect(podTemplate.Spec.Volumes).To(HaveLen(1))
	g.Expect(podTemplate.Spec.Volumes[0].Secret.SecretName).To(Equal(dbCleanConfigSecretName))

	cond := barbicanCondition(barbican, conditionTypeDBCleanReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonDBCleanScheduled))
	g.Expect(cond.Message).To(ContainSubstring("90 days"))
}

func TestReconcileDBClean_HonorsSpecKnobs(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := dbCleanBarbican()
	barbican.Spec.DBClean = &barbicanv1alpha1.DBCleanSpec{
		RetentionDays:             ptr.To(int32(7)),
		Schedule:                  "0 3 * * *",
		CleanUnassociatedProjects: ptr.To(true),
		Suspend:                   true,
	}
	r := newBarbicanTestReconciler(barbican)

	_, err := r.reconcileDBClean(context.Background(), r.Client, barbican, dbCleanConfigSecretName)
	g.Expect(err).NotTo(HaveOccurred())

	var cronJob batchv1.CronJob
	g.Expect(r.Get(context.Background(), dbCleanKey, &cronJob)).To(Succeed())

	g.Expect(cronJob.Spec.Schedule).To(Equal("0 3 * * *"))
	g.Expect(cronJob.Spec.Suspend).To(Equal(ptr.To(true)))
	g.Expect(cronJob.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Command).To(Equal([]string{
		"barbican-manage", "db", "clean", "--min-days", "7",
		"--soft-delete-expired-secrets", "--clean-unassociated-projects",
	}))

	cond := barbicanCondition(barbican, conditionTypeDBCleanReady)
	g.Expect(cond.Message).To(ContainSubstring(`"0 3 * * *"`))
	g.Expect(cond.Message).To(ContainSubstring("7 days"))
}

// A suspended clean-up stays True — pausing it is a deliberate posture, not a
// failure — but it must say so. The CronJob will never fire again, so reporting
// the scheduled message would assert active hard-deletion that is not happening,
// and nothing else surfaces the drift: no run fails, so no Warning event fires,
// and barbican_operator_db_clean_total merely stops incrementing.
func TestReconcileDBClean_SuspendedReportsItsOwnReason(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := dbCleanBarbican()
	barbican.Spec.DBClean = &barbicanv1alpha1.DBCleanSpec{Suspend: true}
	r := newBarbicanTestReconciler(barbican)

	_, err := r.reconcileDBClean(context.Background(), r.Client, barbican, dbCleanConfigSecretName)
	g.Expect(err).NotTo(HaveOccurred())

	cond := barbicanCondition(barbican, conditionTypeDBCleanReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue),
		"a paused clean-up is a posture, not a failure")
	g.Expect(cond.Reason).To(Equal(conditionReasonDBCleanSuspended))
	g.Expect(cond.Message).To(ContainSubstring("suspended"))
	g.Expect(cond.Message).NotTo(ContainSubstring("Database clean-up scheduled"),
		"the scheduled message asserts hard-deletion a suspended CronJob is not doing")
}

// A schema migration in flight must pause the schedule. barbican-manage db
// upgrade holds DDL locks the clean-up's bulk DELETEs on the same tables contend
// with, ConcurrencyPolicy only guards a clean-up against a clean-up, and the
// CronJob controller fires independently of the reconcile pipeline — so the
// suspension has to be projected, not merely intended.
func TestReconcileDBClean_MigrationInFlightSuspendsTheSchedule(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := dbCleanBarbican()
	barbican.Status.InstalledRelease = "2025.2" // spec asks for 2026.1
	r := newBarbicanTestReconciler(barbican)

	_, err := r.reconcileDBClean(context.Background(), r.Client, barbican, dbCleanConfigSecretName)
	g.Expect(err).NotTo(HaveOccurred())

	var cronJob batchv1.CronJob
	g.Expect(r.Get(context.Background(), dbCleanKey, &cronJob)).To(Succeed())
	g.Expect(cronJob.Spec.Suspend).To(Equal(ptr.To(true)),
		"the clean-up must not fire while the schema migrates")

	cond := barbicanCondition(barbican, conditionTypeDBCleanReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue), "a paused clean-up is a posture, not a failure")
	g.Expect(cond.Reason).To(Equal(conditionReasonDBCleanSuspended))
	g.Expect(cond.Message).To(ContainSubstring("2025.2"))
	g.Expect(cond.Message).To(ContainSubstring("2026.1"))
}

// A migration pause is only a posture while the convergence can still happen.
// Every release-gate rejection leaves status.installedRelease apart from the
// spec forever with no Job running, so the pause it holds open is unbounded: the
// clean-up never fires again and the soft-deleted backlog grows without limit.
// Reporting True there would hide exactly that behind the one condition meant to
// report clean-up health.
func TestReconcileDBClean_WedgedReleaseGateReportsTheBlockedPause(t *testing.T) {
	for _, reason := range []string{
		database.ReasonDowngradeNotSupported,
		database.ReasonUpgradePathInvalid,
		database.ReasonVersionParseError,
		conditionReasonImageReleaseMismatch,
	} {
		t.Run(reason, func(t *testing.T) {
			g := NewGomegaWithT(t)
			barbican := dbCleanBarbican()
			barbican.Status.InstalledRelease = "2025.2" // spec asks for 2026.1
			conditions.SetCondition(&barbican.Status.Conditions, metav1.Condition{
				Type:   "DatabaseReady",
				Status: metav1.ConditionFalse,
				Reason: reason,
			})
			r := newBarbicanTestReconciler(barbican)

			_, err := r.reconcileDBClean(context.Background(), r.Client, barbican, dbCleanConfigSecretName)
			g.Expect(err).NotTo(HaveOccurred())

			cond := barbicanCondition(barbican, conditionTypeDBCleanReady)
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse),
				"a pause that cannot end on its own is not a posture")
			g.Expect(cond.Reason).To(Equal(conditionReasonDBCleanBlocked))
			g.Expect(cond.Message).To(ContainSubstring(reason))
		})
	}
}

// A migration that is genuinely running keeps the pause reported as the posture
// it is — the sync Job advances the installed release and the schedule resumes
// behind it.
func TestReconcileDBClean_SyncInProgressKeepsThePauseATrue(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := dbCleanBarbican()
	barbican.Status.InstalledRelease = "2025.2"
	conditions.SetCondition(&barbican.Status.Conditions, metav1.Condition{
		Type:   "DatabaseReady",
		Status: metav1.ConditionFalse,
		Reason: database.ReasonDBSyncInProgress,
	})
	r := newBarbicanTestReconciler(barbican)

	_, err := r.reconcileDBClean(context.Background(), r.Client, barbican, dbCleanConfigSecretName)
	g.Expect(err).NotTo(HaveOccurred())

	cond := barbicanCondition(barbican, conditionTypeDBCleanReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonDBCleanSuspended))
}

// The clean-up step runs ahead of the Database step, so it is reached before a
// rendered config exists. An empty Secret name would build a volume the API
// server rejects outright, so the step reports the wait instead.
func TestReconcileDBClean_WithoutRenderedConfigSchedulesNothing(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := dbCleanBarbican()
	r := newBarbicanTestReconciler(barbican)

	res, err := r.reconcileDBClean(context.Background(), r.Client, barbican, "")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())

	var cronJob batchv1.CronJob
	g.Expect(r.Get(context.Background(), dbCleanKey, &cronJob)).NotTo(Succeed(),
		"no CronJob is projected against a config Secret that does not exist yet")

	cond := barbicanCondition(barbican, conditionTypeDBCleanReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForSecretStores))
}

// A clean-up suspended after a failed run must still report DBCleanSuspended.
// The failed Job stays the newest terminal one for good — a suspended CronJob
// spawns no successor to supersede it, and the default failedJobsHistoryLimit
// retains it — so a JobFailed arm that outranked suspension would pin
// DBCleanReady False forever, holding the aggregate Ready down until someone
// deleted the Job by hand.
func TestReconcileDBClean_SuspendedAfterAFailedRunStaysTrue(t *testing.T) {
	g := NewGomegaWithT(t)
	g.Expect(barbicanmetrics.Register()).To(Succeed())

	barbican := dbCleanBarbican()
	barbican.Name = "clean-suspended-after-failure"
	barbican.Spec.DBClean = &barbicanv1alpha1.DBCleanSpec{Suspend: true}
	t.Cleanup(func() { barbicanmetrics.DeleteForBarbican(barbican.Name, barbican.Namespace) })

	cronJob := seededDBCleanCronJob(barbican)
	failed := dbCleanRunJob(barbican, cronJob, barbican.Name+"-db-clean-28000000",
		batchv1.JobFailed, metav1.Now())
	r := newBarbicanTestReconciler(barbican, cronJob, failed)
	recorder, ok := r.Recorder.(*record.FakeRecorder)
	g.Expect(ok).To(BeTrue())

	_, err := r.reconcileDBClean(context.Background(), r.Client, barbican, dbCleanConfigSecretName)
	g.Expect(err).NotTo(HaveOccurred())

	cond := barbicanCondition(barbican, conditionTypeDBCleanReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue),
		"no successor run can ever clear the failure, so False here would never lift")
	g.Expect(cond.Reason).To(Equal(conditionReasonDBCleanSuspended))
	g.Expect(cond.Message).To(ContainSubstring(failed.Name),
		"the pre-pause failure is still worth naming, it is just not the state the CR is in")
	g.Expect(recorder.Events).NotTo(Receive(),
		"a suspended clean-up raises no event; re-firing the pre-pause failure on every pass would be noise")
}

// TestReconcileDBClean_FailedJobSetsConditionEventAndMetric covers the
// run-failure path: the CronJob itself applies cleanly, so a clean-up that stops
// working is only visible through the Job it spawned.
func TestReconcileDBClean_FailedJobSetsConditionEventAndMetric(t *testing.T) {
	g := NewGomegaWithT(t)
	g.Expect(barbicanmetrics.Register()).To(Succeed())

	barbican := dbCleanBarbican()
	barbican.Name = "clean-failed-metric"
	t.Cleanup(func() { barbicanmetrics.DeleteForBarbican(barbican.Name, barbican.Namespace) })

	cronJob := seededDBCleanCronJob(barbican)
	failed := dbCleanRunJob(barbican, cronJob, barbican.Name+"-db-clean-28000000",
		batchv1.JobFailed, metav1.Now())
	r := newBarbicanTestReconciler(barbican, cronJob, failed)
	recorder, ok := r.Recorder.(*record.FakeRecorder)
	g.Expect(ok).To(BeTrue())

	_, err := r.reconcileDBClean(context.Background(), r.Client, barbican, dbCleanConfigSecretName)
	g.Expect(err).NotTo(HaveOccurred(),
		"a failed run is a status signal, not a reconcile error: retrying the pass cannot fix it")

	cond := barbicanCondition(barbican, conditionTypeDBCleanReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonDBCleanJobFailed))
	g.Expect(cond.Message).To(ContainSubstring(failed.Name))

	g.Expect(recorder.Events).To(Receive(And(
		ContainSubstring(corev1.EventTypeWarning),
		ContainSubstring(conditionReasonDBCleanJobFailed),
		ContainSubstring(failed.Name),
	)))

	g.Expect(barbican.Annotations).To(HaveKey(job.JobUIDAnnotationKey(dbCleanComponent)),
		"the terminal metric must stamp the dedupe annotation so a run is counted once")
	g.Expect(counterValueForLabels(t, ctrlmetrics.Registry, "barbican_operator_db_clean_total",
		map[string]string{"barbican": barbican.Name, "namespace": barbican.Namespace, "result": "failed"})).
		To(Equal(1.0), "a failed run must be counted as result=failed")
}

// TestReconcileDBClean_SucceededJobKeepsConditionTrue pins the newest-wins rule:
// an older failed run must not hold the condition False once a later run has
// succeeded, or a single bad night would wedge Ready until the CronJob's history
// limit pruned the failure away.
func TestReconcileDBClean_SucceededJobKeepsConditionTrue(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := dbCleanBarbican()

	cronJob := seededDBCleanCronJob(barbican)
	now := metav1.Now()
	older := dbCleanRunJob(barbican, cronJob, barbican.Name+"-db-clean-28000000",
		batchv1.JobFailed, metav1.NewTime(now.Add(-24*time.Hour)))
	newer := dbCleanRunJob(barbican, cronJob, barbican.Name+"-db-clean-28001440",
		batchv1.JobComplete, now)
	r := newBarbicanTestReconciler(barbican, cronJob, older, newer)

	_, err := r.reconcileDBClean(context.Background(), r.Client, barbican, dbCleanConfigSecretName)
	g.Expect(err).NotTo(HaveOccurred())

	cond := barbicanCondition(barbican, conditionTypeDBCleanReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonDBCleanScheduled))
}

// The reconcile lists Jobs by label alone, so metav1.IsControlledBy is the only
// thing separating this CronJob's runs from any other Job carrying the same
// commonLabels — and `kubectl create job --from=cronjob/...` produces exactly
// such a Job, with the labels copied but no controller reference. Without this
// case, loosening the filter to "has any owner" would keep the suite green while
// a foreign Job flipped the condition and burned the once-per-UID dedupe
// annotation on a UID this operator never scheduled.
func TestReconcileDBClean_IgnoresJobsItDoesNotControl(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := dbCleanBarbican()

	cronJob := seededDBCleanCronJob(barbican)
	foreign := dbCleanRunJob(barbican, cronJob, barbican.Name+"-db-clean-manual",
		batchv1.JobFailed, metav1.Now())
	foreign.OwnerReferences = nil
	r := newBarbicanTestReconciler(barbican, cronJob, foreign)

	_, err := r.reconcileDBClean(context.Background(), r.Client, barbican, dbCleanConfigSecretName)
	g.Expect(err).NotTo(HaveOccurred())

	cond := barbicanCondition(barbican, conditionTypeDBCleanReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonDBCleanScheduled))
	g.Expect(barbican.Annotations).NotTo(HaveKey(job.JobUIDAnnotationKey(dbCleanComponent)),
		"a foreign run must not consume the dedupe annotation of a run this operator scheduled")
}

// A run that has not reached a terminal condition is not an outcome to report on
// — the condition keeps describing the schedule until the run finishes.
// activeDeadlineSeconds is what bounds how long that can last: it turns a wedged
// run into a JobFailed, which the failure path above then surfaces.
func TestReconcileDBClean_RunningJobLeavesConditionOnTheSchedule(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := dbCleanBarbican()

	cronJob := seededDBCleanCronJob(barbican)
	running := dbCleanRunJob(barbican, cronJob, barbican.Name+"-db-clean-28000000",
		batchv1.JobFailed, metav1.Now())
	running.Status.Conditions = nil
	r := newBarbicanTestReconciler(barbican, cronJob, running)

	_, err := r.reconcileDBClean(context.Background(), r.Client, barbican, dbCleanConfigSecretName)
	g.Expect(err).NotTo(HaveOccurred())

	cond := barbicanCondition(barbican, conditionTypeDBCleanReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonDBCleanScheduled))
	g.Expect(barbican.Annotations).NotTo(HaveKey(job.JobUIDAnnotationKey(dbCleanComponent)),
		"an unfinished run has no terminal state to count")
}

// The clean-up pod injects the same DSN the API pods use; when database TLS is on
// that DSN names ssl_ca/ssl_cert/ssl_key paths under the db-tls mount, so a run
// without the mount fails to open them every single time.
func TestReconcileDBClean_ProjectsDBTLSMaterialWhenEnabled(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := dbCleanBarbican()
	barbican.Spec.Database.TLS = &commonv1.DatabaseTLSSpec{
		Mode:                "verify-full",
		CABundleSecretRef:   commonv1.SecretRefSpec{Name: "barbican-db-ca"},
		ClientCertSecretRef: commonv1.SecretRefSpec{Name: "barbican-db-client"},
	}
	r := newBarbicanTestReconciler(barbican)

	_, err := r.reconcileDBClean(context.Background(), r.Client, barbican, dbCleanConfigSecretName)
	g.Expect(err).NotTo(HaveOccurred())

	var cronJob batchv1.CronJob
	g.Expect(r.Get(context.Background(), dbCleanKey, &cronJob)).To(Succeed())

	tlsVol, tlsMount := barbicanDBTLSVolumeAndMount(barbican)
	podSpec := cronJob.Spec.JobTemplate.Spec.Template.Spec
	g.Expect(podSpec.Volumes).To(ContainElement(tlsVol))
	g.Expect(podSpec.Containers[0].VolumeMounts).To(ContainElement(tlsMount))
}

// dbCleanCronJobName has to be total. The metadata.name bound that keeps
// "{name}-db-clean" inside the CronJob cap is enforced on create only — and can
// only be, since metadata.name is immutable and rejecting the finalizer-removal
// update would wedge the CR in Terminating — so an operator upgrade inherits
// Barbican CRs the bound would reject. Building their name unconditionally would
// fail every reconcile on an apply the API server refuses, with nothing left to
// edit to repair it.
func TestDBCleanCronJobName(t *testing.T) {
	g := NewGomegaWithT(t)

	atLimit := strings.Repeat("b", barbicanv1alpha1.MaxBarbicanNameLength)
	g.Expect(dbCleanCronJobName(atLimit)).To(Equal(atLimit+dbCleanNameSuffix),
		"an admissible name keeps the documented {name}-db-clean form")

	// Sweep every truncation offset against both characters a DNS label may not
	// end on, so the collapsed name is applicable wherever the boundary falls.
	for _, sep := range []string{"-", "."} {
		for i := 1; i < barbicanv1alpha1.MaxBarbicanNameLength; i++ {
			name := strings.Repeat("b", i) + sep + strings.Repeat("c", barbicanv1alpha1.MaxBarbicanNameLength)
			got := dbCleanCronJobName(name)
			g.Expect(len(got)).To(BeNumerically("<=", barbicanv1alpha1.MaxCronJobNameLength), name)
			g.Expect(got).To(HaveSuffix(dbCleanNameSuffix), name)
			g.Expect(utilvalidation.IsDNS1123Subdomain(got)).To(BeEmpty(), name)
			g.Expect(dbCleanCronJobName(name)).To(Equal(got),
				"the name must be stable across passes, or every reconcile orphans the last CronJob")
		}
	}

	// Two CRs sharing the truncated prefix must not collapse onto one CronJob.
	shared := strings.Repeat("b", barbicanv1alpha1.MaxBarbicanNameLength)
	g.Expect(dbCleanCronJobName(shared + "-alpha")).NotTo(Equal(dbCleanCronJobName(shared + "-beta")))
}

// The reconcile itself must go through for such a CR: the CronJob is applied
// under the collapsed name and DBCleanReady is reported like any other Barbican.
func TestReconcileDBClean_AppliesForAnOverlongBarbicanName(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := dbCleanBarbican()
	barbican.Name = strings.Repeat("b", barbicanv1alpha1.MaxBarbicanNameLength+20)
	r := newBarbicanTestReconciler(barbican)

	_, err := r.reconcileDBClean(context.Background(), r.Client, barbican, dbCleanConfigSecretName)
	g.Expect(err).NotTo(HaveOccurred())

	var cronJob batchv1.CronJob
	g.Expect(r.Get(context.Background(), client.ObjectKey{
		Name: dbCleanCronJobName(barbican.Name), Namespace: barbican.Namespace,
	}, &cronJob)).To(Succeed())

	cond := barbicanCondition(barbican, conditionTypeDBCleanReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
}
