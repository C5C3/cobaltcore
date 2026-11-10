// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package cache

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

func TestResolveServers(t *testing.T) {
	brownfield := &commonv1.CacheSpec{Servers: []string{"mc-0:11211", "mc-1:11211"}}
	managed := &commonv1.CacheSpec{ClusterRef: &corev1.LocalObjectReference{Name: "memcached"}}

	tests := []struct {
		name    string
		cache   *commonv1.CacheSpec
		managed []string
		want    string
	}{
		{name: "servers without a managed list", cache: brownfield, want: "mc-0:11211,mc-1:11211"},
		{name: "servers ignore a managed list", cache: brownfield, managed: []string{"x:11211"}, want: "mc-0:11211,mc-1:11211"},
		{name: "clusterRef with a managed list", cache: managed, managed: []string{"a:11211", "b:11211"}, want: "a:11211,b:11211"},
		{name: "clusterRef with a nil list", cache: managed, want: "memcached:11211"},
		{name: "clusterRef with an empty list", cache: managed, managed: []string{}, want: "memcached:11211"},
		// Neither mode (rejected at admission, tolerated here) yields empty.
		{name: "neither mode", cache: &commonv1.CacheSpec{}, managed: []string{"a:11211"}, want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gomega.NewWithT(t).Expect(ResolveServers(tc.cache, tc.managed)).To(gomega.Equal(tc.want))
		})
	}
}

func TestNewMemcached(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(NewMemcached().GroupVersionKind()).To(gomega.Equal(MemcachedGVK))
	g.Expect(NewMemcached().Object).NotTo(gomega.HaveKey("status"))
}

// memcached returns a Memcached ns/mc with the given status; a nil status
// leaves the field out.
func memcached(status map[string]any) *unstructured.Unstructured {
	u := NewMemcached()
	u.SetNamespace("ns")
	u.SetName("mc")
	if status != nil {
		u.Object["status"] = status
	}
	return u
}

// stubReader counts Get calls and answers each one with err, or with obj when
// err is nil.
type stubReader struct {
	client.Reader
	obj  *unstructured.Unstructured
	err  error
	gets int
}

func (s *stubReader) Get(_ context.Context, _ client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	s.gets++
	if s.err != nil {
		return s.err
	}
	s.obj.DeepCopyInto(obj.(*unstructured.Unstructured))
	return nil
}

// fakeReader returns a fake client holding objs, on a scheme of its own. A
// fake client built without WithScheme uses client-go's global scheme, and
// its Get registers the unstructured Memcached kind there, so parallel
// subtests would write one shared map.
func fakeReader(objs ...client.Object) client.Reader {
	return fake.NewClientBuilder().WithScheme(runtime.NewScheme()).WithObjects(objs...).Build()
}

func managedSpec() *commonv1.CacheSpec {
	return &commonv1.CacheSpec{ClusterRef: &corev1.LocalObjectReference{Name: "mc"}}
}

func TestManagedServers_NoClusterRefIssuesNoGet(t *testing.T) {
	g := gomega.NewWithT(t)
	r := &stubReader{err: errors.New("must not be called")}

	got, err := ManagedServers(t.Context(), r, "ns", &commonv1.CacheSpec{Servers: []string{"mc-0:11211"}})

	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(got).To(gomega.BeNil())
	g.Expect(r.gets).To(gomega.BeZero())
}

func TestManagedServers_FallsBackWithoutAList(t *testing.T) {
	tests := []struct {
		name   string
		reader client.Reader
	}{
		{name: "Memcached not found", reader: fakeReader()},
		{name: "kind not served", reader: &stubReader{err: &meta.NoKindMatchError{
			GroupKind: MemcachedGVK.GroupKind(), SearchedVersions: []string{MemcachedGVK.Version},
		}}},
		{name: "no status", reader: fakeReader(memcached(nil))},
		{name: "status without serverList", reader: fakeReader(memcached(map[string]any{"readyReplicas": int64(1)}))},
		{name: "empty serverList", reader: fakeReader(memcached(map[string]any{"serverList": []any{}}))},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := gomega.NewWithT(t)
			got, err := ManagedServers(t.Context(), tc.reader, "ns", managedSpec())
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(got).To(gomega.BeNil())
		})
	}
}

func TestManagedServers_GetErrorIsWrapped(t *testing.T) {
	g := gomega.NewWithT(t)

	_, err := ManagedServers(t.Context(), &stubReader{err: context.DeadlineExceeded}, "ns", managedSpec())

	g.Expect(err).To(gomega.MatchError(context.DeadlineExceeded))
	g.Expect(err.Error()).To(gomega.HavePrefix("getting Memcached ns/mc: "))
}

func TestManagedServers_ServerListNotAStringList(t *testing.T) {
	g := gomega.NewWithT(t)
	r := &stubReader{obj: memcached(map[string]any{"serverList": "mc-0:11211"})}

	_, err := ManagedServers(t.Context(), r, "ns", managedSpec())

	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("reading status.serverList of Memcached ns/mc: ")))
}

func TestManagedServers_RejectsInvalidEntries(t *testing.T) {
	for _, entry := range []string{
		"",
		"a",
		"a:0",
		"a:65536",
		"a:11211\nauth_url = x",
		"a:11211,b:11211",
		"a b:11211",
		"a:-1",
		"a:",
		"-a:11211",
		"[::1]:11211",
	} {
		t.Run(entry, func(t *testing.T) {
			t.Parallel()
			g := gomega.NewWithT(t)
			r := &stubReader{obj: memcached(map[string]any{"serverList": []any{"mc-0.mc.ns.svc:11211", entry}})}

			got, err := ManagedServers(t.Context(), r, "ns", managedSpec())

			g.Expect(got).To(gomega.BeNil())
			g.Expect(err).To(gomega.MatchError(
				`reading Memcached ns/mc: status.serverList[1] ` + strconv.Quote(entry) + ` is not a <dns-name>:<port> entry`))
		})
	}
}

func TestManagedServers_SortsAndDeduplicates(t *testing.T) {
	g := gomega.NewWithT(t)
	c := fakeReader(memcached(map[string]any{
		"serverList": []any{"x-1.x.ns.svc:11211", "x-0.x.ns.svc:11211", "x-1.x.ns.svc:11211"},
	}))

	got, err := ManagedServers(t.Context(), c, "ns", managedSpec())

	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(got).To(gomega.Equal([]string{"x-0.x.ns.svc:11211", "x-1.x.ns.svc:11211"}))
}

func TestManagedServers_ReadsTheReferencedNamespace(t *testing.T) {
	g := gomega.NewWithT(t)
	c := fakeReader(memcached(map[string]any{
		"serverList": []any{"mc-0.mc.ns.svc:11211"},
	}))

	got, err := ManagedServers(t.Context(), c, "other", managedSpec())

	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(got).To(gomega.BeNil(), "a Memcached of the same name in another namespace must not be read")
}
