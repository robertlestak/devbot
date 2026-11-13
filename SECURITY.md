# Security policy

## Supported versions

Security fixes go into the latest minor release only. Before 1.0 that means
the newest `0.Y` line: fixes ship as a new `0.Y.Z` patch, or in the next minor.
Upgrade to get them.

| Version | Supported |
|---|---|
| latest minor (`0.Y.x`) | yes |
| anything older | no |

## Reporting a vulnerability

Please report vulnerabilities privately, never in a public issue, pull
request or discussion. Either:

- use GitHub's [private vulnerability reporting](https://github.com/robertlestak/devbot/security/advisories/new)
  on `github.com/robertlestak/devbot`, or
- email rob@lestak.sh.

Include what you found, how to reproduce it, the version or commit, and the
impact you see: which identity, credential or data it reaches. devbot's
security model, with what it is meant to keep from the agent and from the
people who can file tasks, is in
[docs/reference-architecture.md](docs/reference-architecture.md#security-and-threat-model); a report
showing a way around it is what we most want to hear.

You'll get an acknowledgement within a few working days. We'll keep you
informed while we fix it, agree a disclosure date with you, and credit you
in the advisory and the [changelog](CHANGELOG.md) unless you'd rather not
be named.
