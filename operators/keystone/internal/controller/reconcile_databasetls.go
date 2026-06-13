// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package controller — reconcile_databasetls.go provisions the client
// certificate Keystone presents to MariaDB/MaxScale for mutual TLS
// (CC-0106, REQ-002, REQ-014).
//
// The sub-reconciler has three lifecycle paths:
//
//   - NotRequired: spec.database.tls is nil or disabled — plaintext TCP,
//     preserving the pre-CC-0106 behavior. No Certificate is created.
//   - ExternallyManaged: TLS is enabled but the database is brownfield
//     (spec.database.host, no clusterRef) — the operator does not own the
//     external database's trust domain, so the client keypair is supplied
//     out-of-band via spec.database.tls.clientCertSecretRef. No Certificate
//     is created.
//   - Managed: TLS is enabled and the database is a managed MariaDB cluster
//     (spec.database.clusterRef) — the operator issues a cert-manager
//     Certificate from the shared OpenStack DB CA issuer so MariaDB/MaxScale
//     trust it via their clientCASecretRef (CC-0106, REQ-009).
//
// The typed condition/reason vocabulary lives in dbtls_mode.go alongside the
// DSN mode mapping so the whole DB-TLS surface is declared in one place.
package controller

import (
	"context"
	"fmt"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/forge/internal/common/conditions"
	commontls "github.com/c5c3/forge/internal/common/tls"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// Feature: CC-0106

// dbCAIssuerName is the cluster-scoped cert-manager issuer that anchors the
// OpenStack DB trust domain. The same issuer is referenced by MariaDB.spec.tls
// (serverCertIssuerRef + clientCertIssuerRef) so the Keystone client cert and
// the MariaDB server/client certs share a root of trust (CC-0106, REQ-009).
// The matching ClusterIssuer is declared in
// deploy/flux-system/infrastructure/db-ca-issuer.yaml.
const dbCAIssuerName = "openstack-db-ca-issuer"

// reconcileDatabaseTLS provisions (or, for the disabled/brownfield paths,
// records the absence of) the client certificate Keystone uses for mutual TLS
// to the database (CC-0106, REQ-002, REQ-014).
func (r *KeystoneReconciler) reconcileDatabaseTLS(ctx context.Context,
	keystone *keystonev1alpha1.Keystone,
) (ctrl.Result, error) {
	tlsSpec := keystone.Spec.Database.TLS

	// NotRequired: TLS not requested — preserve pre-CC-0106 plaintext
	// behavior, create no Certificate (CC-0106, REQ-002).
	if tlsSpec == nil || !tlsSpec.Enabled {
		conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
			Type:               conditionTypeDatabaseTLSReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: keystone.Generation,
			Reason:             reasonNotRequired,
			Message:            "Database TLS is not enabled; using plaintext connection",
		})
		return ctrl.Result{}, nil
	}

	// ExternallyManaged: TLS enabled but the database is brownfield (no
	// clusterRef). The operator does not own the external database's trust
	// domain; the client keypair is supplied out-of-band via
	// spec.database.tls.clientCertSecretRef (CC-0106, REQ-014).
	if keystone.Spec.Database.ClusterRef == nil {
		conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
			Type:               conditionTypeDatabaseTLSReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: keystone.Generation,
			Reason:             reasonExternallyManaged,
			Message: fmt.Sprintf(
				"Database TLS uses externally managed client certificate Secret %q (brownfield database)",
				tlsSpec.ClientCertSecretRef.Name,
			),
		})
		return ctrl.Result{}, nil
	}

	// Managed: issue the client Certificate from the shared OpenStack DB CA
	// issuer (CC-0106, REQ-002, REQ-009).
	cert := dbClientCertificate(keystone)
	ready, err := commontls.EnsureCertificate(ctx, r.Client, r.Scheme, keystone, cert)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring database client Certificate %s/%s: %w",
			cert.Namespace, cert.Name, err)
	}

	if !ready {
		log.FromContext(ctx).Info(
			"database client Certificate not yet ready; requeuing (CC-0106, REQ-002)",
			"namespace", cert.Namespace,
			"name", cert.Name,
		)
		conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
			Type:               conditionTypeDatabaseTLSReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: keystone.Generation,
			Reason:             reasonCertificatePending,
			Message: fmt.Sprintf("Waiting for cert-manager to issue database client Certificate %q",
				cert.Name),
		})
		return ctrl.Result{RequeueAfter: RequeueSecretPolling}, nil
	}

	conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
		Type:               conditionTypeDatabaseTLSReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: keystone.Generation,
		Reason:             reasonCertificateIssued,
		Message: fmt.Sprintf("Database client Certificate %q issued into Secret %q",
			cert.Name, cert.Spec.SecretName),
	})
	return ctrl.Result{}, nil
}

// dbClientCertificate builds the cert-manager Certificate for the Keystone
// database client keypair (CC-0106, REQ-002). The Secret it issues is named
// "<keystone.Name>-db-client" and is mounted into the workloads that open a
// database connection (mounted by CC-0106 task 4.2).
//
// CommonName is keystone.Name because the managed-mode database username
// materialised by reconcile_dbconnection_secret.go is the Keystone CR name;
// MaxScale/MariaDB authenticate the mTLS peer by that identity. Usages
// include client auth plus digital signature / key encipherment, the
// standard set for a TLS client keypair.
func dbClientCertificate(keystone *keystonev1alpha1.Keystone) *certmanagerv1.Certificate {
	name := fmt.Sprintf("%s-db-client", keystone.Name)
	return &certmanagerv1.Certificate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: keystone.Namespace,
			Labels:    commonLabels(keystone),
		},
		Spec: certmanagerv1.CertificateSpec{
			SecretName: name,
			CommonName: keystone.Name,
			IssuerRef: cmmeta.IssuerReference{
				Name:  dbCAIssuerName,
				Kind:  "ClusterIssuer",
				Group: "cert-manager.io",
			},
			Usages: []certmanagerv1.KeyUsage{
				certmanagerv1.UsageClientAuth,
				certmanagerv1.UsageDigitalSignature,
				certmanagerv1.UsageKeyEncipherment,
			},
		},
	}
}

// dbTLSVolumeName is the Volume / VolumeMount name used for the Keystone
// database client certificate material (CC-0106, REQ-002, REQ-014). The
// matching in-pod path is dbTLSMountPath declared in
// reconcile_dbconnection_secret.go (which also derives the ssl_*-file paths
// fed into the DSN); the constants stay separate so the mount-point owner
// remains the DSN consumer, while the volume-name owner is this builder.
const dbTLSVolumeName = "db-tls"

// dbTLSEnabled reports whether the Keystone CR requests TLS to the database;
// the helper centralizes the nil/disabled gate so all callers (deployment +
// job builders) decide identically (CC-0106, REQ-002, REQ-014).
func dbTLSEnabled(keystone *keystonev1alpha1.Keystone) bool {
	return keystone.Spec.Database.TLS != nil && keystone.Spec.Database.TLS.Enabled
}

// dbTLSVolumeAndMount builds the Volume + VolumeMount pair that projects the
// client TLS material (ca.crt from caBundleSecretRef; tls.crt + tls.key from
// clientCertSecretRef) into a Keystone workload pod (CC-0106, REQ-002, REQ-014).
// Callers must only invoke it when dbTLSEnabled(keystone) is true; the helper
// itself does not check the gate.
//
// The volume is a Projected volume that merges the two user-supplied Secrets
// onto a single mount path (dbTLSMountPath). Each Secret contributes the
// canonical cert-manager file names — ca.crt from caBundleSecretRef and
// tls.crt/tls.key from clientCertSecretRef — so the ssl_ca/ssl_cert/ssl_key
// DSN parameters (dbTLSPathsForMount) resolve identically in both modes:
//
//   - managed mode: both refs point to the operator-issued <name>-db-client
//     Secret (cert-manager writes all three keys into one Secret). The same
//     name appears in two projection sources, which the kubelet collapses
//     to the same backing Secret.
//   - brownfield mode: refs may point to two distinct Secrets — the canonical
//     enterprise PKI shape where the trust bundle and client keypair are
//     issued by separate authorities.
//
// DECISION: projected volume vs two separate volumes — Chose Projected so the
// existing dbTLSMountPath constant (/etc/keystone/db-tls/) and the ssl_*
// DSN paths derived from it stay a single source of truth. Splitting into
// two mounts would force a parallel constant set and a DSN rewrite.
// DefaultMode 0o400 mirrors the fernet-keys / credential-keys mode so the
// openstack UID can read the material while group/world have no access.
func dbTLSVolumeAndMount(keystone *keystonev1alpha1.Keystone) (corev1.Volume, corev1.VolumeMount) {
	tlsSpec := keystone.Spec.Database.TLS
	volume := corev1.Volume{
		Name: dbTLSVolumeName,
		VolumeSource: corev1.VolumeSource{
			Projected: &corev1.ProjectedVolumeSource{
				DefaultMode: ptr.To(int32(0o400)),
				Sources: []corev1.VolumeProjection{
					{
						Secret: &corev1.SecretProjection{
							LocalObjectReference: corev1.LocalObjectReference{
								Name: tlsSpec.CABundleSecretRef.Name,
							},
							Items: []corev1.KeyToPath{
								{Key: dbTLSCAFileName, Path: dbTLSCAFileName},
							},
						},
					},
					{
						Secret: &corev1.SecretProjection{
							LocalObjectReference: corev1.LocalObjectReference{
								Name: tlsSpec.ClientCertSecretRef.Name,
							},
							Items: []corev1.KeyToPath{
								{Key: dbTLSCertFileName, Path: dbTLSCertFileName},
								{Key: dbTLSKeyFileName, Path: dbTLSKeyFileName},
							},
						},
					},
				},
			},
		},
	}
	mount := corev1.VolumeMount{
		Name:      dbTLSVolumeName,
		MountPath: dbTLSMountPath,
		ReadOnly:  true,
	}
	return volume, mount
}
