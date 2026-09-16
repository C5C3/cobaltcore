// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Byte-identity pins for the files reconcileConfig renders. The goldens below
// are the FULL documents as the renderer produces them today, so a changed key,
// a dropped default, or a section that starts rendering conditionally surfaces
// here as a diff instead of as a silent ConfigMap rotation that rolls every Nova
// workload in the fleet on an operator upgrade.
//
// Several lines of the minimal golden end in a space: novaMinimal bypasses the
// defaulting webhook, so its service-user fields are empty, and that is how oslo
// spells an empty value. Do not trim them.
package controller

import (
	"strings"
	"testing"

	. "github.com/onsi/gomega"

	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
)

// pinNovaConfGolden is the nova.conf rendered for the validNova fixture: both
// optional siblings on, a console proxy behind a gateway, a region, and json
// logging.
const pinNovaConfGolden = `[DEFAULT]
debug = false
log_config_append = /etc/nova/nova.conf.d/logging.ini
state_path = /var/lib/nova
use_stderr = true

[api]
local_metadata_per_cell = false

[api_database]
connection = mysql+pymysql://placeholder

[barbican]
auth_endpoint = http://keystone.openstack.svc.cluster.local:5000
barbican_endpoint_type = internal
barbican_region_name = RegionOne
send_service_user_token = true

[cache]
backend = dogpile.cache.pymemcache
enabled = true
memcache_servers = memcached:11211

[cinder]
auth_type = password
auth_url = http://keystone.openstack.svc.cluster.local:5000
catalog_info = block-storage:cinder:internalURL
os_region_name = RegionOne
project_domain_name = Default
project_name = service
user_domain_name = Default
username = nova

[database]
connection = mysql+pymysql://placeholder

[glance]
region_name = RegionOne
valid_interfaces = internal

[key_manager]
backend = barbican

[keystone_authtoken]
auth_type = password
auth_url = http://keystone.openstack.svc.cluster.local:5000
memcached_servers = memcached:11211
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

[oslo_concurrency]
lock_path = /var/lib/nova/tmp

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

[scheduler]
discover_hosts_in_cells_interval = 300

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

// pinNovaConfMinimalGolden is the nova.conf rendered for the novaMinimal
// fixture: no [cinder], no [key_manager] and no [barbican], no region anywhere,
// no log_config_append, and a console URL naming the cluster-local Service.
const pinNovaConfMinimalGolden = `[DEFAULT]
debug = false
state_path = /var/lib/nova
use_stderr = true

[api]
local_metadata_per_cell = false

[api_database]
connection = mysql+pymysql://placeholder

[cache]
backend = dogpile.cache.pymemcache
enabled = true
memcache_servers = memcached:11211

[database]
connection = mysql+pymysql://placeholder

[glance]
valid_interfaces = internal

[keystone_authtoken]
auth_type = password
auth_url = http://keystone.openstack.svc.cluster.local:5000
memcached_servers = memcached:11211
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

[oslo_concurrency]
lock_path = /var/lib/nova/tmp

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

[scheduler]
discover_hosts_in_cells_interval = 300

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

// TestPinNovaConf_ReleasesRenderIdentically pins the rendered nova.conf of both
// fixtures at both supported releases and asserts the two renders are
// byte-identical. The identity is the point: a release bump must not rotate the
// ConfigMap, so upgrading a Nova never rolls its pods for a config change that
// is not there.
func TestPinNovaConf_ReleasesRenderIdentically(t *testing.T) {
	cases := map[string]struct {
		fixture func() *novav1alpha1.Nova
		golden  string
	}{
		"validNova":   {fixture: validNova, golden: pinNovaConfGolden},
		"novaMinimal": {fixture: novaMinimal, golden: pinNovaConfMinimalGolden},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rendered := make(map[string]string, 2)
			for _, openStackRelease := range []string{"2025.2", "2026.1"} {
				t.Run(openStackRelease, func(t *testing.T) {
					nova := tc.fixture()
					nova.Spec.OpenStackRelease = openStackRelease
					nova.Spec.Image.Tag = openStackRelease
					conf := renderNovaConf(t, nova)
					rendered[openStackRelease] = conf
					expectGolden(t, conf, tc.golden)
				})
			}
			expectGolden(t, rendered["2026.1"], rendered["2025.2"])
		})
	}
}

// expectGolden compares a rendered document against its golden and reports the
// first line the two disagree on, so a changed key is readable without diffing
// two full documents by eye.
func expectGolden(t *testing.T, rendered, golden string) {
	t.Helper()

	if rendered == golden {
		return
	}
	got := strings.Split(rendered, "\n")
	want := strings.Split(golden, "\n")
	for i := range max(len(got), len(want)) {
		gotLine, wantLine := "", ""
		if i < len(got) {
			gotLine = got[i]
		}
		if i < len(want) {
			wantLine = want[i]
		}
		if gotLine != wantLine {
			t.Fatalf("the rendered document differs from the golden at line %d:\n  got:  %q\n  want: %q",
				i+1, gotLine, wantLine)
		}
	}
}

// TestPinNovaOverlays pins the four role overlays, which are assembled by string
// formatting rather than by the INI renderer and therefore have no sorting to
// fall back on. Both worker counts are covered: the webhook default and a
// hand-set one.
func TestPinNovaOverlays(t *testing.T) {
	g := NewGomegaWithT(t)

	g.Expect(metadataOverlay).To(Equal("[neutron]\nservice_metadata_proxy = true\n"))
	g.Expect(novncproxyOverlay).To(Equal(
		"[DEFAULT]\nweb = /usr/share/novnc\n\n[vnc]\nnovncproxy_host = 0.0.0.0\nnovncproxy_port = 6080\n"))

	g.Expect(schedulerOverlay(2)).To(Equal("[scheduler]\nworkers = 2\n"))
	g.Expect(schedulerOverlay(5)).To(Equal("[scheduler]\nworkers = 5\n"))
	g.Expect(conductorOverlay(2)).To(Equal("[conductor]\nworkers = 2\n"))
	g.Expect(conductorOverlay(5)).To(Equal("[conductor]\nworkers = 5\n"))
}

// TestOperatorDefaults_IsAPureFunction pins that the defaults depend on the spec
// alone: the goldens above are asserted through the cluster-writing step, and
// this is the same map without one. The CR is compared against a copy taken
// before the call, because a renderer that wrote back into the spec it reads
// would make every second pass render something else.
func TestOperatorDefaults_IsAPureFunction(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	before := nova.DeepCopy()

	defaults := operatorDefaults(nova)

	g.Expect(defaults).To(HaveKey("DEFAULT"))
	g.Expect(operatorDefaults(nova)).To(Equal(defaults),
		"nothing in the renderer reaches the API server, so a second call over the same input repeats itself")
	g.Expect(nova).To(Equal(before), "operatorDefaults must not write back into the spec it reads")
}
