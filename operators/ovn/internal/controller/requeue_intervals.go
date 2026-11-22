// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import "time"

// RequeueRaftWait is the interval every OVN wait state polls at: a Raft cluster
// that has not elected a leader yet, a per-pod endpoint whose Service or Pod
// carries no address yet, and a maintenance Job that is still running. None of
// these states reaches the workqueue as an error, so this interval is what paces
// the retries. It is short enough to follow a bootstrap that takes a few
// seconds per member and long enough not to poll a wedged cluster into API
// churn.
const RequeueRaftWait = 15 * time.Second

// RequeueRouteProbe is how often a fabric chassis runs its route probe Job,
// and the pace of every pass of a fabric chassis that has nothing else to wait
// for. The probe opens two connections to the Southbound database, so an
// interval of minutes keeps it a background check; the same interval bounds
// how long a drifted FRRConfiguration, which no watch reports, stays drifted.
const RequeueRouteProbe = 5 * time.Minute
