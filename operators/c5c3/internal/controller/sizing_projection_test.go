// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the helpers that project the resolved spec.sizing onto the
// children, plus the sizing builders the per-service projection tests share.
package controller

import (
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/ptr"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	"github.com/c5c3/cobaltcore/internal/common/validation"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
	glancev1alpha1 "github.com/c5c3/cobaltcore/operators/glance/api/v1alpha1"
	keystonev1alpha1 "github.com/c5c3/cobaltcore/operators/keystone/api/v1alpha1"
)

// sizingOf wraps s in the ControlPlane's spec.sizing.
func sizingOf(s c5c3v1alpha1.SizingSpec) *c5c3v1alpha1.ControlPlaneSizingSpec {
	return &c5c3v1alpha1.ControlPlaneSizingSpec{SizingSpec: s}
}

// minimalWith selects the Minimal profile and overlays s.
func minimalWith(s c5c3v1alpha1.SizingSpec) *c5c3v1alpha1.ControlPlaneSizingSpec {
	return &c5c3v1alpha1.ControlPlaneSizingSpec{Profile: c5c3v1alpha1.SizingProfileMinimal, SizingSpec: s}
}

// deploymentReplicas is a Deployment sizing that sets the replica count alone.
func deploymentReplicas(n int32) c5c3v1alpha1.DeploymentSizingSpec {
	return c5c3v1alpha1.DeploymentSizingSpec{ScaledSizingSpec: c5c3v1alpha1.ScaledSizingSpec{Replicas: ptr.To(n)}}
}

// apiReplicas is an API sizing that sets the replica count alone.
func apiReplicas(n int32) *c5c3v1alpha1.APISizingSpec {
	return &c5c3v1alpha1.APISizingSpec{DeploymentSizingSpec: deploymentReplicas(n)}
}

// cpuRequestSizing requests cpu for one container.
func cpuRequestSizing(cpu string) *corev1.ResourceRequirements {
	return &corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu)}}
}

// hostSpread is one spread entry across hostnames.
func hostSpread() []c5c3v1alpha1.SpreadConstraintSpec {
	return []c5c3v1alpha1.SpreadConstraintSpec{{
		MaxSkew: 1, TopologyKey: "kubernetes.io/hostname", WhenUnsatisfiable: corev1.DoNotSchedule,
	}}
}

// expectUnsized asserts that the projection wrote none of the optional sizing
// fields onto a child Deployment: the shape a ControlPlane without spec.sizing
// always projected.
func expectUnsized(g Gomega, d commonv1.DeploymentSpec, replicas int32) {
	g.Expect(d.Replicas).To(Equal(replicas))
	g.Expect(d.Resources).To(BeNil())
	g.Expect(d.NodeSelector).To(BeNil())
	g.Expect(d.Tolerations).To(BeNil())
	g.Expect(d.PriorityClassName).To(BeNil())
	g.Expect(d.TopologySpreadConstraints).To(BeNil())
	g.Expect(d.Affinity).To(BeNil())
}

func TestCompleteSpread(t *testing.T) {
	g := NewGomegaWithT(t)
	g.Expect(completeSpread(nil, map[string]string{"a": "b"})).To(BeNil())
	g.Expect(completeSpread([]c5c3v1alpha1.SpreadConstraintSpec{}, map[string]string{"a": "b"})).To(BeNil())

	selector := keystonev1alpha1.APIPodSelector("cp-keystone")
	got := completeSpread(hostSpread(), selector)
	g.Expect(got).To(HaveLen(1))
	g.Expect(got[0].MaxSkew).To(Equal(int32(1)))
	g.Expect(got[0].TopologyKey).To(Equal("kubernetes.io/hostname"))
	g.Expect(got[0].WhenUnsatisfiable).To(Equal(corev1.DoNotSchedule))
	g.Expect(got[0].LabelSelector.MatchLabels).To(Equal(selector))
	// The completed constraint passes the check the child's webhook runs.
	g.Expect(validation.TopologySpreadSelector(field.NewPath("spec"), got, selector)).To(BeEmpty())

	// The selector is copied, so a child cannot alias it.
	got[0].LabelSelector.MatchLabels["extra"] = "x"
	g.Expect(selector).NotTo(HaveKey("extra"))
}

func TestResolvePlacement(t *testing.T) {
	top := c5c3v1alpha1.PodPlacementSpec{
		NodeSelector:      map[string]string{"pool": "control"},
		Tolerations:       []corev1.Toleration{{Key: "control", Operator: corev1.TolerationOpExists}},
		PriorityClassName: ptr.To("high"),
	}

	t.Run("the top level is the fallback", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ns, tol, pc := resolvePlacement(top, c5c3v1alpha1.PodPlacementSpec{})
		g.Expect(ns).To(Equal(top.NodeSelector))
		g.Expect(tol).To(Equal(top.Tolerations))
		g.Expect(pc).To(Equal(ptr.To("high")))
		// The results are copies.
		ns["x"] = "y"
		tol[0].Key = "changed"
		*pc = "changed"
		g.Expect(top.NodeSelector).NotTo(HaveKey("x"))
		g.Expect(top.Tolerations[0].Key).To(Equal("control"))
		g.Expect(*top.PriorityClassName).To(Equal("high"))
	})

	t.Run("a component value replaces the top level", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ns, tol, pc := resolvePlacement(top, c5c3v1alpha1.PodPlacementSpec{
			NodeSelector:      map[string]string{"pool": "db"},
			Tolerations:       []corev1.Toleration{{Key: "db", Operator: corev1.TolerationOpExists}},
			PriorityClassName: ptr.To("low"),
		})
		g.Expect(ns).To(Equal(map[string]string{"pool": "db"}))
		g.Expect(tol[0].Key).To(Equal("db"))
		g.Expect(pc).To(Equal(ptr.To("low")))
	})

	t.Run("an empty component value inherits and an empty class opts out", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ns, tol, pc := resolvePlacement(top, c5c3v1alpha1.PodPlacementSpec{
			NodeSelector:      map[string]string{},
			Tolerations:       []corev1.Toleration{},
			PriorityClassName: ptr.To(""),
		})
		g.Expect(ns).To(Equal(top.NodeSelector))
		g.Expect(tol).To(Equal(top.Tolerations))
		g.Expect(pc).To(BeNil())
	})

	t.Run("nothing set projects nothing", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ns, tol, pc := resolvePlacement(c5c3v1alpha1.PodPlacementSpec{}, c5c3v1alpha1.PodPlacementSpec{})
		g.Expect(ns).To(BeNil())
		g.Expect(tol).To(BeNil())
		g.Expect(pc).To(BeNil())
	})
}

func TestProjectDeployment(t *testing.T) {
	selector := map[string]string{"app": "x"}

	t.Run("a nil sizing projects the default replicas and nothing else", func(t *testing.T) {
		g := NewGomegaWithT(t)
		d := commonv1.DeploymentSpec{
			Replicas:                  9,
			Resources:                 cpuRequestSizing("1"),
			PriorityClassName:         ptr.To("stale"),
			TopologySpreadConstraints: []corev1.TopologySpreadConstraint{{MaxSkew: 2}},
			NodePlacementSpec:         commonv1.NodePlacementSpec{NodeSelector: map[string]string{"stale": "x"}},
		}
		projectDeployment(&d, c5c3v1alpha1.PodPlacementSpec{}, nil, 3, selector)
		// Every owned field is written, so a stale value is cleared.
		expectUnsized(g, d, 3)
	})

	t.Run("a set sizing is written and deep-copied", func(t *testing.T) {
		g := NewGomegaWithT(t)
		c := deploymentReplicas(2)
		c.Resources = cpuRequestSizing("50m")
		c.SpreadConstraints = hostSpread()
		var d commonv1.DeploymentSpec
		projectDeployment(&d, c5c3v1alpha1.PodPlacementSpec{}, &c, 3, selector)
		g.Expect(d.Replicas).To(Equal(int32(2)))
		g.Expect(d.Resources.Requests.Cpu().String()).To(Equal("50m"))
		g.Expect(d.TopologySpreadConstraints).To(HaveLen(1))
		d.Resources.Requests[corev1.ResourceMemory] = resource.MustParse("1Gi")
		g.Expect(c.Resources.Requests).NotTo(HaveKey(corev1.ResourceMemory))
	})

	t.Run("affinity, strategy and grace periods are never written", func(t *testing.T) {
		g := NewGomegaWithT(t)
		d := commonv1.DeploymentSpec{
			NodePlacementSpec:             commonv1.NodePlacementSpec{Affinity: &corev1.Affinity{}},
			TerminationGracePeriodSeconds: ptr.To[int64](60),
		}
		projectDeployment(&d, c5c3v1alpha1.PodPlacementSpec{}, nil, 3, selector)
		g.Expect(d.Affinity).NotTo(BeNil())
		g.Expect(d.TerminationGracePeriodSeconds).To(Equal(ptr.To[int64](60)))
	})
}

func TestProjectUWSGIAndJobs(t *testing.T) {
	g := NewGomegaWithT(t)
	g.Expect(projectUWSGI(c5c3v1alpha1.ProcessSizingSpec{})).To(BeNil())
	g.Expect(projectUWSGI(c5c3v1alpha1.ProcessSizingSpec{Processes: ptr.To[int32](4)})).To(
		Equal(&commonv1.UWSGISpec{Processes: 4}), "an unset thread count stays zero for the child to default")
	g.Expect(projectUWSGI(c5c3v1alpha1.ProcessSizingSpec{Threads: ptr.To[int32](2)})).To(
		Equal(&commonv1.UWSGISpec{Threads: 2}))

	g.Expect(projectJobs(nil)).To(BeNil())
	g.Expect(projectJobs(&c5c3v1alpha1.JobSizingSpec{})).To(BeNil())
	jobs := projectJobs(&c5c3v1alpha1.JobSizingSpec{PriorityClassName: ptr.To("")})
	g.Expect(jobs).NotTo(BeNil())
	g.Expect(jobs.PriorityClassName).To(Equal(ptr.To("")), "an empty class keeps opting the Jobs out")
	g.Expect(jobs.Resources).To(BeNil())
	jobs = projectJobs(&c5c3v1alpha1.JobSizingSpec{
		ContainerSizingSpec: c5c3v1alpha1.ContainerSizingSpec{Resources: cpuRequestSizing("50m")},
	})
	g.Expect(jobs.Resources.Requests.Cpu().String()).To(Equal("50m"))
	g.Expect(jobs.PriorityClassName).To(BeNil())
	g.Expect(jobs.NodeSelector).To(BeNil(), "the Jobs keep falling back to the API Deployment's placement")
}

func TestGlanceAPIServer(t *testing.T) {
	g := NewGomegaWithT(t)
	both := c5c3v1alpha1.ProcessSizingSpec{Processes: ptr.To[int32](2), Threads: ptr.To[int32](3)}

	g.Expect(glanceAPIServer("2026.1", both)).To(Equal(&glancev1alpha1.APIServerSpec{
		UWSGI: &commonv1.UWSGISpec{Processes: 2, Threads: 3},
	}))
	g.Expect(glanceAPIServer("2026.2", both).Workers).To(BeNil())
	// Below 2026.1 the eventlet server takes the process count as workers and
	// has no thread count.
	g.Expect(glanceAPIServer("2025.2", both)).To(Equal(&glancev1alpha1.APIServerSpec{Workers: ptr.To[int32](2)}))
	g.Expect(glanceAPIServer("2025.2", c5c3v1alpha1.ProcessSizingSpec{Threads: ptr.To[int32](3)})).To(BeNil())
	g.Expect(glanceAPIServer("2026.1", c5c3v1alpha1.ProcessSizingSpec{})).To(BeNil())
	g.Expect(glanceAPIServer("2025.2", c5c3v1alpha1.ProcessSizingSpec{})).To(BeNil())
	g.Expect(glanceAPIServer("not-a-release", both)).To(BeNil())
}
