// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = appsv1.AddToScheme(s)
	_ = policyv1.AddToScheme(s)
	_ = autoscalingv2.AddToScheme(s)
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
	// latest spec change: Generation > ObservedGeneration.
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

func TestEnsureDeployment_deletesAndRequeuesOnSelectorChange(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	// Pre-create a deployment with the old selector.
	existing := testDeployment()
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, existing).
		WithStatusSubresource(existing).
		Build()

	// Desired deployment has a different selector.
	desired := testDeployment()
	desired.Spec.Selector = &metav1.LabelSelector{
		MatchLabels: map[string]string{"app.kubernetes.io/name": "test", "app.kubernetes.io/instance": "test-instance"},
	}

	ready, err := EnsureDeployment(context.Background(), c, s, owner, desired)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse(), "should not be ready when deleted for selector migration")

	// The old Deployment should have been deleted.
	list := &appsv1.DeploymentList{}
	g.Expect(c.List(context.Background(), list, client.InNamespace("default"))).To(Succeed())
	g.Expect(list.Items).To(BeEmpty(), "Deployment should have been deleted for selector migration")
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

func TestEnsureDeployment_reconciles_ownerRef_on_update(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	// Create a Deployment without owner references (simulates out-of-band drift).
	existing := testDeployment()
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, existing).
		WithStatusSubresource(existing).
		Build()

	_, err := EnsureDeployment(context.Background(), c, s, owner, testDeployment())
	g.Expect(err).NotTo(HaveOccurred())

	fetched := &appsv1.Deployment{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(existing), fetched)).To(Succeed())
	g.Expect(fetched.OwnerReferences).To(HaveLen(1))
	g.Expect(fetched.OwnerReferences[0].Name).To(Equal("test-owner"))
}

func TestEnsureDeployment_merges_labels_on_update(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	// Existing Deployment has a user-added label.
	existing := testDeployment()
	existing.Labels = map[string]string{"user-key": "user-value"}
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, existing).
		WithStatusSubresource(existing).
		Build()

	// Desired Deployment introduces an operator-managed label.
	desired := testDeployment()
	desired.Labels = map[string]string{"operator-key": "operator-value"}

	_, err := EnsureDeployment(context.Background(), c, s, owner, desired)
	g.Expect(err).NotTo(HaveOccurred())

	fetched := &appsv1.Deployment{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(existing), fetched)).To(Succeed())
	// Both the user-added and operator-managed labels must be present.
	g.Expect(fetched.Labels).To(HaveKeyWithValue("user-key", "user-value"))
	g.Expect(fetched.Labels).To(HaveKeyWithValue("operator-key", "operator-value"))
}

func TestEnsureDeployment_preserves_labels_when_desired_labels_nil(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	// Existing Deployment has a user-added label.
	existing := testDeployment()
	existing.Labels = map[string]string{"user-key": "user-value"}
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, existing).
		WithStatusSubresource(existing).
		Build()

	// Desired Deployment does not specify any labels (Labels == nil).
	desired := testDeployment()
	desired.Labels = nil

	_, err := EnsureDeployment(context.Background(), c, s, owner, desired)
	g.Expect(err).NotTo(HaveOccurred())

	fetched := &appsv1.Deployment{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(existing), fetched)).To(Succeed())

	// User labels must be preserved when desired.Labels is nil.
	g.Expect(fetched.Labels).To(HaveKeyWithValue("user-key", "user-value"))
}

func TestEnsureDeployment_merges_annotations_on_update(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	existing := testDeployment()
	existing.Annotations = map[string]string{"existing-ann": "val"}
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, existing).
		WithStatusSubresource(existing).
		Build()

	desired := testDeployment()
	desired.Annotations = map[string]string{"new-ann": "new-val"}

	_, err := EnsureDeployment(context.Background(), c, s, owner, desired)
	g.Expect(err).NotTo(HaveOccurred())

	fetched := &appsv1.Deployment{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(existing), fetched)).To(Succeed())
	g.Expect(fetched.Annotations).To(HaveKeyWithValue("existing-ann", "val"))
	g.Expect(fetched.Annotations).To(HaveKeyWithValue("new-ann", "new-val"))
}

// TestEnsureDeployment_preservesLiveReplicasWhenDesiredNil verifies that when
// the desired Deployment leaves .spec.replicas nil (the caller deferred the
// field to an HPA), the update path keeps the live replica count instead of
// resetting it. This is the mechanism that stops the operator from fighting
// the HPA every reconcile (issue #462).
func TestEnsureDeployment_preservesLiveReplicasWhenDesiredNil(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	// The live Deployment has been scaled to 5 by the HPA.
	existing := testDeployment()
	existing.Spec.Replicas = ptr(int32(5))
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, existing).
		WithStatusSubresource(existing).
		Build()

	// The desired Deployment leaves replicas unset (HPA owns the count).
	desired := testDeployment()
	desired.Spec.Replicas = nil

	_, err := EnsureDeployment(context.Background(), c, s, owner, desired)
	g.Expect(err).NotTo(HaveOccurred())

	fetched := &appsv1.Deployment{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(existing), fetched)).To(Succeed())
	g.Expect(fetched.Spec.Replicas).NotTo(BeNil())
	g.Expect(*fetched.Spec.Replicas).To(Equal(int32(5)), "live HPA-owned replica count must be preserved when desired replicas is nil")
}

// TestEnsureDeployment_createsWithExplicitReplicasWhenDesiredNil verifies that
// the create path writes an explicit replica count even when the desired
// Deployment leaves .spec.replicas nil, so the object is not left for the API
// server to implicitly default. The fake client performs no defaulting, so a
// nil here would prove the operator failed to set the field (issue #462).
func TestEnsureDeployment_createsWithExplicitReplicasWhenDesiredNil(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner).
		Build()

	desired := testDeployment()
	desired.Spec.Replicas = nil

	_, err := EnsureDeployment(context.Background(), c, s, owner, desired)
	g.Expect(err).NotTo(HaveOccurred())

	created := &appsv1.Deployment{}
	g.Expect(c.Get(context.Background(), client.ObjectKey{Name: "test-deploy", Namespace: "default"}, created)).To(Succeed())
	g.Expect(created.Spec.Replicas).NotTo(BeNil(), "create path must set an explicit replica count, not rely on API-server defaulting")
	g.Expect(*created.Spec.Replicas).To(Equal(int32(1)))
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

	// The caller's object must remain unchanged.
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

	// Caller omits Protocol (zero value ""), NodePort left at 0.
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
// differs from the existing Service.
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
// that differ from the existing Service.
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
// that differ from the existing Service.
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
// values that match the existing Service.
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

func TestEnsureService_reconciles_ownerRef_on_update(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	// Create a Service without owner references (simulates out-of-band drift).
	existing := testService()
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, existing).
		Build()

	err := EnsureService(context.Background(), c, s, owner, testService())
	g.Expect(err).NotTo(HaveOccurred())

	fetched := &corev1.Service{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(existing), fetched)).To(Succeed())
	g.Expect(fetched.OwnerReferences).To(HaveLen(1))
	g.Expect(fetched.OwnerReferences[0].Name).To(Equal("test-owner"))
}

func TestEnsureService_merges_labels_on_update(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	// Existing Service has a user-added label.
	existing := testService()
	existing.Labels = map[string]string{"user-key": "user-value"}
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, existing).
		Build()

	// Desired Service introduces an operator-managed label.
	desired := testService()
	desired.Labels = map[string]string{"operator-key": "operator-value"}

	err := EnsureService(context.Background(), c, s, owner, desired)
	g.Expect(err).NotTo(HaveOccurred())

	fetched := &corev1.Service{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(existing), fetched)).To(Succeed())
	// Both the user-added and operator-managed labels must be present.
	g.Expect(fetched.Labels).To(HaveKeyWithValue("user-key", "user-value"))
	g.Expect(fetched.Labels).To(HaveKeyWithValue("operator-key", "operator-value"))
}

func TestEnsureService_preserves_labels_when_desired_labels_nil(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	// Existing Service has a user-added label.
	existing := testService()
	existing.Labels = map[string]string{"user-key": "user-value"}
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, existing).
		Build()

	// Desired Service does not specify any labels (Labels == nil).
	desired := testService()
	desired.Labels = nil

	err := EnsureService(context.Background(), c, s, owner, desired)
	g.Expect(err).NotTo(HaveOccurred())

	fetched := &corev1.Service{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(existing), fetched)).To(Succeed())

	// User labels must be preserved when desired.Labels is nil.
	g.Expect(fetched.Labels).To(HaveKeyWithValue("user-key", "user-value"))
}

func TestEnsureService_merges_annotations_on_update(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	existing := testService()
	existing.Annotations = map[string]string{"existing-ann": "val"}
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, existing).
		Build()

	desired := testService()
	desired.Annotations = map[string]string{"new-ann": "new-val"}

	err := EnsureService(context.Background(), c, s, owner, desired)
	g.Expect(err).NotTo(HaveOccurred())

	fetched := &corev1.Service{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(existing), fetched)).To(Succeed())
	g.Expect(fetched.Annotations).To(HaveKeyWithValue("existing-ann", "val"))
	g.Expect(fetched.Annotations).To(HaveKeyWithValue("new-ann", "new-val"))
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

// --- EnsurePDB ---

func testPDB() *policyv1.PodDisruptionBudget {
	minAvailable := intstr.FromInt32(1)
	return &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pdb",
			Namespace: "default",
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: &minAvailable,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "test"},
			},
		},
	}
}

func TestEnsurePDB_creates(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner).
		Build()

	err := EnsurePDB(context.Background(), c, s, owner, testPDB())
	g.Expect(err).NotTo(HaveOccurred())

	created := &policyv1.PodDisruptionBudget{}
	g.Expect(c.Get(context.Background(), client.ObjectKey{Name: "test-pdb", Namespace: "default"}, created)).To(Succeed())
	g.Expect(created.OwnerReferences).To(HaveLen(1))
	g.Expect(created.OwnerReferences[0].Name).To(Equal("test-owner"))
}

func TestEnsurePDB_updatesExisting(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()
	existing := testPDB()

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, existing).
		Build()

	updated := testPDB()
	maxUnavailable := intstr.FromInt32(1)
	updated.Spec.MinAvailable = nil
	updated.Spec.MaxUnavailable = &maxUnavailable

	err := EnsurePDB(context.Background(), c, s, owner, updated)
	g.Expect(err).NotTo(HaveOccurred())

	fetched := &policyv1.PodDisruptionBudget{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(existing), fetched)).To(Succeed())
	g.Expect(fetched.Spec.MinAvailable).To(BeNil())
	g.Expect(fetched.Spec.MaxUnavailable).NotTo(BeNil())
	g.Expect(fetched.Spec.MaxUnavailable.IntValue()).To(Equal(1))
}

func TestEnsurePDB_idempotent(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner).
		Build()

	ctx := context.Background()
	g.Expect(EnsurePDB(ctx, c, s, owner, testPDB())).To(Succeed())
	g.Expect(EnsurePDB(ctx, c, s, owner, testPDB())).To(Succeed())

	list := &policyv1.PodDisruptionBudgetList{}
	g.Expect(c.List(ctx, list, client.InNamespace("default"))).To(Succeed())
	g.Expect(list.Items).To(HaveLen(1))
}

func TestEnsurePDB_reconciles_ownerRef_on_update(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	// Create a PDB without owner references (simulates out-of-band drift).
	existing := testPDB()
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, existing).
		Build()

	err := EnsurePDB(context.Background(), c, s, owner, testPDB())
	g.Expect(err).NotTo(HaveOccurred())

	fetched := &policyv1.PodDisruptionBudget{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(existing), fetched)).To(Succeed())
	g.Expect(fetched.OwnerReferences).To(HaveLen(1))
	g.Expect(fetched.OwnerReferences[0].Name).To(Equal("test-owner"))
}

func TestEnsurePDB_merges_labels_on_update(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	// Existing PDB has a user-added label.
	existing := testPDB()
	existing.Labels = map[string]string{"user-key": "user-value"}
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, existing).
		Build()

	// Desired PDB introduces an operator-managed label.
	desired := testPDB()
	desired.Labels = map[string]string{"operator-key": "operator-value"}

	err := EnsurePDB(context.Background(), c, s, owner, desired)
	g.Expect(err).NotTo(HaveOccurred())

	fetched := &policyv1.PodDisruptionBudget{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(existing), fetched)).To(Succeed())
	// Both the user-added and operator-managed labels must be present.
	g.Expect(fetched.Labels).To(HaveKeyWithValue("user-key", "user-value"))
	g.Expect(fetched.Labels).To(HaveKeyWithValue("operator-key", "operator-value"))
}

func TestEnsurePDB_preserves_labels_when_desired_labels_nil(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	// Existing PDB has a user-added label.
	existing := testPDB()
	existing.Labels = map[string]string{"user-key": "user-value"}
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, existing).
		Build()

	// Desired PDB does not specify any labels (Labels == nil).
	desired := testPDB()
	desired.Labels = nil

	err := EnsurePDB(context.Background(), c, s, owner, desired)
	g.Expect(err).NotTo(HaveOccurred())

	fetched := &policyv1.PodDisruptionBudget{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(existing), fetched)).To(Succeed())

	// User labels must be preserved when desired.Labels is nil.
	g.Expect(fetched.Labels).To(HaveKeyWithValue("user-key", "user-value"))
}

func TestEnsurePDB_merges_annotations_on_update(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	existing := testPDB()
	existing.Annotations = map[string]string{"existing-ann": "val"}
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, existing).
		Build()

	desired := testPDB()
	desired.Annotations = map[string]string{"new-ann": "new-val"}

	err := EnsurePDB(context.Background(), c, s, owner, desired)
	g.Expect(err).NotTo(HaveOccurred())

	fetched := &policyv1.PodDisruptionBudget{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(existing), fetched)).To(Succeed())
	g.Expect(fetched.Annotations).To(HaveKeyWithValue("existing-ann", "val"))
	g.Expect(fetched.Annotations).To(HaveKeyWithValue("new-ann", "new-val"))
}

// --- EnsureHPA ---

func testHPA() *autoscalingv2.HorizontalPodAutoscaler {
	return &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-hpa",
			Namespace: "default",
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       "test-deploy",
			},
			MinReplicas: ptr(int32(1)),
			MaxReplicas: 5,
			Metrics: []autoscalingv2.MetricSpec{
				{
					Type: autoscalingv2.ResourceMetricSourceType,
					Resource: &autoscalingv2.ResourceMetricSource{
						Name: corev1.ResourceCPU,
						Target: autoscalingv2.MetricTarget{
							Type:               autoscalingv2.UtilizationMetricType,
							AverageUtilization: ptr(int32(80)),
						},
					},
				},
			},
		},
	}
}

func TestEnsureHPA_creates(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner).
		Build()

	err := EnsureHPA(context.Background(), c, s, owner, testHPA())
	g.Expect(err).NotTo(HaveOccurred())

	created := &autoscalingv2.HorizontalPodAutoscaler{}
	g.Expect(c.Get(context.Background(), client.ObjectKey{Name: "test-hpa", Namespace: "default"}, created)).To(Succeed())
	g.Expect(created.OwnerReferences).To(HaveLen(1))
	g.Expect(created.OwnerReferences[0].Name).To(Equal("test-owner"))
}

func TestEnsureHPA_updatesExisting(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()
	existing := testHPA()

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, existing).
		Build()

	updated := testHPA()
	updated.Spec.MaxReplicas = 10

	err := EnsureHPA(context.Background(), c, s, owner, updated)
	g.Expect(err).NotTo(HaveOccurred())

	fetched := &autoscalingv2.HorizontalPodAutoscaler{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(existing), fetched)).To(Succeed())
	g.Expect(fetched.Spec.MaxReplicas).To(Equal(int32(10)))
}

func TestEnsureHPA_idempotent(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner).
		Build()

	ctx := context.Background()
	g.Expect(EnsureHPA(ctx, c, s, owner, testHPA())).To(Succeed())
	g.Expect(EnsureHPA(ctx, c, s, owner, testHPA())).To(Succeed())

	list := &autoscalingv2.HorizontalPodAutoscalerList{}
	g.Expect(c.List(ctx, list, client.InNamespace("default"))).To(Succeed())
	g.Expect(list.Items).To(HaveLen(1))
}

func TestEnsureHPA_reconciles_ownerRef_on_update(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	// Create an HPA without owner references (simulates out-of-band drift).
	existing := testHPA()
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, existing).
		Build()

	err := EnsureHPA(context.Background(), c, s, owner, testHPA())
	g.Expect(err).NotTo(HaveOccurred())

	fetched := &autoscalingv2.HorizontalPodAutoscaler{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(existing), fetched)).To(Succeed())
	g.Expect(fetched.OwnerReferences).To(HaveLen(1))
	g.Expect(fetched.OwnerReferences[0].Name).To(Equal("test-owner"))
}

func TestEnsureHPA_merges_labels_on_update(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	// Existing HPA has a user-added label.
	existing := testHPA()
	existing.Labels = map[string]string{"user-key": "user-value"}
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, existing).
		Build()

	// Desired HPA introduces an operator-managed label.
	desired := testHPA()
	desired.Labels = map[string]string{"operator-key": "operator-value"}

	err := EnsureHPA(context.Background(), c, s, owner, desired)
	g.Expect(err).NotTo(HaveOccurred())

	fetched := &autoscalingv2.HorizontalPodAutoscaler{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(existing), fetched)).To(Succeed())
	// Both the user-added and operator-managed labels must be present.
	g.Expect(fetched.Labels).To(HaveKeyWithValue("user-key", "user-value"))
	g.Expect(fetched.Labels).To(HaveKeyWithValue("operator-key", "operator-value"))
}

func TestEnsureHPA_preserves_labels_when_desired_labels_nil(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	// Existing HPA has a user-added label.
	existing := testHPA()
	existing.Labels = map[string]string{"user-key": "user-value"}
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, existing).
		Build()

	// Desired HPA does not specify any labels (Labels == nil).
	desired := testHPA()
	desired.Labels = nil

	err := EnsureHPA(context.Background(), c, s, owner, desired)
	g.Expect(err).NotTo(HaveOccurred())

	fetched := &autoscalingv2.HorizontalPodAutoscaler{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(existing), fetched)).To(Succeed())

	// User labels must be preserved when desired.Labels is nil.
	g.Expect(fetched.Labels).To(HaveKeyWithValue("user-key", "user-value"))
}

func TestEnsureHPA_merges_annotations_on_update(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	existing := testHPA()
	existing.Annotations = map[string]string{"existing-ann": "val"}
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, existing).
		Build()

	desired := testHPA()
	desired.Annotations = map[string]string{"new-ann": "new-val"}

	err := EnsureHPA(context.Background(), c, s, owner, desired)
	g.Expect(err).NotTo(HaveOccurred())

	fetched := &autoscalingv2.HorizontalPodAutoscaler{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(existing), fetched)).To(Succeed())
	g.Expect(fetched.Annotations).To(HaveKeyWithValue("existing-ann", "val"))
	g.Expect(fetched.Annotations).To(HaveKeyWithValue("new-ann", "new-val"))
}

// --- DeleteHPA ---

func TestDeleteHPA_deletes(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()

	existing := testHPA()
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(existing).
		Build()

	err := DeleteHPA(context.Background(), c, "default", "test-hpa")
	g.Expect(err).NotTo(HaveOccurred())

	list := &autoscalingv2.HorizontalPodAutoscalerList{}
	g.Expect(c.List(context.Background(), list, client.InNamespace("default"))).To(Succeed())
	g.Expect(list.Items).To(BeEmpty())
}

func TestDeleteHPA_noop_when_absent(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()

	c := fake.NewClientBuilder().
		WithScheme(s).
		Build()

	err := DeleteHPA(context.Background(), c, "default", "nonexistent-hpa")
	g.Expect(err).NotTo(HaveOccurred())
}
