// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the helpers that project the resolved spec.sizing onto the
// children, plus the sizing builders the per-service projection tests share.
package controller

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

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
	g.Expect(d.VerticalAutoscaling).To(BeNil())
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
			VerticalAutoscaling:       &commonv1.VerticalAutoscalingSpec{UpdateMode: "Off"},
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
		c.VerticalAutoscaling = &commonv1.VerticalAutoscalingSpec{
			UpdateMode: "Initial", MinAllowed: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
		}
		var d commonv1.DeploymentSpec
		projectDeployment(&d, c5c3v1alpha1.PodPlacementSpec{}, &c, 3, selector)
		g.Expect(d.Replicas).To(Equal(int32(2)))
		g.Expect(d.Resources.Requests.Cpu().String()).To(Equal("50m"))
		g.Expect(d.TopologySpreadConstraints).To(HaveLen(1))
		g.Expect(d.VerticalAutoscaling).To(Equal(c.VerticalAutoscaling))
		d.Resources.Requests[corev1.ResourceMemory] = resource.MustParse("1Gi")
		g.Expect(c.Resources.Requests).NotTo(HaveKey(corev1.ResourceMemory))
		d.VerticalAutoscaling.MinAllowed[corev1.ResourceMemory] = resource.MustParse("1Gi")
		g.Expect(c.VerticalAutoscaling.MinAllowed[corev1.ResourceMemory]).To(Equal(resource.MustParse("256Mi")))
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

// The worker Deployments and the fixed-count Deployments take their
// verticalAutoscaling block from the wrapper types, and a nil wrapper clears a
// stale one.
func TestProjectWorkersAndPinnedDeployment_VerticalAutoscaling(t *testing.T) {
	vertical := func() *commonv1.VerticalAutoscalingSpec {
		return &commonv1.VerticalAutoscalingSpec{UpdateMode: "Off"}
	}

	t.Run("workers", func(t *testing.T) {
		g := NewGomegaWithT(t)
		d := commonv1.DeploymentSpec{VerticalAutoscaling: vertical()}
		projectWorkers(&d, c5c3v1alpha1.PodPlacementSpec{}, nil, 3)
		expectUnsized(g, d, 3)

		w := &c5c3v1alpha1.WorkersSizingSpec{
			ScaledSizingSpec:    c5c3v1alpha1.ScaledSizingSpec{Replicas: ptr.To[int32](1)},
			VerticalAutoscaling: vertical(),
		}
		projectWorkers(&d, c5c3v1alpha1.PodPlacementSpec{}, w, 3)
		g.Expect(d.Replicas).To(Equal(int32(1)))
		g.Expect(d.VerticalAutoscaling).To(Equal(vertical()))
		g.Expect(d.VerticalAutoscaling).NotTo(BeIdenticalTo(w.VerticalAutoscaling))
	})

	t.Run("pinned deployment", func(t *testing.T) {
		g := NewGomegaWithT(t)
		d := commonv1.DeploymentSpec{Replicas: 1, VerticalAutoscaling: vertical()}
		projectPinnedDeployment(&d, c5c3v1alpha1.PodPlacementSpec{}, nil)
		expectUnsized(g, d, 1)

		p := &c5c3v1alpha1.PinnedDeploymentSizingSpec{VerticalAutoscaling: vertical()}
		projectPinnedDeployment(&d, c5c3v1alpha1.PodPlacementSpec{}, p)
		g.Expect(d.VerticalAutoscaling).To(Equal(vertical()))
		g.Expect(d.VerticalAutoscaling).NotTo(BeIdenticalTo(p.VerticalAutoscaling))
	})
}

// TestProjectAPI_CarriesAutoscalingBehavior pins that the regenerated
// deepcopy carries spec.sizing.<service>.api.autoscaling.behavior to the
// child as a copy, and that an unset block projects none.
func TestProjectAPI_CarriesAutoscalingBehavior(t *testing.T) {
	selector := keystonev1alpha1.APIPodSelector("cp-keystone")
	withBehavior := func() *c5c3v1alpha1.APISizingSpec {
		api := apiReplicas(1)
		api.Autoscaling = &commonv1.AutoscalingSpec{
			MinReplicas:          ptr.To[int32](1),
			MaxReplicas:          3,
			TargetCPUUtilization: ptr.To[int32](150),
			Behavior: &autoscalingv2.HorizontalPodAutoscalerBehavior{
				ScaleDown: &autoscalingv2.HPAScalingRules{
					StabilizationWindowSeconds: ptr.To[int32](15),
					Policies: []autoscalingv2.HPAScalingPolicy{
						{Type: autoscalingv2.PercentScalingPolicy, Value: 100, PeriodSeconds: 15},
					},
				},
			},
		}
		return api
	}

	t.Run("a set behavior reaches the child as a copy", func(t *testing.T) {
		g := NewGomegaWithT(t)
		api := withBehavior()
		var d commonv1.DeploymentSpec
		_, autoscaling := projectAPI(&d, c5c3v1alpha1.PodPlacementSpec{}, api, selector)
		g.Expect(autoscaling).To(Equal(api.Autoscaling))
		*autoscaling.Behavior.ScaleDown.StabilizationWindowSeconds = 300
		g.Expect(api.Autoscaling.Behavior.ScaleDown.StabilizationWindowSeconds).To(HaveValue(Equal(int32(15))),
			"changing the child's copy must leave the ControlPlane's block unchanged")
	})

	t.Run("an unset autoscaling projects nil", func(t *testing.T) {
		g := NewGomegaWithT(t)
		var d commonv1.DeploymentSpec
		_, autoscaling := projectAPI(&d, c5c3v1alpha1.PodPlacementSpec{}, apiReplicas(1), selector)
		g.Expect(autoscaling).To(BeNil())
		_, autoscaling = projectAPI(&d, c5c3v1alpha1.PodPlacementSpec{}, nil, selector)
		g.Expect(autoscaling).To(BeNil())
	})

	t.Run("the Keystone child carries the behavior", func(t *testing.T) {
		g := NewGomegaWithT(t)
		s := keystoneTestScheme(t)
		cp := keystoneControlPlane()
		cp.Spec.Sizing = minimalWith(c5c3v1alpha1.SizingSpec{
			Keystone: &c5c3v1alpha1.KeystoneSizingSpec{API: withBehavior()},
		})
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
		r := &ControlPlaneReconciler{Client: c, Scheme: s}

		_, err := r.reconcileKeystone(context.Background(), cp)
		g.Expect(err).NotTo(HaveOccurred())
		k := getProjectedKeystone(t, c, cp)
		g.Expect(k.Spec.Autoscaling.Behavior).To(Equal(withBehavior().Autoscaling.Behavior))
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

// TestReconcileServices_SizingProjectsVerticalAutoscaling follows the
// verticalAutoscaling block of every Deployment-shaped sizing component to the
// deployment block of the child it lands on, and shows that a ControlPlane
// without the block writes nil there on the next pass.
func TestReconcileServices_SizingProjectsVerticalAutoscaling(t *testing.T) {
	vertical := func() *commonv1.VerticalAutoscalingSpec {
		return &commonv1.VerticalAutoscalingSpec{
			UpdateMode: "Initial",
			MinAllowed: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
		}
	}
	deployment := func() *c5c3v1alpha1.DeploymentSizingSpec {
		return &c5c3v1alpha1.DeploymentSizingSpec{VerticalAutoscaling: vertical()}
	}
	api := func() *c5c3v1alpha1.APISizingSpec {
		return &c5c3v1alpha1.APISizingSpec{DeploymentSizingSpec: *deployment()}
	}
	ctx := context.Background()

	for _, tc := range []struct {
		name   string
		cp     func() *c5c3v1alpha1.ControlPlane
		sizing c5c3v1alpha1.SizingSpec
		// pass reconciles cp once and returns the projected deployment blocks
		// by the component they size.
		pass func(t *testing.T, cp *c5c3v1alpha1.ControlPlane) func() map[string]commonv1.DeploymentSpec
	}{
		{
			name:   "keystone",
			cp:     keystoneControlPlane,
			sizing: c5c3v1alpha1.SizingSpec{Keystone: &c5c3v1alpha1.KeystoneSizingSpec{API: api()}},
			pass: func(t *testing.T, cp *c5c3v1alpha1.ControlPlane) func() map[string]commonv1.DeploymentSpec {
				s := keystoneTestScheme(t)
				c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
				r := &ControlPlaneReconciler{Client: c, Scheme: s}
				return func() map[string]commonv1.DeploymentSpec {
					_, err := r.reconcileKeystone(ctx, cp)
					NewGomegaWithT(t).Expect(err).NotTo(HaveOccurred())
					return map[string]commonv1.DeploymentSpec{"deployment": getProjectedKeystone(t, c, cp).Spec.Deployment}
				}
			},
		},
		{
			name: "horizon",
			cp:   horizonControlPlane,
			sizing: c5c3v1alpha1.SizingSpec{Horizon: &c5c3v1alpha1.HorizonSizingSpec{
				API: &c5c3v1alpha1.HorizonAPISizingSpec{DeploymentSizingSpec: *deployment()},
			}},
			pass: func(t *testing.T, cp *c5c3v1alpha1.ControlPlane) func() map[string]commonv1.DeploymentSpec {
				r := newHorizonTestReconciler(t, cp)
				return func() map[string]commonv1.DeploymentSpec {
					_, err := r.reconcileHorizon(ctx, cp)
					NewGomegaWithT(t).Expect(err).NotTo(HaveOccurred())
					return map[string]commonv1.DeploymentSpec{"deployment": getProjectedHorizon(t, r.Client, cp).Spec.Deployment}
				}
			},
		},
		{
			name:   "glance",
			cp:     glanceControlPlane,
			sizing: c5c3v1alpha1.SizingSpec{Glance: &c5c3v1alpha1.APIServiceSizingSpec{API: api()}},
			pass: func(t *testing.T, cp *c5c3v1alpha1.ControlPlane) func() map[string]commonv1.DeploymentSpec {
				r := newGlanceTestReconciler(t, cp)
				return func() map[string]commonv1.DeploymentSpec {
					_, err := r.reconcileGlance(ctx, cp)
					NewGomegaWithT(t).Expect(err).NotTo(HaveOccurred())
					return map[string]commonv1.DeploymentSpec{"deployment": getProjectedGlance(t, r.Client, cp).Spec.Deployment}
				}
			},
		},
		{
			name:   "placement",
			cp:     placementControlPlane,
			sizing: c5c3v1alpha1.SizingSpec{Placement: &c5c3v1alpha1.APIServiceSizingSpec{API: api()}},
			pass: func(t *testing.T, cp *c5c3v1alpha1.ControlPlane) func() map[string]commonv1.DeploymentSpec {
				r := newPlacementTestReconciler(t, cp)
				return func() map[string]commonv1.DeploymentSpec {
					_, err := r.reconcilePlacement(ctx, cp)
					NewGomegaWithT(t).Expect(err).NotTo(HaveOccurred())
					return map[string]commonv1.DeploymentSpec{"deployment": getProjectedPlacement(t, r.Client, cp).Spec.Deployment}
				}
			},
		},
		{
			name:   "barbican",
			cp:     barbicanControlPlane,
			sizing: c5c3v1alpha1.SizingSpec{Barbican: &c5c3v1alpha1.APIServiceSizingSpec{API: api()}},
			pass: func(t *testing.T, cp *c5c3v1alpha1.ControlPlane) func() map[string]commonv1.DeploymentSpec {
				r := newBarbicanTestReconciler(t, cp)
				return func() map[string]commonv1.DeploymentSpec {
					_, err := r.reconcileBarbican(ctx, cp)
					NewGomegaWithT(t).Expect(err).NotTo(HaveOccurred())
					return map[string]commonv1.DeploymentSpec{"deployment": getProjectedBarbican(t, r.Client, cp).Spec.Deployment}
				}
			},
		},
		{
			name: "neutron",
			cp:   neutronControlPlane,
			sizing: c5c3v1alpha1.SizingSpec{Neutron: &c5c3v1alpha1.NeutronSizingSpec{
				API:     api(),
				Workers: &c5c3v1alpha1.WorkersSizingSpec{VerticalAutoscaling: vertical()},
			}},
			pass: func(t *testing.T, cp *c5c3v1alpha1.ControlPlane) func() map[string]commonv1.DeploymentSpec {
				r := newNeutronTestReconciler(t, cp)
				return func() map[string]commonv1.DeploymentSpec {
					_, err := r.reconcileNeutron(ctx, cp)
					NewGomegaWithT(t).Expect(err).NotTo(HaveOccurred())
					nn := getProjectedNeutron(t, r.Client, cp)
					return map[string]commonv1.DeploymentSpec{
						"deployment":         nn.Spec.Deployment,
						"workers.deployment": nn.Spec.Workers.Deployment,
					}
				}
			},
		},
		{
			name: "cinder",
			cp:   cinderControlPlane,
			sizing: c5c3v1alpha1.SizingSpec{Cinder: &c5c3v1alpha1.CinderSizingSpec{
				API:       api(),
				Scheduler: deployment(),
				Volume:    &c5c3v1alpha1.PinnedDeploymentSizingSpec{VerticalAutoscaling: vertical()},
				Backup:    &c5c3v1alpha1.PinnedDeploymentSizingSpec{VerticalAutoscaling: vertical()},
			}},
			pass: func(t *testing.T, cp *c5c3v1alpha1.ControlPlane) func() map[string]commonv1.DeploymentSpec {
				r := newCinderTestReconciler(t, cp)
				return func() map[string]commonv1.DeploymentSpec {
					_, err := r.reconcileCinder(ctx, cp)
					NewGomegaWithT(t).Expect(err).NotTo(HaveOccurred())
					cn := getProjectedCinder(t, r.Client, cp)
					return map[string]commonv1.DeploymentSpec{
						"api.deployment":       cn.Spec.API.Deployment,
						"scheduler.deployment": cn.Spec.Scheduler.Deployment,
						"volume.deployment":    cn.Spec.Volume.Deployment,
						"backup.deployment":    cn.Spec.Backup.Deployment,
					}
				}
			},
		},
		{
			name: "nova",
			cp:   novaControlPlane,
			sizing: c5c3v1alpha1.SizingSpec{Nova: &c5c3v1alpha1.NovaSizingSpec{
				API:          api(),
				Metadata:     &c5c3v1alpha1.MetadataAPISizingSpec{DeploymentSizingSpec: *deployment()},
				Scheduler:    &c5c3v1alpha1.WorkerSizingSpec{DeploymentSizingSpec: *deployment()},
				Conductor:    &c5c3v1alpha1.WorkerSizingSpec{DeploymentSizingSpec: *deployment()},
				ConsoleProxy: deployment(),
			}},
			pass: func(t *testing.T, cp *c5c3v1alpha1.ControlPlane) func() map[string]commonv1.DeploymentSpec {
				r := newNovaTestReconciler(t, cp)
				return func() map[string]commonv1.DeploymentSpec {
					_, err := r.reconcileNova(ctx, cp)
					NewGomegaWithT(t).Expect(err).NotTo(HaveOccurred())
					nv := getProjectedNova(t, r.Client, cp)
					return map[string]commonv1.DeploymentSpec{
						"api.deployment":          nv.Spec.API.Deployment,
						"metadata.deployment":     nv.Spec.Metadata.Deployment,
						"scheduler.deployment":    nv.Spec.Scheduler.Deployment,
						"conductor.deployment":    nv.Spec.Conductor.Deployment,
						"consoleProxy.deployment": *nv.Spec.ConsoleProxy.Deployment,
					}
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := tc.cp()
			cp.Spec.Sizing = sizingOf(tc.sizing)
			pass := tc.pass(t, cp)

			for component, d := range pass() {
				g.Expect(d.VerticalAutoscaling).To(Equal(vertical()), component)
			}

			cp.Spec.Sizing = nil
			for component, d := range pass() {
				g.Expect(d.VerticalAutoscaling).To(BeNil(), "%s after the block is removed", component)
			}
		})
	}
}
