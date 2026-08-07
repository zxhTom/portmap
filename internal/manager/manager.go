package manager

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/zxhtom/portmap/internal/config"
	"github.com/zxhtom/portmap/internal/proxy"
	"github.com/zxhtom/portmap/internal/relay"
)

type mappingKey struct {
	Group string
	Name  string
}

func (k mappingKey) String() string {
	return k.Group + "/" + k.Name
}

type entry struct {
	Mapping config.Mapping
	Group   string
	Proxy   *proxy.Proxy
}

type Manager struct {
	mu    sync.Mutex
	dir   string
	cfg   config.Config
	state config.State

	proxies map[mappingKey]*entry
	relay   *relay.Server
}

func New(dir string) *Manager {
	return &Manager{
		dir:     dir,
		proxies: make(map[mappingKey]*entry),
	}
}

func (m *Manager) Init() error {
	cfg, err := config.LoadConfig(m.dir)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	m.cfg = cfg

	if m.cfg.Relay.Enabled && m.cfg.Relay.ListenAddr != "" {
		creds := make(map[string]string)
		for _, u := range m.cfg.Relay.Users {
			creds[u.User] = u.Password
		}
		m.relay = relay.NewServer(m.cfg.Relay.ListenAddr, creds)
		if err := m.relay.Start(); err != nil {
			return fmt.Errorf("relay start: %w", err)
		}
	}

	state, err := config.LoadState(m.dir)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}
	m.state = state

	for _, g := range m.cfg.Groups {
		for _, mp := range g.Mappings {
			key := mappingKey{Group: g.Name, Name: mp.Name}
			e := &entry{Mapping: mp, Group: g.Name}
			m.proxies[key] = e

			st, ok := m.state.Mappings[key.String()]
			if !ok {
				st = config.MappingState{Running: false, LocalPort: mp.LocalPort}
				m.state.Mappings[key.String()] = st
			}
			if ok && st.Running {
				m.restoreMappingLocked(key, e, st)
			}
		}
	}
	if err := config.SaveState(m.dir, m.state); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	return nil
}

func (m *Manager) restoreMappingLocked(key mappingKey, e *entry, st config.MappingState) {
	localPort := st.LocalPort
	if localPort == 0 {
		localPort = e.Mapping.LocalPort
	}
	group := m.mustGroup(key.Group)
	if group == nil {
		return
	}
	if localPort == 0 {
		localPort = m.autoPort(*group)
	}

	if m.HasRelay() {
		e.Mapping.LocalPort = localPort
		st.LocalPort = localPort
	} else {
		lnAddr := fmt.Sprintf("%s:%d", m.cfg.BindAddr, localPort)
		tgAddr := fmt.Sprintf("%s:%d", e.Mapping.TargetHost, e.Mapping.TargetPort)
		e.Proxy = proxy.New(lnAddr, tgAddr)
		if err := e.Proxy.Start(); err != nil {
			slog.Error("restore proxy failed", "key", key.String(), "err", err)
			st.Running = false
		}
		st.LocalPort = localPort
	}
	m.state.Mappings[key.String()] = st
}

func (m *Manager) Shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.proxies {
		if e.Proxy != nil {
			e.Proxy.Stop()
		}
	}
	if m.relay != nil {
		m.relay.Close()
	}
	config.SaveState(m.dir, m.state)
}

func (m *Manager) HasRelay() bool {
	return m.relay != nil
}

func (m *Manager) Relay() *relay.Server {
	return m.relay
}

func (m *Manager) AddMapping(groupName string, mp config.Mapping, autoAssign bool) (config.Mapping, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	group := m.findOrCreateGroup(groupName)
	key := mappingKey{Group: group.Name, Name: mp.Name}

	if _, exists := m.proxies[key]; exists {
		return mp, fmt.Errorf("mapping %s already exists", key.String())
	}

	if mp.TargetHost == "" || mp.TargetPort == 0 {
		return mp, fmt.Errorf("target_host and target_port are required")
	}

	if autoAssign || mp.LocalPort == 0 {
		mp.LocalPort = m.autoPort(*group)
	}

	group.Mappings = append(group.Mappings, mp)
	e := &entry{Mapping: mp, Group: group.Name}
	m.proxies[key] = e

	st := config.MappingState{
		Running:   false,
		LocalPort: mp.LocalPort,
	}
	m.state.Mappings[key.String()] = st

	if err := m.save(); err != nil {
		return mp, err
	}
	slog.Info("mapping added", "key", key.String(), "target", fmt.Sprintf("%s:%d", mp.TargetHost, mp.TargetPort), "local_port", mp.LocalPort)
	return mp, nil
}

func (m *Manager) UpdateMapping(groupName string, mp config.Mapping) (config.Mapping, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	key := mappingKey{Group: groupName, Name: mp.Name}
	existing, ok := m.proxies[key]
	if !ok {
		return mp, fmt.Errorf("mapping %s not found", key.String())
	}

	if mp.TargetHost != "" {
		existing.Mapping.TargetHost = mp.TargetHost
	}
	if mp.TargetPort != 0 {
		existing.Mapping.TargetPort = mp.TargetPort
	}

	needsRestart := false
	if existing.Proxy != nil && (mp.TargetHost != "" || mp.TargetPort != 0) {
		needsRestart = true
		existing.Proxy.Stop()
		existing.Proxy = nil
	}

	if mp.LocalPort > 0 && mp.LocalPort != existing.Mapping.LocalPort {
		if needsRestart {
			existing.Mapping.LocalPort = mp.LocalPort
		} else if existing.Proxy != nil {
			existing.Proxy.Stop()
			existing.Proxy = nil
			existing.Mapping.LocalPort = mp.LocalPort
			needsRestart = true
		} else {
			existing.Mapping.LocalPort = mp.LocalPort
			st := m.state.Mappings[key.String()]
			st.LocalPort = mp.LocalPort
			m.state.Mappings[key.String()] = st
		}
	}

	if needsRestart {
		st := m.state.Mappings[key.String()]
		if st.Running {
			p := m.newProxy(existing.Mapping, groupName)
			existing.Proxy = p
			if err := p.Start(); err != nil {
				slog.Error("restart proxy failed", "key", key.String(), "err", err)
				st.Running = false
				m.state.Mappings[key.String()] = st
			}
		}
	}

	group := m.mustGroup(groupName)
	for i := range group.Mappings {
		if group.Mappings[i].Name == mp.Name {
			group.Mappings[i] = existing.Mapping
			break
		}
	}

	if err := m.save(); err != nil {
		return mp, err
	}
	slog.Info("mapping updated", "key", key.String())
	return existing.Mapping, nil
}

func (m *Manager) DeleteMapping(groupName, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	key := mappingKey{Group: groupName, Name: name}
	e, ok := m.proxies[key]
	if !ok {
		return fmt.Errorf("mapping %s not found", key.String())
	}

	if e.Proxy != nil {
		e.Proxy.Stop()
	}

	delete(m.proxies, key)
	delete(m.state.Mappings, key.String())

	group := m.mustGroup(groupName)
	filtered := group.Mappings[:0]
	for _, mp := range group.Mappings {
		if mp.Name != name {
			filtered = append(filtered, mp)
		}
	}
	group.Mappings = filtered

	if err := m.save(); err != nil {
		return err
	}
	slog.Info("mapping deleted", "key", key.String())
	return nil
}

func (m *Manager) StartMapping(groupName, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	key := mappingKey{Group: groupName, Name: name}
	e, ok := m.proxies[key]
	if !ok {
		return fmt.Errorf("mapping %s not found", key.String())
	}

	st := m.state.Mappings[key.String()]
	if st.Running {
		return fmt.Errorf("mapping %s is already running", key.String())
	}

	if m.HasRelay() {
		if e.Mapping.LocalPort == 0 {
			e.Mapping.LocalPort = m.autoPort(*m.mustGroup(groupName))
		}
		st.Running = true
		st.StartedAt = time.Now().Format(time.RFC3339)
		st.LocalPort = e.Mapping.LocalPort
		m.state.Mappings[key.String()] = st
		m.save()
		m.relay.SetPort(key.String(), e.Mapping.LocalPort)
		slog.Info("mapping started (relay)", "key", key.String(), "port", e.Mapping.LocalPort)
		return nil
	}

	e.Proxy = m.newProxy(e.Mapping, groupName)
	if err := e.Proxy.Start(); err != nil {
		e.Proxy = nil
		return fmt.Errorf("start mapping %s: %w", key.String(), err)
	}

	st.Running = true
	st.StartedAt = time.Now().Format(time.RFC3339)
	st.LocalPort = e.Mapping.LocalPort
	m.state.Mappings[key.String()] = st

	config.SaveState(m.dir, m.state)
	slog.Info("mapping started", "key", key.String(), "port", e.Mapping.LocalPort)
	return nil
}

func (m *Manager) StopMapping(groupName, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	key := mappingKey{Group: groupName, Name: name}
	e, ok := m.proxies[key]
	if !ok {
		return fmt.Errorf("mapping %s not found", key.String())
	}

	st := m.state.Mappings[key.String()]
	if !st.Running {
		return fmt.Errorf("mapping %s is not running", key.String())
	}

	if m.HasRelay() {
		m.relay.DeregisterAndClose(key.String())
		st.Running = false
		st.StartedAt = ""
		m.state.Mappings[key.String()] = st
		m.save()
		slog.Info("mapping stopped (relay)", "key", key.String())
		return nil
	}

	if e.Proxy == nil {
		return fmt.Errorf("mapping %s is not running", key.String())
	}

	e.Proxy.Stop()
	e.Proxy = nil

	st.Running = false
	st.StartedAt = ""
	m.state.Mappings[key.String()] = st

	config.SaveState(m.dir, m.state)
	slog.Info("mapping stopped", "key", key.String())
	return nil
}

func (m *Manager) StartGroup(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	group := m.mustGroup(name)
	var errs []error
	for _, mp := range group.Mappings {
		key := mappingKey{Group: name, Name: mp.Name}
		e, ok := m.proxies[key]
		if !ok {
			continue
		}
		st := m.state.Mappings[key.String()]
		if st.Running {
			continue
		}
		if m.HasRelay() {
			if e.Mapping.LocalPort == 0 {
				e.Mapping.LocalPort = m.autoPort(*group)
			}
			st.Running = true
			st.StartedAt = time.Now().Format(time.RFC3339)
			st.LocalPort = e.Mapping.LocalPort
			m.state.Mappings[key.String()] = st
			m.relay.SetPort(key.String(), e.Mapping.LocalPort)
		} else {
			e.Proxy = m.newProxy(e.Mapping, name)
			if err := e.Proxy.Start(); err != nil {
				e.Proxy = nil
				errs = append(errs, fmt.Errorf("%s: %w", mp.Name, err))
				continue
			}
			st.Running = true
			st.StartedAt = time.Now().Format(time.RFC3339)
			st.LocalPort = e.Mapping.LocalPort
			m.state.Mappings[key.String()] = st
		}
	}

	m.save()
	if len(errs) > 0 {
		return fmt.Errorf("start group %s: %v", name, errs)
	}
	slog.Info("group started", "group", name)
	return nil
}

func (m *Manager) StopGroup(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	group := m.mustGroup(name)
	for _, mp := range group.Mappings {
		key := mappingKey{Group: name, Name: mp.Name}
		e, ok := m.proxies[key]
		if !ok {
			continue
		}
		st := m.state.Mappings[key.String()]
		if !st.Running {
			continue
		}
		if m.HasRelay() {
			m.relay.DeregisterAndClose(key.String())
		} else if e.Proxy != nil {
			e.Proxy.Stop()
			e.Proxy = nil
		}
		st.Running = false
		st.StartedAt = ""
		m.state.Mappings[key.String()] = st
	}

	m.save()
	slog.Info("group stopped", "group", name)
	return nil
}

func (m *Manager) ListMappings() []MappingInfo {
	m.mu.Lock()
	defer m.mu.Unlock()

	var activeTunnels map[string]bool
	if m.relay != nil {
		activeTunnels = m.relay.ActiveTunnels()
	}

	var result []MappingInfo
	for key, e := range m.proxies {
		st := m.state.Mappings[key.String()]
		status := "stopped"
		if st.Running {
			if activeTunnels != nil {
				if activeTunnels[key.String()] {
					status = "connected"
				} else {
					status = "waiting"
				}
			} else {
				status = "running"
			}
		}
		result = append(result, MappingInfo{
			Group:      key.Group,
			Name:       key.Name,
			LocalPort:  st.LocalPort,
			TargetHost: e.Mapping.TargetHost,
			TargetPort: e.Mapping.TargetPort,
			Status:     status,
			StartedAt:  st.StartedAt,
		})
	}
	return result
}

type MappingInfo struct {
	Group      string `json:"group"`
	Name       string `json:"name"`
	LocalPort  int    `json:"local_port"`
	TargetHost string `json:"target_host"`
	TargetPort int    `json:"target_port"`
	Status     string `json:"status"`
	StartedAt  string `json:"started_at,omitempty"`
}

func (m *Manager) SetBindAddr(addr string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cfg.BindAddr = addr
	config.SaveConfig(m.dir, m.cfg)
}

func (m *Manager) Groups() []config.Group {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]config.Group, len(m.cfg.Groups))
	copy(result, m.cfg.Groups)
	return result
}

func (m *Manager) findOrCreateGroup(name string) *config.Group {
	for i := range m.cfg.Groups {
		if m.cfg.Groups[i].Name == name {
			return &m.cfg.Groups[i]
		}
	}
	m.cfg.Groups = append(m.cfg.Groups, config.Group{Name: name})
	return &m.cfg.Groups[len(m.cfg.Groups)-1]
}

func (m *Manager) mustGroup(name string) *config.Group {
	for i := range m.cfg.Groups {
		if m.cfg.Groups[i].Name == name {
			return &m.cfg.Groups[i]
		}
	}
	return nil
}

func (m *Manager) newProxy(mp config.Mapping, groupName string) *proxy.Proxy {
	lnAddr := fmt.Sprintf("%s:%d", m.cfg.BindAddr, mp.LocalPort)
	tgAddr := fmt.Sprintf("%s:%d", mp.TargetHost, mp.TargetPort)
	return proxy.New(lnAddr, tgAddr)
}

func (m *Manager) autoPort(group config.Group) int {
	start := m.cfg.PortRange.Start
	end := m.cfg.PortRange.End
	if group.PortRange != nil {
		start = group.PortRange.Start
		end = group.PortRange.End
	}

	used := make(map[int]bool)
	for _, e := range m.proxies {
		if e.Mapping.LocalPort > 0 {
			used[e.Mapping.LocalPort] = true
		}
	}

	for p := start; p <= end; p++ {
		if !used[p] {
			return p
		}
	}
	return start
}

func (m *Manager) save() error {
	for _, g := range m.cfg.Groups {
		for i := range g.Mappings {
			key := mappingKey{Group: g.Name, Name: g.Mappings[i].Name}
			if e, ok := m.proxies[key]; ok {
				g.Mappings[i] = e.Mapping
			}
		}
	}
	if err := config.SaveConfig(m.dir, m.cfg); err != nil {
		return err
	}
	return config.SaveState(m.dir, m.state)
}
