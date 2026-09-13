// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the pure builders in builtin_registrations.go: the per-service values
// each desired*Registration hands to builtinRegistration, the account roles among
// them.
package controller

import (
	"testing"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// registrationControlPlane builds a ControlPlane declaring every built-in service
// that projects a KeystoneService child, co-located in the ControlPlane's own
// namespace.
func registrationControlPlane() *c5c3v1alpha1.ControlPlane {
	return &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: "default"},
		Spec: c5c3v1alpha1.ControlPlaneSpec{
			Services: c5c3v1alpha1.ServicesSpec{
				Glance:    &c5c3v1alpha1.ServiceGlanceSpec{},
				Placement: &c5c3v1alpha1.ServicePlacementSpec{},
				Barbican:  &c5c3v1alpha1.ServiceBarbicanSpec{},
				Neutron: &c5c3v1alpha1.ServiceNeutronSpec{
					OVN: c5c3v1alpha1.NeutronOVNSpec{
						CentralRef: c5c3v1alpha1.NeutronOVNCentralRef{Name: "ovn"},
					},
				},
				Cinder: &c5c3v1alpha1.ServiceCinderSpec{
					Backends: []c5c3v1alpha1.CinderBackendEntry{{
						Name: "nfs1",
						Type: "NFS",
						NFS:  &c5c3v1alpha1.NFSShareSpec{Server: "nfs.example.com", Path: "/exports/cinder"},
					}},
				},
			},
		},
	}
}

// TestBuiltinRegistration_RolesParameter pins the roles each built-in service
// asks for. Every service needs "service"; the block-storage account needs
// "admin" on top, because cinder deletes an encrypted volume's Barbican secret on
// behalf of an owner that cannot, and Barbican's secure-RBAC defaults admit that
// cross-project reach to admin alone. The rest of the Cinder registration is
// pinned here too: the block-storage catalog row, its two "/v3" endpoints, and
// the service account the projection provisions.
func TestBuiltinRegistration_RolesParameter(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := registrationControlPlane()

	for service, ks := range map[string]*c5c3v1alpha1.KeystoneService{
		"glance":    desiredGlanceRegistration(cp),
		"placement": desiredPlacementRegistration(cp),
		"barbican":  desiredBarbicanRegistration(cp),
		"neutron":   desiredNeutronRegistration(cp),
	} {
		g.Expect(ks.Spec.Account.Roles).To(Equal([]string{"service"}),
			"%s reaches no further than service-to-service calls", service)
	}

	ks := desiredCinderRegistration(cp)
	g.Expect(ks.Spec.Account.Roles).To(Equal([]string{"service", "admin"}),
		"cinder needs admin beside service to delete another project's Barbican secret")
	g.Expect(ks.Name).To(Equal("cp-cinder"))
	g.Expect(ks.Namespace).To(Equal("default"))
	g.Expect(ks.Spec.ControlPlaneRef).To(Equal(c5c3v1alpha1.ControlPlaneRefSpec{Name: "cp", Namespace: "default"}))
	g.Expect(ks.Spec.Catalog.ServiceType).To(Equal("block-storage"))
	g.Expect(ks.Spec.Catalog.ServiceName).To(Equal("cinder"))
	g.Expect(ks.Spec.Account.UserName).To(Equal("cinder"))
	g.Expect(ks.Spec.Account.Project.Name).To(Equal("service-cinder"))
	g.Expect(ks.Spec.Account.Project.Create).To(BeTrue())
	g.Expect(ks.Spec.Catalog.Endpoints).To(HaveLen(2))
	g.Expect(ks.Spec.Catalog.Endpoints[0].Interface).To(Equal(c5c3v1alpha1.ExternalEndpointTypeInternal))
	g.Expect(ks.Spec.Catalog.Endpoints[0].URL).To(Equal("http://cp-cinder.default.svc:8776/v3"))
	g.Expect(ks.Spec.Catalog.Endpoints[1].Interface).To(Equal(c5c3v1alpha1.ExternalEndpointTypePublic))
	g.Expect(ks.Spec.Catalog.Endpoints[1].URL).To(Equal(cinderCatalogURL(cp)))
}

// TestBuiltinRegistration_PlacedCinder covers the placed block-storage service:
// its registration lands in the namespace services.cinder.namespace assigns, and
// the internal row drops the in-cluster URL for the public one, because a name
// that resolves inside the target cluster alone is unreachable for every consumer
// outside it (see internalCatalogURL).
func TestBuiltinRegistration_PlacedCinder(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := registrationControlPlane()
	cp.Spec.Services.Cinder.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{Name: "block"}
	cp.Spec.Services.Cinder.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "edge-a"}
	cp.Spec.Services.Cinder.PublicEndpoint = "https://cinder.example.com"

	ks := desiredCinderRegistration(cp)

	g.Expect(ks.Namespace).To(Equal("block"))
	g.Expect(ks.Spec.ControlPlaneRef.Namespace).To(Equal("default"),
		"the registration names the ControlPlane's namespace explicitly, not its own")
	g.Expect(ks.Spec.Catalog.Endpoints[0].URL).To(Equal("https://cinder.example.com/v3"))
	g.Expect(ks.Spec.Catalog.Endpoints[1].URL).To(Equal("https://cinder.example.com/v3"))
}
