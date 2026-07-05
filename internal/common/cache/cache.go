// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package cache resolves the shared commonv1.CacheSpec into the memcache
// endpoint list the oslo.cache (and Django CACHES) configuration consumes.
package cache

import (
	"fmt"
	"strings"

	commonv1 "github.com/c5c3/forge/internal/common/types"
)

// ResolveServers returns the memcache server list for the shared CacheSpec.
// In brownfield mode (Servers set), it joins them with commas. In managed
// mode (ClusterRef set), it constructs the endpoint from the cluster name:
// the memcached operator provisions a Deployment + headless Service, and the
// Service DNS name resolves to all pod IPs. An empty spec (neither mode)
// yields an empty string; the webhooks reject that shape at admission.
func ResolveServers(cache *commonv1.CacheSpec) string {
	if len(cache.Servers) > 0 {
		return strings.Join(cache.Servers, ",")
	}
	if cache.ClusterRef != nil {
		return fmt.Sprintf("%s:11211", cache.ClusterRef.Name)
	}
	return ""
}
