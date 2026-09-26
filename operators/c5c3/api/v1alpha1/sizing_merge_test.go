// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"reflect"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

// keystoneAPI wraps an API sizing in a SizingSpec, the shape most merge
// cases below need.
func keystoneAPI(api APISizingSpec) SizingSpec {
	return SizingSpec{Keystone: &KeystoneSizingSpec{API: &api}}
}

func apiReplicas(n int32) APISizingSpec {
	return APISizingSpec{DeploymentSizingSpec: deploymentReplicas(n)}
}

func resources(requests, limits corev1.ResourceList) *corev1.ResourceRequirements {
	return &corev1.ResourceRequirements{Requests: requests, Limits: limits}
}

func TestMergeSizing(t *testing.T) {
	spread := func(key string) []SpreadConstraintSpec {
		return []SpreadConstraintSpec{{MaxSkew: 1, TopologyKey: key, WhenUnsatisfiable: corev1.ScheduleAnyway}}
	}
	withPlacement := func(api APISizingSpec, p PodPlacementSpec) APISizingSpec {
		api.PodPlacementSpec = p
		return api
	}
	withSpread := func(api APISizingSpec, s []SpreadConstraintSpec) APISizingSpec {
		api.SpreadConstraints = s
		return api
	}
	withResources := func(api APISizingSpec, rr *corev1.ResourceRequirements) APISizingSpec {
		api.Resources = rr
		return api
	}
	withAutoscaling := func(api APISizingSpec, a *commonv1.AutoscalingSpec) APISizingSpec {
		api.Autoscaling = a
		return api
	}
	tolerations := func(key string) []corev1.Toleration {
		return []corev1.Toleration{{Key: key, Operator: corev1.TolerationOpExists}}
	}

	tests := []struct {
		name     string
		base     SizingSpec
		override SizingSpec
		want     SizingSpec
	}{
		{
			name:     "a set pointer override wins",
			base:     keystoneAPI(apiReplicas(3)),
			override: keystoneAPI(apiReplicas(1)),
			want:     keystoneAPI(apiReplicas(1)),
		},
		{
			name:     "a nil override keeps the base",
			base:     keystoneAPI(apiReplicas(3)),
			override: keystoneAPI(APISizingSpec{}),
			want:     keystoneAPI(apiReplicas(3)),
		},
		{
			name:     "a nil block override keeps the base block",
			base:     keystoneAPI(apiReplicas(3)),
			override: SizingSpec{},
			want:     keystoneAPI(apiReplicas(3)),
		},
		{
			name:     "a nil base takes the override",
			base:     SizingSpec{},
			override: SizingSpec{Nova: &NovaSizingSpec{Scheduler: &WorkerSizingSpec{Workers: ptr.To[int32](4)}}},
			want:     SizingSpec{Nova: &NovaSizingSpec{Scheduler: &WorkerSizingSpec{Workers: ptr.To[int32](4)}}},
		},
		{
			name: "resources merge per resource name within requests and within limits",
			base: keystoneAPI(withResources(APISizingSpec{}, resources(
				corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")},
				corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
			))),
			override: keystoneAPI(withResources(APISizingSpec{}, resources(
				corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
				corev1.ResourceList{
					corev1.ResourceMemory: resource.MustParse("512Mi"),
					corev1.ResourceCPU:    resource.MustParse("1"),
				},
			))),
			want: keystoneAPI(withResources(APISizingSpec{}, resources(
				corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("50m"),
					corev1.ResourceMemory: resource.MustParse("256Mi"),
				},
				corev1.ResourceList{
					corev1.ResourceMemory: resource.MustParse("512Mi"),
					corev1.ResourceCPU:    resource.MustParse("1"),
				},
			))),
		},
		{
			name: "resource claims replace when set",
			base: SizingSpec{Cache: &CacheSizingSpec{ContainerSizingSpec: ContainerSizingSpec{
				Resources: &corev1.ResourceRequirements{Claims: []corev1.ResourceClaim{{Name: "a"}}},
			}}},
			override: SizingSpec{Cache: &CacheSizingSpec{ContainerSizingSpec: ContainerSizingSpec{
				Resources: &corev1.ResourceRequirements{Claims: []corev1.ResourceClaim{{Name: "b"}}},
			}}},
			want: SizingSpec{Cache: &CacheSizingSpec{ContainerSizingSpec: ContainerSizingSpec{
				Resources: &corev1.ResourceRequirements{Claims: []corev1.ResourceClaim{{Name: "b"}}},
			}}},
		},
		{
			name: "a non-empty nodeSelector, tolerations and spread override replaces",
			base: keystoneAPI(withSpread(withPlacement(APISizingSpec{}, PodPlacementSpec{
				NodeSelector: map[string]string{"a": "1", "b": "2"},
				Tolerations:  tolerations("a"),
			}), spread("zone"))),
			override: keystoneAPI(withSpread(withPlacement(APISizingSpec{}, PodPlacementSpec{
				NodeSelector: map[string]string{"c": "3"},
				Tolerations:  tolerations("c"),
			}), spread("host"))),
			want: keystoneAPI(withSpread(withPlacement(APISizingSpec{}, PodPlacementSpec{
				NodeSelector: map[string]string{"c": "3"},
				Tolerations:  tolerations("c"),
			}), spread("host"))),
		},
		{
			name: "an empty nodeSelector, tolerations and spread override inherits",
			base: keystoneAPI(withSpread(withPlacement(APISizingSpec{}, PodPlacementSpec{
				NodeSelector: map[string]string{"a": "1"},
				Tolerations:  tolerations("a"),
			}), spread("zone"))),
			override: keystoneAPI(withSpread(withPlacement(APISizingSpec{}, PodPlacementSpec{
				NodeSelector: map[string]string{},
				Tolerations:  []corev1.Toleration{},
			}), []SpreadConstraintSpec{})),
			want: keystoneAPI(withSpread(withPlacement(APISizingSpec{}, PodPlacementSpec{
				NodeSelector: map[string]string{"a": "1"},
				Tolerations:  tolerations("a"),
			}), spread("zone"))),
		},
		{
			name: "top-level placement merges like a component's",
			base: SizingSpec{PodPlacementSpec: PodPlacementSpec{
				NodeSelector: map[string]string{"a": "1"}, PriorityClassName: ptr.To("high"),
			}},
			override: SizingSpec{PodPlacementSpec: PodPlacementSpec{PriorityClassName: ptr.To("")}},
			want: SizingSpec{PodPlacementSpec: PodPlacementSpec{
				NodeSelector: map[string]string{"a": "1"}, PriorityClassName: ptr.To(""),
			}},
		},
		{
			name: "a set autoscaling replaces the whole block",
			base: keystoneAPI(withAutoscaling(APISizingSpec{}, &commonv1.AutoscalingSpec{
				MinReplicas: ptr.To[int32](2), MaxReplicas: 5, TargetCPUUtilization: ptr.To[int32](80),
			})),
			override: keystoneAPI(withAutoscaling(APISizingSpec{}, &commonv1.AutoscalingSpec{
				MaxReplicas: 9, TargetMemoryUtilization: ptr.To[int32](70),
			})),
			want: keystoneAPI(withAutoscaling(APISizingSpec{}, &commonv1.AutoscalingSpec{
				MaxReplicas: 9, TargetMemoryUtilization: ptr.To[int32](70),
			})),
		},
		{
			name:     "an empty storageSize keeps the base",
			base:     SizingSpec{Database: &DatabaseSizingSpec{StorageSize: "100Gi", Replicas: ptr.To[int32](3)}},
			override: SizingSpec{Database: &DatabaseSizingSpec{Replicas: ptr.To[int32](1)}},
			want:     SizingSpec{Database: &DatabaseSizingSpec{StorageSize: "100Gi", Replicas: ptr.To[int32](1)}},
		},
		{
			name:     "zero merged with zero is zero",
			base:     SizingSpec{},
			override: SizingSpec{},
			want:     SizingSpec{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			got := MergeSizing(tc.base, tc.override)
			g.Expect(equality.Semantic.DeepEqual(got, tc.want)).To(BeTrue(), "got %+v", got)
		})
	}
}

func TestMergeSizing_DoesNotAliasInputs(t *testing.T) {
	g := NewWithT(t)
	base := BuiltinSizing(SizingProfileMinimal)
	base.NodeSelector = map[string]string{"a": "1"}
	override := SizingSpec{
		Keystone: &KeystoneSizingSpec{API: &APISizingSpec{
			DeploymentSizingSpec: DeploymentSizingSpec{
				ScaledSizingSpec: ScaledSizingSpec{
					Replicas: ptr.To[int32](2),
					PinnedSizingSpec: PinnedSizingSpec{ContainerSizingSpec: ContainerSizingSpec{
						Resources: resources(nil, corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}),
					}},
				},
			},
		}},
		Horizon: &HorizonSizingSpec{API: &HorizonAPISizingSpec{DeploymentSizingSpec: deploymentReplicas(4)}},
	}
	baseBefore := base.DeepCopy()
	overrideBefore := override.DeepCopy()

	got := MergeSizing(base, override)
	g.Expect(equality.Semantic.DeepEqual(base, *baseBefore)).To(BeTrue())
	g.Expect(equality.Semantic.DeepEqual(override, *overrideBefore)).To(BeTrue())

	// Mutating the result must not reach either input, neither through a
	// block the override supplied nor through one the base supplied.
	*got.Keystone.API.Replicas = 9
	got.Keystone.API.Resources.Requests[corev1.ResourceMemory] = resource.MustParse("1Gi")
	got.Keystone.API.Resources.Limits[corev1.ResourceCPU] = resource.MustParse("3")
	*got.Horizon.API.Replicas = 9
	got.NodeSelector["b"] = "2"
	*got.Database.Replicas = 9
	g.Expect(equality.Semantic.DeepEqual(base, *baseBefore)).To(BeTrue())
	g.Expect(equality.Semantic.DeepEqual(override, *overrideBefore)).To(BeTrue())
}

func TestResolveSizing(t *testing.T) {
	cp := func(sizing *ControlPlaneSizingSpec) *ControlPlane {
		return &ControlPlane{Spec: ControlPlaneSpec{Sizing: sizing}}
	}

	t.Run("nil spec.sizing resolves to Standard", func(t *testing.T) {
		g := NewWithT(t)
		g.Expect(ResolveSizing(cp(nil), nil)).To(Equal(BuiltinSizing(SizingProfileStandard)))
		g.Expect(EffectiveSizingBase(cp(nil), nil)).To(Equal(SizingProfileStandard))
	})

	t.Run("profile Minimal resolves to Minimal", func(t *testing.T) {
		g := NewWithT(t)
		got := ResolveSizing(cp(&ControlPlaneSizingSpec{Profile: SizingProfileMinimal}), nil)
		g.Expect(equality.Semantic.DeepEqual(got, BuiltinSizing(SizingProfileMinimal))).To(BeTrue())
	})

	t.Run("profileRef applies base, then profile, then ControlPlane values", func(t *testing.T) {
		g := NewWithT(t)
		profile := &SizingProfile{
			ObjectMeta: metav1.ObjectMeta{Name: "site"},
			Spec: SizingProfileSpec{
				Base: SizingProfileMinimal,
				SizingSpec: SizingSpec{
					Keystone: &KeystoneSizingSpec{API: &APISizingSpec{
						DeploymentSizingSpec: deploymentReplicas(2),
						ProcessSizingSpec:    ProcessSizingSpec{Processes: ptr.To[int32](4)},
					}},
					Cache: &CacheSizingSpec{Replicas: ptr.To[int32](2)},
				},
			},
		}
		c := cp(&ControlPlaneSizingSpec{
			ProfileRef: &SizingProfileRef{Name: "site"},
			SizingSpec: SizingSpec{Keystone: &KeystoneSizingSpec{API: &APISizingSpec{
				ProcessSizingSpec: ProcessSizingSpec{Processes: ptr.To[int32](3)},
			}}},
		})
		got := ResolveSizing(c, profile)
		g.Expect(EffectiveSizingBase(c, profile)).To(Equal(SizingProfileMinimal))
		// From the ControlPlane.
		g.Expect(got.Keystone.API.Processes).To(Equal(ptr.To[int32](3)))
		// From the profile.
		g.Expect(got.Keystone.API.Replicas).To(Equal(ptr.To[int32](2)))
		g.Expect(got.Cache.Replicas).To(Equal(ptr.To[int32](2)))
		// From the Minimal base.
		g.Expect(got.Keystone.API.Threads).To(Equal(ptr.To[int32](1)))
		g.Expect(got.Database.StorageSize).To(Equal("512Mi"))
		// The inputs are unchanged.
		g.Expect(profile.Spec.Keystone.API.Processes).To(Equal(ptr.To[int32](4)))
	})

	t.Run("profileRef with a nil profile resolves the ControlPlane values over Standard", func(t *testing.T) {
		g := NewWithT(t)
		c := cp(&ControlPlaneSizingSpec{
			ProfileRef: &SizingProfileRef{Name: "gone"},
			SizingSpec: SizingSpec{Cache: &CacheSizingSpec{Replicas: ptr.To[int32](5)}},
		})
		got := ResolveSizing(c, nil)
		want := BuiltinSizing(SizingProfileStandard)
		want.Cache.Replicas = ptr.To[int32](5)
		g.Expect(got).To(Equal(want))
	})
}

// skeletonSizing allocates every block of the sizing type v holds, recursing
// through the embedded and pointed-to sizing types, and leaves every value
// unset.
func skeletonSizing(v reflect.Value) {
	for i := range v.NumField() {
		f := v.Field(i)
		switch {
		case f.Kind() == reflect.Struct:
			skeletonSizing(f)
		case f.Kind() == reflect.Pointer && f.Type().Elem().Kind() == reflect.Struct &&
			f.Type().Elem().PkgPath() == v.Type().PkgPath():
			f.Set(reflect.New(f.Type().Elem()))
			skeletonSizing(f.Elem())
		}
	}
}

// fillSizing sets every field of v: pointers are allocated, strings, integers
// and maps take a value, and slices one element, recursively. Resources take
// a CPU request and any other Quantity takes 1, since a Quantity cannot be
// filled field by field.
func fillSizing(v reflect.Value) {
	switch kind := v.Kind(); {
	case v.Type() == reflect.TypeOf(corev1.ResourceRequirements{}):
		v.Set(reflect.ValueOf(*resources(corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}, nil)))
	case v.Type() == reflect.TypeOf(resource.Quantity{}):
		v.Set(reflect.ValueOf(resource.MustParse("1")))
	case kind == reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		fillSizing(v.Elem())
	case kind == reflect.Struct:
		for i := range v.NumField() {
			fillSizing(v.Field(i))
		}
	case kind == reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		fillSizing(v.Index(0))
	case kind == reflect.Map:
		key, elem := reflect.New(v.Type().Key()).Elem(), reflect.New(v.Type().Elem()).Elem()
		fillSizing(key)
		fillSizing(elem)
		v.Set(reflect.MakeMap(v.Type()))
		v.SetMapIndex(key, elem)
	case kind == reflect.String:
		v.SetString("x")
	case kind == reflect.Int32, kind == reflect.Int64:
		v.SetInt(1)
	}
}

// TestMergeSizing_CoversEveryField guards the hand-written merge helpers
// against a field added to the sizing types but not merged: with every block
// set on both sides each helper runs, and a field it skips keeps the
// skeleton's unset value, or loses the base's.
func TestMergeSizing_CoversEveryField(t *testing.T) {
	g := NewWithT(t)
	var full, skeleton SizingSpec
	fillSizing(reflect.ValueOf(&full).Elem())
	skeletonSizing(reflect.ValueOf(&skeleton).Elem())

	g.Expect(MergeSizing(skeleton, full)).To(Equal(full))
	g.Expect(MergeSizing(full, skeleton)).To(Equal(full))
}
