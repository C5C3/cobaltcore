// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package satellite

import (
	"context"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// SecretNameForVolume returns the Secret a pod-spec volume mounts, or "" when
// the volume is absent or not Secret-backed. A ConfigMap- or emptyDir-backed
// volume of the searched name reads as no pointer rather than as the next
// volume that happens to carry a Secret.
func SecretNameForVolume(spec *corev1.PodSpec, volumeName string) string {
	if spec == nil {
		return ""
	}
	for i := range spec.Volumes {
		v := &spec.Volumes[i]
		if v.Name == volumeName {
			if v.Secret == nil {
				return ""
			}
			return v.Secret.SecretName
		}
	}
	return ""
}

// SectionPresent reports whether rendered carries header as a whole line: the
// literal token bounded by the start of data or a newline on the left and a
// newline (LF or CR) or the end of data on the right. The boundary check guards
// against substring collisions, so a section [name2] does not satisfy a lookup
// for [name], and a header appearing inside an option value does not either.
func SectionPresent(rendered []byte, header string) bool {
	for _, line := range strings.Split(string(rendered), "\n") {
		// Cut at the first CR so a CRLF-terminated line and a bare CR both end
		// the token, matching the newline forms the renderer can emit.
		if token, _, _ := strings.Cut(line, "\r"); token == header {
			return true
		}
	}
	return false
}

// ObserveParams addresses one satellite's projection: the parent's Deployment,
// the volume carrying the rendered config, and the section header that says this
// satellite made it into the file.
type ObserveParams struct {
	// Children reads the Deployment and its projection Secret, on whichever
	// cluster ResolveParentChildren selected.
	Children client.Client
	// DeploymentKey names the parent's Deployment. Its namespace also locates
	// the projection Secret.
	DeploymentKey client.ObjectKey
	// VolumeName is the pod volume mounting the rendered config.
	VolumeName string
	// DataKey is the key inside that volume's Secret holding the rendered file.
	DataKey string
	// SectionHeader is the INI header to look for, for example "[store]".
	SectionHeader string
}

// SectionProjected reads the Deployment, the Secret its VolumeName mounts and
// that Secret's DataKey, and reports whether SectionHeader is present. A
// missing Deployment, volume, Secret or key is (false, nil): a NotFound
// anywhere on the chain is an authoritative "not projected yet", and only a
// non-NotFound client failure is an error.
//
// Consumers keep their own parent re-read ahead of this call and their
// observeConfigProjected wrappers around it, which own the condition reasons,
// the messages and the requeue intervals.
func SectionProjected(ctx context.Context, p ObserveParams) (bool, error) {
	var deploy appsv1.Deployment
	if err := p.Children.Get(ctx, p.DeploymentKey, &deploy); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("fetching Deployment %s: %w", p.DeploymentKey, err)
	}

	secretName := SecretNameForVolume(&deploy.Spec.Template.Spec, p.VolumeName)
	if secretName == "" {
		return false, nil
	}

	key := client.ObjectKey{Namespace: p.DeploymentKey.Namespace, Name: secretName}
	var secret corev1.Secret
	if err := p.Children.Get(ctx, key, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("fetching projection Secret %s: %w", key, err)
	}
	// An absent data key is nil bytes, which carries no header.
	return SectionPresent(secret.Data[p.DataKey], p.SectionHeader), nil
}
