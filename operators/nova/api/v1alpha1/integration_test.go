// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package v1alpha1

import (
	"context"
	"fmt"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	"github.com/c5c3/cobaltcore/operators/nova/internal/testutil"
)

// --- Helpers ---

// setupEnvTest wraps testutil.SetupNovaEnvTest with the v1alpha1 scheme
// registration and the webhook setup, avoiding the import cycle between testutil
// and this package. The webhook manifests envtest installs carry both kinds
// with failurePolicy=Fail, so both handlers must be served or admission of
// every Nova and NovaCompute fails.
func setupEnvTest(t testing.TB) (client.Client, context.Context, context.CancelFunc) {
	t.Helper()
	return testutil.SetupNovaEnvTest(t, AddToScheme, func(mgr ctrl.Manager) error {
		// mgr.GetAPIReader() mirrors production wiring in main.go: webhook
		// admission lookups (the PriorityClass existence check) read the API server
		// directly, never a stale informer cache.
		if err := (&NovaWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr); err != nil {
			return err
		}
		return (&NovaComputeWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr)
	})
}

// setupEnvTestNoWebhook wraps testutil.SetupNovaEnvTestNoWebhook with the
// v1alpha1 scheme registration. Used by the CRD-only tests so no webhook can mask
// a missing CEL rule or supply a default the CRD schema must supply itself.
func setupEnvTestNoWebhook(t testing.TB) (client.Client, context.Context, context.CancelFunc) {
	t.Helper()
	return testutil.SetupNovaEnvTestNoWebhook(t, AddToScheme)
}

// newNamespace creates a uniquely named namespace for a test.
func newNamespace(t testing.TB, ctx context.Context, c client.Client, prefix string) string {
	t.Helper()
	g := NewGomegaWithT(t)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: prefix}}
	g.Expect(c.Create(ctx, ns)).To(Succeed(), "create namespace")
	return ns.Name
}

// integrationNova returns validNova() stamped with name/namespace for API server
// submission. validNova() already carries every field the CRD marks required, so
// nothing has to be filled in here.
func integrationNova(name, namespace string) *Nova {
	nova := validNova()
	nova.Name = name
	nova.Namespace = namespace
	return nova
}

// minimalNovaManifest returns the unstructured manifest of a Nova carrying only
// the fields no default supplies: no deployment block, no worker count, no
// console block, and no cache backend. The backend has to stay out: the
// webhook's typed round trip writes an empty one into a request that lacks it,
// which satisfies the schema's required marker, so only the value read back
// shows whether the defaulter ran.
//
// The CR is submitted unstructured because that is the only way to express an
// absent block: the metadata, scheduler and conductor blocks hold non-pointer
// Deployment structs, so a typed client serializes "deployment: {}" for each of
// them however empty they are, and the API server then fills the schema default
// of three replicas before any webhook runs. That is the case the one-replica
// defaults exist for, and it is unreachable through the typed client.
func minimalNovaManifest(name, namespace string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": GroupVersion.String(),
		"kind":       "Nova",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
		},
		"spec": map[string]any{
			"openStackRelease": "2025.2",
			"image": map[string]any{
				"repository": "ghcr.io/c5c3/nova",
				"tag":        "2025.2",
			},
			"apiDatabase": map[string]any{
				"clusterRef": map[string]any{"name": "mariadb"},
				"database":   "nova_api",
				"secretRef":  map[string]any{"name": "nova-api-db"},
			},
			"database": map[string]any{
				"clusterRef": map[string]any{"name": "mariadb"},
				"database":   "nova",
				"secretRef":  map[string]any{"name": "nova-db"},
			},
			"cache": map[string]any{
				"clusterRef": map[string]any{"name": "memcached"},
			},
			"messaging": map[string]any{
				"clusterRef": map[string]any{"name": "rabbitmq"},
			},
			"metadata": map[string]any{
				"sharedSecretRef": map[string]any{"name": "nova-metadata-secret"},
			},
			"keystoneEndpoint": "http://keystone.openstack.svc.cluster.local:5000/v3",
			"serviceUser": map[string]any{
				"secretRef": map[string]any{"name": "nova-service-password"},
			},
		},
	}}
}

// expectRejected asserts that err is an API-server rejection carrying want.
func expectRejected(t testing.TB, err error, want string) {
	t.Helper()
	g := NewGomegaWithT(t)
	g.Expect(err).To(HaveOccurred())
	g.Expect(apierrors.IsInvalid(err) || apierrors.IsForbidden(err)).To(BeTrue(),
		fmt.Sprintf("expected Invalid or Forbidden status error, got: %v", err))
	g.Expect(err.Error()).To(ContainSubstring(want))
}

// --- CRD-level CEL / schema enforcement (no validating webhook installed) ---

// TestIntegration_CRD_CELOnly_RejectsSameSchema pins the two-schema rule: nova
// splits its state across the nova_api tables and the cell tables, each with its
// own migration tree, so one schema holding both installs two migration
// histories into it and neither db-sync can be replayed afterwards.
func TestIntegration_CRD_CELOnly_RejectsSameSchema(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)
	ns := newNamespace(t, ctx, c, "same-schema-")

	nova := integrationNova("nova", ns)
	nova.Spec.APIDatabase.Database = nova.Spec.Database.Database
	expectRejected(t, c.Create(ctx, nova), "apiDatabase and database must name different schemas")
}

// TestIntegration_CRD_CELOnly_RejectsCredentialsModeMismatch pins the credential
// half of the same pairing: both schemas are reached over one credential path, so
// a deployment cannot issue one of them engine-issued logins and the other a
// static password.
func TestIntegration_CRD_CELOnly_RejectsCredentialsModeMismatch(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)
	ns := newNamespace(t, ctx, c, "credentials-mode-")

	nova := integrationNova("nova", ns)
	nova.Spec.APIDatabase.CredentialsMode = commonv1.CredentialsModeDynamic
	expectRejected(t, c.Create(ctx, nova), "apiDatabase and database must use the same credentialsMode")
}

// TestIntegration_CRD_CELOnly_AcceptsExplicitAndOmittedStaticCredentialsMode pins
// the has() default on both sides of the same pairing rule: an omitted
// credentialsMode means Static, so a CR that spells Static out on one block and
// omits it on the other names one mode. A side that read the field without the
// default would fail on the absent field and reject that CR at the schema layer,
// which no webhook can overrule.
func TestIntegration_CRD_CELOnly_AcceptsExplicitAndOmittedStaticCredentialsMode(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)

	for _, tc := range []struct {
		name string
		edit func(*Nova)
	}{
		{
			name: "explicit on apiDatabase",
			edit: func(n *Nova) { n.Spec.APIDatabase.CredentialsMode = commonv1.CredentialsModeStatic },
		},
		{
			name: "explicit on database",
			edit: func(n *Nova) { n.Spec.Database.CredentialsMode = commonv1.CredentialsModeStatic },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			ns := newNamespace(t, ctx, c, "credentials-mode-static-")

			nova := integrationNova("nova", ns)
			tc.edit(nova)
			g.Expect(c.Create(ctx, nova)).To(Succeed(),
				"an explicit Static beside an omitted credentialsMode should be accepted")
		})
	}
}

// TestIntegration_CRD_CELOnly_RejectsConsoleDeploymentWhenDisabled pins the
// console pairing rule and its admitted counterpart: a disabled proxy has no
// Deployment to size, while a disabled proxy without a block is an ordinary
// posture. The pair is what makes the pointer on spec.consoleProxy.deployment
// observable at the schema layer: a value block would serialize as an empty
// object on every CR and make the rule fire on a deployment nobody wrote.
func TestIntegration_CRD_CELOnly_RejectsConsoleDeploymentWhenDisabled(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)

	t.Run("with a deployment block", func(t *testing.T) {
		ns := newNamespace(t, ctx, c, "console-disabled-")

		nova := integrationNova("nova", ns)
		nova.Spec.ConsoleProxy.Enabled = ptr.To(false)
		nova.Spec.ConsoleProxy.Deployment = &DeploymentSpec{Replicas: 1}
		expectRejected(t, c.Create(ctx, nova),
			"consoleProxy.deployment must not be set when consoleProxy.enabled is false")
	})

	t.Run("without a deployment block", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ns := newNamespace(t, ctx, c, "console-disabled-ok-")

		nova := integrationNova("nova", ns)
		nova.Spec.ConsoleProxy.Enabled = ptr.To(false)
		g.Expect(c.Create(ctx, nova)).To(Succeed(),
			"a disabled console proxy without a deployment block should be accepted")
	})
}

// TestIntegration_CRD_CELOnly_RejectsDatabaseChange pins the transition rules
// on both database blocks against a schema rename and a mode flip. The cell
// mappings in nova_api store the schema names literally, so a rename re-points
// the processes at an empty schema while the mapping keeps naming the old one.
// The rules sit at the schema layer so the guarantee holds while the validating
// webhook is down.
func TestIntegration_CRD_CELOnly_RejectsDatabaseChange(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)

	for _, tc := range []struct {
		name string
		edit func(*Nova)
		want string
	}{
		{
			name: "apiDatabase schema renamed",
			edit: func(n *Nova) { n.Spec.APIDatabase.Database = "nova_api_new" },
			want: "apiDatabase.database is immutable",
		},
		{
			name: "cell schema renamed",
			edit: func(n *Nova) { n.Spec.Database.Database = "nova_new" },
			want: "database.database is immutable: the cell mappings store the schema name",
		},
		{
			name: "apiDatabase moved to a host",
			edit: func(n *Nova) {
				n.Spec.APIDatabase.ClusterRef = nil
				n.Spec.APIDatabase.Host = "db.example.com"
			},
			want: "apiDatabase mode (managed clusterRef vs brownfield host) is immutable",
		},
		{
			name: "cell database moved to a host",
			edit: func(n *Nova) {
				n.Spec.Database.ClusterRef = nil
				n.Spec.Database.Host = "db.example.com"
			},
			want: "database mode (managed clusterRef vs brownfield host) is immutable",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			ns := newNamespace(t, ctx, c, "database-immutable-")

			g.Expect(c.Create(ctx, integrationNova("nova", ns))).To(Succeed(), "valid Nova should be accepted")

			got := &Nova{}
			g.Expect(c.Get(ctx, types.NamespacedName{Name: "nova", Namespace: ns}, got)).To(Succeed())
			tc.edit(got)
			expectRejected(t, c.Update(ctx, got), tc.want)
		})
	}
}

// TestIntegration_CRD_CELOnly_Cell0SchemaRules pins the two rules cell0 adds to
// the pairing: the API block must not name "<database>_cell0", and the cell
// schema name must leave room for the suffix under the 64-character limit, with
// the longest admissible name on the accepted side of the bound.
func TestIntegration_CRD_CELOnly_Cell0SchemaRules(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)

	t.Run("apiDatabase naming cell0", func(t *testing.T) {
		ns := newNamespace(t, ctx, c, "cell0-collision-")
		nova := integrationNova("nova", ns)
		nova.Spec.APIDatabase.Database = nova.Spec.Database.Database + "_cell0"
		expectRejected(t, c.Create(ctx, nova), "apiDatabase must not name the cell0 schema derived from database")
	})

	t.Run("cell schema one character past the bound", func(t *testing.T) {
		ns := newNamespace(t, ctx, c, "cell0-bound-")
		nova := integrationNova("nova", ns)
		nova.Spec.Database.Database = strings.Repeat("n", 59)
		expectRejected(t, c.Create(ctx, nova), "database.database must be at most 58 characters")
	})

	t.Run("cell schema at the bound", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ns := newNamespace(t, ctx, c, "cell0-bound-ok-")
		nova := integrationNova("nova", ns)
		nova.Spec.Database.Database = strings.Repeat("n", 58)
		g.Expect(c.Create(ctx, nova)).To(Succeed())
	})
}

// TestIntegration_CRD_CELOnly_RejectsTargetClusterRefChange pins the two
// targetClusterRef transition rules against all three edits: adding the ref,
// removing it, and renaming it. Moving a service between clusters is not a
// supported mutation, and the rules are enforced at the schema layer so the
// guarantee holds while the validating webhook is down.
func TestIntegration_CRD_CELOnly_RejectsTargetClusterRefChange(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)

	for _, tc := range []struct {
		name    string
		initial *commonv1.TargetClusterRefSpec
		edit    func(*Nova)
	}{
		{
			name: "added",
			edit: func(n *Nova) { n.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "cluster-a"} },
		},
		{
			name:    "removed",
			initial: &commonv1.TargetClusterRefSpec{Name: "cluster-a"},
			edit:    func(n *Nova) { n.Spec.TargetClusterRef = nil },
		},
		{
			name:    "renamed",
			initial: &commonv1.TargetClusterRefSpec{Name: "cluster-a"},
			edit:    func(n *Nova) { n.Spec.TargetClusterRef.Name = "cluster-b" },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			ns := newNamespace(t, ctx, c, "targetcluster-")

			nova := integrationNova("nova", ns)
			nova.Spec.TargetClusterRef = tc.initial
			g.Expect(c.Create(ctx, nova)).To(Succeed(), "valid Nova should be accepted")

			got := &Nova{}
			g.Expect(c.Get(ctx, types.NamespacedName{Name: "nova", Namespace: ns}, got)).To(Succeed())
			tc.edit(got)
			expectRejected(t, c.Update(ctx, got), "targetClusterRef is immutable")
		})
	}
}

// TestIntegration_CRD_CELOnly_OpenStackReleasePattern pins the openStackRelease
// pattern at the CRD level. The [12] minor class keeps the pattern, the
// validating webhook and release.ParseRelease in agreement, so a non-cadence
// minor is rejected before any of them has to explain itself.
func TestIntegration_CRD_CELOnly_OpenStackReleasePattern(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)

	for _, tc := range []struct {
		release string
		accept  bool
	}{
		{release: "2025.2", accept: true},
		{release: "2025.9"},
	} {
		t.Run(tc.release, func(t *testing.T) {
			g := NewGomegaWithT(t)
			ns := newNamespace(t, ctx, c, "release-pattern-")

			nova := integrationNova("nova", ns)
			nova.Spec.OpenStackRelease = tc.release

			err := c.Create(ctx, nova)
			if tc.accept {
				g.Expect(err).NotTo(HaveOccurred(), "release %q should be accepted", tc.release)
				return
			}
			expectRejected(t, err, "openStackRelease")
		})
	}
}

// TestIntegration_CRD_CELOnly_RejectsEndpointOverridePattern pins the
// ^https?:// pattern every endpoint override carries. An override is handed to a
// client library verbatim, so a scheme nova cannot speak would surface as a
// failed instance boot rather than at admission.
func TestIntegration_CRD_CELOnly_RejectsEndpointOverridePattern(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)
	ns := newNamespace(t, ctx, c, "endpoint-pattern-")

	nova := integrationNova("nova", ns)
	nova.Spec.Endpoints.Placement.Override = "ftp://placement.openstack.svc:8778"
	expectRejected(t, c.Create(ctx, nova), "spec.endpoints.placement.override")
}

// TestIntegration_CRD_CELOnly_RemoteComputeRequiresMessagingTLS pins the rule
// that ties the remote compute contract to a verified bus. A compute on another
// cluster reaches the broker across a cluster boundary, and the messaging CA
// bundle is the only trust anchor the contract carries.
func TestIntegration_CRD_CELOnly_RemoteComputeRequiresMessagingTLS(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)
	ns := newNamespace(t, ctx, c, "remote-compute-tls-")

	plaintext := integrationNova("nova-plaintext", ns)
	plaintext.Spec.RemoteCompute = &NovaRemoteComputeSpec{
		KeystoneEndpoint:      "https://keystone.example.com/v3",
		TransportURLSecretRef: commonv1.SecretRefSpec{Name: "nova-remote-transport"},
	}
	expectRejected(t, c.Create(ctx, plaintext), "remoteCompute requires messaging.tls")

	verified := plaintext.DeepCopy()
	verified.Name = "nova-verified"
	verified.Spec.Messaging.TLS = &commonv1.MessagingTLSSpec{
		CABundleSecretRef: commonv1.SecretRefSpec{Name: "rabbitmq-ca", Key: "ca.crt"},
	}
	NewGomegaWithT(t).Expect(c.Create(ctx, verified)).To(Succeed(),
		"the same remote block on a verified bus is admitted")
}

// TestIntegration_CRD_CELOnly_RemoteComputeFieldMarkers pins the two field
// markers of the remote block: the Keystone URL uses https, and the transport
// URL Secret has a name.
func TestIntegration_CRD_CELOnly_RemoteComputeFieldMarkers(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)
	ns := newNamespace(t, ctx, c, "remote-compute-markers-")

	remote := func(name string) *Nova {
		nova := integrationNova(name, ns)
		nova.Spec.Messaging.TLS = &commonv1.MessagingTLSSpec{
			CABundleSecretRef: commonv1.SecretRefSpec{Name: "rabbitmq-ca", Key: "ca.crt"},
		}
		nova.Spec.RemoteCompute = &NovaRemoteComputeSpec{
			KeystoneEndpoint:      "https://keystone.example.com/v3",
			TransportURLSecretRef: commonv1.SecretRefSpec{Name: "nova-remote-transport"},
		}
		return nova
	}

	noScheme := remote("nova-no-scheme")
	noScheme.Spec.RemoteCompute.KeystoneEndpoint = "keystone.example.com"
	err := c.Create(ctx, noScheme)
	expectRejected(t, err, "spec.remoteCompute.keystoneEndpoint")
	expectRejected(t, err, "should match")

	plaintext := remote("nova-plaintext-keystone")
	plaintext.Spec.RemoteCompute.KeystoneEndpoint = "http://keystone.example.com/v3"
	err = c.Create(ctx, plaintext)
	expectRejected(t, err, "spec.remoteCompute.keystoneEndpoint")
	expectRejected(t, err, "should match '^https://'")

	unnamed := remote("nova-unnamed")
	unnamed.Spec.RemoteCompute.TransportURLSecretRef.Name = ""
	err = c.Create(ctx, unnamed)
	expectRejected(t, err, "spec.remoteCompute.transportURLSecretRef.name")
	expectRejected(t, err, "should be at least 1 chars long")
}

// --- Live admission round-trip (webhooks running) ---

// TestIntegration_WebhookDefaultsServiceUser proves the mutating webhook fills
// the service-user identity defaults and the secretRef key on a CR that supplies
// only the password Secret name.
func TestIntegration_WebhookDefaultsServiceUser(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTest(t)
	ns := newNamespace(t, ctx, c, "serviceuser-defaults-")

	nova := integrationNova("nova", ns)
	nova.Spec.ServiceUser = ServiceUserSpec{
		SecretRef: commonv1.SecretRefSpec{Name: "nova-service-password"},
	}
	g.Expect(c.Create(ctx, nova)).To(Succeed(), "minimal Nova should be accepted after defaults")

	got := &Nova{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "nova", Namespace: ns}, got)).To(Succeed())
	g.Expect(got.Spec.ServiceUser.Username).To(Equal("nova"))
	g.Expect(got.Spec.ServiceUser.ProjectName).To(Equal("service"))
	g.Expect(got.Spec.ServiceUser.UserDomainName).To(Equal("Default"))
	g.Expect(got.Spec.ServiceUser.ProjectDomainName).To(Equal("Default"))
	g.Expect(got.Spec.ServiceUser.SecretRef.Key).To(Equal("password"))
}

// TestIntegration_WebhookDefaultsAbsentDeploymentBlocks proves the absent-block
// path through the real API server: a manifest that names none of the five
// Deployment blocks comes back with one replica on the four non-API ones, the
// raised termination window on the two processes that drain an RPC server, two
// workers each, a projected console proxy, and the default cache backend.
//
// The disabled-console case is the second half of the same proof. It travels
// through the full admission chain (defaulting, then validation, then the CEL
// rules) and keeps spec.consoleProxy.deployment nil, which is the reason that
// field is a pointer: a materialized block would be the shape the console rule
// rejects, produced by the operator itself.
func TestIntegration_WebhookDefaultsAbsentDeploymentBlocks(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTest(t)

	t.Run("console proxy enabled", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ns := newNamespace(t, ctx, c, "deployment-defaults-")

		g.Expect(c.Create(ctx, minimalNovaManifest("nova", ns))).
			To(Succeed(), "a manifest without deployment blocks should be accepted")

		got := &Nova{}
		g.Expect(c.Get(ctx, types.NamespacedName{Name: "nova", Namespace: ns}, got)).To(Succeed())
		g.Expect(got.Spec.API.Deployment.Replicas).To(Equal(commonv1.DefaultReplicas))
		g.Expect(got.Spec.Metadata.Deployment.Replicas).To(Equal(int32(1)))
		g.Expect(got.Spec.Scheduler.Deployment.Replicas).To(Equal(int32(1)))
		g.Expect(got.Spec.Conductor.Deployment.Replicas).To(Equal(int32(1)))
		g.Expect(got.Spec.ConsoleProxy.Enabled).To(Equal(ptr.To(true)))
		g.Expect(got.Spec.ConsoleProxy.Deployment).NotTo(BeNil(),
			"an enabled console proxy gets a materialized deployment block")
		g.Expect(got.Spec.ConsoleProxy.Deployment.Replicas).To(Equal(int32(1)))
		g.Expect(got.Spec.Scheduler.Deployment.TerminationGracePeriodSeconds).
			To(Equal(ptr.To(DefaultRPCTerminationGracePeriodSeconds)))
		g.Expect(got.Spec.Conductor.Deployment.TerminationGracePeriodSeconds).
			To(Equal(ptr.To(DefaultRPCTerminationGracePeriodSeconds)))
		g.Expect(got.Spec.Scheduler.Workers).To(Equal(ptr.To(DefaultWorkers)))
		g.Expect(got.Spec.Conductor.Workers).To(Equal(ptr.To(DefaultWorkers)))
		g.Expect(got.Spec.Cache.Backend).To(Equal(commonv1.DefaultCacheBackend))
	})

	t.Run("console proxy disabled", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ns := newNamespace(t, ctx, c, "console-disabled-defaults-")

		manifest := minimalNovaManifest("nova", ns)
		spec, _, err := unstructured.NestedMap(manifest.Object, "spec")
		g.Expect(err).NotTo(HaveOccurred())
		spec["consoleProxy"] = map[string]any{"enabled": false}
		g.Expect(unstructured.SetNestedMap(manifest.Object, spec, "spec")).To(Succeed())

		g.Expect(c.Create(ctx, manifest)).To(Succeed(),
			"a disabled console proxy without a deployment block should clear the whole admission chain")

		got := &Nova{}
		g.Expect(c.Get(ctx, types.NamespacedName{Name: "nova", Namespace: ns}, got)).To(Succeed())
		g.Expect(got.Spec.ConsoleProxy.Enabled).To(Equal(ptr.To(false)))
		g.Expect(got.Spec.ConsoleProxy.Deployment).To(BeNil(),
			"the defaulting webhook must not materialize the block the console rule rejects")
	})
}

// TestIntegration_WebhookAdmitsDisablingTheConsoleProxy proves the disable
// patch a user actually sends goes through: the mutating webhook materialized a
// console deployment block on create, a merge patch setting only enabled to
// false leaves that block in the request, and the webhook removes it before the
// console rule on NovaSpec measures it.
func TestIntegration_WebhookAdmitsDisablingTheConsoleProxy(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTest(t)
	ns := newNamespace(t, ctx, c, "console-disable-")

	g.Expect(c.Create(ctx, integrationNova("nova", ns))).To(Succeed())
	got := &Nova{}
	key := types.NamespacedName{Name: "nova", Namespace: ns}
	g.Expect(c.Get(ctx, key, got)).To(Succeed())
	g.Expect(got.Spec.ConsoleProxy.Deployment).NotTo(BeNil(), "the webhook materializes the block on create")

	patch := client.RawPatch(types.MergePatchType, []byte(`{"spec":{"consoleProxy":{"enabled":false}}}`))
	g.Expect(c.Patch(ctx, got, patch)).To(Succeed(), "disabling the console proxy must not be rejected")

	g.Expect(c.Get(ctx, key, got)).To(Succeed())
	g.Expect(got.Spec.ConsoleProxy.Enabled).To(HaveValue(BeFalse()))
	g.Expect(got.Spec.ConsoleProxy.Deployment).To(BeNil())
}

// TestIntegration_WebhookDefaultsSharedSecretKey proves the metadata shared
// secret is read from its documented key when the CR names only the Secret. The
// same value has to be configured on the Neutron side, so the operator reads it
// rather than generating it, and the key is the one half it can supply itself.
func TestIntegration_WebhookDefaultsSharedSecretKey(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTest(t)
	ns := newNamespace(t, ctx, c, "sharedsecret-defaults-")

	nova := integrationNova("nova", ns)
	nova.Spec.Metadata.SharedSecretRef = commonv1.SecretRefSpec{Name: "nova-metadata-secret"}
	g.Expect(c.Create(ctx, nova)).To(Succeed(), "a sharedSecretRef without a key should be accepted")

	got := &Nova{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "nova", Namespace: ns}, got)).To(Succeed())
	g.Expect(got.Spec.Metadata.SharedSecretRef.Key).To(Equal(DefaultSharedSecretKey))
}

// TestIntegration_WebhookRejectsUnknownExtraConfigOption pins the extraConfig
// option-catalog check through the real API server: a CR whose extraConfig names
// an option the release's catalog does not carry is rejected by the validating
// webhook. "cpu_allocation_ration" is a typo for the [DEFAULT]
// cpu_allocation_ratio option, which the rendered config would accept silently
// and nova would ignore.
func TestIntegration_WebhookRejectsUnknownExtraConfigOption(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTest(t)
	ns := newNamespace(t, ctx, c, "extraconfig-catalog-")

	nova := integrationNova("nova", ns)
	nova.Spec.ExtraConfig = map[string]map[string]string{
		"DEFAULT": {"cpu_allocation_ration": "16.0"},
	}
	expectRejected(t, c.Create(ctx, nova), "no such option in the nova 2025.2 option catalog")
}

// TestIntegration_WebhookRejectsOverlongName pins the metadata.name bound
// through the real API server. The bound is the archive CronJob's: Kubernetes
// caps a CronJob name at MaxCronJobNameLength characters, and the operator
// appends "-db-archive" to metadata.name to build it, so a name one character
// past the bound would produce a CronJob the API server refuses on every
// reconcile.
func TestIntegration_WebhookRejectsOverlongName(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTest(t)
	ns := newNamespace(t, ctx, c, "name-length-")

	name := strings.Repeat("n", MaxNovaNameLength+1)
	expectRejected(t, c.Create(ctx, integrationNova(name, ns)),
		fmt.Sprintf("Kubernetes caps CronJob names at %d characters", MaxCronJobNameLength))
}

// --- NovaCompute ---

// integrationNovaCompute returns a NovaCompute carrying every field the CRD
// marks required: the Nova it joins and a one-label node selector.
func integrationNovaCompute(name, namespace string) *NovaCompute {
	return &NovaCompute{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: NovaComputeSpec{
			NovaRef:      NovaRef{Name: "nova"},
			NodeSelector: map[string]string{"openstack.c5c3.io/nova-compute-pool": "a"},
		},
	}
}

// TestIntegration_NovaCompute_CRD_CELOnly_Defaults pins the two schema defaults
// the controller reads without a webhook to fill them: virtType kvm and the
// RollingUpdate strategy.
func TestIntegration_NovaCompute_CRD_CELOnly_Defaults(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)
	g := NewGomegaWithT(t)
	ns := newNamespace(t, ctx, c, "novacompute-defaults-")

	g.Expect(c.Create(ctx, integrationNovaCompute("pool", ns))).To(Succeed())

	got := &NovaCompute{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "pool", Namespace: ns}, got)).To(Succeed())
	g.Expect(got.Spec.Libvirt.VirtType).To(Equal("kvm"))
	g.Expect(got.Spec.UpdateStrategy.Type).To(Equal("RollingUpdate"))
	g.Expect(got.Spec.UpdateStrategy.MaxUnavailable).To(BeNil())
}

// TestIntegration_NovaCompute_CRD_CELOnly_RejectsInvalidSpecs pins the create-time
// schema rules: the one-label selector minimum, the two enums, and the
// cpuMode/cpuModels pairing in both directions.
func TestIntegration_NovaCompute_CRD_CELOnly_RejectsInvalidSpecs(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)

	for _, tc := range []struct {
		name string
		edit func(*NovaCompute)
		want string
	}{
		{
			name: "empty node selector",
			edit: func(nc *NovaCompute) { nc.Spec.NodeSelector = map[string]string{} },
			want: "spec.nodeSelector",
		},
		{
			name: "empty novaRef name",
			edit: func(nc *NovaCompute) { nc.Spec.NovaRef.Name = "" },
			want: "spec.novaRef.name",
		},
		{
			name: "virtType xen",
			edit: func(nc *NovaCompute) { nc.Spec.Libvirt.VirtType = "xen" },
			want: "Unsupported value",
		},
		{
			name: "imagesType rbd",
			edit: func(nc *NovaCompute) { nc.Spec.Libvirt.ImagesType = "rbd" },
			want: "Unsupported value",
		},
		{
			name: "cpuModels without custom",
			edit: func(nc *NovaCompute) {
				nc.Spec.Libvirt.CPUMode = "host-model"
				nc.Spec.Libvirt.CPUModels = []string{"Haswell"}
			},
			want: "cpuModels is required when cpuMode is custom and must be empty otherwise",
		},
		{
			name: "custom without cpuModels",
			edit: func(nc *NovaCompute) { nc.Spec.Libvirt.CPUMode = "custom" },
			want: "cpuModels is required when cpuMode is custom and must be empty otherwise",
		},
		{
			name: "cpu model with a space",
			edit: func(nc *NovaCompute) {
				nc.Spec.Libvirt.CPUMode = "custom"
				nc.Spec.Libvirt.CPUModels = []string{"Haswell noTSX"}
			},
			want: "spec.libvirt.cpuModels[0]",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ns := newNamespace(t, ctx, c, "novacompute-invalid-")
			nc := integrationNovaCompute("pool", ns)
			tc.edit(nc)
			expectRejected(t, c.Create(ctx, nc), tc.want)
		})
	}

	t.Run("custom with cpuModels is admitted", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ns := newNamespace(t, ctx, c, "novacompute-custom-")
		nc := integrationNovaCompute("pool", ns)
		nc.Spec.Libvirt.CPUMode = "custom"
		nc.Spec.Libvirt.CPUModels = []string{"Haswell-noTSX", "Skylake-Client"}
		g.Expect(c.Create(ctx, nc)).To(Succeed())
	})
}

// TestIntegration_NovaCompute_CRD_CELOnly_Transitions pins the three transition
// rules: novaRef and targetClusterRef are frozen, while the node selector, the
// drain trigger, stays mutable.
func TestIntegration_NovaCompute_CRD_CELOnly_Transitions(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)

	for _, tc := range []struct {
		name    string
		initial *commonv1.TargetClusterRefSpec
		edit    func(*NovaCompute)
		want    string
	}{
		{
			name: "novaRef renamed",
			edit: func(nc *NovaCompute) { nc.Spec.NovaRef.Name = "other" },
			want: "novaRef is immutable",
		},
		{
			name: "targetClusterRef added",
			edit: func(nc *NovaCompute) {
				nc.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "compute-a"}
			},
			want: "targetClusterRef is immutable",
		},
		{
			name:    "targetClusterRef removed",
			initial: &commonv1.TargetClusterRefSpec{Name: "compute-a"},
			edit:    func(nc *NovaCompute) { nc.Spec.TargetClusterRef = nil },
			want:    "targetClusterRef is immutable",
		},
		{
			name:    "targetClusterRef renamed",
			initial: &commonv1.TargetClusterRefSpec{Name: "compute-a"},
			edit:    func(nc *NovaCompute) { nc.Spec.TargetClusterRef.Name = "compute-b" },
			want:    "targetClusterRef is immutable",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			ns := newNamespace(t, ctx, c, "novacompute-transition-")

			nc := integrationNovaCompute("pool", ns)
			nc.Spec.TargetClusterRef = tc.initial
			g.Expect(c.Create(ctx, nc)).To(Succeed())

			got := &NovaCompute{}
			g.Expect(c.Get(ctx, types.NamespacedName{Name: "pool", Namespace: ns}, got)).To(Succeed())
			tc.edit(got)
			expectRejected(t, c.Update(ctx, got), tc.want)
		})
	}

	t.Run("nodeSelector change is admitted", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ns := newNamespace(t, ctx, c, "novacompute-selector-")
		g.Expect(c.Create(ctx, integrationNovaCompute("pool", ns))).To(Succeed())

		got := &NovaCompute{}
		g.Expect(c.Get(ctx, types.NamespacedName{Name: "pool", Namespace: ns}, got)).To(Succeed())
		got.Spec.NodeSelector = map[string]string{"openstack.c5c3.io/nova-compute-pool": "b"}
		g.Expect(c.Update(ctx, got)).To(Succeed())
	})
}

// TestIntegration_NovaCompute_WebhookRejectsOverlongName pins the metadata.name
// bound through the real API server: the name is the instance label of every
// child, and Kubernetes caps a label value at 63 characters.
func TestIntegration_NovaCompute_WebhookRejectsOverlongName(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTest(t)
	ns := newNamespace(t, ctx, c, "novacompute-name-")

	name := strings.Repeat("p", MaxNovaComputeNameLength+1)
	expectRejected(t, c.Create(ctx, integrationNovaCompute(name, ns)),
		"name must be at most 63 characters")
}

// TestIntegration_NovaCompute_WebhookAdmitsSelectorChange pins that a relabel,
// the drain trigger, passes both admission layers on update.
func TestIntegration_NovaCompute_WebhookAdmitsSelectorChange(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTest(t)
	g := NewGomegaWithT(t)
	ns := newNamespace(t, ctx, c, "novacompute-relabel-")

	g.Expect(c.Create(ctx, integrationNovaCompute("pool", ns))).To(Succeed())
	got := &NovaCompute{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "pool", Namespace: ns}, got)).To(Succeed())
	got.Spec.NodeSelector = map[string]string{"openstack.c5c3.io/nova-compute-pool": "b"}
	g.Expect(c.Update(ctx, got)).To(Succeed())
}

// TestIntegration_NovaCompute_WebhookCatalogCheck pins the extraConfig catalog
// check against the referenced Nova's release through the real API server, and
// the admission of the same overlay while that Nova is absent. The warning the
// second case carries is pinned by the unit tests.
func TestIntegration_NovaCompute_WebhookCatalogCheck(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTest(t)

	t.Run("absent Nova", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ns := newNamespace(t, ctx, c, "novacompute-catalog-absent-")

		nc := integrationNovaCompute("pool", ns)
		nc.Spec.ExtraConfig = map[string]map[string]string{"DEFAULT": {"cpu_allocation_ration": "16.0"}}
		g.Expect(c.Create(ctx, nc)).To(Succeed(), "an absent Nova skips the catalog check")
	})

	t.Run("present Nova", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ns := newNamespace(t, ctx, c, "novacompute-catalog-present-")
		g.Expect(c.Create(ctx, integrationNova("nova", ns))).To(Succeed())

		nc := integrationNovaCompute("pool", ns)
		nc.Spec.ExtraConfig = map[string]map[string]string{"DEFAULT": {"cpu_allocation_ration": "16.0"}}
		expectRejected(t, c.Create(ctx, nc), "no such option in the nova 2025.2 option catalog")
	})
}
