// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the declarative service-account sub-reconciler
// reconcileServiceAccounts (spec.korc.serviceAccounts).
package controller

import (
	"context"
	"testing"

	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/c5c3/forge/internal/common/conditions"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
)

// saControlPlane returns a managed ControlPlane with AdminCredentialReady already
// satisfied and one declared service account, so reconcileServiceAccounts runs
// past its gates. Tests mutate the single declared account as needed.
func saControlPlane() *c5c3v1alpha1.ControlPlane {
	cp := korcControlPlane()
	setAdminCredentialReady(cp)
	cp.Spec.KORC.ServiceAccounts = []c5c3v1alpha1.ServiceAccountSpec{{
		Name:    "nova",
		Project: c5c3v1alpha1.ServiceAccountProjectSpec{Name: "service"},
	}}
	return cp
}

// runServiceAccounts runs reconcileServiceAccounts against cp with the given
// seeded objects (plus a ready ClusterSecretStore) and returns the resulting
// ServiceAccountsReady condition and the client.
func runServiceAccounts(t *testing.T, cp *c5c3v1alpha1.ControlPlane, objs ...client.Object) (*metav1.Condition, client.Client) {
	t.Helper()
	g := NewGomegaWithT(t)
	s := korcTestScheme(t)
	all := append([]client.Object{cp, readyClusterSecretStore()}, objs...)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(all...).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(20)}
	_, err := r.reconcileServiceAccounts(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	return conditions.GetCondition(cp.Status.Conditions, conditionTypeServiceAccountsReady), c
}

// ownedByCP hand-builds a controller OwnerReference to cp so metav1.IsControlledBy
// recognises a seeded child as swept by the prune sweep.
func ownedByCP(cp *c5c3v1alpha1.ControlPlane) []metav1.OwnerReference {
	return []metav1.OwnerReference{{
		APIVersion:         c5c3v1alpha1.GroupVersion.String(),
		Kind:               "ControlPlane",
		Name:               cp.Name,
		UID:                cp.UID,
		Controller:         ptr.To(true),
		BlockOwnerDeletion: ptr.To(true),
	}}
}

func getUserByName(t *testing.T, c client.Client, name, ns string) (*orcv1alpha1.User, bool) {
	t.Helper()
	u := &orcv1alpha1.User{}
	err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, u)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		t.Fatalf("getting User %q: %v", name, err)
	}
	return u, true
}

func TestReconcileServiceAccounts_EmptyListReady(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := korcControlPlane()
	setAdminCredentialReady(cp)

	cond, _ := runServiceAccounts(t, cp)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(reasonNoServiceAccountsDeclared))
}

func TestReconcileServiceAccounts_WaitingForAdminCredential(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := saControlPlane()
	// Drop the gate condition.
	cp.Status.Conditions = nil

	cond, _ := runServiceAccounts(t, cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonWaitingForServiceAccountAdmin))
}

func TestReconcileServiceAccounts_SecretStoreNotReady(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := saControlPlane()
	s := korcTestScheme(t)
	// No ClusterSecretStore seeded => IsClusterSecretStoreReady reports not ready.
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(20)}
	_, err := r.reconcileServiceAccounts(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeServiceAccountsReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonServiceAccountStoreNotReady))
}

func TestReconcileServiceAccounts_ProbeAbsentCreatesManagedUserAndReferencedProject(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := saControlPlane()
	sa := cp.Spec.KORC.ServiceAccounts[0]
	ns := childNamespace(cp)
	// Seed the user probe reporting the user does NOT exist yet (pending-external).
	probe := &orcv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: serviceAccountUserProbeRef(cp, sa), Namespace: ns},
		Status:     orcv1alpha1.UserStatus{Conditions: pendingImportConditions(0)},
	}

	cond, c := runServiceAccounts(t, cp, probe)

	// The probe is dropped once it reports absent.
	_, probeExists := getUserByName(t, c, serviceAccountUserProbeRef(cp, sa), ns)
	g.Expect(probeExists).To(BeFalse(), "the resolved probe must be deleted")

	// The managed User is created with generation-1 password and annotation.
	user, ok := getUserByName(t, c, serviceAccountUserRef(cp, sa), ns)
	g.Expect(ok).To(BeTrue())
	g.Expect(user.Spec.ManagementPolicy).To(Equal(orcv1alpha1.ManagementPolicyManaged))
	g.Expect(user.Spec.Resource).NotTo(BeNil())
	g.Expect(user.Spec.Resource.PasswordRef).NotTo(BeNil())
	g.Expect(string(*user.Spec.Resource.PasswordRef)).To(Equal(serviceAccountPasswordSecretName(cp, sa, 1)))
	g.Expect(user.Annotations[serviceAccountPasswordGenerationAnnotation]).To(Equal("1"))

	pw := &corev1.Secret{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: serviceAccountPasswordSecretName(cp, sa, 1), Namespace: ns}, pw)).To(Succeed())
	g.Expect(pw.Data[serviceAccountPasswordKey]).NotTo(BeEmpty())

	// The referenced project is an unmanaged import.
	proj := &orcv1alpha1.Project{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: serviceAccountProjectRef(cp, sa), Namespace: ns}, proj)).To(Succeed())
	g.Expect(proj.Spec.ManagementPolicy).To(Equal(orcv1alpha1.ManagementPolicyUnmanaged))
	g.Expect(proj.Spec.Import).NotTo(BeNil())

	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonWaitingForServiceAccounts))
}

func TestReconcileServiceAccounts_CollisionFailsLoudly(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := saControlPlane()
	sa := cp.Spec.KORC.ServiceAccounts[0]
	ns := childNamespace(cp)
	// Seed the user probe RESOLVED — a user of that name already exists.
	probe := &orcv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: serviceAccountUserProbeRef(cp, sa), Namespace: ns},
		Status:     orcv1alpha1.UserStatus{Conditions: availableImportConditions()},
	}

	cond, c := runServiceAccounts(t, cp, probe)

	// The managed User must NOT be created — the operator fails loudly.
	_, ok := getUserByName(t, c, serviceAccountUserRef(cp, sa), ns)
	g.Expect(ok).To(BeFalse())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonServiceAccountCollision))
	g.Expect(cond.Message).To(ContainSubstring("adopt=true"))
	g.Expect(cond.Message).To(ContainSubstring("nova"))
}

func TestReconcileServiceAccounts_AdoptSkipsProbe(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := saControlPlane()
	cp.Spec.KORC.ServiceAccounts[0].Adopt = true
	sa := cp.Spec.KORC.ServiceAccounts[0]
	ns := childNamespace(cp)

	_, c := runServiceAccounts(t, cp)

	// adopt=true skips the probe and creates the managed User directly.
	_, probeExists := getUserByName(t, c, serviceAccountUserProbeRef(cp, sa), ns)
	g.Expect(probeExists).To(BeFalse(), "no probe is created when adopt=true")
	user, ok := getUserByName(t, c, serviceAccountUserRef(cp, sa), ns)
	g.Expect(ok).To(BeTrue())
	g.Expect(user.Spec.ManagementPolicy).To(Equal(orcv1alpha1.ManagementPolicyManaged))
}

func TestReconcileServiceAccounts_RotationFlipsPasswordRef(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := saControlPlane()
	sa := cp.Spec.KORC.ServiceAccounts[0]
	ns := childNamespace(cp)

	// A managed User at generation 1 whose generation annotation was CLEARED by the
	// CredentialRotation reconciler — the rotation nudge.
	user := &orcv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{
			Name:        serviceAccountUserRef(cp, sa),
			Namespace:   ns,
			Annotations: map[string]string{serviceAccountPasswordGenerationAnnotation: ""},
		},
		Spec: orcv1alpha1.UserSpec{
			ManagementPolicy: orcv1alpha1.ManagementPolicyManaged,
			Resource: &orcv1alpha1.UserResourceSpec{
				PasswordRef: ptr.To(orcv1alpha1.KubernetesNameRef(serviceAccountPasswordSecretName(cp, sa, 1))),
			},
		},
		Status: orcv1alpha1.UserStatus{
			Conditions: availableImportConditions(),
			Resource:   &orcv1alpha1.UserResourceStatus{AppliedPasswordRef: serviceAccountPasswordSecretName(cp, sa, 1)},
		},
	}
	pwV1 := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: serviceAccountPasswordSecretName(cp, sa, 1), Namespace: ns, OwnerReferences: ownedByCP(cp)},
		Data:       map[string][]byte{serviceAccountPasswordKey: []byte("old-pw")},
	}

	_, c := runServiceAccounts(t, cp, user, pwV1)

	// The passwordRef flips to generation 2 and the annotation is restamped.
	updated, ok := getUserByName(t, c, serviceAccountUserRef(cp, sa), ns)
	g.Expect(ok).To(BeTrue())
	g.Expect(string(*updated.Spec.Resource.PasswordRef)).To(Equal(serviceAccountPasswordSecretName(cp, sa, 2)))
	g.Expect(updated.Annotations[serviceAccountPasswordGenerationAnnotation]).To(Equal("2"))

	pwV2 := &corev1.Secret{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: serviceAccountPasswordSecretName(cp, sa, 2), Namespace: ns}, pwV2)).To(Succeed())
	g.Expect(pwV2.Data[serviceAccountPasswordKey]).NotTo(BeEmpty())

	// The old generation is NOT deleted until K-ORC applies the new one.
	pwV1After := &corev1.Secret{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: serviceAccountPasswordSecretName(cp, sa, 1), Namespace: ns}, pwV1After)).To(Succeed())

	g.Expect(cp.Status.ServiceAccounts).To(HaveLen(1))
	g.Expect(cp.Status.ServiceAccounts[0].PasswordGeneration).To(Equal(int64(2)))
	g.Expect(cp.Status.ServiceAccounts[0].LastPasswordRotation).NotTo(BeNil())
}

func TestReconcileServiceAccounts_ManagedProjectProbeAbsentCreatesManagedProject(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := saControlPlane()
	cp.Spec.KORC.ServiceAccounts[0].Project = c5c3v1alpha1.ServiceAccountProjectSpec{Name: "service", Create: true}
	sa := cp.Spec.KORC.ServiceAccounts[0]
	ns := childNamespace(cp)
	// Both probes report absent so the managed Project and User are created.
	projectProbe := &orcv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: serviceAccountProjectProbeRef(cp, sa), Namespace: ns},
		Status:     orcv1alpha1.ProjectStatus{Conditions: pendingImportConditions(0)},
	}
	userProbe := &orcv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: serviceAccountUserProbeRef(cp, sa), Namespace: ns},
		Status:     orcv1alpha1.UserStatus{Conditions: pendingImportConditions(0)},
	}

	_, c := runServiceAccounts(t, cp, projectProbe, userProbe)

	proj := &orcv1alpha1.Project{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: serviceAccountProjectRef(cp, sa), Namespace: ns}, proj)).To(Succeed())
	g.Expect(proj.Spec.ManagementPolicy).To(Equal(orcv1alpha1.ManagementPolicyManaged))
	g.Expect(proj.Spec.Resource).NotTo(BeNil())
	g.Expect(string(*proj.Spec.Resource.Name)).To(Equal("service"))
}

func TestReconcileServiceAccounts_ManagedProjectCollisionFailsLoudly(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := saControlPlane()
	cp.Spec.KORC.ServiceAccounts[0].Project = c5c3v1alpha1.ServiceAccountProjectSpec{Name: "service", Create: true}
	sa := cp.Spec.KORC.ServiceAccounts[0]
	ns := childNamespace(cp)
	// The project probe RESOLVES — a project of that name already exists.
	projectProbe := &orcv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: serviceAccountProjectProbeRef(cp, sa), Namespace: ns},
		Status:     orcv1alpha1.ProjectStatus{Conditions: availableImportConditions()},
	}

	cond, c := runServiceAccounts(t, cp, projectProbe)

	// The managed Project must NOT be created.
	managed := &orcv1alpha1.Project{}
	err := c.Get(context.Background(), types.NamespacedName{Name: serviceAccountProjectRef(cp, sa), Namespace: ns}, managed)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
	g.Expect(cond.Reason).To(Equal(reasonServiceAccountCollision))
	g.Expect(cond.Message).To(ContainSubstring("project.create=false"))
}

func TestReconcileServiceAccounts_PrunesUndeclaredChild(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := korcControlPlane()
	setAdminCredentialReady(cp)
	// No declared accounts, but a leftover managed User the operator owns.
	ns := childNamespace(cp)
	leftover := &orcv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{
			Name:            serviceAccountChildPrefix(cp) + "user-old",
			Namespace:       ns,
			OwnerReferences: ownedByCP(cp),
		},
		Spec: orcv1alpha1.UserSpec{ManagementPolicy: orcv1alpha1.ManagementPolicyManaged},
	}

	cond, c := runServiceAccounts(t, cp, leftover)

	_, ok := getUserByName(t, c, serviceAccountChildPrefix(cp)+"user-old", ns)
	g.Expect(ok).To(BeFalse(), "the undeclared owned User must be pruned")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonWaitingForServiceAccounts))
}

func TestBuildServiceAccountCloudsYAML_UsesAccountIdentity(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := korcControlPlane()

	out := buildServiceAccountCloudsYAML(cp, "nova", "service", "Default", "s3cret")
	g.Expect(out).To(ContainSubstring("username: \"nova\""))
	g.Expect(out).To(ContainSubstring("password: \"s3cret\""))
	g.Expect(out).To(ContainSubstring("project_name: \"service\""))
	g.Expect(out).To(ContainSubstring("user_domain_name: \"Default\""))
	g.Expect(out).To(ContainSubstring("project_domain_name: \"Default\""))
}
