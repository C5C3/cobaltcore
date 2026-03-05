// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package tls

import (
	"context"
	"fmt"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// Feature: CC-0005

// EnsureCertificate creates or updates a cert-manager Certificate CR.
// It sets an owner reference on the Certificate so it is garbage-collected
// with the owner. The desired parameter must be a fully constructed
// *certmanagerv1.Certificate including ObjectMeta (Name, Namespace) and Spec.
func EnsureCertificate(ctx context.Context, c client.Client, owner client.Object, desired *certmanagerv1.Certificate) error {
	existing := &certmanagerv1.Certificate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      desired.Name,
			Namespace: desired.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, c, existing, func() error {
		existing.Spec = desired.Spec
		return controllerutil.SetControllerReference(owner, existing, c.Scheme())
	})
	if err != nil {
		return fmt.Errorf("creating or updating Certificate %s/%s: %w", desired.Namespace, desired.Name, err)
	}

	return nil
}

// GetTLSSecret retrieves the Kubernetes Secret that cert-manager populates
// with TLS certificate material. The name typically matches the Certificate's
// spec.secretName field.
func GetTLSSecret(ctx context.Context, c client.Client, namespace, name string) (*corev1.Secret, error) {
	secret := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, secret); err != nil {
		return nil, fmt.Errorf("getting TLS Secret %s/%s: %w", namespace, name, err)
	}
	return secret, nil
}
