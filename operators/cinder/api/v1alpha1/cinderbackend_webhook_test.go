// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"context"
	"strings"
	"testing"

	"github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/c5c3/cobaltcore/internal/common/job"
)

// validCinderBackend returns a minimal valid NFS-typed CinderBackend the per-rule
// tests mutate one field of, so every rejection is attributable to exactly one
// rule.
func validCinderBackend() *CinderBackend {
	return &CinderBackend{
		ObjectMeta: metav1.ObjectMeta{Name: "nfs1", Namespace: "openstack"},
		Spec: CinderBackendSpec{
			CinderRef: CinderRefSpec{Name: "cinder"},
			Type:      CinderBackendTypeNFS,
			NFS: &NFSBackendSpec{
				Server:       "nfs.example.com",
				Path:         "/exports/volumes",
				MountOptions: DefaultNFSMountOptions,
			},
		},
	}
}

// validRBDCinderBackend returns a minimal valid RBD-typed CinderBackend, the RBD
// counterpart of validCinderBackend.
func validRBDCinderBackend() *CinderBackend {
	return &CinderBackend{
		ObjectMeta: metav1.ObjectMeta{Name: "rbd1", Namespace: "openstack"},
		Spec: CinderBackendSpec{
			CinderRef: CinderRefSpec{Name: "cinder"},
			Type:      CinderBackendTypeRBD,
			RBD: &RBDBackendSpec{
				Pool:         "volumes",
				User:         "cinder",
				Monitors:     []string{"ceph-mon.openstack.svc.cluster.local"},
				Networks:     []string{"10.244.0.0/16"},
				ClusterName:  DefaultRBDClusterName,
				KeySecretRef: SecretNameRefSpec{Name: "ceph-client-cinder"},
			},
		},
	}
}

func cinderScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := AddToScheme(s); err != nil {
		t.Fatalf("adding cinder scheme: %v", err)
	}
	return s
}

func TestCinderBackendDefault_MountOptions(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &CinderBackendWebhook{}

	// Empty: filled with the soft NFSv4.1 mount.
	empty := validCinderBackend()
	empty.Spec.NFS.MountOptions = ""
	g.Expect(w.Default(context.Background(), empty)).To(gomega.Succeed())
	g.Expect(empty.Spec.NFS.MountOptions).To(gomega.Equal(DefaultNFSMountOptions))

	// Explicit value preserved.
	explicit := validCinderBackend()
	explicit.Spec.NFS.MountOptions = "nfsvers=3,soft"
	g.Expect(w.Default(context.Background(), explicit)).To(gomega.Succeed())
	g.Expect(explicit.Spec.NFS.MountOptions).To(gomega.Equal("nfsvers=3,soft"))

	// A CR without the nfs block has nothing to fill.
	absent := validCinderBackend()
	absent.Spec.NFS = nil
	g.Expect(w.Default(context.Background(), absent)).To(gomega.Succeed())
	g.Expect(absent.Spec.NFS).To(gomega.BeNil())
}

func TestCinderBackendValidate_AcceptsValidBackend(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &CinderBackendWebhook{}

	_, err := w.ValidateCreate(context.Background(), validCinderBackend())
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

func TestCinderBackendDefault_ClusterName(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &CinderBackendWebhook{}

	// Empty: filled with the name librados assumes.
	empty := validRBDCinderBackend()
	empty.Spec.RBD.ClusterName = ""
	g.Expect(w.Default(context.Background(), empty)).To(gomega.Succeed())
	g.Expect(empty.Spec.RBD.ClusterName).To(gomega.Equal("ceph"))

	// Explicit value preserved.
	explicit := validRBDCinderBackend()
	explicit.Spec.RBD.ClusterName = "backup-site"
	g.Expect(w.Default(context.Background(), explicit)).To(gomega.Succeed())
	g.Expect(explicit.Spec.RBD.ClusterName).To(gomega.Equal("backup-site"))

	// A CR without the rbd block has nothing to fill.
	absent := validRBDCinderBackend()
	absent.Spec.RBD = nil
	g.Expect(w.Default(context.Background(), absent)).To(gomega.Succeed())
	g.Expect(absent.Spec.RBD).To(gomega.BeNil())
}

func TestCinderBackendValidate_AcceptsValidRBDBackend(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &CinderBackendWebhook{}

	b := validRBDCinderBackend()
	b.Spec.RBD.Monitors = []string{"10.96.12.3:6789", "ceph-mon-b.rook-ceph.svc", "10.96.12.5:3300", "ceph-mon-c:65535"}
	b.Spec.RBD.Networks = []string{"10.244.0.0/16", "10.96.0.0/12"}
	b.Spec.RBD.SecretUUID = "457eb676-33da-42ec-9a8c-9293d545c337"
	_, err := w.ValidateCreate(context.Background(), b)
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

// Both halves of the union, each reported on spec.rbd: type RBD without the
// block, and type NFS carrying both blocks.
func TestCinderBackendValidate_RejectsRBDUnionMismatch(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &CinderBackendWebhook{}

	missingRBD := validRBDCinderBackend()
	missingRBD.Spec.RBD = nil
	_, err := w.ValidateCreate(context.Background(), missingRBD)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("spec.rbd"))
	g.Expect(err.Error()).To(gomega.ContainSubstring(
		"exactly one backend block matching spec.type must be set (type NFS requires spec.nfs, type RBD requires spec.rbd)"))

	both := validCinderBackend()
	both.Spec.RBD = validRBDCinderBackend().Spec.RBD
	_, err = w.ValidateCreate(context.Background(), both)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("spec.rbd"))
	g.Expect(err.Error()).To(gomega.ContainSubstring("exactly one backend block matching spec.type"))
}

// Each RBD field reaches a file, a command line or an ipBlock verbatim. Every
// shape below breaks exactly one rule, so the helper must return exactly one
// error, on the path of the field that carries it.
func TestCinderBackendValidate_RejectsRBDFieldShapes(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(rbd *RBDBackendSpec)
		wantPath string
		wantSub  string
	}{
		{
			name:     "user with the client. prefix",
			mutate:   func(rbd *RBDBackendSpec) { rbd.User = "client.cinder" },
			wantPath: "spec.rbd.user",
			wantSub:  "without its client. prefix",
		},
		{
			name:     "monitor port out of range",
			mutate:   func(rbd *RBDBackendSpec) { rbd.Monitors = []string{"ceph-mon:70000"} },
			wantPath: "spec.rbd.monitors[0]",
			wantSub:  "port must be between 1 and 65535",
		},
		{
			name:     "monitor port zero",
			mutate:   func(rbd *RBDBackendSpec) { rbd.Monitors = []string{"ceph-mon:0"} },
			wantPath: "spec.rbd.monitors[0]",
			wantSub:  "port must be between 1 and 65535",
		},
		{
			// The schema pattern refuses an empty port; a CR past it reaches the
			// twin.
			name:     "monitor with an empty port",
			mutate:   func(rbd *RBDBackendSpec) { rbd.Monitors = []string{"ceph-mon:"} },
			wantPath: "spec.rbd.monitors[0]",
			wantSub:  "port must be between 1 and 65535",
		},
		{
			name: "monitor with a newline",
			mutate: func(rbd *RBDBackendSpec) {
				rbd.Monitors = []string{"ceph-mon", "ceph-mon\nkeyring = /tmp/other"}
			},
			wantPath: "spec.rbd.monitors[1]",
			wantSub:  "must not contain a newline or carriage return",
		},
		{
			name:     "network with host bits set",
			mutate:   func(rbd *RBDBackendSpec) { rbd.Networks = []string{"10.128.0.1/22"} },
			wantPath: "spec.rbd.networks[0]",
			wantSub:  "must be a canonical IPv4 CIDR",
		},
		{
			name:     "network that is no CIDR",
			mutate:   func(rbd *RBDBackendSpec) { rbd.Networks = []string{"10.244.0.0/16", "not-a-cidr"} },
			wantPath: "spec.rbd.networks[1]",
			wantSub:  "must be a canonical IPv4 CIDR",
		},
		{
			// The pattern admits IPv4 only; the twin must too.
			name:     "IPv6 network",
			mutate:   func(rbd *RBDBackendSpec) { rbd.Networks = []string{"fd00::/8"} },
			wantPath: "spec.rbd.networks[0]",
			wantSub:  "must be a canonical IPv4 CIDR",
		},
		{
			name:     "key Secret name that is no DNS-1123 subdomain",
			mutate:   func(rbd *RBDBackendSpec) { rbd.KeySecretRef.Name = "Ceph_Key" },
			wantPath: "spec.rbd.keySecretRef.name",
			wantSub:  "RFC 1123 subdomain",
		},
		{
			name:     "pool with a carriage return",
			mutate:   func(rbd *RBDBackendSpec) { rbd.Pool = "volumes\r" },
			wantPath: "spec.rbd.pool",
			wantSub:  "must not contain a newline or carriage return",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			b := validRBDCinderBackend()
			tc.mutate(b.Spec.RBD)
			errs := validateRBDBackend(field.NewPath("spec", "rbd"), b.Spec.RBD)
			g.Expect(errs).To(gomega.HaveLen(1))
			g.Expect(errs[0].Type).To(gomega.Equal(field.ErrorTypeInvalid))
			g.Expect(errs[0].Field).To(gomega.Equal(tc.wantPath))
			g.Expect(errs[0].Detail).To(gomega.ContainSubstring(tc.wantSub))

			// The same rule answers through the admission entry point.
			_, err := (&CinderBackendWebhook{}).ValidateCreate(context.Background(), b)
			g.Expect(err).To(gomega.HaveOccurred())
			g.Expect(err.Error()).To(gomega.ContainSubstring(tc.wantPath))
		})
	}
}

// The denylist is selected by spec.type: the options the shared fields render
// are denied everywhere, each driver's own options only on its own type.
func TestCinderBackendValidate_ExtraOptionsDenylistPerType(t *testing.T) {
	tests := []struct {
		name    string
		backend func() *CinderBackend
		option  string
		wantSub string
	}{
		{
			name:    "rbd_pool on RBD",
			backend: validRBDCinderBackend,
			option:  "rbd_pool",
			wantSub: `option "rbd_pool" is owned by spec.rbd.pool`,
		},
		{
			name:    "rbd_keyring_conf on RBD",
			backend: validRBDCinderBackend,
			option:  "rbd_keyring_conf",
			wantSub: `option "rbd_keyring_conf" is owned by the operator (the keyring is projected into /etc/ceph`,
		},
		{
			name:    "nfs_mount_options on NFS",
			backend: validCinderBackend,
			option:  "nfs_mount_options",
			wantSub: `option "nfs_mount_options" is owned by spec.nfs.mountOptions`,
		},
		{
			name:    "nfs_mount_options on RBD",
			backend: validRBDCinderBackend,
			option:  "nfs_mount_options",
		},
		{
			name:    "volume_driver on NFS",
			backend: validCinderBackend,
			option:  "volume_driver",
			wantSub: `option "volume_driver" is owned by spec.type`,
		},
		{
			name:    "volume_driver on RBD",
			backend: validRBDCinderBackend,
			option:  "volume_driver",
			wantSub: `option "volume_driver" is owned by spec.type`,
		},
		{
			name:    "rados_connect_timeout on RBD",
			backend: validRBDCinderBackend,
			option:  "rados_connect_timeout",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			w := &CinderBackendWebhook{}

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
		g.Expect(ExtraOptionsDenylist(CinderBackendType("Other"))).To(gomega.Equal(sharedExtraOptionsDenylist))
	})

	t.Run("the result is a copy", func(t *testing.T) {
		g := gomega.NewWithT(t)
		denylist := ExtraOptionsDenylist(CinderBackendTypeRBD)
		delete(denylist, "volume_driver")
		g.Expect(sharedExtraOptionsDenylist).To(gomega.HaveKey("volume_driver"))
	})
}

// Both directions of the type/nfs union: type NFS without spec.nfs, and spec.nfs
// present alongside a non-NFS type value (unrepresentable via the enum, so a
// bogus value is used to exercise the XOR).
func TestCinderBackendValidate_RejectsUnionMismatch(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &CinderBackendWebhook{}

	missingNFS := validCinderBackend()
	missingNFS.Spec.NFS = nil
	_, err := w.ValidateCreate(context.Background(), missingNFS)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("exactly one backend block matching spec.type"))

	wrongType := validCinderBackend()
	wrongType.Spec.Type = CinderBackendType("Ceph")
	_, err = w.ValidateCreate(context.Background(), wrongType)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("exactly one backend block matching spec.type"))
}

// The name becomes the backend's [<name>] section, the volume_backend_name volume
// types schedule against, and the host identity its volumes are keyed by, so each
// rejected shape breaks one of the three.
func TestCinderBackendValidate_RejectsUnusableNames(t *testing.T) {
	tests := []struct {
		name    string
		wantSub string
	}{
		{name: "default", wantSub: `name must not be "default"`},
		{name: "DEFAULT", wantSub: `name must not be "default"`},
		{name: "database", wantSub: "collides with the [database] section"},
		{name: "keystone_authtoken", wantSub: "collides with the [keystone_authtoken] section"},
		{name: "backend_defaults", wantSub: "collides with the [backend_defaults] section"},
		{name: "nfs@site", wantSub: `name must not contain "@" or "#"`},
		{name: "nfs#pool", wantSub: `name must not contain "@" or "#"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			w := &CinderBackendWebhook{}

			b := validCinderBackend()
			b.Name = tc.name
			_, err := w.ValidateCreate(context.Background(), b)
			g.Expect(err).To(gomega.HaveOccurred())
			g.Expect(err.Error()).To(gomega.ContainSubstring(tc.wantSub))
		})
	}
}

// The service-remove Job's name is copied into a label value, so the two names
// share one 47-character budget. It is checked on create only: both names are
// immutable, so on update the rule could only fire against a CR an earlier
// operator already admitted — including the finalizer-removal update that
// completes its deletion.
func TestCinderBackendValidate_CombinedNameBudget(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &CinderBackendWebhook{}

	atLimit := validCinderBackend()
	atLimit.Spec.CinderRef.Name = strings.Repeat("c", MaxBackendNamePlusCinderRef-MaxBackendNameLength)
	atLimit.Name = strings.Repeat("b", MaxBackendNameLength)
	_, err := w.ValidateCreate(context.Background(), atLimit)
	g.Expect(err).NotTo(gomega.HaveOccurred(), "a pair that still fits the label value must be accepted")

	// The overrun is put on the Cinder name, so metadata.name stays inside its
	// own bound and the shared budget is the only rule broken.
	tooLong := atLimit.DeepCopy()
	tooLong.Spec.CinderRef.Name += "c"
	_, err = w.ValidateCreate(context.Background(), tooLong)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("metadata.name plus spec.cinderRef.name must not exceed 47 characters"))

	_, err = w.ValidateUpdate(context.Background(), tooLong, tooLong.DeepCopy())
	g.Expect(err).NotTo(gomega.HaveOccurred(),
		"an over-long grandfathered pair must stay updatable, or its deletion never completes")
}

// The detach also records the Job's terminal state under a per-backend
// annotation key on the Cinder, and Kubernetes caps an annotation key's name
// part at 63 characters. That bounds metadata.name alone, which the shared
// 47-character budget does not: a one-character Cinder name leaves the backend
// 46 there and still overruns the key.
func TestCinderBackendValidate_NameBoundedByJobUIDAnnotation(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &CinderBackendWebhook{}

	atLimit := validCinderBackend()
	atLimit.Spec.CinderRef.Name = "c"
	atLimit.Name = strings.Repeat("b", MaxBackendNameLength)
	_, err := w.ValidateCreate(context.Background(), atLimit)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(len(job.JobUIDAnnotationKey("service-remove-"+atLimit.Name))).
		To(gomega.Equal(len("cobaltcore.c5c3.io/")+63),
			"the longest admitted name must still fit the annotation key's name part")

	tooLong := atLimit.DeepCopy()
	tooLong.Name += "b"
	_, err = w.ValidateCreate(context.Background(), tooLong)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("metadata.name must not exceed 35 characters"))

	_, err = w.ValidateUpdate(context.Background(), tooLong, tooLong.DeepCopy())
	g.Expect(err).NotTo(gomega.HaveOccurred(),
		"an over-long grandfathered name must stay updatable, or its deletion never completes")
}

// The export fields are written verbatim into the rendered section and into the
// nfs_shares_config file beside it, and the shares file is outside what the
// reconcile-time guard inspects — so a newline in the path would put a second
// export in front of the driver, and a space or a tab a shorter one: the driver
// cuts a shares line at the first space to split the export from its options, so
// it would mount something other than what the operator mounted for it. A
// non-ASCII space is the same divergence with none of the visibility: the driver
// strips the full Unicode whitespace set off the line it reads back, so it
// resolves the export without one where the operator mounted it with one. The CRD
// patterns answer first in production; this is the webhook twin, for an object
// that reached it past the schema.
func TestCinderBackendValidate_RejectsExportControlChars(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(o *CinderBackend)
		wantSub string
	}{
		{
			name: "path with a newline",
			mutate: func(o *CinderBackend) {
				o.Spec.NFS.Path = "/exports/volumes\nnfs.example.com:/exports/other"
			},
			wantSub: "must contain only printable ASCII",
		},
		{
			name: "path with a space",
			mutate: func(o *CinderBackend) {
				o.Spec.NFS.Path = "/exports/volumes -o vers=3"
			},
			wantSub: "must contain only printable ASCII",
		},
		{
			name: "path with a trailing tab",
			mutate: func(o *CinderBackend) {
				o.Spec.NFS.Path = "/exports/volumes\t"
			},
			wantSub: "must contain only printable ASCII",
		},
		{
			// The character an operator pastes in by accident, out of a wiki page or a
			// PDF runbook. It is not in RE2's \s, so a whitespace blacklist would let
			// it through, and the driver strips it off the shares line before resolving
			// the export.
			name: "path with a trailing non-breaking space",
			mutate: func(o *CinderBackend) {
				o.Spec.NFS.Path = "/exports/volumes\u00a0"
			},
			wantSub: "must contain only printable ASCII",
		},
		{
			name: "mountOptions with a newline",
			mutate: func(o *CinderBackend) {
				o.Spec.NFS.MountOptions = "nfsvers=4.1\nbackend_host = elsewhere"
			},
			wantSub: "must not contain a newline or carriage return",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			w := &CinderBackendWebhook{}

			b := validCinderBackend()
			tc.mutate(b)
			_, err := w.ValidateCreate(context.Background(), b)
			g.Expect(err).To(gomega.HaveOccurred())
			g.Expect(err.Error()).To(gomega.ContainSubstring(tc.wantSub))
		})
	}
}

func TestCinderBackendValidate_ExtraOptions(t *testing.T) {
	tests := []struct {
		name    string
		options map[string]string
		wantSub string
	}{
		{
			name:    "denylisted typed-field option rejected",
			options: map[string]string{"nfs_mount_options": "nfsvers=3"},
			wantSub: `option "nfs_mount_options" is owned by spec.nfs.mountOptions`,
		},
		{
			name:    "denylisted operator-owned option rejected",
			options: map[string]string{"backend_host": "elsewhere"},
			wantSub: `option "backend_host" is owned by the operator`,
		},
		{
			name:    "denylisted cache option rejected",
			options: map[string]string{"image_volume_cache_enabled": "false"},
			wantSub: `option "image_volume_cache_enabled" is owned by spec.imageVolumeCache.enabled`,
		},
		{
			name:    "bad key charset rejected",
			options: map[string]string{"bad key": "x"},
			wantSub: "option name must match",
		},
		{
			// A newline in the key would inject a line through the renderer's
			// verbatim "key = value" write, and the charset rule runs before the
			// denylist for exactly that reason.
			name:    "newline in key rejected",
			options: map[string]string{"foo\nnfs_mount_options = hard": "x"},
			wantSub: "option name must match",
		},
		{
			name:    "control-char value rejected",
			options: map[string]string{"nfs_oversub_ratio": "1.0\nnfs_mount_options = hard"},
			wantSub: "must not contain newline or carriage-return",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			w := &CinderBackendWebhook{}

			b := validCinderBackend()
			b.Spec.ExtraOptions = tc.options
			_, err := w.ValidateCreate(context.Background(), b)
			g.Expect(err).To(gomega.HaveOccurred())
			g.Expect(err.Error()).To(gomega.ContainSubstring(tc.wantSub))
		})
	}
}

func TestCinderBackendValidate_ExtraOptionsAllowsBenignOption(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &CinderBackendWebhook{}

	b := validCinderBackend()
	b.Spec.ExtraOptions = map[string]string{"nfs_oversub_ratio": "1.0"}
	_, err := w.ValidateCreate(context.Background(), b)
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

// The cached image volumes belong to the deployment rather than to the requesting
// tenant, so the cache needs the Cinder's spec.internalTenant. Every lookup that
// cannot answer stays silent: the two CRs are applied independently, and a
// warning claiming a misconfiguration must not be the price of GitOps ordering.
func TestCinderBackendValidate_ImageVolumeCacheWarning(t *testing.T) {
	cinderWithout := &Cinder{ObjectMeta: metav1.ObjectMeta{Name: "cinder", Namespace: "openstack"}}
	cinderWith := &Cinder{
		ObjectMeta: metav1.ObjectMeta{Name: "cinder", Namespace: "openstack"},
		Spec: CinderSpec{
			InternalTenant: &InternalTenantSpec{ProjectID: "8f2b", UserID: "3a17"},
		},
	}

	tests := []struct {
		name      string
		cinder    *Cinder
		noClient  bool
		cacheOff  bool
		wantWarns bool
	}{
		{
			name:      "cinder without internalTenant warns",
			cinder:    cinderWithout,
			wantWarns: true,
		},
		{
			name:   "cinder with internalTenant is silent",
			cinder: cinderWith,
		},
		{
			name: "missing cinder is silent",
		},
		{
			name:     "no reader is silent",
			cinder:   cinderWithout,
			noClient: true,
		},
		{
			name:     "disabled cache is silent",
			cinder:   cinderWithout,
			cacheOff: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			w := &CinderBackendWebhook{}
			if !tc.noClient {
				builder := fake.NewClientBuilder().WithScheme(cinderScheme(t))
				if tc.cinder != nil {
					builder = builder.WithObjects(tc.cinder.DeepCopy())
				}
				w.Client = builder.Build()
			}

			b := validCinderBackend()
			b.Spec.ImageVolumeCache = &ImageVolumeCacheSpec{Enabled: !tc.cacheOff}

			warnings, err := w.ValidateCreate(context.Background(), b)
			g.Expect(err).NotTo(gomega.HaveOccurred(), "the cache warning must never reject a backend")
			if tc.wantWarns {
				g.Expect(warnings).To(gomega.ConsistOf(gomega.ContainSubstring("sets no spec.internalTenant")))
				return
			}
			g.Expect(warnings).To(gomega.BeEmpty())
		})
	}
}

func TestCinderBackendValidateDelete_AlwaysAccepts(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &CinderBackendWebhook{}

	warnings, err := w.ValidateDelete(context.Background(), validCinderBackend())
	g.Expect(warnings).To(gomega.BeNil())
	g.Expect(err).NotTo(gomega.HaveOccurred())
}
