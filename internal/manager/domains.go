package manager

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/zxhtom/portmap/internal/config"
	"github.com/zxhtom/portmap/internal/relayprotocol"
	"strings"
)

type DomainRequester func(config.Remote, relayprotocol.Request) (relayprotocol.Response, error)

func (m *Manager) SetDomainRequester(fn DomainRequester) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.domainRequester = fn
}

func domainOwner(instanceID, group, name string) string {
	sum := sha256.Sum256([]byte(instanceID + "\x00" + group + "\x00" + name))
	return hex.EncodeToString(sum[:])
}
func domainZone(remote config.Remote) string {
	if remote.DomainZone != "" {
		return strings.ToLower(strings.TrimSuffix(remote.DomainZone, "."))
	}
	return relayprotocol.DefaultZone
}

func (m *Manager) DomainZone() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cfg.DomainZone != "" {
		return m.cfg.DomainZone
	}
	return relayprotocol.DefaultZone
}

// SetDomainZone changes the suffix for every remote on this client. The relay
// must already serve the requested zone; otherwise existing routes stay intact.
func (m *Manager) SetDomainZone(value string) error {
	zone, err := relayprotocol.ValidateZone(value)
	if err != nil {
		return err
	}
	m.controlMu.Lock()
	m.mu.Lock()
	current := m.cfg
	requester := m.domainRequester
	m.mu.Unlock()
	if current.DomainZone == zone {
		m.controlMu.Unlock()
		return nil
	}
	seen := map[string]bool{}
	for _, group := range current.Groups {
		for _, mp := range group.Mappings {
			if len(mp.DomainPrefixes) == 0 || seen[mp.Remote] {
				continue
			}
			seen[mp.Remote] = true
			remote := remoteFor(current, mp.Remote)
			response, err := requester(remote, relayprotocol.Request{Operation: "info"})
			if err != nil {
				m.controlMu.Unlock()
				return err
			}
			if response.Zone != zone {
				m.controlMu.Unlock()
				return fmt.Errorf("中转站 %s 的域名后缀为 %s，请先配置为 %s", mp.Remote, response.Zone, zone)
			}
		}
	}
	current.DomainZone = zone
	if err := config.SaveConfig(m.dir, current); err != nil {
		m.controlMu.Unlock()
		return err
	}
	m.controlMu.Unlock()
	return m.Reconcile()
}
func (m *Manager) domainRPC(remote config.Remote, mp config.Mapping, group, operation string) error {
	_, err := m.domainRequester(remote, relayprotocol.Request{Operation: operation, Owner: domainOwner(m.instanceID, group, mp.Name), Prefixes: mp.DomainPrefixes, Port: mp.ListenPort, Scheme: mp.UpstreamScheme, Zone: mappingZone(remote, mp)})
	return err
}
func (m *Manager) releaseDomains(group string, mp config.Mapping, cfg config.Config) error {
	if len(mp.DomainPrefixes) == 0 {
		return nil
	}
	return m.domainRPC(remoteFor(cfg, mp.Remote), mp, group, "release")
}

// SetDomains updates a mapping in place, with a brief reconnect when its
// transport changes between public TCP and loopback-only domain routing.
func (m *Manager) SetDomains(group, name string, prefixes []string, scheme string, zones ...string) error {
	prefixes, err := relayprotocol.Normalize(prefixes)
	if err != nil {
		return err
	}
	m.controlMu.Lock()
	err = m.saveDomains(group, name, prefixes, scheme, zones...)
	m.controlMu.Unlock()
	if err != nil {
		return err
	}
	return m.Reconcile()
}
func (m *Manager) saveDomains(group, name string, prefixes []string, scheme string, zones ...string) error {
	key := mappingKey{Group: group, Name: name}
	m.mu.Lock()
	e := m.entries[key]
	cfg := m.cfg
	shutting := m.shuttingDown
	m.mu.Unlock()
	if e == nil || shutting {
		return fmt.Errorf("mapping unavailable")
	}
	e.startMu.Lock()
	defer e.startMu.Unlock()
	old := e.Mapping
	updated := old
	updated.DomainPrefixes = prefixes
	updated.UpstreamScheme = scheme
	updated.SetDefaults()
	if len(zones) > 0 && zones[0] != "" {
		updated.DomainZone = zones[0]
		if err := selectMappingZone(remoteFor(cfg, updated.Remote), &updated); err != nil {
			return err
		}
	}
	if err := updated.Validate(); err != nil {
		return err
	}
	if len(prefixes) > 0 {
		if err := m.domainRPC(remoteFor(cfg, updated.Remote), updated, group, "check"); err != nil {
			return err
		}
	}
	// Stop the old lease before changing its domain set, so its heartbeat cannot
	// race with reassignment. The original config remains intact on write failure.
	e.mu.RLock()
	backend := e.Backend
	e.mu.RUnlock()
	if backend != nil {
		if err := backend.Close(); err != nil {
			return err
		}
	}
	if len(prefixes) > 0 {
		if err := m.domainRPC(remoteFor(cfg, updated.Remote), updated, group, "reserve"); err != nil {
			return err
		}
	}
	next := cfg
	next.Groups = append([]config.Group(nil), cfg.Groups...)
	for i := range next.Groups {
		next.Groups[i].Mappings = append([]config.Mapping(nil), cfg.Groups[i].Mappings...)
		if next.Groups[i].Name == group {
			for j := range next.Groups[i].Mappings {
				if next.Groups[i].Mappings[j].Name == name {
					next.Groups[i].Mappings[j] = updated
				}
			}
		}
	}
	if err := config.SaveConfig(m.dir, next); err != nil {
		if len(old.DomainPrefixes) > 0 {
			m.domainRPC(remoteFor(cfg, old.Remote), old, group, "reserve")
		} else if len(prefixes) > 0 {
			m.domainRPC(remoteFor(cfg, updated.Remote), updated, group, "release")
		}
		return err
	}
	if len(prefixes) == 0 && len(old.DomainPrefixes) > 0 {
		if err := m.domainRPC(remoteFor(cfg, old.Remote), old, group, "release"); err != nil {
			config.SaveConfig(m.dir, cfg)
			return err
		}
	}
	return nil
}

func mappingZone(remote config.Remote, mp config.Mapping) string {
	if mp.DomainZone != "" {
		return mp.DomainZone
	}
	return domainZone(remote)
}
func remoteZones(remote config.Remote) []string {
	if len(remote.DomainZones) > 0 {
		return append([]string(nil), remote.DomainZones...)
	}
	return []string{domainZone(remote)}
}
func selectMappingZone(remote config.Remote, mp *config.Mapping) error {
	if len(mp.DomainPrefixes) == 0 {
		mp.DomainZone = ""
		return nil
	}
	zone := mappingZone(remote, *mp)
	var err error
	zone, err = relayprotocol.ValidateZone(zone)
	if err != nil {
		return err
	}
	for _, allowed := range remoteZones(remote) {
		if allowed == zone {
			mp.DomainZone = zone
			return nil
		}
	}
	return fmt.Errorf("后缀 %s 不属于 VPS %s", zone, remote.Name)
}

func transportRemote(r config.Remote, mp config.Mapping) config.Remote {
	r.DomainZone = mappingZone(r, mp)
	r.DomainZones = nil
	return r
}
