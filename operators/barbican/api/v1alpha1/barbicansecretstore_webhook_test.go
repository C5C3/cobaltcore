// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// validBarbicanSecretStore returns a minimal valid managed (instanceRef) store
// the per-rule tests mutate one field of, so every rejection is attributable to
// exactly one rule.
func validBarbicanSecretStore() *BarbicanSecretStore {
	return &BarbicanSecretStore{
		ObjectMeta: metav1.ObjectMeta{Name: "openbao-primary", Namespace: "openstack"},
		Spec: BarbicanSecretStoreSpec{
			BarbicanRef: BarbicanRefSpec{Name: "barbican"},
			Type:        BarbicanSecretStoreTypeOpenBao,
			OpenBao: &OpenBaoStoreSpec{
				InstanceRef:  &InstanceRefSpec{Name: "openbao"},
				KVMountpoint: DefaultKVMountpoint,
			},
			IsDefault: true,
		},
	}
}

func barbicanScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := AddToScheme(s); err != nil {
		t.Fatalf("adding barbican scheme: %v", err)
	}
	return s
}

func TestBarbicanSecretStoreDefault_KVMountpoint(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &BarbicanSecretStoreWebhook{}

	// Empty: filled with the provisioned mount.
	empty := validBarbicanSecretStore()
	empty.Spec.OpenBao.KVMountpoint = ""
	g.Expect(w.Default(context.Background(), empty)).To(gomega.Succeed())
	g.Expect(empty.Spec.OpenBao.KVMountpoint).To(gomega.Equal("barbican"))

	// A brownfield store's explicit mount is preserved.
	explicit := validBarbicanSecretStore()
	explicit.Spec.OpenBao.KVMountpoint = "kv-barbican"
	g.Expect(w.Default(context.Background(), explicit)).To(gomega.Succeed())
	g.Expect(explicit.Spec.OpenBao.KVMountpoint).To(gomega.Equal("kv-barbican"))

	// A store carrying no openBao block has nothing to default.
	none := validBarbicanSecretStore()
	none.Spec.OpenBao = nil
	g.Expect(w.Default(context.Background(), none)).To(gomega.Succeed())
	g.Expect(none.Spec.OpenBao).To(gomega.BeNil())
}

func TestBarbicanSecretStoreValidate_AcceptsValidStores(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &BarbicanSecretStoreWebhook{}

	_, err := w.ValidateCreate(context.Background(), validBarbicanSecretStore())
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// A brownfield store names any mount in any OpenBao namespace: its server is
	// provisioned outside this operator, so the managed-mode freeze does not apply.
	brownfield := validBarbicanSecretStore()
	brownfield.Spec.OpenBao = &OpenBaoStoreSpec{
		Server: &OpenBaoServerSpec{
			URL:                  "https://openbao.example.com:8200",
			CredentialsSecretRef: SecretNameRefSpec{Name: "openbao-approle"},
		},
		KVMountpoint: "kv-barbican",
		Namespace:    "tenant-a",
	}
	_, err = w.ValidateCreate(context.Background(), brownfield)
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

// Every arm of the union defense in depth, the webhook twin of the three CEL
// rules on the spec.
func TestBarbicanSecretStoreValidate_UnionArms(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(o *BarbicanSecretStore)
		wantSub string
	}{
		{
			name:    "type OpenBao without the openBao block rejected",
			mutate:  func(o *BarbicanSecretStore) { o.Spec.OpenBao = nil },
			wantSub: "exactly one store block matching spec.type",
		},
		{
			name:    "openBao block under a foreign type rejected",
			mutate:  func(o *BarbicanSecretStore) { o.Spec.Type = BarbicanSecretStoreType("KMIP") },
			wantSub: "exactly one store block matching spec.type",
		},
		{
			name: "instanceRef and server both set rejected",
			mutate: func(o *BarbicanSecretStore) {
				o.Spec.OpenBao.Server = &OpenBaoServerSpec{
					URL:                  "https://openbao.example.com:8200",
					CredentialsSecretRef: SecretNameRefSpec{Name: "openbao-approle"},
				}
			},
			wantSub: "exactly one of instanceRef or server",
		},
		{
			name: "neither instanceRef nor server rejected",
			mutate: func(o *BarbicanSecretStore) {
				o.Spec.OpenBao.InstanceRef = nil
			},
			wantSub: "exactly one of instanceRef or server",
		},
		{
			name: "managed store on a foreign mount rejected",
			mutate: func(o *BarbicanSecretStore) {
				o.Spec.OpenBao.KVMountpoint = "kv-barbican"
			},
			wantSub: `spec.openBao.kvMountpoint: Invalid value: "kv-barbican"`,
		},
		{
			name: "managed store in an OpenBao namespace rejected",
			mutate: func(o *BarbicanSecretStore) {
				o.Spec.OpenBao.Namespace = "tenant-a"
			},
			wantSub: `spec.openBao.namespace: Invalid value: "tenant-a"`,
		},
		// A plaintext server URL puts the AppRole credentials and every secret
		// barbican stores on the wire in the clear, and silently makes a supplied
		// caBundleSecretRef a no-op — a CR that reads as TLS-configured but is not.
		{
			name: "brownfield store on a plaintext URL rejected",
			mutate: func(o *BarbicanSecretStore) {
				o.Spec.OpenBao.InstanceRef = nil
				o.Spec.OpenBao.Server = &OpenBaoServerSpec{
					URL:                  "http://vault.internal:8200",
					CredentialsSecretRef: SecretNameRefSpec{Name: "openbao-approle"},
				}
			},
			wantSub: "url must use the https:// scheme",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			w := &BarbicanSecretStoreWebhook{}

			obj := validBarbicanSecretStore()
			tc.mutate(obj)
			_, err := w.ValidateCreate(context.Background(), obj)
			g.Expect(err).To(gomega.HaveOccurred())
			g.Expect(err.Error()).To(gomega.ContainSubstring(tc.wantSub))
		})
	}
}

// metadata.name becomes the suffix of the [secretstore:<name>] section barbican
// reads, and barbican resolves that suffix in the flat section namespace of one
// config file, so a name equal to a section the operator or an oslo library owns
// would collide with it.
func TestBarbicanSecretStoreValidate_RejectsReservedStoreNames(t *testing.T) {
	names := []string{
		"default", "DEFAULT", "database", "keystone_authtoken", "secretstore",
		"vault_plugin", "queue", "oslo_policy", "oslo_middleware", "healthcheck",
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			w := &BarbicanSecretStoreWebhook{}

			obj := validBarbicanSecretStore()
			obj.Name = name
			_, err := w.ValidateCreate(context.Background(), obj)
			g.Expect(err).To(gomega.HaveOccurred())
			g.Expect(err.Error()).To(gomega.ContainSubstring("reserved or operator-managed barbican.conf section"))
		})
	}
}

func TestBarbicanSecretStoreValidate_AcceptsNormalName(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &BarbicanSecretStoreWebhook{}

	obj := validBarbicanSecretStore()
	obj.Name = "openbao-eu-de-1"
	_, err := w.ValidateCreate(context.Background(), obj)
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

// The operator derives the AppRole credentials Secret by appending "-approle" to
// metadata.name, which leaves 55 of the Secret name's 63 characters for the CR.
// Like the sibling Barbican bound the rule is create-only, so a grandfathered CR
// stays editable — including the finalizer-removal update that completes its
// deletion.
func TestBarbicanSecretStoreValidate_NameLengthBoundedByAppRoleSecret(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &BarbicanSecretStoreWebhook{}

	g.Expect(MaxBarbicanSecretStoreNameLength).To(gomega.Equal(55))

	atLimit := validBarbicanSecretStore()
	atLimit.Name = strings.Repeat("s", MaxBarbicanSecretStoreNameLength)
	_, err := w.ValidateCreate(context.Background(), atLimit)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	tooLong := validBarbicanSecretStore()
	tooLong.Name = strings.Repeat("s", MaxBarbicanSecretStoreNameLength+1)
	tooLong.Finalizers = []string{"barbican.openstack.c5c3.io/secretstore"}
	_, err = w.ValidateCreate(context.Background(), tooLong)
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("metadata.name")))
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("-approle")))

	deleting := tooLong.DeepCopy()
	deleting.Finalizers = nil
	_, err = w.ValidateUpdate(context.Background(), tooLong, deleting)
	g.Expect(err).NotTo(gomega.HaveOccurred(),
		"an over-long grandfathered store must stay updatable, or its deletion never completes")
}

func TestBarbicanSecretStoreValidate_ExtraOptions(t *testing.T) {
	tests := []struct {
		name    string
		options map[string]string
		wantSub string
	}{
		{
			name:    "bad key pattern rejected",
			options: map[string]string{"bad key": "x"},
			wantSub: "option name must match",
		},
		{
			name:    "newline in key rejected",
			options: map[string]string{"foo\nvault_url = https://attacker.example": "x"},
			wantSub: "option name must match",
		},
		{
			name:    "empty key rejected",
			options: map[string]string{"": "x"},
			wantSub: "option name must not be empty",
		},
		{
			name:    "derived vault_url rejected",
			options: map[string]string{"vault_url": "https://attacker.example:8200"},
			wantSub: `option "vault_url" is owned by the operator (derived from spec.openBao.instanceRef, or spec.openBao.server.url)`,
		},
		{
			name:    "derived use_ssl rejected",
			options: map[string]string{"use_ssl": "False"},
			wantSub: `option "use_ssl" is owned by the operator (derived from the resolved server URL's scheme)`,
		},
		{
			name:    "ssl_ca_crt_file rejected",
			options: map[string]string{"ssl_ca_crt_file": "/tmp/ca.crt"},
			wantSub: `option "ssl_ca_crt_file" is owned by the operator (spec.openBao.server.caBundleSecretRef`,
		},
		{
			name:    "approle_role_id rejected",
			options: map[string]string{"approle_role_id": "00000000-0000-0000-0000-000000000000"},
			wantSub: `option "approle_role_id" is owned by the operator`,
		},
		{
			name:    "approle_secret_id rejected",
			options: map[string]string{"approle_secret_id": "s.redacted"},
			wantSub: `option "approle_secret_id" is owned by the operator (env-injected from the credentials Secret`,
		},
		{
			name:    "root_token_id rejected",
			options: map[string]string{"root_token_id": "s.root"},
			wantSub: `option "root_token_id" is owned by the operator`,
		},
		{
			name:    "kv_mountpoint rejected",
			options: map[string]string{"kv_mountpoint": "other"},
			wantSub: `option "kv_mountpoint" is owned by spec.openBao.kvMountpoint`,
		},
		{
			name:    "namespace rejected",
			options: map[string]string{"namespace": "tenant-a"},
			wantSub: `option "namespace" is owned by spec.openBao.namespace`,
		},
		{
			name:    "secret_store_plugin rejected",
			options: map[string]string{"secret_store_plugin": "store_crypto"},
			wantSub: `option "secret_store_plugin" is owned by the operator (secret-store section wiring)`,
		},
		{
			name:    "crypto_plugin rejected",
			options: map[string]string{"crypto_plugin": "simple_crypto"},
			wantSub: `option "crypto_plugin" is owned by the operator (secret-store section wiring)`,
		},
		{
			name:    "global_default rejected",
			options: map[string]string{"global_default": "True"},
			wantSub: `option "global_default" is owned by spec.isDefault`,
		},
		{
			name:    "control-char value rejected",
			options: map[string]string{"generate_secret": "10\nvault_url = https://attacker.example"},
			wantSub: "must not contain newline or carriage-return",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			w := &BarbicanSecretStoreWebhook{}

			obj := validBarbicanSecretStore()
			obj.Spec.ExtraOptions = tc.options
			_, err := w.ValidateCreate(context.Background(), obj)
			g.Expect(err).To(gomega.HaveOccurred())
			g.Expect(err.Error()).To(gomega.ContainSubstring(tc.wantSub))
		})
	}
}

func TestBarbicanSecretStoreValidate_ExtraOptionsAllowsBenignOption(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &BarbicanSecretStoreWebhook{}

	obj := validBarbicanSecretStore()
	obj.Spec.ExtraOptions = map[string]string{"generate_secret": "True"}
	_, err := w.ValidateCreate(context.Background(), obj)
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

func TestBarbicanSecretStoreValidate_SiblingDefault(t *testing.T) {
	g := gomega.NewWithT(t)

	existing := validBarbicanSecretStore()
	existing.Name = "existing-default"

	c := fake.NewClientBuilder().WithScheme(barbicanScheme(t)).WithObjects(existing).Build()
	w := &BarbicanSecretStoreWebhook{Client: c}

	// Second default for the same Barbican: rejected.
	obj := validBarbicanSecretStore()
	obj.Name = "new-default"
	_, err := w.ValidateCreate(context.Background(), obj)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("exactly one default store is allowed"))
	g.Expect(err.Error()).To(gomega.ContainSubstring("existing-default"))

	// A store attached to a DIFFERENT Barbican is untouched by both sibling rules.
	other := validBarbicanSecretStore()
	other.Name = "other-default"
	other.Spec.BarbicanRef.Name = "barbican-other"
	_, err = w.ValidateCreate(context.Background(), other)
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

// The [vault_plugin] section is process-global, so a second OpenBao store on the
// same Barbican is rejected even when neither claims the default.
func TestBarbicanSecretStoreValidate_SiblingOpenBaoUniqueness(t *testing.T) {
	g := gomega.NewWithT(t)

	existing := validBarbicanSecretStore()
	existing.Name = "openbao-existing"
	existing.Spec.IsDefault = false

	c := fake.NewClientBuilder().WithScheme(barbicanScheme(t)).WithObjects(existing).Build()
	w := &BarbicanSecretStoreWebhook{Client: c}

	obj := validBarbicanSecretStore()
	obj.Name = "openbao-second"
	obj.Spec.IsDefault = false
	_, err := w.ValidateCreate(context.Background(), obj)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("is already of type OpenBao"))
	g.Expect(err.Error()).To(gomega.ContainSubstring("openbao-existing"))
	g.Expect(err.Error()).NotTo(gomega.ContainSubstring("exactly one default store is allowed"))
}

// On UPDATE the object under validation appears in the sibling List and must not
// collide with itself, under either rule.
func TestBarbicanSecretStoreValidate_SiblingChecksSkipSelfOnUpdate(t *testing.T) {
	g := gomega.NewWithT(t)

	self := validBarbicanSecretStore()
	c := fake.NewClientBuilder().WithScheme(barbicanScheme(t)).WithObjects(self).Build()
	w := &BarbicanSecretStoreWebhook{Client: c}

	updated := validBarbicanSecretStore()
	updated.Spec.ExtraOptions = map[string]string{"generate_secret": "True"}
	_, err := w.ValidateUpdate(context.Background(), self, updated)
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

// A Terminating sibling must not block a replacement store for the same Barbican
// (recreate-during-teardown), under either rule.
func TestBarbicanSecretStoreValidate_SiblingChecksIgnoreTerminating(t *testing.T) {
	g := gomega.NewWithT(t)

	terminating := validBarbicanSecretStore()
	terminating.Name = "openbao-old"
	now := metav1.Now()
	terminating.DeletionTimestamp = &now
	terminating.Finalizers = []string{"barbican.openstack.c5c3.io/secretstore"}

	c := fake.NewClientBuilder().WithScheme(barbicanScheme(t)).WithObjects(terminating).Build()
	w := &BarbicanSecretStoreWebhook{Client: c}

	obj := validBarbicanSecretStore()
	obj.Name = "openbao-new"
	_, err := w.ValidateCreate(context.Background(), obj)
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

// A List failure must surface as an admission error rather than silently
// admitting a store that may collide with a sibling.
func TestBarbicanSecretStoreValidate_SiblingListErrorSurfaced(t *testing.T) {
	g := gomega.NewWithT(t)

	c := fake.NewClientBuilder().WithScheme(barbicanScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
				return errors.New("boom")
			},
		}).Build()
	w := &BarbicanSecretStoreWebhook{Client: c}

	_, err := w.ValidateCreate(context.Background(), validBarbicanSecretStore())
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("listing BarbicanSecretStores for the single-default check"))
	g.Expect(err.Error()).To(gomega.ContainSubstring("listing BarbicanSecretStores for the OpenBao-uniqueness check"))
}
