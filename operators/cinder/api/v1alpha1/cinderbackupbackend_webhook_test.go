// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"context"
	"errors"
	"testing"

	"github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// validCinderBackupBackend returns a minimal valid NFS-typed CinderBackupBackend
// the per-rule tests mutate one field of, so every rejection is attributable to
// exactly one rule.
func validCinderBackupBackend() *CinderBackupBackend {
	return &CinderBackupBackend{
		ObjectMeta: metav1.ObjectMeta{Name: "nfs-backup", Namespace: "openstack"},
		Spec: CinderBackupBackendSpec{
			CinderRef:   CinderRefSpec{Name: "cinder"},
			Type:        CinderBackupBackendTypeNFS,
			FileSize:    ptr.To(DefaultBackupFileSize),
			Compression: DefaultBackupCompression,
			NFS: &NFSBackupBackendSpec{
				Server:       "nfs.example.com",
				Path:         "/exports/backups",
				MountOptions: DefaultNFSMountOptions,
			},
		},
	}
}

// validRBDCinderBackupBackend returns a minimal valid RBD-typed
// CinderBackupBackend, the RBD counterpart of validCinderBackupBackend. It
// carries the fileSize and compression defaults admission materializes on every
// backup target, inert on this type.
func validRBDCinderBackupBackend() *CinderBackupBackend {
	return &CinderBackupBackend{
		ObjectMeta: metav1.ObjectMeta{Name: "rbd-backup", Namespace: "openstack"},
		Spec: CinderBackupBackendSpec{
			CinderRef:   CinderRefSpec{Name: "cinder"},
			Type:        CinderBackupBackendTypeRBD,
			FileSize:    ptr.To(DefaultBackupFileSize),
			Compression: DefaultBackupCompression,
			RBD: &RBDBackupBackendSpec{
				Pool:         "backups",
				User:         "cinder-backup",
				Monitors:     []string{"ceph-mon.openstack.svc.cluster.local"},
				Networks:     []string{"10.244.0.0/16"},
				ClusterName:  DefaultRBDClusterName,
				KeySecretRef: SecretNameRefSpec{Name: "ceph-client-cinder-backup"},
			},
		},
	}
}

func TestCinderBackupBackendDefault_FillsChunkAndMountDefaults(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &CinderBackupBackendWebhook{}

	empty := validCinderBackupBackend()
	empty.Spec.FileSize = nil
	empty.Spec.Compression = ""
	empty.Spec.NFS.MountOptions = ""

	g.Expect(w.Default(context.Background(), empty)).To(gomega.Succeed())
	g.Expect(empty.Spec.FileSize).To(gomega.HaveValue(gomega.Equal(DefaultBackupFileSize)))
	g.Expect(empty.Spec.Compression).To(gomega.Equal("zlib"))
	g.Expect(empty.Spec.NFS.MountOptions).To(gomega.Equal(DefaultNFSMountOptions))

	explicit := validCinderBackupBackend()
	explicit.Spec.FileSize = ptr.To(int64(104857600))
	explicit.Spec.Compression = "zstd"
	explicit.Spec.NFS.MountOptions = "nfsvers=3,soft"

	g.Expect(w.Default(context.Background(), explicit)).To(gomega.Succeed())
	g.Expect(explicit.Spec.FileSize).To(gomega.HaveValue(gomega.Equal(int64(104857600))))
	g.Expect(explicit.Spec.Compression).To(gomega.Equal("zstd"))
	g.Expect(explicit.Spec.NFS.MountOptions).To(gomega.Equal("nfsvers=3,soft"))
}

func TestCinderBackupBackendValidate_AcceptsValidBackend(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &CinderBackupBackendWebhook{}

	_, err := w.ValidateCreate(context.Background(), validCinderBackupBackend())
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

func TestCinderBackupBackendValidate_RejectionTable(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(o *CinderBackupBackend)
		wantSub string
	}{
		{
			name:    "type NFS without the nfs block rejected",
			mutate:  func(o *CinderBackupBackend) { o.Spec.NFS = nil },
			wantSub: "exactly one backup backend block matching spec.type",
		},
		{
			name:    "nfs block under another type rejected",
			mutate:  func(o *CinderBackupBackend) { o.Spec.Type = CinderBackupBackendType("Swift") },
			wantSub: "exactly one backup backend block matching spec.type",
		},
		{
			// A chunk below a mebibyte turns a large volume into a backup of tens of
			// thousands of objects, each with its own round trip.
			name:    "fileSize below the floor rejected",
			mutate:  func(o *CinderBackupBackend) { o.Spec.FileSize = ptr.To(int64(65536)) },
			wantSub: "fileSize must be at least 1048576 bytes",
		},
		{
			// cinder hashes each chunk in 32 KiB blocks and refuses a chunk size that
			// block size does not divide, so an unaligned value fails every backup.
			name:    "fileSize off the block size rejected",
			mutate:  func(o *CinderBackupBackend) { o.Spec.FileSize = ptr.To(int64(52428801)) },
			wantSub: "fileSize must be a multiple of 32768 bytes",
		},
		{
			name:    "compression outside the enum rejected",
			mutate:  func(o *CinderBackupBackend) { o.Spec.Compression = "lz4" },
			wantSub: "Unsupported value",
		},
		{
			// The path is rendered verbatim as backup_share, where the reconcile-time
			// guard would delete the backup Deployment rather than reject the edit.
			name: "nfs path with a newline rejected",
			mutate: func(o *CinderBackupBackend) {
				o.Spec.NFS.Path = "/exports/backups\nbackup_driver = cinder.backup.drivers.swift"
			},
			wantSub: "must contain only printable ASCII",
		},
		{
			// oslo.config strips what surrounds backup_share before cinder derives the
			// export's mount directory from it, so a trailing space would leave the
			// export mounted where the backup driver does not look.
			name: "nfs path with a trailing space rejected",
			mutate: func(o *CinderBackupBackend) {
				o.Spec.NFS.Path = "/exports/backups "
			},
			wantSub: "must contain only printable ASCII",
		},
		{
			// oslo.config strips the whole Unicode whitespace set, not the five ASCII
			// characters RE2 resolves \s to, so a pasted U+00A0 is the same divergence
			// as the trailing space above and no whitespace blacklist would catch it.
			name: "nfs path with a trailing non-breaking space rejected",
			mutate: func(o *CinderBackupBackend) {
				o.Spec.NFS.Path = "/exports/backups\u00a0"
			},
			wantSub: "must contain only printable ASCII",
		},
		{
			name: "nfs mountOptions with a newline rejected",
			mutate: func(o *CinderBackupBackend) {
				o.Spec.NFS.MountOptions = "nfsvers=4.1\nhost = elsewhere"
			},
			wantSub: "must not contain a newline or carriage return",
		},
		{
			name: "denylisted typed-field option rejected",
			mutate: func(o *CinderBackupBackend) {
				o.Spec.ExtraOptions = map[string]string{"backup_file_size": "1048576"}
			},
			wantSub: `option "backup_file_size" is owned by spec.fileSize`,
		},
		{
			name: "denylisted host option rejected",
			mutate: func(o *CinderBackupBackend) {
				o.Spec.ExtraOptions = map[string]string{"host": "elsewhere"}
			},
			wantSub: `option "host" is owned by the operator`,
		},
		{
			name: "bad key charset rejected",
			mutate: func(o *CinderBackupBackend) {
				o.Spec.ExtraOptions = map[string]string{"bad key": "x"}
			},
			wantSub: "option name must match",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			w := &CinderBackupBackendWebhook{}
			obj := validCinderBackupBackend()
			tc.mutate(obj)

			_, err := w.ValidateCreate(context.Background(), obj)
			g.Expect(err).To(gomega.HaveOccurred())
			g.Expect(err.Error()).To(gomega.ContainSubstring(tc.wantSub))
		})
	}
}

func TestCinderBackupBackendDefault_ClusterName(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &CinderBackupBackendWebhook{}

	// Empty: filled with the name librados assumes.
	empty := validRBDCinderBackupBackend()
	empty.Spec.RBD.ClusterName = ""
	g.Expect(w.Default(context.Background(), empty)).To(gomega.Succeed())
	g.Expect(empty.Spec.RBD.ClusterName).To(gomega.Equal("ceph"))

	// Explicit value preserved.
	explicit := validRBDCinderBackupBackend()
	explicit.Spec.RBD.ClusterName = "backup-site"
	g.Expect(w.Default(context.Background(), explicit)).To(gomega.Succeed())
	g.Expect(explicit.Spec.RBD.ClusterName).To(gomega.Equal("backup-site"))

	// An NFS target has no rbd block to fill.
	nfs := validCinderBackupBackend()
	g.Expect(w.Default(context.Background(), nfs)).To(gomega.Succeed())
	g.Expect(nfs.Spec.RBD).To(gomega.BeNil())
}

func TestCinderBackupBackendValidate_AcceptsValidRBDBackend(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &CinderBackupBackendWebhook{}

	b := validRBDCinderBackupBackend()
	b.Spec.RBD.Monitors = []string{"10.96.12.3:6789", "ceph-mon-b.rook-ceph.svc", "ceph-mon-c:65535"}
	b.Spec.RBD.Networks = []string{"10.244.0.0/16", "10.96.0.0/12"}
	b.Spec.ExtraOptions = map[string]string{"backup_ceph_chunk_size": "67108864"}
	_, err := w.ValidateCreate(context.Background(), b)
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

// Both halves of the union, each reported on spec.rbd: type RBD without the
// block, and type NFS carrying both blocks.
func TestCinderBackupBackendValidate_RejectsRBDUnionMismatch(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &CinderBackupBackendWebhook{}

	missingRBD := validRBDCinderBackupBackend()
	missingRBD.Spec.RBD = nil
	_, err := w.ValidateCreate(context.Background(), missingRBD)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("spec.rbd"))
	g.Expect(err.Error()).To(gomega.ContainSubstring(
		"exactly one backup backend block matching spec.type must be set (type NFS requires spec.nfs, type RBD requires spec.rbd)"))

	both := validCinderBackupBackend()
	both.Spec.RBD = validRBDCinderBackupBackend().Spec.RBD
	_, err = w.ValidateCreate(context.Background(), both)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("spec.rbd"))
	g.Expect(err.Error()).NotTo(gomega.ContainSubstring("spec.nfs:"),
		"the nfs block matches type NFS, so only the rbd half fires")
	g.Expect(err.Error()).To(gomega.ContainSubstring("exactly one backup backend block matching spec.type"))
}

// Each RBD field reaches a file, a command line or an ipBlock verbatim. Every
// shape below breaks exactly one rule, so the helper must return exactly one
// error, on the path of the field that carries it.
func TestCinderBackupBackendValidate_RejectsRBDFieldShapes(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(rbd *RBDBackupBackendSpec)
		wantPath string
		wantSub  string
	}{
		{
			name:     "user with the client. prefix",
			mutate:   func(rbd *RBDBackupBackendSpec) { rbd.User = "client.cinder-backup" },
			wantPath: "spec.rbd.user",
			wantSub:  "(write cinder-backup, not client.cinder-backup)",
		},
		{
			name:     "monitor port out of range",
			mutate:   func(rbd *RBDBackupBackendSpec) { rbd.Monitors = []string{"ceph-mon:70000"} },
			wantPath: "spec.rbd.monitors[0]",
			wantSub:  "port must be between 1 and 65535",
		},
		{
			name: "monitor with a newline",
			mutate: func(rbd *RBDBackupBackendSpec) {
				rbd.Monitors = []string{"ceph-mon", "ceph-mon\nkeyring = /tmp/other"}
			},
			wantPath: "spec.rbd.monitors[1]",
			wantSub:  "must not contain a newline or carriage return",
		},
		{
			name:     "network with host bits set",
			mutate:   func(rbd *RBDBackupBackendSpec) { rbd.Networks = []string{"10.128.0.1/22"} },
			wantPath: "spec.rbd.networks[0]",
			wantSub:  "must be a canonical IPv4 CIDR",
		},
		{
			name:     "network that is no CIDR",
			mutate:   func(rbd *RBDBackupBackendSpec) { rbd.Networks = []string{"10.244.0.0/16", "not-a-cidr"} },
			wantPath: "spec.rbd.networks[1]",
			wantSub:  "must be a canonical IPv4 CIDR",
		},
		{
			name:     "key Secret name that is no DNS-1123 subdomain",
			mutate:   func(rbd *RBDBackupBackendSpec) { rbd.KeySecretRef.Name = "Ceph_Key" },
			wantPath: "spec.rbd.keySecretRef.name",
			wantSub:  "RFC 1123 subdomain",
		},
		{
			name:     "pool with a carriage return",
			mutate:   func(rbd *RBDBackupBackendSpec) { rbd.Pool = "backups\r" },
			wantPath: "spec.rbd.pool",
			wantSub:  "must not contain a newline or carriage return",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			b := validRBDCinderBackupBackend()
			tc.mutate(b.Spec.RBD)
			errs := validateRBDBackupBackend(field.NewPath("spec", "rbd"), b.Spec.RBD)
			g.Expect(errs).To(gomega.HaveLen(1))
			g.Expect(errs[0].Type).To(gomega.Equal(field.ErrorTypeInvalid))
			g.Expect(errs[0].Field).To(gomega.Equal(tc.wantPath))
			g.Expect(errs[0].Detail).To(gomega.ContainSubstring(tc.wantSub))

			// The same rule answers through the admission entry point.
			_, err := (&CinderBackupBackendWebhook{}).ValidateCreate(context.Background(), b)
			g.Expect(err).To(gomega.HaveOccurred())
			g.Expect(err.Error()).To(gomega.ContainSubstring(tc.wantPath))
		})
	}
}

// The denylist is selected by spec.type: the options the shared fields render
// are denied everywhere, each driver's own options only on its own type.
func TestCinderBackupBackendValidate_ExtraOptionsDenylistPerType(t *testing.T) {
	tests := []struct {
		name    string
		backend func() *CinderBackupBackend
		option  string
		wantSub string
	}{
		{
			name:    "backup_ceph_pool on RBD",
			backend: validRBDCinderBackupBackend,
			option:  "backup_ceph_pool",
			wantSub: `option "backup_ceph_pool" is owned by spec.rbd.pool`,
		},
		{
			name:    "backup_ceph_conf on RBD",
			backend: validRBDCinderBackupBackend,
			option:  "backup_ceph_conf",
			wantSub: `option "backup_ceph_conf" is owned by the operator (the ceph.conf it projects into /etc/ceph)`,
		},
		{
			name:    "backup_share on NFS",
			backend: validCinderBackupBackend,
			option:  "backup_share",
			wantSub: `option "backup_share" is owned by spec.nfs.server and spec.nfs.path`,
		},
		{
			name:    "backup_share on RBD",
			backend: validRBDCinderBackupBackend,
			option:  "backup_share",
		},
		{
			name:    "backup_driver on NFS",
			backend: validCinderBackupBackend,
			option:  "backup_driver",
			wantSub: `option "backup_driver" is owned by spec.type`,
		},
		{
			name:    "backup_driver on RBD",
			backend: validRBDCinderBackupBackend,
			option:  "backup_driver",
			wantSub: `option "backup_driver" is owned by spec.type`,
		},
		{
			name:    "host on NFS",
			backend: validCinderBackupBackend,
			option:  "host",
			wantSub: `option "host" is owned by the operator`,
		},
		{
			name:    "host on RBD",
			backend: validRBDCinderBackupBackend,
			option:  "host",
			wantSub: `option "host" is owned by the operator`,
		},
		{
			name:    "backup_ceph_chunk_size on RBD",
			backend: validRBDCinderBackupBackend,
			option:  "backup_ceph_chunk_size",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			w := &CinderBackupBackendWebhook{}

			b := tc.backend()
			b.Spec.ExtraOptions = map[string]string{tc.option: "5"}
			_, err := w.ValidateCreate(context.Background(), b)
			if tc.wantSub == "" {
				g.Expect(err).NotTo(gomega.HaveOccurred())
				return
			}
			g.Expect(err).To(gomega.HaveOccurred())
			g.Expect(err.Error()).To(gomega.ContainSubstring(tc.wantSub))
		})
	}

	t.Run("an unknown type gets the shared options alone", func(t *testing.T) {
		g := gomega.NewWithT(t)
		g.Expect(BackupExtraOptionsDenylist(CinderBackupBackendType("Swift"))).
			To(gomega.Equal(sharedBackupExtraOptionsDenylist))
	})

	t.Run("the result is a copy", func(t *testing.T) {
		g := gomega.NewWithT(t)
		denylist := BackupExtraOptionsDenylist(CinderBackupBackendTypeRBD)
		delete(denylist, "backup_driver")
		g.Expect(sharedBackupExtraOptionsDenylist).To(gomega.HaveKey("backup_driver"))
	})
}

// The backup driver is a property of the single cinder-backup Deployment, not one
// of several backends it serves, so a second attachment to the same Cinder
// describes a Deployment that cannot exist.
func TestCinderBackupBackendValidate_SingleAttachment(t *testing.T) {
	g := gomega.NewWithT(t)

	existing := validCinderBackupBackend()
	existing.Name = "existing-backup"

	c := fake.NewClientBuilder().WithScheme(cinderScheme(t)).WithObjects(existing).Build()
	w := &CinderBackupBackendWebhook{Client: c}

	second := validCinderBackupBackend()
	second.Name = "second-backup"
	_, err := w.ValidateCreate(context.Background(), second)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring(`already has CinderBackupBackend "existing-backup" attached`))

	// An attachment to a DIFFERENT Cinder is unaffected.
	other := validCinderBackupBackend()
	other.Name = "other-backup"
	other.Spec.CinderRef.Name = "cinder-other"
	_, err = w.ValidateCreate(context.Background(), other)
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

// On UPDATE the object under validation appears in the sibling List and must not
// collide with itself.
func TestCinderBackupBackendValidate_SingleAttachmentSkipsSelfOnUpdate(t *testing.T) {
	g := gomega.NewWithT(t)

	self := validCinderBackupBackend()
	c := fake.NewClientBuilder().WithScheme(cinderScheme(t)).WithObjects(self).Build()
	w := &CinderBackupBackendWebhook{Client: c}

	updated := validCinderBackupBackend()
	updated.Spec.Compression = "zstd"
	_, err := w.ValidateUpdate(context.Background(), self, updated)
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

// Without a reader the List cannot be performed, and failing closed would reject
// every backup backend a programmatically constructed webhook sees.
func TestCinderBackupBackendValidate_SingleAttachmentSkippedWithoutReader(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &CinderBackupBackendWebhook{}

	_, err := w.ValidateCreate(context.Background(), validCinderBackupBackend())
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

// A List failure must surface as an admission error rather than silently
// admitting a possibly-conflicting second attachment.
func TestCinderBackupBackendValidate_SingleAttachmentListErrorSurfaced(t *testing.T) {
	g := gomega.NewWithT(t)

	c := fake.NewClientBuilder().WithScheme(cinderScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
				return errors.New("boom")
			},
		}).Build()
	w := &CinderBackupBackendWebhook{Client: c}

	_, err := w.ValidateCreate(context.Background(), validCinderBackupBackend())
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("listing CinderBackupBackends"))
}

func TestCinderBackupBackendValidateDelete_AlwaysAccepts(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &CinderBackupBackendWebhook{}

	warnings, err := w.ValidateDelete(context.Background(), validCinderBackupBackend())
	g.Expect(warnings).To(gomega.BeNil())
	g.Expect(err).NotTo(gomega.HaveOccurred())
}
