// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package database

import (
	"net/url"
	"testing"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"

	commonv1 "github.com/c5c3/forge/internal/common/types"
)

var testPaths = TLSPaths{
	CA:   "/etc/keystone/db-tls/ca.crt",
	Cert: "/etc/keystone/db-tls/tls.crt",
	Key:  "/etc/keystone/db-tls/tls.key",
}

func TestSSLParams(t *testing.T) {
	g := gomega.NewWithT(t)

	params, err := SSLParams("require", testPaths)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(params.Get("ssl_ca")).To(gomega.Equal(testPaths.CA))
	g.Expect(params.Get("ssl_cert")).To(gomega.Equal(testPaths.Cert))
	g.Expect(params.Get("ssl_key")).To(gomega.Equal(testPaths.Key))
	g.Expect(params).NotTo(gomega.HaveKey("ssl_verify_cert"))

	params, err = SSLParams("verify-ca", testPaths)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(params.Get("ssl_verify_cert")).To(gomega.Equal("true"))
	g.Expect(params).NotTo(gomega.HaveKey("ssl_verify_identity"))

	params, err = SSLParams("verify-full", testPaths)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(params.Get("ssl_verify_cert")).To(gomega.Equal("true"))
	g.Expect(params.Get("ssl_verify_identity")).To(gomega.Equal("true"))
}

// An unknown mode — including the empty string — must error rather than
// silently assembling a partially-formed DSN.
func TestSSLParams_RejectsUnknownMode(t *testing.T) {
	g := gomega.NewWithT(t)

	for _, mode := range []string{"", "disabled", "bogus"} {
		_, err := SSLParams(mode, testPaths)
		g.Expect(err).To(gomega.HaveOccurred(), "mode %q must be rejected", mode)
	}
}

func TestAppendTLSParams(t *testing.T) {
	g := gomega.NewWithT(t)

	// Nil TLS block: plaintext DSN, no-op.
	query := url.Values{}
	query.Set("charset", "utf8")
	g.Expect(AppendTLSParams(nil, testPaths, query)).To(gomega.Succeed())
	g.Expect(query).To(gomega.HaveLen(1))

	// Enabled mode merges the ssl_* params next to charset.
	tls := &commonv1.DatabaseTLSSpec{Mode: "require"}
	g.Expect(AppendTLSParams(tls, testPaths, query)).To(gomega.Succeed())
	g.Expect(query.Get("ssl_ca")).To(gomega.Equal(testPaths.CA))
	g.Expect(query.Get("charset")).To(gomega.Equal("utf8"))
}

func TestBuildDSN(t *testing.T) {
	g := gomega.NewWithT(t)

	query := url.Values{}
	query.Set("charset", "utf8")
	dsn, digest := BuildDSN("keystone", "s3cret", "db.openstack.svc:3306", "keystone", query)

	g.Expect(dsn).To(gomega.Equal("mysql+pymysql://keystone:s3cret@db.openstack.svc:3306/keystone?charset=utf8"))
	g.Expect(digest).To(gomega.HaveLen(64), "digest must be a sha256 hex string")

	// The digest must be stable for identical inputs and change with them.
	_, digest2 := BuildDSN("keystone", "s3cret", "db.openstack.svc:3306", "keystone", query)
	g.Expect(digest2).To(gomega.Equal(digest))
	_, rotated := BuildDSN("keystone", "rotated", "db.openstack.svc:3306", "keystone", query)
	g.Expect(rotated).NotTo(gomega.Equal(digest))
}

// Reserved characters in the password must be percent-encoded (RFC 3986
// userinfo), while "/" in the ssl_* file paths must stay literal so alembic's
// ConfigParser never sees "%" interpolation syntax.
func TestBuildDSN_EncodingEdgeCases(t *testing.T) {
	g := gomega.NewWithT(t)

	query := url.Values{}
	query.Set("charset", "utf8")
	g.Expect(AppendTLSParams(&commonv1.DatabaseTLSSpec{Mode: "require"}, testPaths, query)).To(gomega.Succeed())

	dsn, _ := BuildDSN("keystone", "p@ss/w:rd", "db:3306", "keystone", query)

	g.Expect(dsn).To(gomega.ContainSubstring("keystone:p%40ss%2Fw%3Ard@db:3306"),
		"reserved characters in the password must be percent-encoded")
	g.Expect(dsn).To(gomega.ContainSubstring("ssl_ca=/etc/keystone/db-tls/ca.crt"),
		"slashes in ssl_* paths must stay literal, not %2F")
}

func TestResolveHostAndPort(t *testing.T) {
	g := gomega.NewWithT(t)

	managed := &commonv1.DatabaseSpec{ClusterRef: &corev1.LocalObjectReference{Name: "mariadb"}}
	g.Expect(ResolveHost(managed, "openstack")).To(gomega.Equal("mariadb.openstack.svc:3306"))

	brownfield := &commonv1.DatabaseSpec{Host: "db.example.com", Port: 3307}
	g.Expect(ResolveHost(brownfield, "openstack")).To(gomega.Equal("db.example.com:3307"))

	g.Expect(Port(&commonv1.DatabaseSpec{})).To(gomega.Equal(int32(3306)), "zero port must default to 3306")
}

func TestResolveUsername(t *testing.T) {
	g := gomega.NewWithT(t)

	data := map[string][]byte{"username": []byte("v-kube-abc")}

	// Static managed mode: the CR instance name IS the MySQL username.
	staticManaged := &commonv1.DatabaseSpec{ClusterRef: &corev1.LocalObjectReference{Name: "mariadb"}}
	name, ok := ResolveUsername(staticManaged, "keystone-prod", data)
	g.Expect(ok).To(gomega.BeTrue())
	g.Expect(name).To(gomega.Equal("keystone-prod"))

	// Dynamic managed mode: the engine-issued username comes from the Secret.
	dynamic := &commonv1.DatabaseSpec{
		ClusterRef:      &corev1.LocalObjectReference{Name: "mariadb"},
		CredentialsMode: commonv1.CredentialsModeDynamic,
	}
	name, ok = ResolveUsername(dynamic, "keystone-prod", data)
	g.Expect(ok).To(gomega.BeTrue())
	g.Expect(name).To(gomega.Equal("v-kube-abc"))

	// A Secret without the username key must report not-ok, never an empty
	// username spliced into a DSN.
	_, ok = ResolveUsername(dynamic, "keystone-prod", map[string][]byte{})
	g.Expect(ok).To(gomega.BeFalse())
}
