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
	"k8s.io/apimachinery/pkg/labels"
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
	glancev1alpha1 "github.com/c5c3/forge/operators/glance/api/v1alpha1"
	"github.com/c5c3/forge/operators/glance/internal/metrics"
)

// dbPurgeConfigMapName is the rendered-config ConfigMap the purge CronJob mounts,
// as the Database step hands it to the parallel group.
const dbPurgeConfigMapName = "test-glance-config-abc"

// seededDBPurgeCronJob returns the purge CronJob carrying a stable UID, so a Job
// seeded with a controller reference to it is recognised by
// metav1.IsControlledBy. The fake client does not mint UIDs on create, so the
// CronJob has to be seeded rather than only created by the reconcile.
func seededDBPurgeCronJob(glance *glancev1alpha1.Glance) *batchv1.CronJob {
	cronJob := dbPurgeCronJob(glance, dbPurgeConfigMapName)
	cronJob.UID = types.UID(glance.Name + "-db-purge-uid")
	return cronJob
}

// dbPurgeRunJob returns a Job as the CronJob controller would spawn it — the
// inherited commonLabels the reconcile lists by, plus the controller reference
// it filters on — in the given terminal state and with a stable UID for the
// terminal-metric dedupe.
func dbPurgeRunJob(glance *glancev1alpha1.Glance, cronJob *batchv1.CronJob, name string,
	condition batchv1.JobConditionType, createdAt metav1.Time,
) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         glance.Namespace,
			UID:               types.UID(name + "-uid"),
			CreationTimestamp: createdAt,
			Labels:            commonLabels(glance),
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

func TestEffectiveDBPurge(t *testing.T) {
	tests := []struct {
		name string
		in   *glancev1alpha1.DBPurgeSpec
		want glancev1alpha1.DBPurgeSpec
	}{
		{
			name: "nil block resolves every operator default",
			in:   nil,
			want: glancev1alpha1.DBPurgeSpec{
				RetentionDays:    ptr.To(glancev1alpha1.DefaultDBPurgeRetentionDays),
				Schedule:         glancev1alpha1.DefaultDBPurgeSchedule,
				PurgeImagesTable: ptr.To(false),
			},
		},
		{
			name: "empty block resolves every operator default",
			in:   &glancev1alpha1.DBPurgeSpec{},
			want: glancev1alpha1.DBPurgeSpec{
				RetentionDays:    ptr.To(glancev1alpha1.DefaultDBPurgeRetentionDays),
				Schedule:         glancev1alpha1.DefaultDBPurgeSchedule,
				PurgeImagesTable: ptr.To(false),
			},
		},
		{
			name: "only the schedule set keeps the default retention",
			in:   &glancev1alpha1.DBPurgeSpec{Schedule: "0 3 * * *"},
			want: glancev1alpha1.DBPurgeSpec{
				RetentionDays:    ptr.To(glancev1alpha1.DefaultDBPurgeRetentionDays),
				Schedule:         "0 3 * * *",
				PurgeImagesTable: ptr.To(false),
			},
		},
		{
			name: "only the retention set keeps the default schedule",
			in:   &glancev1alpha1.DBPurgeSpec{RetentionDays: ptr.To(int32(7))},
			want: glancev1alpha1.DBPurgeSpec{
				RetentionDays:    ptr.To(int32(7)),
				Schedule:         glancev1alpha1.DefaultDBPurgeSchedule,
				PurgeImagesTable: ptr.To(false),
			},
		},
		{
			name: "an explicit purgeImagesTable=false stays false",
			in:   &glancev1alpha1.DBPurgeSpec{PurgeImagesTable: ptr.To(false)},
			want: glancev1alpha1.DBPurgeSpec{
				RetentionDays:    ptr.To(glancev1alpha1.DefaultDBPurgeRetentionDays),
				Schedule:         glancev1alpha1.DefaultDBPurgeSchedule,
				PurgeImagesTable: ptr.To(false),
			},
		},
		{
			name: "a fully set block is passed through verbatim",
			in: &glancev1alpha1.DBPurgeSpec{
				RetentionDays:    ptr.To(int32(1)),
				Schedule:         "@weekly",
				PurgeImagesTable: ptr.To(true),
				Suspend:          true,
			},
			want: glancev1alpha1.DBPurgeSpec{
				RetentionDays:    ptr.To(int32(1)),
				Schedule:         "@weekly",
				PurgeImagesTable: ptr.To(true),
				Suspend:          true,
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
		g.Expect(effectiveDBPurge(nil)).To(Equal(effectiveDBPurge(&glancev1alpha1.DBPurgeSpec{})))
	})
}

func TestReconcileDBPurge_CreatesCronJobWithDefaults(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance() // spec.dbPurge is nil
	r := newGlanceTestReconciler(glance)

	res, err := r.reconcileDBPurge(context.Background(), glance, dbPurgeConfigMapName)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue(), "the purge step never requeues on its own")

	var cronJob batchv1.CronJob
	g.Expect(r.Get(context.Background(), client.ObjectKey{
		Name: "test-glance-db-purge", Namespace: "default",
	}, &cronJob)).To(Succeed())

	g.Expect(cronJob.Spec.Schedule).To(Equal("1 0 * * *"))
	g.Expect(cronJob.Spec.Suspend).To(Equal(ptr.To(false)), "spec.dbPurge.suspend defaults to running")
	g.Expect(cronJob.Spec.ConcurrencyPolicy).To(Equal(batchv1.ForbidConcurrent),
		"a run that outlasts its interval must not be overtaken by the next firing")
	g.Expect(cronJob.Spec.JobTemplate.Spec.ActiveDeadlineSeconds).To(Equal(ptr.To(int64(3600))),
		"a wedged run must reach a terminal state, or DBPurgeReady reports a purge that never happens")
	g.Expect(cronJob.OwnerReferences).To(HaveLen(1))
	g.Expect(cronJob.OwnerReferences[0].Name).To(Equal("test-glance"))

	// The spawned Jobs must be listable by label, so the common labels sit on the
	// CronJob and the JobTemplate alike. The pod template adds the component on
	// top, which is what keeps the purge pods out of the API Service.
	g.Expect(cronJob.Labels).To(Equal(commonLabels(glance)))
	g.Expect(cronJob.Spec.JobTemplate.Labels).To(Equal(commonLabels(glance)),
		"the JobTemplate must carry the labels reconcileDBPurge lists runs by")
	podTemplate := cronJob.Spec.JobTemplate.Spec.Template
	g.Expect(podTemplate.Labels).To(Equal(componentLabels(glance, "db-purge")))
	g.Expect(podTemplate.Labels).To(HaveKeyWithValue(naming.LabelKeyComponent, "db-purge"))
	g.Expect(podTemplate.Spec.RestartPolicy).To(Equal(corev1.RestartPolicyOnFailure))
	g.Expect(podTemplate.Spec.PriorityClassName).To(BeEmpty(),
		"glance's own migration Jobs set no priority class either")

	g.Expect(podTemplate.Spec.Containers).To(HaveLen(1))
	container := podTemplate.Spec.Containers[0]
	g.Expect(container.Name).To(Equal("db-purge"))
	g.Expect(container.Image).To(Equal("ghcr.io/c5c3/glance:2026.1"))
	g.Expect(container.SecurityContext).To(Equal(deployment.RestrictedSecurityContext()))

	// Pinned verbatim rather than by substring: the images pass is opt-in, and
	// the chaining is load-bearing when it is on, so an assertion that survives
	// a reordering or a degraded "&&" would pin nothing.
	g.Expect(container.Command).To(Equal([]string{
		"/bin/sh", "-eu", "-c",
		"glance-manage --config-dir " + glanceConfigDir + " db purge --age_in_days 30 --max_rows 1000",
	}),
		"without spec.dbPurge.purgeImagesTable the images rows — and the UUIDs they reserve — are left alone")

	g.Expect(container.Env).To(HaveLen(1))
	g.Expect(container.Env[0].Name).To(Equal(database.ConnectionEnvVarName))
	g.Expect(container.VolumeMounts).To(Equal([]corev1.VolumeMount{
		{Name: "config", MountPath: glanceConfigDir, ReadOnly: true},
	}))
	g.Expect(podTemplate.Spec.Volumes).To(HaveLen(1))
	g.Expect(podTemplate.Spec.Volumes[0].ConfigMap.Name).To(Equal(dbPurgeConfigMapName))

	cond := conditions.GetCondition(glance.Status.Conditions, conditionTypeDBPurgeReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonDBPurgeScheduled))
	g.Expect(cond.Message).To(ContainSubstring("30 days"))
}

// TestDBPurgePodsNotSelectedByAPIService locks the invariant the API Service
// depends on: only the API pods satisfy its selector. The purge pod template
// once carried the full selector, and because the Service targets a numeric
// port and job pods have no readiness probe, every scheduled run turned a pod
// with nothing listening on 9292 into a ready endpoint.
func TestDBPurgePodsNotSelectedByAPIService(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()

	selector := labels.SelectorFromSet(buildGlanceService(glance, true).Spec.Selector)

	// The API pods must keep matching, or the Service would have no backends.
	apiPodLabels := buildGlanceDeployment(glance, testArtifacts(), "", "").Spec.Template.Labels
	g.Expect(selector.Matches(labels.Set(apiPodLabels))).To(BeTrue(),
		"the API pod template must satisfy the API Service selector")

	purgePodLabels := dbPurgeCronJob(glance, dbPurgeConfigMapName).Spec.JobTemplate.Spec.Template.Labels
	g.Expect(purgePodLabels).To(HaveKeyWithValue(naming.LabelKeyComponent, "db-purge"))
	g.Expect(selector.Matches(labels.Set(purgePodLabels))).To(BeFalse(),
		"db-purge pods must never become endpoints of the API Service")
}

func TestReconcileDBPurge_HonorsSpecKnobs(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	glance.Spec.DBPurge = &glancev1alpha1.DBPurgeSpec{
		RetentionDays:    ptr.To(int32(7)),
		Schedule:         "0 3 * * *",
		PurgeImagesTable: ptr.To(true),
		Suspend:          true,
	}
	r := newGlanceTestReconciler(glance)

	_, err := r.reconcileDBPurge(context.Background(), glance, dbPurgeConfigMapName)
	g.Expect(err).NotTo(HaveOccurred())

	var cronJob batchv1.CronJob
	g.Expect(r.Get(context.Background(), client.ObjectKey{
		Name: "test-glance-db-purge", Namespace: "default",
	}, &cronJob)).To(Succeed())

	g.Expect(cronJob.Spec.Schedule).To(Equal("0 3 * * *"))
	g.Expect(cronJob.Spec.Suspend).To(Equal(ptr.To(true)))
	// The chaining order is load-bearing — the child rows reference the image
	// rows — so the whole command is pinned rather than each half separately:
	// two ContainSubstring assertions pass in either order, and also pass if the
	// "&&" degrades to a ";" that keeps going after a failed first pass.
	g.Expect(cronJob.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Command).To(Equal([]string{
		"/bin/sh", "-eu", "-c",
		"glance-manage --config-dir " + glanceConfigDir + " db purge --age_in_days 7 --max_rows 1000" +
			" && glance-manage --config-dir " + glanceConfigDir + " db purge_images_table --age_in_days 7 --max_rows 1000",
	}))

	cond := conditions.GetCondition(glance.Status.Conditions, conditionTypeDBPurgeReady)
	g.Expect(cond.Message).To(ContainSubstring(`"0 3 * * *"`))
	g.Expect(cond.Message).To(ContainSubstring("7 days"))
}

// A suspended purge stays True — pausing it is a deliberate posture, not a
// failure — but it must say so. The CronJob will never fire again, so reporting
// the scheduled message would assert active hard-deletion that is not happening,
// and nothing else surfaces the drift: no run fails, so no Warning event fires,
// and glance_operator_db_purge_total merely stops incrementing.
func TestReconcileDBPurge_SuspendedReportsItsOwnReason(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	glance.Spec.DBPurge = &glancev1alpha1.DBPurgeSpec{Suspend: true}
	r := newGlanceTestReconciler(glance)

	_, err := r.reconcileDBPurge(context.Background(), glance, dbPurgeConfigMapName)
	g.Expect(err).NotTo(HaveOccurred())

	cond := conditions.GetCondition(glance.Status.Conditions, conditionTypeDBPurgeReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue),
		"a paused purge is a posture, not a failure")
	g.Expect(cond.Reason).To(Equal(conditionReasonDBPurgeSuspended))
	g.Expect(cond.Message).To(ContainSubstring("suspended"))
	g.Expect(cond.Message).NotTo(ContainSubstring("Database purge scheduled"),
		"the scheduled message asserts hard-deletion a suspended CronJob is not doing")
}

// A purge suspended after a failed run must still report DBPurgeSuspended. The
// failed Job stays the newest terminal one for good — a suspended CronJob spawns
// no successor to supersede it, and the default failedJobsHistoryLimit retains it
// — so a JobFailed arm that outranked suspension would pin DBPurgeReady False
// forever, holding the aggregate Ready and the ControlPlane's mirrored
// GlanceReady down until someone deleted the Job by hand.
func TestReconcileDBPurge_SuspendedAfterAFailedRunStaysTrue(t *testing.T) {
	g := NewGomegaWithT(t)
	g.Expect(metrics.Register()).To(Succeed())

	glance := testGlance()
	glance.Name = "purge-suspended-after-failure"
	glance.Spec.DBPurge = &glancev1alpha1.DBPurgeSpec{Suspend: true}
	t.Cleanup(func() { metrics.DeleteForGlance(glance.Name, glance.Namespace) })

	cronJob := seededDBPurgeCronJob(glance)
	failed := dbPurgeRunJob(glance, cronJob, glance.Name+"-db-purge-28000000",
		batchv1.JobFailed, metav1.Now())
	r := newGlanceTestReconciler(glance, cronJob, failed)
	recorder, ok := r.Recorder.(*record.FakeRecorder)
	g.Expect(ok).To(BeTrue())

	_, err := r.reconcileDBPurge(context.Background(), glance, dbPurgeConfigMapName)
	g.Expect(err).NotTo(HaveOccurred())

	cond := conditions.GetCondition(glance.Status.Conditions, conditionTypeDBPurgeReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue),
		"no successor run can ever clear the failure, so False here would never lift")
	g.Expect(cond.Reason).To(Equal(conditionReasonDBPurgeSuspended))
	g.Expect(cond.Message).To(ContainSubstring(failed.Name),
		"the pre-pause failure is still worth naming, it is just not the state the CR is in")
	g.Expect(recorder.Events).NotTo(Receive(),
		"a suspended purge raises no event; re-firing the pre-pause failure on every pass would be noise")
}

// dbPurgeCronJobName has to be total. The metadata.name bound that keeps
// "{name}-db-purge" inside the CronJob cap is enforced on create only — and can
// only be, since metadata.name is immutable and rejecting the finalizer-removal
// update would wedge the CR in Terminating — so an operator upgrade inherits
// Glance CRs the bound would reject. Building their name unconditionally would
// fail every reconcile on an apply the API server refuses, with nothing left to
// edit to repair it.
func TestDBPurgeCronJobName(t *testing.T) {
	g := NewGomegaWithT(t)

	atLimit := strings.Repeat("g", glancev1alpha1.MaxGlanceNameLength)
	g.Expect(dbPurgeCronJobName(atLimit)).To(Equal(atLimit+dbPurgeNameSuffix),
		"an admissible name keeps the documented {name}-db-purge form")

	// Sweep every truncation offset against both characters a DNS label may not
	// end on, so the collapsed name is applicable wherever the boundary falls.
	for _, sep := range []string{"-", "."} {
		for i := 1; i < glancev1alpha1.MaxGlanceNameLength; i++ {
			name := strings.Repeat("g", i) + sep + strings.Repeat("h", glancev1alpha1.MaxGlanceNameLength)
			got := dbPurgeCronJobName(name)
			g.Expect(len(got)).To(BeNumerically("<=", glancev1alpha1.MaxCronJobNameLength), name)
			g.Expect(got).To(HaveSuffix(dbPurgeNameSuffix), name)
			g.Expect(utilvalidation.IsDNS1123Subdomain(got)).To(BeEmpty(), name)
			g.Expect(dbPurgeCronJobName(name)).To(Equal(got),
				"the name must be stable across passes, or every reconcile orphans the last CronJob")
		}
	}

	// Two CRs sharing the truncated prefix must not collapse onto one CronJob.
	shared := strings.Repeat("g", glancev1alpha1.MaxGlanceNameLength)
	g.Expect(dbPurgeCronJobName(shared + "-alpha")).NotTo(Equal(dbPurgeCronJobName(shared + "-beta")))
}

// The reconcile itself must go through for such a CR: the CronJob is applied
// under the collapsed name and DBPurgeReady is reported like any other Glance.
func TestReconcileDBPurge_AppliesForAnOverlongGlanceName(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	glance.Name = strings.Repeat("g", glancev1alpha1.MaxGlanceNameLength+20)
	r := newGlanceTestReconciler(glance)

	_, err := r.reconcileDBPurge(context.Background(), glance, dbPurgeConfigMapName)
	g.Expect(err).NotTo(HaveOccurred())

	var cronJob batchv1.CronJob
	g.Expect(r.Get(context.Background(), client.ObjectKey{
		Name: dbPurgeCronJobName(glance.Name), Namespace: glance.Namespace,
	}, &cronJob)).To(Succeed())

	cond := conditions.GetCondition(glance.Status.Conditions, conditionTypeDBPurgeReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
}

// The purge pod injects the same DSN the API pods use; when database TLS is on
// that DSN names ssl_ca/ssl_cert/ssl_key paths under the db-tls mount, so a
// purge without the mount fails to open them on every single run.
func TestReconcileDBPurge_ProjectsDBTLSMaterialWhenEnabled(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	glance.Spec.Database.TLS = &commonv1.DatabaseTLSSpec{
		Mode:                "verify-full",
		CABundleSecretRef:   commonv1.SecretRefSpec{Name: "glance-db-ca"},
		ClientCertSecretRef: commonv1.SecretRefSpec{Name: "glance-db-client"},
	}
	r := newGlanceTestReconciler(glance)

	_, err := r.reconcileDBPurge(context.Background(), glance, dbPurgeConfigMapName)
	g.Expect(err).NotTo(HaveOccurred())

	var cronJob batchv1.CronJob
	g.Expect(r.Get(context.Background(), client.ObjectKey{
		Name: "test-glance-db-purge", Namespace: "default",
	}, &cronJob)).To(Succeed())

	tlsVol, tlsMount := glanceDBTLSVolumeAndMount(glance)
	podSpec := cronJob.Spec.JobTemplate.Spec.Template.Spec
	g.Expect(podSpec.Volumes).To(ContainElement(tlsVol))
	g.Expect(podSpec.Containers[0].VolumeMounts).To(ContainElement(tlsMount))
}

// TestReconcileDBPurge_FailedJobSetsConditionEventAndMetric covers the run-failure
// path: the CronJob itself applies cleanly, so a purge that stops working is only
// visible through the Job it spawned.
func TestReconcileDBPurge_FailedJobSetsConditionEventAndMetric(t *testing.T) {
	g := NewGomegaWithT(t)
	g.Expect(metrics.Register()).To(Succeed())

	glance := testGlance()
	glance.Name = "purge-failed-metric"
	t.Cleanup(func() { metrics.DeleteForGlance(glance.Name, glance.Namespace) })

	cronJob := seededDBPurgeCronJob(glance)
	failed := dbPurgeRunJob(glance, cronJob, glance.Name+"-db-purge-28000000",
		batchv1.JobFailed, metav1.Now())
	r := newGlanceTestReconciler(glance, cronJob, failed)
	recorder, ok := r.Recorder.(*record.FakeRecorder)
	g.Expect(ok).To(BeTrue())

	_, err := r.reconcileDBPurge(context.Background(), glance, dbPurgeConfigMapName)
	g.Expect(err).NotTo(HaveOccurred(),
		"a failed run is a status signal, not a reconcile error: retrying the pass cannot fix it")

	cond := conditions.GetCondition(glance.Status.Conditions, conditionTypeDBPurgeReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonDBPurgeJobFailed))
	g.Expect(cond.Message).To(ContainSubstring(failed.Name))

	g.Expect(recorder.Events).To(Receive(And(
		ContainSubstring(corev1.EventTypeWarning),
		ContainSubstring(conditionReasonDBPurgeJobFailed),
		ContainSubstring(failed.Name),
	)))

	g.Expect(glance.Annotations).To(HaveKey(job.JobUIDAnnotationKey("db-purge")),
		"the terminal metric must stamp the dedupe annotation so a run is counted once")
	metric := findMetricByLabels(t, ctrlmetrics.Registry, "glance_operator_db_purge_total",
		map[string]string{"glance": glance.Name, "namespace": glance.Namespace, "result": "failed"})
	g.Expect(metric).NotTo(BeNil(), "a failed run must be counted as result=failed")
	g.Expect(metric.GetCounter().GetValue()).To(Equal(1.0))
}

// TestReconcileDBPurge_SucceededJobKeepsConditionTrue also pins the newest-wins
// rule: an older failed run must not hold the condition False once a later run
// has succeeded, or a single bad night would wedge Ready until the CronJob's
// history limit pruned the failure away.
func TestReconcileDBPurge_SucceededJobKeepsConditionTrue(t *testing.T) {
	g := NewGomegaWithT(t)
	g.Expect(metrics.Register()).To(Succeed())

	glance := testGlance()
	glance.Name = "purge-succeeded-metric"
	t.Cleanup(func() { metrics.DeleteForGlance(glance.Name, glance.Namespace) })

	cronJob := seededDBPurgeCronJob(glance)
	now := metav1.Now()
	older := dbPurgeRunJob(glance, cronJob, glance.Name+"-db-purge-28000000",
		batchv1.JobFailed, metav1.NewTime(now.Add(-24*time.Hour)))
	newer := dbPurgeRunJob(glance, cronJob, glance.Name+"-db-purge-28001440",
		batchv1.JobComplete, now)
	r := newGlanceTestReconciler(glance, cronJob, older, newer)

	_, err := r.reconcileDBPurge(context.Background(), glance, dbPurgeConfigMapName)
	g.Expect(err).NotTo(HaveOccurred())

	cond := conditions.GetCondition(glance.Status.Conditions, conditionTypeDBPurgeReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonDBPurgeScheduled))

	metric := findMetricByLabels(t, ctrlmetrics.Registry, "glance_operator_db_purge_total",
		map[string]string{"glance": glance.Name, "namespace": glance.Namespace, "result": "succeeded"})
	g.Expect(metric).NotTo(BeNil(), "the newest terminal run must be the one counted")
	g.Expect(metric.GetCounter().GetValue()).To(Equal(1.0))
	g.Expect(findMetricByLabels(t, ctrlmetrics.Registry, "glance_operator_db_purge_total",
		map[string]string{"glance": glance.Name, "namespace": glance.Namespace, "result": "failed"})).
		To(BeNil(), "the superseded older failure must not be counted")
}

// The reconcile lists Jobs by label alone, so metav1.IsControlledBy is the only
// thing separating this CronJob's runs from any other Job carrying the same
// commonLabels — and `kubectl create job --from=cronjob/...` produces exactly
// such a Job, with the labels copied but no controller reference. Only the
// positive side of that filter is exercised elsewhere; without this case,
// loosening it to "has any owner" would keep the whole suite green while a
// foreign Job flipped the condition and burned the once-per-UID dedupe
// annotation on a UID this operator never scheduled.
func TestReconcileDBPurge_IgnoresJobsItDoesNotControl(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()

	cronJob := seededDBPurgeCronJob(glance)
	foreign := dbPurgeRunJob(glance, cronJob, glance.Name+"-db-purge-manual",
		batchv1.JobFailed, metav1.Now())
	foreign.OwnerReferences = nil
	r := newGlanceTestReconciler(glance, cronJob, foreign)

	_, err := r.reconcileDBPurge(context.Background(), glance, dbPurgeConfigMapName)
	g.Expect(err).NotTo(HaveOccurred())

	cond := conditions.GetCondition(glance.Status.Conditions, conditionTypeDBPurgeReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonDBPurgeScheduled))
	g.Expect(glance.Annotations).NotTo(HaveKey(job.JobUIDAnnotationKey("db-purge")),
		"a foreign run must not consume the dedupe annotation of a run this operator scheduled")
}

// A run that has not reached a terminal condition is not an outcome to report
// on — the condition keeps describing the schedule until the run finishes.
// activeDeadlineSeconds is what bounds how long that can last: it turns a
// wedged run into a JobFailed, which the failure path above then surfaces.
func TestReconcileDBPurge_RunningJobLeavesConditionOnTheSchedule(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()

	cronJob := seededDBPurgeCronJob(glance)
	running := dbPurgeRunJob(glance, cronJob, glance.Name+"-db-purge-28000000",
		batchv1.JobFailed, metav1.Now())
	running.Status.Conditions = nil
	r := newGlanceTestReconciler(glance, cronJob, running)

	_, err := r.reconcileDBPurge(context.Background(), glance, dbPurgeConfigMapName)
	g.Expect(err).NotTo(HaveOccurred())

	cond := conditions.GetCondition(glance.Status.Conditions, conditionTypeDBPurgeReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonDBPurgeScheduled))
	g.Expect(glance.Annotations).NotTo(HaveKey(job.JobUIDAnnotationKey("db-purge")),
		"an unfinished run has no terminal state to count")
}
