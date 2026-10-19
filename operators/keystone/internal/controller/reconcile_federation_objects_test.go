// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	commonconditions "github.com/c5c3/cobaltcore/internal/common/conditions"
	mctestutil "github.com/c5c3/cobaltcore/internal/common/testutil/multicluster"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	keystonev1alpha1 "github.com/c5c3/cobaltcore/operators/keystone/api/v1alpha1"
	"github.com/c5c3/cobaltcore/operators/keystone/internal/identity"
	identityfake "github.com/c5c3/cobaltcore/operators/keystone/internal/identity/fake"
)

// expectBackendEventContaining drains the reconciler's FakeRecorder and
// asserts SOME received event contains the substring (unlike
// expectBackendEvent, which pops events in emission order).
func expectBackendEventContaining(g Gomega, r *KeystoneIdentityBackendReconciler, substring string) {
	fakeRecorder := r.Recorder.(*record.FakeRecorder)
	var events []string
	for {
		select {
		case ev := <-fakeRecorder.Events:
			events = append(events, ev)
		default:
			g.Expect(events).To(ContainElement(ContainSubstring(substring)))
			return
		}
	}
}

// testOIDCBackend returns an OIDC backend attached to testKeystone() with one
// issuer-gated mapping rule and one declarative group carrying a domain-scoped
// role assignment. Status is empty — tests drive the full provisioning flow.
func testOIDCBackend(name, domain string) *keystonev1alpha1.KeystoneIdentityBackend {
	b := testIdentityBackend(name, domain)
	b.Spec.Type = keystonev1alpha1.IdentityBackendTypeOIDC
	b.Spec.LDAP = nil
	b.Spec.OIDC = &keystonev1alpha1.OIDCBackendSpec{
		Issuer:               "https://idp.example.com/realms/cobaltcore",
		ClientID:             "keystone",
		ClientSecretRef:      commonv1.SecretRefSpec{Name: name + "-client"},
		ProtocolID:           keystonev1alpha1.DefaultOIDCProtocolID,
		IdentityProviderName: name,
		RemoteIDAttribute:    keystonev1alpha1.DefaultOIDCRemoteIDAttribute,
	}
	b.Spec.Mappings = []keystonev1alpha1.MappingRuleSpec{{
		Local: []keystonev1alpha1.MappingLocalRuleSpec{{
			User:   &keystonev1alpha1.MappingUserSpec{Name: "{0}"},
			Groups: "{1}",
		}},
		Remote: []keystonev1alpha1.MappingRemoteRuleSpec{
			{Type: "HTTP_OIDC_PREFERRED_USERNAME"},
			{Type: "HTTP_OIDC_ISS", AnyOneOf: []string{"https://idp.example.com/realms/cobaltcore"}},
		},
	}}
	b.Spec.Groups = []keystonev1alpha1.FederationGroupSpec{{
		Name: "federated-users",
		RoleAssignments: []keystonev1alpha1.FederationRoleAssignmentSpec{
			{Role: "member", Domain: true},
		},
	}}
	b.Status = keystonev1alpha1.KeystoneIdentityBackendStatus{}
	return b
}

// Request paths of the federation objects testOIDCBackend("corp-oidc", ...)
// provisions, for the fault-injection tests.
const (
	corpOIDCIdentityProviderPath = "/v3/OS-FEDERATION/identity_providers/corp-oidc"
	corpOIDCMappingPath          = "/v3/OS-FEDERATION/mappings/corp-oidc-mapping"
	corpOIDCProtocolPath         = corpOIDCIdentityProviderPath + "/protocols/openid"
)

func TestOIDCBackend_ProvisionsFederationObjects(t *testing.T) {
	g := NewGomegaWithT(t)
	srv := identityfake.NewServer(testAdminPassword)
	t.Cleanup(srv.Close)
	srv.SeedRole("member")

	ks := testKeystoneWithReadyAPI()
	backend := testOIDCBackend("corp-oidc", "corp")
	r := newBackendTestReconciler(srv, ks, backend, testAdminSecret())

	_, err := reconcileBackendTwice(t, r, backend)
	g.Expect(err).NotTo(HaveOccurred())

	updated := getBackend(t, r.Client, "corp-oidc")
	g.Expect(updated.Status.DomainID).NotTo(BeEmpty())

	idp := srv.IdentityProvider("corp-oidc")
	g.Expect(idp).NotTo(BeNil())
	g.Expect(idp.DomainID).To(Equal(updated.Status.DomainID))
	g.Expect(idp.RemoteIDs).To(ConsistOf("https://idp.example.com/realms/cobaltcore"))
	g.Expect(idp.Enabled).To(BeTrue())

	g.Expect(srv.Mapping("corp-oidc-mapping")).NotTo(BeNil())
	proto := srv.Protocol("corp-oidc", "openid")
	g.Expect(proto).NotTo(BeNil())
	g.Expect(proto.MappingID).To(Equal("corp-oidc-mapping"))

	group := srv.GroupByName("federated-users", updated.Status.DomainID)
	g.Expect(group).NotTo(BeNil())
	g.Expect(srv.RoleAssignments()).To(HaveLen(1))
	g.Expect(srv.RoleAssignments()[0]).To(HavePrefix("domain/" + updated.Status.DomainID + "/group/" + group.ID))

	fedObjects := commonconditions.GetCondition(updated.Status.Conditions, conditionTypeFederationObjectsReady)
	g.Expect(fedObjects).NotTo(BeNil())
	g.Expect(fedObjects.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(fedObjects.Reason).To(Equal(conditionReasonFederationObjectsProvisioned))
	mappings := commonconditions.GetCondition(updated.Status.Conditions, conditionTypeMappingsReady)
	g.Expect(mappings).NotTo(BeNil())
	g.Expect(mappings.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(mappings.Reason).To(Equal(conditionReasonMappingsApplied))

	expectBackendEventContaining(g, r, "Normal IdentityProviderCreated")
}

// TestOIDCBackend_SteadyStateIssuesNoMutatingRequests covers each mapping
// shape whose converged pass must see no drift against the rules the identity
// API returns.
func TestOIDCBackend_SteadyStateIssuesNoMutatingRequests(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*keystonev1alpha1.KeystoneIdentityBackend)
	}{
		{
			name:   "default mapping",
			mutate: func(*keystonev1alpha1.KeystoneIdentityBackend) {},
		},
		{
			// The converted rules omit the empty roles list on the wire.
			name: "project without roles",
			mutate: func(b *keystonev1alpha1.KeystoneIdentityBackend) {
				b.Spec.Mappings[0].Local[0].Projects = []keystonev1alpha1.MappingProjectSpec{{Name: "demo"}}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			srv := identityfake.NewServer(testAdminPassword)
			t.Cleanup(srv.Close)
			srv.SeedRole("member")

			ks := testKeystoneWithReadyAPI()
			backend := testOIDCBackend("corp-oidc", "corp")
			tc.mutate(backend)
			r := newBackendTestReconciler(srv, ks, backend, testAdminSecret())

			_, err := reconcileBackendTwice(t, r, backend)
			g.Expect(err).NotTo(HaveOccurred())
			converged := len(srv.MutatingRequests())
			g.Expect(converged).To(BeNumerically(">", 0), "the first pass must have provisioned objects")

			// A steady-state pass must be read-only against the identity API.
			_, err = r.Reconcile(context.Background(), backendRequest(backend))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(srv.MutatingRequests()).To(HaveLen(converged),
				"a converged pass must not issue mutating federation requests")
		})
	}
}

func TestOIDCBackend_RulesDriftTriggersSingleMappingUpdate(t *testing.T) {
	g := NewGomegaWithT(t)
	srv := identityfake.NewServer(testAdminPassword)
	t.Cleanup(srv.Close)
	srv.SeedRole("member")

	ks := testKeystoneWithReadyAPI()
	backend := testOIDCBackend("corp-oidc", "corp")
	r := newBackendTestReconciler(srv, ks, backend, testAdminSecret())

	_, err := reconcileBackendTwice(t, r, backend)
	g.Expect(err).NotTo(HaveOccurred())

	// Drift the rules out-of-band (as if edited via the CLI) — the next pass
	// must converge them back with exactly one PATCH.
	updated := getBackend(t, r.Client, "corp-oidc")
	updated.Spec.Mappings[0].Remote[1].AnyOneOf = []string{"https://idp.example.com/realms/other"}
	g.Expect(r.Update(context.Background(), updated)).To(Succeed())

	_, err = r.Reconcile(context.Background(), backendRequest(backend))
	g.Expect(err).NotTo(HaveOccurred())

	var patches []string
	for _, req := range srv.Requests() {
		if strings.HasPrefix(req, "PATCH /v3/OS-FEDERATION/mappings/") {
			patches = append(patches, req)
		}
	}
	g.Expect(patches).To(HaveLen(1), "rules drift must trigger exactly one UpdateMapping")
	expectBackendEventContaining(g, r, "Normal MappingUpdated")
}

func TestOIDCBackend_NoMappingRulesStaysPending(t *testing.T) {
	g := NewGomegaWithT(t)
	srv := identityfake.NewServer(testAdminPassword)
	t.Cleanup(srv.Close)

	ks := testKeystoneWithReadyAPI()
	backend := testOIDCBackend("corp-oidc", "corp")
	backend.Spec.Mappings = nil
	r := newBackendTestReconciler(srv, ks, backend, testAdminSecret())

	_, err := reconcileBackendTwice(t, r, backend)
	g.Expect(err).NotTo(HaveOccurred())

	updated := getBackend(t, r.Client, "corp-oidc")
	fedObjects := commonconditions.GetCondition(updated.Status.Conditions, conditionTypeFederationObjectsReady)
	g.Expect(fedObjects.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(fedObjects.Reason).To(Equal(conditionReasonNoMappingRules))
	mappings := commonconditions.GetCondition(updated.Status.Conditions, conditionTypeMappingsReady)
	g.Expect(mappings.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(mappings.Reason).To(Equal(conditionReasonNoMappingRules))

	// The domain create is the only permitted mutation — no federation object
	// may be half-provisioned.
	for _, req := range srv.MutatingRequests() {
		g.Expect(req).To(HavePrefix("POST /v3/domains"))
	}
}

func TestOIDCBackend_MissingRoleWaitsWithBoundedPoll(t *testing.T) {
	g := NewGomegaWithT(t)
	srv := identityfake.NewServer(testAdminPassword)
	t.Cleanup(srv.Close)
	// Deliberately NOT seeding the "member" role.

	ks := testKeystoneWithReadyAPI()
	backend := testOIDCBackend("corp-oidc", "corp")
	r := newBackendTestReconciler(srv, ks, backend, testAdminSecret())

	result, err := reconcileBackendTwice(t, r, backend)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueDatabaseWait))

	updated := getBackend(t, r.Client, "corp-oidc")
	mappings := commonconditions.GetCondition(updated.Status.Conditions, conditionTypeMappingsReady)
	g.Expect(mappings.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(mappings.Reason).To(Equal(conditionReasonRoleOrProjectNotFound))
	g.Expect(mappings.Message).To(ContainSubstring(`role "member" not found`))
}

// TestOIDCBackend_IdentityAPIFaultReportsIdentityAPIError covers a failure in
// each step of the federation provisioning before it first succeeds: the step
// sets its condition to IdentityAPIError and returns the error, the steps
// after it do not run, and the first pass after the fault provisions every
// object. The domain step runs unfaulted, so DomainReady is True throughout.
func TestOIDCBackend_IdentityAPIFaultReportsIdentityAPIError(t *testing.T) {
	cases := []struct {
		name      string
		fault     identityfake.Fault
		condition string
		messages  []string
		// mappingsApplied is set when the failing step runs after the mapping
		// step (MappingsReady True); otherwise the other federation condition
		// is still unset.
		mappingsApplied bool
		// projectScope replaces the group's domain-scoped role assignment
		// with one on project "ops" in domain "shared".
		projectScope bool
	}{
		{
			name:      "identity provider lookup fails",
			fault:     identityfake.Fault{Method: http.MethodGet, Path: corpOIDCIdentityProviderPath, Status: http.StatusServiceUnavailable},
			condition: conditionTypeFederationObjectsReady,
			messages:  []string{`looking up identity provider "corp-oidc"`, "503"},
		},
		{
			name:      "identity provider create fails",
			fault:     identityfake.Fault{Method: http.MethodPut, Path: corpOIDCIdentityProviderPath, Status: http.StatusInternalServerError},
			condition: conditionTypeFederationObjectsReady,
			messages:  []string{`creating identity provider "corp-oidc"`, "500"},
		},
		{
			// The mapping calls travel through gophercloud, whose error text
			// follows the controller's prefix.
			name:      "mapping lookup fails",
			fault:     identityfake.Fault{Method: http.MethodGet, Path: corpOIDCMappingPath, Status: http.StatusServiceUnavailable},
			condition: conditionTypeMappingsReady,
			messages:  []string{`looking up mapping "corp-oidc-mapping"`, "503"},
		},
		{
			name:      "mapping create fails",
			fault:     identityfake.Fault{Method: http.MethodPut, Path: corpOIDCMappingPath, Status: http.StatusInternalServerError},
			condition: conditionTypeMappingsReady,
			messages:  []string{`creating mapping "corp-oidc-mapping"`, "500"},
		},
		{
			name:      "group lookup fails",
			fault:     identityfake.Fault{Method: http.MethodGet, Path: "/v3/groups", Status: http.StatusServiceUnavailable},
			condition: conditionTypeMappingsReady,
			messages:  []string{`ensuring group "federated-users"`, "503"},
		},
		{
			name:      "group create fails",
			fault:     identityfake.Fault{Method: http.MethodPost, Path: "/v3/groups", Status: http.StatusInternalServerError},
			condition: conditionTypeMappingsReady,
			messages:  []string{`ensuring group "federated-users"`, "500"},
		},
		{
			name:      "role lookup fails",
			fault:     identityfake.Fault{Method: http.MethodGet, Path: "/v3/roles", Status: http.StatusServiceUnavailable},
			condition: conditionTypeMappingsReady,
			messages:  []string{`looking up role "member"`, "503"},
		},
		{
			name:      "role assignment probe fails",
			fault:     identityfake.Fault{Method: http.MethodHead, Path: "/v3/domains/", Status: http.StatusServiceUnavailable},
			condition: conditionTypeMappingsReady,
			messages:  []string{`checking role "member" of group "federated-users" on domain`, "503"},
		},
		{
			name:      "role assignment fails",
			fault:     identityfake.Fault{Method: http.MethodPut, Path: "/v3/domains/", Status: http.StatusInternalServerError},
			condition: conditionTypeMappingsReady,
			messages:  []string{`assigning role "member" to group "federated-users" on domain`, "500"},
		},
		{
			// The query keeps the fault off the backend domain's own lookup
			// (name=corp), which runs first.
			name:         "project domain lookup fails",
			fault:        identityfake.Fault{Method: http.MethodGet, Path: "/v3/domains", Query: "name=shared", Status: http.StatusServiceUnavailable},
			condition:    conditionTypeMappingsReady,
			messages:     []string{`looking up domain "shared"`, "503"},
			projectScope: true,
		},
		{
			name:         "project lookup fails",
			fault:        identityfake.Fault{Method: http.MethodGet, Path: "/v3/projects", Status: http.StatusServiceUnavailable},
			condition:    conditionTypeMappingsReady,
			messages:     []string{`looking up project "ops"`, "503"},
			projectScope: true,
		},
		{
			name:         "project role assignment probe fails",
			fault:        identityfake.Fault{Method: http.MethodHead, Path: "/v3/projects/", Status: http.StatusServiceUnavailable},
			condition:    conditionTypeMappingsReady,
			messages:     []string{`checking role "member" of group "federated-users" on project "ops"`, "503"},
			projectScope: true,
		},
		{
			name:         "project role assignment fails",
			fault:        identityfake.Fault{Method: http.MethodPut, Path: "/v3/projects/", Status: http.StatusInternalServerError},
			condition:    conditionTypeMappingsReady,
			messages:     []string{`assigning role "member" to group "federated-users" on project "ops"`, "500"},
			projectScope: true,
		},
		{
			name:            "protocol lookup fails",
			fault:           identityfake.Fault{Method: http.MethodGet, Path: corpOIDCProtocolPath, Status: http.StatusServiceUnavailable},
			condition:       conditionTypeFederationObjectsReady,
			messages:        []string{`looking up protocol "openid"`, "503"},
			mappingsApplied: true,
		},
		{
			name:            "protocol create fails",
			fault:           identityfake.Fault{Method: http.MethodPut, Path: corpOIDCProtocolPath, Status: http.StatusInternalServerError},
			condition:       conditionTypeFederationObjectsReady,
			messages:        []string{`creating protocol "openid"`, "500"},
			mappingsApplied: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			srv := identityfake.NewServer(testAdminPassword)
			t.Cleanup(srv.Close)
			srv.SeedRole("member")

			ks := testKeystoneWithReadyAPI()
			backend := testOIDCBackend("corp-oidc", "corp")
			if tc.projectScope {
				srv.SeedProject("ops", srv.SeedDomain("shared", "", true))
				backend.Spec.Groups[0].RoleAssignments = []keystonev1alpha1.FederationRoleAssignmentSpec{{
					Role:    "member",
					Project: &keystonev1alpha1.FederationProjectScopeSpec{Name: "ops", DomainName: "shared"},
				}}
			}
			r := newBackendTestReconciler(srv, ks, backend, testAdminSecret())
			srv.InjectFault(tc.fault)

			result, err := reconcileBackendTwice(t, r, backend)
			g.Expect(err).To(HaveOccurred())
			g.Expect(result.IsZero()).To(BeTrue())

			failed := getBackend(t, r.Client, "corp-oidc")
			domain := commonconditions.GetCondition(failed.Status.Conditions, conditionTypeDomainReady)
			g.Expect(domain.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(domain.Reason).To(Equal(conditionReasonDomainProvisioned))
			cond := commonconditions.GetCondition(failed.Status.Conditions, tc.condition)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(conditionReasonIdentityAPIError))
			for _, m := range tc.messages {
				g.Expect(cond.Message).To(ContainSubstring(m))
			}
			otherType := conditionTypeMappingsReady
			if tc.condition == conditionTypeMappingsReady {
				otherType = conditionTypeFederationObjectsReady
			}
			other := commonconditions.GetCondition(failed.Status.Conditions, otherType)
			if tc.mappingsApplied {
				g.Expect(other).NotTo(BeNil())
				g.Expect(other.Status).To(Equal(metav1.ConditionTrue))
				g.Expect(other.Reason).To(Equal(conditionReasonMappingsApplied))
			} else {
				g.Expect(other).To(BeNil(), "the steps after the failing one must not run")
			}

			srv.ClearFaults()
			_, err = r.Reconcile(context.Background(), backendRequest(backend))
			g.Expect(err).NotTo(HaveOccurred())

			provisioned := getBackend(t, r.Client, "corp-oidc")
			fedObjects := commonconditions.GetCondition(provisioned.Status.Conditions, conditionTypeFederationObjectsReady)
			g.Expect(fedObjects.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(fedObjects.Reason).To(Equal(conditionReasonFederationObjectsProvisioned))
			mappings := commonconditions.GetCondition(provisioned.Status.Conditions, conditionTypeMappingsReady)
			g.Expect(mappings.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(mappings.Reason).To(Equal(conditionReasonMappingsApplied))
			g.Expect(srv.IdentityProvider("corp-oidc")).NotTo(BeNil())
			g.Expect(srv.Mapping("corp-oidc-mapping")).NotTo(BeNil())
			g.Expect(srv.Protocol("corp-oidc", "openid")).NotTo(BeNil())
			g.Expect(srv.RoleAssignments()).To(HaveLen(1))
		})
	}
}

// Failed drift writes on provisioned federation objects surface as reconcile
// errors while FederationObjectsReady and MappingsReady stay True: the
// IdentityAPIError demotion is transient and dropped. Each write goes through
// once the identity API answers again.
func TestOIDCBackend_UpdateFaultPreservesProvisionedFederation(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	srv := identityfake.NewServer(testAdminPassword)
	t.Cleanup(srv.Close)
	srv.SeedRole("member")

	ks := testKeystoneWithReadyAPI()
	backend := testOIDCBackend("corp-oidc", "corp")
	r := newBackendTestReconciler(srv, ks, backend, testAdminSecret())

	_, err := reconcileBackendTwice(t, r, backend)
	g.Expect(err).NotTo(HaveOccurred())

	// The description drifts the identity provider (and the domain, whose
	// unfaulted PATCH runs first); the extra remote rule drifts the mapping;
	// the out-of-band repoint drifts the protocol's mapping_id.
	live := getBackend(t, r.Client, "corp-oidc")
	live.Spec.Domain.Description = "renamed"
	live.Spec.Mappings[0].Remote = append(live.Spec.Mappings[0].Remote, keystonev1alpha1.MappingRemoteRuleSpec{Type: "HTTP_OIDC_EMAIL"})
	g.Expect(r.Update(ctx, live)).To(Succeed())
	idc := identity.NewHTTPClient(srv.Endpoint(), identity.Credentials{Username: "admin", Password: testAdminPassword}, nil)
	g.Expect(idc.UpdateProtocol(ctx, "corp-oidc", "openid", "other-mapping")).To(Succeed())

	srv.InjectFault(identityfake.Fault{Method: http.MethodPatch, Path: corpOIDCIdentityProviderPath, Status: http.StatusInternalServerError})
	_, err = r.Reconcile(ctx, backendRequest(backend))
	g.Expect(err).To(MatchError(ContainSubstring(`updating identity provider "corp-oidc"`)))
	fedObjects := commonconditions.GetCondition(getBackend(t, r.Client, "corp-oidc").Status.Conditions, conditionTypeFederationObjectsReady)
	g.Expect(fedObjects.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(srv.IdentityProvider("corp-oidc").Description).To(BeEmpty(), "the failed PATCH wrote nothing")

	srv.ClearFaults()
	srv.InjectFault(identityfake.Fault{Method: http.MethodPatch, Path: corpOIDCMappingPath, Status: http.StatusInternalServerError})
	_, err = r.Reconcile(ctx, backendRequest(backend))
	g.Expect(err).To(MatchError(ContainSubstring(`updating mapping "corp-oidc-mapping"`)))
	mappings := commonconditions.GetCondition(getBackend(t, r.Client, "corp-oidc").Status.Conditions, conditionTypeMappingsReady)
	g.Expect(mappings.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(string(srv.Mapping("corp-oidc-mapping").Rules)).NotTo(ContainSubstring("HTTP_OIDC_EMAIL"), "the failed PATCH wrote nothing")

	srv.ClearFaults()
	srv.InjectFault(identityfake.Fault{Method: http.MethodPatch, Path: corpOIDCProtocolPath, Status: http.StatusInternalServerError})
	_, err = r.Reconcile(ctx, backendRequest(backend))
	g.Expect(err).To(MatchError(ContainSubstring(`updating protocol "openid"`)))
	fedObjects = commonconditions.GetCondition(getBackend(t, r.Client, "corp-oidc").Status.Conditions, conditionTypeFederationObjectsReady)
	g.Expect(fedObjects.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(srv.Protocol("corp-oidc", "openid").MappingID).To(Equal("other-mapping"), "the failed PATCH wrote nothing")

	srv.ClearFaults()
	_, err = r.Reconcile(ctx, backendRequest(backend))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(srv.IdentityProvider("corp-oidc").Description).To(Equal("renamed"))
	g.Expect(string(srv.Mapping("corp-oidc-mapping").Rules)).To(ContainSubstring("HTTP_OIDC_EMAIL"))
	g.Expect(srv.Protocol("corp-oidc", "openid").MappingID).To(Equal("corp-oidc-mapping"))
}

func TestOIDCBackendDelete_TearsDownInReverseDependencyOrder(t *testing.T) {
	g := NewGomegaWithT(t)
	srv := identityfake.NewServer(testAdminPassword)
	t.Cleanup(srv.Close)
	srv.SeedRole("member")

	ks := testKeystoneWithReadyAPI()
	backend := testOIDCBackend("corp-oidc", "corp")
	r := newBackendTestReconciler(srv, ks, backend, testAdminSecret())

	_, err := reconcileBackendTwice(t, r, backend)
	g.Expect(err).NotTo(HaveOccurred())

	// Delete the CR: the fake client keeps it (finalizer) with the
	// DeletionTimestamp set, so the next pass funnels through reconcileDelete.
	updated := getBackend(t, r.Client, "corp-oidc")
	g.Expect(r.Delete(context.Background(), updated)).To(Succeed())

	result, err := r.Reconcile(context.Background(), backendRequest(backend))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue())

	// The federation objects are gone …
	g.Expect(srv.IdentityProvider("corp-oidc")).To(BeNil())
	g.Expect(srv.Mapping("corp-oidc-mapping")).To(BeNil())

	// … and were removed protocol → mapping → identity provider.
	var protoIdx, mappingIdx, idpIdx int
	for i, req := range srv.Requests() {
		switch {
		case strings.HasPrefix(req, "DELETE /v3/OS-FEDERATION/identity_providers/") && strings.Contains(req, "/protocols/"):
			protoIdx = i
		case strings.HasPrefix(req, "DELETE /v3/OS-FEDERATION/mappings/"):
			mappingIdx = i
		case strings.HasPrefix(req, "DELETE /v3/OS-FEDERATION/identity_providers/"):
			idpIdx = i
		}
	}
	g.Expect(protoIdx).To(BeNumerically(">", 0))
	g.Expect(mappingIdx).To(BeNumerically(">", protoIdx), "the mapping must outlive the protocol referencing it")
	g.Expect(idpIdx).To(BeNumerically(">", mappingIdx), "the identity provider must be removed last")

	expectBackendEventContaining(g, r, "Normal FederationObjectsDeleted")
	// Retain policy (the fixture default): the domain stays.
	g.Expect(srv.GetDomainByName("corp")).NotTo(BeNil())
}

// TestOIDCBackendDelete_TeardownFaultRetriesOnBoundedPoll covers a failed
// delete in each teardown step: the teardown warns, keeps the finalizer and
// every object not yet deleted, and retries on the bounded poll; the first
// pass after the fault finishes the teardown and releases the backend.
func TestOIDCBackendDelete_TeardownFaultRetriesOnBoundedPoll(t *testing.T) {
	cases := []struct {
		name  string
		path  string
		event string
		// protocolLeft and mappingLeft report which objects the faulted pass
		// leaves in place; the identity provider, deleted last, always is.
		protocolLeft bool
		mappingLeft  bool
	}{
		{
			name:         "protocol delete fails",
			path:         corpOIDCProtocolPath,
			event:        `Deleting protocol "openid" failed`,
			protocolLeft: true,
			mappingLeft:  true,
		},
		{
			name:        "mapping delete fails",
			path:        corpOIDCMappingPath,
			event:       `Deleting mapping "corp-oidc-mapping" failed`,
			mappingLeft: true,
		},
		{
			name:  "identity provider delete fails",
			path:  corpOIDCIdentityProviderPath,
			event: `Deleting identity provider "corp-oidc" failed`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			ctx := context.Background()
			srv := identityfake.NewServer(testAdminPassword)
			t.Cleanup(srv.Close)
			srv.SeedRole("member")

			ks := testKeystoneWithReadyAPI()
			backend := testOIDCBackend("corp-oidc", "corp")
			r := newBackendTestReconciler(srv, ks, backend, testAdminSecret())

			_, err := reconcileBackendTwice(t, r, backend)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(r.Delete(ctx, getBackend(t, r.Client, "corp-oidc"))).To(Succeed())
			srv.InjectFault(identityfake.Fault{Method: http.MethodDelete, Path: tc.path, Status: http.StatusInternalServerError})

			result, err := r.Reconcile(ctx, backendRequest(backend))
			g.Expect(err).NotTo(HaveOccurred(), "a failed teardown step retries on the poll, not through the error path")
			g.Expect(result.RequeueAfter).To(Equal(RequeueDatabaseWait))
			g.Expect(backendHasFinalizer(t, r.Client, "corp-oidc")).To(BeTrue())
			expectBackendEventContaining(g, r, "Warning FederationTeardownFailed "+tc.event)
			g.Expect(srv.Protocol("corp-oidc", "openid") != nil).To(Equal(tc.protocolLeft), "protocol present")
			g.Expect(srv.Mapping("corp-oidc-mapping") != nil).To(Equal(tc.mappingLeft), "mapping present")
			g.Expect(srv.IdentityProvider("corp-oidc")).NotTo(BeNil())

			srv.ClearFaults()
			result, err = r.Reconcile(ctx, backendRequest(backend))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(result.IsZero()).To(BeTrue())
			g.Expect(srv.Protocol("corp-oidc", "openid")).To(BeNil())
			g.Expect(srv.Mapping("corp-oidc-mapping")).To(BeNil())
			g.Expect(srv.IdentityProvider("corp-oidc")).To(BeNil())
			expectBackendEventContaining(g, r, "Normal FederationObjectsDeleted")
			var gone keystonev1alpha1.KeystoneIdentityBackend
			err = r.Get(ctx, backendRequest(backend).NamespacedName, &gone)
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "finalizer released, backend gone")
		})
	}
}

func TestOIDCBackendDelete_ToleratesObjectsAlreadyGone(t *testing.T) {
	g := NewGomegaWithT(t)
	srv := identityfake.NewServer(testAdminPassword)
	t.Cleanup(srv.Close)

	ks := testKeystoneWithReadyAPI()
	backend := testOIDCBackend("corp-oidc", "corp")
	now := metav1.Now()
	backend.DeletionTimestamp = &now
	backend.Finalizers = []string{identityBackendFinalizerName}
	// No federation objects were ever provisioned (e.g. the backend never got
	// past a pending state) — teardown must still release the finalizer.
	r := newBackendTestReconciler(srv, ks, backend, testAdminSecret())

	result, err := r.Reconcile(context.Background(), backendRequest(backend))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue())

	var gone keystonev1alpha1.KeystoneIdentityBackend
	err = r.Get(context.Background(), backendRequest(backend).NamespacedName, &gone)
	g.Expect(err).To(HaveOccurred(), "the finalizer must be released and the CR removed")
}

// --- A placed Keystone tears its federation objects down through the proxy ---

// targetAPIServer stands in for the target cluster's API server. It records the
// path of every request the service proxy rewrote and forwards it to the fake
// identity server with the services/proxy prefix stripped, which is what the
// API server does for a services/proxy call.
func targetAPIServer(t *testing.T, srv *identityfake.Server) (*httptest.Server, *[]string) {
	t.Helper()

	base := strings.TrimSuffix(srv.Endpoint(), "/v3")
	var paths []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)

		_, tail, isProxy := strings.Cut(r.URL.Path, "/proxy")
		if !isProxy {
			http.Error(w, "not a services/proxy request: "+r.URL.Path, http.StatusNotFound)
			return
		}

		//nolint:gosec // G704 false positive: the forwarding target is the test's own identity fake.
		forwarded, err := http.NewRequestWithContext(r.Context(), r.Method, base+tail, r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		forwarded.Header = r.Header.Clone()
		forwarded.URL.RawQuery = r.URL.RawQuery

		resp, err := http.DefaultClient.Do(forwarded) //nolint:gosec // G704 false positive: same fake, see above.
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer func() { _ = resp.Body.Close() }()

		for key, values := range resp.Header {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(api.Close)

	return api, &paths
}

// TestOIDCBackendDelete_PlacedKeystoneTearsDownThroughTheServiceProxy pins the
// teardown of a backend whose Keystone runs on a target cluster. Both halves
// live there: the admin-password Secret the spec references is seeded on the
// target only, and the identity API answers only through that cluster's API
// server. Reading either from the management cluster fails open and retains the
// federation objects, which is the regression this covers.
func TestOIDCBackendDelete_PlacedKeystoneTearsDownThroughTheServiceProxy(t *testing.T) {
	g := NewGomegaWithT(t)
	srv := identityfake.NewServer(testAdminPassword)
	t.Cleanup(srv.Close)
	srv.SeedRole("member")
	api, proxied := targetAPIServer(t, srv)

	ks := testKeystoneWithReadyAPI()
	ks.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "remote-a"}
	backend := testOIDCBackend("corp-oidc", "corp")
	r := newBackendTestReconciler(srv, ks, backend, testAdminSecret())

	_, err := reconcileBackendTwice(t, r, backend)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(srv.IdentityProvider("corp-oidc")).NotTo(BeNil())

	// From here the Keystone is placed for real: its admin-password Secret only
	// exists on the target, and the identity client is the production one, so
	// the teardown has to reach both through the resolved cluster.
	g.Expect(r.Delete(context.Background(), testAdminSecret())).To(Succeed())
	target := fake.NewClientBuilder().WithScheme(testScheme()).WithRuntimeObjects(testAdminSecret()).Build()
	r.IdentityClientFactory = nil
	r.Resolver = mctestutil.ResolverFor(mctestutil.TargetCluster{
		Client: target,
		Config: &rest.Config{Host: api.URL},
	})

	updated := getBackend(t, r.Client, "corp-oidc")
	g.Expect(r.Delete(context.Background(), updated)).To(Succeed())

	result, err := r.Reconcile(context.Background(), backendRequest(backend))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue())

	g.Expect(*proxied).NotTo(BeEmpty(), "the teardown must have gone through the target API server")
	for _, path := range *proxied {
		g.Expect(path).To(HavePrefix("/api/v1/namespaces/default/services/http:test-keystone:5000/proxy/v3"),
			"every identity call of a placed Keystone is a services/proxy call")
	}

	g.Expect(srv.IdentityProvider("corp-oidc")).To(BeNil())
	g.Expect(srv.Mapping("corp-oidc-mapping")).To(BeNil())
	g.Expect(srv.Protocol("corp-oidc", "openid")).To(BeNil())
	expectBackendEventContaining(g, r, "Normal FederationObjectsDeleted")

	var gone keystonev1alpha1.KeystoneIdentityBackend
	err = r.Get(context.Background(), backendRequest(backend).NamespacedName, &gone)
	g.Expect(err).To(HaveOccurred(), "the finalizer must be released once the teardown succeeded")
}

// TestOIDCBackendDelete_UnresolvableTargetRetainsObjectsAndReleases covers the
// deregistered target cluster: the federation objects are unreachable, so the
// teardown warns and releases the finalizer rather than holding the backend
// hostage to a cluster that is gone.
func TestOIDCBackendDelete_UnresolvableTargetRetainsObjectsAndReleases(t *testing.T) {
	g := NewGomegaWithT(t)
	srv := identityfake.NewServer(testAdminPassword)
	t.Cleanup(srv.Close)
	srv.SeedRole("member")

	ks := testKeystoneWithReadyAPI()
	ks.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "nowhere"}
	backend := testOIDCBackend("corp-oidc", "corp")
	r := newBackendTestReconciler(srv, ks, backend, testAdminSecret())

	_, err := reconcileBackendTwice(t, r, backend)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(srv.IdentityProvider("corp-oidc")).NotTo(BeNil())

	r.Resolver = unresolvableResolver{}

	updated := getBackend(t, r.Client, "corp-oidc")
	g.Expect(r.Delete(context.Background(), updated)).To(Succeed())

	result, err := r.Reconcile(context.Background(), backendRequest(backend))
	g.Expect(err).NotTo(HaveOccurred(), "an unreachable target must not fail the pass")
	g.Expect(result.IsZero()).To(BeTrue())

	expectBackendEventContaining(g, r, "Warning FederationTeardownFailed")
	g.Expect(srv.IdentityProvider("corp-oidc")).NotTo(BeNil(),
		"objects on an unreachable cluster are retained, not silently reported as deleted")

	var gone keystonev1alpha1.KeystoneIdentityBackend
	err = r.Get(context.Background(), backendRequest(backend).NamespacedName, &gone)
	g.Expect(err).To(HaveOccurred(), "the finalizer must be released even though the objects stay")
}

// testSAMLBackend returns a SAML backend attached to testKeystone() with one
// IdP-gated mapping rule and one declarative group carrying a domain-scoped
// role assignment. Status is empty — tests drive the full provisioning flow.
func testSAMLBackend(name, domain string) *keystonev1alpha1.KeystoneIdentityBackend {
	b := testIdentityBackend(name, domain)
	b.Spec.Type = keystonev1alpha1.IdentityBackendTypeSAML
	b.Spec.LDAP = nil
	b.Spec.SAML = &keystonev1alpha1.SAMLBackendSpec{
		IdPEntityID:          "https://idp.example.com/realms/cobaltcore",
		IdPMetadata:          keystonev1alpha1.SAMLIdPMetadataSpec{SecretRef: &commonv1.SecretRefSpec{Name: name + "-idp-metadata"}},
		ProtocolID:           keystonev1alpha1.DefaultSAMLProtocolID,
		IdentityProviderName: name,
		RemoteIDAttribute:    keystonev1alpha1.DefaultSAMLRemoteIDAttribute,
		ForwardAttributes:    []string{"username"},
	}
	b.Spec.Mappings = []keystonev1alpha1.MappingRuleSpec{{
		Local: []keystonev1alpha1.MappingLocalRuleSpec{{
			User:   &keystonev1alpha1.MappingUserSpec{Name: "{0}"},
			Groups: "{1}",
		}},
		Remote: []keystonev1alpha1.MappingRemoteRuleSpec{
			{Type: "HTTP_MELLON_USERNAME"},
			{Type: "HTTP_MELLON_IDP", AnyOneOf: []string{"https://idp.example.com/realms/cobaltcore"}},
		},
	}}
	b.Spec.Groups = []keystonev1alpha1.FederationGroupSpec{{
		Name: "federated-users",
		RoleAssignments: []keystonev1alpha1.FederationRoleAssignmentSpec{
			{Role: "member", Domain: true},
		},
	}}
	b.Status = keystonev1alpha1.KeystoneIdentityBackendStatus{}
	return b
}

func TestSAMLBackend_ProvisionsFederationObjects(t *testing.T) {
	g := NewGomegaWithT(t)
	srv := identityfake.NewServer(testAdminPassword)
	t.Cleanup(srv.Close)
	srv.SeedRole("member")

	ks := testKeystoneWithReadyAPI()
	backend := testSAMLBackend("corp-saml", "corp")
	r := newBackendTestReconciler(srv, ks, backend, testAdminSecret())

	_, err := reconcileBackendTwice(t, r, backend)
	g.Expect(err).NotTo(HaveOccurred())

	updated := getBackend(t, r.Client, "corp-saml")
	g.Expect(updated.Status.DomainID).NotTo(BeEmpty())

	idp := srv.IdentityProvider("corp-saml")
	g.Expect(idp).NotTo(BeNil())
	g.Expect(idp.DomainID).To(Equal(updated.Status.DomainID))
	// The identity provider's remote ID is the SAML IdP entityID (not an OIDC
	// issuer), proving FederationRemoteID resolves the right source.
	g.Expect(idp.RemoteIDs).To(ConsistOf("https://idp.example.com/realms/cobaltcore"))
	g.Expect(idp.Enabled).To(BeTrue())

	g.Expect(srv.Mapping("corp-saml-mapping")).NotTo(BeNil())
	// The protocol binds under the SAML default protocolID "mapped".
	proto := srv.Protocol("corp-saml", "mapped")
	g.Expect(proto).NotTo(BeNil())
	g.Expect(proto.MappingID).To(Equal("corp-saml-mapping"))

	fedObjects := commonconditions.GetCondition(updated.Status.Conditions, conditionTypeFederationObjectsReady)
	g.Expect(fedObjects).NotTo(BeNil())
	g.Expect(fedObjects.Status).To(Equal(metav1.ConditionTrue))
	mappings := commonconditions.GetCondition(updated.Status.Conditions, conditionTypeMappingsReady)
	g.Expect(mappings).NotTo(BeNil())
	g.Expect(mappings.Status).To(Equal(metav1.ConditionTrue))

	expectBackendEventContaining(g, r, "Normal IdentityProviderCreated")
}

// TestSAMLBackend_NoMappingRulesStaysPending pins that the shared
// ensureFederation gate reports the pending state for a SAML backend too.
func TestSAMLBackend_NoMappingRulesStaysPending(t *testing.T) {
	g := NewGomegaWithT(t)
	srv := identityfake.NewServer(testAdminPassword)
	t.Cleanup(srv.Close)

	ks := testKeystoneWithReadyAPI()
	backend := testSAMLBackend("corp-saml", "corp")
	backend.Spec.Mappings = nil
	r := newBackendTestReconciler(srv, ks, backend, testAdminSecret())

	_, err := reconcileBackendTwice(t, r, backend)
	g.Expect(err).NotTo(HaveOccurred())

	updated := getBackend(t, r.Client, "corp-saml")
	fedObjects := commonconditions.GetCondition(updated.Status.Conditions, conditionTypeFederationObjectsReady)
	g.Expect(fedObjects).NotTo(BeNil())
	g.Expect(fedObjects.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(fedObjects.Reason).To(Equal(conditionReasonNoMappingRules))
	// No protocol was provisioned while the mapping is empty.
	g.Expect(srv.Protocol("corp-saml", "mapped")).To(BeNil())
}

func TestSAMLBackendDelete_TearsDownInReverseDependencyOrder(t *testing.T) {
	g := NewGomegaWithT(t)
	srv := identityfake.NewServer(testAdminPassword)
	t.Cleanup(srv.Close)
	srv.SeedRole("member")

	ks := testKeystoneWithReadyAPI()
	backend := testSAMLBackend("corp-saml", "corp")
	r := newBackendTestReconciler(srv, ks, backend, testAdminSecret())

	_, err := reconcileBackendTwice(t, r, backend)
	g.Expect(err).NotTo(HaveOccurred())

	updated := getBackend(t, r.Client, "corp-saml")
	g.Expect(r.Delete(context.Background(), updated)).To(Succeed())

	result, err := r.Reconcile(context.Background(), backendRequest(backend))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue())

	g.Expect(srv.IdentityProvider("corp-saml")).To(BeNil())
	g.Expect(srv.Mapping("corp-saml-mapping")).To(BeNil())

	var protoIdx, mappingIdx, idpIdx int
	for i, req := range srv.Requests() {
		switch {
		case strings.HasPrefix(req, "DELETE /v3/OS-FEDERATION/identity_providers/") && strings.Contains(req, "/protocols/"):
			protoIdx = i
		case strings.HasPrefix(req, "DELETE /v3/OS-FEDERATION/mappings/"):
			mappingIdx = i
		case strings.HasPrefix(req, "DELETE /v3/OS-FEDERATION/identity_providers/"):
			idpIdx = i
		}
	}
	g.Expect(protoIdx).To(BeNumerically(">", 0))
	g.Expect(mappingIdx).To(BeNumerically(">", protoIdx), "the mapping must outlive the protocol referencing it")
	g.Expect(idpIdx).To(BeNumerically(">", mappingIdx), "the identity provider must be removed last")

	expectBackendEventContaining(g, r, "Normal FederationObjectsDeleted")
}

// TestSAMLBackend_SubConditionSet pins that a SAML backend's aggregate Ready
// derives from the full federation condition set (not the LDAP two-condition
// set), so the metrics condition_type labels resolve and Ready is not stranded.
func TestSAMLBackend_SubConditionSet(t *testing.T) {
	g := NewGomegaWithT(t)
	backend := testSAMLBackend("corp-saml", "corp")
	g.Expect(identityBackendSubConditionTypesFor(backend)).To(ConsistOf(
		conditionTypeDomainReady,
		conditionTypeFederationObjectsReady,
		conditionTypeMappingsReady,
		conditionTypeConfigProjected,
	))
}

// TestLDAPBackend_ReadyUnaffectedByFederationConditions pins the per-type
// aggregate: an LDAP backend's Ready derives from DomainReady +
// ConfigProjected only — the OIDC-only condition types must not strand it.
func TestLDAPBackend_ReadyUnaffectedByFederationConditions(t *testing.T) {
	g := NewGomegaWithT(t)
	srv := identityfake.NewServer(testAdminPassword)
	t.Cleanup(srv.Close)

	ks := testKeystoneWithReadyAPI()
	backend := testIdentityBackend("corp-ldap", "corp")
	backend.Status = keystonev1alpha1.KeystoneIdentityBackendStatus{}
	domainsSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "test-keystone-domains-abcd1234", Namespace: "default"},
		Data:       map[string][]byte{domainConfFileName("corp"): []byte("[ldap]\n")},
	}
	r := newBackendTestReconciler(srv, ks, backend, testAdminSecret(),
		testProjectedDeployment(ks, domainsSecret.Name, ""), domainsSecret)

	_, err := reconcileBackendTwice(t, r, backend)
	g.Expect(err).NotTo(HaveOccurred())

	updated := getBackend(t, r.Client, "corp-ldap")
	ready := commonconditions.GetCondition(updated.Status.Conditions, "Ready")
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(commonconditions.GetCondition(updated.Status.Conditions, conditionTypeFederationObjectsReady)).To(BeNil())
	g.Expect(commonconditions.GetCondition(updated.Status.Conditions, conditionTypeMappingsReady)).To(BeNil())
}
