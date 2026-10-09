// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the KeystoneUser provision leg: the reserved-name refusal, the
// store gate, the collision probe, the managed user and its password
// generations.
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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// runProvision drives provisionUser once for an order on cluster against the
// seeded management cluster and returns its outcome and the client.
func runProvision(
	t *testing.T, order *c5c3v1alpha1.KeystoneUser, cp *c5c3v1alpha1.ControlPlane, cluster string,
	objs ...client.Object,
) (bool, ctrl.Result, client.Client) {
	t.Helper()
	g := NewGomegaWithT(t)
	c := kuFakeClient(t, nil, append([]client.Object{cp}, objs...)...)
	r := &KeystoneUserReconciler{Client: c, Scheme: c.Scheme()}
	ok, res, err := r.provisionUser(context.Background(), order, cp, cluster)
	g.Expect(err).NotTo(HaveOccurred())
	return ok, res, c
}

// absentUserProbe seeds the collision probe as K-ORC reports a user that does
// not exist, so the gate lets the managed User be created.
func absentUserProbe(order *c5c3v1alpha1.KeystoneUser, cp *c5c3v1alpha1.ControlPlane, cluster string) *orcv1alpha1.User {
	return &orcv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{
			Name: keystoneUserUserProbeRef(order, cluster), Namespace: cp.Namespace,
			Labels: keystoneUserChildLabels(order, cluster),
		},
		Status: orcv1alpha1.UserStatus{Conditions: pendingImportConditions(0)},
	}
}

func kuGetUser(t *testing.T, c client.Client, name string) (*orcv1alpha1.User, bool) {
	t.Helper()
	return ksGetUser(t, c, name, "default")
}

func TestKeystoneUserProvision_ProjectsAnUnscopedManagedUser(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	order := keystoneUserCR(kuTestNamespace)

	ok, res, c := runProvision(t, order, cp, "", readyTenantStoreFor(cp), absentUserProbe(order, cp, ""))

	g.Expect(ok).To(BeFalse(), "a fresh user is not Available yet")
	g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter))
	user, found := kuGetUser(t, c, keystoneUserUserRef(order, ""))
	g.Expect(found).To(BeTrue())
	g.Expect(user.Name).To(MatchRegexp(`^workflow-[0-9a-f]{8}-user-user$`))
	g.Expect(string(*user.Spec.Resource.Name)).To(Equal(kuTestName), "an empty userName means metadata.name")
	g.Expect(string(*user.Spec.Resource.DomainRef)).To(Equal("cp-domain-default"))
	g.Expect(user.Spec.Resource.DefaultProjectRef).To(BeNil(), "the ordered user has no project")
	g.Expect(string(*user.Spec.Resource.PasswordRef)).To(Equal(keystoneUserPasswordSecretName(order, "", 1)))
	g.Expect(user.Labels).To(Equal(map[string]string{
		keystoneUserNameLabel:      kuTestName,
		keystoneUserNamespaceLabel: kuTestNamespace,
		keystoneUserClusterLabel:   "",
	}))
	g.Expect(user.OwnerReferences).To(BeEmpty(), "a child in another namespace carries labels only")

	pw := &corev1.Secret{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{
		Namespace: "default", Name: keystoneUserPasswordSecretName(order, "", 1),
	}, pw)).To(Succeed())
	g.Expect(pw.Data[serviceAccountPasswordKey]).NotTo(BeEmpty())
	g.Expect(pw.Labels).To(HaveKeyWithValue(keystoneUserNameLabel, kuTestName))

	_, probeLeft := kuGetUser(t, c, keystoneUserUserProbeRef(order, ""))
	g.Expect(probeLeft).To(BeFalse(), "the resolved probe is dropped")
}

func TestKeystoneUserProvision_TargetClusterOrderIsKeyedByItsCluster(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	order := keystoneUserCR(kuTestNamespace)

	_, _, c := runProvision(t, order, cp, kuTestCluster, readyTenantStoreFor(cp), absentUserProbe(order, cp, kuTestCluster))

	user, found := kuGetUser(t, c, keystoneUserUserRef(order, kuTestCluster))
	g.Expect(found).To(BeTrue())
	g.Expect(user.Labels).To(HaveKeyWithValue(keystoneUserClusterLabel, kuTestCluster))
	g.Expect(keystoneUserUserRef(order, kuTestCluster)).NotTo(Equal(keystoneUserUserRef(order, "")))
	_, localFound := kuGetUser(t, c, keystoneUserUserRef(order, ""))
	g.Expect(localFound).To(BeFalse())
}

// TestKeystoneUserProvision_AdminIdentityIsRefused folds the case the way
// Keystone does, and refuses before any probe or user exists.
func TestKeystoneUserProvision_AdminIdentityIsRefused(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	order := keystoneUserCR(kuTestNamespace)
	order.Spec.UserName = "ADMIN"

	ok, res, c := runProvision(t, order, cp, "", readyTenantStoreFor(cp))

	g.Expect(ok).To(BeFalse())
	g.Expect(res.IsZero()).To(BeTrue(), "only a ControlPlane edit clears the refusal")
	cond := kuCondition(order, conditionTypeKeystoneUserUserReady)
	g.Expect(cond.Reason).To(Equal(reasonServiceAccountCollision))
	g.Expect(cond.Message).To(ContainSubstring("admin identity"))
	var users orcv1alpha1.UserList
	g.Expect(c.List(context.Background(), &users)).To(Succeed())
	g.Expect(users.Items).To(BeEmpty(), "neither a User nor a probe is projected")
}

// TestKeystoneUserProvision_BuiltinServiceAccountsAreRefused covers the names
// the ControlPlane's built-in registrations create in the admin domain. They are
// refused whether or not the service is enabled, so an order cannot take one
// before its service does.
func TestKeystoneUserProvision_BuiltinServiceAccountsAreRefused(t *testing.T) {
	for _, name := range []string{"Nova", "cinder", "neutron-nova", "hypervisor-operator"} {
		t.Run(name, func(t *testing.T) {
			g := NewGomegaWithT(t)

			cp := ksControlPlane()
			order := keystoneUserCR(kuTestNamespace)
			order.Spec.UserName = name

			ok, res, c := runProvision(t, order, cp, "", readyTenantStoreFor(cp))

			g.Expect(ok).To(BeFalse())
			g.Expect(res.IsZero()).To(BeTrue())
			cond := kuCondition(order, conditionTypeKeystoneUserUserReady)
			g.Expect(cond.Reason).To(Equal(reasonServiceAccountCollision))
			g.Expect(cond.Message).To(ContainSubstring("built-in service account"))
			var users orcv1alpha1.UserList
			g.Expect(c.List(context.Background(), &users)).To(Succeed())
			g.Expect(users.Items).To(BeEmpty(), "neither a User nor a probe is projected")
		})
	}
}

// TestKeystoneUserProvision_ConvergedPassDeletesNothing covers the owned path
// once the probe is gone: a steady-state pass sends no delete for it.
func TestKeystoneUserProvision_ConvergedPassDeletesNothing(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	order := keystoneUserCR(kuTestNamespace)
	var deleted []string
	c := kuFakeClient(t, &interceptor.Funcs{
		Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			deleted = append(deleted, obj.GetName())
			return cl.Delete(ctx, obj, opts...)
		},
	}, append([]client.Object{cp}, kuConvergedChildren(order, cp, "", 1)...)...)
	r := &KeystoneUserReconciler{Client: c, Scheme: c.Scheme()}

	ok, _, err := r.provisionUser(context.Background(), order, cp, "")

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ok).To(BeTrue())
	g.Expect(deleted).To(BeEmpty())
}

func TestKeystoneUserProvision_WaitsForTheStore(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	order := keystoneUserCR(kuTestNamespace)

	ok, res, _ := runProvision(t, order, cp, "")

	g.Expect(ok).To(BeFalse())
	g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter))
	cond := kuCondition(order, conditionTypeKeystoneUserUserReady)
	g.Expect(cond.Reason).To(Equal(reasonServiceAccountStoreNotReady))
	g.Expect(cond.Message).To(ContainSubstring(esoTenantStoreName))
	g.Expect(cond.Message).To(ContainSubstring(`namespace "default"`))
}

func TestKeystoneUserProvision_CollisionGate(t *testing.T) {
	cases := []struct {
		name       string
		conditions []metav1.Condition
		reason     string
		message    string
	}{
		{name: "an existing user", conditions: availableImportConditions(), reason: reasonServiceAccountCollision, message: "never takes over"},
		{name: "an unresolved probe", reason: reasonProbingForCollision, message: "probing whether Keystone user"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := ksControlPlane()
			order := keystoneUserCR(kuTestNamespace)
			probe := absentUserProbe(order, cp, "")
			probe.Status.Conditions = tc.conditions

			ok, res, c := runProvision(t, order, cp, "", readyTenantStoreFor(cp), probe)

			g.Expect(ok).To(BeFalse())
			g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter))
			cond := kuCondition(order, conditionTypeKeystoneUserUserReady)
			g.Expect(cond.Reason).To(Equal(tc.reason))
			g.Expect(cond.Message).To(ContainSubstring(tc.message))
			_, found := kuGetUser(t, c, keystoneUserUserRef(order, ""))
			g.Expect(found).To(BeFalse(), "no managed User is created behind the gate")
		})
	}
}

func TestKeystoneUserProvision_ConvergedUserIsProvisioned(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	order := keystoneUserCR(kuTestNamespace)

	ok, res, _ := runProvision(t, order, cp, "", kuConvergedChildren(order, cp, "", 1)...)

	g.Expect(ok).To(BeTrue())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := kuCondition(order, conditionTypeKeystoneUserUserReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(reasonKeystoneUserProvisioned))
	g.Expect(order.Status.UserID).To(Equal("ku-user-id"))
	g.Expect(order.Status.PasswordGeneration).To(Equal(int64(1)))
	g.Expect(order.Status.LastPasswordRotation).To(BeNil(), "nothing was rotated")
}

// TestKeystoneUserProvision_RaisingTheGenerationRotates walks a rotation from 1
// to 2 over two passes: the first re-points the User at a new password Secret
// and keeps v1, because Keystone still holds it; the second, once K-ORC reports
// v2 applied, drops v1 and reports generation 2.
func TestKeystoneUserProvision_RaisingTheGenerationRotates(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	cp := ksControlPlane()
	order := keystoneUserCR(kuTestNamespace)
	order.Spec.PasswordGeneration = 2
	c := kuFakeClient(t, nil, append([]client.Object{cp}, kuConvergedChildren(order, cp, "", 1)...)...)
	r := &KeystoneUserReconciler{Client: c, Scheme: c.Scheme()}
	v1 := types.NamespacedName{Namespace: "default", Name: keystoneUserPasswordSecretName(order, "", 1)}
	v2 := types.NamespacedName{Namespace: "default", Name: keystoneUserPasswordSecretName(order, "", 2)}

	ok, res, err := r.provisionUser(ctx, order, cp, "")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ok).To(BeFalse(), "the order waits until K-ORC applies v2")
	g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter))
	g.Expect(kuCondition(order, conditionTypeKeystoneUserUserReady).Reason).To(Equal(reasonWaitingForServiceAccounts))
	user, _ := kuGetUser(t, c, keystoneUserUserRef(order, ""))
	g.Expect(string(*user.Spec.Resource.PasswordRef)).To(Equal(v2.Name))
	g.Expect(c.Get(ctx, v2, &corev1.Secret{})).To(Succeed())
	g.Expect(c.Get(ctx, v1, &corev1.Secret{})).To(Succeed(), "v1 stays while Keystone still holds it")
	g.Expect(order.Status.LastPasswordRotation).NotTo(BeNil())

	user.Status.Resource.AppliedPasswordRef = v2.Name
	g.Expect(c.Status().Update(ctx, user)).To(Succeed())

	ok, _, err = r.provisionUser(ctx, order, cp, "")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ok).To(BeTrue())
	g.Expect(order.Status.PasswordGeneration).To(Equal(int64(2)))
	g.Expect(apierrors.IsNotFound(c.Get(ctx, v1, &corev1.Secret{}))).To(BeTrue(),
		"the superseded generation is deleted once v2 is applied")
}

func TestKeystoneUserProvision_UnsetGenerationReadsAsOne(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	order := keystoneUserCR(kuTestNamespace)
	order.Spec.PasswordGeneration = 0

	ok, _, c := runProvision(t, order, cp, "", kuConvergedChildren(order, cp, "", 1)...)

	g.Expect(ok).To(BeTrue())
	g.Expect(order.Status.PasswordGeneration).To(Equal(int64(1)))
	user, _ := kuGetUser(t, c, keystoneUserUserRef(order, ""))
	g.Expect(string(*user.Spec.Resource.PasswordRef)).To(Equal(keystoneUserPasswordSecretName(order, "", 1)),
		"a stored zero rotates nothing")
}

func TestKeystoneUserProvision_UserNotReady(t *testing.T) {
	cases := []struct {
		name       string
		conditions []metav1.Condition
		reason     string
	}{
		{name: "terminal K-ORC error", conditions: terminalImportConditions("invalid domain reference"), reason: reasonServiceAccountsFailed},
		{name: "not yet Available", conditions: pendingImportConditions(0), reason: reasonWaitingForServiceAccounts},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := ksControlPlane()
			order := keystoneUserCR(kuTestNamespace)
			seeded := kuConvergedChildren(order, cp, "", 1)
			seeded[0].(*orcv1alpha1.User).Status.Conditions = tc.conditions

			ok, res, _ := runProvision(t, order, cp, "", seeded...)

			g.Expect(ok).To(BeFalse())
			g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter))
			g.Expect(kuCondition(order, conditionTypeKeystoneUserUserReady).Reason).To(Equal(tc.reason))
			g.Expect(order.Status.PasswordGeneration).To(BeZero(), "no generation is reported before it is applied")
		})
	}
}
