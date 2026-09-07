// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the managed-mode catalog table (managedCatalogRows) and its K-ORC
// Service/Endpoint builders in reconcile_catalog.go.
package controller

import (
	"testing"

	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	. "github.com/onsi/gomega"

	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// TestManagedCatalogRows_IdentityOnly locks in that the managed catalog is driven
// from a table whose ONLY row today is the identity (Keystone) service: one row,
// type "identity", name "keystone", the legacy Service/Endpoint CR names, and a
// single public Endpoint whose URL is the catalog URL.
func TestManagedCatalogRows_IdentityOnly(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := korcControlPlane()

	rows := managedCatalogRows(cp)
	g.Expect(rows).To(HaveLen(1), "today the managed catalog is exactly the identity row")

	row := rows[0]
	g.Expect(row.serviceType).To(Equal("identity"))
	g.Expect(row.serviceName).To(Equal("keystone"))
	g.Expect(row.crName).To(Equal(keystoneServiceName(cp)), "the identity row keeps its legacy Service CR name")
	g.Expect(row.endpoints).To(HaveLen(1), "the default posture is a single public entry")

	ep := row.endpoints[0]
	g.Expect(ep.iface).To(Equal("public"))
	g.Expect(ep.crName).To(Equal(keystoneEndpointName(cp)), "the identity row keeps its legacy Endpoint CR name")
	g.Expect(ep.url).To(Equal(keystoneCatalogURL(cp)))
}

// TestManagedCatalogRows_BuiltinServicesAddNoRow covers the edge the identity-only
// contract turns on: a ControlPlane declaring the image, the placement AND the
// key-manager service still registers exactly one row here. Each built-in carries
// its catalog entry on the KeystoneService child projected for it, registered
// under that child's own names, so nothing joins the identity row.
func TestManagedCatalogRows_BuiltinServicesAddNoRow(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := korcControlPlane()
	cp.Spec.Services.Glance = &c5c3v1alpha1.ServiceGlanceSpec{}
	cp.Spec.Services.Placement = &c5c3v1alpha1.ServicePlacementSpec{}
	// The secret store is a required field, so no ControlPlane carries a Barbican
	// without one.
	cp.Spec.Services.Barbican = &c5c3v1alpha1.ServiceBarbicanSpec{
		SecretStore: c5c3v1alpha1.ServiceBarbicanSecretStoreSpec{
			Dedicated: &c5c3v1alpha1.BarbicanDedicatedSecretStoreSpec{},
		},
	}

	rows := managedCatalogRows(cp)
	g.Expect(rows).To(HaveLen(1), "the built-in services register through their KeystoneService children")
	g.Expect(rows[0].serviceType).To(Equal("identity"))
	g.Expect(rows[0].crName).To(Equal(keystoneServiceName(cp)))
}

// TestManagedCatalogBuilders_IdentityShapeUnchanged is the refactor-equivalence
// lock: the builders must render the identity Service/Endpoint field-for-field as
// the pre-refactor inline literals did, so the live K-ORC CRs (and the catalog
// rows behind them) are byte-identical across the refactor.
func TestManagedCatalogBuilders_IdentityShapeUnchanged(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := korcControlPlane()
	credRef := orcv1alpha1.CloudCredentialsReference{SecretName: "k-orc-clouds-yaml", CloudName: "admin"}

	row := managedCatalogRows(cp)[0]

	service := managedCatalogService(cp, credRef, row)
	g.Expect(service.Name).To(Equal(keystoneServiceName(cp)))
	g.Expect(service.Namespace).To(Equal(childNamespace(cp)))
	g.Expect(service.Spec.ManagementPolicy).To(Equal(orcv1alpha1.ManagementPolicyManaged))
	g.Expect(service.Spec.CloudCredentialsRef).To(Equal(credRef))
	g.Expect(service.Spec.Import).To(BeNil(), "a managed catalog Service registers a Resource, it does not import")
	g.Expect(service.Spec.Resource).NotTo(BeNil())
	g.Expect(service.Spec.Resource.Type).To(Equal("identity"))
	g.Expect(service.Spec.Resource.Name).To(HaveValue(Equal(orcv1alpha1.OpenStackName("keystone"))))
	g.Expect(service.Spec.Resource.Enabled).To(HaveValue(BeTrue()))

	ep := row.endpoints[0]
	endpoint := managedCatalogEndpoint(cp, credRef, row, ep)
	g.Expect(endpoint.Name).To(Equal(keystoneEndpointName(cp)))
	g.Expect(endpoint.Namespace).To(Equal(childNamespace(cp)))
	g.Expect(endpoint.Spec.ManagementPolicy).To(Equal(orcv1alpha1.ManagementPolicyManaged))
	g.Expect(endpoint.Spec.CloudCredentialsRef).To(Equal(credRef))
	g.Expect(endpoint.Spec.Import).To(BeNil())
	g.Expect(endpoint.Spec.Resource).NotTo(BeNil())
	g.Expect(endpoint.Spec.Resource.Interface).To(Equal("public"))
	g.Expect(endpoint.Spec.Resource.URL).To(Equal(keystoneCatalogURL(cp)))
	g.Expect(endpoint.Spec.Resource.ServiceRef).To(Equal(orcv1alpha1.KubernetesNameRef(keystoneServiceName(cp))))
	g.Expect(endpoint.Spec.Resource.Enabled).To(HaveValue(BeTrue()))
}

// TestManagedCatalogBuilders_SyntheticImageRow proves the table drives more than
// the identity row: a synthetic second service (type "image", name "glance") with
// a public and an internal endpoint renders one Service and two Endpoints under
// the documented generic naming convention ("{cp}-{type}-service" /
// "{cp}-{type}-endpoint-{iface}"), each Endpoint carrying its own interface and
// URL and pointing at the row's Service. This is the interim assertion sanctioned
// until a real second service is onboarded.
func TestManagedCatalogBuilders_SyntheticImageRow(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := korcControlPlane()
	ns := childNamespace(cp)
	credRef := orcv1alpha1.CloudCredentialsReference{SecretName: "k-orc-clouds-yaml", CloudName: "admin"}

	wantURL := managedServiceURL("glance-api", ns, 9292, "")
	row := managedCatalogServiceRow{
		serviceType: "image",
		serviceName: "glance",
		crName:      cp.Name + "-image-service",
		endpoints: []managedCatalogEndpointRow{
			{iface: "public", crName: cp.Name + "-image-endpoint-public", url: wantURL},
			{iface: "internal", crName: cp.Name + "-image-endpoint-internal", url: wantURL},
		},
	}

	service := managedCatalogService(cp, credRef, row)
	g.Expect(service.Name).To(Equal("cp-image-service"))
	g.Expect(service.Namespace).To(Equal(ns))
	g.Expect(service.Spec.Resource.Type).To(Equal("image"))
	g.Expect(service.Spec.Resource.Name).To(HaveValue(Equal(orcv1alpha1.OpenStackName("glance"))))

	g.Expect(row.endpoints).To(HaveLen(2), "the image row registers two interfaces")
	for _, ep := range row.endpoints {
		endpoint := managedCatalogEndpoint(cp, credRef, row, ep)
		g.Expect(endpoint.Name).To(Equal("cp-image-endpoint-" + ep.iface))
		g.Expect(endpoint.Namespace).To(Equal(ns))
		g.Expect(endpoint.Spec.Resource.Interface).To(Equal(ep.iface))
		g.Expect(endpoint.Spec.Resource.URL).To(Equal(wantURL))
		g.Expect(string(endpoint.Spec.Resource.ServiceRef)).To(Equal("cp-image-service"),
			"every Endpoint of the row points at the row's Service CR")
	}
}

// TestManagedServiceURL pins the generic in-cluster URL template keystoneEndpointURL
// now wraps, including the empty-path edge (no trailing slash is appended).
func TestManagedServiceURL(t *testing.T) {
	g := NewGomegaWithT(t)

	g.Expect(managedServiceURL("cp-keystone", "openstack", 5000, "/v3")).
		To(Equal("http://cp-keystone.openstack.svc:5000/v3"))
	g.Expect(managedServiceURL("glance-api", "openstack", 9292, "")).
		To(Equal("http://glance-api.openstack.svc:9292"), "an empty path adds no trailing slash")
}

// TestManagedCatalogRegion_Shape pins the projection of the Region CR that adopts
// the region the keystone bootstrap inserted: a managed CR named "{cp}-region" in
// the ControlPlane's child namespace that detaches on delete, plus the two-phase
// description rule. Describing before adoption is the edge that matters: K-ORC's
// adoption filter then looks for a region carrying that description, the bootstrap
// row (whose description is empty) does not match, and the create that follows
// takes Keystone's 409 Conflict as a terminal error.
func TestManagedCatalogRegion_Shape(t *testing.T) {
	credRef := orcv1alpha1.CloudCredentialsReference{SecretName: "k-orc-clouds-yaml", CloudName: "admin"}

	// An empty wantDescription means the CR must carry no description at all: the
	// empty string is not a legal value for K-ORC (MinLength 1).
	tests := []struct {
		name            string
		region          string
		description     string
		adopted         bool
		wantRegion      orcv1alpha1.OpenStackName
		wantDescription string
	}{
		{
			name:        "unadopted region drops the description",
			region:      "RegionOne",
			description: "CobaltCore e2e region",
			adopted:     false,
			wantRegion:  "RegionOne",
		},
		{
			name:            "adopted region carries the description",
			region:          "eu-de-1",
			description:     "CobaltCore e2e region",
			adopted:         true,
			wantRegion:      "eu-de-1",
			wantDescription: "CobaltCore e2e region",
		},
		{
			name:       "adopted region without a description stays undescribed",
			region:     "RegionOne",
			adopted:    true,
			wantRegion: "RegionOne",
		},
		{
			name:       "an empty spec.region falls back to RegionOne",
			region:     "",
			adopted:    true,
			wantRegion: "RegionOne",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := korcControlPlane()
			cp.Spec.Region = tt.region
			cp.Spec.RegionDescription = tt.description

			region := managedCatalogRegion(cp, credRef, tt.adopted)
			g.Expect(region.Name).To(Equal(keystoneRegionName(cp)))
			g.Expect(region.Namespace).To(Equal(childNamespace(cp)))
			g.Expect(region.Spec.ManagementPolicy).To(Equal(orcv1alpha1.ManagementPolicyManaged))
			g.Expect(region.Spec.Import).To(BeNil(), "the region is adopted as a managed CR, not imported")
			g.Expect(region.Spec.ManagedOptions).NotTo(BeNil())
			g.Expect(region.Spec.ManagedOptions.OnDelete).To(Equal(orcv1alpha1.OnDeleteDetach),
				"Keystone refuses to delete a region the identity endpoints still reference")
			g.Expect(region.Spec.CloudCredentialsRef).To(Equal(credRef))
			g.Expect(region.Spec.Resource).NotTo(BeNil())
			g.Expect(region.Spec.Resource.Name).To(HaveValue(Equal(tt.wantRegion)))

			if tt.wantDescription == "" {
				g.Expect(region.Spec.Resource.Description).To(BeNil())
				return
			}
			g.Expect(region.Spec.Resource.Description).To(HaveValue(Equal(tt.wantDescription)))
		})
	}
}
