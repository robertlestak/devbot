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

### Security

- The agent never holds a forge or model credential: a per-pod model proxy
  holds it and forwards only the API routes each agent CLI uses.
- Job pods run in user namespaces with no service-account token, behind
  default-deny egress and no ingress; the bundled Redis admits only this
  release's pods and KEDA, with an ACL user per role.
- Every binary the image build downloads is checked against its published
  SHA-256, and the base images are pinned by digest.

[Unreleased]: https://github.com/robertlestak/devbot/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/robertlestak/devbot/releases/tag/v0.1.0
