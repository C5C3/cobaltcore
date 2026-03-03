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

// dbSyncBackoffLimit is the number of retries before a db-sync Job is considered failed.
const dbSyncBackoffLimit int32 = 4

// dbSyncTTLSeconds is the time-to-live (in seconds) for a completed db-sync Job
// before Kubernetes garbage-collects it.
const dbSyncTTLSeconds int32 = 300

// databaseGVK is the GroupVersionKind for the mariadb-operator Database CR.
var databaseGVK = schema.GroupVersionKind{
	Group:   "k8s.mariadb.com",
	Version: "v1alpha1",
	Kind:    "Database",
}

// userGVK is the GroupVersionKind for the mariadb-operator User CR.
var userGVK = schema.GroupVersionKind{
	Group:   "k8s.mariadb.com",
	Version: "v1alpha1",
	Kind:    "User",
}

// grantGVK is the GroupVersionKind for the mariadb-operator Grant CR.
var grantGVK = schema.GroupVersionKind{
	Group:   "k8s.mariadb.com",
	Version: "v1alpha1",
	Kind:    "Grant",
}

// EnsureDatabase creates a k8s.mariadb.com/v1alpha1 Database custom resource
// using an unstructured object. The Database's spec.mariaDbRef.name is set to
// mariadbRef, and spec.name is set to the given name parameter. Owner
// references are set from the variadic owners parameter.
//
// The operation is idempotent: if the Database already exists, no error is
// returned. (CC-0005)
func EnsureDatabase(ctx context.Context, c client.Client, name, namespace, mariadbRef string, owners ...metav1.OwnerReference) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(databaseGVK)
	obj.SetName(name)
	obj.SetNamespace(namespace)

	mariaDbRefMap := map[string]interface{}{
		"name": mariadbRef,
	}
	if err := unstructured.SetNestedField(obj.Object, mariaDbRefMap, "spec", "mariaDbRef"); err != nil {
		return fmt.Errorf("setting Database spec.mariaDbRef: %w", err)
	}

	if err := unstructured.SetNestedField(obj.Object, name, "spec", "name"); err != nil {
		return fmt.Errorf("setting Database spec.name: %w", err)
	}

	if len(owners) > 0 {
		obj.SetOwnerReferences(owners)
	}

	if err := c.Create(ctx, obj); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("creating Database %s/%s: %w", namespace, name, err)
	}
	return nil
}

// RunDBSyncJob creates a Kubernetes Job that runs a database sync/migration
// command. The Job is configured with a backoff limit of 4 retries, a TTL of
// 300 seconds after completion, and a restart policy of Never. Owner references
// are set from the variadic owners parameter.
//
// The operation is idempotent: if the Job already exists, no error is
// returned. (CC-0005)
func RunDBSyncJob(ctx context.Context, c client.Client, name, namespace, image string, command []string, volumeMounts []corev1.VolumeMount, volumes []corev1.Volume, owners ...metav1.OwnerReference) error {
	backoffLimit := dbSyncBackoffLimit
	ttl := dbSyncTTLSeconds

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			OwnerReferences: owners,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:         "db-sync",
							Image:        image,
							Command:      command,
							VolumeMounts: volumeMounts,
						},
					},
					Volumes:       volumes,
					RestartPolicy: corev1.RestartPolicyNever,
				},
			},
		},
	}

	if err := c.Create(ctx, job); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("creating db-sync Job %s/%s: %w", namespace, name, err)
	}
	return nil
}

// EnsureDatabaseUser creates a k8s.mariadb.com/v1alpha1 User custom resource
// and a corresponding Grant custom resource using unstructured objects. The User
// is configured with the given mariadbRef and password secret reference. The
// Grant gives the user ALL privileges scoped to the specified databaseName and
// all tables within it (databaseName.*).
//
// The operation is idempotent: if either resource already exists, no error is
// returned. (CC-0005)
func EnsureDatabaseUser(ctx context.Context, c client.Client, name, namespace, mariadbRef, passwordSecretName, passwordSecretKey, databaseName string, owners ...metav1.OwnerReference) error {
	if err := ensureUser(ctx, c, name, namespace, mariadbRef, passwordSecretName, passwordSecretKey, owners...); err != nil {
		return err
	}
	if err := ensureGrant(ctx, c, name, namespace, mariadbRef, databaseName, owners...); err != nil {
		return err
	}
	return nil
}

// ensureUser creates a k8s.mariadb.com/v1alpha1 User CR with the given
// mariadbRef and password secret reference. The operation is idempotent. (CC-0005)
func ensureUser(ctx context.Context, c client.Client, name, namespace, mariadbRef, passwordSecretName, passwordSecretKey string, owners ...metav1.OwnerReference) error {
	user := &unstructured.Unstructured{}
	user.SetGroupVersionKind(userGVK)
	user.SetName(name)
	user.SetNamespace(namespace)

	mariaDbRefMap := map[string]interface{}{
		"name": mariadbRef,
	}
	if err := unstructured.SetNestedField(user.Object, mariaDbRefMap, "spec", "mariaDbRef"); err != nil {
		return fmt.Errorf("setting User spec.mariaDbRef: %w", err)
	}

	if err := unstructured.SetNestedField(user.Object, name, "spec", "name"); err != nil {
		return fmt.Errorf("setting User spec.name: %w", err)
	}

	passwordSecretKeyRefMap := map[string]interface{}{
		"name": passwordSecretName,
		"key":  passwordSecretKey,
	}
	if err := unstructured.SetNestedField(user.Object, passwordSecretKeyRefMap, "spec", "passwordSecretKeyRef"); err != nil {
		return fmt.Errorf("setting User spec.passwordSecretKeyRef: %w", err)
	}

	if len(owners) > 0 {
		user.SetOwnerReferences(owners)
	}

	if err := c.Create(ctx, user); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("creating User %s/%s: %w", namespace, name, err)
	}
	return nil
}

// ensureGrant creates a k8s.mariadb.com/v1alpha1 Grant CR that gives the user
// ALL privileges scoped to the specified database and all tables within it.
// The operation is idempotent. (CC-0005)
func ensureGrant(ctx context.Context, c client.Client, name, namespace, mariadbRef, databaseName string, owners ...metav1.OwnerReference) error {
	grant := &unstructured.Unstructured{}
	grant.SetGroupVersionKind(grantGVK)
	grant.SetName(name + "-grant")
	grant.SetNamespace(namespace)

	grantMariaDbRefMap := map[string]interface{}{
		"name": mariadbRef,
	}
	if err := unstructured.SetNestedField(grant.Object, grantMariaDbRefMap, "spec", "mariaDbRef"); err != nil {
		return fmt.Errorf("setting Grant spec.mariaDbRef: %w", err)
	}

	if err := unstructured.SetNestedField(grant.Object, databaseName, "spec", "database"); err != nil {
		return fmt.Errorf("setting Grant spec.database: %w", err)
	}

	if err := unstructured.SetNestedField(grant.Object, "*", "spec", "table"); err != nil {
		return fmt.Errorf("setting Grant spec.table: %w", err)
	}

	if err := unstructured.SetNestedField(grant.Object, name, "spec", "username"); err != nil {
		return fmt.Errorf("setting Grant spec.username: %w", err)
	}

	privileges := []interface{}{"ALL"}
	if err := unstructured.SetNestedSlice(grant.Object, privileges, "spec", "privileges"); err != nil {
		return fmt.Errorf("setting Grant spec.privileges: %w", err)
	}

	if len(owners) > 0 {
		grant.SetOwnerReferences(owners)
	}

	if err := c.Create(ctx, grant); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("creating Grant %s/%s-grant: %w", namespace, name, err)
	}
	return nil
}
