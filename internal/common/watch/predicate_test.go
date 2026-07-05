// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"testing"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

// TestCRUpdatePredicate verifies the For(...) watch predicate filters the
// controller's own status-only updates while admitting spec, label, and
// annotation changes, as well as the live→Terminating deletionTimestamp
// transition that would otherwise stall finalizer cleanup.
func TestCRUpdatePredicate(t *testing.T) {
	p := CRUpdatePredicate()

	base := func(gen int64, labels, annotations map[string]string) *corev1.ConfigMap {
		return &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "cr",
				Namespace:   "default",
				Generation:  gen,
				Labels:      labels,
				Annotations: annotations,
			},
		}
	}

	// terminating clones base(...) and stamps deletionTimestamp so the update
	// looks like a CR that has entered Terminating.
	terminating := func(gen int64, labels, annotations map[string]string) *corev1.ConfigMap {
		cr := base(gen, labels, annotations)
		ts := metav1.Now()
		cr.DeletionTimestamp = &ts
		return cr
	}

	cases := []struct {
		name  string
		old   *corev1.ConfigMap
		new   *corev1.ConfigMap
		admit bool
	}{
		{
			name:  "status-only update filtered",
			old:   base(3, map[string]string{"a": "b"}, map[string]string{"x": "y"}),
			new:   base(3, map[string]string{"a": "b"}, map[string]string{"x": "y"}),
			admit: false,
		},
		{
			name:  "generation change admitted",
			old:   base(3, nil, nil),
			new:   base(4, nil, nil),
			admit: true,
		},
		{
			name:  "label change admitted",
			old:   base(3, map[string]string{"a": "b"}, nil),
			new:   base(3, map[string]string{"a": "c"}, nil),
			admit: true,
		},
		{
			name:  "annotation change admitted",
			old:   base(3, nil, map[string]string{"x": "y"}),
			new:   base(3, nil, map[string]string{"x": "z"}),
			admit: true,
		},
		{
			// kubectl delete sets deletionTimestamp — a metadata-only
			// mutation that does not bump generation on a status-subresource
			// CRD — so this transition MUST be admitted or finalizer cleanup
			// stalls until the next resync.
			name:  "live to terminating admitted",
			old:   base(3, nil, nil),
			new:   terminating(3, nil, nil),
			admit: true,
		},
		{
			// Once Terminating, a status-only update (same generation, still
			// deleting) is filtered like any other status write; the deletion
			// reconcile drives itself via requeue.
			name:  "already-terminating status update filtered",
			old:   terminating(3, nil, nil),
			new:   terminating(3, nil, nil),
			admit: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			admitted := p.Update(event.UpdateEvent{ObjectOld: tc.old, ObjectNew: tc.new})
			g.Expect(admitted).To(gomega.Equal(tc.admit))
		})
	}
}
