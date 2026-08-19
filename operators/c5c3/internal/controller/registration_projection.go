// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// This file is the ONE projection layer both registration mechanisms run on:
// the ControlPlane's inline service accounts and managed catalog rows
// (reconcile_serviceaccounts.go, reconcile_catalog.go) and the KeystoneService
// controller's per-CR registrations (keystoneservice_account.go,
// keystoneservice_catalog.go). Only the hardened flows live here — the
// collision probe gate, the generation-scoped password engine, the OpenBao
// publish leg, the ESO nudges, and the K-ORC child spec builders. Each
// mechanism keeps its own orchestration skeleton, its own reporting model, and
// its own condition message texts, and parameterizes the differences that are
// deliberate (shared vs per-CR role imports, the two push-hash annotation keys,
// the two OpenBao path layouts, the two ownership strategies).
//
// It lives in the controller package rather than in internal/common because it
// reads c5c3 API types and package-local helpers (childNamespace,
// effectiveControlPlaneStoreRef, korcAuthURL, buildServiceAccountCloudsYAML),
// and both reconcilers already live here, so nothing needs exporting.

// --- shared vocabulary ---

// serviceAccountPasswordGenerationAnnotation stamps the current password
// generation N onto the managed K-ORC User CR. reconcileServiceAccounts derives
// N from the User's passwordRef suffix and the annotation is the rotation nudge
// marker: the CredentialRotation reconciler CLEARS it to "" to request a rotation
// (mirroring adminPasswordHashAnnotation), and an empty value drives a generation
// bump on the next pass.
const serviceAccountPasswordGenerationAnnotation = "forge.c5c3.io/password-generation" //nolint:gosec // G101 false positive: annotation key, not a credential.

// serviceAccountPasswordKey is the Secret data key the generated password is
// stored under. K-ORC's passwordRef reads exactly this key; it is also the key
// the materialized consumer Secret carries.
const serviceAccountPasswordKey = "password"

// serviceAccountRoleSlugNonAlnum matches every maximal run of characters outside
// [a-z0-9], which serviceAccountRoleSlug collapses to a single "-".
var serviceAccountRoleSlugNonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

// serviceAccountRoleSlug derives a deterministic, name-safe discriminator for a
// role to embed in the Role import / RoleAssignment child CR names. It lowercases
// the role, collapses every run of characters outside [a-z0-9] to a single "-",
// trims leading/trailing "-", truncates the readable base to 16 chars, and appends
// "-" plus the first 8 hex chars of sha256(role). The hash is taken over the
// ORIGINAL role string (before normalization): Keystone role names are
// case-sensitive and up to 255 chars, so hashing the raw value keeps two roles
// that normalize alike ("Member" vs "member", or two long names sharing a 16-char
// prefix) alias-free. The result is at most 25 bytes.
func serviceAccountRoleSlug(role string) string {
	sum := sha256.Sum256([]byte(role))
	suffix := hex.EncodeToString(sum[:])[:8]
	base := strings.Trim(serviceAccountRoleSlugNonAlnum.ReplaceAllString(strings.ToLower(role), "-"), "-")
	if len(base) > 16 {
		base = base[:16]
	}
	return base + "-" + suffix
}

// parseServiceAccountGeneration extracts the generation N from a password Secret
// name of the form "…-password-vN". ok is false when the name carries no such
// suffix or N is not a positive integer.
func parseServiceAccountGeneration(passwordRefName string) (int64, bool) {
	i := strings.LastIndex(passwordRefName, "-password-v")
	if i < 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(passwordRefName[i+len("-password-v"):], 10, 64)
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

// probeVerdict is the interpretation of a collision probe import.
type probeVerdict int

const (
	// probePending — the probe has not resolved either way yet.
	probePending probeVerdict = iota
	// probeResolved — the probe matched an existing OpenStack resource (collision).
	probeResolved
	// probeAbsent — the probe reports the resource does not exist (safe to create).
	probeAbsent
)

func interpretProbe(obj orcv1alpha1.ObjectWithConditions) probeVerdict {
	switch {
	case korcAvailableUpToDate(obj):
		return probeResolved
	case korcImportPendingExternal(obj):
		return probeAbsent
	default:
		return probePending
	}
}

// pendingServiceAccountObjs returns the not-yet-resolved objects among the given
// ones, for the External-mode classifier.
func pendingServiceAccountObjs(objs ...orcv1alpha1.ObjectWithConditions) []orcv1alpha1.ObjectWithConditions {
	var pending []orcv1alpha1.ObjectWithConditions
	for _, obj := range objs {
		if obj != nil && !korcAvailableUpToDate(obj) {
			pending = append(pending, obj)
		}
	}
	return pending
}

// deleteRegistrationChild issues an idempotent Delete on one named child in
// namespace, tolerating NotFound. It drops resolved collision probes and
// superseded password Secrets — neither of which either mechanism's prune sweep
// may touch while the block that owns them is declared. errPrefix names the
// mechanism ("service-account" or "registration") so the failure keeps pointing
// at the path that issued it.
func deleteRegistrationChild(ctx context.Context, c client.Client, obj client.Object, name, namespace, errPrefix string) error {
	obj.SetName(name)
	obj.SetNamespace(namespace)
	if err := client.IgnoreNotFound(c.Delete(ctx, obj)); err != nil {
		return fmt.Errorf("deleting %s child %q: %w", errPrefix, name, err)
	}
	return nil
}
