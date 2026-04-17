{{/*
Common template helpers for the opencost-ai chart.

Naming follows the standard Helm idiom: a release-scoped fullname,
shortened to 63 characters, plus per-component suffixes so the three
sub-components (gateway, bridge, ollama) get distinct resource names
inside one release.
*/}}

{{- define "opencost-ai.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "opencost-ai.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "opencost-ai.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Per-component names. Using distinct helpers (rather than passing a
component string through a single template) keeps the call sites in
each manifest easy to read.
*/}}
{{- define "opencost-ai.gateway.fullname" -}}
{{- printf "%s-gateway" (include "opencost-ai.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "opencost-ai.bridge.fullname" -}}
{{- printf "%s-bridge" (include "opencost-ai.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "opencost-ai.ollama.fullname" -}}
{{- printf "%s-ollama" (include "opencost-ai.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Common metadata labels. app.kubernetes.io/component is filled in at
the call site so each template gets a distinct component label.
*/}}
{{- define "opencost-ai.labels" -}}
helm.sh/chart: {{ include "opencost-ai.chart" . }}
app.kubernetes.io/name: {{ include "opencost-ai.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: opencost-ai
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{- define "opencost-ai.gateway.selectorLabels" -}}
app.kubernetes.io/name: {{ include "opencost-ai.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: gateway
{{- end -}}

{{- define "opencost-ai.bridge.selectorLabels" -}}
app.kubernetes.io/name: {{ include "opencost-ai.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: bridge
{{- end -}}

{{- define "opencost-ai.ollama.selectorLabels" -}}
app.kubernetes.io/name: {{ include "opencost-ai.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: ollama
{{- end -}}

{{/*
Image reference helper. Pins by digest if one is provided, otherwise
falls back to the tag (which itself defaults to .Chart.AppVersion for
the gateway image when gateway.image.tag is empty).
*/}}
{{- define "opencost-ai.image" -}}
{{- $repo := .image.repository -}}
{{- $tag := default .defaultTag .image.tag -}}
{{- if .image.digest -}}
{{- printf "%s@%s" $repo .image.digest -}}
{{- else -}}
{{- printf "%s:%s" $repo $tag -}}
{{- end -}}
{{- end -}}

{{/*
Name of the Secret that holds the gateway bearer token. Honours
auth.existingSecret when set; otherwise derives from the gateway
fullname. Used both by the Deployment (mount) and the Secret template
(conditional rendering).
*/}}
{{- define "opencost-ai.gateway.authSecretName" -}}
{{- if .Values.gateway.auth.existingSecret -}}
{{- .Values.gateway.auth.existingSecret -}}
{{- else -}}
{{- printf "%s-auth" (include "opencost-ai.gateway.fullname" .) -}}
{{- end -}}
{{- end -}}

{{/*
Ollama service URL. Used by the bridge Deployment env to reach Ollama.
*/}}
{{- define "opencost-ai.ollama.url" -}}
{{- printf "http://%s:%d" (include "opencost-ai.ollama.fullname" .) (int .Values.ollama.service.port) -}}
{{- end -}}

{{/*
Bridge service URL. Used by the gateway Deployment env.
*/}}
{{- define "opencost-ai.bridge.url" -}}
{{- printf "http://%s:%d" (include "opencost-ai.bridge.fullname" .) (int .Values.bridge.service.port) -}}
{{- end -}}
