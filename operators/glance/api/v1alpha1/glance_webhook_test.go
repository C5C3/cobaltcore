// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"context"
	"testing"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

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

	// Shared-block defaults come along too.
	g.Expect(obj.Spec.Deployment.Resources).NotTo(gomega.BeNil())
	g.Expect(obj.Spec.Cache.Backend).To(gomega.Equal(commonv1.DefaultCacheBackend))
	g.Expect(obj.Spec.Logging).NotTo(gomega.BeNil())
	g.Expect(obj.Spec.Logging.Format).To(gomega.Equal("text"))
	g.Expect(obj.Spec.Logging.Level).To(gomega.Equal("INFO"))
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
			// The operator owns [keystone_authtoken] password via
			// spec.serviceUser.secretRef and env-injects it at runtime, so a file
			// override is inert but would leak the service password into the
			// namespace-readable ConfigMap. It is the registry's single Rejected
			// entry, blocked at admission rather than merely reported.
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
