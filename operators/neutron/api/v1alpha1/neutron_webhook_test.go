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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
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

// novaNotifier returns a spec.nova block that passes validation, for the tests
// that mutate exactly one of its values.
func novaNotifier() *NovaSpec {
	return &NovaSpec{
		Region: "RegionOne",
		ServiceUser: NovaNotifierUserSpec{
			Username:          "neutron-nova",
			ProjectName:       "service",
			UserDomainName:    "Default",
			ProjectDomainName: "Default",
			SecretRef:         commonv1.SecretRefSpec{Name: "neutron-nova-notifier", Key: "password"},
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
	// Resources are resolved when the Deployments are rendered, never written
	// into the CR.
	g.Expect(obj.Spec.Deployment.Replicas).To(gomega.Equal(commonv1.DefaultReplicas))
	g.Expect(obj.Spec.Deployment.Resources).To(gomega.BeNil())
	g.Expect(obj.Spec.Workers.Deployment.Replicas).To(gomega.Equal(commonv1.DefaultReplicas))
	g.Expect(obj.Spec.Workers.Deployment.Resources).To(gomega.BeNil())
	g.Expect(obj.Spec.Cache.Backend).To(gomega.Equal(commonv1.DefaultCacheBackend))
	g.Expect(obj.Spec.Logging).NotTo(gomega.BeNil())
	g.Expect(obj.Spec.Logging.Format).To(gomega.Equal("text"))
	g.Expect(obj.Spec.Logging.Level).To(gomega.Equal("INFO"))
}

// The Nova notifier identity is defaulted inside a present spec.nova alone. A
// nil block is what keeps the port notifications off, so the defaulter must not
// materialize one: a CR that named no Nova would otherwise start notifying a
// compute service it has no credentials for.
func TestNeutronDefault_NovaNotifierServiceUser(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &NeutronWebhook{}

	obj := validNeutron()
	obj.Spec.Nova = &NovaSpec{
		ServiceUser: NovaNotifierUserSpec{
			SecretRef: commonv1.SecretRefSpec{Name: "neutron-nova-notifier"},
		},
	}

	g.Expect(w.Default(context.Background(), obj)).To(gomega.Succeed())

	g.Expect(obj.Spec.Nova.ServiceUser.Username).To(gomega.Equal("neutron-nova"))
	g.Expect(obj.Spec.Nova.ServiceUser.ProjectName).To(gomega.Equal("service"))
	g.Expect(obj.Spec.Nova.ServiceUser.UserDomainName).To(gomega.Equal("Default"))
	g.Expect(obj.Spec.Nova.ServiceUser.ProjectDomainName).To(gomega.Equal("Default"))
	g.Expect(obj.Spec.Nova.ServiceUser.SecretRef.Key).To(gomega.Equal("password"))
	// The region has no default: an empty one omits [nova] region_name so the
	// notifier follows the Keystone catalog.
	g.Expect(obj.Spec.Nova.Region).To(gomega.BeEmpty())

	// An explicit identity survives the defaulter untouched.
	explicit := validNeutron()
	explicit.Spec.Nova = &NovaSpec{
		Region: "RegionTwo",
		ServiceUser: NovaNotifierUserSpec{
			Username:  "compute-notifier",
			SecretRef: commonv1.SecretRefSpec{Name: "neutron-nova-notifier", Key: "notifier-password"},
		},
	}
	g.Expect(w.Default(context.Background(), explicit)).To(gomega.Succeed())
	g.Expect(explicit.Spec.Nova.ServiceUser.Username).To(gomega.Equal("compute-notifier"))
	g.Expect(explicit.Spec.Nova.ServiceUser.SecretRef.Key).To(gomega.Equal("notifier-password"))

	// A CR that names no Nova keeps none.
	absent := validNeutron()
	g.Expect(w.Default(context.Background(), absent)).To(gomega.Succeed())
	g.Expect(absent.Spec.Nova).To(gomega.BeNil())
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

	// A configured Nova notifier is accepted as well: the required-ref check
	// fires on an unnamed Secret alone, not on the presence of the block.
	withNova := validNeutron()
	withNova.Spec.Nova = novaNotifier()
	_, err = w.ValidateCreate(context.Background(), withNova)
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
		// A spec.nova block with an unnamed Secret has no password to notify with.
		{
			name: "nova serviceUser secretRef name empty rejected",
			mutate: func(o *Neutron) {
				o.Spec.Nova = &NovaSpec{ServiceUser: NovaNotifierUserSpec{}}
			},
			wantSub: "serviceUser.secretRef.name must be set when spec.nova is configured: " +
				"it carries the password Neutron notifies Nova with",
		},
		// The region and the four identity fields reach [nova] verbatim, so a
		// newline in any of them appends a line to the section the notifier reads.
		{
			name: "nova region with a newline rejected",
			mutate: func(o *Neutron) {
				o.Spec.Nova = novaNotifier()
				o.Spec.Nova.Region = "RegionOne\npassword = hunter2"
			},
			wantSub: "must not contain a newline or carriage return",
		},
		{
			name: "nova serviceUser username with a newline rejected",
			mutate: func(o *Neutron) {
				o.Spec.Nova = novaNotifier()
				o.Spec.Nova.ServiceUser.Username = "neutron-nova\npassword = hunter2"
			},
			wantSub: "must not contain a newline or carriage return",
		},
		{
			name: "nova serviceUser projectName with a newline rejected",
			mutate: func(o *Neutron) {
				o.Spec.Nova = novaNotifier()
				o.Spec.Nova.ServiceUser.ProjectName = "service\npassword = hunter2"
			},
			wantSub: "must not contain a newline or carriage return",
		},
		{
			name: "nova serviceUser userDomainName with a newline rejected",
			mutate: func(o *Neutron) {
				o.Spec.Nova = novaNotifier()
				o.Spec.Nova.ServiceUser.UserDomainName = "Default\npassword = hunter2"
			},
			wantSub: "must not contain a newline or carriage return",
		},
		{
			name: "nova serviceUser projectDomainName with a newline rejected",
			mutate: func(o *Neutron) {
				o.Spec.Nova = novaNotifier()
				o.Spec.Nova.ServiceUser.ProjectDomainName = "Default\npassword = hunter2"
			},
			wantSub: "must not contain a newline or carriage return",
		},
		// The notifier password is env-injected via OS_NOVA__PASSWORD, so a file
		// value is inert at runtime and only copies the credential into the
		// rendered config Secret every pod mounts.
		{
			name: "extraConfig setting the nova notifier password rejected",
			mutate: func(o *Neutron) {
				o.Spec.ExtraConfig = map[string]map[string]string{
					"nova": {"password": "hunter2"},
				}
			},
			wantSub: "password is managed via spec.nova.serviceUser.secretRef",
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
// only ever fire against a CR a pre-upgrade operator already admitted, and it
// would refuse every update to that CR with no field left to edit to repair it.
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
		"an over-long grandfathered CR must stay updatable, since its name cannot be edited to comply")
}

// The finalizer removal reconcileDelete issues is an update, and it passes the
// defaulting webhook before the validating one. A spec the webhook admitted
// earlier can fail today's rules (a topology-spread constraint over the name and
// instance pair alone, a PriorityClass deleted since), and rejecting the removal
// would hold the CR in Terminating. A default the defaulter fills on the removal
// alone, because the CR was last written before an operator release added it, is
// no spec change either. A deleting CR whose spec changes is still validated.
func TestNeutronValidateUpdate_FinalizerRemovalOnADeletingCRSkipsValidation(t *testing.T) {
	ctx := context.Background()
	w := &NeutronWebhook{}

	deleting := func() *Neutron {
		obj := validNeutron()
		obj.Spec.Deployment.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{
			spreadOn("kubernetes.io/hostname", wideSpreadLabels()),
		}
		obj.Finalizers = []string{"neutron.openstack.c5c3.io/finalizer"}
		obj.DeletionTimestamp = ptr.To(metav1.Now())
		return obj
	}
	// release returns the finalizer removal as the validating webhook receives
	// it: the stored object without its finalizer, after the defaulting webhook.
	release := func(g gomega.Gomega, stored *Neutron) *Neutron {
		released := stored.DeepCopy()
		released.Finalizers = nil
		g.Expect(w.Default(ctx, released)).To(gomega.Succeed())
		return released
	}

	t.Run("a spec the defaulter filled on create", func(t *testing.T) {
		g := gomega.NewWithT(t)
		stale := deleting()
		g.Expect(w.Default(ctx, stale)).To(gomega.Succeed())

		warnings, err := w.ValidateUpdate(ctx, stale, release(g, stale))
		g.Expect(warnings).To(gomega.BeNil())
		g.Expect(err).NotTo(gomega.HaveOccurred(),
			"the finalizer removal must pass however the unchanged spec fares against today's rules")
	})

	// validNeutron() carries neither spec.logging nor the service-user names, so
	// the defaulter fills them on the removal alone.
	t.Run("a spec stored before a default existed", func(t *testing.T) {
		g := gomega.NewWithT(t)
		stale := deleting()

		_, err := w.ValidateUpdate(ctx, stale, release(g, stale))
		g.Expect(err).NotTo(gomega.HaveOccurred(),
			"a default filled on the finalizer removal alone must not count as a spec change")
	})

	t.Run("a spec edit on a deleting CR", func(t *testing.T) {
		g := gomega.NewWithT(t)
		stale := deleting()
		g.Expect(w.Default(ctx, stale)).To(gomega.Succeed())
		edited := release(g, stale)
		edited.Spec.Deployment.Replicas = 5

		_, err := w.ValidateUpdate(ctx, stale, edited)
		g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(
			"labelSelector.matchLabels must equal the Deployment selector labels")),
			"a spec edit on a deleting CR is validated like any other")
	})
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

// --- spec.deployment.topologySpreadConstraints ---

// apiSpreadLabels returns the pod selector of the API Deployment of
// validNeutron(), and wideSpreadLabels the name and instance pair that pod
// shares with the worker and ovn-db-sync pods. Both are spelled out rather than
// built from naming.APISelectorLabels, so a renamed constant cannot make the
// tests below agree with themselves.
func apiSpreadLabels() map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":      "neutron",
		"app.kubernetes.io/instance":  "test-neutron",
		"app.kubernetes.io/component": "api",
	}
}

func wideSpreadLabels() map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":     "neutron",
		"app.kubernetes.io/instance": "test-neutron",
	}
}

// spreadOn returns one topology-spread constraint over topologyKey selecting
// the given labels.
func spreadOn(topologyKey string, labels map[string]string) corev1.TopologySpreadConstraint {
	return corev1.TopologySpreadConstraint{
		MaxSkew:           1,
		TopologyKey:       topologyKey,
		WhenUnsatisfiable: corev1.ScheduleAnyway,
		LabelSelector:     &metav1.LabelSelector{MatchLabels: labels},
	}
}

// spec.deployment configures the API Deployment alone, so a custom
// topology-spread constraint has to name that Deployment's pod selector: the
// name and instance labels narrowed by app.kubernetes.io/component=api. The pair
// without the component also matches the pods of the two worker Deployments and
// of the ovn-db-sync CronJob, and would measure the API pods' skew over pods
// they do not control. The rejection message carries the selector the webhook
// required, which is what the component assertion pins.
func TestNeutronValidate_TopologySpreadSelectorNamesTheAPIComponent(t *testing.T) {
	const (
		selectorPath = "spec.deployment.topologySpreadConstraints[0].labelSelector"
		mismatch     = "labelSelector.matchLabels must equal the Deployment selector labels"
		apiComponent = "app.kubernetes.io/component:api"
	)

	withLabel := func(labels map[string]string, key, value string) map[string]string {
		labels[key] = value
		return labels
	}
	withExpression := func(tsc corev1.TopologySpreadConstraint) corev1.TopologySpreadConstraint {
		tsc.LabelSelector.MatchExpressions = []metav1.LabelSelectorRequirement{{
			Key:      "tier",
			Operator: metav1.LabelSelectorOpExists,
		}}
		return tsc
	}

	tests := []struct {
		name string
		tscs []corev1.TopologySpreadConstraint
		// wantSubs is nil when the constraints are admitted.
		wantSubs   []string
		wantAbsent []string
	}{
		{
			name: "the API selector admitted",
			tscs: []corev1.TopologySpreadConstraint{spreadOn("kubernetes.io/hostname", apiSpreadLabels())},
		},
		{
			name:     "the name and instance pair rejected",
			tscs:     []corev1.TopologySpreadConstraint{spreadOn("kubernetes.io/hostname", wideSpreadLabels())},
			wantSubs: []string{selectorPath, mismatch, apiComponent},
		},
		{
			name: "a worker selector rejected",
			tscs: []corev1.TopologySpreadConstraint{spreadOn("kubernetes.io/hostname",
				withLabel(wideSpreadLabels(), "app.kubernetes.io/component", "periodic-workers"))},
			wantSubs: []string{selectorPath, mismatch, apiComponent},
		},
		{
			name: "a fourth label rejected",
			tscs: []corev1.TopologySpreadConstraint{spreadOn("kubernetes.io/hostname",
				withLabel(apiSpreadLabels(), "app.kubernetes.io/managed-by", "neutron-operator"))},
			wantSubs: []string{selectorPath, mismatch, apiComponent},
		},
		{
			name: "matchExpressions beside the API selector rejected",
			tscs: []corev1.TopologySpreadConstraint{
				withExpression(spreadOn("kubernetes.io/hostname", apiSpreadLabels())),
			},
			wantSubs: []string{
				selectorPath + ".matchExpressions",
				"matchExpressions are not allowed; labelSelector must use matchLabels only",
			},
			wantAbsent: []string{mismatch},
		},
		// The empty list names no selector. It only switches the injected
		// defaults off.
		{
			name: "an empty list admitted",
			tscs: []corev1.TopologySpreadConstraint{},
		},
		{
			name: "only the offending index rejected",
			tscs: []corev1.TopologySpreadConstraint{
				spreadOn("topology.kubernetes.io/zone", apiSpreadLabels()),
				spreadOn("kubernetes.io/hostname", wideSpreadLabels()),
			},
			wantSubs:   []string{"spec.deployment.topologySpreadConstraints[1].labelSelector", mismatch},
			wantAbsent: []string{"topologySpreadConstraints[0]"},
		},
		{
			name: "a nil labelSelector rejected",
			tscs: []corev1.TopologySpreadConstraint{{
				MaxSkew:           1,
				TopologyKey:       "kubernetes.io/hostname",
				WhenUnsatisfiable: corev1.ScheduleAnyway,
			}},
			wantSubs: []string{selectorPath, "labelSelector is required on each TopologySpreadConstraint"},
		},
		{
			name: "an empty labelSelector rejected",
			tscs: []corev1.TopologySpreadConstraint{{
				MaxSkew:           1,
				TopologyKey:       "kubernetes.io/hostname",
				WhenUnsatisfiable: corev1.ScheduleAnyway,
				LabelSelector:     &metav1.LabelSelector{},
			}},
			wantSubs: []string{selectorPath, mismatch},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			obj := validNeutron()
			obj.Spec.Deployment.TopologySpreadConstraints = tc.tscs

			_, err := (&NeutronWebhook{}).ValidateCreate(context.Background(), obj)
			if tc.wantSubs == nil {
				g.Expect(err).NotTo(gomega.HaveOccurred())
				return
			}
			g.Expect(apierrors.IsInvalid(err)).To(gomega.BeTrue(), "want an Invalid error, got %v", err)
			for _, sub := range tc.wantSubs {
				g.Expect(err.Error()).To(gomega.ContainSubstring(sub))
			}
			for _, sub := range tc.wantAbsent {
				g.Expect(err.Error()).NotTo(gomega.ContainSubstring(sub))
			}
		})
	}

	// The PriorityClass lookup's NotFound and the selector mismatch land in one
	// response, so the author sees both at once.
	t.Run("a wide selector and a missing PriorityClass aggregate", func(t *testing.T) {
		g := gomega.NewWithT(t)
		obj := validNeutron()
		obj.Spec.Deployment.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{
			spreadOn("kubernetes.io/hostname", wideSpreadLabels()),
		}
		obj.Spec.Deployment.PriorityClassName = ptr.To("nonexistent-class")

		_, err := (&NeutronWebhook{Client: newFakeClient().Build()}).ValidateCreate(context.Background(), obj)
		g.Expect(apierrors.IsInvalid(err)).To(gomega.BeTrue(), "want an Invalid error, got %v", err)
		g.Expect(err.Error()).To(gomega.ContainSubstring(selectorPath))
		g.Expect(err.Error()).To(gomega.ContainSubstring("spec.deployment.priorityClassName"))
	})
}

// The component requirement is a hard switch. A stored CR that still carries
// the name and instance pair takes no update until its constraint names the API
// selector, and that includes an update leaving the spec alone while the CR is
// not being deleted.
func TestNeutronValidateUpdate_WideTopologySpreadSelectorRejected(t *testing.T) {
	stored := func() *Neutron {
		obj := validNeutron()
		obj.Spec.Deployment.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{
			spreadOn("kubernetes.io/hostname", wideSpreadLabels()),
		}
		return obj
	}

	t.Run("a replica change is rejected", func(t *testing.T) {
		g := gomega.NewWithT(t)
		updated := stored()
		updated.Spec.Deployment.Replicas = 5

		_, err := (&NeutronWebhook{}).ValidateUpdate(context.Background(), stored(), updated)
		g.Expect(apierrors.IsInvalid(err)).To(gomega.BeTrue(), "want an Invalid error, got %v", err)
		g.Expect(err.Error()).To(gomega.ContainSubstring("app.kubernetes.io/component:api"))
	})

	t.Run("an unchanged spec on a live CR is rejected", func(t *testing.T) {
		g := gomega.NewWithT(t)
		updated := stored()
		updated.Labels = map[string]string{"team": "network"}

		_, err := (&NeutronWebhook{}).ValidateUpdate(context.Background(), stored(), updated)
		g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(
			"labelSelector.matchLabels must equal the Deployment selector labels")),
			"only a CR that is being deleted skips validation of an unchanged spec")
	})

	t.Run("replacing the pair with the API selector is admitted", func(t *testing.T) {
		g := gomega.NewWithT(t)
		updated := stored()
		updated.Spec.Deployment.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{
			spreadOn("kubernetes.io/hostname", apiSpreadLabels()),
		}

		_, err := (&NeutronWebhook{}).ValidateUpdate(context.Background(), stored(), updated)
		g.Expect(err).NotTo(gomega.HaveOccurred())
	})
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

// TestNeutronValidateUpdate_CarriedRejectedKeyStaysUpdatable covers a Rejected
// key the stored CR was admitted with before the operator owned it: [nova]
// password was the documented way to configure the notifier before spec.nova
// existed. Refusing it on every update would refuse the finalizer removal too, so
// a carried-over value is kept with a warning, while a new or changed one is
// still refused.
func TestNeutronValidateUpdate_CarriedRejectedKeyStaysUpdatable(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &NeutronWebhook{}

	stored := validNeutron()
	stored.Spec.ExtraConfig = map[string]map[string]string{
		"nova": {"password": "hunter2"},
	}

	// An unrelated edit, and the finalizer removal on delete, carry it over.
	scaled := stored.DeepCopy()
	scaled.Spec.Deployment.Replicas = 5
	warnings, err := w.ValidateUpdate(context.Background(), stored, scaled)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(warnings).To(gomega.ContainElement(gomega.And(
		gomega.ContainSubstring("spec.extraConfig[nova][password]"),
		gomega.ContainSubstring("managed via spec.nova.serviceUser.secretRef"),
	)))

	unfinalized := stored.DeepCopy()
	unfinalized.Finalizers = nil
	_, err = w.ValidateUpdate(context.Background(), stored, unfinalized)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// A changed value is a new override, and is refused like one.
	changed := stored.DeepCopy()
	changed.Spec.ExtraConfig["nova"]["password"] = "rotated"
	_, err = w.ValidateUpdate(context.Background(), stored, changed)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("password is managed via spec.nova.serviceUser.secretRef"))

	// So is one the stored object never carried.
	added := validNeutron()
	added.Spec.ExtraConfig = map[string]map[string]string{
		"nova": {"password": "hunter2"},
	}
	warnings, err = w.ValidateUpdate(context.Background(), validNeutron(), added)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("password is managed via spec.nova.serviceUser.secretRef"))
	g.Expect(warnings).To(gomega.BeEmpty())
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

// --- Node placement and spec.jobs validation ---

func TestNeutronValidate_NodePlacementRejected(t *testing.T) {
	for _, block := range []struct {
		name       string
		deployment func(o *Neutron) *commonv1.DeploymentSpec
		path       string
	}{
		{name: "spec.deployment", deployment: func(o *Neutron) *commonv1.DeploymentSpec { return &o.Spec.Deployment }, path: "spec.deployment"},
	} {
		for _, tc := range []struct {
			name   string
			mutate func(d *commonv1.DeploymentSpec)
			want   string
		}{
			{
				name:   "node selector key",
				mutate: func(d *commonv1.DeploymentSpec) { d.NodeSelector = map[string]string{"bad key": "x"} },
				want:   block.path + ".nodeSelector: Invalid value",
			},
			{
				name: "toleration without key or Exists",
				mutate: func(d *commonv1.DeploymentSpec) {
					d.Tolerations = []corev1.Toleration{{Operator: corev1.TolerationOpEqual}}
				},
				want: block.path + ".tolerations[0].operator: Invalid value",
			},
		} {
			t.Run(block.name+"/"+tc.name, func(t *testing.T) {
				g := gomega.NewWithT(t)
				o := validNeutron()
				tc.mutate(block.deployment(o))

				_, err := (&NeutronWebhook{}).ValidateCreate(context.Background(), o)
				g.Expect(err).To(gomega.HaveOccurred())
				g.Expect(err.Error()).To(gomega.ContainSubstring(tc.want))
			})
		}
	}
}

func TestNeutronValidate_JobsRejected(t *testing.T) {
	for _, tc := range []struct {
		name string
		jobs *commonv1.JobSpec
		want string
	}{
		{
			name: "unknown priority class",
			jobs: &commonv1.JobSpec{JobBaseSpec: commonv1.JobBaseSpec{PriorityClassName: ptr.To("typo")}},
			want: "spec.jobs.priorityClassName: Not found",
		},
		{
			name: "memory request above limit",
			jobs: &commonv1.JobSpec{JobBaseSpec: commonv1.JobBaseSpec{Resources: &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
				Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
			}}},
			want: "spec.jobs.resources.requests.memory: Invalid value",
		},
		{
			name: "node selector key",
			jobs: &commonv1.JobSpec{NodePlacementSpec: commonv1.NodePlacementSpec{NodeSelector: map[string]string{"bad key": "x"}}},
			want: "spec.jobs.nodeSelector: Invalid value",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			w := &NeutronWebhook{Client: fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build()}
			o := validNeutron()
			o.Spec.Jobs = tc.jobs

			_, err := w.ValidateCreate(context.Background(), o)
			g.Expect(err).To(gomega.HaveOccurred())
			g.Expect(err.Error()).To(gomega.ContainSubstring(tc.want))
		})
	}
}

func TestNeutronValidate_EmptyJobsAccepted(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &NeutronWebhook{Client: fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build()}
	o := validNeutron()
	o.Spec.Jobs = &commonv1.JobSpec{}

	_, err := w.ValidateCreate(context.Background(), o)
	g.Expect(err).NotTo(gomega.HaveOccurred())
}
