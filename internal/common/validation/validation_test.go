// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package validation

import (
	"context"
	"testing"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	commonv1 "github.com/c5c3/forge/internal/common/types"
)

var testPath = field.NewPath("spec", "test")

func TestDatabaseXOR(t *testing.T) {
	cases := []struct {
		name    string
		db      commonv1.DatabaseSpec
		wantErr bool
	}{
		{"managed mode valid", commonv1.DatabaseSpec{ClusterRef: &corev1.LocalObjectReference{Name: "db"}}, false},
		{"brownfield mode valid", commonv1.DatabaseSpec{Host: "db.example.com"}, false},
		{"both set rejected", commonv1.DatabaseSpec{ClusterRef: &corev1.LocalObjectReference{Name: "db"}, Host: "db.example.com"}, true},
		{"neither set rejected", commonv1.DatabaseSpec{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			errs := DatabaseXOR(testPath, &tc.db)
			g.Expect(len(errs) > 0).To(gomega.Equal(tc.wantErr))
		})
	}
}

func TestCacheXOR(t *testing.T) {
	cases := []struct {
		name    string
		cache   commonv1.CacheSpec
		wantErr bool
	}{
		{"managed mode valid", commonv1.CacheSpec{ClusterRef: &corev1.LocalObjectReference{Name: "mc"}}, false},
		{"brownfield mode valid", commonv1.CacheSpec{Servers: []string{"mc-0:11211"}}, false},
		{"both set rejected", commonv1.CacheSpec{ClusterRef: &corev1.LocalObjectReference{Name: "mc"}, Servers: []string{"mc-0:11211"}}, true},
		// An explicitly empty servers list is NOT a brownfield configuration:
		// with no clusterRef either, the spec has no usable cache source.
		{"neither set rejected", commonv1.CacheSpec{Servers: []string{}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			errs := CacheXOR(testPath, &tc.cache)
			g.Expect(len(errs) > 0).To(gomega.Equal(tc.wantErr))
		})
	}
}

func TestDynamicCredentialsRequireClusterRef(t *testing.T) {
	g := gomega.NewWithT(t)

	dynamicBrownfield := commonv1.DatabaseSpec{CredentialsMode: commonv1.CredentialsModeDynamic, Host: "db"}
	errs := DynamicCredentialsRequireClusterRef(testPath, &dynamicBrownfield)
	g.Expect(errs).To(gomega.HaveLen(1))
	g.Expect(errs[0].Field).To(gomega.Equal("spec.test.credentialsMode"))

	dynamicManaged := commonv1.DatabaseSpec{CredentialsMode: commonv1.CredentialsModeDynamic, ClusterRef: &corev1.LocalObjectReference{Name: "db"}}
	g.Expect(DynamicCredentialsRequireClusterRef(testPath, &dynamicManaged)).To(gomega.BeEmpty())

	staticBrownfield := commonv1.DatabaseSpec{CredentialsMode: commonv1.CredentialsModeStatic, Host: "db"}
	g.Expect(DynamicCredentialsRequireClusterRef(testPath, &staticBrownfield)).To(gomega.BeEmpty())
}

func TestCronSchedule(t *testing.T) {
	g := gomega.NewWithT(t)

	g.Expect(CronSchedule(testPath, "0 0 * * 0")).To(gomega.BeEmpty())
	g.Expect(CronSchedule(testPath, "@hourly")).To(gomega.BeEmpty())

	errs := CronSchedule(testPath, "not-a-cron")
	g.Expect(errs).To(gomega.HaveLen(1))
	g.Expect(errs[0].Detail).To(gomega.ContainSubstring("invalid cron expression"))

	// An empty schedule is a parse error here — the required-vs-defaulted
	// decision (and its message) is the caller's per-field policy.
	g.Expect(CronSchedule(testPath, "")).To(gomega.HaveLen(1))
}

func TestTopologySpreadSelector(t *testing.T) {
	required := map[string]string{
		"app.kubernetes.io/name":     "keystone",
		"app.kubernetes.io/instance": "ks",
	}
	tsc := func(sel *metav1.LabelSelector) corev1.TopologySpreadConstraint {
		return corev1.TopologySpreadConstraint{
			MaxSkew:           1,
			TopologyKey:       "kubernetes.io/hostname",
			WhenUnsatisfiable: corev1.ScheduleAnyway,
			LabelSelector:     sel,
		}
	}

	cases := []struct {
		name     string
		tscs     []corev1.TopologySpreadConstraint
		wantErrs int
	}{
		{"matching selector valid", []corev1.TopologySpreadConstraint{
			tsc(&metav1.LabelSelector{MatchLabels: required}),
		}, 0},
		{"missing selector rejected", []corev1.TopologySpreadConstraint{tsc(nil)}, 1},
		{"wrong labels rejected", []corev1.TopologySpreadConstraint{
			tsc(&metav1.LabelSelector{MatchLabels: map[string]string{"app": "other"}}),
		}, 1},
		{"matchExpressions rejected", []corev1.TopologySpreadConstraint{
			tsc(&metav1.LabelSelector{
				MatchLabels: required,
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: "zone", Operator: metav1.LabelSelectorOpExists},
				},
			}),
		}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			g.Expect(TopologySpreadSelector(testPath, tc.tscs, required)).To(gomega.HaveLen(tc.wantErrs))
		})
	}
}

func TestPriorityClassExists(t *testing.T) {
	g := gomega.NewWithT(t)

	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).
		WithObjects(&schedulingv1.PriorityClass{ObjectMeta: metav1.ObjectMeta{Name: "critical"}}).
		Build()

	g.Expect(PriorityClassExists(context.Background(), c, testPath, "critical")).To(gomega.BeEmpty())

	errs := PriorityClassExists(context.Background(), c, testPath, "typo")
	g.Expect(errs).To(gomega.HaveLen(1))
	g.Expect(errs[0].Type).To(gomega.Equal(field.ErrorTypeNotFound))

	// A nil Reader (programmatically constructed webhook without a client)
	// and an empty name both skip the lookup rather than failing closed.
	g.Expect(PriorityClassExists(context.Background(), nil, testPath, "critical")).To(gomega.BeEmpty())
	g.Expect(PriorityClassExists(context.Background(), c, testPath, "")).To(gomega.BeEmpty())
}
