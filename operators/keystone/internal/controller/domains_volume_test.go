// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"

	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// assertDomainsProjection asserts the pod spec carries the domains volume
// (pointing at secretName, mode 0400) and that the named container mounts it
// read-only at /etc/keystone/domains.
func assertDomainsProjection(g Gomega, podSpec corev1.PodSpec, containerName, secretName string) {
	vol := findVolumeByName(podSpec.Volumes, domainsVolumeName)
	g.Expect(vol).NotTo(BeNil(), "domains volume must be present")
	g.Expect(vol.Secret).NotTo(BeNil())
	g.Expect(vol.Secret.SecretName).To(Equal(secretName))
	g.Expect(vol.Secret.DefaultMode).To(HaveValue(Equal(int32(0o400))),
		"per-domain files carry LDAP bind passwords")

	var container *corev1.Container
	for i := range podSpec.Containers {
		if podSpec.Containers[i].Name == containerName {
			container = &podSpec.Containers[i]
		}
	}
	g.Expect(container).NotTo(BeNil(), "container %s must exist", containerName)
	mount := findVolumeMountByName(container.VolumeMounts, domainsVolumeName)
	g.Expect(mount).NotTo(BeNil(), "domains mount must be present on %s", containerName)
	g.Expect(mount.MountPath).To(Equal(domainsMountPath))
	g.Expect(mount.ReadOnly).To(BeTrue())
}

// assertNoDomainsProjection asserts the pod spec carries no domains volume
// and no container mounts one.
func assertNoDomainsProjection(g Gomega, podSpec corev1.PodSpec) {
	g.Expect(findVolumeByName(podSpec.Volumes, domainsVolumeName)).To(BeNil(),
		"no domains volume may be present when nothing is projected")
	for _, c := range podSpec.Containers {
		g.Expect(findVolumeMountByName(c.VolumeMounts, domainsVolumeName)).To(BeNil(),
			"container %s must not mount the domains volume", c.Name)
	}
}

// TestDomainsVolumeThreading pins the projection contract across every
// workload builder: the domains Secret is mounted in the Deployment and every
// keystone-manage Job/CronJob when a name is threaded, and absent when empty.
func TestDomainsVolumeThreading(t *testing.T) {
	const domainsSecret = "test-keystone-domains-abcd1234"

	builders := []struct {
		name          string
		containerName string
		podSpec       func(domainsSecretName string) corev1.PodSpec
	}{
		{
			name:          "Deployment",
			containerName: "keystone",
			podSpec: func(n string) corev1.PodSpec {
				return buildKeystoneDeployment(deployTestKeystone(), "keystone-config-abc123", "", n, nil).Spec.Template.Spec
			},
		},
		{
			name:          "BootstrapJob",
			containerName: "bootstrap",
			podSpec: func(n string) corev1.PodSpec {
				return buildBootstrapJob(deployTestKeystone(), "keystone-config-abc123", n, "keystone-fernet-keys", "hash").Spec.Template.Spec
			},
		},
		{
			name:          "DBSyncJob",
			containerName: "db-sync",
			podSpec: func(n string) corev1.PodSpec {
				return buildDBSyncJob(deployTestKeystone(), "keystone-config-abc123", n).Spec.Template.Spec
			},
		},
		{
			name:          "SchemaCheckJob",
			containerName: "schema-check",
			podSpec: func(n string) corev1.PodSpec {
				return buildSchemaCheckJob(deployTestKeystone(), "keystone-config-abc123", n).Spec.Template.Spec
			},
		},
		{
			name:          "ExpandJob",
			containerName: "db-expand",
			podSpec: func(n string) corev1.PodSpec {
				return buildExpandJob(deployTestKeystone(), "keystone-config-abc123", n, "2025.2").Spec.Template.Spec
			},
		},
		{
			name:          "MigrateJob",
			containerName: "db-migrate",
			podSpec: func(n string) corev1.PodSpec {
				return buildMigrateJob(deployTestKeystone(), "keystone-config-abc123", n, "2025.2").Spec.Template.Spec
			},
		},
		{
			name:          "ContractJob",
			containerName: "db-contract",
			podSpec: func(n string) corev1.PodSpec {
				return buildContractJob(deployTestKeystone(), "keystone-config-abc123", n, "2025.2").Spec.Template.Spec
			},
		},
		{
			name:          "TrustFlushCronJob",
			containerName: "trust-flush",
			podSpec: func(n string) corev1.PodSpec {
				ks := deployTestKeystone()
				ks.Spec.TrustFlush = &keystonev1alpha1.TrustFlushSpec{Schedule: "0 * * * *"}
				return trustFlushCronJob(ks, "keystone-config-abc123", n).Spec.JobTemplate.Spec.Template.Spec
			},
		},
		{
			name:          "FernetRotationCronJob",
			containerName: "fernet-rotate",
			podSpec: func(n string) corev1.PodSpec {
				return fernetRotationCronJob(deployTestKeystone(), "keystone-config-abc123", "scripts-cm", n).Spec.JobTemplate.Spec.Template.Spec
			},
		},
		{
			name:          "CredentialRotationCronJob",
			containerName: "credential-rotate",
			podSpec: func(n string) corev1.PodSpec {
				return credentialRotationCronJob(deployTestKeystone(), "keystone-config-abc123", "scripts-cm", n).Spec.JobTemplate.Spec.Template.Spec
			},
		},
		{
			name:          "PolicyValidationJob",
			containerName: "validator",
			podSpec: func(n string) corev1.PodSpec {
				return buildPolicyValidationJob(deployTestKeystone(), "keystone-config-abc123", n).Spec.Template.Spec
			},
		},
	}

	for _, b := range builders {
		t.Run(b.name+"/mounted when projected", func(t *testing.T) {
			g := NewGomegaWithT(t)
			assertDomainsProjection(g, b.podSpec(domainsSecret), b.containerName, domainsSecret)
		})
		t.Run(b.name+"/absent when not projected", func(t *testing.T) {
			g := NewGomegaWithT(t)
			assertNoDomainsProjection(g, b.podSpec(""))
		})
	}
}
