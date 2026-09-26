// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

// sizingScheme is webhookScheme plus the client-go types, so the fake client
// can serve PriorityClasses beside SizingProfiles and ControlPlanes.
func sizingScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := webhookScheme(t)
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("add client-go scheme: %v", err)
	}
	return s
}

func priorityClass(name string) *schedulingv1.PriorityClass {
	return &schedulingv1.PriorityClass{ObjectMeta: metav1.ObjectMeta{Name: name}, Value: 1000}
}

func sizingProfile(name string, base SizingProfileName, spec SizingSpec) *SizingProfile {
	return &SizingProfile{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       SizingProfileSpec{Base: base, SizingSpec: spec},
	}
}

func cpuLimit(q string) *corev1.ResourceRequirements {
	return &corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(q)}}
}

func TestValidateSizingSpec(t *testing.T) {
	path := field.NewPath("spec", "sizing")
	tests := []struct {
		name    string
		spec    SizingSpec
		wantErr []string
	}{
		{name: "empty sizing is valid", spec: SizingSpec{}},
		{name: "both built-in profiles are valid", spec: BuiltinSizing(SizingProfileMinimal)},
		{
			name: "a request above its limit",
			spec: keystoneAPI(withResourcesAPI(resources(
				corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
				corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
			))),
			wantErr: []string{"spec.sizing.keystone.api.resources.requests.cpu: Invalid value"},
		},
		{
			name:    "a bad top-level node selector key",
			spec:    SizingSpec{PodPlacementSpec: PodPlacementSpec{NodeSelector: map[string]string{"bad key": "x"}}},
			wantErr: []string{"spec.sizing.nodeSelector: Invalid value: \"bad key\""},
		},
		{
			name: "a bad component toleration",
			spec: SizingSpec{Cinder: &CinderSizingSpec{Volume: &PinnedSizingSpec{PodPlacementSpec: PodPlacementSpec{
				Tolerations: []corev1.Toleration{{Operator: corev1.TolerationOpEqual, Value: "x"}},
			}}}},
			wantErr: []string{"spec.sizing.cinder.volume.tolerations[0].operator"},
		},
		{
			name:    "database replicas 2",
			spec:    SizingSpec{Database: &DatabaseSizingSpec{Replicas: ptr.To[int32](2)}},
			wantErr: []string{"spec.sizing.database.replicas", "2 cannot hold a majority"},
		},
		{
			name:    "a malformed storage size",
			spec:    SizingSpec{Database: &DatabaseSizingSpec{StorageSize: "10G"}},
			wantErr: []string{"spec.sizing.database.storageSize: Invalid value: \"10G\""},
		},
		{
			name: "a cache memory limit below 96Mi",
			spec: SizingSpec{Cache: &CacheSizingSpec{ContainerSizingSpec: ContainerSizingSpec{
				Resources: &corev1.ResourceRequirements{Limits: corev1.ResourceList{
					corev1.ResourceMemory: resource.MustParse("64Mi"),
				}},
			}}},
			wantErr: []string{
				"spec.sizing.cache.resources.limits.memory",
				"memory limit must be at least 96Mi: the Memcached operator requires maxMemoryMB (64) plus 32Mi",
			},
		},
		{
			name: "a cache memory limit of exactly 96Mi is valid",
			spec: SizingSpec{Cache: &CacheSizingSpec{ContainerSizingSpec: ContainerSizingSpec{
				Resources: &corev1.ResourceRequirements{Limits: corev1.ResourceList{
					corev1.ResourceMemory: resource.MustParse("96Mi"),
				}},
			}}},
		},
		{
			name: "an autoscaling policy type the HPA does not know",
			spec: keystoneAPI(APISizingSpec{Autoscaling: &commonv1.AutoscalingSpec{
				MaxReplicas:          3,
				TargetCPUUtilization: ptr.To[int32](80),
				Behavior: &autoscalingv2.HorizontalPodAutoscalerBehavior{ScaleUp: &autoscalingv2.HPAScalingRules{
					Policies: []autoscalingv2.HPAScalingPolicy{{Type: "Replicas", Value: 1, PeriodSeconds: 15}},
				}},
			}}),
			wantErr: []string{`spec.sizing.keystone.api.autoscaling.behavior.scaleUp.policies[0].type: Unsupported value: "Replicas"`},
		},
		{
			name: "spread entries that break each marker",
			spec: SizingSpec{Nova: &NovaSizingSpec{Scheduler: &WorkerSizingSpec{DeploymentSizingSpec: DeploymentSizingSpec{
				SpreadConstraints: []SpreadConstraintSpec{{MaxSkew: 0, WhenUnsatisfiable: "Sometimes"}},
			}}}},
			wantErr: []string{
				"spec.sizing.nova.scheduler.spreadConstraints[0].maxSkew: Invalid value: 0",
				"spec.sizing.nova.scheduler.spreadConstraints[0].topologyKey: Required value",
				"spec.sizing.nova.scheduler.spreadConstraints[0].whenUnsatisfiable: Unsupported value",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			errs := validateSizingSpec(path, &tc.spec)
			if len(tc.wantErr) == 0 {
				g.Expect(errs).To(BeEmpty())
				return
			}
			g.Expect(errs).NotTo(BeEmpty())
			for _, want := range tc.wantErr {
				g.Expect(errs.ToAggregate().Error()).To(ContainSubstring(want))
			}
		})
	}
}

func withResourcesAPI(rr *corev1.ResourceRequirements) APISizingSpec {
	var api APISizingSpec
	api.Resources = rr
	return api
}

func TestValidateResolvedSizing(t *testing.T) {
	path := field.NewPath("spec", "sizing")

	t.Run("a merged request above its limit", func(t *testing.T) {
		g := NewWithT(t)
		resolved := MergeSizing(BuiltinSizing(SizingProfileMinimal), keystoneAPI(withResourcesAPI(cpuLimit("10m"))))
		errs := validateResolvedSizing(path, &resolved)
		g.Expect(errs).To(HaveLen(1))
		g.Expect(errs[0].Field).To(Equal("spec.sizing.keystone.api.resources.requests.cpu"))
	})

	t.Run("an autoscaling target against a zero request", func(t *testing.T) {
		g := NewWithT(t)
		api := withResourcesAPI(resources(corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("0")}, nil))
		api.Autoscaling = &commonv1.AutoscalingSpec{MaxReplicas: 3, TargetCPUUtilization: ptr.To[int32](80)}
		resolved := MergeSizing(BuiltinSizing(SizingProfileStandard), keystoneAPI(api))
		errs := validateResolvedSizing(path, &resolved)
		g.Expect(errs).To(HaveLen(1))
		g.Expect(errs[0].Field).To(Equal("spec.sizing.keystone.api.resources.requests.cpu"))
	})

	t.Run("the federation proxy meets the Keystone autoscaling target", func(t *testing.T) {
		g := NewWithT(t)
		resolved := SizingSpec{Keystone: &KeystoneSizingSpec{
			API: &APISizingSpec{Autoscaling: &commonv1.AutoscalingSpec{
				MaxReplicas: 3, TargetMemoryUtilization: ptr.To[int32](80),
			}},
			FederationProxy: &ContainerSizingSpec{Resources: resources(
				corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("0")}, nil)},
		}}
		errs := validateResolvedSizing(path, &resolved)
		g.Expect(errs).To(HaveLen(1))
		g.Expect(errs[0].Field).To(Equal("spec.sizing.keystone.federationProxy.resources.requests.memory"))

		// Without a Keystone target the proxy's zero request is harmless.
		resolved.Keystone.API.Autoscaling = nil
		g.Expect(validateResolvedSizing(path, &resolved)).To(BeEmpty())
	})

	t.Run("a memory target above 100 the Keystone API pod cannot reach", func(t *testing.T) {
		g := NewWithT(t)
		api := APISizingSpec{Autoscaling: &commonv1.AutoscalingSpec{
			MinReplicas: ptr.To[int32](1), MaxReplicas: 3, TargetMemoryUtilization: ptr.To[int32](150),
		}}
		resolved := MergeSizing(BuiltinSizing(SizingProfileMinimal), keystoneAPI(api))
		errs := validateResolvedSizing(path, &resolved)
		g.Expect(errs).To(HaveLen(1))
		g.Expect(errs[0].Field).To(Equal("spec.sizing.keystone.api.autoscaling.targetMemoryUtilization"))
		g.Expect(errs[0].Detail).To(ContainSubstring("can never be reached"))

		// A federation proxy that names a memory request without a limit may
		// use more memory than it requests, so the pod can reach the target.
		resolved.Keystone.FederationProxy = &ContainerSizingSpec{Resources: resources(
			corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")}, nil)}
		g.Expect(validateResolvedSizing(path, &resolved)).To(BeEmpty())
	})

	t.Run("a maxReplicas below the resolved replica count", func(t *testing.T) {
		g := NewWithT(t)
		api := withResourcesAPI(resources(corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}, nil))
		api.Autoscaling = &commonv1.AutoscalingSpec{MaxReplicas: 2, TargetCPUUtilization: ptr.To[int32](80)}
		resolved := MergeSizing(BuiltinSizing(SizingProfileStandard), keystoneAPI(api))
		errs := validateResolvedSizing(path, &resolved)
		g.Expect(errs).To(HaveLen(1))
		g.Expect(errs[0].Field).To(Equal("spec.sizing.keystone.api.autoscaling.maxReplicas"))
		g.Expect(errs[0].Detail).To(ContainSubstring("maxReplicas must be >= replicas (3) when minReplicas is not set"))

		// Minimal's single replica fits below it, and so does an explicit
		// minReplicas, which the HPA minimum then takes instead.
		resolved = MergeSizing(BuiltinSizing(SizingProfileMinimal), keystoneAPI(api))
		g.Expect(validateResolvedSizing(path, &resolved)).To(BeEmpty())
		api.Autoscaling.MinReplicas = ptr.To[int32](1)
		resolved = MergeSizing(BuiltinSizing(SizingProfileStandard), keystoneAPI(api))
		g.Expect(validateResolvedSizing(path, &resolved)).To(BeEmpty())
	})

	t.Run("an API that names no replica count is checked against the projected default", func(t *testing.T) {
		g := NewWithT(t)
		resolved := SizingSpec{Horizon: &HorizonSizingSpec{API: &HorizonAPISizingSpec{
			Autoscaling: &commonv1.AutoscalingSpec{MaxReplicas: 2},
		}}}
		errs := validateResolvedSizing(path, &resolved)
		g.Expect(errs).To(HaveLen(1))
		g.Expect(errs[0].Field).To(Equal("spec.sizing.horizon.api.autoscaling.maxReplicas"))
		g.Expect(errs[0].Detail).To(ContainSubstring("replicas (3)"))
	})

	t.Run("both built-in profiles resolve valid", func(t *testing.T) {
		g := NewWithT(t)
		for _, name := range []SizingProfileName{SizingProfileMinimal, SizingProfileStandard} {
			s := BuiltinSizing(name)
			g.Expect(validateResolvedSizing(path, &s)).To(BeEmpty(), string(name))
		}
	})
}

// walkSizing calls visit for every sizing struct reachable from v, with its
// JSON path; an embedded struct shares its parent's path.
func walkSizing(v reflect.Value, path *field.Path, visit func(*field.Path, reflect.Value)) {
	visit(path, v)
	for i := range v.NumField() {
		sf, f := v.Type().Field(i), v.Field(i)
		if f.Kind() == reflect.Pointer {
			if f.IsNil() {
				continue
			}
			f = f.Elem()
		}
		if f.Kind() != reflect.Struct || f.Type().PkgPath() != v.Type().PkgPath() {
			continue
		}
		p := path
		if !sf.Anonymous {
			p = path.Child(strings.Split(sf.Tag.Get("json"), ",")[0])
		}
		walkSizing(f, p, visit)
	}
}

// TestSizingComponents_CoverEveryBlock guards the hand-written component walk
// against a block added to the sizing types but not walked: every container
// gets a request above its limit, every placement a bad node-selector key and
// its own priority class, and each must be reported at its own path.
func TestSizingComponents_CoverEveryBlock(t *testing.T) {
	g := NewWithT(t)
	var s SizingSpec
	skeletonSizing(reflect.ValueOf(&s).Elem())
	var wantResources, wantSelectors, wantClasses []string
	walkSizing(reflect.ValueOf(&s).Elem(), field.NewPath("spec"), func(path *field.Path, v reflect.Value) {
		switch b := v.Addr().Interface().(type) {
		case *ContainerSizingSpec:
			b.Resources = resources(corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
				corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")})
			wantResources = append(wantResources, path.Child("resources", "requests", "cpu").String())
		case *PodPlacementSpec:
			b.NodeSelector = map[string]string{"bad key": "x"}
			b.PriorityClassName = ptr.To(path.String())
			wantSelectors = append(wantSelectors, path.Child("nodeSelector").String())
			wantClasses = append(wantClasses, path.Child("priorityClassName").String())
		case *JobSizingSpec:
			b.PriorityClassName = ptr.To(path.String())
			wantClasses = append(wantClasses, path.Child("priorityClassName").String())
		}
	})

	var gotResources, gotSelectors []string
	for _, err := range validateSizingSpec(field.NewPath("spec"), &s) {
		switch {
		case strings.HasSuffix(err.Field, ".resources.requests.cpu"):
			gotResources = append(gotResources, err.Field)
		case strings.HasSuffix(err.Field, "nodeSelector"):
			gotSelectors = append(gotSelectors, err.Field)
		}
	}
	g.Expect(gotResources).To(ConsistOf(wantResources))
	g.Expect(gotSelectors).To(ConsistOf(wantSelectors))

	var gotClasses []string
	for _, pc := range sizingPriorityClassNames(field.NewPath("spec"), &s) {
		gotClasses = append(gotClasses, pc.path.String())
	}
	g.Expect(gotClasses).To(ConsistOf(wantClasses))
}

func TestSizingPriorityClassNames(t *testing.T) {
	g := NewWithT(t)
	s := SizingSpec{
		PodPlacementSpec: PodPlacementSpec{PriorityClassName: ptr.To("high")},
		Keystone: &KeystoneSizingSpec{
			API:  &APISizingSpec{DeploymentSizingSpec: DeploymentSizingSpec{ScaledSizingSpec: ScaledSizingSpec{PinnedSizingSpec: PinnedSizingSpec{PodPlacementSpec: PodPlacementSpec{PriorityClassName: ptr.To("")}}}}},
			Jobs: &JobSizingSpec{PriorityClassName: ptr.To("batch")},
		},
		Nova: &NovaSizingSpec{ConsoleProxy: &DeploymentSizingSpec{ScaledSizingSpec: ScaledSizingSpec{PinnedSizingSpec: PinnedSizingSpec{PodPlacementSpec: PodPlacementSpec{PriorityClassName: ptr.To("high")}}}}},
	}
	got := sizingPriorityClassNames(field.NewPath("spec"), &s)
	// The empty opt-out names no class, and a repeated name is listed once at
	// its first path.
	g.Expect(got).To(HaveLen(2))
	g.Expect(got[0].name).To(Equal("high"))
	g.Expect(got[0].path.String()).To(Equal("spec.priorityClassName"))
	g.Expect(got[1].name).To(Equal("batch"))
	g.Expect(got[1].path.String()).To(Equal("spec.keystone.jobs.priorityClassName"))
}

func TestSizingInertWarnings(t *testing.T) {
	g := NewWithT(t)
	cp := validControlPlane()
	g.Expect(sizingInertWarnings(cp)).To(BeEmpty())

	cp.Spec.Sizing = &ControlPlaneSizingSpec{SizingSpec: SizingSpec{
		Keystone: &KeystoneSizingSpec{},
		Glance:   &APIServiceSizingSpec{},
		Nova:     &NovaSizingSpec{},
	}}
	// Keystone is declared, so only the two undeclared services warn.
	g.Expect(sizingInertWarnings(cp)).To(ConsistOf(
		"spec.sizing.glance is set but spec.services.glance is not; the values are inert",
		"spec.sizing.nova is set but spec.services.nova is not; the values are inert",
	))

	cp.Spec.Services.Glance = &ServiceGlanceSpec{}
	cp.Spec.Services.Nova = &ServiceNovaSpec{}
	g.Expect(sizingInertWarnings(cp)).To(BeEmpty())
}

func TestValidateCreate_SizingRejections(t *testing.T) {
	withSizing := func(s ControlPlaneSizingSpec) *ControlPlane {
		cp := managedControlPlane()
		cp.Spec.Sizing = &s
		return cp
	}
	tests := []struct {
		name string
		cp   *ControlPlane
		want []string
	}{
		{
			name: "a profileRef naming a missing SizingProfile",
			cp:   withSizing(ControlPlaneSizingSpec{ProfileRef: &SizingProfileRef{Name: "missing"}}),
			want: []string{`spec.sizing.profileRef.name: Not found: "missing"`},
		},
		{
			name: "sizing in External mode",
			cp: func() *ControlPlane {
				cp := externalControlPlane()
				cp.Spec.Sizing = &ControlPlaneSizingSpec{SizingSpec: keystoneAPI(apiReplicas(3))}
				return cp
			}(),
			want: []string{"spec.sizing: Forbidden: forbidden when services.keystone.mode is External (no workload is deployed)"},
		},
		{
			name: "Minimal plus a CPU limit below its request",
			cp: withSizing(ControlPlaneSizingSpec{
				Profile:    SizingProfileMinimal,
				SizingSpec: keystoneAPI(withResourcesAPI(cpuLimit("10m"))),
			}),
			want: []string{"spec.sizing.keystone.api.resources.requests.cpu: Invalid value", "cpu request must not exceed limit (10m)"},
		},
		{
			name: "an autoscaling target against a zero CPU request",
			cp: func() *ControlPlane {
				api := withResourcesAPI(resources(corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("0")}, nil))
				api.Autoscaling = &commonv1.AutoscalingSpec{MaxReplicas: 3, TargetCPUUtilization: ptr.To[int32](80)}
				return withSizing(ControlPlaneSizingSpec{SizingSpec: keystoneAPI(api)})
			}(),
			want: []string{"spec.sizing.keystone.api.resources.requests.cpu: Invalid value", "while targetCPUUtilization is set"},
		},
		{
			name: "an autoscaling maxReplicas below Standard's API replicas",
			cp: func() *ControlPlane {
				api := withResourcesAPI(resources(corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m")}, nil))
				api.Autoscaling = &commonv1.AutoscalingSpec{MaxReplicas: 2, TargetCPUUtilization: ptr.To[int32](80)}
				return withSizing(ControlPlaneSizingSpec{SizingSpec: SizingSpec{Glance: &APIServiceSizingSpec{API: &api}}})
			}(),
			want: []string{"spec.sizing.glance.api.autoscaling.maxReplicas: Invalid value: 2", "maxReplicas must be >= replicas (3)"},
		},
		{
			name: "console-proxy sizing while the proxy is disabled",
			cp: func() *ControlPlane {
				cp := withSizing(ControlPlaneSizingSpec{SizingSpec: SizingSpec{Nova: &NovaSizingSpec{
					ConsoleProxy: &DeploymentSizingSpec{ScaledSizingSpec: ScaledSizingSpec{Replicas: ptr.To[int32](2)}},
				}}})
				cp.Spec.Services.Nova = &ServiceNovaSpec{ConsoleProxy: &ServiceNovaConsoleProxySpec{Enabled: ptr.To(false)}}
				return cp
			}(),
			want: []string{"spec.sizing.nova.consoleProxy: Forbidden: must not be set when services.nova.consoleProxy.enabled is false"},
		},
		{
			name: "database replicas 2",
			cp:   withSizing(ControlPlaneSizingSpec{SizingSpec: SizingSpec{Database: &DatabaseSizingSpec{Replicas: ptr.To[int32](2)}}}),
			want: []string{"spec.sizing.database.replicas: Invalid value: 2"},
		},
		{
			name: "a node selector key with a space",
			cp: withSizing(ControlPlaneSizingSpec{SizingSpec: SizingSpec{
				PodPlacementSpec: PodPlacementSpec{NodeSelector: map[string]string{"bad key": "x"}},
			}}),
			want: []string{`spec.sizing.nodeSelector: Invalid value: "bad key"`},
		},
		{
			name: "a priority class naming no PriorityClass",
			cp: withSizing(ControlPlaneSizingSpec{SizingSpec: SizingSpec{
				PodPlacementSpec: PodPlacementSpec{PriorityClassName: ptr.To("missing")},
			}}),
			want: []string{`spec.sizing.priorityClassName: Not found: "missing"`},
		},
		{
			name: "a cache memory limit of 64Mi",
			cp: withSizing(ControlPlaneSizingSpec{SizingSpec: SizingSpec{Cache: &CacheSizingSpec{
				ContainerSizingSpec: ContainerSizingSpec{Resources: &corev1.ResourceRequirements{
					Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Mi")},
				}},
			}}}),
			want: []string{"spec.sizing.cache.resources.limits.memory", "at least 96Mi"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			c := fake.NewClientBuilder().WithScheme(sizingScheme(t)).Build()
			w := &ControlPlaneWebhook{Client: c}
			_, err := w.ValidateCreate(context.Background(), tc.cp)
			g.Expect(err).To(HaveOccurred())
			for _, want := range tc.want {
				g.Expect(err.Error()).To(ContainSubstring(want))
			}
		})
	}
}

func TestValidateCreate_AcceptsResolvableSizing(t *testing.T) {
	g := NewWithT(t)
	c := fake.NewClientBuilder().WithScheme(sizingScheme(t)).WithObjects(
		priorityClass("high"),
		sizingProfile("site", SizingProfileMinimal, SizingSpec{Cache: &CacheSizingSpec{Replicas: ptr.To[int32](2)}}),
	).Build()
	w := &ControlPlaneWebhook{Client: c}
	cp := managedControlPlane()
	cp.Spec.Sizing = &ControlPlaneSizingSpec{
		ProfileRef: &SizingProfileRef{Name: "site"},
		SizingSpec: SizingSpec{PodPlacementSpec: PodPlacementSpec{PriorityClassName: ptr.To("high")}},
	}
	warnings, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(warnings).To(BeEmpty())
}

func TestValidateCreate_SizingErrorReportedOnce(t *testing.T) {
	g := NewWithT(t)
	w := &ControlPlaneWebhook{Client: fake.NewClientBuilder().WithScheme(sizingScheme(t)).Build()}
	cp := managedControlPlane()
	// The raw value already breaks the rule, and the merged value repeats it:
	// validate() reports it and the merged check must not report it again.
	cp.Spec.Sizing = &ControlPlaneSizingSpec{SizingSpec: keystoneAPI(withResourcesAPI(resources(
		corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
		corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
	)))}
	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	var status apierrors.APIStatus
	g.Expect(errors.As(err, &status)).To(BeTrue())
	g.Expect(status.Status().Details.Causes).To(HaveLen(1))
}

func TestValidateSizing_ProfileGetErrorIsInternal(t *testing.T) {
	g := NewWithT(t)
	c := fake.NewClientBuilder().WithScheme(sizingScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*SizingProfile); ok {
				return apierrors.NewServiceUnavailable("etcd is down")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}).Build()
	w := &ControlPlaneWebhook{Client: c}
	cp := managedControlPlane()
	cp.Spec.Sizing = &ControlPlaneSizingSpec{ProfileRef: &SizingProfileRef{Name: "site"}}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("spec.sizing.profileRef.name: Internal error"))
	g.Expect(err.Error()).To(ContainSubstring("etcd is down"))

	// An update that keeps the name but changes spec.sizing cannot skip the
	// merged checks on a profile that may exist, so it is rejected too.
	newCP := cp.DeepCopy()
	newCP.Spec.Sizing.Cache = &CacheSizingSpec{Replicas: ptr.To[int32](2)}
	_, err = w.ValidateUpdate(context.Background(), cp, newCP)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("spec.sizing.profileRef.name: Internal error"))

	// An update that leaves spec.sizing alone reads no profile.
	newCP = cp.DeepCopy()
	newCP.Spec.RegionDescription = "changed"
	_, err = w.ValidateUpdate(context.Background(), cp, newCP)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestValidateCreate_SizingWarnings(t *testing.T) {
	g := NewWithT(t)
	w := &ControlPlaneWebhook{}
	cp := managedControlPlane()
	cp.Spec.Sizing = &ControlPlaneSizingSpec{SizingSpec: SizingSpec{Horizon: &HorizonSizingSpec{}}}
	warnings, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(warnings).To(ContainElement("spec.sizing.horizon is set but spec.services.horizon is not; the values are inert"))

	oldCP := cp.DeepCopy()
	warnings, err = w.ValidateUpdate(context.Background(), oldCP, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(warnings).To(ContainElement("spec.sizing.horizon is set but spec.services.horizon is not; the values are inert"))
}

func TestValidateUpdate_UnresolvableProfileRefUnchangedAdmitted(t *testing.T) {
	g := NewWithT(t)
	// The profile the ControlPlane names was deleted after admission.
	w := &ControlPlaneWebhook{Client: fake.NewClientBuilder().WithScheme(sizingScheme(t)).Build()}
	oldCP := managedControlPlane()
	oldCP.Spec.Sizing = &ControlPlaneSizingSpec{ProfileRef: &SizingProfileRef{Name: "gone"}}
	oldCP.Finalizers = []string{"c5c3.io/controlplane"}
	newCP := oldCP.DeepCopy()
	newCP.Finalizers = nil

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).NotTo(HaveOccurred())

	// An update that changes other sizing values keeps admitting the name it
	// cannot resolve.
	newCP.Spec.Sizing.Cache = &CacheSizingSpec{Replicas: ptr.To[int32](2)}
	_, err = w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).NotTo(HaveOccurred())

	// Renaming the reference is checked again.
	newCP.Spec.Sizing.ProfileRef.Name = "also-gone"
	_, err = w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring(`spec.sizing.profileRef.name: Not found: "also-gone"`))
}

func TestValidateSizing_MissingProfileSkipsMergedSizing(t *testing.T) {
	g := NewWithT(t)
	// "site" set base Minimal, one replica per API, and was deleted after the
	// ControlPlane was admitted with a maxReplicas that one replica fits.
	w := &ControlPlaneWebhook{Client: fake.NewClientBuilder().WithScheme(sizingScheme(t)).Build()}
	api := withResourcesAPI(resources(corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}, nil))
	api.Autoscaling = &commonv1.AutoscalingSpec{MaxReplicas: 2, TargetCPUUtilization: ptr.To[int32](80)}
	oldCP := managedControlPlane()
	oldCP.Spec.Sizing = &ControlPlaneSizingSpec{ProfileRef: &SizingProfileRef{Name: "site"}, SizingSpec: keystoneAPI(api)}

	// Without the profile the merge is unknown, so an edit is not checked
	// against Standard's three replicas, a count no child ever projects.
	newCP := oldCP.DeepCopy()
	newCP.Spec.Sizing.Keystone.API.Resources.Requests[corev1.ResourceCPU] = resource.MustParse("200m")
	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).NotTo(HaveOccurred())

	// The written values are still checked on their own.
	newCP.Spec.Sizing.Keystone.API.Resources.Limits = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}
	_, err = w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("spec.sizing.keystone.api.resources.requests.cpu: Invalid value"))

	// A create naming a missing profile reports the missing name alone.
	_, err = w.ValidateCreate(context.Background(), oldCP)
	g.Expect(err).To(HaveOccurred())
	var status apierrors.APIStatus
	g.Expect(errors.As(err, &status)).To(BeTrue())
	g.Expect(status.Status().Details.Causes).To(HaveLen(1))
	g.Expect(err.Error()).To(ContainSubstring(`spec.sizing.profileRef.name: Not found: "site"`))
}

func TestValidateUpdate_PriorityClassLookedUpOnlyWhenChanged(t *testing.T) {
	g := NewWithT(t)
	gets := map[string]int{}
	// "kept" was deleted after the ControlPlane was admitted; only "new" exists.
	c := fake.NewClientBuilder().WithScheme(sizingScheme(t)).WithObjects(priorityClass("new")).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*schedulingv1.PriorityClass); ok {
					gets[key.Name]++
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).Build()
	w := &ControlPlaneWebhook{Client: c}
	oldCP := managedControlPlane()
	oldCP.Spec.Sizing = &ControlPlaneSizingSpec{SizingSpec: SizingSpec{
		PodPlacementSpec: PodPlacementSpec{PriorityClassName: ptr.To("kept")},
	}}

	// An unrelated update keeps the deleted class and looks nothing up.
	newCP := oldCP.DeepCopy()
	newCP.Spec.RegionDescription = "changed"
	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(gets).To(BeEmpty())

	// Adding a component class looks up only the new name.
	newCP.Spec.Sizing.Keystone = &KeystoneSizingSpec{Jobs: &JobSizingSpec{PriorityClassName: ptr.To("new")}}
	_, err = w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(gets).To(Equal(map[string]int{"new": 1}))
}

func TestDefault_ResolvesDatabaseSizing(t *testing.T) {
	// dedicated returns a managed ControlPlane whose Keystone and Glance run
	// dedicated databases and a dedicated Keystone cache, none of them sized.
	dedicated := func(sizing *ControlPlaneSizingSpec) *ControlPlane {
		cp := managedControlPlane()
		cp.Name, cp.Namespace = "cp", "openstack"
		cp.Spec.Sizing = sizing
		cp.Spec.Infrastructure.Messaging = &commonv1.MessagingSpec{}
		cp.Spec.Services.Keystone.DedicatedBackingServices = &KeystoneDedicatedBackingServicesSpec{
			Database: &commonv1.DatabaseSpec{},
			Cache:    &commonv1.CacheSpec{},
		}
		cp.Spec.Services.Glance = &ServiceGlanceSpec{
			DedicatedBackingServices: &GlanceDedicatedBackingServicesSpec{Database: &commonv1.DatabaseSpec{}},
		}
		return cp
	}
	databases := func(cp *ControlPlane) []*commonv1.DatabaseSpec {
		return []*commonv1.DatabaseSpec{
			&cp.Spec.Infrastructure.Database,
			cp.Spec.Services.Keystone.DedicatedBackingServices.Database,
			cp.Spec.Services.Glance.DedicatedBackingServices.Database,
		}
	}
	site := sizingProfile("site", SizingProfileMinimal, SizingSpec{
		Database: &DatabaseSizingSpec{Replicas: ptr.To[int32](3), StorageSize: "2Gi"},
	})

	tests := []struct {
		name         string
		sizing       *ControlPlaneSizingSpec
		wantReplicas int32
		wantStorage  string
	}{
		{name: "no sizing is Standard", wantReplicas: 3, wantStorage: "100Gi"},
		{name: "profile Minimal", sizing: &ControlPlaneSizingSpec{Profile: SizingProfileMinimal}, wantReplicas: 1, wantStorage: "512Mi"},
		{
			name:         "a profileRef takes the SizingProfile's values",
			sizing:       &ControlPlaneSizingSpec{ProfileRef: &SizingProfileRef{Name: "site"}},
			wantReplicas: 3, wantStorage: "2Gi",
		},
		{
			name: "the ControlPlane's own values win over the profile",
			sizing: &ControlPlaneSizingSpec{
				ProfileRef: &SizingProfileRef{Name: "site"},
				SizingSpec: SizingSpec{Database: &DatabaseSizingSpec{StorageSize: "5Gi"}},
			},
			wantReplicas: 3, wantStorage: "5Gi",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			w := &ControlPlaneWebhook{Client: fake.NewClientBuilder().WithScheme(sizingScheme(t)).WithObjects(site).Build()}
			cp := dedicated(tc.sizing)
			g.Expect(w.Default(context.Background(), cp)).To(Succeed())
			for i, db := range databases(cp) {
				g.Expect(db.Replicas).To(Equal(tc.wantReplicas), "database %d", i)
				g.Expect(db.StorageSize).To(Equal(tc.wantStorage), "database %d", i)
			}
			// Cache and bus replicas follow the sizing on every reconcile, so
			// Default never freezes them.
			g.Expect(cp.Spec.Infrastructure.Cache.Replicas).To(BeZero())
			g.Expect(cp.Spec.Services.Keystone.DedicatedBackingServices.Cache.Replicas).To(BeZero())
			g.Expect(cp.Spec.Infrastructure.Messaging.Replicas).To(BeZero())
		})
	}

	t.Run("an explicit value is never overwritten", func(t *testing.T) {
		g := NewWithT(t)
		w := &ControlPlaneWebhook{}
		cp := dedicated(&ControlPlaneSizingSpec{Profile: SizingProfileMinimal})
		cp.Spec.Infrastructure.Database.Replicas = 3
		cp.Spec.Services.Glance.DedicatedBackingServices.Database.StorageSize = "7Gi"
		g.Expect(w.Default(context.Background(), cp)).To(Succeed())
		g.Expect(cp.Spec.Infrastructure.Database.Replicas).To(Equal(int32(3)))
		g.Expect(cp.Spec.Infrastructure.Database.StorageSize).To(Equal("512Mi"))
		g.Expect(cp.Spec.Services.Glance.DedicatedBackingServices.Database.Replicas).To(Equal(int32(1)))
		g.Expect(cp.Spec.Services.Glance.DedicatedBackingServices.Database.StorageSize).To(Equal("7Gi"))
	})

	t.Run("a missing SizingProfile leaves both fields unset", func(t *testing.T) {
		g := NewWithT(t)
		w := &ControlPlaneWebhook{Client: fake.NewClientBuilder().WithScheme(sizingScheme(t)).Build()}
		cp := dedicated(&ControlPlaneSizingSpec{ProfileRef: &SizingProfileRef{Name: "missing"}})
		g.Expect(w.Default(context.Background(), cp)).To(Succeed())
		for i, db := range databases(cp) {
			g.Expect(db.Replicas).To(BeZero(), "database %d", i)
			g.Expect(db.StorageSize).To(BeEmpty(), "database %d", i)
		}
	})

	t.Run("any other read error rejects the request", func(t *testing.T) {
		g := NewWithT(t)
		c := fake.NewClientBuilder().WithScheme(sizingScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return apierrors.NewServiceUnavailable("etcd is down")
			},
		}).Build()
		w := &ControlPlaneWebhook{Client: c}
		cp := dedicated(&ControlPlaneSizingSpec{ProfileRef: &SizingProfileRef{Name: "site"}})
		err := w.Default(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring(`reading SizingProfile "site"`))
	})

	t.Run("an object with nothing to fill never reads the profile", func(t *testing.T) {
		g := NewWithT(t)
		c := fake.NewClientBuilder().WithScheme(sizingScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return apierrors.NewServiceUnavailable("etcd is down")
			},
		}).Build()
		w := &ControlPlaneWebhook{Client: c}
		cp := dedicated(&ControlPlaneSizingSpec{ProfileRef: &SizingProfileRef{Name: "site"}})
		for _, db := range databases(cp) {
			db.Replicas, db.StorageSize = 3, "2Gi"
		}
		// Every later update, the finalizer removal included, looks like this.
		g.Expect(w.Default(context.Background(), cp)).To(Succeed())
	})

	t.Run("External mode provisions no database and fills nothing", func(t *testing.T) {
		g := NewWithT(t)
		cp := externalControlPlane()
		g.Expect((&ControlPlaneWebhook{}).Default(context.Background(), cp)).To(Succeed())
		g.Expect(cp.Spec.Infrastructure).To(BeNil())
	})
}

func TestValidateUpdate_StandardToMinimalKeepsStoredDatabase(t *testing.T) {
	g := NewWithT(t)
	w := &ControlPlaneWebhook{Client: fake.NewClientBuilder().WithScheme(sizingScheme(t)).Build()}
	oldCP := managedControlPlane()
	oldCP.Name, oldCP.Namespace = "cp", "openstack"
	g.Expect(w.Default(context.Background(), oldCP)).To(Succeed())
	g.Expect(oldCP.Spec.Infrastructure.Database.Replicas).To(Equal(int32(3)))
	g.Expect(oldCP.Spec.Infrastructure.Database.StorageSize).To(Equal("100Gi"))

	newCP := oldCP.DeepCopy()
	newCP.Spec.Sizing = &ControlPlaneSizingSpec{Profile: SizingProfileMinimal}
	g.Expect(w.Default(context.Background(), newCP)).To(Succeed())
	// The stored values are explicit now, so Minimal never overwrites them and
	// the immutability check sees no change.
	g.Expect(newCP.Spec.Infrastructure.Database.Replicas).To(Equal(int32(3)))
	g.Expect(newCP.Spec.Infrastructure.Database.StorageSize).To(Equal("100Gi"))
	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestValidateUpdate_NewDedicatedDatabaseNeedsResolvableSizing(t *testing.T) {
	g := NewWithT(t)
	// The profile the ControlPlane names was deleted after admission.
	w := &ControlPlaneWebhook{Client: fake.NewClientBuilder().WithScheme(sizingScheme(t)).Build()}
	oldCP := managedControlPlane()
	oldCP.Name, oldCP.Namespace = "cp", "openstack"
	oldCP.Spec.Sizing = &ControlPlaneSizingSpec{ProfileRef: &SizingProfileRef{Name: "gone"}}
	oldCP.Spec.Infrastructure.Database.Replicas, oldCP.Spec.Infrastructure.Database.StorageSize = 3, "100Gi"

	// Adding a service with a dedicated database leaves both values unset.
	newCP := oldCP.DeepCopy()
	newCP.Spec.Services.Placement = &ServicePlacementSpec{
		DedicatedBackingServices: &PlacementDedicatedBackingServicesSpec{Database: &commonv1.DatabaseSpec{}},
	}
	g.Expect(w.Default(context.Background(), newCP)).To(Succeed())
	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).To(HaveOccurred())
	dbPath := "spec.services.placement.dedicatedBackingServices.database"
	g.Expect(err.Error()).To(ContainSubstring(dbPath + `.replicas: Required value: must be set while SizingProfile "gone" does not exist`))
	g.Expect(err.Error()).To(ContainSubstring(dbPath + ".storageSize: Required value"))

	// Explicit values need no profile.
	db := newCP.Spec.Services.Placement.DedicatedBackingServices.Database
	db.Replicas, db.StorageSize = 1, "1Gi"
	_, err = w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestValidateUpdate_StoredZeroDatabaseReplicasMigratesOnce(t *testing.T) {
	g := NewWithT(t)
	w := &ControlPlaneWebhook{Client: fake.NewClientBuilder().WithScheme(sizingScheme(t)).Build()}
	// An older webhook admitted the ControlPlane against a CRD that no longer
	// defaults database.replicas, so it is stored as 0, and the reconciler
	// provisioned three replicas for it.
	oldCP := managedControlPlane()
	oldCP.Name, oldCP.Namespace = "cp", "openstack"

	// Any update, the finalizer removal included, fills Standard's count.
	newCP := oldCP.DeepCopy()
	g.Expect(w.Default(context.Background(), newCP)).To(Succeed())
	g.Expect(newCP.Spec.Infrastructure.Database.Replicas).To(Equal(int32(3)))
	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).NotTo(HaveOccurred())

	// Any other count is still a change of the running database.
	newCP.Spec.Infrastructure.Database.Replicas = 1
	_, err = w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("database replicas is immutable after creation"))

	// A new 0, which Default leaves only when it cannot resolve the count, is
	// not normalized.
	stored := newCP.DeepCopy()
	stored.Spec.Infrastructure.Database.Replicas = 3
	newCP.Spec.Infrastructure.Database.Replicas = 0
	_, err = w.ValidateUpdate(context.Background(), stored, newCP)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("database replicas is immutable after creation"))
}
