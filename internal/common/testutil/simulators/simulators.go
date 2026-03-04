// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package simulators

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Feature: CC-0002

// Condition field constants shared across unstructured simulators.
const (
	conditionTypeReady   = "Ready"
	conditionStatusTrue  = "True"
)

// setUnstructuredReadyStatus updates an unstructured resource's status with a
// Ready condition set to True and any additional status fields. It handles the
// common Get → build status → SetNestedField → Status().Update() pattern shared
// by all unstructured simulators.
func setUnstructuredReadyStatus(
	ctx context.Context,
	c client.Client,
	key client.ObjectKey,
	gvk schema.GroupVersionKind,
	reason string,
	message string,
	extraFields map[string]interface{},
) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)

	if err := c.Get(ctx, key, obj); err != nil {
		return fmt.Errorf("getting %s %s: %w", gvk.Kind, key, err)
	}

	status := map[string]interface{}{
		"conditions": []interface{}{
			map[string]interface{}{
				"type":               conditionTypeReady,
				"status":             conditionStatusTrue,
				"reason":             reason,
				"message":            message,
				"lastTransitionTime": metav1.Now().Format(time.RFC3339),
			},
		},
	}
	for k, v := range extraFields {
		status[k] = v
	}

	if err := unstructured.SetNestedField(obj.Object, status, "status"); err != nil {
		return fmt.Errorf("setting %s status: %w", gvk.Kind, err)
	}

	return c.Status().Update(ctx, obj)
}

// SimulateMariaDBReady updates an unstructured MariaDB resource's status to
// indicate readiness by setting the Ready condition to True and readyReplicas.
func SimulateMariaDBReady(ctx context.Context, c client.Client, key client.ObjectKey, replicas int) error {
	return setUnstructuredReadyStatus(ctx, c, key,
		schema.GroupVersionKind{Group: "k8s.mariadb.com", Version: "v1alpha1", Kind: "MariaDB"},
		"MariaDBReady",
		"MariaDB is ready",
		map[string]interface{}{
			"readyReplicas":          int64(replicas),
			"currentPrimaryPodIndex": int64(0),
		},
	)
}

// SimulateMemcachedReady updates an unstructured Memcached resource's status to
// indicate readiness by setting the Ready condition to True, readyReplicas,
// and serverList.
func SimulateMemcachedReady(ctx context.Context, c client.Client, key client.ObjectKey, replicas int, serverList []string) error {
	servers := make([]interface{}, len(serverList))
	for i, s := range serverList {
		servers[i] = s
	}

	return setUnstructuredReadyStatus(ctx, c, key,
		schema.GroupVersionKind{Group: "cache.c5c3.io", Version: "v1alpha1", Kind: "Memcached"},
		"MemcachedReady",
		"Memcached is ready",
		map[string]interface{}{
			"readyReplicas": int64(replicas),
			"serverList":    servers,
		},
	)
}

// SimulateExternalSecretSync updates an unstructured ExternalSecret resource's
// status to indicate successful synchronization.
func SimulateExternalSecretSync(ctx context.Context, c client.Client, key client.ObjectKey) error {
	return setUnstructuredReadyStatus(ctx, c, key,
		schema.GroupVersionKind{Group: "external-secrets.io", Version: "v1beta1", Kind: "ExternalSecret"},
		"SecretSynced",
		"Secret was synced",
		map[string]interface{}{
			"refreshTime": metav1.Now().Format(time.RFC3339),
		},
	)
}

// SimulateJobComplete updates a Job resource's status to indicate successful
// completion.
func SimulateJobComplete(ctx context.Context, c client.Client, key client.ObjectKey) error {
	job := &batchv1.Job{}
	if err := c.Get(ctx, key, job); err != nil {
		return fmt.Errorf("getting Job %s: %w", key, err)
	}

	job.Status.Succeeded = 1
	now := metav1.Now()
	job.Status.CompletionTime = &now
	job.Status.Conditions = []batchv1.JobCondition{
		{
			Type:               batchv1.JobComplete,
			Status:             corev1.ConditionTrue,
			LastTransitionTime: now,
			Reason:             "Completed",
			Message:            "Job completed successfully",
		},
	}

	return c.Status().Update(ctx, job)
}
