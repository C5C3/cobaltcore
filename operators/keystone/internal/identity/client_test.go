// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package identity_test

import (
	"context"
	"errors"
	"testing"

	. "github.com/onsi/gomega"
	"k8s.io/utils/ptr"

	"github.com/c5c3/forge/operators/keystone/internal/identity"
	"github.com/c5c3/forge/operators/keystone/internal/identity/fake"
)

const adminPassword = "s3cret"

func newClient(t *testing.T) (identity.Client, *fake.Server) {
	t.Helper()
	srv := fake.NewServer(adminPassword)
	t.Cleanup(srv.Close)
	c := identity.NewHTTPClient(srv.Endpoint(), identity.Credentials{
		Username: "admin",
		Password: adminPassword,
	}, nil)
	return c, srv
}

func TestCreateAndGetDomain_RoundTrip(t *testing.T) {
	g := NewGomegaWithT(t)
	c, srv := newClient(t)
	ctx := context.Background()

	created, err := c.CreateDomain(ctx, identity.Domain{Name: "corp", Description: "corporate directory"})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(created.ID).NotTo(BeEmpty())
	g.Expect(created.Name).To(Equal("corp"))

	got, err := c.GetDomainByName(ctx, "corp")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(got.ID).To(Equal(created.ID))
	g.Expect(got.Description).To(Equal("corporate directory"))

	// The auth request precedes every API call (per-call authentication).
	reqs := srv.Requests()
	g.Expect(reqs[0]).To(Equal("POST /v3/auth/tokens"))
}

func TestGetDomainByName_NotFound(t *testing.T) {
	g := NewGomegaWithT(t)
	c, _ := newClient(t)

	_, err := c.GetDomainByName(context.Background(), "missing")
	g.Expect(err).To(HaveOccurred())
	g.Expect(errors.Is(err, identity.ErrNotFound)).To(BeTrue())
}

func TestCreateDomain_ConflictOnDuplicateName(t *testing.T) {
	g := NewGomegaWithT(t)
	c, srv := newClient(t)
	srv.SeedDomain("corp", "", true)

	_, err := c.CreateDomain(context.Background(), identity.Domain{Name: "corp"})
	g.Expect(err).To(HaveOccurred())
	g.Expect(errors.Is(err, identity.ErrConflict)).To(BeTrue())
}

func TestAuthenticate_BadPasswordIsUnauthorized(t *testing.T) {
	g := NewGomegaWithT(t)
	srv := fake.NewServer(adminPassword)
	t.Cleanup(srv.Close)
	c := identity.NewHTTPClient(srv.Endpoint(), identity.Credentials{
		Username: "admin",
		Password: "wrong",
	}, nil)

	_, err := c.GetDomainByName(context.Background(), "corp")
	g.Expect(err).To(HaveOccurred())
	g.Expect(errors.Is(err, identity.ErrUnauthorized)).To(BeTrue())
}

func TestUpdateDomain_PatchesEnabledAndDescription(t *testing.T) {
	g := NewGomegaWithT(t)
	c, srv := newClient(t)
	id := srv.SeedDomain("corp", "old", true)

	g.Expect(c.UpdateDomain(context.Background(), id, ptr.To(false), ptr.To("new"))).To(Succeed())

	d := srv.GetDomain(id)
	g.Expect(d.Enabled).To(BeFalse())
	g.Expect(d.Description).To(Equal("new"))

	// Nil fields must leave state untouched.
	g.Expect(c.UpdateDomain(context.Background(), id, nil, ptr.To("newer"))).To(Succeed())
	d = srv.GetDomain(id)
	g.Expect(d.Enabled).To(BeFalse(), "nil enabled must not re-enable the domain")
	g.Expect(d.Description).To(Equal("newer"))
}

func TestDeleteDomain_ForbiddenWhileEnabled(t *testing.T) {
	g := NewGomegaWithT(t)
	c, srv := newClient(t)
	id := srv.SeedDomain("corp", "", true)
	ctx := context.Background()

	// Enabled domain: keystone (and the fake) forbid the delete.
	err := c.DeleteDomain(ctx, id)
	g.Expect(err).To(HaveOccurred())
	g.Expect(errors.Is(err, identity.ErrForbidden)).To(BeTrue())

	// Disable, then delete succeeds.
	g.Expect(c.UpdateDomain(ctx, id, ptr.To(false), nil)).To(Succeed())
	g.Expect(c.DeleteDomain(ctx, id)).To(Succeed())
	g.Expect(srv.GetDomain(id)).To(BeNil())

	// Deleting again is a NotFound.
	err = c.DeleteDomain(ctx, id)
	g.Expect(errors.Is(err, identity.ErrNotFound)).To(BeTrue())
}

// A pure lookup sequence must never issue a mutating call — the request-log
// contract the adopt path relies on.
func TestGetDomainByName_IssuesNoMutatingRequests(t *testing.T) {
	g := NewGomegaWithT(t)
	c, srv := newClient(t)
	srv.SeedDomain("corp", "", true)

	_, err := c.GetDomainByName(context.Background(), "corp")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(srv.MutatingRequests()).To(BeEmpty())
}
