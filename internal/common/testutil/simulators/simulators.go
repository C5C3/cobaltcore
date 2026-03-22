// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package simulators

import (
	"context"
	"fmt"
	"time"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	esov1beta1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1beta1"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
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

// SimulateMariaDBReady updates a MariaDB resource's status to indicate
// readiness by setting the Ready condition to True, replicas, and
// currentPrimaryPodIndex.
func SimulateMariaDBReady(ctx context.Context, c client.Client, key client.ObjectKey, replicas int) error {
	mariadb := &mariadbv1alpha1.MariaDB{}
	if err := c.Get(ctx, key, mariadb); err != nil {
		return fmt.Errorf("getting MariaDB %s: %w", key, err)
	}

	mariadb.Status.Replicas = int32(replicas)
	primaryIdx := 0
	mariadb.Status.CurrentPrimaryPodIndex = &primaryIdx

	meta.SetStatusCondition(&mariadb.Status.Conditions, metav1.Condition{
		Type:    conditionTypeReady,
		Status:  metav1.ConditionTrue,
		Reason:  "MariaDBReady",
		Message: "MariaDB is ready",
	})

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
		schema.GroupVersionKind{Group: "memcached.c5c3.io", Version: "v1beta1", Kind: "Memcached"},
		"MemcachedReady",
		"Memcached is ready",
		map[string]interface{}{
			"readyReplicas": int64(replicas),
			"serverList":    servers,
		},
	)
}

// SimulateExternalSecretSync updates an ExternalSecret resource's status to
// indicate successful synchronization by setting the Ready condition to True
// and updating the refresh time.
func SimulateExternalSecretSync(ctx context.Context, c client.Client, key client.ObjectKey) error {
	es := &esov1beta1.ExternalSecret{}
	if err := c.Get(ctx, key, es); err != nil {
		return fmt.Errorf("getting ExternalSecret %s: %w", key, err)
	}

	es.Status.RefreshTime = metav1.Now()
	es.Status.Conditions = []esov1beta1.ExternalSecretStatusCondition{
		{
			Type:               esov1beta1.ExternalSecretReady,
			Status:             corev1.ConditionTrue,
			Reason:             "SecretSynced",
			Message:            "Secret was synced",
			LastTransitionTime: metav1.Now(),
		},
	}

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
	startTime := metav1.NewTime(now.Add(-1 * time.Second))
	job.Status.StartTime = &startTime
	job.Status.CompletionTime = &now
	job.Status.Conditions = []batchv1.JobCondition{
		{
			Type:               batchv1.JobSuccessCriteriaMet,
			Status:             corev1.ConditionTrue,
			LastTransitionTime: now,
			Reason:             "Completed",
			Message:            "Job completed successfully",
		},
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

// Feature: CC-0005

// SimulateDatabaseReady updates a MariaDB Database resource's status to indicate
// readiness by setting the Ready condition to True (CC-0005).
func SimulateDatabaseReady(ctx context.Context, c client.Client, key client.ObjectKey) error {
	db := &mariadbv1alpha1.Database{}
	if err := c.Get(ctx, key, db); err != nil {
		return fmt.Errorf("getting Database %s: %w", key, err)
	}

	meta.SetStatusCondition(&db.Status.Conditions, metav1.Condition{
		Type:    "Ready",
		Status:  metav1.ConditionTrue,
		Reason:  "DatabaseReady",
		Message: "Database is ready",
	})

	return c.Status().Update(ctx, db)
}

// SimulateUserReady updates a MariaDB User resource's status to indicate
// readiness by setting the Ready condition to True (CC-0005).
func SimulateUserReady(ctx context.Context, c client.Client, key client.ObjectKey) error {
	user := &mariadbv1alpha1.User{}
	if err := c.Get(ctx, key, user); err != nil {
		return fmt.Errorf("getting User %s: %w", key, err)
	}

	meta.SetStatusCondition(&user.Status.Conditions, metav1.Condition{
		Type:    "Ready",
		Status:  metav1.ConditionTrue,
		Reason:  "UserReady",
		Message: "User is ready",
	})

	return c.Status().Update(ctx, user)
}

// SimulateGrantReady updates a MariaDB Grant resource's status to indicate
// readiness by setting the Ready condition to True (CC-0005).
func SimulateGrantReady(ctx context.Context, c client.Client, key client.ObjectKey) error {
	grant := &mariadbv1alpha1.Grant{}
	if err := c.Get(ctx, key, grant); err != nil {
		return fmt.Errorf("getting Grant %s: %w", key, err)
	}

	meta.SetStatusCondition(&grant.Status.Conditions, metav1.Condition{
		Type:    "Ready",
		Status:  metav1.ConditionTrue,
		Reason:  "GrantReady",
		Message: "Grant is ready",
	})

	return c.Status().Update(ctx, grant)
}

// SimulatePushSecretSynced updates a PushSecret resource's status to indicate
// successful synchronization (CC-0005).
func SimulatePushSecretSynced(ctx context.Context, c client.Client, key client.ObjectKey) error {
	ps := &esov1alpha1.PushSecret{}
	if err := c.Get(ctx, key, ps); err != nil {
		return fmt.Errorf("getting PushSecret %s: %w", key, err)
	}

	ps.Status.RefreshTime = metav1.Now()
	ps.Status.Conditions = []esov1alpha1.PushSecretStatusCondition{
		{
			Type:               esov1alpha1.PushSecretReady,
			Status:             corev1.ConditionTrue,
			Reason:             "PushSecretSynced",
			Message:            "PushSecret was synced",
			LastTransitionTime: metav1.Now(),
		},
	}

	return c.Status().Update(ctx, ps)
}

// SimulateCertificateReady updates a cert-manager Certificate resource's status
// to indicate readiness by setting the Ready condition to True (CC-0005).
func SimulateCertificateReady(ctx context.Context, c client.Client, key client.ObjectKey) error {
	cert := &certmanagerv1.Certificate{}
	if err := c.Get(ctx, key, cert); err != nil {
		return fmt.Errorf("getting Certificate %s: %w", key, err)
	}

	now := metav1.Now()
	cert.Status.Conditions = []certmanagerv1.CertificateCondition{
		{
			Type:               certmanagerv1.CertificateConditionReady,
			Status:             cmmeta.ConditionTrue,
			Reason:             "CertificateReady",
			Message:            "Certificate is ready",
			LastTransitionTime: &now,
		},
	}

	return c.Status().Update(ctx, cert)
}

// Feature: CC-0014

// SimulateDeploymentReady updates a Deployment resource's status to indicate
// availability by setting the Available condition to True, readyReplicas, and
// observedGeneration to match the Deployment's Generation (CC-0014, REQ-001).
func SimulateDeploymentReady(ctx context.Context, c client.Client, key client.ObjectKey, replicas int32) error {
	deploy := &appsv1.Deployment{}
	if err := c.Get(ctx, key, deploy); err != nil {
		return fmt.Errorf("getting Deployment %s: %w", key, err)
	}

	deploy.Status.ReadyReplicas = replicas
	deploy.Status.AvailableReplicas = replicas
	deploy.Status.Replicas = replicas
	deploy.Status.UpdatedReplicas = replicas
	deploy.Status.ObservedGeneration = deploy.Generation
	deploy.Status.Conditions = []appsv1.DeploymentCondition{
		{
			Type:               appsv1.DeploymentProgressing,
			Status:             corev1.ConditionTrue,
			Reason:             "NewReplicaSetAvailable",
			Message:            "ReplicaSet has successfully progressed",
			LastTransitionTime: metav1.Now(),
		},
		{
			Type:               appsv1.DeploymentAvailable,
			Status:             corev1.ConditionTrue,
			Reason:             "MinimumReplicasAvailable",
			Message:            "Deployment has minimum availability",
			LastTransitionTime: metav1.Now(),
		},
	}

	return c.Status().Update(ctx, deploy)
}
