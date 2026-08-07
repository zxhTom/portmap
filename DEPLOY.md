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
