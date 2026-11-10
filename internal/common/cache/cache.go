// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package cache resolves the shared commonv1.CacheSpec into the memcache
// endpoint list the oslo.cache (and Django CACHES) configuration consumes. In
// managed mode the list is the per-pod status.serverList of the referenced
// Memcached, read by ManagedServers.
package cache

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	k8svalidation "k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

// MemcachedGVK is the kind spec.cache.clusterRef names. memcached.c5c3.io
// publishes no Go module, so the object is read and watched as an
// unstructured.Unstructured; the fake client and the API server both accept
// it without a scheme registration.
var MemcachedGVK = schema.GroupVersionKind{Group: "memcached.c5c3.io", Version: "v1beta1", Kind: "Memcached"}

// NewMemcached returns an empty unstructured object of MemcachedGVK, for Get
// and for watches.
func NewMemcached() *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(MemcachedGVK)
	return u
}

// ManagedServers returns the validated, sorted, deduplicated status.serverList
// of the Memcached that cache.ClusterRef names in namespace, or nil when there
// is none to use. A spec without ClusterRef issues no read. A missing
// Memcached, a cluster that does not serve the kind, and an absent or empty
// list all return nil without an error, so the caller renders the Service
// name and never waits for its cache. Any other read error, a list that is not
// a list of strings, and an entry that is not a "<dns-name>:<port>" pair are
// errors: the list is written by another controller and reaches an INI file
// verbatim. The entries are sorted because keystonemiddleware's pool picks a
// server by position, so every service must render them in one order.
func ManagedServers(ctx context.Context, c client.Reader, namespace string, cache *commonv1.CacheSpec) ([]string, error) {
	if cache.ClusterRef == nil {
		return nil, nil
	}
	name := cache.ClusterRef.Name
	fallback := func(reason string) ([]string, error) {
		log.FromContext(ctx).V(1).Info("Memcached publishes no server list; rendering the Service name",
			"memcached", namespace+"/"+name, "reason", reason)
		return nil, nil
	}

	u := NewMemcached()
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, u); err != nil {
		switch {
		case apierrors.IsNotFound(err):
			return fallback("NotFound")
		case meta.IsNoMatchError(err):
			return fallback("KindNotServed")
		}
		return nil, fmt.Errorf("getting Memcached %s/%s: %w", namespace, name, err)
	}

	entries, _, err := unstructured.NestedStringSlice(u.Object, "status", "serverList")
	if err != nil {
		return nil, fmt.Errorf("reading status.serverList of Memcached %s/%s: %w", namespace, name, err)
	}
	if len(entries) == 0 {
		return fallback("EmptyServerList")
	}
	for i, entry := range entries {
		if !validServerEntry(entry) {
			return nil, fmt.Errorf("reading Memcached %s/%s: status.serverList[%d] %q is not a <dns-name>:<port> entry",
				namespace, name, i, entry)
		}
	}
	slices.Sort(entries)
	return slices.Compact(entries), nil
}

// validServerEntry reports whether entry is a "<dns-name>:<port>" pair: a DNS
// subdomain host and a port from 1 to 65535. That excludes the empty string,
// a newline, a carriage return, a comma and a space.
func validServerEntry(entry string) bool {
	host, port, err := net.SplitHostPort(entry)
	if err != nil || len(k8svalidation.IsDNS1123Subdomain(host)) > 0 {
		return false
	}
	n, err := strconv.ParseUint(port, 10, 16)
	return err == nil && n >= 1
}

// ResolveServers returns the comma-joined memcache server list for the shared
// CacheSpec. In brownfield mode (Servers set), it joins Servers and ignores
// managed. In managed mode (ClusterRef set), it joins managed, the per-pod
// list ManagedServers read, and falls back to "<ClusterRef.Name>:11211" while
// managed is empty. A Service name is one server to the clients, which spread
// it over every pod it resolves to, so each pod holds its own copy of an
// entry; only a list of per-pod names hashes a key to one pod. An empty spec
// (neither mode) yields an empty string; the webhooks reject that shape at
// admission.
func ResolveServers(cache *commonv1.CacheSpec, managed []string) string {
	if len(cache.Servers) > 0 {
		return strings.Join(cache.Servers, ",")
	}
	if cache.ClusterRef != nil {
		if len(managed) > 0 {
			return strings.Join(managed, ",")
		}
		return fmt.Sprintf("%s:11211", cache.ClusterRef.Name)
	}
	return ""
}
