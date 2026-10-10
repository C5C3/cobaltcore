// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the KeystoneApplicationCredential delivery leg: the OpenBao
// backup, the Secret beside the order, its repair, the switch to a rotated
// credential, and the result of a converged pass.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

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
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// kacPushed returns the order's PushSecret as ESO leaves it once it has pushed
// the document of the credential id with the secret secret: the document's
// hash, the syncedResourceVersion recorded when it was stamped, and a status
// that has moved past it.
func kacPushed(
	order *c5c3v1alpha1.KeystoneApplicationCredential, cp *c5c3v1alpha1.ControlPlane, cluster, id, secret string,
) *esov1alpha1.PushSecret {
	ref := keystoneApplicationCredentialRef(order, cluster)
	sum := sha256.Sum256([]byte(buildAppCredCloudsYAML(cp, id, secret, ref.clusterRef())))
	return &esov1alpha1.PushSecret{
		ObjectMeta: metav1.ObjectMeta{
			Name: keystoneApplicationCredentialPushSecretName(order, cluster), Namespace: "default",
			Labels: ref.childLabels(),
			Annotations: map[string]string{
				keystoneApplicationCredentialPushContentHashAnnotation:  hex.EncodeToString(sum[:]),
				keystoneApplicationCredentialPushSyncedBeforeAnnotation: "1-before-push",
			},
		},
		Status: esov1alpha1.PushSecretStatus{
			Conditions:            []esov1alpha1.PushSecretStatusCondition{{Type: esov1alpha1.PushSecretReady, Status: corev1.ConditionTrue}},
			SyncedResourceVersion: "1-pushed",
		},
	}
}

// newDeliveredKACHarness seeds an order on cluster with generation 1 minted
// and its document pushed to OpenBao.
func newDeliveredKACHarness(
	t *testing.T, cluster string, order *c5c3v1alpha1.KeystoneApplicationCredential, orderFuncs *interceptor.Funcs,
	extra ...client.Object,
) *kacHarness {
	t.Helper()
	cp := kacControlPlane(cluster)
	objs := append([]client.Object{order, cp}, kacReadyReferences(cluster)...)
	objs = append(objs, kacMinted(order, cluster, 1, "ac-1", kacTestClock)...)
	objs = append(objs, kacPushed(order, cp, cluster, "ac-1", "secret-of-ac-1"))
	return newKACHarness(t, cluster, nil, orderFuncs, append(objs, extra...)...)
}

// kacDelivered reads the delivered Secret off the order's cluster.
func kacDelivered(t *testing.T, h *kacHarness) (*corev1.Secret, bool) {
	t.Helper()
	secret := &corev1.Secret{}
	err := h.order.Get(context.Background(),
		types.NamespacedName{Namespace: kuTestNamespace, Name: "workflow-appcred-credentials"}, secret)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		t.Fatalf("reading the delivered Secret: %v", err)
	}
	return secret, true
}

// kacDeliveredCloud is the parsed application-credential cloud of a
// delivered clouds.yaml.
type kacDeliveredCloud struct {
	Auth struct {
		AuthURL                     string `json:"auth_url"`
		ApplicationCredentialID     string `json:"application_credential_id"`
		ApplicationCredentialSecret string `json:"application_credential_secret"`
	} `json:"auth"`
	AuthType string `json:"auth_type"`
}

func kacDeliveredDocument(t *testing.T, secret *corev1.Secret) kacDeliveredCloud {
	t.Helper()
	g := NewGomegaWithT(t)
	var doc struct {
		Clouds map[string]kacDeliveredCloud `json:"clouds"`
	}
	g.Expect(yaml.Unmarshal(secret.Data[appCredCloudsYAMLKey], &doc)).To(Succeed())
	g.Expect(doc.Clouds).To(HaveKey("admin"))
	return doc.Clouds["admin"]
}

func kacDeliveryReady(t *testing.T, h *kacHarness) *metav1.Condition {
	t.Helper()
	return kacCondition(kacGet(t, h), conditionTypeKeystoneApplicationCredentialDeliveryReady)
}

// TestKeystoneApplicationCredentialDelivery_DeliversOnEachCluster is the
// contract on both clusters: the backup in the ControlPlane's namespace, the
// Secret beside the order with its three keys and owner, the status, and the
// result of a converged pass.
func TestKeystoneApplicationCredentialDelivery_DeliversOnEachCluster(t *testing.T) {
	cases := []struct {
		cluster     string
		wantAuthURL string
		wantResult  time.Duration
	}{
		{cluster: c5c3v1alpha1.ManagementCluster, wantAuthURL: "http://cp-keystone.default.svc:5000/v3", wantResult: 720 * time.Hour},
		{cluster: kuTestCluster, wantAuthURL: "https://keystone.example.test/v3", wantResult: orderRefreshAfter},
	}
	for _, tc := range cases {
		t.Run(orderRef{Cluster: tc.cluster}.location(), func(t *testing.T) {
			g := NewGomegaWithT(t)
			ctx := context.Background()
			order := keystoneApplicationCredentialCR()
			h := newDeliveredKACHarness(t, tc.cluster, order, nil)

			result, err := h.reconcile(ctx)

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(result.RequeueAfter).To(Equal(tc.wantResult))

			secret, found := kacDelivered(t, h)
			g.Expect(found).To(BeTrue())
			g.Expect(secret.Type).To(Equal(corev1.SecretTypeOpaque))
			g.Expect(secret.Data).To(HaveLen(3))
			g.Expect(string(secret.Data[applicationCredentialIDKey])).To(Equal("ac-1"))
			g.Expect(string(secret.Data[applicationCredentialSecretKey])).To(Equal("secret-of-ac-1"))
			cloud := kacDeliveredDocument(t, secret)
			g.Expect(cloud.AuthType).To(Equal("v3applicationcredential"))
			g.Expect(cloud.Auth.AuthURL).To(Equal(tc.wantAuthURL))
			g.Expect(cloud.Auth.ApplicationCredentialID).To(Equal("ac-1"))
			g.Expect(cloud.Auth.ApplicationCredentialSecret).To(Equal("secret-of-ac-1"))
			g.Expect(secret.Labels).To(Equal(map[string]string{
				keystoneApplicationCredentialLabelKeys.Name:      kacTestName,
				keystoneApplicationCredentialLabelKeys.Namespace: kuTestNamespace,
			}))
			got := kacGet(t, h)
			g.Expect(metav1.IsControlledBy(secret, got)).To(BeTrue(), "the order owns its Secret")
			g.Expect(got.Status.SecretName).To(Equal("workflow-appcred-credentials"))
			g.Expect(got.Status.SecretKeys).To(Equal([]string{
				"clouds.yaml", "application_credential_id", "application_credential_secret",
			}))
			delivery := kacCondition(got, conditionTypeKeystoneApplicationCredentialDeliveryReady)
			g.Expect(delivery.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(delivery.Reason).To(Equal(reasonKeystoneUserDelivered))
			g.Expect(delivery.Message).To(Equal(`credentials are delivered in Secret "workflow-appcred-credentials" ` +
				`(keys clouds.yaml, application_credential_id, application_credential_secret) in namespace "tenant-a" on ` +
				orderRef{Cluster: tc.cluster}.location()))
			g.Expect(conditions.AllTrue(got.Status.Conditions, "Ready")).To(BeTrue())

			prefix := keystoneApplicationCredentialChildPrefix(order, tc.cluster)
			source := &corev1.Secret{}
			g.Expect(h.mgmt.Get(ctx, types.NamespacedName{Namespace: "default", Name: prefix + "source"}, source)).To(Succeed())
			g.Expect(source.Labels).To(Equal(keystoneApplicationCredentialRef(order, tc.cluster).childLabels()))
			g.Expect(source.Data).To(HaveKeyWithValue("application_credential_id", []byte("ac-1")))
			g.Expect(source.Data).To(HaveKeyWithValue("application_credential_secret", []byte("secret-of-ac-1")))
			g.Expect(source.Data).To(HaveKeyWithValue("auth_url", []byte(tc.wantAuthURL)))
			g.Expect(source.Data).To(HaveKeyWithValue("region_name", []byte("RegionOne")))
			g.Expect(source.Data).To(HaveKeyWithValue("clouds.yaml", secret.Data[appCredCloudsYAMLKey]))
			push := &esov1alpha1.PushSecret{}
			g.Expect(h.mgmt.Get(ctx, types.NamespacedName{Namespace: "default", Name: prefix + "backup"}, push)).To(Succeed())
			g.Expect(push.Spec.DeletionPolicy).To(Equal(esov1alpha1.PushSecretDeletionPolicyDelete))
			g.Expect(push.Spec.Selector.Secret.Name).To(Equal(prefix + "source"))
			g.Expect(push.Spec.Data).To(HaveLen(1))
			g.Expect(push.Spec.Data[0].Match.RemoteRef.RemoteKey).To(Equal(
				"openstack/keystone/default/" + prefix[:len(prefix)-1] + "/service-accounts/application-credential"))
			storeRef := effectiveControlPlaneStoreRef(kacControlPlane(tc.cluster))
			g.Expect(push.Spec.SecretStoreRefs).To(HaveLen(1))
			g.Expect(push.Spec.SecretStoreRefs[0].Name).To(Equal(storeRef.Name))
			g.Expect(push.Annotations).To(HaveKey(keystoneApplicationCredentialPushContentHashAnnotation))
			g.Expect(push.Annotations).To(HaveKey(keystoneApplicationCredentialPushSyncedBeforeAnnotation))
		})
	}
}

func TestKeystoneApplicationCredentialDelivery_WaitsForTheBackup(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	order := keystoneApplicationCredentialCR()
	cp := kacControlPlane("")
	objs := append([]client.Object{order, cp}, kacReadyReferences("")...)
	h := newKACHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil,
		append(objs, kacMinted(order, "", 1, "ac-1", kacTestClock)...)...)

	result, err := h.reconcile(ctx)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(korcRequeueAfter))
	cond := kacDeliveryReady(t, h)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonKeystoneUserBackupNotSynced))
	_, found := kacDelivered(t, h)
	g.Expect(found).To(BeFalse(), "nothing is delivered before the backup is pushed")

	push := &esov1alpha1.PushSecret{}
	g.Expect(h.mgmt.Get(ctx, types.NamespacedName{
		Namespace: "default", Name: keystoneApplicationCredentialPushSecretName(order, ""),
	}, push)).To(Succeed())
	g.Expect(push.Annotations[keystoneApplicationCredentialPushContentHashAnnotation]).
		To(Equal(kacPushed(order, cp, "", "ac-1", "secret-of-ac-1").Annotations[keystoneApplicationCredentialPushContentHashAnnotation]))
	push.Status = esov1alpha1.PushSecretStatus{
		Conditions:            []esov1alpha1.PushSecretStatusCondition{{Type: esov1alpha1.PushSecretReady, Status: corev1.ConditionTrue}},
		SyncedResourceVersion: "2-pushed",
	}
	g.Expect(h.mgmt.Update(ctx, push)).To(Succeed())

	_, err = h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	_, found = kacDelivered(t, h)
	g.Expect(found).To(BeTrue(), "the pass that sees the push delivers")
	g.Expect(kacDeliveryReady(t, h).Reason).To(Equal(reasonKeystoneUserDelivered))
}

// TestKeystoneApplicationCredentialDelivery_RotationSwitchesTheSecret switches
// to generation 2 on a pass whose backup ESO has pushed: the delivered Secret
// carries the new id and secret in the switching pass, and the converged result
// is the shorter of the time to the next rotation and to the grace's end.
func TestKeystoneApplicationCredentialDelivery_RotationSwitchesTheSecret(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	order := keystoneApplicationCredentialCR()
	due := kacTestClock.Add(720 * time.Hour)
	successorExpiry := &metav1.Time{Time: due.Add(744 * time.Hour)}
	h := newDeliveredKACHarness(t, c5c3v1alpha1.ManagementCluster, order, nil,
		kacCredentialChild(order, "", 2, "", successorExpiry, pendingImportConditions(0)),
		kacSecretChild(order, "", 2, "secret-of-ac-2"))
	_, err := h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())

	push := &esov1alpha1.PushSecret{}
	pushKey := types.NamespacedName{Namespace: "default", Name: keystoneApplicationCredentialPushSecretName(order, "")}
	g.Expect(h.mgmt.Get(ctx, pushKey, push)).To(Succeed())
	push.Annotations = kacPushed(order, kacControlPlane(""), "", "ac-2", "secret-of-ac-2").Annotations
	push.Status.SyncedResourceVersion = "2-pushed"
	g.Expect(h.mgmt.Update(ctx, push)).To(Succeed())
	kacMarkAvailable(t, h, 2, "ac-2")
	h.now = due

	result, err := h.reconcile(ctx)

	g.Expect(err).NotTo(HaveOccurred())
	got := kacGet(t, h)
	g.Expect(got.Status.CredentialID).To(Equal("ac-2"))
	secret, _ := kacDelivered(t, h)
	g.Expect(string(secret.Data[applicationCredentialIDKey])).To(Equal("ac-2"))
	g.Expect(string(secret.Data[applicationCredentialSecretKey])).To(Equal("secret-of-ac-2"))
	g.Expect(kacDeliveryReady(t, h).Status).To(Equal(metav1.ConditionTrue))
	g.Expect(result.RequeueAfter).To(Equal(24*time.Hour),
		"the grace period ends before the next rotation is due")
}

func TestKeystoneApplicationCredentialDelivery_ScheduleOffWaitsForAnEvent(t *testing.T) {
	g := NewGomegaWithT(t)

	order := keystoneApplicationCredentialCR()
	order.Spec.Rotation.Interval = &metav1.Duration{}
	h := newDeliveredKACHarness(t, c5c3v1alpha1.ManagementCluster, order, nil)

	result, err := h.reconcile(context.Background())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(kacDeliveryReady(t, h).Status).To(Equal(metav1.ConditionTrue))
	g.Expect(result.IsZero()).To(BeTrue(), "no timer runs with the schedule off")
}

func TestKeystoneApplicationCredentialDelivery_Refusals(t *testing.T) {
	t.Run("an unpublished Keystone on a target cluster", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ctx := context.Background()
		order := keystoneApplicationCredentialCR()
		h := newDeliveredKACHarness(t, kuTestCluster, order, nil)
		cp := keystoneUserControlPlane(assignOn(kuTestNamespace, kuTestCluster))
		live := &c5c3v1alpha1.ControlPlane{}
		g.Expect(h.mgmt.Get(ctx, client.ObjectKeyFromObject(cp), live)).To(Succeed())
		live.Spec.Services.Keystone.PublicEndpoint = ""
		g.Expect(h.mgmt.Update(ctx, live)).To(Succeed())
		g.Expect(h.order.Get(ctx, h.key, order)).To(Succeed())

		result, err := h.r.(*KeystoneApplicationCredentialReconciler).deliverCredential(ctx, h.order, order, live, kuTestCluster)

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(result.IsZero()).To(BeTrue())
		cond := kacCondition(order, conditionTypeKeystoneApplicationCredentialDeliveryReady)
		g.Expect(cond.Reason).To(Equal(reasonKeystoneUserKeystoneNotPublished))
		g.Expect(cond.Message).To(ContainSubstring(`target cluster "edge-1"`))
		_, found := kacDelivered(t, h)
		g.Expect(found).To(BeFalse())
	})

	t.Run("a Secret the order does not own", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ctx := context.Background()
		stranger := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "workflow-appcred-credentials", Namespace: kuTestNamespace},
			Data:       map[string][]byte{"token": []byte("theirs")},
		}
		h := newDeliveredKACHarness(t, c5c3v1alpha1.ManagementCluster, keystoneApplicationCredentialCR(), nil, stranger)

		_, err := h.reconcile(ctx)

		g.Expect(err).NotTo(HaveOccurred())
		cond := kacDeliveryReady(t, h)
		g.Expect(cond.Reason).To(Equal(reasonKeystoneUserDeliveryRefused))
		g.Expect(cond.Message).To(Equal("Secret tenant-a/workflow-appcred-credentials exists and is not owned by this order; " +
			"delete it or rename the order"))
		secret, _ := kacDelivered(t, h)
		g.Expect(secret.Data).To(Equal(map[string][]byte{"token": []byte("theirs")}))
	})

	t.Run("nothing minted yet", func(t *testing.T) {
		g := NewGomegaWithT(t)
		h := newKACHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil,
			append([]client.Object{keystoneApplicationCredentialCR(), kacControlPlane("")}, kacReadyReferences("")...)...)

		_, err := h.reconcile(context.Background())

		g.Expect(err).NotTo(HaveOccurred())
		cond := kacDeliveryReady(t, h)
		g.Expect(cond.Reason).To(Equal(reasonKeystoneApplicationCredentialWaiting))
		g.Expect(cond.Message).To(Equal("the credential is not minted yet; nothing is delivered"))
		_, found := kacDelivered(t, h)
		g.Expect(found).To(BeFalse())
	})

	t.Run("an apply error", func(t *testing.T) {
		g := NewGomegaWithT(t)
		h := newDeliveredKACHarness(t, kuTestCluster, keystoneApplicationCredentialCR(), &interceptor.Funcs{
			Apply: func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error {
				return errors.New("admission webhook denied the request")
			},
		})

		_, err := h.reconcile(context.Background())

		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("delivering Secret tenant-a/workflow-appcred-credentials: "))
		cond := kacDeliveryReady(t, h)
		g.Expect(cond.Reason).To(Equal(reasonKeystoneUserDeliveryError))
		g.Expect(cond.Message).To(ContainSubstring("admission webhook denied the request"))
	})
}

// TestKeystoneApplicationCredentialDelivery_RepairsTheSecret covers the edits
// an owner can make: an overwritten secret is rewritten, a deleted Secret is
// written again, and a key the operator does not own is left in place.
func TestKeystoneApplicationCredentialDelivery_RepairsTheSecret(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	h := newDeliveredKACHarness(t, c5c3v1alpha1.ManagementCluster, keystoneApplicationCredentialCR(), nil)
	_, err := h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())

	secret, _ := kacDelivered(t, h)
	secret.Data[applicationCredentialSecretKey] = []byte("foo")
	secret.Data["extra"] = []byte("mine")
	g.Expect(h.order.Update(ctx, secret)).To(Succeed())
	_, err = h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	secret, _ = kacDelivered(t, h)
	g.Expect(string(secret.Data[applicationCredentialSecretKey])).To(Equal("secret-of-ac-1"))
	g.Expect(string(secret.Data["extra"])).To(Equal("mine"), "a key the operator does not own stays")

	g.Expect(h.order.Delete(ctx, secret)).To(Succeed())
	_, err = h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	secret, found := kacDelivered(t, h)
	g.Expect(found).To(BeTrue(), "a deleted Secret is written again")
	g.Expect(string(secret.Data[applicationCredentialIDKey])).To(Equal("ac-1"))
}

// TestKeystoneApplicationCredentialDelivery_ErrorsAreReported fails each read
// and write of the backup: the error is returned wrapped and DeliveryReady
// reads DeliveryError.
func TestKeystoneApplicationCredentialDelivery_ErrorsAreReported(t *testing.T) {
	boom := errors.New("boom")
	order := keystoneApplicationCredentialCR()
	prefix := keystoneApplicationCredentialChildPrefix(order, "")
	// getFails fails a Get whose object, once read, fails matches.
	getFails := func(matches func(key client.ObjectKey, obj client.Object) bool) *interceptor.Funcs {
		return &interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if err := cl.Get(ctx, key, obj, opts...); err != nil {
					return err
				}
				if matches(key, obj) {
					return boom
				}
				return nil
			},
		}
	}
	cases := []struct {
		name    string
		funcs   *interceptor.Funcs
		wantErr string
	}{
		{
			name: "a credential secret read error",
			funcs: getFails(func(key client.ObjectKey, obj client.Object) bool {
				_, ok := obj.(*corev1.Secret)
				return ok && key.Name == keystoneApplicationCredentialSecretName(order, "", 1)
			}),
			wantErr: "reading the credential secret default/" + prefix + "secret-v1",
		},
		{
			name: "a source Secret write error",
			funcs: &interceptor.Funcs{
				Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					if obj.GetName() == prefix+"source" {
						return boom
					}
					return cl.Create(ctx, obj, opts...)
				},
			},
			wantErr: "assembling order source Secret default/" + prefix + "source",
		},
		{
			name: "a PushSecret stamp error",
			funcs: &interceptor.Funcs{
				Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					if _, ok := obj.(*esov1alpha1.PushSecret); ok {
						return boom
					}
					return cl.Update(ctx, obj, opts...)
				},
			},
			wantErr: "stamping order PushSecret default/" + prefix + "backup",
		},
		{
			// The stamped PushSecret is read once more for ESO's verdict.
			name: "a PushSecret read error",
			funcs: getFails(func(_ client.ObjectKey, obj client.Object) bool {
				ps, ok := obj.(*esov1alpha1.PushSecret)
				return ok && ps.Annotations[keystoneApplicationCredentialPushContentHashAnnotation] != ""
			}),
			wantErr: "reading order PushSecret default/" + prefix + "backup",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			order := keystoneApplicationCredentialCR()
			objs := append([]client.Object{order, kacControlPlane("")}, kacReadyReferences("")...)
			objs = append(objs, kacMinted(order, "", 1, "ac-1", kacTestClock)...)
			h := newKACHarness(t, c5c3v1alpha1.ManagementCluster, tc.funcs, nil, objs...)

			_, err := h.reconcile(context.Background())

			g.Expect(err).To(MatchError(boom))
			g.Expect(err.Error()).To(ContainSubstring(tc.wantErr))
			g.Expect(kacDeliveryReady(t, h).Reason).To(Equal(reasonKeystoneUserDeliveryError))
		})
	}
}
