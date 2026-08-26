// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import commonv1 "github.com/c5c3/cobaltcore/internal/common/types"

// The images the operator resolves when a CR names none. Resolving them here
// rather than in the defaulting webhook keeps a CR that leaves spec.image unset
// tracking the operator's tested image across upgrades, instead of freezing
// today's tag into stored state.
const (
	// defaultOVNRepository holds ovsdb-server, ovn-northd and the relay: all
	// three ship in the one OVN image, so one repository covers the whole
	// control plane.
	defaultOVNRepository = "ghcr.io/c5c3/ovn"

	// Renovate-tracked: the customManager in renovate.json bumps this beside ARG OVN_VERSION in images/ovn/Dockerfile.
	defaultOVNVersion = "26.03.2"

	// defaultBackupShifterRepository holds the backup-shifter image the backup
	// CronJob uploads its snapshots with.
	defaultBackupShifterRepository = "ghcr.io/c5c3/backup-shifter"

	// defaultBackupShifterTag is the floating tag the backup-shifter image
	// carries. The image follows no OpenStack release, so it has no version axis
	// to pin to, and the merge job in .github/workflows/build-images.yaml pushes
	// only ":latest" and ":<commit sha>" — neither of which the operator can name
	// ahead of the merge that produces it.
	//
	// The tag is mutable and kubelet defaults imagePullPolicy to Always for
	// ":latest", so every CronJob firing runs whatever main last pushed, with the
	// S3 credentials in its environment and both database snapshots on its
	// volume. Pin spec.backup.s3.image to a digest to opt out of that until the
	// default here is one.
	defaultBackupShifterTag = "latest"
)

// effectiveImage resolves the OVN image the control-plane workloads run. A nil
// override resolves the operator default; an override is returned verbatim, so a
// digest pin reaches the workloads unchanged.
func effectiveImage(override *commonv1.ImageSpec) commonv1.ImageSpec {
	if override == nil {
		return commonv1.ImageSpec{Repository: defaultOVNRepository, Tag: defaultOVNVersion}
	}
	return *override
}

// effectiveShifterImage resolves the backup-shifter image the upload step of the
// backup CronJob runs, on the same nil-resolves-the-default rule as
// effectiveImage.
func effectiveShifterImage(override *commonv1.ImageSpec) commonv1.ImageSpec {
	if override == nil {
		return commonv1.ImageSpec{Repository: defaultBackupShifterRepository, Tag: defaultBackupShifterTag}
	}
	return *override
}
