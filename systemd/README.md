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
their active installation is under `/etc/systemd/system/`.

The worker receives only a validated operation UUID. Release repositories,
assets, paths, units, executables, and command arguments are fixed in the
binary. There is no general root command runner and no network listener.
