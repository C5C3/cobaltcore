// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package simulators

import (
	"context"
	"fmt"
	"time"

	esov1beta1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1beta1"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
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

// Feature: CC-0005

// SimulateMariaDBReady updates a typed MariaDB resource's status to indicate
// readiness by setting the Ready condition to True, replicas, and primary index.
func SimulateMariaDBReady(ctx context.Context, c client.Client, key client.ObjectKey, replicas int) error {
	mariadb := &mariadbv1alpha1.MariaDB{}
	if err := c.Get(ctx, key, mariadb); err != nil {
		return fmt.Errorf("getting MariaDB %s: %w", key, err)
	}

	now := metav1.Now()
	meta.SetStatusCondition(&mariadb.Status.Conditions, metav1.Condition{
		Type:               conditionTypeReady,
		Status:             metav1.ConditionTrue,
		Reason:             "MariaDBReady",
		Message:            "MariaDB is ready",
		LastTransitionTime: now,
	})
	mariadb.Status.Replicas = int32(replicas)
	mariadb.Status.CurrentPrimaryPodIndex = ptr.To(0)

	return c.Status().Update(ctx, mariadb)
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

// SimulateExternalSecretSync updates a typed ExternalSecret resource's status
// to indicate successful synchronization by setting the Ready condition with
// reason SecretSynced and updating the refreshTime.
func SimulateExternalSecretSync(ctx context.Context, c client.Client, key client.ObjectKey) error {
	es := &esov1beta1.ExternalSecret{}
	if err := c.Get(ctx, key, es); err != nil {
		return fmt.Errorf("getting ExternalSecret %s: %w", key, err)
	}

	now := metav1.Now()

	// ESO uses its own ExternalSecretStatusCondition type (not metav1.Condition),
	// so we set the condition directly rather than using meta.SetStatusCondition.
	es.Status.Conditions = []esov1beta1.ExternalSecretStatusCondition{
		{
			Type:               esov1beta1.ExternalSecretReady,
			Status:             corev1.ConditionTrue,
			Reason:             esov1beta1.ConditionReasonSecretSynced,
			Message:            "Secret was synced",
			LastTransitionTime: now,
		},
	}
	es.Status.RefreshTime = now

	return c.Status().Update(ctx, es)
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
