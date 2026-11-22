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
    # collect <ref table> <owner table> <column> appends one removal for every owner
    # row whose <column> lists a <ref table> row naming this chassis.
    collect() {
      local reftable="$1" owner="$2" column="$3" uuid row refs ids rows
      local -A mine=()
      ids="$(ovn-nbctl $T --bare --columns=_uuid find "$reftable" chassis_name="$CHASSIS")"
      # --bare separates records with a blank line, so the empties are dropped here.
      while read -r uuid; do if [ -n "$uuid" ]; then mine["$uuid"]=1; fi; done <<< "$ids"
      if [ ${#mine[@]} -eq 0 ]; then return 0; fi
      # The owner row is named by its _uuid: read does no CSV unquoting, and a name
      # holding a comma would be split across the two fields. remove takes a UUID.
      rows="$(ovn-nbctl $T --format=csv --no-headings --columns=_uuid,"$column" list "$owner")"
      while IFS=, read -r row refs; do
        for uuid in ${refs//[\"\[\],]/ }; do
          # --if-exists so a row deleted between the listing and the write costs
          # only its own removal rather than every removal in the invocation.
          if [ -n "${mine[$uuid]:-}" ]; then args+=(-- --if-exists remove "$owner" "$row" "$column" "$uuid"); fi
        done
      done <<< "$rows"
    }
    collect Gateway_Chassis Logical_Router_Port gateway_chassis
    collect HA_Chassis HA_Chassis_Group ha_chassis
    if [ ${#args[@]} -gt 0 ]; then ovn-nbctl $T "${args[@]}"; fi
    # The drain is confirmed against the database rather than against the write: an
    # empty batch and an --if-exists removal that resolved no record both exit 0.
    for table in Gateway_Chassis HA_Chassis; do
      left="$(ovn-nbctl $T --bare --columns=_uuid find "$table" chassis_name="$CHASSIS")"
      if [ -n "${left//[[:space:]]/}" ]; then
        echo "evacuation incomplete: a $table row still names $CHASSIS" >&2
        exit 1
      fi
    done
  fabric-node.sh: |
    #!/bin/bash
    set -eu
    f="/etc/ovn-chassis/nodes/${NODE_NAME}"
    until [ -f "$f" ]; do sleep 5; done
    FABRIC_VRF="" FABRIC_TABLE="" FABRIC_MTU="" FABRIC_UPLINKS="" FABRIC_LOOPBACK_V4="" FABRIC_LOOPBACK_V6=""
    # shellcheck source=/dev/null
    . "$f"
    until ovs-vsctl --timeout=5 --no-wait show >/dev/null 2>&1; do sleep 2; done
    V="ovs-vsctl --timeout=15"
    for u in ${FABRIC_UPLINKS//,/ }; do
      nic="${u%%:*}" br="${u#*:}"
      if ! ip link show dev "$nic" >/dev/null 2>&1; then
        echo "ERROR: fabric uplink $nic does not exist on ${NODE_NAME}" >&2; exit 1
      fi
      if [ -n "$(ip -o addr show dev "$nic" scope global)" ]; then
        echo "ERROR: fabric uplink $nic carries a global address on ${NODE_NAME}; a fabric uplink carries link-local addresses only" >&2; exit 1
      fi
      # The device is asked for first: the bridge row exists before its netdev.
      if ip link show dev "$br" >/dev/null 2>&1 && ! $V br-exists "$br"; then
        echo "ERROR: $br exists on ${NODE_NAME} and is not an OVS bridge; a peer bridge takes a name no other device of the node carries" >&2; exit 1
      fi
    done
    if [ -n "$FABRIC_VRF" ] && ip link show dev "$FABRIC_VRF" >/dev/null 2>&1; then
      if [ "$($V --if-exists get open . external_ids:c5c3-fabric-vrf | tr -d '"')" != "$FABRIC_VRF" ] ||
         ! ip -d link show dev "$FABRIC_VRF" | grep -q ' vrf table '; then
        echo "ERROR: ${FABRIC_VRF} exists on ${NODE_NAME} and is not the VRF the fabric script created" >&2; exit 1
      fi
    fi
    want_bridges=" "
    for u in ${FABRIC_UPLINKS//,/ }; do want_bridges="${want_bridges}${u#*:} "; done
    # Bridges this script created earlier and the entry no longer names.
    for br in $($V --bare --columns=name find bridge external_ids:c5c3-fabric=true); do
      case "$want_bridges" in *" $br "*) ;; *) $V --if-exists del-br "$br" ;; esac
    done
    # Uplink ports this script added earlier that the entry no longer pairs with
    # the bridge they are on.
    for p in $($V --bare --columns=name find port external_ids:c5c3-fabric=true); do
      case " ${FABRIC_UPLINKS//,/ } " in *" ${p}:$($V port-to-br "$p") "*) ;; *) $V --if-exists del-port "$p" ;; esac
    done
    # The VRF this script created earlier, when the entry names another or none.
    old_vrf="$($V --if-exists get open . external_ids:c5c3-fabric-vrf | tr -d '"')"
    if [ -n "$old_vrf" ] && [ "$old_vrf" != "$FABRIC_VRF" ]; then
      ip link del "$old_vrf" 2>/dev/null || true
      $V remove open . external_ids c5c3-fabric-vrf
    fi
    if [ -z "$FABRIC_VRF" ]; then
      $V remove open . external_ids dynamic-routing-port-mapping
      exit 0
    fi
    # The guard left only the recorded VRF under this name. A concurrent run may
    # have re-created it on the new table already, so the delete may find nothing.
    if ip link show dev "$FABRIC_VRF" >/dev/null 2>&1 &&
       ! ip -d link show dev "$FABRIC_VRF" | grep -q "vrf table ${FABRIC_TABLE} "; then
      ip link del "$FABRIC_VRF" 2>/dev/null || true
    fi
    if ! ip link show dev "$FABRIC_VRF" >/dev/null 2>&1; then
      $V set open . external_ids:c5c3-fabric-vrf="$FABRIC_VRF"
      if ! ip link add "$FABRIC_VRF" type vrf table "$FABRIC_TABLE" &&
         ! ip -d link show dev "$FABRIC_VRF" 2>/dev/null | grep -q "vrf table ${FABRIC_TABLE} "; then
        echo "ERROR: cannot create the VRF device ${FABRIC_VRF} on ${NODE_NAME}; the node kernel has no vrf module" >&2; exit 1
      fi
    fi
    ip link set "$FABRIC_VRF" up
    for a in $(ip -o addr show dev "$FABRIC_VRF" scope global | awk '{print $4}'); do
      case "$a" in "${FABRIC_LOOPBACK_V4}/32"|"${FABRIC_LOOPBACK_V6}/128") ;; *) ip addr del "$a" dev "$FABRIC_VRF" 2>/dev/null || true ;; esac
    done
    ip addr replace "${FABRIC_LOOPBACK_V4}/32" dev "$FABRIC_VRF"
    ip -6 addr replace "${FABRIC_LOOPBACK_V6}/128" dev "$FABRIC_VRF"
    mapping=""
    for u in ${FABRIC_UPLINKS//,/ }; do
      nic="${u%%:*}" br="${u#*:}"
      $V --may-exist add-br "$br" -- set bridge "$br" external_ids:c5c3-fabric=true \
        -- --may-exist add-port "$br" "$nic" -- set port "$nic" external_ids:c5c3-fabric=true \
        -- set interface "$br" mtu_request="$FABRIC_MTU"
      n=0
      until [ -e "/proc/sys/net/ipv6/conf/${br}/accept_ra" ]; do
        n=$((n+1))
        if [ "$n" -gt 30 ]; then
          echo "ERROR: peer bridge $br did not appear on ${NODE_NAME}" >&2; exit 1
        fi
        sleep 1
      done
      echo 0 > "/proc/sys/net/ipv6/conf/${nic}/accept_ra"
      echo 1 > "/proc/sys/net/ipv6/conf/${nic}/disable_ipv6"
      echo 0 > "/proc/sys/net/ipv6/conf/${br}/accept_ra"
      ip link set "$nic" mtu "$FABRIC_MTU"
      ip link set "$br" master "$FABRIC_VRF"
      ip link set "$nic" up
      ip link set "$br" up
      mapping="${mapping:+${mapping},}${br}=${br}"
    done
    $V set open . external_ids:dynamic-routing-port-mapping="$mapping"
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
  route-probe.sh: |
    #!/bin/bash
    set -eu
    T="--db=${SB_ADDR} -p /etc/ovn/tls/tls.key -c /etc/ovn/tls/tls.crt -C /etc/ovn/tls/ca.crt --timeout=30"
    adv="$(ovn-sbctl $T --bare --columns=_uuid list Advertised_Route)"
    lrn="$(ovn-sbctl $T --bare --columns=_uuid list Learned_Route)"
    printf 'advertised=%s learned=%s\n' "$(grep -c . <<< "$adv" || true)" "$(grep -c . <<< "$lrn" || true)" > /dev/termination-log
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
    if [ -n "${OVS_REVALIDATOR_THREADS:-}" ]; then
      ovs-vsctl --no-wait set open . other_config:n-revalidator-threads="${OVS_REVALIDATOR_THREADS}"
    fi
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

const pinFabricNodesConfigMapGolden = `data:
  node-a: |
    SYSTEM_ID=aaaaaaaa-1111-2222-3333-444444444444
    GATEWAY=false
    BRIDGE_MAPPINGS=physnet1:br-ex,physnet2:br-data
    ENCAP_TYPE=geneve
    FABRIC_VRF=fabric
    FABRIC_TABLE=100
    FABRIC_MTU=9000
    FABRIC_UPLINKS=fab1:br-fab1,fab2:br-fab2
    FABRIC_LOOPBACK_V4=192.0.2.1
    FABRIC_LOOPBACK_V6=2001:db8::1
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

// TestPinNodesConfigMapFabric pins the nodes ConfigMap of a fabric chassis: the
// node with a row in spec.fabric.nodes carries the six FABRIC_ keys after
// ENCAP_TYPE, and the node without one renders exactly as on a chassis without
// the block.
func TestPinNodesConfigMapFabric(t *testing.T) {
	g := NewWithT(t)
	cr := pinNodesChassis()
	cr.Spec.Fabric = testFabric()

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
	g.Expect(string(got)).To(Equal(pinFabricNodesConfigMapGolden),
		"the rendered nodes ConfigMap of a fabric chassis must stay byte-identical")
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
