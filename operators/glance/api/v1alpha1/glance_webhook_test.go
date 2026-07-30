// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/onsi/gomega"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	commonv1 "github.com/c5c3/forge/internal/common/types"
)

// validGlance returns a minimal Glance CR that passes every validation rule.
// Tests mutate single fields to exercise individual rules.
func validGlance() *Glance {
	return &Glance{
		ObjectMeta: metav1.ObjectMeta{Name: "test-glance", Namespace: "openstack"},
		Spec: GlanceSpec{
			OpenStackRelease: "2025.2",
			Deployment:       DeploymentSpec{Replicas: 3},
			Image: commonv1.ImageSpec{
				Repository: "ghcr.io/c5c3/glance",
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
				SecretRef: commonv1.SecretRefSpec{Name: "glance-service-password", Key: "password"},
			},
		},
	}
}

func TestGlanceDefault_MaterializesServiceUserAndLoggingDefaults(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &GlanceWebhook{}

	// Start from a CR whose service-user identity fields and secretRef key are
	// empty so the defaulter has to fill all five.
	obj := validGlance()
	obj.Spec.ServiceUser = ServiceUserSpec{SecretRef: commonv1.SecretRefSpec{Name: "glance-service-password"}}

	g.Expect(w.Default(context.Background(), obj)).To(gomega.Succeed())

	g.Expect(obj.Spec.ServiceUser.Username).To(gomega.Equal("glance"))
	g.Expect(obj.Spec.ServiceUser.ProjectName).To(gomega.Equal("service"))
	g.Expect(obj.Spec.ServiceUser.UserDomainName).To(gomega.Equal("Default"))
	g.Expect(obj.Spec.ServiceUser.ProjectDomainName).To(gomega.Equal("Default"))
	g.Expect(obj.Spec.ServiceUser.SecretRef.Key).To(gomega.Equal("password"))

	// Shared-block defaults come along too — with the glance-specific memory
	// values (512Mi request / 1Gi limit for the boto3-weighted S3 path)
	// replacing the shared 256Mi/512Mi baseline, while CPU keeps the shared
	// defaults.
	g.Expect(obj.Spec.Deployment.Resources).NotTo(gomega.BeNil())
	g.Expect(obj.Spec.Deployment.Resources.Requests.Memory().String()).To(gomega.Equal("512Mi"))
	g.Expect(obj.Spec.Deployment.Resources.Limits.Memory().String()).To(gomega.Equal("1Gi"))
	g.Expect(obj.Spec.Deployment.Resources.Requests.Cpu().String()).To(gomega.Equal("100m"))
	g.Expect(obj.Spec.Deployment.Resources.Limits.Cpu().String()).To(gomega.Equal("500m"))
	g.Expect(obj.Spec.Cache.Backend).To(gomega.Equal(commonv1.DefaultCacheBackend))
	g.Expect(obj.Spec.Logging).NotTo(gomega.BeNil())
	g.Expect(obj.Spec.Logging.Format).To(gomega.Equal("text"))
	g.Expect(obj.Spec.Logging.Level).To(gomega.Equal("INFO"))
}

func TestGlanceDefault_PreservesExplicitResources(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &GlanceWebhook{}

	obj := validGlance()
	obj.Spec.Deployment.Resources = &corev1.ResourceRequirements{
		Limits: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}

	g.Expect(w.Default(context.Background(), obj)).To(gomega.Succeed())

	// A non-empty resources block is left verbatim: neither the glance memory
	// defaults nor the shared baseline overwrite an explicit value.
	g.Expect(obj.Spec.Deployment.Resources.Limits.Memory().String()).To(gomega.Equal("256Mi"))
	g.Expect(obj.Spec.Deployment.Resources.Requests).To(gomega.BeEmpty())
}

func TestGlanceDefault_PreservesExplicitServiceUserValues(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &GlanceWebhook{}

	obj := validGlance()
	obj.Spec.ServiceUser = ServiceUserSpec{
		Username:          "img-svc",
		ProjectName:       "images",
		UserDomainName:    "Corp",
		ProjectDomainName: "Corp",
		SecretRef:         commonv1.SecretRefSpec{Name: "custom-secret", Key: "custom-key"},
	}

	g.Expect(w.Default(context.Background(), obj)).To(gomega.Succeed())

	g.Expect(obj.Spec.ServiceUser.Username).To(gomega.Equal("img-svc"))
	g.Expect(obj.Spec.ServiceUser.ProjectName).To(gomega.Equal("images"))
	g.Expect(obj.Spec.ServiceUser.UserDomainName).To(gomega.Equal("Corp"))
	g.Expect(obj.Spec.ServiceUser.ProjectDomainName).To(gomega.Equal("Corp"))
	g.Expect(obj.Spec.ServiceUser.SecretRef.Key).To(gomega.Equal("custom-key"))
}

func TestGlanceDefault_UWSGISemantics(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &GlanceWebhook{}

	// Nil apiServer / nil uwsgi: nothing is materialized.
	nilBlock := validGlance()
	g.Expect(w.Default(context.Background(), nilBlock)).To(gomega.Succeed())
	g.Expect(nilBlock.Spec.APIServer).To(gomega.BeNil())

	// Present-but-zero uwsgi: processes/threads/httpKeepAlive filled.
	zero := validGlance()
	zero.Spec.OpenStackRelease = "2026.1"
	zero.Spec.APIServer = &APIServerSpec{UWSGI: &UWSGISpec{}}
	g.Expect(w.Default(context.Background(), zero)).To(gomega.Succeed())
	g.Expect(zero.Spec.APIServer.UWSGI.Processes).To(gomega.Equal(DefaultUWSGIProcesses))
	g.Expect(zero.Spec.APIServer.UWSGI.Threads).To(gomega.Equal(DefaultUWSGIThreads))
	g.Expect(zero.Spec.APIServer.UWSGI.HTTPKeepAlive).To(gomega.HaveValue(gomega.BeTrue()))

	// Explicit httpKeepAlive=false is preserved (nil-preserving pointer).
	explicit := validGlance()
	explicit.Spec.OpenStackRelease = "2026.1"
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

func TestGlanceValidateCreate_ValidSpecAccepted(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &GlanceWebhook{}

	_, err := w.ValidateCreate(context.Background(), validGlance())
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

func TestGlanceValidateCreate_RejectionTable(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(o *Glance)
		wantSub string
	}{
		{
			name:    "zero replicas rejected",
			mutate:  func(o *Glance) { o.Spec.Deployment.Replicas = 0 },
			wantSub: "replicas must be at least 1",
		},
		{
			name: "image tag and digest both set rejected",
			mutate: func(o *Glance) {
				o.Spec.Image.Digest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			},
			wantSub: "exactly one of image.tag or image.digest",
		},
		{
			name: "database clusterRef and host both set rejected",
			mutate: func(o *Glance) {
				o.Spec.Database.Host = "mariadb.example.com"
			},
			wantSub: "exactly one of clusterRef or host",
		},
		{
			name: "dynamic credentials without clusterRef rejected",
			mutate: func(o *Glance) {
				o.Spec.Database.ClusterRef = nil
				o.Spec.Database.Host = "mariadb.example.com"
				o.Spec.Database.CredentialsMode = commonv1.CredentialsModeDynamic
			},
			wantSub: "credentialsMode Dynamic requires clusterRef",
		},
		{
			name: "cache clusterRef and servers both set rejected",
			mutate: func(o *Glance) {
				o.Spec.Cache.Servers = []string{"memcached-0:11211"}
			},
			wantSub: "exactly one of clusterRef or servers",
		},
		// Both cache shapes land in [keystone_authtoken].memcached_servers via
		// cache.ResolveServers, which the INI renderer writes verbatim. A newline
		// appends a second auth_url to that section and oslo.config keeps the last
		// value for a non-multi option, so keystonemiddleware would validate every
		// incoming token against an attacker-controlled Keystone.
		{
			name: "cache server with a newline rejected",
			mutate: func(o *Glance) {
				o.Spec.Cache.ClusterRef = nil
				o.Spec.Cache.Servers = []string{"memcached-0:11211\nauth_url = http://attacker.example/v3"}
			},
			wantSub: "must not contain a newline or carriage return",
		},
		{
			name: "cache clusterRef name with a carriage return rejected",
			mutate: func(o *Glance) {
				o.Spec.Cache.ClusterRef = &corev1.LocalObjectReference{
					Name: "memcached\rauth_url = http://attacker.example/v3",
				}
			},
			wantSub: "must not contain a newline or carriage return",
		},
		{
			name:    "empty keystoneEndpoint rejected",
			mutate:  func(o *Glance) { o.Spec.KeystoneEndpoint = "" },
			wantSub: "keystoneEndpoint must be set",
		},
		{
			name:    "non-url keystoneEndpoint rejected",
			mutate:  func(o *Glance) { o.Spec.KeystoneEndpoint = "not-a-url" },
			wantSub: "scheme must be http or https",
		},
		{
			name:    "ftp keystoneEndpoint rejected",
			mutate:  func(o *Glance) { o.Spec.KeystoneEndpoint = "ftp://x" },
			wantSub: "scheme must be http or https",
		},
		{
			name:    "keystoneEndpoint without host rejected",
			mutate:  func(o *Glance) { o.Spec.KeystoneEndpoint = "http://" },
			wantSub: "must include a host",
		},
		{
			name:    "bad keystonePublicEndpoint rejected",
			mutate:  func(o *Glance) { o.Spec.KeystonePublicEndpoint = "ftp://keystone" },
			wantSub: "scheme must be http or https",
		},
		{
			name: "empty extraConfig section rejected",
			mutate: func(o *Glance) {
				o.Spec.ExtraConfig = map[string]map[string]string{"": {"foo": "bar"}}
			},
			wantSub: "extraConfig section name must not be empty",
		},
		{
			name: "empty extraConfig key rejected",
			mutate: func(o *Glance) {
				o.Spec.ExtraConfig = map[string]map[string]string{"image_import_opts": {"": "bar"}}
			},
			wantSub: "extraConfig key must not be empty",
		},
		{
			name: "extraConfig section with newline rejected",
			mutate: func(o *Glance) {
				o.Spec.ExtraConfig = map[string]map[string]string{
					"glance_store]\n[profiler": {"enabled": "true"},
				}
			},
			wantSub: "extraConfig section name must not contain a newline or carriage return",
		},
		{
			name: "extraConfig key with newline rejected",
			mutate: func(o *Glance) {
				o.Spec.ExtraConfig = map[string]map[string]string{
					"glance_store": {"default_backend = s3\n[profiler]\nenabled": "true"},
				}
			},
			wantSub: "extraConfig key and value must not contain a newline or carriage return",
		},
		{
			// The rendered INI writes "%s = %s" verbatim, so a newline in a value
			// smuggles a whole [section] past the ownership and catalog gates —
			// they key on (section, key) names and never look inside a value.
			name: "extraConfig value with newline rejected",
			mutate: func(o *Glance) {
				o.Spec.ExtraConfig = map[string]map[string]string{
					"glance_store": {"default_backend": "s3\n[profiler]\nenabled = true"},
				}
			},
			wantSub: "extraConfig key and value must not contain a newline or carriage return",
		},
		{
			// The operator owns [keystone_authtoken] password via
			// spec.serviceUser.secretRef and env-injects it at runtime, so a file
			// override is inert but would leak the service password into the
			// namespace-readable ConfigMap. It is Rejected, blocked at admission
			// rather than merely reported.
			name: "extraConfig owned password rejected",
			mutate: func(o *Glance) {
				o.Spec.ExtraConfig = map[string]map[string]string{
					"keystone_authtoken": {"password": "s3cr3t"},
				}
			},
			wantSub: "password is managed via spec.serviceUser.secretRef",
		},
		{
			name: "gateway without hostname rejected",
			mutate: func(o *Glance) {
				o.Spec.Gateway = &GatewaySpec{ParentRef: GatewayParentRefSpec{Name: "openstack-gw"}}
			},
			wantSub: "hostname must be set",
		},
		{
			name: "networkPolicy with empty ingress rejected",
			mutate: func(o *Glance) {
				o.Spec.NetworkPolicy = &NetworkPolicySpec{}
			},
			wantSub: "at least one ingress source",
		},
		{
			name: "autoscaling without any target rejected",
			mutate: func(o *Glance) {
				o.Spec.Autoscaling = &AutoscalingSpec{MaxReplicas: 5}
			},
			wantSub: "at least one of targetCPUUtilization or targetMemoryUtilization",
		},
		{
			name: "invalid logging format rejected",
			mutate: func(o *Glance) {
				o.Spec.Logging = &LoggingSpec{Format: "xml"}
			},
			wantSub: "logging.format",
		},
		{
			name: "preStopSleep at grace period rejected",
			mutate: func(o *Glance) {
				o.Spec.Deployment.TerminationGracePeriodSeconds = ptr.To(int64(30))
				o.Spec.Deployment.PreStopSleepSeconds = ptr.To(int64(30))
			},
			wantSub: "strictly less than terminationGracePeriodSeconds",
		},
		{
			// Glance drops the deny-list whenever the matching allow-list is
			// non-empty, so the two halves may never be configured together.
			name: "importFiltering allow and deny hosts rejected",
			mutate: func(o *Glance) {
				o.Spec.ImportFiltering = &ImportFilteringSpec{
					AllowedHosts:    []string{"mirror.example.com"},
					DisallowedHosts: []string{"169.254.169.254"},
				}
			},
			wantSub: "allowedHosts and disallowedHosts are mutually exclusive",
		},
		{
			name: "importFiltering allow and deny schemes rejected",
			mutate: func(o *Glance) {
				o.Spec.ImportFiltering = &ImportFilteringSpec{
					AllowedSchemes:    []string{"https"},
					DisallowedSchemes: []string{"http"},
				}
			},
			wantSub: "allowedSchemes and disallowedSchemes are mutually exclusive",
		},
		{
			name: "importFiltering allow and deny ports rejected",
			mutate: func(o *Glance) {
				o.Spec.ImportFiltering = &ImportFilteringSpec{
					AllowedPorts:    []int32{443},
					DisallowedPorts: []int32{80},
				}
			},
			wantSub: "allowedPorts and disallowedPorts are mutually exclusive",
		},
		{
			name: "importFiltering off-enum scheme rejected",
			mutate: func(o *Glance) {
				o.Spec.ImportFiltering = &ImportFilteringSpec{AllowedSchemes: []string{"ftp"}}
			},
			wantSub: `Unsupported value: "ftp"`,
		},
		{
			name: "importFiltering port zero rejected",
			mutate: func(o *Glance) {
				o.Spec.ImportFiltering = &ImportFilteringSpec{AllowedPorts: []int32{0}}
			},
			wantSub: "port must be between 1 and 65535",
		},
		{
			name: "importFiltering port above range rejected",
			mutate: func(o *Glance) {
				o.Spec.ImportFiltering = &ImportFilteringSpec{AllowedPorts: []int32{70000}}
			},
			wantSub: "port must be between 1 and 65535",
		},
		{
			name: "importFiltering empty host rejected",
			mutate: func(o *Glance) {
				o.Spec.ImportFiltering = &ImportFilteringSpec{DisallowedHosts: []string{""}}
			},
			wantSub: "host must not be empty",
		},
		{
			// The host lists are the only free-form strings in the block and are
			// comma-joined verbatim into glance-api.conf, so a newline would
			// render a whole [section] the extraConfig ownership and catalog
			// gates never inspect — they look at map structure, not values.
			name: "importFiltering denied host with newline rejected",
			mutate: func(o *Glance) {
				o.Spec.ImportFiltering = &ImportFilteringSpec{
					DisallowedHosts: []string{"evil.example.com\n[profiler]\nenabled = true"},
				}
			},
			wantSub: "host must not contain newline or carriage-return characters",
		},
		{
			name: "importFiltering allowed host with carriage return rejected",
			mutate: func(o *Glance) {
				o.Spec.ImportFiltering = &ImportFilteringSpec{
					AllowedHosts: []string{"mirror.example.com\r[profiler]\r"},
				}
			},
			wantSub: "host must not contain newline or carriage-return characters",
		},
		{
			name: "importFiltering overlong host rejected",
			mutate: func(o *Glance) {
				o.Spec.ImportFiltering = &ImportFilteringSpec{
					DisallowedHosts: []string{strings.Repeat("a", 254)},
				}
			},
			wantSub: "host must be at most 253 characters",
		},
		{
			name: "importFiltering list above item cap rejected",
			mutate: func(o *Glance) {
				hosts := make([]string, 65)
				for i := range hosts {
					hosts[i] = fmt.Sprintf("mirror-%d.example.com", i)
				}
				o.Spec.ImportFiltering = &ImportFilteringSpec{AllowedHosts: hosts}
			},
			wantSub: "must have at most 64 items",
		},
		{
			// The schedule carries no CRD pattern, so the webhook's
			// cron.ParseStandard call is the only gate between a typo and a
			// CronJob the API server refuses to create.
			name: "dbPurge unparseable schedule rejected",
			mutate: func(o *Glance) {
				o.Spec.DBPurge = &DBPurgeSpec{Schedule: "totally-not-cron"}
			},
			wantSub: "invalid cron expression",
		},
		{
			name: "dbPurge zero retentionDays rejected",
			mutate: func(o *Glance) {
				o.Spec.DBPurge = &DBPurgeSpec{RetentionDays: ptr.To(int32(0))}
			},
			wantSub: "retentionDays must be at least 1",
		},
		{
			// A resource.Quantity is x-kubernetes-int-or-string in the schema and
			// carries no Minimum marker, so the webhook is the only thing keeping a
			// CR from reading as bounded on a value that bounds nothing.
			name: "staging zero sizeLimit rejected",
			mutate: func(o *Glance) {
				o.Spec.Staging = &StagingSpec{SizeLimit: ptr.To(resource.MustParse("0"))}
			},
			wantSub: "must be at least 1Mi",
		},
		{
			name: "staging negative sizeLimit rejected",
			mutate: func(o *Glance) {
				o.Spec.Staging = &StagingSpec{SizeLimit: ptr.To(resource.MustParse("-1Gi"))}
			},
			wantSub: "must be at least 1Mi",
		},
		{
			// `100m` is the milli suffix — a tenth of a byte — and the single most
			// common quantity typo for `100Mi`. It is positive and it matches the
			// generated schema's int-or-string pattern, so only the floor catches
			// it; admitted, it evicts the pod on the first staged byte and the
			// replacement on the next import, forever.
			name: "staging milli-suffix sizeLimit typo rejected",
			mutate: func(o *Glance) {
				o.Spec.Staging = &StagingSpec{SizeLimit: ptr.To(resource.MustParse("100m"))}
			},
			wantSub: "must be at least 1Mi",
		},
		{
			// The two knobs contradict each other, and silently letting one win
			// would leave a CR whose rendered Deployment does not match either
			// reading of its spec.
			name: "staging unbounded together with sizeLimit rejected",
			mutate: func(o *Glance) {
				o.Spec.Staging = &StagingSpec{
					Unbounded: true,
					SizeLimit: ptr.To(resource.MustParse("40Gi")),
				}
			},
			wantSub: "mutually exclusive",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			w := &GlanceWebhook{}
			obj := validGlance()
			tc.mutate(obj)

			_, err := w.ValidateCreate(context.Background(), obj)
			g.Expect(err).To(gomega.HaveOccurred())
			g.Expect(err.Error()).To(gomega.ContainSubstring(tc.wantSub))
		})
	}
}

// TestGlanceValidate_ImportFilteringValidShapesAccepted pins the shapes the
// mutual-exclusivity rules must NOT reject: an unset block, a present-but-empty
// block, a loosening that stays on one side of every attribute pair, and
// explicitly empty lists — the last being how a deployment opts out of a
// render-time default without tripping the allow/deny pairing.
func TestGlanceValidate_ImportFilteringValidShapesAccepted(t *testing.T) {
	tests := []struct {
		name      string
		filtering *ImportFilteringSpec
	}{
		{
			name:      "nil block accepted",
			filtering: nil,
		},
		{
			name:      "empty block accepted",
			filtering: &ImportFilteringSpec{},
		},
		{
			// Loosening back to glance's own defaults for an http image mirror:
			// both attributes stay on the allow side, so no pair conflicts.
			name: "http mirror loosening accepted",
			filtering: &ImportFilteringSpec{
				AllowedSchemes: []string{"http", "https"},
				AllowedPorts:   []int32{80, 443},
			},
		},
		{
			name:      "deny-only schemes accepted",
			filtering: &ImportFilteringSpec{DisallowedSchemes: []string{"http"}},
		},
		{
			name:      "allow-only hosts accepted",
			filtering: &ImportFilteringSpec{AllowedHosts: []string{"mirror.example.com"}},
		},
		{
			// An explicit empty list opts out of the render-time default; it is
			// not the non-empty allow-list the exclusivity rule guards against.
			name: "explicit empty lists accepted",
			filtering: &ImportFilteringSpec{
				AllowedSchemes:  []string{},
				DisallowedHosts: []string{"169.254.169.254"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			w := &GlanceWebhook{}
			obj := validGlance()
			obj.Spec.ImportFiltering = tc.filtering

			_, err := w.ValidateCreate(context.Background(), obj)
			g.Expect(err).NotTo(gomega.HaveOccurred())
		})
	}
}

// TestGlanceValidate_DBPurgeValidShapesAccepted pins the shapes the webhook must
// NOT reject. Both knobs are optional and resolved at reconcile time, so every
// partially-filled block is a legitimate way of saying "keep the operator
// default for the other half" — including the descriptor form of a schedule,
// which cron.ParseStandard accepts even though it is not five cron fields.
func TestGlanceValidate_DBPurgeValidShapesAccepted(t *testing.T) {
	tests := []struct {
		name    string
		dbPurge *DBPurgeSpec
	}{
		{
			name:    "nil block accepted",
			dbPurge: nil,
		},
		{
			name:    "empty block accepted",
			dbPurge: &DBPurgeSpec{},
		},
		{
			name:    "schedule only accepted",
			dbPurge: &DBPurgeSpec{Schedule: "30 2 * * 0"},
		},
		{
			// The Minimum=1 bound is inclusive.
			name:    "retentionDays only accepted",
			dbPurge: &DBPurgeSpec{RetentionDays: ptr.To(int32(1))},
		},
		{
			name:    "descriptor schedule accepted",
			dbPurge: &DBPurgeSpec{Schedule: "@daily"},
		},
		{
			name:    "images-table opt-in accepted",
			dbPurge: &DBPurgeSpec{PurgeImagesTable: ptr.To(true)},
		},
		{
			name:    "suspended purge accepted",
			dbPurge: &DBPurgeSpec{Suspend: true},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			w := &GlanceWebhook{}
			obj := validGlance()
			obj.Spec.DBPurge = tc.dbPurge

			_, err := w.ValidateCreate(context.Background(), obj)
			g.Expect(err).NotTo(gomega.HaveOccurred())
		})
	}
}

// TestValidateStaging pins the shape of the exported validator directly, the way
// the ControlPlane webhook would call it: nil-safe on both levels — an unset
// block and an unset sizeLimit inside a set block are how a CR asks for the
// operator default, not violations — reporting the 1Mi floor on the sizeLimit
// leaf and the mutual exclusion on the unbounded leaf, so each message names the
// field the caller has to fix. `unbounded` alone is the deliberate opt-out back
// to the pre-bound behaviour and must be admitted.
func TestValidateStaging(t *testing.T) {
	tests := []struct {
		name      string
		staging   *StagingSpec
		wantField string
		wantMsg   string
	}{
		{
			name:    "nil block accepted",
			staging: nil,
		},
		{
			name:    "nil sizeLimit accepted",
			staging: &StagingSpec{},
		},
		{
			name:    "positive sizeLimit accepted",
			staging: &StagingSpec{SizeLimit: ptr.To(resource.MustParse("1Gi"))},
		},
		{
			name:      "zero sizeLimit rejected",
			staging:   &StagingSpec{SizeLimit: ptr.To(resource.MustParse("0"))},
			wantField: "staging.sizeLimit",
			wantMsg:   "must be at least 1Mi",
		},
		{
			name:      "negative sizeLimit rejected",
			staging:   &StagingSpec{SizeLimit: ptr.To(resource.MustParse("-500Mi"))},
			wantField: "staging.sizeLimit",
			wantMsg:   "must be at least 1Mi",
		},
		{
			// Sub-byte but positive: `100m` is a tenth of a byte, the milli-suffix
			// typo for `100Mi`. Only the floor separates it from a usable bound.
			name:      "milli-suffix sizeLimit rejected",
			staging:   &StagingSpec{SizeLimit: ptr.To(resource.MustParse("100m"))},
			wantField: "staging.sizeLimit",
			wantMsg:   "must be at least 1Mi",
		},
		{
			name:    "sizeLimit exactly at the floor accepted",
			staging: &StagingSpec{SizeLimit: ptr.To(resource.MustParse("1Mi"))},
		},
		{
			name:      "sizeLimit just under the floor rejected",
			staging:   &StagingSpec{SizeLimit: ptr.To(resource.MustParse("1048575"))},
			wantField: "staging.sizeLimit",
			wantMsg:   "must be at least 1Mi",
		},
		{
			name:    "unbounded alone accepted",
			staging: &StagingSpec{Unbounded: true},
		},
		{
			name: "unbounded with sizeLimit rejected",
			staging: &StagingSpec{
				Unbounded: true,
				SizeLimit: ptr.To(resource.MustParse("40Gi")),
			},
			wantField: "staging.unbounded",
			wantMsg:   "unbounded and sizeLimit are mutually exclusive: an unbounded staging area has no size limit to configure",
		},
		{
			// The contradiction is reported ahead of the floor, so a CR that also
			// carries an unusable sizeLimit is told about the contradiction first
			// rather than about a value it should not be setting at all.
			name: "unbounded with a sub-floor sizeLimit reports the contradiction",
			staging: &StagingSpec{
				Unbounded: true,
				SizeLimit: ptr.To(resource.MustParse("100m")),
			},
			wantField: "staging.unbounded",
			wantMsg:   "unbounded and sizeLimit are mutually exclusive: an unbounded staging area has no size limit to configure",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			errs := ValidateStaging(field.NewPath("spec").Child("staging"), tc.staging)

			if tc.wantField == "" {
				g.Expect(errs).To(gomega.BeEmpty())
				return
			}
			g.Expect(errs).To(gomega.HaveLen(1))
			g.Expect(errs[0].Type).To(gomega.Equal(field.ErrorTypeInvalid))
			g.Expect(errs[0].Field).To(gomega.HaveSuffix(tc.wantField))
			g.Expect(errs[0].Detail).To(gomega.Equal(tc.wantMsg))
		})
	}
}

// The operator default is resolved at reconcile time, never stamped into the
// stored CR: an unset block must survive admission unset so it keeps tracking
// the default across upgrades. Nothing else in the suite exercises the defaulter
// against spec.staging, so a defaulting rule added later would freeze today's
// value into every stored CR without a single test turning red.
func TestGlanceDefault_LeavesStagingUnset(t *testing.T) {
	g := gomega.NewWithT(t)
	obj := validGlance()

	g.Expect((&GlanceWebhook{}).Default(context.Background(), obj)).To(gomega.Succeed())

	g.Expect(obj.Spec.Staging).To(gomega.BeNil(),
		"spec.staging must not be materialized by the defaulting webhook")
}

// TestValidateImageCache pins the shape of the exported validator directly, the
// way the ControlPlane webhook would call it, and then runs every case through
// both admission verbs: neither floor has a schema counterpart, so an update
// path that skipped them would let a stored CR settle on a value create rejects.
// Nil-safe on both levels — an unset block is how a CR leaves the cache off, an
// unset field inside a set block is how it asks for the operator default.
func TestValidateImageCache(t *testing.T) {
	tests := []struct {
		name       string
		imageCache *ImageCacheSpec
		wantField  string
		wantMsg    string
	}{
		{
			name:       "nil block accepted",
			imageCache: nil,
		},
		{
			// The block alone is the opt-in; both knobs resolve at render time.
			name:       "empty block accepted",
			imageCache: &ImageCacheSpec{},
		},
		{
			// A resource.Quantity is x-kubernetes-int-or-string in the schema and
			// carries no Minimum marker, so the webhook is the only gate between a
			// CR that reads as bounded and a bound that bounds nothing.
			name:       "zero sizeLimit rejected",
			imageCache: &ImageCacheSpec{SizeLimit: ptr.To(resource.MustParse("0"))},
			wantField:  "imageCache.sizeLimit",
			wantMsg:    "must be at least 1Mi",
		},
		{
			name:       "negative sizeLimit rejected",
			imageCache: &ImageCacheSpec{SizeLimit: ptr.To(resource.MustParse("-1Gi"))},
			wantField:  "imageCache.sizeLimit",
			wantMsg:    "must be at least 1Mi",
		},
		{
			name:       "sizeLimit just under the floor rejected",
			imageCache: &ImageCacheSpec{SizeLimit: ptr.To(resource.MustParse("512Ki"))},
			wantField:  "imageCache.sizeLimit",
			wantMsg:    "must be at least 1Mi",
		},
		{
			name:       "sizeLimit exactly at the floor accepted",
			imageCache: &ImageCacheSpec{SizeLimit: ptr.To(resource.MustParse("1Mi"))},
		},
		{
			// A metav1.Duration renders as a plain string, so this floor too is
			// webhook-only.
			name: "sub-minute maintenanceInterval rejected",
			imageCache: &ImageCacheSpec{
				MaintenanceInterval: &metav1.Duration{Duration: 59 * time.Second},
			},
			wantField: "imageCache.maintenanceInterval",
			wantMsg:   "must be at least 1m0s",
		},
		{
			name: "maintenanceInterval exactly at the floor accepted",
			imageCache: &ImageCacheSpec{
				MaintenanceInterval: &metav1.Duration{Duration: time.Minute},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			errs := ValidateImageCache(field.NewPath("spec").Child("imageCache"), tc.imageCache)
			if tc.wantField == "" {
				g.Expect(errs).To(gomega.BeEmpty())
			} else {
				g.Expect(errs).To(gomega.HaveLen(1))
				g.Expect(errs[0].Type).To(gomega.Equal(field.ErrorTypeInvalid))
				g.Expect(errs[0].Field).To(gomega.HaveSuffix(tc.wantField))
				g.Expect(errs[0].Detail).To(gomega.Equal(tc.wantMsg))
			}

			w := &GlanceWebhook{}
			obj := validGlance()
			obj.Spec.ImageCache = tc.imageCache

			_, createErr := w.ValidateCreate(context.Background(), obj)
			_, updateErr := w.ValidateUpdate(context.Background(), validGlance(), obj)

			if tc.wantField == "" {
				g.Expect(createErr).NotTo(gomega.HaveOccurred())
				g.Expect(updateErr).NotTo(gomega.HaveOccurred())
				return
			}
			for verb, err := range map[string]error{"create": createErr, "update": updateErr} {
				g.Expect(err).To(gomega.HaveOccurred(), "%s must reject %s", verb, tc.name)
				g.Expect(err.Error()).To(gomega.ContainSubstring("spec." + tc.wantField))
				g.Expect(err.Error()).To(gomega.ContainSubstring(tc.wantMsg))
			}
		})
	}
}

// The operator injects the `cache` paste filter while spec.imageCache is set, so
// a user middleware of that name is a collision rather than an addition: without
// this rule the rendered pipeline silently carries whichever of the two
// definitions wins. The name is reserved only for as long as the cache is
// enabled — a CR that carried such a middleware before this field existed stays
// admissible.
func TestGlanceValidate_ImageCacheMiddlewareCollision(t *testing.T) {
	userCacheFilter := commonv1.MiddlewareSpec{
		Name:          "cache",
		FilterFactory: "my_cache_middleware:filter_factory",
		Position:      commonv1.PipelinePositionBefore,
	}

	t.Run("rejected while the image cache is enabled", func(t *testing.T) {
		g := gomega.NewWithT(t)
		w := &GlanceWebhook{}

		obj := validGlance()
		obj.Spec.ImageCache = &ImageCacheSpec{}
		obj.Spec.Middleware = []commonv1.MiddlewareSpec{
			{
				Name:          "audit",
				FilterFactory: "audit_middleware:filter_factory",
				Position:      commonv1.PipelinePositionAfter,
			},
			userCacheFilter,
		}

		_, createErr := w.ValidateCreate(context.Background(), obj)
		_, updateErr := w.ValidateUpdate(context.Background(), validGlance(), obj)

		for verb, err := range map[string]error{"create": createErr, "update": updateErr} {
			g.Expect(err).To(gomega.HaveOccurred(), "%s must reject the colliding middleware", verb)
			// The index matters: the message has to name the offending entry, not
			// the list, or a CR with several filters gives no clue which to rename.
			g.Expect(err.Error()).To(gomega.ContainSubstring("spec.middleware[1].name"))
			g.Expect(err.Error()).To(gomega.ContainSubstring(`owns that filter name while spec.imageCache is set`))
		}
	})

	t.Run("accepted while the image cache is disabled", func(t *testing.T) {
		g := gomega.NewWithT(t)
		w := &GlanceWebhook{}

		obj := validGlance()
		obj.Spec.Middleware = []commonv1.MiddlewareSpec{userCacheFilter}

		_, err := w.ValidateCreate(context.Background(), obj)
		g.Expect(err).NotTo(gomega.HaveOccurred(),
			"the cache filter name is reserved only while spec.imageCache is set")
	})
}

// Both image-cache defaults are resolved at render time, never stamped into the
// stored CR — and the block's mere presence is what enables the cache, so a
// defaulter materializing it would switch the feature on for every Glance in the
// cluster. Nothing else in the suite exercises the defaulter against
// spec.imageCache, so such a rule would land without a single test turning red.
func TestGlanceDefault_LeavesImageCacheUnset(t *testing.T) {
	g := gomega.NewWithT(t)
	obj := validGlance()

	g.Expect((&GlanceWebhook{}).Default(context.Background(), obj)).To(gomega.Succeed())

	g.Expect(obj.Spec.ImageCache).To(gomega.BeNil(),
		"spec.imageCache must not be materialized by the defaulting webhook")
}

// TestValidateImportPlugins pins the shape of the exported validator directly,
// the way the ControlPlane webhook would call it, and runs every case through
// both admission verbs. The enum and the count bounds are defense in depth, but
// every rule on a property name is the only gate there is: a map key is out of
// reach of a CRD marker, so an update path that skipped the webhook would let a
// pair break out of the oslo Dict syntax the rendered inject value uses.
func TestValidateImportPlugins(t *testing.T) {
	// A property map the count bound rejects, built one entry past the cap.
	tooManyProperties := make(map[string]string, maxImportInjectEntries+1)
	tooManyRoles := make([]string, maxImportInjectEntries+1)
	for i := 0; i <= maxImportInjectEntries; i++ {
		tooManyProperties[fmt.Sprintf("prop_%02d", i)] = "value"
		tooManyRoles[i] = fmt.Sprintf("role-%02d", i)
	}
	overlong := strings.Repeat("a", maxImportInjectStringLength+1)
	// Exactly at the bound in code points, three times over it in bytes: what the
	// schema counterparts admit and a byte-counting mirror would reject.
	atBound := strings.Repeat("界", maxImportInjectStringLength)

	tests := []struct {
		name string
		// plugins is the block under test; wantField empty means accepted.
		plugins    *ImportPluginsSpec
		wantField  string
		wantType   field.ErrorType
		wantDetail string
	}{
		{
			// Nil-safety is pinned rather than implied: the ControlPlane calls this
			// validator on a field its own CRs usually leave unset.
			name:    "nil block accepted",
			plugins: nil,
		},
		{
			// No sub-block is no plugin, which is a legitimate way to carry the
			// field while every plugin stays off.
			name:    "empty block accepted",
			plugins: &ImportPluginsSpec{},
		},
		{
			name:    "decompression alone accepted",
			plugins: &ImportPluginsSpec{Decompression: &ImportDecompressionSpec{}},
		},
		{
			// An empty outputFormat asks for the render-time default, so it must
			// not be measured against the enum.
			name:    "empty outputFormat accepted",
			plugins: &ImportPluginsSpec{Conversion: &ImportConversionSpec{}},
		},
		{
			name:       "unsupported outputFormat rejected",
			plugins:    &ImportPluginsSpec{Conversion: &ImportConversionSpec{OutputFormat: "qcow3"}},
			wantField:  "importPlugins.conversion.outputFormat",
			wantType:   field.ErrorTypeNotSupported,
			wantDetail: `supported values: "qcow2", "raw", "vmdk"`,
		},
		{
			name: "nil properties rejected",
			plugins: &ImportPluginsSpec{
				InjectMetadata: &ImportInjectMetadataSpec{},
			},
			wantField:  "importPlugins.injectMetadata.properties",
			wantType:   field.ErrorTypeRequired,
			wantDetail: "at least one property must be set",
		},
		{
			// The nil map and the explicit empty map must fail identically: the
			// required marker catches only the first.
			name: "empty properties rejected",
			plugins: &ImportPluginsSpec{
				InjectMetadata: &ImportInjectMetadataSpec{Properties: map[string]string{}},
			},
			wantField:  "importPlugins.injectMetadata.properties",
			wantType:   field.ErrorTypeRequired,
			wantDetail: "at least one property must be set",
		},
		{
			name: "too many properties rejected",
			plugins: &ImportPluginsSpec{
				InjectMetadata: &ImportInjectMetadataSpec{Properties: tooManyProperties},
			},
			wantField:  "importPlugins.injectMetadata.properties",
			wantType:   field.ErrorTypeTooMany,
			wantDetail: "must have at most 64 items",
		},
		{
			name: "empty property name rejected",
			plugins: &ImportPluginsSpec{
				InjectMetadata: &ImportInjectMetadataSpec{Properties: map[string]string{"": "value"}},
			},
			wantField:  "importPlugins.injectMetadata.properties",
			wantType:   field.ErrorTypeInvalid,
			wantDetail: "property name must not be empty",
		},
		{
			// The Dict parser would keep "hw" as the name and hand "vif_model" to
			// the value, so the property nobody wrote is the one that lands.
			name: "colon in a property name rejected",
			plugins: &ImportPluginsSpec{
				InjectMetadata: &ImportInjectMetadataSpec{
					Properties: map[string]string{"hw:vif_model": "virtio"},
				},
			},
			wantField:  "importPlugins.injectMetadata.properties",
			wantType:   field.ErrorTypeInvalid,
			wantDetail: "splits each pair on the first colon",
		},
		{
			name: "comma in a property name rejected",
			plugins: &ImportPluginsSpec{
				InjectMetadata: &ImportInjectMetadataSpec{
					Properties: map[string]string{"hw_qemu,guest_agent": "yes"},
				},
			},
			wantField:  "importPlugins.injectMetadata.properties",
			wantType:   field.ErrorTypeInvalid,
			wantDetail: "property name must not contain a comma",
		},
		{
			name: "overlong property name rejected",
			plugins: &ImportPluginsSpec{
				InjectMetadata: &ImportInjectMetadataSpec{Properties: map[string]string{overlong: "value"}},
			},
			wantField:  "importPlugins.injectMetadata.properties",
			wantType:   field.ErrorTypeInvalid,
			wantDetail: "property name must be at most 255 characters",
		},
		{
			name: "newline in a property name rejected",
			plugins: &ImportPluginsSpec{
				InjectMetadata: &ImportInjectMetadataSpec{
					Properties: map[string]string{"owner\n[glance_store]\nrogue": "yes"},
				},
			},
			wantField:  "importPlugins.injectMetadata.properties",
			wantType:   field.ErrorTypeInvalid,
			wantDetail: "property name must not contain newline or carriage-return characters",
		},
		{
			name: "carriage return in a property name rejected",
			plugins: &ImportPluginsSpec{
				InjectMetadata: &ImportInjectMetadataSpec{Properties: map[string]string{"owner\rrogue": "yes"}},
			},
			wantField:  "importPlugins.injectMetadata.properties",
			wantType:   field.ErrorTypeInvalid,
			wantDetail: "property name must not contain newline or carriage-return characters",
		},
		{
			name: "leading whitespace in a property name rejected",
			plugins: &ImportPluginsSpec{
				InjectMetadata: &ImportInjectMetadataSpec{Properties: map[string]string{" owner": "platform"}},
			},
			wantField:  "importPlugins.injectMetadata.properties",
			wantType:   field.ErrorTypeInvalid,
			wantDetail: "property name must not start or end with whitespace",
		},
		{
			name: "trailing whitespace in a property name rejected",
			plugins: &ImportPluginsSpec{
				InjectMetadata: &ImportInjectMetadataSpec{Properties: map[string]string{"owner ": "platform"}},
			},
			wantField:  "importPlugins.injectMetadata.properties",
			wantType:   field.ErrorTypeInvalid,
			wantDetail: "property name must not start or end with whitespace",
		},
		{
			name: "comma in a property value rejected",
			plugins: &ImportPluginsSpec{
				InjectMetadata: &ImportInjectMetadataSpec{
					Properties: map[string]string{"owner": "platform,team"},
				},
			},
			wantField:  "importPlugins.injectMetadata.properties",
			wantType:   field.ErrorTypeInvalid,
			wantDetail: "property value must not contain a comma",
		},
		{
			name: "newline in a property value rejected",
			plugins: &ImportPluginsSpec{
				InjectMetadata: &ImportInjectMetadataSpec{
					Properties: map[string]string{"owner": "platform\n[glance_store]\nrogue = 1"},
				},
			},
			wantField:  "importPlugins.injectMetadata.properties",
			wantType:   field.ErrorTypeInvalid,
			wantDetail: "property value must not contain newline or carriage-return characters",
		},
		{
			name: "surrounding whitespace in a property value rejected",
			plugins: &ImportPluginsSpec{
				InjectMetadata: &ImportInjectMetadataSpec{Properties: map[string]string{"owner": " platform "}},
			},
			wantField:  "importPlugins.injectMetadata.properties",
			wantType:   field.ErrorTypeInvalid,
			wantDetail: "property value must not start or end with whitespace",
		},
		{
			// The value half needs the bound as much as the name half: every pair
			// renders verbatim into the one inject line, so 64 unbounded values
			// admit a CR whose glance-api.conf crosses the 1 MiB ConfigMap ceiling
			// and can then never be applied.
			name: "overlong property value rejected",
			plugins: &ImportPluginsSpec{
				InjectMetadata: &ImportInjectMetadataSpec{Properties: map[string]string{"owner": overlong}},
			},
			wantField:  "importPlugins.injectMetadata.properties",
			wantType:   field.ErrorTypeInvalid,
			wantDetail: "property value must be at most 255 characters",
		},
		{
			// The schema counterparts count code points (CEL size(), MaxLength), so
			// a name at the bound must be admitted however many bytes its runes
			// occupy — measuring bytes here would reject 85 CJK characters under a
			// message naming a 255-character limit.
			name: "multi-byte property name at the code-point bound accepted",
			plugins: &ImportPluginsSpec{
				InjectMetadata: &ImportInjectMetadataSpec{
					Properties: map[string]string{atBound: "platform"},
				},
			},
		},
		{
			name: "multi-byte property value at the code-point bound accepted",
			plugins: &ImportPluginsSpec{
				InjectMetadata: &ImportInjectMetadataSpec{
					Properties: map[string]string{"owner": atBound},
				},
			},
		},
		{
			name: "multi-byte role at the code-point bound accepted",
			plugins: &ImportPluginsSpec{
				InjectMetadata: &ImportInjectMetadataSpec{
					Properties:      map[string]string{"owner": "platform"},
					IgnoreUserRoles: []string{atBound},
				},
			},
		},
		{
			// Everything past the first colon belongs to the value, so a URL is a
			// legitimate property — rejecting it would rule out the provenance
			// annotations this plugin exists for.
			name: "colon in a property value accepted",
			plugins: &ImportPluginsSpec{
				InjectMetadata: &ImportInjectMetadataSpec{
					Properties: map[string]string{"image_source": "https://mirror.example.com:8443/images"},
				},
			},
		},
		{
			name: "empty role rejected",
			plugins: &ImportPluginsSpec{
				InjectMetadata: &ImportInjectMetadataSpec{
					Properties:      map[string]string{"owner": "platform"},
					IgnoreUserRoles: []string{""},
				},
			},
			wantField:  "importPlugins.injectMetadata.ignoreUserRoles[0]",
			wantType:   field.ErrorTypeInvalid,
			wantDetail: "role must not be empty",
		},
		{
			name: "comma in a role rejected",
			plugins: &ImportPluginsSpec{
				InjectMetadata: &ImportInjectMetadataSpec{
					Properties:      map[string]string{"owner": "platform"},
					IgnoreUserRoles: []string{"admin,reader"},
				},
			},
			wantField:  "importPlugins.injectMetadata.ignoreUserRoles[0]",
			wantType:   field.ErrorTypeInvalid,
			wantDetail: "role must not contain a comma",
		},
		{
			name: "overlong role rejected",
			plugins: &ImportPluginsSpec{
				InjectMetadata: &ImportInjectMetadataSpec{
					Properties:      map[string]string{"owner": "platform"},
					IgnoreUserRoles: []string{overlong},
				},
			},
			wantField:  "importPlugins.injectMetadata.ignoreUserRoles[0]",
			wantType:   field.ErrorTypeInvalid,
			wantDetail: "role must be at most 255 characters",
		},
		{
			name: "too many roles rejected",
			plugins: &ImportPluginsSpec{
				InjectMetadata: &ImportInjectMetadataSpec{
					Properties:      map[string]string{"owner": "platform"},
					IgnoreUserRoles: tooManyRoles,
				},
			},
			wantField:  "importPlugins.injectMetadata.ignoreUserRoles",
			wantType:   field.ErrorTypeTooMany,
			wantDetail: "must have at most 64 items",
		},
		{
			// The explicitly empty list is the opt-in to injecting for admins too,
			// so it must survive admission as written rather than read as unset.
			name: "explicitly empty ignoreUserRoles accepted",
			plugins: &ImportPluginsSpec{
				InjectMetadata: &ImportInjectMetadataSpec{
					Properties:      map[string]string{"owner": "platform"},
					IgnoreUserRoles: []string{},
				},
			},
		},
		{
			name: "all three plugins accepted",
			plugins: &ImportPluginsSpec{
				Decompression: &ImportDecompressionSpec{},
				Conversion:    &ImportConversionSpec{OutputFormat: "raw"},
				InjectMetadata: &ImportInjectMetadataSpec{
					Properties:      map[string]string{"hw_scsi_model": "virtio-scsi", "owner": "platform"},
					IgnoreUserRoles: []string{"admin", "service"},
				},
			},
		},
		// Every value of the enum must be admitted, not just the one the fixture
		// above happens to use.
		{
			name:    "outputFormat qcow2 accepted",
			plugins: &ImportPluginsSpec{Conversion: &ImportConversionSpec{OutputFormat: "qcow2"}},
		},
		{
			name:    "outputFormat raw accepted",
			plugins: &ImportPluginsSpec{Conversion: &ImportConversionSpec{OutputFormat: "raw"}},
		},
		{
			name:    "outputFormat vmdk accepted",
			plugins: &ImportPluginsSpec{Conversion: &ImportConversionSpec{OutputFormat: "vmdk"}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			errs := ValidateImportPlugins(field.NewPath("spec").Child("importPlugins"), tc.plugins)
			if tc.wantField == "" {
				g.Expect(errs).To(gomega.BeEmpty())
			} else {
				g.Expect(errs).To(gomega.HaveLen(1))
				g.Expect(errs[0].Type).To(gomega.Equal(tc.wantType))
				g.Expect(errs[0].Field).To(gomega.ContainSubstring("spec." + tc.wantField))
				g.Expect(errs[0].Detail).To(gomega.ContainSubstring(tc.wantDetail))
			}

			w := &GlanceWebhook{}
			obj := validGlance()
			obj.Spec.ImportPlugins = tc.plugins
			if tc.plugins != nil && tc.plugins.Decompression != nil {
				// The whole-CR verbs also run the cross-field rule that requires an
				// explicit staging bound alongside the decompression plugin
				// (TestValidateImportDecompressionStaging owns that rule), so satisfy
				// it here — this table is about ValidateImportPlugins alone.
				obj.Spec.Staging = &StagingSpec{SizeLimit: ptr.To(resource.MustParse("40Gi"))}
			}

			_, createErr := w.ValidateCreate(context.Background(), obj)
			_, updateErr := w.ValidateUpdate(context.Background(), validGlance(), obj)

			if tc.wantField == "" {
				g.Expect(createErr).NotTo(gomega.HaveOccurred())
				g.Expect(updateErr).NotTo(gomega.HaveOccurred())
				return
			}
			for verb, err := range map[string]error{"create": createErr, "update": updateErr} {
				g.Expect(err).To(gomega.HaveOccurred(), "%s must reject %s", verb, tc.name)
				g.Expect(err.Error()).To(gomega.ContainSubstring("spec." + tc.wantField))
				g.Expect(err.Error()).To(gomega.ContainSubstring(tc.wantDetail))
			}
		})
	}
}

// An oversized property value must not come back verbatim: field.Invalid renders
// BadValue into the admission response, the operator's webhook log, and any
// RequestResponse audit entry, so a value sized against etcd's ~1.5 MiB object
// ceiling would be reproduced in full three times over for a message that is a
// fixed-length statement.
func TestValidateImportPlugins_OversizedValueIsNotEchoedBack(t *testing.T) {
	g := gomega.NewWithT(t)
	huge := strings.Repeat("a", 64*1024)

	errs := ValidateImportPlugins(field.NewPath("spec", "importPlugins"), &ImportPluginsSpec{
		InjectMetadata: &ImportInjectMetadataSpec{Properties: map[string]string{"owner": huge}},
	})

	g.Expect(errs).To(gomega.HaveLen(1))
	bad, ok := errs[0].BadValue.(string)
	g.Expect(ok).To(gomega.BeTrue(), "the property value is reported as the BadValue")
	g.Expect(utf8.RuneCountInString(bad)).To(gomega.Equal(maxImportInjectStringLength+1),
		"the echoed value keeps the bound's worth of prefix plus the ellipsis")
	g.Expect(bad).To(gomega.HavePrefix(huge[:maxImportInjectStringLength]))
	g.Expect(errs[0].Error()).NotTo(gomega.ContainSubstring(huge))
}

// The same must hold for the property NAME, which is echoed twice over: once as
// the BadValue and once as the map-key subscript of the field path, which
// field.Error renders in front of every message — including the ones about the
// value, which name the property they belong to. And it must hold on every
// branch, not only the one that measures length: a comma or a newline is
// rejected before the size is ever looked at, so an oversized key or value
// carrying one would otherwise reach the response untouched.
func TestValidateImportPlugins_OversizedKeyIsNotEchoedBack(t *testing.T) {
	huge := strings.Repeat("a", 64*1024)

	tests := []struct {
		name  string
		key   string
		value string
	}{
		{name: "oversized key", key: huge, value: "platform"},
		{name: "oversized key with a colon", key: huge + ":x", value: "platform"},
		{name: "oversized key with a comma", key: huge + ",x", value: "platform"},
		{name: "oversized key with trailing whitespace", key: huge + " ", value: "platform"},
		{name: "oversized key with a newline", key: huge + "\n", value: "platform"},
		{name: "oversized value with a comma", key: "owner", value: huge + ",x"},
		{name: "oversized value with a newline", key: "owner", value: huge + "\n"},
		{name: "oversized value with trailing whitespace", key: "owner", value: huge + " "},
		// The path names the property on the value side too, so an oversized key
		// must stay truncated even when it is the value that is rejected.
		{name: "oversized key and value", key: huge, value: huge + ",x"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			errs := ValidateImportPlugins(field.NewPath("spec", "importPlugins"), &ImportPluginsSpec{
				InjectMetadata: &ImportInjectMetadataSpec{Properties: map[string]string{tc.key: tc.value}},
			})

			g.Expect(errs).NotTo(gomega.BeEmpty())
			for _, err := range errs {
				g.Expect(err.Field).NotTo(gomega.ContainSubstring(huge),
					"the field path must not carry the property name in full")
				g.Expect(err.Error()).NotTo(gomega.ContainSubstring(huge),
					"neither half may be reproduced in full anywhere in the rendered error")
			}
		})
	}
}

// An oversized ignoreUserRoles entry is echoed as the BadValue on every branch
// that reports it, not only the one that measures its length.
func TestValidateImportPlugins_OversizedRoleIsNotEchoedBack(t *testing.T) {
	huge := strings.Repeat("a", 64*1024)

	for _, role := range []string{huge, huge + ",x", huge + "\n"} {
		g := gomega.NewWithT(t)

		errs := ValidateImportPlugins(field.NewPath("spec", "importPlugins"), &ImportPluginsSpec{
			InjectMetadata: &ImportInjectMetadataSpec{
				Properties:      map[string]string{"owner": "platform"},
				IgnoreUserRoles: []string{role},
			},
		})

		g.Expect(errs).To(gomega.HaveLen(1))
		g.Expect(errs[0].Error()).NotTo(gomega.ContainSubstring(huge))
	}
}

// TestValidateImportDecompressionStaging pins the cross-field rule that ties the
// image_decompression plugin to a staging bound somebody chose. It correlates
// two top-level spec fields, so no CRD marker reaches it and the webhook is the
// sole gate — which also means nothing but this test turns red if the call is
// dropped from either webhook.
func TestValidateImportDecompressionStaging(t *testing.T) {
	sizeLimit := resource.MustParse("40Gi")

	tests := []struct {
		name    string
		plugins *ImportPluginsSpec
		staging *StagingSpec
		wantErr bool
	}{
		{
			name:    "nil plugins accepted",
			plugins: nil,
		},
		{
			// Only the decompression plugin expands past what the caller
			// transferred, so the other two carry no such requirement.
			name: "conversion and injectMetadata without staging accepted",
			plugins: &ImportPluginsSpec{
				Conversion: &ImportConversionSpec{OutputFormat: "raw"},
				InjectMetadata: &ImportInjectMetadataSpec{
					Properties: map[string]string{"owner": "platform"},
				},
			},
		},
		{
			name:    "decompression without a staging block rejected",
			plugins: &ImportPluginsSpec{Decompression: &ImportDecompressionSpec{}},
			wantErr: true,
		},
		{
			// An empty block is how a CR asks for the operator default, which is
			// exactly the inherited number this rule refuses.
			name:    "decompression with an empty staging block rejected",
			plugins: &ImportPluginsSpec{Decompression: &ImportDecompressionSpec{}},
			staging: &StagingSpec{},
			wantErr: true,
		},
		{
			name:    "decompression with an explicit sizeLimit accepted",
			plugins: &ImportPluginsSpec{Decompression: &ImportDecompressionSpec{}},
			staging: &StagingSpec{SizeLimit: &sizeLimit},
		},
		{
			// unbounded is the deliberate no-bound escape hatch; choosing it is an
			// answer to the same question.
			name:    "decompression with unbounded staging accepted",
			plugins: &ImportPluginsSpec{Decompression: &ImportDecompressionSpec{}},
			staging: &StagingSpec{Unbounded: true},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			errs := ValidateImportDecompressionStaging(field.NewPath("spec"), tc.plugins, tc.staging)

			obj := validGlance()
			obj.Spec.ImportPlugins = tc.plugins
			obj.Spec.Staging = tc.staging
			w := &GlanceWebhook{}
			_, createErr := w.ValidateCreate(context.Background(), obj)
			_, updateErr := w.ValidateUpdate(context.Background(), validGlance(), obj)

			if !tc.wantErr {
				g.Expect(errs).To(gomega.BeEmpty())
				g.Expect(createErr).NotTo(gomega.HaveOccurred())
				g.Expect(updateErr).NotTo(gomega.HaveOccurred())
				return
			}

			g.Expect(errs).To(gomega.HaveLen(1))
			g.Expect(errs[0].Type).To(gomega.Equal(field.ErrorTypeRequired))
			g.Expect(errs[0].Field).To(gomega.Equal("spec.staging.sizeLimit"))
			g.Expect(errs[0].Detail).To(gomega.ContainSubstring("importPlugins.decompression is enabled"))
			for verb, err := range map[string]error{"create": createErr, "update": updateErr} {
				g.Expect(err).To(gomega.HaveOccurred(), "%s must reject %s", verb, tc.name)
				g.Expect(err.Error()).To(gomega.ContainSubstring("spec.staging.sizeLimit"))
			}
		})
	}
}

// Every spec.importPlugins default is resolved at render time, never stamped
// into the stored CR — and each sub-block's mere presence is what enables its
// plugin, so a defaulter materializing one would switch a plugin on for every
// Glance in the cluster. Nothing else in the suite exercises the defaulter
// against spec.importPlugins, so such a rule would land without a single test
// turning red.
func TestGlanceDefault_LeavesImportPluginsUnset(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &GlanceWebhook{}

	obj := validGlance()
	g.Expect(w.Default(context.Background(), obj)).To(gomega.Succeed())
	g.Expect(obj.Spec.ImportPlugins).To(gomega.BeNil(),
		"spec.importPlugins must not be materialized by the defaulting webhook")

	// A set block must survive untouched too: its unset fields are how the CR
	// keeps tracking DefaultImportConversionOutputFormat and
	// DefaultImportInjectIgnoreUserRoles across upgrades.
	set := validGlance()
	set.Spec.ImportPlugins = &ImportPluginsSpec{
		Conversion: &ImportConversionSpec{},
		InjectMetadata: &ImportInjectMetadataSpec{
			Properties: map[string]string{"owner": "platform"},
		},
	}
	g.Expect(w.Default(context.Background(), set)).To(gomega.Succeed())
	g.Expect(set.Spec.ImportPlugins.Conversion.OutputFormat).To(gomega.BeEmpty(),
		"outputFormat must stay unset so it keeps tracking the operator default")
	g.Expect(set.Spec.ImportPlugins.InjectMetadata.IgnoreUserRoles).To(gomega.BeNil(),
		"ignoreUserRoles must stay nil so it keeps tracking the operator default")
	g.Expect(set.Spec.ImportPlugins.Decompression).To(gomega.BeNil(),
		"an unrequested plugin must not be materialized by the defaulting webhook")
}

// metadata.name is bounded by the child object with the tightest name budget,
// the "{name}-db-purge" CronJob. Nothing else in the CRD or the webhook bounds
// it, so without this rule a name the API server would refuse as a CronJob
// admits cleanly and the operator falls back to the opaque hashed form it keeps
// only for CRs that predate the bound.
func TestGlanceValidateCreate_NameLengthBoundedByPurgeCronJob(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &GlanceWebhook{}

	atLimit := validGlance()
	atLimit.Name = strings.Repeat("g", MaxGlanceNameLength)
	_, err := w.ValidateCreate(context.Background(), atLimit)
	g.Expect(err).NotTo(gomega.HaveOccurred(),
		"a name that still fits the 52-character CronJob budget must be accepted")

	tooLong := validGlance()
	tooLong.Name = strings.Repeat("g", MaxGlanceNameLength+1)
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
func TestGlanceValidateUpdate_OverlongNameStaysUpdatable(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &GlanceWebhook{}

	grandfathered := validGlance()
	grandfathered.Name = strings.Repeat("g", MaxGlanceNameLength+1)
	grandfathered.Finalizers = []string{"glance.openstack.c5c3.io/finalizer"}

	deleting := grandfathered.DeepCopy()
	deleting.Finalizers = nil

	_, err := w.ValidateUpdate(context.Background(), grandfathered, deleting)
	g.Expect(err).NotTo(gomega.HaveOccurred(),
		"an over-long grandfathered CR must stay updatable, or its deletion never completes")
}

// Shortening the retention window is the one edit on this CR that destroys data
// at the next firing with no undo, and a typo is indistinguishable from an
// intended change at admission time. The warning is what echoes it back.
func TestGlanceValidateUpdate_WarnsOnReducedDBPurgeRetention(t *testing.T) {
	newObjWith := func(p *DBPurgeSpec) *Glance {
		o := validGlance()
		o.Spec.DBPurge = p
		return o
	}
	tests := []struct {
		name     string
		old, new *DBPurgeSpec
		wantWarn bool
	}{
		{
			name: "explicit reduction warns",
			old:  &DBPurgeSpec{RetentionDays: ptr.To(int32(30))},
			new:  &DBPurgeSpec{RetentionDays: ptr.To(int32(1))},
			// A month of rows becomes purgeable the moment the CronJob next fires.
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
			name:     "an unrelated edit is silent",
			old:      &DBPurgeSpec{RetentionDays: ptr.To(int32(7))},
			new:      &DBPurgeSpec{RetentionDays: ptr.To(int32(7)), Schedule: "@weekly"},
			wantWarn: false,
		},
		{
			// Dropping the block restores the default (30), which is a widening
			// from 7 — not a reduction to zero.
			name:     "dropping the block below the default is silent",
			old:      &DBPurgeSpec{RetentionDays: ptr.To(int32(7))},
			new:      nil,
			wantWarn: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			w := &GlanceWebhook{}

			warnings, err := w.ValidateUpdate(context.Background(), newObjWith(tc.old), newObjWith(tc.new))
			g.Expect(err).NotTo(gomega.HaveOccurred())
			if tc.wantWarn {
				g.Expect(warnings).To(gomega.ContainElement(
					gomega.ContainSubstring("spec.dbPurge.retentionDays reduced"),
				))
			} else {
				g.Expect(warnings).NotTo(gomega.ContainElement(
					gomega.ContainSubstring("spec.dbPurge.retentionDays reduced"),
				))
			}
		})
	}
}

// TestGlanceDefault_EmptyImportFilteringListSurvivesAdmission pins the one thing
// the explicitly-empty-list opt-out depends on and that no other test exercises:
// the round-trip through the mutating webhook.
//
// The opt-out is a nil-versus-empty distinction, and every list on
// ImportFilteringSpec carries `omitempty`, so json.Marshal of the defaulted
// object drops an empty-but-present list. The defaulter builds its patch by
// diffing that marshalled object against the raw request, which would turn the
// drop into a `remove` op and silently unset the field — the CR would then
// resolve back to the operator default it just opted out of. controller-runtime
// prevents exactly that: it recomputes the raw-to-undefaulted patch and discards
// every `remove` the marshal round-trip alone produces. This test asserts that
// behavior through the real handler rather than trusting it, because a
// controller-runtime bump that drops it would erase a documented opt-out with no
// other failing test.
func TestGlanceDefault_EmptyImportFilteringListSurvivesAdmission(t *testing.T) {
	g := gomega.NewWithT(t)
	scheme := runtime.NewScheme()
	g.Expect(AddToScheme(scheme)).To(gomega.Succeed())

	// allowedSchemes: [] is the documented way to make disallowedSchemes
	// authoritative; the defaulter must leave it present and empty.
	raw := []byte(`{
		"apiVersion": "glance.openstack.c5c3.io/v1alpha1",
		"kind": "Glance",
		"metadata": {"name": "test-glance", "namespace": "openstack"},
		"spec": {
			"openStackRelease": "2025.2",
			"image": {"repository": "ghcr.io/c5c3/glance", "tag": "2025.2"},
			"keystoneEndpoint": "http://keystone.openstack.svc.cluster.local:5000/v3",
			"importFiltering": {"allowedSchemes": [], "disallowedSchemes": ["http"]}
		}
	}`)

	resp := admission.WithDefaulter[*Glance](scheme, &GlanceWebhook{}).Handle(
		context.Background(),
		admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: admissionv1.Create,
			Object:    runtime.RawExtension{Raw: raw},
		}},
	)

	g.Expect(resp.Allowed).To(gomega.BeTrue())
	for _, p := range resp.Patches {
		g.Expect(p.Operation).NotTo(gomega.Equal("remove"),
			"the defaulting patch must not remove a field the request carried: %+v", p)
	}
}

// TestGlanceValidateCreate_ImportFilteringWarnings pins the two shapes that are
// admitted but do not mean what they look like, and — just as importantly — the
// shapes that must stay silent. A deny-list is inert while its sibling allow-list
// resolves to a non-empty operator default, and widening an allow-list removes
// the scheme/port pin that the literal host denylist cannot stand in for.
func TestGlanceValidateCreate_ImportFilteringWarnings(t *testing.T) {
	tests := []struct {
		name        string
		filtering   *ImportFilteringSpec
		wantSubs    []string
		notWantSubs []string
		wantNone    bool
	}{
		{
			name:     "nil block is silent",
			wantNone: true,
		},
		{
			name:      "default-equal allow lists are silent",
			filtering: &ImportFilteringSpec{AllowedSchemes: []string{"https"}, AllowedPorts: []int32{443}},
			wantNone:  true,
		},
		{
			// Tightening: a host deny-list is authoritative on its own, because
			// allowedHosts has no operator default to keep it non-empty.
			name:      "host deny-list alone is silent",
			filtering: &ImportFilteringSpec{DisallowedHosts: []string{"mirror.example.com"}},
			wantNone:  true,
		},
		{
			name:      "deny-only schemes warn as inert",
			filtering: &ImportFilteringSpec{DisallowedSchemes: []string{"http"}},
			wantSubs: []string{
				"spec.importFiltering.disallowedSchemes is set while spec.importFiltering.allowedSchemes is unset",
				"this list is inert",
			},
		},
		{
			name:      "deny-only ports warn as inert",
			filtering: &ImportFilteringSpec{DisallowedPorts: []int32{80}},
			wantSubs: []string{
				"spec.importFiltering.disallowedPorts is set while spec.importFiltering.allowedPorts is unset",
				"this list is inert",
			},
		},
		{
			// The documented opt-out: the explicitly empty allow-list is what
			// makes the deny-list authoritative, so no inertness warning — but
			// emptying it drops the pin, which does warn.
			name: "explicit empty allow list warns only about the widening",
			filtering: &ImportFilteringSpec{
				AllowedSchemes:    []string{},
				DisallowedSchemes: []string{"http"},
			},
			wantSubs:    []string{"widening the operator default"},
			notWantSubs: []string{"this list is inert"},
		},
		{
			// The loosening the tempest legs and the CRD reference document.
			name: "http mirror loosening warns on both attributes",
			filtering: &ImportFilteringSpec{
				AllowedSchemes: []string{"http", "https"},
				AllowedPorts:   []int32{80, 443},
			},
			wantSubs: []string{
				"spec.importFiltering.allowedSchemes is set to [http https], widening the operator default [https]",
				"spec.importFiltering.allowedPorts is set to [80 443], widening the operator default [443]",
				"spec.networkPolicy.additionalEgress",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			w := &GlanceWebhook{}
			obj := validGlance()
			obj.Spec.ImportFiltering = tc.filtering

			warnings, err := w.ValidateCreate(context.Background(), obj)
			g.Expect(err).NotTo(gomega.HaveOccurred())

			joined := strings.Join(warnings, "\n")
			if tc.wantNone {
				g.Expect(joined).NotTo(gomega.ContainSubstring("importFiltering"))
				return
			}
			for _, sub := range tc.wantSubs {
				g.Expect(joined).To(gomega.ContainSubstring(sub))
			}
			for _, sub := range tc.notWantSubs {
				g.Expect(joined).NotTo(gomega.ContainSubstring(sub))
			}
		})
	}
}

// TestGlanceValidate_UWSGIHarakiriDrainWindow pins the shutdown-envelope rule:
// harakiri must be strictly less than the drain window
// (terminationGracePeriodSeconds - preStopSleepSeconds). With the effective
// defaults (grace 30, preStop 5) the drain window is 25.
func TestGlanceValidate_UWSGIHarakiriDrainWindow(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(o *Glance)
		wantErr bool
	}{
		{
			name:    "harakiri nil accepted",
			mutate:  func(o *Glance) { o.Spec.APIServer = &APIServerSpec{UWSGI: &UWSGISpec{Processes: 2}} },
			wantErr: false,
		},
		{
			name: "harakiri below drain window accepted",
			mutate: func(o *Glance) {
				o.Spec.APIServer = &APIServerSpec{UWSGI: &UWSGISpec{Harakiri: ptr.To(int32(24))}}
			},
			wantErr: false,
		},
		{
			name: "harakiri equal to drain window rejected",
			mutate: func(o *Glance) {
				o.Spec.APIServer = &APIServerSpec{UWSGI: &UWSGISpec{Harakiri: ptr.To(int32(25))}}
			},
			wantErr: true,
		},
		{
			name: "harakiri above drain window rejected",
			mutate: func(o *Glance) {
				o.Spec.APIServer = &APIServerSpec{UWSGI: &UWSGISpec{Harakiri: ptr.To(int32(26))}}
			},
			wantErr: true,
		},
		{
			name: "harakiri honored against explicit grace/preStop window",
			mutate: func(o *Glance) {
				// Explicit grace 60, preStop 10 → drain 50; harakiri 40 would be
				// rejected under the default drain window (25) but fits here.
				o.Spec.Deployment.TerminationGracePeriodSeconds = ptr.To(int64(60))
				o.Spec.Deployment.PreStopSleepSeconds = ptr.To(int64(10))
				o.Spec.APIServer = &APIServerSpec{UWSGI: &UWSGISpec{Harakiri: ptr.To(int32(40))}}
			},
			wantErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			w := &GlanceWebhook{}
			obj := validGlance()
			tc.mutate(obj)

			_, err := w.ValidateCreate(context.Background(), obj)
			if tc.wantErr {
				g.Expect(err).To(gomega.HaveOccurred())
				g.Expect(err.Error()).To(gomega.ContainSubstring(
					"must be strictly less than terminationGracePeriodSeconds - preStopSleepSeconds (25)",
				))
			} else {
				g.Expect(err).NotTo(gomega.HaveOccurred())
			}
		})
	}
}

// TestGlanceValidateUpdate_ValidatesNewObject confirms ValidateUpdate runs the
// value-level rules against the new object.
func TestGlanceValidateUpdate_ValidatesNewObject(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &GlanceWebhook{}
	oldObj := validGlance()
	newObj := validGlance()
	newObj.Spec.KeystonePublicEndpoint = "not-a-url"

	_, err := w.ValidateUpdate(context.Background(), oldObj, newObj)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("keystonePublicEndpoint"))
}

func TestGlanceValidateDelete_AlwaysAccepts(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &GlanceWebhook{}
	_, err := w.ValidateDelete(context.Background(), validGlance())
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

// TestGlanceWarnings_InertLaunchModeKnobs pins the launch-mode warning matrix:
// uwsgi is inert below 2026.1 (eventlet) and workers is inert from 2026.1
// (uWSGI). Both stay warnings, never errors.
func TestGlanceWarnings_InertLaunchModeKnobs(t *testing.T) {
	tests := []struct {
		name     string
		release  string
		mutate   func(o *Glance)
		wantWarn bool
	}{
		{
			name:     "uwsgi set on eventlet release warns",
			release:  "2025.2",
			mutate:   func(o *Glance) { o.Spec.APIServer = &APIServerSpec{UWSGI: &UWSGISpec{Processes: 2}} },
			wantWarn: true,
		},
		{
			name:     "workers set on uwsgi release warns",
			release:  "2026.1",
			mutate:   func(o *Glance) { o.Spec.APIServer = &APIServerSpec{Workers: ptr.To(int32(4))} },
			wantWarn: true,
		},
		{
			name:     "uwsgi set on uwsgi release does not warn",
			release:  "2026.1",
			mutate:   func(o *Glance) { o.Spec.APIServer = &APIServerSpec{UWSGI: &UWSGISpec{Processes: 2}} },
			wantWarn: false,
		},
		{
			name:     "workers set on eventlet release does not warn",
			release:  "2025.2",
			mutate:   func(o *Glance) { o.Spec.APIServer = &APIServerSpec{Workers: ptr.To(int32(4))} },
			wantWarn: false,
		},
		{
			name:     "neither knob set does not warn",
			release:  "2025.2",
			mutate:   func(o *Glance) {},
			wantWarn: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			w := &GlanceWebhook{}
			obj := validGlance()
			obj.Spec.OpenStackRelease = tc.release
			tc.mutate(obj)

			warnings, err := w.ValidateCreate(context.Background(), obj)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			if tc.wantWarn {
				g.Expect(warnings).NotTo(gomega.BeEmpty())
			} else {
				g.Expect(warnings).To(gomega.BeEmpty())
			}
		})
	}
}

// --- extraConfig option-catalog validation ---

// TestGlanceValidate_ExtraConfigUnknownOptionRejected pins that an unknown option
// name under a known catalog section is rejected. "default_backend_typo" is a
// typo for the [glance_store] default_backend option, so the catalog does not
// list it.
func TestGlanceValidate_ExtraConfigUnknownOptionRejected(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &GlanceWebhook{}
	obj := validGlance()
	obj.Spec.ExtraConfig = map[string]map[string]string{
		"glance_store": {"default_backend_typo": "s3"},
	}

	_, err := w.ValidateCreate(context.Background(), obj)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("no such option in the glance 2025.2 option catalog"))
}

// TestGlanceValidate_ExtraConfigUnknownSectionRejected pins that an option under
// a section the catalog does not contain is rejected as an unknown section.
// "glance_stor" is a typo for "glance_store".
func TestGlanceValidate_ExtraConfigUnknownSectionRejected(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &GlanceWebhook{}
	obj := validGlance()
	obj.Spec.ExtraConfig = map[string]map[string]string{
		"glance_stor": {"default_backend": "s3"},
	}

	_, err := w.ValidateCreate(context.Background(), obj)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("no such section in the glance 2025.2 option catalog"))
}

// TestGlanceValidate_ExtraConfigReservedStoreSectionsExempt pins that the
// reserved glance_store staging and tasks sections are skipped whole: an
// arbitrary file-store option under either — one the release catalog does not
// list — is accepted, because CatalogExemptSections exempts the section itself
// (not just the two operator-owned datadir keys).
func TestGlanceValidate_ExtraConfigReservedStoreSectionsExempt(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &GlanceWebhook{}
	obj := validGlance()
	obj.Spec.ExtraConfig = map[string]map[string]string{
		"os_glance_staging_store": {"filesystem_store_chunk_size": "65536"},
		"os_glance_tasks_store":   {"filesystem_store_chunk_size": "65536"},
	}

	_, err := w.ValidateCreate(context.Background(), obj)
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

// TestGlanceValidate_ExtraConfigOwnershipRegistryKeyExempt pins that an
// operator-owned (section, key) pair is exempt even when the catalog section
// exists but does not list that key. [keystone_authtoken] username is a
// non-Rejected registry entry whose key the 2025.2 catalog does not carry, so
// without the registry exemption it would be flagged as an unknown option.
func TestGlanceValidate_ExtraConfigOwnershipRegistryKeyExempt(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &GlanceWebhook{}
	obj := validGlance()
	obj.Spec.ExtraConfig = map[string]map[string]string{
		"keystone_authtoken": {"username": "glance-svc"},
	}

	_, err := w.ValidateCreate(context.Background(), obj)
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

// TestGlanceValidate_ExtraConfigImportFilteringOwnedKeyRejected pins that a
// [import_filtering_opts] key cannot be set through spec.extraConfig: all six are
// Rejected registry entries, so the URI filter is only reachable through
// spec.importFiltering and cannot be loosened past the exclusivity rules, the
// host INI guard, and the WarnImportFiltering warning.
//
// It also pins the per-key registry exemption that gets it here: the section is
// absent from every embedded catalog, but FindUnknownOptions consults the
// exemptions before it decides the section is unknown, so a registered key
// escapes the section verdict and reaches the precise ownership error instead.
func TestGlanceValidate_ExtraConfigImportFilteringOwnedKeyRejected(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &GlanceWebhook{}
	obj := validGlance()
	obj.Spec.ExtraConfig = map[string]map[string]string{
		"import_filtering_opts": {"allowed_schemes": "http,https"},
	}

	_, err := w.ValidateCreate(context.Background(), obj)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("allowed_schemes is managed via spec.importFiltering"))
	g.Expect(err.Error()).To(gomega.ContainSubstring("the web-download URI filter is platform security policy"))
	g.Expect(err.Error()).NotTo(gomega.ContainSubstring("no such section"))
}

// TestGlanceValidate_ExtraConfigImageCacheOwnedKeyRejected pins that none of the
// three [DEFAULT] image_cache_* keys can be set through spec.extraConfig. They
// are Rejected rather than Reported because extraConfig wins over every operator
// default in the merge and ExtraConfigHealthy never gates Ready: an
// image_cache_max_size above the emptyDir bound leaves the pruner nothing to
// prune down to, an image_cache_dir elsewhere spends another emptyDir's bound,
// and image_cache_driver back at centralized_db writes rows into the shared
// Glance database that no db-purge pass reclaims — each one damage already done
// by the time an informational condition could report it.
//
// The reservation does not depend on spec.imageCache being set: the registry
// records "this key is not the user's to set", the same posture every other
// conditionally rendered key here has.
func TestGlanceValidate_ExtraConfigImageCacheOwnedKeyRejected(t *testing.T) {
	tests := []struct {
		key        string
		value      string
		wantImpact string
	}{
		{"image_cache_dir", "/var/lib/glance/staging", "another volume's bound"},
		{"image_cache_driver", "centralized_db", "no db-purge pass reclaims them"},
		{"image_cache_max_size", "1099511627776", "the volume grows until the kubelet evicts the pod"},
	}

	for _, tc := range tests {
		t.Run(tc.key, func(t *testing.T) {
			g := gomega.NewWithT(t)
			w := &GlanceWebhook{}
			obj := validGlance()
			obj.Spec.ImageCache = &ImageCacheSpec{}
			obj.Spec.ExtraConfig = map[string]map[string]string{
				"DEFAULT": {tc.key: tc.value},
			}

			_, createErr := w.ValidateCreate(context.Background(), obj)
			_, updateErr := w.ValidateUpdate(context.Background(), validGlance(), obj)

			for verb, err := range map[string]error{"create": createErr, "update": updateErr} {
				g.Expect(err).To(gomega.HaveOccurred(), "%s must reject [DEFAULT] %s", verb, tc.key)
				g.Expect(err.Error()).To(gomega.ContainSubstring(tc.key + " is managed via spec.imageCache"))
				g.Expect(err.Error()).To(gomega.ContainSubstring(tc.wantImpact))
			}

			// The cache being off does not hand the key back: the operator owns it
			// whenever it renders it, so the rejection is unconditional.
			off := validGlance()
			off.Spec.ExtraConfig = obj.Spec.ExtraConfig
			_, err := w.ValidateCreate(context.Background(), off)
			g.Expect(err).To(gomega.HaveOccurred(),
				"[DEFAULT] %s must stay reserved even while spec.imageCache is nil", tc.key)
		})
	}
}

// TestGlanceValidate_ExtraConfigImportFilteringUnknownKeyRejected pins the other
// half of that exemption: only the six registered keys are admitted under
// [import_filtering_opts]. Any other key falls through to the unknown-section
// verdict, because no catalog carries the section.
func TestGlanceValidate_ExtraConfigImportFilteringUnknownKeyRejected(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &GlanceWebhook{}
	obj := validGlance()
	obj.Spec.ExtraConfig = map[string]map[string]string{
		"import_filtering_opts": {"no_such_option": "x"},
	}

	_, err := w.ValidateCreate(context.Background(), obj)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("no such section in the glance 2025.2 option catalog"))
}

// TestGlanceValidate_ExtraConfigImportPluginsOwnedKeyRejected pins that none of
// the four image-import plugin keys can be set through spec.extraConfig. They
// are Rejected rather than Reported because each one reaches past a guarantee
// spec.importPlugins makes at admission: a hand-written image_import_plugins
// list can put conversion ahead of decompression, a hand-written output_format
// escapes the enum, and a hand-written inject or ignore_user_roles escapes the
// property-name rules that keep a pair inside the oslo Dict syntax.
//
// The last case pins the other half of the registry exemption: none of the three
// sections appears in any embedded catalog, so only the registered keys reach
// the ownership error and every other key still falls through to the
// unknown-section verdict.
func TestGlanceValidate_ExtraConfigImportPluginsOwnedKeyRejected(t *testing.T) {
	tests := []struct {
		name    string
		section string
		key     string
		value   string
	}{
		{"image_import_plugins", "image_import_opts", "image_import_plugins", "image_conversion,image_decompression"},
		{"output_format", "image_conversion", "output_format", "vmdk"},
		{"inject", "inject_metadata_properties", "inject", "owner:rogue"},
		{"ignore_user_roles", "inject_metadata_properties", "ignore_user_roles", "admin,service"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			w := &GlanceWebhook{}
			obj := validGlance()
			obj.Spec.ImportPlugins = &ImportPluginsSpec{Decompression: &ImportDecompressionSpec{}}
			obj.Spec.ExtraConfig = map[string]map[string]string{
				tc.section: {tc.key: tc.value},
			}

			_, createErr := w.ValidateCreate(context.Background(), obj)
			_, updateErr := w.ValidateUpdate(context.Background(), validGlance(), obj)

			for verb, err := range map[string]error{"create": createErr, "update": updateErr} {
				g.Expect(err).To(gomega.HaveOccurred(), "%s must reject [%s] %s", verb, tc.section, tc.key)
				g.Expect(err.Error()).To(gomega.ContainSubstring(tc.key + " is managed via spec.importPlugins"))
				g.Expect(err.Error()).To(gomega.ContainSubstring(
					"bypasses the fixed decompression-before-conversion order",
				))
				// The registry exemption is what gets the override here: without it
				// the section is unknown to every catalog and the user would see the
				// catalog verdict instead of the ownership rejection.
				g.Expect(err.Error()).NotTo(gomega.ContainSubstring("no such section"))
			}

			// No plugin enabled does not hand the key back: the registry records
			// "this key is not the user's to set", the posture every other
			// conditionally rendered key here has.
			off := validGlance()
			off.Spec.ExtraConfig = obj.Spec.ExtraConfig
			_, err := w.ValidateCreate(context.Background(), off)
			g.Expect(err).To(gomega.HaveOccurred(),
				"[%s] %s must stay reserved even while spec.importPlugins is nil", tc.section, tc.key)
		})
	}

	t.Run("unregistered key in an owned section stays a catalog error", func(t *testing.T) {
		g := gomega.NewWithT(t)
		w := &GlanceWebhook{}
		obj := validGlance()
		obj.Spec.ExtraConfig = map[string]map[string]string{
			"image_import_opts": {"no_such_option": "x"},
		}

		_, err := w.ValidateCreate(context.Background(), obj)
		g.Expect(err).To(gomega.HaveOccurred())
		g.Expect(err.Error()).To(gomega.ContainSubstring("no such section in the glance 2025.2 option catalog"))
	})
}

// TestGlanceValidate_ExtraConfigPluginSectionExempt pins that a section declared
// by a loaded plugin is skipped whole, so options under it are never flagged even
// though the catalog cannot enumerate them.
func TestGlanceValidate_ExtraConfigPluginSectionExempt(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &GlanceWebhook{}
	obj := validGlance()
	obj.Spec.Plugins = []commonv1.PluginSpec{
		{Name: "custom-store", ConfigSection: "custom_store"},
	}
	obj.Spec.ExtraConfig = map[string]map[string]string{
		"custom_store": {"endpoint": "https://store.example.com", "bucket": "images"},
	}

	_, err := w.ValidateCreate(context.Background(), obj)
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

// TestGlanceValidate_ExtraConfigEmptyReleaseFailsOpen pins the fail-open behavior
// when spec.openStackRelease does not name a release: the catalog cannot be
// resolved, so the check emits a single warning and no catalog error even though
// "default_backend_typo" is unknown. Other validation may reject the empty
// release, so this asserts only on the warning and the absence of catalog errors.
func TestGlanceValidate_ExtraConfigEmptyReleaseFailsOpen(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &GlanceWebhook{}
	obj := validGlance()
	obj.Spec.OpenStackRelease = ""
	obj.Spec.ExtraConfig = map[string]map[string]string{
		"glance_store": {"default_backend_typo": "s3"},
	}

	warnings, err := w.ValidateCreate(context.Background(), obj)
	g.Expect(warnings).To(gomega.ContainElement(
		"spec.extraConfig was not validated against an option catalog: spec.openStackRelease does not name an OpenStack release",
	))
	if err != nil {
		g.Expect(err.Error()).NotTo(gomega.ContainSubstring("option catalog"))
	}
}

// TestGlanceValidate_ExtraConfigUnknownReleaseFailsOpen pins the fail-open
// behavior for a parseable release the operator ships no catalog for.
func TestGlanceValidate_ExtraConfigUnknownReleaseFailsOpen(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &GlanceWebhook{}
	obj := validGlance()
	obj.Spec.OpenStackRelease = "2027.1"
	obj.Spec.ExtraConfig = map[string]map[string]string{
		"glance_store": {"default_backend_typo": "s3"},
	}

	warnings, err := w.ValidateCreate(context.Background(), obj)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(warnings).To(gomega.ContainElement(
		`spec.extraConfig was not validated against an option catalog: no catalog for release "2027.1" is embedded in this operator build`,
	))
}

// TestGlanceValidate_ExtraConfigDeprecatedOptionWarns pins that a
// deprecated-but-still-accepted option is honored (no error) but surfaces a
// warning naming its replacement. [DEFAULT] logfile is deprecated in favor of
// [DEFAULT] log_file.
func TestGlanceValidate_ExtraConfigDeprecatedOptionWarns(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &GlanceWebhook{}
	obj := validGlance()
	obj.Spec.ExtraConfig = map[string]map[string]string{
		"DEFAULT": {"logfile": "/var/log/glance/glance.log"},
	}

	warnings, err := w.ValidateCreate(context.Background(), obj)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(warnings).To(gomega.ContainElement(gomega.ContainSubstring(
		"deprecated option in glance 2025.2, replaced by [DEFAULT] log_file",
	)))
}

// TestGlanceValidateUpdate_ExtraConfigCatalogGate exercises all four arms of the
// UPDATE re-validation gate. The old object carries a since-invalidated
// extraConfig; the check re-runs only when extraConfig, the plugin
// config-section set, or spec.openStackRelease changes.
func TestGlanceValidateUpdate_ExtraConfigCatalogGate(t *testing.T) {
	w := &GlanceWebhook{}
	oldObj := func() *Glance {
		o := validGlance()
		o.Spec.ExtraConfig = map[string]map[string]string{
			"glance_store": {"default_backend_typo": "s3"},
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
			"glance_store": {"another_typo": "x"},
		}

		_, err := w.ValidateUpdate(context.Background(), old, newObj)
		g.Expect(err).To(gomega.HaveOccurred())
		g.Expect(err.Error()).To(gomega.ContainSubstring("no such option in the glance 2025.2 option catalog"))
	})

	t.Run("openStackRelease edit is re-validated and rejected", func(t *testing.T) {
		g := gomega.NewWithT(t)
		old := oldObj()
		newObj := old.DeepCopy()
		newObj.Spec.OpenStackRelease = "2026.1"

		_, err := w.ValidateUpdate(context.Background(), old, newObj)
		g.Expect(err).To(gomega.HaveOccurred())
		g.Expect(err.Error()).To(gomega.ContainSubstring("no such option in the glance 2026.1 option catalog"))
	})

	t.Run("plugin-list edit is re-validated and rejected", func(t *testing.T) {
		g := gomega.NewWithT(t)
		old := oldObj()
		newObj := old.DeepCopy()
		newObj.Spec.Plugins = []commonv1.PluginSpec{
			{Name: "custom-store", ConfigSection: "custom_store"},
		}

		_, err := w.ValidateUpdate(context.Background(), old, newObj)
		g.Expect(err).To(gomega.HaveOccurred())
		g.Expect(err.Error()).To(gomega.ContainSubstring("no such option in the glance 2025.2 option catalog"))
	})
}
