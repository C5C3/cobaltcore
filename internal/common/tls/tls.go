// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package tls

import (
	"context"
	"fmt"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// EnsureCertificate creates a cert-manager Certificate if it does not exist or
// updates its spec if it already exists. It returns (true, nil) when the
// Certificate has a Ready condition with status True, (false, nil) when it
// exists but is not yet ready, and (false, error) on unexpected failures
func EnsureCertificate(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, cert *certmanagerv1.Certificate) (bool, error) {
	existing := &certmanagerv1.Certificate{}
	err := c.Get(ctx, client.ObjectKeyFromObject(cert), existing)

	if apierrors.IsNotFound(err) {
		if err := controllerutil.SetControllerReference(owner, cert, scheme); err != nil {
			return false, fmt.Errorf("setting owner reference on Certificate %s/%s: %w", cert.Namespace, cert.Name, err)
		}
		if err := c.Create(ctx, cert); err != nil {
			return false, fmt.Errorf("creating Certificate %s/%s: %w", cert.Namespace, cert.Name, err)
		}
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("getting Certificate %s/%s: %w", cert.Namespace, cert.Name, err)
	}

	if !apiequality.Semantic.DeepEqual(existing.Spec, cert.Spec) {
		existing.Spec = cert.Spec
		if err := c.Update(ctx, existing); err != nil {
			return false, fmt.Errorf("updating Certificate %s/%s: %w", cert.Namespace, cert.Name, err)
		}
		// Re-fetch to avoid evaluating stale status from before the spec
		// update.
		if err := c.Get(ctx, client.ObjectKeyFromObject(cert), existing); err != nil {
			return false, fmt.Errorf("re-fetching Certificate %s/%s after update: %w", cert.Namespace, cert.Name, err)
		}
	}

	return IsCertificateReady(existing), nil
}

// IsCertificateReady returns true if the Certificate has a Ready condition
// with status True.
func IsCertificateReady(cert *certmanagerv1.Certificate) bool {
	for _, cond := range cert.Status.Conditions {
		if cond.Type == certmanagerv1.CertificateConditionReady && cond.Status == cmmeta.ConditionTrue {
			return true
		}
	}
	return false
}

// GetTLSSecret retrieves the TLS certificate and private key from the Secret
// identified by key. It returns an error if the Secret is not found or is
// missing the expected tls.crt / tls.key entries.
func GetTLSSecret(ctx context.Context, c client.Client, key client.ObjectKey) (certPEM []byte, keyPEM []byte, err error) {
	secret := &corev1.Secret{}
	if err := c.Get(ctx, key, secret); err != nil {
		return nil, nil, fmt.Errorf("getting TLS Secret %s: %w", key, err)
	}
	certPEM, ok := secret.Data["tls.crt"]
	if !ok {
		return nil, nil, fmt.Errorf("TLS Secret %s is missing key %q", key, "tls.crt")
	}
	keyPEM, ok = secret.Data["tls.key"]
	if !ok {
		return nil, nil, fmt.Errorf("TLS Secret %s is missing key %q", key, "tls.key")
	}
	return certPEM, keyPEM, nil
}
