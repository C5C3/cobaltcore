// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package validation provides the webhook validators shared by the service
// operators, each returning a field.ErrorList the caller appends to its
// aggregate.
//
// DECISION (CEL-vs-webhook split): several of these rules also exist as CEL
// XValidation markers on the shared commonv1 types (the database, cache and
// messaging XORs, Dynamic-requires-clusterRef, the image tag/digest XOR). The
// CEL rules stay — they are the schema-layer gate the API server enforces even
// when the webhook is unavailable. The webhooks keep a defense-in-depth copy
// for objects that bypass schema validation (old objects, direct etcd writes),
// but that copy is THIS package's single implementation rather than a
// hand-rolled per-operator triplicate: each rule now exists exactly twice —
// once as CEL, once here.
package validation

import (
	"context"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/robfig/cron/v3"
	"gopkg.in/inf.v0"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	k8svalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

// DatabaseXOR enforces that exactly one of clusterRef (managed mode) or host
// (brownfield mode) is set, mirroring the CEL rule on commonv1.DatabaseSpec.
func DatabaseXOR(fldPath *field.Path, db *commonv1.DatabaseSpec) field.ErrorList {
	if (db.ClusterRef != nil) == (db.Host != "") {
		return field.ErrorList{field.Invalid(
			fldPath,
			*db,
			"exactly one of clusterRef or host must be set",
		)}
	}
	return nil
}

// CacheXOR enforces that exactly one of clusterRef (managed mode) or servers
// (brownfield mode) is set, mirroring the CEL rule on commonv1.CacheSpec.
func CacheXOR(fldPath *field.Path, cache *commonv1.CacheSpec) field.ErrorList {
	if (cache.ClusterRef != nil) == (len(cache.Servers) > 0) {
		return field.ErrorList{field.Invalid(
			fldPath,
			*cache,
			"exactly one of clusterRef or servers must be set",
		)}
	}
	return nil
}

// MessagingXOR enforces that exactly one of clusterRef (managed mode) or
// secretRef (brownfield mode) is set, mirroring the CEL rule on
// commonv1.MessagingSpec.
func MessagingXOR(fldPath *field.Path, m *commonv1.MessagingSpec) field.ErrorList {
	if (m.ClusterRef != nil) == (m.SecretRef != nil) {
		return field.ErrorList{field.Invalid(
			fldPath,
			*m,
			"exactly one of clusterRef or secretRef must be set",
		)}
	}
	return nil
}

// CacheNoControlChars rejects a newline or carriage return in the CacheSpec
// fields that reach the INI renderer verbatim. cache.ResolveServers joins
// spec.cache.servers with commas (brownfield mode) or derives
// "<clusterRef.name>:11211" (managed mode), and every consumer renders the
// result as "memcached_servers = %s" inside [keystone_authtoken]. A newline
// there injects an additional config line — smuggling a whole key past the
// (section, key)-keyed ownership and catalog gates that guard extraConfig,
// which is exactly how an attacker-controlled auth_url would reach
// keystonemiddleware. Its schema-layer twin is the items pattern on
// CacheSpec.Servers plus the XValidation rule on CacheSpec.ClusterRef.Name.
func CacheNoControlChars(fldPath *field.Path, cache *commonv1.CacheSpec) field.ErrorList {
	const detail = "value must not contain a newline or carriage return: it is rendered verbatim " +
		"into the service configuration file, so a newline injects arbitrary config lines"

	var allErrs field.ErrorList
	if cache.ClusterRef != nil && HasControlChars(cache.ClusterRef.Name) {
		allErrs = append(allErrs, field.Invalid(
			fldPath.Child("clusterRef", "name"),
			cache.ClusterRef.Name,
			detail,
		))
	}
	for i, server := range cache.Servers {
		if HasControlChars(server) {
			allErrs = append(allErrs, field.Invalid(
				fldPath.Child("servers").Index(i),
				server,
				detail,
			))
		}
	}
	return allErrs
}

// HasControlChars reports whether s contains a newline or carriage return. The
// INI renderer writes every section name, key and value verbatim, so either
// character lets one entry inject additional config lines. It is exported so
// the per-operator webhooks that guard their own typed spec fields share this
// one definition of the character set rather than each carrying a copy that
// could drift from it.
func HasControlChars(s string) bool {
	return strings.ContainsAny(s, "\n\r")
}

// DynamicCredentialsRequireClusterRef enforces that CredentialsMode Dynamic
// (engine-issued credentials) is only used in managed mode (ClusterRef set),
// mirroring the second CEL rule on commonv1.DatabaseSpec.
func DynamicCredentialsRequireClusterRef(fldPath *field.Path, db *commonv1.DatabaseSpec) field.ErrorList {
	if db.CredentialsMode == commonv1.CredentialsModeDynamic && db.ClusterRef == nil {
		return field.ErrorList{field.Invalid(
			fldPath.Child("credentialsMode"),
			db.CredentialsMode,
			"credentialsMode Dynamic requires clusterRef (managed mode)",
		)}
	}
	return nil
}

// CronSchedule rejects a schedule cron.ParseStandard cannot parse. Callers
// keep their own empty-schedule guards — the required-vs-defaulted semantics
// (and the message naming the default) are per-field policy.
//
// It also rejects a TZ= or CRON_TZ= prefix, which cron.ParseStandard accepts but
// the API server refuses in a CronJob's spec.schedule on create. Admitting one
// would leave the CR valid and its CronJob unapplicable, failing every reconcile
// on a spec field the controller cannot repair. The check matches the API
// server's own, which looks for "TZ" anywhere in the schedule.
func CronSchedule(fldPath *field.Path, schedule string) field.ErrorList {
	if strings.Contains(schedule, "TZ") {
		return field.ErrorList{field.Invalid(
			fldPath,
			schedule,
			"TZ and CRON_TZ are not allowed in the schedule: the CronJob API rejects them",
		)}
	}
	if _, err := cron.ParseStandard(schedule); err != nil {
		return field.ErrorList{field.Invalid(
			fldPath,
			schedule,
			fmt.Sprintf("invalid cron expression: %v", err),
		)}
	}
	return nil
}

// TopologySpreadSelector enforces that every custom TopologySpreadConstraint
// carries a labelSelector whose matchLabels equal the Deployment's selector
// labels exactly, with no matchExpressions. A selector that widens or narrows
// beyond the Deployment's intent would spread (or fail to spread) the wrong
// pods.
func TopologySpreadSelector(fldPath *field.Path, tscs []corev1.TopologySpreadConstraint, requiredMatchLabels map[string]string) field.ErrorList {
	var allErrs field.ErrorList
	for i, tsc := range tscs {
		if tsc.LabelSelector == nil {
			allErrs = append(allErrs, field.Required(
				fldPath.Index(i).Child("labelSelector"),
				"labelSelector is required on each TopologySpreadConstraint",
			))
			continue
		}
		if !maps.Equal(tsc.LabelSelector.MatchLabels, requiredMatchLabels) {
			allErrs = append(allErrs, field.Invalid(
				fldPath.Index(i).Child("labelSelector"),
				tsc.LabelSelector.MatchLabels,
				fmt.Sprintf("labelSelector.matchLabels must equal the Deployment selector labels %v", requiredMatchLabels),
			))
		}
		// Reject MatchExpressions to prevent selectors that widen or narrow
		// beyond the Deployment's intent. Only exact matchLabels are allowed.
		if len(tsc.LabelSelector.MatchExpressions) > 0 {
			allErrs = append(allErrs, field.Invalid(
				fldPath.Index(i).Child("labelSelector", "matchExpressions"),
				tsc.LabelSelector.MatchExpressions,
				"matchExpressions are not allowed; labelSelector must use matchLabels only",
			))
		}
	}
	return allErrs
}

// SecretStoreRef is the defense-in-depth twin of the CRD markers on
// commonv1.SecretStoreRefSpec: a nil ref is valid (the operators default to the
// shared cluster store), an empty name is field.Required, and a kind outside
// the {ClusterSecretStore, SecretStore} enum is field.NotSupported. It lets the
// webhook reject an invalid store reference that reached etcd through a bypass
// of schema validation.
func SecretStoreRef(fldPath *field.Path, ref *commonv1.SecretStoreRefSpec) field.ErrorList {
	if ref == nil {
		return nil
	}
	var allErrs field.ErrorList
	if ref.Name == "" {
		allErrs = append(allErrs, field.Required(fldPath.Child("name"), "store name must be set"))
	}
	switch ref.Kind {
	case "", commonv1.SecretStoreKindCluster, commonv1.SecretStoreKindNamespaced:
		// Empty kind is accepted here and defaulted to ClusterSecretStore by
		// the CRD marker / EffectiveStoreRef.
	default:
		allErrs = append(allErrs, field.NotSupported(
			fldPath.Child("kind"),
			ref.Kind,
			[]string{string(commonv1.SecretStoreKindCluster), string(commonv1.SecretStoreKindNamespaced)},
		))
	}
	return allErrs
}

// TargetClusterRef is the defense-in-depth twin of the CRD markers on
// commonv1.TargetClusterRefSpec: a nil ref is valid (the CR's children are
// created on the management cluster the operator runs on) and an empty name is
// field.Required. It lets the webhook reject an unresolvable cluster reference
// that reached etcd through a bypass of schema validation.
func TargetClusterRef(fldPath *field.Path, ref *commonv1.TargetClusterRefSpec) field.ErrorList {
	if ref == nil {
		return nil
	}
	if ref.Name == "" {
		return field.ErrorList{field.Required(fldPath.Child("name"), "target cluster name must be set")}
	}
	return nil
}

// TargetClusterRefImmutable is the webhook-layer twin of the two transition CEL
// rules the workload CRDs carry on spec.targetClusterRef: adding the ref,
// removing it, or renaming it is rejected once the CR exists, because each of
// those strands the children already created on the previously selected
// cluster. Both messages contain the string the CEL rules pin, so a client sees
// the same wording whichever layer rejects the update.
func TargetClusterRefImmutable(fldPath *field.Path, oldRef, newRef *commonv1.TargetClusterRefSpec) field.ErrorList {
	if (oldRef == nil) != (newRef == nil) {
		return field.ErrorList{field.Invalid(
			fldPath,
			newRef,
			"targetClusterRef is immutable (adding or removing it after creation is not permitted)",
		)}
	}
	if oldRef == nil || oldRef.Name == newRef.Name {
		return nil
	}
	return field.ErrorList{field.Invalid(
		fldPath.Child("name"),
		newRef.Name,
		"targetClusterRef is immutable (the children already exist on the previously named cluster)",
	)}
}

// PriorityClassExists verifies that name references an existing
// scheduling.k8s.io/v1 PriorityClass, catching typos at admission time. A nil
// Reader or an empty name skips the check — programmatically constructed
// webhooks without a client stay permissive rather than failing closed on a
// lookup they cannot perform.
func PriorityClassExists(ctx context.Context, c client.Reader, fldPath *field.Path, name string) field.ErrorList {
	if name == "" || c == nil {
		return nil
	}
	pc := &schedulingv1.PriorityClass{}
	if err := c.Get(ctx, types.NamespacedName{Name: name}, pc); err != nil {
		if apierrors.IsNotFound(err) {
			return field.ErrorList{field.NotFound(fldPath, name)}
		}
		return field.ErrorList{field.InternalError(
			fldPath,
			fmt.Errorf("failed to look up PriorityClass: %w", err),
		)}
	}
	return nil
}

// NodeSelectorLabels checks the label grammar of a node selector the schema
// cannot express on a map: every key must be a qualified name, reported at
// fldPath, and every value a valid label value, reported at fldPath.Key(key).
// A nil or empty selector returns none. Keys are checked in sorted order, so
// the error list is stable.
func NodeSelectorLabels(fldPath *field.Path, selector map[string]string) field.ErrorList {
	var errs field.ErrorList
	for _, key := range slices.Sorted(maps.Keys(selector)) {
		for _, msg := range k8svalidation.IsQualifiedName(key) {
			errs = append(errs, field.Invalid(fldPath, key, msg))
		}
		for _, msg := range k8svalidation.IsValidLabelValue(selector[key]) {
			errs = append(errs, field.Invalid(fldPath.Key(key), selector[key], msg))
		}
	}
	return errs
}

// tolerationOperators are the operators a toleration may carry; "" means
// Equal. Lt and Gt compare a numeric taint value, so their value must be an
// integer. Whether the cluster enables them (the TaintTolerationComparisonOperators
// feature gate) is left to the API server.
var tolerationOperators = []corev1.TolerationOperator{
	"", corev1.TolerationOpEqual, corev1.TolerationOpExists, corev1.TolerationOpLt, corev1.TolerationOpGt,
}

// taintEffects are the effects a toleration may name; an empty effect matches
// every effect.
var taintEffects = []corev1.TaintEffect{
	corev1.TaintEffectNoSchedule, corev1.TaintEffectPreferNoSchedule, corev1.TaintEffectNoExecute,
}

// Tolerations checks tolerations with the rules of the API server's own
// toleration validation, so a CR the webhook admits never renders a pod
// template the API server refuses:
//
//   - a non-empty key must be a qualified name ([i].key);
//   - an empty key requires the Exists operator ([i].operator);
//   - tolerationSeconds requires the NoExecute effect ([i].effect);
//   - Exists requires an empty value ([i].operator);
//   - Equal, or no operator, requires a valid label value ([i].value);
//   - Lt and Gt require an integer value ([i].value);
//   - the operator must be Equal, Exists, Lt or Gt, and the effect empty or
//     NoSchedule, PreferNoSchedule or NoExecute (field.NotSupported).
func Tolerations(fldPath *field.Path, tolerations []corev1.Toleration) field.ErrorList {
	var errs field.ErrorList
	for i, t := range tolerations {
		idxPath := fldPath.Index(i)
		if t.Key != "" {
			for _, msg := range k8svalidation.IsQualifiedName(t.Key) {
				errs = append(errs, field.Invalid(idxPath.Child("key"), t.Key, msg))
			}
		}
		if t.Key == "" && t.Operator != corev1.TolerationOpExists {
			errs = append(errs, field.Invalid(idxPath.Child("operator"), t.Operator,
				"operator must be Exists when `key` is empty, which means \"match all values and all keys\""))
		}
		if t.TolerationSeconds != nil && t.Effect != corev1.TaintEffectNoExecute {
			errs = append(errs, field.Invalid(idxPath.Child("effect"), t.Effect,
				"effect must be 'NoExecute' when `tolerationSeconds` is set"))
		}
		switch {
		case !slices.Contains(tolerationOperators, t.Operator):
			errs = append(errs, field.NotSupported(idxPath.Child("operator"), t.Operator, tolerationOperators[1:]))
		case t.Operator == corev1.TolerationOpExists && t.Value != "":
			errs = append(errs, field.Invalid(idxPath.Child("operator"), t.Operator,
				"value must be empty when `operator` is 'Exists'"))
		case t.Operator == "" || t.Operator == corev1.TolerationOpEqual:
			for _, msg := range k8svalidation.IsValidLabelValue(t.Value) {
				errs = append(errs, field.Invalid(idxPath.Child("value"), t.Value, msg))
			}
		case t.Operator == corev1.TolerationOpLt || t.Operator == corev1.TolerationOpGt:
			if _, err := strconv.ParseInt(t.Value, 10, 64); err != nil {
				errs = append(errs, field.Invalid(idxPath.Child("value"), t.Value,
					"value must be an integer when `operator` is 'Lt' or 'Gt'"))
			}
		}
		if t.Effect != "" && !slices.Contains(taintEffects, t.Effect) {
			errs = append(errs, field.NotSupported(idxPath.Child("effect"), t.Effect, taintEffects))
		}
	}
	return errs
}

// NodePlacement checks the node selector and the tolerations of p. A nil p
// returns none. The affinity is left to the API server, which validates the
// rendered pod template.
func NodePlacement(fldPath *field.Path, p *commonv1.NodePlacementSpec) field.ErrorList {
	if p == nil {
		return nil
	}
	errs := NodeSelectorLabels(fldPath.Child("nodeSelector"), p.NodeSelector)
	return append(errs, Tolerations(fldPath.Child("tolerations"), p.Tolerations)...)
}

// RequestsWithinLimits rejects a request that exceeds the limit rr sets for
// the same resource, at fldPath.requests.<name>. A nil rr returns none.
// Resources are checked in sorted order, so the error list is stable.
func RequestsWithinLimits(fldPath *field.Path, rr *corev1.ResourceRequirements) field.ErrorList {
	if rr == nil {
		return nil
	}
	var errs field.ErrorList
	for _, name := range slices.Sorted(maps.Keys(rr.Requests)) {
		request := rr.Requests[name]
		if limit, hasLimit := rr.Limits[name]; hasLimit && request.Cmp(limit) > 0 {
			errs = append(errs, field.Invalid(
				fldPath.Child("requests", string(name)),
				request.String(),
				fmt.Sprintf("%s request must not exceed limit (%s)", name, limit.String()),
			))
		}
	}
	return errs
}

// AutoscalingTargetRequests rejects a utilization target of a that the
// HorizontalPodAutoscaler cannot compute against the container rr sizes: a
// target is measured against the sum of the requests of every container in the
// pod, so a zero request either fails the metric or inflates it.
//
// It checks cpu while a.TargetCPUUtilization is set and memory while
// a.TargetMemoryUtilization is set, in that order. A named request decides
// alone: a zero or negative one is rejected at requests.<name>. Without a
// request, a zero or negative limit is rejected at limits.<name>, because the
// API server copies that limit into the request. A block that names neither
// passes: the operator's render-time default fills a positive request. A nil a
// or rr returns none.
func AutoscalingTargetRequests(fldPath *field.Path, rr *corev1.ResourceRequirements, a *commonv1.AutoscalingSpec) field.ErrorList {
	if a == nil || rr == nil {
		return nil
	}
	targets := []struct {
		name   corev1.ResourceName
		target *int32
		field  string
	}{
		{name: corev1.ResourceCPU, target: a.TargetCPUUtilization, field: "targetCPUUtilization"},
		{name: corev1.ResourceMemory, target: a.TargetMemoryUtilization, field: "targetMemoryUtilization"},
	}
	var errs field.ErrorList
	for _, t := range targets {
		if t.target == nil {
			continue
		}
		path := fldPath.Child("requests", string(t.name))
		kind := "request"
		reason := "the HorizontalPodAutoscaler divides the pods' usage by the sum of their containers' requests"
		q, ok := rr.Requests[t.name]
		if !ok {
			path = fldPath.Child("limits", string(t.name))
			kind = "limit"
			reason = "without a request the API server copies the limit into the request, and " + reason
			q, ok = rr.Limits[t.name]
		}
		if !ok || q.Sign() > 0 {
			continue
		}
		errs = append(errs, field.Invalid(path, q.String(), fmt.Sprintf(
			"%s %s must be greater than zero while %s is set: %s", t.name, kind, t.field, reason)))
	}
	return errs
}

// AutoscalingTargetsReachable rejects a utilization target in a that is above
// 100 and that the containers of the API pod can never reach. Each element of
// blocks is the resources block of one container; fldPath is the autoscaling
// path.
//
// A target above 100 needs a container that may use more than it requests.
// Each block's ceiling follows the render-time rule of WithResourceDefaults:
// a nil block, or one that names the resource neither as request nor as
// limit, is unbounded for cpu and has ratio 1 for memory (the default renders
// the same memory as request and limit). A block that names only a request is
// unbounded. A block that names only a limit has ratio 1, because the API
// server copies the limit into the request. A block that names both has ratio
// limit/request; a request of zero or below counts as unbounded, since
// AutoscalingTargetRequests reports it. So does a limit or request written
// with a decimal exponent beyond ±30, such as 1e40 or 0e-40, because rounding
// it to milli-units would build a number with that many digits.
//
// The pod's ceiling is the largest block ratio. It bounds the summed
// utilization from above, so the check never rejects a reachable target. The
// ratios are compared in integers, so a target of exactly 100 × limit /
// request passes whatever the ratio. When every block is bounded and the
// target exceeds the ceiling, the target is rejected at fldPath.<field>. A nil
// a, a target that is nil or at most 100, and an empty blocks return none.
func AutoscalingTargetsReachable(fldPath *field.Path, a *commonv1.AutoscalingSpec, blocks ...*corev1.ResourceRequirements) field.ErrorList {
	if a == nil || len(blocks) == 0 {
		return nil
	}
	targets := []struct {
		name   corev1.ResourceName
		target *int32
		field  string
	}{
		{name: corev1.ResourceCPU, target: a.TargetCPUUtilization, field: "targetCPUUtilization"},
		{name: corev1.ResourceMemory, target: a.TargetMemoryUtilization, field: "targetMemoryUtilization"},
	}
	var errs field.ErrorList
	for _, t := range targets {
		if t.target == nil || *t.target <= 100 {
			continue
		}
		// pct is the largest whole target a bounded block reaches,
		// floor(100 × limit / request). A whole target is reachable exactly
		// when it is at most pct.
		target := inf.NewDec(int64(*t.target), 0)
		var pct *inf.Dec
		reachable := false
		for _, rr := range blocks {
			limit, request, bounded := blockCeiling(t.name, rr)
			if !bounded {
				reachable = true
				break
			}
			p := new(inf.Dec).Mul(limit, inf.NewDec(100, 0))
			p.QuoRound(p, request, 0, inf.RoundFloor)
			if p.Cmp(target) >= 0 {
				reachable = true
				break
			}
			if pct == nil || p.Cmp(pct) > 0 {
				pct = p
			}
		}
		if reachable {
			continue
		}
		errs = append(errs, field.Invalid(fldPath.Child(t.field), *t.target, fmt.Sprintf(
			"%s %d can never be reached: no container of the API pod can use more than %d%% of its %s request, "+
				"because its %s limit caps it there; set a %s limit above the request, or a target of at most %d",
			t.field, *t.target, pct, t.name, t.name, t.name, pct)))
	}
	return errs
}

// blockCeiling returns the largest multiple of its request a container sized
// by rr can use of resource name, as the fraction limit/request in
// milli-units, under the rule AutoscalingTargetsReachable documents. bounded
// is false when the container has no such ceiling.
func blockCeiling(name corev1.ResourceName, rr *corev1.ResourceRequirements) (limit, request *inf.Dec, bounded bool) {
	var req, lim resource.Quantity
	var hasRequest, hasLimit bool
	if rr != nil {
		req, hasRequest = rr.Requests[name]
		lim, hasLimit = rr.Limits[name]
	}
	one := inf.NewDec(1, 0)
	switch {
	case !hasRequest && !hasLimit:
		return one, one, name == corev1.ResourceMemory
	case !hasLimit:
		return nil, nil, false
	case !hasRequest:
		return one, one, true
	case req.Sign() <= 0:
		return nil, nil, false
	}
	limDec, reqDec := lim.AsDec(), req.AsDec()
	if outsideCeilingScale(limDec) || outsideCeilingScale(reqDec) {
		return nil, nil, false
	}
	// Round both up to whole milli-units, as MilliValue and the HPA do,
	// without the int64 overflow of MilliValue.
	return new(inf.Dec).Round(limDec, 3, inf.RoundCeil), new(inf.Dec).Round(reqDec, 3, inf.RoundCeil), true
}

// ceilingMaxScale bounds the decimal scale of a quantity blockCeiling rounds
// to milli-units. Rounding shifts it by 3 - scale digits, and inf.Dec builds
// 10^shift as a big.Int: 1e2000000000 parses cheaply into scale -2e9, and
// the zero 0e-2000000000 into scale 2e9, so either would build a number of
// two billion digits. A parsed nonzero quantity has a scale of at most 9,
// and one below -30 is at least 10^31, which no container is sized with.
const ceilingMaxScale = 30

// outsideCeilingScale reports whether d is too far from milli-units for
// blockCeiling to round.
func outsideCeilingScale(d *inf.Dec) bool {
	return d.Scale() < -ceilingMaxScale || d.Scale() > ceilingMaxScale
}

// The bounds the autoscaling/v2 API validation applies to a scaling rule.
// The API server's validation package cannot be imported, so they are
// restated here.
const (
	maxStabilizationWindowSeconds = 3600
	maxScalingPolicyPeriodSeconds = 1800
)

// AutoscalingBehavior checks a spec.autoscaling.behavior block against the
// rules the autoscaling/v2 API validation applies when the operator writes
// the HorizontalPodAutoscaler, so a bad value fails at admission and not as a
// reconcile error. fldPath is the behavior path. For each set direction
// (scaleUp, then scaleDown) it rejects a stabilizationWindowSeconds outside
// 0..3600, a selectPolicy other than Max, Min or Disabled, a policy type
// other than Pods or Percent, a policy value of zero or below, a policy
// periodSeconds outside 1..1800, and a negative tolerance. A nil b, an unset
// direction and an empty policies list return none: the API server defaults
// an empty list.
func AutoscalingBehavior(fldPath *field.Path, b *autoscalingv2.HorizontalPodAutoscalerBehavior) field.ErrorList {
	if b == nil {
		return nil
	}
	errs := scalingRules(fldPath.Child("scaleUp"), b.ScaleUp)
	return append(errs, scalingRules(fldPath.Child("scaleDown"), b.ScaleDown)...)
}

// scalingRules checks one direction of a behavior block for
// AutoscalingBehavior. A nil r returns none.
func scalingRules(fldPath *field.Path, r *autoscalingv2.HPAScalingRules) field.ErrorList {
	if r == nil {
		return nil
	}
	var errs field.ErrorList
	if w := r.StabilizationWindowSeconds; w != nil && (*w < 0 || *w > maxStabilizationWindowSeconds) {
		errs = append(errs, field.Invalid(fldPath.Child("stabilizationWindowSeconds"), *w,
			fmt.Sprintf("stabilizationWindowSeconds must be between 0 and %d", maxStabilizationWindowSeconds)))
	}
	if sp := r.SelectPolicy; sp != nil {
		supported := []autoscalingv2.ScalingPolicySelect{
			autoscalingv2.MaxChangePolicySelect, autoscalingv2.MinChangePolicySelect, autoscalingv2.DisabledPolicySelect,
		}
		if !slices.Contains(supported, *sp) {
			errs = append(errs, field.NotSupported(fldPath.Child("selectPolicy"), *sp, supported))
		}
	}
	supportedTypes := []autoscalingv2.HPAScalingPolicyType{autoscalingv2.PodsScalingPolicy, autoscalingv2.PercentScalingPolicy}
	for i, p := range r.Policies {
		pp := fldPath.Child("policies").Index(i)
		if !slices.Contains(supportedTypes, p.Type) {
			errs = append(errs, field.NotSupported(pp.Child("type"), p.Type, supportedTypes))
		}
		if p.Value <= 0 {
			errs = append(errs, field.Invalid(pp.Child("value"), p.Value, "value must be greater than zero"))
		}
		if p.PeriodSeconds < 1 || p.PeriodSeconds > maxScalingPolicyPeriodSeconds {
			errs = append(errs, field.Invalid(pp.Child("periodSeconds"), p.PeriodSeconds,
				fmt.Sprintf("periodSeconds must be between 1 and %d", maxScalingPolicyPeriodSeconds)))
		}
	}
	if tol := r.Tolerance; tol != nil && tol.Sign() < 0 {
		errs = append(errs, field.Invalid(fldPath.Child("tolerance"), tol.String(), "tolerance must not be negative"))
	}
	return errs
}

// Job checks a CR's spec.jobs block: requests within limits, an existing
// priority class (PriorityClassExists), and the node placement. A nil spec
// returns none.
func Job(ctx context.Context, c client.Reader, fldPath *field.Path, spec *commonv1.JobSpec) field.ErrorList {
	if spec == nil {
		return nil
	}
	errs := JobBase(ctx, c, fldPath, &spec.JobBaseSpec)
	return append(errs, NodePlacement(fldPath, &spec.NodePlacementSpec)...)
}

// JobBase checks a spec.jobs block without placement: requests within limits
// and an existing priority class. A nil spec returns none.
func JobBase(ctx context.Context, c client.Reader, fldPath *field.Path, spec *commonv1.JobBaseSpec) field.ErrorList {
	if spec == nil {
		return nil
	}
	errs := RequestsWithinLimits(fldPath.Child("resources"), spec.Resources)
	if spec.PriorityClassName != nil {
		errs = append(errs, PriorityClassExists(ctx, c, fldPath.Child("priorityClassName"), *spec.PriorityClassName)...)
	}
	return errs
}

// AttachedSiblings lists list in self's namespace through reader and returns
// the objects that are neither self nor Terminating and for which sameParent
// reports true. It is the List-and-filter skeleton the cross-CR uniqueness
// rules share: each caller keeps its own predicate, field path and message, and
// none repeats the namespace-scoped List or this filter.
//
// T is the satellite type list holds, inferred from sameParent, so callers read
// their own spec fields off the returned siblings without re-asserting.
//
// A nil reader yields no siblings, so a programmatically constructed webhook
// without a client stays permissive rather than failing closed on a lookup it
// cannot perform. The List error is returned unwrapped, leaving the caller to
// wrap it with the text naming its own rule.
func AttachedSiblings[T client.Object](
	ctx context.Context,
	reader client.Reader,
	self client.Object,
	list client.ObjectList,
	sameParent func(other T) bool,
) ([]T, error) {
	if reader == nil {
		return nil, nil
	}
	if err := reader.List(ctx, list, client.InNamespace(self.GetNamespace())); err != nil {
		return nil, err
	}
	items, err := apimeta.ExtractList(list)
	if err != nil {
		return nil, err
	}

	var attached []T
	for _, item := range items {
		// The !ok arm covers a list whose element type is not T, which only a
		// caller pairing the wrong list with sameParent can produce.
		other, ok := item.(T)
		// self appears in the List on UPDATE. A Terminating sibling is on its
		// way out, and blocking a replacement on it would deadlock
		// recreate-during-teardown.
		if !ok || other.GetName() == self.GetName() || other.GetDeletionTimestamp() != nil {
			continue
		}
		if sameParent(other) {
			attached = append(attached, other)
		}
	}
	return attached, nil
}

// DefaultExtraOptionKeyPattern is the charset an extraOptions key must match.
// oslo.config option names are snake_case, so letters, digits and underscores
// are the whole legitimate charset.
var DefaultExtraOptionKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

// ExtraOptionsRules carries the per-consumer half of the ExtraOptions
// validator: the options the projection owns and an optional extra rule. The
// key charset is not a knob — every consumer enforces
// DefaultExtraOptionKeyPattern, which is what the rejection message names.
type ExtraOptionsRules struct {
	// Denylist maps an option name the projection owns to the spec field or
	// component owning it, which the rejection message names.
	Denylist map[string]string
	// PerKey is an optional rule over the keys that survive the denylist. Its
	// error is appended, and the value is still checked for control
	// characters afterwards.
	PerKey func(key, value string) *field.Error
}

// ExtraOptions rejects the entries in opts a projection cannot accept: an empty
// key, a key outside the charset, a key the projection owns, and a value
// carrying a control character. Keys are validated in sorted order so the
// aggregated error list is deterministic.
//
// The charset check runs before the denylist because the denylist matches exact
// strings and is therefore blind to two key-side attacks: a newline in the key
// injects an arbitrary line through the renderer's verbatim `key = value`
// write, and a variant such as a trailing space is not string-equal to the
// denylisted option, yet oslo.config strips it back to one.
func ExtraOptions(path *field.Path, opts map[string]string, rules ExtraOptionsRules) field.ErrorList {
	if len(opts) == 0 {
		return nil
	}

	var errs field.ErrorList
	for _, k := range slices.Sorted(maps.Keys(opts)) {
		v := opts[k]
		if k == "" {
			errs = append(errs, field.Invalid(path, k, "option name must not be empty"))
			continue
		}
		if !DefaultExtraOptionKeyPattern.MatchString(k) {
			errs = append(errs, field.Invalid(
				path.Key(k), k,
				"option name must match ^[A-Za-z0-9_]+$ (letters, digits, and underscore)",
			))
			continue
		}
		if owner, denied := rules.Denylist[k]; denied {
			errs = append(errs, field.Invalid(
				path.Key(k), v,
				fmt.Sprintf("option %q is owned by %s and must not be set via extraOptions", k, owner),
			))
			continue
		}
		if rules.PerKey != nil {
			if err := rules.PerKey(k, v); err != nil {
				errs = append(errs, err)
			}
		}
		// INI-injection guard: a value with an embedded newline injects
		// arbitrary section lines however innocuous the key is.
		if HasControlChars(v) {
			errs = append(errs, field.Invalid(
				path.Key(k), v,
				"value must not contain newline or carriage-return characters",
			))
		}
	}
	return errs
}
