// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package v1alpha1

import (
	"context"
	"fmt"
	"testing"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	"github.com/c5c3/cobaltcore/operators/cinder/internal/testutil"
)

// --- Helpers ---

// setupEnvTest wraps testutil.SetupCinderEnvTest with the v1alpha1 scheme
// registration and all three webhook setups, avoiding the import cycle between
// testutil and this package. The webhook manifests envtest installs carry all
// three kinds (failurePolicy=Fail), so every handler must be served or admission
// of the unserved kind fails.
func setupEnvTest(t testing.TB) (client.Client, context.Context, context.CancelFunc) {
	t.Helper()
	return testutil.SetupCinderEnvTest(t, AddToScheme, func(mgr ctrl.Manager) error {
		// mgr.GetAPIReader() mirrors production wiring in main.go: webhook
		// admission lookups (PriorityClass existence, the cinderRef resolution, the
		// sibling List) read the API server directly, never a stale informer cache.
		if err := (&CinderWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr); err != nil {
			return err
		}
		if err := (&CinderBackendWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr); err != nil {
			return err
		}
		return (&CinderBackupBackendWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr)
	})
}

// setupEnvTestNoWebhook wraps testutil.SetupCinderEnvTestNoWebhook with the
// v1alpha1 scheme registration. Used by the CRD-only tests so no webhook can mask
// a missing CEL rule or supply a default the CRD schema must supply itself.
func setupEnvTestNoWebhook(t testing.TB) (client.Client, context.Context, context.CancelFunc) {
	t.Helper()
	return testutil.SetupCinderEnvTestNoWebhook(t, AddToScheme)
}

// newNamespace creates a uniquely named namespace for a test.
func newNamespace(t testing.TB, ctx context.Context, c client.Client, prefix string) string {
	t.Helper()
	g := NewGomegaWithT(t)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: prefix}}
	g.Expect(c.Create(ctx, ns)).To(Succeed(), "create namespace")
	return ns.Name
}

// integrationCinder returns validCinder() stamped with name/namespace for API
// server submission. validCinder() is a webhook-unit helper whose validators do
// not enforce the CRD required-field markers, so it omits spec.database.database
// and spec.database.secretRef.name; those are filled here so the object clears
// the real CRD schema (both are required even in managed/clusterRef mode).
func integrationCinder(name, namespace string) *Cinder {
	cinder := validCinder()
	cinder.Name = name
	cinder.Namespace = namespace
	cinder.Spec.Database.Database = "cinder"
	cinder.Spec.Database.SecretRef = commonv1.SecretRefSpec{Name: "cinder-db"}
	return cinder
}

// integrationBackend returns validCinderBackend() stamped with name/namespace and
// pointed at cinderRef, for API server submission.
func integrationBackend(name, namespace, cinderRef string) *CinderBackend {
	b := validCinderBackend()
	b.Name = name
	b.Namespace = namespace
	b.Spec.CinderRef = CinderRefSpec{Name: cinderRef}
	return b
}

// integrationRBDBackend returns validRBDCinderBackend() stamped with
// name/namespace and pointed at cinderRef, for API server submission.
func integrationRBDBackend(name, namespace, cinderRef string) *CinderBackend {
	b := validRBDCinderBackend()
	b.Name = name
	b.Namespace = namespace
	b.Spec.CinderRef = CinderRefSpec{Name: cinderRef}
	return b
}

// integrationBackupBackend returns validCinderBackupBackend() stamped with
// name/namespace and pointed at cinderRef, for API server submission.
func integrationBackupBackend(name, namespace, cinderRef string) *CinderBackupBackend {
	b := validCinderBackupBackend()
	b.Name = name
	b.Namespace = namespace
	b.Spec.CinderRef = CinderRefSpec{Name: cinderRef}
	return b
}

// integrationRBDBackupBackend returns validRBDCinderBackupBackend() stamped with
// name/namespace and pointed at cinderRef, for API server submission.
func integrationRBDBackupBackend(name, namespace, cinderRef string) *CinderBackupBackend {
	b := validRBDCinderBackupBackend()
	b.Name = name
	b.Namespace = namespace
	b.Spec.CinderRef = CinderRefSpec{Name: cinderRef}
	return b
}

// expectRejected asserts that err is an API-server rejection carrying want.
func expectRejected(t testing.TB, err error, want string) {
	t.Helper()
	g := NewGomegaWithT(t)
	g.Expect(err).To(HaveOccurred())
	g.Expect(apierrors.IsInvalid(err) || apierrors.IsForbidden(err)).To(BeTrue(),
		fmt.Sprintf("expected Invalid or Forbidden status error, got: %v", err))
	g.Expect(err.Error()).To(ContainSubstring(want))
}

// --- CRD-level CEL / schema enforcement (no validating webhook installed) ---

// TestIntegration_CRD_CELOnly_RejectsCinderRefChange pins the cinderRef
// immutability transition rule on both satellites: re-pointing an attachment at a
// different Cinder would strand the volumes or backups the old deployment created
// under a host identity nothing serves anymore. The validating webhook never
// re-checks it, so the CEL rule is the only enforcement point.
func TestIntegration_CRD_CELOnly_RejectsCinderRefChange(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)

	t.Run("CinderBackend", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ns := newNamespace(t, ctx, c, "backend-cinderref-")

		b := integrationBackend("backend", ns, "cinder-a")
		g.Expect(c.Create(ctx, b)).To(Succeed(), "valid backend should be accepted")

		got := &CinderBackend{}
		g.Expect(c.Get(ctx, types.NamespacedName{Name: "backend", Namespace: ns}, got)).To(Succeed())
		got.Spec.CinderRef.Name = "cinder-b"
		expectRejected(t, c.Update(ctx, got), "cinderRef is immutable")
	})

	t.Run("CinderBackupBackend", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ns := newNamespace(t, ctx, c, "backup-cinderref-")

		b := integrationBackupBackend("backup", ns, "cinder-a")
		g.Expect(c.Create(ctx, b)).To(Succeed(), "valid backup backend should be accepted")

		got := &CinderBackupBackend{}
		g.Expect(c.Get(ctx, types.NamespacedName{Name: "backup", Namespace: ns}, got)).To(Succeed())
		got.Spec.CinderRef.Name = "cinder-b"
		expectRejected(t, c.Update(ctx, got), "cinderRef is immutable")
	})
}

// TestIntegration_CRD_CELOnly_RejectsTypeChange pins the type immutability
// transition rule on both satellites against a type outside the enum, which the
// immutability rule, the enum constraint, or both reject. A CinderBackend moves
// to a well-formed RBD one here as well, so only the transition rule is left to
// answer; TestIntegration_CRD_CELOnly_RejectsBackupTypeChange is the backup
// target's twin of that case.
func TestIntegration_CRD_CELOnly_RejectsTypeChange(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)

	t.Run("CinderBackend", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ns := newNamespace(t, ctx, c, "backend-type-")

		b := integrationBackend("backend", ns, "cinder-a")
		g.Expect(c.Create(ctx, b)).To(Succeed(), "valid backend should be accepted")

		got := &CinderBackend{}
		g.Expect(c.Get(ctx, types.NamespacedName{Name: "backend", Namespace: ns}, got)).To(Succeed())
		got.Spec.Type = CinderBackendTypeRBD
		got.Spec.NFS = nil
		got.Spec.RBD = validRBDCinderBackend().Spec.RBD

		expectRejected(t, c.Update(ctx, got), "type is immutable")
	})

	t.Run("CinderBackupBackend", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ns := newNamespace(t, ctx, c, "backup-type-")

		b := integrationBackupBackend("backup", ns, "cinder-a")
		g.Expect(c.Create(ctx, b)).To(Succeed(), "valid backup backend should be accepted")

		got := &CinderBackupBackend{}
		g.Expect(c.Get(ctx, types.NamespacedName{Name: "backup", Namespace: ns}, got)).To(Succeed())
		got.Spec.Type = CinderBackupBackendType("Swift")

		err := c.Update(ctx, got)
		g.Expect(err).To(HaveOccurred(), "changing type must be rejected on update")
		g.Expect(err.Error()).To(SatisfyAny(
			ContainSubstring("type is immutable"),
			ContainSubstring("Unsupported value"),
		))
	})
}

// TestIntegration_CRD_CELOnly_RejectsNFSUnionMissingBlock pins the type/nfs union
// rule on both satellites: a type-NFS attachment without its spec.nfs block is
// rejected by the CEL rule alone.
func TestIntegration_CRD_CELOnly_RejectsNFSUnionMissingBlock(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)

	t.Run("CinderBackend", func(t *testing.T) {
		ns := newNamespace(t, ctx, c, "backend-union-")
		b := integrationBackend("backend", ns, "cinder-a")
		b.Spec.NFS = nil
		expectRejected(t, c.Create(ctx, b), "exactly one backend block matching spec.type")
	})

	t.Run("CinderBackupBackend", func(t *testing.T) {
		ns := newNamespace(t, ctx, c, "backup-union-")
		b := integrationBackupBackend("backup", ns, "cinder-a")
		b.Spec.NFS = nil
		expectRejected(t, c.Create(ctx, b), "exactly one backup backend block matching spec.type")
	})
}

// TestIntegration_CRD_CELOnly_RejectsRBDUnionMismatch pins the second half of
// the union rule: a type-RBD backend without spec.rbd, and a type-NFS backend
// that carries spec.rbd beside spec.nfs, are both rejected by the CEL rule alone.
func TestIntegration_CRD_CELOnly_RejectsRBDUnionMismatch(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)
	const want = "exactly one backend block matching spec.type must be set (type NFS requires spec.nfs, type RBD requires spec.rbd)"

	t.Run("type RBD without the block", func(t *testing.T) {
		ns := newNamespace(t, ctx, c, "rbd-union-")
		b := integrationRBDBackend("backend", ns, "cinder-a")
		b.Spec.RBD = nil
		expectRejected(t, c.Create(ctx, b), want)
	})

	t.Run("type NFS with both blocks", func(t *testing.T) {
		ns := newNamespace(t, ctx, c, "rbd-union-")
		b := integrationBackend("backend", ns, "cinder-a")
		b.Spec.RBD = validRBDCinderBackend().Spec.RBD
		expectRejected(t, c.Create(ctx, b), want)
	})
}

// TestIntegration_CRD_CELOnly_RBDFieldPatterns pins the RBD field rules on the
// CRD alone: every value reaches a file, a command line or an ipBlock verbatim,
// so the schema must hold them while the webhook is down.
func TestIntegration_CRD_CELOnly_RBDFieldPatterns(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)

	for _, tc := range []struct {
		name   string
		mutate func(rbd *RBDBackendSpec)
		want   []string
	}{
		{
			name:   "user with the client. prefix",
			mutate: func(rbd *RBDBackendSpec) { rbd.User = "client.cinder" },
			want:   []string{"spec.rbd.user", "user is the cephx name without its client. prefix (write cinder, not client.cinder)"},
		},
		{
			name:   "empty monitors",
			mutate: func(rbd *RBDBackendSpec) { rbd.Monitors = []string{} },
			want:   []string{"spec.rbd.monitors", "should have at least 1 items"},
		},
		{
			name:   "network without a prefix length",
			mutate: func(rbd *RBDBackendSpec) { rbd.Networks = []string{"10.244.0.0"} },
			want:   []string{"spec.rbd.networks[0]", "should match"},
		},
		{
			name:   "uppercase key Secret name",
			mutate: func(rbd *RBDBackendSpec) { rbd.KeySecretRef.Name = "Ceph_Key" },
			want:   []string{"spec.rbd.keySecretRef.name", "should match"},
		},
		{
			name:   "secretUUID that is no UUID",
			mutate: func(rbd *RBDBackendSpec) { rbd.SecretUUID = "not-a-uuid" },
			want:   []string{"spec.rbd.secretUUID", "should match"},
		},
		{
			name:   "pool with a space",
			mutate: func(rbd *RBDBackendSpec) { rbd.Pool = "my pool" },
			want:   []string{"spec.rbd.pool", "should match"},
		},
		{
			name:   "clusterName with a path separator",
			mutate: func(rbd *RBDBackendSpec) { rbd.ClusterName = "ceph/../x" },
			want:   []string{"spec.rbd.clusterName", "should match"},
		},
		{
			name:   "monitor carrying a comma",
			mutate: func(rbd *RBDBackendSpec) { rbd.Monitors = []string{"mon-a,mon-b"} },
			want:   []string{"spec.rbd.monitors[0]", "should match"},
		},
		{
			name:   "IPv6 monitor literal",
			mutate: func(rbd *RBDBackendSpec) { rbd.Monitors = []string{"[fd00::1]:6789"} },
			want:   []string{"spec.rbd.monitors[0]", "should match"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ns := newNamespace(t, ctx, c, "rbd-fields-")
			b := integrationRBDBackend("backend", ns, "cinder-a")
			tc.mutate(b.Spec.RBD)
			err := c.Create(ctx, b)
			for _, want := range tc.want {
				expectRejected(t, err, want)
			}
		})
	}

	t.Run("a well-formed block is accepted", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ns := newNamespace(t, ctx, c, "rbd-fields-")
		b := integrationRBDBackend("backend", ns, "cinder-a")
		b.Spec.RBD.Monitors = []string{"10.96.12.3:6789", "ceph-mon-b.rook-ceph.svc"}
		b.Spec.RBD.SecretUUID = "457eb676-33da-42ec-9a8c-9293d545c337"
		g.Expect(c.Create(ctx, b)).To(Succeed())
	})
}

// TestIntegration_CRD_CELOnly_RejectsBackupTypeChange moves an NFS backup target
// to a well-formed RBD one, so the enum and the union rule both pass and only
// the transition rule is left to answer: a restore reads a backup with the
// driver that wrote it.
func TestIntegration_CRD_CELOnly_RejectsBackupTypeChange(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)
	ns := newNamespace(t, ctx, c, "backup-type-rbd-")

	b := integrationBackupBackend("backup", ns, "cinder-a")
	g.Expect(c.Create(ctx, b)).To(Succeed(), "valid backup backend should be accepted")

	got := &CinderBackupBackend{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "backup", Namespace: ns}, got)).To(Succeed())
	got.Spec.Type = CinderBackupBackendTypeRBD
	got.Spec.NFS = nil
	got.Spec.RBD = validRBDCinderBackupBackend().Spec.RBD

	err := c.Update(ctx, got)
	expectRejected(t, err, "type is immutable")
	g.Expect(err.Error()).NotTo(ContainSubstring("exactly one backup backend block"),
		"the update carries the block its new type needs")
}

// TestIntegration_CRD_CELOnly_RejectsBackupRBDUnionMismatch pins the second half
// of the backup union rule: a type-RBD target without spec.rbd, and a type-NFS
// target that carries spec.rbd beside spec.nfs, are both rejected by the CEL
// rule alone.
func TestIntegration_CRD_CELOnly_RejectsBackupRBDUnionMismatch(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)
	const want = "exactly one backup backend block matching spec.type must be set " +
		"(type NFS requires spec.nfs, type RBD requires spec.rbd)"

	t.Run("type RBD without the block", func(t *testing.T) {
		ns := newNamespace(t, ctx, c, "backup-rbd-union-")
		b := integrationRBDBackupBackend("backup", ns, "cinder-a")
		b.Spec.RBD = nil
		expectRejected(t, c.Create(ctx, b), want)
	})

	t.Run("type NFS with both blocks", func(t *testing.T) {
		ns := newNamespace(t, ctx, c, "backup-rbd-union-")
		b := integrationBackupBackend("backup", ns, "cinder-a")
		b.Spec.RBD = validRBDCinderBackupBackend().Spec.RBD
		expectRejected(t, c.Create(ctx, b), want)
	})
}

// TestIntegration_CRD_CELOnly_BackupRBDFieldPatterns pins the RBD field rules of
// the backup target on the CRD alone: every value reaches a file, a command line
// or an ipBlock verbatim, so the schema must hold them while the webhook is
// down.
func TestIntegration_CRD_CELOnly_BackupRBDFieldPatterns(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)

	for _, tc := range []struct {
		name   string
		mutate func(rbd *RBDBackupBackendSpec)
		want   []string
	}{
		{
			name:   "user with the client. prefix",
			mutate: func(rbd *RBDBackupBackendSpec) { rbd.User = "client.cinder-backup" },
			want: []string{
				"spec.rbd.user",
				"user is the cephx name without its client. prefix (write cinder-backup, not client.cinder-backup)",
			},
		},
		{
			name:   "empty monitors",
			mutate: func(rbd *RBDBackupBackendSpec) { rbd.Monitors = []string{} },
			want:   []string{"spec.rbd.monitors", "should have at least 1 items"},
		},
		{
			name:   "network without a prefix length",
			mutate: func(rbd *RBDBackupBackendSpec) { rbd.Networks = []string{"10.244.0.0"} },
			want:   []string{"spec.rbd.networks[0]", "should match"},
		},
		{
			name:   "uppercase key Secret name",
			mutate: func(rbd *RBDBackupBackendSpec) { rbd.KeySecretRef.Name = "Ceph_Key" },
			want:   []string{"spec.rbd.keySecretRef.name", "should match"},
		},
		{
			name:   "pool with a space",
			mutate: func(rbd *RBDBackupBackendSpec) { rbd.Pool = "my pool" },
			want:   []string{"spec.rbd.pool", "should match"},
		},
		{
			name:   "clusterName with a path separator",
			mutate: func(rbd *RBDBackupBackendSpec) { rbd.ClusterName = "ceph/../x" },
			want:   []string{"spec.rbd.clusterName", "should match"},
		},
		{
			name:   "monitor carrying a comma",
			mutate: func(rbd *RBDBackupBackendSpec) { rbd.Monitors = []string{"mon-a,mon-b"} },
			want:   []string{"spec.rbd.monitors[0]", "should match"},
		},
		{
			name:   "IPv6 monitor literal",
			mutate: func(rbd *RBDBackupBackendSpec) { rbd.Monitors = []string{"[fd00::1]:6789"} },
			want:   []string{"spec.rbd.monitors[0]", "should match"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ns := newNamespace(t, ctx, c, "backup-rbd-fields-")
			b := integrationRBDBackupBackend("backup", ns, "cinder-a")
			tc.mutate(b.Spec.RBD)
			err := c.Create(ctx, b)
			for _, want := range tc.want {
				expectRejected(t, err, want)
			}
		})
	}

	t.Run("a well-formed block is accepted", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ns := newNamespace(t, ctx, c, "backup-rbd-fields-")
		b := integrationRBDBackupBackend("backup", ns, "cinder-a")
		b.Spec.RBD.Monitors = []string{"10.96.12.3:6789", "ceph-mon-b.rook-ceph.svc"}
		b.Spec.RBD.Networks = []string{"10.244.0.0/16", "10.96.0.0/12"}
		g.Expect(c.Create(ctx, b)).To(Succeed())
	})
}

// TestIntegration_CRD_CELOnly_RejectsVolumeReplicasAboveOne pins the
// single-writer rule on the volume Deployment: a second cinder-volume under the
// same host identity has the same volume state open twice, which the NFS drivers
// refuse, so the rule is schema-level rather than webhook-level.
func TestIntegration_CRD_CELOnly_RejectsVolumeReplicasAboveOne(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)
	ns := newNamespace(t, ctx, c, "volume-replicas-")

	cinder := integrationCinder("cinder", ns)
	cinder.Spec.Volume.Deployment.Replicas = 2
	expectRejected(t, c.Create(ctx, cinder), "volume/backup deployments run exactly one replica")
}

// TestIntegration_CRD_CELOnly_RejectsBackupStrategyNotRecreate pins the strategy
// half of the same invariant: a rolling update overlaps the surge pod with the
// outgoing one, which is the same collision by another route.
func TestIntegration_CRD_CELOnly_RejectsBackupStrategyNotRecreate(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)
	ns := newNamespace(t, ctx, c, "backup-strategy-")

	cinder := integrationCinder("cinder", ns)
	cinder.Spec.Backup.Deployment.Strategy = &appsv1.DeploymentStrategy{
		Type: appsv1.RollingUpdateDeploymentStrategyType,
	}
	expectRejected(t, c.Create(ctx, cinder), "volume/backup deployments must use the Recreate strategy")
}

// TestIntegration_CRD_CELOnly_RejectsServiceUserWithoutKeystone pins the pairing
// rule: the endpoint names who to authenticate against and the service user
// carries the credentials, so either alone renders a [keystone_authtoken] section
// the service cannot use.
func TestIntegration_CRD_CELOnly_RejectsServiceUserWithoutKeystone(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)
	ns := newNamespace(t, ctx, c, "serviceuser-pairing-")

	cinder := integrationCinder("cinder", ns)
	cinder.Spec.KeystoneEndpoint = ""
	expectRejected(t, c.Create(ctx, cinder), "keystoneEndpoint and serviceUser must be set together")
}

// TestIntegration_CRD_CELOnly_RejectsKeyManagerWithoutKeystone pins the rule that
// follows from the pairing one: castellan reaches Barbican with the same Keystone
// credentials, so a key manager without an endpoint has nothing to authenticate
// with.
func TestIntegration_CRD_CELOnly_RejectsKeyManagerWithoutKeystone(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)
	ns := newNamespace(t, ctx, c, "keymanager-keystone-")

	cinder := integrationCinder("cinder", ns)
	cinder.Spec.KeystoneEndpoint = ""
	cinder.Spec.ServiceUser = nil
	cinder.Spec.KeyManager = &KeyManagerSpec{
		Type:     KeyManagerTypeBarbican,
		Barbican: &BarbicanKeyManagerSpec{Endpoint: "http://barbican.openstack.svc:9311"},
	}
	expectRejected(t, c.Create(ctx, cinder), "keyManager requires keystoneEndpoint")
}

// TestIntegration_CRD_CELOnly_RejectsTargetClusterRefChange pins the
// targetClusterRef rename rule: re-pointing a Cinder at another target cluster is
// rejected by the CRD CEL rule alone, without the validating webhook.
func TestIntegration_CRD_CELOnly_RejectsTargetClusterRefChange(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)
	ns := newNamespace(t, ctx, c, "targetcluster-rename-")

	cinder := integrationCinder("cinder", ns)
	cinder.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "cluster-a"}
	g.Expect(c.Create(ctx, cinder)).To(Succeed(), "valid Cinder should be accepted")

	got := &Cinder{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "cinder", Namespace: ns}, got)).To(Succeed())
	got.Spec.TargetClusterRef.Name = "cluster-b"
	expectRejected(t, c.Update(ctx, got), "targetClusterRef is immutable")
}

// TestIntegration_CRD_CELOnly_OpenStackReleasePattern pins the openStackRelease
// pattern (^\d{4}\.[12]$) at the CRD level: cadence releases are accepted; a
// non-cadence minor and a non-numeric value are rejected.
func TestIntegration_CRD_CELOnly_OpenStackReleasePattern(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)

	cases := []struct {
		release string
		accept  bool
	}{
		{"2025.2", true},
		{"2026.1", true},
		{"2025.3", false},
		{"banana", false},
	}
	for i, tc := range cases {
		t.Run(tc.release, func(t *testing.T) {
			g := NewGomegaWithT(t)
			ns := newNamespace(t, ctx, c, "release-pattern-")

			cinder := integrationCinder(fmt.Sprintf("cinder-%d", i), ns)
			cinder.Spec.OpenStackRelease = tc.release

			err := c.Create(ctx, cinder)
			if tc.accept {
				g.Expect(err).NotTo(HaveOccurred(), "release %q should be accepted", tc.release)
				return
			}
			expectRejected(t, err, "openStackRelease")
		})
	}
}

// TestIntegration_CRD_CELOnly_BackupFileSizeBounds pins the two numeric markers
// on spec.fileSize: cinder splits a volume into chunks of that size and hashes
// each in 32 KiB blocks, so a value below one mebibyte or off that block size
// fails every backup at runtime.
func TestIntegration_CRD_CELOnly_BackupFileSizeBounds(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)

	for _, tc := range []struct {
		name     string
		fileSize int64
	}{
		{name: "below the floor", fileSize: 65536},
		{name: "off the block size", fileSize: 52428801},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ns := newNamespace(t, ctx, c, "filesize-")
			b := integrationBackupBackend("backup", ns, "cinder-a")
			b.Spec.FileSize = ptr.To(tc.fileSize)
			expectRejected(t, c.Create(ctx, b), "fileSize")
		})
	}
}

// TestIntegration_CRD_CELOnly_NFSExportPathPattern pins the ^/[!-~]*$ pattern on
// both export paths against the API server's own regex engine. The allowlist is
// what makes the schema and the python readers agree on where an export lives:
// RE2 — the flavour a schema pattern compiles with — resolves \s to five ASCII
// characters, while cinder's shares-file loader and oslo.config strip the full
// Unicode whitespace set, so a path with a pasted U+00A0 would be admitted and
// then resolved to a different export than the one this operator mounts.
func TestIntegration_CRD_CELOnly_NFSExportPathPattern(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)

	for _, tc := range []struct {
		name   string
		path   string
		accept bool
	}{
		{name: "printable ascii", path: "/exports/volumes", accept: true},
		{name: "trailing ascii space", path: "/exports/volumes "},
		{name: "trailing non-breaking space", path: "/exports/volumes\u00a0"},
		{name: "embedded newline", path: "/exports/volumes\nnfs.example.com:/exports/other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			ns := newNamespace(t, ctx, c, "export-path-")

			backend := integrationBackend("backend", ns, "cinder-a")
			backend.Spec.NFS.Path = tc.path
			backupBackend := integrationBackupBackend("backup", ns, "cinder-a")
			backupBackend.Spec.NFS.Path = tc.path

			backendErr := c.Create(ctx, backend)
			backupErr := c.Create(ctx, backupBackend)
			if tc.accept {
				g.Expect(backendErr).NotTo(HaveOccurred(), "path %q should be accepted", tc.path)
				g.Expect(backupErr).NotTo(HaveOccurred(), "path %q should be accepted", tc.path)
				return
			}
			expectRejected(t, backendErr, "spec.nfs.path")
			expectRejected(t, backupErr, "spec.nfs.path")
		})
	}
}

// TestIntegration_CRD_MountOptionsDefaultMaterialized proves the CRD schema
// default is materialized without the mutating webhook: an nfs block that omits
// mountOptions comes back with the soft NFSv4.1 mount.
func TestIntegration_CRD_MountOptionsDefaultMaterialized(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)
	ns := newNamespace(t, ctx, c, "mountoptions-default-")

	b := integrationBackend("backend", ns, "cinder-a")
	b.Spec.NFS.MountOptions = ""
	g.Expect(c.Create(ctx, b)).To(Succeed(), "backend without mountOptions should be accepted")

	got := &CinderBackend{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "backend", Namespace: ns}, got)).To(Succeed())
	g.Expect(got.Spec.NFS.MountOptions).To(Equal(DefaultNFSMountOptions),
		"CRD default must materialize the mount options")
}

// TestIntegration_CRD_RBDClusterNameDefaultMaterialized proves the CRD schema
// default is materialized without the mutating webhook: an rbd block that omits
// clusterName comes back with ceph.
func TestIntegration_CRD_RBDClusterNameDefaultMaterialized(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)
	ns := newNamespace(t, ctx, c, "clustername-default-")

	b := integrationRBDBackend("backend", ns, "cinder-a")
	b.Spec.RBD.ClusterName = ""
	g.Expect(c.Create(ctx, b)).To(Succeed(), "backend without clusterName should be accepted")

	got := &CinderBackend{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "backend", Namespace: ns}, got)).To(Succeed())
	g.Expect(got.Spec.RBD.ClusterName).To(Equal(DefaultRBDClusterName),
		"CRD default must materialize the cluster name")
}

// TestIntegration_CRD_BackupRBDClusterNameDefaultMaterialized proves the CRD
// schema default is materialized on the backup target without the mutating
// webhook: an rbd block that omits clusterName comes back with ceph.
func TestIntegration_CRD_BackupRBDClusterNameDefaultMaterialized(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)
	ns := newNamespace(t, ctx, c, "backup-clustername-default-")

	b := integrationRBDBackupBackend("backup", ns, "cinder-a")
	b.Spec.RBD.ClusterName = ""
	g.Expect(c.Create(ctx, b)).To(Succeed(), "backup target without clusterName should be accepted")

	got := &CinderBackupBackend{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "backup", Namespace: ns}, got)).To(Succeed())
	g.Expect(got.Spec.RBD.ClusterName).To(Equal(DefaultRBDClusterName),
		"CRD default must materialize the cluster name")
}

// --- Live admission round-trip (webhooks running) ---

// TestIntegration_WebhookRejectsRBDNetworkWithHostBits proves the webhook
// answers what the pattern cannot: 10.128.0.1/22 is shaped like a CIDR, but the
// API server would reject it as an ipBlock.cidr, so the webhook refuses it at
// admission instead.
func TestIntegration_WebhookRejectsRBDNetworkWithHostBits(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTest(t)
	ns := newNamespace(t, ctx, c, "rbd-host-bits-")

	b := integrationRBDBackend("backend", ns, "cinder-a")
	b.Spec.RBD.Networks = []string{"10.128.0.1/22"}
	err := c.Create(ctx, b)
	expectRejected(t, err, "spec.rbd.networks[0]")
	expectRejected(t, err, "must be a canonical IPv4 CIDR such as 10.128.0.0/16 (host bits zero)")
}

// TestIntegration_WebhookRejectsBackupRBDNetworkWithHostBits is the backup
// target's twin: 10.128.0.1/22 passes the pattern, and the webhook refuses it
// because it would end up as an ipBlock.cidr of the Ceph egress rule.
func TestIntegration_WebhookRejectsBackupRBDNetworkWithHostBits(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTest(t)
	ns := newNamespace(t, ctx, c, "backup-rbd-host-bits-")

	b := integrationRBDBackupBackend("backup", ns, "cinder-a")
	b.Spec.RBD.Networks = []string{"10.128.0.1/22"}
	err := c.Create(ctx, b)
	expectRejected(t, err, "spec.rbd.networks[0]")
	expectRejected(t, err, "must be a canonical IPv4 CIDR such as 10.128.0.0/16 (host bits zero)")
}

// TestIntegration_WebhookDefaultsServiceUser proves the mutating webhook fills
// the service-user identity defaults and the secretRef key on a minimal CR that
// supplies only the password Secret name.
func TestIntegration_WebhookDefaultsServiceUser(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTest(t)
	ns := newNamespace(t, ctx, c, "serviceuser-defaults-")

	cinder := integrationCinder("cinder", ns)
	cinder.Spec.ServiceUser = &ServiceUserSpec{
		SecretRef: commonv1.SecretRefSpec{Name: "cinder-service-password"},
	}
	g.Expect(c.Create(ctx, cinder)).To(Succeed(), "minimal Cinder should be accepted after defaults")

	got := &Cinder{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "cinder", Namespace: ns}, got)).To(Succeed())
	g.Expect(got.Spec.ServiceUser.Username).To(Equal("cinder"))
	g.Expect(got.Spec.ServiceUser.ProjectName).To(Equal("service"))
	g.Expect(got.Spec.ServiceUser.UserDomainName).To(Equal("Default"))
	g.Expect(got.Spec.ServiceUser.ProjectDomainName).To(Equal("Default"))
	g.Expect(got.Spec.ServiceUser.SecretRef.Key).To(Equal("password"))
}

// TestIntegration_WebhookDefaultsAbsentDeploymentBlocks proves the absent-block
// path through the real API server: a manifest that names none of the four
// Deployment blocks comes back with one replica on the three single-process
// Deployments, the Recreate strategy on the two single-writer ones, and the
// raised backup memory limit.
//
// The CR is submitted unstructured because that is the only way to express an
// absent block: the four blocks are non-pointer structs, so a typed client
// serializes "deployment: {}" for each of them however empty they are, and the
// API server then fills the schema default of three replicas before any webhook
// runs. That is the case the CEL rules reject and the defaulting webhook
// deliberately leaves alone.
func TestIntegration_WebhookDefaultsAbsentDeploymentBlocks(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTest(t)
	ns := newNamespace(t, ctx, c, "deployment-defaults-")

	manifest := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": GroupVersion.String(),
		"kind":       "Cinder",
		"metadata": map[string]any{
			"name":      "cinder",
			"namespace": ns,
		},
		"spec": map[string]any{
			"openStackRelease": "2026.1",
			"image": map[string]any{
				"repository": "ghcr.io/c5c3/cinder",
				"tag":        "2026.1",
			},
			"database": map[string]any{
				"clusterRef": map[string]any{"name": "mariadb"},
				"database":   "cinder",
				"secretRef":  map[string]any{"name": "cinder-db"},
			},
			"cache": map[string]any{
				"clusterRef": map[string]any{"name": "memcached"},
				"backend":    commonv1.DefaultCacheBackend,
			},
			"messaging": map[string]any{
				"clusterRef": map[string]any{"name": "rabbitmq"},
			},
		},
	}}
	g.Expect(c.Create(ctx, manifest)).To(Succeed(), "a manifest without deployment blocks should be accepted")

	got := &Cinder{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "cinder", Namespace: ns}, got)).To(Succeed())
	g.Expect(got.Spec.API.Deployment.Replicas).To(Equal(commonv1.DefaultReplicas))
	g.Expect(got.Spec.Scheduler.Deployment.Replicas).To(Equal(int32(1)))
	g.Expect(got.Spec.Volume.Deployment.Replicas).To(Equal(int32(1)))
	g.Expect(got.Spec.Backup.Deployment.Replicas).To(Equal(int32(1)))
	g.Expect(got.Spec.Volume.Deployment.Strategy.Type).To(Equal(appsv1.RecreateDeploymentStrategyType))
	g.Expect(got.Spec.Backup.Deployment.Strategy.Type).To(Equal(appsv1.RecreateDeploymentStrategyType))
	g.Expect(got.Spec.Backup.Deployment.Resources).To(BeNil(), "the reconciler, not the webhook, sizes the backup memory")
	g.Expect(got.Spec.API.UWSGI).NotTo(BeNil())
}

// TestIntegration_WebhookRejectsReservedBackendName proves the validating webhook
// rejects a backend whose metadata.name equals a cinder.conf section the catalog
// enumerates.
func TestIntegration_WebhookRejectsReservedBackendName(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTest(t)
	ns := newNamespace(t, ctx, c, "reserved-name-")

	b := integrationBackend("database", ns, "cinder-a")
	expectRejected(t, c.Create(ctx, b), "collides with the [database] section")
}

// TestIntegration_WebhookEnforcesSingleBackupAttachment proves the validating
// webhook enforces the one-backup-driver invariant per Cinder via the live API: a
// second attachment to the same Cinder is rejected, while one to a different
// Cinder is accepted.
func TestIntegration_WebhookEnforcesSingleBackupAttachment(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTest(t)
	ns := newNamespace(t, ctx, c, "single-backup-")

	first := integrationBackupBackend("backup-a1", ns, "cinder-a")
	g.Expect(c.Create(ctx, first)).To(Succeed(), "the first backup backend for cinder-a should be accepted")

	second := integrationBackupBackend("backup-a2", ns, "cinder-a")
	expectRejected(t, c.Create(ctx, second), `already has CinderBackupBackend "backup-a1" attached`)

	other := integrationBackupBackend("backup-b1", ns, "cinder-b")
	g.Expect(c.Create(ctx, other)).To(Succeed(), "a backup backend for a different Cinder should be accepted")
}

// TestIntegration_WebhookRejectsUnknownExtraConfigOption pins the extraConfig
// option-catalog check through the real API server: a CR whose extraConfig names
// an option the release's catalog does not accept is rejected by the validating
// webhook. "glance_api_server" is a typo for the [DEFAULT] glance_api_servers
// option.
func TestIntegration_WebhookRejectsUnknownExtraConfigOption(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTest(t)
	ns := newNamespace(t, ctx, c, "extraconfig-catalog-")

	cinder := integrationCinder("cinder", ns)
	cinder.Spec.ExtraConfig = map[string]map[string]string{
		"DEFAULT": {"glance_api_server": "http://glance.openstack.svc:9292"},
	}
	expectRejected(t, c.Create(ctx, cinder), "no such option in the cinder 2026.1 option catalog")
}

// TestIntegration_CRD_CELOnly_VerticalAutoscaling pins the CEL rules of
// verticalAutoscaling on the Cinder CRD against an envtest API server
// with NO validating webhook installed, so the webhook twins cannot mask a
// missing rule.
func TestIntegration_CRD_CELOnly_VerticalAutoscaling(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	c, ctx, _ := setupEnvTestNoWebhook(t)

	for _, tc := range []struct {
		name    string
		mutate  func(o *Cinder)
		wantSub string
	}{
		{
			name: "foreign-resource",
			mutate: func(o *Cinder) {
				o.Spec.API.Deployment.VerticalAutoscaling = &VerticalAutoscalingSpec{
					UpdateMode: "Off",
					MinAllowed: corev1.ResourceList{corev1.ResourceEphemeralStorage: resource.MustParse("1Gi")},
				}
			},
			wantSub: "minAllowed may only name cpu and memory",
		},
		{
			name: "beside-autoscaling",
			mutate: func(o *Cinder) {
				cpu := int32(80)
				o.Spec.Autoscaling = &AutoscalingSpec{MaxReplicas: 5, TargetCPUUtilization: &cpu}
				o.Spec.API.Deployment.VerticalAutoscaling = &VerticalAutoscalingSpec{UpdateMode: "Off"}
			},
			wantSub: "spec.api.deployment.verticalAutoscaling cannot be set while spec.autoscaling scales the same Deployment",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			ns := newNamespace(t, ctx, c, "celonly-vpa-")
			o := integrationCinder("vpa-"+tc.name, ns)
			tc.mutate(o)

			err := c.Create(ctx, o)
			g.Expect(err).To(HaveOccurred())
			g.Expect(apierrors.IsInvalid(err)).To(BeTrue(), fmt.Sprintf("expected Invalid status error, got: %v", err))
			g.Expect(err.Error()).To(ContainSubstring(tc.wantSub))
		})
	}
}
