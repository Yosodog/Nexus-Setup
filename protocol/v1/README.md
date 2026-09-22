# Nexus updater protocol v1

The CLI and Nexus AMS GUI both use the root-owned updater over the fixed Unix
socket `/run/nexus-updater/control.sock`. Frames are a four-byte big-endian
length followed by at most 64 KiB of UTF-8 JSON.

The socket accepts only these operations:

- `GetStatus`, `CheckUpdates`, `Doctor`, `GetOperation`, `ListComponents`
- `Update`, `Rollback`, `Cleanup`
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

Unknown fields, operations, components, paths, URLs, commands, service names,
and non-canonical UUIDs are rejected. The updater independently discovers
stable releases from the hardcoded official `Yosodog/Nexus-Setup` GitHub
repository and maps components to hardcoded repositories and asset names.
