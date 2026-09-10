// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	dto "github.com/prometheus/client_model/go"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/c5c3/cobaltcore/internal/common/deployment"
	"github.com/c5c3/cobaltcore/internal/common/job"
	"github.com/c5c3/cobaltcore/internal/common/naming"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	cinderv1alpha1 "github.com/c5c3/cobaltcore/operators/cinder/api/v1alpha1"
	"github.com/c5c3/cobaltcore/operators/cinder/internal/metrics"
)

// seededDBPurgeCronJob returns the purge CronJob carrying a stable UID, so a Job
// seeded with a controller reference to it is recognised by
// metav1.IsControlledBy. The fake client does not mint UIDs on create, so the
// CronJob has to be seeded rather than only created by the reconcile.
func seededDBPurgeCronJob(cinder *cinderv1alpha1.Cinder) *batchv1.CronJob {
	cronJob := dbPurgeCronJob(cinder, workloadArtifacts())
	cronJob.UID = types.UID(cinder.Name + "-db-purge-uid")
	return cronJob
}

// dbPurgeRunJob returns a Job as the CronJob controller would spawn it — the
// inherited commonLabels the reconcile lists by, plus the controller reference
// it filters on — in the given terminal state and with a stable UID for the
// terminal-metric dedupe.
func dbPurgeRunJob(cinder *cinderv1alpha1.Cinder, cronJob *batchv1.CronJob, name string,
	condition batchv1.JobConditionType, createdAt metav1.Time,
) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         cinder.Namespace,
			UID:               types.UID(name + "-uid"),
			CreationTimestamp: createdAt,
			Labels:            commonLabels(cinder),
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

// dbPurgeCount reads the db-purge counter for one CR and terminal result off the
// controller-runtime registry, or 0 when no such series exists yet.
func dbPurgeCount(t *testing.T, cinder *cinderv1alpha1.Cinder, result string) float64 {
	t.Helper()
	families, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gathering the metrics registry: %v", err)
	}
	want := map[string]string{"cinder": cinder.Name, "namespace": cinder.Namespace, "result": result}
	for _, family := range families {
		if family.GetName() != "cinder_operator_db_purge_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			if dbPurgeLabelsMatch(metric.GetLabel(), want) {
				return metric.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func dbPurgeLabelsMatch(got []*dto.LabelPair, want map[string]string) bool {
	if len(got) != len(want) {
		return false
	}
	for _, pair := range got {
		if want[pair.GetName()] != pair.GetValue() {
			return false
		}
	}
	return true
}

func TestEffectiveDBPurge(t *testing.T) {
	tests := []struct {
		name string
		in   *cinderv1alpha1.DBPurgeSpec
		want cinderv1alpha1.DBPurgeSpec
	}{
		{
			name: "nil block resolves every operator default",
			in:   nil,
			want: cinderv1alpha1.DBPurgeSpec{
				RetentionDays: ptr.To(cinderv1alpha1.DefaultDBPurgeRetentionDays),
				Schedule:      cinderv1alpha1.DefaultDBPurgeSchedule,
			},
		},
		{
			name: "empty block resolves every operator default",
			in:   &cinderv1alpha1.DBPurgeSpec{},
			want: cinderv1alpha1.DBPurgeSpec{
				RetentionDays: ptr.To(cinderv1alpha1.DefaultDBPurgeRetentionDays),
				Schedule:      cinderv1alpha1.DefaultDBPurgeSchedule,
			},
		},
		{
			name: "only the schedule set keeps the default retention",
			in:   &cinderv1alpha1.DBPurgeSpec{Schedule: "0 3 * * *"},
			want: cinderv1alpha1.DBPurgeSpec{
				RetentionDays: ptr.To(cinderv1alpha1.DefaultDBPurgeRetentionDays),
				Schedule:      "0 3 * * *",
			},
		},
		{
			name: "only the retention set keeps the default schedule",
			in:   &cinderv1alpha1.DBPurgeSpec{RetentionDays: ptr.To(int32(7))},
			want: cinderv1alpha1.DBPurgeSpec{
				RetentionDays: ptr.To(int32(7)),
				Schedule:      cinderv1alpha1.DefaultDBPurgeSchedule,
			},
		},
		{
			name: "a fully set block is passed through verbatim",
			in: &cinderv1alpha1.DBPurgeSpec{
				RetentionDays: ptr.To(int32(1)),
				Schedule:      "@weekly",
				Suspend:       true,
			},
			want: cinderv1alpha1.DBPurgeSpec{
				RetentionDays: ptr.To(int32(1)),
				Schedule:      "@weekly",
				Suspend:       true,
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			g.Expect(effectiveDBPurge(tc.in)).To(Equal(tc.want))
		})
	}

	t.Run("nil block is indistinguishable from an empty one", func(t *testing.T) {
		g := NewGomegaWithT(t)
		g.Expect(effectiveDBPurge(nil)).To(Equal(effectiveDBPurge(&cinderv1alpha1.DBPurgeSpec{})))
	})
}

func TestReconcileDBPurge_CreatesCronJobWithDefaults(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder() // spec.dbPurge is nil
	r := newCinderTestReconciler(cinder)

	res, err := r.reconcileDBPurge(context.Background(), r.Client, cinder, workloadArtifacts())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue(), "the purge step never requeues on its own")

	var cronJob batchv1.CronJob
	g.Expect(r.Get(context.Background(), objectKey(testCinderName+"-db-purge"), &cronJob)).To(Succeed())

	g.Expect(cronJob.Spec.Schedule).To(Equal("1 0 * * *"))
	g.Expect(cronJob.Spec.Suspend).To(Equal(ptr.To(false)), "spec.dbPurge.suspend defaults to running")
	g.Expect(cronJob.Spec.ConcurrencyPolicy).To(Equal(batchv1.ForbidConcurrent),
		"a run that outlasts its interval must not be overtaken by the next firing")
	g.Expect(cronJob.Spec.JobTemplate.Spec.ActiveDeadlineSeconds).To(Equal(ptr.To(int64(3600))),
		"a wedged run must reach a terminal state, or DBPurgeReady reports a purge that never happens")
	g.Expect(cronJob.OwnerReferences).To(HaveLen(1))
	g.Expect(cronJob.OwnerReferences[0].Name).To(Equal(testCinderName))

	// The spawned Jobs must be listable by label, so the common labels sit on the
	// CronJob and the JobTemplate alike. The pod template adds the component on
	// top, which is what keeps the purge pods out of the API Service.
	g.Expect(cronJob.Labels).To(Equal(commonLabels(cinder)))
	g.Expect(cronJob.Spec.JobTemplate.Labels).To(Equal(commonLabels(cinder)),
		"the JobTemplate must carry the labels reconcileDBPurge lists runs by")
	podTemplate := cronJob.Spec.JobTemplate.Spec.Template
	g.Expect(podTemplate.Labels).To(Equal(componentLabels(cinder, "db-purge")))
	g.Expect(podTemplate.Spec.RestartPolicy).To(Equal(corev1.RestartPolicyOnFailure))

	g.Expect(podTemplate.Spec.Containers).To(HaveLen(1))
	container := podTemplate.Spec.Containers[0]
	g.Expect(container.Name).To(Equal("db-purge"))
	g.Expect(container.Image).To(Equal("ghcr.io/c5c3/cinder:2026.1"))
	g.Expect(container.SecurityContext).To(Equal(deployment.RestrictedSecurityContext()))
	// Pinned verbatim: the age is a positional argument, so a stray flag or a
	// reordering changes which rows are deleted rather than failing the run.
	g.Expect(container.Command).To(Equal([]string{
		"cinder-manage", "--config-dir", cinderConfigDir, "db", "purge", "30",
	}))
	g.Expect(container.Env).To(Equal(cinderWorkloadEnv(cinder)),
		"the purge reads the database URL from the same env override the workloads use")

	// The config volume selects its keys, so scheduler.conf — the scheduler's own
	// host identity — is left out of the purge pod like it is out of every other.
	configVol, configMount := configVolumeAndMount(workloadArtifacts())
	g.Expect(podTemplate.Spec.Volumes).To(Equal([]corev1.Volume{configVol}))
	g.Expect(container.VolumeMounts).To(Equal([]corev1.VolumeMount{configMount}))
	for _, item := range podTemplate.Spec.Volumes[0].ConfigMap.Items {
		g.Expect(item.Key).NotTo(Equal(schedulerConfDataKey))
	}

	cond := cinderCondition(cinder, conditionTypeDBPurgeReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonDBPurgeScheduled))
	g.Expect(cond.Message).To(ContainSubstring("30 days"))
}

func TestReconcileDBPurge_HonorsSpecKnobs(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	cinder.Spec.DBPurge = &cinderv1alpha1.DBPurgeSpec{
		RetentionDays: ptr.To(int32(7)),
		Schedule:      "0 3 * * *",
	}
	r := newCinderTestReconciler(cinder)

	_, err := r.reconcileDBPurge(context.Background(), r.Client, cinder, workloadArtifacts())
	g.Expect(err).NotTo(HaveOccurred())

	var cronJob batchv1.CronJob
	g.Expect(r.Get(context.Background(), objectKey(testCinderName+"-db-purge"), &cronJob)).To(Succeed())

	g.Expect(cronJob.Spec.Schedule).To(Equal("0 3 * * *"))
	g.Expect(cronJob.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Command).To(Equal([]string{
		"cinder-manage", "--config-dir", cinderConfigDir, "db", "purge", "7",
	}))

	cond := cinderCondition(cinder, conditionTypeDBPurgeReady)
	g.Expect(cond.Message).To(ContainSubstring(`"0 3 * * *"`))
	g.Expect(cond.Message).To(ContainSubstring("7 days"))
}

// The purge pods must not become endpoints of the API Service: they carry no
// readiness probe, so a scheduled run would turn a pod with nothing listening on
// 8776 into a ready backend the Service routes API requests to.
func TestDBPurgePodsNotSelectedByAPIService(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := workloadCinder()

	selector := labels.SelectorFromSet(buildCinderService(cinder).Spec.Selector)

	apiPodLabels := buildCinderDeployment(cinder, workloadArtifacts(), workloadDigests{}).Spec.Template.Labels
	g.Expect(selector.Matches(labels.Set(apiPodLabels))).To(BeTrue(),
		"the API pod template must satisfy the API Service selector")

	purgePodLabels := dbPurgeCronJob(cinder, workloadArtifacts()).Spec.JobTemplate.Spec.Template.Labels
	g.Expect(purgePodLabels).To(HaveKeyWithValue(naming.LabelKeyComponent, "db-purge"))
	g.Expect(selector.Matches(labels.Set(purgePodLabels))).To(BeFalse(),
		"db-purge pods must never become endpoints of the API Service")
}

// A suspended purge stays True — pausing it is a deliberate posture, not a
// failure — but it must say so. The CronJob will never fire again, so reporting
// the scheduled message would assert active hard-deletion that is not happening.
func TestReconcileDBPurge_SuspendedReportsItsOwnReason(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	cinder.Spec.DBPurge = &cinderv1alpha1.DBPurgeSpec{Suspend: true}
	r := newCinderTestReconciler(cinder)

	_, err := r.reconcileDBPurge(context.Background(), r.Client, cinder, workloadArtifacts())
	g.Expect(err).NotTo(HaveOccurred())

	var cronJob batchv1.CronJob
	g.Expect(r.Get(context.Background(), objectKey(testCinderName+"-db-purge"), &cronJob)).To(Succeed())
	g.Expect(cronJob.Spec.Suspend).To(Equal(ptr.To(true)))

	cond := cinderCondition(cinder, conditionTypeDBPurgeReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue), "a paused purge is a posture, not a failure")
	g.Expect(cond.Reason).To(Equal(conditionReasonDBPurgeSuspended))
	g.Expect(cond.Message).To(ContainSubstring("suspended"))
	g.Expect(cond.Message).NotTo(ContainSubstring("Database purge scheduled"),
		"the scheduled message asserts hard-deletion a suspended CronJob is not doing")
}

// A purge suspended after a failed run must still report DBPurgeSuspended. The
// failed Job stays the newest terminal one for good — a suspended CronJob spawns
// no successor to supersede it — so a JobFailed arm that outranked suspension
// would pin DBPurgeReady False until someone deleted the Job by hand.
func TestReconcileDBPurge_SuspendedAfterAFailedRunStaysTrue(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	cinder.Spec.DBPurge = &cinderv1alpha1.DBPurgeSpec{Suspend: true}

	cronJob := seededDBPurgeCronJob(cinder)
	failed := dbPurgeRunJob(cinder, cronJob, cinder.Name+"-db-purge-28000000", batchv1.JobFailed, metav1.Now())
	r := newCinderTestReconciler(cinder, cronJob, failed)
	recorder, ok := r.Recorder.(*record.FakeRecorder)
	g.Expect(ok).To(BeTrue())

	_, err := r.reconcileDBPurge(context.Background(), r.Client, cinder, workloadArtifacts())
	g.Expect(err).NotTo(HaveOccurred())

	cond := cinderCondition(cinder, conditionTypeDBPurgeReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue),
		"no successor run can ever clear the failure, so False here would never lift")
	g.Expect(cond.Reason).To(Equal(conditionReasonDBPurgeSuspended))
	g.Expect(cond.Message).To(ContainSubstring(failed.Name),
		"the pre-pause failure is still worth naming, it is just not the state the CR is in")
	g.Expect(recorder.Events).NotTo(Receive(),
		"a suspended purge raises no event; re-firing the pre-pause failure on every pass would be noise")
}

// The CronJob itself applies cleanly whether or not the purge works, so a purge
// that stopped working is only visible through the Job it spawned.
func TestReconcileDBPurge_FailedJobSetsConditionEventAndMetric(t *testing.T) {
	g := NewGomegaWithT(t)
	g.Expect(metrics.Register()).To(Succeed())

	cinder := validCinder()
	cinder.Name = "purge-failed-metric"
	t.Cleanup(func() { metrics.DeleteForCinder(cinder.Name, cinder.Namespace) })

	cronJob := seededDBPurgeCronJob(cinder)
	failed := dbPurgeRunJob(cinder, cronJob, cinder.Name+"-db-purge-28000000", batchv1.JobFailed, metav1.Now())
	r := newCinderTestReconciler(cinder, cronJob, failed)
	recorder, ok := r.Recorder.(*record.FakeRecorder)
	g.Expect(ok).To(BeTrue())

	_, err := r.reconcileDBPurge(context.Background(), r.Client, cinder, workloadArtifacts())
	g.Expect(err).NotTo(HaveOccurred(),
		"a failed run is a status signal, not a reconcile error: retrying the pass cannot fix it")

	cond := cinderCondition(cinder, conditionTypeDBPurgeReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonDBPurgeJobFailed))
	g.Expect(cond.Message).To(ContainSubstring(failed.Name))

	g.Expect(recorder.Events).To(Receive(And(
		ContainSubstring(corev1.EventTypeWarning),
		ContainSubstring(conditionReasonDBPurgeJobFailed),
		ContainSubstring(failed.Name),
	)))

	g.Expect(cinder.Annotations).To(HaveKeyWithValue(job.JobUIDAnnotationKey("db-purge"), string(failed.UID)),
		"the terminal metric must stamp the dedupe annotation so a run is counted once")
	g.Expect(dbPurgeCount(t, cinder, "failed")).To(Equal(1.0), "a failed run must be counted as result=failed")

	// A second pass observes the same run and must not count it twice: the
	// counter is what a rate() alert on failing purges reads.
	_, err = r.reconcileDBPurge(context.Background(), r.Client, cinder, workloadArtifacts())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(dbPurgeCount(t, cinder, "failed")).To(Equal(1.0), "a run must be counted once per Job UID")
}

// An older failed run must not hold the condition False once a later run has
// succeeded, or a single bad night would wedge Ready until the CronJob's history
// limit pruned the failure away.
func TestReconcileDBPurge_SucceededJobKeepsConditionTrue(t *testing.T) {
	g := NewGomegaWithT(t)
	g.Expect(metrics.Register()).To(Succeed())

	cinder := validCinder()
	cinder.Name = "purge-succeeded-metric"
	t.Cleanup(func() { metrics.DeleteForCinder(cinder.Name, cinder.Namespace) })

	cronJob := seededDBPurgeCronJob(cinder)
	now := metav1.Now()
	older := dbPurgeRunJob(cinder, cronJob, cinder.Name+"-db-purge-28000000",
		batchv1.JobFailed, metav1.NewTime(now.Add(-24*time.Hour)))
	newer := dbPurgeRunJob(cinder, cronJob, cinder.Name+"-db-purge-28001440", batchv1.JobComplete, now)
	r := newCinderTestReconciler(cinder, cronJob, older, newer)

	_, err := r.reconcileDBPurge(context.Background(), r.Client, cinder, workloadArtifacts())
	g.Expect(err).NotTo(HaveOccurred())

	cond := cinderCondition(cinder, conditionTypeDBPurgeReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonDBPurgeScheduled))

	g.Expect(dbPurgeCount(t, cinder, "succeeded")).To(Equal(1.0),
		"the newest terminal run must be the one counted")
	g.Expect(dbPurgeCount(t, cinder, "failed")).To(BeZero(),
		"the superseded older failure must not be counted")
}

// The reconcile lists Jobs by label alone, so metav1.IsControlledBy is the only
// thing separating this CronJob's runs from any other Job carrying the same
// commonLabels — and `kubectl create job --from=cronjob/...` produces exactly
// such a Job, with the labels copied but no controller reference.
func TestReconcileDBPurge_IgnoresJobsItDoesNotControl(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()

	cronJob := seededDBPurgeCronJob(cinder)
	foreign := dbPurgeRunJob(cinder, cronJob, cinder.Name+"-db-purge-manual", batchv1.JobFailed, metav1.Now())
	foreign.OwnerReferences = nil
	r := newCinderTestReconciler(cinder, cronJob, foreign)

	_, err := r.reconcileDBPurge(context.Background(), r.Client, cinder, workloadArtifacts())
	g.Expect(err).NotTo(HaveOccurred())

	cond := cinderCondition(cinder, conditionTypeDBPurgeReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonDBPurgeScheduled))
	g.Expect(cinder.Annotations).NotTo(HaveKey(job.JobUIDAnnotationKey("db-purge")),
		"a foreign run must not consume the dedupe annotation of a run this operator scheduled")
}

// A run that has not reached a terminal condition is not an outcome to report
// on — the condition keeps describing the schedule until the run finishes.
func TestReconcileDBPurge_RunningJobLeavesConditionOnTheSchedule(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()

	cronJob := seededDBPurgeCronJob(cinder)
	running := dbPurgeRunJob(cinder, cronJob, cinder.Name+"-db-purge-28000000", batchv1.JobFailed, metav1.Now())
	running.Status.Conditions = nil
	r := newCinderTestReconciler(cinder, cronJob, running)

	_, err := r.reconcileDBPurge(context.Background(), r.Client, cinder, workloadArtifacts())
	g.Expect(err).NotTo(HaveOccurred())

	cond := cinderCondition(cinder, conditionTypeDBPurgeReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonDBPurgeScheduled))
	g.Expect(cinder.Annotations).NotTo(HaveKey(job.JobUIDAnnotationKey("db-purge")),
		"an unfinished run has no terminal state to count")
}

// dbPurgeCronJobName has to be total. The metadata.name bound that keeps
// "{name}-db-purge" inside the CronJob cap is enforced on create only — and can
// only be, since metadata.name is immutable and rejecting the finalizer-removal
// update would wedge the CR in Terminating — so an operator upgrade inherits
// Cinder CRs the bound would reject.
func TestDBPurgeCronJobName(t *testing.T) {
	g := NewGomegaWithT(t)

	atLimit := strings.Repeat("c", cinderv1alpha1.MaxCinderNameLength)
	g.Expect(dbPurgeCronJobName(atLimit)).To(Equal(atLimit+dbPurgeNameSuffix),
		"an admissible name keeps the documented {name}-db-purge form")

	// One character past the bound is where the collapse begins.
	overlong := strings.Repeat("c", cinderv1alpha1.MaxCinderNameLength+1)
	collapsed := dbPurgeCronJobName(overlong)
	g.Expect(collapsed).NotTo(Equal(overlong + dbPurgeNameSuffix))
	g.Expect(len(collapsed)).To(BeNumerically("<=", cinderv1alpha1.MaxCronJobNameLength))

	// Sweep every truncation offset against both characters a DNS label may not
	// end on, so the collapsed name is applicable wherever the boundary falls.
	for _, sep := range []string{"-", "."} {
		for i := 1; i < cinderv1alpha1.MaxCinderNameLength; i++ {
			name := strings.Repeat("c", i) + sep + strings.Repeat("d", cinderv1alpha1.MaxCinderNameLength)
			got := dbPurgeCronJobName(name)
			g.Expect(len(got)).To(BeNumerically("<=", cinderv1alpha1.MaxCronJobNameLength), name)
			g.Expect(got).To(HaveSuffix(dbPurgeNameSuffix), name)
			g.Expect(utilvalidation.IsDNS1123Subdomain(got)).To(BeEmpty(), name)
			g.Expect(dbPurgeCronJobName(name)).To(Equal(got),
				"the name must be stable across passes, or every reconcile orphans the last CronJob")
		}
	}

	// Two CRs sharing the truncated prefix must not collapse onto one CronJob.
	shared := strings.Repeat("c", cinderv1alpha1.MaxCinderNameLength)
	g.Expect(dbPurgeCronJobName(shared + "-alpha")).NotTo(Equal(dbPurgeCronJobName(shared + "-beta")))
}

// The reconcile itself must go through for such a CR: the CronJob is applied
// under the collapsed name and DBPurgeReady is reported like any other Cinder.
func TestReconcileDBPurge_AppliesForAnOverlongCinderName(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	cinder.Name = strings.Repeat("c", cinderv1alpha1.MaxCinderNameLength+20)
	r := newCinderTestReconciler(cinder)

	_, err := r.reconcileDBPurge(context.Background(), r.Client, cinder, workloadArtifacts())
	g.Expect(err).NotTo(HaveOccurred())

	var cronJob batchv1.CronJob
	g.Expect(r.Get(context.Background(), objectKey(dbPurgeCronJobName(cinder.Name)), &cronJob)).To(Succeed())

	cond := cinderCondition(cinder, conditionTypeDBPurgeReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
}

// The purge pod injects the same DSN the API pods use; when database TLS is on
// that DSN names ssl_ca/ssl_cert/ssl_key paths under the db-tls mount, so a
// purge without the mount fails to open them on every single run.
func TestReconcileDBPurge_ProjectsDBTLSMaterialWhenEnabled(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	cinder.Spec.Database.TLS = &commonv1.DatabaseTLSSpec{
		Mode:                "verify-full",
		CABundleSecretRef:   commonv1.SecretRefSpec{Name: "cinder-db-ca"},
		ClientCertSecretRef: commonv1.SecretRefSpec{Name: "cinder-db-client"},
	}
	r := newCinderTestReconciler(cinder)

	_, err := r.reconcileDBPurge(context.Background(), r.Client, cinder, workloadArtifacts())
	g.Expect(err).NotTo(HaveOccurred())

	var cronJob batchv1.CronJob
	g.Expect(r.Get(context.Background(), objectKey(testCinderName+"-db-purge"), &cronJob)).To(Succeed())

	tlsVol, tlsMount := cinderDBTLSVolumeAndMount(cinder)
	podSpec := cronJob.Spec.JobTemplate.Spec.Template.Spec
	g.Expect(podSpec.Volumes).To(ContainElement(tlsVol))
	g.Expect(podSpec.Containers[0].VolumeMounts).To(ContainElement(tlsMount))
}

// An apply the API server refuses is an infrastructure failure, not a status
// signal: it surfaces as a wrapped error and sets no condition, so the aggregate
// Ready stays False and the pipeline attributes the failure to this step.
func TestReconcileDBPurge_ApplyFailureWrapsTheError(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	boom := errors.New("apiserver said no")
	r := failingApplyReconciler(boom, "CronJob", testCinderName+"-db-purge", cinder)

	_, err := r.reconcileDBPurge(context.Background(), r.Client, cinder, workloadArtifacts())

	g.Expect(err).To(MatchError(boom))
	g.Expect(err.Error()).To(ContainSubstring("ensuring db purge CronJob"))
	g.Expect(cinderCondition(cinder, conditionTypeDBPurgeReady)).To(BeNil(),
		"a failed apply reports no schedule the CR does not have")
}
