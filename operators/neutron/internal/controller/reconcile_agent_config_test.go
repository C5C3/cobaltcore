// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/c5c3/cobaltcore/internal/common/config"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
)

// resolvedForAgentConfig is the chassis projection the config step consumes, as
// the chassis step resolves it.
func resolvedForAgentConfig() resolvedChassis {
	return resolvedChassis{
		nodeSelector:     map[string]string{testChassisNodeLabel: "true"},
		sbAddress:        testSouthboundAddress,
		clientSecretName: "ovn-client",
	}
}

// renderAgentConfig runs the config step over a fresh reconciler and returns
// both, so a test reads the artefact the pass wrote.
func renderAgentConfig(t *testing.T, cr *neutronv1alpha1.NeutronMetadataAgent,
	objs ...client.Object,
) (*NeutronMetadataAgentReconciler, string) {
	t.Helper()

	r := newAgentTestReconciler(append([]client.Object{cr}, objs...)...)
	_, name, err := r.reconcileAgentConfig(context.Background(), r.Client, cr, resolvedForAgentConfig())
	if err != nil {
		t.Fatalf("rendering the agent config ConfigMap: %v", err)
	}
	return r, name
}

// renderedAgentConfigMap re-reads the ConfigMap the agent config step created.
func renderedAgentConfigMap(t *testing.T, r *NeutronMetadataAgentReconciler, name string) *corev1.ConfigMap {
	t.Helper()

	var cm corev1.ConfigMap
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: name}, &cm); err != nil {
		t.Fatalf("re-reading agent config ConfigMap %s: %v", name, err)
	}
	return &cm
}

// TestReconcileAgentConfig_RendersTheAgentFile pins the artefact the pods mount:
// the two databases the agent reads, the client identity it presents to the
// Southbound one, and the writable paths the operator backs with volumes.
func TestReconcileAgentConfig_RendersTheAgentFile(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := validAgent()
	r, name := renderAgentConfig(t, cr)

	cm := renderedAgentConfigMap(t, r, name)
	g.Expect(name).To(HavePrefix(testAgentName + "-config-"))
	g.Expect(cm.Immutable).NotTo(BeNil())
	g.Expect(*cm.Immutable).To(BeTrue())
	// logging.conf joins the set only for json logging, which the fixture does
	// not ask for.
	g.Expect(cm.Data).To(HaveLen(1))

	conf := cm.Data[metadataAgentConfigFile]
	g.Expect(conf).To(ContainSubstring("[ovs]"))
	g.Expect(conf).To(ContainSubstring("ovsdb_connection = unix:/run/openvswitch/db.sock"))
	g.Expect(conf).To(ContainSubstring("[ovn]"))
	g.Expect(conf).To(ContainSubstring("ovn_sb_connection = " + testSouthboundAddress))
	g.Expect(conf).To(ContainSubstring("ovn_sb_private_key = " + ovnClientKeyPath))
	g.Expect(conf).To(ContainSubstring("ovn_sb_certificate = " + ovnClientCertPath))
	g.Expect(conf).To(ContainSubstring("ovn_sb_ca_cert = " + ovnClientCAPath))
	g.Expect(conf).To(ContainSubstring("state_path = " + neutronStatePath))
	g.Expect(conf).To(ContainSubstring("lock_path = " + neutronStatePath + "/lock"))
	g.Expect(conf).To(ContainSubstring("[oslo_messaging_notifications]"))
	g.Expect(conf).To(ContainSubstring("driver = noop"))
	g.Expect(conf).To(ContainSubstring("debug = false"))
}

// The Southbound address is the one the chassis step resolved off the central,
// so a control plane that republishes it rotates the content-hashed ConfigMap
// rather than editing the mounted file under a running pod.
func TestReconcileAgentConfig_SouthboundAddressFollowsTheCentral(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := validAgent()
	r, before := renderAgentConfig(t, cr)

	moved := resolvedForAgentConfig()
	moved.sbAddress = "ssl:10.96.0.99:6642"
	_, after, err := r.reconcileAgentConfig(context.Background(), r.Client, cr, moved)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(after).NotTo(Equal(before), "a moved address must rotate the content-hashed ConfigMap")
	g.Expect(renderedAgentConfigMap(t, r, after).Data[metadataAgentConfigFile]).
		To(ContainSubstring("ovn_sb_connection = ssl:10.96.0.99:6642"))
}

// Three groups of keys must never reach the file: the shared secret, which is
// env-injected and would otherwise be copied into a ConfigMap every pod mounts,
// and the two privsep escalation keys, whose oslo defaults ("sudo" and "sudo
// privsep-helper") are what the image and the root container resolve.
func TestReconcileAgentConfig_NeverRendersTheSecretOrTheRootHelpers(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := withNovaMetadata("shared_secret")
	r, name := renderAgentConfig(t, cr)

	conf := renderedAgentConfigMap(t, r, name).Data[metadataAgentConfigFile]
	g.Expect(conf).NotTo(ContainSubstring("metadata_proxy_shared_secret"))
	g.Expect(conf).NotTo(ContainSubstring("root_helper"))
	g.Expect(conf).NotTo(ContainSubstring("helper_command"))
}

// The Nova keys follow the block that names them: an agent proxying nowhere
// renders neither, and an agent with a block renders exactly what it set.
func TestReconcileAgentConfig_NovaMetadataKeysFollowTheSpec(t *testing.T) {
	t.Run("no block renders neither key", func(t *testing.T) {
		g := NewGomegaWithT(t)
		r, name := renderAgentConfig(t, validAgent())

		conf := renderedAgentConfigMap(t, r, name).Data[metadataAgentConfigFile]
		g.Expect(conf).NotTo(ContainSubstring("nova_metadata_host"))
		g.Expect(conf).NotTo(ContainSubstring("nova_metadata_port"))
	})

	t.Run("a block renders both keys", func(t *testing.T) {
		g := NewGomegaWithT(t)
		r, name := renderAgentConfig(t, withNovaMetadata("shared_secret"))

		conf := renderedAgentConfigMap(t, r, name).Data[metadataAgentConfigFile]
		g.Expect(conf).To(ContainSubstring("nova_metadata_host = nova-metadata.openstack.svc"))
		g.Expect(conf).To(ContainSubstring("nova_metadata_port = 8775"))
	})

	t.Run("an empty host keeps the oslo default", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cr := withNovaMetadata("shared_secret")
		cr.Spec.NovaMetadata.Host = ""
		r, name := renderAgentConfig(t, cr)

		conf := renderedAgentConfigMap(t, r, name).Data[metadataAgentConfigFile]
		g.Expect(conf).NotTo(ContainSubstring("nova_metadata_host"))
		g.Expect(conf).To(ContainSubstring("nova_metadata_port = 8775"))
	})
}

// The broker section follows spec.messaging, and its TLS keys follow the TLS
// block inside it. An agent that names no bus renders no [oslo_messaging_rabbit]
// at all, because it opens no connection to configure.
func TestReconcileAgentConfig_RabbitSectionFollowsTheMessagingBlock(t *testing.T) {
	t.Run("no messaging renders no broker section", func(t *testing.T) {
		g := NewGomegaWithT(t)
		r, name := renderAgentConfig(t, validAgent())

		conf := renderedAgentConfigMap(t, r, name).Data[metadataAgentConfigFile]
		g.Expect(conf).NotTo(ContainSubstring("[oslo_messaging_rabbit]"))
	})

	t.Run("messaging renders the shared broker defaults", func(t *testing.T) {
		g := NewGomegaWithT(t)
		r, name := renderAgentConfig(t, withManagedMessaging())

		conf := renderedAgentConfigMap(t, r, name).Data[metadataAgentConfigFile]
		g.Expect(conf).To(ContainSubstring("[oslo_messaging_rabbit]"))
		g.Expect(conf).To(ContainSubstring("rabbit_quorum_queue = true"))
		g.Expect(conf).To(ContainSubstring("use_queue_manager = true"))
		g.Expect(conf).NotTo(ContainSubstring("ssl = true"))
		// The URL carries the broker password, so it arrives as an env override
		// rather than in the file every pod mounts.
		g.Expect(conf).NotTo(ContainSubstring("transport_url"))
	})

	t.Run("a TLS block adds the two ssl keys", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cr := withManagedMessaging()
		cr.Spec.Messaging.TLS = &commonv1.MessagingTLSSpec{
			CABundleSecretRef: commonv1.SecretRefSpec{Name: "rabbitmq-ca", Key: "ca.crt"},
		}
		r, name := renderAgentConfig(t, cr)

		conf := renderedAgentConfigMap(t, r, name).Data[metadataAgentConfigFile]
		g.Expect(conf).To(ContainSubstring("ssl = true"))
		g.Expect(conf).To(ContainSubstring("ssl_ca_file = " + rabbitmqCAFilePath))
	})
}

// json logging ships the oslo.log fileConfig beside the agent file and points
// the process at it, so the container emits one JSON record per log line.
func TestReconcileAgentConfig_JSONLoggingShipsLoggingConf(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := validAgent()
	cr.Spec.Logging = &neutronv1alpha1.LoggingSpec{Format: "json", Level: "DEBUG", Debug: ptr.To(true)}
	r, name := renderAgentConfig(t, cr)

	cm := renderedAgentConfigMap(t, r, name)
	g.Expect(cm.Data).To(HaveLen(2))
	g.Expect(cm.Data).To(HaveKey(loggingConfDataKey))
	g.Expect(cm.Data[loggingConfDataKey]).To(ContainSubstring("level = DEBUG"))

	conf := cm.Data[metadataAgentConfigFile]
	g.Expect(conf).To(ContainSubstring("log_config_append = " + loggingConfFilePath))
	g.Expect(conf).To(ContainSubstring("debug = true"))
}

// extraConfig is the escape hatch: it wins over the operator's own value for a
// key the registry does not reject, and it adds sections the operator does not
// render at all.
func TestReconcileAgentConfig_ExtraConfigOverridesTheDefaults(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := validAgent()
	cr.Spec.ExtraConfig = map[string]map[string]string{
		"DEFAULT": {"metadata_workers": "4"},
		"agent":   {"report_interval": "30"},
	}
	r, name := renderAgentConfig(t, cr)

	conf := renderedAgentConfigMap(t, r, name).Data[metadataAgentConfigFile]
	g.Expect(conf).To(ContainSubstring("metadata_workers = 4"))
	g.Expect(conf).To(ContainSubstring("[agent]"))
	g.Expect(conf).To(ContainSubstring("report_interval = 30"))
	// The operator's own DEFAULT keys survive beside the addition.
	g.Expect(conf).To(ContainSubstring("state_path = " + neutronStatePath))
}

// An override of an operator-owned key is rendered (extraConfig is the last
// word) and reported: the informational condition and the Warning event are what
// tells an operator why a value they did not set is in the file.
func TestReconcileAgentConfig_OwnedOverrideIsReported(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := validAgent()
	cr.Spec.ExtraConfig = map[string]map[string]string{"DEFAULT": {"state_path": "/srv/neutron"}}
	r, name := renderAgentConfig(t, cr)

	g.Expect(renderedAgentConfigMap(t, r, name).Data[metadataAgentConfigFile]).
		To(ContainSubstring("state_path = /srv/neutron"))

	cond := agentCondition(cr, config.ConditionTypeExtraConfigHealthy)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Message).To(ContainSubstring("[DEFAULT] state_path"))
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).
		To(ContainElement(ContainSubstring(config.EventReasonExtraConfigOwnedKeyOverride)))

	// The condition is informational: it is neither a sub-condition of the
	// aggregate nor a metrics label, so a pass that reports it still projects.
	g.Expect(agentSubConditionTypes).NotTo(ContainElement(config.ConditionTypeExtraConfigHealthy))
}

// A spec that overrides nothing owned reports the healthy arm, so the condition
// is present on every CR rather than only on the ones with a finding.
func TestReconcileAgentConfig_NoOwnedOverrideIsHealthy(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := validAgent()
	r, _ := renderAgentConfig(t, cr)

	cond := agentCondition(cr, config.ConditionTypeExtraConfigHealthy)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).To(BeEmpty())
}

// TestReconcileAgentConfig_PrunesToRetainCount pins the rollback depth: three
// historical ConfigMaps survive beside the current one.
func TestReconcileAgentConfig_PrunesToRetainCount(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := validAgent()

	stale := make([]client.Object, 0, 5)
	for i := range 5 {
		stale = append(stale, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("%s-config-stale%d", cr.Name, i),
				Namespace: testNamespace,
				Labels:    map[string]string{config.ConfigBaseLabelKey: cr.Name + "-config"},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: neutronv1alpha1.GroupVersion.String(),
					Kind:       "NeutronMetadataAgent",
					Name:       cr.Name,
					UID:        cr.UID,
					Controller: ptr.To(true),
				}},
			},
		})
	}
	r, current := renderAgentConfig(t, cr, stale...)

	var configMaps corev1.ConfigMapList
	g.Expect(r.List(context.Background(), &configMaps, client.InNamespace(testNamespace),
		client.MatchingLabels{config.ConfigBaseLabelKey: cr.Name + "-config"})).To(Succeed())

	names := make([]string, 0, len(configMaps.Items))
	for i := range configMaps.Items {
		names = append(names, configMaps.Items[i].Name)
	}
	g.Expect(names).To(HaveLen(defaultConfigMapRetainCount+1),
		"the current ConfigMap plus %d historical ones", defaultConfigMapRetainCount)
	g.Expect(names).To(ContainElement(current))
}

// A ConfigMap the API server refuses must surface as SecretsReady=False with
// reason ConfigError, so the aggregate Ready cannot stay stale-True at the new
// generation, and the client error must stay unwrappable.
func TestReconcileAgentConfig_CreateFailureMarksSecretsReady(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := validAgent()

	boom := errors.New("admission webhook rejected the ConfigMap")
	c := neutronFakeClientBuilder(cr).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, isConfigMap := obj.(*corev1.ConfigMap); isConfigMap {
					return boom
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).Build()
	r := &NeutronMetadataAgentReconciler{Client: c, Scheme: testScheme(), Recorder: record.NewFakeRecorder(10)}

	res, name, err := r.reconcileAgentConfig(context.Background(), r.Client, cr, resolvedForAgentConfig())

	g.Expect(err).To(MatchError(boom))
	g.Expect(err).To(MatchError(ContainSubstring("creating config ConfigMap:")))
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(name).To(BeEmpty())

	cond := agentCondition(cr, "SecretsReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonConfigError))
}

// agentOperatorDefaults is a pure function of the spec and the resolved chassis,
// so the registry of owned keys can be checked against what the operator
// actually writes: a key claimed there but never rendered is a promise the
// webhook enforces on nothing.
func TestAgentOperatorDefaults_RenderEveryOwnedKey(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := withNovaMetadata("shared_secret")
	cr.Spec.Messaging = &commonv1.MessagingSpec{
		ClusterRef: &corev1.LocalObjectReference{Name: testRabbitmqClusterName},
		TLS:        &commonv1.MessagingTLSSpec{CABundleSecretRef: commonv1.SecretRefSpec{Name: "rabbitmq-ca"}},
	}

	defaults := agentOperatorDefaults(cr, resolvedForAgentConfig())

	for _, owned := range neutronv1alpha1.MetadataAgentOwnedConfigKeys {
		if owned.Key == "metadata_proxy_shared_secret" {
			// The one owned key the operator deliberately never renders: it
			// reaches the process as an env override instead.
			g.Expect(defaults[owned.Section]).NotTo(HaveKey(owned.Key))
			continue
		}
		g.Expect(defaults[owned.Section]).To(HaveKey(owned.Key),
			"%s.%s is registered as operator-owned but never rendered", owned.Section, owned.Key)
	}
}
