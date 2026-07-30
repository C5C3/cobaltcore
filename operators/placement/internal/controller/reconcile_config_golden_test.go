// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Byte-identity pin for the placement.conf reconcileConfig renders. The golden
// below is the FULL file as the renderer produces it today, so a changed key, a
// dropped default, or a reordered section surfaces here as a diff instead of as
// a silent ConfigMap rotation that rolls every Placement Deployment in the fleet
// on an operator upgrade.
package controller

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
)

// pinPlacementConfGolden is the placement.conf rendered for the
// placementForConfig fixture. It is pinned for BOTH supported releases: nothing
// in the renderer branches on spec.openStackRelease, because placement has
// shipped the same WSGI application and the same option names across them.
const pinPlacementConfGolden = `[DEFAULT]
debug = false
use_stderr = true

[api]
auth_strategy = keystone

[keystone_authtoken]
auth_type = password
auth_url = http://keystone.default.svc:5000
memcached_servers = mc:11211
project_domain_name = Default
project_name = service
region_name = RegionOne
user_domain_name = Default
username = placement
www_authenticate_uri = http://keystone.default.svc:5000

[placement_database]
connection = mysql+pymysql://placeholder
connection_recycle_time = 600
max_retries = -1
`

// TestPinPlacementConf_ReleasesRenderIdentically pins the rendered
// placement.conf for 2025.2 and 2026.1 against the golden and asserts the two
// renders are byte-identical. The identity is the point: a release bump must not
// rotate the ConfigMap, so upgrading a Placement never rolls its pods for a
// config change that is not there.
func TestPinPlacementConf_ReleasesRenderIdentically(t *testing.T) {
	renderFor := func(t *testing.T, release string) string {
		t.Helper()
		g := NewGomegaWithT(t)
		placement := placementForConfig()
		placement.Spec.OpenStackRelease = release
		r := newPlacementTestReconciler(placement)

		_, name, err := r.reconcileConfig(context.Background(), placement)
		g.Expect(err).NotTo(HaveOccurred())
		return renderedConfig(t, r, name)
	}

	rendered := make(map[string]string, 2)
	for _, release := range []string{"2025.2", "2026.1"} {
		t.Run(release, func(t *testing.T) {
			g := NewGomegaWithT(t)
			conf := renderFor(t, release)
			rendered[release] = conf
			g.Expect(conf).To(Equal(pinPlacementConfGolden))
		})
	}

	t.Run("2025.2 and 2026.1 render identically", func(t *testing.T) {
		g := NewGomegaWithT(t)
		g.Expect(rendered["2026.1"]).To(Equal(rendered["2025.2"]))
	})
}
