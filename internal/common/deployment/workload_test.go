// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"testing"

	"github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	commonv1 "github.com/c5c3/forge/internal/common/types"
)

// workloadParams is the minimal valid params set: a non-nil deployment spec
// (the documented precondition) and the primary container's identity. Each
// test overrides only the field it exercises.
func workloadParams() WorkloadParams {
	return WorkloadParams{
		Namespace:      "ns",
		Name:           "test-svc",
		Labels:         map[string]string{"app.kubernetes.io/name": "svc", "app.kubernetes.io/instance": "test-svc"},
		SelectorLabels: map[string]string{"app.kubernetes.io/name": "svc"},
		Deployment:     &commonv1.DeploymentSpec{Replicas: 3},
		Container: ContainerParams{
			Name:  "svc-api",
			Image: "registry.example.com/svc:2026.1",
		},
	}
}

// Nil pod annotations must leave the template annotation-free: an operator
// that stamps no rollout-triggering hash must not gain an empty annotation map
// that would churn the pod template.
func TestBuildWorkload_PodAnnotations(t *testing.T) {
	g := gomega.NewWithT(t)

	deploy := BuildWorkload(workloadParams())
	g.Expect(deploy.Spec.Template.Annotations).To(gomega.BeNil())

	annotations := map[string]string{"svc.c5c3.io/db-connection-hash": "abc123"}
	p := workloadParams()
	p.PodAnnotations = annotations
	deploy = BuildWorkload(p)
	g.Expect(deploy.Spec.Template.Annotations).To(gomega.Equal(annotations))
}

// Every container field passes through verbatim, nil included: a service
// without a startup probe renders no startup probe, and the optional slices
// stay nil rather than becoming empty slices that would serialize differently.
func TestBuildWorkload_NilContainerFieldsStayNil(t *testing.T) {
	g := gomega.NewWithT(t)

	deploy := BuildWorkload(workloadParams())
	container := deploy.Spec.Template.Spec.Containers[0]

	g.Expect(container.StartupProbe).To(gomega.BeNil())
	g.Expect(container.LivenessProbe).To(gomega.BeNil())
	g.Expect(container.ReadinessProbe).To(gomega.BeNil())
	g.Expect(container.Env).To(gomega.BeNil())
	g.Expect(container.VolumeMounts).To(gomega.BeNil())
	g.Expect(deploy.Spec.Template.Spec.Volumes).To(gomega.BeNil())

	// A supplied startup probe is rendered untouched.
	p := workloadParams()
	p.Container.StartupProbe = &corev1.Probe{FailureThreshold: 30, PeriodSeconds: 10}
	deploy = BuildWorkload(p)
	container = deploy.Spec.Template.Spec.Containers[0]

	g.Expect(container.StartupProbe).To(gomega.Equal(p.Container.StartupProbe))
}

// While an HPA owns the replica count the Deployment must leave .spec.replicas
// unmanaged, otherwise operator and HPA fight over the field.
func TestBuildWorkload_ReplicasFollowAutoscaling(t *testing.T) {
	g := gomega.NewWithT(t)

	p := workloadParams()
	p.Autoscaling = &commonv1.AutoscalingSpec{MaxReplicas: 5}
	g.Expect(BuildWorkload(p).Spec.Replicas).To(gomega.BeNil())

	p.Autoscaling = nil
	g.Expect(BuildWorkload(p).Spec.Replicas).To(gomega.HaveValue(gomega.Equal(int32(3))))

	// A webhook-bypassed spec (zero replicas) normalizes to the shared default
	// instead of scaling the Deployment to zero pods.
	p.Deployment = &commonv1.DeploymentSpec{}
	g.Expect(BuildWorkload(p).Spec.Replicas).To(gomega.HaveValue(gomega.Equal(commonv1.DefaultReplicas)))
}

// The invariants the builder owns on its own — selector, default strategy, pod
// and container security contexts, resources, and the preStop hook — must be
// present without the caller asking for them.
func TestBuildWorkload_OwnsInvariants(t *testing.T) {
	g := gomega.NewWithT(t)

	resources := corev1.ResourceRequirements{Limits: corev1.ResourceList{}}
	p := workloadParams()
	p.Deployment = &commonv1.DeploymentSpec{Replicas: 3, Resources: &resources}
	deploy := BuildWorkload(p)

	g.Expect(deploy.Name).To(gomega.Equal("test-svc"))
	g.Expect(deploy.Namespace).To(gomega.Equal("ns"))
	g.Expect(deploy.Labels).To(gomega.Equal(p.Labels))
	g.Expect(deploy.Spec.Template.Labels).To(gomega.Equal(p.Labels))
	g.Expect(deploy.Spec.Selector.MatchLabels).To(gomega.Equal(p.SelectorLabels))

	// spec.strategy is nil: the surge-before-remove default keeps available
	// capacity from dropping during a rolling image-tag patch.
	g.Expect(deploy.Spec.Strategy.Type).To(gomega.Equal(appsv1.RollingUpdateDeploymentStrategyType))
	g.Expect(deploy.Spec.Strategy.RollingUpdate.MaxUnavailable).To(gomega.HaveValue(gomega.Equal(intstr.FromInt32(0))))
	g.Expect(deploy.Spec.Strategy.RollingUpdate.MaxSurge).To(gomega.HaveValue(gomega.Equal(intstr.FromInt32(1))))

	podSpec := deploy.Spec.Template.Spec
	g.Expect(podSpec.SecurityContext.FSGroup).To(gomega.HaveValue(gomega.Equal(OpenStackUID)))
	g.Expect(podSpec.TerminationGracePeriodSeconds).To(gomega.HaveValue(gomega.Equal(commonv1.DefaultTerminationGracePeriodSeconds)))
	g.Expect(podSpec.TopologySpreadConstraints).To(gomega.Equal(TopologySpreadConstraints(p.Deployment, p.SelectorLabels)))
	g.Expect(podSpec.PriorityClassName).To(gomega.Equal(""))

	g.Expect(podSpec.Containers).To(gomega.HaveLen(1))
	container := podSpec.Containers[0]
	g.Expect(container.Name).To(gomega.Equal("svc-api"))
	g.Expect(container.Image).To(gomega.Equal("registry.example.com/svc:2026.1"))
	g.Expect(container.SecurityContext).To(gomega.Equal(RestrictedSecurityContext()))
	g.Expect(container.Resources).To(gomega.Equal(resources))
	g.Expect(container.Lifecycle.PreStop.Exec.Command).To(gomega.Equal(PreStopSleepCommand(p.Deployment)))
}

// The Service port and the target port are independent: Keystone keeps the
// Service on 5000 while routing to the federation sidecar port.
func TestBuildService_PortAndTargetPortSplit(t *testing.T) {
	g := gomega.NewWithT(t)

	labels := map[string]string{"app.kubernetes.io/name": "svc", "app.kubernetes.io/instance": "test-svc"}
	selector := map[string]string{"app.kubernetes.io/name": "svc"}
	svc := BuildService("ns", "test-svc", labels, selector, 5000, 8443)

	g.Expect(svc.Name).To(gomega.Equal("test-svc"))
	g.Expect(svc.Namespace).To(gomega.Equal("ns"))
	g.Expect(svc.Labels).To(gomega.Equal(labels))
	g.Expect(svc.Spec.Selector).To(gomega.Equal(selector))
	g.Expect(svc.Spec.Ports).To(gomega.HaveLen(1))
	g.Expect(svc.Spec.Ports[0]).To(gomega.Equal(corev1.ServicePort{
		Port:       5000,
		TargetPort: intstr.FromInt32(8443),
		Protocol:   corev1.ProtocolTCP,
	}))
	// The port stays unnamed: a named port would change the rendered Service.
	g.Expect(svc.Spec.Ports[0].Name).To(gomega.Equal(""))

	// The collapsed case (port == targetPort) renders the same shape.
	svc = BuildService("ns", "test-svc", nil, selector, 9292, 9292)
	g.Expect(svc.Labels).To(gomega.BeNil())
	g.Expect(svc.Spec.Ports[0].TargetPort).To(gomega.Equal(intstr.FromInt32(9292)))
}
