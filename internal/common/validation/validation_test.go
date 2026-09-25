// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package validation

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
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

var testPath = field.NewPath("spec", "test")

func TestDatabaseXOR(t *testing.T) {
	cases := []struct {
		name    string
		db      commonv1.DatabaseSpec
		wantErr bool
	}{
		{"managed mode valid", commonv1.DatabaseSpec{ClusterRef: &corev1.LocalObjectReference{Name: "db"}}, false},
		{"brownfield mode valid", commonv1.DatabaseSpec{Host: "db.example.com"}, false},
		{"both set rejected", commonv1.DatabaseSpec{ClusterRef: &corev1.LocalObjectReference{Name: "db"}, Host: "db.example.com"}, true},
		{"neither set rejected", commonv1.DatabaseSpec{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			errs := DatabaseXOR(testPath, &tc.db)
			g.Expect(len(errs) > 0).To(gomega.Equal(tc.wantErr))
		})
	}
}

func TestCacheXOR(t *testing.T) {
	cases := []struct {
		name    string
		cache   commonv1.CacheSpec
		wantErr bool
	}{
		{"managed mode valid", commonv1.CacheSpec{ClusterRef: &corev1.LocalObjectReference{Name: "mc"}}, false},
		{"brownfield mode valid", commonv1.CacheSpec{Servers: []string{"mc-0:11211"}}, false},
		{"both set rejected", commonv1.CacheSpec{ClusterRef: &corev1.LocalObjectReference{Name: "mc"}, Servers: []string{"mc-0:11211"}}, true},
		// An explicitly empty servers list is NOT a brownfield configuration:
		// with no clusterRef either, the spec has no usable cache source.
		{"neither set rejected", commonv1.CacheSpec{Servers: []string{}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			errs := CacheXOR(testPath, &tc.cache)
			g.Expect(len(errs) > 0).To(gomega.Equal(tc.wantErr))
		})
	}
}

func TestMessagingXOR(t *testing.T) {
	cases := []struct {
		name      string
		messaging commonv1.MessagingSpec
		wantErr   bool
	}{
		{"managed mode valid", commonv1.MessagingSpec{ClusterRef: &corev1.LocalObjectReference{Name: "rabbitmq"}}, false},
		{"brownfield mode valid", commonv1.MessagingSpec{SecretRef: &commonv1.SecretRefSpec{Name: "transport-url"}}, false},
		{
			"both set rejected",
			commonv1.MessagingSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: "rabbitmq"},
				SecretRef:  &commonv1.SecretRefSpec{Name: "transport-url"},
			},
			true,
		},
		{"neither set rejected", commonv1.MessagingSpec{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			errs := MessagingXOR(testPath, &tc.messaging)
			if !tc.wantErr {
				g.Expect(errs).To(gomega.BeEmpty())
				return
			}
			g.Expect(errs).To(gomega.HaveLen(1))
			g.Expect(errs[0].Field).To(gomega.Equal(testPath.String()))
			g.Expect(errs[0].Error()).To(gomega.ContainSubstring("exactly one of clusterRef or secretRef must be set"))
		})
	}
}

func TestCacheNoControlChars(t *testing.T) {
	cases := []struct {
		name    string
		cache   commonv1.CacheSpec
		wantErr bool
	}{
		{"managed mode clean", commonv1.CacheSpec{ClusterRef: &corev1.LocalObjectReference{Name: "mc"}}, false},
		{"brownfield mode clean", commonv1.CacheSpec{Servers: []string{"mc-0:11211", "mc-1:11211"}}, false},
		// Both shapes end up in [keystone_authtoken].memcached_servers, so a
		// newline smuggles an attacker-controlled auth_url into the section.
		{
			"server with a newline rejected",
			commonv1.CacheSpec{Servers: []string{"mc-0:11211\nauth_url = http://attacker.example/v3"}},
			true,
		},
		{
			"server with a carriage return rejected",
			commonv1.CacheSpec{Servers: []string{"mc-0:11211", "mc-1:11211\rregion_name = elsewhere"}},
			true,
		},
		{
			"clusterRef name with a newline rejected",
			commonv1.CacheSpec{ClusterRef: &corev1.LocalObjectReference{Name: "mc\nauth_url = http://attacker.example/v3"}},
			true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			errs := CacheNoControlChars(testPath, &tc.cache)
			g.Expect(len(errs) > 0).To(gomega.Equal(tc.wantErr))
		})
	}
}

func TestDynamicCredentialsRequireClusterRef(t *testing.T) {
	g := gomega.NewWithT(t)

	dynamicBrownfield := commonv1.DatabaseSpec{CredentialsMode: commonv1.CredentialsModeDynamic, Host: "db"}
	errs := DynamicCredentialsRequireClusterRef(testPath, &dynamicBrownfield)
	g.Expect(errs).To(gomega.HaveLen(1))
	g.Expect(errs[0].Field).To(gomega.Equal("spec.test.credentialsMode"))

	dynamicManaged := commonv1.DatabaseSpec{CredentialsMode: commonv1.CredentialsModeDynamic, ClusterRef: &corev1.LocalObjectReference{Name: "db"}}
	g.Expect(DynamicCredentialsRequireClusterRef(testPath, &dynamicManaged)).To(gomega.BeEmpty())

	staticBrownfield := commonv1.DatabaseSpec{CredentialsMode: commonv1.CredentialsModeStatic, Host: "db"}
	g.Expect(DynamicCredentialsRequireClusterRef(testPath, &staticBrownfield)).To(gomega.BeEmpty())
}

func TestCronSchedule(t *testing.T) {
	g := gomega.NewWithT(t)

	g.Expect(CronSchedule(testPath, "0 0 * * 0")).To(gomega.BeEmpty())
	g.Expect(CronSchedule(testPath, "@hourly")).To(gomega.BeEmpty())

	errs := CronSchedule(testPath, "not-a-cron")
	g.Expect(errs).To(gomega.HaveLen(1))
	g.Expect(errs[0].Detail).To(gomega.ContainSubstring("invalid cron expression"))

	// An empty schedule is a parse error here — the required-vs-defaulted
	// decision (and its message) is the caller's per-field policy.
	g.Expect(CronSchedule(testPath, "")).To(gomega.HaveLen(1))

	// cron.ParseStandard accepts a time-zone prefix, but the CronJob API refuses
	// one in spec.schedule, so both spellings are rejected here.
	for _, schedule := range []string{"CRON_TZ=UTC 0 0 * * *", "TZ=Europe/Berlin @daily"} {
		errs = CronSchedule(testPath, schedule)
		g.Expect(errs).To(gomega.HaveLen(1), schedule)
		g.Expect(errs[0].Detail).To(gomega.ContainSubstring("TZ and CRON_TZ are not allowed"))
	}
}

func TestTopologySpreadSelector(t *testing.T) {
	required := map[string]string{
		"app.kubernetes.io/name":     "keystone",
		"app.kubernetes.io/instance": "ks",
	}
	tsc := func(sel *metav1.LabelSelector) corev1.TopologySpreadConstraint {
		return corev1.TopologySpreadConstraint{
			MaxSkew:           1,
			TopologyKey:       "kubernetes.io/hostname",
			WhenUnsatisfiable: corev1.ScheduleAnyway,
			LabelSelector:     sel,
		}
	}

	cases := []struct {
		name     string
		tscs     []corev1.TopologySpreadConstraint
		wantErrs int
	}{
		{"matching selector valid", []corev1.TopologySpreadConstraint{
			tsc(&metav1.LabelSelector{MatchLabels: required}),
		}, 0},
		{"missing selector rejected", []corev1.TopologySpreadConstraint{tsc(nil)}, 1},
		{"wrong labels rejected", []corev1.TopologySpreadConstraint{
			tsc(&metav1.LabelSelector{MatchLabels: map[string]string{"app": "other"}}),
		}, 1},
		{"matchExpressions rejected", []corev1.TopologySpreadConstraint{
			tsc(&metav1.LabelSelector{
				MatchLabels: required,
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: "zone", Operator: metav1.LabelSelectorOpExists},
				},
			}),
		}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			g.Expect(TopologySpreadSelector(testPath, tc.tscs, required)).To(gomega.HaveLen(tc.wantErrs))
		})
	}
}

func TestPriorityClassExists(t *testing.T) {
	g := gomega.NewWithT(t)

	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).
		WithObjects(&schedulingv1.PriorityClass{ObjectMeta: metav1.ObjectMeta{Name: "critical"}}).
		Build()

	g.Expect(PriorityClassExists(context.Background(), c, testPath, "critical")).To(gomega.BeEmpty())

	errs := PriorityClassExists(context.Background(), c, testPath, "typo")
	g.Expect(errs).To(gomega.HaveLen(1))
	g.Expect(errs[0].Type).To(gomega.Equal(field.ErrorTypeNotFound))

	// A nil Reader (programmatically constructed webhook without a client)
	// and an empty name both skip the lookup rather than failing closed.
	g.Expect(PriorityClassExists(context.Background(), nil, testPath, "critical")).To(gomega.BeEmpty())
	g.Expect(PriorityClassExists(context.Background(), c, testPath, "")).To(gomega.BeEmpty())
}

func TestNodeSelectorLabels(t *testing.T) {
	g := gomega.NewWithT(t)

	g.Expect(NodeSelectorLabels(testPath, nil)).To(gomega.BeEmpty())
	g.Expect(NodeSelectorLabels(testPath, map[string]string{})).To(gomega.BeEmpty())
	g.Expect(NodeSelectorLabels(testPath, map[string]string{"kubernetes.io/os": "linux"})).To(gomega.BeEmpty())

	errs := NodeSelectorLabels(testPath, map[string]string{"bad key": "x"})
	g.Expect(errs).To(gomega.HaveLen(1))
	g.Expect(errs[0].Type).To(gomega.Equal(field.ErrorTypeInvalid))
	g.Expect(errs[0].Field).To(gomega.Equal(testPath.String()))

	errs = NodeSelectorLabels(testPath, map[string]string{"a": "-bad-"})
	g.Expect(errs).To(gomega.HaveLen(1))
	g.Expect(errs[0].Type).To(gomega.Equal(field.ErrorTypeInvalid))
	g.Expect(errs[0].Field).To(gomega.Equal(testPath.Key("a").String()))
}

func TestTolerations(t *testing.T) {
	path := field.NewPath("spec", "deployment", "tolerations")
	accepted := []struct {
		name       string
		toleration corev1.Toleration
	}{
		{"key with Exists", corev1.Toleration{Key: "a", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule}},
		{"empty key with Exists matches all", corev1.Toleration{Operator: corev1.TolerationOpExists}},
		{"Equal with NoExecute and seconds", corev1.Toleration{Key: "a", Operator: corev1.TolerationOpEqual, Value: "b", Effect: corev1.TaintEffectNoExecute, TolerationSeconds: ptr.To(int64(30))}},
		{"Gt with a numeric value", corev1.Toleration{Key: "a", Operator: corev1.TolerationOpGt, Value: "5"}},
		{"Lt with a negative value", corev1.Toleration{Key: "a", Operator: corev1.TolerationOpLt, Value: "-3"}},
		{"no operator means Equal", corev1.Toleration{Key: "a", Value: "b"}},
	}
	for _, tc := range accepted {
		t.Run("accepts "+tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			g.Expect(Tolerations(path, []corev1.Toleration{tc.toleration})).To(gomega.BeEmpty())
		})
	}

	rejected := []struct {
		name       string
		toleration corev1.Toleration
		wantField  string
		wantType   field.ErrorType
	}{
		{"invalid key", corev1.Toleration{Key: "bad key", Operator: corev1.TolerationOpExists}, "[0].key", field.ErrorTypeInvalid},
		{"empty key with Equal", corev1.Toleration{Operator: corev1.TolerationOpEqual}, "[0].operator", field.ErrorTypeInvalid},
		{"Exists with a value", corev1.Toleration{Key: "a", Operator: corev1.TolerationOpExists, Value: "b"}, "[0].operator", field.ErrorTypeInvalid},
		{"unknown operator", corev1.Toleration{Key: "a", Operator: "Foo"}, "[0].operator", field.ErrorTypeNotSupported},
		{"unknown effect", corev1.Toleration{Key: "a", Operator: corev1.TolerationOpExists, Effect: "Bad"}, "[0].effect", field.ErrorTypeNotSupported},
		{"seconds without NoExecute", corev1.Toleration{Key: "a", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule, TolerationSeconds: ptr.To(int64(30))}, "[0].effect", field.ErrorTypeInvalid},
		{"invalid Equal value", corev1.Toleration{Key: "a", Operator: corev1.TolerationOpEqual, Value: "-bad-"}, "[0].value", field.ErrorTypeInvalid},
		{"Gt with a non-numeric value", corev1.Toleration{Key: "a", Operator: corev1.TolerationOpGt, Value: "high"}, "[0].value", field.ErrorTypeInvalid},
		{"Lt with an empty value", corev1.Toleration{Key: "a", Operator: corev1.TolerationOpLt}, "[0].value", field.ErrorTypeInvalid},
	}
	for _, tc := range rejected {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			errs := Tolerations(path, []corev1.Toleration{tc.toleration})
			g.Expect(errs).To(gomega.HaveLen(1), "%v", errs)
			g.Expect(errs[0].Field).To(gomega.Equal(path.String() + tc.wantField))
			g.Expect(errs[0].Type).To(gomega.Equal(tc.wantType))
		})
	}
}

func TestNodePlacement(t *testing.T) {
	g := gomega.NewWithT(t)
	path := field.NewPath("spec", "deployment")

	g.Expect(NodePlacement(path, nil)).To(gomega.BeEmpty())
	g.Expect(NodePlacement(path, &commonv1.NodePlacementSpec{})).To(gomega.BeEmpty())

	errs := NodePlacement(path, &commonv1.NodePlacementSpec{
		NodeSelector: map[string]string{"bad key": "x"},
		Tolerations:  []corev1.Toleration{{Operator: corev1.TolerationOpEqual}},
		// Affinity is left to the API server: an empty term passes here.
		Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{}},
	})
	g.Expect(errs).To(gomega.HaveLen(2))
	g.Expect(errs[0].Field).To(gomega.Equal("spec.deployment.nodeSelector"))
	g.Expect(errs[1].Field).To(gomega.Equal("spec.deployment.tolerations[0].operator"))
}

func TestRequestsWithinLimits(t *testing.T) {
	g := gomega.NewWithT(t)
	path := field.NewPath("spec", "jobs", "resources")

	g.Expect(RequestsWithinLimits(path, nil)).To(gomega.BeEmpty())
	// A request without a limit of the same resource is not compared.
	g.Expect(RequestsWithinLimits(path, &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
	})).To(gomega.BeEmpty())

	errs := RequestsWithinLimits(path, &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
	})
	g.Expect(errs).To(gomega.HaveLen(1))
	g.Expect(errs[0].Type).To(gomega.Equal(field.ErrorTypeInvalid))
	g.Expect(errs[0].Field).To(gomega.Equal("spec.jobs.resources.requests.cpu"))
	g.Expect(errs[0].Detail).To(gomega.Equal("cpu request must not exceed limit (1)"))
}

func TestAutoscalingTargetRequests(t *testing.T) {
	path := field.NewPath("spec", "deployment", "resources")
	cpuTarget := &commonv1.AutoscalingSpec{MaxReplicas: 5, TargetCPUUtilization: ptr.To(int32(80))}
	memTarget := &commonv1.AutoscalingSpec{MaxReplicas: 5, TargetMemoryUtilization: ptr.To(int32(70))}
	bothTargets := &commonv1.AutoscalingSpec{
		MaxReplicas:             5,
		TargetCPUUtilization:    ptr.To(int32(80)),
		TargetMemoryUtilization: ptr.To(int32(70)),
	}
	list := func(name corev1.ResourceName, q string) corev1.ResourceList {
		return corev1.ResourceList{name: resource.MustParse(q)}
	}

	accepted := []struct {
		name string
		rr   *corev1.ResourceRequirements
		a    *commonv1.AutoscalingSpec
	}{
		{name: "nil autoscaling", rr: &corev1.ResourceRequirements{Requests: list(corev1.ResourceCPU, "0")}},
		{name: "nil resources", a: cpuTarget},
		{name: "empty resources", rr: &corev1.ResourceRequirements{}, a: bothTargets},
		{name: "positive cpu request", rr: &corev1.ResourceRequirements{Requests: list(corev1.ResourceCPU, "100m")}, a: cpuTarget},
		{name: "positive cpu limit without request", rr: &corev1.ResourceRequirements{Limits: list(corev1.ResourceCPU, "1")}, a: cpuTarget},
		{name: "zero cpu request with only a memory target", rr: &corev1.ResourceRequirements{Requests: list(corev1.ResourceCPU, "0")}, a: memTarget},
		{
			name: "positive cpu request beside a zero limit",
			rr:   &corev1.ResourceRequirements{Requests: list(corev1.ResourceCPU, "100m"), Limits: list(corev1.ResourceCPU, "0")},
			a:    cpuTarget,
		},
	}
	for _, tc := range accepted {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			g.Expect(AutoscalingTargetRequests(path, tc.rr, tc.a)).To(gomega.BeEmpty())
		})
	}

	t.Run("zero cpu request with a cpu target", func(t *testing.T) {
		g := gomega.NewWithT(t)
		errs := AutoscalingTargetRequests(path, &corev1.ResourceRequirements{Requests: list(corev1.ResourceCPU, "0")}, cpuTarget)
		g.Expect(errs).To(gomega.HaveLen(1))
		g.Expect(errs[0].Type).To(gomega.Equal(field.ErrorTypeInvalid))
		g.Expect(errs[0].Field).To(gomega.Equal("spec.deployment.resources.requests.cpu"))
		g.Expect(errs[0].Detail).To(gomega.ContainSubstring("cpu request must be greater than zero while targetCPUUtilization is set"))
	})

	t.Run("negative cpu request is rejected like zero", func(t *testing.T) {
		g := gomega.NewWithT(t)
		errs := AutoscalingTargetRequests(path, &corev1.ResourceRequirements{Requests: list(corev1.ResourceCPU, "-1")}, cpuTarget)
		g.Expect(errs).To(gomega.HaveLen(1))
		g.Expect(errs[0].Field).To(gomega.Equal("spec.deployment.resources.requests.cpu"))
	})

	t.Run("zero memory limit without a request", func(t *testing.T) {
		g := gomega.NewWithT(t)
		errs := AutoscalingTargetRequests(path, &corev1.ResourceRequirements{Limits: list(corev1.ResourceMemory, "0")}, memTarget)
		g.Expect(errs).To(gomega.HaveLen(1))
		g.Expect(errs[0].Field).To(gomega.Equal("spec.deployment.resources.limits.memory"))
		g.Expect(errs[0].Detail).To(gomega.ContainSubstring(
			"memory limit must be greater than zero while targetMemoryUtilization is set: " +
				"without a request the API server copies the limit into the request"))
	})

	// The API server copies a limit into a missing request only, so an explicit
	// zero request stays zero beside any limit: the request decides alone.
	t.Run("zero cpu request beside a positive limit", func(t *testing.T) {
		g := gomega.NewWithT(t)
		errs := AutoscalingTargetRequests(path, &corev1.ResourceRequirements{
			Requests: list(corev1.ResourceCPU, "0"),
			Limits:   list(corev1.ResourceCPU, "1"),
		}, cpuTarget)
		g.Expect(errs).To(gomega.HaveLen(1))
		g.Expect(errs[0].Field).To(gomega.Equal("spec.deployment.resources.requests.cpu"))
		g.Expect(errs[0].Detail).To(gomega.ContainSubstring("cpu request must be greater than zero while targetCPUUtilization is set"))
	})

	t.Run("zero requests with both targets report cpu first", func(t *testing.T) {
		g := gomega.NewWithT(t)
		errs := AutoscalingTargetRequests(path, &corev1.ResourceRequirements{Requests: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("0"),
			corev1.ResourceCPU:    resource.MustParse("0"),
		}}, bothTargets)
		g.Expect(errs).To(gomega.HaveLen(2))
		g.Expect(errs[0].Field).To(gomega.Equal("spec.deployment.resources.requests.cpu"))
		g.Expect(errs[1].Field).To(gomega.Equal("spec.deployment.resources.requests.memory"))
	})
}

func TestJob(t *testing.T) {
	path := field.NewPath("spec", "jobs")
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).
		WithObjects(&schedulingv1.PriorityClass{ObjectMeta: metav1.ObjectMeta{Name: "low"}}).
		Build()
	unknown := &commonv1.JobSpec{JobBaseSpec: commonv1.JobBaseSpec{PriorityClassName: ptr.To("typo")}}

	t.Run("nil spec", func(t *testing.T) {
		g := gomega.NewWithT(t)
		g.Expect(Job(context.Background(), c, path, nil)).To(gomega.BeEmpty())
	})

	t.Run("known class and empty opt-out", func(t *testing.T) {
		g := gomega.NewWithT(t)
		g.Expect(Job(context.Background(), c, path, &commonv1.JobSpec{JobBaseSpec: commonv1.JobBaseSpec{PriorityClassName: ptr.To("low")}})).To(gomega.BeEmpty())
		g.Expect(Job(context.Background(), c, path, &commonv1.JobSpec{JobBaseSpec: commonv1.JobBaseSpec{PriorityClassName: ptr.To("")}})).To(gomega.BeEmpty())
	})

	t.Run("unknown class", func(t *testing.T) {
		g := gomega.NewWithT(t)
		errs := Job(context.Background(), c, path, unknown)
		g.Expect(errs).To(gomega.HaveLen(1))
		g.Expect(errs[0].Type).To(gomega.Equal(field.ErrorTypeNotFound))
		g.Expect(errs[0].Field).To(gomega.Equal("spec.jobs.priorityClassName"))
	})

	t.Run("nil client skips the lookup", func(t *testing.T) {
		g := gomega.NewWithT(t)
		g.Expect(Job(context.Background(), nil, path, unknown)).To(gomega.BeEmpty())
	})

	t.Run("lookup error", func(t *testing.T) {
		g := gomega.NewWithT(t)
		failing := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).
			WithInterceptorFuncs(interceptor.Funcs{
				Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
					return errors.New("apiserver unavailable")
				},
			}).Build()
		errs := Job(context.Background(), failing, path, unknown)
		g.Expect(errs).To(gomega.HaveLen(1))
		g.Expect(errs[0].Type).To(gomega.Equal(field.ErrorTypeInternal))
	})

	t.Run("resources and placement", func(t *testing.T) {
		g := gomega.NewWithT(t)
		errs := Job(context.Background(), c, path, &commonv1.JobSpec{
			JobBaseSpec: commonv1.JobBaseSpec{Resources: &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
				Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
			}},
			NodePlacementSpec: commonv1.NodePlacementSpec{NodeSelector: map[string]string{"bad key": "x"}},
		})
		g.Expect(errs).To(gomega.HaveLen(2))
		g.Expect(errs[0].Field).To(gomega.Equal("spec.jobs.resources.requests.memory"))
		g.Expect(errs[1].Field).To(gomega.Equal("spec.jobs.nodeSelector"))
	})
}

func TestJobBase(t *testing.T) {
	g := gomega.NewWithT(t)
	path := field.NewPath("spec", "jobs")
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build()

	g.Expect(JobBase(context.Background(), c, path, nil)).To(gomega.BeEmpty())

	errs := JobBase(context.Background(), c, path, &commonv1.JobBaseSpec{
		PriorityClassName: ptr.To("typo"),
		Resources: &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
		},
	})
	g.Expect(errs).To(gomega.HaveLen(2))
	g.Expect(errs[0].Field).To(gomega.Equal("spec.jobs.resources.requests.cpu"))
	g.Expect(errs[1].Type).To(gomega.Equal(field.ErrorTypeNotFound))
}

func TestSecretStoreRef(t *testing.T) {
	path := field.NewPath("spec", "secretStoreRef")
	cases := []struct {
		name     string
		ref      *commonv1.SecretStoreRefSpec
		wantErrs int
		wantSub  string
	}{
		{"nil is allowed", nil, 0, ""},
		{"valid cluster ref", &commonv1.SecretStoreRefSpec{Kind: commonv1.SecretStoreKindCluster, Name: "openbao-cluster-store"}, 0, ""},
		{"valid namespaced ref", &commonv1.SecretStoreRefSpec{Kind: commonv1.SecretStoreKindNamespaced, Name: "openbao-tenant-store"}, 0, ""},
		{"empty kind defaults, still valid", &commonv1.SecretStoreRefSpec{Name: "some-store"}, 0, ""},
		{"empty name is required", &commonv1.SecretStoreRefSpec{Kind: commonv1.SecretStoreKindNamespaced}, 1, "name"},
		{"unknown kind not supported", &commonv1.SecretStoreRefSpec{Kind: commonv1.SecretStoreRefKind("Bogus"), Name: "x"}, 1, "kind"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			errs := SecretStoreRef(path, tc.ref)
			g.Expect(errs).To(gomega.HaveLen(tc.wantErrs))
			if tc.wantSub != "" {
				g.Expect(errs.ToAggregate().Error()).To(gomega.ContainSubstring(tc.wantSub))
			}
		})
	}
}

func TestTargetClusterRef(t *testing.T) {
	path := field.NewPath("spec", "targetClusterRef")
	cases := []struct {
		name     string
		ref      *commonv1.TargetClusterRefSpec
		wantErrs int
		wantSub  string
	}{
		{"nil selects the management cluster", nil, 0, ""},
		{"valid ref", &commonv1.TargetClusterRefSpec{Name: "edge-1"}, 0, ""},
		{"empty name is required", &commonv1.TargetClusterRefSpec{}, 1, "targetClusterRef.name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			errs := TargetClusterRef(path, tc.ref)
			g.Expect(errs).To(gomega.HaveLen(tc.wantErrs))
			if tc.wantSub != "" {
				g.Expect(errs.ToAggregate().Error()).To(gomega.ContainSubstring(tc.wantSub))
			}
		})
	}
}

func TestTargetClusterRefImmutable(t *testing.T) {
	path := field.NewPath("spec", "targetClusterRef")
	cases := []struct {
		name     string
		oldRef   *commonv1.TargetClusterRefSpec
		newRef   *commonv1.TargetClusterRefSpec
		wantErrs int
	}{
		{"both nil is unchanged", nil, nil, 0},
		{"same name is unchanged", &commonv1.TargetClusterRefSpec{Name: "edge-1"}, &commonv1.TargetClusterRefSpec{Name: "edge-1"}, 0},
		{"adding the ref rejected", nil, &commonv1.TargetClusterRefSpec{Name: "edge-1"}, 1},
		{"removing the ref rejected", &commonv1.TargetClusterRefSpec{Name: "edge-1"}, nil, 1},
		{"renaming the ref rejected", &commonv1.TargetClusterRefSpec{Name: "edge-1"}, &commonv1.TargetClusterRefSpec{Name: "edge-2"}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			errs := TargetClusterRefImmutable(path, tc.oldRef, tc.newRef)
			g.Expect(errs).To(gomega.HaveLen(tc.wantErrs))
			if tc.wantErrs > 0 {
				g.Expect(errs.ToAggregate().Error()).To(gomega.ContainSubstring("targetClusterRef is immutable"))
			}
		})
	}
}

// The sibling validators are generic over client.Object, so the tests use a
// ConfigMap as the stand-in CR: annotation "parent" is the parent reference the
// sameParent predicate compares, and annotation "default" set to "true" is the
// default flag isDefault reads.
const (
	parentAnnotation  = "parent"
	defaultAnnotation = "default"
)

var errBoom = errors.New("boom")

func siblingCM(namespace, name, parent string, isDefault bool) *corev1.ConfigMap {
	annotations := map[string]string{parentAnnotation: parent}
	if isDefault {
		annotations[defaultAnnotation] = "true"
	}
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Namespace:   namespace,
		Name:        name,
		Annotations: annotations,
	}}
}

// terminating marks cm as deleting. The fake client refuses an object that
// carries a deletionTimestamp without a finalizer, so cm gets one.
func terminating(cm *corev1.ConfigMap) *corev1.ConfigMap {
	deleted := metav1.NewTime(time.Now())
	cm.DeletionTimestamp = &deleted
	cm.Finalizers = []string{"test"}
	return cm
}

func sameParentAs(self client.Object) func(client.Object) bool {
	return func(other client.Object) bool {
		return other.GetAnnotations()[parentAnnotation] == self.GetAnnotations()[parentAnnotation]
	}
}

// siblingFixture seeds a client with self, the one live same-parent sibling the
// filter keeps, and the three it drops: a Terminating one, one attached to
// another parent, and one in another namespace.
func siblingFixture(self *corev1.ConfigMap, extra ...client.Object) client.Client {
	objs := []client.Object{
		self,
		siblingCM("ops", "live-sibling", "glance", false),
		terminating(siblingCM("ops", "terminating-sibling", "glance", false)),
		siblingCM("ops", "other-parent", "glance-2", false),
		siblingCM("elsewhere", "other-namespace", "glance", false),
	}
	return fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).
		WithObjects(append(objs, extra...)...).Build()
}

// listErrorClient fails every List with errBoom, so the tests can assert that
// the validators hand the error back untouched.
func listErrorClient() client.Client {
	return fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return errBoom
			},
		}).Build()
}

// itemlessList is a client.ObjectList without an Items field, which is what
// apimeta.ExtractList rejects. No real CR list looks like this; it exists to
// reach the extract-error path behind a List that succeeds.
type itemlessList struct {
	metav1.TypeMeta
	metav1.ListMeta
}

func (l *itemlessList) DeepCopyObject() runtime.Object { return &itemlessList{} }

// emptyListClient succeeds every List without touching the list object, so the
// caller sees whatever list type it passed in.
func emptyListClient() client.Client {
	return fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return nil
			},
		}).Build()
}

func objectNames(objs []client.Object) []string {
	names := make([]string, 0, len(objs))
	for _, o := range objs {
		names = append(names, o.GetName())
	}
	return names
}

func TestAttachedSiblings(t *testing.T) {
	ctx := context.Background()
	self := siblingCM("ops", "self", "glance", false)

	t.Run("nil reader skips the lookup", func(t *testing.T) {
		g := gomega.NewWithT(t)
		siblings, err := AttachedSiblings(ctx, nil, self, &corev1.ConfigMapList{}, sameParentAs(self))
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(siblings).To(gomega.BeNil())
	})

	t.Run("keeps only the live same-parent sibling", func(t *testing.T) {
		g := gomega.NewWithT(t)
		siblings, err := AttachedSiblings(ctx, siblingFixture(self), self, &corev1.ConfigMapList{}, sameParentAs(self))
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(objectNames(siblings)).To(gomega.Equal([]string{"live-sibling"}))
	})

	t.Run("list error is returned unwrapped", func(t *testing.T) {
		g := gomega.NewWithT(t)
		siblings, err := AttachedSiblings(ctx, listErrorClient(), self, &corev1.ConfigMapList{}, sameParentAs(self))
		g.Expect(siblings).To(gomega.BeNil())
		g.Expect(err).To(gomega.MatchError("boom"))
		g.Expect(err).To(gomega.Equal(errBoom))
		g.Expect(errors.Is(err, errBoom)).To(gomega.BeTrue())
	})

	t.Run("a typed predicate returns typed siblings", func(t *testing.T) {
		g := gomega.NewWithT(t)
		siblings, err := AttachedSiblings(ctx, siblingFixture(self), self, &corev1.ConfigMapList{},
			func(other *corev1.ConfigMap) bool {
				return other.Annotations[parentAnnotation] == self.Annotations[parentAnnotation]
			})
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(siblings).To(gomega.HaveLen(1))
		g.Expect(siblings[0].Name).To(gomega.Equal("live-sibling"))
	})

	t.Run("extract error is returned unwrapped", func(t *testing.T) {
		g := gomega.NewWithT(t)
		siblings, err := AttachedSiblings(ctx, emptyListClient(), self, &itemlessList{}, sameParentAs(self))
		g.Expect(siblings).To(gomega.BeNil())
		g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("Items field")))
	})
}

func TestExtraOptions(t *testing.T) {
	const patternDetail = "option name must match ^[A-Za-z0-9_]+$ (letters, digits, and underscore)"
	const controlCharsDetail = "value must not contain newline or carriage-return characters"
	denylist := map[string]string{"denied": "spec.owner"}

	t.Run("nil and empty options are accepted", func(t *testing.T) {
		g := gomega.NewWithT(t)
		g.Expect(ExtraOptions(testPath, nil, ExtraOptionsRules{})).To(gomega.BeNil())
		g.Expect(ExtraOptions(testPath, map[string]string{}, ExtraOptionsRules{Denylist: denylist})).To(gomega.BeNil())
	})

	t.Run("every rejection fires once, in sorted key order", func(t *testing.T) {
		g := gomega.NewWithT(t)
		errs := ExtraOptions(testPath, map[string]string{
			"":        "x",
			"bad key": "x",
			"denied":  "x",
			"ok":      "a\nb",
		}, ExtraOptionsRules{Denylist: denylist})
		g.Expect(errs).To(gomega.HaveLen(4))

		// The empty key has no path key to hang off, so it is reported on the
		// extraOptions path itself.
		g.Expect(errs[0].Field).To(gomega.Equal(testPath.String()))
		g.Expect(errs[0].BadValue).To(gomega.Equal(""))
		g.Expect(errs[0].Detail).To(gomega.Equal("option name must not be empty"))

		g.Expect(errs[1].Field).To(gomega.Equal(testPath.Key("bad key").String()))
		g.Expect(errs[1].BadValue).To(gomega.Equal("bad key"))
		g.Expect(errs[1].Detail).To(gomega.Equal(patternDetail))

		g.Expect(errs[2].Field).To(gomega.Equal(testPath.Key("denied").String()))
		g.Expect(errs[2].BadValue).To(gomega.Equal("x"))
		g.Expect(errs[2].Detail).To(gomega.Equal(`option "denied" is owned by spec.owner and must not be set via extraOptions`))

		g.Expect(errs[3].Field).To(gomega.Equal(testPath.Key("ok").String()))
		g.Expect(errs[3].BadValue).To(gomega.Equal("a\nb"))
		g.Expect(errs[3].Detail).To(gomega.Equal(controlCharsDetail))
	})

	t.Run("PerKey runs before the control-character check on the same key", func(t *testing.T) {
		g := gomega.NewWithT(t)
		errs := ExtraOptions(testPath, map[string]string{"ok": "a\nb"}, ExtraOptionsRules{
			PerKey: func(key, value string) *field.Error {
				return field.Invalid(testPath.Key(key), value, "per-key rule")
			},
		})
		g.Expect(errs).To(gomega.HaveLen(2))
		g.Expect(errs[0].Detail).To(gomega.Equal("per-key rule"))
		g.Expect(errs[1].Detail).To(gomega.Equal(controlCharsDetail))
	})

	t.Run("a denylisted key never reaches PerKey", func(t *testing.T) {
		g := gomega.NewWithT(t)
		var seen []string
		errs := ExtraOptions(testPath, map[string]string{"denied": "x"}, ExtraOptionsRules{
			Denylist: denylist,
			PerKey: func(key, _ string) *field.Error {
				seen = append(seen, key)
				return nil
			},
		})
		g.Expect(errs).To(gomega.HaveLen(1))
		g.Expect(seen).To(gomega.BeEmpty())
	})

	// The charset is not per-consumer: every caller gets
	// DefaultExtraOptionKeyPattern, which is the pattern the message names.
	t.Run("the default charset is enforced and is what the message names", func(t *testing.T) {
		g := gomega.NewWithT(t)
		errs := ExtraOptions(testPath, map[string]string{"with-dash": "x"}, ExtraOptionsRules{})
		g.Expect(errs).To(gomega.HaveLen(1))
		g.Expect(errs[0].Detail).To(gomega.Equal(patternDetail))
		g.Expect(patternDetail).To(gomega.ContainSubstring(DefaultExtraOptionKeyPattern.String()))
		g.Expect(DefaultExtraOptionKeyPattern.MatchString("with-dash")).To(gomega.BeFalse())
		g.Expect(DefaultExtraOptionKeyPattern.MatchString("s3_store_host")).To(gomega.BeTrue())
	})
}
