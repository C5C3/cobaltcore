// Package applyconfig provides shared helpers for Kubernetes server-side apply
// operations. It centralises the conversion of typed Kubernetes objects into
// runtime.ApplyConfiguration values so that all resource packages (deployment,
// job, etc.) use consistent SSA behaviour. (CC-0005)
package applyconfig

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// DefaultFieldManager is the recommended server-side apply field manager name
// for controllers using this module. Callers may use their own value if they
// need controller-specific ownership tracking. (CC-0005)
const DefaultFieldManager = "cobaltcore-operator"

// ToApplyConfiguration converts a typed Kubernetes object into a
// runtime.ApplyConfiguration suitable for client.Client.Apply().
// The object must have its GVK set before calling this function. (CC-0005)
func ToApplyConfiguration(obj k8sruntime.Object) (k8sruntime.ApplyConfiguration, error) {
	data, err := k8sruntime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return nil, err
	}
	return client.ApplyConfigurationFromUnstructured(&unstructured.Unstructured{Object: data}), nil
}
