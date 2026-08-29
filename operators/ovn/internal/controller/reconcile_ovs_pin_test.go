// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Byte-identity pin for the Open vSwitch DaemonSet. The goldens below are
// FULL-OBJECT YAML captured from the builder as it stands today, so any refactor
// of the projection has to reproduce every rendered byte.
//
// What the goldens protect is the security posture and the host access of a pod
// that owns the node's datapath: a capability added, a host path widened or a
// preStop hook that gains "--cleanup" changes what every workload on that node
// can reach, and none of those changes is visible in a diff of the reconciler.
package controller

import (
	"testing"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// pinCustomChassis moves the knobs both DaemonSets read off their defaults: a
// digest-pinned image, a toleration for the taint a network node carries, and
// resources on the Open vSwitch and the ovn-controller container. The two pin
// files add the rollout knob each of them covers.
func pinCustomChassis() *ovnv1alpha1.OVNChassis {
	cr := testOVNChassis()
	cr.Spec.Image = &commonv1.ImageSpec{
		Repository: "registry.example.com/ovn",
		Digest:     "sha256:1111111111111111111111111111111111111111111111111111111111111111",
	}
	cr.Spec.Tolerations = []corev1.Toleration{{
		Key:      "openstack.c5c3.io/network",
		Operator: corev1.TolerationOpExists,
		Effect:   corev1.TaintEffectNoSchedule,
	}}
	cr.Spec.OVS = &ovnv1alpha1.OVNChassisContainerSpec{Resources: &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("200m"),
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("2"),
			corev1.ResourceMemory: resource.MustParse("2Gi"),
		},
	}}
	cr.Spec.Controller = &ovnv1alpha1.OVNChassisContainerSpec{Resources: &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}}
	return cr
}

// pinCustomOVSChassis adds the rollout pace to the shared custom fixture: two
// nodes may lose their datapath at once.
func pinCustomOVSChassis() *ovnv1alpha1.OVNChassis {
	cr := pinCustomChassis()
	cr.Spec.UpdateStrategy.MaxUnavailable = ptr.To(intstr.FromInt32(2))
	return cr
}

const pinOVSDaemonSetGolden = `metadata:
  labels:
    app.kubernetes.io/component: ovs
    app.kubernetes.io/instance: chassis
    app.kubernetes.io/managed-by: ovnchassis-operator
    app.kubernetes.io/name: ovnchassis
  name: chassis-ovs
  namespace: openstack
spec:
  selector:
    matchLabels:
      app.kubernetes.io/component: ovs
      app.kubernetes.io/instance: chassis
      app.kubernetes.io/name: ovnchassis
  template:
    metadata:
      labels:
        app.kubernetes.io/component: ovs
        app.kubernetes.io/instance: chassis
        app.kubernetes.io/managed-by: ovnchassis-operator
        app.kubernetes.io/name: ovnchassis
    spec:
      containers:
      - command:
        - /bin/bash
        - /etc/ovn-chassis/bin/run-ovsdb.sh
        image: ghcr.io/c5c3/ovn:26.03.2
        lifecycle:
          preStop:
            exec:
              command:
              - ovs-appctl
              - -t
              - /run/openvswitch/ovsdb-server.ctl
              - exit
        name: ovsdb-server
        readinessProbe:
          exec:
            command:
            - ovs-vsctl
            - --timeout=5
            - --no-wait
            - show
          initialDelaySeconds: 5
          periodSeconds: 5
          timeoutSeconds: 5
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
        - mountPath: /var/log/openvswitch
          name: log-ovs
        - mountPath: /tmp
          name: tmp
        - mountPath: /etc/ovn-chassis/bin
          name: scripts
      - command:
        - /bin/bash
        - /etc/ovn-chassis/bin/run-vswitchd.sh
        image: ghcr.io/c5c3/ovn:26.03.2
        lifecycle:
          preStop:
            exec:
              command:
              - ovs-appctl
              - -t
              - /run/openvswitch/ovs-vswitchd.ctl
              - exit
        name: ovs-vswitchd
        readinessProbe:
          exec:
            command:
            - ovs-appctl
            - -t
            - /run/openvswitch/ovs-vswitchd.ctl
            - version
          initialDelaySeconds: 5
          periodSeconds: 5
          timeoutSeconds: 5
        resources: {}
        securityContext:
          allowPrivilegeEscalation: false
          capabilities:
            add:
            - NET_ADMIN
            - SYS_NICE
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
        - mountPath: /var/log/openvswitch
          name: log-ovs
        - mountPath: /tmp
          name: tmp
        - mountPath: /etc/ovn-chassis/bin
          name: scripts
      dnsPolicy: ClusterFirstWithHostNet
      hostNetwork: true
      initContainers:
      - command:
        - /bin/bash
        - /etc/ovn-chassis/bin/host-prepare.sh
        image: ghcr.io/c5c3/ovn:26.03.2
        name: host-prepare
        resources: {}
        securityContext:
          allowPrivilegeEscalation: true
          privileged: true
          readOnlyRootFilesystem: true
          runAsUser: 0
        volumeMounts:
        - mountPath: /lib/modules
          name: modules
          readOnly: true
        - mountPath: /run/openvswitch
          name: run-ovs
        - mountPath: /run/ovn
          name: run-ovn
        - mountPath: /etc/ovn-chassis/bin
          name: scripts
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
      - hostPath:
          path: /lib/modules
        name: modules
      - emptyDir: {}
        name: log-ovs
      - emptyDir: {}
        name: tmp
      - configMap:
          defaultMode: 365
          name: chassis-chassis-scripts
        name: scripts
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

const pinCustomOVSDaemonSetGolden = `metadata:
  labels:
    app.kubernetes.io/component: ovs
    app.kubernetes.io/instance: chassis
    app.kubernetes.io/managed-by: ovnchassis-operator
    app.kubernetes.io/name: ovnchassis
  name: chassis-ovs
  namespace: openstack
spec:
  selector:
    matchLabels:
      app.kubernetes.io/component: ovs
      app.kubernetes.io/instance: chassis
      app.kubernetes.io/name: ovnchassis
  template:
    metadata:
      labels:
        app.kubernetes.io/component: ovs
        app.kubernetes.io/instance: chassis
        app.kubernetes.io/managed-by: ovnchassis-operator
        app.kubernetes.io/name: ovnchassis
    spec:
      containers:
      - command:
        - /bin/bash
        - /etc/ovn-chassis/bin/run-ovsdb.sh
        image: registry.example.com/ovn@sha256:1111111111111111111111111111111111111111111111111111111111111111
        lifecycle:
          preStop:
            exec:
              command:
              - ovs-appctl
              - -t
              - /run/openvswitch/ovsdb-server.ctl
              - exit
        name: ovsdb-server
        readinessProbe:
          exec:
            command:
            - ovs-vsctl
            - --timeout=5
            - --no-wait
            - show
          initialDelaySeconds: 5
          periodSeconds: 5
          timeoutSeconds: 5
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
        - mountPath: /var/log/openvswitch
          name: log-ovs
        - mountPath: /tmp
          name: tmp
        - mountPath: /etc/ovn-chassis/bin
          name: scripts
      - command:
        - /bin/bash
        - /etc/ovn-chassis/bin/run-vswitchd.sh
        image: registry.example.com/ovn@sha256:1111111111111111111111111111111111111111111111111111111111111111
        lifecycle:
          preStop:
            exec:
              command:
              - ovs-appctl
              - -t
              - /run/openvswitch/ovs-vswitchd.ctl
              - exit
        name: ovs-vswitchd
        readinessProbe:
          exec:
            command:
            - ovs-appctl
            - -t
            - /run/openvswitch/ovs-vswitchd.ctl
            - version
          initialDelaySeconds: 5
          periodSeconds: 5
          timeoutSeconds: 5
        resources:
          limits:
            cpu: "2"
            memory: 2Gi
          requests:
            cpu: 200m
            memory: 512Mi
        securityContext:
          allowPrivilegeEscalation: false
          capabilities:
            add:
            - NET_ADMIN
            - SYS_NICE
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
        - mountPath: /var/log/openvswitch
          name: log-ovs
        - mountPath: /tmp
          name: tmp
        - mountPath: /etc/ovn-chassis/bin
          name: scripts
      dnsPolicy: ClusterFirstWithHostNet
      hostNetwork: true
      initContainers:
      - command:
        - /bin/bash
        - /etc/ovn-chassis/bin/host-prepare.sh
        image: registry.example.com/ovn@sha256:1111111111111111111111111111111111111111111111111111111111111111
        name: host-prepare
        resources: {}
        securityContext:
          allowPrivilegeEscalation: true
          privileged: true
          readOnlyRootFilesystem: true
          runAsUser: 0
        volumeMounts:
        - mountPath: /lib/modules
          name: modules
          readOnly: true
        - mountPath: /run/openvswitch
          name: run-ovs
        - mountPath: /run/ovn
          name: run-ovn
        - mountPath: /etc/ovn-chassis/bin
          name: scripts
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
      - hostPath:
          path: /lib/modules
        name: modules
      - emptyDir: {}
        name: log-ovs
      - emptyDir: {}
        name: tmp
      - configMap:
          defaultMode: 365
          name: chassis-chassis-scripts
        name: scripts
  updateStrategy:
    rollingUpdate:
      maxUnavailable: 2
    type: RollingUpdate
status:
  currentNumberScheduled: 0
  desiredNumberScheduled: 0
  numberMisscheduled: 0
  numberReady: 0
`

// TestPinOVSDaemonSet pins the Open vSwitch DaemonSet across a defaulted CR and
// one that moves every knob the builder reads.
func TestPinOVSDaemonSet(t *testing.T) {
	cases := []struct {
		name   string
		cr     func() *ovnv1alpha1.OVNChassis
		golden string
	}{
		{name: "default", cr: testOVNChassis, golden: pinOVSDaemonSetGolden},
		{name: "custom", cr: pinCustomOVSChassis, golden: pinCustomOVSDaemonSetGolden},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)

			got, err := yaml.Marshal(buildOVSDaemonSet(tc.cr()))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(string(got)).To(Equal(tc.golden),
				"the rendered ovs DaemonSet must stay byte-identical")
		})
	}
}

// The rollout blocks the four spellings of spec.updateStrategy render. They are
// pinned on their own rather than through four more full objects: the strategy
// is what decides how many nodes lose their dataplane programming at once, and
// it is the only part of the DaemonSet these inputs change.
const (
	pinStrategyDefaultGolden = `rollingUpdate:
  maxUnavailable: 1
type: RollingUpdate
`

	pinStrategyCountGolden = `rollingUpdate:
  maxUnavailable: 2
type: RollingUpdate
`

	pinStrategyPercentGolden = `rollingUpdate:
  maxUnavailable: 10%
type: RollingUpdate
`

	pinStrategyOnDeleteGolden = `type: OnDelete
`
)

// TestPinChassisUpdateStrategy pins the mapping from spec.updateStrategy onto
// the DaemonSet's own. A nil maxUnavailable renders 1, both spellings of a set
// one are passed through, OnDelete renders no rollingUpdate block at all, and an
// empty type counts as RollingUpdate: a CR that reached the operator without one
// bypassed admission, and a DaemonSet with an empty strategy type is rejected by
// the API server.
func TestPinChassisUpdateStrategy(t *testing.T) {
	cases := []struct {
		name     string
		strategy ovnv1alpha1.OVNChassisUpdateStrategy
		golden   string
	}{
		{
			name:     "nil maxUnavailable",
			strategy: ovnv1alpha1.OVNChassisUpdateStrategy{Type: "RollingUpdate"},
			golden:   pinStrategyDefaultGolden,
		},
		{
			name: "count",
			strategy: ovnv1alpha1.OVNChassisUpdateStrategy{
				Type: "RollingUpdate", MaxUnavailable: ptr.To(intstr.FromInt32(2)),
			},
			golden: pinStrategyCountGolden,
		},
		{
			name: "percentage",
			strategy: ovnv1alpha1.OVNChassisUpdateStrategy{
				Type: "RollingUpdate", MaxUnavailable: ptr.To(intstr.FromString("10%")),
			},
			golden: pinStrategyPercentGolden,
		},
		{
			name:     "on delete",
			strategy: ovnv1alpha1.OVNChassisUpdateStrategy{Type: "OnDelete"},
			golden:   pinStrategyOnDeleteGolden,
		},
		{
			name:     "empty type",
			strategy: ovnv1alpha1.OVNChassisUpdateStrategy{},
			golden:   pinStrategyDefaultGolden,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			cr := testOVNChassis()
			cr.Spec.UpdateStrategy = tc.strategy

			strategy := chassisUpdateStrategy(cr)

			got, err := yaml.Marshal(strategy)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(string(got)).To(Equal(tc.golden))
			g.Expect(strategy.Type).NotTo(BeEmpty(),
				"the API server rejects a DaemonSet whose update strategy has no type")
		})
	}
}

// A maxUnavailable the CR carries must not be shared with the object rendered
// from it: the applied DaemonSet outlives the pass that built it, and a pointer
// into the CR would let a later mutation of one change the other.
func TestChassisUpdateStrategy_CopiesMaxUnavailable(t *testing.T) {
	g := NewWithT(t)
	cr := testOVNChassis()
	cr.Spec.UpdateStrategy.MaxUnavailable = ptr.To(intstr.FromInt32(2))

	strategy := chassisUpdateStrategy(cr)

	g.Expect(strategy.RollingUpdate.MaxUnavailable).NotTo(BeIdenticalTo(cr.Spec.UpdateStrategy.MaxUnavailable))
	g.Expect(*strategy.RollingUpdate.MaxUnavailable).To(Equal(intstr.FromInt32(2)))
	g.Expect(strategy.Type).To(Equal(appsv1.RollingUpdateDaemonSetStrategyType))
}
