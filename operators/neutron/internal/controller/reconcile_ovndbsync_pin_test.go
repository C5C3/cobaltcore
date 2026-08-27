// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Byte-identity pin for the CronJob buildOVNDBSyncCronJob renders. The goldens
// below are FULL-OBJECT YAML captured from the builder as it stands today. The
// object writes to the OVN Northbound database in repair mode, so what it runs,
// how long it may run, and whether it runs at all are pinned rather than
// asserted field by field.
package controller

import (
	"testing"

	. "github.com/onsi/gomega"
	"sigs.k8s.io/yaml"

	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
)

// pinOVNDBSyncLogGolden is the CronJob of a spec.ovnDBSync block that sets
// nothing: the hourly default schedule and the read-only log mode, which is the
// mode a CR gets by not choosing one.
const pinOVNDBSyncLogGolden = `metadata:
  labels:
    app.kubernetes.io/instance: neutron
    app.kubernetes.io/managed-by: neutron-operator
    app.kubernetes.io/name: neutron
  name: neutron-ovn-db-sync
  namespace: openstack
spec:
  concurrencyPolicy: Forbid
  jobTemplate:
    metadata:
      labels:
        app.kubernetes.io/component: ovn-db-sync
        app.kubernetes.io/instance: neutron
        app.kubernetes.io/managed-by: neutron-operator
        app.kubernetes.io/name: neutron
    spec:
      activeDeadlineSeconds: 3600
      backoffLimit: 0
      template:
        metadata:
          labels:
            app.kubernetes.io/component: ovn-db-sync
            app.kubernetes.io/instance: neutron
            app.kubernetes.io/managed-by: neutron-operator
            app.kubernetes.io/name: neutron
        spec:
          containers:
          - command:
            - neutron-ovn-db-sync-util
            - --config-file
            - /etc/neutron/neutron.conf
            - --config-file
            - /etc/neutron/ml2_conf.ini
            - --ovn-neutron_sync_mode
            - log
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
            name: ovn-db-sync
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
          restartPolicy: Never
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
  schedule: 0 * * * *
  suspend: false
status: {}
`

// pinOVNDBSyncRepairGolden is the CronJob of a CR that opted into rewriting the
// Northbound database on a schedule of its own.
const pinOVNDBSyncRepairGolden = `metadata:
  labels:
    app.kubernetes.io/instance: neutron
    app.kubernetes.io/managed-by: neutron-operator
    app.kubernetes.io/name: neutron
  name: neutron-ovn-db-sync
  namespace: openstack
spec:
  concurrencyPolicy: Forbid
  jobTemplate:
    metadata:
      labels:
        app.kubernetes.io/component: ovn-db-sync
        app.kubernetes.io/instance: neutron
        app.kubernetes.io/managed-by: neutron-operator
        app.kubernetes.io/name: neutron
    spec:
      activeDeadlineSeconds: 3600
      backoffLimit: 0
      template:
        metadata:
          labels:
            app.kubernetes.io/component: ovn-db-sync
            app.kubernetes.io/instance: neutron
            app.kubernetes.io/managed-by: neutron-operator
            app.kubernetes.io/name: neutron
        spec:
          containers:
          - command:
            - neutron-ovn-db-sync-util
            - --config-file
            - /etc/neutron/neutron.conf
            - --config-file
            - /etc/neutron/ml2_conf.ini
            - --ovn-neutron_sync_mode
            - repair
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
            name: ovn-db-sync
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
          restartPolicy: Never
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
  schedule: 0 3 * * *
  suspend: false
status: {}
`

// pinOVNDBSyncSuspendedGolden is the paused CronJob. It stays projected with
// every other field intact, so resuming the comparison is one field edit.
const pinOVNDBSyncSuspendedGolden = `metadata:
  labels:
    app.kubernetes.io/instance: neutron
    app.kubernetes.io/managed-by: neutron-operator
    app.kubernetes.io/name: neutron
  name: neutron-ovn-db-sync
  namespace: openstack
spec:
  concurrencyPolicy: Forbid
  jobTemplate:
    metadata:
      labels:
        app.kubernetes.io/component: ovn-db-sync
        app.kubernetes.io/instance: neutron
        app.kubernetes.io/managed-by: neutron-operator
        app.kubernetes.io/name: neutron
    spec:
      activeDeadlineSeconds: 3600
      backoffLimit: 0
      template:
        metadata:
          labels:
            app.kubernetes.io/component: ovn-db-sync
            app.kubernetes.io/instance: neutron
            app.kubernetes.io/managed-by: neutron-operator
            app.kubernetes.io/name: neutron
        spec:
          containers:
          - command:
            - neutron-ovn-db-sync-util
            - --config-file
            - /etc/neutron/neutron.conf
            - --config-file
            - /etc/neutron/ml2_conf.ini
            - --ovn-neutron_sync_mode
            - log
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
            name: ovn-db-sync
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
          restartPolicy: Never
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
  schedule: 0 * * * *
  suspend: true
status: {}
`

// TestPinOVNDBSyncCronJob pins the rendered CronJob across the three variants
// spec.ovnDBSync produces.
func TestPinOVNDBSyncCronJob(t *testing.T) {
	cases := []struct {
		name   string
		sync   neutronv1alpha1.OVNDBSyncSpec
		golden string
	}{
		{
			name:   "log",
			sync:   neutronv1alpha1.OVNDBSyncSpec{},
			golden: pinOVNDBSyncLogGolden,
		},
		{
			name:   "repair",
			sync:   neutronv1alpha1.OVNDBSyncSpec{SyncMode: "repair", Schedule: "0 3 * * *"},
			golden: pinOVNDBSyncRepairGolden,
		},
		{
			name:   "suspended",
			sync:   neutronv1alpha1.OVNDBSyncSpec{Suspend: true},
			golden: pinOVNDBSyncSuspendedGolden,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			neutron := validNeutron()
			neutron.Spec.OVNDBSync = &tc.sync

			got, err := yaml.Marshal(buildOVNDBSyncCronJob(neutron, pinDeploymentConfigMapName))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(string(got)).To(Equal(tc.golden),
				"the rendered ovn-db-sync CronJob must stay byte-identical")
		})
	}
}
