// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"context"
	"strings"
	"testing"

	"github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
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

// validPlacement returns a minimal Placement CR that passes every validation
// rule. Tests mutate single fields to exercise individual rules.
func validPlacement() *Placement {
	return &Placement{
		ObjectMeta: metav1.ObjectMeta{Name: "test-placement", Namespace: "openstack"},
		Spec: PlacementSpec{
			OpenStackRelease: "2025.2",
			Deployment:       DeploymentSpec{Replicas: 3},
			Image: commonv1.ImageSpec{
				Repository: "ghcr.io/c5c3/placement",
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
				SecretRef: commonv1.SecretRefSpec{Name: "placement-service-password", Key: "password"},
			},
		},
	}
}

// newFakeClient returns a controller-runtime fake client with the core
// scheduling API types registered. Additional objects can be pre-populated.
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

func TestPlacementDefault_MaterializesServiceUserAndLoggingDefaults(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &PlacementWebhook{}

	// Start from a CR whose service-user identity fields and secretRef key are
	// empty so the defaulter has to fill all five.
	obj := validPlacement()
	obj.Spec.ServiceUser = ServiceUserSpec{SecretRef: commonv1.SecretRefSpec{Name: "placement-service-password"}}

	g.Expect(w.Default(context.Background(), obj)).To(gomega.Succeed())

	g.Expect(obj.Spec.ServiceUser.Username).To(gomega.Equal("placement"))
	g.Expect(obj.Spec.ServiceUser.ProjectName).To(gomega.Equal("service"))
	g.Expect(obj.Spec.ServiceUser.UserDomainName).To(gomega.Equal("Default"))
	g.Expect(obj.Spec.ServiceUser.ProjectDomainName).To(gomega.Equal("Default"))
	g.Expect(obj.Spec.ServiceUser.SecretRef.Key).To(gomega.Equal("password"))

	// Shared-block defaults come along too; resources are resolved when the
	// Deployment is rendered, never written into the CR.
	g.Expect(obj.Spec.Deployment.Resources).To(gomega.BeNil())
	g.Expect(obj.Spec.Cache.Backend).To(gomega.Equal(commonv1.DefaultCacheBackend))
	g.Expect(obj.Spec.Logging).NotTo(gomega.BeNil())
	g.Expect(obj.Spec.Logging.Format).To(gomega.Equal("text"))
	g.Expect(obj.Spec.Logging.Level).To(gomega.Equal("INFO"))
}

// The webhook writes no resources: a nil block stays nil, an empty one stays
// empty, and an explicit one is left as written. The reconciler resolves the
// defaults when it renders the Deployment.
func TestPlacementDefault_LeavesResourcesAsWritten(t *testing.T) {
	for _, tc := range []struct {
		name      string
		resources *corev1.ResourceRequirements
	}{
		{name: "nil", resources: nil},
		{name: "empty", resources: &corev1.ResourceRequirements{}},
		{name: "explicit", resources: &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m"), corev1.ResourceMemory: resource.MustParse("1Gi")},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("2Gi")},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			w := &PlacementWebhook{}

			obj := validPlacement()
			obj.Spec.Deployment.Resources = tc.resources.DeepCopy()

			g.Expect(w.Default(context.Background(), obj)).To(gomega.Succeed())

			g.Expect(obj.Spec.Deployment.Resources).To(gomega.Equal(tc.resources))
		})
	}
}

func TestPlacementDefault_PreservesExplicitValues(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &PlacementWebhook{}

	obj := validPlacement()
	obj.Spec.ServiceUser = ServiceUserSpec{
		Username:          "placement-svc",
		UserDomainName:    "Corp",
		ProjectDomainName: "Corp",
		SecretRef:         commonv1.SecretRefSpec{Name: "custom-secret", Key: "custom-key"},
	}

	g.Expect(w.Default(context.Background(), obj)).To(gomega.Succeed())

	g.Expect(obj.Spec.ServiceUser.Username).To(gomega.Equal("placement-svc"))
	g.Expect(obj.Spec.ServiceUser.UserDomainName).To(gomega.Equal("Corp"))
	g.Expect(obj.Spec.ServiceUser.ProjectDomainName).To(gomega.Equal("Corp"))
	g.Expect(obj.Spec.ServiceUser.SecretRef.Key).To(gomega.Equal("custom-key"))
	// The one field left empty above is still filled, so an explicit value and a
	// defaulted one can coexist in the same block.
	g.Expect(obj.Spec.ServiceUser.ProjectName).To(gomega.Equal("service"))
}

func TestPlacementDefault_UWSGISemantics(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &PlacementWebhook{}

	// Nil apiServer: nothing is materialized, so the CR keeps tracking the
	// reconciler's defaults instead of freezing today's values.
	nilBlock := validPlacement()
	g.Expect(w.Default(context.Background(), nilBlock)).To(gomega.Succeed())
	g.Expect(nilBlock.Spec.APIServer).To(gomega.BeNil())

	// Present-but-zero uwsgi: processes/threads/httpKeepAlive filled.
	zero := validPlacement()
	zero.Spec.APIServer = &APIServerSpec{UWSGI: &UWSGISpec{}}
	g.Expect(w.Default(context.Background(), zero)).To(gomega.Succeed())
	g.Expect(zero.Spec.APIServer.UWSGI.Processes).To(gomega.Equal(commonv1.DefaultUWSGIProcesses))
	g.Expect(zero.Spec.APIServer.UWSGI.Threads).To(gomega.Equal(commonv1.DefaultUWSGIThreads))
	g.Expect(zero.Spec.APIServer.UWSGI.HTTPKeepAlive).To(gomega.HaveValue(gomega.BeTrue()))

	// An apiServer block without uwsgi is left alone: the nested pointer stays
	// nil rather than being invented.
	emptyAPIServer := validPlacement()
	emptyAPIServer.Spec.APIServer = &APIServerSpec{}
	g.Expect(w.Default(context.Background(), emptyAPIServer)).To(gomega.Succeed())
	g.Expect(emptyAPIServer.Spec.APIServer.UWSGI).To(gomega.BeNil())

	// Explicit httpKeepAlive=false is preserved (nil-preserving pointer).
	explicit := validPlacement()
	explicit.Spec.APIServer = &APIServerSpec{UWSGI: &UWSGISpec{
		Processes:     4,
		Threads:       2,
		HTTPKeepAlive: ptr.To(false),
	}}
	g.Expect(w.Default(context.Background(), explicit)).To(gomega.Succeed())
	g.Expect(explicit.Spec.APIServer.UWSGI.Processes).To(gomega.Equal(int32(4)))
	g.Expect(explicit.Spec.APIServer.UWSGI.Threads).To(gomega.Equal(int32(2)))
	g.Expect(explicit.Spec.APIServer.UWSGI.HTTPKeepAlive).To(gomega.HaveValue(gomega.BeFalse()))
}

// --- Validating webhook ---

func TestPlacementValidateCreate_ValidSpecAccepted(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &PlacementWebhook{}

	_, err := w.ValidateCreate(context.Background(), validPlacement())
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

// TestPlacementValidateCreate_RejectionTable covers the defense-in-depth rules
// that mirror a CRD marker or a CEL rule, plus the extraConfig shape checks no
// schema layer can express (CEL cannot constrain the keys of a
// preserve-unknown-fields map).
func TestPlacementValidateCreate_RejectionTable(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(o *Placement)
		wantSub string
	}{
		{
			name:    "zero replicas rejected",
			mutate:  func(o *Placement) { o.Spec.Deployment.Replicas = 0 },
			wantSub: "replicas must be at least 1",
		},
		{
			name: "image tag and digest both set rejected",
			mutate: func(o *Placement) {
				o.Spec.Image.Digest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			},
			wantSub: "exactly one of image.tag or image.digest",
		},
		{
			name:    "empty keystoneEndpoint rejected",
			mutate:  func(o *Placement) { o.Spec.KeystoneEndpoint = "" },
			wantSub: "keystoneEndpoint must be set",
		},
		{
			name:    "ftp keystoneEndpoint rejected",
			mutate:  func(o *Placement) { o.Spec.KeystoneEndpoint = "ftp://keystone" },
			wantSub: "scheme must be http or https",
		},
		{
			name:    "keystoneEndpoint without host rejected",
			mutate:  func(o *Placement) { o.Spec.KeystoneEndpoint = "http://" },
			wantSub: "must include a host",
		},
		{
			name:    "bad keystonePublicEndpoint rejected",
			mutate:  func(o *Placement) { o.Spec.KeystonePublicEndpoint = "ftp://keystone" },
			wantSub: "scheme must be http or https",
		},
		{
			name:    "unknown logging level rejected",
			mutate:  func(o *Placement) { o.Spec.Logging = &LoggingSpec{Level: "TRACE"} },
			wantSub: "logging.level",
		},
		{
			name: "unknown per-logger level rejected",
			mutate: func(o *Placement) {
				o.Spec.Logging = &LoggingSpec{PerLoggerLevels: map[string]string{"sqlalchemy": "TRACE"}}
			},
			wantSub: "level must be one of DEBUG, INFO, WARNING, ERROR, CRITICAL",
		},
		{
			name: "preStopSleepSeconds at the grace period rejected",
			mutate: func(o *Placement) {
				o.Spec.Deployment.TerminationGracePeriodSeconds = ptr.To(int64(30))
				o.Spec.Deployment.PreStopSleepSeconds = ptr.To(int64(30))
			},
			wantSub: "must be strictly less than terminationGracePeriodSeconds",
		},
		{
			name: "rollingUpdate under a Recreate strategy rejected",
			mutate: func(o *Placement) {
				o.Spec.Deployment.Strategy = &appsv1.DeploymentStrategy{
					Type:          appsv1.RecreateDeploymentStrategyType,
					RollingUpdate: &appsv1.RollingUpdateDeployment{},
				}
			},
			wantSub: "rollingUpdate must not be set when strategy.type is Recreate",
		},
		{
			name: "autoscaling without a utilization target rejected",
			mutate: func(o *Placement) {
				o.Spec.Autoscaling = &AutoscalingSpec{MaxReplicas: 5}
			},
			wantSub: "at least one of targetCPUUtilization or targetMemoryUtilization must be set",
		},
		{
			name: "autoscaling maxReplicas below the replica count rejected",
			mutate: func(o *Placement) {
				o.Spec.Autoscaling = &AutoscalingSpec{MaxReplicas: 2, TargetCPUUtilization: ptr.To(int32(80))}
			},
			wantSub: "maxReplicas must be >= spec.deployment.replicas (3)",
		},
		{
			name:    "networkPolicy without ingress rejected",
			mutate:  func(o *Placement) { o.Spec.NetworkPolicy = &NetworkPolicySpec{} },
			wantSub: "at least one ingress source must be specified",
		},
		{
			name:    "gateway without hostname rejected",
			mutate:  func(o *Placement) { o.Spec.Gateway = &GatewaySpec{} },
			wantSub: "hostname must be set when spec.gateway is configured",
		},
		{
			name: "memory request above the limit rejected",
			mutate: func(o *Placement) {
				o.Spec.Deployment.Resources = &corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")},
					Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
				}
			},
			wantSub: "memory request must not exceed limit (1Gi)",
		},
		{
			name: "empty extraConfig section rejected",
			mutate: func(o *Placement) {
				o.Spec.ExtraConfig = map[string]map[string]string{"": {"debug": "true"}}
			},
			wantSub: "extraConfig section name must not be empty",
		},
		{
			name: "empty extraConfig key rejected",
			mutate: func(o *Placement) {
				o.Spec.ExtraConfig = map[string]map[string]string{"DEFAULT": {"": "true"}}
			},
			wantSub: "extraConfig key must not be empty",
		},
		{
			name: "extraConfig section with a newline rejected",
			mutate: func(o *Placement) {
				o.Spec.ExtraConfig = map[string]map[string]string{
					"api]\n[placement_database": {"connection": "mysql+pymysql://"},
				}
			},
			wantSub: "extraConfig section name must not contain a newline or carriage return",
		},
		{
			name: "extraConfig value with a newline rejected",
			mutate: func(o *Placement) {
				o.Spec.ExtraConfig = map[string]map[string]string{
					"DEFAULT": {"debug": "true\n[placement_database]\nconnection = mysql+pymysql://"},
				}
			},
			wantSub: "must not contain a newline or carriage return",
		},
		// The typed spec fields below reach the same verbatim INI renderer as
		// extraConfig, so a newline in any of them smuggles a whole key past the
		// (section, key)-keyed ownership and catalog gates.
		{
			name: "region with a newline rejected",
			mutate: func(o *Placement) {
				o.Spec.Region = "RegionOne\nauth_url = http://attacker.example/v3"
			},
			wantSub: "spec.region",
		},
		{
			name: "serviceUser.username with a newline rejected",
			mutate: func(o *Placement) {
				o.Spec.ServiceUser.Username = "placement\nproject_name = admin"
			},
			wantSub: "spec.serviceUser.username",
		},
		{
			name: "serviceUser.projectDomainName with a carriage return rejected",
			mutate: func(o *Placement) {
				o.Spec.ServiceUser.ProjectDomainName = "Default\rregion_name = elsewhere"
			},
			wantSub: "spec.serviceUser.projectDomainName",
		},
		// spec.cache reaches the same renderer through cache.ResolveServers, which
		// derives [keystone_authtoken].memcached_servers from either mode.
		{
			name: "cache.servers entry with a newline rejected",
			mutate: func(o *Placement) {
				o.Spec.Cache.ClusterRef = nil
				o.Spec.Cache.Servers = []string{"mc-0:11211\nauth_url = http://attacker.example/v3"}
			},
			wantSub: "spec.cache.servers[0]",
		},
		{
			name: "cache.clusterRef.name with a newline rejected",
			mutate: func(o *Placement) {
				o.Spec.Cache.ClusterRef = &corev1.LocalObjectReference{
					Name: "memcached\nauth_url = http://attacker.example/v3",
				}
			},
			wantSub: "spec.cache.clusterRef.name",
		},
		{
			name: "perLoggerLevels logger name with a newline rejected",
			mutate: func(o *Placement) {
				o.Spec.Logging = &LoggingSpec{PerLoggerLevels: map[string]string{
					"sqlalchemy\nlog_config_append = /etc/placement/placement.conf": "INFO",
				}}
			},
			wantSub: "logger name must not contain a newline or carriage return",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			w := &PlacementWebhook{}
			obj := validPlacement()
			tt.mutate(obj)

			_, err := w.ValidateCreate(context.Background(), obj)
			g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(tt.wantSub)))
		})
	}
}

func TestPlacementValidateCreate_DatabaseXORRejected(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &PlacementWebhook{}
	obj := validPlacement()
	// Managed (clusterRef) and brownfield (host) at once.
	obj.Spec.Database.Host = "mariadb.example.com"

	_, err := w.ValidateCreate(context.Background(), obj)
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("exactly one of clusterRef or host")))
}

func TestPlacementValidateCreate_DynamicCredentialsWithoutClusterRefRejected(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &PlacementWebhook{}
	obj := validPlacement()
	obj.Spec.Database.ClusterRef = nil
	obj.Spec.Database.Host = "mariadb.example.com"
	obj.Spec.Database.CredentialsMode = commonv1.CredentialsModeDynamic

	_, err := w.ValidateCreate(context.Background(), obj)
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("credentialsMode Dynamic requires clusterRef")))
}

func TestPlacementValidateCreate_CacheXORRejected(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &PlacementWebhook{}
	obj := validPlacement()
	obj.Spec.Cache.Servers = []string{"memcached-0:11211"}

	_, err := w.ValidateCreate(context.Background(), obj)
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("exactly one of clusterRef or servers")))
}

func TestPlacementValidateCreate_EmptySecretStoreRefNameRejected(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &PlacementWebhook{}
	obj := validPlacement()
	obj.Spec.SecretStoreRef = &commonv1.SecretStoreRefSpec{Kind: commonv1.SecretStoreKindNamespaced}

	_, err := w.ValidateCreate(context.Background(), obj)
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("secretStoreRef.name")))
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("store name must be set")))
}

func TestPlacementValidateCreate_UnknownSecretStoreRefKindRejected(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &PlacementWebhook{}
	obj := validPlacement()
	obj.Spec.SecretStoreRef = &commonv1.SecretStoreRefSpec{Name: "openbao-tenant-store", Kind: "VaultStore"}

	_, err := w.ValidateCreate(context.Background(), obj)
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("secretStoreRef.kind")))
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("VaultStore")))
}

func TestPlacementValidateCreate_MissingPriorityClassRejected(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &PlacementWebhook{Client: newFakeClient().Build()}
	obj := validPlacement()
	obj.Spec.Deployment.PriorityClassName = ptr.To("nonexistent-class")

	_, err := w.ValidateCreate(context.Background(), obj)
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("priorityClassName")))
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("nonexistent-class")))
}

func TestPlacementValidateCreate_ExistingPriorityClassAccepted(t *testing.T) {
	g := gomega.NewWithT(t)
	pc := &schedulingv1.PriorityClass{
		ObjectMeta: metav1.ObjectMeta{Name: "system-cluster-critical"},
		Value:      1000000,
	}
	w := &PlacementWebhook{Client: newFakeClient(pc).Build()}
	obj := validPlacement()
	obj.Spec.Deployment.PriorityClassName = ptr.To("system-cluster-critical")

	_, err := w.ValidateCreate(context.Background(), obj)
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

func TestPlacementValidateCreate_TopologySpreadWrongSelectorRejected(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &PlacementWebhook{}
	obj := validPlacement()
	obj.Spec.Deployment.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{
		{
			MaxSkew:           1,
			TopologyKey:       "topology.kubernetes.io/zone",
			WhenUnsatisfiable: corev1.DoNotSchedule,
			LabelSelector: &metav1.LabelSelector{
				// Names the app but not this instance, so the constraint would
				// spread every Placement in the namespace as one group.
				MatchLabels: map[string]string{"app.kubernetes.io/name": "placement"},
			},
		},
	}

	_, err := w.ValidateCreate(context.Background(), obj)
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("labelSelector.matchLabels must equal the Deployment selector labels")))
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("test-placement")))
}

// TestPlacementValidateCreate_UWSGIShutdownEnvelope covers the two cross-field
// uWSGI rules: harakiri has to fit inside the drain window left between the
// preStop sleep and SIGKILL, and a keep-alive timeout is only reachable while
// keep-alive is on.
func TestPlacementValidateCreate_UWSGIShutdownEnvelope(t *testing.T) {
	t.Run("harakiri at or above the drain window rejected", func(t *testing.T) {
		g := gomega.NewWithT(t)
		w := &PlacementWebhook{}
		obj := validPlacement()
		// The defaults resolve to a 30s grace period and a 5s preStop sleep, so
		// the drain window is 25s.
		obj.Spec.APIServer = &APIServerSpec{UWSGI: &UWSGISpec{Harakiri: ptr.To(int32(25))}}

		_, err := w.ValidateCreate(context.Background(), obj)
		g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(
			"harakiri (25) must be strictly less than terminationGracePeriodSeconds - preStopSleepSeconds (25)",
		)))
	})

	t.Run("harakiri inside the drain window accepted", func(t *testing.T) {
		g := gomega.NewWithT(t)
		w := &PlacementWebhook{}
		obj := validPlacement()
		obj.Spec.APIServer = &APIServerSpec{UWSGI: &UWSGISpec{Harakiri: ptr.To(int32(24))}}

		_, err := w.ValidateCreate(context.Background(), obj)
		g.Expect(err).NotTo(gomega.HaveOccurred())
	})

	t.Run("httpKeepAliveTimeout with keep-alive off rejected", func(t *testing.T) {
		g := gomega.NewWithT(t)
		w := &PlacementWebhook{}
		obj := validPlacement()
		obj.Spec.APIServer = &APIServerSpec{UWSGI: &UWSGISpec{
			HTTPKeepAlive:        ptr.To(false),
			HTTPKeepAliveTimeout: ptr.To(int32(5)),
		}}

		_, err := w.ValidateCreate(context.Background(), obj)
		g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(
			"httpKeepAliveTimeout may only be set when httpKeepAlive is true",
		)))
	})

	t.Run("httpKeepAliveTimeout with keep-alive unset accepted", func(t *testing.T) {
		g := gomega.NewWithT(t)
		w := &PlacementWebhook{}
		obj := validPlacement()
		// A nil httpKeepAlive is "unset", which resolves to the default true, so
		// the timeout is reachable and must not be rejected.
		obj.Spec.APIServer = &APIServerSpec{UWSGI: &UWSGISpec{HTTPKeepAliveTimeout: ptr.To(int32(5))}}

		_, err := w.ValidateCreate(context.Background(), obj)
		g.Expect(err).NotTo(gomega.HaveOccurred())
	})
}

// TestPlacementValidateCreate_LongNameAccepted pins the deliberate absence of a
// metadata.name length bound. The sibling glance webhook caps the name at 52
// characters because its db-purge CronJob appends a suffix and the API server
// caps a CronJob name at 63; placement renders no CronJob, so any name
// Kubernetes accepts for the CR is valid here and the omission is by design, not
// an oversight.
func TestPlacementValidateCreate_LongNameAccepted(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &PlacementWebhook{}
	obj := validPlacement()
	obj.Name = strings.Repeat("p", 63)

	_, err := w.ValidateCreate(context.Background(), obj)
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

// --- extraConfig option-catalog validation ---

// TestPlacementValidate_ExtraConfigUnknownOptionRejected pins that an unknown
// option name under a known catalog section is rejected, and that the error
// names both the section and the option. "auth_strategy_typo" is a typo for the
// [api] auth_strategy option, so the catalog does not list it.
func TestPlacementValidate_ExtraConfigUnknownOptionRejected(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &PlacementWebhook{}
	obj := validPlacement()
	obj.Spec.ExtraConfig = map[string]map[string]string{
		"api": {"auth_strategy_typo": "keystone"},
	}

	_, err := w.ValidateCreate(context.Background(), obj)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("no such option in the placement 2025.2 option catalog"))
	g.Expect(err.Error()).To(gomega.ContainSubstring("api"))
	g.Expect(err.Error()).To(gomega.ContainSubstring("auth_strategy_typo"))
}

// TestPlacementValidate_ExtraConfigUnknownSectionRejected pins that an option
// under a section the catalog does not contain is rejected as an unknown
// section. "placement_databas" is a typo for "placement_database".
func TestPlacementValidate_ExtraConfigUnknownSectionRejected(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &PlacementWebhook{}
	obj := validPlacement()
	obj.Spec.ExtraConfig = map[string]map[string]string{
		"placement_databas": {"max_overflow": "10"},
	}

	_, err := w.ValidateCreate(context.Background(), obj)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("no such section in the placement 2025.2 option catalog"))
	g.Expect(err.Error()).To(gomega.ContainSubstring("placement_databas"))
}

// TestPlacementValidate_ExtraConfigDeprecatedOptionWarns pins that a
// deprecated-but-still-accepted option is honored (no error) but surfaces a
// warning naming its replacement. [DEFAULT] logfile is deprecated in favor of
// [DEFAULT] log_file.
func TestPlacementValidate_ExtraConfigDeprecatedOptionWarns(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &PlacementWebhook{}
	obj := validPlacement()
	obj.Spec.ExtraConfig = map[string]map[string]string{
		"DEFAULT": {"logfile": "/var/log/placement/placement.log"},
	}

	warnings, err := w.ValidateCreate(context.Background(), obj)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(warnings).To(gomega.ContainElement(gomega.ContainSubstring(
		"deprecated option in placement 2025.2, replaced by [DEFAULT] log_file",
	)))
}

// TestPlacementValidate_ExtraConfigWithoutOptionsAccepted pins that both shapes
// of "no extraConfig" skip the catalog entirely: neither a nil map nor an empty
// one produces an error or a warning.
func TestPlacementValidate_ExtraConfigWithoutOptionsAccepted(t *testing.T) {
	tests := []struct {
		name        string
		extraConfig map[string]map[string]string
	}{
		{name: "nil map", extraConfig: nil},
		{name: "empty map", extraConfig: map[string]map[string]string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			w := &PlacementWebhook{}
			obj := validPlacement()
			obj.Spec.ExtraConfig = tt.extraConfig

			warnings, err := w.ValidateCreate(context.Background(), obj)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(warnings).To(gomega.BeEmpty())
		})
	}
}

// TestPlacementValidate_ExtraConfigOwnedKeyExempt pins that an operator-owned
// (section, key) pair is exempt from the catalog scan even when the catalog
// section exists but does not list that key, and that the Rejected owned key is
// blocked instead of merely reported.
func TestPlacementValidate_ExtraConfigOwnedKeyExempt(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &PlacementWebhook{}

	exempt := validPlacement()
	exempt.Spec.ExtraConfig = map[string]map[string]string{
		"keystone_authtoken": {"username": "placement-svc"},
	}
	_, err := w.ValidateCreate(context.Background(), exempt)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	rejected := validPlacement()
	rejected.Spec.ExtraConfig = map[string]map[string]string{
		"keystone_authtoken": {"password": "hunter2"},
	}
	_, err = w.ValidateCreate(context.Background(), rejected)
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(
		"password is managed via spec.serviceUser.secretRef",
	)))
}

// TestPlacementValidate_ExtraConfigAuthStrategyRejected pins that neither
// spelling of auth_strategy can be set through extraConfig. extraConfig wins the
// merge, so honoring "noauth2" would put the API on the no-auth middleware —
// every request unauthenticated, project and role read from the x-auth-token
// header — and the ExtraConfigHealthy condition could only report it after the
// pods had already loaded the file. [DEFAULT] auth_strategy is the deprecated
// alias oslo.config still honors, so the rejection has to cover it too: the
// ownership and catalog guards both key on the exact (section, key) pair.
func TestPlacementValidate_ExtraConfigAuthStrategyRejected(t *testing.T) {
	for _, section := range []string{"api", "DEFAULT"} {
		t.Run(section, func(t *testing.T) {
			g := gomega.NewWithT(t)
			w := &PlacementWebhook{}
			obj := validPlacement()
			obj.Spec.ExtraConfig = map[string]map[string]string{
				section: {"auth_strategy": "noauth2"},
			}

			_, err := w.ValidateCreate(context.Background(), obj)
			g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(
				"auth_strategy is managed via operator-computed",
			)))
		})
	}
}

// TestPlacementValidate_ExtraConfigUnresolvableReleaseFailsOpen pins the
// fail-open behavior: a release the operator ships no catalog for, and a value
// that does not name a release at all, each yield one warning and no catalog
// error even though the option is unknown.
func TestPlacementValidate_ExtraConfigUnresolvableReleaseFailsOpen(t *testing.T) {
	t.Run("unknown release", func(t *testing.T) {
		g := gomega.NewWithT(t)
		w := &PlacementWebhook{}
		obj := validPlacement()
		obj.Spec.OpenStackRelease = "2027.1"
		obj.Spec.ExtraConfig = map[string]map[string]string{"api": {"auth_strategy_typo": "keystone"}}

		warnings, err := w.ValidateCreate(context.Background(), obj)
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(warnings).To(gomega.ContainElement(
			`spec.extraConfig was not validated against an option catalog: no catalog for release "2027.1" is embedded in this operator build`,
		))
	})

	t.Run("empty release", func(t *testing.T) {
		g := gomega.NewWithT(t)
		w := &PlacementWebhook{}
		obj := validPlacement()
		obj.Spec.OpenStackRelease = ""
		obj.Spec.ExtraConfig = map[string]map[string]string{"api": {"auth_strategy_typo": "keystone"}}

		warnings, err := w.ValidateCreate(context.Background(), obj)
		g.Expect(warnings).To(gomega.ContainElement(
			"spec.extraConfig was not validated against an option catalog: spec.openStackRelease does not name an OpenStack release",
		))
		if err != nil {
			g.Expect(err.Error()).NotTo(gomega.ContainSubstring("option catalog"))
		}
	})
}

// TestPlacementValidateUpdate_ExtraConfigCatalogGate exercises the UPDATE
// re-validation gate. The old object carries a since-invalidated extraConfig;
// the check re-runs only when extraConfig or spec.openStackRelease changes.
func TestPlacementValidateUpdate_ExtraConfigCatalogGate(t *testing.T) {
	w := &PlacementWebhook{}
	oldObj := func() *Placement {
		o := validPlacement()
		o.Spec.ExtraConfig = map[string]map[string]string{
			"api": {"auth_strategy_typo": "keystone"},
		}
		return o
	}

	t.Run("unrelated edit is accepted", func(t *testing.T) {
		g := gomega.NewWithT(t)
		old := oldObj()
		newObj := old.DeepCopy()
		newObj.Spec.Deployment.Replicas = 2

		_, err := w.ValidateUpdate(context.Background(), old, newObj)
		g.Expect(err).NotTo(gomega.HaveOccurred())
	})

	t.Run("extraConfig edit is re-validated and rejected", func(t *testing.T) {
		g := gomega.NewWithT(t)
		old := oldObj()
		newObj := old.DeepCopy()
		newObj.Spec.ExtraConfig = map[string]map[string]string{
			"api": {"another_typo": "x"},
		}

		_, err := w.ValidateUpdate(context.Background(), old, newObj)
		g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("no such option in the placement 2025.2 option catalog")))
	})

	t.Run("openStackRelease edit is re-validated and rejected", func(t *testing.T) {
		g := gomega.NewWithT(t)
		old := oldObj()
		newObj := old.DeepCopy()
		newObj.Spec.OpenStackRelease = "2026.1"

		_, err := w.ValidateUpdate(context.Background(), old, newObj)
		g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("no such option in the placement 2026.1 option catalog")))
	})
}

// --- spec.targetClusterRef (multicluster routing) ---

// TestPlacementValidateUpdate_TargetClusterRefAddedRejected covers the presence
// flip upwards: the children of a CR created without a target cluster live on
// the management cluster, so naming one afterwards is rejected.
func TestPlacementValidateUpdate_TargetClusterRefAddedRejected(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &PlacementWebhook{}
	old := validPlacement()
	newObj := validPlacement()
	newObj.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "edge-1"}

	_, err := w.ValidateUpdate(context.Background(), old, newObj)
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("targetClusterRef is immutable")))
}

// TestPlacementValidateUpdate_TargetClusterRefRemovedRejected covers the presence
// flip downwards: dropping the ref would strand the children on the cluster it
// named.
func TestPlacementValidateUpdate_TargetClusterRefRemovedRejected(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &PlacementWebhook{}
	old := validPlacement()
	old.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "edge-1"}
	newObj := validPlacement()

	_, err := w.ValidateUpdate(context.Background(), old, newObj)
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("targetClusterRef is immutable")))
}

// TestPlacementValidateUpdate_TargetClusterRefChangedRejected covers a rename,
// which would re-point the reconciler at a cluster that holds none of the
// children.
func TestPlacementValidateUpdate_TargetClusterRefChangedRejected(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &PlacementWebhook{}
	old := validPlacement()
	old.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "edge-1"}
	newObj := validPlacement()
	newObj.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "edge-2"}

	_, err := w.ValidateUpdate(context.Background(), old, newObj)
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("targetClusterRef is immutable")))
}

// TestPlacementValidateUpdate_TargetClusterRefUnchangedAccepted proves the check
// freezes only the ref: an unrelated edit on a CR that names a target cluster
// still passes.
func TestPlacementValidateUpdate_TargetClusterRefUnchangedAccepted(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &PlacementWebhook{}
	old := validPlacement()
	old.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "edge-1"}
	newObj := old.DeepCopy()
	newObj.Spec.Deployment.Replicas = 2

	_, err := w.ValidateUpdate(context.Background(), old, newObj)
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

// TestPlacementValidateCreate_EmptyTargetClusterRefNameRejected is the
// defense-in-depth twin of the MinLength marker: a present ref must name a
// cluster.
func TestPlacementValidateCreate_EmptyTargetClusterRefNameRejected(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &PlacementWebhook{}
	obj := validPlacement()
	obj.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: ""}

	_, err := w.ValidateCreate(context.Background(), obj)
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("targetClusterRef.name")))
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("target cluster name must be set")))
}

// --- Node placement and spec.jobs validation ---

func TestPlacementValidate_NodePlacementRejected(t *testing.T) {
	for _, block := range []struct {
		name       string
		deployment func(o *Placement) *commonv1.DeploymentSpec
		path       string
	}{
		{name: "spec.deployment", deployment: func(o *Placement) *commonv1.DeploymentSpec { return &o.Spec.Deployment }, path: "spec.deployment"},
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
				o := validPlacement()
				tc.mutate(block.deployment(o))

				_, err := (&PlacementWebhook{}).ValidateCreate(context.Background(), o)
				g.Expect(err).To(gomega.HaveOccurred())
				g.Expect(err.Error()).To(gomega.ContainSubstring(tc.want))
			})
		}
	}
}

func TestPlacementValidate_JobsRejected(t *testing.T) {
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
			w := &PlacementWebhook{Client: fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build()}
			o := validPlacement()
			o.Spec.Jobs = tc.jobs

			_, err := w.ValidateCreate(context.Background(), o)
			g.Expect(err).To(gomega.HaveOccurred())
			g.Expect(err.Error()).To(gomega.ContainSubstring(tc.want))
		})
	}
}

// The finalizer removal reconcileDelete issues is an update, and it passes the
// defaulting webhook before the validating one. A spec.jobs.priorityClassName
// admitted earlier can name a PriorityClass deleted since, and rejecting the
// removal would hold the CR in Terminating. The stored spec is left undefaulted,
// so a default the defaulter fills on the removal alone must not count as a
// spec change either. A deleting CR whose spec changes is still validated.
func TestPlacementValidateUpdate_FinalizerRemovalOnADeletingCRSkipsValidation(t *testing.T) {
	g := gomega.NewWithT(t)
	ctx := context.Background()
	w := &PlacementWebhook{Client: newFakeClient().Build()}

	stale := validPlacement()
	stale.Spec.Jobs = &commonv1.JobSpec{JobBaseSpec: commonv1.JobBaseSpec{PriorityClassName: ptr.To("deleted-class")}}
	stale.Finalizers = []string{"placement.openstack.c5c3.io/finalizer"}
	stale.DeletionTimestamp = ptr.To(metav1.Now())

	released := stale.DeepCopy()
	released.Finalizers = nil
	g.Expect(w.Default(ctx, released)).To(gomega.Succeed())

	_, err := w.ValidateUpdate(ctx, stale, released)
	g.Expect(err).NotTo(gomega.HaveOccurred(),
		"the finalizer removal must pass however the unchanged spec fares against today's rules")

	edited := released.DeepCopy()
	edited.Spec.Deployment.Replicas = 5
	_, err = w.ValidateUpdate(ctx, stale, edited)
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("spec.jobs.priorityClassName: Not found")),
		"a spec edit on a deleting CR is validated like any other")
}

func TestPlacementValidate_EmptyJobsAccepted(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &PlacementWebhook{Client: fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build()}
	o := validPlacement()
	o.Spec.Jobs = &commonv1.JobSpec{}

	_, err := w.ValidateCreate(context.Background(), o)
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

// TestPlacementValidate_AutoscalingTargetNeedsAPositiveRequest pins the HPA
// request check: the HorizontalPodAutoscaler divides the pods' usage by the
// sum of their containers' requests, so a zero CPU request under a CPU target
// is rejected at its field path, and the same CR without it is admitted.
func TestPlacementValidate_AutoscalingTargetNeedsAPositiveRequest(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &PlacementWebhook{}
	withTarget := func() *Placement {
		o := validPlacement()
		o.Spec.Autoscaling = &AutoscalingSpec{
			MinReplicas:          ptr.To(int32(1)),
			MaxReplicas:          5,
			TargetCPUUtilization: ptr.To(int32(80)),
		}
		return o
	}

	o := withTarget()
	o.Spec.Deployment.Resources = &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("0")},
	}
	_, err := w.ValidateCreate(context.Background(), o)
	g.Expect(apierrors.IsInvalid(err)).To(gomega.BeTrue(), "%v", err)
	g.Expect(err.Error()).To(gomega.ContainSubstring("spec.deployment.resources.requests.cpu"))
	g.Expect(err.Error()).To(gomega.ContainSubstring("cpu request must be greater than zero"))

	_, err = w.ValidateCreate(context.Background(), withTarget())
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

// TestPlacementValidate_AutoscalingTargetsAndBehavior pins the target and behavior
// checks. A CPU target above 100 is admitted, since a container without a CPU
// limit may use more CPU than it requests; a zero target is rejected at the
// lower bound. A memory target above 100 is rejected while no container of the
// API pod names a memory limit above its request, because the render-time
// default makes request and limit equal. A scale-down window above 3600 s is
// rejected at its path.
func TestPlacementValidate_AutoscalingTargetsAndBehavior(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &PlacementWebhook{}
	with := func(a *AutoscalingSpec) *Placement {
		o := validPlacement()
		o.Spec.Deployment.Resources = nil
		a.MinReplicas = ptr.To(int32(1))
		a.MaxReplicas = 5
		o.Spec.Autoscaling = a
		return o
	}

	_, err := w.ValidateCreate(context.Background(), with(&AutoscalingSpec{TargetCPUUtilization: ptr.To(int32(150))}))
	g.Expect(err).NotTo(gomega.HaveOccurred())

	_, err = w.ValidateCreate(context.Background(), with(&AutoscalingSpec{TargetCPUUtilization: ptr.To(int32(0))}))
	g.Expect(apierrors.IsInvalid(err)).To(gomega.BeTrue(), "%v", err)
	g.Expect(err.Error()).To(gomega.ContainSubstring("targetCPUUtilization must be at least 1"))

	o := with(&AutoscalingSpec{TargetMemoryUtilization: ptr.To(int32(150))})
	_, err = w.ValidateCreate(context.Background(), o)
	g.Expect(apierrors.IsInvalid(err)).To(gomega.BeTrue(), "%v", err)
	g.Expect(err.Error()).To(gomega.ContainSubstring("spec.autoscaling.targetMemoryUtilization"))
	g.Expect(err.Error()).To(gomega.ContainSubstring("can never be reached"))

	// The API container's own block decides the ceiling: a memory limit
	// twice the request makes 150 reachable, and 201 is one above 200.
	api := &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
	}
	o = with(&AutoscalingSpec{TargetMemoryUtilization: ptr.To(int32(150))})
	o.Spec.Deployment.Resources = api
	_, err = w.ValidateCreate(context.Background(), o)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	o = with(&AutoscalingSpec{TargetMemoryUtilization: ptr.To(int32(201))})
	o.Spec.Deployment.Resources = api
	_, err = w.ValidateCreate(context.Background(), o)
	g.Expect(apierrors.IsInvalid(err)).To(gomega.BeTrue(), "%v", err)
	g.Expect(err.Error()).To(gomega.ContainSubstring("or a target of at most 200"))

	_, err = w.ValidateCreate(context.Background(), with(&AutoscalingSpec{
		TargetCPUUtilization: ptr.To(int32(80)),
		Behavior: &autoscalingv2.HorizontalPodAutoscalerBehavior{
			ScaleDown: &autoscalingv2.HPAScalingRules{StabilizationWindowSeconds: ptr.To(int32(3601))},
		},
	}))
	g.Expect(apierrors.IsInvalid(err)).To(gomega.BeTrue(), "%v", err)
	g.Expect(err.Error()).To(gomega.ContainSubstring("spec.autoscaling.behavior.scaleDown.stabilizationWindowSeconds"))
	g.Expect(err.Error()).To(gomega.ContainSubstring("stabilizationWindowSeconds must be between 0 and 3600"))
}

// TestAPIPodSelector pins the exported selector to the labels the API
// Deployment's pods carry, the map the spread check compares against.
func TestAPIPodSelector(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(APIPodSelector("x")).To(gomega.Equal(map[string]string{
		"app.kubernetes.io/name":     "placement",
		"app.kubernetes.io/instance": "x",
	}))
	// Each call returns a fresh map, so a caller cannot alias another's.
	a := APIPodSelector("x")
	a["extra"] = "x"
	g.Expect(APIPodSelector("x")).NotTo(gomega.HaveKey("extra"))
}
