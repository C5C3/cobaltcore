{{/*
Shared NetworkPolicy template for the operator charts. Defined once here and
included by each operator chart's templates/networkpolicy.yaml with the
consuming chart's root context, so .Chart/.Release/.Values resolve against the
operator chart. A chart that ships no networkpolicy.yaml renders none.

Chart-specific egress rules come from the "operator-library.chart.networkPolicyEgress"
hook (see _helpers.tpl): the library defines it empty, and a chart that needs
an extra egress rule (barbican-operator's OpenBao port) overrides it.
*/}}
{{- define "operator-library.networkPolicy" -}}
{{/*
  NetworkPolicy for the operator pod.

  Default-deny both directions for pods matching the operator selector, then
  open explicit rules for:
    - Egress to kube-apiserver (required; configured via networkPolicy.kubeApiServer)
    - Egress to DNS (UDP+TCP/53; configured via networkPolicy.dns)
    - Chart-specific egress from the operator-library.chart.networkPolicyEgress hook
    - Ingress to the webhook port 9443 from API server CIDRs (when webhook.enabled)
    - Ingress to the metrics port from explicit peers (when networkPolicy.allowMetricsFrom is non-empty)

  Health probe (8081): Kubelet calls liveness/readiness probes from the host
  network namespace of the node on which the pod is scheduled. These probes
  are NOT subject to NetworkPolicy rules in standard CNI implementations
  (Calico, Cilium, Antrea) — see https://kubernetes.io/docs/concepts/services-networking/network-policies/#what-you-can-t-do-with-network-policies-at-least-not-yet.
  Therefore, no explicit ingress rule is rendered for port 8081.
*/}}
{{- if .Values.networkPolicy.enabled }}
{{- /*
  Fail-closed guards. The JSON schema (values.schema.json)
  already enforces minItems: 1 on both lists when enabled=true, but schema
  validation can be bypassed (e.g. --skip-schema-validation). Mirror the
  fail-closed path of internal/common/networkpolicy: an egress rule with an
  empty ports list would open ALL ports to the peer, and an empty CIDR list
  would render a rule with no destinations. Refuse rather than render an
  unsafe policy.
*/}}
{{- if not .Values.networkPolicy.kubeApiServer.cidrs }}
{{- fail "networkPolicy.kubeApiServer.cidrs must not be empty when networkPolicy.enabled=true: refusing to render a NetworkPolicy that would block all kube-apiserver egress" }}
{{- end }}
{{- if not .Values.networkPolicy.kubeApiServer.ports }}
{{- fail "networkPolicy.kubeApiServer.ports must not be empty when networkPolicy.enabled=true: refusing to render a NetworkPolicy that would open all ports to kube-apiserver" }}
{{- end }}
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: {{ include "operator-library.fullname" . }}
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "operator-library.labels" . | nindent 4 }}
spec:
  podSelector:
    matchLabels:
      {{- include "operator-library.selectorLabels" . | nindent 6 }}
  policyTypes:
    - Ingress
    - Egress
  egress:
    {{- /* DNS egress — UDP+TCP 53 to the configured DNS peer. */}}
    {{- if .Values.networkPolicy.dns.enabled }}
    - to:
        - namespaceSelector:
            matchLabels:
              {{- toYaml .Values.networkPolicy.dns.namespaceSelector | nindent 14 }}
          podSelector:
            matchLabels:
              {{- toYaml .Values.networkPolicy.dns.podSelector | nindent 14 }}
      ports:
        - protocol: UDP
          port: 53
        - protocol: TCP
          port: 53
    {{- end }}
    {{- /*
      kube-apiserver egress — single rule combining all configured CIDRs and
      ports. NetworkPolicy semantics: within one rule, the permitted traffic is
      the cartesian product of `to` peers and `ports`, so one rule with N CIDRs
      and M ports covers all N*M tuples.
    */}}
    - to:
        {{- range .Values.networkPolicy.kubeApiServer.cidrs }}
        - ipBlock:
            cidr: {{ . | quote }}
        {{- end }}
      ports:
        {{- range .Values.networkPolicy.kubeApiServer.ports }}
        - protocol: TCP
          port: {{ . }}
        {{- end }}
    {{- /* Chart-specific egress rules, if the chart overrides the hook. */}}
    {{- with include "operator-library.chart.networkPolicyEgress" . }}
    {{- . | trim | nindent 4 }}
    {{- end }}
  ingress:
    {{- /*
      Webhook ingress — TCP 9443 from webhook clients (API server).
      Falls back to kubeApiServer.cidrs when webhookClients.cidrs is empty,
      since the API server is the sole caller of admission webhooks.
    */}}
    {{- if .Values.webhook.enabled }}
    {{- $webhookCIDRs := .Values.networkPolicy.webhookClients.cidrs }}
    {{- if not $webhookCIDRs }}
    {{- $webhookCIDRs = .Values.networkPolicy.kubeApiServer.cidrs }}
    {{- end }}
    - from:
        {{- range $webhookCIDRs }}
        - ipBlock:
            cidr: {{ . | quote }}
        {{- end }}
      ports:
        - protocol: TCP
          port: 9443
    {{- end }}
    {{- /*
      Metrics ingress — opt-in. Each entry in allowMetricsFrom is rendered
      verbatim as a NetworkPolicyPeer. When the list is empty, no rule is
      emitted and the metrics port is unreachable from outside the pod.
    */}}
    {{- if .Values.networkPolicy.allowMetricsFrom }}
    - from:
        {{- toYaml .Values.networkPolicy.allowMetricsFrom | nindent 8 }}
      ports:
        - protocol: TCP
          port: {{ .Values.metrics.port }}
    {{- end }}
{{- end }}
{{- end }}
