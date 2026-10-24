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

	"github.com/c5c3/cobaltcore/internal/common/deployment"
	"github.com/c5c3/cobaltcore/internal/common/rotation"
	keystonev1alpha1 "github.com/c5c3/cobaltcore/operators/keystone/api/v1alpha1"
)

// keyRotationParams captures the per-flavour differences between the Fernet and
// credential key-rotation CronJobs. Everything else — the init container that
// copies keys into a writable emptyDir, the FSGroup, the security contexts, the
// staging-Secret PATCH target, the config and script mounts, the
// sibling-key read-only mount, the rotation-state emptyDir, the Forbid
// concurrency policy, and the Job deadline — is identical and lives in
// keyRotationCronJob.
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

// keyRotationActiveDeadlineSeconds caps how long one rotation Job may stay
// active. Under the Forbid concurrency policy a Job that never reaches Complete
// or Failed holds the CronJob's only slot, and every later tick skips with
// JobAlreadyActive. A pod stuck in ImagePullBackOff, which backoffLimit does
// not count, or a PATCH hung on a dead API server connection reaches neither
// on its own. The deadline fails such a Job within the hour, so the next tick
// starts a fresh Job from the current template. An hour is far above the
// runtime of a key rotation.
const keyRotationActiveDeadlineSeconds int64 = 3600

// rotationStateVolumeName and rotationStateMountPath name the emptyDir where
// both rotation scripts save the rotated key set and the digest of the
// production keys they rotated, so a container restarted after a failed PATCH
// does not rotate again. Both scripts default STATE_DIR to this path.
const (
	rotationStateVolumeName = "rotation-state"
	rotationStateMountPath  = "/rotation-state"
)

// keyRotationCronJob builds the CronJob that rotates a key repository and
// persists the result back to a staging Secret via the K8s API. The CronJob:
//  1. Mounts the production keys Secret as a read-only volume.
//  2. Uses an init container to copy keys into a writable emptyDir.
//  3. Mounts the rotation script from a versioned ConfigMap at /scripts/.
//  4. Runs /scripts/<keyKind>_rotate.sh, which resets the emptyDir from the
//     read-only source mount and rotates it, or, in a restarted container,
//     restores the key set it saved in the rotation-state emptyDir.
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
	pullPolicy := keystone.Spec.Image.EffectivePullPolicy()

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
		// FSGroup makes the kubelet group-own mounted Secret volumes by
		// the openstack GID so DefaultMode 0o400 still lets the openstack
		// UID read the keys via the group bit. Without it, kubelet would
		// project files as root:root mode 0o400 and the openstack process
		// could not read them; upstream Keystone logs a "key_repository is
		// world readable" WARNING via fernet_utils._check_key_repository
		// for any default-mode (0o644) workaround.
		SecurityContext: &corev1.PodSecurityContext{FSGroup: ptr.To(deployment.OpenStackUID)},
		InitContainers: []corev1.Container{{
			Name:            "copy-keys",
			Image:           image,
			ImagePullPolicy: pullPolicy,
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
			Name:            p.keyKind + "-rotate",
			Image:           image,
			ImagePullPolicy: pullPolicy,
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
				{Name: srcVolName, MountPath: srcMountPath, ReadOnly: true},
				{Name: rotationStateVolumeName, MountPath: rotationStateMountPath},
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
			{
				Name: rotationStateVolumeName,
				VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{},
				},
			},
		},
	}
	// Append the per-domain identity-backend volume/mount when a backend is
	// projected, apply the Job pod settings (the copy-keys init container
	// included), then wrap the pod spec in the shared CronJob boilerplate.
	podSpec.Volumes = append(podSpec.Volumes, extraVolumes...)
	podSpec.Containers[0].VolumeMounts = append(podSpec.Containers[0].VolumeMounts, extraMounts...)
	keystoneJobPod(keystone).Apply(&podSpec)

	cronJob := rotation.BuildCronJob(rotation.CronJobParams{
		Name:      name,
		Namespace: keystone.Namespace,
		Labels:    commonLabels(keystone),
		Schedule:  p.schedule,
		PodLabels: componentLabels(keystone, p.keyKind+"-rotation"),
		PodSpec:   podSpec,
	})
	// A scheduled rotation never starts while an earlier scheduled rotation is
	// still active, so two runs cannot rotate the same key set and race on the
	// staging-Secret PATCH. A Job created with
	// `kubectl create job --from=cronjob/...` is not governed: the CronJob
	// controller never adds it to status.active (upstream
	// pkg/controller/cronjob/cronjob_controllerv2.go emits UnexpectedJob for it
	// and does not adopt it). The policy is a keystone decision, so it is set
	// here and rotation.BuildCronJob stays generic; both key kinds get it
	// through this shared builder.
	cronJob.Spec.ConcurrencyPolicy = batchv1.ForbidConcurrent
	cronJob.Spec.JobTemplate.Spec.ActiveDeadlineSeconds = ptr.To(keyRotationActiveDeadlineSeconds)
	return cronJob
}
