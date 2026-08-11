// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/forge/internal/common/config"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	glancev1alpha1 "github.com/c5c3/forge/operators/glance/api/v1alpha1"
)

// validProjection is a rendered projection over a single default store.
func validProjection() backendsProjection {
	return backendsProjection{
		valid:           true,
		enabledBackends: "store:s3",
		defaultBackend:  "store",
		secretName:      "test-glance-backends-abcd1234",
		hosts:           []string{"https://s3.example.com"},
	}
}

// glanceForConfig returns a Glance with the webhook-defaulted service-user
// identity, a region, a worker count, and an injected middleware so the config
// render exercises every branch.
func glanceForConfig() *glancev1alpha1.Glance {
	glance := testGlance()
	glance.Spec.ServiceUser.Username = "glance"
	glance.Spec.ServiceUser.ProjectName = "service"
	glance.Spec.ServiceUser.UserDomainName = "Default"
	glance.Spec.ServiceUser.ProjectDomainName = "Default"
	glance.Spec.Region = "RegionOne"
	glance.Spec.APIServer = &glancev1alpha1.APIServerSpec{Workers: ptr.To(int32(4))}
	glance.Spec.Middleware = []commonv1.MiddlewareSpec{{
		Name:          "audit",
		FilterFactory: "audit_middleware:filter_factory",
		Position:      commonv1.PipelinePositionAfter,
	}}
	return glance
}

// renderedConfig returns the glance-api.conf produced by reconcileConfig for the
// given artefacts.
func renderedConfig(t *testing.T, r *GlanceReconciler, art configArtifacts) string {
	t.Helper()
	var cm corev1.ConfigMap
	key := client.ObjectKey{Namespace: "default", Name: art.configMapName}
	if err := r.Get(context.Background(), key, &cm); err != nil {
		t.Fatalf("re-reading config ConfigMap %s: %v", art.configMapName, err)
	}
	return cm.Data["glance-api.conf"]
}

func renderedPaste(t *testing.T, r *GlanceReconciler, art configArtifacts) string {
	t.Helper()
	var cm corev1.ConfigMap
	key := client.ObjectKey{Namespace: "default", Name: art.configMapName}
	if err := r.Get(context.Background(), key, &cm); err != nil {
		t.Fatalf("re-reading config ConfigMap %s: %v", art.configMapName, err)
	}
	return cm.Data["glance-api-paste.ini"]
}

func TestReconcileConfig_RendersGlanceAPIConf(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := glanceForConfig()
	r := newGlanceTestReconciler(glance)

	res, art, err := r.reconcileConfig(context.Background(), r.Client, glance, validProjection())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(art.configMapName).NotTo(BeEmpty())
	g.Expect(art.backendsSecretName).To(Equal("test-glance-backends-abcd1234"))

	conf := renderedConfig(t, r, art)
	// [DEFAULT].
	g.Expect(conf).To(ContainSubstring("enabled_backends = store:s3"))
	g.Expect(conf).To(ContainSubstring("workers = 4"))
	g.Expect(conf).To(ContainSubstring("enabled_import_methods = [web-download,copy-image]"))
	// [keystone_authtoken]: identity + region + memcached, www_authenticate_uri
	// falls back to auth_url when no public endpoint is set, and NO password.
	g.Expect(conf).To(ContainSubstring("auth_url = http://keystone.default.svc:5000"))
	g.Expect(conf).To(ContainSubstring("www_authenticate_uri = http://keystone.default.svc:5000"))
	g.Expect(conf).To(ContainSubstring("username = glance"))
	g.Expect(conf).To(ContainSubstring("project_name = service"))
	g.Expect(conf).To(ContainSubstring("region_name = RegionOne"))
	g.Expect(conf).To(ContainSubstring("memcached_servers = mc:11211"))
	g.Expect(conf).NotTo(ContainSubstring("password ="))
	// Reserved stores are always rendered.
	g.Expect(conf).To(ContainSubstring("[os_glance_staging_store]"))
	g.Expect(conf).To(ContainSubstring("filesystem_store_datadir = /var/lib/glance/staging"))
	g.Expect(conf).To(ContainSubstring("[os_glance_tasks_store]"))
	g.Expect(conf).To(ContainSubstring("filesystem_store_datadir = /var/lib/glance/tasks-work"))
	// [glance_store] default and [paste_deploy].
	g.Expect(conf).To(ContainSubstring("default_backend = store"))
	g.Expect(conf).To(ContainSubstring("[paste_deploy]"))
	g.Expect(conf).To(ContainSubstring("flavor = keystone"))
	// [import_filtering_opts]: spec.importFiltering is nil, so the restrictive
	// operator defaults apply. The three keys that resolve to an empty list are
	// still rendered — glance's own allowed_schemes/allowed_ports defaults are
	// non-empty, so an absent key would mean "glance's permissive default", not
	// "no restriction from this operator". The assertions are anchored on both
	// sides because "allowed_ports" is a substring of "disallowed_ports".
	g.Expect(conf).To(ContainSubstring("[import_filtering_opts]"))
	g.Expect(conf).To(ContainSubstring("\nallowed_schemes = [https]\n"))
	g.Expect(conf).To(ContainSubstring("\nallowed_ports = [443]\n"))
	g.Expect(conf).To(ContainSubstring("\ndisallowed_hosts = [localhost,127.0.0.1,0.0.0.0,::1,169.254.169.254," +
		"kubernetes,kubernetes.default,kubernetes.default.svc,kubernetes.default.svc.cluster.local]\n"))
	// An empty list renders "[]" — every one of the six options is declared
	// bounds=True upstream, so oslo.config parses "[]" as the empty list but
	// rejects a bare empty value outright.
	g.Expect(conf).To(ContainSubstring("\nallowed_hosts = []\n"))
	g.Expect(conf).To(ContainSubstring("\ndisallowed_schemes = []\n"))
	g.Expect(conf).To(ContainSubstring("\ndisallowed_ports = []\n"))
	// No policy overrides configured, so no oslo_policy section.
	g.Expect(conf).NotTo(ContainSubstring("[oslo_policy]"))
}

func TestReconcileConfig_PasteContainsPipelineAndComposite(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := glanceForConfig()
	r := newGlanceTestReconciler(glance)

	_, art, err := r.reconcileConfig(context.Background(), r.Client, glance, validProjection())
	g.Expect(err).NotTo(HaveOccurred())

	paste := renderedPaste(t, r, art)
	// The flavored name glance loads ([paste_deploy] flavor = keystone) must be
	// the root composite that routes /healthcheck to the healthcheck app.
	// oslo.middleware ≥ glance 32.0.0 (2026.1) refuses Healthcheck as a paste
	// filter (NotImplementedError at app load), so it must never appear in the
	// pipeline directive.
	g.Expect(paste).To(ContainSubstring("[composite:glance-api-keystone]"))
	g.Expect(paste).To(ContainSubstring("/healthcheck = healthcheck"))
	g.Expect(paste).To(ContainSubstring("[app:healthcheck]"))
	g.Expect(paste).To(ContainSubstring("paste.app_factory = oslo_middleware:Healthcheck.app_factory"))
	g.Expect(paste).NotTo(ContainSubstring("[filter:healthcheck]"))
	// The filter pipeline the composite mounts at /, with the injected
	// middleware after the base filters.
	g.Expect(paste).To(ContainSubstring("[pipeline:api]"))
	g.Expect(paste).To(MatchRegexp(`pipeline = cors http_proxy_to_wsgi versionnegotiation authtoken context audit rootapp`))
	g.Expect(paste).To(ContainSubstring("[filter:audit]"))
	// The rootapp composite the PipelineSpec cannot express.
	g.Expect(paste).To(ContainSubstring("[composite:rootapp]"))
	g.Expect(paste).To(ContainSubstring("paste.composite_factory = glance.api:root_app_factory"))
	// The versioned app factories must resolve to real paste-deploy targets:
	// glance-api loads them at startup, and a dangling reference crash-loops
	// every API pod ("module 'glance.api.v2.router' has no attribute ...").
	g.Expect(paste).To(ContainSubstring("[app:apiv2app]"))
	g.Expect(paste).To(ContainSubstring("paste.app_factory = glance.api.v2.router:API.factory"))
	g.Expect(paste).To(ContainSubstring("[app:apiversions]"))
	g.Expect(paste).To(ContainSubstring("paste.app_factory = glance.api.versions:create_resource"))
}

func TestReconcileConfig_ExtraConfigWins(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := glanceForConfig()
	glance.Spec.ExtraConfig = map[string]map[string]string{
		"DEFAULT": {"enabled_import_methods": "[copy-image]"},
	}
	r := newGlanceTestReconciler(glance)

	_, art, err := r.reconcileConfig(context.Background(), r.Client, glance, validProjection())
	g.Expect(err).NotTo(HaveOccurred())

	conf := renderedConfig(t, r, art)
	g.Expect(conf).To(ContainSubstring("enabled_import_methods = [copy-image]"),
		"extraConfig overrides the operator default")
	g.Expect(conf).NotTo(ContainSubstring("enabled_import_methods = [web-download,copy-image]"))
}

func TestReconcileConfig_OperatorDefaultsWinOverPlugins(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := glanceForConfig()
	glance.Spec.Plugins = []commonv1.PluginSpec{{
		// A plugin section colliding with an operator-computed routing key must
		// NOT override it: the operator default wins so a plugin cannot silently
		// misroute the image store while BackendsReady still reports healthy.
		Name:          "rogue-store",
		ConfigSection: "glance_store",
		Config:        map[string]string{"default_backend": "rogue"},
	}, {
		// A non-colliding plugin section is still merged in.
		Name:          "extra-section",
		ConfigSection: "myplugin",
		Config:        map[string]string{"foo": "bar"},
	}}
	r := newGlanceTestReconciler(glance)

	_, art, err := r.reconcileConfig(context.Background(), r.Client, glance, validProjection())
	g.Expect(err).NotTo(HaveOccurred())

	conf := renderedConfig(t, r, art)
	g.Expect(conf).To(ContainSubstring("default_backend = store"),
		"operator default wins over a colliding plugin section")
	g.Expect(conf).NotTo(ContainSubstring("default_backend = rogue"))
	// Non-colliding plugin sections still render.
	g.Expect(conf).To(ContainSubstring("[myplugin]"))
	g.Expect(conf).To(ContainSubstring("foo = bar"))
}

func TestReconcileConfig_PolicyOverridesRenderOsloPolicy(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := glanceForConfig()
	glance.Spec.PolicyOverrides = &commonv1.PolicySpec{
		Rules: map[string]string{"get_image": "role:reader"},
	}
	r := newGlanceTestReconciler(glance)

	_, art, err := r.reconcileConfig(context.Background(), r.Client, glance, validProjection())
	g.Expect(err).NotTo(HaveOccurred())

	conf := renderedConfig(t, r, art)
	g.Expect(conf).To(ContainSubstring("[oslo_policy]"))
	g.Expect(conf).To(ContainSubstring("policy_file = " + policyFilePath))

	var cm corev1.ConfigMap
	g.Expect(r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: art.configMapName}, &cm)).To(Succeed())
	g.Expect(cm.Data).To(HaveKey("policy.yaml"))
	g.Expect(cm.Data["policy.yaml"]).To(ContainSubstring("get_image"))
}

func TestReconcileConfig_InvalidProjectionWithDeploymentKeepsLastGood(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := glanceForConfig()
	// The live Deployment mounts the last-good config ConfigMap and backends
	// Secret; an invalid projection must return those without re-rendering.
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: subResourceName(glance), Namespace: glance.Namespace},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Volumes: []corev1.Volume{
						{
							Name:         configVolumeName,
							VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "test-glance-config-live"}}},
						},
						{
							Name:         backendsVolumeName,
							VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "test-glance-backends-live"}},
						},
					},
				},
			},
		},
	}
	r := newGlanceTestReconciler(glance, deploy)

	res, art, err := r.reconcileConfig(context.Background(), r.Client, glance, backendsProjection{valid: false})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(art.configMapName).To(Equal("test-glance-config-live"))
	g.Expect(art.backendsSecretName).To(Equal("test-glance-backends-live"))

	// No new ConfigMap was rendered (the returned name is the live one, which
	// was never created in the fake client).
	var cm corev1.ConfigMap
	err = r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: art.configMapName}, &cm)
	g.Expect(err).To(HaveOccurred(), "last-good retention must not render a fresh ConfigMap")
}

func TestReconcileConfig_InvalidProjectionNoDeploymentReturnsEmpty(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := glanceForConfig()
	r := newGlanceTestReconciler(glance)

	res, art, err := r.reconcileConfig(context.Background(), r.Client, glance, backendsProjection{valid: false})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(art.configMapName).To(BeEmpty())
	g.Expect(art.backendsSecretName).To(BeEmpty())
}

// --- extraConfig ownership guard ---

// TestReconcileConfig_OwnedKeyOverrideReported verifies the report-only
// contract: overriding an operator-owned key in spec.extraConfig sets
// ExtraConfigHealthy=False, emits a Warning event, AND still honours the
// override in the rendered glance-api.conf (the guard reports, it does not
// reject).
func TestReconcileConfig_OwnedKeyOverrideReported(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := glanceForConfig()
	glance.Spec.ExtraConfig = map[string]map[string]string{
		"DEFAULT": {"enabled_backends": "rogue:file"},
	}
	r := newGlanceTestReconciler(glance)

	_, art, err := r.reconcileConfig(context.Background(), r.Client, glance, validProjection())
	g.Expect(err).NotTo(HaveOccurred())

	cond := meta.FindStatusCondition(glance.Status.Conditions, config.ConditionTypeExtraConfigHealthy)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(config.ConditionReasonOwnedKeysOverridden))
	g.Expect(cond.Message).To(ContainSubstring("[DEFAULT] enabled_backends"))

	// A Warning event carries the reason and the overridden-key text.
	events := collectEvents(r.Recorder.(*record.FakeRecorder))
	g.Expect(events).To(ContainElement(And(
		ContainSubstring("Warning"),
		ContainSubstring(config.EventReasonExtraConfigOwnedKeyOverride),
		ContainSubstring("[DEFAULT] enabled_backends"),
	)))

	// Report-only: the override is honoured in the rendered glance-api.conf.
	conf := renderedConfig(t, r, art)
	g.Expect(conf).To(ContainSubstring("enabled_backends = rogue:file"))
}

// TestReconcileConfig_ImportFilteringOverrideReported verifies the controller's
// half of the [import_filtering_opts] ownership contract. The six keys are
// Rejected registry entries, so the validating webhook blocks this extraConfig
// at admission and a CR carrying it can only exist behind a bypassed webhook or
// from before the rule landed. The reconciler has no Rejected special case: it
// renders the override and surfaces it through ExtraConfigHealthy=False, so a CR
// that slipped past admission still reports a loosened filter rather than
// silently running one.
func TestReconcileConfig_ImportFilteringOverrideReported(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := glanceForConfig()
	glance.Spec.ExtraConfig = map[string]map[string]string{
		"import_filtering_opts": {"allowed_schemes": "[http,https]"},
	}
	r := newGlanceTestReconciler(glance)

	_, art, err := r.reconcileConfig(context.Background(), r.Client, glance, validProjection())
	g.Expect(err).NotTo(HaveOccurred())

	cond := meta.FindStatusCondition(glance.Status.Conditions, config.ConditionTypeExtraConfigHealthy)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(config.ConditionReasonOwnedKeysOverridden))
	g.Expect(cond.Message).To(ContainSubstring("[import_filtering_opts] allowed_schemes"))

	events := collectEvents(r.Recorder.(*record.FakeRecorder))
	g.Expect(events).To(ContainElement(And(
		ContainSubstring("Warning"),
		ContainSubstring(config.EventReasonExtraConfigOwnedKeyOverride),
		ContainSubstring("[import_filtering_opts] allowed_schemes"),
	)))

	// Honoured-but-reported: the user's value replaces the operator default and
	// is written through verbatim — brackets and all are the user's problem once
	// the webhook is bypassed — while the sibling keys keep their resolved
	// defaults in the operator's own bracket-bounded form.
	conf := renderedConfig(t, r, art)
	g.Expect(conf).To(ContainSubstring("\nallowed_schemes = [http,https]\n"))
	g.Expect(conf).To(ContainSubstring("\nallowed_ports = [443]\n"))
}

// TestReconcileConfig_ImportFilteringChangeRotatesConfigMap verifies that
// tightening or loosening spec.importFiltering reaches the running pods: the
// config ConfigMap is content-hashed, so a changed filter list must produce a new
// name for the deployment step to roll.
func TestReconcileConfig_ImportFilteringChangeRotatesConfigMap(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := glanceForConfig()
	r := newGlanceTestReconciler(glance)

	_, before, err := r.reconcileConfig(context.Background(), r.Client, glance, validProjection())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(before.configMapName).NotTo(BeEmpty())

	glance.Spec.ImportFiltering = &glancev1alpha1.ImportFilteringSpec{
		AllowedSchemes: []string{"http", "https"},
		AllowedPorts:   []int32{80, 443},
	}
	_, after, err := r.reconcileConfig(context.Background(), r.Client, glance, validProjection())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(after.configMapName).NotTo(Equal(before.configMapName),
		"an importFiltering change must rotate the content-hashed ConfigMap")

	conf := renderedConfig(t, r, after)
	g.Expect(conf).To(ContainSubstring("\nallowed_schemes = [http,https]\n"))
	g.Expect(conf).To(ContainSubstring("\nallowed_ports = [80,443]\n"))
}

// TestReconcileConfig_InvalidProjectionStillUpsertsExtraConfigHealthy verifies
// the guard runs before the invalid-projection short-circuit: an invalid
// projection takes the last-good early return, yet ExtraConfigHealthy is still
// upserted (False, naming the overridden key).
func TestReconcileConfig_InvalidProjectionStillUpsertsExtraConfigHealthy(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := glanceForConfig()
	glance.Spec.ExtraConfig = map[string]map[string]string{
		"DEFAULT": {"enabled_backends": "rogue:file"},
	}
	r := newGlanceTestReconciler(glance)

	res, art, err := r.reconcileConfig(context.Background(), r.Client, glance, backendsProjection{valid: false})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	// Last-good early return: no fresh render on the invalid path.
	g.Expect(art.configMapName).To(BeEmpty())

	cond := meta.FindStatusCondition(glance.Status.Conditions, config.ConditionTypeExtraConfigHealthy)
	g.Expect(cond).NotTo(BeNil(), "guard runs before the invalid-projection short-circuit")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(config.ConditionReasonOwnedKeysOverridden))
	g.Expect(cond.Message).To(ContainSubstring("[DEFAULT] enabled_backends"))
}

// TestReconcileConfig_ExtraConfigHealthyTrueOnDefaults verifies that a CR whose
// spec.extraConfig overrides no operator-owned key sets ExtraConfigHealthy=True
// with reason NoOwnedKeysOverridden and emits no event.
func TestReconcileConfig_ExtraConfigHealthyTrueOnDefaults(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := glanceForConfig()
	// image_size_cap is a real glance-api.conf key the operator does not own, so
	// overriding it must not trip the guard.
	glance.Spec.ExtraConfig = map[string]map[string]string{
		"DEFAULT": {"image_size_cap": "1073741824"},
	}
	r := newGlanceTestReconciler(glance)

	_, _, err := r.reconcileConfig(context.Background(), r.Client, glance, validProjection())
	g.Expect(err).NotTo(HaveOccurred())

	cond := meta.FindStatusCondition(glance.Status.Conditions, config.ConditionTypeExtraConfigHealthy)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(config.ConditionReasonNoOwnedKeysOverridden))
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).To(BeEmpty())
}

// TestOperatorDefaults_EventletWorkers pins the launch-mode-conditional worker
// count: below 2026.1 (eventlet) an unset spec.apiServer.workers must still
// render a bounded [DEFAULT] workers so the count never silently scales with the
// node's CPU count and OOMs the container; an explicit value wins. From 2026.1
// (uWSGI) the key is inert, so an unset workers renders nothing.
func TestOperatorDefaults_EventletWorkers(t *testing.T) {
	makeGlance := func(release string, workers *int32) *glancev1alpha1.Glance {
		glance := testGlance()
		glance.Spec.OpenStackRelease = release
		if workers != nil {
			glance.Spec.APIServer = &glancev1alpha1.APIServerSpec{Workers: workers}
		}
		return glance
	}

	expectedDefault := fmt.Sprintf("%d", glancev1alpha1.DefaultEventletWorkers)

	tests := []struct {
		name        string
		release     string
		workers     *int32
		wantWorkers string // "" means the key must be absent
	}{
		{"eventlet unset renders bounded default", "2025.2", nil, expectedDefault},
		{"eventlet explicit wins over default", "2025.2", ptr.To(int32(4)), "4"},
		{"uwsgi unset renders nothing", "2026.1", nil, ""},
		{"uwsgi explicit still rendered inert", "2026.1", ptr.To(int32(4)), "4"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			defaults := operatorDefaults(makeGlance(tc.release, tc.workers), validProjection())
			got, present := defaults["DEFAULT"]["workers"]
			if tc.wantWorkers == "" {
				g.Expect(present).To(BeFalse(), "workers must not render under uWSGI when unset")
				return
			}
			g.Expect(present).To(BeTrue(), "workers must render")
			g.Expect(got).To(Equal(tc.wantWorkers))
		})
	}
}

// TestEffectiveImportFiltering pins the render-time resolution of
// spec.importFiltering: which unset lists pick up an operator default, and the
// one carve-out left — the host denylist glance would ignore anyway.
//
// The security-relevant half of the contract is that no value a CR sets can
// widen a default resolved for another field: a deny-list entry keeps the
// sibling allow-list default, and a host deny-list is unioned onto the baseline
// instead of replacing it. Only an explicitly empty list opts out, so the
// expectations below spell out nil and empty separately.
func TestEffectiveImportFiltering(t *testing.T) {
	tests := []struct {
		name string
		in   *glancev1alpha1.ImportFilteringSpec
		want glancev1alpha1.ImportFilteringSpec
	}{
		{
			name: "nil block resolves to the operator defaults",
			in:   nil,
			want: glancev1alpha1.ImportFilteringSpec{
				AllowedSchemes:  glancev1alpha1.DefaultImportAllowedSchemes,
				AllowedPorts:    glancev1alpha1.DefaultImportAllowedPorts,
				DisallowedHosts: glancev1alpha1.DefaultImportDisallowedHosts,
			},
		},
		{
			name: "empty block resolves to the operator defaults",
			in:   &glancev1alpha1.ImportFilteringSpec{},
			want: glancev1alpha1.ImportFilteringSpec{
				AllowedSchemes:  glancev1alpha1.DefaultImportAllowedSchemes,
				AllowedPorts:    glancev1alpha1.DefaultImportAllowedPorts,
				DisallowedHosts: glancev1alpha1.DefaultImportDisallowedHosts,
			},
		},
		{
			name: "explicitly empty allowedSchemes opts out of the default",
			in:   &glancev1alpha1.ImportFilteringSpec{AllowedSchemes: []string{}},
			want: glancev1alpha1.ImportFilteringSpec{
				AllowedSchemes:  []string{},
				AllowedPorts:    glancev1alpha1.DefaultImportAllowedPorts,
				DisallowedHosts: glancev1alpha1.DefaultImportDisallowedHosts,
			},
		},
		{
			// A deny-list is a tightening edit: it must not cost the sibling
			// allow-list default (which would leave every other scheme reachable).
			name: "disallowedSchemes keeps the allowedSchemes default",
			in:   &glancev1alpha1.ImportFilteringSpec{DisallowedSchemes: []string{"http"}},
			want: glancev1alpha1.ImportFilteringSpec{
				AllowedSchemes:    glancev1alpha1.DefaultImportAllowedSchemes,
				DisallowedSchemes: []string{"http"},
				AllowedPorts:      glancev1alpha1.DefaultImportAllowedPorts,
				DisallowedHosts:   glancev1alpha1.DefaultImportDisallowedHosts,
			},
		},
		{
			// The documented way to make a deny-list authoritative: empty the
			// sibling allow-list explicitly, which glance needs anyway.
			name: "explicitly empty allowedSchemes activates disallowedSchemes",
			in: &glancev1alpha1.ImportFilteringSpec{
				AllowedSchemes:    []string{},
				DisallowedSchemes: []string{"http"},
			},
			want: glancev1alpha1.ImportFilteringSpec{
				AllowedSchemes:    []string{},
				DisallowedSchemes: []string{"http"},
				AllowedPorts:      glancev1alpha1.DefaultImportAllowedPorts,
				DisallowedHosts:   glancev1alpha1.DefaultImportDisallowedHosts,
			},
		},
		{
			name: "allowedHosts clears the disallowedHosts default",
			in:   &glancev1alpha1.ImportFilteringSpec{AllowedHosts: []string{"mirror.example.com"}},
			want: glancev1alpha1.ImportFilteringSpec{
				AllowedSchemes: glancev1alpha1.DefaultImportAllowedSchemes,
				AllowedHosts:   []string{"mirror.example.com"},
				AllowedPorts:   glancev1alpha1.DefaultImportAllowedPorts,
			},
		},
		{
			name: "disallowedPorts keeps the allowedPorts default",
			in:   &glancev1alpha1.ImportFilteringSpec{DisallowedPorts: []int32{22}},
			want: glancev1alpha1.ImportFilteringSpec{
				AllowedSchemes:  glancev1alpha1.DefaultImportAllowedSchemes,
				AllowedPorts:    glancev1alpha1.DefaultImportAllowedPorts,
				DisallowedPorts: []int32{22},
				DisallowedHosts: glancev1alpha1.DefaultImportDisallowedHosts,
			},
		},
		{
			// The host denylist is a security floor: user entries extend it, they
			// do not replace it, so loopback and the metadata address stay denied.
			name: "disallowedHosts entries are unioned onto the baseline",
			in:   &glancev1alpha1.ImportFilteringSpec{DisallowedHosts: []string{"metadata.example.com", "localhost"}},
			want: glancev1alpha1.ImportFilteringSpec{
				AllowedSchemes: glancev1alpha1.DefaultImportAllowedSchemes,
				AllowedPorts:   glancev1alpha1.DefaultImportAllowedPorts,
				// Baseline first in its own order, then the entries it does not
				// already carry — "localhost" is deduplicated.
				DisallowedHosts: append(append([]string{}, glancev1alpha1.DefaultImportDisallowedHosts...), "metadata.example.com"),
			},
		},
		{
			name: "explicitly empty disallowedHosts opts out of the baseline",
			in:   &glancev1alpha1.ImportFilteringSpec{DisallowedHosts: []string{}},
			want: glancev1alpha1.ImportFilteringSpec{
				AllowedSchemes:  glancev1alpha1.DefaultImportAllowedSchemes,
				AllowedPorts:    glancev1alpha1.DefaultImportAllowedPorts,
				DisallowedHosts: []string{},
			},
		},
		{
			name: "explicitly empty allowedPorts opts out of the default",
			in:   &glancev1alpha1.ImportFilteringSpec{AllowedPorts: []int32{}},
			want: glancev1alpha1.ImportFilteringSpec{
				AllowedSchemes:  glancev1alpha1.DefaultImportAllowedSchemes,
				AllowedPorts:    []int32{},
				DisallowedHosts: glancev1alpha1.DefaultImportDisallowedHosts,
			},
		},
		{
			name: "allow-lists set on every attribute pass through unchanged",
			in: &glancev1alpha1.ImportFilteringSpec{
				AllowedSchemes: []string{"https"},
				AllowedHosts:   []string{"mirror.example.com"},
				AllowedPorts:   []int32{443, 8443},
			},
			want: glancev1alpha1.ImportFilteringSpec{
				AllowedSchemes: []string{"https"},
				AllowedHosts:   []string{"mirror.example.com"},
				AllowedPorts:   []int32{443, 8443},
			},
		},
		{
			// The exploit shape the resolver must refuse: three deny-list entries
			// meant to tighten the filter must not empty the allow-lists (opening
			// every port and scheme) nor drop the host baseline.
			name: "deny-lists on every attribute keep every allow-side default",
			in: &glancev1alpha1.ImportFilteringSpec{
				DisallowedSchemes: []string{"http"},
				DisallowedHosts:   []string{"metadata.example.com"},
				DisallowedPorts:   []int32{22},
			},
			want: glancev1alpha1.ImportFilteringSpec{
				AllowedSchemes:    glancev1alpha1.DefaultImportAllowedSchemes,
				DisallowedSchemes: []string{"http"},
				AllowedPorts:      glancev1alpha1.DefaultImportAllowedPorts,
				DisallowedPorts:   []int32{22},
				DisallowedHosts:   append(append([]string{}, glancev1alpha1.DefaultImportDisallowedHosts...), "metadata.example.com"),
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			g.Expect(effectiveImportFiltering(tc.in)).To(Equal(tc.want))
		})
	}

	t.Run("nil block is indistinguishable from an empty one", func(t *testing.T) {
		g := NewGomegaWithT(t)
		g.Expect(effectiveImportFiltering(nil)).To(Equal(effectiveImportFiltering(&glancev1alpha1.ImportFilteringSpec{})))
	})

	// The baseline is a package-level slice shared by every reconcile, so the
	// union must copy rather than append into it — otherwise one CR's entries
	// would leak into the next CR's denylist.
	t.Run("the union leaves the baseline slice untouched", func(t *testing.T) {
		g := NewGomegaWithT(t)
		baseline := append([]string{}, glancev1alpha1.DefaultImportDisallowedHosts...)
		effectiveImportFiltering(&glancev1alpha1.ImportFilteringSpec{DisallowedHosts: []string{"metadata.example.com"}})
		g.Expect(glancev1alpha1.DefaultImportDisallowedHosts).To(Equal(baseline))
	})
}

// TestOperatorDefaults_ImportFilteringRendering pins the wire format of the
// [import_filtering_opts] group: comma-joined oslo ListOpt values in spec order
// (not sorted), each wrapped in the brackets every one of the six options
// requires — they are all declared bounds=True upstream, and oslo.config
// rejects an unbracketed value with `Value should start with "["`. Every one of
// the six keys is present even when its resolved list is empty, as "[]".
func TestOperatorDefaults_ImportFilteringRendering(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := glanceForConfig()
	glance.Spec.ImportFiltering = &glancev1alpha1.ImportFilteringSpec{
		AllowedHosts:      []string{"mirror.example.com", "images.example.org"},
		AllowedPorts:      []int32{80, 443},
		DisallowedSchemes: []string{"http"},
	}

	section := operatorDefaults(glance, validProjection())["import_filtering_opts"]
	g.Expect(section).To(HaveKeyWithValue("allowed_hosts", "[mirror.example.com,images.example.org]"))
	g.Expect(section).To(HaveKeyWithValue("allowed_ports", "[80,443]"))
	g.Expect(section).To(HaveKeyWithValue("disallowed_schemes", "[http]"))
	// allowedSchemes is unset, so it keeps the operator default even though a
	// deny-list is set — which makes the deny-list above inert until the CR
	// empties this list explicitly.
	g.Expect(section).To(HaveKeyWithValue("allowed_schemes", "[https]"))
	// The two lists the resolver leaves empty are still rendered, so the file
	// never falls back to glance's own permissive default for them. "[]" is an
	// empty oslo list; a bare "" does not parse at all.
	g.Expect(section).To(HaveKeyWithValue("disallowed_hosts", "[]"))
	g.Expect(section).To(HaveKeyWithValue("disallowed_ports", "[]"))
}

// TestOperatorDefaults_ImportFilteringIsBracketBounded is the regression test
// for the unbracketed render that reached CI: glance reads these options lazily,
// inside validate_import_uri, so an unparseable value passes every deployment
// and readiness check and surfaces only as a 500 on the first web-download
// import. No test above would have caught it, because they all compared against
// the same wrong format the renderer produced. This one checks the property
// oslo.config actually enforces, for all six keys and for every shape the
// resolver can produce — defaults, explicit lists, and explicit opt-outs.
func TestOperatorDefaults_ImportFilteringIsBracketBounded(t *testing.T) {
	specs := map[string]*glancev1alpha1.ImportFilteringSpec{
		"nil block (operator defaults)": nil,
		"empty block":                   {},
		"allow-lists set": {
			AllowedSchemes: []string{"http", "https"},
			AllowedHosts:   []string{"mirror.example.com"},
			AllowedPorts:   []int32{80, 443},
		},
		"deny-lists set with the allow-side emptied": {
			AllowedSchemes:    []string{},
			AllowedPorts:      []int32{},
			DisallowedSchemes: []string{"http"},
			DisallowedHosts:   []string{"evil.example.com"},
			DisallowedPorts:   []int32{22},
		},
		"every list explicitly empty": {
			AllowedSchemes:    []string{},
			DisallowedSchemes: []string{},
			AllowedHosts:      []string{},
			DisallowedHosts:   []string{},
			AllowedPorts:      []int32{},
			DisallowedPorts:   []int32{},
		},
	}
	keys := []string{
		"allowed_schemes", "disallowed_schemes",
		"allowed_hosts", "disallowed_hosts",
		"allowed_ports", "disallowed_ports",
	}

	for name, spec := range specs {
		t.Run(name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			glance := glanceForConfig()
			glance.Spec.ImportFiltering = spec

			section := operatorDefaults(glance, validProjection())["import_filtering_opts"]
			for _, key := range keys {
				value, ok := section[key]
				g.Expect(ok).To(BeTrue(), "[import_filtering_opts] %s must always render", key)
				g.Expect(value).To(HavePrefix("["), "%s = %q is not bracket-bounded; oslo.config "+
					"rejects it with `Value should start with \"[\"`", key, value)
				g.Expect(value).To(HaveSuffix("]"), "%s = %q is not bracket-bounded; oslo.config "+
					"rejects it with `Value should end with \"]\"`", key, value)
			}
		})
	}
}

// TestEffectiveImportPlugins pins the render-time resolution of
// spec.importPlugins: the presence of a sub-block is the opt-in for its plugin,
// and only a block that is set resolves defaults for its own fields.
//
// The nil-versus-empty half of the contract is the one spec.importFiltering
// carries: an unset ignoreUserRoles picks up glance's admin exemption, while an
// explicitly empty list is honored verbatim and is the only way to have the
// plugin inject for admins too.
func TestEffectiveImportPlugins(t *testing.T) {
	tests := []struct {
		name string
		in   *glancev1alpha1.ImportPluginsSpec
		want glancev1alpha1.ImportPluginsSpec
	}{
		{
			name: "nil block enables no plugin",
			in:   nil,
		},
		{
			name: "empty block enables no plugin",
			in:   &glancev1alpha1.ImportPluginsSpec{},
		},
		{
			// The block carries no fields, so resolution only has to keep it set.
			name: "decompression passes through",
			in:   &glancev1alpha1.ImportPluginsSpec{Decompression: &glancev1alpha1.ImportDecompressionSpec{}},
			want: glancev1alpha1.ImportPluginsSpec{Decompression: &glancev1alpha1.ImportDecompressionSpec{}},
		},
		{
			name: "empty conversion resolves the default output format",
			in:   &glancev1alpha1.ImportPluginsSpec{Conversion: &glancev1alpha1.ImportConversionSpec{}},
			want: glancev1alpha1.ImportPluginsSpec{
				Conversion: &glancev1alpha1.ImportConversionSpec{OutputFormat: "raw"},
			},
		},
		{
			name: "an explicit output format is honored",
			in: &glancev1alpha1.ImportPluginsSpec{
				Conversion: &glancev1alpha1.ImportConversionSpec{OutputFormat: "qcow2"},
			},
			want: glancev1alpha1.ImportPluginsSpec{
				Conversion: &glancev1alpha1.ImportConversionSpec{OutputFormat: "qcow2"},
			},
		},
		{
			name: "nil ignoreUserRoles resolves the admin exemption",
			in: &glancev1alpha1.ImportPluginsSpec{
				InjectMetadata: &glancev1alpha1.ImportInjectMetadataSpec{
					Properties: map[string]string{"hw_disk_bus": "scsi"},
				},
			},
			want: glancev1alpha1.ImportPluginsSpec{
				InjectMetadata: &glancev1alpha1.ImportInjectMetadataSpec{
					Properties:      map[string]string{"hw_disk_bus": "scsi"},
					IgnoreUserRoles: []string{"admin"},
				},
			},
		},
		{
			// The opt-in to injecting for admins too: the empty list survives, it
			// does not fall back to the default the nil case picks up.
			name: "explicitly empty ignoreUserRoles opts out of the exemption",
			in: &glancev1alpha1.ImportPluginsSpec{
				InjectMetadata: &glancev1alpha1.ImportInjectMetadataSpec{
					Properties:      map[string]string{"hw_disk_bus": "scsi"},
					IgnoreUserRoles: []string{},
				},
			},
			want: glancev1alpha1.ImportPluginsSpec{
				InjectMetadata: &glancev1alpha1.ImportInjectMetadataSpec{
					Properties:      map[string]string{"hw_disk_bus": "scsi"},
					IgnoreUserRoles: []string{},
				},
			},
		},
		{
			name: "an explicit ignoreUserRoles list is honored verbatim",
			in: &glancev1alpha1.ImportPluginsSpec{
				InjectMetadata: &glancev1alpha1.ImportInjectMetadataSpec{
					Properties:      map[string]string{"hw_disk_bus": "scsi"},
					IgnoreUserRoles: []string{"admin", "service"},
				},
			},
			want: glancev1alpha1.ImportPluginsSpec{
				InjectMetadata: &glancev1alpha1.ImportInjectMetadataSpec{
					Properties:      map[string]string{"hw_disk_bus": "scsi"},
					IgnoreUserRoles: []string{"admin", "service"},
				},
			},
		},
		{
			name: "all three blocks resolve independently",
			in: &glancev1alpha1.ImportPluginsSpec{
				Decompression: &glancev1alpha1.ImportDecompressionSpec{},
				Conversion:    &glancev1alpha1.ImportConversionSpec{},
				InjectMetadata: &glancev1alpha1.ImportInjectMetadataSpec{
					Properties: map[string]string{"hw_disk_bus": "scsi"},
				},
			},
			want: glancev1alpha1.ImportPluginsSpec{
				Decompression: &glancev1alpha1.ImportDecompressionSpec{},
				Conversion:    &glancev1alpha1.ImportConversionSpec{OutputFormat: "raw"},
				InjectMetadata: &glancev1alpha1.ImportInjectMetadataSpec{
					Properties:      map[string]string{"hw_disk_bus": "scsi"},
					IgnoreUserRoles: []string{"admin"},
				},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			g.Expect(effectiveImportPlugins(tc.in)).To(Equal(tc.want))
		})
	}

	t.Run("nil block is indistinguishable from an empty one", func(t *testing.T) {
		g := NewGomegaWithT(t)
		g.Expect(effectiveImportPlugins(nil)).To(Equal(effectiveImportPlugins(&glancev1alpha1.ImportPluginsSpec{})))
	})

	// The resolver defaults into fresh sub-blocks: the CR it was handed keeps its
	// unset fields unset, so nothing writes the current default back into the
	// stored spec and an upgrade can still move it.
	t.Run("the input spec is left untouched", func(t *testing.T) {
		g := NewGomegaWithT(t)
		in := &glancev1alpha1.ImportPluginsSpec{
			Conversion: &glancev1alpha1.ImportConversionSpec{},
			InjectMetadata: &glancev1alpha1.ImportInjectMetadataSpec{
				Properties: map[string]string{"hw_disk_bus": "scsi"},
			},
		}
		got := effectiveImportPlugins(in)
		g.Expect(got.Conversion.OutputFormat).To(Equal("raw"))
		g.Expect(got.InjectMetadata.IgnoreUserRoles).To(Equal([]string{"admin"}))
		g.Expect(got.Conversion).NotTo(BeIdenticalTo(in.Conversion))
		g.Expect(got.InjectMetadata).NotTo(BeIdenticalTo(in.InjectMetadata))
		g.Expect(in.Conversion.OutputFormat).To(BeEmpty())
		g.Expect(in.InjectMetadata.IgnoreUserRoles).To(BeNil())
	})
}

// TestOperatorDefaults_ImportPluginsRendering pins the wire format of the three
// groups spec.importPlugins renders: the plugin list, the conversion format, and
// the two inject_image_metadata options.
//
// The list is always present, so a CR without the block says "no plugins" rather
// than falling back to glance's own default, and its order is the operator's, not
// the CR's. The two per-plugin groups appear only with their plugin. The one
// asymmetry that is easy to get wrong is bracketing: image_import_plugins is a
// bounded ListOpt and needs the brackets, ignore_user_roles is an unbounded one
// and would read them as part of a role name.
func TestOperatorDefaults_ImportPluginsRendering(t *testing.T) {
	defaultsFor := func(spec *glancev1alpha1.ImportPluginsSpec) map[string]map[string]string {
		glance := glanceForConfig()
		glance.Spec.ImportPlugins = spec
		return operatorDefaults(glance, validProjection())
	}
	injectProperties := map[string]string{"hw_disk_bus": "scsi"}

	t.Run("all three blocks render in the fixed order", func(t *testing.T) {
		g := NewGomegaWithT(t)
		// Decompression ahead of conversion is upstream's requirement, and the
		// literal is spelled out rather than assembled so a reordering of the
		// renderer cannot pass unnoticed.
		defaults := defaultsFor(&glancev1alpha1.ImportPluginsSpec{
			Decompression:  &glancev1alpha1.ImportDecompressionSpec{},
			Conversion:     &glancev1alpha1.ImportConversionSpec{},
			InjectMetadata: &glancev1alpha1.ImportInjectMetadataSpec{Properties: injectProperties},
		})
		g.Expect(defaults["image_import_opts"]).To(HaveKeyWithValue("image_import_plugins",
			"[image_decompression,image_conversion,inject_image_metadata]"))
	})

	t.Run("a single block renders only its own plugin", func(t *testing.T) {
		g := NewGomegaWithT(t)
		defaults := defaultsFor(&glancev1alpha1.ImportPluginsSpec{
			Conversion: &glancev1alpha1.ImportConversionSpec{},
		})
		g.Expect(defaults["image_import_opts"]).To(HaveKeyWithValue("image_import_plugins", "[image_conversion]"))
		g.Expect(defaults).NotTo(HaveKey("inject_metadata_properties"))
	})

	t.Run("a nil block renders an empty list and no plugin group", func(t *testing.T) {
		g := NewGomegaWithT(t)
		defaults := defaultsFor(nil)
		// "[]" is an empty oslo list; an absent key would leave glance at its own
		// default instead.
		g.Expect(defaults["image_import_opts"]).To(HaveKeyWithValue("image_import_plugins", "[]"))
		g.Expect(defaults).NotTo(HaveKey("image_conversion"))
		g.Expect(defaults).NotTo(HaveKey("inject_metadata_properties"))
	})

	t.Run("an empty block renders exactly like a nil one", func(t *testing.T) {
		g := NewGomegaWithT(t)
		g.Expect(defaultsFor(&glancev1alpha1.ImportPluginsSpec{})).To(Equal(defaultsFor(nil)))
	})

	t.Run("the output format defaults to raw", func(t *testing.T) {
		g := NewGomegaWithT(t)
		defaults := defaultsFor(&glancev1alpha1.ImportPluginsSpec{
			Conversion: &glancev1alpha1.ImportConversionSpec{},
		})
		g.Expect(defaults["image_conversion"]).To(HaveKeyWithValue("output_format", "raw"))
	})

	t.Run("an explicit output format is rendered", func(t *testing.T) {
		g := NewGomegaWithT(t)
		defaults := defaultsFor(&glancev1alpha1.ImportPluginsSpec{
			Conversion: &glancev1alpha1.ImportConversionSpec{OutputFormat: "qcow2"},
		})
		g.Expect(defaults["image_conversion"]).To(HaveKeyWithValue("output_format", "qcow2"))
	})

	// The oslo Dict wire format: unquoted key:value pairs joined by commas, keys
	// sorted. Sorting is what keeps the value — and the ConfigMap content hash it
	// feeds — stable across reconciles, so the render is repeated often enough
	// that a map-order render would show up.
	t.Run("inject renders sorted key:value pairs and is stable", func(t *testing.T) {
		g := NewGomegaWithT(t)
		spec := &glancev1alpha1.ImportPluginsSpec{
			InjectMetadata: &glancev1alpha1.ImportInjectMetadataSpec{
				Properties: map[string]string{
					"os_distro":        "ubuntu",
					"hw_disk_bus":      "scsi",
					"hw_scsi_model":    "virtio-scsi",
					"provenance":       "https://images.example.com/builds",
					"hw_qemu_guest_ag": "yes",
				},
			},
		}
		want := "hw_disk_bus:scsi,hw_qemu_guest_ag:yes,hw_scsi_model:virtio-scsi," +
			"os_distro:ubuntu,provenance:https://images.example.com/builds"
		for i := range 32 {
			got := defaultsFor(spec)["inject_metadata_properties"]["inject"]
			g.Expect(got).To(Equal(want), "render %d differs; Go map order must not reach the rendered value", i)
		}
	})

	roleTests := []struct {
		name  string
		roles []string
		want  string
	}{
		{name: "nil roles resolve the admin exemption", roles: nil, want: "admin"},
		{name: "an explicit list is joined verbatim", roles: []string{"admin", "service"}, want: "admin,service"},
		{name: "an explicitly empty list drops the exemption", roles: []string{}, want: ""},
	}
	for _, tc := range roleTests {
		t.Run("ignore_user_roles: "+tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			defaults := defaultsFor(&glancev1alpha1.ImportPluginsSpec{
				InjectMetadata: &glancev1alpha1.ImportInjectMetadataSpec{
					Properties:      injectProperties,
					IgnoreUserRoles: tc.roles,
				},
			})
			section := defaults["inject_metadata_properties"]
			g.Expect(section).To(HaveKeyWithValue("ignore_user_roles", tc.want))
			// Unlike every [import_filtering_opts] list, this one is an unbounded
			// ListOpt: brackets would become part of the first and last role name,
			// so the exemption would silently match nobody.
			g.Expect(section["ignore_user_roles"]).NotTo(HavePrefix("["),
				"ignore_user_roles is an unbounded ListOpt; brackets are read as item text")
			g.Expect(section["ignore_user_roles"]).NotTo(HaveSuffix("]"),
				"ignore_user_roles is an unbounded ListOpt; brackets are read as item text")
		})
	}
}

// TestOperatorDefaults_RegistryDriftGuard is the completeness check tying
// operatorDefaults to glancev1alpha1.OwnedConfigKeys: every key the operator
// renders must be registered (forward) and every registered key must be
// rendered (reverse), so the registry and the renderer cannot drift apart.
func TestOperatorDefaults_RegistryDriftGuard(t *testing.T) {
	glance := glanceForConfig()
	// glanceForConfig already sets Region=RegionOne (emits region_name), Workers
	// (emits workers), and Cache.Servers (emits memcached_servers). Add json
	// logging with per-logger levels and debug so log_config_append,
	// default_log_levels, and debug all render.
	glance.Spec.Logging = &glancev1alpha1.LoggingSpec{
		Format:          "json",
		Debug:           ptr.To(true),
		PerLoggerLevels: map[string]string{"glance": "DEBUG"},
	}
	// Enable the image cache too, so the three conditionally rendered
	// image_cache_* keys are covered by both directions of the check.
	glance.Spec.ImageCache = &glancev1alpha1.ImageCacheSpec{}
	// All three import plugins, so the conditionally rendered [image_conversion]
	// and [inject_metadata_properties] keys are covered as well.
	glance.Spec.ImportPlugins = &glancev1alpha1.ImportPluginsSpec{
		Decompression: &glancev1alpha1.ImportDecompressionSpec{},
		Conversion:    &glancev1alpha1.ImportConversionSpec{},
		InjectMetadata: &glancev1alpha1.ImportInjectMetadataSpec{
			Properties: map[string]string{"hw_disk_bus": "scsi"},
		},
	}

	defaults := operatorDefaults(glance, validProjection())
	defaults = config.InjectOsloPolicyConfig(defaults, policyFilePath)

	registered := make(map[[2]string]struct{}, len(glancev1alpha1.OwnedConfigKeys))
	for _, o := range glancev1alpha1.OwnedConfigKeys {
		registered[[2]string{o.Section, o.Key}] = struct{}{}
	}

	// Forward check: every rendered (section, key) is registered — no exceptions.
	for section, kvs := range defaults {
		for key := range kvs {
			if _, ok := registered[[2]string{section, key}]; ok {
				continue
			}
			t.Errorf("operatorDefaults renders unregistered key [%s] %s: add it to "+
				"glancev1alpha1.OwnedConfigKeys", section, key)
		}
	}

	// Reverse check: every registered key is rendered by operatorDefaults or on
	// the extras list. keystone_authtoken/password is never rendered —
	// keystoneauth.Section omits it — but is registered because a user putting
	// it in spec.extraConfig leaks the credential.
	reverseExtras := map[[2]string]struct{}{
		{"keystone_authtoken", "password"}: {},
	}
	for _, o := range glancev1alpha1.OwnedConfigKeys {
		if _, ok := defaults[o.Section][o.Key]; ok {
			continue
		}
		if _, ok := reverseExtras[[2]string{o.Section, o.Key}]; ok {
			continue
		}
		t.Errorf("registry key [%s] %s is not rendered by operatorDefaults: remove it "+
			"from glancev1alpha1.OwnedConfigKeys or extend the drift-guard extras list", o.Section, o.Key)
	}
}

// --- image cache ---

// TestEffectiveImageCache pins the render-time resolution of spec.imageCache:
// the block's presence is the opt-in, and each of its two fields defaults
// independently, so an empty block and a half-filled one both come back complete.
// The resolver hands back a copy, which is what lets the callers dereference
// SizeLimit without reaching into the CR.
func TestEffectiveImageCache(t *testing.T) {
	tests := []struct {
		name           string
		in             *glancev1alpha1.ImageCacheSpec
		wantSizeLimit  string // "" means the resolver must return nil
		wantMaintCycle time.Duration
	}{
		{
			name: "nil block leaves the cache disabled",
			in:   nil,
		},
		{
			name:           "empty block resolves both operator defaults",
			in:             &glancev1alpha1.ImageCacheSpec{},
			wantSizeLimit:  "10Gi",
			wantMaintCycle: glancev1alpha1.DefaultImageCacheMaintenanceInterval,
		},
		{
			name:           "sizeLimit set keeps it and defaults the interval",
			in:             &glancev1alpha1.ImageCacheSpec{SizeLimit: ptr.To(resource.MustParse("256Mi"))},
			wantSizeLimit:  "256Mi",
			wantMaintCycle: glancev1alpha1.DefaultImageCacheMaintenanceInterval,
		},
		{
			name:           "maintenanceInterval set keeps it and defaults the size",
			in:             &glancev1alpha1.ImageCacheSpec{MaintenanceInterval: &metav1.Duration{Duration: time.Minute}},
			wantSizeLimit:  "10Gi",
			wantMaintCycle: time.Minute,
		},
		{
			name: "both set pass through unchanged",
			in: &glancev1alpha1.ImageCacheSpec{
				SizeLimit:           ptr.To(resource.MustParse("512Mi")),
				MaintenanceInterval: &metav1.Duration{Duration: 30 * time.Minute},
			},
			wantSizeLimit:  "512Mi",
			wantMaintCycle: 30 * time.Minute,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			got := effectiveImageCache(tc.in)
			if tc.wantSizeLimit == "" {
				g.Expect(got).To(BeNil(), "a nil block must stay nil — presence is the opt-in")
				return
			}
			g.Expect(got).NotTo(BeNil())
			g.Expect(got.SizeLimit).NotTo(BeNil())
			g.Expect(got.SizeLimit.String()).To(Equal(tc.wantSizeLimit))
			g.Expect(got.MaintenanceInterval).NotTo(BeNil())
			g.Expect(got.MaintenanceInterval.Duration).To(Equal(tc.wantMaintCycle))
		})
	}

	// The resolver defaults into a copy: the CR it was handed keeps its unset
	// fields unset, so nothing writes the current default back into the stored
	// spec and an upgrade can still move it.
	t.Run("the input spec is left untouched", func(t *testing.T) {
		g := NewGomegaWithT(t)
		in := &glancev1alpha1.ImageCacheSpec{}
		got := effectiveImageCache(in)
		g.Expect(got).NotTo(BeIdenticalTo(in))
		g.Expect(in.SizeLimit).To(BeNil())
		g.Expect(in.MaintenanceInterval).To(BeNil())
	})
}

// TestImageCacheMaxSizeBytes pins the pruner threshold derived from the emptyDir
// bound: 80% of it, computed divide-first.
func TestImageCacheMaxSizeBytes(t *testing.T) {
	tests := []struct {
		limit string
		want  int64
	}{
		// 10737418240 / 10 * 8 — the default bound, and the one case that divides
		// evenly.
		{"10Gi", 8589934592},
		// 268435456 / 10 truncates to 26843545 BEFORE the multiply, so the result
		// is 214748360 rather than the 214748364 a multiply-first order would
		// give. Truncating down is the safe direction: it can only leave the
		// eviction headroom larger, never smaller.
		{"256Mi", 214748360},
		// The 1Mi admission floor: 1048576 / 10 * 8.
		{"1Mi", 838856},
		// The other half of the divide-before-multiply contract. 4Ei is
		// 4611686018427387904 bytes, which still fits an int64 — but multiplying
		// it by 8 first does not, so a reordered Value()*8/10 wraps negative here
		// while the truncating order stays exact.
		{"4Ei", 3689348814741910320},
	}
	for _, tc := range tests {
		t.Run(tc.limit, func(t *testing.T) {
			g := NewGomegaWithT(t)
			g.Expect(imageCacheMaxSizeBytes(resource.MustParse(tc.limit))).To(Equal(tc.want))
		})
	}
}

// TestOperatorDefaults_ImageCacheKeys pins the three [DEFAULT] keys the cache
// renders — and the two it deliberately leaves to glance. image_cache_stall_time
// and image_cache_sqlite_db keep glance's own defaults, so rendering them would
// only claim ownership of two more keys for no behavioural gain.
func TestOperatorDefaults_ImageCacheKeys(t *testing.T) {
	t.Run("enabled renders exactly three keys", func(t *testing.T) {
		g := NewGomegaWithT(t)
		glance := glanceForConfig()
		glance.Spec.ImageCache = &glancev1alpha1.ImageCacheSpec{}

		section := operatorDefaults(glance, validProjection())["DEFAULT"]
		g.Expect(section).To(HaveKeyWithValue("image_cache_dir", "/var/lib/glance/image-cache"))
		// sqlite, not glance's default centralized_db, which would key the cache
		// state in the database per worker_self_reference_url and strand a dead
		// pod's rows there on every replacement.
		g.Expect(section).To(HaveKeyWithValue("image_cache_driver", "sqlite"))
		g.Expect(section).To(HaveKeyWithValue("image_cache_max_size", "8589934592"))
		g.Expect(section).NotTo(HaveKey("image_cache_stall_time"))
		g.Expect(section).NotTo(HaveKey("image_cache_sqlite_db"))
	})

	t.Run("disabled renders none of them", func(t *testing.T) {
		g := NewGomegaWithT(t)
		glance := glanceForConfig()
		glance.Spec.ImageCache = nil

		section := operatorDefaults(glance, validProjection())["DEFAULT"]
		g.Expect(section).NotTo(HaveKey("image_cache_dir"))
		g.Expect(section).NotTo(HaveKey("image_cache_driver"))
		g.Expect(section).NotTo(HaveKey("image_cache_max_size"))
	})
}

// TestOperatorDefaults_ImageCacheExtraConfigRejected pins the registry posture
// the renderer depends on: all three cache keys are Rejected, so the validating
// webhook blocks the override at admission and the merge below never sees one.
// That matters because extraConfig wins over every operator default in
// reconcileConfig, and ExtraConfigHealthy is informational — it never gates
// Ready — so a merged image_cache_max_size above the emptyDir bound would leave
// the pruner nothing to prune down to and the kubelet evicting pods, with the
// CR still reporting Ready=True. The admission side is covered by
// TestGlanceValidate_ExtraConfigImageCacheOwnedKeyRejected.
func TestOperatorDefaults_ImageCacheExtraConfigRejected(t *testing.T) {
	g := NewGomegaWithT(t)

	for _, key := range []string{"image_cache_dir", "image_cache_driver", "image_cache_max_size"} {
		overrides := config.FindOwnedOverrides(
			map[string]map[string]string{"DEFAULT": {key: "whatever"}},
			glancev1alpha1.OwnedConfigKeys,
		)
		g.Expect(overrides).To(ContainElement(And(
			HaveField("Section", "DEFAULT"),
			HaveField("Key", key),
			HaveField("Rejected", BeTrue()),
			HaveField("OwnedBy", "spec.imageCache"),
		)), "[DEFAULT] %s must be blocked at admission, not merely reported", key)
	}
}

// TestRenderPasteINI_ImageCache pins where the injected cache filter lands in the
// pipeline: last of the after-positioned filters and directly before the root
// app, so every request reaching the versioned apps passes the cache — upstream's
// keystone+caching flavor places it in the same spot. With the cache off the
// pipeline directive must stay byte-identical to what it renders today.
func TestRenderPasteINI_ImageCache(t *testing.T) {
	t.Run("enabled without user middleware", func(t *testing.T) {
		g := NewGomegaWithT(t)
		glance := glanceForConfig()
		glance.Spec.Middleware = nil
		glance.Spec.ImageCache = &glancev1alpha1.ImageCacheSpec{}

		paste, err := renderPasteINI(glance)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(paste).To(ContainSubstring("\npipeline = cors http_proxy_to_wsgi versionnegotiation authtoken context cache rootapp\n"))
		// The renderer auto-emits the filter section, so no static counterpart is
		// needed in glanceStaticPasteSections.
		g.Expect(paste).To(ContainSubstring("[filter:cache]"))
		g.Expect(paste).To(ContainSubstring("paste.filter_factory = glance.api.middleware.cache:CacheFilter.factory"))
	})

	t.Run("enabled after a user after-middleware", func(t *testing.T) {
		g := NewGomegaWithT(t)
		glance := glanceForConfig()
		glance.Spec.Middleware = []commonv1.MiddlewareSpec{{
			Name:          "myfilter",
			FilterFactory: "myfilter:filter_factory",
			Position:      commonv1.PipelinePositionAfter,
		}}
		glance.Spec.ImageCache = &glancev1alpha1.ImageCacheSpec{}

		paste, err := renderPasteINI(glance)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(paste).To(ContainSubstring("\npipeline = cors http_proxy_to_wsgi versionnegotiation authtoken context myfilter cache rootapp\n"))

		// The injection extends a clone: appending onto the CR's own slice would
		// write into its backing array whenever it has spare capacity, leaving the
		// reconciler having mutated the spec it renders from.
		g.Expect(glance.Spec.Middleware).To(HaveLen(1))
		g.Expect(glance.Spec.Middleware[0].Name).To(Equal("myfilter"))
	})

	t.Run("disabled renders today's pipeline", func(t *testing.T) {
		g := NewGomegaWithT(t)
		glance := glanceForConfig()
		glance.Spec.Middleware = nil
		glance.Spec.ImageCache = nil

		paste, err := renderPasteINI(glance)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(paste).To(ContainSubstring("\npipeline = cors http_proxy_to_wsgi versionnegotiation authtoken context rootapp\n"))
		g.Expect(paste).NotTo(ContainSubstring("filter:cache"))
	})
}

// TestRenderPasteINI_DuplicateCacheName covers the CR that can only exist behind
// a bypassed webhook: admission reserves the `cache` middleware name while
// spec.imageCache is set, so a CR carrying both never reaches the renderer
// through the normal path. When one does, the shared renderer's duplicate-name
// check is the backstop — the pipeline must fail to render rather than silently
// end up with one of the two definitions.
func TestRenderPasteINI_DuplicateCacheName(t *testing.T) {
	newGlance := func() *glancev1alpha1.Glance {
		glance := glanceForConfig()
		glance.Spec.ImageCache = &glancev1alpha1.ImageCacheSpec{}
		glance.Spec.Middleware = []commonv1.MiddlewareSpec{{
			Name:          "cache",
			FilterFactory: "rogue:filter_factory",
			Position:      commonv1.PipelinePositionAfter,
		}}
		return glance
	}

	t.Run("the renderer refuses the pipeline", func(t *testing.T) {
		g := NewGomegaWithT(t)
		_, err := renderPasteINI(newGlance())
		g.Expect(err).To(MatchError(ContainSubstring(`duplicate middleware Name "cache" in pipeline "api"`)))
	})

	t.Run("reconcileConfig fails the config step", func(t *testing.T) {
		g := NewGomegaWithT(t)
		glance := newGlance()
		r := newGlanceTestReconciler(glance)

		_, art, err := r.reconcileConfig(context.Background(), r.Client, glance, validProjection())
		g.Expect(err).To(MatchError(ContainSubstring("rendering glance-api-paste.ini:")))
		g.Expect(err).To(MatchError(ContainSubstring(`duplicate middleware Name "cache" in pipeline "api"`)))
		g.Expect(art.configMapName).To(BeEmpty())

		// The Config→SecretsReady mapping: a render failure must not leave the
		// aggregate Ready stale-True at the new generation.
		cond := meta.FindStatusCondition(glance.Status.Conditions, "SecretsReady")
		g.Expect(cond).NotTo(BeNil())
		g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(cond.Reason).To(Equal(conditionReasonConfigError))
	})
}
