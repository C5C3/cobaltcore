// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the KeystoneApplicationCredential reconciler's lifecycle: the
// consent gate and the freeze, the reference gates, the naming contract, the
// watch mappers and predicates, and the two-phase teardown.
package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	mchandler "sigs.k8s.io/multicluster-runtime/pkg/handler"
	mcruntime "sigs.k8s.io/multicluster-runtime/pkg/multicluster"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

const kacTestName = "workflow-appcred"

// kacTestClock is the instant the harness clock starts at, on a whole second
// the way a status time keeps it.
var kacTestClock = time.Date(2026, time.October, 10, 12, 0, 0, 0, time.UTC)

// keystoneApplicationCredentialCR returns the order "workflow-appcred" in
// kuTestNamespace for the user "workflow" on the project "workflow-project",
// with the schema's defaults spelled out, already carrying the teardown
// finalizer.
func keystoneApplicationCredentialCR() *c5c3v1alpha1.KeystoneApplicationCredential {
	return &c5c3v1alpha1.KeystoneApplicationCredential{
		ObjectMeta: metav1.ObjectMeta{
			Name:       kacTestName,
			Namespace:  kuTestNamespace,
			Generation: 1,
			UID:        types.UID("kac-uid"),
			Finalizers: []string{keystoneApplicationCredentialFinalizerName},
		},
		Spec: c5c3v1alpha1.KeystoneApplicationCredentialSpec{
			ControlPlaneRef:      c5c3v1alpha1.ControlPlaneRefSpec{Name: "cp", Namespace: "default"},
			UserRef:              c5c3v1alpha1.KeystoneOrderRef{Name: kuTestName},
			ProjectRef:           c5c3v1alpha1.KeystoneOrderRef{Name: kpTestName},
			CredentialGeneration: 1,
			Rotation: c5c3v1alpha1.KeystoneApplicationCredentialRotationSpec{
				Interval:    &metav1.Duration{Duration: 720 * time.Hour},
				GracePeriod: &metav1.Duration{Duration: 24 * time.Hour},
			},
		},
	}
}

// kacReadyReferences returns what the order references once it may mint on
// cluster: the provisioned user at password generation 1 with that password's
// Secret in the ControlPlane's namespace, the provisioned project, and the
// assignment of a role to the user on the project.
func kacReadyReferences(cluster string) []client.Object {
	user, project := kraReadyReferences()
	user.Status.PasswordGeneration = 1
	project.Status.ProjectName = kpTestName
	assignment := keystoneRoleAssignmentCR()
	assignment.Status.RoleID = "r-1"
	password := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: keystoneUserPasswordSecretName(user, cluster, 1), Namespace: "default",
			Labels: keystoneUserChildLabels(user, cluster),
		},
		Data: map[string][]byte{serviceAccountPasswordKey: []byte("user-password-1")},
	}
	return []client.Object{user, project, assignment, password}
}

// kacControlPlane assigns kuTestNamespace on cluster and publishes Keystone,
// so an order on a target cluster can be delivered as well.
func kacControlPlane(cluster string) *c5c3v1alpha1.ControlPlane {
	cp := keystoneUserControlPlane(assignOn(kuTestNamespace, cluster))
	cp.Spec.Services.Keystone.PublicEndpoint = "https://keystone.example.test/v3"
	return cp
}

// kacHarness drives the reconciler with a clock the test moves.
type kacHarness struct {
	*orderHarness
	now time.Time
}

func newKACHarness(t *testing.T, cluster string, mgmtFuncs, orderFuncs *interceptor.Funcs, objs ...client.Object) *kacHarness {
	t.Helper()
	h := &kacHarness{now: kacTestClock}
	h.orderHarness = newOrderHarness(t, cluster, types.NamespacedName{Namespace: kuTestNamespace, Name: kacTestName},
		mgmtFuncs, orderFuncs, objs, func(mgmt client.Client, resolver commonmulticluster.ClusterResolver) orderReconciler {
			return &KeystoneApplicationCredentialReconciler{
				Client: mgmt, Scheme: mgmt.Scheme(), Resolver: resolver,
				Now: func() time.Time { return h.now },
			}
		})
	return h
}

func kacGet(t *testing.T, h *kacHarness) *c5c3v1alpha1.KeystoneApplicationCredential {
	t.Helper()
	return reloadOrder[c5c3v1alpha1.KeystoneApplicationCredential](t, h.orderHarness)
}

func kacCondition(order *c5c3v1alpha1.KeystoneApplicationCredential, condType string) *metav1.Condition {
	return conditions.GetCondition(order.Status.Conditions, condType)
}

// expectKACConditions asserts both sub-conditions read False with reason.
func expectKACConditions(t *testing.T, order *c5c3v1alpha1.KeystoneApplicationCredential, reason string) {
	t.Helper()
	g := NewGomegaWithT(t)
	for _, condType := range keystoneApplicationCredentialSubConditionTypes {
		cond := kacCondition(order, condType)
		g.Expect(cond).NotTo(BeNil(), condType)
		g.Expect(cond.Status).To(Equal(metav1.ConditionFalse), condType)
		g.Expect(cond.Reason).To(Equal(reason), condType)
	}
}

// kacLabelled counts the ApplicationCredentials, Secrets and PushSecrets in the
// ControlPlane's namespace that carry the order's name label.
func kacLabelled(t *testing.T, c client.Client) int {
	t.Helper()
	key := keystoneApplicationCredentialLabelKeys.Name
	return labelledIn(t, c, &orcv1alpha1.ApplicationCredentialList{}, key, kacTestName) +
		labelledIn(t, c, &corev1.SecretList{}, key, kacTestName) +
		labelledIn(t, c, &esov1alpha1.PushSecretList{}, key, kacTestName)
}

// --- lifecycle and gates ---

func TestKeystoneApplicationCredential_InstallsTheFinalizerOnceAssigned(t *testing.T) {
	g := NewGomegaWithT(t)

	order := keystoneApplicationCredentialCR()
	order.Finalizers = nil
	h := newKACHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil,
		append([]client.Object{order, kacControlPlane("")}, kacReadyReferences("")...)...)

	result, err := h.reconcile(context.Background())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).NotTo(BeZero(), "the finalizer pass requeues")
	got := kacGet(t, h)
	g.Expect(got.Finalizers).To(ContainElement(keystoneApplicationCredentialFinalizerName))
	for _, condType := range keystoneApplicationCredentialSubConditionTypes {
		g.Expect(kacCondition(got, condType)).To(BeNil(), "the finalizer pass writes no %s", condType)
	}
	g.Expect(kacLabelled(t, h.mgmt)).To(BeZero())
}

// TestKeystoneApplicationCredential_AdmissionGates covers the scaffold's gates:
// both sub-conditions take the refusal, nothing is created in the
// ControlPlane's namespace, and an order the operator does not serve gets no
// finalizer.
func TestKeystoneApplicationCredential_AdmissionGates(t *testing.T) {
	longCluster := strings.Repeat("c", 64)
	cases := []struct {
		name       string
		cluster    string
		cp         *c5c3v1alpha1.ControlPlane
		wantReason string
	}{
		{
			name: "the namespace is not assigned on the management cluster", cp: keystoneUserControlPlane(),
			wantReason: reasonOrderNamespaceNotAssigned,
		},
		{
			name: "the namespace is not assigned on a target cluster", cluster: kuTestCluster,
			cp: keystoneUserControlPlane(assignOn(kuTestNamespace, "")), wantReason: reasonOrderNamespaceNotAssigned,
		},
		{
			name: "a cluster name no label value can carry", cluster: longCluster,
			cp: keystoneUserControlPlane(assignOn(kuTestNamespace, longCluster)), wantReason: reasonOrderClusterNameTooLong,
		},
		{
			name: "the admin credential is not ready", cp: func() *c5c3v1alpha1.ControlPlane {
				cp := korcControlPlane()
				cp.Spec.NamespaceAssignments = []c5c3v1alpha1.NamespaceAssignmentSpec{assignOn(kuTestNamespace, "")}
				return cp
			}(),
			wantReason: reasonWaitingForServiceAccountAdmin,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			order := keystoneApplicationCredentialCR()
			if tc.wantReason != reasonWaitingForServiceAccountAdmin {
				order.Finalizers = nil
			}
			h := newKACHarness(t, tc.cluster, nil, nil,
				append([]client.Object{order, tc.cp}, kacReadyReferences(tc.cluster)...)...)

			_, err := h.reconcile(context.Background())

			g.Expect(err).NotTo(HaveOccurred())
			got := kacGet(t, h)
			expectKACConditions(t, got, tc.wantReason)
			g.Expect(kacLabelled(t, h.mgmt)).To(BeZero(), "nothing is created in the ControlPlane's namespace")
			if tc.wantReason != reasonWaitingForServiceAccountAdmin {
				g.Expect(got.Finalizers).To(BeEmpty())
			}
		})
	}
}

func TestKeystoneApplicationCredential_ReferenceGates(t *testing.T) {
	without := func(drop func(client.Object) bool) func(string) []client.Object {
		return func(cluster string) []client.Object {
			var objs []client.Object
			for _, obj := range kacReadyReferences(cluster) {
				if !drop(obj) {
					objs = append(objs, obj)
				}
			}
			return objs
		}
	}
	editing := func(edit func(client.Object)) func(string) []client.Object {
		return func(cluster string) []client.Object {
			objs := kacReadyReferences(cluster)
			for _, obj := range objs {
				edit(obj)
			}
			return objs
		}
	}
	isUser := func(obj client.Object) bool { _, ok := obj.(*c5c3v1alpha1.KeystoneUser); return ok }
	isProject := func(obj client.Object) bool { _, ok := obj.(*c5c3v1alpha1.KeystoneProject); return ok }
	isAssignment := func(obj client.Object) bool { _, ok := obj.(*c5c3v1alpha1.KeystoneRoleAssignment); return ok }
	cases := []struct {
		name       string
		objs       func(cluster string) []client.Object
		wantReason string
		wantSub    []string
	}{
		{
			name: "no user", objs: without(isUser), wantReason: reasonKeystoneRoleAssignmentUserNotFound,
			wantSub: []string{`KeystoneUser "workflow"`, `namespace "tenant-a"`},
		},
		{
			name: "no project", objs: without(isProject), wantReason: reasonKeystoneRoleAssignmentProjectNotFound,
			wantSub: []string{`KeystoneProject "workflow-project"`},
		},
		{
			name: "a user of another ControlPlane",
			objs: editing(func(obj client.Object) {
				if user, ok := obj.(*c5c3v1alpha1.KeystoneUser); ok {
					user.Spec.ControlPlaneRef = c5c3v1alpha1.ControlPlaneRefSpec{Name: "other", Namespace: "default"}
				}
			}),
			wantReason: reasonKeystoneRoleAssignmentControlPlaneMismatch,
			wantSub:    []string{"ControlPlane default/other", "this order from default/cp"},
		},
		{
			name: "a user not provisioned yet",
			objs: editing(func(obj client.Object) {
				if user, ok := obj.(*c5c3v1alpha1.KeystoneUser); ok {
					user.Status.Conditions[0].Status = metav1.ConditionFalse
					user.Status.Conditions[0].Reason = reasonProbingForCollision
				}
			}),
			wantReason: reasonKeystoneRoleAssignmentWaitingForUser,
			wantSub:    []string{"not provisioned yet (ProbingForCollision)"},
		},
		{
			name: "a project not provisioned yet",
			objs: editing(func(obj client.Object) {
				if project, ok := obj.(*c5c3v1alpha1.KeystoneProject); ok {
					project.Status.Conditions = nil
				}
			}),
			wantReason: reasonKeystoneProjectWaiting,
			wantSub:    []string{`KeystoneProject "workflow-project" is not provisioned yet`},
		},
		{
			name: "no role assignment", objs: without(isAssignment),
			wantReason: reasonKeystoneApplicationCredentialNoRoleOnProject,
			wantSub: []string{
				`no KeystoneRoleAssignment in namespace "tenant-a" has assigned a role to user "workflow" on project "workflow-project"`,
				"needs a role on the project",
			},
		},
		{
			name: "an assignment K-ORC has not assigned yet",
			objs: editing(func(obj client.Object) {
				if ra, ok := obj.(*c5c3v1alpha1.KeystoneRoleAssignment); ok {
					ra.Status.RoleID = ""
				}
			}),
			wantReason: reasonKeystoneApplicationCredentialNoRoleOnProject,
		},
		{
			name: "an assignment on another project",
			objs: editing(func(obj client.Object) {
				if ra, ok := obj.(*c5c3v1alpha1.KeystoneRoleAssignment); ok {
					ra.Spec.ProjectRef.Name = "other-project"
				}
			}),
			wantReason: reasonKeystoneApplicationCredentialNoRoleOnProject,
		},
	}
	for _, tc := range cases {
		for _, cluster := range []string{c5c3v1alpha1.ManagementCluster, kuTestCluster} {
			t.Run(tc.name+" on "+orderRef{Cluster: cluster}.location(), func(t *testing.T) {
				g := NewGomegaWithT(t)
				objs := append([]client.Object{keystoneApplicationCredentialCR(), kacControlPlane(cluster)}, tc.objs(cluster)...)
				h := newKACHarness(t, cluster, nil, nil, objs...)

				result, err := h.reconcile(context.Background())

				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(result.RequeueAfter).To(Equal(orderRefreshAfter))
				got := kacGet(t, h)
				cond := kacCondition(got, conditionTypeKeystoneApplicationCredentialCredentialReady)
				g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
				g.Expect(cond.Reason).To(Equal(tc.wantReason))
				for _, sub := range tc.wantSub {
					g.Expect(cond.Message).To(ContainSubstring(sub))
				}
				delivery := kacCondition(got, conditionTypeKeystoneApplicationCredentialDeliveryReady)
				g.Expect(delivery.Reason).To(Equal(reasonKeystoneApplicationCredentialWaiting))
				g.Expect(kacLabelled(t, h.mgmt)).To(BeZero(), "a refused order creates nothing")
			})
		}
	}
}

// TestKeystoneApplicationCredential_ProceedsOnceARoleIsAssigned lifts the role
// gate with an assignment K-ORC has assigned: the pass moves on to the mint.
func TestKeystoneApplicationCredential_ProceedsOnceARoleIsAssigned(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	refs := kacReadyReferences("")
	assignment := refs[2].(*c5c3v1alpha1.KeystoneRoleAssignment)
	assignment.Status.RoleID = ""
	h := newKACHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil,
		append([]client.Object{keystoneApplicationCredentialCR(), kacControlPlane("")}, refs...)...)

	_, err := h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(kacCondition(kacGet(t, h), conditionTypeKeystoneApplicationCredentialCredentialReady).Reason).
		To(Equal(reasonKeystoneApplicationCredentialNoRoleOnProject))

	g.Expect(h.order.Get(ctx, client.ObjectKeyFromObject(assignment), assignment)).To(Succeed())
	assignment.Status.RoleID = "r-1"
	g.Expect(h.order.Status().Update(ctx, assignment)).To(Succeed())

	_, err = h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(kacCondition(kacGet(t, h), conditionTypeKeystoneApplicationCredentialCredentialReady).Reason).
		To(Equal(reasonKeystoneApplicationCredentialWaiting), "the credential is minted once the role is assigned")
}

func TestKeystoneApplicationCredential_ReferenceReadErrorIsWrapped(t *testing.T) {
	g := NewGomegaWithT(t)

	boom := errors.New("boom")
	h := newKACHarness(t, kuTestCluster, nil, &interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*c5c3v1alpha1.KeystoneUser); ok {
				return boom
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	}, append([]client.Object{keystoneApplicationCredentialCR(), kacControlPlane(kuTestCluster)},
		kacReadyReferences(kuTestCluster)...)...)

	_, err := h.reconcile(context.Background())

	g.Expect(err).To(MatchError(boom))
	g.Expect(err.Error()).To(ContainSubstring("reading referenced KeystoneUser tenant-a/workflow"))
}

func TestRoleAssignedOnProject(t *testing.T) {
	t.Run("a cluster that does not serve the kind has none", func(t *testing.T) {
		g := NewGomegaWithT(t)
		c := orderFakeClient(t, &interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return &meta.NoKindMatchError{GroupKind: keystoneRoleAssignmentGVK.GroupKind()}
			},
		})
		assigned, err := roleAssignedOnProject(context.Background(), c, keystoneApplicationCredentialCR())
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(assigned).To(BeFalse())
	})

	t.Run("a list error is wrapped", func(t *testing.T) {
		g := NewGomegaWithT(t)
		boom := errors.New("boom")
		c := orderFakeClient(t, &interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error { return boom },
		})
		_, err := roleAssignedOnProject(context.Background(), c, keystoneApplicationCredentialCR())
		g.Expect(err).To(MatchError(boom))
		g.Expect(err.Error()).To(ContainSubstring(`listing KeystoneRoleAssignments in "tenant-a"`))
	})

	t.Run("a role list error ends the pass", func(t *testing.T) {
		g := NewGomegaWithT(t)
		boom := errors.New("boom")
		h := newKACHarness(t, c5c3v1alpha1.ManagementCluster, &interceptor.Funcs{
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*c5c3v1alpha1.KeystoneRoleAssignmentList); ok {
					return boom
				}
				return cl.List(ctx, list, opts...)
			},
		}, nil, append([]client.Object{keystoneApplicationCredentialCR(), kacControlPlane("")}, kacReadyReferences("")...)...)

		_, err := h.reconcile(context.Background())

		g.Expect(err).To(MatchError(boom))
		g.Expect(kacLabelled(t, h.mgmt)).To(BeZero())
	})
}

// TestKeystoneApplicationCredential_PassResult pins when the next pass runs once
// both legs had their say: the legs' timer on a converged order on the
// management cluster, the refresh at the latest everywhere else.
func TestKeystoneApplicationCredential_PassResult(t *testing.T) {
	timer := ctrl.Result{RequeueAfter: 30 * time.Hour}
	short := ctrl.Result{RequeueAfter: 3 * time.Second}
	refresh := ctrl.Result{RequeueAfter: orderRefreshAfter}
	cases := []struct {
		name      string
		cluster   string
		converged bool
		result    ctrl.Result
		want      ctrl.Result
	}{
		{name: "a converged order on the management cluster waits for its timer", converged: true, result: timer, want: timer},
		{name: "a converged order on the management cluster without a timer waits for an event", converged: true},
		{name: "a converged order on a target cluster refreshes first", cluster: kuTestCluster, converged: true, result: timer, want: refresh},
		{name: "a shorter requeue stands", cluster: kuTestCluster, result: short, want: short},
		{name: "an unconverged order refreshes", result: timer, want: refresh},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			order := keystoneApplicationCredentialCR()
			if tc.converged {
				order.Status.Conditions = []metav1.Condition{{
					Type: conditionTypeKeystoneApplicationCredentialDeliveryReady, Status: metav1.ConditionTrue,
					Reason: reasonKeystoneUserDelivered,
				}}
			}
			g.Expect(keystoneApplicationCredentialPassResult(order, tc.cluster, tc.result)).To(Equal(tc.want))
		})
	}
}

// --- naming ---

func TestKeystoneApplicationCredential_ChildNames(t *testing.T) {
	g := NewGomegaWithT(t)

	order := keystoneApplicationCredentialCR()
	local := keystoneApplicationCredentialChildPrefix(order, c5c3v1alpha1.ManagementCluster)
	remote := keystoneApplicationCredentialChildPrefix(order, kuTestCluster)
	g.Expect(local).To(MatchRegexp(`^workflow-appcred-[0-9a-f]{8}-applicationcredential-$`))
	g.Expect(remote).NotTo(Equal(local), "the same order on another cluster is another order")

	g.Expect(keystoneApplicationCredentialMintCloudName(order, "")).To(Equal(local + "mint-cloud"))
	g.Expect(keystoneApplicationCredentialSecretName(order, "", 3)).To(Equal(local + "secret-v3"))
	g.Expect(keystoneApplicationCredentialCredentialName(order, "", 3)).To(Equal(local + "credential-v3"))
	g.Expect(keystoneApplicationCredentialSourceSecretName(order, "")).To(Equal(local + "source"))
	g.Expect(keystoneApplicationCredentialPushSecretName(order, "")).To(Equal(local + "backup"))
	g.Expect(keystoneApplicationCredentialCredentialsSecretName(order)).To(Equal("workflow-appcred-credentials"))
	g.Expect(keystoneApplicationCredentialRemoteKeyFor(ksControlPlane(), local)).To(MatchRegexp(
		`^openstack/keystone/default/workflow-appcred-[0-9a-f]{8}-applicationcredential/service-accounts/application-credential$`))

	order.Name = strings.Repeat("a", 63)
	g.Expect(len(keystoneApplicationCredentialCredentialName(order, kuTestCluster, 1<<62))).To(BeNumerically("<=", 253))
}

func TestCredentialGenerationOf(t *testing.T) {
	cases := []struct {
		name    string
		wantGen int64
		wantOK  bool
	}{
		{name: "p-credential-v1", wantGen: 1, wantOK: true},
		{name: "p-credential-v42", wantGen: 42, wantOK: true},
		{name: "p-credential-v0"},
		{name: "p-credential-v-1"},
		{name: "p-credential-vx"},
		{name: "p-credential-v"},
		{name: "q-credential-v1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			gen, ok := credentialGenerationOf(tc.name, "p-credential-v")
			g.Expect(ok).To(Equal(tc.wantOK))
			g.Expect(gen).To(Equal(tc.wantGen))
		})
	}
}

// TestKeystoneApplicationCredential_StoredDefaults resolves the defaults the
// reconciler applies to an object stored without the schema's.
func TestKeystoneApplicationCredential_StoredDefaults(t *testing.T) {
	g := NewGomegaWithT(t)

	order := keystoneApplicationCredentialCR()
	order.Spec.CredentialGeneration = 0
	order.Spec.Rotation = c5c3v1alpha1.KeystoneApplicationCredentialRotationSpec{}
	g.Expect(keystoneApplicationCredentialGeneration(order)).To(Equal(int64(1)))
	interval, grace := keystoneApplicationCredentialRotation(order)
	g.Expect(interval).To(Equal(720 * time.Hour))
	g.Expect(grace).To(Equal(24 * time.Hour))

	order.Spec.Rotation.Interval = &metav1.Duration{}
	interval, _ = keystoneApplicationCredentialRotation(order)
	g.Expect(interval).To(BeZero(), "an explicit 0s turns the schedule off")
}

// --- watch mappers and predicates ---

// kacEnqueued fires a create event for obj through handler built for
// clusterName on c and returns what lands in the workqueue.
func kacEnqueued(
	handler mchandler.TypedEventHandlerFunc[client.Object, mcreconcile.Request], clusterName mcruntime.ClusterName,
	c client.Client, obj client.Object,
) []mcreconcile.Request {
	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[mcreconcile.Request]())
	defer queue.ShutDown()
	handler(clusterName, fakeTargetCluster{c: c}).
		Create(context.Background(), event.TypedCreateEvent[client.Object]{Object: obj}, queue)
	var requests []mcreconcile.Request
	for queue.Len() > 0 {
		request, _ := queue.Get()
		requests = append(requests, request)
		queue.Done(request)
	}
	return requests
}

func TestKeystoneApplicationCredentialReferenceMappers(t *testing.T) {
	named := keystoneApplicationCredentialCR()
	otherProject := keystoneApplicationCredentialCR()
	otherProject.Name = "other-project-appcred"
	otherProject.Spec.ProjectRef.Name = "other-project"
	otherUser := keystoneApplicationCredentialCR()
	otherUser.Name = "other-user-appcred"
	otherUser.Spec.UserRef.Name = "someone"
	c := orderFakeClient(t, nil, named, otherProject, otherUser)
	request := func(order client.Object) mcreconcile.Request {
		return mcreconcile.Request{
			Request: reconcile.Request{NamespacedName: client.ObjectKeyFromObject(order)}, ClusterName: kuTestCluster,
		}
	}

	t.Run("a user maps to the orders naming it", func(t *testing.T) {
		g := NewGomegaWithT(t)
		g.Expect(kacEnqueued(referencedOrderToApplicationCredentialRequests(applicationCredentialUserName),
			kuTestCluster, c, keystoneUserCR(kuTestNamespace))).
			To(ConsistOf(request(named), request(otherProject)))
	})

	t.Run("a project maps to the orders naming it", func(t *testing.T) {
		g := NewGomegaWithT(t)
		g.Expect(kacEnqueued(referencedOrderToApplicationCredentialRequests(applicationCredentialProjectName),
			kuTestCluster, c, keystoneProjectCR())).
			To(ConsistOf(request(named), request(otherUser)))
	})

	t.Run("an assignment maps to the orders naming both of its references", func(t *testing.T) {
		g := NewGomegaWithT(t)
		g.Expect(kacEnqueued(roleAssignmentToApplicationCredentialRequests(), kuTestCluster, c, keystoneRoleAssignmentCR())).
			To(ConsistOf(request(named)))
	})

	t.Run("a list failure maps to nothing", func(t *testing.T) {
		g := NewGomegaWithT(t)
		failing := orderFakeClient(t, &interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return errors.New("cache not synced")
			},
		})
		g.Expect(kacEnqueued(roleAssignmentToApplicationCredentialRequests(), kuTestCluster, failing,
			keystoneRoleAssignmentCR())).To(BeEmpty())
	})
}

func TestApplicationCredentialAccessors(t *testing.T) {
	g := NewGomegaWithT(t)

	order := keystoneApplicationCredentialCR()
	g.Expect(applicationCredentialUserName(order)).To(Equal(kuTestName))
	g.Expect(applicationCredentialProjectName(order)).To(Equal(kpTestName))
}

func TestKeystoneApplicationCredentialReferencePredicate(t *testing.T) {
	user, project := kraReadyReferences()
	user.Status.PasswordGeneration = 1
	cases := []struct {
		name string
		old  client.Object
		new  func() client.Object
		want bool
	}{
		{name: "a status write that changes nothing the leg reads", old: user, new: func() client.Object {
			u := user.DeepCopy()
			u.Status.UserID = "u-1"
			return u
		}, want: false},
		{name: "the user's password generation moves", old: user, new: func() client.Object {
			u := user.DeepCopy()
			u.Status.PasswordGeneration = 2
			return u
		}, want: true},
		{name: "the user's UserReady flips", old: user, new: func() client.Object {
			u := user.DeepCopy()
			u.Status.Conditions[0].Status = metav1.ConditionFalse
			return u
		}, want: true},
		{name: "a project's name is reported", old: project, new: func() client.Object {
			p := project.DeepCopy()
			p.Status.ProjectName = kpTestName
			return p
		}, want: false},
		{name: "a deletion", old: project, new: func() client.Object {
			p := project.DeepCopy()
			p.DeletionTimestamp = ptr.To(metav1.Now())
			return p
		}, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			g.Expect(keystoneApplicationCredentialReferencePredicate().Update(event.UpdateEvent{
				ObjectOld: tc.old, ObjectNew: tc.new(),
			})).To(Equal(tc.want))
		})
	}
}

func TestKeystoneApplicationCredentialRoleAssignmentPredicate(t *testing.T) {
	assignment := keystoneRoleAssignmentCR()
	cases := []struct {
		name string
		new  func() client.Object
		want bool
	}{
		{name: "the role id is reported", new: func() client.Object {
			ra := assignment.DeepCopy()
			ra.Status.RoleID = "r-1"
			return ra
		}, want: true},
		{name: "another status write", new: func() client.Object {
			ra := assignment.DeepCopy()
			ra.Status.UserID = "u-1"
			return ra
		}, want: false},
		{name: "a spec change", new: func() client.Object {
			ra := assignment.DeepCopy()
			ra.Generation = 2
			return ra
		}, want: true},
		{name: "a deletion", new: func() client.Object {
			ra := assignment.DeepCopy()
			ra.DeletionTimestamp = ptr.To(metav1.Now())
			return ra
		}, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			g.Expect(keystoneApplicationCredentialRoleAssignmentPredicate().Update(event.UpdateEvent{
				ObjectOld: assignment, ObjectNew: tc.new(),
			})).To(Equal(tc.want))
		})
	}
}

func TestControlPlaneToKeystoneApplicationCredentialsMapper(t *testing.T) {
	g := NewGomegaWithT(t)

	explicit := keystoneApplicationCredentialCR()
	elsewhere := keystoneApplicationCredentialCR()
	elsewhere.Name = "other-plane"
	elsewhere.Spec.ControlPlaneRef.Namespace = "other"
	c := fake.NewClientBuilder().WithScheme(korcTestScheme(t)).WithObjects(explicit, elsewhere).
		WithIndex(&c5c3v1alpha1.KeystoneApplicationCredential{}, KeystoneApplicationCredentialControlPlaneRefIndexKey,
			keystoneApplicationCredentialControlPlaneRefExtractor).
		Build()

	g.Expect(controlPlaneToKeystoneApplicationCredentialsMapper(c)(context.Background(), ksControlPlane())).To(Equal(
		[]reconcile.Request{{NamespacedName: client.ObjectKeyFromObject(explicit)}}))

	failing := fake.NewClientBuilder().WithScheme(korcTestScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("cache not synced")
		},
	}).Build()
	g.Expect(controlPlaneToKeystoneApplicationCredentialsMapper(failing)(context.Background(), ksControlPlane())).To(BeNil())
}

func TestKeystoneApplicationCredentialControlPlaneRefExtractor(t *testing.T) {
	g := NewGomegaWithT(t)

	order := keystoneApplicationCredentialCR()
	g.Expect(keystoneApplicationCredentialControlPlaneRefExtractor(order)).To(Equal([]string{"default/cp"}))
	order.Spec.ControlPlaneRef.Namespace = ""
	g.Expect(keystoneApplicationCredentialControlPlaneRefExtractor(order)).To(Equal([]string{"tenant-a/cp"}))
	order.Spec.ControlPlaneRef.Name = ""
	g.Expect(keystoneApplicationCredentialControlPlaneRefExtractor(order)).To(BeNil())
	g.Expect(keystoneApplicationCredentialControlPlaneRefExtractor(ksControlPlane())).To(BeNil())
}

// TestKeystoneApplicationCredential_ReconcileEntry covers the branches
// Reconcile answers itself.
func TestKeystoneApplicationCredential_ReconcileEntry(t *testing.T) {
	t.Run("a missing order is not an error", func(t *testing.T) {
		g := NewGomegaWithT(t)
		h := newKACHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil)
		result, err := h.reconcile(context.Background())
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(result.IsZero()).To(BeTrue())
	})

	t.Run("an unresolvable cluster is skipped", func(t *testing.T) {
		g := NewGomegaWithT(t)
		h := newKACHarness(t, kuTestCluster, nil, nil, keystoneApplicationCredentialCR(), kacControlPlane(kuTestCluster))
		h.r.(*KeystoneApplicationCredentialReconciler).Resolver = &childrenResolver{err: mcruntime.ErrClusterNotFound}

		result, err := h.reconcile(context.Background())

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(result.IsZero()).To(BeTrue())
		g.Expect(kacGet(t, h).Status.Conditions).To(BeEmpty())
	})

	t.Run("a failed read of the order is returned", func(t *testing.T) {
		g := NewGomegaWithT(t)
		boom := errors.New("boom")
		h := newKACHarness(t, c5c3v1alpha1.ManagementCluster, &interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*c5c3v1alpha1.KeystoneApplicationCredential); ok {
					return boom
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}, nil, keystoneApplicationCredentialCR(), kacControlPlane(""))

		_, err := h.reconcile(context.Background())

		g.Expect(err).To(MatchError(boom))
		g.Expect(err.Error()).To(ContainSubstring("fetching KeystoneApplicationCredential"))
	})
}

// --- teardown ---

// kacDeletingOrder returns an order marked for deletion with every child
// seeded: generation 1's credential held by K-ORC's finalizer, its secret, the
// mint document, the source Secret, the PushSecret and the delivered Secret.
func kacDeletingOrder() (*c5c3v1alpha1.KeystoneApplicationCredential, []client.Object) {
	order := keystoneApplicationCredentialCR()
	order.DeletionTimestamp = ptr.To(metav1.Now())
	labels := keystoneApplicationCredentialRef(order, "").childLabels()
	child := func(name string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: name, Namespace: "default", Labels: labels}
	}
	credential := &orcv1alpha1.ApplicationCredential{ObjectMeta: child(keystoneApplicationCredentialCredentialName(order, "", 1))}
	credential.Finalizers = []string{"openstack.k-orc.cloud/applicationcredential"}
	return order, []client.Object{
		credential,
		&corev1.Secret{ObjectMeta: child(keystoneApplicationCredentialSecretName(order, "", 1))},
		&corev1.Secret{ObjectMeta: child(keystoneApplicationCredentialMintCloudName(order, ""))},
		&corev1.Secret{ObjectMeta: child(keystoneApplicationCredentialSourceSecretName(order, ""))},
		&esov1alpha1.PushSecret{ObjectMeta: child(keystoneApplicationCredentialPushSecretName(order, ""))},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name: "workflow-appcred-credentials", Namespace: kuTestNamespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: c5c3v1alpha1.GroupVersion.String(), Kind: "KeystoneApplicationCredential",
				Name: order.Name, UID: order.UID, Controller: ptr.To(true),
			}},
		}},
	}
}

// kacDeleteRecorder records the names of the objects deleted through it.
func kacDeleteRecorder(deleted *[]string) *interceptor.Funcs {
	return &interceptor.Funcs{
		Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			*deleted = append(*deleted, obj.GetName())
			return cl.Delete(ctx, obj, opts...)
		},
	}
}

// TestKeystoneApplicationCredential_DeleteRunsInTwoPhases issues every delete
// but the mint document's while K-ORC still holds a credential, then the mint
// document and the delivered Secret, then releases the finalizer.
func TestKeystoneApplicationCredential_DeleteRunsInTwoPhases(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	order, seeded := kacDeletingOrder()
	var deleted []string
	h := newKACHarness(t, c5c3v1alpha1.ManagementCluster, kacDeleteRecorder(&deleted), nil,
		append([]client.Object{order, kacControlPlane("")}, seeded...)...)
	prefix := keystoneApplicationCredentialChildPrefix(order, "")

	result, err := h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(korcRequeueAfter))
	g.Expect(deleted).To(Equal([]string{
		prefix + "backup", prefix + "credential-v1", prefix + "secret-v1", prefix + "source",
	}), "the mint document stays while K-ORC still deletes a credential through it")
	g.Expect(kacGet(t, h).Finalizers).To(ContainElement(keystoneApplicationCredentialFinalizerName))
	g.Expect(h.mgmt.Get(ctx, types.NamespacedName{Namespace: kuTestNamespace, Name: "workflow-appcred-credentials"},
		&corev1.Secret{})).To(Succeed(), "the delivered Secret stays in the first phase")

	credential := &orcv1alpha1.ApplicationCredential{}
	g.Expect(h.mgmt.Get(ctx, types.NamespacedName{Namespace: "default", Name: prefix + "credential-v1"}, credential)).To(Succeed())
	credential.Finalizers = nil
	g.Expect(h.mgmt.Update(ctx, credential)).To(Succeed())

	deleted = nil
	result, err = h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(korcRequeueAfter))
	g.Expect(deleted).To(Equal([]string{prefix + "mint-cloud", "workflow-appcred-credentials"}))
	g.Expect(kacGet(t, h).Finalizers).To(ContainElement(keystoneApplicationCredentialFinalizerName))

	deleted = nil
	_, err = h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(deleted).To(BeEmpty())
	g.Expect(kacGet(t, h)).To(BeNil(), "the order is gone once nothing it owns is listed")
	g.Expect(kacLabelled(t, h.mgmt)).To(BeZero())
}

func TestKeystoneApplicationCredential_DeleteFailsOpenWithoutTheControlPlane(t *testing.T) {
	g := NewGomegaWithT(t)

	order, seeded := kacDeletingOrder()
	var deleted []string
	h := newKACHarness(t, c5c3v1alpha1.ManagementCluster, kacDeleteRecorder(&deleted), nil,
		append([]client.Object{order}, seeded...)...)
	prefix := keystoneApplicationCredentialChildPrefix(order, "")

	_, err := h.reconcile(context.Background())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(deleted).To(Equal([]string{
		prefix + "backup", prefix + "credential-v1", prefix + "secret-v1", prefix + "mint-cloud", prefix + "source",
		"workflow-appcred-credentials",
	}), "without a plane one pass issues every delete")
	g.Expect(kacGet(t, h)).To(BeNil(), "without a plane the finalizer is released at once")
}

func TestKeystoneApplicationCredential_DeleteOfAFrozenOrderTearsDown(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	order, seeded := kacDeletingOrder()
	seeded[0].SetFinalizers(nil)
	h := newKACHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil,
		append([]client.Object{order, keystoneUserControlPlane()}, seeded...)...)

	for range 3 {
		_, err := h.reconcile(ctx)
		g.Expect(err).NotTo(HaveOccurred())
	}
	g.Expect(kacGet(t, h)).To(BeNil(), "a withdrawn assignment does not stop the teardown")
	g.Expect(kacLabelled(t, h.mgmt)).To(BeZero())
}

func TestKeystoneApplicationCredential_TeardownFailures(t *testing.T) {
	boom := errors.New("boom")
	cases := []struct {
		name    string
		funcs   *interceptor.Funcs
		wantErr string
	}{
		{
			name: "a failed credential list",
			funcs: &interceptor.Funcs{
				List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if _, ok := list.(*orcv1alpha1.ApplicationCredentialList); ok {
						return boom
					}
					return cl.List(ctx, list, opts...)
				},
			},
			wantErr: "listing order *v1alpha1.ApplicationCredentialList",
		},
		{
			name: "a failed delete",
			funcs: &interceptor.Funcs{
				Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					if _, ok := obj.(*orcv1alpha1.ApplicationCredential); ok {
						return boom
					}
					return cl.Delete(ctx, obj, opts...)
				},
			},
			wantErr: "deleting KeystoneApplicationCredential child",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			order, seeded := kacDeletingOrder()
			h := newKACHarness(t, c5c3v1alpha1.ManagementCluster, tc.funcs, nil,
				append([]client.Object{order, kacControlPlane("")}, seeded...)...)

			_, err := h.reconcile(context.Background())

			g.Expect(err).To(MatchError(boom))
			g.Expect(err.Error()).To(ContainSubstring(tc.wantErr))
			g.Expect(kacGet(t, h).Finalizers).To(ContainElement(keystoneApplicationCredentialFinalizerName))
		})
	}

	t.Run("a delivered Secret the order does not control stays", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ctx := context.Background()
		order, seeded := kacDeletingOrder()
		foreign := seeded[len(seeded)-1].(*corev1.Secret)
		foreign.OwnerReferences = nil
		h := newKACHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil, append([]client.Object{order}, seeded...)...)

		_, err := h.reconcile(ctx)

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(kacGet(t, h)).To(BeNil())
		err = h.mgmt.Get(ctx, client.ObjectKeyFromObject(foreign), &corev1.Secret{})
		g.Expect(apierrors.IsNotFound(err)).To(BeFalse(), "a Secret of the delivered name the order does not own stays")
	})
}
