// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package job

import (
	"testing"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

// nodeAffinity and podAntiAffinity are two distinguishable affinity halves: the
// fallback carries both, and only the node half may reach a Job pod.
func nodeAffinity() *corev1.NodeAffinity {
	return &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchExpressions: []corev1.NodeSelectorRequirement{{
					Key: "kubernetes.io/os", Operator: corev1.NodeSelectorOpIn, Values: []string{"linux"},
				}},
			}},
		},
	}
}

func podAntiAffinity() *corev1.PodAntiAffinity {
	return &corev1.PodAntiAffinity{
		PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{{
			Weight:          100,
			PodAffinityTerm: corev1.PodAffinityTerm{TopologyKey: "kubernetes.io/hostname"},
		}},
	}
}

// apiDeployment is an API Deployment block that sets every field a Job pod
// may fall back to.
func apiDeployment() *commonv1.DeploymentSpec {
	return &commonv1.DeploymentSpec{
		PriorityClassName: ptr.To("high"),
		NodePlacementSpec: commonv1.NodePlacementSpec{
			NodeSelector: map[string]string{"a": "b"},
			Tolerations:  []corev1.Toleration{{Key: "dedicated", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule}},
			Affinity:     &corev1.Affinity{NodeAffinity: nodeAffinity(), PodAntiAffinity: podAntiAffinity()},
		},
	}
}

func TestResolvePodSettings_NoSpecNoFallback(t *testing.T) {
	g := gomega.NewWithT(t)

	s := ResolvePodSettings(nil, nil)

	g.Expect(s.Resources).To(gomega.Equal(corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("368Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("368Mi")},
	}))
	g.Expect(s.PriorityClassName).To(gomega.BeEmpty())
	g.Expect(s.Placement).To(gomega.Equal(commonv1.NodePlacementSpec{}))
}

// An unset spec.jobs takes the API Deployment's priority class, selector and
// tolerations, and only the node half of its affinity: the pod anti-affinity
// terms target the API pods.
func TestResolvePodSettings_FallsBackToTheAPIDeployment(t *testing.T) {
	g := gomega.NewWithT(t)
	api := apiDeployment()

	s := ResolvePodSettings(nil, api)

	g.Expect(s.PriorityClassName).To(gomega.Equal("high"))
	g.Expect(s.Placement.NodeSelector).To(gomega.Equal(map[string]string{"a": "b"}))
	g.Expect(s.Placement.Tolerations).To(gomega.Equal(api.Tolerations))
	g.Expect(s.Placement.Affinity).To(gomega.Equal(&corev1.Affinity{NodeAffinity: nodeAffinity()}))
	g.Expect(s.Placement.Affinity.PodAffinity).To(gomega.BeNil())
	g.Expect(s.Placement.Affinity.PodAntiAffinity).To(gomega.BeNil())
}

func TestResolvePodSettings_EmptyPriorityClassOptsOut(t *testing.T) {
	g := gomega.NewWithT(t)

	s := ResolvePodSettings(&commonv1.JobSpec{JobBaseSpec: commonv1.JobBaseSpec{PriorityClassName: ptr.To("")}}, apiDeployment())

	g.Expect(s.PriorityClassName).To(gomega.BeEmpty())
}

func TestResolvePodSettings_SpecOverridesTheFallback(t *testing.T) {
	g := gomega.NewWithT(t)

	s := ResolvePodSettings(&commonv1.JobSpec{
		JobBaseSpec:       commonv1.JobBaseSpec{PriorityClassName: ptr.To("low")},
		NodePlacementSpec: commonv1.NodePlacementSpec{NodeSelector: map[string]string{"pool": "jobs"}},
	}, apiDeployment())

	g.Expect(s.PriorityClassName).To(gomega.Equal("low"))
	g.Expect(s.Placement.NodeSelector).To(gomega.Equal(map[string]string{"pool": "jobs"}))
	// Fields the spec leaves nil still fall back one by one.
	g.Expect(s.Placement.Tolerations).To(gomega.Equal(apiDeployment().Tolerations))
}

// An empty map, list or affinity is an explicit opt-out, not an unset field.
func TestResolvePodSettings_EmptyPlacementOptsOut(t *testing.T) {
	g := gomega.NewWithT(t)

	s := ResolvePodSettings(&commonv1.JobSpec{NodePlacementSpec: commonv1.NodePlacementSpec{
		NodeSelector: map[string]string{},
		Tolerations:  []corev1.Toleration{},
		Affinity:     &corev1.Affinity{},
	}}, apiDeployment())

	g.Expect(s.Placement.NodeSelector).NotTo(gomega.BeNil())
	g.Expect(s.Placement.NodeSelector).To(gomega.BeEmpty())
	g.Expect(s.Placement.Tolerations).NotTo(gomega.BeNil())
	g.Expect(s.Placement.Tolerations).To(gomega.BeEmpty())
	g.Expect(s.Placement.Affinity).To(gomega.Equal(&corev1.Affinity{}))
}

// A fallback affinity without a node half leaves the Job pod without affinity.
func TestResolvePodSettings_PodAntiAffinityOnlyFallbackYieldsNone(t *testing.T) {
	g := gomega.NewWithT(t)
	api := apiDeployment()
	api.Affinity = &corev1.Affinity{PodAntiAffinity: podAntiAffinity()}

	s := ResolvePodSettings(nil, api)

	g.Expect(s.Placement.Affinity).To(gomega.BeNil())
}

// A limit the block names decides that resource: no request is added beside
// it, and the CPU the block leaves out gets the default request.
func TestResolvePodSettings_LimitOnlyResources(t *testing.T) {
	g := gomega.NewWithT(t)

	s := ResolvePodSettings(&commonv1.JobSpec{JobBaseSpec: commonv1.JobBaseSpec{
		Resources: &corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}},
	}}, nil)

	g.Expect(s.Resources.Limits).To(gomega.Equal(corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}))
	g.Expect(s.Resources.Requests).To(gomega.Equal(corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}))
}

// Writing to the resolved placement never reaches spec.jobs or the API
// Deployment it was read from.
func TestResolvePodSettings_ReturnsCopies(t *testing.T) {
	g := gomega.NewWithT(t)

	api := apiDeployment()
	wantAPI := api.DeepCopy()
	fromFallback := ResolvePodSettings(nil, api)
	fromFallback.Placement.NodeSelector["a"] = "changed"
	fromFallback.Placement.Tolerations[0].Key = "changed"
	fromFallback.Placement.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution = nil
	g.Expect(api).To(gomega.Equal(wantAPI))

	spec := &commonv1.JobSpec{NodePlacementSpec: *apiDeployment().NodePlacementSpec.DeepCopy()}
	wantSpec := spec.DeepCopy()
	fromSpec := ResolvePodSettings(spec, nil)
	fromSpec.Placement.NodeSelector["a"] = "changed"
	fromSpec.Placement.Tolerations[0].Key = "changed"
	fromSpec.Placement.Affinity.PodAntiAffinity = nil
	g.Expect(spec).To(gomega.Equal(wantSpec))
}

func TestResolvePodSettingsWithRequestFloor(t *testing.T) {
	g := gomega.NewWithT(t)

	s := ResolvePodSettingsWithRequestFloor(nil, nil)
	g.Expect(s.Resources.Requests).To(gomega.Equal(corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("100m"),
		corev1.ResourceMemory: resource.MustParse("256Mi"),
	}))
	g.Expect(s.Resources.Limits).To(gomega.BeNil())

	// The priority and placement rule is the same as ResolvePodSettings'.
	s = ResolvePodSettingsWithRequestFloor(nil, apiDeployment())
	g.Expect(s.PriorityClassName).To(gomega.Equal("high"))
	g.Expect(s.Placement.Affinity).To(gomega.Equal(&corev1.Affinity{NodeAffinity: nodeAffinity()}))
}

func TestPodSettingsApply_EveryContainer(t *testing.T) {
	g := gomega.NewWithT(t)
	s := ResolvePodSettings(nil, apiDeployment())
	ps := &corev1.PodSpec{
		InitContainers: []corev1.Container{{Name: "init"}},
		Containers:     []corev1.Container{{Name: "main"}, {Name: "sidecar"}},
	}

	s.Apply(ps)

	for _, c := range append(ps.InitContainers, ps.Containers...) {
		g.Expect(c.Resources).To(gomega.Equal(s.Resources), "container %s", c.Name)
	}
	g.Expect(ps.PriorityClassName).To(gomega.Equal("high"))
	g.Expect(ps.NodeSelector).To(gomega.Equal(s.Placement.NodeSelector))
	g.Expect(ps.Tolerations).To(gomega.Equal(s.Placement.Tolerations))
	g.Expect(ps.Affinity).To(gomega.Equal(s.Placement.Affinity))

	// Each container holds its own copy of the resources.
	ps.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("4")
	g.Expect(ps.Containers[1].Resources.Requests[corev1.ResourceCPU]).To(gomega.Equal(resource.MustParse("100m")))
	g.Expect(s.Resources.Requests[corev1.ResourceCPU]).To(gomega.Equal(resource.MustParse("100m")))
}

func TestPodSettingsApply_NilPodSpec(t *testing.T) {
	g := gomega.NewWithT(t)

	g.Expect(func() { ResolvePodSettings(nil, nil).Apply(nil) }).NotTo(gomega.Panic())
}

// Zero settings render no placement: the fields stay nil, so a pod template
// hash is unchanged by an unset block.
func TestPodSettingsApply_ZeroSettingsRenderNoPlacement(t *testing.T) {
	g := gomega.NewWithT(t)
	ps := &corev1.PodSpec{Containers: []corev1.Container{{Name: "main"}}}

	PodSettings{}.Apply(ps)

	g.Expect(ps.NodeSelector).To(gomega.BeNil())
	g.Expect(ps.Tolerations).To(gomega.BeNil())
	g.Expect(ps.Affinity).To(gomega.BeNil())
	g.Expect(ps.PriorityClassName).To(gomega.BeEmpty())
}
