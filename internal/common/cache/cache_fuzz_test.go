// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package cache

import (
	"net"
	"reflect"
	"strconv"
	"strings"
	"testing"

	k8svalidation "k8s.io/apimachinery/pkg/util/validation"
)

// FuzzManagedServers checks that ManagedServers admits a one-entry
// status.serverList exactly when the entry is a DNS subdomain host and a port
// from 1 to 65535, returns the admitted entry unchanged, and never admits an
// entry that would add a line, a key or a second server to the INI value it
// reaches.
func FuzzManagedServers(f *testing.F) {
	// The entries of TestManagedServers_RejectsInvalidEntries, two port edge
	// cases (a signed port, rejected; a zero-padded port, admitted), then valid
	// ones, the lowest and the highest admitted port among them.
	for _, entry := range []string{
		"", "a", "a:0", "a:65536", "a:11211\nauth_url = x", "a:11211,b:11211", "a b:11211",
		"a:-1", "a:", "-a:11211", "[::1]:11211", "a:+1", "a:011211",
		"mc:11211", "a:1", "a:65535", "a-b.c:1",
		"keystone-cache-ring-memcached-0.keystone-cache-ring-memcached.openstack.svc:11211",
	} {
		f.Add(entry)
	}

	f.Fuzz(func(t *testing.T, entry string) {
		r := &stubReader{obj: memcached(map[string]any{"serverList": []any{entry}})}
		got, err := ManagedServers(t.Context(), r, "ns", managedSpec())
		again, againErr := ManagedServers(t.Context(), r, "ns", managedSpec())
		if !reflect.DeepEqual(again, got) || (againErr == nil) != (err == nil) {
			t.Fatalf("determinism: ManagedServers(%q) returned %v, %v, then %v, %v", entry, got, err, again, againErr)
		}

		host, port, splitErr := net.SplitHostPort(entry)
		n, portErr := strconv.ParseUint(port, 10, 16)
		want := splitErr == nil && len(k8svalidation.IsDNS1123Subdomain(host)) == 0 && portErr == nil && n >= 1
		if (err == nil) != want {
			t.Fatalf("admission: ManagedServers(%q) returned error %v, want admitted=%t", entry, err, want)
		}
		if err != nil {
			return
		}
		if !reflect.DeepEqual(got, []string{entry}) {
			t.Fatalf("value: ManagedServers(%q) returned %q, want the entry unchanged", entry, got)
		}
		if strings.ContainsAny(entry, "\n\r,= \t") {
			t.Fatalf("INI safety: ManagedServers admitted %q", entry)
		}
	})
}
