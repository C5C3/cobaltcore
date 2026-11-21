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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

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
