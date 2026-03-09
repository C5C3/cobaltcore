// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Feature: CC-0005

func newScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = appsv1.AddToScheme(s)
	return s
}

func ptr[T any](v T) *T { return &v }

func testOwner() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-owner",
			Namespace: "default",
			UID:       "test-uid",
		},
	}
}

func testDeployment() *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-deploy",
			Namespace: "default",
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr(int32(3)),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "test"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "test"},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "test", Image: "test:latest"},
					},
				},
			},
		},
	}
}

func testService() *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-svc",
			Namespace: "default",
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "test"},
			Ports: []corev1.ServicePort{
				{Port: 80, Protocol: corev1.ProtocolTCP},
			},
		},
	}
}

// --- EnsureDeployment ---

func TestEnsureDeployment_creates(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner).
		Build()

	ready, err := EnsureDeployment(context.Background(), c, s, owner, testDeployment())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse(), "newly created deployment should not be ready")

	created := &appsv1.Deployment{}
	g.Expect(c.Get(context.Background(), client.ObjectKey{Name: "test-deploy", Namespace: "default"}, created)).To(Succeed())
	g.Expect(created.OwnerReferences).To(HaveLen(1))
	g.Expect(created.OwnerReferences[0].Name).To(Equal("test-owner"))
}

func TestEnsureDeployment_updatesExisting(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()
	existing := testDeployment()

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, existing).
		WithStatusSubresource(existing).
		Build()

	updated := testDeployment()
	updated.Spec.Replicas = ptr(int32(5))

	ready, err := EnsureDeployment(context.Background(), c, s, owner, updated)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse())

	fetched := &appsv1.Deployment{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(existing), fetched)).To(Succeed())
	g.Expect(*fetched.Spec.Replicas).To(Equal(int32(5)))
}

func TestEnsureDeployment_readyWhenAvailable(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	existing := testDeployment()
	existing.Generation = 2
	existing.Status.ObservedGeneration = 2
	existing.Status.ReadyReplicas = 3
	existing.Status.Conditions = []appsv1.DeploymentCondition{
		{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue},
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, existing).
		WithStatusSubresource(existing).
		Build()

	ready, err := EnsureDeployment(context.Background(), c, s, owner, testDeployment())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeTrue())
}

func TestEnsureDeployment_notReadyWhenGenerationLags(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	// Simulate a deployment where the controller has not yet processed the
	// latest spec change: Generation > ObservedGeneration (CC-0005).
	existing := testDeployment()
	existing.Generation = 2
	existing.Status.ObservedGeneration = 1
	existing.Status.ReadyReplicas = 3
	existing.Status.Conditions = []appsv1.DeploymentCondition{
		{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue},
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, existing).
		WithStatusSubresource(existing).
		Build()

	ready, err := EnsureDeployment(context.Background(), c, s, owner, testDeployment())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse(), "should return false when ObservedGeneration lags behind Generation on update path")
}

func TestEnsureDeployment_idempotent(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner).
		Build()

	ctx := context.Background()
	_, err := EnsureDeployment(ctx, c, s, owner, testDeployment())
	g.Expect(err).NotTo(HaveOccurred())

	_, err = EnsureDeployment(ctx, c, s, owner, testDeployment())
	g.Expect(err).NotTo(HaveOccurred())

	list := &appsv1.DeploymentList{}
	g.Expect(c.List(ctx, list, client.InNamespace("default"))).To(Succeed())
	g.Expect(list.Items).To(HaveLen(1))
}

// --- EnsureService ---

func TestEnsureService_creates(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner).
		Build()

	err := EnsureService(context.Background(), c, s, owner, testService())
	g.Expect(err).NotTo(HaveOccurred())

	created := &corev1.Service{}
	g.Expect(c.Get(context.Background(), client.ObjectKey{Name: "test-svc", Namespace: "default"}, created)).To(Succeed())
	g.Expect(created.OwnerReferences).To(HaveLen(1))
	g.Expect(created.OwnerReferences[0].Name).To(Equal("test-owner"))
	g.Expect(created.Spec.Ports).To(HaveLen(1))
	g.Expect(created.Spec.Ports[0].Port).To(Equal(int32(80)))
}

func TestEnsureService_updatesPreservingClusterIP(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	existing := testService()
	existing.Spec.ClusterIP = "10.0.0.42"
	existing.Spec.ClusterIPs = []string{"10.0.0.42"}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, existing).
		Build()

	updated := testService()
	updated.Spec.Ports = []corev1.ServicePort{
		{Port: 8080, Protocol: corev1.ProtocolTCP},
	}

	err := EnsureService(context.Background(), c, s, owner, updated)
	g.Expect(err).NotTo(HaveOccurred())

	fetched := &corev1.Service{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(existing), fetched)).To(Succeed())
	g.Expect(fetched.Spec.ClusterIP).To(Equal("10.0.0.42"))
	g.Expect(fetched.Spec.ClusterIPs).To(Equal([]string{"10.0.0.42"}))
	g.Expect(fetched.Spec.Ports[0].Port).To(Equal(int32(8080)))
}

func TestEnsureService_doesNotMutateCallerObject(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	existing := testService()
	existing.Spec.ClusterIP = "10.0.0.42"
	existing.Spec.ClusterIPs = []string{"10.0.0.42"}
	existing.Spec.Type = corev1.ServiceTypeNodePort
	existing.Spec.Ports = []corev1.ServicePort{
		{Port: 80, Protocol: corev1.ProtocolTCP, NodePort: 30080},
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, existing).
		Build()

	desired := testService()
	desired.Spec.Type = corev1.ServiceTypeNodePort
	desired.Spec.Ports = []corev1.ServicePort{
		{Port: 80, Protocol: corev1.ProtocolTCP},
	}

	// Snapshot the desired values before the call.
	originalClusterIP := desired.Spec.ClusterIP
	originalClusterIPs := desired.Spec.ClusterIPs
	originalNodePort := desired.Spec.Ports[0].NodePort

	err := EnsureService(context.Background(), c, s, owner, desired)
	g.Expect(err).NotTo(HaveOccurred())

	// The caller's object must remain unchanged (CC-0005).
	g.Expect(desired.Spec.ClusterIP).To(Equal(originalClusterIP), "caller's ClusterIP must not be mutated")
	g.Expect(desired.Spec.ClusterIPs).To(Equal(originalClusterIPs), "caller's ClusterIPs must not be mutated")
	g.Expect(desired.Spec.Ports[0].NodePort).To(Equal(originalNodePort), "caller's NodePort must not be mutated")
}

func TestEnsureService_updatesPreservingNodePorts(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	existing := testService()
	existing.Spec.Type = corev1.ServiceTypeNodePort
	existing.Spec.Ports = []corev1.ServicePort{
		{Port: 80, Protocol: corev1.ProtocolTCP, NodePort: 30080},
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, existing).
		Build()

	updated := testService()
	updated.Spec.Type = corev1.ServiceTypeNodePort
	// NodePort intentionally left at 0 (not set by caller).
	updated.Spec.Ports = []corev1.ServicePort{
		{Port: 80, Protocol: corev1.ProtocolTCP},
	}

	err := EnsureService(context.Background(), c, s, owner, updated)
	g.Expect(err).NotTo(HaveOccurred())

	fetched := &corev1.Service{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(existing), fetched)).To(Succeed())
	g.Expect(fetched.Spec.Ports[0].NodePort).To(Equal(int32(30080)), "NodePort should be preserved from the existing Service")
}

func TestEnsureService_preservesNodePortWhenProtocolOmitted(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	// Existing port has Protocol "TCP" (defaulted by the API server) and an
	// auto-assigned NodePort.
	existing := testService()
	existing.Spec.Type = corev1.ServiceTypeNodePort
	existing.Spec.Ports = []corev1.ServicePort{
		{Port: 80, Protocol: corev1.ProtocolTCP, NodePort: 30080},
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, existing).
		Build()

	// Caller omits Protocol (zero value ""), NodePort left at 0 (CC-0005).
	updated := testService()
	updated.Spec.Type = corev1.ServiceTypeNodePort
	updated.Spec.Ports = []corev1.ServicePort{
		{Port: 80},
	}

	err := EnsureService(context.Background(), c, s, owner, updated)
	g.Expect(err).NotTo(HaveOccurred())

	fetched := &corev1.Service{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(existing), fetched)).To(Succeed())
	g.Expect(fetched.Spec.Ports[0].NodePort).To(Equal(int32(30080)), "NodePort should be preserved even when caller omits Protocol")
}

func TestEnsureService_idempotent(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner).
		Build()

	ctx := context.Background()
	g.Expect(EnsureService(ctx, c, s, owner, testService())).To(Succeed())
	g.Expect(EnsureService(ctx, c, s, owner, testService())).To(Succeed())

	list := &corev1.ServiceList{}
	g.Expect(c.List(ctx, list, client.InNamespace("default"))).To(Succeed())
	g.Expect(list.Items).To(HaveLen(1))
}

// TestEnsureService_failsOnClusterIPConflict verifies that EnsureService returns
// an error when the desired spec explicitly sets ClusterIP to a value that
// differs from the existing Service (CC-0005).
func TestEnsureService_failsOnClusterIPConflict(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	existing := testService()
	existing.Spec.ClusterIP = "10.0.0.42"
	existing.Spec.ClusterIPs = []string{"10.0.0.42"}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, existing).
		Build()

	desired := testService()
	desired.Spec.ClusterIP = "10.0.0.99"

	err := EnsureService(context.Background(), c, s, owner, desired)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("ClusterIP"))
	g.Expect(err.Error()).To(ContainSubstring("immutable"))
}

// TestEnsureService_failsOnClusterIPsConflict verifies that EnsureService
// returns an error when the desired spec explicitly sets ClusterIPs to values
// that differ from the existing Service (CC-0005).
func TestEnsureService_failsOnClusterIPsConflict(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	existing := testService()
	existing.Spec.ClusterIPs = []string{"10.0.0.42"}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, existing).
		Build()

	desired := testService()
	desired.Spec.ClusterIPs = []string{"10.0.0.99"}

	err := EnsureService(context.Background(), c, s, owner, desired)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("ClusterIPs"))
}

// TestEnsureService_failsOnIPFamiliesConflict verifies that EnsureService
// returns an error when the desired spec explicitly sets IPFamilies to values
// that differ from the existing Service (CC-0005).
func TestEnsureService_failsOnIPFamiliesConflict(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	existing := testService()
	existing.Spec.IPFamilies = []corev1.IPFamily{corev1.IPv4Protocol}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, existing).
		Build()

	desired := testService()
	desired.Spec.IPFamilies = []corev1.IPFamily{corev1.IPv6Protocol}

	err := EnsureService(context.Background(), c, s, owner, desired)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("IPFamilies"))
}

// TestEnsureService_allowsMatchingImmutableFields verifies that EnsureService
// succeeds when the desired spec sets ClusterIP/ClusterIPs/IPFamilies to
// values that match the existing Service (CC-0005).
func TestEnsureService_allowsMatchingImmutableFields(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	existing := testService()
	existing.Spec.ClusterIP = "10.0.0.42"
	existing.Spec.ClusterIPs = []string{"10.0.0.42"}
	existing.Spec.IPFamilies = []corev1.IPFamily{corev1.IPv4Protocol}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, existing).
		Build()

	desired := testService()
	desired.Spec.ClusterIP = "10.0.0.42"
	desired.Spec.ClusterIPs = []string{"10.0.0.42"}
	desired.Spec.IPFamilies = []corev1.IPFamily{corev1.IPv4Protocol}

	err := EnsureService(context.Background(), c, s, owner, desired)
	g.Expect(err).NotTo(HaveOccurred())
}

// --- IsDeploymentReady ---

func TestIsDeploymentReady_true(t *testing.T) {
	g := NewGomegaWithT(t)
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Generation: 2},
		Spec:       appsv1.DeploymentSpec{Replicas: ptr(int32(3))},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 2,
			ReadyReplicas:      3,
			Conditions: []appsv1.DeploymentCondition{
				{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue},
			},
		},
	}
	g.Expect(IsDeploymentReady(deploy)).To(BeTrue())
}

func TestIsDeploymentReady_false_observedGenerationLag(t *testing.T) {
	g := NewGomegaWithT(t)
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Generation: 3},
		Spec:       appsv1.DeploymentSpec{Replicas: ptr(int32(3))},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 2,
			ReadyReplicas:      3,
			Conditions: []appsv1.DeploymentCondition{
				{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue},
			},
		},
	}
	g.Expect(IsDeploymentReady(deploy)).To(BeFalse(), "should return false when ObservedGeneration lags behind Generation")
}

func TestIsDeploymentReady_false_notEnoughReplicas(t *testing.T) {
	g := NewGomegaWithT(t)
	deploy := &appsv1.Deployment{
		Spec: appsv1.DeploymentSpec{Replicas: ptr(int32(3))},
		Status: appsv1.DeploymentStatus{
			ReadyReplicas: 1,
			Conditions: []appsv1.DeploymentCondition{
				{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue},
			},
		},
	}
	g.Expect(IsDeploymentReady(deploy)).To(BeFalse())
}

func TestIsDeploymentReady_false_noCondition(t *testing.T) {
	g := NewGomegaWithT(t)
	deploy := &appsv1.Deployment{
		Spec:   appsv1.DeploymentSpec{Replicas: ptr(int32(1))},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 1},
	}
	g.Expect(IsDeploymentReady(deploy)).To(BeFalse())
}

func TestIsDeploymentReady_false_noStatus(t *testing.T) {
	g := NewGomegaWithT(t)
	deploy := &appsv1.Deployment{
		Spec: appsv1.DeploymentSpec{Replicas: ptr(int32(3))},
	}
	g.Expect(IsDeploymentReady(deploy)).To(BeFalse())
}

func TestIsDeploymentReady_nilReplicas_defaults1(t *testing.T) {
	g := NewGomegaWithT(t)
	deploy := &appsv1.Deployment{
		Status: appsv1.DeploymentStatus{
			ReadyReplicas: 1,
			Conditions: []appsv1.DeploymentCondition{
				{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue},
			},
		},
	}
	g.Expect(IsDeploymentReady(deploy)).To(BeTrue())
}
