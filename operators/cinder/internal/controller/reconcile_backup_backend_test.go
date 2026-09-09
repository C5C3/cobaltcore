// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
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
	g.Expect(projection.server).To(Equal("nfs-backup.nfs.example.com"))
	g.Expect(projection.path).To(Equal("/exports/nfs-backup"))
	g.Expect(projection.secretName).To(HavePrefix("cinder-backup-nfs-backup-"))

	secret := projectedSecret(t, r, projection.secretName)
	g.Expect(secret.Data).To(HaveLen(1))
	conf := secret.Data[backupConfDataKey]
	g.Expect(satellite.SectionPresent(conf, "[DEFAULT]")).To(BeTrue())
	for _, line := range []string{
		"host = cinder-backup",
		"backup_driver = cinder.backup.drivers.nfs.NFSBackupDriver",
		"backup_share = nfs-backup.nfs.example.com:/exports/nfs-backup",
		"backup_mount_point_base = /var/lib/cinder/backup_mount",
		"backup_mount_options = " + cinderv1alpha1.DefaultNFSMountOptions,
		"backup_file_size = 52428800",
		"backup_compression_algorithm = zlib",
		"backup_use_same_host = false",
	} {
		g.Expect(string(conf)).To(ContainSubstring(line))
	}

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
