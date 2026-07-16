// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

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

	res, art, err := r.reconcileConfig(context.Background(), glance, validProjection())
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
	// No policy overrides configured, so no oslo_policy section.
	g.Expect(conf).NotTo(ContainSubstring("[oslo_policy]"))
}

func TestReconcileConfig_PasteContainsPipelineAndComposite(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := glanceForConfig()
	r := newGlanceTestReconciler(glance)

	_, art, err := r.reconcileConfig(context.Background(), glance, validProjection())
	g.Expect(err).NotTo(HaveOccurred())

	paste := renderedPaste(t, r, art)
	g.Expect(paste).To(ContainSubstring("[pipeline:glance-api-keystone]"))
	// The healthcheck filter must stay in the pipeline (the /healthcheck probes
	// depend on it) and the injected middleware appears after the base filters.
	g.Expect(paste).To(MatchRegexp(`pipeline = .*healthcheck.*authtoken context audit rootapp`))
	g.Expect(paste).To(ContainSubstring("[filter:healthcheck]"))
	g.Expect(paste).To(ContainSubstring("[filter:audit]"))
	// The rootapp composite the PipelineSpec cannot express.
	g.Expect(paste).To(ContainSubstring("[composite:rootapp]"))
	g.Expect(paste).To(ContainSubstring("paste.composite_factory = glance.api:root_app_factory"))
}

func TestReconcileConfig_ExtraConfigWins(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := glanceForConfig()
	glance.Spec.ExtraConfig = map[string]map[string]string{
		"DEFAULT": {"enabled_import_methods": "[copy-image]"},
	}
	r := newGlanceTestReconciler(glance)

	_, art, err := r.reconcileConfig(context.Background(), glance, validProjection())
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

	_, art, err := r.reconcileConfig(context.Background(), glance, validProjection())
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

	_, art, err := r.reconcileConfig(context.Background(), glance, validProjection())
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

	res, art, err := r.reconcileConfig(context.Background(), glance, backendsProjection{valid: false})
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

	res, art, err := r.reconcileConfig(context.Background(), glance, backendsProjection{valid: false})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(art.configMapName).To(BeEmpty())
	g.Expect(art.backendsSecretName).To(BeEmpty())
}
