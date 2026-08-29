// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Byte-identity pin for the two ConfigMaps the node step applies. The goldens
// below are FULL-OBJECT YAML captured from the builders as they stand today, so
// any refactor of the node projection has to reproduce every rendered byte.
//
// The chassis pod has no API client, so these two objects are the whole channel
// between the operator and a node: a silent change to a rendered key, to the
// order of the bridge mappings, or to a line of a script is a change to what
// runs on every node the CR selects.
package controller

import (
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// The identities the two pinned nodes are seeded with. Seeding them through the
// live ConfigMap is what makes the golden reproducible: an unseeded node is
// handed a fresh UUID on every run.
const (
	pinSystemIDNodeA = "aaaaaaaa-1111-2222-3333-444444444444"
	pinSystemIDNodeB = "bbbbbbbb-1111-2222-3333-444444444444"
)

// pinNodesChassis is the fixture with both the gateway selector and the bridge
// mappings set, so the golden covers every field an entry can carry.
func pinNodesChassis() *ovnv1alpha1.OVNChassis {
	cr := testOVNChassis()
	cr.Spec.Gateway = &ovnv1alpha1.OVNGatewaySpec{
		NodeSelector: map[string]string{testGatewayNodeLabel: "true"},
	}
	cr.Spec.BridgeMappings = []ovnv1alpha1.OVNBridgeMapping{
		{PhysicalNetwork: "physnet1", Bridge: "br-ex"},
		{PhysicalNetwork: "physnet2", Bridge: "br-data"},
	}
	return cr
}

const pinNodesConfigMapGolden = `data:
  node-a: |
    SYSTEM_ID=aaaaaaaa-1111-2222-3333-444444444444
    GATEWAY=false
    BRIDGE_MAPPINGS=physnet1:br-ex,physnet2:br-data
    ENCAP_TYPE=geneve
  node-b: |
    SYSTEM_ID=bbbbbbbb-1111-2222-3333-444444444444
    GATEWAY=true
    BRIDGE_MAPPINGS=physnet1:br-ex,physnet2:br-data
    ENCAP_TYPE=geneve
metadata:
  labels:
    app.kubernetes.io/component: nodes
    app.kubernetes.io/instance: chassis
    app.kubernetes.io/managed-by: ovnchassis-operator
    app.kubernetes.io/name: ovnchassis
  name: chassis-nodes
  namespace: openstack
`

const pinChassisScriptsConfigMapGolden = `data:
  apply-node.sh: |
    #!/bin/bash
    set -eu
    f="/etc/ovn-chassis/nodes/${NODE_NAME}"
    until [ -f "$f" ]; do sleep 5; done
    # shellcheck source=/dev/null
    . "$f"
    until ovs-vsctl --timeout=5 --no-wait show >/dev/null 2>&1; do sleep 2; done
    ovs-vsctl --timeout=15 set open . \
      external_ids:system-id="${SYSTEM_ID}" external_ids:hostname="${NODE_NAME}" \
      external_ids:ovn-encap-type="${ENCAP_TYPE}" external_ids:ovn-encap-ip="${NODE_IP}" \
      external_ids:ovn-remote="${OVN_REMOTE}" external_ids:ovn-remote-probe-interval="${OVN_REMOTE_PROBE_INTERVAL_MS}"
    if [ -n "${BRIDGE_MAPPINGS}" ]; then ovs-vsctl --timeout=15 set open . external_ids:ovn-bridge-mappings="${BRIDGE_MAPPINGS}"
    else ovs-vsctl --timeout=15 remove open . external_ids ovn-bridge-mappings; fi
    if [ "${GATEWAY}" = "true" ]; then ovs-vsctl --timeout=15 set open . external_ids:ovn-cms-options=enable-chassis-as-gw
    else ovs-vsctl --timeout=15 remove open . external_ids ovn-cms-options; fi
    for m in ${BRIDGE_MAPPINGS//,/ }; do ovs-vsctl --timeout=15 --may-exist add-br "${m#*:}"; done
  chassis-del.sh: |
    #!/bin/bash
    set -eu
    ovn-sbctl --db="${SB_ADDR}" -p /etc/ovn/tls/tls.key -c /etc/ovn/tls/tls.crt -C /etc/ovn/tls/ca.crt --timeout=30 --if-exists chassis-del "$CHASSIS"
  evacuate.sh: |
    #!/bin/bash
    set -eu
    T="--db=${NB_ADDR} -p /etc/ovn/tls/tls.key -c /etc/ovn/tls/tls.crt -C /etc/ovn/tls/ca.crt --timeout=30"
    args=()
    mapfile -t lrps < <(ovn-nbctl $T --bare --columns=name find Logical_Router_Port)
    for lrp in "${lrps[@]}"; do args+=(-- --if-exists lrp-del-gateway-chassis "$lrp" "$CHASSIS"); done
    mapfile -t grps < <(ovn-nbctl $T --bare --columns=name find HA_Chassis_Group)
    for grp in "${grps[@]}"; do args+=(-- --if-exists ha-chassis-group-remove-chassis "$grp" "$CHASSIS"); done
    if [ ${#args[@]} -gt 0 ]; then ovn-nbctl $T "${args[@]}"; fi
  host-prepare.sh: |
    #!/bin/bash
    set -eu
    modprobe openvswitch && modprobe geneve
    install -d -m 0775 -o 42424 -g 42424 /run/openvswitch /run/ovn
    schema=/usr/share/openvswitch/vswitch.ovsschema
    [ -f /run/openvswitch/conf.db ] || ovsdb-tool create /run/openvswitch/conf.db "$schema"
    if [ "$(ovsdb-tool needs-conversion /run/openvswitch/conf.db "$schema")" = yes ]; then
      ovsdb-tool convert /run/openvswitch/conf.db "$schema"
    fi
    chown 42424:42424 /run/openvswitch/conf.db
    if [ -e /run/openvswitch/.conf.db.~lock~ ]; then
      chown 42424:42424 /run/openvswitch/.conf.db.~lock~
    fi
  run-ovsdb.sh: |
    #!/bin/bash
    set -eu
    umask 002
    exec ovsdb-server /run/openvswitch/conf.db \
      --remote=punix:/run/openvswitch/db.sock \
      --remote=db:Open_vSwitch,Open_vSwitch,manager_options \
      --pidfile=/run/openvswitch/ovsdb-server.pid \
      --unixctl=/run/openvswitch/ovsdb-server.ctl
  run-vswitchd.sh: |
    #!/bin/bash
    set -eu
    until ovs-vsctl --timeout=5 --no-wait show >/dev/null 2>&1; do sleep 1; done
    ovs-vsctl --no-wait init
    exec ovs-vswitchd unix:/run/openvswitch/db.sock --pidfile=/run/openvswitch/ovs-vswitchd.pid --unixctl=/run/openvswitch/ovs-vswitchd.ctl
metadata:
  labels:
    app.kubernetes.io/component: scripts
    app.kubernetes.io/instance: chassis
    app.kubernetes.io/managed-by: ovnchassis-operator
    app.kubernetes.io/name: ovnchassis
  name: chassis-chassis-scripts
  namespace: openstack
`

// TestPinNodesConfigMap pins the ConfigMap the chassis pods mount: two selected
// nodes, one of them a gateway, both carrying the same bridge mappings.
func TestPinNodesConfigMap(t *testing.T) {
	g := NewWithT(t)
	cr := pinNodesChassis()

	nodes := []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: testNodeA, Labels: selectedLabels()}},
		{ObjectMeta: metav1.ObjectMeta{Name: testNodeB, Labels: gatewayLabels()}},
	}
	live := map[string]nodeEntry{
		testNodeA: {systemID: pinSystemIDNodeA},
		testNodeB: {systemID: pinSystemIDNodeB},
	}

	got, err := yaml.Marshal(nodesConfigMap(cr, renderNodeEntries(cr, nodes, live)))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(string(got)).To(Equal(pinNodesConfigMapGolden),
		"the rendered nodes ConfigMap must stay byte-identical")
}

// TestPinChassisScriptsConfigMap pins the scripts the chassis containers and the
// maintenance Jobs run. They configure the local Open vSwitch database and edit
// the two OVN databases, so a changed line changes what happens on a node.
func TestPinChassisScriptsConfigMap(t *testing.T) {
	g := NewWithT(t)

	got, err := yaml.Marshal(chassisScriptsConfigMap(pinNodesChassis()))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(string(got)).To(Equal(pinChassisScriptsConfigMapGolden),
		"the rendered chassis scripts must stay byte-identical")
}
