# Troubleshoot Nexus Setup

Start with the command's error and run these **on the affected host**:

```bash
sudo nexus status
sudo nexus doctor
sudo nexus component list
```

`doctor` checks local installation state, update disk space, and managed
directories. It does not test DNS, remote database access, or every service;
use the checks below for those failures. If a command printed an operation
UUID, inspect it with `sudo nexus operation show <uuid>`.

## Bootstrap fails

- **Unsupported platform:** check `/etc/os-release`, `dpkg --print-architecture`,
  and that the host booted with systemd. Only the OS and architecture in the
  [installation guide](getting-started.md) are supported.
- **Download or package error:** check outbound DNS and HTTPS access to GitHub
  Releases and the package repositories. Bootstrap downloads the latest
  published binary; it does not build a local checkout.
- **No release found for the stable channel:** before the first stable
  release, only beta releases exist. Rerun bootstrap with `--channel beta`.
- **Checksum mismatch:** the download was corrupted or altered. Retry; if it
  keeps failing, do not install that binary and report it.
- **`nexus` cannot connect to the updater socket:** check
  `sudo systemctl status nexus-updater.socket nexus-updater.service` and
  `sudo journalctl -u nexus-updater.service -n 100 --no-pager`. Immediately
  after bootstrap, use `sudo nexus ...`. Sign out and back in before trying
  unprivileged commands.

## Installation stops before completion

- **An answer is rejected:** each prompt explains what it expects and asks
  again, up to five times. The domain must be a hostname with a dot and no
  `https://`; nation and alliance IDs are the numbers from their Politics & War
  URLs; the administrator password needs at least 12 characters. A
  database-only host needs two different private IPv4 addresses, and a
  Subs-only host needs an HTTPS Core API URL.
- **Domain does not resolve:** create the DNS A record for the domain, wait
  until `getent hosts your.domain` returns this server's address, and run the
  installer again. Nothing was changed.
- **Domain warning:** the domain resolves, but this server could not reach
  itself through it on port 80. Check that the record points at this server and
  that the firewall or cloud security group allows inbound port 80. Some
  networks cannot reach their own public address; if you are sure DNS and the
  firewall are right, you can continue.
- **Politics & War rejected the API key:** copy the key again from the
  Politics & War Account page. Nothing was changed.
- **Alliance or nation warning:** the API key belongs to a nation outside the
  alliance you entered, or to a different nation than the administrator ID.
  Nexus reads alliance data with this key, so use a key from an alliance
  member and double-check the IDs.
- **Package or release download fails:** verify outbound network access and
  available space, then inspect the command error. `sudo nexus doctor` reports
  free space for later updates; a fresh installation also checks space.
- **Remote database connection or migration fails:** confirm the database
  host and port 3306 are reachable from the application host, the account is
  allowed from that host, and it can modify the selected database. For a
  Setup-managed database host, compare the application answers with its
  root-only `/etc/nexus/credentials/database-only.env` file.
- **TLS certificate request fails:** confirm the domain resolves to the Core
  server and inbound ports 80 and 443 are open. The installer requests a
  certificate after Core starts, so DNS must already be correct. Inspect
  `sudo journalctl -u nginx.service -n 100 --no-pager` for web-server errors.

The installer may have changed packages and written configuration before a
failure, even if it did not record a completed installation. Fix the reported
cause before retrying. If files or services from the failed attempt remain,
inspect them rather than deleting installation state or credentials blindly.

## A component is unhealthy

Run `sudo nexus component list` to see which local component is affected.
Then inspect its service:

```bash
sudo systemctl status nexus-core.service
sudo journalctl -u nexus-core.service -n 100 --no-pager
```

Replace `nexus-core` with `nexus-subs` or `nexus-discord` for those
components. Core runs several units, and `component list` reports Core as
unhealthy if any of them is down:

| Unit | Runs |
|---|---|
| `nexus-core.service` | the `default` queue |
| `nexus-core-sync.service` | the `sync` queue (nation, Discord role, and beige-alert syncs) |
| `nexus-core-pulse-check.service` | `pulse:check` |
| `nexus-core-pulse-work.service` | `pulse:work`, only when Core's `.env` sets `PULSE_INGEST_DRIVER=redis` |
| `nexus-core-subs-stream.service` | `subs:consume-stream`, only when Subs delivers through a Redis Stream |
| `nexus-core-php-fpm.service` | the web application |
| `nexus-scheduler.timer` | scheduled work, every minute |

If an update, rollback, or restart stops with "`<unit>` is not installed", the
updater updated itself on a host whose service units are older than it. Run
the bootstrap command from the [installation guide](getting-started.md) again;
it installs the current units, enables the ones the host needs, and restarts
Core's workers. Then retry the command.
For a split installation, run these checks on the host where the component
actually runs. `sudo nexus component restart <core|subs|discord>` is available
for an installed local component after you fix its underlying error.

## An update or other operation is interrupted

If the CLI stopped waiting, first run `sudo nexus operation show <uuid>`; the
worker may still be running. For a failure, `rolled_back`, or
`recovery_required` status, inspect `sudo nexus status`,
`sudo nexus doctor`, and the affected component's journal. A health-check
failure may have restored the old code, but migrations are not undone. Restore
data from your own backup if necessary. Do not delete operation state or start
another mutation to force a retry; determine which release and services are
active first. [Update and rollback behavior](operations.md#update-rollback-and-cleanup)
explains the limits.

The worker's journal can show where a specific operation stopped. Replace
`UUID` with the ID printed by the command:

```bash
sudo journalctl -u "nexus-updater-worker@UUID.service" -n 100 --no-pager
```

## Legacy checkout detected

`sudo nexus install` can adopt a recognized standard legacy deployment: Core at
`/var/www/nexus`, Subs at `/var/nexus-subs`, and the Discord bot at
`/opt/nexus-ams-discord`. It shows each detected component and its release and
preserves the old checkouts. Each checkout must be a clean clone of its official
`Yosodog` repository, including no untracked files, on a published release tag
of its own; the components do not need to share a tag. Core and the Discord bot
also need a `.env`. A Discord bot can only be adopted together with Core.

Adoption takes over everything the old deployment ran:

- **Supervisor:** every program in `/etc/supervisor/conf.d/*.conf` whose
  `directory=` is one of the checkouts, or whose `command=` runs a file inside
  one, is stopped. Its configuration file is renamed with a `.nexus-disabled`
  suffix once the managed services are healthy, which also disables any other
  program in the same file.
- **Discord:** the legacy `nexus-discord.service` is stopped and disabled, and
  its unit file is renamed to `nexus-discord.service.nexus-disabled` before the
  managed unit replaces it. The managed bot keeps the legacy bot's token,
  application, guild, and relay identity, read from its `.env`; Core's
  `DISCORD_*` settings are not changed.
- **Subs delivery:** if the legacy Subs `.env` sets
  `DELIVERY_DRIVER=redis-stream`, the managed Subs keeps delivering through that
  Redis Stream and `nexus-core-subs-stream.service` consumes it. Core's `.env`
  must have the same `SUBS_REDIS_HMAC_SECRET` and `SUBS_REDIS_STREAM` as Subs.
- **Cron:** the `schedule:run` line for the legacy checkout is removed from
  `/etc/crontab`.

If any step fails, adoption stops the managed services and restores the
Supervisor files, the legacy Discord unit, Nginx, and the crontab, then starts
the legacy programs again. If it reports that a `.nexus-disabled` backup
already exists, an earlier attempt left it behind: compare it with the active
file and remove the one you do not need before retrying.

If assessment fails or the installation is modified or unknown, stop and
inspect that deployment before changing it. `install_nexus.sh` is retired;
use the guided command instead.
