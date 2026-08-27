// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Byte-identity pin for the metadata-agent DaemonSet. The goldens below are
// FULL-OBJECT YAML captured from the builder as it stands today, so any refactor
// of the projection has to reproduce every rendered byte.
//
// The pod is where an instance's metadata request is answered: it runs in the
// node's network namespace, shares the chassis's local database socket, and
// creates its proxy namespaces on the node itself. Every one of those is a
// rendered field rather than a runtime decision.
package controller

import (
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
)

// pinAgentConfigMapName is the rendered config ConfigMap the goldens mount. The
// name is content-addressed in production; pinning a literal here keeps the
// goldens stable while still proving the volume names what the config step
// returned.
const pinAgentConfigMapName = testAgentName + "-config-abcdef12"

// pinAgentChassis is the resolved chassis the goldens are rendered against: a
// node selection and a toleration the agent copies verbatim, the Southbound
// address of the chassis's central, and the client Secret that central
// published.
func pinAgentChassis() resolvedChassis {
	return resolvedChassis{
		nodeSelector: map[string]string{testChassisNodeLabel: "true"},
		tolerations: []corev1.Toleration{{
			Key:      "openstack.c5c3.io/network",
			Operator: corev1.TolerationOpExists,
			Effect:   corev1.TaintEffectNoSchedule,
		}},
		sbAddress:        testSouthboundAddress,
		clientSecretName: "ovn-client",
	}
}

// pinMessagingAndNovaAgent sets both optional blocks, the only configuration in
// which the pod carries environment at all: one env var per block, plus the
// pod-template annotation that rolls the pods when the broker credential
// rotates.
func pinMessagingAndNovaAgent() *neutronv1alpha1.NeutronMetadataAgent {
	cr := validAgent()
	cr.Spec.Messaging = &commonv1.MessagingSpec{
		ClusterRef: &corev1.LocalObjectReference{Name: testRabbitmqClusterName},
		Replicas:   3,
	}
	cr.Spec.NovaMetadata = &neutronv1alpha1.NovaMetadataSpec{
		Host:            "nova-metadata.openstack.svc",
		Port:            8775,
		SharedSecretRef: &commonv1.SecretRefSpec{Name: "nova-metadata-secret", Key: "shared_secret"},
	}
	return cr
}

// pinCustomResourcesAgent pins an image by digest and names both resource
// blocks, so the golden shows the CR's own requests and limits rather than the
// shared container defaults.
func pinCustomResourcesAgent() *neutronv1alpha1.NeutronMetadataAgent {
	cr := validAgent()
	cr.Spec.Image = commonv1.ImageSpec{
		Repository: "registry.example.com/neutron",
		Digest:     "sha256:2222222222222222222222222222222222222222222222222222222222222222",
	}
	cr.Spec.Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("50m"),
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("1"),
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
	}
	return cr
}

// pinAgentDaemonSetGolden is the defaulted agent: no bus, no Nova metadata
// API, so the container carries no environment and the pod template no
// annotation, and both containers run on the shared resource defaults.
const pinAgentDaemonSetGolden = `metadata:
  labels:
    app.kubernetes.io/component: metadata-agent
    app.kubernetes.io/instance: neutron-metadata
    app.kubernetes.io/managed-by: neutronmetadataagent-operator
    app.kubernetes.io/name: neutronmetadataagent
  name: neutron-metadata-metadata-agent
  namespace: openstack
spec:
  selector:
    matchLabels:
      app.kubernetes.io/component: metadata-agent
      app.kubernetes.io/instance: neutron-metadata
      app.kubernetes.io/name: neutronmetadataagent
  template:
    metadata:
      labels:
        app.kubernetes.io/component: metadata-agent
        app.kubernetes.io/instance: neutron-metadata
        app.kubernetes.io/managed-by: neutronmetadataagent-operator
        app.kubernetes.io/name: neutronmetadataagent
    spec:
      containers:
      - command:
        - neutron-ovn-metadata-agent
        - --config-file
        - /etc/neutron/neutron_ovn_metadata_agent.ini
        image: ghcr.io/c5c3/neutron:2026.1
        name: metadata-agent
        readinessProbe:
          exec:
            command:
            - test
            - -S
            - /var/lib/neutron/metadata_proxy
          initialDelaySeconds: 5
          periodSeconds: 5
          timeoutSeconds: 5
        resources:
          limits:
            cpu: 500m
            memory: 512Mi
          requests:
            cpu: 100m
            memory: 256Mi
        securityContext:
          allowPrivilegeEscalation: true
          privileged: true
          readOnlyRootFilesystem: true
          runAsNonRoot: false
          runAsUser: 0
        volumeMounts:
        - mountPath: /run/openvswitch
          name: run-ovs
        - mountPath: /run/netns
          mountPropagation: Bidirectional
          name: run-netns
        - mountPath: /etc/neutron
          name: config
          readOnly: true
        - mountPath: /etc/ovn/tls
          name: ovn-tls
          readOnly: true
        - mountPath: /var/lib/neutron
          name: state
        - mountPath: /tmp
          name: tmp
      dnsPolicy: ClusterFirstWithHostNet
      hostNetwork: true
      initContainers:
      - command:
        - /bin/sh
        - -c
        - until ovsdb-client --timeout=5 transact unix:/run/openvswitch/db.sock '["Open_vSwitch",{"op":"select","table":"Open_vSwitch","where":[],"columns":["external_ids"]}]'
          2>/dev/null | grep -q system-id; do sleep 2; done
        image: ghcr.io/c5c3/neutron:2026.1
        name: wait-for-chassis
        resources:
          limits:
            cpu: 500m
            memory: 512Mi
          requests:
            cpu: 100m
            memory: 256Mi
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
          path: /run/netns
          type: DirectoryOrCreate
        name: run-netns
      - configMap:
          name: neutron-metadata-config-abcdef12
        name: config
      - name: ovn-tls
        secret:
          secretName: ovn-client
      - emptyDir: {}
        name: state
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

// pinMessagingAndNovaDaemonSetGolden is the agent that names both optional
// blocks: the two env vars, sourced from the derived transport-URL Secret and
// from the referenced shared-secret Secret, and the transport-url annotation.
const pinMessagingAndNovaDaemonSetGolden = `metadata:
  labels:
    app.kubernetes.io/component: metadata-agent
    app.kubernetes.io/instance: neutron-metadata
    app.kubernetes.io/managed-by: neutronmetadataagent-operator
    app.kubernetes.io/name: neutronmetadataagent
  name: neutron-metadata-metadata-agent
  namespace: openstack
spec:
  selector:
    matchLabels:
      app.kubernetes.io/component: metadata-agent
      app.kubernetes.io/instance: neutron-metadata
      app.kubernetes.io/name: neutronmetadataagent
  template:
    metadata:
      annotations:
        neutron.c5c3.io/transport-url-hash: b2c3d4
      labels:
        app.kubernetes.io/component: metadata-agent
        app.kubernetes.io/instance: neutron-metadata
        app.kubernetes.io/managed-by: neutronmetadataagent-operator
        app.kubernetes.io/name: neutronmetadataagent
    spec:
      containers:
      - command:
        - neutron-ovn-metadata-agent
        - --config-file
        - /etc/neutron/neutron_ovn_metadata_agent.ini
        env:
        - name: OS_DEFAULT__TRANSPORT_URL
          valueFrom:
            secretKeyRef:
              key: transport_url
              name: neutron-metadata-transport-url
        - name: OS_DEFAULT__METADATA_PROXY_SHARED_SECRET
          valueFrom:
            secretKeyRef:
              key: shared_secret
              name: nova-metadata-secret
        image: ghcr.io/c5c3/neutron:2026.1
        name: metadata-agent
        readinessProbe:
          exec:
            command:
            - test
            - -S
            - /var/lib/neutron/metadata_proxy
          initialDelaySeconds: 5
          periodSeconds: 5
          timeoutSeconds: 5
        resources:
          limits:
            cpu: 500m
            memory: 512Mi
          requests:
            cpu: 100m
            memory: 256Mi
        securityContext:
          allowPrivilegeEscalation: true
          privileged: true
          readOnlyRootFilesystem: true
          runAsNonRoot: false
          runAsUser: 0
        volumeMounts:
        - mountPath: /run/openvswitch
          name: run-ovs
        - mountPath: /run/netns
          mountPropagation: Bidirectional
          name: run-netns
        - mountPath: /etc/neutron
          name: config
          readOnly: true
        - mountPath: /etc/ovn/tls
          name: ovn-tls
          readOnly: true
        - mountPath: /var/lib/neutron
          name: state
        - mountPath: /tmp
          name: tmp
      dnsPolicy: ClusterFirstWithHostNet
      hostNetwork: true
      initContainers:
      - command:
        - /bin/sh
        - -c
        - until ovsdb-client --timeout=5 transact unix:/run/openvswitch/db.sock '["Open_vSwitch",{"op":"select","table":"Open_vSwitch","where":[],"columns":["external_ids"]}]'
          2>/dev/null | grep -q system-id; do sleep 2; done
        image: ghcr.io/c5c3/neutron:2026.1
        name: wait-for-chassis
        resources:
          limits:
            cpu: 500m
            memory: 512Mi
          requests:
            cpu: 100m
            memory: 256Mi
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
          path: /run/netns
          type: DirectoryOrCreate
        name: run-netns
      - configMap:
          name: neutron-metadata-config-abcdef12
        name: config
      - name: ovn-tls
        secret:
          secretName: ovn-client
      - emptyDir: {}
        name: state
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

// pinCustomResourcesDaemonSetGolden is the agent that pins its image by digest
// and names its own requests and limits.
const pinCustomResourcesDaemonSetGolden = `metadata:
  labels:
    app.kubernetes.io/component: metadata-agent
    app.kubernetes.io/instance: neutron-metadata
    app.kubernetes.io/managed-by: neutronmetadataagent-operator
    app.kubernetes.io/name: neutronmetadataagent
  name: neutron-metadata-metadata-agent
  namespace: openstack
spec:
  selector:
    matchLabels:
      app.kubernetes.io/component: metadata-agent
      app.kubernetes.io/instance: neutron-metadata
      app.kubernetes.io/name: neutronmetadataagent
  template:
    metadata:
      labels:
        app.kubernetes.io/component: metadata-agent
        app.kubernetes.io/instance: neutron-metadata
        app.kubernetes.io/managed-by: neutronmetadataagent-operator
        app.kubernetes.io/name: neutronmetadataagent
    spec:
      containers:
      - command:
        - neutron-ovn-metadata-agent
        - --config-file
        - /etc/neutron/neutron_ovn_metadata_agent.ini
        image: registry.example.com/neutron@sha256:2222222222222222222222222222222222222222222222222222222222222222
        name: metadata-agent
        readinessProbe:
          exec:
            command:
            - test
            - -S
            - /var/lib/neutron/metadata_proxy
          initialDelaySeconds: 5
          periodSeconds: 5
          timeoutSeconds: 5
        resources:
          limits:
            cpu: "1"
            memory: 512Mi
          requests:
            cpu: 50m
            memory: 128Mi
        securityContext:
          allowPrivilegeEscalation: true
          privileged: true
          readOnlyRootFilesystem: true
          runAsNonRoot: false
          runAsUser: 0
        volumeMounts:
        - mountPath: /run/openvswitch
          name: run-ovs
        - mountPath: /run/netns
          mountPropagation: Bidirectional
          name: run-netns
        - mountPath: /etc/neutron
          name: config
          readOnly: true
        - mountPath: /etc/ovn/tls
          name: ovn-tls
          readOnly: true
        - mountPath: /var/lib/neutron
          name: state
        - mountPath: /tmp
          name: tmp
      dnsPolicy: ClusterFirstWithHostNet
      hostNetwork: true
      initContainers:
      - command:
        - /bin/sh
        - -c
        - until ovsdb-client --timeout=5 transact unix:/run/openvswitch/db.sock '["Open_vSwitch",{"op":"select","table":"Open_vSwitch","where":[],"columns":["external_ids"]}]'
          2>/dev/null | grep -q system-id; do sleep 2; done
        image: registry.example.com/neutron@sha256:2222222222222222222222222222222222222222222222222222222222222222
        name: wait-for-chassis
        resources:
          limits:
            cpu: "1"
            memory: 512Mi
          requests:
            cpu: 50m
            memory: 128Mi
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
          path: /run/netns
          type: DirectoryOrCreate
        name: run-netns
      - configMap:
          name: neutron-metadata-config-abcdef12
        name: config
      - name: ovn-tls
        secret:
          secretName: ovn-client
      - emptyDir: {}
        name: state
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

// TestPinAgentDaemonSet pins the metadata-agent DaemonSet across the defaulted
// CR, the one that names both optional blocks, and the one that names its own
// resources.
func TestPinAgentDaemonSet(t *testing.T) {
	cases := []struct {
		name   string
		cr     func() *neutronv1alpha1.NeutronMetadataAgent
		digest string
		golden string
	}{
		{
			name:   "default",
			cr:     validAgent,
			golden: pinAgentDaemonSetGolden,
		},
		{
			name:   "messaging-and-nova",
			cr:     pinMessagingAndNovaAgent,
			digest: "b2c3d4",
			golden: pinMessagingAndNovaDaemonSetGolden,
		},
		{
			name:   "custom-resources",
			cr:     pinCustomResourcesAgent,
			golden: pinCustomResourcesDaemonSetGolden,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)

			got, err := yaml.Marshal(buildAgentDaemonSet(tc.cr(), pinAgentChassis(),
				pinAgentConfigMapName, tc.digest))

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(string(got)).To(Equal(tc.golden),
				"the rendered metadata-agent DaemonSet must stay byte-identical")
		})
	}
}
