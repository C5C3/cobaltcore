// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Byte-identity pin for the two worker Deployments buildWorkerDeployment
// renders. The goldens below are FULL-OBJECT YAML captured from the builder as
// it stands today, so what the workers run, mount and select surfaces as a diff
// rather than as a silent pod-template churn that rolls every Neutron worker in
// the fleet on an operator upgrade.
package controller

import (
	"testing"

	. "github.com/onsi/gomega"
	"sigs.k8s.io/yaml"
)

// pinPeriodicWorkersGolden is the periodic-workers Deployment of the shared
// fixture. It runs the same two config files the API pods read and carries
// neither a port nor a probe: nothing dials it, and its work is a queue rather
// than a request.
const pinPeriodicWorkersGolden = `metadata:
  labels:
    app.kubernetes.io/component: periodic-workers
    app.kubernetes.io/instance: neutron
    app.kubernetes.io/managed-by: neutron-operator
    app.kubernetes.io/name: neutron
  name: neutron-periodic-workers
  namespace: openstack
spec:
  replicas: 3
  selector:
    matchLabels:
      app.kubernetes.io/component: periodic-workers
      app.kubernetes.io/instance: neutron
      app.kubernetes.io/name: neutron
  strategy:
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
    type: RollingUpdate
  template:
    metadata:
      labels:
        app.kubernetes.io/component: periodic-workers
        app.kubernetes.io/instance: neutron
        app.kubernetes.io/managed-by: neutron-operator
        app.kubernetes.io/name: neutron
    spec:
      containers:
      - command:
        - neutron-periodic-workers
        - --config-file
        - /etc/neutron/neutron.conf
        - --config-file
        - /etc/neutron/ml2_conf.ini
        env:
        - name: OS_DATABASE__CONNECTION
          valueFrom:
            secretKeyRef:
              key: connection
              name: neutron-db-connection
        - name: OS_DEFAULT__TRANSPORT_URL
          valueFrom:
            secretKeyRef:
              key: transport_url
              name: neutron-transport-url
        image: ghcr.io/c5c3/neutron:2026.1
        lifecycle:
          preStop:
            exec:
              command:
              - /bin/sh
              - -c
              - sleep 5
        name: periodic-workers
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
        - mountPath: /etc/neutron
          name: config
          readOnly: true
        - mountPath: /etc/ovn/tls
          name: ovn-tls
          readOnly: true
        - mountPath: /var/lib/neutron
          name: state
      securityContext:
        fsGroup: 42424
      terminationGracePeriodSeconds: 30
      topologySpreadConstraints:
      - labelSelector:
          matchLabels:
            app.kubernetes.io/component: periodic-workers
            app.kubernetes.io/instance: neutron
            app.kubernetes.io/name: neutron
        maxSkew: 1
        topologyKey: topology.kubernetes.io/zone
        whenUnsatisfiable: ScheduleAnyway
      - labelSelector:
          matchLabels:
            app.kubernetes.io/component: periodic-workers
            app.kubernetes.io/instance: neutron
            app.kubernetes.io/name: neutron
        maxSkew: 1
        topologyKey: kubernetes.io/hostname
        whenUnsatisfiable: ScheduleAnyway
      volumes:
      - configMap:
          name: neutron-config-abc
        name: config
      - name: ovn-tls
        secret:
          defaultMode: 256
          secretName: neutron-ovn-client
      - emptyDir: {}
        name: state
status: {}
`

// pinOVNMaintenanceWorkerGolden is the ovn-maintenance-worker Deployment. It
// differs from its sibling in three places only — the object name, the component
// label and selector, and the binary — which is what the two goldens side by
// side make visible.
const pinOVNMaintenanceWorkerGolden = `metadata:
  labels:
    app.kubernetes.io/component: ovn-maintenance-worker
    app.kubernetes.io/instance: neutron
    app.kubernetes.io/managed-by: neutron-operator
    app.kubernetes.io/name: neutron
  name: neutron-ovn-maintenance-worker
  namespace: openstack
spec:
  replicas: 3
  selector:
    matchLabels:
      app.kubernetes.io/component: ovn-maintenance-worker
      app.kubernetes.io/instance: neutron
      app.kubernetes.io/name: neutron
  strategy:
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
    type: RollingUpdate
  template:
    metadata:
      labels:
        app.kubernetes.io/component: ovn-maintenance-worker
        app.kubernetes.io/instance: neutron
        app.kubernetes.io/managed-by: neutron-operator
        app.kubernetes.io/name: neutron
    spec:
      containers:
      - command:
        - neutron-ovn-maintenance-worker
        - --config-file
        - /etc/neutron/neutron.conf
        - --config-file
        - /etc/neutron/ml2_conf.ini
        env:
        - name: OS_DATABASE__CONNECTION
          valueFrom:
            secretKeyRef:
              key: connection
              name: neutron-db-connection
        - name: OS_DEFAULT__TRANSPORT_URL
          valueFrom:
            secretKeyRef:
              key: transport_url
              name: neutron-transport-url
        image: ghcr.io/c5c3/neutron:2026.1
        lifecycle:
          preStop:
            exec:
              command:
              - /bin/sh
              - -c
              - sleep 5
        name: ovn-maintenance-worker
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
        - mountPath: /etc/neutron
          name: config
          readOnly: true
        - mountPath: /etc/ovn/tls
          name: ovn-tls
          readOnly: true
        - mountPath: /var/lib/neutron
          name: state
      securityContext:
        fsGroup: 42424
      terminationGracePeriodSeconds: 30
      topologySpreadConstraints:
      - labelSelector:
          matchLabels:
            app.kubernetes.io/component: ovn-maintenance-worker
            app.kubernetes.io/instance: neutron
            app.kubernetes.io/name: neutron
        maxSkew: 1
        topologyKey: topology.kubernetes.io/zone
        whenUnsatisfiable: ScheduleAnyway
      - labelSelector:
          matchLabels:
            app.kubernetes.io/component: ovn-maintenance-worker
            app.kubernetes.io/instance: neutron
            app.kubernetes.io/name: neutron
        maxSkew: 1
        topologyKey: kubernetes.io/hostname
        whenUnsatisfiable: ScheduleAnyway
      volumes:
      - configMap:
          name: neutron-config-abc
        name: config
      - name: ovn-tls
        secret:
          defaultMode: 256
          secretName: neutron-ovn-client
      - emptyDir: {}
        name: state
status: {}
`

// TestPinWorkerDeployments pins both worker Deployments. Their selectors are the
// load-bearing part: each narrows on its own component, so neither Deployment
// adopts the other's pods nor the API's.
func TestPinWorkerDeployments(t *testing.T) {
	cases := []struct {
		name      string
		component string
		binary    string
		golden    string
	}{
		{
			name:      "periodic-workers",
			component: componentPeriodicWorkers,
			binary:    "neutron-periodic-workers",
			golden:    pinPeriodicWorkersGolden,
		},
		{
			name:      "ovn-maintenance-worker",
			component: componentOVNMaintenanceWorker,
			binary:    "neutron-ovn-maintenance-worker",
			golden:    pinOVNMaintenanceWorkerGolden,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)

			got, err := yaml.Marshal(buildWorkerDeployment(validNeutron(), tc.component,
				neutronCommand(tc.binary), pinDeploymentConfigMapName, "", "", "", ""))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(string(got)).To(Equal(tc.golden),
				"the rendered Neutron worker Deployment must stay byte-identical")
		})
	}
}
