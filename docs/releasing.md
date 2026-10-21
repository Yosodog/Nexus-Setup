# Release contract

This page is for maintainers who publish Nexus releases. Operators do not need
it to install or update Nexus.

## Repositories and assets

Each repository is versioned and released on its own:

| Repository | Release assets |
|---|---|
| `Nexus-Setup` | `nexus-linux-amd64`, `nexus-linux-amd64.sha256` |
| `Nexus-AMS` | `nexus-core.tar.gz`, `nexus-release.json` |
| `Nexus-AMS-Subs` | `nexus-subs.tar.gz`, `nexus-release.json` |
| `Nexus-AMS-Discord` | `nexus-discord.tar.gz`, `nexus-release.json` |

Tags are `vMAJOR.MINOR.PATCH`, optionally with a prerelease suffix such as
`v1.4.0-beta.1`. Pushing a tag runs the repository's release workflow, which
tests, builds, and publishes the release. A failed test suite blocks
publication. Tags with a suffix are published as GitHub prereleases and are
installed only by hosts on the beta channel. A hotfix is a tag in the one
repository that needs it.

## Compatibility requirements

Every component release publishes `nexus-release.json`, which is also embedded
in the artifact:

```json
{
  "schema_version": 2,
  "component": "nexus-subs",
  "release": "v1.0.1",
  "requires": { "nexus-core": "v1.2.0" },
  "artifact_sha256": "…"
}
```

`requires` lists the minimum version of each other component (`nexus-core`,
`nexus-subs`, `nexus-discord`) or of the updater (`nexus-updater`) that the
release needs. Each component repository keeps its requirements in
`nexus-release.requires.json`. Declare a requirement whenever a release
depends on a newer API or behavior in another piece.

The updater holds back a release until its requirements can be met, updates
the required component in the same run when it can, and refuses a rollback
that would break a requirement. Requirements on components that run on a
different host (for example Core, from a Subs-only host) are not checked.

## Migrations

Releases are installed directly at the newest compatible version, so
intermediate releases do not need to stay published. Every Core migration must:

- run safely after any earlier supported release, and
- stay compatible with the immediately previous Core release, so a code
  rollback still works.

## Before publishing a stable release

Run the clean install, update from the previous release, rollback, cleanup,
and component smoke tests on every supported OS image: Ubuntu 22.04, 24.04,
and 26.04, and Debian 12 and 13. Exercise the NodeSource `nodistro` feed on
Debian 13 and Ubuntu 26.04 rather than assuming earlier distribution results
carry over.

## Trust model

The updater trusts official GitHub Releases from fixed repositories. It
verifies every download against the SHA-256 digest published with the release,
which catches corrupt or substituted transfers. It does not use TUF, release
signatures, provenance enforcement, or automatic database backups.

The updater's local socket protocol is documented in
[protocol/v1](../protocol/v1/README.md), and the embedded systemd units are
mirrored in [systemd](../systemd/README.md).
