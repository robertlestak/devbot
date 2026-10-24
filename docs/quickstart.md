# Quickstart

From an empty namespace to devbot opening its first pull request, on Gitea
or on GitHub. Each step names the section of [operating.md](operating.md)
with the details. Replace what is in `<angle brackets>` with your own
values; everything else is meant to be pasted as it is.

The steps install a release named `devbot` in a namespace named `devbot`,
with the chart's own Redis, the `claude` harness for both roles, the web UI
on, and no sign-in for connector callers (`auth.mode: none`). Once it works,
[Connector auth](operating.md#connector-auth) adds sign-in through the forge,
and [Egress](operating.md#egress) narrows what job pods can reach.

1. [Check the prerequisites](#1-check-the-prerequisites)
2. [Create the namespace](#2-create-the-namespace)
3. [Create the bots](#3-create-the-bots): [Gitea](#gitea-two-bot-users) or [GitHub](#github-two-github-apps)
4. [Create the Secrets](#4-create-the-secrets)
5. [Build the claude image](#5-build-the-claude-image)
6. [Write a values file](#6-write-a-values-file)
7. [Install the chart](#7-install-the-chart)
8. [Point the forge's webhook at serve](#8-point-the-forges-webhook-at-serve)
9. [Give the bots a repo and label an issue](#9-give-the-bots-a-repo-and-label-an-issue)
10. [Verify](#10-verify)

## 1. Check the prerequisites

[Prerequisites](operating.md#prerequisites) has the full list and why. In
short:

- Kubernetes 1.30 or later; for job pods' user namespaces (on by default),
  1.33 or later with containerd 2.0 or later or CRI-O 1.25 or later. `kubectl
  get nodes -o wide` shows each node's Kubernetes, kernel and runtime
  versions. Where the nodes fall short, add `jobs: {userNamespaces: false}`
  to the values file in step 6.
- KEDA 2.16 or later. To see which you run:

  ```bash
  kubectl get deploy -A -o jsonpath='{range .items[*]}{.spec.template.spec.containers[0].image}{"\n"}{end}' | grep keda
  ```

  To install it:

  ```bash
  helm repo add kedacore https://kedacore.github.io/charts
  helm install keda kedacore/keda -n keda --create-namespace --wait
  ```

- Helm 4 (`helm version`). With Helm 3, use `--wait` wherever these steps
  say `--wait=legacy`.
- A NetworkPolicy implementation that enforces egress (Calico, Cilium,
  k3s's built-in controller, ...).
- An Anthropic API key, or a Claude subscription token from
  `claude setup-token`.
- A container registry the cluster's nodes can pull from without a pull
  secret, and Docker with `buildx` to push to it: devbot publishes no image
  with Claude Code, so step 5 builds one ([Agent
  images](operating.md#agent-images)).

## 2. Create the namespace

devbot gets a namespace of its own, with nothing else in it
([Security](operating.md#security)):

```bash
kubectl create namespace devbot
```

## 3. Create the bots

devbot acts on the forge as two bots, an implementer and a reviewer, so the
reviewer can approve the implementer's pull requests.

### Gitea: two bot users

Create two users, `devbot` and `devbot-review`, and a token for each with
the scopes `write:repository,write:issue,read:user,read:organization`. In
the web UI: Site Administration, User Accounts, Create User Account; then,
signed in as each bot, Settings, Applications, Generate New Token. Or with
Gitea's CLI, as the user Gitea runs as, on its server or in its pod:

```bash
gitea admin user create --username devbot --email devbot@<your domain> --random-password
gitea admin user create --username devbot-review --email devbot-review@<your domain> --random-password
gitea admin user generate-access-token --username devbot --token-name devbot \
  --scopes write:repository,write:issue,read:user,read:organization
gitea admin user generate-access-token --username devbot-review --token-name devbot \
  --scopes write:repository,write:issue,read:user,read:organization
```

Keep both tokens for step 4. Don't make the bots site admins: a Gitea token
reaches every repo its user can ([Gitea](operating.md#gitea)).

### GitHub: two GitHub Apps

On the organization that owns the repos: Settings, Developer settings,
GitHub Apps, New GitHub App, twice. Call them, say, `<org>-devbot` (the
implementer) and `<org>-devbot-review` (the reviewer). For each:

- **Repository permissions:** Contents, Pull requests and Issues **read and
  write**; Checks and Commit statuses **read** (Metadata read is always on).
  Nothing else. [GitHub](operating.md#github) has the optional ones, among
  them Workflows, which the implementer App needs only for a repo that lets
  devbot change workflow files.
- **Webhook:** on the implementer App only. Turn it off (uncheck Active) on
  the reviewer App. For the implementer App, the URL is
  `https://<public host>/webhooks/github`, the secret is the one you make in
  step 4, and the events are Issues, Issue comment, Pull request, Pull
  request review, Check run, Check suite, Status and Push. You can fill the
  URL and secret in after step 8.
- **Where it can be installed:** only on this account.

Then, on each App's page, note its **App ID** and generate a **private
key**, which downloads a `.pem` file. Keep both for steps 4 and 6.

## 4. Create the Secrets

Make the webhook secret first, and keep it: you give the same value to the
forge in step 8.

```bash
WEBHOOK_SECRET=$(openssl rand -hex 32)
echo "$WEBHOOK_SECRET"
```

The forge credentials, on **Gitea**:

```bash
kubectl -n devbot create secret generic devbot-forge \
  --from-literal=DEVBOT_TOKEN='<the devbot user token>' \
  --from-literal=DEVBOT_REVIEW_TOKEN='<the devbot-review user token>' \
  --from-literal=GITEA_WEBHOOK_SECRET="$WEBHOOK_SECRET"
```

Or on **GitHub**:

```bash
kubectl -n devbot create secret generic devbot-forge \
  --from-file=GITHUB_APP_DEVBOT_KEY=<path to the implementer App's .pem> \
  --from-file=GITHUB_APP_REVIEW_KEY=<path to the reviewer App's .pem> \
  --from-literal=GITHUB_WEBHOOK_SECRET="$WEBHOOK_SECRET"
```

The model credential, one of:

```bash
kubectl -n devbot create secret generic devbot-claude --from-literal=ANTHROPIC_API_KEY='<your Anthropic API key>'
kubectl -n devbot create secret generic devbot-claude --from-literal=CLAUDE_CODE_OAUTH_TOKEN='<the token from claude setup-token>'
```

Every key and who reads it: [Secrets](operating.md#secrets). The chart
generates Redis's passwords itself.

## 5. Build the claude image

Job pods run the agent's CLI in a sidecar, from the image for its harness.
Codex's is published; Claude Code's isn't, because Claude Code is under
Anthropic's Commercial Terms, so you build it and push it to your own
registry ([Agent images](operating.md#agent-images)). Without it, step 7's
`helm install` refuses to render.

The latest release's version is on
[Docker Hub](https://hub.docker.com/r/robertlestak/devbot/tags). The build
adds Claude Code, at the version that release pins, to that release's base
image, so it needs only the release's `docker/` directory:

```bash
git clone --depth 1 --branch v<X.Y.Z> https://github.com/robertlestak/devbot
cd devbot
docker buildx build -f docker/claude.Dockerfile --platform linux/amd64 \
  --build-arg BASE=docker.io/robertlestak/devbot:<X.Y.Z> \
  -t <your registry>/devbot-claude:<X.Y.Z> --push .
```

Tag it `<X.Y.Z>`, the release's version: the chart runs the agent images at
its own version unless told otherwise. For arm64 job nodes, build
`--platform linux/arm64` instead, or both with a multi-platform builder.

## 6. Write a values file

Save one of these as `values.yaml`. Every key is in
[operating.md](operating.md); `values.example-gitea.yaml` and
`values.example-github.yaml` in the chart show fuller installs.

**Gitea:**

```yaml
config:
  providers:
    - name: gitea                       # repo refs look like gitea:owner/name
      type: gitea
      url: https://<your Gitea host>
      implementer_user: devbot
      reviewer_user: devbot-review
      implementer_token_env: DEVBOT_TOKEN
      reviewer_token_env: DEVBOT_REVIEW_TOKEN
      webhook_secret_env: GITEA_WEBHOOK_SECRET
  agents:
    implement: { harness: claude, model: claude-opus-5-5 }
    review: { harness: claude, model: claude-opus-5-5 }
  # Connector calls (the CLI, MCP) are accepted as this principal, with no
  # sign-in: keep serve's /mcp and /api off the internet (step 8).
  anonymous_principal: operator

secrets:
  forge: devbot-forge
  agents:
    claude: devbot-claude

# The image you built in step 5, for the agent sidecar of a role on claude.
image:
  agents:
    claude:
      repository: <your registry>/devbot-claude

ui:
  enabled: true

# Job pods reach DNS, Redis and Anthropic's API, and here any public
# address too, for the forge and the registries repos install from. A Gitea
# inside the cluster also needs its namespace allowed.
networkPolicy:
  internet: true
  allow:
    - { namespace: <Gitea's namespace, if it runs in this cluster> }
```

Leave out the `allow` entry if Gitea isn't in the cluster. A Gitea on a
private address outside it needs `- { cidr: <its IP>/32, ports: [443] }`
instead.

**GitHub:**

```yaml
config:
  providers:
    - name: github                      # repo refs look like github:owner/name
      type: github
      url: https://api.github.com
      implementer_app_id: <the implementer App's ID>
      implementer_key_env: GITHUB_APP_DEVBOT_KEY
      reviewer_app_id: <the reviewer App's ID>
      reviewer_key_env: GITHUB_APP_REVIEW_KEY
      webhook_secret_env: GITHUB_WEBHOOK_SECRET
  agents:
    implement: { harness: claude, model: claude-opus-5-5 }
    review: { harness: claude, model: claude-opus-5-5 }
  # Connector calls (the CLI, MCP) are accepted as this principal, with no
  # sign-in: expose only /webhooks/ to the internet (step 8).
  anonymous_principal: operator

secrets:
  forge: devbot-forge
  agents:
    claude: devbot-claude

# The image you built in step 5, for the agent sidecar of a role on claude.
image:
  agents:
    claude:
      repository: <your registry>/devbot-claude

ui:
  enabled: true

# Job pods reach DNS, Redis and Anthropic's API, and here any public
# address too, for GitHub and the registries repos install from.
networkPolicy:
  internet: true
```

`networkPolicy.internet` lets whatever the model is talked into send what
it reads anywhere public (never a credential: there is none in its
container). [Egress](operating.md#egress) has the narrower options, an
egress proxy or a registry mirror, for after the first run.

## 7. Install the chart

Install the chart at the same version as the image you built in step 5:

```bash
helm install devbot oci://registry-1.docker.io/robertlestak/devbot-chart \
  --version <X.Y.Z> -n devbot -f values.yaml --wait=legacy
```

serve and the job pods' harness and model proxy run the release's base
image, `docker.io/robertlestak/devbot:<X.Y.Z>`, which has no agent CLI. Each
role's agent sidecar runs `image.agents.<harness>`: here your
`devbot-claude:<X.Y.Z>` (a role on `codex` would run the published
`docker.io/robertlestak/devbot-codex:<X.Y.Z>`). If the install fails, see
[Troubleshooting](operating.md#troubleshooting).

## 8. Point the forge's webhook at serve

serve takes webhooks at `/webhooks/<provider name>`, on the Service
`devbot`, port 80. Without a webhook devbot still works, but only notices
changes on its reconcile loop, every two minutes.

### Gitea

A Gitea in the same cluster delivers to serve's Service directly. Gitea
refuses in-cluster addresses unless its `app.ini` allows them, by host
**and** IP. Find serve's IP:

```bash
kubectl -n devbot get svc devbot -o jsonpath='{.spec.clusterIP}{"\n"}'
```

and add, in Gitea's `app.ini`, then restart Gitea:

```ini
[webhook]
ALLOWED_HOST_LIST = external,devbot.devbot.svc.cluster.local,<serve's IP>/32
```

The IP stays the Service's until it is deleted; `serve.clusterIP` pins it
across reinstalls ([Gitea](operating.md#gitea)). Then, in Site
Administration, Webhooks, add a **System Webhook** of type Gitea:

- **Target URL:** `http://devbot.devbot.svc.cluster.local/webhooks/gitea`
- **Content type:** `application/json`
- **Secret:** the `$WEBHOOK_SECRET` from step 4
- **Trigger on:** custom events. Every one of these is required: without
  one, devbot notices that change only on its next reconcile pass.
  - **Issues:** an issue opened, edited, closed or reopened.
  - **Issue Label:** `devbot/ready` and the other labels on an issue.
  - **Issue Comment:** a comment on an issue.
  - **Pull Request:** a PR opened, edited, closed or reopened.
  - **Pull Request Label:** `devbot/ready` on a PR, for PR adoption.
  - **Pull Request Synchronized:** new commits on a PR's branch.
  - **Pull Request Reviewed** (approved, rejected, comment): a review.
  - **Pull Request Comment:** a comment on a PR.
  - **Status**, **Workflow Run** and **Workflow Job:** checks on a PR's
    head.
  - **Push:** the default branch moving, so a changed `.devbot.yaml`
    applies at once.

A Gitea outside the cluster needs serve at an address it can reach,
like GitHub below, with that address in `ALLOWED_HOST_LIST` if it's
private.

### GitHub

GitHub.com delivers from the internet, so `/webhooks/` must be reachable at
a public HTTPS address. Expose that path, and only that path: with
`auth.mode: none` the rest of serve is open to whoever reaches it. With an
ingress controller and a TLS certificate for the host:

```bash
kubectl -n devbot apply -f - <<'EOF'
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: devbot-webhooks
spec:
  ingressClassName: <your ingress class>
  tls:
    - hosts: [<public host>]
      secretName: <the TLS Secret for that host>
  rules:
    - host: <public host>
      http:
        paths:
          - path: /webhooks/
            pathType: Prefix
            backend:
              service: { name: devbot, port: { number: 80 } }
EOF
```

Then, on the implementer App's settings page, set the **Webhook URL** to
`https://<public host>/webhooks/github` and the **Webhook secret** to the
`$WEBHOOK_SECRET` from step 4, with Active checked.

## 9. Give the bots a repo and label an issue

Pick a repo to try devbot in, ideally one with CI on pull requests.

- **Gitea:** in the repo's Settings, Collaborators, add `devbot` as
  **Administrator** (so it can read who else may direct it, see [Who can
  direct devbot](operating.md#who-can-direct-devbot)) and `devbot-review`
  with **Write**.
- **GitHub:** on each App's page, Install App, and install both on the same
  repositories (or the whole organization).

Then open an issue that says what to build, with an **Acceptance criteria**
section:

```markdown
Add a Goodbye function next to Hello.

## Acceptance criteria

- Goodbye(name) returns "goodbye, <name>"
- covered by a test
```

and add the label **`devbot/ready`** to it, as someone with write access to
the repo. If the repo doesn't have that label yet, create it (any colour);
devbot sets its colour and description, and creates its other labels as
it needs them. Without a `.devbot.yaml`, devbot works in the repo as an
outside contributor would and leaves merging to you
([devbot-yaml.md](devbot-yaml.md)).

## 10. Verify

The pods, and the ScaledJobs that start job pods from the queue:

```bash
kubectl -n devbot get pods
kubectl -n devbot get scaledjob
```

`devbot-...` serve and `devbot-redis-...` should be `Running`, and the
ScaledJobs `devbot-implement` and `devbot-review` listed. Then serve
itself, through a port-forward:

```bash
kubectl -n devbot port-forward svc/devbot 8080:80
```

and, in another shell:

```bash
curl -s http://localhost:8080/readyz
```

answers `ok` once serve reaches Redis. Open <http://localhost:8080/ui/>:
the labelled issue shows on the Tasks page, and its job on the Running page
once a pod has taken it. On the issue, devbot comments that it is tracking
the task and swaps `devbot/ready` for `devbot/working`. The implementer
opens a pull request and the issue moves to `devbot/in-review`; once CI
and the reviewer pass it, to `devbot/approved`, for you to merge.
Job pods appear while they run:

```bash
kubectl -n devbot get jobs,pods -w
```

The [client CLI](operating.md#the-client-cli) talks to the same
port-forward:

```bash
docker run --rm --network host -e DEVBOT_URL=http://localhost:8080 \
  docker.io/robertlestak/devbot:<X.Y.Z> list
```

(`--network host` lets the container reach the port-forward on Linux; on
Docker Desktop use `DEVBOT_URL=http://host.docker.internal:8080` instead.)

Nothing happening? [Troubleshooting](operating.md#troubleshooting) has the
usual causes. Next: [forge.md](forge.md) for driving devbot from the issue
tracker, [devbot-yaml.md](devbot-yaml.md) for a repo's own policy, and
[operating.md](operating.md) for everything an install can change.
