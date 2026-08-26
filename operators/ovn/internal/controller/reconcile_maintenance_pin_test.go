// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Byte-identity pin for the three maintenance Jobs. The goldens below are
// FULL-OBJECT YAML captured from the builders as they stand today, so any
// refactor of the maintenance projection has to reproduce every rendered byte.
//
// Each of the three writes to state no reconcile can read back: the local Open
// vSwitch database of one node, the gateway assignments of the logical model,
// and the Southbound registration of a chassis. A silent change to the node a
// Job is pinned to, to the database it addresses or to the chassis it names is
// a change to what happens to a running network.
package controller

import (
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// The first eight hex characters of the SHA-256 digest of the two node names,
// which is the suffix every maintenance Job name carries.
const (
	pinNodeAHash = "66570ff0"
	pinNodeBHash = "93ef37c6"
)

// pinMaintenanceChassis is the fixture the Job goldens are rendered from. It
// carries a toleration, because a networking node is commonly tainted and the
// Jobs have to reach it anyway.
func pinMaintenanceChassis() *ovnv1alpha1.OVNChassis {
	cr := testOVNChassis()
	cr.Spec.Tolerations = []corev1.Toleration{{
		Key:      "openstack.c5c3.io/network",
		Operator: corev1.TolerationOpExists,
		Effect:   corev1.TaintEffectNoSchedule,
	}}
	return cr
}

// pinMaintenanceCentral is the resolved OVNCentral the goldens are
// parameterised by. The remote is the relay address, which is what a chassis
// dials when the central runs one.
var pinMaintenanceCentral = resolvedCentral{
	ovnRemote:        "ssl:10.96.0.31:6642",
	nbAddress:        testNorthboundAddress,
	sbAddress:        testSouthboundAddress,
	clientSecretName: "ovn-client",
}

const pinApplyJobGolden = `metadata:
  labels:
    app.kubernetes.io/component: maintenance
    app.kubernetes.io/instance: chassis
    app.kubernetes.io/managed-by: ovnchassis-operator
    app.kubernetes.io/name: ovnchassis
  name: chassis-apply-66570ff0
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
        - /etc/ovn-chassis/bin/apply-node.sh
        env:
        - name: NODE_NAME
          valueFrom:
            fieldRef:
              fieldPath: spec.nodeName
        - name: NODE_IP
          valueFrom:
            fieldRef:
              fieldPath: status.hostIP
        - name: OVN_REMOTE
          value: ssl:10.96.0.31:6642
        - name: OVN_REMOTE_PROBE_INTERVAL_MS
          value: "60000"
        image: ghcr.io/c5c3/ovn:26.03.2
        name: maintenance
        resources: {}
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
        - mountPath: /run/openvswitch
          name: run-ovs
        - mountPath: /etc/ovn-chassis/nodes
          name: nodes
      dnsPolicy: ClusterFirstWithHostNet
      hostNetwork: true
      nodeName: node-a
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
      - hostPath:
          path: /run/openvswitch
          type: DirectoryOrCreate
        name: run-ovs
      - configMap:
          name: chassis-nodes
        name: nodes
  ttlSecondsAfterFinished: 86400
status: {}
`

const pinEvacuateJobGolden = `metadata:
  labels:
    app.kubernetes.io/component: maintenance
    app.kubernetes.io/instance: chassis
    app.kubernetes.io/managed-by: ovnchassis-operator
    app.kubernetes.io/name: ovnchassis
  name: chassis-evacuate-66570ff0
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
        - /etc/ovn-chassis/bin/evacuate.sh
        env:
        - name: NB_ADDR
          value: ssl:10.96.0.11:6641
        - name: CHASSIS
          value: 11111111-2222-3333-4444-555555555555
        image: ghcr.io/c5c3/ovn:26.03.2
        name: maintenance
        resources: {}
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

const pinChassisDelJobGolden = `metadata:
  labels:
    app.kubernetes.io/component: maintenance
    app.kubernetes.io/instance: chassis
    app.kubernetes.io/managed-by: ovnchassis-operator
    app.kubernetes.io/name: ovnchassis
  name: chassis-chassis-del-66570ff0
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
        - /etc/ovn-chassis/bin/chassis-del.sh
        env:
        - name: SB_ADDR
          value: ssl:10.96.0.21:6642
        - name: CHASSIS
          value: 11111111-2222-3333-4444-555555555555
        image: ghcr.io/c5c3/ovn:26.03.2
        name: maintenance
        resources: {}
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

// TestPinMaintenanceJobs pins the three Jobs the maintenance step projects.
func TestPinMaintenanceJobs(t *testing.T) {
	entry := nodeEntry{systemID: testFixedSystemID, encapType: "geneve"}

	cases := []struct {
		name   string
		build  func(cr *ovnv1alpha1.OVNChassis) any
		golden string
	}{
		{
			name:   "apply",
			build:  func(cr *ovnv1alpha1.OVNChassis) any { return applyJob(cr, pinMaintenanceCentral, testNodeA) },
			golden: pinApplyJobGolden,
		},
		{
			name:   "evacuate",
			build:  func(cr *ovnv1alpha1.OVNChassis) any { return evacuateJob(cr, pinMaintenanceCentral, testNodeA, entry) },
			golden: pinEvacuateJobGolden,
		},
		{
			name: "chassis-del",
			build: func(cr *ovnv1alpha1.OVNChassis) any {
				return chassisDelJob(cr, pinMaintenanceCentral, testNodeA, entry)
			},
			golden: pinChassisDelJobGolden,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)

			got, err := yaml.Marshal(tc.build(pinMaintenanceChassis()))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(string(got)).To(Equal(tc.golden),
				"the rendered %s Job must stay byte-identical", tc.name)
		})
	}
}

// The Job name is what ties a run to a node, and the node name is hashed into it
// because a node name has four times the room a Job name does.
func TestMaintenanceJobName(t *testing.T) {
	g := NewWithT(t)
	cr := testOVNChassis()

	g.Expect(maintenanceJobName(cr, maintenanceKindApply, testNodeA)).
		To(Equal("chassis-apply-" + pinNodeAHash))
	g.Expect(maintenanceJobName(cr, maintenanceKindEvacuate, testNodeA)).
		To(Equal("chassis-evacuate-" + pinNodeAHash))
	g.Expect(maintenanceJobName(cr, maintenanceKindChassisDel, testNodeA)).
		To(Equal("chassis-chassis-del-" + pinNodeAHash))

	g.Expect(maintenanceJobName(cr, maintenanceKindApply, testNodeB)).
		To(Equal("chassis-apply-" + pinNodeBHash))
	g.Expect(maintenanceJobName(cr, maintenanceKindApply, testNodeB)).
		NotTo(Equal(maintenanceJobName(cr, maintenanceKindApply, testNodeA)),
			"two nodes must not share a Job, or one would cancel the other's run")

	// A node name of any length collapses onto the same eight hex characters, so
	// the Job name budget depends on the CR name alone.
	g.Expect(maintenanceJobName(cr, maintenanceKindChassisDel, strings.Repeat("n", 253))).
		To(HaveLen(len(cr.Name) + len("-chassis-del-") + maintenanceNameHashLength))
}

// The webhook caps metadata.name at 42 characters for this Job alone: the
// longest kind spends 21 on top of it, and Kubernetes rejects an object name
// past 63.
func TestMaintenanceJobName_LongestKindFitsTheNameCap(t *testing.T) {
	g := NewWithT(t)

	cr := testOVNChassis()
	cr.Name = "ovn-chassis-with-a-name-of-42-characters-x"
	g.Expect(cr.Name).To(HaveLen(ovnv1alpha1.MaxOVNChassisNameLength))

	name := maintenanceJobName(cr, maintenanceKindChassisDel, testNodeA)
	g.Expect(name).To(Equal("ovn-chassis-with-a-name-of-42-characters-x-chassis-del-" + pinNodeAHash))
	g.Expect(name).To(HaveLen(63),
		"a CR admitted at the cap must still produce a Job name the API server accepts")
}
