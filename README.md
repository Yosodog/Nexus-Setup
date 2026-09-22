# Nexus Setup

Nexus Setup provides one command for installing and maintaining self-hosted
Nexus AMS on supported systemd hosts.

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

```bash
git clone https://github.com/Yosodog/Nexus-Setup.git
cd Nexus-Setup
sudo ./bootstrap.sh
sudo nexus install
```

The guided installer defaults to putting everything on one server. It asks
only for the domain, administrator account, alliance ID, and Politics & War
API key. Database and internal credentials, paths, service users, systemd
units, queues, component URLs, and TLS email are handled automatically.

Advanced profiles support an existing remote database, web-only, database-only,
and Subs-only hosts. The installer uses published release artifacts instead of
Git checkouts and never regenerates configuration during an update.

For a database-only host, the installer additionally asks for that server's
private IPv4 address and the application server's private IPv4 address. It
binds MySQL/MariaDB to the selected local address, grants the generated
`nexus_app` account only to the named application address, and writes the
connection details to a root-only file under `/etc/nexus/credentials/`. Copy
those details securely to the application host. Permit port 3306 only from
that application address in your host/network firewall; do not publish a
database-only host directly to the internet. This advanced split profile does
not include a GUI on the database host.

For the default profile, the prompts are the domain, administrator email and
password, administrator nation ID, alliance ID, and Politics & War API key.
Discord is optional: create the Discord application yourself, then use
`nexus component install discord` or the Admin Software page to supply its bot
token, application ID, and guild ID. Nexus registers the guild slash commands
after a successful managed install or update; you must still invite the bot in
the Discord Developer Portal.

## Operate Nexus

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
