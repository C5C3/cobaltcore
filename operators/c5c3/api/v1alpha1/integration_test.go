// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package v1alpha1

import (
	"cmp"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	"github.com/c5c3/cobaltcore/operators/c5c3/internal/testutil"
)

// --- Helpers ---

// setupEnvTestNoWebhook wraps testutil.SetupC5c3EnvTestNoWebhook with the
// v1alpha1 scheme registration, avoiding the import cycle between testutil and
// this package. The CRD-only setup keeps every webhook out of the way so no
// webhook check can mask a missing CEL rule.
func setupEnvTestNoWebhook(t testing.TB) (client.Client, context.Context, context.CancelFunc) {
	t.Helper()
	return testutil.SetupC5c3EnvTestNoWebhook(t, AddToScheme)
}

// newNamespace creates a uniquely named namespace for a test.
func newNamespace(t testing.TB, ctx context.Context, c client.Client, prefix string) string {
	t.Helper()
	g := NewGomegaWithT(t)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: prefix}}
	g.Expect(c.Create(ctx, ns)).To(Succeed(), "create namespace")
	return ns.Name
}

// stamp sets name and namespace on a webhook-test fixture, which carries the
// ObjectMeta of a fixture rather than of a submitted object.
func stamp[T client.Object](obj T, name, namespace string) T {
	obj.SetName(name)
	obj.SetNamespace(namespace)
	return obj
}

// expectAdmission asserts that err admits the object when wantSub is empty,
// and otherwise that it is an Invalid status error carrying wantSub.
func expectAdmission(t testing.TB, err error, wantSub string) {
	t.Helper()
	g := NewGomegaWithT(t)
	if wantSub == "" {
		g.Expect(err).NotTo(HaveOccurred(), "admission must admit the object")
		return
	}
	g.Expect(err).To(HaveOccurred(), "admission must reject: %s", wantSub)
	g.Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected Invalid status error, got: %v", err)
	g.Expect(err.Error()).To(ContainSubstring(wantSub))
}

// expectUpdate creates obj, reads it back, applies mutate and asserts the
// update's admission as expectAdmission does. A transition rule compares the
// stored object with the update, so both have to pass through the API server.
func expectUpdate[T client.Object](
	t testing.TB, ctx context.Context, c client.Client, obj T, mutate func(T), wantSub string,
) {
	t.Helper()
	g := NewGomegaWithT(t)
	g.Expect(c.Create(ctx, obj)).To(Succeed(), "the base object must be admitted")
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(obj), obj)).To(Succeed())
	mutate(obj)
	expectAdmission(t, c.Update(ctx, obj), wantSub)
}

// --- CRD-level CEL / schema enforcement (no webhook installed) ---

// TestIntegration_CRD_CELOnly_CRDsInstalled pins the environment the tests in
// this file run in: exactly the twelve c5c3 CRDs, so a widened setup cannot
// bring a sibling-operator CRD or a fake CRD (k8s.mariadb.com,
// openstack.k-orc.cloud, ...) into the API server unnoticed.
//
// Rules: none; this test guards the environment.
func TestIntegration_CRD_CELOnly_CRDsInstalled(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)
	c, ctx, _ := setupEnvTestNoWebhook(t)

	crds := &apiextensionsv1.CustomResourceDefinitionList{}
	g.Expect(c.List(ctx, crds)).To(Succeed())
	names := make([]string, 0, len(crds.Items))
	for _, crd := range crds.Items {
		names = append(names, crd.Name)
	}
	g.Expect(names).To(ConsistOf(
		"controlplanes.c5c3.io",
		"credentialrotations.c5c3.io",
		"keystoneapplicationcredentials.c5c3.io",
		"keystonecatalogentries.c5c3.io",
		"keystoneprojects.c5c3.io",
		"keystoneroleassignments.c5c3.io",
		"keystoneservices.c5c3.io",
		"keystoneusers.c5c3.io",
		"mariadbdatabases.c5c3.io",
		"rabbitmqvhosts.c5c3.io",
		"secretaggregates.c5c3.io",
		"sizingprofiles.c5c3.io",
	))
}

// TestIntegration_CRD_CELOnly_ControlPlaneKeystoneExternalMode pins the
// External-mode rules of services.keystone: the external block is required in
// External mode and only allowed there, and every managed-only knob is
// forbidden beside it.
//
// Rules: controlplane_types.go:411, 412, 413, 414, 415, 416, 417, 418, 419, 420, 421, 422, 423, 424
func TestIntegration_CRD_CELOnly_ControlPlaneKeystoneExternalMode(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	c, ctx, _ := setupEnvTestNoWebhook(t)

	image := &commonv1.ImageSpec{Repository: "ghcr.io/c5c3/keystone", Tag: "2026.1"}
	for _, tc := range []struct {
		name    string
		base    func() *ControlPlane // nil: externalControlPlane
		mutate  func(k *ServiceKeystoneSpec)
		wantSub string // empty: the object must be admitted
	}{
		{
			name:    "external-without-block",
			mutate:  func(k *ServiceKeystoneSpec) { k.External = nil },
			wantSub: "services.keystone.external is required when services.keystone.mode is External",
		},
		{
			name: "block-without-external",
			base: validControlPlane,
			mutate: func(k *ServiceKeystoneSpec) {
				k.External = &ExternalKeystoneSpec{AuthURL: "https://keystone.example.com/v3"}
			},
			wantSub: "services.keystone.external may only be set when services.keystone.mode is External",
		},
		{
			name:    "image",
			mutate:  func(k *ServiceKeystoneSpec) { k.Image = image },
			wantSub: "services.keystone.image is forbidden when services.keystone.mode is External",
		},
		{
			name: "policy-overrides",
			mutate: func(k *ServiceKeystoneSpec) {
				k.PolicyOverrides = &commonv1.PolicySpec{Rules: map[string]string{"identity:list_users": "role:admin"}}
			},
			wantSub: "services.keystone.policyOverrides is forbidden when services.keystone.mode is External",
		},
		{
			name:    "rotation-interval",
			mutate:  func(k *ServiceKeystoneSpec) { k.RotationInterval = &metav1.Duration{Duration: 24 * time.Hour} },
			wantSub: "services.keystone.rotationInterval is forbidden when services.keystone.mode is External",
		},
		{
			name:    "gateway",
			mutate:  func(k *ServiceKeystoneSpec) { k.Gateway = placedGateway("keystone.example.com") },
			wantSub: "services.keystone.gateway is forbidden when services.keystone.mode is External",
		},
		{
			name:    "public-endpoint",
			mutate:  func(k *ServiceKeystoneSpec) { k.PublicEndpoint = "https://keystone.example.com" },
			wantSub: "services.keystone.publicEndpoint is forbidden when services.keystone.mode is External",
		},
		{
			name: "federation-proxy-image",
			mutate: func(k *ServiceKeystoneSpec) {
				k.FederationProxyImage = &commonv1.ImageSpec{Repository: "ghcr.io/c5c3/federation-proxy", Tag: "2026.1"}
			},
			wantSub: "services.keystone.federationProxyImage is forbidden when services.keystone.mode is External",
		},
		{
			name: "dedicated-backing-services",
			mutate: func(k *ServiceKeystoneSpec) {
				k.DedicatedBackingServices = &KeystoneDedicatedBackingServicesSpec{Cache: &commonv1.CacheSpec{
					ClusterRef: &corev1.LocalObjectReference{Name: "cache"},
					Backend:    commonv1.DefaultCacheBackend,
				}}
			},
			wantSub: "services.keystone.dedicatedBackingServices is forbidden when services.keystone.mode is External",
		},
		{
			name:    "namespace",
			mutate:  func(k *ServiceKeystoneSpec) { k.Namespace = &ServiceNamespaceSpec{Name: "keystone"} },
			wantSub: "services.keystone.namespace is forbidden when services.keystone.mode is External",
		},
		{
			name:    "database-credentials-mode",
			mutate:  func(k *ServiceKeystoneSpec) { k.DatabaseCredentialsMode = "Static" },
			wantSub: "services.keystone.databaseCredentialsMode is forbidden when services.keystone.mode is External",
		},
		{
			name: "extra-config",
			mutate: func(k *ServiceKeystoneSpec) {
				k.ExtraConfig = map[string]map[string]string{"DEFAULT": {"debug": "true"}}
			},
			wantSub: "services.keystone.extraConfig is forbidden when services.keystone.mode is External",
		},
		{
			name:    "target-cluster-ref",
			mutate:  func(k *ServiceKeystoneSpec) { k.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "edge-1"} },
			wantSub: "services.keystone.targetClusterRef is forbidden when services.keystone.mode is External",
		},
		{
			name:    "ca-bundle-secret-ref",
			mutate:  func(k *ServiceKeystoneSpec) { k.CABundleSecretRef = &commonv1.SecretRefSpec{Name: "keystone-ca"} },
			wantSub: "services.keystone.caBundleSecretRef is forbidden when services.keystone.mode is External",
		},
		{
			name:   "external-base",
			mutate: func(*ServiceKeystoneSpec) {},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := tc.base
			if base == nil {
				base = externalControlPlane
			}
			cp := stamp(base(), "cp", newNamespace(t, ctx, c, "celonly-external-"))
			tc.mutate(cp.Spec.Services.Keystone)
			expectAdmission(t, c.Create(ctx, cp), tc.wantSub)
		})
	}
}

// TestIntegration_CRD_CELOnly_ControlPlaneDedicatedBackingServices pins the
// rule every service's dedicatedBackingServices block carries: an empty block
// is rejected, a block with only a cache is admitted. Horizon has no database,
// so its message names the cache class alone.
//
// Rules: controlplane_types.go:674, 701, 1253, 1386, 1619, 1806, 2309, 2633
func TestIntegration_CRD_CELOnly_ControlPlaneDedicatedBackingServices(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	c, ctx, _ := setupEnvTestNoWebhook(t)

	const databaseOrCache = "dedicatedBackingServices must declare at least one backing-service class (database, cache)"
	for _, svc := range []struct {
		name string
		base func() *ControlPlane
		// set replaces the service's dedicatedBackingServices block with one
		// holding cache; a nil cache leaves the block empty.
		set     func(cp *ControlPlane, cache *commonv1.CacheSpec)
		wantSub string
	}{
		{
			name: "keystone",
			base: dedicatedControlPlane,
			set: func(cp *ControlPlane, cache *commonv1.CacheSpec) {
				cp.Spec.Services.Keystone.DedicatedBackingServices = &KeystoneDedicatedBackingServicesSpec{Cache: cache}
			},
			wantSub: databaseOrCache,
		},
		{
			name: "horizon",
			base: dedicatedControlPlane,
			set: func(cp *ControlPlane, cache *commonv1.CacheSpec) {
				cp.Spec.Services.Horizon.DedicatedBackingServices = &HorizonDedicatedBackingServicesSpec{Cache: cache}
			},
			wantSub: "dedicatedBackingServices must declare at least one backing-service class (cache)",
		},
		{
			name: "glance",
			base: glanceControlPlane,
			set: func(cp *ControlPlane, cache *commonv1.CacheSpec) {
				cp.Spec.Services.Glance.DedicatedBackingServices = &GlanceDedicatedBackingServicesSpec{Cache: cache}
			},
			wantSub: databaseOrCache,
		},
		{
			name: "placement",
			base: func() *ControlPlane {
				cp := validControlPlane()
				cp.Spec.Services.Placement = &ServicePlacementSpec{}
				return cp
			},
			set: func(cp *ControlPlane, cache *commonv1.CacheSpec) {
				cp.Spec.Services.Placement.DedicatedBackingServices = &PlacementDedicatedBackingServicesSpec{Cache: cache}
			},
			wantSub: databaseOrCache,
		},
		{
			name: "barbican",
			base: barbicanControlPlane,
			set: func(cp *ControlPlane, cache *commonv1.CacheSpec) {
				cp.Spec.Services.Barbican.DedicatedBackingServices = &BarbicanDedicatedBackingServicesSpec{Cache: cache}
			},
			wantSub: databaseOrCache,
		},
		{
			name: "neutron",
			base: neutronControlPlane,
			set: func(cp *ControlPlane, cache *commonv1.CacheSpec) {
				cp.Spec.Services.Neutron.DedicatedBackingServices = &NeutronDedicatedBackingServicesSpec{Cache: cache}
			},
			wantSub: databaseOrCache,
		},
		{
			name: "cinder",
			base: cinderControlPlane,
			set: func(cp *ControlPlane, cache *commonv1.CacheSpec) {
				cp.Spec.Services.Cinder.DedicatedBackingServices = &CinderDedicatedBackingServicesSpec{Cache: cache}
			},
			wantSub: databaseOrCache,
		},
		{
			name: "nova",
			base: novaControlPlane,
			set: func(cp *ControlPlane, cache *commonv1.CacheSpec) {
				cp.Spec.Services.Nova.DedicatedBackingServices = &NovaDedicatedBackingServicesSpec{Cache: cache}
			},
			wantSub: databaseOrCache,
		},
	} {
		t.Run(svc.name+"-empty", func(t *testing.T) {
			cp := stamp(svc.base(), "cp", newNamespace(t, ctx, c, "celonly-dedicated-"))
			svc.set(cp, nil)
			expectAdmission(t, c.Create(ctx, cp), svc.wantSub)
		})
		t.Run(svc.name+"-cache-only", func(t *testing.T) {
			cp := stamp(svc.base(), "cp", newNamespace(t, ctx, c, "celonly-dedicated-"))
			svc.set(cp, &commonv1.CacheSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: "cp-" + svc.name + "-cache"},
				Backend:    commonv1.DefaultCacheBackend,
			})
			expectAdmission(t, c.Create(ctx, cp), "")
		})
	}
}

// TestIntegration_CRD_CELOnly_ControlPlaneGlanceBackends pins the glance
// backends rules: exactly one entry is the default, and the s3 block is set
// exactly when the type is S3. The type enum has the single value S3, so the
// other direction of that rule (an s3 block beside another type) has no
// constructible input.
//
// Rules: controlplane_types.go:1012, 1163
func TestIntegration_CRD_CELOnly_ControlPlaneGlanceBackends(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	c, ctx, _ := setupEnvTestNoWebhook(t)

	for _, tc := range []struct {
		name    string
		mutate  func(g *ServiceGlanceSpec)
		wantSub string // empty: the object must be admitted
	}{
		{
			name:    "no-default",
			mutate:  func(g *ServiceGlanceSpec) { g.Backends[0].IsDefault = false },
			wantSub: "exactly one backends entry must set isDefault",
		},
		{
			name: "two-defaults",
			mutate: func(g *ServiceGlanceSpec) {
				secondary := g.Backends[0]
				secondary.Name = "secondary"
				g.Backends = append(g.Backends, secondary)
			},
			wantSub: "exactly one backends entry must set isDefault",
		},
		{
			name:    "s3-type-without-s3-block",
			mutate:  func(g *ServiceGlanceSpec) { g.Backends[0].S3 = nil },
			wantSub: "the s3 block must be set exactly when type is S3",
		},
		{
			name:   "one-default-s3-entry",
			mutate: func(*ServiceGlanceSpec) {},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cp := stamp(glanceControlPlane(), "cp", newNamespace(t, ctx, c, "celonly-glance-"))
			tc.mutate(cp.Spec.Services.Glance)
			expectAdmission(t, c.Create(ctx, cp), tc.wantSub)
		})
	}
}

// TestIntegration_CRD_CELOnly_ControlPlaneBarbicanSecretStore pins the barbican
// secretStore union: exactly one of dedicated or external.
//
// Rules: controlplane_types.go:1530
func TestIntegration_CRD_CELOnly_ControlPlaneBarbicanSecretStore(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	c, ctx, _ := setupEnvTestNoWebhook(t)

	const exactlyOne = "exactly one of dedicated or external must be set"
	for _, tc := range []struct {
		name    string
		store   ServiceBarbicanSecretStoreSpec
		wantSub string // empty: the object must be admitted
	}{
		{name: "neither", store: ServiceBarbicanSecretStoreSpec{}, wantSub: exactlyOne},
		{
			name: "both",
			store: ServiceBarbicanSecretStoreSpec{
				Dedicated: &BarbicanDedicatedSecretStoreSpec{},
				External:  externalBarbicanStore(),
			},
			wantSub: exactlyOne,
		},
		{name: "dedicated-only", store: ServiceBarbicanSecretStoreSpec{Dedicated: &BarbicanDedicatedSecretStoreSpec{}}},
		{name: "external-only", store: ServiceBarbicanSecretStoreSpec{External: externalBarbicanStore()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cp := stamp(barbicanControlPlane(), "cp", newNamespace(t, ctx, c, "celonly-barbican-"))
			cp.Spec.Services.Barbican.SecretStore = tc.store
			expectAdmission(t, c.Create(ctx, cp), tc.wantSub)
		})
	}
}

// TestIntegration_CRD_CELOnly_ControlPlaneCinderBackends pins the cinder
// backend and backup-backend rules: the driver block matches the type, the
// cephx user carries no client. prefix, a volume backend keeps its type per
// name, and a backup backend keeps its type while its name is unchanged.
//
// Rules: controlplane_types.go:1971, 1972, 1973, 2087, 2156, 2246, 2247, 2248
func TestIntegration_CRD_CELOnly_ControlPlaneCinderBackends(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	c, ctx, _ := setupEnvTestNoWebhook(t)

	const (
		nfsBlock = "the nfs block must be set exactly when type is NFS"
		rbdBlock = "the rbd block must be set exactly when type is RBD"
	)
	clientPrefixedRBD := cinderRBDEntry()
	clientPrefixedRBD.RBD.User = "client.cinder"
	clientPrefixedBackup := cinderBackupRBDEntry()
	clientPrefixedBackup.RBD.User = "client.cinder-backup"

	for _, tc := range []struct {
		name    string
		mutate  func(s *ServiceCinderSpec)
		wantSub string
	}{
		{
			name:    "nfs-entry-without-nfs",
			mutate:  func(s *ServiceCinderSpec) { s.Backends = []CinderBackendEntry{{Name: "nfs1", Type: "NFS"}} },
			wantSub: nfsBlock,
		},
		{
			name:    "rbd-entry-without-rbd",
			mutate:  func(s *ServiceCinderSpec) { s.Backends = []CinderBackendEntry{{Name: "rbd1", Type: "RBD"}} },
			wantSub: rbdBlock,
		},
		{
			name:    "rbd-entry-client-prefixed-user",
			mutate:  func(s *ServiceCinderSpec) { s.Backends = []CinderBackendEntry{clientPrefixedRBD} },
			wantSub: "user is the cephx name without its client. prefix (write cinder, not client.cinder)",
		},
		{
			name:    "nfs-backup-without-nfs",
			mutate:  func(s *ServiceCinderSpec) { s.BackupBackend = &CinderBackupBackendEntry{Name: "bk", Type: "NFS"} },
			wantSub: nfsBlock,
		},
		{
			name:    "rbd-backup-without-rbd",
			mutate:  func(s *ServiceCinderSpec) { s.BackupBackend = &CinderBackupBackendEntry{Name: "bk", Type: "RBD"} },
			wantSub: rbdBlock,
		},
		{
			name:    "rbd-backup-client-prefixed-user",
			mutate:  func(s *ServiceCinderSpec) { s.BackupBackend = clientPrefixedBackup },
			wantSub: "user is the cephx name without its client. prefix (write cinder-backup, not client.cinder-backup)",
		},
	} {
		t.Run("create-rejects-"+tc.name, func(t *testing.T) {
			cp := stamp(cinderControlPlane(), "cp", newNamespace(t, ctx, c, "celonly-cinder-"))
			tc.mutate(cp.Spec.Services.Cinder)
			expectAdmission(t, c.Create(ctx, cp), tc.wantSub)
		})
	}

	nfsBackup := func() *ControlPlane {
		cp := cinderControlPlane()
		cp.Spec.Services.Cinder.BackupBackend = &CinderBackupBackendEntry{
			Name: "bk",
			Type: "NFS",
			NFS:  &NFSShareSpec{Server: "nfs-server.openstack.svc.cluster.local", Path: "/backups"},
		}
		return cp
	}
	for _, tc := range []struct {
		name    string
		base    func() *ControlPlane
		mutate  func(s *ServiceCinderSpec)
		wantSub string // empty: the update must be admitted
	}{
		{
			name: "backend-type-change",
			base: cinderControlPlane,
			mutate: func(s *ServiceCinderSpec) {
				rbd := cinderRBDEntry()
				rbd.Name = "nfs1"
				s.Backends[0] = rbd
			},
			wantSub: "type is immutable: the volumes on a backend were created by its driver; declare the other driver under a new name",
		},
		{
			name: "backup-type-change-under-the-same-name",
			base: nfsBackup,
			mutate: func(s *ServiceCinderSpec) {
				s.BackupBackend = cinderBackupRBDEntry()
				s.BackupBackend.Name = "bk"
			},
			wantSub: "type is immutable while name is unchanged: a restore reads a backup with the driver that wrote it; rename the entry to switch the driver",
		},
		{
			name:   "backup-driver-switch-under-a-new-name",
			base:   nfsBackup,
			mutate: func(s *ServiceCinderSpec) { s.BackupBackend = cinderBackupRBDEntry() },
		},
		{
			name:   "backend-nfs-path-change",
			base:   cinderControlPlane,
			mutate: func(s *ServiceCinderSpec) { s.Backends[0].NFS.Path = "/volumes2" },
		},
	} {
		t.Run("update-"+tc.name, func(t *testing.T) {
			cp := stamp(tc.base(), "cp", newNamespace(t, ctx, c, "celonly-cinder-"))
			expectUpdate(t, ctx, c, cp, func(cp *ControlPlane) { tc.mutate(cp.Spec.Services.Cinder) }, tc.wantSub)
		})
	}
}

// TestIntegration_CRD_CELOnly_ControlPlaneNovaConsoleProxy pins the nova
// consoleProxy rule: a gateway is rejected only beside an explicit
// enabled: false.
//
// Rules: controlplane_types.go:2537
func TestIntegration_CRD_CELOnly_ControlPlaneNovaConsoleProxy(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	c, ctx, _ := setupEnvTestNoWebhook(t)

	for _, tc := range []struct {
		name    string
		enabled *bool
		wantSub string // empty: the object must be admitted
	}{
		{name: "gateway-while-disabled", enabled: ptr.To(false), wantSub: "gateway must not be set when consoleProxy.enabled is false"},
		{name: "gateway-while-enabled", enabled: ptr.To(true)},
		{name: "gateway-with-enabled-unset"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cp := stamp(novaControlPlane(), "cp", newNamespace(t, ctx, c, "celonly-nova-"))
			cp.Spec.Services.Nova.ConsoleProxy = &ServiceNovaConsoleProxySpec{Enabled: tc.enabled, Gateway: novaGateway()}
			expectAdmission(t, c.Create(ctx, cp), tc.wantSub)
		})
	}
}

// TestIntegration_CRD_CELOnly_ControlPlaneSizing pins the sizing rules: profile
// and profileRef are mutually exclusive, and an API component takes either
// autoscaling or verticalAutoscaling. The two API-component markers render a
// second time into the SizingProfile CRD through its inline SizingSpec; they
// are the same Go markers and are proven once, here.
//
// Rules: sizing_types.go:71, 193, 204
func TestIntegration_CRD_CELOnly_ControlPlaneSizing(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	c, ctx, _ := setupEnvTestNoWebhook(t)

	const bothScalers = "autoscaling and verticalAutoscaling cannot both be set on one component"
	hpa := &commonv1.AutoscalingSpec{MaxReplicas: 3, TargetCPUUtilization: ptr.To[int32](80)}
	vpa := &commonv1.VerticalAutoscalingSpec{UpdateMode: "Off"}
	keystoneAPI := func(a *commonv1.AutoscalingSpec, v *commonv1.VerticalAutoscalingSpec) *ControlPlaneSizingSpec {
		return &ControlPlaneSizingSpec{SizingSpec: SizingSpec{Keystone: &KeystoneSizingSpec{API: &APISizingSpec{
			DeploymentSizingSpec: DeploymentSizingSpec{VerticalAutoscaling: v},
			Autoscaling:          a,
		}}}}
	}
	horizonAPI := func(a *commonv1.AutoscalingSpec, v *commonv1.VerticalAutoscalingSpec) *ControlPlaneSizingSpec {
		return &ControlPlaneSizingSpec{SizingSpec: SizingSpec{Horizon: &HorizonSizingSpec{API: &HorizonAPISizingSpec{
			DeploymentSizingSpec: DeploymentSizingSpec{VerticalAutoscaling: v},
			Autoscaling:          a,
		}}}}
	}

	for _, tc := range []struct {
		name    string
		sizing  *ControlPlaneSizingSpec
		wantSub string // empty: the object must be admitted
	}{
		{
			name:    "profile-and-profile-ref",
			sizing:  &ControlPlaneSizingSpec{Profile: SizingProfileMinimal, ProfileRef: &SizingProfileRef{Name: "custom"}},
			wantSub: "profile and profileRef are mutually exclusive",
		},
		{name: "keystone-api-both-scalers", sizing: keystoneAPI(hpa, vpa), wantSub: bothScalers},
		{name: "horizon-api-both-scalers", sizing: horizonAPI(hpa, vpa), wantSub: bothScalers},
		{name: "profile-only", sizing: &ControlPlaneSizingSpec{Profile: SizingProfileMinimal}},
		{name: "profile-ref-only", sizing: &ControlPlaneSizingSpec{ProfileRef: &SizingProfileRef{Name: "custom"}}},
		{name: "keystone-api-autoscaling-only", sizing: keystoneAPI(hpa, nil)},
		{name: "keystone-api-vertical-autoscaling-only", sizing: keystoneAPI(nil, vpa)},
		{name: "horizon-api-autoscaling-only", sizing: horizonAPI(hpa, nil)},
		{name: "horizon-api-vertical-autoscaling-only", sizing: horizonAPI(nil, vpa)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cp := stamp(validControlPlane(), "cp", newNamespace(t, ctx, c, "celonly-sizing-"))
			cp.Spec.Sizing = tc.sizing
			expectAdmission(t, c.Create(ctx, cp), tc.wantSub)
		})
	}
}

// TestIntegration_CRD_CELOnly_CredentialRotation pins the keystoneService
// pairing of a CredentialRotation: required for the serviceAccountPassword
// target and forbidden for any other. The kind has no webhook, so these rules
// are all there is.
//
// Rules: credentialrotation_types.go:57, 58
func TestIntegration_CRD_CELOnly_CredentialRotation(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	c, ctx, _ := setupEnvTestNoWebhook(t)

	for _, tc := range []struct {
		name    string
		spec    CredentialRotationSpec
		wantSub string // empty: the object must be admitted
	}{
		{
			name:    "service-account-password-without-keystone-service",
			spec:    CredentialRotationSpec{Target: RotationTargetServiceAccountPassword},
			wantSub: "keystoneService is required when target is serviceAccountPassword",
		},
		{
			name:    "admin-application-credential-with-keystone-service",
			spec:    CredentialRotationSpec{Target: RotationTargetAdminApplicationCredential, KeystoneService: "glance"},
			wantSub: "keystoneService may only be set when target is serviceAccountPassword",
		},
		{
			name: "admin-application-credential",
			spec: CredentialRotationSpec{Target: RotationTargetAdminApplicationCredential},
		},
		{
			name: "service-account-password-with-keystone-service",
			spec: CredentialRotationSpec{Target: RotationTargetServiceAccountPassword, KeystoneService: "glance"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rotation := stamp(&CredentialRotation{Spec: tc.spec}, "rotation", newNamespace(t, ctx, c, "celonly-rotation-"))
			expectAdmission(t, c.Create(ctx, rotation), tc.wantSub)
		})
	}
}

// TestIntegration_CRD_CELOnly_KeystoneService pins the CEL rules of the
// KeystoneService CRD. The kind has a validating webhook, so the controller
// suite's TestIntegration_KeystoneService_SchemaValidation, which runs with it
// and mixes webhook rejections into its table, cannot attribute a rejection to
// the CRD; this test runs without it.
//
// Rules: keystoneservice_types.go:55, 68, 122, 123, 148, 217, 236, 262, 263
func TestIntegration_CRD_CELOnly_KeystoneService(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	c, ctx, _ := setupEnvTestNoWebhook(t)

	// The base spells out account.domainName so the field exists in oldSelf
	// and its transition rule runs.
	base := func(ns string) *KeystoneService {
		ks := stamp(validKeystoneService(), "ks", ns)
		ks.Spec.Account.DomainName = "Default"
		return ks
	}

	for _, tc := range []struct {
		name    string
		mutate  func(ks *KeystoneService)
		wantSub string // empty: the object must be admitted
	}{
		{
			name: "neither-catalog-nor-account",
			mutate: func(ks *KeystoneService) {
				ks.Spec.Catalog = nil
				ks.Spec.Account = nil
			},
			wantSub: "at least one of spec.catalog or spec.account must be set",
		},
		{
			name:    "identity-service-type",
			mutate:  func(ks *KeystoneService) { ks.Spec.Catalog.ServiceType = "identity" },
			wantSub: "the identity catalog entry is ControlPlane-owned and cannot be registered through a KeystoneService",
		},
		{
			name:   "catalog-and-account",
			mutate: func(*KeystoneService) {},
		},
	} {
		t.Run("create-"+tc.name, func(t *testing.T) {
			ks := base(newNamespace(t, ctx, c, "celonly-ks-"))
			tc.mutate(ks)
			expectAdmission(t, c.Create(ctx, ks), tc.wantSub)
		})
	}

	for _, tc := range []struct {
		name    string
		mutate  func(ks *KeystoneService)
		wantSub string // empty: the update must be admitted
	}{
		{
			name:    "controlplane-ref-name",
			mutate:  func(ks *KeystoneService) { ks.Spec.ControlPlaneRef.Name = "other" },
			wantSub: "controlPlaneRef.name is immutable; delete and re-create the KeystoneService to register against another ControlPlane",
		},
		{
			name:    "service-type",
			mutate:  func(ks *KeystoneService) { ks.Spec.Catalog.ServiceType = "volume" },
			wantSub: "serviceType is immutable; delete and re-create the KeystoneService to register a different service type",
		},
		{
			name:    "service-name",
			mutate:  func(ks *KeystoneService) { ks.Spec.Catalog.ServiceName = "glance2" },
			wantSub: "serviceName is immutable; delete and re-create the KeystoneService to rename its catalog entry",
		},
		{
			name:    "user-name",
			mutate:  func(ks *KeystoneService) { ks.Spec.Account.UserName = "glance-user2" },
			wantSub: "userName is immutable; delete and re-create the KeystoneService to rename its user",
		},
		{
			name:    "domain-name",
			mutate:  func(ks *KeystoneService) { ks.Spec.Account.DomainName = "other" },
			wantSub: "domainName is immutable; delete and re-create the KeystoneService to move it to another domain",
		},
		{
			name:    "project-name",
			mutate:  func(ks *KeystoneService) { ks.Spec.Account.Project.Name = "service2" },
			wantSub: "project.name is immutable; delete and re-create the KeystoneService to re-point its project",
		},
		{
			name:    "project-create",
			mutate:  func(ks *KeystoneService) { ks.Spec.Account.Project.Create = true },
			wantSub: "project.create is immutable; a managed<->referenced flip would orphan or adopt the live project",
		},
		{
			name: "appended-internal-endpoint",
			mutate: func(ks *KeystoneService) {
				ks.Spec.Catalog.Endpoints = append(ks.Spec.Catalog.Endpoints, KeystoneServiceEndpointSpec{
					Interface: ExternalEndpointTypeInternal,
					URL:       "http://glance-api.openstack.svc:9292",
				})
			},
		},
	} {
		t.Run("update-"+tc.name, func(t *testing.T) {
			expectUpdate(t, ctx, c, base(newNamespace(t, ctx, c, "celonly-ks-")), tc.mutate, tc.wantSub)
		})
	}

	// catalog.registeredLimits goes through unstructured objects, because a Go
	// int32 cannot carry 2147483648, the first value past the Maximum marker.
	for _, tc := range []struct {
		name    string
		limits  []any
		wantSub string // empty: the object must be admitted
	}{
		{name: "limit-bounds", limits: []any{
			map[string]any{"resourceName": "r1", "defaultLimit": int64(-1)},
			map[string]any{"resourceName": "r2", "defaultLimit": int64(2147483647)},
		}},
		{name: "limit-above-int32", limits: []any{
			map[string]any{"resourceName": "r1", "defaultLimit": int64(2147483648)},
		}, wantSub: "should be less than or equal to 2147483647"},
		{name: "limit-below-minus-one", limits: []any{
			map[string]any{"resourceName": "r1", "defaultLimit": int64(-2)},
		}, wantSub: "should be greater than or equal to -1"},
		{name: "limit-zero", limits: []any{
			map[string]any{"resourceName": "r1", "defaultLimit": int64(0)},
		}, wantSub: "defaultLimit 0 is refused by K-ORC when it creates a registered limit; use a positive bound"},
		{name: "limit-resourcename-whitespace", limits: []any{
			map[string]any{"resourceName": "image count", "defaultLimit": int64(1)},
		}, wantSub: "should match"},
		{name: "limit-duplicate-resourcename", limits: []any{
			map[string]any{"resourceName": "r1", "defaultLimit": int64(1)},
			map[string]any{"resourceName": "r1", "defaultLimit": int64(2)},
		}, wantSub: "Duplicate value"},
	} {
		t.Run("create-"+tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(base(newNamespace(t, ctx, c, "celonly-ks-")))
			g.Expect(err).NotTo(HaveOccurred())
			u := &unstructured.Unstructured{Object: raw}
			u.SetGroupVersionKind(GroupVersion.WithKind("KeystoneService"))
			g.Expect(unstructured.SetNestedSlice(u.Object, tc.limits, "spec", "catalog", "registeredLimits")).To(Succeed())
			expectAdmission(t, c.Create(ctx, u), tc.wantSub)
		})
	}
}

// --- Order kinds (no webhook on any cluster) ---

// orderObject returns an order of kind as an unstructured object, so a case
// can send the shapes a typed client never would: an empty userName, a zero
// passwordGeneration, a controlPlaneRef without a name.
func orderObject(kind, name, namespace string, spec map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	u.SetGroupVersionKind(GroupVersion.WithKind(kind))
	u.SetName(name)
	u.SetNamespace(namespace)
	return u
}

// specWith returns a deep copy of base with field set to value.
func specWith(base map[string]any, field string, value any) map[string]any {
	spec := runtime.DeepCopyJSON(base)
	spec[field] = value
	return spec
}

// specWithout returns a deep copy of base without field.
func specWithout(base map[string]any, field string) map[string]any {
	spec := runtime.DeepCopyJSON(base)
	delete(spec, field)
	return spec
}

// TestIntegration_CRD_CELOnly_KeystoneUser pins every admission rule of the
// KeystoneUser CRD against the real envtest API server. The kind has no webhook,
// because a target cluster runs none, so these rules are all there is; the
// substrings are the ones the invalid-keystoneuser-cr chainsaw corpus asserts on.
//
// Rules: keystoneuser_types.go:18, 19, 96, 97, 121, 140, 141
func TestIntegration_CRD_CELOnly_KeystoneUser(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)

	const kind = "KeystoneUser"
	ref := map[string]any{"name": "cp"}
	createCases := []struct {
		name       string
		objName    string
		spec       map[string]any
		wantErrSub string
	}{
		{name: "a comma in userName", spec: map[string]any{"controlPlaneRef": ref, "userName": "a,b"}, wantErrSub: "should match"},
		{name: "an empty userName", spec: map[string]any{"controlPlaneRef": ref, "userName": ""}, wantErrSub: "should be at least 1 chars"},
		{name: "a 256-byte userName", spec: map[string]any{"controlPlaneRef": ref, "userName": strings.Repeat("a", 256)}, wantErrSub: "Too long"},
		{
			name: "a zero passwordGeneration", spec: map[string]any{"controlPlaneRef": ref, "passwordGeneration": int64(0)},
			wantErrSub: "should be greater than or equal to 1",
		},
		{
			name: "a 64-byte metadata.name", objName: strings.Repeat("a", 64), spec: map[string]any{"controlPlaneRef": ref},
			wantErrSub: "metadata.name must be at most 63 bytes",
		},
		{name: "a controlPlaneRef without a name", spec: map[string]any{"controlPlaneRef": map[string]any{}}, wantErrSub: "Required value"},
		{
			name: "a controlPlaneRef namespace outside the label shape",
			spec: map[string]any{"controlPlaneRef": map[string]any{"name": "cp", "namespace": "Bad_NS"}}, wantErrSub: "should match",
		},
		{
			name: "a 30s rotation.interval", spec: map[string]any{"controlPlaneRef": ref, "rotation": map[string]any{"interval": "30s"}},
			wantErrSub: "rotation.interval is 0s or at least 1m",
		},
		{
			name: "an 87601h rotation.interval", spec: map[string]any{"controlPlaneRef": ref, "rotation": map[string]any{"interval": "87601h"}},
			wantErrSub: "rotation.interval is at most 87600h",
		},
	}
	for i, tc := range createCases {
		t.Run("create rejects "+tc.name, func(t *testing.T) {
			if tc.wantErrSub == "" {
				t.Fatalf("create case %q needs a wantErrSub: a create case is always a rejection", tc.name)
			}
			ns := newNamespace(t, ctx, c, "test-ku-cel-")
			err := c.Create(ctx, orderObject(kind, cmp.Or(tc.objName, fmt.Sprintf("ku-%d", i)), ns, tc.spec))
			expectAdmission(t, err, tc.wantErrSub)
		})
	}

	t.Run("a minimal order is admitted with passwordGeneration 1", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ns := newNamespace(t, ctx, c, "test-ku-cel-")

		g.Expect(c.Create(ctx, orderObject(kind, strings.Repeat("a", 63), ns,
			map[string]any{"controlPlaneRef": ref}))).To(Succeed(), "a 63-byte name is admitted")
		stored := &KeystoneUser{}
		g.Expect(c.Get(ctx, types.NamespacedName{Namespace: ns, Name: strings.Repeat("a", 63)}, stored)).To(Succeed())
		g.Expect(stored.Spec.PasswordGeneration).To(Equal(int64(1)))
		g.Expect(stored.Spec.UserName).To(BeEmpty(), "the userName default is the reconciler's, not the schema's")

		raw := &unstructured.Unstructured{}
		raw.SetGroupVersionKind(GroupVersion.WithKind(kind))
		g.Expect(c.Get(ctx, client.ObjectKeyFromObject(stored), raw)).To(Succeed())
		g.Expect(raw.Object["spec"].(map[string]any)["rotation"]).To(Equal(map[string]any{"interval": "720h"}),
			"an order without the block stores the interval default")
	})

	t.Run("the interval bounds are admitted", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ns := newNamespace(t, ctx, c, "test-ku-cel-")

		for i, interval := range []string{"0s", "1m", "87600h"} {
			g.Expect(c.Create(ctx, orderObject(kind, fmt.Sprintf("bound-%d", i), ns, map[string]any{
				"controlPlaneRef": ref, "rotation": map[string]any{"interval": interval},
			}))).To(Succeed(), "rotation.interval %s is admitted", interval)
		}
	})

	t.Run("a typed order with the block left zero stores the default", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ns := newNamespace(t, ctx, c, "test-ku-cel-")
		order := &KeystoneUser{
			ObjectMeta: metav1.ObjectMeta{Name: "zero", Namespace: ns},
			Spec:       KeystoneUserSpec{ControlPlaneRef: ControlPlaneRefSpec{Name: "cp"}},
		}
		g.Expect(c.Create(ctx, order)).To(Succeed())

		stored := &KeystoneUser{}
		g.Expect(c.Get(ctx, client.ObjectKeyFromObject(order), stored)).To(Succeed())
		g.Expect(stored.Spec.Rotation.Interval).To(Equal(&metav1.Duration{Duration: 720 * time.Hour}))
	})

	updateCases := []struct {
		name       string
		mutate     func(*KeystoneUser)
		wantErrSub string
	}{
		{
			name:       "a changed controlPlaneRef.name",
			mutate:     func(o *KeystoneUser) { o.Spec.ControlPlaneRef.Name = "other" },
			wantErrSub: "controlPlaneRef.name is immutable; delete and re-create the KeystoneUser to move it to another ControlPlane",
		},
		{
			name:       "a changed controlPlaneRef.namespace",
			mutate:     func(o *KeystoneUser) { o.Spec.ControlPlaneRef.Namespace = "other" },
			wantErrSub: "controlPlaneRef.namespace is immutable; delete and re-create the KeystoneUser to move it to another ControlPlane",
		},
		{
			name:       "a removed controlPlaneRef.namespace",
			mutate:     func(o *KeystoneUser) { o.Spec.ControlPlaneRef.Namespace = "" },
			wantErrSub: "controlPlaneRef.namespace is immutable",
		},
		{
			name:       "a changed explicit userName",
			mutate:     func(o *KeystoneUser) { o.Spec.UserName = "other-user" },
			wantErrSub: "userName is immutable; delete and re-create the KeystoneUser to rename its user",
		},
		{
			name:       "a lowered passwordGeneration",
			mutate:     func(o *KeystoneUser) { o.Spec.PasswordGeneration = 1 },
			wantErrSub: "passwordGeneration may only increase",
		},
	}
	for i, tc := range updateCases {
		t.Run("update rejects "+tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			ns := newNamespace(t, ctx, c, "test-ku-cel-")
			order := &KeystoneUser{
				ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("base-%d", i), Namespace: ns},
				Spec: KeystoneUserSpec{
					ControlPlaneRef:    ControlPlaneRefSpec{Name: "cp", Namespace: "cp-ns"},
					UserName:           "svc-user",
					PasswordGeneration: 2,
				},
			}
			g.Expect(c.Create(ctx, order)).To(Succeed())

			tc.mutate(order)
			expectAdmission(t, c.Update(ctx, order), tc.wantErrSub)
		})
	}

	// The userName rule compares the EFFECTIVE name, so spelling out the
	// metadata.name default is not a rename, and naming anything else is.
	t.Run("update compares the effective userName", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ns := newNamespace(t, ctx, c, "test-ku-cel-")
		order := &KeystoneUser{
			ObjectMeta: metav1.ObjectMeta{Name: "workflow", Namespace: ns},
			Spec:       KeystoneUserSpec{ControlPlaneRef: ControlPlaneRefSpec{Name: "cp"}},
		}
		g.Expect(c.Create(ctx, order)).To(Succeed())

		order.Spec.UserName = "other-user"
		err := c.Update(ctx, order)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("userName is immutable"))

		g.Expect(c.Get(ctx, client.ObjectKeyFromObject(order), order)).To(Succeed())
		order.Spec.UserName = "workflow"
		order.Spec.PasswordGeneration = 3
		g.Expect(c.Update(ctx, order)).To(Succeed(), "the default spelled out and a raised generation are admitted")
	})
}

// orderSchemaCase is one admission case of an order kind's schema test: a
// create from spec, or an update of the stored base object through mutate.
type orderSchemaCase struct {
	name       string
	objName    string
	spec       map[string]any
	mutate     func(spec map[string]any)
	wantErrSub string
}

// runOrderSchemaCases creates each create case as an unstructured object of
// kind and expects it rejected with wantErrSub, admits minimal under a 63-byte
// name, then creates base once per update case, applies mutate to its spec and
// expects the update rejected the same way, or admitted when wantErrSub is
// empty. The order kinds have no webhook, so these schema rules are all there
// is.
func runOrderSchemaCases(
	t *testing.T, ctx context.Context, c client.Client, kind string,
	createCases []orderSchemaCase, minimal, base map[string]any, updateCases []orderSchemaCase,
) {
	t.Helper()
	for i, tc := range createCases {
		t.Run("create rejects "+tc.name, func(t *testing.T) {
			if tc.wantErrSub == "" {
				t.Fatalf("create case %q needs a wantErrSub: a create case is always a rejection", tc.name)
			}
			ns := newNamespace(t, ctx, c, "test-order-cel-")
			err := c.Create(ctx, orderObject(kind, cmp.Or(tc.objName, fmt.Sprintf("order-%d", i)), ns, tc.spec))
			expectAdmission(t, err, tc.wantErrSub)
		})
	}
	t.Run("a minimal order with a 63-byte name is admitted", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ns := newNamespace(t, ctx, c, "test-order-cel-")
		g.Expect(c.Create(ctx, orderObject(kind, strings.Repeat("a", 63), ns, minimal))).To(Succeed())
	})
	for i, tc := range updateCases {
		verb := "update rejects "
		if tc.wantErrSub == "" {
			verb = "update admits "
		}
		t.Run(verb+tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			ns := newNamespace(t, ctx, c, "test-order-cel-")
			stored := orderObject(kind, fmt.Sprintf("base-%d", i), ns, runtime.DeepCopyJSON(base))
			g.Expect(c.Create(ctx, stored)).To(Succeed())

			tc.mutate(stored.Object["spec"].(map[string]any))
			expectAdmission(t, c.Update(ctx, stored), tc.wantErrSub)
		})
	}
}

// orderControlPlaneRefCases are the create rejections and update rejections of
// the controlPlaneRef every order kind carries, with its messages naming kind.
func orderControlPlaneRefCases(kind string, base map[string]any) (create, update []orderSchemaCase) {
	create = []orderSchemaCase{
		{
			name: "a 64-byte metadata.name", objName: strings.Repeat("a", 64), spec: runtime.DeepCopyJSON(base),
			wantErrSub: "metadata.name must be at most 63 bytes",
		},
		{name: "a controlPlaneRef without a name", spec: specWith(base, "controlPlaneRef", map[string]any{}), wantErrSub: "Required value"},
		{
			name:       "a controlPlaneRef namespace outside the label shape",
			spec:       specWith(base, "controlPlaneRef", map[string]any{"name": "cp", "namespace": "Bad_NS"}),
			wantErrSub: "should match",
		},
	}
	update = []orderSchemaCase{
		{
			name: "a changed controlPlaneRef.name",
			mutate: func(spec map[string]any) {
				spec["controlPlaneRef"].(map[string]any)["name"] = "other"
			},
			wantErrSub: "controlPlaneRef.name is immutable; delete and re-create the " + kind +
				" to move it to another ControlPlane",
		},
		{
			name: "a changed controlPlaneRef.namespace",
			mutate: func(spec map[string]any) {
				spec["controlPlaneRef"].(map[string]any)["namespace"] = "other"
			},
			wantErrSub: "controlPlaneRef.namespace is immutable; delete and re-create the " + kind +
				" to move it to another ControlPlane",
		},
	}
	return create, update
}

// TestIntegration_CRD_CELOnly_KeystoneProject pins every admission rule of the
// KeystoneProject CRD against the real envtest API server; the substrings are
// the ones the invalid-keystoneproject-cr chainsaw corpus asserts on.
//
// Rules: keystoneproject_types.go:17, 18, 80, 81
func TestIntegration_CRD_CELOnly_KeystoneProject(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)

	const kind = "KeystoneProject"
	ref := map[string]any{"name": "cp", "namespace": "cp-ns"}
	base := map[string]any{"controlPlaneRef": ref, "projectName": "svc-project"}
	create, update := orderControlPlaneRefCases(kind, base)
	create = append(create,
		orderSchemaCase{name: "a comma in projectName", spec: map[string]any{"controlPlaneRef": ref, "projectName": "a,b"}, wantErrSub: "should match"},
		orderSchemaCase{name: "an empty projectName", spec: map[string]any{"controlPlaneRef": ref, "projectName": ""}, wantErrSub: "should be at least 1 chars"},
		orderSchemaCase{
			name: "a 256-byte projectName", spec: map[string]any{"controlPlaneRef": ref, "projectName": strings.Repeat("a", 256)},
			wantErrSub: "Too long",
		},
	)
	update = append(update,
		orderSchemaCase{
			name:       "a changed explicit projectName",
			mutate:     func(spec map[string]any) { spec["projectName"] = "other-project" },
			wantErrSub: "projectName is immutable; delete and re-create the KeystoneProject to rename its project",
		},
	)
	runOrderSchemaCases(t, ctx, c, kind, create, map[string]any{"controlPlaneRef": map[string]any{"name": "cp"}},
		base, update)
}

// TestIntegration_CRD_CELOnly_KeystoneRoleAssignment pins every admission rule
// of the KeystoneRoleAssignment CRD; the substrings are the ones the
// invalid-keystoneroleassignment-cr chainsaw corpus asserts on.
//
// Rules: keystoneroleassignment_types.go:19, 89, 90, 95, 100, 109
func TestIntegration_CRD_CELOnly_KeystoneRoleAssignment(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)

	const kind = "KeystoneRoleAssignment"
	base := map[string]any{
		"controlPlaneRef": map[string]any{"name": "cp", "namespace": "cp-ns"},
		"userRef":         map[string]any{"name": "workflow"},
		"projectRef":      map[string]any{"name": "workflow-project"},
		"role":            "member",
	}
	create, update := orderControlPlaneRefCases(kind, base)
	create = append(create,
		orderSchemaCase{name: "a missing role", spec: specWithout(base, "role"), wantErrSub: "Required value"},
		orderSchemaCase{name: "a missing userRef", spec: specWithout(base, "userRef"), wantErrSub: "Required value"},
		orderSchemaCase{name: "a missing projectRef", spec: specWithout(base, "projectRef"), wantErrSub: "Required value"},
		orderSchemaCase{
			name: "an empty userRef.name", spec: specWith(base, "userRef", map[string]any{"name": ""}),
			wantErrSub: "should be at least 1 chars",
		},
		orderSchemaCase{name: "a comma in role", spec: specWith(base, "role", "a,b"), wantErrSub: "should match"},
		orderSchemaCase{name: "an empty role", spec: specWith(base, "role", ""), wantErrSub: "should be at least 1 chars"},
		orderSchemaCase{name: "a 256-byte role", spec: specWith(base, "role", strings.Repeat("r", 256)), wantErrSub: "Too long"},
	)
	update = append(update,
		orderSchemaCase{
			name:       "a changed userRef",
			mutate:     func(spec map[string]any) { spec["userRef"] = map[string]any{"name": "other"} },
			wantErrSub: "userRef is immutable; delete and re-create the KeystoneRoleAssignment to bind another user",
		},
		orderSchemaCase{
			name:       "a changed projectRef",
			mutate:     func(spec map[string]any) { spec["projectRef"] = map[string]any{"name": "other"} },
			wantErrSub: "projectRef is immutable; delete and re-create the KeystoneRoleAssignment to bind another project",
		},
		orderSchemaCase{
			name:       "a changed role",
			mutate:     func(spec map[string]any) { spec["role"] = "reader" },
			wantErrSub: "role is immutable; delete and re-create the KeystoneRoleAssignment to assign another role",
		},
	)
	minimal := specWith(base, "controlPlaneRef", map[string]any{"name": "cp"})
	runOrderSchemaCases(t, ctx, c, kind, create, minimal, base, update)
}

// TestIntegration_CRD_CELOnly_KeystoneCatalogEntry pins every admission rule of
// the KeystoneCatalogEntry CRD; the substrings are the ones the
// invalid-keystonecatalogentry-cr chainsaw corpus asserts on.
//
// Rules: keystonecatalogentry_types.go:17, 18, 82, 83, 92, 93
func TestIntegration_CRD_CELOnly_KeystoneCatalogEntry(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)

	const kind = "KeystoneCatalogEntry"
	public := map[string]any{"interface": "public", "url": "https://dns.example.test/v2"}
	base := map[string]any{
		"controlPlaneRef": map[string]any{"name": "cp", "namespace": "cp-ns"},
		"serviceType":     "dns",
		"serviceName":     "svc-dns",
		"endpoints":       []any{public},
	}
	create, update := orderControlPlaneRefCases(kind, base)
	create = append(create,
		orderSchemaCase{name: "a missing serviceType", spec: specWithout(base, "serviceType"), wantErrSub: "Required value"},
		orderSchemaCase{name: "serviceType identity", spec: specWith(base, "serviceType", "identity"), wantErrSub: "ControlPlane-owned"},
		orderSchemaCase{name: "an uppercase serviceType", spec: specWith(base, "serviceType", "DNS"), wantErrSub: "should match"},
		orderSchemaCase{name: "a comma in serviceName", spec: specWith(base, "serviceName", "a,b"), wantErrSub: "should match"},
		orderSchemaCase{name: "an empty serviceName", spec: specWith(base, "serviceName", ""), wantErrSub: "should be at least 1 chars"},
		orderSchemaCase{
			name: "an ftp endpoint URL", spec: specWith(base, "endpoints", []any{map[string]any{"interface": "public", "url": "ftp://x"}}),
			wantErrSub: "should match",
		},
		orderSchemaCase{
			name: "an unknown interface", spec: specWith(base, "endpoints", []any{map[string]any{"interface": "private", "url": "https://x"}}),
			wantErrSub: "Unsupported value",
		},
		orderSchemaCase{
			name: "two public endpoints", spec: specWith(base, "endpoints", []any{public, public}), wantErrSub: "Duplicate value",
		},
		orderSchemaCase{
			name: "a 1025-byte endpoint URL",
			spec: specWith(base, "endpoints", []any{map[string]any{
				"interface": "public", "url": "https://" + strings.Repeat("a", 1025-len("https://")),
			}}),
			wantErrSub: "Too long",
		},
	)
	update = append(update,
		orderSchemaCase{
			name:       "a changed serviceType",
			mutate:     func(spec map[string]any) { spec["serviceType"] = "image" },
			wantErrSub: "serviceType is immutable; delete and re-create the KeystoneCatalogEntry to register a different service type",
		},
		orderSchemaCase{
			name:       "a changed explicit serviceName",
			mutate:     func(spec map[string]any) { spec["serviceName"] = "other-dns" },
			wantErrSub: "serviceName is immutable; delete and re-create the KeystoneCatalogEntry to rename its catalog entry",
		},
		orderSchemaCase{
			name: "a changed endpoints list",
			mutate: func(spec map[string]any) {
				spec["endpoints"] = []any{
					map[string]any{"interface": "public", "url": "https://dns.example.test/v3"},
					map[string]any{"interface": "internal", "url": "http://designate-api.tenant.svc:9001/v2"},
				}
			},
		},
	)
	minimal := map[string]any{"controlPlaneRef": map[string]any{"name": "cp"}, "serviceType": "dns"}
	runOrderSchemaCases(t, ctx, c, kind, create, minimal, base, update)
}

// TestIntegration_CRD_CELOnly_KeystoneApplicationCredential pins every
// admission rule of the KeystoneApplicationCredential CRD, the CEL duration
// rules of spec.rotation included, and the defaults the server stores; the
// substrings are the ones the invalid-keystoneapplicationcredential-cr chainsaw
// corpus asserts on.
//
// Rules: keystoneapplicationcredential_types.go:20, 89, 90, 95, 100, 110, 121, 135, 136, 143
func TestIntegration_CRD_CELOnly_KeystoneApplicationCredential(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)

	const kind = "KeystoneApplicationCredential"
	base := map[string]any{
		"controlPlaneRef":      map[string]any{"name": "cp", "namespace": "cp-ns"},
		"userRef":              map[string]any{"name": "workflow"},
		"projectRef":           map[string]any{"name": "workflow-project"},
		"credentialGeneration": int64(2),
	}
	create, update := orderControlPlaneRefCases(kind, base)
	create = append(create,
		orderSchemaCase{name: "a missing userRef", spec: specWithout(base, "userRef"), wantErrSub: "Required value"},
		orderSchemaCase{name: "a missing projectRef", spec: specWithout(base, "projectRef"), wantErrSub: "Required value"},
		orderSchemaCase{
			name: "an empty userRef.name", spec: specWith(base, "userRef", map[string]any{"name": ""}),
			wantErrSub: "should be at least 1 chars",
		},
		orderSchemaCase{
			name: "a zero credentialGeneration", spec: specWith(base, "credentialGeneration", int64(0)),
			wantErrSub: "should be greater than or equal to 1",
		},
		orderSchemaCase{
			name: "a 30s rotation.interval", spec: specWith(base, "rotation", map[string]any{"interval": "30s"}),
			wantErrSub: "rotation.interval is 0s or at least 1m",
		},
		orderSchemaCase{
			name: "an 87601h rotation.interval", spec: specWith(base, "rotation", map[string]any{"interval": "87601h"}),
			wantErrSub: "rotation.interval is at most 87600h",
		},
		orderSchemaCase{
			name:       "a gracePeriod as long as the interval",
			spec:       specWith(base, "rotation", map[string]any{"interval": "1h", "gracePeriod": "1h"}),
			wantErrSub: "rotation.gracePeriod must be shorter than rotation.interval",
		},
		orderSchemaCase{
			name: "a negative gracePeriod", spec: specWith(base, "rotation", map[string]any{"gracePeriod": "-1h"}),
			wantErrSub: "rotation.gracePeriod is not negative",
		},
	)
	update = append(update,
		orderSchemaCase{
			name:       "a changed userRef",
			mutate:     func(spec map[string]any) { spec["userRef"] = map[string]any{"name": "other"} },
			wantErrSub: "userRef is immutable; delete and re-create the KeystoneApplicationCredential to bind another user",
		},
		orderSchemaCase{
			name:       "a changed projectRef",
			mutate:     func(spec map[string]any) { spec["projectRef"] = map[string]any{"name": "other"} },
			wantErrSub: "projectRef is immutable; delete and re-create the KeystoneApplicationCredential to bind another project",
		},
		orderSchemaCase{
			name:       "a lowered credentialGeneration",
			mutate:     func(spec map[string]any) { spec["credentialGeneration"] = int64(1) },
			wantErrSub: "credentialGeneration may only increase",
		},
		orderSchemaCase{
			name:   "a raised credentialGeneration",
			mutate: func(spec map[string]any) { spec["credentialGeneration"] = int64(5) },
		},
		orderSchemaCase{
			name: "a 0s interval with any non-negative gracePeriod",
			mutate: func(spec map[string]any) {
				spec["rotation"] = map[string]any{"interval": "0s", "gracePeriod": "2160h"}
			},
		},
		orderSchemaCase{
			name:   "a 0s interval with a 0s gracePeriod",
			mutate: func(spec map[string]any) { spec["rotation"] = map[string]any{"interval": "0s", "gracePeriod": "0s"} },
		},
		orderSchemaCase{
			name:   "the longest interval",
			mutate: func(spec map[string]any) { spec["rotation"] = map[string]any{"interval": "87600h"} },
		},
	)
	minimal := specWith(base, "controlPlaneRef", map[string]any{"name": "cp"})
	delete(minimal, "credentialGeneration")
	runOrderSchemaCases(t, ctx, c, kind, create, minimal, base, update)

	refs := KeystoneApplicationCredentialSpec{
		ControlPlaneRef: ControlPlaneRefSpec{Name: "cp"},
		UserRef:         KeystoneOrderRef{Name: "workflow"},
		ProjectRef:      KeystoneOrderRef{Name: "workflow-project"},
	}

	t.Run("an order without the optional fields stores the default literals", func(t *testing.T) {
		g := NewGomegaWithT(t)
		u := orderObject(kind, "minimal", newNamespace(t, ctx, c, "test-kac-default-"), runtime.DeepCopyJSON(minimal))
		g.Expect(c.Create(ctx, u)).To(Succeed())

		stored := &unstructured.Unstructured{}
		stored.SetGroupVersionKind(GroupVersion.WithKind(kind))
		g.Expect(c.Get(ctx, client.ObjectKeyFromObject(u), stored)).To(Succeed())
		spec := stored.Object["spec"].(map[string]any)
		g.Expect(spec["credentialGeneration"]).To(BeEquivalentTo(1))
		g.Expect(spec["rotation"]).To(Equal(map[string]any{"interval": "720h", "gracePeriod": "24h"}))
	})

	t.Run("a typed order stores the Go serialization of the durations", func(t *testing.T) {
		g := NewGomegaWithT(t)
		order := &KeystoneApplicationCredential{
			ObjectMeta: metav1.ObjectMeta{Name: "typed", Namespace: newNamespace(t, ctx, c, "test-kac-default-")},
			Spec:       *refs.DeepCopy(),
		}
		order.Spec.Rotation = KeystoneApplicationCredentialRotationSpec{
			Interval:    &metav1.Duration{Duration: 720 * time.Hour},
			GracePeriod: &metav1.Duration{Duration: 24 * time.Hour},
		}
		g.Expect(c.Create(ctx, order)).To(Succeed(), "the CEL rules accept 720h0m0s and 24h0m0s")

		stored := &unstructured.Unstructured{}
		stored.SetGroupVersionKind(GroupVersion.WithKind(kind))
		g.Expect(c.Get(ctx, client.ObjectKeyFromObject(order), stored)).To(Succeed())
		g.Expect(stored.Object["spec"].(map[string]any)["rotation"]).
			To(Equal(map[string]any{"interval": "720h0m0s", "gracePeriod": "24h0m0s"}))
	})

	t.Run("a typed order with the block left zero stores the defaults", func(t *testing.T) {
		g := NewGomegaWithT(t)
		order := &KeystoneApplicationCredential{
			ObjectMeta: metav1.ObjectMeta{Name: "zero", Namespace: newNamespace(t, ctx, c, "test-kac-default-")},
			Spec:       *refs.DeepCopy(),
		}
		g.Expect(c.Create(ctx, order)).To(Succeed())

		stored := &KeystoneApplicationCredential{}
		g.Expect(c.Get(ctx, client.ObjectKeyFromObject(order), stored)).To(Succeed())
		g.Expect(stored.Spec.CredentialGeneration).To(Equal(int64(1)))
		g.Expect(stored.Spec.Rotation.Interval).To(Equal(&metav1.Duration{Duration: 720 * time.Hour}))
		g.Expect(stored.Spec.Rotation.GracePeriod).To(Equal(&metav1.Duration{Duration: 24 * time.Hour}))
	})
}

// TestIntegration_CRD_CELOnly_MariaDBDatabase pins every admission rule of the
// MariaDBDatabase CRD against the real envtest API server, and the pattern of
// the ControlPlane's spec.infrastructure.publishedDatabaseEndpoint the kind
// reads. The kind has no webhook, so these schema rules are all there is; the
// substrings are the ones the invalid-mariadbdatabase-cr chainsaw corpus
// asserts on.
//
// Rules: mariadbdatabase_types.go:28, 29, 30, 117, 118
func TestIntegration_CRD_CELOnly_MariaDBDatabase(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)

	const kind = "MariaDBDatabase"
	ref := map[string]any{"name": "cp", "namespace": "cp-ns"}
	base := map[string]any{"controlPlaneRef": ref, "databaseName": "svc_db", "deletionPolicy": "Retain"}
	create, update := orderControlPlaneRefCases(kind, base)
	create = append(create,
		orderSchemaCase{name: "a dash in databaseName", spec: specWith(base, "databaseName", "my-db"), wantErrSub: "should match"},
		orderSchemaCase{name: "an empty databaseName", spec: specWith(base, "databaseName", ""), wantErrSub: "should be at least 1 chars"},
		orderSchemaCase{
			name: "a 65-byte databaseName", spec: specWith(base, "databaseName", strings.Repeat("a", 65)),
			wantErrSub: "Too long",
		},
		orderSchemaCase{name: "an unknown deletionPolicy", spec: specWith(base, "deletionPolicy", "Drop"), wantErrSub: "Unsupported value"},
		orderSchemaCase{
			name: "a name that is no MySQL identifier without databaseName", objName: "app.v1",
			spec:       map[string]any{"controlPlaneRef": ref},
			wantErrSub: "metadata.name is not a MySQL identifier once its dashes are underscores; set spec.databaseName",
		},
	)
	update = append(update,
		orderSchemaCase{
			name:       "a changed explicit databaseName",
			mutate:     func(spec map[string]any) { spec["databaseName"] = "other_db" },
			wantErrSub: "databaseName is immutable; delete and re-create the MariaDBDatabase to order another database",
		},
		orderSchemaCase{
			name:   "a flipped deletionPolicy",
			mutate: func(spec map[string]any) { spec["deletionPolicy"] = "Delete" },
		},
	)
	minimal := map[string]any{"controlPlaneRef": map[string]any{"name": "cp"}}
	runOrderSchemaCases(t, ctx, c, kind, create, minimal, base, update)

	t.Run("the schema defaults deletionPolicy to Retain and admits a dotted name with databaseName", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ns := newNamespace(t, ctx, c, "test-mdb-default-")
		order := &MariaDBDatabase{
			ObjectMeta: metav1.ObjectMeta{Name: "app.v1", Namespace: ns},
			Spec: MariaDBDatabaseSpec{
				ControlPlaneRef: ControlPlaneRefSpec{Name: "cp"},
				DatabaseName:    "app_v1",
			},
		}
		g.Expect(c.Create(ctx, order)).To(Succeed())
		stored := &MariaDBDatabase{}
		g.Expect(c.Get(ctx, client.ObjectKeyFromObject(order), stored)).To(Succeed())
		g.Expect(stored.Spec.DeletionPolicy).To(Equal("Retain"))
	})

	// The databaseName rule compares the EFFECTIVE name, so spelling out the
	// metadata.name default is not a change, and naming anything else is.
	t.Run("update compares the effective databaseName", func(t *testing.T) {
		g := NewGomegaWithT(t)
		order := &MariaDBDatabase{
			ObjectMeta: metav1.ObjectMeta{Name: "workflow-db", Namespace: newNamespace(t, ctx, c, "test-mdb-default-")},
			Spec:       MariaDBDatabaseSpec{ControlPlaneRef: ControlPlaneRefSpec{Name: "cp"}},
		}
		g.Expect(c.Create(ctx, order)).To(Succeed())

		order.Spec.DatabaseName = "other_db"
		err := c.Update(ctx, order)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("databaseName is immutable"))

		g.Expect(c.Get(ctx, client.ObjectKeyFromObject(order), order)).To(Succeed())
		order.Spec.DatabaseName = "workflow_db"
		g.Expect(c.Update(ctx, order)).To(Succeed(), "the default spelled out is admitted")
	})

	endpointCases := []struct {
		endpoint string
		admitted bool
	}{
		{endpoint: "db.example.com:3306", admitted: true},
		{endpoint: "[::1]:3306", admitted: true},
		{endpoint: "db.example.com:65535", admitted: true},
		{endpoint: "db.example.com"},
		{endpoint: "https://db:3306"},
		{endpoint: "db.example.com:0"},
		{endpoint: "db.example.com:65536"},
		{endpoint: "db.example.com:99999"},
	}
	for _, tc := range endpointCases {
		t.Run("publishedDatabaseEndpoint "+tc.endpoint, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := stamp(validControlPlane(), "cp-published", newNamespace(t, ctx, c, "test-mdb-default-"))
			cp.Spec.Infrastructure.PublishedDatabaseEndpoint = tc.endpoint

			err := c.Create(ctx, cp, client.DryRunAll)
			if tc.admitted {
				g.Expect(err).NotTo(HaveOccurred())
				return
			}
			expectAdmission(t, err, "should match")
			g.Expect(err.Error()).To(ContainSubstring("spec.infrastructure.publishedDatabaseEndpoint"))
		})
	}
}

// TestIntegration_CRD_CELOnly_RabbitMQVhost pins every admission rule of the
// RabbitMQVhost CRD, and the pattern of the ControlPlane field its orders on a
// target cluster read, against the real envtest API server. The kind has no
// webhook, so these rules are all there is; the substrings are the ones the
// invalid-rabbitmqvhost-cr chainsaw corpus asserts on.
//
// Rules: rabbitmqvhost_types.go:29, 108, 109, 119, 130, 153, 154, 161
func TestIntegration_CRD_CELOnly_RabbitMQVhost(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)

	const kind = "RabbitMQVhost"
	base := map[string]any{
		"controlPlaneRef":    map[string]any{"name": "cp", "namespace": "cp-ns"},
		"passwordGeneration": int64(2),
	}
	create, update := orderControlPlaneRefCases(kind, base)
	create = append(create,
		orderSchemaCase{
			name: "a zero passwordGeneration", spec: specWith(base, "passwordGeneration", int64(0)),
			wantErrSub: "should be greater than or equal to 1",
		},
		orderSchemaCase{
			name: "a deletionPolicy outside the enum", spec: specWith(base, "deletionPolicy", "Drop"),
			wantErrSub: "Unsupported value",
		},
		orderSchemaCase{
			name: "a 30s rotation.interval", spec: specWith(base, "rotation", map[string]any{"interval": "30s"}),
			wantErrSub: "rotation.interval is 0s or at least 1m",
		},
		orderSchemaCase{
			name: "an 87601h rotation.interval", spec: specWith(base, "rotation", map[string]any{"interval": "87601h"}),
			wantErrSub: "rotation.interval is at most 87600h",
		},
		orderSchemaCase{
			name:       "a gracePeriod longer than the interval",
			spec:       specWith(base, "rotation", map[string]any{"interval": "24h", "gracePeriod": "48h"}),
			wantErrSub: "rotation.gracePeriod must be shorter than rotation.interval",
		},
		orderSchemaCase{
			name: "a negative gracePeriod", spec: specWith(base, "rotation", map[string]any{"gracePeriod": "-1h"}),
			wantErrSub: "rotation.gracePeriod is not negative",
		},
	)
	update = append(update,
		orderSchemaCase{
			name:       "a lowered passwordGeneration",
			mutate:     func(spec map[string]any) { spec["passwordGeneration"] = int64(1) },
			wantErrSub: "passwordGeneration may only increase",
		},
		orderSchemaCase{
			name:   "a raised passwordGeneration",
			mutate: func(spec map[string]any) { spec["passwordGeneration"] = int64(5) },
		},
		orderSchemaCase{
			name:   "a changed deletionPolicy",
			mutate: func(spec map[string]any) { spec["deletionPolicy"] = "Delete" },
		},
		orderSchemaCase{
			name:   "a 0s interval with a 0s gracePeriod",
			mutate: func(spec map[string]any) { spec["rotation"] = map[string]any{"interval": "0s", "gracePeriod": "0s"} },
		},
	)
	minimal := map[string]any{"controlPlaneRef": map[string]any{"name": "cp"}}
	runOrderSchemaCases(t, ctx, c, kind, create, minimal, base, update)

	t.Run("an order without the optional fields stores the default literals", func(t *testing.T) {
		g := NewGomegaWithT(t)
		u := orderObject(kind, "minimal", newNamespace(t, ctx, c, "test-rmv-default-"), runtime.DeepCopyJSON(minimal))
		g.Expect(c.Create(ctx, u)).To(Succeed())

		stored := &RabbitMQVhost{}
		g.Expect(c.Get(ctx, client.ObjectKeyFromObject(u), stored)).To(Succeed())
		g.Expect(stored.Spec.PasswordGeneration).To(Equal(int64(1)))
		g.Expect(stored.Spec.Rotation.Interval).To(Equal(&metav1.Duration{Duration: 720 * time.Hour}))
		g.Expect(stored.Spec.Rotation.GracePeriod).To(Equal(&metav1.Duration{Duration: 24 * time.Hour}))
		g.Expect(stored.Spec.DeletionPolicy).To(Equal(RabbitMQVhostDeletionPolicyRetain))
	})

	endpoints := []struct {
		endpoint   string
		wantErrSub string
	}{
		{endpoint: "broker.example.com:5672"},
		{endpoint: "[::1]:5672"},
		{endpoint: "broker.example.com", wantErrSub: "should match"},
		{endpoint: "amqp://broker:5672", wantErrSub: "should match"},
	}
	for i, tc := range endpoints {
		verb := "admits"
		if tc.wantErrSub != "" {
			verb = "rejects"
		}
		t.Run(fmt.Sprintf("the ControlPlane %s publishedMessagingEndpoint %q", verb, tc.endpoint), func(t *testing.T) {
			g := NewGomegaWithT(t)
			// One namespace per case: a ControlPlane is unique per namespace.
			cp := stamp(validControlPlane(), fmt.Sprintf("cp-endpoint-%d", i), newNamespace(t, ctx, c, "test-cp-endpoint-"))
			cp.Spec.Infrastructure.PublishedMessagingEndpoint = tc.endpoint

			err := c.Create(ctx, cp)
			expectAdmission(t, err, tc.wantErrSub)
			if tc.wantErrSub != "" {
				g.Expect(err.Error()).To(ContainSubstring("publishedMessagingEndpoint"))
			}
		})
	}
}
