// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package v1alpha1

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
}
