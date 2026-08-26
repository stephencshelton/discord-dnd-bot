{{/*
Expand the name of the chart.
*/}}
{{- define "discord-dnd-bot.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Fully qualified app name.
*/}}
{{- define "discord-dnd-bot.fullname" -}}
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

{{- define "discord-dnd-bot.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "discord-dnd-bot.labels" -}}
helm.sh/chart: {{ include "discord-dnd-bot.chart" . }}
{{ include "discord-dnd-bot.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: discord-dnd-bot
{{- end }}

{{/*
Selector labels (chart-wide).
*/}}
{{- define "discord-dnd-bot.selectorLabels" -}}
app.kubernetes.io/name: {{ include "discord-dnd-bot.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Per-component selector labels. Call with (dict "root" . "component" "gateway").
*/}}
{{- define "discord-dnd-bot.componentSelectorLabels" -}}
{{ include "discord-dnd-bot.selectorLabels" .root }}
app.kubernetes.io/component: {{ .component }}
{{- end }}

{{/*
ServiceAccount name.
*/}}
{{- define "discord-dnd-bot.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "discord-dnd-bot.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
ARN of the IAM role created by the ACK IAM controller for S3/IRSA access.
Used to annotate the ServiceAccount when s3 + s3.iam are enabled.
*/}}
{{- define "discord-dnd-bot.s3RoleArn" -}}
{{- printf "arn:aws:iam::%v:role/%s-s3" .Values.aws.accountId (include "discord-dnd-bot.fullname" .) }}
{{- end }}

{{/*
Name of the Secret holding sensitive env. Uses existingSecret when provided.
*/}}
{{- define "discord-dnd-bot.secretName" -}}
{{- if .Values.secrets.existingSecret }}
{{- .Values.secrets.existingSecret }}
{{- else if and .Values.secrets.externalSecret.enabled .Values.secrets.externalSecret.target.name }}
{{- .Values.secrets.externalSecret.target.name }}
{{- else }}
{{- printf "%s-secrets" (include "discord-dnd-bot.fullname" .) }}
{{- end }}
{{- end }}

{{/*
ConfigMap name.
*/}}
{{- define "discord-dnd-bot.configMapName" -}}
{{- printf "%s-config" (include "discord-dnd-bot.fullname" .) }}
{{- end }}

{{/*
Resolve the effective image ref for a component.
Call with (dict "root" . "svc" .Values.gateway "default" "gateway").
*/}}
{{- define "discord-dnd-bot.image" -}}
{{- $repo := .svc.image.repository | default .root.Values.image.repository -}}
{{- $tag := .svc.image.tag | default (printf "%s-%s" .default (.root.Values.image.tag | default .root.Chart.AppVersion)) -}}
{{- printf "%s:%s" $repo $tag -}}
{{- end }}
