# Nexus updater protocol v1

The CLI and Nexus AMS GUI both use the root-owned updater over the fixed Unix
socket `/run/nexus-updater/control.sock`. Frames are a four-byte big-endian
length followed by at most 64 KiB of UTF-8 JSON.

The socket accepts only these operations:

- `GetStatus`, `CheckUpdates`, `Doctor`, `GetOperation`, `ListComponents`
- `Update`, `Rollback`, `Cleanup`, `SetChannel`, `SetCoreWorkers`
- `InstallComponent`, `EnableComponent`, `DisableComponent`, `RestartComponent`

Components are exactly `nexus-core`, `nexus-subs`, and `nexus-discord`.

```json
{
  "protocol_version": 1,
  "request_id": "550e8400-e29b-41d4-a716-446655440000",
  "operation": "RestartComponent",
  "payload": {
    "component_id": "nexus-subs",
    "operation_id": "550e8400-e29b-41d4-a716-446655440000",
    "source": "gui"
  }
}
```

Mutations use a caller-generated operation UUID for idempotency. Only Discord
installation accepts configuration, limited to `bot_token`, `client_id`, and
`guild_id`. The coordinator moves those write-only values into a root-owned
runtime file and does not write them into operation state.

`SetChannel` requires `channel`, which is `stable` or `beta`; no other
operation accepts it. `SetCoreWorkers` requires `core_workers`, an integer from
1 to 8, and no other operation accepts it. `GetStatus` reports the count as
`core_workers` when Core is installed. `Rollback` optionally accepts a `component_id`. Without one it restores the
previous release of every component the last update changed.

Unknown fields, operations, components, paths, URLs, commands, service names,
and non-canonical UUIDs are rejected. The updater independently discovers
releases from hardcoded official GitHub repositories: `Yosodog/Nexus-Setup`
for itself and one repository per component, each versioned independently.
It never accepts a release, repository, or asset name from a caller.

`CheckUpdates` returns the planned `components` changes, any `held_back`
releases with the unmet requirement, and an optional `updater_release`.
`GetStatus` reports each component's `release_id` and `previous_release_id`.
