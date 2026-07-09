// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the External keystone-mode helpers: the endpoint reader, the K-ORC
// message classifier, and the stalled-import detector.
package controller

import (
	"testing"
	"time"

	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
)

// TestExternalKeystoneAuthURL_NilSafe covers the two absent-block edge paths
// (no keystone service at all, keystone without an external block) alongside the
// populated case: the helper is called from message builders that must never
// panic on a Managed CR.
func TestExternalKeystoneAuthURL_NilSafe(t *testing.T) {
	g := NewWithT(t)

	noKeystone := &c5c3v1alpha1.ControlPlane{}
	g.Expect(externalKeystoneAuthURL(noKeystone)).To(BeEmpty(),
		"a ControlPlane with no keystone service has no external endpoint")

	noExternal := &c5c3v1alpha1.ControlPlane{
		Spec: c5c3v1alpha1.ControlPlaneSpec{
			Services: c5c3v1alpha1.ServicesSpec{Keystone: &c5c3v1alpha1.ServiceKeystoneSpec{}},
		},
	}
	g.Expect(externalKeystoneAuthURL(noExternal)).To(BeEmpty(),
		"a Managed keystone carries no external block")

	external := &c5c3v1alpha1.ControlPlane{
		Spec: c5c3v1alpha1.ControlPlaneSpec{
			Services: c5c3v1alpha1.ServicesSpec{
				Keystone: &c5c3v1alpha1.ServiceKeystoneSpec{
					Mode:     c5c3v1alpha1.KeystoneModeExternal,
					External: &c5c3v1alpha1.ExternalKeystoneSpec{AuthURL: "https://keystone.example.com/v3"},
				},
			},
		},
	}
	g.Expect(externalKeystoneAuthURL(external)).To(Equal("https://keystone.example.com/v3"))
}

// TestClassifyKORCMessage pins the D3 failure-class vocabulary AND the documented
// precedence. K-ORC collapses every hard failure into reason=TransientError, so
// these substrings are the only discriminator the ControlPlane has; a silent
// reordering here would relabel a TLS failure as an unreachable endpoint.
func TestClassifyKORCMessage(t *testing.T) {
	tests := []struct {
		name string
		msg  string
		want string
	}{
		{
			name: "wrong password yields AuthenticationFailed",
			msg:  `Authentication failed: Expected HTTP response code [200 201] when accessing [POST https://keystone.example.com/v3/auth/tokens], but got 401 instead: {"error":{"code":401,"message":"The request you have made requires authentication."}}`,
			want: conditionReasonAuthenticationFailed,
		},
		{
			name: "Unauthorized without a numeric code still yields AuthenticationFailed",
			msg:  "Unauthorized: the supplied credentials were rejected",
			want: conditionReasonAuthenticationFailed,
		},
		{
			name: "DNS failure yields EndpointUnreachable",
			msg:  `Get "https://keystone.example.com/v3": dial tcp: lookup keystone.example.com: no such host`,
			want: conditionReasonEndpointUnreachable,
		},
		{
			name: "connection refused yields EndpointUnreachable",
			msg:  `Post "https://10.0.0.1:5000/v3/auth/tokens": connection refused`,
			want: conditionReasonEndpointUnreachable,
		},
		{
			name: "timeout yields EndpointUnreachable",
			msg:  `Get "https://keystone.example.com/v3": i/o timeout`,
			want: conditionReasonEndpointUnreachable,
		},
		{
			name: "private CA yields TLSVerificationFailed",
			msg:  `Get "https://keystone.example.com/v3": x509: certificate signed by unknown authority`,
			want: conditionReasonTLSVerificationFailed,
		},
		{
			name: "wrong region or endpointType yields CatalogEndpointMismatch",
			msg:  "No suitable endpoint could be found in the service catalog.",
			want: conditionReasonCatalogEndpointMismatch,
		},
		{
			name: "stale user id 403 yields CredentialDrift",
			msg:  `Expected HTTP response code [201] when accessing [POST .../application_credentials], but got 403 instead: {"error":{"code":403,"message":"You are not authorized to perform the requested action: identity:create_application_credential."}}`,
			want: conditionReasonCredentialDrift,
		},
		{
			// Precedence guard: a TLS failure surfaced through a dial wrapper
			// mentions BOTH x509 and dial tcp. TLS must win — telling the operator
			// to check DNS when the CA bundle is missing sends them the wrong way.
			name: "x509 outranks dial tcp",
			msg:  `Get "https://keystone.example.com/v3": dial tcp 10.0.0.1:443: x509: certificate signed by unknown authority`,
			want: conditionReasonTLSVerificationFailed,
		},
		{
			// Precedence guard: the catalog sentence is emitted by an authenticated
			// client, but gophercloud's wrapper can still carry a 401 from an earlier
			// retry. The catalog class is the more specific, actionable one.
			name: "catalog mismatch outranks a trailing 401",
			msg:  "No suitable endpoint could be found in the service catalog. (previous attempt: 401)",
			want: conditionReasonCatalogEndpointMismatch,
		},
		{
			name: "an unrecognised message classifies to nothing",
			msg:  "waiting for the OpenStack resource to settle",
			want: "",
		},
		{
			name: "an empty message classifies to nothing",
			msg:  "",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			g.Expect(classifyKORCMessage(tt.msg)).To(Equal(tt.want))
		})
	}
}

// korcConditions is a tiny builder for an ApplicationCredential carrying the
// given conditions, used as a stand-in ObjectWithConditions.
func acWithConditions(conds ...metav1.Condition) *orcv1alpha1.ApplicationCredential {
	return &orcv1alpha1.ApplicationCredential{
		Status: orcv1alpha1.ApplicationCredentialStatus{Conditions: conds},
	}
}

// TestClassifyKORCObject_RelaysMessageVerbatim asserts the raw K-ORC message is
// returned byte-identical (no truncation, no reformatting) and that an object
// with no classifiable condition — the common not-yet-converged case — yields
// the empty classification rather than a spurious reason.
func TestClassifyKORCObject_RelaysMessageVerbatim(t *testing.T) {
	g := NewWithT(t)

	raw := `Authentication failed: got 401 instead: {"error":{"message":"The request you have made requires authentication."}}`
	obj := acWithConditions(metav1.Condition{
		Type:    orcv1alpha1.ConditionProgressing,
		Status:  metav1.ConditionTrue,
		Reason:  orcv1alpha1.ConditionReasonTransientError,
		Message: raw,
	})

	reason, message := classifyKORCObject(obj)
	g.Expect(reason).To(Equal(conditionReasonAuthenticationFailed))
	g.Expect(message).To(Equal(raw), "K-ORC's message must be relayed verbatim")

	// Edge: a converging object with no classifiable message.
	reason, message = classifyKORCObject(acWithConditions(metav1.Condition{
		Type:    orcv1alpha1.ConditionAvailable,
		Status:  metav1.ConditionFalse,
		Reason:  orcv1alpha1.ConditionReasonProgressing,
		Message: "Waiting for the resource to be created",
	}))
	g.Expect(reason).To(BeEmpty())
	g.Expect(message).To(BeEmpty())

	// Edge: no conditions at all (freshly created CR).
	reason, _ = classifyKORCObject(acWithConditions())
	g.Expect(reason).To(BeEmpty())

	// Edge: a nil object must not panic.
	reason, _ = classifyKORCObject(nil)
	g.Expect(reason).To(BeEmpty())
}

// TestClassifyExternalKORCFailure_FirstObjectWins asserts the root dependency
// wins: when the admin Domain import already carries a classifiable failure, the
// downstream User and ApplicationCredential — which merely blocked on it — must
// not shadow it.
func TestClassifyExternalKORCFailure_FirstObjectWins(t *testing.T) {
	g := NewWithT(t)

	domainMsg := `Get "https://keystone.example.com/v3": x509: certificate signed by unknown authority`
	domain := &orcv1alpha1.Domain{
		Status: orcv1alpha1.DomainStatus{Conditions: []metav1.Condition{{
			Type:    orcv1alpha1.ConditionProgressing,
			Status:  metav1.ConditionTrue,
			Reason:  orcv1alpha1.ConditionReasonTransientError,
			Message: domainMsg,
		}}},
	}
	user := &orcv1alpha1.User{
		Status: orcv1alpha1.UserStatus{Conditions: []metav1.Condition{{
			Type:    orcv1alpha1.ConditionProgressing,
			Status:  metav1.ConditionTrue,
			Reason:  orcv1alpha1.ConditionReasonTransientError,
			Message: "got 401 instead",
		}}},
	}
	ac := acWithConditions(metav1.Condition{
		Type:    orcv1alpha1.ConditionProgressing,
		Status:  metav1.ConditionTrue,
		Reason:  orcv1alpha1.ConditionReasonTransientError,
		Message: "dial tcp: no such host",
	})

	reason, message := classifyExternalKORCFailure(domain, user, ac)
	g.Expect(reason).To(Equal(conditionReasonTLSVerificationFailed))
	g.Expect(message).To(Equal(domainMsg))

	// Edge: nothing classifies — the caller must fall through to its own reason.
	reason, message = classifyExternalKORCFailure(acWithConditions())
	g.Expect(reason).To(BeEmpty())
	g.Expect(message).To(BeEmpty())

	// Edge: no objects at all.
	reason, _ = classifyExternalKORCFailure()
	g.Expect(reason).To(BeEmpty())
}

// TestKORCImportStalled exercises the silent-empty detector's grace window and
// every non-stalled edge: an Available import, a different Available=False
// message, a fresh transition, and an import with no Available condition yet.
func TestKORCImportStalled(t *testing.T) {
	const grace = 2 * time.Minute
	stale := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	fresh := metav1.Now()

	tests := []struct {
		name  string
		conds []metav1.Condition
		want  bool
	}{
		{
			name: "Available=False on the created-externally marker, past the grace window",
			conds: []metav1.Condition{{
				Type:               orcv1alpha1.ConditionAvailable,
				Status:             metav1.ConditionFalse,
				Message:            korcImportPendingExternalMarker,
				LastTransitionTime: stale,
			}},
			want: true,
		},
		{
			name: "same marker, still inside the grace window",
			conds: []metav1.Condition{{
				Type:               orcv1alpha1.ConditionAvailable,
				Status:             metav1.ConditionFalse,
				Message:            korcImportPendingExternalMarker,
				LastTransitionTime: fresh,
			}},
			want: false,
		},
		{
			name: "Available=True is never stalled",
			conds: []metav1.Condition{{
				Type:               orcv1alpha1.ConditionAvailable,
				Status:             metav1.ConditionTrue,
				Message:            "resource is available",
				LastTransitionTime: stale,
			}},
			want: false,
		},
		{
			name: "Available=False for an unrelated reason is not the silent-empty class",
			conds: []metav1.Condition{{
				Type:               orcv1alpha1.ConditionAvailable,
				Status:             metav1.ConditionFalse,
				Message:            "transient error contacting the OpenStack API",
				LastTransitionTime: stale,
			}},
			want: false,
		},
		{
			name: "no Available condition yet",
			conds: []metav1.Condition{{
				Type:               orcv1alpha1.ConditionProgressing,
				Status:             metav1.ConditionTrue,
				Message:            korcImportPendingExternalMarker,
				LastTransitionTime: stale,
			}},
			want: false,
		},
		{
			name:  "no conditions at all",
			conds: nil,
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			domain := &orcv1alpha1.Domain{Status: orcv1alpha1.DomainStatus{Conditions: tt.conds}}
			g.Expect(korcImportStalled(domain, grace)).To(Equal(tt.want))
		})
	}

	// Edge: a nil object must not panic.
	NewWithT(t).Expect(korcImportStalled(nil, grace)).To(BeFalse())
}
