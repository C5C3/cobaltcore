package deployment

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// EnsureDeployment creates the given Deployment if it does not exist, or updates
// it if it already exists. The caller supplies a fully-constructed Deployment
// object. On update the ResourceVersion from the existing object is preserved
// for optimistic concurrency. (CC-0005, REQ-009)
func EnsureDeployment(ctx context.Context, c client.Client, deployment *appsv1.Deployment) error {
	existing := &appsv1.Deployment{}
	err := c.Get(ctx, client.ObjectKeyFromObject(deployment), existing)
	if err != nil {
		if apierrors.IsNotFound(err) {
			if createErr := c.Create(ctx, deployment); createErr != nil {
				return fmt.Errorf("creating Deployment %s/%s: %w", deployment.Namespace, deployment.Name, createErr)
			}
			return nil
		}
		return fmt.Errorf("getting Deployment %s/%s: %w", deployment.Namespace, deployment.Name, err)
	}
	// Update existing — preserve ResourceVersion for optimistic concurrency.
	deployment.ResourceVersion = existing.ResourceVersion
	if err := c.Update(ctx, deployment); err != nil {
		return fmt.Errorf("updating Deployment %s/%s: %w", deployment.Namespace, deployment.Name, err)
	}
	return nil
}

// EnsureService creates the given Service if it does not exist, or updates it
// if it already exists. The caller supplies a fully-constructed Service object.
// On update the ResourceVersion from the existing object is preserved for
// optimistic concurrency. If the caller specifies a non-empty ClusterIP that
// differs from the existing one, the update is rejected with an error because
// ClusterIP is immutable once assigned by Kubernetes. If the caller's ClusterIP
// is empty, the existing ClusterIP is preserved. (CC-0005, REQ-010)
func EnsureService(ctx context.Context, c client.Client, service *corev1.Service) error {
	existing := &corev1.Service{}
	err := c.Get(ctx, client.ObjectKeyFromObject(service), existing)
	if err != nil {
		if apierrors.IsNotFound(err) {
			if createErr := c.Create(ctx, service); createErr != nil {
				return fmt.Errorf("creating Service %s/%s: %w", service.Namespace, service.Name, createErr)
			}
			return nil
		}
		return fmt.Errorf("getting Service %s/%s: %w", service.Namespace, service.Name, err)
	}
	// Reject updates that specify a different ClusterIP — it is immutable once
	// assigned by Kubernetes, so silently overwriting it would cause the update
	// to appear successful while the actual configuration is not applied.
	if service.Spec.ClusterIP != "" && service.Spec.ClusterIP != existing.Spec.ClusterIP {
		return fmt.Errorf("service %s/%s ClusterIP cannot be changed: existing=%q, requested=%q",
			service.Namespace, service.Name, existing.Spec.ClusterIP, service.Spec.ClusterIP)
	}
	// Update existing — preserve ResourceVersion for optimistic concurrency
	// and ClusterIP which is immutable once assigned.
	service.ResourceVersion = existing.ResourceVersion
	service.Spec.ClusterIP = existing.Spec.ClusterIP
	if err := c.Update(ctx, service); err != nil {
		return fmt.Errorf("updating Service %s/%s: %w", service.Namespace, service.Name, err)
	}
	return nil
}

// IsDeploymentReady returns true if the given Deployment has an "Available"
// condition with status "True". This indicates that the Deployment has reached
// its minimum availability as defined by its deployment strategy. (CC-0005, REQ-005)
func IsDeploymentReady(deployment *appsv1.Deployment) bool {
	if deployment == nil {
		return false
	}
	for _, c := range deployment.Status.Conditions {
		if c.Type == appsv1.DeploymentAvailable && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}
