// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package validation

import (
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/robfig/cron/v3"
	corev1 "k8s.io/api/core/v1"
	metav1validation "k8s.io/apimachinery/pkg/apis/meta/v1/validation"
	k8svalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/ptr"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

// FuzzCronSchedule checks that CronSchedule reports at most one error at the
// schedule's path, rejects every schedule naming TZ, and admits exactly the
// other schedules cron.ParseStandard parses.
func FuzzCronSchedule(f *testing.F) {
	// The schedules of TestCronSchedule.
	for _, schedule := range []string{
		"0 0 * * 0", "@hourly", "not-a-cron", "", "CRON_TZ=UTC 0 0 * * *", "TZ=Europe/Berlin @daily",
		"*/5 * * * *",
	} {
		f.Add(schedule)
	}
	// A TZ= prefix without a space, on which cron.ParseStandard panics.
	f.Add("TZ=")

	f.Fuzz(func(t *testing.T, schedule string) {
		errs := CronSchedule(testPath, schedule)
		if again := CronSchedule(testPath, schedule); !reflect.DeepEqual(again, errs) {
			t.Fatalf("determinism: CronSchedule(%q) returned %v, then %v", schedule, errs, again)
		}
		if len(errs) > 1 {
			t.Fatalf("one error: CronSchedule(%q) returned %d errors: %v", schedule, len(errs), errs)
		}
		for _, e := range errs {
			if e.Field != testPath.String() {
				t.Fatalf("error path: CronSchedule(%q) reported %q, want %q", schedule, e.Field, testPath.String())
			}
		}
		if strings.Contains(schedule, "TZ") &&
			(len(errs) != 1 || !strings.HasPrefix(errs[0].Detail, "TZ and CRON_TZ are not allowed")) {
			t.Fatalf("time zone: CronSchedule(%q) = %v, want the TZ rejection", schedule, errs)
		}
		// cron.ParseStandard panics on a TZ= prefix without a space, so only
		// the schedules CronSchedule parses reach it.
		if !strings.Contains(schedule, "TZ") {
			if _, perr := cron.ParseStandard(schedule); (len(errs) == 0) != (perr == nil) {
				t.Fatalf("cron parser: CronSchedule(%q) = %v, but cron.ParseStandard returned %v", schedule, errs, perr)
			}
		}
	})
}

// FuzzNodeSelectorLabels checks that NodeSelectorLabels reports at the
// selector's path or at the label's key, and admits exactly the labels the API
// server's own label validation admits in the rendered pod template.
func FuzzNodeSelectorLabels(f *testing.F) {
	// The labels of TestNodeSelectorLabels, then empty and overlong ones.
	f.Add("kubernetes.io/os", "linux")
	f.Add("bad key", "x")
	f.Add("a", "-bad-")
	f.Add("", "")
	f.Add("a", "")
	f.Add(strings.Repeat("a", 64), "v")

	f.Fuzz(func(t *testing.T, key, value string) {
		labels := map[string]string{key: value}
		errs := NodeSelectorLabels(testPath, labels)
		if again := NodeSelectorLabels(testPath, labels); !reflect.DeepEqual(again, errs) {
			t.Fatalf("determinism: NodeSelectorLabels(%q) returned %v, then %v", labels, errs, again)
		}
		for _, e := range errs {
			if e.Field != testPath.String() && e.Field != testPath.Key(key).String() {
				t.Fatalf("error path: NodeSelectorLabels(%q) reported %q, want %q or %q",
					labels, e.Field, testPath.String(), testPath.Key(key).String())
			}
		}
		if apiErrs := metav1validation.ValidateLabels(labels, testPath); (len(errs) == 0) != (len(apiErrs) == 0) {
			t.Fatalf("API server labels: NodeSelectorLabels(%q) = %v, but ValidateLabels = %v", labels, errs, apiErrs)
		}
	})
}

// FuzzTolerations checks that Tolerations reports Invalid or NotSupported
// errors under the toleration's index, and admits exactly the tolerations that
// pass the API server's toleration rules, which the oracle below restates.
func FuzzTolerations(f *testing.F) {
	path := field.NewPath("spec", "deployment", "tolerations")
	if errs := Tolerations(path, nil); len(errs) != 0 {
		f.Fatalf("no tolerations: Tolerations(nil) = %v, want none", errs)
	}
	if errs := Tolerations(path, []corev1.Toleration{}); len(errs) != 0 {
		f.Fatalf("no tolerations: Tolerations([]) = %v, want none", errs)
	}

	// The accepted rows of TestTolerations.
	f.Add("a", "Exists", "", "NoSchedule", int64(0), false)
	f.Add("", "Exists", "", "", int64(0), false)
	f.Add("a", "Equal", "b", "NoExecute", int64(30), true)
	f.Add("a", "Gt", "5", "", int64(0), false)
	f.Add("a", "Lt", "-3", "", int64(0), false)
	f.Add("a", "", "b", "", int64(0), false)
	// The rejected rows of TestTolerations.
	f.Add("bad key", "Exists", "", "", int64(0), false)
	f.Add("", "Equal", "", "", int64(0), false)
	f.Add("a", "Exists", "b", "", int64(0), false)
	f.Add("a", "Foo", "", "", int64(0), false)
	f.Add("a", "Exists", "", "Bad", int64(0), false)
	f.Add("a", "Exists", "", "NoSchedule", int64(30), true)
	f.Add("a", "Equal", "-bad-", "", int64(0), false)
	f.Add("a", "Gt", "high", "", int64(0), false)
	f.Add("a", "Lt", "", "", int64(0), false)

	f.Fuzz(func(t *testing.T, key, operator, value, effect string, seconds int64, withSeconds bool) {
		tol := corev1.Toleration{
			Key:      key,
			Operator: corev1.TolerationOperator(operator),
			Value:    value,
			Effect:   corev1.TaintEffect(effect),
		}
		if withSeconds {
			tol.TolerationSeconds = ptr.To(seconds)
		}
		tolerations := []corev1.Toleration{tol}
		errs := Tolerations(path, tolerations)
		if again := Tolerations(path, tolerations); !reflect.DeepEqual(again, errs) {
			t.Fatalf("determinism: Tolerations(%+v) returned %v, then %v", tol, errs, again)
		}
		prefix := path.Index(0).String()
		for _, e := range errs {
			if !strings.HasPrefix(e.Field, prefix) {
				t.Fatalf("error path: Tolerations(%+v) reported %q, want a path under %q", tol, e.Field, prefix)
			}
			if e.Type != field.ErrorTypeInvalid && e.Type != field.ErrorTypeNotSupported {
				t.Fatalf("error type: Tolerations(%+v) reported %q, want Invalid or NotSupported", tol, e.Type)
			}
		}
		_, intErr := strconv.ParseInt(value, 10, 64)
		admitted := slices.Contains([]string{"", "Equal", "Exists", "Lt", "Gt"}, operator) &&
			slices.Contains([]string{"", "NoSchedule", "PreferNoSchedule", "NoExecute"}, effect) &&
			(!withSeconds || effect == "NoExecute") &&
			(key != "" || operator == "Exists") &&
			(key == "" || len(k8svalidation.IsQualifiedName(key)) == 0) &&
			(operator != "Exists" || value == "") &&
			((operator != "" && operator != "Equal") || len(k8svalidation.IsValidLabelValue(value)) == 0) &&
			((operator != "Lt" && operator != "Gt") || intErr == nil)
		if (len(errs) == 0) != admitted {
			t.Fatalf("API server tolerations: Tolerations(%+v) = %v, want admitted %t", tol, errs, admitted)
		}
	})
}

// FuzzExtraOptions checks that ExtraOptions reports at most one error per
// option, admits exactly the options whose key matches the charset, is not
// denylisted and whose value carries no line break, and reports a rejection at
// the option's key (or at the map's path for an empty key).
func FuzzExtraOptions(f *testing.F) {
	rules := ExtraOptionsRules{Denylist: map[string]string{"denied": "spec.owner"}}
	// The options of TestExtraOptions, then a carriage return in a value.
	f.Add("", "x")
	f.Add("bad key", "x")
	f.Add("denied", "x")
	f.Add("ok", "a\nb")
	f.Add("with-dash", "x")
	f.Add("s3_store_host", "x")
	f.Add("ok", "a\rb")

	f.Fuzz(func(t *testing.T, key, value string) {
		opts := map[string]string{key: value}
		errs := ExtraOptions(testPath, opts, rules)
		if again := ExtraOptions(testPath, opts, rules); !reflect.DeepEqual(again, errs) {
			t.Fatalf("determinism: ExtraOptions(%q) returned %v, then %v", opts, errs, again)
		}
		if len(errs) > 1 {
			t.Fatalf("one error: ExtraOptions(%q) returned %d errors: %v", opts, len(errs), errs)
		}
		_, denied := rules.Denylist[key]
		admitted := DefaultExtraOptionKeyPattern.MatchString(key) && !denied && !HasControlChars(value)
		if (len(errs) == 0) != admitted {
			t.Fatalf("admission: ExtraOptions(%q) = %v, want admitted %t", opts, errs, admitted)
		}
		if len(errs) == 1 {
			want := testPath.Key(key).String()
			if key == "" {
				want = testPath.String()
			}
			if errs[0].Field != want {
				t.Fatalf("error path: ExtraOptions(%q) reported %q, want %q", opts, errs[0].Field, want)
			}
		}
	})
}

// FuzzCacheNoControlChars checks that CacheNoControlChars reports one error
// for each of clusterRef.name and servers[0] that carries a line break, at
// that field's path, and none otherwise.
func FuzzCacheNoControlChars(f *testing.F) {
	if errs := CacheNoControlChars(testPath, &commonv1.CacheSpec{}); len(errs) != 0 {
		f.Fatalf("empty cache: CacheNoControlChars(CacheSpec{}) = %v, want none", errs)
	}

	// The fields of TestCacheNoControlChars, then empty ones.
	f.Add("mc", "mc-0:11211")
	f.Add("mc", "mc-0:11211\nauth_url = http://attacker.example/v3")
	f.Add("mc", "mc-1:11211\rregion_name = elsewhere")
	f.Add("mc\nauth_url = http://attacker.example/v3", "mc-0:11211")
	f.Add("", "")

	f.Fuzz(func(t *testing.T, name, server string) {
		cache := commonv1.CacheSpec{
			ClusterRef: &corev1.LocalObjectReference{Name: name},
			Servers:    []string{server},
		}
		errs := CacheNoControlChars(testPath, &cache)
		if again := CacheNoControlChars(testPath, &cache); !reflect.DeepEqual(again, errs) {
			t.Fatalf("determinism: CacheNoControlChars(%q, %q) returned %v, then %v", name, server, errs, again)
		}
		var want, got []string
		if HasControlChars(name) {
			want = append(want, testPath.Child("clusterRef", "name").String())
		}
		if HasControlChars(server) {
			want = append(want, testPath.Child("servers").Index(0).String())
		}
		for _, e := range errs {
			got = append(got, e.Field)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("error paths: CacheNoControlChars(%q, %q) reported %q, want %q", name, server, got, want)
		}
	})
}
