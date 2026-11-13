// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Unit tests for the Glance / GlanceBackend watch mappers. These plain
// handler.MapFunc closures are exercised directly against a pre-indexed fake
// client, mirroring keystone_watches_test.go.
package controller

import (
	"context"
	"fmt"
	"testing"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/v26/api/v1alpha1"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/c5c3/cobaltcore/internal/common/cache"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	glancev1alpha1 "github.com/c5c3/cobaltcore/operators/glance/api/v1alpha1"
)

// namedSecret returns a bare Secret object for a watch event (no data needed —
// the mappers key off the name and namespace only).
func namedSecret(name string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}}
}

// glanceRequest is the reconcile request for the shared test-glance fixture.
var glanceRequest = reconcile.Request{
	NamespacedName: types.NamespacedName{Namespace: "default", Name: "test-glance"},
}

// --- secretToGlanceWithBackendsMapper ---

func TestSecretToGlanceWithBackendsMapper_GlanceReferencedSecret(t *testing.T) {
	g := NewGomegaWithT(t)

	c := newMapperFakeClient(testGlance())
	mapper := secretToGlanceWithBackendsMapper(c)

	// spec.serviceUser.secretRef.name of test-glance.
	reqs := mapper(context.Background(), namedSecret("glance-service-user"))
	g.Expect(reqs).To(ConsistOf(glanceRequest))

	// spec.database.secretRef.name of test-glance.
	reqs = mapper(context.Background(), namedSecret("glance-db"))
	g.Expect(reqs).To(ConsistOf(glanceRequest))
}

func TestSecretToGlanceWithBackendsMapper_BackendSecretWakesParentGlance(t *testing.T) {
	g := NewGomegaWithT(t)

	backend := testGlanceBackend("store", "test-glance")
	c := newMapperFakeClient(testGlance(), backend)
	mapper := secretToGlanceWithBackendsMapper(c)

	// The credentials Secret is referenced only by the backend, so only the
	// backend leg produces the request — for the backend's parent Glance.
	reqs := mapper(context.Background(), namedSecret("store-s3-creds"))
	g.Expect(reqs).To(ConsistOf(glanceRequest))
}

func TestSecretToGlanceWithBackendsMapper_UnionsAndDeduplicates(t *testing.T) {
	g := NewGomegaWithT(t)

	// A Secret referenced by BOTH the Glance spec (serviceUser) and a backend
	// attached to that same Glance must yield exactly one request.
	glance := testGlance()
	dualBackend := testGlanceBackend("dual", "test-glance")
	dualBackend.Spec.S3.CredentialsSecretRef.Name = "glance-service-user"
	c := newMapperFakeClient(glance, dualBackend)

	reqs := secretToGlanceWithBackendsMapper(c)(context.Background(), namedSecret("glance-service-user"))
	g.Expect(reqs).To(ConsistOf(glanceRequest))
}

func TestSecretToGlanceWithBackendsMapper_UnreferencedSecretEnqueuesNothing(t *testing.T) {
	g := NewGomegaWithT(t)

	c := newMapperFakeClient(testGlance(), testGlanceBackend("store", "test-glance"))
	reqs := secretToGlanceWithBackendsMapper(c)(context.Background(), namedSecret("unrelated"))
	g.Expect(reqs).To(BeEmpty())
}

// --- glanceBackendToGlanceMapper ---

func TestGlanceBackendToGlanceMapper_EnqueuesParent(t *testing.T) {
	g := NewGomegaWithT(t)
	mapper := glanceBackendToGlanceMapper()

	backend := testGlanceBackend("store", "test-glance")
	g.Expect(mapper(context.Background(), backend)).To(ConsistOf(glanceRequest))

	// An empty glanceRef (bypassed admission) enqueues nothing rather than a
	// request with an empty name.
	empty := testGlanceBackend("broken", "test-glance")
	empty.Spec.GlanceRef.Name = ""
	g.Expect(mapper(context.Background(), empty)).To(BeEmpty())
}

// --- glanceToGlanceBackendsMapper ---

func TestGlanceToGlanceBackendsMapper_FansOutToAttachedBackends(t *testing.T) {
	g := NewGomegaWithT(t)

	attached1 := testGlanceBackend("a", "test-glance")
	attached2 := testGlanceBackend("b", "test-glance")
	foreign := testGlanceBackend("c", "another-glance")
	c := newMapperFakeClient(attached1, attached2, foreign)

	reqs := glanceToGlanceBackendsMapper(c)(context.Background(), testGlance())
	g.Expect(reqs).To(ConsistOf(
		reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "a"}},
		reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "b"}},
	))

	// A Glance with no attached backends fans out to nothing.
	other := testGlance()
	other.Name = "unattached"
	g.Expect(glanceToGlanceBackendsMapper(c)(context.Background(), other)).To(BeEmpty())
}

func TestGlanceToGlanceBackendsMapper_ListErrorReturnsNil(t *testing.T) {
	g := NewGomegaWithT(t)

	c := glanceFakeClientBuilder(testGlanceBackend("a", "test-glance")).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*glancev1alpha1.GlanceBackendList); ok {
					return fmt.Errorf("simulated list error")
				}
				return cl.List(ctx, list, opts...)
			},
		}).
		Build()

	reqs := glanceToGlanceBackendsMapper(c)(context.Background(), testGlance())
	g.Expect(reqs).To(BeEmpty(), "a List error logs and returns nil")
}

// TestMaxScaleToGlanceMapper_EnqueuesTheCRsOfItsMariaDB verifies a MaxScale
// event enqueues the Glances whose clusterRef names the MariaDB the MaxScale's
// spec.mariaDbRef names, and nothing for a brownfield Glance, a MaxScale of
// another MariaDB, or one without mariaDbRef.
func TestMaxScaleToGlanceMapper_EnqueuesTheCRsOfItsMariaDB(t *testing.T) {
	g := NewGomegaWithT(t)

	managed := testGlance()
	managed.Spec.Database.ClusterRef = &corev1.LocalObjectReference{Name: "openstack-db"}
	brownfield := testGlance()
	brownfield.Name = "brownfield-glance"
	mapper := maxScaleToGlanceMapper(newMapperFakeClient(managed, brownfield))

	maxScaleOf := func(mariaDB string) *mariadbv1alpha1.MaxScale {
		mxs := &mariadbv1alpha1.MaxScale{ObjectMeta: metav1.ObjectMeta{Name: "openstack-db-maxscale", Namespace: "default"}}
		if mariaDB != "" {
			mxs.Spec.MariaDBRef = &mariadbv1alpha1.MariaDBRef{ObjectReference: mariadbv1alpha1.ObjectReference{Name: mariaDB}}
		}
		return mxs
	}
	g.Expect(mapper(context.Background(), maxScaleOf("openstack-db"))).To(ConsistOf(glanceRequest))
	g.Expect(mapper(context.Background(), maxScaleOf("unrelated-db"))).To(BeEmpty())
	g.Expect(mapper(context.Background(), maxScaleOf(""))).To(BeEmpty(),
		"a MaxScale without mariaDbRef concerns no Glance")
}

// TestMemcachedToGlanceMapper verifies a Memcached event enqueues exactly the
// Glance CRs in its namespace whose spec.cache.clusterRef names it, and leaves
// brownfield caches, other namespaces and other names alone.
func TestMemcachedToGlanceMapper(t *testing.T) {
	withCache := func(name, namespace string, spec commonv1.CacheSpec) *glancev1alpha1.Glance {
		cr := testGlance()
		cr.Name, cr.Namespace = name, namespace
		cr.Spec.Cache = spec
		return cr
	}
	managed := func(name string) commonv1.CacheSpec {
		return commonv1.CacheSpec{ClusterRef: &corev1.LocalObjectReference{Name: name}}
	}
	c := glanceFakeClientBuilder(
		withCache("match", "default", managed("mc")),
		withCache("brownfield", "default", commonv1.CacheSpec{Servers: []string{"mc:11211"}}),
		withCache("other-ns", "elsewhere", managed("mc")),
		withCache("other-name", "default", managed("other-mc")),
	).Build()
	mc := cache.NewMemcached()
	mc.SetNamespace("default")
	mc.SetName("mc")

	reqs := memcachedToGlanceMapper(c)(context.Background(), mc)

	g := NewGomegaWithT(t)
	g.Expect(reqs).To(ConsistOf(reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "match"},
	}))
}
