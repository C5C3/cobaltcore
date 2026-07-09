// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"time"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/c5c3/forge/internal/common/secrets"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
)

// openBaoClusterStoreName re-exports the shared ClusterSecretStore name (see
// secrets.OpenBaoClusterStoreName) for the mappers and sub-reconcilers in
// this package.
const openBaoClusterStoreName = secrets.OpenBaoClusterStoreName

// korcCloudsYamlSecretName is the conventional name of the admin clouds.yaml
// Secret (and its ExternalSecret) K-ORC reads its admin credentials from. It
// matches DefaultCloudCredentialsSecretName, the value the defaulting webhook
// applies to spec.korc.adminCredential.cloudCredentialsRef.secretName, and is
// used only as the fallback when a CR somehow reaches the reconciler without the
// webhook having defaulted that field.
//
// Co-location (C1): the ExternalSecret materialises this Secret into the
// SAME namespace as the K-ORC ApplicationCredential/Service/Endpoint CRs the
// operator projects — the ControlPlane's own namespace (childNamespace(cp)) —
// because K-ORC resolves CloudCredentialsRef in the resource's own namespace
// (vendored api/v1alpha1/credentials_ref.go GetCloudCredentialsRef returns the
// resource's Namespace). The AdminCredentialReady gate therefore waits on the
// ExternalSecret in childNamespace(cp), NOT a fixed orc-system one, so the minted
// AC is never pushed before K-ORC can actually authenticate.
const korcCloudsYamlSecretName = "k-orc-clouds-yaml" //nolint:gosec // G101 false positive: Secret name, not a credential.

// adminAppCredentialRemoteKeyFor returns the per-ControlPlane OpenBao path the
// minted admin application credential is mirrored to. The
// path is scoped by both the ControlPlane's Namespace and Name so two
// ControlPlanes never clobber each other's admin credential on the cluster-
// global OpenBao backend. The matching read consumer (the per-CR k-orc
// clouds.yaml ExternalSecret) targets the same key.
func adminAppCredentialRemoteKeyFor(cp *c5c3v1alpha1.ControlPlane) string {
	return fmt.Sprintf("openstack/keystone/%s/%s/admin/app-credential", cp.Namespace, cp.Name)
}

// adminAppCredentialPushSecretName returns the PushSecret name backing up the
// minted application credential to OpenBao.
func adminAppCredentialPushSecretName(cp *c5c3v1alpha1.ControlPlane) string {
	return cp.Name + "-admin-app-credential-backup"
}

// adminAppCredentialPushSecret builds the PushSecret that mirrors the minted
// admin application-credential Secret to OpenBao at the per-ControlPlane path
// openstack/keystone/{cp.Namespace}/{cp.Name}/admin/app-credential,
// scoping the credential so two ControlPlanes never clobber
// each other's admin credential on the cluster-global OpenBao backend.
//
// DECISION (DeletionPolicy): None — the admin application credential is a shared
// bootstrap secret other consumers may depend on; deleting the PushSecret (e.g.
// on ControlPlane teardown) leaves the last-pushed credential intact in OpenBao
// so a fresh control plane is not locked out mid-rotation. Reviewer: please verify.
func adminAppCredentialPushSecret(cp *c5c3v1alpha1.ControlPlane) *esov1alpha1.PushSecret {
	return &esov1alpha1.PushSecret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      adminAppCredentialPushSecretName(cp),
			Namespace: childNamespace(cp),
		},
		Spec: esov1alpha1.PushSecretSpec{
			DeletionPolicy: esov1alpha1.PushSecretDeletionPolicyNone,
			SecretStoreRefs: []esov1alpha1.PushSecretStoreRef{{
				Kind: "ClusterSecretStore",
				Name: openBaoClusterStoreName,
			}},
			Selector: esov1alpha1.PushSecretSelector{
				Secret: &esov1alpha1.PushSecretSecret{
					Name: adminAppCredentialSecretName(cp),
				},
			},
			Data: []esov1alpha1.PushSecretData{{
				Match: esov1alpha1.PushSecretMatch{
					RemoteRef: esov1alpha1.PushSecretRemoteRef{
						RemoteKey: adminAppCredentialRemoteKeyFor(cp),
					},
				},
			}},
		},
	}
}

// ensureKORCCloudsYAMLExternalSecret create-or-updates the per-ControlPlane,
// operator-owned ExternalSecret that materialises the admin clouds.yaml Secret
// K-ORC authenticates with, replacing the retired static single-identity manifest.
// It is created in childNamespace(cp) — co-located with the
// K-ORC resource CRs because K-ORC resolves CloudCredentialsRef in the resource's
// own namespace (C1) — and reads the per-CR OpenBao path
// adminAppCredentialRemoteKeyFor(cp), so an arbitrarily named ControlPlane resolves
// to the correct key with no manifest edit.
//
// The Secret name follows the spec's CloudCredentialsRef.SecretName (defaulted to
// korcCloudsYamlSecretName by the webhook; the fallback covers a webhook-bypass
// edge case). CreationPolicy Owner makes ESO own the materialised Secret, and the
// ExternalSecret itself is owner-referenced to the ControlPlane for GC. The
// ExternalSecret type is esov1 (NOT esov1alpha1 as the PushSecret above is).
//
// A second "cacert" data entry is added when the RESOLVED CA bundle is non-empty,
// so the materialised Secret — the credentials source for the admin imports and the
// catalog CRs — carries the trust anchor next to clouds.yaml. The PushSecret above
// pushes the source Secret WHOLE (no Match.SecretKey), so a COMPLETED push carries
// the key to the OpenBao path and only the read-back must be declared here.
//
// INVARIANT: the gate is the resolved bundle CONTENT, never the presence of
// caBundleSecretRef, and the caller must have nudged the push for that content
// FIRST. setCACertKey writes the source key under the same content predicate, but
// writing it does not push it — a read-back declared for a property no push has
// created yet flips the ExternalSecret to Ready=False and stalls the whole
// admin-credential pipeline behind a WaitingForCloudsYaml that blames the wrong
// Secret. reconcileKORC therefore stamps adminAppCredentialCACertHashAnnotation on
// the PushSecret immediately before calling this; ESO re-pushes on that metadata
// change and retries the ExternalSecret until the property resolves.
//
// Dropping the bundle drops the entry on the next CreateOrUpdate (the Data slice is
// rewritten, not merged), and the same stamp re-pushes a source Secret that no
// longer carries the key.
func (r *ControlPlaneReconciler) ensureKORCCloudsYAMLExternalSecret(ctx context.Context, cp *c5c3v1alpha1.ControlPlane, caBundle string) error {
	name := cp.Spec.KORC.AdminCredential.CloudCredentialsRef.SecretName
	if name == "" {
		name = korcCloudsYamlSecretName
	}
	es := &esov1.ExternalSecret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: childNamespace(cp),
		},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, es, func() error {
		es.Spec.RefreshInterval = &metav1.Duration{Duration: time.Hour}
		es.Spec.SecretStoreRef = esov1.SecretStoreRef{
			Kind: "ClusterSecretStore",
			Name: openBaoClusterStoreName,
		}
		es.Spec.Target = esov1.ExternalSecretTarget{
			Name:           name,
			CreationPolicy: esov1.CreatePolicyOwner,
		}
		data := []esov1.ExternalSecretData{{
			SecretKey: appCredCloudsYAMLKey,
			RemoteRef: esov1.ExternalSecretDataRemoteRef{
				Key:      adminAppCredentialRemoteKeyFor(cp),
				Property: appCredCloudsYAMLKey,
			},
		}}
		if caBundle != "" {
			data = append(data, esov1.ExternalSecretData{
				SecretKey: korcCACertKey,
				RemoteRef: esov1.ExternalSecretDataRemoteRef{
					Key:      adminAppCredentialRemoteKeyFor(cp),
					Property: korcCACertKey,
				},
			})
		}
		es.Spec.Data = data
		return controllerutil.SetControllerReference(cp, es, r.Scheme)
	}); err != nil {
		return fmt.Errorf("ensuring k-orc clouds.yaml ExternalSecret %q: %w", es.Name, err)
	}
	return nil
}
