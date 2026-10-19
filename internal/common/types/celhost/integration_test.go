// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package celhost

import (
	"fmt"
	"maps"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	commonenvtest "github.com/c5c3/cobaltcore/internal/common/testutil/envtest"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

// callerDir returns the directory containing this source file, resolved via
// runtime.Caller(0) so the CRD path is independent of the working directory.
func callerDir() string {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		panic("celhost: runtime.Caller failed to determine source file path")
	}
	return filepath.Dir(thisFile)
}

// TestIntegration_CRD_CELOnly_SharedTypes pins the CEL rules of the shared
// types against an envtest API server that carries only the CELHost CRD. Each
// reject case sets one block on an otherwise empty spec, so the rule it names
// is the only one in play. One accept case sets every block to a valid value;
// the others admit the boundary of a single rule. A subtest fails when a rule
// of the CRD has no reject case.
//
// Rules: types.go:68, 149, 150, 285, 286, 311, 444, 445
// Rules: workload.go:124, 226, 227, 280, 281, 317, 386, 387
// Rules: uwsgi.go:15
func TestIntegration_CRD_CELOnly_SharedTypes(t *testing.T) {
	commonenvtest.SkipIfEnvTestUnavailable(t)
	c, ctx, _ := commonenvtest.SetupEnvTestWithCRDs(t,
		commonenvtest.BuildScheme(AddToScheme), []string{filepath.Join(callerDir(), "crd")})

	const (
		imageExactlyOne     = "exactly one of image.tag or image.digest must be set"
		databaseExactlyOne  = "exactly one of clusterRef or host must be set"
		cacheExactlyOne     = "exactly one of clusterRef or servers must be set"
		messagingExactlyOne = "exactly one of clusterRef or secretRef must be set"
		preStopBeforeGrace  = "preStopSleepSeconds must be strictly less than terminationGracePeriodSeconds"
	)
	ref := func(name string) *corev1.LocalObjectReference { return &corev1.LocalObjectReference{Name: name} }
	secret := commonv1.SecretRefSpec{Name: "s"}
	storage := corev1.ResourceList{corev1.ResourceEphemeralStorage: resource.MustParse("1Gi")}

	cases := []struct {
		name    string
		spec    CELHostSpec
		wantSub string // empty: the object must be admitted
	}{
		{
			name:    "image-tag-and-digest",
			spec:    CELHostSpec{Image: &commonv1.ImageSpec{Repository: "r", Tag: "t", Digest: "sha256:" + strings.Repeat("a", 64)}},
			wantSub: imageExactlyOne,
		},
		{
			name:    "image-neither",
			spec:    CELHostSpec{Image: &commonv1.ImageSpec{Repository: "r"}},
			wantSub: imageExactlyOne,
		},
		{
			name: "database-cluster-ref-and-host",
			spec: CELHostSpec{Database: &commonv1.DatabaseSpec{
				ClusterRef: ref("db"), Host: "db.example.com", Database: "k", SecretRef: secret,
			}},
			wantSub: databaseExactlyOne,
		},
		{
			name:    "database-neither",
			spec:    CELHostSpec{Database: &commonv1.DatabaseSpec{Database: "k", SecretRef: secret}},
			wantSub: databaseExactlyOne,
		},
		{
			name: "database-dynamic-on-host",
			spec: CELHostSpec{Database: &commonv1.DatabaseSpec{
				Host: "db.example.com", CredentialsMode: "Dynamic", Database: "k", SecretRef: secret,
			}},
			wantSub: "credentialsMode Dynamic requires clusterRef (managed mode)",
		},
		{
			name: "cache-cluster-ref-and-servers",
			spec: CELHostSpec{Cache: &commonv1.CacheSpec{
				ClusterRef: ref("mc"), Servers: []string{"mc:11211"}, Backend: commonv1.DefaultCacheBackend,
			}},
			wantSub: cacheExactlyOne,
		},
		{
			name:    "cache-neither",
			spec:    CELHostSpec{Cache: &commonv1.CacheSpec{Backend: commonv1.DefaultCacheBackend}},
			wantSub: cacheExactlyOne,
		},
		{
			name:    "cache-newline-in-cluster-ref",
			spec:    CELHostSpec{Cache: &commonv1.CacheSpec{ClusterRef: ref("mc\nx"), Backend: commonv1.DefaultCacheBackend}},
			wantSub: "clusterRef.name must not contain a newline or carriage return",
		},
		{
			name:    "cache-carriage-return-in-cluster-ref",
			spec:    CELHostSpec{Cache: &commonv1.CacheSpec{ClusterRef: ref("mc\rx"), Backend: commonv1.DefaultCacheBackend}},
			wantSub: "clusterRef.name must not contain a newline or carriage return",
		},
		{
			name: "messaging-cluster-ref-and-secret-ref",
			spec: CELHostSpec{Messaging: &commonv1.MessagingSpec{
				ClusterRef: ref("mq"), SecretRef: &commonv1.SecretRefSpec{Name: "bus"},
			}},
			wantSub: messagingExactlyOne,
		},
		{
			name:    "messaging-neither",
			spec:    CELHostSpec{Messaging: &commonv1.MessagingSpec{}},
			wantSub: messagingExactlyOne,
		},
		{
			name:    "policy-empty-rule-name",
			spec:    CELHostSpec{Policy: &commonv1.PolicySpec{Rules: map[string]string{"": "role:admin"}}},
			wantSub: "policy rule name must not be empty",
		},
		{
			name:    "policy-empty-rule-value",
			spec:    CELHostSpec{Policy: &commonv1.PolicySpec{Rules: map[string]string{"identity:list_users": ""}}},
			wantSub: "policy rule value must not be empty",
		},
		{
			name: "deployment-pre-stop-equals-grace",
			spec: CELHostSpec{Deployment: &commonv1.DeploymentSpec{
				PreStopSleepSeconds: ptr.To[int64](30), TerminationGracePeriodSeconds: ptr.To[int64](30),
			}},
			wantSub: preStopBeforeGrace,
		},
		{
			// The rule substitutes 30 for an absent terminationGracePeriodSeconds;
			// the CRD declares no schema default for it.
			name:    "deployment-pre-stop-above-the-grace-fallback",
			spec:    CELHostSpec{Deployment: &commonv1.DeploymentSpec{PreStopSleepSeconds: ptr.To[int64](40)}},
			wantSub: preStopBeforeGrace,
		},
		{
			name:    "autoscaling-without-targets",
			spec:    CELHostSpec{Autoscaling: &commonv1.AutoscalingSpec{MaxReplicas: 3}},
			wantSub: "at least one of targetCPUUtilization or targetMemoryUtilization must be set",
		},
		{
			name: "autoscaling-min-above-max",
			spec: CELHostSpec{Autoscaling: &commonv1.AutoscalingSpec{
				MinReplicas: ptr.To[int32](5), MaxReplicas: 3, TargetCPUUtilization: ptr.To[int32](80),
			}},
			wantSub: "minReplicas must not exceed maxReplicas",
		},
		{
			name: "autoscaling-min-equals-max",
			spec: CELHostSpec{Autoscaling: &commonv1.AutoscalingSpec{
				MinReplicas: ptr.To[int32](3), MaxReplicas: 3, TargetCPUUtilization: ptr.To[int32](80),
			}},
		},
		{
			name:    "vertical-autoscaling-foreign-min-allowed",
			spec:    CELHostSpec{VerticalAutoscaling: &commonv1.VerticalAutoscalingSpec{UpdateMode: "Off", MinAllowed: storage}},
			wantSub: "minAllowed may only name cpu and memory",
		},
		{
			name:    "vertical-autoscaling-foreign-max-allowed",
			spec:    CELHostSpec{VerticalAutoscaling: &commonv1.VerticalAutoscalingSpec{UpdateMode: "Off", MaxAllowed: storage}},
			wantSub: "maxAllowed may only name cpu and memory",
		},
		{
			name:    "network-policy-empty-ingress",
			spec:    CELHostSpec{NetworkPolicy: &commonv1.NetworkPolicySpec{Ingress: []commonv1.NetworkPolicyIngressSource{}}},
			wantSub: "at least one ingress source must be specified",
		},
		{
			name:    "logging-empty-logger-name",
			spec:    CELHostSpec{Logging: &commonv1.LoggingSpec{PerLoggerLevels: map[string]string{"": "INFO"}}},
			wantSub: "logger name must not be empty",
		},
		{
			name:    "logging-unknown-level",
			spec:    CELHostSpec{Logging: &commonv1.LoggingSpec{PerLoggerLevels: map[string]string{"keystone": "TRACE"}}},
			wantSub: "per-logger level must be one of DEBUG, INFO, WARNING, ERROR, CRITICAL",
		},
		{
			name: "uwsgi-timeout-without-keep-alive",
			spec: CELHostSpec{UWSGI: &commonv1.UWSGISpec{
				HTTPKeepAlive: ptr.To(false), HTTPKeepAliveTimeout: ptr.To[int32](5),
			}},
			wantSub: "httpKeepAliveTimeout may only be set when httpKeepAlive is true",
		},
		{
			name: "every-block-valid",
			spec: CELHostSpec{
				Image:     &commonv1.ImageSpec{Repository: "ghcr.io/c5c3/keystone", Tag: "2026.1"},
				Database:  &commonv1.DatabaseSpec{ClusterRef: ref("db"), Database: "keystone", SecretRef: commonv1.SecretRefSpec{Name: "db-creds"}},
				Cache:     &commonv1.CacheSpec{ClusterRef: ref("memcached"), Backend: commonv1.DefaultCacheBackend},
				Messaging: &commonv1.MessagingSpec{ClusterRef: ref("rabbitmq")},
				Policy:    &commonv1.PolicySpec{Rules: map[string]string{"identity:list_users": "role:admin"}},
				Deployment: &commonv1.DeploymentSpec{
					Replicas: 1, PreStopSleepSeconds: ptr.To[int64](5), TerminationGracePeriodSeconds: ptr.To[int64](30),
				},
				Autoscaling: &commonv1.AutoscalingSpec{MaxReplicas: 3, TargetCPUUtilization: ptr.To[int32](80)},
				VerticalAutoscaling: &commonv1.VerticalAutoscalingSpec{
					UpdateMode: "Off",
					MinAllowed: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
					MaxAllowed: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
				},
				NetworkPolicy: &commonv1.NetworkPolicySpec{Ingress: []commonv1.NetworkPolicyIngressSource{{}}},
				Logging:       &commonv1.LoggingSpec{PerLoggerLevels: map[string]string{"keystone": "DEBUG"}},
				UWSGI:         &commonv1.UWSGISpec{HTTPKeepAlive: ptr.To(true), HTTPKeepAliveTimeout: ptr.To[int32](5)},
			},
		},
		{
			// The keep-alive rule reads an absent httpKeepAlive as satisfied.
			name: "uwsgi-timeout-with-keep-alive-unset",
			spec: CELHostSpec{UWSGI: &commonv1.UWSGISpec{HTTPKeepAliveTimeout: ptr.To[int32](5)}},
		},
	}

	t.Run("every-crd-rule-has-a-reject-case", func(t *testing.T) {
		g := NewGomegaWithT(t)
		crd, err := crdCELMessages(filepath.Join(callerDir(), crdPath))
		g.Expect(err).NotTo(HaveOccurred())
		rejected := map[string]struct{}{}
		for _, tc := range cases {
			if tc.wantSub != "" {
				rejected[tc.wantSub] = struct{}{}
			}
		}
		g.Expect(slices.Sorted(maps.Keys(rejected))).To(Equal(slices.Sorted(maps.Keys(crd))),
			"every rule of the CELHost CRD needs a reject case whose wantSub is its full message")
	})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "celonly-shared-"}}
			g.Expect(c.Create(ctx, ns)).To(Succeed(), "create namespace")

			err := c.Create(ctx, &CELHost{ObjectMeta: metav1.ObjectMeta{Name: tc.name, Namespace: ns.Name}, Spec: tc.spec})
			if tc.wantSub == "" {
				g.Expect(err).NotTo(HaveOccurred(), "admission must admit the object")
				return
			}
			g.Expect(err).To(HaveOccurred(), "admission must reject: %s", tc.wantSub)
			g.Expect(apierrors.IsInvalid(err)).To(BeTrue(), fmt.Sprintf("expected Invalid status error, got: %v", err))
			g.Expect(err.Error()).To(ContainSubstring(tc.wantSub))
		})
	}
}
