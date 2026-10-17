// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package testutil

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/yaml"
)

// chartEgressHook opens the operator-library hook through which an operator
// chart appends egress rules to the shared operator NetworkPolicy.
const chartEgressHook = `{{- define "operator-library.chart.networkPolicyEgress" -}}`

// The template actions that open and close a block inside the hook.
var (
	blockAction = regexp.MustCompile(`^\{\{-?\s*(define|if|with|range|block)\b`)
	endAction   = regexp.MustCompile(`^\{\{-?\s*end\b`)
)

// ChartEgressAdmits reports whether the egress rules the chart in chartDir
// appends to its operator NetworkPolicy admit TCP traffic to port on a pod
// carrying podLabels, in any namespace. It reads the
// operator-library.chart.networkPolicyEgress hook from the chart's
// templates/_helpers.tpl and drops the hook's template actions, so a rule a
// value gates counts as rendered. It ties the rule the chart hard-codes to the
// labels and port the operator gives the pods it calls.
func ChartEgressAdmits(t testing.TB, chartDir string, podLabels map[string]string, port int32) bool {
	t.Helper()
	for _, rule := range chartEgressRules(t, chartDir) {
		peers := len(rule.To) == 0 || slices.ContainsFunc(rule.To, func(peer networkingv1.NetworkPolicyPeer) bool {
			return peerSelects(t, peer, podLabels)
		})
		ports := len(rule.Ports) == 0 || slices.ContainsFunc(rule.Ports, func(p networkingv1.NetworkPolicyPort) bool {
			return (p.Protocol == nil || *p.Protocol == corev1.ProtocolTCP) && p.Port != nil && p.Port.IntValue() == int(port)
		})
		if peers && ports {
			return true
		}
	}
	return false
}

// chartEgressRules parses the egress rules of the chart's hook.
func chartEgressRules(t testing.TB, chartDir string) []networkingv1.NetworkPolicyEgressRule {
	t.Helper()
	path := filepath.Join(chartDir, "templates", "_helpers.tpl")
	data, err := fs.ReadFile(os.DirFS(chartDir), "templates/_helpers.tpl")
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	lines := strings.Split(string(data), "\n")
	start := slices.Index(lines, chartEgressHook)
	if start < 0 {
		t.Fatalf("%s does not define the operator-library.chart.networkPolicyEgress hook", path)
	}

	var body []string
	depth := 1
	for _, line := range lines[start+1:] {
		action := strings.TrimSpace(line)
		switch {
		case blockAction.MatchString(action):
			depth++
		case endAction.MatchString(action):
			depth--
		case !strings.HasPrefix(action, "{{"):
			body = append(body, line)
		}
		if depth == 0 {
			break
		}
	}
	if depth != 0 {
		t.Fatalf("%s does not close the operator-library.chart.networkPolicyEgress hook", path)
	}

	var rules []networkingv1.NetworkPolicyEgressRule
	if err := yaml.UnmarshalStrict([]byte(strings.Join(body, "\n")), &rules); err != nil {
		t.Fatalf("parsing the egress rules of %s: %v", path, err)
	}
	return rules
}

// peerSelects reports whether peer selects a pod carrying podLabels in every
// namespace.
func peerSelects(t testing.TB, peer networkingv1.NetworkPolicyPeer, podLabels map[string]string) bool {
	t.Helper()
	if peer.IPBlock != nil || peer.NamespaceSelector == nil {
		return false
	}
	namespaces, err := metav1.LabelSelectorAsSelector(peer.NamespaceSelector)
	if err != nil {
		t.Fatalf("parsing namespaceSelector %v: %v", peer.NamespaceSelector, err)
	}
	if !namespaces.Empty() {
		return false
	}
	if peer.PodSelector == nil {
		return true
	}
	pods, err := metav1.LabelSelectorAsSelector(peer.PodSelector)
	if err != nil {
		t.Fatalf("parsing podSelector %v: %v", peer.PodSelector, err)
	}
	return pods.Matches(labels.Set(podLabels))
}
