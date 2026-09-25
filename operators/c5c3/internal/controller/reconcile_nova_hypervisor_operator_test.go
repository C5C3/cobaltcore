// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the hypervisor operator's account: the registration
// spec.services.nova.hypervisorOperator projects.
package controller

import (
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// TestDesiredNovaHypervisorOperatorRegistration pins the account-only
// registration: a name derived from the Nova child's, the Nova namespace, no
// catalog entry, a project of its own the registration creates, and admin
// alone.
func TestDesiredNovaHypervisorOperatorRegistration(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "controlplane", Namespace: "openstack"},
		Spec: c5c3v1alpha1.ControlPlaneSpec{Services: c5c3v1alpha1.ServicesSpec{
			Nova: &c5c3v1alpha1.ServiceNovaSpec{HypervisorOperator: &c5c3v1alpha1.ServiceNovaHypervisorOperatorSpec{}},
		}},
	}

	ks := desiredNovaHypervisorOperatorRegistration(cp)

	g.Expect(ks.Name).To(Equal("controlplane-nova-hypervisor-operator"))
	g.Expect(ks.Namespace).To(Equal(cp.NovaNamespace()))
	g.Expect(ks.Spec.ControlPlaneRef).To(Equal(c5c3v1alpha1.ControlPlaneRefSpec{Name: "controlplane", Namespace: "openstack"}))
	g.Expect(ks.Spec.Catalog).To(BeNil(), "an account nothing calls advertises no endpoint")
	g.Expect(ks.Spec.Account).NotTo(BeNil())
	g.Expect(ks.Spec.Account.UserName).To(Equal("hypervisor-operator"))
	g.Expect(ks.Spec.Account.Project).To(Equal(c5c3v1alpha1.ServiceAccountProjectSpec{
		Name: "service-hypervisor-operator", Create: true,
	}), "the account gets a project of its own, apart from service-nova")
	g.Expect(ks.Spec.Account.Roles).To(Equal([]string{"admin"}))
	g.Expect(ks.Spec.Account.DomainName).To(BeEmpty(),
		"an unset domain lets the registration resolve the ControlPlane's admin domain")
	g.Expect(ks.Spec.Account.Adopt).To(BeFalse(), "a colliding user must fail loud, never be taken over")

	placed := cp.DeepCopy()
	placed.Spec.Services.Nova.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{Name: "compute"}
	g.Expect(desiredNovaHypervisorOperatorRegistration(placed).Namespace).To(Equal("compute"),
		"the registration follows the Nova into a dedicated namespace")
	g.Expect(desiredNovaHypervisorOperatorRegistration(placed).Spec.ControlPlaneRef.Namespace).To(Equal("openstack"),
		"the registration names the ControlPlane's namespace explicitly, not its own")

	g.Expect(desiredNovaRegistration(cp).Spec.Account.Roles).To(Equal([]string{"service", "admin"}),
		"nova's own account keeps its roles")
}

// TestNovaHypervisorOperatorRegistrationName_FitsTheLabel pins the length
// budget: validateNovaChildName caps a ControlPlane with Nova at 36
// characters, and the registration name then stays within the 63 characters of
// the c5c3.io/keystoneservice-name label it becomes.
func TestNovaHypervisorOperatorRegistrationName_FitsTheLabel(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: strings.Repeat("c", 36), Namespace: "openstack"},
	}

	g.Expect(novaHypervisorOperatorRegistrationName(cp)).To(HaveLen(61))
}
