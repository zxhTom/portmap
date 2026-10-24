package manager

import (
	"fmt"
	"github.com/zxhtom/portmap/internal/config"
	"github.com/zxhtom/portmap/internal/relayprotocol"
)

func (m *Manager) SetRemoteDomainZones(name string, values []string, primary string) error {
	return m.updateRemoteDomainZones(name, values, primary, true)
}
func (m *Manager) RefreshRemoteDomainZones(name string) error {
	m.mu.Lock()
	remote, err := m.RemoteByName(name)
	requester := m.domainRequester
	m.mu.Unlock()
	if err != nil {
		return err
	}
	response, err := requester(remote, relayprotocol.Request{Operation: "info"})
	if err != nil {
		return err
	}
	return m.updateRemoteDomainZones(name, response.Zones, response.Zone, false)
}
func (m *Manager) updateRemoteDomainZones(name string, values []string, primary string, configure bool) error {
	zones, primary, err := relayprotocol.NormalizeZones(values, primary)
	if err != nil {
		return err
	}
	m.controlMu.Lock()
	defer m.controlMu.Unlock()
	m.mu.Lock()
	cfg := m.cfg
	requester := m.domainRequester
	remote, err := m.RemoteByName(name)
	m.mu.Unlock()
	if err != nil {
		return err
	}
	next := cfg
	next.Remotes = append([]config.Remote(nil), cfg.Remotes...)
	next.Groups = append([]config.Group(nil), cfg.Groups...)
	for i, g := range cfg.Groups {
		next.Groups[i].Mappings = append([]config.Mapping(nil), g.Mappings...)
		for j, mp := range g.Mappings {
			if mp.Remote != name || len(mp.DomainPrefixes) == 0 {
				continue
			}
			zone := mappingZone(remote, mp)
			allowed := false
			for _, z := range zones {
				if z == zone {
					allowed = true
				}
			}
			if !allowed {
				return fmt.Errorf("后缀 %s 仍被隧道 %s/%s 使用", zone, g.Name, mp.Name)
			}
			next.Groups[i].Mappings[j].DomainZone = zone
		}
	}
	if configure {
		if _, err = requester(remote, relayprotocol.Request{Operation: "configure_zones", Zones: zones, Zone: primary}); err != nil {
			return err
		}
	}
	for i := range next.Remotes {
		if next.Remotes[i].Name == name {
			next.Remotes[i].DomainZones = zones
			next.Remotes[i].DomainZone = primary
		}
	}
	if err = config.SaveConfig(m.dir, next); err != nil {
		return err
	}
	return m.reload()
}

func normalizeRemoteZones(r *config.Remote) error {
	if r.DomainZone == "" && len(r.DomainZones) == 0 {
		return nil
	}
	zones, primary, err := relayprotocol.NormalizeZones(r.DomainZones, r.DomainZone)
	if err != nil {
		return err
	}
	r.DomainZone = primary
	r.DomainZones = zones
	return nil
}
