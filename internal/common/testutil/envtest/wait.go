// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package envtest

import (
	"context"
	"testing"
	"time"

	"github.com/onsi/gomega"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// WaitForObservedGeneration polls obj through c until observed() equals its
// metadata.generation, and fails t naming controller when timeout elapses
// first. Each poll refreshes obj in place, so observed reads the
// status.observedGeneration of the latest Get. A controller starts its workers
// only after every one of its sources synced, so the stamp proves the whole
// watch chain of that controller came up. No prerequisite object is needed for
// a controller whose first pass writes status even when a gate reports False.
//
// Against a manager from StartManagedEnvTest, a broken controller setup fails
// in one of three distinct ways. A RegisterController error (a probe without an
// answer, an index registered twice, a kind missing from the scheme) fails
// StartManagedEnvTest with "failed to register ... controller". A watched kind
// the API server does not serve keeps the controller's workers from starting,
// so this wait times out naming the controller; on a multicluster leg the
// manager also stops at once and StartManagedEnvTest reports "manager exited
// with error: no matches for kind ...". A webhook handler missing from
// RegisterWebhooks fails the fixture's Create with the admission error.
func WaitForObservedGeneration(t testing.TB, ctx context.Context, c client.Reader, obj client.Object,
	observed func() int64, controller string, timeout, interval time.Duration,
) {
	t.Helper()
	key := client.ObjectKeyFromObject(obj)
	gomega.NewWithT(t).Eventually(func(g gomega.Gomega) {
		g.Expect(c.Get(ctx, key, obj)).To(gomega.Succeed())
		g.Expect(observed()).To(gomega.Equal(obj.GetGeneration()))
	}, timeout, interval).Should(gomega.Succeed(),
		"the %s controller must stamp status.observedGeneration on %s", controller, key)
}
