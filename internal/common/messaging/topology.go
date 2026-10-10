// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package messaging

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// The kinds of the RabbitMQ Messaging Topology Operator a RabbitMQVhost order
// is provisioned through. They are addressed unstructured for the reason
// RabbitmqClusterGVK gives: this repository takes no dependency on the
// operator's Go module, so the kinds need no scheme registration.
var (
	// VhostGVK is the topology operator's Vhost: one vhost on the broker its
	// rabbitmqClusterReference names.
	VhostGVK = schema.GroupVersionKind{Group: "rabbitmq.com", Version: "v1beta1", Kind: "Vhost"}
	// UserGVK is the topology operator's User: one broker user.
	UserGVK = schema.GroupVersionKind{Group: "rabbitmq.com", Version: "v1beta1", Kind: "User"}
	// PermissionGVK is the topology operator's Permission: one user's
	// permissions on one vhost.
	PermissionGVK = schema.GroupVersionKind{Group: "rabbitmq.com", Version: "v1beta1", Kind: "Permission"}
)

// TopologyOperatorLabel is the label every Secret the topology operator reads
// must carry, with the value "true": its Secret informer caches only labelled
// Secrets, and its admission webhook refuses a User whose
// importCredentialsSecret lacks the label.
const TopologyOperatorLabel = "rabbitmq.com/topology-operator"

// The condition the topology operator reports on every object it reconciles,
// and the reason of a failed create or update.
const (
	topologyConditionReady    = "Ready"
	topologyReasonFailedApply = "FailedCreateOrUpdate"
)

// topologyObject returns an unstructured object of gvk named name in namespace
// with spec.
func topologyObject(gvk schema.GroupVersionKind, name, namespace string, spec map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	u.SetGroupVersionKind(gvk)
	u.SetName(name)
	u.SetNamespace(namespace)
	return u
}

// clusterReference is the rabbitmqClusterReference of an object on the
// RabbitmqCluster clusterName in the object's own namespace.
func clusterReference(clusterName string) map[string]any {
	return map[string]any{"name": clusterName}
}

// BuildVhost returns the Vhost name in namespace that creates the vhost
// vhostName on the RabbitmqCluster clusterName. deletionPolicy is the
// operator's own spelling, "retain" or "delete": it decides whether deleting
// the Vhost deletes the vhost on the broker.
func BuildVhost(name, namespace, vhostName, clusterName, deletionPolicy string) *unstructured.Unstructured {
	return topologyObject(VhostGVK, name, namespace, map[string]any{
		"name":                     vhostName,
		"deletionPolicy":           deletionPolicy,
		"rabbitmqClusterReference": clusterReference(clusterName),
	})
}

// BuildUser returns the User name in namespace that creates a user on the
// RabbitmqCluster clusterName from the username and password keys of the
// Secret credentialsSecret in the same namespace. The Secret must carry
// TopologyOperatorLabel. The user has no tags.
func BuildUser(name, namespace, clusterName, credentialsSecret string) *unstructured.Unstructured {
	return topologyObject(UserGVK, name, namespace, map[string]any{
		"importCredentialsSecret":  map[string]any{"name": credentialsSecret},
		"rabbitmqClusterReference": clusterReference(clusterName),
	})
}

// BuildPermission returns the Permission name in namespace that grants the
// broker user username configure, write and read permissions of ".*" on the
// vhost vhostName of the RabbitmqCluster clusterName.
func BuildPermission(name, namespace, clusterName, vhostName, username string) *unstructured.Unstructured {
	return topologyObject(PermissionGVK, name, namespace, map[string]any{
		"vhost": vhostName,
		"user":  username,
		"permissions": map[string]any{
			"configure": ".*",
			"write":     ".*",
			"read":      ".*",
		},
		"rabbitmqClusterReference": clusterReference(clusterName),
	})
}

// topologyReadyCondition returns the Ready condition of a topology object, or
// nil when its status carries none.
func topologyReadyCondition(u *unstructured.Unstructured) map[string]any {
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conds {
		cond, ok := c.(map[string]any)
		if ok && cond["type"] == topologyConditionReady {
			return cond
		}
	}
	return nil
}

// TopologyReady reports whether the topology operator has reconciled the
// object's current generation and reports it Ready: status.observedGeneration
// equals metadata.generation and the Ready condition is True. A missing status
// is not ready.
func TopologyReady(u *unstructured.Unstructured) bool {
	observed, found, err := unstructured.NestedInt64(u.Object, "status", "observedGeneration")
	if err != nil || !found || observed != u.GetGeneration() {
		return false
	}
	cond := topologyReadyCondition(u)
	return cond != nil && cond["status"] == "True"
}

// TopologyFailure returns the message of a Ready condition the topology
// operator set False with the reason FailedCreateOrUpdate, the shape of a
// broker that refused the object, or "" when the object reports no such
// failure. A failure without a message reads as the reason, so it is never
// mistaken for none.
func TopologyFailure(u *unstructured.Unstructured) string {
	cond := topologyReadyCondition(u)
	if cond == nil || cond["status"] != "False" || cond["reason"] != topologyReasonFailedApply {
		return ""
	}
	if message, _ := cond["message"].(string); message != "" {
		return message
	}
	return topologyReasonFailedApply
}
