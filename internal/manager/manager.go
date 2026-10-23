package manager

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/zxhtom/portmap/internal/config"
	"github.com/zxhtom/portmap/internal/dockerclient"
	"github.com/zxhtom/portmap/internal/forward"
	"github.com/zxhtom/portmap/internal/multipassclient"
	"github.com/zxhtom/portmap/internal/relayprotocol"
	"github.com/zxhtom/portmap/internal/sshclient"
)

type BackendFactory func(group string, mp config.Mapping, remote config.Remote, dockerCfg config.DockerConfig, instanceID string) (forward.Backend, error)

type mappingKey struct {
	Group string
	Name  string
}

func (k mappingKey) String() string {
	return k.Group + "/" + k.Name
}

type entry struct {
	Mapping   config.Mapping
	Group     string
	startMu   sync.Mutex
	mu        sync.RWMutex
	Backend   forward.Backend
	lastError string
}

type Manager struct {
	controlMu           sync.Mutex
	mu                  sync.Mutex
	dir                 string
	cfg                 config.Config
	state               config.State
	entries             map[mappingKey]*entry
	backendFactory      BackendFactory
	relayPortAllocator  RelayPortAllocator
	domainRequester     DomainRequester
	serverRequester     func(config.Remote, relayprotocol.Request) (relayprotocol.Response, error)
	serverDisabled      map[string]bool
	instanceID          string
	instanceLock        *config.InstanceLock
	shuttingDown        bool
	startWg             sync.WaitGroup
	dockerHostOverride  *string
	dockerImageOverride *string
	cleanupTimeout      time.Duration
}

func defaultBackendFactory(group string, mp config.Mapping, remote config.Remote, dockerCfg config.DockerConfig, instanceID string) (forward.Backend, error) {
	mp.DomainOwner = domainOwner(instanceID, group, mp.Name)
	switch mp.EffectiveType() {
	case config.TypeSSH:
		return sshclient.NewTunnel(remote, mp), nil
	case config.TypeMultipass:
		return multipassclient.NewForward(multipassclient.NewClient(""), mp, remote), nil
	case config.TypeDocker:
		dcli, err := dockerclient.NewClient(dockerCfg, instanceID)
		if err != nil {
			return nil, fmt.Errorf("create docker client: %w", err)
		}
		return dockerclient.NewForward(dcli, mp, group, instanceID, dockerCfg.HelperImage), nil
	default:
		return nil, fmt.Errorf("unknown mapping type %s", mp.EffectiveType())
	}
}

func New(dir string) *Manager {
	return &Manager{
		dir:                dir,
		cfg:                config.DefaultConfig(),
		state:              config.DefaultState(),
		entries:            make(map[mappingKey]*entry),
		backendFactory:     defaultBackendFactory,
		relayPortAllocator: sshclient.AllocatePort,
		domainRequester:    sshclient.RelayRequest,
		serverRequester:    sshclient.ServerRequest,
		serverDisabled:     make(map[string]bool),
		cleanupTimeout:     10 * time.Second,
	}
}

func (m *Manager) SetCleanupTimeout(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cleanupTimeout = d
}

func (m *Manager) SetDockerOverrides(host, helperImage *string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dockerHostOverride = host
	m.dockerImageOverride = helperImage
}

func (m *Manager) SetBackendFactory(factory BackendFactory) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.backendFactory = factory
}

func (m *Manager) SetDockerBackendFactory(factory BackendFactory) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.backendFactory = factory
}

func (m *Manager) InstanceID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.instanceID
}

func (m *Manager) Init() error {
	id, lock, err := config.AcquireInstanceLock(m.dir)
	if err != nil {
		return fmt.Errorf("instance lock: %w", err)
	}
	m.instanceID = id
	m.instanceLock = lock

	cfg, err := config.LoadConfig(m.dir)
	if err != nil {
		_ = lock.Release()
		m.instanceLock = nil
		return fmt.Errorf("load config: %w", err)
	}
	if err := validateConfig(&cfg); err != nil {
		_ = lock.Release()
		m.instanceLock = nil
		return fmt.Errorf("invalid configuration: %w", err)
	}
	m.cfg = cfg

	// Merge explicit flag overrides after loading config
	if m.dockerHostOverride != nil {
		m.cfg.Docker.Host = *m.dockerHostOverride
	}
	if m.dockerImageOverride != nil {
		m.cfg.Docker.HelperImage = *m.dockerImageOverride
	}

	state, err := config.LoadState(m.dir)
	if err != nil {
		_ = lock.Release()
		m.instanceLock = nil
		return fmt.Errorf("load state: %w", err)
	}
	m.state = state

	// CleanupOwned before restoring any mappings
	dcli, err := dockerclient.NewClient(m.cfg.Docker, id)
	if err != nil {
		slog.Warn("docker client unavailable for cleanup on init", "err", err)
	} else {
		timeout := m.cleanupTimeout
		if timeout <= 0 {
			timeout = 10 * time.Second
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		if err := dcli.CleanupOwned(ctx); err != nil {
			slog.Warn("docker cleanup failed on init", "err", err)
			var netErr net.Error
			isTimeout := errors.Is(err, context.DeadlineExceeded) ||
				errors.Is(err, context.Canceled) ||
				(errors.As(err, &netErr) && netErr.Timeout())
			errStr := strings.ToLower(err.Error())
			isUnreachable := isTimeout ||
				strings.Contains(errStr, "cannot connect") ||
				strings.Contains(errStr, "connection refused") ||
				strings.Contains(errStr, "no such file or directory") ||
				strings.Contains(errStr, "daemon is not running") ||
				strings.Contains(errStr, "is the docker daemon running") ||
				strings.Contains(errStr, "context deadline exceeded") ||
				strings.Contains(errStr, "i/o timeout")
			if !isUnreachable {
				_ = lock.Release()
				m.instanceLock = nil
				return fmt.Errorf("cleanup owned containers: %w", err)
			}
		}
	}

	for _, g := range m.cfg.Groups {
		for _, mp := range g.Mappings {
			mp.SetDefaults()
			key := mappingKey{Group: g.Name, Name: mp.Name}
			e := &entry{Mapping: mp, Group: g.Name}
			m.entries[key] = e

			st, ok := m.state.Mappings[key.String()]
			if !ok {
				st = config.MappingState{Running: false}
				m.state.Mappings[key.String()] = st
			}
			if st.Running && !st.Invalid && !mp.Expired(time.Now()) {
				// Attempt restore on boot
				_ = m.startEntry(key, e, true)
			}
		}
	}
	config.SaveState(m.dir, m.state)
	return nil
}

func (m *Manager) Shutdown() {
	m.controlMu.Lock()
	defer m.controlMu.Unlock()
	m.mu.Lock()
	m.shuttingDown = true
	entries := make([]*entry, 0, len(m.entries))
	for _, e := range m.entries {
		entries = append(entries, e)
	}
	m.mu.Unlock()

	// Wait for any in-flight starts to complete/abort
	m.startWg.Wait()

	for _, e := range entries {
		e.startMu.Lock()
		e.mu.Lock()
		b := e.Backend
		e.Backend = nil
		e.mu.Unlock()
		if b != nil {
			_ = b.Close()
		}
		e.startMu.Unlock()
	}

	m.mu.Lock()
	config.SaveState(m.dir, m.state)
	if m.instanceLock != nil {
		_ = m.instanceLock.Release()
		m.instanceLock = nil
	}
	m.mu.Unlock()
}

func (m *Manager) RemoteByName(name string) (config.Remote, error) {
	for _, r := range m.cfg.Remotes {
		if r.Name == name {
			if r.DomainZone == "" && m.cfg.DomainZone != "" {
				r.DomainZone = m.cfg.DomainZone
			}
			return r, nil
		}
	}
	return config.Remote{}, fmt.Errorf("remote %q not found", name)
}

func (m *Manager) AddRemote(r config.Remote) error {
	m.controlMu.Lock()
	defer m.controlMu.Unlock()
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
	m.controlMu.Lock()
	defer m.controlMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, g := range m.cfg.Groups {
		for _, mp := range g.Mappings {
			if mp.Remote == name {
				return fmt.Errorf("remote %q is used by mapping %s/%s", name, g.Name, mp.Name)
			}
		}
	}
	for i, r := range m.cfg.Remotes {
		if r.Name == name {
			m.cfg.Remotes = append(m.cfg.Remotes[:i], m.cfg.Remotes[i+1:]...)
			return config.SaveConfig(m.dir, m.cfg)
		}
	}
	return fmt.Errorf("remote %q not found", name)
}

// AddMapping registers an existing, fully specified mapping without contacting
// its relay. This supports offline configuration imports and manager restoration.
func (m *Manager) AddMapping(groupName string, mp config.Mapping) error {
	m.controlMu.Lock()
	defer m.controlMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.addMappingLocked(groupName, mp)
}

func (m *Manager) addMappingLocked(groupName string, mp config.Mapping) error {
	mp.SetDefaults()
	if err := mp.Validate(); err != nil {
		return err
	}
	if mp.UsesRelay() {
		if _, err := m.RemoteByName(mp.Remote); err != nil {
			return err
		}
	}
	key := mappingKey{Group: groupName, Name: mp.Name}
	if _, exists := m.entries[key]; exists {
		return fmt.Errorf("mapping %s already exists", key.String())
	}
	if owner := m.portOwnerLocked(mp); owner != "" {
		return fmt.Errorf("端口 %d 已被映射 %s 占用，请选择其他端口或留空随机分配", mp.ListenPort, owner)
	}
	next := m.cfg
	next.Groups = append([]config.Group(nil), m.cfg.Groups...)
	found := false
	for i := range next.Groups {
		if next.Groups[i].Name == groupName {
			next.Groups[i].Mappings = append(append([]config.Mapping(nil), next.Groups[i].Mappings...), mp)
			found = true
			break
		}
	}
	if !found {
		next.Groups = append(next.Groups, config.Group{Name: groupName, Mappings: []config.Mapping{mp}})
	}
	if err := config.SaveConfig(m.dir, next); err != nil {
		return err
	}
	m.cfg = next
	m.entries[key] = &entry{Mapping: mp, Group: groupName}
	m.state.Mappings[key.String()] = config.MappingState{}
	if err := config.SaveState(m.dir, m.state); err != nil {
		return err
	}
	slog.Info("mapping added", "key", key.String(), "type", mp.EffectiveType(), "listen_port", mp.ListenPort)
	return nil
}

func (m *Manager) DeleteMapping(groupName, name string) error {
	m.controlMu.Lock()
	defer m.controlMu.Unlock()
	key := mappingKey{Group: groupName, Name: name}
	m.mu.Lock()
	e, ok := m.entries[key]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("mapping %s not found", key.String())
	}
	m.mu.Unlock()

	e.startMu.Lock()
	defer e.startMu.Unlock()

	// Short lock to verify identity under startMu
	m.mu.Lock()
	currentEntry, exists := m.entries[key]
	if !exists || currentEntry != e {
		m.mu.Unlock()
		return fmt.Errorf("mapping %s not found", key.String())
	}
	m.mu.Unlock()

	e.mu.Lock()
	backend := e.Backend
	e.mu.Unlock()

	if backend != nil {
		if err := backend.Close(); err != nil {
			return fmt.Errorf("close backend for %s: %w", key.String(), err)
		}
		e.mu.Lock()
		e.Backend = nil
		e.mu.Unlock()
	}

	if err := m.releaseDomains(groupName, e.Mapping, m.cfg); err != nil {
		return fmt.Errorf("release domain bindings: %w", err)
	}
	// Delete from manager structures and save config/state only after Close succeeds
	m.mu.Lock()
	currentEntry, exists = m.entries[key]
	if exists && currentEntry == e {
		delete(m.entries, key)
		delete(m.state.Mappings, key.String())

		group := m.mustGroup(groupName)
		if group != nil {
			filtered := group.Mappings[:0]
			for _, mp := range group.Mappings {
				if mp.Name != name {
					filtered = append(filtered, mp)
				}
			}
			group.Mappings = filtered
		}
		_ = config.SaveConfig(m.dir, m.cfg)
		_ = config.SaveState(m.dir, m.state)
	}
	m.mu.Unlock()

	slog.Info("mapping deleted", "key", key.String())
	return nil
}

func (m *Manager) StartMapping(groupName, name string) error {
	key := mappingKey{Group: groupName, Name: name}
	m.mu.Lock()
	e, ok := m.entries[key]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("mapping %s not found", key.String())
	}
	m.mu.Unlock()

	return m.startEntry(key, e, false)
}

func (m *Manager) startEntry(key mappingKey, e *entry, isRestore bool) error {
	m.mu.Lock()
	if m.shuttingDown {
		m.mu.Unlock()
		return fmt.Errorf("manager is shutting down")
	}
	m.startWg.Add(1)
	m.mu.Unlock()
	defer m.startWg.Done()

	e.startMu.Lock()
	defer e.startMu.Unlock()

	// Short lock to re-verify identity and shutdown
	m.mu.Lock()
	if m.shuttingDown {
		m.mu.Unlock()
		return fmt.Errorf("manager is shutting down")
	}
	currentEntry, exists := m.entries[key]
	if !exists || currentEntry != e {
		m.mu.Unlock()
		return fmt.Errorf("mapping %s was deleted", key.String())
	}

	if e.Mapping.Expired(time.Now()) {
		m.mu.Unlock()
		return fmt.Errorf("mapping expired; renew its validity before starting")
	}
	e.mu.Lock()
	b := e.Backend
	e.mu.Unlock()
	if b != nil && b.Status().State == forward.StateRunning {
		m.mu.Unlock()
		return fmt.Errorf("mapping %s is already running", key.String())
	}

	// Persist intent before connecting so initial failures can recover.
	st := m.state.Mappings[key.String()]
	if isRestore && (!st.Running || st.Invalid) {
		m.mu.Unlock()
		return nil
	}
	if !isRestore {
		st.Failures, st.Invalid, st.CheckedAt, st.CheckError = 0, false, "", ""
		st.Running = true
		m.state.Mappings[key.String()] = st
		if err := config.SaveState(m.dir, m.state); err != nil {
			m.mu.Unlock()
			return fmt.Errorf("save desired state: %w", err)
		}
	}
	mapping := e.Mapping
	factory := m.backendFactory
	dockerCfg := m.cfg.Docker
	instID := m.instanceID
	serverRequester := m.serverRequester
	knownDisabled := m.serverDisabled[domainOwner(instID, key.Group, key.Name)]
	var remote config.Remote
	if e.Mapping.EffectiveType() == config.TypeSSH || (e.Mapping.EffectiveType() == config.TypeMultipass && e.Mapping.Remote != "") {
		r, err := m.RemoteByName(e.Mapping.Remote)
		if err != nil {
			m.mu.Unlock()
			e.mu.Lock()
			e.lastError = err.Error()
			e.mu.Unlock()
			m.setFailedState(key, e, err.Error())
			return err
		}
		remote = r
	}
	m.mu.Unlock()
	if knownDisabled {
		err := fmt.Errorf("mapping disabled on VPS")
		m.setFailedState(key, e, err.Error())
		return err
	}
	if instID != "" && remote.Name != "" && serverRequester != nil {
		response, checkErr := serverRequester(remote, relayprotocol.Request{Operation: "tunnel_check", Owner: domainOwner(instID, key.Group, key.Name)})
		if checkErr == nil && response.Disabled {
			m.mu.Lock()
			m.serverDisabled[domainOwner(instID, key.Group, key.Name)] = true
			m.mu.Unlock()
			err := fmt.Errorf("mapping disabled on VPS")
			m.setFailedState(key, e, err.Error())
			return err
		}
	}

	e.mu.Lock()
	oldBackend := e.Backend
	e.mu.Unlock()

	if oldBackend != nil {
		st := oldBackend.Status()
		if st.State == forward.StateRunning {
			return fmt.Errorf("mapping %s is already running", key.String())
		}
		if err := oldBackend.Close(); err != nil {
			e.mu.Lock()
			e.lastError = err.Error()
			e.mu.Unlock()
			m.setFailedState(key, e, err.Error())
			return fmt.Errorf("close old backend for %s: %w", key.String(), err)
		}
		e.mu.Lock()
		e.Backend = nil
		e.mu.Unlock()
	}

	backend, err := factory(e.Group, mapping, remote, dockerCfg, instID)
	if err != nil {
		e.mu.Lock()
		e.lastError = err.Error()
		e.mu.Unlock()
		m.setFailedState(key, e, err.Error())
		return err
	}

	if err := backend.Start(); err != nil {
		closeErr := backend.Close()
		e.mu.Lock()
		if closeErr != nil {
			e.Backend = backend
			combined := fmt.Errorf("start %s: %w; cleanup close error: %v", key.String(), err, closeErr)
			e.lastError = combined.Error()
			e.mu.Unlock()
			m.setFailedState(key, e, combined.Error())
			return combined
		}
		e.lastError = err.Error()
		e.mu.Unlock()
		m.setFailedState(key, e, err.Error())
		return fmt.Errorf("start %s: %w", key.String(), err)
	}

	e.mu.Lock()
	e.Backend = backend
	e.lastError = ""
	e.mu.Unlock()

	m.mu.Lock()
	if m.shuttingDown {
		m.mu.Unlock()
		_ = backend.Close()
		e.mu.Lock()
		e.Backend = nil
		e.mu.Unlock()
		return fmt.Errorf("manager is shutting down")
	}
	currentEntry, exists = m.entries[key]
	if !exists || currentEntry != e {
		m.mu.Unlock()
		_ = backend.Close()
		e.mu.Lock()
		e.Backend = nil
		e.mu.Unlock()
		return fmt.Errorf("mapping %s was deleted while starting", key.String())
	}

	st = m.state.Mappings[key.String()]
	st.Running = true
	st.StartedAt = time.Now().Format(time.RFC3339)
	m.state.Mappings[key.String()] = st
	_ = config.SaveState(m.dir, m.state)
	m.mu.Unlock()

	slog.Info("mapping started", "key", key.String())
	return nil
}

func (m *Manager) setFailedState(key mappingKey, e *entry, errMsg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	currentEntry, exists := m.entries[key]
	if !exists || currentEntry != e {
		return
	}
	st, ok := m.state.Mappings[key.String()]
	if !ok {
		return
	}
	st.StartedAt = ""
	m.state.Mappings[key.String()] = st
	_ = config.SaveState(m.dir, m.state)
}

func (m *Manager) stopEntry(key mappingKey, e *entry) error {
	e.startMu.Lock()
	defer e.startMu.Unlock()

	m.mu.Lock()
	currentEntry, exists := m.entries[key]
	if !exists || currentEntry != e {
		m.mu.Unlock()
		return fmt.Errorf("mapping %s not found", key.String())
	}

	st := m.state.Mappings[key.String()]
	e.mu.Lock()
	backend := e.Backend
	var isRunning bool
	if backend != nil {
		isRunning = (backend.Status().State == forward.StateRunning)
	}
	e.mu.Unlock()

	if !st.Running && !isRunning && backend == nil {
		m.mu.Unlock()
		return fmt.Errorf("mapping %s is not running", key.String())
	}
	m.mu.Unlock()

	e.mu.Lock()
	b := e.Backend
	e.mu.Unlock()

	if b != nil {
		if err := b.Close(); err != nil {
			return fmt.Errorf("close backend for %s: %w", key.String(), err)
		}
		e.mu.Lock()
		e.Backend = nil
		e.mu.Unlock()
	}

	m.mu.Lock()
	currentEntry, exists = m.entries[key]
	if exists && currentEntry == e {
		st = m.state.Mappings[key.String()]
		st.Running = false
		st.StartedAt = ""
		m.state.Mappings[key.String()] = st
		_ = config.SaveState(m.dir, m.state)
	}
	m.mu.Unlock()

	slog.Info("mapping stopped", "key", key.String())
	return nil
}

func (m *Manager) StopMapping(groupName, name string) error {
	key := mappingKey{Group: groupName, Name: name}
	m.mu.Lock()
	e, ok := m.entries[key]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("mapping %s not found", key.String())
	}
	m.mu.Unlock()

	return m.stopEntry(key, e)
}

func (m *Manager) StartGroup(name string) error {
	m.mu.Lock()
	group := m.mustGroup(name)
	if group == nil {
		m.mu.Unlock()
		return fmt.Errorf("group %q not found", name)
	}
	type item struct {
		key mappingKey
		e   *entry
	}
	var items []item
	for _, mp := range group.Mappings {
		k := mappingKey{Group: name, Name: mp.Name}
		items = append(items, item{key: k, e: m.entries[k]})
	}
	m.mu.Unlock()

	var errs []error
	for _, it := range items {
		if err := m.startEntry(it.key, it.e, false); err != nil {
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
	group := m.mustGroup(name)
	if group == nil {
		m.mu.Unlock()
		return fmt.Errorf("group %q not found", name)
	}
	type item struct {
		key mappingKey
		e   *entry
	}
	var items []item
	for _, mp := range group.Mappings {
		k := mappingKey{Group: name, Name: mp.Name}
		items = append(items, item{key: k, e: m.entries[k]})
	}
	m.mu.Unlock()

	var errs []error
	for _, it := range items {
		if err := m.stopEntry(it.key, it.e); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("stop group %s: %v", name, errs)
	}
	slog.Info("group stopped", "group", name)
	return nil
}

func (m *Manager) ListMappings() []MappingInfo {
	m.mu.Lock()
	type snapshotItem struct {
		zone string
		key  mappingKey
		mp   config.Mapping
		st   config.MappingState
		e    *entry
	}
	items := make([]snapshotItem, 0, len(m.entries))
	for key, e := range m.entries {
		items = append(items, snapshotItem{
			key:  key,
			zone: domainZone(remoteFor(m.cfg, e.Mapping.Remote)),
			mp:   e.Mapping,
			st:   m.state.Mappings[key.String()],
			e:    e,
		})
	}
	m.mu.Unlock()

	result := make([]MappingInfo, 0, len(items))
	for _, it := range items {
		status := string(forward.StateStopped)
		var errMsg string

		it.e.mu.Lock()
		backend := it.e.Backend
		lastErr := it.e.lastError
		it.e.mu.Unlock()

		if backend != nil {
			bs := backend.Status()
			status = string(bs.State)
			errMsg = bs.Error
			if errMsg == "" && lastErr != "" {
				errMsg = lastErr
			}
		} else if it.st.Running {
			status = string(forward.StateFailed)
			if lastErr != "" {
				errMsg = lastErr
			} else {
				errMsg = "backend not initialized"
			}
		} else if lastErr != "" {
			status = string(forward.StateFailed)
			errMsg = lastErr
		}

		health := "已停止"
		if it.st.Running {
			health = "待检测"
			if it.st.Invalid {
				health, status = "作废", "invalid"
			} else if it.st.Failures > 0 || status == "failed" {
				health = "重试中"
			} else if it.st.CheckedAt != "" {
				health = "正常"
			}
			if it.st.CheckError != "" {
				errMsg = it.st.CheckError
			}
		}
		reason := ""
		if it.st.Running && it.st.Invalid {
			reason = "connection_failures"
		}
		if it.mp.Expired(time.Now()) {
			health, status, reason, errMsg = "作废", "invalid", "expired", "有效期已过，请续期"
		}
		domains := make([]string, 0, len(it.mp.DomainPrefixes))
		for _, prefix := range it.mp.DomainPrefixes {
			domains = append(domains, prefix+"."+it.zone)
		}
		result = append(result, MappingInfo{
			DomainPrefixes: append([]string(nil), it.mp.DomainPrefixes...), Domains: domains, UpstreamScheme: it.mp.UpstreamScheme,
			ExpiresAt: it.mp.ExpiresAt, InvalidReason: reason,
			Health: health, ConsecutiveFailures: it.st.Failures, CheckedAt: it.st.CheckedAt,
			Description:    it.mp.Description,
			Group:          it.key.Group,
			Name:           it.key.Name,
			Type:           it.mp.EffectiveType(),
			Remote:         it.mp.Remote,
			ListenPort:     it.mp.ListenPort,
			LocalHost:      it.mp.LocalHost,
			LocalPort:      it.mp.LocalPort,
			Container:      it.mp.Container,
			ContainerPort:  it.mp.ContainerPort,
			BindHost:       it.mp.BindHost,
			Instance:       it.mp.Instance,
			InstancePort:   it.mp.InstancePort,
			Status:         status,
			DesiredRunning: it.st.Running,
			StartedAt:      it.st.StartedAt,
			Error:          errMsg,
		})
	}
	return result
}

type MappingInfo struct {
	DesiredRunning      bool     `json:"desired_running"`
	DomainPrefixes      []string `json:"domain_prefixes,omitempty"`
	Domains             []string `json:"domains,omitempty"`
	UpstreamScheme      string   `json:"upstream_scheme,omitempty"`
	ExpiresAt           string   `json:"expires_at,omitempty"`
	InvalidReason       string   `json:"invalid_reason,omitempty"`
	Health              string   `json:"health"`
	ConsecutiveFailures int      `json:"consecutive_failures"`
	CheckedAt           string   `json:"checked_at,omitempty"`
	Description         string   `json:"description"`
	Group               string   `json:"group"`
	Name                string   `json:"name"`
	Type                string   `json:"type"`
	Remote              string   `json:"remote,omitempty"`
	ListenPort          int      `json:"listen_port"`
	LocalHost           string   `json:"local_host,omitempty"`
	LocalPort           int      `json:"local_port,omitempty"`
	Container           string   `json:"container,omitempty"`
	ContainerPort       int      `json:"container_port,omitempty"`
	BindHost            string   `json:"bind_host,omitempty"`
	Instance            string   `json:"instance,omitempty"`
	InstancePort        int      `json:"instance_port,omitempty"`
	Status              string   `json:"status"`
	StartedAt           string   `json:"started_at,omitempty"`
	Error               string   `json:"error,omitempty"`
}

func (m *Manager) DockerConfig() config.DockerConfig {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg.Docker
}

func (m *Manager) SetDockerConfig(cfg config.DockerConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cfg.Docker = cfg
}

func (m *Manager) ListDockerContainers() ([]dockerclient.ContainerSummary, error) {
	m.mu.Lock()
	dockerCfg := m.cfg.Docker
	instID := m.instanceID
	m.mu.Unlock()

	dcli, err := dockerclient.NewClient(dockerCfg, instID)
	if err != nil {
		return nil, fmt.Errorf("create docker client: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return dcli.ListContainers(ctx)
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

func (m *Manager) ListMultipassInstances() ([]multipassclient.Instance, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return multipassclient.NewClient("").List(ctx)
}

// DescribeMapping changes metadata without touching the running backend.
func (m *Manager) DescribeMapping(group, name, description string) error {
	m.controlMu.Lock()
	defer m.controlMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	key := mappingKey{Group: group, Name: name}
	e, ok := m.entries[key]
	if !ok {
		return fmt.Errorf("mapping not found")
	}
	// Save a copy first, so a failed write leaves memory unchanged.
	next := m.cfg
	next.Groups = append([]config.Group(nil), m.cfg.Groups...)
	for i := range next.Groups {
		next.Groups[i].Mappings = append([]config.Mapping(nil), next.Groups[i].Mappings...)
		if next.Groups[i].Name == group {
			for j := range next.Groups[i].Mappings {
				if next.Groups[i].Mappings[j].Name == name {
					next.Groups[i].Mappings[j].Description = description
				}
			}
		}
	}
	if err := config.SaveConfig(m.dir, next); err != nil {
		return err
	}
	m.cfg = next
	e.Mapping.Description = description
	return nil
}

func (m *Manager) DataDir() string { return m.dir }

// RemoteSummaries never includes passwords, key paths, passphrases or key material.
func (m *Manager) RemoteSummaries() []map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]map[string]any, 0, len(m.cfg.Remotes))
	for _, r := range m.cfg.Remotes {
		result = append(result, map[string]any{"name": r.Name, "host": r.Host, "port": r.Port, "user": r.User, "domain_zone": domainZone(remoteFor(m.cfg, r.Name))})
	}
	return result
}
