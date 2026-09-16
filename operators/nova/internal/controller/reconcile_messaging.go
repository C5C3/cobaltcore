// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"net/url"
	"strings"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/messaging"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
)

// reconcileTransportURLSecret materialises the rabbit:// transport URL into the
// derived <nova.Name>-transport-url Secret and reports the three values the
// later steps need: the URL itself, its SHA-256 digest, which the deployment
// steps stamp into a pod-template annotation so a rotated broker credential
// rolls the pods, and the broker's TCP port, which the networkpolicy step opens
// as an egress peer.
//
// The URL is returned rather than kept internal because it is part of the
// compute contract: a nova-compute runs outside this cluster and reaches the
// control plane over the bus alone, so the compute-config Secret has to carry
// the same URL the control-plane pods use.
//
// The bus carries every instance request past the API: the API hands it to the
// conductor, the conductor asks the scheduler for a host, and every compute node
// takes its work the same way, so all five control-plane workloads and the
// migration Jobs read this one Secret.
//
// The shared flow reads the RabbitmqCluster (managed mode) or the brownfield
// Secret and writes the derived Secret through the children client, in the CR's
// own namespace. A placed Nova whose bus lives on the management cluster
// therefore has to use spec.messaging.secretRef: the helper is same-namespace,
// same-client by design, and the CRD field documents it.
//
// The flow runs checkCellTransportURL before it writes the derived Secret: every
// workload reads the URL from that Secret on start, so a URL refused only after
// the write would still reach the next pod that restarts. A refused URL reports
// SecretsReady=False with reason TransportURLRejected and polls.
//
// The egress port is derived from the transport URL the flow returns. The URL,
// the digest and the port are all zero on the waiting and the refused path, where
// no derived Secret was materialised.
func (r *NovaReconciler) reconcileTransportURLSecret(ctx context.Context, children client.Client,
	nova *novav1alpha1.Nova,
) (ctrl.Result, string, string, int32, error) {
	result, transportURL, digest, err := messaging.ReconcileTransportURLSecret(ctx, messaging.TransportURLSecretFlowParams{
		Client:        children,
		Scheme:        r.Scheme,
		Owner:         nova,
		InstanceName:  nova.Name,
		Namespace:     nova.Namespace,
		Messaging:     &nova.Spec.Messaging,
		Conditions:    &nova.Status.Conditions,
		Generation:    nova.Generation,
		ConditionType: "SecretsReady",
		RequeueAfter:  commonreconcile.RequeueSecretPolling,
		CheckURL:      checkCellTransportURL,
	})
	if err != nil || digest == "" {
		return result, transportURL, digest, 0, err
	}

	return result, transportURL, digest, messaging.EgressPort(transportURL), nil
}

// checkCellTransportURL refuses a transport URL the cell mapping cannot be
// expanded from. The db-sync Job maps cell1 with cellTransportURLTemplate, and
// nova fills its {hostname}:{port} from urlparse of this URL: a URL without an
// explicit port fills the literal "None", and an IPv6 literal loses the brackets
// it needs, so every cast to the cell would fail on a URL oslo.messaging cannot
// parse. A managed URL always names a host name and a port; a brownfield Secret
// may not. A multi-host URL passes: urlparse splits its netloc at the last "@"
// and the last ":", so the template puts the host list back together as long as
// the last host names its port.
//
// The error never quotes the URL: it carries the broker password.
func checkCellTransportURL(transportURL string) error {
	u, err := url.Parse(transportURL)
	if err != nil {
		return errors.New("transport URL does not parse")
	}
	if u.Port() == "" || strings.Contains(u.Host, "[") {
		return errors.New("transport URL must name an explicit port and must not use an IPv6 literal host: " +
			"nova expands the cell mapping's {hostname}:{port} template from it, which renders a missing port as " +
			"\"None\" and drops the brackets of an IPv6 literal")
	}
	return nil
}
