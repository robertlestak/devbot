# Changelog

All notable changes to devbot are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and devbot uses
[Semantic Versioning](https://semver.org/spec/v2.0.0.html). Before 1.0, a
minor release may break things; each breaking change is listed under
**Breaking** with what to change.

## [0.1.0] - 2026-10-18

The first public release.

### Added

- Tasks filed as forge issues, from the MCP connector, the CLI, the HTTP API
  or a `devbot/ready` label, each worked by an implementer pod that opens a
  PR and reviewed by a reviewer pod with a different identity.
- Hand devbot an existing pull request with `devbot/ready`, a same-repo PR
  or one from a fork: devbot opens a tracking issue and carries the PR to
  merge through the same review, fix rounds and merge gate, on its own
  branch (or the fork's, when it allows edits by maintainers and devbot can
  push there) or on `devbot/pr-<N>` with the contributor's commits and
  credit. A fork PR's code is always read from the repo's
  `refs/pull/<N>/head`, and a fork that turns devbot away, at adoption or at
  a push, moves the PR onto `devbot/pr-<N>` with a comment saying why,
  without costing an attempt. Toggled per
  install and per repo with `pull_requests.enabled` and
  `pull_requests.forks` ([Handing devbot an existing PR](docs/forge.md#handing-devbot-an-existing-pr)).
- A deterministic merge gate: green CI, an approval pinned to the head
  commit, and no protected paths touched beyond what a person approved;
  otherwise a person decides.
- Commit attribution settings, per install (`repos.defaults.commits`) and
  per repo (`commits` in `.devbot.yaml`): `co_authored_by` keeps the agent's
  AI co-author trailer and "Generated with" line out of devbot's commits and
  PRs (`false`) or makes sure it's there (`true`), and `trailers` adds lines
  such as a DCO's `Signed-off-by` to every commit devbot pushes. Unset, the
  agent CLI's own default applies ([Commit attribution](docs/devbot-yaml.md#commit-attribution)).
- Gitea, GitHub.com and GitHub Enterprise Server, with GitHub Apps as the bots.
- Per-repo policy in `.devbot.yaml`, with service containers and a per-run
  rootless Docker daemon for repos that need them.
- OAuth sign-in through the forge for connector clients, an event feed by
  origin, and a read-only web UI.
- A Helm chart that scales job pods from zero with KEDA (2.16 or later), and
  multi-arch images: `devbot` (serve, the harness, the model proxy and the
  CLI, with no agent CLI) and `devbot-codex` (the Codex agent sidecar).
- Claude Code's image isn't published, because Anthropic's terms don't allow
  redistributing Claude Code: build it yourself with
  `docker/claude.Dockerfile` and set `image.agents.claude.repository`
  ([Agent images](docs/operating.md#agent-images)).
- The `devbot` CLI for linux and darwin on amd64 and arm64, attached to the
  release with a `checksums.txt`.
- `imagePullSecrets` for images in private registries, such as the claude
  image you build: set on every pod the chart runs and on the service pods
  serve makes, whose admission policy allows exactly those names. Only the
  kubelet reads them; no container mounts one.
- `required_reviewers` in `.devbot.yaml` and `repos.defaults`: logins, such
  as another review bot, that must each approve a PR's current head before
  devbot merges it, on top of devbot's own reviewer. The PR waits at
  `devbot/approved` for them, up to `checks_pending_timeout`, then stops at
  `devbot/needs-human` naming who is missing; a `request_changes` from one
  is a fix round with its text, and a push needs fresh approvals. They need
  no write access and can't direct devbot otherwise. The approval comment
  says which have approved and which are awaited.
- `human_checks` in `.devbot.yaml` and `repos.defaults`: check names (globs)
  only a person can make pass, such as a CLA. One that fails or is pending
  starts no fix round and uses no round or pending timeout; the PR waits at
  `devbot/approved` with a comment naming it, and the gate carries on (and
  merges, with automerge) once it passes. A fix round for other failing
  checks lists only those.
- CI fix rounds and the reviewer are told what CI said about each failed
  check: the last lines of a failed Gitea or GitHub Actions job's log, a
  GitHub check run's output and every page of its annotations (from any
  App), and for a plain commit status a note that its details are behind
  its link. It is fenced as
  untrusted, with terminal escapes and control characters removed, capped
  per job by `ci_failures.log_lines` (default 150) and in all by
  `ci_failures.max_bytes` (default 64 KiB). Details the forge won't give (an
  expired log, a Gitea older than 1.24, a missing permission) leave a note
  and the round goes ahead. On GitHub, job logs need the Actions read
  permission, which the reviewer App asks for too when granted
  ([What CI said](docs/operating.md#what-ci-said)).
- `trusted_reviewers` in `.devbot.yaml` (and `repos.defaults`): review, SAST
  and lint bots whose `request_changes` review on a PR's head sends it back
  for a fix round and holds the merge until dismissed or approved on a later
  head, as an authorized person's does, without the forge write access that
  would also let them direct devbot. Being listed grants nothing else
  ([Trusted reviewers](docs/devbot-yaml.md#trusted-reviewers)).
- Every fix round and every review reads the PR's conversation, its reviews
  and their line comments (with file and line), on devbot's own PRs as on
  carried ones, each labelled with its author and whether it is trusted (an
  authorized person, or a `trusted_reviewers` or `required_reviewers` login);
  untrusted ones are fenced, devbot's own are left out, and a noisy PR is cut
  to its newest 48 KiB. A review's line comments also go into the fix round it
  starts. The reviewer is told to check that trusted reviewers' findings were
  addressed or answered.
- A fix round whose `fix-requested` comment was posted but whose label change
  to `devbot/changes-requested` or job then failed (a forge 5xx, say) is
  finished by the next evaluation, of any kind of round, including after a
  restart, instead of leaving the task in review with nothing queued. A round
  already queued, running or answered by a push isn't queued again.
- A `/base <branch>` comment on a task's issue, from someone who can direct
  devbot, chooses the branch its work goes into. Before devbot accepts the
  task it sets the base the task starts from (the latest wins); after, it is
  a retarget, as the `retarget` connector call does. A branch that doesn't
  exist or isn't a plain name is refused with a comment
  ([Choosing the base branch](docs/forge.md#choosing-the-base-branch)).
- `base_branch` in `.devbot.yaml` (and `repos.defaults`): the base branch of
  every task in the repo that doesn't name its own, for a `develop`/`main`
  layout where `main` is the forge's default branch. An issue labelled
  `devbot/ready` in the forge UI branches from it and opens its PR against it,
  and Docker detection, the rebase queue, branch rules and required checks,
  the merge, `task_status` and events all follow it. A task's own base
  (`file_task`'s `base`, `/base`, `retarget`, a PR base changed on the forge,
  a carried PR's base) wins over it. The base is fixed when devbot accepts the
  task and recorded by its first run, so editing `base_branch` doesn't move
  tasks in flight. A `base_branch` the repo doesn't have stops the task at
  `needs-human` naming it. `.devbot.yaml` is read only from the default
  branch.
- The reviewer gets `.devbot/review/index.md`, listing every changed file,
  and a whole patch per file in `.devbot/review/patches/`, so a large PR is
  reviewed in full rather than cut off. Generated files (`linguist-generated`
  or binary by the base branch's `.gitattributes`, or matching
  `generated_paths`) are listed without their content; the reviewer reviews
  their source, rebuilds them where it can and says whether the committed
  output was reproduced. A PR whose other patches come to more than
  `review_budget_kib` (default 512) gets no review and no verdict: the task
  stops at `needs-human` with the sizes and what a person can do. A repo
  that commits build output should list it in `generated_paths` or mark it
  `linguist-generated` ([What the reviewer is given](docs/devbot-yaml.md#what-the-reviewer-is-given)).
- GitHub at org scale. Reconcile finds labelled issues and PRs with the
  search API, one query per state label per installation of the
  implementer App, so a pass costs the same however many repos the App is
  installed on; a query over the search API's 1,000 results is split by
  creation date, never cut short. The installations and their repos are
  listed in full once an hour, or after an `installation` or
  `installation_repositories` webhook, and a list that fails keeps the last
  one and is counted in `devbot_search_errors_total`. Search results can
  lag a minute or so behind GitHub, which the next pass makes up for
  (Finding work under [GitHub](docs/operating.md#github)).
- Webhook events that can't concern devbot are dropped before any forge
  call or `.devbot.yaml` read, on GitHub and Gitea alike: an issue, PR or
  comment event needs a `devbot/*` label, a `devbot/*` branch, a task's PR
  or a comment that reads like a devbot command, and a status or check
  event needs the head of a task's PR, which it evaluates directly. Tasks'
  PRs are indexed in Redis (`devbot:tracked:*`) by each evaluation, so a
  reconcile pass rebuilds the index after a wipe, and reconcile still
  catches anything dropped. Dropped events are counted in
  `devbot_webhook_events_skipped_total` by kind
  ([Skipped webhooks](docs/architecture.md#skipped-webhooks)).
- GitHub rate limits are shared and paced. What each answer says of a
  credential bucket's limit is recorded in Redis
  (`devbot:quota:<provider>`), so once one serve replica or job pod finds an
  installation's primary limit spent, none sends another request on it
  until the reset. Below `providers[].low_quota_percent` (default 10%) of an
  installation's core limit, reconcile's discovery and branch sweep and
  webhook events no task stands behind wait for the reset, while the
  tasks' own evaluations, merges and job pods go on until the limit
  ([GitHub rate limits](docs/operating.md#github-rate-limits)). The chart's
  `devbot-harness` Redis user can read and write `devbot:quota:*`; a Redis
  of your own needs the same. A webhook event whose handling hits a forge
  rate limit is kept in Redis and handled after the reset, coalescing the
  events about one issue or PR, and left to reconcile after five tries
  ([Rate-limited events](docs/architecture.md#rate-limited-events)).
- Forge metrics: `devbot_forge_requests_total` (by credential bucket,
  method, endpoint template and status class, `304` apart),
  `devbot_forge_ratelimit_remaining`, `_limit` and `_reset_seconds` per
  bucket and resource, `devbot_forge_ratelimit_errors_total`,
  `devbot_webhook_requeue_total`, `devbot_webhook_events_kept` and
  `devbot_reconcile_deferred_total`
  ([Observability](docs/operating.md#observability)).
- Connector calls at scale. `list_tasks` with a `repo` reads only that
  repo's issues; without one it uses each provider's search. A label or
  provider that fails doesn't hide the rest: the HTTP API returns what was
  read with an `X-Devbot-Incomplete` header, and the MCP tool adds an
  `incomplete: …` text. A forge's rate limit reaching a connector call is a
  `429` with `Retry-After` and the reset time, on the HTTP API and the MCP
  tools; `file_task` says `nothing was filed` when the limit came before
  the issue, and names the issue when it came after. A connector call
  waits out rate limits for at most 10 seconds in all. A `repo` or `ref` on
  a provider the install doesn't have says which providers it has, and the
  MCP tools' descriptions and instructions name the install's providers in
  their examples ([Tools](docs/connectors.md#tools),
  [Errors](docs/connectors.md#errors)).
- No size of repo, org or instance stops devbot working. devbot reads,
  creates and repairs only its own labels, by name, in the same number of
  requests whatever the repo has (on Gitea this needs 1.20 or later). Every
  other list is read to its end: open PRs, issue events and timelines,
  reviews, branches, statuses on a commit, repos on a Gitea instance and
  issues in a Gitea search. Docker detection lists a tree of any size on
  Gitea, and reads a GitHub tree cut short a directory at a time. A GitHub
  search that grows past 1,000 results while it is read is split like one
  that started there. The in-memory caches (task details, bases, change
  sets, Docker detection) drop one entry when full. The limits that remain,
  and what each affects, are in
  [Scale limits](docs/operating.md#scale-limits);
  `devbot_docker_detection_errors_total` counts jobs queued without a
  Docker daemon because detection couldn't tell whether the repo needs one.

- On GitHub, the implementer's tokens ask for **Workflows: write** when the
  implementer App's installation grants it, so devbot can push workflow
  changes a person approved (or that a repo lets it make by dropping
  `.github/**` from its `protected_paths`), and a rebase onto a base whose
  workflows changed can be pushed.
  The permission is optional, and the reviewer's, search and listing tokens
  never ask for it. Workflows on `devbot/*` run with the secrets that reach
  the branch, so keep them in environments with deployment-branch limits
  first ([Workflow files and rebases](docs/operating.md#workflow-files-and-rebases)).
- A push the forge refuses for changing workflow files says, in the task's
  comment, whether the implementer's token had the Workflows permission, and
  so whether to grant it or look for a rule blocking workflow changes.
- Changes to protected CI files held for a person's approval
  ([Changes a person may approve](docs/devbot-yaml.md#changes-a-person-may-approve)).
  When everything protected that the work changes is on the
  `approvable_paths` key (default: the CI/CD entries of the default
  `protected_paths`), devbot keeps the commit at `refs/devbot/held/<issue>`,
  which runs no CI, labels the issue `devbot/needs-approval`, and comments
  with the full diff of the protected files (as text, whatever
  `.gitattributes` says), a risk read of it (uses of secrets it adds,
  triggers reaching `devbot/**`, `pull_request_target` or `workflow_run`,
  removed guards, new or unpinned actions, `run:` steps calling files the
  commit changes elsewhere), a warning when approving would run the rest of
  the commit with secrets, and the rest of the change. A protected file left
  binary or not UTF-8 stops at `needs-human` instead. Someone who can direct
  devbot approves with the `devbot/approve` label, added after the
  change was shown, or, signed in through the forge, the `approve_task`
  connector call (`POST /api/task/approve`, `devbot approve REF SHA`), and
  devbot pushes exactly that commit; if the held ref holds anything else by
  then, it stops at `needs-human`. devbot's bots and the logins listed in
  `trusted_reviewers` or `required_reviewers` can't approve. The approval
  of the latest hold binds the files' content: later pushes and the merge
  gate are held to exactly those files as approved, and a retry or retarget
  starts over. A hold or approval in a comment edited since devbot posted it
  counts for nothing. `task_status` shows the held change under `held`, with
  a `warning` that its text is the change's own, and the event feed has
  `task.needs_approval`. `CODEOWNERS` and `.gitattributes` are never
  approvable, at any depth, and listing an always-protected path, or a path
  inside one, in `approvable_paths` makes the file invalid.
- `hack/held-ref-check`, which checks against a live Gitea that a push to a
  held ref starts no Actions run.
- Waits and notes across repos: a task can wait on, or leave a note for, a
  task of another repo on any provider the install has, named by its full
  ref (`gitea:acme/lib#12`), in `wait_task`, `POST /api/task/wait`,
  `devbot wait`, a `/wait` comment, the implementer's `wait_on`, and
  `devbot coord note` and `show`. `#12` still means the task's own repo.
  Whoever sets the wait or sends the note must be able to direct devbot on
  both repos: the person or signed-in caller, or, for the implementer, the
  task's requester; a login counts on its own forge only. Cycles are found
  across repos. When the other task's PR merges into its own base, the
  waiting task is released with `mode: merged-other-repo` and goes on as it
  is, with no rebase, in the same evaluation that handled the merge: the
  tasks waiting from other repos are found through a Redis index,
  `devbot:waiters:<ref>`, that each reconcile pass rebuilds
  ([Across repos](docs/architecture.md#across-repos)).
- Parent tasks, which group the tasks of one change across repos
  ([Parent tasks](docs/connectors.md#parent-tasks)). `file_project`
  (`POST /api/projects`, `devbot project`) files a parent: an issue
  labelled `devbot/parent`, which is not a state label and never queues an
  implement or review job, with a checklist of its children that devbot
  keeps up to date (a `devbot:checklist` comment, edited in place).
  `file_task` takes `parent` (`devbot file -parent`) to file a child in
  any repo on any provider, refused, with nothing written, unless the
  parent is an open parent task that isn't a child itself and the caller
  may direct devbot on both repos.
  The parent rolls up from its children on every evaluation, and at once
  when a child moves: `needs-human` while a child is stopped there, naming
  it; a note on a child in `needs-info` or `needs-approval`; and, once every
  child has merged, a summary of each child and its PR, and its close.
  `cancel` on a parent cancels its open children and closes it, and `retry`
  retries its stopped children. `task_status` on a parent has `kind:
  parent`, its rolled-up `state` and `children`; a child's status and
  events carry `parent`; `list_tasks` takes `parent` and `state: parent`.
  The parent links a stopped child rather than quoting its reason, and a
  caller sees only the children in repos they can read.
  New events: `parent.filed`, `parent.needs_human`, `parent.completed`,
  `parent.cancelled`. A child's in-flight listing and `devbot coord list`
  include its siblings in other repos, with their state, PR and changed
  files. The web UI lists parent tasks first, and shows a parent with its
  children and a child with its parent. Editing the checklist is audited as
  `forge.comment.update`.
- Release gates on waits across repos: a wait on a task of another repo can
  hold past its merge until the work is published, with `until tagged` (or
  `until tagged v*`, for tags matching a glob) after the task in a `/wait`
  comment, `wait_task`'s `on` or its new `until`, `devbot wait`, or the
  implementer's new `wait_until`. Once the other task's PR has merged, the
  waiting task stays held (no implement job, its PR doesn't merge) until a
  tag of that repo contains the merged commit, then is released with
  `mode: tagged` and a comment naming the tag to depend on. `task_status`
  shows `waiting_for` (`merge`, then `tag`) and `wait_until`. devbot reads
  the newest tags on every evaluation, reconcile's included, as forges
  don't always send a webhook for a tag; a tag's push (or Gitea's `create`)
  webhook releases it at once, through a Redis index,
  `devbot:waiters:tags:<repo>`, that each reconcile pass rebuilds. A wait in
  the same repo refuses the gate. `forge.Provider` gains `ListTags` and
  `Contains` (the forge's compare API), and `forge.PR` the merged commit,
  on Gitea, GitHub and the fake forge
  ([Release gates](docs/architecture.md#release-gates)).
- Building against an unmerged dependency: a gated wait on a task of
  another repo can end in `, build against` (`/wait gitea:acme/lib#12 until
  tagged, build against`, `wait_task`'s `on` or its new `build_against`,
  `devbot wait`, the implementer's `wait_until: "tagged, build against"`),
  refused without the gate or in the same repo. While the other task has a
  PR, the waiting task isn't held: each implement run gets that PR's head
  checked out by the harness, with the implementer's credential for the
  other repo, as plain files at `.devbot/deps/<provider>/<owner>/<name>/`,
  and `context.md` names it and its head and says to wire it in the way the
  project uses a local checkout of a dependency, kept out of the commits.
  devbot knows no ecosystem: the implementer wires it, and the harness
  refuses to push work whose diff mentions `.devbot/deps`, failing the
  attempt with that reason. The waiting task's PR isn't reviewed, merged or
  sent CI fix rounds meanwhile (a `building` status says so once;
  `task_status` shows `build_against` and a `note`). When a tag releases the
  wait, the PR goes back for one `release` round, which uses no review
  round, without the checkout, to depend on the other repo at that tag and
  drop any local override; then it is reviewed and merged as usual. If the
  other task is cancelled or stops at `needs-human`, the wait is released as
  any is and the next round builds without the checkout. The reviewer of a
  task that built against another is told to refuse a PR that points its
  dependency on that repo at `.devbot/deps` or any other local or
  unreleased copy of it
  ([Building against an unmerged dependency](docs/architecture.md#building-against-an-unmerged-dependency)).
- Integration checks across a parent's children
  ([Integration checks](docs/devbot-yaml.md#integration-checks)). A new repo
  key, `integration` (`command`, `timeout`, `repos`, `retries`), on a parent
  task's repo runs `command` from a workspace holding each child's repo at
  its PR head, and each of `repos` at its default branch, under
  `<provider>/<owner>/<name>/`, once every child with an open PR is
  approved with green checks on its head, and again whenever one of those
  heads moves. It is a new job kind, `integrate` (`devbot:q:integrate` and
  `devbot:q:integrate-docker`), which runs no model: implement pods take it
  when no implement job waits, and the chart's implement ScaledJobs scale
  on both lists. The result is an `integration` status on the parent naming
  the heads it covers, and the `devbot/integration` commit status on each
  child's head (`forge.status.set` in the audit log; on GitHub, it takes the
  implementer App's optional Commit statuses write). A child of such a
  parent merges only once the result for every child's current head passed,
  waiting at `devbot/approved` until then, which its `task_status` notes; a
  failure puts the parent at `needs-human` with the end of the output and
  emits `parent.needs_human`. A run that couldn't finish is retried up to
  `integration.retries` times, and `retry` on the parent runs a failed check
  again. Without `integration`, nothing changes.
- Plan jobs: devbot decomposes a change across repos into a parent's child
  tasks, for a person to approve
  ([Planning a parent task](docs/connectors.md#planning-a-parent-task)).
  `file_project` (`POST /api/projects`, `devbot project`) takes
  `acceptance_criteria` (`-ac`), and `plan: true` (`-plan`) with `repos`
  (`-repos`), the candidate repos, each of which the caller must be able to
  direct devbot on, or nothing is filed. The parent's evaluation queues a
  new job kind, `plan` (`devbot:q:plan`), which implement pods without a
  Docker daemon take when no implement job waits, before an integration
  check; the chart's implement ScaledJob scales on it too, and KEDA's Redis
  user may read it. The harness clones each candidate repo with the
  implementer's credential and hands the model read-only copies under
  `<provider>/<owner>/<name>/`, with no forge credential anywhere it can
  read, and a new prompt, `internal/prompts/plan.md`. The model's plan (a
  contract, and children with their repo, title, body, acceptance
  criteria, base and `depends_on`, with `until` and `build_against`) is
  checked, and one that fails is a failed attempt, with its reasons on the
  parent for the next one; `max_attempts` bounds them, and a usage limit
  isn't one. A valid plan is posted on the parent with its hash: the parent
  is `needs-approval` (`planning` before that), `task_status` shows the
  plan under `held` (`kind: plan`), and `parent.plan_proposed` is emitted.
  `approve_task` with the parent and the hash (a signed-in person), or
  `devbot/approve` on the parent, approves it under the rules of a held
  change (`parent.plan_approved`); then serve files each child with
  `parent`, its wait and gate set before anything is queued for it, once.
  `edit_task` on the parent (a `note` alone will do) has it planned again.
  Each child's `.devbot/context.md` has the plan's contract, which the
  implementer builds to and the reviewer checks the PR against, with its
  compatibility with the siblings' current versions. An implementer may
  finish with `status: contract-change` and a `reason`: the child is held,
  and the parent is planned again from its children as they stand; the
  approved replan edits kept children (as `edit_task` does), files new ones
  and cancels those it leaves out. A new repo key, `max_replans` (default
  2), bounds the replans, then the parent is `needs-human` until it is
  retried. `retry` on a parent also plans again when its plan stopped
  ([Plan jobs](docs/architecture.md#plan-jobs)).
- `devbot/automerge` on a parent task is carried to its child tasks: set it
  with `file_project` `automerge` (`POST /api/projects`, `devbot project
  -automerge`) or add it to the parent by hand. A child filed under the
  parent, by `file_task` or by an approved plan, gets it, and adding or
  removing it on the parent later does the same once on every open child;
  a person's change on one child afterwards stands. It is carried only to
  child repos where whoever set it may direct devbot, and the parent says
  which children it left out and why. The merge gate is unchanged: each
  child still merges only once its own gate passes, after any task it waits
  on and the parent's integration check. `task_status` on a parent shows
  `automerge`, and its checklist marks the children that have it
  ([Parent tasks](docs/connectors.md#parent-tasks)).
- A model run that ends without a result says why on the issue, in one
  line: `the model run exited 1 after 47m: <last stderr line>`,
  `the model run exited 137 (OOM-killed at the 8Gi limit after 28m)`, or
  `the agent stopped answering (EOF after 28m)`. In the last case, with
  `podLabels.enabled` or `services.enabled`, serve reads how the agent
  container ended from the pod, and the line adds it
  (`; its container ended OOMKilled, exit 137`). The agent answers every
  run, a stopped or failed one too, with the CLI's exit code, signal,
  duration, stderr and stdout tails and whether the OOM killer ended it, and
  logs the same as `run ended`. The `agent.run` audit record carries those
  fields ([A model run fails](docs/operating.md#a-model-run-fails)). With
  your own Redis, give `devbot-harness` read access to
  `devbot:agent-exit:*` (`%R~devbot:agent-exit:*`), or the EOF line goes
  without the container's end.
- A base branch force-pushed to a history of its own (a re-squash of
  `main`, say) leaves an open PR with no common ancestor. On Gitea and
  GitHub alike that PR counts as behind and never merges, automerge or not,
  whatever the forge says of mergeable, and `/rebase` always rebases it. Its
  rebase round says the base was rewritten and has the implementer replay
  only the PR's own commits onto the new base, cut at the base commit the PR
  was last built on, which every push records on its `pr` status
  (`base_sha`). A push without the new base in its history, or still with
  the old one, fails the attempt instead of being pushed
  ([A rewritten base](docs/architecture.md#a-rewritten-base)).

- `approval` in `.devbot.yaml` and `repos.defaults`: `person` (the default)
  keeps every change to `approvable_paths` waiting for a person, and
  `auto_clean` has devbot push one at once when the risk read of its
  protected diff finds nothing, with a comment showing the diff and why it
  was pushed. Any flag still holds it for a person, and the hold says why:
  among them a workflow that runs on devbot's work without a read-only
  top-level `permissions:` or with a job that grants write, a changed
  workflow that already runs on devbot's branches with the secrets, a
  changed reusable workflow (`workflow_call`) that uses them, one that
  would run the rest of the commit, or a protected file other than an
  Actions workflow. The pushed files are recorded as approved, so later fix
  rounds and the merge gate treat them as after a person's approval
  ([Approving without a person](docs/devbot-yaml.md#approving-without-a-person)).
- `devbot_held_approvals_total{repo, by}` counts approvals of held changes,
  `by` a `person` or devbot itself (`auto`).
- Approving a held change from Gitea's label picker, which removes
  `devbot/needs-approval` and adds `devbot/approve` in one edit, counts as
  the approval: devbot finds the hold still waiting in its own comments,
  puts `devbot/needs-approval` back and takes the label as such, with the
  usual checks. A `devbot/approve` on a task where it approves nothing is
  answered with a comment saying why and taken off
  ([Approving a held change](docs/forge.md#approving-a-held-change)).

- Putting `devbot/ready` back on a task whose PR is open queues a fix
  round whose reason, and its "Working on" comment on the issue, say what
  devbot knows of the PR as it queues the round: a manual retry, the PR
  conflicting with or behind its base (with the rebase onto `origin/<base>`
  spelled out), failing checks, or a trusted review's request for changes,
  quoted with its line comments, the reviewer bot's included. "Working on
  the changes requested on PR #N" is posted only for rounds a review, CI or
  an edit to the task asked for; a rebase says "Rebasing PR #N onto
  `<base>`".
- A fix round's context always says what came of reading the PR's feedback:
  the reviews and comments, or a line that nobody but devbot's own bots has
  reviewed or commented. A read that fails stops the attempt, and its
  failure comment names the read.
- The implementer's prompt says devbot holds the forge credentials and the
  agent has none: the forge reaches it through `.devbot/context.md` and
  `devbot coord`, `origin/<base>` is already fetched, and a failed `gh`,
  `curl` or git call of its own to the forge isn't a reason for `cannot`.

- Per-task models: `file_task`, `file_project` (for its child tasks and its
  plan) and `edit_task` take `model` and `review_model`, through the MCP, the
  HTTP API and the CLI (`-model`, `-review-model`), and a `/model <name>`,
  `/model review <name>` or `/model none` comment from someone who can
  direct devbot sets or clears them on the forge. Each role runs on the
  task's model, else the repo's `agents.<role>.model`, else the
  deployment's, for every run of the task; `repos.limits.models` applies to
  a task's pick as to a repo's, with a comment naming the model refused and
  the one used. `task_status` shows each role's model and where it came
  from, the "Working on it" comment names the implementer's, and the audit
  log records each change as `model.set`
  ([Choosing the model](docs/forge.md#choosing-the-model)).

- The foundation of a Slack app, off unless `slack.enabled`: an install
  into each workspace through Slack's OAuth flow (`/slack/install`), only
  from the workspaces or Enterprise Grid orgs in `slack.allowed_teams`
  (one taken off the list later is cut off), with
  each workspace's bot token sealed with the auth key in Redis; the Events
  API and interactivity endpoints, which check Slack's signature and a
  5-minute timestamp window and handle each delivery once across replicas;
  and dropping a workspace's token on `app_uninstalled` or
  `tokens_revoked`. It needs `auth.key_env` and
  `auth.resource_url` in either auth mode, and in `auth.mode: none` refuses
  to start without `slack.allow_anonymous`. The chart mounts
  `secrets.slack` into serve, and serve exports `devbot_slack_*` metrics and
  audits `slack.install` and `slack.uninstall`
  ([Slack](docs/operating.md#slack)).
- Slack users link their Slack account to their forge account, in
  `auth.mode: oauth`: anything an unlinked user does with the app only gets
  them an ephemeral **Connect your forge account** button (`GET /oauth/link`,
  a single-use 10-minute state bound to that Slack user). It has them prove
  they are that Slack user with Sign in with Slack (OpenID Connect) in the
  same browser, refusing and auditing (`auth.link`) anyone else, and only
  then signs them in through the forge, to a consent page naming them and
  their workspace. Linked, what they do from Slack is done as that forge user,
  through the same calls as their MCP client's. A link ends with
  `/devbot disconnect`, the forge refusing their token, Slack deactivating
  the user, or the workspace uninstalling the app. In `auth.mode: none`
  with `slack.allow_anonymous`, each Slack user is `anonymous_principal`
  labelled `slack-<workspace>-<user>`. The app gets the `/devbot` slash
  command (`POST /slack/commands`), the `user_change` event, the
  `/oauth/link/callback` redirect URL and the `openid` user scope: add them
  to an app created from the earlier manifest
  ([Linking](docs/connectors.md#linking)).
- Filing and following tasks from Slack, as the Slack user's devbot
  caller, through the same calls as their MCP client's: `/devbot file` and
  `/devbot project` open a form for `file_task` or `file_project`, whose
  repo pickers offer only the repos the user may direct devbot on
  (found when the form opens, and answered within 1.5 seconds); a
  refusal shows the form again under the MCP's wording, and a filed task is
  announced in the channel, whose message devbot records as the task's
  thread (`devbot:slack:threads`; never from a task's `origin`, which is
  set to `slack:<workspace>/<channel>`). `/devbot status` and
  `/devbot list` answer only the caller, filtered as over MCP, and
  `/devbot help` lists the commands. The repo pickers need the app's
  Options Load URL (`message_menu_options_url`) set to `/slack/interactivity`,
  and devbot tells a user what it can't post in a channel in their messages
  with the app, which needs its Messages tab: add both to an app created
  from the earlier manifest ([Commands](docs/connectors.md#commands)).
- A task filed from Slack is followed in its thread, and only there (never
  by its `origin`): one status message, edited in place as the task starts,
  asks, opens its PR, gets changes requested, waits for approval, is
  approved, merged, stopped, cancelled or retried, and a reply for its
  questions, approvals it waits for, and how it ended. The leader posts
  them from the event feed, in order and once, retrying Slack's rate limits
  and failures; a post Slack refuses for good, or one to a workspace that
  uninstalled the app, is dropped and counted in
  `devbot_slack_posts_dropped_total`, and a cursor that fell out of the feed
  sets each thread's status from its task instead of replaying
  ([Updates in the thread](docs/connectors.md#updates-in-the-thread)).
- Buttons on a task's status message in its thread, for its state: **Answer**
  (a modal with the question, then `answer`), **Approve…** (a modal with
  the held change and its commit, then `approve_task` with that commit;
  only in `auth.mode: oauth`), **Retry**, **Cancel** (after a confirm) and
  **Rebase**. Each acts as the Slack user who clicks it, never as the
  filer, through the same call as their MCP client's, and tells only them
  what came of it. A click the task has moved on from, or an approval of a
  commit devbot no longer holds, does nothing and says why
  ([Buttons](docs/connectors.md#buttons)).
- A **File with devbot** message shortcut: on any message, it opens the
  `/devbot file` form filled in from the message's thread, with the clicked
  message's first line as the title and the thread in the body, each
  message with its author's display name and time, in a fenced block
  marked untrusted; files and attachments are listed by their links, never
  fetched. Nothing is filed until the clicker submits the form, as
  themselves, and the task's thread is the one it was filed from. devbot
  reads the thread as the workspace's bot, so the app asks for the
  `channels:history`, `groups:history`, `im:history` and `mpim:history`
  scopes; where the bot isn't in the channel, the clicker is told how to
  add it. Add the shortcut (callback ID `devbot_file_message`) and the
  scopes to an app created from the earlier manifest, and install it in
  each workspace again ([Filing from a thread](docs/connectors.md#filing-from-a-thread)).
- `@devbot <free text>` in a channel or a thread, or a message to the app,
  drafts a task: an `intake` job, run in a job pod of its own through the
  model proxy, on `slack.intake.model` or else the implementer's model, with
  no forge credential and no repo, reads the request, its thread and the
  repos the requester may direct devbot on, and answers with a draft or
  clarifying questions. serve makes no model call: it checks the answer
  against a schema, drops one that names a repo outside the requester's,
  and shows a draft to the requester alone with **File**, **Edit** (the
  file form, filled in) and **Discard**; nothing is filed until they click
  **File**, as them, and anyone else's click is refused. The draft's
  acceptance criteria and context are shown whole, across as many sections
  as they take; one too long for a Slack message gets no **File**, only
  **Edit** and **Discard**, and says why. Questions are
  posted in the thread, escaped, and the requester's next `@devbot` there
  drafts again with the conversation so far. Each user and workspace gets
  `slack.intake.per_user_per_hour` (10) and
  `slack.intake.per_workspace_per_hour` (100) requests an hour. The chart
  renders the intake's ScaledJob (`<release>-intake`, on
  `devbot:q:intake`), ServiceAccount, NetworkPolicy and Redis user
  (`devbot-intake`, its password `intake-password`) only with
  `config.slack.enabled`. The app asks for the `app_mentions:read` scope
  and the `app_mention` and `message.im` events: add them to an app
  created from the earlier manifest, and install it in each workspace
  again; a Redis of your own needs the `devbot-intake` user
  ([Drafting from a request](docs/connectors.md#drafting-from-a-request)).
- Everything devbot posts in Slack that it didn't write itself is escaped
  as plain text, bare `@channel`, `@here` and `@everyone` included, and
  posted with link unfurling off.
- `/devbot edit <task>` opens a form filled in with the task's title,
  acceptance criteria and context, read as the caller, and saving it calls
  `edit_task` with the fields changed; `/devbot wait <task> <other task>
  [until tagged [pattern]]` calls `wait_task`, and `/devbot retarget <task>
  <branch>` `retarget`. Each acts as the caller, refused as the MCP refuses
  them, and answers only them
  ([Commands](docs/connectors.md#commands)).
- `/devbot channel repo <repo>` gives a channel a default repo, which the
  file and project forms and **File with devbot** start from, and which
  `@devbot` drafts there are pointed at when the requester may direct
  devbot on it. Setting or clearing it (`/devbot channel repo none`) takes
  the right to direct devbot on that repo; it permits nobody anything. It
  is kept under `devbot:slack:channel-repo:*` until the workspace
  uninstalls the app
  ([A channel's default repo](docs/connectors.md#a-channels-default-repo)).
- The agent images pin Claude Code 2.1.319, the Codex CLI 0.167.0 and
  mise v2026.11.5. Both CLIs run through the model proxy.
- The weekly image-versions workflow files its bump task with
  `devbot/automerge` as well as `devbot/ready`, so the bump PR merges on
  its own once the merge gate passes. The workflow fails if the repo has no
  `devbot/automerge` label.
- `list_tasks`, `task_status` and the other calls that read or act on a
  task report only tasks in repos the install works in (`repos.allow`,
  `repos.deny`, `repos.opt_in`, a repo opting out): on a forge shared with
  another install, that install's tasks are left out of lists and "not
  found" by ref, as a repo the caller can't read is
  ([Tools](docs/connectors.md#tools)).
- A fix round that leaves the PR's head unchanged answers only its own
  request. Requested changes submitted on that head afterwards, after a
  rebase or retarget that had nothing to do or after an earlier review
  round, get a fix round; the same request on the same head stops the task
  at `needs-human`.
- Slack's repo pickers and the intake's repo list find every repo a
  signed-in user may direct devbot on, listed with their own sign-in (on
  Gitea, their own, collaborator and team repos, or every repo for a site
  admin; on GitHub, the sign-in App's installations), then checked as a
  filing is. On Gitea, the bots' own list (for `auth.mode: none` and the
  branch sweep) includes every repo when the implementer is a site admin
  ([Filing from Slack](docs/connectors.md#filing-from-slack)).
- Every message devbot sends to one Slack user outside a request's own
  response (the "You're connected" confirmation after linking, the connect
  button after a click from someone not linked) reaches them in their
  messages with the app when devbot isn't in the channel they asked from
  ([Linking](docs/connectors.md#linking)).
- Every Slack slash command, form and repo picker search is acknowledged
  1.5 seconds after serve received it, which leaves Slack's other 1.5
  seconds to the network between it and serve. Work not done by then
  answers through the command's `response_url`, or an ephemeral message
  ([Slack](docs/operating.md#slack)).
- An `@devbot` request's draft shows where the request was made: in the
  channel for a mention at the channel's top level, in the thread for one
  in a thread. The "drafting" notice says the draft will appear there, and
  everything else shown only to the requester goes there too. **File** or
  **Discard** removes the draft and says what came of it once, there; a
  refused **File** says why there and keeps the draft. Questions are
  posted in the thread
  ([Drafting from a request](docs/connectors.md#drafting-from-a-request)).
- `edit_task`, `retarget`, `wait_task` and `approve_task` (over MCP, the
  HTTP API or Slack) on a task devbot is evaluating wait up to 5 seconds,
  never past the caller's own deadline, then are refused as busy: `409`
  with `Retry-After: 1` and `busy: <ref> is being evaluated; try again in
  a moment`, the same text from an MCP tool (whose `/mcp` response carries
  the `Retry-After`) and from a Slack command, logged at info
  ([Errors](docs/connectors.md#errors)).
- In a Slack task's thread, a question, a change held for approval or a
  stop that needs a person shows once in full, in the reply: the status
  message says only the state and points at the reply. Only the status
  message carries buttons, so once the task moves on no message in the
  thread still offers **Answer**, **Approve…** or **Retry**
  ([Updates in the thread](docs/connectors.md#updates-in-the-thread)).
- The operating guide says that installing the Slack app in any workspace
  but the one it was created in needs the app's **Manage Distribution →
  Activate Public Distribution** turned on (not a Marketplace listing;
  `slack.allowed_teams` decides who may install)
  ([Creating the Slack app](docs/operating.md#creating-the-slack-app)).
- Model fallback: `agents.<role>.fallback_models` in the deployment config,
  and the same key in a repo's `.devbot.yaml`, which replaces the
  deployment's list (`[]` turns it off). When a role's model hits a usage
  limit, stays overloaded through two retries on it, or isn't available to
  the credential or Bedrock region, the run moves to the next model,
  carrying over the work so far, and other runs pass that model over until
  it is back. The fleet-wide hold and `backoff.max_retries` apply only once
  every model in the list is out. The first move for each role of a task is
  said on its issue (`model-failover`), `task_status` shows each role's
  fallbacks and the model its last run was on, and
  `devbot_agent_fallbacks_total` counts the moves
  ([Model fallback](docs/operating.md#model-fallback)).
- The claude and codex harnesses classify an overloaded or out-of-capacity
  provider (`overloaded`: 529, 503, "overloaded", "at capacity", Codex's
  "Selected model is at capacity") and a model
  the credential can't use (`unavailable`: an unknown model, or one not
  enabled for the account or Bedrock region), as new `outcome`s of
  `devbot_agent_runs_total`.
- Model runs carry `devbot.model` in the CLI's `OTEL_RESOURCE_ATTRIBUTES`,
  with `devbot.fallback_from` and `devbot.fallback_reason` on a fallback.
- With your own Redis (`redis.enabled: false`), the `devbot-harness` user
  needs the key pattern `~devbot:model:*`: job pods record there which models
  can't serve runs, and what each task's roles last ran on. Without it they
  log a warning and fallbacks don't skip a model another job found out
  ([Redis](docs/operating.md#redis)). The chart's own Redis has it.
- An overload backs off as a usage limit does, with or without fallbacks,
  rather than failing the attempt. On Bedrock, "You don't have access to the
  model" is `unavailable`, not `auth`.

### Security

- The agent never holds a forge or model credential: a per-pod model proxy
  holds it and forwards only the API routes each agent CLI uses.
- Job pods run in user namespaces with no service-account token, behind
  default-deny egress and no ingress; the bundled Redis admits only this
  release's pods and KEDA, with an ACL user per role.
- Every binary the image build downloads is checked against its published
  SHA-256, and the base images are pinned by digest.

[0.1.0]: https://github.com/robertlestak/devbot/releases/tag/v0.1.0
