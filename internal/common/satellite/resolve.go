// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package satellite

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	"github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

// ResolveParams is one satellite's parent lookup: which parent to read, which
// of the satellite's own conditions records a failure, and what the failure
// says.
type ResolveParams struct {
	// Client reads the parent on the management cluster, where every satellite
	// and its parent live.
	Client client.Client
	// Resolver looks up the target cluster the parent names. A nil resolver
	// (the operator runs without a multicluster manager) keeps everything local.
	Resolver multicluster.ClusterResolver
	// Parent is an empty typed parent object the Get fills.
	Parent client.Object
	// ParentKey names the parent, usually the satellite's namespace and the name
	// its parent ref carries.
	ParentKey client.ObjectKey
	// ParentKind is the parent's kind as it appears in the wrapped error text,
	// for example "Glance".
	ParentKind string
	// TargetClusterRef reads the ref off Parent once the Get has filled it.
	TargetClusterRef func() *commonv1.TargetClusterRefSpec

	// Conditions is the satellite's own condition slice, written in place.
	Conditions *[]metav1.Condition
	// Generation is the satellite's generation, stamped as ObservedGeneration.
	Generation int64
	// GateConditionType is the satellite's first gate condition, which records
	// both the missing parent and the unresolvable target, for example
	// "CredentialsReady".
	GateConditionType string
	// WaitingForParentReason is the reason for the missing-parent gate.
	WaitingForParentReason string
	// WaitingForParentMessage is the message for it, formatted by the caller so
	// it carries the parent key.
	WaitingForParentMessage string

	// ProjectionConditionType is the condition demoted when the parent is gone
	// and it still claims a projection. Empty never demotes anything.
	ProjectionConditionType string
	// ProjectionDemotionReason is the reason of that demotion.
	ProjectionDemotionReason string
	// ProjectionDemotionMessage is its message.
	ProjectionDemotionMessage string

	// RequeueAfter is how long a held pass waits before trying again.
	RequeueAfter time.Duration
}

// ResolveParentChildren returns the client the satellite's observations read
// from. A nil children client means the caller returns result and err as they
// are.
//
// A parent that does not exist holds the pass on the gate condition rather than
// falling back to the local client. It is the parent's targetClusterRef that
// decides which cluster the satellite's credentials and observations belong on,
// so guessing local would read or write the wrong cluster for the whole window
// between a GitOps apply landing the satellite and the same apply landing its
// parent. An unresolvable target gets the same treatment under the shared
// multicluster.TargetClusterUnavailable reason.
//
// The projection demotion is opt-in because the two callers disagree, and
// parameterizing the difference is cheaper than changing either. One keeps a
// standing projection claim honest once the parent that carried it is gone; the
// other leaves the claim alone. An absent or already False projection condition
// is never touched, so a satellite that has not converged yet keeps the
// condition absent.
func ResolveParentChildren(ctx context.Context, p ResolveParams) (children client.Client, result ctrl.Result, err error) {
	if err := p.Client.Get(ctx, p.ParentKey, p.Parent); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, ctrl.Result{}, fmt.Errorf("fetching parent %s %s: %w", p.ParentKind, p.ParentKey, err)
		}
		p.setFalse(p.GateConditionType, p.WaitingForParentReason, p.WaitingForParentMessage)
		if p.ProjectionConditionType != "" {
			projected := conditions.GetCondition(*p.Conditions, p.ProjectionConditionType)
			if projected != nil && projected.Status == metav1.ConditionTrue {
				p.setFalse(p.ProjectionConditionType, p.ProjectionDemotionReason, p.ProjectionDemotionMessage)
			}
		}
		return nil, ctrl.Result{RequeueAfter: p.RequeueAfter}, nil
	}

	children, err = multicluster.ResolveChildrenClient(ctx, p.Resolver, p.Client, p.TargetClusterRef())
	if err != nil {
		p.setFalse(p.GateConditionType, multicluster.TargetClusterUnavailable, err.Error())
		return nil, ctrl.Result{RequeueAfter: p.RequeueAfter}, nil
	}
	return children, ctrl.Result{}, nil
}

// setFalse stamps one of the satellite's own conditions False at the observed
// generation. Both conditions this file writes are the satellite controller's,
// which the condition-coverage audit does not scan, so the helper sets them
// instead of handing the decision back.
func (p ResolveParams) setFalse(conditionType, reason, message string) {
	conditions.SetCondition(p.Conditions, metav1.Condition{
		Type:               conditionType,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: p.Generation,
		Reason:             reason,
		Message:            message,
	})
}
