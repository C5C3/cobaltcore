// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Byte-identity pin for the object buildBackupDeployment renders. The golden is
// FULL-OBJECT YAML captured from the builder as it stands today, covering both
// sides of a backup: the target export it writes to and the volume export it
// reads its sources from.
package controller

import (
	"testing"

	. "github.com/onsi/gomega"
	"sigs.k8s.io/yaml"
)

const pinBackupDeploymentGolden = `metadata:
  labels:
    app.kubernetes.io/component: backup
    app.kubernetes.io/instance: cinder
    app.kubernetes.io/managed-by: cinder-operator
    app.kubernetes.io/name: cinder
  name: cinder-backup
  namespace: openstack
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/component: backup
      app.kubernetes.io/instance: cinder
      app.kubernetes.io/name: cinder
  strategy:
    type: Recreate
  template:
    metadata:
      annotations:
        cinder.c5c3.io/installed-release: "2026.1"
      labels:
        app.kubernetes.io/component: backup
        app.kubernetes.io/instance: cinder
        app.kubernetes.io/managed-by: cinder-operator
        app.kubernetes.io/name: cinder
    spec:
      containers:
      - command:
        - cinder-backup
        - --config-dir
        - /etc/cinder/cinder.conf.d
        - --config-dir
        - /etc/cinder/backup.conf.d
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
        name: backup
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
            memory: 2Gi
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
        - mountPath: /etc/cinder/backup.conf.d
          name: backup
          readOnly: true
        - mountPath: /var/lib/cinder/backup_mount/13d0a810a1b2ca9a5b45808fb8546a3e
          name: backup-share
        - mountPath: /var/lib/cinder/mnt/6f3cb55ed3b423dbb7791aaf3783754f
          name: share-nfs
      securityContext:
        fsGroup: 42424
        fsGroupChangePolicy: OnRootMismatch
      terminationGracePeriodSeconds: 30
      topologySpreadConstraints:
      - labelSelector:
          matchLabels:
            app.kubernetes.io/component: backup
            app.kubernetes.io/instance: cinder
            app.kubernetes.io/name: cinder
        maxSkew: 1
        topologyKey: topology.kubernetes.io/zone
        whenUnsatisfiable: ScheduleAnyway
      - labelSelector:
          matchLabels:
            app.kubernetes.io/component: backup
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
      - name: backup
        secret:
          items:
          - key: backup.conf
            path: backup.conf
          secretName: cinder-backup-backups-def456
      - csi:
          driver: nfs.csi.k8s.io
          volumeAttributes:
            mountOptions: nfsvers=4.1,soft,timeo=30,retrans=2
            server: backup-server.openstack.svc.cluster.local
            share: /backups
        name: backup-share
      - csi:
          driver: nfs.csi.k8s.io
          volumeAttributes:
            mountOptions: nfsvers=4.1,soft,timeo=30,retrans=2
            server: nfs-server.openstack.svc.cluster.local
            share: /volumes
        name: share-nfs
status: {}
`

// TestPinBackupDeployment pins the backup pod template with one volume backend
// projected, which is what puts a source mount beside the target one.
func TestPinBackupDeployment(t *testing.T) {
	g := NewWithT(t)

	got, err := yaml.Marshal(buildBackupDeployment(workloadCinder(), testBackupProjection(),
		[]backendProjection{testBackendProjection("nfs")}, workloadArtifacts(),
		workloadDigests{}, testEgressPort))

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(string(got)).To(Equal(pinBackupDeploymentGolden),
		"the rendered backup Deployment must stay byte-identical")
}
