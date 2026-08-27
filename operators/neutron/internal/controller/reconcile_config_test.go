// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/c5c3/cobaltcore/internal/common/config"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
)

// neutronForConfig returns the Neutron the config tests render, carrying a
// region so the optional [keystone_authtoken] keys are exercised.
func neutronForConfig() *neutronv1alpha1.Neutron {
	neutron := validNeutron()
	neutron.Spec.Region = "RegionOne"
	return neutron
}

// resolvedForConfig is the OVN projection the config step consumes, as the
// endpoint step resolves it inside one cluster.
func resolvedForConfig() resolvedOVNEndpoints {
	return resolvedOVNEndpoints{
		nbAddress: testNorthboundAddress,
		sbAddress: testSouthboundAddress,
		central:   ovnCentralFixture(),
	}
}

// renderConfig runs the config step over a fresh reconciler and returns both,
// so a test reads the artefact the pass wrote.
func renderConfig(t *testing.T, neutron *neutronv1alpha1.Neutron, objs ...client.Object) (*NeutronReconciler, string) {
	t.Helper()

	r := newNeutronTestReconciler(append([]client.Object{neutron}, objs...)...)
	_, name, err := r.reconcileConfig(context.Background(), r.Client, neutron, resolvedForConfig())
	if err != nil {
		t.Fatalf("rendering the config ConfigMap: %v", err)
	}
	return r, name
}

// renderedConfigMap re-reads the ConfigMap the config step created.
func renderedConfigMap(t *testing.T, r *NeutronReconciler, name string) *corev1.ConfigMap {
	t.Helper()

	var cm corev1.ConfigMap
	key := client.ObjectKey{Namespace: testNamespace, Name: name}
	if err := r.Get(context.Background(), key, &cm); err != nil {
		t.Fatalf("re-reading config ConfigMap %s: %v", name, err)
	}
	return &cm
}

// iniSections returns the section names of a rendered INI file, in file order.
func iniSections(rendered string) []string {
	var names []string
	for _, line := range strings.Split(rendered, "\n") {
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			names = append(names, strings.TrimSuffix(strings.TrimPrefix(line, "["), "]"))
		}
	}
	return names
}

// TestReconcileConfig_RendersTheConfigFiles pins the artefact the pods mount:
// the data keys, the immutability that forces a new name instead of an in-place
// edit the kubelet would propagate mid-request, and the keys each file carries.
func TestReconcileConfig_RendersTheConfigFiles(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := neutronForConfig()
	r, name := renderConfig(t, neutron)

	cm := renderedConfigMap(t, r, name)
	g.Expect(name).To(HavePrefix("neutron-config-"))
	g.Expect(cm.Immutable).NotTo(BeNil())
	g.Expect(*cm.Immutable).To(BeTrue())
	// logging.conf joins the set only for json logging, which the fixture does
	// not ask for (see TestReconcileConfig_JSONLoggingShipsLoggingConf).
	g.Expect(cm.Data).To(HaveLen(3))
	g.Expect(cm.Data).To(HaveKey(neutronConfDataKey))
	g.Expect(cm.Data).To(HaveKey(ml2ConfDataKey))
	g.Expect(cm.Data).To(HaveKey(uwsgiConfDataKey))

	// uWSGI expands its own magic variable while it reads the file, so the "%t"
	// reaches the ConfigMap verbatim.
	g.Expect(cm.Data[uwsgiConfDataKey]).To(Equal("[uwsgi]\nstart-time = %t\n"))

	conf := cm.Data[neutronConfDataKey]
	g.Expect(conf).To(ContainSubstring("core_plugin = ml2"))
	g.Expect(conf).To(ContainSubstring("service_plugins = ovn-router"))
	g.Expect(conf).To(ContainSubstring("auth_strategy = keystone"))
	g.Expect(conf).To(ContainSubstring("api_paste_config = " + apiPasteConfigPath))
	g.Expect(conf).To(ContainSubstring("state_path = " + neutronStatePath))
	// The API pods serve HTTP alone; the RPC side runs in the worker Deployment.
	g.Expect(conf).To(ContainSubstring("rpc_workers = 0"))
	g.Expect(conf).To(ContainSubstring("rpc_state_report_workers = 0"))
	g.Expect(conf).To(ContainSubstring("dhcp_agent_notification = false"))
	g.Expect(conf).To(ContainSubstring("notify_nova_on_port_status_changes = false"))
	g.Expect(conf).To(ContainSubstring("notify_nova_on_port_data_changes = false"))
	g.Expect(conf).To(ContainSubstring("[oslo_messaging_notifications]"))
	g.Expect(conf).To(ContainSubstring("driver = noop"))
	g.Expect(conf).To(ContainSubstring("lock_path = " + neutronStatePath + "/lock"))
	// The database URL is a placeholder the OS_DATABASE__CONNECTION env override
	// replaces at runtime.
	g.Expect(conf).To(ContainSubstring("[database]"))
	g.Expect(conf).To(ContainSubstring("connection = " + dbConnectionPlaceholder))
	// [keystone_authtoken]: identity + region + memcached, and NO password. The
	// service password reaches the pod through OS_KEYSTONE_AUTHTOKEN__PASSWORD
	// only; a file value would leak it into a namespace-readable ConfigMap. The
	// auth_type = password line is the middleware's plugin name, not a
	// credential, so the assertion anchors on the key.
	g.Expect(conf).To(ContainSubstring("[keystone_authtoken]"))
	g.Expect(conf).To(ContainSubstring("auth_url = http://keystone.openstack.svc:5000"))
	g.Expect(conf).To(ContainSubstring("www_authenticate_uri = http://keystone.openstack.svc:5000"))
	g.Expect(conf).To(ContainSubstring("username = neutron"))
	g.Expect(conf).To(ContainSubstring("region_name = RegionOne"))
	g.Expect(conf).To(ContainSubstring("memcached_servers = mc:11211"))
	g.Expect(conf).NotTo(ContainSubstring("password ="))
	// The three quorum options, and no transport_url: the URL arrives through
	// the OS_DEFAULT__TRANSPORT_URL env override, so the broker password stays
	// out of the ConfigMap.
	g.Expect(conf).To(ContainSubstring("[oslo_messaging_rabbit]"))
	g.Expect(conf).To(ContainSubstring("rabbit_quorum_queue = true"))
	g.Expect(conf).To(ContainSubstring("rabbit_transient_quorum_queue = true"))
	g.Expect(conf).To(ContainSubstring("use_queue_manager = true"))
	g.Expect(conf).NotTo(ContainSubstring("transport_url"))
	// No TLS block on the fixture, so no client trust is configured.
	g.Expect(conf).NotTo(ContainSubstring("ssl = true"))

	ml2 := cm.Data[ml2ConfDataKey]
	g.Expect(ml2).To(ContainSubstring("mechanism_drivers = ovn"))
	g.Expect(ml2).To(ContainSubstring("type_drivers = geneve,flat"))
	g.Expect(ml2).To(ContainSubstring("tenant_network_types = geneve"))
	g.Expect(ml2).To(ContainSubstring("extension_drivers = port_security"))
	g.Expect(ml2).To(ContainSubstring("vni_ranges = 1:65536"))
	g.Expect(ml2).To(ContainSubstring("max_header_size = 38"))
	g.Expect(ml2).To(ContainSubstring("flat_networks = *"))
	g.Expect(ml2).To(ContainSubstring("enable_security_group = true"))
	// The two connections are the addresses the endpoint step resolved, and the
	// six file keys point at the mirrored client identity.
	g.Expect(ml2).To(ContainSubstring("ovn_nb_connection = " + testNorthboundAddress))
	g.Expect(ml2).To(ContainSubstring("ovn_sb_connection = " + testSouthboundAddress))
	for _, line := range []string{
		"ovn_nb_private_key = " + ovnTLSMountPath + "/tls.key",
		"ovn_nb_certificate = " + ovnTLSMountPath + "/tls.crt",
		"ovn_nb_ca_cert = " + ovnTLSMountPath + "/ca.crt",
		"ovn_sb_private_key = " + ovnTLSMountPath + "/tls.key",
		"ovn_sb_certificate = " + ovnTLSMountPath + "/tls.crt",
		"ovn_sb_ca_cert = " + ovnTLSMountPath + "/ca.crt",
	} {
		g.Expect(ml2).To(ContainSubstring(line))
	}
	g.Expect(ml2).To(ContainSubstring("ovn_l3_scheduler = leastloaded"))
	g.Expect(ml2).To(ContainSubstring("ovn_metadata_enabled = true"))
}

// TestReconcileConfig_ML2SectionsRouteToTheirFile pins the split: every section
// ml2_conf.ini carries is an ml2Sections member, and neutron.conf carries none
// of them. The twelve names are the routing table, not a rendered set: a member
// with no key renders nowhere, which is why the defaults produce five of them.
func TestReconcileConfig_ML2SectionsRouteToTheirFile(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := neutronForConfig()
	r, name := renderConfig(t, neutron)
	cm := renderedConfigMap(t, r, name)

	ml2 := iniSections(cm.Data[ml2ConfDataKey])
	g.Expect(ml2).To(ConsistOf("ml2", "ml2_type_flat", "ml2_type_geneve", "ovn", "securitygroup"))
	for _, section := range ml2 {
		g.Expect(ml2Sections).To(HaveKey(section))
	}
	for _, section := range iniSections(cm.Data[neutronConfDataKey]) {
		g.Expect(ml2Sections).NotTo(HaveKey(section),
			"a plugin section must not be rendered into neutron.conf")
	}
	// The remaining seven names carry no operator default and are routed all the
	// same, which is what the extraConfig case below proves.
	g.Expect(ml2Sections).To(HaveLen(12))
}

// TestReconcileConfig_ExtraConfigRoutesByItsSection covers the escape hatch on
// both sides of the split: a plugin option lands in ml2_conf.ini, a
// neutron-server option in neutron.conf, and neither leaks into the other file.
func TestReconcileConfig_ExtraConfigRoutesByItsSection(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := neutronForConfig()
	neutron.Spec.ExtraConfig = map[string]map[string]string{
		"ml2_type_vlan": {"network_vlan_ranges": "physnet1:100:200"},
		"quotas":        {"quota_network": "50"},
	}
	r, name := renderConfig(t, neutron)
	cm := renderedConfigMap(t, r, name)

	ml2 := cm.Data[ml2ConfDataKey]
	conf := cm.Data[neutronConfDataKey]
	g.Expect(ml2).To(ContainSubstring("[ml2_type_vlan]"))
	g.Expect(ml2).To(ContainSubstring("network_vlan_ranges = physnet1:100:200"))
	g.Expect(conf).NotTo(ContainSubstring("network_vlan_ranges"))
	g.Expect(conf).To(ContainSubstring("[quotas]"))
	g.Expect(conf).To(ContainSubstring("quota_network = 50"))
	g.Expect(ml2).NotTo(ContainSubstring("quota_network"))
}

// TestReconcileConfig_NilAndEmptyExtraConfigRenderIdentically pins the
// nil-versus-empty contract: an empty spec.extraConfig map takes the merge path
// while nil skips it, and both must produce the same bytes — otherwise adding
// and removing the last override would rotate the ConfigMap and roll the pods
// for a spec that renders the same files.
func TestReconcileConfig_NilAndEmptyExtraConfigRenderIdentically(t *testing.T) {
	g := NewGomegaWithT(t)

	nilCfg := neutronForConfig()
	nilCfg.Spec.ExtraConfig = nil
	rNil, nilName := renderConfig(t, nilCfg)

	emptyCfg := neutronForConfig()
	emptyCfg.Spec.ExtraConfig = map[string]map[string]string{}
	rEmpty, emptyName := renderConfig(t, emptyCfg)

	g.Expect(emptyName).To(Equal(nilName), "an empty extraConfig must not rotate the ConfigMap")
	g.Expect(renderedConfigMap(t, rEmpty, emptyName).Data).
		To(Equal(renderedConfigMap(t, rNil, nilName).Data))
}

// TestReconcileConfig_JSONLoggingShipsLoggingConf covers the fourth data key:
// format=json points oslo.log at a fileConfig the same ConfigMap carries, and
// the per-logger levels render into the oslo.log CSV.
func TestReconcileConfig_JSONLoggingShipsLoggingConf(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := neutronForConfig()
	neutron.Spec.Logging = &neutronv1alpha1.LoggingSpec{
		Format:          "json",
		Level:           "DEBUG",
		Debug:           ptr.To(true),
		PerLoggerLevels: map[string]string{"sqlalchemy.engine": "WARNING", "neutron": "DEBUG"},
	}
	r, name := renderConfig(t, neutron)
	cm := renderedConfigMap(t, r, name)

	g.Expect(cm.Data).To(HaveLen(4))
	g.Expect(cm.Data).To(HaveKey(loggingConfDataKey))
	g.Expect(cm.Data[loggingConfDataKey]).To(ContainSubstring("class = oslo_log.formatters.JSONFormatter"))
	g.Expect(cm.Data[loggingConfDataKey]).To(ContainSubstring("level = DEBUG"))

	conf := cm.Data[neutronConfDataKey]
	g.Expect(conf).To(ContainSubstring("log_config_append = " + loggingConfFilePath))
	g.Expect(conf).To(ContainSubstring("debug = true"))
	// Sorted keys: the value feeds the ConfigMap content hash, so map iteration
	// order must not reach it.
	g.Expect(conf).To(ContainSubstring("default_log_levels = neutron=DEBUG,sqlalchemy.engine=WARNING"))
}

// TestReconcileConfig_MessagingTLSRendersTheClientTrust covers the TLS block:
// its presence alone enables the client trust, pointing oslo.messaging at the CA
// bundle the deployment step mounts.
func TestReconcileConfig_MessagingTLSRendersTheClientTrust(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := neutronForConfig()
	neutron.Spec.Messaging.TLS = &commonv1.MessagingTLSSpec{
		CABundleSecretRef: commonv1.SecretRefSpec{Name: "rabbitmq-ca", Key: "ca.crt"},
	}
	r, name := renderConfig(t, neutron)

	conf := renderedConfigMap(t, r, name).Data[neutronConfDataKey]
	g.Expect(conf).To(ContainSubstring("ssl = true"))
	g.Expect(conf).To(ContainSubstring("ssl_ca_file = " + rabbitmqCAFilePath))
	g.Expect(rabbitmqCAFilePath).To(Equal("/etc/rabbitmq-ca/ca.crt"))
}

// TestReconcileConfig_OwnedKeyOverrideReported verifies the report-only
// contract: overriding an operator-owned key in spec.extraConfig sets
// ExtraConfigHealthy=False, emits a Warning event, AND still honours the
// override in the rendered file (the guard reports, it does not reject).
func TestReconcileConfig_OwnedKeyOverrideReported(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := neutronForConfig()
	neutron.Spec.ExtraConfig = map[string]map[string]string{
		"ml2": {"mechanism_drivers": "ovn,sriovnicswitch"},
	}
	r, name := renderConfig(t, neutron)

	cond := meta.FindStatusCondition(neutron.Status.Conditions, config.ConditionTypeExtraConfigHealthy)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(config.ConditionReasonOwnedKeysOverridden))
	g.Expect(cond.Message).To(ContainSubstring("[ml2] mechanism_drivers"))

	events := collectEvents(r.Recorder.(*record.FakeRecorder))
	g.Expect(events).To(ContainElement(And(
		ContainSubstring("Warning"),
		ContainSubstring(config.EventReasonExtraConfigOwnedKeyOverride),
		ContainSubstring("[ml2] mechanism_drivers"),
	)))

	// Report-only: the override is honoured in the rendered file.
	ml2 := renderedConfigMap(t, r, name).Data[ml2ConfDataKey]
	g.Expect(ml2).To(ContainSubstring("mechanism_drivers = ovn,sriovnicswitch"))
}

// TestReconcileConfig_ExtraConfigHealthyTrueOnUnownedKey verifies that a CR
// whose spec.extraConfig overrides no operator-owned key sets
// ExtraConfigHealthy=True and emits no event.
func TestReconcileConfig_ExtraConfigHealthyTrueOnUnownedKey(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := neutronForConfig()
	neutron.Spec.ExtraConfig = map[string]map[string]string{
		"quotas": {"quota_port": "100"},
	}
	r, name := renderConfig(t, neutron)

	cond := meta.FindStatusCondition(neutron.Status.Conditions, config.ConditionTypeExtraConfigHealthy)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(config.ConditionReasonNoOwnedKeysOverridden))
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).To(BeEmpty())
	g.Expect(renderedConfigMap(t, r, name).Data[neutronConfDataKey]).To(ContainSubstring("quota_port = 100"))
}

// TestReconcileConfig_SpecChangeRotatesConfigMap verifies the content-hashed
// name reaches the running pods: a changed OVN address must produce a new
// ConfigMap name for the deployment step to roll.
func TestReconcileConfig_SpecChangeRotatesConfigMap(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := neutronForConfig()
	r, before := renderConfig(t, neutron)

	moved := resolvedForConfig()
	moved.nbAddress = "ssl:10.96.0.99:6641"
	_, after, err := r.reconcileConfig(context.Background(), r.Client, neutron, moved)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(after).NotTo(Equal(before), "a moved endpoint must rotate the content-hashed ConfigMap")
	g.Expect(renderedConfigMap(t, r, after).Data[ml2ConfDataKey]).
		To(ContainSubstring("ovn_nb_connection = ssl:10.96.0.99:6641"))
}

// TestReconcileConfig_PrunesToRetainCount pins the rollback depth: three
// historical ConfigMaps survive beside the current one.
func TestReconcileConfig_PrunesToRetainCount(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := neutronForConfig()

	stale := make([]client.Object, 0, 5)
	for i := range 5 {
		stale = append(stale, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("%s-config-stale%d", neutron.Name, i),
				Namespace: testNamespace,
				Labels:    map[string]string{config.ConfigBaseLabelKey: neutron.Name + "-config"},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: neutronv1alpha1.GroupVersion.String(),
					Kind:       "Neutron",
					Name:       neutron.Name,
					UID:        neutron.UID,
					Controller: ptr.To(true),
				}},
			},
		})
	}
	r, current := renderConfig(t, neutron, stale...)

	var configMaps corev1.ConfigMapList
	g.Expect(r.List(context.Background(), &configMaps, client.InNamespace(testNamespace),
		client.MatchingLabels{config.ConfigBaseLabelKey: neutron.Name + "-config"})).To(Succeed())

	names := make([]string, 0, len(configMaps.Items))
	for i := range configMaps.Items {
		names = append(names, configMaps.Items[i].Name)
	}
	g.Expect(names).To(HaveLen(defaultConfigMapRetainCount+1),
		"the current ConfigMap plus %d historical ones", defaultConfigMapRetainCount)
	g.Expect(names).To(ContainElement(current))
}

// TestReconcileConfig_ApplyFailureMarksSecretsReady covers the write path: a
// ConfigMap Create the API server refuses must surface as SecretsReady=False
// with reason ConfigError, so the aggregate Ready cannot stay stale-True at the
// new generation, and the client error must stay unwrappable.
func TestReconcileConfig_ApplyFailureMarksSecretsReady(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := neutronForConfig()

	boom := errors.New("admission webhook rejected the ConfigMap")
	c := neutronFakeClientBuilder(neutron).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, isConfigMap := obj.(*corev1.ConfigMap); isConfigMap {
					return boom
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).
		Build()
	r := &NeutronReconciler{Client: c, Scheme: testScheme(), Recorder: record.NewFakeRecorder(10)}

	res, name, err := r.reconcileConfig(context.Background(), r.Client, neutron, resolvedForConfig())

	g.Expect(err).To(MatchError(boom))
	g.Expect(err).To(MatchError(ContainSubstring("creating config ConfigMap:")))
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(name).To(BeEmpty())

	cond := neutronCondition(neutron, "SecretsReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonConfigError))
	g.Expect(cond.Message).To(ContainSubstring(boom.Error()))
}

// TestOperatorDefaults_RegistryDriftGuard is the completeness check tying
// operatorDefaults to neutronv1alpha1.OwnedConfigKeys: every key the operator
// renders must be registered (forward) and every registered key must be
// rendered (reverse), so the registry and the renderer cannot drift apart.
func TestOperatorDefaults_RegistryDriftGuard(t *testing.T) {
	neutron := neutronForConfig()
	// neutronForConfig already sets Region (emits region_name) and validNeutron
	// sets Cache.Servers (emits memcached_servers). Add the broker TLS block so
	// [oslo_messaging_rabbit] ssl and ssl_ca_file render, and json logging with
	// per-logger levels so log_config_append and default_log_levels render too.
	neutron.Spec.Messaging.TLS = &commonv1.MessagingTLSSpec{
		CABundleSecretRef: commonv1.SecretRefSpec{Name: "rabbitmq-ca", Key: "ca.crt"},
	}
	neutron.Spec.Logging = &neutronv1alpha1.LoggingSpec{
		Format:          "json",
		Debug:           ptr.To(true),
		PerLoggerLevels: map[string]string{"neutron": "DEBUG"},
	}

	defaults := operatorDefaults(neutron, resolvedForConfig())

	registered := make(map[[2]string]struct{}, len(neutronv1alpha1.OwnedConfigKeys))
	for _, o := range neutronv1alpha1.OwnedConfigKeys {
		registered[[2]string{o.Section, o.Key}] = struct{}{}
	}

	// Forward check: every rendered (section, key) is registered — no exceptions.
	for section, kvs := range defaults {
		for key := range kvs {
			if _, ok := registered[[2]string{section, key}]; ok {
				continue
			}
			t.Errorf("operatorDefaults renders unregistered key [%s] %s: add it to "+
				"neutronv1alpha1.OwnedConfigKeys", section, key)
		}
	}

	// Reverse check: every registered key is rendered by operatorDefaults or on
	// the extras list. Both extras are credential material the renderer never
	// emits — [DEFAULT] transport_url and [keystone_authtoken] password arrive
	// through their env overrides — and both are registered because a user
	// putting them in spec.extraConfig would copy the credential into the
	// rendered config.
	reverseExtras := map[[2]string]struct{}{
		{"DEFAULT", "transport_url"}:       {},
		{"keystone_authtoken", "password"}: {},
	}
	for _, o := range neutronv1alpha1.OwnedConfigKeys {
		if _, ok := defaults[o.Section][o.Key]; ok {
			continue
		}
		if _, ok := reverseExtras[[2]string{o.Section, o.Key}]; ok {
			continue
		}
		t.Errorf("registry key [%s] %s is not rendered by operatorDefaults: remove it "+
			"from neutronv1alpha1.OwnedConfigKeys or extend the drift-guard extras list", o.Section, o.Key)
	}
}

// TestEffectiveLogging pins the render-time resolution of spec.logging: a CR
// that bypassed the defaulting webhook still renders the production baseline,
// and the resolver hands back a copy so nothing is written into the CR.
func TestEffectiveLogging(t *testing.T) {
	g := NewGomegaWithT(t)

	fromNil := effectiveLogging(nil)
	g.Expect(fromNil.Format).To(Equal("text"))
	g.Expect(fromNil.Level).To(Equal("INFO"))
	g.Expect(fromNil.Debug).NotTo(BeNil())
	g.Expect(*fromNil.Debug).To(BeFalse())

	// A half-filled block keeps what it sets and defaults the rest.
	partial := &neutronv1alpha1.LoggingSpec{Level: "WARNING"}
	resolved := effectiveLogging(partial)
	g.Expect(resolved.Format).To(Equal("text"))
	g.Expect(resolved.Level).To(Equal("WARNING"))
	g.Expect(partial.Format).To(BeEmpty(), "the input spec must be left untouched")
	g.Expect(partial.Debug).To(BeNil(), "the input spec must be left untouched")
}
