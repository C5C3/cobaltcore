// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Byte-identity pin for the two backup children. The goldens below are
// FULL-OBJECT YAML captured from the builders as they stand today, so any
// refactor of the backup projection has to reproduce every rendered byte. The
// snapshots are the only path back from a logical model an operator error
// corrupted on every Raft member at once, so a silent change to the schedule,
// the retention the script prunes by, the volume the snapshots land on, or the
// bucket they are copied to is a change to the recovery story.
package controller

import (
	"testing"

	. "github.com/onsi/gomega"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// pinBackupOVNCentral is the fixture with no spec.backup at all, so its goldens
// pin the defaults effectiveBackup resolves. Both database addresses are
// published, which is what the step needs before it projects anything.
func pinBackupOVNCentral() *ovnv1alpha1.OVNCentral {
	return publishEndpoints(testOVNCentral())
}

// pinCustomBackupOVNCentral moves every knob of spec.backup off its default, so
// a builder that reads the wrong field is caught.
func pinCustomBackupOVNCentral() *ovnv1alpha1.OVNCentral {
	cr := pinBackupOVNCentral()
	cr.Spec.Backup = &ovnv1alpha1.OVNBackupSpec{
		Schedule:      "0 */6 * * *",
		RetentionDays: ptr.To(int32(3)),
		Suspend:       true,
		Storage: ovnv1alpha1.OVNStorageSpec{
			Size:             "5Gi",
			StorageClassName: ptr.To("fast-ssd"),
		},
	}
	return cr
}

// pinS3BackupOVNCentral adds the off-cluster copy and pins the shifter image by
// digest. It is what turns the snapshot container into an init container.
func pinS3BackupOVNCentral() *ovnv1alpha1.OVNCentral {
	cr := pinBackupOVNCentral()
	cr.Spec.Backup = &ovnv1alpha1.OVNBackupSpec{
		S3: &ovnv1alpha1.OVNBackupS3Spec{
			Bucket:               "ovn-backups",
			Prefix:               "prod/ovn",
			Endpoint:             "https://s3.example.com",
			Region:               "eu-de-1",
			CredentialsSecretRef: commonv1.SecretRefSpec{Name: "ovn-backup-s3"},
			Image: &commonv1.ImageSpec{
				Repository: "registry.example.com/backup-shifter",
				Digest:     "sha256:2222222222222222222222222222222222222222222222222222222222222222",
			},
		},
	}
	return cr
}

const pinBackupPVCGolden = `metadata:
  labels:
    app.kubernetes.io/component: backup
    app.kubernetes.io/instance: ovn
    app.kubernetes.io/managed-by: ovncentral-operator
    app.kubernetes.io/name: ovncentral
  name: ovn-backup
  namespace: openstack
spec:
  accessModes:
  - ReadWriteOnce
  resources:
    requests:
      storage: 1Gi
status: {}
`

const pinCustomBackupPVCGolden = `metadata:
  labels:
    app.kubernetes.io/component: backup
    app.kubernetes.io/instance: ovn
    app.kubernetes.io/managed-by: ovncentral-operator
    app.kubernetes.io/name: ovncentral
  name: ovn-backup
  namespace: openstack
spec:
  accessModes:
  - ReadWriteOnce
  resources:
    requests:
      storage: 5Gi
  storageClassName: fast-ssd
status: {}
`

const pinBackupCronJobGolden = `metadata:
  labels:
    app.kubernetes.io/component: backup
    app.kubernetes.io/instance: ovn
    app.kubernetes.io/managed-by: ovncentral-operator
    app.kubernetes.io/name: ovncentral
  name: ovn-backup
  namespace: openstack
spec:
  concurrencyPolicy: Forbid
  failedJobsHistoryLimit: 3
  jobTemplate:
    metadata:
      labels:
        app.kubernetes.io/component: backup
        app.kubernetes.io/instance: ovn
        app.kubernetes.io/managed-by: ovncentral-operator
        app.kubernetes.io/name: ovncentral
    spec:
      activeDeadlineSeconds: 600
      backoffLimit: 0
      template:
        metadata:
          labels:
            app.kubernetes.io/component: backup
            app.kubernetes.io/instance: ovn
            app.kubernetes.io/managed-by: ovncentral-operator
            app.kubernetes.io/name: ovncentral
        spec:
          containers:
          - command:
            - /bin/bash
            - /etc/ovn-central/bin/backup.sh
            env:
            - name: NB_ADDR
              value: ssl:10.96.0.11:6641
            - name: SB_ADDR
              value: ssl:10.96.0.21:6642
            - name: RETENTION_DAYS
              value: "14"
            image: ghcr.io/c5c3/ovn:26.03.2
            name: backup
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
            - mountPath: /backup
              name: backup
            - mountPath: /etc/ovn/tls
              name: tls
              readOnly: true
            - mountPath: /etc/ovn-central/bin
              name: scripts
            - mountPath: /tmp
              name: tmp
          restartPolicy: Never
          securityContext:
            fsGroup: 42424
            seccompProfile:
              type: RuntimeDefault
          volumes:
          - name: backup
            persistentVolumeClaim:
              claimName: ovn-backup
          - name: tls
            secret:
              secretName: ovn-client
          - configMap:
              defaultMode: 365
              name: ovn-central-scripts
            name: scripts
          - emptyDir: {}
            name: tmp
  schedule: 0 2 * * *
  successfulJobsHistoryLimit: 3
  suspend: false
status: {}
`

const pinCustomBackupCronJobGolden = `metadata:
  labels:
    app.kubernetes.io/component: backup
    app.kubernetes.io/instance: ovn
    app.kubernetes.io/managed-by: ovncentral-operator
    app.kubernetes.io/name: ovncentral
  name: ovn-backup
  namespace: openstack
spec:
  concurrencyPolicy: Forbid
  failedJobsHistoryLimit: 3
  jobTemplate:
    metadata:
      labels:
        app.kubernetes.io/component: backup
        app.kubernetes.io/instance: ovn
        app.kubernetes.io/managed-by: ovncentral-operator
        app.kubernetes.io/name: ovncentral
    spec:
      activeDeadlineSeconds: 600
      backoffLimit: 0
      template:
        metadata:
          labels:
            app.kubernetes.io/component: backup
            app.kubernetes.io/instance: ovn
            app.kubernetes.io/managed-by: ovncentral-operator
            app.kubernetes.io/name: ovncentral
        spec:
          containers:
          - command:
            - /bin/bash
            - /etc/ovn-central/bin/backup.sh
            env:
            - name: NB_ADDR
              value: ssl:10.96.0.11:6641
            - name: SB_ADDR
              value: ssl:10.96.0.21:6642
            - name: RETENTION_DAYS
              value: "3"
            image: ghcr.io/c5c3/ovn:26.03.2
            name: backup
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
            - mountPath: /backup
              name: backup
            - mountPath: /etc/ovn/tls
              name: tls
              readOnly: true
            - mountPath: /etc/ovn-central/bin
              name: scripts
            - mountPath: /tmp
              name: tmp
          restartPolicy: Never
          securityContext:
            fsGroup: 42424
            seccompProfile:
              type: RuntimeDefault
          volumes:
          - name: backup
            persistentVolumeClaim:
              claimName: ovn-backup
          - name: tls
            secret:
              secretName: ovn-client
          - configMap:
              defaultMode: 365
              name: ovn-central-scripts
            name: scripts
          - emptyDir: {}
            name: tmp
  schedule: 0 */6 * * *
  successfulJobsHistoryLimit: 3
  suspend: true
status: {}
`

const pinS3BackupCronJobGolden = `metadata:
  labels:
    app.kubernetes.io/component: backup
    app.kubernetes.io/instance: ovn
    app.kubernetes.io/managed-by: ovncentral-operator
    app.kubernetes.io/name: ovncentral
  name: ovn-backup
  namespace: openstack
spec:
  concurrencyPolicy: Forbid
  failedJobsHistoryLimit: 3
  jobTemplate:
    metadata:
      labels:
        app.kubernetes.io/component: backup
        app.kubernetes.io/instance: ovn
        app.kubernetes.io/managed-by: ovncentral-operator
        app.kubernetes.io/name: ovncentral
    spec:
      activeDeadlineSeconds: 600
      backoffLimit: 0
      template:
        metadata:
          labels:
            app.kubernetes.io/component: backup
            app.kubernetes.io/instance: ovn
            app.kubernetes.io/managed-by: ovncentral-operator
            app.kubernetes.io/name: ovncentral
        spec:
          containers:
          - command:
            - /bin/sh
            - -c
            - rclone copy /backup ":s3:${BUCKET}/${PREFIX}"
            env:
            - name: RCLONE_S3_PROVIDER
              value: Other
            - name: RCLONE_S3_ENDPOINT
              value: https://s3.example.com
            - name: RCLONE_S3_REGION
              value: eu-de-1
            - name: RCLONE_S3_NO_CHECK_BUCKET
              value: "true"
            - name: RCLONE_S3_ACCESS_KEY_ID
              valueFrom:
                secretKeyRef:
                  key: access-key-id
                  name: ovn-backup-s3
            - name: RCLONE_S3_SECRET_ACCESS_KEY
              valueFrom:
                secretKeyRef:
                  key: secret-access-key
                  name: ovn-backup-s3
            - name: BUCKET
              value: ovn-backups
            - name: PREFIX
              value: prod/ovn
            image: registry.example.com/backup-shifter@sha256:2222222222222222222222222222222222222222222222222222222222222222
            name: shifter
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
            - mountPath: /backup
              name: backup
              readOnly: true
            - mountPath: /tmp
              name: tmp
          initContainers:
          - command:
            - /bin/bash
            - /etc/ovn-central/bin/backup.sh
            env:
            - name: NB_ADDR
              value: ssl:10.96.0.11:6641
            - name: SB_ADDR
              value: ssl:10.96.0.21:6642
            - name: RETENTION_DAYS
              value: "14"
            image: ghcr.io/c5c3/ovn:26.03.2
            name: backup
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
            - mountPath: /backup
              name: backup
            - mountPath: /etc/ovn/tls
              name: tls
              readOnly: true
            - mountPath: /etc/ovn-central/bin
              name: scripts
            - mountPath: /tmp
              name: tmp
          restartPolicy: Never
          securityContext:
            fsGroup: 42424
            seccompProfile:
              type: RuntimeDefault
          volumes:
          - name: backup
            persistentVolumeClaim:
              claimName: ovn-backup
          - name: tls
            secret:
              secretName: ovn-client
          - configMap:
              defaultMode: 365
              name: ovn-central-scripts
            name: scripts
          - emptyDir: {}
            name: tmp
  schedule: 0 2 * * *
  successfulJobsHistoryLimit: 3
  suspend: false
status: {}
`

const pinBackupScriptGolden = `#!/bin/bash
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
`

// TestPinBackupPVC pins the volume the snapshots are written to across a
// defaulted spec.backup and one that names its own size and storage class.
func TestPinBackupPVC(t *testing.T) {
	cases := []struct {
		name   string
		cr     func() *ovnv1alpha1.OVNCentral
		golden string
	}{
		{name: "default", cr: pinBackupOVNCentral, golden: pinBackupPVCGolden},
		{name: "custom", cr: pinCustomBackupOVNCentral, golden: pinCustomBackupPVCGolden},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			cr := tc.cr()

			got, err := yaml.Marshal(backupPVC(cr, effectiveBackup(cr)))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(string(got)).To(Equal(tc.golden),
				"the rendered backup PersistentVolumeClaim must stay byte-identical")
		})
	}
}

// TestPinBackupCronJob pins the CronJob across the three shapes it takes: the
// resolved defaults, every knob moved, and the S3 variant that demotes the
// snapshot container to an init container.
func TestPinBackupCronJob(t *testing.T) {
	cases := []struct {
		name   string
		cr     func() *ovnv1alpha1.OVNCentral
		golden string
	}{
		{name: "default", cr: pinBackupOVNCentral, golden: pinBackupCronJobGolden},
		{name: "custom", cr: pinCustomBackupOVNCentral, golden: pinCustomBackupCronJobGolden},
		{name: "s3", cr: pinS3BackupOVNCentral, golden: pinS3BackupCronJobGolden},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			cr := tc.cr()

			got, err := yaml.Marshal(backupCronJob(cr, effectiveBackup(cr)))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(string(got)).To(Equal(tc.golden),
				"the rendered backup CronJob must stay byte-identical")
		})
	}
}

// TestPinBackupScript pins the snapshot script itself. It is the one rendered
// artefact no golden above covers: the CronJob names it by path, and the file it
// resolves to is a key of the shared scripts ConfigMap.
func TestPinBackupScript(t *testing.T) {
	g := NewWithT(t)

	g.Expect(backupScript).To(Equal(pinBackupScriptGolden),
		"the rendered backup script must stay byte-identical")
}
