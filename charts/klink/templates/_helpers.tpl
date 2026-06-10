{{- define "klink.fullname" -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "klink.labels" -}}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{ include "klink.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "klink.selectorLabels" -}}
app.kubernetes.io/name: klink
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "klink.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "klink.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}
