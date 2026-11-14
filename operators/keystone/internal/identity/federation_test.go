// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package identity_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/federation"
	. "github.com/onsi/gomega"
	"k8s.io/utils/ptr"

	"github.com/c5c3/cobaltcore/operators/keystone/internal/identity"
	"github.com/c5c3/cobaltcore/operators/keystone/internal/identity/fake"
)

func TestIdentityProvider_CreateGetUpdateDelete(t *testing.T) {
	g := NewGomegaWithT(t)
	c, srv := newClient(t)
	ctx := context.Background()

	err := c.CreateIdentityProvider(ctx, identity.IdentityProvider{
		ID:        "keycloak",
		DomainID:  "domain-0001",
		Enabled:   ptr.To(true),
		RemoteIDs: []string{"https://idp.example.com/realms/cobaltcore"},
	})
	g.Expect(err).NotTo(HaveOccurred())

	got, err := c.GetIdentityProvider(ctx, "keycloak")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(got.ID).To(Equal("keycloak"))
	g.Expect(got.DomainID).To(Equal("domain-0001"))
	g.Expect(got.RemoteIDs).To(ConsistOf("https://idp.example.com/realms/cobaltcore"))

	err = c.UpdateIdentityProvider(ctx, "keycloak", ptr.To(false), ptr.To("federated corp IdP"),
		[]string{"https://idp.example.com/realms/cobaltcore2"})
	g.Expect(err).NotTo(HaveOccurred())
	rec := srv.IdentityProvider("keycloak")
	g.Expect(rec.Enabled).To(BeFalse())
	g.Expect(rec.Description).To(Equal("federated corp IdP"))
	g.Expect(rec.RemoteIDs).To(ConsistOf("https://idp.example.com/realms/cobaltcore2"))

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
			{Type: "HTTP_OIDC_ISS", AnyOneOf: []string{"https://idp.example.com/realms/cobaltcore"}},
		},
	}}

	g.Expect(c.CreateMapping(ctx, "keycloak-mapping", rules)).To(Succeed())
	g.Expect(srv.Mapping("keycloak-mapping")).NotTo(BeNil())

	got, err := c.GetMapping(ctx, "keycloak-mapping")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(got.Rules).To(HaveLen(1))
	g.Expect(got.Rules[0].Remote).To(HaveLen(2))
	g.Expect(got.Rules[0].Remote[1].AnyOneOf).To(ConsistOf("https://idp.example.com/realms/cobaltcore"))

	rules[0].Remote[1].AnyOneOf = []string{"https://idp.example.com/realms/cobaltcore2"}
	g.Expect(c.UpdateMapping(ctx, "keycloak-mapping", rules)).To(Succeed())
	got, err = c.GetMapping(ctx, "keycloak-mapping")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(got.Rules[0].Remote[1].AnyOneOf).To(ConsistOf("https://idp.example.com/realms/cobaltcore2"))

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

	// Probes report absence before the grant …
	has, err := c.HasRoleForGroupOnDomain(ctx, "domain-0001", groupID, roleID)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(has).To(BeFalse())

	g.Expect(c.AssignRoleToGroupOnDomain(ctx, "domain-0001", groupID, roleID)).To(Succeed())
	g.Expect(c.AssignRoleToGroupOnProject(ctx, projectID, groupID, roleID)).To(Succeed())
	// Re-asserting is idempotent (PUT).
	g.Expect(c.AssignRoleToGroupOnDomain(ctx, "domain-0001", groupID, roleID)).To(Succeed())

	// … and presence afterwards.
	has, err = c.HasRoleForGroupOnDomain(ctx, "domain-0001", groupID, roleID)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(has).To(BeTrue())
	has, err = c.HasRoleForGroupOnProject(ctx, projectID, groupID, roleID)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(has).To(BeTrue())

	g.Expect(srv.RoleAssignments()).To(ConsistOf(
		"domain/domain-0001/group/"+groupID+"/role/"+roleID,
		"project/"+projectID+"/group/"+groupID+"/role/"+roleID,
	))
}

// An API request that gets no answer ends at the request timeout behind its
// method and path, after a token request that succeeded, and leaves the
// identity provider in place.
func TestDeleteIdentityProvider_UnansweredRequestTimesOut(t *testing.T) {
	g := NewGomegaWithT(t)
	c, srv := newClientWithTimeout(t, 100*time.Millisecond)
	ctx := context.Background()
	g.Expect(c.CreateIdentityProvider(ctx, identity.IdentityProvider{ID: "keycloak"})).To(Succeed())
	srv.InjectFault(fake.Fault{Method: http.MethodDelete, Path: "/v3/OS-FEDERATION/identity_providers/keycloak", Hang: true})

	err := c.DeleteIdentityProvider(ctx, "keycloak")
	g.Expect(errors.Is(err, context.DeadlineExceeded)).To(BeTrue(), "got %v", err)
	g.Expect(err.Error()).To(HavePrefix("DELETE /OS-FEDERATION/identity_providers/keycloak: request did not complete within 100ms"))
	g.Expect(err.Error()).NotTo(ContainSubstring("authenticating against"))
	g.Expect(srv.IdentityProvider("keycloak")).NotTo(BeNil())
}

// A mapping request gophercloud sends ends at the request timeout too, behind
// the mapping operation, and matches no sentinel.
func TestDeleteMapping_UnansweredRequestTimesOut(t *testing.T) {
	g := NewGomegaWithT(t)
	c, srv := newClientWithTimeout(t, 100*time.Millisecond)
	srv.InjectFault(fake.Fault{Method: http.MethodDelete, Path: "/v3/OS-FEDERATION/mappings/m1", Hang: true})

	err := c.DeleteMapping(context.Background(), "m1")
	g.Expect(errors.Is(err, context.DeadlineExceeded)).To(BeTrue(), "got %v", err)
	g.Expect(err.Error()).To(HavePrefix("DELETE mapping m1: request did not complete within 100ms"))
	g.Expect(matchesSentinel(err)).To(BeFalse())
}

// The other three mapping operations end at the request timeout as
// DeleteMapping does, each behind its own operation, and match no sentinel.
func TestGetCreateUpdateMapping_UnansweredRequestTimesOut(t *testing.T) {
	rules := []federation.MappingRule{{
		Local:  []federation.RuleLocal{{User: &federation.RuleUser{Name: "{0}"}}},
		Remote: []federation.RuleRemote{{Type: "HTTP_OIDC_PREFERRED_USERNAME"}},
	}}
	cases := []struct {
		name   string
		method string
		prefix string
		call   func(identity.Client) error
	}{
		{
			name:   "GetMapping",
			method: http.MethodGet,
			prefix: "GET mapping m1: ",
			call: func(c identity.Client) error {
				_, err := c.GetMapping(context.Background(), "m1")
				return err
			},
		},
		{
			name:   "CreateMapping",
			method: http.MethodPut,
			prefix: "PUT mapping m1: ",
			call:   func(c identity.Client) error { return c.CreateMapping(context.Background(), "m1", rules) },
		},
		{
			name:   "UpdateMapping",
			method: http.MethodPatch,
			prefix: "PATCH mapping m1: ",
			call:   func(c identity.Client) error { return c.UpdateMapping(context.Background(), "m1", rules) },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			c, srv := newClientWithTimeout(t, 100*time.Millisecond)
			srv.InjectFault(fake.Fault{Method: tc.method, Path: "/v3/OS-FEDERATION/mappings/m1", Hang: true})

			err := tc.call(c)
			g.Expect(errors.Is(err, context.DeadlineExceeded)).To(BeTrue(), "got %v", err)
			g.Expect(err.Error()).To(HavePrefix(tc.prefix + "request did not complete within 100ms"))
			g.Expect(matchesSentinel(err)).To(BeFalse())
		})
	}
}
