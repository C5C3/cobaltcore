package deployment

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/forge/internal/common/applyconfig"
)

// IsDeploymentReady returns true if the given Deployment has reached its desired
// state: all replicas are updated, available, and ready, with no unavailable
// replicas. This is a pure function that inspects the Deployment's status fields
// without making any Kubernetes API calls. (CC-0005, REQ-005)
func IsDeploymentReady(deploy *appsv1.Deployment) bool {
	if deploy == nil {
		return false
	}

	// Kubernetes defaults Spec.Replicas to 1 when nil.
	var desired int32 = 1
	if deploy.Spec.Replicas != nil {
		desired = *deploy.Spec.Replicas
	}

	return deploy.Status.ObservedGeneration >= deploy.Generation &&
		deploy.Status.UpdatedReplicas == desired &&
		deploy.Status.ReadyReplicas == desired &&
		deploy.Status.AvailableReplicas == desired &&
		deploy.Status.UnavailableReplicas == 0
}

// EnsureDeployment uses server-side apply to create or update the given
// Deployment. If ownerRefs are provided they are set on the object before
// applying. (CC-0005, REQ-005, REQ-009, REQ-010)
func EnsureDeployment(ctx context.Context, c client.Client, deploy *appsv1.Deployment, fieldManager string, ownerRefs ...metav1.OwnerReference) error {
	if len(ownerRefs) > 0 {
		deploy.SetOwnerReferences(ownerRefs)
	}

	deploy.SetGroupVersionKind(appsv1.SchemeGroupVersion.WithKind("Deployment"))

	applyConf, err := applyconfig.ToApplyConfiguration(deploy)
	if err != nil {
		return fmt.Errorf("converting Deployment to apply configuration: %w", err)
	}

	if err := c.Apply(ctx, applyConf, client.FieldOwner(fieldManager), client.ForceOwnership); err != nil {
		return fmt.Errorf("applying Deployment %s/%s: %w", deploy.Namespace, deploy.Name, err)
	}
	return nil
}

// EnsureService uses server-side apply to create or update the given Service.
// If ownerRefs are provided they are set on the object before applying.
// (CC-0005, REQ-005, REQ-009, REQ-010)
func EnsureService(ctx context.Context, c client.Client, svc *corev1.Service, fieldManager string, ownerRefs ...metav1.OwnerReference) error {
	if len(ownerRefs) > 0 {
		svc.SetOwnerReferences(ownerRefs)
	}

	svc.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Service"))

	applyConf, err := applyconfig.ToApplyConfiguration(svc)
	if err != nil {
		return fmt.Errorf("converting Service to apply configuration: %w", err)
	}

	if err := c.Apply(ctx, applyConf, client.FieldOwner(fieldManager), client.ForceOwnership); err != nil {
		return fmt.Errorf("applying Service %s/%s: %w", svc.Namespace, svc.Name, err)
	}
	return nil
}
