package deployment

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

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
