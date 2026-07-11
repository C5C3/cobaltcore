// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package job

import (
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MigrationJobParams carries the inputs of BuildMigrationJob: the generic
// database-migration Job skeleton every database-backed service operator runs
// (db_sync, expand/migrate/contract, schema check). The service-specific parts
// — the image, the command, the config mount path, the DB-connection env, and
// any TLS/domain volumes — are supplied by the caller; the skeleton assembles a
// single-container, RestartPolicy=Never Job with the config ConfigMap mounted
// read-only.
type MigrationJobParams struct {
	// Name is the full Job name; Namespace is its namespace.
	Name      string
	Namespace string
	// Labels are applied to the Job; nil leaves it unlabelled.
	Labels map[string]string
	// Image is the container image the migration runs.
	Image string
	// ContainerName names the single container (usually the phase suffix).
	ContainerName string
	// Command is the container command (for example keystone-manage db_sync).
	Command []string
	// ConfigMapName is the rendered config ConfigMap, mounted read-only at
	// ConfigMountPath under the volume name "config".
	ConfigMapName   string
	ConfigMountPath string
	// Env are extra environment variables (for example the DB-connection URL).
	Env []corev1.EnvVar
	// ExtraVolumes and ExtraVolumeMounts are appended after the config volume
	// (for example a DB-TLS keypair or per-domain config).
	ExtraVolumes      []corev1.Volume
	ExtraVolumeMounts []corev1.VolumeMount
	// PriorityClassName sets the Pod priority class; empty leaves it unset.
	PriorityClassName string
	// BackoffLimit sets spec.backoffLimit.
	BackoffLimit int32
	// TTLSecondsAfterFinished, when non-nil, sets spec.ttlSecondsAfterFinished.
	TTLSecondsAfterFinished *int32
	// SecurityContext is applied to the container; supply a restricted context.
	SecurityContext *corev1.SecurityContext
}

// BuildMigrationJob constructs the shared migration-Job skeleton from p. It
// mounts the config ConfigMap read-only at ConfigMountPath, then appends any
// ExtraVolumes/ExtraVolumeMounts, so a caller with no extras gets a plain
// config-mounted Job.
func BuildMigrationJob(p MigrationJobParams) *batchv1.Job {
	backoffLimit := p.BackoffLimit
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      p.Name,
			Namespace: p.Namespace,
			Labels:    p.Labels,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			TTLSecondsAfterFinished: p.TTLSecondsAfterFinished,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy:     corev1.RestartPolicyNever,
					PriorityClassName: p.PriorityClassName,
					Containers: []corev1.Container{{
						Name:            p.ContainerName,
						Image:           p.Image,
						Command:         p.Command,
						SecurityContext: p.SecurityContext,
						Env:             p.Env,
						VolumeMounts: []corev1.VolumeMount{{
							Name:      "config",
							MountPath: p.ConfigMountPath,
							ReadOnly:  true,
						}},
					}},
					Volumes: []corev1.Volume{{
						Name: "config",
						VolumeSource: corev1.VolumeSource{
							ConfigMap: &corev1.ConfigMapVolumeSource{
								LocalObjectReference: corev1.LocalObjectReference{
									Name: p.ConfigMapName,
								},
							},
						},
					}},
				},
			},
		},
	}
	if len(p.ExtraVolumes) > 0 {
		job.Spec.Template.Spec.Volumes = append(job.Spec.Template.Spec.Volumes, p.ExtraVolumes...)
	}
	if len(p.ExtraVolumeMounts) > 0 {
		job.Spec.Template.Spec.Containers[0].VolumeMounts = append(
			job.Spec.Template.Spec.Containers[0].VolumeMounts, p.ExtraVolumeMounts...,
		)
	}
	return job
}
