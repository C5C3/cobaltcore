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

	commonv1 "github.com/c5c3/forge/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
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

// TestManagedCatalogRows_ImageRow covers the image (Glance) catalog row: it is
// absent when services.glance is unset, and present with the generic CR names and
// the D6 public+internal endpoint posture when it is set — the internal endpoint
// always advertises the in-cluster Glance Service URL, while the public one shares
// that URL without a gateway, flips to the gateway hostname when exposed, and
// prefers an explicit services.glance.publicEndpoint over both.
func TestManagedCatalogRows_ImageRow(t *testing.T) {
	t.Run("absent when glance unset", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := korcControlPlane() // services.glance unset
		rows := managedCatalogRows(cp)
		g.Expect(rows).To(HaveLen(1), "with no image service the catalog is the identity row alone")
		g.Expect(rows[0].serviceType).To(Equal("identity"))
	})

	t.Run("present with generic names and both interfaces when set", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := korcControlPlane()
		cp.Spec.Services.Glance = &c5c3v1alpha1.ServiceGlanceSpec{}

		rows := managedCatalogRows(cp)
		g.Expect(rows).To(HaveLen(2), "the image row joins the identity row")

		image := rows[1]
		g.Expect(image.serviceType).To(Equal("image"))
		g.Expect(image.serviceName).To(Equal("glance"))
		g.Expect(image.crName).To(Equal("cp-image-service"))

		// The internal endpoint is registered first, the public one second.
		g.Expect(image.endpoints).To(HaveLen(2), "the image row registers both interfaces")
		internal, public := image.endpoints[0], image.endpoints[1]
		g.Expect(internal.iface).To(Equal("internal"))
		g.Expect(internal.crName).To(Equal("cp-image-endpoint-internal"))
		g.Expect(public.iface).To(Equal("public"))
		g.Expect(public.crName).To(Equal("cp-image-endpoint-public"))

		// Without a gateway the public URL equals the internal in-cluster URL (D6
		// registers both interfaces at birth to avoid a later catalog migration).
		wantInternal := "http://cp-glance.default.svc:9292"
		g.Expect(internal.url).To(Equal(wantInternal))
		g.Expect(public.url).To(Equal(wantInternal),
			"without a gateway the public endpoint shares the in-cluster URL")
	})

	t.Run("public URL flips to the gateway hostname when exposed", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := korcControlPlane()
		cp.Spec.Services.Glance = &c5c3v1alpha1.ServiceGlanceSpec{
			Gateway: &commonv1.GatewaySpec{
				ParentRef: commonv1.GatewayParentRefSpec{Name: "openstack-gw"},
				Hostname:  "glance.example.com",
			},
		}

		rows := managedCatalogRows(cp)
		g.Expect(rows).To(HaveLen(2))
		internal, public := rows[1].endpoints[0], rows[1].endpoints[1]
		g.Expect(internal.url).To(Equal("http://cp-glance.default.svc:9292"),
			"the internal endpoint stays in-cluster even when the public one is exposed")
		g.Expect(public.url).To(Equal("https://glance.example.com"),
			"the public endpoint prefers the gateway hostname, with no /v3 suffix")
	})

	t.Run("explicit publicEndpoint wins over the gateway hostname", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := korcControlPlane()
		cp.Spec.Services.Glance = &c5c3v1alpha1.ServiceGlanceSpec{
			Gateway: &commonv1.GatewaySpec{
				ParentRef: commonv1.GatewayParentRefSpec{Name: "openstack-gw"},
				Hostname:  "glance.example.com",
			},
			PublicEndpoint: "https://glance.127-0-0-1.nip.io:8443",
		}

		rows := managedCatalogRows(cp)
		g.Expect(rows).To(HaveLen(2))
		internal, public := rows[1].endpoints[0], rows[1].endpoints[1]
		g.Expect(internal.url).To(Equal("http://cp-glance.default.svc:9292"),
			"the internal endpoint stays in-cluster even when the public one is exposed")
		g.Expect(public.url).To(Equal("https://glance.127-0-0-1.nip.io:8443"),
			"the override is the only way to advertise a non-443 external port")
	})

	t.Run("publicEndpoint without a gateway still wins", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := korcControlPlane()
		cp.Spec.Services.Glance = &c5c3v1alpha1.ServiceGlanceSpec{
			PublicEndpoint: "https://glance.127-0-0-1.nip.io:8443",
		}

		rows := managedCatalogRows(cp)
		g.Expect(rows).To(HaveLen(2))
		internal, public := rows[1].endpoints[0], rows[1].endpoints[1]
		g.Expect(internal.url).To(Equal("http://cp-glance.default.svc:9292"),
			"the internal endpoint stays in-cluster")
		g.Expect(public.url).To(Equal("https://glance.127-0-0-1.nip.io:8443"),
			"the override wins even without a gateway, matching keystonePublicEndpoint semantics")
	})
}

// TestManagedCatalogRows_PlacementRow covers the placement catalog row on the same
// terms as the image row: absent when services.placement is unset, present with the
// generic CR names and both interfaces when it is set — the internal endpoint always
// advertises the in-cluster Placement Service URL, while the public one shares that
// URL without a gateway, flips to the gateway hostname when exposed, and prefers an
// explicit services.placement.publicEndpoint over both.
func TestManagedCatalogRows_PlacementRow(t *testing.T) {
	t.Run("absent when placement unset", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := korcControlPlane() // services.placement unset
		rows := managedCatalogRows(cp)
		g.Expect(rows).To(HaveLen(1), "with no placement service the catalog is the identity row alone")
		g.Expect(rows[0].serviceType).To(Equal("identity"))
	})

	t.Run("present with generic names and both interfaces when set", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := korcControlPlane()
		cp.Spec.Services.Placement = &c5c3v1alpha1.ServicePlacementSpec{}

		rows := managedCatalogRows(cp)
		g.Expect(rows).To(HaveLen(2), "the placement row joins the identity row")

		placementRow := rows[1]
		g.Expect(placementRow.serviceType).To(Equal("placement"))
		g.Expect(placementRow.serviceName).To(Equal("placement"))
		g.Expect(placementRow.crName).To(Equal("cp-placement-service"))

		// The internal endpoint is registered first, the public one second.
		g.Expect(placementRow.endpoints).To(HaveLen(2), "the placement row registers both interfaces")
		internal, public := placementRow.endpoints[0], placementRow.endpoints[1]
		g.Expect(internal.iface).To(Equal("internal"))
		g.Expect(internal.crName).To(Equal("cp-placement-endpoint-internal"))
		g.Expect(public.iface).To(Equal("public"))
		g.Expect(public.crName).To(Equal("cp-placement-endpoint-public"))

		// Without a gateway the public URL equals the internal in-cluster URL (both
		// interfaces are registered at birth to avoid a later catalog migration).
		wantInternal := "http://cp-placement.default.svc:8778"
		g.Expect(internal.url).To(Equal(wantInternal))
		g.Expect(public.url).To(Equal(wantInternal),
			"without a gateway the public endpoint shares the in-cluster URL")
	})

	t.Run("public URL flips to the gateway hostname when exposed", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := korcControlPlane()
		cp.Spec.Services.Placement = &c5c3v1alpha1.ServicePlacementSpec{
			Gateway: &commonv1.GatewaySpec{
				ParentRef: commonv1.GatewayParentRefSpec{Name: "openstack-gw"},
				Hostname:  "placement.example.com",
			},
		}

		rows := managedCatalogRows(cp)
		g.Expect(rows).To(HaveLen(2))
		internal, public := rows[1].endpoints[0], rows[1].endpoints[1]
		g.Expect(internal.url).To(Equal("http://cp-placement.default.svc:8778"),
			"the internal endpoint stays in-cluster even when the public one is exposed")
		g.Expect(public.url).To(Equal("https://placement.example.com"),
			"the public endpoint prefers the gateway hostname, with no path suffix")
	})

	t.Run("explicit publicEndpoint wins over the gateway hostname", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := korcControlPlane()
		cp.Spec.Services.Placement = &c5c3v1alpha1.ServicePlacementSpec{
			Gateway: &commonv1.GatewaySpec{
				ParentRef: commonv1.GatewayParentRefSpec{Name: "openstack-gw"},
				Hostname:  "placement.example.com",
			},
			PublicEndpoint: "https://placement.127-0-0-1.nip.io:8443",
		}

		rows := managedCatalogRows(cp)
		g.Expect(rows).To(HaveLen(2))
		internal, public := rows[1].endpoints[0], rows[1].endpoints[1]
		g.Expect(internal.url).To(Equal("http://cp-placement.default.svc:8778"),
			"the internal endpoint stays in-cluster even when the public one is exposed")
		g.Expect(public.url).To(Equal("https://placement.127-0-0-1.nip.io:8443"),
			"the override is the only way to advertise a non-443 external port")
	})

	t.Run("publicEndpoint without a gateway still wins", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := korcControlPlane()
		cp.Spec.Services.Placement = &c5c3v1alpha1.ServicePlacementSpec{
			PublicEndpoint: "https://placement.127-0-0-1.nip.io:8443",
		}

		rows := managedCatalogRows(cp)
		g.Expect(rows).To(HaveLen(2))
		internal, public := rows[1].endpoints[0], rows[1].endpoints[1]
		g.Expect(internal.url).To(Equal("http://cp-placement.default.svc:8778"),
			"the internal endpoint stays in-cluster")
		g.Expect(public.url).To(Equal("https://placement.127-0-0-1.nip.io:8443"),
			"the override wins even without a gateway, matching keystonePublicEndpoint semantics")
	})
}

// TestManagedCatalogRows_BarbicanRow covers the key-manager catalog row on the
// same terms as the image and placement rows: absent when services.barbican is
// unset, present with the generic CR names and both interfaces when it is set. The
// internal endpoint always advertises the in-cluster Barbican Service URL, while
// the public one shares that URL without a gateway, flips to the gateway hostname
// when exposed, and prefers an explicit services.barbican.publicEndpoint over
// both.
func TestManagedCatalogRows_BarbicanRow(t *testing.T) {
	// barbicanService builds the minimal service block: the secret store is a
	// required field, so no ControlPlane carries a Barbican without one.
	barbicanService := func() *c5c3v1alpha1.ServiceBarbicanSpec {
		return &c5c3v1alpha1.ServiceBarbicanSpec{
			SecretStore: c5c3v1alpha1.ServiceBarbicanSecretStoreSpec{
				Dedicated: &c5c3v1alpha1.BarbicanDedicatedSecretStoreSpec{},
			},
		}
	}

	t.Run("absent when barbican unset", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := korcControlPlane() // services.barbican unset
		rows := managedCatalogRows(cp)
		g.Expect(rows).To(HaveLen(1), "with no key manager the catalog is the identity row alone")
		g.Expect(rows[0].serviceType).To(Equal("identity"))
	})

	t.Run("present with generic names and both interfaces when set", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := korcControlPlane()
		cp.Spec.Services.Barbican = barbicanService()

		rows := managedCatalogRows(cp)
		g.Expect(rows).To(HaveLen(2), "the key-manager row joins the identity row")

		barbicanRow := rows[1]
		g.Expect(barbicanRow.serviceType).To(Equal("key-manager"))
		g.Expect(barbicanRow.serviceName).To(Equal("barbican"))
		g.Expect(barbicanRow.crName).To(Equal("cp-key-manager-service"))

		// The internal endpoint is registered first, the public one second.
		g.Expect(barbicanRow.endpoints).To(HaveLen(2), "the key-manager row registers both interfaces")
		internal, public := barbicanRow.endpoints[0], barbicanRow.endpoints[1]
		g.Expect(internal.iface).To(Equal("internal"))
		g.Expect(internal.crName).To(Equal("cp-key-manager-endpoint-internal"))
		g.Expect(public.iface).To(Equal("public"))
		g.Expect(public.crName).To(Equal("cp-key-manager-endpoint-public"))

		// Without a gateway the public URL equals the internal in-cluster URL (both
		// interfaces are registered at birth to avoid a later catalog migration).
		wantInternal := "http://cp-barbican.default.svc:9311"
		g.Expect(internal.url).To(Equal(wantInternal))
		g.Expect(public.url).To(Equal(wantInternal),
			"without a gateway the public endpoint shares the in-cluster URL")
	})

	t.Run("public URL flips to the gateway hostname when exposed", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := korcControlPlane()
		cp.Spec.Services.Barbican = barbicanService()
		cp.Spec.Services.Barbican.Gateway = &commonv1.GatewaySpec{
			ParentRef: commonv1.GatewayParentRefSpec{Name: "openstack-gw"},
			Hostname:  "barbican.example.com",
		}

		rows := managedCatalogRows(cp)
		g.Expect(rows).To(HaveLen(2))
		internal, public := rows[1].endpoints[0], rows[1].endpoints[1]
		g.Expect(internal.url).To(Equal("http://cp-barbican.default.svc:9311"),
			"the internal endpoint stays in-cluster even when the public one is exposed")
		g.Expect(public.url).To(Equal("https://barbican.example.com"),
			"the public endpoint prefers the gateway hostname, with no path suffix")
	})

	t.Run("explicit publicEndpoint wins over the gateway hostname", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := korcControlPlane()
		cp.Spec.Services.Barbican = barbicanService()
		cp.Spec.Services.Barbican.Gateway = &commonv1.GatewaySpec{
			ParentRef: commonv1.GatewayParentRefSpec{Name: "openstack-gw"},
			Hostname:  "barbican.example.com",
		}
		cp.Spec.Services.Barbican.PublicEndpoint = "https://barbican.127-0-0-1.nip.io:8443"

		rows := managedCatalogRows(cp)
		g.Expect(rows).To(HaveLen(2))
		internal, public := rows[1].endpoints[0], rows[1].endpoints[1]
		g.Expect(internal.url).To(Equal("http://cp-barbican.default.svc:9311"),
			"the internal endpoint stays in-cluster even when the public one is exposed")
		g.Expect(public.url).To(Equal("https://barbican.127-0-0-1.nip.io:8443"),
			"the override is the only way to advertise a non-443 external port")
	})

	t.Run("publicEndpoint without a gateway still wins", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := korcControlPlane()
		cp.Spec.Services.Barbican = barbicanService()
		cp.Spec.Services.Barbican.PublicEndpoint = "https://barbican.127-0-0-1.nip.io:8443"

		rows := managedCatalogRows(cp)
		g.Expect(rows).To(HaveLen(2))
		internal, public := rows[1].endpoints[0], rows[1].endpoints[1]
		g.Expect(internal.url).To(Equal("http://cp-barbican.default.svc:9311"),
			"the internal endpoint stays in-cluster")
		g.Expect(public.url).To(Equal("https://barbican.127-0-0-1.nip.io:8443"),
			"the override wins even without a gateway, matching keystonePublicEndpoint semantics")
	})
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
