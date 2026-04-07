// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/database"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// Feature: CC-0013

// reconcileDatabase ensures the Keystone database schema is provisioned and
// migrated. In managed mode (ClusterRef set) it creates MariaDB Database, User,
// and Grant CRs and waits for them to become Ready before running the db_sync
// Job. In brownfield mode (Host set) it skips the MariaDB CRs and runs db_sync
// directly (CC-0013).
func (r *KeystoneReconciler) reconcileDatabase(ctx context.Context, keystone *keystonev1alpha1.Keystone, configMapName string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Managed mode: create MariaDB CRs and wait for readiness.
	if keystone.Spec.Database.ClusterRef != nil {
		dbReady, err := database.EnsureDatabase(ctx, r.Client, r.Scheme, keystone, buildDatabase(keystone))
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("ensuring Database: %w", err)
		}
		if !dbReady {
			logger.Info("MariaDB Database not ready, requeuing")
			conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
				Type:               "DatabaseReady",
				Status:             metav1.ConditionFalse,
				ObservedGeneration: keystone.Generation,
				Reason:             "WaitingForDatabase",
				Message:            "MariaDB Database CR is not ready",
			})
			return ctrl.Result{RequeueAfter: RequeueDatabaseWait}, nil
		}

		userReady, err := database.EnsureDatabaseUser(ctx, r.Client, r.Scheme, keystone, buildUser(keystone), buildGrant(keystone))
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("ensuring database user: %w", err)
		}
		if !userReady {
			logger.Info("MariaDB User/Grant not ready, requeuing")
			conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
				Type:               "DatabaseReady",
				Status:             metav1.ConditionFalse,
				ObservedGeneration: keystone.Generation,
				Reason:             "WaitingForDatabase",
				Message:            "MariaDB User or Grant CR is not ready",
			})
			return ctrl.Result{RequeueAfter: RequeueDatabaseWait}, nil
		}
	}

	// Run the db_sync Job.
	done, err := database.RunDBSyncJob(ctx, r.Client, r.Scheme, keystone, buildDBSyncJob(keystone, configMapName))
	if err != nil {
		conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
			Type:               "DatabaseReady",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: keystone.Generation,
			Reason:             "DBSyncFailed",
			Message:            fmt.Sprintf("db_sync job failed: %v", err),
		})
		return ctrl.Result{}, fmt.Errorf("running db_sync: %w", err)
	}
	if !done {
		logger.Info("db_sync job in progress, requeuing")
		conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
			Type:               "DatabaseReady",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: keystone.Generation,
			Reason:             "DBSyncInProgress",
			Message:            "db_sync job is running",
		})
		return ctrl.Result{RequeueAfter: RequeueDatabaseWait}, nil
	}

	conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
		Type:               "DatabaseReady",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: keystone.Generation,
		Reason:             "DatabaseSynced",
		Message:            "Database schema is up to date",
	})
	return ctrl.Result{}, nil
}

func buildDatabase(keystone *keystonev1alpha1.Keystone) *mariadbv1alpha1.Database {
	return &mariadbv1alpha1.Database{
		ObjectMeta: metav1.ObjectMeta{
			Name:      keystone.Name,
			Namespace: keystone.Namespace,
		},
		Spec: mariadbv1alpha1.DatabaseSpec{
			MariaDBRef: mariadbv1alpha1.MariaDBRef{
				ObjectReference: mariadbv1alpha1.ObjectReference{
					Name: keystone.Spec.Database.ClusterRef.Name,
				},
			},
			CharacterSet: "utf8",
			Collate:      "utf8_general_ci",
			Name:         keystone.Spec.Database.Database,
		},
	}
}

func buildUser(keystone *keystonev1alpha1.Keystone) *mariadbv1alpha1.User {
	return &mariadbv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{
			Name:      keystone.Name,
			Namespace: keystone.Namespace,
		},
		Spec: mariadbv1alpha1.UserSpec{
			MariaDBRef: mariadbv1alpha1.MariaDBRef{
				ObjectReference: mariadbv1alpha1.ObjectReference{
					Name: keystone.Spec.Database.ClusterRef.Name,
				},
			},
			PasswordSecretKeyRef: &mariadbv1alpha1.SecretKeySelector{
				LocalObjectReference: mariadbv1alpha1.LocalObjectReference{
					Name: keystone.Spec.Database.SecretRef.Name,
				},
				Key: "password",
			},
		},
	}
}

func buildGrant(keystone *keystonev1alpha1.Keystone) *mariadbv1alpha1.Grant {
	return &mariadbv1alpha1.Grant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      keystone.Name,
			Namespace: keystone.Namespace,
		},
		Spec: mariadbv1alpha1.GrantSpec{
			MariaDBRef: mariadbv1alpha1.MariaDBRef{
				ObjectReference: mariadbv1alpha1.ObjectReference{
					Name: keystone.Spec.Database.ClusterRef.Name,
				},
			},
			Privileges: []string{"ALL PRIVILEGES"},
			Database:   keystone.Spec.Database.Database,
			Table:      "*",
			Username:   keystone.Name,
		},
	}
}

func buildDBSyncJob(keystone *keystonev1alpha1.Keystone, configMapName string) *batchv1.Job {
	backoffLimit := int32(4)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-db-sync", keystone.Name),
			Namespace: keystone.Namespace,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoffLimit,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:    "db-sync",
						Image: fmt.Sprintf("%s:%s", keystone.Spec.Image.Repository, keystone.Spec.Image.Tag),
						// TODO(CC-0042): Wire spec.Resources (or a smaller Job-specific default) to
						// this container. Currently runs as BestEffort QoS. See reconcile_deployment.go
						// containerResources() for the pattern used by the keystone-api container.
						Command: []string{"keystone-manage", "--config-dir=/etc/keystone/keystone.conf.d/", "db_sync"},
						VolumeMounts: []corev1.VolumeMount{{
							Name:      "config",
							MountPath: "/etc/keystone/keystone.conf.d/",
							ReadOnly:  true,
						}},
					}},
					Volumes: []corev1.Volume{{
						Name: "config",
						VolumeSource: corev1.VolumeSource{
							ConfigMap: &corev1.ConfigMapVolumeSource{
								LocalObjectReference: corev1.LocalObjectReference{
									Name: configMapName,
								},
							},
						},
					}},
				},
			},
		},
	}
}
