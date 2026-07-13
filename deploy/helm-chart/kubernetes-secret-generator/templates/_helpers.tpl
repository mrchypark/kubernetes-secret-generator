{{- define "kubernetes-secret-generator.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "kubernetes-secret-generator.fullname" -}}
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

{{- define "kubernetes-secret-generator.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "kubernetes-secret-generator.selectorLabels" -}}
name: {{ include "kubernetes-secret-generator.name" . }}
app.kubernetes.io/name: {{ include "kubernetes-secret-generator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "kubernetes-secret-generator.labels" -}}
helm.sh/chart: {{ include "kubernetes-secret-generator.chart" . }}
{{ include "kubernetes-secret-generator.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "kubernetes-secret-generator.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "kubernetes-secret-generator.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- required "serviceAccount.name is required when serviceAccount.create=false" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "kubernetes-secret-generator.rbacName" -}}
{{- printf "%s-%s" (include "kubernetes-secret-generator.fullname" .) .Release.Namespace | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "kubernetes-secret-generator.watchNamespace" -}}
{{- if eq .Values.scope.mode "ownNamespace" -}}
{{- .Release.Namespace -}}
{{- else if eq .Values.scope.mode "namespaces" -}}
{{- join "," (sortAlpha .Values.scope.namespaces) -}}
{{- else -}}
{{- "" -}}
{{- end -}}
{{- end -}}

{{- define "kubernetes-secret-generator.image" -}}
{{- $registry := default .Values.image.registry .Values.global.imageRegistry -}}
{{- $repository := .Values.image.repository -}}
{{- if $registry -}}
{{- $repository = printf "%s/%s" $registry $repository -}}
{{- end -}}
{{- if .Values.image.digest -}}
{{- printf "%s@%s" $repository .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" $repository .Values.image.tag -}}
{{- end -}}
{{- end -}}

{{- define "kubernetes-secret-generator.imagePullSecrets" -}}
{{- $secrets := concat .Values.global.imagePullSecrets .Values.image.pullSecrets -}}
{{- if $secrets }}
imagePullSecrets:
{{- range $secrets }}
  - name: {{ . | quote }}
{{- end }}
{{- end }}
{{- end -}}

{{- define "kubernetes-secret-generator.validate" -}}
{{- if eq (lower .Values.image.tag) "latest" -}}
{{- fail "image.tag=latest is forbidden; use an exact version or image.digest" -}}
{{- end -}}
{{- if and (eq .Values.profile "production") (not .Values.image.digest) -}}
{{- fail "production profile requires image.digest" -}}
{{- end -}}
{{- if and .Values.migration.confirmedScope (ne .Values.migration.confirmedScope .Values.scope.mode) -}}
{{- fail "migration.confirmedScope must exactly match scope.mode" -}}
{{- end -}}
{{- if .Release.IsUpgrade -}}
{{- if ne .Values.migration.confirmedScope .Values.scope.mode -}}
{{- fail "upgrades require migration.confirmedScope to exactly match scope.mode" -}}
{{- end -}}
{{- if eq .Values.scope.mode "namespaces" -}}
{{- $digest := sha256sum (join "\n" (sortAlpha .Values.scope.namespaces)) -}}
{{- if ne .Values.migration.confirmedNamespacesSHA256 $digest -}}
{{- fail (printf "migration.confirmedNamespacesSHA256 must equal %s for the canonical namespace list" $digest) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- if ne (.Values.replicaCount | int) 1 -}}
{{- fail "v4 supports exactly one controller replica; HPA, manual scaling, and multiple active releases are unsupported" -}}
{{- end -}}
{{- range .Values.args -}}
{{- if regexMatch "^--leader-elect($|=)|^--leader-election-id($|=)" . -}}
{{- fail "leader-election arguments were removed in v4; run exactly one controller replica" -}}
{{- end -}}
{{- end -}}
{{- end -}}
