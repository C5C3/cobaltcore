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

	"github.com/c5c3/forge/internal/common/conditions"
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

	_, err := r.ensureMariaDB(context.Background(), cp, &cp.Spec.Infrastructure.Database)
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

	_, err := r.ensureMariaDB(context.Background(), cp, &cp.Spec.Infrastructure.Database)
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

			_, err := r.ensureMariaDB(context.Background(), cp, &cp.Spec.Infrastructure.Database)
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

			_, err := r.ensureMariaDB(context.Background(), cp, &cp.Spec.Infrastructure.Database)
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

	_, err := r.ensureMemcached(context.Background(), cp, &cp.Spec.Infrastructure.Cache)
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

	memcachedList := &unstructured.UnstructuredList{}
	memcachedList.SetGroupVersionKind(memcachedGVK)
	g.Expect(c.List(context.Background(), memcachedList)).To(Succeed())
	g.Expect(memcachedList.Items).To(HaveLen(1),
		"a brownfield dedicated cache must provision nothing; the shared cache Horizon resolves to is created")
	g.Expect(memcachedList.Items[0].GetName()).To(Equal("openstack-memcached"))
}
