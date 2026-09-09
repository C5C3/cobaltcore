// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Byte-identity pin for the objects buildCinderDeployment and buildCinderService
// render. The goldens below are FULL-OBJECT YAML captured from the builders as
// they stand today, so a changed field order, a dropped default or an extra
// nil-valued field surfaces here as a diff instead of as a silent pod-template
// churn that rolls every Cinder API Deployment in the fleet on an operator
// upgrade.
package controller

import (
	"testing"

	. "github.com/onsi/gomega"
	"sigs.k8s.io/yaml"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	cinderv1alpha1 "github.com/c5c3/cobaltcore/operators/cinder/api/v1alpha1"
)

const pinCinderDeploymentDefaultGolden = `metadata:
  labels:
    app.kubernetes.io/component: api
    app.kubernetes.io/instance: cinder
    app.kubernetes.io/managed-by: cinder-operator
    app.kubernetes.io/name: cinder
  name: cinder
  namespace: openstack
spec:
  replicas: 3
  selector:
    matchLabels:
      app.kubernetes.io/component: api
      app.kubernetes.io/instance: cinder
      app.kubernetes.io/name: cinder
  strategy:
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
    type: RollingUpdate
  template:
    metadata:
      labels:
        app.kubernetes.io/component: api
        app.kubernetes.io/instance: cinder
        app.kubernetes.io/managed-by: cinder-operator
        app.kubernetes.io/name: cinder
    spec:
      containers:
      - command:
        - uwsgi
        - --http
        - :8776
        - --http-keepalive
        - --log-master
        - --log-format
        - '%(method) %(uri) => generated %(rsize) bytes in %(msecs) msecs (%(proto)
          %(status))'
        - --module
        - cinder.wsgi.api:application
        - --master
        - --lazy-apps
        - --need-app
        - --processes
        - "2"
        - --threads
        - "1"
        - --pyargv
        - --config-dir /etc/cinder/cinder.conf.d
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
            port: 8776
          initialDelaySeconds: 15
          periodSeconds: 20
        name: cinder-api
        ports:
        - containerPort: 8776
          name: cinder-api
        readinessProbe:
          failureThreshold: 3
          httpGet:
            path: /healthcheck
            port: 8776
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
        volumeMounts:
        - mountPath: /etc/cinder/cinder.conf.d
          name: config
          readOnly: true
        - mountPath: /var/lib/cinder
          name: state
        - mountPath: /tmp
          name: tmp
      securityContext:
        fsGroup: 42424
      terminationGracePeriodSeconds: 30
      topologySpreadConstraints:
      - labelSelector:
          matchLabels:
            app.kubernetes.io/component: api
            app.kubernetes.io/instance: cinder
            app.kubernetes.io/name: cinder
        maxSkew: 1
        topologyKey: topology.kubernetes.io/zone
        whenUnsatisfiable: ScheduleAnyway
      - labelSelector:
          matchLabels:
            app.kubernetes.io/component: api
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
status: {}
`

const pinCinderDeploymentAutoscalingGolden = `metadata:
  labels:
    app.kubernetes.io/component: api
    app.kubernetes.io/instance: cinder
    app.kubernetes.io/managed-by: cinder-operator
    app.kubernetes.io/name: cinder
  name: cinder
  namespace: openstack
spec:
  selector:
    matchLabels:
      app.kubernetes.io/component: api
      app.kubernetes.io/instance: cinder
      app.kubernetes.io/name: cinder
  strategy:
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
    type: RollingUpdate
  template:
    metadata:
      annotations:
        cinder.c5c3.io/authtoken-hash: auth456
        cinder.c5c3.io/db-connection-hash: dsn123
        cinder.c5c3.io/transport-url-hash: bus789
      labels:
        app.kubernetes.io/component: api
        app.kubernetes.io/instance: cinder
        app.kubernetes.io/managed-by: cinder-operator
        app.kubernetes.io/name: cinder
    spec:
      containers:
      - command:
        - uwsgi
        - --http
        - :8776
        - --http-keepalive
        - --log-master
        - --log-format
        - '%(method) %(uri) => generated %(rsize) bytes in %(msecs) msecs (%(proto)
          %(status))'
        - --module
        - cinder.wsgi.api:application
        - --master
        - --lazy-apps
        - --need-app
        - --processes
        - "2"
        - --threads
        - "1"
        - --pyargv
        - --config-dir /etc/cinder/cinder.conf.d
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
            port: 8776
          initialDelaySeconds: 15
          periodSeconds: 20
        name: cinder-api
        ports:
        - containerPort: 8776
          name: cinder-api
        readinessProbe:
          failureThreshold: 3
          httpGet:
            path: /healthcheck
            port: 8776
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
        volumeMounts:
        - mountPath: /etc/cinder/cinder.conf.d
          name: config
          readOnly: true
        - mountPath: /var/lib/cinder
          name: state
        - mountPath: /tmp
          name: tmp
      securityContext:
        fsGroup: 42424
      terminationGracePeriodSeconds: 30
      topologySpreadConstraints:
      - labelSelector:
          matchLabels:
            app.kubernetes.io/component: api
            app.kubernetes.io/instance: cinder
            app.kubernetes.io/name: cinder
        maxSkew: 1
        topologyKey: topology.kubernetes.io/zone
        whenUnsatisfiable: ScheduleAnyway
      - labelSelector:
          matchLabels:
            app.kubernetes.io/component: api
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
status: {}
`

const pinCinderDeploymentTLSGolden = `metadata:
  labels:
    app.kubernetes.io/component: api
    app.kubernetes.io/instance: cinder
    app.kubernetes.io/managed-by: cinder-operator
    app.kubernetes.io/name: cinder
  name: cinder
  namespace: openstack
spec:
  replicas: 3
  selector:
    matchLabels:
      app.kubernetes.io/component: api
      app.kubernetes.io/instance: cinder
      app.kubernetes.io/name: cinder
  strategy:
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
    type: RollingUpdate
  template:
    metadata:
      labels:
        app.kubernetes.io/component: api
        app.kubernetes.io/instance: cinder
        app.kubernetes.io/managed-by: cinder-operator
        app.kubernetes.io/name: cinder
    spec:
      containers:
      - command:
        - uwsgi
        - --http
        - :8776
        - --http-keepalive
        - --log-master
        - --log-format
        - '%(method) %(uri) => generated %(rsize) bytes in %(msecs) msecs (%(proto)
          %(status))'
        - --module
        - cinder.wsgi.api:application
        - --master
        - --lazy-apps
        - --need-app
        - --processes
        - "2"
        - --threads
        - "1"
        - --pyargv
        - --config-dir /etc/cinder/cinder.conf.d
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
            port: 8776
          initialDelaySeconds: 15
          periodSeconds: 20
        name: cinder-api
        ports:
        - containerPort: 8776
          name: cinder-api
        readinessProbe:
          failureThreshold: 3
          httpGet:
            path: /healthcheck
            port: 8776
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
        volumeMounts:
        - mountPath: /etc/cinder/cinder.conf.d
          name: config
          readOnly: true
        - mountPath: /var/lib/cinder
          name: state
        - mountPath: /tmp
          name: tmp
        - mountPath: /etc/cinder-db-tls/
          name: db-tls
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
            app.kubernetes.io/component: api
            app.kubernetes.io/instance: cinder
            app.kubernetes.io/name: cinder
        maxSkew: 1
        topologyKey: topology.kubernetes.io/zone
        whenUnsatisfiable: ScheduleAnyway
      - labelSelector:
          matchLabels:
            app.kubernetes.io/component: api
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
      - name: rabbitmq-ca
        secret:
          defaultMode: 292
          items:
          - key: ca.crt
            path: ca.crt
          secretName: rabbitmq-ca
status: {}
`

const pinCinderServiceGolden = `metadata:
  labels:
    app.kubernetes.io/instance: cinder
    app.kubernetes.io/managed-by: cinder-operator
    app.kubernetes.io/name: cinder
  name: cinder
  namespace: openstack
spec:
  ports:
  - port: 8776
    protocol: TCP
    targetPort: 8776
  selector:
    app.kubernetes.io/component: api
    app.kubernetes.io/instance: cinder
    app.kubernetes.io/name: cinder
status:
  loadBalancer: {}
`

// TestPinCinderDeployment pins the API pod template in its three shapes: the
// default one, the autoscaled one carrying every rollout digest, and the one
// with both TLS projections mounted.
func TestPinCinderDeployment(t *testing.T) {
	cases := []struct {
		name    string
		cinder  func() *cinderv1alpha1.Cinder
		digests workloadDigests
		golden  string
	}{
		{
			name:   "default",
			cinder: workloadCinder,
			golden: pinCinderDeploymentDefaultGolden,
		},
		{
			name: "autoscaling-with-digests",
			cinder: func() *cinderv1alpha1.Cinder {
				cinder := workloadCinder()
				cinder.Spec.Autoscaling = &cinderv1alpha1.AutoscalingSpec{MaxReplicas: 5}
				return cinder
			},
			digests: workloadTestDigests(),
			golden:  pinCinderDeploymentAutoscalingGolden,
		},
		{
			name: "tls",
			cinder: func() *cinderv1alpha1.Cinder {
				cinder := workloadCinder()
				cinder.Spec.Database.TLS = &commonv1.DatabaseTLSSpec{
					Mode:                "verify-full",
					CABundleSecretRef:   commonv1.SecretRefSpec{Name: "db-ca"},
					ClientCertSecretRef: commonv1.SecretRefSpec{Name: "db-client"},
				}
				cinder.Spec.Messaging.TLS = &commonv1.MessagingTLSSpec{
					CABundleSecretRef: commonv1.SecretRefSpec{Name: "rabbitmq-ca"},
				}
				return cinder
			},
			golden: pinCinderDeploymentTLSGolden,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)

			got, err := yaml.Marshal(buildCinderDeployment(tc.cinder(), workloadArtifacts(), tc.digests))

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(string(got)).To(Equal(tc.golden),
				"the rendered Cinder API Deployment must stay byte-identical")
		})
	}
}

// TestPinCinderService pins the API Service: the port every client and catalog
// entry assumes, and the selector that keeps the other three workloads out of
// its endpoints.
func TestPinCinderService(t *testing.T) {
	g := NewWithT(t)

	got, err := yaml.Marshal(buildCinderService(workloadCinder()))

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(string(got)).To(Equal(pinCinderServiceGolden),
		"the rendered Cinder API Service must stay byte-identical")
}
