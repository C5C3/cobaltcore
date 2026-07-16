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
