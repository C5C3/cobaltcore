// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"testing"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	cinderv1alpha1 "github.com/c5c3/cobaltcore/operators/cinder/api/v1alpha1"
)

// readySchedulerDeployment returns the scheduler Deployment the step builds,
// with the status of a completed rollout.
func readySchedulerDeployment(cinder *cinderv1alpha1.Cinder) *appsv1.Deployment {
	return markDeploymentRolledOut(
		buildSchedulerDeployment(cinder, workloadArtifacts(), workloadDigests{}, testEgressPort))
}

// TestBuildSchedulerDeployment covers what separates the scheduler from the API:
// the second config directory carrying its host overlay, the command that loads
// both, and a readiness signal taken off the message bus because it serves no
// HTTP port.
func TestBuildSchedulerDeployment(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := workloadCinder()

	deploy := buildSchedulerDeployment(cinder, workloadArtifacts(), workloadDigests{}, testEgressPort)
	container := deploy.Spec.Template.Spec.Containers[0]

	g.Expect(deploy.Name).To(Equal("cinder-scheduler"))
	g.Expect(deploy.Name).To(Equal(schedulerName(cinder)))
	g.Expect(container.Command).To(Equal([]string{
		"cinder-scheduler",
		"--config-dir", "/etc/cinder/cinder.conf.d",
		"--config-dir", "/etc/cinder/scheduler.conf.d",
	}))

	overlay := deploy.Spec.Template.Spec.Volumes[len(deploy.Spec.Template.Spec.Volumes)-1]
	g.Expect(overlay.Name).To(Equal(schedulerConfigVolumeName))
	g.Expect(overlay.ConfigMap.Name).To(Equal("cinder-config-abc123"))
	g.Expect(overlay.ConfigMap.Items).To(Equal([]corev1.KeyToPath{
		{Key: schedulerConfDataKey, Path: schedulerConfDataKey},
	}), "the overlay directory holds the host identity and nothing else")
	g.Expect(container.VolumeMounts[len(container.VolumeMounts)-1]).To(Equal(corev1.VolumeMount{
		Name: schedulerConfigVolumeName, MountPath: cinderSchedulerConfigDir, ReadOnly: true,
	}))

	g.Expect(container.Ports).To(BeEmpty(), "the scheduler takes its work off the bus")
	g.Expect(container.LivenessProbe).To(BeNil(),
		"a restart would not bring a disconnected broker back")
	g.Expect(container.StartupProbe).To(BeNil())
	g.Expect(container.ReadinessProbe.Exec.Command).To(Equal(
		[]string{"/var/lib/openstack/bin/cinder-amqp-ready"}))
	g.Expect(container.ReadinessProbe.PeriodSeconds).To(Equal(int32(5)))
	g.Expect(container.ReadinessProbe.FailureThreshold).To(Equal(int32(2)))
	g.Expect(container.ReadinessProbe.TimeoutSeconds).To(Equal(int32(5)))
	g.Expect(container.Env).To(ContainElement(corev1.EnvVar{Name: "CINDER_AMQP_PORT", Value: "5672"}))
}

// TestBuildSchedulerDeployment_KeepsItsOwnReplicaCount covers the autoscaling
// boundary: the HPA in spec.autoscaling targets the API Deployment alone, so the
// scheduler must keep an explicit replica count rather than leaving the field to
// an autoscaler that never writes it.
func TestBuildSchedulerDeployment_KeepsItsOwnReplicaCount(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := workloadCinder()
	cinder.Spec.Autoscaling = &cinderv1alpha1.AutoscalingSpec{MaxReplicas: 5}

	deploy := buildSchedulerDeployment(cinder, workloadArtifacts(), workloadDigests{}, testEgressPort)

	g.Expect(deploy.Spec.Replicas).NotTo(BeNil())
	g.Expect(*deploy.Spec.Replicas).To(Equal(int32(1)))
}

// TestBuildSchedulerDeployment_StampsTheInstalledRelease covers the RPC-version
// stamp: it tracks status.installedRelease once a db-sync has promoted one, and
// falls back to the requested release on a fresh install so the first pods do
// not roll again the moment the marker appears.
func TestBuildSchedulerDeployment_StampsTheInstalledRelease(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := workloadCinder()

	fresh := buildSchedulerDeployment(cinder, workloadArtifacts(), workloadDigests{}, testEgressPort)
	g.Expect(fresh.Spec.Template.Annotations).To(HaveKeyWithValue(installedReleaseAnnotation, "2026.1"))

	cinder.Status.InstalledRelease = "2025.2"
	installed := buildSchedulerDeployment(cinder, workloadArtifacts(), workloadDigests{}, testEgressPort)
	g.Expect(installed.Spec.Template.Annotations).To(HaveKeyWithValue(installedReleaseAnnotation, "2025.2"))
}

// TestReconcileScheduler_ReturnsZeroOutsideAnUpgrade covers the result contract:
// the API step runs after this one, so a scheduler that is still starting must
// report its state without holding the pass back. The Owns(Deployment) watch
// re-enqueues the CR when the rollout progresses.
func TestReconcileScheduler_ReturnsZeroOutsideAnUpgrade(t *testing.T) {
	ctx := context.Background()

	t.Run("a scheduler still rolling out", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cinder := workloadCinder()
		r := newCinderTestReconciler(cinder)

		res, err := r.reconcileScheduler(ctx, r.Client, cinder, workloadArtifacts(),
			workloadTestDigests(), testEgressPort)

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.IsZero()).To(BeTrue())
		g.Expect(r.Get(ctx, objectKey("cinder-scheduler"), &appsv1.Deployment{})).To(Succeed())
		cond := cinderCondition(cinder, "SchedulerReady")
		g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForScheduler))
	})

	t.Run("an available scheduler", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cinder := workloadCinder()
		r := newCinderTestReconciler(cinder, readySchedulerDeployment(cinder))

		res, err := r.reconcileScheduler(ctx, r.Client, cinder, workloadArtifacts(),
			workloadDigests{}, testEgressPort)

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.IsZero()).To(BeTrue())
		cond := cinderCondition(cinder, "SchedulerReady")
		g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(cond.Reason).To(Equal(conditionReasonSchedulerReady))
	})
}

// TestReconcileScheduler_RollingUpdateHoldsUntilConverged covers the upgrade
// gate: the contract phase runs data migrations the old scheduler has no code
// for, so the pass polls until every replica runs the new image.
func TestReconcileScheduler_RollingUpdateHoldsUntilConverged(t *testing.T) {
	ctx := context.Background()

	t.Run("a surge pod still running holds the pass", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cinder := rollingUpdateCinder()
		surging := readySchedulerDeployment(cinder)
		surging.Status.Replicas++
		r := newCinderTestReconciler(cinder, surging)

		res, err := r.reconcileScheduler(ctx, r.Client, cinder, workloadArtifacts(),
			workloadDigests{}, testEgressPort)

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueDeploymentPolling))
		cond := cinderCondition(cinder, "SchedulerReady")
		g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForScheduler))
	})

	t.Run("a converged rollout releases the pass", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cinder := rollingUpdateCinder()
		r := newCinderTestReconciler(cinder, readySchedulerDeployment(cinder))

		res, err := r.reconcileScheduler(ctx, r.Client, cinder, workloadArtifacts(),
			workloadDigests{}, testEgressPort)

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.IsZero()).To(BeTrue())
		g.Expect(cinderCondition(cinder, "SchedulerReady").Status).To(Equal(metav1.ConditionTrue))
	})
}

// TestReconcileScheduler_ApplyFailureWrapsTheError covers the error path: the
// message names the scheduler, so a pipeline error is not confused with the API
// Deployment's.
func TestReconcileScheduler_ApplyFailureWrapsTheError(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := workloadCinder()
	boom := errors.New("quota exceeded")
	r := failingApplyReconciler(boom, "Deployment", schedulerName(cinder), cinder)

	_, err := r.reconcileScheduler(context.Background(), r.Client, cinder, workloadArtifacts(),
		workloadDigests{}, testEgressPort)

	g.Expect(err).To(MatchError(boom))
	g.Expect(err).To(MatchError(ContainSubstring("ensuring scheduler Deployment:")))
}
