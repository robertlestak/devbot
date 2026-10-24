## What and why

<!-- What this changes and why. Link the issue it closes: "Closes #123". -->

## Checklist

- [ ] `gofmt -l .` prints nothing, and `go vet ./...` and `go test ./...` pass (`hack/go` runs them without a local Go)
- [ ] `helm lint deploy/helm/devbot`, and with each `values.example-*.yaml`, passes (if the chart changed)
- [ ] Tests cover the change in behaviour
- [ ] A new or changed config key is documented in `docs/devbot-yaml.md` and `docs/devbot.schema.json`, or `docs/operating.md`
- [ ] A user-visible change has a `CHANGELOG.md` entry under `## [Unreleased]`; a breaking one is under `### Breaking`, saying what to change

<!-- CI runs on the Gitea origin, so this PR shows no CI status on GitHub; a maintainer carries it over. See CONTRIBUTING.md. -->
