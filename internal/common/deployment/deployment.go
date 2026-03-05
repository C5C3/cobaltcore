// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package deployment

// Feature: CC-0005

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// EnsureDeployment creates or updates a Deployment using controllerutil.CreateOrUpdate.
// It sets an owner reference on the Deployment so it is garbage-collected with the owner.
// The spec parameter is the desired Deployment that callers prepare.
// Returns (true, nil) when the Deployment is ready (all replicas available),
// (false, nil) when not yet ready, or (false, err) on failure.
func EnsureDeployment(ctx context.Context, c client.Client, owner client.Object, spec *appsv1.Deployment) (bool, error) {
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      spec.Name,
			Namespace: spec.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, c, deploy, func() error {
		deploy.Labels = spec.Labels
		deploy.Spec = spec.Spec
		return controllerutil.SetControllerReference(owner, deploy, c.Scheme())
	})
	if err != nil {
		return false, fmt.Errorf("creating or updating Deployment %s/%s: %w", spec.Namespace, spec.Name, err)
	}

	// Re-fetch to get current status after create/update.
	if err := c.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		return false, fmt.Errorf("fetching Deployment status %s/%s: %w", spec.Namespace, spec.Name, err)
	}

	return IsDeploymentReady(deploy), nil
}

// EnsureService creates or updates a ClusterIP Service using controllerutil.CreateOrUpdate.
// It sets an owner reference on the Service so it is garbage-collected with the owner.
// The spec parameter is the desired Service that callers prepare.
// ClusterIP and ClusterIPs fields are preserved across updates because they are
// assigned by the API server and must not be overwritten.
func EnsureService(ctx context.Context, c client.Client, owner client.Object, spec *corev1.Service) error {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      spec.Name,
			Namespace: spec.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, c, svc, func() error {
		svc.Labels = spec.Labels
		svc.Spec.Selector = spec.Spec.Selector
		svc.Spec.Ports = spec.Spec.Ports
		svc.Spec.Type = spec.Spec.Type
		// NOTE: Do NOT overwrite Spec.ClusterIP or Spec.ClusterIPs.
		// These are assigned by the API server and must be preserved.
		return controllerutil.SetControllerReference(owner, svc, c.Scheme())
	})
	if err != nil {
		return fmt.Errorf("creating or updating Service %s/%s: %w", spec.Namespace, spec.Name, err)
	}

	return nil
}

// IsDeploymentReady checks whether all desired replicas of a Deployment are
// available, ready, and updated. It handles a nil Spec.Replicas by defaulting
// to 1, which is the Kubernetes API server default.
func IsDeploymentReady(deployment *appsv1.Deployment) bool {
	desired := int32(1)
	if deployment.Spec.Replicas != nil {
		desired = *deployment.Spec.Replicas
	}

	return deployment.Status.AvailableReplicas >= desired &&
		deployment.Status.ReadyReplicas >= desired &&
		deployment.Status.UpdatedReplicas >= desired
}
