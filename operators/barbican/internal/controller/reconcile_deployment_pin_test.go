// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Byte-identity pin for the objects buildBarbicanDeployment and
// buildBarbicanService render. The goldens below are FULL-OBJECT YAML captured
// from the builders as they stand today, so any refactor that moves pod-template
// assembly behind a shared workload builder has to reproduce every rendered
// byte. A changed field order, a dropped default, or an extra nil-valued field
// all surface here as a diff instead of as a silent pod-template churn that
// rolls every Barbican Deployment in the fleet on an operator upgrade.
package controller

import (
	"testing"

	. "github.com/onsi/gomega"
	"sigs.k8s.io/yaml"

	commonv1 "github.com/c5c3/forge/internal/common/types"
	barbicanv1alpha1 "github.com/c5c3/forge/operators/barbican/api/v1alpha1"
)

const pinBarbicanDeploymentDefaultGolden = `metadata:
  labels:
    app.kubernetes.io/component: api
    app.kubernetes.io/instance: test-barbican
    app.kubernetes.io/managed-by: barbican-operator
    app.kubernetes.io/name: barbican
  name: test-barbican
  namespace: openstack
spec:
  replicas: 3
  selector:
    matchLabels:
      app.kubernetes.io/instance: test-barbican
      app.kubernetes.io/name: barbican
  strategy:
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
    type: RollingUpdate
  template:
    metadata:
      labels:
        app.kubernetes.io/component: api
        app.kubernetes.io/instance: test-barbican
        app.kubernetes.io/managed-by: barbican-operator
        app.kubernetes.io/name: barbican
    spec:
      containers:
      - command:
        - uwsgi
        - --http
        - :9311
        - --http-keepalive
        - --log-master
        - --log-format
        - '%(method) %(uri) => generated %(rsize) bytes in %(msecs) msecs (%(proto)
          %(status))'
        - --module
        - barbican.wsgi.api:application
        - --master
        - --lazy-apps
        - --need-app
        - --processes
        - "2"
        - --threads
        - "1"
        env:
        - name: OS_DATABASE__CONNECTION
          valueFrom:
            secretKeyRef:
              key: connection
              name: test-barbican-db-connection
        - name: OS_KEYSTONE_AUTHTOKEN__PASSWORD
          valueFrom:
            secretKeyRef:
              key: password
              name: barbican-service-user
        - name: OS_VAULT_PLUGIN__APPROLE_SECRET_ID
          valueFrom:
            secretKeyRef:
              key: secret-id
              name: primary-approle
        image: ghcr.io/c5c3/barbican:2026.1
        lifecycle:
          preStop:
            exec:
              command:
              - /bin/sh
              - -c
              - sleep 5
        livenessProbe:
          httpGet:
            path: /healthcheck
            port: 9311
          initialDelaySeconds: 15
          periodSeconds: 20
        name: barbican-api
        ports:
        - containerPort: 9311
          name: barbican-api
        readinessProbe:
          failureThreshold: 3
          httpGet:
            path: /healthcheck
            port: 9311
          initialDelaySeconds: 10
          periodSeconds: 15
          timeoutSeconds: 10
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
            path: /healthcheck
            port: 9311
          periodSeconds: 10
          timeoutSeconds: 8
        volumeMounts:
        - mountPath: /etc/barbican
          name: config
          readOnly: true
      securityContext:
        fsGroup: 42424
      terminationGracePeriodSeconds: 30
      topologySpreadConstraints:
      - labelSelector:
          matchLabels:
            app.kubernetes.io/instance: test-barbican
            app.kubernetes.io/name: barbican
        maxSkew: 1
        topologyKey: topology.kubernetes.io/zone
        whenUnsatisfiable: ScheduleAnyway
      - labelSelector:
          matchLabels:
            app.kubernetes.io/instance: test-barbican
            app.kubernetes.io/name: barbican
        maxSkew: 1
        topologyKey: kubernetes.io/hostname
        whenUnsatisfiable: ScheduleAnyway
      volumes:
      - name: config
        secret:
          secretName: test-barbican-config-abc
status: {}
`

const pinBarbicanDeploymentSecretStoreCAGolden = `metadata:
  labels:
    app.kubernetes.io/component: api
    app.kubernetes.io/instance: test-barbican
    app.kubernetes.io/managed-by: barbican-operator
    app.kubernetes.io/name: barbican
  name: test-barbican
  namespace: openstack
spec:
  replicas: 3
  selector:
    matchLabels:
      app.kubernetes.io/instance: test-barbican
      app.kubernetes.io/name: barbican
  strategy:
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
    type: RollingUpdate
  template:
    metadata:
      labels:
        app.kubernetes.io/component: api
        app.kubernetes.io/instance: test-barbican
        app.kubernetes.io/managed-by: barbican-operator
        app.kubernetes.io/name: barbican
    spec:
      containers:
      - command:
        - uwsgi
        - --http
        - :9311
        - --http-keepalive
        - --log-master
        - --log-format
        - '%(method) %(uri) => generated %(rsize) bytes in %(msecs) msecs (%(proto)
          %(status))'
        - --module
        - barbican.wsgi.api:application
        - --master
        - --lazy-apps
        - --need-app
        - --processes
        - "2"
        - --threads
        - "1"
        env:
        - name: OS_DATABASE__CONNECTION
          valueFrom:
            secretKeyRef:
              key: connection
              name: test-barbican-db-connection
        - name: OS_KEYSTONE_AUTHTOKEN__PASSWORD
          valueFrom:
            secretKeyRef:
              key: password
              name: barbican-service-user
        - name: OS_VAULT_PLUGIN__APPROLE_SECRET_ID
          valueFrom:
            secretKeyRef:
              key: secret-id
              name: primary-approle
        image: ghcr.io/c5c3/barbican:2026.1
        lifecycle:
          preStop:
            exec:
              command:
              - /bin/sh
              - -c
              - sleep 5
        livenessProbe:
          httpGet:
            path: /healthcheck
            port: 9311
          initialDelaySeconds: 15
          periodSeconds: 20
        name: barbican-api
        ports:
        - containerPort: 9311
          name: barbican-api
        readinessProbe:
          failureThreshold: 3
          httpGet:
            path: /healthcheck
            port: 9311
          initialDelaySeconds: 10
          periodSeconds: 15
          timeoutSeconds: 10
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
            path: /healthcheck
            port: 9311
          periodSeconds: 10
          timeoutSeconds: 8
        volumeMounts:
        - mountPath: /etc/barbican
          name: config
          readOnly: true
        - mountPath: /etc/barbican-secret-store-ca
          name: secret-store-ca
          readOnly: true
      securityContext:
        fsGroup: 42424
      terminationGracePeriodSeconds: 30
      topologySpreadConstraints:
      - labelSelector:
          matchLabels:
            app.kubernetes.io/instance: test-barbican
            app.kubernetes.io/name: barbican
        maxSkew: 1
        topologyKey: topology.kubernetes.io/zone
        whenUnsatisfiable: ScheduleAnyway
      - labelSelector:
          matchLabels:
            app.kubernetes.io/instance: test-barbican
            app.kubernetes.io/name: barbican
        maxSkew: 1
        topologyKey: kubernetes.io/hostname
        whenUnsatisfiable: ScheduleAnyway
      volumes:
      - name: config
        secret:
          secretName: test-barbican-config-abc
      - name: secret-store-ca
        secret:
          defaultMode: 292
          items:
          - key: ca.crt
            path: ca.crt
          secretName: openbao-instance-tls-ca
status: {}
`

const pinBarbicanDeploymentDBTLSGolden = `metadata:
  labels:
    app.kubernetes.io/component: api
    app.kubernetes.io/instance: test-barbican
    app.kubernetes.io/managed-by: barbican-operator
    app.kubernetes.io/name: barbican
  name: test-barbican
  namespace: openstack
spec:
  replicas: 3
  selector:
    matchLabels:
      app.kubernetes.io/instance: test-barbican
      app.kubernetes.io/name: barbican
  strategy:
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
    type: RollingUpdate
  template:
    metadata:
      labels:
        app.kubernetes.io/component: api
        app.kubernetes.io/instance: test-barbican
        app.kubernetes.io/managed-by: barbican-operator
        app.kubernetes.io/name: barbican
    spec:
      containers:
      - command:
        - uwsgi
        - --http
        - :9311
        - --http-keepalive
        - --log-master
        - --log-format
        - '%(method) %(uri) => generated %(rsize) bytes in %(msecs) msecs (%(proto)
          %(status))'
        - --module
        - barbican.wsgi.api:application
        - --master
        - --lazy-apps
        - --need-app
        - --processes
        - "2"
        - --threads
        - "1"
        env:
        - name: OS_DATABASE__CONNECTION
          valueFrom:
            secretKeyRef:
              key: connection
              name: test-barbican-db-connection
        - name: OS_KEYSTONE_AUTHTOKEN__PASSWORD
          valueFrom:
            secretKeyRef:
              key: password
              name: barbican-service-user
        - name: OS_VAULT_PLUGIN__APPROLE_SECRET_ID
          valueFrom:
            secretKeyRef:
              key: secret-id
              name: primary-approle
        image: ghcr.io/c5c3/barbican:2026.1
        lifecycle:
          preStop:
            exec:
              command:
              - /bin/sh
              - -c
              - sleep 5
        livenessProbe:
          httpGet:
            path: /healthcheck
            port: 9311
          initialDelaySeconds: 15
          periodSeconds: 20
        name: barbican-api
        ports:
        - containerPort: 9311
          name: barbican-api
        readinessProbe:
          failureThreshold: 3
          httpGet:
            path: /healthcheck
            port: 9311
          initialDelaySeconds: 10
          periodSeconds: 15
          timeoutSeconds: 10
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
            path: /healthcheck
            port: 9311
          periodSeconds: 10
          timeoutSeconds: 8
        volumeMounts:
        - mountPath: /etc/barbican
          name: config
          readOnly: true
        - mountPath: /etc/barbican-db-tls/
          name: db-tls
          readOnly: true
      securityContext:
        fsGroup: 42424
      terminationGracePeriodSeconds: 30
      topologySpreadConstraints:
      - labelSelector:
          matchLabels:
            app.kubernetes.io/instance: test-barbican
            app.kubernetes.io/name: barbican
        maxSkew: 1
        topologyKey: topology.kubernetes.io/zone
        whenUnsatisfiable: ScheduleAnyway
      - labelSelector:
          matchLabels:
            app.kubernetes.io/instance: test-barbican
            app.kubernetes.io/name: barbican
        maxSkew: 1
        topologyKey: kubernetes.io/hostname
        whenUnsatisfiable: ScheduleAnyway
      volumes:
      - name: config
        secret:
          secretName: test-barbican-config-abc
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
status: {}
`

const pinBarbicanDeploymentAutoscalingGolden = `metadata:
  labels:
    app.kubernetes.io/component: api
    app.kubernetes.io/instance: test-barbican
    app.kubernetes.io/managed-by: barbican-operator
    app.kubernetes.io/name: barbican
  name: test-barbican
  namespace: openstack
spec:
  selector:
    matchLabels:
      app.kubernetes.io/instance: test-barbican
      app.kubernetes.io/name: barbican
  strategy:
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
    type: RollingUpdate
  template:
    metadata:
      labels:
        app.kubernetes.io/component: api
        app.kubernetes.io/instance: test-barbican
        app.kubernetes.io/managed-by: barbican-operator
        app.kubernetes.io/name: barbican
    spec:
      containers:
      - command:
        - uwsgi
        - --http
        - :9311
        - --http-keepalive
        - --log-master
        - --log-format
        - '%(method) %(uri) => generated %(rsize) bytes in %(msecs) msecs (%(proto)
          %(status))'
        - --module
        - barbican.wsgi.api:application
        - --master
        - --lazy-apps
        - --need-app
        - --processes
        - "2"
        - --threads
        - "1"
        env:
        - name: OS_DATABASE__CONNECTION
          valueFrom:
            secretKeyRef:
              key: connection
              name: test-barbican-db-connection
        - name: OS_KEYSTONE_AUTHTOKEN__PASSWORD
          valueFrom:
            secretKeyRef:
              key: password
              name: barbican-service-user
        - name: OS_VAULT_PLUGIN__APPROLE_SECRET_ID
          valueFrom:
            secretKeyRef:
              key: secret-id
              name: primary-approle
        image: ghcr.io/c5c3/barbican:2026.1
        lifecycle:
          preStop:
            exec:
              command:
              - /bin/sh
              - -c
              - sleep 5
        livenessProbe:
          httpGet:
            path: /healthcheck
            port: 9311
          initialDelaySeconds: 15
          periodSeconds: 20
        name: barbican-api
        ports:
        - containerPort: 9311
          name: barbican-api
        readinessProbe:
          failureThreshold: 3
          httpGet:
            path: /healthcheck
            port: 9311
          initialDelaySeconds: 10
          periodSeconds: 15
          timeoutSeconds: 10
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
            path: /healthcheck
            port: 9311
          periodSeconds: 10
          timeoutSeconds: 8
        volumeMounts:
        - mountPath: /etc/barbican
          name: config
          readOnly: true
      securityContext:
        fsGroup: 42424
      terminationGracePeriodSeconds: 30
      topologySpreadConstraints:
      - labelSelector:
          matchLabels:
            app.kubernetes.io/instance: test-barbican
            app.kubernetes.io/name: barbican
        maxSkew: 1
        topologyKey: topology.kubernetes.io/zone
        whenUnsatisfiable: ScheduleAnyway
      - labelSelector:
          matchLabels:
            app.kubernetes.io/instance: test-barbican
            app.kubernetes.io/name: barbican
        maxSkew: 1
        topologyKey: kubernetes.io/hostname
        whenUnsatisfiable: ScheduleAnyway
      volumes:
      - name: config
        secret:
          secretName: test-barbican-config-abc
status: {}
`

const pinBarbicanDeploymentDigestsGolden = `metadata:
  labels:
    app.kubernetes.io/component: api
    app.kubernetes.io/instance: test-barbican
    app.kubernetes.io/managed-by: barbican-operator
    app.kubernetes.io/name: barbican
  name: test-barbican
  namespace: openstack
spec:
  replicas: 3
  selector:
    matchLabels:
      app.kubernetes.io/instance: test-barbican
      app.kubernetes.io/name: barbican
  strategy:
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
    type: RollingUpdate
  template:
    metadata:
      annotations:
        barbican.c5c3.io/authtoken-hash: auth456
        barbican.c5c3.io/db-connection-hash: dsn123
        barbican.c5c3.io/secret-store-credentials-hash: sid789
      labels:
        app.kubernetes.io/component: api
        app.kubernetes.io/instance: test-barbican
        app.kubernetes.io/managed-by: barbican-operator
        app.kubernetes.io/name: barbican
    spec:
      containers:
      - command:
        - uwsgi
        - --http
        - :9311
        - --http-keepalive
        - --log-master
        - --log-format
        - '%(method) %(uri) => generated %(rsize) bytes in %(msecs) msecs (%(proto)
          %(status))'
        - --module
        - barbican.wsgi.api:application
        - --master
        - --lazy-apps
        - --need-app
        - --processes
        - "2"
        - --threads
        - "1"
        env:
        - name: OS_DATABASE__CONNECTION
          valueFrom:
            secretKeyRef:
              key: connection
              name: test-barbican-db-connection
        - name: OS_KEYSTONE_AUTHTOKEN__PASSWORD
          valueFrom:
            secretKeyRef:
              key: password
              name: barbican-service-user
        - name: OS_VAULT_PLUGIN__APPROLE_SECRET_ID
          valueFrom:
            secretKeyRef:
              key: secret-id
              name: primary-approle
        image: ghcr.io/c5c3/barbican:2026.1
        lifecycle:
          preStop:
            exec:
              command:
              - /bin/sh
              - -c
              - sleep 5
        livenessProbe:
          httpGet:
            path: /healthcheck
            port: 9311
          initialDelaySeconds: 15
          periodSeconds: 20
        name: barbican-api
        ports:
        - containerPort: 9311
          name: barbican-api
        readinessProbe:
          failureThreshold: 3
          httpGet:
            path: /healthcheck
            port: 9311
          initialDelaySeconds: 10
          periodSeconds: 15
          timeoutSeconds: 10
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
            path: /healthcheck
            port: 9311
          periodSeconds: 10
          timeoutSeconds: 8
        volumeMounts:
        - mountPath: /etc/barbican
          name: config
          readOnly: true
      securityContext:
        fsGroup: 42424
      terminationGracePeriodSeconds: 30
      topologySpreadConstraints:
      - labelSelector:
          matchLabels:
            app.kubernetes.io/instance: test-barbican
            app.kubernetes.io/name: barbican
        maxSkew: 1
        topologyKey: topology.kubernetes.io/zone
        whenUnsatisfiable: ScheduleAnyway
      - labelSelector:
          matchLabels:
            app.kubernetes.io/instance: test-barbican
            app.kubernetes.io/name: barbican
        maxSkew: 1
        topologyKey: kubernetes.io/hostname
        whenUnsatisfiable: ScheduleAnyway
      volumes:
      - name: config
        secret:
          secretName: test-barbican-config-abc
status: {}
`

const pinBarbicanServiceGolden = `metadata:
  labels:
    app.kubernetes.io/instance: test-barbican
    app.kubernetes.io/managed-by: barbican-operator
    app.kubernetes.io/name: barbican
  name: test-barbican
  namespace: openstack
spec:
  ports:
  - port: 9311
    protocol: TCP
    targetPort: 9311
  selector:
    app.kubernetes.io/component: api
    app.kubernetes.io/instance: test-barbican
    app.kubernetes.io/name: barbican
status:
  loadBalancer: {}
`

// pinConfigSecretName is the rendered config Secret the pinned Deployments
// mount, standing in for what reconcileConfig hands the deployment step.
const pinConfigSecretName = "test-barbican-config-abc"

// TestPinBarbicanDeployment pins the rendered Deployment across the variants
// that change the pod template: the default (the --module launch the image
// forces), the secret-store CA projection, the database-TLS projection, the
// autoscaling case (where .spec.replicas must stay absent so the HPA owns it),
// and all three digest annotations stamped.
func TestPinBarbicanDeployment(t *testing.T) {
	cases := []struct {
		name            string
		barbican        func() *barbicanv1alpha1.Barbican
		projection      func() secretStoreProjection
		dsnDigest       string
		authtokenDigest string
		golden          string
	}{
		{
			name:       "default",
			barbican:   testBarbican,
			projection: validProjection,
			golden:     pinBarbicanDeploymentDefaultGolden,
		},
		{
			name:       "secret-store-ca",
			barbican:   testBarbican,
			projection: projectionWithCA,
			golden:     pinBarbicanDeploymentSecretStoreCAGolden,
		},
		{
			name: "db-tls",
			barbican: func() *barbicanv1alpha1.Barbican {
				b := testBarbican()
				b.Spec.Database.TLS = &commonv1.DatabaseTLSSpec{
					Mode:                "verify-ca",
					CABundleSecretRef:   commonv1.SecretRefSpec{Name: "db-ca"},
					ClientCertSecretRef: commonv1.SecretRefSpec{Name: "db-client"},
				}
				return b
			},
			projection: validProjection,
			golden:     pinBarbicanDeploymentDBTLSGolden,
		},
		{
			name: "autoscaling",
			barbican: func() *barbicanv1alpha1.Barbican {
				b := testBarbican()
				b.Spec.Autoscaling = &barbicanv1alpha1.AutoscalingSpec{MaxReplicas: 5}
				return b
			},
			projection: validProjection,
			golden:     pinBarbicanDeploymentAutoscalingGolden,
		},
		{
			name:     "hash-annotations",
			barbican: testBarbican,
			projection: func() secretStoreProjection {
				projection := validProjection()
				projection.secretIDDigest = "sid789"
				return projection
			},
			dsnDigest:       "dsn123",
			authtokenDigest: "auth456",
			golden:          pinBarbicanDeploymentDigestsGolden,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)

			got, err := yaml.Marshal(buildBarbicanDeployment(
				tc.barbican(), tc.projection(), pinConfigSecretName, tc.dsnDigest, tc.authtokenDigest,
			))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(string(got)).To(Equal(tc.golden),
				"the rendered Barbican Deployment must stay byte-identical")
		})
	}
}

// TestPinBarbicanService pins the rendered Service, which carries no variants
// beyond the selector phase: the API port is the same on both ends in every
// configuration.
func TestPinBarbicanService(t *testing.T) {
	t.Run("narrowed", func(t *testing.T) {
		g := NewWithT(t)

		got, err := yaml.Marshal(buildBarbicanService(testBarbican(), true))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(string(got)).To(Equal(pinBarbicanServiceGolden),
			"the rendered Barbican Service must stay byte-identical")
	})
}
