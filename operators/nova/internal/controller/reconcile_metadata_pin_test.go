// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Byte-identity pin for the metadata Deployment. The goldens below are
// FULL-OBJECT YAML captured from the builder as it stands today, so a changed
// field order or a dropped default surfaces here as a diff instead of as a
// silent pod-template churn that rolls the front end on an operator upgrade.
package controller

import (
	"strings"
	"testing"

	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
)

// pinNovaMetadataDeploymentDefaultGolden is the metadata Deployment of the
// shared fixture, carrying every rollout digest the front end reads.
const pinNovaMetadataDeploymentDefaultGolden = `metadata:
  labels:
    app.kubernetes.io/component: metadata
    app.kubernetes.io/instance: nova
    app.kubernetes.io/managed-by: nova-operator
    app.kubernetes.io/name: nova
  name: nova-metadata
  namespace: openstack
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/component: metadata
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
        nova.c5c3.io/metadata-secret-hash: meta5
        nova.c5c3.io/transport-url-hash: bus4
      labels:
        app.kubernetes.io/component: metadata
        app.kubernetes.io/instance: nova
        app.kubernetes.io/managed-by: nova-operator
        app.kubernetes.io/name: nova
    spec:
      containers:
      - command:
        - uwsgi
        - --http
        - :8775
        - --http-keepalive
        - --log-master
        - --log-format
        - '%(method) %(uri) => generated %(rsize) bytes in %(msecs) msecs (%(proto)
          %(status))'
        - --module
        - nova.wsgi.metadata:application
        - --master
        - --lazy-apps
        - --need-app
        - --processes
        - "2"
        - --threads
        - "1"
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
        - name: OS_API_DATABASE__CONNECTION
          valueFrom:
            secretKeyRef:
              key: connection
              name: nova-api-db-connection
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
        - name: OS_NEUTRON__METADATA_PROXY_SHARED_SECRET
          valueFrom:
            secretKeyRef:
              key: shared_secret
              name: nova-metadata-secret
        - name: OS_NOVA_CONFIG_DIR
          value: /etc/nova/nova.conf.d
        - name: OS_NOVA_CONFIG_FILES
          value: /var/lib/openstack/etc/nova/api-paste.ini;nova.conf;metadata.conf
        image: ghcr.io/c5c3/nova:2025.2
        lifecycle:
          preStop:
            exec:
              command:
              - /bin/sh
              - -c
              - sleep 5
        livenessProbe:
          httpGet:
            path: /
            port: 8775
          initialDelaySeconds: 15
          periodSeconds: 20
        name: nova-metadata
        ports:
        - containerPort: 8775
          name: nova-metadata
        readinessProbe:
          failureThreshold: 3
          httpGet:
            path: /
            port: 8775
          initialDelaySeconds: 10
          periodSeconds: 15
          timeoutSeconds: 10
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
        startupProbe:
          failureThreshold: 30
          httpGet:
            path: /
            port: 8775
          periodSeconds: 10
          timeoutSeconds: 8
        volumeMounts:
        - mountPath: /etc/nova/nova.conf.d
          name: config
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
            app.kubernetes.io/component: metadata
            app.kubernetes.io/instance: nova
            app.kubernetes.io/name: nova
        maxSkew: 1
        topologyKey: topology.kubernetes.io/zone
        whenUnsatisfiable: ScheduleAnyway
      - labelSelector:
          matchLabels:
            app.kubernetes.io/component: metadata
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
          - key: metadata.conf
            path: metadata.conf
          - key: nova.conf
            path: nova.conf
          name: nova-config-abc123
        name: config
      - emptyDir: {}
        name: state
      - emptyDir: {}
        name: tmp
status: {}
`

// pinNovaMetadataDeploymentTLSGolden is the same Deployment for a Nova that
// verifies every transport: both schemas' client keypairs and the broker's CA
// bundle.
const pinNovaMetadataDeploymentTLSGolden = `metadata:
  labels:
    app.kubernetes.io/component: metadata
    app.kubernetes.io/instance: nova
    app.kubernetes.io/managed-by: nova-operator
    app.kubernetes.io/name: nova
  name: nova-metadata
  namespace: openstack
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/component: metadata
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
        nova.c5c3.io/metadata-secret-hash: meta5
        nova.c5c3.io/transport-url-hash: bus4
      labels:
        app.kubernetes.io/component: metadata
        app.kubernetes.io/instance: nova
        app.kubernetes.io/managed-by: nova-operator
        app.kubernetes.io/name: nova
    spec:
      containers:
      - command:
        - uwsgi
        - --http
        - :8775
        - --http-keepalive
        - --log-master
        - --log-format
        - '%(method) %(uri) => generated %(rsize) bytes in %(msecs) msecs (%(proto)
          %(status))'
        - --module
        - nova.wsgi.metadata:application
        - --master
        - --lazy-apps
        - --need-app
        - --processes
        - "2"
        - --threads
        - "1"
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
        - name: OS_API_DATABASE__CONNECTION
          valueFrom:
            secretKeyRef:
              key: connection
              name: nova-api-db-connection
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
        - name: OS_NEUTRON__METADATA_PROXY_SHARED_SECRET
          valueFrom:
            secretKeyRef:
              key: shared_secret
              name: nova-metadata-secret
        - name: OS_NOVA_CONFIG_DIR
          value: /etc/nova/nova.conf.d
        - name: OS_NOVA_CONFIG_FILES
          value: /var/lib/openstack/etc/nova/api-paste.ini;nova.conf;metadata.conf
        image: ghcr.io/c5c3/nova:2025.2
        lifecycle:
          preStop:
            exec:
              command:
              - /bin/sh
              - -c
              - sleep 5
        livenessProbe:
          httpGet:
            path: /
            port: 8775
          initialDelaySeconds: 15
          periodSeconds: 20
        name: nova-metadata
        ports:
        - containerPort: 8775
          name: nova-metadata
        readinessProbe:
          failureThreshold: 3
          httpGet:
            path: /
            port: 8775
          initialDelaySeconds: 10
          periodSeconds: 15
          timeoutSeconds: 10
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
        startupProbe:
          failureThreshold: 30
          httpGet:
            path: /
            port: 8775
          periodSeconds: 10
          timeoutSeconds: 8
        volumeMounts:
        - mountPath: /etc/nova/nova.conf.d
          name: config
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
            app.kubernetes.io/component: metadata
            app.kubernetes.io/instance: nova
            app.kubernetes.io/name: nova
        maxSkew: 1
        topologyKey: topology.kubernetes.io/zone
        whenUnsatisfiable: ScheduleAnyway
      - labelSelector:
          matchLabels:
            app.kubernetes.io/component: metadata
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
          - key: metadata.conf
            path: metadata.conf
          - key: nova.conf
            path: nova.conf
          name: nova-config-abc123
        name: config
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

// TestPinNovaMetadataDeployment pins what separates the metadata API from the
// compute API: its own module on its own port, the overlay it trusts a proxied
// instance identity from, and the secret digest only it carries. Both shapes are
// rendered at both supported releases, and the second render must differ from
// the first in the release strings alone, which is what keeps a
// release-conditional field out of the builder.
func TestPinNovaMetadataDeployment(t *testing.T) {
	cases := []struct {
		name   string
		nova   func() *novav1alpha1.Nova
		golden string
	}{
		{name: "default", nova: validNova, golden: pinNovaMetadataDeploymentDefaultGolden},
		{name: "tls", nova: tlsNova, golden: pinNovaMetadataDeploymentTLSGolden},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expectGolden(t, renderYAML(t, buildMetadataDeployment(tc.nova(), workloadArtifacts(),
				workloadTestDigests())), tc.golden)
			expectGolden(t, renderYAML(t, buildMetadataDeployment(atRelease(tc.nova(), "2026.1"), workloadArtifacts(),
				workloadTestDigests())),
				strings.ReplaceAll(tc.golden, "2025.2", "2026.1"))
		})
	}
}
