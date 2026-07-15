// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package controller — reconcile_databasetls.go provisions the client
// certificate Keystone presents to MariaDB/MaxScale for mutual TLS
//
// The sub-reconciler has three lifecycle paths:
//
//   - NotRequired: spec.database.tls is nil or disabled — plaintext TCP,
//
// preserving the pre-existing behavior. No Certificate is created.
//   - ExternallyManaged: TLS is enabled but the database is brownfield
//     (spec.database.host, no clusterRef) — the operator does not own the
//     external database's trust domain, so the client keypair is supplied
//     out-of-band via spec.database.tls.clientCertSecretRef. No Certificate
//     is created.
//   - Managed: TLS is enabled and the database is a managed MariaDB cluster
//     (spec.database.clusterRef) — the operator issues a cert-manager
//     Certificate from the shared OpenStack DB CA issuer so MariaDB/MaxScale
//     trust it via their clientCASecretRef.
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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/database"
	commontls "github.com/c5c3/forge/internal/common/tls"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// dbCAIssuerName is the cluster-scoped cert-manager issuer that anchors the
// OpenStack DB trust domain. The same issuer is referenced by MariaDB.spec.tls
// (serverCertIssuerRef + clientCertIssuerRef) so the Keystone client cert and
// the MariaDB server/client certs share a root of trust.
// The matching ClusterIssuer is declared in
// deploy/flux-system/infrastructure/db-ca-issuer.yaml.
const dbCAIssuerName = "openstack-db-ca-issuer"

// reconcileDatabaseTLS provisions (or, for the disabled/brownfield paths,
// records the absence of) the client certificate Keystone uses for mutual TLS
// to the database.
func (r *KeystoneReconciler) reconcileDatabaseTLS(ctx context.Context,
	keystone *keystonev1alpha1.Keystone,
) (ctrl.Result, error) {
	tlsSpec := keystone.Spec.Database.TLS

	// NotRequired: TLS not requested — preserve pre-existing plaintext
	// behavior, create no Certificate. If TLS was
	// previously Managed, delete the now-orphaned <name>-db-client Certificate
	// so cert-manager stops renewing it and garbage-collects the issued Secret
	// via the owner-reference cascade — mirroring HPA/NetworkPolicy/HTTPRoute,
	// which all delete their managed objects on disable (issue #475).
	if !tlsSpec.IsEnabled() {
		if err := r.deleteManagedDBClientCertificate(ctx, keystone); err != nil {
			return ctrl.Result{}, err
		}
		conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
			Type:               conditionTypeDatabaseTLSReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: keystone.Generation,
			Reason:             reasonNotRequired,
			Message:            "Database TLS is not enabled; using plaintext connection",
		})
		return ctrl.Result{}, nil
	}

	// Fail closed against a bypassed or upgrade-pruned admission validation:
	// TLS is enabled, so both certificate secret references must be present —
	// the deployment and job builders mount them into a projected volume
	// (dbTLSVolumeAndMount), and an empty Secret name yields an invalid volume
	// the kubelet rejects, wedging the pod on startup. The validating webhook
	// enforces this on create/update, but a Keystone persisted under the
	// pre-mode-enum schema (which carried a separate tls.enabled bool) could
	// have mode="require" materialized while enabled:false left the refs empty;
	// once the CRD upgrade prunes the enabled field, IsEnabled() flips to true
	// against that stored object and no re-admission re-runs the webhook.
	// Surface a clear DatabaseTLSReady=False condition and stop the chain here,
	// so the flip degrades to a diagnosable status rather than a pod that will
	// not start.
	if tlsSpec.CABundleSecretRef.Name == "" || tlsSpec.ClientCertSecretRef.Name == "" {
		conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
			Type:               conditionTypeDatabaseTLSReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: keystone.Generation,
			Reason:             reasonMissingCertRefs,
			Message: "Database TLS is enabled but caBundleSecretRef.name and " +
				"clientCertSecretRef.name must both be set",
		})
		return ctrl.Result{}, fmt.Errorf(
			"database TLS enabled (mode %q) but caBundleSecretRef.name and/or clientCertSecretRef.name "+
				"are empty; refusing to build an invalid certificate volume", tlsSpec.Mode,
		)
	}

	// ExternallyManaged: TLS enabled but the database is brownfield (no
	// clusterRef). The operator does not own the external database's trust
	// domain; the client keypair is supplied out-of-band via
	// spec.database.tls.clientCertSecretRef. Delete any
	// previously-issued managed Certificate — a CR switched from managed to
	// brownfield must not leave the operator-owned <name>-db-client Certificate
	// being renewed indefinitely (issue #475).
	if keystone.Spec.Database.ClusterRef == nil {
		if err := r.deleteManagedDBClientCertificate(ctx, keystone); err != nil {
			return ctrl.Result{}, err
		}
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
	// issuer.
	cert := dbClientCertificate(keystone)
	ready, err := commontls.EnsureCertificate(ctx, r.Client, r.Scheme, keystone, cert)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring database client Certificate %s/%s: %w",
			cert.Namespace, cert.Name, err)
	}

	if !ready {
		log.FromContext(ctx).Info(
			"database client Certificate not yet ready; requeuing",
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

// dbClientCertificateName returns the name of the operator-issued client
// Certificate (and the Secret cert-manager writes it into):
// "<keystone.Name>-db-client".
func dbClientCertificateName(keystone *keystonev1alpha1.Keystone) string {
	return fmt.Sprintf("%s-db-client", keystone.Name)
}

// deleteManagedDBClientCertificate deletes the operator-issued
// <name>-db-client Certificate when DB TLS is disabled or switched to
// brownfield mode. It is a no-op when cert-manager is not installed: no
// Certificate can exist in that case, and an unconditional Delete would fail
// with "no matches for kind Certificate" — the same CRD-availability gate
// reconcileHTTPRoute applies before deleting an HTTPRoute (issue #475).
func (r *KeystoneReconciler) deleteManagedDBClientCertificate(ctx context.Context, keystone *keystonev1alpha1.Keystone) error {
	if !r.certManagerAvailable {
		return nil
	}
	return deleteDBClientCertificate(ctx, r.Client, keystone.Namespace, dbClientCertificateName(keystone))
}

// deleteDBClientCertificate deletes the Certificate identified by namespace and
// name. It tolerates NotFound, mirroring deleteNetworkPolicy / deleteHTTPRoute
// (issue #475). cert-manager garbage-collects the issued Secret via the
// Certificate owner-reference cascade once the Certificate is gone.
func deleteDBClientCertificate(ctx context.Context, c client.Client, namespace, name string) error {
	cert := &certmanagerv1.Certificate{}
	cert.SetName(name)
	cert.SetNamespace(namespace)
	if err := client.IgnoreNotFound(c.Delete(ctx, cert)); err != nil {
		return fmt.Errorf("deleting database client Certificate %s/%s: %w", namespace, name, err)
	}
	return nil
}

// dbClientCertificate builds the cert-manager Certificate for the Keystone
// database client keypair. The Secret it issues is named
// "<keystone.Name>-db-client" and is mounted into the workloads that open a
// database connection.
//
// CommonName is keystone.Name because the managed-mode database username
// materialised by reconcile_dbconnection_secret.go is the Keystone CR name;
// MaxScale/MariaDB authenticate the mTLS peer by that identity. Usages
// include client auth plus digital signature / key encipherment, the
// standard set for a TLS client keypair.
func dbClientCertificate(keystone *keystonev1alpha1.Keystone) *certmanagerv1.Certificate {
	name := dbClientCertificateName(keystone)
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
// database client certificate material. The
// matching in-pod path is dbTLSMountPath declared in
// reconcile_dbconnection_secret.go (which also derives the ssl_*-file paths
// fed into the DSN); the constants stay separate so the mount-point owner
// remains the DSN consumer, while the volume-name owner is this builder.
const dbTLSVolumeName = "db-tls"

// dbTLSEnabled reports whether the Keystone CR requests TLS to the database;
// the helper centralizes the nil/disabled gate so all callers (deployment +
// job builders) decide identically.
func dbTLSEnabled(keystone *keystonev1alpha1.Keystone) bool {
	return keystone.Spec.Database.TLS.IsEnabled()
}

// dbTLSVolumeAndMount builds the Volume + VolumeMount pair that projects the
// client TLS material (ca.crt from caBundleSecretRef; tls.crt + tls.key from
// clientCertSecretRef) into a Keystone workload pod.
// Callers must only invoke it when dbTLSEnabled(keystone) is true; the helper
// itself does not check the gate.
//
// The volume is a Projected volume that merges the two user-supplied Secrets
// onto a single mount path (dbTLSMountPath). Each Secret contributes the
// canonical cert-manager file names — ca.crt from caBundleSecretRef and
// tls.crt/tls.key from clientCertSecretRef — so the ssl_ca/ssl_cert/ssl_key
// DSN parameters (database.TLSFilePaths) resolve identically in both modes:
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
								{Key: database.TLSCAFileName, Path: database.TLSCAFileName},
							},
						},
					},
					{
						Secret: &corev1.SecretProjection{
							LocalObjectReference: corev1.LocalObjectReference{
								Name: tlsSpec.ClientCertSecretRef.Name,
							},
							Items: []corev1.KeyToPath{
								{Key: database.TLSCertFileName, Path: database.TLSCertFileName},
								{Key: database.TLSKeyFileName, Path: database.TLSKeyFileName},
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
