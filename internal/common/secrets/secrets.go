// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package secrets

// Feature: CC-0005

import (
	"context"
	"fmt"

	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	esov1beta1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1beta1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// WaitForExternalSecret checks whether an ESO ExternalSecret has synced by
// inspecting its Ready condition. Returns (true, nil) when the ExternalSecret
// is ready, (false, nil) when it does not exist or is not yet ready, and
// (false, error) on unexpected failures.
func WaitForExternalSecret(ctx context.Context, c client.Client, namespace, name string) (bool, error) {
	es := &esov1beta1.ExternalSecret{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, es); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("getting ExternalSecret %s/%s: %w", namespace, name, err)
	}

	for _, cond := range es.Status.Conditions {
		if cond.Type == esov1beta1.ExternalSecretReady && cond.Status == corev1.ConditionTrue {
			return true, nil
		}
	}
	return false, nil
}

// IsSecretReady verifies that a Kubernetes Secret exists. Returns (true, nil)
// if the Secret exists, (false, nil) if it is not found, and (false, error) on
// other failures.
func IsSecretReady(ctx context.Context, c client.Client, namespace, name string) (bool, error) {
	secret := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("getting Secret %s/%s: %w", namespace, name, err)
	}
	return true, nil
}

// GetSecretValue reads a specific key from a Kubernetes Secret and returns its
// string value. Returns an error if the Secret is not found or if the key does
// not exist in the Secret's data.
func GetSecretValue(ctx context.Context, c client.Client, namespace, name, key string) (string, error) {
	secret := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, secret); err != nil {
		return "", fmt.Errorf("getting Secret %s/%s: %w", namespace, name, err)
	}
	val, ok := secret.Data[key]
	if !ok {
		return "", fmt.Errorf("key %q not found in Secret %s/%s", key, namespace, name)
	}
	return string(val), nil
}

// EnsurePushSecret creates or updates an ESO PushSecret CR. The owner is set
// as the controller reference so that the PushSecret is garbage-collected when
// the owner is deleted.
func EnsurePushSecret(ctx context.Context, c client.Client, owner client.Object, desired *esov1alpha1.PushSecret) error {
	existing := &esov1alpha1.PushSecret{}
	existing.Name = desired.Name
	existing.Namespace = desired.Namespace

	_, err := controllerutil.CreateOrUpdate(ctx, c, existing, func() error {
		if err := controllerutil.SetControllerReference(owner, existing, c.Scheme()); err != nil {
			return fmt.Errorf("setting controller reference: %w", err)
		}
		existing.Spec = desired.Spec
		return nil
	})
	if err != nil {
		return fmt.Errorf("creating or updating PushSecret %s/%s: %w", desired.Namespace, desired.Name, err)
	}
	return nil
}
