// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Byte-identity pin for the objects buildVolumeDeployment and
// buildServiceRemoveJob render. Both are FULL-OBJECT YAML captured from the
// builders as they stand today: the pod template because a volume service owns
// its backend through a host identity that a silent roll interrupts, and the Job
// because it is what releases that identity again.
package controller

import (
	"testing"

	. "github.com/onsi/gomega"
	"sigs.k8s.io/yaml"
)

const pinVolumeDeploymentGolden = `metadata:
  labels:
    app.kubernetes.io/component: volume-nfs
    app.kubernetes.io/instance: cinder
    app.kubernetes.io/managed-by: cinder-operator
    app.kubernetes.io/name: cinder
  name: cinder-volume-nfs
  namespace: openstack
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/component: volume-nfs
      app.kubernetes.io/instance: cinder
      app.kubernetes.io/name: cinder
  strategy:
    type: Recreate
  template:
    metadata:
      annotations:
        cinder.c5c3.io/installed-release: "2026.1"
      labels:
        app.kubernetes.io/component: volume-nfs
        app.kubernetes.io/instance: cinder
        app.kubernetes.io/managed-by: cinder-operator
        app.kubernetes.io/name: cinder
    spec:
      containers:
      - command:
        - cinder-volume
        - --config-dir
        - /etc/cinder/cinder.conf.d
        - --config-dir
        - /etc/cinder/backends.conf.d
        - --config-dir
        - /etc/cinder/volume.conf.d
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
        name: volume-nfs
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
        - mountPath: /etc/cinder/backends.conf.d
          name: backend
          readOnly: true
        - mountPath: /etc/cinder/volume.conf.d
          name: volume-overlay
          readOnly: true
        - mountPath: /var/lib/cinder/mnt/6f3cb55ed3b423dbb7791aaf3783754f
          name: share-nfs
      securityContext:
        fsGroup: 42424
        fsGroupChangePolicy: OnRootMismatch
      terminationGracePeriodSeconds: 30
      topologySpreadConstraints:
      - labelSelector:
          matchLabels:
            app.kubernetes.io/component: volume-nfs
            app.kubernetes.io/instance: cinder
            app.kubernetes.io/name: cinder
        maxSkew: 1
        topologyKey: topology.kubernetes.io/zone
        whenUnsatisfiable: ScheduleAnyway
      - labelSelector:
          matchLabels:
            app.kubernetes.io/component: volume-nfs
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
      - name: backend
        secret:
          items:
          - key: backend.conf
            path: backend.conf
          - key: shares
            path: nfs.shares
          secretName: cinder-backend-nfs-abc123
      - name: volume-overlay
        secret:
          items:
          - key: volume.conf
            path: volume.conf
          secretName: cinder-backend-nfs-abc123
      - csi:
          driver: nfs.csi.k8s.io
          volumeAttributes:
            mountOptions: nfsvers=4.1,soft,timeo=30,retrans=2
            server: nfs-server.openstack.svc.cluster.local
            share: /volumes
        name: share-nfs
status: {}
`

const pinServiceRemoveJobGolden = `metadata:
  labels:
    app.kubernetes.io/component: service-remove
    app.kubernetes.io/instance: cinder
    app.kubernetes.io/managed-by: cinder-operator
    app.kubernetes.io/name: cinder
  name: cinder-nfs-service-remove
  namespace: openstack
spec:
  backoffLimit: 4
  template:
    metadata:
      labels:
        app.kubernetes.io/component: service-remove
        app.kubernetes.io/instance: cinder
        app.kubernetes.io/managed-by: cinder-operator
        app.kubernetes.io/name: cinder
    spec:
      containers:
      - command:
        - /bin/sh
        - -eu
        - -c
        - rc=0; cinder-manage --config-dir /etc/cinder/cinder.conf.d service remove
          cinder-volume cinder@nfs || rc=$?; case "$rc" in 0|2) exit 0;; *) exit "$rc";;
          esac
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
        image: ghcr.io/c5c3/cinder:2026.1
        name: service-remove
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
        - mountPath: /etc/cinder/cinder.conf.d
          name: config
          readOnly: true
      restartPolicy: Never
      volumes:
      - configMap:
          name: cinder-config-abc123
        name: config
  ttlSecondsAfterFinished: 3600
status: {}
`

// TestPinVolumeDeployment pins the pod one backend's volume service runs in: the
// three config directories, the export mounted where cinder resolves its volumes
// to, and the single-writer shape the NFS drivers require.
func TestPinVolumeDeployment(t *testing.T) {
	g := NewWithT(t)

	got, err := yaml.Marshal(buildVolumeDeployment(workloadCinder(), testBackendProjection("nfs"),
		workloadArtifacts(), workloadDigests{}, testEgressPort))

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(string(got)).To(Equal(pinVolumeDeploymentGolden),
		"the rendered volume Deployment must stay byte-identical")
}

// TestPinServiceRemoveJob pins the detach Job: the command that drops the host
// identity from the service registry, and the pod it runs in.
func TestPinServiceRemoveJob(t *testing.T) {
	g := NewWithT(t)

	got, err := yaml.Marshal(buildServiceRemoveJob(workloadCinder(), "nfs", workloadArtifacts()))

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(string(got)).To(Equal(pinServiceRemoveJobGolden),
		"the rendered service-remove Job must stay byte-identical")
}
