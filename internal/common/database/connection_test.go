// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package database

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/c5c3/forge/internal/common/multicluster"
	commonv1 "github.com/c5c3/forge/internal/common/types"
)

const (
	connInstance  = "keystone"
	connNamespace = "openstack"
	connTLSMount  = "/etc/keystone/db-tls/"
)

func connScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	return s
}

func connOwner() *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "keystone-owner", Namespace: connNamespace, UID: "conn-uid"}}
}

func upstreamSecret(data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "keystone-db", Namespace: connNamespace},
		Data:       data,
	}
}

func staticDBSpec() *commonv1.DatabaseSpec {
	return &commonv1.DatabaseSpec{
		ClusterRef: &corev1.LocalObjectReference{Name: "mariadb"},
		Database:   "keystone",
		SecretRef:  commonv1.SecretRefSpec{Name: "keystone-db"},
	}
}

func connParams(c client.Client, s *runtime.Scheme, owner client.Object, spec *commonv1.DatabaseSpec, conds *[]metav1.Condition) ConnectionSecretFlowParams {
	return ConnectionSecretFlowParams{
		Client:        c,
		Scheme:        s,
		Owner:         owner,
		InstanceName:  connInstance,
		Namespace:     connNamespace,
		Database:      spec,
		TLSMountPath:  connTLSMount,
		Conditions:    conds,
		Generation:    1,
		ConditionType: "SecretsReady",
		RequeueAfter:  15 * time.Second,
	}
}

// --- helpers ---

func TestConnectionSecretName(t *testing.T) {
	g := NewWithT(t)
	g.Expect(ConnectionSecretName("keystone")).To(Equal("keystone-db-connection"))
}

func TestConnectionEnvVar(t *testing.T) {
	g := NewWithT(t)
	e := ConnectionEnvVar("keystone")
	g.Expect(e.Name).To(Equal(ConnectionEnvVarName))
	g.Expect(e.ValueFrom.SecretKeyRef.Name).To(Equal("keystone-db-connection"))
	g.Expect(e.ValueFrom.SecretKeyRef.Key).To(Equal(ConnectionSecretKey))
}

func TestConnectionEnvVarForSection(t *testing.T) {
	g := NewWithT(t)
	// Placement keeps its database options in [placement_database], so the
	// oslo.config override must be named after that section.
	e := ConnectionEnvVarForSection("placement-sample", "placement_database")
	g.Expect(e.Name).To(Equal("OS_PLACEMENT_DATABASE__CONNECTION"))
	g.Expect(e.ValueFrom.SecretKeyRef.Name).To(Equal("placement-sample-db-connection"))
	g.Expect(e.ValueFrom.SecretKeyRef.Key).To(Equal(ConnectionSecretKey))

	// The legacy helper is exactly the "database"-section specialisation, so
	// existing callers keep their OS_DATABASE__CONNECTION spelling.
	g.Expect(ConnectionEnvVar(connInstance)).To(Equal(ConnectionEnvVarForSection(connInstance, "database")))
}

func TestTLSFilePaths(t *testing.T) {
	g := NewWithT(t)
	p := TLSFilePaths("/etc/keystone/db-tls/")
	g.Expect(p.CA).To(Equal("/etc/keystone/db-tls/ca.crt"))
	g.Expect(p.Cert).To(Equal("/etc/keystone/db-tls/tls.crt"))
	g.Expect(p.Key).To(Equal("/etc/keystone/db-tls/tls.key"))
}

// --- waiting gates ---

func TestReconcileConnectionSecret_upstreamMissing(t *testing.T) {
	g := NewWithT(t)
	s := connScheme()
	owner := connOwner()
	var conds []metav1.Condition
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(owner).Build()

	res, digest, err := ReconcileConnectionSecret(context.Background(), connParams(c, s, owner, staticDBSpec(), &conds))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(digest).To(BeEmpty())
	g.Expect(res.RequeueAfter).To(Equal(15 * time.Second))
	cond := findCond(conds, "SecretsReady")
	g.Expect(cond.Reason).To(Equal(ReasonWaitingForDBCredentials))
	g.Expect(cond.Message).To(ContainSubstring("not found"))
	// No derived Secret was materialised.
	sec := &corev1.Secret{}
	err = c.Get(context.Background(), client.ObjectKey{Name: "keystone-db-connection", Namespace: connNamespace}, sec)
	g.Expect(err).To(HaveOccurred())
}

func TestReconcileConnectionSecret_brownfieldMissingUsername(t *testing.T) {
	g := NewWithT(t)
	s := connScheme()
	owner := connOwner()
	var conds []metav1.Condition
	// Brownfield: username must come from the Secret; here only password is set.
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(owner, upstreamSecret(map[string][]byte{"password": []byte("pw")})).
		Build()
	spec := &commonv1.DatabaseSpec{Host: "db.example.com", Database: "keystone", SecretRef: commonv1.SecretRefSpec{Name: "keystone-db"}}

	res, digest, err := ReconcileConnectionSecret(context.Background(), connParams(c, s, owner, spec, &conds))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(digest).To(BeEmpty())
	g.Expect(res.RequeueAfter).To(Equal(15 * time.Second))
	g.Expect(findCond(conds, "SecretsReady").Message).To(ContainSubstring(`missing key "username"`))
}

func TestReconcileConnectionSecret_missingPassword(t *testing.T) {
	g := NewWithT(t)
	s := connScheme()
	owner := connOwner()
	var conds []metav1.Condition
	// Static managed mode: username derived from the CR name, but password absent.
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(owner, upstreamSecret(map[string][]byte{})).
		Build()

	_, digest, err := ReconcileConnectionSecret(context.Background(), connParams(c, s, owner, staticDBSpec(), &conds))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(digest).To(BeEmpty())
	g.Expect(findCond(conds, "SecretsReady").Message).To(ContainSubstring(`missing key "password"`))
}

// --- happy path ---

func TestReconcileConnectionSecret_createsStaticSecret(t *testing.T) {
	g := NewWithT(t)
	s := connScheme()
	owner := connOwner()
	var conds []metav1.Condition
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(owner, upstreamSecret(map[string][]byte{"password": []byte("s3cr3t")})).
		Build()

	res, digest, err := ReconcileConnectionSecret(context.Background(), connParams(c, s, owner, staticDBSpec(), &conds))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(digest).NotTo(BeEmpty())

	derived := &corev1.Secret{}
	g.Expect(c.Get(context.Background(), client.ObjectKey{Name: "keystone-db-connection", Namespace: connNamespace}, derived)).To(Succeed())
	// Exactly one "connection" key, owned by the owner.
	g.Expect(derived.Data).To(HaveLen(1))
	conn := string(derived.Data[ConnectionSecretKey])
	// Static managed mode: username is the CR name; host is the MariaDB Service DNS.
	g.Expect(conn).To(HavePrefix("mysql+pymysql://keystone:s3cr3t@mariadb.openstack.svc:3306/keystone"))
	g.Expect(conn).To(ContainSubstring("charset=utf8"))
	g.Expect(derived.OwnerReferences).To(HaveLen(1))
	g.Expect(derived.OwnerReferences[0].Name).To(Equal("keystone-owner"))
}

// TestReconcileConnectionSecret_remoteCreatesLabelledAndUnowned pins the
// target-cluster path: the derived Secret is stamped with the ownership labels
// and carries no owner reference, because a reference to a CR on another cluster
// names a UID this one cannot resolve.
func TestReconcileConnectionSecret_remoteCreatesLabelledAndUnowned(t *testing.T) {
	g := NewWithT(t)
	s := connScheme()
	owner := connOwner()
	var conds []metav1.Condition
	c := multicluster.Remote(fake.NewClientBuilder().WithScheme(s).
		WithObjects(owner, upstreamSecret(map[string][]byte{"password": []byte("s3cr3t")})).
		Build())

	_, digest, err := ReconcileConnectionSecret(context.Background(), connParams(c, s, owner, staticDBSpec(), &conds))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(digest).NotTo(BeEmpty())

	derived := &corev1.Secret{}
	g.Expect(c.Get(context.Background(), client.ObjectKey{Name: "keystone-db-connection", Namespace: connNamespace}, derived)).To(Succeed())
	g.Expect(derived.Data).To(HaveLen(1))
	g.Expect(derived.Labels).To(Equal(map[string]string{
		multicluster.OwnerKindLabel:      "ConfigMap",
		multicluster.OwnerNameLabel:      "keystone-owner",
		multicluster.OwnerNamespaceLabel: connNamespace,
	}))
	g.Expect(derived.OwnerReferences).To(BeEmpty(), "a remote child must carry no owner reference")
}

func TestReconcileConnectionSecret_dynamicUsesSecretUsername(t *testing.T) {
	g := NewWithT(t)
	s := connScheme()
	owner := connOwner()
	var conds []metav1.Condition
	spec := staticDBSpec()
	spec.CredentialsMode = commonv1.CredentialsModeDynamic
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(owner, upstreamSecret(map[string][]byte{"username": []byte("v-kube-abc"), "password": []byte("pw")})).
		Build()

	_, _, err := ReconcileConnectionSecret(context.Background(), connParams(c, s, owner, spec, &conds))
	g.Expect(err).NotTo(HaveOccurred())
	derived := &corev1.Secret{}
	g.Expect(c.Get(context.Background(), client.ObjectKey{Name: "keystone-db-connection", Namespace: connNamespace}, derived)).To(Succeed())
	g.Expect(string(derived.Data[ConnectionSecretKey])).To(HavePrefix("mysql+pymysql://v-kube-abc:pw@"))
}

func TestReconcileConnectionSecret_tlsParamsUseLiteralSlash(t *testing.T) {
	g := NewWithT(t)
	s := connScheme()
	owner := connOwner()
	var conds []metav1.Condition
	spec := staticDBSpec()
	spec.TLS = &commonv1.DatabaseTLSSpec{
		Mode:                "verify-full",
		CABundleSecretRef:   commonv1.SecretRefSpec{Name: "db-ca"},
		ClientCertSecretRef: commonv1.SecretRefSpec{Name: "db-client"},
	}
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(owner, upstreamSecret(map[string][]byte{"password": []byte("pw")})).
		Build()

	_, _, err := ReconcileConnectionSecret(context.Background(), connParams(c, s, owner, spec, &conds))
	g.Expect(err).NotTo(HaveOccurred())
	derived := &corev1.Secret{}
	g.Expect(c.Get(context.Background(), client.ObjectKey{Name: "keystone-db-connection", Namespace: connNamespace}, derived)).To(Succeed())
	conn := string(derived.Data[ConnectionSecretKey])
	// The ssl_ca path keeps its literal "/" (not %2F) so alembic's ConfigParser
	// never sees "%" interpolation syntax.
	g.Expect(conn).To(ContainSubstring("ssl_ca=" + connTLSMount + "ca.crt"))
	g.Expect(conn).To(ContainSubstring("ssl_verify_identity=true"))
	g.Expect(conn).NotTo(ContainSubstring("%2F"))
}

// --- drift repair & digest stability ---

func TestReconcileConnectionSecret_repairsDrift(t *testing.T) {
	g := NewWithT(t)
	s := connScheme()
	owner := connOwner()
	var conds []metav1.Condition
	// A pre-existing derived Secret with a stale value AND an extra key.
	stale := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "keystone-db-connection", Namespace: connNamespace},
		Data:       map[string][]byte{ConnectionSecretKey: []byte("stale"), "extra": []byte("x")},
	}
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(owner, stale, upstreamSecret(map[string][]byte{"password": []byte("pw")})).
		Build()

	_, _, err := ReconcileConnectionSecret(context.Background(), connParams(c, s, owner, staticDBSpec(), &conds))
	g.Expect(err).NotTo(HaveOccurred())
	derived := &corev1.Secret{}
	g.Expect(c.Get(context.Background(), client.ObjectKey{Name: "keystone-db-connection", Namespace: connNamespace}, derived)).To(Succeed())
	// Data is replaced wholesale: the extra key is gone and the value refreshed.
	g.Expect(derived.Data).To(HaveLen(1))
	g.Expect(string(derived.Data[ConnectionSecretKey])).NotTo(Equal("stale"))
}

func TestReconcileConnectionSecret_digestChangesWithCredentials(t *testing.T) {
	g := NewWithT(t)
	s := connScheme()
	owner := connOwner()
	var conds []metav1.Condition
	ctx := context.Background()

	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(owner, upstreamSecret(map[string][]byte{"password": []byte("pw1")})).
		Build()
	_, d1, err := ReconcileConnectionSecret(ctx, connParams(c, s, owner, staticDBSpec(), &conds))
	g.Expect(err).NotTo(HaveOccurred())

	// A second reconcile with the same credentials yields a stable digest.
	_, d1again, err := ReconcileConnectionSecret(ctx, connParams(c, s, owner, staticDBSpec(), &conds))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(d1again).To(Equal(d1))

	// Rotating the password changes the digest so the pods roll.
	rotated := upstreamSecret(map[string][]byte{"password": []byte("pw2")})
	g.Expect(c.Update(ctx, rotated)).To(Succeed())
	_, d2, err := ReconcileConnectionSecret(ctx, connParams(c, s, owner, staticDBSpec(), &conds))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(d2).NotTo(Equal(d1))
}

func findCond(conds []metav1.Condition, t string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == t {
			return &conds[i]
		}
	}
	return nil
}
