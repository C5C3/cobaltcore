// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package simulators

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Feature: CC-0002

func newUnstructured(group, version, kind, name, namespace string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group: group, Version: version, Kind: kind,
	})
	obj.SetName(name)
	obj.SetNamespace(namespace)
	return obj
}

func newScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = batchv1.AddToScheme(s)
	return s
}

// --- SimulateMariaDBReady ---

func TestSimulateMariaDBReady(t *testing.T) {
	g := NewGomegaWithT(t)

	mariadb := newUnstructured("k8s.mariadb.com", "v1alpha1", "MariaDB", "test-mariadb", "default")

	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(mariadb).
		WithStatusSubresource(mariadb).
		Build()

	err := SimulateMariaDBReady(context.Background(), c, client.ObjectKeyFromObject(mariadb), 3)
	g.Expect(err).NotTo(HaveOccurred())

	updated := newUnstructured("k8s.mariadb.com", "v1alpha1", "MariaDB", "test-mariadb", "default")
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(mariadb), updated)).To(Succeed())

	readyReplicas, found, err := unstructured.NestedInt64(updated.Object, "status", "readyReplicas")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeTrue())
	g.Expect(readyReplicas).To(BeEquivalentTo(3))

	primaryIdx, found, err := unstructured.NestedInt64(updated.Object, "status", "currentPrimaryPodIndex")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeTrue())
	g.Expect(primaryIdx).To(BeEquivalentTo(0))

	conditions, found, err := unstructured.NestedSlice(updated.Object, "status", "conditions")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeTrue())
	g.Expect(conditions).To(HaveLen(1))

	cond := conditions[0].(map[string]interface{})
	g.Expect(cond["type"]).To(Equal("Ready"))
	g.Expect(cond["status"]).To(Equal("True"))
	g.Expect(cond["reason"]).To(Equal("MariaDBReady"))
}

func TestSimulateMariaDBReady_idempotent(t *testing.T) {
	g := NewGomegaWithT(t)

	mariadb := newUnstructured("k8s.mariadb.com", "v1alpha1", "MariaDB", "test-mariadb", "default")

	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(mariadb).
		WithStatusSubresource(mariadb).
		Build()

	ctx := context.Background()
	key := client.ObjectKeyFromObject(mariadb)

	// Call twice — must not produce duplicate conditions.
	g.Expect(SimulateMariaDBReady(ctx, c, key, 3)).To(Succeed())
	g.Expect(SimulateMariaDBReady(ctx, c, key, 3)).To(Succeed())

	updated := newUnstructured("k8s.mariadb.com", "v1alpha1", "MariaDB", "test-mariadb", "default")
	g.Expect(c.Get(ctx, key, updated)).To(Succeed())

	conditions, found, err := unstructured.NestedSlice(updated.Object, "status", "conditions")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeTrue())
	g.Expect(conditions).To(HaveLen(1), "expected exactly 1 condition after two calls")
}

func TestSimulateMariaDBReady_zeroReplicas(t *testing.T) {
	g := NewGomegaWithT(t)

	mariadb := newUnstructured("k8s.mariadb.com", "v1alpha1", "MariaDB", "test-mariadb", "default")

	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(mariadb).
		WithStatusSubresource(mariadb).
		Build()

	g.Expect(SimulateMariaDBReady(context.Background(), c, client.ObjectKeyFromObject(mariadb), 0)).To(Succeed())

	updated := newUnstructured("k8s.mariadb.com", "v1alpha1", "MariaDB", "test-mariadb", "default")
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(mariadb), updated)).To(Succeed())

	readyReplicas, found, err := unstructured.NestedInt64(updated.Object, "status", "readyReplicas")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeTrue())
	g.Expect(readyReplicas).To(BeEquivalentTo(0))
}

func TestSimulateMariaDBReady_notFound(t *testing.T) {
	g := NewGomegaWithT(t)

	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		Build()

	key := client.ObjectKey{Name: "missing", Namespace: "default"}
	err := SimulateMariaDBReady(context.Background(), c, key, 1)
	g.Expect(err).To(HaveOccurred())
}

// --- SimulateMemcachedReady ---

func TestSimulateMemcachedReady(t *testing.T) {
	g := NewGomegaWithT(t)

	memcached := newUnstructured("cache.c5c3.io", "v1alpha1", "Memcached", "test-memcached", "default")
	servers := []string{"mc-0.mc:11211", "mc-1.mc:11211"}

	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(memcached).
		WithStatusSubresource(memcached).
		Build()

	err := SimulateMemcachedReady(context.Background(), c, client.ObjectKeyFromObject(memcached), 2, servers)
	g.Expect(err).NotTo(HaveOccurred())

	updated := newUnstructured("cache.c5c3.io", "v1alpha1", "Memcached", "test-memcached", "default")
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(memcached), updated)).To(Succeed())

	readyReplicas, found, err := unstructured.NestedInt64(updated.Object, "status", "readyReplicas")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeTrue())
	g.Expect(readyReplicas).To(BeEquivalentTo(2))

	sl, found, err := unstructured.NestedStringSlice(updated.Object, "status", "serverList")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeTrue())
	g.Expect(sl).To(Equal([]string{"mc-0.mc:11211", "mc-1.mc:11211"}))

	conditions, found, err := unstructured.NestedSlice(updated.Object, "status", "conditions")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeTrue())
	g.Expect(conditions).To(HaveLen(1))

	cond := conditions[0].(map[string]interface{})
	g.Expect(cond["type"]).To(Equal("Ready"))
	g.Expect(cond["status"]).To(Equal("True"))
	g.Expect(cond["reason"]).To(Equal("MemcachedReady"))
}

func TestSimulateMemcachedReady_idempotent(t *testing.T) {
	g := NewGomegaWithT(t)

	memcached := newUnstructured("cache.c5c3.io", "v1alpha1", "Memcached", "test-memcached", "default")
	servers := []string{"mc-0.mc:11211"}

	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(memcached).
		WithStatusSubresource(memcached).
		Build()

	ctx := context.Background()
	key := client.ObjectKeyFromObject(memcached)

	g.Expect(SimulateMemcachedReady(ctx, c, key, 1, servers)).To(Succeed())
	g.Expect(SimulateMemcachedReady(ctx, c, key, 1, servers)).To(Succeed())

	updated := newUnstructured("cache.c5c3.io", "v1alpha1", "Memcached", "test-memcached", "default")
	g.Expect(c.Get(ctx, key, updated)).To(Succeed())

	conditions, found, err := unstructured.NestedSlice(updated.Object, "status", "conditions")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeTrue())
	g.Expect(conditions).To(HaveLen(1), "expected exactly 1 condition after two calls")
}

func TestSimulateMemcachedReady_emptyServerList(t *testing.T) {
	g := NewGomegaWithT(t)

	memcached := newUnstructured("cache.c5c3.io", "v1alpha1", "Memcached", "test-memcached", "default")

	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(memcached).
		WithStatusSubresource(memcached).
		Build()

	g.Expect(SimulateMemcachedReady(context.Background(), c, client.ObjectKeyFromObject(memcached), 0, []string{})).To(Succeed())

	updated := newUnstructured("cache.c5c3.io", "v1alpha1", "Memcached", "test-memcached", "default")
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(memcached), updated)).To(Succeed())

	readyReplicas, found, err := unstructured.NestedInt64(updated.Object, "status", "readyReplicas")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeTrue())
	g.Expect(readyReplicas).To(BeEquivalentTo(0))
}

func TestSimulateMemcachedReady_notFound(t *testing.T) {
	g := NewGomegaWithT(t)

	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		Build()

	key := client.ObjectKey{Name: "missing", Namespace: "default"}
	err := SimulateMemcachedReady(context.Background(), c, key, 1, []string{"mc:11211"})
	g.Expect(err).To(HaveOccurred())
}

// --- SimulateExternalSecretSync ---

func TestSimulateExternalSecretSync(t *testing.T) {
	g := NewGomegaWithT(t)

	es := newUnstructured("external-secrets.io", "v1beta1", "ExternalSecret", "test-es", "default")

	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(es).
		WithStatusSubresource(es).
		Build()

	err := SimulateExternalSecretSync(context.Background(), c, client.ObjectKeyFromObject(es))
	g.Expect(err).NotTo(HaveOccurred())

	updated := newUnstructured("external-secrets.io", "v1beta1", "ExternalSecret", "test-es", "default")
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(es), updated)).To(Succeed())

	refreshTime, found, err := unstructured.NestedString(updated.Object, "status", "refreshTime")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeTrue())
	g.Expect(refreshTime).NotTo(BeEmpty())

	conditions, found, err := unstructured.NestedSlice(updated.Object, "status", "conditions")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeTrue())
	g.Expect(conditions).To(HaveLen(1))

	cond := conditions[0].(map[string]interface{})
	g.Expect(cond["type"]).To(Equal("Ready"))
	g.Expect(cond["status"]).To(Equal("True"))
	g.Expect(cond["reason"]).To(Equal("SecretSynced"))
}

func TestSimulateExternalSecretSync_idempotent(t *testing.T) {
	g := NewGomegaWithT(t)

	es := newUnstructured("external-secrets.io", "v1beta1", "ExternalSecret", "test-es", "default")

	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(es).
		WithStatusSubresource(es).
		Build()

	ctx := context.Background()
	key := client.ObjectKeyFromObject(es)

	g.Expect(SimulateExternalSecretSync(ctx, c, key)).To(Succeed())
	g.Expect(SimulateExternalSecretSync(ctx, c, key)).To(Succeed())

	updated := newUnstructured("external-secrets.io", "v1beta1", "ExternalSecret", "test-es", "default")
	g.Expect(c.Get(ctx, key, updated)).To(Succeed())

	conditions, found, err := unstructured.NestedSlice(updated.Object, "status", "conditions")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeTrue())
	g.Expect(conditions).To(HaveLen(1), "expected exactly 1 condition after two calls")
}

func TestSimulateExternalSecretSync_notFound(t *testing.T) {
	g := NewGomegaWithT(t)

	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		Build()

	key := client.ObjectKey{Name: "missing", Namespace: "default"}
	err := SimulateExternalSecretSync(context.Background(), c, key)
	g.Expect(err).To(HaveOccurred())
}

// --- SimulateJobComplete ---

func TestSimulateJobComplete(t *testing.T) {
	g := NewGomegaWithT(t)

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-job",
			Namespace: "default",
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(job).
		WithStatusSubresource(job).
		Build()

	err := SimulateJobComplete(context.Background(), c, client.ObjectKeyFromObject(job))
	g.Expect(err).NotTo(HaveOccurred())

	updated := &batchv1.Job{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(job), updated)).To(Succeed())

	g.Expect(updated.Status.Succeeded).To(BeEquivalentTo(1))
	g.Expect(updated.Status.CompletionTime).NotTo(BeNil())
	g.Expect(updated.Status.Conditions).To(HaveLen(1))

	cond := updated.Status.Conditions[0]
	g.Expect(cond.Type).To(Equal(batchv1.JobComplete))
	g.Expect(cond.Status).To(Equal(corev1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("Completed"))
}

func TestSimulateJobComplete_idempotent(t *testing.T) {
	g := NewGomegaWithT(t)

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-job",
			Namespace: "default",
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(job).
		WithStatusSubresource(job).
		Build()

	ctx := context.Background()
	key := client.ObjectKeyFromObject(job)

	g.Expect(SimulateJobComplete(ctx, c, key)).To(Succeed())
	g.Expect(SimulateJobComplete(ctx, c, key)).To(Succeed())

	updated := &batchv1.Job{}
	g.Expect(c.Get(ctx, key, updated)).To(Succeed())

	g.Expect(updated.Status.Conditions).To(HaveLen(1), "expected exactly 1 condition after two calls")
}

func TestSimulateJobComplete_notFound(t *testing.T) {
	g := NewGomegaWithT(t)

	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		Build()

	key := client.ObjectKey{Name: "missing", Namespace: "default"}
	err := SimulateJobComplete(context.Background(), c, key)
	g.Expect(err).To(HaveOccurred())
}
