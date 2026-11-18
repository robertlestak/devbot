# devbot

Coding agents that work from your issue tracker: a task goes in as an issue, or an existing pull request is handed over, and a reviewed pull request comes out. An implementer pod writes the change, a reviewer pod with its own identity reviews it, and a deterministic gate merges it. Gitea, GitHub.com and GitHub Enterprise Server.

**Source, docs and demo:** https://github.com/robertlestak/devbot

This is devbot's base image: serve, the job harness, the model proxy and the `devbot` CLI, with git and mise and **no agent CLI**. Each role's agent sidecar runs a harness image built on it:

| Image | For |
|---|---|
| `robertlestak/devbot` | serve, the harness, the model proxy, the CLI |
| `robertlestak/devbot-codex` | the agent sidecar of a role on the `codex` harness |
| your own `devbot-claude` | the agent sidecar of a role on `claude`: Claude Code is under Anthropic's terms, so build it yourself ([how](https://github.com/robertlestak/devbot/blob/main/docs/operating.md#agent-images)) |

## Install

With the Helm chart (needs Kubernetes 1.30+, KEDA 2.16+ and Helm 4):

```bash
helm install devbot oci://registry-1.docker.io/robertlestak/devbot-chart \
  --version X.Y.Z -n devbot --create-namespace -f values.yaml --wait=legacy
```

Start with the [quickstart](https://github.com/robertlestak/devbot/blob/main/docs/quickstart.md); every setting is in [operating.md](https://github.com/robertlestak/devbot/blob/main/docs/operating.md).

## Tags

`vX.Y.Z` and `X.Y.Z` for a release, `X.Y` and `X` for the latest in that line, and `latest`. linux/amd64 and linux/arm64. MIT licensed.
