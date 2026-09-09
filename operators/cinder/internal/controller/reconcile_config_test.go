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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/config"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	cinderv1alpha1 "github.com/c5c3/cobaltcore/operators/cinder/api/v1alpha1"
)

// cinderForConfig is the Keystone-backed fixture the config tests render from.
// It adds the region the shared fixture leaves empty, so the conditional
// [keystone_authtoken] region_name is covered.
func cinderForConfig() *cinderv1alpha1.Cinder {
	cinder := validCinder()
	cinder.Spec.Region = "RegionOne"
	return cinder
}

// renderConfig runs the config step over a fresh reconciler and returns both,
// so a test reads the artefact the pass wrote.
func renderConfig(t *testing.T, cinder *cinderv1alpha1.Cinder, objs ...client.Object) (*CinderReconciler, configArtifacts) {
	t.Helper()

	r := newCinderTestReconciler(append([]client.Object{cinder}, objs...)...)
	_, art, err := r.reconcileConfig(context.Background(), r.Client, cinder)
	if err != nil {
		t.Fatalf("rendering the config ConfigMap: %v", err)
	}
	return r, art
}

// renderedConfigMap re-reads the ConfigMap the config step created.
func renderedConfigMap(t *testing.T, r *CinderReconciler, name string) *corev1.ConfigMap {
	t.Helper()

	var cm corev1.ConfigMap
	key := client.ObjectKey{Namespace: testNamespace, Name: name}
	if err := r.Get(context.Background(), key, &cm); err != nil {
		t.Fatalf("re-reading config ConfigMap %s: %v", name, err)
	}
	return &cm
}

// renderCinderConf renders the fixture and returns the cinder.conf document.
func renderCinderConf(t *testing.T, cinder *cinderv1alpha1.Cinder) string {
	t.Helper()

	r, art := renderConfig(t, cinder)
	return renderedConfigMap(t, r, art.configMapName).Data[cinderConfDataKey]
}

// TestReconcileConfig_KeystoneBackedRendersAuthSections covers the deployment
// that authenticates: the pipeline is keystone, both Keystone sections are
// there, and neither carries the password — it arrives through the env override
// so it never lands in the ConfigMap every pod mounts.
func TestReconcileConfig_KeystoneBackedRendersAuthSections(t *testing.T) {
	g := NewGomegaWithT(t)
	conf := renderCinderConf(t, cinderForConfig())

	g.Expect(conf).To(ContainSubstring("auth_strategy = keystone"))
	for _, line := range []string{
		"[keystone_authtoken]",
		"auth_type = password",
		"auth_url = http://keystone.openstack.svc:5000",
		"www_authenticate_uri = http://keystone.openstack.svc:5000",
		"username = cinder",
		"project_name = service",
		"user_domain_name = Default",
		"project_domain_name = Default",
		"region_name = RegionOne",
		"memcached_servers = mc:11211",
		"[service_user]",
		"send_service_user_token = true",
	} {
		g.Expect(conf).To(ContainSubstring(line))
	}
	g.Expect(conf).NotTo(ContainSubstring("password = "),
		"both passwords arrive through the oslo.config env overrides, never through the file")
}

// TestReconcileConfig_NoauthOmitsKeystoneSections covers the Cinder the storage
// suites deploy on a cluster without an identity service: no endpoint, no
// service user, and therefore no section that would need credentials.
func TestReconcileConfig_NoauthOmitsKeystoneSections(t *testing.T) {
	g := NewGomegaWithT(t)
	conf := renderCinderConf(t, keystoneFreeCinder())

	g.Expect(conf).To(ContainSubstring("auth_strategy = noauth"))
	for _, section := range []string{"[keystone_authtoken]", "[service_user]", "[key_manager]", "[barbican]"} {
		g.Expect(conf).NotTo(ContainSubstring(section))
	}
}

// TestReconcileConfig_KeyManagerRenderedOnlyWhenSet covers both halves of the
// castellan wiring: absent by default, and pinned to the internal Barbican
// endpoint plus the Keystone castellan authenticates against when configured.
func TestReconcileConfig_KeyManagerRenderedOnlyWhenSet(t *testing.T) {
	t.Run("absent by default", func(t *testing.T) {
		g := NewGomegaWithT(t)
		conf := renderCinderConf(t, cinderForConfig())
		g.Expect(conf).NotTo(ContainSubstring("[key_manager]"))
		g.Expect(conf).NotTo(ContainSubstring("[barbican]"))
	})

	t.Run("rendered with spec.keyManager", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cinder := cinderForConfig()
		cinder.Spec.KeyManager = &cinderv1alpha1.KeyManagerSpec{
			Type:     cinderv1alpha1.KeyManagerTypeBarbican,
			Barbican: &cinderv1alpha1.BarbicanKeyManagerSpec{Endpoint: "http://barbican.openstack.svc:9311"},
		}
		conf := renderCinderConf(t, cinder)

		for _, line := range []string{
			"backend = barbican",
			"barbican_endpoint = http://barbican.openstack.svc:9311",
			"barbican_endpoint_type = internal",
			"auth_endpoint = http://keystone.openstack.svc:5000",
		} {
			g.Expect(conf).To(ContainSubstring(line))
		}
	})

	// The union rule pairs type Barbican with the block; a CR that bypassed it
	// must not crash the reconcile, and must not claim a key manager it cannot
	// address either.
	t.Run("a bypassed union rule renders nothing", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cinder := cinderForConfig()
		cinder.Spec.KeyManager = &cinderv1alpha1.KeyManagerSpec{Type: cinderv1alpha1.KeyManagerTypeBarbican}
		conf := renderCinderConf(t, cinder)

		g.Expect(conf).NotTo(ContainSubstring("[key_manager]"))
		g.Expect(conf).NotTo(ContainSubstring("[barbican]"))
	})
}

// TestReconcileConfig_PrivsepContextsAlwaysRendered pins the two privileged
// helper contexts, including the empty capability set: oslo renders an empty
// value as a bare "key = " line, which is what tells privsep to keep no
// capability at all rather than fall back to its compiled-in set.
func TestReconcileConfig_PrivsepContextsAlwaysRendered(t *testing.T) {
	for name, cinder := range map[string]*cinderv1alpha1.Cinder{
		"keystone": cinderForConfig(),
		"noauth":   keystoneFreeCinder(),
	} {
		t.Run(name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			conf := renderCinderConf(t, cinder)

			for _, section := range []string{"[cinder_sys_admin]", "[privsep_osbrick]"} {
				g.Expect(conf).To(ContainSubstring(section))
			}
			g.Expect(strings.Count(conf, "helper_command = privsep-helper --config-dir /etc/cinder/cinder.conf.d")).
				To(Equal(2), "both privsep contexts start the same helper")
			g.Expect(strings.Count(conf, "capabilities = \n")).To(Equal(2),
				"an empty capability set renders as a bare key line, not as an omitted key")
		})
	}
}

// TestReconcileConfig_StaticPathsAndNoEnabledBackends pins the image paths the
// config-free container needs spelled out, and the one key that must never
// appear: each cinder-volume gets a per-backend overlay naming its single
// backend, so enabled_backends has no place in the shared document.
func TestReconcileConfig_StaticPathsAndNoEnabledBackends(t *testing.T) {
	g := NewGomegaWithT(t)
	r, art := renderConfig(t, cinderForConfig())
	cm := renderedConfigMap(t, r, art.configMapName)

	conf := cm.Data[cinderConfDataKey]
	for _, line := range []string{
		"api_paste_config = /var/lib/openstack/etc/cinder/api-paste.ini",
		"resource_query_filters_file = /var/lib/openstack/etc/cinder/resource_filters.json",
		"state_path = /var/lib/cinder",
		"image_conversion_dir = /var/lib/cinder/conversion",
		"host = cinder",
		"use_stderr = true",
		"debug = false",
		"connection = mysql+pymysql://placeholder",
		"lock_path = /var/lib/cinder/tmp",
		"backend_url = file:///var/lib/cinder/coordination",
		"driver = noop",
	} {
		g.Expect(conf).To(ContainSubstring(line))
	}
	for _, data := range cm.Data {
		g.Expect(data).NotTo(ContainSubstring("enabled_backends"),
			"the backends are named by the per-backend volume overlay, never here")
	}
}

// TestReconcileConfig_OptionalDefaultKeys covers the [DEFAULT] keys that follow
// an optional spec field: absent when the field is, rendered when it is set.
func TestReconcileConfig_OptionalDefaultKeys(t *testing.T) {
	t.Run("absent when unset", func(t *testing.T) {
		g := NewGomegaWithT(t)
		conf := renderCinderConf(t, cinderForConfig())
		for _, key := range []string{
			"glance_api_servers",
			"cinder_internal_tenant_project_id",
			"cinder_internal_tenant_user_id",
			"default_log_levels",
			"log_config_append",
		} {
			g.Expect(conf).NotTo(ContainSubstring(key))
		}
	})

	t.Run("rendered when set", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cinder := cinderForConfig()
		cinder.Spec.GlanceEndpoint = "http://glance.openstack.svc:9292"
		cinder.Spec.InternalTenant = &cinderv1alpha1.InternalTenantSpec{
			ProjectID: "proj-id", UserID: "user-id",
		}
		cinder.Spec.Logging = &cinderv1alpha1.LoggingSpec{
			Format:          "text",
			Level:           "INFO",
			Debug:           ptr.To(true),
			PerLoggerLevels: map[string]string{"cinder": "DEBUG", "amqp": "WARN"},
		}
		conf := renderCinderConf(t, cinder)

		for _, line := range []string{
			"glance_api_servers = http://glance.openstack.svc:9292",
			"cinder_internal_tenant_project_id = proj-id",
			"cinder_internal_tenant_user_id = user-id",
			"debug = true",
			"default_log_levels = amqp=WARN,cinder=DEBUG",
		} {
			g.Expect(conf).To(ContainSubstring(line))
		}
	})
}

// TestReconcileConfig_OptionalDataKeys covers the two files that ship only when
// the spec asks for them, on both the ConfigMap and the returned artefacts the
// workload steps project from.
func TestReconcileConfig_OptionalDataKeys(t *testing.T) {
	t.Run("neither by default", func(t *testing.T) {
		g := NewGomegaWithT(t)
		r, art := renderConfig(t, cinderForConfig())

		g.Expect(art.dataKeys).To(Equal([]string{cinderConfDataKey, schedulerConfDataKey}))
		cm := renderedConfigMap(t, r, art.configMapName)
		g.Expect(cm.Data).NotTo(HaveKey(loggingINIDataKey))
		g.Expect(cm.Data).NotTo(HaveKey(policyYAMLDataKey))
	})

	t.Run("json logging ships logging.ini", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cinder := cinderForConfig()
		cinder.Spec.Logging = &cinderv1alpha1.LoggingSpec{Format: "json", Level: "DEBUG"}
		r, art := renderConfig(t, cinder)

		g.Expect(art.dataKeys).To(ContainElement(loggingINIDataKey))
		cm := renderedConfigMap(t, r, art.configMapName)
		g.Expect(cm.Data[loggingINIDataKey]).To(ContainSubstring("class = oslo_log.formatters.JSONFormatter"))
		g.Expect(cm.Data[loggingINIDataKey]).To(ContainSubstring("level = DEBUG"))
		g.Expect(cm.Data[cinderConfDataKey]).To(
			ContainSubstring("log_config_append = /etc/cinder/cinder.conf.d/logging.ini"))
	})

	t.Run("policy overrides ship policy.yaml", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cinder := cinderForConfig()
		cinder.Spec.PolicyOverrides = &commonv1.PolicySpec{
			Rules: map[string]string{"volume:create": "role:member"},
		}
		r, art := renderConfig(t, cinder)

		g.Expect(art.dataKeys).To(ContainElement(policyYAMLDataKey))
		cm := renderedConfigMap(t, r, art.configMapName)
		g.Expect(cm.Data[policyYAMLDataKey]).To(ContainSubstring("volume:create"))
		g.Expect(cm.Data[cinderConfDataKey]).To(
			ContainSubstring("policy_file = /etc/cinder/cinder.conf.d/policy.yaml"))
	})

	// A policyOverrides block naming a ConfigMap that does not exist is the
	// failure path: nothing is rendered and the condition says why.
	t.Run("an unreadable policy ConfigMap fails the step", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cinder := cinderForConfig()
		cinder.Spec.PolicyOverrides = &commonv1.PolicySpec{
			ConfigMapRef: &corev1.LocalObjectReference{Name: "missing-policy"},
		}
		r := newCinderTestReconciler(cinder)

		_, art, err := r.reconcileConfig(context.Background(), r.Client, cinder)
		g.Expect(err).To(MatchError(ContainSubstring("building policy:")))
		g.Expect(art.configMapName).To(BeEmpty())

		cond := cinderCondition(cinder, "SecretsReady")
		g.Expect(cond).NotTo(BeNil())
		g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(cond.Reason).To(Equal(conditionReasonConfigError))
	})
}

// TestReconcileConfig_SchedulerConfNamesOneHost pins the scheduler overlay: one
// identity for the whole Deployment, however many replicas run.
func TestReconcileConfig_SchedulerConfNamesOneHost(t *testing.T) {
	g := NewGomegaWithT(t)
	r, art := renderConfig(t, cinderForConfig())

	g.Expect(renderedConfigMap(t, r, art.configMapName).Data[schedulerConfDataKey]).
		To(Equal("[DEFAULT]\nhost = cinder-scheduler\n"))
}

// TestReconcileConfig_ControlCharKeepsLastGood covers the value that would inject
// further INI lines into a rendered section: nothing is re-rendered, the running
// pods keep the ConfigMap they mount, and the condition says the config failed.
func TestReconcileConfig_ControlCharKeepsLastGood(t *testing.T) {
	liveDeployment := func(cinder *cinderv1alpha1.Cinder) *appsv1.Deployment {
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: cinder.Name, Namespace: cinder.Namespace},
			Spec: appsv1.DeploymentSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Volumes: []corev1.Volume{{
							Name: configVolumeName,
							VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
								LocalObjectReference: corev1.LocalObjectReference{Name: "cinder-config-old"},
								Items: []corev1.KeyToPath{
									{Key: schedulerConfDataKey, Path: schedulerConfDataKey},
									{Key: cinderConfDataKey, Path: cinderConfDataKey},
								},
							}},
						}},
					},
				},
			},
		}
	}

	newCinder := func() *cinderv1alpha1.Cinder {
		cinder := cinderForConfig()
		cinder.Spec.ExtraConfig = map[string]map[string]string{
			"DEFAULT": {"rootwrap_config": "value\n[evil]\nkey = injected"},
		}
		return cinder
	}

	t.Run("the live Deployment's artefacts are kept", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cinder := newCinder()
		r := newCinderTestReconciler(cinder, liveDeployment(cinder))

		res, art, err := r.reconcileConfig(context.Background(), r.Client, cinder)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.IsZero()).To(BeTrue())
		g.Expect(art.configMapName).To(Equal("cinder-config-old"))
		g.Expect(art.dataKeys).To(Equal([]string{cinderConfDataKey, schedulerConfDataKey}))

		// The returned name is the live one, which was never created here: no fresh
		// ConfigMap was rendered.
		var cm corev1.ConfigMap
		key := client.ObjectKey{Namespace: testNamespace, Name: art.configMapName}
		g.Expect(r.Get(context.Background(), key, &cm)).NotTo(Succeed())

		cond := cinderCondition(cinder, "SecretsReady")
		g.Expect(cond).NotTo(BeNil())
		g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(cond.Reason).To(Equal(conditionReasonConfigError))
		g.Expect(cond.Message).To(ContainSubstring("[DEFAULT]"))
	})

	t.Run("first install has nothing to keep", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cinder := newCinder()
		r := newCinderTestReconciler(cinder)

		_, art, err := r.reconcileConfig(context.Background(), r.Client, cinder)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(art.configMapName).To(BeEmpty())
		g.Expect(art.dataKeys).To(BeEmpty())
	})
}

// TestReconcileConfig_ExtraConfigOverlay covers the escape hatch: nil and empty
// blocks render the defaults byte-identically, and a value under a reported key
// wins over the operator's.
func TestReconcileConfig_ExtraConfigOverlay(t *testing.T) {
	t.Run("nil and empty render identically", func(t *testing.T) {
		g := NewGomegaWithT(t)
		nilCinder := cinderForConfig()
		nilCinder.Spec.ExtraConfig = nil
		emptyCinder := cinderForConfig()
		emptyCinder.Spec.ExtraConfig = map[string]map[string]string{}

		_, nilArt := renderConfig(t, nilCinder)
		_, emptyArt := renderConfig(t, emptyCinder)
		g.Expect(emptyArt.configMapName).To(Equal(nilArt.configMapName),
			"the ConfigMap name is the content hash, so an empty block must not rotate it")
	})

	t.Run("a user value wins over the operator default", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cinder := cinderForConfig()
		cinder.Spec.ExtraConfig = map[string]map[string]string{
			"DEFAULT": {"state_path": "/srv/cinder"},
			"nova":    {"interface": "internal"},
		}
		conf := renderCinderConf(t, cinder)

		g.Expect(conf).To(ContainSubstring("state_path = /srv/cinder"))
		g.Expect(conf).NotTo(ContainSubstring("state_path = /var/lib/cinder\n"))
		g.Expect(conf).To(ContainSubstring("[nova]"))

		// The guard reports the take-over; it does not reject it.
		cond := cinderCondition(cinder, "ExtraConfigHealthy")
		g.Expect(cond).NotTo(BeNil())
		g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	})
}

// TestReconcileConfig_PrunesToRetainCount pins the rollback depth: three
// historical ConfigMaps survive beside the current one.
func TestReconcileConfig_PrunesToRetainCount(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := cinderForConfig()
	baseName := cinder.Name + "-config"

	stale := make([]client.Object, 0, 5)
	for i := range 5 {
		stale = append(stale, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("%s-stale%d", baseName, i),
				Namespace: testNamespace,
				Labels:    map[string]string{config.ConfigBaseLabelKey: baseName},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: cinderv1alpha1.GroupVersion.String(),
					Kind:       "Cinder",
					Name:       cinder.Name,
					UID:        cinder.UID,
					Controller: ptr.To(true),
				}},
			},
		})
	}
	r, art := renderConfig(t, cinder, stale...)

	var configMaps corev1.ConfigMapList
	g.Expect(r.List(context.Background(), &configMaps, client.InNamespace(testNamespace),
		client.MatchingLabels{config.ConfigBaseLabelKey: baseName})).To(Succeed())

	names := make([]string, 0, len(configMaps.Items))
	for i := range configMaps.Items {
		names = append(names, configMaps.Items[i].Name)
	}
	g.Expect(names).To(HaveLen(defaultConfigMapRetainCount+1),
		"the current ConfigMap plus %d historical ones", defaultConfigMapRetainCount)
	g.Expect(names).To(ContainElement(art.configMapName))
}

// TestOperatorDefaults_RegistryDriftGuard asserts that every key the renderer
// writes is registered as operator-owned. The reverse direction is deliberately
// not asserted: the registry is static by contract, so it also carries the keys
// a conditional render only sometimes emits and the credential keys the renderer
// never writes at all.
func TestOperatorDefaults_RegistryDriftGuard(t *testing.T) {
	// The maximal CR: every conditional block is on, so the forward check covers
	// the conditionally rendered keys too.
	cinder := cinderForConfig()
	cinder.Spec.GlanceEndpoint = "http://glance.openstack.svc:9292"
	cinder.Spec.InternalTenant = &cinderv1alpha1.InternalTenantSpec{ProjectID: "proj", UserID: "user"}
	cinder.Spec.KeyManager = &cinderv1alpha1.KeyManagerSpec{
		Type:     cinderv1alpha1.KeyManagerTypeBarbican,
		Barbican: &cinderv1alpha1.BarbicanKeyManagerSpec{Endpoint: "http://barbican.openstack.svc:9311"},
	}
	cinder.Spec.Messaging.TLS = &commonv1.MessagingTLSSpec{
		CABundleSecretRef: commonv1.SecretRefSpec{Name: "rabbitmq-ca"},
	}
	cinder.Spec.Logging = &cinderv1alpha1.LoggingSpec{
		Format:          "json",
		Debug:           ptr.To(true),
		PerLoggerLevels: map[string]string{"cinder": "DEBUG"},
	}

	defaults := config.InjectOsloPolicyConfig(operatorDefaults(cinder), policyFilePath)

	registered := make(map[[2]string]struct{}, len(cinderv1alpha1.OwnedConfigKeys))
	for _, owned := range cinderv1alpha1.OwnedConfigKeys {
		registered[[2]string{owned.Section, owned.Key}] = struct{}{}
	}
	for section, kvs := range defaults {
		for key := range kvs {
			if _, ok := registered[[2]string{section, key}]; ok {
				continue
			}
			t.Errorf("operatorDefaults renders unregistered key [%s] %s: add it to "+
				"cinderv1alpha1.OwnedConfigKeys", section, key)
		}
	}
}
