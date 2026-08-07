# Deployment Guide (Linux)

## Prerequisites

- Linux server with Go 1.25+ (build host) or pre-built binary
- Systemd (most modern Linux distributions)

## 1. Build

On your build machine (can be the target server or a separate machine, as long as the architecture matches):

```bash
git clone <your-repo-url> portmap
cd portmap
CGO_ENABLED=0 go build -ldflags="-s -w" -o portmap ./cmd/portmap/
```

This produces a statically linked `portmap` binary (~6 MB).

If building on a different architecture, set `GOOS` and `GOARCH`:
```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w" -o portmap ./cmd/portmap/
```

## 2. Install Binary

```bash
sudo cp portmap /usr/local/bin/portmap
sudo chmod +x /usr/local/bin/portmap
```

## 3. Create Directories

```bash
sudo mkdir -p /etc/portmap
sudo mkdir -p /var/log/portmap
sudo chown nobody:nogroup /var/log/portmap
```

## 4. Install systemd Service

```bash
sudo cp deploy/portmap.service /etc/systemd/system/portmap.service
sudo systemctl daemon-reload
```

Verify the service file contents:
```bash
cat /etc/systemd/system/portmap.service
```

Expected:
```ini
[Unit]
Description=Portmap - Port Mapping Manager
After=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/portmap serve --log-file=/var/log/portmap/portmap.log
Restart=always
RestartSec=5
User=nobody
Group=nogroup
Environment=PORTMAP_HOME=/etc/portmap

[Install]
WantedBy=multi-user.target
```

### Security note

The service runs as `nobody:nogroup` by default. This user needs:
- Read/write access to `$PORTMAP_HOME` (config + state files)
- Write access to the log file path
- Permission to bind to the configured ports

Port binding below 1024 requires `CAP_NET_BIND_SERVICE` or root. If you need low ports, add to the service file:
```ini
[Service]
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
```

Or change User to root (not recommended):
```ini
User=root
```

## 5. Configure portmap

Edit `/etc/portmap/config.json`:

```json
{
  "bind_addr": "0.0.0.0",
  "port_range": {
    "start": 20000,
    "end": 30000
  },
  "groups": []
}
```

Or add mappings interactively after starting the daemon (see below).

## 6. Start the Service

```bash
sudo systemctl enable portmap
sudo systemctl start portmap
sudo systemctl status portmap
```

Check logs:
```bash
sudo journalctl -u portmap -f
# or
sudo tail -f /var/log/portmap/portmap.log
```

## 7. Add Port Mappings

The daemon is now running. Use the CLI to manage mappings:

```bash
# Point CLI at the daemon's API
export PORTMAP_HOME=/etc/portmap

# Add a mapping: public port 20000 -> internal 10.0.0.5:8080
sudo -E portmap add my-api --target-host 10.0.0.5 --target-port 8080 -g web

# Start it
sudo -E portmap start my-api -g web

# View
sudo -E portmap list
```

Since the daemon runs as `nobody`, the CLI also needs read/write access to `$PORTMAP_HOME` or communicate purely via the API. Running CLI commands with `sudo -E` ensures access.

Alternatively, if you grant your admin user access to the data directory:
```bash
sudo chown -R youruser:youruser /etc/portmap
```

Then update the service file's `User` to match, or keep using `sudo`.

## 8. Firewall

Ensure the mapped ports are open in your firewall:

```bash
# ufw
sudo ufw allow 20000:30000/tcp

# iptables
sudo iptables -A INPUT -p tcp --dport 20000:30000 -j ACCEPT

# firewalld
sudo firewall-cmd --permanent --add-port=20000-30000/tcp
sudo firewall-cmd --reload
```

## 9. Verify

From an external machine:

```bash
# Test that the mapped port is reachable
curl http://<server-public-ip>:20000

# Or check port is open
nc -zv <server-public-ip> 20000
```

## 10. Management Commands

```bash
# List all mappings
sudo -E portmap list

# Add a mapping with fixed port
sudo -E portmap add grafana --target-host 10.0.0.10 --target-port 3000 -p 23000 -g infra

# Update target
sudo -E portmap update grafana -g infra --target-port 3001

# Start/stop individual mapping
sudo -E portmap start grafana -g infra
sudo -E portmap stop grafana -g infra

# Start/stop entire group
sudo -E portmap group start web
sudo -E portmap group stop web

# Delete a mapping (stops it first)
sudo -E portmap delete grafana -g infra
```

## Upgrading

```bash
# Stop service
sudo systemctl stop portmap

# Replace binary
sudo cp portmap /usr/local/bin/portmap

# Start service
sudo systemctl start portmap

# Mappings are restored from state.json automatically
```

## Troubleshooting

### Service fails to start

```bash
sudo journalctl -u portmap -n 50 --no-pager
```

Common issues:
- **Port already in use**: Check `ss -tlnp | grep <port>`
- **Permission denied on data dir**: `sudo chown nobody:nogroup /etc/portmap`
- **Bind address already in use**: Another process or a previous portmap instance is using the port

### CLI can't connect to daemon

```bash
# Check daemon is running
sudo systemctl status portmap

# Check API is listening
ss -tlnp | grep 9876

# Verify PORTMAP_HOME matches daemon's config
echo $PORTMAP_HOME
```

### Mappings not persisting across restarts

Ensure `nobody` has write permission to `$PORTMAP_HOME/state.json`:
```bash
sudo chown nobody:nogroup /etc/portmap/state.json
sudo chmod 600 /etc/portmap/state.json
```

### Low port binding (<1024)

Add to service file:
```ini
[Service]
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
```

Then:
```bash
sudo systemctl daemon-reload
sudo systemctl restart portmap
```

---

# Relay Tunnel Deployment (NAT Traversal)

This setup is for the **ABC scenario**: A (internal, no public IP) → B (VPS, public IP) tunnel, C (external client) accesses A through B.

## Architecture

```
Internal machine A              VPS B (public IP)           External client C
┌──────────────┐    tunnel     ┌──────────────┐           ┌──────────┐
│ portmap      │────outbound──→│ portmap serve │           │          │
│ connect      │               │  --relay     │←───TCP───│ client   │
│              │               │  port 20000  │           │          │
│ target:8080  │               └──────────────┘           └──────────┘
└──────────────┘
```

- **A** initiates the outbound tunnel to B (only A→B direction needs to be reachable)
- **B** listens on public ports, forwards traffic through the tunnel to A
- **C** connects to B's public port just like any normal service

## Deploy on VPS (B)

Same as the standard deployment, but with `--relay`:

```bash
# Build and install as before
sudo cp portmap /usr/local/bin/portmap
sudo mkdir -p /etc/portmap /var/log/portmap
sudo chown nobody:nogroup /var/log/portmap
```

### Service file (with relay)

`/etc/systemd/system/portmap.service`:

```ini
[Unit]
Description=Portmap Relay Server
After=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/portmap serve --relay --log-file=/var/log/portmap/portmap.log
Restart=always
RestartSec=5
User=nobody
Group=nogroup
Environment=PORTMAP_HOME=/etc/portmap

# For ports below 1024 (optional)
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE

[Install]
WantedBy=multi-user.target
```

Enable and start:
```bash
sudo systemctl daemon-reload
sudo systemctl enable --now portmap
```

The relay server listens on port 7000 (default, configurable in `config.json` `relay.listen_addr`). Make sure this port is reachable from internal machine A:

```bash
sudo ufw allow 7000/tcp
```

### Add mappings on VPS

```bash
sudo -E portmap add my-app --target-host <any-ip> --target-port 8080 -g infra
sudo -E portmap start my-app -g infra
```

Note: `--target-host` and `--target-port` here are informational (shown in list). The actual target is determined by the tunnel client on machine A.

## Deploy on Internal Machine (A)

The internal machine only needs the `portmap` binary — no service needed. It runs `portmap connect`.

### Option 1: Simple CLI (manual)

```bash
./portmap connect --relay-addr <vps-public-ip>:7000
```

This reads mappings from the server's config and tunnels them all.

### Option 2: tunnel.json (explicit)

Create `~/.config/portmap/tunnel.json`:

```json
{
  "relay_addr": "vps-public-ip:7000",
  "tunnels": [
    {
      "mapping_id": "infra/my-app",
      "target_host": "127.0.0.1",
      "target_port": 8080
    }
  ]
}
```

Then run:
```bash
./portmap connect
```

### Option 3: systemd service for auto-start on A

`/etc/systemd/system/portmap-tunnel.service`:

```ini
[Unit]
Description=Portmap Tunnel Client
After=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/portmap connect --relay-addr <vps-public-ip>:7000
Restart=always
RestartSec=5
User=nobody
Group=nogroup
Environment=PORTMAP_HOME=/etc/portmap

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl enable --now portmap-tunnel
```

The tunnel client auto-reconnects on connection loss.

## Verify

On VPS (B):
```bash
sudo -E portmap list
```

Should show:
```
GROUP  NAME     STATUS     LOCAL_PORT  TARGET
infra  my-app   connected  20000       127.0.0.1:8080
```

Status `connected` means the tunnel is active.

From external client (C):
```bash
curl http://<vps-public-ip>:20000
```

## Firewall Summary

| Machine | Port    | Direction | Purpose           |
|---------|---------|-----------|-------------------|
| VPS B   | 20000+  | IN        | Public mapped ports |
| VPS B   | 7000    | IN        | Relay tunnel from A |
| VPS B   | 9876    | LOCAL     | Management API |
| Machine A | any   | OUT       | Outbound to B:7000 |
