// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Byte-identity pin for the objects buildKeystoneDeployment and
// buildKeystoneService render. The goldens below are FULL-OBJECT YAML captured
// from the builders as they stand today, so any refactor that moves pod-template
// assembly behind a shared workload builder has to reproduce every rendered byte
// — a changed field order, a dropped default, or an extra nil-valued field all
// surface here as a diff instead of as a silent pod-template churn that rolls
// every Keystone Deployment in the fleet on an operator upgrade.
package controller

import (
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	commonv1 "github.com/c5c3/forge/internal/common/types"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

const pinKeystoneDeploymentDefaultGolden = `metadata:
  labels:
    app.kubernetes.io/instance: test-keystone
    app.kubernetes.io/managed-by: keystone-operator
    app.kubernetes.io/name: keystone
  name: test-keystone
  namespace: default
spec:
  replicas: 3
  selector:
    matchLabels:
      app.kubernetes.io/instance: test-keystone
      app.kubernetes.io/name: keystone
  strategy:
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
    type: RollingUpdate
  template:
    metadata:
      labels:
        app.kubernetes.io/instance: test-keystone
        app.kubernetes.io/managed-by: keystone-operator
        app.kubernetes.io/name: keystone
    spec:
      containers:
      - command:
        - uwsgi
        - --http
        - :5000
        - --http-keepalive
        - --log-master
        - --log-format
        - '%(method) %(uri) => generated %(rsize) bytes in %(msecs) msecs (%(proto)
          %(status))'
        - --wsgi-file
        - /var/lib/openstack/bin/keystone-wsgi-public
        - --master
        - --lazy-apps
        - --need-app
        - --processes
        - "2"
        - --threads
        - "1"
        - --pyargv=--config-dir=/etc/keystone/keystone.conf.d/
        env:
        - name: OS_DATABASE__CONNECTION
          valueFrom:
            secretKeyRef:
              key: connection
              name: test-keystone-db-connection
        image: ghcr.io/c5c3/keystone:2025.2
        lifecycle:
          preStop:
            exec:
              command:
              - /bin/sh
              - -c
              - sleep 5
        livenessProbe:
          initialDelaySeconds: 15
          periodSeconds: 20
          tcpSocket:
            port: 5000
        name: keystone
        ports:
        - containerPort: 5000
          name: keystone
        readinessProbe:
          exec:
            command:
            - /bin/sh
            - -c
            - python3 -c "import os,socket; from urllib.parse import urlparse; raw=os.environ.get('OS_DATABASE__CONNECTION','');
              u=urlparse('//'+raw.split('://',1)[-1]); socket.create_connection((u.hostname,
              u.port or 3306), 20).close()"
          failureThreshold: 3
          initialDelaySeconds: 10
          periodSeconds: 30
          timeoutSeconds: 25
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
        startupProbe:
          failureThreshold: 30
          httpGet:
            path: /v3
            port: 5000
          periodSeconds: 10
        volumeMounts:
        - mountPath: /etc/keystone/keystone.conf.d/
          name: config
          readOnly: true
        - mountPath: /etc/keystone/fernet-keys/
          name: fernet-keys
          readOnly: true
        - mountPath: /etc/keystone/credential-keys/
          name: credential-keys
          readOnly: true
      securityContext:
        fsGroup: 42424
      terminationGracePeriodSeconds: 30
      topologySpreadConstraints:
      - labelSelector:
          matchLabels:
            app.kubernetes.io/instance: test-keystone
            app.kubernetes.io/name: keystone
        maxSkew: 1
        topologyKey: topology.kubernetes.io/zone
        whenUnsatisfiable: ScheduleAnyway
      - labelSelector:
          matchLabels:
            app.kubernetes.io/instance: test-keystone
            app.kubernetes.io/name: keystone
        maxSkew: 1
        topologyKey: kubernetes.io/hostname
        whenUnsatisfiable: ScheduleAnyway
      volumes:
      - configMap:
          name: test-keystone-config
        name: config
      - name: fernet-keys
        secret:
          defaultMode: 256
          secretName: test-keystone-fernet-keys
      - name: credential-keys
        secret:
          defaultMode: 256
          secretName: test-keystone-credential-keys
status: {}
`

const pinKeystoneDeploymentDynamicCredentialsGolden = `metadata:
  labels:
    app.kubernetes.io/instance: test-keystone
    app.kubernetes.io/managed-by: keystone-operator
    app.kubernetes.io/name: keystone
  name: test-keystone
  namespace: default
spec:
  replicas: 3
  selector:
    matchLabels:
      app.kubernetes.io/instance: test-keystone
      app.kubernetes.io/name: keystone
  strategy:
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
    type: RollingUpdate
  template:
    metadata:
      annotations:
        keystone.c5c3.io/db-connection-hash: 0123abc
      labels:
        app.kubernetes.io/instance: test-keystone
        app.kubernetes.io/managed-by: keystone-operator
        app.kubernetes.io/name: keystone
    spec:
      containers:
      - command:
        - uwsgi
        - --http
        - :5000
        - --http-keepalive
        - --log-master
        - --log-format
        - '%(method) %(uri) => generated %(rsize) bytes in %(msecs) msecs (%(proto)
          %(status))'
        - --wsgi-file
        - /var/lib/openstack/bin/keystone-wsgi-public
        - --master
        - --lazy-apps
        - --need-app
        - --processes
        - "2"
        - --threads
        - "1"
        - --pyargv=--config-dir=/etc/keystone/keystone.conf.d/
        env:
        - name: OS_DATABASE__CONNECTION
          valueFrom:
            secretKeyRef:
              key: connection
              name: test-keystone-db-connection
        image: ghcr.io/c5c3/keystone:2025.2
        lifecycle:
          preStop:
            exec:
              command:
              - /bin/sh
              - -c
              - sleep 5
        livenessProbe:
          initialDelaySeconds: 15
          periodSeconds: 20
          tcpSocket:
            port: 5000
        name: keystone
        ports:
        - containerPort: 5000
          name: keystone
        readinessProbe:
          exec:
            command:
            - /bin/sh
            - -c
            - python3 -c "import os,socket; from urllib.parse import urlparse; raw=os.environ.get('OS_DATABASE__CONNECTION','');
              u=urlparse('//'+raw.split('://',1)[-1]); socket.create_connection((u.hostname,
              u.port or 3306), 20).close()"
          failureThreshold: 3
          initialDelaySeconds: 10
          periodSeconds: 30
          timeoutSeconds: 25
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
        startupProbe:
          failureThreshold: 30
          httpGet:
            path: /v3
            port: 5000
          periodSeconds: 10
        volumeMounts:
        - mountPath: /etc/keystone/keystone.conf.d/
          name: config
          readOnly: true
        - mountPath: /etc/keystone/fernet-keys/
          name: fernet-keys
          readOnly: true
        - mountPath: /etc/keystone/credential-keys/
          name: credential-keys
          readOnly: true
      securityContext:
        fsGroup: 42424
      terminationGracePeriodSeconds: 30
      topologySpreadConstraints:
      - labelSelector:
          matchLabels:
            app.kubernetes.io/instance: test-keystone
            app.kubernetes.io/name: keystone
        maxSkew: 1
        topologyKey: topology.kubernetes.io/zone
        whenUnsatisfiable: ScheduleAnyway
      - labelSelector:
          matchLabels:
            app.kubernetes.io/instance: test-keystone
            app.kubernetes.io/name: keystone
        maxSkew: 1
        topologyKey: kubernetes.io/hostname
        whenUnsatisfiable: ScheduleAnyway
      volumes:
      - configMap:
          name: test-keystone-config
        name: config
      - name: fernet-keys
        secret:
          defaultMode: 256
          secretName: test-keystone-fernet-keys
      - name: credential-keys
        secret:
          defaultMode: 256
          secretName: test-keystone-credential-keys
status: {}
`

const pinKeystoneDeploymentDBTLSDomainsGolden = `metadata:
  labels:
    app.kubernetes.io/instance: test-keystone
    app.kubernetes.io/managed-by: keystone-operator
    app.kubernetes.io/name: keystone
  name: test-keystone
  namespace: default
spec:
  replicas: 3
  selector:
    matchLabels:
      app.kubernetes.io/instance: test-keystone
      app.kubernetes.io/name: keystone
  strategy:
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
    type: RollingUpdate
  template:
    metadata:
      labels:
        app.kubernetes.io/instance: test-keystone
        app.kubernetes.io/managed-by: keystone-operator
        app.kubernetes.io/name: keystone
    spec:
      containers:
      - command:
        - uwsgi
        - --http
        - :5000
        - --http-keepalive
        - --log-master
        - --log-format
        - '%(method) %(uri) => generated %(rsize) bytes in %(msecs) msecs (%(proto)
          %(status))'
        - --wsgi-file
        - /var/lib/openstack/bin/keystone-wsgi-public
        - --master
        - --lazy-apps
        - --need-app
        - --processes
        - "2"
        - --threads
        - "1"
        - --pyargv=--config-dir=/etc/keystone/keystone.conf.d/
        env:
        - name: OS_DATABASE__CONNECTION
          valueFrom:
            secretKeyRef:
              key: connection
              name: test-keystone-db-connection
        image: ghcr.io/c5c3/keystone:2025.2
        lifecycle:
          preStop:
            exec:
              command:
              - /bin/sh
              - -c
              - sleep 5
        livenessProbe:
          initialDelaySeconds: 15
          periodSeconds: 20
          tcpSocket:
            port: 5000
        name: keystone
        ports:
        - containerPort: 5000
          name: keystone
        readinessProbe:
          exec:
            command:
            - /bin/sh
            - -c
            - python3 -c "import os,socket; from urllib.parse import urlparse; raw=os.environ.get('OS_DATABASE__CONNECTION','');
              u=urlparse('//'+raw.split('://',1)[-1]); socket.create_connection((u.hostname,
              u.port or 3306), 20).close()"
          failureThreshold: 3
          initialDelaySeconds: 10
          periodSeconds: 30
          timeoutSeconds: 25
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
        startupProbe:
          failureThreshold: 30
          httpGet:
            path: /v3
            port: 5000
          periodSeconds: 10
        volumeMounts:
        - mountPath: /etc/keystone/keystone.conf.d/
          name: config
          readOnly: true
        - mountPath: /etc/keystone/fernet-keys/
          name: fernet-keys
          readOnly: true
        - mountPath: /etc/keystone/credential-keys/
          name: credential-keys
          readOnly: true
        - mountPath: /etc/keystone/db-tls/
          name: db-tls
          readOnly: true
        - mountPath: /etc/keystone/domains
          name: domains
          readOnly: true
      securityContext:
        fsGroup: 42424
      terminationGracePeriodSeconds: 30
      topologySpreadConstraints:
      - labelSelector:
          matchLabels:
            app.kubernetes.io/instance: test-keystone
            app.kubernetes.io/name: keystone
        maxSkew: 1
        topologyKey: topology.kubernetes.io/zone
        whenUnsatisfiable: ScheduleAnyway
      - labelSelector:
          matchLabels:
            app.kubernetes.io/instance: test-keystone
            app.kubernetes.io/name: keystone
        maxSkew: 1
        topologyKey: kubernetes.io/hostname
        whenUnsatisfiable: ScheduleAnyway
      volumes:
      - configMap:
          name: test-keystone-config
        name: config
      - name: fernet-keys
        secret:
          defaultMode: 256
          secretName: test-keystone-fernet-keys
      - name: credential-keys
        secret:
          defaultMode: 256
          secretName: test-keystone-credential-keys
      - name: db-tls
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
      - name: domains
        secret:
          defaultMode: 256
          secretName: test-keystone-domains-abc
status: {}
`

const pinKeystoneDeploymentFederationGolden = `metadata:
  labels:
    app.kubernetes.io/instance: test-keystone
    app.kubernetes.io/managed-by: keystone-operator
    app.kubernetes.io/name: keystone
  name: test-keystone
  namespace: default
spec:
  replicas: 3
  selector:
    matchLabels:
      app.kubernetes.io/instance: test-keystone
      app.kubernetes.io/name: keystone
  strategy:
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
    type: RollingUpdate
  template:
    metadata:
      labels:
        app.kubernetes.io/instance: test-keystone
        app.kubernetes.io/managed-by: keystone-operator
        app.kubernetes.io/name: keystone
    spec:
      containers:
      - command:
        - uwsgi
        - --http
        - 127.0.0.1:5000
        - --buffer-size
        - "65535"
        - --http-keepalive
        - --log-master
        - --log-format
        - '%(method) %(uri) => generated %(rsize) bytes in %(msecs) msecs (%(proto)
          %(status))'
        - --wsgi-file
        - /var/lib/openstack/bin/keystone-wsgi-public
        - --master
        - --lazy-apps
        - --need-app
        - --processes
        - "2"
        - --threads
        - "1"
        - --pyargv=--config-dir=/etc/keystone/keystone.conf.d/
        env:
        - name: OS_DATABASE__CONNECTION
          valueFrom:
            secretKeyRef:
              key: connection
              name: test-keystone-db-connection
        image: ghcr.io/c5c3/keystone:2025.2
        lifecycle:
          preStop:
            exec:
              command:
              - /bin/sh
              - -c
              - sleep 5
        livenessProbe:
          exec:
            command:
            - /bin/sh
            - -c
            - python3 -c "import socket; socket.create_connection(('127.0.0.1', 5000),
              5).close()"
          initialDelaySeconds: 15
          periodSeconds: 20
        name: keystone
        ports:
        - containerPort: 5000
          name: keystone
        readinessProbe:
          exec:
            command:
            - /bin/sh
            - -c
            - python3 -c "import os,socket; from urllib.parse import urlparse; raw=os.environ.get('OS_DATABASE__CONNECTION','');
              u=urlparse('//'+raw.split('://',1)[-1]); socket.create_connection((u.hostname,
              u.port or 3306), 20).close()"
          failureThreshold: 3
          initialDelaySeconds: 10
          periodSeconds: 30
          timeoutSeconds: 25
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
        startupProbe:
          exec:
            command:
            - /bin/sh
            - -c
            - python3 -c "import urllib.request; urllib.request.urlopen('http://127.0.0.1:5000/v3',
              timeout=5)"
          failureThreshold: 30
          periodSeconds: 10
        volumeMounts:
        - mountPath: /etc/keystone/keystone.conf.d/
          name: config
          readOnly: true
        - mountPath: /etc/keystone/fernet-keys/
          name: fernet-keys
          readOnly: true
        - mountPath: /etc/keystone/credential-keys/
          name: credential-keys
          readOnly: true
      - command:
        - apache2
        - -DFOREGROUND
        - -f
        - /etc/keystone-federation-proxy/httpd-base.conf
        image: ghcr.io/c5c3/keystone-federation-proxy:2025.2
        livenessProbe:
          initialDelaySeconds: 15
          periodSeconds: 20
          tcpSocket:
            port: 5050
        name: federation-proxy
        ports:
        - containerPort: 5050
          name: proxy
        readinessProbe:
          httpGet:
            path: /v3
            port: 5050
          initialDelaySeconds: 5
          periodSeconds: 10
        resources:
          limits:
            memory: 256Mi
          requests:
            cpu: 25m
            memory: 64Mi
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
          periodSeconds: 2
          tcpSocket:
            port: 5050
        volumeMounts:
        - mountPath: /etc/keystone-federation-proxy/conf.d
          name: federation-proxy-config
          readOnly: true
        - mountPath: /etc/keystone-federation-proxy/metadata
          name: federation-metadata
          readOnly: true
        - mountPath: /etc/keystone-federation-proxy/mellon
          name: federation-mellon
          readOnly: true
        - mountPath: /tmp
          name: federation-proxy-tmp
      securityContext:
        fsGroup: 42424
      terminationGracePeriodSeconds: 30
      topologySpreadConstraints:
      - labelSelector:
          matchLabels:
            app.kubernetes.io/instance: test-keystone
            app.kubernetes.io/name: keystone
        maxSkew: 1
        topologyKey: topology.kubernetes.io/zone
        whenUnsatisfiable: ScheduleAnyway
      - labelSelector:
          matchLabels:
            app.kubernetes.io/instance: test-keystone
            app.kubernetes.io/name: keystone
        maxSkew: 1
        topologyKey: kubernetes.io/hostname
        whenUnsatisfiable: ScheduleAnyway
      volumes:
      - configMap:
          name: test-keystone-config
        name: config
      - name: fernet-keys
        secret:
          defaultMode: 256
          secretName: test-keystone-fernet-keys
      - name: credential-keys
        secret:
          defaultMode: 256
          secretName: test-keystone-credential-keys
      - name: federation-proxy-config
        secret:
          defaultMode: 256
          items:
          - key: proxy.conf
            path: proxy.conf
          secretName: test-keystone-federation-abc123
      - name: federation-metadata
        secret:
          defaultMode: 256
          items:
          - key: oidc-metadata-idp.example.com.provider
            path: idp.example.com.provider
          - key: oidc-metadata-idp.example.com.client
            path: idp.example.com.client
          secretName: test-keystone-federation-abc123
      - name: federation-mellon
        secret:
          defaultMode: 256
          items:
          - key: mellon-sp-key
            path: sp.key
          - key: mellon-idp-metadata
            path: idp-metadata.xml
          secretName: test-keystone-federation-abc123
      - emptyDir: {}
        name: federation-proxy-tmp
status: {}
`

const pinKeystoneDeploymentAutoscalingGolden = `metadata:
  labels:
    app.kubernetes.io/instance: test-keystone
    app.kubernetes.io/managed-by: keystone-operator
    app.kubernetes.io/name: keystone
  name: test-keystone
  namespace: default
spec:
  selector:
    matchLabels:
      app.kubernetes.io/instance: test-keystone
      app.kubernetes.io/name: keystone
  strategy:
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
    type: RollingUpdate
  template:
    metadata:
      labels:
        app.kubernetes.io/instance: test-keystone
        app.kubernetes.io/managed-by: keystone-operator
        app.kubernetes.io/name: keystone
    spec:
      containers:
      - command:
        - uwsgi
        - --http
        - :5000
        - --http-keepalive
        - --log-master
        - --log-format
        - '%(method) %(uri) => generated %(rsize) bytes in %(msecs) msecs (%(proto)
          %(status))'
        - --wsgi-file
        - /var/lib/openstack/bin/keystone-wsgi-public
        - --master
        - --lazy-apps
        - --need-app
        - --processes
        - "2"
        - --threads
        - "1"
        - --pyargv=--config-dir=/etc/keystone/keystone.conf.d/
        env:
        - name: OS_DATABASE__CONNECTION
          valueFrom:
            secretKeyRef:
              key: connection
              name: test-keystone-db-connection
        image: ghcr.io/c5c3/keystone:2025.2
        lifecycle:
          preStop:
            exec:
              command:
              - /bin/sh
              - -c
              - sleep 5
        livenessProbe:
          initialDelaySeconds: 15
          periodSeconds: 20
          tcpSocket:
            port: 5000
        name: keystone
        ports:
        - containerPort: 5000
          name: keystone
        readinessProbe:
          exec:
            command:
            - /bin/sh
            - -c
            - python3 -c "import os,socket; from urllib.parse import urlparse; raw=os.environ.get('OS_DATABASE__CONNECTION','');
              u=urlparse('//'+raw.split('://',1)[-1]); socket.create_connection((u.hostname,
              u.port or 3306), 20).close()"
          failureThreshold: 3
          initialDelaySeconds: 10
          periodSeconds: 30
          timeoutSeconds: 25
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
        startupProbe:
          failureThreshold: 30
          httpGet:
            path: /v3
            port: 5000
          periodSeconds: 10
        volumeMounts:
        - mountPath: /etc/keystone/keystone.conf.d/
          name: config
          readOnly: true
        - mountPath: /etc/keystone/fernet-keys/
          name: fernet-keys
          readOnly: true
        - mountPath: /etc/keystone/credential-keys/
          name: credential-keys
          readOnly: true
      securityContext:
        fsGroup: 42424
      terminationGracePeriodSeconds: 30
      topologySpreadConstraints:
      - labelSelector:
          matchLabels:
            app.kubernetes.io/instance: test-keystone
            app.kubernetes.io/name: keystone
        maxSkew: 1
        topologyKey: topology.kubernetes.io/zone
        whenUnsatisfiable: ScheduleAnyway
      - labelSelector:
          matchLabels:
            app.kubernetes.io/instance: test-keystone
            app.kubernetes.io/name: keystone
        maxSkew: 1
        topologyKey: kubernetes.io/hostname
        whenUnsatisfiable: ScheduleAnyway
      volumes:
      - configMap:
          name: test-keystone-config
        name: config
      - name: fernet-keys
        secret:
          defaultMode: 256
          secretName: test-keystone-fernet-keys
      - name: credential-keys
        secret:
          defaultMode: 256
          secretName: test-keystone-credential-keys
status: {}
`

const pinKeystoneServiceGolden = `metadata:
  labels:
    app.kubernetes.io/instance: test-keystone
    app.kubernetes.io/managed-by: keystone-operator
    app.kubernetes.io/name: keystone
  name: test-keystone
  namespace: default
spec:
  ports:
  - port: 5000
    protocol: TCP
    targetPort: 5000
  selector:
    app.kubernetes.io/instance: test-keystone
    app.kubernetes.io/name: keystone
status:
  loadBalancer: {}
`

const pinKeystoneServiceFederationGolden = `metadata:
  labels:
    app.kubernetes.io/instance: test-keystone
    app.kubernetes.io/managed-by: keystone-operator
    app.kubernetes.io/name: keystone
  name: test-keystone
  namespace: default
spec:
  ports:
  - port: 5000
    protocol: TCP
    targetPort: 5050
  selector:
    app.kubernetes.io/instance: test-keystone
    app.kubernetes.io/name: keystone
status:
  loadBalancer: {}
`

// pinFederationProjection returns the federation projection the sidecar variant
// renders from: one OIDC metadata pair and one SAML (mellon) pair, so both
// conditional volumes and both conditional sidecar mounts are exercised.
func pinFederationProjection() *federationProjection {
	return &federationProjection{
		SecretName: "test-keystone-federation-abc123",
		MetadataItems: []corev1.KeyToPath{
			{Key: "oidc-metadata-idp.example.com.provider", Path: "idp.example.com.provider"},
			{Key: "oidc-metadata-idp.example.com.client", Path: "idp.example.com.client"},
		},
		RemoteIDAttribute: "HTTP_OIDC_ISS",
		MellonItems: []corev1.KeyToPath{
			{Key: "mellon-sp-key", Path: "sp.key"},
			{Key: "mellon-idp-metadata", Path: "idp-metadata.xml"},
		},
		SAMLProtocolID:        "mapped",
		SAMLRemoteIDAttribute: "MELLON_IDP",
		ProxyImage:            commonv1.ImageSpec{Repository: "ghcr.io/c5c3/keystone-federation-proxy", Tag: "2025.2"},
	}
}

// TestPinKeystoneDeployment pins the rendered Deployment across the variants
// that change the pod template: the plain default, the Dynamic-credentials hash
// annotation, the database-TLS and per-domain config volumes, the federation
// sidecar (which also rebinds uWSGI to 127.0.0.1 and swaps the kubelet probes
// for localhost execs), and the autoscaling case where .spec.replicas must stay
// absent so the HPA owns it.
func TestPinKeystoneDeployment(t *testing.T) {
	cases := []struct {
		name              string
		keystone          func() *keystonev1alpha1.Keystone
		configMapName     string
		dbConnectionHash  string
		domainsSecretName string
		fed               *federationProjection
		golden            string
	}{
		{
			name:          "default",
			keystone:      testKeystone,
			configMapName: "test-keystone-config",
			golden:        pinKeystoneDeploymentDefaultGolden,
		},
		{
			name: "dynamic-credentials",
			keystone: func() *keystonev1alpha1.Keystone {
				ks := testKeystone()
				ks.Spec.Database.CredentialsMode = commonv1.CredentialsModeDynamic
				return ks
			},
			configMapName:    "test-keystone-config",
			dbConnectionHash: "0123abc",
			golden:           pinKeystoneDeploymentDynamicCredentialsGolden,
		},
		{
			name: "db-tls-and-domains",
			keystone: func() *keystonev1alpha1.Keystone {
				ks := testKeystone()
				ks.Spec.Database.TLS = &commonv1.DatabaseTLSSpec{
					Mode:                "verify-ca",
					CABundleSecretRef:   commonv1.SecretRefSpec{Name: "db-ca"},
					ClientCertSecretRef: commonv1.SecretRefSpec{Name: "db-client"},
				}
				return ks
			},
			configMapName:     "test-keystone-config",
			domainsSecretName: "test-keystone-domains-abc",
			golden:            pinKeystoneDeploymentDBTLSDomainsGolden,
		},
		{
			name:          "federation",
			keystone:      testKeystone,
			configMapName: "test-keystone-config",
			fed:           pinFederationProjection(),
			golden:        pinKeystoneDeploymentFederationGolden,
		},
		{
			name: "autoscaling",
			keystone: func() *keystonev1alpha1.Keystone {
				ks := testKeystone()
				ks.Spec.Autoscaling = &keystonev1alpha1.AutoscalingSpec{MaxReplicas: 5}
				return ks
			},
			configMapName: "test-keystone-config",
			golden:        pinKeystoneDeploymentAutoscalingGolden,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)

			got, err := yaml.Marshal(buildKeystoneDeployment(
				tc.keystone(), tc.configMapName, tc.dbConnectionHash, tc.domainsSecretName, tc.fed,
			))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(string(got)).To(Equal(tc.golden),
				"the rendered Keystone Deployment must stay byte-identical")
		})
	}
}

// TestPinKeystoneService pins the rendered Service in both port modes: the
// Service port stays 5000 either way, but with federation active the targetPort
// moves to the header-stripping sidecar.
func TestPinKeystoneService(t *testing.T) {
	cases := []struct {
		name             string
		federationActive bool
		golden           string
	}{
		{name: "default", golden: pinKeystoneServiceGolden},
		{name: "federation", federationActive: true, golden: pinKeystoneServiceFederationGolden},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)

			got, err := yaml.Marshal(buildKeystoneService(testKeystone(), tc.federationActive))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(string(got)).To(Equal(tc.golden),
				"the rendered Keystone Service must stay byte-identical")
		})
	}
}
