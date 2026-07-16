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
