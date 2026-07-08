// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package identity_test

import (
	"context"
	"errors"
	"testing"

	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/federation"
	. "github.com/onsi/gomega"
	"k8s.io/utils/ptr"

	"github.com/c5c3/forge/operators/keystone/internal/identity"
)

func TestIdentityProvider_CreateGetUpdateDelete(t *testing.T) {
	g := NewGomegaWithT(t)
	c, srv := newClient(t)
	ctx := context.Background()

	err := c.CreateIdentityProvider(ctx, identity.IdentityProvider{
		ID:        "keycloak",
		DomainID:  "domain-0001",
		Enabled:   ptr.To(true),
		RemoteIDs: []string{"https://idp.example.com/realms/forge"},
	})
	g.Expect(err).NotTo(HaveOccurred())

	got, err := c.GetIdentityProvider(ctx, "keycloak")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(got.ID).To(Equal("keycloak"))
	g.Expect(got.DomainID).To(Equal("domain-0001"))
	g.Expect(got.RemoteIDs).To(ConsistOf("https://idp.example.com/realms/forge"))

	err = c.UpdateIdentityProvider(ctx, "keycloak", ptr.To(false), ptr.To("federated corp IdP"),
		[]string{"https://idp.example.com/realms/forge2"})
	g.Expect(err).NotTo(HaveOccurred())
	rec := srv.IdentityProvider("keycloak")
	g.Expect(rec.Enabled).To(BeFalse())
	g.Expect(rec.Description).To(Equal("federated corp IdP"))
	g.Expect(rec.RemoteIDs).To(ConsistOf("https://idp.example.com/realms/forge2"))

	g.Expect(c.DeleteIdentityProvider(ctx, "keycloak")).To(Succeed())
	g.Expect(srv.IdentityProvider("keycloak")).To(BeNil())
}

func TestIdentityProvider_GetNotFound(t *testing.T) {
	g := NewGomegaWithT(t)
	c, _ := newClient(t)

	_, err := c.GetIdentityProvider(context.Background(), "missing")
	g.Expect(err).To(HaveOccurred())
	g.Expect(errors.Is(err, identity.ErrNotFound)).To(BeTrue())
}

func TestIdentityProvider_CreateConflict(t *testing.T) {
	g := NewGomegaWithT(t)
	c, _ := newClient(t)
	ctx := context.Background()

	idp := identity.IdentityProvider{ID: "dup", DomainID: "d1"}
	g.Expect(c.CreateIdentityProvider(ctx, idp)).To(Succeed())
	err := c.CreateIdentityProvider(ctx, idp)
	g.Expect(errors.Is(err, identity.ErrConflict)).To(BeTrue())
}

func TestProtocol_CreateGetUpdateDelete(t *testing.T) {
	g := NewGomegaWithT(t)
	c, srv := newClient(t)
	ctx := context.Background()

	g.Expect(c.CreateIdentityProvider(ctx, identity.IdentityProvider{ID: "keycloak"})).To(Succeed())
	g.Expect(c.CreateProtocol(ctx, "keycloak", "openid", "keycloak-mapping")).To(Succeed())

	got, err := c.GetProtocol(ctx, "keycloak", "openid")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(got.ID).To(Equal("openid"))
	g.Expect(got.MappingID).To(Equal("keycloak-mapping"))

	g.Expect(c.UpdateProtocol(ctx, "keycloak", "openid", "other-mapping")).To(Succeed())
	g.Expect(srv.Protocol("keycloak", "openid").MappingID).To(Equal("other-mapping"))

	g.Expect(c.DeleteProtocol(ctx, "keycloak", "openid")).To(Succeed())
	_, err = c.GetProtocol(ctx, "keycloak", "openid")
	g.Expect(errors.Is(err, identity.ErrNotFound)).To(BeTrue())
}

func TestProtocol_MissingIdentityProviderIsNotFound(t *testing.T) {
	g := NewGomegaWithT(t)
	c, _ := newClient(t)

	err := c.CreateProtocol(context.Background(), "missing-idp", "openid", "m")
	g.Expect(errors.Is(err, identity.ErrNotFound)).To(BeTrue())
}

func TestMapping_RoundTripViaGophercloud(t *testing.T) {
	g := NewGomegaWithT(t)
	c, srv := newClient(t)
	ctx := context.Background()

	rules := []federation.MappingRule{{
		Local: []federation.RuleLocal{{
			User:   &federation.RuleUser{Name: "{0}"},
			Groups: "{1}",
		}},
		Remote: []federation.RuleRemote{
			{Type: "HTTP_OIDC_PREFERRED_USERNAME"},
			{Type: "HTTP_OIDC_ISS", AnyOneOf: []string{"https://idp.example.com/realms/forge"}},
		},
	}}

	g.Expect(c.CreateMapping(ctx, "keycloak-mapping", rules)).To(Succeed())
	g.Expect(srv.Mapping("keycloak-mapping")).NotTo(BeNil())

	got, err := c.GetMapping(ctx, "keycloak-mapping")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(got.Rules).To(HaveLen(1))
	g.Expect(got.Rules[0].Remote).To(HaveLen(2))
	g.Expect(got.Rules[0].Remote[1].AnyOneOf).To(ConsistOf("https://idp.example.com/realms/forge"))

	rules[0].Remote[1].AnyOneOf = []string{"https://idp.example.com/realms/forge2"}
	g.Expect(c.UpdateMapping(ctx, "keycloak-mapping", rules)).To(Succeed())
	got, err = c.GetMapping(ctx, "keycloak-mapping")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(got.Rules[0].Remote[1].AnyOneOf).To(ConsistOf("https://idp.example.com/realms/forge2"))

	g.Expect(c.DeleteMapping(ctx, "keycloak-mapping")).To(Succeed())
	_, err = c.GetMapping(ctx, "keycloak-mapping")
	g.Expect(errors.Is(err, identity.ErrNotFound)).To(BeTrue())
}

func TestMapping_GetNotFoundMapsSentinel(t *testing.T) {
	g := NewGomegaWithT(t)
	c, _ := newClient(t)

	_, err := c.GetMapping(context.Background(), "missing")
	g.Expect(errors.Is(err, identity.ErrNotFound)).To(BeTrue())
}

func TestMapping_BadPasswordMapsUnauthorized(t *testing.T) {
	g := NewGomegaWithT(t)
	_, srv := newClient(t)
	// A wrong admin password fails at the token endpoint, before any
	// gophercloud call happens.
	badClient := identity.NewHTTPClient(srv.Endpoint(), identity.Credentials{
		Username: "admin",
		Password: "wrong",
	}, nil)

	err := badClient.CreateMapping(context.Background(), "m", nil)
	g.Expect(errors.Is(err, identity.ErrUnauthorized)).To(BeTrue())
}

func TestGroups_GetCreateRoundTrip(t *testing.T) {
	g := NewGomegaWithT(t)
	c, srv := newClient(t)
	ctx := context.Background()

	_, err := c.GetGroupByName(ctx, "federated-users", "domain-0001")
	g.Expect(errors.Is(err, identity.ErrNotFound)).To(BeTrue())

	created, err := c.CreateGroup(ctx, identity.Group{Name: "federated-users", DomainID: "domain-0001"})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(created.ID).NotTo(BeEmpty())

	got, err := c.GetGroupByName(ctx, "federated-users", "domain-0001")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(got.ID).To(Equal(created.ID))

	// Same name in a different domain stays NotFound.
	_, err = c.GetGroupByName(ctx, "federated-users", "other-domain")
	g.Expect(errors.Is(err, identity.ErrNotFound)).To(BeTrue())
	g.Expect(srv.GroupByName("federated-users", "domain-0001")).NotTo(BeNil())
}

func TestRolesProjectsAndAssignments(t *testing.T) {
	g := NewGomegaWithT(t)
	c, srv := newClient(t)
	ctx := context.Background()

	roleID := srv.SeedRole("member")
	projectID := srv.SeedProject("demo", "domain-0001")
	groupID := srv.SeedGroup("federated-users", "domain-0001")

	role, err := c.GetRoleByName(ctx, "member")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(role.ID).To(Equal(roleID))

	_, err = c.GetRoleByName(ctx, "missing-role")
	g.Expect(errors.Is(err, identity.ErrNotFound)).To(BeTrue())

	project, err := c.GetProjectByName(ctx, "demo", "domain-0001")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(project.ID).To(Equal(projectID))

	_, err = c.GetProjectByName(ctx, "demo", "other-domain")
	g.Expect(errors.Is(err, identity.ErrNotFound)).To(BeTrue())

	g.Expect(c.AssignRoleToGroupOnDomain(ctx, "domain-0001", groupID, roleID)).To(Succeed())
	g.Expect(c.AssignRoleToGroupOnProject(ctx, projectID, groupID, roleID)).To(Succeed())
	// Re-asserting is idempotent (PUT).
	g.Expect(c.AssignRoleToGroupOnDomain(ctx, "domain-0001", groupID, roleID)).To(Succeed())

	g.Expect(srv.RoleAssignments()).To(ConsistOf(
		"domain/domain-0001/group/"+groupID+"/role/"+roleID,
		"project/"+projectID+"/group/"+groupID+"/role/"+roleID,
	))
}
