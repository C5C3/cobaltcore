// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package database

import (
	"testing"

	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"

	"github.com/c5c3/forge/internal/common/deployment"
)

// keystoneJobSet returns a JobSetParams shaped like the keystone operator's
// db_sync/schema-check inputs, including a db-tls extra volume, so the builder
// tests exercise the full pass-through.
func keystoneJobSet() JobSetParams {
	return JobSetParams{
		InstanceName:    "keystone",
		Namespace:       "openstack",
		Image:           "registry.example.com/keystone@sha256:abc",
		ConfigMapName:   "keystone-config-abc123",
		ConfigMountPath: "/etc/keystone/keystone.conf.d/",
		Env: []corev1.EnvVar{{
			Name: "OS_DATABASE__CONNECTION",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "keystone-db-connection"},
					Key:                  "connection",
				},
			},
		}},
		ExtraVolumes: []corev1.Volume{{
			Name: "db-tls",
		}},
		ExtraVolumeMounts: []corev1.VolumeMount{{
			Name:      "db-tls",
			MountPath: "/etc/keystone/db-tls/",
			ReadOnly:  true,
		}},
		PriorityClassName:  "keystone-critical",
		SyncCommand:        []string{"keystone-manage", "--config-dir=/etc/keystone/keystone.conf.d/", "db_sync"},
		SchemaCheckCommand: []string{"/bin/sh", "-eu", "-c", "keystone-manage db_sync --check"},
	}
}

func TestSyncJob(t *testing.T) {
	g := NewWithT(t)
	p := keystoneJobSet()
	j := SyncJob(p)

	g.Expect(j.Name).To(Equal("keystone-db-sync"))
	g.Expect(j.Namespace).To(Equal("openstack"))
	g.Expect(*j.Spec.BackoffLimit).To(Equal(syncJobBackoffLimit))

	spec := j.Spec.Template.Spec
	g.Expect(spec.RestartPolicy).To(Equal(corev1.RestartPolicyNever))
	g.Expect(spec.PriorityClassName).To(Equal("keystone-critical"))
	g.Expect(spec.Containers).To(HaveLen(1))

	c := spec.Containers[0]
	g.Expect(c.Name).To(Equal("db-sync"))
	g.Expect(c.Image).To(Equal(p.Image))
	g.Expect(c.Command).To(Equal(p.SyncCommand))
	g.Expect(c.Env).To(Equal(p.Env))
	// The restricted security context is applied to every migration Job.
	g.Expect(c.SecurityContext).To(Equal(deployment.RestrictedSecurityContext()))

	// Config volume is mounted first, read-only, at the configured path; the
	// extra db-tls mount is appended after it.
	g.Expect(c.VolumeMounts).To(HaveLen(2))
	g.Expect(c.VolumeMounts[0].Name).To(Equal("config"))
	g.Expect(c.VolumeMounts[0].MountPath).To(Equal(p.ConfigMountPath))
	g.Expect(c.VolumeMounts[0].ReadOnly).To(BeTrue())
	g.Expect(c.VolumeMounts[1].Name).To(Equal("db-tls"))

	g.Expect(spec.Volumes).To(HaveLen(2))
	g.Expect(spec.Volumes[0].Name).To(Equal("config"))
	g.Expect(spec.Volumes[0].ConfigMap.Name).To(Equal(p.ConfigMapName))
	g.Expect(spec.Volumes[1].Name).To(Equal("db-tls"))
}

func TestSchemaCheckJob(t *testing.T) {
	g := NewWithT(t)
	p := keystoneJobSet()
	j := SchemaCheckJob(p)

	g.Expect(j.Name).To(Equal("keystone-schema-check"))
	g.Expect(j.Spec.Template.Spec.Containers[0].Name).To(Equal("schema-check"))
	g.Expect(j.Spec.Template.Spec.Containers[0].Command).To(Equal(p.SchemaCheckCommand))
	// The read-only check uses a lower backoff limit than db-sync.
	g.Expect(*j.Spec.BackoffLimit).To(Equal(schemaCheckJobBackoffLimit))
	// TTLSecondsAfterFinished is deliberately unset (#415).
	g.Expect(j.Spec.TTLSecondsAfterFinished).To(BeNil())
}

func TestBuildJob_upgradePhasePinsImageAndSuffix(t *testing.T) {
	g := NewWithT(t)
	p := keystoneJobSet()
	// The expand phase pins the old release image independently of p.Image and
	// runs db_sync --expand under the "db-expand" suffix.
	j := BuildJob(p, "registry.example.com/keystone:2025.1", "db-expand",
		[]string{"keystone-manage", "db_sync", "--expand"}, 4)

	g.Expect(j.Name).To(Equal("keystone-db-expand"))
	c := j.Spec.Template.Spec.Containers[0]
	g.Expect(c.Name).To(Equal("db-expand"))
	g.Expect(c.Image).To(Equal("registry.example.com/keystone:2025.1"))
	g.Expect(c.Command).To(Equal([]string{"keystone-manage", "db_sync", "--expand"}))
	g.Expect(*j.Spec.BackoffLimit).To(Equal(int32(4)))
}

// TestBuildJob_noExtras exercises the empty-extras edge path: a JobSetParams with
// no ExtraVolumes/ExtraVolumeMounts, no Env, and no PriorityClass yields a plain
// config-mounted Job with a single volume and mount.
func TestBuildJob_noExtras(t *testing.T) {
	g := NewWithT(t)
	p := JobSetParams{
		InstanceName:    "glance",
		Namespace:       "openstack",
		Image:           "registry.example.com/glance:2026.1",
		ConfigMapName:   "glance-config",
		ConfigMountPath: "/etc/glance/glance.conf.d/",
		SyncCommand:     []string{"glance-manage", "db", "sync"},
	}
	j := SyncJob(p)

	g.Expect(j.Name).To(Equal("glance-db-sync"))
	spec := j.Spec.Template.Spec
	g.Expect(spec.PriorityClassName).To(BeEmpty())
	g.Expect(spec.Volumes).To(HaveLen(1))
	c := spec.Containers[0]
	g.Expect(c.Env).To(BeEmpty())
	g.Expect(c.VolumeMounts).To(HaveLen(1))
	g.Expect(c.Command).To(Equal([]string{"glance-manage", "db", "sync"}))
}
