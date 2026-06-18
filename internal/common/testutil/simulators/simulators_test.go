// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package simulators

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"

	"github.com/c5c3/forge/internal/common/deployment"
	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

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
	_ = mariadbv1alpha1.AddToScheme(s)
	_ = appsv1.AddToScheme(s)
	_ = esov1.AddToScheme(s)
	return s
}

// --- SimulateMariaDBReady ---

func TestSimulateMariaDBReady(t *testing.T) {
	g := NewGomegaWithT(t)

	mariadb := &mariadbv1alpha1.MariaDB{
		ObjectMeta: metav1.ObjectMeta{Name: "test-mariadb", Namespace: "default"},
	}

	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(mariadb).
		WithStatusSubresource(mariadb).
		Build()

	err := SimulateMariaDBReady(context.Background(), c, client.ObjectKeyFromObject(mariadb), 3)
	g.Expect(err).NotTo(HaveOccurred())

	updated := &mariadbv1alpha1.MariaDB{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(mariadb), updated)).To(Succeed())

	g.Expect(updated.Status.Replicas).To(BeEquivalentTo(3))
	g.Expect(updated.Status.CurrentPrimaryPodIndex).NotTo(BeNil())
	g.Expect(*updated.Status.CurrentPrimaryPodIndex).To(Equal(0))
	g.Expect(updated.Status.Conditions).To(HaveLen(1))

	cond := updated.Status.Conditions[0]
	g.Expect(cond.Type).To(Equal("Ready"))
	g.Expect(string(cond.Status)).To(Equal("True"))
	g.Expect(cond.Reason).To(Equal("MariaDBReady"))
}

func TestSimulateMariaDBReady_idempotent(t *testing.T) {
	g := NewGomegaWithT(t)

	mariadb := &mariadbv1alpha1.MariaDB{
		ObjectMeta: metav1.ObjectMeta{Name: "test-mariadb", Namespace: "default"},
	}

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

	updated := &mariadbv1alpha1.MariaDB{}
	g.Expect(c.Get(ctx, key, updated)).To(Succeed())

	g.Expect(updated.Status.Conditions).To(HaveLen(1), "expected exactly 1 condition after two calls")
}

func TestSimulateMariaDBReady_zeroReplicas(t *testing.T) {
	g := NewGomegaWithT(t)

	mariadb := &mariadbv1alpha1.MariaDB{
		ObjectMeta: metav1.ObjectMeta{Name: "test-mariadb", Namespace: "default"},
	}

	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(mariadb).
		WithStatusSubresource(mariadb).
		Build()

	g.Expect(SimulateMariaDBReady(context.Background(), c, client.ObjectKeyFromObject(mariadb), 0)).To(Succeed())

	updated := &mariadbv1alpha1.MariaDB{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(mariadb), updated)).To(Succeed())

	g.Expect(updated.Status.Replicas).To(BeEquivalentTo(0))
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

	memcached := newUnstructured("memcached.c5c3.io", "v1beta1", "Memcached", "test-memcached", "default")
	servers := []string{"mc-0.mc:11211", "mc-1.mc:11211"}

	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(memcached).
		WithStatusSubresource(memcached).
		Build()

	err := SimulateMemcachedReady(context.Background(), c, client.ObjectKeyFromObject(memcached), 2, servers)
	g.Expect(err).NotTo(HaveOccurred())

	updated := newUnstructured("memcached.c5c3.io", "v1beta1", "Memcached", "test-memcached", "default")
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

	memcached := newUnstructured("memcached.c5c3.io", "v1beta1", "Memcached", "test-memcached", "default")
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

	updated := newUnstructured("memcached.c5c3.io", "v1beta1", "Memcached", "test-memcached", "default")
	g.Expect(c.Get(ctx, key, updated)).To(Succeed())

	conditions, found, err := unstructured.NestedSlice(updated.Object, "status", "conditions")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeTrue())
	g.Expect(conditions).To(HaveLen(1), "expected exactly 1 condition after two calls")
}

func TestSimulateMemcachedReady_emptyServerList(t *testing.T) {
	g := NewGomegaWithT(t)

	memcached := newUnstructured("memcached.c5c3.io", "v1beta1", "Memcached", "test-memcached", "default")

	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(memcached).
		WithStatusSubresource(memcached).
		Build()

	g.Expect(SimulateMemcachedReady(context.Background(), c, client.ObjectKeyFromObject(memcached), 0, []string{})).To(Succeed())

	updated := newUnstructured("memcached.c5c3.io", "v1beta1", "Memcached", "test-memcached", "default")
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

	es := &esov1.ExternalSecret{
		ObjectMeta: metav1.ObjectMeta{Name: "test-es", Namespace: "default"},
	}

	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(es).
		WithStatusSubresource(es).
		Build()

	err := SimulateExternalSecretSync(context.Background(), c, client.ObjectKeyFromObject(es))
	g.Expect(err).NotTo(HaveOccurred())

	updated := &esov1.ExternalSecret{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(es), updated)).To(Succeed())

	g.Expect(updated.Status.RefreshTime.IsZero()).To(BeFalse())
	g.Expect(updated.Status.Conditions).To(HaveLen(1))

	cond := updated.Status.Conditions[0]
	g.Expect(cond.Type).To(Equal(esov1.ExternalSecretReady))
	g.Expect(cond.Status).To(Equal(corev1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("SecretSynced"))
}

func TestSimulateExternalSecretSync_idempotent(t *testing.T) {
	g := NewGomegaWithT(t)

	es := &esov1.ExternalSecret{
		ObjectMeta: metav1.ObjectMeta{Name: "test-es", Namespace: "default"},
	}

	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(es).
		WithStatusSubresource(es).
		Build()

	ctx := context.Background()
	key := client.ObjectKeyFromObject(es)

	g.Expect(SimulateExternalSecretSync(ctx, c, key)).To(Succeed())
	g.Expect(SimulateExternalSecretSync(ctx, c, key)).To(Succeed())

	updated := &esov1.ExternalSecret{}
	g.Expect(c.Get(ctx, key, updated)).To(Succeed())

	g.Expect(updated.Status.Conditions).To(HaveLen(1), "expected exactly 1 condition after two calls")
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
	g.Expect(updated.Status.StartTime).NotTo(BeNil())
	g.Expect(updated.Status.CompletionTime).NotTo(BeNil())
	g.Expect(updated.Status.Conditions).To(HaveLen(2))

	// SuccessCriteriaMet must come before Complete (K8s 1.35 requirement).
	g.Expect(updated.Status.Conditions[0].Type).To(Equal(batchv1.JobSuccessCriteriaMet))
	g.Expect(updated.Status.Conditions[0].Status).To(Equal(corev1.ConditionTrue))

	cond := updated.Status.Conditions[1]
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

	g.Expect(updated.Status.Conditions).To(HaveLen(2), "expected exactly 2 conditions after two calls")
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

// --- SimulateDeploymentReady ---

func TestSimulateDeploymentReady(t *testing.T) {
	g := NewGomegaWithT(t)

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-deploy",
			Namespace:  "default",
			Generation: 1,
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(deploy).
		WithStatusSubresource(deploy).
		Build()

	err := SimulateDeploymentReady(context.Background(), c, client.ObjectKeyFromObject(deploy), 3)
	g.Expect(err).NotTo(HaveOccurred())

	updated := &appsv1.Deployment{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(deploy), updated)).To(Succeed())

	g.Expect(updated.Status.ReadyReplicas).To(BeEquivalentTo(3))
	g.Expect(updated.Status.AvailableReplicas).To(BeEquivalentTo(3))
	g.Expect(updated.Status.Replicas).To(BeEquivalentTo(3))
	g.Expect(updated.Status.UpdatedReplicas).To(BeEquivalentTo(3))
	g.Expect(updated.Status.ObservedGeneration).To(Equal(int64(1)))
	g.Expect(updated.Status.Conditions).To(HaveLen(2))

	progressingCond := updated.Status.Conditions[0]
	g.Expect(progressingCond.Type).To(Equal(appsv1.DeploymentProgressing))
	g.Expect(progressingCond.Status).To(Equal(corev1.ConditionTrue))
	g.Expect(progressingCond.Reason).To(Equal("NewReplicaSetAvailable"))

	availableCond := updated.Status.Conditions[1]
	g.Expect(availableCond.Type).To(Equal(appsv1.DeploymentAvailable))
	g.Expect(availableCond.Status).To(Equal(corev1.ConditionTrue))
	g.Expect(availableCond.Reason).To(Equal("MinimumReplicasAvailable"))
}

func TestSimulateDeploymentReady_idempotent(t *testing.T) {
	g := NewGomegaWithT(t)

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-deploy",
			Namespace:  "default",
			Generation: 1,
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(deploy).
		WithStatusSubresource(deploy).
		Build()

	ctx := context.Background()
	key := client.ObjectKeyFromObject(deploy)

	g.Expect(SimulateDeploymentReady(ctx, c, key, 3)).To(Succeed())
	g.Expect(SimulateDeploymentReady(ctx, c, key, 3)).To(Succeed())

	updated := &appsv1.Deployment{}
	g.Expect(c.Get(ctx, key, updated)).To(Succeed())

	g.Expect(updated.Status.Conditions).To(HaveLen(2), "expected exactly 2 conditions after two calls")
}

func TestSimulateDeploymentReady_notFound(t *testing.T) {
	g := NewGomegaWithT(t)

	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		Build()

	key := client.ObjectKey{Name: "missing", Namespace: "default"}
	err := SimulateDeploymentReady(context.Background(), c, key, 3)
	g.Expect(err).To(HaveOccurred())
}

func TestSimulateDeploymentReady_IsDeploymentReadyReturnsTrue(t *testing.T) {
	g := NewGomegaWithT(t)

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-deploy",
			Namespace:  "default",
			Generation: 1,
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(deploy).
		WithStatusSubresource(deploy).
		Build()

	err := SimulateDeploymentReady(context.Background(), c, client.ObjectKeyFromObject(deploy), 3)
	g.Expect(err).NotTo(HaveOccurred())

	updated := &appsv1.Deployment{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(deploy), updated)).To(Succeed())

	g.Expect(deployment.IsDeploymentReady(updated)).To(BeTrue())
}

func TestSimulateDeploymentReady_zeroReplicas(t *testing.T) {
	g := NewGomegaWithT(t)

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-deploy",
			Namespace:  "default",
			Generation: 1,
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(deploy).
		WithStatusSubresource(deploy).
		Build()

	g.Expect(SimulateDeploymentReady(context.Background(), c, client.ObjectKeyFromObject(deploy), 0)).To(Succeed())

	updated := &appsv1.Deployment{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(deploy), updated)).To(Succeed())
	g.Expect(updated.Status.ReadyReplicas).To(BeEquivalentTo(0))
}
