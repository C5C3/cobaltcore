// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	commonconditions "github.com/c5c3/forge/internal/common/conditions"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
	identityfake "github.com/c5c3/forge/operators/keystone/internal/identity/fake"
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
		Issuer:               "https://idp.example.com/realms/forge",
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
			{Type: "HTTP_OIDC_ISS", AnyOneOf: []string{"https://idp.example.com/realms/forge"}},
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
	g.Expect(idp.RemoteIDs).To(ConsistOf("https://idp.example.com/realms/forge"))
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

func TestOIDCBackend_SteadyStateIssuesNoMutatingRequests(t *testing.T) {
	g := NewGomegaWithT(t)
	srv := identityfake.NewServer(testAdminPassword)
	t.Cleanup(srv.Close)
	srv.SeedRole("member")

	ks := testKeystoneWithReadyAPI()
	backend := testOIDCBackend("corp-oidc", "corp")
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
		testProjectedDeployment(ks, domainsSecret.Name), domainsSecret)

	_, err := reconcileBackendTwice(t, r, backend)
	g.Expect(err).NotTo(HaveOccurred())

	updated := getBackend(t, r.Client, "corp-ldap")
	ready := commonconditions.GetCondition(updated.Status.Conditions, "Ready")
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(commonconditions.GetCondition(updated.Status.Conditions, conditionTypeFederationObjectsReady)).To(BeNil())
	g.Expect(commonconditions.GetCondition(updated.Status.Conditions, conditionTypeMappingsReady)).To(BeNil())
}
