{{/*
Operator Deployment skeleton shared by the operator charts.

Consuming charts render it with a one-line template that passes the root context:

    {{- include "operator-library.deployment" . }}

All settings (image, replicas, resources, webhook, leaderElection, metrics,
rbac.namespaceScoped) are read from the consuming chart's .Values.
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
      containers:
        - name: manager
          image: "{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}"
          imagePullPolicy: {{ .Values.image.pullPolicy }}
          args:
            {{- if .Values.leaderElection.enabled }}
            - --leader-elect
            {{- end }}
            {{- if .Values.rbac.namespaceScoped }}
            - --namespace={{ .Release.Namespace }}
            {{- end }}
            {{- if not .Values.webhook.enabled }}
            - --enable-webhooks=false
            {{- end }}
            - --metrics-bind-address=:{{ .Values.metrics.port }}
            - --health-probe-bind-address=:8081
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
