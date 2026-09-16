// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	"github.com/c5c3/cobaltcore/internal/common/database"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	"github.com/c5c3/cobaltcore/internal/common/secrets"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
)

// conditionReasonMetadataSharedSecretEmpty is the SecretsReady=False reason set
// while the metadata shared secret's key exists but carries no value.
const conditionReasonMetadataSharedSecretEmpty = "MetadataSharedSecretEmpty"

// secretValues carries the three Secret values reconcileSecrets read once the
// gates passed. All three feed the compute contract, the Secret a nova-compute
// outside this cluster reads to join the control plane: the service-user
// password authenticates its calls, the metadata shared secret is the value the
// Neutron metadata agent signs proxied requests with, and messagingCA is the
// broker's CA bundle. messagingCA is empty when spec.messaging.tls is unset,
// which is the plaintext bus.
type secretValues struct {
	serviceUserPassword  string
	metadataSharedSecret string
	messagingCA          string
}

// effectiveServiceUserKey returns the Secret data key holding the service-user
// password, defaulting to novav1alpha1.DefaultServiceUserSecretKey when
// spec.serviceUser.secretRef.key is empty (a CR that bypassed the defaulting
// webhook).
func effectiveServiceUserKey(nova *novav1alpha1.Nova) string {
	if key := nova.Spec.ServiceUser.SecretRef.Key; key != "" {
		return key
	}
	return novav1alpha1.DefaultServiceUserSecretKey
}

// effectiveSharedSecretKey returns the Secret data key holding the metadata
// shared secret, defaulting to novav1alpha1.DefaultSharedSecretKey when
// spec.metadata.sharedSecretRef.key is empty (a CR that bypassed the defaulting
// webhook).
func effectiveSharedSecretKey(nova *novav1alpha1.Nova) string {
	if key := nova.Spec.Metadata.SharedSecretRef.Key; key != "" {
		return key
	}
	return novav1alpha1.DefaultSharedSecretKey
}

// effectiveMessagingCAKey returns the Secret data key holding the broker's CA
// bundle, defaulting to database.TLSCAFileName ("ca.crt") when
// spec.messaging.tls.caBundleSecretRef.key is empty. No defaulting webhook fills
// that key on a Nova, and the same fallback picks the key the pod projection
// renames to the file [oslo_messaging_rabbit] ssl_ca_file names. Callers must
// only invoke it when spec.messaging.tls is set.
func effectiveMessagingCAKey(nova *novav1alpha1.Nova) string {
	if key := nova.Spec.Messaging.TLS.CABundleSecretRef.Key; key != "" {
		return key
	}
	return database.TLSCAFileName
}

// reconcileSecrets checks that the Kubernetes Secrets Nova reads exist before
// proceeding and returns the three values they carry. It gates on the selected
// secret store first, then on five credential Secrets in order: the nova_api
// database credentials, the cell database credentials, the service-user
// password, the metadata shared secret, and, only for a Nova whose bus is TLS,
// the broker CA bundle. Every gate maintains SecretsReady, and a gate that is
// not ready returns the polling requeue with empty values, so no later step
// renders a config or rolls a pod on a half-synced Secret.
//
// Nova always has a service user. An instance boot needs a Placement
// allocation, a Neutron port and a Glance image, and all three are
// authenticated calls, so there is no Keystone-free arm to skip the gate for.
//
// The pipeline derives two rollout digests from the returned values with
// secrets.AdminPasswordDigest, one over the service-user password and one over
// the metadata shared secret, and the deployment steps stamp them into
// pod-template annotations. Both values are env-var-consumed rather than
// volume-mounted, so a rotation at the OpenBao source only takes effect on a Pod
// restart. The values themselves go on to the compute-config Secret the compute
// nodes read.
//
// Both the selected store and the credential Secrets are read on the children
// cluster: they are materialised beside the workload that consumes them.
func (r *NovaReconciler) reconcileSecrets(ctx context.Context, children client.Client,
	nova *novav1alpha1.Nova,
) (ctrl.Result, secretValues, error) {
	// Check the selected secret store first so upstream backend outages surface
	// as SecretsReady=False even while per-ExternalSecret caches still report
	// Ready=True from their last successful sync. The store is the one this Nova
	// selected via spec.secretStoreRef (default: the shared cluster-scoped
	// openbao-cluster-store); a namespaced store is resolved in the Nova's own
	// namespace.
	storeReady, err := secrets.GateStoreReady(ctx, children,
		secrets.EffectiveStoreRef(nova.Spec.SecretStoreRef), nova.Namespace,
		&nova.Status.Conditions, nova.Generation, "SecretsReady")
	if err != nil {
		return ctrl.Result{}, secretValues{}, err
	}
	if !storeReady {
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, secretValues{}, nil
	}

	// Validate the credential Secrets from a declarative (secretRef, expectedKeys)
	// list. Each check reads the materialized Secret first (the steady-state fast
	// path) and only consults the ExternalSecret to build a precise
	// SecretsReady=False message when the Secret is not yet usable.
	serviceUserKey := effectiveServiceUserKey(nova)
	sharedSecretKey := effectiveSharedSecretKey(nova)
	credentialGates := []secrets.CredentialGateSpec{
		{
			Key:          client.ObjectKey{Namespace: nova.Namespace, Name: nova.Spec.APIDatabase.SecretRef.Name},
			Reason:       "WaitingForDBCredentials",
			Noun:         "API database credentials",
			WaitingMsg:   "Waiting for ESO to sync the nova_api database credentials from OpenBao",
			ExpectedKeys: []string{"username", "password"},
		},
		{
			Key:          client.ObjectKey{Namespace: nova.Namespace, Name: nova.Spec.Database.SecretRef.Name},
			Reason:       "WaitingForDBCredentials",
			Noun:         "Cell database credentials",
			WaitingMsg:   "Waiting for ESO to sync the cell database credentials from OpenBao",
			ExpectedKeys: []string{"username", "password"},
		},
		{
			Key:          client.ObjectKey{Namespace: nova.Namespace, Name: nova.Spec.ServiceUser.SecretRef.Name},
			Reason:       "WaitingForServiceUserCredentials",
			Noun:         "Service-user credentials",
			WaitingMsg:   "Waiting for ESO to sync the Nova service-user password from OpenBao",
			ExpectedKeys: []string{serviceUserKey},
		},
		{
			Key:          client.ObjectKey{Namespace: nova.Namespace, Name: nova.Spec.Metadata.SharedSecretRef.Name},
			Reason:       "WaitingForMetadataSharedSecret",
			Noun:         "Nova metadata shared secret",
			WaitingMsg:   "Waiting for ESO to sync the Nova metadata shared secret from OpenBao",
			ExpectedKeys: []string{sharedSecretKey},
		},
	}
	// The broker CA is gated only for a bus the CR asked to verify. A Nova on a
	// plaintext bus references no such Secret, and gating on one would leave it
	// waiting for a Secret nothing ever creates.
	messagingCAKey := ""
	if nova.Spec.Messaging.TLS != nil {
		messagingCAKey = effectiveMessagingCAKey(nova)
		credentialGates = append(credentialGates, secrets.CredentialGateSpec{
			Key: client.ObjectKey{
				Namespace: nova.Namespace,
				Name:      nova.Spec.Messaging.TLS.CABundleSecretRef.Name,
			},
			Reason:       "WaitingForMessagingCA",
			Noun:         "Messaging CA bundle",
			WaitingMsg:   "Waiting for the RabbitMQ CA bundle Secret to carry the broker trust anchor",
			ExpectedKeys: []string{messagingCAKey},
		})
	}
	ready, err := secrets.GateCredentials(ctx, children, credentialGates,
		&nova.Status.Conditions, nova.Generation, "SecretsReady")
	if err != nil {
		return ctrl.Result{}, secretValues{}, err
	}
	if !ready {
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, secretValues{}, nil
	}

	// Read the gated values. A read that fails here is a backend failure rather
	// than a missing Secret, so it is returned wrapped instead of polled: an
	// empty value would clear a rollout annotation and roll every pod.
	var values secretValues
	values.serviceUserPassword, err = secrets.GetSecretValue(ctx, children,
		client.ObjectKey{Namespace: nova.Namespace, Name: nova.Spec.ServiceUser.SecretRef.Name}, serviceUserKey)
	if err != nil {
		return ctrl.Result{}, secretValues{}, fmt.Errorf("reading the service-user password: %w", err)
	}
	values.metadataSharedSecret, err = secrets.GetSecretValue(ctx, children,
		client.ObjectKey{Namespace: nova.Namespace, Name: nova.Spec.Metadata.SharedSecretRef.Name}, sharedSecretKey)
	if err != nil {
		return ctrl.Result{}, secretValues{}, fmt.Errorf("reading the metadata shared secret: %w", err)
	}
	// The gate above only asks whether the key exists, so an empty value passes
	// it, and an empty value is what a wrong OpenBao path or ExternalSecret
	// template syncs. nova keys the HMAC it checks X-Instance-ID-Signature against
	// with this value and refuses no empty key, so any client that reaches the
	// metadata API could sign a request for any instance and read its user data.
	// The value is also copied into the compute contract. The step waits on it
	// exactly as it waits on a missing key.
	if values.metadataSharedSecret == "" {
		conditions.SetCondition(&nova.Status.Conditions, metav1.Condition{
			Type:               "SecretsReady",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: nova.Generation,
			Reason:             conditionReasonMetadataSharedSecretEmpty,
			Message: fmt.Sprintf("Secret %s key %q is empty; the metadata API would accept a forged instance signature",
				nova.Spec.Metadata.SharedSecretRef.Name, sharedSecretKey),
		})
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, secretValues{}, nil
	}
	if nova.Spec.Messaging.TLS != nil {
		values.messagingCA, err = secrets.GetSecretValue(ctx, children,
			client.ObjectKey{Namespace: nova.Namespace, Name: nova.Spec.Messaging.TLS.CABundleSecretRef.Name},
			messagingCAKey)
		if err != nil {
			return ctrl.Result{}, secretValues{}, fmt.Errorf("reading the messaging CA bundle: %w", err)
		}
	}

	conditions.SetCondition(&nova.Status.Conditions, metav1.Condition{
		Type:               "SecretsReady",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: nova.Generation,
		Reason:             "SecretsAvailable",
	})
	return ctrl.Result{}, values, nil
}
