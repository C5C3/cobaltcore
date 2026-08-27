// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/c5c3/cobaltcore/internal/common/job"
	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
	neutronmetrics "github.com/c5c3/cobaltcore/operators/neutron/internal/metrics"
)

// ovnDBSyncCronJobKey addresses the sync CronJob of the shared fixture.
var ovnDBSyncCronJobKey = client.ObjectKey{Namespace: testNamespace, Name: testNeutronName + "-ovn-db-sync"}

// syncingNeutron returns a Neutron that schedules the OVN database
// synchronisation, with the block the given function fills in.
func syncingNeutron(mutate func(*neutronv1alpha1.OVNDBSyncSpec)) *neutronv1alpha1.Neutron {
	neutron := validNeutron()
	spec := &neutronv1alpha1.OVNDBSyncSpec{}
	if mutate != nil {
		mutate(spec)
	}
	neutron.Spec.OVNDBSync = spec
	return neutron
}

// seededOVNDBSyncCronJob returns the sync CronJob carrying a stable UID, so a
// Job seeded with a controller reference to it is recognised by
// metav1.IsControlledBy. The fake client mints no UIDs on create, so the CronJob
// has to be seeded rather than only created by the reconcile.
func seededOVNDBSyncCronJob(neutron *neutronv1alpha1.Neutron) *batchv1.CronJob {
	cronJob := buildOVNDBSyncCronJob(neutron, deploymentConfigMapName)
	cronJob.UID = types.UID(neutron.Name + "-ovn-db-sync-uid")
	return cronJob
}

// ovnDBSyncRunJob returns a Job as the CronJob controller would spawn it — the
// inherited component labels the reconcile lists by, plus the controller
// reference it filters on — in the given terminal state and with a stable UID
// for the terminal-metric dedupe.
func ovnDBSyncRunJob(neutron *neutronv1alpha1.Neutron, cronJob *batchv1.CronJob, name string,
	condition batchv1.JobConditionType, createdAt metav1.Time,
) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         neutron.Namespace,
			UID:               types.UID(name + "-uid"),
			CreationTimestamp: createdAt,
			Labels:            componentLabels(neutron, componentOVNDBSync),
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

// TestEffectiveOVNDBSync pins the reconcile-time resolution: an unset field
// tracks the operator default across upgrades rather than freezing today's value
// into the stored CR.
func TestEffectiveOVNDBSync(t *testing.T) {
	g := NewGomegaWithT(t)

	fromNil := effectiveOVNDBSync(nil)
	g.Expect(fromNil.Schedule).To(Equal(neutronv1alpha1.DefaultOVNDBSyncSchedule))
	g.Expect(fromNil.SyncMode).To(Equal("log"))
	g.Expect(fromNil.Suspend).To(BeFalse())

	// A half-filled block keeps what it sets and defaults the rest.
	partial := &neutronv1alpha1.OVNDBSyncSpec{SyncMode: "repair"}
	resolved := effectiveOVNDBSync(partial)
	g.Expect(resolved.Schedule).To(Equal(neutronv1alpha1.DefaultOVNDBSyncSchedule))
	g.Expect(resolved.SyncMode).To(Equal("repair"))
	g.Expect(partial.Schedule).To(BeEmpty(), "the input spec must be left untouched")
}

// TestReconcileOVNDBSync_NilSpecDeletesTheCronJob covers the opt-out: removing
// spec.ovnDBSync from a CR that had it must take the CronJob with it, and the
// absence is a configured state rather than a degraded one, so the condition
// resolves True.
func TestReconcileOVNDBSync_NilSpecDeletesTheCronJob(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	// The CronJob a previous spec left behind.
	previous := syncingNeutron(nil)
	neutron := validNeutron()
	r := newNeutronTestReconciler(neutron, seededOVNDBSyncCronJob(previous))

	res, err := r.reconcileOVNDBSync(ctx, r.Client, neutron, deploymentConfigMapName)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(apierrors.IsNotFound(r.Get(ctx, ovnDBSyncCronJobKey, &batchv1.CronJob{}))).To(BeTrue())
	cond := neutronCondition(neutron, conditionTypeOVNDBSyncReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonOVNDBSyncNotRequired))
}

// TestReconcileOVNDBSync_DeleteFailurePropagates covers the error path of the
// opt-out: a CronJob that could not be deleted keeps running against a CR that
// no longer asks for it, so the failure has to surface rather than be reported
// as NotRequired.
func TestReconcileOVNDBSync_DeleteFailurePropagates(t *testing.T) {
	g := NewGomegaWithT(t)
	boom := errors.New("the API server refused the delete")
	neutron := validNeutron()
	c := neutronFakeClientBuilder(neutron).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if _, isCronJob := obj.(*batchv1.CronJob); isCronJob {
					return boom
				}
				return cl.Delete(ctx, obj, opts...)
			},
		}).
		Build()
	r := &NeutronReconciler{Client: c, Scheme: testScheme(), Recorder: record.NewFakeRecorder(10)}

	_, err := r.reconcileOVNDBSync(context.Background(), r.Client, neutron, deploymentConfigMapName)

	g.Expect(err).To(MatchError(boom))
	g.Expect(err).To(MatchError(ContainSubstring("deleting ovn-db-sync CronJob:")))
	g.Expect(neutronCondition(neutron, conditionTypeOVNDBSyncReady)).To(BeNil())
}

// TestReconcileOVNDBSync_ProjectsTheCronJob pins the mode the utility runs in.
// The default is the read-only one: repair mode rewrites the Northbound
// database, which is not something a CR gets by leaving a field unset.
func TestReconcileOVNDBSync_ProjectsTheCronJob(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(*neutronv1alpha1.OVNDBSyncSpec)
		wantMode string
	}{
		{name: "default mode", wantMode: "log"},
		{
			name:     "repair mode",
			mutate:   func(s *neutronv1alpha1.OVNDBSyncSpec) { s.SyncMode = "repair" },
			wantMode: "repair",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			ctx := context.Background()
			neutron := syncingNeutron(tc.mutate)
			r := newNeutronTestReconciler(neutron)

			res, err := r.reconcileOVNDBSync(ctx, r.Client, neutron, deploymentConfigMapName)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(res.IsZero()).To(BeTrue())

			var cronJob batchv1.CronJob
			g.Expect(r.Get(ctx, ovnDBSyncCronJobKey, &cronJob)).To(Succeed())
			g.Expect(cronJob.Spec.Schedule).To(Equal(neutronv1alpha1.DefaultOVNDBSyncSchedule))
			g.Expect(cronJob.Spec.Suspend).To(Equal(ptr.To(false)))

			container := cronJob.Spec.JobTemplate.Spec.Template.Spec.Containers[0]
			g.Expect(container.Command).To(Equal([]string{
				"neutron-ovn-db-sync-util",
				"--config-file", "/etc/neutron/neutron.conf",
				"--config-file", "/etc/neutron/ml2_conf.ini",
				"--ovn-neutron_sync_mode", tc.wantMode,
			}))

			cond := neutronCondition(neutron, conditionTypeOVNDBSyncReady)
			g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(cond.Reason).To(Equal(conditionReasonOVNDBSyncScheduled))
			g.Expect(cond.Message).To(ContainSubstring(tc.wantMode))
		})
	}
}

// TestReconcileOVNDBSync_SuspendPausesTheSchedule covers the maintenance-window
// escape hatch: the CronJob stays projected so resuming it is one field edit,
// and the condition says so under its own reason, because a paused comparison
// raises no other signal.
func TestReconcileOVNDBSync_SuspendPausesTheSchedule(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	neutron := syncingNeutron(func(s *neutronv1alpha1.OVNDBSyncSpec) { s.Suspend = true })
	r := newNeutronTestReconciler(neutron)

	_, err := r.reconcileOVNDBSync(ctx, r.Client, neutron, deploymentConfigMapName)
	g.Expect(err).NotTo(HaveOccurred())

	var cronJob batchv1.CronJob
	g.Expect(r.Get(ctx, ovnDBSyncCronJobKey, &cronJob)).To(Succeed())
	g.Expect(cronJob.Spec.Suspend).To(Equal(ptr.To(true)))
	cond := neutronCondition(neutron, conditionTypeOVNDBSyncReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonOVNDBSyncSuspended))
}

// TestReconcileOVNDBSync_FailedRun covers the failure path end to end: the
// condition names the Job and carries the reading of the log line that means the
// Northbound was unreachable, a Warning event repeats it, and the run is counted
// exactly once no matter how many passes observe the same terminal Job.
func TestReconcileOVNDBSync_FailedRun(t *testing.T) {
	g := NewGomegaWithT(t)
	g.Expect(neutronmetrics.Register()).To(Succeed())
	ctx := context.Background()

	neutron := syncingNeutron(nil)
	neutron.Name = "ovn-db-sync-failed"
	t.Cleanup(func() { neutronmetrics.DeleteForNeutron(neutron.Name, neutron.Namespace) })

	cronJob := seededOVNDBSyncCronJob(neutron)
	failed := ovnDBSyncRunJob(neutron, cronJob, neutron.Name+"-ovn-db-sync-28000000",
		batchv1.JobFailed, metav1.Now())
	r := newNeutronTestReconciler(neutron, cronJob, failed)
	recorder, ok := r.Recorder.(*record.FakeRecorder)
	g.Expect(ok).To(BeTrue())

	for range 3 {
		_, err := r.reconcileOVNDBSync(ctx, r.Client, neutron, deploymentConfigMapName)
		g.Expect(err).NotTo(HaveOccurred(),
			"a failed run is a status signal, not a reconcile error: retrying the pass cannot fix it")
	}

	cond := neutronCondition(neutron, conditionTypeOVNDBSyncReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonOVNDBSyncJobFailed))
	g.Expect(cond.Message).To(ContainSubstring(failed.Name))
	g.Expect(cond.Message).To(ContainSubstring("Could not retrieve schema from <address>"))

	g.Expect(collectEvents(recorder)).To(ContainElement(And(
		ContainSubstring(corev1.EventTypeWarning),
		ContainSubstring(conditionReasonOVNDBSyncJobFailed),
		ContainSubstring(failed.Name),
	)))

	g.Expect(neutron.Annotations).To(HaveKey(job.JobUIDAnnotationKey(componentOVNDBSync)),
		"the terminal metric must stamp the dedupe annotation so a run is counted once")
	g.Expect(counterValue(t, "neutron_operator_ovn_db_sync_total", map[string]string{
		"neutron": neutron.Name, "namespace": neutron.Namespace, "result": "failed",
	})).To(Equal(1.0))
	g.Expect(counterValue(t, "neutron_operator_db_sync_total", map[string]string{
		"neutron": neutron.Name, "namespace": neutron.Namespace, "result": "failed",
	})).To(Equal(0.0), "the ovn-db-sync run must not be counted as a schema migration")
}

// TestReconcileOVNDBSync_SuspendOutranksAFailedRun covers the ordering: a
// suspended CronJob spawns no successor, so the failed Job stays the newest
// terminal one for good and a JobFailed arm winning here would pin the aggregate
// Ready False until someone deleted the Job by hand.
func TestReconcileOVNDBSync_SuspendOutranksAFailedRun(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := syncingNeutron(func(s *neutronv1alpha1.OVNDBSyncSpec) { s.Suspend = true })
	cronJob := seededOVNDBSyncCronJob(neutron)
	failed := ovnDBSyncRunJob(neutron, cronJob, neutron.Name+"-ovn-db-sync-28000000",
		batchv1.JobFailed, metav1.Now())
	r := newNeutronTestReconciler(neutron, cronJob, failed)

	_, err := r.reconcileOVNDBSync(context.Background(), r.Client, neutron, deploymentConfigMapName)
	g.Expect(err).NotTo(HaveOccurred())

	cond := neutronCondition(neutron, conditionTypeOVNDBSyncReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonOVNDBSyncSuspended))
	g.Expect(cond.Message).To(ContainSubstring(failed.Name),
		"the failure is still named, it is just not the state the CR is in")
}

// TestReconcileOVNDBSync_NewestTerminalRunWins covers the recovery: an older
// failure must not hold the condition False once a later run has succeeded, or a
// single unreachable-database night would wedge Ready until the CronJob's
// history limit pruned the failure away.
func TestReconcileOVNDBSync_NewestTerminalRunWins(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := syncingNeutron(nil)
	cronJob := seededOVNDBSyncCronJob(neutron)
	now := metav1.Now()
	older := ovnDBSyncRunJob(neutron, cronJob, neutron.Name+"-ovn-db-sync-28000000",
		batchv1.JobFailed, metav1.NewTime(now.Add(-time.Hour)))
	newer := ovnDBSyncRunJob(neutron, cronJob, neutron.Name+"-ovn-db-sync-28000060",
		batchv1.JobComplete, now)
	r := newNeutronTestReconciler(neutron, cronJob, older, newer)

	_, err := r.reconcileOVNDBSync(context.Background(), r.Client, neutron, deploymentConfigMapName)
	g.Expect(err).NotTo(HaveOccurred())

	cond := neutronCondition(neutron, conditionTypeOVNDBSyncReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonOVNDBSyncScheduled))
}

// The reconcile lists Jobs by label alone, so metav1.IsControlledBy is the only
// thing separating this CronJob's runs from any other Job carrying the same
// labels — and `kubectl create job --from=cronjob/...` produces exactly such a
// Job, with the labels copied but no controller reference.
func TestReconcileOVNDBSync_IgnoresJobsItDoesNotControl(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := syncingNeutron(nil)
	cronJob := seededOVNDBSyncCronJob(neutron)
	foreign := ovnDBSyncRunJob(neutron, cronJob, neutron.Name+"-ovn-db-sync-manual",
		batchv1.JobFailed, metav1.Now())
	foreign.OwnerReferences = nil
	r := newNeutronTestReconciler(neutron, cronJob, foreign)

	_, err := r.reconcileOVNDBSync(context.Background(), r.Client, neutron, deploymentConfigMapName)
	g.Expect(err).NotTo(HaveOccurred())

	cond := neutronCondition(neutron, conditionTypeOVNDBSyncReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonOVNDBSyncScheduled))
	g.Expect(neutron.Annotations).NotTo(HaveKey(job.JobUIDAnnotationKey(componentOVNDBSync)),
		"a foreign run must not consume the dedupe annotation of a run this operator scheduled")
}
