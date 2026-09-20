// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// conditionReasonTransportErrorRetryFailed is the reason a call site reports when
// unlatchKORCTransportErrors returns an error, for example a Forbidden status
// patch because the chart's RBAC predates the status-subresource grant. The latch
// itself stays in place, so the underlying transport failure is still described by
// the K-ORC condition the call site relays.
//
// It sits here rather than in the External-mode reason block of external_mode.go:
// Managed-mode paths report it too (reconcileCatalog, ensureAccount,
// ensureCatalog), so it is not part of that block's External-only contract.
const conditionReasonTransportErrorRetryFailed = "TransportErrorRetryFailed"

// korcTransportUnlatchedAtAnnotation records, in RFC 3339, when this operator
// last cleared a latched transport error from the child it is stamped on. It is
// the only clock korcTransportUnlatchBackoff is measured against.
//
// The condition's own LastTransitionTime cannot serve, because it is stamped by
// whoever wrote the condition — K-ORC in production, kubectl in the e2e leg — so
// pacing this operator's writes by it compares one pod's wall clock against
// another's. A K-ORC clock half a minute ahead would hold every latch forever with
// no diagnostic, and a LastTransitionTime that survives a re-latch (K-ORC carries
// the previous one over whenever the condition is otherwise unchanged) would make
// every following pass clear again, bounded only by the controller's rate limiter.
const korcTransportUnlatchedAtAnnotation = "c5c3.io/korc-transport-unlatched-at"

// korcTransportLatched returns obj's generation-current terminal Progressing
// condition when its reason is InvalidConfiguration and its message classifies
// as a transport failure, and nil otherwise.
//
// An UnrecoverableError latch is never transport-latched, and neither is a
// message classifyKORCMessage reads as TLS ("x509") or authentication ("401",
// "Unauthorized") even when it also names a dial error. That is the safe
// direction: retrying repairs neither a missing trust anchor nor a stale
// password, so the latch stays and keeps reporting what a human has to fix.
func korcTransportLatched(obj orcv1alpha1.ObjectWithConditions) *metav1.Condition {
	if korcTerminalReason(obj) != orcv1alpha1.ConditionReasonInvalidConfiguration {
		return nil
	}
	cond := apimeta.FindStatusCondition(obj.GetConditions(), orcv1alpha1.ConditionProgressing)
	if cond == nil || classifyKORCMessage(cond.Message) != conditionReasonEndpointUnreachable {
		return nil
	}
	return cond
}

// korcUnlatchDue reports whether co's latch may be cleared now. The first latch
// this operator sees on a child is handed back straight away — K-ORC has already
// given up on it — and every further clear waits out korcTransportUnlatchBackoff
// plus the child's own korcUnlatchOffset, so the children of one plane, which
// latch inside the same window, are not all handed back in one synchronized
// burst.
//
// A stamp that does not parse counts as due, and so does one dated in the future:
// only this operator writes the annotation and each clear rewrites it, so the
// reading self-heals, while a latch nothing ever clears is the worse failure. A
// future stamp is never one this pass wrote — a node whose NTP has not converged
// after boot, a hypervisor clock step, or anything else holding patch on the
// K-ORC kind writing one by hand — and honouring it would hold the latch until
// wall-clock time catches up with no diagnostic anywhere, which is the exact
// failure this annotation was introduced to avoid.
func korcUnlatchDue(co client.Object) bool {
	stamped, ok := co.GetAnnotations()[korcTransportUnlatchedAtAnnotation]
	if !ok {
		return true
	}
	last, err := time.Parse(time.RFC3339, stamped)
	if err != nil || last.After(time.Now()) {
		return true
	}
	return time.Since(last) >= korcTransportUnlatchBackoff+korcUnlatchOffset(co)
}

// korcUnlatchOffset is the child's own share of the jitter span, spread over
// [0, korcTransportUnlatchJitter × korcTransportUnlatchBackoff).
//
// It is DERIVED FROM THE UID rather than drawn per evaluation. korcUnlatchDue
// runs once per reconcile pass, so a fresh draw is simply re-rolled every
// korcRequeueAfter until one falls below the elapsed time — by the first pass
// past the span's upper bound every draw is below it, and the whole plane clears
// together anyway. Only an offset that holds still keeps each child in its own
// place in the spread.
func korcUnlatchOffset(co client.Object) time.Duration {
	sum := sha256.Sum256([]byte(co.GetUID()))
	frac := float64(binary.BigEndian.Uint32(sum[:4])) / (1 << 32)
	return time.Duration(frac * korcTransportUnlatchJitter * float64(korcTransportUnlatchBackoff))
}

// isNilKORCObject reports whether obj is nil or a typed nil pointer. Every K-ORC
// kind is addressed as a pointer, and a (*orcv1alpha1.Project)(nil) held in an
// interface is NOT nil as an interface: it satisfies the assertion and then
// dereferences a nil receiver in GetConditions. Several call sites build their
// slice from struct fields that may be absent, so the guard is not theoretical.
func isNilKORCObject(obj client.Object) bool {
	if obj == nil {
		return true
	}
	v := reflect.ValueOf(obj)
	return v.Kind() == reflect.Ptr && v.IsNil()
}

// korcUnlatchError wraps every error unlatchKORCTransportErrors returns, so a
// call site that relays it through another helper can still report it as
// conditionReasonTransportErrorRetryFailed rather than as its own generic reason.
// ensureKeystoneServiceRoles is the one such relay: applyAccountRole owns the
// role children but no condition, and hands its error up to ensureAccount.
type korcUnlatchError struct{ err error }

func (e *korcUnlatchError) Error() string { return e.err.Error() }
func (e *korcUnlatchError) Unwrap() error { return e.err }

// isKORCUnlatchError reports whether err came from unlatchKORCTransportErrors,
// through any number of wrappings.
func isKORCUnlatchError(err error) bool {
	var unlatch *korcUnlatchError
	return errors.As(err, &unlatch)
}

// unlatchKORCTransportErrors hands every obj that korcTransportLatched reports
// back to K-ORC, by removing the latched Progressing condition from its status at
// most once per korcTransportUnlatchBackoff per child.
//
// POLICY, stated once here because all seven call sites share it: a latch this
// pass does not clear — one still inside the backoff, and every terminal error
// that is not a transport failure — is left untouched for the caller's own
// terminal-error and availability reporting to describe. An error returned here is
// the caller's to report as conditionReasonTransportErrorRetryFailed, and means a
// latch the operator cannot clear at all (a Forbidden from RBAC that predates the
// status-subresource grant). An apiserver error the next pass can retry from is
// NOT returned: this repair is opportunistic, and failing the sub-reconciler on it
// would overwrite the K-ORC failure the caller relays with an apiserver symptom,
// exactly when both systems are degraded.
//
// WHY the condition is REMOVED, and why nothing is deleted: K-ORC's
// ShouldReconcile skips every object whose Progressing condition is False at the
// current generation, across operator restarts and K-ORC upgrades, and
// reconciles the object again once the condition is absent. Removing it hands
// the child back to K-ORC's own idempotent create/adopt flow. Deleting and
// recreating the child instead would be destructive: K-ORC's delete path on a
// managed child that has no status id looks the resource up by name and deletes
// what it finds, which for an adopted account is the pre-existing Keystone user.
//
// K-ORC writes its status with Server-Side Apply and force ownership, so this
// merge patch cannot cost it a field-manager conflict on its next write.
func unlatchKORCTransportErrors(
	ctx context.Context, c client.Client, objs ...orcv1alpha1.ObjectWithConditions,
) error {
	for _, obj := range objs {
		co, ok := obj.(client.Object)
		if !ok || isNilKORCObject(co) {
			continue
		}
		cond := korcTransportLatched(obj)
		if cond == nil || !korcUnlatchDue(co) {
			continue
		}
		// Copied out because the clear's own response overwrites the condition
		// slice cond points into.
		message := cond.Message

		conds := obj.GetConditions()
		apimeta.RemoveStatusCondition(&conds, orcv1alpha1.ConditionProgressing)

		// The CLEAR goes FIRST and the stamp below records that it happened, so the
		// backoff paces the repair rather than the diagnostic. Stamping ahead of the
		// clear would pace BOTH: a clear the apiserver denies outright is reported
		// by the call site as conditionReasonTransportErrorRetryFailed and then, on
		// the very next pass, silenced by the stamp its own failure had already
		// written — the reason naming the denial would sit on the CR for one
		// rate-limiter tick per backoff, which no scrape, watch or alert can catch.
		// The reverse order costs an extra clear on every pass whose stamp behind a
		// clear that succeeded is the write that fails, which is why that stamp is
		// retried inline below rather than left to the next pass.
		//
		// The resourceVersion carries optimistic locking, so a status K-ORC rewrote
		// between the caller's read and here is rejected rather than overwritten. A
		// marshal failure of a []metav1.Condition is not reachable; it is wrapped
		// and returned rather than swallowed.
		body, err := json.Marshal(map[string]any{
			"metadata": map[string]any{"resourceVersion": co.GetResourceVersion()},
			"status":   map[string]any{"conditions": conds},
		})
		if err == nil {
			err = c.Status().Patch(ctx, co, client.RawPatch(types.MergePatchType, body))
		}
		skip, err := korcUnlatchOutcome(ctx, co, err, "clearing the latched transport error on")
		if err != nil {
			return err
		}
		if skip {
			continue
		}

		log.FromContext(ctx).Info("cleared a latched K-ORC transport error so K-ORC retries",
			"kind", fmt.Sprintf("%T", co), "name", co.GetName(), "namespace", co.GetNamespace(),
			"message", message)

		// Only now is there something to pace: the stamp is what korcUnlatchDue
		// measures the NEXT clear of this child against. A stamp that never lands
		// leaves the child UNPACED — the next pass reads no annotation, finds the
		// re-latched child due again, and clears it again — so the one retry per
		// child per korcTransportUnlatchBackoff this file exists to enforce
		// collapses to one per korcRequeueAfter, against a Keystone that may be
		// failing precisely because it is overloaded. So it is retried inline,
		// rather than left to a pass that has lost the only record of this one.
		//
		// It is NOT classified like the clear above, because the repair itself has
		// already happened: the latch is gone and K-ORC is retrying. Reporting the
		// stamp as conditionReasonTransportErrorRetryFailed would tell the CR that
		// the retry failed on a child whose retry succeeded, and returning it would
		// abandon every object behind this one, which needs the same repair. A raw
		// transport error reaches exactly that on the most ordinary failure there
		// is — an apiserver restart or an LB blip dropping the connection carries
		// no APIStatus, so korcUnlatchOutcome's retryable branch cannot recognize
		// it. A stamp this operator cannot write is loud as a log line instead.
		stampErr := retry.OnError(retry.DefaultBackoff, func(error) bool { return true }, func() error {
			stamp, err := json.Marshal(map[string]any{"metadata": map[string]any{
				"annotations": map[string]string{
					korcTransportUnlatchedAtAnnotation: time.Now().UTC().Format(time.RFC3339),
				},
			}})
			if err != nil {
				return err
			}
			return c.Patch(ctx, co, client.RawPatch(types.MergePatchType, stamp))
		})
		if stampErr != nil {
			log.FromContext(ctx).Error(stampErr, "could not pace a K-ORC transport unlatch; this child "+
				"is handed back to K-ORC again on the next pass",
				"kind", fmt.Sprintf("%T", co), "name", co.GetName(), "namespace", co.GetNamespace())
		}
	}
	return nil
}

// korcUnlatchOutcome classifies the result of one write in the unlatch. skip is
// true when the pass gives this child up and carries on with the next one: the
// child is gone, K-ORC wrote its status concurrently, or the apiserver answered
// with something the next pass can retry from. The latch survives all three, and
// the caller's own terminal-error reporting keeps describing it.
//
// Everything else is wrapped with what failed on which child and returned, so a
// latch the operator can never clear — a Forbidden on <kind>/status from a chart
// whose RBAC predates the grant — is loud rather than silently permanent.
func korcUnlatchOutcome(ctx context.Context, co client.Object, err error, what string) (bool, error) {
	switch {
	case err == nil:
		return false, nil
	case apierrors.IsNotFound(err), apierrors.IsConflict(err):
		return true, nil
	case apierrors.IsTooManyRequests(err), apierrors.IsServerTimeout(err), apierrors.IsTimeout(err),
		apierrors.IsInternalError(err), apierrors.IsServiceUnavailable(err):
		// Default verbosity, not V(1): the operator binaries run zap with
		// Development=false, which pins the logger to InfoLevel and drops V(1)
		// entirely. A webhook with failurePolicy: Fail that is down answers every
		// unlatch write with an internal error, so this branch can switch the
		// transport self-healing off cluster-wide — with no condition and no event,
		// this line is then the only record that it happened.
		log.FromContext(ctx).Info("deferring a K-ORC transport unlatch to the next pass",
			"kind", fmt.Sprintf("%T", co), "name", co.GetName(), "namespace", co.GetNamespace(),
			"error", err)
		return true, nil
	default:
		return false, &korcUnlatchError{fmt.Errorf("%s %T %s/%s: %w",
			what, co, co.GetNamespace(), co.GetName(), err)}
	}
}
