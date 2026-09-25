// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

// validNova returns a minimal Nova CR that passes every validation rule. Tests
// mutate single fields to exercise individual rules. Every Deployment block
// spells out its replica count the way an admitted CR carries it: the schema
// default fills a present block and the defaulting webhook an absent one, so a
// zero never reaches validation in production.
func validNova() *Nova {
	return &Nova{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nova", Namespace: "openstack"},
		Spec: NovaSpec{
			OpenStackRelease: "2025.2",
			Image: commonv1.ImageSpec{
				Repository: "ghcr.io/c5c3/nova",
				Tag:        "2025.2",
			},
			APIDatabase: commonv1.DatabaseSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: "mariadb"},
				Database:   "nova_api",
				SecretRef:  commonv1.SecretRefSpec{Name: "nova-api-db"},
			},
			Database: commonv1.DatabaseSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: "mariadb"},
				Database:   "nova",
				SecretRef:  commonv1.SecretRefSpec{Name: "nova-db"},
			},
			Cache: commonv1.CacheSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: "memcached"},
				Backend:    commonv1.DefaultCacheBackend,
			},
			Messaging: commonv1.MessagingSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: "rabbitmq"},
			},
			API: NovaAPISpec{Deployment: DeploymentSpec{Replicas: 3}},
			Metadata: NovaMetadataSpec{
				Deployment:      DeploymentSpec{Replicas: 1},
				SharedSecretRef: commonv1.SecretRefSpec{Name: "nova-metadata-secret", Key: "shared_secret"},
			},
			Scheduler:        NovaSchedulerSpec{Deployment: DeploymentSpec{Replicas: 1}},
			Conductor:        NovaConductorSpec{Deployment: DeploymentSpec{Replicas: 1}},
			KeystoneEndpoint: "http://keystone.openstack.svc.cluster.local:5000/v3",
			ServiceUser: ServiceUserSpec{
				SecretRef: commonv1.SecretRefSpec{Name: "nova-service-password", Key: "password"},
			},
		},
	}
}

// --- Defaulting webhook ---

// A CR that carries none of the five Deployment blocks is the case the webhook
// defaults reach: the schema default fills only a block the request already
// carries.
func TestNovaDefault_MaterializesAbsentDeploymentBlocks(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &NovaWebhook{}

	obj := validNova()
	obj.Spec.API = NovaAPISpec{}
	obj.Spec.Metadata = NovaMetadataSpec{
		SharedSecretRef: commonv1.SecretRefSpec{Name: "nova-metadata-secret"},
	}
	obj.Spec.Scheduler = NovaSchedulerSpec{}
	obj.Spec.Conductor = NovaConductorSpec{}
	obj.Spec.ConsoleProxy = NovaConsoleProxySpec{}
	obj.Spec.Cache.Backend = ""

	g.Expect(w.Default(context.Background(), obj)).To(gomega.Succeed())

	// The API scales on request rate and keeps the shared default; the other four
	// resolve to one.
	g.Expect(obj.Spec.API.Deployment.Replicas).To(gomega.Equal(commonv1.DefaultReplicas))
	g.Expect(obj.Spec.Metadata.Deployment.Replicas).To(gomega.Equal(int32(1)))
	g.Expect(obj.Spec.Scheduler.Deployment.Replicas).To(gomega.Equal(int32(1)))
	g.Expect(obj.Spec.Conductor.Deployment.Replicas).To(gomega.Equal(int32(1)))
	g.Expect(obj.Spec.ConsoleProxy.Deployment).NotTo(gomega.BeNil())
	g.Expect(obj.Spec.ConsoleProxy.Deployment.Replicas).To(gomega.Equal(int32(1)))

	// No block gets resources: the reconcilers resolve them when they render
	// each Deployment.
	g.Expect(obj.Spec.API.Deployment.Resources).To(gomega.BeNil())
	g.Expect(obj.Spec.Metadata.Deployment.Resources).To(gomega.BeNil())
	g.Expect(obj.Spec.Scheduler.Deployment.Resources).To(gomega.BeNil())
	g.Expect(obj.Spec.Conductor.Deployment.Resources).To(gomega.BeNil())
	g.Expect(obj.Spec.ConsoleProxy.Deployment.Resources).To(gomega.BeNil())

	// The two RPC servers drain for over two and a half minutes, so both get a
	// grace window that covers it; the HTTP front ends keep the shared default.
	g.Expect(obj.Spec.Scheduler.Deployment.TerminationGracePeriodSeconds).To(
		gomega.HaveValue(gomega.Equal(DefaultRPCTerminationGracePeriodSeconds)))
	g.Expect(obj.Spec.Conductor.Deployment.TerminationGracePeriodSeconds).To(
		gomega.HaveValue(gomega.Equal(DefaultRPCTerminationGracePeriodSeconds)))
	g.Expect(obj.Spec.API.Deployment.TerminationGracePeriodSeconds).To(gomega.BeNil())

	g.Expect(obj.Spec.Scheduler.Workers).To(gomega.HaveValue(gomega.Equal(DefaultWorkers)))
	g.Expect(obj.Spec.Conductor.Workers).To(gomega.HaveValue(gomega.Equal(DefaultWorkers)))

	// Both HTTP front ends run under uWSGI, so both blocks are materialized.
	g.Expect(obj.Spec.API.UWSGI).NotTo(gomega.BeNil())
	g.Expect(obj.Spec.API.UWSGI.Processes).To(gomega.Equal(commonv1.DefaultUWSGIProcesses))
	g.Expect(obj.Spec.Metadata.UWSGI).NotTo(gomega.BeNil())
	g.Expect(obj.Spec.Metadata.UWSGI.Threads).To(gomega.Equal(commonv1.DefaultUWSGIThreads))

	g.Expect(obj.Spec.ConsoleProxy.Enabled).To(gomega.HaveValue(gomega.BeTrue()))
	g.Expect(obj.Spec.Cache.Backend).To(gomega.Equal(commonv1.DefaultCacheBackend))
	g.Expect(obj.Spec.Logging).NotTo(gomega.BeNil())
	g.Expect(obj.Spec.Logging.Format).To(gomega.Equal("text"))
	g.Expect(obj.Spec.Logging.Level).To(gomega.Equal("INFO"))
}

// Every default is conditional on a zero value, so a CR that spells a knob out
// keeps it. The console switch is the one that matters most: a false the
// defaulter overwrote would project a proxy the submitter turned off.
func TestNovaDefault_LeavesExplicitValuesAlone(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &NovaWebhook{}

	obj := validNova()
	obj.Spec.Scheduler.Deployment.Replicas = 4
	obj.Spec.Scheduler.Deployment.TerminationGracePeriodSeconds = ptr.To(int64(90))
	obj.Spec.Scheduler.Workers = ptr.To(int32(8))
	obj.Spec.Conductor.Workers = ptr.To(int32(16))
	obj.Spec.ConsoleProxy.Enabled = ptr.To(false)
	obj.Spec.Metadata.SharedSecretRef.Key = "metadata-proxy"

	g.Expect(w.Default(context.Background(), obj)).To(gomega.Succeed())

	g.Expect(obj.Spec.Scheduler.Deployment.Replicas).To(gomega.Equal(int32(4)))
	g.Expect(obj.Spec.Scheduler.Deployment.TerminationGracePeriodSeconds).To(
		gomega.HaveValue(gomega.Equal(int64(90))))
	g.Expect(obj.Spec.Scheduler.Workers).To(gomega.HaveValue(gomega.Equal(int32(8))))
	g.Expect(obj.Spec.Conductor.Workers).To(gomega.HaveValue(gomega.Equal(int32(16))))
	g.Expect(obj.Spec.ConsoleProxy.Enabled).To(gomega.HaveValue(gomega.BeFalse()))
	g.Expect(obj.Spec.Metadata.SharedSecretRef.Key).To(gomega.Equal("metadata-proxy"))
}

// A disabled proxy must reach validation with a nil Deployment: a zero-valued
// DeploymentSpec marshals as an empty object, so materializing it here would
// make the operator itself produce the shape the console rule rejects.
func TestNovaDefault_DisabledConsoleProxyKeepsTheBlockAbsent(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &NovaWebhook{}

	obj := validNova()
	obj.Spec.ConsoleProxy.Enabled = ptr.To(false)

	g.Expect(w.Default(context.Background(), obj)).To(gomega.Succeed())
	g.Expect(obj.Spec.ConsoleProxy.Deployment).To(gomega.BeNil())

	// And the CR the defaulter produced is one the validator accepts.
	_, err := w.ValidateCreate(context.Background(), obj)
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

// Switching a running proxy off is a patch that sets enabled to false and
// nothing else, so the request still carries the deployment block this webhook
// materialized on create. The defaulter has to take that block away again, or
// the console rule rejects the disable over a field the submitter never wrote.
func TestNovaDefault_DisablingTheConsoleProxyRemovesTheMaterializedBlock(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &NovaWebhook{}

	old := validNova()
	g.Expect(w.Default(context.Background(), old)).To(gomega.Succeed())
	g.Expect(old.Spec.ConsoleProxy.Deployment).NotTo(gomega.BeNil())

	disabled := old.DeepCopy()
	disabled.Spec.ConsoleProxy.Enabled = ptr.To(false)
	g.Expect(w.Default(context.Background(), disabled)).To(gomega.Succeed())
	g.Expect(disabled.Spec.ConsoleProxy.Deployment).To(gomega.BeNil())

	_, err := w.ValidateUpdate(context.Background(), old, disabled)
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

// The service-user block is required, so only its fields are filled, and each
// only when empty.
func TestNovaDefault_ServiceUserIdentity(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &NovaWebhook{}

	minimal := validNova()
	minimal.Spec.ServiceUser = ServiceUserSpec{
		SecretRef: commonv1.SecretRefSpec{Name: "nova-service-password"},
	}
	g.Expect(w.Default(context.Background(), minimal)).To(gomega.Succeed())
	g.Expect(minimal.Spec.ServiceUser.Username).To(gomega.Equal("nova"))
	g.Expect(minimal.Spec.ServiceUser.ProjectName).To(gomega.Equal("service"))
	g.Expect(minimal.Spec.ServiceUser.UserDomainName).To(gomega.Equal("Default"))
	g.Expect(minimal.Spec.ServiceUser.ProjectDomainName).To(gomega.Equal("Default"))
	g.Expect(minimal.Spec.ServiceUser.SecretRef.Key).To(gomega.Equal("password"))

	explicit := validNova()
	explicit.Spec.ServiceUser = ServiceUserSpec{
		Username:          "compute-svc",
		ProjectName:       "compute",
		UserDomainName:    "Corp",
		ProjectDomainName: "Corp",
		SecretRef:         commonv1.SecretRefSpec{Name: "custom-secret", Key: "custom-key"},
	}
	g.Expect(w.Default(context.Background(), explicit)).To(gomega.Succeed())
	g.Expect(explicit.Spec.ServiceUser.Username).To(gomega.Equal("compute-svc"))
	g.Expect(explicit.Spec.ServiceUser.ProjectDomainName).To(gomega.Equal("Corp"))
	g.Expect(explicit.Spec.ServiceUser.SecretRef.Key).To(gomega.Equal("custom-key"))
}

// The Secret name has no default (the same value has to reach the Neutron
// metadata agent), but the key inside it does.
func TestNovaDefault_SharedSecretKey(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &NovaWebhook{}

	obj := validNova()
	obj.Spec.Metadata.SharedSecretRef = commonv1.SecretRefSpec{Name: "nova-metadata-secret"}

	g.Expect(w.Default(context.Background(), obj)).To(gomega.Succeed())
	g.Expect(obj.Spec.Metadata.SharedSecretRef.Name).To(gomega.Equal("nova-metadata-secret"))
	g.Expect(obj.Spec.Metadata.SharedSecretRef.Key).To(gomega.Equal(DefaultSharedSecretKey))
}

// --- Validating webhook ---

func TestNovaValidateCreate_ValidSpecAccepted(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &NovaWebhook{}

	_, err := w.ValidateCreate(context.Background(), validNova())
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

func TestNovaValidateCreate_RejectionTable(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(o *Nova)
		wantPath string
		wantSub  string
	}{
		{
			// Both halves run their own db-sync against a different set of
			// migrations, so one schema would carry both migration histories.
			name:     "both database blocks on one schema rejected",
			mutate:   func(o *Nova) { o.Spec.APIDatabase.Database = "nova" },
			wantPath: "spec.apiDatabase.database",
			wantSub:  "apiDatabase and database must name different schemas",
		},
		{
			name: "mismatched credentialsMode rejected",
			mutate: func(o *Nova) {
				o.Spec.APIDatabase.CredentialsMode = commonv1.CredentialsModeDynamic
			},
			wantPath: "spec.apiDatabase.credentialsMode",
			wantSub:  "apiDatabase and database must use the same credentialsMode",
		},
		{
			name: "console deployment while the proxy is disabled rejected",
			mutate: func(o *Nova) {
				o.Spec.ConsoleProxy.Enabled = ptr.To(false)
				o.Spec.ConsoleProxy.Deployment = &DeploymentSpec{Replicas: 1}
			},
			wantPath: "spec.consoleProxy.deployment",
			wantSub:  "consoleProxy.deployment must not be set when consoleProxy.enabled is false",
		},
		{
			// cell0 is provisioned as "<database>_cell0", so an API block naming it
			// would run the nova_api migrations into the schema map_cell0 maps.
			name:     "apiDatabase naming the derived cell0 schema rejected",
			mutate:   func(o *Nova) { o.Spec.APIDatabase.Database = "nova_cell0" },
			wantPath: "spec.apiDatabase.database",
			wantSub:  "apiDatabase must not name the cell0 schema derived from database",
		},
		{
			name:     "cell database name leaving no room for the cell0 suffix rejected",
			mutate:   func(o *Nova) { o.Spec.Database.Database = strings.Repeat("n", 59) },
			wantPath: "spec.database.database",
			wantSub:  "database.database must be at most 58 characters",
		},
		{
			name:     "image with both a tag and a digest rejected",
			mutate:   func(o *Nova) { o.Spec.Image.Digest = "sha256:" + strings.Repeat("a", 64) },
			wantPath: "spec.image",
			wantSub:  "exactly one of image.tag or image.digest must be set",
		},
		{
			name:     "apiDatabase with both clusterRef and host rejected",
			mutate:   func(o *Nova) { o.Spec.APIDatabase.Host = "db.example.com" },
			wantPath: "spec.apiDatabase",
			wantSub:  "exactly one of clusterRef or host must be set",
		},
		{
			name:     "cell database with both clusterRef and host rejected",
			mutate:   func(o *Nova) { o.Spec.Database.Host = "db.example.com" },
			wantPath: "spec.database",
			wantSub:  "exactly one of clusterRef or host must be set",
		},
		{
			// Both blocks run Dynamic, so the pairing rule stays quiet and the
			// rejection belongs to the block that lost its clusterRef.
			name: "Dynamic apiDatabase on a brownfield host rejected",
			mutate: func(o *Nova) {
				o.Spec.APIDatabase.ClusterRef = nil
				o.Spec.APIDatabase.Host = "db.example.com"
				o.Spec.APIDatabase.CredentialsMode = commonv1.CredentialsModeDynamic
				o.Spec.Database.CredentialsMode = commonv1.CredentialsModeDynamic
			},
			wantPath: "spec.apiDatabase.credentialsMode",
			wantSub:  "credentialsMode Dynamic requires clusterRef",
		},
		{
			name: "Dynamic cell database on a brownfield host rejected",
			mutate: func(o *Nova) {
				o.Spec.Database.ClusterRef = nil
				o.Spec.Database.Host = "db.example.com"
				o.Spec.APIDatabase.CredentialsMode = commonv1.CredentialsModeDynamic
				o.Spec.Database.CredentialsMode = commonv1.CredentialsModeDynamic
			},
			wantPath: "spec.database.credentialsMode",
			wantSub:  "credentialsMode Dynamic requires clusterRef",
		},
		{
			name:     "cache with both servers and clusterRef rejected",
			mutate:   func(o *Nova) { o.Spec.Cache.Servers = []string{"memcached-0:11211"} },
			wantPath: "spec.cache",
			wantSub:  "exactly one of clusterRef or servers must be set",
		},
		{
			// The name is rendered into the [cache] and [keystone_authtoken] server
			// lists, so a newline in it injects config lines.
			name:     "cache clusterRef name with a newline rejected",
			mutate:   func(o *Nova) { o.Spec.Cache.ClusterRef.Name = "memcached\n[workarounds]" },
			wantPath: "spec.cache.clusterRef.name",
			wantSub:  "rendered verbatim into the service configuration file",
		},
		{
			name: "messaging with both clusterRef and secretRef rejected",
			mutate: func(o *Nova) {
				o.Spec.Messaging.SecretRef = &commonv1.SecretRefSpec{Name: "nova-transport-url"}
			},
			wantPath: "spec.messaging",
			wantSub:  "exactly one of clusterRef or secretRef must be set",
		},
		{
			name:     "messaging with neither clusterRef nor secretRef rejected",
			mutate:   func(o *Nova) { o.Spec.Messaging.ClusterRef = nil },
			wantPath: "spec.messaging",
			wantSub:  "exactly one of clusterRef or secretRef must be set",
		},
		{
			name:     "empty keystoneEndpoint rejected",
			mutate:   func(o *Nova) { o.Spec.KeystoneEndpoint = "" },
			wantPath: "spec.keystoneEndpoint",
			wantSub:  "keystoneEndpoint must be set",
		},
		{
			name:     "keystoneEndpoint without a scheme rejected",
			mutate:   func(o *Nova) { o.Spec.KeystoneEndpoint = "keystone.openstack.svc:5000" },
			wantPath: "spec.keystoneEndpoint",
			wantSub:  "scheme must be http or https",
		},
		{
			name:     "unparseable keystoneEndpoint rejected",
			mutate:   func(o *Nova) { o.Spec.KeystoneEndpoint = "http://[::1" },
			wantPath: "spec.keystoneEndpoint",
			wantSub:  "must be a valid URL",
		},
		{
			name:     "keystonePublicEndpoint without a scheme rejected",
			mutate:   func(o *Nova) { o.Spec.KeystonePublicEndpoint = "keystone.example.com" },
			wantPath: "spec.keystonePublicEndpoint",
			wantSub:  "scheme must be http or https",
		},
		{
			name: "placement override without a host rejected",
			mutate: func(o *Nova) {
				o.Spec.Endpoints.Placement.Override = "http://"
			},
			wantPath: "spec.endpoints.placement.override",
			wantSub:  "URL must include a host",
		},
		{
			name: "neutron override without a scheme rejected",
			mutate: func(o *Nova) {
				o.Spec.Endpoints.Neutron.Override = "neutron.openstack.svc:9696"
			},
			wantPath: "spec.endpoints.neutron.override",
			wantSub:  "scheme must be http or https",
		},
		{
			name: "glance override without a scheme rejected",
			mutate: func(o *Nova) {
				o.Spec.Endpoints.Glance.Override = "glance.openstack.svc:9292"
			},
			wantPath: "spec.endpoints.glance.override",
			wantSub:  "scheme must be http or https",
		},
		{
			name: "cinder override without a scheme rejected",
			mutate: func(o *Nova) {
				o.Spec.Endpoints.Cinder = NovaOptionalEndpointSpec{
					Enabled:  true,
					Override: "cinder.openstack.svc:8776",
				}
			},
			wantPath: "spec.endpoints.cinder.override",
			wantSub:  "scheme must be http or https",
		},
		{
			name: "barbican override without a scheme rejected",
			mutate: func(o *Nova) {
				o.Spec.Endpoints.Barbican = NovaOptionalEndpointSpec{
					Enabled:  true,
					Override: "barbican.openstack.svc:9311",
				}
			},
			wantPath: "spec.endpoints.barbican.override",
			wantSub:  "scheme must be http or https",
		},
		{
			// The schedule carries no CRD pattern, so the webhook's
			// cron.ParseStandard call is the only gate between a typo and a CronJob
			// the API server refuses to create.
			name:     "dbArchive four-field schedule rejected",
			mutate:   func(o *Nova) { o.Spec.DBArchive = &DBArchiveSpec{Schedule: "* * * *"} },
			wantPath: "spec.dbArchive.schedule",
			wantSub:  "invalid cron expression",
		},
		{
			name:     "dbArchive zero maxRows rejected",
			mutate:   func(o *Nova) { o.Spec.DBArchive = &DBArchiveSpec{MaxRows: ptr.To(int32(0))} },
			wantPath: "spec.dbArchive.maxRows",
			wantSub:  "maxRows must be at least 1",
		},
		{
			name:     "dbArchive zero retentionDays rejected",
			mutate:   func(o *Nova) { o.Spec.DBArchive = &DBArchiveSpec{RetentionDays: ptr.To(int32(0))} },
			wantPath: "spec.dbArchive.retentionDays",
			wantSub:  "retentionDays must be at least 1",
		},
		{
			name:     "dbArchive negative sleep rejected",
			mutate:   func(o *Nova) { o.Spec.DBArchive = &DBArchiveSpec{Sleep: ptr.To(int32(-1))} },
			wantPath: "spec.dbArchive.sleep",
			wantSub:  "sleep must be non-negative",
		},
		{
			name:     "zero scheduler workers rejected",
			mutate:   func(o *Nova) { o.Spec.Scheduler.Workers = ptr.To(int32(0)) },
			wantPath: "spec.scheduler.workers",
			wantSub:  "workers must be at least 1",
		},
		{
			name:     "zero conductor workers rejected",
			mutate:   func(o *Nova) { o.Spec.Conductor.Workers = ptr.To(int32(0)) },
			wantPath: "spec.conductor.workers",
			wantSub:  "workers must be at least 1",
		},
		{
			name:     "zero api replicas rejected",
			mutate:   func(o *Nova) { o.Spec.API.Deployment.Replicas = 0 },
			wantPath: "spec.api.deployment.replicas",
			wantSub:  "replicas must be at least 1",
		},
		{
			name: "conductor grace period under ten seconds rejected",
			mutate: func(o *Nova) {
				o.Spec.Conductor.Deployment.TerminationGracePeriodSeconds = ptr.To(int64(5))
			},
			wantPath: "spec.conductor.deployment.terminationGracePeriodSeconds",
			wantSub:  "terminationGracePeriodSeconds must be at least 10",
		},
		{
			// A preStop sleep that fills the whole grace window leaves no time to
			// drain between the end of the sleep and the kubelet's SIGKILL.
			name: "scheduler preStop sleep equal to the grace period rejected",
			mutate: func(o *Nova) {
				o.Spec.Scheduler.Deployment.TerminationGracePeriodSeconds = ptr.To(int64(20))
				o.Spec.Scheduler.Deployment.PreStopSleepSeconds = ptr.To(int64(20))
			},
			wantPath: "spec.scheduler.deployment.preStopSleepSeconds",
			wantSub:  "preStopSleepSeconds (20) must be strictly less than terminationGracePeriodSeconds (20)",
		},
		{
			name: "metadata Recreate strategy with a rollingUpdate block rejected",
			mutate: func(o *Nova) {
				o.Spec.Metadata.Deployment.Strategy = &appsv1.DeploymentStrategy{
					Type:          appsv1.RecreateDeploymentStrategyType,
					RollingUpdate: &appsv1.RollingUpdateDeployment{},
				}
			},
			wantPath: "spec.metadata.deployment.strategy.rollingUpdate",
			wantSub:  "rollingUpdate must not be set when strategy.type is Recreate",
		},
		{
			name: "console proxy memory request above its limit rejected",
			mutate: func(o *Nova) {
				o.Spec.ConsoleProxy.Deployment = &DeploymentSpec{
					Replicas: 1,
					Resources: &corev1.ResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")},
						Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
					},
				}
			},
			wantPath: "spec.consoleProxy.deployment.resources.requests.memory",
			wantSub:  "memory request must not exceed limit (1Gi)",
		},
		{
			name: "metadata harakiri beyond the drain window rejected",
			mutate: func(o *Nova) {
				o.Spec.Metadata.UWSGI = &UWSGISpec{Harakiri: ptr.To(int32(60))}
			},
			wantPath: "spec.metadata.uwsgi.harakiri",
			wantSub:  "harakiri (60) must be strictly less than terminationGracePeriodSeconds - preStopSleepSeconds (25)",
		},
		{
			// The default window leaves 30 - 5 = 25 seconds to drain. A request
			// harakiri aborts only at that mark gets no time to be answered.
			name:     "api harakiri equal to the drain window rejected",
			mutate:   func(o *Nova) { o.Spec.API.UWSGI = &UWSGISpec{Harakiri: ptr.To(int32(25))} },
			wantPath: "spec.api.uwsgi.harakiri",
			wantSub:  "harakiri (25) must be strictly less than terminationGracePeriodSeconds - preStopSleepSeconds (25)",
		},
		{
			// minReplicas falls back to the API replica count, so the HPA would
			// carry a floor above its ceiling, which the API server refuses.
			name: "autoscaling ceiling below the api replicas without minReplicas rejected",
			mutate: func(o *Nova) {
				o.Spec.Autoscaling = &AutoscalingSpec{MaxReplicas: 2, TargetCPUUtilization: ptr.To(int32(80))}
			},
			wantPath: "spec.autoscaling.maxReplicas",
			wantSub:  "maxReplicas must be >= spec.api.deployment.replicas (3) when minReplicas is not set",
		},
		{
			name:     "autoscaling without a utilization target rejected",
			mutate:   func(o *Nova) { o.Spec.Autoscaling = &AutoscalingSpec{MaxReplicas: 5} },
			wantPath: "spec.autoscaling",
			wantSub:  "at least one of targetCPUUtilization or targetMemoryUtilization must be set",
		},
		{
			name: "autoscaling minReplicas above maxReplicas rejected",
			mutate: func(o *Nova) {
				o.Spec.Autoscaling = &AutoscalingSpec{
					MinReplicas:          ptr.To(int32(6)),
					MaxReplicas:          5,
					TargetCPUUtilization: ptr.To(int32(80)),
				}
			},
			wantPath: "spec.autoscaling.minReplicas",
			wantSub:  "minReplicas must not exceed maxReplicas",
		},
		{
			name: "autoscaling zero maxReplicas rejected",
			mutate: func(o *Nova) {
				o.Spec.Autoscaling = &AutoscalingSpec{MaxReplicas: 0, TargetCPUUtilization: ptr.To(int32(80))}
			},
			wantPath: "spec.autoscaling.maxReplicas",
			wantSub:  "maxReplicas must be at least 1",
		},
		{
			name: "autoscaling zero minReplicas rejected",
			mutate: func(o *Nova) {
				o.Spec.Autoscaling = &AutoscalingSpec{
					MinReplicas:          ptr.To(int32(0)),
					MaxReplicas:          5,
					TargetCPUUtilization: ptr.To(int32(80)),
				}
			},
			wantPath: "spec.autoscaling.minReplicas",
			wantSub:  "minReplicas must be at least 1",
		},
		{
			name: "autoscaling CPU target above 100 rejected",
			mutate: func(o *Nova) {
				o.Spec.Autoscaling = &AutoscalingSpec{MaxReplicas: 5, TargetCPUUtilization: ptr.To(int32(101))}
			},
			wantPath: "spec.autoscaling.targetCPUUtilization",
			wantSub:  "targetCPUUtilization must be between 1 and 100",
		},
		{
			name: "autoscaling zero memory target rejected",
			mutate: func(o *Nova) {
				o.Spec.Autoscaling = &AutoscalingSpec{MaxReplicas: 5, TargetMemoryUtilization: ptr.To(int32(0))}
			},
			wantPath: "spec.autoscaling.targetMemoryUtilization",
			wantSub:  "targetMemoryUtilization must be between 1 and 100",
		},
		{
			name: "api gateway without a hostname rejected",
			mutate: func(o *Nova) {
				o.Spec.Gateway = &GatewaySpec{ParentRef: GatewayParentRefSpec{Name: "gateway"}}
			},
			wantPath: "spec.gateway.hostname",
			wantSub:  "hostname must be set when spec.gateway is configured",
		},
		{
			name: "api gateway without a parentRef name rejected",
			mutate: func(o *Nova) {
				o.Spec.Gateway = &GatewaySpec{Hostname: "nova.example.com"}
			},
			wantPath: "spec.gateway.parentRef.name",
			wantSub:  "parentRef.name must be set when spec.gateway is configured",
		},
		{
			name: "metadata gateway without a hostname rejected",
			mutate: func(o *Nova) {
				o.Spec.Metadata.Gateway = &GatewaySpec{ParentRef: GatewayParentRefSpec{Name: "gateway"}}
			},
			wantPath: "spec.metadata.gateway.hostname",
			wantSub:  "hostname must be set when spec.metadata.gateway is configured",
		},
		{
			name: "metadata gateway without a parentRef name rejected",
			mutate: func(o *Nova) {
				o.Spec.Metadata.Gateway = &GatewaySpec{Hostname: "metadata.example.com"}
			},
			wantPath: "spec.metadata.gateway.parentRef.name",
			wantSub:  "parentRef.name must be set when spec.metadata.gateway is configured",
		},
		{
			name: "console gateway without a hostname rejected",
			mutate: func(o *Nova) {
				o.Spec.ConsoleProxy.Gateway = &GatewaySpec{ParentRef: GatewayParentRefSpec{Name: "gateway"}}
			},
			wantPath: "spec.consoleProxy.gateway.hostname",
			wantSub:  "hostname must be set when spec.consoleProxy.gateway is configured",
		},
		{
			name: "console gateway without a parentRef name rejected",
			mutate: func(o *Nova) {
				o.Spec.ConsoleProxy.Gateway = &GatewaySpec{Hostname: "console.example.com"}
			},
			wantPath: "spec.consoleProxy.gateway.parentRef.name",
			wantSub:  "parentRef.name must be set when spec.consoleProxy.gateway is configured",
		},
		{
			// The page and the WebSocket are served from the root of the console
			// hostname, so a prefix route would match neither.
			name: "console gateway path prefix rejected",
			mutate: func(o *Nova) {
				o.Spec.ConsoleProxy.Gateway = &GatewaySpec{
					ParentRef: GatewayParentRefSpec{Name: "gateway"},
					Hostname:  "console.example.com",
					Path:      "/console",
				}
			},
			wantPath: "spec.consoleProxy.gateway.path",
			wantSub:  "the console page and its WebSocket are served from the root",
		},
		{
			name:     "networkPolicy without an ingress source rejected",
			mutate:   func(o *Nova) { o.Spec.NetworkPolicy = &NetworkPolicySpec{} },
			wantPath: "spec.networkPolicy.ingress",
			wantSub:  "at least one ingress source must be specified",
		},
		{
			name:     "empty serviceUser secretRef name rejected",
			mutate:   func(o *Nova) { o.Spec.ServiceUser.SecretRef.Name = "" },
			wantPath: "spec.serviceUser.secretRef.name",
			wantSub:  "secretRef.name must be set",
		},
		{
			name:     "empty metadata sharedSecretRef name rejected",
			mutate:   func(o *Nova) { o.Spec.Metadata.SharedSecretRef.Name = "" },
			wantPath: "spec.metadata.sharedSecretRef.name",
			wantSub:  "sharedSecretRef.name must be set",
		},
		{
			name:     "unsupported logging level rejected",
			mutate:   func(o *Nova) { o.Spec.Logging = &LoggingSpec{Level: "TRACE"} },
			wantPath: "spec.logging.level",
			wantSub:  `Unsupported value: "TRACE"`,
		},
		{
			name: "empty per-logger name rejected",
			mutate: func(o *Nova) {
				o.Spec.Logging = &LoggingSpec{PerLoggerLevels: map[string]string{"": "INFO"}}
			},
			wantPath: "spec.logging.perLoggerLevels",
			wantSub:  "logger name must not be empty",
		},
		{
			// A logger name renders into the [DEFAULT] default_log_levels CSV, and
			// no CRD enum backs the map, so the webhook is the only gate between a
			// newline and an injected config line.
			name: "per-logger name with a newline rejected",
			mutate: func(o *Nova) {
				o.Spec.Logging = &LoggingSpec{PerLoggerLevels: map[string]string{
					"nova\ntransport_url = rabbit://x": "DEBUG",
				}}
			},
			wantPath: "spec.logging.perLoggerLevels",
			wantSub:  "logger name must not contain a newline or carriage return: it is rendered verbatim into nova.conf",
		},
		{
			name: "per-logger level outside the enum rejected",
			mutate: func(o *Nova) {
				o.Spec.Logging = &LoggingSpec{PerLoggerLevels: map[string]string{"nova": "VERBOSE"}}
			},
			wantPath: "spec.logging.perLoggerLevels[nova]",
			wantSub:  "level must be one of DEBUG, INFO, WARNING, ERROR, CRITICAL",
		},
		{
			name: "unknown extraConfig option rejected",
			mutate: func(o *Nova) {
				o.Spec.ExtraConfig = map[string]map[string]string{"DEFAULT": {"not_an_option": "x"}}
			},
			wantPath: "spec.extraConfig[DEFAULT][not_an_option]",
			wantSub:  "no such option in the nova 2025.2 option catalog",
		},
		{
			name: "rejected owned key in extraConfig refused",
			mutate: func(o *Nova) {
				o.Spec.ExtraConfig = map[string]map[string]string{
					"api_database": {"connection": "mysql+pymysql://root:pw@db/nova_api"},
				}
			},
			wantPath: "spec.extraConfig[api_database][connection]",
			wantSub:  "connection is managed via spec.apiDatabase",
		},
		{
			name: "unknown extraConfig section rejected",
			mutate: func(o *Nova) {
				o.Spec.ExtraConfig = map[string]map[string]string{"no_such_section": {"x": "y"}}
			},
			wantPath: "spec.extraConfig[no_such_section][x]",
			wantSub:  "no such section in the nova 2025.2 option catalog",
		},
		{
			name: "empty extraConfig section rejected",
			mutate: func(o *Nova) {
				o.Spec.ExtraConfig = map[string]map[string]string{"": {"debug": "true"}}
			},
			wantPath: "spec.extraConfig",
			wantSub:  "extraConfig section name must not be empty",
		},
		{
			name: "empty extraConfig key rejected",
			mutate: func(o *Nova) {
				o.Spec.ExtraConfig = map[string]map[string]string{"DEFAULT": {"": "true"}}
			},
			wantPath: "spec.extraConfig[DEFAULT]",
			wantSub:  "extraConfig key must not be empty",
		},
		{
			name: "extraConfig section with a newline rejected",
			mutate: func(o *Nova) {
				o.Spec.ExtraConfig = map[string]map[string]string{
					"DEFAULT]\n[workarounds": {"disable_rootwrap": "true"},
				}
			},
			wantPath: "spec.extraConfig",
			wantSub:  "extraConfig section name must not contain a newline or carriage return",
		},
		{
			// The renderer writes "%s = %s" verbatim, and the ownership and catalog
			// gates key on (section, key) names without looking inside a value, so
			// a newline behind an admitted key smuggles the Rejected transport_url
			// past both.
			name: "extraConfig value with a newline rejected",
			mutate: func(o *Nova) {
				o.Spec.ExtraConfig = map[string]map[string]string{
					"DEFAULT": {"debug": "true\ntransport_url = rabbit://x"},
				}
			},
			wantPath: "spec.extraConfig[DEFAULT][debug]",
			wantSub:  "extraConfig key and value must not contain a newline or carriage return",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			w := &NovaWebhook{}
			obj := validNova()
			tc.mutate(obj)

			_, err := w.ValidateCreate(context.Background(), obj)
			g.Expect(err).To(gomega.HaveOccurred())
			g.Expect(err.Error()).To(gomega.ContainSubstring(tc.wantSub))
			g.Expect(err.Error()).To(gomega.ContainSubstring(tc.wantPath))
		})
	}
}

func TestNovaValidateCreate_NameLengthBoundedByArchiveCronJob(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &NovaWebhook{}

	g.Expect(MaxNovaNameLength).To(gomega.Equal(41))

	atLimit := validNova()
	atLimit.Name = strings.Repeat("n", MaxNovaNameLength)
	_, err := w.ValidateCreate(context.Background(), atLimit)
	g.Expect(err).NotTo(gomega.HaveOccurred(),
		"a name that still fits the 52-character CronJob budget must be accepted")

	tooLong := validNova()
	tooLong.Name = strings.Repeat("n", MaxNovaNameLength+1)
	_, err = w.ValidateCreate(context.Background(), tooLong)
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("metadata.name")))
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("name must be at most 41 characters")))
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("db-archive")))
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("52")))
}

// The bound is create-only. metadata.name is immutable, so on update it could
// only ever fire against a CR a pre-upgrade operator already admitted, and the
// validating webhook registers the update verb, so it also sees the
// finalizer-removal update reconcileDelete issues. Rejecting that would wedge the
// grandfathered CR in Terminating forever, with no field left to edit to repair
// it.
func TestNovaValidateUpdate_OverlongNameStaysUpdatable(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &NovaWebhook{}

	grandfathered := validNova()
	grandfathered.Name = strings.Repeat("n", MaxNovaNameLength+1)
	grandfathered.Finalizers = []string{"nova.openstack.c5c3.io/finalizer"}

	deleting := grandfathered.DeepCopy()
	deleting.Finalizers = nil

	_, err := w.ValidateUpdate(context.Background(), grandfathered, deleting)
	g.Expect(err).NotTo(gomega.HaveOccurred(),
		"an over-long grandfathered CR must stay updatable, or its deletion never completes")
}

// The boundary of the cell0 bound and of the console path rule: the longest cell
// schema name that still leaves room for "_cell0", and a console route on the
// root of its hostname, are both admitted.
func TestNovaValidateCreate_Cell0BoundAndRootConsolePathAccepted(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &NovaWebhook{}

	obj := validNova()
	obj.Spec.Database.Database = strings.Repeat("n", 58)
	obj.Spec.ConsoleProxy.Gateway = &GatewaySpec{
		ParentRef: GatewayParentRefSpec{Name: "gateway"},
		Hostname:  "console.example.com",
		Path:      "/",
	}

	_, err := w.ValidateCreate(context.Background(), obj)
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

// An omitted credentialsMode means Static, so a CR that spells Static out on one
// database block and omits it on the other names one mode for both. Comparing
// the raw fields would reject that CR over a spelling. The CEL twin on NovaSpec
// has the same trap in a has() default missing on either side, which the
// CRD-only envtest pins.
func TestNovaValidateCreate_ExplicitAndOmittedStaticCredentialsModeAccepted(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(o *Nova)
	}{
		{"explicit on apiDatabase", func(o *Nova) { o.Spec.APIDatabase.CredentialsMode = commonv1.CredentialsModeStatic }},
		{"explicit on database", func(o *Nova) { o.Spec.Database.CredentialsMode = commonv1.CredentialsModeStatic }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			w := &NovaWebhook{}
			obj := validNova()
			tc.mutate(obj)

			_, err := w.ValidateCreate(context.Background(), obj)
			g.Expect(err).NotTo(gomega.HaveOccurred())
		})
	}
}

// The accepted side of the autoscaling bounds. An HPA whose floor falls back to
// the API replica count and whose ceiling equals it is a valid HPA, and 100 and 1
// percent are in range; an off-by-one on any of them would refuse a CR the API
// server takes.
func TestNovaValidateCreate_AutoscalingAtItsBoundsAccepted(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &NovaWebhook{}

	obj := validNova()
	obj.Spec.Autoscaling = &AutoscalingSpec{
		MaxReplicas:             obj.Spec.API.Deployment.Replicas,
		TargetCPUUtilization:    ptr.To(int32(100)),
		TargetMemoryUtilization: ptr.To(int32(1)),
	}

	_, err := w.ValidateCreate(context.Background(), obj)
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

// A Deployment takes a priorityClassName that names no PriorityClass, and the
// failure only surfaces when its ReplicaSet is refused every pod, as an event on
// an object the CR author never looks at. The lookup moves that failure to
// admission. It goes through the injected reader, which the fake client stands in
// for, and an existing class has to pass it.
func TestNovaValidateCreate_PriorityClassMustExist(t *testing.T) {
	existing := &schedulingv1.PriorityClass{
		ObjectMeta: metav1.ObjectMeta{Name: "nova-critical"},
		Value:      1000000,
	}
	w := &NovaWebhook{Client: fake.NewClientBuilder().WithObjects(existing).Build()}

	t.Run("missing class rejected", func(t *testing.T) {
		g := gomega.NewWithT(t)
		obj := validNova()
		obj.Spec.Scheduler.Deployment.PriorityClassName = ptr.To("nova-critcal")

		_, err := w.ValidateCreate(context.Background(), obj)
		g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("spec.scheduler.deployment.priorityClassName")))
		g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(`Not found: "nova-critcal"`)))
	})

	t.Run("existing class accepted", func(t *testing.T) {
		g := gomega.NewWithT(t)
		obj := validNova()
		obj.Spec.Scheduler.Deployment.PriorityClassName = ptr.To("nova-critical")

		_, err := w.ValidateCreate(context.Background(), obj)
		g.Expect(err).NotTo(gomega.HaveOccurred())
	})
}

// Every typed field rendered verbatim into nova.conf and the compute fragment
// rejects a newline: the region, the four service-user names and the console
// hostname. The fragment is what every hypervisor loads, and spec.extraConfig
// never reaches it, so these fields are the only way a CR author could.
func TestNovaValidate_TypedFieldsRejectControlChars(t *testing.T) {
	const injected = "RegionOne\n[workarounds]\ndisable_rootwrap = true"

	for _, tc := range []struct {
		path   string
		mutate func(o *Nova)
	}{
		{"spec.region", func(o *Nova) { o.Spec.Region = injected }},
		{"spec.serviceUser.username", func(o *Nova) { o.Spec.ServiceUser.Username = injected }},
		{"spec.serviceUser.projectName", func(o *Nova) { o.Spec.ServiceUser.ProjectName = injected }},
		{"spec.serviceUser.userDomainName", func(o *Nova) { o.Spec.ServiceUser.UserDomainName = injected }},
		{"spec.serviceUser.projectDomainName", func(o *Nova) { o.Spec.ServiceUser.ProjectDomainName = injected }},
		{"spec.consoleProxy.gateway.hostname", func(o *Nova) {
			o.Spec.ConsoleProxy.Gateway = &GatewaySpec{
				ParentRef: GatewayParentRefSpec{Name: "gateway"},
				Hostname:  "console.example.com\r\n",
			}
		}},
	} {
		t.Run(tc.path, func(t *testing.T) {
			g := gomega.NewWithT(t)
			w := &NovaWebhook{}
			obj := validNova()
			tc.mutate(obj)

			_, err := w.ValidateCreate(context.Background(), obj)
			g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(tc.path)))
			g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(
				"rendered verbatim into nova.conf and nova-compute.conf")))

			_, err = w.ValidateUpdate(context.Background(), validNova(), obj)
			g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(tc.path)),
				"the rule is shared with update, so a later edit cannot introduce the newline either")
		})
	}
}

// A Nova named "<x><suffix>" builds children under names the Nova "<x>" in the
// same namespace already uses: the nova_api MariaDB objects of "<x>" are the
// cell schema's objects of "<x>-api", and the process Deployments of "<x>" are
// the API Deployments of the others. Deleting either CR would delete the other's
// objects, so the name is refused at create.
func TestNovaValidateCreate_NameSuffixCollidingWithASiblingsChildRejected(t *testing.T) {
	for _, suffix := range []string{"-api", "-metadata", "-scheduler", "-conductor", "-novncproxy", "-console"} {
		t.Run(suffix, func(t *testing.T) {
			g := gomega.NewWithT(t)
			w := &NovaWebhook{}
			obj := validNova()
			obj.Name = "nova" + suffix

			_, err := w.ValidateCreate(context.Background(), obj)
			g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("metadata.name")))
			g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(
				"name must not end in \"" + suffix + "\": a Nova named \"nova\"")))

			// Create-only, like the length bound: an admitted CR stays updatable.
			_, err = w.ValidateUpdate(context.Background(), obj, obj.DeepCopy())
			g.Expect(err).NotTo(gomega.HaveOccurred())
		})
	}

	// The Nova "nova" with spec.database.database "nova" names its cell0 Database
	// and Grant "nova-nova-cell0", which is what a Nova of that name calls its
	// cell schema's objects.
	t.Run("-cell0", func(t *testing.T) {
		g := gomega.NewWithT(t)
		w := &NovaWebhook{}
		obj := validNova()
		obj.Name = "nova-nova-cell0"

		_, err := w.ValidateCreate(context.Background(), obj)
		g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("metadata.name")))
		g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(
			"name must not end in \"-cell0\": a Nova in this namespace names its cell0 MariaDB Database and Grant")))

		_, err = w.ValidateUpdate(context.Background(), obj, obj.DeepCopy())
		g.Expect(err).NotTo(gomega.HaveOccurred())
	})

	t.Run("a suffix that only starts like one is accepted", func(t *testing.T) {
		g := gomega.NewWithT(t)
		w := &NovaWebhook{}
		obj := validNova()
		obj.Name = "nova-apiary"

		_, err := w.ValidateCreate(context.Background(), obj)
		g.Expect(err).NotTo(gomega.HaveOccurred())
	})
}

// The webhook twin of the transition rules on both database blocks. A renamed
// schema re-points the processes at an empty one while the cell mappings keep
// naming the old, and a mode flip re-targets the whole connection.
func TestNovaValidateUpdate_DatabaseBlocksImmutable(t *testing.T) {
	for _, tc := range []struct {
		name     string
		edit     func(o *Nova)
		wantPath string
		wantSub  string
	}{
		{
			name:     "apiDatabase schema renamed",
			edit:     func(o *Nova) { o.Spec.APIDatabase.Database = "nova_api_new" },
			wantPath: "spec.apiDatabase.database",
			wantSub:  "apiDatabase.database is immutable",
		},
		{
			name:     "cell schema renamed",
			edit:     func(o *Nova) { o.Spec.Database.Database = "nova_new" },
			wantPath: "spec.database.database",
			wantSub:  "database.database is immutable: the cell mappings store the schema name",
		},
		{
			name: "apiDatabase moved from clusterRef to host",
			edit: func(o *Nova) {
				o.Spec.APIDatabase.ClusterRef = nil
				o.Spec.APIDatabase.Host = "db.example.com"
			},
			wantPath: "spec.apiDatabase.clusterRef",
			wantSub:  "apiDatabase mode (managed clusterRef vs brownfield host) is immutable",
		},
		{
			name: "cell database moved from clusterRef to host",
			edit: func(o *Nova) {
				o.Spec.Database.ClusterRef = nil
				o.Spec.Database.Host = "db.example.com"
			},
			wantPath: "spec.database.clusterRef",
			wantSub:  "database mode (managed clusterRef vs brownfield host) is immutable",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			w := &NovaWebhook{}
			newObj := validNova()
			tc.edit(newObj)

			_, err := w.ValidateUpdate(context.Background(), validNova(), newObj)
			g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(tc.wantPath)))
			g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(tc.wantSub)))
		})
	}

	t.Run("an unrelated edit on both blocks is accepted", func(t *testing.T) {
		g := gomega.NewWithT(t)
		w := &NovaWebhook{}
		newObj := validNova()
		newObj.Spec.APIDatabase.ClusterRef.Name = "mariadb-2"
		newObj.Spec.Database.SecretRef.Name = "nova-db-rotated"

		_, err := w.ValidateUpdate(context.Background(), validNova(), newObj)
		g.Expect(err).NotTo(gomega.HaveOccurred())
	})
}

// The finalizer removal reconcileDelete issues is an update, and the validating
// webhook sees it. A spec the webhook admitted earlier can fail today's rules (an
// owned key a later operator rejects, a PriorityClass deleted since), and
// rejecting the removal would hold the CR in Terminating with nothing left to
// edit. A deleting CR whose spec changes is still validated.
func TestNovaValidateUpdate_FinalizerRemovalOnADeletingCRSkipsValidation(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &NovaWebhook{}

	stale := validNova()
	stale.Spec.ExtraConfig = map[string]map[string]string{
		"api_database": {"connection": "mysql+pymysql://root:pw@db/nova_api"},
	}
	stale.Finalizers = []string{"nova.openstack.c5c3.io/finalizer"}
	stale.DeletionTimestamp = &metav1.Time{Time: time.Now()}

	released := stale.DeepCopy()
	released.Finalizers = nil

	_, err := w.ValidateUpdate(context.Background(), stale, released)
	g.Expect(err).NotTo(gomega.HaveOccurred(),
		"the finalizer removal must pass however the unchanged spec fares against today's rules")

	edited := released.DeepCopy()
	edited.Spec.API.Deployment.Replicas = 5
	_, err = w.ValidateUpdate(context.Background(), stale, edited)
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("connection is managed via spec.apiDatabase")),
		"a spec edit on a deleting CR is validated like any other")
}

// --- extraConfig ---

// The Rejected owned keys are refused whatever the catalog says. The
// keystone_authtoken credentials are the case that pins the ordering: no catalog
// enumerates them (keystonemiddleware loads them through an auth plugin), yet the
// ownership registry exempts their keys from the catalog scan, so the rejection
// names the ownership rule and never the missing option.
func TestNovaValidate_ExtraConfigRejectedOwnedKeys(t *testing.T) {
	tests := []struct {
		name        string
		extraConfig map[string]map[string]string
		wantSub     string
	}{
		{
			name:        "transport_url rejected",
			extraConfig: map[string]map[string]string{"DEFAULT": {"transport_url": "rabbit://user:pw@broker"}},
			wantSub:     "transport_url is managed via spec.messaging",
		},
		{
			name:        "keystone_authtoken password rejected",
			extraConfig: map[string]map[string]string{"keystone_authtoken": {"password": "hunter2"}},
			wantSub:     "password is managed via spec.serviceUser.secretRef",
		},
		{
			name:        "neutron metadata_proxy_shared_secret rejected",
			extraConfig: map[string]map[string]string{"neutron": {"metadata_proxy_shared_secret": "s3cret"}},
			wantSub:     "metadata_proxy_shared_secret is managed via spec.metadata.sharedSecretRef",
		},
		{
			name:        "console proxy listen port rejected",
			extraConfig: map[string]map[string]string{"vnc": {"novncproxy_port": "6081"}},
			wantSub:     "novncproxy_port is managed via operator-computed",
		},
		{
			name:        "rabbit TLS switch rejected",
			extraConfig: map[string]map[string]string{"oslo_messaging_rabbit": {"ssl": "false"}},
			wantSub:     "ssl is managed via spec.messaging.tls",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			w := &NovaWebhook{}

			obj := validNova()
			obj.Spec.ExtraConfig = tc.extraConfig
			_, err := w.ValidateCreate(context.Background(), obj)
			g.Expect(err).To(gomega.HaveOccurred())
			g.Expect(err.Error()).To(gomega.ContainSubstring(tc.wantSub))
			g.Expect(err.Error()).NotTo(gomega.ContainSubstring("option catalog"),
				"an owned key is exempt from the catalog scan, so only the ownership rule may fire")
		})
	}
}

func TestNovaValidate_ExtraConfigUnknownOptionRejected(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &NovaWebhook{}

	obj := validNova()
	obj.Spec.ExtraConfig = map[string]map[string]string{
		"DEFAULT": {"not_an_option": "x"},
	}
	_, err := w.ValidateCreate(context.Background(), obj)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("no such option in the nova 2025.2 option catalog"))
}

// A release the build ships no catalog for must not block admission: the check
// fails open with exactly one warning, and the two misses are distinguishable.
func TestNovaValidate_ExtraConfigFailsOpenWithoutCatalog(t *testing.T) {
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
			w := &NovaWebhook{}

			obj := validNova()
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
func TestNovaValidate_ExtraConfigAbsentAndEmptyAccepted(t *testing.T) {
	for _, tc := range []struct {
		name        string
		extraConfig map[string]map[string]string
	}{
		{name: "nil map accepted", extraConfig: nil},
		{name: "empty map accepted", extraConfig: map[string]map[string]string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			w := &NovaWebhook{}

			obj := validNova()
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
func TestNovaValidateUpdate_ExtraConfigCatalogGate(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &NovaWebhook{}

	stale := validNova()
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
	g.Expect(err.Error()).To(gomega.ContainSubstring("no such option in the nova 2025.2 option catalog"))
}

// An OpenStack upgrade is an edit to spec.openStackRelease alone, and it is the
// edit that can invalidate an unchanged extraConfig: [DEFAULT] watch_log_file is
// in the 2025.2 catalog and gone from 2026.1. The release is one of the two
// inputs the update gate compares, so the bump is measured against the new
// catalog instead of carrying an option nova no longer registers into the
// upgrade.
func TestNovaValidateUpdate_ReleaseChangeRevalidatesExtraConfig(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &NovaWebhook{}

	running := validNova()
	running.Spec.ExtraConfig = map[string]map[string]string{
		"DEFAULT": {"watch_log_file": "true"},
	}
	_, err := w.ValidateCreate(context.Background(), running)
	g.Expect(err).NotTo(gomega.HaveOccurred(), "the option is in the 2025.2 catalog")

	upgraded := running.DeepCopy()
	upgraded.Spec.OpenStackRelease = "2026.1"
	_, err = w.ValidateUpdate(context.Background(), running, upgraded)
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("spec.extraConfig[DEFAULT][watch_log_file]")))
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("no such option in the nova 2026.1 option catalog")))
}

// A deprecated option still takes effect, so it is admitted. The warning is the
// one place the author learns the spelling to move to before a release drops the
// old one.
func TestNovaValidate_ExtraConfigDeprecatedOptionWarns(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &NovaWebhook{}

	obj := validNova()
	obj.Spec.ExtraConfig = map[string]map[string]string{
		"DEFAULT": {"logfile": "/var/log/nova/nova.log"},
	}
	warnings, err := w.ValidateCreate(context.Background(), obj)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(warnings).To(gomega.ConsistOf(gomega.ContainSubstring(
		"spec.extraConfig [DEFAULT] logfile: deprecated option in nova 2025.2, replaced by [DEFAULT] log_file")))
}

// A CR admitted clean can gain the newline in a later edit, so the shape rule
// holds on update too. Were it create-only, flipping the debug switch would be
// enough to smuggle transport_url past the ownership gate.
func TestNovaValidateUpdate_ExtraConfigValueWithANewlineRejected(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &NovaWebhook{}

	running := validNova()
	running.Spec.ExtraConfig = map[string]map[string]string{"DEFAULT": {"debug": "true"}}

	edited := running.DeepCopy()
	edited.Spec.ExtraConfig["DEFAULT"]["debug"] = "true\ntransport_url = rabbit://x"

	_, err := w.ValidateUpdate(context.Background(), running, edited)
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("spec.extraConfig[DEFAULT][debug]")))
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(
		"extraConfig key and value must not contain a newline or carriage return")))
}

// --- Warnings ---

// Widening what the archive moves is the edit worth echoing back: the rows land
// in the shadow tables rather than disappearing, but a typo (3 for 30) is
// indistinguishable from an intended change at admission time. Narrowing the
// scope, and leaving it alone, stay silent.
func TestNovaValidateUpdate_WarnsOnReducedDBArchiveRetention(t *testing.T) {
	newObjWith := func(a *DBArchiveSpec) *Nova {
		o := validNova()
		o.Spec.DBArchive = a
		return o
	}
	// wantWarn is the warning a row expects, and empty for a silent one. The texts
	// differ by case because a removed window moves everything, not a band.
	tests := []struct {
		name     string
		old, new *DBArchiveSpec
		wantWarn string
	}{
		{
			name:     "explicit reduction warns",
			old:      &DBArchiveSpec{RetentionDays: ptr.To(int32(30))},
			new:      &DBArchiveSpec{RetentionDays: ptr.To(int32(3))},
			wantWarn: "spec.dbArchive.retentionDays reduced 30 to 3: the next archive run also moves the rows soft-deleted between 3 and 30 days ago",
		},
		{
			// Dropping the window leaves every soft-deleted row eligible, which is
			// the widest the scope gets.
			name:     "removing a set window warns",
			old:      &DBArchiveSpec{RetentionDays: ptr.To(int32(30))},
			new:      &DBArchiveSpec{},
			wantWarn: "spec.dbArchive.retentionDays removed (was 30): the next archive run moves every soft-deleted row",
		},
		{
			name:     "dropping the whole block after a set window warns",
			old:      &DBArchiveSpec{RetentionDays: ptr.To(int32(30))},
			new:      nil,
			wantWarn: "spec.dbArchive.retentionDays removed (was 30): the next archive run moves every soft-deleted row",
		},
		{
			name: "raising the window is silent",
			old:  &DBArchiveSpec{RetentionDays: ptr.To(int32(7))},
			new:  &DBArchiveSpec{RetentionDays: ptr.To(int32(90))},
		},
		{
			// Any edit elsewhere in the CR arrives with the window unchanged, so an
			// equal-value warning would echo on every update.
			name: "keeping the window is silent",
			old:  &DBArchiveSpec{RetentionDays: ptr.To(int32(30))},
			new:  &DBArchiveSpec{RetentionDays: ptr.To(int32(30))},
		},
		{
			name: "no window on either side is silent",
			old:  nil,
			new:  nil,
		},
		{
			// Setting a window where there was none narrows what the run moves.
			name: "setting a window where there was none is silent",
			old:  nil,
			new:  &DBArchiveSpec{RetentionDays: ptr.To(int32(7))},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			w := &NovaWebhook{}

			warnings, err := w.ValidateUpdate(context.Background(), newObjWith(tc.old), newObjWith(tc.new))
			g.Expect(err).NotTo(gomega.HaveOccurred())
			if tc.wantWarn != "" {
				g.Expect(warnings).To(gomega.ContainElement(gomega.ContainSubstring(tc.wantWarn)))
				return
			}
			g.Expect(warnings).NotTo(gomega.ContainElement(
				gomega.ContainSubstring("spec.dbArchive.retentionDays"),
			))
		})
	}
}

// --- spec.targetClusterRef (multicluster routing) ---
//
// The webhook twin of the two transition CEL rules on NovaSpec. Both layers
// carry the rule so it still holds for a CR that reaches the webhook through a
// schema bypass, where re-pointing the ref would abandon every child the
// reconciler placed on the old cluster.

// TestNovaValidateUpdate_TargetClusterRefAddedRejected covers the presence flip
// upwards: the children of a CR created without a target cluster live on the
// management cluster, so naming one afterwards is rejected.
func TestNovaValidateUpdate_TargetClusterRefAddedRejected(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &NovaWebhook{}
	old := validNova()
	newObj := validNova()
	newObj.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "edge-1"}

	_, err := w.ValidateUpdate(context.Background(), old, newObj)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("targetClusterRef is immutable"))
}

// TestNovaValidateUpdate_TargetClusterRefRemovedRejected covers the presence flip
// downwards: dropping the ref would strand the children on the cluster it named.
func TestNovaValidateUpdate_TargetClusterRefRemovedRejected(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &NovaWebhook{}
	old := validNova()
	old.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "edge-1"}
	newObj := validNova()

	_, err := w.ValidateUpdate(context.Background(), old, newObj)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("targetClusterRef is immutable"))
}

// TestNovaValidateUpdate_TargetClusterRefChangedRejected covers a rename, which
// would re-point the reconciler at a cluster that holds none of the children.
func TestNovaValidateUpdate_TargetClusterRefChangedRejected(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &NovaWebhook{}
	old := validNova()
	old.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "edge-1"}
	newObj := validNova()
	newObj.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "edge-2"}

	_, err := w.ValidateUpdate(context.Background(), old, newObj)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("targetClusterRef is immutable"))
}

// TestNovaValidateUpdate_TargetClusterRefUnchangedAccepted proves the check
// freezes only the ref: an unrelated edit on a CR that names a target cluster
// still passes.
func TestNovaValidateUpdate_TargetClusterRefUnchangedAccepted(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &NovaWebhook{}
	old := validNova()
	old.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "edge-1"}
	newObj := old.DeepCopy()
	newObj.Spec.API.Deployment.Replicas = 5

	_, err := w.ValidateUpdate(context.Background(), old, newObj)
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

func TestNovaValidateDelete_AlwaysAccepts(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &NovaWebhook{}

	warnings, err := w.ValidateDelete(context.Background(), validNova())
	g.Expect(warnings).To(gomega.BeNil())
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

// Each non-API Deployment block is measured against the selector of the one
// Deployment it configures, so a topology-spread constraint set there has to
// name that selector exactly. The component values are duplicated from the
// controller package, and one that drifts from the value the controller stamps
// on the pods would reject the very constraint the operator produces. The
// rejection message carries the selector the block required, which is what pins
// each value here.
func TestNovaValidate_TopologySpreadSelectorNamesTheComponent(t *testing.T) {
	// A selector the operator never produces, so every block rejects it and
	// reports the selector it expected instead.
	wrongSelector := []corev1.TopologySpreadConstraint{{
		MaxSkew:           1,
		TopologyKey:       "kubernetes.io/hostname",
		WhenUnsatisfiable: corev1.ScheduleAnyway,
		LabelSelector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "wrong"}},
	}}

	tests := []struct {
		name          string
		mutate        func(o *Nova, tscs []corev1.TopologySpreadConstraint)
		wantComponent string
	}{
		{
			name: "metadata",
			mutate: func(o *Nova, tscs []corev1.TopologySpreadConstraint) {
				o.Spec.Metadata.Deployment.TopologySpreadConstraints = tscs
			},
			wantComponent: componentMetadata,
		},
		{
			name: "scheduler",
			mutate: func(o *Nova, tscs []corev1.TopologySpreadConstraint) {
				o.Spec.Scheduler.Deployment.TopologySpreadConstraints = tscs
			},
			wantComponent: componentScheduler,
		},
		{
			name: "conductor",
			mutate: func(o *Nova, tscs []corev1.TopologySpreadConstraint) {
				o.Spec.Conductor.Deployment.TopologySpreadConstraints = tscs
			},
			wantComponent: componentConductor,
		},
		{
			name: "console proxy",
			mutate: func(o *Nova, tscs []corev1.TopologySpreadConstraint) {
				o.Spec.ConsoleProxy.Deployment = &DeploymentSpec{
					Replicas:                  1,
					TopologySpreadConstraints: tscs,
				}
			},
			wantComponent: componentConsoleProxy,
		},
	}

	// The names the controller builds its objects from. Spelling them out here
	// rather than reading the constants keeps the test from agreeing with a
	// renamed constant.
	wantValues := map[string]string{
		"metadata":      "metadata",
		"scheduler":     "scheduler",
		"conductor":     "conductor",
		"console proxy": "novncproxy",
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			w := &NovaWebhook{}
			obj := validNova()
			tc.mutate(obj, wrongSelector)

			g.Expect(tc.wantComponent).To(gomega.Equal(wantValues[tc.name]))

			_, err := w.ValidateCreate(context.Background(), obj)
			g.Expect(err).To(gomega.HaveOccurred())
			g.Expect(err.Error()).To(gomega.ContainSubstring(
				"app.kubernetes.io/component:" + wantValues[tc.name]))
		})
	}
}

// remoteComputeNova returns validNova() on a verified bus with the remote
// compute block set, the shape the remote-compute rules admit.
func remoteComputeNova() *Nova {
	obj := validNova()
	obj.Spec.Messaging.TLS = &commonv1.MessagingTLSSpec{
		CABundleSecretRef: commonv1.SecretRefSpec{Name: "rabbitmq-ca", Key: "ca.crt"},
	}
	obj.Spec.RemoteCompute = &NovaRemoteComputeSpec{
		KeystoneEndpoint: "https://keystone.example.com/v3",
		TransportURLSecretRef: commonv1.SecretRefSpec{
			Name: "nova-remote-transport", Key: commonv1.DefaultTransportURLSecretKey,
		},
	}
	return obj
}

// The remote transport URL is read under the key a brownfield bus Secret uses,
// and the block itself stays opt-in.
func TestNovaDefault_RemoteComputeTransportKey(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &NovaWebhook{}

	obj := remoteComputeNova()
	obj.Spec.RemoteCompute.TransportURLSecretRef.Key = ""
	g.Expect(w.Default(context.Background(), obj)).To(gomega.Succeed())
	g.Expect(obj.Spec.RemoteCompute.TransportURLSecretRef.Key).To(gomega.Equal("transport_url"))

	explicit := remoteComputeNova()
	explicit.Spec.RemoteCompute.TransportURLSecretRef.Key = "url"
	g.Expect(w.Default(context.Background(), explicit)).To(gomega.Succeed())
	g.Expect(explicit.Spec.RemoteCompute.TransportURLSecretRef.Key).To(gomega.Equal("url"))

	absent := validNova()
	g.Expect(w.Default(context.Background(), absent)).To(gomega.Succeed())
	g.Expect(absent.Spec.RemoteCompute).To(gomega.BeNil(), "the remote contract is never switched on by default")
}

func TestNovaValidateCreate_RemoteComputeOnAVerifiedBusAccepted(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &NovaWebhook{}

	_, err := w.ValidateCreate(context.Background(), remoteComputeNova())
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

// The remote-compute rules as a CR that bypassed the schema meets them: the
// twin of the CEL rule, the URL check that also keeps a newline out of the
// remote fragment, and the Secret name the remote transport URL is read from.
func TestNovaValidateCreate_RemoteComputeRejections(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mutate   func(o *Nova)
		wantPath string
		wantSub  string
	}{
		{
			name:     "remote compute on a plaintext bus",
			mutate:   func(o *Nova) { o.Spec.Messaging.TLS = nil },
			wantPath: "spec.messaging.tls: Required value",
			wantSub:  "is required when spec.remoteCompute is set",
		},
		{
			name:     "keystone endpoint carrying a newline",
			mutate:   func(o *Nova) { o.Spec.RemoteCompute.KeystoneEndpoint = "https://k\n[x]" },
			wantPath: "spec.remoteCompute.keystoneEndpoint",
			wantSub:  "must be a valid URL",
		},
		{
			name:     "keystone endpoint without a scheme",
			mutate:   func(o *Nova) { o.Spec.RemoteCompute.KeystoneEndpoint = "keystone.example.com" },
			wantPath: "spec.remoteCompute.keystoneEndpoint",
			wantSub:  "scheme must be http or https",
		},
		{
			name:     "keystone endpoint over plain http",
			mutate:   func(o *Nova) { o.Spec.RemoteCompute.KeystoneEndpoint = "http://keystone.example.com/v3" },
			wantPath: "spec.remoteCompute.keystoneEndpoint",
			wantSub:  "must use scheme https: every compute on another cluster sends the nova service-user password",
		},
		{
			name:     "transport URL Secret without a name",
			mutate:   func(o *Nova) { o.Spec.RemoteCompute.TransportURLSecretRef.Name = "" },
			wantPath: "spec.remoteCompute.transportURLSecretRef.name: Required value",
			wantSub:  "the transport URL of the broker's external listener",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			w := &NovaWebhook{}
			obj := remoteComputeNova()
			tc.mutate(obj)

			_, err := w.ValidateCreate(context.Background(), obj)
			g.Expect(err).To(gomega.HaveOccurred())
			g.Expect(err.Error()).To(gomega.ContainSubstring(tc.wantPath))
			g.Expect(err.Error()).To(gomega.ContainSubstring(tc.wantSub))

			_, err = w.ValidateUpdate(context.Background(), remoteComputeNova(), obj)
			g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(tc.wantPath)),
				"the rule is shared with update, so a later edit cannot introduce the violation either")
		})
	}
}

// --- Node placement and spec.jobs validation ---

func TestNovaValidate_NodePlacementRejected(t *testing.T) {
	for _, block := range []struct {
		name       string
		deployment func(o *Nova) *commonv1.DeploymentSpec
		path       string
	}{
		{name: "spec.api.deployment", deployment: func(o *Nova) *commonv1.DeploymentSpec { return &o.Spec.API.Deployment }, path: "spec.api.deployment"},
		{name: "spec.metadata.deployment", deployment: func(o *Nova) *commonv1.DeploymentSpec { return &o.Spec.Metadata.Deployment }, path: "spec.metadata.deployment"},
		{name: "spec.scheduler.deployment", deployment: func(o *Nova) *commonv1.DeploymentSpec { return &o.Spec.Scheduler.Deployment }, path: "spec.scheduler.deployment"},
		{name: "spec.conductor.deployment", deployment: func(o *Nova) *commonv1.DeploymentSpec { return &o.Spec.Conductor.Deployment }, path: "spec.conductor.deployment"},
		{name: "spec.consoleProxy.deployment", deployment: func(o *Nova) *commonv1.DeploymentSpec {
			return func() *commonv1.DeploymentSpec {
				o.Spec.ConsoleProxy.Deployment = &DeploymentSpec{Replicas: 1}
				return o.Spec.ConsoleProxy.Deployment
			}()
		}, path: "spec.consoleProxy.deployment"},
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
				o := validNova()
				tc.mutate(block.deployment(o))

				_, err := (&NovaWebhook{}).ValidateCreate(context.Background(), o)
				g.Expect(err).To(gomega.HaveOccurred())
				g.Expect(err.Error()).To(gomega.ContainSubstring(tc.want))
			})
		}
	}
}

func TestNovaValidate_JobsRejected(t *testing.T) {
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
			w := &NovaWebhook{Client: fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build()}
			o := validNova()
			o.Spec.Jobs = tc.jobs

			_, err := w.ValidateCreate(context.Background(), o)
			g.Expect(err).To(gomega.HaveOccurred())
			g.Expect(err.Error()).To(gomega.ContainSubstring(tc.want))
		})
	}
}

func TestNovaValidate_EmptyJobsAccepted(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &NovaWebhook{Client: fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build()}
	o := validNova()
	o.Spec.Jobs = &commonv1.JobSpec{}

	_, err := w.ValidateCreate(context.Background(), o)
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

// TestNovaValidate_AutoscalingTargetNeedsAPositiveRequest pins the HPA
// request check: the HorizontalPodAutoscaler divides the pods' usage by the
// sum of their containers' requests, so a zero CPU request under a CPU target
// is rejected at its field path, and the same CR without it is admitted.
func TestNovaValidate_AutoscalingTargetNeedsAPositiveRequest(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &NovaWebhook{}
	withTarget := func() *Nova {
		o := validNova()
		o.Spec.Autoscaling = &AutoscalingSpec{
			MinReplicas:          ptr.To(int32(1)),
			MaxReplicas:          5,
			TargetCPUUtilization: ptr.To(int32(80)),
		}
		return o
	}

	o := withTarget()
	o.Spec.API.Deployment.Resources = &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("0")},
	}
	_, err := w.ValidateCreate(context.Background(), o)
	g.Expect(apierrors.IsInvalid(err)).To(gomega.BeTrue(), "%v", err)
	g.Expect(err.Error()).To(gomega.ContainSubstring("spec.api.deployment.resources.requests.cpu"))
	g.Expect(err.Error()).To(gomega.ContainSubstring("cpu request must be greater than zero"))

	_, err = w.ValidateCreate(context.Background(), withTarget())
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

// TestPodSelectors pins each exported selector to the labels the pods of its
// Deployment carry, the maps the spread checks compare against.
func TestPodSelectors(t *testing.T) {
	g := gomega.NewWithT(t)
	base := func(component string) map[string]string {
		return map[string]string{
			"app.kubernetes.io/name":      "nova",
			"app.kubernetes.io/instance":  "x",
			"app.kubernetes.io/component": component,
		}
	}
	g.Expect(APIPodSelector("x")).To(gomega.Equal(base("api")))
	g.Expect(MetadataPodSelector("x")).To(gomega.Equal(base("metadata")))
	g.Expect(SchedulerPodSelector("x")).To(gomega.Equal(base("scheduler")))
	g.Expect(ConductorPodSelector("x")).To(gomega.Equal(base("conductor")))
	g.Expect(ConsoleProxyPodSelector("x")).To(gomega.Equal(base("novncproxy")))
	// Each call returns a fresh map, so a caller cannot alias another's.
	a := ConductorPodSelector("x")
	a["extra"] = "x"
	g.Expect(ConductorPodSelector("x")).NotTo(gomega.HaveKey("extra"))
}
