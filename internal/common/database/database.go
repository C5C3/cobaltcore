package database

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// DatabaseOpts configures the MariaDB Database CR to create.
type DatabaseOpts struct {
	Name         string
	Namespace    string
	MariaDBRef   string // name of the MariaDB CR
	DatabaseName string // the actual database name
}

// DatabaseUserOpts configures the MariaDB User and Grant CRs to create.
type DatabaseUserOpts struct {
	Name               string
	Namespace          string
	MariaDBRef         string
	Username           string
	PasswordSecretName string
	PasswordSecretKey  string
	DatabaseName       string
	Privileges         []string
}

// DBSyncJobOpts configures the database schema migration Job.
type DBSyncJobOpts struct {
	Name      string
	Namespace string
	Image     string // "repository:tag" format
	Command   []string
	Env       []corev1.EnvVar
}

var databaseGVK = schema.GroupVersionKind{
	Group: "k8s.mariadb.com", Version: "v1alpha1", Kind: "Database",
}

var userGVK = schema.GroupVersionKind{
	Group: "k8s.mariadb.com", Version: "v1alpha1", Kind: "User",
}

var grantGVK = schema.GroupVersionKind{
	Group: "k8s.mariadb.com", Version: "v1alpha1", Kind: "Grant",
}

// EnsureDatabase creates or updates a MariaDB Database CR.
// Uses unstructured client. (CC-0005 / REQ-004)
func EnsureDatabase(ctx context.Context, c client.Client, owner client.Object, scheme *runtime.Scheme, opts DatabaseOpts) (string, error) {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(databaseGVK)
	obj.SetName(opts.Name)
	obj.SetNamespace(opts.Namespace)

	spec := map[string]interface{}{
		"mariaDbRef": map[string]interface{}{
			"name": opts.MariaDBRef,
		},
		"name": opts.DatabaseName,
	}

	if err := unstructured.SetNestedField(obj.Object, spec, "spec"); err != nil {
		return "", fmt.Errorf("setting Database spec: %w", err)
	}

	obj.SetAPIVersion(databaseGVK.Group + "/" + databaseGVK.Version)
	obj.SetKind(databaseGVK.Kind)

	if err := controllerutil.SetControllerReference(owner, obj, scheme); err != nil {
		return "", fmt.Errorf("setting controller reference: %w", err)
	}

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(databaseGVK)
	err := c.Get(ctx, client.ObjectKeyFromObject(obj), existing)
	if apierrors.IsNotFound(err) {
		if err := c.Create(ctx, obj); err != nil {
			return "", fmt.Errorf("creating Database: %w", err)
		}
		return opts.Name, nil
	}
	if err != nil {
		return "", fmt.Errorf("checking for existing Database: %w", err)
	}

	existing.Object["spec"] = obj.Object["spec"]
	existing.SetOwnerReferences(obj.GetOwnerReferences())
	if err := c.Update(ctx, existing); err != nil {
		return "", fmt.Errorf("updating Database: %w", err)
	}

	return opts.Name, nil
}

// EnsureDatabaseUser creates or updates a MariaDB User CR and a Grant CR.
// (CC-0005 / REQ-004)
func EnsureDatabaseUser(ctx context.Context, c client.Client, owner client.Object, scheme *runtime.Scheme, opts DatabaseUserOpts) (string, error) {
	if err := ensureUser(ctx, c, owner, scheme, opts); err != nil {
		return "", err
	}
	if err := ensureGrant(ctx, c, owner, scheme, opts); err != nil {
		return "", err
	}
	return opts.Name, nil
}

func ensureUser(ctx context.Context, c client.Client, owner client.Object, scheme *runtime.Scheme, opts DatabaseUserOpts) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(userGVK)
	obj.SetName(opts.Name)
	obj.SetNamespace(opts.Namespace)

	spec := map[string]interface{}{
		"mariaDbRef": map[string]interface{}{
			"name": opts.MariaDBRef,
		},
		"name": opts.Username,
		"passwordSecretKeyRef": map[string]interface{}{
			"name": opts.PasswordSecretName,
			"key":  opts.PasswordSecretKey,
		},
	}

	if err := unstructured.SetNestedField(obj.Object, spec, "spec"); err != nil {
		return fmt.Errorf("setting User spec: %w", err)
	}

	obj.SetAPIVersion(userGVK.Group + "/" + userGVK.Version)
	obj.SetKind(userGVK.Kind)

	if err := controllerutil.SetControllerReference(owner, obj, scheme); err != nil {
		return fmt.Errorf("setting controller reference on User: %w", err)
	}

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(userGVK)
	err := c.Get(ctx, client.ObjectKeyFromObject(obj), existing)
	if apierrors.IsNotFound(err) {
		if err := c.Create(ctx, obj); err != nil {
			return fmt.Errorf("creating User: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("checking for existing User: %w", err)
	}

	existing.Object["spec"] = obj.Object["spec"]
	existing.SetOwnerReferences(obj.GetOwnerReferences())
	if err := c.Update(ctx, existing); err != nil {
		return fmt.Errorf("updating User: %w", err)
	}

	return nil
}

func ensureGrant(ctx context.Context, c client.Client, owner client.Object, scheme *runtime.Scheme, opts DatabaseUserOpts) error {
	grantName := opts.Name + "-grant"

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(grantGVK)
	obj.SetName(grantName)
	obj.SetNamespace(opts.Namespace)

	privileges := make([]interface{}, len(opts.Privileges))
	for i, p := range opts.Privileges {
		privileges[i] = p
	}

	spec := map[string]interface{}{
		"mariaDbRef": map[string]interface{}{
			"name": opts.MariaDBRef,
		},
		"database":   opts.DatabaseName,
		"table":      "*",
		"username":   opts.Username,
		"privileges": privileges,
	}

	if err := unstructured.SetNestedField(obj.Object, spec, "spec"); err != nil {
		return fmt.Errorf("setting Grant spec: %w", err)
	}

	obj.SetAPIVersion(grantGVK.Group + "/" + grantGVK.Version)
	obj.SetKind(grantGVK.Kind)

	if err := controllerutil.SetControllerReference(owner, obj, scheme); err != nil {
		return fmt.Errorf("setting controller reference on Grant: %w", err)
	}

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(grantGVK)
	err := c.Get(ctx, client.ObjectKeyFromObject(obj), existing)
	if apierrors.IsNotFound(err) {
		if err := c.Create(ctx, obj); err != nil {
			return fmt.Errorf("creating Grant: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("checking for existing Grant: %w", err)
	}

	existing.Object["spec"] = obj.Object["spec"]
	existing.SetOwnerReferences(obj.GetOwnerReferences())
	if err := c.Update(ctx, existing); err != nil {
		return fmt.Errorf("updating Grant: %w", err)
	}

	return nil
}

// RunDBSyncJob creates a batch/v1 Job for database schema migration.
// (CC-0005 / REQ-004)
func RunDBSyncJob(ctx context.Context, c client.Client, owner client.Object, scheme *runtime.Scheme, opts DBSyncJobOpts) (string, error) {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      opts.Name,
			Namespace: opts.Namespace,
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:    "db-sync",
							Image:   opts.Image,
							Command: opts.Command,
							Env:     opts.Env,
						},
					},
					RestartPolicy: corev1.RestartPolicyOnFailure,
				},
			},
		},
	}

	if err := controllerutil.SetControllerReference(owner, job, scheme); err != nil {
		return "", fmt.Errorf("setting controller reference on Job: %w", err)
	}

	err := c.Create(ctx, job)
	if apierrors.IsAlreadyExists(err) {
		return opts.Name, nil
	}
	if err != nil {
		return "", fmt.Errorf("creating db-sync Job: %w", err)
	}

	return opts.Name, nil
}
