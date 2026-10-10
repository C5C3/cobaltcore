// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/c5c3/cobaltcore/internal/common/satellite"
	cinderv1alpha1 "github.com/c5c3/cobaltcore/operators/cinder/api/v1alpha1"
)

// backupSecretsInNamespace lists the projection Secrets of the backup service.
func backupSecretsInNamespace(t *testing.T, r *CinderReconciler) []string {
	t.Helper()
	var list corev1.SecretList
	if err := r.List(context.Background(), &list, client.InNamespace(testNamespace)); err != nil {
		t.Fatalf("listing Secrets: %v", err)
	}
	var names []string
	for _, secret := range list.Items {
		if len(secret.Data[backupConfDataKey]) > 0 {
			names = append(names, secret.Name)
		}
	}
	return names
}

func TestReconcileBackupBackend_ZeroAttachedIsNoBackupBackend(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cinder := validCinder()
	// The backup backend detached, leaving its projection Secret behind.
	stale := staleProjectionSecret(t, cinder, "cinder-backup-gone")
	r := newCinderTestReconciler(cinder, stale)

	res, projection, err := r.reconcileBackupBackend(ctx, r.Client, cinder)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue(), "waiting states never requeue")
	g.Expect(projection).To(BeNil())
	g.Expect(r.Get(ctx, client.ObjectKeyFromObject(stale), &corev1.Secret{})).NotTo(Succeed(),
		"a detached backup backend keeps no Deployment, so its Secrets are swept")

	cond := cinderCondition(cinder, conditionTypeBackupBackendReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue), "backups are opt-in, not missing")
	g.Expect(cond.Reason).To(Equal(conditionReasonNoBackupBackend))
	g.Expect(cond.Message).To(Equal("No CinderBackupBackend is attached; the backup service is not rendered"))
}

func TestReconcileBackupBackend_NotCredentialReadyWaits(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	r := newCinderTestReconciler(cinder, testCinderBackupBackend("nfs-backup"))

	res, projection, err := r.reconcileBackupBackend(context.Background(), r.Client, cinder)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(projection).To(BeNil())
	g.Expect(backupSecretsInNamespace(t, r)).To(BeEmpty())

	cond := cinderCondition(cinder, conditionTypeBackupBackendReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForBackupBackend))
	g.Expect(cond.Message).To(ContainSubstring("nfs-backup"))
}

// TestReconcileBackupBackend_TwoReadyIsAmbiguous covers the pair that raced past
// the webhook's single-attachment check: the backup driver is a property of the
// one cinder-backup Deployment, so a second candidate renders nothing at all
// rather than picking one arbitrarily.
func TestReconcileBackupBackend_TwoReadyIsAmbiguous(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	r := newCinderTestReconciler(cinder,
		credentialReadyBackupBackend("first"), credentialReadyBackupBackend("second"))

	res, projection, err := r.reconcileBackupBackend(context.Background(), r.Client, cinder)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(projection).To(BeNil())
	g.Expect(backupSecretsInNamespace(t, r)).To(BeEmpty(), "nothing is rendered while the choice is ambiguous")

	cond := cinderCondition(cinder, conditionTypeBackupBackendReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonMultipleBackupBackends))
	g.Expect(cond.Message).To(And(ContainSubstring("first"), ContainSubstring("second")))
}

func TestReconcileBackupBackend_SingleReadyProjects(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	r := newCinderTestReconciler(cinder, credentialReadyBackupBackend("nfs-backup"))

	res, projection, err := r.reconcileBackupBackend(context.Background(), r.Client, cinder)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(projection).NotTo(BeNil())
	g.Expect(projection.name).To(Equal("nfs-backup"))
	g.Expect(projection.backupType).To(Equal(cinderv1alpha1.CinderBackupBackendTypeNFS))
	g.Expect(projection.server).To(Equal("nfs-backup.nfs.example.com"))
	g.Expect(projection.path).To(Equal("/exports/nfs-backup"))
	g.Expect(projection.rbd).To(BeNil())
	g.Expect(projection.secretName).To(HavePrefix("cinder-backup-nfs-backup-"))

	secret := projectedSecret(t, r, projection.secretName)
	g.Expect(secret.Data).To(HaveLen(1))
	conf := secret.Data[backupConfDataKey]
	g.Expect(satellite.SectionPresent(conf, "[DEFAULT]")).To(BeTrue())
	g.Expect(string(conf)).To(Equal(`[DEFAULT]
backup_compression_algorithm = zlib
backup_driver = cinder.backup.drivers.nfs.NFSBackupDriver
backup_file_size = 52428800
backup_mount_options = ` + cinderv1alpha1.DefaultNFSMountOptions + `
backup_mount_point_base = /var/lib/cinder/backup_mount
backup_share = nfs-backup.nfs.example.com:/exports/nfs-backup
backup_use_same_host = false
host = cinder-backup
`))

	cond := cinderCondition(cinder, conditionTypeBackupBackendReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonBackupBackendProjected))
	g.Expect(cond.Message).To(ContainSubstring("nfs-backup"))
}

func TestReconcileBackupBackend_CustomKnobsAndExtraOptionsRendered(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	backupBackend := credentialReadyBackupBackend("nfs-backup")
	backupBackend.Spec.FileSize = ptr.To(int64(104857600))
	backupBackend.Spec.Compression = "zstd"
	// A CRD-bypass CR sets both a genuinely-extra option and one that collides
	// with an operator key; the operator key must win the collision.
	backupBackend.Spec.ExtraOptions = map[string]string{
		"backup_enable_progress_timer": "false",
		"backup_share":                 "attacker.example.com:/exports/elsewhere",
	}
	r := newCinderTestReconciler(cinder, backupBackend)

	_, projection, err := r.reconcileBackupBackend(context.Background(), r.Client, cinder)

	g.Expect(err).NotTo(HaveOccurred())
	conf := string(projectedSecret(t, r, projection.secretName).Data[backupConfDataKey])
	g.Expect(conf).To(ContainSubstring("backup_file_size = 104857600"))
	g.Expect(conf).To(ContainSubstring("backup_compression_algorithm = zstd"))
	g.Expect(conf).To(ContainSubstring("backup_enable_progress_timer = false"))
	g.Expect(conf).To(ContainSubstring("backup_share = nfs-backup.nfs.example.com:/exports/nfs-backup"),
		"the operator key wins over a colliding extraOption")
	g.Expect(conf).NotTo(ContainSubstring("attacker.example.com"))
}

// TestReconcileBackupBackend_EmptyMountOptionsRendersVerbatim covers the backup
// backend that spells the option string out as empty: the same submitter choice
// the volume backends allow, rendered verbatim so the projected mount and the
// driver's backup_mount_options agree on it.
func TestReconcileBackupBackend_EmptyMountOptionsRendersVerbatim(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	backupBackend := credentialReadyBackupBackend("nfs-backup")
	backupBackend.Spec.NFS.MountOptions = ""
	r := newCinderTestReconciler(cinder, backupBackend)

	_, projection, err := r.reconcileBackupBackend(context.Background(), r.Client, cinder)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(projection).NotTo(BeNil())
	g.Expect(projection.mountOptions).To(BeEmpty())
	conf := string(projectedSecret(t, r, projection.secretName).Data[backupConfDataKey])
	g.Expect(conf).To(ContainSubstring("backup_mount_options = \n"))
}

// TestReconcileBackupBackend_ControlCharSkipsProjection covers the value that
// would inject further options into the rendered [DEFAULT] section: nothing is
// rendered, the fault is warned about, and the condition names the wait.
func TestReconcileBackupBackend_ControlCharSkipsProjection(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	backupBackend := credentialReadyBackupBackend("nfs-backup")
	backupBackend.Spec.ExtraOptions = map[string]string{
		"backup_enable_progress_timer": "false\nbackup_use_same_host = true",
	}
	r := newCinderTestReconciler(cinder, backupBackend)

	res, projection, err := r.reconcileBackupBackend(context.Background(), r.Client, cinder)

	g.Expect(err).NotTo(HaveOccurred(), "a per-backend fault never fails the step")
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(projection).To(BeNil())
	g.Expect(backupSecretsInNamespace(t, r)).To(BeEmpty())

	events := collectEvents(r.Recorder.(*record.FakeRecorder))
	g.Expect(events).To(ContainElement(ContainSubstring("CinderBackupBackendSkipped")))

	cond := cinderCondition(cinder, conditionTypeBackupBackendReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForBackupBackend))
}

// TestReconcileBackupBackend_StaleBaseNamePruned covers the replaced backup
// backend: the Secrets of the one that detached carry the export a restore would
// read, so they are swept rather than retained.
func TestReconcileBackupBackend_StaleBaseNamePruned(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cinder := validCinder()
	stale := staleProjectionSecret(t, cinder, "cinder-backup-previous")
	r := newCinderTestReconciler(cinder, credentialReadyBackupBackend("nfs-backup"), stale)

	_, projection, err := r.reconcileBackupBackend(ctx, r.Client, cinder)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(projection).NotTo(BeNil())
	g.Expect(r.Get(ctx, client.ObjectKeyFromObject(stale), &corev1.Secret{})).NotTo(Succeed(),
		"the replaced backup backend's Secret must be swept")
	g.Expect(r.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: projection.secretName},
		&corev1.Secret{})).To(Succeed())
}

// TestReconcileBackupBackend_CreateFailureIsWrapped covers the infrastructure
// fault: it surfaces as an error so the workqueue backs off, unlike the
// per-backend faults above.
func TestReconcileBackupBackend_CreateFailureIsWrapped(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	c := cinderFakeClientBuilder(cinder, credentialReadyBackupBackend("nfs-backup")).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*corev1.Secret); ok {
					return apierrors.NewInternalError(errors.New("etcd is unavailable"))
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).Build()
	r := &CinderReconciler{Client: c, Scheme: testScheme(), Recorder: record.NewFakeRecorder(10)}

	_, projection, err := r.reconcileBackupBackend(context.Background(), r.Client, cinder)

	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring(`creating backup secret for "nfs-backup":`))
	g.Expect(projection).To(BeNil())
	g.Expect(cinderCondition(cinder, conditionTypeBackupBackendReady)).To(BeNil(),
		"an infrastructure failure leaves the condition untouched")
}

// An RBD target projects the Ceph backup driver's section, the Ceph client
// configuration and the keyring into one Secret. The chunked driver's fileSize
// and compression are not rendered, even when the CR carries them, because the
// Ceph driver reads neither.
func TestReconcileBackupBackend_RBDTargetProjects(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	backupBackend := credentialReadyRBDBackupBackend("rbdbk")
	backupBackend.Spec.FileSize = ptr.To(cinderv1alpha1.DefaultBackupFileSize)
	backupBackend.Spec.RBD.Monitors = []string{"10.96.12.3:6789", "ceph-mon-b.rook-ceph.svc"}
	backupBackend.Spec.RBD.Networks = []string{"10.244.0.0/16", "10.96.0.0/12"}
	r := newCinderTestReconciler(cinder, backupBackend, rbdKeySecret("rbdbk", "  "+testRBDKey+"\n"))

	res, projection, err := r.reconcileBackupBackend(context.Background(), r.Client, cinder)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(projection).NotTo(BeNil())
	g.Expect(projection.name).To(Equal("rbdbk"))
	g.Expect(projection.backupType).To(Equal(cinderv1alpha1.CinderBackupBackendTypeRBD))
	g.Expect(projection.server).To(BeEmpty(), "an RBD target mounts no export")
	g.Expect(projection.path).To(BeEmpty())
	keySum := sha256.Sum256([]byte(testRBDKey))
	g.Expect(projection.rbd).To(Equal(&rbdProjection{
		clusterName: "ceph",
		user:        "cinder-backup",
		networks:    []string{"10.244.0.0/16", "10.96.0.0/12"},
		keyDigest:   hex.EncodeToString(keySum[:]),
	}), "the digest is taken over the trimmed key")
	g.Expect(projection.rbd.keyDigest).To(MatchRegexp(`^[0-9a-f]{64}$`))
	g.Expect(projection.secretName).To(HavePrefix("cinder-backup-rbdbk-"))

	secret := projectedSecret(t, r, projection.secretName)
	g.Expect(secret.Data).To(HaveLen(3))
	g.Expect(secret.Data).To(HaveKey(backupConfDataKey))
	g.Expect(secret.Data).To(HaveKey(cephConfDataKey))
	g.Expect(secret.Data).To(HaveKey(keyringDataKey))
	g.Expect(string(secret.Data[backupConfDataKey])).To(Equal(`[DEFAULT]
backup_ceph_conf = /etc/ceph/ceph.conf
backup_ceph_pool = backups
backup_ceph_user = cinder-backup
backup_driver = cinder.backup.drivers.ceph.CephBackupDriver
backup_use_same_host = false
host = cinder-backup
`))
	g.Expect(string(secret.Data[cephConfDataKey])).To(Equal(`[global]
mon_host = 10.96.12.3:6789,ceph-mon-b.rook-ceph.svc
keyring = /etc/ceph/ceph.client.cinder-backup.keyring
log_file = /dev/null
admin_socket = /tmp/$cluster-$name.$pid.$cctid.asok
`))
	g.Expect(string(secret.Data[keyringDataKey])).To(Equal("[client.cinder-backup]\n\tkey = "+testRBDKey+"\n"),
		"the key is trimmed of the whitespace a hand-made Secret picks up")

	cond := cinderCondition(cinder, conditionTypeBackupBackendReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonBackupBackendProjected))

	// The cluster name names both files the section and ceph.conf point at.
	renamed := credentialReadyRBDBackupBackend("rbdbk")
	renamed.Spec.RBD.ClusterName = "site-b"
	r = newCinderTestReconciler(cinder, renamed, rbdKeySecret("rbdbk", testRBDKey))
	_, projection, err = r.reconcileBackupBackend(context.Background(), r.Client, cinder)
	g.Expect(err).NotTo(HaveOccurred())
	secret = projectedSecret(t, r, projection.secretName)
	g.Expect(string(secret.Data[backupConfDataKey])).To(ContainSubstring("backup_ceph_conf = /etc/ceph/site-b.conf\n"))
	g.Expect(string(secret.Data[cephConfDataKey])).
		To(ContainSubstring("keyring = /etc/ceph/site-b.client.cinder-backup.keyring\n"))
}

// A CR written past the webhook keeps its free-form options, but neither one
// that collides with an operator key nor one the webhook denies for the type.
func TestReconcileBackupBackend_RBDExtraOptionsCannotOverrideOperatorKeys(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	backupBackend := credentialReadyRBDBackupBackend("rbdbk")
	backupBackend.Spec.ExtraOptions = map[string]string{
		"backup_ceph_chunk_size": "67108864",
		"backup_ceph_pool":       "images",
		"backup_ceph_conf":       "/tmp/other.conf",
	}
	r := newCinderTestReconciler(cinder, backupBackend, rbdKeySecret("rbdbk", testRBDKey))

	_, projection, err := r.reconcileBackupBackend(context.Background(), r.Client, cinder)

	g.Expect(err).NotTo(HaveOccurred())
	conf := string(projectedSecret(t, r, projection.secretName).Data[backupConfDataKey])
	g.Expect(conf).To(ContainSubstring("backup_ceph_chunk_size = 67108864\n"))
	g.Expect(conf).To(ContainSubstring("backup_ceph_pool = backups\n"), "the typed pool wins")
	g.Expect(conf).NotTo(ContainSubstring("images"))
	g.Expect(conf).To(ContainSubstring("backup_ceph_conf = /etc/ceph/ceph.conf\n"))
	g.Expect(conf).NotTo(ContainSubstring("/tmp/other.conf"))
}

// The gate saw the key, but the Secret is gone by the time the parent renders.
// The target is skipped like any per-target fault, and the warning names the
// Secret and the data key without ever carrying a key.
func TestReconcileBackupBackend_RBDKeyVanishedBetweenGateAndRenderSkips(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	r := newCinderTestReconciler(cinder, credentialReadyRBDBackupBackend("rbdbk"))

	res, projection, err := r.reconcileBackupBackend(context.Background(), r.Client, cinder)

	g.Expect(err).NotTo(HaveOccurred(), "a vanished key is a waiting state, not a failure")
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(projection).To(BeNil())
	g.Expect(backupSecretsInNamespace(t, r)).To(BeEmpty())

	events := collectEvents(r.Recorder.(*record.FakeRecorder))
	g.Expect(events).To(ConsistOf(
		`Warning CinderBackupBackendSkipped Skipping backup backend rbdbk: RBD key Secret "rbdbk-key" or its userKey data key is missing`))
	g.Expect(events[0]).NotTo(ContainSubstring(testRBDKey))

	cond := cinderCondition(cinder, conditionTypeBackupBackendReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForBackupBackend))
	g.Expect(cond.Message).To(ContainSubstring(`RBD key Secret "rbdbk-key"`))
}

// A read that fails for another reason is an infrastructure fault: it surfaces
// as an error so the workqueue backs off.
func TestReconcileBackupBackend_RBDKeyReadErrorIsWrapped(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	c := cinderFakeClientBuilder(cinder, credentialReadyRBDBackupBackend("rbdbk"), rbdKeySecret("rbdbk", testRBDKey)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey,
				obj client.Object, opts ...client.GetOption,
			) error {
				if _, ok := obj.(*corev1.Secret); ok {
					return apierrors.NewInternalError(errors.New("etcd is unavailable"))
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	r := &CinderReconciler{Client: c, Scheme: testScheme(), Recorder: record.NewFakeRecorder(10)}

	_, projection, err := r.reconcileBackupBackend(context.Background(), r.Client, cinder)

	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring(`reading the RBD key of backup backend "rbdbk":`))
	g.Expect(apierrors.IsInternalError(err)).To(BeTrue(), "the cause stays on the chain")
	g.Expect(projection).To(BeNil())
	g.Expect(cinderCondition(cinder, conditionTypeBackupBackendReady)).To(BeNil(),
		"an infrastructure failure leaves the condition untouched")
}

// The admission union rule guarantees spec.rbd on a type-RBD target; one
// written past it has nothing to render.
func TestReconcileBackupBackend_RBDBlockMissingSkips(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	backupBackend := credentialReadyRBDBackupBackend("rbdbk")
	backupBackend.Spec.RBD = nil
	r := newCinderTestReconciler(cinder, backupBackend)

	_, projection, err := r.reconcileBackupBackend(context.Background(), r.Client, cinder)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(projection).To(BeNil())
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).To(ConsistOf(
		"Warning CinderBackupBackendSkipped Skipping backup backend rbdbk: backup backend rbdbk has type RBD but no rbd block"))
	g.Expect(cinderCondition(cinder, conditionTypeBackupBackendReady).Reason).
		To(Equal(conditionReasonWaitingForBackupBackend))
}

// A rotation reaches the render through the parent's Secret watch while the
// gate's CredentialsReady=True from before the rotation still stands. A key the
// gate would refuse is refused here as well, so it never reaches a keyring, and
// the warning names the Secret and the data key without the value; a newline in
// the key is caught before it can add a keyring line.
func TestReconcileBackupBackend_RBDKeyNotCephxSkips(t *testing.T) {
	for _, tc := range []struct {
		name, key, wantFault string
	}{
		{
			name:      "empty",
			key:       " \n",
			wantFault: `Secret "rbdbk-key" carries an empty userKey`,
		},
		{
			name:      "not base64",
			key:       "key = " + testRBDKey,
			wantFault: `Secret "rbdbk-key" carries a userKey that is not a cephx key (base64 expected)`,
		},
		{
			name:      "a newline inside the key",
			key:       "AQDHlkVoYx3QKRAA\n[client.admin]",
			wantFault: "DEFAULT/ceph",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cinder := validCinder()
			r := newCinderTestReconciler(cinder, credentialReadyRBDBackupBackend("rbdbk"), rbdKeySecret("rbdbk", tc.key))

			_, projection, err := r.reconcileBackupBackend(context.Background(), r.Client, cinder)

			g.Expect(err).NotTo(HaveOccurred(), "a refused key is a waiting state, not a failure")
			g.Expect(projection).To(BeNil())
			g.Expect(backupSecretsInNamespace(t, r)).To(BeEmpty())
			events := collectEvents(r.Recorder.(*record.FakeRecorder))
			g.Expect(events).To(ConsistOf(
				HavePrefix("Warning CinderBackupBackendSkipped Skipping backup backend rbdbk: ")))
			g.Expect(events[0]).To(ContainSubstring(tc.wantFault))
			g.Expect(events[0]).NotTo(ContainSubstring("AQDHlkVoYx3QKRAA"))
		})
	}
}

// An extraOptions value written past the webhook with a newline would add a
// line to backup.conf, such as a second backup_ceph_conf. The RBD section is
// checked on its own, so the target is skipped before anything is rendered.
func TestReconcileBackupBackend_RBDControlCharSkipsProjection(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	backupBackend := credentialReadyRBDBackupBackend("rbdbk")
	backupBackend.Spec.ExtraOptions = map[string]string{
		"backup_ceph_chunk_size": "67108864\nbackup_ceph_conf = /tmp/other.conf",
	}
	r := newCinderTestReconciler(cinder, backupBackend, rbdKeySecret("rbdbk", testRBDKey))

	res, projection, err := r.reconcileBackupBackend(context.Background(), r.Client, cinder)

	g.Expect(err).NotTo(HaveOccurred(), "a per-backend fault never fails the step")
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(projection).To(BeNil())
	g.Expect(backupSecretsInNamespace(t, r)).To(BeEmpty())
	events := collectEvents(r.Recorder.(*record.FakeRecorder))
	g.Expect(events).To(ConsistOf(And(
		HavePrefix("Warning CinderBackupBackendSkipped Skipping backup backend rbdbk: "),
		ContainSubstring("[DEFAULT] option name or value contains a newline"))))
	g.Expect(events[0]).NotTo(ContainSubstring(testRBDKey))
	g.Expect(cinderCondition(cinder, conditionTypeBackupBackendReady).Reason).
		To(Equal(conditionReasonWaitingForBackupBackend))
}
