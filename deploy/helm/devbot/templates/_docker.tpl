{{/* The extended resource the daemon gets /dev/net/tun as: the chart's own
device plugin's (docker.dind.tunPlugin), or one the install already runs. */}}
{{- define "devbot.tunResource" -}}
{{- $d := .Values.docker.dind -}}
{{- $name := $d.tunResource | default (ternary "devbot.io/tun" "" $d.tunPlugin.enabled) -}}
{{- if not $name }}{{ fail "docker.mode dind: the daemon needs /dev/net/tun, and a userns pod can't mount the node's by hostPath; turn docker.dind.tunPlugin.enabled on, or set docker.dind.tunResource to the extended resource a device plugin of your own gives it out as; see docs/operating.md#dind" }}{{ end -}}
{{- if not (regexMatch "^[a-z0-9]([-a-z0-9.]*[a-z0-9])?/[-a-zA-Z0-9_.]+$" $name) }}{{ fail (printf "docker.dind.tunResource: %q isn't an extended resource name, <domain>/<name>" $name) }}{{ end -}}
{{- $name -}}
{{- end }}

{{/* The job pod's Docker sidecar (docker.mode dind), in .root's job pod. The
agent reaches it at unix:///devbot-docker/docker.sock, dind's own socket.
Native, like the others: it starts before the agent and stops after the
harness. */}}
{{- define "devbot.dockerSidecar" -}}
{{- $root := .root -}}
{{- $d := $root.Values.docker -}}
{{- if not $root.Values.jobs.userNamespaces }}{{ fail "docker.mode dind: needs jobs.userNamespaces, which is all that keeps the daemon's unconfined sandbox off the node" }}{{ end }}
{{- if not $d.dind.acceptLooserSandbox }}{{ fail "docker.mode dind: set docker.dind.acceptLooserSandbox true to accept the daemon container's sandbox (allowPrivilegeEscalation, procMount Unmasked, the seccomp and AppArmor profiles in docker.dind, Unconfined by default), which Pod Security's baseline level refuses; see docs/operating.md#dind" }}{{ end }}
- name: docker
  image: {{ $d.dind.image }}
  imagePullPolicy: IfNotPresent
  restartPolicy: Always
  # dockerd alone, without the image's default TCP listeners: the socket is
  # the way in, for this pod's agent only.
  args: [dockerd, --host=unix:///devbot-docker/docker.sock]
  env:
    {{- with include "devbot.egressEnv" $root | trim }}
    # Image pulls go out like the agent's own traffic.
    {{- . | nindent 4 }}
    {{- else }} []
    {{- end }}
  # Each setting rootless Docker needs, and why (docs/operating.md#dind).
  securityContext:
    # The image's rootless user, whose subordinate IDs are below.
    runAsUser: 1000
    runAsGroup: 1000
    runAsNonRoot: true
    # newuidmap and newgidmap, which map those IDs into the daemon's user
    # namespace, are setuid.
    allowPrivilegeEscalation: true
    # RuntimeDefault refuses unshare(CLONE_NEWUSER) and mount without
    # CAP_SYS_ADMIN in the pod's namespace, which no container here has.
    seccompProfile:
      {{- toYaml $d.dind.seccompProfile | nindent 6 }}
    # The runtime's profile refuses mount.
    appArmorProfile:
      {{- toYaml $d.dind.appArmorProfile | nindent 6 }}
    # The masked /proc paths stop the kernel mounting a fresh procfs for
    # a container. Kubernetes allows this only in a user namespace.
    procMount: Unmasked
  # Ready when the API answers, not when the socket appears: dockerd makes
  # that before it can serve, and the agent and checks start after this.
  startupProbe:
    exec: { command: [docker, --host=unix:///devbot-docker/docker.sock, version] }
    periodSeconds: 1
    failureThreshold: 120
  resources:
    {{- $res := deepCopy $d.dind.resources }}
    {{- $_ := set $res "limits" (merge (dict (include "devbot.tunResource" $root) 1) ($res.limits | default dict)) }}
    {{- toYaml $res | nindent 4 }}
  volumeMounts:
    - { name: docker, mountPath: /devbot-docker }
    # One level above the data root: the emptyDir's root is root's, and
    # dockerd, as uid 1000, makes docker/ in it itself.
    - { name: docker-data, mountPath: /home/rootless/.local/share }
    # The workspace, at the agent's path, so a bind mount from it has its
    # files; read-only, as the agent's commands are the ones to change them.
    - { name: work, mountPath: /work, readOnly: true }
    - { name: docker-subid, mountPath: /etc/subuid, subPath: subuid, readOnly: true }
    - { name: docker-subid, mountPath: /etc/subgid, subPath: subgid, readOnly: true }
{{- end }}

{{/* The Docker sidecar's volumes, beside the socket's. */}}
{{- define "devbot.dockerVolumes" -}}
{{- $root := .root -}}
{{- $d := $root.Values.docker -}}
- { name: docker, emptyDir: {} }
- name: docker-data
  emptyDir:
    {{- with dig "limits" "ephemeral-storage" "" $d.dind.resources }}
    sizeLimit: {{ . }}
    {{- else }} {}
    {{- end }}
- name: docker-subid
  configMap: { name: {{ $root.Release.Name }}-dind }
{{- end }}
