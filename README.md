# Nexus Setup

Nexus Setup installs and maintains a self-hosted [Nexus AMS](https://github.com/Yosodog/Nexus-AMS)
deployment with one command, `nexus`. It installs published releases, manages
the systemd services, and handles updates, rollback, and cleanup.

## Guides

- [Install Nexus step by step](docs/getting-started.md) — prepare a server,
  run the installer, and verify the result.
- [Choose an installation profile](docs/installation-profiles.md) — one
  server, a remote database, or split database and Subs hosts.
- [Operate Nexus](docs/operations.md) — updates, rollback, release channels,
  optional components, and cleanup.
- [Troubleshoot](docs/troubleshooting.md) — installation failures, service
  health, and interrupted operations.
- [Release contract](docs/releasing.md) — for maintainers publishing Nexus
  releases.

## Before you install

You need:

- An **amd64** server running Ubuntu 22.04, 24.04, or 26.04 LTS, or Debian 12
  or 13, booted with systemd and dedicated to Nexus.
- A domain pointed at the server, with inbound ports **80** and **443** open.
  The installer requests a TLS certificate, so DNS must already work.
- Outbound access to GitHub Releases, the Ubuntu/Debian package repositories,
  Let's Encrypt, and Politics & War.
- The administrator's email, a password of at least 12 characters, the
  administrator's nation ID, the alliance ID, and a Politics & War API key and
  mutation key.

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/Yosodog/Nexus-Setup/main/bootstrap.sh | sudo bash
sudo nexus install
```

The default profile installs Core and Subs on one server with a local
database. The [installation guide](docs/getting-started.md) walks through every
prompt; see [installation profiles](docs/installation-profiles.md) for other
layouts.

## Commands

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

Read commands support `--json`. Changing commands ask for confirmation and
accept `--yes` to skip it. Use `sudo nexus ...` right after bootstrap; after
signing out and back in, the operator account selected by bootstrap can run
the operating commands without `sudo`.

Administrators with the `manage-system` permission can run the same update,
rollback, cleanup, and local component actions from **Admin → Settings →
Software** in Nexus. Remote, database-only, and Subs-only hosts are managed
with their own CLI.

## What updates do and do not do

- Core, Subs, Discord, and the `nexus` updater are versioned independently.
  `nexus update` moves each installed component straight to its newest release
  that is compatible with the others.
- Database migrations run forward-only when Core changes. If health checks fail
  afterward, the code switches back to the previous release, but **migrations
  are not reversed**.
- **The updater does not back up the database.** Take your own snapshot before
  updating.
- `nexus cleanup` keeps the current and previous release of each component. It
  never removes database data, credentials, uploads, writable storage, or
  operation history.

See the [operations guide](docs/operations.md) for details.

## Upgrading a legacy checkout

`sudo nexus install` can adopt a recognized Git-checkout deployment made by
the old installer. It preserves the old checkout. `install_nexus.sh` no
longer installs anything.

## License

GNU GPL v3. See [LICENSE](LICENSE).
