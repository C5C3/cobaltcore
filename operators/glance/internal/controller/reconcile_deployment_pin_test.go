// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Byte-identity pin for the objects buildGlanceDeployment and
// buildGlanceService render. The goldens below are FULL-OBJECT YAML captured
// from the builders as they stand today, so any refactor that moves pod-template
// assembly behind a shared workload builder has to reproduce every rendered byte
// — a changed field order, a dropped default, or an extra nil-valued field all
// surface here as a diff instead of as a silent pod-template churn that rolls
// every Glance Deployment in the fleet on an operator upgrade.
package controller

import (
	"testing"

	. "github.com/onsi/gomega"
	"sigs.k8s.io/yaml"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	glancev1alpha1 "github.com/c5c3/cobaltcore/operators/glance/api/v1alpha1"
)

const pinGlanceDeploymentDefaultGolden = `metadata:
  labels:
    app.kubernetes.io/component: api
    app.kubernetes.io/instance: test-glance
    app.kubernetes.io/managed-by: glance-operator
    app.kubernetes.io/name: glance
  name: test-glance
  namespace: default
spec:
  replicas: 3
  selector:
    matchLabels:
      app.kubernetes.io/instance: test-glance
      app.kubernetes.io/name: glance
  strategy:
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
    type: RollingUpdate
  template:
    metadata:
      labels:
        app.kubernetes.io/component: api
        app.kubernetes.io/instance: test-glance
        app.kubernetes.io/managed-by: glance-operator
        app.kubernetes.io/name: glance
    spec:
      containers:
      - command:
        - uwsgi
        - --http
        - :9292
        - --http-auto-chunked
        - --http-chunked-input
        - --http-keepalive
        - --log-master
        - --log-format
        - '%(method) %(uri) => generated %(rsize) bytes in %(msecs) msecs (%(proto)
          %(status))'
        - --wsgi-file
        - /var/lib/openstack/bin/glance-wsgi-api
        - --master
        - --lazy-apps
        - --need-app
        - --processes
        - "2"
        - --threads
        - "1"
        - --pyargv
        - --config-dir /etc/glance/glance-api.conf.d/ --config-dir /etc/glance/backends.conf.d/
        env:
        - name: OS_DATABASE__CONNECTION
          valueFrom:
            secretKeyRef:
              key: connection
              name: test-glance-db-connection
        - name: OS_KEYSTONE_AUTHTOKEN__PASSWORD
          valueFrom:
            secretKeyRef:
              key: password
              name: glance-service-user
        image: ghcr.io/c5c3/glance:2026.1
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
            port: 9292
          initialDelaySeconds: 15
          periodSeconds: 20
        name: glance-api
        ports:
        - containerPort: 9292
          name: glance-api
        readinessProbe:
          failureThreshold: 3
          httpGet:
            path: /healthcheck
            port: 9292
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
        - mountPath: /etc/glance/glance-api.conf.d/
          name: config
          readOnly: true
        - mountPath: /etc/glance/backends.conf.d/
          name: backends
          readOnly: true
        - mountPath: /var/lib/glance/staging
          name: staging
        - mountPath: /var/lib/glance/tasks-work
          name: tasks-work
      securityContext:
        fsGroup: 42424
      terminationGracePeriodSeconds: 30
      topologySpreadConstraints:
      - labelSelector:
          matchLabels:
            app.kubernetes.io/instance: test-glance
            app.kubernetes.io/name: glance
        maxSkew: 1
        topologyKey: topology.kubernetes.io/zone
        whenUnsatisfiable: ScheduleAnyway
      - labelSelector:
          matchLabels:
            app.kubernetes.io/instance: test-glance
            app.kubernetes.io/name: glance
        maxSkew: 1
        topologyKey: kubernetes.io/hostname
        whenUnsatisfiable: ScheduleAnyway
      volumes:
      - configMap:
          name: test-glance-config-abc
        name: config
      - name: backends
        secret:
          secretName: test-glance-backends-abc
      - emptyDir:
          sizeLimit: 10Gi
        name: staging
      - emptyDir:
          sizeLimit: 10Gi
        name: tasks-work
status: {}
`

const pinGlanceDeploymentEventletGolden = `metadata:
  labels:
    app.kubernetes.io/component: api
    app.kubernetes.io/instance: test-glance
    app.kubernetes.io/managed-by: glance-operator
    app.kubernetes.io/name: glance
  name: test-glance
  namespace: default
spec:
  replicas: 3
  selector:
    matchLabels:
      app.kubernetes.io/instance: test-glance
      app.kubernetes.io/name: glance
  strategy:
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
    type: RollingUpdate
  template:
    metadata:
      labels:
        app.kubernetes.io/component: api
        app.kubernetes.io/instance: test-glance
        app.kubernetes.io/managed-by: glance-operator
        app.kubernetes.io/name: glance
    spec:
      containers:
      - command:
        - glance-api
        - --config-dir
        - /etc/glance/glance-api.conf.d/
        - --config-dir
        - /etc/glance/backends.conf.d/
        env:
        - name: OS_DATABASE__CONNECTION
          valueFrom:
            secretKeyRef:
              key: connection
              name: test-glance-db-connection
        - name: OS_KEYSTONE_AUTHTOKEN__PASSWORD
          valueFrom:
            secretKeyRef:
              key: password
              name: glance-service-user
        image: ghcr.io/c5c3/glance:2026.1
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
            port: 9292
          initialDelaySeconds: 15
          periodSeconds: 20
        name: glance-api
        ports:
        - containerPort: 9292
          name: glance-api
        readinessProbe:
          failureThreshold: 3
          httpGet:
            path: /healthcheck
            port: 9292
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
        - mountPath: /etc/glance/glance-api.conf.d/
          name: config
          readOnly: true
        - mountPath: /etc/glance/backends.conf.d/
          name: backends
          readOnly: true
        - mountPath: /var/lib/glance/staging
          name: staging
        - mountPath: /var/lib/glance/tasks-work
          name: tasks-work
      securityContext:
        fsGroup: 42424
      terminationGracePeriodSeconds: 30
      topologySpreadConstraints:
      - labelSelector:
          matchLabels:
            app.kubernetes.io/instance: test-glance
            app.kubernetes.io/name: glance
        maxSkew: 1
        topologyKey: topology.kubernetes.io/zone
        whenUnsatisfiable: ScheduleAnyway
      - labelSelector:
          matchLabels:
            app.kubernetes.io/instance: test-glance
            app.kubernetes.io/name: glance
        maxSkew: 1
        topologyKey: kubernetes.io/hostname
        whenUnsatisfiable: ScheduleAnyway
      volumes:
      - configMap:
          name: test-glance-config-abc
        name: config
      - name: backends
        secret:
          secretName: test-glance-backends-abc
      - emptyDir:
          sizeLimit: 10Gi
        name: staging
      - emptyDir:
          sizeLimit: 10Gi
        name: tasks-work
status: {}
`

const pinGlanceDeploymentImageCacheGolden = `metadata:
  labels:
    app.kubernetes.io/component: api
    app.kubernetes.io/instance: test-glance
    app.kubernetes.io/managed-by: glance-operator
    app.kubernetes.io/name: glance
  name: test-glance
  namespace: default
spec:
  replicas: 3
  selector:
    matchLabels:
      app.kubernetes.io/instance: test-glance
      app.kubernetes.io/name: glance
  strategy:
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
    type: RollingUpdate
  template:
    metadata:
      labels:
        app.kubernetes.io/component: api
        app.kubernetes.io/instance: test-glance
        app.kubernetes.io/managed-by: glance-operator
        app.kubernetes.io/name: glance
    spec:
      containers:
      - command:
        - uwsgi
        - --http
        - :9292
        - --http-auto-chunked
        - --http-chunked-input
        - --http-keepalive
        - --log-master
        - --log-format
        - '%(method) %(uri) => generated %(rsize) bytes in %(msecs) msecs (%(proto)
          %(status))'
        - --wsgi-file
        - /var/lib/openstack/bin/glance-wsgi-api
        - --master
        - --lazy-apps
        - --need-app
        - --processes
        - "2"
        - --threads
        - "1"
        - --pyargv
        - --config-dir /etc/glance/glance-api.conf.d/ --config-dir /etc/glance/backends.conf.d/
        env:
        - name: OS_DATABASE__CONNECTION
          valueFrom:
            secretKeyRef:
              key: connection
              name: test-glance-db-connection
        - name: OS_KEYSTONE_AUTHTOKEN__PASSWORD
          valueFrom:
            secretKeyRef:
              key: password
              name: glance-service-user
        image: ghcr.io/c5c3/glance:2026.1
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
            port: 9292
          initialDelaySeconds: 15
          periodSeconds: 20
        name: glance-api
        ports:
        - containerPort: 9292
          name: glance-api
        readinessProbe:
          failureThreshold: 3
          httpGet:
            path: /healthcheck
            port: 9292
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
        - mountPath: /etc/glance/glance-api.conf.d/
          name: config
          readOnly: true
        - mountPath: /etc/glance/backends.conf.d/
          name: backends
          readOnly: true
        - mountPath: /var/lib/glance/staging
          name: staging
        - mountPath: /var/lib/glance/tasks-work
          name: tasks-work
        - mountPath: /var/lib/glance/image-cache
          name: image-cache
      - command:
        - /bin/sh
        - -eu
        - -c
        - |
          interval=300
          poll=30
          high_water_kib=8388608
          dir=/var/lib/glance/image-cache
          conf=/etc/glance/glance-api.conf.d/
          elapsed=0
          failures=0
          while true; do
            sleep "$poll"
            elapsed=$((elapsed + poll))
            used_kib=$(du -sk --exclude=incomplete --exclude=invalid --exclude=queue "$dir" | cut -f1)
            if [ -z "$used_kib" ]; then
              echo "glance-cache-maintenance unmeasured: could not measure $dir; running a pass anyway" >&2
              used_kib=$high_water_kib
            fi
            if [ "$elapsed" -lt "$interval" ] && [ "$used_kib" -lt "$high_water_kib" ]; then
              continue
            fi
            elapsed=0
            rc=0
            glance-cache-pruner --config-dir "$conf" || rc=1
            glance-cache-cleaner --config-dir "$conf" || rc=1
            if [ "$rc" -eq 0 ]; then
              if [ "$failures" -ne 0 ]; then
                echo "glance-cache-maintenance recovered: after $failures consecutive failures" >&2
              fi
              failures=0
              continue
            fi
            failures=$((failures + 1))
            echo "glance-cache-maintenance failed: $failures consecutive, cache at $used_kib KiB of $high_water_kib" >&2
          done
        image: ghcr.io/c5c3/glance:2026.1
        name: cache-maintenance
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
        volumeMounts:
        - mountPath: /etc/glance/glance-api.conf.d/
          name: config
          readOnly: true
        - mountPath: /var/lib/glance/image-cache
          name: image-cache
      securityContext:
        fsGroup: 42424
      terminationGracePeriodSeconds: 30
      topologySpreadConstraints:
      - labelSelector:
          matchLabels:
            app.kubernetes.io/instance: test-glance
            app.kubernetes.io/name: glance
        maxSkew: 1
        topologyKey: topology.kubernetes.io/zone
        whenUnsatisfiable: ScheduleAnyway
      - labelSelector:
          matchLabels:
            app.kubernetes.io/instance: test-glance
            app.kubernetes.io/name: glance
        maxSkew: 1
        topologyKey: kubernetes.io/hostname
        whenUnsatisfiable: ScheduleAnyway
      volumes:
      - configMap:
          name: test-glance-config-abc
        name: config
      - name: backends
        secret:
          secretName: test-glance-backends-abc
      - emptyDir:
          sizeLimit: 10Gi
        name: staging
      - emptyDir:
          sizeLimit: 10Gi
        name: tasks-work
      - emptyDir:
          sizeLimit: 10Gi
        name: image-cache
status: {}
`

const pinGlanceDeploymentDBTLSGolden = `metadata:
  labels:
    app.kubernetes.io/component: api
    app.kubernetes.io/instance: test-glance
    app.kubernetes.io/managed-by: glance-operator
    app.kubernetes.io/name: glance
  name: test-glance
  namespace: default
spec:
  replicas: 3
  selector:
    matchLabels:
      app.kubernetes.io/instance: test-glance
      app.kubernetes.io/name: glance
  strategy:
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
    type: RollingUpdate
  template:
    metadata:
      labels:
        app.kubernetes.io/component: api
        app.kubernetes.io/instance: test-glance
        app.kubernetes.io/managed-by: glance-operator
        app.kubernetes.io/name: glance
    spec:
      containers:
      - command:
        - uwsgi
        - --http
        - :9292
        - --http-auto-chunked
        - --http-chunked-input
        - --http-keepalive
        - --log-master
        - --log-format
        - '%(method) %(uri) => generated %(rsize) bytes in %(msecs) msecs (%(proto)
          %(status))'
        - --wsgi-file
        - /var/lib/openstack/bin/glance-wsgi-api
        - --master
        - --lazy-apps
        - --need-app
        - --processes
        - "2"
        - --threads
        - "1"
        - --pyargv
        - --config-dir /etc/glance/glance-api.conf.d/ --config-dir /etc/glance/backends.conf.d/
        env:
        - name: OS_DATABASE__CONNECTION
          valueFrom:
            secretKeyRef:
              key: connection
              name: test-glance-db-connection
        - name: OS_KEYSTONE_AUTHTOKEN__PASSWORD
          valueFrom:
            secretKeyRef:
              key: password
              name: glance-service-user
        image: ghcr.io/c5c3/glance:2026.1
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
            port: 9292
          initialDelaySeconds: 15
          periodSeconds: 20
        name: glance-api
        ports:
        - containerPort: 9292
          name: glance-api
        readinessProbe:
          failureThreshold: 3
          httpGet:
            path: /healthcheck
            port: 9292
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
        - mountPath: /etc/glance/glance-api.conf.d/
          name: config
          readOnly: true
        - mountPath: /etc/glance/backends.conf.d/
          name: backends
          readOnly: true
        - mountPath: /var/lib/glance/staging
          name: staging
        - mountPath: /var/lib/glance/tasks-work
          name: tasks-work
        - mountPath: /etc/glance/db-tls/
          name: db-tls
          readOnly: true
      securityContext:
        fsGroup: 42424
      terminationGracePeriodSeconds: 30
      topologySpreadConstraints:
      - labelSelector:
          matchLabels:
            app.kubernetes.io/instance: test-glance
            app.kubernetes.io/name: glance
        maxSkew: 1
        topologyKey: topology.kubernetes.io/zone
        whenUnsatisfiable: ScheduleAnyway
      - labelSelector:
          matchLabels:
            app.kubernetes.io/instance: test-glance
            app.kubernetes.io/name: glance
        maxSkew: 1
        topologyKey: kubernetes.io/hostname
        whenUnsatisfiable: ScheduleAnyway
      volumes:
      - configMap:
          name: test-glance-config-abc
        name: config
      - name: backends
        secret:
          secretName: test-glance-backends-abc
      - emptyDir:
          sizeLimit: 10Gi
        name: staging
      - emptyDir:
          sizeLimit: 10Gi
        name: tasks-work
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

const pinGlanceDeploymentAutoscalingGolden = `metadata:
  labels:
    app.kubernetes.io/component: api
    app.kubernetes.io/instance: test-glance
    app.kubernetes.io/managed-by: glance-operator
    app.kubernetes.io/name: glance
  name: test-glance
  namespace: default
spec:
  selector:
    matchLabels:
      app.kubernetes.io/instance: test-glance
      app.kubernetes.io/name: glance
  strategy:
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
    type: RollingUpdate
  template:
    metadata:
      annotations:
        glance.c5c3.io/authtoken-hash: auth456
        glance.c5c3.io/db-connection-hash: dsn123
      labels:
        app.kubernetes.io/component: api
        app.kubernetes.io/instance: test-glance
        app.kubernetes.io/managed-by: glance-operator
        app.kubernetes.io/name: glance
    spec:
      containers:
      - command:
        - uwsgi
        - --http
        - :9292
        - --http-auto-chunked
        - --http-chunked-input
        - --http-keepalive
        - --log-master
        - --log-format
        - '%(method) %(uri) => generated %(rsize) bytes in %(msecs) msecs (%(proto)
          %(status))'
        - --wsgi-file
        - /var/lib/openstack/bin/glance-wsgi-api
        - --master
        - --lazy-apps
        - --need-app
        - --processes
        - "2"
        - --threads
        - "1"
        - --pyargv
        - --config-dir /etc/glance/glance-api.conf.d/ --config-dir /etc/glance/backends.conf.d/
        env:
        - name: OS_DATABASE__CONNECTION
          valueFrom:
            secretKeyRef:
              key: connection
              name: test-glance-db-connection
        - name: OS_KEYSTONE_AUTHTOKEN__PASSWORD
          valueFrom:
            secretKeyRef:
              key: password
              name: glance-service-user
        image: ghcr.io/c5c3/glance:2026.1
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
            port: 9292
          initialDelaySeconds: 15
          periodSeconds: 20
        name: glance-api
        ports:
        - containerPort: 9292
          name: glance-api
        readinessProbe:
          failureThreshold: 3
          httpGet:
            path: /healthcheck
            port: 9292
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
        - mountPath: /etc/glance/glance-api.conf.d/
          name: config
          readOnly: true
        - mountPath: /etc/glance/backends.conf.d/
          name: backends
          readOnly: true
        - mountPath: /var/lib/glance/staging
          name: staging
        - mountPath: /var/lib/glance/tasks-work
          name: tasks-work
      securityContext:
        fsGroup: 42424
      terminationGracePeriodSeconds: 30
      topologySpreadConstraints:
      - labelSelector:
          matchLabels:
            app.kubernetes.io/instance: test-glance
            app.kubernetes.io/name: glance
        maxSkew: 1
        topologyKey: topology.kubernetes.io/zone
        whenUnsatisfiable: ScheduleAnyway
      - labelSelector:
          matchLabels:
            app.kubernetes.io/instance: test-glance
            app.kubernetes.io/name: glance
        maxSkew: 1
        topologyKey: kubernetes.io/hostname
        whenUnsatisfiable: ScheduleAnyway
      volumes:
      - configMap:
          name: test-glance-config-abc
        name: config
      - name: backends
        secret:
          secretName: test-glance-backends-abc
      - emptyDir:
          sizeLimit: 10Gi
        name: staging
      - emptyDir:
          sizeLimit: 10Gi
        name: tasks-work
status: {}
`

const pinGlanceDeploymentUnboundedStagingGolden = `metadata:
  labels:
    app.kubernetes.io/component: api
    app.kubernetes.io/instance: test-glance
    app.kubernetes.io/managed-by: glance-operator
    app.kubernetes.io/name: glance
  name: test-glance
  namespace: default
spec:
  replicas: 3
  selector:
    matchLabels:
      app.kubernetes.io/instance: test-glance
      app.kubernetes.io/name: glance
  strategy:
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
    type: RollingUpdate
  template:
    metadata:
      labels:
        app.kubernetes.io/component: api
        app.kubernetes.io/instance: test-glance
        app.kubernetes.io/managed-by: glance-operator
        app.kubernetes.io/name: glance
    spec:
      containers:
      - command:
        - uwsgi
        - --http
        - :9292
        - --http-auto-chunked
        - --http-chunked-input
        - --http-keepalive
        - --log-master
        - --log-format
        - '%(method) %(uri) => generated %(rsize) bytes in %(msecs) msecs (%(proto)
          %(status))'
        - --wsgi-file
        - /var/lib/openstack/bin/glance-wsgi-api
        - --master
        - --lazy-apps
        - --need-app
        - --processes
        - "2"
        - --threads
        - "1"
        - --pyargv
        - --config-dir /etc/glance/glance-api.conf.d/ --config-dir /etc/glance/backends.conf.d/
        env:
        - name: OS_DATABASE__CONNECTION
          valueFrom:
            secretKeyRef:
              key: connection
              name: test-glance-db-connection
        - name: OS_KEYSTONE_AUTHTOKEN__PASSWORD
          valueFrom:
            secretKeyRef:
              key: password
              name: glance-service-user
        image: ghcr.io/c5c3/glance:2026.1
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
            port: 9292
          initialDelaySeconds: 15
          periodSeconds: 20
        name: glance-api
        ports:
        - containerPort: 9292
          name: glance-api
        readinessProbe:
          failureThreshold: 3
          httpGet:
            path: /healthcheck
            port: 9292
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
        - mountPath: /etc/glance/glance-api.conf.d/
          name: config
          readOnly: true
        - mountPath: /etc/glance/backends.conf.d/
          name: backends
          readOnly: true
        - mountPath: /var/lib/glance/staging
          name: staging
        - mountPath: /var/lib/glance/tasks-work
          name: tasks-work
      securityContext:
        fsGroup: 42424
      terminationGracePeriodSeconds: 30
      topologySpreadConstraints:
      - labelSelector:
          matchLabels:
            app.kubernetes.io/instance: test-glance
            app.kubernetes.io/name: glance
        maxSkew: 1
        topologyKey: topology.kubernetes.io/zone
        whenUnsatisfiable: ScheduleAnyway
      - labelSelector:
          matchLabels:
            app.kubernetes.io/instance: test-glance
            app.kubernetes.io/name: glance
        maxSkew: 1
        topologyKey: kubernetes.io/hostname
        whenUnsatisfiable: ScheduleAnyway
      volumes:
      - configMap:
          name: test-glance-config-abc
        name: config
      - name: backends
        secret:
          secretName: test-glance-backends-abc
      - emptyDir: {}
        name: staging
      - emptyDir: {}
        name: tasks-work
status: {}
`

const pinGlanceServiceGolden = `metadata:
  labels:
    app.kubernetes.io/instance: test-glance
    app.kubernetes.io/managed-by: glance-operator
    app.kubernetes.io/name: glance
  name: test-glance
  namespace: default
spec:
  ports:
  - port: 9292
    protocol: TCP
    targetPort: 9292
  selector:
    app.kubernetes.io/component: api
    app.kubernetes.io/instance: test-glance
    app.kubernetes.io/name: glance
status:
  loadBalancer: {}
`

// pinArtifacts returns the rendered config/backends artefact names the pinned
// Deployments mount, standing in for what reconcileConfig hands the deployment
// step.
func pinArtifacts() configArtifacts {
	return configArtifacts{
		configMapName:      "test-glance-config-abc",
		backendsSecretName: "test-glance-backends-abc",
	}
}

// TestPinGlanceDeployment pins the rendered Deployment across the variants that
// change the pod template: the uWSGI default (2026.1), the eventlet launch mode
// (2025.2), the image cache with its extra volume and maintenance sidecar, the
// database-TLS projection, the autoscaling case with both digest annotations
// (where .spec.replicas must stay absent so the HPA owns it), and the deliberate
// opt-out that leaves the scratch emptyDirs unbounded.
func TestPinGlanceDeployment(t *testing.T) {
	cases := []struct {
		name            string
		glance          func() *glancev1alpha1.Glance
		dsnDigest       string
		authtokenDigest string
		golden          string
	}{
		{
			name:   "default-uwsgi",
			glance: testGlance,
			golden: pinGlanceDeploymentDefaultGolden,
		},
		{
			name: "eventlet",
			glance: func() *glancev1alpha1.Glance {
				gl := testGlance()
				gl.Spec.OpenStackRelease = "2025.2"
				return gl
			},
			golden: pinGlanceDeploymentEventletGolden,
		},
		{
			name: "image-cache",
			glance: func() *glancev1alpha1.Glance {
				gl := testGlance()
				gl.Spec.ImageCache = &glancev1alpha1.ImageCacheSpec{}
				return gl
			},
			golden: pinGlanceDeploymentImageCacheGolden,
		},
		{
			name: "db-tls",
			glance: func() *glancev1alpha1.Glance {
				gl := testGlance()
				gl.Spec.Database.TLS = &commonv1.DatabaseTLSSpec{
					Mode:                "verify-ca",
					CABundleSecretRef:   commonv1.SecretRefSpec{Name: "db-ca"},
					ClientCertSecretRef: commonv1.SecretRefSpec{Name: "db-client"},
				}
				return gl
			},
			golden: pinGlanceDeploymentDBTLSGolden,
		},
		{
			name: "autoscaling-with-digests",
			glance: func() *glancev1alpha1.Glance {
				gl := testGlance()
				gl.Spec.Autoscaling = &glancev1alpha1.AutoscalingSpec{MaxReplicas: 5}
				return gl
			},
			dsnDigest:       "dsn123",
			authtokenDigest: "auth456",
			golden:          pinGlanceDeploymentAutoscalingGolden,
		},
		{
			name: "unbounded-staging",
			glance: func() *glancev1alpha1.Glance {
				gl := testGlance()
				gl.Spec.Staging = &glancev1alpha1.StagingSpec{Unbounded: true}
				return gl
			},
			golden: pinGlanceDeploymentUnboundedStagingGolden,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)

			got, err := yaml.Marshal(buildGlanceDeployment(
				tc.glance(), pinArtifacts(), tc.dsnDigest, tc.authtokenDigest,
			))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(string(got)).To(Equal(tc.golden),
				"the rendered Glance Deployment must stay byte-identical")
		})
	}
}

// TestPinGlanceService pins the rendered Service, which carries no variants: the
// API port is the same on both ends in every configuration.
func TestPinGlanceService(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		g := NewWithT(t)

		got, err := yaml.Marshal(buildGlanceService(testGlance(), true))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(string(got)).To(Equal(pinGlanceServiceGolden),
			"the rendered Glance Service must stay byte-identical")
	})
}
