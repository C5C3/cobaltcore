// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/federation"

	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// toGophercloudMappingRules converts the typed spec.mappings rules into
// gophercloud's federation.MappingRule wire shape. The CRD types mirror
// keystone's mapping-rule JSON one-to-one (camelCase field names map to the
// snake_case keys), so the conversion is a pure field-by-field copy — the
// rule grammar is closed and nothing is derived or defaulted here.
func toGophercloudMappingRules(rules []keystonev1alpha1.MappingRuleSpec) []federation.MappingRule {
	if len(rules) == 0 {
		return nil
	}
	out := make([]federation.MappingRule, 0, len(rules))
	for i := range rules {
		out = append(out, federation.MappingRule{
			Local:  toGophercloudLocalRules(rules[i].Local),
			Remote: toGophercloudRemoteRules(rules[i].Remote),
		})
	}
	return out
}

func toGophercloudRemoteRules(remote []keystonev1alpha1.MappingRemoteRuleSpec) []federation.RuleRemote {
	out := make([]federation.RuleRemote, 0, len(remote))
	for i := range remote {
		r := &remote[i]
		out = append(out, federation.RuleRemote{
			Type:      r.Type,
			Regex:     r.Regex,
			AnyOneOf:  r.AnyOneOf,
			NotAnyOf:  r.NotAnyOf,
			Blacklist: r.Blacklist,
			Whitelist: r.Whitelist,
		})
	}
	return out
}

func toGophercloudLocalRules(local []keystonev1alpha1.MappingLocalRuleSpec) []federation.RuleLocal {
	out := make([]federation.RuleLocal, 0, len(local))
	for i := range local {
		l := &local[i]
		rule := federation.RuleLocal{
			Domain:   toGophercloudDomain(l.Domain),
			GroupIDs: l.GroupIDs,
			Groups:   l.Groups,
		}
		if l.Group != nil {
			rule.Group = &federation.Group{
				ID:     l.Group.ID,
				Name:   l.Group.Name,
				Domain: toGophercloudDomain(l.Group.Domain),
			}
		}
		if len(l.Projects) > 0 {
			projects := make([]federation.RuleProject, 0, len(l.Projects))
			for j := range l.Projects {
				p := &l.Projects[j]
				roles := make([]federation.RuleProjectRole, 0, len(p.Roles))
				for k := range p.Roles {
					roles = append(roles, federation.RuleProjectRole{Name: p.Roles[k].Name})
				}
				projects = append(projects, federation.RuleProject{Name: p.Name, Roles: roles})
			}
			rule.Projects = projects
		}
		if l.User != nil {
			user := &federation.RuleUser{
				ID:     l.User.ID,
				Name:   l.User.Name,
				Email:  l.User.Email,
				Domain: toGophercloudDomain(l.User.Domain),
			}
			if l.User.Type != "" {
				userType := federation.UserType(l.User.Type)
				user.Type = &userType
			}
			rule.User = user
		}
		out = append(out, rule)
	}
	return out
}

func toGophercloudDomain(d *keystonev1alpha1.MappingDomainSpec) *federation.Domain {
	if d == nil {
		return nil
	}
	return &federation.Domain{ID: d.ID, Name: d.Name}
}
