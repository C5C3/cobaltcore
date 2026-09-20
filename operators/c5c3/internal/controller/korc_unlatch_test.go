// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the K-ORC transport-error unlatch helper unlatchKORCTransportErrors.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// --- fixtures ---

// unlatchTestClient seeds a fake client with objs and the interceptors a test
// needs.
//
// Every kind the helper writes a status on MUST be registered with
// WithStatusSubresource: the fake client answers a status write on an
// unregistered kind with NotFound, which the helper reads as "the child
// vanished" and skips. A test that drops the registration passes while clearing
// nothing.
func unlatchTestClient(t *testing.T, funcs interceptor.Funcs, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(korcTestScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&orcv1alpha1.Project{}, &orcv1alpha1.User{}).
		WithInterceptorFuncs(funcs).
		Build()
}

// unlatchTestProject builds a K-ORC Project carrying conds, the child kind the
// incident latched.
func unlatchTestProject(name string, conds []metav1.Condition) *orcv1alpha1.Project {
	return &orcv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Status:     orcv1alpha1.ProjectStatus{Conditions: conds},
	}
}

// unlatchedAt stamps the annotation the backoff is measured against, as an
// unlatch this operator performed age ago.
func unlatchedAt(age time.Duration) map[string]string {
	return map[string]string{
		korcTransportUnlatchedAtAnnotation: time.Now().Add(-age).UTC().Format(time.RFC3339),
	}
}

// unlatchLiveProject reads a seeded Project back, so the pointer the helper is
// given carries a live resourceVersion as it does after an apply in production.
func unlatchLiveProject(t *testing.T, c client.Client, name string) *orcv1alpha1.Project {
	t.Helper()
	g := NewGomegaWithT(t)
	p := &orcv1alpha1.Project{}
	g.Expect(c.Get(context.Background(), client.ObjectKey{Name: name, Namespace: "default"}, p)).To(Succeed())
	return p
}

// terminalConditions stamps a terminal Progressing=False carrying reason and msg.
func terminalConditions(reason, msg string) []metav1.Condition {
	return []metav1.Condition{{
		Type:               orcv1alpha1.ConditionProgressing,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            msg,
		LastTransitionTime: metav1.NewTime(time.Now().Add(-time.Minute)),
	}}
}

// conditionsJSON renders an object's status.conditions so a test can compare the
// stored bytes before and after a call.
func conditionsJSON(t *testing.T, obj orcv1alpha1.ObjectWithConditions) string {
	t.Helper()
	g := NewGomegaWithT(t)
	b, err := json.Marshal(obj.GetConditions())
	g.Expect(err).NotTo(HaveOccurred())
	return string(b)
}

// refusedMessage is the verbatim dial error K-ORC latched in the incident.
func refusedMessage() string { return transportLatchedConditions()[0].Message }

// --- tests ---

func TestUnlatchKORCTransportErrors_ClearsATransportLatch(t *testing.T) {
	g := NewGomegaWithT(t)

	conds := append(transportLatchedConditions(), metav1.Condition{
		Type:               orcv1alpha1.ConditionAvailable,
		Status:             metav1.ConditionFalse,
		Reason:             orcv1alpha1.ConditionReasonProgressing,
		Message:            "waiting for the OpenStack resource",
		LastTransitionTime: metav1.Now(),
	})
	c := unlatchTestClient(t, interceptor.Funcs{}, unlatchTestProject("p", conds))

	live := unlatchLiveProject(t, c, "p")
	g.Expect(orcv1alpha1.GetTerminalError(live)).To(HaveOccurred(),
		"the seed only proves anything while the latch is generation-current")

	g.Expect(unlatchKORCTransportErrors(context.Background(), c, live)).To(Succeed())

	after := unlatchLiveProject(t, c, "p")
	g.Expect(apimeta.FindStatusCondition(after.GetConditions(), orcv1alpha1.ConditionProgressing)).To(BeNil(),
		"the latch must be gone from the live object so K-ORC reconciles it again")
	g.Expect(apimeta.FindStatusCondition(after.GetConditions(), orcv1alpha1.ConditionAvailable)).NotTo(BeNil(),
		"every other condition must survive the patch")
	g.Expect(orcv1alpha1.GetTerminalError(live)).NotTo(HaveOccurred(),
		"the patch response must leave the caller's pointer unlatched too")

	stamped, err := time.Parse(time.RFC3339, after.Annotations[korcTransportUnlatchedAtAnnotation])
	g.Expect(err).NotTo(HaveOccurred(), "the clear must stamp when it happened, by this operator's clock")
	g.Expect(time.Since(stamped)).To(BeNumerically("<", time.Minute))
}

// The backoff is measured against the annotation this operator stamps, never
// against the condition's LastTransitionTime, which K-ORC (or, in the e2e leg,
// kubectl) writes from a different clock.
func TestUnlatchKORCTransportErrors_HonoursTheBackoff(t *testing.T) {
	rows := []struct {
		name string
		// unlatched is how long ago this operator last cleared the child.
		unlatched time.Duration
		wantClear bool
	}{
		{name: "a child cleared a moment ago is left alone", unlatched: 0},
		{
			name:      "a child cleared a second before the backoff is left alone",
			unlatched: korcTransportUnlatchBackoff - time.Second,
		},
		{
			// Well past the jittered upper bound, so the offset cannot decide the row.
			name:      "a child cleared long ago is due again",
			unlatched: 10 * time.Minute,
			wantClear: true,
		},
		{
			// A negative age is a stamp in the FUTURE, which this operator cannot
			// have written: a node whose NTP has not converged stamps every latched
			// child ahead of itself, and honouring that would hold each latch until
			// wall-clock time catches up — silently, for as long as the skew lasts.
			name:      "a child stamped in the future by a skewed clock is due now",
			unlatched: -45 * time.Minute,
			wantClear: true,
		},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			g := NewGomegaWithT(t)

			p := unlatchTestProject("p", transportLatchedConditions())
			p.Annotations = unlatchedAt(row.unlatched)
			c := unlatchTestClient(t, interceptor.Funcs{}, p)

			live := unlatchLiveProject(t, c, "p")
			g.Expect(orcv1alpha1.GetTerminalError(live)).To(HaveOccurred())
			before := conditionsJSON(t, live)

			g.Expect(unlatchKORCTransportErrors(context.Background(), c, live)).To(Succeed())

			after := unlatchLiveProject(t, c, "p")
			if row.wantClear {
				g.Expect(apimeta.FindStatusCondition(after.GetConditions(), orcv1alpha1.ConditionProgressing)).
					To(BeNil())
				return
			}
			g.Expect(conditionsJSON(t, after)).To(Equal(before),
				"a child cleared inside the backoff stays latched")
			g.Expect(after.Annotations).To(Equal(p.Annotations),
				"and its stamp is not refreshed, so the backoff cannot be extended forever")
		})
	}
}

// korcUnlatchJitterSpan is the window korcUnlatchOffset spreads children over.
func korcUnlatchJitterSpan() time.Duration {
	return time.Duration(korcTransportUnlatchJitter * float64(korcTransportUnlatchBackoff))
}

// A child's offset must hold still across evaluations. korcUnlatchDue is only
// ever called once per reconcile pass, so an offset drawn per evaluation is
// simply re-rolled every korcRequeueAfter until a draw falls below the elapsed
// time — which delays the burst rather than breaking it.
func TestKORCUnlatchDue_OffsetHoldsStillAcrossEvaluations(t *testing.T) {
	g := NewGomegaWithT(t)

	// Halfway into the jitter span, where a redrawn offset would answer
	// differently about every other call.
	p := unlatchTestProject("p", nil)
	p.UID = types.UID("9a7f1c3e-0b52-4d18-9f44-2c6b8e5d7a31")
	p.Annotations = unlatchedAt(korcTransportUnlatchBackoff + korcUnlatchJitterSpan()/2)

	first := korcUnlatchDue(p)
	for range 64 {
		g.Expect(korcUnlatchDue(p)).To(Equal(first),
			"the same child evaluated twice with the same stamp must answer the same")
	}
}

// The children of one plane latch inside the same window and are cleared in the
// same pass, so they all carry the same stamp. Their due times must therefore
// land in DIFFERENT passes; otherwise the plane is handed back to a Keystone
// that may be failing because it is overloaded in one burst of two API writes
// per child, issued synchronously inside a single reconcile.
func TestKORCUnlatchDue_SpreadsOnePlanesChildrenAcrossPasses(t *testing.T) {
	g := NewGomegaWithT(t)

	const children = 120
	// The pass a child first comes due on, counted from the one that first sees
	// korcTransportUnlatchBackoff elapsed.
	releasedOnPass := map[int]int{}

	for i := range children {
		p := unlatchTestProject(fmt.Sprintf("child-%d", i), nil)
		p.UID = types.UID(fmt.Sprintf("11111111-2222-3333-4444-%012d", i))

		due := false
		for pass := 0; pass < 32 && !due; pass++ {
			p.Annotations = unlatchedAt(korcTransportUnlatchBackoff + time.Duration(pass)*korcRequeueAfter)
			if due = korcUnlatchDue(p); due {
				releasedOnPass[pass]++
			}
		}
		g.Expect(due).To(BeTrue(), "every child must come due within a bounded number of passes")
	}

	g.Expect(len(releasedOnPass)).To(BeNumerically(">=", 3),
		"the span must be wide enough to reach several korcRequeueAfter passes, got %v", releasedOnPass)
	for pass, released := range releasedOnPass {
		g.Expect(released).To(BeNumerically("<", children/2),
			"pass %d released %d of %d children in one burst", pass, released, children)
	}
}

// A latch whose condition K-ORC transitioned only a moment ago is still cleared:
// K-ORC has already given up on the child, and the operator's own stamp — not
// K-ORC's clock — is what paces the retries.
func TestUnlatchKORCTransportErrors_ClearsAFreshLatchOnFirstSight(t *testing.T) {
	g := NewGomegaWithT(t)

	conds := transportLatchedConditions()
	conds[0].LastTransitionTime = metav1.Now()
	c := unlatchTestClient(t, interceptor.Funcs{}, unlatchTestProject("p", conds))

	g.Expect(unlatchKORCTransportErrors(context.Background(), c, unlatchLiveProject(t, c, "p"))).To(Succeed())
	g.Expect(apimeta.FindStatusCondition(unlatchLiveProject(t, c, "p").GetConditions(),
		orcv1alpha1.ConditionProgressing)).To(BeNil())
}

// Only a transport failure is cleared. Every other latch is a state the operator
// reports and never rewrites, so an unclassifiable message, a TLS or auth
// message that also mentions a dial error, K-ORC's other terminal reason, and a
// stale condition all leave the object byte-identical.
func TestUnlatchKORCTransportErrors_LeavesMisconfigurationLatched(t *testing.T) {
	rows := []struct {
		name string
		// generation is the live object's metadata.generation; the fake client
		// stores it verbatim.
		generation int64
		conds      []metav1.Condition
		// staleCondition marks the row whose condition observed an older
		// generation, which is not a terminal latch at all.
		staleCondition bool
	}{
		{
			name: "a 409 from Keystone is a configuration conflict, not a transport failure",
			conds: terminalConditions(orcv1alpha1.ConditionReasonInvalidConfiguration,
				"invalid configuration creating resource: Expected HTTP response code [201] "+
					"when accessing [POST http://keystone:5000/v3/projects], but got 409 instead"),
		},
		{
			name: "a dial error that also names x509 classifies as a TLS failure",
			conds: terminalConditions(orcv1alpha1.ConditionReasonInvalidConfiguration,
				`invalid configuration creating resource: Post "https://keystone.example.com:5000/v3/projects": `+
					`dial tcp 10.96.227.194:5000: tls: failed to verify certificate: `+
					`x509: certificate signed by unknown authority`),
		},
		{
			// The other half of the same rule: no retry rotates a stale password, so
			// an authentication failure must keep reporting what a human has to fix.
			name: "a dial error that also names 401 classifies as an authentication failure",
			conds: terminalConditions(orcv1alpha1.ConditionReasonInvalidConfiguration,
				`invalid configuration creating resource: Post "http://keystone:5000/v3/projects": `+
					`dial tcp 10.96.227.194:5000: got 401 Unauthorized`),
		},
		{
			name: "a dial error that also names Unauthorized classifies as an authentication failure",
			conds: terminalConditions(orcv1alpha1.ConditionReasonInvalidConfiguration,
				`invalid configuration creating resource: Post "http://keystone:5000/v3/projects": `+
					`dial tcp 10.96.227.194:5000: Unauthorized`),
		},
		{
			name:  "an UnrecoverableError is K-ORC's other terminal reason",
			conds: terminalConditions(orcv1alpha1.ConditionReasonUnrecoverableError, refusedMessage()),
		},
		{
			name:           "a condition observing an older generation is not a current latch",
			generation:     2,
			conds:          staleGenerationTransportConditions(1),
			staleCondition: true,
		},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			g := NewGomegaWithT(t)

			p := unlatchTestProject("p", row.conds)
			p.Generation = row.generation
			c := unlatchTestClient(t, interceptor.Funcs{}, p)

			live := unlatchLiveProject(t, c, "p")
			observed := apimeta.FindStatusCondition(live.GetConditions(), orcv1alpha1.ConditionProgressing)
			g.Expect(observed).NotTo(BeNil())
			if row.staleCondition {
				g.Expect(observed.ObservedGeneration).NotTo(Equal(live.GetGeneration()),
					"the row only proves anything while the condition is stale")
			} else {
				g.Expect(orcv1alpha1.GetTerminalError(live)).To(HaveOccurred(),
					"the row only proves anything while the latch is generation-current")
			}
			before := conditionsJSON(t, live)

			g.Expect(unlatchKORCTransportErrors(context.Background(), c, live)).To(Succeed())

			after := unlatchLiveProject(t, c, "p")
			g.Expect(conditionsJSON(t, after)).To(Equal(before))
			g.Expect(after.Annotations).NotTo(HaveKey(korcTransportUnlatchedAtAnnotation),
				"a latch the helper leaves alone must not be stamped either")
		})
	}
}

// staleGenerationTransportConditions is the incident's latch as K-ORC leaves it
// after the spec changed underneath: the message still classifies as a transport
// failure, but the condition describes a generation that no longer exists.
func staleGenerationTransportConditions(observed int64) []metav1.Condition {
	conds := transportLatchedConditions()
	conds[0].ObservedGeneration = observed
	return conds
}

func TestUnlatchKORCTransportErrors_NoObjects(t *testing.T) {
	g := NewGomegaWithT(t)

	calls := 0
	c := unlatchTestClient(t, interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey,
			obj client.Object, opts ...client.GetOption,
		) error {
			calls++
			return cl.Get(ctx, key, obj, opts...)
		},
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			calls++
			return cl.List(ctx, list, opts...)
		},
		Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object,
			patch client.Patch, opts ...client.PatchOption,
		) error {
			calls++
			return cl.Patch(ctx, obj, patch, opts...)
		},
		Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			calls++
			return cl.Update(ctx, obj, opts...)
		},
		SubResourcePatch: func(ctx context.Context, cl client.Client, subResourceName string,
			obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption,
		) error {
			calls++
			return cl.SubResource(subResourceName).Patch(ctx, obj, patch, opts...)
		},
		SubResourceUpdate: func(ctx context.Context, cl client.Client, subResourceName string,
			obj client.Object, opts ...client.SubResourceUpdateOption,
		) error {
			calls++
			return cl.SubResource(subResourceName).Update(ctx, obj, opts...)
		},
	})

	g.Expect(unlatchKORCTransportErrors(context.Background(), c)).To(Succeed())
	g.Expect(calls).To(Equal(0), "a call with nothing to clear must not touch the API server")
}

// A nil interface, a TYPED nil (the shape a caller building its slice from an
// absent struct field passes), and a child with no conditions at all are all
// skipped rather than dereferenced.
func TestUnlatchKORCTransportErrors_SkipsNilAndConditionless(t *testing.T) {
	g := NewGomegaWithT(t)

	var none orcv1alpha1.ObjectWithConditions
	var typedNil *orcv1alpha1.Project
	c := unlatchTestClient(t, interceptor.Funcs{}, unlatchTestProject("p", nil))
	live := unlatchLiveProject(t, c, "p")

	g.Expect(unlatchKORCTransportErrors(context.Background(), c, none, typedNil, live)).To(Succeed())
	g.Expect(unlatchLiveProject(t, c, "p").GetConditions()).To(BeEmpty())
}

// The remaining conditions must marshal as an empty JSON list: a merge patch
// carrying null under status.conditions leaves the stored list untouched, so the
// latch would survive a call that reports success. The same body carries the
// child's live resourceVersion, which is what makes the clear optimistic.
func TestUnlatchKORCTransportErrors_ClearBodyIsAListAndVersioned(t *testing.T) {
	g := NewGomegaWithT(t)

	var bodies, versions []string
	c := unlatchTestClient(t, interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, cl client.Client, subResourceName string,
			obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption,
		) error {
			data, err := patch.Data(obj)
			g.Expect(err).NotTo(HaveOccurred())
			bodies = append(bodies, string(data))
			// The version the body must carry is the one the object holds when the
			// patch is built, which is the one the caller read it at.
			versions = append(versions, obj.GetResourceVersion())
			return cl.SubResource(subResourceName).Patch(ctx, obj, patch, opts...)
		},
	}, unlatchTestProject("p", transportLatchedConditions()))

	live := unlatchLiveProject(t, c, "p")
	g.Expect(unlatchKORCTransportErrors(context.Background(), c, live)).To(Succeed())

	g.Expect(bodies).To(HaveLen(1))
	g.Expect(bodies[0]).To(ContainSubstring(`"conditions":[]`))
	g.Expect(bodies[0]).NotTo(ContainSubstring(`"conditions":null`))
	g.Expect(versions[0]).NotTo(BeEmpty())
	g.Expect(bodies[0]).To(ContainSubstring(`"resourceVersion":"`+versions[0]+`"`),
		"without the resourceVersion the clear would overwrite a status K-ORC rewrote in between")
	g.Expect(unlatchLiveProject(t, c, "p").GetConditions()).To(BeEmpty())
}

// The optimistic lock the body carries is the one the apiserver enforces: a
// status K-ORC rewrote between the stamp and the clear is rejected rather than
// overwritten with the condition list this pass read.
func TestUnlatchKORCTransportErrors_ConcurrentStatusWriteIsNotOverwritten(t *testing.T) {
	g := NewGomegaWithT(t)

	// K-ORC writes the child's status after the caller read it and before the
	// clear lands, which is the window the resourceVersion in the clear's body
	// closes.
	rewriteStatus := interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, cl client.Client, subResourceName string,
			obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption,
		) error {
			live := &orcv1alpha1.Project{}
			g.Expect(cl.Get(ctx, client.ObjectKeyFromObject(obj), live)).To(Succeed())
			live.Status.Conditions = append(live.Status.Conditions, metav1.Condition{
				Type:               orcv1alpha1.ConditionAvailable,
				Status:             metav1.ConditionFalse,
				Reason:             orcv1alpha1.ConditionReasonInvalidConfiguration,
				Message:            refusedMessage(),
				LastTransitionTime: metav1.Now(),
			})
			g.Expect(cl.Status().Update(ctx, live)).To(Succeed())
			return cl.SubResource(subResourceName).Patch(ctx, obj, patch, opts...)
		},
	}
	c := unlatchTestClient(t, rewriteStatus, unlatchTestProject("p", transportLatchedConditions()))

	live := unlatchLiveProject(t, c, "p")
	g.Expect(unlatchKORCTransportErrors(context.Background(), c, live)).
		To(Succeed(), "losing the race is not a failure; the next pass re-evaluates")

	after := unlatchLiveProject(t, c, "p")
	g.Expect(apimeta.FindStatusCondition(after.GetConditions(), orcv1alpha1.ConditionAvailable)).NotTo(BeNil(),
		"the condition K-ORC wrote in between must not be rolled back by the clear")
	g.Expect(apimeta.FindStatusCondition(after.GetConditions(), orcv1alpha1.ConditionProgressing)).NotTo(BeNil(),
		"and the latch it re-stamped must survive rather than be cleared from a stale read")
}

func TestUnlatchKORCTransportErrors_ToleratesNotFound(t *testing.T) {
	g := NewGomegaWithT(t)

	c := unlatchTestClient(t, interceptor.Funcs{
		SubResourcePatch: func(_ context.Context, _ client.Client, _ string,
			obj client.Object, _ client.Patch, _ ...client.SubResourcePatchOption,
		) error {
			return apierrors.NewNotFound(orcv1alpha1.Resource("projects"), obj.GetName())
		},
	}, unlatchTestProject("p", transportLatchedConditions()))

	live := unlatchLiveProject(t, c, "p")
	g.Expect(unlatchKORCTransportErrors(context.Background(), c, live)).
		To(Succeed(), "a child that is already gone is not a failure")
}

// A conflict means K-ORC wrote the status concurrently. The pass gives up on
// that child and continues, so the objects behind it are still processed.
func TestUnlatchKORCTransportErrors_ToleratesConflict(t *testing.T) {
	g := NewGomegaWithT(t)

	var patched []string
	c := unlatchTestClient(t, interceptor.Funcs{
		SubResourcePatch: func(_ context.Context, _ client.Client, _ string,
			obj client.Object, _ client.Patch, _ ...client.SubResourcePatchOption,
		) error {
			patched = append(patched, obj.GetName())
			return apierrors.NewConflict(orcv1alpha1.Resource("projects"), obj.GetName(),
				errors.New("object was modified"))
		},
	},
		unlatchTestProject("first", transportLatchedConditions()),
		unlatchTestProject("second", transportLatchedConditions()),
	)

	g.Expect(unlatchKORCTransportErrors(context.Background(), c,
		unlatchLiveProject(t, c, "first"), unlatchLiveProject(t, c, "second"))).To(Succeed())
	g.Expect(patched).To(Equal([]string{"first", "second"}),
		"a conflicting child must not stop the objects behind it")
	g.Expect(apimeta.FindStatusCondition(unlatchLiveProject(t, c, "first").GetConditions(),
		orcv1alpha1.ConditionProgressing)).NotTo(BeNil())
}

// An apiserver that is overloaded, unavailable or timing out is a failure the
// NEXT pass retries from. Reporting it would overwrite the K-ORC failure the
// caller relays with an apiserver symptom, exactly when both are degraded — so
// the latch simply survives and the objects behind it are still processed.
func TestUnlatchKORCTransportErrors_DefersRetryableAPIServerErrors(t *testing.T) {
	projects := orcv1alpha1.Resource("projects")
	rows := []struct {
		name string
		err  error
	}{
		{name: "too many requests", err: apierrors.NewTooManyRequests("slow down", 1)},
		{name: "service unavailable", err: apierrors.NewServiceUnavailable("apiserver is starting up")},
		{name: "server timeout", err: apierrors.NewServerTimeout(projects, "patch", 1)},
		{name: "request timeout", err: apierrors.NewTimeoutError("request timed out", 1)},
		{name: "etcd timeout", err: apierrors.NewInternalError(errors.New("etcdserver: request timed out"))},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			g := NewGomegaWithT(t)

			var patched []string
			c := unlatchTestClient(t, interceptor.Funcs{
				SubResourcePatch: func(_ context.Context, _ client.Client, _ string,
					obj client.Object, _ client.Patch, _ ...client.SubResourcePatchOption,
				) error {
					patched = append(patched, obj.GetName())
					return row.err
				},
			},
				unlatchTestProject("first", transportLatchedConditions()),
				unlatchTestProject("second", transportLatchedConditions()),
			)

			g.Expect(unlatchKORCTransportErrors(context.Background(), c,
				unlatchLiveProject(t, c, "first"), unlatchLiveProject(t, c, "second"))).
				To(Succeed(), "an opportunistic repair must not fail the caller's sub-reconciler")
			g.Expect(patched).To(Equal([]string{"first", "second"}),
				"a deferred child must not stop the objects behind it")
			g.Expect(apimeta.FindStatusCondition(unlatchLiveProject(t, c, "first").GetConditions(),
				orcv1alpha1.ConditionProgressing)).NotTo(BeNil(), "the latch survives for the next pass")
		})
	}
}

// Deferring is silent on the CR by design, so the log line is the only record
// that it happened — and it has to survive the production logging profile. The
// operator binaries build their logger from zap.Options{Development: false},
// which pins it to InfoLevel and drops V(1) entirely. A webhook on the K-ORC
// kinds with failurePolicy: Fail that is down answers every unlatch write with
// an internal error, which switches this repair off for every child at once.
func TestUnlatchKORCTransportErrors_LogsTheDeferralAtDefaultVerbosity(t *testing.T) {
	g := NewGomegaWithT(t)

	c := unlatchTestClient(t, interceptor.Funcs{
		SubResourcePatch: func(_ context.Context, _ client.Client, _ string,
			_ client.Object, _ client.Patch, _ ...client.SubResourcePatchOption,
		) error {
			return apierrors.NewInternalError(errors.New(
				`failed calling webhook "validate.openstack.k-orc.cloud": connection refused`))
		},
	}, unlatchTestProject("p", transportLatchedConditions()))

	// Verbosity 0 is the production profile: anything logged at V(1) or deeper is
	// dropped rather than captured here.
	var logs []string
	logger := funcr.New(func(prefix, args string) {
		logs = append(logs, prefix+" "+args)
	}, funcr.Options{Verbosity: 0})
	ctx := log.IntoContext(context.Background(), logger)

	g.Expect(unlatchKORCTransportErrors(ctx, c, unlatchLiveProject(t, c, "p"))).To(Succeed())

	combined := strings.Join(logs, "\n")
	g.Expect(combined).To(ContainSubstring("deferring a K-ORC transport unlatch to the next pass"),
		"a repair that defers on every child with nothing on the CR must at least be logged")
	g.Expect(combined).To(ContainSubstring(`"name"="p"`),
		"the line must name the child, so the deferral can be attributed")
}

// Any other denial is a real failure: the RBAC the chart ships may predate the
// status-subresource grant, and swallowing that would hide a latch the operator
// can never clear.
func TestUnlatchKORCTransportErrors_PropagatesForbidden(t *testing.T) {
	g := NewGomegaWithT(t)

	var patched []string
	c := unlatchTestClient(t, interceptor.Funcs{
		SubResourcePatch: func(_ context.Context, _ client.Client, _ string,
			obj client.Object, _ client.Patch, _ ...client.SubResourcePatchOption,
		) error {
			patched = append(patched, obj.GetName())
			return apierrors.NewForbidden(orcv1alpha1.Resource("projects"), obj.GetName(),
				errors.New("projects/status is forbidden"))
		},
	},
		unlatchTestProject("first", transportLatchedConditions()),
		unlatchTestProject("second", transportLatchedConditions()),
	)

	err := unlatchKORCTransportErrors(context.Background(), c,
		unlatchLiveProject(t, c, "first"), unlatchLiveProject(t, c, "second"))
	g.Expect(apierrors.IsForbidden(err)).To(BeTrue(), "the status reason must survive the %w wrapping")
	g.Expect(strings.HasPrefix(err.Error(), "clearing the latched transport error on *v1alpha1.Project")).
		To(BeTrue(), "got %q", err.Error())
	g.Expect(patched).To(Equal([]string{"first"}), "a denied patch must stop the pass")

	g.Expect(unlatchLiveProject(t, c, "first").Annotations).NotTo(HaveKey(korcTransportUnlatchedAtAnnotation),
		"only a clear that HAPPENED is stamped: the stamp paces the next clear, and a denial "+
			"that paced itself would silence the caller's report of it until the backoff expired")

	// Which is what the next pass has to prove. The denial must be reported again
	// rather than sit on the CR for one rate-limiter tick per backoff, where no
	// scrape, watch or condition-based alert can catch it.
	err = unlatchKORCTransportErrors(context.Background(), c,
		unlatchLiveProject(t, c, "first"), unlatchLiveProject(t, c, "second"))
	g.Expect(apierrors.IsForbidden(err)).To(BeTrue(),
		"a latch the operator can never clear must fail loud on EVERY pass")
	g.Expect(patched).To(Equal([]string{"first", "first"}), "and be retried rather than paced away")
}

// The classification the call sites relay survives the clear. K-ORC copies the
// Progressing reason and message onto Available for as long as the child is not
// available, and only Progressing is removed — so a pass that clears the latch
// still reports EndpointUnreachable rather than falling back to a generic wait.
func TestUnlatchKORCTransportErrors_LeavesTheClassifiableMessage(t *testing.T) {
	g := NewGomegaWithT(t)

	conds := append(transportLatchedConditions(), metav1.Condition{
		Type:               orcv1alpha1.ConditionAvailable,
		Status:             metav1.ConditionFalse,
		Reason:             orcv1alpha1.ConditionReasonInvalidConfiguration,
		Message:            refusedMessage(),
		LastTransitionTime: metav1.NewTime(time.Now().Add(-time.Minute)),
	})
	c := unlatchTestClient(t, interceptor.Funcs{}, unlatchTestProject("p", conds))

	live := unlatchLiveProject(t, c, "p")
	g.Expect(unlatchKORCTransportErrors(context.Background(), c, live)).To(Succeed())

	reason, rawMessage := classifyKORCObject(unlatchLiveProject(t, c, "p"))
	g.Expect(reason).To(Equal(conditionReasonEndpointUnreachable))
	g.Expect(rawMessage).To(Equal(refusedMessage()))
}

// The clear succeeded, so the child MUST end the pass paced. The stamp is the
// only record that the clear happened — korcUnlatchDue reads nothing else — so a
// stamp lost to a passing apiserver failure leaves the next pass finding the
// re-latched child due again, and the one retry per child per backoff collapses
// to one per korcRequeueAfter against a Keystone that may be failing precisely
// because it is overloaded.
func TestUnlatchKORCTransportErrors_RetriesTheStampBehindASuccessfulClear(t *testing.T) {
	g := NewGomegaWithT(t)

	var attempts int
	c := unlatchTestClient(t, interceptor.Funcs{
		Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object,
			patch client.Patch, opts ...client.PatchOption,
		) error {
			attempts++
			if attempts == 1 {
				return apierrors.NewTooManyRequests("slow down", 1)
			}
			return cl.Patch(ctx, obj, patch, opts...)
		},
	}, unlatchTestProject("p", transportLatchedConditions()))

	g.Expect(unlatchKORCTransportErrors(context.Background(), c, unlatchLiveProject(t, c, "p"))).To(Succeed())

	after := unlatchLiveProject(t, c, "p")
	g.Expect(attempts).To(Equal(2), "a stamp the apiserver shed must be retried inline, not dropped")
	g.Expect(after.Annotations).To(HaveKey(korcTransportUnlatchedAtAnnotation))
	g.Expect(korcUnlatchDue(after)).To(BeFalse(),
		"a child whose clear succeeded is paced, so the next pass does not hand it back again")
}

// A stamp that cannot be written at all is the diagnostic failing behind a
// repair that already succeeded: the latch is gone and K-ORC is retrying.
// Reporting it as conditionReasonTransportErrorRetryFailed would tell the CR
// that the retry failed on a child whose retry succeeded, and returning it would
// abandon every child behind this one — which needs the same repair, and which
// a raw transport error would reach on the most ordinary failure there is.
func TestUnlatchKORCTransportErrors_DoesNotReportAFailedStamp(t *testing.T) {
	rows := []struct {
		name string
		err  error
	}{
		{
			// No APIStatus, so none of korcUnlatchOutcome's apierrors predicates
			// match it: an apiserver restart or an LB blip dropping the connection
			// on the stamp would otherwise land in the terminal branch.
			name: "a raw transport error",
			err: &url.Error{
				Op: "Patch", URL: "https://10.0.0.1:443/apis/openstack.k-orc.cloud",
				Err: errors.New("connect: connection refused"),
			},
		},
		{name: "a shedding apiserver", err: apierrors.NewTooManyRequests("slow down", 1)},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			g := NewGomegaWithT(t)

			var stamped []string
			c := unlatchTestClient(t, interceptor.Funcs{
				Patch: func(_ context.Context, _ client.WithWatch, obj client.Object,
					_ client.Patch, _ ...client.PatchOption,
				) error {
					stamped = append(stamped, obj.GetName())
					return row.err
				},
			},
				unlatchTestProject("first", transportLatchedConditions()),
				unlatchTestProject("second", transportLatchedConditions()),
			)

			var logs []string
			logger := funcr.New(func(prefix, args string) {
				logs = append(logs, prefix+" "+args)
			}, funcr.Options{Verbosity: 0})
			ctx := log.IntoContext(context.Background(), logger)

			g.Expect(unlatchKORCTransportErrors(ctx, c,
				unlatchLiveProject(t, c, "first"), unlatchLiveProject(t, c, "second"))).
				To(Succeed(), "the clear succeeded; the stamp behind it is not the caller's to report")

			for _, name := range []string{"first", "second"} {
				after := unlatchLiveProject(t, c, name)
				g.Expect(apimeta.FindStatusCondition(after.GetConditions(), orcv1alpha1.ConditionProgressing)).
					To(BeNil(), "a lost stamp must not stop the children behind it from being cleared")
				g.Expect(after.Annotations).NotTo(HaveKey(korcTransportUnlatchedAtAnnotation))
			}
			g.Expect(stamped).To(ContainElements("first", "second"))

			combined := strings.Join(logs, "\n")
			g.Expect(combined).To(ContainSubstring("could not pace a K-ORC transport unlatch"),
				"a child this operator cannot pace is silent on the CR, so the log is the only record")
			g.Expect(combined).To(ContainSubstring(`"name"="first"`),
				"the line must name the child, so the unpaced clear can be attributed")
		})
	}
}
