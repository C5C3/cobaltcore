// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/config"
	mctestutil "github.com/c5c3/cobaltcore/internal/common/testutil/multicluster"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
)

// pinComputeConfigGolden is the fragment rendered for the validNova fixture:
// both optional siblings on, a console proxy behind a gateway, a region, and a
// plaintext bus. It is the document a nova-compute outside this cluster reads,
// so a section that appears or disappears here changes what a compute the
// operator never sees is configured with.
const pinComputeConfigGolden = `[DEFAULT]
debug = false
use_stderr = true

[barbican]
auth_endpoint = http://keystone.openstack.svc.cluster.local:5000
barbican_endpoint_type = internal
barbican_region_name = RegionOne
send_service_user_token = true

[cinder]
auth_type = password
auth_url = http://keystone.openstack.svc.cluster.local:5000
catalog_info = block-storage:cinder:internalURL
os_region_name = RegionOne
project_domain_name = Default
project_name = service
user_domain_name = Default
username = nova

[glance]
region_name = RegionOne
valid_interfaces = internal

[key_manager]
backend = barbican

[keystone_authtoken]
auth_type = password
auth_url = http://keystone.openstack.svc.cluster.local:5000
project_domain_name = Default
project_name = service
region_name = RegionOne
user_domain_name = Default
username = nova
www_authenticate_uri = https://keystone.example.com

[neutron]
auth_type = password
auth_url = http://keystone.openstack.svc.cluster.local:5000
project_domain_name = Default
project_name = service
region_name = RegionOne
user_domain_name = Default
username = nova
valid_interfaces = internal

[oslo_messaging_notifications]
driver = noop

[oslo_messaging_rabbit]
rabbit_quorum_queue = true
rabbit_transient_quorum_queue = true
use_queue_manager = true

[placement]
auth_type = password
auth_url = http://keystone.openstack.svc.cluster.local:5000
project_domain_name = Default
project_name = service
region_name = RegionOne
user_domain_name = Default
username = nova
valid_interfaces = internal

[service_user]
auth_type = password
auth_url = http://keystone.openstack.svc.cluster.local:5000
project_domain_name = Default
project_name = service
region_name = RegionOne
send_service_user_token = true
user_domain_name = Default
username = nova

[upgrade_levels]
compute = auto

[vnc]
enabled = true
novncproxy_base_url = https://console.example.com/vnc_lite.html
`

// pinComputeConfigMinimalGolden is the same fragment for the smallest Nova:
// both optional siblings off, no region, no gateway. Several lines end in a
// space, because novaMinimal bypasses the defaulting webhook and that is how
// oslo spells an empty value. Do not trim them.
const pinComputeConfigMinimalGolden = `[DEFAULT]
debug = false
use_stderr = true

[glance]
valid_interfaces = internal

[keystone_authtoken]
auth_type = password
auth_url = http://keystone.openstack.svc.cluster.local:5000
project_domain_name = 
project_name = 
user_domain_name = 
username = 
www_authenticate_uri = http://keystone.openstack.svc.cluster.local:5000

[neutron]
auth_type = password
auth_url = http://keystone.openstack.svc.cluster.local:5000
project_domain_name = 
project_name = 
user_domain_name = 
username = 
valid_interfaces = internal

[oslo_messaging_notifications]
driver = noop

[oslo_messaging_rabbit]
rabbit_quorum_queue = true
rabbit_transient_quorum_queue = true
use_queue_manager = true

[placement]
auth_type = password
auth_url = http://keystone.openstack.svc.cluster.local:5000
project_domain_name = 
project_name = 
user_domain_name = 
username = 
valid_interfaces = internal

[service_user]
auth_type = password
auth_url = http://keystone.openstack.svc.cluster.local:5000
project_domain_name = 
project_name = 
send_service_user_token = true
user_domain_name = 
username = 

[upgrade_levels]
compute = auto

[vnc]
enabled = true
novncproxy_base_url = http://nova-novncproxy.openstack.svc.cluster.local:6080/vnc_lite.html
`

// testComputeTransportURL is the bus URL the messaging step returns, which the
// compute contract republishes verbatim.
const testComputeTransportURL = "rabbit://default_user_abc:s3cr3t@rabbitmq.openstack.svc:5672/"

// computeSecretValues returns the three values reconcileSecrets reads and the
// compute contract carries onward.
func computeSecretValues() secretValues {
	return secretValues{
		serviceUserPassword:  "svc-pw",
		metadataSharedSecret: "shared-secret",
		messagingCA:          "-----BEGIN CERTIFICATE-----",
	}
}

// publishedComputeConfig reads the compute-contract Secret back off c.
func publishedComputeConfig(t *testing.T, c client.Client, nova *novav1alpha1.Nova) *corev1.Secret {
	t.Helper()

	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: nova.Namespace, Name: computeConfigSecretName(nova)}
	if err := c.Get(context.Background(), key, secret); err != nil {
		t.Fatalf("reading the published compute config Secret %s: %v", key, err)
	}
	return secret
}

// TestPinComputeConfigFragment pins the fragment byte for byte for both
// fixtures. The document names no release, so the 2026.1 render has to be the
// same bytes: a compute is configured from what the CR says about the services
// it calls, not from the version the control plane runs.
func TestPinComputeConfigFragment(t *testing.T) {
	g := NewGomegaWithT(t)

	expectGolden(t, config.RenderINI(computeConfigDefaults(validNova())), pinComputeConfigGolden)
	expectGolden(t, config.RenderINI(computeConfigDefaults(novaMinimal())), pinComputeConfigMinimalGolden)

	next := validNova()
	next.Spec.OpenStackRelease = "2026.1"
	g.Expect(config.RenderINI(computeConfigDefaults(next))).To(Equal(pinComputeConfigGolden),
		"the fragment carries no release, so an upgrade must not rewrite the compute contract")
}

// TestComputeConfigDefaults_OmitsDatabaseAndMetadataKeys covers the difference
// between the compute fragment and the control plane's own nova.conf: the
// sections a compute needs are there, and the ones that would point it at a
// schema, a memcached or a front end it does not run are not.
func TestComputeConfigDefaults_OmitsDatabaseAndMetadataKeys(t *testing.T) {
	g := NewGomegaWithT(t)

	rendered := config.RenderINI(computeConfigDefaults(validNova()))

	for _, present := range []string{
		"[placement]", "[neutron]", "[glance]", "[cinder]", "[key_manager]", "[barbican]",
		"[keystone_authtoken]", "[service_user]", "[oslo_messaging_rabbit]", "[vnc]",
	} {
		g.Expect(rendered).To(ContainSubstring(present))
	}
	g.Expect(rendered).To(ContainSubstring(
		"novncproxy_base_url = https://console.example.com/vnc_lite.html"))
	g.Expect(rendered).To(ContainSubstring("www_authenticate_uri = https://keystone.example.com"),
		"the address a 401 points a client at is not one this process dials, so it stays")

	for _, absent := range []string{
		"[database]", "[api_database]", "[api]", "[cache]",
		"memcached_servers", "service_metadata_proxy", "metadata_proxy_shared_secret", "state_path",
	} {
		g.Expect(rendered).NotTo(ContainSubstring(absent),
			"%s configures something a compute node does not run or cannot reach", absent)
	}

	g.Expect(config.RenderINI(computeConfigDefaults(novaWithMessagingTLS()))).
		To(ContainSubstring("ssl_ca_file = /etc/nova/compute-config/ca.crt"),
			"the bundle is read from the directory the compute mounts this Secret at")

	// nova's [vnc] enabled defaults to true, so a deployment without a console
	// proxy has to switch the console off on the compute too: left out, the
	// compute would still attach VNC devices and hand out a console URL nothing
	// serves.
	disabled := config.RenderINI(computeConfigDefaults(disabledProxyNova()))
	g.Expect(disabled).To(ContainSubstring("[vnc]\nenabled = false\n"))
	g.Expect(disabled).NotTo(ContainSubstring("novncproxy_base_url"),
		"a deployment without a console proxy has no base URL to hand out")
}

// TestReconcileComputeConfig_ControlCharInATypedFieldKeepsThePublishedSecret
// covers the fragment's own guard. The config step refuses a newline in a typed
// field for nova.conf and lets the pipeline carry on, so this step is the only
// thing between an injected section and every hypervisor that loads the
// fragment: it must not write it, and the Secret already published stays as it
// was.
func TestReconcileComputeConfig_ControlCharInATypedFieldKeepsThePublishedSecret(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	r := newNovaTestReconciler(nova)

	_, err := r.reconcileComputeConfig(context.Background(), r.Client, nova,
		testComputeTransportURL, computeSecretValues())
	g.Expect(err).NotTo(HaveOccurred())
	published := publishedComputeConfig(t, r.Client, nova)

	nova.Spec.Region = "RegionOne\n[workarounds]\ndisable_rootwrap = true"
	result, err := r.reconcileComputeConfig(context.Background(), r.Client, nova,
		testComputeTransportURL, computeSecretValues())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue())
	live := publishedComputeConfig(t, r.Client, nova)
	g.Expect(live.Data).To(Equal(published.Data), "the injected section must never reach the Secret")
	g.Expect(string(live.Data[computeConfigFragmentKey])).NotTo(ContainSubstring("[workarounds]"))

	cond := novaCondition(nova, conditionTypeComputeConfigReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonComputeConfigError))
}

// TestReconcileComputeConfig_KeySetWithAndWithoutTLS pins the contract itself:
// five keys on a plaintext bus, six on a verified one, each carrying the value
// the upstream step produced.
func TestReconcileComputeConfig_KeySetWithAndWithoutTLS(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	r := newNovaTestReconciler(nova)

	result, err := r.reconcileComputeConfig(context.Background(), r.Client, nova,
		testComputeTransportURL, computeSecretValues())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue())

	secret := publishedComputeConfig(t, r.Client, nova)
	g.Expect(secret.Type).To(Equal(corev1.SecretTypeOpaque))
	g.Expect(secret.Labels).To(Equal(componentLabels(nova, componentComputeConfig)))
	g.Expect(secret.Data).To(HaveLen(5), "a plaintext bus ships no CA bundle")
	g.Expect(secret.Data).NotTo(HaveKey(caBundleKey))
	g.Expect(string(secret.Data[computeConfigFragmentKey])).To(Equal(pinComputeConfigGolden))
	g.Expect(string(secret.Data[transportURLKey])).To(Equal(testComputeTransportURL))
	g.Expect(string(secret.Data[passwordKey])).To(Equal("svc-pw"))
	g.Expect(string(secret.Data[metadataSharedSecretKey])).To(Equal("shared-secret"))
	g.Expect(string(secret.Data[cellNameKey])).To(Equal("cell1"))

	tlsNova := novaWithMessagingTLS()
	tlsReconciler := newNovaTestReconciler(tlsNova)

	_, err = tlsReconciler.reconcileComputeConfig(context.Background(), tlsReconciler.Client, tlsNova,
		testComputeTransportURL, computeSecretValues())

	g.Expect(err).NotTo(HaveOccurred())
	tlsSecret := publishedComputeConfig(t, tlsReconciler.Client, tlsNova)
	g.Expect(tlsSecret.Data).To(HaveLen(6))
	g.Expect(string(tlsSecret.Data[caBundleKey])).To(Equal("-----BEGIN CERTIFICATE-----"))
}

// TestReconcileComputeConfig_DropsTheCABundleWhenTheBusLeavesTLS covers the key
// that comes and goes with the bus: a Nova moved off a TLS bus has to take the
// CA bundle out of the contract, or a compute keeps a trust anchor nothing
// verifies against. The apply owns the whole key set, so the second write
// removes the key it wrote the first time.
func TestReconcileComputeConfig_DropsTheCABundleWhenTheBusLeavesTLS(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := novaWithMessagingTLS()
	r := newNovaTestReconciler(nova)

	_, err := r.reconcileComputeConfig(context.Background(), r.Client, nova,
		testComputeTransportURL, computeSecretValues())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(publishedComputeConfig(t, r.Client, nova).Data).To(HaveKey(caBundleKey))

	nova.Spec.Messaging.TLS = nil
	_, err = r.reconcileComputeConfig(context.Background(), r.Client, nova,
		testComputeTransportURL, computeSecretValues())
	g.Expect(err).NotTo(HaveOccurred())

	secret := publishedComputeConfig(t, r.Client, nova)
	g.Expect(secret.Data).To(HaveLen(5))
	g.Expect(secret.Data).NotTo(HaveKey(caBundleKey))
}

// TestReconcileComputeConfig_ReplacesStaleFragment covers the reason the name is
// stable: a compute mounts the Secret by name, so a rotated password or a
// changed fragment has to reach it through the same object rather than through a
// new one.
func TestReconcileComputeConfig_ReplacesStaleFragment(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	stale := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      computeConfigSecretName(nova),
			Namespace: nova.Namespace,
			UID:       "compute-config-uid",
			Labels:    componentLabels(nova, componentComputeConfig),
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			computeConfigFragmentKey: []byte("[DEFAULT]\ndebug = true\n"),
			transportURLKey:          []byte("rabbit://retired:retired@rabbitmq.openstack.svc:5672/"),
			passwordKey:              []byte("retired-pw"),
			metadataSharedSecretKey:  []byte("retired-shared"),
			cellNameKey:              []byte(computeCellName),
		},
	}
	r := newNovaTestReconciler(nova, stale)

	_, err := r.reconcileComputeConfig(context.Background(), r.Client, nova,
		testComputeTransportURL, computeSecretValues())

	g.Expect(err).NotTo(HaveOccurred())
	secret := publishedComputeConfig(t, r.Client, nova)
	g.Expect(secret.UID).To(Equal(stale.UID), "the Secret is updated in place, never replaced")
	g.Expect(string(secret.Data[computeConfigFragmentKey])).To(Equal(pinComputeConfigGolden))
	g.Expect(string(secret.Data[computeConfigFragmentKey])).NotTo(ContainSubstring("debug = true"))
	g.Expect(string(secret.Data[transportURLKey])).To(Equal(testComputeTransportURL))
	g.Expect(string(secret.Data[passwordKey])).To(Equal("svc-pw"))
	g.Expect(string(secret.Data[metadataSharedSecretKey])).To(Equal("shared-secret"))
}

// TestReconcileComputeConfig_SetsStatusRefAndCondition covers the handover the
// status publishes: the name a consumer resolves the Secret by, and the
// condition that says it is there.
func TestReconcileComputeConfig_SetsStatusRefAndCondition(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	r := newNovaTestReconciler(nova)

	_, err := r.reconcileComputeConfig(context.Background(), r.Client, nova,
		testComputeTransportURL, computeSecretValues())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(nova.Status.ComputeConfigSecretRef).NotTo(BeNil())
	g.Expect(nova.Status.ComputeConfigSecretRef.Name).To(Equal("nova-compute-config"))

	cond := novaCondition(nova, conditionTypeComputeConfigReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonComputeConfigPublished))
	g.Expect(cond.ObservedGeneration).To(Equal(nova.Generation))
}

// TestReconcileComputeConfig_ApplyFailureSetsErrorAndWraps covers the failing
// write: the condition carries the reason a compute cannot be brought up, and
// the status advertises no Secret the cluster does not have.
func TestReconcileComputeConfig_ApplyFailureSetsErrorAndWraps(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	boom := errors.New("the API server is unavailable")
	r := failingApplyReconciler(boom, "Secret", computeConfigSecretName(nova), nova)

	_, err := r.reconcileComputeConfig(context.Background(), r.Client, nova,
		testComputeTransportURL, computeSecretValues())

	g.Expect(err).To(MatchError(boom))
	g.Expect(err.Error()).To(HavePrefix("publishing the compute config Secret:"))
	g.Expect(nova.Status.ComputeConfigSecretRef).To(BeNil(),
		"a Secret that was not written must not be advertised")

	cond := novaCondition(nova, conditionTypeComputeConfigReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonComputeConfigError))
	g.Expect(cond.Message).To(ContainSubstring("the API server is unavailable"))
}

// TestReconcileComputeConfig_RefusesAnUnownedSecretOnATarget covers the placed
// Nova whose target cluster already carries a Secret of that name: the apply
// refuses it rather than overwriting somebody else's bytes and deleting the
// object at teardown.
func TestReconcileComputeConfig_RefusesAnUnownedSecretOnATarget(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	nova.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "remote-a"}
	foreign := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      computeConfigSecretName(nova),
			Namespace: nova.Namespace,
			UID:       "foreign-uid",
		},
		Data: map[string][]byte{computeConfigFragmentKey: []byte("somebody else's fragment")},
	}
	r := newNovaTestReconciler(nova)
	children := mctestutil.RemoteChildren(t, r.Client, novaFakeClientBuilder(foreign).Build())

	_, err := r.reconcileComputeConfig(context.Background(), children, nova,
		testComputeTransportURL, computeSecretValues())

	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(HavePrefix("publishing the compute config Secret:"))
	g.Expect(err.Error()).To(ContainSubstring("refusing to adopt pre-existing"))

	cond := novaCondition(nova, conditionTypeComputeConfigReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonComputeConfigError))

	live := publishedComputeConfig(t, children, nova)
	g.Expect(string(live.Data[computeConfigFragmentKey])).To(Equal("somebody else's fragment"))
	g.Expect(live.Labels).To(BeEmpty(), "a refused object keeps no ownership label")
}
