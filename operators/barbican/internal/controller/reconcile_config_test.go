// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/config"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	barbicanv1alpha1 "github.com/c5c3/cobaltcore/operators/barbican/api/v1alpha1"
)

// validProjection returns the secret-store projection a single ready managed
// store produces, so the config tests can render without driving the store step.
func validProjection() secretStoreProjection {
	return secretStoreProjection{
		valid: true,
		sections: map[string]map[string]string{
			"secretstore": {
				"enable_multiple_secret_stores": "true",
				"stores_lookup_suffix":          "primary",
			},
			"secretstore:primary": {
				"secret_store_plugin": "vault_plugin",
				"global_default":      "True",
			},
			"vault_plugin": {
				"vault_url":       instanceURL(testInstanceName, testNamespace),
				"use_ssl":         "true",
				"approle_role_id": "role-id-value",
				"kv_mountpoint":   "barbican",
			},
		},
		credentialsSecretName: "primary" + approleSecretNameSuffix,
		defaultStore:          "primary",
	}
}

// renderConfig runs the config step over the shared Barbican fixture and returns
// the rendered Secret.
func renderConfig(t *testing.T, barbican *barbicanv1alpha1.Barbican, projection secretStoreProjection, objs ...client.Object) (*BarbicanReconciler, *corev1.Secret) {
	t.Helper()
	g := NewGomegaWithT(t)
	r := newBarbicanTestReconciler(append([]client.Object{barbican}, objs...)...)

	res, name, err := r.reconcileConfig(context.Background(), r.Client, barbican, projection)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(name).NotTo(BeEmpty())

	var secret corev1.Secret
	g.Expect(r.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: name}, &secret)).To(Succeed())
	return r, &secret
}

// sectionOf returns the lines of one INI section of the rendered document.
func sectionOf(t *testing.T, rendered, section string) []string {
	t.Helper()
	var (
		lines []string
		in    bool
	)
	for _, line := range strings.Split(rendered, "\n") {
		switch {
		case line == "["+section+"]":
			in = true
		case strings.HasPrefix(line, "["):
			in = false
		case in && line != "":
			lines = append(lines, line)
		}
	}
	if lines == nil {
		t.Fatalf("section [%s] not found in:\n%s", section, rendered)
	}
	return lines
}

func TestReconcileConfig_RendersOneSecretWithBothFiles(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := testBarbican()
	_, secret := renderConfig(t, barbican, validProjection())

	// One immutable, content-hashed Secret carries both startup files: barbican
	// reads exactly one configuration file, and it holds the vault plugin's
	// approle_role_id, so the whole document is credential-bearing.
	g.Expect(secret.Name).To(HavePrefix(barbican.Name + "-config-"))
	g.Expect(secret.Immutable).NotTo(BeNil())
	g.Expect(*secret.Immutable).To(BeTrue())
	g.Expect(secret.Labels).To(HaveKeyWithValue(config.ConfigBaseLabelKey, barbican.Name+"-config"))
	g.Expect(secret.Data).To(HaveLen(2))
	g.Expect(secret.Data).To(HaveKey(barbicanConfDataKey))
	g.Expect(secret.Data).To(HaveKey(barbicanPasteDataKey))
}

func TestReconcileConfig_OperatorOwnedOptions(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := testBarbican()
	_, secret := renderConfig(t, barbican, validProjection())
	rendered := string(secret.Data[barbicanConfDataKey])

	// The schema has exactly one writer: the db-sync Job, never the API pods.
	g.Expect(sectionOf(t, rendered, "DEFAULT")).To(ContainElement("db_auto_create = false"))
	// The links in API responses address the published endpoint.
	g.Expect(sectionOf(t, rendered, "DEFAULT")).To(ContainElement(
		fmt.Sprintf("host_href = http://%s.%s.svc.cluster.local:%d", barbican.Name, testNamespace, barbicanAPIPort),
	))
	// No worker Deployment exists, so asynchronous order processing stays off.
	g.Expect(sectionOf(t, rendered, "queue")).To(Equal([]string{"enable = false"}))
	// The DSN arrives through OS_DATABASE__CONNECTION; the file carries only the
	// parseable placeholder and the oslo.db tuning.
	g.Expect(sectionOf(t, rendered, "database")).To(ConsistOf(
		"connection = "+dbConnectionPlaceholder,
		"connection_recycle_time = 600",
		"max_retries = -1",
	))
	// The middleware password is env-injected, never rendered.
	g.Expect(sectionOf(t, rendered, "keystone_authtoken")).NotTo(ContainElement(HavePrefix("password =")))
	g.Expect(sectionOf(t, rendered, "keystone_authtoken")).To(ContainElement("auth_url = " + barbican.Spec.KeystoneEndpoint))
	g.Expect(sectionOf(t, rendered, "keystone_authtoken")).To(ContainElement("memcached_servers = mc:11211"))
}

// TestReconcileConfig_GatewayHostHrefHasNoTrailingSlash pins the endpoint shape
// barbican concatenates its self-referencing links onto.
func TestReconcileConfig_GatewayHostHrefHasNoTrailingSlash(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := testBarbican()
	barbican.Spec.Gateway = &commonv1.GatewaySpec{Hostname: "barbican.example.com"}
	_, secret := renderConfig(t, barbican, validProjection())

	g.Expect(sectionOf(t, string(secret.Data[barbicanConfDataKey]), "DEFAULT")).
		To(ContainElement("host_href = https://barbican.example.com"))
}

func TestReconcileConfig_SecretStoreSectionsFollowTheProjection(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := testBarbican()
	_, secret := renderConfig(t, barbican, validProjection())
	rendered := string(secret.Data[barbicanConfDataKey])

	g.Expect(sectionOf(t, rendered, "secretstore")).To(ConsistOf(
		"enable_multiple_secret_stores = true", "stores_lookup_suffix = primary",
	))
	g.Expect(sectionOf(t, rendered, "secretstore:primary")).To(ConsistOf(
		"global_default = True", "secret_store_plugin = vault_plugin",
	))
	g.Expect(sectionOf(t, rendered, "vault_plugin")).To(ContainElement("approle_role_id = role-id-value"))
	g.Expect(rendered).NotTo(ContainSubstring("approle_secret_id"))
}

func TestReconcileConfig_PolicyOverridesAreInjected(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := testBarbican()
	barbican.Spec.PolicyOverrides = &commonv1.PolicySpec{
		Rules: map[string]string{"secrets:get": "rule:admin_or_creator"},
	}
	_, secret := renderConfig(t, barbican, validProjection())

	g.Expect(sectionOf(t, string(secret.Data[barbicanConfDataKey]), "oslo_policy")).
		To(ConsistOf("policy_file = " + policyFilePath))
	g.Expect(secret.Data).To(HaveKey(policyFileDataKey))
	g.Expect(string(secret.Data[policyFileDataKey])).To(ContainSubstring("secrets:get"))
}

// TestReconcileConfig_ExtraConfigOverlay covers both halves of the escape hatch:
// a free option is merged, and an override of an operator-owned key is honored
// but surfaced through ExtraConfigHealthy so the deviation is visible in status.
func TestReconcileConfig_ExtraConfigOverlay(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := testBarbican()
	barbican.Spec.ExtraConfig = map[string]map[string]string{
		"DEFAULT":   {"db_auto_create": "true"},
		"crypto":    {"enabled_crypto_plugins": "simple_crypto"},
		"kombu_ssl": {"version": "TLSv1_2"},
	}
	r, secret := renderConfig(t, barbican, validProjection())
	rendered := string(secret.Data[barbicanConfDataKey])

	g.Expect(sectionOf(t, rendered, "DEFAULT")).To(ContainElement("db_auto_create = true"), "the user override wins")
	g.Expect(sectionOf(t, rendered, "crypto")).To(ConsistOf("enabled_crypto_plugins = simple_crypto"))
	g.Expect(sectionOf(t, rendered, "kombu_ssl")).To(ConsistOf("version = TLSv1_2"))

	cond := barbicanCondition(barbican, config.ConditionTypeExtraConfigHealthy)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(config.ConditionReasonOwnedKeysOverridden))
	g.Expect(cond.Message).To(ContainSubstring("[DEFAULT] db_auto_create"))
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).To(
		ContainElement(ContainSubstring(config.EventReasonExtraConfigOwnedKeyOverride)),
	)
}

func TestReconcileConfig_PluginSectionsAreMerged(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := testBarbican()
	barbican.Spec.Plugins = []commonv1.PluginSpec{{
		Name:          "simple-crypto",
		ConfigSection: "simple_crypto_plugin",
		Config:        map[string]string{"plugin_name": "Software Only Crypto"},
	}}
	_, secret := renderConfig(t, barbican, validProjection())

	g.Expect(sectionOf(t, string(secret.Data[barbicanConfDataKey]), "simple_crypto_plugin")).
		To(ConsistOf("plugin_name = Software Only Crypto"))
}

// TestReconcileConfig_DuplicatePluginSectionFails covers the rendering failure
// path: the step must flip SecretsReady=False rather than leaving the aggregate
// Ready stale-True at the new generation.
func TestReconcileConfig_DuplicatePluginSectionFails(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := testBarbican()
	barbican.Spec.Plugins = []commonv1.PluginSpec{
		{Name: "first", ConfigSection: "crypto", Config: map[string]string{"a": "1"}},
		{Name: "second", ConfigSection: "crypto", Config: map[string]string{"b": "2"}},
	}
	r := newBarbicanTestReconciler(barbican)

	_, name, err := r.reconcileConfig(context.Background(), r.Client, barbican, validProjection())

	g.Expect(err).To(MatchError(ContainSubstring("rendering plugin config")))
	g.Expect(name).To(BeEmpty())
	cond := barbicanCondition(barbican, "SecretsReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonConfigError))
}

// TestReconcileConfig_InvalidProjectionKeepsLastGood is the retention contract:
// with no renderable secret-store projection the step returns the Secret the
// running Deployment mounts and writes nothing, so a store that went unready
// cannot roll the pods onto a config without a secret store.
func TestReconcileConfig_InvalidProjectionKeepsLastGood(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := testBarbican()
	lastGood := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: barbican.Name + "-config-deadbeef", Namespace: testNamespace,
			Labels: map[string]string{config.ConfigBaseLabelKey: barbican.Name + "-config"},
		},
		Data: map[string][]byte{barbicanConfDataKey: []byte("[DEFAULT]\n")},
	}
	r := newBarbicanTestReconciler(barbican, lastGood,
		testProjectedDeployment(lastGood.Name))

	res, name, err := r.reconcileConfig(context.Background(), r.Client, barbican, secretStoreProjection{})

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(name).To(Equal(lastGood.Name))

	var secrets corev1.SecretList
	g.Expect(r.List(context.Background(), &secrets, client.InNamespace(testNamespace),
		client.MatchingLabels{config.ConfigBaseLabelKey: barbican.Name + "-config"})).To(Succeed())
	g.Expect(secrets.Items).To(HaveLen(1), "nothing is re-rendered against an invalid projection")

	// The ownership guard still runs on the retention path.
	g.Expect(barbicanCondition(barbican, config.ConditionTypeExtraConfigHealthy)).NotTo(BeNil())
}

// TestReconcileConfig_InvalidProjectionOnFirstInstall covers the same path
// before any Deployment exists: there is no last-good to keep, and the empty
// name is what makes the database and deployment steps wait.
func TestReconcileConfig_InvalidProjectionOnFirstInstall(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := testBarbican()
	r := newBarbicanTestReconciler(barbican)

	res, name, err := r.reconcileConfig(context.Background(), r.Client, barbican, secretStoreProjection{})

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(name).To(BeEmpty())
}

// TestReconcileConfig_PrunesToRetainCount pins the rollback depth: three
// historical Secrets survive beside the current one.
func TestReconcileConfig_PrunesToRetainCount(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := testBarbican()

	stale := make([]client.Object, 0, 5)
	for i := range 5 {
		stale = append(stale, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("%s-config-stale%d", barbican.Name, i),
				Namespace: testNamespace,
				Labels:    map[string]string{config.ConfigBaseLabelKey: barbican.Name + "-config"},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: barbicanv1alpha1.GroupVersion.String(),
					Kind:       "Barbican",
					Name:       barbican.Name,
					UID:        barbican.UID,
					Controller: func() *bool { b := true; return &b }(),
				}},
			},
		})
	}
	r, current := renderConfig(t, barbican, validProjection(), stale...)

	var secrets corev1.SecretList
	g.Expect(r.List(context.Background(), &secrets, client.InNamespace(testNamespace),
		client.MatchingLabels{config.ConfigBaseLabelKey: barbican.Name + "-config"})).To(Succeed())

	names := make([]string, 0, len(secrets.Items))
	for i := range secrets.Items {
		names = append(names, secrets.Items[i].Name)
	}
	g.Expect(names).To(HaveLen(defaultConfigRetainCount+1),
		"the current Secret plus %d historical ones", defaultConfigRetainCount)
	g.Expect(names).To(ContainElement(current.Name))
}

// TestRenderPasteINI_ReleaseVariants grounds both bases in the file the release's
// image ships: 2025.2 carries the repoze.profile pipeline and filter, 2026.1
// carries the oslo request_id filter in every pipeline and neither profile
// section. Both route /healthcheck to the oslo healthcheck app.
func TestRenderPasteINI_ReleaseVariants(t *testing.T) {
	tests := []struct {
		name    string
		release string
		profile bool
	}{
		{name: "2025.2 ships repoze.profile", release: "2025.2", profile: true},
		{name: "2026.1 ships request_id", release: "2026.1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			barbican := testBarbican()
			barbican.Spec.OpenStackRelease = tc.release

			rendered, err := renderPasteINI(barbican)
			g.Expect(err).NotTo(HaveOccurred())

			// The healthcheck app sits above the pipeline in both, so the probes stay
			// outside authtoken.
			g.Expect(sectionOf(t, rendered, "composite:main")).To(ContainElement("/healthcheck = healthcheck"))
			g.Expect(sectionOf(t, rendered, "app:healthcheck")).To(
				ContainElement("paste.app_factory = oslo_middleware:Healthcheck.app_factory"),
			)
			g.Expect(sectionOf(t, rendered, "composite:main")).To(ContainElement("/v1 = " + keystonePipelineName))

			pipelines := pipelineDirectives(rendered)
			g.Expect(pipelines).NotTo(BeEmpty())

			if tc.profile {
				g.Expect(rendered).To(ContainSubstring("[pipeline:barbican-profile]"))
				g.Expect(sectionOf(t, rendered, "filter:profile")).To(ContainElement("use = egg:repoze.profile"))
				g.Expect(rendered).NotTo(ContainSubstring("request_id"))
				return
			}

			g.Expect(rendered).NotTo(ContainSubstring("repoze.profile"))
			g.Expect(rendered).NotTo(ContainSubstring("[pipeline:barbican-profile]"))
			g.Expect(sectionOf(t, rendered, "filter:request_id")).To(
				ContainElement("paste.filter_factory = oslo_middleware.request_id:RequestId.factory"),
			)
			for section, directive := range pipelines {
				g.Expect(directive).To(ContainSubstring("request_id"), "pipeline %s", section)
			}
		})
	}
}

// TestRenderPasteINI_MiddlewareExtendsTheKeystonePipeline pins which of the
// shipped pipelines spec.middleware reaches: the one composite:main routes /v1
// to, since that is the pipeline that serves the API.
func TestRenderPasteINI_MiddlewareExtendsTheKeystonePipeline(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := testBarbican()
	barbican.Spec.Middleware = []commonv1.MiddlewareSpec{{
		Name:          "audit",
		FilterFactory: "keystonemiddleware.audit:filter_factory",
		Position:      commonv1.PipelinePositionAfter,
	}}

	rendered, err := renderPasteINI(barbican)
	g.Expect(err).NotTo(HaveOccurred())

	directive := pipelineDirectives(rendered)["pipeline:"+keystonePipelineName]
	g.Expect(directive).To(Equal("cors request_id http_proxy_to_wsgi authtoken context microversion audit apiapp"))
}

// TestRenderPasteINI_DuplicateMiddlewareIsRejected covers the renderer's own
// guard reaching the config step: a duplicate filter name would silently
// overwrite a section, so it fails the render instead.
func TestRenderPasteINI_DuplicateMiddlewareIsRejected(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := testBarbican()
	barbican.Spec.Middleware = []commonv1.MiddlewareSpec{
		{Name: "extra", FilterFactory: "a:factory", Position: commonv1.PipelinePositionAfter},
		{Name: "extra", FilterFactory: "b:factory", Position: commonv1.PipelinePositionAfter},
	}
	r := newBarbicanTestReconciler(barbican)

	_, name, err := r.reconcileConfig(context.Background(), r.Client, barbican, validProjection())

	g.Expect(err).To(MatchError(ContainSubstring("rendering barbican-api-paste.ini")))
	g.Expect(name).To(BeEmpty())
	g.Expect(barbicanCondition(barbican, "SecretsReady").Reason).To(Equal(conditionReasonConfigError))
}

// pipelineDirectives maps each [pipeline:*] section name to its pipeline value.
func pipelineDirectives(rendered string) map[string]string {
	directives := map[string]string{}
	current := ""
	for _, line := range strings.Split(rendered, "\n") {
		if strings.HasPrefix(line, "[pipeline:") && strings.HasSuffix(line, "]") {
			current = strings.Trim(line, "[]")
			continue
		}
		if strings.HasPrefix(line, "[") {
			current = ""
			continue
		}
		if current != "" && strings.HasPrefix(line, "pipeline = ") {
			directives[current] = strings.TrimPrefix(line, "pipeline = ")
		}
	}
	return directives
}
