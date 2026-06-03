// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the infrastructure sub-reconciler (CC-0110, REQ-008).
package controller

import (
	"context"
	"testing"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

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
			Infrastructure: c5c3v1alpha1.InfrastructureSpec{
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

func TestReconcileInfrastructure_BrownfieldSkipsChildren(t *testing.T) {
	g := NewGomegaWithT(t)

	s := infraTestScheme(t)
	cp := &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: "default", Generation: 1},
		Spec: c5c3v1alpha1.ControlPlaneSpec{
			OpenStackRelease: "2025.2",
			Infrastructure: c5c3v1alpha1.InfrastructureSpec{
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
