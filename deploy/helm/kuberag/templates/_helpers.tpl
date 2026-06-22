{{/*
Expand the name of the chart.
*/}}
{{- define "kuberag.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "kuberag.sloRules" -}}
- name: kuberag-retriever-slo
  rules:
    - alert: KuberagRetrieverHighErrorRate
      expr: |
        sum by (namespace, service) (
          rate(kuberag_retriever_queries_total{result=~"error|generation_error"}[5m])
        )
        /
        clamp_min(
          sum by (namespace, service) (rate(kuberag_retriever_queries_total[5m])),
          0.001
        )
        > 0.05
      for: 10m
      labels:
        severity: warning
      annotations:
        summary: Retriever error rate is above 5%
    - alert: KuberagRetrieverHighP99Latency
      expr: |
        histogram_quantile(
          0.99,
          sum by (namespace, service, le) (
            rate(kuberag_retriever_query_duration_seconds_bucket[5m])
          )
        ) > 2
      for: 10m
      labels:
        severity: warning
      annotations:
        summary: Retriever p99 latency is above 2 seconds
    - alert: KuberagRetrieverSaturated
      expr: |
        max_over_time(kuberag_retriever_queries_in_flight[5m])
        /
        clamp_min(max_over_time(kuberag_retriever_concurrency_limit[5m]), 1)
        > 0.9
      for: 10m
      labels:
        severity: warning
      annotations:
        summary: Retriever concurrency is above 90%
    - alert: KuberagRetrieverRejectingTraffic
      expr: |
        sum by (namespace, service, reason) (
          rate(kuberag_retriever_rejected_requests_total{reason=~"rate_limit|rate_limit_backend|concurrency"}[5m])
        ) > 1
      for: 10m
      labels:
        severity: warning
      annotations:
        summary: Retriever is rejecting more than one request per second
    - alert: KuberagControllerReconcileErrors
      expr: sum(rate(controller_runtime_reconcile_errors_total[5m])) > 0
      for: 15m
      labels:
        severity: warning
      annotations:
        summary: kuberag controller reconciliation is failing
    - alert: KuberagIngestionStale
      expr: |
        time() - rag_knowledgebase_last_successful_ingestion_timestamp_seconds
        > {{ .Values.metrics.prometheusRule.ingestionFreshnessSeconds }}
      for: 15m
      labels:
        severity: warning
      annotations:
        summary: No successful KnowledgeBase ingestion within the freshness objective
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "kuberag.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "kuberag.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "kuberag.labels" -}}
helm.sh/chart: {{ include "kuberag.chart" . }}
{{ include "kuberag.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels.
*/}}
{{- define "kuberag.selectorLabels" -}}
app.kubernetes.io/name: kuberag
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
ServiceAccount name.
*/}}
{{- define "kuberag.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "kuberag.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}
