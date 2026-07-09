// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"context"
	"fmt"
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
		// Collides with the typed spec.websso / spec.multiDomain blocks set
		// below, so both extraConfig-collision guards participate.
		SettingWebSSOChoices:             {Raw: []byte(`[]`)},
		SettingMultiDomainDomainDropdown: {Raw: []byte(`true`)},
	}
	h.Spec.Gateway = &GatewaySpec{}
	h.Spec.NetworkPolicy = &NetworkPolicySpec{}
	h.Spec.Autoscaling = &AutoscalingSpec{MaxReplicas: 0}
	h.Spec.Logging = &LoggingSpec{Format: "xml", PerLoggerLevels: map[string]string{"django": "TRACE"}}
	// Break websso on every hook: a duplicate choice id, an initialChoice and
	// an idpMapping key that name no declared choice, and a non-URL
	// keystoneURL. Every new websso validation hook must participate in the
	// aggregated error so a future short-circuit is caught here.
	h.Spec.WebSSO = &WebSSOSpec{
		Enabled: true,
		Choices: []WebSSOChoice{
			{ID: "keycloak_openid", Label: "Keycloak"},
			{ID: "keycloak_openid", Label: "Keycloak again"},
		},
		IDPMapping:    map[string]WebSSOIDPTarget{"unknown_choice": {IdentityProvider: "kc", Protocol: "openid"}},
		InitialChoice: "not-a-choice",
		KeystoneURL:   "ftp://keystone.example.com/v3",
	}
	// Break multiDomain: dropdown enabled without multi-domain support, and a
	// duplicate domain name.
	h.Spec.MultiDomain = &MultiDomainSpec{
		Enabled:        false,
		DomainDropdown: true,
		DomainChoices: []DomainChoice{
			{Name: "planetexpress", Label: "Planet Express"},
			{Name: "planetexpress", Label: "Planet Express again"},
		},
	}

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
		// Every websso and multiDomain validation path must participate.
		"websso.choices[1].id",
		"websso.initialChoice",
		"websso.idpMapping[unknown_choice]",
		"websso.keystoneURL",
		"multiDomain.domainDropdown",
		"multiDomain.domainChoices[1].name",
		"WEBSSO_CHOICES is managed via spec.websso",
		"OPENSTACK_KEYSTONE_DOMAIN_DROPDOWN is managed via spec.multiDomain",
	} {
		g.Expect(err.Error()).To(gomega.ContainSubstring(sub),
			"aggregated error must contain %q", sub)
	}
}

// TestDefault_WebSSOPrependsCredentialsChoice pins the credentials fallback:
// enabling SSO must never lock out local accounts, so the defaulter prepends
// the credentials choice and preselects it.
func TestDefault_WebSSOPrependsCredentialsChoice(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &HorizonWebhook{}
	h := validHorizon()
	h.Spec.WebSSO = &WebSSOSpec{
		Enabled: true,
		Choices: []WebSSOChoice{{ID: "keycloak_openid", Label: "Keycloak"}},
	}

	g.Expect(w.Default(context.Background(), h)).To(gomega.Succeed())
	g.Expect(h.Spec.WebSSO.Choices).To(gomega.Equal([]WebSSOChoice{
		{ID: DefaultWebSSOCredentialsChoiceID, Label: DefaultWebSSOCredentialsChoiceLabel},
		{ID: "keycloak_openid", Label: "Keycloak"},
	}))
	g.Expect(h.Spec.WebSSO.InitialChoice).To(gomega.Equal(DefaultWebSSOCredentialsChoiceID))
}

// TestDefault_WebSSOKeepsExplicitCredentialsChoiceAndInitialChoice guards
// idempotency: a second admission pass must not duplicate the fallback or
// overwrite an operator's explicit initialChoice.
func TestDefault_WebSSOKeepsExplicitCredentialsChoiceAndInitialChoice(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &HorizonWebhook{}
	h := validHorizon()
	h.Spec.WebSSO = &WebSSOSpec{
		Enabled: true,
		Choices: []WebSSOChoice{
			{ID: "keycloak_openid", Label: "Keycloak"},
			{ID: DefaultWebSSOCredentialsChoiceID, Label: "Local"},
		},
		InitialChoice: "keycloak_openid",
	}

	g.Expect(w.Default(context.Background(), h)).To(gomega.Succeed())
	g.Expect(h.Spec.WebSSO.Choices).To(gomega.HaveLen(2))
	g.Expect(h.Spec.WebSSO.InitialChoice).To(gomega.Equal("keycloak_openid"))
}

// TestDefault_WebSSODisabledIsUntouched ensures a disabled (or absent) block
// stays inert, so the renderer keeps emitting nothing at all.
func TestDefault_WebSSODisabledIsUntouched(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &HorizonWebhook{}

	h := validHorizon()
	h.Spec.WebSSO = &WebSSOSpec{Enabled: false}
	g.Expect(w.Default(context.Background(), h)).To(gomega.Succeed())
	g.Expect(h.Spec.WebSSO.Choices).To(gomega.BeEmpty())
	g.Expect(h.Spec.WebSSO.InitialChoice).To(gomega.BeEmpty())

	h = validHorizon()
	g.Expect(w.Default(context.Background(), h)).To(gomega.Succeed())
	g.Expect(h.Spec.WebSSO).To(gomega.BeNil())
	g.Expect(h.Spec.MultiDomain).To(gomega.BeNil())
}

// TestDefault_MultiDomainDefaultsDefaultDomain covers the enabled path; a
// disabled block must keep an empty defaultDomain.
func TestDefault_MultiDomainDefaultsDefaultDomain(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &HorizonWebhook{}

	h := validHorizon()
	h.Spec.MultiDomain = &MultiDomainSpec{Enabled: true}
	g.Expect(w.Default(context.Background(), h)).To(gomega.Succeed())
	g.Expect(h.Spec.MultiDomain.DefaultDomain).To(gomega.Equal(DefaultMultiDomainDefaultDomain))

	h = validHorizon()
	h.Spec.MultiDomain = &MultiDomainSpec{Enabled: false}
	g.Expect(w.Default(context.Background(), h)).To(gomega.Succeed())
	g.Expect(h.Spec.MultiDomain.DefaultDomain).To(gomega.BeEmpty())
}

// TestValidate_WebSSOEnabledRequiresChoices covers the webhook-bypassed CR
// (envtest, direct etcd write) where the defaulter never ran.
func TestValidate_WebSSOEnabledRequiresChoices(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &HorizonWebhook{}
	h := validHorizon()
	h.Spec.WebSSO = &WebSSOSpec{Enabled: true}

	_, err := w.ValidateCreate(context.Background(), h)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("at least one choice must be declared"))
}

// TestDefault_ThenValidate_WebSSOEnabledRequiresChoices reproduces the real
// admission order: mutating admission runs first, then the CEL rules and the
// validating webhook see whatever it wrote. A defaulter that prepended the
// credentials fallback onto an empty list would hand both gates a one-element
// list and the requirement would never fire.
func TestDefault_ThenValidate_WebSSOEnabledRequiresChoices(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &HorizonWebhook{}
	h := validHorizon()
	h.Spec.WebSSO = &WebSSOSpec{Enabled: true}

	g.Expect(w.Default(context.Background(), h)).To(gomega.Succeed())
	g.Expect(h.Spec.WebSSO.Choices).To(gomega.BeEmpty(),
		"an invalid CR must not be defaulted into a valid one")

	_, err := w.ValidateCreate(context.Background(), h)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("at least one choice must be declared"))
}

// TestDefault_WebSSORejectsChoicesOverflowingThePrepend pins the MaxItems
// arithmetic: mutating admission runs before schema validation, so a list the
// prepend would grow past the bound must be rejected here — naming the count
// the operator submitted — rather than by the API server naming count+1.
func TestDefault_WebSSORejectsChoicesOverflowingThePrepend(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &HorizonWebhook{}

	choices := make([]WebSSOChoice, 0, maxWebSSOChoices)
	for i := 0; i < maxWebSSOChoices; i++ {
		choices = append(choices, WebSSOChoice{
			ID:    fmt.Sprintf("idp-%d", i),
			Label: fmt.Sprintf("IdP %d", i),
		})
	}

	h := validHorizon()
	h.Spec.WebSSO = &WebSSOSpec{Enabled: true, Choices: choices}
	err := w.Default(context.Background(), h)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("spec.websso.choices"))
	g.Expect(err.Error()).To(gomega.ContainSubstring(fmt.Sprintf("must have at most %d entries", maxWebSSOChoices-1)))
	g.Expect(h.Spec.WebSSO.Choices).To(gomega.HaveLen(maxWebSSOChoices), "the rejected list must not be mutated")

	// The same count WITH the credentials fallback already declared needs no
	// prepend, so it stays within the bound and is accepted.
	h = validHorizon()
	h.Spec.WebSSO = &WebSSOSpec{
		Enabled: true,
		Choices: append([]WebSSOChoice{{ID: DefaultWebSSOCredentialsChoiceID, Label: DefaultWebSSOCredentialsChoiceLabel}},
			choices[1:]...),
	}
	g.Expect(w.Default(context.Background(), h)).To(gomega.Succeed())
	g.Expect(h.Spec.WebSSO.Choices).To(gomega.HaveLen(maxWebSSOChoices))

	// One below the bound still leaves room for the prepend.
	h = validHorizon()
	h.Spec.WebSSO = &WebSSOSpec{Enabled: true, Choices: choices[1:]}
	g.Expect(w.Default(context.Background(), h)).To(gomega.Succeed())
	g.Expect(h.Spec.WebSSO.Choices).To(gomega.HaveLen(maxWebSSOChoices))
	g.Expect(h.Spec.WebSSO.Choices[0].ID).To(gomega.Equal(DefaultWebSSOCredentialsChoiceID))
}

// TestValidate_MultiDomainDropdownRequiresChoices covers the dropdown that
// would render an empty select.
func TestValidate_MultiDomainDropdownRequiresChoices(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &HorizonWebhook{}
	h := validHorizon()
	h.Spec.MultiDomain = &MultiDomainSpec{Enabled: true, DomainDropdown: true}

	_, err := w.ValidateCreate(context.Background(), h)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("at least one domain choice must be declared"))
}

// TestValidate_WebSSOAcceptsFullyMappedBlock pins the happy path so the
// defense-in-depth checks cannot drift into rejecting a valid projection —
// this is exactly the shape the ControlPlane operator projects.
func TestValidate_WebSSOAcceptsFullyMappedBlock(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &HorizonWebhook{}
	h := validHorizon()
	h.Spec.WebSSO = &WebSSOSpec{
		Enabled: true,
		Choices: []WebSSOChoice{
			{ID: DefaultWebSSOCredentialsChoiceID, Label: DefaultWebSSOCredentialsChoiceLabel},
			{ID: "keycloak_openid", Label: "keycloak"},
		},
		IDPMapping:    map[string]WebSSOIDPTarget{"keycloak_openid": {IdentityProvider: "keycloak", Protocol: "openid"}},
		InitialChoice: DefaultWebSSOCredentialsChoiceID,
		KeystoneURL:   "https://keystone.127-0-0-1.nip.io/v3",
	}
	h.Spec.MultiDomain = &MultiDomainSpec{
		Enabled:        true,
		DefaultDomain:  DefaultMultiDomainDefaultDomain,
		DomainDropdown: true,
		DomainChoices: []DomainChoice{
			{Name: "Default", Label: "Default"},
			{Name: "planetexpress", Label: "planetexpress"},
		},
	}

	_, err := w.ValidateCreate(context.Background(), h)
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

// TestValidate_ExtraConfigWebSSOAloneIsAllowed pins the escape hatch: the
// collision guard must only fire when the typed block is ALSO set, so a CR
// that predates the typed field keeps validating.
func TestValidate_ExtraConfigWebSSOAloneIsAllowed(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &HorizonWebhook{}
	h := validHorizon()
	h.Spec.ExtraConfig = map[string]apiextensionsv1.JSON{
		SettingWebSSOEnabled:      {Raw: []byte(`true`)},
		SettingMultiDomainSupport: {Raw: []byte(`true`)},
	}

	_, err := w.ValidateCreate(context.Background(), h)
	g.Expect(err).NotTo(gomega.HaveOccurred())
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
