# devbot - working in this repo

- Go 1.26, standard library first. Without a local Go toolchain, prefix commands with `hack/go` (runs them in a container).
- `internal/forge` is the only place forge APIs appear. Core code uses the
  `forge.Provider` interface and the lowest-common-denominator types; never add
  a method only one forge can implement.
- `internal/core` is level-triggered. Every entry point must be safe to run
  twice, in any order, after a Redis wipe. Add a test in `engine_test.go` for
  any new transition, including the "nothing to do" second run.
- The harness never runs git inside the shared workspace after handing it to
  the agent, never follows symlinks out of it, and never puts a forge
  credential anywhere the agent container can read. `harness_test.go` has
  tests for each; keep them passing.
- Comments explain why, not what - match the surrounding density.
- Before pushing: `hack/go gofmt -l .` prints nothing, `hack/go go vet ./...`
  and `hack/go go test ./...` pass, `helm lint deploy/helm/devbot` (and with each `values.example-*.yaml`).
- Docs live in `docs/`. A new or changed config key goes in `docs/devbot-yaml.md`
  and `docs/devbot.schema.json` (repo keys) or `docs/operating.md` (deployment
  keys) in the same change: `internal/config/docs_test.go` fails otherwise.
  Diagrams are mermaid, so check they still parse when you edit them.
- devbot is before 1.0, so breaking changes are allowed. Every user-visible
  change gets an entry under `## [Unreleased]` in `CHANGELOG.md` in the same
  change; a breaking one goes under `### Breaking` there and says what an
  operator or repo has to change. The code still carries no shims,
  deprecation paths or removed-key warnings: when something is removed or
  renamed, delete it outright, in code, config, chart values, tests and docs.
  Docs describe only what devbot does now, with no "used to" or "is gone"
  sections.
