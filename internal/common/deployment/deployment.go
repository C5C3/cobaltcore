package deployment

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const fieldManager = "cobaltcore-operator"

// IsDeploymentReady returns true when the given Deployment has an Available
// condition with status True and the number of ready replicas is at least equal
// to the desired replica count from the spec. It is a pure function that
// inspects the in-memory object without making any API calls. (CC-0005 / REQ-005)
func IsDeploymentReady(deployment *appsv1.Deployment) bool {
	if deployment == nil {
		return false
	}
	if deployment.Spec.Replicas == nil {
		return false
	}

	desired := *deployment.Spec.Replicas
	if deployment.Status.ReadyReplicas < desired {
		return false
	}

	for _, c := range deployment.Status.Conditions {
		if c.Type == appsv1.DeploymentAvailable && c.Status == corev1.ConditionTrue {
			return true
		}
	}

	return false
}

// toUnstructuredApply converts a typed K8s object to an unstructured apply
// configuration for use with client.Apply().
func toUnstructuredApply(obj k8sruntime.Object) (k8sruntime.ApplyConfiguration, error) {
	data, err := k8sruntime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return nil, fmt.Errorf("converting to unstructured: %w", err)
	}
	u := &unstructured.Unstructured{Object: data}
	return client.ApplyConfigurationFromUnstructured(u), nil
}

// EnsureDeployment applies a Deployment via server-side apply with field manager.
// Sets owner references for garbage collection. (CC-0005 / REQ-005)
func EnsureDeployment(ctx context.Context, c client.Client, owner client.Object, scheme *k8sruntime.Scheme, deployment *appsv1.Deployment) error {
	if err := controllerutil.SetControllerReference(owner, deployment, scheme); err != nil {
		return fmt.Errorf("setting controller reference on Deployment %s/%s: %w", deployment.Namespace, deployment.Name, err)
	}

	// Server-side apply requires TypeMeta to be set.
	deployment.APIVersion = "apps/v1"
	deployment.Kind = "Deployment"

	ac, err := toUnstructuredApply(deployment)
	if err != nil {
		return fmt.Errorf("preparing Deployment %s/%s for apply: %w", deployment.Namespace, deployment.Name, err)
	}

	force := true
	if err := c.Apply(ctx, ac, &client.ApplyOptions{FieldManager: fieldManager, Force: &force}); err != nil {
		return fmt.Errorf("applying Deployment %s/%s: %w", deployment.Namespace, deployment.Name, err)
	}

	return nil
}

// EnsureService applies a Service via server-side apply with field manager.
// Sets owner references for garbage collection. (CC-0005 / REQ-005)
func EnsureService(ctx context.Context, c client.Client, owner client.Object, scheme *k8sruntime.Scheme, service *corev1.Service) error {
	if err := controllerutil.SetControllerReference(owner, service, scheme); err != nil {
		return fmt.Errorf("setting controller reference on Service %s/%s: %w", service.Namespace, service.Name, err)
	}

	service.APIVersion = "v1"
	service.Kind = "Service"

	ac, err := toUnstructuredApply(service)
	if err != nil {
		return fmt.Errorf("preparing Service %s/%s for apply: %w", service.Namespace, service.Name, err)
	}

	force := true
	if err := c.Apply(ctx, ac, &client.ApplyOptions{FieldManager: fieldManager, Force: &force}); err != nil {
		return fmt.Errorf("applying Service %s/%s: %w", service.Namespace, service.Name, err)
	}

	return nil
}
