// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
)

// korcAdminUsername / korcAdminDomainName identify the OpenStack admin user and
// its domain that the Keystone bootstrap creates; the c5c3-operator imports them
// into K-ORC (unmanaged) rather than creating them.
const (
	korcAdminUsername   = "admin"
	korcAdminDomainName = "Default"
)

// adminUserRef returns the Kubernetes metadata.name of the imported K-ORC User
// CR the admin application credential is associated with. AdminCredentialSpec has
// no UserRef field, but K-ORC's ApplicationCredentialResourceSpec.UserRef is
// REQUIRED, so we derive a deterministic name scoped by cp.Name (mirroring
// adminDomainRef) — this way two ControlPlanes in one namespace produce DISTINCT
// User objects rather than colliding on a shared name. The
// inner OpenStack username the import resolves to is still "admin": it is set
// independently via Spec.Import.Filter.Name = OpenStackName(korcAdminUsername) in
// ensureKORCAdminImports. The matching User CR is provisioned there as an
// unmanaged import, so the reference always resolves.
func adminUserRef(cp *c5c3v1alpha1.ControlPlane) string {
	return fmt.Sprintf("%s-user-admin", cp.Name)
}

// adminDomainRef is the deterministic name of the K-ORC Domain CR the admin User
// import is scoped to.
func adminDomainRef(cp *c5c3v1alpha1.ControlPlane) string {
	return cp.Name + "-domain-default"
}

// ensureKORCAdminImports ensures the K-ORC Domain and User that the admin
// ApplicationCredential's UserRef depends on exist as UNMANAGED imports. The
// Keystone bootstrap creates the real admin user (in the Default domain); K-ORC
// must import — not create — it, otherwise the ApplicationCredential blocks on
// "Waiting for User/admin to be created". Both CRs are owned by the ControlPlane
// for GC and reuse the admin clouds.yaml credentials.
func (r *ControlPlaneReconciler) ensureKORCAdminImports(ctx context.Context, cp *c5c3v1alpha1.ControlPlane, credRef orcv1alpha1.CloudCredentialsReference) error {
	domain := &orcv1alpha1.Domain{
		ObjectMeta: metav1.ObjectMeta{Name: adminDomainRef(cp), Namespace: childNamespace(cp)},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, domain, func() error {
		domain.Spec.ManagementPolicy = orcv1alpha1.ManagementPolicyUnmanaged
		domain.Spec.CloudCredentialsRef = credRef
		domain.Spec.Import = &orcv1alpha1.DomainImport{
			Filter: &orcv1alpha1.DomainFilter{Name: ptr.To(orcv1alpha1.KeystoneName(korcAdminDomainName))},
		}
		return controllerutil.SetControllerReference(cp, domain, r.Scheme)
	}); err != nil {
		return fmt.Errorf("admin Domain import %q: %w", domain.Name, err)
	}

	user := &orcv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: adminUserRef(cp), Namespace: childNamespace(cp)},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, user, func() error {
		user.Spec.ManagementPolicy = orcv1alpha1.ManagementPolicyUnmanaged
		user.Spec.CloudCredentialsRef = credRef
		user.Spec.Import = &orcv1alpha1.UserImport{
			Filter: &orcv1alpha1.UserFilter{
				Name:      ptr.To(orcv1alpha1.OpenStackName(korcAdminUsername)),
				DomainRef: ptr.To(orcv1alpha1.KubernetesNameRef(adminDomainRef(cp))),
			},
		}
		return controllerutil.SetControllerReference(cp, user, r.Scheme)
	}); err != nil {
		return fmt.Errorf("admin User import %q: %w", user.Name, err)
	}
	return nil
}
