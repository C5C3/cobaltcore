// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the KeystoneUser provision leg: the reserved-name refusal, the
// store gate, the collision probe, the managed user and its password
// generations.
package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// kuTestClock is the instant the reconciler's clock reads in these tests, on a
// whole second the way a status time keeps it.
var kuTestClock = time.Date(2026, time.October, 10, 12, 0, 0, 0, time.UTC)

// kuTestNow is the reconciler clock that reads kuTestClock.
func kuTestNow() time.Time { return kuTestClock }

// runProvision drives provisionUser once for an order on cluster against the
// seeded management cluster and returns its outcome and the client. The one
// fake client doubles as the order's cluster client.
func runProvision(
	t *testing.T, order *c5c3v1alpha1.KeystoneUser, cp *c5c3v1alpha1.ControlPlane, cluster string,
	objs ...client.Object,
) (bool, ctrl.Result, client.Client) {
	t.Helper()
	g := NewGomegaWithT(t)
	c := kuFakeClient(t, nil, append([]client.Object{cp}, objs...)...)
	r := &KeystoneUserReconciler{Client: c, Scheme: c.Scheme(), Now: kuTestNow}
	ok, res, err := r.provisionUser(context.Background(), c, order, cp, cluster)
	g.Expect(err).NotTo(HaveOccurred())
	return ok, res, c
}

// kuOrderRotatedAgo returns the order at password generation 1 whose last
// rotation lies age before kuTestClock; it is due once age reaches the
// interval.
func kuOrderRotatedAgo(age time.Duration) *c5c3v1alpha1.KeystoneUser {
	order := keystoneUserCR(kuTestNamespace)
	order.Status.PasswordGeneration = 1
	order.Status.LastPasswordRotation = &metav1.Time{Time: kuTestClock.Add(-age)}
	return order
}

// kuDeletingCredential returns a KeystoneApplicationCredential beside the order
// that names the user and is being deleted; its finalizer keeps it listed.
func kuDeletingCredential() *c5c3v1alpha1.KeystoneApplicationCredential {
	credential := keystoneApplicationCredentialCR()
	credential.DeletionTimestamp = ptr.To(metav1.Now())
	return credential
}

// kuPasswordRef reads the password Secret the managed User points at.
func kuPasswordRef(t *testing.T, c client.Client, order *c5c3v1alpha1.KeystoneUser) string {
	t.Helper()
	user, found := kuGetUser(t, c, keystoneUserUserRef(order, ""))
	if !found {
		t.Fatalf("the managed User is missing")
	}
	return string(*user.Spec.Resource.PasswordRef)
}

// kuPasswordSecretExists reports whether generation gen's password Secret is in
// the ControlPlane's namespace.
func kuPasswordSecretExists(t *testing.T, c client.Client, order *c5c3v1alpha1.KeystoneUser, gen int64) bool {
	t.Helper()
	err := c.Get(context.Background(), types.NamespacedName{
		Namespace: "default", Name: keystoneUserPasswordSecretName(order, "", gen),
	}, &corev1.Secret{})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("reading the password Secret of generation %d: %v", gen, err)
	}
	return err == nil
}

// kuApply marks generation gen applied on the managed User, as K-ORC reports
// it.
func kuApply(t *testing.T, c client.Client, order *c5c3v1alpha1.KeystoneUser, gen int64) {
	t.Helper()
	user, _ := kuGetUser(t, c, keystoneUserUserRef(order, ""))
	user.Status.Resource.AppliedPasswordRef = keystoneUserPasswordSecretName(order, "", gen)
	if err := c.Status().Update(context.Background(), user); err != nil {
		t.Fatalf("marking generation %d applied: %v", gen, err)
	}
}

// kuFailCredentialList fails every List of KeystoneApplicationCredentials.
var kuFailCredentialList = &interceptor.Funcs{
	List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if _, ok := list.(*c5c3v1alpha1.KeystoneApplicationCredentialList); ok {
			return errors.New("boom")
		}
		return cl.List(ctx, list, opts...)
	},
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
	r := &KeystoneUserReconciler{Client: c, Scheme: c.Scheme(), Now: kuTestNow}

	ok, _, err := r.provisionUser(context.Background(), c, order, cp, "")

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
	g.Expect(res.RequeueAfter).To(Equal(defaultKeystoneUserRotationInterval), "the converged leg waits for its schedule")
	cond := kuCondition(order, conditionTypeKeystoneUserUserReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(reasonKeystoneUserProvisioned))
	g.Expect(order.Status.UserID).To(Equal("ku-user-id"))
	g.Expect(order.Status.PasswordGeneration).To(Equal(int64(1)))
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
	r := &KeystoneUserReconciler{Client: c, Scheme: c.Scheme(), Now: kuTestNow}
	v1 := types.NamespacedName{Namespace: "default", Name: keystoneUserPasswordSecretName(order, "", 1)}
	v2 := types.NamespacedName{Namespace: "default", Name: keystoneUserPasswordSecretName(order, "", 2)}

	ok, res, err := r.provisionUser(ctx, c, order, cp, "")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ok).To(BeFalse(), "the order waits until K-ORC applies v2")
	g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter))
	g.Expect(kuCondition(order, conditionTypeKeystoneUserUserReady).Reason).To(Equal(reasonWaitingForServiceAccounts))
	user, _ := kuGetUser(t, c, keystoneUserUserRef(order, ""))
	g.Expect(string(*user.Spec.Resource.PasswordRef)).To(Equal(v2.Name))
	g.Expect(c.Get(ctx, v2, &corev1.Secret{})).To(Succeed())
	g.Expect(c.Get(ctx, v1, &corev1.Secret{})).To(Succeed(), "v1 stays while Keystone still holds it")
	g.Expect(order.Status.LastPasswordRotation).NotTo(BeNil())
	g.Expect(order.Status.LastPasswordRotation.Time).To(BeTemporally("==", kuTestClock),
		"the re-point pass stamps the reconciler's clock")

	user.Status.Resource.AppliedPasswordRef = v2.Name
	g.Expect(c.Status().Update(ctx, user)).To(Succeed())

	ok, _, err = r.provisionUser(ctx, c, order, cp, "")
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

// --- the password schedule ---

func TestKeystoneUserRotationInterval(t *testing.T) {
	cases := []struct {
		name     string
		interval *metav1.Duration
		want     time.Duration
	}{
		{name: "an absent interval reads as the default", want: 720 * time.Hour},
		{name: "0s turns the schedule off", interval: &metav1.Duration{}, want: 0},
		{name: "a set interval is kept", interval: &metav1.Duration{Duration: 36 * time.Hour}, want: 36 * time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			order := keystoneUserCR(kuTestNamespace)
			order.Spec.Rotation.Interval = tc.interval
			g.Expect(keystoneUserRotationInterval(order)).To(Equal(tc.want))
		})
	}
}

func TestKeystoneUserNextPasswordRotation(t *testing.T) {
	g := NewGomegaWithT(t)

	order := keystoneUserCR(kuTestNamespace)
	g.Expect(keystoneUserNextPasswordRotation(order, time.Hour)).To(BeNil(), "nothing is scheduled before the first password")
	order.Status.LastPasswordRotation = &metav1.Time{Time: kuTestClock}
	g.Expect(keystoneUserNextPasswordRotation(order, time.Hour).Time).To(BeTemporally("==", kuTestClock.Add(time.Hour)))
	g.Expect(keystoneUserNextPasswordRotation(order, 0)).To(BeNil(), "an interval of 0s schedules nothing")
	g.Expect(keystoneUserNextPasswordRotation(order, -time.Hour)).To(BeNil(), "a negative interval schedules nothing")
}

// TestKeystoneUserProvision_FirstProvisionStampsTheAnchor covers the pass that
// first sees the password applied: it anchors the schedule at the clock and
// wakes the order one interval later.
func TestKeystoneUserProvision_FirstProvisionStampsTheAnchor(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	order := keystoneUserCR(kuTestNamespace)

	ok, res, c := runProvision(t, order, cp, "", kuConvergedChildren(order, cp, "", 1)...)

	g.Expect(ok).To(BeTrue())
	g.Expect(order.Status.LastPasswordRotation.Time).To(BeTemporally("==", kuTestClock))
	g.Expect(order.Status.NextPasswordRotation.Time).To(BeTemporally("==", kuTestClock.Add(defaultKeystoneUserRotationInterval)))
	g.Expect(res.RequeueAfter).To(Equal(defaultKeystoneUserRotationInterval))
	g.Expect(kuPasswordRef(t, c, order)).To(Equal(keystoneUserPasswordSecretName(order, "", 1)), "the anchor rotates nothing")
}

// TestKeystoneUserProvision_DueScheduleRotates walks a scheduled rotation from
// 1 to 2: the due pass re-points the User and stamps the clock, and the pass
// that sees v2 applied reports generation 2, stamps the anchor again and drops
// v1.
func TestKeystoneUserProvision_DueScheduleRotates(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	cp := ksControlPlane()
	order := kuOrderRotatedAgo(defaultKeystoneUserRotationInterval + time.Hour)
	c := kuFakeClient(t, nil, append([]client.Object{cp}, kuConvergedChildren(order, cp, "", 1)...)...)
	clock := kuTestClock
	r := &KeystoneUserReconciler{Client: c, Scheme: c.Scheme(), Now: func() time.Time { return clock }}

	ok, res, err := r.provisionUser(ctx, c, order, cp, "")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ok).To(BeFalse(), "the order waits until K-ORC applies v2")
	g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter))
	g.Expect(kuCondition(order, conditionTypeKeystoneUserUserReady).Reason).To(Equal(reasonWaitingForServiceAccounts))
	g.Expect(kuPasswordRef(t, c, order)).To(Equal(keystoneUserPasswordSecretName(order, "", 2)))
	g.Expect(kuPasswordSecretExists(t, c, order, 2)).To(BeTrue())
	g.Expect(kuPasswordSecretExists(t, c, order, 1)).To(BeTrue(), "v1 stays while Keystone still holds it")
	g.Expect(order.Status.PasswordGeneration).To(Equal(int64(1)))
	g.Expect(order.Status.LastPasswordRotation.Time).To(BeTemporally("==", kuTestClock))
	g.Expect(order.Status.NextPasswordRotation.Time).To(BeTemporally("==", kuTestClock.Add(defaultKeystoneUserRotationInterval)))

	kuApply(t, c, order, 2)
	clock = kuTestClock.Add(time.Minute)

	ok, res, err = r.provisionUser(ctx, c, order, cp, "")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ok).To(BeTrue())
	g.Expect(order.Status.PasswordGeneration).To(Equal(int64(2)))
	g.Expect(order.Status.LastPasswordRotation.Time).To(BeTemporally("==", clock),
		"the pass that sees v2 applied stamps the anchor again")
	g.Expect(order.Status.NextPasswordRotation.Time).To(BeTemporally("==", clock.Add(defaultKeystoneUserRotationInterval)))
	g.Expect(res.RequeueAfter).To(Equal(defaultKeystoneUserRotationInterval))
	g.Expect(kuPasswordSecretExists(t, c, order, 1)).To(BeFalse(), "the superseded generation is deleted once v2 is applied")
	g.Expect(kuPasswordSecretExists(t, c, order, 3)).To(BeFalse(), "the applied generation is not rotated again")
}

func TestKeystoneUserProvision_ScheduleOffSchedulesNothing(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	order := kuOrderRotatedAgo(800 * time.Hour)
	order.Spec.Rotation.Interval = &metav1.Duration{}
	order.Status.NextPasswordRotation = &metav1.Time{Time: kuTestClock.Add(-80 * time.Hour)}

	ok, res, c := runProvision(t, order, cp, "", kuConvergedChildren(order, cp, "", 1)...)

	g.Expect(ok).To(BeTrue())
	g.Expect(kuPasswordRef(t, c, order)).To(Equal(keystoneUserPasswordSecretName(order, "", 1)))
	g.Expect(kuPasswordSecretExists(t, c, order, 2)).To(BeFalse())
	g.Expect(order.Status.NextPasswordRotation).To(BeNil(), "0s clears the next rotation")
	g.Expect(res.IsZero()).To(BeTrue())
}

// TestKeystoneUserProvision_UpgradedOrderKeepsItsRecordedRotation pins the
// upgrade anchor: an order rotated by hand longer than an interval ago rotates
// at once, one without a record is only anchored.
func TestKeystoneUserProvision_UpgradedOrderKeepsItsRecordedRotation(t *testing.T) {
	t.Run("a recorded rotation older than the interval rotates at once", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := ksControlPlane()
		order := kuOrderRotatedAgo(800 * time.Hour)

		ok, _, c := runProvision(t, order, cp, "", kuConvergedChildren(order, cp, "", 1)...)

		g.Expect(ok).To(BeFalse(), "the order waits until K-ORC applies v2")
		g.Expect(kuPasswordRef(t, c, order)).To(Equal(keystoneUserPasswordSecretName(order, "", 2)))
	})
	t.Run("no record only stamps the anchor", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := ksControlPlane()
		order := keystoneUserCR(kuTestNamespace)
		order.Status.PasswordGeneration = 1

		ok, res, c := runProvision(t, order, cp, "", kuConvergedChildren(order, cp, "", 1)...)

		g.Expect(ok).To(BeTrue())
		g.Expect(kuPasswordRef(t, c, order)).To(Equal(keystoneUserPasswordSecretName(order, "", 1)))
		g.Expect(order.Status.LastPasswordRotation.Time).To(BeTemporally("==", kuTestClock))
		g.Expect(res.RequeueAfter).To(Equal(defaultKeystoneUserRotationInterval))
	})
}

// TestKeystoneUserProvision_ScheduleWaitsForACredentialTeardown pins the hold:
// a due schedule waits while a credential order beside the order that names
// the user is being deleted, and only such a credential order holds it.
func TestKeystoneUserProvision_ScheduleWaitsForACredentialTeardown(t *testing.T) {
	t.Run("a deleting credential naming the user holds", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := ksControlPlane()
		order := kuOrderRotatedAgo(defaultKeystoneUserRotationInterval + time.Hour)

		ok, res, c := runProvision(t, order, cp, "",
			append(kuConvergedChildren(order, cp, "", 1), kuDeletingCredential())...)

		g.Expect(ok).To(BeTrue(), "the live password stays valid")
		g.Expect(kuPasswordRef(t, c, order)).To(Equal(keystoneUserPasswordSecretName(order, "", 1)))
		g.Expect(kuPasswordSecretExists(t, c, order, 2)).To(BeFalse())
		cond := kuCondition(order, conditionTypeKeystoneUserUserReady)
		g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(cond.Reason).To(Equal(reasonKeystoneUserProvisioned))
		g.Expect(cond.Message).To(ContainSubstring(`; the scheduled password rotation waits until ` +
			`KeystoneApplicationCredential(s) ["workflow-appcred"] in namespace "tenant-a" are deleted`))
		g.Expect(res.RequeueAfter).To(Equal(orderReferenceHoldRequeueAfter))
		g.Expect(order.Status.NextPasswordRotation.Time).To(BeTemporally("==", kuTestClock.Add(-time.Hour)),
			"the held order keeps its due anchor")
	})

	cases := []struct {
		name   string
		mutate func(*c5c3v1alpha1.KeystoneApplicationCredential)
	}{
		{
			name:   "a credential naming the user without a deletion timestamp",
			mutate: func(ac *c5c3v1alpha1.KeystoneApplicationCredential) { ac.DeletionTimestamp = nil },
		},
		{
			name:   "a deleting credential naming another user",
			mutate: func(ac *c5c3v1alpha1.KeystoneApplicationCredential) { ac.Spec.UserRef.Name = "someone" },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name+" does not hold", func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := ksControlPlane()
			order := kuOrderRotatedAgo(defaultKeystoneUserRotationInterval + time.Hour)
			credential := kuDeletingCredential()
			tc.mutate(credential)

			ok, res, c := runProvision(t, order, cp, "", append(kuConvergedChildren(order, cp, "", 1), credential)...)

			g.Expect(ok).To(BeFalse(), "the order waits until K-ORC applies v2")
			g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter))
			g.Expect(kuPasswordRef(t, c, order)).To(Equal(keystoneUserPasswordSecretName(order, "", 2)))
		})
	}
}

// TestKeystoneUser_TargetClusterScheduleReadsTheOrderCluster pins that a due
// schedule of an order on a target cluster reads the credential orders on that
// cluster: a deleting one there holds the rotation, and a cluster that does not
// serve the kind rotates.
func TestKeystoneUser_TargetClusterScheduleReadsTheOrderCluster(t *testing.T) {
	// The order and the credential land on the order's cluster, everything in
	// the ControlPlane's namespace on the management cluster.
	reconcileDue := func(t *testing.T, orderFuncs *interceptor.Funcs, extra ...client.Object) (
		*c5c3v1alpha1.KeystoneUser, *kuHarness, ctrl.Result,
	) {
		t.Helper()
		cp := keystoneUserControlPlane(assignOn(kuTestNamespace, kuTestCluster))
		cp.Spec.Services.Keystone.PublicEndpoint = "https://keystone.example.test/v3"
		order := kuOrderRotatedAgo(defaultKeystoneUserRotationInterval + time.Hour)
		objs := append([]client.Object{order, cp}, kuConvergedChildren(order, cp, kuTestCluster, 1)...)
		h := newKUHarness(t, kuTestCluster, nil, orderFuncs, append(objs, extra...)...)
		result, err := h.reconcile(context.Background())
		if err != nil {
			t.Fatalf("reconciling the order: %v", err)
		}
		return order, h, result
	}
	passwordRef := func(t *testing.T, h *kuHarness, order *c5c3v1alpha1.KeystoneUser) string {
		t.Helper()
		user, found := kuGetUser(t, h.mgmt, keystoneUserUserRef(order, kuTestCluster))
		if !found {
			t.Fatalf("the managed User is missing")
		}
		return string(*user.Spec.Resource.PasswordRef)
	}

	t.Run("a deleting credential on the order's cluster holds", func(t *testing.T) {
		g := NewGomegaWithT(t)

		order, h, result := reconcileDue(t, nil, kuDeletingCredential())

		g.Expect(passwordRef(t, h, order)).To(Equal(keystoneUserPasswordSecretName(order, kuTestCluster, 1)))
		g.Expect(kuCondition(h.get(t), conditionTypeKeystoneUserUserReady).Message).To(
			ContainSubstring("waits until KeystoneApplicationCredential(s)"))
		g.Expect(result.RequeueAfter).To(Equal(orderReferenceHoldRequeueAfter))
	})

	t.Run("a cluster without the credential kind rotates", func(t *testing.T) {
		g := NewGomegaWithT(t)

		order, h, _ := reconcileDue(t, &interceptor.Funcs{
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*c5c3v1alpha1.KeystoneApplicationCredentialList); ok {
					return &meta.NoKindMatchError{GroupKind: keystoneApplicationCredentialGVK.GroupKind()}
				}
				return cl.List(ctx, list, opts...)
			},
		})

		g.Expect(passwordRef(t, h, order)).To(Equal(keystoneUserPasswordSecretName(order, kuTestCluster, 2)))
	})
}

func TestKeystoneUserProvision_RaisedGenerationIsNotHeld(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	order := kuOrderRotatedAgo(defaultKeystoneUserRotationInterval + time.Hour)
	order.Spec.PasswordGeneration = 2

	ok, _, c := runProvision(t, order, cp, "", append(kuConvergedChildren(order, cp, "", 1), kuDeletingCredential())...)

	g.Expect(ok).To(BeFalse(), "the order waits until K-ORC applies v2")
	g.Expect(kuPasswordRef(t, c, order)).To(Equal(keystoneUserPasswordSecretName(order, "", 2)),
		"a raised generation goes through a credential teardown")
}

// TestKeystoneUserProvision_LostStatusWriteDoesNotRotateTwice covers a re-point
// pass whose status write was lost: the User points at v2, the status still
// names generation 1 and its due anchor. No v3 follows, and the anchor moves
// once v2 is applied.
func TestKeystoneUserProvision_LostStatusWriteDoesNotRotateTwice(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	cp := ksControlPlane()
	order := kuOrderRotatedAgo(defaultKeystoneUserRotationInterval + time.Hour)
	seeded := kuConvergedChildren(order, cp, "", 1)
	user := seeded[0].(*orcv1alpha1.User)
	user.Spec.Resource.PasswordRef = ptr.To(orcv1alpha1.KubernetesNameRef(keystoneUserPasswordSecretName(order, "", 2)))
	v2 := seeded[1].DeepCopyObject().(*corev1.Secret)
	v2.Name = keystoneUserPasswordSecretName(order, "", 2)
	c := kuFakeClient(t, nil, append([]client.Object{cp, v2}, seeded...)...)
	r := &KeystoneUserReconciler{Client: c, Scheme: c.Scheme(), Now: kuTestNow}

	ok, _, err := r.provisionUser(ctx, c, order, cp, "")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ok).To(BeFalse(), "the order waits until K-ORC applies v2")
	g.Expect(kuPasswordRef(t, c, order)).To(Equal(keystoneUserPasswordSecretName(order, "", 2)))
	g.Expect(kuPasswordSecretExists(t, c, order, 3)).To(BeFalse(), "the due anchor starts no second rotation")

	kuApply(t, c, order, 2)

	ok, res, err := r.provisionUser(ctx, c, order, cp, "")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ok).To(BeTrue())
	g.Expect(order.Status.PasswordGeneration).To(Equal(int64(2)))
	g.Expect(order.Status.LastPasswordRotation.Time).To(BeTemporally("==", kuTestClock), "the anchor is stamped again")
	g.Expect(res.RequeueAfter).To(Equal(defaultKeystoneUserRotationInterval))
	g.Expect(kuPasswordSecretExists(t, c, order, 3)).To(BeFalse())
}

// TestKeystoneUserProvision_AppliedRotationEndsTheHold covers a re-point whose
// status write was lost while a credential order is being deleted: the pass
// that sees v2 applied anchors the schedule again, so nothing waits any more.
func TestKeystoneUserProvision_AppliedRotationEndsTheHold(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	cp := ksControlPlane()
	order := kuOrderRotatedAgo(defaultKeystoneUserRotationInterval + time.Hour)
	seeded := kuConvergedChildren(order, cp, "", 1)
	user := seeded[0].(*orcv1alpha1.User)
	user.Spec.Resource.PasswordRef = ptr.To(orcv1alpha1.KubernetesNameRef(keystoneUserPasswordSecretName(order, "", 2)))
	user.Status.Resource.AppliedPasswordRef = keystoneUserPasswordSecretName(order, "", 2)
	v2 := seeded[1].DeepCopyObject().(*corev1.Secret)
	v2.Name = keystoneUserPasswordSecretName(order, "", 2)
	c := kuFakeClient(t, nil, append([]client.Object{cp, v2, kuDeletingCredential()}, seeded...)...)
	r := &KeystoneUserReconciler{Client: c, Scheme: c.Scheme(), Now: kuTestNow}

	ok, res, err := r.provisionUser(ctx, c, order, cp, "")

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ok).To(BeTrue())
	g.Expect(order.Status.PasswordGeneration).To(Equal(int64(2)))
	g.Expect(kuCondition(order, conditionTypeKeystoneUserUserReady).Message).NotTo(ContainSubstring("waits"))
	g.Expect(res.RequeueAfter).To(Equal(defaultKeystoneUserRotationInterval))
	g.Expect(kuPasswordSecretExists(t, c, order, 3)).To(BeFalse())
}

func TestKeystoneUserProvision_CredentialListErrorIsReported(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	order := kuOrderRotatedAgo(defaultKeystoneUserRotationInterval + time.Hour)
	c := kuFakeClient(t, kuFailCredentialList, append([]client.Object{cp}, kuConvergedChildren(order, cp, "", 1)...)...)
	r := &KeystoneUserReconciler{Client: c, Scheme: c.Scheme(), Now: kuTestNow}

	ok, _, err := r.provisionUser(context.Background(), c, order, cp, "")

	g.Expect(err).To(MatchError(ContainSubstring("listing KeystoneApplicationCredentials")))
	g.Expect(err).To(MatchError(ContainSubstring("boom")))
	g.Expect(ok).To(BeFalse())
	cond := kuCondition(order, conditionTypeKeystoneUserUserReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonServiceAccountError))
	g.Expect(cond.Message).To(Equal(err.Error()))
	g.Expect(kuPasswordRef(t, c, order)).To(Equal(keystoneUserPasswordSecretName(order, "", 1)))
}

// TestKeystoneUserProvision_NotDueScheduleReadsNoCredentialOrders pins that
// only a due pass reads the credential orders: with their List failing, a
// pass whose schedule is not due still converges.
func TestKeystoneUserProvision_NotDueScheduleReadsNoCredentialOrders(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	order := kuOrderRotatedAgo(time.Hour)
	c := kuFakeClient(t, kuFailCredentialList, append([]client.Object{cp}, kuConvergedChildren(order, cp, "", 1)...)...)
	r := &KeystoneUserReconciler{Client: c, Scheme: c.Scheme(), Now: kuTestNow}

	ok, res, err := r.provisionUser(context.Background(), c, order, cp, "")

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ok).To(BeTrue())
	g.Expect(kuCondition(order, conditionTypeKeystoneUserUserReady).Reason).To(Equal(reasonKeystoneUserProvisioned))
	g.Expect(res.RequeueAfter).To(Equal(defaultKeystoneUserRotationInterval - time.Hour))
}
