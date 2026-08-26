// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Byte-identity pin for the two relay children. The goldens below are
// FULL-OBJECT YAML captured from the builders as they stand today, so any
// refactor of the relay projection has to reproduce every rendered byte. The
// Southbound address reaches the pods as a command argument and the Service
// port is what every chassis is pointed at, so a change to either restarts or
// strands the whole relay tier.
package controller

import (
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// pinRelayOVNCentral is the defaulted relay fixture: two replicas and no
// resources, so the golden pins the shared defaults the relay inherits.
func pinRelayOVNCentral() *ovnv1alpha1.OVNCentral {
	cr := publishEndpoints(testOVNCentral())
	cr.Spec.Relay = &ovnv1alpha1.OVNRelaySpec{Replicas: 2}
	return cr
}

// pinCustomRelayOVNCentral moves both relay knobs off their defaults and pins
// the image by digest, so a builder that reads the wrong field is caught.
func pinCustomRelayOVNCentral() *ovnv1alpha1.OVNCentral {
	cr := pinRelayOVNCentral()
	cr.Spec.Image = &commonv1.ImageSpec{
		Repository: "registry.example.com/ovn",
		Digest:     "sha256:1111111111111111111111111111111111111111111111111111111111111111",
	}
	cr.Spec.Relay = &ovnv1alpha1.OVNRelaySpec{
		Replicas: 1,
		Resources: &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("50m"),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
		},
	}
	return cr
}

const pinRelayDeploymentGolden = `metadata:
  labels:
    app.kubernetes.io/component: sb-relay
    app.kubernetes.io/instance: ovn
    app.kubernetes.io/managed-by: ovncentral-operator
    app.kubernetes.io/name: ovncentral
  name: ovn-sb-relay
  namespace: openstack
spec:
  replicas: 2
  selector:
    matchLabels:
      app.kubernetes.io/component: sb-relay
      app.kubernetes.io/instance: ovn
      app.kubernetes.io/name: ovncentral
  strategy:
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
    type: RollingUpdate
  template:
    metadata:
      labels:
        app.kubernetes.io/component: sb-relay
        app.kubernetes.io/instance: ovn
        app.kubernetes.io/managed-by: ovncentral-operator
        app.kubernetes.io/name: ovncentral
    spec:
      containers:
      - command:
        - /usr/share/ovn/scripts/ovn-ctl
        - --db-sb-relay-remote=ssl:10.96.0.21:6642
        - --ovn-sb-relay-db-ssl-key=/etc/ovn/tls/tls.key
        - --ovn-sb-relay-db-ssl-cert=/etc/ovn/tls/tls.crt
        - --ovn-sb-relay-db-ssl-ca-cert=/etc/ovn/tls/ca.crt
        - run_sb_relay_ovsdb
        image: ghcr.io/c5c3/ovn:26.03.2
        lifecycle:
          preStop:
            exec:
              command:
              - /bin/sh
              - -c
              - sleep 5
        name: relay
        readinessProbe:
          exec:
            command:
            - ovsdb-client
            - -p
            - /etc/ovn/tls/tls.key
            - -c
            - /etc/ovn/tls/tls.crt
            - -C
            - /etc/ovn/tls/ca.crt
            - list-dbs
            - ssl:127.0.0.1:6642
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
        - mountPath: /var/run/ovn
          name: run
        - mountPath: /var/log/ovn
          name: log
        - mountPath: /tmp
          name: tmp
        - mountPath: /etc/ovn/tls
          name: tls
          readOnly: true
      securityContext:
        fsGroup: 42424
      terminationGracePeriodSeconds: 30
      topologySpreadConstraints:
      - labelSelector:
          matchLabels:
            app.kubernetes.io/component: sb-relay
            app.kubernetes.io/instance: ovn
            app.kubernetes.io/name: ovncentral
        maxSkew: 1
        topologyKey: topology.kubernetes.io/zone
        whenUnsatisfiable: ScheduleAnyway
      - labelSelector:
          matchLabels:
            app.kubernetes.io/component: sb-relay
            app.kubernetes.io/instance: ovn
            app.kubernetes.io/name: ovncentral
        maxSkew: 1
        topologyKey: kubernetes.io/hostname
        whenUnsatisfiable: ScheduleAnyway
      volumes:
      - emptyDir: {}
        name: run
      - emptyDir: {}
        name: log
      - emptyDir: {}
        name: tmp
      - name: tls
        secret:
          secretName: ovn-sb-relay
status: {}
`

const pinCustomRelayDeploymentGolden = `metadata:
  labels:
    app.kubernetes.io/component: sb-relay
    app.kubernetes.io/instance: ovn
    app.kubernetes.io/managed-by: ovncentral-operator
    app.kubernetes.io/name: ovncentral
  name: ovn-sb-relay
  namespace: openstack
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/component: sb-relay
      app.kubernetes.io/instance: ovn
      app.kubernetes.io/name: ovncentral
  strategy:
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
    type: RollingUpdate
  template:
    metadata:
      labels:
        app.kubernetes.io/component: sb-relay
        app.kubernetes.io/instance: ovn
        app.kubernetes.io/managed-by: ovncentral-operator
        app.kubernetes.io/name: ovncentral
    spec:
      containers:
      - command:
        - /usr/share/ovn/scripts/ovn-ctl
        - --db-sb-relay-remote=ssl:10.96.0.21:6642
        - --ovn-sb-relay-db-ssl-key=/etc/ovn/tls/tls.key
        - --ovn-sb-relay-db-ssl-cert=/etc/ovn/tls/tls.crt
        - --ovn-sb-relay-db-ssl-ca-cert=/etc/ovn/tls/ca.crt
        - run_sb_relay_ovsdb
        image: registry.example.com/ovn@sha256:1111111111111111111111111111111111111111111111111111111111111111
        lifecycle:
          preStop:
            exec:
              command:
              - /bin/sh
              - -c
              - sleep 5
        name: relay
        readinessProbe:
          exec:
            command:
            - ovsdb-client
            - -p
            - /etc/ovn/tls/tls.key
            - -c
            - /etc/ovn/tls/tls.crt
            - -C
            - /etc/ovn/tls/ca.crt
            - list-dbs
            - ssl:127.0.0.1:6642
          initialDelaySeconds: 5
          periodSeconds: 5
          timeoutSeconds: 5
        resources:
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
        - mountPath: /var/run/ovn
          name: run
        - mountPath: /var/log/ovn
          name: log
        - mountPath: /tmp
          name: tmp
        - mountPath: /etc/ovn/tls
          name: tls
          readOnly: true
      securityContext:
        fsGroup: 42424
      terminationGracePeriodSeconds: 30
      topologySpreadConstraints:
      - labelSelector:
          matchLabels:
            app.kubernetes.io/component: sb-relay
            app.kubernetes.io/instance: ovn
            app.kubernetes.io/name: ovncentral
        maxSkew: 1
        topologyKey: topology.kubernetes.io/zone
        whenUnsatisfiable: ScheduleAnyway
      - labelSelector:
          matchLabels:
            app.kubernetes.io/component: sb-relay
            app.kubernetes.io/instance: ovn
            app.kubernetes.io/name: ovncentral
        maxSkew: 1
        topologyKey: kubernetes.io/hostname
        whenUnsatisfiable: ScheduleAnyway
      volumes:
      - emptyDir: {}
        name: run
      - emptyDir: {}
        name: log
      - emptyDir: {}
        name: tmp
      - name: tls
        secret:
          secretName: ovn-sb-relay
status: {}
`

const pinRelayServiceGolden = `metadata:
  labels:
    app.kubernetes.io/component: sb-relay
    app.kubernetes.io/instance: ovn
    app.kubernetes.io/managed-by: ovncentral-operator
    app.kubernetes.io/name: ovncentral
  name: ovn-sb-relay
  namespace: openstack
spec:
  ports:
  - port: 6642
    protocol: TCP
    targetPort: 6642
  selector:
    app.kubernetes.io/component: sb-relay
    app.kubernetes.io/instance: ovn
    app.kubernetes.io/name: ovncentral
status:
  loadBalancer: {}
`

// TestPinRelayDeployment pins the relay workload across a defaulted relay block
// and one that moves both of its knobs.
func TestPinRelayDeployment(t *testing.T) {
	cases := []struct {
		name   string
		cr     func() *ovnv1alpha1.OVNCentral
		golden string
	}{
		{name: "default", cr: pinRelayOVNCentral, golden: pinRelayDeploymentGolden},
		{name: "custom", cr: pinCustomRelayOVNCentral, golden: pinCustomRelayDeploymentGolden},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)

			got, err := yaml.Marshal(buildRelayDeployment(tc.cr()))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(string(got)).To(Equal(tc.golden),
				"the rendered relay Deployment must stay byte-identical")
		})
	}
}

// TestPinRelayService pins the Service the chassis reach the relays through. It
// carries no clusterIP of its own: the API server assigns one, and a builder
// that set the field would make every apply fight it.
func TestPinRelayService(t *testing.T) {
	g := NewWithT(t)

	got, err := yaml.Marshal(buildRelayService(pinRelayOVNCentral()))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(string(got)).To(Equal(pinRelayServiceGolden),
		"the rendered relay Service must stay byte-identical")
}
