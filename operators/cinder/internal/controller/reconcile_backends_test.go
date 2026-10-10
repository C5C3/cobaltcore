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

// projectedSecret re-reads a projection Secret written by the backend steps.
func projectedSecret(t *testing.T, r *CinderReconciler, name string) *corev1.Secret {
	t.Helper()
	var secret corev1.Secret
	key := client.ObjectKey{Namespace: testNamespace, Name: name}
	if err := r.Get(context.Background(), key, &secret); err != nil {
		t.Fatalf("re-reading projection Secret %s: %v", name, err)
	}
	return &secret
}

func TestReconcileBackends_ZeroAttachedIsNoBackends(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cinder := validCinder()
	// The last backend detached, leaving its projection Secret behind.
	stale := staleProjectionSecret(t, cinder, "cinder-backend-gone")
	r := newCinderTestReconciler(cinder, stale)

	res, projections, hosts, err := r.reconcileBackends(ctx, r.Client, cinder)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue(), "waiting states never requeue")
	g.Expect(projections).To(BeNil())
	g.Expect(hosts).To(BeNil())
	g.Expect(r.Get(ctx, client.ObjectKeyFromObject(stale), &corev1.Secret{})).NotTo(Succeed(),
		"detaching the last backend sweeps its Secrets like detaching one of several does")

	cond := cinderCondition(cinder, conditionTypeBackendsReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue), "a Cinder without volume backends is not broken")
	g.Expect(cond.Reason).To(Equal(conditionReasonNoBackends))
	g.Expect(cond.Message).To(Equal("No CinderBackend is attached; volume services are not rendered"))
}

func TestReconcileBackends_SingleReadyBackendProjects(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	r := newCinderTestReconciler(cinder, credentialReadyBackend("nfs1"))

	res, projections, hosts, err := r.reconcileBackends(context.Background(), r.Client, cinder)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(projections).To(HaveLen(1))
	g.Expect(projections[0].name).To(Equal("nfs1"))
	g.Expect(projections[0].server).To(Equal("nfs1.nfs.example.com"))
	g.Expect(projections[0].path).To(Equal("/exports/nfs1"))
	g.Expect(projections[0].mountOptions).To(Equal(cinderv1alpha1.DefaultNFSMountOptions))
	g.Expect(projections[0].secretName).To(HavePrefix("cinder-backend-nfs1-"),
		"each backend renders under its own base name so one backend's change does not roll the others")
	g.Expect(hosts).To(ConsistOf("tcp://nfs1.nfs.example.com:2049"))

	secret := projectedSecret(t, r, projections[0].secretName)
	g.Expect(secret.Data).To(HaveLen(3))

	conf := secret.Data[backendConfDataKey]
	g.Expect(satellite.SectionPresent(conf, "[nfs1]")).To(BeTrue(),
		"the backend section header must be a whole line [nfs1]")
	for _, line := range []string{
		"volume_driver = cinder.volume.drivers.nfs.NfsDriver",
		"volume_backend_name = nfs1",
		"backend_host = cinder",
		"nfs_shares_config = /etc/cinder/backends.conf.d/nfs1.shares",
		"nfs_mount_point_base = /var/lib/cinder/mnt",
		"nfs_mount_options = " + cinderv1alpha1.DefaultNFSMountOptions,
		"nas_secure_file_operations = true",
		"nas_secure_file_permissions = true",
		"nfs_snapshot_support = false",
		"nfs_sparsed_volumes = true",
		"nfs_qcow2_volumes = false",
	} {
		g.Expect(string(conf)).To(ContainSubstring(line))
	}
	// The cache is off unless the backend asks for it.
	g.Expect(string(conf)).NotTo(ContainSubstring("image_volume_cache_enabled"))

	g.Expect(string(secret.Data[sharesDataKey])).To(Equal("nfs1.nfs.example.com:/exports/nfs1\n"))
	g.Expect(string(secret.Data[volumeOverlayDataKey])).To(Equal("[DEFAULT]\nenabled_backends = nfs1\n"))

	cond := cinderCondition(cinder, conditionTypeBackendsReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonAllBackendsProjected))
	g.Expect(cond.Message).To(Equal("All 1 attached backends are projected: nfs1"))
}

// TestReconcileBackends_EmptyMountOptionsRendersVerbatim covers the backend that
// spells the option string out as empty. The CRD default fills the field for
// every manifest that omits it, so an empty value is the submitter's own choice:
// mount the export with the kernel defaults. It is rendered verbatim, and the
// projection carries the same empty string, so the CSI mount the workload step
// builds and the driver's own nfs_mount_options agree on it.
func TestReconcileBackends_EmptyMountOptionsRendersVerbatim(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	backend := credentialReadyBackend("nfs1")
	backend.Spec.NFS.MountOptions = ""
	r := newCinderTestReconciler(cinder, backend)

	_, projections, _, err := r.reconcileBackends(context.Background(), r.Client, cinder)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(projections).To(HaveLen(1))
	g.Expect(projections[0].mountOptions).To(BeEmpty())
	conf := string(projectedSecret(t, r, projections[0].secretName).Data[backendConfDataKey])
	g.Expect(conf).To(ContainSubstring("nfs_mount_options = \n"))
}

func TestReconcileBackends_ImageVolumeCacheRendered(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	cached := credentialReadyBackend("cached")
	cached.Spec.ImageVolumeCache = &cinderv1alpha1.ImageVolumeCacheSpec{
		Enabled:   true,
		MaxSizeGB: ptr.To(int32(200)),
		MaxCount:  ptr.To(int32(50)),
	}
	// The bounds are configured but the cache is off: a disabled block renders
	// nothing at all, not even the bounds.
	off := credentialReadyBackend("off")
	off.Spec.ImageVolumeCache = &cinderv1alpha1.ImageVolumeCacheSpec{MaxSizeGB: ptr.To(int32(10))}
	r := newCinderTestReconciler(cinder, cached, off)

	_, projections, _, err := r.reconcileBackends(context.Background(), r.Client, cinder)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(projections).To(HaveLen(2))
	g.Expect(projections[0].name).To(Equal("cached"), "Collect sorts the projections by name")

	cachedConf := string(projectedSecret(t, r, projections[0].secretName).Data[backendConfDataKey])
	g.Expect(cachedConf).To(ContainSubstring("image_volume_cache_enabled = true"))
	g.Expect(cachedConf).To(ContainSubstring("image_volume_cache_max_size_gb = 200"))
	g.Expect(cachedConf).To(ContainSubstring("image_volume_cache_max_count = 50"))

	offConf := string(projectedSecret(t, r, projections[1].secretName).Data[backendConfDataKey])
	g.Expect(offConf).NotTo(ContainSubstring("image_volume_cache"))
}

func TestReconcileBackends_ExtraOptionsCannotOverrideOperatorKeys(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	backend := credentialReadyBackend("nfs1")
	// A CRD-bypass CR sets both a genuinely-extra option and one that collides
	// with an operator key; the operator key must win the collision.
	backend.Spec.ExtraOptions = map[string]string{
		"nfs_used_ratio": "0.95",
		"volume_driver":  "cinder.volume.drivers.lvm.LVMVolumeDriver",
	}
	r := newCinderTestReconciler(cinder, backend)

	_, projections, _, err := r.reconcileBackends(context.Background(), r.Client, cinder)

	g.Expect(err).NotTo(HaveOccurred())
	conf := string(projectedSecret(t, r, projections[0].secretName).Data[backendConfDataKey])
	g.Expect(conf).To(ContainSubstring("nfs_used_ratio = 0.95"))
	g.Expect(conf).To(ContainSubstring("volume_driver = cinder.volume.drivers.nfs.NfsDriver"),
		"the operator key wins over a colliding extraOption")
	g.Expect(conf).NotTo(ContainSubstring("LVMVolumeDriver"))
}

func TestReconcileBackends_NotCredentialReadyWaits(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	r := newCinderTestReconciler(cinder, testCinderBackend("nfs1"))

	res, projections, hosts, err := r.reconcileBackends(context.Background(), r.Client, cinder)

	g.Expect(err).NotTo(HaveOccurred(), "a pending backend never fails the step")
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(projections).To(BeEmpty())
	g.Expect(hosts).To(BeEmpty())

	cond := cinderCondition(cinder, conditionTypeBackendsReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForBackends))
	g.Expect(cond.Message).To(Equal("Waiting for backends: nfs1"))
}

// The export reaches the shares file rather than the rendered section, so the
// section guard never sees it. Admission rejects such a path, but a CR written
// past admission would otherwise put a second export line in front of the driver
// — naming a share the operator never mounted or derived an egress rule for.
func TestReconcileBackends_ControlCharInExportSkipsBackend(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	bad := credentialReadyBackend("bad")
	bad.Spec.NFS.Path = "/exports/bad\nnfs.example.com:/exports/other"
	r := newCinderTestReconciler(cinder, bad, credentialReadyBackend("nfs1"))

	res, projections, hosts, err := r.reconcileBackends(context.Background(), r.Client, cinder)

	g.Expect(err).NotTo(HaveOccurred(), "a per-backend fault never fails the step")
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(projections).To(HaveLen(1), "the healthy sibling is still projected")
	g.Expect(projections[0].name).To(Equal("nfs1"))
	g.Expect(hosts).To(ConsistOf("tcp://nfs1.nfs.example.com:2049"))

	events := collectEvents(r.Recorder.(*record.FakeRecorder))
	g.Expect(events).To(ContainElement(ContainSubstring("CinderBackendSkipped")))

	var secrets corev1.SecretList
	g.Expect(r.List(context.Background(), &secrets, client.InNamespace(testNamespace))).To(Succeed())
	for _, secret := range secrets.Items {
		g.Expect(secret.Name).NotTo(HavePrefix("cinder-backend-bad-"))
	}
}

func TestReconcileBackends_ControlCharSkipsBackendAndProjectsSiblings(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	// "bad" is credential-ready but one of its extraOptions values carries a
	// newline, which would inject further options into the rendered section.
	bad := credentialReadyBackend("bad")
	bad.Spec.ExtraOptions = map[string]string{"nfs_used_ratio": "0.95\nnas_secure_file_operations = false"}
	r := newCinderTestReconciler(cinder, bad, credentialReadyBackend("nfs1"))

	res, projections, hosts, err := r.reconcileBackends(context.Background(), r.Client, cinder)

	g.Expect(err).NotTo(HaveOccurred(), "a per-backend fault never fails the step")
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(projections).To(HaveLen(1), "the healthy sibling is still projected")
	g.Expect(projections[0].name).To(Equal("nfs1"))
	g.Expect(hosts).To(ConsistOf("tcp://nfs1.nfs.example.com:2049"))

	events := collectEvents(r.Recorder.(*record.FakeRecorder))
	g.Expect(events).To(ContainElement(ContainSubstring("CinderBackendSkipped")))

	// Nothing was written for the poisoned backend.
	var secrets corev1.SecretList
	g.Expect(r.List(context.Background(), &secrets, client.InNamespace(testNamespace))).To(Succeed())
	for _, secret := range secrets.Items {
		g.Expect(secret.Name).NotTo(HavePrefix("cinder-backend-bad-"))
	}

	cond := cinderCondition(cinder, conditionTypeBackendsReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForBackends))
	g.Expect(cond.Message).To(Equal("Waiting for backends: bad"))
}

// TestReconcileBackends_DeletingBackendIsNotProjected proves a detaching backend
// leaves the projection on the same pass its deletion timestamp is set, rather
// than when the object finally goes: the service-remove finalizer keeps it
// around until its Job completes.
func TestReconcileBackends_DeletingBackendIsNotProjected(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	deleting := credentialReadyBackend("going")
	deletion := metav1.Now()
	deleting.DeletionTimestamp = &deletion
	deleting.Finalizers = []string{"cinder.openstack.c5c3.io/service-remove"}
	r := newCinderTestReconciler(cinder, deleting, credentialReadyBackend("nfs1"))

	_, projections, hosts, err := r.reconcileBackends(context.Background(), r.Client, cinder)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(projections).To(HaveLen(1))
	g.Expect(projections[0].name).To(Equal("nfs1"))
	g.Expect(hosts).To(ConsistOf("tcp://nfs1.nfs.example.com:2049"))

	cond := cinderCondition(cinder, conditionTypeBackendsReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue),
		"a deleting backend is gone from the projection, not pending in it")
}

// TestReconcileBackends_StaleBaseNamesPruned covers the backend that detached:
// its own base name is swept whole, since every Secret under it carries the
// export details of a backend this Cinder no longer serves.
func TestReconcileBackends_StaleBaseNamesPruned(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cinder := validCinder()
	stale := staleProjectionSecret(t, cinder, "cinder-backend-old")
	r := newCinderTestReconciler(cinder, credentialReadyBackend("nfs1"), stale)

	_, projections, _, err := r.reconcileBackends(ctx, r.Client, cinder)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(projections).To(HaveLen(1))

	g.Expect(r.Get(ctx, client.ObjectKeyFromObject(stale), &corev1.Secret{})).NotTo(Succeed(),
		"the detached backend's projection Secret must be swept")
	g.Expect(r.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: projections[0].secretName},
		&corev1.Secret{})).To(Succeed(), "the projected backend's own Secret survives")
}

// TestReconcileBackends_CreateFailureIsWrapped covers the infrastructure fault:
// unlike a per-backend fault it surfaces as an error so the workqueue backs off,
// and it leaves the condition exactly as the previous pass left it.
func TestReconcileBackends_CreateFailureIsWrapped(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	cinder.Status.Conditions = []metav1.Condition{{
		Type:               conditionTypeBackendsReady,
		Status:             metav1.ConditionTrue,
		Reason:             conditionReasonAllBackendsProjected,
		Message:            "the previous pass",
		LastTransitionTime: metav1.Now(),
	}}
	c := cinderFakeClientBuilder(cinder, credentialReadyBackend("nfs1")).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*corev1.Secret); ok {
					return apierrors.NewInternalError(errors.New("etcd is unavailable"))
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).Build()
	r := &CinderReconciler{Client: c, Scheme: testScheme(), Recorder: record.NewFakeRecorder(10)}

	_, projections, _, err := r.reconcileBackends(context.Background(), r.Client, cinder)

	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring(`creating backend secret for "nfs1":`))
	g.Expect(projections).To(BeNil())

	cond := cinderCondition(cinder, conditionTypeBackendsReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Message).To(Equal("the previous pass"),
		"an infrastructure failure leaves the condition the last pass wrote")
}

// An RBD backend projects its section, the Ceph client configuration and the
// keyring into one Secret, and mounts no export: no shares file is written and
// no egress host is reported, because the Ceph rule is derived from the
// networks the projection carries.
func TestReconcileBackends_RBDBackendProjects(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	backend := credentialReadyRBDBackend("rbd-a")
	backend.Spec.RBD.Monitors = []string{"10.96.12.3:6789", "ceph-mon-b.rook-ceph.svc"}
	backend.Spec.RBD.Networks = []string{"10.244.0.0/16", "10.96.0.0/12"}
	r := newCinderTestReconciler(cinder, backend, rbdKeySecret("rbd-a", "  "+testRBDKey+"\n"))

	res, projections, hosts, err := r.reconcileBackends(context.Background(), r.Client, cinder)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(hosts).To(BeNil(), "an RBD backend mounts no export")
	g.Expect(projections).To(HaveLen(1))
	g.Expect(projections[0].name).To(Equal("rbd-a"))
	g.Expect(projections[0].backendType).To(Equal(cinderv1alpha1.CinderBackendTypeRBD))
	g.Expect(projections[0].server).To(BeEmpty())
	g.Expect(projections[0].path).To(BeEmpty())
	keySum := sha256.Sum256([]byte(testRBDKey))
	g.Expect(projections[0].rbd).To(Equal(&rbdProjection{
		clusterName: "ceph",
		user:        "cinder",
		networks:    []string{"10.244.0.0/16", "10.96.0.0/12"},
		keyDigest:   hex.EncodeToString(keySum[:]),
	}), "the digest is taken over the trimmed key")
	g.Expect(projections[0].secretName).To(HavePrefix("cinder-backend-rbd-a-"))

	secret := projectedSecret(t, r, projections[0].secretName)
	g.Expect(secret.Data).To(HaveLen(4))
	g.Expect(secret.Data).NotTo(HaveKey(sharesDataKey))
	g.Expect(string(secret.Data[backendConfDataKey])).To(Equal(`[rbd-a]
backend_host = cinder
rbd_ceph_conf = /etc/ceph/ceph.conf
rbd_cluster_name = ceph
rbd_pool = volumes
rbd_user = cinder
volume_backend_name = rbd-a
volume_driver = cinder.volume.drivers.rbd.RBDDriver
`))
	g.Expect(string(secret.Data[cephConfDataKey])).To(Equal(`[global]
mon_host = 10.96.12.3:6789,ceph-mon-b.rook-ceph.svc
keyring = /etc/ceph/ceph.client.cinder.keyring
log_file = /dev/null
admin_socket = /tmp/$cluster-$name.$pid.$cctid.asok
`))
	g.Expect(string(secret.Data[keyringDataKey])).To(Equal("[client.cinder]\n\tkey = "+testRBDKey+"\n"),
		"the key is trimmed of the whitespace a hand-made Secret picks up")
	g.Expect(string(secret.Data[volumeOverlayDataKey])).To(Equal("[DEFAULT]\nenabled_backends = rbd-a\n"))

	cond := cinderCondition(cinder, conditionTypeBackendsReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonAllBackendsProjected))
}

// The cluster name names the configuration and keyring files the section and
// ceph.conf point at, and rbd_secret_uuid is rendered only when it is set.
func TestReconcileBackends_RBDSecretUUIDRendered(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	backend := credentialReadyRBDBackend("rbd1")
	backend.Spec.RBD.ClusterName = "site-b"
	backend.Spec.RBD.SecretUUID = "457eb676-33da-42ec-9a8c-9293d545c337"
	r := newCinderTestReconciler(cinder, backend, rbdKeySecret("rbd1", testRBDKey))

	_, projections, _, err := r.reconcileBackends(context.Background(), r.Client, cinder)

	g.Expect(err).NotTo(HaveOccurred())
	secret := projectedSecret(t, r, projections[0].secretName)
	conf := string(secret.Data[backendConfDataKey])
	g.Expect(conf).To(ContainSubstring("rbd_secret_uuid = 457eb676-33da-42ec-9a8c-9293d545c337\n"))
	g.Expect(conf).To(ContainSubstring("rbd_cluster_name = site-b\n"))
	g.Expect(conf).To(ContainSubstring("rbd_ceph_conf = /etc/ceph/site-b.conf\n"))
	g.Expect(string(secret.Data[cephConfDataKey])).
		To(ContainSubstring("keyring = /etc/ceph/site-b.client.cinder.keyring\n"))

	// Unset, the driver falls back to the cluster FSID it reads at start.
	r = newCinderTestReconciler(cinder, credentialReadyRBDBackend("rbd1"), rbdKeySecret("rbd1", testRBDKey))
	_, projections, _, err = r.reconcileBackends(context.Background(), r.Client, cinder)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(string(projectedSecret(t, r, projections[0].secretName).Data[backendConfDataKey])).
		NotTo(ContainSubstring("rbd_secret_uuid"))
}

// A CR written past the webhook keeps its free-form options, but neither one
// that collides with an operator key nor one the webhook denies for the type:
// rbd_keyring_conf is rendered by nobody, and the denylist alone keeps it out.
func TestReconcileBackends_RBDExtraOptionsCannotOverrideOperatorKeys(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	backend := credentialReadyRBDBackend("rbd1")
	backend.Spec.ExtraOptions = map[string]string{
		"rados_connect_timeout": "5",
		"rbd_pool":              "images",
		"rbd_keyring_conf":      "/tmp/other.keyring",
	}
	r := newCinderTestReconciler(cinder, backend, rbdKeySecret("rbd1", testRBDKey))

	_, projections, _, err := r.reconcileBackends(context.Background(), r.Client, cinder)

	g.Expect(err).NotTo(HaveOccurred())
	conf := string(projectedSecret(t, r, projections[0].secretName).Data[backendConfDataKey])
	g.Expect(conf).To(ContainSubstring("rados_connect_timeout = 5\n"))
	g.Expect(conf).To(ContainSubstring("rbd_pool = volumes\n"), "the typed pool wins")
	g.Expect(conf).NotTo(ContainSubstring("images"))
	g.Expect(conf).NotTo(ContainSubstring("rbd_keyring_conf"))
}

// The gate saw the key, but the Secret is gone by the time the parent renders.
// The backend is skipped like any per-backend fault, and the warning names the
// Secret and the data key without ever carrying a key.
func TestReconcileBackends_RBDKeyVanishedBetweenGateAndRenderSkips(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	r := newCinderTestReconciler(cinder, credentialReadyRBDBackend("rbd1"), credentialReadyBackend("nfs1"))

	res, projections, hosts, err := r.reconcileBackends(context.Background(), r.Client, cinder)

	g.Expect(err).NotTo(HaveOccurred(), "a vanished key is a waiting state, not a failure")
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(projections).To(HaveLen(1), "the healthy sibling is still projected")
	g.Expect(projections[0].name).To(Equal("nfs1"))
	g.Expect(hosts).To(ConsistOf("tcp://nfs1.nfs.example.com:2049"))

	events := collectEvents(r.Recorder.(*record.FakeRecorder))
	g.Expect(events).To(ConsistOf(
		`Warning CinderBackendSkipped Skipping backend rbd1: RBD key Secret "rbd1-key" or its userKey data key is missing`))

	cond := cinderCondition(cinder, conditionTypeBackendsReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForBackends))
	g.Expect(cond.Message).To(Equal("Waiting for backends: rbd1"))
}

// A read that fails for another reason is an infrastructure fault: it surfaces
// as an error so the workqueue backs off.
func TestReconcileBackends_RBDKeyReadErrorIsWrapped(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	c := cinderFakeClientBuilder(cinder, credentialReadyRBDBackend("rbd1"), rbdKeySecret("rbd1", testRBDKey)).
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

	_, projections, _, err := r.reconcileBackends(context.Background(), r.Client, cinder)

	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring(`reading the RBD key of backend "rbd1":`))
	g.Expect(apierrors.IsInternalError(err)).To(BeTrue(), "the cause stays on the chain")
	g.Expect(projections).To(BeNil())
}

// The admission union rule guarantees spec.rbd on a type-RBD backend; one
// written past it has nothing to render.
func TestReconcileBackends_RBDBlockMissingSkips(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	backend := credentialReadyRBDBackend("rbd1")
	backend.Spec.RBD = nil
	r := newCinderTestReconciler(cinder, backend)

	_, projections, _, err := r.reconcileBackends(context.Background(), r.Client, cinder)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(projections).To(BeEmpty())
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).To(ConsistOf(
		"Warning CinderBackendSkipped Skipping backend rbd1: backend rbd1 has type RBD but no rbd block"))
	g.Expect(cinderCondition(cinder, conditionTypeBackendsReady).Message).To(Equal("Waiting for backends: rbd1"))
}

// The key reaches the keyring file verbatim, outside the rendered section, so a
// newline inside it would add a line the gate never saw. The render-time check
// skips the backend, and the warning names the files, never the key.
func TestReconcileBackends_RBDKeyControlCharSkips(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	poisoned := "AQDHlkVoYx3QKRAA\n[client.admin]"
	r := newCinderTestReconciler(cinder, credentialReadyRBDBackend("rbd1"), rbdKeySecret("rbd1", poisoned))

	_, projections, _, err := r.reconcileBackends(context.Background(), r.Client, cinder)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(projections).To(BeEmpty())
	events := collectEvents(r.Recorder.(*record.FakeRecorder))
	g.Expect(events).To(ConsistOf(ContainSubstring("Skipping backend rbd1:")))
	g.Expect(events[0]).To(ContainSubstring("rbd1/ceph"))
	g.Expect(events[0]).NotTo(ContainSubstring("AQDHlkVoYx3QKRAA"))
}

// A rotation reaches the render through the parent's Secret watch while the
// gate's CredentialsReady=True from before the rotation still stands. A key the
// gate would refuse is refused here as well, so it never reaches a keyring, and
// the warning names the Secret and the data key without the value.
func TestReconcileBackends_RBDKeyRotatedToANonCephxValueSkips(t *testing.T) {
	for _, tc := range []struct {
		name, key, wantFault string
	}{
		{
			name:      "empty",
			key:       " \n",
			wantFault: `Secret "rbd1-key" carries an empty userKey`,
		},
		{
			name:      "not base64",
			key:       "key = " + testRBDKey,
			wantFault: `Secret "rbd1-key" carries a userKey that is not a cephx key (base64 expected)`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cinder := validCinder()
			r := newCinderTestReconciler(cinder, credentialReadyRBDBackend("rbd1"), rbdKeySecret("rbd1", tc.key))

			_, projections, _, err := r.reconcileBackends(context.Background(), r.Client, cinder)

			g.Expect(err).NotTo(HaveOccurred(), "a refused key is a waiting state, not a failure")
			g.Expect(projections).To(BeEmpty())
			g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).To(ConsistOf(
				"Warning CinderBackendSkipped Skipping backend rbd1: " + tc.wantFault))
			g.Expect(cinderCondition(cinder, conditionTypeBackendsReady).Message).
				To(Equal("Waiting for backends: rbd1"))
		})
	}
}
