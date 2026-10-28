{{- define "devbot.labels" -}}
app.kubernetes.io/name: devbot
app.kubernetes.io/instance: {{ .Release.Name }}
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end }}

{{- define "devbot.image" -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}
{{- end }}

{{/* The agent sidecar's image for a harness: image.agents.<harness>, its
tag defaulting to image's. Never the base image, which has no CLI to run.
helm lint, which is neither an install nor an upgrade, renders the defaults,
which can't name a claude image. */}}
{{- define "devbot.agentImage" -}}
{{- $i := index (.root.Values.image.agents | default dict) .harness | default dict -}}
{{- if and (not $i.repository) (or .root.Release.IsInstall .root.Release.IsUpgrade) }}
{{- $why := ternary "devbot publishes no image with Claude Code, which is under Anthropic's terms, so build one" "build one" (eq .harness "claude") }}
{{- fail (printf "image.agents.%s.repository: the %s role runs harness %s, which needs an image with its CLI; %s and set this to it: https://github.com/robertlestak/devbot/blob/main/docs/operating.md#agent-images" .harness .kind .harness $why) }}
{{- end -}}
{{ $i.repository }}:{{ $i.tag | default .root.Values.image.tag | default .root.Chart.AppVersion }}
{{- end }}

{{/* The egress proxy's variables, for every job container, when there is
one. The model proxy is on loopback, and the Pod Identity agent and the
cluster are reached directly. */}}
{{- define "devbot.egressEnv" -}}
{{- with .Values.networkPolicy.egressProxy }}
{{- if .url }}
{{- $no := join "," (concat (list "127.0.0.1" "localhost" "169.254.170.23" "fd00:ec2::23" ".svc" ".cluster.local") (.noProxy | default list)) }}
{{- range $k := list "HTTPS_PROXY" "HTTP_PROXY" "https_proxy" "http_proxy" }}
- { name: {{ $k }}, value: {{ $.Values.networkPolicy.egressProxy.url | quote }} }
{{- end }}
- { name: NO_PROXY, value: {{ $no | quote }} }
- { name: no_proxy, value: {{ $no | quote }} }
{{- end }}
{{- end }}
{{- end }}

{{/* The job pod: the model proxy (the role's model credential, nothing
else), the agent sidecar (the model, with neither the model credential nor
the forge's) and the harness (forge credentials, no model credential). See
internal/agent and internal/modelproxy. */}}
{{- define "devbot.jobPod" -}}
{{- $root := .root -}}
{{- $harness := dig "agents" .kind "harness" "claude" $root.Values.config -}}
{{- $bot := ternary "implementer" "reviewer" (eq .kind "implement") -}}
{{- $aws := dig "agents" .kind "aws" "auth" "" $root.Values.config -}}
{{- $agentSecret := index ($root.Values.secrets.agents | default dict) $harness | default "" -}}
{{- /* IRSA and Pod Identity bring their credential through the ServiceAccount;
everything else needs the harness's Secret. */ -}}
{{- $fromSA := has $aws (list "irsa" "pod_identity") -}}
{{- $docker := and .docker $root.Values.docker.mode -}}
{{- /* The agent's commands and the checks reach the run's Docker by
DOCKER_HOST; Ryuk, testcontainers' reaper, would need the socket mounted
into a container, and the run's containers go with the run anyway. */ -}}
{{- $agentEnv := $root.Values.agentEnv | default dict -}}
{{- if $docker }}{{ $agentEnv = merge (dict "DOCKER_HOST" "unix:///devbot-docker/docker.sock" "TESTCONTAINERS_RYUK_DISABLED" "true") (deepCopy $agentEnv) }}{{ end -}}
{{- /* A run never downloads a Claude Code update it won't use. */ -}}
{{- if eq $harness "claude" }}{{ $agentEnv = merge (dict "DISABLE_AUTOUPDATER" "1") (deepCopy $agentEnv) }}{{ end -}}
{{- if and (not $agentSecret) (not $fromSA) }}{{ fail (printf "secrets.agents.%s: the %s role runs harness %s, so its model proxy needs that Secret" $harness .kind $harness) }}{{ end -}}
{{- $saAnnotations := dig "serviceAccount" "annotations" dict (index $root.Values.jobs .kind) -}}
{{- if and (eq $aws "irsa") (not (index $saAnnotations "eks.amazonaws.com/role-arn")) }}{{ fail (printf "jobs.%s.serviceAccount.annotations: config.agents.%s.aws.auth is irsa, so the ServiceAccount needs eks.amazonaws.com/role-arn" .kind .kind) }}{{ end -}}
metadata:
  labels:
    {{- include "devbot.labels" $root | nindent 4 }}
    app.kubernetes.io/component: {{ .kind }}
    {{- if $docker }}
    docker.devbot.io/mode: {{ $docker }}
    {{- end }}
  annotations:
    sidecar.istio.io/inject: "false"
    checksum/config: {{ include (print $root.Template.BasePath "/configmap.yaml") $root | sha256sum }}
    # EKS's webhook gives an IRSA or Pod Identity credential to every
    # container of a pod; only the model proxy may have it.
    eks.amazonaws.com/skip-containers: harness,agent{{ if $docker }},docker{{ end }}
spec:
  restartPolicy: Never
  enableServiceLinks: false
  # The role's own ServiceAccount, never serve's: what EKS binds to it
  # reaches only this role's model proxy. IRSA and Pod Identity project their
  # own token, so no API token is mounted.
  serviceAccountName: {{ $root.Release.Name }}-{{ .kind }}
  automountServiceAccountToken: false
  {{- with include "devbot.imagePullSecrets" $root | trim }}{{ . | nindent 2 }}{{ end }}
  {{- with $root.Values.jobs.runtimeClassName }}
  runtimeClassName: {{ . }}
  {{- end }}
  {{- with $root.Values.jobs.nodeSelector }}
  nodeSelector:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- with $root.Values.jobs.tolerations }}
  tolerations:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- with $root.Values.jobs.affinity }}
  affinity:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- with $root.Values.jobs.topologySpreadConstraints }}
  topologySpreadConstraints:
    {{- range . }}
    {{- /* Both roles in one selector: they share the nodes. */ -}}
    {{- $c := . -}}
    {{- if not (hasKey $c "labelSelector") }}
    {{- $c = merge (dict "labelSelector" (dict
          "matchLabels" (dict "app.kubernetes.io/name" "devbot" "app.kubernetes.io/instance" $root.Release.Name)
          "matchExpressions" (list (dict "key" "app.kubernetes.io/component" "operator" "In" "values" (list "implement" "review"))))) (deepCopy $c) }}
    {{- end }}
    - {{ toYaml $c | indent 6 | trim }}
    {{- end }}
  {{- end }}
  {{- if $root.Values.jobs.userNamespaces }}
  # Root in the agent container is an unprivileged user on the node.
  hostUsers: false
  {{- end }}
  securityContext:
    seccompProfile: { type: RuntimeDefault }
  initContainers:
    # Native sidecars, started in order, each once the one before is up, and
    # stopped when the harness exits. The model proxy comes first: the agent
    # reads its address and token at start.
    - name: model-proxy
      image: {{ include "devbot.image" $root }}
      imagePullPolicy: {{ $root.Values.image.pullPolicy }}
      restartPolicy: Always
      args: [model-proxy, {{ .kind }}]
      env:
        - { name: DEVBOT_CONFIG, value: /etc/devbot/config.yaml }
        - { name: DEVBOT_MODEL_PROXY_STATE, value: /run/devbot-model/proxy.json }
        {{- with include "devbot.egressEnv" $root | trim }}{{ . | nindent 8 }}{{ end }}
      {{- if not $fromSA }}
      envFrom:
        # Only the credential of the harness this role runs.
        - secretRef: { name: {{ $agentSecret }} }
      {{- end }}
      startupProbe:
        exec: { command: [test, -s, /run/devbot-model/proxy.json] }
        periodSeconds: 1
        failureThreshold: 60
      securityContext:
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: true
        capabilities: { drop: [ALL] }
      resources:
        {{- toYaml $root.Values.jobs.modelProxyResources | nindent 8 }}
      volumeMounts:
        - { name: model, mountPath: /run/devbot-model }
        - { name: config, mountPath: /etc/devbot }
    {{- if $docker }}
    {{- include "devbot.dockerSidecar" $ | nindent 4 }}
    {{- end }}
    - name: agent
      image: {{ include "devbot.agentImage" (dict "root" $root "harness" $harness "kind" .kind) }}
      imagePullPolicy: {{ $root.Values.image.pullPolicy }}
      restartPolicy: Always
      args: [agent, {{ .kind }}]
      env:
        - { name: DEVBOT_CONFIG, value: /etc/devbot/config.yaml }
        - { name: DEVBOT_AGENT_SOCKET, value: /run/devbot/agent.sock }
        - { name: DEVBOT_MODEL_PROXY_STATE, value: /run/devbot-model/proxy.json }
        {{- with $agentEnv }}
        # agentEnv reaches the model's commands and the checks too.
        - { name: DEVBOT_PASS_ENV, value: {{ keys . | sortAlpha | join "," | quote }} }
        {{- end }}
        {{- range $k, $v := $agentEnv }}
        - { name: {{ $k }}, value: {{ $v | quote }} }
        {{- end }}
        {{- with include "devbot.egressEnv" $root | trim }}{{ . | nindent 8 }}{{ end }}
      # Root, because the model installs what a repo needs with apt; the
      # user namespace (jobs.userNamespaces) keeps that root off the node.
      securityContext:
        allowPrivilegeEscalation: false
        capabilities: { drop: [NET_RAW, MKNOD] }
      startupProbe:
        exec: { command: [test, -S, /run/devbot/agent.sock] }
        periodSeconds: 1
        failureThreshold: 60
      resources:
        {{- toYaml $root.Values.jobs.agentResources | nindent 8 }}
      volumeMounts:
        - { name: work, mountPath: /work }
        - { name: run, mountPath: /run/devbot }
        - { name: model, mountPath: /run/devbot-model, readOnly: true }
        - { name: config, mountPath: /etc/devbot }
        {{- if $docker }}
        - { name: docker, mountPath: /devbot-docker }
        {{- end }}
  containers:
    - name: harness
      image: {{ include "devbot.image" $root }}
      imagePullPolicy: {{ $root.Values.image.pullPolicy }}
      args: [run, {{ .kind }}]
      env:
        - { name: DEVBOT_CONFIG, value: /etc/devbot/config.yaml }
        - { name: DEVBOT_AGENT_SOCKET, value: /run/devbot/agent.sock }
        - { name: DEVBOT_PRIVATE_DIR, value: /harness }
        {{- if $docker }}
        # This pod takes the jobs of repos that ask for a Docker daemon.
        - { name: DEVBOT_DOCKER, value: "true" }
        {{- end }}
        # Only the harness talks to Redis, as a user that can't reach sign-ins.
        {{- include "devbot.redisEnv" (dict "root" $root "user" "harness") | nindent 8 }}
        # For serve to label this pod with the job it takes (pod_labels).
        - name: DEVBOT_POD_NAME
          valueFrom: { fieldRef: { fieldPath: metadata.name } }
        {{- range $k, $v := $root.Values.harnessEnv }}
        - { name: {{ $k }}, value: {{ $v | quote }} }
        {{- end }}
        # The role's own bot credential, one key of the forge Secret per
        # provider: never the other bot's, nor the webhook secret.
        {{- range $root.Values.config.providers }}
        {{- $env := index . (printf "%s_token_env" $bot) | default (index . (printf "%s_key_env" $bot)) }}
        {{- if $env }}
        - name: {{ $env }}
          valueFrom: { secretKeyRef: { name: {{ $root.Values.secrets.forge }}, key: {{ $env }} } }
        {{- end }}
        {{- end }}
        {{- with include "devbot.egressEnv" $root | trim }}{{ . | nindent 8 }}{{ end }}
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532
        runAsGroup: 65532
        readOnlyRootFilesystem: true
        allowPrivilegeEscalation: false
        capabilities: { drop: [ALL] }
      resources:
        {{- toYaml $root.Values.jobs.harnessResources | nindent 8 }}
      volumeMounts:
        - { name: work, mountPath: /work }
        - { name: run, mountPath: /run/devbot }
        - { name: config, mountPath: /etc/devbot }
        - { name: harness, mountPath: /harness }
  volumes:
    - { name: work, emptyDir: {} }
    - { name: run, emptyDir: {} }
    - { name: harness, emptyDir: {} }
    - { name: model, emptyDir: {} }
    {{- if $docker }}
    {{- include "devbot.dockerVolumes" $ | nindent 4 }}
    {{- end }}
    - name: config
      configMap: { name: {{ $root.Release.Name }}-config }
{{- end }}

{{/* serve's metrics port, from config.metrics_addr (default :9090). */}}
{{- define "devbot.metricsPort" -}}
{{ .Values.config.metrics_addr | default ":9090" | splitList ":" | last }}
{{- end }}

{{/* serve's own port, from config.listen (default :8080): the container's
port http, which the probes and the Service reach it at. */}}
{{- define "devbot.listenPort" -}}
{{ .Values.config.listen | default ":8080" | splitList ":" | last }}
{{- end }}

{{/* The Secret with the Redis users' passwords: redis.existingSecret, or
the one the chart generates for its own Redis. */}}
{{- define "devbot.redisSecret" -}}
{{- if .Values.redis.existingSecret -}}
{{ .Values.redis.existingSecret }}
{{- else if .Values.redis.enabled -}}
{{ .Release.Name }}-redis
{{- else -}}
{{ fail "redis.existingSecret: with redis.enabled false, name the Secret with your Redis's devbot-serve, devbot-harness and devbot-keda passwords" }}
{{- end -}}
{{- end }}

{{/* A Redis client's credential, as env: the ACL user (serve or harness)
and its password from the Secret, and the CA and Sentinel password from
secrets.redis when config.redis names them. */}}
{{- define "devbot.redisEnv" -}}
- { name: DEVBOT_REDIS_USERNAME, value: devbot-{{ .user }} }
- name: DEVBOT_REDIS_PASSWORD
  valueFrom: { secretKeyRef: { name: {{ include "devbot.redisSecret" .root }}, key: {{ .user }}-password } }
{{- range $param, $key := include "devbot.redisExtraSecrets" .root | fromYaml }}
- name: {{ $key }}
  valueFrom: { secretKeyRef: { name: {{ $.root.Values.secrets.redis }}, key: {{ $key }} } }
{{- end }}
{{- end }}

{{/* Redis's users, in redis.conf form, with pw (in the Redis container's
script) hashing each password. Nobody connects without one. serve has
everything. The job harness has the keys a job uses (queues, leases, the
hold, the event feed, counters, and the locks and markers of the engine it
runs) and not devbot:auth:*, where sign-ins live, nor anything that
lists, flushes or reconfigures. KEDA reads the queue lengths, with a Lua
script that Redis checks against the same ACL. A Redis of your
own (redis.enabled false) needs these users.

The default user is off, so no connection can be it, but may do anything:
Redis replays its append-only file as the default user and checks each
EXEC against that user's ACL (multi.c), so without that every transaction
in the file is refused on a restart. */}}
{{- define "devbot.redisACL" -}}
user default off ~* &* +@all
user devbot-serve on #$(pw "$SERVE_PASSWORD") ~* &* +@all
user devbot-harness on #$(pw "$HARNESS_PASSWORD") resetchannels -@all +@read +@write +@connection +@transaction +eval +evalsha -@admin -@dangerous -function -scan -randomkey {{ include "devbot.harnessKeys" . }}
user devbot-keda on #$(pw "$KEDA_PASSWORD") resetchannels -@all +@connection -@admin -@dangerous +type +llen +eval +evalsha -script -function ~devbot:q:implement ~devbot:q:review ~devbot:q:implement-docker ~devbot:q:review-docker ~devbot:q:integrate ~devbot:q:integrate-docker ~devbot:q:plan
{{- end }}

{{/* The harness's key patterns: internal/queue's, less what only serve
uses (webhook dedupe, subscriber cursors, the leader lease, the rebase
queue and its behind count, the branch sweep), and read-only the bots'
logins, which serve records for pods that can't read the other bot's, the
repo config generation, which serve bumps on a push, and how a job pod's
services are doing and how its agent container ended, which serve records. */}}
{{- define "devbot.harnessKeys" -}}
~devbot:queued ~devbot:q:* ~devbot:ratelimit:until ~devbot:quota:* ~devbot:events ~devbot:stats ~devbot:lease:* ~devbot:lease-info:* ~devbot:merge:* ~devbot:merge-retry:* ~devbot:eval:* ~devbot:eval-dirty:* ~devbot:once:* ~devbot:eval-requests %R~devbot:bots:* %R~devbot:repo-config:* %R~devbot:services:* %R~devbot:agent-exit:*
{{- end }}

{{/* config.redis's secrets beyond the user's password, each a key of
secrets.redis named by its *_env: the CA (tls.ca_env) and Sentinel's
password (sentinel.password_env). A dict of TriggerAuthentication parameter
to key. Fails when one is named without that Secret, or the Secret is set
and none is. */}}
{{- define "devbot.redisExtraSecrets" -}}
{{- $r := .Values.config.redis | default dict -}}
{{- $keys := dict -}}
{{- range $param, $key := dict "sentinelPassword" (dig "sentinel" "password_env" "" $r) "ca" (dig "tls" "ca_env" "" $r) -}}
{{- if $key }}{{ $_ := set $keys $param $key }}{{ end -}}
{{- end -}}
{{- if and $keys (not .Values.secrets.redis) }}{{ fail "secrets.redis: config.redis names secrets (sentinel.password_env or tls.ca_env), so they need a Secret" }}{{ end -}}
{{- if and .Values.secrets.redis (not $keys) }}{{ fail "secrets.redis is set, but config.redis names none of its keys (sentinel.password_env, tls.ca_env)" }}{{ end -}}
{{- toYaml $keys -}}
{{- end }}

{{/* A ScaledJob's trigger on queue .list, reaching Redis as config.redis
says: one endpoint, or a master through Sentinel, with TLS as devbot does.
The user and secrets are in the TriggerAuthentication. */}}
{{- define "devbot.redisTrigger" -}}
{{- $root := .root -}}
{{- $r := $root.Values.config.redis | default dict -}}
{{- $master := dig "sentinel" "master" "" $r -}}
- type: {{ ternary "redis-sentinel" "redis" (ne $master "") }}
  metadata:
    {{- if $master }}
    {{- $addrs := list }}
    {{- range dig "sentinel" "addrs" list $r }}{{ $addrs = append $addrs (include "devbot.kedaAddr" (dict "root" $root "addr" .)) }}{{ end }}
    addresses: {{ join "," $addrs | quote }}
    sentinelMaster: {{ $master | quote }}
    {{- with dig "sentinel" "username" "" $r }}
    sentinelUsername: {{ . | quote }}
    {{- end }}
    {{- else }}
    address: {{ include "devbot.redisAddr" (dict "root" $root "fqdn" true) }}
    {{- end }}
    {{- with $r.db }}
    databaseIndex: {{ . | quote }}
    {{- end }}
    {{- if dig "tls" "enabled" false $r }}
    enableTLS: "true"
    {{- end }}
    listName: {{ .list }}
    listLength: "1"
  authenticationRef: { name: {{ $root.Release.Name }}-redis }
{{- end }}

{{/* Whether serve labels job pods: config.pod_labels.enabled, else
podLabels.enabled. */}}
{{- define "devbot.podLabels" -}}
{{ dig "pod_labels" "enabled" .Values.podLabels.enabled .Values.config }}
{{- end }}

{{/* Whether serve runs service pods: config.services.enabled, else
services.enabled. */}}
{{- define "devbot.services" -}}
{{ dig "services" "enabled" .Values.services.enabled .Values.config }}
{{- end }}

{{/* The Redis address: config.redis.addr, else the bundled Redis's
Service, by its release's name. .fqdn: as KEDA, in a namespace of its own,
reaches it. */}}
{{- define "devbot.redisAddr" -}}
{{- $root := .root -}}
{{- $addr := dig "addr" "" ($root.Values.config.redis | default dict) -}}
{{- if not $addr -}}
{{- $addr = ternary (printf "%s-redis:6379" $root.Release.Name) "localhost:6379" (eq $root.Values.redis.enabled true) -}}
{{- end -}}
{{- if .fqdn -}}
{{ include "devbot.kedaAddr" (dict "root" $root "addr" $addr) }}
{{- else -}}
{{ $addr }}
{{- end -}}
{{- end }}

{{/* An address as KEDA's operator reaches it: it resolves names from its
own namespace, so a single-label host, a Service in the release's, gets that
namespace's domain. Dotted names, IPs and localhost are left alone. */}}
{{- define "devbot.kedaAddr" -}}
{{- if and (regexMatch "^[A-Za-z0-9-]+(:[0-9]+)?$" .addr) (not (regexMatch "^(localhost|[0-9]+)(:|$)" .addr)) -}}
{{ regexReplaceAll "^([^:]+)" .addr (printf "${1}.%s.svc.cluster.local" .root.Release.Namespace) }}
{{- else -}}
{{ .addr }}
{{- end -}}
{{- end }}

{{/* A value from values.yaml as a CEL literal, each element and map value
in dyn(): Kubernetes' CEL refuses a list or map literal of mixed types,
which a pod spec's (strings and ints) is. */}}
{{- define "devbot.cel" -}}
{{- if kindIs "map" . -}}
{ {{- range $i, $k := keys . | sortAlpha }}{{ if $i }}, {{ end }}{{ toJson $k }}: dyn({{ include "devbot.cel" (index $ $k) }}){{ end -}} }
{{- else if kindIs "slice" . -}}
[ {{- range $i, $v := . }}{{ if $i }}, {{ end }}dyn({{ include "devbot.cel" $v }}){{ end -}} ]
{{- else -}}
{{ toJson . }}
{{- end -}}
{{- end }}

{{/* imagePullSecrets' names, as a YAML list: each entry {name: x} or just
x (values.schema.json requires the name). */}}
{{- define "devbot.pullSecretNames" -}}
{{- $names := list -}}
{{- range .Values.imagePullSecrets | default list -}}
{{- if kindIs "map" . }}{{ $names = append $names .name }}{{ else }}{{ $names = append $names . }}{{ end -}}
{{- end -}}
{{- toYaml $names -}}
{{- end }}

{{/* A pod spec's imagePullSecrets, from imagePullSecrets, or nothing. Only
the kubelet reads them: no container mounts one. */}}
{{- define "devbot.imagePullSecrets" -}}
{{- with include "devbot.pullSecretNames" . | fromYamlArray }}
imagePullSecrets:
{{- range . }}
  - name: {{ . | quote }}
{{- end }}
{{- end }}
{{- end }}
