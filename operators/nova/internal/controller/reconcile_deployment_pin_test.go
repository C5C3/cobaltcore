// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Byte-identity pin for the objects the API workload step renders. The goldens
// below are FULL-OBJECT YAML captured from the builders as they stand today, so
// a changed field order, a dropped default or an extra nil-valued field surfaces
// here as a diff instead of as a silent pod-template churn that rolls every Nova
// API Deployment in the fleet on an operator upgrade.
package controller

import (
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	"sigs.k8s.io/yaml"

	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
)

// pinNovaAPIDeploymentDefaultGolden is the API Deployment of the shared fixture:
// three replicas under uWSGI, json logging, and no optional projection.
const pinNovaAPIDeploymentDefaultGolden = `metadata:
  labels:
    app.kubernetes.io/component: api
    app.kubernetes.io/instance: nova
    app.kubernetes.io/managed-by: nova-operator
    app.kubernetes.io/name: nova
  name: nova
  namespace: openstack
spec:
  replicas: 3
  selector:
    matchLabels:
      app.kubernetes.io/component: api
      app.kubernetes.io/instance: nova
      app.kubernetes.io/name: nova
  strategy:
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
    type: RollingUpdate
  template:
    metadata:
      labels:
        app.kubernetes.io/component: api
        app.kubernetes.io/instance: nova
        app.kubernetes.io/managed-by: nova-operator
        app.kubernetes.io/name: nova
    spec:
      containers:
      - command:
        - uwsgi
        - --http
        - :8774
        - --http-keepalive
        - --log-master
        - --log-format
        - '%(method) %(uri) => generated %(rsize) bytes in %(msecs) msecs (%(proto)
          %(status))'
        - --module
        - nova.wsgi.osapi_compute:application
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
        - name: OS_NOVA_CONFIG_DIR
          value: /etc/nova/nova.conf.d
        - name: OS_NOVA_CONFIG_FILES
          value: /var/lib/openstack/etc/nova/api-paste.ini;nova.conf
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
            port: 8774
          initialDelaySeconds: 15
          periodSeconds: 20
        name: nova-api
        ports:
        - containerPort: 8774
          name: nova-api
        readinessProbe:
          failureThreshold: 3
          httpGet:
            path: /
            port: 8774
          initialDelaySeconds: 10
          periodSeconds: 15
          timeoutSeconds: 10
        resources:
          limits:
            memory: 512Mi
          requests:
            cpu: 100m
            memory: 512Mi
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
            port: 8774
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
            app.kubernetes.io/component: api
            app.kubernetes.io/instance: nova
            app.kubernetes.io/name: nova
        maxSkew: 1
        topologyKey: topology.kubernetes.io/zone
        whenUnsatisfiable: ScheduleAnyway
      - labelSelector:
          matchLabels:
            app.kubernetes.io/component: api
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
      - emptyDir: {}
        name: state
      - emptyDir: {}
        name: tmp
status: {}
`

// pinNovaAPIDeploymentAutoscalingGolden is the same Deployment with an HPA
// owning the replica count and every rollout digest stamped.
const pinNovaAPIDeploymentAutoscalingGolden = `metadata:
  labels:
    app.kubernetes.io/component: api
    app.kubernetes.io/instance: nova
    app.kubernetes.io/managed-by: nova-operator
    app.kubernetes.io/name: nova
  name: nova
  namespace: openstack
spec:
  selector:
    matchLabels:
      app.kubernetes.io/component: api
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
        nova.c5c3.io/transport-url-hash: bus4
      labels:
        app.kubernetes.io/component: api
        app.kubernetes.io/instance: nova
        app.kubernetes.io/managed-by: nova-operator
        app.kubernetes.io/name: nova
    spec:
      containers:
      - command:
        - uwsgi
        - --http
        - :8774
        - --http-keepalive
        - --log-master
        - --log-format
        - '%(method) %(uri) => generated %(rsize) bytes in %(msecs) msecs (%(proto)
          %(status))'
        - --module
        - nova.wsgi.osapi_compute:application
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
        - name: OS_NOVA_CONFIG_DIR
          value: /etc/nova/nova.conf.d
        - name: OS_NOVA_CONFIG_FILES
          value: /var/lib/openstack/etc/nova/api-paste.ini;nova.conf
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
            port: 8774
          initialDelaySeconds: 15
          periodSeconds: 20
        name: nova-api
        ports:
        - containerPort: 8774
          name: nova-api
        readinessProbe:
          failureThreshold: 3
          httpGet:
            path: /
            port: 8774
          initialDelaySeconds: 10
          periodSeconds: 15
          timeoutSeconds: 10
        resources:
          limits:
            memory: 512Mi
          requests:
            cpu: 100m
            memory: 512Mi
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
            port: 8774
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
            app.kubernetes.io/component: api
            app.kubernetes.io/instance: nova
            app.kubernetes.io/name: nova
        maxSkew: 1
        topologyKey: topology.kubernetes.io/zone
        whenUnsatisfiable: ScheduleAnyway
      - labelSelector:
          matchLabels:
            app.kubernetes.io/component: api
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
      - emptyDir: {}
        name: state
      - emptyDir: {}
        name: tmp
status: {}
`

// pinNovaAPIDeploymentTLSGolden is the Deployment of a Nova that verifies every
// transport: both schemas' client keypairs and the broker's CA bundle.
const pinNovaAPIDeploymentTLSGolden = `metadata:
  labels:
    app.kubernetes.io/component: api
    app.kubernetes.io/instance: nova
    app.kubernetes.io/managed-by: nova-operator
    app.kubernetes.io/name: nova
  name: nova
  namespace: openstack
spec:
  replicas: 3
  selector:
    matchLabels:
      app.kubernetes.io/component: api
      app.kubernetes.io/instance: nova
      app.kubernetes.io/name: nova
  strategy:
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
    type: RollingUpdate
  template:
    metadata:
      labels:
        app.kubernetes.io/component: api
        app.kubernetes.io/instance: nova
        app.kubernetes.io/managed-by: nova-operator
        app.kubernetes.io/name: nova
    spec:
      containers:
      - command:
        - uwsgi
        - --http
        - :8774
        - --http-keepalive
        - --log-master
        - --log-format
        - '%(method) %(uri) => generated %(rsize) bytes in %(msecs) msecs (%(proto)
          %(status))'
        - --module
        - nova.wsgi.osapi_compute:application
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
        - name: OS_NOVA_CONFIG_DIR
          value: /etc/nova/nova.conf.d
        - name: OS_NOVA_CONFIG_FILES
          value: /var/lib/openstack/etc/nova/api-paste.ini;nova.conf
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
            port: 8774
          initialDelaySeconds: 15
          periodSeconds: 20
        name: nova-api
        ports:
        - containerPort: 8774
          name: nova-api
        readinessProbe:
          failureThreshold: 3
          httpGet:
            path: /
            port: 8774
          initialDelaySeconds: 10
          periodSeconds: 15
          timeoutSeconds: 10
        resources:
          limits:
            memory: 512Mi
          requests:
            cpu: 100m
            memory: 512Mi
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
            port: 8774
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
            app.kubernetes.io/component: api
            app.kubernetes.io/instance: nova
            app.kubernetes.io/name: nova
        maxSkew: 1
        topologyKey: topology.kubernetes.io/zone
        whenUnsatisfiable: ScheduleAnyway
      - labelSelector:
          matchLabels:
            app.kubernetes.io/component: api
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

// pinNovaAPIServiceGolden is the API Service.
const pinNovaAPIServiceGolden = `metadata:
  labels:
    app.kubernetes.io/instance: nova
    app.kubernetes.io/managed-by: nova-operator
    app.kubernetes.io/name: nova
  name: nova
  namespace: openstack
spec:
  ports:
  - port: 8774
    protocol: TCP
    targetPort: 8774
  selector:
    app.kubernetes.io/component: api
    app.kubernetes.io/instance: nova
    app.kubernetes.io/name: nova
status:
  loadBalancer: {}
`

// pinNovaAPIPDBGolden is the API PodDisruptionBudget.
const pinNovaAPIPDBGolden = `metadata:
  labels:
    app.kubernetes.io/instance: nova
    app.kubernetes.io/managed-by: nova-operator
    app.kubernetes.io/name: nova
  name: nova
  namespace: openstack
spec:
  minAvailable: 1
  selector:
    matchExpressions:
    - key: batch.kubernetes.io/job-name
      operator: DoesNotExist
    matchLabels:
      app.kubernetes.io/component: api
      app.kubernetes.io/instance: nova
      app.kubernetes.io/name: nova
status:
  currentHealthy: 0
  desiredHealthy: 0
  disruptionsAllowed: 0
  expectedPods: 0
`

// TestPinNovaAPIDeployment pins the API pod template in its three shapes: the
// default one, the autoscaled one carrying every rollout digest, and the one
// with all three TLS projections mounted. Each shape is rendered at both
// supported releases: the second render must differ from the first in the
// release strings alone, which is what keeps a release-conditional field out of
// the workload builders.
func TestPinNovaAPIDeployment(t *testing.T) {
	cases := []struct {
		name    string
		nova    func() *novav1alpha1.Nova
		digests workloadDigests
		golden  string
	}{
		{
			name:   "default",
			nova:   validNova,
			golden: pinNovaAPIDeploymentDefaultGolden,
		},
		{
			name: "autoscaling-with-digests",
			nova: func() *novav1alpha1.Nova {
				nova := validNova()
				nova.Spec.Autoscaling = &novav1alpha1.AutoscalingSpec{MaxReplicas: 5}
				return nova
			},
			digests: workloadTestDigests(),
			golden:  pinNovaAPIDeploymentAutoscalingGolden,
		},
		{
			name:   "tls",
			nova:   tlsNova,
			golden: pinNovaAPIDeploymentTLSGolden,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expectGolden(t,
				renderYAML(t, buildAPIDeployment(tc.nova(), workloadArtifacts(), tc.digests)),
				tc.golden)
			expectGolden(t,
				renderYAML(t, buildAPIDeployment(atRelease(tc.nova(), "2026.1"),
					workloadArtifacts(), tc.digests)),
				strings.ReplaceAll(tc.golden, "2025.2", "2026.1"))
		})
	}
}

// TestPinNovaAPIService pins the API Service: the port every client and catalog
// entry assumes, and the selector that keeps the other four workloads out of its
// endpoints.
func TestPinNovaAPIService(t *testing.T) {
	expectGolden(t, renderYAML(t, buildAPIService(validNova())), pinNovaAPIServiceGolden)
}

// TestPinNovaAPIPDB pins the budget: the selector that excludes the Job pods,
// and the minAvailable the shared builder derives from the replica count.
func TestPinNovaAPIPDB(t *testing.T) {
	expectGolden(t, renderYAML(t, buildPodDisruptionBudget(validNova())), pinNovaAPIPDBGolden)
}

// renderYAML marshals a built object the way the pins compare it.
func renderYAML(t *testing.T, obj any) string {
	t.Helper()
	g := NewGomegaWithT(t)

	out, err := yaml.Marshal(obj)
	g.Expect(err).NotTo(HaveOccurred())
	return string(out)
}

// atRelease moves a fixture to another OpenStack release. The image tag and the
// release move together, which is the operator's bump-in-lockstep contract, so
// the rendered workload differs in the image and in the installed-release stamp
// alone.
func atRelease(nova *novav1alpha1.Nova, release string) *novav1alpha1.Nova {
	nova.Spec.OpenStackRelease = release
	nova.Spec.Image.Tag = release
	return nova
}
