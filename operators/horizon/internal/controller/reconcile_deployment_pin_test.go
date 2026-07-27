// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Byte-identity pin for the objects buildHorizonDeployment and
// buildHorizonService render. The goldens below are FULL-OBJECT YAML captured
// from the builders as they stand today, so any refactor that moves pod-template
// assembly behind a shared workload builder has to reproduce every rendered byte
// — a changed field order, a dropped default, or an extra nil-valued field all
// surface here as a diff instead of as a silent pod-template churn that rolls
// every Horizon Deployment in the fleet on an operator upgrade.
package controller

import (
	"testing"

	. "github.com/onsi/gomega"
	"sigs.k8s.io/yaml"

	horizonv1alpha1 "github.com/c5c3/forge/operators/horizon/api/v1alpha1"
)

const pinHorizonDeploymentDefaultGolden = `metadata:
  labels:
    app.kubernetes.io/instance: test-horizon
    app.kubernetes.io/managed-by: horizon-operator
    app.kubernetes.io/name: horizon
  name: test-horizon
  namespace: default
spec:
  replicas: 3
  selector:
    matchLabels:
      app.kubernetes.io/instance: test-horizon
      app.kubernetes.io/name: horizon
  strategy:
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
    type: RollingUpdate
  template:
    metadata:
      annotations:
        horizon.c5c3.io/secret-key-hash: sk-hash-123
      labels:
        app.kubernetes.io/instance: test-horizon
        app.kubernetes.io/managed-by: horizon-operator
        app.kubernetes.io/name: horizon
    spec:
      containers:
      - command:
        - uwsgi
        - --http
        - :8080
        - --module
        - openstack_dashboard.wsgi
        - --master
        - --die-on-term
        - --need-app
        - --processes
        - "2"
        - --threads
        - "1"
        - --static-map
        - /static=/var/lib/openstack/horizon-static
        - --log-master
        - --log-format
        - '%(method) %(uri) => generated %(rsize) bytes in %(msecs) msecs (%(proto)
          %(status))'
        env:
        - name: HORIZON_SECRET_KEY
          valueFrom:
            secretKeyRef:
              key: secret-key
              name: horizon-secret-key
        image: ghcr.io/c5c3/horizon:2025.2
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
            port: 8080
        name: horizon
        ports:
        - containerPort: 8080
          name: horizon
        readinessProbe:
          failureThreshold: 3
          httpGet:
            httpHeaders:
            - name: Host
              value: localhost
            path: /auth/login/
            port: 8080
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
            httpHeaders:
            - name: Host
              value: localhost
            path: /auth/login/
            port: 8080
          periodSeconds: 10
        volumeMounts:
        - mountPath: /etc/openstack-dashboard/
          name: config
          readOnly: true
      securityContext:
        fsGroup: 42424
      terminationGracePeriodSeconds: 30
      topologySpreadConstraints:
      - labelSelector:
          matchLabels:
            app.kubernetes.io/instance: test-horizon
            app.kubernetes.io/name: horizon
        maxSkew: 1
        topologyKey: topology.kubernetes.io/zone
        whenUnsatisfiable: ScheduleAnyway
      - labelSelector:
          matchLabels:
            app.kubernetes.io/instance: test-horizon
            app.kubernetes.io/name: horizon
        maxSkew: 1
        topologyKey: kubernetes.io/hostname
        whenUnsatisfiable: ScheduleAnyway
      volumes:
      - configMap:
          name: test-horizon-config
        name: config
status: {}
`

const pinHorizonDeploymentAutoscalingGolden = `metadata:
  labels:
    app.kubernetes.io/instance: test-horizon
    app.kubernetes.io/managed-by: horizon-operator
    app.kubernetes.io/name: horizon
  name: test-horizon
  namespace: default
spec:
  selector:
    matchLabels:
      app.kubernetes.io/instance: test-horizon
      app.kubernetes.io/name: horizon
  strategy:
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
    type: RollingUpdate
  template:
    metadata:
      labels:
        app.kubernetes.io/instance: test-horizon
        app.kubernetes.io/managed-by: horizon-operator
        app.kubernetes.io/name: horizon
    spec:
      containers:
      - command:
        - uwsgi
        - --http
        - :8080
        - --module
        - openstack_dashboard.wsgi
        - --master
        - --die-on-term
        - --need-app
        - --processes
        - "2"
        - --threads
        - "1"
        - --static-map
        - /static=/var/lib/openstack/horizon-static
        - --log-master
        - --log-format
        - '%(method) %(uri) => generated %(rsize) bytes in %(msecs) msecs (%(proto)
          %(status))'
        env:
        - name: HORIZON_SECRET_KEY
          valueFrom:
            secretKeyRef:
              key: secret-key
              name: horizon-secret-key
        image: ghcr.io/c5c3/horizon:2025.2
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
            port: 8080
        name: horizon
        ports:
        - containerPort: 8080
          name: horizon
        readinessProbe:
          failureThreshold: 3
          httpGet:
            httpHeaders:
            - name: Host
              value: localhost
            path: /auth/login/
            port: 8080
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
            httpHeaders:
            - name: Host
              value: localhost
            path: /auth/login/
            port: 8080
          periodSeconds: 10
        volumeMounts:
        - mountPath: /etc/openstack-dashboard/
          name: config
          readOnly: true
      securityContext:
        fsGroup: 42424
      terminationGracePeriodSeconds: 30
      topologySpreadConstraints:
      - labelSelector:
          matchLabels:
            app.kubernetes.io/instance: test-horizon
            app.kubernetes.io/name: horizon
        maxSkew: 1
        topologyKey: topology.kubernetes.io/zone
        whenUnsatisfiable: ScheduleAnyway
      - labelSelector:
          matchLabels:
            app.kubernetes.io/instance: test-horizon
            app.kubernetes.io/name: horizon
        maxSkew: 1
        topologyKey: kubernetes.io/hostname
        whenUnsatisfiable: ScheduleAnyway
      volumes:
      - configMap:
          name: test-horizon-config
        name: config
status: {}
`

const pinHorizonServiceGolden = `metadata:
  labels:
    app.kubernetes.io/instance: test-horizon
    app.kubernetes.io/managed-by: horizon-operator
    app.kubernetes.io/name: horizon
  name: test-horizon
  namespace: default
spec:
  ports:
  - port: 8080
    protocol: TCP
    targetPort: 8080
  selector:
    app.kubernetes.io/instance: test-horizon
    app.kubernetes.io/name: horizon
status:
  loadBalancer: {}
`

// TestPinHorizonDeployment pins the rendered Deployment in both variants that
// change the pod template: the default with the SECRET_KEY hash annotation, and
// the autoscaling case where the annotation is absent and .spec.replicas must
// stay absent too so the HPA owns it.
func TestPinHorizonDeployment(t *testing.T) {
	cases := []struct {
		name          string
		horizon       func() *horizonv1alpha1.Horizon
		secretKeyHash string
		golden        string
	}{
		{
			name:          "default",
			horizon:       testHorizon,
			secretKeyHash: "sk-hash-123",
			golden:        pinHorizonDeploymentDefaultGolden,
		},
		{
			name: "autoscaling",
			horizon: func() *horizonv1alpha1.Horizon {
				hz := testHorizon()
				hz.Spec.Autoscaling = &horizonv1alpha1.AutoscalingSpec{MaxReplicas: 4}
				return hz
			},
			golden: pinHorizonDeploymentAutoscalingGolden,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)

			got, err := yaml.Marshal(buildHorizonDeployment(
				tc.horizon(), "test-horizon-config", tc.secretKeyHash,
			))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(string(got)).To(Equal(tc.golden),
				"the rendered Horizon Deployment must stay byte-identical")
		})
	}
}

// TestPinHorizonService pins the rendered Service, which carries no variants:
// the dashboard port is the same on both ends in every configuration.
func TestPinHorizonService(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		g := NewWithT(t)

		got, err := yaml.Marshal(buildHorizonService(testHorizon()))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(string(got)).To(Equal(pinHorizonServiceGolden),
			"the rendered Horizon Service must stay byte-identical")
	})
}
