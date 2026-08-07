package manager

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/zxhtom/portmap/internal/config"
	"github.com/zxhtom/portmap/internal/sshclient"
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
	Tunnel  *sshclient.Tunnel
}

type Manager struct {
	mu      sync.Mutex
	dir     string
	cfg     config.Config
	state   config.State
	entries map[mappingKey]*entry
}

func New(dir string) *Manager {
	return &Manager{
		dir:     dir,
		entries: make(map[mappingKey]*entry),
	}
}

func (m *Manager) Init() error {
	cfg, err := config.LoadConfig(m.dir)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	m.cfg = cfg

	state, err := config.LoadState(m.dir)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}
	m.state = state

	for _, g := range m.cfg.Groups {
		for _, mp := range g.Mappings {
			key := mappingKey{Group: g.Name, Name: mp.Name}
			e := &entry{Mapping: mp, Group: g.Name}
			m.entries[key] = e

			st, ok := m.state.Mappings[key.String()]
			if !ok {
				st = config.MappingState{Running: false}
				m.state.Mappings[key.String()] = st
			}
			if st.Running {
				m.startMappingLocked(key, e, st)
			}
		}
	}
	config.SaveState(m.dir, m.state)
	return nil
}

func (m *Manager) Shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.entries {
		if e.Tunnel != nil {
			e.Tunnel.Close()
		}
	}
	config.SaveState(m.dir, m.state)
}

func (m *Manager) RemoteByName(name string) (config.Remote, error) {
	for _, r := range m.cfg.Remotes {
		if r.Name == name {
			return r, nil
		}
	}
	return config.Remote{}, fmt.Errorf("remote %q not found", name)
}

func (m *Manager) AddRemote(r config.Remote) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, existing := range m.cfg.Remotes {
		if existing.Name == r.Name {
			return fmt.Errorf("remote %q already exists", r.Name)
		}
	}
	m.cfg.Remotes = append(m.cfg.Remotes, r)
	return config.SaveConfig(m.dir, m.cfg)
}

func (m *Manager) DeleteRemote(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, r := range m.cfg.Remotes {
		if r.Name == name {
			m.cfg.Remotes = append(m.cfg.Remotes[:i], m.cfg.Remotes[i+1:]...)
			return config.SaveConfig(m.dir, m.cfg)
		}
	}
	return fmt.Errorf("remote %q not found", name)
}

func (m *Manager) AddMapping(groupName string, mp config.Mapping) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if mp.LocalHost == "" || mp.LocalPort == 0 {
		return fmt.Errorf("local_host and local_port are required")
	}
	if mp.ListenPort == 0 {
		return fmt.Errorf("listen_port is required (port on remote server)")
	}
	if mp.Remote == "" {
		return fmt.Errorf("remote is required (reference to a configured remote)")
	}

	if _, err := m.RemoteByName(mp.Remote); err != nil {
		return err
	}

	key := mappingKey{Group: groupName, Name: mp.Name}
	if _, exists := m.entries[key]; exists {
		return fmt.Errorf("mapping %s already exists", key.String())
	}

	group := m.findOrCreateGroup(groupName)
	group.Mappings = append(group.Mappings, mp)
	m.entries[key] = &entry{Mapping: mp, Group: groupName}

	m.state.Mappings[key.String()] = config.MappingState{Running: false}

	if err := config.SaveConfig(m.dir, m.cfg); err != nil {
		return err
	}
	config.SaveState(m.dir, m.state)
	slog.Info("mapping added", "key", key.String(), "remote", mp.Remote, "listen", mp.ListenPort, "target", fmt.Sprintf("%s:%d", mp.LocalHost, mp.LocalPort))
	return nil
}

func (m *Manager) DeleteMapping(groupName, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	key := mappingKey{Group: groupName, Name: name}
	e, ok := m.entries[key]
	if !ok {
		return fmt.Errorf("mapping %s not found", key.String())
	}

	if e.Tunnel != nil {
		e.Tunnel.Close()
	}

	delete(m.entries, key)
	delete(m.state.Mappings, key.String())

	group := m.mustGroup(groupName)
	filtered := group.Mappings[:0]
	for _, mp := range group.Mappings {
		if mp.Name != name {
			filtered = append(filtered, mp)
		}
	}
	group.Mappings = filtered

	if err := config.SaveConfig(m.dir, m.cfg); err != nil {
		return err
	}
	config.SaveState(m.dir, m.state)
	slog.Info("mapping deleted", "key", key.String())
	return nil
}

func (m *Manager) StartMapping(groupName, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	key := mappingKey{Group: groupName, Name: name}
	e, ok := m.entries[key]
	if !ok {
		return fmt.Errorf("mapping %s not found", key.String())
	}

	st := m.state.Mappings[key.String()]
	if st.Running {
		return fmt.Errorf("mapping %s is already running", key.String())
	}

	return m.startMappingLocked(key, e, st)
}

func (m *Manager) startMappingLocked(key mappingKey, e *entry, st config.MappingState) error {
	remote, err := m.RemoteByName(e.Mapping.Remote)
	if err != nil {
		return err
	}

	e.Tunnel = sshclient.NewTunnel(remote, e.Mapping)
	if err := e.Tunnel.Dial(remote); err != nil {
		e.Tunnel = nil
		return fmt.Errorf("start %s: %w", key.String(), err)
	}

	st.Running = true
	st.StartedAt = time.Now().Format(time.RFC3339)
	m.state.Mappings[key.String()] = st
	config.SaveState(m.dir, m.state)
	slog.Info("mapping started", "key", key.String())
	return nil
}

func (m *Manager) StopMapping(groupName, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	key := mappingKey{Group: groupName, Name: name}
	e, ok := m.entries[key]
	if !ok {
		return fmt.Errorf("mapping %s not found", key.String())
	}

	st := m.state.Mappings[key.String()]
	if !st.Running {
		return fmt.Errorf("mapping %s is not running", key.String())
	}

	if e.Tunnel != nil {
		e.Tunnel.Close()
		e.Tunnel = nil
	}

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
	if group == nil {
		return fmt.Errorf("group %q not found", name)
	}
	var errs []error
	for _, mp := range group.Mappings {
		key := mappingKey{Group: name, Name: mp.Name}
		e := m.entries[key]
		st := m.state.Mappings[key.String()]
		if st.Running {
			continue
		}
		if err := m.startMappingLocked(key, e, st); err != nil {
			errs = append(errs, err)
		}
	}

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
	if group == nil {
		return fmt.Errorf("group %q not found", name)
	}
	for _, mp := range group.Mappings {
		key := mappingKey{Group: name, Name: mp.Name}
		e := m.entries[key]
		st := m.state.Mappings[key.String()]
		if !st.Running {
			continue
		}
		if e.Tunnel != nil {
			e.Tunnel.Close()
			e.Tunnel = nil
		}
		st.Running = false
		st.StartedAt = ""
		m.state.Mappings[key.String()] = st
	}
	config.SaveState(m.dir, m.state)
	slog.Info("group stopped", "group", name)
	return nil
}

func (m *Manager) ListMappings() []MappingInfo {
	m.mu.Lock()
	defer m.mu.Unlock()

	result := make([]MappingInfo, 0, len(m.entries))
	for key, e := range m.entries {
		st := m.state.Mappings[key.String()]
		status := "stopped"
		if st.Running {
			status = "running"
		}
		result = append(result, MappingInfo{
			Group:      key.Group,
			Name:       key.Name,
			Remote:     e.Mapping.Remote,
			ListenPort: e.Mapping.ListenPort,
			LocalHost:  e.Mapping.LocalHost,
			LocalPort:  e.Mapping.LocalPort,
			Status:     status,
			StartedAt:  st.StartedAt,
		})
	}
	return result
}

type MappingInfo struct {
	Group      string `json:"group"`
	Name       string `json:"name"`
	Remote     string `json:"remote"`
	ListenPort int    `json:"listen_port"`
	LocalHost  string `json:"local_host"`
	LocalPort  int    `json:"local_port"`
	Status     string `json:"status"`
	StartedAt  string `json:"started_at,omitempty"`
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
