// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

// WorkloadParams assembles the API Deployment for a service operator. Every
// field is caller-supplied and rendered verbatim; the invariants the builder
// owns on its own are documented on BuildWorkload.
type WorkloadParams struct {
	// Namespace and Name address the Deployment. Callers pass the CR namespace
	// and the shared sub-resource name.
	Namespace string
	Name      string

	// Labels are stamped on both the Deployment and the pod template. They are
	// the full common label set, a superset of SelectorLabels.
	Labels map[string]string

	// SelectorLabels are the immutable pod selector: they become
	// .spec.selector.matchLabels and the label selector of the default topology
	// spread constraints.
	SelectorLabels map[string]string

	// PodAnnotations are stamped on the pod template verbatim. Nil leaves the
	// template annotation-free, so an operator that stamps no rollout-triggering
	// content hash renders exactly the same template as before.
	PodAnnotations map[string]string

	// Deployment is the CR's shared deployment knob block. It must be non-nil
	// (see BuildWorkload).
	Deployment *commonv1.DeploymentSpec

	// Autoscaling is the CR's autoscaling block, nil when no HPA is requested.
	// A non-nil value leaves .spec.replicas unmanaged.
	Autoscaling *commonv1.AutoscalingSpec

	// Container is the primary API container.
	Container ContainerParams

	// Volumes are the pod volumes, rendered verbatim. Nil stays nil.
	Volumes []corev1.Volume
}

// ContainerParams is the primary API container. Every field is passed through
// verbatim, nil included: a nil StartupProbe renders no startup probe, a nil
// Env renders no env block. The builder adds only the three container-level
// invariants documented on BuildWorkload (resources, security context, preStop
// hook).
type ContainerParams struct {
	Name    string
	Image   string
	Command []string

	Env []corev1.EnvVar

	Ports []corev1.ContainerPort

	LivenessProbe  *corev1.Probe
	ReadinessProbe *corev1.Probe
	StartupProbe   *corev1.Probe

	VolumeMounts []corev1.VolumeMount
}

// BuildWorkload renders the API Deployment shared by every service operator.
// It owns exactly the invariants the operators would otherwise each repeat:
//
//   - .spec.replicas from DeploymentReplicas, so an HPA-owned count stays
//     unmanaged;
//   - .spec.selector from p.SelectorLabels and .spec.strategy from Strategy;
//   - the pod-level knobs TerminationGracePeriodSeconds,
//     TopologySpreadConstraints, and PriorityClassName, plus the FSGroup pod
//     security context;
//   - on the single container, ContainerResources, RestrictedSecurityContext,
//     and the preStop sleep hook from PreStopSleepCommand.
//
// Everything else — image, command, env, ports, probes, volumes, and mounts —
// comes from p and is rendered verbatim, nilness included, so a service that
// has no startup probe or no volumes renders none.
//
// p.Deployment must be non-nil; every caller passes &cr.Spec.Deployment. The
// builder does not nil-check it, matching the contract of the knob helpers it
// calls (Strategy, ContainerResources, and friends all dereference the spec).
//
// BuildWorkload is a total function over valid params: it performs no I/O and
// has no error paths.
func BuildWorkload(p WorkloadParams) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      p.Name,
			Namespace: p.Namespace,
			Labels:    p.Labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: DeploymentReplicas(p.Deployment, p.Autoscaling),
			Selector: &metav1.LabelSelector{
				MatchLabels: p.SelectorLabels,
			},
			Strategy: Strategy(p.Deployment),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      p.Labels,
					Annotations: p.PodAnnotations,
				},
				Spec: corev1.PodSpec{
					TerminationGracePeriodSeconds: ptr.To(TerminationGracePeriodSeconds(p.Deployment)),
					TopologySpreadConstraints:     TopologySpreadConstraints(p.Deployment, p.SelectorLabels),
					PriorityClassName:             PriorityClassName(p.Deployment),
					SecurityContext:               &corev1.PodSecurityContext{FSGroup: ptr.To(OpenStackUID)},
					Containers: []corev1.Container{{
						Name:            p.Container.Name,
						Image:           p.Container.Image,
						Resources:       ContainerResources(p.Deployment),
						SecurityContext: RestrictedSecurityContext(),
						Command:         p.Container.Command,
						Env:             p.Container.Env,
						Ports:           p.Container.Ports,
						LivenessProbe:   p.Container.LivenessProbe,
						ReadinessProbe:  p.Container.ReadinessProbe,
						StartupProbe:    p.Container.StartupProbe,
						Lifecycle: &corev1.Lifecycle{
							PreStop: &corev1.LifecycleHandler{
								Exec: &corev1.ExecAction{
									Command: PreStopSleepCommand(p.Deployment),
								},
							},
						},
						VolumeMounts: p.Container.VolumeMounts,
					}},
					Volumes: p.Volumes,
				},
			},
		},
	}
}

// BuildService renders the API Service shared by every service operator: the
// labels and selector the caller supplies plus exactly one unnamed TCP port.
// Splitting port from targetPort lets a service keep its stable Service port
// while routing traffic to a sidecar, the way Keystone does with federation
// active.
//
// BuildService is a total function over valid params: it performs no I/O and
// has no error paths.
func BuildService(namespace, name string, labels, selector map[string]string, port, targetPort int32) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Selector: selector,
			Ports: []corev1.ServicePort{{
				Port:       port,
				TargetPort: intstr.FromInt32(targetPort),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
}
