// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the KeystoneApplicationCredential provision leg: the mint
// document, the credential generations, the schedule and its triggers, the
// grace sweep and the stray prune, all driven by the harness clock.
package controller

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/yaml"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// kacCredentialChild returns generation gen's ApplicationCredential as K-ORC
// reports it with conds, carrying the Keystone id when id is set.
func kacCredentialChild(
	order *c5c3v1alpha1.KeystoneApplicationCredential, cluster string, gen int64, id string,
	expiresAt *metav1.Time, conds []metav1.Condition,
) *orcv1alpha1.ApplicationCredential {
	ac := &orcv1alpha1.ApplicationCredential{
		ObjectMeta: metav1.ObjectMeta{
			Name: keystoneApplicationCredentialCredentialName(order, cluster, gen), Namespace: "default",
			Labels: keystoneApplicationCredentialRef(order, cluster).childLabels(),
		},
		Spec: orcv1alpha1.ApplicationCredentialSpec{
			ManagementPolicy: orcv1alpha1.ManagementPolicyManaged,
			Resource:         &orcv1alpha1.ApplicationCredentialResourceSpec{ExpiresAt: expiresAt},
		},
		Status: orcv1alpha1.ApplicationCredentialStatus{Conditions: conds},
	}
	if id != "" {
		ac.Status.ID = ptr.To(id)
		ac.Status.Resource = &orcv1alpha1.ApplicationCredentialResourceStatus{ExpiresAt: expiresAt}
	}
	return ac
}

// kacSecretChild returns generation gen's secret Secret carrying value.
func kacSecretChild(order *c5c3v1alpha1.KeystoneApplicationCredential, cluster string, gen int64, value string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: keystoneApplicationCredentialSecretName(order, cluster, gen), Namespace: "default",
			Labels: keystoneApplicationCredentialRef(order, cluster).childLabels(),
		},
		Data: map[string][]byte{appCredSecretValueKey: []byte(value)},
	}
}

// kacMinted records on order that generation gen, with the Keystone id id,
// became the delivered credential at at, and returns its Available
// ApplicationCredential and its secret.
func kacMinted(
	order *c5c3v1alpha1.KeystoneApplicationCredential, cluster string, gen int64, id string, at time.Time,
) []client.Object {
	interval, grace := keystoneApplicationCredentialRotation(order)
	var expires *metav1.Time
	if interval > 0 {
		expires = &metav1.Time{Time: at.Add(interval + grace)}
	}
	order.Status.CredentialID = id
	order.Status.CredentialGeneration = gen
	order.Status.LastRotation = &metav1.Time{Time: at}
	order.Status.CredentialExpiresAt = expires
	order.Status.NextRotation = nextRotation(order, interval, grace)
	return []client.Object{
		kacCredentialChild(order, cluster, gen, id, expires, availableImportConditions()),
		kacSecretChild(order, cluster, gen, "secret-of-"+id),
	}
}

// newMintedKACHarness seeds an assigned order on cluster with its references
// and generation 1 minted at kacTestClock with the id ac-1.
func newMintedKACHarness(
	t *testing.T, cluster string, order *c5c3v1alpha1.KeystoneApplicationCredential, mgmtFuncs *interceptor.Funcs,
	extra ...client.Object,
) *kacHarness {
	t.Helper()
	objs := append([]client.Object{order, kacControlPlane(cluster)}, kacReadyReferences(cluster)...)
	objs = append(objs, kacMinted(order, cluster, 1, "ac-1", kacTestClock)...)
	return newKACHarness(t, cluster, mgmtFuncs, nil, append(objs, extra...)...)
}

// kacCredential reads generation gen's ApplicationCredential, or nil once it
// is gone.
func kacCredential(t *testing.T, h *kacHarness, gen int64) *orcv1alpha1.ApplicationCredential {
	t.Helper()
	ac := &orcv1alpha1.ApplicationCredential{}
	key := types.NamespacedName{
		Namespace: "default",
		Name:      keystoneApplicationCredentialCredentialName(keystoneApplicationCredentialCR(), h.cluster, gen),
	}
	if err := h.mgmt.Get(context.Background(), key, ac); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		t.Fatalf("reading credential generation %d: %v", gen, err)
	}
	return ac
}

// kacSecret reads generation gen's secret Secret, or nil once it is gone.
func kacSecret(t *testing.T, h *kacHarness, gen int64) *corev1.Secret {
	t.Helper()
	secret := &corev1.Secret{}
	key := types.NamespacedName{
		Namespace: "default",
		Name:      keystoneApplicationCredentialSecretName(keystoneApplicationCredentialCR(), h.cluster, gen),
	}
	if err := h.mgmt.Get(context.Background(), key, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		t.Fatalf("reading the secret of credential generation %d: %v", gen, err)
	}
	return secret
}

// kacMarkAvailable reports generation gen Available with the Keystone id id
// and the expiry its spec asked for, as K-ORC does once Keystone holds it.
func kacMarkAvailable(t *testing.T, h *kacHarness, gen int64, id string) {
	t.Helper()
	g := NewGomegaWithT(t)
	ac := kacCredential(t, h, gen)
	g.Expect(ac).NotTo(BeNil())
	ac.Status = orcv1alpha1.ApplicationCredentialStatus{
		Conditions: availableImportConditions(),
		ID:         ptr.To(id),
		Resource:   &orcv1alpha1.ApplicationCredentialResourceStatus{ExpiresAt: ac.Spec.Resource.ExpiresAt},
	}
	g.Expect(h.mgmt.Update(context.Background(), ac)).To(Succeed())
}

// kacCredentialReady returns the order's CredentialReady.
func kacCredentialReady(t *testing.T, h *kacHarness) *metav1.Condition {
	t.Helper()
	return kacCondition(kacGet(t, h), conditionTypeKeystoneApplicationCredentialCredentialReady)
}

// kacMintCloud parses the mint document's single cloud.
func kacMintCloud(t *testing.T, h *kacHarness) (*corev1.Secret, deliveredCloud) {
	t.Helper()
	g := NewGomegaWithT(t)
	secret := &corev1.Secret{}
	g.Expect(h.mgmt.Get(context.Background(), types.NamespacedName{
		Namespace: "default", Name: keystoneApplicationCredentialMintCloudName(keystoneApplicationCredentialCR(), h.cluster),
	}, secret)).To(Succeed())
	var doc struct {
		Clouds map[string]deliveredCloud `json:"clouds"`
	}
	g.Expect(yaml.Unmarshal(secret.Data[appCredCloudsYAMLKey], &doc)).To(Succeed())
	g.Expect(doc.Clouds).To(HaveKey("admin"))
	return secret, doc.Clouds["admin"]
}

// --- the mint document ---

func TestKeystoneApplicationCredentialProvision_WritesTheMintDocument(t *testing.T) {
	for _, cluster := range []string{c5c3v1alpha1.ManagementCluster, kuTestCluster} {
		t.Run(orderRef{Cluster: cluster}.location(), func(t *testing.T) {
			g := NewGomegaWithT(t)
			order := keystoneApplicationCredentialCR()
			cp := kacControlPlane(cluster)
			h := newKACHarness(t, cluster, nil, nil, append([]client.Object{order, cp}, kacReadyReferences(cluster)...)...)

			_, err := h.reconcile(context.Background())

			g.Expect(err).NotTo(HaveOccurred())
			secret, cloud := kacMintCloud(t, h)
			g.Expect(secret.Labels).To(Equal(keystoneApplicationCredentialRef(order, cluster).childLabels()))
			g.Expect(secret.OwnerReferences).To(BeEmpty())
			g.Expect(cloud.Auth.Username).To(Equal(kuTestName))
			g.Expect(cloud.Auth.Password).To(Equal("user-password-1"))
			g.Expect(cloud.Auth.ProjectName).To(Equal(kpTestName))
			g.Expect(cloud.Auth.UserDomainName).To(Equal(adminDomainName(cp)))
			g.Expect(cloud.Auth.ProjectDomainName).To(Equal(adminDomainName(cp)))
			g.Expect(cloud.Auth.AuthURL).To(Equal(korcAuthURL(cp, nil)),
				"K-ORC reads the mint document on the management cluster")
			g.Expect(secret.Data).NotTo(HaveKey(korcCACertKey), "a co-located Keystone has no bundle")
		})
	}

	t.Run("a placed Keystone's CA bundle is stamped beside it", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := kacControlPlane("")
		cp.Spec.Services.Keystone.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "edge"}
		cp.Spec.Services.Keystone.CABundleSecretRef = &commonv1.SecretRefSpec{Name: "keystone-ca", Key: "ca.crt"}
		h := newKACHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil,
			append([]client.Object{keystoneApplicationCredentialCR(), cp, externalCASecret()}, kacReadyReferences("")...)...)

		_, err := h.reconcile(context.Background())

		g.Expect(err).NotTo(HaveOccurred())
		secret, cloud := kacMintCloud(t, h)
		g.Expect(string(secret.Data[korcCACertKey])).To(Equal(testCABundle))
		g.Expect(cloud.Auth.AuthURL).To(Equal("https://keystone.example.test/v3"))
	})
}

// TestKeystoneApplicationCredentialProvision_PasswordRotationReachesTheMintDocument
// moves the user's password generation on a minted order: the mint document
// carries the new password on the next pass, and no credential child changes.
func TestKeystoneApplicationCredentialProvision_PasswordRotationReachesTheMintDocument(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	order := keystoneApplicationCredentialCR()
	h := newMintedKACHarness(t, c5c3v1alpha1.ManagementCluster, order, nil)
	_, err := h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	_, cloud := kacMintCloud(t, h)
	g.Expect(cloud.Auth.Password).To(Equal("user-password-1"))
	before := kacCredential(t, h, 1).ResourceVersion

	user := &c5c3v1alpha1.KeystoneUser{}
	g.Expect(h.order.Get(ctx, types.NamespacedName{Namespace: kuTestNamespace, Name: kuTestName}, user)).To(Succeed())
	g.Expect(h.mgmt.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: keystoneUserPasswordSecretName(user, "", 2), Namespace: "default"},
		Data:       map[string][]byte{serviceAccountPasswordKey: []byte("user-password-2")},
	})).To(Succeed())
	user.Status.PasswordGeneration = 2
	g.Expect(h.order.Status().Update(ctx, user)).To(Succeed())

	_, err = h.reconcile(ctx)

	g.Expect(err).NotTo(HaveOccurred())
	_, cloud = kacMintCloud(t, h)
	g.Expect(cloud.Auth.Password).To(Equal("user-password-2"))
	g.Expect(kacCredential(t, h, 1).ResourceVersion).To(Equal(before), "the credential is not touched")
	g.Expect(kacCredential(t, h, 2)).To(BeNil(), "a password rotation is not a credential rotation")
}

func TestKeystoneApplicationCredentialProvision_MintDocumentWaits(t *testing.T) {
	editRefs := func(edit func(obj client.Object) client.Object) []client.Object {
		var objs []client.Object
		for _, obj := range kacReadyReferences("") {
			if obj = edit(obj); obj != nil {
				objs = append(objs, obj)
			}
		}
		return objs
	}
	cases := []struct {
		name       string
		cp         func() *c5c3v1alpha1.ControlPlane
		refs       []client.Object
		wantReason string
		wantSub    string
	}{
		{
			name: "the password Secret is missing",
			refs: editRefs(func(obj client.Object) client.Object {
				if _, ok := obj.(*corev1.Secret); ok {
					return nil
				}
				return obj
			}),
			wantReason: reasonKeystoneRoleAssignmentWaitingForUser,
			wantSub:    `the password of generation 1 of KeystoneUser "workflow" is not available yet`,
		},
		{
			name: "the password key is empty",
			refs: editRefs(func(obj client.Object) client.Object {
				if secret, ok := obj.(*corev1.Secret); ok {
					secret.Data = nil
				}
				return obj
			}),
			wantReason: reasonKeystoneRoleAssignmentWaitingForUser,
			wantSub:    "is not available yet",
		},
		{
			name: "the project reports no name",
			refs: editRefs(func(obj client.Object) client.Object {
				if project, ok := obj.(*c5c3v1alpha1.KeystoneProject); ok {
					project.Status.ProjectName = ""
				}
				return obj
			}),
			wantReason: reasonKeystoneProjectWaiting,
			wantSub:    `KeystoneProject "workflow-project" reports no project name yet`,
		},
		{
			name: "the CA bundle Secret is missing",
			cp: func() *c5c3v1alpha1.ControlPlane {
				cp := kacControlPlane("")
				cp.Spec.Services.Keystone.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "edge"}
				cp.Spec.Services.Keystone.CABundleSecretRef = &commonv1.SecretRefSpec{Name: "keystone-ca", Key: "ca.crt"}
				return cp
			},
			refs:       kacReadyReferences(""),
			wantReason: reasonKeystoneApplicationCredentialWaitingForCABundle,
			wantSub:    "spec.services.keystone.caBundleSecretRef",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := kacControlPlane("")
			if tc.cp != nil {
				cp = tc.cp()
			}
			h := newKACHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil,
				append([]client.Object{keystoneApplicationCredentialCR(), cp}, tc.refs...)...)

			result, err := h.reconcile(context.Background())

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(result.RequeueAfter).To(Equal(korcRequeueAfter))
			cond := kacCredentialReady(t, h)
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(tc.wantReason))
			g.Expect(cond.Message).To(ContainSubstring(tc.wantSub))
			g.Expect(kacLabelled(t, h.mgmt)).To(BeZero(), "nothing is written before the mint document can be")
		})
	}

	t.Run("a password read error is wrapped", func(t *testing.T) {
		g := NewGomegaWithT(t)
		boom := errors.New("boom")
		order := keystoneApplicationCredentialCR()
		h := newKACHarness(t, c5c3v1alpha1.ManagementCluster, &interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.Secret); ok && strings.Contains(key.Name, "-user-password-v") {
					return boom
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}, nil, append([]client.Object{order, kacControlPlane("")}, kacReadyReferences("")...)...)

		_, err := h.reconcile(context.Background())

		g.Expect(err).To(MatchError(boom))
		user := keystoneUserCR(kuTestNamespace)
		g.Expect(err.Error()).To(ContainSubstring(`reading the password Secret default/` +
			keystoneUserPasswordSecretName(user, "", 1) + ` of KeystoneUser "workflow"`))
		g.Expect(kacCredentialReady(t, h).Reason).To(Equal(reasonKeystoneApplicationCredentialError))
	})
}

// --- generations ---

// TestKeystoneApplicationCredentialProvision_FirstMint walks generation 1 from
// the first admitted pass to the switch: the secret and the credential with
// the exact spec, the wait, and the status once K-ORC reports it Available.
func TestKeystoneApplicationCredentialProvision_FirstMint(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	order := keystoneApplicationCredentialCR()
	cp := kacControlPlane("")
	h := newKACHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil,
		append([]client.Object{order, cp}, kacReadyReferences("")...)...)
	prefix := keystoneApplicationCredentialChildPrefix(order, "")

	result, err := h.reconcile(ctx)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(korcRequeueAfter))
	secret := kacSecret(t, h, 1)
	g.Expect(secret).NotTo(BeNil())
	g.Expect(string(secret.Data[appCredSecretValueKey])).To(MatchRegexp(`^[A-Za-z0-9_-]{43}$`))
	g.Expect(secret.Labels).To(Equal(keystoneApplicationCredentialRef(order, "").childLabels()))
	value := string(secret.Data[appCredSecretValueKey])

	ac := kacCredential(t, h, 1)
	g.Expect(ac).NotTo(BeNil())
	g.Expect(ac.Labels).To(Equal(keystoneApplicationCredentialRef(order, "").childLabels()))
	g.Expect(ac.OwnerReferences).To(BeEmpty())
	g.Expect(ac.Spec.ManagementPolicy).To(Equal(orcv1alpha1.ManagementPolicyManaged))
	g.Expect(ac.Spec.CloudCredentialsRef).To(Equal(orcv1alpha1.CloudCredentialsReference{
		SecretName: prefix + "mint-cloud", CloudName: korcCloudName(cp),
	}))
	res := ac.Spec.Resource
	g.Expect(res).NotTo(BeNil())
	user := keystoneUserCR(kuTestNamespace)
	g.Expect(string(res.UserRef)).To(Equal(keystoneUserUserRef(user, "")))
	g.Expect(string(res.SecretRef)).To(Equal(prefix + "secret-v1"))
	g.Expect(res.Unrestricted).To(Equal(ptr.To(false)))
	g.Expect(res.Description).To(Equal(ptr.To("KeystoneApplicationCredential tenant-a/workflow-appcred generation 1")))
	g.Expect(res.ExpiresAt).NotTo(BeNil())
	g.Expect(res.ExpiresAt.Time).To(BeTemporally("==", kacTestClock.Add(720*time.Hour+24*time.Hour)))

	cond := kacCredentialReady(t, h)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonKeystoneApplicationCredentialWaiting))

	_, err = h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(string(kacSecret(t, h, 1).Data[appCredSecretValueKey])).To(Equal(value), "the value is written once")
	g.Expect(kacCredential(t, h, 1).Spec.Resource.ExpiresAt.Time).To(BeTemporally("==", res.ExpiresAt.Time),
		"a live credential is never applied again")

	kacMarkAvailable(t, h, 1, "ac-1")
	_, err = h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	got := kacGet(t, h)
	g.Expect(got.Status.CredentialID).To(Equal("ac-1"))
	g.Expect(got.Status.CredentialGeneration).To(Equal(int64(1)))
	g.Expect(got.Status.LastRotation.Time).To(BeTemporally("==", h.now))
	g.Expect(got.Status.CredentialExpiresAt.Time).To(BeTemporally("==", res.ExpiresAt.Time))
	g.Expect(got.Status.NextRotation.Time).To(BeTemporally("==", h.now.Add(720*time.Hour)))
	g.Expect(got.Status.PreviousCredentialID).To(BeEmpty())
	g.Expect(got.Status.PreviousCredentialDeleteAt).To(BeNil())
	cond = kacCondition(got, conditionTypeKeystoneApplicationCredentialCredentialReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(reasonKeystoneApplicationCredentialMinted))
	g.Expect(cond.Message).To(Equal(`credential generation 1 (id ac-1) is minted for user "workflow" on project "workflow-project"`))
	g.Expect(string(kacSecret(t, h, 1).Data[appCredSecretValueKey])).To(Equal(value))
}

func TestKeystoneApplicationCredentialProvision_ScheduleOffMintsWithoutExpiry(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	order := keystoneApplicationCredentialCR()
	order.Spec.Rotation.Interval = &metav1.Duration{}
	h := newKACHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil,
		append([]client.Object{order, kacControlPlane("")}, kacReadyReferences("")...)...)

	_, err := h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(kacCredential(t, h, 1).Spec.Resource.ExpiresAt).To(BeNil())

	kacMarkAvailable(t, h, 1, "ac-1")
	result, err := h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	got := kacGet(t, h)
	g.Expect(got.Status.CredentialID).To(Equal("ac-1"))
	g.Expect(got.Status.CredentialExpiresAt).To(BeNil())
	g.Expect(got.Status.NextRotation).To(BeNil())
	g.Expect(result.RequeueAfter).To(Equal(orderRefreshAfter), "no timer runs with the schedule off")
}

// TestKeystoneApplicationCredentialProvision_ExpiryReadsKORCFirst switches to
// generation 1 with an expiry K-ORC reports apart from the one the CR asked
// for: status takes the reported one, and the CR's own only while K-ORC reports
// none. The schedule follows the expiry status holds.
func TestKeystoneApplicationCredentialProvision_ExpiryReadsKORCFirst(t *testing.T) {
	reported := &metav1.Time{Time: kacTestClock.Add(100 * time.Hour)}
	cases := []struct {
		name       string
		resource   *orcv1alpha1.ApplicationCredentialResourceStatus
		wantExpiry time.Time
		wantNext   time.Time
	}{
		{
			name:       "the expiry K-ORC reports",
			resource:   &orcv1alpha1.ApplicationCredentialResourceStatus{ExpiresAt: reported},
			wantExpiry: reported.Time,
			wantNext:   reported.Add(-24 * time.Hour),
		},
		{
			name:       "the CR's own while K-ORC reports none",
			wantExpiry: kacTestClock.Add(744 * time.Hour),
			wantNext:   kacTestClock.Add(720 * time.Hour),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			ctx := context.Background()
			h := newKACHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil,
				append([]client.Object{keystoneApplicationCredentialCR(), kacControlPlane("")}, kacReadyReferences("")...)...)
			_, err := h.reconcile(ctx)
			g.Expect(err).NotTo(HaveOccurred())
			ac := kacCredential(t, h, 1)
			ac.Status = orcv1alpha1.ApplicationCredentialStatus{
				Conditions: availableImportConditions(), ID: ptr.To("ac-1"), Resource: tc.resource,
			}
			g.Expect(h.mgmt.Update(ctx, ac)).To(Succeed())

			_, err = h.reconcile(ctx)

			g.Expect(err).NotTo(HaveOccurred())
			got := kacGet(t, h)
			g.Expect(got.Status.CredentialExpiresAt.Time).To(BeTemporally("==", tc.wantExpiry))
			g.Expect(got.Status.NextRotation.Time).To(BeTemporally("==", tc.wantNext))
		})
	}
}

func TestKeystoneApplicationCredentialProvision_KORCOutcomes(t *testing.T) {
	order := keystoneApplicationCredentialCR()
	cases := []struct {
		name       string
		ac         *orcv1alpha1.ApplicationCredential
		funcs      *interceptor.Funcs
		wantReason string
		wantSub    string
	}{
		{
			name:       "not yet Available",
			ac:         kacCredentialChild(order, "", 1, "", nil, pendingImportConditions(0)),
			wantReason: reasonKeystoneApplicationCredentialWaiting,
			wantSub:    "credential generation 1 is registered but not yet Available",
		},
		{
			name:       "Available without an id",
			ac:         kacCredentialChild(order, "", 1, "", nil, availableImportConditions()),
			wantReason: reasonKeystoneApplicationCredentialWaiting,
		},
		{
			name:       "a terminal error",
			ac:         kacCredentialChild(order, "", 1, "", nil, terminalImportConditions("the user has no role on the project")),
			wantReason: reasonKeystoneApplicationCredentialFailed,
			wantSub:    "K-ORC reported a terminal error on credential generation 1: the user has no role on the project",
		},
		{
			name: "a latch the operator cannot clear",
			ac:   kacCredentialChild(order, "", 1, "", nil, transportLatchedConditions()),
			funcs: &interceptor.Funcs{
				SubResourcePatch: func(ctx context.Context, cl client.Client, subResourceName string,
					obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption,
				) error {
					return apierrors.NewForbidden(orcv1alpha1.Resource("applicationcredentials"), obj.GetName(),
						errors.New("applicationcredentials/status is forbidden"))
				},
			},
			wantReason: conditionReasonTransportErrorRetryFailed,
			wantSub:    "forbidden",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			h := newKACHarness(t, c5c3v1alpha1.ManagementCluster, tc.funcs, nil,
				append([]client.Object{keystoneApplicationCredentialCR(), kacControlPlane(""), tc.ac},
					kacReadyReferences("")...)...)

			result, err := h.reconcile(context.Background())

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(result.RequeueAfter).To(Equal(korcRequeueAfter))
			cond := kacCredentialReady(t, h)
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(tc.wantReason))
			g.Expect(cond.Message).To(ContainSubstring(tc.wantSub))
			g.Expect(kacGet(t, h).Status.CredentialID).To(BeEmpty())
		})
	}
}

func TestKeystoneApplicationCredentialProvision_ApplyErrorIsReported(t *testing.T) {
	g := NewGomegaWithT(t)

	boom := errors.New("boom")
	h := newKACHarness(t, c5c3v1alpha1.ManagementCluster, &interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*orcv1alpha1.ApplicationCredential); ok {
				return boom
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	}, nil, append([]client.Object{keystoneApplicationCredentialCR(), kacControlPlane("")}, kacReadyReferences("")...)...)

	_, err := h.reconcile(context.Background())

	g.Expect(err).To(MatchError(boom))
	g.Expect(kacCredentialReady(t, h).Reason).To(Equal(reasonKeystoneApplicationCredentialError))
}

// TestKeystoneApplicationCredentialProvision_ErrorsAreReported fails each list
// and delete of the leg and the CA bundle read: the error is returned wrapped
// and CredentialReady reads CredentialError.
func TestKeystoneApplicationCredentialProvision_ErrorsAreReported(t *testing.T) {
	boom := errors.New("boom")
	prefix := keystoneApplicationCredentialChildPrefix(keystoneApplicationCredentialCR(), "")
	listFails := func(fails func(client.ObjectList) bool) *interceptor.Funcs {
		return &interceptor.Funcs{
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if fails(list) {
					return boom
				}
				return cl.List(ctx, list, opts...)
			},
		}
	}
	deleteFails := func(name string) *interceptor.Funcs {
		return &interceptor.Funcs{
			Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if obj.GetName() == name {
					return boom
				}
				return cl.Delete(ctx, obj, opts...)
			},
		}
	}
	// seed returns the order with its references, and generation 1 minted when
	// minted is set.
	seed := func(cp *c5c3v1alpha1.ControlPlane, minted bool, extra ...client.Object) []client.Object {
		order := keystoneApplicationCredentialCR()
		objs := append([]client.Object{order, cp}, kacReadyReferences("")...)
		if minted {
			objs = append(objs, kacMinted(order, "", 1, "ac-1", kacTestClock)...)
		}
		return append(objs, extra...)
	}
	// graceOver seeds generation 2 live and generation 1 superseded with its
	// grace period over, its ApplicationCredential still listed when listed is
	// set.
	graceOver := func(listed bool) []client.Object {
		order := keystoneApplicationCredentialCR()
		previous := kacMinted(order.DeepCopy(), "", 1, "ac-1", kacTestClock.Add(-time.Hour))
		if !listed {
			previous = previous[1:]
		}
		objs := append([]client.Object{order, kacControlPlane("")}, kacReadyReferences("")...)
		objs = append(objs, previous...)
		objs = append(objs, kacMinted(order, "", 2, "ac-2", kacTestClock)...)
		order.Status.PreviousCredentialID = "ac-1"
		order.Status.PreviousCredentialGeneration = 1
		order.Status.PreviousCredentialDeleteAt = &metav1.Time{Time: kacTestClock}
		return objs
	}
	placedKeystone := kacControlPlane("")
	placedKeystone.Spec.Services.Keystone.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "edge"}
	placedKeystone.Spec.Services.Keystone.CABundleSecretRef = &commonv1.SecretRefSpec{Name: "keystone-ca", Key: "ca.crt"}

	cases := []struct {
		name    string
		funcs   *interceptor.Funcs
		objs    []client.Object
		wantErr string
	}{
		{
			name: "a credential list error",
			funcs: listFails(func(list client.ObjectList) bool {
				_, ok := list.(*orcv1alpha1.ApplicationCredentialList)
				return ok
			}),
			objs:    seed(kacControlPlane(""), false),
			wantErr: "listing the order's ApplicationCredentials",
		},
		{
			name: "a secret list error",
			funcs: listFails(func(list client.ObjectList) bool {
				_, ok := list.(*corev1.SecretList)
				return ok
			}),
			objs:    seed(kacControlPlane(""), false),
			wantErr: "listing the order's Secrets",
		},
		{
			name:  "a stray delete error",
			funcs: deleteFails(prefix + "secret-v7"),
			objs: seed(kacControlPlane(""), true,
				kacSecretChild(keystoneApplicationCredentialCR(), "", 7, "v7")),
			wantErr: "deleting stray credential generation " + prefix + "secret-v7",
		},
		{
			name:    "a superseded credential delete error",
			funcs:   deleteFails(prefix + "credential-v1"),
			objs:    graceOver(true),
			wantErr: "deleting superseded credential generation 1",
		},
		{
			name:    "a superseded secret delete error",
			funcs:   deleteFails(prefix + "secret-v1"),
			objs:    graceOver(false),
			wantErr: "deleting the secret of superseded credential generation 1",
		},
		{
			name: "a CA bundle read error",
			funcs: &interceptor.Funcs{
				Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if _, ok := obj.(*corev1.Secret); ok && key.Name == "keystone-ca" {
						return boom
					}
					return cl.Get(ctx, key, obj, opts...)
				},
			},
			objs:    seed(placedKeystone, false, externalCASecret()),
			wantErr: "reading the Keystone CA bundle",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			h := newKACHarness(t, c5c3v1alpha1.ManagementCluster, tc.funcs, nil, tc.objs...)

			_, err := h.reconcile(context.Background())

			g.Expect(err).To(MatchError(boom))
			g.Expect(err.Error()).To(ContainSubstring(tc.wantErr))
			cond := kacCredentialReady(t, h)
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(reasonKeystoneApplicationCredentialError))
		})
	}
}

// TestKeystoneApplicationCredentialProvision_RefusesAForeignCredential seeds an
// Available ApplicationCredential under generation 1's name that does not carry
// the order's labels: the order refuses to adopt it and switches nothing.
func TestKeystoneApplicationCredentialProvision_RefusesAForeignCredential(t *testing.T) {
	g := NewGomegaWithT(t)

	order := keystoneApplicationCredentialCR()
	foreign := &orcv1alpha1.ApplicationCredential{
		ObjectMeta: metav1.ObjectMeta{Name: keystoneApplicationCredentialCredentialName(order, "", 1), Namespace: "default"},
		Status:     orcv1alpha1.ApplicationCredentialStatus{Conditions: availableImportConditions(), ID: ptr.To("theirs")},
	}
	h := newKACHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil,
		append([]client.Object{order, kacControlPlane(""), foreign}, kacReadyReferences("")...)...)

	_, err := h.reconcile(context.Background())

	g.Expect(err).To(MatchError(ContainSubstring(
		"refusing to adopt pre-existing ApplicationCredential default/" + foreign.Name)))
	g.Expect(kacCredentialReady(t, h).Reason).To(Equal(reasonKeystoneApplicationCredentialError))
	g.Expect(kacGet(t, h).Status.CredentialID).To(BeEmpty(), "a foreign credential is never switched to")
}

// --- the schedule ---

// TestKeystoneApplicationCredentialProvision_ScheduledRotation walks one
// rotation: nothing before the due time, the successor at it, the delivered
// credential reported while it is minted, the switch once it is Available, and
// the grace sweep.
func TestKeystoneApplicationCredentialProvision_ScheduledRotation(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	order := keystoneApplicationCredentialCR()
	h := newMintedKACHarness(t, c5c3v1alpha1.ManagementCluster, order, nil)
	due := kacTestClock.Add(720 * time.Hour)

	h.now = due.Add(-time.Second)
	_, err := h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(kacCredential(t, h, 2)).To(BeNil(), "nothing rotates before the due time")

	h.now = due
	_, err = h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	successor := kacCredential(t, h, 2)
	g.Expect(successor).NotTo(BeNil())
	g.Expect(successor.Spec.Resource.ExpiresAt.Time).To(BeTemporally("==", due.Add(744*time.Hour)))
	g.Expect(kacSecret(t, h, 2)).NotTo(BeNil())
	cond := kacCredentialReady(t, h)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue), "the delivered credential stays valid")
	g.Expect(cond.Reason).To(Equal(reasonKeystoneApplicationCredentialMinted))
	g.Expect(cond.Message).To(Equal("credential generation 1 is delivered; generation 2 is being minted"))
	g.Expect(kacGet(t, h).Status.CredentialID).To(Equal("ac-1"))

	kacMarkAvailable(t, h, 2, "ac-2")
	h.now = due.Add(30 * time.Second)
	_, err = h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	got := kacGet(t, h)
	g.Expect(got.Status.PreviousCredentialID).To(Equal("ac-1"))
	g.Expect(got.Status.PreviousCredentialGeneration).To(Equal(int64(1)))
	g.Expect(got.Status.PreviousCredentialDeleteAt.Time).To(BeTemporally("==", h.now.Add(24*time.Hour)))
	g.Expect(got.Status.CredentialID).To(Equal("ac-2"))
	g.Expect(got.Status.CredentialGeneration).To(Equal(int64(2)))
	g.Expect(got.Status.LastRotation.Time).To(BeTemporally("==", h.now))
	g.Expect(got.Status.CredentialExpiresAt.Time).To(BeTemporally("==", due.Add(744*time.Hour)))
	g.Expect(got.Status.NextRotation.Time).To(BeTemporally("==", due.Add(720*time.Hour)),
		"the successor's expiry minus the grace period comes before the switch plus the interval")
	g.Expect(kacCredentialReady(t, h).Message).To(ContainSubstring("credential generation 2 (id ac-2) is minted"))
	g.Expect(kacCredential(t, h, 1)).NotTo(BeNil(), "the superseded credential stays during the grace period")

	var logs []string
	logger := funcr.New(func(prefix, args string) { logs = append(logs, prefix+" "+args) }, funcr.Options{})
	h.now = got.Status.PreviousCredentialDeleteAt.Time
	_, err = h.reconcile(log.IntoContext(ctx, logger))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(kacCredential(t, h, 1)).To(BeNil(), "the superseded credential is deleted once the grace period ends")
	g.Expect(kacSecret(t, h, 1)).NotTo(BeNil(), "its secret goes on the pass that finds it gone")
	g.Expect(kacGet(t, h).Status.PreviousCredentialID).To(Equal("ac-1"))

	_, err = h.reconcile(log.IntoContext(ctx, logger))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(kacSecret(t, h, 1)).To(BeNil())
	got = kacGet(t, h)
	g.Expect(got.Status.PreviousCredentialID).To(BeEmpty())
	g.Expect(got.Status.PreviousCredentialGeneration).To(BeZero())
	g.Expect(got.Status.PreviousCredentialDeleteAt).To(BeNil())
	g.Expect(strings.Join(logs, "\n")).To(ContainSubstring("deleted superseded credential generation 1 (id ac-1)"))
	g.Expect(kacCredential(t, h, 2)).NotTo(BeNil())
	g.Expect(kacSecret(t, h, 2)).NotTo(BeNil())
}

func TestKeystoneApplicationCredentialProvision_ZeroGraceDeletesInTheSwitchingPass(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	order := keystoneApplicationCredentialCR()
	order.Spec.Rotation.GracePeriod = &metav1.Duration{}
	h := newMintedKACHarness(t, c5c3v1alpha1.ManagementCluster, order, nil)
	h.now = kacTestClock.Add(720 * time.Hour)
	_, err := h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	kacMarkAvailable(t, h, 2, "ac-2")

	_, err = h.reconcile(ctx)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(kacGet(t, h).Status.CredentialID).To(Equal("ac-2"))
	g.Expect(kacCredential(t, h, 1)).To(BeNil(), "with no grace the delete is issued in the switching pass")
}

// TestKeystoneApplicationCredentialProvision_ManualRotation raises the
// declared generation: it mints that generation directly, and a declared
// generation at or below the live one changes nothing.
func TestKeystoneApplicationCredentialProvision_ManualRotation(t *testing.T) {
	t.Run("a raise to 5 mints generation 5 directly", func(t *testing.T) {
		g := NewGomegaWithT(t)
		order := keystoneApplicationCredentialCR()
		order.Spec.CredentialGeneration = 5
		h := newMintedKACHarness(t, c5c3v1alpha1.ManagementCluster, order, nil)

		_, err := h.reconcile(context.Background())

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(kacCredential(t, h, 5)).NotTo(BeNil())
		for _, gen := range []int64{2, 3, 4} {
			g.Expect(kacCredential(t, h, gen)).To(BeNil(), "generation %d is skipped", gen)
		}
	})

	for _, declared := range []int64{1, 2} {
		t.Run(fmt.Sprintf("a declared generation %d with generation 2 live changes nothing", declared), func(t *testing.T) {
			g := NewGomegaWithT(t)
			order := keystoneApplicationCredentialCR()
			order.Spec.CredentialGeneration = declared
			objs := append([]client.Object{order, kacControlPlane("")}, kacReadyReferences("")...)
			h := newKACHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil,
				append(objs, kacMinted(order, "", 2, "ac-2", kacTestClock)...)...)

			_, err := h.reconcile(context.Background())

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(kacCredential(t, h, 3)).To(BeNil())
			g.Expect(kacGet(t, h).Status.CredentialGeneration).To(Equal(int64(2)))
		})
	}
}

func TestKeystoneApplicationCredentialProvision_LostSecretRotates(t *testing.T) {
	cases := []struct {
		name string
		edit func(secret *corev1.Secret) *corev1.Secret
	}{
		{name: "a deleted secret", edit: func(*corev1.Secret) *corev1.Secret { return nil }},
		{name: "an emptied value", edit: func(secret *corev1.Secret) *corev1.Secret {
			secret.Data = map[string][]byte{appCredSecretValueKey: nil}
			return secret
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			order := keystoneApplicationCredentialCR()
			objs := append([]client.Object{order, kacControlPlane("")}, kacReadyReferences("")...)
			minted := kacMinted(order, "", 1, "ac-1", kacTestClock)
			objs = append(objs, minted[0])
			if secret := tc.edit(minted[1].(*corev1.Secret)); secret != nil {
				objs = append(objs, secret)
			}
			h := newKACHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil, objs...)

			var logs []string
			logger := funcr.New(func(prefix, args string) { logs = append(logs, prefix+" "+args) }, funcr.Options{})
			_, err := h.reconcile(log.IntoContext(context.Background(), logger))

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(kacCredential(t, h, 2)).NotTo(BeNil())
			g.Expect(strings.Join(logs, "\n")).To(ContainSubstring("the secret of credential generation 1 is lost; rotating"))
		})
	}
}

// TestKeystoneApplicationCredentialProvision_NoRotationDuringGrace raises the
// declared generation while a grace period runs: nothing is minted until the
// sweep has cleared it, and the pending rotation starts on the pass after.
func TestKeystoneApplicationCredentialProvision_NoRotationDuringGrace(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	order := keystoneApplicationCredentialCR()
	order.Spec.CredentialGeneration = 3
	objs := append([]client.Object{order, kacControlPlane("")}, kacReadyReferences("")...)
	previous := kacMinted(order.DeepCopy(), "", 1, "ac-1", kacTestClock.Add(-time.Hour))
	objs = append(objs, previous...)
	objs = append(objs, kacMinted(order, "", 2, "ac-2", kacTestClock)...)
	order.Status.PreviousCredentialID = "ac-1"
	order.Status.PreviousCredentialGeneration = 1
	order.Status.PreviousCredentialDeleteAt = &metav1.Time{Time: kacTestClock.Add(time.Hour)}
	h := newKACHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil, objs...)

	_, err := h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(kacCredential(t, h, 3)).To(BeNil(), "no rotation starts while a grace period runs")
	g.Expect(kacCredential(t, h, 1)).NotTo(BeNil())

	h.now = kacTestClock.Add(time.Hour)
	_, err = h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(kacCredential(t, h, 1)).To(BeNil())
	g.Expect(kacCredential(t, h, 3)).To(BeNil())

	_, err = h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(kacGet(t, h).Status.PreviousCredentialDeleteAt).To(BeNil())
	g.Expect(kacCredential(t, h, 3)).To(BeNil(), "the sweep clears the grace in this pass")

	_, err = h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(kacCredential(t, h, 3)).NotTo(BeNil(), "the pending rotation starts on the pass after")
}

func TestKeystoneApplicationCredentialProvision_InFlightSuccessorIsContinued(t *testing.T) {
	g := NewGomegaWithT(t)

	order := keystoneApplicationCredentialCR()
	order.Spec.CredentialGeneration = 5
	h := newMintedKACHarness(t, c5c3v1alpha1.ManagementCluster, order, nil,
		kacCredentialChild(order, "", 2, "", nil, pendingImportConditions(0)), kacSecretChild(order, "", 2, "v2"))

	_, err := h.reconcile(context.Background())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(kacCredential(t, h, 5)).To(BeNil(), "no further generation starts while one is minted")
	g.Expect(kacCredential(t, h, 2)).NotTo(BeNil())
	g.Expect(kacCredentialReady(t, h).Message).To(Equal("credential generation 1 is delivered; generation 2 is being minted"))
}

// TestKeystoneApplicationCredentialProvision_FailedSuccessorIsReplacedByARaise
// keeps a successor K-ORC failed terminally while it is the declared
// generation, and gives it up for the declared one once that is raised above
// it.
func TestKeystoneApplicationCredentialProvision_FailedSuccessorIsReplacedByARaise(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	order := keystoneApplicationCredentialCR()
	order.Spec.CredentialGeneration = 2
	h := newMintedKACHarness(t, c5c3v1alpha1.ManagementCluster, order, nil,
		kacCredentialChild(order, "", 2, "", nil, terminalImportConditions("application credential limit reached")),
		kacSecretChild(order, "", 2, "v2"))

	_, err := h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(kacCredentialReady(t, h).Reason).To(Equal(reasonKeystoneApplicationCredentialFailed))
	g.Expect(kacCredential(t, h, 2)).NotTo(BeNil(), "a failed successor stays while it is the declared generation")

	g.Expect(h.order.Get(ctx, h.key, order)).To(Succeed())
	order.Spec.CredentialGeneration = 3
	g.Expect(h.order.Update(ctx, order)).To(Succeed())

	_, err = h.reconcile(ctx)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(kacCredential(t, h, 2)).To(BeNil(), "the raise gives the failed successor up")
	g.Expect(kacSecret(t, h, 2)).To(BeNil())
	g.Expect(kacCredential(t, h, 3)).NotTo(BeNil(), "the raised generation is minted")
	g.Expect(kacGet(t, h).Status.CredentialID).To(Equal("ac-1"), "the delivered credential stays")
}

func TestKeystoneApplicationCredentialProvision_ExpiryBoundsTheSchedule(t *testing.T) {
	t.Run("a lengthened interval rotates before the expiry", func(t *testing.T) {
		g := NewGomegaWithT(t)
		order := keystoneApplicationCredentialCR()
		h := newMintedKACHarness(t, c5c3v1alpha1.ManagementCluster, order, nil)
		g.Expect(h.order.Get(context.Background(), h.key, order)).To(Succeed())
		order.Spec.Rotation.Interval = &metav1.Duration{Duration: 2000 * time.Hour}
		g.Expect(h.order.Update(context.Background(), order)).To(Succeed())

		_, err := h.reconcile(context.Background())

		g.Expect(err).NotTo(HaveOccurred())
		got := kacGet(t, h)
		g.Expect(got.Status.NextRotation.Time).To(BeTemporally("==",
			got.Status.CredentialExpiresAt.Add(-24*time.Hour)))
	})

	t.Run("an expired credential is rotated at once", func(t *testing.T) {
		g := NewGomegaWithT(t)
		order := keystoneApplicationCredentialCR()
		h := newMintedKACHarness(t, c5c3v1alpha1.ManagementCluster, order, nil)
		g.Expect(h.order.Get(context.Background(), h.key, order)).To(Succeed())
		order.Spec.Rotation.Interval = &metav1.Duration{}
		g.Expect(h.order.Update(context.Background(), order)).To(Succeed())
		h.now = kacTestClock.Add(745 * time.Hour)

		_, err := h.reconcile(context.Background())

		g.Expect(err).NotTo(HaveOccurred())
		successor := kacCredential(t, h, 2)
		g.Expect(successor).NotTo(BeNil())
		g.Expect(successor.Spec.Resource.ExpiresAt).To(BeNil(), "the schedule is off for the successor")
	})
}

func TestKeystoneApplicationCredentialProvision_StraysAreDeleted(t *testing.T) {
	g := NewGomegaWithT(t)

	order := keystoneApplicationCredentialCR()
	var deleted []string
	h := newMintedKACHarness(t, c5c3v1alpha1.ManagementCluster, order, kacDeleteRecorder(&deleted),
		kacCredentialChild(order, "", 2, "", nil, pendingImportConditions(0)), kacSecretChild(order, "", 2, "v2"),
		kacCredentialChild(order, "", 3, "", nil, pendingImportConditions(0)), kacSecretChild(order, "", 3, "v3"),
		kacSecretChild(order, "", 7, "v7"))
	prefix := keystoneApplicationCredentialChildPrefix(order, "")

	_, err := h.reconcile(context.Background())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(deleted).To(ConsistOf(prefix+"credential-v2", prefix+"secret-v2", prefix+"secret-v7"),
		"only the live generation and the highest in-flight one stay")
	g.Expect(kacCredential(t, h, 1)).NotTo(BeNil())
	g.Expect(kacCredential(t, h, 3)).NotTo(BeNil())
	g.Expect(kacSecret(t, h, 3)).NotTo(BeNil())
}

// TestKeystoneApplicationCredentialProvision_FreezePausesTheSchedule withdraws
// the assignment past the due time: nothing is minted or deleted, and the
// first pass after the entry returns rotates.
func TestKeystoneApplicationCredentialProvision_FreezePausesTheSchedule(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	order := keystoneApplicationCredentialCR()
	var deleted []string
	objs := append([]client.Object{order, keystoneUserControlPlane()}, kacReadyReferences("")...)
	objs = append(objs, kacMinted(order, "", 1, "ac-1", kacTestClock)...)
	h := newKACHarness(t, c5c3v1alpha1.ManagementCluster, kacDeleteRecorder(&deleted), nil, objs...)
	h.now = kacTestClock.Add(800 * time.Hour)

	_, err := h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	expectKACConditions(t, kacGet(t, h), reasonOrderNamespaceNotAssigned)
	g.Expect(kacCredential(t, h, 2)).To(BeNil(), "a frozen order mints nothing")
	g.Expect(deleted).To(BeEmpty(), "a frozen order deletes nothing")
	g.Expect(kacGet(t, h).Status.CredentialID).To(Equal("ac-1"))

	cp := &c5c3v1alpha1.ControlPlane{}
	g.Expect(h.mgmt.Get(ctx, types.NamespacedName{Namespace: "default", Name: "cp"}, cp)).To(Succeed())
	cp.Spec.NamespaceAssignments = []c5c3v1alpha1.NamespaceAssignmentSpec{assignOn(kuTestNamespace, "")}
	g.Expect(h.mgmt.Update(ctx, cp)).To(Succeed())

	_, err = h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(kacCredential(t, h, 2)).NotTo(BeNil(), "the overdue rotation runs once the order thaws")
}

// TestKeystoneApplicationCredentialProvision_LostRoleKeepsTheCredential deletes
// the role assignment after the mint, past the due time: the order reports
// NoRoleOnProject, mints and deletes nothing, and the delivered Secret and its
// DeliveryReady stay.
func TestKeystoneApplicationCredentialProvision_LostRoleKeepsTheCredential(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	order := keystoneApplicationCredentialCR()
	order.Status.Conditions = []metav1.Condition{{
		Type: conditionTypeKeystoneApplicationCredentialDeliveryReady, Status: metav1.ConditionTrue,
		Reason: reasonKeystoneUserDelivered, LastTransitionTime: metav1.Now(),
	}}
	var deleted []string
	delivered := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: "workflow-appcred-credentials", Namespace: kuTestNamespace,
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: c5c3v1alpha1.GroupVersion.String(), Kind: "KeystoneApplicationCredential",
			Name: order.Name, UID: order.UID, Controller: ptr.To(true),
		}},
	}}
	h := newMintedKACHarness(t, c5c3v1alpha1.ManagementCluster, order, kacDeleteRecorder(&deleted), delivered)
	assignment := keystoneRoleAssignmentCR()
	g.Expect(h.order.Get(ctx, client.ObjectKeyFromObject(assignment), assignment)).To(Succeed())
	assignment.Finalizers = nil
	g.Expect(h.order.Update(ctx, assignment)).To(Succeed())
	g.Expect(h.order.Delete(ctx, assignment)).To(Succeed())
	deleted = nil
	h.now = kacTestClock.Add(800 * time.Hour)

	result, err := h.reconcile(ctx)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(orderRefreshAfter))
	g.Expect(kacCredentialReady(t, h).Reason).To(Equal(reasonKeystoneApplicationCredentialNoRoleOnProject))
	g.Expect(kacCredential(t, h, 2)).To(BeNil(), "the schedule pauses")
	g.Expect(deleted).To(BeEmpty())
	g.Expect(h.order.Get(ctx, client.ObjectKeyFromObject(delivered), &corev1.Secret{})).To(Succeed())
	g.Expect(kacGet(t, h).Status.CredentialID).To(Equal("ac-1"))
	delivery := kacCondition(kacGet(t, h), conditionTypeKeystoneApplicationCredentialDeliveryReady)
	g.Expect(delivery.Status).To(Equal(metav1.ConditionTrue),
		"a refused reference keeps the delivered credential's DeliveryReady")
}

func TestNextRotationAndRequeueAt(t *testing.T) {
	g := NewGomegaWithT(t)

	order := keystoneApplicationCredentialCR()
	g.Expect(nextRotation(order, time.Hour, time.Minute)).To(BeNil(), "nothing is due before the first mint")
	order.Status.LastRotation = &metav1.Time{Time: kacTestClock}
	g.Expect(nextRotation(order, 0, time.Minute)).To(BeNil(), "nothing is due with the schedule off")
	g.Expect(nextRotation(order, time.Hour, time.Minute).Time).To(Equal(kacTestClock.Add(time.Hour)))
	order.Status.CredentialExpiresAt = &metav1.Time{Time: kacTestClock.Add(30 * time.Minute)}
	g.Expect(nextRotation(order, time.Hour, time.Minute).Time).To(Equal(kacTestClock.Add(29 * time.Minute)))

	g.Expect(requeueAt(nil, kacTestClock).RequeueAfter).To(BeZero())
	g.Expect(requeueAt(&metav1.Time{Time: kacTestClock}, kacTestClock).RequeueAfter).To(BeZero(), "a past time wakes nothing")
	g.Expect(requeueAt(&metav1.Time{Time: kacTestClock.Add(time.Hour)}, kacTestClock).RequeueAfter).To(Equal(time.Hour))
}

func TestGeneratedAppCredValueMutator(t *testing.T) {
	g := NewGomegaWithT(t)

	secret := &corev1.Secret{Data: map[string][]byte{}}
	g.Expect(generatedAppCredValueMutator(secret)).To(Succeed())
	value := string(secret.Data[appCredSecretValueKey])
	g.Expect(regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`).MatchString(value)).To(BeTrue())
	g.Expect(generatedAppCredValueMutator(secret)).To(Succeed())
	g.Expect(string(secret.Data[appCredSecretValueKey])).To(Equal(value), "a present value is kept")
}
