// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"errors"
	"testing"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

// The mapper tests use corev1.ConfigMap as the stand-in CR type: the mapper
// is generic over the list/object factories, so any registered type works.
const testIndexKey = "spec.secretRefs.name"

func testMapperConfig(withOwnerLeg bool) SecretMapperConfig {
	cfg := SecretMapperConfig{
		IndexKey: testIndexKey,
		NewList:  func() client.ObjectList { return &corev1.ConfigMapList{} },
	}
	if withOwnerLeg {
		cfg.OwnerGroup = "example.c5c3.io"
		cfg.OwnerKind = "ConfigMap"
		cfg.NewObject = func() client.Object { return &corev1.ConfigMap{} }
	}
	return cfg
}

// indexByRefAnnotation indexes ConfigMaps by their "secret-ref" annotation,
// simulating the per-operator secret-name extractor.
func indexByRefAnnotation(obj client.Object) []string {
	if ref, ok := obj.GetAnnotations()["secret-ref"]; ok && ref != "" {
		return []string{ref}
	}
	return nil
}

func testCM(name, namespace, secretRef string) *corev1.ConfigMap {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	if secretRef != "" {
		cm.Annotations = map[string]string{"secret-ref": secretRef}
	}
	return cm
}

func testSecret(name, namespace string, owners ...metav1.OwnerReference) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: namespace, OwnerReferences: owners,
	}}
}

func TestSecretToOwnersMapper_IndexHit(t *testing.T) {
	g := gomega.NewWithT(t)

	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).
		WithObjects(testCM("cr-a", "ns1", "db-secret"), testCM("cr-b", "ns1", "other"), testCM("cr-c", "ns2", "db-secret")).
		WithIndex(&corev1.ConfigMap{}, testIndexKey, indexByRefAnnotation).
		Build()

	requests := SecretToOwnersMapper(c, testMapperConfig(false))(context.Background(), testSecret("db-secret", "ns1"))

	g.Expect(requests).To(gomega.ConsistOf(
		reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "ns1", Name: "cr-a"}},
	), "only the same-namespace CR referencing the Secret may be enqueued")
}

func TestSecretToOwnersMapper_NoMatchesReturnsNil(t *testing.T) {
	g := gomega.NewWithT(t)

	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).
		WithIndex(&corev1.ConfigMap{}, testIndexKey, indexByRefAnnotation).
		Build()

	requests := SecretToOwnersMapper(c, testMapperConfig(false))(context.Background(), testSecret("unreferenced", "ns1"))

	g.Expect(requests).To(gomega.BeNil())
}

// The owner-ref leg matches on the API group only — not the exact APIVersion
// — so Secrets persisted with an older APIVersion keep resolving after a
// version bump.
func TestSecretToOwnersMapper_OwnerRefGroupOnlyMatch(t *testing.T) {
	g := gomega.NewWithT(t)

	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).
		WithObjects(testCM("owner-cr", "ns1", "")).
		WithIndex(&corev1.ConfigMap{}, testIndexKey, indexByRefAnnotation).
		Build()

	secret := testSecret(
		"staging", "ns1",
		metav1.OwnerReference{APIVersion: "example.c5c3.io/v1beta7", Kind: "ConfigMap", Name: "owner-cr", UID: "u1"},
		// Wrong group: must be ignored.
		metav1.OwnerReference{APIVersion: "other.io/v1", Kind: "ConfigMap", Name: "wrong-group", UID: "u2"},
		// Wrong kind: must be ignored.
		metav1.OwnerReference{APIVersion: "example.c5c3.io/v1", Kind: "Other", Name: "wrong-kind", UID: "u3"},
	)

	requests := SecretToOwnersMapper(c, testMapperConfig(true))(context.Background(), secret)

	g.Expect(requests).To(gomega.ConsistOf(
		reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "ns1", Name: "owner-cr"}},
	))
}

// A stale owner-ref whose target no longer exists in the cache is dropped
// instead of enqueueing work for a deleted CR.
func TestSecretToOwnersMapper_StaleOwnerRefDropped(t *testing.T) {
	g := gomega.NewWithT(t)

	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).
		WithIndex(&corev1.ConfigMap{}, testIndexKey, indexByRefAnnotation).
		Build()

	secret := testSecret("staging", "ns1",
		metav1.OwnerReference{APIVersion: "example.c5c3.io/v1", Kind: "ConfigMap", Name: "gone", UID: "u1"})

	requests := SecretToOwnersMapper(c, testMapperConfig(true))(context.Background(), secret)

	g.Expect(requests).To(gomega.BeNil())
}

// An empty OwnerKind disables the owner-ref leg entirely: even a matching
// owner reference must not enqueue (the c5c3 index-only shape).
func TestSecretToOwnersMapper_EmptyOwnerKindDisablesLeg(t *testing.T) {
	g := gomega.NewWithT(t)

	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).
		WithObjects(testCM("owner-cr", "ns1", "")).
		WithIndex(&corev1.ConfigMap{}, testIndexKey, indexByRefAnnotation).
		Build()

	secret := testSecret("staging", "ns1",
		metav1.OwnerReference{APIVersion: "example.c5c3.io/v1", Kind: "ConfigMap", Name: "owner-cr", UID: "u1"})

	requests := SecretToOwnersMapper(c, testMapperConfig(false))(context.Background(), secret)

	g.Expect(requests).To(gomega.BeNil())
}

func cmWithRef(name, namespace, ref string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: namespace,
		Annotations: map[string]string{"clusterRef": ref},
	}}
}

func TestClusterRefMapper_MatchesRefInSameNamespace(t *testing.T) {
	g := gomega.NewWithT(t)
	// ConfigMaps stand in for CRs; the clusterRef name lives in an annotation.
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).
		WithObjects(
			cmWithRef("cr-a", "ns1", "mariadb"), // matches
			cmWithRef("cr-b", "ns1", "other"),   // wrong ref
			cmWithRef("cr-c", "ns1", ""),        // no ref
			cmWithRef("cr-d", "ns2", "mariadb"), // wrong namespace
		).Build()

	mapper := ClusterRefMapper(c,
		func() client.ObjectList { return &corev1.ConfigMapList{} },
		func(o client.Object) string { return o.GetAnnotations()["clusterRef"] })

	changed := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "mariadb", Namespace: "ns1"}}
	reqs := mapper(context.Background(), changed)
	g.Expect(reqs).To(gomega.ConsistOf(
		reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "ns1", Name: "cr-a"}},
	))
}

func TestClusterRefMapper_NoMatchReturnsNil(t *testing.T) {
	g := gomega.NewWithT(t)
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).
		WithObjects(cmWithRef("cr-a", "ns1", "mariadb")).Build()

	mapper := ClusterRefMapper(c,
		func() client.ObjectList { return &corev1.ConfigMapList{} },
		func(o client.Object) string { return o.GetAnnotations()["clusterRef"] })

	changed := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "absent", Namespace: "ns1"}}
	g.Expect(mapper(context.Background(), changed)).To(gomega.BeEmpty())
}

// storeRefEffective extracts a store reference from a ConfigMap standing in for
// a CR: annotation "store-kind"/"store-name" model the CR's effective ref.
func storeRefEffective(o client.Object) commonv1.SecretStoreRefSpec {
	a := o.GetAnnotations()
	return commonv1.SecretStoreRefSpec{
		Kind: commonv1.SecretStoreRefKind(a["store-kind"]),
		Name: a["store-name"],
	}
}

func cmWithStore(name, namespace, kind, storeName string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: namespace,
		Annotations: map[string]string{"store-kind": kind, "store-name": storeName},
	}}
}

func TestStoreRefFanOut_ClusterKindEnqueuesMatchingRefs(t *testing.T) {
	g := gomega.NewWithT(t)
	// A cluster-scoped store fans out to every CR pinned to it by name, across
	// namespaces. cr-c pins a different store and must be skipped.
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(
		cmWithStore("cr-a", "ns1", "ClusterSecretStore", "openbao-cluster-store"),
		cmWithStore("cr-b", "ns2", "ClusterSecretStore", "openbao-cluster-store"),
		cmWithStore("cr-c", "ns2", "ClusterSecretStore", "other-store"),
	).Build()

	mapper := StoreRefFanOut(c, commonv1.SecretStoreKindCluster,
		func() client.ObjectList { return &corev1.ConfigMapList{} },
		storeRefEffective)

	changed := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "openbao-cluster-store"}}
	reqs := mapper(context.Background(), changed)
	g.Expect(reqs).To(gomega.ConsistOf(
		reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "ns1", Name: "cr-a"}},
		reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "ns2", Name: "cr-b"}},
	))
}

func TestStoreRefFanOut_ClusterKindIgnoresOtherStoreName(t *testing.T) {
	g := gomega.NewWithT(t)
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(
		cmWithStore("cr-a", "ns1", "ClusterSecretStore", "openbao-cluster-store"),
	).Build()

	mapper := StoreRefFanOut(c, commonv1.SecretStoreKindCluster,
		func() client.ObjectList { return &corev1.ConfigMapList{} },
		storeRefEffective)

	changed := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "unrelated-store"}}
	g.Expect(mapper(context.Background(), changed)).To(gomega.BeEmpty())
}

func TestStoreRefFanOut_NamespacedKindScopesToStoreNamespace(t *testing.T) {
	g := gomega.NewWithT(t)
	// A namespaced store only enqueues the CR in its own namespace that pins it
	// as a namespaced ref. cr-b (ns2) and cr-c (cluster ref) must be skipped.
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(
		cmWithStore("cr-a", "ns1", "SecretStore", "openbao-tenant-store"),
		cmWithStore("cr-b", "ns2", "SecretStore", "openbao-tenant-store"),
		cmWithStore("cr-c", "ns1", "ClusterSecretStore", "openbao-tenant-store"),
	).Build()

	mapper := StoreRefFanOut(c, commonv1.SecretStoreKindNamespaced,
		func() client.ObjectList { return &corev1.ConfigMapList{} },
		storeRefEffective)

	changed := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "openbao-tenant-store", Namespace: "ns1"}}
	reqs := mapper(context.Background(), changed)
	g.Expect(reqs).To(gomega.ConsistOf(
		reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "ns1", Name: "cr-a"}},
	))
}

func TestStoreRefFanOut_NamespacedKindIgnoresForeignNamespace(t *testing.T) {
	g := gomega.NewWithT(t)
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(
		cmWithStore("cr-a", "ns1", "SecretStore", "openbao-tenant-store"),
	).Build()

	mapper := StoreRefFanOut(c, commonv1.SecretStoreKindNamespaced,
		func() client.ObjectList { return &corev1.ConfigMapList{} },
		storeRefEffective)

	// The store event is in ns2, where no CR pins it — the ns1 CR must not fire.
	changed := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "openbao-tenant-store", Namespace: "ns2"}}
	g.Expect(mapper(context.Background(), changed)).To(gomega.BeEmpty())
}

// TestSecretToOwnersMapper_AllNamespaces pins the cross-namespace secret watch: a
// CR that places its services (and their secret material) in namespaces of their
// own references Secrets outside its own namespace, so the default
// namespace-scoped List would look for it where it does not live and silently
// drop the event.
func TestSecretToOwnersMapper_AllNamespaces(t *testing.T) {
	g := gomega.NewWithT(t)

	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).
		WithObjects(testCM("cr-a", "ns1", "db-secret"), testCM("cr-b", "ns2", "other")).
		WithIndex(&corev1.ConfigMap{}, testIndexKey, indexByRefAnnotation).
		Build()

	// The Secret lives in ns2; the CR referencing it lives in ns1.
	secret := testSecret("db-secret", "ns2")

	scoped := SecretToOwnersMapper(c, testMapperConfig(false))(context.Background(), secret)
	g.Expect(scoped).To(gomega.BeEmpty(), "the namespace-scoped List cannot see a CR in another namespace")

	cfg := testMapperConfig(false)
	cfg.AllNamespaces = true
	widened := SecretToOwnersMapper(c, cfg)(context.Background(), secret)
	g.Expect(widened).To(gomega.ConsistOf(
		reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "ns1", Name: "cr-a"}},
	))
}

// The satellite tests reuse corev1.ConfigMap as the stand-in satellite CR: the
// "parent" annotation models its parent reference, the "secret-ref" annotation
// (shared with the Secret-mapper tests above) the Secret it consumes.
const testParentIndexKey = "spec.parentRef.name"

// parentOfConfigMap extracts a satellite's parent reference, mirroring the
// per-operator accessor shape: a wrong-type object has no parent.
func parentOfConfigMap(o client.Object) string {
	cm, ok := o.(*corev1.ConfigMap)
	if !ok {
		return ""
	}
	return cm.Annotations["parent"]
}

func cmSatellite(name, namespace, parent, secretRef string) *corev1.ConfigMap {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Annotations: map[string]string{}},
	}
	if parent != "" {
		cm.Annotations["parent"] = parent
	}
	if secretRef != "" {
		cm.Annotations["secret-ref"] = secretRef
	}
	return cm
}

// satelliteClient builds a fake client with both satellite indexes registered,
// the parent-ref index through the helper under test.
func satelliteClient(objs ...client.Object) client.WithWatch {
	return fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).
		WithObjects(objs...).
		WithIndex(&corev1.ConfigMap{}, testParentIndexKey, ParentRefIndexer(parentOfConfigMap)).
		WithIndex(&corev1.ConfigMap{}, testIndexKey, indexByRefAnnotation).
		Build()
}

// failingIndexer rejects every registration so the error wrapping is testable.
type failingIndexer struct{}

func (failingIndexer) IndexField(context.Context, client.Object, string, client.IndexerFunc) error {
	return errors.New("boom")
}

// recordingIndexer accepts the registration and keeps what it was given.
type recordingIndexer struct {
	key     string
	extract client.IndexerFunc
}

func (r *recordingIndexer) IndexField(_ context.Context, _ client.Object, field string, extract client.IndexerFunc) error {
	r.key = field
	r.extract = extract
	return nil
}

func TestParentRefIndexer(t *testing.T) {
	tests := []struct {
		name string
		obj  client.Object
		want []string
	}{
		{"attached satellite indexes its parent name", cmSatellite("backend", "ns1", "glance", ""), []string{"glance"}},
		{"unattached satellite indexes nothing", cmSatellite("backend", "ns1", "", ""), nil},
		{"object of another type indexes nothing", testSecret("db-secret", "ns1"), nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			g.Expect(ParentRefIndexer(parentOfConfigMap)(tt.obj)).To(gomega.Equal(tt.want))
		})
	}
}

func TestRegisterParentRefIndex_WrapsError(t *testing.T) {
	t.Run("a rejected registration is wrapped with the index key", func(t *testing.T) {
		g := gomega.NewWithT(t)

		err := RegisterParentRefIndex(context.Background(), failingIndexer{},
			&corev1.ConfigMap{}, testParentIndexKey, parentOfConfigMap)

		g.Expect(err).To(gomega.MatchError(`registering field indexer "spec.parentRef.name": boom`))
	})

	t.Run("an accepted registration passes the parent-ref indexer through", func(t *testing.T) {
		g := gomega.NewWithT(t)
		indexer := &recordingIndexer{}

		err := RegisterParentRefIndex(context.Background(), indexer,
			&corev1.ConfigMap{}, testParentIndexKey, parentOfConfigMap)

		g.Expect(err).To(gomega.Succeed())
		g.Expect(indexer.key).To(gomega.Equal(testParentIndexKey))
		g.Expect(indexer.extract).NotTo(gomega.BeNil())
		g.Expect(indexer.extract(cmSatellite("backend", "ns1", "glance", ""))).To(gomega.Equal([]string{"glance"}))
	})
}

func TestSatelliteToParentMapper(t *testing.T) {
	tests := []struct {
		name string
		obj  client.Object
		want []reconcile.Request
	}{
		{
			name: "an attached satellite enqueues its parent in its own namespace",
			obj:  cmSatellite("backend", "ns1", "glance", ""),
			want: []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: "ns1", Name: "glance"}}},
		},
		{
			name: "an unattached satellite enqueues nothing",
			obj:  cmSatellite("backend", "ns1", "", ""),
			want: nil,
		},
		{
			name: "an object of another type enqueues nothing",
			obj:  testSecret("db-secret", "ns1"),
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			g.Expect(SatelliteToParentMapper(parentOfConfigMap)(context.Background(), tt.obj)).To(gomega.Equal(tt.want))
		})
	}
}

func TestParentToSatellitesMapper(t *testing.T) {
	const listErrMsg = "listing satellites for parent watch"
	parent := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "glance", Namespace: "ns1"}}

	t.Run("every attached satellite in the parent's namespace is enqueued", func(t *testing.T) {
		g := gomega.NewWithT(t)
		c := satelliteClient(
			cmSatellite("backend-a", "ns1", "glance", ""), // matches
			cmSatellite("backend-b", "ns1", "other", ""),  // another parent
			cmSatellite("backend-c", "ns1", "", ""),       // unattached
			cmSatellite("backend-d", "ns2", "glance", ""), // another namespace
		)

		mapper := ParentToSatellitesMapper(c,
			func() client.ObjectList { return &corev1.ConfigMapList{} }, testParentIndexKey, listErrMsg)

		g.Expect(mapper(context.Background(), parent)).To(gomega.ConsistOf(
			reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "ns1", Name: "backend-a"}},
		))
	})

	// An empty non-nil slice keeps the mapper's contract distinguishable from
	// the error arm below, which returns nil.
	t.Run("a parent without satellites yields an empty non-nil slice", func(t *testing.T) {
		g := gomega.NewWithT(t)
		c := satelliteClient(cmSatellite("backend-a", "ns1", "other", ""))

		mapper := ParentToSatellitesMapper(c,
			func() client.ObjectList { return &corev1.ConfigMapList{} }, testParentIndexKey, listErrMsg)

		requests := mapper(context.Background(), parent)
		g.Expect(requests).To(gomega.BeEmpty())
		g.Expect(requests).NotTo(gomega.BeNil())
	})

	t.Run("a List error enqueues nothing", func(t *testing.T) {
		g := gomega.NewWithT(t)
		c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).
			WithIndex(&corev1.ConfigMap{}, testParentIndexKey, ParentRefIndexer(parentOfConfigMap)).
			WithInterceptorFuncs(interceptor.Funcs{
				List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
					return errors.New("boom")
				},
			}).Build()

		mapper := ParentToSatellitesMapper(c,
			func() client.ObjectList { return &corev1.ConfigMapList{} }, testParentIndexKey, listErrMsg)

		g.Expect(mapper(context.Background(), parent)).To(gomega.BeNil())
	})
}

func TestSecretToParentsViaSatellitesMapper(t *testing.T) {
	const listErrMsg = "listing satellites for Secret watch"
	secret := testSecret("db-secret", "ns1")
	baseRequest := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "ns1", Name: "glance"}}
	base := func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{baseRequest}
	}
	newList := func() client.ObjectList { return &corev1.ConfigMapList{} }

	// The base leg already enqueues "glance"; the satellite attached to it must
	// not enqueue it a second time, while the satellite of another parent adds
	// exactly one request.
	t.Run("the union with the base requests carries no duplicate parent", func(t *testing.T) {
		g := gomega.NewWithT(t)
		c := satelliteClient(
			cmSatellite("backend-a", "ns1", "glance", "db-secret"),
			cmSatellite("backend-b", "ns1", "other-glance", "db-secret"),
			cmSatellite("backend-c", "ns1", "third-glance", "other-secret"), // another Secret
		)

		requests := SecretToParentsViaSatellitesMapper(base, c, newList, testIndexKey, listErrMsg, parentOfConfigMap)(
			context.Background(), secret)

		g.Expect(requests).To(gomega.ConsistOf(
			baseRequest,
			reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "ns1", Name: "other-glance"}},
		))
		g.Expect(requests).To(gomega.HaveLen(2))
	})

	t.Run("a satellite without a parent reference is skipped", func(t *testing.T) {
		g := gomega.NewWithT(t)
		c := satelliteClient(cmSatellite("backend-a", "ns1", "", "db-secret"))

		requests := SecretToParentsViaSatellitesMapper(base, c, newList, testIndexKey, listErrMsg, parentOfConfigMap)(
			context.Background(), secret)

		g.Expect(requests).To(gomega.ConsistOf(baseRequest))
	})

	t.Run("a List error returns the base requests unchanged", func(t *testing.T) {
		g := gomega.NewWithT(t)
		c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).
			WithIndex(&corev1.ConfigMap{}, testIndexKey, indexByRefAnnotation).
			WithInterceptorFuncs(interceptor.Funcs{
				List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
					return errors.New("boom")
				},
			}).Build()

		requests := SecretToParentsViaSatellitesMapper(base, c, newList, testIndexKey, listErrMsg, parentOfConfigMap)(
			context.Background(), secret)

		g.Expect(requests).To(gomega.ConsistOf(baseRequest))
	})
}
