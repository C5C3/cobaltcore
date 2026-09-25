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
	"k8s.io/utils/ptr"

	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	"github.com/c5c3/cobaltcore/internal/common/testutil"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
)

// readyConductorDeployment returns the conductor Deployment the step builds,
// with the status of a completed rollout.
func readyConductorDeployment(nova *novav1alpha1.Nova) *appsv1.Deployment {
	return markDeploymentRolledOut(
		buildConductorDeployment(nova, workloadArtifacts(), workloadDigests{}, testEgressPort))
}

// TestBuildConductorDeployment covers the conductor's own shape: its overlay
// directory, the command that loads it beside the shared one, and the bus
// readiness probe it shares with the scheduler.
func TestBuildConductorDeployment(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()

	deploy := buildConductorDeployment(nova, workloadArtifacts(), workloadTestDigests(), testEgressPort)
	container := deploy.Spec.Template.Spec.Containers[0]

	g.Expect(deploy.Name).To(Equal("nova-conductor"))
	g.Expect(deploy.Name).To(Equal(conductorName(nova)))
	g.Expect(deploy.Labels).To(HaveKeyWithValue("app.kubernetes.io/component", componentConductor))
	g.Expect(deploy.Spec.Selector.MatchLabels).To(Equal(
		componentSelectorLabels(nova, componentConductor)))
	g.Expect(deploy.Spec.Template.Annotations).To(
		HaveKeyWithValue(installedReleaseAnnotation, "2025.2"))

	g.Expect(container.Command).To(Equal([]string{
		"nova-conductor",
		"--config-dir", "/etc/nova/nova.conf.d",
		"--config-dir", "/etc/nova/conductor.conf.d",
	}))
	g.Expect(mountPaths(container.VolumeMounts)).To(Equal([]string{
		novaConfigDir, "/etc/nova/conductor.conf.d", novaStatePath, tmpMountPath,
	}))

	g.Expect(container.Ports).To(BeEmpty(), "the conductor takes its work off the bus")
	g.Expect(container.LivenessProbe).To(BeNil())
	g.Expect(container.ReadinessProbe).To(Equal(amqpReadinessProbe()))
	g.Expect(container.Env).To(ContainElement(corev1.EnvVar{Name: "NOVA_AMQP_PORT", Value: "5672"}))
	g.Expect(envNames(container.Env)).To(ContainElements(
		"OS_API_DATABASE__CONNECTION", "OS_DATABASE__CONNECTION"),
		"the conductor writes the cell schema and reads the mappings in nova_api")
}

// TestReconcileConductor_ReturnsZeroOutsideAnUpgrade covers the result contract:
// a conductor that is still starting reports its state without holding the pass
// back.
func TestReconcileConductor_ReturnsZeroOutsideAnUpgrade(t *testing.T) {
	ctx := context.Background()

	t.Run("a conductor still rolling out", func(t *testing.T) {
		g := NewGomegaWithT(t)
		nova := validNova()
		r := newNovaTestReconciler(nova)

		res, err := r.reconcileConductor(ctx, r.Client, nova, workloadArtifacts(),
			workloadTestDigests(), testEgressPort)

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.IsZero()).To(BeTrue())
		g.Expect(r.Get(ctx, objectKey("nova-conductor"), &appsv1.Deployment{})).To(Succeed())
		cond := novaCondition(nova, "ConductorReady")
		g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForConductor))
	})

	t.Run("an available conductor", func(t *testing.T) {
		g := NewGomegaWithT(t)
		nova := validNova()
		r := newNovaTestReconciler(nova, readyConductorDeployment(nova))

		res, err := r.reconcileConductor(ctx, r.Client, nova, workloadArtifacts(),
			workloadDigests{}, testEgressPort)

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.IsZero()).To(BeTrue())
		cond := novaCondition(nova, "ConductorReady")
		g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(cond.Reason).To(Equal(conditionReasonConductorReady))
	})
}

// TestReconcileConductor_RollingUpdateHoldsUntilConverged covers the upgrade
// gate: the conductor writes the rows the API reads, so the contract phase must
// not run while a replica is still on the old image.
func TestReconcileConductor_RollingUpdateHoldsUntilConverged(t *testing.T) {
	ctx := context.Background()

	t.Run("a surge pod still running holds the pass", func(t *testing.T) {
		g := NewGomegaWithT(t)
		nova := rollingUpdateNova()
		surging := readyConductorDeployment(nova)
		surging.Status.Replicas++
		r := newNovaTestReconciler(nova, surging)

		res, err := r.reconcileConductor(ctx, r.Client, nova, workloadArtifacts(),
			workloadDigests{}, testEgressPort)

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueDeploymentPolling))
		cond := novaCondition(nova, "ConductorReady")
		g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForConductor))
	})

	t.Run("a converged rollout releases the pass", func(t *testing.T) {
		g := NewGomegaWithT(t)
		nova := rollingUpdateNova()
		r := newNovaTestReconciler(nova, readyConductorDeployment(nova))

		res, err := r.reconcileConductor(ctx, r.Client, nova, workloadArtifacts(),
			workloadDigests{}, testEgressPort)

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.IsZero()).To(BeTrue())
		g.Expect(novaCondition(nova, "ConductorReady").Status).To(Equal(metav1.ConditionTrue))
	})
}

// TestReconcileConductor_ApplyFailureWrapsTheError covers the error path: the
// message names the conductor, so a pipeline error is not confused with the
// scheduler's Deployment.
func TestReconcileConductor_ApplyFailureWrapsTheError(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	boom := errors.New("quota exceeded")
	r := failingApplyReconciler(boom, "Deployment", conductorName(nova), nova)

	_, err := r.reconcileConductor(context.Background(), r.Client, nova, workloadArtifacts(),
		workloadDigests{}, testEgressPort)

	g.Expect(err).To(MatchError(boom))
	g.Expect(err).To(MatchError(ContainSubstring("ensuring conductor Deployment:")))
}

// TestConductorAndSchedulerDoNotShareAnOverlay covers the separation the two
// overlay directories exist for: oslo.config reads every file in a --config-dir,
// so a conductor that saw the scheduler's document would take over its worker
// count.
func TestConductorAndSchedulerDoNotShareAnOverlay(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	art := workloadArtifacts()

	conductor := buildConductorDeployment(nova, art, workloadDigests{}, testEgressPort)
	scheduler := buildSchedulerDeployment(nova, art, workloadDigests{}, testEgressPort)

	g.Expect(overlayKeys(conductor)).To(Equal([]string{conductorConfDataKey}))
	g.Expect(overlayKeys(scheduler)).To(Equal([]string{schedulerConfDataKey}))
}

// overlayKeys returns the ConfigMap keys every volume of a Deployment projects
// except the shared config volume's.
func overlayKeys(deploy *appsv1.Deployment) []string {
	var keys []string
	for _, volume := range deploy.Spec.Template.Spec.Volumes {
		if volume.Name == configVolumeName || volume.ConfigMap == nil {
			continue
		}
		for _, item := range volume.ConfigMap.Items {
			keys = append(keys, item.Key)
		}
	}
	return keys
}

// TestBuildConductorDeployment_RendersResourceDefaults verifies that the
// conductor memory follows spec.conductor.workers, one single-threaded process
// per worker, beside a 100m CPU request and no CPU limit: 512Mi at the default
// two, 656Mi at three, and still 512Mi when only spec.scheduler.workers moves.
func TestBuildConductorDeployment_RendersResourceDefaults(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(nova *novav1alpha1.Nova)
		want   string
	}{
		{name: "default workers", mutate: func(*novav1alpha1.Nova) {}, want: "512Mi"},
		{
			name:   "three conductor workers",
			mutate: func(nova *novav1alpha1.Nova) { nova.Spec.Conductor.Workers = ptr.To(int32(3)) },
			want:   "656Mi",
		},
		{
			name:   "three scheduler workers",
			mutate: func(nova *novav1alpha1.Nova) { nova.Spec.Scheduler.Workers = ptr.To(int32(3)) },
			want:   "512Mi",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			nova := validNova()
			tc.mutate(nova)

			deploy := buildConductorDeployment(nova, workloadArtifacts(), workloadDigests{}, testEgressPort)

			g.Expect(deploy.Spec.Template.Spec.Containers[0].Resources).To(Equal(testutil.RenderedResourceDefaults(tc.want)))
		})
	}
}
