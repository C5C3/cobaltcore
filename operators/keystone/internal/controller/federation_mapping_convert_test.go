// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"encoding/json"
	"testing"

	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/federation"
	. "github.com/onsi/gomega"
	"k8s.io/utils/ptr"

	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

func TestToGophercloudMappingRules_EmptyAndNil(t *testing.T) {
	g := NewGomegaWithT(t)
	g.Expect(toGophercloudMappingRules(nil)).To(BeNil())
	g.Expect(toGophercloudMappingRules([]keystonev1alpha1.MappingRuleSpec{})).To(BeNil())
}

// TestToGophercloudMappingRules_FullShape converts a rule exercising every
// typed field and asserts the wire JSON matches keystone's mapping-rule
// grammar (snake_case keys, omitted zero values) — the contract the CRD types
// document as a one-to-one mirror.
func TestToGophercloudMappingRules_FullShape(t *testing.T) {
	g := NewGomegaWithT(t)

	rules := []keystonev1alpha1.MappingRuleSpec{{
		Local: []keystonev1alpha1.MappingLocalRuleSpec{
			{
				User: &keystonev1alpha1.MappingUserSpec{
					Name:   "{0}",
					Email:  "{1}",
					Domain: &keystonev1alpha1.MappingDomainSpec{Name: "corp"},
					Type:   "ephemeral",
				},
				Group: &keystonev1alpha1.MappingGroupSpec{
					Name:   "federated-users",
					Domain: &keystonev1alpha1.MappingDomainSpec{Name: "corp"},
				},
			},
			{
				Projects: []keystonev1alpha1.MappingProjectSpec{{
					Name:  "demo",
					Roles: []keystonev1alpha1.MappingProjectRoleSpec{{Name: "member"}},
				}},
				Groups: "{2}",
			},
		},
		Remote: []keystonev1alpha1.MappingRemoteRuleSpec{
			{Type: "HTTP_OIDC_PREFERRED_USERNAME"},
			{Type: "HTTP_OIDC_EMAIL"},
			{
				Type:     "HTTP_OIDC_ISS",
				Regex:    ptr.To(false),
				AnyOneOf: []string{"https://idp.example.com/realms/forge"},
			},
		},
	}}

	out := toGophercloudMappingRules(rules)
	g.Expect(out).To(HaveLen(1))

	payload, err := json.Marshal(federation.CreateMappingOpts{Rules: out})
	g.Expect(err).NotTo(HaveOccurred())

	var decoded map[string]any
	g.Expect(json.Unmarshal(payload, &decoded)).To(Succeed())
	rulesJSON := decoded["rules"].([]any)
	rule := rulesJSON[0].(map[string]any)

	local := rule["local"].([]any)
	g.Expect(local).To(HaveLen(2))
	first := local[0].(map[string]any)
	user := first["user"].(map[string]any)
	g.Expect(user["name"]).To(Equal("{0}"))
	g.Expect(user["type"]).To(Equal("ephemeral"))
	g.Expect(user["domain"].(map[string]any)["name"]).To(Equal("corp"))
	g.Expect(first["group"].(map[string]any)["name"]).To(Equal("federated-users"))
	// Zero values are omitted from the wire shape.
	g.Expect(first).NotTo(HaveKey("groups"))
	g.Expect(first).NotTo(HaveKey("group_ids"))
	second := local[1].(map[string]any)
	g.Expect(second["groups"]).To(Equal("{2}"))
	projects := second["projects"].([]any)
	g.Expect(projects[0].(map[string]any)["name"]).To(Equal("demo"))

	remote := rule["remote"].([]any)
	g.Expect(remote).To(HaveLen(3))
	iss := remote[2].(map[string]any)
	g.Expect(iss["type"]).To(Equal("HTTP_OIDC_ISS"))
	g.Expect(iss["any_one_of"]).To(ConsistOf("https://idp.example.com/realms/forge"))
	g.Expect(iss["regex"]).To(Equal(false))
}
