// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/apply"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

// DaemonSetParams assembles a node-level DaemonSet. Every field is
// caller-supplied and rendered verbatim; the two defaults the builder owns are
// documented on BuildDaemonSet.
type DaemonSetParams struct {
	// Namespace and Name address the DaemonSet.
	Namespace string
	Name      string

	// Labels are stamped on both the DaemonSet and the pod template. They are
	// the full common label set, a superset of SelectorLabels.
	Labels map[string]string

	// SelectorLabels are the immutable pod selector: they become
	// .spec.selector.matchLabels.
	SelectorLabels map[string]string

	// PodAnnotations are stamped on the pod template verbatim. Nil leaves the
	// template annotation-free.
	PodAnnotations map[string]string

	// UpdateStrategy replaces the builder's default rolling update. Nil renders
	// RollingUpdate with maxUnavailable 1 and no maxSurge.
	UpdateStrategy *appsv1.DaemonSetUpdateStrategy

	// NodeSelector picks the nodes the DaemonSet runs on. Nil selects every
	// schedulable node.
	NodeSelector map[string]string

	// Tolerations let the pod onto tainted nodes.
	Tolerations []corev1.Toleration

	// HostNetwork puts the pod in the node's network namespace. It also sets
	// the DNS policy to ClusterFirstWithHostNet, without which the pod resolves
	// against the node's resolv.conf and loses cluster DNS.
	HostNetwork bool

	// PriorityClassName and ServiceAccountName are rendered verbatim. Empty
	// leaves each to the namespace default.
	PriorityClassName  string
	ServiceAccountName string

	// TerminationGracePeriodSeconds overrides the shared default. Nil renders
	// commonv1.DefaultTerminationGracePeriodSeconds.
	TerminationGracePeriodSeconds *int64

	// PodSecurityContext is the pod-level security context, rendered verbatim.
	// Nil renders none: a node-level pod sets its posture per container, and an
	// FSGroup the API Deployments carry would apply to host mounts here.
	PodSecurityContext *corev1.PodSecurityContext

	// InitContainers and Containers are rendered verbatim, each container's own
	// SecurityContext included. Containers must be non-empty.
	InitContainers []corev1.Container
	Containers     []corev1.Container

	// Volumes are the pod volumes, rendered verbatim. Nil stays nil.
	Volumes []corev1.Volume
}

// BuildDaemonSet renders a node-level DaemonSet. It owns two defaults and
// nothing else:
//
//   - .spec.updateStrategy, when the caller names none: RollingUpdate with
//     maxUnavailable 1, so a chassis rollout takes one node at a time;
//   - .spec.template.spec.terminationGracePeriodSeconds, when the caller names
//     none: commonv1.DefaultTerminationGracePeriodSeconds, the same value the
//     API Deployments run on.
//
// The DNS policy is derived rather than taken: HostNetwork sets
// ClusterFirstWithHostNet, and nothing else sets a policy at all.
//
// Unlike BuildWorkload, the builder applies NO security context of its own,
// neither on the pod nor on a container. A node-level workload mixes postures
// within one pod (see CapabilitySecurityContext and
// PrivilegedSecurityContext), so each container carries the one it needs and a
// builder-owned default would only be the wrong one somewhere.
//
// BuildDaemonSet is a total function over valid params: it performs no I/O and
// has no error paths.
func BuildDaemonSet(p DaemonSetParams) *appsv1.DaemonSet {
	updateStrategy := appsv1.DaemonSetUpdateStrategy{
		Type: appsv1.RollingUpdateDaemonSetStrategyType,
		RollingUpdate: &appsv1.RollingUpdateDaemonSet{
			MaxUnavailable: ptr.To(intstr.FromInt32(1)),
		},
	}
	if p.UpdateStrategy != nil {
		updateStrategy = *p.UpdateStrategy
	}

	grace := p.TerminationGracePeriodSeconds
	if grace == nil {
		grace = ptr.To(commonv1.DefaultTerminationGracePeriodSeconds)
	}

	var dnsPolicy corev1.DNSPolicy
	if p.HostNetwork {
		dnsPolicy = corev1.DNSClusterFirstWithHostNet
	}

	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      p.Name,
			Namespace: p.Namespace,
			Labels:    p.Labels,
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: p.SelectorLabels,
			},
			UpdateStrategy: updateStrategy,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      p.Labels,
					Annotations: p.PodAnnotations,
				},
				Spec: corev1.PodSpec{
					NodeSelector:                  p.NodeSelector,
					Tolerations:                   p.Tolerations,
					HostNetwork:                   p.HostNetwork,
					DNSPolicy:                     dnsPolicy,
					PriorityClassName:             p.PriorityClassName,
					ServiceAccountName:            p.ServiceAccountName,
					TerminationGracePeriodSeconds: grace,
					SecurityContext:               p.PodSecurityContext,
					InitContainers:                p.InitContainers,
					Containers:                    p.Containers,
					Volumes:                       p.Volumes,
				},
			},
		},
	}
}

// EnsureDaemonSet creates a DaemonSet if it does not exist or applies its
// desired state via Server-Side Apply if it already exists. It returns the live
// DaemonSet beside its readiness: (live, true, nil) when every node the
// DaemonSet selects runs a ready pod of the current template, (live, false, nil)
// while a rollout is still in flight, and (nil, false, error) on unexpected
// failures.
//
// Readiness is read from a Get after the apply, not from the applied object:
// the counters it is judged by live on the status subresource, which the apply
// strips from its request body, and they are written by the DaemonSet
// controller rather than by the operator. The live object is handed back with
// the verdict because callers quote the same counters in their conditions.
//
// A DaemonSet that selects no node at all is ready. Its rollout has nothing
// left to do, and the alternative — reporting a CR unready until somebody
// labels a node — would make an empty node selection indistinguishable from a
// stuck one.
func EnsureDaemonSet(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, ds *appsv1.DaemonSet) (*appsv1.DaemonSet, bool, error) {
	if err := apply.EnsureObject(ctx, c, scheme, owner, ds, apply.FieldManager); err != nil {
		return nil, false, err
	}

	live := &appsv1.DaemonSet{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(ds), live); err != nil {
		return nil, false, fmt.Errorf("getting DaemonSet %s/%s: %w", ds.Namespace, ds.Name, err)
	}

	// Guard against stale status the same way IsDeploymentReady does: after a
	// spec change the API server bumps Generation, and the counters below still
	// describe the previous template until the controller catches up.
	return live, live.Status.ObservedGeneration == live.Generation &&
		live.Status.NumberReady == live.Status.UpdatedNumberScheduled &&
		live.Status.UpdatedNumberScheduled == live.Status.DesiredNumberScheduled, nil
}
