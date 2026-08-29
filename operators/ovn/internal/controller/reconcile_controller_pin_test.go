// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Byte-identity pin for the ovn-controller DaemonSet. The goldens below are
// FULL-OBJECT YAML captured from the builder as it stands today, so any refactor
// of the projection has to reproduce every rendered byte.
//
// The pod is where the chassis meets the control plane: the client keypair it
// mounts is the identity the Southbound database grants its rights by, the
// environment is what the init container writes into the node's own database,
// and the preStop hook is what decides whether a rollout keeps the node's flows
// or recomputes them.
package controller

import (
	"testing"

	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"

	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// pinChassisCentral is the resolved central the default golden is rendered
// against: an OVNCentral without a relay, so the chassis dial the Southbound
// database itself.
func pinChassisCentral() resolvedCentral {
	return resolvedCentral{
		ovnRemote:        testSouthboundAddress,
		nbAddress:        testNorthboundAddress,
		sbAddress:        testSouthboundAddress,
		clientSecretName: "ovn-client",
	}
}

// pinCustomChassisCentral is the resolved central of a control plane that runs a
// relay: the remote is the relay's address while the Southbound one stays the
// database's, and the client Secret carries a name the chassis cannot derive.
func pinCustomChassisCentral() resolvedCentral {
	return resolvedCentral{
		ovnRemote:        "ssl:10.96.0.77:6642",
		nbAddress:        testNorthboundAddress,
		sbAddress:        testSouthboundAddress,
		clientSecretName: "ovn-chassis-client",
	}
}

// pinCustomControllerChassis adds the rollout pace and the two node-level knobs
// to the shared custom fixture (pinCustomChassis, in reconcile_ovs_pin_test.go).
//
// The encapsulation is set to vxlan on purpose: it reaches a node through the
// nodes ConfigMap, so the golden must show the probe interval and no trace of
// the encapsulation.
func pinCustomControllerChassis() *ovnv1alpha1.OVNChassis {
	cr := pinCustomChassis()
	cr.Spec.UpdateStrategy.MaxUnavailable = ptr.To(intstr.FromString("10%"))
	cr.Spec.EncapType = "vxlan"
	cr.Spec.RemoteProbeIntervalMs = 30000
	return cr
}

const pinControllerDaemonSetGolden = `metadata:
  labels:
    app.kubernetes.io/component: ovn-controller
    app.kubernetes.io/instance: chassis
    app.kubernetes.io/managed-by: ovnchassis-operator
    app.kubernetes.io/name: ovnchassis
  name: chassis-ovn-controller
  namespace: openstack
spec:
  selector:
    matchLabels:
      app.kubernetes.io/component: ovn-controller
      app.kubernetes.io/instance: chassis
      app.kubernetes.io/name: ovnchassis
  template:
    metadata:
      labels:
        app.kubernetes.io/component: ovn-controller
        app.kubernetes.io/instance: chassis
        app.kubernetes.io/managed-by: ovnchassis-operator
        app.kubernetes.io/name: ovnchassis
    spec:
      containers:
      - command:
        - ovn-controller
        - unix:/run/openvswitch/db.sock
        - --pidfile=/run/ovn/ovn-controller.pid
        - --unixctl=/run/ovn/ovn-controller.ctl
        - -p
        - /etc/ovn/tls/tls.key
        - -c
        - /etc/ovn/tls/tls.crt
        - -C
        - /etc/ovn/tls/ca.crt
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
          value: ssl:10.96.0.21:6642
        - name: OVN_REMOTE_PROBE_INTERVAL_MS
          value: "60000"
        image: ghcr.io/c5c3/ovn:26.03.2
        lifecycle:
          preStop:
            exec:
              command:
              - ovn-appctl
              - -t
              - /run/ovn/ovn-controller.ctl
              - exit
              - --restart
        name: ovn-controller
        readinessProbe:
          exec:
            command:
            - sh
            - -c
            - ovn-appctl -t /run/ovn/ovn-controller.ctl connection-status | grep -q
              connected
          initialDelaySeconds: 5
          periodSeconds: 5
          timeoutSeconds: 5
        resources: {}
        securityContext:
          allowPrivilegeEscalation: false
          capabilities:
            add:
            - NET_ADMIN
            drop:
            - ALL
          privileged: false
          readOnlyRootFilesystem: true
          runAsGroup: 42424
          runAsNonRoot: false
          runAsUser: 0
          seccompProfile:
            type: RuntimeDefault
        volumeMounts:
        - mountPath: /run/openvswitch
          name: run-ovs
        - mountPath: /run/ovn
          name: run-ovn
        - mountPath: /var/log/ovn
          name: log-ovn
        - mountPath: /tmp
          name: tmp
        - mountPath: /etc/ovn/tls
          name: tls
          readOnly: true
      dnsPolicy: ClusterFirstWithHostNet
      hostNetwork: true
      initContainers:
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
          value: ssl:10.96.0.21:6642
        - name: OVN_REMOTE_PROBE_INTERVAL_MS
          value: "60000"
        image: ghcr.io/c5c3/ovn:26.03.2
        name: apply-node
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
        - mountPath: /run/openvswitch
          name: run-ovs
        - mountPath: /etc/ovn-chassis/bin
          name: scripts
        - mountPath: /etc/ovn-chassis/nodes
          name: nodes
      nodeSelector:
        openstack.c5c3.io/chassis: "true"
      securityContext:
        seccompProfile:
          type: RuntimeDefault
      terminationGracePeriodSeconds: 30
      volumes:
      - hostPath:
          path: /run/openvswitch
          type: DirectoryOrCreate
        name: run-ovs
      - hostPath:
          path: /run/ovn
          type: DirectoryOrCreate
        name: run-ovn
      - emptyDir: {}
        name: log-ovn
      - emptyDir: {}
        name: tmp
      - configMap:
          defaultMode: 365
          name: chassis-chassis-scripts
        name: scripts
      - configMap:
          name: chassis-nodes
        name: nodes
      - name: tls
        secret:
          secretName: ovn-client
  updateStrategy:
    rollingUpdate:
      maxUnavailable: 1
    type: RollingUpdate
status:
  currentNumberScheduled: 0
  desiredNumberScheduled: 0
  numberMisscheduled: 0
  numberReady: 0
`

const pinCustomControllerDaemonSetGolden = `metadata:
  labels:
    app.kubernetes.io/component: ovn-controller
    app.kubernetes.io/instance: chassis
    app.kubernetes.io/managed-by: ovnchassis-operator
    app.kubernetes.io/name: ovnchassis
  name: chassis-ovn-controller
  namespace: openstack
spec:
  selector:
    matchLabels:
      app.kubernetes.io/component: ovn-controller
      app.kubernetes.io/instance: chassis
      app.kubernetes.io/name: ovnchassis
  template:
    metadata:
      labels:
        app.kubernetes.io/component: ovn-controller
        app.kubernetes.io/instance: chassis
        app.kubernetes.io/managed-by: ovnchassis-operator
        app.kubernetes.io/name: ovnchassis
    spec:
      containers:
      - command:
        - ovn-controller
        - unix:/run/openvswitch/db.sock
        - --pidfile=/run/ovn/ovn-controller.pid
        - --unixctl=/run/ovn/ovn-controller.ctl
        - -p
        - /etc/ovn/tls/tls.key
        - -c
        - /etc/ovn/tls/tls.crt
        - -C
        - /etc/ovn/tls/ca.crt
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
          value: ssl:10.96.0.77:6642
        - name: OVN_REMOTE_PROBE_INTERVAL_MS
          value: "30000"
        image: registry.example.com/ovn@sha256:1111111111111111111111111111111111111111111111111111111111111111
        lifecycle:
          preStop:
            exec:
              command:
              - ovn-appctl
              - -t
              - /run/ovn/ovn-controller.ctl
              - exit
              - --restart
        name: ovn-controller
        readinessProbe:
          exec:
            command:
            - sh
            - -c
            - ovn-appctl -t /run/ovn/ovn-controller.ctl connection-status | grep -q
              connected
          initialDelaySeconds: 5
          periodSeconds: 5
          timeoutSeconds: 5
        resources:
          requests:
            cpu: 100m
            memory: 256Mi
        securityContext:
          allowPrivilegeEscalation: false
          capabilities:
            add:
            - NET_ADMIN
            drop:
            - ALL
          privileged: false
          readOnlyRootFilesystem: true
          runAsGroup: 42424
          runAsNonRoot: false
          runAsUser: 0
          seccompProfile:
            type: RuntimeDefault
        volumeMounts:
        - mountPath: /run/openvswitch
          name: run-ovs
        - mountPath: /run/ovn
          name: run-ovn
        - mountPath: /var/log/ovn
          name: log-ovn
        - mountPath: /tmp
          name: tmp
        - mountPath: /etc/ovn/tls
          name: tls
          readOnly: true
      dnsPolicy: ClusterFirstWithHostNet
      hostNetwork: true
      initContainers:
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
          value: ssl:10.96.0.77:6642
        - name: OVN_REMOTE_PROBE_INTERVAL_MS
          value: "30000"
        image: registry.example.com/ovn@sha256:1111111111111111111111111111111111111111111111111111111111111111
        name: apply-node
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
        - mountPath: /run/openvswitch
          name: run-ovs
        - mountPath: /etc/ovn-chassis/bin
          name: scripts
        - mountPath: /etc/ovn-chassis/nodes
          name: nodes
      nodeSelector:
        openstack.c5c3.io/chassis: "true"
      securityContext:
        seccompProfile:
          type: RuntimeDefault
      terminationGracePeriodSeconds: 30
      tolerations:
      - effect: NoSchedule
        key: openstack.c5c3.io/network
        operator: Exists
      volumes:
      - hostPath:
          path: /run/openvswitch
          type: DirectoryOrCreate
        name: run-ovs
      - hostPath:
          path: /run/ovn
          type: DirectoryOrCreate
        name: run-ovn
      - emptyDir: {}
        name: log-ovn
      - emptyDir: {}
        name: tmp
      - configMap:
          defaultMode: 365
          name: chassis-chassis-scripts
        name: scripts
      - configMap:
          name: chassis-nodes
        name: nodes
      - name: tls
        secret:
          secretName: ovn-chassis-client
  updateStrategy:
    rollingUpdate:
      maxUnavailable: 10%
    type: RollingUpdate
status:
  currentNumberScheduled: 0
  desiredNumberScheduled: 0
  numberMisscheduled: 0
  numberReady: 0
`

// TestPinControllerDaemonSet pins the ovn-controller DaemonSet across a
// defaulted CR attached to a relay-less central and one that moves every knob
// the builder reads.
func TestPinControllerDaemonSet(t *testing.T) {
	cases := []struct {
		name    string
		cr      func() *ovnv1alpha1.OVNChassis
		central func() resolvedCentral
		golden  string
	}{
		{
			name:    "default",
			cr:      testOVNChassis,
			central: pinChassisCentral,
			golden:  pinControllerDaemonSetGolden,
		},
		{
			name:    "custom",
			cr:      pinCustomControllerChassis,
			central: pinCustomChassisCentral,
			golden:  pinCustomControllerDaemonSetGolden,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)

			got, err := yaml.Marshal(buildControllerDaemonSet(tc.cr(), tc.central()))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(string(got)).To(Equal(tc.golden),
				"the rendered ovn-controller DaemonSet must stay byte-identical")
		})
	}
}
