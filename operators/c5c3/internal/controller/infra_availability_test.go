// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the anti-affinity and disruption budget of the backing services.
package controller

import (
	"context"
	"errors"
	"testing"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/v26/api/v1alpha1"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	"github.com/c5c3/cobaltcore/internal/common/messaging"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// availabilitySizing returns the availability block of one backing-service
// class: mode, and a budget of enabled and maxUnavailable when either is set.
func availabilitySizing(mode c5c3v1alpha1.AntiAffinityMode, enabled *bool, maxUnavailable *int32) c5c3v1alpha1.AvailabilitySizingSpec {
	a := c5c3v1alpha1.AvailabilitySizingSpec{AntiAffinity: mode}
	if enabled != nil || maxUnavailable != nil {
		a.PodDisruptionBudget = &c5c3v1alpha1.BackingPodDisruptionBudgetSpec{Enabled: enabled, MaxUnavailable: maxUnavailable}
	}
	return a
}

func TestResolveBackingAvailability(t *testing.T) {
	required, preferred, off := c5c3v1alpha1.AntiAffinityRequired, c5c3v1alpha1.AntiAffinityPreferred, c5c3v1alpha1.AntiAffinityOff
	for _, tc := range []struct {
		name     string
		a        c5c3v1alpha1.AvailabilitySizingSpec
		replicas int32
		want     backingAvailability
	}{
		{
			name: "one replica gets nothing, whatever the block sets", replicas: 1,
			a:    availabilitySizing(required, ptr.To(true), ptr.To[int32](2)),
			want: backingAvailability{antiAffinity: off},
		},
		{
			name: "zero replicas get nothing", replicas: 0,
			a:    availabilitySizing(preferred, nil, nil),
			want: backingAvailability{antiAffinity: off},
		},
		{
			name: "three replicas with an empty block take the default and a budget of 1", replicas: 3,
			want: backingAvailability{antiAffinity: preferred, maxUnavailable: ptr.To[int32](1), multiReplica: true},
		},
		{
			name: "a set mode wins over the default", replicas: 3,
			a:    availabilitySizing(off, nil, nil),
			want: backingAvailability{antiAffinity: off, maxUnavailable: ptr.To[int32](1), multiReplica: true},
		},
		{
			name: "a mode that bypassed admission takes the default", replicas: 3,
			a:    availabilitySizing("Strict", nil, nil),
			want: backingAvailability{antiAffinity: preferred, maxUnavailable: ptr.To[int32](1), multiReplica: true},
		},
		{
			name: "enabled: false yields no budget", replicas: 3,
			a:    availabilitySizing("", ptr.To(false), nil),
			want: backingAvailability{antiAffinity: preferred, multiReplica: true},
		},
		{
			name: "enabled: false yields no budget beside maxUnavailable 2", replicas: 3,
			a:    availabilitySizing("", ptr.To(false), ptr.To[int32](2)),
			want: backingAvailability{antiAffinity: preferred, multiReplica: true},
		},
		{
			name: "maxUnavailable 2 yields 2", replicas: 5,
			a:    availabilitySizing("", nil, ptr.To[int32](2)),
			want: backingAvailability{antiAffinity: preferred, maxUnavailable: ptr.To[int32](2), multiReplica: true},
		},
		{
			name: "a maxUnavailable 0 that bypassed admission yields 1", replicas: 3,
			a:    availabilitySizing("", ptr.To(true), ptr.To[int32](0)),
			want: backingAvailability{antiAffinity: preferred, maxUnavailable: ptr.To[int32](1), multiReplica: true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			g.Expect(resolveBackingAvailability(tc.a, tc.replicas, preferred)).To(Equal(tc.want))
		})
	}

	t.Run("the resolved count does not alias the sizing", func(t *testing.T) {
		g := NewGomegaWithT(t)
		a := availabilitySizing("", nil, ptr.To[int32](2))
		av := resolveBackingAvailability(a, 3, required)
		*av.maxUnavailable = 9
		g.Expect(*a.PodDisruptionBudget.MaxUnavailable).To(Equal(int32(2)))
	})
}

// wantMariaDBAffinity is the spec.affinity a MariaDB named name carries for
// mode, spelled out apart from the production helper: one hostname term on
// app.kubernetes.io/instance, required or preferred with weight 100. Off
// carries none.
func wantMariaDBAffinity(name string, mode c5c3v1alpha1.AntiAffinityMode) *mariadbv1alpha1.AffinityConfig {
	term := mariadbv1alpha1.PodAffinityTerm{
		LabelSelector: &mariadbv1alpha1.LabelSelector{
			MatchExpressions: []mariadbv1alpha1.LabelSelectorRequirement{{
				Key: "app.kubernetes.io/instance", Operator: metav1.LabelSelectorOpIn, Values: []string{name},
			}},
		},
		TopologyKey: "kubernetes.io/hostname",
	}
	switch mode {
	case c5c3v1alpha1.AntiAffinityRequired:
		return &mariadbv1alpha1.AffinityConfig{Affinity: mariadbv1alpha1.Affinity{
			PodAntiAffinity: &mariadbv1alpha1.PodAntiAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: []mariadbv1alpha1.PodAffinityTerm{term},
			},
		}}
	case c5c3v1alpha1.AntiAffinityPreferred:
		return &mariadbv1alpha1.AffinityConfig{Affinity: mariadbv1alpha1.Affinity{
			PodAntiAffinity: &mariadbv1alpha1.PodAntiAffinity{
				PreferredDuringSchedulingIgnoredDuringExecution: []mariadbv1alpha1.WeightedPodAffinityTerm{
					{Weight: 100, PodAffinityTerm: term},
				},
			},
		}}
	case c5c3v1alpha1.AntiAffinityOff:
	}
	return nil
}

// wantMariaDBBudget is the spec.podDisruptionBudget of maxUnavailable v.
func wantMariaDBBudget(v intstr.IntOrString) *mariadbv1alpha1.PodDisruptionBudget {
	return &mariadbv1alpha1.PodDisruptionBudget{MaxUnavailable: &v}
}

func TestEnsureMariaDB_Availability(t *testing.T) {
	databaseSizing := func(a c5c3v1alpha1.AvailabilitySizingSpec) *c5c3v1alpha1.DatabaseSizingSpec {
		return &c5c3v1alpha1.DatabaseSizingSpec{AvailabilitySizingSpec: a}
	}
	createTests := []struct {
		name         string
		replicas     int32
		sizing       *c5c3v1alpha1.DatabaseSizingSpec
		wantAffinity *mariadbv1alpha1.AffinityConfig
		wantBudget   *mariadbv1alpha1.PodDisruptionBudget
	}{
		{
			name: "three replicas get the required term and a budget of 1", replicas: 3,
			sizing:       databaseSizing(c5c3v1alpha1.AvailabilitySizingSpec{}),
			wantAffinity: wantMariaDBAffinity("openstack-db", c5c3v1alpha1.AntiAffinityRequired),
			wantBudget:   wantMariaDBBudget(intstr.FromInt32(1)),
		},
		{
			name: "Preferred gets one weighted term", replicas: 3,
			sizing:       databaseSizing(availabilitySizing(c5c3v1alpha1.AntiAffinityPreferred, nil, nil)),
			wantAffinity: wantMariaDBAffinity("openstack-db", c5c3v1alpha1.AntiAffinityPreferred),
			wantBudget:   wantMariaDBBudget(intstr.FromInt32(1)),
		},
		{
			name: "Off beside enabled: false gets no affinity and a budget of 100%", replicas: 3,
			sizing:     databaseSizing(availabilitySizing(c5c3v1alpha1.AntiAffinityOff, ptr.To(false), nil)),
			wantBudget: wantMariaDBBudget(intstr.FromString("100%")),
		},
		{
			name: "maxUnavailable 2 reaches the budget", replicas: 5,
			sizing:       databaseSizing(availabilitySizing("", nil, ptr.To[int32](2))),
			wantAffinity: wantMariaDBAffinity("openstack-db", c5c3v1alpha1.AntiAffinityRequired),
			wantBudget:   wantMariaDBBudget(intstr.FromInt32(2)),
		},
		{
			name: "a single instance gets neither", replicas: 1,
			sizing: databaseSizing(availabilitySizing(c5c3v1alpha1.AntiAffinityRequired, ptr.To(true), ptr.To[int32](1))),
		},
		{
			name: "an instance without a count of its own is judged on the sizing's 3", replicas: 0,
			sizing:       &c5c3v1alpha1.DatabaseSizingSpec{Replicas: ptr.To[int32](3)},
			wantAffinity: wantMariaDBAffinity("openstack-db", c5c3v1alpha1.AntiAffinityRequired),
			wantBudget:   wantMariaDBBudget(intstr.FromInt32(1)),
		},
		{
			name: "an instance's own count of 1 wins over the sizing's 3", replicas: 1,
			sizing: &c5c3v1alpha1.DatabaseSizingSpec{Replicas: ptr.To[int32](3)},
		},
		{
			name: "a nil sizing at three replicas gets the required term and a budget of 1", replicas: 3,
			wantAffinity: wantMariaDBAffinity("openstack-db", c5c3v1alpha1.AntiAffinityRequired),
			wantBudget:   wantMariaDBBudget(intstr.FromInt32(1)),
		},
	}
	for _, tc := range createTests {
		t.Run("on create, "+tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			_, c, cp, _, pass := newEnsureMariaDBPass(t, g, tc.replicas, tc.sizing, nil, nil)
			_, err := pass()
			g.Expect(err).NotTo(HaveOccurred())
			m := getMariaDB(g, c, cp.Namespace)
			g.Expect(m.Spec.Affinity).To(Equal(tc.wantAffinity))
			g.Expect(m.Spec.PodDisruptionBudget).To(Equal(tc.wantBudget))
			if m.Spec.Affinity != nil {
				g.Expect(m.Spec.Affinity.AntiAffinityEnabled).To(BeNil(), "antiAffinityEnabled is never set")
			}
		})
	}

	// unprojected is an owned Galera MariaDB that carries everything a pass
	// writes except the anti-affinity and the budget.
	unprojected := func() *mariadbv1alpha1.MariaDB {
		m := ownedGaleraMariaDB("img:1")
		m.Spec.Galera.InitContainer.SecurityContext = wantMariaDBContainerSecurityContext()
		m.Spec.Affinity = nil
		m.Spec.PodDisruptionBudget = nil
		return m
	}

	t.Run("an owned Galera MariaDB without the fields gets them with one Update, then none", func(t *testing.T) {
		g := NewGomegaWithT(t)
		_, c, cp, updates, pass := newEnsureMariaDBPass(t, g, 3, nil, unprojected(), nil)
		_, err := pass()
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(*updates).To(Equal(1))
		live := getMariaDB(g, c, cp.Namespace)
		g.Expect(live.Spec.Affinity).To(Equal(wantMariaDBAffinity("openstack-db", c5c3v1alpha1.AntiAffinityRequired)))
		g.Expect(live.Spec.PodDisruptionBudget).To(Equal(wantMariaDBBudget(intstr.FromInt32(1))))

		_, err = pass()
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(*updates).To(Equal(1), "a second pass finds nothing to change")
		g.Expect(getMariaDB(g, c, cp.Namespace).ResourceVersion).To(Equal(live.ResourceVersion))
	})

	t.Run("a live antiAffinityEnabled survives the write and a live nodeAffinity is replaced", func(t *testing.T) {
		g := NewGomegaWithT(t)
		seed := unprojected()
		seed.Spec.Affinity = &mariadbv1alpha1.AffinityConfig{
			Affinity: mariadbv1alpha1.Affinity{NodeAffinity: &mariadbv1alpha1.NodeAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: &mariadbv1alpha1.NodeSelector{
					NodeSelectorTerms: []mariadbv1alpha1.NodeSelectorTerm{{
						MatchExpressions: []mariadbv1alpha1.NodeSelectorRequirement{{
							Key: "pool", Operator: corev1.NodeSelectorOpIn, Values: []string{"db"},
						}},
					}},
				},
			}},
			AntiAffinityEnabled: ptr.To(true),
		}
		sizing := databaseSizing(availabilitySizing(c5c3v1alpha1.AntiAffinityPreferred, nil, nil))
		_, c, cp, updates, pass := newEnsureMariaDBPass(t, g, 3, sizing, seed, nil)
		_, err := pass()
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(*updates).To(Equal(1))
		want := wantMariaDBAffinity("openstack-db", c5c3v1alpha1.AntiAffinityPreferred)
		want.AntiAffinityEnabled = ptr.To(true)
		g.Expect(getMariaDB(g, c, cp.Namespace).Spec.Affinity).To(Equal(want))

		_, err = pass()
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(*updates).To(Equal(1), "the kept antiAffinityEnabled counts as matching")
	})

	t.Run("an owned MariaDB scaled to one instance loses both fields", func(t *testing.T) {
		g := NewGomegaWithT(t)
		_, c, cp, _, pass := newEnsureMariaDBPass(t, g, 1, nil, ownedGaleraMariaDB("img:1"), nil)
		_, err := pass()
		g.Expect(err).NotTo(HaveOccurred())
		m := getMariaDB(g, c, cp.Namespace)
		g.Expect(m.Spec.Affinity).To(BeNil())
		g.Expect(m.Spec.PodDisruptionBudget).To(BeNil())
	})

	t.Run("a MariaDB the ControlPlane does not own is not written", func(t *testing.T) {
		g := NewGomegaWithT(t)
		s := infraTestScheme(t)
		cp := managedInfraControlPlane()
		cp.Spec.Infrastructure.Database.Replicas = 3
		foreign := unprojected()
		foreign.Namespace = cp.Namespace
		updates := 0
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, foreign).
			WithInterceptorFuncs(mariadbUpdateInterceptor(&updates, nil)).Build()
		r := &ControlPlaneReconciler{Client: c, Scheme: s}

		_, err := r.ensureMariaDB(context.Background(), c, cp, &cp.Spec.Infrastructure.Database, nil, cp.Namespace)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(updates).To(BeZero(), "an adopted MariaDB is read-only")
		m := getMariaDB(g, c, cp.Namespace)
		g.Expect(m.Spec.Affinity).To(BeNil())
		g.Expect(m.Spec.PodDisruptionBudget).To(BeNil())
	})

	t.Run("a failing Update is returned wrapped", func(t *testing.T) {
		g := NewGomegaWithT(t)
		sentinel := errors.New("update rejected")
		_, _, _, updates, pass := newEnsureMariaDBPass(t, g, 3, nil, unprojected(), sentinel)
		_, err := pass()
		g.Expect(err).To(MatchError(ContainSubstring(`updating owned MariaDB "openstack-db"`)))
		g.Expect(errors.Is(err, sentinel)).To(BeTrue())
		g.Expect(*updates).To(Equal(1))
	})
}

// wantMemcachedHA is the spec.highAvailability of a cache with preset and a
// budget of maxUnavailable pods, in the types the API decodes them to.
func wantMemcachedHA(preset string, maxUnavailable int64) map[string]interface{} {
	return map[string]interface{}{
		"antiAffinityPreset":  preset,
		"podDisruptionBudget": map[string]interface{}{"enabled": true, "maxUnavailable": maxUnavailable},
	}
}

func TestEnsureMemcached_Availability(t *testing.T) {
	cacheSizing := func(a c5c3v1alpha1.AvailabilitySizingSpec) *c5c3v1alpha1.CacheSizingSpec {
		return &c5c3v1alpha1.CacheSizingSpec{AvailabilitySizingSpec: a}
	}
	highAvailability := func(g Gomega, u *unstructured.Unstructured) (map[string]interface{}, bool) {
		ha, found, err := unstructured.NestedMap(u.Object, "spec", "highAvailability")
		g.Expect(err).NotTo(HaveOccurred())
		return ha, found
	}

	createTests := []struct {
		name     string
		replicas int32
		sizing   *c5c3v1alpha1.CacheSizingSpec
		want     map[string]interface{}
	}{
		{
			name: "three replicas get soft anti-affinity and a budget of 1", replicas: 3,
			sizing: cacheSizing(c5c3v1alpha1.AvailabilitySizingSpec{}), want: wantMemcachedHA("soft", 1),
		},
		{
			name: "Required gets hard anti-affinity", replicas: 3,
			sizing: cacheSizing(availabilitySizing(c5c3v1alpha1.AntiAffinityRequired, nil, nil)),
			want:   wantMemcachedHA("hard", 1),
		},
		{
			name: "Off beside a budget gets soft anti-affinity", replicas: 3,
			sizing: cacheSizing(availabilitySizing(c5c3v1alpha1.AntiAffinityOff, nil, ptr.To[int32](2))),
			want:   wantMemcachedHA("soft", 2),
		},
		{
			name: "Off beside enabled: false gets no spec.highAvailability", replicas: 3,
			sizing: cacheSizing(availabilitySizing(c5c3v1alpha1.AntiAffinityOff, ptr.To(false), nil)),
		},
		{
			name: "one replica gets no spec.highAvailability", replicas: 1,
			sizing: cacheSizing(availabilitySizing(c5c3v1alpha1.AntiAffinityRequired, ptr.To(true), ptr.To[int32](1))),
		},
		{
			name: "a cache without a count of its own is judged on the sizing's 3", replicas: 0,
			sizing: &c5c3v1alpha1.CacheSizingSpec{Replicas: ptr.To[int32](3)}, want: wantMemcachedHA("soft", 1),
		},
		{
			name: "a cache's own count of 1 wins over the sizing's 3", replicas: 1,
			sizing: &c5c3v1alpha1.CacheSizingSpec{Replicas: ptr.To[int32](3)},
		},
		{name: "a nil sizing at one replica gets no spec.highAvailability", replicas: 1},
	}
	for _, tc := range createTests {
		t.Run("on create, "+tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			s := infraTestScheme(t)
			cp := managedInfraControlPlane()
			cp.Spec.Infrastructure.Cache.Replicas = tc.replicas
			c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
			r := &ControlPlaneReconciler{Client: c, Scheme: s}

			_, err := r.ensureMemcached(context.Background(), c, cp, &cp.Spec.Infrastructure.Cache, tc.sizing, cp.Namespace)
			g.Expect(err).NotTo(HaveOccurred())
			u, _, _ := getMemcached(g, c, cp.Namespace)
			ha, found := highAvailability(g, u)
			if tc.want == nil {
				g.Expect(found).To(BeFalse(), "spec.highAvailability must be absent, got %v", ha)
				return
			}
			g.Expect(ha).To(Equal(tc.want))
		})
	}

	// owned builds a label-owned cache in the identity namespace of a
	// ControlPlane whose cache declares replicas, seeded at seedReplicas with
	// the restricted security and ha (unless nil) as spec.highAvailability.
	owned := func(
		t *testing.T, g Gomega, replicas int32, seedReplicas int64, ha interface{},
	) (*ControlPlaneReconciler, client.Client, *c5c3v1alpha1.ControlPlane, *int) {
		t.Helper()
		s := infraTestScheme(t)
		cp := splitNamespaceControlPlane()
		cp.Spec.Infrastructure.Cache.Replicas = replicas
		seed := seedMemcached("identity", controlPlaneChildLabels(cp), ptr.To(seedReplicas))
		g.Expect(unstructured.SetNestedMap(seed.Object, wantMemcachedSecurity(), "spec", "security")).To(Succeed())
		if ha != nil {
			g.Expect(unstructured.SetNestedField(seed.Object, ha, "spec", "highAvailability")).To(Succeed())
		}
		updates := 0
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, seed).
			WithInterceptorFuncs(memcachedUpdateInterceptor(&updates, nil)).Build()
		return &ControlPlaneReconciler{Client: c, Scheme: s}, c, cp, &updates
	}

	t.Run("an owned cache going from three replicas to one loses both keys with one Update, then none", func(t *testing.T) {
		g := NewGomegaWithT(t)
		r, c, cp, updates := owned(t, g, 1, 3, wantMemcachedHA("soft", 1))
		for range 2 {
			_, err := r.ensureMemcached(context.Background(), c, cp, &cp.Spec.Infrastructure.Cache, nil, "identity")
			g.Expect(err).NotTo(HaveOccurred())
		}
		g.Expect(*updates).To(Equal(1))
		u, replicas, _ := getMemcached(g, c, "identity")
		g.Expect(replicas).To(Equal(int64(1)))
		_, found := highAvailability(g, u)
		g.Expect(found).To(BeFalse())
	})

	t.Run("a live gracefulShutdown survives every write", func(t *testing.T) {
		g := NewGomegaWithT(t)
		graceful := map[string]interface{}{"enabled": true, "preStopDelaySeconds": int64(10)}
		r, c, cp, updates := owned(t, g, 3, 3, map[string]interface{}{"gracefulShutdown": graceful})

		_, err := r.ensureMemcached(context.Background(), c, cp, &cp.Spec.Infrastructure.Cache, nil, "identity")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(*updates).To(Equal(1))
		u, _, _ := getMemcached(g, c, "identity")
		ha, _ := highAvailability(g, u)
		want := wantMemcachedHA("soft", 1)
		want["gracefulShutdown"] = graceful
		g.Expect(ha).To(Equal(want))

		// Both settings off: the preset stays soft beside gracefulShutdown.
		off := cacheSizing(availabilitySizing(c5c3v1alpha1.AntiAffinityOff, ptr.To(false), nil))
		for range 2 {
			_, err = r.ensureMemcached(context.Background(), c, cp, &cp.Spec.Infrastructure.Cache, off, "identity")
			g.Expect(err).NotTo(HaveOccurred())
		}
		g.Expect(*updates).To(Equal(2), "the written soft preset counts as matching")
		u, _, _ = getMemcached(g, c, "identity")
		ha, _ = highAvailability(g, u)
		g.Expect(ha).To(Equal(map[string]interface{}{"antiAffinityPreset": "soft", "gracefulShutdown": graceful}))
	})

	t.Run("a spec.highAvailability that is not a map fails the pass without a write", func(t *testing.T) {
		g := NewGomegaWithT(t)
		r, c, cp, updates := owned(t, g, 3, 3, "soft")
		_, err := r.ensureMemcached(context.Background(), c, cp, &cp.Spec.Infrastructure.Cache, nil, "identity")
		g.Expect(err).To(MatchError(And(ContainSubstring(`reading Memcached "openstack-memcached"`),
			ContainSubstring("spec.highAvailability"))))
		g.Expect(*updates).To(BeZero())
	})

	t.Run("a Memcached the ControlPlane does not own is not written", func(t *testing.T) {
		g := NewGomegaWithT(t)
		s := infraTestScheme(t)
		cp := splitNamespaceControlPlane()
		foreign := seedMemcached("identity", nil, ptr.To(int64(3)))
		updates := 0
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, foreign).
			WithInterceptorFuncs(memcachedUpdateInterceptor(&updates, nil)).Build()
		r := &ControlPlaneReconciler{Client: c, Scheme: s}

		_, err := r.ensureMemcached(context.Background(), c, cp, &cp.Spec.Infrastructure.Cache, nil, "identity")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(updates).To(BeZero())
		u, _, _ := getMemcached(g, c, "identity")
		_, found := highAvailability(g, u)
		g.Expect(found).To(BeFalse())
	})
}

// wantBusAffinity is the spec.affinity of the RabbitmqCluster name for mode,
// spelled out apart from the production helper: one hostname term on
// app.kubernetes.io/name, required or preferred with weight 100.
func wantBusAffinity(name string, mode c5c3v1alpha1.AntiAffinityMode) map[string]interface{} {
	term := map[string]interface{}{
		"labelSelector": map[string]interface{}{"matchLabels": map[string]interface{}{"app.kubernetes.io/name": name}},
		"topologyKey":   "kubernetes.io/hostname",
	}
	if mode == c5c3v1alpha1.AntiAffinityPreferred {
		return map[string]interface{}{"podAntiAffinity": map[string]interface{}{
			"preferredDuringSchedulingIgnoredDuringExecution": []interface{}{
				map[string]interface{}{"weight": int64(100), "podAffinityTerm": term},
			},
		}}
	}
	return map[string]interface{}{"podAntiAffinity": map[string]interface{}{
		"requiredDuringSchedulingIgnoredDuringExecution": []interface{}{term},
	}}
}

// messagingAvailability returns a messaging sizing that carries a alone.
func messagingAvailability(a c5c3v1alpha1.AvailabilitySizingSpec) *c5c3v1alpha1.MessagingSizingSpec {
	return &c5c3v1alpha1.MessagingSizingSpec{AvailabilitySizingSpec: a}
}

// messagingReplicas returns a messaging sizing that sets the replica count
// alone.
func messagingReplicas(n int32) *c5c3v1alpha1.MessagingSizingSpec {
	return &c5c3v1alpha1.MessagingSizingSpec{ScaledSizingSpec: c5c3v1alpha1.ScaledSizingSpec{Replicas: ptr.To(n)}}
}

// ownedBus is a live RabbitmqCluster controlled by cp at replicas, carrying
// affinity (unless nil) as spec.affinity.
func ownedBus(
	t *testing.T, g Gomega, cp *c5c3v1alpha1.ControlPlane, replicas int64, affinity interface{},
) *unstructured.Unstructured {
	t.Helper()
	u := rabbitmqWithConditions("openstack-rabbitmq", cp.Namespace, nil)
	g.Expect(unstructured.SetNestedField(u.Object, replicas, "spec", "replicas")).To(Succeed())
	if affinity != nil {
		g.Expect(unstructured.SetNestedField(u.Object, affinity, "spec", "affinity")).To(Succeed())
	}
	g.Expect(controllerutil.SetControllerReference(cp, u, infraTestScheme(t))).To(Succeed())
	return u
}

// getBus reads the RabbitmqCluster openstack-rabbitmq of cp.
func getBus(g Gomega, c client.Client, cp *c5c3v1alpha1.ControlPlane) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(messaging.RabbitmqClusterGVK)
	g.Expect(c.Get(context.Background(), types.NamespacedName{
		Name: "openstack-rabbitmq", Namespace: cp.Namespace,
	}, u)).To(Succeed())
	return u
}

// busAffinity returns the spec.affinity of u and whether it is set.
func busAffinity(g Gomega, u *unstructured.Unstructured) (map[string]interface{}, bool) {
	affinity, found, err := unstructured.NestedMap(u.Object, "spec", "affinity")
	g.Expect(err).NotTo(HaveOccurred())
	return affinity, found
}

// controlledBudget is a live bus budget of maxUnavailable pods that cp
// controls, as ensureRabbitMQBudget creates it.
func controlledBudget(t *testing.T, g Gomega, cp *c5c3v1alpha1.ControlPlane, maxUnavailable int32) *policyv1.PodDisruptionBudget {
	t.Helper()
	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "openstack-rabbitmq", Namespace: cp.Namespace, UID: "pdb-uid"},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MaxUnavailable: ptr.To(intstr.FromInt32(maxUnavailable)),
			Selector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "openstack-rabbitmq"}},
		},
	}
	g.Expect(controllerutil.SetControllerReference(cp, pdb, infraTestScheme(t))).To(Succeed())
	return pdb
}

// getBudget reads the bus budget of cp, or returns nil when there is none.
func getBudget(g Gomega, c client.Client, cp *c5c3v1alpha1.ControlPlane) *policyv1.PodDisruptionBudget {
	pdb := &policyv1.PodDisruptionBudget{}
	err := c.Get(context.Background(), types.NamespacedName{Name: "openstack-rabbitmq", Namespace: cp.Namespace}, pdb)
	if apierrors.IsNotFound(err) {
		return nil
	}
	g.Expect(err).NotTo(HaveOccurred())
	return pdb
}

// failingBudget returns interceptor funcs that fail the one verb of get,
// create, update or delete with err on a PodDisruptionBudget and pass every
// other request through.
func failingBudget(verb string, err error) interceptor.Funcs {
	isBudget := func(obj client.Object) bool {
		_, ok := obj.(*policyv1.PodDisruptionBudget)
		return ok
	}
	return interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if verb == "get" && isBudget(obj) {
				return err
			}
			return c.Get(ctx, key, obj, opts...)
		},
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if verb == "create" && isBudget(obj) {
				return err
			}
			return c.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if verb == "update" && isBudget(obj) {
				return err
			}
			return c.Update(ctx, obj, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if verb == "delete" && isBudget(obj) {
				return err
			}
			return c.Delete(ctx, obj, opts...)
		},
	}
}

// countingWrites counts every Create, Update and Delete in *writes.
func countingWrites(writes *int) interceptor.Funcs {
	return interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			*writes++
			return c.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			*writes++
			return c.Update(ctx, obj, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			*writes++
			return c.Delete(ctx, obj, opts...)
		},
	}
}

func TestEnsureRabbitMQ_Availability(t *testing.T) {
	type seedFunc func(*c5c3v1alpha1.ControlPlane) client.Object
	// pass builds a managed-messaging ControlPlane whose bus declares replicas
	// and a fake client holding it and every seed, with funcs intercepting its
	// requests. It returns the client, the ControlPlane and a func that runs
	// one ensureRabbitMQ pass with sizing.
	pass := func(
		t *testing.T, replicas int32, sizing *c5c3v1alpha1.MessagingSizingSpec, funcs interceptor.Funcs, seeds ...seedFunc,
	) (client.Client, *c5c3v1alpha1.ControlPlane, func() error) {
		t.Helper()
		s := infraTestScheme(t)
		cp := managedMessagingControlPlane()
		cp.Spec.Infrastructure.Messaging.Replicas = replicas
		objs := []client.Object{cp}
		for _, seed := range seeds {
			objs = append(objs, seed(cp))
		}
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).WithInterceptorFuncs(funcs).Build()
		r := &ControlPlaneReconciler{Client: c, Scheme: s}
		run := func() error {
			_, err := r.ensureRabbitMQ(context.Background(), c, cp, cp.Spec.Infrastructure.Messaging, sizing, cp.Namespace)
			return err
		}
		return c, cp, run
	}
	bus := func(t *testing.T, g Gomega, replicas int64, affinity interface{}) seedFunc {
		return func(cp *c5c3v1alpha1.ControlPlane) client.Object { return ownedBus(t, g, cp, replicas, affinity) }
	}
	budget := func(t *testing.T, g Gomega, maxUnavailable int32) seedFunc {
		return func(cp *c5c3v1alpha1.ControlPlane) client.Object { return controlledBudget(t, g, cp, maxUnavailable) }
	}
	disabled := messagingAvailability(availabilitySizing("", ptr.To(false), nil))

	affinityTests := []struct {
		name     string
		replicas int32
		sizing   *c5c3v1alpha1.MessagingSizingSpec
		want     map[string]interface{}
	}{
		{
			name: "three brokers get the required term", replicas: 3,
			sizing: messagingAvailability(c5c3v1alpha1.AvailabilitySizingSpec{}),
			want:   wantBusAffinity("openstack-rabbitmq", c5c3v1alpha1.AntiAffinityRequired),
		},
		{
			name: "Preferred gets the weighted term", replicas: 3,
			sizing: messagingAvailability(availabilitySizing(c5c3v1alpha1.AntiAffinityPreferred, nil, nil)),
			want:   wantBusAffinity("openstack-rabbitmq", c5c3v1alpha1.AntiAffinityPreferred),
		},
		{
			name: "a nil sizing at three brokers gets the required term", replicas: 3,
			want: wantBusAffinity("openstack-rabbitmq", c5c3v1alpha1.AntiAffinityRequired),
		},
		{
			name: "Off gets no affinity", replicas: 3,
			sizing: messagingAvailability(availabilitySizing(c5c3v1alpha1.AntiAffinityOff, nil, nil)),
		},
		{
			name: "a single broker gets no affinity", replicas: 1,
			sizing: messagingAvailability(availabilitySizing(c5c3v1alpha1.AntiAffinityRequired, nil, nil)),
		},
		{
			name: "a bus without a count of its own is judged on the sizing's 3", replicas: 0,
			sizing: messagingReplicas(3), want: wantBusAffinity("openstack-rabbitmq", c5c3v1alpha1.AntiAffinityRequired),
		},
		{name: "a bus's own count of 1 wins over the sizing's 3", replicas: 1, sizing: messagingReplicas(3)},
	}
	for _, tc := range affinityTests {
		t.Run("on create, "+tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			c, cp, run := pass(t, tc.replicas, tc.sizing, interceptor.Funcs{})
			g.Expect(run()).To(Succeed())
			affinity, found := busAffinity(g, getBus(g, c, cp))
			if tc.want == nil {
				g.Expect(found).To(BeFalse(), "spec.affinity must be absent, got %v", affinity)
				return
			}
			g.Expect(affinity).To(Equal(tc.want))
		})
	}

	t.Run("Off removes a live spec.affinity from an owned bus", func(t *testing.T) {
		g := NewGomegaWithT(t)
		off := messagingAvailability(availabilitySizing(c5c3v1alpha1.AntiAffinityOff, nil, nil))
		c, cp, run := pass(t, 3, off, interceptor.Funcs{},
			bus(t, g, 3, wantBusAffinity("openstack-rabbitmq", c5c3v1alpha1.AntiAffinityRequired)))
		g.Expect(run()).To(Succeed())
		_, found := busAffinity(g, getBus(g, c, cp))
		g.Expect(found).To(BeFalse())
	})

	t.Run("an owned bus without the term gets it", func(t *testing.T) {
		g := NewGomegaWithT(t)
		c, cp, run := pass(t, 3, nil, interceptor.Funcs{}, bus(t, g, 3, nil))
		g.Expect(run()).To(Succeed())
		affinity, _ := busAffinity(g, getBus(g, c, cp))
		g.Expect(affinity).To(Equal(wantBusAffinity("openstack-rabbitmq", c5c3v1alpha1.AntiAffinityRequired)))
	})

	t.Run("a live spec.affinity that is not a map fails the pass", func(t *testing.T) {
		g := NewGomegaWithT(t)
		_, _, run := pass(t, 3, nil, interceptor.Funcs{}, bus(t, g, 3, "hostname"))
		g.Expect(run()).To(MatchError(ContainSubstring(`reading RabbitmqCluster "openstack-rabbitmq"`)))
	})

	t.Run("a live spec.affinity that is not a map fails the pass when the tolerations differ too", func(t *testing.T) {
		g := NewGomegaWithT(t)
		sizing := &c5c3v1alpha1.MessagingSizingSpec{
			Tolerations: []corev1.Toleration{{Key: "dedicated", Operator: corev1.TolerationOpExists}},
		}
		c, cp, run := pass(t, 3, sizing, interceptor.Funcs{}, bus(t, g, 3, "hostname"))
		g.Expect(run()).To(MatchError(ContainSubstring(`reading RabbitmqCluster "openstack-rabbitmq"`)))
		affinity, _, err := unstructured.NestedString(getBus(g, c, cp).Object, "spec", "affinity")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(affinity).To(Equal("hostname"), "the bus must not be rewritten")
	})

	for _, tc := range []struct {
		name   string
		sizing *c5c3v1alpha1.MessagingSizingSpec
	}{
		{name: "an empty block", sizing: messagingAvailability(c5c3v1alpha1.AvailabilitySizingSpec{})},
		{name: "a nil sizing"},
	} {
		t.Run("a bus created at three brokers with "+tc.name+" gets a controlled budget of 1", func(t *testing.T) {
			g := NewGomegaWithT(t)
			c, cp, run := pass(t, 3, tc.sizing, interceptor.Funcs{})
			g.Expect(run()).To(Succeed())
			pdb := getBudget(g, c, cp)
			g.Expect(pdb).NotTo(BeNil(), "the bus budget must be created beside the bus")
			g.Expect(metav1.IsControlledBy(pdb, cp)).To(BeTrue())
			g.Expect(pdb.Spec.MaxUnavailable).To(Equal(ptr.To(intstr.FromInt32(1))))
			g.Expect(pdb.Spec.MinAvailable).To(BeNil())
			g.Expect(pdb.Spec.Selector).To(Equal(&metav1.LabelSelector{
				MatchLabels: map[string]string{"app.kubernetes.io/name": "openstack-rabbitmq"},
			}))
		})
	}

	t.Run("maxUnavailable 2 updates the controlled budget in place", func(t *testing.T) {
		g := NewGomegaWithT(t)
		c, cp, run := pass(t, 3, messagingAvailability(availabilitySizing("", nil, ptr.To[int32](2))),
			interceptor.Funcs{}, bus(t, g, 3, nil), budget(t, g, 1))
		g.Expect(run()).To(Succeed())
		pdb := getBudget(g, c, cp)
		g.Expect(pdb.UID).To(Equal(types.UID("pdb-uid")), "the budget is updated, not recreated")
		g.Expect(pdb.Spec.MaxUnavailable).To(Equal(ptr.To(intstr.FromInt32(2))))
	})

	t.Run("a controlled budget with minAvailable gets maxUnavailable alone", func(t *testing.T) {
		g := NewGomegaWithT(t)
		withMin := func(cp *c5c3v1alpha1.ControlPlane) client.Object {
			pdb := controlledBudget(t, g, cp, 1)
			pdb.Spec.MaxUnavailable, pdb.Spec.MinAvailable = nil, ptr.To(intstr.FromString("50%"))
			return pdb
		}
		c, cp, run := pass(t, 3, nil, interceptor.Funcs{}, bus(t, g, 3, nil), withMin)
		g.Expect(run()).To(Succeed())
		pdb := getBudget(g, c, cp)
		g.Expect(pdb.Spec.MinAvailable).To(BeNil())
		g.Expect(pdb.Spec.MaxUnavailable).To(Equal(ptr.To(intstr.FromInt32(1))))
	})

	t.Run("a controlled budget whose selector drifted is restored in place", func(t *testing.T) {
		g := NewGomegaWithT(t)
		drifted := func(cp *c5c3v1alpha1.ControlPlane) client.Object {
			pdb := controlledBudget(t, g, cp, 1)
			pdb.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "other"}}
			return pdb
		}
		c, cp, run := pass(t, 3, nil, interceptor.Funcs{}, bus(t, g, 3, nil), drifted)
		g.Expect(run()).To(Succeed())
		pdb := getBudget(g, c, cp)
		g.Expect(pdb.UID).To(Equal(types.UID("pdb-uid")), "the budget is updated, not recreated")
		g.Expect(pdb.Spec.Selector).To(Equal(&metav1.LabelSelector{
			MatchLabels: map[string]string{"app.kubernetes.io/name": "openstack-rabbitmq"},
		}))
		g.Expect(pdb.Spec.MaxUnavailable).To(Equal(ptr.To(intstr.FromInt32(1))))
	})

	t.Run("a bus without a count of its own gets a budget on the sizing's 3", func(t *testing.T) {
		g := NewGomegaWithT(t)
		c, cp, run := pass(t, 0, messagingReplicas(3), interceptor.Funcs{})
		g.Expect(run()).To(Succeed())
		pdb := getBudget(g, c, cp)
		g.Expect(pdb).NotTo(BeNil(), "the bus runs the sizing's 3 brokers, so it gets a budget")
		g.Expect(pdb.Spec.MaxUnavailable).To(Equal(ptr.To(intstr.FromInt32(1))))
	})

	t.Run("a bus's own count of 1 gets no budget beside the sizing's 3", func(t *testing.T) {
		g := NewGomegaWithT(t)
		c, cp, run := pass(t, 1, messagingReplicas(3), interceptor.Funcs{})
		g.Expect(run()).To(Succeed())
		g.Expect(getBudget(g, c, cp)).To(BeNil())
	})

	for _, tc := range []struct {
		name     string
		replicas int32
		sizing   *c5c3v1alpha1.MessagingSizingSpec
	}{
		{name: "enabled: false", replicas: 3, sizing: disabled},
		{name: "a bus at one broker", replicas: 1},
	} {
		t.Run(tc.name+" deletes the controlled budget", func(t *testing.T) {
			g := NewGomegaWithT(t)
			c, cp, run := pass(t, tc.replicas, tc.sizing, interceptor.Funcs{},
				bus(t, g, int64(tc.replicas), nil), budget(t, g, 1))
			g.Expect(run()).To(Succeed())
			g.Expect(getBudget(g, c, cp)).To(BeNil())
		})

		t.Run(tc.name+" without a budget writes none", func(t *testing.T) {
			g := NewGomegaWithT(t)
			c, cp, run := pass(t, tc.replicas, tc.sizing, interceptor.Funcs{}, bus(t, g, int64(tc.replicas), nil))
			g.Expect(run()).To(Succeed())
			g.Expect(getBudget(g, c, cp)).To(BeNil())
		})
	}

	t.Run("a budget of that name the ControlPlane does not control is left unchanged", func(t *testing.T) {
		g := NewGomegaWithT(t)
		foreign := func(cp *c5c3v1alpha1.ControlPlane) client.Object {
			return &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{Name: "openstack-rabbitmq", Namespace: cp.Namespace},
				Spec:       policyv1.PodDisruptionBudgetSpec{MinAvailable: ptr.To(intstr.FromInt32(2))},
			}
		}
		c, cp, run := pass(t, 3, nil, interceptor.Funcs{}, bus(t, g, 3, nil), foreign)
		before := getBudget(g, c, cp)
		g.Expect(run()).To(Succeed())
		g.Expect(getBudget(g, c, cp)).To(Equal(before))

		// Disabled, it is not deleted either.
		c, cp, run = pass(t, 3, disabled, interceptor.Funcs{}, bus(t, g, 3, nil), foreign)
		g.Expect(run()).To(Succeed())
		g.Expect(getBudget(g, c, cp)).NotTo(BeNil())
	})

	t.Run("an adopted bus gets no budget", func(t *testing.T) {
		g := NewGomegaWithT(t)
		adopted := func(cp *c5c3v1alpha1.ControlPlane) client.Object {
			u := rabbitmqWithConditions("openstack-rabbitmq", cp.Namespace, nil)
			g.Expect(unstructured.SetNestedField(u.Object, int64(3), "spec", "replicas")).To(Succeed())
			return u
		}
		c, cp, run := pass(t, 3, nil, interceptor.Funcs{}, adopted)
		g.Expect(run()).To(Succeed())
		g.Expect(getBudget(g, c, cp)).To(BeNil())
	})

	t.Run("the refused shrink leaves the budget unchanged", func(t *testing.T) {
		g := NewGomegaWithT(t)
		c, cp, run := pass(t, 3, nil, interceptor.Funcs{}, bus(t, g, 5, nil), budget(t, g, 2))
		g.Expect(run()).To(MatchError(ContainSubstring(messagingRecreateAllowedAnnotation)))
		g.Expect(getBudget(g, c, cp).Spec.MaxUnavailable).To(Equal(ptr.To(intstr.FromInt32(2))))
	})

	for _, tc := range []struct {
		name       string
		replicas   int32
		wantBudget *intstr.IntOrString
	}{
		{
			name:     "an authorised recreate at three brokers keeps the budget and updates it for the new bus",
			replicas: 3, wantBudget: ptr.To(intstr.FromInt32(1)),
		},
		{
			name:     "an authorised recreate at one broker keeps the budget until the new bus, then deletes it",
			replicas: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			c, cp, run := pass(t, tc.replicas, nil, interceptor.Funcs{}, bus(t, g, 5, nil), budget(t, g, 2))
			cp.Annotations = map[string]string{messagingRecreateAllowedAnnotation: "true"}

			// The first pass deletes the bus and leaves the budget alone.
			g.Expect(run()).To(Succeed())
			u := &unstructured.Unstructured{}
			u.SetGroupVersionKind(messaging.RabbitmqClusterGVK)
			g.Expect(apierrors.IsNotFound(c.Get(context.Background(), types.NamespacedName{
				Name: "openstack-rabbitmq", Namespace: cp.Namespace,
			}, u))).To(BeTrue(), "the first pass deletes the bus")
			pdb := getBudget(g, c, cp)
			g.Expect(pdb).NotTo(BeNil(), "the budget outlives the deleted bus")
			g.Expect(pdb.UID).To(Equal(types.UID("pdb-uid")))
			g.Expect(pdb.Spec.MaxUnavailable).To(Equal(ptr.To(intstr.FromInt32(2))))

			// The second pass creates the bus at the declared count and keeps the
			// budget in line with it.
			g.Expect(run()).To(Succeed())
			replicas, _, err := unstructured.NestedInt64(getBus(g, c, cp).Object, "spec", "replicas")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(replicas).To(Equal(int64(tc.replicas)))
			pdb = getBudget(g, c, cp)
			if tc.wantBudget == nil {
				g.Expect(pdb).To(BeNil(), "a bus of one broker gets no budget")
				return
			}
			g.Expect(pdb).NotTo(BeNil())
			g.Expect(pdb.UID).To(Equal(types.UID("pdb-uid")), "the budget is updated, not recreated")
			g.Expect(pdb.Spec.MaxUnavailable).To(Equal(tc.wantBudget))
		})
	}

	t.Run("a terminating bus leaves the budget unchanged", func(t *testing.T) {
		g := NewGomegaWithT(t)
		terminating := func(cp *c5c3v1alpha1.ControlPlane) client.Object {
			u := ownedBus(t, g, cp, 3, nil)
			u.SetFinalizers([]string{"deletion.finalizers.rabbitmqclusters.rabbitmq.com"})
			u.SetDeletionTimestamp(ptr.To(metav1.Now()))
			return u
		}
		c, cp, run := pass(t, 3, nil, interceptor.Funcs{}, terminating, budget(t, g, 2))
		g.Expect(run()).To(Succeed())
		pdb := getBudget(g, c, cp)
		g.Expect(pdb).NotTo(BeNil())
		g.Expect(pdb.Spec.MaxUnavailable).To(Equal(ptr.To(intstr.FromInt32(2))), "a terminating bus leaves its budget alone")
	})

	t.Run("a second pass writes nothing", func(t *testing.T) {
		g := NewGomegaWithT(t)
		writes := 0
		_, _, run := pass(t, 3, nil, countingWrites(&writes))
		g.Expect(run()).To(Succeed())
		g.Expect(writes).To(Equal(2), "the first pass creates the bus and its budget")
		g.Expect(run()).To(Succeed())
		g.Expect(writes).To(Equal(2), "a settled bus and budget are not written again")
	})

	sentinel := errors.New("boom")
	for _, tc := range []struct {
		verb     string
		replicas int32
		sizing   *c5c3v1alpha1.MessagingSizingSpec
		seeds    func(t *testing.T, g Gomega) []seedFunc
		want     string
	}{
		{verb: "get", replicas: 3, want: `getting PodDisruptionBudget "openstack-rabbitmq": boom`},
		{verb: "create", replicas: 3, want: `creating PodDisruptionBudget "openstack-rabbitmq": boom`},
		{
			verb: "update", replicas: 3, sizing: messagingAvailability(availabilitySizing("", nil, ptr.To[int32](2))),
			seeds: func(t *testing.T, g Gomega) []seedFunc { return []seedFunc{bus(t, g, 3, nil), budget(t, g, 1)} },
			want:  `updating PodDisruptionBudget "openstack-rabbitmq": boom`,
		},
		{
			verb: "delete", replicas: 3, sizing: disabled,
			seeds: func(t *testing.T, g Gomega) []seedFunc { return []seedFunc{bus(t, g, 3, nil), budget(t, g, 1)} },
			want:  `deleting PodDisruptionBudget "openstack-rabbitmq": boom`,
		},
	} {
		t.Run("a failing "+tc.verb+" of the budget is returned wrapped", func(t *testing.T) {
			g := NewGomegaWithT(t)
			var seeds []seedFunc
			if tc.seeds != nil {
				seeds = tc.seeds(t, g)
			}
			_, _, run := pass(t, tc.replicas, tc.sizing, failingBudget(tc.verb, sentinel), seeds...)
			err := run()
			g.Expect(err).To(MatchError(tc.want))
			g.Expect(errors.Is(err, sentinel)).To(BeTrue())
		})
	}

	t.Run("a failing create of the budget surfaces as RabbitMQError", func(t *testing.T) {
		g := NewGomegaWithT(t)
		s := infraTestScheme(t)
		cp := managedMessagingControlPlane()
		cp.Spec.Infrastructure.Messaging.Replicas = 3
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).
			WithInterceptorFuncs(failingBudget("create", sentinel)).Build()
		r := &ControlPlaneReconciler{Client: c, Scheme: s}

		_, err := r.reconcileInfrastructure(context.Background(), cp)
		g.Expect(errors.Is(err, sentinel)).To(BeTrue(), "the failure must reach the caller for backoff")
		cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeInfrastructureReady)
		g.Expect(cond).NotTo(BeNil())
		g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(cond.Reason).To(Equal("RabbitMQError"))
		g.Expect(cond.Message).To(ContainSubstring(`creating PodDisruptionBudget "openstack-rabbitmq": boom`))
	})
}
