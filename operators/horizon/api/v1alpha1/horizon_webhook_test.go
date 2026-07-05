// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"context"
	"strings"
	"testing"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	commonv1 "github.com/c5c3/forge/internal/common/types"
)

// validHorizon returns a minimal Horizon CR that passes every validation
// rule. Tests mutate single fields to exercise individual rules.
func validHorizon() *Horizon {
	return &Horizon{
		ObjectMeta: metav1.ObjectMeta{Name: "test-horizon", Namespace: "default"},
		Spec: HorizonSpec{
			Deployment: DeploymentSpec{Replicas: 3},
			Image: commonv1.ImageSpec{
				Repository: "ghcr.io/c5c3/horizon",
				Tag:        "2025.2",
			},
			Cache: commonv1.CacheSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: "memcached"},
				Backend:    DefaultCacheBackend,
			},
			KeystoneEndpoint: "http://keystone.openstack.svc.cluster.local:5000/v3",
			SecretKeyRef:     commonv1.SecretRefSpec{Name: "horizon-secret-key", Key: "secret-key"},
		},
	}
}

func TestDefault_ZeroValueObject(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &HorizonWebhook{}
	h := &Horizon{}

	g.Expect(w.Default(context.Background(), h)).To(gomega.Succeed())

	g.Expect(h.Spec.Deployment.Replicas).To(gomega.Equal(commonv1.DefaultReplicas))
	g.Expect(h.Spec.Deployment.Resources).NotTo(gomega.BeNil())
	g.Expect(h.Spec.Cache.Backend).To(gomega.Equal(DefaultCacheBackend))
	g.Expect(h.Spec.SecretKeyRef.Key).To(gomega.Equal(DefaultSecretKeyKey))
	g.Expect(h.Spec.Logging).NotTo(gomega.BeNil())
	g.Expect(h.Spec.Logging.Format).To(gomega.Equal("text"))
	g.Expect(h.Spec.Logging.Level).To(gomega.Equal("INFO"))
	g.Expect(h.Spec.Logging.Debug).To(gomega.HaveValue(gomega.BeFalse()))
}

func TestDefault_PreservesExplicitValues(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &HorizonWebhook{}
	h := validHorizon()
	h.Spec.Deployment.Replicas = 5
	h.Spec.Cache.Backend = "django.core.cache.backends.locmem.LocMemCache"
	h.Spec.SecretKeyRef.Key = "custom-key"

	g.Expect(w.Default(context.Background(), h)).To(gomega.Succeed())

	g.Expect(h.Spec.Deployment.Replicas).To(gomega.Equal(int32(5)))
	g.Expect(h.Spec.Cache.Backend).To(gomega.Equal("django.core.cache.backends.locmem.LocMemCache"))
	g.Expect(h.Spec.SecretKeyRef.Key).To(gomega.Equal("custom-key"))
}

// TestDefault_ThenValidate_ZeroValueObject exercises the Default →
// ValidateCreate sequence on a bare &Horizon{} — simulating a caller outside
// the admission pipeline (envtest without the defaulter, CLI preflight) — and
// asserts the remaining rejections are the genuinely required fields, not
// confusing downstream parser errors.
func TestDefault_ThenValidate_ZeroValueObject(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &HorizonWebhook{}
	h := &Horizon{}

	g.Expect(w.Default(context.Background(), h)).To(gomega.Succeed())
	_, err := w.ValidateCreate(context.Background(), h)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("image"))
	g.Expect(err.Error()).To(gomega.ContainSubstring("keystoneEndpoint"))
	g.Expect(err.Error()).To(gomega.ContainSubstring("secretKeyRef"))
	g.Expect(err.Error()).To(gomega.ContainSubstring("cache"))
}

func TestValidateCreate_ValidSpecAccepted(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &HorizonWebhook{}

	_, err := w.ValidateCreate(context.Background(), validHorizon())
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

// TestValidateCreate_ValidExtraConfigKeyAccepted guards against the
// identifier check over-rejecting: a well-formed Django setting name must
// still pass admission.
func TestValidateCreate_ValidExtraConfigKeyAccepted(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &HorizonWebhook{}
	h := validHorizon()
	h.Spec.ExtraConfig = map[string]apiextensionsv1.JSON{
		"SESSION_TIMEOUT": {Raw: []byte(`3600`)},
	}

	_, err := w.ValidateCreate(context.Background(), h)
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

func TestValidateCreate_RejectionTable(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(h *Horizon)
		wantSub string
	}{
		{
			name:    "zero replicas rejected",
			mutate:  func(h *Horizon) { h.Spec.Deployment.Replicas = 0 },
			wantSub: "replicas must be at least 1",
		},
		{
			name: "image tag and digest both set rejected",
			mutate: func(h *Horizon) {
				h.Spec.Image.Digest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			},
			wantSub: "exactly one of image.tag or image.digest",
		},
		{
			name:    "image tag and digest both empty rejected",
			mutate:  func(h *Horizon) { h.Spec.Image.Tag = "" },
			wantSub: "exactly one of image.tag or image.digest",
		},
		{
			name: "cache clusterRef and servers both set rejected",
			mutate: func(h *Horizon) {
				h.Spec.Cache.Servers = []string{"memcached-0:11211"}
			},
			wantSub: "cache",
		},
		{
			name:    "empty keystoneEndpoint rejected",
			mutate:  func(h *Horizon) { h.Spec.KeystoneEndpoint = "" },
			wantSub: "keystoneEndpoint must be set",
		},
		{
			name:    "non-http keystoneEndpoint rejected",
			mutate:  func(h *Horizon) { h.Spec.KeystoneEndpoint = "ldap://keystone:5000/v3" },
			wantSub: "scheme must be http or https",
		},
		{
			name:    "keystoneEndpoint without host rejected",
			mutate:  func(h *Horizon) { h.Spec.KeystoneEndpoint = "http://" },
			wantSub: "must include a host",
		},
		{
			name:    "empty secretKeyRef name rejected",
			mutate:  func(h *Horizon) { h.Spec.SecretKeyRef.Name = "" },
			wantSub: "secretKeyRef.name must be set",
		},
		{
			name: "SECRET_KEY in extraConfig rejected",
			mutate: func(h *Horizon) {
				h.Spec.ExtraConfig = map[string]apiextensionsv1.JSON{
					"SECRET_KEY": {Raw: []byte(`"leak"`)},
				}
			},
			wantSub: "SECRET_KEY is managed via spec.secretKeyRef",
		},
		{
			name: "extraConfig key with newline rejected (code injection)",
			mutate: func(h *Horizon) {
				h.Spec.ExtraConfig = map[string]apiextensionsv1.JSON{
					"FOO\nimport os": {Raw: []byte(`"x"`)},
				}
			},
			wantSub: "valid Python identifier",
		},
		{
			name: "extraConfig key with space rejected",
			mutate: func(h *Horizon) {
				h.Spec.ExtraConfig = map[string]apiextensionsv1.JSON{
					"bad name": {Raw: []byte(`"x"`)},
				}
			},
			wantSub: "valid Python identifier",
		},
		{
			name: "SECRET_KEY with trailing space rejected (guard evasion)",
			mutate: func(h *Horizon) {
				h.Spec.ExtraConfig = map[string]apiextensionsv1.JSON{
					"SECRET_KEY ": {Raw: []byte(`"leak"`)},
				}
			},
			wantSub: "valid Python identifier",
		},
		{
			name: "gateway without hostname rejected",
			mutate: func(h *Horizon) {
				h.Spec.Gateway = &GatewaySpec{ParentRef: GatewayParentRefSpec{Name: "openstack-gw"}}
			},
			wantSub: "hostname must be set",
		},
		{
			name: "gateway without parentRef name rejected",
			mutate: func(h *Horizon) {
				h.Spec.Gateway = &GatewaySpec{Hostname: "horizon.example.com"}
			},
			wantSub: "parentRef.name must be set",
		},
		{
			name: "networkPolicy with empty ingress rejected",
			mutate: func(h *Horizon) {
				h.Spec.NetworkPolicy = &NetworkPolicySpec{}
			},
			wantSub: "at least one ingress source",
		},
		{
			name: "autoscaling minReplicas above maxReplicas rejected",
			mutate: func(h *Horizon) {
				h.Spec.Autoscaling = &AutoscalingSpec{
					MinReplicas:          ptr.To(int32(5)),
					MaxReplicas:          2,
					TargetCPUUtilization: ptr.To(int32(80)),
				}
			},
			wantSub: "minReplicas must not exceed maxReplicas",
		},
		{
			name: "autoscaling implicit minReplicas above maxReplicas rejected",
			mutate: func(h *Horizon) {
				h.Spec.Autoscaling = &AutoscalingSpec{
					MaxReplicas:          2,
					TargetCPUUtilization: ptr.To(int32(80)),
				}
			},
			wantSub: "maxReplicas must be >= spec.deployment.replicas",
		},
		{
			name: "autoscaling without any target rejected",
			mutate: func(h *Horizon) {
				h.Spec.Autoscaling = &AutoscalingSpec{MaxReplicas: 5}
			},
			wantSub: "at least one of targetCPUUtilization or targetMemoryUtilization",
		},
		{
			name: "invalid logging format rejected",
			mutate: func(h *Horizon) {
				h.Spec.Logging = &LoggingSpec{Format: "xml"}
			},
			wantSub: "logging.format",
		},
		{
			name: "invalid per-logger level rejected",
			mutate: func(h *Horizon) {
				h.Spec.Logging = &LoggingSpec{PerLoggerLevels: map[string]string{"django": "TRACE"}}
			},
			wantSub: "level must be one of",
		},
		{
			name: "preStopSleep at grace period rejected",
			mutate: func(h *Horizon) {
				h.Spec.Deployment.TerminationGracePeriodSeconds = ptr.To(int64(30))
				h.Spec.Deployment.PreStopSleepSeconds = ptr.To(int64(30))
			},
			wantSub: "strictly less than terminationGracePeriodSeconds",
		},
		{
			name: "requests above limits rejected",
			mutate: func(h *Horizon) {
				h.Spec.Deployment.Resources = &corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
					Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
				}
			},
			wantSub: "request must not exceed limit",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			w := &HorizonWebhook{}
			h := validHorizon()
			tc.mutate(h)

			_, err := w.ValidateCreate(context.Background(), h)
			g.Expect(err).To(gomega.HaveOccurred())
			g.Expect(err.Error()).To(gomega.ContainSubstring(tc.wantSub))
		})
	}
}

// TestValidateCreate_RunsAllValidations breaks every independently breakable
// rule at once and asserts error accumulation (no short-circuiting): each
// field name must surface in the single aggregated admission error.
func TestValidateCreate_RunsAllValidations(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &HorizonWebhook{}
	h := validHorizon()
	h.Spec.Deployment.Replicas = 0
	h.Spec.Image.Tag = ""
	h.Spec.Cache.Servers = []string{"memcached-0:11211"}
	h.Spec.KeystoneEndpoint = "ftp://keystone"
	h.Spec.SecretKeyRef.Name = ""
	h.Spec.ExtraConfig = map[string]apiextensionsv1.JSON{
		"SECRET_KEY": {Raw: []byte(`"x"`)},
		"bad name":   {Raw: []byte(`"y"`)},
	}
	h.Spec.Gateway = &GatewaySpec{}
	h.Spec.NetworkPolicy = &NetworkPolicySpec{}
	h.Spec.Autoscaling = &AutoscalingSpec{MaxReplicas: 0}
	h.Spec.Logging = &LoggingSpec{Format: "xml", PerLoggerLevels: map[string]string{"django": "TRACE"}}

	_, err := w.ValidateCreate(context.Background(), h)
	g.Expect(err).To(gomega.HaveOccurred())
	for _, sub := range []string{
		"replicas must be at least 1",
		"exactly one of image.tag or image.digest",
		"cache",
		"scheme must be http or https",
		"secretKeyRef.name must be set",
		"SECRET_KEY is managed via spec.secretKeyRef",
		"valid Python identifier",
		"hostname must be set",
		"parentRef.name must be set",
		"at least one ingress source",
		"maxReplicas must be at least 1",
		"logging.format",
		"level must be one of",
	} {
		g.Expect(err.Error()).To(gomega.ContainSubstring(sub),
			"aggregated error must contain %q", sub)
	}
}

func TestValidateUpdate_ValidatesNewObject(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &HorizonWebhook{}
	oldObj := validHorizon()
	newObj := validHorizon()
	newObj.Spec.KeystoneEndpoint = "not-a-url"

	_, err := w.ValidateUpdate(context.Background(), oldObj, newObj)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("keystoneEndpoint"))
}

func TestValidateDelete_AlwaysAccepts(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &HorizonWebhook{}
	_, err := w.ValidateDelete(context.Background(), validHorizon())
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

func TestValidateKeystoneEndpoint_HTTPSAccepted(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &HorizonWebhook{}
	h := validHorizon()
	h.Spec.KeystoneEndpoint = "https://keystone.127-0-0-1.nip.io/v3"

	_, err := w.ValidateCreate(context.Background(), h)
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

// TestErrorMessagesStartLowercase spot-checks that webhook messages follow
// the Go error-string convention used across the aggregated admission error.
func TestErrorMessagesStartLowercase(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &HorizonWebhook{}
	h := validHorizon()
	h.Spec.KeystoneEndpoint = ""

	_, err := w.ValidateCreate(context.Background(), h)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(strings.Contains(err.Error(), "keystoneEndpoint must be set")).To(gomega.BeTrue())
}
