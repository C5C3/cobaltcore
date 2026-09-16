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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/c5c3/cobaltcore/internal/common/deployment"
	"github.com/c5c3/cobaltcore/internal/common/job"
	"github.com/c5c3/cobaltcore/internal/common/naming"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
	"github.com/c5c3/cobaltcore/operators/nova/internal/metrics"
)

// The two database-connection overrides the archive pod must carry: nova-manage
// reaches both schemas, and --all-cells is read out of the nova_api one.
const (
	apiDBConnectionEnvVarName  = "OS_API_DATABASE__CONNECTION"
	cellDBConnectionEnvVarName = "OS_DATABASE__CONNECTION"
)

// seededDBArchiveCronJob returns the archive CronJob carrying a stable UID, so a
// Job seeded with a controller reference to it is recognised by
// metav1.IsControlledBy. The fake client does not mint UIDs on create, so the
// CronJob has to be seeded rather than only created by the reconcile.
func seededDBArchiveCronJob(nova *novav1alpha1.Nova) *batchv1.CronJob {
	cronJob := dbArchiveCronJob(nova, workloadArtifacts())
	cronJob.UID = types.UID(nova.Name + "-db-archive-uid")
	return cronJob
}

// dbArchiveRunJob returns a Job as the CronJob controller would spawn it: the
// inherited labels the reconcile lists by, plus the controller reference it
// filters on, in the given terminal state and with a stable UID for the
// terminal-metric dedupe.
func dbArchiveRunJob(nova *novav1alpha1.Nova, cronJob *batchv1.CronJob, name string,
	condition batchv1.JobConditionType, createdAt metav1.Time,
) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         nova.Namespace,
			UID:               types.UID(name + "-uid"),
			CreationTimestamp: createdAt,
			Labels:            componentLabels(nova, componentDBArchive),
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

// dbArchiveCount reads the db-archive counter for one CR and terminal result off
// the controller-runtime registry, or 0 when no such series exists yet.
func dbArchiveCount(t *testing.T, nova *novav1alpha1.Nova, result string) float64 {
	t.Helper()
	families, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gathering the metrics registry: %v", err)
	}
	want := map[string]string{"nova": nova.Name, "namespace": nova.Namespace, "result": result}
	for _, family := range families {
		if family.GetName() != "nova_operator_db_archive_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			if dbArchiveLabelsMatch(metric.GetLabel(), want) {
				return metric.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func dbArchiveLabelsMatch(got []*dto.LabelPair, want map[string]string) bool {
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

func TestEffectiveDBArchive(t *testing.T) {
	tests := []struct {
		name string
		in   *novav1alpha1.DBArchiveSpec
		want novav1alpha1.DBArchiveSpec
	}{
		{
			name: "nil block resolves every operator default",
			in:   nil,
			want: novav1alpha1.DBArchiveSpec{
				Schedule: novav1alpha1.DefaultDBArchiveSchedule,
				MaxRows:  ptr.To(novav1alpha1.DefaultDBArchiveMaxRows),
				Sleep:    ptr.To(novav1alpha1.DefaultDBArchiveSleep),
			},
		},
		{
			name: "empty block resolves every operator default",
			in:   &novav1alpha1.DBArchiveSpec{},
			want: novav1alpha1.DBArchiveSpec{
				Schedule: novav1alpha1.DefaultDBArchiveSchedule,
				MaxRows:  ptr.To(novav1alpha1.DefaultDBArchiveMaxRows),
				Sleep:    ptr.To(novav1alpha1.DefaultDBArchiveSleep),
			},
		},
		{
			name: "only the schedule set keeps the default bounds",
			in:   &novav1alpha1.DBArchiveSpec{Schedule: "0 3 * * *"},
			want: novav1alpha1.DBArchiveSpec{
				Schedule: "0 3 * * *",
				MaxRows:  ptr.To(novav1alpha1.DefaultDBArchiveMaxRows),
				Sleep:    ptr.To(novav1alpha1.DefaultDBArchiveSleep),
			},
		},
		{
			// Zero is a value the schema accepts and the resolver must keep: it runs
			// the batches back to back, which a default of 1 would silently override.
			name: "a zero sleep survives the defaulting",
			in:   &novav1alpha1.DBArchiveSpec{Sleep: ptr.To(int32(0))},
			want: novav1alpha1.DBArchiveSpec{
				Schedule: novav1alpha1.DefaultDBArchiveSchedule,
				MaxRows:  ptr.To(novav1alpha1.DefaultDBArchiveMaxRows),
				Sleep:    ptr.To(int32(0)),
			},
		},
		{
			name: "a fully set block is passed through verbatim",
			in: &novav1alpha1.DBArchiveSpec{
				Schedule:      "@weekly",
				MaxRows:       ptr.To(int32(50)),
				Sleep:         ptr.To(int32(5)),
				RetentionDays: ptr.To(int32(30)),
				Suspend:       true,
			},
			want: novav1alpha1.DBArchiveSpec{
				Schedule:      "@weekly",
				MaxRows:       ptr.To(int32(50)),
				Sleep:         ptr.To(int32(5)),
				RetentionDays: ptr.To(int32(30)),
				Suspend:       true,
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			g.Expect(effectiveDBArchive(tc.in)).To(Equal(tc.want))
		})
	}

	t.Run("nil block is indistinguishable from an empty one", func(t *testing.T) {
		g := NewGomegaWithT(t)
		g.Expect(effectiveDBArchive(nil)).To(Equal(effectiveDBArchive(&novav1alpha1.DBArchiveSpec{})))
	})

	// An unset window means every soft-deleted row is eligible, so a resolver that
	// materialized a number here would hold back rows the contract says are not.
	t.Run("an unset retention window stays unset", func(t *testing.T) {
		g := NewGomegaWithT(t)
		g.Expect(effectiveDBArchive(nil).RetentionDays).To(BeNil())
		g.Expect(effectiveDBArchive(&novav1alpha1.DBArchiveSpec{}).RetentionDays).To(BeNil())
	})

	// The resolver must not write its defaults back into the CR: the stored spec
	// is what keeps tracking the operator defaults across upgrades.
	t.Run("the input spec is left untouched", func(t *testing.T) {
		g := NewGomegaWithT(t)
		in := &novav1alpha1.DBArchiveSpec{}

		out := effectiveDBArchive(in)
		*out.MaxRows = 7

		g.Expect(in).To(Equal(&novav1alpha1.DBArchiveSpec{}))
	})
}

func TestReconcileDBArchive_CreatesCronJobWithDefaults(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova() // spec.dbArchive is nil
	r := newNovaTestReconciler(nova)

	res, err := r.reconcileDBArchive(context.Background(), r.Client, nova, workloadArtifacts())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue(), "the archive step never requeues on its own")

	var cronJob batchv1.CronJob
	g.Expect(r.Get(context.Background(), objectKey(testNovaName+"-db-archive"), &cronJob)).To(Succeed())

	g.Expect(cronJob.Spec.Schedule).To(Equal("@daily"))
	g.Expect(cronJob.Spec.Suspend).To(Equal(ptr.To(false)), "spec.dbArchive.suspend defaults to running")
	g.Expect(cronJob.Spec.ConcurrencyPolicy).To(Equal(batchv1.ForbidConcurrent),
		"a run that outlasts its interval must not be overtaken by the next firing")
	g.Expect(cronJob.Spec.JobTemplate.Spec.ActiveDeadlineSeconds).To(Equal(ptr.To(int64(3600))),
		"a wedged run must reach a terminal state, or DBArchiveReady reports an archive that never happens")
	g.Expect(cronJob.OwnerReferences).To(HaveLen(1))
	g.Expect(cronJob.OwnerReferences[0].Name).To(Equal(testNovaName))

	// The spawned Jobs must be listable by label, so the archive labels sit on the
	// CronJob and the JobTemplate alike. The component is what keeps the archive
	// pods out of the API Service.
	g.Expect(cronJob.Labels).To(Equal(componentLabels(nova, componentDBArchive)))
	g.Expect(cronJob.Spec.JobTemplate.Labels).To(Equal(componentLabels(nova, componentDBArchive)),
		"the JobTemplate must carry the labels reconcileDBArchive lists runs by")
	podTemplate := cronJob.Spec.JobTemplate.Spec.Template
	g.Expect(podTemplate.Labels).To(Equal(componentLabels(nova, componentDBArchive)))
	g.Expect(podTemplate.Spec.RestartPolicy).To(Equal(corev1.RestartPolicyOnFailure))
	// The projected database TLS files are mode 0400; without the openstack GID
	// owning them the archive could not read its client key.
	g.Expect(podTemplate.Spec.SecurityContext).To(Equal(
		&corev1.PodSecurityContext{FSGroup: ptr.To(deployment.OpenStackUID)}))

	g.Expect(podTemplate.Spec.Containers).To(HaveLen(1))
	container := podTemplate.Spec.Containers[0]
	g.Expect(container.Name).To(Equal("db-archive"))
	g.Expect(container.Image).To(Equal("ghcr.io/c5c3/nova:2025.2"))
	g.Expect(container.SecurityContext).To(Equal(deployment.RestrictedSecurityContext()))
	g.Expect(container.Command).To(Equal([]string{"/bin/sh", "-eu", "-c", dbArchiveScript}))

	// nova-manage opens both schemas: the cell rows are archived into the cell's
	// shadow tables, and --all-cells is resolved out of the nova_api cell map.
	g.Expect(envNames(container.Env)).To(ContainElements(
		apiDBConnectionEnvVarName, cellDBConnectionEnvVarName))
	g.Expect(envValue(container.Env, dbArchiveMaxRowsEnvVarName)).To(Equal("1000"))
	g.Expect(envValue(container.Env, dbArchiveSleepEnvVarName)).To(Equal("1"))
	g.Expect(envNames(container.Env)).NotTo(ContainElement(dbArchiveRetentionDaysEnvVarName),
		"without a retention window the script must pass no --before at all")

	// The whole rendered ConfigMap is mounted, the way the migration Jobs mount
	// it: nova-manage needs no file selection.
	g.Expect(podTemplate.Spec.Volumes).To(HaveLen(2), "the config directory and a writable /tmp")
	g.Expect(podTemplate.Spec.Volumes[0].ConfigMap.Name).To(Equal(workloadArtifacts().configMapName))
	g.Expect(podTemplate.Spec.Volumes[0].ConfigMap.Items).To(BeEmpty())
	g.Expect(container.VolumeMounts).To(Equal([]corev1.VolumeMount{
		{Name: configVolumeName, MountPath: novaConfigDir, ReadOnly: true},
		{Name: tmpVolumeName, MountPath: tmpMountPath},
	}))

	cond := novaCondition(nova, conditionTypeDBArchiveReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonDBArchiveScheduled))
	g.Expect(cond.Message).To(ContainSubstring("1000 soft-deleted rows"))
	g.Expect(cond.Message).NotTo(ContainSubstring("days"),
		"no retention window is configured, so the message must not claim one")
}

func TestReconcileDBArchive_HonorsSpecKnobs(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	nova.Spec.DBArchive = &novav1alpha1.DBArchiveSpec{
		Schedule:      "0 3 * * *",
		MaxRows:       ptr.To(int32(500)),
		Sleep:         ptr.To(int32(3)),
		RetentionDays: ptr.To(int32(30)),
	}
	r := newNovaTestReconciler(nova)

	_, err := r.reconcileDBArchive(context.Background(), r.Client, nova, workloadArtifacts())
	g.Expect(err).NotTo(HaveOccurred())

	var cronJob batchv1.CronJob
	g.Expect(r.Get(context.Background(), objectKey(testNovaName+"-db-archive"), &cronJob)).To(Succeed())

	g.Expect(cronJob.Spec.Schedule).To(Equal("0 3 * * *"))
	env := cronJob.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Env
	g.Expect(envValue(env, dbArchiveMaxRowsEnvVarName)).To(Equal("500"))
	g.Expect(envValue(env, dbArchiveSleepEnvVarName)).To(Equal("3"))
	g.Expect(envValue(env, dbArchiveRetentionDaysEnvVarName)).To(Equal("30"),
		"the script turns the window into the --before date it passes")

	cond := novaCondition(nova, conditionTypeDBArchiveReady)
	g.Expect(cond.Message).To(ContainSubstring(`"0 3 * * *"`))
	g.Expect(cond.Message).To(ContainSubstring("last 30 days"))
}

// A nil block and an empty one are documented to resolve identically, which is
// only true if they also render the same object: a CronJob that differed would
// roll the pod template the moment a user wrote "dbArchive: {}".
func TestReconcileDBArchive_NilAndEmptyRenderIdentically(t *testing.T) {
	g := NewGomegaWithT(t)
	nilBlock := validNova()
	emptyBlock := validNova()
	emptyBlock.Spec.DBArchive = &novav1alpha1.DBArchiveSpec{}

	g.Expect(dbArchiveCronJob(nilBlock, workloadArtifacts())).
		To(Equal(dbArchiveCronJob(emptyBlock, workloadArtifacts())))
}

// A suspended archive stays True, pausing it is a deliberate posture, but it
// must say so. The CronJob will never fire again, so reporting the scheduled
// message would assert active archiving that is not happening.
func TestReconcileDBArchive_SuspendedReportsItsOwnReason(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	nova.Spec.DBArchive = &novav1alpha1.DBArchiveSpec{Suspend: true}
	r := newNovaTestReconciler(nova)

	_, err := r.reconcileDBArchive(context.Background(), r.Client, nova, workloadArtifacts())
	g.Expect(err).NotTo(HaveOccurred())

	var cronJob batchv1.CronJob
	g.Expect(r.Get(context.Background(), objectKey(testNovaName+"-db-archive"), &cronJob)).To(Succeed())
	g.Expect(cronJob.Spec.Suspend).To(Equal(ptr.To(true)))

	cond := novaCondition(nova, conditionTypeDBArchiveReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue), "a paused archive is a posture, not a failure")
	g.Expect(cond.Reason).To(Equal(conditionReasonDBArchiveSuspended))
	g.Expect(cond.Message).To(ContainSubstring("suspended"))
	g.Expect(cond.Message).NotTo(ContainSubstring("Database archive scheduled"),
		"the scheduled message asserts archiving a suspended CronJob is not doing")
}

// An archive suspended after a failed run must still report DBArchiveSuspended.
// The failed Job stays the newest terminal one for good, a suspended CronJob
// spawns no successor to supersede it, so a JobFailed arm that outranked
// suspension would pin DBArchiveReady False until someone deleted the Job by
// hand.
func TestReconcileDBArchive_SuspendedAfterAFailedRunStaysTrue(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	nova.Spec.DBArchive = &novav1alpha1.DBArchiveSpec{Suspend: true}

	cronJob := seededDBArchiveCronJob(nova)
	failed := dbArchiveRunJob(nova, cronJob, nova.Name+"-db-archive-28000000", batchv1.JobFailed, metav1.Now())
	r := newNovaTestReconciler(nova, cronJob, failed)
	recorder, ok := r.Recorder.(*record.FakeRecorder)
	g.Expect(ok).To(BeTrue())

	_, err := r.reconcileDBArchive(context.Background(), r.Client, nova, workloadArtifacts())
	g.Expect(err).NotTo(HaveOccurred())

	cond := novaCondition(nova, conditionTypeDBArchiveReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue),
		"no successor run can ever clear the failure, so False here would never lift")
	g.Expect(cond.Reason).To(Equal(conditionReasonDBArchiveSuspended))
	g.Expect(cond.Message).To(ContainSubstring(failed.Name),
		"the pre-pause failure is still worth naming, it is just not the state the CR is in")
	g.Expect(recorder.Events).NotTo(Receive(),
		"a suspended archive raises no event; re-firing the pre-pause failure on every pass would be noise")
}

// The CronJob itself applies cleanly whether or not the archive works, so an
// archive that stopped working is only visible through the Job it spawned.
func TestReconcileDBArchive_FailedJobSetsConditionEventAndMetric(t *testing.T) {
	g := NewGomegaWithT(t)
	g.Expect(metrics.Register()).To(Succeed())

	nova := validNova()
	nova.Name = "archive-failed-metric"
	t.Cleanup(func() { metrics.DeleteForNova(nova.Name, nova.Namespace) })

	cronJob := seededDBArchiveCronJob(nova)
	failed := dbArchiveRunJob(nova, cronJob, nova.Name+"-db-archive-28000000", batchv1.JobFailed, metav1.Now())
	r := newNovaTestReconciler(nova, cronJob, failed)
	recorder, ok := r.Recorder.(*record.FakeRecorder)
	g.Expect(ok).To(BeTrue())

	_, err := r.reconcileDBArchive(context.Background(), r.Client, nova, workloadArtifacts())
	g.Expect(err).NotTo(HaveOccurred(),
		"a failed run is a status signal, not a reconcile error: retrying the pass cannot fix it")

	cond := novaCondition(nova, conditionTypeDBArchiveReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonDBArchiveJobFailed))
	g.Expect(cond.Message).To(ContainSubstring(failed.Name))

	g.Expect(recorder.Events).To(Receive(And(
		ContainSubstring(corev1.EventTypeWarning),
		ContainSubstring(conditionReasonDBArchiveJobFailed),
		ContainSubstring(failed.Name),
	)))

	g.Expect(nova.Annotations).To(HaveKeyWithValue(job.JobUIDAnnotationKey(componentDBArchive), string(failed.UID)),
		"the terminal metric must stamp the dedupe annotation so a run is counted once")
	g.Expect(dbArchiveCount(t, nova, "failed")).To(Equal(1.0), "a failed run must be counted as result=failed")

	// A second pass observes the same run and must not count it twice: the counter
	// is what a rate() alert on failing archives reads.
	_, err = r.reconcileDBArchive(context.Background(), r.Client, nova, workloadArtifacts())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(dbArchiveCount(t, nova, "failed")).To(Equal(1.0), "a run must be counted once per Job UID")
}

// An older failed run must not hold the condition False once a later run has
// succeeded, or a single bad night would wedge Ready until the CronJob's history
// limit pruned the failure away.
func TestReconcileDBArchive_SucceededJobKeepsConditionTrue(t *testing.T) {
	g := NewGomegaWithT(t)
	g.Expect(metrics.Register()).To(Succeed())

	nova := validNova()
	nova.Name = "archive-succeeded-metric"
	t.Cleanup(func() { metrics.DeleteForNova(nova.Name, nova.Namespace) })

	cronJob := seededDBArchiveCronJob(nova)
	now := metav1.Now()
	older := dbArchiveRunJob(nova, cronJob, nova.Name+"-db-archive-28000000",
		batchv1.JobFailed, metav1.NewTime(now.Add(-24*time.Hour)))
	newer := dbArchiveRunJob(nova, cronJob, nova.Name+"-db-archive-28001440", batchv1.JobComplete, now)
	r := newNovaTestReconciler(nova, cronJob, older, newer)

	_, err := r.reconcileDBArchive(context.Background(), r.Client, nova, workloadArtifacts())
	g.Expect(err).NotTo(HaveOccurred())

	cond := novaCondition(nova, conditionTypeDBArchiveReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonDBArchiveScheduled))

	g.Expect(dbArchiveCount(t, nova, "succeeded")).To(Equal(1.0),
		"the newest terminal run must be the one counted")
	g.Expect(dbArchiveCount(t, nova, "failed")).To(BeZero(),
		"the superseded older failure must not be counted")
}

// The reconcile lists Jobs by label alone, so metav1.IsControlledBy is the only
// thing separating this CronJob's runs from any other Job carrying the same
// labels, and `kubectl create job --from=cronjob/...` produces exactly such a
// Job, with the labels copied but no controller reference.
func TestReconcileDBArchive_IgnoresJobsItDoesNotControl(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()

	cronJob := seededDBArchiveCronJob(nova)
	foreign := dbArchiveRunJob(nova, cronJob, nova.Name+"-db-archive-manual", batchv1.JobFailed, metav1.Now())
	foreign.OwnerReferences = nil
	r := newNovaTestReconciler(nova, cronJob, foreign)

	_, err := r.reconcileDBArchive(context.Background(), r.Client, nova, workloadArtifacts())
	g.Expect(err).NotTo(HaveOccurred())

	cond := novaCondition(nova, conditionTypeDBArchiveReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonDBArchiveScheduled))
	g.Expect(nova.Annotations).NotTo(HaveKey(job.JobUIDAnnotationKey(componentDBArchive)),
		"a foreign run must not consume the dedupe annotation of a run this operator scheduled")
}

// A run that has not reached a terminal condition is not an outcome to report
// on, the condition keeps describing the schedule until the run finishes.
func TestReconcileDBArchive_RunningJobLeavesConditionOnTheSchedule(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()

	cronJob := seededDBArchiveCronJob(nova)
	running := dbArchiveRunJob(nova, cronJob, nova.Name+"-db-archive-28000000", batchv1.JobFailed, metav1.Now())
	running.Status.Conditions = nil
	r := newNovaTestReconciler(nova, cronJob, running)

	_, err := r.reconcileDBArchive(context.Background(), r.Client, nova, workloadArtifacts())
	g.Expect(err).NotTo(HaveOccurred())

	cond := novaCondition(nova, conditionTypeDBArchiveReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonDBArchiveScheduled))
	g.Expect(nova.Annotations).NotTo(HaveKey(job.JobUIDAnnotationKey(componentDBArchive)),
		"an unfinished run has no terminal state to count")
}

// The run the CronJob fired after a failed one is still going, so the failure
// is the newest outcome there is. Were the unfinished run taken as the newest,
// the condition would flip back to the schedule the moment the next firing
// started and hide a failure no run has cleared yet.
func TestReconcileDBArchive_UnfinishedRunDoesNotSupersedeAFailure(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	nova.Name = "archive-unfinished-after-failure"
	t.Cleanup(func() { metrics.DeleteForNova(nova.Name, nova.Namespace) })

	cronJob := seededDBArchiveCronJob(nova)
	now := metav1.Now()
	failed := dbArchiveRunJob(nova, cronJob, nova.Name+"-db-archive-28000000",
		batchv1.JobFailed, metav1.NewTime(now.Add(-24*time.Hour)))
	running := dbArchiveRunJob(nova, cronJob, nova.Name+"-db-archive-28001440", batchv1.JobComplete, now)
	running.Status.Conditions = nil
	r := newNovaTestReconciler(nova, cronJob, failed, running)

	_, err := r.reconcileDBArchive(context.Background(), r.Client, nova, workloadArtifacts())
	g.Expect(err).NotTo(HaveOccurred())

	cond := novaCondition(nova, conditionTypeDBArchiveReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonDBArchiveJobFailed))
	g.Expect(cond.Message).To(ContainSubstring(failed.Name))
}

// Listing the runs is how the step learns that the archive stopped working, so
// a List the API server refuses must fail the step rather than report a healthy
// schedule it could not check.
func TestReconcileDBArchive_JobListFailureWrapsTheError(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	boom := errors.New("jobs.batch is forbidden")
	c := novaFakeClientBuilder(nova).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*batchv1.JobList); ok {
				return boom
			}
			return cl.List(ctx, list, opts...)
		},
	}).Build()
	r := &NovaReconciler{Client: c, Scheme: testScheme(), Recorder: record.NewFakeRecorder(10)}

	_, err := r.reconcileDBArchive(context.Background(), r.Client, nova, workloadArtifacts())

	g.Expect(err).To(MatchError(boom))
	g.Expect(err).To(MatchError(ContainSubstring("listing db-archive Jobs")))
	g.Expect(novaCondition(nova, conditionTypeDBArchiveReady)).To(BeNil(),
		"a run history the step could not read reports no schedule")
}

// dbArchiveCronJobName has to be total. The metadata.name bound that keeps
// "{name}-db-archive" inside the CronJob cap is enforced on create only, and can
// only be, since metadata.name is immutable and rejecting the finalizer-removal
// update would wedge the CR in Terminating, so an operator upgrade inherits Nova
// CRs the bound would reject.
func TestDBArchiveCronJobName(t *testing.T) {
	g := NewGomegaWithT(t)

	atLimit := strings.Repeat("n", novav1alpha1.MaxNovaNameLength)
	g.Expect(dbArchiveCronJobName(atLimit)).To(Equal(atLimit+dbArchiveNameSuffix),
		"an admissible name keeps the documented {name}-db-archive form")

	// One character past the bound is where the collapse begins.
	overlong := strings.Repeat("n", novav1alpha1.MaxNovaNameLength+1)
	collapsed := dbArchiveCronJobName(overlong)
	g.Expect(collapsed).NotTo(Equal(overlong + dbArchiveNameSuffix))
	g.Expect(len(collapsed)).To(BeNumerically("<=", novav1alpha1.MaxCronJobNameLength))

	// Sweep every truncation offset against both characters a DNS label may not
	// end on, so the collapsed name is applicable wherever the boundary falls.
	for _, sep := range []string{"-", "."} {
		for i := 1; i < novav1alpha1.MaxNovaNameLength; i++ {
			name := strings.Repeat("n", i) + sep + strings.Repeat("o", novav1alpha1.MaxNovaNameLength)
			got := dbArchiveCronJobName(name)
			g.Expect(len(got)).To(BeNumerically("<=", novav1alpha1.MaxCronJobNameLength), name)
			g.Expect(got).To(HaveSuffix(dbArchiveNameSuffix), name)
			g.Expect(utilvalidation.IsDNS1123Subdomain(got)).To(BeEmpty(), name)
			g.Expect(dbArchiveCronJobName(name)).To(Equal(got),
				"the name must be stable across passes, or every reconcile orphans the last CronJob")
		}
	}

	// Two CRs sharing the truncated prefix must not collapse onto one CronJob.
	shared := strings.Repeat("n", novav1alpha1.MaxNovaNameLength)
	g.Expect(dbArchiveCronJobName(shared + "-alpha")).NotTo(Equal(dbArchiveCronJobName(shared + "-beta")))
}

// The reconcile itself must go through for such a CR: the CronJob is applied
// under the collapsed name and DBArchiveReady is reported like any other Nova.
func TestReconcileDBArchive_AppliesForAnOverlongNovaName(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	nova.Name = strings.Repeat("n", novav1alpha1.MaxNovaNameLength+20)
	r := newNovaTestReconciler(nova)

	_, err := r.reconcileDBArchive(context.Background(), r.Client, nova, workloadArtifacts())
	g.Expect(err).NotTo(HaveOccurred())

	var cronJob batchv1.CronJob
	g.Expect(r.Get(context.Background(), objectKey(dbArchiveCronJobName(nova.Name)), &cronJob)).To(Succeed())

	cond := novaCondition(nova, conditionTypeDBArchiveReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
}

// The archive pod injects the same DSNs the API pods use; when database TLS is
// on those DSNs name ssl_ca/ssl_cert/ssl_key paths under the per-schema mounts,
// so an archive without them fails to open the databases on every single run.
func TestReconcileDBArchive_ProjectsDBTLSMaterialWhenEnabled(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	for _, db := range []*commonv1.DatabaseSpec{&nova.Spec.APIDatabase, &nova.Spec.Database} {
		db.TLS = &commonv1.DatabaseTLSSpec{
			Mode:                "verify-full",
			CABundleSecretRef:   commonv1.SecretRefSpec{Name: "nova-db-ca"},
			ClientCertSecretRef: commonv1.SecretRefSpec{Name: "nova-db-client"},
		}
	}
	r := newNovaTestReconciler(nova)

	_, err := r.reconcileDBArchive(context.Background(), r.Client, nova, workloadArtifacts())
	g.Expect(err).NotTo(HaveOccurred())

	var cronJob batchv1.CronJob
	g.Expect(r.Get(context.Background(), objectKey(testNovaName+"-db-archive"), &cronJob)).To(Succeed())

	tlsVolumes, tlsMounts := novaDBTLSVolumesAndMounts(nova)
	g.Expect(tlsVolumes).To(HaveLen(2), "both schemas verify their transport in this fixture")
	podSpec := cronJob.Spec.JobTemplate.Spec.Template.Spec
	for i := range tlsVolumes {
		g.Expect(podSpec.Volumes).To(ContainElement(tlsVolumes[i]))
		g.Expect(podSpec.Containers[0].VolumeMounts).To(ContainElement(tlsMounts[i]))
	}
}

// An apply the API server refuses is an infrastructure failure, not a status
// signal: it surfaces as a wrapped error and sets no condition, so the aggregate
// Ready stays False and the pipeline attributes the failure to this step.
func TestReconcileDBArchive_ApplyFailureWrapsTheError(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	boom := errors.New("apiserver said no")
	r := failingApplyReconciler(boom, "CronJob", testNovaName+"-db-archive", nova)

	_, err := r.reconcileDBArchive(context.Background(), r.Client, nova, workloadArtifacts())

	g.Expect(err).To(MatchError(boom))
	g.Expect(err.Error()).To(ContainSubstring("ensuring db-archive CronJob"))
	g.Expect(novaCondition(nova, conditionTypeDBArchiveReady)).To(BeNil(),
		"a failed apply reports no schedule the CR does not have")
}

// The archive pods must not become endpoints of the API Service: they carry no
// readiness probe, so a scheduled run would turn a pod with nothing listening on
// 8774 into a ready backend the Service routes API requests to.
func TestDBArchivePodsNotSelectedByAPIService(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()

	selector := labels.SelectorFromSet(buildAPIService(nova).Spec.Selector)

	apiPodLabels := buildAPIDeployment(nova, workloadArtifacts(), workloadDigests{}).Spec.Template.Labels
	g.Expect(selector.Matches(labels.Set(apiPodLabels))).To(BeTrue(),
		"the API pod template must satisfy the API Service selector")

	archivePodLabels := dbArchiveCronJob(nova, workloadArtifacts()).Spec.JobTemplate.Spec.Template.Labels
	g.Expect(archivePodLabels).To(HaveKeyWithValue(naming.LabelKeyComponent, componentDBArchive))
	g.Expect(archivePodLabels).NotTo(Equal(apiSelectorLabels(nova)))
	g.Expect(selector.Matches(labels.Set(archivePodLabels))).To(BeFalse(),
		"db-archive pods must never become endpoints of the API Service")
}
