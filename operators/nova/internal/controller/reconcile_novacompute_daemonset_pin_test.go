// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Byte-identity pin for the nova-compute DaemonSet of a NovaCompute. The
// goldens below are FULL-OBJECT YAML captured from the builder, so any refactor
// of the projection has to reproduce every rendered byte.
//
// The pod is where a hypervisor's instances are driven from: it runs as root
// in the node's network namespace, reaches the node's libvirt and Open vSwitch
// through their sockets, and writes the instance disks to the node. Every one
// of those is a rendered field rather than a runtime decision.
package controller

import (
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
)

// The inputs the goldens are rendered from. The ConfigMap name is
// content-addressed in production; a literal keeps the goldens stable while
// still proving the volume names what the config step returned.
const (
	pinNovaComputeConfigMap = testPoolName + "-config-abcdef12"
	pinNovaComputeHash      = "0123456789abcdef"
)

func pinNovaComputeImage() commonv1.ImageSpec {
	return commonv1.ImageSpec{Repository: novaComputeDefaultRepository, Tag: "2025.2"}
}

// pinOnDeleteNovaCompute names its own resources, a toleration, and the
// OnDelete strategy.
func pinOnDeleteNovaCompute() *novav1alpha1.NovaCompute {
	cr := validNovaCompute()
	cr.Spec.UpdateStrategy = novav1alpha1.NovaComputeUpdateStrategy{Type: "OnDelete"}
	cr.Spec.Tolerations = []corev1.Toleration{{
		Key: "openstack.c5c3.io/compute", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule,
	}}
	cr.Spec.Resources = &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("1Gi"),
		},
		Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("4Gi")},
	}
	return cr
}

type pinNovaComputeCase struct {
	name     string
	cr       func() *novav1alpha1.NovaCompute
	affinity *corev1.Affinity
	golden   string
}

func pinNovaComputeCases() []pinNovaComputeCase {
	return []pinNovaComputeCase{
		{
			name:     "default",
			cr:       validNovaCompute,
			affinity: novaComputeAffinity(validNovaCompute(), nil, nil),
			golden:   pinNovaComputeDaemonSetGolden,
		},
		{
			name:     "ondelete-with-resources",
			cr:       pinOnDeleteNovaCompute,
			affinity: novaComputeAffinity(validNovaCompute(), nil, nil),
			golden:   pinOnDeleteNovaComputeDaemonSetGolden,
		},
		{
			name:     "held-and-excluded",
			cr:       validNovaCompute,
			affinity: novaComputeAffinity(validNovaCompute(), []string{"node-9"}, []string{"node-2"}),
			golden:   pinHeldNovaComputeDaemonSetGolden,
		},
	}
}

// pinNovaComputeDaemonSetGolden is the defaulted pool: the selector term alone,
// no resources, the rolling update of one node at a time.
const pinNovaComputeDaemonSetGolden = `metadata:
  labels:
    app.kubernetes.io/component: nova-compute
    app.kubernetes.io/instance: pool-a
    app.kubernetes.io/managed-by: novacompute-operator
    app.kubernetes.io/name: novacompute
  name: pool-a-nova-compute
  namespace: openstack
spec:
  selector:
    matchLabels:
      app.kubernetes.io/component: nova-compute
      app.kubernetes.io/instance: pool-a
      app.kubernetes.io/name: novacompute
  template:
    metadata:
      annotations:
        nova.openstack.c5c3.io/compute-config-hash: 0123456789abcdef
      labels:
        app.kubernetes.io/component: nova-compute
        app.kubernetes.io/instance: pool-a
        app.kubernetes.io/managed-by: novacompute-operator
        app.kubernetes.io/name: novacompute
    spec:
      affinity:
        nodeAffinity:
          requiredDuringSchedulingIgnoredDuringExecution:
            nodeSelectorTerms:
            - matchExpressions:
              - key: openstack.c5c3.io/nova-compute-pool
                operator: In
                values:
                - a
      containers:
      - command:
        - nova-compute
        - --config-file
        - /etc/nova/compute-config/nova-compute.conf
        - --config-dir
        - /etc/nova/compute-pool.conf.d
        env:
        - name: OS_DEFAULT__HOST
          valueFrom:
            fieldRef:
              fieldPath: spec.nodeName
        - name: OS_DEFAULT__MY_IP
          valueFrom:
            fieldRef:
              fieldPath: status.hostIP
        - name: OS_VNC__SERVER_PROXYCLIENT_ADDRESS
          valueFrom:
            fieldRef:
              fieldPath: status.hostIP
        - name: OS_LIBVIRT__LIVE_MIGRATION_INBOUND_ADDR
          valueFrom:
            fieldRef:
              fieldPath: status.hostIP
        - name: OS_DEFAULT__TRANSPORT_URL
          valueFrom:
            secretKeyRef:
              key: transport_url
              name: nova-compute-config
        - name: OS_KEYSTONE_AUTHTOKEN__PASSWORD
          valueFrom:
            secretKeyRef:
              key: password
              name: nova-compute-config
        - name: OS_SERVICE_USER__PASSWORD
          valueFrom:
            secretKeyRef:
              key: password
              name: nova-compute-config
        - name: OS_PLACEMENT__PASSWORD
          valueFrom:
            secretKeyRef:
              key: password
              name: nova-compute-config
        - name: OS_NEUTRON__PASSWORD
          valueFrom:
            secretKeyRef:
              key: password
              name: nova-compute-config
        - name: OS_CINDER__PASSWORD
          valueFrom:
            secretKeyRef:
              key: password
              name: nova-compute-config
        image: ghcr.io/c5c3/nova-compute:2025.2
        name: nova-compute
        resources: {}
        securityContext:
          allowPrivilegeEscalation: true
          privileged: true
          readOnlyRootFilesystem: true
          runAsNonRoot: false
          runAsUser: 0
        volumeMounts:
        - mountPath: /etc/nova/compute-config
          name: compute-config
          readOnly: true
        - mountPath: /etc/nova/compute-pool.conf.d
          name: pool-config
          readOnly: true
        - mountPath: /run/libvirt
          name: run-libvirt
        - mountPath: /var/lib/nova
          mountPropagation: Bidirectional
          name: var-lib-nova
        - mountPath: /run/openvswitch
          name: run-openvswitch
        - mountPath: /dev
          name: dev
        - mountPath: /sys/fs/cgroup
          name: sys-fs-cgroup
          readOnly: true
        - mountPath: /lib/modules
          name: lib-modules
          readOnly: true
        - mountPath: /etc/iscsi
          name: etc-iscsi
        - mountPath: /etc/nvme
          name: etc-nvme
        - mountPath: /etc/multipath
          name: etc-multipath
        - mountPath: /etc/multipath.conf
          name: etc-multipath-conf
        - mountPath: /tmp
          name: tmp
      dnsPolicy: ClusterFirstWithHostNet
      hostNetwork: true
      initContainers:
      - command:
        - python3
        - -c
        - |
          import json, os, socket, time
          path = os.environ.get("OVSDB_SOCKET", "/run/openvswitch/db.sock")
          query = {"id": 0, "method": "transact", "params": ["Open_vSwitch", {"op": "select", "table": "Open_vSwitch", "where": [], "columns": ["external_ids"]}]}
          def registered():
              s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
              s.settimeout(5)
              try:
                  s.connect(path)
                  s.sendall(json.dumps(query).encode())
                  decoder, buf = json.JSONDecoder(), ""
                  while True:
                      chunk = s.recv(65536)
                      if not chunk:
                          return False
                      buf += chunk.decode()
                      while buf.strip():
                          try:
                              msg, end = decoder.raw_decode(buf.lstrip())
                          except ValueError:
                              break
                          buf = buf.lstrip()[end:]
                          if msg.get("id") != 0:
                              continue
                          for row in msg["result"][0]["rows"]:
                              if any(k == "system-id" for k, _ in row["external_ids"][1]):
                                  return True
                          return False
              finally:
                  s.close()
          while True:
              try:
                  if registered():
                      break
              except (OSError, ValueError, KeyError, IndexError, TypeError):
                  pass
              time.sleep(2)
        env:
        - name: OVSDB_SOCKET
          value: /run/openvswitch/db.sock
        image: ghcr.io/c5c3/nova-compute:2025.2
        name: wait-for-chassis
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
          name: run-openvswitch
      securityContext:
        seccompProfile:
          type: RuntimeDefault
      terminationGracePeriodSeconds: 30
      volumes:
      - name: compute-config
        secret:
          secretName: nova-compute-config
      - configMap:
          name: pool-a-config-abcdef12
        name: pool-config
      - hostPath:
          path: /run/libvirt
          type: DirectoryOrCreate
        name: run-libvirt
      - hostPath:
          path: /var/lib/nova
          type: DirectoryOrCreate
        name: var-lib-nova
      - hostPath:
          path: /run/openvswitch
          type: DirectoryOrCreate
        name: run-openvswitch
      - hostPath:
          path: /dev
        name: dev
      - hostPath:
          path: /sys/fs/cgroup
        name: sys-fs-cgroup
      - hostPath:
          path: /lib/modules
        name: lib-modules
      - hostPath:
          path: /etc/iscsi
          type: DirectoryOrCreate
        name: etc-iscsi
      - hostPath:
          path: /etc/nvme
          type: DirectoryOrCreate
        name: etc-nvme
      - hostPath:
          path: /etc/multipath
          type: DirectoryOrCreate
        name: etc-multipath
      - hostPath:
          path: /etc/multipath.conf
          type: FileOrCreate
        name: etc-multipath-conf
      - emptyDir: {}
        name: tmp
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

// pinOnDeleteNovaComputeDaemonSetGolden is a pool that names its resources
// (applied to both containers), a toleration, and OnDelete.
const pinOnDeleteNovaComputeDaemonSetGolden = `metadata:
  labels:
    app.kubernetes.io/component: nova-compute
    app.kubernetes.io/instance: pool-a
    app.kubernetes.io/managed-by: novacompute-operator
    app.kubernetes.io/name: novacompute
  name: pool-a-nova-compute
  namespace: openstack
spec:
  selector:
    matchLabels:
      app.kubernetes.io/component: nova-compute
      app.kubernetes.io/instance: pool-a
      app.kubernetes.io/name: novacompute
  template:
    metadata:
      annotations:
        nova.openstack.c5c3.io/compute-config-hash: 0123456789abcdef
      labels:
        app.kubernetes.io/component: nova-compute
        app.kubernetes.io/instance: pool-a
        app.kubernetes.io/managed-by: novacompute-operator
        app.kubernetes.io/name: novacompute
    spec:
      affinity:
        nodeAffinity:
          requiredDuringSchedulingIgnoredDuringExecution:
            nodeSelectorTerms:
            - matchExpressions:
              - key: openstack.c5c3.io/nova-compute-pool
                operator: In
                values:
                - a
      containers:
      - command:
        - nova-compute
        - --config-file
        - /etc/nova/compute-config/nova-compute.conf
        - --config-dir
        - /etc/nova/compute-pool.conf.d
        env:
        - name: OS_DEFAULT__HOST
          valueFrom:
            fieldRef:
              fieldPath: spec.nodeName
        - name: OS_DEFAULT__MY_IP
          valueFrom:
            fieldRef:
              fieldPath: status.hostIP
        - name: OS_VNC__SERVER_PROXYCLIENT_ADDRESS
          valueFrom:
            fieldRef:
              fieldPath: status.hostIP
        - name: OS_LIBVIRT__LIVE_MIGRATION_INBOUND_ADDR
          valueFrom:
            fieldRef:
              fieldPath: status.hostIP
        - name: OS_DEFAULT__TRANSPORT_URL
          valueFrom:
            secretKeyRef:
              key: transport_url
              name: nova-compute-config
        - name: OS_KEYSTONE_AUTHTOKEN__PASSWORD
          valueFrom:
            secretKeyRef:
              key: password
              name: nova-compute-config
        - name: OS_SERVICE_USER__PASSWORD
          valueFrom:
            secretKeyRef:
              key: password
              name: nova-compute-config
        - name: OS_PLACEMENT__PASSWORD
          valueFrom:
            secretKeyRef:
              key: password
              name: nova-compute-config
        - name: OS_NEUTRON__PASSWORD
          valueFrom:
            secretKeyRef:
              key: password
              name: nova-compute-config
        - name: OS_CINDER__PASSWORD
          valueFrom:
            secretKeyRef:
              key: password
              name: nova-compute-config
        image: ghcr.io/c5c3/nova-compute:2025.2
        name: nova-compute
        resources:
          limits:
            memory: 4Gi
          requests:
            cpu: 500m
            memory: 1Gi
        securityContext:
          allowPrivilegeEscalation: true
          privileged: true
          readOnlyRootFilesystem: true
          runAsNonRoot: false
          runAsUser: 0
        volumeMounts:
        - mountPath: /etc/nova/compute-config
          name: compute-config
          readOnly: true
        - mountPath: /etc/nova/compute-pool.conf.d
          name: pool-config
          readOnly: true
        - mountPath: /run/libvirt
          name: run-libvirt
        - mountPath: /var/lib/nova
          mountPropagation: Bidirectional
          name: var-lib-nova
        - mountPath: /run/openvswitch
          name: run-openvswitch
        - mountPath: /dev
          name: dev
        - mountPath: /sys/fs/cgroup
          name: sys-fs-cgroup
          readOnly: true
        - mountPath: /lib/modules
          name: lib-modules
          readOnly: true
        - mountPath: /etc/iscsi
          name: etc-iscsi
        - mountPath: /etc/nvme
          name: etc-nvme
        - mountPath: /etc/multipath
          name: etc-multipath
        - mountPath: /etc/multipath.conf
          name: etc-multipath-conf
        - mountPath: /tmp
          name: tmp
      dnsPolicy: ClusterFirstWithHostNet
      hostNetwork: true
      initContainers:
      - command:
        - python3
        - -c
        - |
          import json, os, socket, time
          path = os.environ.get("OVSDB_SOCKET", "/run/openvswitch/db.sock")
          query = {"id": 0, "method": "transact", "params": ["Open_vSwitch", {"op": "select", "table": "Open_vSwitch", "where": [], "columns": ["external_ids"]}]}
          def registered():
              s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
              s.settimeout(5)
              try:
                  s.connect(path)
                  s.sendall(json.dumps(query).encode())
                  decoder, buf = json.JSONDecoder(), ""
                  while True:
                      chunk = s.recv(65536)
                      if not chunk:
                          return False
                      buf += chunk.decode()
                      while buf.strip():
                          try:
                              msg, end = decoder.raw_decode(buf.lstrip())
                          except ValueError:
                              break
                          buf = buf.lstrip()[end:]
                          if msg.get("id") != 0:
                              continue
                          for row in msg["result"][0]["rows"]:
                              if any(k == "system-id" for k, _ in row["external_ids"][1]):
                                  return True
                          return False
              finally:
                  s.close()
          while True:
              try:
                  if registered():
                      break
              except (OSError, ValueError, KeyError, IndexError, TypeError):
                  pass
              time.sleep(2)
        env:
        - name: OVSDB_SOCKET
          value: /run/openvswitch/db.sock
        image: ghcr.io/c5c3/nova-compute:2025.2
        name: wait-for-chassis
        resources:
          limits:
            memory: 4Gi
          requests:
            cpu: 500m
            memory: 1Gi
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
          name: run-openvswitch
      securityContext:
        seccompProfile:
          type: RuntimeDefault
      terminationGracePeriodSeconds: 30
      tolerations:
      - effect: NoSchedule
        key: openstack.c5c3.io/compute
        operator: Exists
      volumes:
      - name: compute-config
        secret:
          secretName: nova-compute-config
      - configMap:
          name: pool-a-config-abcdef12
        name: pool-config
      - hostPath:
          path: /run/libvirt
          type: DirectoryOrCreate
        name: run-libvirt
      - hostPath:
          path: /var/lib/nova
          type: DirectoryOrCreate
        name: var-lib-nova
      - hostPath:
          path: /run/openvswitch
          type: DirectoryOrCreate
        name: run-openvswitch
      - hostPath:
          path: /dev
        name: dev
      - hostPath:
          path: /sys/fs/cgroup
        name: sys-fs-cgroup
      - hostPath:
          path: /lib/modules
        name: lib-modules
      - hostPath:
          path: /etc/iscsi
          type: DirectoryOrCreate
        name: etc-iscsi
      - hostPath:
          path: /etc/nvme
          type: DirectoryOrCreate
        name: etc-nvme
      - hostPath:
          path: /etc/multipath
          type: DirectoryOrCreate
        name: etc-multipath
      - hostPath:
          path: /etc/multipath.conf
          type: FileOrCreate
        name: etc-multipath-conf
      - emptyDir: {}
        name: tmp
  updateStrategy:
    type: OnDelete
status:
  currentNumberScheduled: 0
  desiredNumberScheduled: 0
  numberMisscheduled: 0
  numberReady: 0
`

// pinHeldNovaComputeDaemonSetGolden is a pool yielding node-9 to another pool
// (a NotIn beside the selector) and keeping its pod on the draining node-2 (a
// term of its own).
const pinHeldNovaComputeDaemonSetGolden = `metadata:
  labels:
    app.kubernetes.io/component: nova-compute
    app.kubernetes.io/instance: pool-a
    app.kubernetes.io/managed-by: novacompute-operator
    app.kubernetes.io/name: novacompute
  name: pool-a-nova-compute
  namespace: openstack
spec:
  selector:
    matchLabels:
      app.kubernetes.io/component: nova-compute
      app.kubernetes.io/instance: pool-a
      app.kubernetes.io/name: novacompute
  template:
    metadata:
      annotations:
        nova.openstack.c5c3.io/compute-config-hash: 0123456789abcdef
      labels:
        app.kubernetes.io/component: nova-compute
        app.kubernetes.io/instance: pool-a
        app.kubernetes.io/managed-by: novacompute-operator
        app.kubernetes.io/name: novacompute
    spec:
      affinity:
        nodeAffinity:
          requiredDuringSchedulingIgnoredDuringExecution:
            nodeSelectorTerms:
            - matchExpressions:
              - key: openstack.c5c3.io/nova-compute-pool
                operator: In
                values:
                - a
              matchFields:
              - key: metadata.name
                operator: NotIn
                values:
                - node-9
            - matchFields:
              - key: metadata.name
                operator: In
                values:
                - node-2
      containers:
      - command:
        - nova-compute
        - --config-file
        - /etc/nova/compute-config/nova-compute.conf
        - --config-dir
        - /etc/nova/compute-pool.conf.d
        env:
        - name: OS_DEFAULT__HOST
          valueFrom:
            fieldRef:
              fieldPath: spec.nodeName
        - name: OS_DEFAULT__MY_IP
          valueFrom:
            fieldRef:
              fieldPath: status.hostIP
        - name: OS_VNC__SERVER_PROXYCLIENT_ADDRESS
          valueFrom:
            fieldRef:
              fieldPath: status.hostIP
        - name: OS_LIBVIRT__LIVE_MIGRATION_INBOUND_ADDR
          valueFrom:
            fieldRef:
              fieldPath: status.hostIP
        - name: OS_DEFAULT__TRANSPORT_URL
          valueFrom:
            secretKeyRef:
              key: transport_url
              name: nova-compute-config
        - name: OS_KEYSTONE_AUTHTOKEN__PASSWORD
          valueFrom:
            secretKeyRef:
              key: password
              name: nova-compute-config
        - name: OS_SERVICE_USER__PASSWORD
          valueFrom:
            secretKeyRef:
              key: password
              name: nova-compute-config
        - name: OS_PLACEMENT__PASSWORD
          valueFrom:
            secretKeyRef:
              key: password
              name: nova-compute-config
        - name: OS_NEUTRON__PASSWORD
          valueFrom:
            secretKeyRef:
              key: password
              name: nova-compute-config
        - name: OS_CINDER__PASSWORD
          valueFrom:
            secretKeyRef:
              key: password
              name: nova-compute-config
        image: ghcr.io/c5c3/nova-compute:2025.2
        name: nova-compute
        resources: {}
        securityContext:
          allowPrivilegeEscalation: true
          privileged: true
          readOnlyRootFilesystem: true
          runAsNonRoot: false
          runAsUser: 0
        volumeMounts:
        - mountPath: /etc/nova/compute-config
          name: compute-config
          readOnly: true
        - mountPath: /etc/nova/compute-pool.conf.d
          name: pool-config
          readOnly: true
        - mountPath: /run/libvirt
          name: run-libvirt
        - mountPath: /var/lib/nova
          mountPropagation: Bidirectional
          name: var-lib-nova
        - mountPath: /run/openvswitch
          name: run-openvswitch
        - mountPath: /dev
          name: dev
        - mountPath: /sys/fs/cgroup
          name: sys-fs-cgroup
          readOnly: true
        - mountPath: /lib/modules
          name: lib-modules
          readOnly: true
        - mountPath: /etc/iscsi
          name: etc-iscsi
        - mountPath: /etc/nvme
          name: etc-nvme
        - mountPath: /etc/multipath
          name: etc-multipath
        - mountPath: /etc/multipath.conf
          name: etc-multipath-conf
        - mountPath: /tmp
          name: tmp
      dnsPolicy: ClusterFirstWithHostNet
      hostNetwork: true
      initContainers:
      - command:
        - python3
        - -c
        - |
          import json, os, socket, time
          path = os.environ.get("OVSDB_SOCKET", "/run/openvswitch/db.sock")
          query = {"id": 0, "method": "transact", "params": ["Open_vSwitch", {"op": "select", "table": "Open_vSwitch", "where": [], "columns": ["external_ids"]}]}
          def registered():
              s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
              s.settimeout(5)
              try:
                  s.connect(path)
                  s.sendall(json.dumps(query).encode())
                  decoder, buf = json.JSONDecoder(), ""
                  while True:
                      chunk = s.recv(65536)
                      if not chunk:
                          return False
                      buf += chunk.decode()
                      while buf.strip():
                          try:
                              msg, end = decoder.raw_decode(buf.lstrip())
                          except ValueError:
                              break
                          buf = buf.lstrip()[end:]
                          if msg.get("id") != 0:
                              continue
                          for row in msg["result"][0]["rows"]:
                              if any(k == "system-id" for k, _ in row["external_ids"][1]):
                                  return True
                          return False
              finally:
                  s.close()
          while True:
              try:
                  if registered():
                      break
              except (OSError, ValueError, KeyError, IndexError, TypeError):
                  pass
              time.sleep(2)
        env:
        - name: OVSDB_SOCKET
          value: /run/openvswitch/db.sock
        image: ghcr.io/c5c3/nova-compute:2025.2
        name: wait-for-chassis
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
          name: run-openvswitch
      securityContext:
        seccompProfile:
          type: RuntimeDefault
      terminationGracePeriodSeconds: 30
      volumes:
      - name: compute-config
        secret:
          secretName: nova-compute-config
      - configMap:
          name: pool-a-config-abcdef12
        name: pool-config
      - hostPath:
          path: /run/libvirt
          type: DirectoryOrCreate
        name: run-libvirt
      - hostPath:
          path: /var/lib/nova
          type: DirectoryOrCreate
        name: var-lib-nova
      - hostPath:
          path: /run/openvswitch
          type: DirectoryOrCreate
        name: run-openvswitch
      - hostPath:
          path: /dev
        name: dev
      - hostPath:
          path: /sys/fs/cgroup
        name: sys-fs-cgroup
      - hostPath:
          path: /lib/modules
        name: lib-modules
      - hostPath:
          path: /etc/iscsi
          type: DirectoryOrCreate
        name: etc-iscsi
      - hostPath:
          path: /etc/nvme
          type: DirectoryOrCreate
        name: etc-nvme
      - hostPath:
          path: /etc/multipath
          type: DirectoryOrCreate
        name: etc-multipath
      - hostPath:
          path: /etc/multipath.conf
          type: FileOrCreate
        name: etc-multipath-conf
      - emptyDir: {}
        name: tmp
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

// TestPinNovaComputeDaemonSet pins the nova-compute DaemonSet across the
// defaulted pool, a pool with its own resources, toleration and OnDelete
// strategy, and a pool that holds one draining node and yields one to another
// pool.
func TestPinNovaComputeDaemonSet(t *testing.T) {
	for _, tc := range pinNovaComputeCases() {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)

			got, err := yaml.Marshal(buildNovaComputeDaemonSet(tc.cr(), pinNovaComputeImage(),
				testContract, pinNovaComputeConfigMap, pinNovaComputeHash, tc.affinity))

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(string(got)).To(Equal(tc.golden),
				"the rendered nova-compute DaemonSet must stay byte-identical")
		})
	}
}
