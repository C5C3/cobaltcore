// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Byte-identity pin for the object buildSchedulerDeployment renders. The golden
// is FULL-OBJECT YAML captured from the builder as it stands today, so a change
// to the scheduler pod template surfaces here rather than as a silent roll of a
// process the whole fleet's volume placement runs through.
package controller

import (
	"testing"

	. "github.com/onsi/gomega"
	"sigs.k8s.io/yaml"
)

const pinSchedulerDeploymentGolden = `metadata:
  labels:
    app.kubernetes.io/component: scheduler
    app.kubernetes.io/instance: cinder
    app.kubernetes.io/managed-by: cinder-operator
    app.kubernetes.io/name: cinder
  name: cinder-scheduler
  namespace: openstack
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/component: scheduler
      app.kubernetes.io/instance: cinder
      app.kubernetes.io/name: cinder
  strategy:
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
    type: RollingUpdate
  template:
    metadata:
      annotations:
        cinder.c5c3.io/authtoken-hash: auth456
        cinder.c5c3.io/db-connection-hash: dsn123
        cinder.c5c3.io/installed-release: "2026.1"
        cinder.c5c3.io/transport-url-hash: bus789
      labels:
        app.kubernetes.io/component: scheduler
        app.kubernetes.io/instance: cinder
        app.kubernetes.io/managed-by: cinder-operator
        app.kubernetes.io/name: cinder
    spec:
      containers:
      - command:
        - cinder-scheduler
        - --config-dir
        - /etc/cinder/cinder.conf.d
        - --config-dir
        - /etc/cinder/scheduler.conf.d
        env:
        - name: OS_DATABASE__CONNECTION
          valueFrom:
            secretKeyRef:
              key: connection
              name: cinder-db-connection
        - name: OS_DEFAULT__TRANSPORT_URL
          valueFrom:
            secretKeyRef:
              key: transport_url
              name: cinder-transport-url
        - name: OS_KEYSTONE_AUTHTOKEN__PASSWORD
          valueFrom:
            secretKeyRef:
              key: password
              name: cinder-service-user
        - name: OS_SERVICE_USER__PASSWORD
          valueFrom:
            secretKeyRef:
              key: password
              name: cinder-service-user
        - name: CINDER_AMQP_PORT
          value: "5672"
        image: ghcr.io/c5c3/cinder:2026.1
        lifecycle:
          preStop:
            exec:
              command:
              - /bin/sh
              - -c
              - sleep 5
        name: scheduler
        readinessProbe:
          exec:
            command:
            - /var/lib/openstack/bin/cinder-amqp-ready
          failureThreshold: 2
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
        - mountPath: /etc/cinder/cinder.conf.d
          name: config
          readOnly: true
        - mountPath: /var/lib/cinder
          name: state
        - mountPath: /tmp
          name: tmp
        - mountPath: /etc/cinder/scheduler.conf.d
          name: scheduler-config
          readOnly: true
      securityContext:
        fsGroup: 42424
      terminationGracePeriodSeconds: 30
      topologySpreadConstraints:
      - labelSelector:
          matchLabels:
            app.kubernetes.io/component: scheduler
            app.kubernetes.io/instance: cinder
            app.kubernetes.io/name: cinder
        maxSkew: 1
        topologyKey: topology.kubernetes.io/zone
        whenUnsatisfiable: ScheduleAnyway
      - labelSelector:
          matchLabels:
            app.kubernetes.io/component: scheduler
            app.kubernetes.io/instance: cinder
            app.kubernetes.io/name: cinder
        maxSkew: 1
        topologyKey: kubernetes.io/hostname
        whenUnsatisfiable: ScheduleAnyway
      volumes:
      - configMap:
          items:
          - key: cinder.conf
            path: cinder.conf
          - key: policy.yaml
            path: policy.yaml
          name: cinder-config-abc123
        name: config
      - emptyDir: {}
        name: state
      - emptyDir: {}
        name: tmp
      - configMap:
          items:
          - key: scheduler.conf
            path: scheduler.conf
          name: cinder-config-abc123
        name: scheduler-config
status: {}
`

// TestPinSchedulerDeployment pins the scheduler pod template: the two config
// directories, the readiness probe taken off the message bus, and the release
// stamp that rolls the pod after a contract phase.
func TestPinSchedulerDeployment(t *testing.T) {
	g := NewWithT(t)

	got, err := yaml.Marshal(buildSchedulerDeployment(workloadCinder(), workloadArtifacts(),
		workloadTestDigests(), testEgressPort))

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(string(got)).To(Equal(pinSchedulerDeploymentGolden),
		"the rendered scheduler Deployment must stay byte-identical")
}
