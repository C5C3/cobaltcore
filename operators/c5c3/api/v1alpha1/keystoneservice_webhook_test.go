// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"context"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// validKeystoneService returns a minimal valid KeystoneService the per-rule
// tests mutate one field of, so every rejection is attributable to exactly one
// rule. metadata.name, the catalog service name and the user name are three
// DISTINCT values on purpose: every fallback in this webhook resolves to
// metadata.name, so a fixture that shared them would let a broken fallback pass.
func validKeystoneService() *KeystoneService {
	return &KeystoneService{
		ObjectMeta: metav1.ObjectMeta{Name: "glance-registration", Namespace: "openstack"},
		Spec: KeystoneServiceSpec{
			ControlPlaneRef: ControlPlaneRefSpec{Name: "controlplane"},
			Catalog: &KeystoneServiceCatalogSpec{
				ServiceType: "image",
				ServiceName: "glance",
				Endpoints: []KeystoneServiceEndpointSpec{
					{Interface: ExternalEndpointTypePublic, URL: "https://image.example.com"},
				},
			},
			Account: &KeystoneServiceAccountSpec{
				UserName: "glance-user",
				Project:  ServiceAccountProjectSpec{Name: "service"},
				Roles:    []string{"admin", "service"},
			},
		},
	}
}

// --- defaulting ---

func TestKeystoneServiceDefault_MaterializesUserName(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneServiceWebhook{}

	ks := validKeystoneService()
	ks.Spec.Account.UserName = ""

	g.Expect(w.Default(context.Background(), ks)).To(Succeed())
	g.Expect(ks.Spec.Account.UserName).To(Equal("glance-registration"),
		"an empty userName must materialize to metadata.name")
}

func TestKeystoneServiceDefault_PreservesExplicitUserName(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneServiceWebhook{}

	ks := validKeystoneService()

	g.Expect(w.Default(context.Background(), ks)).To(Succeed())
	g.Expect(ks.Spec.Account.UserName).To(Equal("glance-user"))
}

// A catalog-only CR has no account block to default. The webhook must leave the
// nil pointer alone rather than materializing an account nobody declared.
func TestKeystoneServiceDefault_LeavesCatalogOnlyCRUntouched(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneServiceWebhook{}

	ks := validKeystoneService()
	ks.Spec.Account = nil
	before := ks.DeepCopy()

	g.Expect(w.Default(context.Background(), ks)).To(Succeed())
	g.Expect(ks.Spec.Account).To(BeNil())
	g.Expect(ks.Spec).To(Equal(before.Spec))
}

func TestKeystoneServiceDefault_IsIdempotent(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneServiceWebhook{}

	ks := validKeystoneService()
	ks.Spec.Account.UserName = ""

	g.Expect(w.Default(context.Background(), ks)).To(Succeed())
	once := ks.DeepCopy()
	g.Expect(w.Default(context.Background(), ks)).To(Succeed())

	g.Expect(ks.Spec).To(Equal(once.Spec))
}

// --- create validation ---

func TestKeystoneServiceValidateCreate_AcceptsValidCRs(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*KeystoneService)
	}{
		{"both blocks", func(*KeystoneService) {}},
		{"catalog only", func(ks *KeystoneService) { ks.Spec.Account = nil }},
		{"catalog only without endpoints", func(ks *KeystoneService) {
			ks.Spec.Account = nil
			ks.Spec.Catalog.Endpoints = nil
			ks.Spec.Catalog.ServiceName = ""
		}},
		{"account only with just a project", func(ks *KeystoneService) {
			ks.Spec.Catalog = nil
			ks.Spec.Account = &KeystoneServiceAccountSpec{Project: ServiceAccountProjectSpec{Name: "service"}}
		}},
		{"one endpoint row per interface", func(ks *KeystoneService) {
			ks.Spec.Catalog.Endpoints = []KeystoneServiceEndpointSpec{
				{Interface: ExternalEndpointTypePublic, URL: "https://image.example.com/public"},
				{Interface: ExternalEndpointTypeInternal, URL: "https://image.example.com/internal"},
				{Interface: ExternalEndpointTypeAdmin, URL: "http://image.internal/admin"},
			}
		}},
		{"an explicit own-namespace controlPlaneRef", func(ks *KeystoneService) {
			ks.Spec.ControlPlaneRef.Namespace = "openstack"
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			ks := validKeystoneService()
			tc.mutate(ks)

			_, err := (&KeystoneServiceWebhook{}).ValidateCreate(context.Background(), ks)
			g.Expect(err).NotTo(HaveOccurred())
		})
	}
}

func TestKeystoneServiceValidateCreate_RejectsInvalidSpecs(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*KeystoneService)
		wantSub string
	}{
		{"neither block", func(ks *KeystoneService) {
			ks.Spec.Catalog, ks.Spec.Account = nil, nil
		}, "at least one of spec.catalog or spec.account must be set"},

		{"empty controlPlaneRef name", func(ks *KeystoneService) {
			ks.Spec.ControlPlaneRef.Name = ""
		}, "spec.controlPlaneRef.name"},
		{"controlPlaneRef namespace is not a label", func(ks *KeystoneService) {
			ks.Spec.ControlPlaneRef.Namespace = "Not-A-Label"
		}, "must be a lowercase alphanumeric RFC-1123 label"},

		{"empty serviceType", func(ks *KeystoneService) {
			ks.Spec.Catalog.ServiceType = ""
		}, "spec.catalog.serviceType"},
		{"serviceType is not a label", func(ks *KeystoneService) {
			ks.Spec.Catalog.ServiceType = "Not_A_Label"
		}, "must be a lowercase alphanumeric DNS-1123 label"},
		{"identity serviceType", func(ks *KeystoneService) {
			ks.Spec.Catalog.ServiceType = "identity"
		}, "ControlPlane-owned"},
		{"comma in serviceName", func(ks *KeystoneService) {
			ks.Spec.Catalog.ServiceName = "glance,evil"
		}, "must not contain a comma"},

		{"endpoint url without a scheme", func(ks *KeystoneService) {
			ks.Spec.Catalog.Endpoints[0].URL = "keystone.example"
		}, "must be an http(s) URL"},
		{"endpoint url without a host", func(ks *KeystoneService) {
			ks.Spec.Catalog.Endpoints[0].URL = "https://"
		}, "must include a host"},
		{"endpoint url over the cap", func(ks *KeystoneService) {
			ks.Spec.Catalog.Endpoints[0].URL = "https://image.example.com/" + strings.Repeat("a", maxCatalogEndpointURLBytes)
		}, "must be at most 1024 bytes"},
		{"unsupported endpoint interface", func(ks *KeystoneService) {
			ks.Spec.Catalog.Endpoints[0].Interface = "internal2"
		}, "Unsupported value"},
		{"empty endpoint interface", func(ks *KeystoneService) {
			ks.Spec.Catalog.Endpoints[0].Interface = ""
		}, "spec.catalog.endpoints[0].interface"},
		{"duplicate endpoint interface", func(ks *KeystoneService) {
			ks.Spec.Catalog.Endpoints = []KeystoneServiceEndpointSpec{
				{Interface: ExternalEndpointTypePublic, URL: "https://image.example.com/a"},
				{Interface: ExternalEndpointTypePublic, URL: "https://image.example.com/b"},
			}
		}, "Duplicate value"},

		{"comma in userName", func(ks *KeystoneService) {
			ks.Spec.Account.UserName = "glance,evil"
		}, "spec.account.userName"},
		{"comma in domainName", func(ks *KeystoneService) {
			ks.Spec.Account.DomainName = "Default,evil"
		}, "spec.account.domainName"},
		{"empty project name", func(ks *KeystoneService) {
			ks.Spec.Account.Project.Name = ""
		}, "spec.account.project.name"},
		{"comma in project name", func(ks *KeystoneService) {
			ks.Spec.Account.Project.Name = "service,evil"
		}, "KeystoneName pattern"},
		{"comma in a role", func(ks *KeystoneService) {
			ks.Spec.Account.Roles = []string{"admin", "service,evil"}
		}, "spec.account.roles[1]"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			ks := validKeystoneService()
			tc.mutate(ks)

			_, err := (&KeystoneServiceWebhook{}).ValidateCreate(context.Background(), ks)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring(tc.wantSub))
		})
	}
}

// The child-name bound the CRD schema cannot express. The boundaries are exact:
// a role-less CR spends keystoneServiceChildNameOverhead (43) bytes on its
// longest child, one with roles spends keystoneServiceRoleChildNameOverhead
// (55), and the apiserver caps the composed name at maxObjectNameBytes (253).
func TestKeystoneServiceValidateCreate_BoundsChildNameLength(t *testing.T) {
	cases := []struct {
		name        string
		nameLen     int
		roles       []string
		catalogOnly bool
		wantErr     bool
	}{
		{"role-less at the limit", maxObjectNameBytes - keystoneServiceChildNameOverhead, nil, false, false},
		{"role-less one byte over", maxObjectNameBytes - keystoneServiceChildNameOverhead + 1, nil, false, true},
		{"with roles at the limit", maxObjectNameBytes - keystoneServiceRoleChildNameOverhead, []string{"admin"}, false, false},
		{"with roles one byte over", maxObjectNameBytes - keystoneServiceRoleChildNameOverhead + 1, []string{"admin"}, false, true},
		// A catalog-only CR is charged the role-less base too: an account block
		// can be added later and metadata.name can never be shortened.
		{"catalog-only at the limit", maxObjectNameBytes - keystoneServiceChildNameOverhead, nil, true, false},
		{"catalog-only one byte over", maxObjectNameBytes - keystoneServiceChildNameOverhead + 1, nil, true, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			ks := validKeystoneService()
			ks.Name = strings.Repeat("a", tc.nameLen)
			if tc.catalogOnly {
				ks.Spec.Account = nil
			} else {
				ks.Spec.Account.Roles = tc.roles
			}

			_, err := (&KeystoneServiceWebhook{}).ValidateCreate(context.Background(), ks)
			if !tc.wantErr {
				g.Expect(err).NotTo(HaveOccurred())
				return
			}
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("object-name limit"))
			g.Expect(err.Error()).To(ContainSubstring("254 bytes"))
		})
	}
}

// --- update validation ---

func TestKeystoneServiceValidateUpdate_FreezesIdentity(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*KeystoneService)
		wantSub string
	}{
		{"controlPlaneRef name", func(ks *KeystoneService) {
			ks.Spec.ControlPlaneRef.Name = "other-plane"
		}, "controlPlaneRef.name is immutable"},
		{"controlPlaneRef namespace", func(ks *KeystoneService) {
			ks.Spec.ControlPlaneRef.Namespace = "other-namespace"
		}, "controlPlaneRef.namespace is immutable"},
		{"serviceType", func(ks *KeystoneService) {
			ks.Spec.Catalog.ServiceType = "volume"
		}, "serviceType is immutable"},
		{"serviceName", func(ks *KeystoneService) {
			ks.Spec.Catalog.ServiceName = "glance-renamed"
		}, "serviceName is immutable"},
		// Dropping an explicit serviceName falls back to metadata.name, which is
		// a rename of the registered catalog row, not a no-op.
		{"serviceName cleared to the fallback", func(ks *KeystoneService) {
			ks.Spec.Catalog.ServiceName = ""
		}, "serviceName is immutable"},
		{"userName", func(ks *KeystoneService) {
			ks.Spec.Account.UserName = "glance-renamed"
		}, "userName is immutable"},
		{"domainName", func(ks *KeystoneService) {
			ks.Spec.Account.DomainName = "corp"
		}, "domainName is immutable"},
		{"project name", func(ks *KeystoneService) {
			ks.Spec.Account.Project.Name = "other-project"
		}, "project.name is immutable"},
		{"project create", func(ks *KeystoneService) {
			ks.Spec.Account.Project.Create = true
		}, "project.create is immutable"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			oldKS := validKeystoneService()
			newKS := validKeystoneService()
			tc.mutate(newKS)

			_, err := (&KeystoneServiceWebhook{}).ValidateUpdate(context.Background(), oldKS, newKS)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring(tc.wantSub))
		})
	}
}

func TestKeystoneServiceValidateUpdate_AdmitsMutableChanges(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*KeystoneService)
	}{
		{"catalog adopt", func(ks *KeystoneService) { ks.Spec.Catalog.Adopt = true }},
		{"account adopt", func(ks *KeystoneService) { ks.Spec.Account.Adopt = true }},
		{"endpoint url", func(ks *KeystoneService) {
			ks.Spec.Catalog.Endpoints[0].URL = "https://image.example.com/v2"
		}},
		{"an added endpoint row", func(ks *KeystoneService) {
			ks.Spec.Catalog.Endpoints = append(ks.Spec.Catalog.Endpoints,
				KeystoneServiceEndpointSpec{Interface: ExternalEndpointTypeAdmin, URL: "https://image.internal"})
		}},
		{"a removed endpoint row", func(ks *KeystoneService) { ks.Spec.Catalog.Endpoints = nil }},
		{"an added role", func(ks *KeystoneService) {
			ks.Spec.Account.Roles = append(ks.Spec.Account.Roles, "reader")
		}},
		{"a removed role", func(ks *KeystoneService) { ks.Spec.Account.Roles = nil }},
		{"a rotation policy", func(ks *KeystoneService) {
			ks.Spec.Account.Rotation = &ServiceAccountRotationSpec{Mode: ServiceAccountRotationModeScheduled}
		}},
		// A block added or removed as a whole is a create or a teardown the
		// reconciler handles, so no freeze compares its fields.
		{"a removed catalog block", func(ks *KeystoneService) { ks.Spec.Catalog = nil }},
		{"a removed account block", func(ks *KeystoneService) { ks.Spec.Account = nil }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			oldKS := validKeystoneService()
			newKS := validKeystoneService()
			tc.mutate(newKS)

			_, err := (&KeystoneServiceWebhook{}).ValidateUpdate(context.Background(), oldKS, newKS)
			g.Expect(err).NotTo(HaveOccurred())
		})
	}
}

// A block the old object did not declare carries no frozen identity, so adding
// one with values that differ from the CR's own name is admitted.
func TestKeystoneServiceValidateUpdate_AdmitsAddedBlocks(t *testing.T) {
	g := NewGomegaWithT(t)

	oldKS := validKeystoneService()
	oldKS.Spec.Account = nil
	newKS := validKeystoneService()

	_, err := (&KeystoneServiceWebhook{}).ValidateUpdate(context.Background(), oldKS, newKS)
	g.Expect(err).NotTo(HaveOccurred())

	oldKS = validKeystoneService()
	oldKS.Spec.Catalog = nil
	newKS = validKeystoneService()

	_, err = (&KeystoneServiceWebhook{}).ValidateUpdate(context.Background(), oldKS, newKS)
	g.Expect(err).NotTo(HaveOccurred())
}

// A CR stored before the defaulting webhook existed carries an empty userName.
// Materializing the documented fallback onto it changes no identity and must be
// admitted; materializing anything else is a rename.
func TestKeystoneServiceValidateUpdate_UserNameMaterialization(t *testing.T) {
	g := NewGomegaWithT(t)

	oldKS := validKeystoneService()
	oldKS.Spec.Account.UserName = ""

	admitted := validKeystoneService()
	admitted.Spec.Account.UserName = admitted.Name

	_, err := (&KeystoneServiceWebhook{}).ValidateUpdate(context.Background(), oldKS, admitted)
	g.Expect(err).NotTo(HaveOccurred(), "materializing metadata.name onto an empty userName is not a rename")

	rejected := validKeystoneService()
	rejected.Spec.Account.UserName = "someone-else"

	_, err = (&KeystoneServiceWebhook{}).ValidateUpdate(context.Background(), oldKS, rejected)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("userName is immutable"))
}

// An empty controlPlaneRef.namespace means the CR's own, so the transition to
// and from an explicit value naming that same namespace resolves to the same
// plane and must be admitted in both directions.
func TestKeystoneServiceValidateUpdate_AdmitsOwnNamespaceMaterialization(t *testing.T) {
	g := NewGomegaWithT(t)

	implicit := validKeystoneService()
	explicit := validKeystoneService()
	explicit.Spec.ControlPlaneRef.Namespace = explicit.Namespace

	_, err := (&KeystoneServiceWebhook{}).ValidateUpdate(context.Background(), implicit, explicit)
	g.Expect(err).NotTo(HaveOccurred())

	_, err = (&KeystoneServiceWebhook{}).ValidateUpdate(context.Background(), explicit, implicit)
	g.Expect(err).NotTo(HaveOccurred())
}

// ValidateUpdate re-runs the value rules, so an edit cannot introduce a value
// ValidateCreate would have rejected.
func TestKeystoneServiceValidateUpdate_ReRunsValueRules(t *testing.T) {
	g := NewGomegaWithT(t)

	oldKS := validKeystoneService()
	oldKS.Spec.Catalog.ServiceName = ""
	newKS := validKeystoneService()
	newKS.Spec.Catalog.ServiceName = ""
	newKS.Spec.Catalog.Endpoints[0].URL = "keystone.example"

	_, err := (&KeystoneServiceWebhook{}).ValidateUpdate(context.Background(), oldKS, newKS)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("must be an http(s) URL"))

	// A comma introduced into serviceName trips the value rule as well as the
	// freeze. Both errors aggregate into one response, so asserting the comma
	// message proves the value rules ran and did not stop at the freeze.
	renamed := validKeystoneService()
	renamed.Spec.Catalog.ServiceName = "glance,evil"

	_, err = (&KeystoneServiceWebhook{}).ValidateUpdate(context.Background(), validKeystoneService(), renamed)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("must not contain a comma"))
	g.Expect(err.Error()).To(ContainSubstring("serviceName is immutable"))
}

// ValidateDelete is inherited from the shared NoopDeleteValidator and never
// registered, so it must never reject.
func TestKeystoneServiceValidateDelete_IsNoop(t *testing.T) {
	g := NewGomegaWithT(t)

	warnings, err := (&KeystoneServiceWebhook{}).ValidateDelete(context.Background(), validKeystoneService())
	g.Expect(warnings).To(BeNil())
	g.Expect(err).NotTo(HaveOccurred())
}
