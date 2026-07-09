// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"sort"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/config"
	horizonv1alpha1 "github.com/c5c3/forge/operators/horizon/api/v1alpha1"
)

func TestReconcileConfig_RendersLocalSettings(t *testing.T) {
	g := NewGomegaWithT(t)
	h := testHorizon()
	r := newTestReconciler(testScheme(), h)

	name, err := r.reconcileConfig(context.Background(), h)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(name).To(HavePrefix("test-horizon-config-"))

	var cm corev1.ConfigMap
	g.Expect(r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, &cm)).To(Succeed())
	rendered := cm.Data["local_settings.py"]
	g.Expect(rendered).NotTo(BeEmpty())

	// SECRET_KEY comes from the environment and never enters the ConfigMap.
	g.Expect(rendered).To(ContainSubstring(`SECRET_KEY = os.environ["HORIZON_SECRET_KEY"]`))
	g.Expect(rendered).NotTo(ContainSubstring("super-secret"))

	// Design decision D1: signed-cookie sessions + Memcached-backed cache.
	g.Expect(rendered).To(ContainSubstring(`SESSION_ENGINE = "django.contrib.sessions.backends.signed_cookies"`))
	g.Expect(rendered).To(ContainSubstring(`"BACKEND": "django.core.cache.backends.memcached.PyMemcacheCache"`))
	g.Expect(rendered).To(ContainSubstring(`"LOCATION": ["memcached:11211"]`))

	g.Expect(rendered).To(ContainSubstring(`OPENSTACK_KEYSTONE_URL = "http://keystone.default.svc.cluster.local:5000/v3"`))
	// The server-side clients must use the internal catalog interface — the
	// public entries may only resolve outside the cluster. The legacy "...URL"
	// spelling is required by openstack_dashboard's ENDPOINT_TYPE_TO_INTERFACE
	// map; a bare "internal" fails every catalog lookup.
	g.Expect(rendered).To(ContainSubstring(`OPENSTACK_ENDPOINT_TYPE = "internalURL"`))
	g.Expect(rendered).To(ContainSubstring("COMPRESS_OFFLINE = True"))
	g.Expect(rendered).To(ContainSubstring(`STATIC_ROOT = "/var/lib/openstack/horizon-static"`))
	g.Expect(rendered).To(ContainSubstring("DEBUG = False"))

	// ALLOWED_HOSTS is a closed allow-list — the fixed probe Host header and
	// every standard Kubernetes DNS form of the dashboard Service — never
	// the "*" wildcard that would disable Django's Host-header validation.
	g.Expect(rendered).To(ContainSubstring(`ALLOWED_HOSTS = ["localhost", "test-horizon", "test-horizon.default", "test-horizon.default.svc", "test-horizon.default.svc.cluster.local"]`))
	g.Expect(rendered).NotTo(ContainSubstring(`ALLOWED_HOSTS = ["*"]`))

	cond := conditions.GetCondition(h.Status.Conditions, conditionTypeConfigReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonConfigRendered))
}

func TestReconcileConfig_ExtraConfigOverridesDefaults(t *testing.T) {
	g := NewGomegaWithT(t)
	h := testHorizon()
	h.Spec.ExtraConfig = map[string]apiextensionsv1.JSON{
		"ALLOWED_HOSTS":   {Raw: []byte(`["horizon.example.com"]`)},
		"SESSION_TIMEOUT": {Raw: []byte(`1800`)},
	}
	r := newTestReconciler(testScheme(), h)

	name, err := r.reconcileConfig(context.Background(), h)
	g.Expect(err).NotTo(HaveOccurred())

	var cm corev1.ConfigMap
	g.Expect(r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, &cm)).To(Succeed())
	rendered := cm.Data["local_settings.py"]

	// User value wins over the operator default.
	g.Expect(rendered).To(ContainSubstring(`ALLOWED_HOSTS = ["horizon.example.com"]`))
	g.Expect(rendered).NotTo(ContainSubstring(`ALLOWED_HOSTS = ["*"]`))
	// New settings are appended.
	g.Expect(rendered).To(ContainSubstring("SESSION_TIMEOUT = 1800"))
}

func TestReconcileConfig_ExtraConfigSecretKeyNeverEntersConfigMap(t *testing.T) {
	g := NewGomegaWithT(t)
	h := testHorizon()
	// A webhook bypass (direct etcd write, disabled admission, an old stored
	// object) could smuggle a SECRET_KEY into extraConfig. The renderer must
	// fail closed and drop it so the session-signing key never enters the
	// namespace-readable ConfigMap and cannot override the env-sourced lookup.
	h.Spec.ExtraConfig = map[string]apiextensionsv1.JSON{
		"SECRET_KEY":      {Raw: []byte(`"attacker-known"`)},
		"SESSION_TIMEOUT": {Raw: []byte(`1800`)},
	}
	r := newTestReconciler(testScheme(), h)

	name, err := r.reconcileConfig(context.Background(), h)
	g.Expect(err).NotTo(HaveOccurred())

	var cm corev1.ConfigMap
	g.Expect(r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, &cm)).To(Succeed())
	rendered := cm.Data["local_settings.py"]

	// The attacker-controlled key material must not appear anywhere, and no
	// plaintext SECRET_KEY assignment may shadow the env-sourced lookup.
	g.Expect(rendered).NotTo(ContainSubstring("attacker-known"))
	g.Expect(rendered).NotTo(ContainSubstring(`SECRET_KEY = "`))
	// The env-sourced SECRET_KEY line remains the only SECRET_KEY assignment.
	g.Expect(rendered).To(ContainSubstring(`SECRET_KEY = os.environ["HORIZON_SECRET_KEY"]`))
	// Non-SECRET_KEY extraConfig entries still pass through unaffected.
	g.Expect(rendered).To(ContainSubstring("SESSION_TIMEOUT = 1800"))
}

func TestReconcileConfig_AllowedHostsIncludesGatewayHostname(t *testing.T) {
	g := NewGomegaWithT(t)
	h := testHorizon()
	h.Spec.Gateway = gatewaySpec()
	r := newTestReconciler(testScheme(), h)

	name, err := r.reconcileConfig(context.Background(), h)
	g.Expect(err).NotTo(HaveOccurred())

	var cm corev1.ConfigMap
	g.Expect(r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, &cm)).To(Succeed())
	rendered := cm.Data["local_settings.py"]

	// The Gateway forwards requests with spec.gateway.hostname as the Host
	// header, so it joins the closed ALLOWED_HOSTS allow-list alongside the
	// probe Host and the Service DNS names.
	g.Expect(rendered).To(ContainSubstring(`ALLOWED_HOSTS = ["localhost", "test-horizon", "test-horizon.default", "test-horizon.default.svc", "test-horizon.default.svc.cluster.local", "horizon.127-0-0-1.nip.io"]`))
}

func TestReconcileConfig_InvalidExtraConfigJSONFails(t *testing.T) {
	g := NewGomegaWithT(t)
	h := testHorizon()
	h.Spec.ExtraConfig = map[string]apiextensionsv1.JSON{
		"BROKEN": {Raw: []byte(`{not json`)},
	}
	r := newTestReconciler(testScheme(), h)

	_, err := r.reconcileConfig(context.Background(), h)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("rendering local_settings.py"))
}

func TestReconcileConfig_PerLoggerLevelsReachLoggingDict(t *testing.T) {
	g := NewGomegaWithT(t)
	h := testHorizon()
	h.Spec.Logging = &horizonv1alpha1.LoggingSpec{
		Level:           "WARNING",
		PerLoggerLevels: map[string]string{"django.request": "ERROR"},
	}
	r := newTestReconciler(testScheme(), h)

	name, err := r.reconcileConfig(context.Background(), h)
	g.Expect(err).NotTo(HaveOccurred())

	var cm corev1.ConfigMap
	g.Expect(r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, &cm)).To(Succeed())
	rendered := cm.Data["local_settings.py"]
	g.Expect(rendered).To(ContainSubstring(`"level": "WARNING"`))
	g.Expect(rendered).To(ContainSubstring(`"django.request": {"level": "ERROR"}`))
}

func TestPruneStaleConfigMaps_RetainsNewestAndCurrent(t *testing.T) {
	g := NewGomegaWithT(t)
	h := testHorizon()
	r := newTestReconciler(testScheme(), h)
	ctx := context.Background()

	// Produce 6 distinct ConfigMap generations by varying extraConfig.
	var names []string
	for i := 0; i < 6; i++ {
		h.Spec.ExtraConfig = map[string]apiextensionsv1.JSON{
			"SESSION_TIMEOUT": {Raw: []byte(fmt.Sprintf(`%d`, 1000+i))},
		}
		name, err := r.reconcileConfig(ctx, h)
		g.Expect(err).NotTo(HaveOccurred())
		names = append(names, name)
	}

	current := names[len(names)-1]
	g.Expect(r.pruneStaleConfigMaps(ctx, h, current)).To(Succeed())

	var cms corev1.ConfigMapList
	g.Expect(r.List(ctx, &cms, client.InNamespace("default"),
		client.MatchingLabels{config.ConfigBaseLabelKey: "test-horizon-config"})).To(Succeed())

	remaining := make([]string, 0, len(cms.Items))
	for _, cm := range cms.Items {
		remaining = append(remaining, cm.Name)
	}
	// Retain = 3 historical + the current one.
	g.Expect(remaining).To(HaveLen(defaultConfigMapRetainCount + 1))
	g.Expect(remaining).To(ContainElement(current), "the active ConfigMap must survive pruning")

	// Identity of the pruned pair: with the fake client every ConfigMap
	// shares one CreationTimestamp second, so the prune helper's documented
	// tie-break (name descending) decides retention. Compute the expected
	// survivors the same way and assert the exact pruned set.
	historical := append([]string(nil), names[:len(names)-1]...)
	sort.Sort(sort.Reverse(sort.StringSlice(historical)))
	expectedRetained := historical[:defaultConfigMapRetainCount]
	expectedPruned := historical[defaultConfigMapRetainCount:]
	g.Expect(remaining).To(ContainElements(expectedRetained[0], expectedRetained[1], expectedRetained[2]))
	for _, pruned := range expectedPruned {
		g.Expect(remaining).NotTo(ContainElement(pruned), "ConfigMap %s must be pruned", pruned)
	}
}

// webssoTestHorizon returns a Horizon with a fully-populated websso block in
// the shape the ControlPlane operator projects: the credentials fallback
// first, one federated choice, and a browser-facing keystoneURL distinct from
// the cluster-local spec.keystoneEndpoint.
func webssoTestHorizon() *horizonv1alpha1.Horizon {
	h := testHorizon()
	h.Spec.WebSSO = &horizonv1alpha1.WebSSOSpec{
		Enabled: true,
		Choices: []horizonv1alpha1.WebSSOChoice{
			{ID: horizonv1alpha1.DefaultWebSSOCredentialsChoiceID, Label: horizonv1alpha1.DefaultWebSSOCredentialsChoiceLabel},
			{ID: "keycloak_openid", Label: "keycloak"},
		},
		IDPMapping: map[string]horizonv1alpha1.WebSSOIDPTarget{
			"keycloak_openid": {IdentityProvider: "keycloak", Protocol: "openid"},
		},
		InitialChoice: horizonv1alpha1.DefaultWebSSOCredentialsChoiceID,
		KeystoneURL:   "https://keystone.127-0-0-1.nip.io/v3",
	}
	return h
}

// TestRenderLocalSettings_NoWebSSOOmitsAllWebSSOSettings pins the
// byte-identical render for the CR that never opts in. It asserts the specific
// setting names are absent rather than that the rendered module is empty, so
// the test survives unrelated settings being added.
func TestRenderLocalSettings_NoWebSSOOmitsAllWebSSOSettings(t *testing.T) {
	g := NewGomegaWithT(t)
	rendered, err := renderLocalSettings(testHorizon())
	g.Expect(err).NotTo(HaveOccurred())

	for _, name := range horizonv1alpha1.WebSSOSettingNames {
		g.Expect(rendered).NotTo(ContainSubstring(name), "%s must not be rendered without spec.websso", name)
	}
	for _, name := range horizonv1alpha1.MultiDomainSettingNames {
		g.Expect(rendered).NotTo(ContainSubstring(name), "%s must not be rendered without spec.multiDomain", name)
	}
}

// TestRenderLocalSettings_DisabledWebSSOOmitsAllWebSSOSettings covers the
// prepared-but-off block: enabled=false must render nothing even though
// choices are populated.
func TestRenderLocalSettings_DisabledWebSSOOmitsAllWebSSOSettings(t *testing.T) {
	g := NewGomegaWithT(t)
	h := webssoTestHorizon()
	h.Spec.WebSSO.Enabled = false
	h.Spec.MultiDomain = &horizonv1alpha1.MultiDomainSpec{Enabled: false, DomainDropdown: true}

	rendered, err := renderLocalSettings(h)
	g.Expect(err).NotTo(HaveOccurred())
	for _, name := range horizonv1alpha1.WebSSOSettingNames {
		g.Expect(rendered).NotTo(ContainSubstring(name))
	}
	for _, name := range horizonv1alpha1.MultiDomainSettingNames {
		g.Expect(rendered).NotTo(ContainSubstring(name))
	}
}

// TestRenderLocalSettings_WebSSORendersChoicesMappingAndInitialChoice asserts
// the exact Python literals openstack_auth consumes, including the preserved
// choice order (credentials first).
func TestRenderLocalSettings_WebSSORendersChoicesMappingAndInitialChoice(t *testing.T) {
	g := NewGomegaWithT(t)
	rendered, err := renderLocalSettings(webssoTestHorizon())
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(rendered).To(ContainSubstring(`WEBSSO_ENABLED = True`))
	g.Expect(rendered).To(ContainSubstring(
		`WEBSSO_CHOICES = [["credentials", "Keystone Credentials"], ["keycloak_openid", "keycloak"]]`,
	))
	g.Expect(rendered).To(ContainSubstring(
		`WEBSSO_IDP_MAPPING = {"keycloak_openid": ["keycloak", "openid"]}`,
	))
	g.Expect(rendered).To(ContainSubstring(`WEBSSO_INITIAL_CHOICE = "credentials"`))
	g.Expect(rendered).To(ContainSubstring(`WEBSSO_KEYSTONE_URL = "https://keystone.127-0-0-1.nip.io/v3"`))
}

// TestRenderLocalSettings_WebSSOForcesUseHTTPRefererFalse guards the setting
// that makes the round trip work at all: upstream defaults it to True, which
// would have openstack_auth validate the returned token against the external
// gateway URL from inside the pod.
func TestRenderLocalSettings_WebSSOForcesUseHTTPRefererFalse(t *testing.T) {
	g := NewGomegaWithT(t)
	rendered, err := renderLocalSettings(webssoTestHorizon())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(rendered).To(ContainSubstring(`WEBSSO_USE_HTTP_REFERER = False`))
}

// TestRenderLocalSettings_WebSSOOmitsEmptyOptionalSettings covers the
// minimal enabled block: no mapping, no initialChoice, no keystoneURL. An
// empty WEBSSO_IDP_MAPPING dict or an empty WEBSSO_KEYSTONE_URL string would
// each override an upstream default with a meaningless value.
func TestRenderLocalSettings_WebSSOOmitsEmptyOptionalSettings(t *testing.T) {
	g := NewGomegaWithT(t)
	h := testHorizon()
	h.Spec.WebSSO = &horizonv1alpha1.WebSSOSpec{
		Enabled: true,
		Choices: []horizonv1alpha1.WebSSOChoice{
			{ID: horizonv1alpha1.DefaultWebSSOCredentialsChoiceID, Label: horizonv1alpha1.DefaultWebSSOCredentialsChoiceLabel},
		},
	}

	rendered, err := renderLocalSettings(h)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(rendered).To(ContainSubstring(`WEBSSO_ENABLED = True`))
	g.Expect(rendered).NotTo(ContainSubstring(horizonv1alpha1.SettingWebSSOIDPMapping))
	g.Expect(rendered).NotTo(ContainSubstring(horizonv1alpha1.SettingWebSSOInitialChoice))
	g.Expect(rendered).NotTo(ContainSubstring(horizonv1alpha1.SettingWebSSOKeystoneURL))
}

// TestRenderLocalSettings_MultiDomainRendersDropdownAndChoices covers the
// LDAP-domain login path the domain dropdown exists for.
func TestRenderLocalSettings_MultiDomainRendersDropdownAndChoices(t *testing.T) {
	g := NewGomegaWithT(t)
	h := testHorizon()
	h.Spec.MultiDomain = &horizonv1alpha1.MultiDomainSpec{
		Enabled:        true,
		DefaultDomain:  horizonv1alpha1.DefaultMultiDomainDefaultDomain,
		DomainDropdown: true,
		DomainChoices: []horizonv1alpha1.DomainChoice{
			{Name: "Default", Label: "Default"},
			{Name: "planetexpress", Label: "planetexpress"},
		},
	}

	rendered, err := renderLocalSettings(h)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(rendered).To(ContainSubstring(`OPENSTACK_KEYSTONE_MULTIDOMAIN_SUPPORT = True`))
	g.Expect(rendered).To(ContainSubstring(`OPENSTACK_KEYSTONE_DEFAULT_DOMAIN = "Default"`))
	g.Expect(rendered).To(ContainSubstring(`OPENSTACK_KEYSTONE_DOMAIN_DROPDOWN = True`))
	g.Expect(rendered).To(ContainSubstring(
		`OPENSTACK_KEYSTONE_DOMAIN_CHOICES = [["Default", "Default"], ["planetexpress", "planetexpress"]]`,
	))
}

// TestRenderLocalSettings_MultiDomainDropdownOffOmitsChoices asserts the
// double gate: Horizon only consults DOMAIN_CHOICES when DOMAIN_DROPDOWN is
// True, so both are omitted together.
func TestRenderLocalSettings_MultiDomainDropdownOffOmitsChoices(t *testing.T) {
	g := NewGomegaWithT(t)
	h := testHorizon()
	h.Spec.MultiDomain = &horizonv1alpha1.MultiDomainSpec{
		Enabled:       true,
		DomainChoices: []horizonv1alpha1.DomainChoice{{Name: "planetexpress", Label: "planetexpress"}},
	}

	rendered, err := renderLocalSettings(h)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(rendered).To(ContainSubstring(`OPENSTACK_KEYSTONE_MULTIDOMAIN_SUPPORT = True`))
	// defaultDomain falls back to "Default" for a CR that bypassed the webhook.
	g.Expect(rendered).To(ContainSubstring(`OPENSTACK_KEYSTONE_DEFAULT_DOMAIN = "Default"`))
	g.Expect(rendered).NotTo(ContainSubstring(horizonv1alpha1.SettingMultiDomainDomainDropdown))
	g.Expect(rendered).NotTo(ContainSubstring(horizonv1alpha1.SettingMultiDomainDomainChoices))
}

// TestRenderLocalSettings_ExtraConfigOverridesWebSSOChoices pins the
// render-time escape hatch. The validating webhook rejects this combination at
// admission, but the renderer must keep extraConfig winning for a CR that
// bypassed it — the same "user values win" contract every other setting has.
func TestRenderLocalSettings_ExtraConfigOverridesWebSSOChoices(t *testing.T) {
	g := NewGomegaWithT(t)
	h := webssoTestHorizon()
	h.Spec.ExtraConfig = map[string]apiextensionsv1.JSON{
		horizonv1alpha1.SettingWebSSOChoices: {Raw: []byte(`[["custom", "Custom"]]`)},
	}

	rendered, err := renderLocalSettings(h)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(rendered).To(ContainSubstring(`WEBSSO_CHOICES = [["custom", "Custom"]]`))
	g.Expect(rendered).NotTo(ContainSubstring(`["keycloak_openid", "keycloak"]`))
}

// TestRenderLocalSettings_GatewaySetsSecureProxySSLHeader guards the setting
// WebSSO depends on: behind a TLS-terminating Gateway the pod sees plain HTTP,
// so without this Django would announce an "http://" origin that Keystone's
// verbatim trusted_dashboard match rejects.
func TestRenderLocalSettings_GatewaySetsSecureProxySSLHeader(t *testing.T) {
	g := NewGomegaWithT(t)
	h := testHorizon()
	h.Spec.Gateway = &horizonv1alpha1.GatewaySpec{
		Hostname:  "horizon.127-0-0-1.nip.io",
		ParentRef: horizonv1alpha1.GatewayParentRefSpec{Name: "openstack-gw"},
	}

	rendered, err := renderLocalSettings(h)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(rendered).To(ContainSubstring(`SECURE_PROXY_SSL_HEADER = ["HTTP_X_FORWARDED_PROTO", "https"]`))
}

// TestRenderLocalSettings_NoGatewayOmitsSecureProxySSLHeader is the security
// half: with no Gateway nothing is guaranteed to overwrite X-Forwarded-Proto,
// so a direct client could forge an https:// scheme.
func TestRenderLocalSettings_NoGatewayOmitsSecureProxySSLHeader(t *testing.T) {
	g := NewGomegaWithT(t)
	rendered, err := renderLocalSettings(testHorizon())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(rendered).NotTo(ContainSubstring(horizonv1alpha1.SettingSecureProxySSLHeader))
}
