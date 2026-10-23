package manager

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/zxhtom/portmap/internal/config"
	"github.com/zxhtom/portmap/internal/forward"
	"github.com/zxhtom/portmap/internal/relayprotocol"
)

// Run reconciles configuration and desired running state until cancellation.
func (m *Manager) Run(ctx context.Context) {
	syncDone := make(chan struct{})
	go func() { defer close(syncDone); m.runServerSync(ctx) }()
	defer func() { <-syncDone }()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := m.reconcile(true); err != nil {
				slog.Warn("configuration rejected; retaining last valid configuration", "err", err)
			}
		}
	}
}

// Reconcile retries failed mappings, including failures on initial startup.
func (m *Manager) Reconcile() error { return m.reconcile(false) }

func (m *Manager) reconcile(periodic bool) error {
	m.controlMu.Lock()
	defer m.controlMu.Unlock()
	err := m.reload()
	if expireErr := m.expireMappings(); expireErr != nil {
		return expireErr
	}
	m.mu.Lock()
	if m.shuttingDown {
		m.mu.Unlock()
		return err
	}
	entries := make(map[mappingKey]*entry)
	for key, e := range m.entries {
		if st := m.state.Mappings[key.String()]; st.Running && !st.Invalid && !e.Mapping.Expired(time.Now()) {
			entries[key] = e
		}
	}
	m.mu.Unlock()
	for key, e := range entries {
		e.startMu.Lock()
		m.mu.Lock()
		st := m.state.Mappings[key.String()]
		eligible := !m.shuttingDown && m.entries[key] == e && st.Running && !st.Invalid
		m.mu.Unlock()
		if !eligible {
			e.startMu.Unlock()
			continue
		}
		e.mu.RLock()
		b := e.Backend
		e.mu.RUnlock()
		// Configuration is polled every 5 seconds; network checks/retries every 30 seconds.
		last, _ := time.Parse(time.RFC3339Nano, st.CheckedAt)
		if periodic && time.Since(last) < 30*time.Second {
			e.startMu.Unlock()
			continue
		}
		var checkErr error
		if b == nil || b.Status().State != forward.StateRunning {
			checkErr = fmt.Errorf("backend is not running")
		} else if checker, ok := b.(interface{ Check(context.Context) error }); ok {
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			checkErr = checker.Check(ctx)
			cancel()
		}
		if checkErr != nil {
			// Close first so Start cannot mistake an unhealthy listener for a healthy tunnel.
			if b != nil {
				if closeErr := b.Close(); closeErr != nil {
					checkErr = fmt.Errorf("%w; close: %v", checkErr, closeErr)
				}
			}
			// The failed health round counts once, regardless of rebuild outcome.
			retry := m.recordCheck(key, e, checkErr)
			e.startMu.Unlock()
			if retry {
				if startErr := m.startEntry(key, e, true); startErr != nil {
					slog.Warn("mapping recovery failed", "mapping", key.String(), "err", startErr)
				}
			}
		} else {
			m.recordCheck(key, e, nil)
			e.startMu.Unlock()
		}
	}
	return err
}

func validateConfig(c *config.Config) error {
	if c.DomainZone != "" {
		zone, err := relayprotocol.ValidateZone(c.DomainZone)
		if err != nil || zone != c.DomainZone {
			return fmt.Errorf("invalid domain_zone")
		}
	}
	remotes := make(map[string]bool)
	for _, r := range c.Remotes {
		if r.Name == "" || r.Host == "" || r.Port < 0 || r.Port > 65535 || remotes[r.Name] {
			return fmt.Errorf("invalid or duplicate remote")
		}
		remotes[r.Name] = true
	}
	groups := make(map[string]bool)
	keys := make(map[mappingKey]bool)
	ports := make(map[string]string)
	domains := make(map[string]string)
	for i := range c.Groups {
		g := &c.Groups[i]
		if g.Name == "" || groups[g.Name] {
			return fmt.Errorf("invalid or duplicate group")
		}
		groups[g.Name] = true
		for j := range g.Mappings {
			mp := &g.Mappings[j]
			mp.SetDefaults()
			if err := mp.Validate(); err != nil {
				return fmt.Errorf("invalid mapping in group %s: %w", g.Name, err)
			}
			key := mappingKey{g.Name, mp.Name}
			if keys[key] || ((mp.EffectiveType() == config.TypeSSH || (mp.EffectiveType() == config.TypeMultipass && mp.Remote != "")) && !remotes[mp.Remote]) {
				return fmt.Errorf("duplicate mapping or missing remote for %s", key.String())
			}
			if mp.UsesRelay() {
				identity := fmt.Sprintf("%s/%d", relayIdentity(remoteFor(*c, mp.Remote)), mp.ListenPort)
				if owner := ports[identity]; owner != "" {
					return fmt.Errorf("中转站端口 %d 被 %s 和 %s 重复使用", mp.ListenPort, owner, key.String())
				}
				ports[identity] = key.String()
			}
			if len(mp.DomainPrefixes) > 0 {
				r := remoteFor(*c, mp.Remote)
				for _, prefix := range mp.DomainPrefixes {
					host := prefix + "." + domainZone(r)
					identity := relayIdentity(r) + "/" + host
					if owner := domains[identity]; owner != "" {
						return fmt.Errorf("域名 %s 被 %s 和 %s 重复绑定", host, owner, key.String())
					}
					domains[identity] = key.String()
				}
			}
			keys[key] = true
		}
	}
	return nil
}

func remoteFor(c config.Config, name string) config.Remote {
	for _, r := range c.Remotes {
		if r.Name == name {
			if r.DomainZone == "" && c.DomainZone != "" {
				r.DomainZone = c.DomainZone
			}
			return r
		}
	}
	return config.Remote{}
}

func (m *Manager) reload() error {
	// Read once: never recreate config.json if it disappears during an atomic save.
	data, err := os.ReadFile(filepath.Join(m.dir, "config.json"))
	if err != nil {
		return fmt.Errorf("read config.json: %w", err)
	}
	next := config.DefaultConfig()
	if err := json.Unmarshal(data, &next); err != nil {
		return fmt.Errorf("cannot parse config.json")
	}
	if next.Docker.Host == "" {
		next.Docker.Host = config.DefaultDockerHost
	}
	if next.Docker.HelperImage == "" {
		next.Docker.HelperImage = config.DefaultDockerHelperImage
	}
	if err := validateConfig(&next); err != nil {
		return err
	}
	if m.dockerHostOverride != nil {
		next.Docker.Host = *m.dockerHostOverride
	}
	if m.dockerImageOverride != nil {
		next.Docker.HelperImage = *m.dockerImageOverride
	}
	m.mu.Lock()
	if m.shuttingDown || reflect.DeepEqual(m.cfg, next) {
		m.mu.Unlock()
		return nil
	}
	previous := m.cfg
	wanted := make(map[mappingKey]config.Mapping)
	for _, g := range next.Groups {
		for _, mp := range g.Mappings {
			wanted[mappingKey{g.Name, mp.Name}] = mp
		}
	}
	changed := make(map[mappingKey]*entry)
	for key, e := range m.entries {
		mp, exists := wanted[key]
		dependenciesChanged := (e.Mapping.EffectiveType() == config.TypeSSH || (e.Mapping.EffectiveType() == config.TypeMultipass && e.Mapping.Remote != "")) && remoteFor(previous, e.Mapping.Remote) != remoteFor(next, mp.Remote)
		if e.Mapping.EffectiveType() == config.TypeDocker && previous.Docker != next.Docker {
			dependenciesChanged = true
		}
		oldTransport, newTransport := e.Mapping, mp
		oldTransport.Description = ""
		oldTransport.ExpiresAt = ""
		newTransport.ExpiresAt = ""
		newTransport.Description = ""
		if !exists || !reflect.DeepEqual(oldTransport, newTransport) || dependenciesChanged {
			changed[key] = e
		}
	}
	m.mu.Unlock()
	for key, e := range changed {
		e.startMu.Lock()
		e.mu.RLock()
		b := e.Backend
		e.mu.RUnlock()
		if b != nil {
			if err := b.Close(); err != nil {
				e.startMu.Unlock()
				return fmt.Errorf("close changed mapping %s: %w", key.String(), err)
			}
		}
		if desired, exists := wanted[key]; !exists || len(desired.DomainPrefixes) == 0 || relayIdentity(remoteFor(previous, e.Mapping.Remote)) != relayIdentity(remoteFor(next, desired.Remote)) {
			if err := m.releaseDomains(key.Group, e.Mapping, previous); err != nil {
				e.startMu.Unlock()
				return err
			}
		}
		m.mu.Lock()
		delete(m.entries, key)
		if _, exists := wanted[key]; !exists {
			delete(m.state.Mappings, key.String())
		}
		m.mu.Unlock()
		e.startMu.Unlock()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, mp := range wanted {
		if _, exists := m.entries[key]; !exists {
			m.entries[key] = &entry{Mapping: mp, Group: key.Group}
		} else {
			m.entries[key].Mapping.Description = mp.Description
			m.entries[key].Mapping.ExpiresAt = mp.ExpiresAt
		}
		if _, exists := m.state.Mappings[key.String()]; !exists {
			m.state.Mappings[key.String()] = config.MappingState{}
		}
	}
	m.cfg = next
	slog.Info("configuration reloaded")
	return config.SaveState(m.dir, m.state)
}
