// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Byte-identity pin for the objects the database step renders. The goldens
// below are FULL-OBJECT YAML captured from the builders as they stand today, so
// any refactor of the Raft projection has to reproduce every rendered byte. A
// changed field order, a dropped default, or a shifted node port all surface
// here as a diff instead of as a silent pod-template churn that restarts every
// Raft member in the fleet on an operator upgrade.
//
// The scripts are pinned with the ConfigMap that carries them: their content is
// what places a member in its Raft cluster and what opens the client port, and
// an edit to either is a change to the database's own bootstrap.
package controller

import (
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// pinCustomOVNCentral is the fixture behind the "custom" goldens: every knob of
// the Northbound block moved off its default at once, so a builder that reads
// the wrong field cannot hide behind a value that happens to match the default.
func pinCustomOVNCentral() *ovnv1alpha1.OVNCentral {
	cr := testOVNCentral()
	cr.Spec.Image = &commonv1.ImageSpec{
		Repository: "registry.example.com/ovn",
		Digest:     "sha256:1111111111111111111111111111111111111111111111111111111111111111",
	}
	cr.Spec.Northbound = ovnv1alpha1.OVNDatabaseSpec{
		Replicas:            1,
		ExternallyReachable: true,
		NodePortBase:        ptr.To(int32(31000)),
		ElectionTimerMs:     5000,
		InactivityProbeMs:   30000,
		Storage: ovnv1alpha1.OVNStorageSpec{
			Size:             "10Gi",
			StorageClassName: ptr.To("fast"),
		},
		Resources: &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("1Gi"),
			},
		},
	}
	return cr
}

const pinNorthboundHeadlessServiceGolden = `metadata:
  labels:
    app.kubernetes.io/component: nb
    app.kubernetes.io/instance: ovn
    app.kubernetes.io/managed-by: ovncentral-operator
    app.kubernetes.io/name: ovncentral
  name: ovn-nb
  namespace: openstack
spec:
  clusterIP: None
  ports:
  - name: client
    port: 6641
    targetPort: 6641
  - name: raft
    port: 6643
    targetPort: 6643
  publishNotReadyAddresses: true
  selector:
    app.kubernetes.io/component: nb
    app.kubernetes.io/instance: ovn
    app.kubernetes.io/name: ovncentral
status:
  loadBalancer: {}
`

const pinSouthboundHeadlessServiceGolden = `metadata:
  labels:
    app.kubernetes.io/component: sb
    app.kubernetes.io/instance: ovn
    app.kubernetes.io/managed-by: ovncentral-operator
    app.kubernetes.io/name: ovncentral
  name: ovn-sb
  namespace: openstack
spec:
  clusterIP: None
  ports:
  - name: client
    port: 6642
    targetPort: 6642
  - name: raft
    port: 6644
    targetPort: 6644
  publishNotReadyAddresses: true
  selector:
    app.kubernetes.io/component: sb
    app.kubernetes.io/instance: ovn
    app.kubernetes.io/name: ovncentral
status:
  loadBalancer: {}
`

const pinNorthboundMemberServiceGolden = `metadata:
  labels:
    app.kubernetes.io/component: nb
    app.kubernetes.io/instance: ovn
    app.kubernetes.io/managed-by: ovncentral-operator
    app.kubernetes.io/name: ovncentral
  name: ovn-nb-1
  namespace: openstack
spec:
  ports:
  - name: client
    port: 6641
    targetPort: 6641
  selector:
    statefulset.kubernetes.io/pod-name: ovn-nb-1
  type: ClusterIP
status:
  loadBalancer: {}
`

const pinSouthboundMemberServiceGolden = `metadata:
  labels:
    app.kubernetes.io/component: sb
    app.kubernetes.io/instance: ovn
    app.kubernetes.io/managed-by: ovncentral-operator
    app.kubernetes.io/name: ovncentral
  name: ovn-sb-1
  namespace: openstack
spec:
  ports:
  - name: client
    port: 6642
    targetPort: 6642
  selector:
    statefulset.kubernetes.io/pod-name: ovn-sb-1
  type: ClusterIP
status:
  loadBalancer: {}
`

const pinCustomMemberServiceGolden = `metadata:
  labels:
    app.kubernetes.io/component: nb
    app.kubernetes.io/instance: ovn
    app.kubernetes.io/managed-by: ovncentral-operator
    app.kubernetes.io/name: ovncentral
  name: ovn-nb-0
  namespace: openstack
spec:
  ports:
  - name: client
    nodePort: 31000
    port: 6641
    targetPort: 6641
  selector:
    statefulset.kubernetes.io/pod-name: ovn-nb-0
  type: NodePort
status:
  loadBalancer: {}
`

const pinNorthboundStatefulSetGolden = `metadata:
  labels:
    app.kubernetes.io/component: nb
    app.kubernetes.io/instance: ovn
    app.kubernetes.io/managed-by: ovncentral-operator
    app.kubernetes.io/name: ovncentral
  name: ovn-nb
  namespace: openstack
spec:
  persistentVolumeClaimRetentionPolicy:
    whenDeleted: Delete
    whenScaled: Retain
  podManagementPolicy: Parallel
  replicas: 3
  selector:
    matchLabels:
      app.kubernetes.io/component: nb
      app.kubernetes.io/instance: ovn
      app.kubernetes.io/name: ovncentral
  serviceName: ovn-nb
  template:
    metadata:
      labels:
        app.kubernetes.io/component: nb
        app.kubernetes.io/instance: ovn
        app.kubernetes.io/managed-by: ovncentral-operator
        app.kubernetes.io/name: ovncentral
    spec:
      containers:
      - command:
        - /bin/bash
        - -c
        - exec /etc/ovn-central/bin/run-nb.sh
        env:
        - name: OVN_DBDIR
          value: /var/lib/ovn
        - name: ELECTION_TIMER_MS
          value: "1000"
        - name: INACTIVITY_PROBE_MS
          value: "60000"
        - name: POD_NAMESPACE
          valueFrom:
            fieldRef:
              fieldPath: metadata.namespace
        image: ghcr.io/c5c3/ovn:26.03.2
        lifecycle:
          postStart:
            exec:
              command:
              - /etc/ovn-central/bin/set-connection-nb.sh
          preStop:
            exec:
              command:
              - ovs-appctl
              - -t
              - /var/run/ovn/ovnnb_db.ctl
              - exit
        name: ovsdb
        ports:
        - containerPort: 6641
          name: client
        - containerPort: 6643
          name: raft
        readinessProbe:
          exec:
            command:
            - ovsdb-client
            - -p
            - /etc/ovn/tls/tls.key
            - -c
            - /etc/ovn/tls/tls.crt
            - -C
            - /etc/ovn/tls/ca.crt
            - list-dbs
            - ssl:127.0.0.1:6641
          initialDelaySeconds: 5
          periodSeconds: 5
          timeoutSeconds: 5
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
        - mountPath: /var/lib/ovn
          name: db
        - mountPath: /var/run/ovn
          name: run
        - mountPath: /var/log/ovn
          name: log
        - mountPath: /tmp
          name: tmp
        - mountPath: /etc/ovn/tls
          name: tls
          readOnly: true
        - mountPath: /etc/ovn-central/bin
          name: scripts
      securityContext:
        fsGroup: 42424
        runAsGroup: 42424
        runAsNonRoot: true
        runAsUser: 42424
        seccompProfile:
          type: RuntimeDefault
      terminationGracePeriodSeconds: 300
      volumes:
      - emptyDir: {}
        name: run
      - emptyDir: {}
        name: log
      - emptyDir: {}
        name: tmp
      - name: tls
        secret:
          secretName: ovn-nb-server
      - configMap:
          defaultMode: 365
          name: ovn-central-scripts
        name: scripts
  updateStrategy:
    type: RollingUpdate
  volumeClaimTemplates:
  - metadata:
      name: db
    spec:
      accessModes:
      - ReadWriteOnce
      resources:
        requests:
          storage: 1Gi
    status: {}
status:
  availableReplicas: 0
  replicas: 0
`

const pinSouthboundStatefulSetGolden = `metadata:
  labels:
    app.kubernetes.io/component: sb
    app.kubernetes.io/instance: ovn
    app.kubernetes.io/managed-by: ovncentral-operator
    app.kubernetes.io/name: ovncentral
  name: ovn-sb
  namespace: openstack
spec:
  persistentVolumeClaimRetentionPolicy:
    whenDeleted: Delete
    whenScaled: Retain
  podManagementPolicy: Parallel
  replicas: 3
  selector:
    matchLabels:
      app.kubernetes.io/component: sb
      app.kubernetes.io/instance: ovn
      app.kubernetes.io/name: ovncentral
  serviceName: ovn-sb
  template:
    metadata:
      labels:
        app.kubernetes.io/component: sb
        app.kubernetes.io/instance: ovn
        app.kubernetes.io/managed-by: ovncentral-operator
        app.kubernetes.io/name: ovncentral
    spec:
      containers:
      - command:
        - /bin/bash
        - -c
        - exec /etc/ovn-central/bin/run-sb.sh
        env:
        - name: OVN_DBDIR
          value: /var/lib/ovn
        - name: ELECTION_TIMER_MS
          value: "1000"
        - name: INACTIVITY_PROBE_MS
          value: "60000"
        - name: POD_NAMESPACE
          valueFrom:
            fieldRef:
              fieldPath: metadata.namespace
        image: ghcr.io/c5c3/ovn:26.03.2
        lifecycle:
          postStart:
            exec:
              command:
              - /etc/ovn-central/bin/set-connection-sb.sh
          preStop:
            exec:
              command:
              - ovs-appctl
              - -t
              - /var/run/ovn/ovnsb_db.ctl
              - exit
        name: ovsdb
        ports:
        - containerPort: 6642
          name: client
        - containerPort: 6644
          name: raft
        readinessProbe:
          exec:
            command:
            - ovsdb-client
            - -p
            - /etc/ovn/tls/tls.key
            - -c
            - /etc/ovn/tls/tls.crt
            - -C
            - /etc/ovn/tls/ca.crt
            - list-dbs
            - ssl:127.0.0.1:6642
          initialDelaySeconds: 5
          periodSeconds: 5
          timeoutSeconds: 5
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
        - mountPath: /var/lib/ovn
          name: db
        - mountPath: /var/run/ovn
          name: run
        - mountPath: /var/log/ovn
          name: log
        - mountPath: /tmp
          name: tmp
        - mountPath: /etc/ovn/tls
          name: tls
          readOnly: true
        - mountPath: /etc/ovn-central/bin
          name: scripts
      securityContext:
        fsGroup: 42424
        runAsGroup: 42424
        runAsNonRoot: true
        runAsUser: 42424
        seccompProfile:
          type: RuntimeDefault
      terminationGracePeriodSeconds: 300
      volumes:
      - emptyDir: {}
        name: run
      - emptyDir: {}
        name: log
      - emptyDir: {}
        name: tmp
      - name: tls
        secret:
          secretName: ovn-sb-server
      - configMap:
          defaultMode: 365
          name: ovn-central-scripts
        name: scripts
  updateStrategy:
    type: RollingUpdate
  volumeClaimTemplates:
  - metadata:
      name: db
    spec:
      accessModes:
      - ReadWriteOnce
      resources:
        requests:
          storage: 1Gi
    status: {}
status:
  availableReplicas: 0
  replicas: 0
`

const pinCustomStatefulSetGolden = `metadata:
  labels:
    app.kubernetes.io/component: nb
    app.kubernetes.io/instance: ovn
    app.kubernetes.io/managed-by: ovncentral-operator
    app.kubernetes.io/name: ovncentral
  name: ovn-nb
  namespace: openstack
spec:
  persistentVolumeClaimRetentionPolicy:
    whenDeleted: Delete
    whenScaled: Retain
  podManagementPolicy: Parallel
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/component: nb
      app.kubernetes.io/instance: ovn
      app.kubernetes.io/name: ovncentral
  serviceName: ovn-nb
  template:
    metadata:
      labels:
        app.kubernetes.io/component: nb
        app.kubernetes.io/instance: ovn
        app.kubernetes.io/managed-by: ovncentral-operator
        app.kubernetes.io/name: ovncentral
    spec:
      containers:
      - command:
        - /bin/bash
        - -c
        - exec /etc/ovn-central/bin/run-nb.sh
        env:
        - name: OVN_DBDIR
          value: /var/lib/ovn
        - name: ELECTION_TIMER_MS
          value: "5000"
        - name: INACTIVITY_PROBE_MS
          value: "30000"
        - name: POD_NAMESPACE
          valueFrom:
            fieldRef:
              fieldPath: metadata.namespace
        image: registry.example.com/ovn@sha256:1111111111111111111111111111111111111111111111111111111111111111
        lifecycle:
          postStart:
            exec:
              command:
              - /etc/ovn-central/bin/set-connection-nb.sh
          preStop:
            exec:
              command:
              - ovs-appctl
              - -t
              - /var/run/ovn/ovnnb_db.ctl
              - exit
        name: ovsdb
        ports:
        - containerPort: 6641
          name: client
        - containerPort: 6643
          name: raft
        readinessProbe:
          exec:
            command:
            - ovsdb-client
            - -p
            - /etc/ovn/tls/tls.key
            - -c
            - /etc/ovn/tls/tls.crt
            - -C
            - /etc/ovn/tls/ca.crt
            - list-dbs
            - ssl:127.0.0.1:6641
          initialDelaySeconds: 5
          periodSeconds: 5
          timeoutSeconds: 5
        resources:
          limits:
            memory: 1Gi
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
        - mountPath: /var/lib/ovn
          name: db
        - mountPath: /var/run/ovn
          name: run
        - mountPath: /var/log/ovn
          name: log
        - mountPath: /tmp
          name: tmp
        - mountPath: /etc/ovn/tls
          name: tls
          readOnly: true
        - mountPath: /etc/ovn-central/bin
          name: scripts
      securityContext:
        fsGroup: 42424
        runAsGroup: 42424
        runAsNonRoot: true
        runAsUser: 42424
        seccompProfile:
          type: RuntimeDefault
      terminationGracePeriodSeconds: 300
      volumes:
      - emptyDir: {}
        name: run
      - emptyDir: {}
        name: log
      - emptyDir: {}
        name: tmp
      - name: tls
        secret:
          secretName: ovn-nb-server
      - configMap:
          defaultMode: 365
          name: ovn-central-scripts
        name: scripts
  updateStrategy:
    type: RollingUpdate
  volumeClaimTemplates:
  - metadata:
      name: db
    spec:
      accessModes:
      - ReadWriteOnce
      resources:
        requests:
          storage: 10Gi
      storageClassName: fast
    status: {}
status:
  availableReplicas: 0
  replicas: 0
`

const pinCentralScriptsConfigMapGolden = `data:
  backup.sh: |
    #!/bin/bash
    set -eu
    dir="${BACKUP_DIR:-/backup}"
    ts="$(date -u +%Y%m%dT%H%M%SZ)"
    find "${dir}" -name '*.backup.tmp' -type f -mtime +1 -delete
    for spec in "nb:${NB_ADDR}:OVN_Northbound" "sb:${SB_ADDR}:OVN_Southbound"; do
      db="${spec%%:*}"; rest="${spec#*:}"; schema="${rest##*:}"; addr="${rest%:*}"
      out="${dir}/${db}-${ts}.backup"
      if ! ovsdb-client -p /etc/ovn/tls/tls.key -c /etc/ovn/tls/tls.crt -C /etc/ovn/tls/ca.crt \
          backup "${addr}" "${schema}" > "${out}.tmp"; then
        rm -f "${out}.tmp"; echo "backup of ${schema} at ${addr} failed" >&2; exit 1
      fi
      if [ ! -s "${out}.tmp" ]; then
        rm -f "${out}.tmp"; echo "backup of ${schema} at ${addr} produced an empty snapshot" >&2; exit 1
      fi
      mv "${out}.tmp" "${out}"
    done
    find "${dir}" -name '*.backup' -type f -mtime "+${RETENTION_DAYS}" -delete
    find "${dir}" -name '*.backup' -type f -size 0 -delete
  run-nb.sh: |
    #!/bin/bash
    set -eu
    FQDN="$(hostname -f)"
    ORD="${HOSTNAME##*-}"
    ARGS="--db-nb-cluster-local-addr=${FQDN} --db-nb-cluster-local-proto=ssl --ovn-nb-db-ssl-key=/etc/ovn/tls/tls.key --ovn-nb-db-ssl-cert=/etc/ovn/tls/tls.crt --ovn-nb-db-ssl-ca-cert=/etc/ovn/tls/ca.crt --db-nb-election-timer=${ELECTION_TIMER_MS}"
    if [ "$ORD" != 0 ]; then
      ARGS="$ARGS --db-nb-cluster-remote-addr=ovn-nb-0.ovn-nb.${POD_NAMESPACE}.svc.cluster.local --db-nb-cluster-remote-proto=ssl"
    fi
    exec /usr/share/ovn/scripts/ovn-ctl $ARGS run_nb_ovsdb
  run-sb.sh: |
    #!/bin/bash
    set -eu
    FQDN="$(hostname -f)"
    ORD="${HOSTNAME##*-}"
    ARGS="--db-sb-cluster-local-addr=${FQDN} --db-sb-cluster-local-proto=ssl --ovn-sb-db-ssl-key=/etc/ovn/tls/tls.key --ovn-sb-db-ssl-cert=/etc/ovn/tls/tls.crt --ovn-sb-db-ssl-ca-cert=/etc/ovn/tls/ca.crt --db-sb-election-timer=${ELECTION_TIMER_MS}"
    if [ "$ORD" != 0 ]; then
      ARGS="$ARGS --db-sb-cluster-remote-addr=ovn-sb-0.ovn-sb.${POD_NAMESPACE}.svc.cluster.local --db-sb-cluster-remote-proto=ssl"
    fi
    exec /usr/share/ovn/scripts/ovn-ctl $ARGS run_sb_ovsdb
  set-connection-nb.sh: |
    #!/bin/bash
    for i in $(seq 1 120); do
      if ovn-nbctl --no-leader-only --timeout=5 --db=unix:/var/run/ovn/ovnnb_db.sock set-connection pssl:6641:0.0.0.0 -- set connection . inactivity_probe=${INACTIVITY_PROBE_MS}; then
        exit 0
      fi
      sleep 1
    done
    exit 1
  set-connection-sb.sh: |
    #!/bin/bash
    for i in $(seq 1 120); do
      if ovn-sbctl --no-leader-only --timeout=5 --db=unix:/var/run/ovn/ovnsb_db.sock set-connection pssl:6642:0.0.0.0 -- set connection . inactivity_probe=${INACTIVITY_PROBE_MS}; then
        exit 0
      fi
      sleep 1
    done
    exit 1
metadata:
  labels:
    app.kubernetes.io/instance: ovn
    app.kubernetes.io/managed-by: ovncentral-operator
    app.kubernetes.io/name: ovncentral
  name: ovn-central-scripts
  namespace: openstack
`

// TestPinRaftHeadlessService pins the Service the Raft members resolve each
// other through. The two databases differ only in their ports and their
// component label, which is exactly what a shared builder can get wrong.
func TestPinRaftHeadlessService(t *testing.T) {
	cases := []struct {
		name   string
		db     func(*ovnv1alpha1.OVNCentral) raftDB
		golden string
	}{
		{name: "northbound", db: northboundDB, golden: pinNorthboundHeadlessServiceGolden},
		{name: "southbound", db: southboundDB, golden: pinSouthboundHeadlessServiceGolden},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			cr := testOVNCentral()

			got, err := yaml.Marshal(raftHeadlessService(cr, tc.db(cr)))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(string(got)).To(Equal(tc.golden),
				"the rendered headless Service must stay byte-identical")
		})
	}
}

// TestPinRaftMemberService pins the Service one member is addressed through, in
// both postures: the cluster-internal default, and the node port a CR opts into
// with externallyReachable. The pinned ordinal is 1 for the defaulted CR, so a
// builder that ignored the ordinal would render the base port and be caught.
func TestPinRaftMemberService(t *testing.T) {
	cases := []struct {
		name    string
		cr      func() *ovnv1alpha1.OVNCentral
		db      func(*ovnv1alpha1.OVNCentral) raftDB
		ordinal int32
		golden  string
	}{
		{name: "northbound", cr: testOVNCentral, db: northboundDB, ordinal: 1, golden: pinNorthboundMemberServiceGolden},
		{name: "southbound", cr: testOVNCentral, db: southboundDB, ordinal: 1, golden: pinSouthboundMemberServiceGolden},
		{name: "custom-base", cr: pinCustomOVNCentral, db: northboundDB, ordinal: 0, golden: pinCustomMemberServiceGolden},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			cr := tc.cr()

			got, err := yaml.Marshal(raftPerPodService(cr, tc.db(cr), tc.ordinal))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(string(got)).To(Equal(tc.golden),
				"the rendered member Service must stay byte-identical")
		})
	}
}

// TestPinRaftStatefulSet pins the workload itself across the two databases and
// across a CR that moves every Northbound knob off its default: a digest-pinned
// image, a single member, a named storage class, explicit resources, and both
// timers changed.
func TestPinRaftStatefulSet(t *testing.T) {
	cases := []struct {
		name   string
		cr     func() *ovnv1alpha1.OVNCentral
		db     func(*ovnv1alpha1.OVNCentral) raftDB
		golden string
	}{
		{name: "northbound", cr: testOVNCentral, db: northboundDB, golden: pinNorthboundStatefulSetGolden},
		{name: "southbound", cr: testOVNCentral, db: southboundDB, golden: pinSouthboundStatefulSetGolden},
		{name: "custom", cr: pinCustomOVNCentral, db: northboundDB, golden: pinCustomStatefulSetGolden},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			cr := tc.cr()

			got, err := yaml.Marshal(raftStatefulSet(cr, tc.db(cr)))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(string(got)).To(Equal(tc.golden),
				"the rendered StatefulSet must stay byte-identical")
		})
	}
}

// TestPinCentralScriptsConfigMap pins the scripts both databases run. The
// ConfigMap carries no per-database variant: one object holds the scripts of
// both, and both database steps apply it.
func TestPinCentralScriptsConfigMap(t *testing.T) {
	g := NewWithT(t)

	got, err := yaml.Marshal(centralScriptsConfigMap(testOVNCentral()))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(string(got)).To(Equal(pinCentralScriptsConfigMapGolden),
		"the rendered scripts ConfigMap must stay byte-identical")
}
