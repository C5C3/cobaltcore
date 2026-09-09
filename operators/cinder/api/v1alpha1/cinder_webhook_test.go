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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

// validCinder returns a minimal Cinder CR that passes every validation rule.
// Tests mutate single fields to exercise individual rules. Every Deployment
// block spells out its replica count the way an admitted CR carries it: the
// schema default fills a present block and the defaulting webhook an absent one,
// so a zero never reaches validation in production.
func validCinder() *Cinder {
	return &Cinder{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cinder", Namespace: "openstack"},
		Spec: CinderSpec{
			OpenStackRelease: "2025.2",
			Image: commonv1.ImageSpec{
				Repository: "ghcr.io/c5c3/cinder",
				Tag:        "2025.2",
			},
			Database: commonv1.DatabaseSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: "mariadb"},
			},
			Cache: commonv1.CacheSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: "memcached"},
				Backend:    commonv1.DefaultCacheBackend,
			},
			Messaging: commonv1.MessagingSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: "rabbitmq"},
			},
			API:              CinderAPISpec{Deployment: DeploymentSpec{Replicas: 3}},
			Scheduler:        CinderSchedulerSpec{Deployment: DeploymentSpec{Replicas: 1}},
			Volume:           CinderVolumeSpec{Deployment: DeploymentSpec{Replicas: 1}},
			Backup:           CinderBackupSpec{Deployment: DeploymentSpec{Replicas: 1}},
			KeystoneEndpoint: "http://keystone.openstack.svc.cluster.local:5000/v3",
			ServiceUser: &ServiceUserSpec{
				SecretRef: commonv1.SecretRefSpec{Name: "cinder-service-password", Key: "password"},
			},
		},
	}
}

// --- Defaulting webhook ---

// A CR that carries none of the four Deployment blocks is the case the webhook
// defaults reach: the schema default fills only a block the request already
// carries.
func TestCinderDefault_MaterializesAbsentDeploymentBlocks(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &CinderWebhook{}

	obj := validCinder()
	obj.Spec.API = CinderAPISpec{}
	obj.Spec.Scheduler = CinderSchedulerSpec{}
	obj.Spec.Volume = CinderVolumeSpec{}
	obj.Spec.Backup = CinderBackupSpec{}

	g.Expect(w.Default(context.Background(), obj)).To(gomega.Succeed())

	// The API scales horizontally and keeps the shared default; the other three
	// resolve to one.
	g.Expect(obj.Spec.API.Deployment.Replicas).To(gomega.Equal(commonv1.DefaultReplicas))
	g.Expect(obj.Spec.Scheduler.Deployment.Replicas).To(gomega.Equal(int32(1)))
	g.Expect(obj.Spec.Volume.Deployment.Replicas).To(gomega.Equal(int32(1)))
	g.Expect(obj.Spec.Backup.Deployment.Replicas).To(gomega.Equal(int32(1)))

	// Both single-writer Deployments get Recreate; the API keeps the reconciler's
	// rolling default (a nil strategy).
	g.Expect(obj.Spec.Volume.Deployment.Strategy).To(gomega.HaveValue(gomega.Equal(
		appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType})))
	g.Expect(obj.Spec.Backup.Deployment.Strategy).To(gomega.HaveValue(gomega.Equal(
		appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType})))
	g.Expect(obj.Spec.API.Deployment.Strategy).To(gomega.BeNil())

	// The backup container carries the raised memory limit; every other block
	// keeps the shared baseline.
	g.Expect(obj.Spec.Backup.Deployment.Resources.Requests.Memory().String()).To(gomega.Equal("256Mi"))
	g.Expect(obj.Spec.Backup.Deployment.Resources.Requests.Cpu().String()).To(gomega.Equal("100m"))
	g.Expect(obj.Spec.Backup.Deployment.Resources.Limits.Memory().String()).To(gomega.Equal("2Gi"))
	g.Expect(obj.Spec.Backup.Deployment.Resources.Limits.Cpu().String()).To(gomega.Equal("500m"))
	g.Expect(obj.Spec.API.Deployment.Resources.Limits.Memory().String()).To(gomega.Equal("512Mi"))

	// The API always runs under uWSGI, so the block is materialized.
	g.Expect(obj.Spec.API.UWSGI).NotTo(gomega.BeNil())
	g.Expect(obj.Spec.API.UWSGI.Processes).To(gomega.Equal(commonv1.DefaultUWSGIProcesses))
	g.Expect(obj.Spec.API.UWSGI.Threads).To(gomega.Equal(commonv1.DefaultUWSGIThreads))

	g.Expect(obj.Spec.Cache.Backend).To(gomega.Equal(commonv1.DefaultCacheBackend))
	g.Expect(obj.Spec.Logging).NotTo(gomega.BeNil())
	g.Expect(obj.Spec.Logging.Format).To(gomega.Equal("text"))
	g.Expect(obj.Spec.Logging.Level).To(gomega.Equal("INFO"))
}

// A resources block the user wrote is never touched, not even the half of it
// that is empty — the same nil-or-empty condition the shared defaults use.
func TestCinderDefault_PreservesExplicitBackupResources(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &CinderWebhook{}

	obj := validCinder()
	obj.Spec.Backup.Deployment.Resources = &corev1.ResourceRequirements{
		Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("4Gi")},
	}

	g.Expect(w.Default(context.Background(), obj)).To(gomega.Succeed())

	g.Expect(obj.Spec.Backup.Deployment.Resources.Limits.Memory().String()).To(gomega.Equal("4Gi"))
	g.Expect(obj.Spec.Backup.Deployment.Resources.Requests).To(gomega.BeEmpty())
}

// A present volume/backup block arrives with replicas already at the schema
// default of three, and rewriting that to one would overwrite a value the
// submitter can see in their own manifest. The CEL rule rejects it instead, and
// an explicit strategy is left alone.
func TestCinderDefault_LeavesPresentSingletonBlocksAlone(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &CinderWebhook{}

	obj := validCinder()
	obj.Spec.Volume.Deployment.Replicas = commonv1.DefaultReplicas
	obj.Spec.Backup.Deployment.Strategy = &appsv1.DeploymentStrategy{
		Type: appsv1.RollingUpdateDeploymentStrategyType,
	}

	g.Expect(w.Default(context.Background(), obj)).To(gomega.Succeed())

	g.Expect(obj.Spec.Volume.Deployment.Replicas).To(gomega.Equal(commonv1.DefaultReplicas))
	g.Expect(obj.Spec.Backup.Deployment.Strategy.Type).To(gomega.Equal(appsv1.RollingUpdateDeploymentStrategyType))
}

// The service-user block stays optional: a Keystone-free deployment omits it
// together with spec.keystoneEndpoint, so the defaulter must not materialize it.
func TestCinderDefault_ServiceUserIdentity(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &CinderWebhook{}

	absent := validCinder()
	absent.Spec.KeystoneEndpoint = ""
	absent.Spec.ServiceUser = nil
	g.Expect(w.Default(context.Background(), absent)).To(gomega.Succeed())
	g.Expect(absent.Spec.ServiceUser).To(gomega.BeNil())

	minimal := validCinder()
	minimal.Spec.ServiceUser = &ServiceUserSpec{
		SecretRef: commonv1.SecretRefSpec{Name: "cinder-service-password"},
	}
	g.Expect(w.Default(context.Background(), minimal)).To(gomega.Succeed())
	g.Expect(minimal.Spec.ServiceUser.Username).To(gomega.Equal("cinder"))
	g.Expect(minimal.Spec.ServiceUser.ProjectName).To(gomega.Equal("service"))
	g.Expect(minimal.Spec.ServiceUser.UserDomainName).To(gomega.Equal("Default"))
	g.Expect(minimal.Spec.ServiceUser.ProjectDomainName).To(gomega.Equal("Default"))
	g.Expect(minimal.Spec.ServiceUser.SecretRef.Key).To(gomega.Equal("password"))

	explicit := validCinder()
	explicit.Spec.ServiceUser = &ServiceUserSpec{
		Username:          "block-svc",
		ProjectName:       "volumes",
		UserDomainName:    "Corp",
		ProjectDomainName: "Corp",
		SecretRef:         commonv1.SecretRefSpec{Name: "custom-secret", Key: "custom-key"},
	}
	g.Expect(w.Default(context.Background(), explicit)).To(gomega.Succeed())
	g.Expect(explicit.Spec.ServiceUser.Username).To(gomega.Equal("block-svc"))
	g.Expect(explicit.Spec.ServiceUser.SecretRef.Key).To(gomega.Equal("custom-key"))
}

// Barbican is the only castellan key manager, so a present block that names none
// is filled rather than rejected — but a block that names one keeps it.
func TestCinderDefault_KeyManagerType(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &CinderWebhook{}

	absent := validCinder()
	g.Expect(w.Default(context.Background(), absent)).To(gomega.Succeed())
	g.Expect(absent.Spec.KeyManager).To(gomega.BeNil())

	empty := validCinder()
	empty.Spec.KeyManager = &KeyManagerSpec{
		Barbican: &BarbicanKeyManagerSpec{Endpoint: "http://barbican.openstack.svc:9311"},
	}
	g.Expect(w.Default(context.Background(), empty)).To(gomega.Succeed())
	g.Expect(empty.Spec.KeyManager.Type).To(gomega.Equal(KeyManagerTypeBarbican))
}

// --- Validating webhook ---

func TestCinderValidateCreate_ValidSpecAccepted(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &CinderWebhook{}

	_, err := w.ValidateCreate(context.Background(), validCinder())
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

func TestCinderValidateCreate_RejectionTable(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(o *Cinder)
		wantSub string
	}{
		{
			name:    "zero API replicas rejected",
			mutate:  func(o *Cinder) { o.Spec.API.Deployment.Replicas = 0 },
			wantSub: "replicas must be at least 1",
		},
		{
			name: "image tag and digest both set rejected",
			mutate: func(o *Cinder) {
				o.Spec.Image.Digest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			},
			wantSub: "exactly one of image.tag or image.digest",
		},
		{
			name:    "database clusterRef and host both set rejected",
			mutate:  func(o *Cinder) { o.Spec.Database.Host = "mariadb.example.com" },
			wantSub: "exactly one of clusterRef or host",
		},
		{
			name: "dynamic credentials without clusterRef rejected",
			mutate: func(o *Cinder) {
				o.Spec.Database.ClusterRef = nil
				o.Spec.Database.Host = "mariadb.example.com"
				o.Spec.Database.CredentialsMode = commonv1.CredentialsModeDynamic
			},
			wantSub: "credentialsMode Dynamic requires clusterRef",
		},
		{
			name:    "cache clusterRef and servers both set rejected",
			mutate:  func(o *Cinder) { o.Spec.Cache.Servers = []string{"memcached-0:11211"} },
			wantSub: "exactly one of clusterRef or servers",
		},
		{
			// The cache reaches [coordination] backend_url through the verbatim INI
			// renderer, so a newline there injects arbitrary config lines.
			name: "cache server with a newline rejected",
			mutate: func(o *Cinder) {
				o.Spec.Cache.ClusterRef = nil
				o.Spec.Cache.Servers = []string{"memcached-0:11211\nauth_url = http://attacker.example/v3"}
			},
			wantSub: "must not contain a newline or carriage return",
		},
		{
			// The bus is required: the API hands every volume request to the
			// scheduler over it, so a Cinder without a broker accepts requests
			// nothing acts on.
			name:    "messaging with neither clusterRef nor secretRef rejected",
			mutate:  func(o *Cinder) { o.Spec.Messaging = commonv1.MessagingSpec{} },
			wantSub: "exactly one of clusterRef or secretRef",
		},
		{
			name: "messaging with both clusterRef and secretRef rejected",
			mutate: func(o *Cinder) {
				o.Spec.Messaging.SecretRef = &commonv1.SecretRefSpec{Name: "transport-url"}
			},
			wantSub: "exactly one of clusterRef or secretRef",
		},
		{
			// A second cinder-volume under the same host identity has the same
			// volume state open twice, which the NFS drivers refuse.
			name:    "volume replicas above one rejected",
			mutate:  func(o *Cinder) { o.Spec.Volume.Deployment.Replicas = 2 },
			wantSub: "volume/backup deployments run exactly one replica",
		},
		{
			// A rolling update overlaps the surge pod with the outgoing one, which
			// is the same collision by another route.
			name: "backup rolling-update strategy rejected",
			mutate: func(o *Cinder) {
				o.Spec.Backup.Deployment.Strategy = &appsv1.DeploymentStrategy{
					Type: appsv1.RollingUpdateDeploymentStrategyType,
				}
			},
			wantSub: "volume/backup deployments must use the Recreate strategy",
		},
		{
			// cinder passes both IDs to the volume API without resolving them
			// through Keystone, so an empty one creates the cached volumes under
			// nothing.
			name: "internalTenant with an empty userID rejected",
			mutate: func(o *Cinder) {
				o.Spec.InternalTenant = &InternalTenantSpec{ProjectID: "8f2b", UserID: ""}
			},
			wantSub: "userID must be set",
		},
		{
			name: "dbPurge zero retentionDays rejected",
			mutate: func(o *Cinder) {
				o.Spec.DBPurge = &DBPurgeSpec{RetentionDays: ptr.To(int32(0))}
			},
			wantSub: "retentionDays must be at least 1",
		},
		{
			// The schedule carries no CRD pattern, so the webhook's
			// cron.ParseStandard call is the only gate between a typo and a CronJob
			// the API server refuses to create.
			name: "dbPurge four-field schedule rejected",
			mutate: func(o *Cinder) {
				o.Spec.DBPurge = &DBPurgeSpec{Schedule: "* * * *"}
			},
			wantSub: "invalid cron expression",
		},
		{
			name:    "keystoneEndpoint without serviceUser rejected",
			mutate:  func(o *Cinder) { o.Spec.ServiceUser = nil },
			wantSub: "keystoneEndpoint and serviceUser must be set together",
		},
		{
			name:    "serviceUser without keystoneEndpoint rejected",
			mutate:  func(o *Cinder) { o.Spec.KeystoneEndpoint = "" },
			wantSub: "keystoneEndpoint and serviceUser must be set together",
		},
		{
			name: "keyManager without keystoneEndpoint rejected",
			mutate: func(o *Cinder) {
				o.Spec.KeystoneEndpoint = ""
				o.Spec.ServiceUser = nil
				o.Spec.KeyManager = &KeyManagerSpec{
					Type:     KeyManagerTypeBarbican,
					Barbican: &BarbicanKeyManagerSpec{Endpoint: "http://barbican.openstack.svc:9311"},
				}
			},
			wantSub: "keyManager requires keystoneEndpoint",
		},
		{
			name: "keyManager type without its block rejected",
			mutate: func(o *Cinder) {
				o.Spec.KeyManager = &KeyManagerSpec{Type: KeyManagerTypeBarbican}
			},
			wantSub: "exactly one key-manager block matching spec.keyManager.type",
		},
		{
			name: "keyManager block without its type rejected",
			mutate: func(o *Cinder) {
				o.Spec.KeyManager = &KeyManagerSpec{
					Type:     KeyManagerType("Vault"),
					Barbican: &BarbicanKeyManagerSpec{Endpoint: "http://barbican.openstack.svc:9311"},
				}
			},
			wantSub: "exactly one key-manager block matching spec.keyManager.type",
		},
		{
			name: "barbican endpoint without a scheme rejected",
			mutate: func(o *Cinder) {
				o.Spec.KeyManager = &KeyManagerSpec{
					Type:     KeyManagerTypeBarbican,
					Barbican: &BarbicanKeyManagerSpec{Endpoint: "barbican.openstack.svc:9311"},
				}
			},
			wantSub: "scheme must be http or https",
		},
		{
			name:    "glanceEndpoint without a host rejected",
			mutate:  func(o *Cinder) { o.Spec.GlanceEndpoint = "http://" },
			wantSub: "URL must include a host",
		},
		{
			// The drain window between the end of the preStop sleep and SIGKILL must
			// stay positive, and a uWSGI request kill must fit inside it.
			name: "uwsgi harakiri beyond the drain window rejected",
			mutate: func(o *Cinder) {
				o.Spec.API.UWSGI = &UWSGISpec{Harakiri: ptr.To(int32(60))}
			},
			wantSub: "harakiri (60) must be strictly less than",
		},
		{
			name: "scheduler preStop sleep beyond the grace period rejected",
			mutate: func(o *Cinder) {
				o.Spec.Scheduler.Deployment.PreStopSleepSeconds = ptr.To(int64(30))
			},
			wantSub: "preStopSleepSeconds (30) must be strictly less than",
		},
		{
			name: "volume resource request above its limit rejected",
			mutate: func(o *Cinder) {
				o.Spec.Volume.Deployment.Resources = &corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")},
					Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
				}
			},
			wantSub: "memory request must not exceed limit",
		},
		{
			name: "autoscaling without a utilization target rejected",
			mutate: func(o *Cinder) {
				o.Spec.Autoscaling = &AutoscalingSpec{MaxReplicas: 5}
			},
			wantSub: "at least one of targetCPUUtilization or targetMemoryUtilization",
		},
		{
			name: "networkPolicy without an ingress source rejected",
			mutate: func(o *Cinder) {
				o.Spec.NetworkPolicy = &NetworkPolicySpec{}
			},
			wantSub: "at least one ingress source must be specified",
		},
		{
			name: "gateway without a hostname rejected",
			mutate: func(o *Cinder) {
				o.Spec.Gateway = &GatewaySpec{ParentRef: GatewayParentRefSpec{Name: "gateway"}}
			},
			wantSub: "hostname must be set when spec.gateway is configured",
		},
		{
			// A Keystone-free Cinder renders auth_strategy = noauth, which takes the
			// project from the request URL and validates no token, so publishing it
			// through a Gateway serves every volume operation unauthenticated.
			name: "gateway without keystoneEndpoint rejected",
			mutate: func(o *Cinder) {
				o.Spec.KeystoneEndpoint = ""
				o.Spec.ServiceUser = nil
				o.Spec.Gateway = &GatewaySpec{
					Hostname:  "cinder.example.com",
					ParentRef: GatewayParentRefSpec{Name: "gateway"},
				}
			},
			wantSub: "gateway requires keystoneEndpoint",
		},
		{
			name: "unsupported logging level rejected",
			mutate: func(o *Cinder) {
				o.Spec.Logging = &LoggingSpec{Level: "TRACE"}
			},
			wantSub: "Unsupported value",
		},
		{
			name: "empty per-logger name rejected",
			mutate: func(o *Cinder) {
				o.Spec.Logging = &LoggingSpec{PerLoggerLevels: map[string]string{"": "INFO"}}
			},
			wantSub: "logger name must not be empty",
		},
		{
			// A logger name renders into the [DEFAULT] default_log_levels CSV, which
			// the INI renderer writes verbatim, so a newline injects config lines the
			// same way an extraConfig key would.
			name: "per-logger name with a newline rejected",
			mutate: func(o *Cinder) {
				o.Spec.Logging = &LoggingSpec{PerLoggerLevels: map[string]string{
					"cinder\ntransport_url = rabbit://attacker": "DEBUG",
				}}
			},
			wantSub: "logger name must not contain a newline or carriage return",
		},
		{
			// The map values cannot be expressed as a CRD enum on
			// additionalProperties, so the webhook is the only gate on them.
			name: "per-logger level outside the enum rejected",
			mutate: func(o *Cinder) {
				o.Spec.Logging = &LoggingSpec{PerLoggerLevels: map[string]string{"cinder": "VERBOSE"}}
			},
			wantSub: "level must be one of DEBUG, INFO, WARNING, ERROR, CRITICAL",
		},
		{
			name: "empty extraConfig section rejected",
			mutate: func(o *Cinder) {
				o.Spec.ExtraConfig = map[string]map[string]string{"": {"debug": "true"}}
			},
			wantSub: "extraConfig section name must not be empty",
		},
		{
			name: "empty extraConfig key rejected",
			mutate: func(o *Cinder) {
				o.Spec.ExtraConfig = map[string]map[string]string{"DEFAULT": {"": "true"}}
			},
			wantSub: "extraConfig key must not be empty",
		},
		{
			name: "extraConfig section with a newline rejected",
			mutate: func(o *Cinder) {
				o.Spec.ExtraConfig = map[string]map[string]string{
					"DEFAULT]\n[profiler": {"enabled": "true"},
				}
			},
			wantSub: "extraConfig section name must not contain a newline or carriage return",
		},
		{
			// The rendered INI writes "%s = %s" verbatim, so a newline in a value
			// smuggles a whole key past the ownership and catalog gates — they key on
			// (section, key) names and never look inside a value. auth_strategy is
			// exactly the key that gate exists for: anything but keystone serves the
			// API through a pipeline without token validation.
			name: "extraConfig value with a newline rejected",
			mutate: func(o *Cinder) {
				o.Spec.ExtraConfig = map[string]map[string]string{
					"DEFAULT": {"debug": "true\nauth_strategy = noauth"},
				}
			},
			wantSub: "extraConfig key and value must not contain a newline or carriage return",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			w := &CinderWebhook{}
			obj := validCinder()
			tc.mutate(obj)

			_, err := w.ValidateCreate(context.Background(), obj)
			g.Expect(err).To(gomega.HaveOccurred())
			g.Expect(err.Error()).To(gomega.ContainSubstring(tc.wantSub))
		})
	}
}

func TestCinderValidateCreate_NameLengthBoundedByPurgeCronJob(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &CinderWebhook{}

	atLimit := validCinder()
	atLimit.Name = strings.Repeat("c", MaxCinderNameLength)
	_, err := w.ValidateCreate(context.Background(), atLimit)
	g.Expect(err).NotTo(gomega.HaveOccurred(),
		"a name that still fits the 52-character CronJob budget must be accepted")

	tooLong := validCinder()
	tooLong.Name = strings.Repeat("c", MaxCinderNameLength+1)
	_, err = w.ValidateCreate(context.Background(), tooLong)
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("metadata.name")))
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("db-purge")))
}

// The bound is create-only. metadata.name is immutable, so on update it could
// only ever fire against a CR a pre-upgrade operator already admitted — and the
// validating webhook registers the update verb, so it also sees the
// finalizer-removal update reconcileDelete issues. Rejecting that would wedge the
// grandfathered CR in Terminating forever, with no field left to edit to repair
// it.
func TestCinderValidateUpdate_OverlongNameStaysUpdatable(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &CinderWebhook{}

	grandfathered := validCinder()
	grandfathered.Name = strings.Repeat("c", MaxCinderNameLength+1)
	grandfathered.Finalizers = []string{"cinder.openstack.c5c3.io/finalizer"}

	deleting := grandfathered.DeepCopy()
	deleting.Finalizers = nil

	_, err := w.ValidateUpdate(context.Background(), grandfathered, deleting)
	g.Expect(err).NotTo(gomega.HaveOccurred(),
		"an over-long grandfathered CR must stay updatable, or its deletion never completes")
}

// --- extraConfig ---

// The Rejected owned keys are refused whatever the catalog says. The two privsep
// sections are the case that pins the ordering: no catalog enumerates them
// (oslo.privsep registers them at runtime), yet the ownership registry exempts
// their keys from the catalog scan, so the rejection names the ownership rule and
// never the missing section.
func TestCinderValidate_ExtraConfigRejectedOwnedKeys(t *testing.T) {
	tests := []struct {
		name        string
		extraConfig map[string]map[string]string
		wantSub     string
	}{
		{
			name:        "auth_strategy rejected",
			extraConfig: map[string]map[string]string{"DEFAULT": {"auth_strategy": "noauth"}},
			wantSub:     "auth_strategy is managed via operator-computed and must not be set in extraConfig",
		},
		{
			name:        "transport_url rejected",
			extraConfig: map[string]map[string]string{"DEFAULT": {"transport_url": "rabbit://user:pw@broker"}},
			wantSub:     "transport_url is managed via spec.messaging",
		},
		{
			name:        "privsep capabilities rejected",
			extraConfig: map[string]map[string]string{"cinder_sys_admin": {"capabilities": "CAP_SYS_ADMIN"}},
			wantSub:     "capabilities is managed via operator-computed",
		},
		{
			name:        "os-brick privsep helper_command rejected",
			extraConfig: map[string]map[string]string{"privsep_osbrick": {"helper_command": "/bin/sh"}},
			wantSub:     "helper_command is managed via operator-computed",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			w := &CinderWebhook{}

			obj := validCinder()
			obj.Spec.ExtraConfig = tc.extraConfig
			_, err := w.ValidateCreate(context.Background(), obj)
			g.Expect(err).To(gomega.HaveOccurred())
			g.Expect(err.Error()).To(gomega.ContainSubstring(tc.wantSub))
			g.Expect(err.Error()).NotTo(gomega.ContainSubstring("option catalog"),
				"an owned key is exempt from the catalog scan, so only the ownership rule may fire")
		})
	}
}

// A backend's own section is rendered from its CinderBackend CR, so naming it in
// extraConfig reaches the catalog as a section it does not know.
func TestCinderValidate_ExtraConfigBackendSectionRejected(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &CinderWebhook{}

	obj := validCinder()
	obj.Spec.ExtraConfig = map[string]map[string]string{
		"nfs1": {"volume_backend_name": "nfs1"},
	}
	_, err := w.ValidateCreate(context.Background(), obj)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("no such section in the cinder 2025.2 option catalog"))
}

func TestCinderValidate_ExtraConfigUnknownOptionRejected(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &CinderWebhook{}

	obj := validCinder()
	obj.Spec.ExtraConfig = map[string]map[string]string{
		"DEFAULT": {"not_an_option": "x"},
	}
	_, err := w.ValidateCreate(context.Background(), obj)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("no such option in the cinder 2025.2 option catalog"))
}

// A deprecated-but-accepted option is admitted and reported: the catalog knows
// the replacement, so the warning names it rather than leaving the user with an
// option cinder ignores at runtime.
func TestCinderValidate_ExtraConfigDeprecatedOptionWarns(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &CinderWebhook{}

	obj := validCinder()
	obj.Spec.ExtraConfig = map[string]map[string]string{
		"DEFAULT": {"logfile": "/var/log/cinder/cinder.log"},
	}

	warnings, err := w.ValidateCreate(context.Background(), obj)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(warnings).To(gomega.ConsistOf(gomega.ContainSubstring(
		"deprecated option in cinder 2025.2, replaced by [DEFAULT] log_file")))
}

// A release the build ships no catalog for must not block admission: the check
// fails open with exactly one warning, and the two misses are distinguishable.
func TestCinderValidate_ExtraConfigFailsOpenWithoutCatalog(t *testing.T) {
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
			w := &CinderWebhook{}

			obj := validCinder()
			obj.Spec.OpenStackRelease = tc.release
			// An option no catalog carries: with a catalog resolved this would be a
			// rejection, so the acceptance below is attributable to the fail-open path.
			obj.Spec.ExtraConfig = map[string]map[string]string{
				"DEFAULT": {"not_an_option": "x"},
			}

			warnings, err := w.ValidateCreate(context.Background(), obj)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(warnings).To(gomega.ConsistOf(gomega.ContainSubstring(tc.wantSub)))
		})
	}
}

// Neither shape of "no overrides" may produce a warning or an error: both are the
// ordinary case.
func TestCinderValidate_ExtraConfigAbsentAndEmptyAccepted(t *testing.T) {
	for _, tc := range []struct {
		name        string
		extraConfig map[string]map[string]string
	}{
		{name: "nil map accepted", extraConfig: nil},
		{name: "empty map accepted", extraConfig: map[string]map[string]string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			w := &CinderWebhook{}

			obj := validCinder()
			obj.Spec.ExtraConfig = tc.extraConfig
			warnings, err := w.ValidateCreate(context.Background(), obj)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(warnings).To(gomega.BeEmpty())
		})
	}
}

// The catalog check is re-run on update only when one of its inputs changed, so a
// CR whose extraConfig went stale-invalid against a regenerated catalog stays
// editable through every other field.
func TestCinderValidateUpdate_ExtraConfigCatalogGate(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &CinderWebhook{}

	stale := validCinder()
	stale.Spec.ExtraConfig = map[string]map[string]string{
		"DEFAULT": {"not_an_option": "x"},
	}

	scaled := stale.DeepCopy()
	scaled.Spec.API.Deployment.Replicas = 5
	_, err := w.ValidateUpdate(context.Background(), stale, scaled)
	g.Expect(err).NotTo(gomega.HaveOccurred(),
		"an edit that leaves extraConfig and the release alone must not re-run the catalog check")

	edited := stale.DeepCopy()
	edited.Spec.ExtraConfig = map[string]map[string]string{
		"DEFAULT": {"still_not_an_option": "x"},
	}
	_, err = w.ValidateUpdate(context.Background(), stale, edited)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("no such option in the cinder 2025.2 option catalog"))
}

// --- Warnings ---

// Shortening the retention window is the one edit on this CR that destroys data
// at the next firing with no undo, and a typo is indistinguishable from an
// intended change at admission time. The warning is what echoes it back.
func TestCinderValidateUpdate_WarnsOnReducedDBPurgeRetention(t *testing.T) {
	newObjWith := func(p *DBPurgeSpec) *Cinder {
		o := validCinder()
		o.Spec.DBPurge = p
		return o
	}
	tests := []struct {
		name     string
		old, new *DBPurgeSpec
		wantWarn bool
	}{
		{
			name:     "explicit reduction warns",
			old:      &DBPurgeSpec{RetentionDays: ptr.To(int32(30))},
			new:      &DBPurgeSpec{RetentionDays: ptr.To(int32(1))},
			wantWarn: true,
		},
		{
			// An unset field resolves to the operator default, so setting a value
			// below it is just as much a reduction as lowering an explicit one.
			name:     "reduction below the resolved default warns",
			old:      nil,
			new:      &DBPurgeSpec{RetentionDays: ptr.To(int32(7))},
			wantWarn: true,
		},
		{
			name:     "raising the retention is silent",
			old:      &DBPurgeSpec{RetentionDays: ptr.To(int32(7))},
			new:      &DBPurgeSpec{RetentionDays: ptr.To(int32(90))},
			wantWarn: false,
		},
		{
			// Dropping the block restores the default (30), which is a widening from
			// 7 — not a reduction to zero.
			name:     "dropping the block below the default is silent",
			old:      &DBPurgeSpec{RetentionDays: ptr.To(int32(7))},
			new:      nil,
			wantWarn: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			w := &CinderWebhook{}

			warnings, err := w.ValidateUpdate(context.Background(), newObjWith(tc.old), newObjWith(tc.new))
			g.Expect(err).NotTo(gomega.HaveOccurred())
			if tc.wantWarn {
				g.Expect(warnings).To(gomega.ContainElement(
					gomega.ContainSubstring("spec.dbPurge.retentionDays reduced"),
				))
				return
			}
			g.Expect(warnings).NotTo(gomega.ContainElement(
				gomega.ContainSubstring("spec.dbPurge.retentionDays reduced"),
			))
		})
	}
}

// --- spec.targetClusterRef (multicluster routing) ---
//
// The webhook twin of the two transition CEL rules on CinderSpec. Both layers
// carry the rule so it still holds for a CR that reaches the webhook through a
// schema bypass, where re-pointing the ref would abandon every child the
// reconciler placed on the old cluster.

// TestCinderValidateUpdate_TargetClusterRefAddedRejected covers the presence
// flip upwards: the children of a CR created without a target cluster live on
// the management cluster, so naming one afterwards is rejected.
func TestCinderValidateUpdate_TargetClusterRefAddedRejected(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &CinderWebhook{}
	old := validCinder()
	newObj := validCinder()
	newObj.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "edge-1"}

	_, err := w.ValidateUpdate(context.Background(), old, newObj)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("targetClusterRef is immutable"))
}

// TestCinderValidateUpdate_TargetClusterRefRemovedRejected covers the presence
// flip downwards: dropping the ref would strand the children on the cluster it
// named.
func TestCinderValidateUpdate_TargetClusterRefRemovedRejected(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &CinderWebhook{}
	old := validCinder()
	old.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "edge-1"}
	newObj := validCinder()

	_, err := w.ValidateUpdate(context.Background(), old, newObj)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("targetClusterRef is immutable"))
}

// TestCinderValidateUpdate_TargetClusterRefChangedRejected covers a rename,
// which would re-point the reconciler at a cluster that holds none of the
// children.
func TestCinderValidateUpdate_TargetClusterRefChangedRejected(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &CinderWebhook{}
	old := validCinder()
	old.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "edge-1"}
	newObj := validCinder()
	newObj.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "edge-2"}

	_, err := w.ValidateUpdate(context.Background(), old, newObj)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("targetClusterRef is immutable"))
}

// TestCinderValidateUpdate_TargetClusterRefUnchangedAccepted proves the check
// freezes only the ref: an unrelated edit on a CR that names a target cluster
// still passes.
func TestCinderValidateUpdate_TargetClusterRefUnchangedAccepted(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &CinderWebhook{}
	old := validCinder()
	old.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "edge-1"}
	newObj := old.DeepCopy()
	newObj.Spec.API.Deployment.Replicas = 5

	_, err := w.ValidateUpdate(context.Background(), old, newObj)
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

func TestCinderValidateDelete_AlwaysAccepts(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &CinderWebhook{}

	warnings, err := w.ValidateDelete(context.Background(), validCinder())
	g.Expect(warnings).To(gomega.BeNil())
	g.Expect(err).NotTo(gomega.HaveOccurred())
}
