package manager

import (
	"fmt"
	"github.com/zxhtom/portmap/internal/config"
	"strings"
)

type RelayPortAllocator func(config.Remote, int, func(int) bool) (int, error)

// SetRelayPortAllocator allows deterministic, offline test fixtures.
func (m *Manager) SetRelayPortAllocator(fn RelayPortAllocator) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.relayPortAllocator = fn
}

func relayIdentity(r config.Remote) string { return strings.ToLower(r.Addr()) }
func (m *Manager) portOwnerLocked(mp config.Mapping) string {
	if !mp.UsesRelay() {
		return ""
	}
	r, err := m.RemoteByName(mp.Remote)
	if err != nil {
		return ""
	}
	for key, e := range m.entries {
		other := e.Mapping
		if !other.UsesRelay() || other.ListenPort != mp.ListenPort {
			continue
		}
		otherRemote, err := m.RemoteByName(other.Remote)
		if err == nil && relayIdentity(r) == relayIdentity(otherRemote) {
			return key.String()
		}
	}
	return ""
}

// CreateMapping probes the actual relay and persists the selected port before
// returning it to CLI/Web clients. Allocation is serialized with config edits.
func (m *Manager) CreateMapping(group string, mp config.Mapping) (config.Mapping, error) {
	m.controlMu.Lock()
	defer m.controlMu.Unlock()
	mp.SetDefaults()
	// Validate everything before allocating; zero is accepted only as a relay
	// creation request, never in persisted configuration.
	candidate := mp
	if mp.ListenPort == 0 && mp.UsesRelay() {
		candidate.ListenPort = 10000
	}
	if err := candidate.Validate(); err != nil {
		return mp, err
	}
	m.mu.Lock()
	if m.shuttingDown {
		m.mu.Unlock()
		return mp, fmt.Errorf("manager is shutting down")
	}
	if _, exists := m.entries[mappingKey{Group: group, Name: mp.Name}]; exists {
		m.mu.Unlock()
		return mp, fmt.Errorf("mapping already exists")
	}
	var remote config.Remote
	if mp.UsesRelay() {
		var err error
		remote, err = m.RemoteByName(mp.Remote)
		if err != nil {
			m.mu.Unlock()
			return mp, err
		}
	}
	reserved := make(map[int]string)
	if mp.UsesRelay() {
		for key, e := range m.entries {
			if e.Mapping.UsesRelay() {
				r, err := m.RemoteByName(e.Mapping.Remote)
				if err == nil && relayIdentity(r) == relayIdentity(remote) {
					reserved[e.Mapping.ListenPort] = key.String()
				}
			}
		}
	}
	allocator := m.relayPortAllocator
	m.mu.Unlock()
	if owner := reserved[mp.ListenPort]; mp.ListenPort != 0 && owner != "" {
		return mp, fmt.Errorf("中转站端口 %d 已被映射 %s 占用，请选择其他端口或留空随机分配", mp.ListenPort, owner)
	}
	if mp.UsesRelay() {
		if allocator == nil {
			return mp, fmt.Errorf("relay port allocator unavailable")
		}
		port, err := allocator(remote, mp.ListenPort, func(port int) bool { return reserved[port] != "" })
		if err != nil {
			return mp, err
		}
		if (mp.ListenPort == 0 && (port < 10000 || port > 65535)) || (mp.ListenPort != 0 && port != mp.ListenPort) {
			return mp, fmt.Errorf("invalid relay port allocation")
		}
		mp.ListenPort = port
	}
	if len(mp.DomainPrefixes) > 0 {
		if m.instanceID == "" {
			return mp, fmt.Errorf("domain mapping requires an initialized manager")
		}
		if err := m.domainRPC(remote, mp, group, "reserve"); err != nil {
			return mp, err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.shuttingDown {
		return mp, fmt.Errorf("manager is shutting down")
	}
	if err := m.addMappingLocked(group, mp); err != nil {
		if len(mp.DomainPrefixes) > 0 {
			m.domainRPC(remote, mp, group, "release")
		}
		return mp, err
	}
	return mp, nil
}
