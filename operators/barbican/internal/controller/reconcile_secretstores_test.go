// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/c5c3/cobaltcore/internal/common/secrets"
	barbicanv1alpha1 "github.com/c5c3/cobaltcore/operators/barbican/api/v1alpha1"
)

// managedStoreNamed returns a managed store of the given name attached to the
// shared Barbican fixture.
func managedStoreNamed(name string, isDefault bool) *barbicanv1alpha1.BarbicanSecretStore {
	store := testManagedStore()
	store.Name = name
	store.UID = types.UID(name + "-uid")
	store.Spec.IsDefault = isDefault
	return store
}

// brownfieldStoreNamed returns a brownfield store of the given name pointing at
// the given server URL.
func brownfieldStoreNamed(name, url string, isDefault bool) *barbicanv1alpha1.BarbicanSecretStore {
	store := testBrownfieldStore()
	store.Name = name
	store.UID = types.UID(name + "-uid")
	store.Spec.IsDefault = isDefault
	store.Spec.OpenBao.Server.URL = url
	return store
}

// projectStores runs the secret-store step over the shared Barbican fixture with
// the given cluster objects seeded.
func projectStores(t *testing.T, objs ...client.Object) (*BarbicanReconciler, *barbicanv1alpha1.Barbican, secretStoreProjection, error) {
	t.Helper()
	barbican := testBarbican()
	r := newBarbicanTestReconciler(append([]client.Object{barbican}, objs...)...)
	res, projection, err := r.reconcileSecretStores(context.Background(), r.Client, barbican)
	if !res.IsZero() {
		t.Fatalf("the secret-store step must never requeue, got %v", res)
	}
	return r, barbican, projection, err
}

// TestSecretStoreListReadsManagementCluster proves the attached-stores list
// stays on the embedded client while the credentials it names are read on the
// children client. The children fake is built without the barbican API group,
// so a misrouted List fails with "no kind is registered" instead of quietly
// returning an empty set that would look like "no store attached".
func TestSecretStoreListReadsManagementCluster(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := testBarbican()
	store := readyStore(managedStoreNamed(testStoreName, true))
	// Management: the CR and its sibling store, with the field index the list
	// selects on (SetupWithManager registers the same extractor).
	r := newBarbicanTestReconciler(barbican, store)

	childrenScheme := runtime.NewScheme()
	g.Expect(clientgoscheme.AddToScheme(childrenScheme)).To(Succeed())
	children := fake.NewClientBuilder().
		WithScheme(childrenScheme).
		WithObjects(managedApproleSecret(testStoreName, testRoleID, testSecretID)).
		Build()

	_, projection, err := r.reconcileSecretStores(context.Background(), children, barbican)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(projection.valid).To(BeTrue(), "the sibling store was found beside the CR")
	g.Expect(projection.defaultStore).To(Equal(testStoreName))
	// The AppRole credentials the store named resolved from the children
	// cluster, which is the only place they exist in this test.
	g.Expect(projection.sections["vault_plugin"]).To(HaveKeyWithValue("approle_role_id", testRoleID))
	g.Expect(projection.secretIDDigest).NotTo(BeEmpty())
}

func TestReconcileSecretStores_NoStoresAttached(t *testing.T) {
	g := NewGomegaWithT(t)
	_, barbican, projection, err := projectStores(t)

	g.Expect(err).NotTo(HaveOccurred(), "an unattached Barbican is a state to report, not a failure")
	g.Expect(projection.valid).To(BeFalse())
	g.Expect(projection.sections).To(BeEmpty())

	cond := barbicanCondition(barbican, conditionTypeSecretStoresReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonNoDefaultSecretStore))
}

// TestReconcileSecretStores_PendingStoreContributesNoHost pins the D-gate on the
// egress set as well as on the rendered sections: a store whose credentials are
// not ready yet opens no port on the API pods. It is the operator, not the API
// pods, that authenticates a store, so nothing deadlocks — and without the gate
// anyone able to create a BarbicanSecretStore could add a
// destination-unrestricted egress port to a Barbican they cannot edit, just by
// naming a server URL.
func TestReconcileSecretStores_PendingStoreContributesNoHost(t *testing.T) {
	g := NewGomegaWithT(t)
	pending := managedStoreNamed("pending", true) // no CredentialsReady condition
	_, barbican, projection, err := projectStores(t, pending)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(projection.valid).To(BeFalse())
	g.Expect(projection.hosts).To(BeEmpty())

	cond := barbicanCondition(barbican, conditionTypeSecretStoresReady)
	g.Expect(cond.Reason).To(Equal(conditionReasonNoDefaultSecretStore))
}

// TestReconcileSecretStores_DeletingStoreIsDeProjected covers the other end of
// the store lifecycle: a deleting store leaves both the rendered sections and
// the egress set on the same pass.
func TestReconcileSecretStores_DeletingStoreIsDeProjected(t *testing.T) {
	g := NewGomegaWithT(t)
	deleting := readyStore(managedStoreNamed("leaving", true))
	deleting.DeletionTimestamp = &metav1.Time{Time: metav1.Now().Time}
	deleting.Finalizers = []string{"test.c5c3.io/hold"} // the fake client requires one to keep it
	_, barbican, projection, err := projectStores(t, deleting, managedApproleSecret("leaving", "role", "secret"))

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(projection.valid).To(BeFalse())
	g.Expect(projection.hosts).To(BeEmpty())
	g.Expect(barbicanCondition(barbican, conditionTypeSecretStoresReady).Reason).To(Equal(conditionReasonNoDefaultSecretStore))
}

// TestReconcileSecretStores_DefaultResolution covers both violations of the
// exactly-one-default rule. Neither is an error and neither requeues: the store
// watch wakes the parent when a store flips isDefault, and the config step keeps
// last-good in the meantime.
func TestReconcileSecretStores_DefaultResolution(t *testing.T) {
	tests := []struct {
		name   string
		stores []client.Object
	}{
		{
			name:   "no ready store is marked default",
			stores: []client.Object{readyStore(managedStoreNamed("alpha", false))},
		},
		{
			name: "two ready stores are marked default",
			stores: []client.Object{
				readyStore(managedStoreNamed("alpha", true)),
				readyStore(managedStoreNamed("beta", true)),
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			objs := make([]client.Object, 0, len(tc.stores)+2)
			objs = append(objs, tc.stores...)
			objs = append(objs, managedApproleSecret("alpha", "role", "secret"), managedApproleSecret("beta", "role", "secret"))
			_, barbican, projection, err := projectStores(t, objs...)

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(projection.valid).To(BeFalse())
			g.Expect(projection.sections).To(BeEmpty(), "an invalid projection renders nothing, so the config step keeps last-good")

			cond := barbicanCondition(barbican, conditionTypeSecretStoresReady)
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(conditionReasonNoDefaultSecretStore))
		})
	}
}

// TestReconcileSecretStores_MultipleOpenBaoStores is the fail-closed backstop
// for a CR that bypassed the validating webhook: [vault_plugin] is
// process-global, so a second ready OpenBao store would replace the first one's
// server and credentials rather than being configured beside it.
func TestReconcileSecretStores_MultipleOpenBaoStores(t *testing.T) {
	g := NewGomegaWithT(t)
	_, barbican, projection, err := projectStores(t,
		readyStore(managedStoreNamed("alpha", true)),
		readyStore(managedStoreNamed("beta", false)),
		managedApproleSecret("alpha", "role", "secret"),
		managedApproleSecret("beta", "role", "secret"))

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(projection.valid).To(BeFalse())
	g.Expect(projection.sections).To(BeEmpty())

	cond := barbicanCondition(barbican, conditionTypeSecretStoresReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonMultipleOpenBaoStores))
	g.Expect(cond.Message).To(ContainSubstring("2 attached"))
}

// TestReconcileSecretStores_ManagedRendersSections asserts the rendered sections
// of a managed store exactly, including the two things that must never appear in
// the file: approle_secret_id, and a global_default on a non-default store.
func TestReconcileSecretStores_ManagedRendersSections(t *testing.T) {
	g := NewGomegaWithT(t)
	_, barbican, projection, err := projectStores(t,
		readyStore(managedStoreNamed("primary", true)),
		managedApproleSecret("primary", "role-id-value", "secret-id-value"))

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(projection.valid).To(BeTrue())
	g.Expect(projection.defaultStore).To(Equal("primary"))

	g.Expect(projection.sections["secretstore"]).To(Equal(map[string]string{
		"enable_multiple_secret_stores": "true",
		"stores_lookup_suffix":          "primary",
	}))
	g.Expect(projection.sections["secretstore:primary"]).To(Equal(map[string]string{
		"secret_store_plugin": "vault_plugin",
		"global_default":      "True",
	}))
	g.Expect(projection.sections["vault_plugin"]).To(Equal(map[string]string{
		"vault_url":       instanceURL(testInstanceName, testNamespace),
		"use_ssl":         "true",
		"ssl_ca_crt_file": secretStoreCAFilePath,
		"approle_role_id": "role-id-value",
		"kv_mountpoint":   "barbican",
	}))
	// The AppRole secret ID travels only as OS_VAULT_PLUGIN__APPROLE_SECRET_ID.
	for name, section := range projection.sections {
		g.Expect(section).NotTo(HaveKey("approle_secret_id"), "section %s", name)
	}

	// The pod-side wiring the deployment step resolves.
	g.Expect(projection.credentialsSecretName).To(Equal("primary" + approleSecretNameSuffix))
	g.Expect(projection.caSecretName).To(Equal(testInstanceName + instanceCASecretSuffix))
	g.Expect(projection.secretIDDigest).To(Equal(secrets.AdminPasswordDigest("secret-id-value")),
		"the digest is what rolls the pods when the store controller re-mints")

	cond := barbicanCondition(barbican, conditionTypeSecretStoresReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonAllStoresProjected))
}

// TestReconcileSecretStores_GlobalDefaultOnlyOnDefault covers the multi-store
// render shape without tripping the single-OpenBao-store gate: the second store
// is attached but not credential-ready, so only the default is rendered while
// the suffix list stays in sorted order.
func TestReconcileSecretStores_GlobalDefaultOnlyOnDefault(t *testing.T) {
	g := NewGomegaWithT(t)
	_, _, projection, err := projectStores(t,
		readyStore(managedStoreNamed("alpha", true)),
		managedStoreNamed("zulu", false), // attached, not credential-ready
		managedApproleSecret("alpha", "role", "secret"))

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(projection.valid).To(BeTrue())
	g.Expect(projection.sections["secretstore"]["stores_lookup_suffix"]).To(Equal("alpha"))
	g.Expect(projection.sections["secretstore:alpha"]).To(HaveKeyWithValue("global_default", "True"))
	g.Expect(projection.sections).NotTo(HaveKey("secretstore:zulu"))
	g.Expect(projection.hosts).To(HaveLen(1), "only the credential-ready store contributes an egress host")
}

// TestReconcileSecretStores_BrownfieldVariants covers the three plugin options
// that differ per mode: the URL, the TLS flag derived from its scheme, and
// whether a CA file is referenced at all.
func TestReconcileSecretStores_BrownfieldVariants(t *testing.T) {
	tests := []struct {
		name          string
		url           string
		caBundle      string
		namespaceOpt  string
		wantUseSSL    string
		wantCAFile    bool
		wantNamespace bool
	}{
		{name: "https without a CA bundle", url: "https://bao.example.com:8200", wantUseSSL: "true"},
		{name: "https with a CA bundle", url: "https://bao.example.com:8200", caBundle: "bao-ca", wantUseSSL: "true", wantCAFile: true},
		{name: "plain http", url: "http://bao.example.com:8200", wantUseSSL: "false"},
		{name: "scoped to a server namespace", url: "https://bao.example.com:8200", namespaceOpt: "tenant-a", wantUseSSL: "true", wantNamespace: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			store := readyStore(brownfieldStoreNamed("external", tc.url, true))
			store.Spec.OpenBao.KVMountpoint = "kv"
			store.Spec.OpenBao.Namespace = tc.namespaceOpt
			if tc.caBundle != "" {
				store.Spec.OpenBao.Server.CABundleSecretRef = &barbicanv1alpha1.SecretNameRefSpec{Name: tc.caBundle}
			}
			creds := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "brownfield-approle", Namespace: testNamespace},
				Data: map[string][]byte{
					barbicanv1alpha1.OpenBaoRoleIDKey:   []byte("brownfield-role"),
					barbicanv1alpha1.OpenBaoSecretIDKey: []byte("brownfield-secret"),
				},
			}
			_, _, projection, err := projectStores(t, store, creds)

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(projection.valid).To(BeTrue())
			plugin := projection.sections["vault_plugin"]
			g.Expect(plugin).To(HaveKeyWithValue("vault_url", tc.url))
			g.Expect(plugin).To(HaveKeyWithValue("use_ssl", tc.wantUseSSL))
			g.Expect(plugin).To(HaveKeyWithValue("kv_mountpoint", "kv"))
			g.Expect(plugin).To(HaveKeyWithValue("approle_role_id", "brownfield-role"))
			if tc.wantCAFile {
				g.Expect(plugin).To(HaveKeyWithValue("ssl_ca_crt_file", secretStoreCAFilePath))
				g.Expect(projection.caSecretName).To(Equal(tc.caBundle))
			} else {
				g.Expect(plugin).NotTo(HaveKey("ssl_ca_crt_file"))
				g.Expect(projection.caSecretName).To(BeEmpty())
			}
			if tc.wantNamespace {
				g.Expect(plugin).To(HaveKeyWithValue("namespace", tc.namespaceOpt))
			} else {
				g.Expect(plugin).NotTo(HaveKey("namespace"))
			}
			g.Expect(projection.credentialsSecretName).To(Equal("brownfield-approle"))
			g.Expect(projection.hosts).To(ConsistOf(tc.url))
		})
	}
}

// Deleting a store detaches the secret material written through it: barbican
// resolves each stored secret through the store it was written to, so once the
// [secretstore:<name>] section is gone those secrets stop resolving. The store
// CR carries no finalizer and its deletion is not validated, and the parent
// re-renders as soon as the remaining stores form a valid projection — which is
// exactly the moment the operator believes the swap worked. The recorded set is
// what makes the drop observable at that pass.
func TestReconcileSecretStores_DroppedStoreWarnsAndUpdatesTheRecord(t *testing.T) {
	g := NewGomegaWithT(t)

	barbican := testBarbican()
	barbican.Status.ProjectedSecretStores = []string{"store-a"}
	r := newBarbicanTestReconciler(barbican,
		readyStore(managedStoreNamed("store-b", true)),
		managedApproleSecret("store-b", "role-id-value", "secret-id-value"))

	_, projection, err := r.reconcileSecretStores(context.Background(), r.Client, barbican)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(projection.valid).To(BeTrue())
	g.Expect(barbican.Status.ProjectedSecretStores).To(Equal([]string{"store-b"}))
	g.Expect(barbican.Status.ProjectedSecretStoreHosts).To(Equal([]string{instanceURL(testInstanceName, testNamespace)}),
		"the host record is what a later invalid pass widens its egress set from, so it moves with the names")

	events := collectEvents(r.Recorder.(*record.FakeRecorder))
	g.Expect(events).To(ContainElement(And(
		ContainSubstring("SecretStoreDetached"),
		ContainSubstring("store-a"),
	)))
}

// The steady state must stay quiet: an unchanged store set records the same
// names again and emits nothing.
func TestReconcileSecretStores_UnchangedStoreSetIsSilent(t *testing.T) {
	g := NewGomegaWithT(t)

	barbican := testBarbican()
	barbican.Status.ProjectedSecretStores = []string{"primary"}
	r := newBarbicanTestReconciler(barbican,
		readyStore(managedStoreNamed("primary", true)),
		managedApproleSecret("primary", "role-id-value", "secret-id-value"))

	_, projection, err := r.reconcileSecretStores(context.Background(), r.Client, barbican)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(projection.valid).To(BeTrue())
	g.Expect(barbican.Status.ProjectedSecretStores).To(Equal([]string{"primary"}))

	for _, event := range collectEvents(r.Recorder.(*record.FakeRecorder)) {
		g.Expect(event).NotTo(ContainSubstring("SecretStoreDetached"))
	}
}

// TestReconcileSecretStores_ExtraOptions covers the escape hatch in all three
// shapes: an operator key it must not win, a free option it may add, and a value
// carrying an INI-injecting control character.
func TestReconcileSecretStores_ExtraOptions(t *testing.T) {
	tests := []struct {
		name         string
		extraOptions map[string]string
		wantPending  bool
		assert       func(g *WithT, plugin map[string]string)
	}{
		{
			name:         "empty map merges nothing",
			extraOptions: map[string]string{},
			assert: func(g *WithT, plugin map[string]string) {
				g.Expect(plugin).To(HaveLen(5))
			},
		},
		{
			name:         "a free option is merged",
			extraOptions: map[string]string{"max_retries": "5"},
			assert: func(g *WithT, plugin map[string]string) {
				g.Expect(plugin).To(HaveKeyWithValue("max_retries", "5"))
			},
		},
		// The four denied options the operator never writes itself, so the
		// "did I already write this key" check cannot see them. root_token_id is
		// the one that matters: barbican's vault plugin prefers a root token over
		// AppRole auth, so it would defeat the mount-scoped AppRole — and with it
		// the kv_mountpoint confinement — on any CR that reached etcd without the
		// validating webhook.
		{
			name: "a denied option the operator never writes is dropped",
			extraOptions: map[string]string{
				"root_token_id":     "hvs.rootroot",
				"approle_secret_id": "leaked",
				"crypto_plugin":     "simple_crypto",
				"ssl_ca_crt_file":   "/tmp/attacker-ca.pem",
			},
			assert: func(g *WithT, plugin map[string]string) {
				g.Expect(plugin).NotTo(HaveKey("root_token_id"))
				g.Expect(plugin).NotTo(HaveKey("approle_secret_id"))
				g.Expect(plugin).NotTo(HaveKey("crypto_plugin"))
				g.Expect(plugin).To(HaveKeyWithValue("ssl_ca_crt_file", secretStoreCAFilePath))
			},
		},
		{
			name:         "an operator key is not overridden",
			extraOptions: map[string]string{"kv_mountpoint": "hijacked", "approle_role_id": "hijacked"},
			assert: func(g *WithT, plugin map[string]string) {
				g.Expect(plugin).To(HaveKeyWithValue("kv_mountpoint", "barbican"))
				g.Expect(plugin).To(HaveKeyWithValue("approle_role_id", "role-id-value"))
			},
		},
		{
			name:         "a control character in a value is rejected",
			extraOptions: map[string]string{"max_retries": "5\napprole_secret_id = leaked"},
			wantPending:  true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			store := readyStore(managedStoreNamed("primary", true))
			store.Spec.ExtraOptions = tc.extraOptions
			_, barbican, projection, err := projectStores(t, store,
				managedApproleSecret("primary", "role-id-value", "secret-id-value"))

			g.Expect(err).NotTo(HaveOccurred(), "a poisoned option is a state to report, not a reconcile failure")
			if tc.wantPending {
				g.Expect(projection.valid).To(BeFalse())
				g.Expect(barbicanCondition(barbican, conditionTypeSecretStoresReady).Reason).
					To(Equal(conditionReasonWaitingForSecretStores))
				return
			}
			g.Expect(projection.valid).To(BeTrue())
			tc.assert(g, projection.sections["vault_plugin"])
		})
	}
}

// TestReconcileSecretStores_CredentialsSecretMissing covers the window between
// the store controller flipping CredentialsReady and the credentials Secret
// being readable through the parent's cache. The parent tolerates it: last-good
// is retained, no error reaches the workqueue.
func TestReconcileSecretStores_CredentialsSecretMissing(t *testing.T) {
	g := NewGomegaWithT(t)
	r, barbican, projection, err := projectStores(t, readyStore(managedStoreNamed("primary", true)))

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(projection.valid).To(BeFalse())
	g.Expect(projection.hosts).To(HaveLen(1))

	cond := barbicanCondition(barbican, conditionTypeSecretStoresReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForSecretStores))
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).To(
		ContainElement(ContainSubstring("BarbicanSecretStoreSkipped")),
	)
}

// TestReconcileSecretStores_CredentialBlipKeepsTheProjectedHost pins the egress
// set of an invalid projection to the config the running pods actually mount.
// The config step retains last-good, so the pods keep the [vault_plugin] section
// and its server URL; the deployment step neither errors nor requeues, so the
// networkpolicy step still runs. Dropping the host of a store whose
// CredentialsReady merely blipped would therefore leave the OpenBao egress rule
// out of a policy that denies by default, cutting the pods off from the server
// they are still pointed at.
func TestReconcileSecretStores_CredentialBlipKeepsTheProjectedHost(t *testing.T) {
	g := NewGomegaWithT(t)

	barbican := testBarbican()
	barbican.Status.ProjectedSecretStores = []string{"primary"}
	barbican.Status.ProjectedSecretStoreHosts = []string{instanceURL(testInstanceName, testNamespace)}
	// CredentialsReady is absent: the store contributes nothing to this pass.
	r := newBarbicanTestReconciler(barbican, managedStoreNamed("primary", true))

	_, projection, err := r.reconcileSecretStores(context.Background(), r.Client, barbican)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(projection.valid).To(BeFalse())
	g.Expect(projection.hosts).To(ConsistOf(instanceURL(testInstanceName, testNamespace)))
}

// TestReconcileSecretStores_RetainedHostIgnoresTheLiveStoreSpec pins the widened
// set to the RECORD of the last valid projection rather than to the store specs
// as they read right now. spec.openBao.server.url is mutable and only
// scheme-checked, so re-deriving the host would hand anyone who can write a
// store the very capability the credential-ready gate exists to deny: point the
// URL at a server of their choosing, let the projection go invalid, and the port
// lands in a destination-unrestricted egress rule on API pods they cannot edit.
func TestReconcileSecretStores_RetainedHostIgnoresTheLiveStoreSpec(t *testing.T) {
	g := NewGomegaWithT(t)

	const projectedURL = "https://bao.example.com:8200"

	barbican := testBarbican()
	barbican.Status.ProjectedSecretStores = []string{"external"}
	barbican.Status.ProjectedSecretStoreHosts = []string{projectedURL}
	// The live spec was repointed after the projection was recorded, and the
	// store no longer passes its own credential gate against the new server.
	repointed := brownfieldStoreNamed("external", "https://attacker.example.com:9999", true)
	r := newBarbicanTestReconciler(barbican, repointed)

	_, projection, err := r.reconcileSecretStores(context.Background(), r.Client, barbican)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(projection.valid).To(BeFalse())
	g.Expect(projection.hosts).To(ConsistOf(projectedURL),
		"only the recorded host of the last valid projection may widen the egress set")
}

// A store that was never projected contributes no host once it stops being
// credential-ready: the record is what the pods run, and an empty record means
// there is no last-good config pointing anywhere.
func TestReconcileSecretStores_UnprojectedStoreContributesNoRetainedHost(t *testing.T) {
	g := NewGomegaWithT(t)

	barbican := testBarbican()
	r := newBarbicanTestReconciler(barbican, managedStoreNamed("primary", true))

	_, projection, err := r.reconcileSecretStores(context.Background(), r.Client, barbican)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(projection.valid).To(BeFalse())
	g.Expect(projection.hosts).To(BeEmpty())
}

// TestReconcileSecretStores_ListErrorPropagates separates infrastructure from
// state: a failing List is not a store waiting to become ready, so it must reach
// the workqueue instead of silently invalidating the projection.
func TestReconcileSecretStores_ListErrorPropagates(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := testBarbican()
	boom := errors.New("etcd unavailable")
	c := barbicanFakeClientBuilder(barbican).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, isStoreList := list.(*barbicanv1alpha1.BarbicanSecretStoreList); isStoreList {
					return boom
				}
				return cl.List(ctx, list, opts...)
			},
		}).
		Build()
	r := &BarbicanReconciler{Client: c, Scheme: testScheme(), Recorder: record.NewFakeRecorder(10)}

	_, projection, err := r.reconcileSecretStores(context.Background(), r.Client, barbican)

	g.Expect(err).To(MatchError(boom), "the client error must stay unwrappable")
	g.Expect(err).To(MatchError(ContainSubstring("listing BarbicanSecretStores")))
	g.Expect(projection.valid).To(BeFalse())
}
