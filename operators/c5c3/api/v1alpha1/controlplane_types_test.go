// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"regexp"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	commonv1 "github.com/c5c3/forge/internal/common/types"
)

func TestSchemeBuilderRegistersControlPlane(t *testing.T) {
	s := runtime.NewScheme()
	if err := AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme failed: %v", err)
	}

	for _, kind := range []string{"ControlPlane", "ControlPlaneList"} {
		gvk := schema.GroupVersionKind{Group: "c5c3.io", Version: "v1alpha1", Kind: kind}
		if _, err := s.New(gvk); err != nil {
			t.Fatalf("scheme.New(%v) failed: %v", gvk, err)
		}
	}
}

// controlPlaneReleasePattern mirrors the +kubebuilder:validation:Pattern marker
// on ControlPlaneSpec.OpenStackRelease. The CRD schema is the enforcement point
// at admission time; this test pins the contract so a marker change is caught.
const controlPlaneReleasePattern = `^\d{4}\.[12]$`

func TestOpenStackReleasePattern(t *testing.T) {
	re := regexp.MustCompile(controlPlaneReleasePattern)
	tests := []struct {
		release string
		valid   bool
	}{
		{"2024.1", true},
		{"2025.2", true},
		{"2023.1", true},
		{"", false},
		{"2025", false},
		{"2025.", false},
		{"25.2", false},
		{"2025.22", false},
		{"v2025.2", false},
		{"2025.2 ", false},
		// Non-cadence minors: OpenStack ships only YYYY.1 and YYYY.2, so the
		// [12] class must reject any other single digit — keeping the CRD
		// pattern in agreement with release.ParseRelease.
		{"2025.0", false},
		{"2025.3", false},
		{"2025.9", false},
	}
	for _, tt := range tests {
		got := re.MatchString(tt.release)
		if got != tt.valid {
			t.Errorf("release %q: pattern match = %v, want %v", tt.release, got, tt.valid)
		}
	}
}

// TestControlPlaneSpecReusesCommonTypes asserts the ControlPlane reuses the
// canonical commonv1 shapes for infrastructure and policy, so the
// aggregate and the per-service CRs validate the same way. Assigning the
// commonv1 zero values to the spec fields is a compile-time type assertion.
func TestControlPlaneSpecReusesCommonTypes(t *testing.T) {
	spec := ControlPlaneSpec{Infrastructure: &InfrastructureSpec{}}

	// These assignments only compile if the field types are exactly the
	// commonv1 types — guarding against an accidental local copy.
	spec.Infrastructure.Database = commonv1.DatabaseSpec{Database: "ks", SecretRef: commonv1.SecretRefSpec{Name: "s"}}
	spec.Infrastructure.Cache = commonv1.CacheSpec{Backend: "dogpile.cache.pymemcache"}
	spec.GlobalPolicyOverrides = &commonv1.PolicySpec{}
	spec.Services.Keystone = &ServiceKeystoneSpec{}
	spec.Services.Keystone.Image = &commonv1.ImageSpec{Repository: "r", Tag: "t"}
	spec.Services.Keystone.PolicyOverrides = &commonv1.PolicySpec{}
	spec.Services.Keystone.Gateway = &commonv1.GatewaySpec{}
	spec.KORC.AdminCredential.PasswordSecretRef = commonv1.SecretRefSpec{Name: "admin"}

	if spec.Infrastructure.Database.Database != "ks" {
		t.Errorf("unexpected database name %q", spec.Infrastructure.Database.Database)
	}
	if spec.Infrastructure.Cache.Backend != "dogpile.cache.pymemcache" {
		t.Errorf("unexpected cache backend %q", spec.Infrastructure.Cache.Backend)
	}
}

// TestServiceKeystoneSpecDeepCopy verifies the shared keystone subset
// round-trips through DeepCopy with independent pointer storage (plan decision #2).
func TestServiceKeystoneSpecDeepCopy(t *testing.T) {
	replicas := int32(5)
	spec := ServiceKeystoneSpec{
		Replicas:         &replicas,
		Image:            &commonv1.ImageSpec{Repository: "ghcr.io/c5c3/keystone", Tag: "2025.2"},
		RotationInterval: &metav1.Duration{},
	}

	clone := spec.DeepCopy()
	if clone.Replicas == spec.Replicas {
		t.Errorf("DeepCopy did not allocate a new *int32 for Replicas")
	}
	if clone.Image == spec.Image {
		t.Errorf("DeepCopy did not allocate a new *ImageSpec for Image")
	}
	if *clone.Replicas != 5 {
		t.Errorf("DeepCopy altered Replicas: got %d", *clone.Replicas)
	}
}

// TestServiceKeystoneSpecExternalDeepCopy verifies the External-mode block
// (mode + the typed external pointer, incl. its optional caBundleSecretRef)
// round-trips through DeepCopy with independent pointer storage.
func TestServiceKeystoneSpecExternalDeepCopy(t *testing.T) {
	spec := ServiceKeystoneSpec{
		Mode: KeystoneModeExternal,
		External: &ExternalKeystoneSpec{
			AuthURL:           "https://keystone.example.com/v3",
			EndpointType:      ExternalEndpointTypePublic,
			CABundleSecretRef: &commonv1.SecretRefSpec{Name: "brownfield-keystone-ca", Key: "ca.crt"},
		},
	}

	clone := spec.DeepCopy()
	if clone.External == spec.External {
		t.Errorf("DeepCopy did not allocate a new *ExternalKeystoneSpec for External")
	}
	if clone.External.CABundleSecretRef == spec.External.CABundleSecretRef {
		t.Errorf("DeepCopy did not allocate a new *SecretRefSpec for CABundleSecretRef")
	}
	if clone.Mode != KeystoneModeExternal {
		t.Errorf("DeepCopy altered Mode: got %q", clone.Mode)
	}
	if clone.External.AuthURL != "https://keystone.example.com/v3" {
		t.Errorf("DeepCopy altered AuthURL: got %q", clone.External.AuthURL)
	}

	// Mutating the clone's external block must not touch the source.
	clone.External.AuthURL = "https://other.example.com/v3"
	if spec.External.AuthURL != "https://keystone.example.com/v3" {
		t.Errorf("DeepCopy aliased External: source AuthURL changed to %q", spec.External.AuthURL)
	}
}

// TestIsExternalKeystone exercises the nil-safe discriminator across the three
// Keystone service states (nil, Managed, External).
func TestIsExternalKeystone(t *testing.T) {
	tests := []struct {
		name string
		ks   *ServiceKeystoneSpec
		want bool
	}{
		{"nil keystone", nil, false},
		{"managed (explicit)", &ServiceKeystoneSpec{Mode: KeystoneModeManaged}, false},
		{"managed (unset mode)", &ServiceKeystoneSpec{}, false},
		{"external", &ServiceKeystoneSpec{Mode: KeystoneModeExternal}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cp := &ControlPlane{Spec: ControlPlaneSpec{Services: ServicesSpec{Keystone: tt.ks}}}
			if got := cp.IsExternalKeystone(); got != tt.want {
				t.Errorf("IsExternalKeystone() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestKORCSpecShape exercises the KORC/AdminCredential nested shape
// and the application-credential defaults' field types.
func TestKORCSpecShape(t *testing.T) {
	restricted := true
	korc := KORCSpec{
		AdminCredential: AdminCredentialSpec{
			CloudCredentialsRef: CloudCredentialsRef{CloudName: "admin", SecretName: "k-orc-clouds-yaml"},
			PasswordSecretRef:   commonv1.SecretRefSpec{Name: "admin-pw"},
			ApplicationCredential: ApplicationCredentialSpec{
				Restricted: &restricted,
				AccessRules: []AccessRule{
					{Service: "identity", Method: "GET", Path: "/v3/users"},
				},
				Rotation: RotationSpec{Mode: RotationModePasswordDriven},
			},
			BootstrapResources: []BootstrapResourceSpec{{Kind: "Project", Name: "service"}},
		},
	}

	clone := korc.DeepCopy()
	if clone.AdminCredential.ApplicationCredential.Restricted == korc.AdminCredential.ApplicationCredential.Restricted {
		t.Errorf("DeepCopy did not allocate a new *bool for Restricted")
	}
	if clone.AdminCredential.ApplicationCredential.Rotation.Mode != RotationModePasswordDriven {
		t.Errorf("unexpected rotation mode %q", clone.AdminCredential.ApplicationCredential.Rotation.Mode)
	}
}

// TestDedicatedBackingServicesDeepCopy verifies the per-service dedicated blocks
// round-trip through DeepCopy with independent pointer storage. The reconciler
// DeepCopies the effective (dedicated-or-shared) specs onto the service children,
// so an aliased ClusterRef here would let a child projection mutate the
// ControlPlane spec it was derived from.
func TestDedicatedBackingServicesDeepCopy(t *testing.T) {
	spec := ServiceKeystoneSpec{
		DedicatedBackingServices: &KeystoneDedicatedBackingServicesSpec{
			Database: &commonv1.DatabaseSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: "cp-keystone-db"},
				Database:   "keystone",
			},
			Cache: &commonv1.CacheSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: "cp-keystone-cache"},
				Backend:    commonv1.DefaultCacheBackend,
			},
		},
	}

	clone := spec.DeepCopy()
	if clone.DedicatedBackingServices == spec.DedicatedBackingServices {
		t.Errorf("DeepCopy did not allocate a new *KeystoneDedicatedBackingServicesSpec")
	}
	if clone.DedicatedBackingServices.Database == spec.DedicatedBackingServices.Database {
		t.Errorf("DeepCopy did not allocate a new *DatabaseSpec for the dedicated database")
	}
	if clone.DedicatedBackingServices.Database.ClusterRef == spec.DedicatedBackingServices.Database.ClusterRef {
		t.Errorf("DeepCopy did not allocate a new *LocalObjectReference for the dedicated database clusterRef")
	}
	if clone.DedicatedBackingServices.Cache.ClusterRef == spec.DedicatedBackingServices.Cache.ClusterRef {
		t.Errorf("DeepCopy did not allocate a new *LocalObjectReference for the dedicated cache clusterRef")
	}

	// Mutating the clone must not touch the source.
	clone.DedicatedBackingServices.Database.ClusterRef.Name = "other-db"
	if spec.DedicatedBackingServices.Database.ClusterRef.Name != "cp-keystone-db" {
		t.Errorf("DeepCopy aliased the dedicated database clusterRef: source name changed to %q",
			spec.DedicatedBackingServices.Database.ClusterRef.Name)
	}
}

// TestDedicatedBackingServicesAccessors exercises the nil-safe reads the webhook
// and the reconciler share, across the three states a service can be in: no
// service block, a service that shares the ControlPlane-wide instances (the
// default), and a service that opted into dedicated instances.
func TestDedicatedBackingServicesAccessors(t *testing.T) {
	tests := []struct {
		name              string
		cp                *ControlPlane
		wantKeystoneDB    bool
		wantKeystoneCache bool
		wantHorizonCache  bool
	}{
		{
			name: "no service blocks",
			cp:   &ControlPlane{},
		},
		{
			name: "services share the ControlPlane-wide instances",
			cp: &ControlPlane{Spec: ControlPlaneSpec{Services: ServicesSpec{
				Keystone: &ServiceKeystoneSpec{},
				Horizon:  &ServiceHorizonSpec{},
			}}},
		},
		{
			name: "keystone takes a dedicated cache only",
			cp: &ControlPlane{Spec: ControlPlaneSpec{Services: ServicesSpec{
				Keystone: &ServiceKeystoneSpec{
					DedicatedBackingServices: &KeystoneDedicatedBackingServicesSpec{
						Cache: &commonv1.CacheSpec{Backend: commonv1.DefaultCacheBackend},
					},
				},
			}}},
			wantKeystoneCache: true,
		},
		{
			name: "both services take dedicated instances",
			cp: &ControlPlane{Spec: ControlPlaneSpec{Services: ServicesSpec{
				Keystone: &ServiceKeystoneSpec{
					DedicatedBackingServices: &KeystoneDedicatedBackingServicesSpec{
						Database: &commonv1.DatabaseSpec{Database: "keystone"},
						Cache:    &commonv1.CacheSpec{Backend: commonv1.DefaultCacheBackend},
					},
				},
				Horizon: &ServiceHorizonSpec{
					DedicatedBackingServices: &HorizonDedicatedBackingServicesSpec{
						Cache: &commonv1.CacheSpec{Backend: commonv1.DefaultCacheBackend},
					},
				},
			}}},
			wantKeystoneDB:    true,
			wantKeystoneCache: true,
			wantHorizonCache:  true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cp.DedicatedKeystoneDatabase() != nil; got != tc.wantKeystoneDB {
				t.Errorf("DedicatedKeystoneDatabase() present = %v, want %v", got, tc.wantKeystoneDB)
			}
			if got := tc.cp.DedicatedKeystoneCache() != nil; got != tc.wantKeystoneCache {
				t.Errorf("DedicatedKeystoneCache() present = %v, want %v", got, tc.wantKeystoneCache)
			}
			if got := tc.cp.DedicatedHorizonCache() != nil; got != tc.wantHorizonCache {
				t.Errorf("DedicatedHorizonCache() present = %v, want %v", got, tc.wantHorizonCache)
			}
		})
	}
}

func TestServiceNamespaceAccessors(t *testing.T) {
	cpIn := func(services ServicesSpec) *ControlPlane {
		return &ControlPlane{
			ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: "openstack"},
			Spec:       ControlPlaneSpec{Services: services},
		}
	}

	tests := []struct {
		name          string
		cp            *ControlPlane
		wantKeystone  string
		wantHorizon   string
		wantDedicated []string
	}{
		{
			name:         "no service blocks default to the ControlPlane namespace",
			cp:           cpIn(ServicesSpec{}),
			wantKeystone: "openstack",
			wantHorizon:  "openstack",
		},
		{
			name: "service blocks without an assignment default to the ControlPlane namespace",
			cp: cpIn(ServicesSpec{
				Keystone: &ServiceKeystoneSpec{},
				Horizon:  &ServiceHorizonSpec{},
			}),
			wantKeystone: "openstack",
			wantHorizon:  "openstack",
		},
		{
			name: "each service takes a namespace of its own",
			cp: cpIn(ServicesSpec{
				Keystone: &ServiceKeystoneSpec{
					Namespace: &ServiceNamespaceSpec{Name: "identity", Lifecycle: ServiceNamespaceLifecycleManaged},
				},
				Horizon: &ServiceHorizonSpec{
					Namespace: &ServiceNamespaceSpec{Name: "dashboard", Lifecycle: ServiceNamespaceLifecycleExternal},
				},
			}),
			wantKeystone:  "identity",
			wantHorizon:   "dashboard",
			wantDedicated: []string{"identity", "dashboard"},
		},
		{
			name: "co-located services yield one dedicated namespace",
			cp: cpIn(ServicesSpec{
				Keystone: &ServiceKeystoneSpec{Namespace: &ServiceNamespaceSpec{Name: "shared-ns"}},
				Horizon:  &ServiceHorizonSpec{Namespace: &ServiceNamespaceSpec{Name: "shared-ns"}},
			}),
			wantKeystone:  "shared-ns",
			wantHorizon:   "shared-ns",
			wantDedicated: []string{"shared-ns"},
		},
		{
			// Webhook-bypass shape: an assignment naming the ControlPlane's own
			// namespace must never be enumerated as dedicated, or teardown would
			// delete the ControlPlane's own namespace.
			name: "an assignment equal to the ControlPlane namespace is not dedicated",
			cp: cpIn(ServicesSpec{
				Keystone: &ServiceKeystoneSpec{Namespace: &ServiceNamespaceSpec{Name: "openstack"}},
			}),
			wantKeystone: "openstack",
			wantHorizon:  "openstack",
		},
		{
			// Webhook-bypass shape: an empty name is not an assignment.
			name: "an empty assignment name falls back to the ControlPlane namespace",
			cp: cpIn(ServicesSpec{
				Keystone: &ServiceKeystoneSpec{Namespace: &ServiceNamespaceSpec{}},
			}),
			wantKeystone: "openstack",
			wantHorizon:  "openstack",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cp.KeystoneNamespace(); got != tc.wantKeystone {
				t.Errorf("KeystoneNamespace() = %q, want %q", got, tc.wantKeystone)
			}
			if got := tc.cp.HorizonNamespace(); got != tc.wantHorizon {
				t.Errorf("HorizonNamespace() = %q, want %q", got, tc.wantHorizon)
			}
			var gotNames []string
			for _, ns := range tc.cp.DedicatedServiceNamespaces() {
				gotNames = append(gotNames, ns.Name)
			}
			if len(gotNames) != len(tc.wantDedicated) {
				t.Fatalf("DedicatedServiceNamespaces() = %v, want %v", gotNames, tc.wantDedicated)
			}
			for i := range gotNames {
				if gotNames[i] != tc.wantDedicated[i] {
					t.Errorf("DedicatedServiceNamespaces()[%d] = %q, want %q", i, gotNames[i], tc.wantDedicated[i])
				}
			}
		})
	}
}

func TestDedicatedServiceNamespacesCarriesTheLifecycle(t *testing.T) {
	cp := &ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: "openstack"},
		Spec: ControlPlaneSpec{Services: ServicesSpec{
			Horizon: &ServiceHorizonSpec{
				Namespace: &ServiceNamespaceSpec{Name: "dashboard", Lifecycle: ServiceNamespaceLifecycleExternal},
			},
		}},
	}
	got := cp.DedicatedServiceNamespaces()
	if len(got) != 1 {
		t.Fatalf("DedicatedServiceNamespaces() = %v, want one entry", got)
	}
	if got[0].Lifecycle != ServiceNamespaceLifecycleExternal {
		t.Errorf("lifecycle = %q, want %q", got[0].Lifecycle, ServiceNamespaceLifecycleExternal)
	}
}
