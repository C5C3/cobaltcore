// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"encoding/json"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
)

// sizingLeaves flattens s into its JSON leaves, keyed by slash-separated path,
// so a test can assert which values a profile sets without walking every type.
func sizingLeaves(t *testing.T, s SizingSpec) map[string]any {
	t.Helper()
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal sizing: %v", err)
	}
	var tree map[string]any
	if err := json.Unmarshal(raw, &tree); err != nil {
		t.Fatalf("unmarshal sizing: %v", err)
	}
	leaves := map[string]any{}
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		m, ok := v.(map[string]any)
		if !ok {
			leaves[prefix] = v
			return
		}
		for k, child := range m {
			p := k
			if prefix != "" {
				p = prefix + "/" + k
			}
			walk(p, child)
		}
	}
	walk("", tree)
	return leaves
}

func TestBuiltinSizing_StandardMatchesTodaysConstants(t *testing.T) {
	g := NewWithT(t)
	three := float64(commonv1.DefaultReplicas)
	one := float64(novav1alpha1.DefaultComponentReplicas)

	// Standard sets replica counts and the database volume size, and nothing
	// else: no resources, processes, threads, workers, placement, spread or
	// autoscaling, so every other value stays with the child operators.
	g.Expect(sizingLeaves(t, BuiltinSizing(SizingProfileStandard))).To(Equal(map[string]any{
		"database/replicas":          three,
		"database/storageSize":       commonv1.DatabaseStorageSizeDefault,
		"cache/replicas":             three,
		"messaging/replicas":         three,
		"keystone/api/replicas":      three,
		"horizon/api/replicas":       three,
		"glance/api/replicas":        three,
		"placement/api/replicas":     three,
		"barbican/api/replicas":      three,
		"neutron/api/replicas":       three,
		"neutron/workers/replicas":   three,
		"cinder/api/replicas":        three,
		"cinder/scheduler/replicas":  float64(1),
		"nova/api/replicas":          three,
		"nova/metadata/replicas":     one,
		"nova/scheduler/replicas":    one,
		"nova/conductor/replicas":    one,
		"nova/consoleProxy/replicas": one,
	}))
	g.Expect(commonv1.DatabaseStorageSizeDefault).To(Equal("100Gi"))
}

func TestBuiltinSizing_MinimalCarriesTable(t *testing.T) {
	g := NewWithT(t)
	cpu50 := "50m"
	leaves := sizingLeaves(t, BuiltinSizing(SizingProfileMinimal))

	for _, svc := range []string{"keystone", "glance", "placement", "barbican", "neutron", "cinder", "nova"} {
		g.Expect(leaves).To(HaveKeyWithValue(svc+"/api/replicas", float64(1)), svc)
		g.Expect(leaves).To(HaveKeyWithValue(svc+"/api/processes", float64(1)), svc)
		g.Expect(leaves).To(HaveKeyWithValue(svc+"/api/threads", float64(1)), svc)
		g.Expect(leaves).To(HaveKeyWithValue(svc+"/api/resources/requests/cpu", cpu50), svc)
		g.Expect(leaves).To(HaveKeyWithValue(svc+"/jobs/resources/requests/cpu", cpu50), svc)
	}
	g.Expect(leaves).To(HaveKeyWithValue("horizon/api/replicas", float64(1)))
	g.Expect(leaves).To(HaveKeyWithValue("horizon/api/resources/requests/cpu", cpu50))
	g.Expect(leaves).NotTo(HaveKey("horizon/api/processes"))
	g.Expect(leaves).To(HaveKeyWithValue("neutron/workers/replicas", float64(1)))
	g.Expect(leaves).To(HaveKeyWithValue("neutron/workers/resources/requests/cpu", cpu50))
	g.Expect(leaves).To(HaveKeyWithValue("cinder/scheduler/replicas", float64(1)))
	g.Expect(leaves).To(HaveKeyWithValue("cinder/scheduler/resources/requests/cpu", cpu50))
	g.Expect(leaves).To(HaveKeyWithValue("cinder/volume/resources/requests/cpu", cpu50))
	g.Expect(leaves).To(HaveKeyWithValue("cinder/backup/resources/requests/cpu", cpu50))
	g.Expect(leaves).To(HaveKeyWithValue("nova/metadata/replicas", float64(1)))
	g.Expect(leaves).To(HaveKeyWithValue("nova/metadata/processes", float64(1)))
	g.Expect(leaves).To(HaveKeyWithValue("nova/metadata/threads", float64(1)))
	g.Expect(leaves).To(HaveKeyWithValue("nova/metadata/resources/requests/cpu", cpu50))
	for _, c := range []string{"scheduler", "conductor"} {
		g.Expect(leaves).To(HaveKeyWithValue("nova/"+c+"/replicas", float64(1)), c)
		g.Expect(leaves).To(HaveKeyWithValue("nova/"+c+"/workers", float64(1)), c)
		g.Expect(leaves).To(HaveKeyWithValue("nova/"+c+"/resources/requests/cpu", cpu50), c)
	}
	g.Expect(leaves).To(HaveKeyWithValue("nova/consoleProxy/replicas", float64(1)))
	g.Expect(leaves).To(HaveKeyWithValue("nova/consoleProxy/resources/requests/cpu", cpu50))

	g.Expect(leaves).To(HaveKeyWithValue("database/replicas", float64(1)))
	g.Expect(leaves).To(HaveKeyWithValue("database/storageSize", "512Mi"))
	g.Expect(leaves).To(HaveKeyWithValue("database/resources/requests/cpu", "100m"))
	g.Expect(leaves).To(HaveKeyWithValue("database/resources/requests/memory", "1Gi"))
	g.Expect(leaves).To(HaveKeyWithValue("database/resources/limits/memory", "1Gi"))
	g.Expect(leaves).To(HaveKeyWithValue("cache/replicas", float64(1)))
	g.Expect(leaves).To(HaveKeyWithValue("cache/resources/requests/cpu", cpu50))
	g.Expect(leaves).To(HaveKeyWithValue("cache/resources/requests/memory", "128Mi"))
	g.Expect(leaves).To(HaveKeyWithValue("cache/resources/limits/memory", "128Mi"))
	g.Expect(leaves).To(HaveKeyWithValue("messaging/replicas", float64(1)))
	g.Expect(leaves).To(HaveKeyWithValue("messaging/resources/requests/cpu", "100m"))
	g.Expect(leaves).To(HaveKeyWithValue("messaging/resources/requests/memory", "512Mi"))
	g.Expect(leaves).To(HaveKeyWithValue("messaging/resources/limits/memory", "512Mi"))
	g.Expect(leaves).To(HaveKeyWithValue("secretStore/resources/requests/cpu", cpu50))
	g.Expect(leaves).To(HaveKeyWithValue("secretStore/resources/requests/memory", "256Mi"))
	g.Expect(leaves).To(HaveKeyWithValue("secretStore/resources/limits/memory", "256Mi"))

	// Neither profile sets placement, spread, autoscaling or a priority class,
	// and the federation proxy keeps the Keystone operator's own default.
	for key := range leaves {
		g.Expect(key).NotTo(ContainSubstring("nodeSelector"))
		g.Expect(key).NotTo(ContainSubstring("tolerations"))
		g.Expect(key).NotTo(ContainSubstring("priorityClassName"))
		g.Expect(key).NotTo(ContainSubstring("spreadConstraints"))
		g.Expect(key).NotTo(ContainSubstring("autoscaling"))
		g.Expect(key).NotTo(ContainSubstring("federationProxy"))
	}
}

func TestBuiltinSizing_ReturnsFreshCopy(t *testing.T) {
	g := NewWithT(t)
	for _, name := range []SizingProfileName{SizingProfileMinimal, SizingProfileStandard} {
		first := BuiltinSizing(name)
		*first.Keystone.API.Replicas = 42
		first.Database.StorageSize = "1Ti"
		first.NodeSelector = map[string]string{"a": "b"}
		if first.Cache.Resources != nil {
			first.Cache.Resources.Limits[corev1.ResourceMemory] = resource.MustParse("9Gi")
		}

		next := BuiltinSizing(name)
		g.Expect(next.Keystone.API.Replicas).NotTo(Equal(ptr.To[int32](42)), string(name))
		g.Expect(next.Database.StorageSize).NotTo(Equal("1Ti"), string(name))
		g.Expect(next.NodeSelector).To(BeNil(), string(name))
		if next.Cache.Resources != nil {
			g.Expect(next.Cache.Resources.Limits.Memory().String()).To(Equal("128Mi"), string(name))
		}
	}
}

func TestBuiltinSizing_UnknownIsStandard(t *testing.T) {
	g := NewWithT(t)
	standard := BuiltinSizing(SizingProfileStandard)
	g.Expect(BuiltinSizing("")).To(Equal(standard))
	g.Expect(BuiltinSizing("Large")).To(Equal(standard))
	g.Expect(BuiltinSizing(SizingProfileMinimal)).NotTo(Equal(standard))
}
