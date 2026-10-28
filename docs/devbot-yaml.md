# `.devbot.yaml` reference

`.devbot.yaml` is **optional**. A repo can commit one to its default branch to
set its own policy: what devbot may merge, which checks prove a change works,
which paths need a person. Without one, devbot works in the repo the way any
outside contributor would, under the deployment's [default policy](#repos-without-devbotyaml),
and nothing has to be added to the repo. That's how devbot works on other
people's projects.

Machine-readable schema: [`devbot.schema.json`](devbot.schema.json), whose
`$id` is its raw URL on GitHub, devbot's public home. Point your editor at it to
catch typos, since devbot ignores keys it doesn't know. With the YAML
language server (VS Code's YAML extension, most LSP editors), the first line
of the file does it:

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/robertlestak/devbot/main/docs/devbot.schema.json
version: 1
enabled: true
```

## Which repos devbot works in

| The repo has... | devbot... |
|---|---|
| `.devbot.yaml` (without `enabled: false`) | uses that file, exactly as written (key defaults below) |
| `.devbot.yaml` with `enabled: false` | stays out: an explicit opt-out always wins |
| no `.devbot.yaml` | uses the deployment's [default policy](#repos-without-devbotyaml), unless the install sets `repos.opt_in: file`, in which case it stays out |

In every case, both bot users (implementer and reviewer) need **write access**,
since giving them access is what puts a repo in reach. The repo must also pass
the deployment's `repos.allow` / `repos.deny` lists, if it has any. See
[operating.md](operating.md#deployment-config-config).

## How devbot reads it

- **Only from the default branch.** A PR that changes `.devbot.yaml` takes effect when it merges, not before. A task with another [base branch](connectors.md#retargeting), whether its own or the repo's `base_branch`, is still governed by the default branch's file, not the base's: `base_branch` names where tasks go, not which file governs them.
- **Cached for up to 5 minutes** per repo. A push to the default branch that touches the file drops the cache immediately, as long as the webhook arrives.
- **Always protected.** These are added to `protected_paths` whether you list them or not, and each matches at any depth in the tree: `.devbot.yaml` (in a subdirectory too), so devbot can never widen its own permissions by editing it, and the files the agent CLIs read as instructions, settings or MCP config, so a change can't steer the next model run or the reviewer: `**/.devbot/**`, `**/.claude/**`, `**/.codex/**`, `.mcp.json`, `CLAUDE.md`, `CLAUDE.local.md` and `AGENTS.md`. A task that asks to change one ends at `needs-human`, for a person to merge. None of them can be made [approvable](#changes-a-person-may-approve): an `approvable_paths` entry that names one, or a path inside one, or matches one (`AGENTS.md`, `.claude/settings.json`, `sub/.devbot/x`, `**`, `*.md`) makes the file invalid.
- **Unknown keys are ignored.** Durations use Go syntax (`90s`, `20m`, `1h30m`).

## Minimal

```yaml
version: 1
enabled: true
checks:
  - go test ./...
```

Merges stay with a human (`automerge` defaults to `false`), and the repo's PR
CI has to report a status (`require_checks` defaults to `true`).

## Keys

The deployment caps `max_attempts`, `check_retries`, `max_review_rounds`,
`max_rebase_rounds` and `timeouts.*`, may restrict which models a repo
or a task picks, can force `docker`, `pull_requests.enabled` and
`pull_requests.forks` off, and sets limits on `services` (`repos.limits` in
[operating.md](operating.md#deployment-config-config)). A value over its cap
is cut to the cap, a model the deployment doesn't allow gives way to the
deployment's own (a task's, to the repo's and then the deployment's), and
`repos.limits.docker: false`, `repos.limits.pull_requests.enabled: false` and
`repos.limits.pull_requests.forks: false` turn those off whatever the repo
says. `services` are the exception: a list outside `repos.limits.services`
isn't trimmed but refused, and stops the task (see [Services](#services)).

| Key | Type | Default | What it does |
|---|---|---|---|
| `version` | int | - | Schema version. Use `1`. |
| `enabled` | bool | `true` | `false` is an **explicit opt-out**: devbot ignores the repo's issues and `file_task` refuses, even when the install would work in repos without a file. |
| `base_branch` | branch name | the default branch | The base branch of every task in the repo that doesn't name its own: the implementer branches from it, the PR is opened against it, and Docker detection, the rebase queue, the branch rules and required checks devbot reads, and the merge all use it. A task's own base wins: `file_task`'s `base`, a `retarget`, a PR base changed on the forge, a carried PR's base (see [Retargeting](connectors.md#retargeting)). It is read when devbot accepts a task and recorded on the issue by the task's first run, so changing it doesn't move a task already in flight. A plain name (letters, digits and `. _ / + @ -`, not starting with `.`, `/` or `-`), or the file is invalid; a branch the repo doesn't have stops the task at `needs-human` naming it. The file itself is still read from the default branch only. Set it for a `develop`/`main` layout where `main` is the default branch but work should land on `develop`. |
| `automerge` | bool | `false` | Whether devbot merges a PR that passes the [merge gate](#the-merge-gate). With `false`, a passing PR moves to `devbot/approved` and waits for a person, unless its issue carries `devbot/automerge` (see `task_automerge`). Remember that a merge triggers whatever the repo's CI does on the default branch, which may be a deploy. |
| `task_automerge` | bool | `true` | Whether a single task can ask to be merged, with the `devbot/automerge` label on its issue (`file_task` sets it with `automerge: true`), when `automerge` is `false`. The label only replaces the person's merge click: the PR still has to pass the whole [merge gate](#the-merge-gate) first. With `false`, the label is ignored, the PR waits at `devbot/approved`, and the approval comment says per-task automerge is disabled. Set it for repos where a merge to the default branch deploys something sensitive. |
| `merge_method` | string | `squash` | How devbot merges, passed to the forge. Gitea accepts `merge`, `rebase`, `rebase-merge`, `squash`, `fast-forward-only`; GitHub accepts `merge`, `squash`, `rebase`. A method the forge doesn't accept stops the merge at `needs-human`. |
| `require_checks` | bool | `true` | Whether the merge gate requires checks on the PR head. With `true` and **no** checks reported at all, devbot waits 10 minutes from its last push for CI to appear, then stops the task as `needs-human`. A repo with no PR CI should add some, or set `false`. Checks the repo's own branch rules require are waited for either way (see [the merge gate](#the-merge-gate)). |
| `checks_pending_timeout` | duration | `2h` | How long checks may stay pending on a PR's head, from when devbot first saw that head's checks pending, its last re-run of them or the last retry, before the task stops at `needs-human`, naming the checks still pending. A check that never finishes (a runner that died, a status left waiting) would otherwise hold the task in review for good, and, while it has the turn, every other automerge on its base branch. Each new head starts the wait over, whoever pushed it. Set it above your slowest CI run. `0` or unset is the default; a negative is an error. |
| `max_attempts` | int | `3` | Failed implement attempts allowed before the task stops at `devbot/needs-human`, and on the repo of a parent task filed with `plan`, failed runs of its plan job before the parent does. A failed attempt is a model error, checks still failing after `check_retries`, an unusable `result.json`, or a pod that died. **Hitting the model's usage limit is not an attempt.** `retry` resets the count. `0` or unset is the default; a negative is an error. |
| `max_replans` | int | `2` | On the repo of a [parent task](connectors.md#planning-a-parent-task) filed with `plan`: how many times it is planned again because a child task's implementer asked to change the contract (`status: contract-change`). Past it, the next request stops the parent at `needs-human`, and `retry` on the parent counts afresh. A plan job's own failed runs count towards `max_attempts` instead. `0` means a request always goes to a person; a negative is an error. |
| `check_retries` | int | `2` | Within one attempt, how many times the harness resumes the model's own session with the failing check output before the attempt counts as failed. `0` means no second chances. |
| `max_review_rounds` | int | `3` | Fix rounds allowed on one PR before it stops at `needs-human`. A round is the PR going back to the implementer: requested changes from the reviewer or an authorized person, CI failure, or a rebase after the base moved; a check that was cancelled isn't a CI failure (see [Cancelled checks](operating.md#cancelled-checks)). At most one round per head commit: a round whose push leaves the PR at the head it was asked for (the implementer found nothing to change) stops at `needs-human`, since asking again would get the same answer. **A clean rebase doesn't count**: one whose push left the PR's changes exactly as they were (no conflict resolution, nothing else) and whose new head was approved and green again. A rebase that needed conflict resolution, or that was followed by failing CI or requested changes, counts. Since a rebase can only turn out clean afterwards, one is still let through when every round is used; if it isn't clean, the next request stops the task. `0` or unset is the default; a negative is an error. |
| `max_rebase_rounds` | int | `10` | Clean rebases allowed on one PR before it stops at `needs-human`, the backstop for a PR whose base keeps moving under it. They count here instead of towards `max_review_rounds`, and the stop says it was this cap. `retry` resets both counts. `0` or unset is the default; a negative is an error. |
| `rebase` | string | `lazy` | When devbot rebases an approved, green PR that is behind its base or conflicts with it. `lazy`: only when it is about to merge or someone asks. Without automerge, it stays at `devbot/approved`, noted as behind, until a person adds `devbot/rebase` to the issue, comments `/rebase`, or calls the `rebase` connector action. With automerge (the repo's, or `devbot/automerge`), PRs on one base branch take turns, oldest approval first: one is rebased, re-reviewed and merged, then the next. `eager`: every such PR is rebased as soon as its base moves, a rebase and a re-review apiece for each merge into the base. See [Rebasing](#rebasing). |
| `setup` | list of strings | `[]` | **Hints, not a script.** Shown to the implementer as a known-good starting point for installing the repo's runtime. The agent decides what to actually run and records it in `.devbot/setup.sh` (see [Runtime setup](#runtime-setup)). Without hints, the agent works it out, and its PR suggests some. |
| `checks` | list of strings | `[]` | Shell commands that must all pass before a PR is opened (see [Checks](#checks)). They're devbot's own pre-PR verification, not CI. When empty, the implementer finds the project's own checks and the harness re-runs the commands it names, as in [repos without `.devbot.yaml`](#repos-without-devbotyaml). Only if it names none does the PR open with nothing re-run. |
| `protected_paths` | list of globs | [the list below](#protected-paths) | Files a change may not touch without a person. The implementer is told to leave them alone, except those on `approvable_paths`; work that touches one is **never pushed** on its own say: it stops at `needs-human`, or, for a file on `approvable_paths`, waits unpushed for a person's approval. The merge gate stops a PR that touches one, unless it is a file on `approvable_paths` holding what an approval approved. Setting the key replaces the default list (`[]` leaves only the [always-protected paths](#how-devbot-reads-it)). See [protected paths](#protected-paths); glob rules are [below](#glob-syntax). |
| `approvable_paths` | list of globs | `[.github/** .gitea/** .forgejo/** .gitlab-ci.yml .gitlab/** .circleci/** .buildkite/** Jenkinsfile]` | The protected paths a person may approve a change to, by default the CI/CD entries of [the default `protected_paths`](#protected-paths): work that changes one is held, unpushed, at `devbot/needs-approval`, with its diff on the issue, and pushed once someone who can direct devbot approves exactly that content (see [Changes a person may approve](#changes-a-person-may-approve)). Only a path that is also protected is affected, and a `CODEOWNERS` or `.gitattributes` file, at any depth, never is, whatever the list covers. `[]` sends every protected change to `needs-human`. Listing an [always-protected path](#how-devbot-reads-it), or a path inside one, makes the file invalid. Glob rules are [below](#glob-syntax). For repos without a file, `repos.defaults.approvable_paths`. |
| `approval` | string | `person` | Who approves a held change to `approvable_paths`. `person`: someone who can direct devbot, every time. `auto_clean`: devbot pushes the change at once, as if approved, when the [risk read](#changes-a-person-may-approve) of its protected diff finds nothing, with a comment showing that diff and saying so; anything else is held for a person, as with `person`, and the comment says why. An auto-pushed change binds content as a person's approval does. Paths that always need a person never get this far. For repos without a file, `repos.defaults.approval`. See [Approving without a person](#approving-without-a-person). |
| `authorized_users` | list of logins | `[]` | Narrows who may direct devbot on **this** repo: when set, a person needs the forge permission the install requires **and** a place on this list. It never grants access on its own. Logins match in any case, as on the forge. Empty means no narrowing. devbot's own bot users are always authorized. See [Who can direct devbot](#who-can-direct-devbot). |
| `required_reviewers` | list of logins | `[]` | Reviewers, typically other bots such as a security reviewer, that must each **approve the current head** before devbot merges, on top of devbot's own reviewer's approval. Until they do, the PR waits at `devbot/approved`, for up to `checks_pending_timeout` from when devbot first found the head without them, then `needs-human` naming who is missing. A `request_changes` from one on the head is a fix round with its text, and a push needs fresh approvals. Their reviews count in the merge gate and nowhere else: they need no forge access, and being listed doesn't let them direct devbot (`devbot/ready`, answers, labels, connector calls). The approval comment says which have approved and which are awaited. See [the merge gate](#the-merge-gate). |
| `human_checks` | list of globs | `[]` | Checks, by the name the forge reports them under, that only a person can make pass: a CLA, a manual sign-off, a change-advisory-board gate. [Glob rules](#glob-syntax) apply to the name. One that fails or is pending starts no fix round, uses no round and no `checks_pending_timeout`: once devbot's reviewer has approved, the PR waits at `devbot/approved` with a comment naming the check, and carries on when it turns green. It still never merges while one isn't green. Other failing checks on the same head get a fix round that lists only them. |
| `trusted_reviewers` | list of logins | `[]` | Review bots (and people) whose change requests on the PRs devbot works on count, without the forge permission it takes to direct devbot. A `request_changes` review from a listed login on the PR's current head sends the PR back for a fix round with its text and line comments, and holds the merge until it is dismissed or that login approves a later head, as an authorized person's does. Their reviews and comments are shown to the implementer and the reviewer as trusted. Being listed grants **nothing else**: not `devbot/ready`, answers, `/rebase`, `/base`, `devbot/rebase`, `devbot/automerge` or connector calls. Logins match in any case. For repos without a file, `repos.defaults.trusted_reviewers`. See [Trusted reviewers](#trusted-reviewers). |
| `generated_paths` | list of globs | `[]` | Generated files: build output, bundles, vendored code. Like a file the base branch's `.gitattributes` marks `linguist-generated`, each is listed in the reviewer's index without its content, and the reviewer reviews the source behind it and rebuilds it where it can. Read from the default branch like the rest of the file, so a PR can't mark its own changes generated. See [What the reviewer is given](#what-the-reviewer-is-given); glob rules are [below](#glob-syntax). |
| `review_budget_kib` | int | `512` | How much patch content, in KiB, of the files that aren't generated a review may be given. A PR within it is reviewed in full, however big its generated files. Past it no review runs: the task stops at `needs-human` with the sizes, for a person to review it, mark generated paths or raise this. See [What the reviewer is given](#what-the-reviewer-is-given). `0` or unset is the default; a negative is an error. |
| `docker` | bool | unset: [detected](#docker) | Gives each implement and review run of the repo a Docker daemon of its own, at `DOCKER_HOST`, for tests that start containers themselves: testcontainers, docker compose. `true` turns it on, `false` off; unset, devbot decides from what the task's base branch has. See [Docker](#docker). Only when the install runs one (`docker.mode` in [operating.md](operating.md#docker)); elsewhere, and where `repos.limits.docker` is `false`, the runs get none. |
| `pull_requests.enabled` | bool | `true` | Whether devbot carries a PR it didn't open to merge when an authorized person adds `devbot/ready` to it (see [Handing devbot an existing PR](forge.md#handing-devbot-an-existing-pr)). With `false`, a `devbot/ready` on a PR gets one short comment saying PR adoption is off for this repo and comes off again, and nothing is queued. Where `repos.limits.pull_requests.enabled` is `false`, it is off whatever this says. For repos without a file, `repos.defaults.pull_requests.enabled`. |
| `pull_requests.forks` | bool | `true` | Whether that goes for a PR from a fork too (see [PRs from forks](forge.md#prs-from-forks)): devbot works on the fork's branch when its author allows edits by maintainers, and otherwise on a `devbot/pr-<N>` branch of this repo, with a new PR that supersedes theirs. Only where `pull_requests.enabled` is on as well. With `false`, a `devbot/ready` on a fork's PR gets one short comment saying PRs from forks aren't adopted in this repo and comes off again, and nothing is queued; same-repo PRs are still carried. Where `repos.limits.pull_requests.forks` is `false`, it is off whatever this says. For repos without a file, `repos.defaults.pull_requests.forks`. |
| `labels` | map of key to value | `{}` | Static labels that attribute this repo's work for cost and usage reporting, e.g. `{team: payments, cost_center: "1234"}`. Each job pod carries them as `labels.devbot.io/<key>`, the job's log records as `labels`, and its usage line on the PR lists them; metrics carry them only where the install lists the key in `metrics.static_labels` ([operating.md](operating.md#cost-and-usage-reporting)). A key is lowercase letters, digits and `_`, starting with a letter, at most 63 characters; a value is a Kubernetes label value: at most 63 of `A-Z a-z 0-9 - _ .`, starting and ending alphanumeric. An invalid one makes the whole file invalid, like a YAML error. For repos without a file, `repos.defaults.labels`. |
| `commits.co_authored_by` | bool | unset: the agent CLI's default | Whether the commits devbot pushes credit the agent. Unset, whatever the agent CLI does by default stands (Claude Code adds `Co-Authored-By: Claude … <noreply@anthropic.com>`; Codex adds nothing unless its own config asks). `false`: no AI co-author trailer and no "Generated with …" line, in the commits or in the PR title and body devbot writes; a person's `Co-authored-by` stays. `true`: each commit carries the co-author trailer of the CLI that ran, in its own words (`Co-Authored-By: Claude <noreply@anthropic.com>`, `Co-authored-by: Codex <noreply@openai.com>`), once. See [Commit attribution](#commit-attribution). For repos without a file, and for a file that leaves it unset, `repos.defaults.commits.co_authored_by`. |
| `commits.trailers` | list of `Key: value` | `[]` | Trailers added to every commit devbot pushes for a task, each once, such as a DCO's `Signed-off-by: devbot <devbot@example.com>`. Each must be one `Key: value` trailer (a key of letters, digits and `-`, a colon, and a value on one line); anything else makes the whole file invalid, like a YAML error. See [Commit attribution](#commit-attribution). For repos without a file, and for a file that leaves it unset, `repos.defaults.commits.trailers`; `[]` here means none, whatever the install's default. |
| `agents.implement.model` | string | deployment's | Model for the implementer on this repo, named the way the deployment's harness for the role names models. A task's own model, set when it is filed or edited through a connector or by a [`/model` comment](forge.md#choosing-the-model), beats it: each role runs on the task's, else this, else the deployment's `agents.<role>.model`, and `repos.limits.models` applies to the task's and this alike, a refused one giving way to the next. For a role the deployment runs on Amazon Bedrock, a Bedrock model ID or inference profile. The harness is the deployment's choice (`agents.implement.harness` in [operating.md](operating.md#deployment-config-config)), because it decides which credential the job pod holds. |
| `agents.review.model` | string | deployment's | The same for the reviewer. |
| `agents.implement.mcp_servers` | list of names | deployment's | Which of the deployment's [MCP servers](operating.md#mcp-servers) for the implementer it gets on this repo: a subset of them, or `[]` for none. A repo can only narrow the list: naming a server the deployment doesn't give the role is a config error, which stops the task at `needs-human` with the error when its job starts, and a repo can't define a server. What the servers return is untrusted, and a browser or a search tool can send what the agent read out of the pod, so `[]` is the choice for a repo whose code or tasks shouldn't reach them. |
| `agents.review.mcp_servers` | list of names | deployment's | The same for the reviewer. |
| `services` | list | `[]` | Service containers (a database, a KMS) that every implement and review run gets its own of, like a CI job's services, reachable from the agent's commands and the checks on `127.0.0.1` at their ports. Only on an install that runs them, with images it allows. See [Services](#services) for each entry's keys: `services[].name`, `services[].image`, `services[].env`, `services[].command`, `services[].args`, `services[].ports`, `services[].ready.tcp`, `services[].ready.exec`, `services[].resources.cpu` and `services[].resources.memory`. |
| `env` | map of name to value | `{}` | Environment devbot exports to the agent's commands and the checks, in both roles: where the tests find the services, say (`APP_TEST_DB_DSN: postgres://postgres:ci@localhost:54337/postgres`). Not for secrets: the model reads it, and it is in the context and the PR. Names devbot sets itself, and loader and interpreter hooks like `NODE_OPTIONS` or `LD_PRELOAD`, are refused (see [Services](#services)). |
| `timeouts.implement` | duration | `60m` | Limit on each implement run of the model, and on the whole attempt. The job pod's `jobs.implement.activeDeadlineSeconds` (Helm) must be longer. |
| `timeouts.review` | duration | `20m` | The same for a review. The pod limit is `jobs.review.activeDeadlineSeconds`. |
| `integration.command` | string | unset: no check | On the repo of a [parent task](architecture.md#parent-tasks): the integration check, a shell command run from a workspace that holds each child task's repo at its PR head, under `<provider>/<owner>/<name>/`, before any child merges. Its exit status is the result. See [Integration checks](#integration-checks). The other `integration.*` keys need it. |
| `integration.timeout` | duration | `30m` | How long the command may run. It runs in an implement job pod, so `repos.limits.timeouts.implement` caps it, and `jobs.implement.activeDeadlineSeconds` (Helm) must leave room for it and the clones. `0` or unset is the default; a negative is an error. |
| `integration.repos` | list of repo refs | `[]` | Repos no child task changes that the command needs too, as full refs (`gitea:acme/schema`), each checked out at its default branch under `<provider>/<owner>/<name>/`. A child's own repo is at its PR head whatever this says. |
| `integration.retries` | int | `2` | How many times a run that couldn't finish (a clone that failed, services that didn't start, a pod that died) is tried again for the same heads before the parent stops at `needs-human`. A command that fails isn't retried: that is the result. At most `10`. |
| `branch_cleanup` | bool | `true` | Whether devbot deletes its `devbot/*` branches once their PR is merged or closed, whoever merged or closed it, and branches left over from attempts that never opened a PR. See [Branch cleanup](forge.md#branch-cleanup) for exactly which branches it deletes and which it never touches. With `false`, devbot deletes none. |
| `branch_cleanup_grace` | duration | `1h` | How long after its PR closed reconcile leaves a branch the task itself didn't delete (a PR merged or closed after its task ended, or one whose task was cancelled). |
| `orphan_branch_grace` | duration | `24h` | How long after its last commit reconcile leaves a `devbot/*` branch that has no PR and no task running. |

## Repos without `.devbot.yaml`

devbot acts like a new contributor:

- **It follows the project's own rules.** The implementer reads `CONTRIBUTING`, the README and any `AGENTS.md`, and follows the project's code style, test conventions, commit-message format and sign-off (it adds `Signed-off-by` when the project requires DCO). It adds no devbot files to the repo and suggests none.
- **It finds the project's own checks.** It works out how the project verifies a change from the CI workflow files, Makefile, package scripts or CONTRIBUTING, runs those, and reports the exact commands in its result. **The harness re-runs exactly those commands** in a fresh shell before opening the PR, so verification stays deterministic. The PR lists them as "found by the implementer", and the reviewer sees them.
- **People merge, and a person approves every CI change.** The built-in default policy is:

```yaml
version: 1
enabled: true
automerge: false
require_checks: false        # a project's CI may not run on PRs from a bot, or at all
# protected_paths, approvable_paths, approval, task_automerge, checks_pending_timeout, max_attempts, check_retries, max_review_rounds, max_rebase_rounds, rebase,
# merge_method, review_budget_kib, timeouts, branch_cleanup, branch_cleanup_grace, orphan_branch_grace: the key defaults above
```

An install can change any of these for every repo without a file, using
`repos.defaults` in its config (written in `.devbot.yaml`'s own keys). A repo's
own `.devbot.yaml` replaces the default policy entirely, with one exception:
[`commits`](#commit-attribution), whose keys a file sets one by one over
`repos.defaults.commits`.

## Runtime setup

The image ships no language toolchains (git, curl and `mise` only), and the
agent runs as root in a throwaway container. Before it changes anything, it:

1. installs languages with `mise use -g <tool>@<version>`
2. installs dependencies with the repo's own tools (`npm ci`, `go mod download`, ...)
3. installs system packages with `apt-get update && apt-get install -y ...`, and starts any services the tests need, such as a database. A service `apt` can't provide comes from [`services`](#services) instead

It writes every command, in order, to `.devbot/setup.sh`. The harness
**sources that script in a fresh shell before running the checks**, so it must
be idempotent and must `export` whatever the checks need, such as a DSN. The
script appears in the PR description. If the repo has no `setup` hints, the
agent's suggested hints are in the PR too, ready to copy into `.devbot.yaml`.

## Docker

A repo's implement and review runs get a Docker daemon of their own, and
`DOCKER_HOST` points the model's commands and the checks at it, when the
repo needs one. Tests that start their own containers through the Docker API
then work in both roles: the reviewer can run them too. The daemon is a
rootless `dockerd` in a sidecar of the run's own pod, with its own storage, so
a run sees only its own containers, images, networks and ports, and they go
when the pod does, however the run ends (see
[operating.md](operating.md#docker)). Repos that don't get one pay for
nothing: their runs have no daemon.

Which repos get one, the first that applies:

1. `docker: true` or `docker: false` in the repo's `.devbot.yaml`.
2. For a repo without the file, a `docker` key in the deployment's
   `repos.defaults`.
3. Otherwise devbot looks at the task's base branch, the branch its PR goes
   into, and gives the runs a daemon when it finds one of these at the root
   or up to two directories down (`services/api/go.mod`, say):

   | Ecosystem | Signal |
   |---|---|
   | Go | `go.mod` requires `github.com/testcontainers/testcontainers-go` or a module under it |
   | Node | `package.json` has `testcontainers` or an `@testcontainers/` package in `dependencies`, `devDependencies`, `peerDependencies` or `optionalDependencies` |
   | Python | `testcontainers` in `pyproject.toml`, `requirements*.txt`, `setup.cfg` or `Pipfile` |
   | JVM | `org.testcontainers` in `pom.xml`, `build.gradle` or `build.gradle.kts` |
   | .NET | `Testcontainers` in a `*.csproj` or `Directory.Packages.props` |
   | Rust | `testcontainers` in `Cargo.toml` |
   | Compose | a `docker-compose.yml`, `docker-compose.yaml`, `compose.yml` or `compose.yaml` |

Whatever the repo says, there is no daemon where the install runs none
(`docker.mode`), or where its `repos.limits.docker` is `false`; and with
`docker.detect: false` only step 1 or 2 turns it on.

Detection reads the base branch, not the PR, so a change can't give itself a
daemon: a dependency a PR adds counts once it has merged. It reads a capped
number of small files, shallowest first, so in a big monorepo a manifest
deep in the list may go unread; `docker: true` settles it. If the forge
won't list the branch's files, the runs get no daemon. When detection turns
the daemon on, the task's start status says so and names the file, as does
the model's context. A repo that doesn't want it, a Compose file that only
deploys, say, opts out with `docker: false`.

```yaml
version: 1
docker: true
checks:
  - go test ./...
```

A testcontainers test needs nothing devbot-specific. `DOCKER_HOST` is a unix
socket, ports a container publishes are on `localhost`, and
`TESTCONTAINERS_RYUK_DISABLED=true` is set, since the run's containers go with
the run anyway. For example, in Go:

```go
func TestQuery(t *testing.T) {
	ctx := t.Context()
	pg, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithPassword("test"),
		postgres.BasicWaitStrategies())
	if err != nil {
		t.Fatal(err)
	}
	dsn, _ := pg.ConnectionString(ctx, "sslmode=disable")
	db, _ := sql.Open("pgx", dsn)
	var one int
	if err := db.QueryRowContext(ctx, "select 1").Scan(&one); err != nil || one != 1 {
		t.Fatalf("select 1: %d %v", one, err)
	}
}
```

The daemon supports the whole Docker API, `docker build` and compose
included. It sees `/work`, where the repo is, at the same path, read-only:
a bind mount of a repo file (`./init.sql:/docker-entrypoint-initdb.d/init.sql`)
has the file, but a container can't write to it.

The install has to run the daemon (`docker.mode`), and the chart runs a
device plugin on the job nodes for its network. What has been checked, and
where, is in [operating.md](operating.md#dind) and [Tested
environments](operating.md#tested-environments).

## Checks

After the model reports `done`, the harness runs, on the agent side:

```bash
cd <repo>
source .devbot/setup.sh          # if present; failing here fails the checks
( <check 1> ) || exit 1
( <check 2> ) || exit 1
...
```

- Each check runs from the repo root, in its own subshell, in order, and the first failure stops the run.
- The whole run has a 30-minute limit.
- Write checks the way you'd type them: pipes, `&&` and `$(...)` all work.
- On failure, the model is resumed with the last 120 lines of output, up to `check_retries` times.
- The final output goes into the PR under "Checks run by devbot".

Checks should be what a reviewer would run by hand: format, vet or lint, unit
tests. Builds, signing and deploys belong in CI.

## Commit attribution

`commits` decides how the commits devbot pushes credit the agent, and which
trailers they carry. With neither key set, on the repo or the install, devbot
leaves commit messages as the agent wrote them, but for a co-author trailer
crediting the commit's own author (below).

```yaml
commits:
  # No "Co-Authored-By: Claude <noreply@anthropic.com>" and no
  # "Generated with Claude Code" line, in the commits or the PR.
  co_authored_by: false
  # The project asks for a DCO sign-off on every commit.
  trailers:
    - "Signed-off-by: devbot <devbot@example.com>"
```

- **Where it applies.** Every commit devbot pushes for the task: the
  implementer's, those of each fix round, and those a rebase rewrites. Review
  comments aren't commits, and are left alone.
- **Two places at once.** devbot sets the agent CLI's own attribution
  setting from `co_authored_by` (Claude Code's `attribution` in a settings
  file of devbot's, which a repo's `.claude/settings.json` can't override;
  Codex's `codex_git_commit` feature), and then holds the commits to it
  itself before pushing them, in case the CLI didn't.
- **`co_authored_by: false`** drops every `Co-authored-by` trailer naming an
  agent vendor's no-reply address (`noreply@anthropic.com`,
  `noreply@openai.com`) and every "Generated with Claude Code" or "Generated
  with Codex" line, from the commit messages and from the PR title, body and
  task comment devbot writes from the agent's result. Trailers naming people
  stay.
- **`co_authored_by: true`** adds the co-author trailer of the CLI that ran
  to each commit that doesn't already have one.
- **`trailers`** are added the way `git interpret-trailers --if-exists
  addIfDifferent` adds them: one already on the commit, with the same key and
  value, isn't added again.
- **Never its own co-author.** A `Co-authored-by` trailer naming the
  commit's own author email (devbot's git identity, in any case) is dropped
  whatever `co_authored_by` says, set or not: a commit crediting its author
  says nothing. The agent is told not to add co-author trailers at all;
  this catches the ones it adds anyway.
- **Only devbot's commits.** devbot changes only commits it made, those its
  own git identity committed. A person's commits on a PR devbot
  [carries](forge.md#handing-devbot-an-existing-pr), or from a fork it
  supersedes, keep their messages, and nothing already on a carried PR's
  branch is rewritten. A commit that already reads as it should is pushed
  unchanged, so a fix round doesn't rewrite the commits before it.
- **Layered over the install's.** `repos.defaults.commits` in the
  deployment config ([operating.md](operating.md#deployment-config-config))
  is the starting point for every repo, with or without a file: a key the
  file sets replaces the install's, and a key it leaves unset keeps it.
  `trailers: []` turns off the install's trailers for the repo.

## Services

Tests that need a service the agent can't install with `apt` (a database
built with an extension, a KMS) get it from `services`, the way a CI job
gets its service containers. Each implement and each review run gets **its
own** instances, started for that run and deleted with it, so any number of
runs, of one repo or many, test at once without sharing a service, a port or
its data.

```yaml
services:
  - name: pg
    image: postgres:17
    env: {POSTGRES_PASSWORD: ci}
    ports: [5432]
    ready: {exec: [pg_isready, -U, postgres, -h, 127.0.0.1]}
env:
  DATABASE_URL: postgres://postgres:ci@localhost:5432/postgres?sslmode=disable
```

| Key | Type | Default | Meaning |
|---|---|---|---|
| `services[].name` | string | required | The service's name, unique in the list: lowercase letters, digits and `-`, starting with a letter, at most 32. |
| `services[].image` | string | required | The image. A short name is Docker Hub's, as Docker reads it (`postgres:17` is `docker.io/library/postgres:17`), and the full name must start with one of the deployment's allowed prefixes or be one of its allowed images (`repos.limits.services.images`), unless the install allows any image. |
| `services[].env` | map | `{}` | The service container's environment. |
| `services[].command` | list of strings | the image's | Replaces the image's entrypoint. |
| `services[].args` | list of strings | the image's | Replaces the image's command, the entrypoint's arguments. |
| `services[].ports` | list of ports | `[]` | TCP ports the service listens on, from 1024 to 65535. The run reaches each on `127.0.0.1` (and `::1`) at the same port; the run's other services do too. No two services may share a port, and `8462` is the model proxy's. |
| `services[].ready.tcp` | port | - | Ready once this port accepts connections. Checked every 2 s. |
| `services[].ready.exec` | list of strings | - | Ready once this command, run in the service's container, exits 0. Checked every 2 s. Set `tcp` or `exec`, or neither: then ready once it runs. |
| `services[].resources.cpu` | quantity | install's | The container's CPU limit, as Kubernetes writes it (`500m`, `2`), up to the deployment's ceiling. |
| `services[].resources.memory` | quantity | install's | The container's memory limit (`512Mi`, `4Gi`), up to the deployment's ceiling. |

How a run gets them:

1. When a job takes its task, it asks serve for the services, which starts
   them in a pod of their own, owned by the job's Kubernetes Job. The
   services share that pod's loopback, so one reaches another on
   `127.0.0.1`, as in a CI job.
2. The run listens on each service port on its own pod's loopback and passes
   every connection to the service pod, so the agent's commands and the
   checks reach a service at `localhost:<port>`. `env` holds the addresses
   the tests read, and the context lists the services and `env`, so the
   implementer and the reviewer both run the tests against them.
3. The model starts only once every service is ready. One whose image can't
   be pulled, that exits (the end of its log is quoted), that Kubernetes
   refuses, or that isn't ready within the deployment's `services.ready_timeout`
   (default 5 minutes) stops the task at `devbot/needs-human` with that
   reason, before the model starts: it isn't an attempt, and fixing the
   cause and adding `devbot/ready` retries it. A service isn't restarted: one
   that exits after it was ready (killed for memory, say), or whose pod is
   evicted or deleted, stops the run there, at `devbot/needs-human` with
   which service stopped and why. That isn't an attempt either, so raise its
   `resources` or fix the cause, then add `devbot/ready`.
4. When the run ends, however it ends, the service pod and its network
   policy are deleted. A pod outlives its run by at most a few seconds if
   serve is running, its role's deadline if serve isn't, and its Job either
   way, since Kubernetes deletes it with the Job.

The install has the last word. A `services` list it won't run (it doesn't
run services at all, an image outside its allowed prefixes, more services
than `repos.limits.services.max`, or a limit over its ceilings) stops the
task at `devbot/needs-human` with the rule it broke, before anything
starts. Each service pod admits its own run's job pod alone, at its ports;
no other run reaches it. See [Service containers](operating.md#service-containers).

`env` can't set what devbot sets itself: `PATH`, `HOME`, the proxy
variables (`HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY` and their lowercase
forms), or anything starting `DEVBOT_`, `ANTHROPIC_`, `CLAUDE_`, `CODEX_`,
`OPENAI_`, `AWS_` or `OTEL_`. Nor can it set a name that has a loader, a
shell or an interpreter run code or read config from where it points:
`env` comes from the base branch, but a value can point into the checkout
(`NODE_OPTIONS=--require ./x.js`), and the agent's own CLI runs with it, the
reviewer's included. Those are anything starting `LD_`, `DYLD_`, `GIT_`,
`XDG_`, `NPM_CONFIG_` or `npm_config_`, and `SHELL`, `BASH_ENV`, `ENV`,
`ZDOTDIR`, `SHELLOPTS`, `BASHOPTS`, `PS4`, `PROMPT_COMMAND`, `IFS`, `GCONV_PATH`,
`EDITOR`, `VISUAL`, `PAGER`, `NODE_OPTIONS`, `NODE_PATH`, `PYTHONSTARTUP`,
`PYTHONPATH`, `PYTHONHOME`, `PYTHONUSERBASE`, `PERL5OPT`, `PERL5LIB`,
`PERLLIB`, `PERL5DB`, `RUBYOPT`, `RUBYLIB`, `JAVA_TOOL_OPTIONS`,
`_JAVA_OPTIONS` and `JDK_JAVA_OPTIONS`. A name that does either makes the
file invalid.

## Protected paths

Unless `protected_paths` is set, by the repo's `.devbot.yaml` or the deployment's `repos.defaults`, these are protected:

```yaml
protected_paths:
  # CI/CD: it runs on push, with the repo's secrets. These are also the
  # default approvable_paths: a person can approve a change to them.
  - .github/**
  - .gitea/**
  - .forgejo/**
  - .gitlab-ci.yml
  - .gitlab/**
  - .circleci/**
  - .buildkite/**
  - Jenkinsfile
  - .pre-commit-config.yaml
  # how git checks a tree out, and who must review it
  - .gitmodules
  - .gitattributes
  - CODEOWNERS              # anywhere: the root, .github/, .gitea/, docs/
```

The [always-protected paths](#how-devbot-reads-it), `.devbot.yaml` and the
agents' configuration, are added to whatever the list is. Whatever the list, a
change also counts as protected when it **adds a symlink** (or turns a file
into one), **retargets an existing symlink**, or **changes a submodule pointer**
(a gitlink) at any path.

devbot reads what a branch changes from git, not from the forge's file list:
every path between the branch and its merge base with the base branch, with
its file mode, renames split into the old path and the new one. Changes the
base branch brought in, by a rebase or a merge, aren't the branch's. It checks
twice:

- **Before pushing.** The harness checks the implementer's commits in its own
  clone. If they touch anything protected, it pushes nothing to the branch: CI
  there would otherwise run the change, with the repo's secrets, before anyone
  had looked at it. A change to a file on `approvable_paths` is held for a
  person's approval ([below](#changes-a-person-may-approve)), unless the
  approval of the task's latest hold is of exactly those changes; anything
  else protected stops the task at `devbot/needs-human`, listing the paths.
- **In the merge gate.** devbot fetches the PR's base and head and checks the
  head it is about to merge. Files on `approvable_paths` pass only as exactly
  the files the latest hold's approval approved; anything else protected stops
  it. If it can't work the list out in full, nothing merges, and the next
  evaluation tries again.

A task that needs any other protected change is for a person. The implementer
is told to say so instead.

### Changes a person may approve

`approvable_paths` lists the protected paths whose changes a person can read
in a diff and approve: by default, the CI/CD configuration above.
`.pre-commit-config.yaml`, `.gitmodules`, `.gitattributes` and `CODEOWNERS`
aren't on it. The always-protected paths, a `CODEOWNERS` or `.gitattributes`
file at any depth (`.github/CODEOWNERS` too, though `.github/**` is
approvable), a symlink and a submodule pointer can never be. Nothing a model
wrote runs as CI until a person has looked at it:

1. **devbot holds the work.** When everything protected that a run's work
   changes is on `approvable_paths`, the harness doesn't push it to the
   branch. It keeps the commit on the forge at `refs/devbot/held/<issue>`,
   with what the PR is to say (the summary, the checks' output, the
   attachments) at `refs/devbot/held-pr/<issue>`. Those are neither branches
   nor tags, and forges run CI for branches and tags alone (see
   [operating.md](operating.md#held-changes-run-no-ci)). A protected file the
   work leaves binary (a NUL byte) or not valid UTF-8 can't be shown, so the
   task stops at `needs-human` instead, as it does when the diff is too big
   for a comment.
2. **It shows them on the issue** and moves the task to
   `devbot/needs-approval`, with a comment holding the full diff of every
   protected file the work changes, the files it changes besides with their
   line counts, the implementer's summary, and a **risk read** of the
   protected diff. The diff shows every file as text and reads no
   `.gitattributes`, the base's or the change's, so no attribute can turn a
   workflow into "Binary files differ". The risk read flags:
   - uses of the repo's secrets it adds: `secrets.NAME`, and any other
     expression that reads them (`secrets['X']`, `toJSON(secrets)`,
     `secrets: inherit`);
   - triggers it adds or widens that run the workflow for devbot's own work:
     a `push` or `create` that reaches `devbot/**`, a `pull_request`, and
     `pull_request_target` and `workflow_run`, which run with the secrets
     whatever started them;
   - `branches-ignore` entries and `if:` conditions it removes that kept jobs
     off `devbot/` branches;
   - third-party actions it brings in, and `uses:` not pinned to a commit
     SHA;
   - a workflow that runs on devbot's work (any trigger above, or
     `workflow_call`) with a token that may write: clean only when it sets a
     top-level `permissions:` that grants no write (`{}`, `read-all`, or
     only `read` scopes, so no `id-token: write`) and no job sets
     `permissions` that grant write. With no top-level `permissions:` its
     jobs get the repo's default token, which is write on Gitea and on many
     GitHub repos. The read judges the workflow as the change leaves it,
     whatever it was before, and a workflow that can't reach devbot's work
     (tags only, `workflow_dispatch`, `schedule`, pushes to branches outside
     `devbot/**`) isn't read for this;
   - `run:` steps, in a workflow or an action, that call a file the same
     commit changes outside the protected paths (by its path, or `make` for
     a changed Makefile, `npm`, `yarn` or `pnpm` for a changed
     `package.json`).

   When a workflow runs on devbot's branches or uses the secrets, the comment
   warns that approving pushes the whole commit, so CI runs the rest of the
   change, which the comment only lists by name, with those secrets, and says
   how to fetch the held commit to read it first. When a workflow does both,
   it warns too that later fix rounds can change the scripts, Makefile or
   tests it runs without touching a protected path, and that those run with
   the secrets with no approval asked for. When a workflow that others can
   call (`workflow_call`) uses the secrets, it warns that it runs wherever
   its callers do, which may be devbot's branches, and that the read doesn't
   follow them. The risk read is a reading aid: it can miss things, and says
   so.
3. **It waits.** A hold isn't a failed attempt and has no timeout, like
   `devbot/needs-info`, and it survives the job pod and devbot restarting:
   the refs and the comment are on the forge. `task_status` reports the state
   as `needs-approval`, with the held commit and the comment under `held`.
4. **Someone who can direct devbot approves it**, by adding `devbot/approve`
   to the issue, or, signed in to devbot through the forge, with the
   `approve_task` connector call naming the held commit they were shown.
   Without sign-in (`auth.mode: none`) `approve_task` is refused: its caller
   is often a model relaying the held change's own words. devbot's own bots
   can't approve, and nor can a login listed in `trusted_reviewers` or
   `required_reviewers`, even with write access. A label from anyone else, or
   one not added strictly after devbot's comment showing the change (in a
   later second, as forges keep time), is ignored with a comment and taken
   off. The label approves the change devbot holds when it is added: if
   devbot has shown a newer one by then, approving from a page that still
   shows the older one approves the newer one, so read the latest comment.
   `approve_task` names the commit, and is refused if it isn't the latest.
   Adding the label in the same edit that removes `devbot/needs-approval`,
   as Gitea's label picker does, approves too: devbot finds the hold still
   waiting in its own comments, puts `devbot/needs-approval` back and checks
   the label as above. On a task in any other state, or with no hold waiting,
   `devbot/approve` is ignored with a comment saying why and taken off.
5. **devbot pushes exactly that commit**, then opens or updates the PR and
   puts it in review as usual. If `refs/devbot/held/<issue>` no longer holds
   the approved commit (something replaced it there), devbot pushes nothing
   and stops at `needs-human`, whatever the replacement changes.

**An approval binds content, not the task.** devbot records the approved
content of each protected file (its path, mode and blob) on the issue. Only
the approval of the task's latest hold counts, and none from before a retry
(`devbot/ready`, `retry`) or a retarget. A later push, a fix round or a rebase
that resolves a conflict included, whose protected changes are exactly the
files approved, as approved, no more and no fewer, goes straight through; any
other is held again, for a new approval. A change the base branch brings in
isn't the branch's. The [merge gate](#the-merge-gate) holds the PR's head to
the same: protected files that differ from the approval, by an edit a person
pushed to the branch after it, say, stop the merge at `needs-human`.

devbot reads holds and approvals from the hidden markers on its own comments,
and anyone with write access can edit any comment. A hold or an approval in
a comment edited since devbot posted it counts for nothing.

Not approving leaves the task at `devbot/needs-approval`; `devbot/ready`
starts it over, and removing the state label cancels it. On GitHub, pushing an
approved change to `.github/workflows/` needs the implementer App's
**Workflows** permission, and GitHub may refuse even the held ref without it;
a refused push stops the task at `needs-human` saying so (see [operating.md](operating.md#ci-on-devbots-branches)).

### Approving without a person

The hold is there so that a workflow a model wrote doesn't run, with the
repo's secrets, before anyone has looked at it, and the risk read says when
that could happen. With `approval: auto_clean` (in `.devbot.yaml`, or
`repos.defaults.approval` for repos without one), devbot lets it decide:

- **A change the risk read finds nothing in is pushed at once**, as if a
  person had approved it, by the run that made it. It isn't kept at
  `refs/devbot/held/<issue>` and the task never waits at
  `devbot/needs-approval`. devbot comments with the same protected diff, the
  rest of the change and the empty risk read, saying it pushed the change
  because the read was clean, then records the approval in a comment of its
  own, then opens or updates the PR as usual.
- **Anything else holds it for a person**, exactly as with `approval:
  person`, and the hold comment says why. Clean means none of the
  [flags above](#changes-a-person-may-approve): no use of the secrets
  added, no trigger added or widened that runs the workflow for devbot's
  work, no guard removed that kept jobs off `devbot/` branches, no
  third-party action brought in and no `uses:` not pinned to a commit SHA,
  no workflow that runs on devbot's work (including through
  `workflow_call`) without a top-level `permissions:` that grants no write
  (`{}`, `read-all`, or only `read` scopes) or with a job whose
  `permissions` grant write (the token would reach whatever a later fix
  round changes in the code it runs), no `run:`
  step calling a file the commit changes elsewhere, and every workflow parsed. It also means none of the warnings: no changed workflow
  that already runs on devbot's branches with the secrets (an edited step there
  runs with them without adding a flagged line), no changed workflow that
  other workflows can call (`workflow_call`) while it uses the secrets
  (its callers decide where it runs, and the read doesn't follow them), and
  none that runs on devbot's branches or uses the secrets while the commit
  changes files outside the protected paths, which it would run
  unreviewed. A change that only removes uses of the secrets or triggers is
  clean.
- **Only Actions workflows.** The risk read parses workflow files in
  `.github/workflows/`, `.gitea/workflows/` and `.forgejo/workflows/`, and
  finds almost nothing in other CI files (`.gitlab-ci.yml`, `.circleci/**`,
  `.buildkite/**`, `Jenkinsfile`) or in scripts and actions an existing
  workflow runs. A change to any of those is held for a person, whatever
  the read says.
- **It binds content the same way.** devbot records what the protected
  files hold as a person's approval records it, so a later push, a fix
  round included, that changes them is held again, and read again, and the
  merge gate merges them only as approved.
- Only `approvable_paths` are affected. A change to a path that always needs
  a person, a protected file it leaves binary or not UTF-8, a symlink or a
  submodule stops at `needs-human` as before, and so does a diff too big to
  show.

The risk read can miss things; `auto_clean` trusts it to find the ways a
workflow change reaches the secrets from devbot's own branches. Keep
`person` where a missed one would matter more than the clicks it saves.
`devbot_held_approvals_total{by="auto"}` counts these pushes, and
`{by="person"}` a person's approvals (see
[operating.md](operating.md#observability)).

## Glob syntax

- `*`, `?` and `[...]` match within one path segment (Go `path.Match`).
- `**` matches any number of directories: `deploy/**` is everything under `deploy/` (and `deploy` itself), and `**/*.sql` is any `.sql` file.
- **A pattern with no `/` also matches by file name anywhere**, like `.gitignore`: `*.sql` catches `db/migrations/001.sql`.
- Paths are relative to the repo root, and a pattern with a `/` in it matches from there: `build/Makefile` is the root's `build/` only, `**/build/Makefile` any of them.
- A leading `/` is ignored and doesn't anchor: `/Makefile` is the same as `Makefile`, a `Makefile` anywhere. To narrow a bare name, write the directory it is in (`dir/name`).

A rename counts as touching both the old and the new path.

## Who can direct devbot

Moving a task, by labelling it `devbot/ready`, answering a question, or
requesting changes in review, takes an **authorized** actor (a change request
also counts from a [trusted reviewer](#trusted-reviewers)). That includes
people using a connector when the install signs them in through the forge
(`auth.mode: oauth`): they act as themselves, and devbot refuses a call these
rules would ignore. So does `devbot/automerge`: the label counts only when an authorized actor added it (or devbot filed the issue with it).

The forge decides who that is. An actor is authorized when:

- they're one of devbot's own bot users (the implementer and the reviewer), always, or
- the forge gives them at least the deployment's minimum permission on the repo
  (`write` unless the install sets `authorization.min_permission`), directly or
  through an org team. The repo's owner has it. **And**, if this file sets
  `authorized_users`, they're on that list.

`authorized_users` only narrows: a listed person without forge access is still
unauthorized, and leaving it unset or empty means anyone with the permission.
There's no install-wide list.

A login in `required_reviewers` is trusted for its reviews of the PR and
nothing else: its `request_changes` on the head is a fix round and its
approval counts in the merge gate, without any forge permission, but it is
authorized for nothing above unless these rules make it so.

To read people's permissions, the implementer bot needs access the install
sets up: on Gitea, admin on the repo or on the org team that holds it (site
admin also works); on GitHub, read access to repo metadata. If the forge
refuses, devbot treats the person as unauthorized, and its comment says it
couldn't check rather than that they aren't allowed. See
[operating.md](operating.md#who-can-direct-devbot).

Everyone else's comments still reach the agent, but fenced and marked
**untrusted**: as information, never as instructions. So does the issue
itself when its author is unauthorized: the person who added `devbot/ready`
approved it as a task, so the agent builds what it describes and is told who
approved it, but takes no instructions from its text. A `devbot/ready` label
added by someone unauthorized is removed, with a comment saying which rule
failed: no forge permission, devbot couldn't check it, or not on this file's
`authorized_users`.

Approval covers the issue as devbot reads it when it accepts the label and
queues the task. devbot hashes the title and body then, records the hash in
the "Working on it" comment, and before every implement, fix and review job
compares the issue with it. When it changed, devbot asks the forge who edited
it since it was approved: if everyone who did is authorized, devbot records
the new text, says who changed it, and carries on. If anyone else edited it,
or the forge doesn't say who, the task stops at `devbot/needs-human` with a
comment saying the issue changed after it was approved. An authorized person
who has read the issue re-applies `devbot/ready` to approve it as it stands.

How much devbot catches depends on what the forge records:

- **GitHub** records who made every title and body edit, so an edit made
  after the label went on but before devbot accepted it is judged the same
  way.
- **Gitea**'s API records who renamed an issue but not body edits at all.
  Renames are judged as on GitHub. Any body edit after devbot hashed the
  issue stops the task, whoever made it. A body edit before that leaves no
  trace, so devbot takes it as approved: one made between the label going on
  and devbot accepting it, or before devbot queues the task again while it
  has no "Working on it" comment yet (a run that failed before posting one,
  or a job lost with Redis). On Gitea, label an issue whose author can't
  direct devbot only when you're happy for it to be worked on as it then
  stands.

To redirect a task after approval, on either forge, use
[`edit_task`](connectors.md#editing-a-task) rather than editing the issue by
hand: devbot makes the edit itself and pins the new text to whoever asked, so
the task carries on without the forge's edit history, and without stopping on
Gitea.

### Trusted reviewers

Review bots (CodeRabbit-style reviewers, SAST and lint bots) usually have
read access only, so their reviews would count for nothing. List them in
`trusted_reviewers` and a `request_changes` review from one on the PR's
current head counts exactly like an authorized person's: it sends the PR
back for a fix round, carrying the review's text and its line comments, and
it holds the merge until it is dismissed or the same login approves a later
head (see [the merge gate](#the-merge-gate)). That is all the list grants: a
trusted reviewer still can't add `devbot/ready`, answer a question, ask for
a rebase, add `devbot/automerge` or call a connector. Without the list, such
a bot's review neither sends the PR back nor holds the merge, but its text
still reaches the agents, fenced as untrusted.

### What the agents see of a PR

Every fix round and every review reads the PR's own conversation, its
reviews and every review's line comments (with the file and line), on the
PRs devbot opened as on the ones it carries. Each is shown with its author
and whether that author is trusted: an authorized person, a
`trusted_reviewers` login and a `required_reviewers` login are; anyone else is fenced and marked
**untrusted**. devbot's own comments and reviews are left out. The reviewer
is told to check that what trusted reviewers found was fixed or answered,
and to weigh untrusted findings as information. A noisy PR is cut to its
newest reviews and comments, 48 KiB of them, with a note of how many were
left out.

### What the reviewer is given

The reviewer works in a checkout of the PR's head, with the base branch at
`origin/<base>`. In its `.devbot/`:

- **`review/index.md`** lists every file the PR changes against its merge
  base: its status (added, modified, deleted, renamed, copied, type
  changed), lines added and removed, the size of its patch and of the file,
  its blob ids, whether it counts as generated and why, and where its patch
  is. The index is never cut short.
- **`review/patches/`** has the whole patch of each file that isn't
  generated, one file each. For anything else the reviewer uses `git diff`
  and `git show` against the base.

A file counts as **generated** when the base branch's `.gitattributes` marks
it `linguist-generated`, when git sees it as binary (by its content and the
base's `.gitattributes`), or when it matches [`generated_paths`](#keys). A
rename counts only when both of its paths do. Nothing on the PR's head
decides it: a `.gitattributes` the PR adds or changes has no effect on its
own review (and `.gitattributes` is a [protected path](#protected-paths)).
Generated files are listed without their content. The reviewer is told to
review the source they're built from, to rebuild them where it can and check
that the committed output is reproduced, and to say in its summary whether
it was. Output the build doesn't reproduce is a defect.

The patches of the files that aren't generated must fit
[`review_budget_kib`](#keys) (512 KiB unless the repo sets it). A PR
within it is reviewed in full and its verdict stands, however large its
generated files are. A PR over it gets no review and no verdict: the task
stops at `needs-human` with the sizes and what a person can do, which is
review it themselves, mark build output as generated, or raise the budget,
then add `devbot/ready`. devbot doesn't record a request for changes or send
it back to the implementer, since nothing was found wrong with it.

## The merge gate

A PR merges only when **all** of these are true. Each failure says what happens instead:

| Condition | If not |
|---|---|
| The PR is against the task's base, and its head was built on it | a PR moved on the forge has its new base adopted; then a rebase onto the new base, which uses no review round (see [retargeting](connectors.md#retargeting)) |
| No `request_changes` review on the current head from the reviewer, an authorized person, a [`trusted_reviewers`](#trusted-reviewers) login, or a `required_reviewers` login (whose last review of the head is what counts) | fix round, with the review's text and its line comments |
| No check outside `human_checks` is pending | wait, up to `checks_pending_timeout` (2h) from when devbot first saw this head's checks pending, its re-run of them or a retry; then `needs-human`, naming the checks still pending, and the task gives up its turn in the rebase queue |
| No check outside `human_checks` has failed | fix round (CI), listing only those checks, with [what CI said](operating.md#what-ci-said) about each in the implementer's context. When every failing check was cancelled rather than failing, devbot re-runs them instead, up to 3 times per head, using no round, then `needs-human` (see [Cancelled checks](operating.md#cancelled-checks)) |
| An `approve` review **from the reviewer bot on the current head commit** (a push after review invalidates it) | queue a review |
| Nothing the PR changes is [protected](#protected-paths), read from git for the current head, but files on `approvable_paths` that are exactly the files the [approval](#changes-a-person-may-approve) of the task's latest hold approved, as approved | `needs-human`; if git can't say, wait and try again |
| There are checks on the head, or `require_checks: false` | wait 10 min from devbot's last push, then `needs-human` |
| Every check the repo's own branch rules require has reported on the head | wait 10 min from devbot's last push, then `needs-human` |
| The forge knows whether the PR conflicts (it may still be working it out), or the PR shares no history with the base, which needs a rebase either way | wait |
| Every `required_reviewers` login's last review of the current head is an `approve` | `devbot/approved`, naming who has approved and who is awaited, up to `checks_pending_timeout` from when devbot first found this head without them; then `needs-human`, naming who is missing, and the task gives up its turn in the rebase queue. With automerge, it merges once they approve |
| Every check matching `human_checks` passes | `devbot/approved`, with a comment naming the checks for a person to act on; no fix round and no timeout. With automerge, it merges once they pass |
| For a child of a [parent task](architecture.md#parent-tasks) whose repo defines `integration`: the parent's last [integration check](#integration-checks) on every child's current head passed | `devbot/approved`, saying what the check waits for; no fix round. With automerge, it merges once a check on its siblings' current heads passes too |
| Mergeable, not behind the base, and sharing history with it: a PR left with no common ancestor by a base force-pushed to a history of its own counts as behind, whatever the forge says of mergeable, and its rebase replays only its own commits (see [a rewritten base](architecture.md#a-rewritten-base)) | with automerge, wait at `devbot/approved` for its turn in the base branch's rebase queue, then a fix round (rebase); without automerge, or while a standing change request holds it (next row) or it waits on either row above, `devbot/approved`, noted as behind, until someone asks for a rebase. With `rebase: eager`, a fix round (rebase) either way |
| No authorized person's or `trusted_reviewers` login's last review of the PR, on any head, is a `request_changes` that hasn't been dismissed | `devbot/approved`, for a person to merge; it merges itself once they approve a later head or the review is dismissed |
| `automerge: true`, or the issue has `devbot/automerge` and `task_automerge` isn't `false` | `devbot/approved`, for a person to merge |

"The base" is the task's base branch: the default branch, unless the task was
filed with a `base` or [retargeted](connectors.md#retargeting). The branch rules
are that branch's, and the merge is into it.

The rows are checked in order and `devbot/automerge` only affects the last three, so it never
bypasses a review, CI, protected paths, a required reviewer, a human check, an integration check, a rebase or a standing change request. Merges are serialized per repo. A per-task merge
says so in its comment on the issue and in the `task.merged` event.

### Rebasing

An approved PR isn't rebased just because its base moved, unless the repo sets
`rebase: eager`: one merge would otherwise send every approved PR on the base
back for a rebase and a re-review at once.

- **With automerge**, and no person's change request holding it, approved PRs
  that are behind their base queue per base branch, oldest approval first, and
  only the first is rebased. Once it merges or drops out (stops, is closed or
  cancelled, or is retargeted to another base), the next one goes. The others
  wait at `devbot/approved`, saying which task they are queued behind.
- **Without automerge**, the PR waits at `devbot/approved`, and its status notes
  that it is behind. A person decides when it is rebased.

Anyone who [can direct devbot](#who-can-direct-devbot) can ask for a rebase at
any time: add `devbot/rebase` to the issue, comment `/rebase` on it, or call the
`rebase` connector action. The rebase happens once the PR is approved and green,
ahead of any queue, and devbot then takes the label off. A request from anyone
else is ignored. When the repo's own rules only merge an up-to-date branch, the
approval comment says that the PR needs a rebase before it can merge, and how
to ask for one. A rebase devbot starts uses no review round as long as it turns
out clean (see `max_review_rounds`).

The repo's own branch rules (branch protection, rulesets) still apply to the
merge itself, which the forge enforces. When it refuses a merge the gate
passed, devbot stops at `needs-human` rather than trying again, quoting the
forge and listing the rules it reports. Exempt devbot's reviewer from them (the
comment says how on that forge) and retry, or meet them yourself and merge.

Any other error from the merge isn't taken at its word: devbot reads the PR
again first, and a PR that merged after all (a reply lost to a timeout) is
recorded as merged. A head that moved since the gate read it goes back
through the gate on the next evaluation. Anything else that may pass (a
timeout, a server error, a rate limit, the forge still checking the PR, or
the forge saying the head moved when it didn't) is retried on later
evaluations, waiting as for the model's usage limits (`backoff.base`,
doubling up to `backoff.cap`), and stops at `needs-human` after
`backoff.max_retries` failures in a row on the same head. A new head starts
the count over.

## Integration checks

Each child of a [parent task](architecture.md#parent-tasks) passes its own
repo's CI, which tests it against the other repos as they are, not against
its siblings' PRs. On the parent's repo, `integration` checks them together
before any of them merges:

```yaml
integration:
  command: make -C github/acme/app e2e   # from the workspace root
  timeout: 30m
  repos: [github:acme/schema]            # needed, but changed by no child
```

- **When it runs:** once every child with an open PR has it approved by
  devbot's reviewer with its checks green on its current head (the child
  waits at `devbot/approved`), and again whenever one of those heads moves
  after that. A child without a PR doesn't hold it up (its repo is checked
  out at its default branch), and one already merged is checked at the head
  it merged with.
- **Where:** in an implement job pod, with the egress, `services`, `env` and
  Docker daemon of the parent's repo, as an implement run's checks have, and
  no model. The harness clones each head with the implementer's credential,
  in its own container, and hands the agent container a copy: every child's
  repo at its PR head (at its default branch for a child without a PR yet),
  and each of `repos` at its default branch, under
  `<provider>/<owner>/<name>/` of the workspace root, which is where the
  command starts. No forge credential is in the workspace or the command's
  environment, and `DEVBOT_INTEGRATION_HEADS` lists the heads checked
  (`<task>@<sha>`, space-separated). Two children of one repo can't share its
  directory, so the check fails saying so: give each repo one child.
- **The result** is a comment on the parent naming the heads checked, with
  the end of the command's output when it failed, and the
  `devbot/integration` commit status on each child's head. A result counts
  for exactly those heads. On GitHub the status takes the
  implementer App's Commit statuses write; without it, the result is on
  the parent alone. The children's own gates leave that status out of
  their CI: a failure there sends no child back.
- **The merge gate:** a child merges, by automerge or `devbot/automerge`,
  only once the parent's last result for every child's current head passed.
  Until then it waits at `devbot/approved`, and its comment and
  `task_status` say what it waits for. A person can still merge it by hand.
- **A failure** puts the parent at `needs-human` with the output, and is not
  run again on the same heads. A person decides which child to send back
  (`edit_task`, or a review on its PR): its new head runs the check again.
  Retrying the parent runs it again on the same heads.
- **A run that couldn't finish** (a clone that failed, services that didn't
  start, a pod that died) is tried again, up to `integration.retries` times,
  then the parent stops at `needs-human` too. Like any job, it waits out
  the fleet's [usage-limit](architecture.md#usage-limits) hold.

Without `integration`, a parent's children merge as any task does.

## Examples

A Go service with a database in its tests, merged by people:

```yaml
version: 1
enabled: true
automerge: false
setup:
  - mise use -g go@1.26
  - apt-get update && apt-get install -y postgresql-17   # start it on a free port, export DATABASE_URL
checks:
  - test -z "$(gofmt -l .)"
  - go vet ./...
  - go test -count=1 ./...
protected_paths:             # replaces the default list, so name the CI config too
  - .gitea/**
  - deploy/**
  - "*.sql"
```

A repo whose data-layer tests need a Postgres built from its own image and an
OpenBao dev server that Postgres keeps its keys in, as its CI runs them. Without the
services, those tests skip, and the suite passes without them:

```yaml
version: 1
enabled: true
services:
  - name: openbao
    image: openbao/openbao:2.4.1
    env: {BAO_DEV_ROOT_TOKEN_ID: root, BAO_DEV_LISTEN_ADDRESS: 0.0.0.0:18237}
    ports: [18237]
    ready: {tcp: 18237}
  - name: pg
    image: registry.example.com/ci-postgres:17
    # openbao is on the pod's loopback too
    env: {POSTGRES_PASSWORD: ci, PGPORT: "54337", BAO_ADDR: http://127.0.0.1:18237, BAO_TOKEN: root}
    ports: [54337]
    ready: {exec: [pg_isready, -U, postgres, -h, 127.0.0.1, -p, "54337"]}
    resources: {cpu: "2", memory: 2Gi}
env:
  APP_TEST_DB_DSN: postgres://postgres:ci@localhost:54337/postgres?sslmode=disable
  APP_TEST_KMS_ADDR: http://localhost:18237
setup:
  - mise use -g go@1.26
checks:
  - go test -count=1 ./...
```

The install has to allow both images, with `repos.limits.services.images:
[registry.example.com/, docker.io/openbao/]`.

A repo where people merge, and a task can't ask to merge itself because a
merge to `main` deploys:

```yaml
version: 1
enabled: true
automerge: false
task_automerge: false
checks:
  - make test
```

A low-stakes repo where devbot merges its own work:

```yaml
version: 1
enabled: true
automerge: true
merge_method: squash
max_review_rounds: 2
checks:
  - npm ci && npm test
agents:
  review:
    model: claude-sonnet-5-5    # a different model reviews
```

Whether or not a repo has a file, both bot users need **write** access. If
`require_checks` is on, which is the default for a repo's own file, the repo
also needs CI that reports a status on pull requests.
