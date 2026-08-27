// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Byte-identity pin for the objects buildNeutronDeployment and
// buildNeutronService render. The goldens below are FULL-OBJECT YAML captured
// from the builders as they stand today, so any refactor that moves pod-template
// assembly behind a shared workload builder has to reproduce every rendered
// byte. A changed field order, a dropped default, or an extra nil-valued field
// all surface here as a diff instead of as a silent pod-template churn that
// rolls every Neutron Deployment in the fleet on an operator upgrade.
package controller

import (
	"testing"

	. "github.com/onsi/gomega"
	"sigs.k8s.io/yaml"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
)

// pinNeutronDeploymentDefaultGolden is the API Deployment of the shared
// fixture: uWSGI on the API port, the five environment variables, and the
// three volumes every workload of a TLS-free Neutron carries.
const pinNeutronDeploymentDefaultGolden = `metadata:
  labels:
    app.kubernetes.io/component: api
    app.kubernetes.io/instance: neutron
    app.kubernetes.io/managed-by: neutron-operator
    app.kubernetes.io/name: neutron
  name: neutron
  namespace: openstack
spec:
  replicas: 3
  selector:
    matchLabels:
      app.kubernetes.io/component: api
      app.kubernetes.io/instance: neutron
      app.kubernetes.io/name: neutron
  strategy:
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
    type: RollingUpdate
  template:
    metadata:
      labels:
        app.kubernetes.io/component: api
        app.kubernetes.io/instance: neutron
        app.kubernetes.io/managed-by: neutron-operator
        app.kubernetes.io/name: neutron
    spec:
      containers:
      - command:
        - uwsgi
        - --http
        - :9696
        - --http-keepalive
        - --log-master
        - --log-format
        - '%(method) %(uri) => generated %(rsize) bytes in %(msecs) msecs (%(proto)
          %(status))'
        - --module
        - neutron.wsgi.api
        - --master
        - --lazy-apps
        - --need-app
        - --processes
        - "2"
        - --threads
        - "1"
        - --ini
        - /etc/neutron/uwsgi.ini
        env:
        - name: OS_NEUTRON_CONFIG_DIR
          value: /etc/neutron
        - name: OS_NEUTRON_CONFIG_FILES
          value: neutron.conf;ml2_conf.ini
        - name: OS_DATABASE__CONNECTION
          valueFrom:
            secretKeyRef:
              key: connection
              name: neutron-db-connection
        - name: OS_DEFAULT__TRANSPORT_URL
          valueFrom:
            secretKeyRef:
              key: transport_url
              name: neutron-transport-url
        - name: OS_KEYSTONE_AUTHTOKEN__PASSWORD
          valueFrom:
            secretKeyRef:
              key: password
              name: neutron-service-user
        image: ghcr.io/c5c3/neutron:2026.1
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
            port: 9696
          initialDelaySeconds: 15
          periodSeconds: 20
        name: neutron-api
        ports:
        - containerPort: 9696
          name: neutron-api
        readinessProbe:
          failureThreshold: 3
          httpGet:
            path: /
            port: 9696
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
            path: /
            port: 9696
          periodSeconds: 10
          timeoutSeconds: 8
        volumeMounts:
        - mountPath: /etc/neutron
          name: config
          readOnly: true
        - mountPath: /etc/ovn/tls
          name: ovn-tls
          readOnly: true
        - mountPath: /var/lib/neutron
          name: state
      securityContext:
        fsGroup: 42424
      terminationGracePeriodSeconds: 30
      topologySpreadConstraints:
      - labelSelector:
          matchLabels:
            app.kubernetes.io/component: api
            app.kubernetes.io/instance: neutron
            app.kubernetes.io/name: neutron
        maxSkew: 1
        topologyKey: topology.kubernetes.io/zone
        whenUnsatisfiable: ScheduleAnyway
      - labelSelector:
          matchLabels:
            app.kubernetes.io/component: api
            app.kubernetes.io/instance: neutron
            app.kubernetes.io/name: neutron
        maxSkew: 1
        topologyKey: kubernetes.io/hostname
        whenUnsatisfiable: ScheduleAnyway
      volumes:
      - configMap:
          name: neutron-config-abc
        name: config
      - name: ovn-tls
        secret:
          defaultMode: 256
          secretName: neutron-ovn-client
      - emptyDir: {}
        name: state
status: {}
`

// pinNeutronDeploymentAutoscalingGolden is the same Deployment with an HPA
// owning the replica count. The point of the golden is the absence of
// .spec.replicas: writing it would make the operator and the HPA fight over
// the field on every pass.
const pinNeutronDeploymentAutoscalingGolden = `metadata:
  labels:
    app.kubernetes.io/component: api
    app.kubernetes.io/instance: neutron
    app.kubernetes.io/managed-by: neutron-operator
    app.kubernetes.io/name: neutron
  name: neutron
  namespace: openstack
spec:
  selector:
    matchLabels:
      app.kubernetes.io/component: api
      app.kubernetes.io/instance: neutron
      app.kubernetes.io/name: neutron
  strategy:
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
    type: RollingUpdate
  template:
    metadata:
      labels:
        app.kubernetes.io/component: api
        app.kubernetes.io/instance: neutron
        app.kubernetes.io/managed-by: neutron-operator
        app.kubernetes.io/name: neutron
    spec:
      containers:
      - command:
        - uwsgi
        - --http
        - :9696
        - --http-keepalive
        - --log-master
        - --log-format
        - '%(method) %(uri) => generated %(rsize) bytes in %(msecs) msecs (%(proto)
          %(status))'
        - --module
        - neutron.wsgi.api
        - --master
        - --lazy-apps
        - --need-app
        - --processes
        - "2"
        - --threads
        - "1"
        - --ini
        - /etc/neutron/uwsgi.ini
        env:
        - name: OS_NEUTRON_CONFIG_DIR
          value: /etc/neutron
        - name: OS_NEUTRON_CONFIG_FILES
          value: neutron.conf;ml2_conf.ini
        - name: OS_DATABASE__CONNECTION
          valueFrom:
            secretKeyRef:
              key: connection
              name: neutron-db-connection
        - name: OS_DEFAULT__TRANSPORT_URL
          valueFrom:
            secretKeyRef:
              key: transport_url
              name: neutron-transport-url
        - name: OS_KEYSTONE_AUTHTOKEN__PASSWORD
          valueFrom:
            secretKeyRef:
              key: password
              name: neutron-service-user
        image: ghcr.io/c5c3/neutron:2026.1
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
            port: 9696
          initialDelaySeconds: 15
          periodSeconds: 20
        name: neutron-api
        ports:
        - containerPort: 9696
          name: neutron-api
        readinessProbe:
          failureThreshold: 3
          httpGet:
            path: /
            port: 9696
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
            path: /
            port: 9696
          periodSeconds: 10
          timeoutSeconds: 8
        volumeMounts:
        - mountPath: /etc/neutron
          name: config
          readOnly: true
        - mountPath: /etc/ovn/tls
          name: ovn-tls
          readOnly: true
        - mountPath: /var/lib/neutron
          name: state
      securityContext:
        fsGroup: 42424
      terminationGracePeriodSeconds: 30
      topologySpreadConstraints:
      - labelSelector:
          matchLabels:
            app.kubernetes.io/component: api
            app.kubernetes.io/instance: neutron
            app.kubernetes.io/name: neutron
        maxSkew: 1
        topologyKey: topology.kubernetes.io/zone
        whenUnsatisfiable: ScheduleAnyway
      - labelSelector:
          matchLabels:
            app.kubernetes.io/component: api
            app.kubernetes.io/instance: neutron
            app.kubernetes.io/name: neutron
        maxSkew: 1
        topologyKey: kubernetes.io/hostname
        whenUnsatisfiable: ScheduleAnyway
      volumes:
      - configMap:
          name: neutron-config-abc
        name: config
      - name: ovn-tls
        secret:
          defaultMode: 256
          secretName: neutron-ovn-client
      - emptyDir: {}
        name: state
status: {}
`

// pinNeutronDeploymentDigestsGolden is the Deployment of a pass that resolved
// all four content digests. Each one is what rolls the pods when the value
// behind it rotates, so the annotation block is part of the contract.
const pinNeutronDeploymentDigestsGolden = `metadata:
  labels:
    app.kubernetes.io/component: api
    app.kubernetes.io/instance: neutron
    app.kubernetes.io/managed-by: neutron-operator
    app.kubernetes.io/name: neutron
  name: neutron
  namespace: openstack
spec:
  replicas: 3
  selector:
    matchLabels:
      app.kubernetes.io/component: api
      app.kubernetes.io/instance: neutron
      app.kubernetes.io/name: neutron
  strategy:
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
    type: RollingUpdate
  template:
    metadata:
      annotations:
        neutron.c5c3.io/authtoken-hash: auth456
        neutron.c5c3.io/db-connection-hash: dsn123
        neutron.c5c3.io/ovn-client-hash: ovn012
        neutron.c5c3.io/transport-url-hash: amqp789
      labels:
        app.kubernetes.io/component: api
        app.kubernetes.io/instance: neutron
        app.kubernetes.io/managed-by: neutron-operator
        app.kubernetes.io/name: neutron
    spec:
      containers:
      - command:
        - uwsgi
        - --http
        - :9696
        - --http-keepalive
        - --log-master
        - --log-format
        - '%(method) %(uri) => generated %(rsize) bytes in %(msecs) msecs (%(proto)
          %(status))'
        - --module
        - neutron.wsgi.api
        - --master
        - --lazy-apps
        - --need-app
        - --processes
        - "2"
        - --threads
        - "1"
        - --ini
        - /etc/neutron/uwsgi.ini
        env:
        - name: OS_NEUTRON_CONFIG_DIR
          value: /etc/neutron
        - name: OS_NEUTRON_CONFIG_FILES
          value: neutron.conf;ml2_conf.ini
        - name: OS_DATABASE__CONNECTION
          valueFrom:
            secretKeyRef:
              key: connection
              name: neutron-db-connection
        - name: OS_DEFAULT__TRANSPORT_URL
          valueFrom:
            secretKeyRef:
              key: transport_url
              name: neutron-transport-url
        - name: OS_KEYSTONE_AUTHTOKEN__PASSWORD
          valueFrom:
            secretKeyRef:
              key: password
              name: neutron-service-user
        image: ghcr.io/c5c3/neutron:2026.1
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
            port: 9696
          initialDelaySeconds: 15
          periodSeconds: 20
        name: neutron-api
        ports:
        - containerPort: 9696
          name: neutron-api
        readinessProbe:
          failureThreshold: 3
          httpGet:
            path: /
            port: 9696
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
            path: /
            port: 9696
          periodSeconds: 10
          timeoutSeconds: 8
        volumeMounts:
        - mountPath: /etc/neutron
          name: config
          readOnly: true
        - mountPath: /etc/ovn/tls
          name: ovn-tls
          readOnly: true
        - mountPath: /var/lib/neutron
          name: state
      securityContext:
        fsGroup: 42424
      terminationGracePeriodSeconds: 30
      topologySpreadConstraints:
      - labelSelector:
          matchLabels:
            app.kubernetes.io/component: api
            app.kubernetes.io/instance: neutron
            app.kubernetes.io/name: neutron
        maxSkew: 1
        topologyKey: topology.kubernetes.io/zone
        whenUnsatisfiable: ScheduleAnyway
      - labelSelector:
          matchLabels:
            app.kubernetes.io/component: api
            app.kubernetes.io/instance: neutron
            app.kubernetes.io/name: neutron
        maxSkew: 1
        topologyKey: kubernetes.io/hostname
        whenUnsatisfiable: ScheduleAnyway
      volumes:
      - configMap:
          name: neutron-config-abc
        name: config
      - name: ovn-tls
        secret:
          defaultMode: 256
          secretName: neutron-ovn-client
      - emptyDir: {}
        name: state
status: {}
`

// pinNeutronDeploymentDBTLSGolden is the Deployment of a CR that reaches its
// database over TLS. The projected volume merges the CA bundle and the client
// keypair onto the one mount point the DSN's ssl_ca/ssl_cert/ssl_key paths are
// derived from.
const pinNeutronDeploymentDBTLSGolden = `metadata:
  labels:
    app.kubernetes.io/component: api
    app.kubernetes.io/instance: neutron
    app.kubernetes.io/managed-by: neutron-operator
    app.kubernetes.io/name: neutron
  name: neutron
  namespace: openstack
spec:
  replicas: 3
  selector:
    matchLabels:
      app.kubernetes.io/component: api
      app.kubernetes.io/instance: neutron
      app.kubernetes.io/name: neutron
  strategy:
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
    type: RollingUpdate
  template:
    metadata:
      labels:
        app.kubernetes.io/component: api
        app.kubernetes.io/instance: neutron
        app.kubernetes.io/managed-by: neutron-operator
        app.kubernetes.io/name: neutron
    spec:
      containers:
      - command:
        - uwsgi
        - --http
        - :9696
        - --http-keepalive
        - --log-master
        - --log-format
        - '%(method) %(uri) => generated %(rsize) bytes in %(msecs) msecs (%(proto)
          %(status))'
        - --module
        - neutron.wsgi.api
        - --master
        - --lazy-apps
        - --need-app
        - --processes
        - "2"
        - --threads
        - "1"
        - --ini
        - /etc/neutron/uwsgi.ini
        env:
        - name: OS_NEUTRON_CONFIG_DIR
          value: /etc/neutron
        - name: OS_NEUTRON_CONFIG_FILES
          value: neutron.conf;ml2_conf.ini
        - name: OS_DATABASE__CONNECTION
          valueFrom:
            secretKeyRef:
              key: connection
              name: neutron-db-connection
        - name: OS_DEFAULT__TRANSPORT_URL
          valueFrom:
            secretKeyRef:
              key: transport_url
              name: neutron-transport-url
        - name: OS_KEYSTONE_AUTHTOKEN__PASSWORD
          valueFrom:
            secretKeyRef:
              key: password
              name: neutron-service-user
        image: ghcr.io/c5c3/neutron:2026.1
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
            port: 9696
          initialDelaySeconds: 15
          periodSeconds: 20
        name: neutron-api
        ports:
        - containerPort: 9696
          name: neutron-api
        readinessProbe:
          failureThreshold: 3
          httpGet:
            path: /
            port: 9696
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
            path: /
            port: 9696
          periodSeconds: 10
          timeoutSeconds: 8
        volumeMounts:
        - mountPath: /etc/neutron
          name: config
          readOnly: true
        - mountPath: /etc/ovn/tls
          name: ovn-tls
          readOnly: true
        - mountPath: /var/lib/neutron
          name: state
        - mountPath: /etc/neutron-db-tls/
          name: db-tls
          readOnly: true
      securityContext:
        fsGroup: 42424
      terminationGracePeriodSeconds: 30
      topologySpreadConstraints:
      - labelSelector:
          matchLabels:
            app.kubernetes.io/component: api
            app.kubernetes.io/instance: neutron
            app.kubernetes.io/name: neutron
        maxSkew: 1
        topologyKey: topology.kubernetes.io/zone
        whenUnsatisfiable: ScheduleAnyway
      - labelSelector:
          matchLabels:
            app.kubernetes.io/component: api
            app.kubernetes.io/instance: neutron
            app.kubernetes.io/name: neutron
        maxSkew: 1
        topologyKey: kubernetes.io/hostname
        whenUnsatisfiable: ScheduleAnyway
      volumes:
      - configMap:
          name: neutron-config-abc
        name: config
      - name: ovn-tls
        secret:
          defaultMode: 256
          secretName: neutron-ovn-client
      - emptyDir: {}
        name: state
      - name: db-tls
        projected:
          defaultMode: 256
          sources:
          - secret:
              items:
              - key: ca.crt
                path: ca.crt
              name: neutron-db-ca
          - secret:
              items:
              - key: tls.crt
                path: tls.crt
              - key: tls.key
                path: tls.key
              name: neutron-db-client
status: {}
`

// pinNeutronDeploymentRabbitmqCAGolden is the Deployment of a CR that verifies
// its broker against a private CA. Whatever key the CR names is projected as
// ca.crt, which is the file [oslo_messaging_rabbit] ssl_ca_file points at.
const pinNeutronDeploymentRabbitmqCAGolden = `metadata:
  labels:
    app.kubernetes.io/component: api
    app.kubernetes.io/instance: neutron
    app.kubernetes.io/managed-by: neutron-operator
    app.kubernetes.io/name: neutron
  name: neutron
  namespace: openstack
spec:
  replicas: 3
  selector:
    matchLabels:
      app.kubernetes.io/component: api
      app.kubernetes.io/instance: neutron
      app.kubernetes.io/name: neutron
  strategy:
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
    type: RollingUpdate
  template:
    metadata:
      labels:
        app.kubernetes.io/component: api
        app.kubernetes.io/instance: neutron
        app.kubernetes.io/managed-by: neutron-operator
        app.kubernetes.io/name: neutron
    spec:
      containers:
      - command:
        - uwsgi
        - --http
        - :9696
        - --http-keepalive
        - --log-master
        - --log-format
        - '%(method) %(uri) => generated %(rsize) bytes in %(msecs) msecs (%(proto)
          %(status))'
        - --module
        - neutron.wsgi.api
        - --master
        - --lazy-apps
        - --need-app
        - --processes
        - "2"
        - --threads
        - "1"
        - --ini
        - /etc/neutron/uwsgi.ini
        env:
        - name: OS_NEUTRON_CONFIG_DIR
          value: /etc/neutron
        - name: OS_NEUTRON_CONFIG_FILES
          value: neutron.conf;ml2_conf.ini
        - name: OS_DATABASE__CONNECTION
          valueFrom:
            secretKeyRef:
              key: connection
              name: neutron-db-connection
        - name: OS_DEFAULT__TRANSPORT_URL
          valueFrom:
            secretKeyRef:
              key: transport_url
              name: neutron-transport-url
        - name: OS_KEYSTONE_AUTHTOKEN__PASSWORD
          valueFrom:
            secretKeyRef:
              key: password
              name: neutron-service-user
        image: ghcr.io/c5c3/neutron:2026.1
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
            port: 9696
          initialDelaySeconds: 15
          periodSeconds: 20
        name: neutron-api
        ports:
        - containerPort: 9696
          name: neutron-api
        readinessProbe:
          failureThreshold: 3
          httpGet:
            path: /
            port: 9696
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
            path: /
            port: 9696
          periodSeconds: 10
          timeoutSeconds: 8
        volumeMounts:
        - mountPath: /etc/neutron
          name: config
          readOnly: true
        - mountPath: /etc/ovn/tls
          name: ovn-tls
          readOnly: true
        - mountPath: /var/lib/neutron
          name: state
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
            app.kubernetes.io/instance: neutron
            app.kubernetes.io/name: neutron
        maxSkew: 1
        topologyKey: topology.kubernetes.io/zone
        whenUnsatisfiable: ScheduleAnyway
      - labelSelector:
          matchLabels:
            app.kubernetes.io/component: api
            app.kubernetes.io/instance: neutron
            app.kubernetes.io/name: neutron
        maxSkew: 1
        topologyKey: kubernetes.io/hostname
        whenUnsatisfiable: ScheduleAnyway
      volumes:
      - configMap:
          name: neutron-config-abc
        name: config
      - name: ovn-tls
        secret:
          defaultMode: 256
          secretName: neutron-ovn-client
      - emptyDir: {}
        name: state
      - name: rabbitmq-ca
        secret:
          defaultMode: 292
          items:
          - key: ca.crt
            path: ca.crt
          secretName: rabbitmq-ca
status: {}
`

// pinNeutronServiceGolden is the API Service. Its selector carries the
// component key from the first pass: the two worker Deployments of the same CR
// would otherwise become endpoints of an API nobody can serve from them.
const pinNeutronServiceGolden = `metadata:
  labels:
    app.kubernetes.io/instance: neutron
    app.kubernetes.io/managed-by: neutron-operator
    app.kubernetes.io/name: neutron
  name: neutron
  namespace: openstack
spec:
  ports:
  - port: 9696
    protocol: TCP
    targetPort: 9696
  selector:
    app.kubernetes.io/component: api
    app.kubernetes.io/instance: neutron
    app.kubernetes.io/name: neutron
status:
  loadBalancer: {}
`

// pinDeploymentConfigMapName is the rendered config ConfigMap the pinned
// workloads mount, standing in for what reconcileConfig hands the deployment
// step.
const pinDeploymentConfigMapName = "neutron-config-abc"

// TestPinNeutronDeployment pins the rendered Deployment across the variants that
// change the pod template: the default, the autoscaling case (where
// .spec.replicas must stay absent so the HPA owns it), all four digest
// annotations stamped, and the two TLS projections.
func TestPinNeutronDeployment(t *testing.T) {
	cases := []struct {
		name    string
		neutron func() *neutronv1alpha1.Neutron
		digests [4]string
		golden  string
	}{
		{
			name:    "default",
			neutron: validNeutron,
			golden:  pinNeutronDeploymentDefaultGolden,
		},
		{
			name: "autoscaling",
			neutron: func() *neutronv1alpha1.Neutron {
				n := validNeutron()
				n.Spec.Autoscaling = &neutronv1alpha1.AutoscalingSpec{MaxReplicas: 5}
				return n
			},
			golden: pinNeutronDeploymentAutoscalingGolden,
		},
		{
			name:    "hash-annotations",
			neutron: validNeutron,
			digests: [4]string{"dsn123", "auth456", "amqp789", "ovn012"},
			golden:  pinNeutronDeploymentDigestsGolden,
		},
		{
			name: "db-tls",
			neutron: func() *neutronv1alpha1.Neutron {
				n := validNeutron()
				n.Spec.Database.TLS = &commonv1.DatabaseTLSSpec{
					Mode:                "verify-full",
					CABundleSecretRef:   commonv1.SecretRefSpec{Name: "neutron-db-ca"},
					ClientCertSecretRef: commonv1.SecretRefSpec{Name: "neutron-db-client"},
				}
				return n
			},
			golden: pinNeutronDeploymentDBTLSGolden,
		},
		{
			name: "rabbitmq-ca",
			neutron: func() *neutronv1alpha1.Neutron {
				n := validNeutron()
				n.Spec.Messaging.TLS = &commonv1.MessagingTLSSpec{
					CABundleSecretRef: commonv1.SecretRefSpec{Name: "rabbitmq-ca", Key: "ca.crt"},
				}
				return n
			},
			golden: pinNeutronDeploymentRabbitmqCAGolden,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)

			got, err := yaml.Marshal(buildNeutronDeployment(tc.neutron(), pinDeploymentConfigMapName,
				tc.digests[0], tc.digests[1], tc.digests[2], tc.digests[3]))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(string(got)).To(Equal(tc.golden),
				"the rendered Neutron Deployment must stay byte-identical")
		})
	}
}

// TestPinNeutronService pins the rendered Service, which carries no variants:
// the API port is the same on both ends in every configuration.
func TestPinNeutronService(t *testing.T) {
	g := NewWithT(t)

	got, err := yaml.Marshal(buildNeutronService(validNeutron()))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(string(got)).To(Equal(pinNeutronServiceGolden),
		"the rendered Neutron Service must stay byte-identical")
}
