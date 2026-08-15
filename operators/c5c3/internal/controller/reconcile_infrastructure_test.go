// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the infrastructure sub-reconciler.
package controller

import (
	"context"
	"testing"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	mcruntime "sigs.k8s.io/multicluster-runtime/pkg/multicluster"

	"github.com/c5c3/forge/internal/common/conditions"
	commonmulticluster "github.com/c5c3/forge/internal/common/multicluster"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
)

// infraTestScheme returns a runtime.Scheme registering c5c3, client-go, and the
// mariadb-operator types. Unstructured objects (Memcached) need no registration.
func infraTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("adding client-go scheme: %v", err)
	}
	if err := c5c3v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding c5c3 scheme: %v", err)
	}
	if err := mariadbv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding mariadb scheme: %v", err)
	}
	return s
}

// managedInfraControlPlane builds a ControlPlane whose database and cache are in
// managed mode (ClusterRef set).
func managedInfraControlPlane() *c5c3v1alpha1.ControlPlane {
	return &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "cp",
			Namespace:  "default",
			Generation: 1,
			UID:        types.UID("cp-uid"),
		},
		Spec: c5c3v1alpha1.ControlPlaneSpec{
			OpenStackRelease: "2025.2",
			Region:           "RegionOne",
			Infrastructure: &c5c3v1alpha1.InfrastructureSpec{
				Database: commonv1.DatabaseSpec{
					ClusterRef: &corev1.LocalObjectReference{Name: "openstack-db"},
					Database:   "keystone",
					SecretRef:  commonv1.SecretRefSpec{Name: "keystone-db"},
				},
				Cache: commonv1.CacheSpec{
					ClusterRef: &corev1.LocalObjectReference{Name: "openstack-memcached"},
					Backend:    "dogpile.cache.pymemcache",
					Replicas:   3,
				},
			},
		},
	}
}

func TestReconcileInfrastructure_ManagedProjectsChildren(t *testing.T) {
	g := NewGomegaWithT(t)

	s := infraTestScheme(t)
	cp := managedInfraControlPlane()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileInfrastructure(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	// MariaDB child: correct name + namespace.
	var mariadb mariadbv1alpha1.MariaDB
	g.Expect(c.Get(context.Background(), types.NamespacedName{
		Name:      "openstack-db",
		Namespace: childNamespace(cp),
	}, &mariadb)).To(Succeed(), "MariaDB CR must be created in the openstack namespace")
	g.Expect(mariadb.Spec.Storage.Size).NotTo(BeNil(), "MariaDB must have a storage size (webhook requirement)")

	// MariaDB owner reference points at the ControlPlane.
	g.Expect(mariadb.OwnerReferences).To(HaveLen(1))
	g.Expect(mariadb.OwnerReferences[0].Name).To(Equal("cp"))
	g.Expect(mariadb.OwnerReferences[0].Kind).To(Equal("ControlPlane"))

	// Memcached child: unstructured GVK + name + replicas.
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(memcachedGVK)
	g.Expect(c.Get(context.Background(), types.NamespacedName{
		Name:      "openstack-memcached",
		Namespace: childNamespace(cp),
	}, u)).To(Succeed(), "Memcached CR must be created in the openstack namespace")
	g.Expect(u.GroupVersionKind()).To(Equal(memcachedGVK))

	replicas, found, err := unstructured.NestedInt64(u.Object, "spec", "replicas")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeTrue(), "Memcached spec.replicas must be set")
	g.Expect(replicas).To(Equal(int64(3)))

	// Memcached owner reference points at the ControlPlane.
	g.Expect(u.GetOwnerReferences()).To(HaveLen(1))
	g.Expect(u.GetOwnerReferences()[0].Name).To(Equal("cp"))
}

func TestReconcileInfrastructure_ManagedRequeuesWhileNotReady(t *testing.T) {
	g := NewGomegaWithT(t)

	s := infraTestScheme(t)
	cp := managedInfraControlPlane()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	res, err := r.reconcileInfrastructure(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	// Children freshly created, no Ready status yet -> requeue, condition False.
	g.Expect(res.RequeueAfter).To(Equal(infraRequeueAfter))

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeInfrastructureReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
}

func TestReconcileInfrastructure_ManagedReadyWhenChildrenReady(t *testing.T) {
	g := NewGomegaWithT(t)

	s := infraTestScheme(t)
	cp := managedInfraControlPlane()

	readyMariaDB := &mariadbv1alpha1.MariaDB{
		ObjectMeta: metav1.ObjectMeta{Name: "openstack-db", Namespace: childNamespace(cp)},
		Status: mariadbv1alpha1.MariaDBStatus{
			Conditions: []metav1.Condition{{
				Type:               "Ready",
				Status:             metav1.ConditionTrue,
				Reason:             "Ready",
				Message:            "ready",
				LastTransitionTime: metav1.Now(),
			}},
		},
	}
	readyMemcached := &unstructured.Unstructured{}
	readyMemcached.SetGroupVersionKind(memcachedGVK)
	readyMemcached.SetName("openstack-memcached")
	readyMemcached.SetNamespace(childNamespace(cp))
	_ = unstructured.SetNestedSlice(readyMemcached.Object, []interface{}{
		map[string]interface{}{"type": "Ready", "status": "True"},
	}, "status", "conditions")

	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(cp, readyMariaDB, readyMemcached).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	res, err := r.reconcileInfrastructure(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeInfrastructureReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
}

// TestReconcileInfrastructure_ManagedAdoptsExistingWithoutMutating verifies the
// adoption-safe path: when a MariaDB / Memcached with the clusterRef name already
// exists (e.g. the infrastructure stack provisions "openstack-db" / "openstack-
// memcached" under the same name), reconcileInfrastructure adopts it as-is. It
// must NOT overwrite immutable storage fields (which the mariadb-operator webhook
// rejects) and must NOT claim GC ownership of a resource it did not create.
func TestReconcileInfrastructure_ManagedAdoptsExistingWithoutMutating(t *testing.T) {
	g := NewGomegaWithT(t)

	s := infraTestScheme(t)
	cp := managedInfraControlPlane()

	existingSize := resource.MustParse("1Gi")
	existingMariaDB := &mariadbv1alpha1.MariaDB{
		ObjectMeta: metav1.ObjectMeta{Name: "openstack-db", Namespace: childNamespace(cp)},
		Spec: mariadbv1alpha1.MariaDBSpec{
			Replicas: 1,
			Storage: mariadbv1alpha1.Storage{
				Size:             &existingSize,
				StorageClassName: "standard",
			},
		},
	}
	existingMemcached := &unstructured.Unstructured{}
	existingMemcached.SetGroupVersionKind(memcachedGVK)
	existingMemcached.SetName("openstack-memcached")
	existingMemcached.SetNamespace(childNamespace(cp))
	_ = unstructured.SetNestedField(existingMemcached.Object, int64(1), "spec", "replicas")

	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(cp, existingMariaDB, existingMemcached).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileInfrastructure(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred(), "adopting pre-existing infra must not error")

	// MariaDB: immutable storage preserved, topology untouched, NOT adopted for GC.
	var mariadb mariadbv1alpha1.MariaDB
	g.Expect(c.Get(context.Background(), types.NamespacedName{
		Name: "openstack-db", Namespace: childNamespace(cp),
	}, &mariadb)).To(Succeed())
	g.Expect(mariadb.Spec.Storage.StorageClassName).To(Equal("standard"),
		"existing storageClassName must be preserved (it is immutable)")
	g.Expect(mariadb.Spec.Replicas).To(Equal(int32(1)),
		"existing replica topology must be preserved, not reshaped to the projected default")
	g.Expect(mariadb.OwnerReferences).To(BeEmpty(),
		"must not claim GC ownership of pre-existing infrastructure")

	// Memcached: replicas untouched, NOT adopted for GC.
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(memcachedGVK)
	g.Expect(c.Get(context.Background(), types.NamespacedName{
		Name: "openstack-memcached", Namespace: childNamespace(cp),
	}, u)).To(Succeed())
	replicas, _, _ := unstructured.NestedInt64(u.Object, "spec", "replicas")
	g.Expect(replicas).To(Equal(int64(1)), "existing Memcached replicas must be preserved")
	g.Expect(u.GetOwnerReferences()).To(BeEmpty(),
		"must not claim GC ownership of pre-existing Memcached")
}

// TestEnsureMariaDB_OwnedReconcilesReplicas verifies the owner-aware path: a
// MariaDB this ControlPlane OWNS (created on an earlier pass) has its mutable
// projection — spec.replicas — reconciled back to the projected default when it
// has drifted, while its immutable storage is left untouched. This is what keeps
// a ControlPlane-owned database evolving with the projection without reshaping a
// pre-existing/adopted cluster (which the adoption test covers).
func TestEnsureMariaDB_OwnedReconcilesReplicas(t *testing.T) {
	g := NewGomegaWithT(t)

	s := infraTestScheme(t)
	cp := managedInfraControlPlane()

	existingSize := resource.MustParse("100Gi")
	ownedMariaDB := &mariadbv1alpha1.MariaDB{
		ObjectMeta: metav1.ObjectMeta{Name: "openstack-db", Namespace: childNamespace(cp)},
		Spec: mariadbv1alpha1.MariaDBSpec{
			Replicas: 1, // drifted below the projected default (infraMariaDBReplicasDefault)
			Storage: mariadbv1alpha1.Storage{
				Size:             &existingSize,
				StorageClassName: "standard",
			},
		},
	}
	// Mark the MariaDB as owned by this ControlPlane (controller owner reference).
	g.Expect(controllerutil.SetControllerReference(cp, ownedMariaDB, s)).To(Succeed())

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, ownedMariaDB).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.ensureMariaDB(context.Background(), c, cp, &cp.Spec.Infrastructure.Database, cp.KeystoneNamespace())
	g.Expect(err).NotTo(HaveOccurred())

	var mariadb mariadbv1alpha1.MariaDB
	g.Expect(c.Get(context.Background(), types.NamespacedName{
		Name: "openstack-db", Namespace: childNamespace(cp),
	}, &mariadb)).To(Succeed())
	g.Expect(mariadb.Spec.Replicas).To(Equal(infraMariaDBReplicasDefault),
		"an owned MariaDB must have its replicas reconciled to the projected default")
	g.Expect(mariadb.Spec.Galera).NotTo(BeNil())
	g.Expect(mariadb.Spec.Galera.Enabled).To(BeTrue(),
		"the default 3-replica projection re-enables Galera on an owned MariaDB")
	g.Expect(mariadb.Spec.Storage.StorageClassName).To(Equal("standard"),
		"storage stays immutable even for an owned MariaDB")
}

// TestEnsureMariaDB_OwnedReconcilesGaleraState isolates the Galera-only drift
// case: an owned MariaDB already sits at the projected replica default, but its
// Galera flag has drifted off. ensureMariaDB must flip Galera back on without
// touching the (already-correct) replica count or the immutable storage, proving
// the update triggers on Galera drift alone and not only on a replica mismatch.
func TestEnsureMariaDB_OwnedReconcilesGaleraState(t *testing.T) {
	g := NewGomegaWithT(t)

	s := infraTestScheme(t)
	cp := managedInfraControlPlane() // Database.Replicas defaults to infraMariaDBReplicasDefault

	existingSize := resource.MustParse("100Gi")
	ownedMariaDB := &mariadbv1alpha1.MariaDB{
		ObjectMeta: metav1.ObjectMeta{Name: "openstack-db", Namespace: childNamespace(cp)},
		Spec: mariadbv1alpha1.MariaDBSpec{
			Replicas: infraMariaDBReplicasDefault,             // already at the projected default
			Galera:   &mariadbv1alpha1.Galera{Enabled: false}, // only Galera has drifted off
			Storage: mariadbv1alpha1.Storage{
				Size:             &existingSize,
				StorageClassName: "standard",
			},
		},
	}
	// Mark the MariaDB as owned by this ControlPlane (controller owner reference).
	g.Expect(controllerutil.SetControllerReference(cp, ownedMariaDB, s)).To(Succeed())

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, ownedMariaDB).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.ensureMariaDB(context.Background(), c, cp, &cp.Spec.Infrastructure.Database, cp.KeystoneNamespace())
	g.Expect(err).NotTo(HaveOccurred())

	var mariadb mariadbv1alpha1.MariaDB
	g.Expect(c.Get(context.Background(), types.NamespacedName{
		Name: "openstack-db", Namespace: childNamespace(cp),
	}, &mariadb)).To(Succeed())
	g.Expect(mariadb.Spec.Replicas).To(Equal(infraMariaDBReplicasDefault),
		"replicas already at the default must stay unchanged when only Galera drifted")
	g.Expect(mariadb.Spec.Galera).NotTo(BeNil())
	g.Expect(mariadb.Spec.Galera.Enabled).To(BeTrue(),
		"ensureMariaDB must re-enable Galera when only the Galera flag has drifted on an owned MariaDB")
	g.Expect(mariadb.Spec.Storage.StorageClassName).To(Equal("standard"),
		"storage stays immutable while correcting Galera drift")
}

// TestEnsureMariaDB_ReplicasFromSpec verifies the fresh-create projection honours
// spec.infrastructure.database.replicas: a single replica yields a non-Galera
// MariaDB (so it schedules on a single-node kind), three replicas yield a Galera
// cluster, and a zero value (only reachable when CRD validation is bypassed) is
// floored to the default with Galera enabled. Storage is always the fixed size.
func TestEnsureMariaDB_ReplicasFromSpec(t *testing.T) {
	tests := []struct {
		name         string
		specReplicas int32
		wantReplicas int32
		wantGalera   bool
	}{
		{name: "single replica disables Galera", specReplicas: 1, wantReplicas: 1, wantGalera: false},
		{name: "three replicas enable Galera", specReplicas: 3, wantReplicas: 3, wantGalera: true},
		{name: "zero replicas floored to default", specReplicas: 0, wantReplicas: infraMariaDBReplicasDefault, wantGalera: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)

			s := infraTestScheme(t)
			cp := managedInfraControlPlane()
			cp.Spec.Infrastructure.Database.Replicas = tc.specReplicas
			c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
			r := &ControlPlaneReconciler{Client: c, Scheme: s}

			_, err := r.ensureMariaDB(context.Background(), c, cp, &cp.Spec.Infrastructure.Database, cp.KeystoneNamespace())
			g.Expect(err).NotTo(HaveOccurred())

			var mariadb mariadbv1alpha1.MariaDB
			g.Expect(c.Get(context.Background(), types.NamespacedName{
				Name: "openstack-db", Namespace: childNamespace(cp),
			}, &mariadb)).To(Succeed())
			g.Expect(mariadb.Spec.Replicas).To(Equal(tc.wantReplicas))
			g.Expect(mariadb.Spec.Galera).NotTo(BeNil())
			g.Expect(mariadb.Spec.Galera.Enabled).To(Equal(tc.wantGalera))
			g.Expect(mariadb.Spec.Storage.Size).NotTo(BeNil(),
				"storage size is fixed regardless of replica count")
		})
	}
}

// TestEnsureMariaDB_StorageSizeFromSpec verifies the fresh-create projection
// honours spec.infrastructure.database.storageSize: an explicit value is written
// to the owned MariaDB's spec.storage.size verbatim (so kind/CI can request a
// small test-sized volume), while an empty value (only reachable when the CRD
// default is bypassed, e.g. a fake-client build like this one) falls back to the
// production baseline default rather than a zero-sized volume the mariadb-operator
// would reject.
func TestEnsureMariaDB_StorageSizeFromSpec(t *testing.T) {
	tests := []struct {
		name        string
		specStorage string
		wantStorage string
	}{
		{name: "explicit small volume projected verbatim", specStorage: "512Mi", wantStorage: "512Mi"},
		{name: "explicit large volume projected verbatim", specStorage: "100Gi", wantStorage: "100Gi"},
		{name: "empty falls back to the baseline default", specStorage: "", wantStorage: infraMariaDBStorageSizeDefault},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)

			s := infraTestScheme(t)
			cp := managedInfraControlPlane()
			cp.Spec.Infrastructure.Database.StorageSize = tc.specStorage
			c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
			r := &ControlPlaneReconciler{Client: c, Scheme: s}

			_, err := r.ensureMariaDB(context.Background(), c, cp, &cp.Spec.Infrastructure.Database, cp.KeystoneNamespace())
			g.Expect(err).NotTo(HaveOccurred())

			var mariadb mariadbv1alpha1.MariaDB
			g.Expect(c.Get(context.Background(), types.NamespacedName{
				Name: "openstack-db", Namespace: childNamespace(cp),
			}, &mariadb)).To(Succeed())
			g.Expect(mariadb.Spec.Storage.Size).NotTo(BeNil())
			want := resource.MustParse(tc.wantStorage)
			g.Expect(mariadb.Spec.Storage.Size.Equal(want)).To(BeTrue(),
				"projected storage size %s must equal %s", mariadb.Spec.Storage.Size, tc.wantStorage)
		})
	}
}

// TestEnsureMemcached_OwnedReconcilesReplicas verifies the owner-aware path for
// Memcached: a Memcached this ControlPlane OWNS has spec.replicas reconciled to
// cp.Spec.Infrastructure.Cache.Replicas when they differ, so a ControlPlane spec
// change scales the owned cache instead of being ignored after first creation.
func TestEnsureMemcached_OwnedReconcilesReplicas(t *testing.T) {
	g := NewGomegaWithT(t)

	s := infraTestScheme(t)
	cp := managedInfraControlPlane() // Cache.Replicas = 3

	ownedMemcached := &unstructured.Unstructured{}
	ownedMemcached.SetGroupVersionKind(memcachedGVK)
	ownedMemcached.SetName("openstack-memcached")
	ownedMemcached.SetNamespace(childNamespace(cp))
	_ = unstructured.SetNestedField(ownedMemcached.Object, int64(1), "spec", "replicas") // drifted
	g.Expect(controllerutil.SetControllerReference(cp, ownedMemcached, s)).To(Succeed())

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, ownedMemcached).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.ensureMemcached(context.Background(), c, cp, &cp.Spec.Infrastructure.Cache, cp.KeystoneNamespace())
	g.Expect(err).NotTo(HaveOccurred())

	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(memcachedGVK)
	g.Expect(c.Get(context.Background(), types.NamespacedName{
		Name: "openstack-memcached", Namespace: childNamespace(cp),
	}, u)).To(Succeed())
	replicas, found, nerr := unstructured.NestedInt64(u.Object, "spec", "replicas")
	g.Expect(nerr).NotTo(HaveOccurred())
	g.Expect(found).To(BeTrue())
	g.Expect(replicas).To(Equal(int64(3)),
		"an owned Memcached must have spec.replicas reconciled to the ControlPlane spec")
}

func TestReconcileInfrastructure_BrownfieldSkipsChildren(t *testing.T) {
	g := NewGomegaWithT(t)

	s := infraTestScheme(t)
	cp := &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: "default", Generation: 1},
		Spec: c5c3v1alpha1.ControlPlaneSpec{
			OpenStackRelease: "2025.2",
			Infrastructure: &c5c3v1alpha1.InfrastructureSpec{
				Database: commonv1.DatabaseSpec{
					Host:      "db.example.com",
					Database:  "keystone",
					SecretRef: commonv1.SecretRefSpec{Name: "keystone-db"},
				},
				Cache: commonv1.CacheSpec{
					Servers: []string{"memcached.example.com:11211"},
					Backend: "dogpile.cache.pymemcache",
				},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	res, err := r.reconcileInfrastructure(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))

	// No MariaDB child must exist.
	var mariadbList mariadbv1alpha1.MariaDBList
	g.Expect(c.List(context.Background(), &mariadbList)).To(Succeed())
	g.Expect(mariadbList.Items).To(BeEmpty(), "brownfield DB must not create a MariaDB CR")

	// No Memcached child must exist.
	memcachedList := &unstructured.UnstructuredList{}
	memcachedList.SetGroupVersionKind(memcachedGVK)
	g.Expect(client.IgnoreNotFound(c.List(context.Background(), memcachedList))).To(Succeed())
	g.Expect(memcachedList.Items).To(BeEmpty(), "brownfield cache must not create a Memcached CR")

	// Nothing to provision -> InfrastructureReady True immediately.
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeInfrastructureReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
}

// externalInfraControlPlane builds an External-mode ControlPlane: the identity
// plane is a pre-existing Keystone and there is NO spec.infrastructure block
// (the validating webhook forbids it in External mode).
func externalInfraControlPlane() *c5c3v1alpha1.ControlPlane {
	return &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: "default", Generation: 1},
		Spec: c5c3v1alpha1.ControlPlaneSpec{
			OpenStackRelease: "2025.2",
			Services: c5c3v1alpha1.ServicesSpec{
				Keystone: &c5c3v1alpha1.ServiceKeystoneSpec{
					Mode:     c5c3v1alpha1.KeystoneModeExternal,
					External: &c5c3v1alpha1.ExternalKeystoneSpec{AuthURL: "https://keystone.example.com/v3"},
				},
			},
		},
	}
}

// TestReconcileInfrastructure_ExternalModeReportsExternallyManaged asserts the
// External-mode short-circuit: InfrastructureReady=True with the dedicated
// ExternallyManaged reason, a message naming the external endpoint, no requeue,
// and provably zero backing-service children.
func TestReconcileInfrastructure_ExternalModeReportsExternallyManaged(t *testing.T) {
	g := NewGomegaWithT(t)

	s := infraTestScheme(t)
	cp := externalInfraControlPlane()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	res, err := r.reconcileInfrastructure(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}), "the External short-circuit must not requeue")

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeInfrastructureReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonExternallyManaged))
	g.Expect(cond.Message).To(ContainSubstring("https://keystone.example.com/v3"),
		"the ExternallyManaged message must name the external endpoint")

	// Absence of the managed children is the acceptance criterion, so assert it
	// explicitly rather than relying on the condition alone.
	var mariadbList mariadbv1alpha1.MariaDBList
	g.Expect(c.List(context.Background(), &mariadbList)).To(Succeed())
	g.Expect(mariadbList.Items).To(BeEmpty(), "External mode must not create a MariaDB CR")

	memcachedList := &unstructured.UnstructuredList{}
	memcachedList.SetGroupVersionKind(memcachedGVK)
	g.Expect(c.List(context.Background(), memcachedList)).To(Succeed())
	g.Expect(memcachedList.Items).To(BeEmpty(), "External mode must not create a Memcached CR")
}

// TestReconcileInfrastructure_NilInfrastructureNonExternalFailsClosed covers the
// webhook-bypass edge path: a Managed CR whose spec.infrastructure was dropped
// must fail closed with a named reason rather than dereference the nil block or
// silently report Ready.
func TestReconcileInfrastructure_NilInfrastructureNonExternalFailsClosed(t *testing.T) {
	g := NewGomegaWithT(t)

	s := infraTestScheme(t)
	cp := managedInfraControlPlane()
	cp.Spec.Infrastructure = nil
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	res, err := r.reconcileInfrastructure(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(infraRequeueAfter))

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeInfrastructureReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonInfrastructureNotConfigured))

	var mariadbList mariadbv1alpha1.MariaDBList
	g.Expect(c.List(context.Background(), &mariadbList)).To(Succeed())
	g.Expect(mariadbList.Items).To(BeEmpty())
}

// dedicatedInfraControlPlane keeps the managed shared block but opts BOTH services
// out of it: a dedicated database and cache for Keystone, a dedicated cache for
// Horizon. It is the opt-in shape the reconciler must provision, own, and gate
// readiness on — and, because nothing resolves to the shared block any more, the
// shape that must leave the shared instances unprovisioned.
func dedicatedInfraControlPlane() *c5c3v1alpha1.ControlPlane {
	cp := managedInfraControlPlane()
	cp.Spec.Services = c5c3v1alpha1.ServicesSpec{
		Keystone: &c5c3v1alpha1.ServiceKeystoneSpec{
			DedicatedBackingServices: &c5c3v1alpha1.KeystoneDedicatedBackingServicesSpec{
				Database: &commonv1.DatabaseSpec{
					ClusterRef:      &corev1.LocalObjectReference{Name: "cp-keystone-db"},
					CredentialsMode: commonv1.CredentialsModeStatic,
					Database:        "keystone",
					SecretRef:       commonv1.SecretRefSpec{Name: "keystone-db"},
					Replicas:        1,
					StorageSize:     "512Mi",
				},
				Cache: &commonv1.CacheSpec{
					ClusterRef: &corev1.LocalObjectReference{Name: "cp-keystone-cache"},
					Backend:    commonv1.DefaultCacheBackend,
					Replicas:   1,
				},
			},
		},
		Horizon: &c5c3v1alpha1.ServiceHorizonSpec{
			DedicatedBackingServices: &c5c3v1alpha1.HorizonDedicatedBackingServicesSpec{
				Cache: &commonv1.CacheSpec{
					ClusterRef: &corev1.LocalObjectReference{Name: "cp-horizon-cache"},
					Backend:    commonv1.DefaultCacheBackend,
					Replicas:   2,
				},
			},
		},
	}
	return cp
}

// readyMemcached builds a Memcached child that already reports Ready, so a test
// can gate readiness on exactly the instances it wants still converging.
func readyMemcached(name, namespace string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(memcachedGVK)
	u.SetName(name)
	u.SetNamespace(namespace)
	_ = unstructured.SetNestedSlice(u.Object, []interface{}{
		map[string]interface{}{"type": "Ready", "status": "True"},
	}, "status", "conditions")
	return u
}

// TestReconcileInfrastructure_DedicatedProjectsChildren verifies a service that
// opts into dedicated backing services gets its OWN MariaDB and Memcached
// children — provisioned and controller-OWNED exactly like a shared one (the
// owner reference is what tears them down with the ControlPlane) — with the
// topology and volume size taken from the DEDICATED spec, not the shared one.
//
// The fixture opts BOTH services out of BOTH shared instances, so the shared
// block has no consumer left: nothing resolves to it, so nothing is provisioned
// for it (see TestReconcileInfrastructure_SkipsSharedInstancesNoServiceResolvesTo).
func TestReconcileInfrastructure_DedicatedProjectsChildren(t *testing.T) {
	g := NewGomegaWithT(t)

	s := infraTestScheme(t)
	cp := dedicatedInfraControlPlane()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileInfrastructure(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	// Keystone's dedicated MariaDB: sized from the dedicated spec (1 replica, no
	// Galera, 512Mi) — independently of the shared cluster's 3-replica default.
	var dedicatedDB mariadbv1alpha1.MariaDB
	g.Expect(c.Get(context.Background(), types.NamespacedName{
		Name: "cp-keystone-db", Namespace: childNamespace(cp),
	}, &dedicatedDB)).To(Succeed(), "the dedicated MariaDB must be provisioned")
	g.Expect(dedicatedDB.Spec.Replicas).To(Equal(int32(1)))
	g.Expect(dedicatedDB.Spec.Galera).NotTo(BeNil())
	g.Expect(dedicatedDB.Spec.Galera.Enabled).To(BeFalse(),
		"a single-replica dedicated database must not enable Galera")
	g.Expect(dedicatedDB.Spec.Storage.Size).NotTo(BeNil())
	g.Expect(dedicatedDB.Spec.Storage.Size.Equal(resource.MustParse("512Mi"))).To(BeTrue(),
		"the dedicated volume size must come from the dedicated spec, not the shared one")
	g.Expect(metav1.IsControlledBy(&dedicatedDB, cp)).To(BeTrue(),
		"the dedicated MariaDB must be controller-owned so it is torn down with the ControlPlane")

	// Each service's dedicated cache gets its own Memcached, owned, at its own
	// replica count.
	for _, tc := range []struct {
		name     string
		replicas int64
	}{
		{name: "cp-keystone-cache", replicas: 1},
		{name: "cp-horizon-cache", replicas: 2},
	} {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(memcachedGVK)
		g.Expect(c.Get(context.Background(), types.NamespacedName{
			Name: tc.name, Namespace: childNamespace(cp),
		}, u)).To(Succeed(), "Memcached %q must be provisioned", tc.name)
		replicas, found, nerr := unstructured.NestedInt64(u.Object, "spec", "replicas")
		g.Expect(nerr).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(replicas).To(Equal(tc.replicas), "Memcached %q replica count", tc.name)
		g.Expect(u.GetOwnerReferences()).NotTo(BeEmpty(), "Memcached %q must be owned", tc.name)
	}
}

// TestReconcileInfrastructure_SkipsSharedInstancesNoServiceResolvesTo pins the
// consumer-driven provisioning rule: a MANAGED shared instance every service has
// opted out of has no consumer, so it is not provisioned and does not gate
// readiness. Keystone is the ControlPlane's only database consumer, and the
// webhook materializes spec.infrastructure.database whenever it is omitted (3
// Galera replicas, 100Gi) — so provisioning the declared set rather than the
// resolved one would leave a full Galera cluster nothing talks to, with
// InfrastructureReady blocked on it coming up.
func TestReconcileInfrastructure_SkipsSharedInstancesNoServiceResolvesTo(t *testing.T) {
	g := NewGomegaWithT(t)

	s := infraTestScheme(t)
	cp := dedicatedInfraControlPlane()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileInfrastructure(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	var mariadbList mariadbv1alpha1.MariaDBList
	g.Expect(c.List(context.Background(), &mariadbList)).To(Succeed())
	g.Expect(mariadbList.Items).To(HaveLen(1))
	g.Expect(mariadbList.Items[0].Name).To(Equal("cp-keystone-db"),
		"the shared MariaDB has no consumer left and must not be provisioned")

	memcachedList := &unstructured.UnstructuredList{}
	memcachedList.SetGroupVersionKind(memcachedGVK)
	g.Expect(c.List(context.Background(), memcachedList)).To(Succeed())
	var names []string
	for _, item := range memcachedList.Items {
		names = append(names, item.GetName())
	}
	g.Expect(names).To(ConsistOf("cp-keystone-cache", "cp-horizon-cache"),
		"the shared Memcached has no consumer left and must not be provisioned")
}

// TestReconcileInfrastructure_PartialOptOutKeepsConsumedSharedCache is the other
// half of the rule: a shared instance a service STILL resolves to is provisioned
// as before. Keystone here takes only a dedicated database, so the shared cache
// keeps its consumer while the shared database loses its only one.
func TestReconcileInfrastructure_PartialOptOutKeepsConsumedSharedCache(t *testing.T) {
	g := NewGomegaWithT(t)

	s := infraTestScheme(t)
	cp := dedicatedInfraControlPlane()
	// Drop both dedicated caches: every service is back on the shared cache, and
	// only the database is dedicated.
	cp.Spec.Services.Keystone.DedicatedBackingServices.Cache = nil
	cp.Spec.Services.Horizon = nil

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileInfrastructure(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	var mariadbList mariadbv1alpha1.MariaDBList
	g.Expect(c.List(context.Background(), &mariadbList)).To(Succeed())
	g.Expect(mariadbList.Items).To(HaveLen(1))
	g.Expect(mariadbList.Items[0].Name).To(Equal("cp-keystone-db"))

	memcachedList := &unstructured.UnstructuredList{}
	memcachedList.SetGroupVersionKind(memcachedGVK)
	g.Expect(c.List(context.Background(), memcachedList)).To(Succeed())
	g.Expect(memcachedList.Items).To(HaveLen(1))
	g.Expect(memcachedList.Items[0].GetName()).To(Equal("openstack-memcached"),
		"the shared cache still has a consumer and must still be provisioned")
}

// TestManagedInfraInstances_DeduplicatesOnChildIdentity covers the
// webhook-bypassed collision the admission rule (validateDedicatedBackingServices
// rejects a duplicate clusterRef name) makes unreachable on the API path. Two
// entries resolving to ONE child CR would run ensure against it twice per pass,
// each projecting a different topology; because the controller Owns() the child
// with no update predicate, each of those writes re-enqueues the ControlPlane —
// a self-sustaining loop of conflicting writes. Deduplicating on (kind, name)
// fails closed: one entry, one projection.
func TestManagedInfraInstances_DeduplicatesOnChildIdentity(t *testing.T) {
	g := NewGomegaWithT(t)

	s := infraTestScheme(t)
	cp := dedicatedInfraControlPlane()
	// Direct-to-etcd shape: Keystone's dedicated cache collides with the shared
	// one Horizon still resolves to.
	cp.Spec.Services.Keystone.DedicatedBackingServices.Cache.ClusterRef.Name = "openstack-memcached"
	cp.Spec.Services.Horizon.DedicatedBackingServices = nil

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	instances := r.managedInfraInstances(cp)
	var caches int
	for _, inst := range instances {
		if inst.kind == "Memcached" {
			caches++
		}
	}
	g.Expect(caches).To(Equal(1), "the colliding declarations must resolve to ONE managed instance")

	_, err := r.reconcileInfrastructure(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(memcachedGVK)
	g.Expect(c.Get(context.Background(), types.NamespacedName{
		Name: "openstack-memcached", Namespace: childNamespace(cp),
	}, u)).To(Succeed())
	replicas, found, nerr := unstructured.NestedInt64(u.Object, "spec", "replicas")
	g.Expect(nerr).NotTo(HaveOccurred())
	g.Expect(found).To(BeTrue())
	g.Expect(replicas).To(Equal(int64(1)),
		"the first resolution wins outright; the second must not re-project a conflicting topology")
}

// TestReconcileInfrastructure_DedicatedNotReadyGatesCollectively verifies
// readiness is gated across the WHOLE managed set: with every other instance
// Ready but one dedicated instance still converging, InfrastructureReady stays
// False and the message names the pending instance — so the consuming service's
// projection (gated on InfrastructureReady) is deferred until the database it
// actually talks to is up.
func TestReconcileInfrastructure_DedicatedNotReadyGatesCollectively(t *testing.T) {
	g := NewGomegaWithT(t)

	s := infraTestScheme(t)
	cp := dedicatedInfraControlPlane()

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(
		cp,
		readyMemcached("cp-keystone-cache", childNamespace(cp)),
		readyMemcached("cp-horizon-cache", childNamespace(cp)),
	).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	// The dedicated MariaDB is freshly created by this pass and carries no Ready
	// condition yet — the only instance still converging.
	res, err := r.reconcileInfrastructure(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(infraRequeueAfter))

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeInfrastructureReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse),
		"a pending DEDICATED instance must hold InfrastructureReady False even when every other instance is Ready")
	g.Expect(cond.Reason).To(Equal("WaitingForDatabase"))
	g.Expect(cond.Message).To(ContainSubstring("cp-keystone-db"),
		"the message must name the pending instance")
	g.Expect(cond.Message).To(ContainSubstring("dedicatedBackingServices.database"),
		"the message must name where the pending instance was declared")
}

// TestReconcileInfrastructure_DedicatedAdoptsExistingWithoutMutating verifies the
// adoption-safe path applies to a dedicated instance exactly as it does to a
// shared one: a pre-existing, externally-provisioned CR under the dedicated name
// is adopted read-only — never reshaped, never GC-claimed — so pointing a service
// at an operator-managed-elsewhere instance cannot destroy it.
func TestReconcileInfrastructure_DedicatedAdoptsExistingWithoutMutating(t *testing.T) {
	g := NewGomegaWithT(t)

	s := infraTestScheme(t)
	cp := dedicatedInfraControlPlane()

	existingSize := resource.MustParse("50Gi")
	existing := &mariadbv1alpha1.MariaDB{
		ObjectMeta: metav1.ObjectMeta{Name: "cp-keystone-db", Namespace: childNamespace(cp)},
		Spec: mariadbv1alpha1.MariaDBSpec{
			Replicas: 3,
			Storage: mariadbv1alpha1.Storage{
				Size:             &existingSize,
				StorageClassName: "premium",
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, existing).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileInfrastructure(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred(), "adopting a pre-existing dedicated instance must not error")

	var mariadb mariadbv1alpha1.MariaDB
	g.Expect(c.Get(context.Background(), types.NamespacedName{
		Name: "cp-keystone-db", Namespace: childNamespace(cp),
	}, &mariadb)).To(Succeed())
	g.Expect(mariadb.Spec.Replicas).To(Equal(int32(3)),
		"an adopted dedicated MariaDB must not be reshaped to the declared topology")
	g.Expect(mariadb.Spec.Storage.StorageClassName).To(Equal("premium"))
	g.Expect(mariadb.OwnerReferences).To(BeEmpty(),
		"must not claim GC ownership of a pre-existing dedicated instance")
}

// TestReconcileInfrastructure_DedicatedBrownfieldProvisionsNothing covers the
// managed-versus-brownfield split at the dedicated level: a dedicated instance
// that references an externally operated endpoint provisions no child CR. Keystone
// is the only database consumer and it points at an external one here, so no
// MariaDB is created at all — the shared managed database it opted out of has no
// consumer left. The shared cache still has one (Horizon resolves to it), so it is
// still provisioned.
func TestReconcileInfrastructure_DedicatedBrownfieldProvisionsNothing(t *testing.T) {
	g := NewGomegaWithT(t)

	s := infraTestScheme(t)
	cp := managedInfraControlPlane()
	cp.Spec.Services = c5c3v1alpha1.ServicesSpec{
		Keystone: &c5c3v1alpha1.ServiceKeystoneSpec{
			DedicatedBackingServices: &c5c3v1alpha1.KeystoneDedicatedBackingServicesSpec{
				Database: &commonv1.DatabaseSpec{
					Host:      "keystone-db.example.com",
					Port:      3306,
					Database:  "keystone",
					SecretRef: commonv1.SecretRefSpec{Name: "keystone-db"},
				},
				Cache: &commonv1.CacheSpec{
					Servers: []string{"keystone-mc.example.com:11211"},
					Backend: commonv1.DefaultCacheBackend,
				},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileInfrastructure(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	var mariadbList mariadbv1alpha1.MariaDBList
	g.Expect(c.List(context.Background(), &mariadbList)).To(Succeed())
	g.Expect(mariadbList.Items).To(BeEmpty(),
		"a brownfield dedicated database provisions nothing, and the shared managed database "+
			"Keystone opted out of has no consumer left")

	// Horizon is NOT declared here, so it resolves to no cache at all — the shared
	// managed cache Keystone opted out of therefore has no consumer and is not
	// provisioned. (Enumerating the shared cache for an undeclared Horizon would
	// provision an instance nothing talks to, and — once services can be placed in
	// separate namespaces — hold InfrastructureReady behind a cache for a service
	// that does not exist; see the Horizon gate in managedInfraInstances.)
	memcachedList := &unstructured.UnstructuredList{}
	memcachedList.SetGroupVersionKind(memcachedGVK)
	g.Expect(c.List(context.Background(), memcachedList)).To(Succeed())
	g.Expect(memcachedList.Items).To(BeEmpty(),
		"a brownfield dedicated cache provisions nothing, and no declared service resolves to the shared cache")
}

// --- per-service namespaces (issue #646): backing services follow the service ---

// splitNamespaceControlPlane places Keystone and Horizon in namespaces of their
// own, both on the SHARED spec.infrastructure block. Each namespace must
// therefore get its own set of instances materialized from that one declaration.
func splitNamespaceControlPlane() *c5c3v1alpha1.ControlPlane {
	cp := managedInfraControlPlane()
	cp.Namespace = "openstack"
	cp.Spec.Services = c5c3v1alpha1.ServicesSpec{
		Keystone: &c5c3v1alpha1.ServiceKeystoneSpec{
			Namespace: &c5c3v1alpha1.ServiceNamespaceSpec{
				Name:      "identity",
				Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
			},
		},
		Horizon: &c5c3v1alpha1.ServiceHorizonSpec{
			Namespace: &c5c3v1alpha1.ServiceNamespaceSpec{
				Name:      "dashboard",
				Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
			},
		},
	}
	return cp
}

// TestManagedInfraInstances_FollowTheServiceNamespace pins the core of "backing
// services follow the namespace": the SAME shared spec.infrastructure block
// materializes one instance per namespace that consumes it. Keystone and Horizon
// placed apart yield a database and a cache in the identity namespace and a
// second cache in the dashboard namespace — not one set in the ControlPlane's.
func TestManagedInfraInstances_FollowTheServiceNamespace(t *testing.T) {
	g := NewGomegaWithT(t)
	s := infraTestScheme(t)
	cp := splitNamespaceControlPlane()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	instances := r.managedInfraInstances(cp)

	type placement struct{ kind, name, namespace string }
	got := make([]placement, 0, len(instances))
	for _, inst := range instances {
		got = append(got, placement{inst.kind, inst.name, inst.namespace})
	}
	g.Expect(got).To(ConsistOf(
		placement{"MariaDB", "openstack-db", "identity"},
		placement{"Memcached", "openstack-memcached", "identity"},
		placement{"Memcached", "openstack-memcached", "dashboard"},
	), "each namespace hosting a service gets its own instances from the shared block")
}

// TestManagedInfraInstances_ColocatedServicesShare verifies the dedup: services
// placed in the SAME namespace share that namespace's instances, so the shared
// cache is ensured once, not twice.
func TestManagedInfraInstances_ColocatedServicesShare(t *testing.T) {
	g := NewGomegaWithT(t)
	s := infraTestScheme(t)
	cp := splitNamespaceControlPlane()
	cp.Spec.Services.Horizon.Namespace.Name = "identity"
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	instances := r.managedInfraInstances(cp)
	g.Expect(instances).To(HaveLen(2), "co-located services share one database and one cache")
	for _, inst := range instances {
		g.Expect(inst.namespace).To(Equal("identity"))
	}
}

// TestManagedInfraInstances_UnassignedIsUnchanged pins the default: a ControlPlane
// with no namespace assignments enumerates exactly what it always did — one
// database and one cache, both in the ControlPlane's own namespace.
func TestManagedInfraInstances_UnassignedIsUnchanged(t *testing.T) {
	g := NewGomegaWithT(t)
	s := infraTestScheme(t)
	cp := managedInfraControlPlane()
	cp.Spec.Services = c5c3v1alpha1.ServicesSpec{
		Keystone: &c5c3v1alpha1.ServiceKeystoneSpec{},
		Horizon:  &c5c3v1alpha1.ServiceHorizonSpec{},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	instances := r.managedInfraInstances(cp)
	g.Expect(instances).To(HaveLen(2))
	for _, inst := range instances {
		g.Expect(inst.namespace).To(Equal(cp.Namespace))
	}
}

// TestReconcileInfrastructure_CrossNamespaceChildrenAreLabelledNotOwned verifies
// the ownership substitute: a backing service in a service namespace carries the
// ownership labels and NO owner reference (Kubernetes forbids a cross-namespace
// one), while one in the ControlPlane's own namespace keeps its owner reference.
func TestReconcileInfrastructure_CrossNamespaceChildrenAreLabelledNotOwned(t *testing.T) {
	g := NewGomegaWithT(t)
	s := infraTestScheme(t)
	cp := splitNamespaceControlPlane()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileInfrastructure(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	var mariadb mariadbv1alpha1.MariaDB
	g.Expect(c.Get(context.Background(), types.NamespacedName{
		Name: "openstack-db", Namespace: "identity",
	}, &mariadb)).To(Succeed(), "the database must be provisioned in Keystone's namespace")
	g.Expect(mariadb.OwnerReferences).To(BeEmpty(),
		"a cross-namespace child cannot carry an owner reference")
	g.Expect(mariadb.Labels).To(HaveKeyWithValue(controlPlaneNameLabel, "cp"))
	g.Expect(mariadb.Labels).To(HaveKeyWithValue(controlPlaneNamespaceLabel, "openstack"))

	// Nothing lands in the ControlPlane's own namespace: no service resolves there.
	g.Expect(c.Get(context.Background(), types.NamespacedName{
		Name: "openstack-db", Namespace: "openstack",
	}, &mariadbv1alpha1.MariaDB{})).NotTo(Succeed())
}

// TestEnsureMariaDB_RefusesToReshapeAForeignInstance verifies the never-adopt
// guard survives the cross-namespace move: a MariaDB in a service namespace that
// carries neither our owner reference nor our labels is adopted read-only — its
// topology is never re-projected, and it is never claimed.
func TestEnsureMariaDB_RefusesToReshapeAForeignInstance(t *testing.T) {
	g := NewGomegaWithT(t)
	s := infraTestScheme(t)
	cp := splitNamespaceControlPlane()

	foreign := &mariadbv1alpha1.MariaDB{
		ObjectMeta: metav1.ObjectMeta{Name: "openstack-db", Namespace: "identity"},
		Spec:       mariadbv1alpha1.MariaDBSpec{Replicas: 1},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, foreign).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.ensureMariaDB(context.Background(), c, cp, &cp.Spec.Infrastructure.Database, "identity")
	g.Expect(err).NotTo(HaveOccurred())

	var live mariadbv1alpha1.MariaDB
	g.Expect(c.Get(context.Background(), types.NamespacedName{
		Name: "openstack-db", Namespace: "identity",
	}, &live)).To(Succeed())
	g.Expect(live.Spec.Replicas).To(Equal(int32(1)),
		"an externally-provisioned instance must not have its topology re-projected")
	g.Expect(live.Labels).NotTo(HaveKey(controlPlaneNameLabel),
		"ownership must never be claimed over an instance we did not create")
}

// TestManagedInfraInstances_UndeclaredHorizonHasNoCache pins the Horizon gate: a
// ControlPlane that declares only Keystone must not enumerate a cache for a
// dashboard that does not exist. While every service shared the ControlPlane's
// namespace an undeclared Horizon's shared cache simply deduplicated against
// Keystone's; once Keystone is placed in a namespace of its own the phantom
// dashboard cache would be a distinct instance in the ControlPlane's namespace,
// with no consumer, holding InfrastructureReady back forever.
func TestManagedInfraInstances_UndeclaredHorizonHasNoCache(t *testing.T) {
	g := NewGomegaWithT(t)
	s := infraTestScheme(t)
	cp := managedInfraControlPlane()
	cp.Namespace = "openstack"
	cp.Spec.Services = c5c3v1alpha1.ServicesSpec{
		Keystone: &c5c3v1alpha1.ServiceKeystoneSpec{
			Namespace: &c5c3v1alpha1.ServiceNamespaceSpec{Name: "identity"},
		},
		// Horizon deliberately unset.
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	instances := r.managedInfraInstances(cp)

	type placement struct{ kind, namespace string }
	got := make([]placement, 0, len(instances))
	for _, inst := range instances {
		got = append(got, placement{inst.kind, inst.namespace})
	}
	g.Expect(got).To(ConsistOf(
		placement{"MariaDB", "identity"},
		placement{"Memcached", "identity"},
	), "only Keystone's own database and cache are provisioned; no phantom dashboard cache")
}

// --- Glance (issue #672): a third database + cache consumer ---

// TestManagedInfraInstances_GlanceEnumeratedOnlyWhenDeclared pins the
// no-consumer-no-instance rule for Glance: an undeclared Glance enumerates
// nothing, and a co-located declared Glance resolves to the SAME shared database
// and cache as Keystone, so the entries dedup away rather than provisioning a
// second set.
func TestManagedInfraInstances_GlanceEnumeratedOnlyWhenDeclared(t *testing.T) {
	g := NewGomegaWithT(t)
	s := infraTestScheme(t)
	cp := managedInfraControlPlane()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	// Without services.glance: only Keystone's shared database and cache.
	g.Expect(r.managedInfraInstances(cp)).To(HaveLen(2))

	// With services.glance sharing the ControlPlane's namespace: Glance resolves to
	// the same shared instances, so the (kind, namespace, name) dedup collapses
	// them — still two.
	cp.Spec.Services.Glance = &c5c3v1alpha1.ServiceGlanceSpec{}
	g.Expect(r.managedInfraInstances(cp)).To(HaveLen(2),
		"a co-located Glance shares Keystone's instances, so nothing new is enumerated")
}

// TestManagedInfraInstances_GlanceDedicatedNamespaceMaterializesInstances verifies
// backing services follow Glance into a namespace of its own: the shared block
// materializes a second database and cache in the Glance namespace, distinct from
// Keystone's in the ControlPlane's namespace.
func TestManagedInfraInstances_GlanceDedicatedNamespaceMaterializesInstances(t *testing.T) {
	g := NewGomegaWithT(t)
	s := infraTestScheme(t)
	cp := managedInfraControlPlane()
	cp.Namespace = "openstack"
	cp.Spec.Services = c5c3v1alpha1.ServicesSpec{
		Keystone: &c5c3v1alpha1.ServiceKeystoneSpec{},
		Glance: &c5c3v1alpha1.ServiceGlanceSpec{
			Namespace: &c5c3v1alpha1.ServiceNamespaceSpec{Name: "images"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	instances := r.managedInfraInstances(cp)
	type placement struct{ kind, name, namespace string }
	got := make([]placement, 0, len(instances))
	for _, inst := range instances {
		got = append(got, placement{inst.kind, inst.name, inst.namespace})
	}
	g.Expect(got).To(ConsistOf(
		placement{"MariaDB", "openstack-db", "openstack"},
		placement{"Memcached", "openstack-memcached", "openstack"},
		placement{"MariaDB", "openstack-db", "images"},
		placement{"Memcached", "openstack-memcached", "images"},
	), "Glance placed apart materializes the shared block a second time in its namespace")
}

// TestManagedInfraInstances_GlanceDedicatedBackingServices verifies a Glance that
// opts into a dedicated database is enumerated as its own instance (declared at
// the dedicated path), while its still-shared cache dedups against Keystone's.
func TestManagedInfraInstances_GlanceDedicatedBackingServices(t *testing.T) {
	g := NewGomegaWithT(t)
	s := infraTestScheme(t)
	cp := managedInfraControlPlane()
	cp.Spec.Services = c5c3v1alpha1.ServicesSpec{
		Keystone: &c5c3v1alpha1.ServiceKeystoneSpec{},
		Glance: &c5c3v1alpha1.ServiceGlanceSpec{
			DedicatedBackingServices: &c5c3v1alpha1.GlanceDedicatedBackingServicesSpec{
				Database: &commonv1.DatabaseSpec{
					ClusterRef: &corev1.LocalObjectReference{Name: "cp-glance-db"},
					Database:   "glance",
					SecretRef:  commonv1.SecretRefSpec{Name: "glance-db"},
					Replicas:   1,
				},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	instances := r.managedInfraInstances(cp)
	byName := make(map[string]infraInstance, len(instances))
	for _, inst := range instances {
		byName[inst.kind+"/"+inst.name] = inst
	}
	g.Expect(byName).To(HaveKey("MariaDB/cp-glance-db"))
	g.Expect(byName["MariaDB/cp-glance-db"].declaredAt).To(
		Equal("spec.services.glance.dedicatedBackingServices.database"),
	)
	// Keystone still shares the ControlPlane database, and Glance's own cache is
	// not dedicated — so it resolves to (and dedups against) the shared cache
	// Keystone consumes.
	g.Expect(byName).To(HaveKey("MariaDB/openstack-db"))
	g.Expect(byName).To(HaveKey("Memcached/openstack-memcached"))
}

// --- Placement: a fourth database + cache consumer ---

// TestManagedInfraInstances_PlacementEnumeratedOnlyWhenDeclared pins the
// no-consumer-no-instance rule for Placement: an undeclared Placement enumerates
// nothing, and a co-located declared Placement resolves to the SAME shared
// database and cache as Keystone, so the entries dedup away rather than
// provisioning a second set.
func TestManagedInfraInstances_PlacementEnumeratedOnlyWhenDeclared(t *testing.T) {
	g := NewGomegaWithT(t)
	s := infraTestScheme(t)
	cp := managedInfraControlPlane()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	// Without services.placement: only Keystone's shared database and cache.
	g.Expect(r.managedInfraInstances(cp)).To(HaveLen(2))

	// With services.placement sharing the ControlPlane's namespace: Placement
	// resolves to the same shared instances, so the (kind, namespace, name) dedup
	// collapses them — still two.
	cp.Spec.Services.Placement = &c5c3v1alpha1.ServicePlacementSpec{}
	g.Expect(r.managedInfraInstances(cp)).To(HaveLen(2),
		"a co-located Placement shares Keystone's instances, so nothing new is enumerated")
}

// TestManagedInfraInstances_PlacementDedicatedNamespaceMaterializesInstances
// verifies backing services follow Placement into a namespace of its own: the
// shared block materializes a second database and cache in the Placement
// namespace, distinct from Keystone's in the ControlPlane's namespace.
func TestManagedInfraInstances_PlacementDedicatedNamespaceMaterializesInstances(t *testing.T) {
	g := NewGomegaWithT(t)
	s := infraTestScheme(t)
	cp := managedInfraControlPlane()
	cp.Namespace = "openstack"
	cp.Spec.Services = c5c3v1alpha1.ServicesSpec{
		Keystone: &c5c3v1alpha1.ServiceKeystoneSpec{},
		Placement: &c5c3v1alpha1.ServicePlacementSpec{
			Namespace: &c5c3v1alpha1.ServiceNamespaceSpec{Name: "compute"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	instances := r.managedInfraInstances(cp)
	type instancePlacement struct{ kind, name, namespace string }
	got := make([]instancePlacement, 0, len(instances))
	for _, inst := range instances {
		got = append(got, instancePlacement{inst.kind, inst.name, inst.namespace})
	}
	g.Expect(got).To(ConsistOf(
		instancePlacement{"MariaDB", "openstack-db", "openstack"},
		instancePlacement{"Memcached", "openstack-memcached", "openstack"},
		instancePlacement{"MariaDB", "openstack-db", "compute"},
		instancePlacement{"Memcached", "openstack-memcached", "compute"},
	), "Placement placed apart materializes the shared block a second time in its namespace")
}

// TestManagedInfraInstances_PlacementDedicatedBackingServices verifies a Placement
// that opts into a dedicated database is enumerated as its own instance (declared
// at the dedicated path), while its still-shared cache dedups against Keystone's.
func TestManagedInfraInstances_PlacementDedicatedBackingServices(t *testing.T) {
	g := NewGomegaWithT(t)
	s := infraTestScheme(t)
	cp := managedInfraControlPlane()
	cp.Spec.Services = c5c3v1alpha1.ServicesSpec{
		Keystone: &c5c3v1alpha1.ServiceKeystoneSpec{},
		Placement: &c5c3v1alpha1.ServicePlacementSpec{
			DedicatedBackingServices: &c5c3v1alpha1.PlacementDedicatedBackingServicesSpec{
				Database: &commonv1.DatabaseSpec{
					ClusterRef: &corev1.LocalObjectReference{Name: "cp-placement-db"},
					Database:   "placement",
					SecretRef:  commonv1.SecretRefSpec{Name: "placement-db"},
					Replicas:   1,
				},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	instances := r.managedInfraInstances(cp)
	byName := make(map[string]infraInstance, len(instances))
	for _, inst := range instances {
		byName[inst.kind+"/"+inst.name] = inst
	}
	g.Expect(byName).To(HaveKey("MariaDB/cp-placement-db"))
	g.Expect(byName["MariaDB/cp-placement-db"].declaredAt).To(
		Equal("spec.services.placement.dedicatedBackingServices.database"),
	)
	// Keystone still shares the ControlPlane database, and Placement's own cache is
	// not dedicated — so it resolves to (and dedups against) the shared cache
	// Keystone consumes.
	g.Expect(byName).To(HaveKey("MariaDB/openstack-db"))
	g.Expect(byName).To(HaveKey("Memcached/openstack-memcached"))
}

// --- Barbican: a fifth database + cache consumer ---

// TestManagedInfraInstances_BarbicanEnumeratedOnlyWhenDeclared pins the
// no-consumer-no-instance rule for Barbican: an undeclared Barbican enumerates
// nothing, and a co-located declared Barbican resolves to the SAME shared database
// and cache as Keystone, so the entries dedup away rather than provisioning a
// second set.
func TestManagedInfraInstances_BarbicanEnumeratedOnlyWhenDeclared(t *testing.T) {
	g := NewGomegaWithT(t)
	s := infraTestScheme(t)
	cp := managedInfraControlPlane()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	// Without services.barbican: only Keystone's shared database and cache.
	g.Expect(r.managedInfraInstances(cp)).To(HaveLen(2))

	// With services.barbican sharing the ControlPlane's namespace: Barbican
	// resolves to the same shared instances, so the (kind, namespace, name) dedup
	// collapses them, still two.
	cp.Spec.Services.Barbican = &c5c3v1alpha1.ServiceBarbicanSpec{
		SecretStore: c5c3v1alpha1.ServiceBarbicanSecretStoreSpec{
			Dedicated: &c5c3v1alpha1.BarbicanDedicatedSecretStoreSpec{},
		},
	}
	g.Expect(r.managedInfraInstances(cp)).To(HaveLen(2),
		"a co-located Barbican shares Keystone's instances, so nothing new is enumerated")
}

// TestManagedInfraInstances_BarbicanDedicatedBackingServices verifies a Barbican
// that opts into a dedicated database and a dedicated cache is enumerated as its
// own pair of instances, each declared at the dedicated path and placed in the
// namespace the Barbican service occupies. The dedicated database carries
// credentialsMode Static, the only mode the webhook admits for one: the OpenBao
// database engine is bootstrapped per namespace against the SHARED cluster, so no
// engine role could issue credentials for it.
func TestManagedInfraInstances_BarbicanDedicatedBackingServices(t *testing.T) {
	g := NewGomegaWithT(t)
	s := infraTestScheme(t)
	cp := managedInfraControlPlane()
	cp.Namespace = "openstack"
	cp.Spec.Services = c5c3v1alpha1.ServicesSpec{
		Keystone: &c5c3v1alpha1.ServiceKeystoneSpec{},
		Barbican: &c5c3v1alpha1.ServiceBarbicanSpec{
			Namespace: &c5c3v1alpha1.ServiceNamespaceSpec{Name: "key-manager"},
			SecretStore: c5c3v1alpha1.ServiceBarbicanSecretStoreSpec{
				Dedicated: &c5c3v1alpha1.BarbicanDedicatedSecretStoreSpec{},
			},
			DedicatedBackingServices: &c5c3v1alpha1.BarbicanDedicatedBackingServicesSpec{
				Database: &commonv1.DatabaseSpec{
					ClusterRef:      &corev1.LocalObjectReference{Name: "cp-barbican-db"},
					Database:        "barbican",
					SecretRef:       commonv1.SecretRefSpec{Name: "barbican-db"},
					CredentialsMode: commonv1.CredentialsModeStatic,
					Replicas:        1,
				},
				Cache: &commonv1.CacheSpec{
					ClusterRef: &corev1.LocalObjectReference{Name: "cp-barbican-memcached"},
					Backend:    "dogpile.cache.pymemcache",
					Replicas:   1,
				},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	instances := r.managedInfraInstances(cp)
	byName := make(map[string]infraInstance, len(instances))
	for _, inst := range instances {
		byName[inst.kind+"/"+inst.name] = inst
	}
	g.Expect(byName).To(HaveKey("MariaDB/cp-barbican-db"))
	g.Expect(byName["MariaDB/cp-barbican-db"].declaredAt).To(
		Equal("spec.services.barbican.dedicatedBackingServices.database"),
	)
	g.Expect(byName["MariaDB/cp-barbican-db"].namespace).To(Equal("key-manager"),
		"the backing services follow the service into its namespace")
	g.Expect(byName).To(HaveKey("Memcached/cp-barbican-memcached"))
	g.Expect(byName["Memcached/cp-barbican-memcached"].declaredAt).To(
		Equal("spec.services.barbican.dedicatedBackingServices.cache"),
	)
	g.Expect(byName["Memcached/cp-barbican-memcached"].namespace).To(Equal("key-manager"))
	// Keystone still shares the ControlPlane's own instances.
	g.Expect(byName).To(HaveKey("MariaDB/openstack-db"))
	g.Expect(byName).To(HaveKey("Memcached/openstack-memcached"))
}

// TestBarbicanDeclaredAt_FallsBackToTheSharedBlock is the other half of the
// declaredAt contract: a Barbican that declares no dedicated instance resolves to
// the shared block, and the condition message names spec.infrastructure rather
// than a per-service path nobody wrote.
func TestBarbicanDeclaredAt_FallsBackToTheSharedBlock(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := managedInfraControlPlane()
	cp.Spec.Services.Barbican = &c5c3v1alpha1.ServiceBarbicanSpec{
		SecretStore: c5c3v1alpha1.ServiceBarbicanSecretStoreSpec{
			Dedicated: &c5c3v1alpha1.BarbicanDedicatedSecretStoreSpec{},
		},
	}

	g.Expect(barbicanDatabaseDeclaredAt(cp)).To(Equal("spec.infrastructure.database"))
	g.Expect(barbicanCacheDeclaredAt(cp)).To(Equal("spec.infrastructure.cache"))
}

// --- per-service target clusters: the backing services follow the service ---

// placedInfraControlPlane places Horizon — and with it the cache it resolves to —
// on a target cluster, leaving Keystone and its own database and cache at home.
// The asymmetry is deliberate: the two local instances are enumerated FIRST, so a
// pass that resolved clusters lazily would already have written them by the time
// it reached the placed one.
func placedInfraControlPlane(targetCluster string) *c5c3v1alpha1.ControlPlane {
	cp := managedInfraControlPlane()
	cp.Namespace = "openstack"
	cp.Spec.Services = c5c3v1alpha1.ServicesSpec{
		Keystone: &c5c3v1alpha1.ServiceKeystoneSpec{},
		Horizon: &c5c3v1alpha1.ServiceHorizonSpec{
			Namespace: &c5c3v1alpha1.ServiceNamespaceSpec{
				Name:      "dashboard",
				Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
			},
			TargetClusterRef: &commonv1.TargetClusterRefSpec{Name: targetCluster},
		},
	}
	return cp
}

// TestReconcileInfrastructure_PlacedBackingServicesLandOnTheTarget verifies a
// placed service's cache is provisioned on ITS cluster: that is where the
// mariadb-operator (here, the memcached operator) that has to act on the CR runs,
// and where the service's own pods have to reach it. The unplaced pair is
// untouched, so a ControlPlane that places one service keeps the rest at home.
func TestReconcileInfrastructure_PlacedBackingServicesLandOnTheTarget(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	s := infraTestScheme(t)
	cp := placedInfraControlPlane("remote-a")

	target := fake.NewClientBuilder().WithScheme(s).Build()
	resolver := &childrenResolver{children: target}
	r := &ControlPlaneReconciler{
		Client:   fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build(),
		Scheme:   s,
		Resolver: resolver,
	}

	_, err := r.reconcileInfrastructure(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(resolver.names).To(Equal([]mcruntime.ClusterName{"remote-a"}),
		"one lookup per placed namespace, whatever that namespace hosts")

	// The dashboard's cache is on the target, recognizable by its labels alone.
	remote := &unstructured.Unstructured{}
	remote.SetGroupVersionKind(memcachedGVK)
	g.Expect(target.Get(ctx, types.NamespacedName{
		Name: "openstack-memcached", Namespace: "dashboard",
	}, remote)).To(Succeed(), "the placed service's cache must be provisioned on its own cluster")
	g.Expect(remote.GetLabels()).To(Equal(remoteChildLabels(cp)))
	g.Expect(remote.GetOwnerReferences()).To(BeEmpty(),
		"an owner reference on the target cluster names a UID that cluster cannot resolve")

	athome := &unstructured.Unstructured{}
	athome.SetGroupVersionKind(memcachedGVK)
	g.Expect(r.Client.Get(ctx, types.NamespacedName{
		Name: "openstack-memcached", Namespace: "dashboard",
	}, athome)).NotTo(Succeed(), "a placed instance must not be provisioned at home as well")

	// Keystone stayed home, so its database did too — owner-referenced, as ever.
	var mariadb mariadbv1alpha1.MariaDB
	g.Expect(r.Client.Get(ctx, types.NamespacedName{
		Name: "openstack-db", Namespace: "openstack",
	}, &mariadb)).To(Succeed())
	g.Expect(metav1.GetControllerOf(&mariadb)).NotTo(BeNil())
	g.Expect(target.Get(ctx, types.NamespacedName{
		Name: "openstack-db", Namespace: "openstack",
	}, &mariadbv1alpha1.MariaDB{})).NotTo(Succeed(),
		"an unplaced service's database must not reach the target cluster")
}

// TestReconcileInfrastructure_UnresolvableTargetProvisionsNothing covers the
// cluster that does not resolve. The proof is what did NOT happen at home: the
// ControlPlane's own database and cache are enumerated before the placed cache, so
// finding them absent is what shows every cluster is resolved before the first
// write rather than one instance at a time.
func TestReconcileInfrastructure_UnresolvableTargetProvisionsNothing(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	s := infraTestScheme(t)
	cp := placedInfraControlPlane("remote-a")

	target := fake.NewClientBuilder().WithScheme(s).Build()
	r := &ControlPlaneReconciler{
		Client:   fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build(),
		Scheme:   s,
		Resolver: &childrenResolver{children: target, err: mcruntime.ErrClusterNotFound},
	}

	res, err := r.reconcileInfrastructure(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred(), "an unregistered cluster is a state to wait out, not a reconcile failure")
	g.Expect(res.RequeueAfter).To(Equal(infraRequeueAfter))

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeInfrastructureReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(commonmulticluster.TargetClusterUnavailable))
	g.Expect(cond.Message).To(ContainSubstring("cluster not found"))

	for name, c := range map[string]client.Client{"management": r.Client, "target": target} {
		var databases mariadbv1alpha1.MariaDBList
		g.Expect(c.List(ctx, &databases)).To(Succeed())
		g.Expect(databases.Items).To(BeEmpty(), "no database may be provisioned on the %s cluster", name)

		caches := &unstructured.UnstructuredList{}
		caches.SetGroupVersionKind(memcachedGVK)
		g.Expect(c.List(ctx, caches)).To(Succeed())
		g.Expect(caches.Items).To(BeEmpty(), "no cache may be provisioned on the %s cluster", name)
	}
}
