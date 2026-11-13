# devbot-codex

The agent sidecar image for [devbot](https://github.com/robertlestak/devbot) roles that run the `codex` harness: devbot's base image (`robertlestak/devbot`) plus the OpenAI Codex CLI, at the version devbot pins and tests through its model proxy. The agent never holds a model credential: the proxy beside it in the job pod does.

The devbot Helm chart uses this image for `codex` roles by default (`image.agents.codex`). See [Agent harnesses](https://github.com/robertlestak/devbot/blob/main/docs/operating.md#agent-harnesses).

Tags match `robertlestak/devbot`: `vX.Y.Z`, `X.Y.Z`, `X.Y`, `X`, `latest`. linux/amd64 and linux/arm64. devbot is MIT licensed; the Codex CLI is Apache-2.0.
