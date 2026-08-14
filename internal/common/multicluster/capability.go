// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package multicluster

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CapabilityProbeFailed is the condition reason every service operator sets
// when the capability probe itself fails, that is, when ChildrenServeKind
// answers with an error instead of a verdict. It sits one step past
// TargetClusterUnavailable: the cluster resolved, and what could not be
// established is whether it serves the kind. Sharing the reason
// keeps the failure recognizable across operators, whatever the CR's condition
// type happens to be.
const CapabilityProbeFailed = "CapabilityProbeFailed"

// ChildrenServeKind reports whether the cluster the children client writes
// to serves gvk. Local children answer from localAvailable, the latch the
// caller probed at setup; remote children are probed live through their
// cluster's RESTMapper on every call, so a CRD installed on a target after
// engagement is seen on the next reconcile.
//
// A remote client without a mapper cannot be asked at all, which is a probe
// failure and not a verdict. Callers act on false by skipping a delete, and
// that inference holds only for a cluster that was asked and answered no —
// answering false for one that could not be asked would silently leave an
// operator-owned child running on the target after the CR stopped claiming it.
// ClusterServesKind collapses the same case into false because its filter
// signature carries no error; here the error is reportable, so it is reported.
//
// A no-match answer is the negative verdict this probe is for. Every other
// error is returned unmodified, because it says nothing about gvk (the
// target's API server briefly unreachable, a 429 under throttling, an
// aggregated APIService reporting Available=False) and belongs in a status
// condition under CapabilityProbeFailed, wrapped once by the caller.
//
// A target that serves the kind costs nothing in steady state: the mapping is
// already in the mapper's cache and no API call is made. A target that does
// not serve it costs one discovery round-trip per probing pass, bounded by the
// watch-driven reconciles plus the operator's sync period (ten minutes by
// default), and only for CRs that name a target cluster.
//
// The mapping probe is spelled out here rather than taken from
// gateway.IsGVKAvailable, which is the same check: the gateway package reaches
// this one through apply and so cannot be imported back.
func ChildrenServeKind(children client.Client, localAvailable bool, gvk schema.GroupVersionKind) (bool, error) {
	if !IsRemote(children) {
		return localAvailable, nil
	}

	mapper := children.RESTMapper()
	if mapper == nil {
		return false, fmt.Errorf("the target cluster's client carries no RESTMapper, so it cannot be probed for %s", gvk)
	}

	if _, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version); err != nil {
		if meta.IsNoMatchError(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
