// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	commonv1 "github.com/c5c3/forge/internal/common/types"
)

// validIdentityBackend returns a minimal valid KeystoneIdentityBackend the
// per-rule tests mutate one field of, so every rejection is attributable to
// exactly one rule.
func validIdentityBackend() *KeystoneIdentityBackend {
	return &KeystoneIdentityBackend{
		ObjectMeta: metav1.ObjectMeta{Name: "corp-ldap", Namespace: "openstack"},
		Spec: KeystoneIdentityBackendSpec{
			KeystoneRef: KeystoneRefSpec{Name: "keystone"},
			Domain: DomainSpec{
				Name:           "corp",
				Mode:           DomainModeManage,
				DeletionPolicy: DomainDeletionPolicyRetain,
			},
			Type: IdentityBackendTypeLDAP,
			LDAP: &LDAPBackendSpec{
				URL:                      "ldap://ldap.corp.example.com:389",
				BindCredentialsSecretRef: commonv1.SecretRefSpec{Name: "corp-ldap-bind"},
				Suffix:                   "dc=corp,dc=example,dc=com",
				Users:                    LDAPUserSpec{TreeDN: "ou=people,dc=corp,dc=example,dc=com"},
				ReadOnly:                 ptr.To(true),
			},
		},
	}
}

func identityBackendScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := AddToScheme(s); err != nil {
		t.Fatalf("adding keystone scheme: %v", err)
	}
	return s
}

func TestIdentityBackendDefault_MaterializesDocumentedDefaults(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneIdentityBackendWebhook{}

	b := validIdentityBackend()
	b.Spec.Domain.Mode = ""
	b.Spec.Domain.DeletionPolicy = ""
	b.Spec.LDAP.ReadOnly = nil

	g.Expect(w.Default(context.Background(), b)).To(Succeed())
	g.Expect(b.Spec.Domain.Mode).To(Equal(DomainModeManage))
	g.Expect(b.Spec.Domain.DeletionPolicy).To(Equal(DomainDeletionPolicyRetain))
	g.Expect(b.Spec.LDAP.ReadOnly).To(HaveValue(BeTrue()))
}

// Default() followed by validate() on a zero-value object must not hide a
// precondition behind the happy-path fixture: the errors reported are the
// genuinely missing required fields, not a nil-pointer panic or a cryptic
// parser message.
func TestIdentityBackendDefaultThenValidate_ZeroValueObject(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneIdentityBackendWebhook{}

	b := &KeystoneIdentityBackend{ObjectMeta: metav1.ObjectMeta{Name: "zero", Namespace: "openstack"}}
	g.Expect(w.Default(context.Background(), b)).To(Succeed())

	_, err := w.ValidateCreate(context.Background(), b)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("keystoneRef.name must be set"))
	g.Expect(err.Error()).To(ContainSubstring("domain.name must be set"))
}

func TestIdentityBackendValidate_AcceptsValidBackend(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneIdentityBackendWebhook{}

	_, err := w.ValidateCreate(context.Background(), validIdentityBackend())
	g.Expect(err).NotTo(HaveOccurred())
}

func TestIdentityBackendValidate_RejectsUnionMismatch(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneIdentityBackendWebhook{}

	b := validIdentityBackend()
	b.Spec.LDAP = nil

	_, err := w.ValidateCreate(context.Background(), b)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("exactly one backend block matching spec.type"))
}

func TestIdentityBackendValidate_RejectsDefaultDomainCaseInsensitive(t *testing.T) {
	for _, name := range []string{"default", "Default", "DEFAULT"} {
		t.Run(name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			w := &KeystoneIdentityBackendWebhook{}

			b := validIdentityBackend()
			b.Spec.Domain.Name = name

			_, err := w.ValidateCreate(context.Background(), b)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("must never be backed by an external identity backend"))
		})
	}
}

func TestIdentityBackendValidate_RejectsBadURLScheme(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneIdentityBackendWebhook{}

	b := validIdentityBackend()
	b.Spec.LDAP.URL = "http://ldap.corp.example.com"

	_, err := w.ValidateCreate(context.Background(), b)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("ldap:// or ldaps://"))
}

func TestIdentityBackendValidate_RejectsBindSecretRefKey(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneIdentityBackendWebhook{}

	b := validIdentityBackend()
	b.Spec.LDAP.BindCredentialsSecretRef.Key = "bindpw"

	_, err := w.ValidateCreate(context.Background(), b)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring(`data keys are fixed ("username" and "password")`))
}

func TestIdentityBackendValidate_RejectsCASecretRefKey(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneIdentityBackendWebhook{}

	b := validIdentityBackend()
	b.Spec.LDAP.TLS = &LDAPTLSSpec{
		CABundleSecretRef: commonv1.SecretRefSpec{Name: "corp-ldap-ca", Key: "bundle.pem"},
	}

	_, err := w.ValidateCreate(context.Background(), b)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring(`data key is fixed ("ca.crt")`))
}

// A newline (or carriage-return) in any rendered LDAP value is an INI-injection
// vector — RenderINI writes values verbatim, so a smuggled "\nuser_allow_create
// = true" would defeat the readOnly forcing. The webhook rejects it per field.
func TestIdentityBackendValidate_RejectsControlCharsInLDAPFields(t *testing.T) {
	inject := "ldap://ldap.corp.example.com\nuser_allow_create = true"
	tests := []struct {
		name  string
		mutin func(*KeystoneIdentityBackend)
	}{
		{"url", func(b *KeystoneIdentityBackend) { b.Spec.LDAP.URL = inject }},
		{"suffix", func(b *KeystoneIdentityBackend) { b.Spec.LDAP.Suffix = "dc=x\nuser_allow_create = true" }},
		{"users.treeDN", func(b *KeystoneIdentityBackend) { b.Spec.LDAP.Users.TreeDN = "ou=p\r,dc=x" }},
		{"users.filter", func(b *KeystoneIdentityBackend) { b.Spec.LDAP.Users.Filter = "(x)\nuser_allow_create = true" }},
		{"users.mailAttribute", func(b *KeystoneIdentityBackend) { b.Spec.LDAP.Users.MailAttribute = "mail\nfoo = bar" }},
		{"groups.memberAttribute", func(b *KeystoneIdentityBackend) {
			b.Spec.LDAP.Groups = &LDAPGroupSpec{TreeDN: "ou=g,dc=x", MemberAttribute: "member\nfoo = bar"}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			w := &KeystoneIdentityBackendWebhook{}

			b := validIdentityBackend()
			tc.mutin(b)

			_, err := w.ValidateCreate(context.Background(), b)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("must not contain newline or carriage-return"))
		})
	}
}

// The extraOptions escape hatch is the highest-value injection vector: a value
// containing "\nuser_allow_create = true" would render after the forced-false
// options and win under oslo.config's last-value-wins scalar semantics. Reject
// control characters in extraOptions values regardless of how innocuous the
// key is.
func TestIdentityBackendValidate_RejectsControlCharsInExtraOptionValue(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneIdentityBackendWebhook{}

	b := validIdentityBackend()
	b.Spec.ExtraOptions = map[string]string{
		"zzz_pwn": "x\nuser_allow_create = true\ngroup_allow_delete = true",
	}

	_, err := w.ValidateCreate(context.Background(), b)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("must not contain newline or carriage-return"))
}

// A newline in an extraOptions KEY is the same INI-injection vector as a
// newline in the value: RenderINI writes `key = value` verbatim, so a key of
// "zzz_pwn\nuser_allow_create = true" injects that line regardless of how
// innocuous the value is. The value-side guard never inspects the key, so the
// key allowlist is the gate.
func TestIdentityBackendValidate_RejectsControlCharsInExtraOptionKey(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneIdentityBackendWebhook{}

	b := validIdentityBackend()
	b.Spec.ExtraOptions = map[string]string{
		"zzz_pwn\nuser_allow_create = true": "x",
	}

	_, err := w.ValidateCreate(context.Background(), b)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("option name must match"))
}

// A trailing space on an extraOptions KEY evades both the exact-string
// denylist and the readOnly forced-option check ("user_allow_create " !=
// "user_allow_create"), yet oslo.config strips it to a duplicate that
// overrides the forced false. The key allowlist rejects the malformed key
// before either exact-match check runs.
func TestIdentityBackendValidate_RejectsDenylistEvadingExtraOptionKey(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneIdentityBackendWebhook{}

	b := validIdentityBackend()
	b.Spec.ExtraOptions = map[string]string{"user_allow_create ": "true"}

	_, err := w.ValidateCreate(context.Background(), b)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("option name must match"))
}

func TestIdentityBackendValidate_ExtraOptionsDenylist(t *testing.T) {
	tests := []struct {
		name    string
		option  string
		errText string
	}{
		{"typed-field option url", "url", `option "url" is owned by`},
		{"typed-field option user_tree_dn", "user_tree_dn", `option "user_tree_dn" is owned by`},
		{"operator-owned driver", "driver", `option "driver" is owned by`},
		{"operator-owned domain_config_dir", "domain_config_dir", `option "domain_config_dir" is owned by`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			w := &KeystoneIdentityBackendWebhook{}

			b := validIdentityBackend()
			b.Spec.ExtraOptions = map[string]string{tc.option: "x"}

			_, err := w.ValidateCreate(context.Background(), b)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring(tc.errText))
		})
	}
}

func TestIdentityBackendValidate_ExtraOptionsReadOnlyForced(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneIdentityBackendWebhook{}

	b := validIdentityBackend()
	b.Spec.ExtraOptions = map[string]string{"user_allow_create": "true"}

	_, err := w.ValidateCreate(context.Background(), b)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring(`conflicts with readOnly: true`))

	// With an explicit readOnly: false, the same option is permitted (the
	// operator no longer forces the write-enabling options).
	b.Spec.LDAP.ReadOnly = ptr.To(false)
	_, err = w.ValidateCreate(context.Background(), b)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestIdentityBackendValidate_ExtraOptionsAllowsUnownedOption(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneIdentityBackendWebhook{}

	b := validIdentityBackend()
	b.Spec.ExtraOptions = map[string]string{"page_size": "100"}

	_, err := w.ValidateCreate(context.Background(), b)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestIdentityBackendValidate_ExtraOptionsRejectsEmptyKey(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneIdentityBackendWebhook{}

	b := validIdentityBackend()
	b.Spec.ExtraOptions = map[string]string{"": "x"}

	_, err := w.ValidateCreate(context.Background(), b)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("option name must not be empty"))
}

func TestIdentityBackendValidate_DomainUniqueness(t *testing.T) {
	g := NewGomegaWithT(t)
	s := identityBackendScheme(t)

	existing := validIdentityBackend()
	existing.Name = "existing-ldap"
	existing.Spec.Domain.Name = "Corp" // differs only in case

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(existing).Build()
	w := &KeystoneIdentityBackendWebhook{Client: c}

	// Same Keystone + case-insensitively equal domain name: rejected.
	b := validIdentityBackend()
	_, err := w.ValidateCreate(context.Background(), b)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("domain name collides"))
	g.Expect(err.Error()).To(ContainSubstring("existing-ldap"))

	// Different Keystone, same domain name: accepted.
	b2 := validIdentityBackend()
	b2.Spec.KeystoneRef.Name = "keystone-other"
	_, err = w.ValidateCreate(context.Background(), b2)
	g.Expect(err).NotTo(HaveOccurred())
}

// On UPDATE the object under validation appears in the sibling List and must
// not collide with itself.
func TestIdentityBackendValidate_DomainUniquenessSkipsSelfOnUpdate(t *testing.T) {
	g := NewGomegaWithT(t)
	s := identityBackendScheme(t)

	self := validIdentityBackend()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(self).Build()
	w := &KeystoneIdentityBackendWebhook{Client: c}

	updated := validIdentityBackend()
	updated.Spec.Domain.Description = "updated"
	_, err := w.ValidateUpdate(context.Background(), self, updated)
	g.Expect(err).NotTo(HaveOccurred())
}

// A Terminating sibling must not block a replacement backend for the same
// domain (recreate-during-teardown).
func TestIdentityBackendValidate_DomainUniquenessIgnoresTerminatingSibling(t *testing.T) {
	g := NewGomegaWithT(t)
	s := identityBackendScheme(t)

	terminating := validIdentityBackend()
	terminating.Name = "old-ldap"
	now := metav1.Now()
	terminating.DeletionTimestamp = &now
	terminating.Finalizers = []string{"keystone.openstack.c5c3.io/identitybackend"}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(terminating).Build()
	w := &KeystoneIdentityBackendWebhook{Client: c}

	b := validIdentityBackend()
	_, err := w.ValidateCreate(context.Background(), b)
	g.Expect(err).NotTo(HaveOccurred())
}

// Aggregate test proving error accumulation: every violated rule surfaces in
// one admission error, with a substring assertion per rule (no
// short-circuiting).
func TestIdentityBackendValidateCreate_RunsAllValidations(t *testing.T) {
	g := NewGomegaWithT(t)
	s := identityBackendScheme(t)

	existing := validIdentityBackend()
	existing.Name = "existing-ldap"
	existing.Spec.Domain.Name = "broken" // collides with the CR under test
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(existing).Build()
	w := &KeystoneIdentityBackendWebhook{Client: c}

	b := validIdentityBackend()
	b.Spec.Domain.Name = "broken"
	b.Spec.LDAP.URL = "http://not-ldap"
	b.Spec.LDAP.BindCredentialsSecretRef.Key = "oops"
	b.Spec.ExtraOptions = map[string]string{
		"driver":            "sql",
		"user_allow_create": "true",
	}

	_, err := w.ValidateCreate(context.Background(), b)
	g.Expect(err).To(HaveOccurred())
	msg := err.Error()
	g.Expect(msg).To(ContainSubstring("ldap:// or ldaps://"))
	g.Expect(msg).To(ContainSubstring(`data keys are fixed`))
	g.Expect(msg).To(ContainSubstring(`option "driver" is owned by`))
	g.Expect(msg).To(ContainSubstring(`conflicts with readOnly: true`))
	g.Expect(msg).To(ContainSubstring("domain name collides"))
}
