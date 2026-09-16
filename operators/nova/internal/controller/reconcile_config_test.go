// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/c5c3/cobaltcore/internal/common/config"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
)

// renderConfig runs the config step over a fresh reconciler and returns both, so
// a test reads the artefact the pass wrote.
func renderConfig(t *testing.T, nova *novav1alpha1.Nova, objs ...client.Object) (*NovaReconciler, configArtifacts) {
	t.Helper()

	r := newNovaTestReconciler(append([]client.Object{nova}, objs...)...)
	_, art, err := r.reconcileConfig(context.Background(), r.Client, nova)
	if err != nil {
		t.Fatalf("rendering the config ConfigMap: %v", err)
	}
	return r, art
}

// renderedConfigMap re-reads the ConfigMap the config step created.
func renderedConfigMap(t *testing.T, r *NovaReconciler, name string) *corev1.ConfigMap {
	t.Helper()

	var cm corev1.ConfigMap
	key := client.ObjectKey{Namespace: testNamespace, Name: name}
	if err := r.Get(context.Background(), key, &cm); err != nil {
		t.Fatalf("re-reading config ConfigMap %s: %v", name, err)
	}
	return &cm
}

// renderNovaConf renders the fixture and returns the shared nova.conf document.
func renderNovaConf(t *testing.T, nova *novav1alpha1.Nova) string {
	t.Helper()

	r, art := renderConfig(t, nova)
	return renderedConfigMap(t, r, art.configMapName).Data[novaConfDataKey]
}

// sectionOf returns the body of one INI section of doc, so a test can assert a
// key belongs to the section it names rather than to the document at large.
func sectionOf(t *testing.T, doc, section string) string {
	t.Helper()

	header := "[" + section + "]\n"
	start := strings.Index(doc, header)
	if start < 0 {
		t.Fatalf("section [%s] is not in the rendered document:\n%s", section, doc)
	}
	body := doc[start+len(header):]
	if end := strings.Index(body, "\n["); end >= 0 {
		body = body[:end+1]
	}
	return body
}

// novaMaximal returns the fixture with every conditional render switched on:
// validNova's optional siblings, exposed console proxy and json logging, plus a
// verified bus, debug logging with per-logger levels and an override on every
// endpoint. A key only a switch emits is reachable from no smaller fixture.
func novaMaximal() *novav1alpha1.Nova {
	nova := novaWithMessagingTLS()
	nova.Spec.Logging = &novav1alpha1.LoggingSpec{
		Format:          "json",
		Level:           "INFO",
		Debug:           ptr.To(true),
		PerLoggerLevels: map[string]string{"nova": "DEBUG", "amqp": "WARNING"},
	}
	nova.Spec.Endpoints = novav1alpha1.NovaEndpointsSpec{
		Placement: novav1alpha1.NovaEndpointSpec{Override: "http://placement.svc:8778"},
		Neutron:   novav1alpha1.NovaEndpointSpec{Override: "http://neutron.svc:9696"},
		Glance:    novav1alpha1.NovaEndpointSpec{Override: "http://glance.svc:9292"},
		Cinder:    novav1alpha1.NovaOptionalEndpointSpec{Enabled: true, Override: "http://cinder.svc:8776"},
		Barbican:  novav1alpha1.NovaOptionalEndpointSpec{Enabled: true, Override: "http://barbican.svc:9311"},
	}
	return nova
}

// TestReconcileConfig_RendersFiveClientSections covers the full fixture: nova
// calls five services, three of them with credentials of its own, and none of
// the five carries a password, because every one of them is env-injected.
func TestReconcileConfig_RendersFiveClientSections(t *testing.T) {
	g := NewGomegaWithT(t)
	conf := renderNovaConf(t, validNova())

	for _, section := range []string{"placement", "neutron", "cinder"} {
		body := sectionOf(t, conf, section)
		g.Expect(body).To(ContainSubstring("auth_type = password"),
			"[%s] authenticates as the service account", section)
		g.Expect(body).To(ContainSubstring("username = nova"))
		g.Expect(body).NotTo(ContainSubstring("password = "),
			"[%s] reads its password from the OS_<SECTION>__PASSWORD override", section)
	}

	// Glance is reached with the token of the request nova is serving, so the
	// section addresses the service and carries no identity at all.
	glance := sectionOf(t, conf, "glance")
	g.Expect(glance).To(ContainSubstring("valid_interfaces = internal"))
	g.Expect(glance).NotTo(ContainSubstring("auth_type"))

	// Castellan authenticates against the Keystone named here with the service
	// account, and sends nova's own token so a key read outlives the user token.
	barbican := sectionOf(t, conf, "barbican")
	g.Expect(barbican).To(ContainSubstring("send_service_user_token = true"))
	g.Expect(barbican).To(ContainSubstring(
		"auth_endpoint = http://keystone.openstack.svc.cluster.local:5000"))
	g.Expect(barbican).NotTo(ContainSubstring("password"))
	g.Expect(sectionOf(t, conf, "key_manager")).To(ContainSubstring("backend = barbican"))

	// Both schemas are addressed by a placeholder the env overrides replace.
	g.Expect(sectionOf(t, conf, "api_database")).
		To(ContainSubstring("connection = " + dbConnectionPlaceholder))
	g.Expect(sectionOf(t, conf, "database")).
		To(ContainSubstring("connection = " + dbConnectionPlaceholder))
}

// TestReconcileConfig_OptionalSiblingsOff covers the standalone Nova: no volume
// service and no key manager, so neither section is there to point at a service
// the deployment does not run, and no endpoint is overridden anywhere.
func TestReconcileConfig_OptionalSiblingsOff(t *testing.T) {
	g := NewGomegaWithT(t)
	conf := renderNovaConf(t, novaMinimal())

	for _, section := range []string{"[cinder]", "[key_manager]", "[barbican]"} {
		g.Expect(conf).NotTo(ContainSubstring(section))
	}
	g.Expect(conf).NotTo(ContainSubstring("endpoint_override"),
		"an empty override leaves nova resolving the address from the catalog")
	g.Expect(conf).NotTo(ContainSubstring("endpoint_template"))
}

// TestReconcileConfig_OverridesRenderEndpointKeys covers the deployment whose
// catalog carries addresses the pods cannot reach: every one of the five
// services is addressed directly, each through the key its own section spells
// the override with.
func TestReconcileConfig_OverridesRenderEndpointKeys(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	nova.Spec.Endpoints = novav1alpha1.NovaEndpointsSpec{
		Placement: novav1alpha1.NovaEndpointSpec{Override: "http://placement.svc:8778"},
		Neutron:   novav1alpha1.NovaEndpointSpec{Override: "http://neutron.svc:9696"},
		Glance:    novav1alpha1.NovaEndpointSpec{Override: "http://glance.svc:9292"},
		Cinder:    novav1alpha1.NovaOptionalEndpointSpec{Enabled: true, Override: "http://cinder.svc:8776"},
		Barbican:  novav1alpha1.NovaOptionalEndpointSpec{Enabled: true, Override: "http://barbican.svc:9311"},
	}
	conf := renderNovaConf(t, nova)

	g.Expect(sectionOf(t, conf, "placement")).
		To(ContainSubstring("endpoint_override = http://placement.svc:8778"))
	g.Expect(sectionOf(t, conf, "neutron")).
		To(ContainSubstring("endpoint_override = http://neutron.svc:9696"))
	g.Expect(sectionOf(t, conf, "glance")).
		To(ContainSubstring("endpoint_override = http://glance.svc:9292"))
	g.Expect(sectionOf(t, conf, "cinder")).
		To(ContainSubstring("endpoint_template = http://cinder.svc:8776"))
	g.Expect(sectionOf(t, conf, "barbican")).
		To(ContainSubstring("barbican_endpoint = http://barbican.svc:9311"))
}

// TestReconcileConfig_ConsoleDisabledRendersVncOff covers the Nova whose
// consoles are switched off: the switch says so and no base URL is advertised,
// because the Service the URL would name is not projected.
func TestReconcileConfig_ConsoleDisabledRendersVncOff(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	nova.Spec.ConsoleProxy = novav1alpha1.NovaConsoleProxySpec{Enabled: ptr.To(false)}
	conf := renderNovaConf(t, nova)

	vnc := sectionOf(t, conf, "vnc")
	g.Expect(vnc).To(ContainSubstring("enabled = false"))
	g.Expect(vnc).NotTo(ContainSubstring("novncproxy_base_url"))
}

// TestReconcileConfig_ConsoleBaseURLFollowsTheGateway covers both console
// addresses: the gateway hostname for a browser outside the cluster, and the
// cluster-local Service for a Nova exposed through no gateway at all.
func TestReconcileConfig_ConsoleBaseURLFollowsTheGateway(t *testing.T) {
	t.Run("the gateway hostname wins", func(t *testing.T) {
		g := NewGomegaWithT(t)
		g.Expect(sectionOf(t, renderNovaConf(t, validNova()), "vnc")).
			To(ContainSubstring("novncproxy_base_url = https://console.example.com/vnc_lite.html"))
	})

	t.Run("without a gateway the Service is named", func(t *testing.T) {
		g := NewGomegaWithT(t)
		g.Expect(sectionOf(t, renderNovaConf(t, novaMinimal()), "vnc")).To(ContainSubstring(
			"novncproxy_base_url = http://nova-novncproxy.openstack.svc.cluster.local:6080/vnc_lite.html"))
	})
}

// TestReconcileConfig_OverlaysNeverCarryTheSharedSecret pins the two values no
// rendered file may carry: the metadata shared secret, which is env-injected and
// would otherwise land in the ConfigMap every pod mounts, and [DEFAULT] host,
// which every process fills with its own pod name.
func TestReconcileConfig_OverlaysNeverCarryTheSharedSecret(t *testing.T) {
	g := NewGomegaWithT(t)
	r, art := renderConfig(t, validNova())
	cm := renderedConfigMap(t, r, art.configMapName)

	for key, data := range cm.Data {
		g.Expect(data).NotTo(ContainSubstring("metadata_proxy_shared_secret"),
			"%s must not carry the value the metadata signature is verified with", key)
	}
	g.Expect(cm.Data[metadataConfDataKey]).To(Equal("[neutron]\nservice_metadata_proxy = true\n"))
	g.Expect(cm.Data[novaConfDataKey]).NotTo(ContainSubstring("\nhost = "),
		"each replica registers under its own pod name, so the file names no host")
}

// TestReconcileConfig_MessagingTLSRendersSSL covers the bus that verifies its
// broker. The workloads project the CA bundle at a fixed path, and unless ssl
// and ssl_ca_file name it every process speaks plaintext AMQP to a TLS listener
// and never joins the bus. A plaintext bus renders neither key.
func TestReconcileConfig_MessagingTLSRendersSSL(t *testing.T) {
	g := NewGomegaWithT(t)
	rabbit := sectionOf(t, renderNovaConf(t, novaWithMessagingTLS()), "oslo_messaging_rabbit")

	g.Expect(rabbit).To(ContainSubstring("ssl = true\n"))
	g.Expect(rabbit).To(ContainSubstring("ssl_ca_file = /etc/rabbitmq-ca/ca.crt\n"))

	g.Expect(sectionOf(t, renderNovaConf(t, validNova()), "oslo_messaging_rabbit")).
		NotTo(ContainSubstring("ssl"))
}

// TestReconcileConfig_OptionalDataKeys covers the file set: the five INI files
// ship on every render, and logging.ini only for a Nova that logs json.
func TestReconcileConfig_OptionalDataKeys(t *testing.T) {
	alwaysRendered := []string{
		conductorConfDataKey, metadataConfDataKey, novaConfDataKey,
		novncproxyConfDataKey, schedulerConfDataKey,
	}

	t.Run("text logging ships five files", func(t *testing.T) {
		g := NewGomegaWithT(t)
		r, art := renderConfig(t, novaMinimal())

		g.Expect(art.dataKeys).To(Equal(alwaysRendered))
		g.Expect(renderedConfigMap(t, r, art.configMapName).Data).NotTo(HaveKey(loggingINIDataKey))
	})

	t.Run("json logging ships logging.ini", func(t *testing.T) {
		g := NewGomegaWithT(t)
		nova := validNova()
		nova.Spec.Logging = &novav1alpha1.LoggingSpec{Format: "json", Level: "DEBUG"}
		r, art := renderConfig(t, nova)

		g.Expect(art.dataKeys).To(ContainElement(loggingINIDataKey))
		cm := renderedConfigMap(t, r, art.configMapName)
		g.Expect(cm.Data[loggingINIDataKey]).To(ContainSubstring("class = oslo_log.formatters.JSONFormatter"))
		g.Expect(cm.Data[loggingINIDataKey]).To(ContainSubstring("level = DEBUG"))
		g.Expect(cm.Data[novaConfDataKey]).To(
			ContainSubstring("log_config_append = /etc/nova/nova.conf.d/logging.ini"))
	})
}

// TestReconcileConfig_LoggingRendersDebugAndLoggerLevels covers the two logging
// knobs the level does not express. oslo.log gates its extra-verbose paths on
// debug alone, so a debug switch that never reaches the file leaves an operator
// chasing a fault without them; the per-logger levels render sorted, so the
// ConfigMap hash does not rotate with map iteration order.
func TestReconcileConfig_LoggingRendersDebugAndLoggerLevels(t *testing.T) {
	g := NewGomegaWithT(t)
	defaults := sectionOf(t, renderNovaConf(t, novaMaximal()), "DEFAULT")

	g.Expect(defaults).To(ContainSubstring("debug = true\n"))
	g.Expect(defaults).To(ContainSubstring("default_log_levels = amqp=WARNING,nova=DEBUG\n"))
}

// TestReconcileConfig_ControlCharKeepsLastGood covers the value that would inject
// further INI lines into a rendered section: nothing is re-rendered, the running
// pods keep the ConfigMap they mount, and the condition says the config failed.
func TestReconcileConfig_ControlCharKeepsLastGood(t *testing.T) {
	liveDeployment := func(nova *novav1alpha1.Nova) *appsv1.Deployment {
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: nova.Name, Namespace: nova.Namespace},
			Spec: appsv1.DeploymentSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Volumes: []corev1.Volume{{
							Name: configVolumeName,
							VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
								LocalObjectReference: corev1.LocalObjectReference{Name: "nova-config-old"},
								// The API projects its own subset, which never carries the
								// metadata overlay: the keys have to come from the ConfigMap.
								Items: []corev1.KeyToPath{
									{Key: novaConfDataKey, Path: novaConfDataKey},
								},
							}},
						}},
					},
				},
			},
		}
	}

	liveConfigMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "nova-config-old", Namespace: testNamespace},
		Data: map[string]string{
			novaConfDataKey:       "[DEFAULT]\n",
			metadataConfDataKey:   metadataOverlay,
			schedulerConfDataKey:  schedulerOverlay(novav1alpha1.DefaultWorkers),
			conductorConfDataKey:  conductorOverlay(novav1alpha1.DefaultWorkers),
			novncproxyConfDataKey: novncproxyOverlay,
		},
	}

	newNova := func() *novav1alpha1.Nova {
		nova := validNova()
		nova.Spec.ExtraConfig = map[string]map[string]string{
			"DEFAULT": {"instance_name_template": "value\n[evil]\nkey = injected"},
		}
		return nova
	}

	t.Run("the live Deployment's artefacts are kept", func(t *testing.T) {
		g := NewGomegaWithT(t)
		nova := newNova()
		r := newNovaTestReconciler(nova, liveDeployment(nova), liveConfigMap.DeepCopy())

		res, art, err := r.reconcileConfig(context.Background(), r.Client, nova)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.IsZero()).To(BeTrue())
		g.Expect(art.configMapName).To(Equal("nova-config-old"))
		// Every key of the live ConfigMap, not the API's projection: the metadata
		// Deployment still has to find metadata.conf in the recovered artefacts.
		g.Expect(art.dataKeys).To(Equal([]string{
			conductorConfDataKey, metadataConfDataKey, novaConfDataKey, novncproxyConfDataKey, schedulerConfDataKey,
		}))

		// No fresh ConfigMap was rendered next to the live one.
		var cms corev1.ConfigMapList
		g.Expect(r.List(context.Background(), &cms, client.InNamespace(testNamespace))).To(Succeed())
		g.Expect(cms.Items).To(HaveLen(1))

		cond := novaCondition(nova, "SecretsReady")
		g.Expect(cond).NotTo(BeNil())
		g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(cond.Reason).To(Equal(conditionReasonConfigError))
		g.Expect(cond.Message).To(ContainSubstring("[DEFAULT]"))
	})

	t.Run("a live ConfigMap that cannot be read is an error", func(t *testing.T) {
		g := NewGomegaWithT(t)
		nova := newNova()
		r := newNovaTestReconciler(nova, liveDeployment(nova))

		_, art, err := r.reconcileConfig(context.Background(), r.Client, nova)
		g.Expect(err).To(MatchError(ContainSubstring("fetching last-good ConfigMap")))
		g.Expect(art).To(Equal(configArtifacts{}))
	})

	// Only NotFound means first install. Any other read failure has to be an
	// error, or the pass would treat a running Nova as a first install and hand
	// the later steps no config to mount.
	t.Run("a live Deployment that cannot be read is an error", func(t *testing.T) {
		g := NewGomegaWithT(t)
		nova := newNova()
		boom := errors.New("deployments.apps is forbidden")
		c := novaFakeClientBuilder(nova, liveDeployment(nova), liveConfigMap.DeepCopy()).
			WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object,
					opts ...client.GetOption,
				) error {
					if _, isDeployment := obj.(*appsv1.Deployment); isDeployment {
						return boom
					}
					return cl.Get(ctx, key, obj, opts...)
				},
			}).
			Build()
		r := &NovaReconciler{Client: c, Scheme: testScheme(), Recorder: record.NewFakeRecorder(10)}

		_, art, err := r.reconcileConfig(context.Background(), r.Client, nova)
		g.Expect(err).To(MatchError(boom))
		g.Expect(err).To(MatchError(ContainSubstring("fetching Deployment")))
		g.Expect(art).To(Equal(configArtifacts{}))
	})

	t.Run("first install has nothing to keep", func(t *testing.T) {
		g := NewGomegaWithT(t)
		nova := newNova()
		r := newNovaTestReconciler(nova)

		_, art, err := r.reconcileConfig(context.Background(), r.Client, nova)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(art.configMapName).To(BeEmpty())
		g.Expect(art.dataKeys).To(BeEmpty())
	})
}

// TestReconcileConfig_ConfigMapFailuresMarkSecretsReady covers the two
// ConfigMap calls the API server can refuse: the create, and the listing the
// prune reads the history from. Each has to flip SecretsReady=False with reason
// ConfigError, so the aggregate Ready cannot stay True at a generation whose
// config never landed, hand the later steps no artefacts, and keep the client
// error unwrappable.
func TestReconcileConfig_ConfigMapFailuresMarkSecretsReady(t *testing.T) {
	boom := errors.New("admission webhook rejected the request")

	cases := []struct {
		name    string
		funcs   interceptor.Funcs
		wrapped string
	}{
		{
			name: "the ConfigMap create",
			funcs: interceptor.Funcs{
				Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					if _, isConfigMap := obj.(*corev1.ConfigMap); isConfigMap {
						return boom
					}
					return cl.Create(ctx, obj, opts...)
				},
			},
			wrapped: "creating config ConfigMap:",
		},
		{
			name: "the prune listing",
			funcs: interceptor.Funcs{
				List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if _, isConfigMapList := list.(*corev1.ConfigMapList); isConfigMapList {
						return boom
					}
					return cl.List(ctx, list, opts...)
				},
			},
			wrapped: "pruning config ConfigMaps:",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			nova := validNova()
			c := novaFakeClientBuilder(nova).WithInterceptorFuncs(tc.funcs).Build()
			r := &NovaReconciler{Client: c, Scheme: testScheme(), Recorder: record.NewFakeRecorder(10)}

			res, art, err := r.reconcileConfig(context.Background(), r.Client, nova)

			g.Expect(err).To(MatchError(boom))
			g.Expect(err).To(MatchError(ContainSubstring(tc.wrapped)))
			g.Expect(res.IsZero()).To(BeTrue())
			g.Expect(art).To(Equal(configArtifacts{}))

			cond := novaCondition(nova, "SecretsReady")
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(conditionReasonConfigError))
			g.Expect(cond.Message).To(ContainSubstring(boom.Error()))
		})
	}
}

// TestReconcileConfig_ExtraConfigOverlay covers the escape hatch: nil and empty
// blocks render the defaults byte-identically, and a value under a reported key
// wins over the operator's.
func TestReconcileConfig_ExtraConfigOverlay(t *testing.T) {
	t.Run("nil and empty render identically", func(t *testing.T) {
		g := NewGomegaWithT(t)
		nilNova := validNova()
		nilNova.Spec.ExtraConfig = nil
		emptyNova := validNova()
		emptyNova.Spec.ExtraConfig = map[string]map[string]string{}

		_, nilArt := renderConfig(t, nilNova)
		_, emptyArt := renderConfig(t, emptyNova)
		g.Expect(emptyArt.configMapName).To(Equal(nilArt.configMapName),
			"the ConfigMap name is the content hash, so an empty block must not rotate it")
	})

	t.Run("a user value wins over the operator default", func(t *testing.T) {
		g := NewGomegaWithT(t)
		nova := validNova()
		nova.Spec.ExtraConfig = map[string]map[string]string{
			"DEFAULT":          {"state_path": "/srv/nova"},
			"filter_scheduler": {"host_subset_size": "3"},
		}
		conf := renderNovaConf(t, nova)

		g.Expect(conf).To(ContainSubstring("state_path = /srv/nova"))
		g.Expect(conf).NotTo(ContainSubstring("state_path = /var/lib/nova\n"))
		g.Expect(conf).To(ContainSubstring("[filter_scheduler]"))

		// The guard reports the take-over; it does not reject it.
		cond := novaCondition(nova, "ExtraConfigHealthy")
		g.Expect(cond).NotTo(BeNil())
		g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	})
}

// TestReconcileConfig_PrunesToRetainCount pins the rollback depth: three
// historical ConfigMaps survive beside the current one.
func TestReconcileConfig_PrunesToRetainCount(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	baseName := nova.Name + "-config"

	stale := make([]client.Object, 0, 5)
	for i := range 5 {
		stale = append(stale, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("%s-stale%d", baseName, i),
				Namespace: testNamespace,
				Labels:    map[string]string{config.ConfigBaseLabelKey: baseName},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: novav1alpha1.GroupVersion.String(),
					Kind:       "Nova",
					Name:       nova.Name,
					UID:        nova.UID,
					Controller: ptr.To(true),
				}},
			},
		})
	}
	r, art := renderConfig(t, nova, stale...)

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

// TestReconcileConfig_WorkersDefaultWhenNil covers the two worker counts on a CR
// that bypassed the defaulting webhook: the overlays carry the default rather
// than a zero that would leave the process with no worker at all.
func TestReconcileConfig_WorkersDefaultWhenNil(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := novaMinimal()
	g.Expect(nova.Spec.Scheduler.Workers).To(BeNil())
	g.Expect(nova.Spec.Conductor.Workers).To(BeNil())

	r, art := renderConfig(t, nova)
	cm := renderedConfigMap(t, r, art.configMapName)

	g.Expect(cm.Data[schedulerConfDataKey]).
		To(Equal(fmt.Sprintf("[scheduler]\nworkers = %d\n", novav1alpha1.DefaultWorkers)))
	g.Expect(cm.Data[conductorConfDataKey]).
		To(Equal(fmt.Sprintf("[conductor]\nworkers = %d\n", novav1alpha1.DefaultWorkers)))
}

// TestReconcileConfig_WorkersFollowTheSpec covers the two worker counts a CR
// sets. The conductor answers every database call the computes make and the
// scheduler every placement decision, so the two are sized apart, and an
// overlay carrying the other block's count or the default starves whichever one
// was raised. The counts also have to change the ConfigMap name, or the edit
// never rolls the pods that read them.
func TestReconcileConfig_WorkersFollowTheSpec(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	nova.Spec.Scheduler.Workers = ptr.To(int32(5))
	nova.Spec.Conductor.Workers = ptr.To(int32(3))

	r, art := renderConfig(t, nova)
	cm := renderedConfigMap(t, r, art.configMapName)

	g.Expect(cm.Data[schedulerConfDataKey]).To(Equal("[scheduler]\nworkers = 5\n"))
	g.Expect(cm.Data[conductorConfDataKey]).To(Equal("[conductor]\nworkers = 3\n"))

	_, defaultArt := renderConfig(t, validNova())
	g.Expect(art.configMapName).NotTo(Equal(defaultArt.configMapName))
}

// parseOverlaySections parses a rendered overlay back into its INI sections, so
// the drift guard reads the overlays the same way it reads operatorDefaults.
// The overlays are operator-built documents of "[section]" headers and
// "key = value" lines, which is the whole grammar this needs to cover.
func parseOverlaySections(t *testing.T, overlay string) map[string]map[string]string {
	t.Helper()

	sections := map[string]map[string]string{}
	current := ""
	for _, line := range strings.Split(overlay, "\n") {
		switch {
		case line == "":
		case strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"):
			current = strings.Trim(line, "[]")
			sections[current] = map[string]string{}
		default:
			key, value, ok := strings.Cut(line, " = ")
			if !ok || current == "" {
				t.Fatalf("overlay line %q is neither a section header nor a key", line)
			}
			sections[current][key] = value
		}
	}
	return sections
}

// TestOperatorDefaults_RegistryDriftGuard asserts that every key the renderer
// writes, in the shared document and in the four overlays alike, is registered
// as operator-owned and lives in a section the embedded option catalogs are
// checked against. The reverse direction is deliberately not asserted: the
// registry is static by contract, so it also carries the keys a conditional
// render only sometimes emits and the credential keys the renderer never writes
// at all.
func TestOperatorDefaults_RegistryDriftGuard(t *testing.T) {
	g := NewGomegaWithT(t)

	rendered := map[string]map[string]map[string]string{
		// The maximal CR, so the forward check covers the conditional keys too.
		"novaMaximal": operatorDefaults(novaMaximal()),
		"novaMinimal": operatorDefaults(novaMinimal()),
	}
	overlays := map[string]string{
		metadataConfDataKey:   metadataOverlay,
		schedulerConfDataKey:  schedulerOverlay(novav1alpha1.DefaultWorkers),
		conductorConfDataKey:  conductorOverlay(novav1alpha1.DefaultWorkers),
		novncproxyConfDataKey: novncproxyOverlay,
	}
	for name, overlay := range overlays {
		sections := parseOverlaySections(t, overlay)
		rendered[name] = sections
		for section, options := range sections {
			g.Expect(config.CheckNoControlChars(section, options)).To(Succeed(),
				"%s must stay a document of single-line options", name)
		}
	}

	registered := make(map[[2]string]struct{}, len(novav1alpha1.OwnedConfigKeys))
	for _, owned := range novav1alpha1.OwnedConfigKeys {
		registered[[2]string{owned.Section, owned.Key}] = struct{}{}
	}
	for fixture, sections := range rendered {
		for section, kvs := range sections {
			if !slices.Contains(novav1alpha1.RenderedSections, section) {
				t.Errorf("%s renders section [%s] outside novav1alpha1.RenderedSections, so an "+
					"override aimed at it is never scanned against the option catalog", fixture, section)
			}
			for key := range kvs {
				if _, ok := registered[[2]string{section, key}]; ok {
					continue
				}
				t.Errorf("%s renders unregistered key [%s] %s: add it to "+
					"novav1alpha1.OwnedConfigKeys", fixture, section, key)
			}
		}
	}
}
