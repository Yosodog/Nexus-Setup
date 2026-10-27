# Nexus updater systemd units

`bootstrap.sh` installs the single static `nexus` binary and asks it to install
the embedded root-owned units. The files in this directory mirror those
embedded templates for review and packaging tests.

- `nexus-updater.socket` owns the protected local control socket.
- `nexus-updater.service` runs the small request coordinator.
- `nexus-updater-worker@.service` runs one durable, globally locked mutation.
- `nexus-discord-register.service` registers dedicated-guild slash commands
  under the unprivileged Discord account after install, enable, update, or
  code rollback. Registration failure makes the managed operation fail.

Other component, queue-worker, and scheduler units are embedded in the binary;
their active installation is under `/etc/systemd/system/`. Core's units are
`nexus-core.service` (`default` queue), the `nexus-core-worker@.service`
template for extra `default` queue workers (`PartOf=nexus-core.service`),
`nexus-core-sync.service` (`sync` queue), `nexus-core-pulse-check.service`,
`nexus-core-pulse-work.service`
(Redis Pulse ingest only), `nexus-core-subs-stream.service` (Subs Redis Stream
delivery only), and `nexus-scheduler.service` with its timer.
`nexus-scheduler.service` is a oneshot `schedule:run` tick with
`KillMode=process`, so commands Laravel starts with `runInBackground()` keep
running after the tick exits instead of being killed with its control group.

The coordinator and worker cannot write `/etc/systemd/system`, so a unit added
by a newer updater reaches an existing managed host when bootstrap runs again:
`nexus internal configure-host` rewrites the units, enables the Core units the
host needs, and restarts Core's workers. It leaves a legacy
`nexus-discord.service` in place for adoption to back up.
`sudo nexus doctor --fix` rewrites just the updater's own three units and
reloads systemd, which is enough when only those changed.

The coordinator and worker leave `User=` and `Group=` unset and run as root by
default. With systemd 255, `User=root` together with `NoNewPrivileges=yes`
starts the service without `CAP_SETUID`, and the worker then cannot run Artisan
through `runuser -u nexus-core`.

The worker receives only a validated operation UUID. Release repositories,
assets, paths, units, executables, and command arguments are fixed in the
binary. There is no general root command runner and no network listener.
