# Contributing to devbot

Thanks for helping. Bug reports, fixes, docs and features are all welcome.
For anything bigger than a small fix, open an issue first so we can agree on
the shape before you write it.

## Where the repo lives

devbot's origin is a Gitea instance, where CI runs and releases are cut.
`github.com/robertlestak/devbot` is a mirror of it. Issues and pull requests
are welcome on GitHub: a maintainer carries an accepted PR over to the origin,
keeping your commits and authorship, and it reaches GitHub again through the
mirror. Because CI runs on the origin, a GitHub PR shows no CI status; run the
checks below yourself before you open it.

## Setting up

devbot is Go 1.26 (the exact toolchain is pinned in `go.mod`), standard
library first. Without a local Go toolchain, prefix any command with
`hack/go`, which runs it in a container:

```sh
hack/go go test ./...
```

The chart checks need [Helm](https://helm.sh).

## Checks

Before you push, all of these must be clean:

```sh
hack/go gofmt -l .        # prints nothing
hack/go go vet ./...
hack/go go test ./...
helm lint deploy/helm/devbot
for f in deploy/helm/devbot/values.example-*.yaml; do helm lint deploy/helm/devbot -f "$f"; done
```

CI runs the same. The README's [Development](README.md#development) section
describes what the tests cover, including the opt-in live GitHub test that CI
never runs.

## Conventions

[AGENTS.md](AGENTS.md) holds the rules for working on devbot's code, for
people and coding agents alike. In short:

- `internal/forge` is the only place forge APIs appear. Core code uses the
  `forge.Provider` interface and its lowest-common-denominator types; don't add
  a method only one forge can implement.
- `internal/core` is level-triggered: every entry point must be safe to run
  twice, in any order, after a Redis wipe. A new transition gets a test in
  `engine_test.go`, including the "nothing to do" second run.
- The harness never runs git in the shared workspace after handing it to the
  agent, never follows symlinks out of it, and never puts a forge credential
  where the agent container can read it. `harness_test.go` tests each of these.
- Comments explain why, not what; match the surrounding density.
- Docs live in `docs/` and describe what devbot does now. A new or changed
  config key goes in `docs/devbot-yaml.md` and `docs/devbot.schema.json` (repo
  keys) or `docs/operating.md` (deployment keys) in the same change;
  `internal/config/docs_test.go` fails otherwise. Diagrams are mermaid.

## Compatibility and the changelog

devbot is before 1.0, so breaking changes are allowed. A user-visible change
gets an entry under `## [Unreleased]` in [CHANGELOG.md](CHANGELOG.md), and a
breaking one goes under a `### Breaking` heading there, saying what an
operator or repo has to change. The code itself carries no shims,
deprecation paths or removed-key warnings: when something is removed or
renamed, it goes everywhere at once, in code, config, chart values, tests and
docs.

## Pull requests

- Keep a PR to one change, with tests where behaviour changes.
- Write commit messages as `area: what changed`, with a body that says why.
- Fill in the pull request template.

A maintainer reviews every PR against its issue's acceptance criteria and the
conventions above, and merges it once the checks pass on the origin. devbot
also works on its own repo, so some PRs come from its bots; they go through
the same review and checks.

## Reporting security issues

Don't open a public issue for a vulnerability. See [SECURITY.md](SECURITY.md).

## Code of conduct

Everyone taking part is expected to follow the
[code of conduct](CODE_OF_CONDUCT.md).
