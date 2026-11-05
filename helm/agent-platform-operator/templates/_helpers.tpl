{{/* vim: set filetype=mustache: */}}
{{/*
Expand the name of the chart.
*/}}
{{- define "name" -}}
{{- .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Create chart name and version as used by the chart label. A label value is at
most 63 characters and begins and ends alphanumeric: the cut of a long version
(a branch build's <version>-dev.<branch>.<date>.<time>.<sha>, or the
<version>+<digest> helm-controller installs) can land on any run of ".", "_"
(from "+") and "-", so the whole run is trimmed.
*/}}
{{- define "chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimAll "-._" -}}
{{- end -}}

{{/*
Common labels
*/}}
{{- define "labels.common" -}}
app: {{ include "name" . | quote }}
{{ include "labels.selector" . }}
app.kubernetes.io/managed-by: {{ .Release.Service | quote }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
application.giantswarm.io/team: {{ index .Chart.Annotations "io.giantswarm.application.team" | quote }}
helm.sh/chart: {{ include "chart" . | quote }}
{{- end -}}

{{/*
Selector labels
*/}}
{{- define "labels.selector" -}}
app.kubernetes.io/name: {{ include "name" . | quote }}
app.kubernetes.io/instance: {{ .Release.Name | quote }}
{{- end -}}

{{/*
The network policy resource the operator copies, as {"group": ..., "resource": ...};
empty without a flavor or without names.
*/}}
{{- define "networkPolicyResource" -}}
{{- $flavor := .Values.networkPolicy.flavor -}}
{{- if and $flavor .Values.networkPolicy.names -}}
{{- if eq $flavor "kubernetes" -}}
{{- dict "group" "networking.k8s.io" "resource" "networkpolicies" | toJson -}}
{{- else if eq $flavor "cilium" -}}
{{- dict "group" "cilium.io" "resource" "ciliumnetworkpolicies" | toJson -}}
{{- else -}}
{{- fail (printf "networkPolicy.flavor %q: kubernetes, cilium or empty" $flavor) -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
secretReaders is the --secret-readers list:
<clusterrole>=<namespace>/<serviceaccount>, comma-separated.
*/}}
{{- define "secretReaders" -}}
{{- $out := list }}
{{- range .Values.secretReaders }}
{{- $out = append $out (printf "%s=%s/%s" .clusterRole .serviceAccount.namespace .serviceAccount.name) }}
{{- end }}
{{- join "," $out }}
{{- end }}
