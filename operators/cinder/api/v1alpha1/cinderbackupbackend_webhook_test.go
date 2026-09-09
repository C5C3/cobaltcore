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
