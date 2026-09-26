// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"context"
	"errors"
	"testing"

	. "github.com/onsi/gomega"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

func TestSizingProfileDefault_SetsStandard(t *testing.T) {
	g := NewWithT(t)
	w := &SizingProfileWebhook{}

	p := sizingProfile("site", "", SizingSpec{})
	g.Expect(w.Default(context.Background(), p)).To(Succeed())
	g.Expect(p.Spec.Base).To(Equal(SizingProfileStandard))

	p = sizingProfile("site", SizingProfileMinimal, SizingSpec{})
	g.Expect(w.Default(context.Background(), p)).To(Succeed())
	g.Expect(p.Spec.Base).To(Equal(SizingProfileMinimal))
}

func TestSizingProfileValidateCreate(t *testing.T) {
	tests := []struct {
		name string
		base SizingProfileName
		spec SizingSpec
		want string
	}{
		{name: "an unknown base", base: "Large", want: `spec.base: Unsupported value: "Large"`},
		{
			name: "a request above its limit",
			spec: SizingSpec{Messaging: &ScaledSizingSpec{PinnedSizingSpec: PinnedSizingSpec{
				ContainerSizingSpec: ContainerSizingSpec{Resources: resources(
					corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")},
					corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
				)},
			}}},
			want: "spec.messaging.resources.requests.memory: Invalid value",
		},
		{
			name: "a node selector key with a space",
			spec: SizingSpec{PodPlacementSpec: PodPlacementSpec{NodeSelector: map[string]string{"bad key": "x"}}},
			want: `spec.nodeSelector: Invalid value: "bad key"`,
		},
		{
			name: "database replicas 2",
			spec: SizingSpec{Database: &DatabaseSizingSpec{Replicas: ptr.To[int32](2)}},
			want: "spec.database.replicas: Invalid value: 2",
		},
		{
			name: "a cache memory limit of 64Mi",
			spec: SizingSpec{Cache: &CacheSizingSpec{ContainerSizingSpec: ContainerSizingSpec{
				Resources: resources(nil, corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Mi")}),
			}}},
			want: "memory limit must be at least 96Mi",
		},
		{
			name: "a spread entry with maxSkew 0",
			spec: SizingSpec{Glance: &APIServiceSizingSpec{API: &APISizingSpec{DeploymentSizingSpec: DeploymentSizingSpec{
				SpreadConstraints: []SpreadConstraintSpec{{TopologyKey: "kubernetes.io/hostname", WhenUnsatisfiable: corev1.DoNotSchedule}},
			}}}},
			want: "spec.glance.api.spreadConstraints[0].maxSkew: Invalid value: 0",
		},
		{
			name: "a priority class naming no PriorityClass",
			spec: SizingSpec{Nova: &NovaSizingSpec{Jobs: &JobSizingSpec{PriorityClassName: ptr.To("missing")}}},
			want: `spec.nova.jobs.priorityClassName: Not found: "missing"`,
		},
		{
			name: "an autoscaling policy period above 1800 seconds",
			spec: keystoneAPI(APISizingSpec{Autoscaling: &commonv1.AutoscalingSpec{
				MaxReplicas:          3,
				TargetCPUUtilization: ptr.To[int32](80),
				Behavior: &autoscalingv2.HorizontalPodAutoscalerBehavior{ScaleDown: &autoscalingv2.HPAScalingRules{
					Policies: []autoscalingv2.HPAScalingPolicy{{Type: autoscalingv2.PodsScalingPolicy, Value: 1, PeriodSeconds: 1801}},
				}},
			}}),
			want: "spec.keystone.api.autoscaling.behavior.scaleDown.policies[0].periodSeconds: Invalid value: 1801: " +
				"periodSeconds must be between 1 and 1800",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			w := &SizingProfileWebhook{Client: fake.NewClientBuilder().WithScheme(sizingScheme(t)).Build()}
			_, err := w.ValidateCreate(context.Background(), sizingProfile("site", tc.base, tc.spec))
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring(tc.want))
		})
	}

	t.Run("both built-in profiles are valid as a profile's values", func(t *testing.T) {
		g := NewWithT(t)
		w := &SizingProfileWebhook{Client: fake.NewClientBuilder().WithScheme(sizingScheme(t)).Build()}
		for _, name := range []SizingProfileName{SizingProfileMinimal, SizingProfileStandard} {
			_, err := w.ValidateCreate(context.Background(), sizingProfile("site", name, BuiltinSizing(name)))
			g.Expect(err).NotTo(HaveOccurred(), string(name))
		}
	})
}

// referencingControlPlane returns a ControlPlane in ns that sizes itself from
// the SizingProfile named profile, with its own Keystone CPU limit.
func referencingControlPlane(ns, name, profile, cpuLimitValue string) *ControlPlane {
	cp := managedControlPlane()
	cp.Namespace, cp.Name = ns, name
	cp.Spec.Sizing = &ControlPlaneSizingSpec{
		ProfileRef: &SizingProfileRef{Name: profile},
		SizingSpec: keystoneAPI(withResourcesAPI(cpuLimit(cpuLimitValue))),
	}
	return cp
}

func TestSizingProfileValidateCreate_RejectsReferencingControlPlane(t *testing.T) {
	g := NewWithT(t)
	// "site" was deleted after the plane was admitted, and the plane's 100m
	// Keystone CPU limit was set while no merge could be checked.
	w := &SizingProfileWebhook{Client: fake.NewClientBuilder().WithScheme(sizingScheme(t)).
		WithObjects(referencingControlPlane("tenant-a", "cp", "site", "100m")).Build()}

	// Restoring "site" with a 200m Keystone request breaks the plane.
	_, err := w.ValidateCreate(context.Background(), sizingProfile("site", SizingProfileStandard,
		keystoneAPI(withResourcesAPI(resources(corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m")}, nil)))))
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("ControlPlane tenant-a/cp: spec.sizing.keystone.api.resources.requests.cpu"))

	// Restoring it with values the plane fits is admitted: Minimal requests 50m.
	_, err = w.ValidateCreate(context.Background(), sizingProfile("site", SizingProfileMinimal, SizingSpec{}))
	g.Expect(err).NotTo(HaveOccurred())
}

func TestSizingProfileValidateUpdate_RejectsReferencingControlPlane(t *testing.T) {
	g := NewWithT(t)
	referencing := referencingControlPlane("tenant-a", "cp", "site", "100m")
	// The unrelated plane would break too, but it references another profile.
	unrelated := referencingControlPlane("tenant-b", "cp", "other", "10m")
	w := &SizingProfileWebhook{Client: fake.NewClientBuilder().WithScheme(sizingScheme(t)).
		WithObjects(referencing, unrelated).Build()}

	oldP := sizingProfile("site", SizingProfileStandard, SizingSpec{})
	// A 200m Keystone request exceeds the 100m limit the referencing plane sets.
	newP := sizingProfile("site", SizingProfileStandard, keystoneAPI(withResourcesAPI(resources(
		corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m")}, nil))))

	_, err := w.ValidateUpdate(context.Background(), oldP, newP)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("ControlPlane tenant-a/cp: spec.sizing.keystone.api.resources.requests.cpu"))
	g.Expect(err.Error()).NotTo(ContainSubstring("tenant-b"))

	// Switching the base to Minimal instead keeps the plane valid: Minimal
	// requests 50m, below its 100m limit.
	_, err = w.ValidateUpdate(context.Background(), oldP, sizingProfile("site", SizingProfileMinimal, SizingSpec{}))
	g.Expect(err).NotTo(HaveOccurred())
}

func TestSizingProfileValidateUpdate_RejectsMaxReplicasBelowReferencingReplicas(t *testing.T) {
	g := NewWithT(t)
	w := &SizingProfileWebhook{Client: fake.NewClientBuilder().WithScheme(sizingScheme(t)).
		WithObjects(referencingControlPlane("tenant-a", "cp", "site", "1")).Build()}
	api := withResourcesAPI(resources(corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}, nil))
	api.Autoscaling = &commonv1.AutoscalingSpec{MaxReplicas: 2, TargetCPUUtilization: ptr.To[int32](80)}

	// The referencing plane runs Standard's three Keystone API replicas.
	_, err := w.ValidateUpdate(context.Background(), sizingProfile("site", SizingProfileStandard, SizingSpec{}),
		sizingProfile("site", SizingProfileStandard, keystoneAPI(api)))
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring(
		"ControlPlane tenant-a/cp: spec.sizing.keystone.api.autoscaling.maxReplicas: Invalid value: 2"))
}

func TestSizingProfileValidateUpdate_AdmitsWhenUnreferenced(t *testing.T) {
	g := NewWithT(t)
	unrelated := referencingControlPlane("tenant-b", "cp", "other", "10m")
	w := &SizingProfileWebhook{Client: fake.NewClientBuilder().WithScheme(sizingScheme(t)).
		WithObjects(unrelated).Build()}
	newP := sizingProfile("site", SizingProfileMinimal, keystoneAPI(withResourcesAPI(resources(
		corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m")}, nil))))

	_, err := w.ValidateUpdate(context.Background(), sizingProfile("site", SizingProfileStandard, SizingSpec{}), newP)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestSizingProfileValidateUpdate_ListErrorIsInternal(t *testing.T) {
	g := NewWithT(t)
	c := fake.NewClientBuilder().WithScheme(sizingScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return apierrors.NewServiceUnavailable("etcd is down")
		},
	}).Build()
	w := &SizingProfileWebhook{Client: c}
	oldP := sizingProfile("site", SizingProfileStandard, SizingSpec{})

	// A metadata-only edit changes no merged sizing and lists nothing.
	relabeled := oldP.DeepCopy()
	relabeled.Labels = map[string]string{"team": "platform"}
	_, err := w.ValidateUpdate(context.Background(), oldP, relabeled)
	g.Expect(err).NotTo(HaveOccurred())

	_, err = w.ValidateUpdate(context.Background(), oldP, sizingProfile("site", SizingProfileMinimal, SizingSpec{}))
	g.Expect(err).To(HaveOccurred())
	var status apierrors.APIStatus
	g.Expect(errors.As(err, &status)).To(BeTrue())
	g.Expect(status.Status().Details.Causes).To(HaveLen(1))
	g.Expect(err.Error()).To(ContainSubstring("spec: Internal error: listing ControlPlanes"))
}

func TestSizingProfileValidateUpdate_LooksUpOnlyNewPriorityClasses(t *testing.T) {
	g := NewWithT(t)
	gets := map[string]int{}
	c := fake.NewClientBuilder().WithScheme(sizingScheme(t)).WithObjects(priorityClass("new")).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*schedulingv1.PriorityClass); ok {
					gets[key.Name]++
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).Build()
	w := &SizingProfileWebhook{Client: c}
	// "kept" was deleted after the profile was admitted.
	oldP := sizingProfile("site", SizingProfileStandard, SizingSpec{
		PodPlacementSpec: PodPlacementSpec{PriorityClassName: ptr.To("kept")},
	})
	newP := oldP.DeepCopy()
	newP.Spec.Cinder = &CinderSizingSpec{Jobs: &JobSizingSpec{PriorityClassName: ptr.To("new")}}

	_, err := w.ValidateUpdate(context.Background(), oldP, newP)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(gets).To(Equal(map[string]int{"new": 1}))
}
