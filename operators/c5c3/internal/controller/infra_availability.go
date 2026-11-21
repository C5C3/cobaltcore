// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/v26/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// mariaDBInstanceLabel is the pod label the mariadb-operator sets to the
// MariaDB's name, and the key of the term it builds for antiAffinityEnabled
// (v26.10.2, api/v1alpha1/base_types.go, AffinityConfig.SetDefaults).
const mariaDBInstanceLabel = "app.kubernetes.io/instance"

// backingAntiAffinityWeight is the weight of a preferred hostname term.
const backingAntiAffinityWeight int32 = 100

// backingAvailability is the anti-affinity and disruption budget one backing
// service instance resolves to (resolveBackingAvailability).
type backingAvailability struct {
	// antiAffinity is Required, Preferred or Off, never "".
	antiAffinity c5c3v1alpha1.AntiAffinityMode
	// maxUnavailable is the budget's count, or nil for no budget.
	maxUnavailable *int32
	// multiReplica is true when the instance runs more than one replica.
	multiReplica bool
}

// resolveBackingAvailability resolves a, the availability sizing of a backing
// service class, for one instance of replicas pods. A single replica (or a
// count below one) gets no anti-affinity and no budget, whatever a holds.
// Several replicas get a's mode when it is one of the three values and
// defaultAntiAffinity otherwise, which covers the unset mode and a value
// that bypassed admission. They get no budget when a disables it, else a's
// maxUnavailable when it is 1 or more, else 1. The returned count never
// aliases a.
func resolveBackingAvailability(
	a c5c3v1alpha1.AvailabilitySizingSpec, replicas int32, defaultAntiAffinity c5c3v1alpha1.AntiAffinityMode,
) backingAvailability {
	if replicas <= 1 {
		return backingAvailability{antiAffinity: c5c3v1alpha1.AntiAffinityOff}
	}
	av := backingAvailability{antiAffinity: defaultAntiAffinity, maxUnavailable: ptr.To[int32](1), multiReplica: true}
	switch a.AntiAffinity {
	case c5c3v1alpha1.AntiAffinityRequired, c5c3v1alpha1.AntiAffinityPreferred, c5c3v1alpha1.AntiAffinityOff:
		av.antiAffinity = a.AntiAffinity
	}
	if b := a.PodDisruptionBudget; b != nil {
		switch {
		case b.Enabled != nil && !*b.Enabled:
			av.maxUnavailable = nil
		case b.MaxUnavailable != nil && *b.MaxUnavailable >= 1:
			av.maxUnavailable = ptr.To(*b.MaxUnavailable)
		}
	}
	return av
}

// applyMariaDBAvailability writes av onto m. spec.affinity carries one
// hostname term on the pods of m, required or preferred with weight 100, the
// term the mariadb-operator builds for antiAffinityEnabled, or is removed for
// Off. Every other key of a live spec.affinity, nodeAffinity included, is
// replaced, except antiAffinityEnabled: the mariadb-operator webhook refuses
// to change it while spec.affinity is set before and after an update, so a
// live value is kept and none is ever set.
//
// spec.podDisruptionBudget is removed on a single instance. On a Galera
// cluster it carries av's count, or maxUnavailable "100%" without a budget:
// with the field unset the mariadb-operator gives every Galera cluster
// minAvailable 50%. The mariadb-operator creates the PodDisruptionBudget
// object from the field once and never updates it, so a changed value reaches
// an existing database only after that object is deleted.
func applyMariaDBAvailability(m *mariadbv1alpha1.MariaDB, av backingAvailability) {
	var antiAffinityEnabled *bool
	if m.Spec.Affinity != nil {
		antiAffinityEnabled = m.Spec.Affinity.AntiAffinityEnabled
	}
	term := mariadbv1alpha1.PodAffinityTerm{
		LabelSelector: &mariadbv1alpha1.LabelSelector{
			MatchExpressions: []mariadbv1alpha1.LabelSelectorRequirement{{
				Key:      mariaDBInstanceLabel,
				Operator: metav1.LabelSelectorOpIn,
				Values:   []string{m.Name},
			}},
		},
		TopologyKey: corev1.LabelHostname,
	}
	var podAntiAffinity *mariadbv1alpha1.PodAntiAffinity
	switch av.antiAffinity {
	case c5c3v1alpha1.AntiAffinityRequired:
		podAntiAffinity = &mariadbv1alpha1.PodAntiAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []mariadbv1alpha1.PodAffinityTerm{term},
		}
	case c5c3v1alpha1.AntiAffinityPreferred:
		podAntiAffinity = &mariadbv1alpha1.PodAntiAffinity{
			PreferredDuringSchedulingIgnoredDuringExecution: []mariadbv1alpha1.WeightedPodAffinityTerm{
				{Weight: backingAntiAffinityWeight, PodAffinityTerm: term},
			},
		}
	case c5c3v1alpha1.AntiAffinityOff:
	}
	m.Spec.Affinity = nil
	if podAntiAffinity != nil {
		m.Spec.Affinity = &mariadbv1alpha1.AffinityConfig{
			Affinity:            mariadbv1alpha1.Affinity{PodAntiAffinity: podAntiAffinity},
			AntiAffinityEnabled: antiAffinityEnabled,
		}
	}

	switch {
	case !av.multiReplica:
		m.Spec.PodDisruptionBudget = nil
	case av.maxUnavailable != nil:
		m.Spec.PodDisruptionBudget = &mariadbv1alpha1.PodDisruptionBudget{
			MaxUnavailable: ptr.To(intstr.FromInt32(*av.maxUnavailable)),
		}
	default:
		m.Spec.PodDisruptionBudget = &mariadbv1alpha1.PodDisruptionBudget{
			MaxUnavailable: ptr.To(intstr.FromString("100%")),
		}
	}
}

// mariaDBAvailabilityMatches reports whether m already carries what
// applyMariaDBAvailability writes for av.
func mariaDBAvailabilityMatches(m *mariadbv1alpha1.MariaDB, av backingAvailability) bool {
	want := m.DeepCopy()
	applyMariaDBAvailability(want, av)
	return equality.Semantic.DeepEqual(m.Spec.Affinity, want.Spec.Affinity) &&
		equality.Semantic.DeepEqual(m.Spec.PodDisruptionBudget, want.Spec.PodDisruptionBudget)
}
