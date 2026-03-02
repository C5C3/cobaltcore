package database

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// grantAllTables is the MariaDB wildcard value meaning "all tables" in a Grant CR.
const grantAllTables = "*"

// mariadbGVK returns the GVK for a given MariaDB operator kind.
func mariadbGVK(kind string) schema.GroupVersionKind {
	return schema.GroupVersionKind{
		Group:   "k8s.mariadb.com",
		Version: "v1alpha1",
		Kind:    kind,
	}
}

// EnsureDatabase creates a MariaDB Database CR (k8s.mariadb.com/v1alpha1) via the
// unstructured client. The function is idempotent -- if the Database already exists,
// it returns nil. (CC-0005, REQ-004, REQ-009, REQ-010)
func EnsureDatabase(ctx context.Context, c client.Client, name, namespace, databaseName, mariadbRefName string, ownerRefs ...metav1.OwnerReference) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(mariadbGVK("Database"))
	obj.SetName(name)
	obj.SetNamespace(namespace)

	if len(ownerRefs) > 0 {
		obj.SetOwnerReferences(ownerRefs)
	}

	mariaDbRef := map[string]interface{}{
		"name": mariadbRefName,
	}
	if err := unstructured.SetNestedField(obj.Object, mariaDbRef, "spec", "mariaDbRef"); err != nil {
		return fmt.Errorf("setting Database spec.mariaDbRef: %w", err)
	}

	if err := unstructured.SetNestedField(obj.Object, databaseName, "spec", "name"); err != nil {
		return fmt.Errorf("setting Database spec.name: %w", err)
	}

	err := c.Create(ctx, obj)
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating Database: %w", err)
	}
	return nil
}

// EnsureDatabaseUser creates a MariaDB User CR and a Grant CR
// (k8s.mariadb.com/v1alpha1) via the unstructured client. Both operations are
// idempotent -- if the resources already exist, the function returns nil.
// (CC-0005, REQ-004, REQ-009)
func EnsureDatabaseUser(ctx context.Context, c client.Client, name, namespace, mariadbRefName, passwordSecretName, passwordSecretKey string, databaseName string, privileges []string, ownerRefs ...metav1.OwnerReference) error {
	if err := ensureUser(ctx, c, name, namespace, mariadbRefName, passwordSecretName, passwordSecretKey, ownerRefs); err != nil {
		return err
	}
	return ensureGrant(ctx, c, name, namespace, mariadbRefName, databaseName, privileges, ownerRefs)
}

// ensureUser creates a MariaDB User CR. Idempotent — returns nil if it already exists.
func ensureUser(ctx context.Context, c client.Client, name, namespace, mariadbRefName, passwordSecretName, passwordSecretKey string, ownerRefs []metav1.OwnerReference) error {
	userObj := &unstructured.Unstructured{}
	userObj.SetGroupVersionKind(mariadbGVK("User"))
	userObj.SetName(name)
	userObj.SetNamespace(namespace)

	if len(ownerRefs) > 0 {
		userObj.SetOwnerReferences(ownerRefs)
	}

	mariaDbRef := map[string]interface{}{
		"name": mariadbRefName,
	}
	if err := unstructured.SetNestedField(userObj.Object, mariaDbRef, "spec", "mariaDbRef"); err != nil {
		return fmt.Errorf("setting User spec.mariaDbRef: %w", err)
	}

	passwordSecretKeyRef := map[string]interface{}{
		"name": passwordSecretName,
		"key":  passwordSecretKey,
	}
	if err := unstructured.SetNestedField(userObj.Object, passwordSecretKeyRef, "spec", "passwordSecretKeyRef"); err != nil {
		return fmt.Errorf("setting User spec.passwordSecretKeyRef: %w", err)
	}

	if err := unstructured.SetNestedField(userObj.Object, name, "spec", "name"); err != nil {
		return fmt.Errorf("setting User spec.name: %w", err)
	}

	err := c.Create(ctx, userObj)
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating User: %w", err)
	}
	return nil
}

// ensureGrant creates a MariaDB Grant CR. Idempotent — returns nil if it already exists.
func ensureGrant(ctx context.Context, c client.Client, userName, namespace, mariadbRefName, databaseName string, privileges []string, ownerRefs []metav1.OwnerReference) error {
	grantObj := &unstructured.Unstructured{}
	grantObj.SetGroupVersionKind(mariadbGVK("Grant"))
	grantObj.SetName(userName + "-grant")
	grantObj.SetNamespace(namespace)

	if len(ownerRefs) > 0 {
		grantObj.SetOwnerReferences(ownerRefs)
	}

	grantMariaDbRef := map[string]interface{}{
		"name": mariadbRefName,
	}
	if err := unstructured.SetNestedField(grantObj.Object, grantMariaDbRef, "spec", "mariaDbRef"); err != nil {
		return fmt.Errorf("setting Grant spec.mariaDbRef: %w", err)
	}

	if err := unstructured.SetNestedField(grantObj.Object, databaseName, "spec", "database"); err != nil {
		return fmt.Errorf("setting Grant spec.database: %w", err)
	}

	if err := unstructured.SetNestedField(grantObj.Object, grantAllTables, "spec", "table"); err != nil {
		return fmt.Errorf("setting Grant spec.table: %w", err)
	}

	if err := unstructured.SetNestedField(grantObj.Object, userName, "spec", "username"); err != nil {
		return fmt.Errorf("setting Grant spec.username: %w", err)
	}

	privsIface := make([]interface{}, len(privileges))
	for i, p := range privileges {
		privsIface[i] = p
	}
	if err := unstructured.SetNestedSlice(grantObj.Object, privsIface, "spec", "privileges"); err != nil {
		return fmt.Errorf("setting Grant spec.privileges: %w", err)
	}

	err := c.Create(ctx, grantObj)
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating Grant: %w", err)
	}
	return nil
}

// RunDBSyncJob creates a batch/v1 Job for database schema migration. The function
// is idempotent -- if the Job already exists, it returns nil.
// (CC-0005, REQ-004, REQ-009)
func RunDBSyncJob(ctx context.Context, c client.Client, name, namespace, image string, command []string, env []corev1.EnvVar, ownerRefs ...metav1.OwnerReference) error {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:    "db-sync",
							Image:   image,
							Command: command,
							Env:     env,
						},
					},
					RestartPolicy: corev1.RestartPolicyOnFailure,
				},
			},
		},
	}

	if len(ownerRefs) > 0 {
		job.SetOwnerReferences(ownerRefs)
	}

	err := c.Create(ctx, job)
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating db-sync Job: %w", err)
	}
	return nil
}
