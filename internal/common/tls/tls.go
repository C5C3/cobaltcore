// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package tls

import (
	"context"
	"fmt"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/forge/internal/common/multicluster"
)

// EnsureCertificate creates a cert-manager Certificate if it does not exist or
// updates its spec if it already exists. It returns (true, nil) when the
// Certificate has a Ready condition with status True, (false, nil) when it
// exists but is not yet ready, and (false, error) on unexpected failures
func EnsureCertificate(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, cert *certmanagerv1.Certificate) (bool, error) {
	existing := &certmanagerv1.Certificate{}
	err := c.Get(ctx, client.ObjectKeyFromObject(cert), existing)

	if apierrors.IsNotFound(err) {
		if err := multicluster.Claim(c, scheme, owner, cert); err != nil {
			return false, fmt.Errorf("setting owner reference on Certificate %s/%s: %w", cert.Namespace, cert.Name, err)
		}
		if err := c.Create(ctx, cert); err != nil {
			return false, fmt.Errorf("creating Certificate %s/%s: %w", cert.Namespace, cert.Name, err)
		}
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("getting Certificate %s/%s: %w", cert.Namespace, cert.Name, err)
	}

	// The same claim the create branch makes, on the object that is actually
	// about to be rewritten. A Certificate of this name somebody else provisioned
	// is refused rather than adopted: its spec would be replaced and reissued
	// under the operator's issuer, dnsNames and secretName, breaking the workload
	// that depends on it. On a target cluster the claim is also what stamps the
	// ownership labels, so a Certificate this operator does own is picked up by
	// the teardown sweep.
	before := existing.DeepCopy()
	if err := multicluster.Claim(c, scheme, owner, existing); err != nil {
		return false, err
	}

	// The claim mutates metadata in place, and the Update below is the only thing
	// that carries those mutations to the cluster. A Certificate written before
	// ownership moved to the labels still has its spec, so a spec-only gate would
	// recompute the claim and throw it away on every pass: the dangling controller
	// reference would stay on the object, and the target's garbage collector
	// resolves it to a missing owner and collects the Certificate as an orphan the
	// moment the owner's kind is registered there.
	claimed := !apiequality.Semantic.DeepEqual(before.ObjectMeta, existing.ObjectMeta)

	if claimed || !apiequality.Semantic.DeepEqual(existing.Spec, cert.Spec) {
		existing.Spec = cert.Spec
		if err := c.Update(ctx, existing); err != nil {
			return false, fmt.Errorf("updating Certificate %s/%s: %w", cert.Namespace, cert.Name, err)
		}
		// Re-fetch to avoid evaluating stale status from before the spec
		// update.
		if err := c.Get(ctx, client.ObjectKeyFromObject(cert), existing); err != nil {
			return false, fmt.Errorf("re-fetching Certificate %s/%s after update: %w", cert.Namespace, cert.Name, err)
		}
	}

	return isCertificateReady(existing), nil
}

// isCertificateReady returns true if the Certificate has a Ready condition
// with status True.
func isCertificateReady(cert *certmanagerv1.Certificate) bool {
	for _, cond := range cert.Status.Conditions {
		if cond.Type == certmanagerv1.CertificateConditionReady && cond.Status == cmmeta.ConditionTrue {
			return true
		}
	}
	return false
}
