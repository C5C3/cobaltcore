// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

// pinRemoteComputeConfigGolden is the remote fragment rendered for the
// validNova fixture with spec.remoteCompute.keystoneEndpoint set to
// testRemoteKeystoneEndpoint. It differs from pinComputeConfigGolden in the
// addressing keys alone: every auth_url and [barbican] auth_endpoint name the
// remote Keystone URL, and every client section resolves the public catalog
// row.
const pinRemoteComputeConfigGolden = `[DEFAULT]
debug = false
use_stderr = true

[barbican]
auth_endpoint = https://keystone.example.com/v3
barbican_endpoint_type = public
barbican_region_name = RegionOne
send_service_user_token = true

[cinder]
auth_type = password
auth_url = https://keystone.example.com/v3
catalog_info = block-storage:cinder:publicURL
os_region_name = RegionOne
project_domain_name = Default
project_name = service
user_domain_name = Default
username = nova

[glance]
region_name = RegionOne
valid_interfaces = public

[key_manager]
backend = barbican

[keystone_authtoken]
auth_type = password
auth_url = https://keystone.example.com/v3
project_domain_name = Default
project_name = service
region_name = RegionOne
user_domain_name = Default
username = nova
www_authenticate_uri = https://keystone.example.com

[neutron]
auth_type = password
auth_url = https://keystone.example.com/v3
project_domain_name = Default
project_name = service
region_name = RegionOne
user_domain_name = Default
username = nova
valid_interfaces = public

[oslo_messaging_notifications]
driver = noop

[oslo_messaging_rabbit]
rabbit_quorum_queue = true
rabbit_transient_quorum_queue = true
use_queue_manager = true

[placement]
auth_type = password
auth_url = https://keystone.example.com/v3
project_domain_name = Default
project_name = service
region_name = RegionOne
user_domain_name = Default
username = nova
valid_interfaces = public

[service_user]
auth_type = password
auth_url = https://keystone.example.com/v3
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

// testRemoteTransportURL is the broker's external listener the remote
// contract carries in place of the in-cluster bus URL.
const testRemoteTransportURL = "rabbit://u:p@198.51.100.10:5671/"

// remoteTransportSecret returns the Secret remoteComputeNova reads its remote
// transport URL from, carrying value under the default key.
func remoteTransportSecret(value string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testRemoteTransportSecret, Namespace: testNamespace},
		Data:       map[string][]byte{commonv1.DefaultTransportURLSecretKey: []byte(value)},
	}
}

// publishedRemoteComputeConfig reads the remote compute-contract Secret back
// off c.
func publishedRemoteComputeConfig(t *testing.T, c client.Client, nova *novav1alpha1.Nova) *corev1.Secret {
	t.Helper()

	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: nova.Namespace, Name: remoteComputeConfigSecretName(nova)}
	if err := c.Get(context.Background(), key, secret); err != nil {
		t.Fatalf("reading the published remote compute config Secret %s: %v", key, err)
	}
	return secret
}

// remoteComputeConfigAbsent reports whether c holds no remote compute-contract
// Secret for nova.
func remoteComputeConfigAbsent(c client.Client, nova *novav1alpha1.Nova) bool {
	err := c.Get(context.Background(), client.ObjectKey{
		Namespace: nova.Namespace, Name: remoteComputeConfigSecretName(nova),
	}, &corev1.Secret{})
	return apierrors.IsNotFound(err)
}

// expectComputeConfigCondition asserts ComputeConfigReady carries status and
// reason, and returns it for further checks.
func expectComputeConfigCondition(g Gomega, nova *novav1alpha1.Nova, status metav1.ConditionStatus,
	reason string,
) *metav1.Condition {
	cond := novaCondition(nova, conditionTypeComputeConfigReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(status))
	g.Expect(cond.Reason).To(Equal(reason))
	return cond
}

// TestPinRemoteComputeConfigFragment pins the remote fragment byte for byte,
// and pins what it shares with the in-cluster one: the address a 401 points a
// client at, the bus settings and the console. Only the addressing keys may
// differ between the two documents.
func TestPinRemoteComputeConfigFragment(t *testing.T) {
	g := NewGomegaWithT(t)

	nova := validNova()
	nova.Spec.RemoteCompute = &novav1alpha1.NovaRemoteComputeSpec{KeystoneEndpoint: testRemoteKeystoneEndpoint}

	remote := remoteComputeConfigDefaults(nova)
	expectGolden(t, config.RenderINI(remote), pinRemoteComputeConfigGolden)

	local := computeConfigDefaults(nova)
	g.Expect(remote).To(HaveLen(len(local)), "both documents carry the same sections")
	for section := range local {
		g.Expect(remote).To(HaveKey(section))
	}
	g.Expect(remote["keystone_authtoken"]["www_authenticate_uri"]).
		To(Equal(local["keystone_authtoken"]["www_authenticate_uri"]))
	g.Expect(remote["oslo_messaging_rabbit"]).To(Equal(local["oslo_messaging_rabbit"]))
	g.Expect(remote["vnc"]).To(Equal(local["vnc"]))
}

// TestRemoteComputeConfigDefaults_DropsEveryOverride covers the overrides: they
// name addresses the control-plane pods dial, so the in-cluster fragment keeps
// them and the remote one resolves the public catalog row instead.
func TestRemoteComputeConfigDefaults_DropsEveryOverride(t *testing.T) {
	g := NewGomegaWithT(t)

	nova := remoteComputeNova()
	nova.Spec.Endpoints.Placement.Override = "http://placement.openstack.svc:8778"
	nova.Spec.Endpoints.Neutron.Override = "http://neutron.openstack.svc:9696"
	nova.Spec.Endpoints.Glance.Override = "http://glance.openstack.svc:9292"
	nova.Spec.Endpoints.Cinder.Override = "http://cinder.openstack.svc:8776/v3/%(project_id)s"
	nova.Spec.Endpoints.Barbican.Override = "http://barbican.openstack.svc:9311"

	local := config.RenderINI(computeConfigDefaults(nova))
	g.Expect(local).To(ContainSubstring("endpoint_override = http://placement.openstack.svc:8778"))
	g.Expect(local).To(ContainSubstring("endpoint_template = http://cinder.openstack.svc:8776"))
	g.Expect(local).To(ContainSubstring("barbican_endpoint = http://barbican.openstack.svc:9311"))

	remote := config.RenderINI(remoteComputeConfigDefaults(nova))
	for _, absent := range []string{"endpoint_override", "endpoint_template", "barbican_endpoint ="} {
		g.Expect(remote).NotTo(ContainSubstring(absent),
			"%s names an address the control-plane pods dial, not one a compute cluster reaches", absent)
	}
	g.Expect(strings.Count(remote, "valid_interfaces = public")).To(Equal(3))
}

// TestRemoteComputeConfigDefaults_OptionalSiblingsOff covers a Nova without
// block storage and key manager: the remote rewrite has no section of theirs to
// re-address, and must not invent one.
func TestRemoteComputeConfigDefaults_OptionalSiblingsOff(t *testing.T) {
	g := NewGomegaWithT(t)

	nova := remoteComputeNova()
	nova.Spec.Endpoints.Cinder.Enabled = false
	nova.Spec.Endpoints.Barbican.Enabled = false

	sections := remoteComputeConfigDefaults(nova)
	g.Expect(sections).NotTo(HaveKey("cinder"))
	g.Expect(sections).NotTo(HaveKey("barbican"))
	g.Expect(sections).NotTo(HaveKey("key_manager"))
	g.Expect(sections["placement"]["valid_interfaces"]).To(Equal("public"))
}

// TestReconcileComputeConfig_PublishesTheRemoteContract covers the second
// contract: the same six keys as the in-cluster one, the external transport URL
// in place of the bus URL, and every other value shared.
func TestReconcileComputeConfig_PublishesTheRemoteContract(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := remoteComputeNova()
	r := newNovaTestReconciler(nova, remoteTransportSecret(testRemoteTransportURL))

	result, err := r.reconcileComputeConfig(context.Background(), r.Client, nova,
		testComputeTransportURL, computeSecretValues())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue())

	local := publishedComputeConfig(t, r.Client, nova)
	remote := publishedRemoteComputeConfig(t, r.Client, nova)
	g.Expect(remote.Type).To(Equal(corev1.SecretTypeOpaque))
	g.Expect(remote.Labels).To(Equal(componentLabels(nova, componentRemoteComputeConfig)))
	g.Expect(remote.Data).To(HaveLen(6))
	for key := range local.Data {
		g.Expect(remote.Data).To(HaveKey(key), "the remote contract carries every key of the in-cluster one")
	}
	g.Expect(string(remote.Data[transportURLKey])).To(Equal(testRemoteTransportURL))
	g.Expect(string(local.Data[transportURLKey])).To(Equal(testComputeTransportURL),
		"the in-cluster contract keeps the bus URL its neighbours reach")
	for _, key := range []string{passwordKey, metadataSharedSecretKey, cellNameKey, caBundleKey} {
		g.Expect(remote.Data[key]).To(Equal(local.Data[key]), "%s is the same value in both contracts", key)
	}
	g.Expect(string(remote.Data[computeConfigFragmentKey])).
		To(Equal(config.RenderINI(remoteComputeConfigDefaults(nova))))
	g.Expect(string(remote.Data[computeConfigFragmentKey])).
		To(ContainSubstring("ssl_ca_file = /etc/nova/compute-config/ca.crt"),
			"the remote compute mounts the Secret at the same path")

	g.Expect(nova.Status.RemoteComputeConfigSecretRef).NotTo(BeNil())
	g.Expect(nova.Status.RemoteComputeConfigSecretRef.Name).To(Equal("nova-remote-compute-config"))
	expectComputeConfigCondition(g, nova, metav1.ConditionTrue, conditionReasonComputeConfigPublished)
}

// TestReconcileComputeConfig_WaitsForTheRemoteTransportURL covers a transport
// URL that is not there yet: the in-cluster contract is still written, the
// pipeline goes on, and a remote contract published earlier keeps its bytes.
func TestReconcileComputeConfig_WaitsForTheRemoteTransportURL(t *testing.T) {
	t.Run("missing Secret", func(t *testing.T) {
		g := NewGomegaWithT(t)
		nova := remoteComputeNova()
		r := newNovaTestReconciler(nova)

		result, err := r.reconcileComputeConfig(context.Background(), r.Client, nova,
			testComputeTransportURL, computeSecretValues())

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(result.IsZero()).To(BeTrue(), "the wait does not halt the control-plane steps behind it")
		publishedComputeConfig(t, r.Client, nova)
		g.Expect(nova.Status.ComputeConfigSecretRef).NotTo(BeNil())
		g.Expect(remoteComputeConfigAbsent(r.Client, nova)).To(BeTrue())
		g.Expect(nova.Status.RemoteComputeConfigSecretRef).To(BeNil())

		cond := expectComputeConfigCondition(g, nova, metav1.ConditionFalse,
			conditionReasonWaitingForRemoteTransportURL)
		g.Expect(cond.Message).To(HavePrefix("spec.remoteCompute.transportURLSecretRef:"))
		g.Expect(cond.Message).To(ContainSubstring("not found"))
	})

	t.Run("empty key keeps the published Secret", func(t *testing.T) {
		g := NewGomegaWithT(t)
		nova := remoteComputeNova()
		r := newNovaTestReconciler(nova, remoteTransportSecret(testRemoteTransportURL))

		_, err := r.reconcileComputeConfig(context.Background(), r.Client, nova,
			testComputeTransportURL, computeSecretValues())
		g.Expect(err).NotTo(HaveOccurred())
		published := publishedRemoteComputeConfig(t, r.Client, nova)

		live := &corev1.Secret{}
		g.Expect(r.Get(context.Background(), client.ObjectKey{
			Namespace: testNamespace, Name: testRemoteTransportSecret,
		}, live)).To(Succeed())
		live.Data[commonv1.DefaultTransportURLSecretKey] = nil
		g.Expect(r.Update(context.Background(), live)).To(Succeed())

		result, err := r.reconcileComputeConfig(context.Background(), r.Client, nova,
			testComputeTransportURL, computeSecretValues())

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(result.IsZero()).To(BeTrue())
		g.Expect(publishedRemoteComputeConfig(t, r.Client, nova).Data).To(Equal(published.Data))
		cond := expectComputeConfigCondition(g, nova, metav1.ConditionFalse,
			conditionReasonWaitingForRemoteTransportURL)
		g.Expect(cond.Message).To(HavePrefix("spec.remoteCompute.transportURLSecretRef:"))
		g.Expect(cond.Message).To(ContainSubstring(`missing key "transport_url"`))
	})
}

// TestReconcileComputeConfig_RejectsANonRabbitRemoteURL covers a remote URL for
// a driver the compute is not configured for. The error names the scheme and
// never the URL, which carries the broker password.
func TestReconcileComputeConfig_RejectsANonRabbitRemoteURL(t *testing.T) {
	g := NewGomegaWithT(t)
	const amqp = "amqp://u:p@198.51.100.10:5671/"
	nova := remoteComputeNova()
	r := newNovaTestReconciler(nova, remoteTransportSecret(amqp))

	_, err := r.reconcileComputeConfig(context.Background(), r.Client, nova,
		testComputeTransportURL, computeSecretValues())

	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(HavePrefix("resolving the remote transport URL:"))
	g.Expect(err.Error()).To(ContainSubstring("scheme must be rabbit"))
	g.Expect(err.Error()).NotTo(ContainSubstring(amqp))
	g.Expect(remoteComputeConfigAbsent(r.Client, nova)).To(BeTrue())

	cond := expectComputeConfigCondition(g, nova, metav1.ConditionFalse, conditionReasonComputeConfigError)
	g.Expect(cond.Message).NotTo(ContainSubstring("u:p@"))
}

// TestReconcileComputeConfig_RemoteApplyFailureSetsErrorAndWraps covers the
// failing write of the remote Secret: the status advertises no Secret the
// cluster does not have.
func TestReconcileComputeConfig_RemoteApplyFailureSetsErrorAndWraps(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := remoteComputeNova()
	boom := errors.New("the API server is unavailable")
	r := failingApplyReconciler(boom, "Secret", remoteComputeConfigSecretName(nova),
		nova, remoteTransportSecret(testRemoteTransportURL))

	_, err := r.reconcileComputeConfig(context.Background(), r.Client, nova,
		testComputeTransportURL, computeSecretValues())

	g.Expect(err).To(MatchError(boom))
	g.Expect(err.Error()).To(HavePrefix("publishing the remote compute config Secret:"))
	g.Expect(nova.Status.RemoteComputeConfigSecretRef).To(BeNil())
	cond := expectComputeConfigCondition(g, nova, metav1.ConditionFalse, conditionReasonComputeConfigError)
	g.Expect(cond.Message).To(ContainSubstring("the API server is unavailable"))
}

// TestReconcileComputeConfig_ClearingRemoteComputeDeletesTheSecret covers the
// block's removal: the remote contract goes with it, a same-named Secret the
// Nova does not control stays, and a failing delete is reported.
func TestReconcileComputeConfig_ClearingRemoteComputeDeletesTheSecret(t *testing.T) {
	t.Run("the published Secret is deleted", func(t *testing.T) {
		g := NewGomegaWithT(t)
		nova := remoteComputeNova()
		r := newNovaTestReconciler(nova, remoteTransportSecret(testRemoteTransportURL))

		_, err := r.reconcileComputeConfig(context.Background(), r.Client, nova,
			testComputeTransportURL, computeSecretValues())
		g.Expect(err).NotTo(HaveOccurred())
		publishedRemoteComputeConfig(t, r.Client, nova)

		nova.Spec.RemoteCompute = nil
		_, err = r.reconcileComputeConfig(context.Background(), r.Client, nova,
			testComputeTransportURL, computeSecretValues())

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(remoteComputeConfigAbsent(r.Client, nova)).To(BeTrue())
		g.Expect(nova.Status.RemoteComputeConfigSecretRef).To(BeNil())
		g.Expect(nova.Status.ComputeConfigSecretRef).NotTo(BeNil(), "the in-cluster contract stays")
		expectComputeConfigCondition(g, nova, metav1.ConditionTrue, conditionReasonComputeConfigPublished)
	})

	// On a target cluster the Secret carries the ownership labels in place of
	// an owner reference, so the delete has to recognize it by those.
	t.Run("the published Secret is deleted on a target cluster", func(t *testing.T) {
		g := NewGomegaWithT(t)
		nova := remoteComputeNova()
		nova.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "remote-a"}
		r := newNovaTestReconciler(nova)
		target := novaFakeClientBuilder(remoteTransportSecret(testRemoteTransportURL)).Build()
		children := mctestutil.RemoteChildren(t, r.Client, target)

		_, err := r.reconcileComputeConfig(context.Background(), children, nova,
			testComputeTransportURL, computeSecretValues())
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(publishedRemoteComputeConfig(t, target, nova).OwnerReferences).To(BeEmpty(),
			"a target-cluster child is claimed by its labels")

		nova.Spec.RemoteCompute = nil
		_, err = r.reconcileComputeConfig(context.Background(), children, nova,
			testComputeTransportURL, computeSecretValues())

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(remoteComputeConfigAbsent(target, nova)).To(BeTrue(),
			"the Secret carries the broker URL and both credentials, so it must not outlive the block")
		g.Expect(nova.Status.RemoteComputeConfigSecretRef).To(BeNil())
		expectComputeConfigCondition(g, nova, metav1.ConditionTrue, conditionReasonComputeConfigPublished)
	})

	t.Run("a Secret the Nova does not control survives", func(t *testing.T) {
		g := NewGomegaWithT(t)
		nova := novaWithMessagingTLS()
		foreign := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: remoteComputeConfigSecretName(nova), Namespace: nova.Namespace},
			Data:       map[string][]byte{computeConfigFragmentKey: []byte("somebody else's fragment")},
		}
		r := newNovaTestReconciler(nova, foreign)

		_, err := r.reconcileComputeConfig(context.Background(), r.Client, nova,
			testComputeTransportURL, computeSecretValues())

		g.Expect(err).NotTo(HaveOccurred())
		live := &corev1.Secret{}
		g.Expect(r.Get(context.Background(), client.ObjectKeyFromObject(foreign), live)).To(Succeed())
		g.Expect(string(live.Data[computeConfigFragmentKey])).To(Equal("somebody else's fragment"))
	})

	t.Run("a failing delete is reported", func(t *testing.T) {
		g := NewGomegaWithT(t)
		nova := remoteComputeNova()
		boom := errors.New("the API server is unavailable")
		r := failingDeleteReconciler(boom, "Secret", remoteComputeConfigSecretName(nova),
			nova, remoteTransportSecret(testRemoteTransportURL))

		_, err := r.reconcileComputeConfig(context.Background(), r.Client, nova,
			testComputeTransportURL, computeSecretValues())
		g.Expect(err).NotTo(HaveOccurred())

		nova.Spec.RemoteCompute = nil
		_, err = r.reconcileComputeConfig(context.Background(), r.Client, nova,
			testComputeTransportURL, computeSecretValues())

		g.Expect(err).To(MatchError(boom))
		g.Expect(err.Error()).To(HavePrefix("deleting the remote compute config Secret:"))
		expectComputeConfigCondition(g, nova, metav1.ConditionFalse, conditionReasonComputeConfigError)
	})
}

// TestReconcileComputeConfig_ControlCharInRemoteKeystoneEndpointKeepsThePublishedSecret
// covers the remote fragment's own guard. spec.remoteCompute.keystoneEndpoint
// is rendered into every auth_url of the remote fragment, and a CR that
// bypassed admission can carry a newline in it: the injected section must not
// reach the remote Secret, and the one published earlier stays as it was.
func TestReconcileComputeConfig_ControlCharInRemoteKeystoneEndpointKeepsThePublishedSecret(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := remoteComputeNova()
	r := newNovaTestReconciler(nova, remoteTransportSecret(testRemoteTransportURL))

	_, err := r.reconcileComputeConfig(context.Background(), r.Client, nova,
		testComputeTransportURL, computeSecretValues())
	g.Expect(err).NotTo(HaveOccurred())
	published := publishedRemoteComputeConfig(t, r.Client, nova)

	nova.Spec.RemoteCompute.KeystoneEndpoint = "https://k\n[workarounds]\ndisable_rootwrap = true"
	result, err := r.reconcileComputeConfig(context.Background(), r.Client, nova,
		testComputeTransportURL, computeSecretValues())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue())
	live := publishedRemoteComputeConfig(t, r.Client, nova)
	g.Expect(live.Data).To(Equal(published.Data), "the injected section must never reach the Secret")
	expectComputeConfigCondition(g, nova, metav1.ConditionFalse, conditionReasonComputeConfigError)
}
