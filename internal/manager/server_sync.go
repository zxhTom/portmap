package manager

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/zxhtom/portmap/internal/config"
	"github.com/zxhtom/portmap/internal/relayprotocol"
)

func (m *Manager) SetServerRequester(fn func(config.Remote, relayprotocol.Request) (relayprotocol.Response, error)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.serverRequester = fn
}

func mappingTarget(row MappingInfo) string {
	if row.Type == config.TypeMultipass {
		return fmt.Sprintf("%s:%d", row.Instance, row.InstancePort)
	}
	return net.JoinHostPort(row.LocalHost, fmt.Sprint(row.LocalPort))
}

func (m *Manager) SyncServers() {
	m.mu.Lock()
	if m.shuttingDown || m.instanceID == "" {
		m.mu.Unlock()
		return
	}
	clientID := m.instanceID
	cfg := m.cfg
	requester := m.serverRequester
	m.mu.Unlock()
	if requester == nil {
		return
	}
	clientName, _ := os.Hostname()
	rows := m.ListMappings()
	type batch struct {
		remote config.Remote
		items  []relayprotocol.MappingReport
	}
	batches := make(map[string]*batch)
	for _, remote := range cfg.Remotes {
		effective := remoteFor(cfg, remote.Name)
		identity := relayIdentity(effective)
		if _, exists := batches[identity]; !exists {
			batches[identity] = &batch{remote: effective}
		}
	}
	for _, row := range rows {
		if row.Remote == "" {
			continue
		}
		remote := remoteFor(cfg, row.Remote)
		b := batches[relayIdentity(remote)]
		if b == nil {
			continue
		}
		listenHost := "0.0.0.0"
		if len(row.Domains) > 0 {
			listenHost = "127.0.0.1"
		}
		b.items = append(b.items, relayprotocol.MappingReport{
			Owner: domainOwner(clientID, row.Group, row.Name),
			Group: row.Group, Name: row.Name, Type: row.Type,
			ListenHost: listenHost, ListenPort: row.ListenPort,
			Target: mappingTarget(row), Domains: row.Domains,
			Description: row.Description, DesiredRunning: row.DesiredRunning,
			Status: row.Status, Health: row.Health, Invalid: row.InvalidReason != "", ExpiresAt: row.ExpiresAt,
		})
	}
	for _, b := range batches {
		m.mu.Lock()
		results := []relayprotocol.RecoveryResult{}
		owners := map[string]bool{}
		for _, item := range b.items {
			owners[item.Owner] = true
		}
		for owner, result := range m.recoveryResults[relayIdentity(b.remote)] {
			if owners[owner] {
				results = append(results, result)
			} else {
				delete(m.recoveryResults[relayIdentity(b.remote)], owner)
			}
		}
		m.mu.Unlock()
		snapshot := relayprotocol.Request{RecoveryProtocol: 2, RecoveryResults: results, Operation: "tunnel_snapshot", ClientID: clientID, ClientName: clientName, Mappings: b.items}
		response, err := requester(b.remote, snapshot)
		if err != nil && (err.Error() == "invalid request" || strings.Contains(err.Error(), `unknown field "recovery_protocol"`)) {
			snapshot.RecoveryProtocol = 0
			response, err = requester(b.remote, snapshot)
		}
		if err != nil {
			continue
		} // Plain SSH VPS or an older relay remains usable.
		disabled := make(map[string]bool, len(response.DisabledOwners))
		for _, owner := range response.DisabledOwners {
			disabled[owner] = true
		}
		m.mu.Lock()
		for _, item := range b.items {
			m.serverDisabled[item.Owner] = disabled[item.Owner]
		}
		m.mu.Unlock()
		for _, item := range b.items {
			if disabled[item.Owner] {
				m.closeDisabledMapping(mappingKey{Group: item.Group, Name: item.Name})
			}
		}
		for _, command := range response.RecoveryCommands {
			m.processRecovery(b.remote, b.items, command)
		}
	}
}

func (m *Manager) closeDisabledMapping(key mappingKey) {
	m.mu.Lock()
	e := m.entries[key]
	m.mu.Unlock()
	if e == nil {
		return
	}
	e.startMu.Lock()
	defer e.startMu.Unlock()
	e.mu.Lock()
	b := e.Backend
	e.mu.Unlock()
	if b != nil {
		if err := b.Close(); err != nil {
			e.mu.Lock()
			e.lastError = fmt.Sprintf("cannot close VPS-disabled mapping: %v", err)
			e.mu.Unlock()
			return
		}
	}
	e.mu.Lock()
	e.Backend = nil
	e.lastError = "mapping disabled on VPS"
	e.mu.Unlock()
}

func (m *Manager) runServerSync(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		m.SyncServers()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
