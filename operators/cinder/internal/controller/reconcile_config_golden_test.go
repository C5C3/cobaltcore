// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Byte-identity pins for the files reconcileConfig renders. The goldens below
// are the FULL documents as the renderer produces them today, so a changed key,
// a dropped default, or a section that starts rendering conditionally surfaces
// here as a diff instead of as a silent ConfigMap rotation that rolls every
// Cinder Deployment in the fleet on an operator upgrade.
//
// The "capabilities = " lines end in a space on purpose: that is how oslo spells
// an empty value, and the privsep contexts depend on it. Do not trim it.
package controller

import (
	"testing"

	cinderv1alpha1 "github.com/c5c3/cobaltcore/operators/cinder/api/v1alpha1"
	. "github.com/onsi/gomega"
)

// pinCinderConfKeystoneGolden is the cinder.conf rendered for the Keystone-backed
// cinderForConfig fixture.
const pinCinderConfKeystoneGolden = `[DEFAULT]
api_paste_config = /var/lib/openstack/etc/cinder/api-paste.ini
auth_strategy = keystone
debug = false
host = cinder
image_conversion_dir = /var/lib/cinder/conversion
resource_query_filters_file = /var/lib/openstack/etc/cinder/resource_filters.json
state_path = /var/lib/cinder
use_stderr = true

[cinder_sys_admin]
capabilities = 
helper_command = privsep-helper --config-dir /etc/cinder/cinder.conf.d

[coordination]
backend_url = file:///var/lib/cinder/coordination

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
username = cinder
www_authenticate_uri = http://keystone.openstack.svc:5000

[oslo_concurrency]
lock_path = /var/lib/cinder/tmp

[oslo_messaging_notifications]
driver = noop

[oslo_messaging_rabbit]
rabbit_quorum_queue = true
rabbit_transient_quorum_queue = true
use_queue_manager = true

[privsep_osbrick]
capabilities = 
helper_command = privsep-helper --config-dir /etc/cinder/cinder.conf.d

[service_user]
auth_type = password
auth_url = http://keystone.openstack.svc:5000
project_domain_name = Default
project_name = service
region_name = RegionOne
send_service_user_token = true
user_domain_name = Default
username = cinder
`

// pinCinderConfNoauthGolden is the cinder.conf rendered for the Keystone-free
// fixture: the same file minus the two identity sections, with the pipeline on
// noauth. The privsep contexts stay, because the volume services need their
// privileged helper whether or not an identity service exists.
const pinCinderConfNoauthGolden = `[DEFAULT]
api_paste_config = /var/lib/openstack/etc/cinder/api-paste.ini
auth_strategy = noauth
debug = false
host = cinder
image_conversion_dir = /var/lib/cinder/conversion
resource_query_filters_file = /var/lib/openstack/etc/cinder/resource_filters.json
state_path = /var/lib/cinder
use_stderr = true

[cinder_sys_admin]
capabilities = 
helper_command = privsep-helper --config-dir /etc/cinder/cinder.conf.d

[coordination]
backend_url = file:///var/lib/cinder/coordination

[database]
connection = mysql+pymysql://placeholder

[oslo_concurrency]
lock_path = /var/lib/cinder/tmp

[oslo_messaging_notifications]
driver = noop

[oslo_messaging_rabbit]
rabbit_quorum_queue = true
rabbit_transient_quorum_queue = true
use_queue_manager = true

[privsep_osbrick]
capabilities = 
helper_command = privsep-helper --config-dir /etc/cinder/cinder.conf.d
`

// TestPinCinderConf_ReleasesRenderIdentically pins the rendered cinder.conf of
// both deployment shapes at both supported releases and asserts the two renders
// are byte-identical. The identity is the point: a release bump must not rotate
// the ConfigMap, so upgrading a Cinder never rolls its pods for a config change
// that is not there.
func TestPinCinderConf_ReleasesRenderIdentically(t *testing.T) {
	cases := map[string]struct {
		fixture func() *cinderv1alpha1.Cinder
		golden  string
	}{
		"keystone": {fixture: cinderForConfig, golden: pinCinderConfKeystoneGolden},
		"noauth":   {fixture: keystoneFreeCinder, golden: pinCinderConfNoauthGolden},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rendered := make(map[string]string, 2)
			for _, openStackRelease := range []string{"2025.2", "2026.1"} {
				t.Run(openStackRelease, func(t *testing.T) {
					g := NewGomegaWithT(t)
					cinder := tc.fixture()
					cinder.Spec.OpenStackRelease = openStackRelease
					cinder.Spec.Image.Tag = openStackRelease
					conf := renderCinderConf(t, cinder)
					rendered[openStackRelease] = conf
					g.Expect(conf).To(Equal(tc.golden))
				})
			}
			g := NewGomegaWithT(t)
			g.Expect(rendered["2026.1"]).To(Equal(rendered["2025.2"]))
		})
	}
}

// TestPinSchedulerConf pins the scheduler overlay, which is assembled by string
// concatenation rather than by the INI renderer and therefore has no sorting to
// fall back on.
func TestPinSchedulerConf(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := cinderForConfig()
	r, art := renderConfig(t, cinder)

	g.Expect(renderedConfigMap(t, r, art.configMapName).Data[schedulerConfDataKey]).
		To(Equal("[DEFAULT]\nhost = cinder-scheduler\n"))
}

// TestOperatorDefaults_IsAPureFunction pins that the defaults depend on the spec
// alone: the goldens above are asserted through the cluster-writing step, and
// this is the same map without one. The CR is compared against a copy taken
// before the call, because a renderer that wrote back into the spec it reads
// would make every second pass render something else.
func TestOperatorDefaults_IsAPureFunction(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := cinderForConfig()
	before := cinder.DeepCopy()

	defaults := operatorDefaults(cinder)

	g.Expect(defaults).To(HaveKey("DEFAULT"))
	g.Expect(operatorDefaults(cinder)).To(Equal(defaults),
		"nothing in the renderer reaches the API server, so a second call over the same input repeats itself")
	g.Expect(cinder).To(Equal(before), "operatorDefaults must not write back into the spec it reads")
}
