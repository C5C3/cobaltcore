// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Byte-identity pin for the console proxy Deployment. The goldens below are
// FULL-OBJECT YAML captured from the builder as it stands today, so a changed
// field order or a dropped default surfaces here as a diff instead of as a
// silent pod-template churn that cuts every console session on an operator
// upgrade.
package controller

import (
	"strings"
	"testing"

	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
)

// pinNovaConsoleProxyDeploymentDefaultGolden is the console proxy Deployment of
// the shared fixture.
const pinNovaConsoleProxyDeploymentDefaultGolden = `metadata:
  labels:
    app.kubernetes.io/component: novncproxy
    app.kubernetes.io/instance: nova
    app.kubernetes.io/managed-by: nova-operator
    app.kubernetes.io/name: nova
  name: nova-novncproxy
  namespace: openstack
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/component: novncproxy
      app.kubernetes.io/instance: nova
      app.kubernetes.io/name: nova
  strategy:
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
    type: RollingUpdate
  template:
    metadata:
      annotations:
        nova.c5c3.io/api-db-connection-hash: apidsn1
        nova.c5c3.io/authtoken-hash: auth3
        nova.c5c3.io/db-connection-hash: celldsn2
        nova.c5c3.io/installed-release: "2025.2"
        nova.c5c3.io/transport-url-hash: bus4
      labels:
        app.kubernetes.io/component: novncproxy
        app.kubernetes.io/instance: nova
        app.kubernetes.io/managed-by: nova-operator
        app.kubernetes.io/name: nova
    spec:
      containers:
      - command:
        - nova-novncproxy
        - --config-dir
        - /etc/nova/nova.conf.d
        - --config-dir
        - /etc/nova/novncproxy.conf.d
        env:
        - name: OS_DEFAULT__TRANSPORT_URL
          valueFrom:
            secretKeyRef:
              key: transport_url
              name: nova-transport-url
        - name: OS_DATABASE__CONNECTION
          valueFrom:
            secretKeyRef:
              key: connection
              name: nova-db-connection
        - name: OS_KEYSTONE_AUTHTOKEN__PASSWORD
          valueFrom:
            secretKeyRef:
              key: password
              name: nova-service-user
        - name: OS_SERVICE_USER__PASSWORD
          valueFrom:
            secretKeyRef:
              key: password
              name: nova-service-user
        - name: OS_PLACEMENT__PASSWORD
          valueFrom:
            secretKeyRef:
              key: password
              name: nova-service-user
        - name: OS_NEUTRON__PASSWORD
          valueFrom:
            secretKeyRef:
              key: password
              name: nova-service-user
        - name: OS_CINDER__PASSWORD
          valueFrom:
            secretKeyRef:
              key: password
              name: nova-service-user
        image: ghcr.io/c5c3/nova:2025.2
        lifecycle:
          preStop:
            exec:
              command:
              - /bin/sh
              - -c
              - sleep 5
        name: novncproxy
        ports:
        - containerPort: 6080
          name: novncproxy
        readinessProbe:
          failureThreshold: 3
          httpGet:
            path: /vnc_lite.html
            port: 6080
          initialDelaySeconds: 10
          periodSeconds: 15
          timeoutSeconds: 10
        resources:
          limits:
            memory: 368Mi
          requests:
            cpu: 100m
            memory: 368Mi
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
        - mountPath: /etc/nova/nova.conf.d
          name: config
          readOnly: true
        - mountPath: /etc/nova/novncproxy.conf.d
          name: novncproxy-overlay
          readOnly: true
        - mountPath: /var/lib/nova
          name: state
        - mountPath: /tmp
          name: tmp
      securityContext:
        fsGroup: 42424
      terminationGracePeriodSeconds: 30
      topologySpreadConstraints:
      - labelSelector:
          matchLabels:
            app.kubernetes.io/component: novncproxy
            app.kubernetes.io/instance: nova
            app.kubernetes.io/name: nova
        maxSkew: 1
        topologyKey: topology.kubernetes.io/zone
        whenUnsatisfiable: ScheduleAnyway
      - labelSelector:
          matchLabels:
            app.kubernetes.io/component: novncproxy
            app.kubernetes.io/instance: nova
            app.kubernetes.io/name: nova
        maxSkew: 1
        topologyKey: kubernetes.io/hostname
        whenUnsatisfiable: ScheduleAnyway
      volumes:
      - configMap:
          items:
          - key: logging.ini
            path: logging.ini
          - key: nova.conf
            path: nova.conf
          name: nova-config-abc123
        name: config
      - configMap:
          items:
          - key: novncproxy.conf
            path: novncproxy.conf
          name: nova-config-abc123
        name: novncproxy-overlay
      - emptyDir: {}
        name: state
      - emptyDir: {}
        name: tmp
status: {}
`

// pinNovaConsoleProxyDeploymentTLSGolden is the same Deployment for a Nova that
// verifies every transport: both schemas' client keypairs and the broker's CA
// bundle.
const pinNovaConsoleProxyDeploymentTLSGolden = `metadata:
  labels:
    app.kubernetes.io/component: novncproxy
    app.kubernetes.io/instance: nova
    app.kubernetes.io/managed-by: nova-operator
    app.kubernetes.io/name: nova
  name: nova-novncproxy
  namespace: openstack
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/component: novncproxy
      app.kubernetes.io/instance: nova
      app.kubernetes.io/name: nova
  strategy:
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
    type: RollingUpdate
  template:
    metadata:
      annotations:
        nova.c5c3.io/api-db-connection-hash: apidsn1
        nova.c5c3.io/authtoken-hash: auth3
        nova.c5c3.io/db-connection-hash: celldsn2
        nova.c5c3.io/installed-release: "2025.2"
        nova.c5c3.io/transport-url-hash: bus4
      labels:
        app.kubernetes.io/component: novncproxy
        app.kubernetes.io/instance: nova
        app.kubernetes.io/managed-by: nova-operator
        app.kubernetes.io/name: nova
    spec:
      containers:
      - command:
        - nova-novncproxy
        - --config-dir
        - /etc/nova/nova.conf.d
        - --config-dir
        - /etc/nova/novncproxy.conf.d
        env:
        - name: OS_DEFAULT__TRANSPORT_URL
          valueFrom:
            secretKeyRef:
              key: transport_url
              name: nova-transport-url
        - name: OS_DATABASE__CONNECTION
          valueFrom:
            secretKeyRef:
              key: connection
              name: nova-db-connection
        - name: OS_KEYSTONE_AUTHTOKEN__PASSWORD
          valueFrom:
            secretKeyRef:
              key: password
              name: nova-service-user
        - name: OS_SERVICE_USER__PASSWORD
          valueFrom:
            secretKeyRef:
              key: password
              name: nova-service-user
        - name: OS_PLACEMENT__PASSWORD
          valueFrom:
            secretKeyRef:
              key: password
              name: nova-service-user
        - name: OS_NEUTRON__PASSWORD
          valueFrom:
            secretKeyRef:
              key: password
              name: nova-service-user
        - name: OS_CINDER__PASSWORD
          valueFrom:
            secretKeyRef:
              key: password
              name: nova-service-user
        image: ghcr.io/c5c3/nova:2025.2
        lifecycle:
          preStop:
            exec:
              command:
              - /bin/sh
              - -c
              - sleep 5
        name: novncproxy
        ports:
        - containerPort: 6080
          name: novncproxy
        readinessProbe:
          failureThreshold: 3
          httpGet:
            path: /vnc_lite.html
            port: 6080
          initialDelaySeconds: 10
          periodSeconds: 15
          timeoutSeconds: 10
        resources:
          limits:
            memory: 368Mi
          requests:
            cpu: 100m
            memory: 368Mi
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
        - mountPath: /etc/nova/nova.conf.d
          name: config
          readOnly: true
        - mountPath: /etc/nova/novncproxy.conf.d
          name: novncproxy-overlay
          readOnly: true
        - mountPath: /var/lib/nova
          name: state
        - mountPath: /tmp
          name: tmp
        - mountPath: /etc/nova-db-tls/api/
          name: api-db-tls
          readOnly: true
        - mountPath: /etc/nova-db-tls/cell/
          name: cell-db-tls
          readOnly: true
        - mountPath: /etc/rabbitmq-ca
          name: rabbitmq-ca
          readOnly: true
      securityContext:
        fsGroup: 42424
      terminationGracePeriodSeconds: 30
      topologySpreadConstraints:
      - labelSelector:
          matchLabels:
            app.kubernetes.io/component: novncproxy
            app.kubernetes.io/instance: nova
            app.kubernetes.io/name: nova
        maxSkew: 1
        topologyKey: topology.kubernetes.io/zone
        whenUnsatisfiable: ScheduleAnyway
      - labelSelector:
          matchLabels:
            app.kubernetes.io/component: novncproxy
            app.kubernetes.io/instance: nova
            app.kubernetes.io/name: nova
        maxSkew: 1
        topologyKey: kubernetes.io/hostname
        whenUnsatisfiable: ScheduleAnyway
      volumes:
      - configMap:
          items:
          - key: logging.ini
            path: logging.ini
          - key: nova.conf
            path: nova.conf
          name: nova-config-abc123
        name: config
      - configMap:
          items:
          - key: novncproxy.conf
            path: novncproxy.conf
          name: nova-config-abc123
        name: novncproxy-overlay
      - emptyDir: {}
        name: state
      - emptyDir: {}
        name: tmp
      - name: api-db-tls
        projected:
          defaultMode: 256
          sources:
          - secret:
              items:
              - key: ca.crt
                path: ca.crt
              name: db-ca
          - secret:
              items:
              - key: tls.crt
                path: tls.crt
              - key: tls.key
                path: tls.key
              name: db-client
      - name: cell-db-tls
        projected:
          defaultMode: 256
          sources:
          - secret:
              items:
              - key: ca.crt
                path: ca.crt
              name: db-ca
          - secret:
              items:
              - key: tls.crt
                path: tls.crt
              - key: tls.key
                path: tls.key
              name: db-client
      - name: rabbitmq-ca
        secret:
          defaultMode: 292
          items:
          - key: ca.crt
            path: ca.crt
          secretName: nova-messaging-ca
status: {}
`

// TestPinNovaConsoleProxyDeployment pins the console proxy: the noVNC client
// page the readiness probe measures, its overlay directory, and the single
// database connection it opens. The fixture digests go in because the hash
// annotations are the only thing that rolls a running proxy once the broker,
// database or service-user credential it started with is rotated. Both shapes
// are rendered at both supported releases, and the second render must differ
// from the first in the release strings alone, which is what keeps a
// release-conditional field out of the builder.
func TestPinNovaConsoleProxyDeployment(t *testing.T) {
	cases := []struct {
		name   string
		nova   func() *novav1alpha1.Nova
		golden string
	}{
		{name: "default", nova: validNova, golden: pinNovaConsoleProxyDeploymentDefaultGolden},
		{name: "tls", nova: tlsNova, golden: pinNovaConsoleProxyDeploymentTLSGolden},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expectGolden(t, renderYAML(t, buildConsoleProxyDeployment(tc.nova(), workloadArtifacts(),
				workloadTestDigests())), tc.golden)
			expectGolden(t, renderYAML(t, buildConsoleProxyDeployment(atRelease(tc.nova(), "2026.1"), workloadArtifacts(),
				workloadTestDigests())),
				strings.ReplaceAll(tc.golden, "2025.2", "2026.1"))
		})
	}
}
