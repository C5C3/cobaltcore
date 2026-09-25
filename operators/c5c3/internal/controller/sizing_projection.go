// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Projection of the resolved spec.sizing onto the children's pod-level fields.
package controller

import (
	"maps"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/c5c3/cobaltcore/internal/common/release"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
	glancev1alpha1 "github.com/c5c3/cobaltcore/operators/glance/api/v1alpha1"
)

// The helpers below write every field they own on every pass, set or not, so
// clearing a value on the ControlPlane clears it on the child instead of
// leaving the last projected value pinned. They never write affinity, the
// rollout strategy or the graceful-termination timings, which stay with the
// child operators.

// resolvePlacement returns the node selector, tolerations and priority class
// of one component: the component's own value when it sets one, else the
// top-level spec.sizing value, else none. An empty map or list inherits,
// because the defaulting webhook's round trip drops it. A resolved empty
// priority class is none, which is how a component opts out of the top-level
// class. The results are copies.
func resolvePlacement(top, component c5c3v1alpha1.PodPlacementSpec) (map[string]string, []corev1.Toleration, *string) {
	nodeSelector := component.NodeSelector
	if len(nodeSelector) == 0 {
		nodeSelector = top.NodeSelector
	}
	tolerations := component.Tolerations
	if len(tolerations) == 0 {
		tolerations = top.Tolerations
	}
	priorityClassName := component.PriorityClassName
	if priorityClassName == nil {
		priorityClassName = top.PriorityClassName
	}
	var pc *string
	if priorityClassName != nil && *priorityClassName != "" {
		pc = ptr.To(*priorityClassName)
	}
	var out []corev1.Toleration
	for i := range tolerations {
		out = append(out, *tolerations[i].DeepCopy())
	}
	if len(nodeSelector) == 0 {
		return nil, out, pc
	}
	return maps.Clone(nodeSelector), out, pc
}

// completeSpread turns the selector-free spread entries of a component into
// the topology spread constraints of its Deployment, each selecting the
// Deployment's pods by selector. No entries yield nil, which keeps the child
// operator's default spread.
func completeSpread(entries []c5c3v1alpha1.SpreadConstraintSpec, selector map[string]string) []corev1.TopologySpreadConstraint {
	if len(entries) == 0 {
		return nil
	}
	out := make([]corev1.TopologySpreadConstraint, 0, len(entries))
	for _, e := range entries {
		out = append(out, corev1.TopologySpreadConstraint{
			MaxSkew:           e.MaxSkew,
			TopologyKey:       e.TopologyKey,
			WhenUnsatisfiable: e.WhenUnsatisfiable,
			LabelSelector:     &metav1.LabelSelector{MatchLabels: maps.Clone(selector)},
		})
	}
	return out
}

// projectPod writes the resources and the placement of c onto d. A nil c
// writes none of its own values and leaves the top-level placement in force.
func projectPod(d *commonv1.DeploymentSpec, top c5c3v1alpha1.PodPlacementSpec, c *c5c3v1alpha1.PinnedSizingSpec) {
	var p c5c3v1alpha1.PinnedSizingSpec
	if c != nil {
		p = *c
	}
	d.Resources = p.Resources.DeepCopy()
	d.NodeSelector, d.Tolerations, d.PriorityClassName = resolvePlacement(top, p.PodPlacementSpec)
}

// projectScaled is projectPod plus the replica count, which falls back to
// defaultReplicas, the count the ControlPlane always projected.
func projectScaled(
	d *commonv1.DeploymentSpec, top c5c3v1alpha1.PodPlacementSpec, c *c5c3v1alpha1.ScaledSizingSpec, defaultReplicas int32,
) {
	d.Replicas = defaultReplicas
	var pinned *c5c3v1alpha1.PinnedSizingSpec
	if c != nil {
		if c.Replicas != nil {
			d.Replicas = *c.Replicas
		}
		pinned = &c.PinnedSizingSpec
	}
	projectPod(d, top, pinned)
}

// projectDeployment is projectScaled plus the spread constraints, completed
// with selector, the pod selector the child's webhook requires.
func projectDeployment(
	d *commonv1.DeploymentSpec, top c5c3v1alpha1.PodPlacementSpec, c *c5c3v1alpha1.DeploymentSizingSpec,
	defaultReplicas int32, selector map[string]string,
) {
	var scaled *c5c3v1alpha1.ScaledSizingSpec
	var spread []c5c3v1alpha1.SpreadConstraintSpec
	if c != nil {
		scaled = &c.ScaledSizingSpec
		spread = c.SpreadConstraints
	}
	projectScaled(d, top, scaled, defaultReplicas)
	d.TopologySpreadConstraints = completeSpread(spread, selector)
}

// projectAPI projects a service API component onto its Deployment d and
// returns the uWSGI and autoscaling blocks the caller places on the child.
func projectAPI(
	d *commonv1.DeploymentSpec, top c5c3v1alpha1.PodPlacementSpec, a *c5c3v1alpha1.APISizingSpec, selector map[string]string,
) (*commonv1.UWSGISpec, *commonv1.AutoscalingSpec) {
	var deployment *c5c3v1alpha1.DeploymentSizingSpec
	var processes c5c3v1alpha1.ProcessSizingSpec
	var autoscaling *commonv1.AutoscalingSpec
	if a != nil {
		deployment = &a.DeploymentSizingSpec
		processes = a.ProcessSizingSpec
		autoscaling = a.Autoscaling
	}
	projectDeployment(d, top, deployment, commonv1.DefaultReplicas, selector)
	return projectUWSGI(processes), autoscaling.DeepCopy()
}

// projectWorker projects an RPC worker component onto its Deployment d and
// returns the worker count the caller places on the child, or nil to leave
// the child's own default in force.
func projectWorker(
	d *commonv1.DeploymentSpec, top c5c3v1alpha1.PodPlacementSpec, w *c5c3v1alpha1.WorkerSizingSpec,
	defaultReplicas int32, selector map[string]string,
) *int32 {
	var deployment *c5c3v1alpha1.DeploymentSizingSpec
	var workers *int32
	if w != nil {
		deployment = &w.DeploymentSizingSpec
		workers = w.Workers
	}
	projectDeployment(d, top, deployment, defaultReplicas, selector)
	if workers == nil {
		return nil
	}
	return ptr.To(*workers)
}

// projectUWSGI returns the uWSGI block of a process sizing, or nil when it
// sets neither count. A count it leaves unset stays zero, which the child's
// defaulting fills.
func projectUWSGI(p c5c3v1alpha1.ProcessSizingSpec) *commonv1.UWSGISpec {
	if p.Processes == nil && p.Threads == nil {
		return nil
	}
	u := &commonv1.UWSGISpec{}
	if p.Processes != nil {
		u.Processes = *p.Processes
	}
	if p.Threads != nil {
		u.Threads = *p.Threads
	}
	return u
}

// projectJobs returns the spec.jobs block of a Job sizing, or nil when it sets
// neither resources nor a priority class. The child falls back to its API
// Deployment for the Job placement and for an unset priority class.
func projectJobs(j *c5c3v1alpha1.JobSizingSpec) *commonv1.JobSpec {
	if j == nil || (j.Resources == nil && j.PriorityClassName == nil) {
		return nil
	}
	spec := &commonv1.JobSpec{JobBaseSpec: commonv1.JobBaseSpec{Resources: j.Resources.DeepCopy()}}
	if j.PriorityClassName != nil {
		spec.PriorityClassName = ptr.To(*j.PriorityClassName)
	}
	return spec
}

// glanceAPIServer returns the spec.apiServer block of the Glance child for
// the processes and threads of its API sizing. From release 2026.1 Glance
// runs under uWSGI and takes both counts as spec.apiServer.uwsgi; below it
// runs the eventlet server, which takes the process count as
// spec.apiServer.workers and has no thread count. The rule is the one the
// Glance webhook warns on (warnInertLaunchModeKnobs), so the child never
// carries an inert knob. It returns nil when nothing applies, including an
// unparseable release.
func glanceAPIServer(openStackRelease string, p c5c3v1alpha1.ProcessSizingSpec) *glancev1alpha1.APIServerSpec {
	rel, err := release.ParseRelease(openStackRelease)
	if err != nil {
		return nil
	}
	if rel.Year > 2026 || (rel.Year == 2026 && rel.Minor >= 1) {
		if u := projectUWSGI(p); u != nil {
			return &glancev1alpha1.APIServerSpec{UWSGI: u}
		}
		return nil
	}
	if p.Processes == nil {
		return nil
	}
	return &glancev1alpha1.APIServerSpec{Workers: ptr.To(*p.Processes)}
}
