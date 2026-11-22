// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Byte-identity pin for the objects the Fabric step projects. The goldens below
// are FULL-OBJECT YAML captured from the builders as they stand today.
//
// The FRRConfiguration is what frr-k8s renders a node's BGP configuration from,
// so a silent change to a router, a neighbor or a line of the raw block is a
// change to what every fabric node announces and accepts. The route probe Job
// holds the client keypair of the central and talks to its Southbound
// database.
package controller

import (
	"testing"

	. "github.com/onsi/gomega"
	"sigs.k8s.io/yaml"

	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
)

const pinFRRConfigurationGolden = `apiVersion: frrk8s.metallb.io/v1beta1
kind: FRRConfiguration
metadata:
  annotations:
    ovn.openstack.c5c3.io/node: node-a
  labels:
    app.kubernetes.io/component: fabric
    app.kubernetes.io/instance: chassis
    app.kubernetes.io/managed-by: ovnchassis-operator
    app.kubernetes.io/name: ovnchassis
    openstack.c5c3.io/owner-kind: OVNChassis
    openstack.c5c3.io/owner-name: chassis
    openstack.c5c3.io/owner-namespace: openstack
  name: chassis-fabric-66570ff0
  namespace: frr-k8s-system
spec:
  bgp:
    bfdProfiles:
    - detectMultiplier: 3
      name: fabric
      receiveInterval: 300
      transmitInterval: 300
    routers:
    - asn: 4200000001
      id: 192.0.2.1
      imports:
      - vrf: ovnvrf42
      neighbors:
      - bfdProfile: fabric
        dynamicASN: external
        interface: br-fab1
        toAdvertise:
          allowed:
            mode: all
        toReceive:
          allowed:
            mode: filtered
            prefixes:
            - prefix: 0.0.0.0/0
            - prefix: ::/0
      - bfdProfile: fabric
        dynamicASN: external
        interface: br-fab2
        toAdvertise:
          allowed:
            mode: all
        toReceive:
          allowed:
            mode: filtered
            prefixes:
            - prefix: 0.0.0.0/0
            - prefix: ::/0
      prefixes:
      - 192.0.2.1/32
      - 2001:db8::1/128
      vrf: fabric
    - asn: 4200000001
      vrf: ovnvrf42
    - asn: 4200000001
      imports:
      - vrf: fabric
      vrf: ovnvrf10
  nodeSelector:
    matchLabels:
      kubernetes.io/hostname: node-a
  raw:
    priority: 10
    rawConfig: |
      router bgp 4200000001 vrf ovnvrf42
       address-family ipv4 unicast
        redistribute kernel
       exit-address-family
       address-family ipv6 unicast
        redistribute kernel
       exit-address-family
      exit
      ip prefix-list br-fab1-fabric-allowed-ipv4 seq 100 permit 198.51.100.0/24 ge 32
      ip prefix-list br-fab2-fabric-allowed-ipv4 seq 100 permit 198.51.100.0/24 ge 32
      ipv6 prefix-list br-fab1-fabric-allowed-ipv6 seq 100 permit 2001:db8::/64 ge 128
      ipv6 prefix-list br-fab2-fabric-allowed-ipv6 seq 100 permit 2001:db8::/64 ge 128
`

const pinRouteProbeJobGolden = `metadata:
  labels:
    app.kubernetes.io/component: maintenance
    app.kubernetes.io/instance: chassis
    app.kubernetes.io/managed-by: ovnchassis-operator
    app.kubernetes.io/name: ovnchassis
  name: chassis-route-probe
  namespace: openstack
spec:
  activeDeadlineSeconds: 300
  backoffLimit: 0
  template:
    metadata:
      labels:
        app.kubernetes.io/component: maintenance
        app.kubernetes.io/instance: chassis
        app.kubernetes.io/managed-by: ovnchassis-operator
        app.kubernetes.io/name: ovnchassis
    spec:
      containers:
      - command:
        - /bin/bash
        - /etc/ovn-chassis/bin/route-probe.sh
        env:
        - name: SB_ADDR
          value: ssl:10.96.0.21:6642
        image: ghcr.io/c5c3/ovn:26.03.2
        imagePullPolicy: Always
        name: maintenance
        resources:
          limits:
            memory: 368Mi
          requests:
            cpu: 70m
            memory: 368Mi
        securityContext:
          allowPrivilegeEscalation: false
          capabilities:
            drop:
            - ALL
          readOnlyRootFilesystem: true
          runAsGroup: 42424
          runAsNonRoot: true
          runAsUser: 42424
          seccompProfile:
            type: RuntimeDefault
        volumeMounts:
        - mountPath: /etc/ovn-chassis/bin
          name: scripts
        - mountPath: /tmp
          name: tmp
        - mountPath: /etc/ovn/tls
          name: tls
          readOnly: true
      restartPolicy: Never
      securityContext:
        seccompProfile:
          type: RuntimeDefault
      tolerations:
      - effect: NoSchedule
        key: openstack.c5c3.io/network
        operator: Exists
      volumes:
      - configMap:
          defaultMode: 365
          name: chassis-chassis-scripts
        name: scripts
      - emptyDir: {}
        name: tmp
      - name: tls
        secret:
          secretName: ovn-client
  ttlSecondsAfterFinished: 86400
status: {}
`

// TestPinRouteProbeJob pins the Job that counts the Southbound routes: the
// maintenance Job shape with the client Secret mounted, no node pin and no host
// network.
func TestPinRouteProbeJob(t *testing.T) {
	g := NewWithT(t)

	got, err := yaml.Marshal(routeProbeJob(pinMaintenanceChassis(), pinMaintenanceCentral))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(string(got)).To(Equal(pinRouteProbeJobGolden),
		"the rendered route probe Job must stay byte-identical")
}

// TestPinFRRConfiguration pins the FRRConfiguration of a fabric node with two
// uplinks, one announceable range per family, VRF id 42 advertised and VRF id
// 10 learned.
func TestPinFRRConfiguration(t *testing.T) {
	g := NewWithT(t)
	cr := fabricChassis()
	labels, err := commonmulticluster.OwnerLabels(newTestScheme(t), cr)
	g.Expect(err).NotTo(HaveOccurred())

	obj, err := buildFRRConfiguration(cr, testNodeA, fabricEntry(true), labels)
	g.Expect(err).NotTo(HaveOccurred())

	got, err := yaml.Marshal(obj.Object)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(string(got)).To(Equal(pinFRRConfigurationGolden),
		"the rendered FRRConfiguration must stay byte-identical")
}
