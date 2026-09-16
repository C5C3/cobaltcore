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
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
)

// readySchedulerDeployment returns the scheduler Deployment the step builds,
// with the status of a completed rollout.
func readySchedulerDeployment(nova *novav1alpha1.Nova) *appsv1.Deployment {
	return markDeploymentRolledOut(
		buildSchedulerDeployment(nova, workloadArtifacts(), workloadDigests{}, testEgressPort))
}

// mountPaths returns the mount paths of a container, in order.
func mountPaths(mounts []corev1.VolumeMount) []string {
	paths := make([]string, 0, len(mounts))
	for _, mount := range mounts {
		paths = append(paths, mount.MountPath)
	}
	return paths
}

// TestBuildSchedulerDeployment covers what separates the scheduler from the two
// front ends: the second config directory carrying its worker count, the command
// that loads both, and a readiness signal taken off the message bus because it
// serves no HTTP port.
func TestBuildSchedulerDeployment(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()

	deploy := buildSchedulerDeployment(nova, workloadArtifacts(), workloadTestDigests(), testEgressPort)
	container := deploy.Spec.Template.Spec.Containers[0]

	g.Expect(deploy.Name).To(Equal("nova-scheduler"))
	g.Expect(deploy.Name).To(Equal(schedulerName(nova)))
	g.Expect(deploy.Labels).To(HaveKeyWithValue("app.kubernetes.io/component", componentScheduler))
	g.Expect(deploy.Spec.Selector.MatchLabels).To(Equal(
		componentSelectorLabels(nova, componentScheduler)))
	g.Expect(deploy.Spec.Template.Annotations).To(
		HaveKeyWithValue(installedReleaseAnnotation, "2025.2"))
	g.Expect(deploy.Spec.Template.Annotations).NotTo(HaveKey(metadataSecretHashAnnotation))

	g.Expect(container.Command).To(Equal([]string{
		"nova-scheduler",
		"--config-dir", "/etc/nova/nova.conf.d",
		"--config-dir", "/etc/nova/scheduler.conf.d",
	}))
	g.Expect(mountPaths(container.VolumeMounts)).To(Equal([]string{
		novaConfigDir, "/etc/nova/scheduler.conf.d", novaStatePath, tmpMountPath,
	}))

	g.Expect(container.Ports).To(BeEmpty(), "the scheduler takes its work off the bus")
	g.Expect(container.LivenessProbe).To(BeNil(),
		"a restart would not bring a disconnected broker back")
	g.Expect(container.StartupProbe).To(BeNil())
	g.Expect(container.ReadinessProbe).To(Equal(&corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			Exec: &corev1.ExecAction{Command: []string{"/var/lib/openstack/bin/nova-amqp-ready"}},
		},
		PeriodSeconds:    5,
		FailureThreshold: 1,
		TimeoutSeconds:   5,
	}))

	g.Expect(container.Env).To(ContainElement(corev1.EnvVar{Name: "NOVA_AMQP_PORT", Value: "5672"}))
	g.Expect(envNames(container.Env)).NotTo(ContainElements(
		novaConfigDirEnvVarName, novaConfigFilesEnvVarName),
		"a console script reads its documents through --config-dir, not through the environment")
	g.Expect(envNames(container.Env)).NotTo(ContainElement(metadataSharedSecretEnvVarName))
}

// TestBuildRoleDeployments_KeepTheirOwnReplicaCount covers the autoscaling
// boundary: the HPA in spec.autoscaling targets the API Deployment alone, so each
// of the other four roles must keep the replica count of its own block rather
// than leaving the field to an autoscaler that never writes it. A role that
// handed the field over would be created at one replica and stay there whatever
// its block asks for.
func TestBuildRoleDeployments_KeepTheirOwnReplicaCount(t *testing.T) {
	art := workloadArtifacts()
	cases := []struct {
		name     string
		replicas func(*novav1alpha1.Nova) *int32
		build    func(*novav1alpha1.Nova) *appsv1.Deployment
	}{
		{
			name:     "scheduler",
			replicas: func(nova *novav1alpha1.Nova) *int32 { return &nova.Spec.Scheduler.Deployment.Replicas },
			build: func(nova *novav1alpha1.Nova) *appsv1.Deployment {
				return buildSchedulerDeployment(nova, art, workloadDigests{}, testEgressPort)
			},
		},
		{
			name:     "conductor",
			replicas: func(nova *novav1alpha1.Nova) *int32 { return &nova.Spec.Conductor.Deployment.Replicas },
			build: func(nova *novav1alpha1.Nova) *appsv1.Deployment {
				return buildConductorDeployment(nova, art, workloadDigests{}, testEgressPort)
			},
		},
		{
			name:     "metadata",
			replicas: func(nova *novav1alpha1.Nova) *int32 { return &nova.Spec.Metadata.Deployment.Replicas },
			build: func(nova *novav1alpha1.Nova) *appsv1.Deployment {
				return buildMetadataDeployment(nova, art, workloadDigests{})
			},
		},
		{
			name:     "console proxy",
			replicas: func(nova *novav1alpha1.Nova) *int32 { return &nova.Spec.ConsoleProxy.Deployment.Replicas },
			build: func(nova *novav1alpha1.Nova) *appsv1.Deployment {
				return buildConsoleProxyDeployment(nova, art, workloadDigests{})
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			nova := validNova()
			nova.Spec.Autoscaling = &novav1alpha1.AutoscalingSpec{MaxReplicas: 5}
			*tc.replicas(nova) = 2

			deploy := tc.build(nova)

			g.Expect(deploy.Spec.Replicas).NotTo(BeNil())
			g.Expect(*deploy.Spec.Replicas).To(Equal(int32(2)))
		})
	}
}

// TestReconcileScheduler_ReturnsZeroOutsideAnUpgrade covers the result contract:
// the API step runs after this one, so a scheduler that is still starting must
// report its state without holding the pass back.
func TestReconcileScheduler_ReturnsZeroOutsideAnUpgrade(t *testing.T) {
	ctx := context.Background()

	t.Run("a scheduler still rolling out", func(t *testing.T) {
		g := NewGomegaWithT(t)
		nova := validNova()
		r := newNovaTestReconciler(nova)

		res, err := r.reconcileScheduler(ctx, r.Client, nova, workloadArtifacts(),
			workloadTestDigests(), testEgressPort)

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.IsZero()).To(BeTrue())
		g.Expect(r.Get(ctx, objectKey("nova-scheduler"), &appsv1.Deployment{})).To(Succeed())
		g.Expect(r.Get(ctx, objectKey("nova-scheduler"), &corev1.Service{})).NotTo(Succeed(),
			"the scheduler serves no port, so no Service is projected")
		cond := novaCondition(nova, "SchedulerReady")
		g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForScheduler))
	})

	t.Run("an available scheduler", func(t *testing.T) {
		g := NewGomegaWithT(t)
		nova := validNova()
		r := newNovaTestReconciler(nova, readySchedulerDeployment(nova))

		res, err := r.reconcileScheduler(ctx, r.Client, nova, workloadArtifacts(),
			workloadDigests{}, testEgressPort)

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.IsZero()).To(BeTrue())
		cond := novaCondition(nova, "SchedulerReady")
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
		nova := rollingUpdateNova()
		surging := readySchedulerDeployment(nova)
		surging.Status.Replicas++
		r := newNovaTestReconciler(nova, surging)

		res, err := r.reconcileScheduler(ctx, r.Client, nova, workloadArtifacts(),
			workloadDigests{}, testEgressPort)

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueDeploymentPolling))
		cond := novaCondition(nova, "SchedulerReady")
		g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForScheduler))
	})

	t.Run("a converged rollout releases the pass", func(t *testing.T) {
		g := NewGomegaWithT(t)
		nova := rollingUpdateNova()
		r := newNovaTestReconciler(nova, readySchedulerDeployment(nova))

		res, err := r.reconcileScheduler(ctx, r.Client, nova, workloadArtifacts(),
			workloadDigests{}, testEgressPort)

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.IsZero()).To(BeTrue())
		g.Expect(novaCondition(nova, "SchedulerReady").Status).To(Equal(metav1.ConditionTrue))
	})
}

// TestReconcileScheduler_ApplyFailureWrapsTheError covers the error path: the
// message names the scheduler, so a pipeline error is not confused with another
// role's Deployment.
func TestReconcileScheduler_ApplyFailureWrapsTheError(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	boom := errors.New("quota exceeded")
	r := failingApplyReconciler(boom, "Deployment", schedulerName(nova), nova)

	_, err := r.reconcileScheduler(context.Background(), r.Client, nova, workloadArtifacts(),
		workloadDigests{}, testEgressPort)

	g.Expect(err).To(MatchError(boom))
	g.Expect(err).To(MatchError(ContainSubstring("ensuring scheduler Deployment:")))
}

// TestAMQPPortEnv covers the probe's input: the port of the transport URL the
// messaging step resolved, so a broker that does not listen on 5672 is still
// measured on the port nova dials.
func TestAMQPPortEnv(t *testing.T) {
	g := NewGomegaWithT(t)

	g.Expect(amqpPortEnv(5671)).To(Equal(corev1.EnvVar{Name: "NOVA_AMQP_PORT", Value: "5671"}))
	g.Expect(amqpPortEnv(0)).To(Equal(corev1.EnvVar{Name: "NOVA_AMQP_PORT", Value: "0"}),
		"a messaging step that resolved no port stamps zero rather than nothing")
}
