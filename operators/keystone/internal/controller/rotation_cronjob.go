// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"
	"strconv"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	"github.com/c5c3/forge/internal/common/deployment"
	"github.com/c5c3/forge/internal/common/rotation"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// keyRotationParams captures the per-flavour differences between the Fernet and
// credential key-rotation CronJobs. Everything else — the init container that
// copies keys into a writable emptyDir, the FSGroup, the security contexts, the
// staging-Secret PATCH target, the config and script mounts, and the
// sibling-key read-only mount — is identical and lives in keyRotationCronJob.
type keyRotationParams struct {
	// keyKind is the rotated key kind ("fernet" or "credential"). It drives the
	// production Secret name (<keystone>-<keyKind>-keys), the SA/CronJob name
	// (<keystone>-<keyKind>-rotate), the container name (<keyKind>-rotate), the
	// /etc/keystone/<keyKind>-keys repository directory, the emptyDir/source
	// volume names, and the /scripts/<keyKind>_rotate.sh command.
	keyKind string
	// otherKeyKind is the sibling key kind, mounted read-only so keystone-manage
	// sees both key repositories referenced by the config even though this job
	// only rotates keyKind.
	otherKeyKind string
	// stagingSecretName is the operator-owned staging Secret the CronJob PATCHes
	// its rotation output into (the CronJob SA may only patch this Secret, never
	// the production Secret).
	stagingSecretName string
	// schedule is the cron schedule from the relevant spec field.
	schedule string
	// maxActiveKeysEnv is the oslo.config env-var override name (e.g.
	// OS_fernet_tokens__max_active_keys), maxActiveKeys its normalized value.
	maxActiveKeysEnv string
	maxActiveKeys    int
}

// keyRotationCronJob builds the CronJob that rotates a key repository and
// persists the result back to a staging Secret via the K8s API. The CronJob:
//  1. Mounts the production keys Secret as a read-only volume.
//  2. Uses an init container to copy keys into a writable emptyDir.
//  3. Mounts the rotation script from a versioned ConfigMap at /scripts/.
//  4. Runs /scripts/<keyKind>_rotate.sh against the emptyDir.
//  5. PATCHes the updated keys onto the staging Secret using the pod's
//     ServiceAccount.
//
// It is parameterised by keyRotationParams so the Fernet and credential
// CronJobs, which differ only in the secret-name pair, env-var name, and script,
// share one builder (removing the prior "MUST stay in sync" drift risk).
func keyRotationCronJob(keystone *keystonev1alpha1.Keystone, configMapName, scriptConfigMapName, domainsSecretName string, p keyRotationParams) *batchv1.CronJob {
	name := fmt.Sprintf("%s-%s-rotate", keystone.Name, p.keyKind)
	secretName := fmt.Sprintf("%s-%s-keys", keystone.Name, p.keyKind)
	otherSecretName := fmt.Sprintf("%s-%s-keys", keystone.Name, p.otherKeyKind)
	image := keystone.Spec.Image.Reference()

	keyDir := "/etc/keystone/" + p.keyKind + "-keys"
	otherKeyDir := "/etc/keystone/" + p.otherKeyKind + "-keys"
	srcVolName := p.keyKind + "-keys-src"
	srcMountPath := "/" + p.keyKind + "-keys-src"
	keyVolName := p.keyKind + "-keys"
	otherVolName := p.otherKeyKind + "-keys"

	// Project the per-domain identity-backend config so keystone-manage
	// rotation commands see the same domain-specific driver files the API
	// pods load; empty when no backend is projected.
	var extraVolumes []corev1.Volume
	var extraMounts []corev1.VolumeMount
	if domainsSecretName != "" {
		domVol, domMount := domainsVolumeAndMount(domainsSecretName)
		extraVolumes = append(extraVolumes, domVol)
		extraMounts = append(extraMounts, domMount)
	}

	podSpec := corev1.PodSpec{
		ServiceAccountName: name,
		RestartPolicy:      corev1.RestartPolicyOnFailure,
		PriorityClassName:  priorityClassName(keystone),
		// FSGroup makes the kubelet group-own mounted Secret volumes by
		// the openstack GID so DefaultMode 0o400 still lets the openstack
		// UID read the keys via the group bit. Without it, kubelet would
		// project files as root:root mode 0o400 and the openstack process
		// could not read them; upstream Keystone logs a "key_repository is
		// world readable" WARNING via fernet_utils._check_key_repository
		// for any default-mode (0o644) workaround.
		SecurityContext: &corev1.PodSecurityContext{FSGroup: ptr.To(deployment.OpenStackUID)},
		InitContainers: []corev1.Container{{
			Name:  "copy-keys",
			Image: image,
			// `install -m 0400` materialises each key in the writable emptyDir
			// at owner-read-only mode. A plain `cp` would inherit the kubelet
			// emptyDir mode and re-introduce the world-readable directory for the
			// rotation Pod.
			Command:         []string{"sh", "-c", fmt.Sprintf("install -m 0400 %s/* %s/", srcMountPath, keyDir)},
			SecurityContext: deployment.RestrictedSecurityContext(),
			VolumeMounts: []corev1.VolumeMount{
				{Name: srcVolName, MountPath: srcMountPath, ReadOnly: true},
				{Name: keyVolName, MountPath: keyDir},
			},
		}},
		Containers: []corev1.Container{{
			Name:  p.keyKind + "-rotate",
			Image: image,
			// TODO: Wire spec.Resources (or a smaller Job-specific default) to
			// this container. Currently runs as BestEffort QoS. See reconcile_deployment.go
			// containerResources() for the pattern used by the keystone container.
			Command:         []string{"/scripts/" + p.keyKind + "_rotate.sh"},
			SecurityContext: deployment.RestrictedSecurityContext(),
			Env: []corev1.EnvVar{
				// SECRET_NAME points at the staging Secret — the CronJob SA
				// is only permitted to patch the staging Secret, never the
				// production Secret.
				{Name: "SECRET_NAME", Value: p.stagingSecretName},
				{Name: "SECRET_NAMESPACE", ValueFrom: &corev1.EnvVarSource{
					FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"},
				}},
				// oslo.config honours OS_<GROUP>__<KEY> env var overrides, so this
				// takes precedence over the compiled-in default (3) without needing
				// to mount the ConfigMap. Uses the normalized value to stay
				// consistent with the Secret's minimum floor of 3.
				{
					Name:  p.maxActiveKeysEnv,
					Value: strconv.Itoa(p.maxActiveKeys),
				},
				// Override [database].connection via oslo.config env-var so the
				// rotate CronJob reads the DB URL from the derived Secret instead
				// of the ConfigMap.
				buildDBConnectionEnvVar(keystone),
			},
			VolumeMounts: []corev1.VolumeMount{
				{Name: keyVolName, MountPath: keyDir},
				{Name: otherVolName, MountPath: otherKeyDir, ReadOnly: true},
				{Name: "config", MountPath: "/etc/keystone/keystone.conf.d/", ReadOnly: true},
				{Name: "scripts", MountPath: "/scripts", ReadOnly: true},
			},
		}},
		Volumes: []corev1.Volume{
			{
				Name: srcVolName,
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName:  secretName,
						DefaultMode: ptr.To(int32(0o400)),
					},
				},
			},
			{
				Name: keyVolName,
				VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{},
				},
			},
			{
				// keystone-manage reads the full config which references both
				// key repositories; mount the sibling keys read-only so the
				// directory exists even though this job only rotates one kind.
				Name: otherVolName,
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName:  otherSecretName,
						DefaultMode: ptr.To(int32(0o400)),
					},
				},
			},
			{
				Name: "config",
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: configMapName},
					},
				},
			},
			{
				Name: "scripts",
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: scriptConfigMapName},
						DefaultMode:          ptr.To(int32(0o555)),
					},
				},
			},
		},
	}
	// Append the per-domain identity-backend volume/mount when a backend is
	// projected, then wrap the pod spec in the shared CronJob boilerplate.
	podSpec.Volumes = append(podSpec.Volumes, extraVolumes...)
	podSpec.Containers[0].VolumeMounts = append(podSpec.Containers[0].VolumeMounts, extraMounts...)

	return rotation.BuildCronJob(rotation.CronJobParams{
		Name:      name,
		Namespace: keystone.Namespace,
		Labels:    commonLabels(keystone),
		Schedule:  p.schedule,
		PodLabels: commonLabels(keystone),
		PodSpec:   podSpec,
	})
}
