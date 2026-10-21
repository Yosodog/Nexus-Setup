# Nexus Setup

Nexus Setup provides one command for installing and maintaining self-hosted
Nexus AMS on supported systemd hosts.

## Guides

- [Install Nexus step by step](docs/getting-started.md) — prepare a server,
  run the installer, and verify the result.
- [Choose an installation profile](docs/installation-profiles.md) — all five
  layouts, including remote database and Subs-only hosts.
- [Operate Nexus](docs/operations.md) — updates, rollback, components, and
  cleanup.
- [Troubleshoot](docs/troubleshooting.md) — installation failures, service
  health, and interrupted operations.

## Supported hosts

- Ubuntu 22.04, 24.04, or 26.04 LTS on amd64
- Debian 12 or 13 on amd64

Other platforms are diagnosed but are not modified by the managed installer.
These are release targets, not a substitute for qualification: run the clean
install, update, rollback, cleanup, and component smoke matrix on all five OS
images before publishing a stable release. In particular, the NodeSource
`nodistro` feed must be exercised on Debian 13 and Ubuntu 26.04 rather than
assumed to work from earlier distribution results.

Before installing, point the domain at this server, allow inbound ports 80 and
443, and make sure the host can reach GitHub Releases, Ubuntu/Debian package
repositories, Let's Encrypt, and Politics & War. A remote-database profile also
needs a working database account and network route. Installation requests a TLS
certificate after Core starts, so DNS and port 80 must already work.

## Install

Follow the [step-by-step installation guide](docs/getting-started.md) for
prerequisites, every prompt, and verification. The short path is:

```bash
curl -fsSL https://raw.githubusercontent.com/Yosodog/Nexus-Setup/main/bootstrap.sh | sudo bash
sudo nexus install
```

To follow beta releases, pass `--channel beta` to both:

```bash
curl -fsSL https://raw.githubusercontent.com/Yosodog/Nexus-Setup/main/bootstrap.sh | sudo bash -s -- --channel beta
sudo nexus install --channel beta
```

The default profile installs Core and Subs on one server with a local database.
See [installation profiles](docs/installation-profiles.md) for other layouts.
The installer uses published release artifacts instead of Git checkouts and
does not regenerate configuration during updates.

## Operate Nexus

See the [operations guide](docs/operations.md) for the safe sequence for each
command and the [troubleshooting guide](docs/troubleshooting.md) if one fails.

```text
nexus status
nexus doctor
nexus update --check
nexus update
nexus rollback [core|subs|discord]
nexus cleanup
nexus channel [stable|beta]

nexus component list
nexus component install subs|discord
nexus component enable subs|discord
nexus component disable subs|discord
nexus component restart core|subs|discord
nexus operation show <uuid>
```

Read commands support `--json`. Mutations prompt before changing the host and
support `--yes` to skip the final confirmation; `nexus install` still prompts
for its required configuration. Immediately after bootstrap, use
`sudo nexus ...`; after signing out and back in, the operator account selected
by bootstrap can use the command without `sudo`.

`nexus update` checks each installed component's official GitHub Releases on
the host's channel and moves every component to its newest release whose
requirements the others meet. Components are versioned independently, so an
update usually touches only the components that have new releases. It runs
forward-only database migrations when Core changes, switches versioned release
directories, and health-checks the changed components. If post-activation
health fails, the changed components switch back to their previous releases;
migrations are not reversed. The updater then updates itself if a newer
`nexus` release exists.

The updater does **not** make a database backup. Take a manual database snapshot
before an update if the installation's data matters to you. Release maintainers
must keep each migration compatible with the immediately previous Core release
so code rollback can work. A failed migration or interrupted operation
may require an operator to inspect `nexus status`, `nexus doctor`, and
`nexus operation show <uuid>` locally before retrying. Do not manually delete
operation state to force another update.

`nexus cleanup` keeps the current and previous release of every component and
removes only older managed release directories plus stale download/staging
files. It never removes database data, credentials, uploads, writable storage,
operation history, or preserved legacy checkouts.

The Admin **Settings → Software** page presents the same update, rollback,
cleanup, and local component actions for an administrator with `manage-system`.
The GUI and CLI share the root-owned updater; a web-only or remote component
cannot silently gain lifecycle control on another host. Database-only and
Subs-only hosts are operated with the local CLI.

## Release contract

Each repository is versioned and released on its own:

| Repository | Release assets |
|---|---|
| `Nexus-Setup` | `nexus-linux-amd64`, `nexus-linux-amd64.sha256` |
| `Nexus-AMS` | `nexus-core.tar.gz`, `nexus-release.json` |
| `Nexus-AMS-Subs` | `nexus-subs.tar.gz`, `nexus-release.json` |
| `Nexus-AMS-Discord` | `nexus-discord.tar.gz`, `nexus-release.json` |

Tags are `vMAJOR.MINOR.PATCH`, optionally with a prerelease suffix such as
`v1.4.0-beta.1`. Pushing a tag runs the repository's release workflow, which
tests, builds, and publishes the release; tags with a suffix are published as
GitHub prereleases and are visible only to hosts on the beta channel. A hotfix
is a tag in the one repository that needs it.

Every component release publishes `nexus-release.json`, which is also embedded
in the artifact:

```json
{
  "schema_version": 2,
  "component": "nexus-subs",
  "release": "v0.4.0",
  "requires": { "nexus-core": "v1.5.0" },
  "artifact_sha256": "…"
}
```

`requires` lists the minimum version of each other component (`nexus-core`,
`nexus-subs`, `nexus-discord`) or of the updater (`nexus-updater`) that the
release needs. Declare a requirement whenever a release depends on a newer API
or behavior in another piece; the updater holds back a release until its
requirements can be met, updates the required component in the same run when
it can, and refuses a rollback that would break a requirement. Requirements on
components that run on a different host (for example Core, from a Subs-only
host) are not checked. Each component repository keeps its requirements in
`nexus-release.requires.json`.

Releases are installed directly at the newest compatible version, so
intermediate releases do not need to stay published, but keep each Core
migration safe to run after any earlier supported release. A failed test suite
blocks publication. Verify a clean installation, update from the previous
release, rollback, and cleanup on every supported OS image before publishing a
stable release.

The updater trusts official GitHub Releases. It verifies every download
against the SHA-256 digest published with the release, which catches corrupt or
substituted transfers, but it does not use TUF, release signatures, provenance
enforcement, or automatic database backups.

## Legacy installer

`install_nexus.sh` is a fail-safe compatibility shim and no longer performs
installations. Existing checkout adoption is handled by `nexus install`; it
must preserve modified or unknown legacy deployments rather than overwrite
them. Adoption covers Core (`/var/www/nexus`), Subs (`/var/nexus-subs`), and
the Discord bot (`/opt/nexus-ams-discord`). It stops every Supervisor program
that runs from those checkouts and the legacy `nexus-discord.service`, keeps the
bot's relay identity and a Subs Redis Stream delivery, and restores all of it
if adoption fails. See
[Legacy checkout detected](docs/troubleshooting.md#legacy-checkout-detected).

## Core services

A Core host runs one systemd unit per long-running process:
`nexus-core.service` (`default` queue), `nexus-core-sync.service` (`sync`
queue), `nexus-core-pulse-check.service`, `nexus-core-php-fpm.service`, and
`nexus-scheduler.timer`. `nexus-core-pulse-work.service` runs when Core's `.env`
sets `PULSE_INGEST_DRIVER=redis`, and `nexus-core-subs-stream.service` runs when
Subs delivers through a Redis Stream. Bootstrap installs the unit files;
rerunning it on a managed host installs units added by a newer updater and
enables the ones the host needs.
