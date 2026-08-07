# portmap

TCP port mapping manager daemon. Deploy on a public-facing server to forward traffic to backend services on any reachable network.

## Quick Start

```bash
# Build
go build -o portmap ./cmd/portmap/

# Start daemon
./portmap serve

# Add a port mapping (listen on port 20000, forward to 10.0.0.5:8080)
./portmap add web --target-host 10.0.0.5 --target-port 8080 -g my-group

# Start it
./portmap start web -g my-group

# View all mappings
./portmap list
```

Now `0.0.0.0:20000` forwards to `10.0.0.5:8080`.

## Concepts

### Mapping

A mapping defines a TCP listener that forwards all incoming connections to a target host:port.

| Field        | Required | Default | Description                                    |
|-------------|----------|---------|------------------------------------------------|
| name         | yes      | —       | Unique identifier within its group              |
| target_host  | yes      | —       | Upstream host to forward to                     |
| target_port  | yes      | —       | Upstream port to forward to                     |
| local_port   | no       | auto    | Port portmap listens on. 0 = auto-assign        |

### Group

A group bundles related mappings for batch start/stop and shared port range.

| Field      | Default | Description                                   |
|-----------|---------|-----------------------------------------------|
| name       | —       | Group identifier                              |
| port_range | nil     | Port range for auto-assign (overrides global) |

### State Persistence

Each mapping's running/stopped status is written to `state.json`. When the daemon restarts, it restores the previous state — mappings that were running are started again, stopped mappings stay stopped.

## CLI Reference

All commands communicate with the daemon via HTTP API.

```
PORTMAP_HOME    Data directory, default ~/.config/portmap
--data-dir      Override data directory (persistent flag)
--api-addr      Daemon API address, default 127.0.0.1:9876 (persistent flag)
```

### serve

Start the daemon.

```bash
portmap serve [flags]

Flags:
  --bind-addr string   Bind address for port listeners (default from config: 0.0.0.0)
  --log-file string    Log file path (default stderr only)
```

### add

Add a new port mapping. Mappings are not running by default.

```bash
portmap add <name> [flags]

Flags:
  -g, --group string        Group name (default "default")
  -p, --port int            Local listen port (0=auto, default 0)
      --target-host string  Upstream host (required)
      --target-port int     Upstream port (required)
```

Example:
```bash
portmap add grafana -g infra --target-host 10.0.0.10 --target-port 3000
portmap add ssh-relay -g infra --target-host 10.0.0.11 --target-port 22 -p 2222
```

### update

Update an existing mapping. Only specified fields are changed. If the mapping is running and target or port changes, it's restarted automatically.

```bash
portmap update <name> [flags]

Flags:
  -g, --group string        Group name (default "default")
  -p, --port int            New local port (0=keep current)
      --target-host string  New upstream host
      --target-port int     New upstream port
```

### delete

Delete a mapping. Stops it first if running.

```bash
portmap delete <name> -g <group>
```

### list

List all mappings with status.

```bash
portmap list
```

Output columns: GROUP, NAME, STATUS, LOCAL_PORT, TARGET, STARTED

### start / stop

Start or stop a single mapping.

```bash
portmap start <name> -g <group>
portmap stop <name> -g <group>
```

### group start / group stop

Start or stop all mappings in a group.

```bash
portmap group start <group>
portmap group stop <group>
```

## API

The daemon exposes a REST API on `127.0.0.1:9876`:

| Method   | Path                          | Description         |
|----------|-------------------------------|---------------------|
| GET      | /api/mappings                 | List all mappings   |
| POST     | /api/mappings                 | Add a mapping       |
| PUT      | /api/mappings/:name           | Update a mapping    |
| DELETE   | /api/mappings/:name?group=g   | Delete a mapping    |
| POST     | /api/mappings/:name/start?group=g | Start a mapping |
| POST     | /api/mappings/:name/stop?group=g  | Stop a mapping  |
| POST     | /api/groups/:name/start       | Start group         |
| POST     | /api/groups/:name/stop        | Stop group          |

### Add mapping request

```json
{
  "group": "infra",
  "name": "grafana",
  "local_port": 0,
  "target_host": "10.0.0.10",
  "target_port": 3000
}
```

### List response

```json
[
  {
    "group": "infra",
    "name": "grafana",
    "local_port": 20000,
    "target_host": "10.0.0.10",
    "target_port": 3000,
    "status": "running",
    "started_at": "2026-08-07T16:00:00+08:00"
  }
]
```

## Configuration

Config files live in `$PORTMAP_HOME` (default `~/.config/portmap/`).

### config.json

```json
{
  "bind_addr": "0.0.0.0",
  "port_range": {
    "start": 20000,
    "end": 30000
  },
  "groups": [
    {
      "name": "infra",
      "port_range": {"start": 20000, "end": 20099},
      "mappings": [
        {
          "name": "grafana",
          "local_port": 20000,
          "target_host": "10.0.0.10",
          "target_port": 3000
        }
      ]
    },
    {
      "name": "web",
      "mappings": [
        {
          "name": "api",
          "local_port": 0,
          "target_host": "10.0.0.20",
          "target_port": 8080
        }
      ]
    }
  ]
}
```

- `bind_addr`: IP to bind listeners on. Use `0.0.0.0` to accept connections from any interface, `127.0.0.1` for local only.
- `port_range`: Global fallback range for auto-assigned ports.
- `groups[].port_range`: Per-group port range override. Omitted = use global.
- `mappings[].local_port`: `0` means auto-assign from the applicable port range.

### state.json (daemon-managed, do not edit manually)

```json
{
  "mappings": {
    "infra/grafana": {
      "running": true,
      "local_port": 20000,
      "started_at": "2026-08-07T16:00:00+08:00"
    }
  }
}
```
