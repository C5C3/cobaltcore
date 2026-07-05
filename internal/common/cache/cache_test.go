// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package cache

import (
	"testing"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"

	commonv1 "github.com/c5c3/forge/internal/common/types"
)

func TestResolveServers(t *testing.T) {
	g := gomega.NewWithT(t)

	brownfield := &commonv1.CacheSpec{Servers: []string{"mc-0:11211", "mc-1:11211"}}
	g.Expect(ResolveServers(brownfield)).To(gomega.Equal("mc-0:11211,mc-1:11211"))

	managed := &commonv1.CacheSpec{ClusterRef: &corev1.LocalObjectReference{Name: "memcached"}}
	g.Expect(ResolveServers(managed)).To(gomega.Equal("memcached:11211"))

	// Neither mode (rejected at admission, tolerated here) yields empty.
	g.Expect(ResolveServers(&commonv1.CacheSpec{})).To(gomega.Equal(""))
}
