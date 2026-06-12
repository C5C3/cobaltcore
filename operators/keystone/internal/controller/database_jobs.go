// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

func buildDatabase(keystone *keystonev1alpha1.Keystone) *mariadbv1alpha1.Database {
	key := mariaDBResourceKey(keystone)
	return &mariadbv1alpha1.Database{
		ObjectMeta: metav1.ObjectMeta{
			Name:      key.Name,
			Namespace: key.Namespace,
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
	key := mariaDBResourceKey(keystone)
	return &mariadbv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{
			Name:      key.Name,
			Namespace: key.Namespace,
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
	key := mariaDBResourceKey(keystone)
	return &mariadbv1alpha1.Grant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      key.Name,
			Namespace: key.Namespace,
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
			Username:   key.Name,
		},
	}
}

// buildDBJob constructs a keystone-manage db_sync Job with the shared container spec,
// volume mounts, and security context used by both regular db_sync and upgrade phase
// jobs. This single builder prevents drift when these need to change in the future.
//
// TODO: Wire spec.Resources (or a smaller Job-specific default) to the
// container. Currently runs as BestEffort QoS. See reconcile_deployment.go
// containerResources() for the pattern used by the keystone container.
func buildDBJob(keystone *keystonev1alpha1.Keystone, configMapName, imageTag, nameSuffix string, command []string) *batchv1.Job {
	backoffLimit := int32(4)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%s", keystone.Name, nameSuffix),
			Namespace: keystone.Namespace,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoffLimit,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy:     corev1.RestartPolicyNever,
					PriorityClassName: priorityClassName(keystone),
					Containers: []corev1.Container{{
						Name:            nameSuffix,
						Image:           fmt.Sprintf("%s:%s", keystone.Spec.Image.Repository, imageTag),
						Command:         command,
						SecurityContext: restrictedSecurityContext(),
						// Override [database].connection via oslo.config env-var so every
						// db_sync variant (db-sync, expand, migrate, contract, schema-check)
						// reads the DB URL from the derived Secret instead of the ConfigMap.
						Env: []corev1.EnvVar{buildDBConnectionEnvVar(keystone)},
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
	// Project the db-tls client keypair into every db_sync variant
	// (db-sync, expand, migrate, contract, schema-check) when DB TLS is
	// enabled; the gate is centralised in dbTLSEnabled so deployment and job
	// builders decide identically.
	if dbTLSEnabled(keystone) {
		tlsVol, tlsMount := dbTLSVolumeAndMount(keystone)
		job.Spec.Template.Spec.Volumes = append(job.Spec.Template.Spec.Volumes, tlsVol)
		job.Spec.Template.Spec.Containers[0].VolumeMounts = append(
			job.Spec.Template.Spec.Containers[0].VolumeMounts, tlsMount,
		)
	}
	return job
}

func buildDBSyncJob(keystone *keystonev1alpha1.Keystone, configMapName string) *batchv1.Job {
	return buildDBJob(keystone, configMapName, keystone.Spec.Image.Tag, "db-sync",
		[]string{"keystone-manage", "--config-dir=/etc/keystone/keystone.conf.d/", "db_sync"})
}

// buildUpgradeJob creates a db_sync Job for one of the expand-migrate-contract
// upgrade phases. The imageTag parameter allows callers to pin the
// image independently of spec.Image.Tag (expand/migrate use the old release,
// contract uses the new release).
func buildUpgradeJob(keystone *keystonev1alpha1.Keystone, configMapName, imageTag, phase, flag string) *batchv1.Job {
	return buildDBJob(keystone, configMapName, imageTag, fmt.Sprintf("db-%s", phase),
		[]string{"keystone-manage", "--config-dir=/etc/keystone/keystone.conf.d/", "db_sync", flag})
}

// buildExpandJob creates a db_sync --expand Job using the given imageTag.
func buildExpandJob(keystone *keystonev1alpha1.Keystone, configMapName, imageTag string) *batchv1.Job {
	return buildUpgradeJob(keystone, configMapName, imageTag, "expand", "--expand")
}

// buildMigrateJob creates a db_sync --migrate Job using the given imageTag.
func buildMigrateJob(keystone *keystonev1alpha1.Keystone, configMapName, imageTag string) *batchv1.Job {
	return buildUpgradeJob(keystone, configMapName, imageTag, "migrate", "--migrate")
}

// buildContractJob creates a db_sync --contract Job using the given imageTag.
func buildContractJob(keystone *keystonev1alpha1.Keystone, configMapName, imageTag string) *batchv1.Job {
	return buildUpgradeJob(keystone, configMapName, imageTag, "contract", "--contract")
}

// buildSchemaCheckJob constructs a schema-check Job that verifies the database
// schema matches the expected Alembic migration state after db_sync completes.
// The Job runs keystone-manage db_sync --check which exits 0 when the schema is
// up-to-date, and 1..4 when expand/migrate/contract migrations are pending.
// It delegates to buildDBJob for the shared pod spec and overrides backoffLimit=2
// for a read-only check. TTLSecondsAfterFinished is
// intentionally left unset: the completed Job is the RunJob state record, and a
// TTL-driven garbage-collection would re-create it on the next reconcile, causing
// a re-creation loop (#415). The Job is cleaned up via owner-reference GC
// with the Keystone CR.
func buildSchemaCheckJob(keystone *keystonev1alpha1.Keystone, configMapName string) *batchv1.Job {
	// Read-only schema verification via keystone-manage db_sync --check.
	// Exit codes: 0 = up-to-date, 1..4 = needs expand/migrate/contract.
	// This avoids parsing db_version output, which mixes Oslo log lines with
	// revision hashes and only reports the expand head (not the contract head).
	schemaCheckScript := `keystone-manage --config-dir=/etc/keystone/keystone.conf.d/ db_sync --check`

	j := buildDBJob(keystone, configMapName, keystone.Spec.Image.Tag, "schema-check",
		[]string{"/bin/sh", "-eu", "-c", schemaCheckScript})

	// Override defaults: fewer retries for a read-only check. The completed Job
	// lingers as the RunJob state record (no TTL) to avoid a TTL-driven
	// re-creation loop (#415).
	backoffLimit := int32(2)
	j.Spec.BackoffLimit = &backoffLimit

	return j
}
