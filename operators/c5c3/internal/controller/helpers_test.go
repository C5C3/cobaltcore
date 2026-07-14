// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	commonv1 "github.com/c5c3/forge/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
)

func TestIntervalToCron(t *testing.T) {
	tests := []struct {
		name     string
		interval time.Duration
		want     string
		wantErr  bool
	}{
		{
			name:     "168h maps to weekly Sunday midnight",
			interval: 168 * time.Hour,
			want:     "0 0 * * 0",
		},
		{
			name:     "24h maps to daily midnight",
			interval: 24 * time.Hour,
			want:     "0 0 * * *",
		},
		{
			name:     "multiple of 24h maps to daily midnight",
			interval: 72 * time.Hour,
			want:     "0 0 * * *",
		},
		{
			name:     "unsupported interval returns an error",
			interval: 5 * time.Hour,
			wantErr:  true,
		},
		{
			name:     "zero interval returns an error",
			interval: 0,
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := intervalToCron(tt.interval)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("intervalToCron(%v) = %q, want error", tt.interval, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("intervalToCron(%v) returned unexpected error: %v", tt.interval, err)
			}
			if got != tt.want {
				t.Errorf("intervalToCron(%v) = %q, want %q", tt.interval, got, tt.want)
			}
		})
	}
}

func TestIntervalToCronErrorNamesUnsupportedValue(t *testing.T) {
	const interval = 5 * time.Hour
	_, err := intervalToCron(interval)
	if err == nil {
		t.Fatalf("intervalToCron(%v) = nil error, want error naming the value", interval)
	}
	if !strings.Contains(err.Error(), interval.String()) {
		t.Errorf("error %q does not name unsupported value %q", err.Error(), interval.String())
	}
}

// TestEffectiveBackingServices pins the shared-by-default / dedicated-on-request
// resolution every consumer of a backing service routes through: a service that
// opted in resolves to ITS instance, one that did not resolves to the
// ControlPlane-wide one, and an unresolvable instance (External mode, or a
// webhook-bypassed CR with no infrastructure block) resolves to nil so callers
// fail closed instead of dereferencing it.
func TestEffectiveBackingServices(t *testing.T) {
	sharedInfra := func() *c5c3v1alpha1.InfrastructureSpec {
		return &c5c3v1alpha1.InfrastructureSpec{
			Database: commonv1.DatabaseSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: "openstack-db"},
				Database:   "keystone",
			},
			Cache: commonv1.CacheSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: "openstack-memcached"},
				Backend:    commonv1.DefaultCacheBackend,
			},
		}
	}

	tests := []struct {
		name              string
		cp                *c5c3v1alpha1.ControlPlane
		wantKeystoneDB    string // "" = expect nil
		wantKeystoneCache string
		wantHorizonCache  string
	}{
		{
			name: "no dedicated blocks: every service shares the ControlPlane-wide instances",
			cp: &c5c3v1alpha1.ControlPlane{Spec: c5c3v1alpha1.ControlPlaneSpec{
				Infrastructure: sharedInfra(),
				Services: c5c3v1alpha1.ServicesSpec{
					Keystone: &c5c3v1alpha1.ServiceKeystoneSpec{},
					Horizon:  &c5c3v1alpha1.ServiceHorizonSpec{},
				},
			}},
			wantKeystoneDB:    "openstack-db",
			wantKeystoneCache: "openstack-memcached",
			wantHorizonCache:  "openstack-memcached",
		},
		{
			name: "keystone takes a dedicated database only: its cache stays shared",
			cp: &c5c3v1alpha1.ControlPlane{Spec: c5c3v1alpha1.ControlPlaneSpec{
				Infrastructure: sharedInfra(),
				Services: c5c3v1alpha1.ServicesSpec{
					Keystone: &c5c3v1alpha1.ServiceKeystoneSpec{
						DedicatedBackingServices: &c5c3v1alpha1.KeystoneDedicatedBackingServicesSpec{
							Database: &commonv1.DatabaseSpec{
								ClusterRef: &corev1.LocalObjectReference{Name: "cp-keystone-db"},
								Database:   "keystone",
							},
						},
					},
					Horizon: &c5c3v1alpha1.ServiceHorizonSpec{},
				},
			}},
			wantKeystoneDB:    "cp-keystone-db",
			wantKeystoneCache: "openstack-memcached",
			wantHorizonCache:  "openstack-memcached",
		},
		{
			name: "each service takes its own dedicated cache",
			cp: &c5c3v1alpha1.ControlPlane{Spec: c5c3v1alpha1.ControlPlaneSpec{
				Infrastructure: sharedInfra(),
				Services: c5c3v1alpha1.ServicesSpec{
					Keystone: &c5c3v1alpha1.ServiceKeystoneSpec{
						DedicatedBackingServices: &c5c3v1alpha1.KeystoneDedicatedBackingServicesSpec{
							Cache: &commonv1.CacheSpec{
								ClusterRef: &corev1.LocalObjectReference{Name: "cp-keystone-cache"},
								Backend:    commonv1.DefaultCacheBackend,
							},
						},
					},
					Horizon: &c5c3v1alpha1.ServiceHorizonSpec{
						DedicatedBackingServices: &c5c3v1alpha1.HorizonDedicatedBackingServicesSpec{
							Cache: &commonv1.CacheSpec{
								ClusterRef: &corev1.LocalObjectReference{Name: "cp-horizon-cache"},
								Backend:    commonv1.DefaultCacheBackend,
							},
						},
					},
				},
			}},
			wantKeystoneDB:    "openstack-db",
			wantKeystoneCache: "cp-keystone-cache",
			wantHorizonCache:  "cp-horizon-cache",
		},
		{
			name: "no infrastructure block and no dedicated instances: nothing resolves",
			cp: &c5c3v1alpha1.ControlPlane{Spec: c5c3v1alpha1.ControlPlaneSpec{
				Services: c5c3v1alpha1.ServicesSpec{Keystone: &c5c3v1alpha1.ServiceKeystoneSpec{}},
			}},
		},
	}

	clusterRefName := func(ref *corev1.LocalObjectReference) string {
		if ref == nil {
			return ""
		}
		return ref.Name
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotDB string
			if db := effectiveKeystoneDatabase(tc.cp); db != nil {
				gotDB = clusterRefName(db.ClusterRef)
			}
			if gotDB != tc.wantKeystoneDB {
				t.Errorf("effectiveKeystoneDatabase() = %q, want %q", gotDB, tc.wantKeystoneDB)
			}

			var gotKSCache string
			if cache := effectiveKeystoneCache(tc.cp); cache != nil {
				gotKSCache = clusterRefName(cache.ClusterRef)
			}
			if gotKSCache != tc.wantKeystoneCache {
				t.Errorf("effectiveKeystoneCache() = %q, want %q", gotKSCache, tc.wantKeystoneCache)
			}

			var gotHZCache string
			if cache := effectiveHorizonCache(tc.cp); cache != nil {
				gotHZCache = clusterRefName(cache.ClusterRef)
			}
			if gotHZCache != tc.wantHorizonCache {
				t.Errorf("effectiveHorizonCache() = %q, want %q", gotHZCache, tc.wantHorizonCache)
			}
		})
	}
}
