{{/*
Operator Deployment skeleton shared by the operator charts.

Consuming charts render it with a one-line template that passes the root context:

    {{- include "operator-library.deployment" . }}

All settings (image, replicas, resources, webhook, leaderElection, metrics,
rbac.namespaceScoped, logging, extraArgs, extraEnv) are read from the consuming
chart's .Values. What only the chart knows — an operator-specific flag derived
from its own values, an environment variable — comes through the
"operator-library.chart.args" and "operator-library.chart.env" hooks (see
_helpers.tpl), so the library names no operator.
*/}}
{{- define "operator-library.deployment" -}}
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ include "operator-library.fullname" . }}
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "operator-library.labels" . | nindent 4 }}
spec:
  replicas: {{ .Values.replicas }}
  selector:
    matchLabels:
      {{- include "operator-library.selectorLabels" . | nindent 6 }}
  template:
    metadata:
      labels:
        {{- include "operator-library.selectorLabels" . | nindent 8 }}
    spec:
      serviceAccountName: {{ include "operator-library.serviceAccountName" . }}
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532
        runAsGroup: 65532
        fsGroup: 65532
        seccompProfile:
          type: RuntimeDefault
      # Best-effort spread across nodes so a single node drain cannot evict
      # every replica — and with it the in-process admission webhook — at once.
      # ScheduleAnyway keeps single-node (kind) clusters schedulable.
      topologySpreadConstraints:
        - maxSkew: 1
          topologyKey: kubernetes.io/hostname
          whenUnsatisfiable: ScheduleAnyway
          labelSelector:
            matchLabels:
              {{- include "operator-library.selectorLabels" . | nindent 14 }}
      containers:
        - name: manager
          image: "{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}{{ with .Values.image.digest }}@{{ . }}{{ end }}"
          imagePullPolicy: {{ .Values.image.pullPolicy }}
          args:
            {{- if .Values.leaderElection.enabled }}
            - --leader-elect
            {{- end }}
            {{- if .Values.rbac.namespaceScoped }}
            - --namespace={{ .Release.Namespace }}
            # Target clusters off: a namespace-scoped install renders a Role in
            # the release namespace and nothing anywhere else, so the operator
            # may not read the registration Secrets in the clusters namespace.
            # Left at its default the Secret informer would be widened to a
            # namespace the operator has no grant for, the cache would never
            # sync, and the manager would fail to start.
            - --clusters-namespace=
            {{- end }}
            {{- if not .Values.webhook.enabled }}
            - --enable-webhooks=false
            {{- end }}
            {{- with .Values.controller }}
            {{- with .maxConcurrentReconciles }}
            - --max-concurrent-reconciles={{ . }}
            {{- end }}
            {{- end }}
            {{- with include "operator-library.chart.args" . }}
            {{- . | trim | nindent 12 }}
            {{- end }}
            - --metrics-bind-address=:{{ .Values.metrics.port }}
            - --health-probe-bind-address=:8081
            {{- if .Values.logging.development }}
            - --zap-devel=true
            {{- end }}
            {{- with .Values.logging.level }}
            - --zap-log-level={{ . }}
            {{- end }}
            {{- with .Values.logging.encoder }}
            - --zap-encoder={{ . }}
            {{- end }}
            {{- with .Values.extraArgs }}
            {{- toYaml . | nindent 12 }}
            {{- end }}
          {{- $chartEnv := include "operator-library.chart.env" . | trim }}
          {{- if or $chartEnv .Values.extraEnv }}
          env:
            {{- with $chartEnv }}
            {{- . | nindent 12 }}
            {{- end }}
            {{- with .Values.extraEnv }}
            {{- toYaml . | nindent 12 }}
            {{- end }}
          {{- end }}
          ports:
            - name: metrics
              containerPort: {{ .Values.metrics.port }}
              protocol: TCP
            - name: health
              containerPort: 8081
              protocol: TCP
            {{- if .Values.webhook.enabled }}
            - name: webhook
              containerPort: 9443
              protocol: TCP
            {{- end }}
          livenessProbe:
            httpGet:
              path: /healthz
              port: 8081
          readinessProbe:
            httpGet:
              path: /readyz
              port: 8081
          resources:
            {{- toYaml .Values.resources | nindent 12 }}
          {{- if .Values.webhook.enabled }}
          volumeMounts:
            - name: webhook-certs
              mountPath: /tmp/k8s-webhook-server/serving-certs
              readOnly: true
          {{- end }}
          securityContext:
            allowPrivilegeEscalation: false
            capabilities:
              drop:
                - ALL
            readOnlyRootFilesystem: true
            seccompProfile:
              type: RuntimeDefault
      {{- if .Values.webhook.enabled }}
      volumes:
        - name: webhook-certs
          secret:
            secretName: {{ include "operator-library.fullname" . }}-webhook-cert
      {{- end }}
{{- end }}
