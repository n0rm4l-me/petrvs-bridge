{{- define "hub.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "hub.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := include "hub.name" . -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "hub.labels" -}}
app.kubernetes.io/name: {{ include "hub.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}

{{- define "hub.selectorLabels" -}}
app.kubernetes.io/name: {{ include "hub.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "hub.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "hub.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "hub.agentsConfigMap" -}}
{{- if .Values.agents.existingConfigMap -}}
{{- .Values.agents.existingConfigMap -}}
{{- else -}}
{{- printf "%s-agents" (include "hub.fullname" .) -}}
{{- end -}}
{{- end -}}

{{- define "hub.image" -}}
{{- $repo := required "image.repository is required" .Values.image.repository -}}
{{- if .Values.image.digest -}}
{{- printf "%s@%s" $repo .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" $repo (default .Chart.AppVersion .Values.image.tag) -}}
{{- end -}}
{{- end -}}

{{/*
A chat or user id for the environment, quoted. It must be a string: a number in a values file reaches the template as a
float and would be rendered as 1.23456789e+08 (toString does not help, it prints the float the same way), and the hub would
exit with "is not a number". values.schema.json refuses it first; this is the second line, for --skip-schema-validation.
Call with (list "hub.chatId" .Values.hub.chatId).
*/}}
{{- define "hub.id" -}}
{{- $name := index . 0 -}}
{{- $v := index . 1 -}}
{{- if not (kindIs "string" $v) -}}
{{- fail (printf "%s must be a quoted string such as \"123456789\" (a number is rendered as 1.23456789e+08); with --set use --set-string" $name) -}}
{{- end -}}
{{- $v | toString | quote -}}
{{- end -}}
