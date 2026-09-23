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

`status` reports the profile, release, component state, and recent operations.
`doctor` checks installation state, disk space, and managed directories.
`component list` shows which local components are installed and healthy.
`update --check` lists stable releases that would be applied. Database-only
hosts have no application release and cannot use update commands.

Core installations made before the mutation-key prompt was added need
`PW_API_MUTATION_KEY` added securely to the root-controlled
`/etc/nexus/nexus-core/.env`. `nexus update` preserves that file and will not
ask for the key later.

## Update, rollback, and cleanup

Before an update, take a **manual database backup or snapshot**. Nexus Setup
does not do this for you. Then run:

```bash
sudo nexus update --check
sudo nexus update
sudo nexus status
sudo nexus component list
```

The updater applies each intermediate stable release in order, runs
forward-only database migrations, activates the new code, and checks local
components. If health checks fail after activation, it switches code back to
the previous release. Database migrations are **not** reversed. If the update
finishes unsuccessfully, inspect the operation before retrying.

`sudo nexus rollback` restores the immediately previous **code** release when
one exists. It does not restore the database. Check application and data
compatibility before using it. To reclaim old managed releases, run
`sudo nexus cleanup`; the command previews what it will remove and keeps the
current and previous release. It leaves database data, credentials, uploads,
writable storage, operation history, and preserved legacy checkouts alone.

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

Install Subs only if it is absent on a Core host. It reuses Core's local
configuration. Discord is optional: first create a Discord application and
bot, then install it on a Core host. The command asks for its bot token,
application ID, and guild ID. The IDs must be 17–20 digit numbers. Invite
the bot to your guild separately. Managed install and update register its
guild commands; a registration failure makes the operation fail. Core can be
restarted, but it cannot be installed, enabled, or disabled as an optional
component. Use `nexus component list` to see the actions available locally.

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
