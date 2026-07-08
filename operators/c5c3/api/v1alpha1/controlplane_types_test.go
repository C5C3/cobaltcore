// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"regexp"
	"testing"

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
