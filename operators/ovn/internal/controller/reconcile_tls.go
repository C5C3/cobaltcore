// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"cmp"
	"context"
	"fmt"
	"strings"
	"time"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	"github.com/c5c3/cobaltcore/internal/common/naming"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	commontls "github.com/c5c3/cobaltcore/internal/common/tls"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// The condition type and reasons of the TLS step.
const (
	conditionTypeTLSReady = "TLSReady"

	conditionReasonCertManagerUnavailable = "CertManagerUnavailable"
	conditionReasonCertificatesIssued     = "CertificatesIssued"
	conditionReasonCertificatePending     = "CertificatePending"
	conditionReasonCertificateError       = "CertificateError"
)

// The lifetime every OVN certificate is issued with and the lead time
// cert-manager renews it at: a year of validity, renewed a month before it runs
// out. Each renewal rewrites the Secret and restarts nothing, so a long lead
// time costs nothing and leaves a renewal that keeps failing thirty days to be
// noticed before the databases start rejecting each other.
const (
	certificateDuration    = 8760 * time.Hour
	certificateRenewBefore = 720 * time.Hour
)

// certificateKeySize is the bit size of the P-256 key every OVN certificate
// carries. The handshake cost is paid again on every reconnect of every
// chassis, and an RSA key of comparable strength costs an order of magnitude
// more per handshake.
const certificateKeySize = 256

// defaultIssuerKind mirrors the +kubebuilder:default on OVNIssuerRef.Kind. It is
// resolved for a CR that reached the controller without one, which only happens
// when the CRD default was bypassed; an empty kind would otherwise reach
// cert-manager, which rejects it.
const defaultIssuerKind = "ClusterIssuer"

// certificateIssuerGroup is the API group of both cert-manager issuer kinds.
const certificateIssuerGroup = "cert-manager.io"

// reconcileTLS requests the certificates every OVN connection is authenticated
// with: one server keypair per database, one of its own for the relay tier when
// the CR runs one, and one client keypair shared by everything that dials them.
//
// The step ends at the issued client Secret rather than at the Certificates,
// because the Secret is what the workloads mount and what an OVNChassis is
// pointed at through status.clientSecretName. A Certificate cert-manager
// reports Ready but whose Secret carries no CA certificate is the one failure
// mode that looks converged from the Certificate alone and wedges every
// connection: OVN authenticates its peers against that CA and nothing else.
func (r *OVNCentralReconciler) reconcileTLS(ctx context.Context, children client.Client, cr *ovnv1alpha1.OVNCentral) (ctrl.Result, error) {
	serves, err := commonmulticluster.ChildrenServeKind(children, r.certManagerAvailable, certificateGVK)
	if err != nil {
		err = fmt.Errorf("probing the target cluster for the Certificate kind: %w", err)
		centralSkeleton.MarkFailed(cr, conditionTypeTLSReady, commonmulticluster.CapabilityProbeFailed, err)
		return ctrl.Result{}, err
	}
	if !serves {
		// A wait rather than an error: spec.tls is required, so the operator
		// cannot fall back to anything, and the fix (install cert-manager on the
		// cluster the children land on) happens outside the CR. Polling picks it
		// up without an edit to the CR.
		conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
			Type:               conditionTypeTLSReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cr.Generation,
			Reason:             conditionReasonCertManagerUnavailable,
			Message:            "cert-manager is not installed on the cluster the children land on; spec.tls requires it",
		})
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, nil
	}

	// All of them are ensured before the first pending one is reported, so a
	// cluster starting from nothing requests every certificate on its first pass
	// instead of one per polling interval.
	certificates := []*certmanagerv1.Certificate{
		serverCertificate(cr, northboundDB(cr)),
		serverCertificate(cr, southboundDB(cr)),
		clientCertificate(cr),
	}
	if cr.Spec.Relay != nil {
		certificates = append(certificates, relayCertificate(cr))
	}
	var pending []string
	for _, cert := range certificates {
		ready, err := commontls.EnsureCertificate(ctx, children, r.Scheme, cr, cert)
		if err != nil {
			err = fmt.Errorf("ensuring Certificate %s: %w", cert.Name, err)
			centralSkeleton.MarkFailed(cr, conditionTypeTLSReady, conditionReasonCertificateError, err)
			return ctrl.Result{}, err
		}
		if !ready {
			pending = append(pending, cert.Name)
		}
	}
	if len(pending) > 0 {
		conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
			Type:               conditionTypeTLSReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cr.Generation,
			Reason:             conditionReasonCertificatePending,
			Message:            fmt.Sprintf("Waiting for cert-manager to issue %s", strings.Join(pending, ", ")),
		})
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, nil
	}

	// The Secret is read through the target cluster's own uncached reader:
	// cert-manager wrote it moments ago, and a cache that has not caught up
	// would report it absent and hold the whole CR unready for a polling
	// interval.
	name := clientSecretName(cr)
	secret := &corev1.Secret{}
	switch err := commonmulticluster.LiveReader(children).Get(ctx,
		client.ObjectKey{Namespace: cr.Namespace, Name: name}, secret); {
	case apierrors.IsNotFound(err):
		conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
			Type:               conditionTypeTLSReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cr.Generation,
			Reason:             conditionReasonCertificatePending,
			Message:            fmt.Sprintf("Waiting for cert-manager to write Secret %s", name),
		})
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, nil
	case err != nil:
		err = fmt.Errorf("reading client Secret %s: %w", name, err)
		centralSkeleton.MarkFailed(cr, conditionTypeTLSReady, conditionReasonCertificateError, err)
		return ctrl.Result{}, err
	}

	// A Secret that is missing a key is reported and left alone: no requeue and
	// no returned error. Both would have the operator retry a state only an edit
	// to spec.tls.issuerRef can leave, and the retry would neither change the
	// Secret nor the message the CR already carries.
	if defect := clientSecretDefect(cr, secret); defect != "" {
		conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
			Type:               conditionTypeTLSReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cr.Generation,
			Reason:             conditionReasonCertificateError,
			Message:            defect,
		})
		return ctrl.Result{}, nil
	}

	cr.Status.ClientSecretName = name
	conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
		Type:               conditionTypeTLSReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: cr.Generation,
		Reason:             conditionReasonCertificatesIssued,
		Message:            fmt.Sprintf("Both server certificates and the client certificate in Secret %s are issued", name),
	})
	return ctrl.Result{}, nil
}

// clientSecretDefect names what the issued client Secret is missing, or returns
// the empty string when it carries all three keys. The missing CA certificate
// gets a message of its own because it has one cause worth naming: an issuer
// that is not CA-backed (ACME, for one) issues a perfectly valid keypair and no
// ca.crt at all, and OVN cannot authenticate a peer without it.
func clientSecretDefect(cr *ovnv1alpha1.OVNCentral, secret *corev1.Secret) string {
	for _, key := range []string{corev1.TLSCertKey, corev1.TLSPrivateKeyKey} {
		if len(secret.Data[key]) == 0 {
			return fmt.Sprintf("issued Secret %s lacks %s", clientSecretName(cr), key)
		}
	}
	if len(secret.Data[cmmeta.TLSCAKey]) == 0 {
		return fmt.Sprintf("issuer %s/%s issued no %s; spec.tls.issuerRef must name a CA issuer",
			cmp.Or(cr.Spec.TLS.IssuerRef.Kind, defaultIssuerKind), cr.Spec.TLS.IssuerRef.Name, cmmeta.TLSCAKey)
	}
	return ""
}

// clientSecretName names the Secret holding the keypair every OVN client
// authenticates with, and the Certificate that issues it. It is published in
// status.clientSecretName, which is what an OVNChassis mounts.
func clientSecretName(cr *ovnv1alpha1.OVNCentral) string {
	return cr.Name + "-client"
}

// serverCertificate builds the keypair one database's Raft members listen with.
//
// The subject alternative names cover each member under both spellings the
// cluster resolves it by, plus the headless Service itself: a member dials its
// peers by the FQDN the run script takes from hostname -f, while a client
// inside the cluster may hold either form. The usages include client auth
// because a Raft peer connects out with the same keypair it listens with, so a
// server-only certificate would let the members accept connections they cannot
// make.
func serverCertificate(cr *ovnv1alpha1.OVNCentral, db raftDB) *certmanagerv1.Certificate {
	name := raftServerSecretName(cr, db)
	service := raftName(cr, db)

	var dnsNames []string
	for ordinal := int32(0); ordinal < db.spec.Replicas; ordinal++ {
		member := raftMemberName(cr, db, ordinal)
		dnsNames = append(dnsNames,
			fmt.Sprintf("%s.%s.%s.svc.cluster.local", member, service, cr.Namespace),
			fmt.Sprintf("%s.%s.%s.svc", member, service, cr.Namespace))
	}
	dnsNames = append(dnsNames, fmt.Sprintf("%s.%s.svc.cluster.local", service, cr.Namespace))

	cert := baseCertificate(cr, name)
	cert.Spec.CommonName = service
	cert.Spec.DNSNames = dnsNames
	cert.Spec.Usages = []certmanagerv1.KeyUsage{
		certmanagerv1.UsageServerAuth,
		certmanagerv1.UsageClientAuth,
		certmanagerv1.UsageDigitalSignature,
		certmanagerv1.UsageKeyEncipherment,
	}
	return cert
}

// relayCertificate builds the keypair the Southbound relays listen with and
// dial the Raft cluster with. ovn-ctl configures a relay with one key and one
// certificate for both directions, so this one carries both usages.
//
// It is the relay's own identity rather than the Southbound database's server
// keypair. The relay tier is the largest attack surface in the control plane —
// every chassis in the fleet holds an open connection to it — and handing it
// the database's private key would make one relay pod the whole database's
// identity to anything that verifies the name it dialled.
func relayCertificate(cr *ovnv1alpha1.OVNCentral) *certmanagerv1.Certificate {
	name := relayName(cr)

	cert := baseCertificate(cr, name)
	cert.Spec.CommonName = name
	cert.Spec.DNSNames = []string{
		fmt.Sprintf("%s.%s.svc.cluster.local", name, cr.Namespace),
		fmt.Sprintf("%s.%s.svc", name, cr.Namespace),
	}
	cert.Spec.Usages = []certmanagerv1.KeyUsage{
		certmanagerv1.UsageServerAuth,
		certmanagerv1.UsageClientAuth,
		certmanagerv1.UsageDigitalSignature,
		certmanagerv1.UsageKeyEncipherment,
	}
	return cert
}

// clientCertificate builds the keypair every OVN client authenticates with:
// northd against both databases, an OVNChassis against the Southbound, and an
// operator running ovn-nbctl by hand. One certificate serves them all today,
// which is the whole authorization story: the connection rows carry no
// role= column, so OVN's own RBAC is not in force and every certificate the
// configured CA signed has unrestricted read and write on both databases.
// Per-client identities and an ovn-controller role on the Southbound listener
// are a design change the two have to make together.
func clientCertificate(cr *ovnv1alpha1.OVNCentral) *certmanagerv1.Certificate {
	name := clientSecretName(cr)

	cert := baseCertificate(cr, name)
	cert.Spec.CommonName = name
	cert.Spec.Usages = []certmanagerv1.KeyUsage{
		certmanagerv1.UsageClientAuth,
		certmanagerv1.UsageDigitalSignature,
		certmanagerv1.UsageKeyEncipherment,
	}
	return cert
}

// baseCertificate builds what the three certificates share: the issuer, the
// lifetime, the key parameters, and a Secret named after the Certificate
// itself, so nothing has to carry a second name to find the material.
//
// The labels are the CR's shared set rather than a per-component one. Two of
// the three belong to a database each and the third to no component at all, and
// one label set lets a selector reach every certificate of one control plane
// without enumerating components.
func baseCertificate(cr *ovnv1alpha1.OVNCentral, name string) *certmanagerv1.Certificate {
	return &certmanagerv1.Certificate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cr.Namespace,
			Labels:    naming.CommonLabels(centralAppName, cr.Name),
		},
		Spec: certmanagerv1.CertificateSpec{
			SecretName:  name,
			Duration:    &metav1.Duration{Duration: certificateDuration},
			RenewBefore: &metav1.Duration{Duration: certificateRenewBefore},
			PrivateKey: &certmanagerv1.CertificatePrivateKey{
				Algorithm: certmanagerv1.ECDSAKeyAlgorithm,
				Size:      certificateKeySize,
			},
			IssuerRef: cmmeta.IssuerReference{
				Name:  cr.Spec.TLS.IssuerRef.Name,
				Kind:  cmp.Or(cr.Spec.TLS.IssuerRef.Kind, defaultIssuerKind),
				Group: certificateIssuerGroup,
			},
		},
	}
}
