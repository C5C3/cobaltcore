// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the KeystoneUser delivery leg: the OpenBao backup, the Secret beside
// the order, its repair, and the auth URL per cluster.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// deliveredCloud is the parsed auth block of a delivered clouds.yaml, with the
// project keys a scoped document would carry so a test can prove they are
// absent.
type deliveredCloud struct {
	Auth struct {
		AuthURL           string `json:"auth_url"`
		Username          string `json:"username"`
		Password          string `json:"password"`
		UserDomainName    string `json:"user_domain_name"`
		ProjectName       string `json:"project_name"`
		ProjectDomainName string `json:"project_domain_name"`
	} `json:"auth"`
}

// deliveredSecret reads the order's delivered Secret off its cluster.
func deliveredSecret(t *testing.T, c client.Client) (*corev1.Secret, bool) {
	t.Helper()
	secret := &corev1.Secret{}
	err := c.Get(context.Background(), types.NamespacedName{Namespace: kuTestNamespace, Name: "workflow-credentials"}, secret)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		t.Fatalf("reading the delivered Secret: %v", err)
	}
	return secret, true
}

// deliveredAuth parses the delivered clouds.yaml's single cloud.
func deliveredAuth(t *testing.T, secret *corev1.Secret) deliveredCloud {
	t.Helper()
	g := NewGomegaWithT(t)
	var doc struct {
		Clouds map[string]deliveredCloud `json:"clouds"`
	}
	g.Expect(yaml.Unmarshal(secret.Data[appCredCloudsYAMLKey], &doc)).To(Succeed())
	g.Expect(doc.Clouds).To(HaveLen(1))
	for _, cloud := range doc.Clouds {
		return cloud
	}
	return deliveredCloud{}
}

// convergedOrderHarness seeds an assigned, converged order on cluster with every
// child in the ControlPlane's namespace converged on generation 1.
func convergedOrderHarness(
	t *testing.T, cp *c5c3v1alpha1.ControlPlane, cluster string, orderFuncs *interceptor.Funcs, extra ...client.Object,
) (*c5c3v1alpha1.KeystoneUser, *kuHarness) {
	t.Helper()
	cp.Spec.NamespaceAssignments = []c5c3v1alpha1.NamespaceAssignmentSpec{assignOn(kuTestNamespace, cluster)}
	order := keystoneUserCR(kuTestNamespace)
	objs := append([]client.Object{order, cp}, kuConvergedChildren(order, cp, cluster, 1)...)
	return order, newKUHarness(t, cluster, nil, orderFuncs, append(objs, extra...)...)
}

// TestKeystoneUserDelivery_ManagementClusterOrderIsDelivered is the whole
// contract on the management cluster, beside a Managed Keystone that is not
// placed: the Secret, its clouds.yaml, its owner, the backup in the
// ControlPlane's namespace, the status, and the result of a converged pass.
func TestKeystoneUserDelivery_ManagementClusterOrderIsDelivered(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	cp := ksControlPlane()
	order, h := convergedOrderHarness(t, cp, c5c3v1alpha1.ManagementCluster, nil)

	result, err := h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue(), "the ControlPlane watch reaches a delivered order on the management cluster")

	secret, found := deliveredSecret(t, h.order)
	g.Expect(found).To(BeTrue())
	g.Expect(string(secret.Data[serviceAccountPasswordKey])).To(Equal("seeded-password"))
	auth := deliveredAuth(t, secret).Auth
	g.Expect(auth.AuthURL).To(Equal("http://cp-keystone.default.svc:5000/v3"))
	g.Expect(auth.Username).To(Equal(kuTestName))
	g.Expect(auth.UserDomainName).To(Equal(adminDomainName(cp)))
	g.Expect(auth.Password).To(Equal("seeded-password"))
	g.Expect(string(secret.Data[appCredCloudsYAMLKey])).NotTo(ContainSubstring("project_name"))
	g.Expect(string(secret.Data[appCredCloudsYAMLKey])).NotTo(ContainSubstring("project_domain_name"))
	g.Expect(secret.Labels).To(Equal(map[string]string{
		keystoneUserNameLabel: kuTestName, keystoneUserNamespaceLabel: kuTestNamespace,
	}))
	got := h.get(t)
	g.Expect(metav1.IsControlledBy(secret, got)).To(BeTrue(), "the order owns its Secret")

	g.Expect(got.Status.SecretName).To(Equal("workflow-credentials"))
	g.Expect(got.Status.SecretKeys).To(Equal([]string{"clouds.yaml", "password"}))
	delivery := kuCondition(got, conditionTypeKeystoneUserDeliveryReady)
	g.Expect(delivery.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(delivery.Reason).To(Equal(reasonKeystoneUserDelivered))
	g.Expect(conditions.AllTrue(got.Status.Conditions, "Ready")).To(BeTrue())

	prefix := keystoneUserChildPrefix(order, "")
	source := &corev1.Secret{}
	g.Expect(h.mgmt.Get(ctx, types.NamespacedName{Namespace: "default", Name: prefix + "source"}, source)).To(Succeed())
	g.Expect(source.Labels).To(Equal(keystoneUserChildLabels(order, "")))
	g.Expect(source.Data).To(HaveKey("clouds.yaml"))
	push := &esov1alpha1.PushSecret{}
	g.Expect(h.mgmt.Get(ctx, types.NamespacedName{Namespace: "default", Name: prefix + "backup"}, push)).To(Succeed())
	g.Expect(push.Labels).To(Equal(keystoneUserChildLabels(order, "")))
	g.Expect(push.Spec.Data).To(HaveLen(1))
	g.Expect(push.Spec.Data[0].Match.RemoteRef.RemoteKey).To(MatchRegexp(
		`^openstack/keystone/default/workflow-[0-9a-f]{8}-user/service-accounts/credentials$`))
	storeRef := effectiveControlPlaneStoreRef(cp)
	g.Expect(push.Spec.SecretStoreRefs).To(HaveLen(1))
	g.Expect(push.Spec.SecretStoreRefs[0].Name).To(Equal(storeRef.Name))
	g.Expect(push.Spec.SecretStoreRefs[0].Kind).To(Equal(string(storeRef.Kind)))
	g.Expect(push.Spec.DeletionPolicy).To(Equal(esov1alpha1.PushSecretDeletionPolicyDelete))
	g.Expect(push.Spec.Selector.Secret.Name).To(Equal(prefix + "source"))
}

func TestKeystoneUserDelivery_WaitsForTheBackup(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	_, h := convergedOrderHarness(t, cp, c5c3v1alpha1.ManagementCluster, nil)
	push := &esov1alpha1.PushSecret{}
	g.Expect(h.mgmt.Get(context.Background(), types.NamespacedName{
		Namespace: "default", Name: keystoneUserPushSecretName(keystoneUserCR(kuTestNamespace), ""),
	}, push)).To(Succeed())
	push.Status.Conditions = nil
	g.Expect(h.mgmt.Update(context.Background(), push)).To(Succeed())

	result, err := h.reconcile(context.Background())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(korcRequeueAfter))
	g.Expect(kuCondition(h.get(t), conditionTypeKeystoneUserDeliveryReady).Reason).To(Equal(reasonKeystoneUserBackupNotSynced))
	_, found := deliveredSecret(t, h.order)
	g.Expect(found).To(BeFalse(), "nothing is delivered before the backup holds it")
}

// TestKeystoneUserDelivery_UnpublishedKeystone covers an order on a cluster that
// cannot reach the in-cluster Keystone Service while nothing is published: the
// leg refuses with a zero result, the pass turns that into the refresh, and the
// user stays provisioned.
func TestKeystoneUserDelivery_UnpublishedKeystone(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	order, h := convergedOrderHarness(t, cp, kuTestCluster, nil)

	result, err := h.reconcile(context.Background())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(orderRefreshAfter))
	got := h.get(t)
	delivery := kuCondition(got, conditionTypeKeystoneUserDeliveryReady)
	g.Expect(delivery.Reason).To(Equal(reasonKeystoneUserKeystoneNotPublished))
	g.Expect(delivery.Message).To(ContainSubstring("spec.services.keystone.publicEndpoint"))
	g.Expect(delivery.Message).To(ContainSubstring(`target cluster "edge-1"`))
	g.Expect(kuCondition(got, conditionTypeKeystoneUserUserReady).Status).To(Equal(metav1.ConditionTrue))
	_, found := deliveredSecret(t, h.order)
	g.Expect(found).To(BeFalse())

	legResult, err := h.r.deliverCredentials(context.Background(), h.order, order, cp, kuTestCluster, 1)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(legResult.IsZero()).To(BeTrue(), "the leg itself asks for no requeue")
}

// TestKeystoneUserDelivery_AuthURLPerCluster pins the auth URL a delivered
// document carries: the public endpoint off Keystone's cluster, the in-cluster
// URL on it, and the external authURL in External mode.
func TestKeystoneUserDelivery_AuthURLPerCluster(t *testing.T) {
	cases := []struct {
		name    string
		cluster string
		cp      func() *c5c3v1alpha1.ControlPlane
		want    string
	}{
		{
			name: "published endpoint on a target cluster", cluster: kuTestCluster,
			cp: func() *c5c3v1alpha1.ControlPlane {
				cp := ksControlPlane()
				cp.Spec.Services.Keystone.PublicEndpoint = "https://keystone.example.test/v3"
				return cp
			},
			want: "https://keystone.example.test/v3",
		},
		{
			name: "the cluster Keystone is placed on", cluster: kuTestCluster,
			cp: func() *c5c3v1alpha1.ControlPlane {
				cp := ksControlPlane()
				cp.Spec.Services.Keystone.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: kuTestCluster}
				cp.Spec.Services.Keystone.PublicEndpoint = "https://keystone.example.test/v3"
				return cp
			},
			want: "http://cp-keystone.default.svc:5000/v3",
		},
		{
			name: "external mode", cluster: kuTestCluster,
			cp: func() *c5c3v1alpha1.ControlPlane {
				cp := korcExternalControlPlane()
				setAdminCredentialReady(cp)
				return cp
			},
			want: "https://keystone.example.com/v3",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			_, h := convergedOrderHarness(t, tc.cp(), tc.cluster, nil)

			result, err := h.reconcile(context.Background())

			g.Expect(err).NotTo(HaveOccurred())
			secret, found := deliveredSecret(t, h.order)
			g.Expect(found).To(BeTrue())
			g.Expect(deliveredAuth(t, secret).Auth.AuthURL).To(Equal(tc.want))
			g.Expect(result.RequeueAfter).To(Equal(orderRefreshAfter),
				"a converged order on a target cluster comes back on the refresh")
			_, onManagement := deliveredSecret(t, h.mgmt)
			g.Expect(onManagement).To(BeFalse(), "the Secret is written beside the order only")
		})
	}
}

func TestKeystoneUserDelivery_ForeignSecretIsRefused(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	stranger := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "workflow-credentials", Namespace: kuTestNamespace},
		Data:       map[string][]byte{"token": []byte("theirs")},
	}
	order, h := convergedOrderHarness(t, cp, c5c3v1alpha1.ManagementCluster, nil, stranger)

	result, err := h.r.deliverCredentials(context.Background(), h.order, order, cp, "", 1)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue())
	cond := kuCondition(order, conditionTypeKeystoneUserDeliveryReady)
	g.Expect(cond.Reason).To(Equal(reasonKeystoneUserDeliveryRefused))
	g.Expect(cond.Message).To(ContainSubstring("tenant-a/workflow-credentials"))
	secret, _ := deliveredSecret(t, h.order)
	g.Expect(secret.Data).To(Equal(map[string][]byte{"token": []byte("theirs")}), "the stranger's Secret is unchanged")
	g.Expect(secret.OwnerReferences).To(BeEmpty())
}

// TestKeystoneUserDelivery_RepairsTheSecret covers the three edits an owner can
// make: an overwritten password is rewritten, a deleted Secret is written again,
// and a key the operator does not own is left in place.
func TestKeystoneUserDelivery_RepairsTheSecret(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	_, h := convergedOrderHarness(t, ksControlPlane(), c5c3v1alpha1.ManagementCluster, nil)
	_, err := h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())

	secret, _ := deliveredSecret(t, h.order)
	secret.Data[serviceAccountPasswordKey] = []byte("foo")
	secret.Data["extra"] = []byte("mine")
	g.Expect(h.order.Update(ctx, secret)).To(Succeed())
	_, err = h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	secret, _ = deliveredSecret(t, h.order)
	g.Expect(string(secret.Data[serviceAccountPasswordKey])).To(Equal("seeded-password"))
	g.Expect(string(secret.Data["extra"])).To(Equal("mine"), "a key the operator does not own stays")

	g.Expect(h.order.Delete(ctx, secret)).To(Succeed())
	_, err = h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	secret, found := deliveredSecret(t, h.order)
	g.Expect(found).To(BeTrue(), "a deleted Secret is written again")
	g.Expect(string(secret.Data[serviceAccountPasswordKey])).To(Equal("seeded-password"))
}

// TestKeystoneUserDelivery_ApplyErrorIsReported fails the apply on the order's
// cluster only, which for an order on a target cluster is the delivered Secret
// alone.
func TestKeystoneUserDelivery_ApplyErrorIsReported(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	cp.Spec.Services.Keystone.PublicEndpoint = "https://keystone.example.test/v3"
	_, h := convergedOrderHarness(t, cp, kuTestCluster, &interceptor.Funcs{
		Apply: func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error {
			return errors.New("admission webhook denied the request")
		},
	})

	_, err := h.reconcile(context.Background())

	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("delivering Secret"))
	cond := kuCondition(h.get(t), conditionTypeKeystoneUserDeliveryReady)
	g.Expect(cond.Reason).To(Equal(reasonKeystoneUserDeliveryError))
	g.Expect(cond.Message).To(ContainSubstring("admission webhook denied the request"))
}

// TestKeystoneUserDelivery_RotationReachesTheSecret raises the generation on a
// delivered order: while K-ORC has not applied v2 nothing new is delivered;
// once it has, the backup is re-pushed with v2 and nothing is delivered until
// ESO reports that push, although the PushSecret still reads Ready from the v1
// push; then the Secret carries the v2 password.
func TestKeystoneUserDelivery_RotationReachesTheSecret(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	cp := ksControlPlane()
	order, h := convergedOrderHarness(t, cp, c5c3v1alpha1.ManagementCluster, nil)
	_, err := h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	pushKey := types.NamespacedName{Namespace: "default", Name: keystoneUserPushSecretName(order, "")}
	push := &esov1alpha1.PushSecret{}
	g.Expect(h.mgmt.Get(ctx, pushKey, push)).To(Succeed())
	v1Hash := push.Annotations[keystoneUserPushContentHashAnnotation]
	g.Expect(v1Hash).NotTo(BeEmpty())

	live := h.get(t)
	live.Spec.PasswordGeneration = 2
	g.Expect(h.order.Update(ctx, live)).To(Succeed())
	_, err = h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(kuCondition(h.get(t), conditionTypeKeystoneUserDeliveryReady).Reason).To(Equal(reasonWaitingForServiceAccounts),
		"between the re-point and K-ORC applying v2 nothing is delivered")

	v2 := &corev1.Secret{}
	g.Expect(h.mgmt.Get(ctx, types.NamespacedName{
		Namespace: "default", Name: keystoneUserPasswordSecretName(order, "", 2),
	}, v2)).To(Succeed())
	user, _ := kuGetUser(t, h.mgmt, keystoneUserUserRef(order, ""))
	user.Status.Resource.AppliedPasswordRef = v2.Name
	g.Expect(h.mgmt.Status().Update(ctx, user)).To(Succeed())

	_, err = h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	got := h.get(t)
	g.Expect(got.Status.PasswordGeneration).To(Equal(int64(2)))
	g.Expect(kuCondition(got, conditionTypeKeystoneUserDeliveryReady).Reason).To(Equal(reasonKeystoneUserBackupNotSynced),
		"a Ready left over from the v1 push does not let v2 through")
	secret, _ := deliveredSecret(t, h.order)
	g.Expect(string(secret.Data[serviceAccountPasswordKey])).To(Equal("seeded-password"),
		"nothing is delivered before OpenBao holds v2")

	source := &corev1.Secret{}
	g.Expect(h.mgmt.Get(ctx, types.NamespacedName{
		Namespace: "default", Name: keystoneUserSourceSecretName(order, ""),
	}, source)).To(Succeed())
	g.Expect(source.Data[serviceAccountPasswordKey]).To(Equal(v2.Data[serviceAccountPasswordKey]),
		"the document the backup pushes carries v2")
	g.Expect(h.mgmt.Get(ctx, pushKey, push)).To(Succeed())
	sum := sha256.Sum256(source.Data[appCredCloudsYAMLKey])
	g.Expect(push.Annotations).To(HaveKeyWithValue(keystoneUserPushContentHashAnnotation, hex.EncodeToString(sum[:])),
		"the re-push is stamped with the v2 document")
	g.Expect(push.Annotations[keystoneUserPushContentHashAnnotation]).NotTo(Equal(v1Hash))

	// ESO pushes the new stamp: syncedResourceVersion moves.
	push.Status.SyncedResourceVersion = "2-pushed"
	g.Expect(h.mgmt.Update(ctx, push)).To(Succeed())

	_, err = h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	got = h.get(t)
	g.Expect(kuCondition(got, conditionTypeKeystoneUserDeliveryReady).Reason).To(Equal(reasonKeystoneUserDelivered))
	secret, _ = deliveredSecret(t, h.order)
	g.Expect(secret.Data[serviceAccountPasswordKey]).To(Equal(v2.Data[serviceAccountPasswordKey]))
	g.Expect(string(secret.Data[serviceAccountPasswordKey])).NotTo(Equal("seeded-password"))
}

// TestKeystoneUserDelivery_MissingPasswordIsAWait covers a generation whose
// password Secret is not there yet: a wait, not an error.
func TestKeystoneUserDelivery_MissingPasswordIsAWait(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	order, h := convergedOrderHarness(t, cp, c5c3v1alpha1.ManagementCluster, nil)

	result, err := h.r.deliverCredentials(context.Background(), h.order, order, cp, "", 7)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(korcRequeueAfter))
	cond := kuCondition(order, conditionTypeKeystoneUserDeliveryReady)
	g.Expect(cond.Reason).To(Equal(reasonWaitingForServiceAccounts))
	g.Expect(cond.Message).To(ContainSubstring("generation 7"))
	var pushSecrets esov1alpha1.PushSecretList
	g.Expect(h.mgmt.List(context.Background(), &pushSecrets)).To(Succeed())
	g.Expect(pushSecrets.Items).To(HaveLen(1), "no backup is touched without a password")
	g.Expect(pushSecrets.Items[0].Annotations).To(Equal(kuSeededPushAnnotations(order, cp, "")))
}

// TestKeystoneUserDelivery_ForeignPushSecretIsRefused seeds a PushSecret at the
// order's child name that carries the same order's labels for another cluster:
// the delivery refuses to adopt it and leaves it as it is.
func TestKeystoneUserDelivery_ForeignPushSecretIsRefused(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	cp := ksControlPlane()
	cp.Spec.NamespaceAssignments = []c5c3v1alpha1.NamespaceAssignmentSpec{assignOn(kuTestNamespace, "")}
	order := keystoneUserCR(kuTestNamespace)
	children := kuConvergedChildren(order, cp, "", 1)
	foreignLabels := keystoneUserChildLabels(order, kuTestCluster)
	for _, obj := range children {
		if _, ok := obj.(*esov1alpha1.PushSecret); ok {
			obj.SetLabels(foreignLabels)
		}
	}
	h := newKUHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil, append([]client.Object{order, cp}, children...)...)

	_, err := h.reconcile(ctx)

	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("refusing to adopt"))
	g.Expect(kuCondition(h.get(t), conditionTypeKeystoneUserDeliveryReady).Reason).To(Equal(reasonKeystoneUserDeliveryError))
	push := &esov1alpha1.PushSecret{}
	g.Expect(h.mgmt.Get(ctx, types.NamespacedName{
		Namespace: cp.Namespace, Name: keystoneUserPushSecretName(order, ""),
	}, push)).To(Succeed())
	g.Expect(push.Labels).To(Equal(foreignLabels), "the foreign PushSecret is not claimed")
	_, found := deliveredSecret(t, h.order)
	g.Expect(found).To(BeFalse())
}
