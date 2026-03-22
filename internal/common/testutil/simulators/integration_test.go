// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package simulators

import (
	"testing"

	. "github.com/onsi/gomega"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	envtestutil "github.com/c5c3/forge/internal/common/testutil/envtest"
)

// Feature: CC-0002

// newUnstructuredCR creates a minimal unstructured custom resource with the
// given GVK, name, and namespace.
func newUnstructuredCR(group, version, kind, name, namespace string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group: group, Version: version, Kind: kind,
	})
	obj.SetName(name)
	obj.SetNamespace(namespace)
	return obj
}

// --- SimulateMariaDBReady integration ---

func TestIntegration_SimulateMariaDBReady(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, cancel := envtestutil.SetupEnvTest(t)
	defer cancel()

	// Create namespace.
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-mariadb-ns"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	// Create the MariaDB resource.
	mariadb := newUnstructuredCR("k8s.mariadb.com", "v1alpha1", "MariaDB", "test-mariadb", ns.Name)
	g.Expect(c.Create(ctx, mariadb)).To(Succeed())

	key := client.ObjectKey{Name: "test-mariadb", Namespace: ns.Name}

	// Call the simulator.
	g.Expect(SimulateMariaDBReady(ctx, c, key, 3)).To(Succeed())

	// Verify status via a fresh Get.
	updated := newUnstructuredCR("k8s.mariadb.com", "v1alpha1", "MariaDB", "test-mariadb", ns.Name)
	g.Expect(c.Get(ctx, key, updated)).To(Succeed())

	readyReplicas, found, _ := unstructured.NestedInt64(updated.Object, "status", "readyReplicas")
	g.Expect(found).To(BeTrue())
	g.Expect(readyReplicas).To(BeEquivalentTo(3))

	conditions, found, _ := unstructured.NestedSlice(updated.Object, "status", "conditions")
	g.Expect(found).To(BeTrue())
	g.Expect(conditions).NotTo(BeEmpty())

	cond := conditions[0].(map[string]interface{})
	g.Expect(cond["type"]).To(Equal("Ready"))
	g.Expect(cond["status"]).To(Equal("True"))
}

// --- SimulateMemcachedReady integration ---

func TestIntegration_SimulateMemcachedReady(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, cancel := envtestutil.SetupEnvTest(t)
	defer cancel()

	// Create namespace.
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-memcached-ns"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	// Create the Memcached resource.
	memcached := newUnstructuredCR("memcached.c5c3.io", "v1beta1", "Memcached", "test-memcached", ns.Name)
	g.Expect(c.Create(ctx, memcached)).To(Succeed())

	key := client.ObjectKey{Name: "test-memcached", Namespace: ns.Name}
	servers := []string{"mc-0.mc:11211", "mc-1.mc:11211"}

	// Call the simulator.
	g.Expect(SimulateMemcachedReady(ctx, c, key, 2, servers)).To(Succeed())

	// Verify status via a fresh Get.
	updated := newUnstructuredCR("memcached.c5c3.io", "v1beta1", "Memcached", "test-memcached", ns.Name)
	g.Expect(c.Get(ctx, key, updated)).To(Succeed())

	readyReplicas, found, _ := unstructured.NestedInt64(updated.Object, "status", "readyReplicas")
	g.Expect(found).To(BeTrue())
	g.Expect(readyReplicas).To(BeEquivalentTo(2))

	sl, found, _ := unstructured.NestedStringSlice(updated.Object, "status", "serverList")
	g.Expect(found).To(BeTrue())
	g.Expect(sl).To(Equal([]string{"mc-0.mc:11211", "mc-1.mc:11211"}))

	conditions, found, _ := unstructured.NestedSlice(updated.Object, "status", "conditions")
	g.Expect(found).To(BeTrue())
	g.Expect(conditions).NotTo(BeEmpty())

	cond := conditions[0].(map[string]interface{})
	g.Expect(cond["type"]).To(Equal("Ready"))
	g.Expect(cond["status"]).To(Equal("True"))
}

// --- SimulateExternalSecretSync integration ---

func TestIntegration_SimulateExternalSecretSync(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, cancel := envtestutil.SetupEnvTest(t)
	defer cancel()

	// Create namespace.
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-es-ns"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	// Create the ExternalSecret resource.
	es := newUnstructuredCR("external-secrets.io", "v1beta1", "ExternalSecret", "test-es", ns.Name)
	g.Expect(c.Create(ctx, es)).To(Succeed())

	key := client.ObjectKey{Name: "test-es", Namespace: ns.Name}

	// Call the simulator.
	g.Expect(SimulateExternalSecretSync(ctx, c, key)).To(Succeed())

	// Verify status via a fresh Get.
	updated := newUnstructuredCR("external-secrets.io", "v1beta1", "ExternalSecret", "test-es", ns.Name)
	g.Expect(c.Get(ctx, key, updated)).To(Succeed())

	refreshTime, found, _ := unstructured.NestedString(updated.Object, "status", "refreshTime")
	g.Expect(found).To(BeTrue())
	g.Expect(refreshTime).NotTo(BeEmpty())

	conditions, found, _ := unstructured.NestedSlice(updated.Object, "status", "conditions")
	g.Expect(found).To(BeTrue())
	g.Expect(conditions).NotTo(BeEmpty())

	cond := conditions[0].(map[string]interface{})
	g.Expect(cond["type"]).To(Equal("Ready"))
	g.Expect(cond["status"]).To(Equal("True"))
	g.Expect(cond["reason"]).To(Equal("SecretSynced"))
}

// --- SimulateJobComplete integration ---

func TestIntegration_SimulateJobComplete(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, cancel := envtestutil.SetupEnvTest(t)
	defer cancel()

	// Create namespace.
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-job-ns"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	// Create the Job resource using the typed API.
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-job",
			Namespace: ns.Name,
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:    "worker",
							Image:   "busybox",
							Command: []string{"echo", "done"},
						},
					},
				},
			},
		},
	}
	g.Expect(c.Create(ctx, job)).To(Succeed())

	key := client.ObjectKey{Name: "test-job", Namespace: ns.Name}

	// Call the simulator.
	g.Expect(SimulateJobComplete(ctx, c, key)).To(Succeed())

	// Verify status via a fresh Get.
	updated := &batchv1.Job{}
	g.Expect(c.Get(ctx, key, updated)).To(Succeed())

	g.Expect(updated.Status.Succeeded).To(BeEquivalentTo(1))
	g.Expect(updated.Status.CompletionTime).NotTo(BeNil())
	g.Expect(updated.Status.Conditions).NotTo(BeEmpty())

	cond := updated.Status.Conditions[0]
	g.Expect(cond.Type).To(Equal(batchv1.JobComplete))
	g.Expect(cond.Status).To(Equal(corev1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("Completed"))
}
