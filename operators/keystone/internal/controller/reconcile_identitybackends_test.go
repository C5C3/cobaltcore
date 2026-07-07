// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	commonconditions "github.com/c5c3/forge/internal/common/conditions"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// testIdentityBackend returns a DomainReady LDAP backend attached to
// testKeystone(), with its bind Secret name pre-wired to
// testBindSecret's name.
func testIdentityBackend(name, domain string) *keystonev1alpha1.KeystoneIdentityBackend {
	return &keystonev1alpha1.KeystoneIdentityBackend{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  "default",
			UID:        types.UID("backend-uid-" + name),
			Generation: 1,
		},
		Spec: keystonev1alpha1.KeystoneIdentityBackendSpec{
			KeystoneRef: keystonev1alpha1.KeystoneRefSpec{Name: "test-keystone"},
			Domain: keystonev1alpha1.DomainSpec{
				Name:           domain,
				Mode:           keystonev1alpha1.DomainModeManage,
				DeletionPolicy: keystonev1alpha1.DomainDeletionPolicyRetain,
			},
			Type: keystonev1alpha1.IdentityBackendTypeLDAP,
			LDAP: &keystonev1alpha1.LDAPBackendSpec{
				URL:                      "ldap://ldap.example.com:389",
				BindCredentialsSecretRef: commonv1.SecretRefSpec{Name: name + "-bind"},
				Suffix:                   "dc=example,dc=com",
				Users:                    keystonev1alpha1.LDAPUserSpec{TreeDN: "ou=people,dc=example,dc=com"},
			},
		},
		Status: keystonev1alpha1.KeystoneIdentityBackendStatus{
			Conditions: []metav1.Condition{{
				Type:               conditionTypeDomainReady,
				Status:             metav1.ConditionTrue,
				Reason:             "DomainProvisioned",
				LastTransitionTime: metav1.Now(),
			}},
			DomainID: "domain-0001",
		},
	}
}

// testBindSecret returns the bind-credentials Secret for a backend built by
// testIdentityBackend.
func testBindSecret(backendName string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: backendName + "-bind", Namespace: "default"},
		Data: map[string][]byte{
			"username": []byte("cn=admin,dc=example,dc=com"),
			"password": []byte("bind-pw"),
		},
	}
}

func TestReconcileIdentityBackends_NotRequiredWhenNoBackends(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()
	r := newTestReconciler(ks)

	name, err := r.reconcileIdentityBackends(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(name).To(BeEmpty())

	cond := commonconditions.GetCondition(ks.Status.Conditions, conditionTypeIdentityBackendsReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonIdentityBackendsNotRequired))
}

func TestReconcileIdentityBackends_ProjectsReadyBackend(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()
	backend := testIdentityBackend("corp-ldap", "corp")
	r := newTestReconciler(ks, backend, testBindSecret("corp-ldap"))
	ctx := context.Background()

	name, err := r.reconcileIdentityBackends(ctx, ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(name).To(HavePrefix("test-keystone-domains-"))

	var secret corev1.Secret
	g.Expect(r.Client.Get(ctx, client.ObjectKey{Namespace: "default", Name: name}, &secret)).To(Succeed())
	g.Expect(secret.Data).To(HaveKey("keystone.corp.conf"))
	conf := string(secret.Data["keystone.corp.conf"])
	g.Expect(conf).To(ContainSubstring("[identity]\ndriver = ldap"))
	g.Expect(conf).To(ContainSubstring("url = ldap://ldap.example.com:389"))
	g.Expect(conf).To(ContainSubstring("user = cn=admin,dc=example,dc=com"))
	g.Expect(conf).To(ContainSubstring("password = bind-pw"))
	g.Expect(conf).To(ContainSubstring("user_tree_dn = ou=people,dc=example,dc=com"))
	// ReadOnly defaults to true (nil pointer): write options forced off.
	g.Expect(conf).To(ContainSubstring("user_allow_create = false"))
	g.Expect(conf).To(ContainSubstring("group_allow_delete = false"))
	// Unset optional attributes are omitted so keystone defaults apply.
	g.Expect(conf).NotTo(ContainSubstring("user_id_attribute"))
	g.Expect(conf).NotTo(ContainSubstring("group_tree_dn"))

	cond := commonconditions.GetCondition(ks.Status.Conditions, conditionTypeIdentityBackendsReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonAllBackendsProjected))

	// Deterministic naming: a second pass produces the same content hash.
	name2, err := r.reconcileIdentityBackends(ctx, ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(name2).To(Equal(name))
}

func TestReconcileIdentityBackends_RendersOptionalFieldsAndExtraOptions(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()
	backend := testIdentityBackend("corp-ldap", "corp")
	backend.Spec.LDAP.ReadOnly = ptr.To(false)
	backend.Spec.LDAP.Users.ObjectClass = "inetOrgPerson"
	backend.Spec.LDAP.Users.IDAttribute = "uid"
	backend.Spec.LDAP.Groups = &keystonev1alpha1.LDAPGroupSpec{
		TreeDN:          "ou=groups,dc=example,dc=com",
		MemberAttribute: "member",
	}
	backend.Spec.LDAP.Pool = &keystonev1alpha1.LDAPPoolSpec{Enabled: true, Size: ptr.To(int32(10))}
	backend.Spec.LDAP.TLS = &keystonev1alpha1.LDAPTLSSpec{
		CABundleSecretRef: commonv1.SecretRefSpec{Name: "corp-ldap-ca"},
	}
	backend.Spec.ExtraOptions = map[string]string{"page_size": "100"}

	caSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "corp-ldap-ca", Namespace: "default"},
		Data:       map[string][]byte{"ca.crt": []byte("PEMDATA")},
	}
	r := newTestReconciler(ks, backend, testBindSecret("corp-ldap"), caSecret)
	ctx := context.Background()

	name, err := r.reconcileIdentityBackends(ctx, ks)
	g.Expect(err).NotTo(HaveOccurred())

	var secret corev1.Secret
	g.Expect(r.Client.Get(ctx, client.ObjectKey{Namespace: "default", Name: name}, &secret)).To(Succeed())
	conf := string(secret.Data["keystone.corp.conf"])
	g.Expect(conf).To(ContainSubstring("user_objectclass = inetOrgPerson"))
	g.Expect(conf).To(ContainSubstring("user_id_attribute = uid"))
	g.Expect(conf).To(ContainSubstring("group_tree_dn = ou=groups,dc=example,dc=com"))
	g.Expect(conf).To(ContainSubstring("group_member_attribute = member"))
	g.Expect(conf).To(ContainSubstring("use_pool = true"))
	g.Expect(conf).To(ContainSubstring("pool_size = 10"))
	g.Expect(conf).To(ContainSubstring("page_size = 100"))
	g.Expect(conf).To(ContainSubstring("tls_cacertfile = /etc/keystone/domains/corp-ca.pem"))
	// The CA PEM is projected as a sibling key.
	g.Expect(secret.Data).To(HaveKeyWithValue("corp-ca.pem", []byte("PEMDATA")))
	// readOnly: false leaves the write-enabling options unset.
	g.Expect(conf).NotTo(ContainSubstring("user_allow_create"))
}

func TestReconcileIdentityBackends_SkipsNotDomainReadyBackend(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()
	backend := testIdentityBackend("corp-ldap", "corp")
	backend.Status.Conditions = []metav1.Condition{{
		Type:               conditionTypeDomainReady,
		Status:             metav1.ConditionFalse,
		Reason:             "WaitingForKeystoneAPI",
		LastTransitionTime: metav1.Now(),
	}}
	r := newTestReconciler(ks, backend, testBindSecret("corp-ldap"))

	name, err := r.reconcileIdentityBackends(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred(), "a pending domain must never fail or requeue the pipeline")
	g.Expect(name).To(BeEmpty())

	cond := commonconditions.GetCondition(ks.Status.Conditions, conditionTypeIdentityBackendsReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForBackends))
	g.Expect(cond.Message).To(ContainSubstring("corp-ldap"))
}

func TestReconcileIdentityBackends_SkipsDeletingBackend(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()
	backend := testIdentityBackend("corp-ldap", "corp")
	now := metav1.Now()
	backend.DeletionTimestamp = &now
	backend.Finalizers = []string{identityBackendFinalizerName}
	r := newTestReconciler(ks, backend, testBindSecret("corp-ldap"))

	name, err := r.reconcileIdentityBackends(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(name).To(BeEmpty(), "a deleting backend must be de-projected immediately")

	// De-projection is not a waiting state: with the deleting backend
	// excluded, nothing is pending.
	cond := commonconditions.GetCondition(ks.Status.Conditions, conditionTypeIdentityBackendsReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonAllBackendsProjected))
}

func TestReconcileIdentityBackends_MissingBindSecretSkipsAndWarns(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()
	healthy := testIdentityBackend("corp-ldap", "corp")
	broken := testIdentityBackend("broken-ldap", "brokendomain")
	// broken's bind Secret is deliberately absent.
	r := newTestReconciler(ks, healthy, broken, testBindSecret("corp-ldap"))
	ctx := context.Background()

	name, err := r.reconcileIdentityBackends(ctx, ks)
	g.Expect(err).NotTo(HaveOccurred(), "a missing bind Secret must never fail the pipeline")
	g.Expect(name).NotTo(BeEmpty(), "healthy siblings keep being projected")

	var secret corev1.Secret
	g.Expect(r.Client.Get(ctx, client.ObjectKey{Namespace: "default", Name: name}, &secret)).To(Succeed())
	g.Expect(secret.Data).To(HaveKey("keystone.corp.conf"))
	g.Expect(secret.Data).NotTo(HaveKey("keystone.brokendomain.conf"))

	cond := commonconditions.GetCondition(ks.Status.Conditions, conditionTypeIdentityBackendsReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForBackends))
	expectEvent(g, r, "Warning IdentityBackendSkipped")
}

// A bind Secret whose password (or username) carries a newline is an INI
// injection the webhook cannot catch — it never reads Secrets. The renderer is
// the last line of defense: it must refuse to emit the corrupted config,
// skipping+warning like a missing Secret while healthy siblings keep projecting.
func TestReconcileIdentityBackends_ControlCharInBindSecretSkipsAndWarns(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()
	healthy := testIdentityBackend("corp-ldap", "corp")
	poisoned := testIdentityBackend("evil-ldap", "evildomain")
	poisonedSecret := testBindSecret("evil-ldap")
	// A newline in the bind password would render an extra [ldap] line,
	// re-enabling the write options readOnly forces off.
	poisonedSecret.Data["password"] = []byte("pw\nuser_allow_create = true")
	r := newTestReconciler(ks, healthy, poisoned, testBindSecret("corp-ldap"), poisonedSecret)
	ctx := context.Background()

	name, err := r.reconcileIdentityBackends(ctx, ks)
	g.Expect(err).NotTo(HaveOccurred(), "a control-char value must never fail the pipeline")
	g.Expect(name).NotTo(BeEmpty(), "healthy siblings keep being projected")

	var secret corev1.Secret
	g.Expect(r.Get(ctx, client.ObjectKey{Namespace: "default", Name: name}, &secret)).To(Succeed())
	g.Expect(secret.Data).To(HaveKey("keystone.corp.conf"))
	g.Expect(secret.Data).NotTo(HaveKey("keystone.evildomain.conf"), "the poisoned backend must not be projected")
	// The forced read-only option is never overridden in the healthy config.
	g.Expect(string(secret.Data["keystone.corp.conf"])).To(ContainSubstring("user_allow_create = false"))

	cond := commonconditions.GetCondition(ks.Status.Conditions, conditionTypeIdentityBackendsReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForBackends))
	expectEvent(g, r, "Warning IdentityBackendSkipped")
}

// A CRD-bypass backend (direct etcd write / disabled webhook) can carry a
// control character in an extraOptions KEY, not just a value — RenderINI writes
// `key = value` verbatim, so a newline in the key injects an arbitrary [ldap]
// line the same way. The renderer backstop must refuse to emit the corrupted
// config, skipping+warning while healthy siblings keep projecting.
func TestReconcileIdentityBackends_ControlCharInExtraOptionKeySkipsAndWarns(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()
	healthy := testIdentityBackend("corp-ldap", "corp")
	poisoned := testIdentityBackend("evil-ldap", "evildomain")
	// A newline in the key renders an extra [ldap] line, re-enabling the write
	// options readOnly forces off.
	poisoned.Spec.ExtraOptions = map[string]string{"zzz_pwn\nuser_allow_create = true": "x"}
	r := newTestReconciler(ks, healthy, poisoned, testBindSecret("corp-ldap"), testBindSecret("evil-ldap"))
	ctx := context.Background()

	name, err := r.reconcileIdentityBackends(ctx, ks)
	g.Expect(err).NotTo(HaveOccurred(), "a control-char key must never fail the pipeline")
	g.Expect(name).NotTo(BeEmpty(), "healthy siblings keep being projected")

	var secret corev1.Secret
	g.Expect(r.Get(ctx, client.ObjectKey{Namespace: "default", Name: name}, &secret)).To(Succeed())
	g.Expect(secret.Data).To(HaveKey("keystone.corp.conf"))
	g.Expect(secret.Data).NotTo(HaveKey("keystone.evildomain.conf"), "the poisoned backend must not be projected")
	// The forced read-only option is never overridden in the healthy config.
	g.Expect(string(secret.Data["keystone.corp.conf"])).To(ContainSubstring("user_allow_create = false"))

	cond := commonconditions.GetCondition(ks.Status.Conditions, conditionTypeIdentityBackendsReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForBackends))
	expectEvent(g, r, "Warning IdentityBackendSkipped")
}

func TestReconcileIdentityBackends_DuplicateDomainSkipsCollidingSet(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()
	a := testIdentityBackend("a-ldap", "corp")
	// Case-insensitive collision that bypassed the webhook.
	b := testIdentityBackend("b-ldap", "Corp")
	r := newTestReconciler(ks, a, b, testBindSecret("a-ldap"), testBindSecret("b-ldap"))

	name, err := r.reconcileIdentityBackends(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(name).To(BeEmpty(), "NONE of a colliding set may be projected")

	cond := commonconditions.GetCondition(ks.Status.Conditions, conditionTypeIdentityBackendsReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForBackends))
	g.Expect(cond.Message).To(ContainSubstring("duplicate domain"))
}

// Backends attached to a DIFFERENT Keystone in the same namespace are
// invisible via the keystoneRef index.
func TestReconcileIdentityBackends_IgnoresBackendsOfOtherKeystones(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()
	other := testIdentityBackend("other-ldap", "corp")
	other.Spec.KeystoneRef.Name = "another-keystone"
	r := newTestReconciler(ks, other, testBindSecret("other-ldap"))

	name, err := r.reconcileIdentityBackends(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(name).To(BeEmpty())

	cond := commonconditions.GetCondition(ks.Status.Conditions, conditionTypeIdentityBackendsReady)
	g.Expect(cond.Reason).To(Equal(conditionReasonIdentityBackendsNotRequired))
}

// pruneStaleDomainsSecrets with an empty current name removes every
// historical domains Secret — the last backend detached, so no bind password
// may linger.
func TestPruneStaleDomainsSecrets_FullCleanupWhenNothingProjected(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()
	backend := testIdentityBackend("corp-ldap", "corp")
	r := newTestReconciler(ks, backend, testBindSecret("corp-ldap"))
	ctx := context.Background()

	name, err := r.reconcileIdentityBackends(ctx, ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(name).NotTo(BeEmpty())

	// Backend detaches: nothing projected anymore, prune with empty current.
	g.Expect(r.Client.Delete(ctx, backend)).To(Succeed())
	g.Expect(r.pruneStaleDomainsSecrets(ctx, ks, "")).To(Succeed())

	var secret corev1.Secret
	err = r.Client.Get(ctx, client.ObjectKey{Namespace: "default", Name: name}, &secret)
	g.Expect(err).To(HaveOccurred(), "the stale domains Secret must be pruned when nothing is projected")
}

// The config render must flip the [identity] domain options with the
// projection state — and the render cache must not serve a stale entry across
// the flip (attach/detach changes no Keystone generation).
func TestReconcileConfig_DomainFlagsFollowProjectionState(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()
	r := newTestReconciler(ks, testDBCredentialsSecret())
	ctx := context.Background()

	nameOff, err := r.reconcileConfig(ctx, ks, false)
	g.Expect(err).NotTo(HaveOccurred())
	var cmOff corev1.ConfigMap
	g.Expect(r.Client.Get(ctx, client.ObjectKey{Namespace: "default", Name: nameOff}, &cmOff)).To(Succeed())
	g.Expect(cmOff.Data["keystone.conf"]).NotTo(ContainSubstring("domain_specific_drivers_enabled"))

	// Projection turned on at the same generation: the cache must miss and
	// the re-render must carry the domain options.
	nameOn, err := r.reconcileConfig(ctx, ks, true)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(nameOn).NotTo(Equal(nameOff))
	var cmOn corev1.ConfigMap
	g.Expect(r.Client.Get(ctx, client.ObjectKey{Namespace: "default", Name: nameOn}, &cmOn)).To(Succeed())
	g.Expect(cmOn.Data["keystone.conf"]).To(ContainSubstring("domain_specific_drivers_enabled = true"))
	g.Expect(cmOn.Data["keystone.conf"]).To(ContainSubstring("domain_config_dir = /etc/keystone/domains"))

	// User extraConfig still wins over the projected defaults.
	ks2 := testKeystone()
	ks2.Name = "test-keystone-extra"
	ks2.UID = "ks-uid-extra"
	ks2.Spec.ExtraConfig = map[string]map[string]string{
		"identity": {"domain_specific_drivers_enabled": "false"},
	}
	r2 := newTestReconciler(ks2, testDBCredentialsSecret())
	nameExtra, err := r2.reconcileConfig(ctx, ks2, true)
	g.Expect(err).NotTo(HaveOccurred())
	var cmExtra corev1.ConfigMap
	g.Expect(r2.Client.Get(ctx, client.ObjectKey{Namespace: "default", Name: nameExtra}, &cmExtra)).To(Succeed())
	g.Expect(cmExtra.Data["keystone.conf"]).To(ContainSubstring("domain_specific_drivers_enabled = false"))
}

// The IdentityBackends pipeline step must never short-circuit the chain for a
// waiting backend: the full Reconcile still creates the Deployment so
// first-install can bring the API up.
func TestReconcile_PendingIdentityBackendDoesNotBlockDeployment(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()
	backend := testIdentityBackend("corp-ldap", "corp")
	backend.Status.Conditions = nil // freshly created: DomainReady not set yet
	configMapName := testComputeConfigMapName(t)
	objs := append([]runtime.Object{
		ks, backend, testBindSecret("corp-ldap"),
		testCompletedDBSyncJob(configMapName), testCompletedSchemaCheckJob(configMapName),
		testCompletedBootstrapJob(configMapName), testDBCredentialsSecret(),
		testAdminCredentialsSecret(), testReadyKeystoneDeployment(),
		testFernetKeysSecret(), testCredentialKeysSecret(),
	}, testReadyExternalSecrets()...)
	r := newTestReconciler(objs...)
	r.HTTPClient = testHealthyHTTPClient()

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: client.ObjectKeyFromObject(ks),
	})
	g.Expect(err).NotTo(HaveOccurred())

	var updated keystonev1alpha1.Keystone
	g.Expect(r.Client.Get(context.Background(), client.ObjectKeyFromObject(ks), &updated)).To(Succeed())
	ib := commonconditions.GetCondition(updated.Status.Conditions, conditionTypeIdentityBackendsReady)
	g.Expect(ib).NotTo(BeNil())
	g.Expect(ib.Status).To(Equal(metav1.ConditionFalse))
	// The pipeline ran past IdentityBackends: DeploymentReady was evaluated.
	g.Expect(commonconditions.GetCondition(updated.Status.Conditions, "DeploymentReady")).NotTo(BeNil())
	// The aggregate Ready is gated on the pending backend.
	ready := commonconditions.GetCondition(updated.Status.Conditions, "Ready")
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
}
