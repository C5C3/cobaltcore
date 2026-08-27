// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Byte-identity pins for the two files reconcileConfig renders. The goldens
// below are the FULL files as the renderer produces them today, so a changed
// key, a dropped default, or a section routed to the other file surfaces here as
// a diff instead of as a silent ConfigMap rotation that rolls every Neutron
// Deployment in the fleet on an operator upgrade.
package controller

import (
	"testing"

	. "github.com/onsi/gomega"
)

// pinNeutronConfGolden is the neutron.conf rendered for the neutronForConfig
// fixture. [nova] carries no key and is still emitted: neutron reads the section
// for its Nova notification credentials, and an operator filling it in through
// spec.extraConfig should find the section already there.
const pinNeutronConfGolden = `[DEFAULT]
api_paste_config = /var/lib/openstack/etc/neutron/api-paste.ini
auth_strategy = keystone
core_plugin = ml2
debug = false
dhcp_agent_notification = false
notify_nova_on_port_data_changes = false
notify_nova_on_port_status_changes = false
rpc_state_report_workers = 0
rpc_workers = 0
service_plugins = ovn-router
state_path = /var/lib/neutron
use_stderr = true

[database]
connection = mysql+pymysql://placeholder

[keystone_authtoken]
auth_type = password
auth_url = http://keystone.openstack.svc:5000
memcached_servers = mc:11211
project_domain_name = Default
project_name = service
region_name = RegionOne
user_domain_name = Default
username = neutron
www_authenticate_uri = http://keystone.openstack.svc:5000

[nova]

[oslo_concurrency]
lock_path = /var/lib/neutron/lock

[oslo_messaging_notifications]
driver = noop

[oslo_messaging_rabbit]
rabbit_quorum_queue = true
rabbit_transient_quorum_queue = true
use_queue_manager = true
`

// pinML2ConfGolden is the ml2_conf.ini rendered for the same fixture. Five of
// the twelve ml2Sections appear: the operator defaults fill those, and the
// remaining seven are routing entries that render only once spec.extraConfig
// puts a key under them.
const pinML2ConfGolden = `[ml2]
extension_drivers = port_security
mechanism_drivers = ovn
tenant_network_types = geneve
type_drivers = geneve,flat

[ml2_type_flat]
flat_networks = *

[ml2_type_geneve]
max_header_size = 38
vni_ranges = 1:65536

[ovn]
ovn_l3_scheduler = leastloaded
ovn_metadata_enabled = true
ovn_nb_ca_cert = /etc/ovn/tls/ca.crt
ovn_nb_certificate = /etc/ovn/tls/tls.crt
ovn_nb_connection = ssl:10.96.0.11:6641
ovn_nb_private_key = /etc/ovn/tls/tls.key
ovn_sb_ca_cert = /etc/ovn/tls/ca.crt
ovn_sb_certificate = /etc/ovn/tls/tls.crt
ovn_sb_connection = ssl:10.96.0.21:6642
ovn_sb_private_key = /etc/ovn/tls/tls.key

[securitygroup]
enable_security_group = true
`

// TestPinNeutronConf_ReleasesRenderIdentically pins both rendered files for
// 2025.2 and 2026.1 against the goldens and asserts the two renders are
// byte-identical. The identity is the point: a release bump must not rotate the
// ConfigMap, so upgrading a Neutron never rolls its pods for a config change
// that is not there.
func TestPinNeutronConf_ReleasesRenderIdentically(t *testing.T) {
	renderFor := func(t *testing.T, release string) map[string]string {
		t.Helper()
		neutron := neutronForConfig()
		neutron.Spec.OpenStackRelease = release
		r, name := renderConfig(t, neutron)
		return renderedConfigMap(t, r, name).Data
	}

	rendered := make(map[string]map[string]string, 2)
	for _, release := range []string{"2025.2", "2026.1"} {
		t.Run(release, func(t *testing.T) {
			g := NewGomegaWithT(t)
			data := renderFor(t, release)
			rendered[release] = data
			g.Expect(data[neutronConfDataKey]).To(Equal(pinNeutronConfGolden))
			g.Expect(data[ml2ConfDataKey]).To(Equal(pinML2ConfGolden))
		})
	}

	t.Run("2025.2 and 2026.1 render identically", func(t *testing.T) {
		g := NewGomegaWithT(t)
		g.Expect(rendered["2026.1"]).To(Equal(rendered["2025.2"]))
	})
}

// TestOperatorDefaults_IsAPureFunction pins that the defaults depend on the spec
// and the resolved endpoints alone: the golden above is asserted through the
// cluster-writing step, and this is the same map without one.
func TestOperatorDefaults_IsAPureFunction(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := neutronForConfig()

	defaults := operatorDefaults(neutron, resolvedForConfig())
	neutronConf, ml2Conf := splitML2Sections(defaults)

	g.Expect(neutronConf).To(HaveKey("DEFAULT"))
	g.Expect(ml2Conf).To(HaveLen(5))
	g.Expect(defaults["ovn"]["ovn_nb_connection"]).To(Equal(testNorthboundAddress))
	// Nothing in the renderer reaches the API server, so a second call over the
	// same inputs produces the same map.
	g.Expect(operatorDefaults(neutron, resolvedForConfig())).To(Equal(defaults))
}
