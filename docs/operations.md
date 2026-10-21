# Operate Nexus

Run these commands **on the host that owns the component**. Use `sudo` until
you have signed out and back in after bootstrap. Read commands accept
`--json`; changing commands ask for confirmation unless you add `--yes`.

## Check health and updates

```bash
sudo nexus status
sudo nexus doctor
sudo nexus component list
sudo nexus update --check
```

`status` reports the profile, release channel, each component's current and
previous release, component state, and recent operations.
`doctor` checks installation state, disk space, and managed directories.
`component list` shows which local components are installed and healthy.
`update --check` lists the release each component would move to, any newer
release that is held back and why, and any newer `nexus` updater. Database-only
hosts have no application release and cannot use update commands.

Upgrading from a beta installation made before the installer asked for the
mutation key? Add `PW_API_MUTATION_KEY` to the root-controlled
`/etc/nexus/nexus-core/.env` yourself. `nexus update` preserves that file and
does not prompt for new keys.

## Update, rollback, and cleanup

Before an update, take a **manual database backup or snapshot**. Nexus Setup
does not do this for you. Then run:

```bash
sudo nexus update --check
sudo nexus update
sudo nexus status
sudo nexus component list
```

Core, Subs, and Discord are versioned separately. The updater moves each
installed component straight to its newest release that is compatible with the
others, runs forward-only database migrations when Core changes, activates the
new code, and checks the changed components. A release that needs a newer
version of another component is held back until that version is available;
`update --check` shows the reason. If health checks fail after activation, the
changed components switch back to their previous releases. Database migrations
are **not** reversed. If the update finishes unsuccessfully, inspect the
operation before retrying. When a held-back release needs a newer updater, run
`sudo nexus update` again after the updater has updated itself.

`sudo nexus rollback` restores the previous **code** release of every component
the last update changed. `sudo nexus rollback subs` (or `core`, `discord`)
restores only that component. Rollback refuses to leave components on releases
that are incompatible with each other. It does not restore the database. Check
application and data compatibility before using it. To reclaim old managed releases, run
`sudo nexus cleanup`; the command previews what it will remove and keeps the
current and previous release. It leaves database data, credentials, uploads,
writable storage, operation history, and preserved legacy checkouts alone.

## Choose a release channel

```bash
sudo nexus channel
sudo nexus channel beta
sudo nexus channel stable
```

Installations use the **stable** channel by default, which is the right choice
for a live alliance. The **beta** channel also installs prereleases such as
`v1.4.0-beta.1`, which get less testing; use it on a test installation. To
start a new installation on beta, run bootstrap with `-s -- --channel beta`
after `sudo bash` and use `sudo nexus install --channel beta`.

A channel change applies to the next `nexus update`. Switching back to stable
never downgrades anything: components stay on their prereleases until a stable
release is newer.

## Install and control components

```bash
sudo nexus component install subs
sudo nexus component install discord
sudo nexus component disable subs
sudo nexus component enable subs
sudo nexus component restart core
sudo nexus component restart subs
sudo nexus component restart discord
```

A component installed later gets the newest release on the host's channel that
is compatible with the installed components. Install Subs only if it is absent
on a Core host; it reuses Core's local
configuration.

Discord is optional. First create a Discord application and bot and invite it
to your server (steps 2–5 of the
[Discord self-hosting guide](https://github.com/Yosodog/Nexus-AMS-Discord/blob/main/docs/self-hosting.md#2-create-the-discord-application)),
then install it on a Core host. The command asks for its bot token,
application ID, and guild ID. The IDs must be 17–20 digit numbers. Invite
the bot to your guild separately. Managed install and update register its
guild commands; a registration failure makes the operation fail. Core can be
restarted, but it cannot be installed, enabled, or disabled as an optional
component. Use `nexus component list` to see the actions available locally.

`component restart core` restarts every Core unit: the `default` queue
worker (`nexus-core.service`), the `sync` queue worker
(`nexus-core-sync.service`), Pulse's server checks
(`nexus-core-pulse-check.service`), and PHP-FPM. When Core's `.env` sets
`PULSE_INGEST_DRIVER=redis`, `nexus-core-pulse-work.service` also runs; when
Subs delivers through a Redis Stream, `nexus-core-subs-stream.service` runs
`subs:consume-stream`. The scheduler runs from `nexus-scheduler.timer`. Updates
and rollbacks restart and health-check the same units.

New installations deliver Subs events to Core over HTTP. A host adopted from a
legacy deployment that used `DELIVERY_DRIVER=redis-stream` keeps it: the
installation state records the mode, so updates never switch it back to HTTP.
Subs reads the stream's HMAC secret from
`/etc/nexus/credentials/nexus-subs/redis-hmac-secret`; Core keeps its own
`SUBS_REDIS_*` settings in `/etc/nexus/nexus-core/.env`, and the two must
agree.

The administrator's **Settings → Software** page offers the same local
update, rollback, cleanup, and component actions if the account has
`manage-system`. It cannot operate a remote component or a database-only
host; use that host's CLI.

## Inspect an operation

Changing commands print an operation UUID and normally wait for completion.
If the terminal closes or the CLI stops waiting, the operation may continue
in the background. Use its UUID to check the result:

```bash
sudo nexus operation show <uuid>
```

The JSON form, `sudo nexus operation show <uuid> --json`, is useful for
scripts. For failed, rolled-back, or `recovery_required` results, follow the
[troubleshooting guide](troubleshooting.md) before starting another change.
