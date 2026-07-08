// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/secrets"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
	"github.com/c5c3/forge/operators/keystone/internal/identity"
)

// Per-backend condition types and reasons for the OIDC federation objects.
// The dedicated KeystoneIdentityBackendReconciler owns both conditions (single
// status writer); the keystone-side sub-reconciler never touches them.
const (
	// conditionTypeFederationObjectsReady reports the keystone federation API
	// objects (identity provider + protocol) as upserted and drift-free.
	conditionTypeFederationObjectsReady = "FederationObjectsReady"
	// conditionTypeMappingsReady reports the mapping rules, declarative
	// groups, and role assignments as applied.
	conditionTypeMappingsReady = "MappingsReady"

	// FederationObjectsReady reasons (conditionReasonIdentityAPIError is
	// shared with DomainReady).
	conditionReasonFederationObjectsProvisioned = "FederationObjectsProvisioned"

	// MappingsReady reasons.
	conditionReasonMappingsApplied       = "MappingsApplied"
	conditionReasonNoMappingRules        = "NoMappingRules"
	conditionReasonRoleOrProjectNotFound = "RoleOrProjectNotFound"
)

// Shared federation projection vocabulary. Like the domains constants in
// reconcile_identitybackends.go, these live on the keystone side of the
// contract and are read by the dedicated backend controller
// (isConfigProjected) so both controllers agree on the volume/key naming by
// construction.
const (
	// federationProxyConfigVolumeName projects the rendered proxy.conf into
	// the mod_auth_openidc sidecar.
	federationProxyConfigVolumeName = "federation-proxy-config"
	// federationMetadataVolumeName projects the per-backend OIDCMetadataDir
	// documents into the sidecar.
	federationMetadataVolumeName = "federation-metadata"
	// federationProxyConfMountPath is where the sidecar's IncludeOptional
	// picks the rendered proxy.conf up.
	federationProxyConfMountPath = "/etc/keystone-federation-proxy/conf.d"
	// federationMetadataMountPath is the OIDCMetadataDir the sidecar serves
	// provider metadata from.
	federationMetadataMountPath = "/etc/keystone-federation-proxy/metadata"
)

// federationClientKeyName returns the Secret data key carrying one backend's
// mod_auth_openidc client document. Keyed by backend (CR) name — the real
// metadata filename (the RFC3986-escaped issuer) contains '%', which is
// invalid in Secret keys, so KeyToPath items map the safe key to the real
// filename at mount time.
func federationClientKeyName(backendName string) string {
	return backendName + ".client"
}

// federationMappingID returns the keystone mapping ID a backend's protocol
// binds to.
func federationMappingID(backend *keystonev1alpha1.KeystoneIdentityBackend) string {
	return backend.EffectiveIdentityProviderName() + "-mapping"
}

// ensureFederation upserts the three keystone federation API objects for one
// OIDC backend — identity provider, mapping, protocol — plus the declarative
// groups and role assignments, with real drift detection (writes happen only
// on divergence). Ordering is load-bearing: the mapping must exist before the
// protocol that references it, so the sequence is identity provider →
// mapping/groups → protocol. FederationObjectsReady covers the identity
// provider + protocol; MappingsReady covers the mapping, groups, and role
// assignments.
func (r *KeystoneIdentityBackendReconciler) ensureFederation(ctx context.Context, backend *keystonev1alpha1.KeystoneIdentityBackend, idc identity.Client) (ctrl.Result, error) {
	// A mapping with zero rules is unrepresentable in keystone (the mapping
	// schema requires at least one rule) and a protocol cannot exist without
	// its mapping — surface the pending state loudly instead of failing API
	// calls on every pass. Spec-driven: adding spec.mappings bumps the
	// generation and re-enqueues this backend.
	if len(backend.Spec.Mappings) == 0 {
		msg := "spec.mappings is empty: keystone requires at least one mapping rule before the federation protocol can be provisioned"
		r.setFederationObjectsReady(backend, metav1.ConditionFalse, conditionReasonNoMappingRules, msg)
		r.setMappingsReady(backend, metav1.ConditionFalse, conditionReasonNoMappingRules, msg)
		return ctrl.Result{}, nil
	}

	if result, err := r.ensureIdentityProvider(ctx, backend, idc); !result.IsZero() || err != nil {
		return result, err
	}
	if result, err := r.ensureMappingAndGroups(ctx, backend, idc); !result.IsZero() || err != nil {
		return result, err
	}
	return r.ensureProtocol(ctx, backend, idc)
}

// ensureIdentityProvider upserts the keystone identity provider named
// spec.oidc.identityProviderName with remote_ids=[issuer] and the backend's
// domain, drift-patching description/remoteIDs/enabled only on change.
func (r *KeystoneIdentityBackendReconciler) ensureIdentityProvider(ctx context.Context, backend *keystonev1alpha1.KeystoneIdentityBackend, idc identity.Client) (ctrl.Result, error) {
	idpName := backend.EffectiveIdentityProviderName()
	issuer := backend.Spec.OIDC.Issuer
	description := backend.Spec.Domain.Description

	existing, err := idc.GetIdentityProvider(ctx, idpName)
	if err != nil && !errors.Is(err, identity.ErrNotFound) {
		r.setFederationObjectsReady(backend, metav1.ConditionFalse, conditionReasonIdentityAPIError,
			fmt.Sprintf("looking up identity provider %q: %v", idpName, err))
		return ctrl.Result{}, fmt.Errorf("looking up identity provider %q: %w", idpName, err)
	}

	if existing == nil {
		if err := idc.CreateIdentityProvider(ctx, identity.IdentityProvider{
			ID:          idpName,
			DomainID:    backend.Status.DomainID,
			Description: description,
			Enabled:     ptr.To(true),
			RemoteIDs:   []string{issuer},
		}); err != nil {
			r.setFederationObjectsReady(backend, metav1.ConditionFalse, conditionReasonIdentityAPIError,
				fmt.Sprintf("creating identity provider %q: %v", idpName, err))
			return ctrl.Result{}, fmt.Errorf("creating identity provider %q: %w", idpName, err)
		}
		r.Recorder.Eventf(backend, corev1.EventTypeNormal, "IdentityProviderCreated",
			"Created identity provider %q (remote ID %s)", idpName, issuer)
		return ctrl.Result{}, nil
	}

	// Drift-only writes: patch exactly the diverged fields, never the
	// domain_id (immutable in keystone once set).
	var enabled *bool
	if existing.Enabled != nil && !*existing.Enabled {
		enabled = ptr.To(true)
	}
	var desc *string
	if existing.Description != description {
		desc = &description
	}
	var remoteIDs []string
	if !reflect.DeepEqual(existing.RemoteIDs, []string{issuer}) {
		remoteIDs = []string{issuer}
	}
	if enabled != nil || desc != nil || remoteIDs != nil {
		if err := idc.UpdateIdentityProvider(ctx, idpName, enabled, desc, remoteIDs); err != nil {
			r.setFederationObjectsReady(backend, metav1.ConditionFalse, conditionReasonIdentityAPIError,
				fmt.Sprintf("updating identity provider %q: %v", idpName, err))
			return ctrl.Result{}, fmt.Errorf("updating identity provider %q: %w", idpName, err)
		}
	}
	return ctrl.Result{}, nil
}

// ensureMappingAndGroups upserts the keystone mapping (create, or update only
// on rules drift via deep-compare), the declarative groups (by name in the
// backend's domain, create-if-missing), and the role assignments (resolved by
// role/project name; the assignment PUT is idempotent). It maintains
// MappingsReady.
func (r *KeystoneIdentityBackendReconciler) ensureMappingAndGroups(ctx context.Context, backend *keystonev1alpha1.KeystoneIdentityBackend, idc identity.Client) (ctrl.Result, error) {
	mappingID := federationMappingID(backend)
	desired := toGophercloudMappingRules(backend.Spec.Mappings)

	existing, err := idc.GetMapping(ctx, mappingID)
	switch {
	case errors.Is(err, identity.ErrNotFound):
		if err := idc.CreateMapping(ctx, mappingID, desired); err != nil {
			r.setMappingsReady(backend, metav1.ConditionFalse, conditionReasonIdentityAPIError,
				fmt.Sprintf("creating mapping %q: %v", mappingID, err))
			return ctrl.Result{}, fmt.Errorf("creating mapping %q: %w", mappingID, err)
		}
		r.Recorder.Eventf(backend, corev1.EventTypeNormal, "MappingCreated",
			"Created federation mapping %q (%d rules)", mappingID, len(desired))
	case err != nil:
		r.setMappingsReady(backend, metav1.ConditionFalse, conditionReasonIdentityAPIError,
			fmt.Sprintf("looking up mapping %q: %v", mappingID, err))
		return ctrl.Result{}, fmt.Errorf("looking up mapping %q: %w", mappingID, err)
	case !reflect.DeepEqual(existing.Rules, desired):
		if err := idc.UpdateMapping(ctx, mappingID, desired); err != nil {
			r.setMappingsReady(backend, metav1.ConditionFalse, conditionReasonIdentityAPIError,
				fmt.Sprintf("updating mapping %q: %v", mappingID, err))
			return ctrl.Result{}, fmt.Errorf("updating mapping %q: %w", mappingID, err)
		}
		r.Recorder.Eventf(backend, corev1.EventTypeNormal, "MappingUpdated",
			"Updated federation mapping %q (%d rules)", mappingID, len(desired))
	}

	if result, err := r.ensureFederationGroups(ctx, backend, idc); !result.IsZero() || err != nil {
		return result, err
	}

	r.setMappingsReady(backend, metav1.ConditionTrue, conditionReasonMappingsApplied,
		fmt.Sprintf("mapping %q, %d group(s), and role assignments applied", mappingID, len(backend.Spec.Groups)))
	return ctrl.Result{}, nil
}

// ensureFederationGroups creates the declarative target groups in the
// backend's domain (create-if-missing, never mutated afterwards — keystone
// cascades group deletion with the domain per the deletion policy) and
// applies their role assignments.
func (r *KeystoneIdentityBackendReconciler) ensureFederationGroups(ctx context.Context, backend *keystonev1alpha1.KeystoneIdentityBackend, idc identity.Client) (ctrl.Result, error) {
	domainID := backend.Status.DomainID
	for i := range backend.Spec.Groups {
		gs := &backend.Spec.Groups[i]

		group, err := idc.GetGroupByName(ctx, gs.Name, domainID)
		if errors.Is(err, identity.ErrNotFound) {
			group, err = idc.CreateGroup(ctx, identity.Group{
				Name:        gs.Name,
				DomainID:    domainID,
				Description: gs.Description,
			})
			if err == nil {
				r.Recorder.Eventf(backend, corev1.EventTypeNormal, "FederationGroupCreated",
					"Created group %q in domain %s", gs.Name, domainID)
			}
		}
		if err != nil {
			r.setMappingsReady(backend, metav1.ConditionFalse, conditionReasonIdentityAPIError,
				fmt.Sprintf("ensuring group %q: %v", gs.Name, err))
			return ctrl.Result{}, fmt.Errorf("ensuring group %q: %w", gs.Name, err)
		}

		for j := range gs.RoleAssignments {
			if result, err := r.applyRoleAssignment(ctx, backend, idc, group, &gs.RoleAssignments[j]); !result.IsZero() || err != nil {
				return result, err
			}
		}
	}
	return ctrl.Result{}, nil
}

// applyRoleAssignment grants one role to the group on the backend's domain or
// on a named project. A missing role or project is a waiting state (the
// operator may be racing whoever provisions them), not a hard failure:
// MappingsReady goes False/RoleOrProjectNotFound and the pass retries on a
// bounded poll.
func (r *KeystoneIdentityBackendReconciler) applyRoleAssignment(ctx context.Context, backend *keystonev1alpha1.KeystoneIdentityBackend, idc identity.Client, group *identity.Group, ra *keystonev1alpha1.FederationRoleAssignmentSpec) (ctrl.Result, error) {
	role, err := idc.GetRoleByName(ctx, ra.Role)
	if errors.Is(err, identity.ErrNotFound) {
		r.setMappingsReady(backend, metav1.ConditionFalse, conditionReasonRoleOrProjectNotFound,
			fmt.Sprintf("role %q not found", ra.Role))
		return ctrl.Result{RequeueAfter: RequeueDatabaseWait}, nil
	}
	if err != nil {
		r.setMappingsReady(backend, metav1.ConditionFalse, conditionReasonIdentityAPIError,
			fmt.Sprintf("looking up role %q: %v", ra.Role, err))
		return ctrl.Result{}, fmt.Errorf("looking up role %q: %w", ra.Role, err)
	}

	if ra.Domain {
		// Drift-only writes: probe before granting so steady-state passes
		// issue no mutating identity-API calls.
		has, err := idc.HasRoleForGroupOnDomain(ctx, backend.Status.DomainID, group.ID, role.ID)
		if err != nil {
			r.setMappingsReady(backend, metav1.ConditionFalse, conditionReasonIdentityAPIError,
				fmt.Sprintf("checking role %q of group %q on domain: %v", ra.Role, group.Name, err))
			return ctrl.Result{}, fmt.Errorf("checking role %q on domain: %w", ra.Role, err)
		}
		if has {
			return ctrl.Result{}, nil
		}
		if err := idc.AssignRoleToGroupOnDomain(ctx, backend.Status.DomainID, group.ID, role.ID); err != nil {
			r.setMappingsReady(backend, metav1.ConditionFalse, conditionReasonIdentityAPIError,
				fmt.Sprintf("assigning role %q to group %q on domain: %v", ra.Role, group.Name, err))
			return ctrl.Result{}, fmt.Errorf("assigning role %q on domain: %w", ra.Role, err)
		}
		return ctrl.Result{}, nil
	}

	// Project scope: the project domain defaults to the backend's own domain.
	projectDomainID := backend.Status.DomainID
	if ra.Project.DomainName != "" {
		domain, err := idc.GetDomainByName(ctx, ra.Project.DomainName)
		if errors.Is(err, identity.ErrNotFound) {
			r.setMappingsReady(backend, metav1.ConditionFalse, conditionReasonRoleOrProjectNotFound,
				fmt.Sprintf("domain %q of project %q not found", ra.Project.DomainName, ra.Project.Name))
			return ctrl.Result{RequeueAfter: RequeueDatabaseWait}, nil
		}
		if err != nil {
			r.setMappingsReady(backend, metav1.ConditionFalse, conditionReasonIdentityAPIError,
				fmt.Sprintf("looking up domain %q: %v", ra.Project.DomainName, err))
			return ctrl.Result{}, fmt.Errorf("looking up domain %q: %w", ra.Project.DomainName, err)
		}
		projectDomainID = domain.ID
	}
	project, err := idc.GetProjectByName(ctx, ra.Project.Name, projectDomainID)
	if errors.Is(err, identity.ErrNotFound) {
		r.setMappingsReady(backend, metav1.ConditionFalse, conditionReasonRoleOrProjectNotFound,
			fmt.Sprintf("project %q not found in domain %s", ra.Project.Name, projectDomainID))
		return ctrl.Result{RequeueAfter: RequeueDatabaseWait}, nil
	}
	if err != nil {
		r.setMappingsReady(backend, metav1.ConditionFalse, conditionReasonIdentityAPIError,
			fmt.Sprintf("looking up project %q: %v", ra.Project.Name, err))
		return ctrl.Result{}, fmt.Errorf("looking up project %q: %w", ra.Project.Name, err)
	}
	has, err := idc.HasRoleForGroupOnProject(ctx, project.ID, group.ID, role.ID)
	if err != nil {
		r.setMappingsReady(backend, metav1.ConditionFalse, conditionReasonIdentityAPIError,
			fmt.Sprintf("checking role %q of group %q on project %q: %v", ra.Role, group.Name, ra.Project.Name, err))
		return ctrl.Result{}, fmt.Errorf("checking role %q on project %q: %w", ra.Role, ra.Project.Name, err)
	}
	if has {
		return ctrl.Result{}, nil
	}
	if err := idc.AssignRoleToGroupOnProject(ctx, project.ID, group.ID, role.ID); err != nil {
		r.setMappingsReady(backend, metav1.ConditionFalse, conditionReasonIdentityAPIError,
			fmt.Sprintf("assigning role %q to group %q on project %q: %v", ra.Role, group.Name, ra.Project.Name, err))
		return ctrl.Result{}, fmt.Errorf("assigning role %q on project %q: %w", ra.Role, ra.Project.Name, err)
	}
	return ctrl.Result{}, nil
}

// ensureProtocol upserts the federation protocol binding
// spec.oidc.protocolID of the identity provider to the backend's mapping,
// drift-patching mapping_id only on change. It completes
// FederationObjectsReady.
func (r *KeystoneIdentityBackendReconciler) ensureProtocol(ctx context.Context, backend *keystonev1alpha1.KeystoneIdentityBackend, idc identity.Client) (ctrl.Result, error) {
	idpName := backend.EffectiveIdentityProviderName()
	protocolID := backend.EffectiveOIDCProtocolID()
	mappingID := federationMappingID(backend)

	existing, err := idc.GetProtocol(ctx, idpName, protocolID)
	switch {
	case errors.Is(err, identity.ErrNotFound):
		if err := idc.CreateProtocol(ctx, idpName, protocolID, mappingID); err != nil {
			r.setFederationObjectsReady(backend, metav1.ConditionFalse, conditionReasonIdentityAPIError,
				fmt.Sprintf("creating protocol %q: %v", protocolID, err))
			return ctrl.Result{}, fmt.Errorf("creating protocol %q: %w", protocolID, err)
		}
		r.Recorder.Eventf(backend, corev1.EventTypeNormal, "ProtocolCreated",
			"Created protocol %q on identity provider %q (mapping %s)", protocolID, idpName, mappingID)
	case err != nil:
		r.setFederationObjectsReady(backend, metav1.ConditionFalse, conditionReasonIdentityAPIError,
			fmt.Sprintf("looking up protocol %q: %v", protocolID, err))
		return ctrl.Result{}, fmt.Errorf("looking up protocol %q: %w", protocolID, err)
	case existing.MappingID != mappingID:
		if err := idc.UpdateProtocol(ctx, idpName, protocolID, mappingID); err != nil {
			r.setFederationObjectsReady(backend, metav1.ConditionFalse, conditionReasonIdentityAPIError,
				fmt.Sprintf("updating protocol %q: %v", protocolID, err))
			return ctrl.Result{}, fmt.Errorf("updating protocol %q: %w", protocolID, err)
		}
	}

	r.setFederationObjectsReady(backend, metav1.ConditionTrue, conditionReasonFederationObjectsProvisioned,
		fmt.Sprintf("identity provider %q and protocol %q provisioned", idpName, protocolID))
	return ctrl.Result{}, nil
}

// teardownFederationObjects removes the federation API objects on backend
// deletion, in reverse dependency order — protocol → mapping → identity
// provider — tolerating objects already gone. Unlike the domain deletion
// policy, this teardown is unconditional (the Phase-0 deletion decision: the
// finalizer always removes the federation objects); the domain itself still
// follows spec.domain.deletionPolicy, and groups created inside it follow the
// domain (keystone cascades domain contents). Failures warn and retry on a
// bounded poll; a missing admin credential fails open like the domain path.
func (r *KeystoneIdentityBackendReconciler) teardownFederationObjects(ctx context.Context, keystone *keystonev1alpha1.Keystone, backend *keystonev1alpha1.KeystoneIdentityBackend) (ctrl.Result, error) {
	creds, err := r.adminCredentials(ctx, keystone)
	if err != nil {
		if secrets.IsMissingSecretOrKey(err) {
			r.Recorder.Eventf(backend, corev1.EventTypeWarning, "FederationTeardownFailed",
				"Retaining federation objects of %q: admin password Secret is unavailable (%v)",
				backend.EffectiveIdentityProviderName(), err)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	idc := r.identityClient(internalAPIURL(keystone), creds)

	idpName := backend.EffectiveIdentityProviderName()
	protocolID := backend.EffectiveOIDCProtocolID()
	mappingID := federationMappingID(backend)

	if err := idc.DeleteProtocol(ctx, idpName, protocolID); err != nil && !errors.Is(err, identity.ErrNotFound) {
		r.Recorder.Eventf(backend, corev1.EventTypeWarning, "FederationTeardownFailed",
			"Deleting protocol %q failed: %v", protocolID, err)
		return ctrl.Result{RequeueAfter: RequeueDatabaseWait}, nil
	}
	if err := idc.DeleteMapping(ctx, mappingID); err != nil && !errors.Is(err, identity.ErrNotFound) {
		r.Recorder.Eventf(backend, corev1.EventTypeWarning, "FederationTeardownFailed",
			"Deleting mapping %q failed: %v", mappingID, err)
		return ctrl.Result{RequeueAfter: RequeueDatabaseWait}, nil
	}
	if err := idc.DeleteIdentityProvider(ctx, idpName); err != nil && !errors.Is(err, identity.ErrNotFound) {
		r.Recorder.Eventf(backend, corev1.EventTypeWarning, "FederationTeardownFailed",
			"Deleting identity provider %q failed: %v", idpName, err)
		return ctrl.Result{RequeueAfter: RequeueDatabaseWait}, nil
	}
	r.Recorder.Eventf(backend, corev1.EventTypeNormal, "FederationObjectsDeleted",
		"Deleted protocol %q, mapping %q, and identity provider %q", protocolID, mappingID, idpName)
	return ctrl.Result{}, nil
}

// setFederationObjectsReady upserts the FederationObjectsReady condition.
func (r *KeystoneIdentityBackendReconciler) setFederationObjectsReady(backend *keystonev1alpha1.KeystoneIdentityBackend, status metav1.ConditionStatus, reason, message string) {
	conditions.SetCondition(&backend.Status.Conditions, metav1.Condition{
		Type:               conditionTypeFederationObjectsReady,
		Status:             status,
		ObservedGeneration: backend.Generation,
		Reason:             reason,
		Message:            message,
	})
}

// setMappingsReady upserts the MappingsReady condition.
func (r *KeystoneIdentityBackendReconciler) setMappingsReady(backend *keystonev1alpha1.KeystoneIdentityBackend, status metav1.ConditionStatus, reason, message string) {
	conditions.SetCondition(&backend.Status.Conditions, metav1.Condition{
		Type:               conditionTypeMappingsReady,
		Status:             status,
		ObservedGeneration: backend.Generation,
		Reason:             reason,
		Message:            message,
	})
}
