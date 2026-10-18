// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"errors"
	"testing"

	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/runtime/schema"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"

	"github.com/c5c3/cobaltcore/internal/common/deployment"
	mctestutil "github.com/c5c3/cobaltcore/internal/common/testutil/multicluster"
)

// TestSetupWithManager_FailsWhenAProbeGetsNoAnswer runs each controller's setup
// against an API server that answers every optional-kind probe but one. The
// operator has to fail its start on that probe's error instead of running
// without the watch for a kind the cluster may serve.
func TestSetupWithManager_FailsWhenAProbeGetsNoAnswer(t *testing.T) {
	controllers := []struct {
		name   string
		setup  func(mcmanager.Manager) error
		probed []schema.GroupVersionKind
	}{
		{
			name:   "OVNCentral",
			setup:  func(mgr mcmanager.Manager) error { return (&OVNCentralReconciler{}).SetupWithManager(mgr) },
			probed: []schema.GroupVersionKind{certificateGVK, deployment.VPAGVK},
		},
		{
			name:   "OVNChassis",
			setup:  func(mgr mcmanager.Manager) error { return (&OVNChassisReconciler{}).SetupWithManager(mgr) },
			probed: []schema.GroupVersionKind{deployment.VPAGVK},
		},
	}
	for _, c := range controllers {
		for _, probed := range c.probed {
			t.Run(c.name+"/"+probed.Kind, func(t *testing.T) {
				t.Parallel()
				g := NewGomegaWithT(t)
				noAnswer := errors.New("connection refused")
				mgr := mctestutil.UnprobeableManager(t, newTestScheme(t), probed.GroupKind(), noAnswer)

				g.Expect(c.setup(mgr)).To(MatchError(noAnswer))
			})
		}
	}
}
