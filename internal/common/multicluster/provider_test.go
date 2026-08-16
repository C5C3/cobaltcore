// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package multicluster

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-logr/logr"
	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// A kubeconfig is opaque to everything under test here, so the fixtures are two
// distinguishable byte strings rather than parseable YAML.
var (
	testKubeconfig        = []byte("apiVersion: v1\nkind: Config\nclusters: []\n")
	testRotatedKubeconfig = []byte("apiVersion: v1\nkind: Config\nclusters: [rotated]\n")
)

func TestParseNamespacesTrimsEveryEntry(t *testing.T) {
	g := gomega.NewWithT(t)

	// The value is typed into a Secret by hand or templated by a chart, so the
	// spaces around the separators are the normal case and not the exception.
	namespaces, err := parseNamespaces([]byte(" openstack , tenant-a,tenant-b "))

	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(namespaces).To(gomega.Equal([]string{"openstack", "tenant-a", "tenant-b"}))
}

func TestParseNamespacesAcceptsASingleEntry(t *testing.T) {
	g := gomega.NewWithT(t)

	namespaces, err := parseNamespaces([]byte("openstack"))

	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(namespaces).To(gomega.Equal([]string{"openstack"}))
}

// An operator that reads an empty namespaces key must not fall back to the
// cluster-wide cache: the key is there, so somebody meant to restrict this
// cluster, and guessing "all namespaces" would hand out exactly the scope they
// were taking away.
func TestParseNamespacesRefusesAnEmptyValue(t *testing.T) {
	for _, raw := range []string{"", "   ", "\n\t "} {
		t.Run(raw, func(t *testing.T) {
			g := gomega.NewWithT(t)

			namespaces, err := parseNamespaces([]byte(raw))

			g.Expect(namespaces).To(gomega.BeNil())
			g.Expect(err).To(gomega.MatchError("namespaces key present but empty"))
		})
	}
}

// The offending entry has to be in the message. It is the only thing that gets
// an operator from "the cluster never engaged" to the character they mistyped.
func TestParseNamespacesRefusesAnEntryThatIsNoDNS1123Label(t *testing.T) {
	g := gomega.NewWithT(t)

	namespaces, err := parseNamespaces([]byte("tenant-a,Not_Valid,tenant-b"))

	g.Expect(namespaces).To(gomega.BeNil())
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring(`"Not_Valid"`))
}

// A trailing or doubled comma names no namespace either, and the whole value is
// refused rather than engaging the cluster one namespace short of what was
// asked for.
func TestParseNamespacesRefusesAnEmptyEntry(t *testing.T) {
	for _, raw := range []string{"tenant-a,,tenant-b", "tenant-a,"} {
		t.Run(raw, func(t *testing.T) {
			g := gomega.NewWithT(t)

			namespaces, err := parseNamespaces([]byte(raw))

			g.Expect(namespaces).To(gomega.BeNil())
			g.Expect(err).To(gomega.HaveOccurred())
		})
	}
}

func TestIsUnknownNamespaceForCacheMatchesTheCacheError(t *testing.T) {
	g := gomega.NewWithT(t)

	// Both messages controller-runtime's multi-namespace cache produces, copied
	// verbatim: a Get names the object key, a List the namespace.
	g.Expect(IsUnknownNamespaceForCache(
		errors.New("unable to get: tenant-c/seed because of unknown namespace for the cache"))).
		To(gomega.BeTrue())
	g.Expect(IsUnknownNamespaceForCache(
		errors.New("unable to list: tenant-c because of unknown namespace for the cache"))).
		To(gomega.BeTrue())
}

// The read sites wrap what the client hands them, so the match has to survive
// wrapping — which is the whole reason it is a substring test and not an
// errors.Is against a sentinel controller-runtime does not export.
func TestIsUnknownNamespaceForCacheMatchesAWrappedError(t *testing.T) {
	g := gomega.NewWithT(t)

	wrapped := fmt.Errorf("getting Secret tenant-c/keystone-db: %w",
		errors.New("unable to get: tenant-c/keystone-db because of unknown namespace for the cache"))

	g.Expect(IsUnknownNamespaceForCache(wrapped)).To(gomega.BeTrue())
}

// Everything else is a different failure with a different remedy, and a read
// site that treated it as a scope mismatch would put a misleading message on
// the CR.
func TestIsUnknownNamespaceForCacheRejectsOtherErrors(t *testing.T) {
	g := gomega.NewWithT(t)

	g.Expect(IsUnknownNamespaceForCache(nil)).To(gomega.BeFalse())
	g.Expect(IsUnknownNamespaceForCache(errors.New("connection refused"))).To(gomega.BeFalse())
	g.Expect(IsUnknownNamespaceForCache(
		apierrors.NewNotFound(corev1.Resource("secrets"), "keystone-db"))).To(gomega.BeFalse())
}

func TestRegistrationHashIsStableForTheSameRegistration(t *testing.T) {
	g := gomega.NewWithT(t)

	g.Expect(registrationHash(testKubeconfig, []byte("tenant-a"), true)).
		To(gomega.Equal(registrationHash(testKubeconfig, []byte("tenant-a"), true)))
	g.Expect(registrationHash(testKubeconfig, nil, false)).
		To(gomega.Equal(registrationHash(testKubeconfig, nil, false)))
}

// The upstream rotation semantic: a new kubeconfig under the same cluster name
// rebuilds the cluster.
func TestRegistrationHashCoversTheKubeconfig(t *testing.T) {
	g := gomega.NewWithT(t)

	g.Expect(registrationHash(testKubeconfig, []byte("tenant-a"), true)).
		NotTo(gomega.Equal(registrationHash(testRotatedKubeconfig, []byte("tenant-a"), true)))
}

// The same semantic extended to the scope: a cache's namespaces are fixed when
// it is built, so narrowing or widening them only takes effect if the changed
// value rebuilds the cluster.
func TestRegistrationHashCoversTheNamespacesValue(t *testing.T) {
	g := gomega.NewWithT(t)

	g.Expect(registrationHash(testKubeconfig, []byte("tenant-a"), true)).
		NotTo(gomega.Equal(registrationHash(testKubeconfig, []byte("tenant-a,tenant-b"), true)))
}

// Adding the key to a registration that had none is the migration from a
// cluster-wide cache to a scoped one, and it has to rebuild the cluster like
// any other scope change. The empty value is the discriminating case: hashing
// the raw bytes alone would make it indistinguishable from the absent key.
func TestRegistrationHashSeparatesAnAbsentKeyFromAPresentOne(t *testing.T) {
	g := gomega.NewWithT(t)

	absent := registrationHash(testKubeconfig, nil, false)

	g.Expect(absent).NotTo(gomega.Equal(registrationHash(testKubeconfig, nil, true)))
	g.Expect(absent).NotTo(gomega.Equal(registrationHash(testKubeconfig, []byte("tenant-a"), true)))
}

// registrationKubeconfig is a registration kubeconfig whose only variables are
// the two blocks the engagement refuses on: clusterFields is appended inside
// clusters[0].cluster, credential inside users[0].user.
func registrationKubeconfig(clusterFields, credential string) []byte {
	return []byte(`apiVersion: v1
kind: Config
clusters:
  - name: target
    cluster:
      server: https://api.target.example:6443
` + clusterFields + `contexts:
  - name: target
    context:
      cluster: target
      user: target
current-context: target
users:
  - name: target
    user:
` + credential)
}

// credentialKubeconfig varies only the user's credential block.
func credentialKubeconfig(credential string) []byte {
	return registrationKubeconfig("", credential)
}

// TestCreateAndEngageClusterRefusesAnExecCredential closes the path from "can
// write a Secret in the clusters namespace" to "runs a command in the operator
// pod". clientcmd honors both credential plugins, and client-go would fork the
// named binary the first time the engaged cluster's transport issues a request —
// under the operator's ServiceAccount, with every other engaged cluster's
// credentials in the same process. The refusal is before cluster.New, so no
// transport that could run one is ever built.
func TestCreateAndEngageClusterRefusesAnExecCredential(t *testing.T) {
	tests := []struct {
		name       string
		credential string
	}{
		{
			name: "exec plugin",
			credential: `      exec:
        apiVersion: client.authentication.k8s.io/v1
        command: /bin/sh
        args: ["-c", "cat /var/run/secrets/kubernetes.io/serviceaccount/token"]
        interactiveMode: Never
`,
		},
		{
			name: "auth provider",
			credential: `      auth-provider:
        name: oidc
`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			p := NewKubeconfigProvider(KubeconfigProviderOptions{Namespace: "c5c3-clusters"})

			err := p.createAndEngageCluster(t.Context(), "hostile-target",
				credentialKubeconfig(tc.credential), nil, "hash", logr.Discard())

			g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(
				`refusing the kubeconfig of cluster "hostile-target"`)))
			g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(
				"exec and auth-provider credentials are not supported")))
			_, engaged := p.getCluster("hostile-target")
			g.Expect(engaged).To(gomega.BeFalse(), "a refused registration must engage no cluster")
		})
	}
}

// TestCreateAndEngageClusterRefusesAFileBackedCredential closes the same path as
// the exec guard, without the fork. clientcmd resolves tokenFile,
// client-certificate, client-key and certificate-authority against the operator
// pod's OWN filesystem, so a registration naming
// /var/run/secrets/kubernetes.io/serviceaccount/token and a server the author
// controls hands this operator's ServiceAccount token to that server on the first
// LIST — and that token reads every other registration Secret in the namespace,
// which turns one namespaced Secret create into the whole fleet's kubeconfigs.
// The refusal is before cluster.New, so no transport that could send one is built.
func TestCreateAndEngageClusterRefusesAFileBackedCredential(t *testing.T) {
	tests := []struct {
		name       string
		kubeconfig func(dir string) []byte
	}{
		{
			name: "token file",
			kubeconfig: func(dir string) []byte {
				return credentialKubeconfig("      tokenFile: " + filepath.Join(dir, "token") + "\n")
			},
		},
		{
			name: "client certificate and key files",
			kubeconfig: func(dir string) []byte {
				return credentialKubeconfig(
					"      client-certificate: " + filepath.Join(dir, "tls.crt") + "\n" +
						"      client-key: " + filepath.Join(dir, "tls.key") + "\n")
			},
		},
		{
			name: "certificate authority file",
			kubeconfig: func(dir string) []byte {
				return registrationKubeconfig(
					"      certificate-authority: "+filepath.Join(dir, "ca.crt")+"\n",
					"      token: an-inline-token\n")
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			// clientcmd reads or at least opens each of these while it builds the
			// rest.Config, so they have to exist here — as they do in the pod the
			// registration is aimed at.
			dir := t.TempDir()
			for _, name := range []string{"token", "tls.crt", "tls.key", "ca.crt"} {
				g.Expect(os.WriteFile(filepath.Join(dir, name), []byte("stand-in"), 0o600)).To(gomega.Succeed())
			}

			p := NewKubeconfigProvider(KubeconfigProviderOptions{Namespace: "c5c3-clusters"})

			err := p.createAndEngageCluster(t.Context(), "hostile-target",
				tc.kubeconfig(dir), nil, "hash", logr.Discard())

			g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(
				`refusing the kubeconfig of cluster "hostile-target"`)))
			g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(
				"file-backed credentials")))
			_, engaged := p.getCluster("hostile-target")
			g.Expect(engaged).To(gomega.BeFalse(), "a refused registration must engage no cluster")
		})
	}
}

// TestCreateAndEngageClusterRefusesAFileBackedCredentialWithoutReadingIt pins
// WHERE that guard has to run. Building the rest.Config already acts on the path:
// clientcmd os.ReadFile()s tokenFile and os.Open()s client-certificate and
// certificate-authority while it resolves the credential. A guard that inspects
// the finished rest.Config is therefore unreachable for exactly the inputs whose
// read never returns — tokenFile: /dev/zero grows os.ReadFile's buffer until the
// operator is OOM-killed, a named pipe with no writer blocks the registration
// controller's only worker forever, and the same Secret does it again after every
// restart.
//
// Neither of those can be exercised safely from a unit test. A path that does not
// exist stands in for them, because it fails in the same place — inside clientcmd's
// read, before any guard on its result could run — so a refusal that names the
// file-backed credentials is only reachable if nothing touched the path.
func TestCreateAndEngageClusterRefusesAFileBackedCredentialWithoutReadingIt(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-file")

	tests := []struct {
		name       string
		kubeconfig []byte
	}{
		{
			name:       "token file",
			kubeconfig: credentialKubeconfig("      tokenFile: " + missing + "\n"),
		},
		{
			name: "client certificate and key files",
			kubeconfig: credentialKubeconfig(
				"      client-certificate: " + missing + "\n" +
					"      client-key: " + missing + "\n"),
		},
		{
			name: "certificate authority file",
			kubeconfig: registrationKubeconfig(
				"      certificate-authority: "+missing+"\n",
				"      token: an-inline-token\n"),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			p := NewKubeconfigProvider(KubeconfigProviderOptions{Namespace: "c5c3-clusters"})

			err := p.createAndEngageCluster(t.Context(), "hostile-target",
				tc.kubeconfig, nil, "hash", logr.Discard())

			g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(
				`refusing the kubeconfig of cluster "hostile-target"`)))
			g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(
				"file-backed credentials")))
			_, engaged := p.getCluster("hostile-target")
			g.Expect(engaged).To(gomega.BeFalse(), "a refused registration must engage no cluster")
		})
	}
}

// A credential plugin is refused whichever users[] entry carries it. Which
// context is current is the registration author's choice, so a guard that only
// looked at the current one would be a field away from being bypassed.
func TestCreateAndEngageClusterRefusesAnExecCredentialOutsideTheCurrentContext(t *testing.T) {
	g := gomega.NewWithT(t)

	kubeconfig := append(credentialKubeconfig("      token: an-inline-token\n"),
		[]byte(`  - name: unused
    user:
      exec:
        apiVersion: client.authentication.k8s.io/v1
        command: /bin/sh
        args: ["-c", "cat /var/run/secrets/kubernetes.io/serviceaccount/token"]
        interactiveMode: Never
`)...)

	p := NewKubeconfigProvider(KubeconfigProviderOptions{Namespace: "c5c3-clusters"})

	err := p.createAndEngageCluster(t.Context(), "hostile-target",
		kubeconfig, nil, "hash", logr.Discard())

	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(
		"exec and auth-provider credentials are not supported")))
	_, engaged := p.getCluster("hostile-target")
	g.Expect(engaged).To(gomega.BeFalse(), "a refused registration must engage no cluster")
}
