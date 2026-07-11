// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"cmp"
	"context"
	"fmt"
	"time"

	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/c5c3/forge/internal/common/apply"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
)

// adminUserName / adminProjectName / adminDomainName resolve the three OpenStack
// admin identities the control plane authenticates as, from
// spec.korc.adminCredential. Each falls back to the constant the defaulting
// webhook would have materialized, so a webhook-bypassed CR still resolves to the
// identities a stock Keystone bootstrap creates ("admin"/"admin"/"Default")
// rather than to an empty filter that would import the wrong resource.
//
// They are consumed in exactly two places, and that pairing is a HARD Keystone
// constraint rather than a convenience: buildPasswordCloudsYAML renders
// adminUserName as the clouds.yaml `username`, and ensureKORCAdminImports uses the
// same value as the admin User import filter the ApplicationCredential's UserRef
// resolves to. Keystone's default policy allows creating an application credential
// only for the TOKEN'S OWN user — even an admin token is refused (403,
// identity:create_application_credential) for another user — so the authenticating
// identity and the AC's owner must be one and the same user.
// TestPasswordCloudsYAMLIdentityMatchesUserImportFilter locks that in.
func adminUserName(cp *c5c3v1alpha1.ControlPlane) string {
	return cmp.Or(cp.Spec.KORC.AdminCredential.UserName, c5c3v1alpha1.DefaultAdminUserName)
}

// adminProjectName resolves the OpenStack admin project name (clouds.yaml
// `project_name`).
func adminProjectName(cp *c5c3v1alpha1.ControlPlane) string {
	return cmp.Or(cp.Spec.KORC.AdminCredential.ProjectName, c5c3v1alpha1.DefaultAdminProjectName)
}

// adminDomainName resolves the OpenStack admin domain name. Phase-1 nuance: the
// single value feeds BOTH user_domain_name and project_domain_name in the
// generated clouds.yaml as well as the admin Domain import filter, so the admin
// user and project must live in the same domain.
func adminDomainName(cp *c5c3v1alpha1.ControlPlane) string {
	return cmp.Or(cp.Spec.KORC.AdminCredential.DomainName, c5c3v1alpha1.DefaultAdminDomainName)
}

// adminUserRef returns the Kubernetes metadata.name of the imported K-ORC User
// CR the admin application credential is associated with. AdminCredentialSpec has
// no UserRef field, but K-ORC's ApplicationCredentialResourceSpec.UserRef is
// REQUIRED, so we derive a deterministic name scoped by cp.Name (mirroring
// adminDomainRef) — this way two ControlPlanes in one namespace produce DISTINCT
// User objects rather than colliding on a shared name.
//
// The Kubernetes CR name is a stable handle and is deliberately NOT derived from
// the configurable identity: the inner OpenStack username the import resolves to
// is set independently via Spec.Import.Filter.Name = OpenStackName(adminUserName)
// in ensureKORCAdminImports. Editing spec.korc.adminCredential.userName therefore
// updates the filter in place; K-ORC imports resolve once, so the already-resolved
// id is not re-resolved and the mismatch surfaces as KORCReady=False/
// CredentialDrift rather than silently repointing the credential.
func adminUserRef(cp *c5c3v1alpha1.ControlPlane) string {
	return fmt.Sprintf("%s-user-admin", cp.Name)
}

// adminDomainRef is the deterministic name of the K-ORC Domain CR the admin User
// import is scoped to.
func adminDomainRef(cp *c5c3v1alpha1.ControlPlane) string {
	return cp.Name + "-domain-default"
}

// korcAdminImports carries the two admin-identity imports ensureKORCAdminImports
// reconciled, with their live status. reconcileKORC needs the OBJECTS, not just a
// rendered message: in External mode it classifies K-ORC's condition messages to
// distinguish the failure classes K-ORC itself collapses into TransientError.
type korcAdminImports struct {
	domain *orcv1alpha1.Domain
	user   *orcv1alpha1.User
}

// objects returns the imports in dependency order — Domain before User — so a
// classifier reports the ROOT stuck dependency rather than the resource that
// merely blocked on it.
func (i korcAdminImports) objects() []orcv1alpha1.ObjectWithConditions {
	var objs []orcv1alpha1.ObjectWithConditions
	if i.domain != nil {
		objs = append(objs, i.domain)
	}
	if i.user != nil {
		objs = append(objs, i.user)
	}
	return objs
}

// statusFragment reports the first admin import (Domain before User) that is not
// yet usable — either a terminal K-ORC error or a not-yet-Available state — or an
// empty string when both imports are Available with no terminal error.
func (i korcAdminImports) statusFragment() string {
	if i.domain != nil {
		if frag := korcImportStatusFragment("Domain", i.domain.Name, i.domain); frag != "" {
			return frag
		}
	}
	if i.user != nil {
		return korcImportStatusFragment("User", i.user.Name, i.user)
	}
	return ""
}

// stalledImport reports the first admin import that has been waiting to be
// "created externally" for longer than grace, as a human-readable "Kind name"
// pair. See korcImportStalled for why that is a misconfiguration signal in
// External mode rather than a legitimate wait.
func (i korcAdminImports) stalledImport(grace time.Duration) (string, bool) {
	if korcImportStalled(i.domain, grace) {
		return fmt.Sprintf("Domain %q", i.domain.Name), true
	}
	if korcImportStalled(i.user, grace) {
		return fmt.Sprintf("User %q", i.user.Name), true
	}
	return "", false
}

// ensureKORCAdminImports ensures the K-ORC Domain and User that the admin
// ApplicationCredential's UserRef depends on exist as UNMANAGED imports. The
// Keystone bootstrap creates the real admin user (in the Default domain); K-ORC
// must import — not create — it, otherwise the ApplicationCredential blocks on
// "Waiting for User/admin to be created". Both CRs are owned by the ControlPlane
// for GC and reuse the admin clouds.yaml credentials.
//
// It returns both reconciled imports carrying live status (Server-Side Apply
// writes the server response back) so reconcileKORC can fold the stuck dependency into
// the KORCReady message and, in External mode, classify its condition messages —
// the documented failure class (wrong clouds.yaml endpoint, K-ORC swallowing list
// errors, an import hanging on "created externally") otherwise surfaces only as an
// eternal "WaitingForApplicationCredential" with no pointer to the real cause.
func (r *ControlPlaneReconciler) ensureKORCAdminImports(ctx context.Context, cp *c5c3v1alpha1.ControlPlane, credRef orcv1alpha1.CloudCredentialsReference) (korcAdminImports, error) {
	domain := &orcv1alpha1.Domain{
		ObjectMeta: metav1.ObjectMeta{Name: adminDomainRef(cp), Namespace: childNamespace(cp)},
		Spec: orcv1alpha1.DomainSpec{
			ManagementPolicy:    orcv1alpha1.ManagementPolicyUnmanaged,
			CloudCredentialsRef: credRef,
			Import: &orcv1alpha1.DomainImport{
				Filter: &orcv1alpha1.DomainFilter{Name: ptr.To(orcv1alpha1.KeystoneName(adminDomainName(cp)))},
			},
		},
	}
	if err := apply.EnsureObject(ctx, r.Client, r.Scheme, cp, domain, apply.FieldManager); err != nil {
		return korcAdminImports{}, fmt.Errorf("admin Domain import %q: %w", domain.Name, err)
	}

	user := &orcv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: adminUserRef(cp), Namespace: childNamespace(cp)},
		Spec: orcv1alpha1.UserSpec{
			ManagementPolicy:    orcv1alpha1.ManagementPolicyUnmanaged,
			CloudCredentialsRef: credRef,
			// The filter name MUST be the same identity buildPasswordCloudsYAML renders
			// as `username`: Keystone's default policy only lets a token mint an
			// application credential for its own user, and this User is the AC's UserRef.
			Import: &orcv1alpha1.UserImport{
				Filter: &orcv1alpha1.UserFilter{
					Name:      ptr.To(orcv1alpha1.OpenStackName(adminUserName(cp))),
					DomainRef: ptr.To(orcv1alpha1.KubernetesNameRef(adminDomainRef(cp))),
				},
			},
		},
	}
	if err := apply.EnsureObject(ctx, r.Client, r.Scheme, cp, user, apply.FieldManager); err != nil {
		return korcAdminImports{}, fmt.Errorf("admin User import %q: %w", user.Name, err)
	}

	return korcAdminImports{domain: domain, user: user}, nil
}

// korcImportStatusFragment reports the import-status fragment for a single K-ORC
// import resource: a terminal error takes precedence over a not-yet-Available
// state, and an Available resource with no terminal error yields an empty string.
func korcImportStatusFragment(kind, name string, obj orcv1alpha1.ObjectWithConditions) string {
	if termErr := orcv1alpha1.GetTerminalError(obj); termErr != nil {
		return fmt.Sprintf("admin %s import %q failed terminally: %v", kind, name, termErr)
	}
	if !orcv1alpha1.IsAvailable(obj) {
		return fmt.Sprintf("admin %s import %q is not yet Available", kind, name)
	}
	return ""
}
