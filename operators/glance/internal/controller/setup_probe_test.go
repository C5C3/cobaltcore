// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"errors"
	"testing"

	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/c5c3/cobaltcore/internal/common/deployment"
	mctestutil "github.com/c5c3/cobaltcore/internal/common/testutil/multicluster"
)

// TestSetupWithManager_FailsWhenAProbeGetsNoAnswer runs the setup against an
// API server that answers every optional-kind probe but one. The operator has
// to fail its start on that probe's error instead of running without the watch
// for a kind the cluster may serve.
func TestSetupWithManager_FailsWhenAProbeGetsNoAnswer(t *testing.T) {
	for _, probed := range []schema.GroupVersionKind{httpRouteGVK, deployment.VPAGVK} {
		t.Run(probed.Kind, func(t *testing.T) {
			t.Parallel()
			g := NewGomegaWithT(t)
			noAnswer := errors.New("connection refused")
			mgr := mctestutil.UnprobeableManager(t, testScheme(), probed.GroupKind(), noAnswer)

			g.Expect((&GlanceReconciler{}).SetupWithManager(mgr)).To(MatchError(noAnswer))
		})
	}
}
