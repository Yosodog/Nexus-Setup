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
git clone https://github.com/Yosodog/Nexus-Setup.git
cd Nexus-Setup
sudo ./bootstrap.sh
sudo nexus install
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
nexus rollback
nexus cleanup

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

`nexus update` discovers stable releases from the official
`Yosodog/Nexus-Setup` GitHub Releases page, applies every intermediate release,
runs forward-only database migrations, switches versioned release directories,
and health-checks all installed local components. If post-activation health
fails, code switches back to the immediately previous release; migrations are
not reversed.

The updater does **not** make a database backup. Take a manual database snapshot
before an update if the installation's data matters to you. Release maintainers
must keep each stable migration compatible with the immediately previous Core
release so code rollback can work. A failed migration or interrupted operation
may require an operator to inspect `nexus status`, `nexus doctor`, and
`nexus operation show <uuid>` locally before retrying. Do not manually delete
operation state to force another update.

`nexus cleanup` keeps the current and previous release for every component and
removes only older managed release directories plus stale download/staging
files. It never removes database data, credentials, uploads, writable storage,
operation history, or preserved legacy checkouts.

The Admin **Settings → Software** page presents the same update, rollback,
cleanup, and local component actions for an administrator with `manage-system`.
The GUI and CLI share the root-owned updater; a web-only or remote component
cannot silently gain lifecycle control on another host. Database-only and
Subs-only hosts are operated with the local CLI.

## Release contract

All four repositories publish the same stable `vMAJOR.MINOR.PATCH` tag:

| Repository | Fixed asset |
|---|---|
| `Nexus-Setup` | `nexus-linux-amd64` |
| `Nexus-AMS` | `nexus-core.tar.gz` |
| `Nexus-AMS-Subs` | `nexus-subs.tar.gz` |
| `Nexus-AMS-Discord` | `nexus-discord.tar.gz` |

Core, Subs, and Discord publish first. Setup verifies those public releases and
assets before publishing the canonical release. Stable releases must not be
deleted because updates apply them sequentially.

Publish every stable version in sequence, including intermediate patch
releases. Do not publish a new Setup tag until matching Core, Subs, and Discord
assets are public. The Setup release workflow checks their presence. A failed
Core test suite blocks publication; do not bypass it to produce an incomplete
canonical release. Verify a clean installation, update from the prior stable
version, rollback, and cleanup on every supported OS image before calling a
release stable.

The updater intentionally trusts official GitHub Releases. It does not use
TUF, release signatures, checksum files, provenance enforcement, or automatic
database backups.

## Legacy installer

`install_nexus.sh` is a fail-safe compatibility shim and no longer performs
installations. Existing checkout adoption is handled by `nexus install`; it
must preserve modified or unknown legacy deployments rather than overwrite
them.
