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
  published binary; it does not build the local checkout.
- **`nexus` cannot connect to the updater socket:** check
  `sudo systemctl status nexus-updater.socket nexus-updater.service` and
  `sudo journalctl -u nexus-updater.service -n 100 --no-pager`. Immediately
  after bootstrap, use `sudo nexus ...`. Sign out and back in before trying
  unprivileged commands.

## Installation stops before completion

- **Invalid installer answers:** the domain must be a hostname with a dot;
  administrator and alliance IDs must be numeric; the administrator password
  must be at least 12 characters. Remote database answers cannot be blank.
  Core profiles also require a separate Politics & War mutation key.
  A database-only host needs two different private IPv4 addresses, and its
  listen address must belong to that host. A Subs-only host needs an HTTPS
  Core API URL and both tokens.
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
components. Core also uses `nexus-core-php-fpm.service` and
`nexus-scheduler.timer`; check them when web or scheduled work is failing.
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

`sudo nexus install` can adopt a recognized standard legacy deployment. It
shows the detected release and components and preserves the old checkout.
If assessment fails or the installation is modified or unknown, stop and
inspect that deployment before changing it. `install_nexus.sh` is retired;
use the guided command instead.
