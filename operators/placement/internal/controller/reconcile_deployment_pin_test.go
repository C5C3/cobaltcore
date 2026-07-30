// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Byte-identity pin for the objects buildPlacementDeployment and
// buildPlacementService render. The goldens below are FULL-OBJECT YAML captured
// from the builders as they stand today, so any refactor that moves pod-template
// assembly behind a shared workload builder has to reproduce every rendered
// byte. A changed field order, a dropped default, or an extra nil-valued field
// all surface here as a diff instead of as a silent pod-template churn that
// rolls every Placement Deployment in the fleet on an operator upgrade.
package controller

import (
	"testing"

	. "github.com/onsi/gomega"
	"sigs.k8s.io/yaml"

	commonv1 "github.com/c5c3/forge/internal/common/types"
	placementv1alpha1 "github.com/c5c3/forge/operators/placement/api/v1alpha1"
)

const pinPlacementDeploymentDefaultGolden = `metadata:
  labels:
    app.kubernetes.io/component: api
    app.kubernetes.io/instance: test-placement
    app.kubernetes.io/managed-by: placement-operator
    app.kubernetes.io/name: placement
  name: test-placement
  namespace: default
spec:
  replicas: 3
  selector:
    matchLabels:
      app.kubernetes.io/instance: test-placement
      app.kubernetes.io/name: placement
  strategy:
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
    type: RollingUpdate
  template:
    metadata:
      labels:
        app.kubernetes.io/component: api
        app.kubernetes.io/instance: test-placement
        app.kubernetes.io/managed-by: placement-operator
        app.kubernetes.io/name: placement
    spec:
      containers:
      - command:
        - uwsgi
        - --http
        - :8778
        - --http-keepalive
        - --log-master
        - --log-format
        - '%(method) %(uri) => generated %(rsize) bytes in %(msecs) msecs (%(proto)
          %(status))'
        - --wsgi-file
        - /var/lib/openstack/bin/placement-api
        - --master
        - --lazy-apps
        - --need-app
        - --processes
        - "2"
        - --threads
        - "1"
        env:
        - name: OS_PLACEMENT_CONFIG_DIR
          value: /etc/placement
        - name: OS_PLACEMENT_DATABASE__CONNECTION
          valueFrom:
            secretKeyRef:
              key: connection
              name: test-placement-db-connection
        - name: OS_KEYSTONE_AUTHTOKEN__PASSWORD
          valueFrom:
            secretKeyRef:
              key: password
              name: placement-service-user
        image: ghcr.io/c5c3/placement:2026.1
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
            port: 8778
          initialDelaySeconds: 15
          periodSeconds: 20
        name: placement-api
        ports:
        - containerPort: 8778
          name: placement-api
        readinessProbe:
          failureThreshold: 3
          httpGet:
            path: /
            port: 8778
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
        volumeMounts:
        - mountPath: /etc/placement
          name: config
          readOnly: true
      securityContext:
        fsGroup: 42424
      terminationGracePeriodSeconds: 30
      topologySpreadConstraints:
      - labelSelector:
          matchLabels:
            app.kubernetes.io/instance: test-placement
            app.kubernetes.io/name: placement
        maxSkew: 1
        topologyKey: topology.kubernetes.io/zone
        whenUnsatisfiable: ScheduleAnyway
      - labelSelector:
          matchLabels:
            app.kubernetes.io/instance: test-placement
            app.kubernetes.io/name: placement
        maxSkew: 1
        topologyKey: kubernetes.io/hostname
        whenUnsatisfiable: ScheduleAnyway
      volumes:
      - configMap:
          name: test-placement-config-abc
        name: config
status: {}
`

const pinPlacementDeploymentAutoscalingGolden = `metadata:
  labels:
    app.kubernetes.io/component: api
    app.kubernetes.io/instance: test-placement
    app.kubernetes.io/managed-by: placement-operator
    app.kubernetes.io/name: placement
  name: test-placement
  namespace: default
spec:
  selector:
    matchLabels:
      app.kubernetes.io/instance: test-placement
      app.kubernetes.io/name: placement
  strategy:
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
    type: RollingUpdate
  template:
    metadata:
      labels:
        app.kubernetes.io/component: api
        app.kubernetes.io/instance: test-placement
        app.kubernetes.io/managed-by: placement-operator
        app.kubernetes.io/name: placement
    spec:
      containers:
      - command:
        - uwsgi
        - --http
        - :8778
        - --http-keepalive
        - --log-master
        - --log-format
        - '%(method) %(uri) => generated %(rsize) bytes in %(msecs) msecs (%(proto)
          %(status))'
        - --wsgi-file
        - /var/lib/openstack/bin/placement-api
        - --master
        - --lazy-apps
        - --need-app
        - --processes
        - "2"
        - --threads
        - "1"
        env:
        - name: OS_PLACEMENT_CONFIG_DIR
          value: /etc/placement
        - name: OS_PLACEMENT_DATABASE__CONNECTION
          valueFrom:
            secretKeyRef:
              key: connection
              name: test-placement-db-connection
        - name: OS_KEYSTONE_AUTHTOKEN__PASSWORD
          valueFrom:
            secretKeyRef:
              key: password
              name: placement-service-user
        image: ghcr.io/c5c3/placement:2026.1
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
            port: 8778
          initialDelaySeconds: 15
          periodSeconds: 20
        name: placement-api
        ports:
        - containerPort: 8778
          name: placement-api
        readinessProbe:
          failureThreshold: 3
          httpGet:
            path: /
            port: 8778
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
        volumeMounts:
        - mountPath: /etc/placement
          name: config
          readOnly: true
      securityContext:
        fsGroup: 42424
      terminationGracePeriodSeconds: 30
      topologySpreadConstraints:
      - labelSelector:
          matchLabels:
            app.kubernetes.io/instance: test-placement
            app.kubernetes.io/name: placement
        maxSkew: 1
        topologyKey: topology.kubernetes.io/zone
        whenUnsatisfiable: ScheduleAnyway
      - labelSelector:
          matchLabels:
            app.kubernetes.io/instance: test-placement
            app.kubernetes.io/name: placement
        maxSkew: 1
        topologyKey: kubernetes.io/hostname
        whenUnsatisfiable: ScheduleAnyway
      volumes:
      - configMap:
          name: test-placement-config-abc
        name: config
status: {}
`

const pinPlacementDeploymentDBTLSGolden = `metadata:
  labels:
    app.kubernetes.io/component: api
    app.kubernetes.io/instance: test-placement
    app.kubernetes.io/managed-by: placement-operator
    app.kubernetes.io/name: placement
  name: test-placement
  namespace: default
spec:
  replicas: 3
  selector:
    matchLabels:
      app.kubernetes.io/instance: test-placement
      app.kubernetes.io/name: placement
  strategy:
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
    type: RollingUpdate
  template:
    metadata:
      labels:
        app.kubernetes.io/component: api
        app.kubernetes.io/instance: test-placement
        app.kubernetes.io/managed-by: placement-operator
        app.kubernetes.io/name: placement
    spec:
      containers:
      - command:
        - uwsgi
        - --http
        - :8778
        - --http-keepalive
        - --log-master
        - --log-format
        - '%(method) %(uri) => generated %(rsize) bytes in %(msecs) msecs (%(proto)
          %(status))'
        - --wsgi-file
        - /var/lib/openstack/bin/placement-api
        - --master
        - --lazy-apps
        - --need-app
        - --processes
        - "2"
        - --threads
        - "1"
        env:
        - name: OS_PLACEMENT_CONFIG_DIR
          value: /etc/placement
        - name: OS_PLACEMENT_DATABASE__CONNECTION
          valueFrom:
            secretKeyRef:
              key: connection
              name: test-placement-db-connection
        - name: OS_KEYSTONE_AUTHTOKEN__PASSWORD
          valueFrom:
            secretKeyRef:
              key: password
              name: placement-service-user
        image: ghcr.io/c5c3/placement:2026.1
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
            port: 8778
          initialDelaySeconds: 15
          periodSeconds: 20
        name: placement-api
        ports:
        - containerPort: 8778
          name: placement-api
        readinessProbe:
          failureThreshold: 3
          httpGet:
            path: /
            port: 8778
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
        volumeMounts:
        - mountPath: /etc/placement
          name: config
          readOnly: true
        - mountPath: /etc/placement-db-tls/
          name: db-tls
          readOnly: true
      securityContext:
        fsGroup: 42424
      terminationGracePeriodSeconds: 30
      topologySpreadConstraints:
      - labelSelector:
          matchLabels:
            app.kubernetes.io/instance: test-placement
            app.kubernetes.io/name: placement
        maxSkew: 1
        topologyKey: topology.kubernetes.io/zone
        whenUnsatisfiable: ScheduleAnyway
      - labelSelector:
          matchLabels:
            app.kubernetes.io/instance: test-placement
            app.kubernetes.io/name: placement
        maxSkew: 1
        topologyKey: kubernetes.io/hostname
        whenUnsatisfiable: ScheduleAnyway
      volumes:
      - configMap:
          name: test-placement-config-abc
        name: config
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

const pinPlacementDeploymentDigestsGolden = `metadata:
  labels:
    app.kubernetes.io/component: api
    app.kubernetes.io/instance: test-placement
    app.kubernetes.io/managed-by: placement-operator
    app.kubernetes.io/name: placement
  name: test-placement
  namespace: default
spec:
  replicas: 3
  selector:
    matchLabels:
      app.kubernetes.io/instance: test-placement
      app.kubernetes.io/name: placement
  strategy:
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
    type: RollingUpdate
  template:
    metadata:
      annotations:
        placement.c5c3.io/authtoken-hash: auth456
        placement.c5c3.io/db-connection-hash: dsn123
      labels:
        app.kubernetes.io/component: api
        app.kubernetes.io/instance: test-placement
        app.kubernetes.io/managed-by: placement-operator
        app.kubernetes.io/name: placement
    spec:
      containers:
      - command:
        - uwsgi
        - --http
        - :8778
        - --http-keepalive
        - --log-master
        - --log-format
        - '%(method) %(uri) => generated %(rsize) bytes in %(msecs) msecs (%(proto)
          %(status))'
        - --wsgi-file
        - /var/lib/openstack/bin/placement-api
        - --master
        - --lazy-apps
        - --need-app
        - --processes
        - "2"
        - --threads
        - "1"
        env:
        - name: OS_PLACEMENT_CONFIG_DIR
          value: /etc/placement
        - name: OS_PLACEMENT_DATABASE__CONNECTION
          valueFrom:
            secretKeyRef:
              key: connection
              name: test-placement-db-connection
        - name: OS_KEYSTONE_AUTHTOKEN__PASSWORD
          valueFrom:
            secretKeyRef:
              key: password
              name: placement-service-user
        image: ghcr.io/c5c3/placement:2026.1
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
            port: 8778
          initialDelaySeconds: 15
          periodSeconds: 20
        name: placement-api
        ports:
        - containerPort: 8778
          name: placement-api
        readinessProbe:
          failureThreshold: 3
          httpGet:
            path: /
            port: 8778
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
        volumeMounts:
        - mountPath: /etc/placement
          name: config
          readOnly: true
      securityContext:
        fsGroup: 42424
      terminationGracePeriodSeconds: 30
      topologySpreadConstraints:
      - labelSelector:
          matchLabels:
            app.kubernetes.io/instance: test-placement
            app.kubernetes.io/name: placement
        maxSkew: 1
        topologyKey: topology.kubernetes.io/zone
        whenUnsatisfiable: ScheduleAnyway
      - labelSelector:
          matchLabels:
            app.kubernetes.io/instance: test-placement
            app.kubernetes.io/name: placement
        maxSkew: 1
        topologyKey: kubernetes.io/hostname
        whenUnsatisfiable: ScheduleAnyway
      volumes:
      - configMap:
          name: test-placement-config-abc
        name: config
status: {}
`

const pinPlacementServiceGolden = `metadata:
  labels:
    app.kubernetes.io/instance: test-placement
    app.kubernetes.io/managed-by: placement-operator
    app.kubernetes.io/name: placement
  name: test-placement
  namespace: default
spec:
  ports:
  - port: 8778
    protocol: TCP
    targetPort: 8778
  selector:
    app.kubernetes.io/component: api
    app.kubernetes.io/instance: test-placement
    app.kubernetes.io/name: placement
status:
  loadBalancer: {}
`

// pinConfigMapName is the rendered config ConfigMap the pinned Deployments
// mount, standing in for what reconcileConfig hands the deployment step.
const pinConfigMapName = "test-placement-config-abc"

// TestPinPlacementDeployment pins the rendered Deployment across the variants
// that change the pod template: the default, the autoscaling case (where
// .spec.replicas must stay absent so the HPA owns it), the database-TLS
// projection, and both digest annotations stamped.
func TestPinPlacementDeployment(t *testing.T) {
	cases := []struct {
		name            string
		placement       func() *placementv1alpha1.Placement
		dsnDigest       string
		authtokenDigest string
		golden          string
	}{
		{
			name:      "default",
			placement: testPlacement,
			golden:    pinPlacementDeploymentDefaultGolden,
		},
		{
			name: "autoscaling",
			placement: func() *placementv1alpha1.Placement {
				p := testPlacement()
				p.Spec.Autoscaling = &placementv1alpha1.AutoscalingSpec{MaxReplicas: 5}
				return p
			},
			golden: pinPlacementDeploymentAutoscalingGolden,
		},
		{
			name: "db-tls",
			placement: func() *placementv1alpha1.Placement {
				p := testPlacement()
				p.Spec.Database.TLS = &commonv1.DatabaseTLSSpec{
					Mode:                "verify-ca",
					CABundleSecretRef:   commonv1.SecretRefSpec{Name: "db-ca"},
					ClientCertSecretRef: commonv1.SecretRefSpec{Name: "db-client"},
				}
				return p
			},
			golden: pinPlacementDeploymentDBTLSGolden,
		},
		{
			name:            "hash-annotations",
			placement:       testPlacement,
			dsnDigest:       "dsn123",
			authtokenDigest: "auth456",
			golden:          pinPlacementDeploymentDigestsGolden,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)

			got, err := yaml.Marshal(buildPlacementDeployment(
				tc.placement(), pinConfigMapName, tc.dsnDigest, tc.authtokenDigest,
			))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(string(got)).To(Equal(tc.golden),
				"the rendered Placement Deployment must stay byte-identical")
		})
	}
}

// TestPinPlacementService pins the rendered Service, which carries no variants
// beyond the selector phase: the API port is the same on both ends in every
// configuration.
func TestPinPlacementService(t *testing.T) {
	t.Run("narrowed", func(t *testing.T) {
		g := NewWithT(t)

		got, err := yaml.Marshal(buildPlacementService(testPlacement(), true))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(string(got)).To(Equal(pinPlacementServiceGolden),
			"the rendered Placement Service must stay byte-identical")
	})
}
