// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package messaging

import (
	"testing"

	. "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestBuildVhost(t *testing.T) {
	g := NewWithT(t)

	for _, policy := range []string{"retain", "delete"} {
		u := BuildVhost("app-1a2b3c4d-vhost-vhost", "openstack", "app-1a2b3c4d", "cp-rabbitmq", policy)
		g.Expect(u.GroupVersionKind()).To(Equal(VhostGVK))
		g.Expect(u.GetAPIVersion()).To(Equal("rabbitmq.com/v1beta1"))
		g.Expect(u.GetName()).To(Equal("app-1a2b3c4d-vhost-vhost"))
		g.Expect(u.GetNamespace()).To(Equal("openstack"))
		g.Expect(u.Object["spec"]).To(Equal(map[string]any{
			"name":                     "app-1a2b3c4d",
			"deletionPolicy":           policy,
			"rabbitmqClusterReference": map[string]any{"name": "cp-rabbitmq"},
		}))
	}
}

func TestBuildUser(t *testing.T) {
	g := NewWithT(t)

	u := BuildUser("app-1a2b3c4d-vhost-user-v1", "openstack", "cp-rabbitmq", "app-1a2b3c4d-vhost-password-v1")
	g.Expect(u.GroupVersionKind()).To(Equal(UserGVK))
	g.Expect(u.GetName()).To(Equal("app-1a2b3c4d-vhost-user-v1"))
	g.Expect(u.GetNamespace()).To(Equal("openstack"))
	g.Expect(u.Object["spec"]).To(Equal(map[string]any{
		"importCredentialsSecret":  map[string]any{"name": "app-1a2b3c4d-vhost-password-v1"},
		"rabbitmqClusterReference": map[string]any{"name": "cp-rabbitmq"},
	}), "the user carries no tags, so it reaches no management API")
}

func TestBuildPermission(t *testing.T) {
	g := NewWithT(t)

	u := BuildPermission("app-1a2b3c4d-vhost-permission-v1", "openstack", "cp-rabbitmq", "app-1a2b3c4d", "app-1a2b3c4d-v1")
	g.Expect(u.GroupVersionKind()).To(Equal(PermissionGVK))
	g.Expect(u.GetName()).To(Equal("app-1a2b3c4d-vhost-permission-v1"))
	g.Expect(u.GetNamespace()).To(Equal("openstack"))
	g.Expect(u.Object["spec"]).To(Equal(map[string]any{
		"vhost":                    "app-1a2b3c4d",
		"user":                     "app-1a2b3c4d-v1",
		"permissions":              map[string]any{"configure": ".*", "write": ".*", "read": ".*"},
		"rabbitmqClusterReference": map[string]any{"name": "cp-rabbitmq"},
	}))
}

// topologyStatus returns a Vhost at generation 2 whose status carries
// observedGeneration observed (unset when negative) and conds.
func topologyStatus(observed int64, conds ...map[string]any) *unstructured.Unstructured {
	u := BuildVhost("v", "ns", "v", "c", "retain")
	u.SetGeneration(2)
	status := map[string]any{}
	if observed >= 0 {
		status["observedGeneration"] = observed
	}
	if len(conds) > 0 {
		list := make([]any, 0, len(conds))
		for _, c := range conds {
			list = append(list, c)
		}
		status["conditions"] = list
	}
	u.Object["status"] = status
	return u
}

func readyCondition(status, reason, message string) map[string]any {
	return map[string]any{"type": "Ready", "status": status, "reason": reason, "message": message}
}

func TestTopologyReady(t *testing.T) {
	cases := []struct {
		name string
		obj  *unstructured.Unstructured
		want bool
	}{
		{name: "no status", obj: BuildVhost("v", "ns", "v", "c", "retain")},
		{name: "a status without observedGeneration", obj: topologyStatus(-1, readyCondition("True", "SuccessfulCreateOrUpdate", ""))},
		{name: "a stale observedGeneration", obj: topologyStatus(1, readyCondition("True", "SuccessfulCreateOrUpdate", ""))},
		{name: "no Ready condition", obj: topologyStatus(2, map[string]any{"type": "PasswordSynced", "status": "True"})},
		{name: "Ready=False", obj: topologyStatus(2, readyCondition("False", "FailedCreateOrUpdate", "boom"))},
		{name: "Ready=Unknown", obj: topologyStatus(2, readyCondition("Unknown", "", ""))},
		{
			name: "Ready=True at the current generation",
			obj:  topologyStatus(2, map[string]any{"type": "PasswordSynced", "status": "True"}, readyCondition("True", "SuccessfulCreateOrUpdate", "")),
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			NewWithT(t).Expect(TopologyReady(tc.obj)).To(Equal(tc.want))
		})
	}
}

func TestTopologyFailure(t *testing.T) {
	cases := []struct {
		name string
		obj  *unstructured.Unstructured
		want string
	}{
		{name: "no status", obj: BuildUser("u", "ns", "c", "s")},
		{name: "a ready object", obj: topologyStatus(2, readyCondition("True", "SuccessfulCreateOrUpdate", ""))},
		{name: "a not-ready object for another reason", obj: topologyStatus(2, readyCondition("False", "Pending", "waiting"))},
		{
			name: "a failed create or update", obj: topologyStatus(2, readyCondition("False", "FailedCreateOrUpdate", "user does not exist")),
			want: "user does not exist",
		},
		{
			name: "a failure without a message reads as its reason", obj: topologyStatus(2, readyCondition("False", "FailedCreateOrUpdate", "")),
			want: "FailedCreateOrUpdate",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			NewWithT(t).Expect(TopologyFailure(tc.obj)).To(Equal(tc.want))
		})
	}
}
